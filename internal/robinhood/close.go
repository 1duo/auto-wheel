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
	var a wheel.Candidate
	p := st.Positions[symbol]
	if p.PutID == "" || p.CallID != "" || p.OwnedShares != 0 || st.Account != b.Config.Account {
		return a, errors.New("no exclusively owned put to close")
	}
	rows, err := b.collection(ctx, "get_option_instruments", object{"ids": p.PutID}, "instruments")
	if err != nil {
		return a, err
	}
	if len(rows) != 1 || text(rows[0], "id") != p.PutID {
		return a, errors.New("close instrument identity missing")
	}
	meta := rows[0]
	op, err := instrument(meta, p.PutID)
	if err != nil {
		return a, err
	}
	op.Quantity = 1
	if op.Symbol != symbol || op.Kind != "put" || op.Strike != p.PutStrike || op.Expiry != p.PutExpiry || text(meta, "underlying_type") != "equity" || text(meta, "state") != "active" || text(meta, "tradability") != "tradable" {
		return a, errors.New("owned close instrument changed or is untradable")
	}
	a = wheel.Candidate{Closing: true, Quote: wheel.Quote{Option: op, Standard: true, Tradable: true}}
	orderID := ""
	var entry wheel.OrderRecord
	for id, r := range st.CompletedOrders {
		if !r.Closing && r.Symbol == symbol && r.OptionID == p.PutID && r.At.Equal(p.PutOpenedAt) {
			if orderID != "" {
				return a, errors.New("ambiguous opening receipt")
			}
			orderID, entry = id, r
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
	quotes, err := b.quoteRows(ctx, "get_option_quotes", object{"instrument_ids": []string{p.PutID}})
	if err != nil {
		return a, err
	}
	if len(quotes) != 1 || optionID(quotes[0]) != p.PutID {
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
	if time.Since(a.At) > time.Duration(b.Config.MaxQuoteAgeSeconds)*time.Second {
		return a, errors.New("close quote stale")
	}
	return a, nil
}
