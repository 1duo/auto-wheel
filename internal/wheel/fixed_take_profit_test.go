package wheel

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func (m *fixedMemoryJournal) ArchiveFixedPolicyState(s State) (string, error) {
	if m.failArchive {
		return "", errors.New("injected policy archive failure")
	}
	if string(m.raw) != mustJSON(s) {
		return "", errors.New("archive differs from durable state")
	}
	m.archives++
	return fixedDigest(s), nil
}

func mustJSON(s State) string { raw, _ := json.Marshal(s); return string(raw) }

type tpBroker struct {
	t               *testing.T
	state           *State
	store           *fixedMemoryJournal
	now             *time.Time
	snapshot        Snapshot
	quote           Candidate
	submits         int
	reviews         int
	submitErr       error
	emptyID         bool
	afterReview     func()
	beforeTransport func()
}

func (b *tpBroker) Snapshot(context.Context) (Snapshot, error) {
	s := b.snapshot
	s.At = *b.now
	return s, nil
}
func (b *tpBroker) CloseQuote(context.Context, State, string) (Candidate, error) { return b.quote, nil }
func (b *tpBroker) Review(_ context.Context, a Candidate) error {
	b.reviews++
	if !a.Closing || a.Option != b.quote.Option || b.state.Fixed.TakeProfit.Signals[0].Phase != "claimed" {
		b.t.Fatal("review preceded durable exact close claim")
	}
	if b.afterReview != nil {
		b.afterReview()
	}
	return nil
}
func (b *tpBroker) SubmitFixed(ctx context.Context, a Candidate, ref string, deadline time.Time, guard func() error) (string, error) {
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
	in := durable.Intents[a.Symbol]
	if in.Ref != ref || in.Ref == "" || in.OrderID != "" || fixedDigest(in.Candidate) != fixedDigest(a) {
		b.t.Fatal("close transport before durable ref/terms")
	}
	if d, ok := ctx.Deadline(); !ok || d.After(deadline) || d.After(a.At.Add(30*time.Second)) {
		b.t.Fatal("close transport lacks freshness deadline")
	}
	b.submits++
	if b.submitErr != nil || b.emptyID {
		return "", b.submitErr
	}
	return "close-order", nil
}

