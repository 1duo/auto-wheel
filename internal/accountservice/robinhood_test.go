package accountservice

import (
	"context"
	"encoding/json"
	"testing"

	"autowheel/internal/coordinator"
	"autowheel/internal/sharedbroker"
)

type stockWire struct {
	result sharedbroker.Object
}

func (*stockWire) Supports(string, string) bool { return true }
func (b *stockWire) Call(_ context.Context, name string, _ map[string]any) (any, error) {
	if name == "get_option_chains" {
		return sharedbroker.Object{"chains": []any{sharedbroker.Object{
			"symbol": "MU", "underlying_instruments": []any{sharedbroker.Object{"instrument": "https://api.robinhood.com/instruments/01234567-0123-0123-0123-0123456789ab/"}},
		}}}, nil
	}
	return b.result, nil
}

func TestStockPlanPreservesDayAndMicroshareSemantics(t *testing.T) {
	b := Robinhood{Facts: &sharedbroker.Backend{Client: &stockWire{}, Account: "1234"}, StockFeeBound: func(sharedbroker.Object) (int64, error) { return 100, nil }}
	request := json.RawMessage(`{"intent_key":"exit","symbol":"MU","side":"sell","quantity":1.2920840000000002,"kind":"regular_hours_market","limit_price":null,"stop_price":null,"time_in_force":"day"}`)
	arguments := sharedbroker.Object{"account_number": "1234", "symbol": "MU", "side": "sell", "quantity": "1.292084", "type": "market", "time_in_force": "gfd", "market_hours": "regular_hours"}
	encode := func() json.RawMessage {
		raw, err := json.Marshal(stockPreparation{Request: request, Arguments: arguments, Ref: "reference"})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	r, err := b.Plan(t.Context(), "tradepulse", encode())
	if err != nil || r.QuantityMicros != 1_292_084 || r.SharesMicros != 1_292_084 || r.Kind != "stock_sell" {
		t.Fatal(r, err)
	}
	arguments["quantity"] = "1.292083"
	if _, err := b.Plan(t.Context(), "tradepulse", encode()); err == nil {
		t.Fatal("one-microshare change accepted")
	}
	arguments["quantity"] = "1.292084"
	arguments["time_in_force"] = "gtc"
	if _, err := b.Plan(t.Context(), "tradepulse", encode()); err == nil {
		t.Fatal("day order silently converted to GTC")
	}
}

func TestStockReceiptAcceptsEquivalentLimitAndMarketReferencePrice(t *testing.T) {
	for _, kind := range []string{"limit", "market"} {
		t.Run(kind, func(t *testing.T) {
			args := sharedbroker.Object{"account_number": "1234", "symbol": "MU", "side": "sell", "quantity": "1", "type": kind, "time_in_force": "gfd", "market_hours": "regular_hours"}
			if kind == "limit" {
				args["limit_price"] = "1000"
			}
			order := sharedbroker.Object{"id": "order-1", "symbol": "MU", "side": "sell", "quantity": "1.000000", "instrument_id": "instrument", "placed_agent": "agentic", "type": kind, "trigger": "immediate", "market_hours": "regular_hours", "time_in_force": "gfd", "price": "1000.000000", "stop_price": nil}
			wire := &stockWire{result: sharedbroker.Object{"data": sharedbroker.Object{"order": order}}}
			b := Robinhood{Facts: &sharedbroker.Backend{Client: wire, Account: "1234"}}
			raw, err := json.Marshal(stockPreparation{Arguments: args, Ref: "reference"})
			if err != nil {
				t.Fatal(err)
			}
			in := coordinator.Intent{Strategy: "tradepulse", Request: coordinator.Request{Payload: raw, RefID: "reference", Symbol: "MU", UnderlyingID: "instrument", QuantityMicros: 1_000_000}}
			if id, _, err := b.PlaceOnce(t.Context(), in); err != nil || id != "order-1" {
				t.Fatal(id, err)
			}
			order["trigger"] = "stop"
			if _, _, err := b.PlaceOnce(t.Context(), in); err == nil {
				t.Fatal("changed execution terms accepted")
			}
			order["trigger"] = "immediate"
			if kind == "limit" {
				order["price"] = "1000.01"
				if _, _, err := b.PlaceOnce(t.Context(), in); err == nil {
					t.Fatal("changed limit price accepted")
				}
			}
		})
	}
}
