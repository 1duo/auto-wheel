package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"autowheel/internal/wheel"
)

type officialClose struct {
	Date    string `json:"date"`
	Symbol  string `json:"symbol"`
	Native  int64  `json:"close_native"`
	LabelNS int64  `json:"bar_label_ns"`
	Source  string `json:"source"`
}

type endpointPoint struct {
	Date       string `json:"date"`
	Cash       int64  `json:"cash_cents"`
	Shares     int    `json:"shares"`
	StockValue int64  `json:"stock_value_cents"`
	Liability  *int64 `json:"option_liability_cents"`
	Committed  int64  `json:"committed_cents"`
	Available  int64  `json:"available_cash_cents"`
	Equity     *int64 `json:"equity_cents"`
	Missing    string `json:"missing_mark_contract,omitempty"`
}

type endpointPeriod struct {
	Start       *int64   `json:"starting_equity_cents"`
	End         *int64   `json:"ending_equity_cents"`
	PnL         *int64   `json:"net_pnl_cents"`
	Return      *float64 `json:"return_on_80000_pct"`
	Drawdown    *int64   `json:"closing_drawdown_cents"`
	DrawdownPct *float64 `json:"closing_drawdown_pct"`
	Premiums    int64    `json:"premium_cents"`
	Fees        int64    `json:"fees_cents"`
	Sales       int      `json:"sales"`
	Assignments int      `json:"assignments"`
	Calls       int      `json:"call_aways"`
}

type endpointResult struct {
	Profile          profile                   `json:"profile"`
	Mode             wheel.SelectionMode       `json:"mode"`
	Fee              int64                     `json:"fee_per_contract_cents"`
	Allowance        int64                     `json:"price_allowance_cents"`
	EndEquity        *int64                    `json:"ending_equity_cents"`
	PnL              *int64                    `json:"net_pnl_cents"`
	Return           *float64                  `json:"return_on_80000_pct"`
	ReturnCommitted  *float64                  `json:"return_on_peak_committed_pct"`
	Premiums         int64                     `json:"premium_cents"`
	Fees             int64                     `json:"fees_cents"`
	StockRealized    int64                     `json:"stock_realized_cents"`
	StockUnrealized  int64                     `json:"stock_unrealized_cents"`
	Assignments      int                       `json:"modeled_assignments"`
	Calls            int                       `json:"modeled_call_aways"`
	PeakCommitted    int64                     `json:"peak_committed_cents"`
	AverageUse       float64                   `json:"close_sample_average_buying_power_use_pct"`
	Cash             int64                     `json:"ending_cash_cents"`
	Shares           int                       `json:"ending_shares"`
	Basis            int64                     `json:"ending_share_basis_cents"`
	Short            *quote                    `json:"ending_short_option,omitempty"`
	Liability        *int64                    `json:"ending_option_liability_cents"`
	Drawdown         *int64                    `json:"close_sample_max_drawdown_cents"`
	DrawdownPct      *float64                  `json:"close_sample_max_drawdown_pct"`
	EndpointComplete bool                      `json:"endpoint_complete"`
	DailyComplete    bool                      `json:"daily_valuation_complete"`
	Daily            []endpointPoint           `json:"daily_equity"`
	Trades           []trade                   `json:"trade_ledger"`
	Orders           []order                   `json:"order_ledger"`
	Periods          map[string]endpointPeriod `json:"periods"`
	Accounting       bool                      `json:"cash_ledger_reconciled"`
	PnLReconciled    bool                      `json:"endpoint_pnl_reconciled"`
	Rejections       map[string]int            `json:"selection_rejections"`
}

func validateEndpointInput(in input) error {
	if in.Schema != "AutoWheelTwoMonthEndpointInputV1" && in.Schema != "AutoWheelThreeMonthEndpointInputV1" && in.Schema != "AutoWheelYearToDateEndpointInputV1" && in.Schema != "AutoWheelCalendarYearEndpointInputV1" {
		return errors.New("endpoint input schema required")
	}
	base := in
	if in.Schema == "AutoWheelTwoMonthEndpointInputV1" {
		base.Schema = "AutoWheelTwoMonthInputV1"
	}
	if err := validateInput(base); err != nil {
		return err
	}
	count := 0
	for _, d := range in.Days {
		if !diagnosticExpiry(d.Close.At) {
			continue
		}
		ref, ok := in.ExpiryCloses[d.Date]
		label, err := time.Parse("2006-01-02", d.Date)
		if !ok || err != nil || ref.Date != d.Date || ref.Symbol != in.Symbol || ref.Native <= 0 || ref.Native > 1_000_000*10_000_000 || ref.LabelNS != label.UnixNano() || len(in.Sources[ref.Source]) != 64 {
			return fmt.Errorf("missing or invalid official closing reference: %s", d.Date)
		}
		count++
	}
	if len(in.ExpiryCloses) != count {
		return errors.New("unexpected closing reference date")
	}
	return nil
}

