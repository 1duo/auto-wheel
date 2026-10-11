package databento

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestBatchHTTPFailureRetainsBodyAndHold(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusGatewayTimeout} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			ledger := newTestLedger(t)
			if err := ledger.EnableUncapped(); err != nil {
				t.Fatal(err)
			}
			payload := []byte(`{"error":"upstream unavailable"}`)
			calls := 0
			client, err := NewClientWithTransport(newTestKey(t), ledger, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch strings.TrimPrefix(r.URL.Path, "/v0") {
				case CostEndpoint:
					return fakeJSONResponse(http.StatusOK, json.Number("1")), nil
				case BatchSubmitEndpoint:
					calls++
					return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(payload)), Header: make(http.Header)}, nil
				default:
					t.Fatalf("unexpected endpoint %s", r.URL.Path)
					return nil, io.ErrUnexpectedEOF
				}
			}))
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			query := newBatchFullDefinitionQuery()
			quote, err := client.GetBatchCost(t.Context(), query)
			if err != nil {
				t.Fatal(err)
			}
			ack, body, err := client.SubmitBatch(t.Context(), query, quote)
			if err == nil || !bytes.Equal(body, payload) || ack.StatusCode != status || ack.Bytes != int64(len(payload)) || ack.ResponseHash != fmt.Sprintf("%x", sha256.Sum256(payload)) || ack.JobID != "" || ack.State != "" {
				t.Fatal("HTTP failure lost evidence or became a successful acknowledgement")
			}
			hold := ledger.TotalReserved()
			row, ok := ledger.Find(ack.OperationID)
			if !ok || row.Status != "reserved" || hold <= 0 {
				t.Fatal("HTTP failure lost its hold")
			}
			if _, _, err := client.SubmitBatch(t.Context(), query, quote); err == nil || calls != 1 || ledger.TotalReserved() != hold {
				t.Fatal("HTTP failure was retried or its hold changed")
			}
		})
	}
}

// Bulk batch tests are added but intentionally not run by the authoring step.
// The owner runs the narrow Linux check plus the actual purchase after review.

var testBatchFullRaw = []string{"NFLX", "TSLA", "NVDA", "NBIS", "IREN", "NOW", "AMD", "AMZN", "GOOGL", "META", "MSFT", "PLTR", "UBER", "AVGO", "ORCL"}

func newBatchFullMiniQuery() BatchQuery {
	return BatchQuery{
		Dataset: miniDataset,
		Schema:  miniSchema,
		Symbols: append([]string(nil), testBatchFullRaw...),
		StypeIn: "raw_symbol",
		Start:   BatchStart,
		End:     BatchEnd,
	}
}

func newBatchFullDefinitionQuery() BatchQuery {
	syms := make([]string, 0, len(testBatchFullRaw))
	for _, raw := range testBatchFullRaw {
		syms = append(syms, raw+".OPT")
	}
	return BatchQuery{
		Dataset: "OPRA.PILLAR",
		Schema:  "definition",
		Symbols: syms,
		StypeIn: "parent",
		Start:   BatchStart,
		End:     BatchEnd,
	}
}

func batchCostOK(cost any) roundTripFunc {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.TrimPrefix(r.URL.Path, "/v0") == CostEndpoint {
			return fakeJSONResponse(http.StatusOK, cost), nil
		}
		return fakeJSONResponse(http.StatusNotFound, 0), nil
	})
}

