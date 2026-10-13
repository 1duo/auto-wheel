// autowheel-two-month-study is an offline, causal, continuous account diagnostic.
// It has no broker, credential, order submission, or production-state interface.
package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"autowheel/internal/optionmodel"
	"autowheel/internal/wheel"
)

const initialCash int64 = 8_000_000

type profile struct {
	ID          string              `json:"id"`
	Mode        wheel.SelectionMode `json:"mode"`
	PutFloor    float64             `json:"put_delta_floor"`
	PutCeiling  float64             `json:"put_delta_ceiling"`
	CallCeiling float64             `json:"call_delta_ceiling"`
	SpreadBPS   int                 `json:"max_spread_bps"`
	Preset      string              `json:"strategy_preset,omitempty"`
}

func baseline(mode wheel.SelectionMode) profile {
	p := profile{ID: "baseline", Mode: mode, PutFloor: .15, PutCeiling: .35, CallCeiling: .85, SpreadBPS: 2000}
	if mode == wheel.SelectionConservative {
		p.PutFloor, p.PutCeiling = .05, .15
	}
	return p
}

func profiles(mode wheel.SelectionMode) []profile {
	base := baseline(mode)
	all := []profile{base}
	values := []float64{.20, .275, .35}
	if mode == wheel.SelectionConservative {
		values = []float64{.05, .075, .10}
	}
	for _, put := range values {
		for _, call := range []float64{.60, .725, .85} {
			for _, spread := range []int{1000, 2000} {
				p := base
				if mode == wheel.SelectionConservative {
					p.PutFloor = put
				} else {
					p.PutCeiling = put
				}
				p.CallCeiling, p.SpreadBPS = call, spread
				if p == base {
					continue
				}
				p.ID = fmt.Sprintf("put-%.3f-call-%.3f-spread-%d", put, call, spread)
				all = append(all, p)
			}
		}
	}
	return all
}

type stock struct {
	At  time.Time `json:"at"`
	Bid int64     `json:"bid_cents"`
	Ask int64     `json:"ask_cents"`
}

type quote struct {
	Contract  string `json:"contract"`
	Kind      string `json:"kind"`
	Expiry    string `json:"expiry"`
	Strike    int64  `json:"strike_cents"`
	Bid       int64  `json:"bid_cents"`
	Ask       int64  `json:"ask_cents"`
	BidSize   int    `json:"bid_size"`
	AskSize   int    `json:"ask_size"`
	ReceiveNS int64  `json:"receive_ns"`
	EventNS   int64  `json:"event_ns"`
	Flags     int    `json:"native_flags"`
}

type sample struct {
	At     time.Time       `json:"at"`
	Stock  stock           `json:"stock"`
	Quotes []quote         `json:"quotes"`
	Marks  []valuationMark `json:"valuation_marks,omitempty"`
}

// A one-sided closing ask can value a held short, but cannot enter selection.
type valuationMark struct {
	Contract  string `json:"contract"`
	Ask       int64  `json:"ask_cents"`
	AskSize   int    `json:"ask_size"`
	ReceiveNS int64  `json:"receive_ns"`
	EventNS   int64  `json:"event_ns"`
	Reason    string `json:"source_reason"`
	Flags     int    `json:"native_flags"`
}

type executionBook struct {
	Contract string `json:"contract"`
	Status   string `json:"status"`
	Quote    *quote `json:"quote,omitempty"`
}

type execution struct {
	At          time.Time       `json:"at"`
	NotBeforeNS int64           `json:"not_before_ns"`
	Books       []executionBook `json:"books"`
}

type day struct {
	Date      string     `json:"date"`
	Entry     *sample    `json:"entry,omitempty"`
	Close     sample     `json:"close"`
	Execution *execution `json:"execution,omitempty"`
}

