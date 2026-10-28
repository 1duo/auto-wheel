package moomoo

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"autowheel/internal/wheel"
)

// Source is the production Moomoo ExecutionSource. Every call re-reads the
// authenticated account; nothing is cached across cycles. Its interpretation
// rules are documented in docs/moomoo-deployment.md and are deliberately
// one-sided: a fact that does not match a reviewed shape blocks execution.
type Source struct {
	Client *Client
	Config Config
	Clock  func() time.Time

	mu        sync.Mutex
	history   map[string]orderRow // Every order created since the cutoff, by ID; replaced wholesale.
	historyAt time.Time
	fills     map[string]wheel.Cents // Terminal FILLED_ALL opening credits by order ID and ref.
}

var _ ExecutionSource = (*Source)(nil)

// NewSource requires the complete-coverage native policy and its cutoff.
func NewSource(client *Client, config Config) (*Source, error) {
	if client == nil || config.Validate() != nil || config.AccountID == "" || config.Strategy.FixedPolicy == nil ||
		config.Strategy.OrderCoveragePolicy != wheel.CompleteMoomooOrders || config.Strategy.ExclusiveOrdersSince.IsZero() {
		return nil, errors.New("Moomoo execution source requires its qualified account-bound policy and order cutoff")
	}
	return &Source{Client: client, Config: config}, nil
}

func (s *Source) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

// optionCode is the parsed identity of a provider option code. Strike digits
// are dollars*1000, so a standard strike is an exact multiple of ten.
type optionCode struct {
	Root   string
	Expiry string
	Kind   string
	Strike wheel.Cents
}

var optionCodePattern = regexp.MustCompile(`^US\.([A-Z]{1,5})(\d{6})([CP])(\d{1,10})$`)

func parseOptionCode(code string) (optionCode, error) {
	m := optionCodePattern.FindStringSubmatch(code)
	if m == nil {
		return optionCode{}, errors.New("Moomoo option code is not a reviewed single-leg equity option")
	}
	date, err := time.Parse("060102", m[2])
	digits, numErr := strconv.ParseInt(m[4], 10, 64)
	if err != nil || date.Format("060102") != m[2] || numErr != nil || digits <= 0 || digits%10 != 0 || wheel.Cents(digits/10) > wheel.MaxMoney/100 {
		return optionCode{}, errors.New("Moomoo option code date or strike is ambiguous")
	}
	kind := "put"
	if m[3] == "C" {
		kind = "call"
	}
	return optionCode{Root: m[1], Expiry: date.Format(time.DateOnly), Kind: kind, Strike: wheel.Cents(digits / 10)}, nil
}

// option returns the exact engine identity. ChainID is the underlying code,
// stable across reads; it is never copied from local state.
func (c optionCode) option(code string, quantity int) wheel.Option {
	return wheel.Option{ID: code, ChainID: "US." + c.Root, Symbol: c.Root, Kind: c.Kind, Expiry: c.Expiry, Strike: c.Strike, Quantity: quantity}
}

// wholeQuantity accepts only exact integers, optionally with zero fractions.
func wholeQuantity(value string) (int64, error) {
	micros, err := quantityMicros(value)
	if err != nil || micros%1_000_000 != 0 {
		return 0, errors.New("Moomoo quantity is not an exact whole number")
	}
	return micros / 1_000_000, nil
}

type positionRow struct {
	Code         string `json:"code"`
	Side         string `json:"position_side"`
	SecurityType string `json:"security_type"`
	Quantity     string `json:"qty"`
	Available    string `json:"can_sell_qty"`
	Cost         string `json:"cost_price"`
	Currency     string `json:"currency"`
}

func parsePositionRows(raw []byte) ([]positionRow, error) {
	var docs []map[string]json.RawMessage
	if exactJSON(raw, &docs) != nil || docs == nil || len(docs) > 20000 {
		return nil, errors.New("Moomoo position list is missing or exceeds limit")
	}
	rows := make([]positionRow, 0, len(docs))
	seen := map[string]bool{}
	for _, doc := range docs {
		if err := rejectQuoteAliases(doc, "code", "position_side", "security_type", "qty", "can_sell_qty", "cost_price", "currency"); err != nil {
			return nil, err
		}
		encoded, _ := json.Marshal(doc)
		var row positionRow
		if exactJSON(encoded, &row) != nil || row.Code == "" || strings.TrimSpace(row.Code) != row.Code || row.SecurityType == "" ||
			row.Side != "LONG" && row.Side != "SHORT" || seen[row.Code+"/"+row.Side] {
			return nil, errors.New("Moomoo position identity is missing, ambiguous or duplicated")
		}
		seen[row.Code+"/"+row.Side] = true
		rows = append(rows, row)
	}
	return rows, nil
}

