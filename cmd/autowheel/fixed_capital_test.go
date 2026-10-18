package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aruntime "autowheel/internal/runtime"
)

func TestCapitalRebindRequiresPauseLockAndImmutableSource(t *testing.T) {
	config, _, state, _ := fixedCycleFixture(t)
	// This command uses the wall clock; keep the fixture binding in the past.
	state.Fixed.BoundAt = state.Fixed.BoundAt.AddDate(0, 0, -7)
	state.Fixed.Spec.FeeKnownAt = state.Fixed.Spec.FeeKnownAt.AddDate(0, 0, -7)
	config.FixedPolicy.Spec.FeeKnownAt = state.Fixed.Spec.FeeKnownAt
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
	before, err := os.ReadFile(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	config.FixedPolicy.Spec.PhysicalFeeBudget = 1000
	config.MaxCollateral = 20_000_000
	config.FixedPolicy.Spec.Limits.Account, config.FixedPolicy.Spec.Limits.Symbol = 20_000_000, 20_000_000
	var output bytes.Buffer
	if rebindCapital(config, dir, &output) == nil {
		t.Fatal("capital changed without durable pause")
	}
	if err = aruntime.AtomicJSON(filepath.Join(dir, "HALT"), map[string]string{"reason": "reviewed capital transition"}); err != nil {
		t.Fatal(err)
	}
	if err = aruntime.AtomicJSON(filepath.Join(dir, "LIVE.json"), map[string]string{"release": "old"}); err != nil {
		t.Fatal(err)
	}
	if rebindCapital(config, dir, &output) == nil {
		t.Fatal("capital changed with live authorization")
	}
	if err = os.Remove(filepath.Join(dir, "LIVE.json")); err != nil {
		t.Fatal(err)
	}
	j, _, err = aruntime.OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rebindCapital(config, dir, &output) == nil {
		t.Fatal("capital changed while another controller held its journal lock")
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "fixed-capital-archive")
	if err = os.Symlink(t.TempDir(), archive); err != nil {
		t.Fatal(err)
	}
	if rebindCapital(config, dir, &output) == nil {
		t.Fatal("capital source followed an archive directory symlink")
	}
	if err = os.Remove(archive); err != nil {
		t.Fatal(err)
	}
	if err = rebindCapital(config, dir, &output); err != nil {
		t.Fatal(err)
	}
	pin := sha256.Sum256(before)
	raw, err := os.ReadFile(filepath.Join(archive, hex.EncodeToString(pin[:])+".json"))
	if err != nil || !bytes.Equal(raw, before) || strings.Contains(output.String(), config.Account) {
		t.Fatal("capital source was not archived exactly or output disclosed the account", err)
	}
	j, loaded, err := aruntime.OpenJournal(dir)
	if err != nil {
		t.Fatal("capital journal failed real reopen", err)
	}
	defer j.Close()
	if loaded.Fixed.PolicySHA != config.FixedPolicy.Spec.Digest() || loaded.Fixed.Spec.Limits.Symbol != 20_000_000 {
		t.Fatal("reopened journal lost exact financial binding")
	}
	if _, err = os.Stat(filepath.Join(dir, "HALT")); err != nil {
		t.Fatal("capital rebind cleared pause")
	}
	if _, err = os.Lstat(filepath.Join(dir, "LIVE.json")); !os.IsNotExist(err) {
		t.Fatal("capital rebind authorized trading")
	}
}
