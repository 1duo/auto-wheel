package contractpolicy

import "testing"

func TestOpeningAdmissionBoundaries(t *testing.T) {
	for _, b := range []Book{{100, 117}, {0, 100}, {101, 100}, {-1, 100}, {maxPrice, maxPrice + 1}} {
		if EntryBookUsable(b) {
			t.Fatal("invalid or wide book admitted", b)
		}
	}
	for _, b := range []Book{{100, 115}, {37, 43}, {maxPrice, maxPrice}} {
		if !EntryBookUsable(b) {
			t.Fatal("valid boundary book rejected", b)
		}
	}
	for _, fee := range []int64{0, 100, 200} {
		for _, delta := range []int64{0, 1} {
			ok, err := StockCostCallAllowed(10000, 1_000_000-fee+delta, fee)
			if err != nil || ok != (delta == 0) {
				t.Fatal("one-cent acquisition/delivery floor changed", fee, delta, ok, err)
			}
		}
	}
	for _, args := range [][3]int64{{0, 1, 0}, {1, 0, 0}, {1, 1, -1}, {maxPrice, maxCapital, 1}} {
		if _, err := StockCostCallAllowed(args[0], args[1], args[2]); err == nil {
			t.Fatal("unknown or overflowing floor accepted", args)
		}
	}
	l := CapitalLimits{Account: 8_000_000, Symbol: 2_666_666, Reserve: 1_680_944}
	u := CapitalUse{Available: 2_681_044, NewCollateral: 1_000_000, Debit: 100, Addition: 1_000_000, ActiveSymbols: 2, NewSymbol: true}
	if !CanOpen(l, u) {
		t.Fatal("funded third slot rejected")
	}
	u.Available--
	if CanOpen(l, u) {
		t.Fatal("opening spent a cent of reserve")
	}
	u.Available++
	u.ActiveSymbols = 3
	if CanOpen(l, u) {
		t.Fatal("fourth symbol admitted")
	}
	// A covered call consumes no new collateral, but still funds its fee from
	// existing cash. Future option premiums must not finance that fee.
	u = CapitalUse{Available: l.Reserve + 100, Debit: 100, AccountExposure: l.Account, SymbolExposure: l.Symbol, ActiveSymbols: 3}
	if !CanOpen(l, u) {
		t.Fatal("funded existing-lot call at both exposure limits rejected")
	}
	for _, mutate := range []func(*CapitalUse){func(u *CapitalUse) { u.Available-- }, func(u *CapitalUse) { u.AccountExposure++ },
		func(u *CapitalUse) { u.SymbolExposure++ }, func(u *CapitalUse) { u.Available = maxCapital + 1 }, func(u *CapitalUse) { u.Debit = -1 }} {
		bad := u
		mutate(&bad)
		if CanOpen(l, bad) {
			t.Fatal("invalid cash/exposure admitted", bad)
		}
	}
	if CanOpen(CapitalLimits{}, u) {
		t.Fatal("missing limits interpreted as unlimited")
	}
}
