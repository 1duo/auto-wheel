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
	"autowheel/internal/sharedclient"
	"autowheel/internal/wheel"
)

func TestSelectionControlPausesAndRequiresRestart(t *testing.T) {
	dir := t.TempDir()
	c := wheel.DefaultConfig()
	a := &activationControl{Config: c, Dir: dir, Capable: true}
	if err := aruntime.AtomicJSON(filepath.Join(dir, "LIVE.json"), activationRecord{Release: version, ConfigDigest: configDigest(c), At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s := status{Mode: "live", LiveCapable: true, LiveReadiness: &liveReadiness{At: time.Now(), Ready: true}, Broker: &wheel.Snapshot{At: time.Now()}, Candidates: []wheel.Candidate{{}}}
	h := operatorAPI(func() status { return s }, "read_only", dir, make(chan struct{}, 1), operatorFeatures{Activation: a})
	request := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9841/control/"+path, nil)
		r.Header.Set("X-AutoWheel-Control", "confirm")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request("selection/conservative"); w.Code != http.StatusAccepted {
		t.Fatalf("selection failed: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "HALT")); err != nil {
		t.Fatal("selection did not persist pause", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "LIVE.json")); !os.IsNotExist(err) {
		t.Fatal("selection retained live authorization")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9841/status", nil))
	var got status
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.PendingSelectionMode != wheel.SelectionConservative || got.Mode != "read_only" || !got.Halted || got.LiveReadiness != nil || len(got.Candidates) != 0 || activationReady(got) {
		t.Fatalf("stale readiness/plan remained usable: %+v", got)
	}
	for _, action := range []string{"live", "resume", "refresh"} {
		if w := request(action); w.Code != http.StatusConflict {
			t.Fatalf("%s bypassed pending policy: %d", action, w.Code)
		}
	}
	loaded, err := loadSelectionOverride(c, dir)
	if err != nil || loaded.SelectionMode != wheel.SelectionConservative || loaded.PutMaxDelta != .15 {
		t.Fatalf("restart lost preset: %+v %v", loaded, err)
	}
	if a.Config.SelectionMode != "" || configDigest(loaded) == configDigest(c) {
		t.Fatal("policy mutated old process or reused activation digest")
	}
	if info, err := os.Stat(filepath.Join(dir, "selection.json")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("selection must remain private")
	}
}

func TestSelectionControlBoundaryAndSubmissionLock(t *testing.T) {
	dir := t.TempDir()
	a := &activationControl{Config: wheel.DefaultConfig(), Dir: dir}
	var submitMu sync.Mutex
	h := operatorAPI(func() status { return status{} }, "read_only", dir, make(chan struct{}, 1), operatorFeatures{Activation: a, SubmitMu: &submitMu})
	for _, tc := range []struct {
		mode, origin, header, body, host string
		want                             int
	}{
		{"unknown", "", "confirm", "", "127.0.0.1:9841", 400},
		{"conservative", "https://example.com", "confirm", "", "127.0.0.1:9841", 403},
		{"conservative", "", "", "", "127.0.0.1:9841", 403},
		{"conservative", "", "confirm", "payload", "127.0.0.1:9841", 403},
		{"conservative", "", "confirm", "", "evil.example", 403},
	} {
		r := httptest.NewRequest(http.MethodPost, "/control/selection/"+tc.mode, strings.NewReader(tc.body))
		r.Host = tc.host
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("X-AutoWheel-Control", tc.header)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("boundary status %d, want %d", w.Code, tc.want)
		}
	}
	submitMu.Lock()
	done := make(chan int, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9841/control/selection/balanced", nil)
		r.Header.Set("X-AutoWheel-Control", "confirm")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		done <- w.Code
	}()
	<-started
	select {
	case <-done:
		submitMu.Unlock()
		t.Fatal("selection bypassed submission lock")
	case <-time.After(20 * time.Millisecond):
	}
	_, err := os.Stat(filepath.Join(dir, "selection.json"))
	submitMu.Unlock()
	if !os.IsNotExist(err) || <-done != http.StatusAccepted {
		t.Fatal("selection was not serialized with submissions")
	}
}

func TestSelectionOverrideFailsClosed(t *testing.T) {
	c := wheel.DefaultConfig()
	for _, raw := range []string{`{"mode":"unknown"}`, `{"mode":""}`, `{"mode":"balanced","extra":true}`, `{"mode":"balanced"}{}`, `null`} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "selection.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadSelectionOverride(c, dir); err == nil {
			t.Fatalf("invalid override accepted: %s", raw)
		}
	}
	dir := t.TempDir()
	if got, err := loadSelectionOverride(c, dir); err != nil || configDigest(got) != configDigest(c) {
		t.Fatal("absent override changed legacy configuration")
	}
	other := filepath.Join(t.TempDir(), "selection.json")
	if err := aruntime.AtomicJSON(other, selectionOverride{Mode: wheel.SelectionBalanced}); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(dir, "selection.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSelectionOverride(c, dir); err == nil {
		t.Fatal("symlink selection policy accepted")
	}
}

func TestSelectionObserverExceptionRequiresNoRegistration(t *testing.T) {
	now := time.Now()
	base := sharedclient.Status{At: now}
	if !unregisteredObserver(base, now) {
		t.Fatal("fresh empty observer not recognized")
	}
	for _, change := range []func(*sharedclient.Status){
		func(s *sharedclient.Status) { s.Ready = true },
		func(s *sharedclient.Status) { s.Reconciled = true },
		func(s *sharedclient.Status) { s.SingleWriter = true },
		func(s *sharedclient.Status) { s.NewRisk = true },
		func(s *sharedclient.Status) { s.Management = true },
		func(s *sharedclient.Status) { s.Generation = 1 },
		func(s *sharedclient.Status) { s.Epoch = 1 },
		func(s *sharedclient.Status) { s.At = now.Add(-91 * time.Second) },
		func(s *sharedclient.Status) { s.At = now.Add(3 * time.Second) },
		func(s *sharedclient.Status) { s.At = time.Time{} },
	} {
		s := base
		change(&s)
		if unregisteredObserver(s, now) {
			t.Fatal("registered, permitted or stale observer accepted")
		}
	}
}
