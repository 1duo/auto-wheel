package wheel

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"autowheel/internal/contractpolicy"
)

var ErrASAPDeferred = errors.New("ASAP opportunity has not been claimed")

// Claims survive failed reviews, unsent transports and restart. A fresh quote
// can be observed again before a claim; a claimed symbol cannot be rearmed by
// the account loop during that exchange session.
type FixedASAPAttempt struct {
	Signal    FixedSignal `json:"signal"`
	Ref       string      `json:"ref_id"`
	At        time.Time   `json:"at"`
	Deadline  time.Time   `json:"deadline"`
	RosterSHA string      `json:"roster_sha256"`
}

type FixedASAPState struct {
	Initial           bool                        `json:"initial_native_binding,omitzero"`
	Since             time.Time                   `json:"since"`
	SourceStateSHA    string                      `json:"source_state_sha256"`
	PreviousPolicySHA string                      `json:"previous_policy_sha256"`
	LastScanAt        time.Time                   `json:"last_scan_at,omitzero"`
	LastCapacitySHA   string                      `json:"last_capacity_sha256,omitempty"`
	Attempts          map[string]FixedASAPAttempt `json:"attempts,omitempty"`
}

func asapKey(symbol string, at time.Time) string {
	return at.In(Eastern).Format(time.DateOnly) + ":" + symbol
}

// ASAPEntryDeadline preserves the existing native opening guard and its last
// 15-minute exclusion. Resting take-profit orders retain their full session.
func ASAPEntryDeadline(now time.Time) (time.Time, bool) {
	close, known := SessionClose(now)
	return close.Add(-15 * time.Minute), known && TradingWindow(now)
}

// ASAPEntryOpens is the first moment a session's ASAP openings may be claimed:
// the scheduled close-30m sale. The 2021-2026 causal entry replay found earlier
// same-day sales paid wider spreads and larger tails with no timing benefit.
// It gates new claims only; stored claims keep their original session binding.
func ASAPEntryOpens(now time.Time) (time.Time, bool) {
	close, known := SessionClose(now)
	return close.Add(-30 * time.Minute), known && TradingWindow(now)
}

func asapEntryOpen(now time.Time) bool {
	opens, known := ASAPEntryOpens(now)
	deadline, open := ASAPEntryDeadline(now)
	return known && open && !now.Before(opens) && now.Before(deadline)
}

