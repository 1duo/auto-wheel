package databento

import (
	"bytes"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func newMiniQuery(symbols []string, limit int) Query {
	return Query{
		Dataset: "EQUS.MINI",
		Schema:  "mbp-1",
		Symbols: symbols,
		StypeIn: "raw_symbol",
		Start:   "2026-04-06T13:30:00Z",
		End:     "2026-04-06T13:40:00Z",
		Limit:   limit,
	}
}

var testMiniFullUniverse = []string{"NFLX", "TSLA", "NVDA", "NBIS", "IREN", "NOW", "AMD", "AMZN", "GOOGL", "META", "MSFT", "PLTR", "UBER", "AVGO", "ORCL"}

func TestMiniOneAndFifteenSuccess(t *testing.T) {
	cases := []struct {
		name    string
		symbols []string
		limit   int
		hold    int64
	}{
		{"single", []string{"NFLX"}, 1, 2244},
		{"fifteen", testMiniFullUniverse, 100, 224400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger := newTestLedger(t)
			payload := []byte("{\"record\":1}\n")
			var unitCalls, rangeCalls int
			var gotUnitDataset string
			var gotDataset, gotSchema, gotSymbols, gotStype, gotLimit string
			var gotEncoding, gotCompression, gotPrettyPx, gotPrettyTs, gotMapSymbols, gotStypeOut string
			var gotHost, gotMethod string
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch strings.TrimPrefix(r.URL.Path, "/v0") {
				case CostEndpoint:
					return fakeJSONResponse(http.StatusOK, 0), nil
				case "/metadata.list_unit_prices":
					unitCalls++
					gotUnitDataset = r.URL.Query().Get("dataset")
					if r.Method != http.MethodGet {
						t.Errorf("unit price must be GET, got %s", r.Method)
					}
					if r.URL.Host != "hist.databento.com" {
						t.Errorf("production host must be fixed, got %s", r.URL.Host)
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`[{"mode":"historical-streaming","unit_prices":{"mbp-1":"1.2"}}]`)), Header: make(http.Header)}, nil
				case RangeEndpoint:
					rangeCalls++
					gotHost = r.URL.Host
					gotMethod = r.Method
					if err := r.ParseForm(); err != nil {
						t.Fatal(err)
					}
					gotDataset = r.Form.Get("dataset")
					gotSchema = r.Form.Get("schema")
					gotSymbols = r.Form.Get("symbols")
					gotStype = r.Form.Get("stype_in")
					gotLimit = r.Form.Get("limit")
					gotEncoding = r.Form.Get("encoding")
					gotCompression = r.Form.Get("compression")
					gotPrettyPx = r.Form.Get("pretty_px")
					gotPrettyTs = r.Form.Get("pretty_ts")
					gotMapSymbols = r.Form.Get("map_symbols")
					gotStypeOut = r.Form.Get("stype_out")
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(payload)), Header: make(http.Header)}, nil
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
			q := newMiniQuery(tc.symbols, tc.limit)
			quote, err := c.GetCost(t.Context(), q)
			if err != nil {
				t.Fatalf("GetCost: %v", err)
			}
			if quote.CostNano != 0 {
				t.Fatalf("zero quote cost %d", quote.CostNano)
			}
			receipt, body, err := c.GetRange(t.Context(), q, quote, 4096)
			if err != nil {
				t.Fatalf("GetRange: %v", err)
			}
			if !bytes.Equal(body, payload) {
				t.Fatal("body mismatch")
			}
			if receipt.ReservedNano != tc.hold {
				t.Fatalf("receipt hold %d want %d", receipt.ReservedNano, tc.hold)
			}
			if got := ledger.TotalReserved(); got != tc.hold {
				t.Fatalf("ledger total %d want %d", got, tc.hold)
			}
			if unitCalls != 1 || rangeCalls != 1 {
				t.Fatalf("unit %d range %d want 1 1", unitCalls, rangeCalls)
			}
			if gotUnitDataset != "EQUS.MINI" {
				t.Fatalf("unit price dataset %q want EQUS.MINI", gotUnitDataset)
			}
			if gotHost != "hist.databento.com" || gotMethod != http.MethodPost {
				t.Fatalf("range host/method %s %s", gotHost, gotMethod)
			}
			if gotDataset != "EQUS.MINI" || gotSchema != "mbp-1" {
				t.Fatalf("range dataset/schema %q %q", gotDataset, gotSchema)
			}
			if gotSymbols != strings.Join(tc.symbols, ",") {
				t.Fatalf("range symbols %q", gotSymbols)
			}
			if gotStype != "raw_symbol" {
				t.Fatalf("range stype %q", gotStype)
			}
			if gotLimit != strconv.Itoa(tc.limit) {
				t.Fatalf("range limit %q", gotLimit)
			}
			if gotEncoding != "json" || gotCompression != "none" || gotPrettyPx != "false" || gotPrettyTs != "false" || gotMapSymbols != "true" || gotStypeOut != "instrument_id" {
				t.Fatalf("pinned output params changed: %q %q %q %q %q %q", gotEncoding, gotCompression, gotPrettyPx, gotPrettyTs, gotMapSymbols, gotStypeOut)
			}
			if _, _, err := c.GetRange(t.Context(), q, quote, 4096); err == nil {
				t.Fatal("repeat must be rejected")
			}
			if rangeCalls != 1 || ledger.TotalReserved() != tc.hold {
				t.Fatal("repeat resent or changed reservation")
			}
		})
	}
}

