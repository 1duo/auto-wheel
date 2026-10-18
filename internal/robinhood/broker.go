package robinhood

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"autowheel/internal/sharedbroker"
	"autowheel/internal/wheel"
)

type Broker struct {
	MCP              BrokerClient
	Config           wheel.Config
	BeforeSubmit     func() error
	AccountInfo      *AccountInfo
	proofMu          sync.Mutex
	proofs           map[string]bool
	SubmitMu         *sync.Mutex
	SnapshotProvider func(context.Context) (wheel.Snapshot, error)
	SubmitProvider   func(context.Context, wheel.Candidate, string) (string, error)
}

type BrokerClient interface {
	Call(context.Context, string, map[string]any) (any, error)
	Supports(string, string) bool
}

// AccountInfo is display-only evidence, never input to planning or ownership.
// It remains useful when a complete tradable snapshot cannot be constructed.
type AccountInfo struct {
	At                      time.Time     `json:"at"`
	AccountLast4            string        `json:"account_last4"`
	OptionsLevel            string        `json:"options_level"`
	AvailableCash           wheel.Cents   `json:"available_cash_cents"`
	Holdings                []HoldingInfo `json:"holdings"`
	SnapshotComplete        bool          `json:"snapshot_complete"`
	ExecutionSnapshotUsable bool          `json:"execution_snapshot_usable"`
	OrderCoveragePolicy     string        `json:"order_coverage_policy,omitempty"`
	CoverageWarning         string        `json:"coverage_warning,omitempty"`
}

type HoldingInfo struct {
	Symbol    string `json:"symbol"`
	Quantity  string `json:"quantity"`
	Available string `json:"available"`
}

func (b *Broker) scoped(tool string, args object) (object, error) {
	if b.MCP.Supports(tool, "account_number") {
		args["account_number"] = b.Config.Account
		return args, nil
	}
	return nil, fmt.Errorf("%s cannot be bound to configured account", tool)
}

func (b *Broker) Snapshot(ctx context.Context) (wheel.Snapshot, error) {
	if b.SnapshotProvider != nil {
		return b.SnapshotProvider(ctx)
	}
	started := time.Now()
	s := wheel.Snapshot{Account: b.Config.Account}
	if b.Config.OrderCoveragePolicy == "exclusive_standard" {
		if err := b.Config.Validate(); err != nil {
			return s, err
		}
	}
	accounts, err := b.collection(ctx, "get_accounts", object{}, "accounts")
	if err != nil {
		return s, err
	}
	var account object
	for _, a := range accounts {
		if text(a, "account_number") == s.Account {
			if account != nil {
				return s, errors.New("duplicate bound account")
			}
			account = a
		}
	}
	if account == nil || account["agentic_allowed"] != true || text(account, "state") != "active" || account["deactivated"] == true || account["permanently_deactivated"] == true {
		return s, errors.New("configured account is not an active agentic account")
	}
	if text(account, "type") != "cash" {
		return s, errors.New("configured account is not explicitly a cash account")
	}
	if enabled, present := account["margin_enabled"]; present && enabled != false {
		return s, errors.New("cash account margin flag is enabled or unproven")
	}
	level := text(account, "option_level")
	if level == "" {
		level = text(account, "options_level")
	}
	if level == "" {
		level = text(account, "option_trading_level")
	}
	if level != "2" && level != "3" && level != "option_level_2" && level != "option_level_3" {
		return s, errors.New("Level 2+ options approval missing")
	}
	args, err := b.scoped("get_portfolio", object{})
	if err != nil {
		return s, err
	}
	v, err := b.MCP.Call(ctx, "get_portfolio", args)
	if err != nil {
		return s, err
	}
	p, err := single(v)
	if err != nil {
		return s, err
	}
	cash, err := money(p, "cash")
	if err != nil {
		return s, err
	}
	bp, ok := p["buying_power"].(object)
	if !ok {
		return s, errors.New("unleveraged buying power missing")
	}
	unleveraged, err := money(bp, "unleveraged_buying_power")
	if err != nil {
		return s, err
	}
	pending, err := money(p, "pending_deposits")
	if err != nil {
		return s, err
	}
	unsettled, err := money(account, "unsettled_funds")
	if err != nil {
		return s, err
	}
	s.AvailableCash = max(0, min(cash, unleveraged)-pending-unsettled)
	if b.Config.OrderCoveragePolicy == "exclusive_standard" {
		for _, key := range []string{"crypto_value", "event_contracts_value", "fixed_income_value", "futures_value", "mutual_funds_value"} {
			value, err := money(p, key)
			if err != nil || value != 0 {
				return s, fmt.Errorf("unsupported or unproven portfolio category %s in exclusive standard account", key)
			}
		}
	}
	args, err = b.scoped("get_equity_positions", object{})
	if err != nil {
		return s, err
	}
	positions, err := b.collection(ctx, "get_equity_positions", args, "positions")
	if err != nil {
		return s, err
	}
	info := &AccountInfo{At: started, AccountLast4: b.Config.Account[max(0, len(b.Config.Account)-4):], OptionsLevel: level, AvailableCash: s.AvailableCash}
	for _, p := range positions {
		q := text(p, "quantity")
		n, qerr := integer(p, "quantity")
		if qerr != nil || n > 0 {
			info.Holdings = append(info.Holdings, HoldingInfo{Symbol: text(p, "symbol"), Quantity: q, Available: text(p, "shares_available_for_sells")})
		}
	}
	b.AccountInfo = info
	for _, p := range positions {
		symbol := text(p, "symbol")
		if symbol == "" {
			return s, errors.New("equity position symbol missing")
		}
		qtext := text(p, "quantity")
		availableText := text(p, "shares_available_for_sells")
		if !decimalQuantity(availableText) {
			return s, errors.New("available equity quantity missing or invalid")
		}
		qty, qtyOK := new(big.Rat).SetString(qtext)
		avail, availOK := new(big.Rat).SetString(availableText)
		if !qtyOK || !availOK || avail.Cmp(qty) > 0 {
			return s, errors.New("available shares exceed held shares")
		}
		if !decimalQuantity(qtext) {
			return s, errors.New("invalid equity quantity")
		}
		if qty.Sign() == 0 {
			continue
		}
		q, qerr := integer(p, "quantity")
		available, aerr := integer(p, "shares_available_for_sells")
		cost, cerr := money(p, "average_buy_price")
		if qerr != nil || aerr != nil || cerr != nil {
			exposure, err := positionCapital(qtext, text(p, "average_buy_price"))
			if err != nil || s.ExternalCapital > wheel.MaxMoney-exposure {
				return s, errors.New("external inventory capital is unknown or out of range")
			}
			s.ExternalCapital += exposure
			s.ExternalSymbols = append(s.ExternalSymbols, symbol)
			continue
		}
		s.Shares = append(s.Shares, wheel.Shares{Symbol: symbol, Quantity: q, Available: available, Cost: cost})
	}
	args, err = b.scoped("get_option_positions", object{})
	if err != nil {
		return s, err
	}
	positions, err = b.collection(ctx, "get_option_positions", args, "positions")
	if err != nil {
		return s, err
	}
	for _, p := range positions {
		q, err := integer(p, "quantity")
		if err != nil {
			return s, err
		}
		// Assignment can begin while the short still has a nonzero quantity.
		// Neither that quantity nor a vanished position proves settlement.
		for _, key := range []string{"pending_assignment_quantity", "pending_expiration_quantity", "pending_exercise_quantity"} {
			pending, err := integer(p, key)
			if err != nil || pending != 0 {
				return s, errors.New("option settlement facts are incomplete or pending")
			}
		}
		if q == 0 {
			continue
		}
		if text(p, "type") != "short" {
			return s, errors.New("account has unsupported long options; freeze")
		}
		op, err := b.positionOption(ctx, p)
		if err != nil {
			return s, err
		}
		op.Quantity = q
		s.Options = append(s.Options, op)
	}
	for _, tool := range []string{"get_option_orders", "get_equity_orders"} {
		args, err = b.scoped(tool, object{})
		if err != nil {
			return s, err
		}
		orders, err := b.collection(ctx, tool, args, "orders")
		if err != nil {
			return s, err
		}
		for _, p := range orders {
			o, err := b.order(ctx, p, tool == "get_option_orders")
			if err != nil {
				return s, err
			}
			s.Orders = append(s.Orders, o)
		}
	}
	if err = b.otherCashObligations(ctx, account); err != nil {
		return s, err
	}
	if err = b.orderAdmissionCoverage(ctx); err != nil {
		return s, err
	}
	if time.Since(started) > time.Duration(b.Config.MaxQuoteAgeSeconds)*time.Second {
		return s, errors.New("account snapshot collection exceeded freshness limit")
	}
	s.At = started
	s.OrderCoveragePolicy = b.Config.OrderCoveragePolicy
	info.OrderCoveragePolicy = b.Config.OrderCoveragePolicy
	info.ExecutionSnapshotUsable = true
	info.SnapshotComplete = b.Config.OrderCoveragePolicy != "exclusive_standard"
	if !info.SnapshotComplete {
		info.CoverageWarning = wheel.StandardOrderWarning
	}
	return s, nil
}