type input struct {
	Schema          string                   `json:"schema"`
	Symbol          string                   `json:"symbol"`
	Start           string                   `json:"start,omitempty"`
	End             string                   `json:"end,omitempty"`
	Days            []day                    `json:"days"`
	Sources         map[string]string        `json:"source_sha256"`
	ExpiryCloses    map[string]officialClose `json:"expiry_closes,omitempty"`
	ReferenceCloses map[string]officialClose `json:"reference_closes,omitempty"`
}

type trade struct {
	Date             string   `json:"date"`
	Event            string   `json:"event"`
	Contract         string   `json:"contract"`
	Strike           int64    `json:"strike_cents"`
	Price            int64    `json:"price_cents"`
	Cashflow         int64    `json:"cashflow_cents"`
	Fee              int64    `json:"fee_cents"`
	Delta            float64  `json:"modeled_delta,omitzero"`
	ImpliedVol       float64  `json:"modeled_implied_vol,omitzero"`
	DecisionNS       int64    `json:"decision_ns,omitzero"`
	DecisionQuoteNS  int64    `json:"decision_quote_receive_ns,omitzero"`
	ExecutionNS      int64    `json:"execution_receive_ns,omitzero"`
	ObservedBid      int64    `json:"observed_execution_bid_cents,omitzero"`
	ExecutionProfile *profile `json:"execution_profile,omitempty"`
	EntryRegime      string   `json:"entry_regime,omitempty"`
}

type order struct {
	Date            string `json:"date"`
	Contract        string `json:"contract"`
	DecisionNS      int64  `json:"decision_ns"`
	DecisionBid     int64  `json:"decision_bid_cents"`
	DecisionQuoteNS int64  `json:"decision_quote_receive_ns"`
	Limit           int64  `json:"limit_cents"`
	Status          string `json:"status"`
	ExecutionNS     int64  `json:"execution_receive_ns,omitzero"`
	ObservedBid     int64  `json:"observed_execution_bid_cents,omitzero"`
}

type period struct {
	StartEquity int64   `json:"starting_equity_cents"`
	NetPnL      int64   `json:"net_pnl_cents"`
	Return      float64 `json:"return_on_80000_pct"`
	Drawdown    int64   `json:"closing_drawdown_cents"`
	DrawdownPct float64 `json:"closing_drawdown_pct"`
	Premiums    int64   `json:"premium_cents"`
	Fees        int64   `json:"fees_cents"`
	Sales       int     `json:"sales"`
	Assignments int     `json:"assignments"`
	Calls       int     `json:"call_aways"`
	HoldingDays int     `json:"stock_holding_closes"`
}

type point struct {
	Date       string `json:"date"`
	Cash       int64  `json:"cash_cents"`
	Shares     int    `json:"shares"`
	StockValue int64  `json:"stock_value_cents"`
	Liability  int64  `json:"option_liability_cents"`
	Committed  int64  `json:"committed_cents"`
	Available  int64  `json:"available_cash_cents"`
	Equity     int64  `json:"equity_cents"`
}

type account struct {
	Cash          int64
	Shares        int
	Basis         int64
	Short         *quote
	Premiums      int64
	Fees          int64
	StockRealized int64
	Assignments   int
	CallAways     int
	PeakCommitted int64
	Trades        []trade
}

