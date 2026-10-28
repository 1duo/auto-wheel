package wheel

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"
)

// One durable attempt per opening receipt. Keep it after cancellation, rejection
// or an unsent attempt so neither a poll nor a restart can resubmit that order.
type FixedRestingClose struct {
	Ref       string    `json:"ref_id"`
	Candidate Candidate `json:"candidate"`
	At        time.Time `json:"at"`
}

// An explicit offline operator recovery retains the complete unsent claim and
// its immutable source archive. The normal account cycle cannot create one.
type FixedUnsentRearm struct {
	Attempt   FixedRestingClose `json:"attempt"`
	SourceSHA string            `json:"source_journal_sha256"`
	At        time.Time         `json:"at"`
}

// Submission uses the entire reviewed regular session. The broker owns the
// lifetime of the GTC order; this deadline only bounds its initial submission.
func FixedRestingCloseDeadline(now time.Time) (time.Time, bool) {
	close, ok := SessionClose(now)
	t := now.In(Eastern)
	open := time.Date(t.Year(), t.Month(), t.Day(), 9, 30, 0, 0, Eastern)
	return close, ok && !now.Before(open) && now.Before(close)
}

func (f FixedPolicyState) batchPolicyMatches(b FixedBatch) bool {
	if b.PolicySHA == f.PolicySHA {
		return true
	}
	if f.Spec.ID == FixedConservativeASAPPolicy {
		old := f.Spec
		old.Limits.Slots = 0
		for _, id := range []string{FixedConservativeTP75Policy, FixedConservativeRestingTP75Policy} {
			old.ID = id
			if f.Replenishment != nil && b.PolicySHA == old.Digest() && b.LatestAt.Before(f.Replenishment.Since) && b.Phase == "sealed" {
				for _, signal := range b.Signals {
					if signal.Phase == "ready" || signal.Phase == "unobserved" {
						return false
					}
				}
				return true
			}
		}
		return false
	}
	old := f.Spec
	old.ID = FixedConservativeTP75Policy
	if !f.Spec.HasRestingTP75() || f.RestingTP75Since.IsZero() ||
		b.PolicySHA != old.Digest() || !b.LatestAt.Before(f.RestingTP75Since) || b.Phase != "sealed" {
		return false
	}
	for _, signal := range b.Signals {
		if signal.Phase == "ready" || signal.Phase == "unobserved" {
			return false
		}
	}
	return true
}

func (f FixedPolicyState) validateRestingTakeProfit(s State) error {
	if !f.Spec.HasRestingTP75() {
		if !f.RestingTP75Since.IsZero() || len(f.RestingTakeProfit) != 0 || len(f.RestingUnsentRearms) != 0 {
			return errors.New("resting TP authority requires the explicit resting policy")
		}
		return nil
	}
	if f.TakeProfit != nil || f.RestingTP75Since.IsZero() || f.RestingTP75Since.Before(f.BoundAt) || len(f.RestingTakeProfit)+len(f.RestingUnsentRearms) > 1024 {
		return errors.New("resting TP policy clock or attempt ledger is invalid")
	}
	refs := map[string]bool{}
	validateAttempt := func(entryID string, attempt FixedRestingClose) error {
		a := attempt.Candidate
		r, found := s.CompletedOrders[entryID]
		if !found || r.Closing || r.Filled != 1 || r.Symbol != a.Symbol || r.OptionID != a.ID ||
			entryID != a.EntryOrderID || attempt.Ref == "" || refs[attempt.Ref] || !a.RestingTakeProfit ||
			attempt.At.Before(f.RestingTP75Since) || attempt.At.Before(r.At) || !a.TradingWindow(attempt.At) ||
			fixedCloseMarket(f.Spec, a, attempt.At) != nil {
			return errors.New("resting TP attempt lacks exact opening ownership, target or source evidence")
		}
		refs[attempt.Ref] = true
		return nil
	}
	for entryID, attempt := range f.RestingTakeProfit {
		if err := validateAttempt(entryID, attempt); err != nil {
			return err
		}
	}
	for ref, rearm := range f.RestingUnsentRearms {
		if ref != rearm.Attempt.Ref || rearm.At.Before(rearm.Attempt.At) || !fixedSHA(rearm.SourceSHA) {
			return errors.New("resting TP unsent rearm has invalid source or timing")
		}
		if err := validateAttempt(rearm.Attempt.Candidate.EntryOrderID, rearm.Attempt); err != nil {
			return err
		}
		for _, in := range s.Intents {
			if in.Ref == ref {
				return errors.New("resting TP rearm overlaps an unresolved submission")
			}
		}
		for _, receipt := range s.CompletedOrders {
			if receipt.Ref == ref {
				return errors.New("resting TP rearm overlaps a broker-receipted attempt")
			}
		}
	}
	for _, in := range s.Intents {
		if !in.Candidate.Closing {
			continue
		}
		a, found := f.RestingTakeProfit[in.Candidate.EntryOrderID]
		if !found || a.Ref != in.Ref || !a.At.Equal(in.At) || a.Candidate != in.Candidate || !fixedCloseOwnership(s, in.Candidate) {
			return errors.New("resting TP intent differs from its durable single-use attempt")
		}
	}
	return nil
}

