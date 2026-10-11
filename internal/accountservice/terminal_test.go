package accountservice

import (
	"encoding/json"
	"testing"
	"time"

	"autowheel/internal/sharedbroker"
)

func TestCancelledPartialBuyIsAppliedOnceBeforeCancelCompletes(t *testing.T) {
	s, b := testServer(t)
	b.request.Payload = json.RawMessage(`{"request":{},"arguments":{"account_number":"1234","symbol":"AAA","side":"buy","quantity":"1","type":"limit","limit_price":"1000","time_in_force":"gfd","market_hours":"regular_hours"},"ref_id":"r"}`)
	prepared := s.handle(t.Context(), "tradepulse", rpc("prepare_stock", map[string]any{}))
	if !prepared.OK {
		t.Fatal(prepared)
	}
	in, _ := s.Ledger.Lookup("tradepulse", "k")
	if _, err := s.Ledger.StartSend("tradepulse", "k", in.TicketVersion, in.Request.Digest, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Ledger.Complete("tradepulse", "k", "order-1", json.RawMessage(`{"order":true}`)); err != nil {
		t.Fatal(err)
	}
	in, _ = s.Ledger.Lookup("tradepulse", "k")
	b.facts.Orders = []sharedbroker.Object{{"id": "order-1", "symbol": "AAA", "state": "queued"}}
	if response := s.handle(t.Context(), "tradepulse", rpc("cancel_stock", map[string]any{"order_id": "order-1"})); !response.OK {
		t.Fatal(response)
	}
	at := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	b.facts.Digest = b.request.Digest
	b.facts.Positions = []sharedbroker.Object{{"symbol": "AAA", "quantity": "0.4"}}
	b.facts.Orders = []sharedbroker.Object{{"id": "order-1", "instrument_id": "AAA", "symbol": "AAA", "state": "partially_filled_rest_cancelled", "placed_agent": "agentic", "ref_id": "r", "quantity": "1", "cumulative_quantity": "0.4", "side": "buy", "fees": "0", "created_at": at, "executions": []any{sharedbroker.Object{"id": "fill-1", "quantity": "0.4", "price": "1000", "fees": "0", "timestamp": at}}}}
	if _, err := s.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Ledger.Status()
	cancel, _ := s.Ledger.Lookup("tradepulse", "cancel:order-1")
	if st.FreeCashCents != 960_000 || st.CapitalCents != 40_000 || st.Owners[0].QuantityMicros != 400_000 || cancel.State != "cancel_terminal" {
		t.Fatal("fill/cancel race lost accounting", st, cancel)
	}
	if _, err := s.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	again, _ := s.Ledger.Status()
	if again.FreeCashCents != st.FreeCashCents || again.CapitalCents != st.CapitalCents {
		t.Fatal("cumulative fill applied twice", again)
	}
	b.facts.Orders[0]["cumulative_quantity"] = "0.5"
	if _, _, err := stockTerminal(in, b.facts.Orders[0], time.Now()); err == nil {
		t.Fatal("incomplete cumulative executions accepted")
	}
	if _, err := s.refresh(t.Context()); err == nil {
		t.Fatal("previous terminal economics changed without a freeze")
	}
}
