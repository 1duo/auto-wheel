// Package researchselection validates the frozen 2024-only research selection.
// It cannot qualify historical prices, accounting declarations or live trading.
package researchselection

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"time"

	"autowheel/internal/wheel"
)

const (
	Baseline          = "conservative-roi-v1"
	Protocol          = "2024-h2-stressed-pnl-minus-drawdown-v1"
	MaxSelectionBytes = 1 << 20
	MaxReportBytes    = 16 << 20
)

// Evidence preserves exact source bytes through JSON base64 encoding.
// The whole evidence is bound by the continuous replay's input digest.
type Evidence struct {
	Selection []byte `json:"selection_bytes"`
	Report    []byte `json:"development_report_bytes"`
}

type Point struct {
	Date      string `json:"date"`
	Cash      int64  `json:"cash_cents"`
	Available int64  `json:"available_cash_cents"`
	Committed int64  `json:"committed_cents"`
	Active    int    `json:"active_symbols"`
	Equity    *int64 `json:"equity_cents"`
}

type Trade struct {
	Event string `json:"event"`
}

type Result struct {
	Strategy       string  `json:"strategy"`
	Fee            int64   `json:"fee_per_contract_cents"`
	Allowance      int64   `json:"price_allowance_cents"`
	Seed           int64   `json:"starting_cash_cents"`
	PnL            *int64  `json:"net_pnl_cents"`
	CashReconciled bool    `json:"cash_ledger_reconciled"`
	PnLReconciled  bool    `json:"pnl_reconciled"`
	Daily          []Point `json:"daily_equity"`
	Trades         []Trade `json:"trade_ledger"`
}

func Sessions() []string {
	var sessions []string
	for d := time.Date(2024, 7, 1, 9, 35, 0, 0, wheel.Eastern); d.Year() == 2024; d = d.AddDate(0, 0, 1) {
		if wheel.TradingWindow(d) {
			sessions = append(sessions, d.Format("2006-01-02"))
		}
	}
	return sessions
}

// Winner reconstructs the fixed development metric; no 2025 prices are read.
func Winner(results []Result, seed int64) (string, error) {
	if seed <= 0 || seed > 100_000_000 || len(results) != 8 {
		return "", errors.New("invalid frozen development grid")
	}
	sessions := Sessions()
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
	winner, best, found := Baseline, int64(0), false
	for _, id := range wheel.StrategyPresetNames() {
		pass := true
		for _, fee := range []int64{6, 200} {
			m, b := measured[key{id, fee}], measured[key{Baseline, fee}]
			pass = pass && m.sales >= 12 && m.drawdown <= b.drawdown+seed/100
		}
		score := measured[key{id, 200}].score
		if pass && (!found || score > best || (score == best && cmp.Compare(id, winner) < 0)) {
			winner, best, found = id, score, true
		}
	}
	return winner, nil
}

type selection struct {
	Schema           string `json:"schema"`
	Protocol         string `json:"protocol"`
	DevelopmentYear  int    `json:"development_year"`
	HeldOutYear      int    `json:"held_out_year"`
	DevelopmentStart string `json:"development_start"`
	DevelopmentEnd   string `json:"development_end"`
	HeldOutStart     string `json:"held_out_start"`
	HeldOutEnd       string `json:"held_out_end"`
	Candidate        string `json:"candidate"`
	Baseline         string `json:"baseline"`
	Report           string `json:"development_report"`
	Digest           string `json:"development_report_sha256"`
	FrozenAt         string `json:"frozen_at"`
}

func Validate(e Evidence, seed int64) (string, error) {
	if len(e.Selection) == 0 || len(e.Selection) > MaxSelectionBytes || len(e.Report) == 0 || len(e.Report) > MaxReportBytes {
		return "", errors.New("frozen development evidence missing or oversized")
	}
	var s selection
	if json.Unmarshal(e.Selection, &s) != nil || s.Schema != "AutoWheelFrozenMultiyearSelectionV1" || s.Protocol != Protocol || s.DevelopmentYear != 2024 || s.HeldOutYear != 2025 ||
		s.DevelopmentStart != "2024-07-01" || s.DevelopmentEnd != "2024-12-31" || s.HeldOutStart != "2025-01-02" || s.HeldOutEnd != "2025-12-31" || s.Baseline != Baseline {
		return "", errors.New("invalid development/held-out separation")
	}
	if at, err := time.Parse(time.RFC3339Nano, s.FrozenAt); err != nil || at.IsZero() {
		return "", errors.New("missing frozen selection timestamp")
	}
	digest := sha256.Sum256(e.Report)
	if hex.EncodeToString(digest[:]) != s.Digest {
		return "", errors.New("frozen development report changed")
	}
	var report struct {
		Schema       string   `json:"schema"`
		Year         int      `json:"year"`
		Start        string   `json:"start"`
		End          string   `json:"end"`
		Seed         int64    `json:"starting_cash_cents"`
		Ceiling      int64    `json:"max_collateral_cents"`
		MaxSymbols   int      `json:"max_managed_symbols"`
		MaxContracts int      `json:"max_contracts_per_symbol"`
		Symbols      []string `json:"symbols"`
		Results      []Result `json:"results"`
	}
	if json.Unmarshal(e.Report, &report) != nil || report.Schema != "AutoWheelCalendarYearPortfolioDiagnosticV1" || report.Year != 2024 || report.Start != "2024-07-01" || report.End != "2024-12-31" || report.Seed != seed || report.Ceiling != 15_000_000 || report.MaxSymbols != 3 || report.MaxContracts != 1 || !slices.Equal(report.Symbols, wheel.DefaultConfig().Symbols) {
		return "", errors.New("frozen source differs from the complete same-seed 2024 research scope")
	}
	winner, err := Winner(report.Results, seed)
	if err != nil || winner != s.Candidate {
		return "", errors.New("frozen candidate differs from independently recomputed 2024 winner")
	}
	return winner, nil
}

func readRegular(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("frozen source missing, oversized or not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.Join(errors.New("frozen source identity changed"), err, f.Close())
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	err = errors.Join(err, f.Close())
	if err != nil || int64(len(b)) > limit {
		return nil, errors.Join(errors.New("frozen source unreadable or oversized"), err)
	}
	return b, nil
}

// Load resolves only regular bounded files. Embedded evidence is revalidated
// by the replay engine; no filesystem reads are needed while replaying frames.
func Load(path string, seed int64) (Evidence, string, error) {
	b, err := readRegular(path, MaxSelectionBytes)
	if err != nil {
		return Evidence{}, "", err
	}
	var s selection
	if err := json.Unmarshal(b, &s); err != nil || s.Report == "" {
		return Evidence{}, "", errors.New("invalid frozen development report reference")
	}
	report, err := readRegular(s.Report, MaxReportBytes)
	if err != nil {
		return Evidence{}, "", err
	}
	e := Evidence{Selection: b, Report: report}
	winner, err := Validate(e, seed)
	return e, winner, err
}
