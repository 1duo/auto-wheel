package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

func TestTP75NativeTransitionPauseArchiveAndActivationBinding(t *testing.T) {
	config, _, state, _ := fixedCycleFixture(t)
	state.Fixed.BoundAt = state.Fixed.BoundAt.AddDate(0, 0, -7)
	state.Fixed.Spec.FeeKnownAt = state.Fixed.Spec.FeeKnownAt.AddDate(0, 0, -7)
	config.FixedPolicy.Spec = state.Fixed.Spec
	config.FixedPolicy.Spec.ID = wheel.FixedConservativeTP75Policy
	dir := filepath.Join(t.TempDir(), "state")
	j, _, err := aruntime.OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = j.Save(*state); err != nil {
		t.Fatal(err)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if enableTP75(config, dir, &out) == nil {
		t.Fatal("enabled policy without durable pause")
	}
	if err = aruntime.AtomicJSON(filepath.Join(dir, "HALT"), "authorized TP75 transition"); err != nil {
		t.Fatal(err)
	}
	if err = aruntime.AtomicJSON(filepath.Join(dir, "LIVE.json"), map[string]string{"release": "previous"}); err != nil {
		t.Fatal(err)
	}
	if enableTP75(config, dir, &out) == nil {
		t.Fatal("reused previous live authority")
	}
	if err = os.Remove(filepath.Join(dir, "LIVE.json")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = enableTP75(config, dir, &out); err != nil {
		t.Fatal(err)
	}
	j, loaded, err := aruntime.OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if loaded.Fixed.Spec.ID != wheel.FixedConservativeTP75Policy || loaded.Fixed.PolicySHA != config.FixedPolicy.Spec.Digest() {
		t.Fatal("TP75 binding lost on reopen")
	}
	files, err := os.ReadDir(filepath.Join(dir, "fixed-policy-archive"))
	if err != nil || len(files) != 1 {
		t.Fatal("missing immutable source", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "fixed-policy-archive", files[0].Name()))
	if err != nil || !bytes.Equal(raw, before) {
		t.Fatal("archived source changed", err)
	}
	if _, err = os.Stat(filepath.Join(dir, "HALT")); err != nil {
		t.Fatal("transition cleared pause")
	}
	if _, err = os.Lstat(filepath.Join(dir, "LIVE.json")); !os.IsNotExist(err) {
		t.Fatal("transition activated transport")
	}
	now := time.Now()
	s := status{Config: config.PublicCopy(), LiveCapable: true, Broker: &wheel.Snapshot{At: now, OrderCoveragePolicy: "exclusive_standard"}, LiveReadiness: &liveReadiness{At: now, Ready: true, Policy: wheel.FixedConservativePolicy, OrderCoveragePolicy: "exclusive_standard", CoverageWarning: wheel.StandardOrderWarning}}
	if activationReady(s) {
		t.Fatal("hold readiness authorized TP75")
	}
	s.LiveReadiness.Policy = wheel.FixedConservativeTP75Policy
	if !activationReady(s) {
		t.Fatal("fresh TP75 readiness rejected")
	}
	weekday := time.Date(2026, 10, 20, 15, 28, 59, 0, wheel.Eastern)
	if cycleDelay(config, weekday) != time.Second || cycleDelay(config, weekday.Add(2*time.Second)) != 59*time.Second {
		t.Fatal("daily capture/execution clocks not scheduled")
	}
}
