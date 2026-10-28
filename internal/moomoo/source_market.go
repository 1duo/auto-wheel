package moomoo

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"time"

	"autowheel/internal/contractpolicy"
	"autowheel/internal/wheel"
)

// optionTick is a US equity option minimum price variation that is valid at
// price for every class consistent with the observed NBBO prices. Classes use
// one of three regimes: 1¢ everywhere, 1¢ below $3 and 5¢ at/above, or 5¢ below
// $3 and 10¢ at/above. A displayed quote off the coarser grid proves a finer
// regime; without such proof the coarsest regime is used.
func optionTick(price wheel.Cents, observed ...wheel.Cents) wheel.Cents {
	allPenny, finer := false, false
	for _, q := range observed {
		switch {
		case q <= 0:
		case q%5 != 0 && q >= 300:
			allPenny = true
		case q%5 != 0, q >= 300 && q%10 != 0:
			finer = true
		}
	}
	switch {
	case allPenny:
		return 1
	case price < 300 && finer:
		return 1
	case price < 300:
		return 5
	case finer:
		return 5
	}
	return 10
}

// Reviewed Moomoo US one-contract option fees, from this account's actual
// order-fee records: $1.99 commission per order, $0.30 platform and $0.65
// contract fees per contract, with ceilings for ORF, OCC and CAT. Sales add
// TAF and the SEC fee on premium. Every component rounds up.
const (
	optionOrderFeeCents = 199 + 30 + 65 + 3 + 3 + 1
	secRatePerHundredM  = 2060 // $20.60 per million of sale proceeds.
)

func secFee(notional wheel.Cents) wheel.Cents {
	return (notional*secRatePerHundredM + 99_999_999) / 100_000_000
}

func optionFeeBound(sell bool, limit wheel.Cents) wheel.Cents {
	if !sell {
		return optionOrderFeeCents
	}
	// TAF, SEC on the limit, and one cent for SEC on any price improvement.
	return optionOrderFeeCents + 1 + secFee(limit*100) + 1
}

// Assignment and expiration legs carry no option fees; a call-away sale pays
// SEC plus TAF/CAT ceilings, matching the engine's physical delivery minimum.
func physicalFeeBound(strike wheel.Cents) wheel.Cents { return secFee(strike*100) + 3 }

// contractBooks reads exact deliverable metadata and one consistent snapshot
// packet for the underlying and the contract.
func (s *Source) contractBooks(ctx context.Context, contract contractpolicy.Contract) (wheel.FixedSignalEvidence, error) {
	out, _, err := s.contractMarket(ctx, contract)
	return out, err
}

func (s *Source) contractMarket(ctx context.Context, contract contractpolicy.Contract) (wheel.FixedSignalEvidence, MarketQuoteFacts, error) {
	var out wheel.FixedSignalEvidence
	code, err := parseOptionCode(contract.ID)
	if err != nil || code.Root != contract.Symbol || code.Kind != contract.Kind || code.Expiry != contract.Expiry || int64(code.Strike) != contract.Strike {
		return out, MarketQuoteFacts{}, errors.New("Moomoo contract identity contradicts its provider code")
	}
	underlying := "US." + contract.Symbol
	basic, err := s.Client.readQuoteBatch(ctx, "/api/v1.0/quote/stock-basicinfo", []string{contract.ID})
	if err != nil {
		return out, MarketQuoteFacts{}, err
	}
	facts, err := instrumentFacts(basic.Rows[contract.ID])
	if err != nil || facts.Code != contract.ID || facts.Type != "DRVT" || facts.Owner != underlying || facts.LotSize != 100 ||
		facts.ContractSize == nil || *facts.ContractSize != 100 || facts.State != "NORMAL" || facts.Suspended {
		return out, MarketQuoteFacts{}, errors.New("Moomoo contract is not a tradable standard 100-share deliverable")
	}
	packet, err := s.Client.readQuoteBatch(ctx, "/api/v1.0/quote/snapshot", []string{underlying, contract.ID})
	if err != nil {
		return out, MarketQuoteFacts{}, err
	}
	now := s.now()
	stock, err := marketQuoteFacts(packet.Rows[underlying], now)
	if err != nil {
		return out, MarketQuoteFacts{}, err
	}
	option, err := marketQuoteFacts(packet.Rows[contract.ID], now)
	if err != nil {
		return out, MarketQuoteFacts{}, err
	}
	if stock.Code != underlying || stock.Status != "NORMAL" || stock.Suspended || option.Code != contract.ID || option.Status != "NORMAL" || option.Suspended || !option.Option ||
		option.BidSize < 0 || option.BidSize > 1_000_000 {
		return out, MarketQuoteFacts{}, errors.New("Moomoo underlying or contract book is not in normal trading")
	}
	// The snapshot clock is the latest update of the copied book; the engine
	// independently requires it to be fresh before any order.
	out = wheel.FixedSignalEvidence{Contract: contract, Stock: contractpolicy.Book{Bid: int64(stock.Bid), Ask: int64(stock.Ask)},
		Option: contractpolicy.Book{Bid: int64(option.Bid), Ask: int64(option.Ask)}, BidAt: stock.QuoteAt, AskAt: stock.QuoteAt, OptionAt: option.QuoteAt,
		BidSize: int(option.BidSize), Tick: optionTick(option.Bid, option.Bid, option.Ask)}
	return out, option, nil
}

