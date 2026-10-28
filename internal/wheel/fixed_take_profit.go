package wheel

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"
)

// A daily reservation precedes reads. Each owned contract gets at most one
// observation and one submission attempt in that session, including restarts.
type FixedCloseSignal struct {
	Symbol       string     `json:"symbol"`
	Option       Option     `json:"option"`
	Phase        string     `json:"phase"`
	Reason       string     `json:"reason,omitempty"`
	Candidate    *Candidate `json:"candidate,omitzero"`
	ObservedAt   time.Time  `json:"observed_at,omitzero"`
	TransitionAt time.Time  `json:"transition_at"`
}

type FixedCloseBatch struct {
	PolicySHA   string             `json:"policy_sha256"`
	PreviousSHA string             `json:"previous_journal_sha256,omitempty"`
	Day         string             `json:"day"`
	SignalAt    time.Time          `json:"signal_at"`
	ExecutionAt time.Time          `json:"execution_at"`
	LatestAt    time.Time          `json:"latest_at"`
	StartedAt   time.Time          `json:"started_at"`
	SealedAt    time.Time          `json:"sealed_at,omitzero"`
	Signals     []FixedCloseSignal `json:"signals"`
}

// Whole-journal archives retain daily source evidence and the ownership ledger.
type FixedPolicyArchiver interface {
	ArchiveFixedPolicyState(State) (string, error)
}

type FixedCloseBroker interface {
	Snapshot(context.Context) (Snapshot, error)
	CloseQuote(context.Context, State, string) (Candidate, error)
	Review(context.Context, Candidate) error
	SubmitFixed(context.Context, Candidate, string, time.Time, func() error) (string, error)
}

func FixedCloseTimes(date time.Time) (signal, execution, latest time.Time, ok bool) {
	close, ok := SessionClose(date)
	if !ok {
		return time.Time{}, time.Time{}, time.Time{}, false
	}
	return close.Add(-31 * time.Minute), close.Add(-30 * time.Minute), close.Add(-26 * time.Minute), true
}

// FixedTP75Limit uses gross broker fill credit and REVIEWED fee allowances, not
// invented actual fees. Net credit N=credit-opening allowance; debit+closing
// allowance must be <=N/4. Rounding down in whole per-share ticks preserves
// at least 75% under those bounds. Missing/too-small credit has no valid limit.
func FixedTP75Limit(credit, fee, tick Cents) (Cents, error) {
	if credit <= fee || credit > MaxMoney || fee < 0 || fee > 10000 || tick < 1 || tick > MaxMoney/100 {
		return 0, errors.New("TP75 credit, fee allowance or tick is invalid")
	}
	debit := (credit-fee)/4 - fee
	limit := debit / 100 / tick * tick
	if debit <= 0 || limit <= 0 {
		return 0, errors.New("TP75 leaves no positive tick after fee allowances")
	}
	return limit, nil
}

func fixedCloseOwnership(s State, a Candidate) bool {
	p, own := s.Positions[a.Symbol]
	opened := p.PutOpenedAt
	if a.Kind == "call" {
		opened = p.CallOpenedAt
	}
	r, receipt := s.CompletedOrders[a.EntryOrderID]
	return own && a.Closing && a.Quantity == 1 && a.ID != "" && a.ChainID != "" &&
		p.MissingSince.IsZero() && receipt && !r.Closing && r.Filled == 1 &&
		r.Symbol == a.Symbol && r.OptionID == a.ID && r.At.Equal(opened) && !opened.IsZero() &&
		(a.Kind == "put" && p.PutID == a.ID && p.PutExpiry == a.Expiry && p.PutStrike == a.Strike && p.OwnedShares == 0 && p.CallID == "" ||
			a.Kind == "call" && p.CallID == a.ID && p.CallExpiry == a.Expiry && p.PutID == "" && p.OwnedShares == 100)
}

