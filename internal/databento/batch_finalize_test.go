package databento

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBatchAcquiredLedgerStates(t *testing.T) {
	base := Reservation{OperationID: strings.Repeat("1", 64), QueryHash: strings.Repeat("2", 64), ResponseHash: strings.Repeat("3", 64), Endpoint: BatchSubmitEndpoint, CostNano: 100, ReservedNano: 110, At: time.Now().UTC(), Status: "acquired", DataSHA256: strings.Repeat("4", 64), Bytes: 123}
	for _, tc := range []struct {
		name   string
		change func(*Reservation, *ledgerState)
		valid  bool
	}{
		{"uncapped_acquired", func(*Reservation, *ledgerState) {}, true},
		{"capped_batch", func(_ *Reservation, s *ledgerState) {
			s.Version = 1
			s.LifetimeNano = MaxLifetimeNano
			s.AuthorizationMode = ""
		}, false},
		{"range_acquired", func(r *Reservation, _ *ledgerState) { r.Endpoint = RangeEndpoint }, false},
		{"ACK_complete", func(r *Reservation, _ *ledgerState) { r.Status = "complete" }, false},
		{"pending_with_data", func(r *Reservation, _ *ledgerState) { r.Status = "reserved" }, false},
		{"missing_proof", func(r *Reservation, _ *ledgerState) { r.DataSHA256 = "" }, false},
		{"zero_bytes", func(r *Reservation, _ *ledgerState) { r.Bytes = 0 }, false},
		{"changed_margin", func(r *Reservation, _ *ledgerState) { r.ReservedNano-- }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			s := ledgerState{Version: 2, LifetimeNano: 0, AuthorizationMode: "uncapped"}
			tc.change(&r, &s)
			s.Reservations = []Reservation{r}
			if got := validateLedgerState(s) == nil; got != tc.valid {
				t.Fatalf("structural validity=%v want %v", got, tc.valid)
			}
		})
	}
}

func TestBatchFinalizeRejectsEarlyEvidence(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	l, err := OpenLedger(filepath.Join(dir, "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.EnableUncapped(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(l.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.FinalizeBatchAcquisition(context.Background(), BatchAcquisitionInputs{}); err == nil {
		t.Fatal("deadline missing accepted")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if _, err := l.FinalizeBatchAcquisition(ctx, BatchAcquisitionInputs{}); err == nil {
		t.Fatal("missing originals accepted")
	}
	after, err := os.ReadFile(l.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("failed verification changed journal")
	}
}

func TestBatchPrivateEvidenceRejectsSymlinkAndDuplicate(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "input.json")
	if err := os.WriteFile(p, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked.json")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if _, err := batchEvidence(link, 1024); err == nil {
		t.Fatal("symlink accepted")
	}
	if _, err := batchEvidence(p, 1); err == nil {
		t.Fatal("oversized input accepted")
	}
	var row struct {
		Value int `json:"value"`
	}
	if err := strictBatchJSON([]byte(`{"value":1,"value":2}`), &row); err == nil {
		t.Fatal("duplicate field accepted")
	}
	if err := strictBatchJSON([]byte(`{"value":1} {}`), &row); err == nil {
		t.Fatal("trailing body accepted")
	}
}