func batchJobBody(t *testing.T, q BatchQuery, jobID, state string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"id":             jobID,
		"state":          state,
		"dataset":        q.Dataset,
		"schema":         q.Schema,
		"symbols":        strings.Join(q.Symbols, ","),
		"stype_in":       q.StypeIn,
		"stype_out":      batchStypeOut,
		"start":          q.Start,
		"end":            q.End,
		"encoding":       batchEncoding,
		"compression":    batchCompression,
		"split_duration": batchSplitDuration,
		"limit":          nil,
		"cost_usd":       nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestBatchCappedRejectsSubmitWithoutPOST(t *testing.T) {
	ledger := newTestLedger(t)
	if ledger.IsUncapped() {
		t.Fatal("new ledger must be capped")
	}
	q := newBatchFullMiniQuery()
	var batchCalls int
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch strings.TrimPrefix(r.URL.Path, "/v0") {
		case CostEndpoint:
			if r.URL.Host != "hist.databento.com" || r.Method != http.MethodPost {
				t.Errorf("cost must be fixed POST host")
			}
			return fakeJSONResponse(http.StatusOK, json.Number("1.5")), nil
		case BatchSubmitEndpoint:
			batchCalls++
			return fakeJSONResponse(http.StatusOK, 0), nil
		default:
			t.Fatalf("unexpected endpoint %s", r.URL.Path)
			return nil, io.ErrUnexpectedEOF
		}
	})
	c, err := NewClientWithTransport(newTestKey(t), ledger, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	quote, err := c.GetBatchCost(t.Context(), q)
	if err != nil {
		t.Fatalf("free batch quote must succeed when capped: %v", err)
	}
	if quote.Endpoint != BatchSubmitEndpoint {
		t.Fatal("batch quote must bind batch endpoint")
	}
	if _, _, err := c.SubmitBatch(t.Context(), q, quote); err == nil {
		t.Fatal("capped batch submit accepted")
	}
	if batchCalls != 0 {
		t.Fatalf("capped submit sent %d paid POSTs", batchCalls)
	}
	if _, ok := ledger.Find(quote.OperationID); ok {
		t.Fatal("capped rejection must not reserve")
	}
}

