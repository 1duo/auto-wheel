package wheel

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"autowheel/internal/contractpolicy"
)

type fixedExecutionBroker struct {
	t               *testing.T
	store           *fixedMemoryJournal
	now             *time.Time
	snapshot        Snapshot
	evidence        FixedExecutionEvidence
	readCount       int
	reviews         int
	submits         int
	beforeRead      func(int)
	afterReview     func()
	submitErr       error
	emptyID         bool
	orderID         string
	beforeTransport func()
}

func (b *fixedExecutionBroker) Snapshot(context.Context) (Snapshot, error) {
	b.readCount++
	if b.beforeRead != nil {
		b.beforeRead(b.readCount)
	}
	s := b.snapshot
	s.At = *b.now
	return s, nil
}

func (b *fixedExecutionBroker) RefreshForFixed(_ context.Context, chosen contractpolicy.Contract, _ func() time.Time) (FixedExecutionEvidence, error) {
	if chosen.ID != b.evidence.Market.Contract.ID {
		return FixedExecutionEvidence{}, errors.New("wrong fixed identity")
	}
	return b.evidence, nil
}

func (b *fixedExecutionBroker) Review(_ context.Context, a Candidate) error {
	b.reviews++
	if a.ID != b.evidence.Market.Contract.ID || a.Closing || a.Quantity != 1 || a.Delta != 0 || a.OI != 0 {
		b.t.Fatal("fixed review changed contract or fabricated Greek/OI evidence")
	}
	if b.afterReview != nil {
		b.afterReview()
	}
	return nil
}

func (b *fixedExecutionBroker) SubmitFixed(ctx context.Context, a Candidate, ref string, deadline time.Time, guard func() error) (string, error) {
	if expires, known := ctx.Deadline(); !known || expires.After(deadline) || expires.After(a.At.Add(30*time.Second)) {
		b.t.Fatal("connection setup is not bounded by source freshness and the weekly deadline")
	}
	if b.beforeTransport != nil {
		b.beforeTransport()
	}
	if err := guard(); err != nil {
		return "", errors.Join(ErrNotSubmitted, err)
	}
	b.submits++
	var durable State
	if err := json.Unmarshal(b.store.raw, &durable); err != nil {
		b.t.Fatal(err)
	}
	in := durable.Intents[a.Symbol]
	if ref == "" || in.Ref != ref || in.OrderID != "" || fixedDigest(in.Candidate) != fixedDigest(a) || b.now.After(deadline) || durable.Fixed.Batch.Signals[0].Phase != "claimed" {
		b.t.Fatal("broker transport preceded exact durable intent or deadline")
	}
	if b.emptyID || b.submitErr != nil {
		return "", b.submitErr
	}
	if b.orderID != "" {
		return b.orderID, nil
	}
	return "accepted-order", nil
}