func fixedCloseMarket(spec FixedPolicySpec, a Candidate, now time.Time) error {
	limit, err := FixedTP75Limit(a.EntryCredit, spec.OptionFeeBudget, a.Tick)
	if err != nil || !a.Closing || a.Quantity != 1 || a.Strike <= 0 || a.Strike > MaxMoney/100 ||
		!a.Standard || !a.Tradable || a.CloseLimit <= 0 || a.CloseLimit > limit || a.CloseLimit%a.Tick != 0 ||
		a.RestingTakeProfit != (spec.HasRestingTP75()) ||
		!a.RestingTakeProfit && a.Ask > a.CloseLimit ||
		a.Ask <= 0 || a.Bid < 0 || a.Bid > a.Ask || a.AskSize < 1 {
		return errors.New("TP75 quote does not meet the fee-aware profit floor")
	}
	for _, at := range []time.Time{a.At, a.UnderlyingAt} {
		if at.IsZero() || !fresh(at, now, spec.MaxQuoteAge) {
			return errors.New("TP75 source clock is missing, future or stale")
		}
	}
	if a.Expiry < now.In(Eastern).Format(time.DateOnly) {
		return errors.New("TP75 option expired")
	}
	return nil
}

func (f FixedPolicyState) validateTakeProfit(s State) error {
	if err := f.validateRestingTakeProfit(s); err != nil {
		return err
	}
	if f.Spec.HasRestingTP75() {
		return nil
	}
	if f.Spec.ID != FixedConservativeTP75Policy {
		if f.TakeProfit != nil {
			return errors.New("hold policy cannot carry TP authority")
		}
		for _, in := range s.Intents {
			if in.Candidate.Closing {
				return errors.New("hold policy cannot submit closes")
			}
		}
		return nil
	}
	b := f.TakeProfit
	if b == nil {
		for _, in := range s.Intents {
			if in.Candidate.Closing {
				return errors.New("TP close has no daily reservation")
			}
		}
		return nil
	}
	day, err := time.ParseInLocation(time.DateOnly, b.Day, Eastern)
	signal, execution, latest, ok := FixedCloseTimes(day)
	if err != nil || !ok || b.PolicySHA != f.PolicySHA || !b.SignalAt.Equal(signal) || !b.ExecutionAt.Equal(execution) ||
		!b.LatestAt.Equal(latest) || b.StartedAt.Before(signal) || !b.StartedAt.Before(execution) || b.StartedAt.Before(f.BoundAt) ||
		b.PreviousSHA != "" && !fixedSHA(b.PreviousSHA) || len(b.Signals) > 3 ||
		!b.SealedAt.IsZero() && b.SealedAt.Before(b.StartedAt) {
		return errors.New("TP daily reservation clock or policy binding is invalid")
	}
	previous := ""
	for _, v := range b.Signals {
		if !fixedSymbol(v.Symbol) || v.Symbol <= previous || v.Option.Symbol != v.Symbol || v.Option.Quantity != 1 ||
			v.Option.ID == "" || v.Option.ChainID == "" || v.Option.Kind != "put" && v.Option.Kind != "call" || v.TransitionAt.Before(b.StartedAt) {
			return errors.New("TP daily owned contract ordering is invalid")
		}
		previous = v.Symbol
		switch v.Phase {
		case "unobserved", "abstained":
			if v.Candidate != nil || !v.ObservedAt.IsZero() || v.Phase == "unobserved" && (!b.SealedAt.IsZero() || v.Reason != "") ||
				v.Phase == "abstained" && !slices.Contains([]string{"source_unavailable", "below_target", "capture_interrupted"}, v.Reason) {
				return errors.New("TP empty observation is invalid")
			}
		case "ready", "claimed", "expired":
			if v.Candidate == nil || v.Candidate.Option != v.Option || v.Reason != "" ||
				v.ObservedAt.Before(b.StartedAt) || !v.ObservedAt.Before(execution) || fixedCloseMarket(f.Spec, *v.Candidate, v.ObservedAt) != nil ||
				v.Phase == "ready" && !v.TransitionAt.Equal(v.ObservedAt) ||
				v.Phase != "ready" && b.SealedAt.IsZero() ||
				v.Phase == "claimed" && (v.TransitionAt.Before(execution) || v.TransitionAt.After(latest) || v.TransitionAt.Before(b.SealedAt)) ||
				v.Phase == "expired" && !v.TransitionAt.After(latest) {
				return errors.New("TP captured observation or claim is invalid")
			}
		default:
			return errors.New("unknown TP signal phase")
		}
	}
	for symbol, in := range s.Intents {
		if !in.Candidate.Closing {
			continue
		}
		i := slices.IndexFunc(b.Signals, func(v FixedCloseSignal) bool { return v.Symbol == symbol })
		if i < 0 || b.Signals[i].Phase != "claimed" || b.Signals[i].Candidate == nil ||
			in.At.Before(b.Signals[i].TransitionAt) || in.At.After(latest) ||
			in.Candidate.Option != b.Signals[i].Option || in.Candidate.EntryCredit != b.Signals[i].Candidate.EntryCredit ||
			in.Candidate.EntryOrderID != b.Signals[i].Candidate.EntryOrderID || !fixedCloseOwnership(s, in.Candidate) ||
			fixedCloseMarket(f.Spec, in.Candidate, in.At) != nil ||
			in.Candidate.CloseLimit > b.Signals[i].Candidate.CloseLimit {
			return errors.New("TP intent lacks its exact single-use daily claim")
		}
	}
	return nil
}