func TestMiniCeilingGate(t *testing.T) {
	for _, tt := range []struct {
		name string
		wire string
		hold int64
	}{
		{"low observed does not shrink hold", `[{"mode":"historical-streaming","unit_prices":{"mbp-1":"1.2"}}]`, 2244},
		{"at ceiling same hold", `[{"mode":"historical-streaming","unit_prices":{"mbp-1":"2"}}]`, 2244},
		{"above ceiling", `[{"mode":"historical-streaming","unit_prices":{"mbp-1":"2.000000001"}}]`, 0},
		{"fractional nano rounds up above", `[{"mode":"historical-streaming","unit_prices":{"mbp-1":"2.0000000001"}}]`, 0},
		{"duplicate streaming rows", `[{"mode":"historical-streaming","unit_prices":{"mbp-1":"1.2"}},{"mode":"historical-streaming","unit_prices":{"mbp-1":"1.2"}}]`, 0},
		{"missing rate", `[{"mode":"historical-streaming","unit_prices":{}}]`, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ledger := newTestLedger(t)
			var unitCalls, rangeCalls int
			var gotUnitDataset string
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch strings.TrimPrefix(r.URL.Path, "/v0") {
				case CostEndpoint:
					return fakeJSONResponse(http.StatusOK, 0), nil
				case "/metadata.list_unit_prices":
					unitCalls++
					gotUnitDataset = r.URL.Query().Get("dataset")
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(tt.wire)), Header: make(http.Header)}, nil
				case RangeEndpoint:
					rangeCalls++
					if ledger.TotalReserved() != tt.hold {
						t.Fatal("financial request preceded exact fixed-ceiling reservation")
					}
					return fakeJSONResponse(http.StatusOK, map[string]int{"record": 1}), nil
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
			q := newMiniQuery([]string{"NFLX"}, 1)
			quote, err := c.GetCost(t.Context(), q)
			if err != nil {
				t.Fatal(err)
			}
			receipt, _, err := c.GetRange(t.Context(), q, quote, 4096)
			if tt.hold == 0 {
				if err == nil || rangeCalls != 0 || ledger.TotalReserved() != 0 {
					t.Fatal("unqualified price consumed money or sent a financial request")
				}
				if unitCalls != 1 {
					t.Fatalf("scope must reach the unit-price gate, unit calls %d", unitCalls)
				}
				if gotUnitDataset != "EQUS.MINI" {
					t.Fatalf("unit price dataset %q want EQUS.MINI", gotUnitDataset)
				}
			} else {
				if err != nil || rangeCalls != 1 || receipt.ReservedNano != tt.hold {
					t.Fatalf("fixed reservation got %d calls %d err %v", receipt.ReservedNano, rangeCalls, err)
				}
				if unitCalls != 1 || ledger.TotalReserved() != tt.hold {
					t.Fatalf("unit %d total %d", unitCalls, ledger.TotalReserved())
				}
				if gotUnitDataset != "EQUS.MINI" {
					t.Fatalf("unit price dataset %q want EQUS.MINI", gotUnitDataset)
				}
			}
		})
	}
}

