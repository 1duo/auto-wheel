package wheel

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"autowheel/internal/contractpolicy"
)

// FixedExecutionEvidence is refreshed source/deliverable evidence. It never
// selects a new contract; only the already claimed signal can become an order.
type FixedExecutionEvidence struct {
	Market         FixedSignalEvidence
	ChainID        string
	Deliverable100 bool
}

type FixedBroker interface {
	Snapshot(context.Context) (Snapshot, error)
	RefreshForFixed(context.Context, contractpolicy.Contract, func() time.Time) (FixedExecutionEvidence, error)
	Review(context.Context, Candidate) error
	SubmitFixed(context.Context, Candidate, string, time.Time, func() error) (string, error)
}

// BindNative checks the inherited exclusive-order ledger before changing the
// journal's policy. A flat account with unexplained activity is not a handover.
func (c *FixedController) BindNative(config Config, s Snapshot, now time.Time) error {
	if err := config.Validate(); err != nil {
		return err
	}
	if config.FixedPolicy == nil || config.FixedPolicy.Spec.Digest() != c.spec.Digest() {
		return errors.New("fixed native binding differs from configured policy")
	}
	if err := checkOrderScope(config, *c.state, s); err != nil {
		return err
	}
	return c.Bind(s, now)
}

func (c *FixedController) nativeConfig(config Config) error {
	if err := c.check(); err != nil {
		return err
	}
	if err := config.Validate(); err != nil {
		return err
	}
	if config.Account != c.spec.Account || config.OrderCoveragePolicy != "exclusive_standard" ||
		config.CoordinatorSocket != "" || config.PriorityProfitClose != nil || config.MaxSymbols != 3 ||
		config.SymbolLimitScope != "managed" || config.MaxCollateral != Cents(c.spec.Limits.Account) ||
		config.Reserve != Cents(c.spec.Limits.Reserve) || config.Fee != c.spec.OptionFeeBudget ||
		config.MaxQuoteAgeSeconds != c.spec.MaxQuoteAge {
		return errors.New("fixed native account, roster, cash limits or fee binding differs")
	}
	if config.FixedPolicy == nil || config.FixedPolicy.Spec.Digest() != c.spec.Digest() {
		return errors.New("fixed native configuration has no matching explicit policy")
	}
	return nil
}

// ReconcileNative retains the existing exclusive-account assignment inference
// and settlement delay, with exact fixed-policy option metadata. It saves only
// a fully validated copy; ambiguous reporting cannot partially consume intents.
func (c *FixedController) ReconcileNative(config Config, s Snapshot, now time.Time) error {
	if err := c.nativeConfig(config); err != nil {
		return err
	}
	if s.Account != c.spec.Account || !fresh(s.At, now, c.spec.MaxQuoteAge) || c.spec.FeeKnownAt.After(now) || s.AvailableCash < 0 || s.AvailableCash > MaxMoney {
		return errors.New("fixed reconciliation account, freshness or cash is unknown")
	}
	if err := checkOrderScope(config, *c.state, s); err != nil {
		return err
	}
	shares := map[string]bool{}
	stockHeld := map[string]bool{}
	for _, h := range s.Shares {
		if !fixedSymbol(h.Symbol) || shares[h.Symbol] || h.Quantity != 0 && h.Quantity != 100 ||
			h.Available < 0 || h.Available > h.Quantity || h.Quantity == 100 && (h.Cost <= 0 || h.Cost > MaxMoney/100) {
			return errors.New("fixed reconciliation stock identity or quantity is ambiguous")
		}
		shares[h.Symbol] = true
		stockHeld[h.Symbol] = h.Quantity != 0
	}
	for _, op := range s.Options {
		want, found := c.state.Fixed.Contracts[op.Symbol]
		if in, pending := c.state.Intents[op.Symbol]; pending && in.Candidate.ID == op.ID {
			want, found = in.Candidate.Option, true
		}
		if !found || op != want {
			return errors.New("fixed broker option differs from its owned exact contract")
		}
		if op.Kind == "put" && stockHeld[op.Symbol] {
			return errors.New("fixed put and assignment inventory overlap; wait for settled broker reporting")
		}
	}
	for _, o := range s.Orders {
		in, pending := c.state.Intents[o.Symbol]
		if pending && !o.Terminal() && (in.OrderID != "" && in.OrderID == o.ID || in.OrderID == "" && in.Ref == o.Ref) {
			q := in.Candidate.Option
			if o.Kind != q.Kind || o.Strike != q.Strike {
				return errors.New("fixed broker order contract changed during submission")
			}
		}
	}
	next := *c.state
	next.Positions, next.Intents, next.CompletedOrders = maps.Clone(next.Positions), maps.Clone(next.Intents), maps.Clone(next.CompletedOrders)
	f := c.copy()
	next.Fixed = &f
	if err := Reconcile(&next, s, now); err != nil {
		return err
	}
	f.Contracts = map[string]Option{}
	for symbol, p := range next.Positions {
		id := p.PutID + p.CallID
		if id == "" {
			continue
		}
		op := c.state.Fixed.Contracts[symbol]
		if in, had := c.state.Intents[symbol]; had && in.Candidate.ID == id {
			op = in.Candidate.Option
		}
		f.Contracts[symbol] = op
	}
	return c.saveState(next)
}

