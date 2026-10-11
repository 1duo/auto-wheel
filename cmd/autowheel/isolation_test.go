package main

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

func writePeerConfig(t *testing.T, dir, account string) {
	t.Helper()
	err := aruntime.AtomicJSON(filepath.Join(dir, "config.json"), map[string]any{
		"schema_version":                 "PilotConfigV1",
		"expected_account_identity_hash": fmt.Sprintf("%x", sha256.Sum256([]byte(account))),
		"live_order_enabled":             false,
		"other_peer_owned_field":         "permitted",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTradePulseIsolationBinding(t *testing.T) {
	c := wheel.DefaultConfig()
	c.Account = "12345678"
	if tradePulseIsolation(c) == nil {
		t.Fatal("unconfigured peer allowed live trading")
	}
	c.TradePulseStateDir = t.TempDir()
	if tradePulseIsolation(c) == nil {
		t.Fatal("missing peer binding allowed live trading")
	}
	writePeerConfig(t, c.TradePulseStateDir, c.Account)
	if err := tradePulseIsolation(c); err == nil || !strings.Contains(err.Error(), "separate account") {
		t.Fatal("a paused peer sharing the account must block activation")
	}
	writePeerConfig(t, c.TradePulseStateDir, "87654321")
	if err := tradePulseIsolation(c); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(c.TradePulseStateDir, "config.json")
	for _, raw := range []string{
		`{"schema_version":"PilotConfigV1","expected_account_identity_hash":null}`,
		`{"schema_version":"Other","expected_account_identity_hash":"` + strings.Repeat("a", 64) + `"}`,
		`{"schema_version":"PilotConfigV1","expected_account_identity_hash":"bad"}`,
		`{"schema_version":"PilotConfigV1","expected_account_identity_hash":"` + strings.Repeat("A", 64) + `"}`,
		`{"schema_version":"PilotConfigV1","expected_account_identity_hash":"` + strings.Repeat("a", 64) + `"} {}`,
		`{"schema_version":`,
	} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if tradePulseIsolation(c) == nil {
			t.Fatal("invalid peer binding allowed live trading")
		}
	}
	writePeerConfig(t, c.TradePulseStateDir, "87654321")
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if tradePulseIsolation(c) == nil {
		t.Fatal("public peer file allowed live trading")
	}
	writePeerConfig(t, c.TradePulseStateDir, "87654321")
	if err := os.Rename(path, path+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path+".real", path); err != nil {
		t.Fatal(err)
	}
	if tradePulseIsolation(c) == nil {
		t.Fatal("symlinked peer binding allowed live trading")
	}
	c.TradePulseStateDir = "relative/state"
	if c.Validate() == nil {
		t.Fatal("relative peer boundary allowed")
	}
}

func TestTradePulseConflictFencesActivation(t *testing.T) {
	c := wheel.DefaultConfig()
	c.Account = "12345678"
	c.TradePulseStateDir = t.TempDir()
	writePeerConfig(t, c.TradePulseStateDir, "87654321")
	a := &activationControl{Capable: true, Config: c, Dir: t.TempDir()}
	if err := a.enable(); err != nil || !a.enabled() {
		t.Fatal("separate-account fixture failed activation", err)
	}
	writePeerConfig(t, c.TradePulseStateDir, c.Account)
	peerBefore, err := os.ReadFile(filepath.Join(c.TradePulseStateDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if a.enabled() || a.enable() == nil {
		t.Fatal("cached activation bypassed fresh account conflict")
	}
	// Even a stale published 'ready' cannot authorize activation. No broker
	// transport exists in this test and the peer's own pause flag is false.
	s := status{LiveCapable: true, Broker: &wheel.Snapshot{At: time.Now()}, LiveReadiness: &liveReadiness{At: time.Now(), Ready: true}}
	var mu sync.Mutex
	handler := operatorAPI(func() status { return s }, "read_only", a.Dir, make(chan struct{}, 1), operatorFeatures{Activation: a, SubmitMu: &mu})
	r := httptest.NewRequest("POST", "http://127.0.0.1:9841/control/live", nil)
	r.Header.Set("X-AutoWheel-Control", "confirm")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusConflict {
		t.Fatal("native control bypassed account conflict", w.Code)
	}
	if r := preflight(t.Context(), c, wheel.NewState(), nil, wheel.Snapshot{}); r.Ready || !strings.Contains(r.Reason, "separate account") {
		t.Fatal("peer conflict reached broker preflight")
	}
	if err := a.fenceIsolation(); err == nil {
		t.Fatal("submission fence failed to report conflict")
	}
	if _, err := os.Stat(filepath.Join(a.Dir, "HALT")); err != nil {
		t.Fatal("isolation loss did not persist pause", err)
	}
	if _, err := os.Stat(filepath.Join(a.Dir, "LIVE.json")); !os.IsNotExist(err) {
		t.Fatal("isolation loss did not revoke activation")
	}
	peerAfter, err := os.ReadFile(filepath.Join(c.TradePulseStateDir, "config.json"))
	if err != nil || string(peerBefore) != string(peerAfter) {
		t.Fatal("AutoWheel changed its peer configuration")
	}
	writePeerConfig(t, c.TradePulseStateDir, "87654321")
	if a.enabled() {
		t.Fatal("restored isolation restarted trading without user activation")
	}
}
