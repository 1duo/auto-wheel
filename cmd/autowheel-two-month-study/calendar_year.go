package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"autowheel/internal/wheel"
)

type historicalAction struct {
	Symbol string `json:"symbol"`
	Date   string `json:"effective_date"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
}

// Freeze the longest official-close coverage authorized for each year. 2024
// begins July 1; 2025 covers the full year. Neither period invents early data.
func calendarYearSchedule(year int) (sessions, entries []string, err error) {
	if year != 2024 && year != 2025 {
		return nil, nil, errors.New("calendar-year study requires 2024 or 2025")
	}
	lastYear, lastWeek := 0, 0
	month := time.January
	if year == 2024 {
		month = time.July
	}
	for d := time.Date(year, month, 1, 9, 35, 0, 0, wheel.Eastern); d.Year() == year; d = d.AddDate(0, 0, 1) {
		if !wheel.TradingWindow(d) {
			continue
		}
		date := d.Format("2006-01-02")
		sessions = append(sessions, date)
		y, week := d.ISOWeek()
		if (y != lastYear || week != lastWeek) && d.Weekday() != time.Friday {
			entries = append(entries, date)
		}
		lastYear, lastWeek = y, week
	}
	return sessions, entries, nil
}

func validateCalendarYearInput(in input) error {
	start, err := time.Parse("2006-01-02", in.Start)
	if err != nil || !slices.Contains(portfolioSymbols, in.Symbol) || len(in.Sources) == 0 {
		return errors.New("invalid calendar-year scope")
	}
	sessions, entries, err := calendarYearSchedule(start.Year())
	if err != nil {
		return err
	}
	if in.Start != sessions[0] || in.End != sessions[len(sessions)-1] || len(in.ReferenceCloses) != len(sessions) {
		return errors.New("references must cover every session of the frozen research period")
	}
	for _, date := range sessions {
		ref, ok := in.ReferenceCloses[date]
		label, _ := time.Parse("2006-01-02", date)
		if !ok || ref.Date != date || ref.Symbol != in.Symbol || ref.Native <= 0 || ref.Native > 1_000_000*10_000_000 || ref.LabelNS != label.UnixNano() || len(in.Sources[ref.Source]) != 64 {
			return errors.New("invalid calendar-year raw closing reference")
		}
	}
	return validateStudySchedule(in, sessions, entries)
}

func calendarYearProfiles() []profile {
	var all []profile
	for _, id := range wheel.StrategyPresetNames() {
		c, _ := wheel.DefaultConfig().WithStrategyPreset(id)
		all = append(all, profile{ID: id, Mode: c.SelectionMode, PutFloor: c.PutMinDelta, PutCeiling: c.PutMaxDelta,
			CallCeiling: c.CallMaxDelta, SpreadBPS: c.MaxSpreadBPS, Preset: id})
	}
	return all
}

func writeExclusiveReport(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return errors.Join(err, f.Close())
}

func writeCalendarYearPlan(path string, year int) error {
	sessions, entries, err := calendarYearSchedule(year)
	if err != nil {
		return err
	}
	closes, expiries := map[string]string{}, []string{}
	for _, date := range sessions {
		d, _ := time.ParseInLocation("2006-01-02", date, wheel.Eastern)
		at, _ := wheel.SessionClose(d)
		closes[date] = at.Format(time.RFC3339)
		if diagnosticExpiry(d) {
			expiries = append(expiries, date)
		}
	}
	return writeExclusiveReport(path, map[string]any{
		"schema": "AutoWheelCalendarYearPlanV1", "year": year, "start": sessions[0], "end": sessions[len(sessions)-1],
		"symbols": portfolioSymbols, "sessions": sessions, "entry_dates": entries, "session_closes": closes,
		"weekly_expiries": expiries, "profiles": calendarYearProfiles(), "max_managed_symbols": 3,
		"max_contracts_per_symbol": 1, "max_collateral_cents": 15_000_000, "production_qualified": false,
		"development_year": 2024, "held_out_year": 2025, "max_experiments": 25,
		"development_start": "2024-07-01", "development_end": "2024-12-31", "held_out_start": "2025-01-02", "held_out_end": "2025-12-31",
		"cost_cases": [][2]int64{{6, 0}, {200, 1}}, "seed_policy": "One observed current cash seed pinned identically for both independent calendar-year counterfactuals; no peer inventory.",
		"selection_policy":        "Net quoted ROI per calendar day, with frozen admissions before future execution books; no recycled sale premium, no replacement of failed fills.",
		"universe_policy":         "Contemporary ten-symbol selection is retrospective; neither year is a blind universe test.",
		"corporate_action_policy": "Require source-pinned full-universe action and dividend coverage. A held split or other unsupported action blocks the run; never discard inventory or mix adjusted stock and raw option strikes.",
	})
}

func verifyFilePin(path, expected string) error {
	if len(expected) != 64 {
		return errors.New("source pin must be SHA256")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("pinned source missing or not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	err = errors.Join(err, f.Close())
	if err != nil || hex.EncodeToString(h.Sum(nil)) != expected {
		return errors.New("pinned source changed")
	}
	return nil
}

func runCalendarYearPortfolio(root, output string, seed, ceiling int64, year int, frozenSelection string) error {
	sessions, _, err := calendarYearSchedule(year)
	if err != nil {
		return err
	}
	profiles := calendarYearProfiles()
	if year == 2025 {
		selected, err := validateFrozenSelection(frozenSelection, seed)
		if err != nil {
			return err
		}
		profiles = slices.DeleteFunc(profiles, func(p profile) bool { return p.ID != selected && p.ID != "conservative-roi-v1" })
	} else if frozenSelection != "" {
		return errors.New("development cannot consume a held-out selection override")
	}
	inputs, pins := []input{}, map[string]string{}
	read := func(path string, v any) error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, (64<<20)+1))
		if err != nil || len(b) > 64<<20 {
			return errors.New("calendar-year input exceeds size limit")
		}
		digest := sha256.Sum256(b)
		pins[path] = hex.EncodeToString(digest[:])
		return json.Unmarshal(b, v)
	}
	if frozenSelection != "" {
		var evidence any
		if err := read(frozenSelection, &evidence); err != nil {
			return err
		}
	}
	for _, symbol := range portfolioSymbols {
		var in input
		if err := read(filepath.Join(root, "inputs", symbol+".json"), &in); err != nil {
			return err
		}
		if in.Schema != "AutoWheelCalendarYearEndpointInputV1" || in.Symbol != symbol || in.Start != sessions[0] {
			return errors.New("calendar-year input identity mismatch")
		}
		if err := validateEndpointInput(in); err != nil {
			return fmt.Errorf("%s: %w", symbol, err)
		}
		for path, digest := range in.Sources {
			if previous, ok := pins[path]; ok && previous != digest {
				return errors.New("inconsistent source pin")
			}
			pins[path] = digest
		}
		inputs = append(inputs, in)
	}
	var actions struct {
		Schema    string              `json:"schema"`
		Start     string              `json:"start"`
		End       string              `json:"end"`
		Symbols   []string            `json:"symbols"`
		Actions   []historicalAction  `json:"actions"`
		Dividends []portfolioDividend `json:"dividends"`
		Sources   map[string]string   `json:"source_sha256"`
	}
	if err := read(filepath.Join(root, "corporate-actions.json"), &actions); err != nil {
		return err
	}
	if actions.Schema != "AutoWheelCalendarYearActionsV1" || actions.Start != sessions[0] || actions.End != sessions[len(sessions)-1] || !slices.Equal(actions.Symbols, portfolioSymbols) || len(actions.Sources) == 0 {
		return errors.New("complete full-universe corporate-action coverage required")
	}
	for path, digest := range actions.Sources {
		if previous, ok := pins[path]; ok && previous != digest {
			return errors.New("inconsistent corporate-action pin")
		}
		pins[path] = digest
	}
	for _, a := range actions.Actions {
		if !slices.Contains(portfolioSymbols, a.Symbol) || !slices.Contains(sessions, a.Date) || a.Kind == "" || actions.Sources[a.Source] == "" {
			return errors.New("invalid corporate-action evidence")
		}
	}
	requiredSplit := historicalAction{Symbol: "AVGO", Date: "2024-07-15", Kind: "split"}
	if year == 2025 {
		requiredSplit = historicalAction{Symbol: "NOW", Date: "2025-12-18", Kind: "split"}
	}
	if !slices.ContainsFunc(actions.Actions, func(a historicalAction) bool {
		return a.Symbol == requiredSplit.Symbol && a.Date == requiredSplit.Date && a.Kind == requiredSplit.Kind
	}) {
		return errors.New("verified in-period stock split omitted from corporate-action coverage")
	}
	dividendIDs := map[string]bool{}
	for _, f := range actions.Dividends {
		if f.ID == "" || dividendIDs[f.ID] || !slices.Contains(portfolioSymbols, f.Symbol) || !slices.Contains(sessions, f.ExDate) {
			return errors.New("invalid or duplicate dividend fact")
		}
		if _, err := portfolioDividendCents(f, 100); err != nil {
			return err
		}
		dividendIDs[f.ID] = true
	}
	for path, digest := range pins {
		if err := verifyFilePin(path, digest); err != nil {
			return fmt.Errorf("source verification: %w", err)
		}
	}
	results := []map[string]any{}
	for _, p := range profiles {
		for _, cost := range [][2]int64{{6, 0}, {200, 1}} {
			r, err := simulatePortfolioWithActions(inputs, p, seed, cost[0], cost[1], ceiling, actions.Dividends, nil, actions.Actions)
			if err != nil {
				return err
			}
			r["performance"] = portfolioPerformance(r["daily_equity"].([]portfolioPoint), seed)
			results = append(results, r)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	code, err := os.ReadFile(executable)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(code)
	return writeExclusiveReport(output, map[string]any{
		"schema": "AutoWheelCalendarYearPortfolioDiagnosticV1", "year": year, "start": inputs[0].Start, "end": inputs[0].End,
		"symbols": portfolioSymbols, "starting_cash_cents": seed, "max_collateral_cents": ceiling, "max_managed_symbols": 3,
		"max_contracts_per_symbol": 1, "source_sha256": pins, "profiles": profiles, "results": results,
		"executable_sha256": hex.EncodeToString(digest[:]), "frozen_selection": frozenSelection,
		"production_qualified": false, "historical_performance_qualified": false, "blind_universe_test": false,
		"limitations": []string{"Endpoint samples cannot prove queue priority, continuous pending-order execution, complete chains, historical OI availability or early exercise.", "Modeled European Greeks use a fixed 4% rate and zero dividend yield. Observed dividends accrue separately; publication clocks remain unqualified.", "Expiry settlement has no delay. Missing option marks remain null and block complete drawdown reporting.", "Held unsupported corporate actions stop the run. This diagnostic cannot promote a live strategy."},
	})
}

// Missing marks propagate through period endpoints and daily risk statistics.
// Cash remains reportable independently of the equity valuation.
func portfolioPerformance(daily []portfolioPoint, seed int64) map[string]any {
	months := []map[string]any{}
	start := new(seed)
	peak, drawdown, utilization, drawdownPct := seed, int64(0), float64(0), float64(0)
	complete := true
	for i, point := range daily {
		utilization += float64(point.Committed) / float64(seed) * 100
		if point.Equity == nil {
			complete = false
		} else {
			peak = max(peak, *point.Equity)
			drawdown = max(drawdown, peak-*point.Equity)
			drawdownPct = max(drawdownPct, float64(peak-*point.Equity)/float64(peak)*100)
		}
		if i+1 != len(daily) && daily[i+1].Date[:7] == point.Date[:7] {
			continue
		}
		row := map[string]any{"month": point.Date[:7], "last_session": point.Date, "starting_equity_cents": start,
			"ending_equity_cents": point.Equity, "ending_cash_cents": point.Cash, "dividend_receivable_cents": point.Receivable,
			"available_cash_cents": point.Available, "committed_cents": point.Committed, "active_symbols": point.Active, "net_pnl_cents": nil, "period_return_pct": nil}
		if start != nil && point.Equity != nil {
			row["net_pnl_cents"] = *point.Equity - *start
			if *start > 0 {
				row["period_return_pct"] = float64(*point.Equity-*start) / float64(*start) * 100
			}
		}
		months = append(months, row)
		start = point.Equity
	}
	r := map[string]any{"end_of_month": months, "daily_valuation_complete": complete,
		"max_drawdown_cents": nil, "max_drawdown_pct": nil, "average_committed_pct_of_seed": nil}
	if len(daily) > 0 {
		r["average_committed_pct_of_seed"] = utilization / float64(len(daily))
	}
	if complete {
		r["max_drawdown_cents"], r["max_drawdown_pct"] = drawdown, drawdownPct
	}
	return r
}
