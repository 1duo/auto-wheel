package wheel

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"
)

type asapTestBroker struct {
	*fixedExecutionBroker
	reviewErr error
}

func (b *asapTestBroker) Review(ctx context.Context, a Candidate) error {
	if err := b.fixedExecutionBroker.Review(ctx, a); err != nil {
		return err
	}
	return b.reviewErr
}

func (b *asapTestBroker) SubmitFixed(ctx context.Context, a Candidate, ref string, deadline time.Time, guard func() error) (string, error) {
	if b.beforeTransport != nil {
		b.beforeTransport()
	}
	if err := guard(); err != nil {
		return "", errors.Join(ErrNotSubmitted, err)
	}
	var durable State
	if err := json.Unmarshal(b.store.raw, &durable); err != nil {
		b.t.Fatal(err)
	}
	claim := durable.Fixed.Replenishment.Attempts[asapKey(a.Symbol, *b.now)]
	in := durable.Intents[a.Symbol]
	if claim.Ref != ref || in.Ref != ref || in.OrderID != "" || fixedDigest(a) != fixedDigest(in.Candidate) || !TradingWindow(*b.now) || !b.now.Before(deadline) {
		b.t.Fatal("ASAP transport preceded durable exact claim and reference")
	}
	if exp, ok := ctx.Deadline(); !ok || exp.After(a.At.Add(30*time.Second)) || exp.After(deadline) {
		b.t.Fatal("ASAP connection outlived freshness or native cutoff")
	}
	b.submits++
	if b.submitErr != nil {
		return "", b.submitErr
	}
	if b.emptyID {
		return "", nil
	}
	return a.Symbol + "-open", nil
}

