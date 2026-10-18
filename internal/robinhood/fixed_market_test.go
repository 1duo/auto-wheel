package robinhood

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"autowheel/internal/wheel"
)

type fixedMarketClient struct {
	stock, chain, quote object
	menu                []any
	calls               []string
}

func (*fixedMarketClient) Supports(_, key string) bool { return key == "cursor" }

func (c *fixedMarketClient) Call(_ context.Context, name string, args object) (any, error) {
	c.calls = append(c.calls, name)
	switch name {
	case "get_option_chains":
		return object{"data": object{"chains": []any{c.chain}}}, nil
	case "get_equity_quotes":
		return object{"data": object{"results": []any{object{"quote": c.stock}}}}, nil
	case "get_option_instruments":
		if args["expiration_dates"] != "2026-10-30,2026-11-06" || args["type"] != "put" || args["tradability"] != nil {
			return nil, fmt.Errorf("incorrect full identity menu request: %v", args)
		}
		return object{"data": object{"instruments": c.menu}}, nil
	case "get_option_quotes":
		ids, ok := args["instrument_ids"].([]string)
		if !ok || !slices.Equal(ids, []string{"put-90"}) {
			return nil, fmt.Errorf("quote selection used liquidity or wrong contract: %v", args)
		}
		return object{"data": object{"results": []any{object{"quote": c.quote}}}}, nil
	}
	return nil, fmt.Errorf("unexpected non-market request: %s", name)
}

func TestFixedMarketObservation(t *testing.T) {
	at := time.Date(2026, 10, 2, 16, 0, 0, 0, time.UTC)
	for _, scenario := range []string{"fresh", "stale_ask", "zero_chosen_bid", "untradable_chosen", "missing_bid_clock", "conflicting_id", "duplicate_menu", "duplicate_expiry", "missing_cash_deliverable"} {
		t.Run(scenario, func(t *testing.T) {
			stamp := at.Format(time.RFC3339Nano)
			client := &fixedMarketClient{
				stock: object{"symbol": "AAPL", "has_traded": true, "state": "active", "bid_price": "99.99", "ask_price": "100.01", "venue_bid_time": stamp, "venue_ask_time": stamp, "venue_last_trade_time": stamp},
				chain: object{"id": "chain", "symbol": "AAPL", "can_open_position": true, "trade_value_multiplier": "100", "cash_component": nil, "underlying_instruments": []any{object{"instrument": "equity", "symbol": "AAPL"}}, "expiration_dates": []any{"2026-10-09", "2026-11-06", "2026-10-30"}},
				quote: object{"instrument_id": "put-90", "bid_price": "1.00", "ask_price": "1.10", "bid_size": json.Number("1"), "updated_at": stamp},
			}
			for _, row := range []struct{ id, strike, expiry string }{{"put-90", "90.00", "2026-10-30"}, {"put-85", "85.00", "2026-10-30"}, {"later", "90.00", "2026-11-06"}} {
				client.menu = append(client.menu, object{"id": row.id, "chain_id": "chain", "chain_symbol": "AAPL", "type": "put", "expiration_date": row.expiry, "strike_price": row.strike, "trade_value_multiplier": "100", "underlying_type": "equity", "tradability": "tradable", "state": "active", "min_ticks": object{"cutoff_price": "3.00", "below_tick": "0.01", "above_tick": "0.05"}})
			}
			wantErr := false
			switch scenario {
			case "stale_ask":
				client.stock["venue_ask_time"] = at.Add(-31 * time.Second).Format(time.RFC3339Nano)
			case "zero_chosen_bid":
				client.quote["bid_price"], client.quote["bid_size"] = "0.00", json.Number("0")
			case "untradable_chosen":
				client.menu[0].(object)["tradability"] = "untradable"
			case "missing_bid_clock":
				delete(client.stock, "venue_bid_time")
				wantErr = true
			case "conflicting_id":
				client.quote["option_id"] = "other"
				wantErr = true
			case "duplicate_menu":
				client.menu = append(client.menu, client.menu[0])
				wantErr = true
			case "duplicate_expiry":
				client.chain["expiration_dates"] = []any{"2026-10-09", "2026-10-09", "2026-10-30"}
				wantErr = true
			case "missing_cash_deliverable":
				delete(client.chain, "cash_component")
				wantErr = true
			}
			b := &Broker{MCP: client, Config: wheel.DefaultConfig()}
			got, err := b.ObserveFixedPolicy(t.Context(), "conservative", "put", "AAPL", func() time.Time { return at })
			if (err != nil) != wantErr {
				t.Fatalf("observation error: %v, wanted error %v", err, wantErr)
			}
			if got.TradingAuthorized || got.DeliverableReview {
				t.Fatal("market observation granted account/order authority")
			}
			if wantErr {
				return
			}
			if got.Choice == nil || got.Choice.ID != "put-90" || got.MenuContracts != 3 || !got.Structural100 || got.Kind != "put" {
				t.Fatalf("wrong identity or evidence: %+v", got)
			}
			if got.MarketFresh != (scenario != "stale_ask") || got.EntryBookUsable != (scenario != "zero_chosen_bid" && scenario != "untradable_chosen") {
				t.Fatalf("bad freshness/liquidity decision: %+v", got)
			}
			if !slices.Equal(client.calls, []string{"get_option_chains", "get_equity_quotes", "get_option_instruments", "get_option_quotes"}) {
				t.Fatalf("unexpected request sequence: %v", client.calls)
			}
		})
	}
}