type result struct {
	Profile              profile             `json:"profile"`
	Role                 string              `json:"study_role,omitempty"`
	Mode                 wheel.SelectionMode `json:"mode"`
	FeePerContract       int64               `json:"fee_per_contract_cents"`
	Allowance            int64               `json:"price_allowance_cents"`
	AugustProfile        profile             `json:"august_profile"`
	Orders               []order             `json:"order_ledger"`
	Periods              map[string]period   `json:"periods"`
	EndingEquity         int64               `json:"ending_equity_cents"`
	NetPnL               int64               `json:"net_pnl_cents"`
	Premiums             int64               `json:"premium_cents"`
	Fees                 int64               `json:"fees_cents"`
	StockRealized        int64               `json:"stock_realized_cents"`
	StockUnrealized      int64               `json:"stock_unrealized_cents"`
	Return               float64             `json:"return_on_80000_pct"`
	ReturnOnCommitted    *float64            `json:"return_on_peak_committed_pct"`
	Drawdown             float64             `json:"close_sample_max_drawdown_pct"`
	DrawdownCents        int64               `json:"close_sample_max_drawdown_cents"`
	AverageUse           float64             `json:"close_sample_average_buying_power_use_pct"`
	PeakCommitted        int64               `json:"peak_committed_cents"`
	Assignments          int                 `json:"modeled_assignments"`
	CallAways            int                 `json:"modeled_call_aways"`
	EndingCash           int64               `json:"ending_cash_cents"`
	EndingShares         int                 `json:"ending_shares"`
	EndingBasis          int64               `json:"ending_share_basis_cents"`
	EndingShort          *quote              `json:"ending_short_option,omitempty"`
	EndingLiability      int64               `json:"ending_option_liability_cents"`
	Trades               []trade             `json:"trade_ledger"`
	Daily                []point             `json:"daily_equity"`
	SelectionRejections  map[string]int      `json:"selection_rejections"`
	AccountingReconciled bool                `json:"accounting_reconciled"`
}

// A bounded single-symbol diagnostic never accepts a price above $10,000.
func validMoney(x int64) bool { return x > 0 && x <= 1_000_000 }

func validateSample(s sample, symbol string) error {
	if s.At.IsZero() || !validMoney(s.Stock.Bid) || !validMoney(s.Stock.Ask) || s.Stock.Ask < s.Stock.Bid || s.Stock.At.IsZero() || !s.Stock.At.Before(s.At) || s.At.Sub(s.Stock.At) > 30*time.Second {
		return errors.New("missing, invalid or stale stock observation")
	}
	seen := map[string]bool{}
	for _, q := range s.Quotes {
		if seen[q.Contract] {
			return errors.New("duplicate option quote")
		}
		seen[q.Contract] = true
		if err := validateQuote(q, symbol, s.At, 30*time.Second); err != nil {
			return err
		}
	}
	return nil
}

func validateQuote(q quote, symbol string, cut time.Time, maxAge time.Duration) error {
	if len(q.Contract) != 21 || strings.TrimSpace(q.Contract[:6]) != symbol || (q.Kind != "put" && q.Kind != "call") || !validMoney(q.Strike) || !validMoney(q.Bid) || !validMoney(q.Ask) || q.Ask < q.Bid || q.BidSize < 1 || q.AskSize < 1 {
		return errors.New("invalid option quote")
	}
	expiry, err := time.Parse("2006-01-02", q.Expiry)
	if err != nil || expiry.Format("060102") != q.Contract[6:12] || !diagnosticExpiry(expiry) {
		return errors.New("option expiry differs from pilot scope")
	}
	for _, digit := range q.Contract[13:] {
		if digit < '0' || digit > '9' {
			return errors.New("invalid OSI strike")
		}
	}
	rawStrike, err := strconv.ParseInt(q.Contract[13:], 10, 64)
	if err != nil || rawStrike != q.Strike*10 || (q.Kind == "put" && q.Contract[12] != 'P') || (q.Kind == "call" && q.Contract[12] != 'C') {
		return errors.New("option identity fields disagree")
	}
	recv, event := time.Unix(0, q.ReceiveNS), time.Unix(0, q.EventNS)
	if q.ReceiveNS <= 0 || q.EventNS <= 0 || event.After(recv) || !recv.Before(cut) || cut.Sub(event) > maxAge || q.Flags&128 == 0 || q.Flags&12 != 0 {
		return errors.New("invalid or stale option clocks")
	}
	return nil
}

