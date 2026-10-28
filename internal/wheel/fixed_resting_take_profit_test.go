package wheel

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"
)

type restingTPBroker struct{ *tpBroker }

func (b *restingTPBroker) Review(_ context.Context, a Candidate) error {
	b.reviews++
	limit, err := FixedTP75Limit(a.EntryCredit, 100, a.Tick)
	if err != nil || !a.RestingTakeProfit || a.CloseLimit != limit || a.Ask <= a.CloseLimit {
		b.t.Fatal("target was not submitted while above the TP price", a, err)
	}
	if b.afterReview != nil {
		b.afterReview()
	}
	return nil
}

func (b *restingTPBroker) SubmitFixed(ctx context.Context, a Candidate, ref string, deadline time.Time, guard func() error) (string, error) {
	id, err := b.tpBroker.SubmitFixed(ctx, a, ref, deadline, guard)
	if id != "" {
		id = a.Symbol + "-close"
	}
	return id, err
}

func restingTPFixture(t *testing.T, call bool) (*FixedController, Config, *restingTPBroker) {
	t.Helper()
	_, config, b := tpFixture(t, call)
	config.FixedPolicy.Spec.ID = FixedConservativeRestingTP75Policy
	*b.now = time.Date(2026, 10, 21, 10, 0, 0, 0, Eastern)
	next, err := EnableFixedRestingTP75(*b.state, config, b.now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	*b.state = next
	if err = b.store.Save(next); err != nil {
		t.Fatal(err)
	}
	c, err := NewFixedController(config.FixedPolicy.Spec, b.state, b.store)
	if err != nil {
		t.Fatal(err)
	}
	b.quote.RestingTakeProfit = true
	b.quote.CloseLimit, err = FixedTP75Limit(b.quote.EntryCredit, config.Fee, b.quote.Tick)
	if err != nil {
		t.Fatal(err)
	}
	b.quote.At, b.quote.UnderlyingAt = *b.now, *b.now
	b.quote.Bid, b.quote.Ask = 90, 100
	return c, config, &restingTPBroker{b}
}

func workingRestingOrder(in Intent) Order {
	a := in.Candidate
	return Order{ID: in.OrderID, Ref: in.Ref, Symbol: a.Symbol, OptionID: a.ID, Kind: a.Kind, Strike: a.Strike,
		Quantity: 1, State: "confirmed", Side: "buy", Effect: "close", CreatedAt: in.At,
		Type: "limit", TimeInForce: "gtc", Limit: a.CloseLimit}
}

func TestRestingTP75ImmediateTargetRestartAndBrokerFill(t *testing.T) {
	for _, call := range []bool{false, true} {
		t.Run(map[bool]string{false: "put", true: "call"}[call], func(t *testing.T) {
			c, config, b := restingTPFixture(t, call)
			if err := c.ExecuteRestingTakeProfit(t.Context(), config, b, "MSFT", func() time.Time { return *b.now }); err != nil {
				t.Fatal("morning resting target blocked", err)
			}
			in := b.state.Intents["MSFT"]
			if b.submits != 1 || in.Candidate.CloseLimit != 23 || in.Candidate.TimeInForce() != "gtc" {
				t.Fatal("wrong initial target", in)
			}
			b.snapshot.Orders = append(b.snapshot.Orders, workingRestingOrder(in))
			c, b.state = restartFixedQueue(t, c, b.store)
			*b.now = b.now.Add(time.Hour)
			s, _ := b.Snapshot(t.Context())
			if err := c.ReconcileNative(config, s, *b.now); err != nil {
				t.Fatal("working target did not survive restart", err)
			}
			if !RestingTP75OrdersAcknowledged(c.spec.ID, b.state.Intents, s) || len(c.PendingRestingTakeProfit()) != 0 {
				t.Fatal("existing target was recreated or blocked readiness")
			}
			if c.ExecuteRestingTakeProfit(t.Context(), config, b, "MSFT", func() time.Time { return *b.now }) == nil || b.submits != 1 {
				t.Fatal("working GTC retried")
			}
			last := &b.snapshot.Orders[len(b.snapshot.Orders)-1]
			last.State, last.Filled = "filled", 1
			s, _ = b.Snapshot(t.Context())
			if err := c.ReconcileNative(config, s, *b.now); err != nil || len(b.state.Intents) != 1 {
				t.Fatal("fill freed lifecycle before position reporting", err)
			}
			b.snapshot.Options = nil
			s, _ = b.Snapshot(t.Context())
			if err := c.ReconcileNative(config, s, *b.now); err != nil || len(b.state.Intents) != 0 {
				t.Fatal("broker-confirmed fill not reconciled", err)
			}
			p, exists := b.state.Positions["MSFT"]
			if exists != call || call && (p.OwnedShares != 100 || p.CallID != "" || p.Cost != 9000) || len(c.PendingRestingTakeProfit()) != 0 {
				t.Fatal("TP fill changed stock ownership or reopened its lifecycle", p)
			}
		})
	}
}

func TestRestingTP75NoRetryAndSafetyGates(t *testing.T) {
	for _, scenario := range []string{"unknown", "empty_receipt", "not_sent", "cancelled", "rejected", "stale", "cash", "assignment", "stock_lost", "deadline", "save"} {
		t.Run(scenario, func(t *testing.T) {
			c, config, b := restingTPFixture(t, scenario == "stock_lost")
			switch scenario {
			case "unknown":
				b.submitErr = errors.New("timeout")
			case "empty_receipt":
				b.emptyID = true
			case "not_sent":
				b.submitErr = ErrNotSubmitted
			case "stale":
				b.quote.At = b.now.Add(-time.Minute)
			case "cash":
				b.afterReview = func() { b.snapshot.AvailableCash = config.Reserve }
			case "assignment":
				b.snapshot.Shares = []Shares{{Symbol: "MSFT", Quantity: 100, Available: 100, Cost: 9000}}
			case "stock_lost":
				b.snapshot.Shares = nil
			case "deadline":
				b.beforeTransport = func() { *b.now = b.now.Add(31 * time.Second) }
			case "save":
				c.store = fixedFailIntentStore{b.store}
			}
			err := c.ExecuteRestingTakeProfit(t.Context(), config, b, "MSFT", func() time.Time { return *b.now })
			terminal := scenario == "cancelled" || scenario == "rejected"
			if (err == nil) != terminal {
				t.Fatal("unexpected outcome", err)
			}
			if terminal {
				o := workingRestingOrder(b.state.Intents["MSFT"])
				o.State = scenario
				b.snapshot.Orders = append(b.snapshot.Orders, o)
				s, _ := b.Snapshot(t.Context())
				if err = c.ReconcileNative(config, s, *b.now); err != nil {
					t.Fatal(err)
				}
			}
			expectedSubmits := 0
			if terminal || scenario == "unknown" || scenario == "not_sent" || scenario == "empty_receipt" {
				expectedSubmits = 1
			}
			if b.submits != expectedSubmits {
				t.Fatal("unsafe transport", b.submits)
			}
			if terminal || scenario == "unknown" || scenario == "not_sent" || scenario == "empty_receipt" || scenario == "deadline" {
				c, b.state = restartFixedQueue(t, c, b.store)
				if len(c.PendingRestingTakeProfit()) != 0 || c.ExecuteRestingTakeProfit(t.Context(), config, b, "MSFT", func() time.Time { return *b.now }) == nil || b.submits != expectedSubmits {
					t.Fatal("restart retried a consumed lifecycle")
				}
			}
		})
	}
}

func TestRestingTP75OtherSymbolEntriesReserveBuybackCash(t *testing.T) {
	c, config, b := restingTPFixture(t, false)
	if err := c.ExecuteRestingTakeProfit(t.Context(), config, b, "MSFT", func() time.Time { return *b.now }); err != nil {
		t.Fatal(err)
	}
	b.snapshot.Orders = append(b.snapshot.Orders, workingRestingOrder(b.state.Intents["MSFT"]))
	s, _ := b.Snapshot(t.Context())
	e := FixedExecutionEvidence{Market: fixedQueueEvidence("AAPL", "put", *b.now), ChainID: "chain", Deliverable100: true}
	signal := FixedSignal{Symbol: "AAPL", Kind: "put", Contract: new(e.Market.Contract)}
	if _, err := c.openingCandidate(signal, e, s, *b.now, true); err != nil {
		t.Fatal("acknowledged target blocked an unrelated funded entry", err)
	}
	s.AvailableCash = config.Reserve + Cents(e.Market.Contract.Strike)*100 + config.Fee
	if _, err := c.openingCandidate(signal, e, s, *b.now, true); err == nil {
		t.Fatal("entry spent cash committed to its resting buyback")
	}
	for _, change := range []string{"missing", "price", "duration", "filled"} {
		t.Run(change, func(t *testing.T) {
			s, _ := b.Snapshot(t.Context())
			s.Orders = append([]Order(nil), s.Orders...)
			o := &s.Orders[len(s.Orders)-1]
			switch change {
			case "missing":
				s.Orders = s.Orders[:len(s.Orders)-1]
			case "price":
				o.Limit++
			case "duration":
				o.TimeInForce = "gfd"
			case "filled":
				o.State, o.Filled = "filled", 1
			}
			if _, err := c.openingCandidate(signal, e, s, *b.now, true); err == nil {
				t.Fatal("unproven target allowed another order")
			}
		})
	}
}

func TestRestingTP75MultipleTargetsShareOneCashReserve(t *testing.T) {
	c, config, b := restingTPFixture(t, false)
	// Add a second exactly imported lifecycle before any target is attempted.
	st := b.state
	p := st.Positions["MSFT"]
	p.PutID, p.Provenance = "aapl-put", "autowheel put fill aapl-ref"
	r := st.CompletedOrders["old-order"]
	r.Ref, r.Symbol, r.OptionID = "aapl-ref", "AAPL", p.PutID
	st.Positions["AAPL"], st.CompletedOrders["aapl-open"] = p, r
	source := st.Fixed.Legacy.Source
	source.Positions, source.CompletedOrders = maps.Clone(st.Positions), maps.Clone(st.CompletedOrders)
	st.Fixed.Legacy = &FixedLegacyImport{StateSHA: fixedDigest(source), Source: source}
	config.FixedPolicy.ImportLegacyStateSHA = st.Fixed.Legacy.StateSHA
	op := b.quote.Option
	op.Symbol, op.ID = "AAPL", p.PutID
	st.Fixed.Contracts["AAPL"] = op
	b.snapshot.Options = append(b.snapshot.Options, op)
	b.snapshot.Orders = append(b.snapshot.Orders, Order{ID: "aapl-open", Ref: r.Ref, Symbol: "AAPL", OptionID: op.ID,
		Quantity: 1, Filled: 1, State: "filled", Side: "sell", Effect: "open", CreatedAt: r.At})
	if err := b.store.Save(*st); err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return *b.now }
	if err := c.ExecuteRestingTakeProfit(t.Context(), config, b, "MSFT", clock); err != nil {
		t.Fatal(err)
	}
	b.snapshot.Orders = append(b.snapshot.Orders, workingRestingOrder(st.Intents["MSFT"]))
	b.quote.Option, b.quote.EntryOrderID = op, "aapl-open"
	b.snapshot.AvailableCash = config.Reserve + b.quote.CloseLimit*100 + config.Fee
	if err := c.ExecuteRestingTakeProfit(t.Context(), config, b, "AAPL", clock); err == nil || b.submits != 1 {
		t.Fatal("second target spent the first target's reserve")
	}
	b.snapshot.AvailableCash += b.quote.CloseLimit*100 + config.Fee
	if err := c.ExecuteRestingTakeProfit(t.Context(), config, b, "AAPL", clock); err != nil || b.submits != 2 || len(st.Intents) != 2 {
		t.Fatal("two funded targets could not coexist", err)
	}
}