func (c *FixedController) BeginTakeProfit(now time.Time) error {
	if err := c.check(); err != nil {
		return err
	}
	signal, execution, latest, ok := FixedCloseTimes(now)
	if c.spec.ID != FixedConservativeTP75Policy || !ok || now.Before(signal) || !now.Before(execution) || len(c.state.Intents) != 0 {
		return errors.New("TP capture is outside its session or has unresolved submission")
	}
	f := c.copy()
	previous := ""
	if f.TakeProfit != nil {
		if !f.TakeProfit.SignalAt.Before(signal) {
			return errors.New("TP session already reserved; never recapture")
		}
		if err := c.RecoverTakeProfit(now); err != nil {
			return err
		}
		f = c.copy()
		archive, ok := c.store.(FixedPolicyArchiver)
		if !ok {
			return errors.New("TP daily evidence requires a durable policy archiver")
		}
		var err error
		previous, err = archive.ArchiveFixedPolicyState(*c.state)
		if err != nil || !fixedSHA(previous) {
			return errors.Join(errors.New("TP prior source archive failed"), err)
		}
	}
	b := &FixedCloseBatch{PolicySHA: f.PolicySHA, PreviousSHA: previous, Day: signal.Format(time.DateOnly),
		SignalAt: signal, ExecutionAt: execution, LatestAt: latest, StartedAt: now}
	for _, symbol := range slices.Sorted(maps.Keys(f.Contracts)) {
		if c.state.Positions[symbol].MissingSince.IsZero() {
			b.Signals = append(b.Signals, FixedCloseSignal{Symbol: symbol, Option: f.Contracts[symbol], Phase: "unobserved", TransitionAt: now})
		}
	}
	f.TakeProfit = b
	return c.save(f)
}

