package moomoo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

func nativeGrant(t *testing.T) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trading.json")
	account := "123456"
	if err := aruntime.AtomicJSON(path, Credential{Resource: Resource, ClientID: "client", AccessToken: "private-access", RefreshToken: "private-refresh",
		ExpiresAt: time.Now().Add(time.Hour), Scope: "quote:read trade:read trade:write accid:" + account}); err != nil {
		t.Fatal(err)
	}
	return path, account
}

func TestTradingGrantScopeAndCanonicalBoundary(t *testing.T) {
	path, account := nativeGrant(t)
	client := NewTradingClient(path, account)
	if err := client.CheckAccess(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"quote:read trade:read", "quote:read trade:read trade:write accid:*", "quote:read trade:read trade:write accid:654321", "quote:read trade:read trade:write quote:write accid:123456", "quote:read trade:read trade:write accid:123456 accid:123456"} {
		if tradingScopeError(account, scope) == nil {
			t.Fatal("unbound/broadened trading scope accepted")
		}
	}
	raw, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), `"scope"`, `"Scope"`, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if client.CheckAccess(t.Context()) == nil {
		t.Fatal("credential case alias accepted")
	}
}

func TestTradingRefreshRejectsBroadeningWithoutReplacement(t *testing.T) {
	path, account := nativeGrant(t)
	var credential Credential
	raw, _ := os.ReadFile(path)
	_ = json.Unmarshal(raw, &credential)
	credential.ExpiresAt = time.Now()
	if err := aruntime.AtomicJSON(path, credential); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	client := NewTradingClient(path, account)
	requests := 0
	client.http.Transport = reviewTransport(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.URL.String() != tokenEndpoint || req.GetBody != nil {
			t.Fatal("refresh left its non-replayable pinned route")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"private-new","refresh_token":"private-new-refresh","token_type":"Bearer","expires_in":3600,"scope":"quote:read trade:read trade:write accid:*"}`))}, nil
	})
	if client.CheckAccess(t.Context()) == nil || requests != 1 {
		t.Fatal("broadened refresh accepted or replayed")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("rejected refresh replaced the bound credential")
	}
}

func TestPlacementTransportSingleAttemptAndFinalGuard(t *testing.T) {
	for _, kind := range []string{"ack", "timeout", "challenge", "redirect", "denied", "foreign"} {
		t.Run(kind, func(t *testing.T) {
			path, account := nativeGrant(t)
			client := NewTradingClient(path, account)
			terms, now, _, _ := receiptFixture(t)
			terms.AccountID = account
			request, _ := PreparePlacement(terms, now)
			if kind == "foreign" {
				request.Path = "/api/v1.0/accounts/654321/orders"
			}
			guarded, hits := false, 0
			client.http.Transport = reviewTransport(func(req *http.Request) (*http.Response, error) {
				hits++
				if !guarded || req.URL.String() != restEndpoint+request.Path || req.Method != "POST" || req.GetBody != nil {
					t.Fatal("financial dispatch escaped pinned, guarded single-attempt transport")
				}
				if kind == "timeout" {
					return nil, errors.New("private network detail")
				}
				status, body := 200, `{"s":"ok","d":{"order_id":"broker-id"}}`
				if kind == "challenge" {
					body = `{"s":"error","jump_url":"private"}`
				}
				if kind == "redirect" {
					status = 307
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Location": {"https://untrusted.invalid/"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			status, raw, err := client.sendPlacement(t.Context(), request, func() error {
				guarded = true
				if kind == "denied" {
					return errors.New("paused")
				}
				return nil
			})
			if kind == "denied" || kind == "foreign" {
				if hits != 0 || !errors.Is(err, wheel.ErrNotSubmitted) {
					t.Fatal("local denial reached financial transport")
				}
			} else {
				if hits != 1 || errors.Is(err, wheel.ErrNotSubmitted) {
					t.Fatal("attempt replayed or uncertainty became known unsent")
				}
				_, decodeErr := DecodePlacementResponse(status, raw)
				if kind != "ack" && err == nil && decodeErr == nil {
					t.Fatal("unqualified response became acknowledgement")
				}
			}
		})
	}
}

func nativeActivation(t *testing.T) *Activation {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return &Activation{Dir: dir, Binding: ActivationBinding{AccountID: "123456", PolicyID: wheel.FixedConservativeASAPPolicy, Release: "test-release",
		ConfigSHA256: strings.Repeat("a", 64), ExecutableSHA256: strings.Repeat("b", 64)}, CheckReady: func() error { return nil }}
}

func TestNativeActivationBindingPauseAndDispatchSerialization(t *testing.T) {
	a := nativeActivation(t)
	if a.Enabled() {
		t.Fatal("missing record enabled trading")
	}
	if err := a.Enable(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*ActivationBinding){func(b *ActivationBinding) { b.AccountID = "654321" }, func(b *ActivationBinding) { b.Release = "other" }, func(b *ActivationBinding) { b.ConfigSHA256 = strings.Repeat("c", 64) }, func(b *ActivationBinding) { b.ExecutableSHA256 = strings.Repeat("d", 64) }} {
		binding := a.Binding
		change(&a.Binding)
		if a.Enabled() {
			t.Fatal("foreign binding reused activation")
		}
		a.Binding = binding
	}
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() { done <- a.WithLive(func() error { close(started); <-release; return nil }) }()
	<-started
	var paused atomic.Bool
	pauseDone := make(chan error, 1)
	go func() { err := a.Pause("operator pause"); paused.Store(true); pauseDone <- err }()
	if paused.Load() {
		t.Fatal("pause crossed in-flight native dispatch")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-pauseDone; err != nil {
		t.Fatal(err)
	}
	if a.Enabled() {
		t.Fatal("durable pause left trading enabled")
	}
	reopened := &Activation{Dir: a.Dir, Binding: a.Binding, CheckReady: a.CheckReady}
	if reopened.Enabled() {
		t.Fatal("pause did not survive reopening")
	}
}

func TestNativeActivationRejectsUnreadyAndAlteredRecord(t *testing.T) {
	a := nativeActivation(t)
	a.CheckReady = func() error { return errors.New("broker facts missing") }
	if a.Enable() == nil {
		t.Fatal("unqualified facts enabled live operation")
	}
	a.CheckReady = func() error { return nil }
	if err := a.Enable(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.Dir, "LIVE.json")
	raw, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), `"binding"`, `"Binding"`, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if a.Enabled() {
		t.Fatal("activation case alias accepted")
	}
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if a.Enabled() {
		t.Fatal("nonprivate activation accepted")
	}
}

func TestExecutionUnqualifiedSourceCannotActivate(t *testing.T) {
	runtime := &Execution{Broker: &Broker{}}
	if runtime.Prepare(context.Background()) == nil || runtime.Readiness().Ready || runtime.CheckReady() == nil {
		t.Fatal("missing financial source became native readiness")
	}
}
