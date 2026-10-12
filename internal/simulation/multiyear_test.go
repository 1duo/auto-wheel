package simulation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"autowheel/internal/researchselection"
	"autowheel/internal/wheel"
)

func multiyearSpec(t *testing.T, preset string, fee wheel.Cents, year int) Spec {
	t.Helper()
	c, err := wheel.DefaultConfig().WithStrategyPreset(preset)
	if err != nil {
		t.Fatal(err)
	}
	c.MaxCollateral, c.Fee = 15_000_000, fee
	s := Spec{Schema: "AutoWheelReplaySpecV1", Mode: "historical_quote_replay", ResearchVariant: ResearchMultiyearROI,
		Start: "2024-07-01", End: "2024-12-31", InitialCash: 8_500_000, Source: "Synthetic admission check; no historical observations",
		CorporateActionsAudited: true, Config: c}
	if year == 2025 {
		s.Start, s.End = "2025-01-02", "2025-12-31"
		s.FrozenDevelopment = syntheticDevelopment(t, preset)
	}
	return s
}

// These invented paths test selection binding only; they never supply market
// observations, source qualification, or historical performance.
func syntheticDevelopment(t *testing.T, winner string) *researchselection.Evidence {
	t.Helper()
	seed := int64(8_500_000)
	var results []researchselection.Result
	for _, id := range wheel.StrategyPresetNames() {
		for _, cost := range [][2]int64{{6, 0}, {200, 1}} {
			pnl := int64(10000)
			if id == winner {
				pnl = 40000
			}
			r := researchselection.Result{Strategy: id, Fee: cost[0], Allowance: cost[1], Seed: seed, PnL: new(pnl), CashReconciled: true, PnLReconciled: true}
			for _, date := range researchselection.Sessions() {
				r.Daily = append(r.Daily, researchselection.Point{Date: date, Cash: seed + pnl, Available: seed + pnl, Equity: new(seed + pnl)})
			}
			for range 12 {
				r.Trades = append(r.Trades, researchselection.Trade{Event: "modeled_sell_put"})
			}
			results = append(results, r)
		}
	}
	report, err := json.Marshal(map[string]any{"schema": "AutoWheelCalendarYearPortfolioDiagnosticV1", "year": 2024, "start": "2024-07-01", "end": "2024-12-31", "starting_cash_cents": seed,
		"max_collateral_cents": 15_000_000, "max_managed_symbols": 3, "max_contracts_per_symbol": 1, "symbols": wheel.DefaultConfig().Symbols, "results": results, "historical_performance_qualified": false})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(report)
	selection, err := json.Marshal(map[string]any{"schema": "AutoWheelFrozenMultiyearSelectionV1", "protocol": researchselection.Protocol, "development_year": 2024, "held_out_year": 2025,
		"development_start": "2024-07-01", "development_end": "2024-12-31", "held_out_start": "2025-01-02", "held_out_end": "2025-12-31", "baseline": researchselection.Baseline, "candidate": winner,
		"development_report_sha256": hex.EncodeToString(digest[:]), "frozen_at": "2026-10-12T04:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	return &researchselection.Evidence{Selection: selection, Report: report}
}

func TestMultiyearContinuousHeldoutRejectsUnfrozenAndEditedPolicies(t *testing.T) {
	for name, change := range map[string]func(*Spec){
		"missing_freeze": func(s *Spec) { s.FrozenDevelopment = nil },
		"unselected_preset": func(s *Spec) {
			s.Config, _ = s.Config.WithStrategyPreset("balanced-roi-v1")
		},
		"changed_seed":   func(s *Spec) { s.InitialCash++ },
		"changed_report": func(s *Spec) { s.FrozenDevelopment.Report[0] ^= 1 },
		"edited_winner": func(s *Spec) {
			var metadata map[string]any
			if err := json.Unmarshal(s.FrozenDevelopment.Selection, &metadata); err != nil {
				t.Fatal(err)
			}
			metadata["candidate"] = "balanced-roi-v1"
			s.FrozenDevelopment.Selection, _ = json.Marshal(metadata)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := multiyearSpec(t, "liquid-roi-v1", 6, 2025)
			change(&s)
			r := replay{report: Report{Spec: s}}
			if err := r.init(); err == nil {
				t.Fatal("held-out evaluation accepted unbound or changed development evidence")
			}
		})
	}
	s := multiyearSpec(t, "liquid-roi-v1", 6, 2025)
	s.Config, _ = s.Config.WithStrategyPreset(researchselection.Baseline)
	r := replay{report: Report{Spec: s}}
	if err := r.init(); err != nil {
		t.Fatal("baseline comparison was rejected", err)
	}
	s.Start, s.End = "2024-07-01", "2024-12-31"
	r = replay{report: Report{Spec: s}}
	if err := r.init(); err == nil {
		t.Fatal("development consumed held-out selection evidence")
	}
}

func TestMultiyearContinuousScopeRequiresActualCoverage(t *testing.T) {
	for _, year := range []int{2024, 2025} {
		for _, preset := range wheel.StrategyPresetNames() {
			for _, fee := range []wheel.Cents{6, 200} {
				s := multiyearSpec(t, preset, fee, year)
				r := replay{report: Report{Spec: s}}
				if err := r.init(); err != nil {
					t.Fatal("valid offline scope rejected", year, preset, fee, err)
				}
				want := 128
				if year == 2025 {
					want = 250
				}
				if len(r.valuations) != want || r.cash != 8_500_000 || r.report.Spec.Config.MaxCollateral != 15_000_000 {
					t.Fatal("scope changed the calendar, cash seed or offline ceiling")
				}
				var input bytes.Buffer
				if err := json.NewEncoder(&input).Encode(s); err != nil {
					t.Fatal(err)
				}
				result := Replay(&input)
				if result.Status != "blocked" || result.Performance != nil || len(result.Equity) != 0 || len(result.Trades) != 0 {
					t.Fatal("declared coverage produced performance without actual observations")
				}
				if err := s.Config.Validate(); err == nil {
					t.Fatal("offline ceiling became a valid production configuration")
				}
			}
		}
	}
}

func TestMultiyearContinuousScopeRejectsPolicyDrift(t *testing.T) {
	for name, change := range map[string]func(*Spec){
		"earlier_unavailable_history": func(s *Spec) { s.Start = "2024-01-01" },
		"changed_period":              func(s *Spec) { s.End = "2025-12-31" },
		"unnamed_preset":              func(s *Spec) { s.Config.StrategyPreset = "" },
		"unknown_preset":              func(s *Spec) { s.Config.StrategyPreset = "unknown" },
		"edited_delta":                func(s *Spec) { s.Config.PutMinDelta = .09 },
		"omitted_symbol":              func(s *Spec) { s.Config.Symbols = s.Config.Symbols[:9] },
		"extra_slots":                 func(s *Spec) { s.Config.MaxSymbols = 4 },
		"higher_ceiling":              func(s *Spec) { s.Config.MaxCollateral++ },
		"wrong_fee_case":              func(s *Spec) { s.Config.Fee = 100 },
		"unsecured_starting_cash":     func(s *Spec) { s.InitialCash = 15_000_001 },
		"peer_cash":                   func(s *Spec) { s.PeerReserve = 1000 },
		"peer_symbols":                func(s *Spec) { s.PeerSymbols = []string{"NVDA"} },
		"account_binding":             func(s *Spec) { s.Config.Account = "12345678" },
		"missing_action_audit":        func(s *Spec) { s.CorporateActionsAudited = false },
		"unqualified_oi":              func(s *Spec) { s.Config.MinOI = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			s := multiyearSpec(t, "conservative-roi-v1", 6, 2024)
			change(&s)
			r := replay{report: Report{Spec: s}}
			if err := r.init(); err == nil {
				t.Fatal("changed policy passed frozen offline scope")
			}
		})
	}
	base := wheel.DefaultConfig()
	if base.MaxCollateral != 8_000_000 || base.Fee != 100 || base.Validate() != nil {
		t.Fatal("production defaults changed")
	}
}