func (c *FixedController) executionCandidate(signal FixedSignal, e FixedExecutionEvidence, now time.Time) (Candidate, error) {
	q := e.Market
	if signal.Contract == nil || q.Contract != *signal.Contract || e.ChainID == "" || !e.Deliverable100 ||
		!contractpolicy.EntryBookUsable(q.Stock) || !contractpolicy.EntryBookUsable(q.Option) ||
		q.Tick < 1 || q.Option.Bid%int64(q.Tick) != 0 || q.BidSize < 1 ||
		q.Option.Bid*100 <= int64(c.spec.OptionFeeBudget) ||
		!fresh(q.BidAt, now, c.spec.MaxQuoteAge) || !fresh(q.AskAt, now, c.spec.MaxQuoteAge) || !fresh(q.OptionAt, now, c.spec.MaxQuoteAge) {
		return Candidate{}, errors.New("fixed execution identity, books, clocks or deliverable is unproven")
	}
	underlyingAt := q.BidAt
	if q.AskAt.Before(underlyingAt) {
		underlyingAt = q.AskAt
	}
	a := Candidate{Quote: Quote{Option: Option{ID: q.Contract.ID, Symbol: q.Contract.Symbol, Kind: q.Contract.Kind,
		Expiry: q.Contract.Expiry, Strike: Cents(q.Contract.Strike), ChainID: e.ChainID, Quantity: 1},
		At: q.OptionAt, UnderlyingAt: underlyingAt, Spot: Cents((q.Stock.Bid + q.Stock.Ask + 1) / 2),
		Bid: Cents(q.Option.Bid), Ask: Cents(q.Option.Ask), Tick: q.Tick, BidSize: q.BidSize, Standard: true, Tradable: true}}
	a.NetPremium, a.Collateral = a.Bid*100-c.spec.OptionFeeBudget, a.Strike*100
	// Delta/OI and option-ROI ranking are intentionally not inputs to this path.
	return a, nil
}

func (c *FixedController) canExecute(config Config, signal FixedSignal, e FixedExecutionEvidence, s Snapshot, now time.Time) (Candidate, error) {
	if err := c.nativeConfig(config); err != nil {
		return Candidate{}, err
	}
	b := c.state.Fixed.Batch
	i := -1
	if b != nil {
		i = slices.IndexFunc(b.Signals, func(v FixedSignal) bool { return v.Symbol == signal.Symbol })
	}
	if i < 0 || b.Signals[i].Phase != "claimed" || fixedSignalDigest(b.Signals[i]) != signal.DataSHA ||
		fixedDigest(b.Roster) != fixedDigest(config.FixedPolicy.Roster) ||
		now.Before(b.ExecutionAt) || now.After(b.LatestAt) || !TradingWindow(now) ||
		len(c.state.Intents) != 0 || !fresh(s.At, now, c.spec.MaxQuoteAge) {
		return Candidate{}, errors.New("fixed execution has no current single-use claim or fresh account")
	}
	return c.openingCandidate(signal, e, s, now)
}

func (c *FixedController) openingCandidate(signal FixedSignal, e FixedExecutionEvidence, s Snapshot, now time.Time) (Candidate, error) {
	if len(c.state.Intents) != 0 || s.Account != c.spec.Account || !fresh(s.At, now, c.spec.MaxQuoteAge) {
		return Candidate{}, errors.New("fixed opening lacks a reconciled fresh account")
	}
	a, err := c.executionCandidate(signal, e, now)
	if err != nil {
		return Candidate{}, err
	}
	u := contractpolicy.CapitalUse{Available: int64(s.AvailableCash), Debit: int64(c.spec.OptionFeeBudget), ActiveSymbols: len(c.state.Positions)}
	for symbol, p := range c.state.Positions {
		if !p.MissingSince.IsZero() {
			return Candidate{}, errors.New("fixed opening waits for unresolved settlement")
		}
		exposure := p.PutStrike * 100
		if p.OwnedShares == 100 {
			if p.Cost*100 > MaxMoney-c.spec.PhysicalFeeBudget {
				return Candidate{}, errors.New("fixed stock acquisition ceiling overflow")
			}
			exposure = p.Cost*100 + c.spec.PhysicalFeeBudget
		}
		if exposure < 0 || int64(exposure) > int64(MaxMoney)-u.AccountExposure {
			return Candidate{}, errors.New("fixed account exposure overflow")
		}
		u.AccountExposure += int64(exposure)
		if symbol == a.Symbol {
			u.SymbolExposure = int64(exposure)
		}
	}
	p, managed := c.state.Positions[a.Symbol]
	if a.Kind == "put" {
		if managed {
			return Candidate{}, errors.New("fixed put cannot replace a managed lifecycle")
		}
		u.NewCollateral, u.Addition, u.NewSymbol = int64(a.Collateral), int64(a.Collateral), true
	} else {
		ceiling, basis, err := fixedPositionBasis(c.spec, c.state.Fixed.PolicySHA, p)
		allowed, floorErr := contractpolicy.StockCostCallAllowed(int64(a.Strike), int64(ceiling), int64(c.spec.PhysicalFeeBudget))
		if err != nil || floorErr != nil || !allowed || ceiling != signal.AcquisitionCeiling || basis != signal.BasisSHA {
			return Candidate{}, errors.New("fixed call basis changed or its stock-cost floor failed")
		}
		owned := false
		for _, h := range s.Shares {
			if h.Symbol == a.Symbol && h.Quantity == 100 && h.Available == 100 && h.Cost == p.Cost {
				owned = true
			}
		}
		if !owned {
			return Candidate{}, errors.New("fixed call lacks its own currently available shares")
		}
	}
	if !contractpolicy.CanOpen(c.spec.Limits, u) {
		return Candidate{}, errors.New("fixed fee, reserve, slot or exposure limit blocks opening")
	}
	return a, nil
}