// Account metadata documents absence of rhc_account_number as no crypto link.
// A present but malformed link never means an empty account. The linked account
// may reserve the same cash pool even when it holds no crypto inventory.
func (b *Broker) otherCashObligations(ctx context.Context, account object) error {
	link, present := account["rhc_account_number"]
	if !present {
		return nil
	}
	crypto, valid := link.(string)
	if !valid || crypto == "" || text(account, "rhs_account_number") == "" {
		return errors.New("linked crypto account identity is incomplete")
	}
	backend := sharedbroker.Backend{Client: b.MCP, Account: b.Config.Account}
	args := object{"rhs_account_number": text(account, "rhs_account_number")}
	orders, err := backend.Collection(ctx, "get_crypto_orders", "results", args)
	if err != nil {
		return err
	}
	for _, order := range orders {
		if !sharedbroker.Terminal(text(order, "state")) {
			return errors.New("working or unrecognized crypto order requires operator review")
		}
		if b.Config.OrderCoveragePolicy == "exclusive_standard" {
			created, cErr := timestamp(order, "created_at")
			updated, uErr := timestamp(order, "updated_at")
			if cErr != nil || uErr != nil || !created.Before(b.Config.ExclusiveOrdersSince) || !updated.Before(b.Config.ExclusiveOrdersSince) {
				return errors.New("outside crypto activity since native retirement or unproven timing; freeze")
			}
		}
	}
	positions, err := backend.Collection(ctx, "get_crypto_positions", "results", object{"rhs_account_number": text(account, "rhs_account_number")})
	if err != nil {
		return err
	}
	for _, position := range positions {
		for _, field := range []string{"quantity", "quantity_transferable", "quantity_held_for_sell"} {
			quantity, err := sharedbroker.Decimal(text(position, field))
			if err != nil || quantity.Sign() != 0 {
				return errors.New("unsupported crypto inventory, transfer/sale hold or unproven quantity")
			}
		}
	}
	return nil
}