func validateValuationMarks(s sample, symbol string) error {
	seen := map[string]bool{}
	for _, q := range s.Quotes {
		seen[q.Contract] = true
	}
	for _, m := range s.Marks {
		if seen[m.Contract] || len(m.Contract) != 21 || strings.TrimSpace(m.Contract[:6]) != symbol || !validMoney(m.Ask) || m.AskSize < 1 || (m.Reason != "bid_unavailable" && m.Reason != "extended_window_observation") {
			return errors.New("invalid valuation-only ask")
		}
		seen[m.Contract] = true
		exp, err := time.Parse("20060102", "20"+m.Contract[6:12])
		if err != nil || !diagnosticExpiry(exp) || (m.Contract[12] != 'P' && m.Contract[12] != 'C') {
			return errors.New("invalid valuation contract identity")
		}
		for _, c := range m.Contract[13:] {
			if c < '0' || c > '9' {
				return errors.New("invalid valuation strike identity")
			}
		}
		recv, event := time.Unix(0, m.ReceiveNS), time.Unix(0, m.EventNS)
		if m.ReceiveNS <= 0 || m.EventNS <= 0 || event.After(recv) || !recv.Before(s.At) || s.At.Sub(event) > 5*time.Minute || m.Flags&128 == 0 || m.Flags&12 != 0 {
			return errors.New("invalid or stale valuation clocks")
		}
	}
	return nil
}

func selectContract(symbol string, p profile, fee int64, s sample, cost int64) (quote, float64, float64, map[string]int, bool) {
	c := wheel.DefaultConfig()
	// Historical counterfactuals retain their original eligibility. The shared
	// portfolio applies its explicitly reported capital policy at admission.
	c.MaxCollateral, c.MaxSymbols = wheel.MaxMoney, 1
	if p.Preset != "" {
		var err error
		c, err = c.WithStrategyPreset(p.Preset)
		if err != nil {
			return quote{}, 0, 0, map[string]int{"unknown_preset": 1}, false
		}
	}
	c.Symbols = []string{symbol}
	c.SelectionMode = p.Mode
	c.CallMaxDelta = p.CallCeiling
	c.MaxSpreadBPS = p.SpreadBPS
	c.Fee = wheel.Cents(fee)
	// The pilot deliberately omits the unqualified OI filter. This is disclosed.
	c.MinOI = 0
	if p.Mode == wheel.SelectionConservative {
		c.PutMinDelta, c.PutMaxDelta = .05, .15
	}
	kind := "put"
	if cost > 0 {
		kind = "call"
	}
	spot := (s.Stock.Bid + s.Stock.Ask) / 2
	quotes := []wheel.Quote{}
	models := map[string]optionmodel.IVResult{}
	originals := map[string]quote{}
	rejected := map[string]int{}
	for _, q := range s.Quotes {
		if q.Kind != kind {
			continue
		}
		expiryDate, _ := time.ParseInLocation("2006-01-02", q.Expiry, wheel.Eastern)
		expiry, open := wheel.SessionClose(expiryDate)
		if !open {
			rejected["expiry_calendar_rejected"]++
			continue
		}
		optionType := optionmodel.Put
		if kind == "call" {
			optionType = optionmodel.Call
		}
		iv, err := optionmodel.ImpliedVol(optionmodel.IVInput{Type: optionType,
			SpotCents: spot, StrikeCents: q.Strike, ObservedMidCents: (q.Bid + q.Ask) / 2,
			RiskFree: .04, DivYield: 0, HasRate: true, HasDiv: true, Expiry: expiry, Now: s.At,
			SpotKnownAt: s.Stock.At, OptionKnownAt: time.Unix(0, q.ReceiveNS), RateKnownAt: s.At, DivKnownAt: s.At})
		if err != nil {
			rejected["IV_model_rejected"]++
			continue
		}
		if kind == "put" && (math.Abs(iv.Delta) < p.PutFloor || math.Abs(iv.Delta) > p.PutCeiling) {
			rejected["profile_delta_filter"]++
			continue
		}
		models[q.Contract], originals[q.Contract] = iv, q
		quotes = append(quotes, wheel.Quote{Option: wheel.Option{ID: q.Contract, Symbol: symbol, Kind: q.Kind, Expiry: q.Expiry,
			Strike: wheel.Cents(q.Strike)}, Bid: wheel.Cents(q.Bid), Ask: wheel.Cents(q.Ask),
			Spot: wheel.Cents(spot), Delta: iv.Delta, BidSize: q.BidSize, OI: 0, Tick: wheel.Cents(orderIncrement(s.At.In(wheel.Eastern).Format("2006-01-02"), q.Contract, q.Bid)),
			Standard: true, Tradable: true, Weekly: true, At: time.Unix(0, q.ReceiveNS), UnderlyingAt: s.Stock.At})
	}
	candidate, ok := wheel.Select(c, symbol, kind, quotes, wheel.Cents(cost), s.At)
	if !ok {
		rejected["no_eligible_contract_in_sample"]++
		return quote{}, 0, 0, rejected, false
	}
	model := models[candidate.ID]
	return originals[candidate.ID], model.Delta, model.ImpliedVol, rejected, true
}