func (c *FixedController) onlyRestingIntents() bool {
	for _, in := range c.state.Intents {
		if !c.spec.HasRestingTP75() || !in.Candidate.RestingTakeProfit || in.OrderID == "" {
			return false
		}
	}
	return true
}

// Readiness may accept existing acknowledged targets, but never a lost receipt,
// a changed order or a fill whose position reporting has not caught up.
func RestingTP75OrdersAcknowledged(policy string, intents map[string]Intent, s Snapshot) bool {
	for symbol, in := range intents {
		a := in.Candidate
		if (policy != FixedConservativeRestingTP75Policy && policy != FixedConservativeASAPPolicy) || !a.RestingTakeProfit || !a.Closing || in.OrderID == "" {
			return false
		}
		count := 0
		for _, o := range s.Orders {
			if o.ID != in.OrderID {
				continue
			}
			if o.Terminal() || o.Filled != 0 || o.Symbol != symbol || o.OptionID != a.ID ||
				o.Ref != "" && o.Ref != in.Ref || o.Side != "buy" || o.Effect != "close" || o.Quantity != 1 ||
				o.Type != "limit" || o.TimeInForce != "gtc" || o.Limit != a.CloseLimit {
				return false
			}
			count++
		}
		if count != 1 {
			return false
		}
	}
	return true
}

func (c *FixedController) CheckPendingOrders(s Snapshot) error {
	_, err := c.restingCloseCommitment(s)
	return err
}

// Reserve the full possible debit plus its fee for every acknowledged working
// target. Never fund another order from expected collateral release or fills.
func (c *FixedController) restingCloseCommitment(s Snapshot) (Cents, error) {
	if !RestingTP75OrdersAcknowledged(c.spec.ID, c.state.Intents, s) {
		return 0, errors.New("unresolved submission blocks further orders")
	}
	var debit Cents
	for _, in := range c.state.Intents {
		amount := in.Candidate.CloseLimit*100 + c.spec.OptionFeeBudget
		if amount > MaxMoney-debit {
			return 0, errors.New("resting TP commitment is missing, duplicated or overflows")
		}
		debit += amount
	}
	return debit, nil
}

// PendingRestingTakeProfit excludes every attempted opening lifecycle, including
// rejected/cancelled orders. It grants no authority without the fresh checks.
func (c *FixedController) PendingRestingTakeProfit() []string {
	if c.check() != nil || !c.spec.HasRestingTP75() {
		return nil
	}
	var out []string
	for _, symbol := range slices.Sorted(maps.Keys(c.state.Fixed.Contracts)) {
		if _, pending := c.state.Intents[symbol]; pending {
			continue
		}
		p, op := c.state.Positions[symbol], c.state.Fixed.Contracts[symbol]
		opened := p.PutOpenedAt
		if op.Kind == "call" {
			opened = p.CallOpenedAt
		}
		for id, r := range c.state.CompletedOrders {
			if !r.Closing && r.Filled == 1 && r.Symbol == symbol && r.OptionID == op.ID && r.At.Equal(opened) {
				if _, attempted := c.state.Fixed.RestingTakeProfit[id]; !attempted {
					out = append(out, symbol)
				}
				break
			}
		}
	}
	return out
}