// Pending approvals can become orders after a human acts. They must not share
// cash with a new wheel intent. Neither basic order history nor an unavailable
// advanced-order reader proves that contingent/OCO groups are absent.
func (b *Broker) orderAdmissionCoverage(ctx context.Context) error {
	args, err := b.scoped("get_trade_approval_setting", object{})
	if err != nil {
		return err
	}
	v, err := b.MCP.Call(ctx, "get_trade_approval_setting", args)
	if err != nil {
		return err
	}
	o, err := single(v)
	if err != nil {
		return err
	}
	setting, ok := o["setting"].(object)
	if !ok || text(setting, "account_number") != b.Config.Account || setting["human_must_approve_trades"] != false {
		return errors.New("automatic order admission requires broker-confirmed manual approvals off")
	}
	args, err = b.scoped("get_trade_approvals", object{})
	if err != nil {
		return err
	}
	approvals, err := b.collection(ctx, "get_trade_approvals", args, "approvals")
	if err != nil {
		return err
	}
	if len(approvals) != 0 {
		return errors.New("pending broker trade approvals require operator review")
	}
	if b.Config.OrderCoveragePolicy == "exclusive_standard" {
		return nil
	}
	if !b.MCP.Supports("get_advanced_orders", "") {
		return errors.New("advanced/OCO order coverage unavailable: MCP lacks get_advanced_orders")
	}
	args, err = b.scoped("get_advanced_orders", object{})
	if err != nil {
		return err
	}
	orders, err := b.collection(ctx, "get_advanced_orders", args, "orders")
	if err != nil {
		return err
	}
	// A group's terminal-looking state alone cannot prove that every child is
	// terminal. Only an explicit empty collection is supported until actual
	// provider group/child schemas and lifecycle receipts have been qualified.
	if len(orders) != 0 {
		return errors.New("advanced/OCO groups require qualified group and child-order reconciliation")
	}
	return nil
}

func (b *Broker) order(ctx context.Context, p object, option bool) (wheel.Order, error) {
	o := wheel.Order{ID: text(p, "id"), Ref: text(p, "ref_id"), State: text(p, "state"), Symbol: text(p, "chain_symbol"), Side: text(p, "side")}
	if o.Symbol == "" {
		o.Symbol = text(p, "symbol")
	}
	if o.ID == "" || o.State == "" {
		return o, errors.New("order identity or state missing")
	}
	var err error
	o.CreatedAt, err = timestamp(p, "created_at")
	if err != nil {
		return o, err
	}
	if text(p, "last_transaction_at") != "" {
		o.LastTransactionAt, err = timestamp(p, "last_transaction_at")
		if err != nil {
			return o, err
		}
	}
	if !option {
		if o.Symbol == "" {
			return o, errors.New("equity order symbol missing")
		}
		if !o.Terminal() {
			return o, fmt.Errorf("working equity order in %s requires operator review", o.Symbol)
		}
		// Equity history detects intervening manual trades by symbol and time.
		// Fractional quantities are not rounded or used as option coverage.
		return o, nil
	}
	o.Quantity, err = integer(p, "quantity")
	if err != nil {
		return o, err
	}
	o.Filled, err = integer(p, "processed_quantity")
	if err != nil {
		o.Filled, err = integer(p, "cumulative_quantity")
		if err != nil {
			return o, errors.New("order fill quantity missing")
		}
	}
	if option {
		legs, ok := p["legs"].([]any)
		if !ok || len(legs) != 1 {
			return o, errors.New("multi-leg or missing option order legs unsupported")
		}
		leg, ok := legs[0].(object)
		if !ok {
			return o, errors.New("invalid option order leg")
		}
		var op wheel.Option
		// Historical terminal orders need only identity for reconciliation. Open
		// orders also need contract metadata to reserve cash and symbol slots.
		o.OptionID = optionID(leg)
		o.Side = text(leg, "side")
		o.Effect = text(leg, "position_effect")
		if !o.Terminal() {
			op, err = b.positionOption(ctx, leg)
			if err != nil {
				return o, err
			}
			o.Symbol = op.Symbol
			o.Kind = op.Kind
			o.Strike = op.Strike
		}
		if o.OptionID == "" {
			return o, errors.New("order option identity missing")
		}
	}
	if o.Symbol == "" {
		return o, errors.New("order symbol missing")
	}
	if o.Quantity < 1 || o.Filled < 0 || o.Filled > o.Quantity {
		return o, errors.New("invalid order quantities")
	}
	return o, nil
}

func (b *Broker) positionOption(ctx context.Context, p object) (wheel.Option, error) {
	id := optionID(p)
	if id == "" {
		return wheel.Option{}, errors.New("option instrument ID missing")
	}
	args := object{}
	if b.MCP.Supports("get_option_instruments", "ids") {
		args["ids"] = id
	} else if b.MCP.Supports("get_option_instruments", "option_ids") {
		args["option_ids"] = []string{id}
	} else {
		return wheel.Option{}, errors.New("MCP lacks instrument lookup by ID; cannot value existing exposure")
	}
	rows, err := b.collection(ctx, "get_option_instruments", args, "instruments")
	if err != nil {
		return wheel.Option{}, err
	}
	if len(rows) != 1 || text(rows[0], "id") != id {
		return wheel.Option{}, errors.New("instrument lookup identity mismatch")
	}
	return instrument(rows[0], id)
}

func instrument(p object, id string) (wheel.Option, error) {
	op := wheel.Option{ID: id, ChainID: text(p, "chain_id"), Symbol: text(p, "chain_symbol"), Kind: text(p, "type"), Expiry: text(p, "expiration_date")}
	if op.Symbol == "" {
		op.Symbol = text(p, "symbol")
	}
	var err error
	op.Strike, err = money(p, "strike_price")
	if err != nil {
		return op, err
	}
	mult, err := integer(p, "trade_value_multiplier")
	if err != nil || mult != 100 {
		return op, errors.New("nonstandard or unknown option multiplier")
	}
	if _, err = time.Parse("2006-01-02", op.Expiry); err != nil || op.ID == "" || op.Symbol == "" || (op.Kind != "put" && op.Kind != "call") {
		return op, errors.New("invalid option contract")
	}
	return op, nil
}

