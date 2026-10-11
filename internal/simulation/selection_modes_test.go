package simulation

import (
	"encoding/json"
	"testing"

	"autowheel/internal/wheel"
)

func TestReplaySelectionModes(t *testing.T) {
	for _, tc := range []struct {
		mode         wheel.SelectionMode
		lowDelta     float64
		highDelta    float64
		wantLowDelta bool
	}{
		{wheel.SelectionConservative, -.06, -.14, true},
		{wheel.SelectionBalanced, -.18, -.30, false},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			var spec Spec
			if err := json.NewDecoder(AccountingFixture()).Decode(&spec); err != nil {
				t.Fatal(err)
			}
			config, err := spec.Config.WithSelectionMode(tc.mode)
			if err != nil {
				t.Fatal(err)
			}
			spec.Config = config
			r := &replay{report: Report{Spec: spec}}
			if err := r.init(); err != nil {
				t.Fatal("declared production selection mode rejected", err)
			}
			f := fixtureFrame(r.decisions[0], "decision")
			f.CompleteSymbols = spec.Config.Symbols
			low := f.Quotes[0]
			low.Delta = tc.lowDelta
			f.Quotes[0] = low
			high := low
			high.ID += "-higher-premium"
			high.Delta = tc.highDelta
			high.Bid, high.Ask = low.Bid+100, low.Ask+100
			f.Quotes = append(f.Quotes, high)
			for i := range f.Chains {
				if f.Chains[i].Symbol == high.Symbol && f.Chains[i].Kind == high.Kind {
					f.Chains[i].Contracts = append(f.Chains[i].Contracts, high.ID)
				}
			}
			if err := r.step(f); err != nil {
				t.Fatal(err)
			}
			wantID := high.ID
			if tc.wantLowDelta {
				wantID = low.ID
			}
			if r.pending == nil || r.pending.Candidate.ID != wantID || r.metrics.Submissions != 1 {
				t.Fatal("replay lost the named mode's ranking at selection or pre-submit")
			}
		})
	}
}

func TestReplaySelectionModeIsolation(t *testing.T) {
	for name, mutate := range map[string]func(*Spec){
		"conservative_zero_floor": func(s *Spec) { s.Config.PutMinDelta = 0 },
		"conservative_balanced_band": func(s *Spec) {
			s.Config.PutMinDelta, s.Config.PutMaxDelta = .15, .35
		},
		"variant_with_named_mode": func(s *Spec) {
			s.ResearchVariant = ResearchPutRiskCap
			s.Config.PutMinDelta = 0
		},
		"unknown_mode": func(s *Spec) { s.Config.SelectionMode = "unknown" },
		"slot_drift":   func(s *Spec) { s.Config.MaxSymbols = 2 },
		"call_drift":   func(s *Spec) { s.Config.CallMinDelta = .6 },
		"account":      func(s *Spec) { s.Config.Account = "1234" },
	} {
		t.Run(name, func(t *testing.T) {
			var spec Spec
			if err := json.NewDecoder(AccountingFixture()).Decode(&spec); err != nil {
				t.Fatal(err)
			}
			var err error
			spec.Config, err = spec.Config.WithSelectionMode(wheel.SelectionConservative)
			if err != nil {
				t.Fatal(err)
			}
			mutate(&spec)
			if err := (&replay{report: Report{Spec: spec}}).init(); err == nil {
				t.Fatal("named-mode replay admitted an undeclared strategy or production account")
			}
		})
	}
}
