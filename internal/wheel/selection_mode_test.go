package wheel

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

func mustConservative(t *testing.T) Config {
	t.Helper()
	c := DefaultConfig()
	c.Account = "12345678"
	out, err := c.WithSelectionMode(SelectionConservative)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustBalanced(t *testing.T) Config {
	t.Helper()
	c := DefaultConfig()
	c.Account = "12345678"
	out, err := c.WithSelectionMode(SelectionBalanced)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mkPut(id string, strike, bid, ask Cents, delta float64, now time.Time) Quote {
	return Quote{
		Option:       Option{ID: id, Symbol: "NVDA", Kind: "put", Expiry: "2026-10-09", Strike: strike},
		At:           now,
		UnderlyingAt: now,
		Spot:         21000,
		Bid:          bid,
		Ask:          ask,
		Tick:         1,
		Delta:        delta,
		OI:           1000,
		BidSize:      5,
		Standard:     true,
		Tradable:     true,
	}
}

func mkCall(id string, strike, bid, ask Cents, delta float64, now time.Time) Quote {
	return Quote{
		Option:       Option{ID: id, Symbol: "NVDA", Kind: "call", Expiry: "2026-10-06", Strike: strike},
		At:           now,
		UnderlyingAt: now,
		Spot:         21000,
		Bid:          bid,
		Ask:          ask,
		Tick:         1,
		Delta:        delta,
		OI:           1000,
		BidSize:      5,
		Standard:     true,
		Tradable:     true,
	}
}

func TestWithSelectionModeSetsExactBands(t *testing.T) {
	base := DefaultConfig()
	base.Account = "12345678"
	cons, err := base.WithSelectionMode(SelectionConservative)
	if err != nil {
		t.Fatal(err)
	}
	if cons.SelectionMode != SelectionConservative || cons.PutMinDelta != .05 || cons.PutMaxDelta != .15 {
		t.Fatalf("conservative bands: %+v", cons)
	}
	bal, err := base.WithSelectionMode(SelectionBalanced)
	if err != nil {
		t.Fatal(err)
	}
	if bal.SelectionMode != SelectionBalanced || bal.PutMinDelta != .15 || bal.PutMaxDelta != .35 {
		t.Fatalf("balanced bands: %+v", bal)
	}
	// All other settings unchanged.
	if cons.Fee != base.Fee || cons.MaxSymbols != base.MaxSymbols || cons.CallMinDelta != base.CallMinDelta ||
		cons.CallMaxDelta != base.CallMaxDelta || cons.Symbols[0] != base.Symbols[0] || len(cons.Symbols) != len(base.Symbols) {
		t.Fatal("conservative changed unrelated settings")
	}
	if bal.Fee != base.Fee || bal.CallMinDelta != base.CallMinDelta || bal.CallMaxDelta != base.CallMaxDelta {
		t.Fatal("balanced changed unrelated settings")
	}
	if err := cons.Validate(); err != nil {
		t.Fatalf("conservative validate: %v", err)
	}
	if err := bal.Validate(); err != nil {
		t.Fatalf("balanced validate: %v", err)
	}
	for _, mode := range []SelectionMode{"", "aggressive", "CONSERVATIVE"} {
		if _, err := base.WithSelectionMode(mode); err == nil {
			t.Fatalf("accepted mode %q", mode)
		}
	}
}

func TestConservativeSelectPrefersLowDeltaOverPremium(t *testing.T) {
	_, _, _, _, now := fixture()
	cons := mustConservative(t)
	low := mkPut("cons-low", 18000, 150, 160, -.06, now)
	high := mkPut("cons-high", 19500, 300, 310, -.14, now)
	// Sanity: high premium has higher day ROI, so ROI-first would pick it.
	a, ok := Select(cons, "NVDA", "put", []Quote{high, low}, 0, now)
	if !ok || a.ID != "cons-low" {
		t.Fatalf("conservative must pick lowest absolute delta, got %+v %v", a, ok)
	}
	if a.Delta != -.06 {
		t.Fatalf("wrong delta %+v", a)
	}
	// Legacy empty mode with the same custom bands keeps balanced ROI ranking.
	legacy := DefaultConfig()
	legacy.Account = "12345678"
	legacy.PutMinDelta, legacy.PutMaxDelta = .05, .15
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy custom bands rejected: %v", err)
	}
	b, ok := Select(legacy, "NVDA", "put", []Quote{low, high}, 0, now)
	if !ok || b.ID != "cons-high" {
		t.Fatalf("legacy empty mode must preserve ROI ranking, got %+v %v", b, ok)
	}
	// Preview agrees with Select for both disciplines.
	if p, ok := PreviewCandidate(cons, "NVDA", "put", []Quote{high, low}, 0, now); !ok || p.ID != "cons-low" {
		t.Fatalf("conservative preview: %+v %v", p, ok)
	}
	if p, ok := PreviewCandidate(legacy, "NVDA", "put", []Quote{low, high}, 0, now); !ok || p.ID != "cons-high" {
		t.Fatalf("legacy preview: %+v %v", p, ok)
	}
}

func TestConservativeRejectsAboveCap(t *testing.T) {
	_, _, _, _, now := fixture()
	cons := mustConservative(t)
	bal := mustBalanced(t)
	above := mkPut("above-cap", 19000, 200, 210, -.16, now)
	if _, ok := Select(cons, "NVDA", "put", []Quote{above}, 0, now); ok {
		t.Fatal("conservative accepted delta above .15")
	}
	if _, ok := PreviewCandidate(cons, "NVDA", "put", []Quote{above}, 0, now); ok {
		t.Fatal("conservative preview accepted delta above .15")
	}
	if _, ok := Select(bal, "NVDA", "put", []Quote{above}, 0, now); !ok {
		t.Fatal("balanced must accept delta .16 within .15-.35")
	}
}

func TestConservativeTieBreakDeterministic(t *testing.T) {
	_, _, _, _, now := fixture()
	cons := mustConservative(t)
	lo := mkPut("tie-lo", 18000, 120, 130, -.10, now)
	hi := mkPut("tie-hi", 19000, 250, 260, -.10, now)
	// Same delta: higher ROI wins, regardless of input order.
	for _, quotes := range [][]Quote{{lo, hi}, {hi, lo}} {
		a, ok := Select(cons, "NVDA", "put", quotes, 0, now)
		if !ok || a.ID != "tie-hi" {
			t.Fatalf("delta tie must break by ROI, got %+v %v", a, ok)
		}
	}
	// Identical economics: lexicographic ID wins deterministically.
	a1 := mkPut("put-a", 18500, 200, 210, -.10, now)
	b1 := mkPut("put-b", 18500, 200, 210, -.10, now)
	for _, quotes := range [][]Quote{{a1, b1}, {b1, a1}} {
		a, ok := Select(cons, "NVDA", "put", quotes, 0, now)
		if !ok || a.ID != "put-a" {
			t.Fatalf("identical tie must pick put-a, got %+v %v", a, ok)
		}
	}
}

func TestPlanConservativeVsBalancedOrdering(t *testing.T) {
	c, st, s, _, now := fixture()
	c.Symbols = []string{"NVDA", "TSLA"} // Explicit synthetic universe, independent of deployment defaults.
	cons, err := c.WithSelectionMode(SelectionConservative)
	if err != nil {
		t.Fatal(err)
	}
	bal, err := c.WithSelectionMode(SelectionBalanced)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(symbol, id string, delta, rpd float64, collateral Cents) Candidate {
		return Candidate{
			Quote: Quote{
				Option:       Option{ID: id, Symbol: symbol, Kind: "put"},
				At:           now,
				UnderlyingAt: now,
				Delta:        delta,
			},
			ReturnPerDay: rpd,
			Collateral:   collateral,
		}
	}
	base := []Candidate{
		mk("NVDA", "low-risk", -.06, 0.002, 1800000),
		mk("TSLA", "high-prem", -.14, 0.005, 1900000),
	}
	consPlan, err := Plan(cons, st, s, slices.Clone(base), now)
	if err != nil || len(consPlan) != 2 || consPlan[0].Symbol != "NVDA" || consPlan[1].Symbol != "TSLA" {
		t.Fatalf("conservative plan order: %+v %v", consPlan, err)
	}
	balancedCandidates := slices.Clone(base)
	balancedCandidates[0].Delta, balancedCandidates[1].Delta = -.16, -.30
	balPlan, err := Plan(bal, st, s, slices.Clone(balancedCandidates), now)
	if err != nil || len(balPlan) != 2 || balPlan[0].Symbol != "TSLA" || balPlan[1].Symbol != "NVDA" {
		t.Fatalf("balanced plan order: %+v %v", balPlan, err)
	}
	// Empty legacy mode matches balanced ROI ordering.
	legacyPlan, err := Plan(c, st, s, balancedCandidates, now)
	if err != nil || len(legacyPlan) != 2 || legacyPlan[0].Symbol != "TSLA" {
		t.Fatalf("legacy plan order: %+v %v", legacyPlan, err)
	}
}

func TestCallsRetainedBothModes(t *testing.T) {
	_, _, _, _, now := fixture()
	cons := mustConservative(t)
	bal := mustBalanced(t)
	hi := mkCall("call-high", 20500, 600, 620, .70, now)
	lo := mkCall("call-low", 21000, 700, 710, .55, now)
	for _, cfg := range []Config{cons, bal} {
		a, ok := Select(cfg, "NVDA", "call", []Quote{lo, hi}, 22000, now)
		if !ok || a.ID != "call-high" {
			t.Fatalf("mode %q must keep delta-first calls, got %+v %v", cfg.SelectionMode, a, ok)
		}
		if p, ok := PreviewCandidate(cfg, "NVDA", "call", []Quote{lo, hi}, 22000, now); !ok || p.ID != "call-high" {
			t.Fatalf("mode %q preview calls: %+v %v", cfg.SelectionMode, p, ok)
		}
	}
	// Plan keeps call allocation priority in both modes with assigned inventory.
	for _, cfg := range []Config{cons, bal} {
		_, st, s, _, n := fixture()
		cfg.Account = "12345678"
		cfg.Symbols = []string{"NVDA", "TSLA"}
		st.Positions["NVDA"] = Position{OwnedShares: 100, Cost: 20000, Provenance: "own assignment"}
		s.Shares = []Shares{{Symbol: "NVDA", Quantity: 100, Available: 100, Cost: 20000}}
		call := Candidate{
			Quote:        Quote{Option: Option{ID: "own-call", Symbol: "NVDA", Kind: "call"}, At: n, UnderlyingAt: n, Delta: .70},
			ReturnPerDay: 0.001,
		}
		put := Candidate{
			Quote:        Quote{Option: Option{ID: "new-put", Symbol: "TSLA", Kind: "put"}, At: n, UnderlyingAt: n, Delta: -.06},
			ReturnPerDay: 0.01,
			Collateral:   1800000,
		}
		if cfg.SelectionMode == SelectionBalanced {
			put.Delta = -.20
		}
		plan, err := Plan(cfg, st, s, []Candidate{put, call}, n)
		if err != nil || len(plan) != 2 || plan[0].Kind != "call" || plan[1].Kind != "put" {
			t.Fatalf("mode %q call priority: %+v %v", cfg.SelectionMode, plan, err)
		}
	}
}

func TestUnknownSelectionModeRejected(t *testing.T) {
	c, st, s, q, now := fixture()
	bad := c
	bad.SelectionMode = SelectionMode("aggressive")
	if err := bad.Validate(); err == nil {
		t.Fatal("unknown mode accepted by Validate")
	}
	if _, ok := Select(bad, q.Symbol, "put", []Quote{q}, 0, now); ok {
		t.Fatal("unknown mode accepted by Select")
	}
	if _, ok := PreviewCandidate(bad, q.Symbol, "put", []Quote{q}, 0, now); ok {
		t.Fatal("unknown mode accepted by Preview")
	}
	a, _ := Select(c, q.Symbol, "put", []Quote{q}, 0, now)
	if _, err := Plan(bad, st, s, []Candidate{a}, now); err == nil {
		t.Fatal("unknown mode accepted by Plan")
	}
}

func TestExplicitInconsistentBandsRejected(t *testing.T) {
	base := DefaultConfig()
	base.Account = "12345678"
	consWrong := base
	consWrong.SelectionMode = SelectionConservative
	consWrong.PutMinDelta, consWrong.PutMaxDelta = .15, .35
	if err := consWrong.Validate(); err == nil {
		t.Fatal("conservative with balanced bands accepted")
	}
	balWrong := base
	balWrong.SelectionMode = SelectionBalanced
	balWrong.PutMinDelta, balWrong.PutMaxDelta = .05, .15
	if err := balWrong.Validate(); err == nil {
		t.Fatal("balanced with conservative bands accepted")
	}
	// Bypassed Validate must still not select or allocate. Each quote lies
	// inside its config's stated bands, so only the preset gate can reject it.
	_, _, _, _, now := fixture()
	qCons := mkPut("inconsistent-cons", 19000, 200, 210, -.20, now)
	if _, ok := Select(consWrong, "NVDA", "put", []Quote{qCons}, 0, now); ok {
		t.Fatal("inconsistent conservative bands selected")
	}
	qBal := mkPut("inconsistent-bal", 18000, 150, 160, -.10, now)
	if _, ok := Select(balWrong, "NVDA", "put", []Quote{qBal}, 0, now); ok {
		t.Fatal("inconsistent balanced bands selected")
	}
	// Finite delta checks retained.
	for _, tc := range []Config{
		func() Config { c := base; c.PutMinDelta = math.NaN(); return c }(),
		func() Config { c := base; c.PutMaxDelta = math.Inf(1); return c }(),
		func() Config { c := base; c.PutMinDelta = 0; return c }(),
	} {
		if err := tc.Validate(); err == nil {
			t.Fatalf("non-finite or zero put band accepted: %+v", tc)
		}
	}
}

func TestEmptyModeLegacySerialization(t *testing.T) {
	def := DefaultConfig()
	if def.SelectionMode != "" {
		t.Fatalf("DefaultConfig must stay empty, got %q", def.SelectionMode)
	}
	if got := def.EffectiveSelectionMode(); got != SelectionBalanced {
		t.Fatalf("empty effective mode = %q, want balanced", got)
	}
	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "selection_mode") {
		t.Fatalf("empty mode must omit selection_mode JSON: %s", raw)
	}
	// Old JSON without the field decodes to empty and behaves as balanced.
	var decoded Config
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SelectionMode != "" || decoded.EffectiveSelectionMode() != SelectionBalanced {
		t.Fatalf("decoded legacy: %+v", decoded)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("legacy baseline must validate: %v", err)
	}
	// Empty mode preserves custom legacy bands instead of clamping.
	legacy := DefaultConfig()
	legacy.Account = "12345678"
	legacy.PutMinDelta, legacy.PutMaxDelta = .05, .15
	if err := legacy.Validate(); err != nil {
		t.Fatalf("empty custom bands rejected: %v", err)
	}
	if got := legacy.EffectiveSelectionMode(); got != SelectionBalanced {
		t.Fatalf("legacy effective = %q", got)
	}
}
