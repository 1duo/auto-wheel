package databento

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newTestKey(t *testing.T) []byte {
	t.Helper()
	return []byte("db-" + strings.Repeat("A", 29))
}

func newTestQuery() Query {
	return Query{
		Dataset: "OPRA.PILLAR",
		Schema:  "cmbp-1",
		Symbols: []string{"NFLX.OPT"},
		StypeIn: "parent",
		Start:   "2026-04-06T13:30:00Z",
		End:     "2026-04-06T13:40:00Z",
		Limit:   100,
	}
}

func newTestLedger(t *testing.T) *Ledger {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ledger.json")
	l, err := OpenLedger(path)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func fakeJSONResponse(status int, v any) *http.Response {
	raw, _ := json.Marshal(v)
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(raw)),
		Header:     make(http.Header),
	}
}

func TestParseNanoDollars(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"0", 0, true},
		{"0.00", 0, true},
		{"1", 1_000_000_000, true},
		{"0.000000001", 1, true},
		{"0.0000000001", 1, true}, // rounds UP to 1 nano
		{"1.0000000001", 1_000_000_001, true},
		{"10", 10_000_000_000, true},
		{"", 0, false},
		{"-1", 0, false},
		{"abc", 0, false},
		{"1.2.3", 0, false},
		{".5", 0, false},
		{"1.", 0, false}, // trailing dot has empty frac? actually allowed? treat as invalid
		{"99999999999999999999", 0, false},
	}
	for i := range len(cases) {
		got, err := ParseNanoDollars(cases[i].in)
		if cases[i].ok && err != nil {
			t.Fatalf("case %q: unexpected error %v", cases[i].in, err)
		}
		if !cases[i].ok && err == nil {
			t.Fatalf("case %q: expected error", cases[i].in)
		}
		if cases[i].ok && got != cases[i].want {
			t.Fatalf("case %q: got %d want %d", cases[i].in, got, cases[i].want)
		}
	}
}

func TestQueryValidateCanonical(t *testing.T) {
	q := newTestQuery()
	if err := q.Validate(); err != nil {
		t.Fatalf("valid query rejected: %v", err)
	}
	h1, err := q.Hash()
	if err != nil || len(h1) != 64 {
		t.Fatalf("hash failed: %v", err)
	}
	mut := q
	mut.Symbols = []string{"MSFT"}
	h2, _ := mut.Hash()
	if h1 == h2 {
		t.Fatal("mutated query must change hash")
	}
	reordered := q
	reordered.Symbols = []string{"MSFT", "AAPL"}
	_ = reordered
	bad := q
	bad.Schema = "greeks"
	if err := bad.Validate(); err == nil {
		t.Fatal("unsupported schema must be rejected")
	}
}

func TestGetCostOffline(t *testing.T) {
	ctx := t.Context()
	key := newTestKey(t)
	defer ZeroKey(key)
	ledger := newTestLedger(t)
	q := newTestQuery()
	wantCost := "0.012345678"
	var gotHost, gotMethod, gotURL string
	var calls int
	var gotAuth bool
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		gotHost = r.URL.Host
		gotMethod = r.Method
		gotURL = r.URL.String()
		_, _, gotAuth = r.BasicAuth()
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("dataset") != q.Dataset || r.Form.Get("symbols") != strings.Join(q.Symbols, ",") || r.Form.Get("limit") != "100" {
			t.Fatal("form query mismatch")
		}
		return fakeJSONResponse(http.StatusOK, json.Number(wantCost)), nil
	})
	c, err := NewClientWithTransport(key, ledger, rt)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer c.Close()
	quote, err := c.GetCost(ctx, q)
	if err != nil {
		t.Fatalf("GetCost: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly one HTTP call, got %d", calls)
	}
	if gotHost != "hist.databento.com" {
		t.Fatalf("production host must be fixed, got %s", gotHost)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("get_cost must be POST, got %s", gotMethod)
	}
	if !strings.HasPrefix(gotURL, BaseURL+"/metadata.get_cost") {
		t.Fatalf("cost endpoint wrong: %s", gotURL)
	}
	if !gotAuth {
		t.Fatal("Basic auth missing")
	}
	wantNano, _ := ParseNanoDollars(wantCost)
	if quote.CostNano != wantNano {
		t.Fatalf("cost nano %d want %d", quote.CostNano, wantNano)
	}
	wantHash, _ := q.Hash()
	if quote.QueryHash != wantHash {
		t.Fatal("quote query hash mismatch")
	}
	wantOp, _ := q.OperationID(RangeEndpoint)
	if quote.OperationID != wantOp {
		t.Fatal("quote operation id mismatch")
	}
	if err := quote.Valid(time.Now().UTC()); err != nil {
		t.Fatalf("quote should be valid: %v", err)
	}
}

