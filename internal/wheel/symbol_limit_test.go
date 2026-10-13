package wheel

import (
	"testing"
	"time"
)

func TestSymbolLimitFiltersBeforeRankingAndAgainAtAdmission(t *testing.T) {
	c, st, s, _, now := fixture()
	c, _ = c.WithStrategyPreset("conservative-roi-v1")
	if c.SymbolCollateralLimit() != 2_666_666 {
		t.Fatal("default symbol limit must floor one third of the fixed account ceiling")
	}
	large := mkPut("oversized-high-roi", 27000, 400, 410, -.14, now)
	large.Spot = 30000
	small := mkPut("funded", 26000, 200, 210, -.12, now)
	small.Spot = 30000
	a, ok := Select(c, "NVDA", "put", []Quote{large, small}, 0, now)
	if !ok || a.ID != small.ID {
		t.Fatal("oversized best yield hid a funded lower-ranked contract")
	}
	// A candidate retained from an earlier scan cannot bypass admission.
	a.Quote = large
	a.Collateral = large.Strike * 100
	if got, err := Plan(c, st, s, []Candidate{a}, now); err != nil || len(got) != 0 {
		t.Fatal("oversized retained candidate admitted", err)
	}
}

func TestSymbolLimitIncludesFeeAtExactBoundary(t *testing.T) {
	c, st, s, q, now := fixture()
	c.MaxCollateral = 6_000_300 // Three slots of $20,001, including the $1 entry fee.
	a, ok := Select(c, q.Symbol, "put", []Quote{q}, 0, now)
	if !ok {
		t.Fatal("exactly funded contract rejected")
	}
	if got, err := Plan(c, st, s, []Candidate{a}, now); err != nil || len(got) != 1 {
		t.Fatal("exact boundary rejected at admission", err)
	}
	c.MaxCollateral--
	if _, ok := Select(c, q.Symbol, "put", []Quote{q}, 0, now); ok {
		t.Fatal("entry fee exceeded symbol budget")
	}
	if got, err := Plan(c, st, s, []Candidate{a}, now); err != nil || len(got) != 0 {
		t.Fatal("admission ignored the fee or changed ceiling", err)
	}
	if FitsSymbolCollateral(MaxMoney, 1, MaxMoney) || FitsSymbolCollateral(1, -1, 100) || FitsSymbolCollateral(1, 1, 0) {
		t.Fatal("invalid or overflowing budget admitted")
	}
}

func TestSymbolLimitKeepsCoveredExitForExistingOversizedStock(t *testing.T) {
	c, st, s, _, now := fixture()
	c, _ = c.WithStrategyPreset("conservative-roi-v1")
	st.Positions["NVDA"] = Position{OwnedShares: 100, Cost: 40000, Provenance: "owned-receipt"}
	s.Shares = []Shares{{Symbol: "NVDA", Quantity: 100, Available: 100, Cost: 40000}}
	q := mkCall("covered-exit", 35000, 1050, 1060, .70, now)
	q.Spot = 36000
	a, ok := Select(c, "NVDA", "call", []Quote{q}, 40000, now)
	if !ok || a.CallAwayPnL >= 0 {
		t.Fatal("existing below-cost covered exit was blocked or its loss hidden")
	}
	if got, err := Plan(c, st, s, []Candidate{a}, now); err != nil || len(got) != 1 {
		t.Fatal("symbol entry cap stranded existing owned stock", err)
	}
}

func TestSymbolLimitStopsSubmissionAfterCeilingChanges(t *testing.T) {
	c, st, s, q, now := fixture()
	a, ok := Select(c, q.Symbol, "put", []Quote{q}, 0, now)
	if !ok {
		t.Fatal("initial candidate invalid")
	}
	c.MaxCollateral = 3_000_000
	b := &fakeBroker{snapshot: s, quotes: []Quote{q}}
	if err := Execute(t.Context(), c, &st, &fakeStore{}, b, a, func() time.Time { return now }); err == nil || b.submissions != 0 || len(st.Intents) != 0 {
		t.Fatal("a candidate from an earlier ceiling reached submission", err)
	}
}
