package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"autowheel/internal/wheel"
)

func developmentFixture() []developmentResult {
	sessions, _, _ := calendarYearSchedule(2024)
	results := []developmentResult{}
	for index, id := range wheel.StrategyPresetNames() {
		for _, cost := range [][2]int64{{6, 0}, {200, 1}} {
			pnl := int64((index + 1) * 10000)
			r := developmentResult{Strategy: id, Fee: cost[0], Allowance: cost[1], Seed: 8_500_000, PnL: new(pnl), CashReconciled: true, PnLReconciled: true}
			for _, date := range sessions {
				r.Daily = append(r.Daily, portfolioPoint{Date: date, Cash: r.Seed, Available: r.Seed, Equity: new(r.Seed + pnl)})
			}
			for range 12 {
				r.Trades = append(r.Trades, portfolioTrade{trade: trade{Event: "modeled_sell_put"}})
			}
			results = append(results, r)
		}
	}
	return results
}

func TestMultiyearFrozenWinnerRejectsEditedSelection(t *testing.T) {
	results := developmentFixture()
	winner, err := developmentWinner(results, 8_500_000)
	if err != nil || winner != "cost-protected-roi-v1" {
		t.Fatal("incorrect development winner", winner, err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "2024.json")
	report := map[string]any{"schema": "AutoWheelCalendarYearPortfolioDiagnosticV1", "year": 2024, "starting_cash_cents": 8_500_000, "results": results}
	if err := writeExclusiveReport(path, report); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	rawDigest := sha256.Sum256(b)
	digest := hex.EncodeToString(rawDigest[:])
	selection := map[string]any{"schema": "AutoWheelFrozenMultiyearSelectionV1", "protocol": "2024-h2-stressed-pnl-minus-drawdown-v1",
		"development_year": 2024, "held_out_year": 2025, "baseline": "conservative-roi-v1", "candidate": winner,
		"development_report": path, "development_report_sha256": digest}
	selectedPath := filepath.Join(dir, "frozen.json")
	if err := writeExclusiveReport(selectedPath, selection); err != nil {
		t.Fatal(err)
	}
	if got, err := validateFrozenSelection(selectedPath, 8_500_000); err != nil || got != winner {
		t.Fatal("valid frozen selection rejected", err)
	}
	selection["candidate"] = "balanced-roi-v1"
	b, _ = json.Marshal(selection)
	if err := os.WriteFile(selectedPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateFrozenSelection(selectedPath, 8_500_000); err == nil {
		t.Fatal("hand-edited winner was accepted for held-out evaluation")
	}
	if _, err := validateFrozenSelection("", 8_500_000); err == nil {
		t.Fatal("unfrozen 2025 evaluation accepted")
	}
	results[0].Daily[0].Equity = nil
	if _, err := developmentWinner(results, 8_500_000); err == nil {
		t.Fatal("missing development mark was assigned a score")
	}
}

func TestMultiyearMonthEndCashAndMissingDrawdown(t *testing.T) {
	daily := []portfolioPoint{{Date: "2024-01-30", Cash: 1000, Equity: nil}, {Date: "2024-01-31", Cash: 1010, Equity: new(int64(1030))},
		{Date: "2024-02-29", Cash: 1050, Equity: new(int64(1100))}}
	r := portfolioPerformance(daily, 1000)
	months := r["end_of_month"].([]map[string]any)
	if r["daily_valuation_complete"] != false || r["max_drawdown_cents"] != nil || len(months) != 2 || months[0]["ending_cash_cents"] != int64(1010) || months[1]["net_pnl_cents"] != int64(70) {
		t.Fatal("missing mark or month-end cash was misreported", r)
	}
}
