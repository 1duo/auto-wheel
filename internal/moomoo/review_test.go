package moomoo

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aruntime "autowheel/internal/runtime"
)

func TestJSONRejectsAmbiguityAndTruncation(t *testing.T) {
	for _, raw := range []string{
		`{"s":"error","s":"ok"}`, `{"a":{"scope":"trade:write","scope":"trade:read"}}`,
		`{"a":1,"\u0061":2}`, `{"a":1} {"a":2}`, "{\"a\":\"\xff\"}",
	} {
		var out any
		if err := exactJSON([]byte(raw), &out); err == nil {
			t.Fatal("ambiguous JSON accepted")
		}
	}
	var out struct {
		Scope string `json:"scope"`
	}
	if err := boundedJSON(strings.NewReader(`{"scope":"trade:read"}`+strings.Repeat(" ", 64)+`garbage`), 32, &out); err == nil {
		t.Fatal("oversized response accepted as valid prefix")
	}
	if err := privateJSON(strings.NewReader(`{"scope":"trade:read","unexpected":true}`), 256, &out); err == nil {
		t.Fatal("unknown private credential/config field accepted")
	}
}

func TestMCPStreamReturnsWithoutEOFAndSkipsNotifications(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	type result struct {
		raw []byte
		err error
	}
	done := make(chan result, 1)
	go func() { raw, err := sseResponse(reader, 7); done <- result{raw, err} }()
	stream := ": keepalive\r\n\r\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{}}\r\n\r\ndata: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{\"tools\":[]}}\r\n\r\n"
	if _, err := io.WriteString(writer, stream); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || !strings.Contains(string(got.raw), `"id":7`) {
			t.Fatalf("stream response: %v", got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("reader waited for EOF after receiving its response")
	}
	for _, stream := range []string{
		"data: {\"jsonrpc\":\"2.0\",\"id\":\"7\",\"result\":{}}\n\n",
		"data: {\"jsonrpc\":\"2.0\",\"id\":8,\"result\":{}}\n\n",
		"data: {\"jsonrpc\":\"2.0\",\"id\":7,\"id\":8,\"result\":{}}\n\n",
		"data: {\"jsonrpc\":\"2.0\",\"id\":7,\"method\":\"sampling/createMessage\"}\n\n",
	} {
		if _, err := sseResponse(strings.NewReader(stream), 7); err == nil {
			t.Fatal("unmatched or unsupported message accepted")
		}
	}
}

func TestMCPNegotiatesOnlyReviewedProtocols(t *testing.T) {
	for _, version := range []string{protocol, "2025-03-26", "2024-11-05", "future-version"} {
		t.Run(version, func(t *testing.T) {
			calls := 0
			client := NewClient(evidenceCredential(t))
			client.http = &http.Client{Transport: reviewTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				var body struct {
					ID     int64  `json:"id"`
					Method string `json:"method"`
				}
				if json.NewDecoder(req.Body).Decode(&body) != nil {
					t.Fatal("invalid test request")
				}
				wantHeader := protocol
				if body.Method != "initialize" {
					wantHeader = version
				}
				if req.Header.Get("MCP-Protocol-Version") != wantHeader {
					t.Fatal("post-initialization request ignored negotiated protocol")
				}
				if body.Method == "notifications/initialized" {
					return &http.Response{StatusCode: http.StatusAccepted, Body: http.NoBody}, nil
				}
				var result any = map[string]any{"protocolVersion": version}
				if body.Method == "tools/list" {
					result = map[string]any{"tools": []any{map[string]any{"name": "get_funds", "inputSchema": map[string]any{"type": "object"}}}}
				}
				raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": body.ID, "result": result})
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
			})}
			err := client.Connect(t.Context())
			if version == protocol || version == "2025-03-26" {
				if err != nil || calls != 3 {
					t.Fatalf("reviewed protocol failed: calls=%d error=%v", calls, err)
				}
			} else if err == nil || calls != 1 {
				t.Fatal("unreviewed protocol reached normal operation")
			}
		})
	}
}

type reviewTransport func(*http.Request) (*http.Response, error)

