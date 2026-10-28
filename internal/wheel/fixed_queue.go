package wheel

import (
	"errors"
	"maps"
	"slices"
	"time"

	"autowheel/internal/contractpolicy"
)

// FixedStore uses the existing account journal and an immutable weekly archive.
// No method in this interface can call a broker or submit an order.
type FixedStore interface {
	Store
	ArchiveFixedBatch(FixedBatch) (string, error)
}

var ErrFixedSignalAbstained = errors.New("fixed signal was durably abstained")
var ErrFixedJournalDurability = errors.New("fixed journal durability is uncertain; restart required")

// FixedController is called by the serialized account cycle. Provider reads may
// run in parallel; these journal transitions must remain serialized.
type FixedController struct {
	spec     FixedPolicySpec
	state    *State
	store    FixedStore
	poisoned bool
}

func NewFixedController(spec FixedPolicySpec, state *State, store FixedStore) (*FixedController, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if state == nil || store == nil {
		return nil, errors.New("fixed controller needs the locked account journal")
	}
	if spec.HasTP75() {
		if _, ok := store.(FixedPolicyArchiver); !ok {
			return nil, errors.New("TP75 requires a durable policy journal archiver")
		}
	}
	if err := state.Validate(); err != nil {
		return nil, err
	}
	if state.Account != spec.Account || state.Fixed != nil && state.Fixed.PolicySHA != spec.Digest() {
		return nil, errors.New("fixed controller account or financial policy differs from journal")
	}
	return &FixedController{spec: spec, state: state, store: store}, nil
}

func (c *FixedController) check() error {
	if c.poisoned {
		return errors.New("fixed journal save failed; restart from durable state before continuing")
	}
	if err := c.state.Validate(); err != nil {
		return err
	}
	if c.state.Fixed == nil || c.state.Fixed.PolicySHA != c.spec.Digest() {
		return errors.New("fixed financial policy is not bound")
	}
	return nil
}

func (c *FixedController) save(f FixedPolicyState) error {
	next := *c.state
	next.Fixed = &f
	return c.saveState(next)
}

func (c *FixedController) saveState(next State) error {
	if err := next.Validate(); err != nil {
		return err
	}
	// Retain the conservative in-memory transition if durability is uncertain.
	// No caller receives a claim after a failed Save, and this controller fences
	// further mutations until the journal is reopened and reconciled.
	*c.state = next
	if err := c.store.Save(next); err != nil {
		c.poisoned = true
		return errors.Join(ErrFixedJournalDurability, err)
	}
	return nil
}

func (c *FixedController) copy() FixedPolicyState {
	f := *c.state.Fixed
	f.Contracts = maps.Clone(f.Contracts)
	f.RestingTakeProfit = maps.Clone(f.RestingTakeProfit)
	f.RestingUnsentRearms = maps.Clone(f.RestingUnsentRearms)
	if f.Replenishment != nil {
		r := *f.Replenishment
		r.Attempts = maps.Clone(r.Attempts)
		f.Replenishment = &r
	}
	if f.TakeProfit != nil {
		b := *f.TakeProfit
		b.Signals = slices.Clone(b.Signals)
		for i := range b.Signals {
			if b.Signals[i].Candidate != nil {
				b.Signals[i].Candidate = new(*b.Signals[i].Candidate)
			}
		}
		f.TakeProfit = &b
	}
	if f.Batch != nil {
		b := *f.Batch
		b.Managed = slices.Clone(b.Managed)
		b.Roster.Symbols = slices.Clone(b.Roster.Symbols)
		b.Signals = slices.Clone(b.Signals)
		for i := range b.Signals {
			if b.Signals[i].Contract != nil {
				b.Signals[i].Contract = new(*b.Signals[i].Contract)
			}
		}
		f.Batch = &b
	}
	return f
}

// Bind requires a successful fresh broker snapshot and flat old lifecycles.
// It does not retire a peer, acquire the operational lock or authorize trading.
func (c *FixedController) Bind(s Snapshot, now time.Time) error {
	if c.poisoned || s.Account != c.spec.Account || !fresh(s.At, now, c.spec.MaxQuoteAge) || c.spec.FeeKnownAt.After(now) {
		return errors.New("fixed binding lacks fresh account or fee evidence")
	}
	if c.state.Fixed != nil {
		return c.check()
	}
	if len(c.state.Positions) != 0 || len(c.state.Intents) != 0 || len(s.Options) != 0 ||
		(s.OrderCoveragePolicy != CompleteMoomooOrders && (len(s.ExternalSymbols) != 0 || s.ExternalCapital != 0)) {
		return errors.New("fixed policy transition requires broker-confirmed flat old lifecycles")
	}
	for _, h := range s.Shares {
		if h.Quantity != 0 {
			return errors.New("fixed policy cannot adopt pre-existing stock")
		}
	}
	for _, o := range s.Orders {
		if !o.Terminal() {
			return errors.New("fixed policy transition has a working broker order")
		}
	}
	f := FixedPolicyState{Version: 1, Spec: c.spec, PolicySHA: c.spec.Digest(), BoundAt: now}
	if c.spec.HasRestingTP75() {
		f.RestingTP75Since = now
	}
	if c.spec.ID == FixedConservativeASAPPolicy {
		if s.OrderCoveragePolicy != CompleteMoomooOrders {
			return errors.New("fresh ASAP binding requires complete independent Moomoo facts")
		}
		f.Replenishment = &FixedASAPState{Initial: true, Since: now, SourceStateSHA: fixedDigest(*c.state)}
	}
	return c.save(f)
}