func (c *FixedController) checkRestingTakeProfit(config Config, a Candidate, s Snapshot, now time.Time) error {
	if err := c.nativeConfig(config); err != nil {
		return err
	}
	if !c.spec.HasRestingTP75() || !a.RestingTakeProfit || !a.TradingWindow(now) ||
		s.Account != c.spec.Account || !fresh(s.At, now, c.spec.MaxQuoteAge) ||
		!fixedCloseOwnership(*c.state, a) || c.state.Fixed.Contracts[a.Symbol] != a.Option ||
		len(c.state.CompletedOrders)+len(c.state.Intents) >= 1024 || len(c.state.Fixed.RestingTakeProfit)+len(c.state.Fixed.RestingUnsentRearms) >= 1024 {
		return errors.New("resting TP lacks fresh account, exact ownership, capacity or an open session")
	}
	if _, attempted := c.state.Fixed.RestingTakeProfit[a.EntryOrderID]; attempted {
		return errors.New("resting TP lifecycle already attempted; never retry automatically")
	}
	if _, pending := c.state.Intents[a.Symbol]; pending {
		return errors.New("resting TP symbol already has an unresolved order")
	}
	if err := fixedCloseMarket(c.spec, a, now); err != nil {
		return err
	}
	committed, err := c.restingCloseCommitment(s)
	if err != nil {
		return err
	}
	count, stock := 0, false
	for _, op := range s.Options {
		if op.Symbol == a.Symbol {
			if op != a.Option {
				return errors.New("resting TP broker contract drift")
			}
			count++
		}
	}
	for _, h := range s.Shares {
		if h.Symbol == a.Symbol {
			if a.Kind == "put" && h.Quantity != 0 {
				return errors.New("resting TP put overlaps assignment stock")
			}
			stock = h.Quantity == 100 && h.Cost == c.state.Positions[a.Symbol].Cost
		}
	}
	debit := a.CloseLimit*100 + c.spec.OptionFeeBudget
	if count != 1 || a.Kind == "call" && !stock || s.AvailableCash < config.Reserve ||
		committed > s.AvailableCash-config.Reserve || debit > s.AvailableCash-config.Reserve-committed {
		return errors.New("resting TP lacks exact broker inventory or settled cash after existing commitments and reserve")
	}
	return nil
}

// RearmFixedUnsentTP75 is operator-only. A durable claim with neither its intent
// nor a completed receipt can arise only from clearUnsent after ErrNotSubmitted:
// reconciliation always records a broker terminal receipt before clearing the
// intent, and unknown transport always retains it. Broker-history absence alone
// is never sufficient. The caller pauses, locks and archives the exact journal.
func RearmFixedUnsentTP75(s State, config Config, symbol, ref, sourceSHA string, now time.Time) (State, error) {
	if err := s.Validate(); err != nil {
		return State{}, err
	}
	if err := config.Validate(); err != nil {
		return State{}, err
	}
	if s.Fixed == nil || config.FixedPolicy == nil || !s.Fixed.Spec.HasRestingTP75() ||
		s.Fixed.PolicySHA != config.FixedPolicy.Spec.Digest() || config.FixedPolicy.ImportLegacyStateSHA != fixedLegacyBinding(s.Fixed) ||
		len(s.Intents) != 0 || symbol == "" || ref == "" || !fixedSHA(sourceSHA) || now.IsZero() {
		return State{}, errors.New("unsent TP rearm requires its exact paused policy, source and no unresolved submission")
	}
	entryID := ""
	var attempt FixedRestingClose
	for id, a := range s.Fixed.RestingTakeProfit {
		if a.Ref == ref && a.Candidate.Symbol == symbol {
			entryID, attempt = id, a
		}
	}
	if entryID == "" || now.Before(attempt.At) || !fixedCloseOwnership(s, attempt.Candidate) {
		return State{}, errors.New("unsent TP rearm differs from the exact current owned attempt")
	}
	for _, receipt := range s.CompletedOrders {
		if receipt.Ref == ref {
			return State{}, errors.New("broker-receipted TP attempts cannot be rearmed")
		}
	}
	f := *s.Fixed
	f.RestingTakeProfit, f.RestingUnsentRearms = maps.Clone(f.RestingTakeProfit), maps.Clone(f.RestingUnsentRearms)
	if f.RestingUnsentRearms == nil {
		f.RestingUnsentRearms = map[string]FixedUnsentRearm{}
	}
	f.RestingUnsentRearms[ref] = FixedUnsentRearm{Attempt: attempt, SourceSHA: sourceSHA, At: now}
	delete(f.RestingTakeProfit, entryID)
	s.Fixed = &f
	return s, s.Validate()
}