// orderRow is the full provider order. Unknown instructions remain visible to
// the engine as outside orders, which freeze execution.
type orderRow struct {
	receiptRow
	Raw []byte `json:"-"`
}

func (r orderRow) createdMicros() int64 { n, _ := r.Created.Int64(); return n }
func (r orderRow) updatedMicros() int64 { n, _ := r.Updated.Int64(); return n }

func (r orderRow) remark() string {
	if r.Remark == nil {
		return ""
	}
	return *r.Remark
}

// terminalOrderStatuses are the Moomoo order states that can no longer change.
var terminalOrderStatuses = []string{"FILLED_ALL", "CANCELLED_ALL", "CANCELLED_PART", "FAILED", "SUBMIT_FAILED", "DISABLED", "DELETED"}

func (r orderRow) terminal() bool { return slices.Contains(terminalOrderStatuses, r.Status) }

// readOrderRows reads one paginated order resource and preserves every field.
func (c *Client) readOrderRows(ctx context.Context, account, resource string, query url.Values) ([]orderRow, error) {
	evidence, _, err := collectOrderPages(ctx, func(ctx context.Context, q url.Values) (json.RawMessage, error) {
		return c.readGET(ctx, "/api/v1.0/accounts/"+account+"/"+resource, q)
	}, query)
	if err != nil {
		return nil, err
	}
	rows := make([]orderRow, 0, len(evidence))
	for _, e := range evidence {
		var row receiptRow
		if exactJSON(e.Raw, &row) != nil || !row.agrees(e) {
			return nil, errors.New("Moomoo order row differs from its provider evidence")
		}
		rows = append(rows, orderRow{receiptRow: row, Raw: e.Raw})
	}
	return rows, nil
}

// accountOrders returns every uncompleted order (the documented /orders scope,
// which also keeps fills and cancels for 24 hours) plus every order created
// since the cutoff. /orders is always read fresh. The first history read covers
// the whole cutoff window; later reads cover only the last hour and back to the
// oldest order still uncompleted, because terminal orders never change and
// anything newer is in /orders. History is reused for at most 25 seconds.
func (s *Source) accountOrders(ctx context.Context, account string, since, now time.Time) ([]orderRow, error) {
	recent, err := s.Client.readOrderRows(ctx, account, "orders", url.Values{"trd_market": {"US"}, "page_size": {"100"}})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	cache, at := s.history, s.historyAt
	s.mu.Unlock()
	if cache == nil || now.Before(at) || now.Sub(at) > 25*time.Second {
		start := since
		if cache != nil {
			start = at.Add(-time.Hour)
			for _, row := range cache {
				if created := time.UnixMicro(row.createdMicros()); !row.terminal() && created.Before(start) {
					start = created
				}
			}
			if start.Before(since) {
				start = since
			}
		}
		rows, err := s.Client.readOrderRows(ctx, account, "orders_history", url.Values{"trd_market": {"US"}, "page_size": {"100"},
			"start": {strconv.FormatInt(start.UnixMicro(), 10)}, "end": {strconv.FormatInt(now.UnixMicro(), 10)}})
		if err != nil {
			return nil, err
		}
		merged, err := mergeOrderRows(slices.Collect(maps.Values(cache)), rows)
		if err != nil {
			return nil, err
		}
		cache = make(map[string]orderRow, len(merged))
		for _, row := range merged {
			cache[row.ID] = row
		}
		s.mu.Lock()
		s.history, s.historyAt = cache, now
		s.mu.Unlock()
	}
	return mergeOrderRows(recent, slices.Collect(maps.Values(cache)))
}

