package robinhood

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"autowheel/internal/wheel"
)

func closingReviewFixture(t *testing.T) (object, wheel.Config, wheel.Candidate) {
	o, c, a := previewFixture(t)
	a.Closing, a.CloseLimit = true, 170
	o["direction"] = "debit"
	leg := o["legs"].([]any)[0].(object)
	leg["side"], leg["position_effect"] = "buy", "close"
	o["collateral"].(object)["cash"].(object)["direction"] = "credit"
	q := o["option_quotes"].([]any)[0].(object)
	q["ask_price"], q["ask_size"] = "1.70", json.Number("1")
	return o, c, a
}

func TestCloseReviewAndReceipt(t *testing.T) {
	for _, change := range []string{"valid", "open_direction", "open_side", "fee", "release_amount", "release_direction", "ask", "liquidity", "stale"} {
		t.Run(change, func(t *testing.T) {
			o, c, a := closingReviewFixture(t)
			switch change {
			case "open_direction":
				o["direction"] = "credit"
			case "open_side":
				o["legs"].([]any)[0].(object)["side"] = "sell"
			case "fee":
				o["fees"].(object)["total_fee"] = "1.01"
			case "release_amount":
				o["collateral"].(object)["cash"].(object)["amount"] = "22999"
			case "release_direction":
				o["collateral"].(object)["cash"].(object)["direction"] = "debit"
			case "ask":
				o["option_quotes"].([]any)[0].(object)["ask_price"] = "1.71"
			case "liquidity":
				o["option_quotes"].([]any)[0].(object)["ask_size"] = json.Number("0")
			case "stale":
				o["option_quotes"].([]any)[0].(object)["updated_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339Nano)
			}
			err := reviewIdentity(o, c.Account, a)
			if err == nil {
				err = validateReview(o, c, a, true)
			}
			if (err == nil) != (change == "valid") {
				t.Fatalf("review %s: %v", change, err)
			}
		})
	}
	_, _, a := closingReviewFixture(t)
	order := object{"id": "close-order", "created_at": time.Now().Format(time.RFC3339Nano), "placed_agent": "agentic", "chain_symbol": "NVDA", "direction": "debit", "quantity": "1", "price": "1.70", "premium": "170.00", "trade_value_multiplier": "100.0000", "type": "limit", "trigger": "immediate", "time_in_force": "gfd", "market_hours": "regular_hours", "state": "confirmed", "legs": []any{object{"option_id": "option", "option_type": "put", "strike_price": "230.00", "expiration_date": "2026-10-09", "side": "buy", "position_effect": "close", "ratio_quantity": json.Number("1")}}}
	if id, err := receipt(object{"data": object{"order": order}}, a); err != nil || id != "close-order" {
		t.Fatal("valid close receipt", err)
	}
	order["direction"] = "credit"
	if _, err := receipt(object{"data": object{"order": order}}, a); err == nil {
		t.Fatal("opening receipt accepted for close")
	}
	b := Broker{}
	args := b.orderArgs(a)
	leg := args["legs"].([]object)[0]
	if args["price"] != "1.70" || leg["side"] != "buy" || leg["position_effect"] != "close" {
		t.Fatal("wrong close transport", args)
	}
}

type closeQuoteClient struct {
	mutate string
	at     time.Time
}

func (c closeQuoteClient) Supports(string, string) bool { return true }
func (c closeQuoteClient) Call(_ context.Context, name string, args object) (any, error) {
	stamp := time.Now().Format(time.RFC3339Nano)
	switch name {
	case "get_option_instruments":
		id := "option"
		if c.mutate == "contract" {
			id = "other"
		}
		return object{"instruments": []any{object{"id": id, "chain_id": "chain", "chain_symbol": "NVDA", "underlying_type": "equity", "expiration_date": "2026-10-16", "strike_price": "210.00", "type": "put", "state": "active", "tradability": "tradable", "trade_value_multiplier": "100.0000", "min_ticks": object{"above_tick": "0.05", "below_tick": "0.01", "cutoff_price": "3.00"}}}}, nil
	case "get_option_orders":
		credit := "22.00"
		if c.mutate == "credit" {
			credit = "2200"
		}
		return object{"orders": []any{object{"id": "opening", "created_at": c.at.Format(time.RFC3339Nano), "placed_agent": "agentic", "chain_id": "chain", "chain_symbol": "NVDA", "direction": "credit", "quantity": "1", "processed_quantity": "1", "processed_premium": credit, "price": "0.22", "premium": "22.00", "trade_value_multiplier": "100.0000", "type": "limit", "trigger": "immediate", "time_in_force": "gfd", "market_hours": "regular_hours", "state": "filled", "legs": []any{object{"option_id": "option", "option_type": "put", "strike_price": "210.00", "expiration_date": "2026-10-16", "side": "sell", "position_effect": "open", "ratio_quantity": json.Number("1"), "executions": []any{object{"price": "0.22", "quantity": "1"}}}}}}}, nil
	case "get_equity_quotes":
		return object{"results": []any{object{"quote": object{"symbol": "NVDA", "state": "active", "has_traded": true, "last_trade_price": "216.72", "venue_last_trade_time": stamp}}}}, nil
	case "get_option_quotes":
		size := "20"
		if c.mutate == "liquidity" {
			size = "0"
		}
		if c.mutate == "stale" {
			stamp = time.Now().Add(-time.Minute).Format(time.RFC3339Nano)
		}
		return object{"results": []any{object{"quote": object{"instrument_id": "option", "bid_price": "0.01", "ask_price": "0.02", "ask_size": size, "updated_at": stamp}}}}, nil
	default:
		return nil, fmt.Errorf("unexpected financial or other tool: %s", name)
	}
}

func TestCloseQuoteExactOwnedFill(t *testing.T) {
	for _, change := range []string{"valid", "unowned", "duplicate", "contract", "credit", "liquidity", "stale"} {
		t.Run(change, func(t *testing.T) {
			c := wheel.DefaultConfig()
			c.Account = "account"
			at := time.Now().Add(-time.Hour)
			st := wheel.NewState()
			st.Account = c.Account
			st.Positions["NVDA"] = wheel.Position{PutID: "option", PutExpiry: "2026-10-16", PutStrike: 21000, PutOpenedAt: at, Provenance: "own"}
			st.CompletedOrders = map[string]wheel.OrderRecord{"opening": {Ref: "ref", Symbol: "NVDA", OptionID: "option", At: at}}
			if change == "unowned" {
				delete(st.CompletedOrders, "opening")
			}
			if change == "duplicate" {
				st.CompletedOrders["other"] = st.CompletedOrders["opening"]
			}
			b := Broker{Config: c, MCP: closeQuoteClient{mutate: change, at: at}}
			a, err := b.CloseQuote(t.Context(), st, "NVDA")
			if change == "valid" {
				if err != nil || a.EntryCredit != 2200 || a.CloseLimit != 2 || !a.Closing {
					t.Fatalf("close quote %+v %v", a, err)
				}
			} else if err == nil {
				t.Fatal("unproven close quote admitted")
			}
		})
	}
}