func (c *FixedController) RecordTakeProfit(symbol string, a Candidate, sourceErr error, now time.Time) error {
	if err := c.check(); err != nil {
		return err
	}
	f := c.copy()
	b := f.TakeProfit
	if b == nil || !b.SealedAt.IsZero() || now.Before(b.StartedAt) || !now.Before(b.ExecutionAt) {
		return errors.New("TP capture is closed")
	}
	i := slices.IndexFunc(b.Signals, func(v FixedCloseSignal) bool { return v.Symbol == symbol })
	if i < 0 || b.Signals[i].Phase != "unobserved" {
		return errors.New("TP observation already consumed")
	}
	v := &b.Signals[i]
	v.Phase, v.Reason, v.TransitionAt = "abstained", "source_unavailable", now
	if sourceErr == nil && a.Option == v.Option && fixedCloseOwnership(*c.state, a) {
		limit, limitErr := FixedTP75Limit(a.EntryCredit, c.spec.OptionFeeBudget, a.Tick)
		if limitErr != nil || a.Ask > limit {
			v.Reason = "below_target"
		}
		if fixedCloseMarket(c.spec, a, now) == nil {
			a.CloseLimit = limit
			v.Phase, v.Reason, v.ObservedAt, v.Candidate = "ready", "", now, new(a)
		}
	}
	return c.save(f)
}

// Recovery never re-reads an omitted quote. It consumes unobserved reservations
// even during the capture minute after a restart, and expires missed attempts.
func (c *FixedController) RecoverTakeProfit(now time.Time) error {
	if err := c.check(); err != nil {
		return err
	}
	f := c.copy()
	b := f.TakeProfit
	if b == nil {
		return nil
	}
	if now.Before(b.StartedAt) {
		return errors.New("TP recovery precedes its reservation")
	}
	changed := false
	if b.SealedAt.IsZero() {
		b.SealedAt, changed = now, true
		for i := range b.Signals {
			if b.Signals[i].Phase == "unobserved" {
				b.Signals[i].Phase, b.Signals[i].Reason, b.Signals[i].TransitionAt = "abstained", "capture_interrupted", now
			}
		}
	}
	if now.After(b.LatestAt) {
		for i := range b.Signals {
			if b.Signals[i].Phase == "ready" {
				b.Signals[i].Phase, b.Signals[i].TransitionAt, changed = "expired", now, true
			}
		}
	}
	if changed {
		return c.save(f)
	}
	return nil
}

func (c *FixedController) checkTakeProfit(config Config, signal FixedCloseSignal, a Candidate, s Snapshot, now time.Time) error {
	if err := c.nativeConfig(config); err != nil {
		return err
	}
	b := c.state.Fixed.TakeProfit
	if c.spec.ID != FixedConservativeTP75Policy || b == nil || b.SealedAt.IsZero() || now.Before(b.ExecutionAt) || now.After(b.LatestAt) ||
		!TradingWindow(now) || len(c.state.Intents) != 0 || s.Account != c.spec.Account || !fresh(s.At, now, c.spec.MaxQuoteAge) ||
		len(c.state.CompletedOrders) >= 1024 || !fixedCloseOwnership(*c.state, a) || a.Option != signal.Option || signal.Candidate == nil ||
		a.EntryCredit != signal.Candidate.EntryCredit || a.EntryOrderID != signal.Candidate.EntryOrderID || a.CloseLimit > signal.Candidate.CloseLimit {
		return errors.New("TP execution lacks its owned daily claim or current account")
	}
	if err := checkOrderScope(config, *c.state, s); err != nil {
		return err
	}
	if err := fixedCloseMarket(c.spec, a, now); err != nil {
		return err
	}
	count := 0
	for _, op := range s.Options {
		if op.Symbol == a.Symbol {
			if op != a.Option {
				return errors.New("TP broker contract drift")
			}
			count++
		}
	}
	if count != 1 {
		return errors.New("TP broker must report exactly one owned short")
	}
	stock := false
	for _, h := range s.Shares {
		if h.Symbol != a.Symbol {
			continue
		}
		if a.Kind == "put" && h.Quantity != 0 {
			return errors.New("TP put overlaps assignment stock")
		}
		if a.Kind == "call" && h.Quantity == 100 && h.Cost == c.state.Positions[a.Symbol].Cost {
			stock = true
		}
	}
	if a.Kind == "call" && !stock {
		return errors.New("TP call lacks its own retained stock")
	}
	for _, o := range s.Orders {
		if !o.Terminal() {
			return errors.New("TP waits for working orders")
		}
	}
	debit := a.CloseLimit*100 + c.spec.OptionFeeBudget
	if s.AvailableCash < config.Reserve || debit > s.AvailableCash-config.Reserve {
		return errors.New("TP buyback exceeds settled available cash after reserve")
	}
	return nil
}