// mergeOrderRows keeps the newest update of each order. Equal update clocks
// with different facts are ambiguous.
func mergeOrderRows(recent, history []orderRow) ([]orderRow, error) {
	byID := map[string]orderRow{}
	for _, row := range append(recent, history...) {
		prior, seen := byID[row.ID]
		if !seen || row.updatedMicros() > prior.updatedMicros() {
			byID[row.ID] = row
			continue
		}
		if row.updatedMicros() == prior.updatedMicros() && (row.Status != prior.Status || !sameRemark(row.Dealt, prior.Dealt) || row.Price != prior.Price || row.Quantity != prior.Quantity) {
			return nil, errors.New("Moomoo recent and historical order facts conflict")
		}
	}
	rows := make([]orderRow, 0, len(byID))
	for _, row := range byID {
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b orderRow) int {
		return cmp.Or(cmp.Compare(a.createdMicros(), b.createdMicros()), strings.Compare(a.ID, b.ID))
	})
	return rows, nil
}

// ownedContract is an option this journal sold, with the time of its own
// opening order. Lifecycle legs created before that time cannot be ours.
type ownedContract struct {
	code   optionCode
	opened time.Time
}

func ownedContracts(st wheel.State) map[string]ownedContract {
	owned := map[string]ownedContract{}
	add := func(id string, opened time.Time) {
		code, err := parseOptionCode(id)
		if err != nil || opened.IsZero() {
			return
		}
		if prior, seen := owned[id]; !seen || opened.Before(prior.opened) {
			owned[id] = ownedContract{code: code, opened: opened}
		}
	}
	for _, r := range st.CompletedOrders {
		if !r.Closing && r.Filled == 1 {
			add(r.OptionID, r.At)
		}
	}
	for _, p := range st.Positions {
		add(p.PutID, p.PutOpenedAt)
		add(p.CallID, p.CallOpenedAt)
	}
	for _, in := range st.Intents {
		if !in.Candidate.Closing {
			add(in.Candidate.ID, in.At)
		}
	}
	return owned
}

// settlement is an observed Moomoo expiration/assignment leg pair for an owned
// contract: the option BUY_BACK at zero and, for assignment, the stock leg at
// exactly the strike with the same whole-second creation clock and no remark.
type settlement struct {
	symbol string
	at     int64
	price  wheel.Cents // Stock leg price; zero for expiration.
	stock  string      // BUY for put assignment, SELL for call-away.
}

func systemLeg(r orderRow) bool {
	return r.remark() == "" && r.Type == "LIMIT" && r.TIF == "DAY" && r.Status == "FILLED_ALL" && r.createdMicros()%1_000_000 == 0 &&
		r.Currency == "USD" && r.Exchange == "US"
}

func classifySettlements(rows []orderRow, owned map[string]ownedContract) (map[string]bool, []settlement) {
	excluded := map[string]bool{}
	var found []settlement
	for _, r := range rows {
		contract, mine := owned[r.Code]
		if !mine || !systemLeg(r) || r.SecurityType != "OPTION" || r.Side != "BUY_BACK" || r.Dealt == nil || r.Average == nil {
			continue
		}
		price, err := wheel.ParseCents(r.Price)
		qty, qtyErr := receiptContracts(r.Quantity)
		dealt, dealtErr := receiptContracts(*r.Dealt)
		average, avgErr := wheel.ParseCents(cmp.Or(*r.Average, "0"))
		if err != nil || price != 0 || qtyErr != nil || qty != 1 || dealtErr != nil || dealt != 1 || avgErr != nil || average != 0 ||
			time.UnixMicro(r.createdMicros()).Before(contract.opened) {
			continue
		}
		leg := settlement{symbol: contract.code.Root, at: r.createdMicros()}
		want := "BUY"
		if contract.code.Kind == "call" {
			want = "SELL"
		}
		var stock []orderRow
		for _, s := range rows {
			if s.createdMicros() == r.createdMicros() && s.Code == "US."+contract.code.Root && s.SecurityType == "STOCK" && systemLeg(s) {
				stock = append(stock, s)
			}
		}
		if len(stock) > 1 {
			continue // Several same-clock stock legs cannot be attributed; keep all visible.
		}
		if len(stock) == 1 {
			s := stock[0]
			p, pErr := wheel.ParseCents(s.Price)
			q, qErr := wholeQuantity(s.Quantity)
			if s.Side != want || pErr != nil || p != contract.code.Strike || qErr != nil || q != 100 || s.Dealt == nil || s.Average == nil {
				continue
			}
			d, dErr := wholeQuantity(*s.Dealt)
			a, aErr := wheel.ParseCents(*s.Average)
			if dErr != nil || d != 100 || aErr != nil || a != p {
				continue
			}
			leg.price, leg.stock = p, want
			excluded[s.ID] = true
		}
		excluded[r.ID] = true
		found = append(found, leg)
	}
	return excluded, found
}

