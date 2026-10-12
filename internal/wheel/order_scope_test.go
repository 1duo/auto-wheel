package wheel

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func standardFixture() (Config, State, Snapshot, Quote, time.Time) {
	c, st, s, q, now := fixture()
	c.OrderCoveragePolicy = "exclusive_standard"
	c.ExclusiveOrdersSince = now.Add(-time.Hour)
	c.TradePulseStateDir = "/private/retired-peer"
	c.RetiredTradePulse = filepath.Join(c.TradePulseStateDir, "supervisor", "cross-host-handoff-sealed.json")
	s.OrderCoveragePolicy = c.OrderCoveragePolicy
	return c, st, s, q, now
}

func TestExclusiveStandardScope(t *testing.T) {
	for _, name := range []string{"clean", "old_stock", "new_stock", "old_stock_new_fill", "missing_stock_time", "unknown_option_terminal", "unknown_option_working", "unknown_inventory", "fractional", "duplicate", "full_policy", "no_snapshot_policy", "no_retirement", "shared_writer"} {
		t.Run(name, func(t *testing.T) {
			c, st, s, _, now := standardFixture()
			stock := Order{ID: "stock", Symbol: "AAPL", State: "filled", CreatedAt: now.Add(-2 * time.Hour), LastTransactionAt: now.Add(-2 * time.Hour)}
			switch name {
			case "old_stock":
				s.Orders = []Order{stock}
			case "new_stock":
				stock.CreatedAt = now
				s.Orders = []Order{stock}
			case "old_stock_new_fill":
				stock.LastTransactionAt = now
				s.Orders = []Order{stock}
			case "missing_stock_time":
				stock.LastTransactionAt = time.Time{}
				s.Orders = []Order{stock}
			case "unknown_option_terminal", "unknown_option_working":
				stock.OptionID = "external-option"
				if name == "unknown_option_working" {
					stock.State = "queued"
				}
				s.Orders = []Order{stock}
			case "unknown_inventory":
				s.Shares = []Shares{{Symbol: "AAPL", Quantity: 100, Available: 100, Cost: 20000}}
			case "fractional":
				s.ExternalSymbols = []string{"MU"}
			case "duplicate":
				s.Orders = []Order{stock, stock}
			case "full_policy":
				c.OrderCoveragePolicy = ""
			case "no_snapshot_policy":
				s.OrderCoveragePolicy = ""
			case "no_retirement":
				c.RetiredTradePulse = ""
			case "shared_writer":
				c.CoordinatorSocket = "/tmp/coordinator.sock"
			}
			err := ReconcileForConfig(c, &st, s, now)
			allowed := name == "clean" || name == "old_stock"
			if (err == nil) != allowed {
				t.Fatal("scope result", name, err)
			}
			if len(st.Positions) != 0 || len(st.Intents) != 0 {
				t.Fatal("outside activity mutated ownership")
			}
		})
	}
}

func TestExclusiveStandardCompletedOwnership(t *testing.T) {
	c, st, s, q, now := standardFixture()
	in := Intent{Ref: "own-ref", OrderID: "own-order", Candidate: Candidate{Quote: q}, At: now}
	st.Intents[q.Symbol] = in
	s.Orders = []Order{{ID: in.OrderID, Ref: in.Ref, Symbol: q.Symbol, OptionID: q.ID, Quantity: 1, Filled: 1, Side: "sell", Effect: "open", State: "filled", CreatedAt: now}}
	s.Options = []Option{q.Option}
	s.Options[0].Quantity = 1
	if err := ReconcileForConfig(c, &st, s, now); err != nil {
		t.Fatal(err)
	}
	if len(st.Intents) != 0 || st.CompletedOrders[in.OrderID].Ref != in.Ref {
		t.Fatal("completed ownership not retained")
	}
	delete(st.Positions, q.Symbol) // The receipt must outlive a closed lifecycle.
	s.Options = nil
	if err := CheckOrderScope(c, st, s); err != nil {
		t.Fatal("own historical fill rejected", err)
	}
	for _, change := range []string{"contract", "ref", "quantity", "side", "working"} {
		o := s.Orders[0]
		switch change {
		case "contract":
			o.OptionID = "other"
		case "ref":
			o.Ref = "other"
		case "quantity":
			o.Quantity = 2
		case "side":
			o.Side = "buy"
		case "working":
			o.State = "confirmed"
		}
		bad := s
		bad.Orders = []Order{o}
		if CheckOrderScope(c, st, bad) == nil {
			t.Fatal("modified receipt accepted", change)
		}
	}
	if err := st.Validate(); err != nil {
		t.Fatal(err)
	}
}

type changingReviewBroker struct {
	fakeBroker
	afterReview func(*changingReviewBroker)
}

func (b *changingReviewBroker) Review(context.Context, Candidate) error { b.afterReview(b); return nil }

func TestExclusiveStandardRechecksAfterReview(t *testing.T) {
	for _, change := range []string{"none", "cash", "outside_order", "unknown_outcome"} {
		t.Run(change, func(t *testing.T) {
			c, st, s, q, now := standardFixture()
			a, _ := Select(c, q.Symbol, q.Kind, []Quote{q}, 0, now)
			b := &changingReviewBroker{fakeBroker: fakeBroker{snapshot: s, quotes: []Quote{q}}}
			b.afterReview = func(b *changingReviewBroker) {
				switch change {
				case "cash":
					b.snapshot.AvailableCash = 0
				case "outside_order":
					b.snapshot.Orders = []Order{{ID: "manual", Symbol: "AAPL", State: "filled", CreatedAt: now, LastTransactionAt: now}}
				case "unknown_outcome":
					b.submitErr = context.DeadlineExceeded
				}
			}
			err := Execute(t.Context(), c, &st, &fakeStore{}, b, a, func() time.Time { return now })
			if change == "none" || change == "unknown_outcome" {
				if b.submissions != 1 || len(st.Intents) != 1 {
					t.Fatal("durable submission lost", err)
				}
				if change == "unknown_outcome" {
					_ = Execute(t.Context(), c, &st, &fakeStore{}, b, a, func() time.Time { return now })
					if b.submissions != 1 {
						t.Fatal("ambiguous submission retried")
					}
				} else if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || b.submissions != 0 || len(st.Intents) != 0 {
				t.Fatal("review race permitted submission", err)
			}
		})
	}
}
