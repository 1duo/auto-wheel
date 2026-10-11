package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

func TestConfigCheckHasNoTradingOrHandoffEffects(t *testing.T) {
	dir := t.TempDir()
	c, err := wheel.DefaultConfig().WithSelectionMode(wheel.SelectionConservative)
	if err != nil {
		t.Fatal(err)
	}
	c.Account = "12345678"
	c.TradePulseStateDir = filepath.Join(dir, "absent-peer")
	c.RetiredTradePulse = filepath.Join(c.TradePulseStateDir, "supervisor", "cross-host-handoff-sealed.json")
	config := filepath.Join(dir, "config.json")
	if err := aruntime.AtomicJSON(config, c); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "absent-state")
	output, err := os.CreateTemp(dir, "config-check-")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	args, stdout := os.Args, os.Stdout
	t.Cleanup(func() { os.Args, os.Stdout = args, stdout })
	os.Stdout = output
	baseArgs := []string{"autowheel", "check-config", "--config", config, "--state", state}
	os.Args = baseArgs
	if err := run(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	var report configCheckReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if !report.ConfigurationValid || report.Scope != "configuration_only" || report.BrokerVerified || report.TradingAuthorized || report.ConfigDigest != configDigest(c) || report.AccountLast4 != "5678" || report.Config.Account != "" || strings.Contains(string(raw), c.Account) {
		t.Fatal("configuration-only evidence was incorrect or disclosed the account")
	}
	for _, flag := range []string{"--live", "--live-capable", "--exclusive-account"} {
		os.Args = append(slices.Clone(baseArgs), flag)
		if err := run(); err == nil {
			t.Fatal("configuration check accepted activation flag", flag)
		}
	}
	c.MaxSymbols = 4
	if err := aruntime.AtomicJSON(config, c); err != nil {
		t.Fatal(err)
	}
	os.Args = baseArgs
	if err := run(); err == nil {
		t.Fatal("configuration check skipped native validation")
	}
	for _, path := range []string{state, c.TradePulseStateDir} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("configuration check touched financial or peer state", path, err)
		}
	}
	if after, err := output.Stat(); err != nil || after.Size() != int64(len(raw)) {
		t.Fatal("failed checks emitted success evidence", err)
	}
}
