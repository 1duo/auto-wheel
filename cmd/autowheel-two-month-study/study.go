package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"autowheel/internal/wheel"
)

// Selection has already finished when this function sees an execution book.
// The limit is fixed. An invalid or adverse first book cannot be skipped.
func modelOrder(d day, q quote, allowance int64) (order, bool) {
	limit := q.Bid - allowance
	if allowance < 0 {
		limit = 0
	} else if limit > 0 {
		limit -= limit % orderIncrement(d.Date, q.Contract, limit)
	}
	o := order{Date: d.Date, Contract: q.Contract, DecisionNS: d.Entry.At.UnixNano(), DecisionBid: q.Bid,
		DecisionQuoteNS: q.ReceiveNS, Limit: limit, Status: "unfilled_no_observed_first_book"}
	if o.Limit <= 0 {
		o.Status = "unfilled_invalid_limit"
		return o, false
	}
	if d.Execution == nil {
		return o, false
	}
	for _, b := range d.Execution.Books {
		if b.Contract != q.Contract {
			continue
		}
		if b.Status != "observed_first_book" || b.Quote == nil {
			o.Status = "unfilled_" + b.Status
			return o, false
		}
		x := *b.Quote
		o.ExecutionNS, o.ObservedBid = x.ReceiveNS, x.Bid
		if x.Contract != q.Contract || x.ReceiveNS < o.DecisionNS+int64(time.Second) || x.ReceiveNS >= d.Execution.At.UnixNano() ||
			x.EventNS <= 0 || x.EventNS > x.ReceiveNS || d.Execution.At.UnixNano()-x.EventNS > int64(time.Minute) ||
			x.Flags&128 == 0 || x.Flags&12 != 0 || x.Bid <= 0 || x.Ask < x.Bid || x.BidSize < 1 || x.AskSize < 1 {
			o.Status = "unfilled_invalid_first_book"
			return o, false
		}
		if x.Bid < o.Limit {
			o.Status = "unfilled_first_bid_below_limit"
			return o, false
		}
		o.Status = "modeled_filled_at_fixed_limit"
		return o, true
	}
	return o, false
}

// The bounded July–September universe has dated positive Penny Program facts.
// The threshold applies to the submitted simple-order price, not the quote.
func pennyIncrement(price int64) int64 {
	if price < 300 {
		return 1
	}
	return 5
}

// NOW entered the Penny Program on July 1, 2026. Earlier 2026 simple
// orders use five cents below $3 and ten cents at or above $3.
// https://www.nasdaqtrader.com/MicroNews.aspx?id=OTA+2026-25
func orderIncrement(date, contract string, price int64) int64 {
	if date >= "2026-01-01" && date < "2026-07-01" && strings.HasPrefix(contract, "NOW   ") {
		if price < 300 {
			return 5
		}
		return 10
	}
	return pennyIncrement(price)
}

func studyMonths(first, last string) []string {
	months := []string{}
	date, _ := time.Parse("2006-01-02", first[:7]+"-01")
	for date.Format("2006-01") <= last[:7] {
		months = append(months, date.Format("2006-01"))
		date = date.AddDate(0, 1, 0)
	}
	return months
}

func periodMetrics(r result) map[string]period {
	all := map[string]period{}
	start := initialCash
	for _, month := range []string{"2026-08", "2026-09"} {
		if !slices.ContainsFunc(r.Daily, func(p point) bool { return strings.HasPrefix(p.Date, month) }) {
			continue
		}
		m := period{StartEquity: start}
		peak := start
		for _, p := range r.Daily {
			if !strings.HasPrefix(p.Date, month) {
				continue
			}
			peak = max(peak, p.Equity)
			m.Drawdown = max(m.Drawdown, peak-p.Equity)
			m.DrawdownPct = max(m.DrawdownPct, float64(peak-p.Equity)/float64(peak)*100)
			m.NetPnL = p.Equity - start
			if p.Shares > 0 {
				m.HoldingDays++
			}
		}
		for _, t := range r.Trades {
			if !strings.HasPrefix(t.Date, month) {
				continue
			}
			if strings.HasPrefix(t.Event, "modeled_sell_") {
				m.Sales++
				m.Premiums += t.Price * 100
				m.Fees += t.Fee
			}
			if t.Event == "modeled_put_assignment" {
				m.Assignments++
			}
			if t.Event == "modeled_call_away" {
				m.Calls++
			}
		}
		m.Return = float64(m.NetPnL) / float64(initialCash) * 100
		all[month] = m
		start += m.NetPnL
	}
	return all
}