// PreviewTakeProfit proves existing own close compatibility outside market
// hours. It does not require today's profit trigger, create a claim or submit.
func (c *FixedController) PreviewTakeProfit(config Config, a Candidate, s Snapshot, now time.Time) error {
	if err := c.ReconcileNative(config, s, now); err != nil {
		return err
	}
	committed, err := c.restingCloseCommitment(s)
	if err != nil {
		return err
	}
	if !c.spec.HasTP75() || !fixedCloseOwnership(*c.state, a) || c.state.Fixed.Contracts[a.Symbol] != a.Option ||
		!a.Standard || !a.Tradable || a.Tick < 1 || a.CloseLimit <= 0 || a.CloseLimit > MaxMoney/100 || a.CloseLimit%a.Tick != 0 ||
		a.Bid < 0 || a.Bid > a.Ask || a.Ask <= 0 || !a.RestingTakeProfit && a.Ask > a.CloseLimit || a.AskSize < 1 || a.EntryCredit <= c.spec.OptionFeeBudget ||
		s.AvailableCash < config.Reserve || committed > s.AvailableCash-config.Reserve || a.CloseLimit*100+c.spec.OptionFeeBudget > s.AvailableCash-config.Reserve-committed {
		return errors.New("TP preview lacks exact owned fill, usable book or settled buyback cash")
	}
	return nil
}

// ExecuteTakeProfit claims before reads/review, persists ref before transport,
// and retains unknown outcomes. A failed opportunity cannot be retried today.
func (c *FixedController) ExecuteTakeProfit(ctx context.Context, config Config, broker FixedCloseBroker, symbol string, clock func() time.Time) error {
	if broker == nil || clock == nil {
		return errors.New("TP execution needs broker and clock")
	}
	if err := c.nativeConfig(config); err != nil {
		return err
	}
	f := c.copy()
	b := f.TakeProfit
	now := clock()
	if b == nil || b.SealedAt.IsZero() || now.Before(b.ExecutionAt) || now.After(b.LatestAt) || !TradingWindow(now) || len(c.state.Intents) != 0 {
		return errors.New("TP daily execution bucket unavailable")
	}
	i := slices.IndexFunc(b.Signals, func(v FixedCloseSignal) bool { return v.Symbol == symbol })
	if i < 0 || b.Signals[i].Phase != "ready" {
		return errors.New("TP opportunity was not prepared or is consumed")
	}
	for _, v := range b.Signals[:i] {
		if v.Phase == "ready" {
			return errors.New("TP cannot overtake an earlier owned symbol")
		}
	}
	b.Signals[i].Phase, b.Signals[i].TransitionAt = "claimed", now
	if err := c.save(f); err != nil {
		return err
	}
	signal := b.Signals[i]
	s, err := broker.Snapshot(ctx)
	if err != nil {
		return err
	}
	if err = c.ReconcileNative(config, s, clock()); err != nil {
		return err
	}
	a, err := broker.CloseQuote(ctx, *c.state, symbol)
	if err != nil {
		return err
	}
	// Never increase the signal's debit limit after prices move.
	a.CloseLimit = min(a.CloseLimit, signal.Candidate.CloseLimit)
	if err = c.checkTakeProfit(config, signal, a, s, clock()); err != nil {
		return err
	}
	if err = broker.Review(ctx, a); err != nil {
		return err
	}
	s, err = broker.Snapshot(ctx)
	if err != nil {
		return err
	}
	if err = c.ReconcileNative(config, s, clock()); err != nil {
		return err
	}
	if err = c.checkTakeProfit(config, signal, a, s, clock()); err != nil {
		return err
	}
	ref, err := newRef()
	if err != nil {
		return err
	}
	next := *c.state
	next.Intents = maps.Clone(next.Intents)
	next.Intents[symbol] = Intent{Ref: ref, Candidate: a, At: clock()}
	if err = c.saveState(next); err != nil {
		return err
	}
	clearUnsent := func(cause error) error {
		next := *c.state
		next.Intents = maps.Clone(next.Intents)
		delete(next.Intents, symbol)
		return errors.Join(cause, c.saveState(next))
	}
	guard := func() error {
		at := clock()
		if err := c.check(); err != nil {
			return err
		}
		if ctx.Err() != nil || at.Before(b.ExecutionAt) || at.After(b.LatestAt) || !TradingWindow(at) ||
			c.state.Intents[symbol].Ref != ref || !fresh(a.At, at, c.spec.MaxQuoteAge) || !fresh(a.UnderlyingAt, at, c.spec.MaxQuoteAge) || !fresh(s.At, at, c.spec.MaxQuoteAge) {
			return ErrNotSubmitted
		}
		return nil
	}
	if err = guard(); err != nil {
		return clearUnsent(errors.Join(ErrNotSubmitted, err))
	}
	deadline := b.LatestAt
	for _, at := range []time.Time{a.At, a.UnderlyingAt, s.At} {
		if expires := at.Add(time.Duration(c.spec.MaxQuoteAge) * time.Second); expires.Before(deadline) {
			deadline = expires
		}
	}
	transport, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	id, err := broker.SubmitFixed(transport, a, ref, b.LatestAt, guard)
	if err != nil {
		if errors.Is(err, ErrNotSubmitted) {
			return clearUnsent(err)
		}
		return errors.New("TP submission outcome unknown; durable intent retained, no retry")
	}
	if id == "" {
		return errors.New("TP receipt identity missing; durable intent retained, no retry")
	}
	next = *c.state
	next.Intents = maps.Clone(next.Intents)
	in := next.Intents[symbol]
	in.OrderID = id
	next.Intents[symbol] = in
	return c.saveState(next)
}