func orderState(status string, filled int) (string, error) {
	switch status {
	case "FILLED_ALL":
		return "filled", nil
	case "CANCELLED_ALL", "CANCELLED_PART", "DISABLED", "DELETED":
		return "cancelled", nil
	case "FAILED", "SUBMIT_FAILED":
		if filled != 0 {
			return "", errors.New("failed Moomoo order reports a fill")
		}
		return "failed", nil
	}
	// Every other provider state, including unknown ones, remains working.
	return strings.ToLower(status), nil
}

func (r orderRow) wheelOrder() (wheel.Order, error) {
	created, updated := time.UnixMicro(r.createdMicros()).UTC(), time.UnixMicro(r.updatedMicros()).UTC()
	o := wheel.Order{ID: r.ID, Type: strings.ToLower(r.Type), CreatedAt: created, LastTransactionAt: updated}
	if validSubmissionRef(r.remark()) {
		o.Ref = r.remark()
	}
	switch r.TIF {
	case "DAY":
		o.TimeInForce = "gfd"
	case "GTC":
		o.TimeInForce = "gtc"
	default:
		o.TimeInForce = strings.ToLower(r.TIF)
	}
	quantity, err := wholeQuantity(r.Quantity)
	if err != nil || quantity < 1 || quantity > 1_000_000 {
		return o, errors.New("Moomoo order quantity is not an exact whole number")
	}
	filled := int64(0)
	if r.Dealt != nil && *r.Dealt != "" {
		if filled, err = wholeQuantity(*r.Dealt); err != nil || filled < 0 || filled > quantity {
			return o, errors.New("Moomoo order fill quantity is invalid")
		}
	}
	// A completed fill must report exactly what filled; missing fill facts on a
	// terminal order would otherwise read as an unfilled order.
	if r.Status == "FILLED_ALL" && (r.Dealt == nil || filled != quantity) || r.Status == "CANCELLED_PART" && (r.Dealt == nil || filled == 0 || filled >= quantity) {
		return o, errors.New("Moomoo terminal order fill facts are missing or contradictory")
	}
	o.Quantity, o.Filled = int(quantity), int(filled)
	if o.State, err = orderState(r.Status, o.Filled); err != nil {
		return o, err
	}
	if r.Price != "" {
		if o.Limit, err = wheel.ParseCents(r.Price); err != nil {
			return o, errors.New("Moomoo order price is not exact cents")
		}
	}
	switch r.Side {
	case "SELL_SHORT":
		o.Side, o.Effect = "sell", "open"
	case "BUY_BACK":
		o.Side, o.Effect = "buy", "close"
	case "BUY":
		o.Side, o.Effect = "buy", "open"
	case "SELL":
		o.Side, o.Effect = "sell", "close"
	default:
		o.Side = strings.ToLower(r.Side)
	}
	switch {
	case r.SecurityType == "STOCK" && strings.HasPrefix(r.Code, "US.") && len(r.Code) > 3:
		o.Symbol = strings.TrimPrefix(r.Code, "US.")
	case r.SecurityType == "OPTION":
		code, err := parseOptionCode(r.Code)
		if err != nil {
			return o, err
		}
		o.Symbol, o.OptionID, o.Kind, o.Strike = code.Root, r.Code, code.Kind, code.Strike
	default:
		// Multi-leg, warrants and other instruments are never AutoWheel's.
		o.Symbol, o.OptionID = r.Code, r.Code
	}
	return o, nil
}

// maxPositionUnits bounds shares or contracts in one position row, keeping every
// strike×100×quantity and coverage product far inside int64.
const maxPositionUnits = 1_000_000