type trainingRun struct {
	Profile profile `json:"profile"`
	Result  *result `json:"result,omitempty"`
	Error   string  `json:"coverage_error,omitempty"`
}

type selection struct {
	Mode             wheel.SelectionMode `json:"mode"`
	Candidate        profile             `json:"training_selected_profile"`
	Recommended      profile             `json:"research_recommended_profile"`
	Status           string              `json:"status"`
	Reasons          []string            `json:"validation_reasons"`
	Training         []metrics           `json:"training_grid"`
	TrainingRuns     []trainingRun       `json:"training_ledgers"`
	Complete         bool                `json:"training_coverage_complete"`
	CoverageFailures []string            `json:"evaluation_coverage_errors,omitempty"`
}

func isCoverage(err error) bool {
	_, missing := errors.AsType[*missingMarkError](err)
	_, ambiguity := errors.AsType[*coverageError](err)
	return missing || ambiguity
}

func chooseTraining(in input, mode wheel.SelectionMode) (profile, []metrics, []trainingRun, error) {
	train := in
	train.Days = slices.DeleteFunc(slices.Clone(in.Days), func(d day) bool { return d.Date >= "2026-09-01" })
	grid, runs := []metrics{}, []trainingRun{}
	for _, p := range profiles(mode) {
		r, err := simulate(train, p, p, 200, 1)
		if err != nil {
			if !isCoverage(err) {
				return profile{}, nil, nil, err
			}
			grid = append(grid, metrics{Profile: p, CoverageError: err.Error()})
			runs = append(runs, trainingRun{Profile: p, Error: err.Error()})
			continue
		}
		grid = append(grid, trainingMetrics(r))
		runs = append(runs, trainingRun{Profile: p, Result: new(r)})
	}
	slices.SortFunc(grid, trainingCompare)
	if !grid[0].Eligible || slices.ContainsFunc(grid, func(m metrics) bool { return m.CoverageError != "" }) {
		return baseline(mode), grid, runs, nil
	}
	return grid[0].Profile, grid, runs, nil
}

func validateCandidate(base, chosen result, changedCall bool) (bool, bool) {
	b, c := base.Periods["2026-09"], chosen.Periods["2026-09"]
	pass := c.Sales >= 1 && c.NetPnL >= b.NetPnL && c.DrawdownPct <= b.DrawdownPct+1e-12 &&
		(chosen.Mode != wheel.SelectionConservative || c.Assignments <= b.Assignments)
	if changedCall {
		pass = pass && c.HoldingDays <= b.HoldingDays && chosen.EndingShares <= base.EndingShares &&
			slices.ContainsFunc(chosen.Trades, func(t trade) bool { return t.Date >= "2026-09-01" && t.Event == "modeled_sell_call" })
	}
	better := c.NetPnL > b.NetPnL || c.DrawdownPct < b.DrawdownPct-1e-12 || (chosen.Mode == wheel.SelectionConservative && c.Assignments < b.Assignments)
	return pass, better
}