func (b *Broker) Quotes(ctx context.Context, symbol, kind string) ([]wheel.Quote, error) {
	chains, err := b.collection(ctx, "get_option_chains", object{"underlying_symbol": symbol}, "chains")
	if err != nil {
		return nil, err
	}
	if len(chains) != 1 || text(chains[0], "symbol") != symbol || text(chains[0], "id") == "" {
		return nil, errors.New("option chain identity mismatch")
	}
	chain := chains[0]
	// A 100 multiplier alone does not exclude adjusted deliverables. Require
	// exactly one 100-share underlying and zero cash deliverable in the chain.
	underlyings, ok := chain["underlying_instruments"].([]any)
	if !ok || len(underlyings) != 1 {
		return nil, errors.New("option deliverable structure unknown or adjusted")
	}
	underlying, ok := underlyings[0].(object)
	if !ok {
		return nil, errors.New("invalid option deliverable")
	}
	if text(underlying, "instrument") == "" || (text(underlying, "symbol") != "" && text(underlying, "symbol") != symbol) || chain["can_open_position"] != true {
		return nil, errors.New("underlying identity or chain opening permission missing")
	}
	mult, err := integer(chain, "trade_value_multiplier")
	if err != nil || mult != 100 {
		return nil, errors.New("unknown or adjusted chain multiplier")
	}
	if _, known := chain["cash_component"]; !known {
		return nil, errors.New("cash deliverable field missing")
	}
	if chain["cash_component"] != nil {
		cash, err := money(chain, "cash_component")
		if err != nil || cash != 0 {
			return nil, errors.New("cash/adjusted deliverable unsupported")
		}
	}
	dates, ok := chain["expiration_dates"].([]any)
	if !ok {
		return nil, errors.New("chain expiry list missing")
	}
	today, _ := time.Parse("2006-01-02", time.Now().In(wheel.Eastern).Format("2006-01-02"))
	nearest := ""
	for _, v := range dates {
		d, ok := v.(string)
		if !ok {
			return nil, errors.New("invalid expiry list")
		}
		at, err := time.Parse("2006-01-02", d)
		if err != nil {
			return nil, err
		}
		days := int(at.Sub(today).Hours() / 24)
		if days < 1 || days > b.Config.MaxDays {
			continue
		}
		if kind == "put" && at.Weekday() != time.Friday && at.Weekday() != time.Thursday {
			continue
		}
		if kind == "put" && at.Weekday() == time.Thursday {
			listed := false
			if weekly, ok := chain["weekly_expiration_dates"].([]any); ok {
				for _, x := range weekly {
					if x == d {
						listed = true
					}
				}
			}
			if !listed {
				continue
			}
		}
		if nearest == "" || d < nearest {
			nearest = d
		}
	}
	if nearest == "" {
		return nil, nil
	}
	contracts, err := b.collection(ctx, "get_option_instruments", object{"chain_id": text(chain, "id"), "expiration_dates": nearest, "type": kind, "state": "active", "tradability": "tradable"}, "instruments")
	if err != nil {
		return nil, err
	}
	if len(contracts) == 0 {
		return nil, errors.New("listed nearest expiry has no contract data")
	}
	if err := b.verifyDeliverable(ctx, chain, nearest, contracts, kind); err != nil {
		return nil, err
	}
	if len(contracts) > 1000 {
		return nil, errors.New("option chain exceeds contract limit")
	}
	stock, err := b.quoteRows(ctx, "get_equity_quotes", object{"symbols": []string{symbol}})
	if err != nil {
		return nil, err
	}
	if len(stock) != 1 || text(stock[0], "symbol") != symbol || stock[0]["has_traded"] != true || text(stock[0], "state") != "active" {
		return nil, errors.New("underlying quote identity mismatch")
	}
	spot, err := ceilingPrice(text(stock[0], "last_trade_price"))
	if err != nil {
		return nil, err
	}
	stockAt, err := timestamp(stock[0], "venue_last_trade_time")
	if err != nil {
		return nil, err
	}
	byID := map[string]object{}
	ids := make([]string, 0, len(contracts))
	for _, p := range contracts {
		id := text(p, "id")
		if id == "" || byID[id] != nil {
			return nil, errors.New("missing or duplicated contract ID")
		}
		byID[id] = p
		ids = append(ids, id)
	}
	var out []wheel.Quote
	for start := 0; start < len(ids); start += 50 {
		batch := ids[start:min(start+50, len(ids))]
		quotes, err := b.quoteRows(ctx, "get_option_quotes", object{"instrument_ids": batch})
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, p := range quotes {
			id := optionID(p)
			meta, ok := byID[id]
			if !ok || seen[id] || !slices.Contains(batch, id) {
				return nil, errors.New("quote identity mismatch or duplicate")
			}
			seen[id] = true
			op, err := instrument(meta, id)
			if err != nil {
				return nil, errors.New("invalid nearest-expiry option instrument")
			}
			if op.Strike <= 0 || op.Strike > wheel.MaxMoney/100 {
				return nil, errors.New("nearest-expiry option strike out of range")
			}
			if op.Symbol != symbol || op.Kind != kind || op.Expiry != nearest || text(meta, "chain_id") != text(chain, "id") || text(meta, "underlying_type") != "equity" {
				return nil, errors.New("contract identity differs from chain request")
			}
			q := wheel.Quote{Option: op, Spot: spot, UnderlyingAt: stockAt, Standard: true, Tradable: text(meta, "tradability") == "tradable" && text(meta, "state") == "active", Weekly: text(meta, "expiration_type") == "weekly"}
			q.At, err = timestamp(p, "updated_at")
			if err != nil {
				return nil, err
			}
			q.Bid, err = money(p, "bid_price")
			if err != nil {
				return nil, err
			}
			q.Ask, err = money(p, "ask_price")
			if err != nil {
				return nil, err
			}
			if q.Ask < q.Bid || q.Ask > wheel.MaxMoney/100 {
				return nil, errors.New("invalid nearest-expiry option book")
			}
			q.Delta, err = strconv.ParseFloat(text(p, "delta"), 64)
			if err != nil || math.IsNaN(q.Delta) || math.IsInf(q.Delta, 0) || math.Abs(q.Delta) > 1 || (kind == "put" && q.Delta > 0) || (kind == "call" && q.Delta < 0) {
				return nil, errors.New("invalid nearest-expiry option delta")
			}
			q.OI, err = integer(p, "open_interest")
			if err != nil {
				return nil, err
			}
			q.BidSize, err = integer(p, "bid_size")
			if err != nil {
				return nil, err
			}
			// Contract tick rules are authoritative. Unknown ticks cannot trade.
			ticks, ok := meta["min_ticks"].(object)
			if !ok {
				return nil, errors.New("nearest-expiry option ticks missing")
			}
			cut, err := money(ticks, "cutoff_price")
			if err != nil {
				return nil, err
			}
			key := "below_tick"
			if q.Bid >= cut {
				key = "above_tick"
			}
			q.Tick, err = money(ticks, key)
			if err != nil || q.Tick <= 0 {
				return nil, errors.New("invalid nearest-expiry option tick")
			}
			out = append(out, q)
		}
		if len(seen) != len(batch) {
			return nil, errors.New("incomplete option quote batch")
		}
	}
	return out, nil
}