func asapFixture(t *testing.T) (*FixedController, *State, *fixedMemoryJournal, Config, *asapTestBroker, *time.Time) {
	t.Helper()
	c, st, store, at, roster := fixedQueueFixture(t)
	roster.Symbols = []string{"MSFT", "AAPL", "AMD", "INTC", "ORCL", "BAC"}
	config := DefaultConfig()
	config.Account, config.Symbols = st.Account, roster.Symbols
	config.OrderCoveragePolicy, config.TradePulseStateDir = "exclusive_standard", "/peer"
	config.RetiredTradePulse, config.ExclusiveOrdersSince = "/peer/supervisor/cross-host-handoff-sealed.json", at.AddDate(0, 0, -30)
	config.Reserve = Cents(c.spec.Limits.Reserve)
	config.FixedPolicy = &FixedPolicyConfig{Spec: c.spec, Roster: roster}
	config.FixedPolicy.Spec.ID = FixedConservativeTP75Policy
	next, err := EnableFixedTP75(*st, config, at.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	config.FixedPolicy.Spec.ID = FixedConservativeRestingTP75Policy
	next, err = EnableFixedRestingTP75(next, config, at)
	if err != nil {
		t.Fatal(err)
	}
	now := new(time.Date(2026, 10, 20, 10, 0, 0, 0, Eastern))
	config.FixedPolicy.Spec.ID, config.FixedPolicy.Spec.Limits.Slots, config.MaxSymbols = FixedConservativeASAPPolicy, 5, 5
	next, err = EnableFixedASAP(next, config, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	*st = next
	if err = store.Save(next); err != nil {
		t.Fatal(err)
	}
	c, err = NewFixedController(config.FixedPolicy.Spec, st, store)
	if err != nil {
		t.Fatal(err)
	}
	b := &asapTestBroker{fixedExecutionBroker: &fixedExecutionBroker{t: t, store: store, now: now, snapshot: Snapshot{Account: st.Account, OrderCoveragePolicy: "exclusive_standard", AvailableCash: 8_404_717}, evidence: FixedExecutionEvidence{Market: fixedQueueEvidence("MSFT", "put", *now), ChainID: "chain", Deliverable100: true}}}
	return c, st, store, config, b, now
}

func asapFill(t *testing.T, c *FixedController, config Config, b *asapTestBroker, st *State, symbol string) {
	t.Helper()
	in := st.Intents[symbol]
	a := in.Candidate
	b.snapshot.Orders = append(b.snapshot.Orders, Order{ID: in.OrderID, Ref: in.Ref, Symbol: symbol, OptionID: a.ID, Kind: a.Kind, Strike: a.Strike, Quantity: 1, Filled: 1, State: "filled", Side: "sell", Effect: "open", CreatedAt: in.At})
	b.snapshot.Options = append(b.snapshot.Options, a.Option)
	b.snapshot.AvailableCash -= a.Collateral
	s, _ := b.Snapshot(t.Context())
	if err := c.ReconcileNative(config, s, *b.now); err != nil {
		t.Fatal("ASAP fill reconciliation", err)
	}
}

func TestASAPFundedFifthSlotAndSixthBlocked(t *testing.T) {
	c, st, _, config, b, now := asapFixture(t)
	for i, symbol := range config.Symbols {
		b.evidence.Market = fixedQueueEvidence(symbol, "put", *now)
		err := c.ExecuteASAP(t.Context(), config, b, b.evidence.Market, func() time.Time { return *now })
		if i == 5 {
			if !errors.Is(err, ErrASAPDeferred) || b.submits != 5 || len(st.Positions) != 5 || len(st.Intents) != 0 {
				t.Fatal("sixth slot reached transport", err)
			}
			break
		}
		if err != nil {
			t.Fatal("funded midweek slot rejected", i, err)
		}
		asapFill(t, c, config, b, st, symbol)
		*now = now.Add(time.Second)
	}
	if err := st.Validate(); err != nil {
		t.Fatal("five native lifecycles invalid", err)
	}
}

func TestASAPReadAdmissionDeferralsDoNotConsumeSubmission(t *testing.T) {
	for _, scenario := range []string{"snapshot", "refresh", "stale", "future", "deliverable", "cash", "liquidity", "review", "cash_after_review", "foreign_after_review", "stale_after_review", "cutoff_after_review"} {
		t.Run(scenario, func(t *testing.T) {
			c, st, _, config, b, now := asapFixture(t)
			original := b.evidence
			cash := b.snapshot.AvailableCash
			switch scenario {
			case "snapshot":
				b.snapshotErr = errors.New("read unavailable")
			case "refresh":
				b.refreshErr = errors.New("quote unavailable")
			case "stale":
				b.evidence.Market.AskAt = now.Add(-31 * time.Second)
			case "future":
				b.evidence.Market.OptionAt = now.Add(3 * time.Second)
			case "deliverable":
				b.evidence.Deliverable100 = false
			case "cash":
				b.snapshot.AvailableCash = config.Reserve
			case "liquidity":
				b.evidence.Market.BidSize = 0
			case "review":
				b.reviewErr = errors.New("broker spread alert")
			case "cash_after_review":
				b.afterReview = func() { b.snapshot.AvailableCash = config.Reserve }
			case "foreign_after_review":
				b.afterReview = func() {
					b.snapshot.Options = append(b.snapshot.Options, Option{ID: "foreign", Symbol: "AAPL", Kind: "put", Quantity: 1, Strike: 9000})
				}
			case "stale_after_review":
				b.afterReview = func() { *now = now.Add(31 * time.Second) }
			case "cutoff_after_review":
				b.afterReview = func() { *now = time.Date(2026, 10, 20, 15, 45, 0, 0, Eastern) }
			}
			if err := c.ExecuteASAP(t.Context(), config, b, original.Market, func() time.Time { return *now }); err == nil || b.submits != 0 || len(st.Intents) != 0 || len(st.Fixed.Replenishment.Attempts) != 0 {
				t.Fatal("pretransport deferral consumed or submitted an opportunity", err)
			}
			b.snapshotErr, b.refreshErr, b.reviewErr, b.afterReview = nil, nil, nil, nil
			b.snapshot.AvailableCash, b.snapshot.Options = cash, nil
			*now = time.Date(2026, 10, 20, 10, 2, 0, 0, Eastern)
			b.evidence = original
			b.evidence.Market = fixedQueueEvidence("MSFT", "put", *now)
			if err := c.ExecuteASAP(t.Context(), config, b, b.evidence.Market, func() time.Time { return *now }); err != nil || b.submits != 1 {
				t.Fatal("fresh eligible decision was lost after read-only deferral", err)
			}
		})
	}
}

func TestASAPClaimUnknownAndUnsentNeverRetry(t *testing.T) {
	for _, scenario := range []string{"accepted", "unknown", "empty", "not_sent", "save", "expires_before_transport"} {
		t.Run(scenario, func(t *testing.T) {
			c, st, store, config, b, now := asapFixture(t)
			switch scenario {
			case "unknown":
				b.submitErr = errors.New("unknown transport outcome")
			case "empty":
				b.emptyID = true
			case "not_sent":
				b.submitErr = ErrNotSubmitted
			case "save":
				b.afterReview = func() { store.failSave = true }
			case "expires_before_transport":
				b.beforeTransport = func() { *now = now.Add(time.Minute) }
			}
			err := c.ExecuteASAP(t.Context(), config, b, b.evidence.Market, func() time.Time { return *now })
			if (err == nil) != (scenario == "accepted") {
				t.Fatal("wrong outcome", err)
			}
			count := b.submits
			if scenario == "save" {
				if count != 0 {
					t.Fatal("save failure reached transport")
				}
				return
			}
			c, st = restartFixedQueue(t, c, store)
			b.beforeTransport = nil
			b.submitErr = nil
			b.emptyID = false
			b.evidence.Market = fixedQueueEvidence("MSFT", "put", *now)
			if c.ExecuteASAP(t.Context(), config, b, b.evidence.Market, func() time.Time { return *now }) == nil || b.submits != count {
				t.Fatal("restart retried a consumed attempt")
			}
			if len(st.Fixed.Replenishment.Attempts) != 1 {
				t.Fatal("durable claim lost")
			}
			if (scenario == "unknown" || scenario == "empty") && st.Intents["MSFT"].Ref == "" {
				t.Fatal("unknown submission lost durable reference")
			}
		})
	}
}

func TestASAPCapacityEventsThrottleAndRestart(t *testing.T) {
	c, _, store, config, b, now := asapFixture(t)
	s, _ := b.Snapshot(t.Context())
	for _, test := range []struct {
		seconds int
		change  bool
		want    bool
	}{{0, false, true}, {5, false, false}, {59, false, false}, {60, false, true}, {61, true, true}, {62, false, false}} {
		at := now.Add(time.Duration(test.seconds) * time.Second)
		s.At = at
		if test.change {
			s.AvailableCash++
		}
		got, err := c.BeginASAPScan(config, s, at)
		if err != nil || got != test.want {
			t.Fatal("capacity cooldown", test, got, err)
		}
		c, _ = restartFixedQueue(t, c, store)
	}
}

func TestASAPMigrationRetainsWorkingTargetsAndCashCommitment(t *testing.T) {
	c, config, b := restingTPFixture(t, false)
	if err := c.ExecuteRestingTakeProfit(t.Context(), config, b, "MSFT", func() time.Time { return *b.now }); err != nil {
		t.Fatal(err)
	}
	before := *b.state
	config.FixedPolicy.Spec.ID, config.FixedPolicy.Spec.Limits.Slots, config.MaxSymbols = FixedConservativeASAPPolicy, 5, 5
	next, err := EnableFixedASAP(before, config, *b.now)
	if err != nil {
		t.Fatal(err)
	}
	if fixedDigest(before.Intents) != fixedDigest(next.Intents) || fixedDigest(before.Positions) != fixedDigest(next.Positions) || fixedDigest(before.CompletedOrders) != fixedDigest(next.CompletedOrders) || fixedDigest(before.Fixed.RestingTakeProfit) != fixedDigest(next.Fixed.RestingTakeProfit) {
		t.Fatal("migration rewrote ownership or broker targets")
	}
	*b.state = next
	control, err := NewFixedController(config.FixedPolicy.Spec, b.state, b.store)
	if err != nil {
		t.Fatal(err)
	}
	b.snapshot.Orders = append(b.snapshot.Orders, workingRestingOrder(next.Intents["MSFT"]))
	s, _ := b.Snapshot(t.Context())
	committed, err := control.restingCloseCommitment(s)
	if err != nil || committed != 2400 {
		t.Fatal("working GTC debit/fee lost", committed, err)
	}
	bad := before
	bad.Intents = maps.Clone(before.Intents)
	in := bad.Intents["MSFT"]
	in.OrderID = ""
	bad.Intents["MSFT"] = in
	if _, err := EnableFixedASAP(bad, config, *b.now); err == nil {
		t.Fatal("unknown order crossed migration")
	}
	config.Reserve++
	config.FixedPolicy.Spec.Limits.Reserve++
	if _, err := EnableFixedASAP(before, config, *b.now); err == nil {
		t.Fatal("ASAP migration silently changed reserve")
	}
}

func TestASAPSameSessionCloseAndForeignInventoryStayExcluded(t *testing.T) {
	c, st, _, config, b, now := asapFixture(t)
	st.CompletedOrders = map[string]OrderRecord{"closed": {Closing: true, Filled: 1, Ref: "closed-ref", Symbol: "AAPL", OptionID: "old", At: now.Add(-time.Hour), ResolvedAt: now.Add(-time.Minute)}}
	s, _ := b.Snapshot(t.Context())
	if slices.Contains(c.ASAPSymbols(config, s, *now), "AAPL") {
		t.Fatal("closed symbol reentered same session")
	}
	b.snapshot.Shares = []Shares{{Symbol: "AAPL", Quantity: 100, Available: 100, Cost: 9000}}
	b.evidence.Market = fixedQueueEvidence("AAPL", "call", *now)
	if err := c.ExecuteASAP(t.Context(), config, b, b.evidence.Market, func() time.Time { return *now }); err == nil || b.submits != 0 {
		t.Fatal("external stock appropriated for call", err)
	}
}
