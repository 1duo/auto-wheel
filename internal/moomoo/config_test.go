package moomoo

import (
	"os"
	"path/filepath"
	"testing"

	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

func TestFiveSlotTargetPreservesAccountAndCapital(t *testing.T) {
	c := DefaultConfig()
	c.AccountID, c.Strategy.Account = "123456", "123456"
	c.ExecutionPolicy = "" // Existing observation configuration before migration.
	c.Strategy.MaxSymbols = 3
	c.Strategy, _ = c.Strategy.WithStrategyPreset("conservative-roi-v1")
	c.Strategy.Reserve = 12345
	next, err := c.WithFiveSlotPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if next.ExecutionPolicy != wheel.FixedConservativeASAPPolicy || next.Strategy.MaxSymbols != 5 ||
		next.AccountID != c.AccountID || next.Strategy.Account != c.Strategy.Account || next.Strategy.Reserve != c.Strategy.Reserve ||
		next.Strategy.MaxCollateral != c.Strategy.MaxCollateral || next.Strategy.Fee != c.Strategy.Fee || next.Strategy.FixedPolicy != nil {
		t.Fatal("five-slot target changed account/capital or invented broker financial evidence")
	}
	if next.Strategy.SymbolCollateralLimit() != next.Strategy.MaxCollateral/5 {
		t.Fatal("five-slot generic collateral limit is unavailable")
	}
	for _, slots := range []int{0, 4, 6} {
		bad := next
		bad.Strategy.MaxSymbols = slots
		if bad.Validate() == nil {
			t.Fatal("target policy accepted a different slot count")
		}
	}
	bad := next
	bad.Strategy.StrategyPreset = "conservative-roi-v1"
	if bad.Validate() == nil {
		t.Fatal("old ROI policy masqueraded as the five-slot ASAP target")
	}
}

func TestIndependentDeploymentBinding(t *testing.T) {
	c := DefaultConfig()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	j, err := OpenState(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenState(dir, c); err == nil {
		t.Fatal("second observer obtained deployment lock")
	}
	j.Close()
	c.AccountID, c.Strategy.Account = "123456", "123456"
	j, err = OpenState(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	j.Close()
	c.AccountID, c.Strategy.Account = "654321", "654321"
	if _, err := OpenState(dir, c); err == nil {
		t.Fatal("another account reused deployment state")
	}
	other := t.TempDir()
	if err := os.Chmod(other, 0700); err != nil {
		t.Fatal(err)
	}
	if err := aruntime.AtomicJSON(filepath.Join(other, "robinhood.token.json"), "do not read"); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenState(other, DefaultConfig()); err == nil {
		t.Fatal("Robinhood state appropriated")
	}
	if _, err := os.Stat(filepath.Join(other, "moomoo-deployment.json")); !os.IsNotExist(err) {
		t.Fatal("rejected state was marked as Moomoo")
	}
	c = DefaultConfig()
	c.Strategy.CoordinatorSocket = "/tmp/coordinator"
	if c.Validate() == nil {
		t.Fatal("shared coordinator admitted")
	}
}