func (a *account) committed() int64 {
	if a.Shares == 100 {
		return a.Basis * 100
	}
	if a.Short != nil && a.Short.Kind == "put" {
		return a.Short.Strike * 100
	}
	return 0
}

func (a *account) sell(date string, q quote, fee, fillPrice int64, delta, vol float64) error {
	// Match production's pre-premium cash requirement: the submitted sale
	// must already have funds for the fee and full put collateral.
	if fee < 0 || a.Cash < fee || a.Short != nil || (q.Kind == "call" && a.Shares != 100) || (q.Kind == "put" && (a.Shares != 0 || a.Cash-fee < q.Strike*100)) {
		return errors.New("pilot collateral or coverage violation")
	}
	credit := fillPrice * 100
	if credit <= fee || a.Cash > int64(wheel.MaxMoney)-credit {
		return errors.New("invalid or overflowing premium")
	}
	a.Cash += credit - fee
	a.Premiums += credit
	a.Fees += fee
	a.Short = new(q)
	a.PeakCommitted = max(a.PeakCommitted, a.committed())
	a.Trades = append(a.Trades, trade{Date: date, Event: "modeled_sell_" + q.Kind, Contract: q.Contract, Strike: q.Strike,
		Price: fillPrice, Cashflow: credit - fee, Fee: fee, Delta: delta, ImpliedVol: vol})
	return nil
}

func (a *account) settle(date string, observed stock) error {
	if a.Short == nil || a.Short.Expiry != date {
		return nil
	}
	q := *a.Short
	if observed.Bid <= q.Strike && q.Strike <= observed.Ask {
		return &coverageError{fmt.Sprintf("ambiguous expiry stock spread: %s %s", date, q.Contract)}
	}
	spot := (observed.Bid + observed.Ask) / 2
	return a.settleSpot(date, spot)
}

// Settlement reference is separate from tradable stock quotes. Only the
// explicit endpoint-recovery path may use official end-of-day references.
func (a *account) settleSpot(date string, spot int64) error {
	if a.Short == nil || a.Short.Expiry != date {
		return nil
	}
	return a.settleOutcome(date, spot < a.Short.Strike, spot > a.Short.Strike)
}

func (a *account) settleOutcome(date string, putITM, callITM bool) error {
	if a.Short == nil || a.Short.Expiry != date {
		return nil
	}
	q := *a.Short
	event, flow := "modeled_expire_worthless", int64(0)
	if q.Kind == "put" && putITM {
		if a.Shares != 0 || a.Cash < q.Strike*100 {
			return errors.New("assignment cash not secured")
		}
		flow = -q.Strike * 100
		a.Shares, a.Basis = 100, q.Strike
		a.Assignments++
		event = "modeled_put_assignment"
	} else if q.Kind == "call" && callITM {
		if a.Shares != 100 {
			return errors.New("call-away not covered")
		}
		flow = q.Strike * 100
		a.StockRealized += (q.Strike - a.Basis) * 100
		a.Shares, a.Basis = 0, 0
		a.CallAways++
		event = "modeled_call_away"
	}
	a.Cash += flow
	a.Short = nil
	a.Trades = append(a.Trades, trade{Date: date, Event: event, Contract: q.Contract, Strike: q.Strike, Cashflow: flow})
	return nil
}