// Begin persists a capture reservation BEFORE provider reads. The complete
// managed/roster ordering is frozen; partial source failures cannot change it.
func (c *FixedController) Begin(roster FixedRoster, now time.Time) error {
	if err := c.check(); err != nil {
		return err
	}
	signal, execution, latest, ok := FixedEntryTimes(now)
	if !ok || now.Before(signal) || !now.Before(execution) || !c.onlyRestingIntents() {
		return errors.New("fixed signal capture is outside its bucket or has unresolved submission")
	}
	f := c.copy()
	previous := ""
	if f.Batch != nil {
		if !f.Batch.SignalAt.Before(signal) {
			return errors.New("fixed signal cohort already reserved; never recapture it")
		}
		if err := c.Recover(now); err != nil {
			return err
		}
		f = c.copy()
		var err error
		previous, err = c.store.ArchiveFixedBatch(*f.Batch)
		if err != nil || !fixedSHA(previous) {
			return errors.Join(errors.New("prior fixed signal batch was not durably archived"), err)
		}
	}
	managed := slices.Sorted(maps.Keys(c.state.Positions))
	order, err := fixedOrder(managed, roster.Symbols)
	if err != nil {
		return err
	}
	roster.Symbols = slices.Clone(roster.Symbols)
	b := &FixedBatch{PolicySHA: f.PolicySHA, PreviousSHA: previous, Day: signal.Format(time.DateOnly),
		SignalAt: signal, ExecutionAt: execution, LatestAt: latest, StartedAt: now,
		Phase: "capturing", Managed: managed, Roster: roster, FeeSourceSHA: c.spec.FeeSourceSHA, FeeKnownAt: c.spec.FeeKnownAt}
	for _, symbol := range order {
		p := c.state.Positions[symbol]
		entry := FixedSignal{Symbol: symbol, Kind: "put", Phase: "unobserved"}
		if p.OwnedShares == 100 {
			entry.Kind = "call"
		}
		if p.PutID != "" || p.CallID != "" || !p.MissingSince.IsZero() || p.OwnedShares == 100 && p.InventoryAt.IsZero() {
			entry.Phase, entry.Reason, entry.TransitionAt = "abstained", "lifecycle_busy", now
		}
		b.Signals = append(b.Signals, entry)
	}
	f.Batch = b
	return c.save(f)
}

func (c *FixedController) capture(symbol string, now time.Time) (FixedPolicyState, int, error) {
	if err := c.check(); err != nil {
		return FixedPolicyState{}, 0, err
	}
	f := c.copy()
	if f.Batch == nil || f.Batch.Phase != "capturing" || now.Before(f.Batch.StartedAt) || !now.Before(f.Batch.ExecutionAt) {
		return f, 0, errors.New("fixed capture reservation is absent or closed")
	}
	i := slices.IndexFunc(f.Batch.Signals, func(s FixedSignal) bool { return s.Symbol == symbol })
	if i < 0 || f.Batch.Signals[i].Phase != "unobserved" {
		return f, 0, errors.New("fixed signal identity is not awaiting observation")
	}
	return f, i, nil
}

func (c *FixedController) Record(symbol string, evidence FixedSignalEvidence, now time.Time) error {
	f, i, err := c.capture(symbol, now)
	if err != nil {
		return err
	}
	entry := f.Batch.Signals[i]
	chosen, err := contractpolicy.Choose("conservative", entry.Kind, f.Batch.Day, symbol, evidence.Stock, evidence.Menu)
	if err != nil || chosen == nil || *chosen != evidence.Contract {
		reason := "source_unavailable"
		if err == nil && chosen == nil {
			reason = "no_target"
		}
		return c.reject(f, i, reason, now)
	}
	entry.Contract, entry.ObservedAt = new(*chosen), now
	entry.Stock, entry.Option, entry.BidAt, entry.AskAt, entry.OptionAt = evidence.Stock, evidence.Option, evidence.BidAt, evidence.AskAt, evidence.OptionAt
	entry.Tick, entry.BidSize = evidence.Tick, evidence.BidSize
	entry.MenuSHA = fixedDigest(evidence.Menu)
	if entry.Kind == "call" {
		if !fixedPhysicalFeeCovered(c.spec, chosen.Strike) {
			return c.reject(c.copy(), i, "fee_budget", now)
		}
		entry.AcquisitionCeiling, entry.BasisSHA, err = fixedPositionBasis(c.spec, f.PolicySHA, c.state.Positions[symbol])
		if err != nil {
			return c.reject(c.copy(), i, "basis_unavailable", now)
		}
		allowed, err := contractpolicy.StockCostCallAllowed(chosen.Strike, int64(entry.AcquisitionCeiling), int64(c.spec.PhysicalFeeBudget))
		if err != nil || !allowed {
			return c.reject(c.copy(), i, "cost_floor", now)
		}
	}
	entry.Phase, entry.TransitionAt = "ready", now
	entry.DataSHA = fixedSignalDigest(entry)
	f.Batch.Signals[i] = entry
	next := *c.state
	next.Fixed = &f
	if err := next.Validate(); err != nil {
		return c.reject(c.copy(), i, "invalid_book", now)
	}
	return c.save(f)
}