func study(in input) ([]result, []selection, error) {
	all, selections := []result{}, []selection{}
	for _, mode := range []wheel.SelectionMode{wheel.SelectionConservative, wheel.SelectionBalanced} {
		candidate, grid, runs, err := chooseTraining(in, mode)
		if err != nil {
			return nil, nil, err
		}
		complete := !slices.ContainsFunc(grid, func(m metrics) bool { return m.CoverageError != "" })
		s := selection{Mode: mode, Candidate: candidate, Recommended: baseline(mode), Status: "retain_baseline", Training: grid, TrainingRuns: runs, Complete: complete}
		pass, better := true, false
		for _, costs := range [][2]int64{{100, 0}, {200, 1}} {
			base, e1 := simulate(in, baseline(mode), baseline(mode), costs[0], costs[1])
			chosen, e2 := simulate(in, baseline(mode), candidate, costs[0], costs[1])
			for i, e := range []error{e1, e2} {
				if e == nil {
					continue
				}
				if !isCoverage(e) {
					return nil, nil, e
				}
				pass = false
				s.CoverageFailures = append(s.CoverageFailures, fmt.Sprintf("fee=%d role=%d: %s", costs[0], i, e))
			}
			if e1 == nil {
				base.Role = "baseline"
				all = append(all, base)
			}
			if e2 == nil {
				chosen.Role = "training_selected_candidate"
				all = append(all, chosen)
			}
			if e1 != nil || e2 != nil {
				continue
			}
			b, c := base.Periods["2026-08"], chosen.Periods["2026-08"]
			if b != c {
				return nil, nil, errors.New("candidate and control August states differ")
			}
			ok, improved := validateCandidate(base, chosen, candidate.CallCeiling != baseline(mode).CallCeiling)
			pass = pass && ok
			better = better || improved
		}
		if !complete {
			s.Status = "retain_baseline_incomplete_training_coverage"
			s.Reasons = append(s.Reasons, "Incomplete August grid: unpriced alternatives have absent metrics and cannot justify tuning.")
		}
		if len(s.CoverageFailures) > 0 {
			s.Status = "evaluation_coverage_failure"
			s.Reasons = append(s.Reasons, "Some continuous portfolios lack prices or an unambiguous expiry. No promotion is allowed.")
		}
		if candidate.ID != "baseline" && complete && pass && better {
			s.Recommended, s.Status = candidate, "candidate_passed_retrospective_september"
			s.Reasons = append(s.Reasons, "Passed both fixed cost scenarios from common August inventory. This previously inspected September sample is not blind evidence or a proven optimum.")
		} else {
			s.Reasons = append(s.Reasons, "Retain baseline unless both September cost cases pass all activity, loss, drawdown, assignment and exit guards with strict improvement.")
		}
		selections = append(selections, s)
	}
	return all, selections, nil
}

func validateInput(in input) error {
	if in.Schema == "AutoWheelCalendarYearEndpointInputV1" {
		return validateCalendarYearInput(in)
	}
	if in.Schema == "AutoWheelYearToDateEndpointInputV1" {
		return validateYearToDateInput(in)
	}
	threeMonth := in.Schema == "AutoWheelThreeMonthEndpointInputV1"
	count, first := 42, "2026-08-01"
	if threeMonth {
		count, first = 64, "2026-07-01"
	}
	// Keep the study's original universe independent of later production pruning.
	studySymbols := []string{"NFLX", "TSLA", "NVDA", "NBIS", "IREN", "NOW", "AMD", "AMZN", "GOOGL", "META", "MSFT", "PLTR", "UBER", "AVGO", "ORCL"}
	if (in.Schema != "AutoWheelTwoMonthInputV1" && !threeMonth) || !slices.Contains(studySymbols, in.Symbol) || len(in.Days) != count || len(in.Sources) == 0 {
		return errors.New("incorrect bounded study input scope")
	}
	expected := []string{}
	start, _ := time.Parse("2006-01-02", first)
	for d := start; d.Format("2006-01-02") <= "2026-09-30"; d = d.AddDate(0, 0, 1) {
		if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday && d.Format("2006-01-02") != "2026-09-07" && d.Format("2006-01-02") != "2026-07-03" {
			expected = append(expected, d.Format("2006-01-02"))
		}
	}
	entries := []string{"2026-08-03", "2026-08-10", "2026-08-17", "2026-08-24", "2026-08-31", "2026-09-08", "2026-09-14", "2026-09-21", "2026-09-28"}
	if threeMonth {
		entries = append([]string{"2026-07-01", "2026-07-06", "2026-07-13", "2026-07-20", "2026-07-27"}, entries...)
	}
	return validateStudySchedule(in, expected, entries)
}