// ErrRestingTPDeferred marks a failure confined to one symbol's target before
// its single-use claim: its quote, review or funding. Nothing was claimed, so
// the cycle may try another symbol's pending target. A claimed attempt, even a
// proven-unsent one, is never deferred: its cause may be account-wide.
var ErrRestingTPDeferred = errors.New("resting TP deferred for this symbol")

func restingTPDeferred(err error) error { return errors.Join(ErrRestingTPDeferred, err) }

// RestingTargetGaps reports each owned short without a target intent: "pending"
// before its single attempt, "no_working_target" afterwards (cancelled, rejected
// or proven unsent), which then holds to expiry without operator recovery.
func (c *FixedController) RestingTargetGaps() map[string]string {
	if c.check() != nil || !c.spec.HasRestingTP75() {
		return nil
	}
	pending := c.PendingRestingTakeProfit()
	gaps := map[string]string{}
	for symbol := range c.state.Fixed.Contracts {
		// A working target, or an unresolved one that already blocks orders.
		if _, intent := c.state.Intents[symbol]; intent {
			continue
		}
		gaps[symbol] = "no_working_target"
		if slices.Contains(pending, symbol) {
			gaps[symbol] = "pending"
		}
	}
	return gaps
}

// ExecuteRestingTakeProfit places the fixed target even while the ask is above
// it. The ref and single-use lifecycle claim are saved together before transport.
func (c *FixedController) ExecuteRestingTakeProfit(ctx context.Context, config Config, broker FixedCloseBroker, symbol string, clock func() time.Time) error {
	if broker == nil || clock == nil {
		return errors.New("resting TP needs broker and clock")
	}
	if err := c.nativeConfig(config); err != nil {
		return err
	}
	s, err := broker.Snapshot(ctx)
	if err != nil {
		return err
	}
	if err = c.ReconcileNative(config, s, clock()); err != nil {
		return err
	}
	// Account-wide blocks stop the cycle; they are never deferred per symbol.
	if _, err = c.restingCloseCommitment(s); err != nil {
		return err
	}
	if len(c.state.CompletedOrders)+len(c.state.Intents) >= 1024 || len(c.state.Fixed.RestingTakeProfit)+len(c.state.Fixed.RestingUnsentRearms) >= 1024 {
		return errors.New("resting TP attempt ledger is full")
	}
	a, err := broker.CloseQuote(ctx, *c.state, symbol)
	if err != nil {
		return restingTPDeferred(err)
	}
	if err = c.checkRestingTakeProfit(config, a, s, clock()); err != nil {
		return restingTPDeferred(err)
	}
	if err = broker.Review(ctx, a); err != nil {
		return restingTPDeferred(err)
	}
	s, err = broker.Snapshot(ctx)
	if err != nil {
		return err
	}
	if err = c.ReconcileNative(config, s, clock()); err != nil {
		return err
	}
	if err = c.checkRestingTakeProfit(config, a, s, clock()); err != nil {
		return restingTPDeferred(err)
	}
	ref, err := newRef()
	if err != nil {
		return err
	}
	next, f := *c.state, c.copy()
	next.Fixed, next.Intents = &f, maps.Clone(next.Intents)
	if f.RestingTakeProfit == nil {
		f.RestingTakeProfit = map[string]FixedRestingClose{}
	}
	at := clock()
	f.RestingTakeProfit[a.EntryOrderID] = FixedRestingClose{Ref: ref, Candidate: a, At: at}
	next.Intents[symbol] = Intent{Ref: ref, Candidate: a, At: at}
	if err = c.saveState(next); err != nil {
		return err
	}
	clearUnsent := func(cause error) error {
		next := *c.state
		next.Intents = maps.Clone(next.Intents)
		delete(next.Intents, symbol)
		return errors.Join(cause, c.saveState(next))
	}
	deadline, _ := FixedRestingCloseDeadline(at)
	guard := func() error {
		if err := c.check(); err != nil {
			return err
		}
		now := clock()
		if ctx.Err() != nil || !a.TradingWindow(now) || !now.Before(deadline) || c.state.Intents[symbol].Ref != ref ||
			!fresh(a.At, now, c.spec.MaxQuoteAge) || !fresh(a.UnderlyingAt, now, c.spec.MaxQuoteAge) || !fresh(s.At, now, c.spec.MaxQuoteAge) {
			return ErrNotSubmitted
		}
		return nil
	}
	if err = guard(); err != nil {
		return clearUnsent(errors.Join(ErrNotSubmitted, err))
	}
	transportDeadline := deadline
	for _, source := range []time.Time{a.At, a.UnderlyingAt, s.At} {
		if expires := source.Add(time.Duration(c.spec.MaxQuoteAge) * time.Second); expires.Before(transportDeadline) {
			transportDeadline = expires
		}
	}
	transport, cancel := context.WithDeadline(ctx, transportDeadline)
	defer cancel()
	id, err := broker.SubmitFixed(transport, a, ref, deadline, guard)
	if err != nil {
		if errors.Is(err, ErrNotSubmitted) {
			return clearUnsent(err)
		}
		return errors.New("resting TP outcome unknown; durable intent retained, no retry")
	}
	if id == "" {
		return errors.New("resting TP receipt missing; durable intent retained, no retry")
	}
	next = *c.state
	next.Intents = maps.Clone(next.Intents)
	in := next.Intents[symbol]
	in.OrderID = id
	next.Intents[symbol] = in
	return c.saveState(next)
}