func TestMiniRejectsUnqualifiedScope(t *testing.T) {
	full15 := testMiniFullUniverse
	sixteen := append(slices.Clone(full15), "AAPL")
	cases := []struct {
		name  string
		query Query
	}{
		{"opra plus mbp-1 cross", Query{Dataset: "OPRA.PILLAR", Schema: "mbp-1", Symbols: []string{"NFLX"}, StypeIn: "raw_symbol", Start: "2026-04-06T13:30:00Z", End: "2026-04-06T13:40:00Z", Limit: 1}},
		{"mini plus definition cross", Query{Dataset: "EQUS.MINI", Schema: "definition", Symbols: []string{"NFLX"}, StypeIn: "raw_symbol", Start: "2026-04-06T13:30:00Z", End: "2026-04-06T13:40:00Z", Limit: 1}},
		{"mini plus statistics cross", Query{Dataset: "EQUS.MINI", Schema: "statistics", Symbols: []string{"NFLX"}, StypeIn: "raw_symbol", Start: "2026-04-06T13:30:00Z", End: "2026-04-06T13:40:00Z", Limit: 1}},
		{"mini plus cmbp-1 cross", Query{Dataset: "EQUS.MINI", Schema: "cmbp-1", Symbols: []string{"NFLX"}, StypeIn: "raw_symbol", Start: "2026-04-06T13:30:00Z", End: "2026-04-06T13:40:00Z", Limit: 1}},
		{"other dataset plus mbp-1", Query{Dataset: "XNAS.MINI", Schema: "mbp-1", Symbols: []string{"NFLX"}, StypeIn: "raw_symbol", Start: "2026-04-06T13:30:00Z", End: "2026-04-06T13:40:00Z", Limit: 1}},
		{"non-whitelist stock", Query{Dataset: "EQUS.MINI", Schema: "mbp-1", Symbols: []string{"AAPL"}, StypeIn: "raw_symbol", Start: "2026-04-06T13:30:00Z", End: "2026-04-06T13:40:00Z", Limit: 1}},
		{"lowercase stock", Query{Dataset: "EQUS.MINI", Schema: "mbp-1", Symbols: []string{"nflx"}, StypeIn: "raw_symbol", Start: "2026-04-06T13:30:00Z", End: "2026-04-06T13:40:00Z", Limit: 1}},
		{"duplicate stock", Query{Dataset: "EQUS.MINI", Schema: "mbp-1", Symbols: []string{"NFLX", "NFLX"}, StypeIn: "raw_symbol", Start: "2026-04-06T13:30:00Z", End: "2026-04-06T13:40:00Z", Limit: 1}},
		{"sixteen symbols", Query{Dataset: "EQUS.MINI", Schema: "mbp-1", Symbols: sixteen, StypeIn: "raw_symbol", Start: "2026-04-06T13:30:00Z", End: "2026-04-06T13:40:00Z", Limit: 1}},
		{"parent stype", Query{Dataset: "EQUS.MINI", Schema: "mbp-1", Symbols: []string{"NFLX", "TSLA"}, StypeIn: "parent", Start: "2026-04-06T13:30:00Z", End: "2026-04-06T13:40:00Z", Limit: 1}},
		{"instrument id stype", Query{Dataset: "EQUS.MINI", Schema: "mbp-1", Symbols: []string{"NFLX"}, StypeIn: "instrument_id", Start: "2026-04-06T13:30:00Z", End: "2026-04-06T13:40:00Z", Limit: 1}},
		{"limit above record cap", Query{Dataset: "EQUS.MINI", Schema: "mbp-1", Symbols: []string{"NFLX"}, StypeIn: "raw_symbol", Start: "2026-04-06T13:30:00Z", End: "2026-04-06T13:40:00Z", Limit: 10001}},
		{"window above 24h", Query{Dataset: "EQUS.MINI", Schema: "mbp-1", Symbols: []string{"NFLX"}, StypeIn: "raw_symbol", Start: "2026-04-06T13:30:00Z", End: "2026-04-07T13:31:00Z", Limit: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger := newTestLedger(t)
			var unitCalls, rangeCalls int
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch strings.TrimPrefix(r.URL.Path, "/v0") {
				case CostEndpoint:
					return fakeJSONResponse(http.StatusOK, 0), nil
				case "/metadata.list_unit_prices":
					unitCalls++
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`[{"mode":"historical-streaming","unit_prices":{"mbp-1":"1.2"}}]`)), Header: make(http.Header)}, nil
				case RangeEndpoint:
					rangeCalls++
					return fakeJSONResponse(http.StatusOK, "data"), nil
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
			quote, err := c.GetCost(t.Context(), tc.query)
			if err != nil {
				t.Fatalf("free quote must succeed: %v", err)
			}
			if _, _, err := c.GetRange(t.Context(), tc.query, quote, 4096); err == nil {
				t.Fatalf("%s accepted", tc.name)
			}
			if unitCalls != 0 {
				t.Fatalf("%s reached unit-price gate, calls %d", tc.name, unitCalls)
			}
			if rangeCalls != 0 {
				t.Fatalf("%s sent a financial request, calls %d", tc.name, rangeCalls)
			}
			if ledger.TotalReserved() != 0 {
				t.Fatalf("%s reserved %d", tc.name, ledger.TotalReserved())
			}
		})
	}
}