func TestBatchIssuedQuoteMutationRejects(t *testing.T) {
	ledger := newTestLedger(t)
	if err := ledger.EnableUncapped(); err != nil {
		t.Fatal(err)
	}
	q := newBatchFullMiniQuery()
	var batchCalls int
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch strings.TrimPrefix(r.URL.Path, "/v0") {
		case CostEndpoint:
			return fakeJSONResponse(http.StatusOK, json.Number("0.5")), nil
		case BatchSubmitEndpoint:
			batchCalls++
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(batchJobBody(t, q, "job-ABC_123", "queued"))), Header: make(http.Header)}, nil
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
			return nil, io.ErrUnexpectedEOF
		}
	})
	c, err := NewClientWithTransport(newTestKey(t), ledger, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	quote, err := c.GetBatchCost(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	mut := quote
	mut.CostNano++
	if _, _, err := c.SubmitBatch(t.Context(), q, mut); err == nil || batchCalls != 0 {
		t.Fatal("mutated quote cost must reject without POST")
	}
	mutQ := q
	mutQ.Symbols = append([]string(nil), q.Symbols...)
	mutQ.Symbols[0] = "ORCL"
	if _, _, err := c.SubmitBatch(t.Context(), mutQ, quote); err == nil || batchCalls != 0 {
		t.Fatal("mutated scope must reject without POST")
	}
}

func TestBatchStaleQuoteRejects(t *testing.T) {
	ledger := newTestLedger(t)
	if err := ledger.EnableUncapped(); err != nil {
		t.Fatal(err)
	}
	q := newBatchFullMiniQuery()
	var batchCalls int
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.TrimPrefix(r.URL.Path, "/v0") == CostEndpoint {
			return fakeJSONResponse(http.StatusOK, json.Number("0.5")), nil
		}
		if strings.TrimPrefix(r.URL.Path, "/v0") == BatchSubmitEndpoint {
			batchCalls++
		}
		return fakeJSONResponse(http.StatusOK, 0), nil
	})
	c, err := NewClientWithTransport(newTestKey(t), ledger, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	quote, err := c.GetBatchCost(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	opID, _ := q.OperationID(BatchSubmitEndpoint)
	stale := quote
	stale.QuotedAt = time.Now().UTC().Add(-16 * time.Minute)
	c.mu.Lock()
	c.quotes[opID] = stale
	c.mu.Unlock()
	if _, _, err := c.SubmitBatch(t.Context(), q, stale); err == nil || batchCalls != 0 {
		t.Fatal("stale quote must reject without POST")
	}
	future := quote
	future.QuotedAt = time.Now().UTC().Add(5 * time.Minute)
	c.mu.Lock()
	c.quotes[opID] = future
	c.mu.Unlock()
	if _, _, err := c.SubmitBatch(t.Context(), q, future); err == nil || batchCalls != 0 {
		t.Fatal("future quote must reject without POST")
	}
}

func TestBatchDuplicateSubmitSendsOnceAfterSuccess(t *testing.T) {
	ledger := newTestLedger(t)
	if err := ledger.EnableUncapped(); err != nil {
		t.Fatal(err)
	}
	q := newBatchFullMiniQuery()
	var batchCalls int
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch strings.TrimPrefix(r.URL.Path, "/v0") {
		case CostEndpoint:
			return fakeJSONResponse(http.StatusOK, json.Number("2")), nil
		case BatchSubmitEndpoint:
			batchCalls++
			if r.URL.Host != "hist.databento.com" || r.Method != http.MethodPost {
				t.Errorf("batch must be fixed POST host")
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(batchJobBody(t, q, "job-dup1", "queued"))), Header: make(http.Header)}, nil
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
			return nil, io.ErrUnexpectedEOF
		}
	})
	c, err := NewClientWithTransport(newTestKey(t), ledger, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	quote, err := c.GetBatchCost(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	ack, raw, err := c.SubmitBatch(t.Context(), q, quote)
	if err != nil || ack.JobID != "job-dup1" || len(raw) == 0 {
		t.Fatalf("first submit: %v", err)
	}
	before := ledger.TotalReserved()
	if _, _, err := c.SubmitBatch(t.Context(), q, quote); err == nil {
		t.Fatal("duplicate must reject")
	}
	if batchCalls != 1 {
		t.Fatalf("duplicate sent second POST, calls=%d", batchCalls)
	}
	if got := ledger.TotalReserved(); got != before {
		t.Fatal("duplicate changed hold")
	}
	kept, ok := ledger.Find(ack.OperationID)
	if !ok || kept.Status != "reserved" {
		t.Fatal("ack must stay reserved")
	}
}

func TestBatchDuplicateSubmitSendsOnceAfterTimeout(t *testing.T) {
	ledger := newTestLedger(t)
	if err := ledger.EnableUncapped(); err != nil {
		t.Fatal(err)
	}
	q := newBatchFullDefinitionQuery()
	var batchCalls int
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch strings.TrimPrefix(r.URL.Path, "/v0") {
		case CostEndpoint:
			return fakeJSONResponse(http.StatusOK, json.Number("1")), nil
		case BatchSubmitEndpoint:
			batchCalls++
			return nil, io.ErrUnexpectedEOF
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
			return nil, io.ErrUnexpectedEOF
		}
	})
	c, err := NewClientWithTransport(newTestKey(t), ledger, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	quote, err := c.GetBatchCost(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.SubmitBatch(t.Context(), q, quote); err == nil {
		t.Fatal("timeout must error")
	}
	if batchCalls != 1 {
		t.Fatalf("timeout calls=%d", batchCalls)
	}
	before := ledger.TotalReserved()
	if before <= 0 {
		t.Fatal("timeout must retain hold")
	}
	if _, _, err := c.SubmitBatch(t.Context(), q, quote); err == nil {
		t.Fatal("retry after timeout must reject")
	}
	if batchCalls != 1 || ledger.TotalReserved() != before {
		t.Fatal("retry after timeout resent or changed hold")
	}
}

func TestBatchDuplicateSubmitSendsOnceAfterReopen(t *testing.T) {
	ledger := newTestLedger(t)
	if err := ledger.EnableUncapped(); err != nil {
		t.Fatal(err)
	}
	q := newBatchFullMiniQuery()
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch strings.TrimPrefix(r.URL.Path, "/v0") {
		case CostEndpoint:
			return fakeJSONResponse(http.StatusOK, json.Number("1")), nil
		case BatchSubmitEndpoint:
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(batchJobBody(t, q, "job-reopen1", "done"))), Header: make(http.Header)}, nil
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
			return nil, io.ErrUnexpectedEOF
		}
	})
	c, err := NewClientWithTransport(newTestKey(t), ledger, transport)
	if err != nil {
		t.Fatal(err)
	}
	quote, err := c.GetBatchCost(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	ack, _, err := c.SubmitBatch(t.Context(), q, quote)
	if err != nil {
		t.Fatal(err)
	}
	path := ledger.path
	c.Close()
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	kept, ok := reopened.Find(ack.OperationID)
	if !ok || kept.Status != "reserved" {
		t.Fatal("reopen lost reserved batch hold")
	}
	if _, err := reopened.Reserve(ack.OperationID, ack.QueryHash, BatchSubmitEndpoint, ack.CostNano, time.Now().UTC(), quote.ResponseHash); err == nil {
		t.Fatal("reopen duplicate reserve accepted")
	}
}