// workingReserve is the most cash an uncompleted order can still consume.
// Short calls are bounded only by stock coverage, checked by the caller.
func workingReserve(o wheel.Order, optionFee wheel.Cents) (wheel.Cents, error) {
	if o.Terminal() || o.Side == "sell" && o.Effect == "close" {
		return 0, nil
	}
	remaining := wheel.Cents(o.Quantity - o.Filled)
	if remaining <= 0 {
		return 0, nil
	}
	if o.Side == "buy" {
		if o.Type != "limit" || o.Limit <= 0 || o.Limit > wheel.MaxMoney/100 {
			return 0, errors.New("uncompleted Moomoo buy order has no bounded limit")
		}
		if o.OptionID == "" {
			return o.Limit * remaining, nil
		}
		return o.Limit*100*remaining + optionFee*remaining, nil
	}
	if o.Side == "sell" && o.OptionID != "" && o.Kind == "put" && o.Strike > 0 {
		return o.Strike * 100 * remaining, nil
	}
	if o.Side == "sell" && o.OptionID != "" && o.Kind == "call" {
		return 0, nil
	}
	return 0, errors.New("uncompleted Moomoo short sale or call obligation cannot be bounded")
}

func addMoney(total *wheel.Cents, amount wheel.Cents) error {
	if amount < 0 || amount > wheel.MaxMoney-*total {
		return errors.New("Moomoo obligations exceed money limits")
	}
	*total += amount
	return nil
}

// externalCapital is ceil(qty*cost) using exact rationals; display only.
func externalCapital(quantity, cost string) wheel.Cents {
	q, qOK := new(big.Rat).SetString(quantity)
	c, cOK := new(big.Rat).SetString(cost)
	if !qOK || !cOK || q.Sign() < 0 || c.Sign() < 0 {
		return 0
	}
	cents := new(big.Rat).Mul(new(big.Rat).Mul(q.Abs(q), c), big.NewRat(100, 1))
	n := new(big.Int).Quo(cents.Num(), cents.Denom())
	if new(big.Rat).SetInt(n).Cmp(cents) < 0 {
		n.Add(n, big.NewInt(1))
	}
	if !n.IsInt64() || n.Int64() > int64(wheel.MaxMoney) {
		return wheel.MaxMoney
	}
	return wheel.Cents(n.Int64())
}

// Snapshot is the complete account view. AvailableCash is settled USD cash
// after funds on hold, assets in transit, every short-put obligation and every
// uncompleted order; obligations beyond settled cash would require borrowing
// and block execution entirely.
func (s *Source) Snapshot(ctx context.Context, owned wheel.State) (wheel.Snapshot, error) {
	started := s.now()
	account := s.Config.AccountID
	cutoff := s.Config.Strategy.ExclusiveOrdersSince
	if owned.Account != "" && owned.Account != account || cutoff.IsZero() || cutoff.After(started) {
		return wheel.Snapshot{}, errors.New("Moomoo snapshot account or order cutoff is unqualified")
	}
	if _, err := s.Client.authorizedUSAccount(ctx, account); err != nil {
		return wheel.Snapshot{}, err
	}
	funds, err := s.Client.readGET(ctx, "/api/v1.0/accounts/"+account+"/funds", url.Values{"currency": {"USD"}})
	if err != nil {
		return wheel.Snapshot{}, err
	}
	cash, err := mapCashInputs(account, started, funds)
	if err != nil {
		return wheel.Snapshot{}, err
	}
	// Blank balances for markets this account does not use are not debits; an
	// unparseable or unrecognized currency balance could hide one, so it blocks.
	if len(cash.CurrencyDebitFields) != 0 || len(cash.UnreadableCurrencyFields) != 0 {
		return wheel.Snapshot{}, errors.New("Moomoo account has a negative or unreadable currency balance; borrowing is not permitted")
	}
	rawPositions, err := s.Client.readGET(ctx, "/api/v1.0/accounts/"+account+"/positions", nil)
	if err != nil {
		return wheel.Snapshot{}, err
	}
	positions, err := parsePositionRows(rawPositions)
	if err != nil {
		return wheel.Snapshot{}, err
	}
	rows, err := s.accountOrders(ctx, account, cutoff, started)
	if err != nil {
		return wheel.Snapshot{}, err
	}
	snapshot, err := buildSnapshot(account, s.Config.Strategy, owned, cash, positions, rows, started)
	if err != nil {
		return wheel.Snapshot{}, err
	}
	if elapsed := s.now().Sub(started); elapsed < 0 || elapsed > 20*time.Second {
		return wheel.Snapshot{}, errors.New("Moomoo account snapshot exceeded its freshness budget")
	}
	return snapshot, nil
}

