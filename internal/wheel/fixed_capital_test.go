package wheel

import (
	"encoding/json"
	"testing"
	"time"
)

func TestFixedCapitalRebindPreservesOwnedLifecyclesAndRollback(t *testing.T) {
	for _, kind := range []string{"put", "stock", "covered_call"} {
		t.Run(kind, func(t *testing.T) {
			control, st, _, config, snapshot, now := legacyImportFixture(t, kind != "put", kind == "covered_call")
			if err := control.BindNative(config, snapshot, now); err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(st)
			old := st.Fixed.Spec
			config.FixedPolicy.Spec.PhysicalFeeBudget = 1000
			config.MaxCollateral = 20_000_000
			config.FixedPolicy.Spec.Limits.Account, config.FixedPolicy.Spec.Limits.Symbol = 20_000_000, 20_000_000
			transition := now.Add(time.Minute)
			config.FixedPolicy.Spec.FeeKnownAt = now
			next, err := RebindFixedCapital(*st, config, transition)
			if err != nil {
				t.Fatal(err)
			}
			if config.SymbolCollateralLimit() != 20_000_000 {
				t.Fatal("fixed policy retained an independent per-symbol allocation")
			}
			loaded, err := NewFixedController(config.FixedPolicy.Spec, &next, new(fixedMemoryJournal))
			if err != nil {
				t.Fatal("new capital policy did not survive restart", err)
			}
			snapshot.At = transition
			if err = loaded.ReconcileNative(config, snapshot, transition); err != nil {
				t.Fatal("new cap broke existing ownership reconciliation", err)
			}
			config.MaxCollateral = Cents(old.Limits.Account)
			old.FeeKnownAt = next.Fixed.Spec.FeeKnownAt
			config.FixedPolicy.Spec = old
			rolledBack, err := RebindFixedCapital(next, config, transition.Add(time.Minute))
			if err != nil {
				t.Fatal("compatible paused rollback lost ownership", err)
			}
			if !next.Fixed.BoundAt.Equal(transition) || !rolledBack.Fixed.BoundAt.Equal(transition.Add(time.Minute)) {
				t.Fatal("financial rebinding lost its actual transition clock")
			}
			rolledBack.Fixed.BoundAt = st.Fixed.BoundAt
			rolledBack.Fixed.Spec = st.Fixed.Spec
			after, _ := json.Marshal(rolledBack)
			unchanged, _ := json.Marshal(st)
			if string(before) != string(after) || string(before) != string(unchanged) {
				t.Fatal("capital transition changed inventory, receipts or import source")
			}
		})
	}
}

func TestFixedCapitalRebindRejectsOtherFinancialChanges(t *testing.T) {
	for _, change := range []string{"fee", "reserve", "import", "batch"} {
		t.Run(change, func(t *testing.T) {
			control, st, _, config, snapshot, now := legacyImportFixture(t, false, false)
			if err := control.BindNative(config, snapshot, now); err != nil {
				t.Fatal(err)
			}
			config.FixedPolicy.Spec.PhysicalFeeBudget = 1000
			config.MaxCollateral = 20_000_000
			config.FixedPolicy.Spec.Limits.Account, config.FixedPolicy.Spec.Limits.Symbol = 20_000_000, 20_000_000
			switch change {
			case "fee":
				config.Fee++
				config.FixedPolicy.Spec.OptionFeeBudget++
			case "reserve":
				config.Reserve++
				config.FixedPolicy.Spec.Limits.Reserve++
			case "import":
				config.FixedPolicy.ImportLegacyStateSHA = ""
			case "batch":
				if err := control.Begin(config.FixedPolicy.Roster, now.AddDate(0, 0, 7).Add(-time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := RebindFixedCapital(*st, config, now); err == nil {
				t.Fatal("capital command crossed another policy or pending-signal boundary")
			}
		})
	}
}
