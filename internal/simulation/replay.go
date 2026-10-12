package simulation

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"autowheel/internal/researchselection"
	"autowheel/internal/wheel"
)

type pending struct {
	Candidate             wheel.Candidate
	Order                 wheel.Order
	DwellStart, LastQuote time.Time
}
type inventory struct {
	Cost      wheel.Cents
	SettledAt time.Time
}
type credit struct {
	Amount    wheel.Cents
	SettledAt time.Time
}

// exclusionIdentity tracks roster-derived metadata for an ID ever seen in a
// listed chain, without inventing strike or deliverable attributes for a
// no-recent-book observation.
type exclusionIdentity struct {
	Symbol string
	Kind   string
	Expiry string
	Weekly bool
}

type replay struct {
	report                        Report
	state                         wheel.State
	cash                          wheel.Cents
	now                           time.Time
	quotes                        map[string]Quote
	catalog                       map[string]wheel.Option
	stocks                        map[string]Stock
	shorts                        map[string]wheel.Option
	shares                        map[string]inventory
	orders                        []wheel.Order
	pending                       *pending
	credits                       []credit
	metrics                       Metrics
	decisions, valuations         []time.Time
	decisionIndex, valuationIndex int
	seenRoster                    map[string]exclusionIdentity
	selectionStubs                map[string]wheel.Quote
	// omissionPre captures pre-transition held/pending identities so an ID that
	// required real execution/lifecycle source in the same frame can never be
	// omitted, even if fill/lifecycle/Reconcile cleared it post-transition.
	omissionRequiredBefore map[string]bool
}

// Replay reads a Spec followed by time-ordered Frame objects, one JSON object
// per line. Incomplete runs deliberately contain no performance/partial curve.
// The stream is hashed verbatim, including frames after a validation failure.
func Replay(input io.Reader) Report {
	hash := sha256.New()
	stream := io.TeeReader(input, hash)
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	r := replay{report: Report{Schema: "AutoWheelReplayReportV1", Status: "blocked", Assumptions: []string{
		"Counterfactual, initially all cash and no inventory; not actual broker fills or deployed performance.",
		"The declared fixed symbol universe was selected retrospectively; no parameter optimization.",
		"GFD sell limit at decision bid: 1-second latency, then 5-second sized bid dwell with observations no more than 1 second apart; no queue-priority proof or price improvement.",
		"Scanning and broker review are instantaneous modeled approvals; historical quotes cannot prove Robinhood acceptance, scan latency or execution. Production Execute pre-submit checks run against the simulated snapshot.",
		"Explicit modeled assignment/expiry/forced-close events; supplied settlement dates control cash/share reuse; production 24-hour ownership inference is retained.",
		"NAV includes economic cash and stock at bid less short-option ask liabilities; cash premiums alone are not profit. No interest, taxes or data-subscription costs.",
		"Drawdown is measured at session closes; intraday drawdown can be worse. Coverage declarations require independent vendor/normalizer audit; they are not broker receipts.",
		"Peer reservation scenarios hold idle cash and exclude peer symbols; they do not reconstruct historical TradePulse trades or profits.",
	}}}
	line := 0
	var failure error
	for scanner.Scan() {
		line++
		if failure != nil {
			continue
		}
		if line == 1 {
			failure = decode(scanner.Bytes(), &r.report.Spec)
			if failure == nil {
				failure = r.init()
			}
		} else {
			var frame Frame
			failure = decode(scanner.Bytes(), &frame)
			if failure == nil {
				failure = r.step(frame)
			}
		}
		if failure != nil {
			failure = fmt.Errorf("line %d: %w", line, failure)
		}
	}
	if scanner.Err() != nil {
		_, drainErr := io.Copy(io.Discard, stream)
		failure = errors.Join(failure, scanner.Err(), drainErr)
	}
	if line == 0 {
		failure = errors.New("empty replay: no spec or historical observations")
	}
	r.report.InputSHA256 = hex.EncodeToString(hash.Sum(nil))
	r.report.ObservedDecisions, r.report.ObservedValuations = r.decisionIndex, r.valuationIndex
	if failure == nil && (r.decisionIndex != len(r.decisions) || r.valuationIndex != len(r.valuations)) {
		failure = fmt.Errorf("incomplete coverage: decisions %d/%d, valuations %d/%d", r.decisionIndex, len(r.decisions), r.valuationIndex, len(r.valuations))
	}
	if failure != nil {
		r.report.Blockers = []string{failure.Error()}
		r.report.Equity, r.report.Trades = nil, nil
		return r.report
	}
	r.report.Status = "counterfactual"
	if r.report.Spec.Mode == "fixture" {
		r.report.Status = "fixture"
	}
	last := r.report.Equity[len(r.report.Equity)-1]
	r.metrics.EndingEquity = last.Equity
	r.metrics.NetPnL = last.Equity - r.report.Spec.InitialCash
	r.metrics.ReturnPct = float64(r.metrics.NetPnL) / float64(r.report.Spec.InitialCash) * 100
	peak := r.report.Spec.InitialCash
	for _, p := range r.report.Equity {
		peak = max(peak, p.Equity)
		r.metrics.MaxDrawdownPct = max(r.metrics.MaxDrawdownPct, float64(peak-p.Equity)/float64(peak)*100)
	}
	r.report.Performance, r.report.EndState = &r.metrics, &r.state
	return r.report
}

func decode(data []byte, target any) error {
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("trailing JSON content")
	}
	return nil
}

