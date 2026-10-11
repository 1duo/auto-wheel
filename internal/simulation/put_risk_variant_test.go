package simulation

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"autowheel/internal/wheel"
)

func putRiskSpec(t *testing.T) Spec {
	t.Helper()
	var spec Spec
	if err := json.NewDecoder(AccountingFixture()).Decode(&spec); err != nil {
		t.Fatal(err)
	}
	spec.ResearchVariant = ResearchPutRiskCap
	spec.Config.PutMinDelta, spec.Config.PutMaxDelta = 0, .15
	return spec
}

func putRiskReplay(t *testing.T) (*replay, Frame) {
	t.Helper()
	spec := putRiskSpec(t)
	r := &replay{report: Report{Spec: spec}}
	if err := r.init(); err != nil {
		t.Fatal(err)
	}
	f := fixtureFrame(r.decisions[0], "decision")
	f.CompleteSymbols = spec.Config.Symbols
	f.Quotes[0].Delta = -.15
	return r, f
}

func TestPutRiskVariantIsolation(t *testing.T) {
	for _, cap := range []float64{.10, .15, .20} {
		for _, fee := range []wheel.Cents{100, 200} {
			spec := putRiskSpec(t)
			spec.Config.PutMaxDelta, spec.Config.Fee = cap, fee
			r := &replay{report: Report{Spec: spec}}
			if err := r.init(); err != nil {
				t.Fatalf("allowed research cap=%v fee=%v: %v", cap, fee, err)
			}
			if spec.Config.Validate() == nil {
				t.Fatal("research zero floor became a valid production config")
			}
		}
	}
	for name, mutate := range map[string]func(*Spec){
		"unnamed_zero_floor": func(s *Spec) { s.ResearchVariant = "" },
		"unknown_variant":    func(s *Spec) { s.ResearchVariant = "unknown" },
		"variant_case":       func(s *Spec) { s.ResearchVariant = strings.ToUpper(ResearchPutRiskCap) },
		"variant_whitespace": func(s *Spec) { s.ResearchVariant += " " },
		"baseline_band":      func(s *Spec) { s.Config.PutMinDelta, s.Config.PutMaxDelta = .15, .35 },
		"positive_floor":     func(s *Spec) { s.Config.PutMinDelta = .01 },
		"negative_floor":     func(s *Spec) { s.Config.PutMinDelta = -.01 },
		"zero_cap":           func(s *Spec) { s.Config.PutMaxDelta = 0 },
		"unlisted_cap":       func(s *Spec) { s.Config.PutMaxDelta = .25 },
		"nan_floor":          func(s *Spec) { s.Config.PutMinDelta = math.NaN() },
		"nan_cap":            func(s *Spec) { s.Config.PutMaxDelta = math.NaN() },
		"infinite_cap":       func(s *Spec) { s.Config.PutMaxDelta = math.Inf(1) },
		"nan_call":           func(s *Spec) { s.Config.CallMaxDelta = math.NaN() },
		"production_account": func(s *Spec) { s.Config.Account = "1234" },
		"peer_state":         func(s *Spec) { s.Config.TradePulseStateDir = "/tmp/research-forbidden-peer" },
		"coordinator": func(s *Spec) {
			s.Config.CoordinatorSocket = "/tmp/research-forbidden-coordinator"
			s.Config.CoordinatorPolicy = strings.Repeat("0", 64)
		},
		"slot_drift":      func(s *Spec) { s.Config.MaxSymbols = 2 },
		"call_drift":      func(s *Spec) { s.Config.CallMinDelta = .6 },
		"liquidity_drift": func(s *Spec) { s.Config.MinOI++ },
		"fee_drift":       func(s *Spec) { s.Config.Fee = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			spec := putRiskSpec(t)
			mutate(&spec)
			if err := (&replay{report: Report{Spec: spec}}).init(); err == nil {
				t.Fatal("invalid or unrelated configuration change admitted")
			}
		})
	}
	var baseline Spec
	if err := json.NewDecoder(AccountingFixture()).Decode(&baseline); err != nil {
		t.Fatal(err)
	}
	if err := (&replay{report: Report{Spec: baseline}}).init(); err != nil {
		t.Fatal("frozen baseline rejected", err)
	}
	if baseline.Config.PutMinDelta != .15 || baseline.Config.PutMaxDelta != .35 {
		t.Fatal("baseline delta limits changed")
	}
}

