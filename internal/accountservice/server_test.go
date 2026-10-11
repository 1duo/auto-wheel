package accountservice

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"autowheel/internal/coordinator"
	"autowheel/internal/sharedbroker"
	"autowheel/internal/sharedclient"
)

type fakeBroker struct {
	facts           sharedbroker.Facts
	err             error
	request         coordinator.Request
	places, cancels int
}

func (b *fakeBroker) Snapshot(context.Context) (sharedbroker.Facts, error) {
	b.facts.At = time.Now()
	return b.facts, b.err
}
func (b *fakeBroker) Underlying(context.Context, string) (string, error) { return "AAA", nil }
func (b *fakeBroker) Plan(context.Context, string, json.RawMessage) (coordinator.Request, error) {
	return b.request, nil
}
func (b *fakeBroker) Review(context.Context, coordinator.Intent) (json.RawMessage, time.Time, error) {
	return json.RawMessage(`{"review":true}`), time.Now().Add(time.Minute), nil
}
func (b *fakeBroker) PlaceOnce(context.Context, coordinator.Intent) (string, json.RawMessage, error) {
	b.places++
	return "", nil, errors.New("lost broker response")
}
func (b *fakeBroker) CancelOnce(context.Context, coordinator.Intent) (json.RawMessage, error) {
	b.cancels++
	return json.RawMessage(`{"data":{"accepted":true}}`), nil
}

func testServer(t *testing.T) (*Server, *fakeBroker) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	l, err := coordinator.OpenLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	h := strings.Repeat("a", 64)
	if err = l.Initialize(coordinator.Import{AccountHash: h, PolicyHash: h, SourceProof: h, FreeCashCents: 1_000_000, Registrations: []coordinator.Registration{{Strategy: "tradepulse", Release: h, PolicyHash: h, NewRisk: true, Management: true}, {Strategy: "autowheel", Release: h, PolicyHash: h, NewRisk: true}}}); err != nil {
		t.Fatal(err)
	}
	if err = l.SetAccountPause(false); err != nil {
		t.Fatal(err)
	}
	b := &fakeBroker{facts: sharedbroker.Facts{AccountHash: h, EligibleCash: 1_000_000}, request: coordinator.Request{IntentKey: "k", RefID: "r", UnderlyingID: "AAA", Symbol: "AAA", Kind: "stock_buy", QuantityMicros: 1_000_000, CashCents: 100_000, CapitalCents: 100_000, Digest: h}}
	s := &Server{Config: Config{SocketDir: dir, PolicyHash: h, Releases: map[string]string{"tradepulse": h, "autowheel": h}, Qualification: func(coordinator.Status, sharedbroker.Facts) error { return nil }}, Ledger: l, Broker: b}
	return s, b
}
func rpc(op string, payload any) sharedclient.Request {
	raw, _ := json.Marshal(payload)
	return sharedclient.Request{Version: 1, Op: op, Payload: raw}
}

func TestLostPlacementNeverReplaysOrReleases(t *testing.T) {
	s, b := testServer(t)
	prepared := s.handle(t.Context(), "tradepulse", rpc("prepare_stock", map[string]any{}))
	if !prepared.OK {
		t.Fatal(prepared)
	}
	var ticket map[string]any
	json.Unmarshal(prepared.Data, &ticket)
	commit := rpc("commit_stock", map[string]any{"intent_key": ticket["intent_key"], "request_digest": ticket["request_digest"], "ticket_version": ticket["ticket_version"]})
	first := s.handle(t.Context(), "tradepulse", commit)
	if first.OK || first.Unsent || b.places != 1 {
		t.Fatal(first, b.places)
	}
	s.handle(t.Context(), "tradepulse", commit)
	st, err := s.Ledger.Status()
	if err != nil || b.places != 1 || st.FreeCashCents != 900_000 || st.Intents[0].State != "uncertain" {
		t.Fatal(st, err, b.places)
	}
	if err = s.Ledger.Withdraw("tradepulse", "k"); err == nil {
		t.Fatal("lost financial response withdrawn")
	}
}
func TestCancelAcknowledgmentRetainsOriginalObligations(t *testing.T) {
	s, b := testServer(t)
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
	b.facts.Orders = []sharedbroker.Object{{"id": "order-1", "symbol": "AAA", "state": "queued"}}
	request := rpc("cancel_stock", map[string]any{"order_id": "order-1"})
	first := s.handle(t.Context(), "tradepulse", request)
	if !first.OK {
		t.Fatal(first)
	}
	second := s.handle(t.Context(), "tradepulse", request)
	if !second.OK || b.cancels != 1 {
		t.Fatal(second, b.cancels)
	}
	st, _ := s.Ledger.Status()
	original, _ := s.Ledger.Lookup("tradepulse", "k")
	if st.FreeCashCents != 900_000 || st.CapitalCents != 100_000 || original.State != "accepted" {
		t.Fatal("cancel ack released resources", st, original)
	}
	if got := s.handle(t.Context(), "autowheel", request); got.OK {
		t.Fatal("peer cancellation admitted")
	}
}
func TestPausePersistsWithoutBrokerHealth(t *testing.T) {
	s, b := testServer(t)
	b.err = errors.New("broker unavailable")
	before, _ := s.Ledger.Status()
	result := s.handle(t.Context(), "autowheel", rpc("permission", map[string]any{"new_risk": false, "management": false}))
	if !result.OK {
		t.Fatal(result)
	}
	after, _ := s.Ledger.Status()
	for i, r := range after.Registrations {
		if r.Strategy == "autowheel" && (r.NewRisk || r.Epoch <= before.Registrations[i].Epoch) {
			t.Fatal("pause did not fence tickets", r)
		}
	}
	grant := s.handle(t.Context(), "autowheel", rpc("permission", map[string]any{"new_risk": true, "management": false}))
	if grant.OK {
		t.Fatal("permission granted without broker health")
	}
}

func TestPendingQualificationPreventsReadyAndAdmission(t *testing.T) {
	s, b := testServer(t)
	s.Config.PendingQualification = []string{"cutover not qualified", "fees not qualified"}
	if _, err := s.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	st, err := s.Ledger.Status()
	if err != nil {
		t.Fatal(err)
	}
	status := s.health("tradepulse", b.facts, st)
	if status.Ready || len(status.Blockers) != 2 {
		t.Fatal("qualification requirements hidden or bypassed", status)
	}
	result := s.handle(t.Context(), "tradepulse", rpc("prepare_stock", map[string]any{}))
	if result.OK || b.places != 0 {
		t.Fatal("unqualified execution admitted", result)
	}
}