func (b *Broker) orderArgs(a wheel.Candidate) object {
	side, effect, _, limit := a.OrderTerms()
	return object{"account_number": b.Config.Account, "legs": []object{{"option_id": a.ID, "side": side, "position_effect": effect, "ratio_quantity": 1}}, "quantity": "1", "price": limit.Dollars(), "type": "limit", "time_in_force": "gfd", "market_hours": "regular_hours"}
}

func (b *Broker) preview(ctx context.Context, a wheel.Candidate) (object, error) {
	args := b.orderArgs(a)
	args["chain_symbol"], args["underlying_type"] = a.Symbol, "equity"
	v, err := b.MCP.Call(ctx, "review_option_order", args)
	if err != nil {
		return nil, err
	}
	o, err := single(v)
	if err != nil {
		return nil, err
	}
	if err = reviewIdentity(o, b.Config.Account, a); err != nil {
		return nil, err
	}
	return o, nil
}

func reviewIdentity(o object, account string, a wheel.Candidate) error {
	side, effect, direction, limit := a.OrderTerms()
	if text(o, "account_number") != account || text(o, "direction") != direction || text(o, "type") != "limit" || text(o, "time_in_force") != "gfd" || text(o, "market_hours") != "regular_hours" {
		return errors.New("review account or order type mismatch")
	}
	q, err := integer(o, "quantity")
	if err != nil || q != 1 {
		return errors.New("review quantity mismatch")
	}
	price, err := money(o, "price")
	if err != nil || price != limit {
		return errors.New("review limit price mismatch")
	}
	legs, ok := o["legs"].([]any)
	if !ok || len(legs) != 1 {
		return errors.New("review leg count mismatch")
	}
	leg, ok := legs[0].(object)
	if !ok {
		return errors.New("invalid review leg")
	}
	ratio, err := integer(leg, "ratio_quantity")
	if err != nil || ratio != 1 || text(leg, "option_id") != a.ID || text(leg, "side") != side || text(leg, "position_effect") != effect {
		return errors.New("review contract or leg mismatch")
	}
	return nil
}

func (b *Broker) Review(ctx context.Context, a wheel.Candidate) error {
	_, err := b.ReviewEvidence(ctx, a)
	return err
}

// ReviewEvidence lets the sole account writer durably bind the exact validated
// review before issuing a coordinator ticket. It does not place an order.
func (b *Broker) ReviewEvidence(ctx context.Context, a wheel.Candidate) (map[string]any, error) {
	o, err := b.preview(ctx, a)
	if err != nil {
		return nil, err
	}
	if err = validateReview(o, b.Config, a, true); err != nil {
		return nil, err
	}
	return o, nil
}

// PreviewCheck validates compatibility outside market hours without permitting a
// stale quote to enter execution. Actual execution always requires fresh quotes.
func (b *Broker) PreviewCheck(ctx context.Context, a wheel.Candidate) error {
	o, err := b.preview(ctx, a)
	if err != nil {
		return err
	}
	return validateReview(o, b.Config, a, false)
}

func validateReview(o object, c wheel.Config, a wheel.Candidate, requireFresh bool) error {
	checks, ok := o["order_checks"].(object)
	if !ok {
		return errors.New("broker review checks missing")
	}
	if len(checks) != 0 {
		code := text(checks, "alertType")
		if len(code) == 0 || len(code) > 80 {
			return errors.New("broker review contains an unknown alert")
		}
		for _, r := range code {
			if (r < 'A' || r > 'Z') && r != '_' {
				return errors.New("broker review contains an unknown alert")
			}
		}
		return fmt.Errorf("broker review alert: %s", code)
	}
	fees, ok := o["fees"].(object)
	if !ok {
		return errors.New("broker fees missing")
	}
	fee, err := money(fees, "total_fee")
	if err != nil || fee > c.Fee {
		return errors.New("broker fees unknown or exceed reserved allowance")
	}
	collateral, ok := o["collateral"].(object)
	if !ok || text(collateral, "account_number") != c.Account {
		return errors.New("review collateral account mismatch")
	}
	cashDirection := "debit"
	if a.Closing {
		if a.Kind != "put" || a.CloseLimit <= 0 || a.CloseLimit > wheel.MaxMoney/100 {
			return errors.New("unsupported close review")
		}
		cashDirection = "credit"
	}
	cash, ok := collateral["cash"].(object)
	if !ok || cash["infinite"] != false || text(cash, "direction") != cashDirection {
		return errors.New("unlimited or unexpected collateral change")
	}
	amount, err := money(cash, "amount")
	if err != nil {
		return err
	}
	equities, ok := collateral["equities"].([]any)
	if !ok {
		return errors.New("collateral equities missing")
	}
	if a.Kind == "put" {
		// Plan prohibits existing stock/options/orders in a new put symbol, so
		// this change must equal its complete strike-times-100 requirement.
		if amount != a.Strike*100 || len(equities) != 0 {
			return errors.New("cash-secured put collateral mismatch")
		}
	} else if a.Kind == "call" {
		if amount != 0 || len(equities) != 1 {
			return errors.New("covered call collateral mismatch")
		}
		e, ok := equities[0].(object)
		if !ok {
			return errors.New("invalid share collateral")
		}
		q, qerr := integer(e, "quantity")
		u, uerr := integer(e, "uncovered_shares")
		if qerr != nil || uerr != nil || q != 100 || u != 0 || text(e, "symbol") != a.Symbol || text(e, "direction") != "debit" || text(e, "instrument_id") == "" {
			return errors.New("call is not covered by exactly 100 available shares")
		}
	} else {
		return errors.New("unsupported reviewed strategy")
	}
	quotes, ok := o["option_quotes"].([]any)
	if !ok || len(quotes) != 1 {
		return errors.New("review option quote missing")
	}
	q, ok := quotes[0].(object)
	if !ok || optionID(q) != a.ID {
		return errors.New("review quote contract mismatch")
	}
	at, err := timestamp(q, "updated_at")
	if err != nil {
		return err
	}
	if requireFresh {
		age := time.Since(at)
		bid, err := money(q, "bid_price")
		validPrice := err == nil && bid >= a.Bid
		if a.Closing {
			ask, aerr := money(q, "ask_price")
			size, serr := integer(q, "ask_size")
			validPrice = aerr == nil && serr == nil && ask > 0 && ask <= a.CloseLimit && size > 0
		}
		if !validPrice || age < -2*time.Second || age > time.Duration(c.MaxQuoteAgeSeconds)*time.Second {
			return errors.New("review quote stale or outside limit")
		}
	}
	return nil
}

