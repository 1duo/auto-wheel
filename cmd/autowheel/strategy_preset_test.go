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
