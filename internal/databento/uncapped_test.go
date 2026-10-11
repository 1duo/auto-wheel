package databento

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Targeted controls for user-authorized removal of the cumulative $10 cap.

func TestUncappedDefaultRemainsCapped(t *testing.T) {
	ledger := newTestLedger(t)
	if ledger.IsUncapped() {
		t.Fatal("new ledger must remain capped")
	}
	if got := ledger.Remaining(); got != MaxLifetimeNano {
		t.Fatalf("new remaining %d want %d", got, MaxLifetimeNano)
	}
}

func TestUncappedMigrationPreservesAndReopens(t *testing.T) {
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
	qh, _ := q.Hash()
	op, _ := q.OperationID(RangeEndpoint)
	respHash := strings.Repeat("4", 64)
	first, err := ledger.Reserve(op, qh, RangeEndpoint, 250_000_000, time.Now().UTC(), respHash)
	if err != nil {
		ledger.Close()
		t.Fatal(err)
	}
	before := ledger.TotalReserved()
	if err := ledger.EnableUncapped(); err != nil {
		ledger.Close()
		t.Fatal(err)
	}
	if !ledger.IsUncapped() {
		ledger.Close()
		t.Fatal("migration must report uncapped")
	}
	if got := ledger.TotalReserved(); got != before {
		ledger.Close()
		t.Fatal("migration reset total")
	}
	kept, ok := ledger.Find(op)
	if !ok || kept != first {
		ledger.Close()
		t.Fatal("migration mutated reservation")
	}
	if kept.Status != "reserved" {
		ledger.Close()
		t.Fatal("migration changed unknown status")
	}
	// Idempotent second migration.
	if err := ledger.EnableUncapped(); err != nil {
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
	if !reopened.IsUncapped() {
		t.Fatal("uncapped did not persist across reopen")
	}
	if got := reopened.TotalReserved(); got != before {
		t.Fatalf("reopen total %d want %d", got, before)
	}
	kept, ok = reopened.Find(op)
	if !ok || kept != first {
		t.Fatal("reopen lost reservation")
	}
}

func TestUncappedBeyondTenOnlyAfterMigration(t *testing.T) {
	ledger := newTestLedger(t)
	// Fill near the $10 cap with capped policy: 36 holds of $0.275.
	for i := range 36 {
		q := newTestQuery()
		q.Symbols = []string{strings.Repeat("B", i+1) + ".OPT"}
		qh, err := q.Hash()
		if err != nil {
			t.Fatal(err)
		}
		op, err := q.OperationID(RangeEndpoint)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ledger.Reserve(op, qh, RangeEndpoint, 250_000_000, time.Now().UTC(), strings.Repeat("7", 64)); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	extra := newTestQuery()
	extra.Symbols = []string{"OVER.CAP"}
	extraQH, _ := extra.Hash()
	extraOP, _ := extra.OperationID(RangeEndpoint)
	if _, err := ledger.Reserve(extraOP, extraQH, RangeEndpoint, 250_000_000, time.Now().UTC(), strings.Repeat("7", 64)); err == nil {
		t.Fatal("capped ledger allowed cumulative beyond $10")
	}
	if err := ledger.EnableUncapped(); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Reserve(extraOP, extraQH, RangeEndpoint, 250_000_000, time.Now().UTC(), strings.Repeat("7", 64)); err != nil {
		t.Fatalf("uncapped must allow beyond $10: %v", err)
	}
	if ledger.IsUncapped() && ledger.Remaining() == MaxLifetimeNano-36*275_000_000 {
		t.Fatal("uncapped Remaining must not masquerade as capped budget")
	}
}

func TestUncappedOverflowStillRejected(t *testing.T) {
	ledger := newTestLedger(t)
	if err := ledger.EnableUncapped(); err != nil {
		t.Fatal(err)
	}
	q := newTestQuery()
	qh, _ := q.Hash()
	op, _ := q.OperationID(RangeEndpoint)
	if _, err := ledger.Reserve(op, qh, RangeEndpoint, 1<<63-1, time.Now().UTC(), strings.Repeat("8", 64)); err == nil {
		t.Fatal("individual MaxInt64 overflow accepted")
	}
	// A single reservation larger than $10 is allowed when uncapped, but an
	// overflow sum must still be rejected by validateLedgerState on reopen.
	if _, err := ReserveAmount(1<<63 - 1); err == nil {
		t.Fatal("ReserveAmount overflow accepted")
	}
	// Large individual holds are allowed; their cumulative sum still must fit.
	const large = int64(4_200_000_000_000_000_000)
	if _, err := ledger.Reserve(op, qh, RangeEndpoint, large, time.Now().UTC(), strings.Repeat("8", 64)); err != nil {
		t.Fatalf("uncapped large hold: %v", err)
	}
	if _, err := ledger.Reserve(strings.Repeat("a", 64), qh, RangeEndpoint, large, time.Now().UTC(), strings.Repeat("8", 64)); err == nil {
		t.Fatal("cumulative int64 overflow accepted")
	}
	if got := ledger.TotalReserved(); got != 4_620_000_000_000_000_000 {
		t.Fatalf("overflow rejection changed prior hold: %d", got)
	}
	bad := ledger.state
	second := bad.Reservations[0]
	second.OperationID = strings.Repeat("a", 64)
	bad.Reservations = append(append([]Reservation(nil), bad.Reservations...), second)
	if err := validateLedgerState(bad); err == nil {
		t.Fatal("on-disk cumulative overflow accepted")
	}
}

func TestUncappedMigrationRejectsClosedOrPoisoned(t *testing.T) {
	closed := newTestLedger(t)
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := closed.EnableUncapped(); err == nil {
		t.Fatal("closed migration accepted")
	}
	poisoned := newTestLedger(t)
	poisoned.failed = true
	if err := poisoned.EnableUncapped(); err == nil || poisoned.IsUncapped() {
		t.Fatal("poisoned migration accepted")
	}
}

func TestUncappedRejectsMalformedPolicy(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ledger.json")
	bodies := []string{
		`{"version":1,"lifetime_nano":0,"reservations":[]}`,
		`{"version":2,"lifetime_nano":10000000000,"reservations":[]}`,
		`{"version":2,"lifetime_nano":0,"authorization_mode":"","reservations":[]}`,
		`{"version":2,"lifetime_nano":0,"authorization_mode":"unlimited","reservations":[]}`,
		`{"version":1,"lifetime_nano":10000000000,"authorization_mode":"uncapped","reservations":[]}`,
		`{"version":3,"lifetime_nano":0,"authorization_mode":"uncapped","reservations":[]}`,
	}
	for i, body := range bodies {
		if err := os.WriteFile(path, []byte(body+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if l, err := OpenLedger(path); err == nil {
			l.Close()
			t.Fatalf("malformed policy %d accepted: %s", i, body)
		}
	}
}