// The caller holds the journal lock under durable HALT, revokes activation,
// archives the source journal and persists the result. Ownership and its original
// binding time survive. No current cohort or pending financial write can cross.
func EnableFixedRestingTP75(s State, config Config, now time.Time) (State, error) {
	if err := s.Validate(); err != nil {
		return State{}, err
	}
	if err := config.Validate(); err != nil {
		return State{}, err
	}
	if s.Fixed == nil || config.FixedPolicy == nil || s.Fixed.Spec.ID != FixedConservativeTP75Policy ||
		config.FixedPolicy.Spec.ID != FixedConservativeRestingTP75Policy || s.Account != config.Account ||
		now.IsZero() || now.Before(s.Fixed.BoundAt) || len(s.Intents) != 0 ||
		config.FixedPolicy.ImportLegacyStateSHA != fixedLegacyBinding(s.Fixed) {
		return State{}, errors.New("resting TP transition requires the exact paused TP75 ownership with no unresolved submissions")
	}
	before, after := s.Fixed.Spec, config.FixedPolicy.Spec
	before.ID = after.ID
	if before.Digest() != after.Digest() || before.FeeSourceSHA != after.FeeSourceSHA || !before.FeeKnownAt.Equal(after.FeeKnownAt) {
		return State{}, errors.New("resting TP transition preserves account, capital, fees and source settings")
	}
	f := *s.Fixed
	if f.Batch != nil {
		if f.Batch.Phase != "sealed" || !f.Batch.LatestAt.Before(now) {
			return State{}, errors.New("resting TP transition waits for the existing entry cohort to end")
		}
		b := *f.Batch
		b.Signals = slices.Clone(b.Signals)
		for i := range b.Signals {
			if b.Signals[i].Phase == "ready" {
				b.Signals[i].Phase, b.Signals[i].TransitionAt = "expired", now
			}
		}
		f.Batch = &b
	}
	f.Spec, f.PolicySHA, f.RestingTP75Since = after, after.Digest(), now
	f.TakeProfit = nil // The caller archives the complete original daily evidence.
	s.Fixed = &f
	return s, s.Validate()
}