func (b *Broker) Submit(ctx context.Context, a wheel.Candidate, ref string) (string, error) {
	id, _, err := b.SubmitEvidence(ctx, a, ref)
	return id, err
}

// SubmitEvidence returns the exact financial RPC receipt without performing a
// second read after transport. Its caller persists it before acknowledging the
// strategy; a lost read cannot erase an already observed order identity.
func (b *Broker) SubmitEvidence(ctx context.Context, a wheel.Candidate, ref string) (string, any, error) {
	return b.submitEvidence(ctx, a, ref, time.Time{}, nil)
}

func (b *Broker) submitEvidence(ctx context.Context, a wheel.Candidate, ref string, fixedDeadline time.Time, fixedGuard func() error) (string, any, error) {
	if (b.Config.FixedPolicy != nil) != !fixedDeadline.IsZero() {
		return "", nil, wheel.ErrNotSubmitted
	}
	if b.SubmitMu != nil {
		b.SubmitMu.Lock()
		defer b.SubmitMu.Unlock()
	}
	if b.BeforeSubmit != nil {
		if err := b.BeforeSubmit(); err != nil {
			return "", nil, fmt.Errorf("%w: %v", wheel.ErrNotSubmitted, err)
		}
	}
	if !wheel.TradingWindow(time.Now()) || time.Until(a.At) > 2*time.Second || time.Until(a.UnderlyingAt) > 2*time.Second || time.Since(a.At) > time.Duration(b.Config.MaxQuoteAgeSeconds)*time.Second || time.Since(a.UnderlyingAt) > time.Duration(b.Config.MaxQuoteAgeSeconds)*time.Second || ctx.Err() != nil {
		return "", nil, wheel.ErrNotSubmitted
	}
	args := b.orderArgs(a)
	args["ref_id"] = ref
	if b.SubmitProvider != nil {
		if a.Closing {
			return "", nil, wheel.ErrNotSubmitted
		}
		id, err := b.SubmitProvider(ctx, a, ref)
		return id, nil, err
	}
	if !fixedDeadline.IsZero() && !fixedDeadlineAllows(fixedDeadline, time.Now()) {
		return "", nil, wheel.ErrNotSubmitted
	}
	guard := func() error {
		if b.BeforeSubmit != nil {
			if err := b.BeforeSubmit(); err != nil {
				return err
			}
		}
		if !wheel.TradingWindow(time.Now()) || ctx.Err() != nil || time.Until(a.At) > 2*time.Second || time.Until(a.UnderlyingAt) > 2*time.Second ||
			time.Since(a.At) > time.Duration(b.Config.MaxQuoteAgeSeconds)*time.Second || time.Since(a.UnderlyingAt) > time.Duration(b.Config.MaxQuoteAgeSeconds)*time.Second ||
			!fixedDeadline.IsZero() && !fixedDeadlineAllows(fixedDeadline, time.Now()) {
			return wheel.ErrNotSubmitted
		}
		if fixedGuard != nil {
			return fixedGuard()
		}
		return nil
	}
	var v any
	var err error
	transportDeadline := a.At.Add(time.Duration(b.Config.MaxQuoteAgeSeconds) * time.Second)
	if expires := a.UnderlyingAt.Add(time.Duration(b.Config.MaxQuoteAgeSeconds) * time.Second); expires.Before(transportDeadline) {
		transportDeadline = expires
	}
	transportCtx, cancel := context.WithDeadline(ctx, transportDeadline)
	defer cancel()
	if client, ok := b.MCP.(interface {
		CallGuarded(context.Context, string, map[string]any, func() error) (any, error)
	}); ok {
		v, err = client.CallGuarded(transportCtx, "place_option_order", args, guard)
	} else if !fixedDeadline.IsZero() {
		return "", nil, wheel.ErrNotSubmitted // No unguarded fixed-policy transport.
	} else {
		v, err = b.MCP.Call(transportCtx, "place_option_order", args)
	}
	if err != nil {
		return "", nil, err
	}
	id, err := receipt(v, a)
	return id, v, err
}

// RecoverReceipt binds an operator-supplied ID after a lost response. Contract
// similarity or order-history absence never proves which write was accepted.
func (b *Broker) RecoverReceipt(ctx context.Context, a wheel.Candidate, id string) error {
	args, err := b.scoped("get_option_orders", object{"order_id": id})
	if err != nil {
		return err
	}
	orders, err := b.collection(ctx, "get_option_orders", args, "orders")
	if err != nil {
		return err
	}
	if len(orders) != 1 || text(orders[0], "id") != id {
		return errors.New("recovery ID not uniquely present in configured account")
	}
	got, err := receipt(object{"data": object{"order": orders[0]}}, a)
	if err != nil || got != id {
		return errors.New("recovery order does not match the durable intent")
	}
	return nil
}

