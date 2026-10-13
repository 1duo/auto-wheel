package main

import (
	"encoding/json"
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

func TestPostPreflightSnapshotMustReconcile(t *testing.T) {
	for _, name := range []string{"healthy", "foreign_option", "unresolved_order", "confirmed_fill", "stale_snapshot"} {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			c := wheel.DefaultConfig()
			c.Account = "12345678"
			st := wheel.NewState()
			st.Account = c.Account
			snapshot := wheel.Snapshot{Account: c.Account, At: now, AvailableCash: c.MaxCollateral}
			wantReady := name == "healthy" || name == "confirmed_fill"
			if name == "foreign_option" {
				snapshot.Options = []wheel.Option{{ID: "foreign", Symbol: "NVDA", Kind: "put", Strike: 20000, Quantity: 1}}
			}
			if name == "unresolved_order" || name == "confirmed_fill" {
				st.Intents["NVDA"] = wheel.Intent{Ref: "own-ref", At: now, Candidate: wheel.Candidate{Quote: wheel.Quote{Option: wheel.Option{ID: "own-put", Symbol: "NVDA", Kind: "put", Strike: 20000}}}}
				order := wheel.Order{ID: "own-order", Ref: "own-ref", Symbol: "NVDA", OptionID: "own-put", Quantity: 1, Side: "sell", Effect: "open", State: "queued"}
				if name == "confirmed_fill" {
					order.State, order.Filled = "filled", 1
					snapshot.Options = []wheel.Option{{ID: "own-put", Symbol: "NVDA", Kind: "put", Strike: 20000, Quantity: 1}}
				}
				snapshot.Orders = []wheel.Order{order}
			}
			if name == "stale_snapshot" {
				snapshot.At = now.Add(-31 * time.Second)
			}
			dir := filepath.Join(t.TempDir(), "state")
			journal, _, err := aruntime.OpenJournal(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer journal.Close()
			if err := journal.Save(st); err != nil {
				t.Fatal(err)
			}
			checked := liveReadiness{At: now.Add(-time.Minute), Ready: true}
			next, err := recheckReadiness(c, &st, journal, snapshot, checked, now)
			if err != nil || next.Ready != wantReady {
				t.Fatalf("ready=%v, want=%v, error=%v", next.Ready, wantReady, err)
			}
			if wantReady && next.At != checked.At {
				t.Fatal("snapshot refresh extended compatibility evidence lifetime")
			}
			s := status{LiveCapable: true, LiveReadiness: &next, Broker: &snapshot, Intents: st.Intents}
			handler := operatorAPI(func() status { return s }, "read_only", dir, make(chan struct{}, 1))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest("GET", "http://127.0.0.1:9841/readyz", nil))
			wantCode := http.StatusServiceUnavailable
			if wantReady {
				wantCode = http.StatusOK
			}
			if response.Code != wantCode {
				t.Fatalf("readiness API=%d, want=%d", response.Code, wantCode)
			}
			if name == "confirmed_fill" {
				raw, err := os.ReadFile(filepath.Join(dir, "journal.json"))
				var persisted wheel.State
				if err != nil || json.Unmarshal(raw, &persisted) != nil || persisted.Positions["NVDA"].PutID != "own-put" || len(persisted.Intents) != 0 {
					t.Fatal("confirmed ownership was published before durable reconciliation")
				}
			}
		})
	}
}

func TestLiveActivationPersistenceAndGuards(t *testing.T) {
	dir := t.TempDir()
	c := wheel.DefaultConfig()
	c.Account = "12345678"
	c.SymbolLimitScope = "managed"
	c.TradePulseStateDir = t.TempDir()
	writePeerConfig(t, c.TradePulseStateDir, "87654321")
	a := &activationControl{Capable: true, Config: c, Dir: dir}
	var mu sync.Mutex
	s := status{LiveCapable: true, Broker: &wheel.Snapshot{At: time.Now()}}
	handler := operatorAPI(func() status { return s }, "read_only", dir, make(chan struct{}, 1), operatorFeatures{Activation: a, SubmitMu: &mu})
	request := func(action string) int {
		r := httptest.NewRequest("POST", "http://127.0.0.1:9841/control/"+action, nil)
		r.Header.Set("X-AutoWheel-Control", "confirm")
		authorizeOperatorRequest(t, r, dir)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if request("live") != http.StatusConflict || a.enabled() {
		t.Fatal("activation bypassed readiness")
	}
	s.LiveReadiness = &liveReadiness{At: time.Now(), Ready: true}
	s.Intents = map[string]wheel.Intent{"NVDA": {Ref: "unknown"}}
	if request("live") != http.StatusConflict {
		t.Fatal("unresolved write allowed activation")
	}
	s.Intents = nil
	s.LiveReadiness.At = time.Now().Add(-21 * time.Minute)
	if request("live") != http.StatusConflict {
		t.Fatal("expired checks allowed activation")
	}
	s.LiveReadiness.At = time.Now()
	if request("live") != http.StatusAccepted || !a.enabled() {
		t.Fatal("verified activation failed")
	}
	info, err := os.Lstat(filepath.Join(dir, "LIVE.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("activation must be private")
	}
	restarted := *a
	if !restarted.enabled() {
		t.Fatal("operator activation did not survive restart")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "LIVE.json"))
	var record activationRecord
	if err != nil || json.Unmarshal(raw, &record) != nil || len(record.ExecutableDigest) != 64 {
		t.Fatal("activation did not bind the executable")
	}
	for _, digest := range []string{"", strings.Repeat("0", 64)} {
		changed := record
		changed.ExecutableDigest = digest
		if err := aruntime.AtomicJSON(filepath.Join(dir, "LIVE.json"), changed); err != nil {
			t.Fatal(err)
		}
		if restarted.enabled() {
			t.Fatal("missing or changed executable identity reused activation")
		}
	}
	if err := aruntime.AtomicJSON(filepath.Join(dir, "LIVE.json"), record); err != nil {
		t.Fatal(err)
	}
	restarted.Config.MaxSymbols = 2
	if restarted.enabled() {
		t.Fatal("configuration change reused activation")
	}
	old := version
	version = "changed-release"
	enabled := a.enabled()
	version = old
	if enabled {
		t.Fatal("new release reused activation")
	}
	if request("halt") != http.StatusAccepted || a.enabled() {
		t.Fatal("pause did not revoke activation")
	}
	if _, err := os.Stat(filepath.Join(dir, "HALT")); err != nil {
		t.Fatal("pause not durable")
	}
	if request("live") != http.StatusAccepted || !a.enabled() {
		t.Fatal("one-click reactivation failed")
	}
	if _, err := os.Stat(filepath.Join(dir, "HALT")); !os.IsNotExist(err) {
		t.Fatal("reactivation did not clear pause")
	}
}