func TestBatchOldHoldsSurviveBatchReserve(t *testing.T) {
	ledger := newTestLedger(t)
	var olds []Reservation
	for i := range 8 {
		rq := newTestQuery()
		rq.Symbols = []string{strings.Repeat("Q", i+1) + ".OPT"}
		qh, _ := rq.Hash()
		op, _ := rq.OperationID(RangeEndpoint)
		r, err := ledger.Reserve(op, qh, RangeEndpoint, 250_000_000, time.Now().UTC(), strings.Repeat("3", 64))
		if err != nil {
			t.Fatal(err)
		}
		olds = append(olds, r)
	}
	before := ledger.TotalReserved()
	if err := ledger.EnableUncapped(); err != nil {
		t.Fatal(err)
	}
	q := newBatchFullMiniQuery()
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch strings.TrimPrefix(r.URL.Path, "/v0") {
		case CostEndpoint:
			return fakeJSONResponse(http.StatusOK, json.Number("3")), nil
		case BatchSubmitEndpoint:
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(batchJobBody(t, q, "job-olds1", "processing"))), Header: make(http.Header)}, nil
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
			return nil, io.ErrUnexpectedEOF
		}
	})
	c, err := NewClientWithTransport(newTestKey(t), ledger, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	quote, err := c.GetBatchCost(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	ack, _, err := c.SubmitBatch(t.Context(), q, quote)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range olds {
		kept, ok := ledger.Find(want.OperationID)
		if !ok || kept != want {
			t.Fatal("old hold mutated by batch")
		}
	}
	wantTotal := before + ack.ReservedNano
	if got := ledger.TotalReserved(); got != wantTotal {
		t.Fatalf("total %d want %d", got, wantTotal)
	}
}

func TestBatchFullRangeNoLimitFixedFormatForm(t *testing.T) {
	ledger := newTestLedger(t)
	if err := ledger.EnableUncapped(); err != nil {
		t.Fatal(err)
	}
	q := newBatchFullDefinitionQuery()
	var gotCostLimit, gotSubmitLimit, gotSplitSize string
	var gotEncoding, gotCompression, gotStypeOut, gotSplitDuration, gotDelivery, gotMode string
	var gotSplitSymbols, gotPrettyPx, gotPrettyTs, gotMapSymbols string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch strings.TrimPrefix(r.URL.Path, "/v0") {
		case CostEndpoint:
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			gotCostLimit = r.Form.Get("limit")
			gotMode = r.Form.Get("mode")
			if r.Form.Get("dataset") != q.Dataset || r.Form.Get("symbols") != strings.Join(q.Symbols, ",") || r.Form.Get("start") != BatchStart || r.Form.Get("end") != BatchEnd {
				t.Fatal("cost scope mismatch")
			}
			return fakeJSONResponse(http.StatusOK, "4.25"), nil
		case BatchSubmitEndpoint:
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			gotSubmitLimit = r.Form.Get("limit")
			gotSplitSize = r.Form.Get("split_size")
			gotEncoding = r.Form.Get("encoding")
			gotCompression = r.Form.Get("compression")
			gotStypeOut = r.Form.Get("stype_out")
			gotSplitDuration = r.Form.Get("split_duration")
			gotSplitSymbols = r.Form.Get("split_symbols")
			gotPrettyPx = r.Form.Get("pretty_px")
			gotPrettyTs = r.Form.Get("pretty_ts")
			gotMapSymbols = r.Form.Get("map_symbols")
			gotDelivery = r.Form.Get("delivery")
			if r.Form.Get("symbols") != strings.Join(q.Symbols, ",") {
				t.Fatalf("submit symbols truncated: %q", r.Form.Get("symbols"))
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(batchJobBody(t, q, "job-form1", "queued"))), Header: make(http.Header)}, nil
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
			return nil, io.ErrUnexpectedEOF
		}
	})
	c, err := NewClientWithTransport(newTestKey(t), ledger, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	quote, err := c.GetBatchCost(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if quote.CostNano != 4_250_000_000 {
		t.Fatalf("cost %d", quote.CostNano)
	}
	if gotCostLimit != "" || gotMode != "historical" {
		t.Fatalf("cost form must omit limit and set mode, got limit=%q mode=%q", gotCostLimit, gotMode)
	}
	if _, _, err := c.SubmitBatch(t.Context(), q, quote); err != nil {
		t.Fatal(err)
	}
	if gotSubmitLimit != "" || gotSplitSize != "" {
		t.Fatal("submit must omit limit/split_size")
	}
	if gotEncoding != "dbn" || gotCompression != "zstd" || gotStypeOut != "instrument_id" || gotSplitDuration != "day" || gotDelivery != "download" {
		t.Fatalf("fixed format changed: %q %q %q %q %q", gotEncoding, gotCompression, gotStypeOut, gotSplitDuration, gotDelivery)
	}
	if gotSplitSymbols != "false" || gotPrettyPx != "false" || gotPrettyTs != "false" || gotMapSymbols != "false" {
		t.Fatal("fixed flags must be false")
	}
}

func TestBatchMalformedJobKeepsReserved(t *testing.T) {
	bodies := []struct {
		name string
		make func(q BatchQuery) []byte
	}{
		{"empty", func(BatchQuery) []byte { return []byte{} }},
		{"trailing", func(q BatchQuery) []byte { return append(batchJobBody(t, q, "job-x1", "queued"), '\n', '{', '}') }},
		{"duplicate", func(q BatchQuery) []byte {
			raw := batchJobBody(t, q, "job-x1", "queued")
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			dup, _ := json.Marshal(map[string]any{"id": "job-x1", "state": "queued"})
			_ = dup
			return []byte(`{"id":"job-x1","id":"job-x1","state":"queued","dataset":` + `"` + q.Dataset + `"}`)
		}},
		{"bad-job", func(q BatchQuery) []byte { return bytes.Clone(batchJobBody(t, q, "bad job!", "queued")) }},
		{"bad-state", func(q BatchQuery) []byte { return bytes.Clone(batchJobBody(t, q, "job-x1", "flying")) }},
		{"limit-present", func(q BatchQuery) []byte {
			raw := batchJobBody(t, q, "job-x1", "queued")
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			m["limit"] = 100
			out, _ := json.Marshal(m)
			return out
		}},
	}
	for _, tc := range bodies {
		t.Run(tc.name, func(t *testing.T) {
			ledger := newTestLedger(t)
			if err := ledger.EnableUncapped(); err != nil {
				t.Fatal(err)
			}
			q := newBatchFullMiniQuery()
			body := tc.make(q)
			var calls int
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch strings.TrimPrefix(r.URL.Path, "/v0") {
				case CostEndpoint:
					return fakeJSONResponse(http.StatusOK, json.Number("1")), nil
				case BatchSubmitEndpoint:
					calls++
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
				default:
					t.Fatalf("unexpected %s", r.URL.Path)
					return nil, io.ErrUnexpectedEOF
				}
			})
			c, err := NewClientWithTransport(newTestKey(t), ledger, transport)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			quote, err := c.GetBatchCost(t.Context(), q)
			if err != nil {
				t.Fatal(err)
			}
			ack, _, err := c.SubmitBatch(t.Context(), q, quote)
			if err == nil {
				t.Fatal("malformed ack accepted")
			}
			if ack.JobID != "" {
				t.Fatal("malformed must not set job")
			}
			kept, ok := ledger.Find(ack.OperationID)
			if !ok || kept.Status != "reserved" {
				t.Fatal("malformed must keep reserved")
			}
			if _, _, err := c.SubmitBatch(t.Context(), q, quote); err == nil || calls != 1 {
				t.Fatal("malformed retry must not resend")
			}
		})
	}
}

