package databento

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func capacityReservations(n int) []Reservation {
	rows := make([]Reservation, n)
	for i := range n {
		rows[i] = Reservation{
			OperationID: fmt.Sprintf("%x", sha256.Sum256(fmt.Appendf(nil, "operation-%d", i))),
			QueryHash:   fmt.Sprintf("%x", sha256.Sum256(fmt.Appendf(nil, "query-%d", i))),
			Endpoint:    BatchSubmitEndpoint, CostNano: 1, ReservedNano: 2,
			At:           time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC),
			ResponseHash: strings.Repeat("7", 64), Status: "reserved",
		}
	}
	return rows
}

func TestBatchEvidenceCapacityRefusalNeverSubmits(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ledger.json")
	st := ledgerState{Version: uncappedLedgerVersion, AuthorizationMode: uncappedAuthorizationMode, Reservations: capacityReservations(4000)}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil || len(raw) >= maxLedgerFileBytes || len(raw)+128*len(st.Reservations) <= maxLedgerFileBytes {
		t.Fatal("fixture must be readable but lack completion-evidence headroom")
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ledger, err := OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	paidCalls := 0
	client, err := NewClientWithTransport(newTestKey(t), ledger, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.TrimPrefix(r.URL.Path, "/v0") == CostEndpoint {
			return fakeJSONResponse(http.StatusOK, json.Number("0.01")), nil
		}
		paidCalls++
		return nil, io.ErrUnexpectedEOF
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	query := newBatchFullMiniQuery()
	quote, err := client.GetBatchCost(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	ack, _, err := client.SubmitBatch(t.Context(), query, quote)
	if err == nil || err.Error() != "databento ledger evidence capacity exceeded" || ack.OperationID != "" || paidCalls != 0 {
		t.Fatal("storage refusal must occur before financial submission")
	}
	if _, found := ledger.Find(quote.OperationID); found {
		t.Fatal("storage refusal created a reservation")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("preflight refusal altered prior financial evidence")
	}
}

func TestLedgerMultiyearHistoryPreservedAndStorageBounded(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ledger.json")
	st := ledgerState{Version: uncappedLedgerVersion, AuthorizationMode: uncappedAuthorizationMode, Reservations: capacityReservations(3000)}
	if err := validateLedgerState(st); err != nil {
		t.Fatal(err)
	}
	if err := writeLedgerFile(path, st); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) <= 1<<20 {
		t.Fatalf("fixture must cross the former reader limit: bytes=%d error=%v", len(raw), err)
	}
	ledger, err := OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	first := st.Reservations[0]
	if _, err := ledger.Reserve(first.OperationID, first.QueryHash, first.Endpoint, first.CostNano, first.At, first.ResponseHash); err == nil {
		t.Fatal("duplicate operation must remain prohibited")
	}
	if _, err := ledger.Reserve(strings.Repeat("a", 64), strings.Repeat("b", 64), BatchSubmitEndpoint, 1, first.At, first.ResponseHash); err != nil {
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
	if !reopened.IsUncapped() || reopened.TotalReserved() != 6002 || len(reopened.state.Reservations) != 3001 || !slices.Equal(reopened.state.Reservations[:3000], st.Reservations) {
		t.Fatal("authorization, total, or original reservation history changed")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	oversized := ledgerState{Version: st.Version, AuthorizationMode: st.AuthorizationMode, Reservations: capacityReservations(10000)}
	if err := writeLedgerFile(path, oversized); err == nil {
		t.Fatal("oversized evidence must still be refused")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("storage rejection changed the existing ledger")
	}
}
