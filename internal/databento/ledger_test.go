package databento

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLedgerSecondProcessRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	path := filepath.Join(dir, "ledger.json")
	first, err := OpenLedger(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer first.Close()
	if _, err := OpenLedger(path); err == nil {
		t.Fatal("second concurrent ledger must be rejected")
	}
}

func TestLedgerBudgetAndMutation(t *testing.T) {
	ledger := newTestLedger(t)
	q := newTestQuery()
	qh, _ := q.Hash()
	op, _ := q.OperationID(RangeEndpoint)
	respHash := strings.Repeat("4", 64)
	if _, err := ledger.Reserve(op, qh, RangeEndpoint, 1_000_000_000, time.Now().UTC(), respHash); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Same identity must never grant a second send.
	if _, err := ledger.Reserve(op, qh, RangeEndpoint, 1_000_000_000, time.Now().UTC(), respHash); err == nil {
		t.Fatal("duplicate reservation accepted")
	}
	// Same identity, mutated cost: rejected.
	if _, err := ledger.Reserve(op, qh, RangeEndpoint, 2_000_000_000, time.Now().UTC(), respHash); err == nil {
		t.Fatal("mutated reservation must be rejected")
	}
	// Excessive cost beyond $10 lifetime: rejected.
	q2 := newTestQuery()
	q2.Symbols = []string{"MSFT"}
	qh2, _ := q2.Hash()
	op2, _ := q2.OperationID(RangeEndpoint)
	if _, err := ledger.Reserve(op2, qh2, RangeEndpoint, MaxLifetimeNano, time.Now().UTC(), respHash); err == nil {
		t.Fatal("excessive budget must be rejected")
	}
	if got := ledger.Remaining(); got != MaxLifetimeNano-1_100_000_000 {
		t.Fatalf("remaining %d unexpected", got)
	}
}