// The RPC response is bound to the explicit account-scoped request. The real
// broker receipt does not echo account/ref_id; its ID and exact order fields
// bind it to the durable intent. Missing/ambiguous responses retain that intent.
func receipt(v any, a wheel.Candidate) (string, error) {
	side, effect, direction, limit := a.OrderTerms()
	data, err := single(v)
	if err != nil {
		return "", err
	}
	o, ok := data["order"].(object)
	if !ok {
		return "", errors.New("order receipt missing")
	}
	q, err := integer(o, "quantity")
	if err != nil || q != 1 {
		return "", errors.New("receipt quantity mismatch")
	}
	price, err := money(o, "price")
	if err != nil || price != limit {
		return "", errors.New("receipt price mismatch")
	}
	premium, perr := money(o, "premium")
	mult, err := integer(o, "trade_value_multiplier")
	if perr != nil || premium != limit*100 || err != nil || mult != 100 || text(o, "id") == "" || text(o, "chain_symbol") != a.Symbol || (a.ChainID != "" && text(o, "chain_id") != a.ChainID) || text(o, "direction") != direction || text(o, "type") != "limit" || text(o, "trigger") != "immediate" || text(o, "time_in_force") != "gfd" || text(o, "market_hours") != "regular_hours" || text(o, "state") == "" {
		return "", errors.New("receipt identity or order mismatch")
	}
	created, cerr := timestamp(o, "created_at")
	if cerr != nil || text(o, "placed_agent") != "agentic" || (!a.At.IsZero() && created.Before(a.At)) {
		return "", errors.New("receipt source or creation time mismatch")
	}
	legs, ok := o["legs"].([]any)
	if !ok || len(legs) != 1 {
		return "", errors.New("receipt legs missing")
	}
	leg, ok := legs[0].(object)
	if !ok {
		return "", errors.New("invalid receipt leg")
	}
	strike, err := money(leg, "strike_price")
	ratio, rerr := integer(leg, "ratio_quantity")
	if err != nil || rerr != nil || ratio != 1 || optionID(leg) != a.ID || text(leg, "side") != side || text(leg, "position_effect") != effect || text(leg, "option_type") != a.Kind || strike != a.Strike || text(leg, "expiration_date") != a.Expiry {
		return "", errors.New("receipt contract mismatch")
	}
	return text(o, "id"), nil
}

// VerifyOptionReceipt keeps the same exact receipt checks for coordinator replies.
func VerifyOptionReceipt(v any, a wheel.Candidate) (string, error) { return receipt(v, a) }

func (b *Broker) quoteRows(ctx context.Context, tool string, args object) ([]object, error) {
	rs, err := b.collection(ctx, tool, args, "quotes")
	if err != nil {
		return nil, err
	}
	out := make([]object, 0, len(rs))
	for _, r := range rs {
		q, ok := r["quote"].(object)
		if !ok {
			return nil, errors.New("live quote missing in results")
		}
		out = append(out, q)
	}
	return out, nil
}

// A simulated short call reports the broker's share deliverable as held plus
// uncovered shares. It is never submitted. Its alerts/infinite risk are expected
// when probing an account without shares, and cannot authorize a real order.
func (b *Broker) verifyDeliverable(ctx context.Context, chain object, expiry string, contracts []object, kind string) error {
	b.proofMu.Lock()
	defer b.proofMu.Unlock()
	key := text(chain, "id") + ":" + expiry
	if b.proofs[key] {
		return nil
	}
	calls := contracts
	if kind != "call" {
		var err error
		calls, err = b.collection(ctx, "get_option_instruments", object{"chain_id": text(chain, "id"), "expiration_dates": expiry, "type": "call", "state": "active", "tradability": "tradable"}, "instruments")
		if err != nil {
			return err
		}
	}
	if len(calls) == 0 {
		return errors.New("no call contract for deliverable proof")
	}
	meta := calls[0]
	op, err := instrument(meta, text(meta, "id"))
	if err != nil {
		return err
	}
	if op.Kind != "call" || op.Symbol != text(chain, "symbol") || op.Expiry != expiry || text(meta, "chain_id") != text(chain, "id") || text(meta, "underlying_type") != "equity" {
		return errors.New("deliverable probe contract mismatch")
	}
	o, err := b.preview(ctx, wheel.Candidate{Quote: wheel.Quote{Option: op, Bid: 5}})
	if err != nil {
		return err
	}
	collateral, ok := o["collateral"].(object)
	if !ok || text(collateral, "account_number") != b.Config.Account {
		return errors.New("deliverable proof account missing")
	}
	equities, ok := collateral["equities"].([]any)
	if !ok || len(equities) != 1 {
		return errors.New("deliverable proof lacks one equity")
	}
	e, ok := equities[0].(object)
	if !ok {
		return errors.New("invalid deliverable proof")
	}
	q, err := integer(e, "quantity")
	u, uerr := integer(e, "uncovered_shares")
	if err != nil || uerr != nil || q+u != 100 || text(e, "symbol") != op.Symbol || text(e, "instrument_id") == "" || text(e, "direction") != "debit" {
		return errors.New("broker deliverable is not exactly 100 shares")
	}
	underlyings := chain["underlying_instruments"].([]any)
	underlying := underlyings[0].(object)
	link, err := url.Parse(text(underlying, "instrument"))
	if err != nil || strings.TrimSuffix(link.Path, "/") != "/instruments/"+text(e, "instrument_id") {
		return errors.New("deliverable equity identity mismatch")
	}
	if b.proofs == nil {
		b.proofs = map[string]bool{}
	}
	b.proofs[key] = true
	return nil
}

func decimalQuantity(s string) bool {
	if s == "0E-8" {
		return true
	}
	if s == "" || len(s) > 30 {
		return false
	}
	whole, fraction, _ := strings.Cut(s, ".")
	if whole == "" || len(fraction) > 12 {
		return false
	}
	for _, r := range whole + fraction {
		if r < '0' || r > '9' {
			return false
		}
	}
	n, err := strconv.ParseInt(whole, 10, 64)
	return err == nil && n <= 1_000_000
}