func TestBatchScopeMismatchKeepsReserved(t *testing.T) {
	ledger := newTestLedger(t)
	if err := ledger.EnableUncapped(); err != nil {
		t.Fatal(err)
	}
	q := newBatchFullMiniQuery()
	mismatch := batchJobBody(t, q, "job-scope1", "queued")
	var m map[string]any
	if err := json.Unmarshal(mismatch, &m); err != nil {
		t.Fatal(err)
	}
	m["symbols"] = "NFLX,TSLA"
	bad, _ := json.Marshal(m)
	var calls int
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch strings.TrimPrefix(r.URL.Path, "/v0") {
		case CostEndpoint:
			return fakeJSONResponse(http.StatusOK, json.Number("1")), nil
		case BatchSubmitEndpoint:
			calls++
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(bad)), Header: make(http.Header)}, nil
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
			return nil, io.ErrUnexpectedEOF
		}
	})
	c, err := NewClientWithTransport(newTestKey(t), ledger, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	quote, err := c.GetBatchCost(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	ack, _, err := c.SubmitBatch(t.Context(), q, quote)
	if err == nil {
		t.Fatal("scope mismatch accepted")
	}
	kept, ok := ledger.Find(ack.OperationID)
	if !ok || kept.Status != "reserved" {
		t.Fatal("scope mismatch must keep reserved")
	}
	if _, _, err := c.SubmitBatch(t.Context(), q, quote); err == nil || calls != 1 {
		t.Fatal("scope retry must not resend")
	}
}

func TestBatchAckDoesNotComplete(t *testing.T) {
	ledger := newTestLedger(t)
	if err := ledger.EnableUncapped(); err != nil {
		t.Fatal(err)
	}
	q := newBatchFullMiniQuery()
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch strings.TrimPrefix(r.URL.Path, "/v0") {
		case CostEndpoint:
			return fakeJSONResponse(http.StatusOK, json.Number("1")), nil
		case BatchSubmitEndpoint:
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(batchJobBody(t, q, "job-nocomplete", "done"))), Header: make(http.Header)}, nil
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
			return nil, io.ErrUnexpectedEOF
		}
	})
	c, err := NewClientWithTransport(newTestKey(t), ledger, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	quote, err := c.GetBatchCost(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	ack, _, err := c.SubmitBatch(t.Context(), q, quote)
	if err != nil {
		t.Fatal(err)
	}
	kept, ok := ledger.Find(ack.OperationID)
	if !ok || kept.Status != "reserved" {
		t.Fatal("ack must not complete")
	}
	if err := ledger.Complete(ack.OperationID, strings.Repeat("9", 64), 1); err == nil {
		t.Fatal("batch Complete must be rejected")
	}
	kept, _ = ledger.Find(ack.OperationID)
	if kept.Status != "reserved" {
		t.Fatal("Complete changed batch status")
	}
	// A manually altered journal cannot promote a job acknowledgement into
	// downloaded market-data evidence on reopen either.
	forged := ledger.state
	row := kept
	row.Status, row.DataSHA256, row.Bytes = "complete", strings.Repeat("9", 64), 1
	forged.Reservations = []Reservation{row}
	if err := validateLedgerState(forged); err == nil {
		t.Fatal("forged completed batch journal accepted")
	}
}

func TestBatchLegacyCanonicalPinned(t *testing.T) {
	mini := newBatchFullMiniQuery()
	def := newBatchFullDefinitionQuery()
	stats := newBatchFullDefinitionQuery()
	stats.Schema = "statistics"
	cases := []struct {
		name string
		q    BatchQuery
		hash string
		op   string
	}{
		{"mini full", mini, "9c604cd5cfe9964412a2c476932c7f2bdfdc729da9583b55b7a46a34bd53997f", "49c41dc18e73955b3ec122ae210e34b37d9a4fccf25fdd33e6d8727bc6ba67f7"},
		{"definition full", def, "246cc3970301838109fb4158b404faf2ad292e407f6b41ecbc9ede1719e918d4", "f855f7ac29734d2d617342d79c47687d19b66241fffb84daeb34f4aef2fb85fb"},
		{"statistics full", stats, "a664df16c797aa7091f38282829da833242363ee98002c42f3166a9b7ece8571", "01d81f5fc68b636d5d3a0aa64a933167bd8e52dad52bac461cede83f2ddc6401"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.q.Validate(); err != nil {
				t.Fatalf("legacy %s rejected: %v", tc.name, err)
			}
			h, err := tc.q.Hash()
			if err != nil || h != tc.hash {
				t.Fatalf("legacy %s hash %q want %q err %v", tc.name, h, tc.hash, err)
			}
			op, err := tc.q.OperationID(BatchSubmitEndpoint)
			if err != nil || op != tc.op {
				t.Fatalf("legacy %s op %q want %q err %v", tc.name, op, tc.op, err)
			}
		})
	}
}