func TestGetCostRejectsBadQuote(t *testing.T) {
	ctx := t.Context()
	key := newTestKey(t)
	defer ZeroKey(key)
	for _, body := range []any{
		map[string]any{"cost": "not-a-number"},
		map[string]any{"nonsense": 1},
		map[string]any{"cost": "-1"},
	} {
		ledger := newTestLedger(t)
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return fakeJSONResponse(http.StatusOK, body), nil
		})
		c, _ := NewClientWithTransport(key, ledger, rt)
		if _, err := c.GetCost(ctx, newTestQuery()); err == nil {
			t.Fatalf("bad cost body %+v must be rejected", body)
		}
		c.Close()
	}
}

func TestGetCostNoRetryOnFailure(t *testing.T) {
	ctx := t.Context()
	key := newTestKey(t)
	defer ZeroKey(key)
	ledger := newTestLedger(t)
	calls := 0
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return fakeJSONResponse(http.StatusInternalServerError, map[string]any{"cost": "1"}), nil
	})
	c, _ := NewClientWithTransport(key, ledger, rt)
	defer c.Close()
	if _, err := c.GetCost(ctx, newTestQuery()); err == nil {
		t.Fatal("expected error on 500")
	}
	if calls != 1 {
		t.Fatalf("financial calls must not retry, got %d", calls)
	}
}

func TestListDatasetsOffline(t *testing.T) {
	ctx := t.Context()
	key := newTestKey(t)
	defer ZeroKey(key)
	ledger := newTestLedger(t)
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Errorf("listing must be GET")
		}
		if !strings.HasPrefix(r.URL.String(), BaseURL+"/metadata.list_datasets") {
			t.Errorf("listing endpoint wrong: %s", r.URL.String())
		}
		return fakeJSONResponse(http.StatusOK, []string{"OPRA.PILLAR"}), nil
	})
	c, _ := NewClientWithTransport(key, ledger, rt)
	defer c.Close()
	got, err := c.ListDatasets(ctx)
	if err != nil {
		t.Fatalf("ListDatasets: %v", err)
	}
	if len(got) != 1 || got[0] != "OPRA.PILLAR" {
		t.Fatalf("unexpected datasets: %v", got)
	}
}

// testAPI exposes the provider's observed wire shapes and separates free calls
// from the financial request counter. It never uses a network socket.
func testAPI(t *testing.T, financial roundTripFunc) roundTripFunc {
	t.Helper()
	return func(r *http.Request) (*http.Response, error) {
		switch strings.TrimPrefix(r.URL.Path, "/v0") {
		case CostEndpoint:
			return fakeJSONResponse(http.StatusOK, json.Number("0.001")), nil
		case "/metadata.list_unit_prices":
			return fakeJSONResponse(http.StatusOK, []any{map[string]any{"mode": "historical-streaming", "unit_prices": map[string]any{"definition": 5, "statistics": 11, "cmbp-1": 0.16}}}), nil
		case RangeEndpoint:
			return financial(r)
		default:
			t.Fatalf("unexpected endpoint %s", r.URL.Path)
			return nil, io.ErrUnexpectedEOF
		}
	}
}