// EnableFixedTP75 is deliberately limited to the current own pre-cohort handover.
// It cannot change capital, fees, roster authority, or inventory classification.
func EnableFixedTP75(s State, config Config, now time.Time) (State, error) {
	if err := s.Validate(); err != nil {
		return State{}, err
	}
	if err := config.Validate(); err != nil {
		return State{}, err
	}
	if s.Fixed == nil || config.FixedPolicy == nil || s.Account != config.Account || s.Fixed.Spec.ID != FixedConservativePolicy ||
		config.FixedPolicy.Spec.ID != FixedConservativeTP75Policy || now.IsZero() || now.Before(s.Fixed.BoundAt) ||
		len(s.Intents) != 0 || s.Fixed.Batch != nil || s.Fixed.TakeProfit != nil || config.FixedPolicy.ImportLegacyStateSHA != fixedLegacyBinding(s.Fixed) {
		return State{}, errors.New("TP75 transition needs the exact paused pre-cohort hold policy")
	}
	for symbol, p := range s.Positions {
		if !s.Fixed.inherited(symbol, p) {
			return State{}, errors.New("TP75 cannot reclassify native or external inventory")
		}
	}
	before, after := s.Fixed.Spec, config.FixedPolicy.Spec
	before.ID = after.ID
	if before.Digest() != after.Digest() || before.FeeSourceSHA != after.FeeSourceSHA || !before.FeeKnownAt.Equal(after.FeeKnownAt) || after.FeeKnownAt.After(now) {
		return State{}, errors.New("TP75 transition preserves all capital, fee and clock settings")
	}
	f := *s.Fixed
	f.Spec, f.PolicySHA, f.BoundAt = after, after.Digest(), now
	s.Fixed = &f
	return s, s.Validate()
}