func (a *account) mark(d day) (point, error) {
	liability := int64(0)
	if a.Short != nil {
		found := false
		for _, q := range d.Close.Quotes {
			if q.Contract == a.Short.Contract {
				liability = q.Ask * 100
				found = true
				break
			}
		}
		if !found {
			for _, m := range d.Close.Marks {
				if m.Contract == a.Short.Contract {
					liability, found = m.Ask*100, true
					break
				}
			}
			if !found {
				return point{}, &missingMarkError{Date: d.Date, Contract: a.Short.Contract}
			}
		}
	}
	value := int64(a.Shares) * d.Close.Stock.Bid
	committed := a.committed()
	available := a.Cash
	if a.Short != nil && a.Short.Kind == "put" {
		available -= a.Short.Strike * 100
	}
	if available < 0 {
		return point{}, errors.New("negative unencumbered cash")
	}
	return point{d.Date, a.Cash, a.Shares, value, liability, committed, available, a.Cash + value - liability}, nil
}

type missingMarkError struct {
	Date, Contract string
}

type coverageError struct{ Message string }

func (e *coverageError) Error() string { return e.Message }

func (e *missingMarkError) Error() string {
	return fmt.Sprintf("missing held-short ask: %s %s", e.Date, e.Contract)
}

func simulate(in input, august, september profile, fee, allowance int64) (result, error) {
	a := account{Cash: initialCash}
	r := result{Profile: september, AugustProfile: august, Mode: august.Mode, FeePerContract: fee, Allowance: allowance, SelectionRejections: map[string]int{}}
	peak, drawdown, totalUse := float64(initialCash), 0.0, 0.0
	for _, d := range in.Days {
		current := august
		if d.Date >= "2026-09-01" {
			current = september
		}
		if d.Entry != nil && a.Short == nil {
			if (d.Entry.Stock.Ask-d.Entry.Stock.Bid)*10000 > d.Entry.Stock.Ask*100 {
				r.SelectionRejections["underlying_spread_exceeds_1pct"]++
			} else {
				q, delta, vol, reject, ok := selectContract(in.Symbol, current, fee, *d.Entry, a.Basis)
				for key, n := range reject {
					r.SelectionRejections[key] += n
				}
				if ok {
					o, fill := modelOrder(d, q, allowance)
					r.Orders = append(r.Orders, o)
					if fill {
						if err := a.sell(d.Date, q, fee, o.Limit, delta, vol); err != nil {
							return result{}, err
						}
						t := &a.Trades[len(a.Trades)-1]
						t.DecisionNS, t.DecisionQuoteNS, t.ExecutionNS, t.ObservedBid = o.DecisionNS, o.DecisionQuoteNS, o.ExecutionNS, o.ObservedBid
					}
				}
			}
		}
		if a.Short != nil && a.Short.Expiry < d.Date {
			return result{}, errors.New("unsettled expired option")
		}
		if err := a.settle(d.Date, d.Close.Stock); err != nil {
			return result{}, err
		}
		p, err := a.mark(d)
		if err != nil {
			return result{}, err
		}
		r.Daily = append(r.Daily, p)
		peak = max(peak, float64(p.Equity))
		drawdown = max(drawdown, (peak-float64(p.Equity))/peak*100)
		r.DrawdownCents = max(r.DrawdownCents, int64(peak)-p.Equity)
		totalUse += float64(p.Committed) / float64(initialCash) * 100
	}
	last := r.Daily[len(r.Daily)-1]
	r.EndingEquity, r.NetPnL = last.Equity, last.Equity-initialCash
	r.Premiums, r.Fees, r.StockRealized = a.Premiums, a.Fees, a.StockRealized
	r.StockUnrealized = last.StockValue - a.Basis*int64(a.Shares)
	r.Return = float64(r.NetPnL) / float64(initialCash) * 100
	if a.PeakCommitted > 0 {
		r.ReturnOnCommitted = new(float64(r.NetPnL) / float64(a.PeakCommitted) * 100)
	}
	r.Drawdown, r.AverageUse, r.PeakCommitted = drawdown, totalUse/float64(len(r.Daily)), a.PeakCommitted
	r.Assignments, r.CallAways = a.Assignments, a.CallAways
	r.EndingCash, r.EndingShares, r.EndingBasis, r.EndingShort, r.EndingLiability = a.Cash, a.Shares, a.Basis, a.Short, last.Liability
	r.Trades = a.Trades
	if r.NetPnL != a.Premiums-a.Fees+a.StockRealized+r.StockUnrealized-last.Liability {
		return result{}, errors.New("P&L components do not reconcile")
	}
	cash := initialCash
	for _, t := range a.Trades {
		cash += t.Cashflow
	}
	if cash != a.Cash {
		return result{}, errors.New("cash ledger does not reconcile")
	}
	r.AccountingReconciled = true
	r.Periods = periodMetrics(r)
	return r, nil
}

