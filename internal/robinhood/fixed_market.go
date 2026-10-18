package robinhood

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"autowheel/internal/contractpolicy"
	"autowheel/internal/wheel"
)

// FixedMarketObservation is source evidence, not an executable Candidate.
// Account funding, exact deliverable review and ownership remain separate.
type FixedMarketObservation struct {
	At                time.Time                `json:"observed_at"`
	Day               string                   `json:"session_date"`
	Mode              string                   `json:"policy_mode"`
	Kind              string                   `json:"option_kind"`
	Symbol            string                   `json:"symbol"`
	Stock             contractpolicy.Book      `json:"underlying_book"`
	BidAt             time.Time                `json:"underlying_bid_at"`
	AskAt             time.Time                `json:"underlying_ask_at"`
	MenuContracts     int                      `json:"menu_contracts"`
	Choice            *contractpolicy.Contract `json:"choice,omitzero"`
	Option            contractpolicy.Book      `json:"option_book"`
	OptionAt          time.Time                `json:"option_at,omitzero"`
	BidSize           int                      `json:"bid_size"`
	Tick              wheel.Cents              `json:"tick_cents"`
	MarketFresh       bool                     `json:"market_fresh"`
	EntryBookUsable   bool                     `json:"entry_book_usable"`
	Structural100     bool                     `json:"structural_100_share_chain"`
	DeliverableReview bool                     `json:"account_deliverable_reviewed"`
	TradingAuthorized bool                     `json:"trading_authorized"`
}