func TestPutRiskVariantBoundaryAndPreSubmit(t *testing.T) {
	for name, delta := range map[string]float64{
		"cap_equality":  -.15,
		"low_delta":     -.01,
		"over_cap":      -.150000001,
		"zero":          0,
		"negative_zero": math.Copysign(0, -1),
		"positive":      .01,
		"nan":           math.NaN(),
		"positive_inf":  math.Inf(1),
		"negative_inf":  math.Inf(-1),
	} {
		t.Run(name, func(t *testing.T) {
			r, f := putRiskReplay(t)
			f.Quotes[0].Delta = delta
			if err := r.step(f); err != nil {
				t.Fatal(err)
			}
			eligible := delta < 0 && math.Abs(delta) <= .15
			if (r.metrics.Submissions == 1) != eligible || (r.pending != nil) != eligible {
				t.Fatal("risk boundary bypassed selection or pre-submit checks")
			}
			if eligible && (r.pending.Candidate.ID != f.Quotes[0].ID || r.pending.Candidate.Delta != delta) {
				t.Fatal("pre-submit reranking changed the verified candidate")
			}
		})
	}
}

func TestPutRiskVariantNetYieldRanking(t *testing.T) {
	r, f := putRiskReplay(t)
	lowRisk := f.Quotes[0]
	lowRisk.ID += "-low-risk-higher-yield"
	lowRisk.Delta, lowRisk.Strike = -.08, 9000
	// Same premium, less collateral: choose higher net yield within the cap.
	f.Quotes = append(f.Quotes, lowRisk)
	for i := range f.Chains {
		if f.Chains[i].Symbol == lowRisk.Symbol && f.Chains[i].Kind == "put" {
			f.Chains[i].Contracts = append(f.Chains[i].Contracts, lowRisk.ID)
		}
	}
	if err := r.step(f); err != nil {
		t.Fatal(err)
	}
	if r.pending == nil || r.pending.Candidate.ID != lowRisk.ID {
		t.Fatal("forced the delta boundary instead of maximizing eligible yield")
	}
}

func TestPutRiskVariantNearestExpiryAndGreekGates(t *testing.T) {
	t.Run("nearest_over_cap", func(t *testing.T) {
		r, f := putRiskReplay(t)
		later := f.Quotes[0]
		later.ID += "-later"
		later.Delta = -.10
		f.Quotes[0].Expiry, f.Quotes[0].Delta = "2026-04-09", -.20
		for i := range f.Chains {
			if f.Chains[i].Symbol == "NVDA" && f.Chains[i].Kind == "put" {
				f.Chains[i].Expiry = "2026-04-09"
			}
		}
		f.Quotes = append(f.Quotes, later)
		f.Chains = append(f.Chains, Chain{Symbol: "NVDA", Kind: "put", Expiry: later.Expiry, At: f.At, Weekly: true, Contracts: []string{later.ID}})
		if err := r.step(f); err != nil || r.metrics.Submissions != 0 {
			t.Fatal("risk failure advanced to a later expiry", err)
		}
	})
	for _, delta := range []float64{-.01, -.20} {
		r, f := putRiskReplay(t)
		f.Quotes[0].Delta, f.Quotes[0].GreeksAt = delta, time.Time{}
		if err := r.step(f); err == nil || !strings.Contains(err.Error(), "selection Greeks") || r.metrics.Submissions != 0 {
			t.Fatal("unverified delta was used for inclusion or exclusion")
		}
	}
}

func TestPutRiskVariantPreservesCoveredCallExit(t *testing.T) {
	r, f := putRiskReplay(t)
	r.state.Positions["NVDA"] = wheel.Position{OwnedShares: 100, Cost: 10000, InventoryAt: f.At.Add(-48 * time.Hour), Provenance: "invented put-risk variant call test"}
	r.shares["NVDA"] = inventory{Cost: 10000, SettledAt: f.At}
	if err := r.step(f); err != nil {
		t.Fatal(err)
	}
	if r.pending == nil || r.pending.Candidate.ID != "fixture-NVDA-call" || r.pending.Candidate.Delta != .85 || r.pending.Candidate.Strike >= 10000 {
		t.Fatal("put risk cap constrained the aggressive below-cost covered call")
	}
}

