package robinhood

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"autowheel/internal/contractpolicy"
	"autowheel/internal/wheel"
)

type fixedMarketClient struct {
	stock, chain, quote object
	menu                []any
	calls               []string
}

func (*fixedMarketClient) Supports(_, key string) bool { return key == "cursor" || key == "ids" }

func (c *fixedMarketClient) Call(_ context.Context, name string, args object) (any, error) {
	c.calls = append(c.calls, name)
	switch name {
	case "get_option_chains":
		return object{"data": object{"chains": []any{c.chain}}}, nil
	case "get_equity_quotes":
		return object{"data": object{"results": []any{object{"quote": c.stock}}}}, nil
	case "get_option_instruments":
		if id, lookup := args["ids"]; lookup {
			if id != "put-90" || len(args) != 1 {
				return nil, fmt.Errorf("refresh changed the selected identity: %v", args)
			}
			var matches []any
			for _, item := range c.menu {
				if item.(object)["id"] == id {
					matches = append(matches, item)
				}
			}
			return object{"data": object{"instruments": matches}}, nil
		}
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

func fixedMarketFixture(at time.Time) *fixedMarketClient {
	stamp := at.Format(time.RFC3339Nano)
	client := &fixedMarketClient{
		stock: object{"symbol": "AAPL", "has_traded": true, "state": "active", "bid_price": "99.99", "ask_price": "100.01", "venue_bid_time": stamp, "venue_ask_time": stamp, "venue_last_trade_time": stamp},
		chain: object{"id": "chain", "symbol": "AAPL", "can_open_position": true, "trade_value_multiplier": "100", "cash_component": nil, "underlying_instruments": []any{object{"instrument": "equity", "symbol": "AAPL"}}, "expiration_dates": []any{"2026-10-09", "2026-11-06", "2026-10-30"}},
		quote: object{"instrument_id": "put-90", "bid_price": "1.00", "ask_price": "1.10", "bid_size": json.Number("1"), "updated_at": stamp},
	}
	for _, row := range []struct{ id, strike, expiry string }{{"put-90", "90.00", "2026-10-30"}, {"put-85", "85.00", "2026-10-30"}, {"later", "90.00", "2026-11-06"}} {
		client.menu = append(client.menu, object{"id": row.id, "chain_id": "chain", "chain_symbol": "AAPL", "type": "put", "expiration_date": row.expiry, "strike_price": row.strike, "trade_value_multiplier": "100", "underlying_type": "equity", "tradability": "tradable", "state": "active", "min_ticks": object{"cutoff_price": "3.00", "below_tick": "0.01", "above_tick": "0.05"}})
	}
	return client
}

func TestFixedMarketObservation(t *testing.T) {
	at := time.Date(2026, 10, 2, 16, 0, 0, 0, time.UTC)
	for _, scenario := range []string{"fresh", "stale_ask", "wide_stock", "zero_chosen_bid", "untradable_chosen", "missing_bid_clock", "conflicting_id", "duplicate_menu", "duplicate_expiry", "missing_cash_deliverable"} {
		t.Run(scenario, func(t *testing.T) {
			client := fixedMarketFixture(at)
			wantErr := false
			switch scenario {
			case "stale_ask":
				client.stock["venue_ask_time"] = at.Add(-31 * time.Second).Format(time.RFC3339Nano)
			case "wide_stock":
				client.stock["bid_price"], client.stock["ask_price"] = "90.00", "110.00"
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
			if got.MarketFresh != (scenario != "stale_ask") || got.EntryBookUsable != (scenario != "wide_stock" && scenario != "zero_chosen_bid" && scenario != "untradable_chosen") {
				t.Fatalf("bad freshness/liquidity decision: %+v", got)
			}
			if !slices.Equal(client.calls, []string{"get_option_chains", "get_equity_quotes", "get_option_instruments", "get_option_quotes"}) {
				t.Fatalf("unexpected request sequence: %v", client.calls)
			}
		})
	}
}

func TestFixedIdentityRefresh(t *testing.T) {
	at := time.Date(2026, 10, 2, 16, 0, 0, 0, time.UTC)
	for _, scenario := range []string{"moved_stock", "metadata_drift", "duplicate_lookup", "missing_expiration", "zero_chosen_bid", "stale_side"} {
		t.Run(scenario, func(t *testing.T) {
			client := fixedMarketFixture(at)
			b := &Broker{MCP: client, Config: wheel.DefaultConfig()}
			signal, err := b.ObserveFixedPolicy(t.Context(), "conservative", "put", "AAPL", func() time.Time { return at })
			if err != nil || signal.Choice == nil {
				t.Fatal("signal observation failed", err)
			}
			client.calls = nil
			other := maps.Clone(client.menu[0].(object))
			other["id"], other["strike_price"] = "put-105", "105.00"
			client.menu = append(client.menu, other)
			// A fresh signal at this higher spot would choose a different strike.
			// Execution refresh must quote only the original signal's $90 put.
			client.stock["bid_price"], client.stock["ask_price"] = "119.99", "120.01"
			later := at.Add(time.Minute)
			stamp := later.Format(time.RFC3339Nano)
			client.stock["venue_bid_time"], client.stock["venue_ask_time"] = stamp, stamp
			client.quote["updated_at"] = stamp
			retargeted, chooseErr := contractpolicy.Choose("conservative", "put", signal.Day, signal.Symbol, contractpolicy.Book{Bid: 11999, Ask: 12001},
				[]contractpolicy.Contract{*signal.Choice, {ID: "put-105", Symbol: signal.Symbol, Expiry: signal.Choice.Expiry, Kind: "put", Strike: 10500}})
			if chooseErr != nil || retargeted == nil || retargeted.ID != "put-105" {
				t.Fatal("fixture does not distinguish refresh from retargeting", retargeted, chooseErr)
			}
			switch scenario {
			case "metadata_drift":
				client.menu[0].(object)["strike_price"] = "91.00"
			case "duplicate_lookup":
				client.menu = append(client.menu, client.menu[0])
			case "missing_expiration":
				client.chain["expiration_dates"] = []any{"2026-10-09"}
			case "zero_chosen_bid":
				client.quote["bid_price"], client.quote["bid_size"] = "0.00", json.Number("0")
			case "stale_side":
				client.stock["venue_ask_time"] = at.Format(time.RFC3339Nano)
			}
			got, err := b.RefreshFixedIdentity(t.Context(), "conservative", *signal.Choice, func() time.Time { return later })
			wantErr := scenario == "metadata_drift" || scenario == "duplicate_lookup" || scenario == "missing_expiration"
			if (err != nil) != wantErr {
				t.Fatal("incorrect refresh identity gate", scenario, err)
			}
			if wantErr {
				if slices.Contains(client.calls, "get_option_quotes") {
					t.Fatal("ambiguous metadata reached quote refresh")
				}
				return
			}
			if got.Choice == nil || *got.Choice != *signal.Choice || !got.IdentityRefresh || got.MenuContracts != 1 || got.TradingAuthorized || got.DeliverableReview {
				t.Fatalf("refresh replaced identity or granted authority: %+v", got)
			}
			if got.MarketFresh != (scenario != "stale_side") || got.EntryBookUsable != (scenario != "zero_chosen_bid") {
				t.Fatal("refreshed clocks or liquidity ignored", got)
			}
		})
	}
}
