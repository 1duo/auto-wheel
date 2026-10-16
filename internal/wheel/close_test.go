package wheel

import (
	"context"
	"errors"
	"testing"
	"time"
)

func closeFixture() (Config, State, Snapshot, Candidate, time.Time) {
	c, st, s, q, now := standardFixture()
	q.Quantity, q.ChainID, q.Ask, q.Bid, q.Tick = 1, "chain", 2, 1, 1
	a := Candidate{Quote: q, Closing: true, CloseLimit: 2, EntryCredit: 2200}
	st.Positions[q.Symbol] = Position{PutID: q.ID, PutExpiry: q.Expiry, PutStrike: q.Strike, PutOpenedAt: now.Add(-time.Minute), Provenance: "own put"}
	st.CompletedOrders = map[string]OrderRecord{"opening": {Ref: "opening-ref", Symbol: q.Symbol, OptionID: q.ID, At: now.Add(-time.Minute)}}
	s.Options = []Option{q.Option}
	return c, st, s, a, now
}

type closeTestBroker struct {
	fakeBroker
	candidate    Candidate
	afterReview  func()
	beforeSubmit func()
}

func (b *closeTestBroker) CloseQuote(context.Context, State, string) (Candidate, error) {
	return b.candidate, nil
}
func (b *closeTestBroker) Review(context.Context, Candidate) error {
	if b.afterReview != nil {
		b.afterReview()
	}
	return nil
}
func (b *closeTestBroker) Submit(ctx context.Context, a Candidate, ref string) (string, error) {
	if b.beforeSubmit != nil {
		b.beforeSubmit()
	}
	return b.fakeBroker.Submit(ctx, a, ref)
}

func TestCloseChecks(t *testing.T) {
	for _, change := range []string{"valid", "above_entry_cap", "wrong_contract", "long_or_extra", "unowned", "stock", "cash", "stale", "price", "loss", "outside_order", "shared", "intent", "tick"} {
		t.Run(change, func(t *testing.T) {
			c, st, s, a, now := closeFixture()
			switch change {
			case "above_entry_cap":
				c.MaxCollateral = 100000
			case "wrong_contract":
				a.ID = "other"
			case "long_or_extra":
				s.Options[0].Quantity = 2
			case "unowned":
				delete(st.Positions, a.Symbol)
			case "stock":
				s.Shares = []Shares{{Symbol: a.Symbol, Quantity: 100}}
			case "cash":
				s.AvailableCash = 0
			case "stale":
				a.At = now.Add(-time.Minute)
			case "price":
				a.Ask = a.CloseLimit + 1
			case "loss":
				a.EntryCredit = 200
			case "outside_order":
				s.Orders = []Order{{ID: "manual", CreatedAt: now, Symbol: a.Symbol, OptionID: a.ID, Side: "buy", Effect: "close"}}
			case "shared":
				c.CoordinatorSocket = "/tmp/coordinator"
			case "intent":
				st.Intents[a.Symbol] = Intent{Ref: "pending", Candidate: a, At: now}
			case "tick":
				a.Tick = 5
			}
			err := CheckClose(c, st, s, a, now)
			if (err == nil) != (change == "valid" || change == "above_entry_cap") {
				t.Fatalf("check %s: %v", change, err)
			}
		})
	}
}

func TestCloseSubmissionFences(t *testing.T) {
	for _, change := range []string{"valid", "store_failure", "unknown", "not_sent", "cash_after_review", "manual_after_review", "price_above_approval"} {
		t.Run(change, func(t *testing.T) {
			c, st, s, a, now := closeFixture()
			b := &closeTestBroker{fakeBroker: fakeBroker{snapshot: s}, candidate: a}
			store := &fakeStore{}
			b.beforeSubmit = func() {
				if st.Intents[a.Symbol].Ref == "" || store.saves < 3 {
					t.Fatal("submission before durable intent")
				}
			}
			switch change {
			case "store_failure":
				store.failAt = 3
			case "unknown":
				b.submitErr = context.DeadlineExceeded
			case "not_sent":
				b.submitErr = ErrNotSubmitted
			case "cash_after_review":
				b.afterReview = func() { b.snapshot.AvailableCash = 0 }
			case "manual_after_review":
				b.afterReview = func() { b.snapshot.Orders = []Order{{ID: "manual", Symbol: a.Symbol, CreatedAt: now}} }
			case "price_above_approval":
				b.candidate.Ask = a.CloseLimit + 1
			}
			err := ExecuteClose(t.Context(), c, &st, store, b, a, func() time.Time { return now })
			if change == "valid" {
				if err != nil || b.submissions != 1 || st.Intents[a.Symbol].OrderID == "" {
					t.Fatalf("successful close: %v %+v", err, st)
				}
				return
			}
			if err == nil {
				t.Fatal("failure accepted")
			}
			if change == "unknown" {
				if st.Intents[a.Symbol].Ref == "" {
					t.Fatal("ambiguous intent erased")
				}
				_ = ExecuteClose(t.Context(), c, &st, store, b, a, func() time.Time { return now })
				if b.submissions != 1 {
					t.Fatal("ambiguous close retried")
				}
			} else if change == "not_sent" {
				if !errors.Is(err, ErrNotSubmitted) || len(st.Intents) != 0 {
					t.Fatal("known unsent outcome retained")
				}
			} else if b.submissions != 0 {
				t.Fatal("unsafe submission")
			}
		})
	}
}

func TestCloseFillReconciliation(t *testing.T) {
	for _, phase := range []string{"working", "filled_position_lags", "filled_flat", "cancelled", "assignment", "missing_order", "wrong_side"} {
		t.Run(phase, func(t *testing.T) {
			c, st, s, a, now := closeFixture()
			st.Intents[a.Symbol] = Intent{Ref: "close-ref", OrderID: "close-order", Candidate: a, At: now}
			o := Order{ID: "close-order", Ref: "close-ref", Symbol: a.Symbol, OptionID: a.ID, Quantity: 1, Filled: 1, State: "filled", Side: "buy", Effect: "close", CreatedAt: now}
			switch phase {
			case "working":
				o.State = "confirmed"
				o.Filled = 0
			case "filled_flat":
				s.Options = nil
			case "cancelled":
				o.State = "cancelled"
				o.Filled = 0
			case "assignment":
				s.Options = nil
				s.Shares = []Shares{{Symbol: a.Symbol, Quantity: 100, Available: 100, Cost: a.Strike}}
			case "wrong_side":
				o.Side = "sell"
			}
			if phase != "missing_order" {
				s.Orders = []Order{o}
			}
			err := ReconcileForConfig(c, &st, s, now)
			if phase == "assignment" || phase == "missing_order" || phase == "wrong_side" {
				if err == nil || len(st.Intents) != 1 {
					t.Fatalf("ambiguous close freed ownership: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if phase == "filled_flat" {
				if len(st.Positions) != 0 || len(st.Intents) != 0 || !st.CompletedOrders[o.ID].Closing {
					t.Fatal("filled close not retained")
				}
				if err = CheckOrderScope(c, st, s); err != nil {
					t.Fatal("closed receipt not accepted", err)
				}
			} else if phase == "cancelled" {
				if len(st.Positions) != 1 || len(st.Intents) != 0 || !st.CompletedOrders[o.ID].Closing {
					t.Fatal("cancelled close erased inventory")
				}
			} else if len(st.Intents) != 1 || len(st.Positions) != 1 {
				t.Fatal("slot freed before confirmed close")
			}
		})
	}
}
