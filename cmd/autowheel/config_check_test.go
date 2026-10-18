package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

func TestTransitionCheckIsOfflineAndDoesNotRewriteInventory(t *testing.T) {
	dir := t.TempDir()
	c := wheel.DefaultConfig()
	c.Account = "12345678"
	config := filepath.Join(dir, "config.json")
	if err := aruntime.AtomicJSON(config, c); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	at := time.Date(2026, 10, 16, 14, 7, 0, 0, wheel.Eastern)
	st := wheel.NewState()
	st.Account = c.Account
	st.Positions["HOOD"] = wheel.Position{PutID: "own-put", PutExpiry: "2026-10-23", PutStrike: 8500,
		PutOpenedAt: at, Provenance: "autowheel put fill own-ref"}
	st.CompletedOrders = map[string]wheel.OrderRecord{"own-order": {Ref: "own-ref", Symbol: "HOOD", OptionID: "own-put", At: at, Filled: 1}}
	if err := aruntime.AtomicJSON(filepath.Join(state, "journal.json"), st); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(state, "journal.json"))
	output, err := os.CreateTemp(dir, "transition-check-")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	args, stdout := os.Args, os.Stdout
	t.Cleanup(func() { os.Args, os.Stdout = args, stdout })
	os.Stdout = output
	baseArgs := []string{"autowheel", "check-transition", "--config", config, "--state", state}
	os.Args = baseArgs
	if err := run(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(output.Name())
	var report struct {
		Digest string `json:"legacy_state_sha256"`
		Broker bool   `json:"broker_verified"`
		Trade  bool   `json:"trading_authorized"`
	}
	want, _ := wheel.FixedLegacyDigest(st)
	if json.Unmarshal(raw, &report) != nil || report.Digest != want || report.Broker || report.Trade ||
		strings.Contains(string(raw), c.Account) || strings.Contains(string(raw), "own-ref") {
		t.Fatal("transition check changed its binding, authority or privacy")
	}
	after, _ := os.ReadFile(filepath.Join(state, "journal.json"))
	if string(before) != string(after) {
		t.Fatal("offline check rewrote the journal")
	}
	for _, flag := range []string{"--live", "--live-capable", "--exclusive-account"} {
		os.Args = append(slices.Clone(baseArgs), flag)
		if err := run(); err == nil {
			t.Fatal("transition inspection accepted an activation flag")
		}
	}
}

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
	if report.SymbolCollateral != 2_666_666 {
		t.Fatal("configuration inspection omitted or changed the fixed symbol limit")
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

func TestConfigRequiresPrivateRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := aruntime.AtomicJSON(path, wheel.DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("public configuration accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(link); err == nil {
		t.Fatal("configuration symlink accepted")
	}
}