func TestBatchValidBoundedSlicesAndRaw(t *testing.T) {
	sliceStart := "2026-04-06T13:30:00Z"
	sliceEnd := "2026-04-06T13:40:00Z"
	maximumSymbols := make([]string, 2000)
	for i := range maximumSymbols {
		maximumSymbols[i] = fmt.Sprintf("NFLX  260410C%08d", i+1)
	}
	cases := []struct {
		name string
		q    BatchQuery
	}{
		{"raw maximum day and symbol count", BatchQuery{Dataset: "OPRA.PILLAR", Schema: "statistics", Symbols: maximumSymbols, StypeIn: "raw_symbol", Start: "2026-04-06T00:00:00Z", End: "2026-04-07T00:00:00Z"}},
		{"raw mixture definition", BatchQuery{Dataset: "OPRA.PILLAR", Schema: "definition", Symbols: []string{"NFLX  260410C00150000", "NFLX  260410P00150000", "TSLA  260410C00200000"}, StypeIn: "raw_symbol", Start: sliceStart, End: sliceEnd}},
		{"raw cmbp-1", BatchQuery{Dataset: "OPRA.PILLAR", Schema: "cmbp-1", Symbols: []string{"NVDA  260410C00150000", "NVDA  260410P00150000"}, StypeIn: "raw_symbol", Start: sliceStart, End: sliceEnd}},
		{"raw statistics", BatchQuery{Dataset: "OPRA.PILLAR", Schema: "statistics", Symbols: []string{"MSFT  260410C00150000"}, StypeIn: "raw_symbol", Start: sliceStart, End: sliceEnd}},
		{"sliced mini", BatchQuery{Dataset: miniDataset, Schema: miniSchema, Symbols: []string{"NFLX", "TSLA"}, StypeIn: "raw_symbol", Start: sliceStart, End: sliceEnd}},
		{"sliced parent definition", BatchQuery{Dataset: "OPRA.PILLAR", Schema: "definition", Symbols: []string{"NFLX.OPT", "TSLA.OPT"}, StypeIn: "parent", Start: sliceStart, End: sliceEnd}},
		{"sliced parent statistics", BatchQuery{Dataset: "OPRA.PILLAR", Schema: "statistics", Symbols: []string{"NVDA.OPT"}, StypeIn: "parent", Start: sliceStart, End: sliceEnd}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.q.Validate(); err != nil {
				t.Fatalf("%s rejected: %v", tc.name, err)
			}
		})
	}
}

