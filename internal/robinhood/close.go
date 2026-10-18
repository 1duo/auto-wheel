package robinhood

import (
	"autowheel/internal/wheel"
	"context"
	"errors"
	"time"
)

// CloseQuote reads the exact owned contract and its actual opening fill, never
// the nearest expiry used for new entries. It cannot place or cancel an order.
func (b *Broker) CloseQuote(ctx context.Context, st wheel.State, symbol string) (wheel.Candidate, error) {
	return b.closeQuote(ctx, st, symbol, true)
}

// PreviewClose permits old source clocks only for read-only compatibility.
func (b *Broker) PreviewClose(ctx context.Context, st wheel.State, symbol string) (wheel.Candidate, error) {
	return b.closeQuote(ctx, st, symbol, false)
}

func (b *Broker) closeQuote(ctx context.Context, st wheel.State, symbol string, requireFresh bool) (wheel.Candidate, error) {
	var a wheel.Candidate
	p := st.Positions[symbol]
	id, kind, expiry, opened := p.PutID, "put", p.PutExpiry, p.PutOpenedAt
	call := p.CallID != "" && p.PutID == "" && p.OwnedShares == 100 && b.Config.FixedPolicy != nil && b.Config.FixedPolicy.Spec.ID == wheel.FixedConservativeTP75Policy
	if call {
		id, kind, expiry, opened = p.CallID, "call", p.CallExpiry, p.CallOpenedAt
	}
	if id == "" || !call && (p.CallID != "" || p.OwnedShares != 0) || st.Account != b.Config.Account || opened.IsZero() {
		return a, errors.New("no exclusively owned short with an opening clock to close")
	}
	rows, err := b.collection(ctx, "get_option_instruments", object{"ids": id}, "instruments")
	if err != nil {
		return a, err
	}
	if len(rows) != 1 || text(rows[0], "id") != id {
		return a, errors.New("close instrument identity missing")
	}
	meta := rows[0]
	op, err := instrument(meta, id)
	if err != nil {
		return a, err
	}
	op.Quantity = 1
	if op.Symbol != symbol || op.Kind != kind || kind == "put" && op.Strike != p.PutStrike || op.Expiry != expiry || text(meta, "underlying_type") != "equity" || text(meta, "state") != "active" || text(meta, "tradability") != "tradable" {
		return a, errors.New("owned close instrument changed or is untradable")
	}
	a = wheel.Candidate{Closing: true, Quote: wheel.Quote{Option: op, Standard: true, Tradable: true}}
	orderID := ""
	var entry wheel.OrderRecord
	for receiptID, r := range st.CompletedOrders {
		if !r.Closing && r.Filled == 1 && r.Symbol == symbol && r.OptionID == id && r.At.Equal(opened) {
			if orderID != "" {
				return a, errors.New("ambiguous opening receipt")
			}
			orderID, entry = receiptID, r
		}
	}
	if orderID == "" {
		return a, errors.New("opening receipt not owned")
	}
	orders, err := b.collection(ctx, "get_option_orders", object{"account_number": b.Config.Account, "order_id": orderID}, "orders")
	if err != nil {
		return a, err
	}
	if len(orders) != 1 || text(orders[0], "id") != orderID || text(orders[0], "state") != "filled" {
		return a, errors.New("opening fill unavailable")
	}
	opening := orders[0]
	if text(opening, "ref_id") != "" && text(opening, "ref_id") != entry.Ref {
		return a, errors.New("opening fill reference differs from owned receipt")
	}
	limit, err := money(opening, "price")
	if err != nil || limit <= 0 || limit > wheel.MaxMoney/100 {
		return a, errors.New("opening limit invalid")
	}
	original := wheel.Candidate{Quote: wheel.Quote{Option: op, At: entry.At, Bid: limit}}
	if _, err = receipt(object{"data": object{"order": opening}}, original); err != nil {
		return a, err
	}
	filled, err := integer(opening, "processed_quantity")
	if err != nil || filled != 1 {
		return a, errors.New("opening quantity not fully filled")
	}
	leg := opening["legs"].([]any)[0].(object) // receipt validated the leg shape.
	executions, ok := leg["executions"].([]any)
	if !ok || len(executions) != 1 {
		return a, errors.New("opening execution ambiguous")
	}
	e, ok := executions[0].(object)
	if !ok {
		return a, errors.New("opening execution missing")
	}
	qty, qerr := integer(e, "quantity")
	price, perr := money(e, "price")
	credit, cerr := money(opening, "processed_premium")
	if qerr != nil || qty != 1 || perr != nil || price < limit || price > wheel.MaxMoney/100 || cerr != nil || credit != price*100 {
		return a, errors.New("opening premium or execution mismatch")
	}
	a.EntryCredit = credit
	a.EntryOrderID = orderID
	stocks, err := b.quoteRows(ctx, "get_equity_quotes", object{"symbols": []string{symbol}})
	if err != nil {
		return a, err
	}
	if len(stocks) != 1 || text(stocks[0], "symbol") != symbol || text(stocks[0], "state") != "active" || stocks[0]["has_traded"] != true {
		return a, errors.New("close underlying quote unavailable")
	}
	a.Spot, err = ceilingPrice(text(stocks[0], "last_trade_price"))
	if err != nil {
		return a, err
	}
	a.UnderlyingAt, err = timestamp(stocks[0], "venue_last_trade_time")
	if err != nil {
		return a, err
	}
	quotes, err := b.quoteRows(ctx, "get_option_quotes", object{"instrument_ids": []string{id}})
	if err != nil {
		return a, err
	}
	if len(quotes) != 1 || optionID(quotes[0]) != id {
		return a, errors.New("close quote identity missing")
	}
	q := quotes[0]
	a.Bid, err = money(q, "bid_price")
	if err != nil {
		return a, err
	}
	a.Ask, err = money(q, "ask_price")
	if err != nil {
		return a, err
	}
	size, err := integer(q, "ask_size")
	if err != nil || size < 1 {
		return a, errors.New("no offered close liquidity")
	}
	a.AskSize = size
	a.At, err = timestamp(q, "updated_at")
	if err != nil {
		return a, err
	}
	ticks, ok := meta["min_ticks"].(object)
	if !ok {
		return a, errors.New("close tick schedule missing")
	}
	cutoff, err := money(ticks, "cutoff_price")
	if err != nil || cutoff <= 0 {
		return a, errors.New("close tick cutoff invalid")
	}
	key := "below_tick"
	if a.Ask >= cutoff {
		key = "above_tick"
	}
	a.Tick, err = money(ticks, key)
	if err != nil || a.Tick < 1 || a.Ask <= 0 || a.Ask > wheel.MaxMoney/100 || a.Ask%a.Tick != 0 {
		return a, errors.New("close ask is not a valid limit")
	}
	a.CloseLimit = a.Ask
	if time.Until(a.At) > 2*time.Second || time.Until(a.UnderlyingAt) > 2*time.Second || requireFresh && (time.Since(a.At) > time.Duration(b.Config.MaxQuoteAgeSeconds)*time.Second || time.Since(a.UnderlyingAt) > time.Duration(b.Config.MaxQuoteAgeSeconds)*time.Second) {
		return a, errors.New("close quote stale")
	}
	return a, nil
}
