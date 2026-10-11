package coordinator

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

var proof = strings.Repeat("a", 64)
var review = json.RawMessage(`{"exact_review":true}`)

func seed(t *testing.T, cash int64, owners ...Ownership) (*Ledger, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	l, err := OpenLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	var cost int64
	for _, o := range owners {
		cost += ownerCommitment(o)
	}
	err = l.Initialize(Import{AccountHash: proof, PolicyHash: proof, SourceProof: proof, FreeCashCents: cash, CapitalCents: cost, Owners: owners, Registrations: []Registration{
		{Strategy: "tradepulse", Release: proof, PolicyHash: proof, NewRisk: true, Management: true},
		{Strategy: "autowheel", Release: proof, PolicyHash: proof, NewRisk: true},
	}})
	if err != nil {
		l.Close()
		t.Fatal(err)
	}
	if err = l.SetAccountPause(false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, dir
}
func req(key, id, kind string, cash int64) Request {
	r := Request{IntentKey: key, UnderlyingID: id, Symbol: id, Kind: kind, CashCents: cash, RefID: key, Digest: proof, QuantityMicros: 1_000_000}
	if kind == "stock_buy" || kind == "put" {
		r.CapitalCents = cash
	}
	return r
}
func prepare(t *testing.T, l *Ledger, owner string, r Request) Intent {
	t.Helper()
	if _, err := l.Reserve(owner, r, 1_000_000, time.Now()); err != nil {
		t.Fatal(err)
	}
	in, err := l.ReviewPrepared(owner, r.IntentKey, hash(review), "", review, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return in
}
func TestCashAndOwnershipAdmission(t *testing.T) {
	l, _ := seed(t, 1_000_000)
	first := prepare(t, l, "tradepulse", req("six", "AAA", "stock_buy", 600_000))
	if _, err := l.Reserve("autowheel", req("eight", "BBB", "put", 800_000), 1_000_000, time.Now()); err == nil {
		t.Fatal("same cash allocated twice")
	}
	if _, err := l.StartSend("tradepulse", "six", first.TicketVersion, proof, time.Now()); err != nil {
		t.Fatal("existing hold deducted twice at commit", err)
	}
	if _, err := l.StartSend("tradepulse", "six", first.TicketVersion, proof, time.Now()); err == nil {
		t.Fatal("placement replayed")
	}
	s, _ := l.Status()
	if s.FreeCashCents != 400_000 || s.CapitalCents != 600_000 {
		t.Fatal("reservation accounting changed", s)
	}
}
func TestConcurrentUnderlyingHasOneOwner(t *testing.T) {
	l, _ := seed(t, 1_000_000)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	wg.Go(func() {
		_, err := l.Reserve("tradepulse", req("stock", "SAME", "stock_buy", 100_000), 1_000_000, time.Now())
		results <- err
	})
	wg.Go(func() {
		_, err := l.Reserve("autowheel", req("put", "SAME", "put", 100_000), 1_000_000, time.Now())
		results <- err
	})
	wg.Wait()
	close(results)
	admitted := 0
	for err := range results {
		if err == nil {
			admitted++
		}
	}
	s, _ := l.Status()
	if admitted != 1 || len(s.Owners) != 1 || len(s.Intents) != 1 {
		t.Fatal("underlying got multiple owners", admitted, s)
	}
}
func TestTicketsEpochWithdrawalAndRestart(t *testing.T) {
	l, dir := seed(t, 1_000_000)
	r := req("intent", "AAA", "stock_buy", 100_000)
	old := prepare(t, l, "tradepulse", r)
	fresh, err := l.ReviewPrepared("tradepulse", r.IntentKey, hash(review), "", review, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l.StartSend("tradepulse", r.IntentKey, old.TicketVersion, proof, time.Now()); err == nil {
		t.Fatal("superseded ticket sent")
	}
	if err = l.SetPermission("tradepulse", true, true); err != nil {
		t.Fatal(err)
	}
	if _, err = l.StartSend("tradepulse", r.IntentKey, fresh.TicketVersion, proof, time.Now()); err == nil {
		t.Fatal("old permission epoch sent")
	}
	if err = l.Withdraw("tradepulse", r.IntentKey); err != nil {
		t.Fatal(err)
	}
	if _, err = l.StartSend("tradepulse", r.IntentKey, fresh.TicketVersion, proof, time.Now()); err == nil {
		t.Fatal("withdrawn late commit sent")
	}
	changed := r
	changed.CashCents++
	if _, err = l.Reserve("tradepulse", changed, 1_000_000, time.Now()); err == nil {
		t.Fatal("intent key mutated")
	}
	retained, err := l.Reserve("tradepulse", r, 1_000_000, time.Now())
	if err != nil || retained.State != "withdrawn" {
		t.Fatal("withdrawn intent resurrected", retained, err)
	}
	crashed := prepare(t, l, "tradepulse", req("crash", "BBB", "stock_buy", 200_000))
	if _, err = l.StartSend("tradepulse", "crash", crashed.TicketVersion, proof, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s, _ := reopened.Status()
	if s.FreeCashCents != 800_000 {
		t.Fatal("restart released uncertain cash", s.FreeCashCents)
	}
	if err = reopened.Withdraw("tradepulse", "crash"); err == nil {
		t.Fatal("started intent withdrawn")
	}
	if _, err = reopened.Reserve("autowheel", req("peer", "CCC", "put", 100_000), 1_000_000, time.Now()); err == nil {
		t.Fatal("unknown write allowed another transport")
	}
}
func TestCallCoverageAssignmentAndCallAway(t *testing.T) {
	l, _ := seed(t, 1_000_000, Ownership{UnderlyingID: "PEER", Symbol: "PEER", Strategy: "tradepulse", QuantityMicros: 100_000_000, CostCents: 100_000})
	call := req("peer-call", "PEER", "call", 100)
	call.SharesMicros = 100_000_000
	if _, err := l.Reserve("autowheel", call, 1_000_000, time.Now()); err == nil {
		t.Fatal("call used peer shares")
	}
	put := req("own-put", "OWN", "put", 100_100)
	put.CapitalCents = 100_000
	in := prepare(t, l, "autowheel", put)
	if _, err := l.StartSend("autowheel", put.IntentKey, in.TicketVersion, proof, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := l.Complete("autowheel", put.IntentKey, "put-id", json.RawMessage(`{"id":"put-id"}`)); err != nil {
		t.Fatal(err)
	}
	owner := Ownership{UnderlyingID: "OWN", Symbol: "OWN", Strategy: "autowheel", PutCollateralCents: 100_000, CashHoldCents: 100_100}
	if err := l.ApplyTerminal("autowheel", put.IntentKey, TerminalProof{BrokerOrderID: "put-id", EvidenceDigest: proof, FilledMicros: 1_000_000, Ownership: owner}); err != nil {
		t.Fatal(err)
	}
	s, _ := l.Status()
	var v int64
	for _, o := range s.Owners {
		if o.Symbol == "OWN" {
			v = o.Version
		}
	}
	assigned := Ownership{UnderlyingID: "OWN", Symbol: "OWN", Strategy: "autowheel", QuantityMicros: 100_000_000, CostCents: 100_000}
	event := InventoryTransition{EventID: "assignment", EvidenceDigest: proof, Kind: "put_assignment", PriorVersion: v, DebitCents: 100_100, Ownership: assigned}
	if err := l.ApplyInventoryTransition(event); err != nil {
		t.Fatal(err)
	}
	if err := l.ApplyInventoryTransition(event); err != nil {
		t.Fatal("assignment applied twice", err)
	}
	call = req("own-call", "OWN", "call", 100)
	call.SharesMicros = 100_000_000
	if _, err := l.Reserve("autowheel", call, 1_000_000, time.Now()); err != nil {
		t.Fatal(err)
	}
	duplicate := call
	duplicate.IntentKey = "duplicate"
	duplicate.RefID = "duplicate"
	if _, err := l.Reserve("autowheel", duplicate, 1_000_000, time.Now()); err == nil {
		t.Fatal("owned coverage reused")
	}
	sale := req("peer-sale", "OWN", "stock_sell", 0)
	sale.SharesMicros = 100_000_000
	if _, err := l.Reserve("tradepulse", sale, 1_000_000, time.Now()); err == nil {
		t.Fatal("peer sold covered shares")
	}
}
func TestAccountPauseAlsoBlocksManagement(t *testing.T) {
	l, _ := seed(t, 1_000_000, Ownership{UnderlyingID: "AAA", Symbol: "AAA", Strategy: "tradepulse", QuantityMicros: 1_000_000, CostCents: 10000})
	sale := req("exit", "AAA", "stock_sell", 0)
	sale.SharesMicros = 1_000_000
	if err := l.SetPermission("tradepulse", false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve("tradepulse", sale, 1_000_000, time.Now()); err != nil {
		t.Fatal("paused new risk lost owned management", err)
	}
	if err := l.SetAccountPause(true); err != nil {
		t.Fatal(err)
	}
	sale.IntentKey = "second"
	sale.RefID = "second"
	if _, err := l.Reserve("tradepulse", sale, 1_000_000, time.Now()); err == nil {
		t.Fatal("management bypassed account pause")
	}
}