func (r *replay) init() error {
	s := r.report.Spec
	if s.FrozenDevelopment != nil && s.ResearchVariant != ResearchMultiyearROI {
		return errors.New("frozen development evidence belongs only to the multiyear research variant")
	}
	if s.Schema != "AutoWheelReplaySpecV1" || (s.Mode != "fixture" && s.Mode != "historical_quote_replay") || s.Source == "" {
		return errors.New("invalid spec version, mode or source")
	}
	start, err := time.ParseInLocation("2006-01-02", s.Start, wheel.Eastern)
	if err != nil {
		return err
	}
	end, err := time.ParseInLocation("2006-01-02", s.End, wheel.Eastern)
	maxDays, maxCash := 200, wheel.Cents(8_000_000)
	if s.ResearchVariant == ResearchMultiyearROI {
		if !((s.Start == "2024-07-01" && s.End == "2024-12-31") || (s.Start == "2025-01-02" && s.End == "2025-12-31")) {
			return errors.New("multiyear replay requires the frozen July-December 2024 or full 2025 period")
		}
		maxDays, maxCash = 365, 15_000_000
	}
	if err != nil || end.Before(start) || end.Sub(start) > time.Duration(maxDays)*24*time.Hour {
		return errors.New("invalid replay date envelope")
	}
	if s.InitialCash <= 0 || s.InitialCash > maxCash || s.PeerReserve < 0 || s.PeerReserve >= s.InitialCash {
		return errors.New("invalid initial capital or peer reservation")
	}
	defaultConfig := wheel.DefaultConfig()
	validationConfig := s.Config
	switch s.ResearchVariant {
	case "":
		if s.Config.SelectionMode != "" {
			modeConfig, err := defaultConfig.WithSelectionMode(s.Config.SelectionMode)
			if err != nil {
				return err
			}
			defaultConfig = modeConfig
		}
	case ResearchPutRiskCap:
		if s.Config.PutMinDelta != 0 || (s.Config.PutMaxDelta != .10 && s.Config.PutMaxDelta != .15 && s.Config.PutMaxDelta != .20) {
			return errors.New("put-risk study requires a zero floor and explicit 0.10, 0.15 or 0.20 cap")
		}
		// Keep production Config.Validate strict. Only this named offline
		// study admits a zero floor; Select still requires negative delta.
		validationConfig.PutMinDelta = s.Config.PutMaxDelta
		defaultConfig.PutMinDelta = 0
		defaultConfig.PutMaxDelta = s.Config.PutMaxDelta
	case ResearchMultiyearROI:
		if s.Config.StrategyPreset == "" || s.Config.MaxCollateral != 15_000_000 || s.PeerReserve != 0 || len(s.PeerSymbols) != 0 {
			return errors.New("multiyear replay requires a named preset, $150,000 research ceiling and no peer inventory or reservation")
		}
		if s.Start == "2025-01-02" {
			if s.FrozenDevelopment == nil {
				return errors.New("2025 continuous replay requires frozen 2024 development evidence")
			}
			winner, err := researchselection.Validate(*s.FrozenDevelopment, int64(s.InitialCash))
			if err != nil {
				return err
			}
			if s.Config.StrategyPreset != winner && s.Config.StrategyPreset != researchselection.Baseline {
				return errors.New("held-out replay admits only the frozen 2024 winner and baseline")
			}
		} else if s.FrozenDevelopment != nil {
			return errors.New("development replay cannot consume a held-out selection")
		}
		var err error
		defaultConfig, err = defaultConfig.WithStrategyPreset(s.Config.StrategyPreset)
		if err != nil {
			return err
		}
		defaultConfig.MaxCollateral = 15_000_000
		// Only this exact offline variant changes the collateral envelope.
		// Validate every other field through the unchanged production gate.
		validationConfig.MaxCollateral = 8_000_000
	default:
		return errors.New("unknown research variant")
	}
	if err := validationConfig.Validate(); err != nil {
		return err
	}
	if s.Config.Account != "" || s.Config.TradePulseStateDir != "" || s.Config.CoordinatorSocket != "" || s.Config.CoordinatorPolicy != "" {
		return errors.New("simulation cannot bind production accounts, credentials or coordinator")
	}
	// Fee stress applies to the frozen baseline or a separately named study.
	validFee := s.Config.Fee == 100 || s.Config.Fee == 200
	if s.ResearchVariant == ResearchMultiyearROI {
		validFee = s.Config.Fee == 6 || s.Config.Fee == 200
	}
	if !validFee {
		return errors.New("fee must match the declared offline cost case")
	}
	defaultConfig.Fee = s.Config.Fee
	a, err := json.Marshal(defaultConfig)
	if err != nil {
		return err
	}
	b, err := json.Marshal(s.Config)
	if err != nil {
		return err
	}
	if string(a) != string(b) {
		return errors.New("strategy differs from the declared AutoWheel study")
	}
	if s.ResearchVariant == ResearchPutRiskCap {
		r.report.Assumptions = append(r.report.Assumptions, fmt.Sprintf("Separate put-risk study: verified strictly negative put delta with absolute cap %.2f and no positive floor; nearest weekly expiry and net premium/collateral/calendar-day ranking retained. Calls and all other strategy gates are unchanged. Delta is a sensitivity proxy, not an assignment probability.", s.Config.PutMaxDelta))
	}
	if s.ResearchVariant == ResearchMultiyearROI {
		r.report.Assumptions = append(r.report.Assumptions, "Separate continuous multiyear confirmation: the named frozen ROI preset and $150,000 offline ceiling apply to the declared ten-symbol universe. The observed current cash seed is a counterfactual, not historical account capital. Standard five-minute scanning and all chain/OI/Greek/continuous execution/settlement gates remain required. Six-cent or $2 fees are modeled; this path does not apply the endpoint diagnostic's additional one-cent price allowance or qualify its returns.")
	}
	seen := map[string]bool{}
	for _, symbol := range s.PeerSymbols {
		if seen[symbol] || !slices.Contains(defaultConfig.Symbols, symbol) {
			return errors.New("invalid peer symbol reservation")
		}
		seen[symbol] = true
	}
	r.cash, r.state = s.InitialCash, wheel.NewState()
	r.state.Account = "0000"
	r.quotes, r.stocks = map[string]Quote{}, map[string]Stock{}
	r.catalog = map[string]wheel.Option{}
	r.shorts, r.shares = map[string]wheel.Option{}, map[string]inventory{}
	r.seenRoster = map[string]exclusionIdentity{}
	r.selectionStubs = map[string]wheel.Quote{}
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		if !wheel.CalendarKnown(day) {
			return errors.New("replay outside reviewed session calendar")
		}
		for minute := 575; minute < 945; minute += s.Config.PollSeconds / 60 {
			t := day.Add(time.Duration(minute) * time.Minute)
			if wheel.TradingWindow(t) {
				r.decisions = append(r.decisions, t)
			}
		}
		if wheel.TradingWindow(day.Add(575 * time.Minute)) {
			r.valuations = append(r.valuations, sessionClose(day))
		}
	}
	if len(r.valuations) == 0 {
		return errors.New("no eligible sessions")
	}
	r.report.ExpectedDecisions, r.report.ExpectedValuations = len(r.decisions), len(r.valuations)
	if !s.CorporateActionsAudited {
		return errors.New("raw prices and corporate-action coverage have not been audited")
	}
	return nil
}

