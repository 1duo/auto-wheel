package main

import "autowheel/internal/researchselection"

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

func developmentWinner(results []developmentResult, seed int64) (string, error) {
	rows := make([]researchselection.Result, len(results))
	for i, r := range results {
		row := researchselection.Result{Strategy: r.Strategy, Fee: r.Fee, Allowance: r.Allowance, Seed: r.Seed,
			PnL: r.PnL, CashReconciled: r.CashReconciled, PnLReconciled: r.PnLReconciled}
		for _, p := range r.Daily {
			row.Daily = append(row.Daily, researchselection.Point{Date: p.Date, Cash: p.Cash, Available: p.Available, Committed: p.Committed, Active: p.Active, Equity: p.Equity})
		}
		for _, t := range r.Trades {
			row.Trades = append(row.Trades, researchselection.Trade{Event: t.Event})
		}
		rows[i] = row
	}
	return researchselection.Winner(rows, seed)
}

func validateFrozenSelection(path string, seed int64) (string, error) {
	_, winner, err := researchselection.Load(path, seed)
	return winner, err
}