// Only reporting needs intermediate marks: this hold-to-expiry strategy does
// not consume them for orders, cash security, assignment or stock ownership.
func endpointMark(a *account, d day) (endpointPoint, error) {
	p, err := a.mark(d)
	if err == nil {
		return endpointPoint{Date: p.Date, Cash: p.Cash, Shares: p.Shares, StockValue: p.StockValue, Liability: new(p.Liability),
			Committed: p.Committed, Available: p.Available, Equity: new(p.Equity)}, nil
	}
	missing, ok := errors.AsType[*missingMarkError](err)
	if !ok {
		return endpointPoint{}, err
	}
	available := a.Cash
	if a.Short != nil && a.Short.Kind == "put" {
		available -= a.Short.Strike * 100
	}
	if available < 0 {
		return endpointPoint{}, errors.New("negative secured cash")
	}
	return endpointPoint{Date: d.Date, Cash: a.Cash, Shares: a.Shares, StockValue: int64(a.Shares) * d.Close.Stock.Bid,
		Committed: a.committed(), Available: available, Missing: missing.Contract}, nil
}

func endpointPeriods(r endpointResult) map[string]endpointPeriod {
	all := map[string]endpointPeriod{}
	start := new(initialCash)
	for _, month := range studyMonths(r.Daily[0].Date, r.Daily[len(r.Daily)-1].Date) {
		m := endpointPeriod{Start: start}
		complete := start != nil
		peak, dd, pct := int64(0), int64(0), 0.0
		if start != nil {
			peak = *start
		}
		for _, p := range r.Daily {
			if !strings.HasPrefix(p.Date, month) {
				continue
			}
			m.End = p.Equity
			if p.Equity == nil {
				complete = false
				continue
			}
			peak = max(peak, *p.Equity)
			dd = max(dd, peak-*p.Equity)
			pct = max(pct, float64(peak-*p.Equity)/float64(peak)*100)
		}
		if m.Start != nil && m.End != nil {
			m.PnL = new(*m.End - *m.Start)
			m.Return = new(float64(*m.PnL) / float64(initialCash) * 100)
		}
		if complete {
			m.Drawdown, m.DrawdownPct = new(dd), new(pct)
		}
		for _, t := range r.Trades {
			if !strings.HasPrefix(t.Date, month) {
				continue
			}
			if strings.HasPrefix(t.Event, "modeled_sell_") {
				m.Premiums += t.Price * 100
				m.Fees += t.Fee
				m.Sales++
			}
			if t.Event == "modeled_put_assignment" {
				m.Assignments++
			}
			if t.Event == "modeled_call_away" {
				m.Calls++
			}
		}
		all[month], start = m, m.End
	}
	return all
}

