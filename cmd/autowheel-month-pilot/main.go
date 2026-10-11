// autowheel-month-pilot is an offline, explicitly approximate monthly diagnostic.
// It has no broker, credential, order submission, or production-state interface.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"autowheel/internal/optionmodel"
	"autowheel/internal/wheel"
)

const initialCash int64 = 8_000_000

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
}

type sample struct {
	At     time.Time `json:"at"`
	Stock  stock     `json:"stock"`
	Quotes []quote   `json:"quotes"`
}

type day struct {
	Date  string  `json:"date"`
	Entry *sample `json:"entry,omitempty"`
	Close sample  `json:"close"`
}

type input struct {
	Schema  string            `json:"schema"`
	Symbol  string            `json:"symbol"`
	Days    []day             `json:"days"`
	Sources map[string]string `json:"source_sha256"`
}

type trade struct {
	Date       string  `json:"date"`
	Event      string  `json:"event"`
	Contract   string  `json:"contract"`
	Strike     int64   `json:"strike_cents"`
	Price      int64   `json:"price_cents"`
	Cashflow   int64   `json:"cashflow_cents"`
	Fee        int64   `json:"fee_cents"`
	Delta      float64 `json:"modeled_delta,omitzero"`
	ImpliedVol float64 `json:"modeled_implied_vol,omitzero"`
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
	Mode                 wheel.SelectionMode `json:"mode"`
	FeePerContract       int64               `json:"fee_per_contract_cents"`
	EndingEquity         int64               `json:"ending_equity_cents"`
	NetPnL               int64               `json:"net_pnl_cents"`
	Premiums             int64               `json:"premium_cents"`
	Fees                 int64               `json:"fees_cents"`
	StockRealized        int64               `json:"stock_realized_cents"`
	StockUnrealized      int64               `json:"stock_unrealized_cents"`
	Return               float64             `json:"return_on_80000_pct"`
	ReturnOnCommitted    float64             `json:"return_on_peak_committed_pct"`
	Drawdown             float64             `json:"close_sample_max_drawdown_pct"`
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

func validateSample(s sample) error {
	if s.At.IsZero() || !validMoney(s.Stock.Bid) || !validMoney(s.Stock.Ask) || s.Stock.Ask < s.Stock.Bid || s.Stock.At.IsZero() || s.Stock.At.After(s.At) || s.At.Sub(s.Stock.At) > 30*time.Second {
		return errors.New("missing, invalid or stale stock observation")
	}
	seen := map[string]bool{}
	for _, q := range s.Quotes {
		if seen[q.Contract] || len(q.Contract) != 21 || q.Contract[:6] != "NVDA  " || (q.Kind != "put" && q.Kind != "call") || !validMoney(q.Strike) || !validMoney(q.Bid) || !validMoney(q.Ask) || q.Ask < q.Bid || q.BidSize < 1 || q.AskSize < 1 {
			return errors.New("invalid or duplicate option quote")
		}
		seen[q.Contract] = true
		expiry, err := time.Parse("2006-01-02", q.Expiry)
		if err != nil || expiry.Format("060102") != q.Contract[6:12] || expiry.Weekday() != time.Friday {
			return errors.New("option expiry differs from pilot scope")
		}
		for _, digit := range q.Contract[13:] {
			if digit < '0' || digit > '9' {
				return errors.New("invalid OSI strike")
			}
		}
		rawStrike, err := strconv.ParseInt(q.Contract[13:], 10, 64)
		if err != nil || rawStrike != q.Strike*10 || q.Strike < 19000 || q.Strike > 24500 || (q.Kind == "put" && q.Contract[12] != 'P') || (q.Kind == "call" && q.Contract[12] != 'C') {
			return errors.New("option identity fields disagree")
		}
		recv, event := time.Unix(0, q.ReceiveNS), time.Unix(0, q.EventNS)
		if q.ReceiveNS <= 0 || q.EventNS <= 0 || event.After(recv) || recv.After(s.At) || s.At.Sub(event) > 30*time.Second {
			return errors.New("invalid or stale option clocks")
		}
	}
	return nil
}

func selectContract(mode wheel.SelectionMode, fee int64, s sample, cost int64) (quote, float64, float64, map[string]int, bool) {
	c := wheel.DefaultConfig()
	c.Symbols = []string{"NVDA"}
	c.SelectionMode = mode
	c.Fee = wheel.Cents(fee)
	// The pilot deliberately omits the unqualified OI filter. This is disclosed.
	c.MinOI = 0
	if mode == wheel.SelectionConservative {
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
		expiry, _ := time.ParseInLocation("2006-01-02 15:04", q.Expiry+" 16:00", wheel.Eastern)
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
		models[q.Contract], originals[q.Contract] = iv, q
		quotes = append(quotes, wheel.Quote{Option: wheel.Option{ID: q.Contract, Symbol: "NVDA", Kind: q.Kind, Expiry: q.Expiry,
			Strike: wheel.Cents(q.Strike)}, Bid: wheel.Cents(q.Bid), Ask: wheel.Cents(q.Ask),
			Spot: wheel.Cents(spot), Delta: iv.Delta, BidSize: q.BidSize, OI: 0, Tick: 1,
			Standard: true, Tradable: true, Weekly: true, At: time.Unix(0, q.ReceiveNS), UnderlyingAt: s.Stock.At})
	}
	candidate, ok := wheel.Select(c, "NVDA", kind, quotes, wheel.Cents(cost), s.At)
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

func (a *account) sell(date string, q quote, fee int64, delta, vol float64) error {
	if a.Short != nil || (q.Kind == "call" && a.Shares != 100) || (q.Kind == "put" && (a.Shares != 0 || a.Cash < q.Strike*100)) {
		return errors.New("pilot collateral or coverage violation")
	}
	credit := q.Bid * 100
	if credit <= fee || a.Cash > int64(wheel.MaxMoney)-credit {
		return errors.New("invalid or overflowing premium")
	}
	a.Cash += credit - fee
	a.Premiums += credit
	a.Fees += fee
	a.Short = new(q)
	a.PeakCommitted = max(a.PeakCommitted, a.committed())
	a.Trades = append(a.Trades, trade{Date: date, Event: "modeled_sell_" + q.Kind, Contract: q.Contract, Strike: q.Strike,
		Price: q.Bid, Cashflow: credit - fee, Fee: fee, Delta: delta, ImpliedVol: vol})
	return nil
}

func (a *account) settle(date string, spot int64) error {
	if a.Short == nil || a.Short.Expiry != date {
		return nil
	}
	q := *a.Short
	event, flow := "modeled_expire_worthless", int64(0)
	if q.Kind == "put" && spot < q.Strike {
		if a.Shares != 0 || a.Cash < q.Strike*100 {
			return errors.New("assignment cash not secured")
		}
		flow = -q.Strike * 100
		a.Shares, a.Basis = 100, q.Strike
		a.Assignments++
		event = "modeled_put_assignment"
	} else if q.Kind == "call" && spot > q.Strike {
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
			return point{}, fmt.Errorf("missing held-short ask: %s %s", d.Date, a.Short.Contract)
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

func simulate(in input, mode wheel.SelectionMode, fee int64) (result, error) {
	a := account{Cash: initialCash}
	r := result{Mode: mode, FeePerContract: fee, SelectionRejections: map[string]int{}}
	peak, drawdown, totalUse := float64(initialCash), 0.0, 0.0
	for _, d := range in.Days {
		if d.Entry != nil && a.Short == nil {
			q, delta, vol, reject, ok := selectContract(mode, fee, *d.Entry, a.Basis)
			for key, n := range reject {
				r.SelectionRejections[key] += n
			}
			if ok {
				if err := a.sell(d.Date, q, fee, delta, vol); err != nil {
					return result{}, err
				}
			}
		}
		if a.Short != nil && a.Short.Expiry < d.Date {
			return result{}, errors.New("unsettled expired option")
		}
		if err := a.settle(d.Date, (d.Close.Stock.Bid+d.Close.Stock.Ask)/2); err != nil {
			return result{}, err
		}
		p, err := a.mark(d)
		if err != nil {
			return result{}, err
		}
		r.Daily = append(r.Daily, p)
		peak = max(peak, float64(p.Equity))
		drawdown = max(drawdown, (peak-float64(p.Equity))/peak*100)
		totalUse += float64(p.Committed) / float64(initialCash) * 100
	}
	last := r.Daily[len(r.Daily)-1]
	r.EndingEquity, r.NetPnL = last.Equity, last.Equity-initialCash
	r.Premiums, r.Fees, r.StockRealized = a.Premiums, a.Fees, a.StockRealized
	r.StockUnrealized = last.StockValue - a.Basis*int64(a.Shares)
	r.Return = float64(r.NetPnL) / float64(initialCash) * 100
	if a.PeakCommitted > 0 {
		r.ReturnOnCommitted = float64(r.NetPnL) / float64(a.PeakCommitted) * 100
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
	return r, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	path := flag.String("input", "", "complete bounded pilot input")
	output := flag.String("output", "", "new report file; never overwrites")
	flag.Parse()
	if *path == "" || *output == "" || flag.NArg() != 0 {
		return errors.New("provide --input FILE --output NEW_FILE")
	}
	b, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	if len(b) > 8<<20 {
		return errors.New("input exceeds pilot bound")
	}
	var in input
	if err = json.Unmarshal(b, &in); err != nil {
		return err
	}
	if in.Schema != "AutoWheelMonthlyDiagnosticInputV1" || in.Symbol != "NVDA" || len(in.Days) != 21 || len(in.Sources) == 0 {
		return errors.New("incorrect pilot input scope")
	}
	expected := []string{}
	start, _ := time.Parse("2006-01-02", "2026-09-01")
	for d := start; d.Month() == time.September; d = d.AddDate(0, 0, 1) {
		if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday && d.Format("2006-01-02") != "2026-09-07" {
			expected = append(expected, d.Format("2006-01-02"))
		}
	}
	entries := map[string]bool{"2026-09-01": true, "2026-09-08": true, "2026-09-14": true, "2026-09-21": true, "2026-09-28": true}
	for i, d := range in.Days {
		if d.Date != expected[i] || (d.Entry != nil) != entries[d.Date] || d.Close.At.In(wheel.Eastern).Format("2006-01-02 15:04:05") != d.Date+" 16:00:00" {
			return errors.New("missing or incorrectly scheduled session")
		}
		if err = validateSample(d.Close); err != nil {
			return fmt.Errorf("%s close: %w", d.Date, err)
		}
		if d.Entry != nil {
			if d.Entry.At.In(wheel.Eastern).Format("2006-01-02 15:04:05") != d.Date+" 09:35:00" {
				return errors.New("wrong entry clock")
			}
			if err = validateSample(*d.Entry); err != nil {
				return fmt.Errorf("%s entry: %w", d.Date, err)
			}
		}
	}
	results := []result{}
	for _, fee := range []int64{100, 200} {
		for _, mode := range []wheel.SelectionMode{wheel.SelectionConservative, wheel.SelectionBalanced} {
			r, e := simulate(in, mode, fee)
			if e != nil {
				return fmt.Errorf("%s: %w", mode, e)
			}
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
	digest, codeDigest := sha256.Sum256(b), sha256.Sum256(code)
	report := map[string]any{"schema": "AutoWheelMonthlyDiagnosticReportV1", "status": "approximate_monthly_diagnostic",
		"start": "2026-09-01", "end": "2026-09-30", "symbol": "NVDA", "initial_cash_cents": initialCash,
		"input_sha256": hex.EncodeToString(digest[:]), "executable_sha256": hex.EncodeToString(codeDigest[:]),
		"source_sha256": in.Sources, "results": results, "production_qualified": false,
		"assumptions": []string{
			"Actual OPRA observed bid/ask snapshots; modeled instant sale at the observed bid. No queue, dwell, GFD or intraday execution replay.",
			"One weekly entry opportunity at09:35 Eastern on September1,8,14,21,28; other days are closing marks only. Calls use Friday expiries. This differs from continuous production scanning and shorter-dated calls.",
			"Existing wheel.Select rankings and mode bands reused on a limited $190–$245 strike roster. Open-interest filter disabled because causal OI is unqualified; standard100-share deliverables, tradability and one-cent ticks are assumptions.",
			"Delta reconstructed with European BSM implied volatility from observed midquotes floored to cents; fixed4% rate and zero dividend yield are research assumptions, not observed historical Greeks/rates/dividends.",
			"Stock input is actual EQUS.MINI component-venue bid/ask, not SIP NBBO or official closing trades. Dividend cashflows and corporate actions are omitted and unaudited.",
			"Assignment only at expiry using the closing component-venue stock midpoint. No early exercise, after-hours exercise, broker reporting delay or pin assignment; an exactly-at-strike option expires worthless. Assigned shares assumed settled by the next weekly entry.",
			"Stock valued at observed bid and held shorts at observed ask. End-of-month open liabilities and inventory included. Drawdown and mean capital use measured on21 closing samples, not continuously.",
			"Fees $1 per sold contract baseline and $2 stress; no additional exercise/assignment/equity commissions, cash interest, slippage, taxes or data charges in strategy P&L.",
		}}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(append(raw, '\n'))
	return errors.Join(writeErr, f.Close())
}