func fixedExecutionFixture(t *testing.T) (*FixedController, *State, *fixedMemoryJournal, Config, *fixedExecutionBroker, *time.Time) {
	t.Helper()
	c, st, store, signalAt, roster := fixedQueueFixture(t)
	if err := c.Begin(roster, signalAt); err != nil {
		t.Fatal(err)
	}
	for _, symbol := range roster.Symbols {
		if err := c.Record(symbol, fixedQueueEvidence(symbol, "put", signalAt), signalAt); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Seal(signalAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.Account, config.Symbols = st.Account, roster.Symbols
	config.OrderCoveragePolicy, config.TradePulseStateDir = "exclusive_standard", "/peer"
	config.RetiredTradePulse, config.ExclusiveOrdersSince = "/peer/supervisor/cross-host-handoff-sealed.json", signalAt.AddDate(0, 0, -30)
	config.Reserve = Cents(c.spec.Limits.Reserve)
	config.MaxQuoteAgeSeconds = c.spec.MaxQuoteAge
	config.FixedPolicy = &FixedPolicyConfig{Spec: c.spec, Roster: roster}
	now := new(signalAt.Add(time.Minute))
	b := &fixedExecutionBroker{t: t, store: store, now: now,
		snapshot: Snapshot{Account: st.Account, OrderCoveragePolicy: "exclusive_standard", AvailableCash: 8_404_717},
		evidence: FixedExecutionEvidence{Market: fixedQueueEvidence("MSFT", "put", *now), ChainID: "chain", Deliverable100: true}}
	return c, st, store, config, b, now
}

func TestFixedNativeExecutionDurabilityAndNoRetry(t *testing.T) {
	for _, scenario := range []string{"accepted", "unknown", "not_sent", "empty_receipt"} {
		t.Run(scenario, func(t *testing.T) {
			c, st, store, config, b, now := fixedExecutionFixture(t)
			switch scenario {
			case "unknown":
				b.submitErr = errors.New("transport timed out")
			case "not_sent":
				b.submitErr = ErrNotSubmitted
			case "empty_receipt":
				b.emptyID = true
			}
			// A changed midpoint would choose another strike if selection were
			// repeated. Execution must still use the original $90 put.
			b.evidence.Market.Stock = contractpolicy.Book{Bid: 11999, Ask: 12001}
			err := c.ExecuteNative(t.Context(), config, b, "MSFT", func() time.Time { return *now })
			if (err == nil) != (scenario == "accepted") || b.submits != 1 || b.reviews != 1 || b.readCount != 2 {
				t.Fatal("wrong one-attempt execution result", err, b.submits, b.reviews, b.readCount)
			}
			in, retained := st.Intents["MSFT"]
			if retained != (scenario != "not_sent") || scenario == "accepted" && (in.OrderID != "accepted-order" || in.Candidate.Strike != 9000) {
				t.Fatal("unknown outcome lost its fence, or known unsent order retained risk", st.Intents)
			}
			c, _ = restartFixedQueue(t, c, store)
			if c.ExecuteNative(t.Context(), config, b, "MSFT", func() time.Time { return *now }) == nil || b.submits != 1 {
				t.Fatal("restart replayed the broker submission")
			}
		})
	}
}

func TestFixedNativeExecutionFreshAccountAndSourceGates(t *testing.T) {
	for _, scenario := range []string{"stale_ask", "wrong_contract", "no_deliverable", "zero_bid", "fee_only", "wrong_tick", "cash_after_review", "outside_after_review", "deadline_after_review", "auth_expires_snapshot", "auth_exceeds_deadline"} {
		t.Run(scenario, func(t *testing.T) {
			c, st, _, config, b, now := fixedExecutionFixture(t)
			switch scenario {
			case "stale_ask":
				b.evidence.Market.AskAt = now.Add(-31 * time.Second)
			case "wrong_contract":
				b.evidence.Market.Contract.Strike++
			case "no_deliverable":
				b.evidence.Deliverable100 = false
			case "zero_bid":
				b.evidence.Market.Option.Bid = 0
			case "fee_only":
				b.evidence.Market.Option = contractpolicy.Book{Bid: 1, Ask: 1}
				b.evidence.Market.Tick = 1
			case "wrong_tick":
				b.evidence.Market.Tick = 3
			case "cash_after_review":
				b.afterReview = func() { b.snapshot.AvailableCash = c.spec.OptionFeeBudget }
			case "outside_after_review":
				b.afterReview = func() { b.snapshot.ExternalSymbols = []string{"HOOD"} }
			case "deadline_after_review":
				b.afterReview = func() { *now = now.Add(5*time.Minute + time.Nanosecond) }
			case "auth_expires_snapshot":
				b.beforeTransport = func() { *now = now.Add(31 * time.Second) }
			case "auth_exceeds_deadline":
				b.beforeTransport = func() { *now = now.Add(5*time.Minute + time.Nanosecond) }
			}
			if err := c.ExecuteNative(t.Context(), config, b, "MSFT", func() time.Time { return *now }); err == nil || b.submits != 0 || len(st.Intents) != 0 {
				t.Fatal("bad current data reached transport", err, b.submits, st.Intents)
			}
			if st.Fixed.Batch.Signals[0].Phase != "claimed" {
				t.Fatal("failed execution left a later quote opportunity")
			}
		})
	}
}

type fixedFailIntentStore struct{ *fixedMemoryJournal }

func (s fixedFailIntentStore) Save(st State) error {
	if len(st.Intents) != 0 {
		return errors.New("injected intent fsync failure")
	}
	return s.fixedMemoryJournal.Save(st)
}

func TestFixedNativeIntentSaveFailureFencesController(t *testing.T) {
	c, _, store, config, b, now := fixedExecutionFixture(t)
	c.store = fixedFailIntentStore{store}
	if err := c.ExecuteNative(t.Context(), config, b, "MSFT", func() time.Time { return *now }); err == nil || b.submits != 0 {
		t.Fatal("transport ran despite failed intent save", err)
	}
	if err := c.Recover(*now); err == nil {
		t.Fatal("uncertain journal save did not poison controller")
	}
}

func TestFixedNativeFillAndAssignmentPreserveExactOwnership(t *testing.T) {
	c, st, store, config, b, now := fixedExecutionFixture(t)
	if err := c.ExecuteNative(t.Context(), config, b, "MSFT", func() time.Time { return *now }); err != nil {
		t.Fatal(err)
	}
	in := st.Intents["MSFT"]
	s := b.snapshot
	s.At = *now
	s.Options = []Option{in.Candidate.Option}
	s.Orders = []Order{{ID: in.OrderID, Ref: in.Ref, Symbol: "MSFT", OptionID: in.Candidate.ID, Quantity: 1, Filled: 1, State: "filled", Side: "sell", Effect: "open", CreatedAt: in.At}}
	bad := s
	bad.Options = []Option{in.Candidate.Option}
	bad.Options[0].Strike++
	before := string(store.raw)
	if c.ReconcileNative(config, bad, *now) == nil || string(store.raw) != before || len(st.Intents) != 1 {
		t.Fatal("metadata drift partially consumed the confirmed intent")
	}
	if err := c.ReconcileNative(config, s, *now); err != nil || len(st.Intents) != 0 || st.Fixed.Contracts["MSFT"] != in.Candidate.Option {
		t.Fatal("own fill did not retain exact option identity", err)
	}
	c, st = restartFixedQueue(t, c, store)
	// A settled own assignment still waits through the existing reporting delay.
	*now = time.Date(2026, 11, 23, 15, 0, 0, 0, Eastern)
	s.At, s.Options = *now, nil
	s.Shares = []Shares{{Symbol: "MSFT", Quantity: 100, Available: 100, Cost: in.Candidate.Strike}}
	if err := c.ReconcileNative(config, s, *now); err != nil || st.Positions["MSFT"].OwnedShares != 0 {
		t.Fatal("settlement delay was bypassed", err)
	}
	*now = now.Add(24 * time.Hour)
	s.At = *now
	if err := c.ReconcileNative(config, s, *now); err != nil || st.Positions["MSFT"].OwnedShares != 100 || len(st.Fixed.Contracts) != 0 {
		t.Fatal("settled own assignment did not preserve provenance", err)
	}
	if _, _, err := fixedPositionBasis(c.spec, st.Fixed.PolicySHA, st.Positions["MSFT"]); err != nil {
		t.Fatal(err)
	}
	// The same own lot can enter its next weekly covered call. It cannot be
	// confused with a second put or a different strike when the fill reports.
	*now = time.Date(2026, 11, 30, 15, 29, 0, 0, Eastern)
	roster := st.Fixed.Batch.Roster
	if err := c.Begin(roster, *now); err != nil {
		t.Fatal(err)
	}
	call := fixedQueueEvidence("MSFT", "call", *now)
	call.Contract.Expiry = "2026-12-31"
	call.Menu = []contractpolicy.Contract{call.Contract}
	if err := c.Record("MSFT", call, *now); err != nil {
		t.Fatal(err)
	}
	if err := c.Abstain("AAPL", "source_unavailable", *now); err != nil {
		t.Fatal(err)
	}
	if err := c.Seal(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	call.BidAt, call.AskAt, call.OptionAt = *now, *now, *now
	b.evidence = FixedExecutionEvidence{Market: call, ChainID: "chain", Deliverable100: true}
	b.snapshot, b.orderID = s, "accepted-call"
	if err := c.ExecuteNative(t.Context(), config, b, "MSFT", func() time.Time { return *now }); err != nil {
		t.Fatal(err)
	}
	in = st.Intents["MSFT"]
	s.At, s.Options = *now, []Option{in.Candidate.Option}
	s.Orders = append(s.Orders, Order{ID: in.OrderID, Ref: in.Ref, Symbol: "MSFT", OptionID: in.Candidate.ID,
		Quantity: 1, Filled: 1, State: "filled", Side: "sell", Effect: "open", CreatedAt: in.At})
	if err := c.ReconcileNative(config, s, *now); err != nil || st.Fixed.Contracts["MSFT"].Strike != 10500 {
		t.Fatal("covered-call exact strike was not retained", err)
	}
	bad = s
	bad.Options = []Option{in.Candidate.Option}
	bad.Options[0].Strike++
	if c.ReconcileNative(config, bad, *now) == nil {
		t.Fatal("matching call ID bypassed exact strike verification")
	}
	*now = time.Date(2026, 12, 9, 15, 0, 0, 0, Eastern)
	s.At, s.Options, s.Shares = *now, nil, nil
	if err := c.ReconcileNative(config, s, *now); err != nil || st.Positions["MSFT"].OwnedShares != 100 {
		t.Fatal("call-away reporting bypassed the settlement delay", err)
	}
	*now = now.Add(24 * time.Hour)
	s.At = *now
	if err := c.ReconcileNative(config, s, *now); err != nil || len(st.Positions) != 0 || len(st.Fixed.Contracts) != 0 {
		t.Fatal("settled own call-away left stale inventory metadata", err)
	}
}
