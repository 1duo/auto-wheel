package main

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"autowheel/internal/wheel"
)

var portfolioSymbols = []string{"NVDA", "IREN", "NOW", "AMD", "AMZN", "GOOGL", "META", "MSFT", "PLTR", "AVGO"}

type portfolioDividend struct {
	ID      string `json:"id"`
	Symbol  string `json:"symbol"`
	Amount  string `json:"cash_usd_exact"`
	ExDate  string `json:"ex_date"`
	PayDate string `json:"payment_date"`
}

type portfolioCandidate struct {
	Symbol      string  `json:"symbol"`
	Quote       quote   `json:"quote"`
	Profile     profile `json:"profile"`
	Regime      string  `json:"regime,omitempty"`
	Delta       float64 `json:"modeled_delta"`
	Vol         float64 `json:"modeled_implied_vol"`
	Limit       int64   `json:"limit_cents"`
	Numerator   int64   `json:"net_roi_credit_cents"`
	Denominator int64   `json:"capital_days_cents"`
	Fee         int64   `json:"fee_cents"`
}

type portfolioTrade struct {
	Symbol string `json:"symbol"`
	trade
}

type portfolioOrder struct {
	Symbol      string `json:"symbol"`
	Rank        int    `json:"decision_rank"`
	Numerator   int64  `json:"net_roi_credit_cents"`
	Denominator int64  `json:"capital_days_cents"`
	order
}

type portfolioPosition struct {
	Symbol      string `json:"symbol"`
	Shares      int    `json:"shares"`
	Basis       int64  `json:"basis_cents"`
	Short       *quote `json:"short,omitempty"`
	Value       int64  `json:"stock_value_cents"`
	NativeValue int64  `json:"native_stock_bid_value_cents"`
	Liability   *int64 `json:"option_liability_cents"`
	Supplement  bool   `json:"eod_supplement_used"`
}

type portfolioPoint struct {
	Date         string              `json:"date"`
	Cash         int64               `json:"cash_cents"`
	Receivable   int64               `json:"dividend_receivable_cents"`
	Available    int64               `json:"available_cash_cents"`
	Committed    int64               `json:"committed_cents"`
	Active       int                 `json:"active_symbols"`
	Equity       *int64              `json:"equity_cents"`
	NativeEquity *int64              `json:"native_stock_bid_equity_cents"`
	Positions    []portfolioPosition `json:"positions"`
}

// Integer ROI cross-products stay within int64: bounded prices <= 1e6 cents,
// 100 shares, and at most eight capital-days per contract.
func portfolioROICompare(a, b portfolioCandidate) int {
	return cmp.Or(cmp.Compare(b.Numerator*a.Denominator, a.Numerator*b.Denominator),
		cmp.Compare(a.Symbol, b.Symbol), cmp.Compare(a.Quote.Contract, b.Quote.Contract))
}

func portfolioTotals(accounts map[string]*account) (active int, committed, reserved int64) {
	for _, a := range accounts {
		if a.Shares > 0 || a.Short != nil {
			active++
		}
		committed += a.committed()
		if a.Short != nil && a.Short.Kind == "put" {
			reserved += a.Short.Strike * 100
		}
	}
	return
}

// Freeze all admissions at the decision clock. Future fills cannot add slots,
// recycle premium or replace an unfilled higher-ranked order on this date.
func portfolioAdmissions(all []portfolioCandidate, accounts map[string]*account, cash, fee, ceiling int64) []portfolioCandidate {
	all = slices.Clone(all)
	for i := range all {
		all[i].Fee = fee
	}
	return portfolioAdmissionsWithFees(all, accounts, cash, ceiling)
}

func portfolioAdmissionsWithFees(all []portfolioCandidate, accounts map[string]*account, cash, ceiling int64) []portfolioCandidate {
	return portfolioAdmissionsWithLimits(all, accounts, cash, ceiling, 0)
}

