package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

func TestMultiyearStrategySwitchRevokesActivation(t *testing.T) {
	dir := t.TempDir()
	c := wheel.DefaultConfig()
	a := &activationControl{Config: c, Dir: dir, Capable: true}
	if err := aruntime.AtomicJSON(filepath.Join(dir, "LIVE.json"), activationRecord{Release: version, ConfigDigest: configDigest(c), At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	h := operatorAPI(func() status {
		return status{Mode: "live", Candidates: []wheel.Candidate{{}}, LiveReadiness: &liveReadiness{Ready: true}}
	}, "read_only", dir, make(chan struct{}, 1), operatorFeatures{Activation: a})
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9841/control/strategy/liquid-roi-v1", nil)
	r.Header.Set("X-AutoWheel-Control", "confirm")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := os.Lstat(filepath.Join(dir, "LIVE.json")); !os.IsNotExist(err) {
		t.Fatal("old live record survived strategy change")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9841/status", nil))
	var got status
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.PendingStrategyPreset != "liquid-roi-v1" || !got.Halted || got.Mode != "read_only" || got.LiveReadiness != nil || len(got.Candidates) != 0 {
		t.Fatalf("stale plan or readiness survived: %+v %v", got, err)
	}
	loaded, err := loadSelectionOverride(c, dir)
	if err != nil || loaded.StrategyPreset != "liquid-roi-v1" || loaded.MaxSpreadBPS != 1000 || configDigest(loaded) == configDigest(c) {
		t.Fatal("restart lost policy or reused activation digest", err)
	}
	for _, s := range []selectionOverride{{}, {Mode: "balanced", Preset: "balanced-roi-v1"}, {Preset: "unknown"}} {
		if _, err := s.apply(c); err == nil {
			t.Fatal("ambiguous or unknown policy accepted")
		}
	}
}

func TestMultiyearStrategyRollbackRemainsPaused(t *testing.T) {
	for _, prior := range []selectionOverride{{Mode: wheel.SelectionBalanced}, {Preset: "conservative-roi-v1"}} {
		for _, preset := range wheel.StrategyPresetNames() {
			t.Run(string(prior.Mode)+prior.Preset+"/"+preset, func(t *testing.T) {
				dir := t.TempDir()
				base := wheel.DefaultConfig()
				base.Account = "12345678"
				base.TradePulseStateDir = t.TempDir()
				writePeerConfig(t, base.TradePulseStateDir, "87654321")
				if err := aruntime.AtomicJSON(filepath.Join(dir, "selection.json"), prior); err != nil {
					t.Fatal(err)
				}
				original, err := loadSelectionOverride(base, dir)
				if err != nil {
					t.Fatal(err)
				}
				a := &activationControl{Config: original, Dir: dir, Capable: true}
				if err := a.enable(); err != nil || !a.enabled() {
					t.Fatal("fixture activation was not valid", err)
				}
				h := operatorAPI(func() status { return status{} }, "read_only", dir, make(chan struct{}, 1), operatorFeatures{Activation: a})
				request := func(handler http.Handler, action string) int {
					r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9841/control/"+action, nil)
					r.Header.Set("X-AutoWheel-Control", "confirm")
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, r)
					return w.Code
				}
				if request(h, "strategy/"+preset) != http.StatusAccepted || a.enabled() {
					t.Fatal("strategy switch retained prior activation")
				}
				selected, err := loadSelectionOverride(base, dir)
				if err != nil || selected.StrategyPreset != preset {
					t.Fatal("new preset was not loaded", err)
				}
				// Exercise the documented rollback: restore the saved private
				// selection while paused, then construct the restarted process.
				if err := aruntime.AtomicJSON(filepath.Join(dir, "selection.json"), prior); err != nil {
					t.Fatal(err)
				}
				loaded, err := loadSelectionOverride(base, dir)
				if err != nil || configDigest(loaded) != configDigest(original) {
					t.Fatal("rollback did not restore the exact prior configuration", err)
				}
				for _, action := range []string{"live", "resume", "refresh"} {
					if request(h, action) != http.StatusConflict {
						t.Fatal("file restoration bypassed the old process restart fence", action)
					}
				}
				restarted := &activationControl{Config: loaded, Dir: dir, Capable: true}
				if restarted.enabled() {
					t.Fatal("restoring the old digest restored live authorization")
				}
				if _, err := os.Lstat(filepath.Join(dir, "LIVE.json")); !os.IsNotExist(err) {
					t.Fatal("rollback recreated a live record")
				}
				if _, err := os.Lstat(filepath.Join(dir, "HALT")); err != nil {
					t.Fatal("rollback cleared the persistent pause", err)
				}
				fresh := operatorAPI(func() status { return status{LiveCapable: true} }, "read_only", dir, make(chan struct{}, 1), operatorFeatures{Activation: restarted})
				if request(fresh, "live") != http.StatusConflict {
					t.Fatal("rollback activated without fresh readiness")
				}
			})
		}
	}
}