func (f FixedPolicyState) validateASAP(s State) error {
	r := f.Replenishment
	if f.Spec.ID != FixedConservativeASAPPolicy {
		if r != nil {
			return errors.New("ASAP authority requires its explicit financial policy")
		}
		return nil
	}
	previous := f.Spec
	previous.ID, previous.Limits.Slots = FixedConservativeRestingTP75Policy, 0
	if r == nil || r.Since.IsZero() || r.Since.Before(f.RestingTP75Since) || !fixedSHA(r.SourceStateSHA) ||
		!r.Initial && r.PreviousPolicySHA != previous.Digest() || r.Initial && (r.PreviousPolicySHA != "" || !r.Since.Equal(f.BoundAt) || !r.Since.Equal(f.RestingTP75Since) || f.Legacy != nil) || len(r.Attempts) > 512 ||
		!r.LastScanAt.IsZero() && (r.LastScanAt.Before(r.Since) || !fixedSHA(r.LastCapacitySHA)) ||
		r.LastScanAt.IsZero() && r.LastCapacitySHA != "" {
		return errors.New("ASAP migration, source or scan binding is invalid")
	}
	refs := map[string]bool{}
	for key, attempt := range r.Attempts {
		x := attempt.Signal
		deadline, open := ASAPEntryDeadline(attempt.At)
		if key != asapKey(x.Symbol, attempt.At) || !fixedSymbol(x.Symbol) || attempt.Ref == "" || refs[attempt.Ref] ||
			attempt.At.Before(r.Since) || !open || !attempt.Deadline.Equal(deadline) || !fixedSHA(attempt.RosterSHA) ||
			x.Phase != "claimed" || !x.TransitionAt.Equal(attempt.At) || x.Contract == nil || x.Contract.Symbol != x.Symbol || x.Contract.Kind != x.Kind ||
			x.ObservedAt.After(attempt.At) || !fresh(x.ObservedAt, attempt.At, f.Spec.MaxQuoteAge) ||
			!fixedSHA(x.MenuSHA) || !fixedSHA(x.DataSHA) || x.DataSHA != fixedSignalDigest(x) ||
			!contractpolicy.EntryBookUsable(x.Stock) || !contractpolicy.EntryBookUsable(x.Option) ||
			x.Option.Bid*100 <= int64(f.Spec.OptionFeeBudget) || x.BidSize < 1 || x.Tick < 1 || x.Option.Bid%int64(x.Tick) != 0 {
			return errors.New("ASAP single-use claim lacks exact source, identity or session binding")
		}
		for _, at := range []time.Time{x.BidAt, x.AskAt, x.OptionAt} {
			if !fresh(at, x.ObservedAt, f.Spec.MaxQuoteAge) {
				return errors.New("ASAP captured source clock is invalid")
			}
		}
		chosen, err := contractpolicy.Choose("conservative", x.Kind, attempt.At.In(Eastern).Format(time.DateOnly), x.Symbol, x.Stock, []contractpolicy.Contract{*x.Contract})
		if err != nil || chosen == nil || *chosen != *x.Contract {
			return errors.New("ASAP claim contract is outside the original conservative target")
		}
		if x.Kind == "call" {
			allowed, err := contractpolicy.StockCostCallAllowed(x.Contract.Strike, int64(x.AcquisitionCeiling), int64(f.Spec.PhysicalFeeBudget))
			if err != nil || !allowed || !fixedSHA(x.BasisSHA) {
				return errors.New("ASAP call claim lacks its owned-stock acquisition floor")
			}
		} else if x.Kind != "put" || x.AcquisitionCeiling != 0 || x.BasisSHA != "" {
			return errors.New("ASAP put claim contains invalid ownership terms")
		}
		refs[attempt.Ref] = true
	}
	for symbol, in := range s.Intents {
		if in.Candidate.Closing {
			continue
		}
		a, found := r.Attempts[asapKey(symbol, in.At)]
		q := in.Candidate
		if !found || a.Ref != in.Ref || in.At.Before(a.At) || !in.At.Before(a.Deadline) || a.Signal.Contract == nil || q.Quantity != 1 ||
			(contractpolicy.Contract{ID: q.ID, Symbol: q.Symbol, Kind: q.Kind, Expiry: q.Expiry, Strike: int64(q.Strike)}) != *a.Signal.Contract {
			return errors.New("ASAP opening intent differs from its durable single-use claim")
		}
	}
	return nil
}

func (f FixedPolicyState) asapOwnedOpening(s State, symbol string, p Position) bool {
	if f.Replenishment == nil || p.PutOpenedAt.Before(f.Replenishment.Since) {
		return false
	}
	a, found := f.Replenishment.Attempts[asapKey(symbol, p.PutOpenedAt)]
	if !found || a.Signal.Kind != "put" || a.Signal.Contract == nil || p.PutOpenedAt.Before(a.At) || !p.PutOpenedAt.Before(a.Deadline) {
		return false
	}
	if p.PutID != "" && (p.PutID != a.Signal.Contract.ID || p.PutStrike != Cents(a.Signal.Contract.Strike) || p.PutExpiry != a.Signal.Contract.Expiry) {
		return false
	}
	for _, receipt := range s.CompletedOrders {
		if !receipt.Closing && receipt.Filled == 1 && receipt.Ref == a.Ref && receipt.Symbol == symbol &&
			receipt.OptionID == a.Signal.Contract.ID && receipt.At.Equal(p.PutOpenedAt) {
			return true
		}
	}
	return false
}

func (f FixedPolicyState) openingClockAllowed(s State, symbol string, p Position) bool {
	if f.Replenishment != nil && !p.PutOpenedAt.Before(f.Replenishment.Since) {
		return f.asapOwnedOpening(s, symbol, p)
	}
	_, entry, latest, known := FixedEntryTimes(p.PutOpenedAt)
	return known && !p.PutOpenedAt.Before(f.BoundAt) && !p.PutOpenedAt.Before(entry) && !p.PutOpenedAt.After(latest)
}