func TestRestingTP75RegularSessionAndEarlyClose(t *testing.T) {
	for _, day := range []time.Time{time.Date(2026, 10, 20, 0, 0, 0, 0, Eastern), time.Date(2026, 11, 27, 0, 0, 0, 0, Eastern)} {
		open := day.Add(9*time.Hour + 30*time.Minute)
		close, _ := SessionClose(day)
		for _, at := range []time.Time{open, open.Add(time.Second), close.Add(-time.Nanosecond)} {
			if deadline, ok := FixedRestingCloseDeadline(at); !ok || !deadline.Equal(close) {
				t.Fatal("regular-session target unavailable", at)
			}
		}
		for _, at := range []time.Time{open.Add(-time.Nanosecond), close, close.Add(time.Second)} {
			if _, ok := FixedRestingCloseDeadline(at); ok {
				t.Fatal("out-of-session submission allowed", at)
			}
		}
	}
}

func TestRestingTP75ExplicitUnsentRearmRetainsAuditAndOneAttempt(t *testing.T) {
	c, config, b := restingTPFixture(t, false)
	b.submitErr = ErrNotSubmitted
	clock := func() time.Time { return *b.now }
	if err := c.ExecuteRestingTakeProfit(t.Context(), config, b, "MSFT", clock); !errors.Is(err, ErrNotSubmitted) {
		t.Fatal(err)
	}
	old := b.state.Fixed.RestingTakeProfit[b.quote.EntryOrderID]
	if len(c.PendingRestingTakeProfit()) != 0 || len(b.state.Intents) != 0 {
		t.Fatal("unsent claim retried itself or retained an unknown submission")
	}
	before := mustJSON(*b.state)
	pin, err := b.store.ArchiveFixedPolicyState(*b.state)
	if err != nil {
		t.Fatal(err)
	}
	next, err := RearmFixedUnsentTP75(*b.state, config, "MSFT", old.Ref, pin, b.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if before != mustJSON(*b.state) || next.Fixed.RestingUnsentRearms[old.Ref].Attempt != old || next.Fixed.RestingUnsentRearms[old.Ref].SourceSHA != pin {
		t.Fatal("operator rearm lost or mutated original evidence")
	}
	*b.state = next
	if err := b.store.Save(next); err != nil {
		t.Fatal(err)
	}
	c, b.state = restartFixedQueue(t, c, b.store)
	b.submitErr = nil
	if len(c.PendingRestingTakeProfit()) != 1 {
		t.Fatal("operator rearm did not release exactly one target")
	}
	if err := c.ExecuteRestingTakeProfit(t.Context(), config, b, "MSFT", clock); err != nil {
		t.Fatal(err)
	}
	if b.state.Intents["MSFT"].Ref == old.Ref || fixedDigest(b.state.Fixed.RestingUnsentRearms[old.Ref].Attempt) != fixedDigest(old) {
		t.Fatal("rearm reused the old ref or lost its audit")
	}
	if _, err := RearmFixedUnsentTP75(*b.state, config, "MSFT", old.Ref, pin, *b.now); err == nil {
		t.Fatal("operator rearm crossed a pending submission")
	}
}

func TestRestingTP75UnsentRearmRejectsUnknownAndReceiptedOrders(t *testing.T) {
	for _, change := range []string{"unknown", "receipted", "wrong_ref", "wrong_symbol", "changed_policy", "bad_source", "past_clock"} {
		t.Run(change, func(t *testing.T) {
			c, config, b := restingTPFixture(t, false)
			b.submitErr = ErrNotSubmitted
			if change == "unknown" {
				b.submitErr = errors.New("timeout")
			}
			_ = c.ExecuteRestingTakeProfit(t.Context(), config, b, "MSFT", func() time.Time { return *b.now })
			a := b.state.Fixed.RestingTakeProfit[b.quote.EntryOrderID]
			ref, symbol, pin, now := a.Ref, "MSFT", fixedDigest(*b.state), b.now.Add(time.Second)
			switch change {
			case "receipted":
				b.state.CompletedOrders["cancelled"] = OrderRecord{Ref: ref, Symbol: symbol, OptionID: b.quote.ID, At: *b.now, Closing: true}
			case "wrong_ref":
				ref = "other"
			case "wrong_symbol":
				symbol = "AAPL"
			case "changed_policy":
				config.FixedPolicy.Spec.ID = FixedConservativeTP75Policy
			case "bad_source":
				pin = "missing"
			case "past_clock":
				now = a.At.Add(-time.Second)
			}
			if _, err := RearmFixedUnsentTP75(*b.state, config, symbol, ref, pin, now); err == nil {
				t.Fatal("unsafe order rearm admitted", change)
			}
		})
	}
}

func TestRestingTP75TransitionPreservesNativeOwnershipAndEndedBatch(t *testing.T) {
	c, st, store, config, b, now := fixedExecutionFixture(t)
	// Start this native fixture directly under the original TP75 policy.
	c.spec.ID = FixedConservativeTP75Policy
	config.FixedPolicy.Spec = c.spec
	st.Fixed.Spec, st.Fixed.PolicySHA = c.spec, c.spec.Digest()
	st.Fixed.Batch.PolicySHA = st.Fixed.PolicySHA
	if err := store.Save(*st); err != nil {
		t.Fatal(err)
	}
	if err := c.ExecuteNative(t.Context(), config, b, "MSFT", func() time.Time { return *now }); err != nil {
		t.Fatal(err)
	}
	in := st.Intents["MSFT"]
	s := b.snapshot
	s.At, s.Options = *now, []Option{in.Candidate.Option}
	s.Orders = []Order{{ID: in.OrderID, Ref: in.Ref, Symbol: "MSFT", OptionID: in.Candidate.ID,
		Quantity: 1, Filled: 1, State: "filled", Side: "sell", Effect: "open", CreatedAt: in.At}}
	if err := c.ReconcileNative(config, s, *now); err != nil {
		t.Fatal(err)
	}
	before, bound := mustJSON(*st), st.Fixed.BoundAt
	config.FixedPolicy.Spec.ID = FixedConservativeRestingTP75Policy
	if _, err := EnableFixedRestingTP75(*st, config, *now); err == nil {
		t.Fatal("transition crossed an active entry cohort")
	}
	transitionAt := st.Fixed.Batch.LatestAt.Add(time.Minute)
	next, err := EnableFixedRestingTP75(*st, config, transitionAt)
	if err != nil {
		t.Fatal("ended native cohort could not migrate", err)
	}
	if before != mustJSON(*st) || !next.Fixed.BoundAt.Equal(bound) || next.Positions["MSFT"] != st.Positions["MSFT"] ||
		fixedDigest(next.CompletedOrders) != fixedDigest(st.CompletedOrders) || next.Fixed.Batch.PolicySHA != st.Fixed.PolicySHA {
		t.Fatal("transition rewrote native inventory or its original policy evidence")
	}
	*st = next
	if err := store.Save(next); err != nil {
		t.Fatal(err)
	}
	c, err = NewFixedController(config.FixedPolicy.Spec, st, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Claim("AAPL", transitionAt); err == nil {
		t.Fatal("old policy batch granted new execution authority")
	}
	nextSignal := now.AddDate(0, 0, 7).Add(-time.Minute)
	if err := c.Begin(config.FixedPolicy.Roster, nextSignal); err != nil || st.Fixed.Batch.PolicySHA != config.FixedPolicy.Spec.Digest() {
		t.Fatal("next cohort did not acquire the new policy binding", err)
	}
}