func (f reviewTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOAuthRejectsMalformedRefreshWithoutReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	old := Credential{Resource: Resource, ClientID: "client", AccessToken: "old-access", RefreshToken: "old-refresh", Scope: "quote:read trade:read", ExpiresAt: time.Now().Add(-time.Hour)}
	if err := aruntime.AtomicJSON(path, old); err != nil {
		t.Fatal(err)
	}
	calls := 0
	h := &http.Client{Transport: reviewTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.String() != tokenEndpoint || req.GetBody != nil {
			t.Fatal("refresh endpoint/rewind boundary")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"new-access","token_type":"Bearer","expires_in":7200,"scope":"quote:read trade:read"} trailing`))}, nil
	})}
	if _, err := (&Tokens{Path: path}).Access(t.Context(), h); err == nil || calls != 1 {
		t.Fatal("malformed refresh accepted or replayed")
	}
	f, err := aruntime.PrivateRead(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var retained Credential
	if err := json.NewDecoder(f).Decode(&retained); err != nil || retained.AccessToken != old.AccessToken {
		t.Fatal("failed refresh replaced the durable credential")
	}
}

func TestOAuthScopeRejectionHasPrivateSafeDiagnostics(t *testing.T) {
	for _, tc := range []struct{ scope, reason string }{
		{"quote:read trade:read quote:write", "watchlist write"},
		{"quote:read trade:read trade:write", "trading write"},
		{"quote:read trade:read accid:private-account", "account scope encoding"},
		{"quote:read trade:read private-permission", "unsupported permission"},
		{"trade:read", "market data read"},
		{"quote:read", "account and trade read"},
	} {
		err := readOnlyScopeError(tc.scope)
		if err == nil || !strings.Contains(err.Error(), tc.reason) || strings.Contains(err.Error(), "private-") {
			t.Fatalf("unsafe or unhelpful permission diagnostic: %v", err)
		}
	}
	for _, scope := range []string{"quote:read trade:read trade:write", "quote:read trade:read accid:private-account"} {
		h := &http.Client{Transport: reviewTransport(func(*http.Request) (*http.Response, error) {
			body, _ := json.Marshal(map[string]any{"access_token": "private-access", "refresh_token": "private-refresh", "token_type": "Bearer", "expires_in": 7200, "scope": scope})
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
		})}
		credential, err := exchange(t.Context(), h, nil, Credential{ClientID: "client"})
		if err == nil || credential.AccessToken != "" || credential.RefreshToken != "" || strings.Contains(err.Error(), "private-") {
			t.Fatal("rejected grant produced usable credentials or exposed private data")
		}
	}
}

func TestOAuthRegistrationRequestsAndEnforcesReadOnly(t *testing.T) {
	for _, scope := range []string{observerScope, "quote:read quote:write trade:read trade:write accid:*", ""} {
		h := &http.Client{Transport: reviewTransport(func(req *http.Request) (*http.Response, error) {
			var request struct {
				Scope    string   `json:"scope"`
				Redirect []string `json:"redirect_uris"`
			}
			if req.URL.String() != registerEndpoint || req.GetBody != nil || json.NewDecoder(req.Body).Decode(&request) != nil || request.Scope != observerScope || len(request.Redirect) != 1 || request.Redirect[0] != "http://127.0.0.1:8766/oauth/callback" {
				t.Fatal("observer registration omitted its scope or callback boundary")
			}
			body, _ := json.Marshal(map[string]any{"client_id": "client", "token_endpoint_auth_method": "none", "scope": scope})
			return &http.Response{StatusCode: http.StatusCreated, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
		})}
		id, err := registerObserver(t.Context(), h, "http://127.0.0.1:8766/oauth/callback")
		if scope == observerScope {
			if err != nil || id != "client" {
				t.Fatalf("read-only registration refused: %v", err)
			}
		} else if err == nil || id != "" {
			t.Fatal("broad or unproven registration reached the consent stage")
		}
	}
}

func TestAccountBindingCommittedBeforeConfigAndRequiresLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	c := DefaultConfig()
	state, err := OpenState(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.BindAccount("123456"); err != nil {
		t.Fatal(err)
	}
	if err := state.BindAccount("654321"); err == nil {
		t.Fatal("second account replaced authenticated selection")
	}
	state.Close()
	if err := state.BindAccount("123456"); err == nil {
		t.Fatal("closed lock permitted binding")
	}
	if _, err := OpenState(dir, c); err == nil {
		t.Fatal("old config silently erased committed account binding")
	}
	c.AccountID, c.Strategy.Account = "123456", "123456"
	state, err = OpenState(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	state.Close()
}

func TestOAuthCallbackDenialEndsWaitAndRejectsReplay(t *testing.T) {
	results := make(chan authorizationResult, 1)
	handler := authorizationCallback("expected-state", results)
	request := func(query string) int {
		response := httptest.NewRecorder()
		handler(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/oauth/callback?"+query, nil))
		return response.Code
	}
	for _, query := range []string{"state=wrong&code=code", "state=expected-state&state=other&code=code", "state=expected-state&code=code&error=access_denied"} {
		if request(query) != http.StatusBadRequest {
			t.Fatal("invalid callback accepted")
		}
	}
	if len(results) != 0 {
		t.Fatal("invalid callback ended the valid authorization wait")
	}
	if request("state=expected-state&error=access_denied") != http.StatusOK {
		t.Fatal("valid authorization denial was not acknowledged")
	}
	if result := <-results; result.Err == nil || result.Code != "" {
		t.Fatal("denial did not terminate the pending login")
	}
	if request("state=expected-state&code=code") != http.StatusConflict {
		t.Fatal("consumed callback state was reused")
	}
}

func TestOAuthCallbackAcknowledgesBeforeLoginCanCloseServer(t *testing.T) {
	results := make(chan authorizationResult)
	release := make(chan struct{})
	go func() {
		<-release
		<-results
	}()
	server := httptest.NewServer(authorizationCallback("state", results))
	defer server.Close()
	defer close(release)
	client := server.Client()
	client.Timeout = time.Second
	response, err := client.Get(server.URL + "?state=state&code=code")
	if err != nil {
		t.Fatal("browser acknowledgement waited for the login exchange", err)
	}
	defer response.Body.Close()
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil || !strings.Contains(line, "authorization received") || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("browser acknowledgement was not flushed before releasing login")
	}
}