// Stock trades may have sub-cent precision. Ceiling to cents conservatively
// values call intrinsic time value. With cent-denominated strikes and strict
// strike < spot, it also cannot admit an at/above-market put strike.
func ceilingPrice(s string) (wheel.Cents, error) {
	if !decimalQuantity(s) {
		return 0, errors.New("invalid underlying price")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return 0, errors.New("invalid underlying price")
	}
	r.Mul(r, big.NewRat(100, 1))
	n, remainder := new(big.Int), new(big.Int)
	n.QuoRem(r.Num(), r.Denom(), remainder)
	if remainder.Sign() > 0 {
		n.Add(n, big.NewInt(1))
	}
	if !n.IsInt64() || n.Sign() <= 0 || n.Int64() > int64(wheel.MaxMoney) {
		return 0, errors.New("underlying price out of range")
	}
	return wheel.Cents(n.Int64()), nil
}

func positionCapital(quantity, price string) (wheel.Cents, error) {
	if !decimalQuantity(quantity) || !decimalQuantity(price) {
		return 0, errors.New("invalid inventory cost")
	}
	q, qok := new(big.Rat).SetString(quantity)
	p, pok := new(big.Rat).SetString(price)
	if !qok || !pok || q.Sign() <= 0 || p.Sign() <= 0 {
		return 0, errors.New("inventory cost missing")
	}
	q.Mul(q, p)
	q.Mul(q, big.NewRat(100, 1))
	n, remainder := new(big.Int), new(big.Int)
	n.QuoRem(q.Num(), q.Denom(), remainder)
	if remainder.Sign() > 0 {
		n.Add(n, big.NewInt(1))
	}
	if !n.IsInt64() || n.Int64() > int64(wheel.MaxMoney) {
		return 0, errors.New("inventory capital out of range")
	}
	return wheel.Cents(n.Int64()), nil
}

func (b *Broker) collection(ctx context.Context, tool string, args object, key string) ([]object, error) {
	var out []object
	seen := map[string]bool{}
	for range 32 {
		v, err := b.MCP.Call(ctx, tool, args)
		if err != nil {
			return nil, err
		}
		rows, next, err := rows(v, key)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", tool, err)
		}
		out = append(out, rows...)
		if len(out) > 10000 {
			return nil, errors.New("broker collection exceeds row limit")
		}
		if next == "" {
			return out, nil
		}
		if seen[next] || !b.MCP.Supports(tool, "cursor") {
			return nil, errors.New("broker pagination incomplete or repeated")
		}
		seen[next] = true
		args["cursor"] = next
	}
	return nil, errors.New("broker collection exceeds page limit")
}

func rows(v any, key string) ([]object, string, error) {
	next := ""
	for range 4 {
		if a, ok := v.([]any); ok {
			out := make([]object, 0, len(a))
			for _, x := range a {
				o, ok := x.(object)
				if !ok {
					return nil, "", errors.New("collection row is not an object")
				}
				out = append(out, o)
			}
			return out, next, nil
		}
		o, ok := v.(object)
		if !ok {
			return nil, "", errors.New("collection shape unsupported")
		}
		if x, ok := o["next"]; ok && x != nil && x != "" {
			raw, ok := x.(string)
			if !ok {
				return nil, "", errors.New("invalid pagination")
			}
			if strings.HasPrefix(raw, "https://") {
				u, err := url.Parse(raw)
				if err != nil {
					return nil, "", err
				}
				raw = u.Query().Get("cursor")
				if raw == "" {
					return nil, "", errors.New("unrecognized next-page link")
				}
			}
			next = raw
		}
		if x, ok := o["next_cursor"]; ok && x != nil && x != "" {
			var valid bool
			next, valid = x.(string)
			if !valid {
				return nil, "", errors.New("invalid next cursor")
			}
		}
		if x, ok := o[key]; ok {
			v = x
			continue
		}
		if x, ok := o["results"]; ok {
			v = x
			continue
		}
		if x, ok := o["data"]; ok {
			v = x
			continue
		}
		return nil, "", errors.New("required collection absent")
	}
	return nil, "", errors.New("provider nesting too deep")
}

func single(v any) (object, error) {
	o, ok := v.(object)
	if !ok {
		return nil, errors.New("expected provider object")
	}
	if x, ok := o["data"]; ok {
		o, ok = x.(object)
		if !ok {
			return nil, errors.New("expected provider data object")
		}
	}
	return o, nil
}
func text(o object, key string) string {
	switch v := o[key].(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	}
	return ""
}
func money(o object, key string) (wheel.Cents, error) {
	s := text(o, key)
	n, err := wheel.ParseCents(s)
	if err != nil {
		return 0, fmt.Errorf("missing or invalid money field %s", key)
	}
	return n, nil
}
func integer(o object, key string) (int, error) {
	s := text(o, key)
	if s == "0E-8" {
		return 0, nil
	}
	whole, fraction, _ := strings.Cut(s, ".")
	if fraction != "" && strings.Trim(fraction, "0") != "" {
		return 0, errors.New("fractional quantity unsupported")
	}
	n, err := strconv.Atoi(whole)
	if err != nil || n < 0 || n > 1_000_000 {
		return 0, fmt.Errorf("missing or invalid integer field %s", key)
	}
	return n, nil
}
func timestamp(o object, key string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, text(o, key))
	if err != nil {
		return t, fmt.Errorf("missing or invalid timestamp %s", key)
	}
	return t, nil
}
func optionID(o object) string {
	for _, key := range []string{"option_id", "instrument_id", "option", "instrument", "id"} {
		v := text(o, key)
		if v != "" {
			if strings.Contains(v, "://") {
				u, err := url.Parse(v)
				if err != nil {
					return ""
				}
				id, ok := strings.CutPrefix(strings.TrimSuffix(u.Path, "/"), "/options/instruments/")
				if !ok || strings.Contains(id, "/") || id == "" {
					return ""
				}
				v = id
			}
			return v
		}
	}
	return ""
}