func buildSnapshot(account string, strategy wheel.Config, owned wheel.State, cash *CashInputs, positions []positionRow, rows []orderRow, at time.Time) (wheel.Snapshot, error) {
	cutoff := strategy.ExclusiveOrdersSince
	fee := strategy.Fee
	snapshot := wheel.Snapshot{OrderCoveragePolicy: wheel.CompleteMoomooOrders, At: at, Account: account, Shares: []wheel.Shares{}, Options: []wheel.Option{}, Orders: []wheel.Order{}}
	managed := map[string]bool{}
	for symbol := range owned.Positions {
		managed[symbol] = true
	}
	for symbol := range owned.Intents {
		managed[symbol] = true
	}
	contracts := ownedContracts(owned)
	excluded, settlements := classifySettlements(rows, contracts)
	var reserved wheel.Cents
	external := map[string]bool{}
	// Long shares not yet committed to a short call, in quantity micros. Every
	// short call position and uncompleted short call order consumes coverage.
	uncovered := map[string]int64{}
	for _, row := range positions {
		if row.SecurityType == "STOCK" && row.Side == "LONG" {
			if q, err := quantityMicros(row.Quantity); err == nil && q > 0 && q <= maxPositionUnits*1_000_000 {
				uncovered[strings.TrimPrefix(row.Code, "US.")] += q
			}
		}
	}
	cover := func(symbol string, contracts int64) error {
		if contracts < 0 || contracts > maxPositionUnits {
			return errors.New("Moomoo short call quantity exceeds the reviewed bound")
		}
		need := contracts * 100 * 1_000_000
		if uncovered[symbol] < need {
			return errors.New("Moomoo short call is not covered by long stock; freeze")
		}
		uncovered[symbol] -= need
		return nil
	}
	for _, row := range positions {
		magnitude, err := quantityMicros(row.Quantity)
		if err != nil {
			return wheel.Snapshot{}, errors.New("Moomoo position quantity is not an exact decimal")
		}
		if row.Side == "LONG" && magnitude < 0 {
			return wheel.Snapshot{}, errors.New("Moomoo long position reports a negative quantity")
		}
		magnitude = max(magnitude, -magnitude)
		if magnitude == 0 {
			continue
		}
		// Bound units before any checked money or coverage arithmetic.
		if magnitude > maxPositionUnits*1_000_000 {
			return wheel.Snapshot{}, errors.New("Moomoo position quantity exceeds the reviewed bound")
		}
		if row.Currency != "USD" || !strings.HasPrefix(row.Code, "US.") {
			return wheel.Snapshot{}, errors.New("Moomoo non-US or non-USD position is outside the reviewed account scope")
		}
		switch row.SecurityType {
		case "STOCK":
			symbol := strings.TrimPrefix(row.Code, "US.")
			if row.Side == "SHORT" {
				return wheel.Snapshot{}, errors.New("Moomoo short stock is an unbounded obligation; freeze")
			}
			if !managed[symbol] {
				external[symbol] = true
				snapshot.ExternalCapital = min(wheel.MaxMoney, snapshot.ExternalCapital+externalCapital(row.Quantity, row.Cost))
				continue
			}
			quantity, qErr := wholeQuantity(row.Quantity)
			available, aErr := wholeQuantity(row.Available)
			if qErr != nil || aErr != nil || quantity > 1_000_000 || available < 0 || available > quantity {
				return wheel.Snapshot{}, errors.New("managed Moomoo stock quantity is not an exact whole lot")
			}
			// Only the broker's own assignment fill proves this lot's basis. Without
			// it the cost stays zero, which freezes reconciliation.
			cost := assignmentCost(symbol, owned.Positions[symbol], settlements)
			snapshot.Shares = append(snapshot.Shares, wheel.Shares{Symbol: symbol, Quantity: int(quantity), Available: int(available), Cost: cost})
		case "OPTION":
			code, err := parseOptionCode(row.Code)
			if err != nil {
				return wheel.Snapshot{}, err
			}
			if magnitude%1_000_000 != 0 {
				return wheel.Snapshot{}, errors.New("Moomoo option quantity is fractional")
			}
			contractsHeld := magnitude / 1_000_000
			if row.Side == "SHORT" && code.Kind == "call" {
				if err := cover(code.Root, contractsHeld); err != nil {
					return wheel.Snapshot{}, err
				}
			}
			if _, mine := contracts[row.Code]; mine && managed[code.Root] {
				if row.Side != "SHORT" {
					return wheel.Snapshot{}, errors.New("owned Moomoo option is reported long; freeze")
				}
				snapshot.Options = append(snapshot.Options, code.option(row.Code, int(contractsHeld)))
				if code.Kind == "put" {
					if err := addMoney(&reserved, code.Strike*100*wheel.Cents(contractsHeld)); err != nil {
						return wheel.Snapshot{}, err
					}
				}
				continue
			}
			external[code.Root] = true
			if row.Side == "SHORT" && code.Kind == "put" {
				if err := addMoney(&reserved, code.Strike*100*wheel.Cents(contractsHeld)); err != nil {
					return wheel.Snapshot{}, err
				}
			}
		default:
			if row.Side == "SHORT" {
				return wheel.Snapshot{}, errors.New("Moomoo short instrument is outside the reviewed scope; freeze")
			}
			external[strings.TrimPrefix(row.Code, "US.")] = true
		}
	}
	for _, row := range rows {
		if excluded[row.ID] {
			continue
		}
		// The cutoff audit proved earlier orders terminal; an earlier order that
		// is still working or changed after the cutoff stays visible and freezes.
		if row.terminal() && row.createdMicros() < cutoff.UnixMicro() && row.updatedMicros() < cutoff.UnixMicro() {
			continue
		}
		order, err := row.wheelOrder()
		if err != nil {
			return wheel.Snapshot{}, err
		}
		amount, err := workingReserve(order, fee)
		if err != nil {
			return wheel.Snapshot{}, err
		}
		if !order.Terminal() && order.Side == "sell" && order.Effect == "open" && order.Kind == "call" {
			if err := cover(order.Symbol, int64(order.Quantity-order.Filled)); err != nil {
				return wheel.Snapshot{}, err
			}
		}
		if err := addMoney(&reserved, amount); err != nil {
			return wheel.Snapshot{}, err
		}
		snapshot.Orders = append(snapshot.Orders, order)
	}
	available, err := availableCash(cash, reserved)
	if err != nil {
		return wheel.Snapshot{}, err
	}
	snapshot.AvailableCash = available
	for symbol := range external {
		snapshot.ExternalSymbols = append(snapshot.ExternalSymbols, symbol)
	}
	slices.Sort(snapshot.ExternalSymbols)
	slices.SortFunc(snapshot.Shares, func(a, b wheel.Shares) int { return strings.Compare(a.Symbol, b.Symbol) })
	slices.SortFunc(snapshot.Options, func(a, b wheel.Option) int { return strings.Compare(a.ID, b.ID) })
	return snapshot, nil
}