func sessionClose(at time.Time) time.Time {
	t := at.In(wheel.Eastern)
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, wheel.Eastern)
	minute := 960
	if !wheel.TradingWindow(d.Add(780 * time.Minute)) {
		minute = 780
	}
	return d.Add(time.Duration(minute) * time.Minute)
}

func (r *replay) step(f Frame) error {
	if f.At.IsZero() || (!r.now.IsZero() && !f.At.After(r.now)) {
		return errors.New("timestamps must strictly increase")
	}
	date := f.At.In(wheel.Eastern).Format("2006-01-02")
	if date < r.report.Spec.Start || date > r.report.Spec.End {
		return errors.New("observation outside spec window")
	}
	if !slices.Contains([]string{"market", "decision", "valuation", "lifecycle", "session_end"}, f.Kind) {
		return errors.New("unknown frame kind")
	}
	if f.UnsupportedAction != "" {
		return fmt.Errorf("unsupported corporate action: %s", f.UnsupportedAction)
	}
	if r.decisionIndex < len(r.decisions) && f.At.After(r.decisions[r.decisionIndex]) {
		return errors.New("missing scheduled five-minute decision")
	}
	if r.valuationIndex < len(r.valuations) && f.At.After(r.valuations[r.valuationIndex]) {
		return errors.New("missing session-close valuation")
	}
	if r.seenRoster == nil {
		r.seenRoster = map[string]exclusionIdentity{}
	}
	if r.selectionStubs == nil {
		r.selectionStubs = map[string]wheel.Quote{}
	}
	if len(f.NoRecentBook) > 0 && f.Kind != "decision" {
		return errors.New("no-recent-book observations only allowed on scheduled decision frames")
	}
	if len(f.SelectionOmissions) > 0 || f.SelectionProof != nil {
		if f.Kind != "decision" {
			return errors.New("selection omissions/proofs only allowed on scheduled decision frames")
		}
	}
	// Capture identities only at decisions, before fill/lifecycle/Reconcile.
	// A same-frame transition cannot erase a real execution/held-source need.
	if f.Kind == "decision" {
		if r.omissionRequiredBefore == nil {
			r.omissionRequiredBefore = map[string]bool{}
		}
		clear(r.omissionRequiredBefore)
		if r.pending != nil {
			r.omissionRequiredBefore[r.pending.Candidate.ID] = true
			if r.pending.Order.OptionID != "" {
				r.omissionRequiredBefore[r.pending.Order.OptionID] = true
			}
		}
		for id := range r.shorts {
			r.omissionRequiredBefore[id] = true
		}
		for _, pos := range r.state.Positions {
			if pos.PutID != "" {
				r.omissionRequiredBefore[pos.PutID] = true
			}
			if pos.CallID != "" {
				r.omissionRequiredBefore[pos.CallID] = true
			}
		}
	}
	r.now = f.At
	if f.Kind == "decision" {
		if r.decisionIndex >= len(r.decisions) || !f.At.Equal(r.decisions[r.decisionIndex]) {
			return errors.New("unscheduled decision")
		}
		complete := slices.Clone(f.CompleteSymbols)
		slices.Sort(complete)
		expected := slices.Clone(r.report.Spec.Config.Symbols)
		slices.Sort(expected)
		if !slices.Equal(complete, expected) {
			return errors.New("incomplete point-in-time chain inventory")
		}
		// A decision contains replacement chain snapshots, not an accumulation
		// of contracts that were listed or eligible on earlier dates.
		clear(r.quotes)
		clear(r.selectionStubs)
	}
	seenQuotes := map[string]bool{}
	for _, q := range f.Quotes {
		if seenQuotes[q.ID] || q.ID == "" || q.Quantity != 1 || !slices.Contains(r.report.Spec.Config.Symbols, q.Symbol) || (q.Kind != "put" && q.Kind != "call") || q.Strike <= 0 || q.Strike > wheel.MaxMoney/100 || q.Spot <= 0 || q.Spot > wheel.MaxMoney/100 || q.Bid < 0 || q.Ask < q.Bid || q.Ask > wheel.MaxMoney/100 || q.Tick <= 0 || q.OI < 0 || q.BidSize < 0 {
			return errors.New("invalid/duplicate option observation")
		}
		exp, err := time.ParseInLocation("2006-01-02", q.Expiry, wheel.Eastern)
		if err != nil {
			return err
		}
		if q.At.After(sessionClose(exp)) {
			return errors.New("option quote after expiration session close")
		}
		for _, t := range []time.Time{q.At, q.UnderlyingAt, q.ListedAt, q.OIAt} {
			if t.IsZero() || t.After(f.At) {
				return errors.New("missing or future option/underlying/listing/OI/Greek timestamp")
			}
		}
		if !q.ObservedAt.IsZero() && (q.ObservedAt.Before(q.At) || q.ObservedAt.After(f.At)) {
			return errors.New("invalid book observation timestamp")
		}
		if q.GreeksAt.After(f.At) {
			return errors.New("future selection Greeks")
		}
		if old, ok := r.catalog[q.ID]; ok && old != q.Option {
			return errors.New("contract identity reused or changed across snapshots")
		}
		if prior, ok := r.seenRoster[q.ID]; ok && (prior.Symbol != q.Symbol || prior.Kind != q.Kind || prior.Expiry != q.Expiry || prior.Weekly != q.Weekly) {
			return errors.New("contract identity reused or changed across snapshots")
		}
		r.catalog[q.ID] = q.Option
		if old, ok := r.quotes[q.ID]; ok && q.At.Before(old.At) {
			return errors.New("contract identity drift or quote time reversal")
		}
		if held, ok := r.shorts[q.ID]; ok && (held != q.Option || !q.Standard) {
			return errors.New("held contract identity/deliverable changed")
		}
		seenQuotes[q.ID], r.quotes[q.ID] = true, q
	}
	seenStocks := map[string]bool{}
	for _, s := range f.Stocks {
		if seenStocks[s.Symbol] {
			return errors.New("duplicate stock observation")
		}
		seenStocks[s.Symbol] = true
		if !slices.Contains(r.report.Spec.Config.Symbols, s.Symbol) || s.At.IsZero() || s.At.After(f.At) || s.Bid <= 0 || s.Ask < s.Bid || s.Ask > wheel.MaxMoney/100 {
			return errors.New("invalid raw stock observation")
		}
		if prior, ok := r.stocks[s.Symbol]; ok && s.At.Before(prior.At) {
			return errors.New("stock quote time reversal")
		}
		r.stocks[s.Symbol] = s
	}
	// Missing execution observations are not evidence that an order failed to
	// fill. Require continuous <=1s NBBO sampling until fill or GFD expiry.
	if r.pending != nil && f.At.Sub(r.pending.LastQuote) > time.Second {
		return errors.New("missing <=1-second execution NBBO coverage for pending order")
	}
	if r.pending != nil && !f.At.Before(sessionClose(r.pending.Order.CreatedAt)) {
		observed := false
		for _, q := range f.Quotes {
			if q.ID == r.pending.Candidate.ID {
				if _, err := r.executionInterval(f, q, r.pending); err != nil {
					return err
				}
				observed = true
				break
			}
		}
		if !observed {
			return errors.New("missing final execution NBBO coverage at GFD expiry")
		}
		r.expireOrder()
	}
	if err := r.fill(f); err != nil {
		return err
	}
	for _, lifecycle := range f.Lifecycles {
		if err := r.lifecycle(lifecycle); err != nil {
			return err
		}
	}
	if f.Dividend != 0 {
		if f.Dividend < 0 || f.Dividend > wheel.MaxMoney || f.DividendEvidence == "" || f.DividendAvailableAt.IsZero() || f.DividendAvailableAt.Before(r.now) {
			return errors.New("invalid dividend evidence")
		}
		if _, ok := r.shares[f.DividendSymbol]; !ok {
			return errors.New("dividend lacks owned stock")
		}
		if err := r.changeCash(f.Dividend); err != nil {
			return err
		}
		r.metrics.Dividends += f.Dividend
		r.credits = append(r.credits, credit{Amount: f.Dividend, SettledAt: f.DividendAvailableAt})
	}
	if err := wheel.Reconcile(&r.state, r.snapshot(), r.now); err != nil {
		return err
	}
	if err := r.state.Validate(); err != nil {
		return err
	}
	if f.Kind == "decision" {
		if err := r.decide(f); err != nil {
			return err
		}
		r.decisionIndex++
	}
	if f.Kind == "valuation" {
		if r.valuationIndex >= len(r.valuations) || !f.At.Equal(r.valuations[r.valuationIndex]) {
			return errors.New("valuation is not at the session close")
		}
		point, err := r.mark()
		if err != nil {
			return err
		}
		r.report.Equity = append(r.report.Equity, point)
		r.valuationIndex++
	}
	return nil
}

