package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

func TestProfitStandingAuthorizationAndPauseFence(t *testing.T) {
	c := wheel.DefaultConfig()
	c.Account, c.TradePulseStateDir = "12345678", t.TempDir()
	writePeerConfig(t, c.TradePulseStateDir, "87654321")
	dir := t.TempDir()
	a := &activationControl{Capable: true, Config: c, Dir: dir}
	if err := a.enable(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "LIVE.json"))
	if err != nil {
		t.Fatal(err)
	}
	ready := status{Mode: "live", LiveCapable: true, Broker: &wheel.Snapshot{At: time.Now()}, LiveReadiness: &liveReadiness{Ready: true, At: time.Now()}}
	st := wheel.NewState()
	p := &profitControl{activation: a, config: c, state: &st, submitMu: &sync.Mutex{}, wake: make(chan struct{}, 1), read: func() status { return ready }}
	preview := &profitPreview{ID: "id", ExpiresAt: time.Now().Add(time.Minute), ContinueLive: true}
	if err := p.authorize(preview); err != nil {
		t.Fatal("existing live authorization rejected", err)
	}
	p.preview = preview
	// Unsupported policy prevents broker access, but a close validation failure
	// must not revoke an existing live session or arm a new close-only session.
	if err := p.execute(t.Context(), preview.ID); err == nil {
		t.Fatal("unsupported execution accepted")
	}
	after, err := os.ReadFile(filepath.Join(dir, "LIVE.json"))
	if err != nil || !bytes.Equal(before, after) || !a.enabled() || a.profitOnly() != "" || len(st.Intents) != 0 {
		t.Fatal("standing authorization changed on a pre-transport failure", err)
	}
	if err := pauseTrading(a, dir, "operator paused during preview"); err != nil {
		t.Fatal(err)
	}
	// Even a stale status claiming live cannot bypass the durable halt.
	if p.authorize(preview) == nil || a.enabled() {
		t.Fatal("pause was overwritten by a live preview")
	}
	ready.Halted = true
	if p.authorize(preview) == nil || a.enabled() {
		t.Fatal("live preview silently converted into close-only activation")
	}
	if _, err := os.Stat(filepath.Join(dir, "HALT")); err != nil {
		t.Fatal("explicit pause lost", err)
	}
}

func TestProfitControlAuthenticationAndBusy(t *testing.T) {
	dir := t.TempDir()
	p := &profitControl{cycleMu: &sync.Mutex{}}
	h := operatorAPI(func() status { return status{} }, "read_only", dir, make(chan struct{}, 1), operatorFeatures{Profit: p})
	for _, path := range []string{"profit-preview/NVDA", "take-profit/id"} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:9841/control/"+path, nil)
		r.Header.Set("X-AutoWheel-Control", "confirm")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatal("unauthenticated profit control", w.Code)
		}
		authorizeOperatorRequest(t, r, dir)
		p.cycleMu.Lock()
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		p.cycleMu.Unlock()
		if w.Code != http.StatusConflict {
			t.Fatal("concurrent cycle admitted", w.Code)
		}
	}
}

func TestProfitPreviewOneUseAndReadiness(t *testing.T) {
	for _, tc := range []string{"expired", "wrong_id", "not_ready", "not_paused"} {
		t.Run(tc, func(t *testing.T) {
			p := &profitControl{preview: &profitPreview{ID: "id", ExpiresAt: time.Now().Add(time.Minute)}, read: func() status { return status{Halted: tc != "not_paused"} }}
			id := "id"
			if tc == "wrong_id" {
				id = "other"
			}
			if tc == "expired" {
				p.preview.ExpiresAt = time.Now().Add(-time.Second)
			}
			if p.execute(context.Background(), id) == nil || p.preview != nil {
				t.Fatal("unready close admitted or preview reusable")
			}
		})
	}
}

func TestProfitOnlyActivationBinding(t *testing.T) {
	dir := t.TempDir()
	c := wheel.DefaultConfig()
	c.Account = "12345678"
	c.TradePulseStateDir = t.TempDir()
	writePeerConfig(t, c.TradePulseStateDir, "87654321")
	a := &activationControl{Capable: true, Dir: dir, Config: c}
	if err := a.enablePurpose("preview-id"); err != nil {
		t.Fatal(err)
	}
	if !a.enabled() || a.profitOnly() != "preview-id" {
		t.Fatal("close purpose missing")
	}
	copy := *a
	copy.Config.MaxSymbols = 2
	if copy.enabled() {
		t.Fatal("close authorization survived config change")
	}
	if err := pauseTrading(a, dir, "test close completed"); err != nil {
		t.Fatal(err)
	}
	if a.enabled() || a.profitOnly() != "" {
		t.Fatal("close activation survived pause")
	}
	if _, err := os.Stat(filepath.Join(dir, "HALT")); err != nil {
		t.Fatal(err)
	}
	if err := aruntime.AtomicJSON(filepath.Join(dir, "LIVE.json"), map[string]string{"profit_only": "id"}); err != nil {
		t.Fatal(err)
	}
	if a.enabled() {
		t.Fatal("unbound close activated")
	}
}

func TestProfitAttemptReturnsToPauseOnFailure(t *testing.T) {
	c := wheel.DefaultConfig()
	c.Account = "12345678"
	c.TradePulseStateDir = t.TempDir()
	writePeerConfig(t, c.TradePulseStateDir, "87654321")
	dir := t.TempDir()
	a := &activationControl{Capable: true, Config: c, Dir: dir}
	st := wheel.NewState()
	ready := status{Halted: true, LiveCapable: true, Broker: &wheel.Snapshot{At: time.Now()}, LiveReadiness: &liveReadiness{Ready: true, At: time.Now()}}
	p := &profitControl{activation: a, config: c, state: &st, submitMu: &sync.Mutex{}, wake: make(chan struct{}, 1), read: func() status { return ready }, preview: &profitPreview{ID: "id", ExpiresAt: time.Now().Add(time.Minute)}}
	// Unsupported account policy fails before broker access, after purpose binding.
	if p.execute(t.Context(), "id") == nil {
		t.Fatal("unsupported execution accepted")
	}
	if p.busy.Load() || p.preview != nil || a.enabled() || a.profitOnly() != "" || len(st.Intents) != 0 {
		t.Fatal("failed close was left armed")
	}
	if _, err := os.Stat(filepath.Join(dir, "HALT")); err != nil {
		t.Fatal("failed close did not persist pause", err)
	}
}
