package databento

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPilotReservationBasis(t *testing.T) {
	for _, tt := range []struct {
		schema string
		limit  int
		quote  int64
		basis  int64
		hold   int64
	}{
		{"statistics", 1, 0, 12_240, 13_464},
		{"statistics", 100, 0, 1_224_000, 1_346_400},
		{"statistics", 10_000, 0, 122_400_000, 134_640_000},
		{"definition", 1, 0, 6_120, 6_732},
		{"definition", 100, 0, 612_000, 673_200},
		{"definition", 10_000, 0, 61_200_000, 67_320_000},
		{"cmbp-1", 1, 0, 255, 281},
		{"cmbp-1", 100, 0, 25_500, 28_050},
		{"cmbp-1", 10_000, 0, 2_550_000, 2_805_000},
		{"cmbp-1", 1, 1_000_001, 1_000_001, 1_100_002},
	} {
		basis, err := pilotReservationBasis(tt.schema, tt.limit, tt.quote)
		if err != nil || basis != tt.basis {
			t.Fatalf("limit %d quote %d: basis=%d err=%v", tt.limit, tt.quote, basis, err)
		}
		hold, err := ReserveAmount(basis)
		if err != nil || hold != tt.hold {
			t.Fatalf("limit %d: hold=%d err=%v", tt.limit, hold, err)
		}
	}
	for _, limit := range []int{-1, 0, 10_001, int(^uint(0) >> 1)} {
		if _, err := pilotReservationBasis("cmbp-1", limit, 0); err == nil {
			t.Fatalf("invalid record cap %d accepted", limit)
		}
	}
	if _, err := pilotReservationBasis("cmbp-1", 1, -1); err == nil {
		t.Fatal("negative quote accepted")
	}
	if _, err := pilotReservationBasis("cbbo-1s", 1, 0); err == nil {
		t.Fatal("unsupported schema accepted")
	}
	basis, err := pilotReservationBasis("cmbp-1", 1, 1<<63-1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReserveAmount(basis); err == nil {
		t.Fatal("overflowing margin accepted")
	}
}

func TestGetRangeZeroCostPreservesLegacyHolds(t *testing.T) {
	ledger := newTestLedger(t)
	var original []Reservation
	for i := range 6 {
		q := newTestQuery()
		q.Symbols = []string{fmt.Sprintf("LEGACY%d.OPT", i)}
		op, _ := q.OperationID(RangeEndpoint)
		qh, _ := q.Hash()
		r, err := ledger.Reserve(op, qh, RangeEndpoint, 250_000_000, time.Now().UTC(), strings.Repeat("1", 64))
		if err != nil {
			t.Fatal(err)
		}
		original = append(original, r)
	}
	const wantTotal = int64(1_650_000_000 + 281)
	calls := 0
	transport := testAPI(t, func(*http.Request) (*http.Response, error) {
		calls++
		if ledger.TotalReserved() != wantTotal {
			t.Fatal("request sent before retaining legacy and new full holds")
		}
		return fakeJSONResponse(http.StatusOK, map[string]int{"record": 1}), nil
	})
	// The exact query estimate may be zero; the positive hard record bound stays.
	zeroCostTransport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.TrimPrefix(r.URL.Path, "/v0") == CostEndpoint {
			return fakeJSONResponse(http.StatusOK, 0), nil
		}
		return transport(r)
	})
	client, err := NewClientWithTransport(newTestKey(t), ledger, zeroCostTransport)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	q := newTestQuery()
	q.Limit = 1
	quote, err := client.GetCost(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	receipt, _, err := client.GetRange(t.Context(), q, quote, 1024)
	if err != nil || receipt.ReservedNano != 281 || calls != 1 {
		t.Fatalf("zero estimate reservation: receipt=%d calls=%d err=%v", receipt.ReservedNano, calls, err)
	}
	if _, _, err := client.GetRange(t.Context(), q, quote, 1024); err == nil || calls != 1 {
		t.Fatal("small reservation enabled repeat request")
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLedger(ledger.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.TotalReserved() != wantTotal {
		t.Fatal("mixed reservation total changed on reopen")
	}
	for _, prior := range original {
		if actual, ok := reopened.Find(prior.OperationID); !ok || actual != prior {
			t.Fatal("legacy amount, identity or unresolved status changed")
		}
	}
}

func TestGetRangeSchemaPriceCeilings(t *testing.T) {
	for _, tt := range []struct {
		name, schema, wire string
		hold               int64 // Zero means reject before reserve/send.
	}{
		{"definition ceiling", "definition", `[{"mode":"historical-streaming","unit_prices":{"definition":"6"}}]`, 6_732},
		{"statistics ceiling", "statistics", `[{"mode":"historical-streaming","unit_prices":{"statistics":"12"}}]`, 13_464},
		{"quote ceiling", "cmbp-1", `[{"mode":"historical-streaming","unit_prices":{"cmbp-1":"0.25"}}]`, 281},
		{"lower observed rate does not shrink hold", "cmbp-1", `[{"mode":"historical-streaming","unit_prices":{"cmbp-1":"0.01"}}]`, 281},
		{"definition above", "definition", `[{"mode":"historical-streaming","unit_prices":{"definition":"6.000000001"}}]`, 0},
		{"statistics above", "statistics", `[{"mode":"historical-streaming","unit_prices":{"statistics":"12.000000001"}}]`, 0},
		{"quote above", "cmbp-1", `[{"mode":"historical-streaming","unit_prices":{"cmbp-1":"0.250000001"}}]`, 0},
		{"fractional nano rounds up", "cmbp-1", `[{"mode":"historical-streaming","unit_prices":{"cmbp-1":"0.2500000001"}}]`, 0},
		{"wrong mode", "cmbp-1", `[{"mode":"historical","unit_prices":{"cmbp-1":"0.16"}}]`, 0},
		{"wrong schema", "cmbp-1", `[{"mode":"historical-streaming","unit_prices":{"statistics":"11"}}]`, 0},
		{"missing rate", "cmbp-1", `[{"mode":"historical-streaming","unit_prices":{}}]`, 0},
		{"duplicate mode rows", "cmbp-1", `[{"mode":"historical-streaming","unit_prices":{"cmbp-1":"0.16"}},{"mode":"historical-streaming","unit_prices":{"cmbp-1":"13"}}]`, 0},
		{"equal duplicate mode rows", "cmbp-1", `[{"mode":"historical-streaming","unit_prices":{"cmbp-1":"0.16"}},{"mode":"historical-streaming","unit_prices":{"cmbp-1":"0.16"}}]`, 0},
		{"duplicate rate keys", "cmbp-1", `[{"mode":"historical-streaming","unit_prices":{"cmbp-1":"13","cmbp-1":"0.16"}}]`, 0},
		{"duplicate mode keys", "cmbp-1", `[{"mode":"live","mode":"historical-streaming","unit_prices":{"cmbp-1":"0.16"}}]`, 0},
		{"case aliases", "cmbp-1", `[{"mode":"historical-streaming","Mode":"live","unit_prices":{"cmbp-1":"0.16"}}]`, 0},
		{"malformed rate", "cmbp-1", `[{"mode":"historical-streaming","unit_prices":{"cmbp-1":"NaN"}}]`, 0},
		{"negative rate", "cmbp-1", `[{"mode":"historical-streaming","unit_prices":{"cmbp-1":"-0.1"}}]`, 0},
		{"no rows", "cmbp-1", `[]`, 0},
		{"trailing JSON", "cmbp-1", `[{"mode":"historical-streaming","unit_prices":{"cmbp-1":"0.16"}}] []`, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ledger := newTestLedger(t)
			calls := 0
			transport := testAPI(t, func(*http.Request) (*http.Response, error) {
				calls++
				if ledger.TotalReserved() != tt.hold {
					t.Fatal("financial request preceded exact fixed-ceiling reservation")
				}
				return fakeJSONResponse(http.StatusOK, map[string]int{"record": 1}), nil
			})
			client, err := NewClientWithTransport(newTestKey(t), ledger, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch strings.TrimPrefix(r.URL.Path, "/v0") {
				case CostEndpoint:
					return fakeJSONResponse(http.StatusOK, 0), nil
				case "/metadata.list_unit_prices":
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(tt.wire)), Header: make(http.Header)}, nil
				default:
					return transport(r)
				}
			}))
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			q := newTestQuery()
			q.Schema, q.Limit = tt.schema, 1
			quote, err := client.GetCost(t.Context(), q)
			if err != nil {
				t.Fatal(err)
			}
			receipt, _, err := client.GetRange(t.Context(), q, quote, 1024)
			if tt.hold == 0 {
				if err == nil || calls != 0 || ledger.TotalReserved() != 0 {
					t.Fatal("ambiguous/unqualified price consumed money or sent a financial request")
				}
			} else if err != nil || calls != 1 || receipt.ReservedNano != tt.hold {
				t.Fatalf("fixed reservation got %d calls %d err %v", receipt.ReservedNano, calls, err)
			}
		})
	}
}