func TestPutRiskVariantSyntheticLifecycle(t *testing.T) {
	input := mutateFixture(t, func(i int, v map[string]any) {
		if i == 0 {
			v["research_variant"] = ResearchPutRiskCap
			config := v["strategy_config"].(map[string]any)
			config["put_min_delta"], config["put_max_delta"] = 0, .15
			return
		}
		for _, q := range v["quotes"].([]any) {
			quote := q.(map[string]any)
			if quote["kind"] == "put" {
				quote["delta"] = -.15
			}
		}
	})
	r := Replay(input)
	if r.Status != "fixture" || r.Performance == nil || r.Spec.ResearchVariant != ResearchPutRiskCap {
		t.Fatal("synthetic variant failed or lost its study identity", r.Blockers)
	}
	m := r.Performance
	if m.Fills != 2 || m.Assignments != 1 || m.CallAways != 1 || m.Premiums != 40000 || m.Fees != 200 || m.StockRealized != -50000 || m.NetPnL != -10200 || m.EndingEquity != 7989800 {
		t.Fatalf("synthetic variant lifecycle accounting: %+v", m)
	}
	if r.EndState == nil || len(r.EndState.Positions) != 0 || r.ObservedDecisions != 370 || r.ObservedValuations != 5 {
		t.Fatal("variant lost physical ownership/coverage checks")
	}
}

func TestPutRiskVariantDeterministicYieldTie(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		r, f := putRiskReplay(t)
		other := f.Quotes[0]
		other.ID = "aaa-invented-yield-tie"
		other.Delta = -.10
		f.Quotes = append(f.Quotes, other)
		for i := range f.Chains {
			if f.Chains[i].Symbol == other.Symbol && f.Chains[i].Kind == "put" {
				f.Chains[i].Contracts = append(f.Chains[i].Contracts, other.ID)
			}
		}
		if reverse {
			f.Quotes[0], f.Quotes[len(f.Quotes)-1] = f.Quotes[len(f.Quotes)-1], f.Quotes[0]
		}
		if err := r.step(f); err != nil || r.pending == nil || r.pending.Candidate.ID != other.ID {
			t.Fatal("existing deterministic yield tie changed with input order", err)
		}
	}
}

func TestPutRiskVariantOmissionProofConfigBinding(t *testing.T) {
	for _, sourceConfig := range []string{"matching", "baseline", "other_cap"} {
		t.Run(sourceConfig, func(t *testing.T) {
			r, f := putRiskReplay(t)
			omitIDs(t, &f, callIDs(f), SelectionReasonInactiveKind)
			setValidOmissionProof(t, r, &f)
			if sourceConfig != "matching" {
				c := selectionBoundConfig(r)
				if sourceConfig == "baseline" {
					c.PutMinDelta, c.PutMaxDelta = .15, .35
				} else {
					c.PutMaxDelta = .10
				}
				f.SelectionProof.ConfigSHA256 = SelectionConfigHash(c)
			}
			err := r.step(f)
			if sourceConfig == "matching" {
				if err != nil || r.metrics.Submissions != 1 {
					t.Fatal("variant-bound proof lost the real active put", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "selection config proof") || r.metrics.Submissions != 0 {
				t.Fatal("a different study's proof bypassed config binding", err)
			}
		})
	}
}

func TestPutRiskVariantNoRecentBookPreservesNearest(t *testing.T) {
	r, f := putRiskReplay(t)
	later := f.Quotes[0]
	later.ID += "-later"
	f.Quotes[0].Expiry = "2026-04-09"
	for i := range f.Chains {
		if f.Chains[i].Symbol == "NVDA" && f.Chains[i].Kind == "put" {
			f.Chains[i].Expiry = "2026-04-09"
		}
	}
	f.Quotes = append(f.Quotes, later)
	f.Chains = append(f.Chains, Chain{Symbol: later.Symbol, Kind: "put", Expiry: later.Expiry, At: f.At, Weekly: true, Contracts: []string{later.ID}})
	excludeIDs(t, r, &f, []string{"fixture-NVDA-put"})
	if err := r.step(f); err != nil || r.metrics.Submissions != 0 {
		t.Fatal("variant advanced past the source-proven stale nearest expiry", err)
	}
}
