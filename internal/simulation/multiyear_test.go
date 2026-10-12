package simulation

import (
	"bytes"
	"encoding/json"
	"testing"

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
	}
	return s
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