func portfolioAdmissionsWithLimits(all []portfolioCandidate, accounts map[string]*account, cash, ceiling, symbolLimit int64) []portfolioCandidate {
	active, committed, reserve := portfolioTotals(accounts)
	available, budget := cash-reserve, ceiling-committed
	selected, seen := []portfolioCandidate{}, map[string]bool{}
	for _, c := range all {
		fee := c.Fee
		if seen[c.Symbol] {
			continue
		}
		a := accounts[c.Symbol]
		if a.Short != nil || available < fee {
			continue
		}
		if c.Quote.Kind == "put" {
			collateral := c.Quote.Strike * 100
			if symbolLimit > 0 && !wheel.FitsSymbolCollateral(wheel.Cents(collateral), wheel.Cents(fee), wheel.Cents(symbolLimit)) {
				continue
			}
			if a.Shares != 0 || active >= 3 || collateral+fee > available || collateral+fee > budget {
				continue
			}
			available -= collateral
			budget -= collateral + fee
			active++
		} else if a.Shares != 100 {
			continue
		}
		available -= fee
		seen[c.Symbol] = true
		selected = append(selected, c)
	}
	return selected
}

func portfolioCandidates(in input, index int, a *account, base profile, fee, allowance int64) []portfolioCandidate {
	return portfolioCandidatesWithFees(in, index, a, base, portfolioFeeSchedule{Fixed: fee}, allowance)
}

func portfolioCandidatesWithFees(in input, index int, a *account, base profile, fees portfolioFeeSchedule, allowance int64) []portfolioCandidate {
	d := in.Days[index]
	if d.Entry == nil || a.Short != nil {
		return nil
	}
	p, regime, allow := base, "", true
	if base.ID == "cycle-adaptive-v1" {
		p, regime, allow = cycleProfile(in, index)
	}
	if !allow && a.Shares == 0 {
		return nil
	}
	if (d.Entry.Stock.Ask-d.Entry.Stock.Bid)*10000 > d.Entry.Stock.Ask*100 {
		return nil
	}
	kind := "put"
	if a.Shares > 0 {
		kind = "call"
	}
	nearest := ""
	for _, q := range d.Entry.Quotes {
		if q.Kind == kind && (nearest == "" || q.Expiry < nearest) {
			nearest = q.Expiry
		}
	}
	all := []portfolioCandidate{}
	for _, original := range d.Entry.Quotes {
		if original.Kind != kind || original.Expiry != nearest {
			continue
		}
		s := *d.Entry
		s.Quotes = []quote{original}
		q, delta, vol, _, ok := selectContract(in.Symbol, p, fees.optionSale(d.Date, original.Bid), s, a.Basis)
		if !ok {
			continue
		}
		limit := q.Bid - allowance
		if limit <= 0 {
			continue
		}
		limit -= limit % orderIncrement(d.Date, q.Contract, limit)
		fee := fees.optionSale(d.Date, limit)
		numerator, capital := limit*100-fee, q.Strike*100
		if numerator <= 0 {
			continue
		}
		if kind == "call" {
			spot := (s.Stock.Bid + s.Stock.Ask) / 2
			numerator = (limit-max(0, spot-q.Strike))*100 - fee
			capital = spot * 100
		}
		expiry, _ := time.Parse("2006-01-02", q.Expiry)
		today, _ := time.Parse("2006-01-02", d.Date)
		days := int64(expiry.Sub(today).Hours() / 24)
		if days < 1 || days > 8 {
			continue
		}
		all = append(all, portfolioCandidate{in.Symbol, q, p, regime, delta, vol, limit, numerator, capital * days, fee})
	}
	return all
}