func (r *replay) changeCash(delta wheel.Cents) error {
	if delta > wheel.MaxMoney || delta < -wheel.MaxMoney || r.cash+delta < 0 || r.cash+delta > wheel.MaxMoney {
		return errors.New("cash underflow/overflow")
	}
	if r.cash+delta < r.report.Spec.PeerReserve {
		return errors.New("modeled cash flow would consume peer-reserved cash; liquidation model required")
	}
	r.cash += delta
	return nil
}

func (r *replay) snapshot() wheel.Snapshot {
	s := wheel.Snapshot{At: r.now, Account: r.state.Account, ExternalSymbols: slices.Clone(r.report.Spec.PeerSymbols), ExternalCapital: r.report.Spec.PeerReserve}
	available := r.cash - r.report.Spec.PeerReserve
	for _, c := range r.credits {
		if r.now.Before(c.SettledAt) {
			available -= c.Amount
		}
	}
	for _, symbol := range slices.Sorted(maps.Keys(r.shares)) {
		inv := r.shares[symbol]
		h := wheel.Shares{Symbol: symbol, Quantity: 100, Cost: inv.Cost}
		if !r.now.Before(inv.SettledAt) {
			h.Available = 100
		}
		s.Shares = append(s.Shares, h)
	}
	for _, id := range slices.Sorted(maps.Keys(r.shorts)) {
		op := r.shorts[id]
		s.Options = append(s.Options, op)
		if op.Kind == "put" {
			available -= op.Strike * 100
		}
	}
	if r.pending != nil && r.pending.Candidate.Kind == "put" {
		available -= r.pending.Candidate.Collateral + r.report.Spec.Config.Fee
	}
	s.AvailableCash = max(0, available)
	s.Orders = slices.Clone(r.orders)
	if r.pending != nil {
		s.Orders = append(s.Orders, r.pending.Order)
	}
	return s
}

