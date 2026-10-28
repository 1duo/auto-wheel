package wheel

import (
	"context"
	"errors"
	"testing"
	"time"
)

type closeQuoteErrBroker struct {
	*restingTPBroker
	err error
}

func (b *closeQuoteErrBroker) CloseQuote(ctx context.Context, st State, symbol string) (Candidate, error) {
	if b.err != nil {
		return Candidate{}, b.err
	}
	return b.restingTPBroker.CloseQuote(ctx, st, symbol)
}

// Only failures before the single-use claim may let the cycle try another
// symbol; claimed, unknown and account-wide outcomes must stop it.
func TestRestingTPDeferralIsConfinedToPreClaimSymbolFailures(t *testing.T) {
	for _, scenario := range []string{"quote", "stale", "cash", "not_sent", "unknown", "unresolved"} {
		t.Run(scenario, func(t *testing.T) {
			c, config, rb := restingTPFixture(t, false)
			b := &closeQuoteErrBroker{restingTPBroker: rb}
			switch scenario {
			case "quote":
				b.err = errors.New("no offered close liquidity")
			case "stale":
				rb.quote.At = rb.now.Add(-time.Minute)
			case "cash":
				rb.afterReview = func() { rb.snapshot.AvailableCash = config.Reserve }
			case "not_sent":
				rb.submitErr = ErrNotSubmitted
			case "unknown":
				rb.submitErr = errors.New("timeout")
			case "unresolved":
				st := *rb.state
				st.Intents = map[string]Intent{"MSFT": {Ref: "lost", At: *rb.now, Candidate: rb.quote}}
				*rb.state = st
			}
			err := c.ExecuteRestingTakeProfit(t.Context(), config, b, "MSFT", func() time.Time { return *rb.now })
			wantDeferred := scenario == "quote" || scenario == "stale" || scenario == "cash"
			if err == nil || errors.Is(err, ErrRestingTPDeferred) != wantDeferred {
				t.Fatal("wrong deferral classification", err)
			}
			if wantDeferred && (rb.submits != 0 || len(c.PendingRestingTakeProfit()) != 1) {
				t.Fatal("deferred target consumed its claim", rb.submits)
			}
		})
	}
}

func TestRestingTargetGapsReportUntargetedPositions(t *testing.T) {
	c, config, b := restingTPFixture(t, false)
	if gaps := c.RestingTargetGaps(); gaps["MSFT"] != "pending" {
		t.Fatal("unplaced target not reported", gaps)
	}
	if err := c.ExecuteRestingTakeProfit(t.Context(), config, b, "MSFT", func() time.Time { return *b.now }); err != nil {
		t.Fatal(err)
	}
	if gaps := c.RestingTargetGaps(); len(gaps) != 0 {
		t.Fatal("working target reported as a gap", gaps)
	}
	o := workingRestingOrder(b.state.Intents["MSFT"])
	o.State = "cancelled"
	b.snapshot.Orders = append(b.snapshot.Orders, o)
	s, _ := b.Snapshot(t.Context())
	if err := c.ReconcileNative(config, s, *b.now); err != nil {
		t.Fatal(err)
	}
	if gaps := c.RestingTargetGaps(); gaps["MSFT"] != "no_working_target" {
		t.Fatal("broker-cancelled target left the position silently untargeted", gaps)
	}
}

// After a lost receipt, the broker's unique working order with this ref is
// bound so acknowledged-target admission resumes without an operator.
func TestReconcileBindsRefMatchedWorkingTarget(t *testing.T) {
	c, config, b := restingTPFixture(t, false)
	b.submitErr = errors.New("timeout")
	if err := c.ExecuteRestingTakeProfit(t.Context(), config, b, "MSFT", func() time.Time { return *b.now }); err == nil {
		t.Fatal("unknown outcome accepted")
	}
	in := b.state.Intents["MSFT"]
	if in.OrderID != "" {
		t.Fatal("unknown outcome invented a receipt")
	}
	s, _ := b.Snapshot(t.Context())
	if err := c.ReconcileNative(config, s, *b.now); err == nil {
		t.Fatal("missing broker order did not block")
	}
	in.OrderID = "MSFT-close"
	o := workingRestingOrder(in)
	b.snapshot.Orders = append(b.snapshot.Orders, o)
	s, _ = b.Snapshot(t.Context())
	if err := c.ReconcileNative(config, s, *b.now); err != nil {
		t.Fatal(err)
	}
	if b.state.Intents["MSFT"].OrderID != "MSFT-close" || c.CheckPendingOrders(s) != nil || b.submits != 1 {
		t.Fatal("ref-matched working target was not bound", b.state.Intents["MSFT"])
	}
	b.snapshot.Orders[len(b.snapshot.Orders)-1].Limit++
	s, _ = b.Snapshot(t.Context())
	if err := c.ReconcileNative(config, s, *b.now); err == nil {
		t.Fatal("bound target accepted a changed broker price")
	}
}

// A credit that cannot fund TP75 would create a position that never receives
// its target; it is deferred before any claim.
func TestASAPOpeningRequiresFundableRestingTarget(t *testing.T) {
	c, st, _, config, b, now := asapFixture(t)
	e := fixedQueueEvidence("MSFT", "put", *now)
	e.Option.Bid, e.Option.Ask, e.Tick = 8, 9, 1
	b.evidence.Market = e
	err := c.ExecuteASAP(t.Context(), config, b, e, func() time.Time { return *now })
	if !errors.Is(err, ErrASAPDeferred) || b.submits != 0 || len(st.Positions) != 0 {
		t.Fatal("unfundable target reached transport", err)
	}
	if _, claimed := st.Fixed.Replenishment.Attempts[asapKey("MSFT", *now)]; claimed {
		t.Fatal("deferred opening consumed its session claim")
	}
	e.Option.Bid, e.Option.Ask = 10, 11
	b.evidence.Market = e
	if err = c.ExecuteASAP(t.Context(), config, b, e, func() time.Time { return *now }); err != nil || b.submits != 1 {
		t.Fatal("fundable opening rejected", err)
	}
}