func (c *FixedController) reject(f FixedPolicyState, i int, reason string, now time.Time) error {
	f.Batch.Signals[i] = FixedSignal{Symbol: f.Batch.Signals[i].Symbol, Kind: f.Batch.Signals[i].Kind,
		Phase: "abstained", Reason: reason, TransitionAt: now}
	if err := c.save(f); err != nil {
		return err
	}
	return ErrFixedSignalAbstained
}

func (c *FixedController) Abstain(symbol, reason string, now time.Time) error {
	f, i, err := c.capture(symbol, now)
	if err != nil {
		return err
	}
	if !fixedAbstention(reason) {
		return errors.New("unknown fixed abstention reason")
	}
	f.Batch.Signals[i].Phase, f.Batch.Signals[i].Reason, f.Batch.Signals[i].TransitionAt = "abstained", reason, now
	return c.save(f)
}

func (c *FixedController) Seal(now time.Time) error {
	if err := c.check(); err != nil {
		return err
	}
	f := c.copy()
	if f.Batch == nil || f.Batch.Phase != "capturing" || now.Before(f.Batch.StartedAt) || !now.Before(f.Batch.ExecutionAt) {
		return errors.New("fixed capture cannot be sealed at this clock")
	}
	f.Batch.Phase, f.Batch.SealedAt = "sealed", now
	return c.save(f) // Validate rejects any omitted/unobserved roster entry.
}

// Recover preserves already durable choices and seals unknown entries as
// explicit abstentions. It never fetches a later quote or cancels broker orders.
func (c *FixedController) Recover(now time.Time) error {
	if err := c.check(); err != nil {
		return err
	}
	f := c.copy()
	if f.Batch == nil {
		return nil
	}
	b := f.Batch
	if now.Before(b.StartedAt) {
		return errors.New("fixed recovery clock precedes capture")
	}
	changed := false
	if b.Phase == "capturing" {
		b.Phase, b.SealedAt, b.RecoveredAt = "sealed", now, now
		for i := range b.Signals {
			if b.Signals[i].Phase == "unobserved" {
				b.Signals[i].Phase, b.Signals[i].Reason, b.Signals[i].TransitionAt = "abstained", "capture_interrupted", now
			}
		}
		changed = true
	}
	if now.After(b.LatestAt) {
		for i := range b.Signals {
			if b.Signals[i].Phase == "ready" {
				b.Signals[i].Phase, b.Signals[i].TransitionAt = "expired", now
				changed = true
			}
		}
	}
	if changed {
		return c.save(f)
	}
	return nil
}

func (c *FixedController) readySignal(symbol string, now time.Time) (FixedSignal, int, error) {
	if err := c.check(); err != nil {
		return FixedSignal{}, -1, err
	}
	b := c.state.Fixed.Batch
	if b == nil || b.PolicySHA != c.state.Fixed.PolicySHA || b.Phase != "sealed" || now.Before(b.ExecutionAt) || now.After(b.LatestAt) || !TradingWindow(now) || !c.onlyRestingIntents() {
		return FixedSignal{}, -1, errors.New("fixed execution bucket is closed or submission is unresolved")
	}
	i := slices.IndexFunc(b.Signals, func(s FixedSignal) bool { return s.Symbol == symbol })
	if i < 0 || b.Signals[i].Phase != "ready" {
		return FixedSignal{}, -1, errors.New("fixed signal was not prepared or has already been consumed")
	}
	for _, prior := range b.Signals[:i] {
		if prior.Phase == "ready" {
			return FixedSignal{}, -1, errors.New("fixed signal cannot overtake an earlier roster entry")
		}
	}
	result := b.Signals[i]
	result.Contract = new(*result.Contract)
	return result, i, nil
}

// Claim grants one attempt, not permission to trade. The executor must prove fresh
// identity/books and recheck account, ownership, basis, costs, capacity, readiness
// and the durable ref_id even after a successful optional read preflight.
func (c *FixedController) Claim(symbol string, now time.Time) (FixedSignal, error) {
	result, i, err := c.readySignal(symbol, now)
	if err != nil {
		return FixedSignal{}, err
	}
	f := c.copy()
	f.Batch.Signals[i].Phase, f.Batch.Signals[i].TransitionAt = "claimed", now
	if err := c.save(f); err != nil {
		return FixedSignal{}, err
	}
	result.Phase, result.TransitionAt = "claimed", now
	return result, nil
}