func (r *replay) decide(f Frame) error {
	if r.seenRoster == nil {
		r.seenRoster = map[string]exclusionIdentity{}
	}
	if r.selectionStubs == nil {
		r.selectionStubs = map[string]wheel.Quote{}
	}
	clear(r.selectionStubs)
	if f.Kind != "decision" {
		if len(f.NoRecentBook) > 0 || len(f.SelectionOmissions) > 0 || f.SelectionProof != nil {
			return errors.New("selection observations only allowed on scheduled decision frames")
		}
		return errors.New("no-recent-book observations only allowed on scheduled decision frames")
	}
	// Validate chain roster shape first and index every listed ID.
	chainByID := map[string]exclusionIdentity{}
	scopes := map[string]bool{}
	groups := map[string]bool{}
	for _, chain := range f.Chains {
		key := chain.Symbol + "/" + chain.Kind + "/" + chain.Expiry
		if groups[key] || !slices.Contains(r.report.Spec.Config.Symbols, chain.Symbol) || (chain.Kind != "put" && chain.Kind != "call") || chain.At.IsZero() || chain.At.After(r.now) || r.now.Sub(chain.At) > 30*time.Second || len(chain.Contracts) == 0 {
			return errors.New("invalid or stale point-in-time listed-chain roster")
		}
		groups[key] = true
		scopes[chain.Symbol+"/"+chain.Kind] = true
		exp, err := time.ParseInLocation("2006-01-02", chain.Expiry, wheel.Eastern)
		if err != nil || chain.At.After(sessionClose(exp)) {
			return errors.New("invalid or expired point-in-time roster expiry")
		}
		for _, id := range chain.Contracts {
			if _, dup := chainByID[id]; dup {
				return errors.New("listed contract omitted from quotes or mismatched expiry/metadata")
			}
			chainByID[id] = exclusionIdentity{Symbol: chain.Symbol, Kind: chain.Kind, Expiry: chain.Expiry, Weekly: chain.Weekly}
		}
	}
	for _, symbol := range r.report.Spec.Config.Symbols {
		for _, kind := range []string{"put", "call"} {
			if !scopes[symbol+"/"+kind] {
				return errors.New("missing put/call listed-chain scope")
			}
		}
	}
	// Persistent roster identity: a source-state reset must never reuse an ID
	// with different symbol/kind/expiry/weekly.
	for id, ident := range chainByID {
		if prior, ok := r.seenRoster[id]; ok && prior != ident {
			return errors.New("contract identity reused or changed across snapshots")
		}
	}
	// Validate bounded research-only exclusions and build transient adapters.
	stubs, err := r.validateNoRecentBook(f, chainByID)
	if err != nil {
		return err
	}
	// Post-transition portfolio for causal omission proofs. Snapshot carries no
	// quotes; held/pending/final valuation and forced-close sources are never
	// suppressed by omissions.
	snap := r.snapshot()
	bound := selectionBoundConfig(r)
	quoteIDs := map[string]bool{}
	for _, q := range f.Quotes {
		quoteIDs[q.ID] = true
	}
	exclusionIDs := map[string]bool{}
	for _, e := range f.NoRecentBook {
		exclusionIDs[e.OptionID] = true
	}
	omitted, hasGlobal, hasFull, err := r.validateSelectionOmissions(f, chainByID, quoteIDs, exclusionIDs, snap, bound)
	if err != nil {
		return err
	}
	// Every listed ID must be exactly one real quote, validated NoRecentBook,
	// or validated SelectionOmission. Missing unproved IDs still block.
	for id, ident := range chainByID {
		_, inQuotes := r.quotes[id]
		_, inStubs := stubs[id]
		_, inOmitted := omitted[id]
		count := 0
		if inQuotes {
			count++
		}
		if inStubs {
			count++
		}
		if inOmitted {
			count++
		}
		if count != 1 {
			return errors.New("listed contract omitted from quotes or mismatched expiry/metadata")
		}
		if inQuotes {
			q := r.quotes[id]
			if q.Symbol != ident.Symbol || q.Kind != ident.Kind || q.Expiry != ident.Expiry || q.Weekly != ident.Weekly {
				return errors.New("listed contract omitted from quotes or mismatched expiry/metadata")
			}
			if prior, ok := r.seenRoster[id]; ok && prior != ident {
				return errors.New("contract identity reused or changed across snapshots")
			}
		}
	}
	if len(chainByID) != len(f.Quotes)+len(f.NoRecentBook)+len(f.SelectionOmissions) {
		return errors.New("quoted contracts do not match independent listing roster")
	}
	// Record roster identities only after all roster/exclusion/omission checks pass.
	for id, ident := range chainByID {
		r.seenRoster[id] = ident
	}
	// Causal fence path: skip Select ranking entirely and prove via actual
	// Plan(nil). Invalid Plan errors block; only the precise global fence maps
	// to BlockedCycles. Omitted IDs never enter Select, quotes, catalog, fills,
	// or marks; NoRecent stubs for active scopes remain in simBroker.
	if hasGlobal || hasFull {
		if hasGlobal && hasFull {
			return errors.New("mixed global and full-slot omission reasons require mutually exclusive fences")
		}
		if hasGlobal {
			if len(r.state.Intents) == 0 {
				return errors.New("global omission without post-transition intent fence")
			}
			if _, err := wheel.Plan(bound, r.state, snap, nil, r.now); err == nil {
				return errors.New("false global omission proof; Plan(nil) succeeded")
			} else if err.Error() != selectionFenceError {
				return err
			}
			r.metrics.BlockedCycles++
			return nil
		}
		// hasFull
		if len(r.state.Intents) != 0 {
			return errors.New("full-slot omission with pending intent fence")
		}
		if _, err := wheel.Plan(bound, r.state, snap, nil, r.now); err != nil {
			return err
		}
		active, busy := selectionActiveBusy(bound, r.state, snap)
		if len(active) < bound.MaxSymbols {
			return errors.New("false full-slot omission proof; active slots remain")
		}
		if selectionHasEligibleCall(r.state, snap, busy) {
			return errors.New("full-slot omission with ready covered-call symbol")
		}
		return nil
	}
	var candidates []wheel.Candidate
	for _, symbol := range r.report.Spec.Config.Symbols {
		kind := "put"
		if r.state.Positions[symbol].OwnedShares == 100 {
			kind = "call"
		}
		// Combined real plus identity-only adapters so production Select still
		// discovers the nearest eligible expiry before market gates. Adapters
		// are always ineligible and never become candidates.
		var combined []wheel.Quote
		for _, q := range f.Quotes {
			if q.Symbol == symbol {
				combined = append(combined, q.Quote)
			}
		}
		for _, stub := range stubs {
			if stub.Symbol == symbol {
				combined = append(combined, stub)
			}
		}
		if len(combined) == 0 {
			return fmt.Errorf("%s has no verified listed-contract snapshot", symbol)
		}
		nearest := nearestSelectionExpiry(r.report.Spec.Config, symbol, kind, combined, r.now)
		for _, q := range f.Quotes {
			if selectionUsesDelta(r.report.Spec.Config, q.Quote, symbol, kind, nearest, r.state.Positions[symbol].Cost, r.now) && (q.GreeksAt.IsZero() || q.GreeksAt.After(r.now) || r.now.Sub(q.GreeksAt) > 30*time.Second) {
				return errors.New("missing or stale selection Greeks for ranking-relevant contract")
			}
		}
		if a, ok := wheel.Select(r.report.Spec.Config, symbol, kind, combined, r.state.Positions[symbol].Cost, r.now); ok {
			candidates = append(candidates, a)
		}
	}
	c := r.report.Spec.Config
	c.Account = r.state.Account
	plan, err := wheel.Plan(c, r.state, r.snapshot(), candidates, r.now)
	if err != nil {
		if len(r.state.Intents) > 0 {
			r.metrics.BlockedCycles++
			return nil
		}
		return err
	}
	if len(plan) == 0 {
		return nil
	}
	if err := wheel.Execute(context.Background(), c, &r.state, memoryStore{}, simBroker{r: r}, plan[0], func() time.Time { return r.now }); err != nil {
		return err
	}
	// Execute uses crypto-random submission references in production. Normalize
	// the simulated receipt/ref only after the exact pre-submit path succeeds,
	// so repeated offline runs have identical journals and audit traces.
	in := r.state.Intents[plan[0].Symbol]
	in.Ref = r.pending.Order.Ref
	r.state.Intents[plan[0].Symbol] = in
	return nil
}