type metrics struct {
	Profile       profile `json:"profile"`
	NetPnL        int64   `json:"net_pnl_cents"`
	DrawdownCents int64   `json:"max_closing_drawdown_cents"`
	Score         int64   `json:"pnl_minus_drawdown_cents"`
	Assignments   int     `json:"modeled_assignments"`
	SoldContracts int     `json:"sold_contracts"`
	Eligible      bool    `json:"eligible_for_training_selection"`
	CoverageError string  `json:"coverage_error,omitempty"`
}

// Unpriced profiles have no invented zero P&L or risk metrics in the report.
func (m metrics) MarshalJSON() ([]byte, error) {
	if m.CoverageError != "" {
		return json.Marshal(struct {
			Profile       profile `json:"profile"`
			CoverageError string  `json:"coverage_error"`
			Eligible      bool    `json:"eligible_for_training_selection"`
		}{m.Profile, m.CoverageError, false})
	}
	type observedMetrics metrics
	return json.Marshal(observedMetrics(m))
}

func sold(r result) int {
	n := 0
	for _, t := range r.Trades {
		if strings.HasPrefix(t.Event, "modeled_sell_") {
			n++
		}
	}
	return n
}

func trainingMetrics(r result) metrics {
	n := sold(r)
	return metrics{Profile: r.Profile, NetPnL: r.NetPnL, DrawdownCents: r.DrawdownCents,
		Score: r.NetPnL - r.DrawdownCents, Assignments: r.Assignments, SoldContracts: n, Eligible: n >= 2}
}

func trainingCompare(a, b metrics) int {
	if (a.CoverageError == "") != (b.CoverageError == "") {
		if a.CoverageError == "" {
			return -1
		}
		return 1
	}
	if a.Eligible != b.Eligible {
		if a.Eligible {
			return -1
		}
		return 1
	}
	if a.Profile.Mode == wheel.SelectionConservative {
		if d := cmp.Compare(a.Assignments, b.Assignments); d != 0 {
			return d
		}
	}
	if d := cmp.Compare(b.Score, a.Score); d != 0 {
		return d
	}
	// Equivalent training evidence retains the existing behavior.
	if a.Profile.ID == "baseline" && b.Profile.ID != "baseline" {
		return -1
	}
	if b.Profile.ID == "baseline" && a.Profile.ID != "baseline" {
		return 1
	}
	return cmp.Or(cmp.Compare(a.Profile.PutFloor, b.Profile.PutFloor),
		cmp.Compare(a.Profile.PutCeiling, b.Profile.PutCeiling),
		cmp.Compare(b.Profile.CallCeiling, a.Profile.CallCeiling),
		cmp.Compare(a.Profile.SpreadBPS, b.Profile.SpreadBPS), cmp.Compare(a.Profile.ID, b.Profile.ID))
}