func simulateEndpoint(in input, p profile, fee, allowance int64) (endpointResult, error) {
	a := account{Cash: initialCash}
	r := endpointResult{Profile: p, Mode: p.Mode, Fee: fee, Allowance: allowance, DailyComplete: true, Rejections: map[string]int{}}
	peak, dd, pct, totalUse := initialCash, int64(0), 0.0, 0.0
	for index, d := range in.Days {
		if d.Entry != nil && a.Short == nil {
			selected, regime, permitPut := p, "", true
			if p.ID == "cycle-adaptive-v1" {
				selected, regime, permitPut = cycleProfile(in, index)
			}
			if !permitPut && a.Shares == 0 {
				r.Rejections["cycle_policy_cash_entry"]++
			} else if (d.Entry.Stock.Ask-d.Entry.Stock.Bid)*10000 > d.Entry.Stock.Ask*100 {
				r.Rejections["underlying_spread_exceeds_1pct"]++
			} else {
				q, delta, vol, rejected, ok := selectContract(in.Symbol, selected, fee, *d.Entry, a.Basis)
				for k, n := range rejected {
					r.Rejections[k] += n
				}
				if ok && in.Schema == "AutoWheelYearToDateEndpointInputV1" && !yearOrderFunded(q, a.Cash, fee) {
					r.Rejections["cash_security_or_capital_cap"]++
					ok = false
				}
				if ok && in.Schema == "AutoWheelYearToDateEndpointInputV1" {
					limit := q.Bid - allowance
					limit -= limit % orderIncrement(d.Date, q.Contract, limit)
					if limit <= 0 || limit*100 <= fee {
						r.Rejections["nonpositive_net_fixed_limit"]++
						ok = false
					}
				}
				if ok {
					o, filled := modelOrder(d, q, allowance)
					r.Orders = append(r.Orders, o)
					if filled {
						if err := a.sell(d.Date, q, fee, o.Limit, delta, vol); err != nil {
							return endpointResult{}, err
						}
						t := &a.Trades[len(a.Trades)-1]
						t.DecisionNS, t.DecisionQuoteNS, t.ExecutionNS, t.ObservedBid = o.DecisionNS, o.DecisionQuoteNS, o.ExecutionNS, o.ObservedBid
						if p.ID == "cycle-adaptive-v1" {
							t.ExecutionProfile, t.EntryRegime = new(selected), regime
						}
					}
				}
			}
		}
		if a.Short != nil && a.Short.Expiry < d.Date {
			return endpointResult{}, errors.New("unsettled expired option")
		}
		if a.Short != nil && a.Short.Expiry == d.Date {
			ref, ok := in.ExpiryCloses[d.Date]
			if !ok || ref.Native <= 0 || ref.Native > 1_000_000*10_000_000 {
				return endpointResult{}, errors.New("official expiry reference required")
			}
			strikeNative := a.Short.Strike * 10_000_000
			if err := a.settleOutcome(d.Date, strikeNative-ref.Native >= 10_000_000, ref.Native-strikeNative >= 10_000_000); err != nil {
				return endpointResult{}, err
			}
		}
		mark, err := endpointMark(&a, d)
		if err != nil {
			return endpointResult{}, err
		}
		r.Daily = append(r.Daily, mark)
		if mark.Equity == nil {
			r.DailyComplete = false
		} else {
			peak = max(peak, *mark.Equity)
			dd = max(dd, peak-*mark.Equity)
			pct = max(pct, float64(peak-*mark.Equity)/float64(peak)*100)
		}
		totalUse += float64(mark.Committed) / float64(initialCash) * 100
	}
	last := r.Daily[len(r.Daily)-1]
	r.EndEquity, r.Liability, r.EndpointComplete = last.Equity, last.Liability, last.Equity != nil
	r.Premiums, r.Fees, r.StockRealized, r.StockUnrealized = a.Premiums, a.Fees, a.StockRealized, last.StockValue-a.Basis*int64(a.Shares)
	r.Assignments, r.Calls, r.PeakCommitted = a.Assignments, a.CallAways, a.PeakCommitted
	r.Cash, r.Shares, r.Basis, r.Short, r.Trades = a.Cash, a.Shares, a.Basis, a.Short, a.Trades
	r.AverageUse = totalUse / float64(len(r.Daily))
	if r.EndpointComplete {
		r.PnL = new(*r.EndEquity - initialCash)
		r.Return = new(float64(*r.PnL) / float64(initialCash) * 100)
		if a.PeakCommitted > 0 {
			r.ReturnCommitted = new(float64(*r.PnL) / float64(a.PeakCommitted) * 100)
		}
		if *r.PnL != r.Premiums-r.Fees+r.StockRealized+r.StockUnrealized-*r.Liability {
			return endpointResult{}, errors.New("endpoint P&L does not reconcile")
		}
		r.PnLReconciled = true
	}
	if r.DailyComplete {
		r.Drawdown, r.DrawdownPct = new(dd), new(pct)
	}
	cash := initialCash
	for _, t := range r.Trades {
		cash += t.Cashflow
	}
	if cash != a.Cash {
		return endpointResult{}, errors.New("cash ledger does not reconcile")
	}
	r.Accounting = true
	r.Periods = endpointPeriods(r)
	return r, nil
}

func endpointStudy(in input) ([]endpointResult, error) {
	all := []endpointResult{}
	for _, mode := range []wheel.SelectionMode{wheel.SelectionConservative, wheel.SelectionBalanced} {
		for _, costs := range [][2]int64{{100, 0}, {200, 1}} {
			r, err := simulateEndpoint(in, baseline(mode), costs[0], costs[1])
			if err != nil {
				return nil, err
			}
			all = append(all, r)
		}
	}
	if in.Schema == "AutoWheelYearToDateEndpointInputV1" {
		p := baseline(wheel.SelectionBalanced)
		p.ID = "cycle-adaptive-v1"
		for _, costs := range [][2]int64{{100, 0}, {200, 1}} {
			r, err := simulateEndpoint(in, p, costs[0], costs[1])
			if err != nil {
				return nil, err
			}
			all = append(all, r)
		}
	}
	return all, nil
}