// availableCash removes every obligation exactly once. Moomoo enforces
// cash-secured puts against net cash power (max_sell_short tracks it, not margin
// power), so its own holds appear as us_cash - us_net_cash_power. Only the
// obligations it does not visibly hold are subtracted again, and the result
// never exceeds net cash power. If withdrawable cash also reflects those holds,
// the overlap can only understate cash; then no new risk is funded, and the
// snapshot fails only when the cash ledger itself cannot cover the obligations.
func availableCash(cash *CashInputs, reserved wheel.Cents) (wheel.Cents, error) {
	power, held := cash.USDCashFloor, wheel.Cents(0)
	if cash.NetCashPowerKnown {
		power = cash.NetCashPowerFloor
		held = max(0, cash.USDCashFloor-cash.NetCashPowerFloor)
	}
	available := min(cash.LiquidityComponent, power) - max(0, reserved-held)
	if available >= 0 {
		return available, nil
	}
	if reserved > cash.USDCashFloor {
		return 0, errors.New("Moomoo obligations exceed the cash ledger; margin borrowing is not permitted")
	}
	return 0, nil
}

// assignmentCost is the broker's own assignment fill price for the managed
// lifecycle's latest put assignment. Moomoo's position cost is a running
// average across the account's history and cannot prove this lot's basis.
func assignmentCost(symbol string, p wheel.Position, legs []settlement) wheel.Cents {
	var cost wheel.Cents
	var at int64
	for _, leg := range legs {
		if leg.symbol == symbol && leg.stock == "BUY" && !p.PutOpenedAt.IsZero() && leg.at >= p.PutOpenedAt.UnixMicro() && leg.at >= at {
			cost, at = leg.price, leg.at
		}
	}
	return cost
}

func (s *Source) String() string { return fmt.Sprintf("moomoo-source(%s)", s.Config.Deployment) }