func TestMiniPreservesOpra(t *testing.T) {
	ledger := newTestLedger(t)
	payload := []byte("{\"record\":1}\n")
	var unitCalls, rangeCalls int
	var gotUnitDataset string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch strings.TrimPrefix(r.URL.Path, "/v0") {
		case CostEndpoint:
			return fakeJSONResponse(http.StatusOK, 0), nil
		case "/metadata.list_unit_prices":
			unitCalls++
			gotUnitDataset = r.URL.Query().Get("dataset")
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`[{"mode":"historical-streaming","unit_prices":{"definition":5,"statistics":11,"cmbp-1":0.16}}]`)), Header: make(http.Header)}, nil
		case RangeEndpoint:
			rangeCalls++
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("dataset") != "OPRA.PILLAR" || r.Form.Get("schema") != "cmbp-1" {
				t.Fatal("wrong OPRA dataset or schema form")
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(payload)), Header: make(http.Header)}, nil
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
	q := newTestQuery()
	q.Limit = 1
	quote, err := c.GetCost(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	receipt, body, err := c.GetRange(t.Context(), q, quote, 4096)
	if err != nil {
		t.Fatalf("legacy OPRA download blocked: %v", err)
	}
	if rangeCalls != 1 || unitCalls != 1 {
		t.Fatalf("unit %d range %d want 1 1", unitCalls, rangeCalls)
	}
	if gotUnitDataset != "OPRA.PILLAR" {
		t.Fatalf("unit price dataset %q want OPRA.PILLAR", gotUnitDataset)
	}
	if receipt.ReservedNano != 281 {
		t.Fatalf("legacy hold %d want 281", receipt.ReservedNano)
	}
	if ledger.TotalReserved() != 281 {
		t.Fatalf("ledger total %d want 281", ledger.TotalReserved())
	}
	if !bytes.Equal(body, payload) {
		t.Fatal("body mismatch")
	}
}

func TestMiniNoRetryAndUnknownHold(t *testing.T) {
	ledger := newTestLedger(t)
	payload := []byte("{\"record\":1}\n")
	var rangeCalls int
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch strings.TrimPrefix(r.URL.Path, "/v0") {
		case CostEndpoint:
			return fakeJSONResponse(http.StatusOK, 0), nil
		case "/metadata.list_unit_prices":
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`[{"mode":"historical-streaming","unit_prices":{"mbp-1":"1.2"}}]`)), Header: make(http.Header)}, nil
		case RangeEndpoint:
			rangeCalls++
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			syms := r.Form.Get("symbols")
			if syms == "TSLA" {
				return fakeJSONResponse(http.StatusGatewayTimeout, map[string]int{"error": 1}), nil
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(payload)), Header: make(http.Header)}, nil
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
	q1 := newMiniQuery([]string{"NFLX"}, 1)
	quote1, err := c.GetCost(t.Context(), q1)
	if err != nil {
		t.Fatal(err)
	}
	receipt1, _, err := c.GetRange(t.Context(), q1, quote1, 4096)
	if err != nil {
		t.Fatalf("first MINI download: %v", err)
	}
	if receipt1.ReservedNano != 2244 || ledger.TotalReserved() != 2244 {
		t.Fatalf("first hold receipt %d total %d", receipt1.ReservedNano, ledger.TotalReserved())
	}
	if rangeCalls != 1 {
		t.Fatalf("range calls %d want 1", rangeCalls)
	}
	if _, _, err := c.GetRange(t.Context(), q1, quote1, 4096); err == nil {
		t.Fatal("duplicate MINI accepted")
	}
	if rangeCalls != 1 || ledger.TotalReserved() != 2244 {
		t.Fatal("duplicate resent or changed reservation")
	}
	c2, err := NewClientWithTransport(newTestKey(t), ledger, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	again, err := c2.GetCost(t.Context(), q1)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c2.GetRange(t.Context(), q1, again, 4096); err == nil {
		t.Fatal("new client resent duplicate MINI")
	}
	if rangeCalls != 1 || ledger.TotalReserved() != 2244 {
		t.Fatal("cross-client duplicate resent or changed reservation")
	}
	q2 := newMiniQuery([]string{"TSLA"}, 1)
	quote2, err := c2.GetCost(t.Context(), q2)
	if err != nil {
		t.Fatal(err)
	}
	receipt2, _, err := c2.GetRange(t.Context(), q2, quote2, 4096)
	if err == nil {
		t.Fatal("expected 504 failure")
	}
	if receipt2.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("unknown status %d", receipt2.StatusCode)
	}
	if receipt2.ReservedNano != 2244 || ledger.TotalReserved() != 4488 {
		t.Fatalf("unknown hold receipt %d total %d", receipt2.ReservedNano, ledger.TotalReserved())
	}
	if rangeCalls != 2 {
		t.Fatalf("range calls %d want 2", rangeCalls)
	}
	if _, _, err := c2.GetRange(t.Context(), q2, quote2, 4096); err == nil {
		t.Fatal("unknown duplicate accepted")
	}
	if rangeCalls != 2 || ledger.TotalReserved() != 4488 {
		t.Fatal("unknown repeat resent or changed reservation")
	}
}