func TestBatchRejectsBoundedResearch(t *testing.T) {
	sliceStart := "2026-04-06T13:30:00Z"
	sliceEnd := "2026-04-06T13:40:00Z"
	raw2001 := make([]string, 0, 2001)
	for i := range 2001 {
		raw2001 = append(raw2001, fmt.Sprintf("NFLX  260410C%08d", i+1))
	}
	broadParent := make([]string, 0, 16)
	for _, raw := range testBatchFullRaw {
		broadParent = append(broadParent, raw+".OPT")
	}
	broadParent = append(broadParent, "NFLX.OPT")
	cases := []struct {
		name string
		q    BatchQuery
	}{
		{"raw over 2000", BatchQuery{Dataset: "OPRA.PILLAR", Schema: "definition", Symbols: raw2001, StypeIn: "raw_symbol", Start: sliceStart, End: sliceEnd}},
		{"raw invalid root", BatchQuery{Dataset: "OPRA.PILLAR", Schema: "definition", Symbols: []string{"AAPL  260410C00150000"}, StypeIn: "raw_symbol", Start: sliceStart, End: sliceEnd}},
		{"raw invalid date", BatchQuery{Dataset: "OPRA.PILLAR", Schema: "definition", Symbols: []string{"NFLX  260229C00150000"}, StypeIn: "raw_symbol", Start: sliceStart, End: sliceEnd}},
		{"raw bad padding", BatchQuery{Dataset: "OPRA.PILLAR", Schema: "definition", Symbols: []string{" NFLX 260410C00150000"}, StypeIn: "raw_symbol", Start: sliceStart, End: sliceEnd}},
		{"raw zero strike", BatchQuery{Dataset: "OPRA.PILLAR", Schema: "definition", Symbols: []string{"NFLX  260410C00000000"}, StypeIn: "raw_symbol", Start: sliceStart, End: sliceEnd}},
		{"raw duplicates", BatchQuery{Dataset: "OPRA.PILLAR", Schema: "definition", Symbols: []string{"NFLX  260410C00150000", "NFLX  260410C00150000"}, StypeIn: "raw_symbol", Start: sliceStart, End: sliceEnd}},
		{"raw full six months", BatchQuery{Dataset: "OPRA.PILLAR", Schema: "definition", Symbols: []string{"NFLX  260410C00150000"}, StypeIn: "raw_symbol", Start: BatchStart, End: BatchEnd}},
		{"mini slice over 24h", BatchQuery{Dataset: miniDataset, Schema: miniSchema, Symbols: []string{"NFLX"}, StypeIn: "raw_symbol", Start: "2026-04-06T00:00:00Z", End: "2026-04-07T01:00:00Z"}},
		{"parent slice over 24h", BatchQuery{Dataset: "OPRA.PILLAR", Schema: "definition", Symbols: []string{"NFLX.OPT"}, StypeIn: "parent", Start: "2026-04-06T00:00:00Z", End: "2026-04-07T01:00:00Z"}},
		{"raw slice over 24h", BatchQuery{Dataset: "OPRA.PILLAR", Schema: "cmbp-1", Symbols: []string{"NFLX  260410C00150000"}, StypeIn: "raw_symbol", Start: "2026-04-06T00:00:00Z", End: "2026-04-07T01:00:00Z"}},
		{"outside before", BatchQuery{Dataset: miniDataset, Schema: miniSchema, Symbols: []string{"NFLX"}, StypeIn: "raw_symbol", Start: "2026-04-05T00:00:00Z", End: "2026-04-05T01:00:00Z"}},
		{"outside after", BatchQuery{Dataset: miniDataset, Schema: miniSchema, Symbols: []string{"NFLX"}, StypeIn: "raw_symbol", Start: "2026-10-03T00:00:00Z", End: "2026-10-04T00:00:00Z"}},
		{"offset equivalent", BatchQuery{Dataset: miniDataset, Schema: miniSchema, Symbols: []string{"NFLX"}, StypeIn: "raw_symbol", Start: "2026-04-06T00:00:00+00:00", End: "2026-04-06T01:00:00+00:00"}},
		{"noncanonical zeros", BatchQuery{Dataset: miniDataset, Schema: miniSchema, Symbols: []string{"NFLX"}, StypeIn: "raw_symbol", Start: "2026-04-06T13:30:00.000Z", End: sliceEnd}},
		{"broad parent", BatchQuery{Dataset: "OPRA.PILLAR", Schema: "definition", Symbols: broadParent, StypeIn: "parent", Start: BatchStart, End: BatchEnd}},
		{"cmbp-1 parent", BatchQuery{Dataset: "OPRA.PILLAR", Schema: "cmbp-1", Symbols: []string{"NFLX.OPT"}, StypeIn: "parent", Start: BatchStart, End: BatchEnd}},
		{"other schema trades", BatchQuery{Dataset: "OPRA.PILLAR", Schema: "trades", Symbols: []string{"NFLX.OPT"}, StypeIn: "parent", Start: BatchStart, End: BatchEnd}},
		{"mini wrong schema", BatchQuery{Dataset: miniDataset, Schema: "definition", Symbols: []string{"NFLX"}, StypeIn: "raw_symbol", Start: BatchStart, End: BatchEnd}},
		{"raw wrong schema", BatchQuery{Dataset: "OPRA.PILLAR", Schema: "trades", Symbols: []string{"NFLX  260410C00150000"}, StypeIn: "raw_symbol", Start: sliceStart, End: sliceEnd}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.q.Validate(); err == nil {
				t.Fatalf("%s accepted", tc.name)
			}
		})
	}
}