// Observe builds a complete conservative menu from the documented unpaginated
// chain and selects identity before contract liquidity is considered.
func (s *Source) Observe(ctx context.Context, kind, symbol string, clock func() time.Time) (wheel.FixedSignalEvidence, error) {
	if clock == nil || !validChainSymbol(symbol) || kind != "put" && kind != "call" {
		return wheel.FixedSignalEvidence{}, errors.New("Moomoo observation needs an explicit symbol, side and clock")
	}
	day := clock().In(wheel.Eastern).Format(time.DateOnly)
	session, _ := time.Parse(time.DateOnly, day)
	audit, err := s.Client.collectOptionChain(ctx, s.Config.AccountID, symbol, session)
	if err != nil {
		return wheel.FixedSignalEvidence{}, err
	}
	all, err := audit.SelectionMenu()
	if err != nil {
		return wheel.FixedSignalEvidence{}, err
	}
	var menu []contractpolicy.Contract
	for _, c := range all {
		if c.Kind == kind {
			menu = append(menu, c)
		}
	}
	packet, err := s.Client.readQuoteBatch(ctx, "/api/v1.0/quote/snapshot", []string{"US." + symbol})
	if err != nil {
		return wheel.FixedSignalEvidence{}, err
	}
	stock, err := marketQuoteFacts(packet.Rows["US."+symbol], s.now())
	if err != nil || stock.Status != "NORMAL" || stock.Suspended {
		return wheel.FixedSignalEvidence{}, errors.New("Moomoo underlying book is unavailable")
	}
	choice, err := contractpolicy.Choose("conservative", kind, day, symbol, contractpolicy.Book{Bid: int64(stock.Bid), Ask: int64(stock.Ask)}, menu)
	if err != nil || choice == nil {
		return wheel.FixedSignalEvidence{}, errors.Join(errors.New("no conservative Moomoo contract"), err)
	}
	out, err := s.contractBooks(ctx, *choice)
	if err != nil {
		return wheel.FixedSignalEvidence{}, err
	}
	if clock().In(wheel.Eastern).Format(time.DateOnly) != day {
		return wheel.FixedSignalEvidence{}, errors.New("session changed during Moomoo observation")
	}
	out.Menu = slices.Clone(menu)
	return out, nil
}

func (s *Source) Refresh(ctx context.Context, chosen contractpolicy.Contract, clock func() time.Time) (wheel.FixedExecutionEvidence, error) {
	market, err := s.contractBooks(ctx, chosen)
	if err != nil {
		return wheel.FixedExecutionEvidence{}, err
	}
	return wheel.FixedExecutionEvidence{Market: market, ChainID: "US." + chosen.Symbol, Deliverable100: true}, nil
}

type tradingInfo struct {
	MaxSellShort int64
	MaxBuyBack   int64
}