func tpFixture(t *testing.T, call bool) (*FixedController, Config, *tpBroker) {
	t.Helper()
	c, st, store, config, snapshot, at := legacyImportFixture(t, call, call)
	if call {
		p := st.Positions["MSFT"]
		p.CallOpenedAt = st.CompletedOrders["call-order"].At
		st.Positions["MSFT"] = p
		config.FixedPolicy.ImportLegacyStateSHA, _ = FixedLegacyDigest(*st)
	}
	if err := c.BindNative(config, snapshot, at); err != nil {
		t.Fatal(err)
	}
	old := mustJSON(*st)
	config.FixedPolicy.Spec.ID = FixedConservativeTP75Policy
	next, err := EnableFixedTP75(*st, config, at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if mustJSON(*st) != old || fixedDigest(next.Positions) != fixedDigest(st.Positions) || fixedDigest(next.CompletedOrders) != fixedDigest(st.CompletedOrders) || fixedDigest(next.Fixed.Legacy) != fixedDigest(st.Fixed.Legacy) {
		t.Fatal("transition changed existing ownership")
	}
	*st = next
	if err = store.Save(*st); err != nil {
		t.Fatal(err)
	}
	c, err = NewFixedController(config.FixedPolicy.Spec, st, store)
	if err != nil {
		t.Fatal(err)
	}
	signal := time.Date(2026, 10, 20, 15, 29, 0, 0, Eastern)
	a := Candidate{Closing: true, CloseLimit: 20, EntryCredit: 10000, EntryOrderID: "old-order", Quote: Quote{Option: snapshot.Options[0], At: signal, UnderlyingAt: signal, Spot: 10000, Bid: 19, Ask: 20, Tick: 1, AskSize: 1, Standard: true, Tradable: true}}
	if call {
		a.EntryOrderID = "call-order"
	}
	if err = c.BeginTakeProfit(signal); err != nil {
		t.Fatal(err)
	}
	if err = c.RecordTakeProfit("MSFT", a, nil, signal); err != nil {
		t.Fatal(err)
	}
	if err = c.RecoverTakeProfit(signal.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	now := new(signal.Add(time.Minute))
	a.At, a.UnderlyingAt = *now, *now
	return c, config, &tpBroker{t: t, state: st, store: store, now: now, snapshot: snapshot, quote: a}
}

func TestFixedTP75NetFeeAndTickBoundaries(t *testing.T) {
	for _, v := range []struct{ credit, fee, tick, want Cents }{
		{2200, 100, 1, 4}, {2000, 100, 1, 3}, {10000, 100, 5, 20}, {10000, 100, 1, 23}, {500, 100, 1, 0}, {MaxMoney, 100, 1, 249999998},
	} {
		got, err := FixedTP75Limit(v.credit, v.fee, v.tick)
		if got != v.want || (err == nil) != (v.want > 0) {
			t.Fatal(v, got, err)
		}
		if err == nil && (got*100+v.fee)*4 > v.credit-v.fee {
			t.Fatal("rounded limit captures less than 75% net")
		}
	}
	for _, v := range [][3]Cents{{0, 100, 1}, {MaxMoney + 1, 100, 1}, {10000, -1, 1}, {10000, 100, 0}, {10000, 100, MaxMoney}} {
		if _, err := FixedTP75Limit(v[0], v[1], v[2]); err == nil {
			t.Fatal("invalid money/tick accepted", v)
		}
	}
}

func TestFixedTP75BothKindsNoRetryAndBrokerConfirmedSettlement(t *testing.T) {
	for _, call := range []bool{false, true} {
		for _, outcome := range []string{"filled", "unknown", "not_sent", "empty_receipt"} {
			t.Run(map[bool]string{false: "put", true: "call"}[call]+"/"+outcome, func(t *testing.T) {
				c, config, b := tpFixture(t, call)
				if outcome == "unknown" {
					b.submitErr = errors.New("timeout")
				}
				if outcome == "not_sent" {
					b.submitErr = ErrNotSubmitted
				}
				b.emptyID = outcome == "empty_receipt"
				err := c.ExecuteTakeProfit(t.Context(), config, b, "MSFT", func() time.Time { return *b.now })
				if (err == nil) != (outcome == "filled") || b.submits != 1 || b.reviews != 1 {
					t.Fatal("wrong close outcome", err, b.submits)
				}
				if (len(b.state.Intents) == 1) != (outcome != "not_sent") {
					t.Fatal("lost unknown intent or retained known unsent intent")
				}
				c, b.state = restartFixedQueue(t, c, b.store)
				if c.ExecuteTakeProfit(t.Context(), config, b, "MSFT", func() time.Time { return *b.now }) == nil || b.submits != 1 {
					t.Fatal("restart retried daily close")
				}
				if outcome != "filled" {
					return
				}
				in := b.state.Intents["MSFT"]
				s := b.snapshot
				s.At = *b.now
				s.Orders = append(s.Orders, Order{ID: in.OrderID, Ref: in.Ref, Symbol: "MSFT", OptionID: in.Candidate.ID, Kind: in.Candidate.Kind, Strike: in.Candidate.Strike, Quantity: 1, Filled: 1, State: "filled", Side: "buy", Effect: "close", CreatedAt: in.At})
				if err = c.ReconcileNative(config, s, *b.now); err != nil || len(b.state.Intents) != 1 {
					t.Fatal("freed lifecycle before broker option disappeared", err)
				}
				s.Options = nil
				if err = c.ReconcileNative(config, s, *b.now); err != nil || len(b.state.Intents) != 0 {
					t.Fatal("confirmed buyback not reconciled", err)
				}
				p, held := b.state.Positions["MSFT"]
				if held != call || call && (p.OwnedShares != 100 || p.CallID != "" || p.Cost != 9000 || !p.CallOpenedAt.IsZero()) {
					t.Fatal("buyback changed stock ownership", p)
				}
				if !b.state.CompletedOrders["close-order"].ResolvedAt.Equal(*b.now) {
					t.Fatal("missing broker-confirmation reentry fence")
				}
				e := FixedExecutionEvidence{Market: fixedQueueEvidence("MSFT", "put", *b.now), ChainID: "chain", Deliverable100: true}
				signal := FixedSignal{Symbol: "MSFT", Kind: "put", Contract: new(e.Market.Contract)}
				if call {
					e.Market = fixedQueueEvidence("MSFT", "call", *b.now)
					signal.Kind, signal.Contract = "call", new(e.Market.Contract)
					signal.AcquisitionCeiling, signal.BasisSHA, err = fixedPositionBasis(c.spec, b.state.Fixed.PolicySHA, p)
					if err != nil {
						t.Fatal(err)
					}
				}
				if _, err = c.openingCandidate(signal, e, s, *b.now, true); err == nil {
					t.Fatal("same-day recycling allowed")
				}
				other := FixedExecutionEvidence{Market: fixedQueueEvidence("AAPL", "put", *b.now), ChainID: "chain", Deliverable100: true}
				if _, err = c.openingCandidate(FixedSignal{Symbol: "AAPL", Kind: "put", Contract: new(other.Market.Contract)}, other, s, *b.now, true); err != nil {
					t.Fatal("unrelated settled-cash entry acquired a global cooldown", err)
				}
				next := b.now.AddDate(0, 0, 1)
				s.At = next
				if call {
					s.Shares[0].Available = 100
				}
				e.Market.BidAt, e.Market.AskAt, e.Market.OptionAt = next, next, next
				if _, err = c.openingCandidate(signal, e, s, next, true); err != nil {
					t.Fatal("settled next-session entry blocked", err)
				}
			})
		}
	}
}

func TestFixedTP75RejectsCurrentDriftAndDurabilityFailure(t *testing.T) {
	for _, scenario := range []string{"source", "target", "cash", "assignment", "stock_lost", "deadline", "save"} {
		t.Run(scenario, func(t *testing.T) {
			c, config, b := tpFixture(t, scenario == "stock_lost")
			switch scenario {
			case "source":
				b.quote.At = b.now.Add(-31 * time.Second)
			case "target":
				b.quote.Ask, b.quote.CloseLimit = 24, 24
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
			if err := c.ExecuteTakeProfit(t.Context(), config, b, "MSFT", func() time.Time { return *b.now }); err == nil || b.submits != 0 {
				t.Fatal("unsafe close reached transport", err)
			}
			if scenario == "save" && c.RecoverTakeProfit(*b.now) == nil {
				t.Fatal("failed fsync did not poison controller")
			}
		})
	}
}

func TestFixedTP75MissedCaptureAndArchive(t *testing.T) {
	c, _, b := tpFixture(t, false)
	next := time.Date(2026, 10, 21, 15, 29, 0, 0, Eastern)
	if err := c.BeginTakeProfit(next); err != nil {
		t.Fatal(err)
	}
	c, b.state = restartFixedQueue(t, c, b.store)
	if err := c.RecoverTakeProfit(next.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if b.state.Fixed.TakeProfit.Signals[0].Reason != "capture_interrupted" || b.store.archives != 1 {
		t.Fatal("missed observation was not fenced/archived")
	}
	if c.RecordTakeProfit("MSFT", b.quote, nil, next.Add(2*time.Second)) == nil || c.BeginTakeProfit(next.Add(2*time.Second)) == nil {
		t.Fatal("restart recaptured later market")
	}
	b.store.failArchive = true
	if c.BeginTakeProfit(next.AddDate(0, 0, 1)) == nil {
		t.Fatal("overwrote unarchived evidence")
	}
}

func TestFixedTP75TransitionRejectsOtherChanges(t *testing.T) {
	for _, change := range []string{"fee", "capital", "import", "native_batch"} {
		c, st, _, config, s, now := legacyImportFixture(t, false, false)
		if err := c.BindNative(config, s, now); err != nil {
			t.Fatal(err)
		}
		config.FixedPolicy.Spec.ID = FixedConservativeTP75Policy
		switch change {
		case "fee":
			config.Fee++
			config.FixedPolicy.Spec.OptionFeeBudget++
		case "capital":
			config.MaxCollateral++
			config.FixedPolicy.Spec.Limits.Account++
		case "import":
			config.FixedPolicy.ImportLegacyStateSHA = ""
		case "native_batch":
			if err := c.Begin(config.FixedPolicy.Roster, now.AddDate(0, 0, 7).Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := EnableFixedTP75(*st, config, now.Add(time.Minute)); err == nil {
			t.Fatal("TP transition crossed another financial boundary", change)
		}
	}
}