func TestGetRangeRequiresFreshMatchingQuote(t *testing.T) {
	ledger := newTestLedger(t)
	calls := 0
	c, _ := NewClientWithTransport(newTestKey(t), ledger, testAPI(t, func(r *http.Request) (*http.Response, error) {
		calls++
		return fakeJSONResponse(200, map[string]int{"record": 1}), nil
	}))
	defer c.Close()
	q := newTestQuery()
	fresh, err := c.GetCost(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	forged := fresh
	forged.CostNano = 0
	stale := fresh
	stale.QuotedAt = time.Now().Add(-QuoteTTL - time.Minute)
	future := fresh
	future.QuotedAt = time.Now().Add(time.Second)
	for _, quote := range []Quote{{}, forged, stale, future} {
		if _, _, err := c.GetRange(t.Context(), q, quote, 1024); err == nil {
			t.Fatal("invalid or unissued quote accepted")
		}
	}
	mutated := q
	mutated.Symbols = []string{"MSFT.OPT"}
	if _, _, err := c.GetRange(t.Context(), mutated, fresh, 1024); err == nil {
		t.Fatal("mutated query accepted")
	}
	if calls != 0 || ledger.TotalReserved() != 0 {
		t.Fatal("invalid quote sent a financial request")
	}
	if _, _, err := c.GetRange(t.Context(), q, fresh, 1024); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || ledger.TotalReserved() != 1_100_000 {
		t.Fatal("qualified sample not conservatively reserved")
	}
}

func TestGetRangeReservesBeforeSendAndStaysCharged(t *testing.T) {
	ctx := t.Context()
	ledger := newTestLedger(t)
	q := newTestQuery()
	payload := []byte("{\"record\":1}\n")
	calls := 0
	transport := testAPI(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			if ledger.TotalReserved() != 1_100_000 {
				t.Error("financial send preceded durable reservation")
			}
			return nil, io.ErrUnexpectedEOF
		}
		// Distinct qualified sample proceeds only with both holds retained.
		if ledger.TotalReserved() != 2_200_000 {
			t.Errorf("distinct send saw total %d want 2200000", ledger.TotalReserved())
			return nil, io.ErrUnexpectedEOF
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(payload)), Header: make(http.Header)}, nil
	})
	c, _ := NewClientWithTransport(newTestKey(t), ledger, transport)
	quote, err := c.GetCost(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.GetRange(ctx, q, quote, 1024); err == nil {
		t.Fatal("expected failure")
	}
	if got := ledger.TotalReserved(); got != 1_100_000 {
		t.Fatalf("unknown hold %d want 1100000", got)
	}
	if _, _, err := c.GetRange(ctx, q, quote, 1024); err == nil {
		t.Fatal("duplicate accepted")
	}
	if calls != 1 {
		t.Fatalf("duplicate resent paid request, calls %d", calls)
	}
	c.Close()
	c2, _ := NewClientWithTransport(newTestKey(t), ledger, transport)
	defer c2.Close()
	again, err := c2.GetCost(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c2.GetRange(ctx, q, again, 1024); err == nil {
		t.Fatal("new client resent duplicate")
	}
	if calls != 1 {
		t.Fatalf("new client resent paid request, calls %d", calls)
	}
	// Unknown attempt keeps its full hold; a different cost-qualified sample
	// may still proceed within the remaining cumulative holds.
	q2 := newTestQuery()
	q2.Symbols = []string{"MSFT.OPT"}
	next, err := c2.GetCost(ctx, q2)
	if err != nil {
		t.Fatal(err)
	}
	receipt, body, err := c2.GetRange(ctx, q2, next, 1024)
	if err != nil {
		t.Fatalf("distinct qualified download blocked after unknown: %v", err)
	}
	if calls != 2 {
		t.Fatalf("distinct qualified download not sent, calls %d", calls)
	}
	if receipt.ReservedNano != 1_100_000 || ledger.TotalReserved() != 2_200_000 {
		t.Fatalf("holds not retained: receipt %d total %d", receipt.ReservedNano, ledger.TotalReserved())
	}
	if !bytes.Equal(body, payload) {
		t.Fatal("distinct body mismatch")
	}
	oldOp, _ := q.OperationID(RangeEndpoint)
	if oldEntry, ok := ledger.Find(oldOp); !ok || oldEntry.Status != "reserved" {
		t.Fatalf("unknown hold mutated: %+v", oldEntry)
	}
	if _, _, err := c2.GetRange(ctx, q2, next, 1024); err == nil {
		t.Fatal("distinct repeat accepted")
	}
	if calls != 2 || ledger.TotalReserved() != 2_200_000 {
		t.Fatal("repeat resent or reservation changed")
	}
}

func TestGetRangeReceiptHashesBoundedBody(t *testing.T) {
	ledger := newTestLedger(t)
	payload := []byte("{\"record\":1}\n")
	transport := testAPI(t, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Fatal("wrong content type")
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("encoding") != "json" || r.Form.Get("limit") != "100" {
			t.Fatal("wrong financial form")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(payload)), Header: make(http.Header)}, nil
	})
	c, _ := NewClientWithTransport(newTestKey(t), ledger, transport)
	defer c.Close()
	q := newTestQuery()
	quote, err := c.GetCost(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	receipt, body, err := c.GetRange(t.Context(), q, quote, 1024)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(payload)
	if !bytes.Equal(body, payload) || receipt.ResponseSHA256 != hex.EncodeToString(hash[:]) || receipt.CostNano != quote.CostNano {
		t.Fatal("receipt mismatch")
	}
	entry, ok := ledger.Find(receipt.OperationID)
	if !ok || entry.Status != "complete" || entry.DataSHA256 != receipt.ResponseSHA256 {
		t.Fatal("completion not durable")
	}
	if _, _, err := c.GetRange(t.Context(), q, quote, 1024); err == nil {
		t.Fatal("successful download repeated")
	}
	c2, _ := NewClientWithTransport(newTestKey(t), newTestLedger(t), transport)
	defer c2.Close()
	second, err := c2.GetCost(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c2.GetRange(t.Context(), q, second, 2); err == nil {
		t.Fatal("oversize body accepted")
	}
}

func TestGetRangeRejectsUnqualifiedScopeAndPrice(t *testing.T) {
	ledger := newTestLedger(t)
	calls := 0
	transport := testAPI(t, func(r *http.Request) (*http.Response, error) { calls++; return fakeJSONResponse(200, "data"), nil })
	c, _ := NewClientWithTransport(newTestKey(t), ledger, transport)
	defer c.Close()
	q := newTestQuery()
	q.Limit = 10001
	quote, err := c.GetCost(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.GetRange(t.Context(), q, quote, 1024); err == nil {
		t.Fatal("broad scope accepted")
	}
	c2, _ := NewClientWithTransport(newTestKey(t), ledger, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.TrimPrefix(r.URL.Path, "/v0") == "/metadata.list_unit_prices" {
			return fakeJSONResponse(200, []any{map[string]any{"mode": "historical-streaming", "unit_prices": map[string]any{"cmbp-1": 13}}}), nil
		}
		return transport(r)
	}))
	defer c2.Close()
	q = newTestQuery()
	quote, err = c2.GetCost(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c2.GetRange(t.Context(), q, quote, 1024); err == nil {
		t.Fatal("rate above qualified ceiling accepted")
	}
	if calls != 0 || ledger.TotalReserved() != 0 {
		t.Fatal("unqualified download consumed budget")
	}
}

func TestGetRangeAllowsSelectiveRawOSIBatch(t *testing.T) {
	ledger := newTestLedger(t)
	q := Query{
		Dataset: "OPRA.PILLAR",
		Schema:  "statistics",
		Symbols: []string{"NFLX  260410C00150000", "NFLX  260410P00150000"},
		StypeIn: "raw_symbol",
		Start:   "2026-04-06T13:30:00Z",
		End:     "2026-04-06T13:40:00Z",
		Limit:   100,
	}
	if err := q.Validate(); err != nil {
		t.Fatalf("selective query must validate: %v", err)
	}
	payload := []byte("{\"record\":1}\n")
	var rangeCalls int
	var gotSymbols, gotStype string
	transport := testAPI(t, func(r *http.Request) (*http.Response, error) {
		rangeCalls++
		if r.URL.Host != "hist.databento.com" {
			t.Fatal("production host must be fixed")
		}
		if r.Method != http.MethodPost {
			t.Fatal("range must be POST")
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		gotSymbols = r.Form.Get("symbols")
		gotStype = r.Form.Get("stype_in")
		if r.Form.Get("dataset") != "OPRA.PILLAR" || r.Form.Get("schema") != "statistics" {
			t.Fatal("wrong dataset or schema form")
		}
		if r.Form.Get("encoding") != "json" || r.Form.Get("compression") != "none" {
			t.Fatal("wrong financial encoding form")
		}
		if r.Form.Get("pretty_px") != "false" || r.Form.Get("pretty_ts") != "false" {
			t.Fatal("wrong financial pretty form")
		}
		if r.Form.Get("map_symbols") != "true" || r.Form.Get("stype_out") != "instrument_id" {
			t.Fatal("wrong financial symbol mapping form")
		}
		if r.Form.Get("limit") != "100" {
			t.Fatal("wrong limit form")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(payload)), Header: make(http.Header)}, nil
	})
	c, err := NewClientWithTransport(newTestKey(t), ledger, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	quote, err := c.GetCost(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	receipt, body, err := c.GetRange(t.Context(), q, quote, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if rangeCalls != 1 {
		t.Fatalf("selective batch must send once, got %d", rangeCalls)
	}
	if gotSymbols != strings.Join(q.Symbols, ",") || gotStype != "raw_symbol" {
		t.Fatalf("wrong selective form symbols=%q stype=%q", gotSymbols, gotStype)
	}
	if !bytes.Equal(body, payload) {
		t.Fatal("body mismatch")
	}
	if receipt.ReservedNano != 1_346_400 || ledger.TotalReserved() != 1_346_400 {
		t.Fatalf("selective hold must match conservative record-cap basis, got receipt %d total %d", receipt.ReservedNano, ledger.TotalReserved())
	}
	if _, _, err := c.GetRange(t.Context(), q, quote, 1024); err == nil {
		t.Fatal("selective repeat must be rejected")
	}
	if rangeCalls != 1 {
		t.Fatal("repeat resent paid request")
	}
}

func TestGetRangeRejectsNonSelectiveBatches(t *testing.T) {
	cases := []struct {
		name    string
		symbols []string
		stypeIn string
	}{
		{"mixed expiry", []string{"NFLX  260410C00150000", "NFLX  260411C00150000"}, "raw_symbol"},
		{"mixed root", []string{"NFLX  260410C00150000", "AAPL  260410C00150000"}, "raw_symbol"},
		{"malformed", []string{"NFLX  260410C00150000", "BAD"}, "raw_symbol"},
		{"invalid date", []string{"NFLX  260229C00150000", "NFLX  260229P00150000"}, "raw_symbol"},
		{"duplicate", []string{"NFLX  260410C00150000", "NFLX  260410C00150000"}, "raw_symbol"},
		{"parent list", []string{"NFLX  260410C00150000", "NFLX  260410P00150000"}, "parent"},
		{"instrument list", []string{"NFLX  260410C00150000", "NFLX  260410P00150000"}, "instrument_id"},
	}
	for _, tc := range cases {
		ledger := newTestLedger(t)
		var rangeCalls int
		transport := testAPI(t, func(r *http.Request) (*http.Response, error) {
			rangeCalls++
			return fakeJSONResponse(http.StatusOK, "data"), nil
		})
		c, err := NewClientWithTransport(newTestKey(t), ledger, transport)
		if err != nil {
			t.Fatal(err)
		}
		q := Query{
			Dataset: "OPRA.PILLAR",
			Schema:  "statistics",
			Symbols: tc.symbols,
			StypeIn: tc.stypeIn,
			Start:   "2026-04-06T13:30:00Z",
			End:     "2026-04-06T13:40:00Z",
			Limit:   100,
		}
		quote, err := c.GetCost(t.Context(), q)
		if err != nil {
			c.Close()
			t.Fatalf("%s: free quote must succeed: %v", tc.name, err)
		}
		if _, _, err := c.GetRange(t.Context(), q, quote, 1024); err == nil {
			c.Close()
			t.Fatalf("%s: non-selective batch accepted", tc.name)
		}
		c.Close()
		if rangeCalls != 0 {
			t.Fatalf("%s: non-selective batch sent %d paid requests", tc.name, rangeCalls)
		}
		if ledger.TotalReserved() != 0 {
			t.Fatalf("%s: non-selective batch reserved %d", tc.name, ledger.TotalReserved())
		}
	}
}

func TestLoadKeyPrivateFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	path := filepath.Join(dir, "pilot.key")
	raw := "db-" + strings.Repeat("B", 29) + "\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := LoadKey(path)
	if err != nil {
		t.Fatalf("LoadKey: %v", err)
	}
	defer ZeroKey(got)
	if string(got) != strings.TrimSpace(raw) {
		t.Fatal("key content mismatch")
	}
	// Symlink must be rejected.
	link := filepath.Join(dir, "link.key")
	if err := os.Symlink(path, link); err == nil {
		if _, err := LoadKey(link); err == nil {
			t.Fatal("symlink key must be rejected")
		}
	}
	// Wrong permissions must be rejected.
	if err := os.Chmod(path, 0644); err == nil {
		if _, err := LoadKey(path); err == nil {
			t.Fatal("0644 key must be rejected")
		}
	}
}

func TestConcurrentReserveSameIdentity(t *testing.T) {
	ledger := newTestLedger(t)
	q := newTestQuery()
	qh, _ := q.Hash()
	op, _ := q.OperationID(RangeEndpoint)
	respHash := strings.Repeat("3", 64)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range 8 {
		wg.Go(func() {
			_, errs[i] = ledger.Reserve(op, qh, RangeEndpoint, 100_000_000, time.Now().UTC(), respHash)
		})
	}
	wg.Wait()
	successes := 0
	for _, err := range errs {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("got %d successful reservations, want 1", successes)
	}
	// 100M cost + 10M margin = 110M exactly once.
	if got := ledger.TotalReserved(); got != 110_000_000 {
		t.Fatalf("total %d want 110000000", got)
	}
}