// validateNoRecentBook checks bounded research-only freshness-ineligibility
// observations for NEW put/call selection. It enforces exact configured closed
// window [decisionT-MaxQuoteAge,T], lowercase 64-hex source hash,
// Complete=true, unique IDs, no overlap with real quotes, roster membership,
// and NEW-only scope (never held shorts or pending orders). It records
// roster-derived metadata explicitly and builds transient identity-only
// wheel.Quote adapters documented below. Adapters are never stored in
// r.quotes/catalog as market observations, never emitted to JSON, valued,
// allocated or executed.
func (r *replay) validateNoRecentBook(f Frame, chainByID map[string]exclusionIdentity) (map[string]wheel.Quote, error) {
	stubs := map[string]wheel.Quote{}
	if len(f.NoRecentBook) == 0 {
		return stubs, nil
	}
	seen := map[string]bool{}
	quoteIDs := map[string]bool{}
	for _, q := range f.Quotes {
		quoteIDs[q.ID] = true
	}
	maxAge := time.Duration(r.report.Spec.Config.MaxQuoteAgeSeconds) * time.Second
	expectedThrough := f.At
	expectedFrom := f.At.Add(-maxAge)
	for _, e := range f.NoRecentBook {
		if e.OptionID == "" {
			return nil, errors.New("invalid no-recent-book observation")
		}
		if seen[e.OptionID] {
			return nil, errors.New("duplicate no-recent-book observation")
		}
		seen[e.OptionID] = true
		if quoteIDs[e.OptionID] {
			return nil, errors.New("no-recent-book overlaps real quote")
		}
		if e.From.IsZero() || e.Through.IsZero() || !e.Through.Equal(expectedThrough) || !e.From.Equal(expectedFrom) {
			return nil, errors.New("invalid no-recent-book window; require exact closed [decisionT-MaxQuoteAge,T]")
		}
		if !e.Complete {
			return nil, errors.New("incomplete no-recent-book coverage")
		}
		if len(e.SourceSHA256) != 64 || strings.Trim(e.SourceSHA256, "0123456789abcdef") != "" {
			return nil, errors.New("invalid no-recent-book source hash")
		}
		ident, ok := chainByID[e.OptionID]
		if !ok {
			return nil, errors.New("no-recent-book for absent roster ID")
		}
		if _, held := r.shorts[e.OptionID]; held {
			return nil, errors.New("no-recent-book cannot cover held short ID")
		}
		for _, pos := range r.state.Positions {
			if pos.PutID == e.OptionID || pos.CallID == e.OptionID {
				return nil, errors.New("no-recent-book cannot cover held short ID")
			}
		}
		if r.pending != nil && (r.pending.Candidate.ID == e.OptionID || r.pending.Order.OptionID == e.OptionID) {
			return nil, errors.New("no-recent-book cannot cover pending order ID")
		}
		if old, ok := r.catalog[e.OptionID]; ok && (old.Symbol != ident.Symbol || old.Kind != ident.Kind || old.Expiry != ident.Expiry) {
			return nil, errors.New("contract identity reused or changed across snapshots")
		}
		if prior, ok := r.seenRoster[e.OptionID]; ok && prior != ident {
			return nil, errors.New("contract identity reused or changed across snapshots")
		}
		// Identity-only selection adapter: Symbol/Kind/Expiry/Weekly
		// participate in production expiry discovery. All market fields stay
		// zero (At/UnderlyingAt zero, Standard/Tradable false, no fabricated
		// price or knownAt) so the adapter is always ineligible, never
		// stored in r.quotes/catalog, never valued, allocated or executed.
		stubs[e.OptionID] = wheel.Quote{
			Option: wheel.Option{ID: e.OptionID, Symbol: ident.Symbol, Kind: ident.Kind, Expiry: ident.Expiry},
			Weekly: ident.Weekly,
		}
	}
	for id, stub := range stubs {
		r.selectionStubs[id] = stub
	}
	return stubs, nil
}

// These validation predicates mirror Select's expiry and non-delta filters.
// They never select, rank or change market fields. Every contract still reaches
// production Select, including an illiquid nearest expiry. Review these with
// any strategy change; the report binds this code and wheel/strategy.go hashes.
func nearestSelectionExpiry(c wheel.Config, symbol, kind string, quotes []wheel.Quote, now time.Time) string {
	today, _ := time.Parse("2006-01-02", now.In(wheel.Eastern).Format("2006-01-02"))
	nearest := ""
	for _, q := range quotes {
		if q.Symbol != symbol || q.Kind != kind {
			continue
		}
		exp, err := time.Parse("2006-01-02", q.Expiry)
		if err != nil {
			continue
		}
		days := int(exp.Sub(today).Hours() / 24)
		if days < 1 || days > c.MaxDays || kind == "put" && exp.Weekday() != time.Friday && !(exp.Weekday() == time.Thursday && q.Weekly) {
			continue
		}
		if nearest == "" || q.Expiry < nearest {
			nearest = q.Expiry
		}
	}
	return nearest
}

