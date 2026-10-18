package robinhood

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

func parallelMCPFixture(t *testing.T, onCall func(object) (any, bool), observe ...func(*http.Request)) (*Client, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token.json")
	if err := aruntime.AtomicJSON(path, Credential{Resource: Endpoint, ClientID: "fixture", AccessToken: "test-access", RefreshToken: "test-refresh", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, inspect := range observe {
			inspect(r)
		}
		var body object
		d := json.NewDecoder(r.Body)
		d.UseNumber()
		if err := d.Decode(&body); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch body["method"] {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "parallel-fixture")
			result = object{"protocolVersion": protocol}
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			result = object{"tools": []object{
				{"name": "get_accounts", "inputSchema": object{"type": "object", "properties": object{"symbol": object{"type": "string"}}}},
				{"name": "place_option_order", "inputSchema": object{"type": "object", "properties": object{}}},
			}}
		case "tools/call":
			if r.Header.Get("Mcp-Session-Id") != "parallel-fixture" {
				t.Error("RPC session changed while reads were active")
			}
			value, bad := onCall(body["params"].(map[string]any))
			result = object{"structuredContent": value, "isError": bad}
		default:
			t.Error("unexpected RPC method")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		raw, _ := json.Marshal(object{"jsonrpc": "2.0", "id": body["id"], "result": result})
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", raw)
	}))
	t.Cleanup(server.Close)
	c := NewClient(path, true)
	c.endpoint = server.URL
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	return c, path
}

func TestMCPParallelReadsKeepRequestIdentityAndWriteFence(t *testing.T) {
	entered := make(chan string, 3)
	release := make(chan struct{})
	var active atomic.Int32
	var writes atomic.Int32
	client, _ := parallelMCPFixture(t, func(params object) (any, bool) {
		if params["name"] == "place_option_order" {
			writes.Add(1)
			return object{"accepted": true}, false
		}
		symbol := params["arguments"].(map[string]any)["symbol"].(string)
		active.Add(1)
		entered <- symbol
		select {
		case <-release:
		case <-t.Context().Done():
			return nil, true
		}
		active.Add(-1)
		return object{"symbol": symbol}, false
	})
	var workers sync.WaitGroup
	errs := make(chan error, 4)
	for _, symbol := range []string{"AMD", "INTC", "ORCL"} {
		workers.Go(func() {
			value, err := client.Call(t.Context(), "get_accounts", object{"symbol": symbol})
			if err == nil && value.(object)["symbol"] != symbol {
				err = errors.New("out-of-order SSE response used a different request's identity")
			}
			errs <- err
		})
	}
	for range 3 {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			close(release)
			workers.Wait()
			t.Fatal("shared client serialized HTTP reads")
		}
	}
	workers.Go(func() {
		_, err := client.CallGuarded(t.Context(), "place_option_order", object{}, func() error {
			if active.Load() != 0 {
				return errors.New("write guard overtook active reads")
			}
			return nil
		})
		errs <- err
	})
	close(release)
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if writes.Load() != 1 {
		t.Fatal("write was lost or replayed", writes.Load())
	}
}

type expiryDuringRefresh struct {
	base    http.RoundTripper
	expired atomic.Bool
	fail    bool
}

func (r *expiryDuringRefresh) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.String() != tokenEndpoint {
		return r.base.RoundTrip(req)
	}
	r.expired.Store(true)
	if r.fail {
		return nil, errors.New("fixture refresh unavailable")
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"new-access","refresh_token":"new-refresh","token_type":"bearer","expires_in":3600}`)), Header: make(http.Header), Request: req}, nil
}

func TestMCPTransportGuardRunsAfterAuthenticationWithoutReplay(t *testing.T) {
	var writes atomic.Int32
	var applicationError atomic.Bool
	client, path := parallelMCPFixture(t, func(object) (any, bool) {
		writes.Add(1)
		return object{"accepted": true}, applicationError.Load()
	})
	transport := &expiryDuringRefresh{base: client.http.Transport}
	client.http.Transport = transport
	if err := aruntime.AtomicJSON(path, Credential{Resource: Endpoint, ClientID: "fixture", AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	guardCalls := 0
	_, err := client.CallGuarded(t.Context(), "place_option_order", object{}, func() error {
		guardCalls++
		if !transport.expired.Load() {
			t.Fatal("transport guard ran before credential refresh")
		}
		return errors.New("funding snapshot or weekly deadline expired during authentication")
	})
	if !errors.Is(err, wheel.ErrNotSubmitted) || writes.Load() != 0 || guardCalls != 1 {
		t.Fatal("expired guard reached HTTP order transport", err, writes.Load(), guardCalls)
	}
	if _, err = client.CallGuarded(t.Context(), "place_option_order", object{}, func() error { return nil }); err != nil || writes.Load() != 1 {
		t.Fatal("valid guarded transport failed", err, writes.Load())
	}
	applicationError.Store(true)
	_, err = client.CallGuarded(t.Context(), "place_option_order", object{}, func() error { return nil })
	if err == nil || errors.Is(err, wheel.ErrNotSubmitted) || writes.Load() != 2 {
		t.Fatal("post-transport error was classified unsent or replayed", err, writes.Load())
	}
	transport.fail = true
	if err := aruntime.AtomicJSON(path, Credential{Resource: Endpoint, ClientID: "fixture", AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	_, err = client.CallGuarded(t.Context(), "place_option_order", object{}, func() error { return nil })
	if !errors.Is(err, wheel.ErrNotSubmitted) || writes.Load() != 2 {
		t.Fatal("failed authentication created an uncertain financial submission", err, writes.Load())
	}
}

func TestMCPUsesFreshConnectionsWithoutAutomaticPOSTReplay(t *testing.T) {
	var mu sync.Mutex
	addresses := map[string]bool{}
	requests := 0
	client, _ := parallelMCPFixture(t, func(object) (any, bool) { return object{"ok": true}, false }, func(req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if req.ProtoMajor != 1 || addresses[req.RemoteAddr] {
			t.Error("request reused a cached or HTTP2 connection")
		}
		addresses[req.RemoteAddr] = true
		requests++
	})
	for range 2 {
		if _, err := client.Call(t.Context(), "get_accounts", object{}); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 5 || len(addresses) != 5 { // initialize, notification, tools, two reads
		t.Fatal("HTTP requests were pooled, duplicated or lost", requests, len(addresses))
	}
}