func portfolioDividendCents(f portfolioDividend, shares int) (int64, error) {
	for _, date := range []string{f.ExDate, f.PayDate} {
		if _, err := time.Parse("2006-01-02", date); err != nil {
			return 0, errors.New("invalid dividend date")
		}
	}
	whole, fraction, _ := strings.Cut(f.Amount, ".")
	if whole == "" || len(f.Amount) > 40 || len(fraction) > 12 || strings.Trim(whole+fraction, "0123456789") != "" {
		return 0, errors.New("dividend must be an exact plain decimal")
	}
	r, ok := new(big.Rat).SetString(f.Amount)
	if !ok || r.Sign() < 0 || (shares != 0 && shares != 100) || f.PayDate < f.ExDate {
		return 0, errors.New("invalid dividend")
	}
	r.Mul(r, big.NewRat(int64(shares)*100, 1))
	if !r.IsInt() || !r.Num().IsInt64() || r.Num().Int64() > 1_000_000 {
		return 0, errors.New("inexact or excessive dividend")
	}
	return r.Num().Int64(), nil
}

func simulatePortfolio(inputs []input, base profile, seed, fee, allowance, ceiling int64, facts []portfolioDividend, supplements map[string]int64) (map[string]any, error) {
	return simulatePortfolioWithActions(inputs, base, seed, fee, allowance, ceiling, facts, supplements, nil)
}

func simulatePortfolioWithActions(inputs []input, base profile, seed, fee, allowance, ceiling int64, facts []portfolioDividend, supplements map[string]int64, actions []historicalAction) (map[string]any, error) {
	return simulatePortfolioUsingFees(inputs, base, seed, portfolioFeeSchedule{Fixed: fee}, allowance, ceiling, facts, supplements, actions)
}

func simulatePortfolioUsingFees(inputs []input, base profile, seed int64, feeSchedule portfolioFeeSchedule, allowance, ceiling int64, facts []portfolioDividend, supplements map[string]int64, actions []historicalAction) (map[string]any, error) {
	return simulatePortfolioUsingLimits(inputs, base, seed, feeSchedule, allowance, ceiling, facts, supplements, actions, 0)
}