// EnableFixedASAP is a paused, archived transition from the exact resting
// policy. Its acknowledged broker orders are retained and reverified at startup.
func EnableFixedASAP(s State, config Config, now time.Time) (State, error) {
	if err := s.Validate(); err != nil {
		return State{}, err
	}
	if err := config.Validate(); err != nil {
		return State{}, err
	}
	if s.Fixed == nil || config.FixedPolicy == nil || s.Fixed.Spec.ID != FixedConservativeRestingTP75Policy ||
		config.FixedPolicy.Spec.ID != FixedConservativeASAPPolicy || s.Account != config.Account ||
		now.IsZero() || now.Before(s.Fixed.RestingTP75Since) || config.FixedPolicy.ImportLegacyStateSHA != fixedLegacyBinding(s.Fixed) {
		return State{}, errors.New("ASAP transition requires the exact paused resting TP75 ownership")
	}
	before, after := s.Fixed.Spec, config.FixedPolicy.Spec
	before.ID, before.Limits.Slots = after.ID, 5
	if before.Digest() != after.Digest() || before.FeeSourceSHA != after.FeeSourceSHA || !before.FeeKnownAt.Equal(after.FeeKnownAt) {
		return State{}, errors.New("ASAP transition preserves account, capital, reserve, fees and source settings")
	}
	for _, in := range s.Intents {
		if !in.Candidate.RestingTakeProfit || !in.Candidate.Closing || in.OrderID == "" {
			return State{}, errors.New("ASAP transition cannot cross an unknown or opening submission")
		}
	}
	if s.Fixed.Batch != nil && (!s.Fixed.Batch.LatestAt.Before(now) || s.Fixed.Batch.Phase != "sealed") {
		return State{}, errors.New("ASAP transition waits for the old opening cohort to end")
	}
	f := *s.Fixed
	f.Replenishment = &FixedASAPState{Since: now, SourceStateSHA: fixedDigest(s), PreviousPolicySHA: s.Fixed.PolicySHA}
	f.Spec, f.PolicySHA = after, after.Digest()
	s.Fixed = &f
	return s, s.Validate()
}

func (c *FixedController) ASAPSymbols(config Config, s Snapshot, now time.Time) []string {
	if c.check() != nil || c.spec.ID != FixedConservativeASAPPolicy || !asapEntryOpen(now) ||
		c.state.Fixed.Replenishment == nil || !c.onlyRestingIntents() || !fresh(s.At, now, c.spec.MaxQuoteAge) || s.Account != c.spec.Account {
		return nil
	}
	committed, err := c.restingCloseCommitment(s)
	if err != nil || committed > MaxMoney-c.spec.OptionFeeBudget || s.AvailableCash < committed+c.spec.OptionFeeBudget ||
		s.AvailableCash-committed-c.spec.OptionFeeBudget < Cents(c.spec.Limits.Reserve) {
		return nil
	}
	order, err := fixedOrder(slices.Sorted(maps.Keys(c.state.Positions)), config.FixedPolicy.Roster.Symbols)
	if err != nil {
		return nil
	}
	var out []string
	for _, symbol := range order {
		if s.OrderCoveragePolicy == CompleteMoomooOrders && slices.Contains(s.ExternalSymbols, symbol) {
			continue
		}
		p, managed := c.state.Positions[symbol]
		if p.PutID != "" || p.CallID != "" || !p.MissingSince.IsZero() || !managed && len(c.state.Positions) >= c.spec.Limits.ManagedSlots() {
			continue
		}
		if _, claimed := c.state.Fixed.Replenishment.Attempts[asapKey(symbol, now)]; claimed {
			continue
		}
		closedToday := false
		for _, r := range c.state.CompletedOrders {
			at := r.ResolvedAt
			if at.IsZero() {
				at = r.At
			}
			if r.Symbol == symbol && r.Closing && r.Filled == 1 && at.In(Eastern).Format(time.DateOnly) >= now.In(Eastern).Format(time.DateOnly) {
				closedToday = true
			}
		}
		if !closedToday {
			out = append(out, symbol)
		}
	}
	return out
}