func validateStudySchedule(in input, expected, entries []string) error {
	if len(in.Days) != len(expected) {
		return errors.New("missing study session")
	}
	for i, d := range in.Days {
		closeAt, open := wheel.SessionClose(d.Close.At)
		if !open || d.Date != expected[i] || (d.Entry != nil) != slices.Contains(entries, d.Date) || d.Close.At.In(wheel.Eastern).Format("2006-01-02") != d.Date || !d.Close.At.Equal(closeAt) {
			return errors.New("incorrect session schedule")
		}
		if err := validateSample(d.Close, in.Symbol); err != nil {
			return fmt.Errorf("%s close: %w", d.Date, err)
		}
		if err := validateValuationMarks(d.Close, in.Symbol); err != nil {
			return err
		}
		if d.Entry == nil {
			if d.Execution != nil {
				return errors.New("unscheduled execution")
			}
			continue
		}
		if len(d.Entry.Marks) > 0 || d.Entry.At.In(wheel.Eastern).Format("2006-01-02 15:04:05") != d.Date+" 09:35:00" {
			return errors.New("incorrect decision sample")
		}
		if err := validateSample(*d.Entry, in.Symbol); err != nil {
			return fmt.Errorf("%s entry: %w", d.Date, err)
		}
		if d.Execution == nil || d.Execution.NotBeforeNS != d.Entry.At.UnixNano()+int64(time.Second) || !d.Execution.At.Equal(d.Entry.At.Add(time.Minute)) {
			return errors.New("missing or incorrect execution window")
		}
		seen := map[string]bool{}
		for _, b := range d.Execution.Books {
			if seen[b.Contract] || len(b.Contract) != 21 || strings.TrimSpace(b.Contract[:6]) != in.Symbol || b.Status == "" {
				return errors.New("invalid or duplicate execution outcome")
			}
			seen[b.Contract] = true
			if (b.Status == "observed_first_book") != (b.Quote != nil) {
				return errors.New("execution status/quote differs")
			}
			if b.Quote != nil {
				x := *b.Quote
				if err := validateQuote(x, in.Symbol, d.Execution.At, time.Minute); err != nil {
					return fmt.Errorf("execution: %w", err)
				}
				if x.Contract != b.Contract || x.ReceiveNS < d.Execution.NotBeforeNS || x.ReceiveNS >= d.Execution.At.UnixNano() {
					return errors.New("execution outside causal window")
				}
			}
		}
		for _, x := range d.Entry.Quotes {
			if !seen[x.Contract] {
				return errors.New("selected roster lacks an execution outcome")
			}
		}
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("input", "", "bounded input")
	output := flag.String("output", "", "exclusive report")
	endpoint := flag.Bool("endpoint-recovery", false, "separate baseline-only endpoint diagnostic; missing daily equity remains null")
	portfolio := flag.Bool("shared-portfolio", false, "offline ten-symbol ROI-ranked account with three managed slots")
	availablePresets := flag.Bool("available-presets", false, "compare four fixed research presets on finished 2026 endpoint inputs; always unqualified")
	studyStart := flag.String("study-start", "", "first finished session for an independent available-presets replay")
	studyEnd := flag.String("study-end", "", "last finished session for an independent available-presets replay")
	studySymbols := flag.String("study-symbols", "", "comma-separated frozen 2026 research symbols; requires available-presets")
	studyPreset := flag.String("study-preset", "", "one frozen research preset; requires available-presets")
	limitSymbolCollateral := flag.Bool("limit-symbol-collateral", false, "apply the native fixed account ceiling / three per-symbol admission limit; shared 2026 endpoint only")
	includeAdaptive := flag.Bool("include-adaptive", false, "include the research-only adaptive comparison; requires available-presets")
	calendarYear := flag.Int("calendar-year", 0, "2024 or 2025 shared portfolio with frozen research presets")
	frozenSelection := flag.String("frozen-selection", "", "2024-only frozen selection required for the 2025 evaluation")
	planOnly := flag.Bool("plan-only", false, "write the frozen calendar-year schedule and strategy plan without reading market data")
	seed := flag.Int64("starting-cash-cents", 0, "observed cash seed for shared portfolio")
	ceiling := flag.Int64("max-collateral-cents", 15_000_000, "shared portfolio exposure ceiling; puts remain cash-secured")
	flag.Parse()
	if (*limitSymbolCollateral && (!*portfolio || *calendarYear != 0 || *planOnly || *endpoint)) || (*includeAdaptive && !*availablePresets) {
		return errors.New("symbol limit requires the shared 2026 endpoint path; adaptive comparison requires available-presets")
	}
	if (*availablePresets && (!*portfolio || *calendarYear != 0 || *planOnly || *endpoint || *frozenSelection != "")) || (!*availablePresets && (*studyStart != "" || *studyEnd != "" || *studySymbols != "" || *studyPreset != "")) {
		return errors.New("available-presets requires the finished shared-portfolio endpoint path; period bounds require available-presets")
	}
	if *planOnly {
		if *path != "" || *output == "" || flag.NArg() != 0 || !*portfolio {
			return errors.New("plan-only requires shared-portfolio, calendar-year and output; no input")
		}
		return writeCalendarYearPlan(*output, *calendarYear)
	}
	if *path == "" || *output == "" || flag.NArg() != 0 {
		return errors.New("provide --input and --output")
	}
	if *portfolio {
		if *calendarYear != 0 {
			return runCalendarYearPortfolio(*path, *output, *seed, *ceiling, *calendarYear, *frozenSelection)
		}
		if *availablePresets {
			return runPortfolioUniverseWithPolicy(*path, *output, *seed, *ceiling, true, *studyStart, *studyEnd, *studySymbols, *studyPreset, portfolioStudyPolicy{LimitSymbolCollateral: *limitSymbolCollateral, IncludeAdaptive: *includeAdaptive})
		}
		return runPortfolioUniverseWithPolicy(*path, *output, *seed, *ceiling, false, "", "", "", "", portfolioStudyPolicy{LimitSymbolCollateral: *limitSymbolCollateral})
	}
	if *calendarYear != 0 {
		return errors.New("calendar-year requires shared-portfolio")
	}
	b, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	if len(b) > 64<<20 {
		return errors.New("input bound exceeded")
	}
	var in input
	if err = json.Unmarshal(b, &in); err != nil {
		return err
	}
	if *endpoint {
		err = validateEndpointInput(in)
	} else {
		if in.Schema == "AutoWheelThreeMonthEndpointInputV1" || in.Schema == "AutoWheelYearToDateEndpointInputV1" {
			return errors.New("three-month scope permits fixed baseline endpoint diagnostics only")
		}
		err = validateInput(in)
	}
	if err != nil {
		return err
	}
	var results any
	var selections []selection
	if *endpoint {
		results, err = endpointStudy(in)
	} else {
		results, selections, err = study(in)
	}
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	code, err := os.ReadFile(exe)
	if err != nil {
		return err
	}
	digest, codeDigest := sha256.Sum256(b), sha256.Sum256(code)
	report := map[string]any{"schema": "AutoWheelTwoMonthStudyV1", "status": "retrospective_approximate_diagnostic", "start": "2026-08-01", "end": "2026-09-30", "symbol": in.Symbol, "initial_cash_cents": initialCash,
		"input_sha256": hex.EncodeToString(digest[:]), "executable_sha256": hex.EncodeToString(codeDigest[:]), "source_sha256": in.Sources, "results": results, "selections": selections, "production_qualified": false, "blind_out_of_sample": false,
		"training_window": []string{"2026-08-03", "2026-08-31"}, "evaluation_window": []string{"2026-09-01", "2026-09-30"}}
	if *endpoint {
		report["schema"] = "AutoWheelTwoMonthEndpointStudyV1"
		delete(report, "selections")
		delete(report, "training_window")
		report["expiry_closes"] = in.ExpiryCloses
		report["tuning_permitted"] = false
		report["expiry_reference_publication_timestamps_qualified"] = false
		report["valuation_policy"] = "Observed fresh asks only; missing interior liability/equity stays null. Endpoint returns require actual boundary valuations; drawdown requires every daily valuation."
		report["expiry_policy"] = "Official consolidated close, expiry-only automatic exercise at one cent ITM; no early exercise, contrary instructions, after-hours changes or broker delays. Official bar midnight label is not its availability timestamp."
		report["simple_order_tick_policy"] = "Penny Program: 1 cent below $3, 5 cents at or above $3. Bid minus allowance rounded down at submitted limit price. Dated class facts bound externally; historical publication and broker routing unqualified."
		if in.Schema == "AutoWheelThreeMonthEndpointInputV1" {
			report["schema"] = "AutoWheelThreeMonthEndpointStudyV1"
			report["start"] = "2026-07-01"
			delete(report, "evaluation_window")
		}
		if in.Schema == "AutoWheelYearToDateEndpointInputV1" {
			report["schema"] = "AutoWheelYearToDateEndpointStudyV1"
			report["start"], report["end"] = in.Start, in.End
			report["valuation_policy"] = "Selection requires fresh two-sided quotes within 30 seconds. Closing liability uses the latest observed positive ask within five minutes, including separately typed valuation-only observations; this is not an instantaneous closing NBBO. Missing liability/equity stays null. Monthly returns require actual boundaries; closing drawdown requires every daily observation."
			delete(report, "evaluation_window")
			report["universe_selection_bias"] = "Contemporary ten-symbol universe selected using July–September 2026 results; these same-year observations are retrospective."
		}
	}
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
