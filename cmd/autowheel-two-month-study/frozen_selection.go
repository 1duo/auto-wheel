package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"os"
	"slices"

	"autowheel/internal/wheel"
)

type developmentResult struct {
	Strategy       string           `json:"strategy"`
	Fee            int64            `json:"fee_per_contract_cents"`
	Allowance      int64            `json:"price_allowance_cents"`
	Seed           int64            `json:"starting_cash_cents"`
	PnL            *int64           `json:"net_pnl_cents"`
	CashReconciled bool             `json:"cash_ledger_reconciled"`
	PnLReconciled  bool             `json:"pnl_reconciled"`
	Daily          []portfolioPoint `json:"daily_equity"`
	Trades         []portfolioTrade `json:"trade_ledger"`
}

// Recompute the development metric independently of the Python selector and
// any hand-edited selection file. No held-out prices are accepted here.
func developmentWinner(results []developmentResult, seed int64) (string, error) {
	if seed <= 0 || seed > 100_000_000 || len(results) != 8 {
		return "", errors.New("invalid frozen development grid")
	}
	sessions, _, _ := calendarYearSchedule(2024)
	type key struct {
		id  string
		fee int64
	}
	type measure struct {
		score, drawdown int64
		sales           int
	}
	measured := map[key]measure{}
	for _, r := range results {
		k := key{r.Strategy, r.Fee}
		if _, exists := measured[k]; exists || !slices.Contains(wheel.StrategyPresetNames(), r.Strategy) || r.Seed != seed ||
			(r.Fee != 6 && r.Fee != 200) || (r.Fee == 6 && r.Allowance != 0) || (r.Fee == 200 && r.Allowance != 1) ||
			!r.CashReconciled || !r.PnLReconciled || r.PnL == nil || len(r.Daily) != len(sessions) {
			return "", errors.New("invalid development result")
		}
		peak, drawdown, sales := seed, int64(0), 0
		for i, p := range r.Daily {
			if p.Date != sessions[i] || p.Equity == nil || *p.Equity < -int64(wheel.MaxMoney) || *p.Equity > int64(wheel.MaxMoney) || p.Cash < 0 || p.Cash > int64(wheel.MaxMoney) || p.Available < 0 || p.Available > p.Cash || p.Committed < 0 || p.Committed > 15_000_000 || p.Active < 0 || p.Active > 3 {
				return "", errors.New("incomplete or unsafe development path")
			}
			peak = max(peak, *p.Equity)
			drawdown = max(drawdown, peak-*p.Equity)
		}
		for _, t := range r.Trades {
			if t.Event == "modeled_sell_put" || t.Event == "modeled_sell_call" {
				sales++
			}
		}
		if *r.Daily[len(r.Daily)-1].Equity-seed != *r.PnL {
			return "", errors.New("development endpoint does not reconcile")
		}
		measured[k] = measure{*r.PnL - drawdown, drawdown, sales}
	}
	baseline, winner, best := "conservative-roi-v1", "conservative-roi-v1", int64(0)
	found := false
	for _, id := range wheel.StrategyPresetNames() {
		pass := true
		for _, fee := range []int64{6, 200} {
			m, b := measured[key{id, fee}], measured[key{baseline, fee}]
			pass = pass && m.sales >= 12 && m.drawdown <= b.drawdown+seed/100
		}
		score := measured[key{id, 200}].score
		if pass && (!found || score > best || (score == best && cmp.Compare(id, winner) < 0)) {
			winner, best, found = id, score, true
		}
	}
	return winner, nil
}

func validateFrozenSelection(path string, seed int64) (string, error) {
	if path == "" {
		return "", errors.New("2025 requires a frozen 2024-only selection before evaluation")
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 1<<20 {
		return "", errors.New("frozen selection missing or oversized")
	}
	var selection struct {
		Schema          string `json:"schema"`
		Protocol        string `json:"protocol"`
		DevelopmentYear int    `json:"development_year"`
		HeldOutYear     int    `json:"held_out_year"`
		Candidate       string `json:"candidate"`
		Baseline        string `json:"baseline"`
		Report          string `json:"development_report"`
		Digest          string `json:"development_report_sha256"`
	}
	if json.Unmarshal(b, &selection) != nil || selection.Schema != "AutoWheelFrozenMultiyearSelectionV1" || selection.Protocol != "2024-h2-stressed-pnl-minus-drawdown-v1" || selection.DevelopmentYear != 2024 || selection.HeldOutYear != 2025 || selection.Baseline != "conservative-roi-v1" {
		return "", errors.New("invalid development/held-out separation")
	}
	if err := verifyFilePin(selection.Report, selection.Digest); err != nil {
		return "", err
	}
	b, err = os.ReadFile(selection.Report)
	if err != nil || len(b) > 128<<20 {
		return "", errors.New("development report missing or oversized")
	}
	var report struct {
		Schema  string              `json:"schema"`
		Year    int                 `json:"year"`
		Seed    int64               `json:"starting_cash_cents"`
		Results []developmentResult `json:"results"`
	}
	if json.Unmarshal(b, &report) != nil || report.Schema != "AutoWheelCalendarYearPortfolioDiagnosticV1" || report.Year != 2024 || report.Seed != seed {
		return "", errors.New("frozen source is not the same-seed 2024 development report")
	}
	winner, err := developmentWinner(report.Results, seed)
	if err != nil || winner != selection.Candidate {
		return "", errors.New("frozen candidate differs from independently recomputed 2024 winner")
	}
	return winner, nil
}