// tradingInfo reads the account-bound maximum quantities for one exact limit
// order. It is a GET query and never reserves or places anything.
func (c *Client) tradingInfo(ctx context.Context, account, code string, limit wheel.Cents) (tradingInfo, error) {
	raw, err := c.readGET(ctx, "/api/v1.0/accounts/"+account+"/acctradinginfo", url.Values{"code": {code}, "price": {limit.Dollars()}, "order_type": {"LIMIT"}})
	if err != nil {
		return tradingInfo{}, err
	}
	var fields map[string]json.RawMessage
	if exactJSON(raw, &fields) != nil || rejectQuoteAliases(fields, "max_sell_short", "max_buy_back") != nil {
		return tradingInfo{}, errors.New("Moomoo trading capacity is missing or ambiguous")
	}
	read := func(key string) (int64, error) {
		var text string
		if exactJSON(fields[key], &text) != nil {
			return 0, errors.New("Moomoo trading capacity is not a decimal string")
		}
		micros, err := quantityMicros(text)
		if err != nil || micros < 0 {
			return 0, errors.New("Moomoo trading capacity is invalid")
		}
		return micros / 1_000_000, nil
	}
	var info tradingInfo
	if info.MaxSellShort, err = read("max_sell_short"); err != nil {
		return tradingInfo{}, err
	}
	if info.MaxBuyBack, err = read("max_buy_back"); err != nil {
		return tradingInfo{}, err
	}
	return info, nil
}

// Review proves the exact account accepts this one-contract limit order now:
// broker capacity for the side, a standard deliverable and reviewed fee bounds.
func (s *Source) Review(ctx context.Context, candidate wheel.Candidate) (ExecutionReview, error) {
	code, err := parseOptionCode(candidate.ID)
	if err != nil || code.option(candidate.ID, 1) != candidate.Option || candidate.Quantity != 1 {
		return ExecutionReview{}, errors.New("Moomoo review candidate identity is inconsistent")
	}
	side, limit := "SELL_SHORT", candidate.Bid
	if candidate.Closing {
		side, limit = "BUY_BACK", candidate.CloseLimit
	}
	if limit <= 0 || limit > wheel.MaxMoney/100 {
		return ExecutionReview{}, errors.New("Moomoo review limit is invalid")
	}
	if _, err := s.contractBooks(ctx, contractpolicy.Contract{ID: candidate.ID, Symbol: candidate.Symbol, Expiry: candidate.Expiry, Kind: candidate.Kind, Strike: int64(candidate.Strike)}); err != nil {
		return ExecutionReview{}, err
	}
	info, err := s.Client.tradingInfo(ctx, s.Config.AccountID, candidate.ID, limit)
	if err != nil {
		return ExecutionReview{}, err
	}
	if !candidate.Closing && info.MaxSellShort < 1 || candidate.Closing && info.MaxBuyBack < 1 {
		return ExecutionReview{}, errors.New("Moomoo account capacity does not permit this option order")
	}
	return ExecutionReview{At: s.now(), AccountID: s.Config.AccountID, CandidateSHA256: candidateDigest(candidate), Side: side,
		FeeCents: optionFeeBound(!candidate.Closing, limit), PhysicalFeeCents: physicalFeeBound(candidate.Strike), Deliverable100: true, OptionsAuthorized: true}, nil
}

// openingFill re-reads this journal's own opening receipt by order ID. It is
// the only source of entry credit for a take-profit target.
func (s *Source) openingFill(ctx context.Context, r wheel.OrderRecord, orderID string) (wheel.Cents, error) {
	key := orderID + "/" + r.Ref
	s.mu.Lock()
	credit, known := s.fills[key]
	s.mu.Unlock()
	if known {
		return credit, nil // A FILLED_ALL order's fill never changes.
	}
	from := r.At.Add(-time.Minute)
	rows, err := s.Client.readOrderRows(ctx, s.Config.AccountID, "orders_history", url.Values{"trd_market": {"US"}, "page_size": {"100"}, "code": {r.OptionID},
		"start": {strconv.FormatInt(from.UnixMicro(), 10)}, "end": {strconv.FormatInt(s.now().UnixMicro(), 10)}})
	if err != nil {
		return 0, err
	}
	recent, err := s.Client.readOrderRows(ctx, s.Config.AccountID, "orders", url.Values{"trd_market": {"US"}, "page_size": {"100"}})
	if err != nil {
		return 0, err
	}
	var match *orderRow
	for _, row := range append(rows, recent...) {
		if row.ID != orderID {
			continue
		}
		if match != nil && (match.Status != row.Status || !sameRemark(match.Average, row.Average) || !sameRemark(match.Dealt, row.Dealt)) {
			return 0, errors.New("Moomoo opening receipt facts conflict")
		}
		match = &row
	}
	if match == nil || match.Code != r.OptionID || match.remark() != r.Ref || match.Side != "SELL_SHORT" || match.Status != "FILLED_ALL" ||
		match.Dealt == nil || match.Average == nil {
		return 0, errors.New("Moomoo opening fill is unavailable or not owned")
	}
	dealt, err := receiptContracts(*match.Dealt)
	price, priceErr := wheel.ParseCents(match.Price)
	average, avgErr := wheel.ParseCents(*match.Average)
	if err != nil || dealt != 1 || priceErr != nil || avgErr != nil || average < price || average <= 0 || average > wheel.MaxMoney/100 {
		return 0, errors.New("Moomoo opening fill quantity or price is unproven")
	}
	s.mu.Lock()
	if s.fills == nil {
		s.fills = map[string]wheel.Cents{}
	}
	s.fills[key] = average * 100
	s.mu.Unlock()
	return average * 100, nil
}