func TestLedgerRejectsCorruption(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	path := filepath.Join(dir, "ledger.json")
	// Trailing content.
	if err := os.WriteFile(path, []byte("{}\nTRAIL"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := OpenLedger(path); err == nil {
		t.Fatal("trailing content must be rejected")
	}
	// Overflow total.
	overflow := `{"version":1,"lifetime_nano":10000000000,"reservations":[{"operation_id":"` + strings.Repeat("a", 64) + `","query_hash":"` + strings.Repeat("b", 64) + `","endpoint":"/timeseries.get_range","cost_nano":9000000000,"reserved_nano":9900000000,"at":"2024-01-01T00:00:00Z","response_hash":"` + strings.Repeat("c", 64) + `"},{"operation_id":"` + strings.Repeat("d", 64) + `","query_hash":"` + strings.Repeat("e", 64) + `","endpoint":"/timeseries.get_range","cost_nano":9000000000,"reserved_nano":9900000000,"at":"2024-01-01T00:00:00Z","response_hash":"` + strings.Repeat("f", 64) + `"}]}` + "\n"
	if err := os.WriteFile(path, []byte(overflow), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := OpenLedger(path); err == nil {
		t.Fatal("overflow ledger must be rejected")
	}
}

func TestLedgerRejectsInsecurePaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	// Parent made group-readable: must be rejected.
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := OpenLedger(path); err == nil {
		t.Fatal("non-private parent must be rejected")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	l, err := OpenLedger(path)
	if err != nil {
		t.Fatalf("open after fix: %v", err)
	}
	l.Close()
	// Symlinked ledger path must be rejected.
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte(`{"version":1,"lifetime_nano":10000000000,"reservations":[]}`+"\n"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(real, link); err == nil {
		if _, err := OpenLedger(link); err == nil {
			t.Fatal("symlink ledger must be rejected")
		}
	}
}

func TestLedgerConcurrentDistinctIdentities(t *testing.T) {
	ledger := newTestLedger(t)
	const attempts = 40
	var accepted atomic.Int64
	var budgetRejected atomic.Int64
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Go(func() {
			q := newTestQuery()
			q.Symbols = []string{strings.Repeat("A", i+1)}
			qh, err := q.Hash()
			if err != nil {
				t.Errorf("hash: %v", err)
				return
			}
			op, err := q.OperationID(RangeEndpoint)
			if err != nil {
				t.Errorf("operation id: %v", err)
				return
			}
			_, err = ledger.Reserve(op, qh, RangeEndpoint, 250_000_000, time.Now().UTC(), strings.Repeat("5", 64))
			if err == nil {
				accepted.Add(1)
				return
			}
			if strings.Contains(err.Error(), "remaining authorization") {
				budgetRejected.Add(1)
				return
			}
			t.Errorf("unexpected reserve error: %v", err)
		})
	}
	wg.Wait()
	// Each hold is $0.275; 36 total $9.90 and the 37th exceeds the $10 cap.
	if got := accepted.Load(); got != 36 {
		t.Fatalf("accepted %d want 36", got)
	}
	if got := budgetRejected.Load(); got != attempts-36 {
		t.Fatalf("budget rejected %d want %d", got, attempts-36)
	}
	if got := ledger.TotalReserved(); got != 36*275_000_000 {
		t.Fatalf("total %d want %d", got, 36*275_000_000)
	}
	if got := ledger.Remaining(); got != MaxLifetimeNano-36*275_000_000 {
		t.Fatalf("remaining %d unexpected", got)
	}
}

func TestLedgerUnknownPersistsAcrossReopenAllowsDistinct(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ledger.json")
	ledger, err := OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	q := newTestQuery()
	qh, err := q.Hash()
	if err != nil {
		ledger.Close()
		t.Fatal(err)
	}
	op, err := q.OperationID(RangeEndpoint)
	if err != nil {
		ledger.Close()
		t.Fatal(err)
	}
	respHash := strings.Repeat("4", 64)
	if _, err := ledger.Reserve(op, qh, RangeEndpoint, 250_000_000, time.Now().UTC(), respHash); err != nil {
		ledger.Close()
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	r, ok := reopened.Find(op)
	if !ok || r.Status != "reserved" {
		t.Fatalf("unknown hold not durable: %+v", r)
	}
	if got := reopened.TotalReserved(); got != 275_000_000 {
		t.Fatalf("total %d want 275000000", got)
	}
	if got := reopened.Remaining(); got != MaxLifetimeNano-275_000_000 {
		t.Fatalf("remaining %d unexpected", got)
	}
	// Old identity stays blocked even after reopen.
	if _, err := reopened.Reserve(op, qh, RangeEndpoint, 250_000_000, time.Now().UTC(), respHash); err == nil {
		t.Fatal("unknown operation retry allowed")
	}
	// A distinct valid reserve retains the old entry/status and correct totals.
	q2 := newTestQuery()
	q2.Symbols = []string{"MSFT.OPT"}
	qh2, err := q2.Hash()
	if err != nil {
		t.Fatal(err)
	}
	op2, err := q2.OperationID(RangeEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Reserve(op2, qh2, RangeEndpoint, 250_000_000, time.Now().UTC(), respHash); err != nil {
		t.Fatalf("distinct reserve blocked after unknown: %v", err)
	}
	r, ok = reopened.Find(op)
	if !ok || r.Status != "reserved" {
		t.Fatalf("old unknown mutated: %+v", r)
	}
	r2, ok := reopened.Find(op2)
	if !ok || r2.Status != "reserved" {
		t.Fatalf("distinct hold missing: %+v", r2)
	}
	if got := reopened.TotalReserved(); got != 550_000_000 {
		t.Fatalf("total %d want 550000000", got)
	}
	if got := reopened.Remaining(); got != MaxLifetimeNano-550_000_000 {
		t.Fatalf("remaining %d unexpected", got)
	}
}

func TestReserveAmountMargin(t *testing.T) {
	got, err := ReserveAmount(1_000_000_000)
	if err != nil || got != 1_100_000_000 {
		t.Fatalf("margin got %d err %v", got, err)
	}
	got, err = ReserveAmount(0)
	if err != nil || got != 0 {
		t.Fatalf("zero got %d err %v", got, err)
	}
	if _, err := ReserveAmount(-1); err == nil {
		t.Fatal("negative must fail")
	}
	if _, err := ReserveAmount(1<<63 - 1); err == nil {
		t.Fatal("overflow must fail")
	}
}

func TestLedgerRejectsDuplicateJSONFields(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ledger.json")
	for _, body := range []string{
		`{"version":1,"version":1,"lifetime_nano":10000000000,"reservations":[]}`,
		`{"version":1,"lifetime_nano":10000000000,"reservations":[{"status":"reserved","status":"complete"}]}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if l, err := OpenLedger(path); err == nil {
			l.Close()
			t.Fatal("duplicate JSON field accepted")
		}
	}
}

func TestLedgerCompletionSurvivesReopenAndClosedHandleCannotWrite(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ledger.json")
	ledger, err := OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	q := newTestQuery()
	qh, _ := q.Hash()
	op, _ := q.OperationID(RangeEndpoint)
	quoteHash, dataHash := strings.Repeat("4", 64), strings.Repeat("6", 64)
	if _, err := ledger.Reserve(op, qh, RangeEndpoint, 250_000_000, time.Now().UTC(), quoteHash); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Complete(op, dataHash, 128); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Reserve(op, qh, RangeEndpoint, 250_000_000, time.Now().UTC(), quoteHash); err == nil {
		t.Fatal("closed handle reserved")
	}
	if err := ledger.Complete(op, dataHash, 128); err == nil {
		t.Fatal("closed handle completed")
	}
	reopened, err := OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.TotalReserved(); got != 275_000_000 {
		t.Fatalf("reopened reservation %d", got)
	}
	r, ok := reopened.Find(op)
	if !ok || r.Status != "complete" || r.DataSHA256 != dataHash || r.Bytes != 128 {
		t.Fatalf("receipt not durable: %+v", r)
	}
	if _, err := reopened.Reserve(op, qh, RangeEndpoint, 250_000_000, time.Now().UTC(), quoteHash); err == nil {
		t.Fatal("completed operation granted a second send")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("rejected operation changed durable ledger")
	}
}