// ObserveFixedPolicy performs only get_* reads. Missing books are not replaced,
// Greek/OI values are not needed or invented, and no broker review is performed.
func (b *Broker) ObserveFixedPolicy(ctx context.Context, mode, kind, symbol string, clock func() time.Time) (FixedMarketObservation, error) {
	var out FixedMarketObservation
	if clock == nil {
		return out, errors.New("fixed policy needs an observation clock")
	}
	day := clock().In(wheel.Eastern).Format(time.DateOnly)
	// Validate the policy and symbol before making a provider request.
	if _, err := contractpolicy.Choose(mode, kind, day, symbol, contractpolicy.Book{Bid: 100, Ask: 100}, nil); err != nil {
		return out, err
	}
	chains, err := b.collection(ctx, "get_option_chains", object{"underlying_symbol": symbol}, "chains")
	if err != nil {
		return out, err
	}
	if len(chains) != 1 || text(chains[0], "symbol") != symbol || text(chains[0], "id") == "" {
		return out, errors.New("fixed-policy chain identity ambiguous")
	}
	chain := chains[0]
	underlyings, ok := chain["underlying_instruments"].([]any)
	if !ok || len(underlyings) != 1 || chain["can_open_position"] != true {
		return out, errors.New("fixed-policy equity chain unavailable")
	}
	underlying, ok := underlyings[0].(object)
	if !ok || text(underlying, "instrument") == "" || text(underlying, "symbol") != "" && text(underlying, "symbol") != symbol {
		return out, errors.New("fixed-policy underlying identity missing")
	}
	mult, err := integer(chain, "trade_value_multiplier")
	if err != nil || mult != 100 {
		return out, errors.New("fixed-policy multiplier is not standard")
	}
	if cash, exists := chain["cash_component"]; !exists || cash != nil && text(chain, "cash_component") == "" {
		return out, errors.New("fixed-policy cash deliverable unknown")
	} else if cash != nil {
		v, err := money(chain, "cash_component")
		if err != nil || v != 0 {
			return out, errors.New("fixed-policy adjusted cash deliverable unsupported")
		}
	}
	stocks, err := b.quoteRows(ctx, "get_equity_quotes", object{"symbols": []string{symbol}})
	if err != nil {
		return out, err
	}
	if len(stocks) != 1 || text(stocks[0], "symbol") != symbol || stocks[0]["has_traded"] != true || text(stocks[0], "state") != "active" {
		return out, errors.New("fixed-policy underlying book unavailable")
	}
	bid, err := money(stocks[0], "bid_price")
	if err != nil {
		return out, err
	}
	ask, err := money(stocks[0], "ask_price")
	if err != nil {
		return out, err
	}
	out = FixedMarketObservation{Day: day, Mode: mode, Kind: kind, Symbol: symbol, Stock: contractpolicy.Book{Bid: int64(bid), Ask: int64(ask)}, Structural100: true}
	if out.BidAt, err = timestamp(stocks[0], "venue_bid_time"); err != nil {
		return out, err
	}
	if out.AskAt, err = timestamp(stocks[0], "venue_ask_time"); err != nil {
		return out, err
	}
	if _, err = contractpolicy.Choose(mode, kind, day, symbol, out.Stock, nil); err != nil {
		return out, err
	}
	dates, ok := chain["expiration_dates"].([]any)
	if !ok {
		return out, errors.New("fixed-policy expiration list unavailable")
	}
	start, _ := time.Parse(time.DateOnly, day)
	var wanted []string
	seenDates := make(map[string]bool, len(dates))
	for _, value := range dates {
		date, ok := value.(string)
		expiry, err := time.Parse(time.DateOnly, date)
		if !ok || err != nil || expiry.Format(time.DateOnly) != date || seenDates[date] {
			return out, errors.New("fixed-policy expiration list ambiguous")
		}
		seenDates[date] = true
		days := int(expiry.Sub(start) / (24 * time.Hour))
		if days >= 21 && days <= 45 {
			wanted = append(wanted, date)
		}
	}
	if len(wanted) == 0 {
		out.At = clock()
		return out, nil
	}
	slices.Sort(wanted)
	meta, err := b.collection(ctx, "get_option_instruments", object{"chain_id": text(chain, "id"), "expiration_dates": strings.Join(wanted, ","), "type": kind, "state": "active"}, "instruments")
	if err != nil {
		return out, err
	}
	if len(meta) == 0 {
		return out, errors.New("listed fixed-policy expirations have no instrument data")
	}
	byID := make(map[string]object, len(meta))
	menu := make([]contractpolicy.Contract, 0, len(meta))
	for _, raw := range meta {
		op, err := instrument(raw, text(raw, "id"))
		if err != nil || op.ID == "" || byID[op.ID] != nil || op.Symbol != symbol || op.Kind != kind ||
			!slices.Contains(wanted, op.Expiry) || text(raw, "chain_id") != text(chain, "id") || text(raw, "underlying_type") != "equity" {
			return out, errors.New("fixed-policy instrument menu is incomplete or ambiguous")
		}
		byID[op.ID] = raw
		menu = append(menu, contractpolicy.Contract{ID: op.ID, Symbol: op.Symbol, Kind: op.Kind, Expiry: op.Expiry, Strike: int64(op.Strike)})
	}
	out.MenuContracts = len(menu)
	out.Choice, err = contractpolicy.Choose(mode, kind, day, symbol, out.Stock, menu)
	if err != nil || out.Choice == nil {
		out.At = clock()
		return out, err
	}
	quotes, err := b.quoteRows(ctx, "get_option_quotes", object{"instrument_ids": []string{out.Choice.ID}})
	if err != nil {
		return out, err
	}
	if len(quotes) != 1 || optionID(quotes[0]) != out.Choice.ID {
		return out, errors.New("chosen fixed-policy quote missing or ambiguous")
	}
	q := quotes[0]
	for _, field := range []string{"instrument_id", "option_id"} {
		if id := text(q, field); id != "" && id != out.Choice.ID {
			return out, errors.New("chosen fixed-policy quote identity conflicts")
		}
	}
	if out.OptionAt, err = timestamp(q, "updated_at"); err != nil {
		return out, err
	}
	bid, err = money(q, "bid_price")
	if err != nil {
		return out, err
	}
	ask, err = money(q, "ask_price")
	if err != nil || ask < bid || ask > wheel.MaxMoney/100 {
		return out, errors.New("chosen fixed-policy book malformed")
	}
	out.Option = contractpolicy.Book{Bid: int64(bid), Ask: int64(ask)}
	if out.BidSize, err = integer(q, "bid_size"); err != nil {
		return out, err
	}
	chosen := byID[out.Choice.ID]
	ticks, ok := chosen["min_ticks"].(object)
	if !ok {
		return out, errors.New("chosen fixed-policy tick unknown")
	}
	cutoff, err := money(ticks, "cutoff_price")
	if err != nil {
		return out, err
	}
	tick := "below_tick"
	if bid >= cutoff {
		tick = "above_tick"
	}
	out.Tick, err = money(ticks, tick)
	if err != nil || out.Tick < 1 {
		return out, errors.New("chosen fixed-policy tick invalid")
	}
	out.At = clock()
	if out.At.In(wheel.Eastern).Format(time.DateOnly) != day {
		return out, errors.New("session changed during fixed-policy observation")
	}
	fresh := func(at time.Time) bool {
		age := out.At.Sub(at)
		return b.Config.MaxQuoteAgeSeconds >= 1 && b.Config.MaxQuoteAgeSeconds <= 60 &&
			age >= -2*time.Second && age <= time.Duration(b.Config.MaxQuoteAgeSeconds)*time.Second
	}
	out.MarketFresh = fresh(out.BidAt) && fresh(out.AskAt) && fresh(out.OptionAt)
	out.EntryBookUsable = bid > 0 && ask > 0 && out.BidSize >= 1 &&
		int64(ask-bid)*20000 <= 1500*int64(ask+bid) && text(chosen, "tradability") == "tradable" && text(chosen, "state") == "active"
	return out, nil
}