// PreviewNative is non-trading integration evidence. It neither captures nor
// claims a weekly signal; ExecuteNative accepts only a symbol and consumes its
// independent durable signal, so this preview cannot be submitted as a draft.
func (c *FixedController) PreviewNative(config Config, e FixedExecutionEvidence, s Snapshot, now time.Time) (Candidate, error) {
	if err := c.ReconcileNative(config, s, now); err != nil {
		return Candidate{}, err
	}
	q := e.Market.Contract
	signal := FixedSignal{Symbol: q.Symbol, Kind: q.Kind, Contract: new(q)}
	if q.Kind == "call" {
		var err error
		signal.AcquisitionCeiling, signal.BasisSHA, err = fixedPositionBasis(c.spec, c.state.Fixed.PolicySHA, c.state.Positions[q.Symbol])
		if err != nil {
			return Candidate{}, err
		}
	}
	return c.openingCandidate(signal, e, s, now)
}

// ExecuteNative consumes the signal BEFORE reads or reviews. Any rejection
// ends this signal's opportunity. No error path automatically retries transport.
func (c *FixedController) ExecuteNative(ctx context.Context, config Config, b FixedBroker, symbol string, clock func() time.Time) error {
	if b == nil || clock == nil {
		return errors.New("fixed execution needs broker and clock")
	}
	if err := c.nativeConfig(config); err != nil {
		return err
	}
	signal, err := c.Claim(symbol, clock())
	if err != nil {
		return err
	}
	s, err := b.Snapshot(ctx)
	if err != nil {
		return err
	}
	if err = c.ReconcileNative(config, s, clock()); err != nil {
		return err
	}
	e, err := b.RefreshForFixed(ctx, *signal.Contract, clock)
	if err != nil {
		return err
	}
	a, err := c.canExecute(config, signal, e, s, clock())
	if err != nil {
		return err
	}
	if err = b.Review(ctx, a); err != nil {
		return err
	}
	// Review does not reserve cash or shares. Reconcile all account facts again.
	s, err = b.Snapshot(ctx)
	if err != nil {
		return err
	}
	if err = c.ReconcileNative(config, s, clock()); err != nil {
		return err
	}
	a, err = c.canExecute(config, signal, e, s, clock())
	if err != nil {
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
	deadline := c.state.Fixed.Batch.LatestAt
	guard := func() error {
		at := clock()
		if err := c.check(); err != nil {
			return err
		}
		if ctx.Err() != nil || at.Before(c.state.Fixed.Batch.ExecutionAt) || at.After(deadline) || !TradingWindow(at) ||
			c.state.Intents[symbol].Ref != ref || !fresh(a.At, at, c.spec.MaxQuoteAge) ||
			!fresh(e.Market.BidAt, at, c.spec.MaxQuoteAge) || !fresh(e.Market.AskAt, at, c.spec.MaxQuoteAge) || !fresh(s.At, at, c.spec.MaxQuoteAge) {
			return ErrNotSubmitted
		}
		return nil
	}
	if err := guard(); err != nil {
		return clearUnsent(errors.Join(ErrNotSubmitted, err))
	}
	// Also bound connection setup/write time, not just the check before Do.
	// Once transport is invoked, any timeout remains an unknown order outcome.
	transportDeadline := deadline
	for _, at := range []time.Time{a.At, e.Market.BidAt, e.Market.AskAt, s.At} {
		if expires := at.Add(time.Duration(c.spec.MaxQuoteAge) * time.Second); expires.Before(transportDeadline) {
			transportDeadline = expires
		}
	}
	transportCtx, cancel := context.WithDeadline(ctx, transportDeadline)
	defer cancel()
	id, err := b.SubmitFixed(transportCtx, a, ref, deadline, guard)
	if err != nil {
		if errors.Is(err, ErrNotSubmitted) {
			return clearUnsent(err)
		}
		return errors.New("fixed submission outcome unknown; durable intent retained, no retry")
	}
	if id == "" {
		return errors.New("fixed receipt identity missing; durable intent retained, no retry")
	}
	next = *c.state
	next.Intents = maps.Clone(next.Intents)
	in := next.Intents[symbol]
	in.OrderID = id
	next.Intents[symbol] = in
	return c.saveState(next)
}