func simulatePortfolioUsingLimits(inputs []input, base profile, seed int64, feeSchedule portfolioFeeSchedule, allowance, ceiling int64, facts []portfolioDividend, supplements map[string]int64, actions []historicalAction, symbolLimit int64) (map[string]any, error) {
	if symbolLimit < 0 || symbolLimit > ceiling {
		return nil, errors.New("invalid per-symbol research collateral limit")
	}
	fee := feeSchedule.Fixed
	if seed <= 0 || seed > 100_000_000 || ceiling <= 0 || ceiling > 15_000_000 || fee < 0 || fee > 200 || allowance < 0 || allowance > 1 || len(inputs) == 0 {
		return nil, errors.New("invalid portfolio scope or money bounds")
	}
	if feeSchedule.Robinhood {
		if fee != 0 || allowance != 0 {
			return nil, errors.New("Robinhood fees cannot be overridden by flat costs or price allowances")
		}
		for _, in := range inputs {
			for _, d := range in.Days {
				if date, err := time.Parse("2006-01-02", d.Date); err != nil || date.Format("2006-01-02") != d.Date || d.Date < "2026-01-01" || d.Date > "2026-10-09" {
					return nil, errors.New("Robinhood fee schedule outside verified 2026 window")
				}
			}
		}
	}
	accounts := map[string]*account{}
	for _, in := range inputs {
		accounts[in.Symbol] = &account{}
	}
	dividendIDs := map[string]bool{}
	for _, f := range facts {
		if f.ID == "" || dividendIDs[f.ID] || accounts[f.Symbol] == nil {
			return nil, errors.New("invalid or duplicate dividend identity")
		}
		if _, err := portfolioDividendCents(f, 100); err != nil {
			return nil, err
		}
		dividendIDs[f.ID] = true
	}
	cash, income, paid := seed, int64(0), int64(0)
	entitlements := []map[string]any{}
	payments := map[string]int64{}
	orders, trades, daily := []portfolioOrder{}, []portfolioTrade{}, []portfolioPoint{}
	peakCommit, peakSlots, supplementalMarks := int64(0), 0, 0
	for index, date := range inputs[0].Days {
		for _, action := range actions {
			if action.Date == date.Date {
				a := accounts[action.Symbol]
				if a == nil || a.Shares != 0 || a.Short != nil {
					return nil, fmt.Errorf("unsupported held corporate action: %s %s; cannot erase or resize inventory", action.Date, action.Symbol)
				}
			}
		}
		// Prior-session shares establish entitlement; receivables cannot fund orders.
		for _, f := range facts {
			if f.ExDate != date.Date {
				continue
			}
			a := accounts[f.Symbol]
			if a == nil {
				return nil, errors.New("unknown dividend symbol")
			}
			amount, err := portfolioDividendCents(f, a.Shares)
			if err != nil {
				return nil, err
			}
			income += amount
			payments[f.PayDate] += amount
			entitlements = append(entitlements, map[string]any{"id": f.ID, "symbol": f.Symbol, "ex_date": f.ExDate, "payment_date": f.PayDate, "shares": a.Shares, "amount_cents": amount})
		}
		all := []portfolioCandidate{}
		for _, in := range inputs {
			all = append(all, portfolioCandidatesWithFees(in, index, accounts[in.Symbol], base, feeSchedule, allowance)...)
		}
		slices.SortFunc(all, portfolioROICompare)
		selected := portfolioAdmissionsWithLimits(all, accounts, cash, ceiling, symbolLimit)
		for rank, c := range selected {
			in := inputs[slices.IndexFunc(inputs, func(in input) bool { return in.Symbol == c.Symbol })]
			o, filled := modelOrder(in.Days[index], c.Quote, allowance)
			orders = append(orders, portfolioOrder{c.Symbol, rank + 1, c.Numerator, c.Denominator, o})
			if !filled {
				continue
			}
			a := accounts[c.Symbol]
			a.Cash = cash
			if err := a.sell(date.Date, c.Quote, c.Fee, o.Limit, c.Delta, c.Vol); err != nil {
				return nil, err
			}
			cash = a.Cash
			t := &a.Trades[len(a.Trades)-1]
			t.DecisionNS, t.DecisionQuoteNS, t.ExecutionNS, t.ObservedBid = o.DecisionNS, o.DecisionQuoteNS, o.ExecutionNS, o.ObservedBid
			t.ExecutionProfile, t.EntryRegime = new(c.Profile), c.Regime
			trades = append(trades, portfolioTrade{c.Symbol, *t})
			active, commitment, reserve := portfolioTotals(accounts)
			if active > 3 || commitment > ceiling || cash-reserve < 0 {
				return nil, errors.New("shared portfolio limit violated")
			}
			peakCommit, peakSlots = max(peakCommit, commitment), max(peakSlots, active)
		}
		for _, in := range inputs {
			a, d := accounts[in.Symbol], in.Days[index]
			if a.Short != nil && a.Short.Expiry < d.Date {
				return nil, errors.New("unsettled shared option")
			}
			if a.Short != nil && a.Short.Expiry == d.Date {
				ref := in.ExpiryCloses[d.Date]
				strike := a.Short.Strike * 10_000_000
				a.Cash = cash
				if err := a.settleOutcome(d.Date, strike-ref.Native >= 10_000_000, ref.Native-strike >= 10_000_000); err != nil {
					return nil, err
				}
				t := &a.Trades[len(a.Trades)-1]
				if t.Event == "modeled_call_away" {
					t.Fee = feeSchedule.stockDeliverySale(d.Date, t.Strike)
					t.Cashflow -= t.Fee
					a.Cash -= t.Fee
					a.Fees += t.Fee
				}
				cash = a.Cash
				trades = append(trades, portfolioTrade{in.Symbol, a.Trades[len(a.Trades)-1]})
			}
		}
		cash += payments[date.Date]
		paid += payments[date.Date]
		active, commitment, reserve := portfolioTotals(accounts)
		if cash < reserve || cash > int64(wheel.MaxMoney) || active > 3 || commitment > ceiling {
			return nil, errors.New("invalid shared cash or exposure")
		}
		p := portfolioPoint{Date: date.Date, Cash: cash, Receivable: income - paid, Available: cash - reserve, Committed: commitment, Active: active}
		equity, native, complete := cash+p.Receivable, cash+p.Receivable, true
		for _, in := range inputs {
			a, d := accounts[in.Symbol], in.Days[index]
			a.Cash = cash
			mark, err := endpointMark(a, d)
			if err != nil {
				return nil, err
			}
			liability, supplement := mark.Liability, false
			if liability == nil {
				if ask, ok := supplements[d.Date+"/"+a.Short.Contract]; ok {
					liability, supplement = new(ask*100), true
					supplementalMarks++
				}
			}
			value := int64(0)
			if a.Shares == 100 {
				value = in.ReferenceCloses[d.Date].Native / 100_000
			}
			if liability == nil {
				complete = false
			} else {
				equity += value - *liability
				native += mark.StockValue - *liability
			}
			p.Positions = append(p.Positions, portfolioPosition{in.Symbol, a.Shares, a.Basis, a.Short, value, mark.StockValue, liability, supplement})
		}
		if complete {
			p.Equity, p.NativeEquity = new(equity), new(native)
		}
		daily = append(daily, p)
		peakCommit, peakSlots = max(peakCommit, commitment), max(peakSlots, active)
	}
	last := daily[len(daily)-1]
	premiums, fees, realized, unrealized, assignments, calls := int64(0), int64(0), int64(0), int64(0), 0, 0
	for _, a := range accounts {
		premiums += a.Premiums
		fees += a.Fees
		realized += a.StockRealized
		assignments += a.Assignments
		calls += a.CallAways
	}
	for _, p := range last.Positions {
		unrealized += p.Value - int64(p.Shares)*p.Basis
	}
	check := seed + paid
	for _, t := range trades {
		check += t.Cashflow
	}
	if check != cash {
		return nil, errors.New("shared cash ledger failed")
	}
	r := map[string]any{"strategy": base.ID, "mode": base.Mode, "fee_per_contract_cents": fee, "price_allowance_cents": allowance, "starting_cash_cents": seed, "ending_cash_cents": cash, "ending_equity_cents": last.Equity, "premium_cents": premiums, "fees_cents": fees, "dividend_income_cents": income, "dividend_paid_cents": paid, "ending_dividend_receivable_cents": income - paid, "stock_realized_cents": realized, "stock_unrealized_cents": unrealized, "assignments": assignments, "call_aways": calls, "peak_committed_cents": peakCommit, "peak_active_symbols": peakSlots, "eod_supplement_mark_states": supplementalMarks, "cash_ledger_reconciled": true, "daily_equity": daily, "trade_ledger": trades, "order_ledger": orders, "ending_positions": last.Positions}
	if base.ID == "baseline" {
		r["strategy"] = string(base.Mode)
	}
	r["max_symbol_collateral_cents"] = symbolLimit
	if feeSchedule.Robinhood {
		delete(r, "fee_per_contract_cents")
		r["fee_model"] = robinhoodFeeDisclosure()
	}
	if last.Equity != nil {
		pnl := *last.Equity - seed
		liability := int64(0)
		for _, p := range last.Positions {
			liability += *p.Liability
		}
		if pnl != premiums-fees+income+realized+unrealized-liability {
			return nil, errors.New("shared P&L failed")
		}
		r["net_pnl_cents"], r["return_on_starting_cash_pct"], r["pnl_reconciled"] = pnl, float64(pnl)/float64(seed)*100, true
	}
	return r, nil
}