func selectionUsesDelta(c wheel.Config, q wheel.Quote, symbol, kind, nearest string, cost wheel.Cents, now time.Time) bool {
	if nearest == "" || q.Expiry != nearest || q.Symbol != symbol || q.Kind != kind || !q.Standard || !q.Tradable || q.Strike <= 0 || q.Strike > wheel.MaxMoney/100 || q.Spot <= 0 || q.Bid < c.MinBid || q.Ask < q.Bid || q.Ask > wheel.MaxMoney/100 || q.OI < c.MinOI || q.BidSize < 1 || q.Tick < 1 {
		return false
	}
	for _, at := range []time.Time{q.At, q.UnderlyingAt} {
		age := now.Sub(at)
		if at.IsZero() || age < -2*time.Second || age > time.Duration(c.MaxQuoteAgeSeconds)*time.Second {
			return false
		}
	}
	if int64(q.Ask-q.Bid)*10000 > int64(q.Ask)*int64(c.MaxSpreadBPS) || kind == "put" && q.Strike >= q.Spot || kind == "call" && !c.AllowBelowCost && q.Strike < cost {
		return false
	}
	return q.Bid/q.Tick*q.Tick*100-c.Fee > 0
}

type memoryStore struct{}

func (memoryStore) Save(s wheel.State) error { return s.Validate() }

type simBroker struct{ r *replay }

func (b simBroker) Snapshot(context.Context) (wheel.Snapshot, error) { return b.r.snapshot(), nil }
func (b simBroker) Quotes(_ context.Context, symbol, kind string) ([]wheel.Quote, error) {
	var out []wheel.Quote
	for _, q := range b.r.quotes {
		if q.Symbol == symbol && q.Kind == kind {
			out = append(out, q.Quote)
		}
	}
	// Identity-only selection adapters preserve pre-submit re-selection
	// semantics: excluded nearest-expiry contracts still participate in
	// production expiry discovery but remain always ineligible. Unset market
	// fields are unknown (At zero, Standard/Tradable false, no fabricated
	// price/knownAt), never stored in r.quotes/catalog, never valued,
	// allocated or executed.
	for _, stub := range b.r.selectionStubs {
		if stub.Symbol == symbol && stub.Kind == kind {
			out = append(out, stub)
		}
	}
	return out, nil
}
func (b simBroker) Review(context.Context, wheel.Candidate) error { return nil } // Explicit model assumption, no MCP request.
func (b simBroker) Submit(_ context.Context, a wheel.Candidate, _ string) (string, error) {
	r := b.r
	if r.pending != nil {
		return "", errors.New("duplicate in-flight submission")
	}
	r.metrics.Submissions++
	ref := fmt.Sprintf("simulation-%08d", r.metrics.Submissions)
	o := wheel.Order{ID: ref, Ref: ref, Symbol: a.Symbol, OptionID: a.ID, Kind: a.Kind, Strike: a.Strike, Quantity: 1, State: "queued", Side: "sell", Effect: "open", CreatedAt: r.now}
	r.pending = &pending{Candidate: a, Order: o, LastQuote: r.now}
	r.report.Trades = append(r.report.Trades, Trade{At: r.now, Symbol: a.Symbol, OptionID: a.ID, Action: "submit", Price: a.Bid})
	return ref, nil
}

func (r *replay) fill(f Frame) error {
	p := r.pending
	if p == nil {
		return nil
	}
	for _, q := range f.Quotes {
		if q.ID != p.Candidate.ID {
			continue
		}
		eligible, err := r.executionInterval(f, q, p)
		if err != nil {
			return err
		}
		if !eligible || f.At.Before(p.Order.CreatedAt.Add(time.Second)) || q.Bid < p.Candidate.Bid || q.BidSize < 1 || !q.Standard || !q.Tradable {
			p.DwellStart = time.Time{}
			p.LastQuote = f.At
			continue
		}
		if p.DwellStart.IsZero() || f.At.Sub(p.LastQuote) > time.Second {
			p.DwellStart = f.At
		}
		p.LastQuote = f.At
		if f.At.Sub(p.DwellStart) < 5*time.Second {
			return nil
		}
		// No expected premium financed admission; it becomes economic cash
		// only here, and is unavailable until the next reviewed session.
		fee := r.report.Spec.Config.Fee
		cash := p.Candidate.Bid*100 - fee
		if err := r.changeCash(cash); err != nil {
			return err
		}
		r.credits = append(r.credits, credit{Amount: cash, SettledAt: nextSettlement(r.now)})
		r.metrics.Fills++
		r.metrics.Premiums += p.Candidate.Bid * 100
		r.metrics.Fees += fee
		op := p.Candidate.Option
		op.Quantity = 1
		r.shorts[op.ID] = op
		p.Order.State, p.Order.Filled, p.Order.LastTransactionAt = "filled", 1, r.now
		r.orders = append(r.orders, p.Order)
		r.report.Trades = append(r.report.Trades, Trade{At: r.now, Symbol: op.Symbol, OptionID: op.ID, Action: "sell_" + op.Kind, Price: p.Candidate.Bid, CashFlow: cash, Fee: fee, Assumption: "sized bid dwell; simulated fill, queue position unknown"})
		r.pending = nil
		return nil
	}
	return nil
}

// executionInterval separates the age of the last actual quote from the time
// its current book was observed. Quiet periods are usable only with declared
// complete intervening coverage and preserved quote timestamps. A hash/flag
// alone cannot qualify a source; the historical report gate enforces that audit.
func (r *replay) executionInterval(f Frame, q Quote, p *pending) (bool, error) {
	if q.At.IsZero() || q.At.After(f.At) {
		return false, errors.New("missing or future execution quote")
	}
	fresh := f.At.Sub(q.At) <= time.Duration(r.report.Spec.Config.MaxQuoteAgeSeconds)*time.Second
	c := f.ExecutionCoverage
	if c == nil {
		// The invented fixture still supports its explicitly sampled point path.
		// Historical input and carried books cannot use that compatibility path.
		if r.report.Spec.Mode != "fixture" || !q.At.Equal(f.At) {
			return false, errors.New("missing source-bound execution interval coverage")
		}
		return fresh, nil
	}
	if !c.Complete || c.OptionID != q.ID || c.From.IsZero() || !c.From.Equal(p.LastQuote) || !c.Through.Equal(f.At) || !c.Through.After(c.From) || c.Through.Sub(c.From) > time.Second || !q.ObservedAt.Equal(f.At) {
		return false, errors.New("incomplete or mismatched execution interval coverage")
	}
	if len(c.SourceSHA256) != 64 || strings.Trim(c.SourceSHA256, "0123456789abcdef") != "" {
		return false, errors.New("invalid execution source hash")
	}
	if c.MinBid < 0 || c.MinBid > q.Bid || c.MinBidSize < 0 || c.MinBidSize > q.BidSize {
		return false, errors.New("invalid execution interval minima")
	}
	return fresh && c.ValidBooks && c.QuotesFresh && c.MinBid >= p.Candidate.Bid && c.MinBidSize >= 1, nil
}