// Smaller future holds permit more evidence entries. Capacity must be checked
// before HTTP, and every pending entry must retain room for a completed receipt.
func TestGetRangeLedgerCapacityStopsBeforeSend(t *testing.T) {
	ledger := newTestLedger(t)
	rows := make([]Reservation, 3000)
	for i := range rows {
		rows[i] = Reservation{
			OperationID: fmt.Sprintf("%064x", i+1), QueryHash: strings.Repeat("2", 64),
			Endpoint: RangeEndpoint, CostNano: 12_240, ReservedNano: 13_464,
			At: time.Now().UTC(), ResponseHash: strings.Repeat("3", 64), Status: "reserved",
		}
	}
	makeState := func(n int) ledgerState {
		return ledgerState{Version: ledgerVersion, LifetimeNano: MaxLifetimeNano, Reservations: rows[:n]}
	}
	lo, hi := 0, len(rows)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		raw, err := json.MarshalIndent(makeState(mid), "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if len(raw)+1+128*mid <= maxLedgerFileBytes {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	if lo == 0 || lo == len(rows) {
		t.Fatal("capacity fixture did not straddle the evidence bound")
	}
	ledger.state = makeState(lo)
	if err := validateLedgerState(ledger.state); err != nil {
		t.Fatal(err)
	}
	if err := writeLedgerFile(ledger.path, ledger.state); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(ledger.path)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	client, err := NewClientWithTransport(newTestKey(t), ledger, testAPI(t, func(*http.Request) (*http.Response, error) {
		calls++
		return fakeJSONResponse(http.StatusOK, map[string]int{"record": 1}), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	q := newTestQuery()
	q.Limit = 1
	quote, err := client.GetCost(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.GetRange(t.Context(), q, quote, 1024); err == nil || calls != 0 {
		t.Fatal("full evidence ledger allowed financial send")
	}
	after, err := os.ReadFile(ledger.path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("capacity failure replaced the durable ledger")
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLedger(ledger.path)
	if err != nil {
		t.Fatal("capacity failure made ledger unreadable", err)
	}
	defer reopened.Close()
	// Exercise the longest valid positive int64 byte count, requiring no new
	// authorization or amount change; this is a synthetic receipt only.
	if err := reopened.Complete(rows[0].OperationID, strings.Repeat("4", 64), 1<<63-1); err != nil {
		t.Fatal("reserved completion headroom unavailable", err)
	}
}