func runPortfolio(root, output string, seed, ceiling int64) error {
	return runPortfolioScope(root, output, seed, ceiling, false, "", "")
}

func runPortfolioScope(root, output string, seed, ceiling int64, availablePresets bool, first, last string) error {
	return runPortfolioUniverse(root, output, seed, ceiling, availablePresets, first, last, "", "")
}

func runPortfolioUniverse(root, output string, seed, ceiling int64, availablePresets bool, first, last, symbolList, presetID string) error {
	return runPortfolioUniverseWithPolicy(root, output, seed, ceiling, availablePresets, first, last, symbolList, presetID, portfolioStudyPolicy{})
}

type portfolioStudyPolicy struct {
	LimitSymbolCollateral bool
	IncludeAdaptive       bool
}

func runPortfolioUniverseWithPolicy(root, output string, seed, ceiling int64, availablePresets bool, first, last, symbolList, presetID string, policy portfolioStudyPolicy) error {
	if !availablePresets && (symbolList != "" || presetID != "") {
		return errors.New("custom research universe and preset require available-presets")
	}
	symbols, err := availableStudySymbols(symbolList)
	if err != nil {
		return err
	}
	profiles := []profile{}
	if availablePresets {
		profiles = calendarYearProfiles()
		if policy.IncludeAdaptive {
			p := baseline(wheel.SelectionBalanced)
			p.ID = "cycle-adaptive-v1"
			profiles = append(profiles, p)
		}
		if presetID != "" {
			profiles = slices.DeleteFunc(profiles, func(p profile) bool { return p.ID != presetID })
			if len(profiles) != 1 {
				return errors.New("unknown frozen research preset")
			}
		}
	} else {
		for _, mode := range []wheel.SelectionMode{wheel.SelectionConservative, wheel.SelectionBalanced, "adaptive"} {
			p := baseline(mode)
			if mode == "adaptive" {
				p = baseline(wheel.SelectionBalanced)
				p.ID = "cycle-adaptive-v1"
			}
			profiles = append(profiles, p)
		}
	}
	inputs, pins := []input{}, map[string]string{}
	read := func(path string, v any) error {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if len(b) > 64<<20 {
			return errors.New("portfolio input too large")
		}
		digest := sha256.Sum256(b)
		pins[path] = hex.EncodeToString(digest[:])
		return json.Unmarshal(b, v)
	}
	for _, symbol := range symbols {
		var in input
		if err := read(filepath.Join(root, "study-v2", "inputs", symbol+".json"), &in); err != nil {
			return err
		}
		if in.Symbol != symbol {
			return errors.New("portfolio input symbol mismatch")
		}
		if availablePresets && in.Schema != "AutoWheelYearToDateEndpointInputV1" {
			return errors.New("available-presets requires finished 2026 YTD inputs; calendar-year action gates cannot be bypassed")
		}
		if err := validateEndpointInput(in); err != nil {
			return fmt.Errorf("%s: %w", symbol, err)
		}
		inputs = append(inputs, in)
	}
	var dividends struct {
		Facts []portfolioDividend `json:"facts"`
	}
	if err := read(filepath.Join(root, "dividend-facts.json"), &dividends); err != nil {
		return err
	}
	// A caller-frozen subset cannot own shares in omitted research symbols.
	dividends.Facts = slices.DeleteFunc(dividends.Facts, func(f portfolioDividend) bool {
		return !slices.Contains(symbols, f.Symbol)
	})
	var recovered struct {
		Observations []eodObservation `json:"observations"`
	}
	if err := read(filepath.Join(root, "valuation-completion-v1", "eod-observations.json"), &recovered); err != nil {
		return err
	}
	supplements := map[string]int64{}
	for _, o := range recovered.Observations {
		var response json.RawMessage
		if err := read(o.Path, &response); err != nil {
			return err
		}
		if pins[o.Path] != o.SHA {
			return errors.New("invalid EOD supplement pin")
		}
		ask, err := validateEODSupplement(o, response)
		if err != nil {
			return err
		}
		key := o.Date + "/" + o.Contract
		if _, exists := supplements[key]; exists {
			return errors.New("duplicate EOD supplement")
		}
		supplements[key] = ask
	}
	if first != "" || last != "" {
		var err error
		inputs, err = clipPortfolioInputs(inputs, first, last)
		if err != nil {
			return err
		}
	}
	results := []map[string]any{}
	symbolLimit := int64(0)
	if policy.LimitSymbolCollateral {
		symbolLimit = int64((wheel.Config{MaxCollateral: wheel.Cents(ceiling), MaxSymbols: 3}).SymbolCollateralLimit())
		if symbolLimit <= 0 {
			return errors.New("invalid derived per-symbol research collateral limit")
		}
	}
	for _, p := range profiles {
		r, err := simulatePortfolioUsingLimits(inputs, p, seed, portfolioFeeSchedule{Robinhood: true}, 0, ceiling, dividends.Facts, supplements, nil, symbolLimit)
		if err != nil {
			return err
		}
		results = append(results, r)
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	b, err := os.ReadFile(executable)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(b)
	report := map[string]any{"schema": "AutoWheelSharedPortfolioEndpointStudyV1", "start": inputs[0].Start, "end": inputs[0].End, "symbols": portfolioSymbols, "starting_cash_cents": seed, "max_managed_symbols": 3, "max_contracts_per_symbol": 1, "max_collateral_cents": ceiling, "source_sha256": pins, "executable_sha256": hex.EncodeToString(digest[:]), "results": results, "production_qualified": false, "ranking_policy": "All eligible nearest-expiry contracts ranked by net quoted ROI per day, then symbol and contract. Put denominator: strike collateral. Call numerator: signed time value less fee; denominator: decision stock midpoint. Original delta bands retained; delta-first ordering overridden in this offline study. Admissions frozen before observing executions; unfilled selections are not replaced.", "valuation_policy": "Owned stock: official unadjusted close, aggregate position value floored to cents. Native stock bid sensitivity retained. Fresh native option asks preferred; hash-pinned EOD supplements only for missing marks. Supplement publication clocks do not meet strict 4pm freshness. Missing marks remain null.", "limitations": []string{"Weekly decisions only; 194 sessions, 40 entry dates. Bounded strikes, not complete chains.", "European modeled Greeks; fixed 4% pricing rate and zero dividend yield; open-interest filtering omitted.", "Expiry-only exercise at one cent ITM, no early exercise or settlement delay. Observed ordinary dividends accrue at ex-date and pay on payment date.", "Contemporary universe selected from July-September outcomes; retrospective, not blind out-of-sample.", "Robinhood dated retail fees only; call-away proceeds use stock-sale regulatory rules as a model inference. No cash interest, taxes, optional subscriptions or service charges. Not broker invoices."}}
	report["fee_model"] = robinhoodFeeDisclosure()
	report["max_symbol_collateral_cents"] = symbolLimit
	report["symbol_collateral_policy"] = "Explicit historical counterfactual: per-symbol admission limit disabled."
	if policy.LimitSymbolCollateral {
		report["symbol_collateral_policy"] = "Fixed account ceiling divided by three managed slots, floored to cents. New put collateral plus entry fee must fit. Existing stock is never forcibly liquidated."
	}
	report["adaptive_research_included"] = policy.IncludeAdaptive
	if availablePresets {
		report["schema"] = "AutoWheelAvailablePresetEndpointStudyV1"
		report["symbols"] = symbols
		report["selected_preset"] = presetID
		report["limitations"].([]string)[3] = "Retrospective research universe; universe selection has no blind out-of-sample qualification."
		if symbolList != "" {
			report["universe_policy"] = "Caller-frozen subset of the bounded 2026 research universe. Custom membership does not qualify historical performance or authorize production deployment."
		}
		report["historical_performance_qualified"] = false
		report["strategy_promotion_qualified"] = false
		report["sessions"] = len(inputs[0].Days)
		report["period_policy"] = "Validate the entire finished input first, then replay the exact requested session slice with an independent flat cash seed. Remaining reference maps do not supply decisions. This is an unqualified endpoint diagnostic."
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(raw, '\n'))
	return errors.Join(err, f.Close())
}