func nextSettlement(at time.Time) time.Time {
	t := at.In(wheel.Eastern)
	d := time.Date(t.Year(), t.Month(), t.Day(), 9, 35, 0, 0, wheel.Eastern)
	for range 10 {
		d = d.AddDate(0, 0, 1)
		if wheel.TradingWindow(d) {
			return d.Add(-5 * time.Minute)
		}
	}
	return at.Add(1000 * 24 * time.Hour) // Outside known sessions funds remain locked.
}

func (r *replay) expireOrder() {
	p := r.pending
	p.Order.State = "cancelled"
	p.Order.LastTransactionAt = r.now
	r.orders = append(r.orders, p.Order)
	r.metrics.ExpiredOrders++
	r.report.Trades = append(r.report.Trades, Trade{At: r.now, Symbol: p.Order.Symbol, OptionID: p.Order.OptionID, Action: "day_order_expired", Price: p.Candidate.Bid})
	r.pending = nil
}

func (r *replay) lifecycle(l Lifecycle) error {
	op, ok := r.shorts[l.OptionID]
	if !ok || l.Assumption == "" || l.SettledAt.IsZero() || l.SettledAt.Before(r.now) {
		return errors.New("unowned lifecycle or missing model/settlement assumption")
	}
	if (l.Outcome == "assigned" || l.Outcome == "called_away") && l.SettledAt.Before(nextSettlement(r.now)) {
		return errors.New("physical lifecycle cannot reuse funds/shares before T+1")
	}
	exp, _ := time.ParseInLocation("2006-01-02", op.Expiry, wheel.Eastern)
	if l.Outcome == "expired" && r.now.Before(sessionClose(exp)) {
		return errors.New("expiry before contract session close")
	}
	flow := wheel.Cents(0)
	fee := wheel.Cents(0)
	price := op.Strike
	switch l.Outcome {
	case "expired":
	case "assigned":
		if op.Kind != "put" {
			return errors.New("assignment is not a put")
		}
		if _, owned := r.shares[op.Symbol]; owned {
			return errors.New("assignment duplicates inventory")
		}
		flow = -op.Strike * 100
		r.shares[op.Symbol] = inventory{Cost: op.Strike, SettledAt: l.SettledAt}
		r.metrics.Assignments++
	case "called_away":
		inv, owned := r.shares[op.Symbol]
		if op.Kind != "call" || !owned {
			return errors.New("uncovered call-away")
		}
		flow = op.Strike * 100
		r.metrics.StockRealized += (op.Strike - inv.Cost) * 100
		delete(r.shares, op.Symbol)
		r.metrics.CallAways++
		r.credits = append(r.credits, credit{Amount: flow, SettledAt: l.SettledAt})
	case "forced_closed":
		q, exists := r.quotes[op.ID]
		if !exists || q.Ask <= 0 || r.now.Sub(q.At) > 30*time.Second || l.ClosePrice != q.Ask {
			return errors.New("forced broker close needs a fresh ask, never strategy repricing")
		}
		fee = r.report.Spec.Config.Fee
		price = l.ClosePrice
		flow = -l.ClosePrice*100 - fee
		r.metrics.Fees += fee
	default:
		return errors.New("unknown lifecycle outcome")
	}
	if err := r.changeCash(flow); err != nil {
		return err
	}
	delete(r.shorts, op.ID)
	r.report.Trades = append(r.report.Trades, Trade{At: r.now, Symbol: op.Symbol, OptionID: op.ID, Action: l.Outcome, Price: price, CashFlow: flow, Fee: fee, Assumption: l.Assumption})
	return nil
}

func (r *replay) mark() (Point, error) {
	s := r.snapshot()
	p := Point{At: r.now, Cash: r.cash, Available: s.AvailableCash, Committed: r.report.Spec.PeerReserve}
	for symbol, inv := range r.shares {
		q, ok := r.stocks[symbol]
		if !ok || r.now.Sub(q.At) > 30*time.Second {
			return p, fmt.Errorf("missing fresh stock bid for %s valuation", symbol)
		}
		p.StockValue += q.Bid * 100
		p.Committed += inv.Cost * 100
	}
	for id, op := range r.shorts {
		q, ok := r.quotes[id]
		if !ok || q.Ask <= 0 || r.now.Sub(q.At) > 30*time.Second {
			return p, fmt.Errorf("missing fresh short-option ask for %s valuation", id)
		}
		p.OptionLiability += q.Ask * 100
		if op.Kind == "put" {
			p.Committed += op.Strike * 100
		}
	}
	if r.pending != nil && r.pending.Candidate.Kind == "put" {
		p.Committed += r.pending.Candidate.Collateral
	}
	for symbol, pos := range r.state.Positions {
		if pos.PutID != "" {
			if _, open := r.shorts[pos.PutID]; !open {
				if _, stock := r.shares[symbol]; !stock {
					p.Committed += pos.PutStrike * 100
				}
			}
		}
	}
	active := map[string]bool{}
	for symbol := range r.state.Positions {
		active[symbol] = true
	}
	for symbol := range r.state.Intents {
		active[symbol] = true
	}
	p.Active = len(active)
	p.Equity = p.Cash + p.StockValue - p.OptionLiability
	if p.Equity < -wheel.MaxMoney || p.Equity > wheel.MaxMoney || p.Committed > wheel.MaxMoney {
		return p, errors.New("valuation out of range")
	}
	return p, nil
}