// CloseQuote prices the resting TP75 target for an exact owned short from its
// actual broker fill credit and the reviewed fee allowance.
func (s *Source) CloseQuote(ctx context.Context, state wheel.State, symbol string) (wheel.Candidate, error) {
	spec := s.Config.Strategy.FixedPolicy.Spec
	p := state.Positions[symbol]
	id, opened := p.PutID, p.PutOpenedAt
	if p.CallID != "" && p.PutID == "" && p.OwnedShares == 100 {
		id, opened = p.CallID, p.CallOpenedAt
	} else if p.PutID == "" || p.CallID != "" || p.OwnedShares != 0 {
		return wheel.Candidate{}, errors.New("no exclusively owned Moomoo short to close")
	}
	code, err := parseOptionCode(id)
	if err != nil || code.Root != symbol || opened.IsZero() {
		return wheel.Candidate{}, errors.New("owned Moomoo contract identity is invalid")
	}
	orderID := ""
	var entry wheel.OrderRecord
	for receipt, r := range state.CompletedOrders {
		if !r.Closing && r.Filled == 1 && r.Symbol == symbol && r.OptionID == id && r.At.Equal(opened) {
			if orderID != "" {
				return wheel.Candidate{}, errors.New("ambiguous Moomoo opening receipt")
			}
			orderID, entry = receipt, r
		}
	}
	if orderID == "" {
		return wheel.Candidate{}, errors.New("Moomoo opening receipt is not owned")
	}
	credit, err := s.openingFill(ctx, entry, orderID)
	if err != nil {
		return wheel.Candidate{}, err
	}
	contract := contractpolicy.Contract{ID: id, Symbol: symbol, Expiry: code.Expiry, Kind: code.Kind, Strike: int64(code.Strike)}
	market, book, err := s.contractMarket(ctx, contract)
	if err != nil {
		return wheel.Candidate{}, err
	}
	if market.Option.Ask <= 0 || book.AskSize < 1 || book.AskSize > 1_000_000 {
		return wheel.Candidate{}, errors.New("no offered Moomoo close liquidity")
	}
	a := wheel.Candidate{Closing: true, RestingTakeProfit: spec.HasRestingTP75(), EntryCredit: credit, EntryOrderID: orderID,
		Quote: wheel.Quote{Option: code.option(id, 1), At: market.OptionAt, UnderlyingAt: market.BidAt,
			Spot: wheel.Cents((market.Stock.Bid + market.Stock.Ask + 1) / 2), Bid: wheel.Cents(market.Option.Bid), Ask: wheel.Cents(market.Option.Ask),
			AskSize: int(book.AskSize), Standard: true, Tradable: true}}
	// Only displayed quotes prove a tick regime; auction fills can be finer.
	observed := []wheel.Cents{a.Bid, a.Ask}
	ceiling, err := wheel.FixedTP75Limit(credit, spec.OptionFeeBudget, 1)
	if err != nil {
		return wheel.Candidate{}, err
	}
	a.Tick = optionTick(ceiling, observed...)
	if a.CloseLimit, err = wheel.FixedTP75Limit(credit, spec.OptionFeeBudget, a.Tick); err != nil {
		return wheel.Candidate{}, err
	}
	if lower := optionTick(a.CloseLimit, observed...); a.CloseLimit < 300 && lower < a.Tick {
		// Rounding crossed below $3; a finer tick is valid only below that tier.
		limit, err := wheel.FixedTP75Limit(credit, spec.OptionFeeBudget, lower)
		if err != nil {
			return wheel.Candidate{}, err
		}
		a.Tick, a.CloseLimit = lower, min(limit, 299/lower*lower)
	}
	if !a.RestingTakeProfit {
		return wheel.Candidate{}, errors.New("Moomoo close requires the resting TP75 policy")
	}
	return a, nil
}