// BeginASAPScan is durable request throttling, not an order capability. A real
// reconciled cash, ownership or target change bypasses the unchanged-capacity
// cooldown. A restart cannot silently run the same scan repeatedly.
func (c *FixedController) BeginASAPScan(config Config, s Snapshot, now time.Time) (bool, error) {
	if err := c.nativeConfig(config); err != nil {
		return false, err
	}
	if len(c.ASAPSymbols(config, s, now)) == 0 {
		return false, nil
	}
	hash := fixedDigest(struct {
		Cash      Cents
		Positions map[string]Position
		Intents   map[string]Intent
	}{s.AvailableCash, c.state.Positions, c.state.Intents})
	r := c.state.Fixed.Replenishment
	if now.Before(r.LastScanAt) {
		return false, errors.New("ASAP scan clock moved backwards")
	}
	if hash == r.LastCapacitySHA && now.Sub(r.LastScanAt) < time.Minute {
		return false, nil
	}
	f := c.copy()
	f.Replenishment.LastScanAt, f.Replenishment.LastCapacitySHA = now, hash
	return true, c.save(f)
}

// ExecuteASAP selects from a causal fresh menu, proves funding and exact
// deliverables before the durable daily claim, then uses the shared reviewed
// executor. No review, save or submission failure can recreate this claim.
func (c *FixedController) ExecuteASAP(ctx context.Context, config Config, b FixedBroker, input FixedSignalEvidence, clock func() time.Time) error {
	if b == nil || clock == nil {
		return errors.New("ASAP execution needs a broker and clock")
	}
	if err := c.nativeConfig(config); err != nil {
		return err
	}
	now := clock()
	symbol := input.Contract.Symbol
	if c.spec.ID != FixedConservativeASAPPolicy || !slices.Contains(config.FixedPolicy.Roster.Symbols, symbol) && c.state.Positions[symbol].OwnedShares != 100 ||
		!asapEntryOpen(now) || config.FixedPolicy.Roster.KnownAt.After(now) || config.FixedPolicy.Roster.From > now.In(Eastern).Format(time.DateOnly) || config.FixedPolicy.Roster.Through < now.In(Eastern).Format(time.DateOnly) {
		return errors.Join(ErrASAPDeferred, errors.New("ASAP entry window, session or roster is unavailable"))
	}
	chosen, err := contractpolicy.Choose("conservative", input.Contract.Kind, now.In(Eastern).Format(time.DateOnly), symbol, input.Stock, input.Menu)
	if err != nil || chosen == nil || *chosen != input.Contract {
		return errors.Join(ErrASAPDeferred, errors.New("ASAP conservative menu identity is unproven"))
	}
	signal := FixedSignal{Symbol: symbol, Kind: input.Contract.Kind, Contract: new(*chosen), ObservedAt: now, Stock: input.Stock, Option: input.Option, BidAt: input.BidAt, AskAt: input.AskAt, OptionAt: input.OptionAt, BidSize: input.BidSize, Tick: input.Tick, MenuSHA: fixedDigest(input.Menu)}
	if signal.Kind == "call" {
		signal.AcquisitionCeiling, signal.BasisSHA, err = fixedPositionBasis(c.spec, c.state.Fixed.PolicySHA, c.state.Positions[symbol])
		if err != nil {
			return errors.Join(ErrASAPDeferred, err)
		}
	}
	signal.DataSHA = fixedSignalDigest(signal)
	s, e, err := c.nativeExecutionReads(ctx, config, b, signal, clock)
	if err != nil {
		return errors.Join(ErrASAPDeferred, err)
	}
	now = clock()
	if !slices.Contains(c.ASAPSymbols(config, s, now), symbol) {
		return errors.Join(ErrASAPDeferred, errors.New("ASAP funding, capacity or single-use lifecycle is unavailable"))
	}
	if _, err = c.openingCandidate(signal, e, s, now, true); err != nil {
		return errors.Join(ErrASAPDeferred, err)
	}
	return c.executeNativeClaimed(ctx, config, b, signal, e, s, clock)
}
