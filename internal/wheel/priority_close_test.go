package wheel

import (
	"context"
	"testing"
	"time"
)

func TestPriorityProfitCloseSequence(t *testing.T) {
	c, st, s, a, now := closeFixture()
	c.MaxCollateral = 0
	c.PriorityProfitClose = &ProfitCloseTarget{Symbol: a.Symbol, OptionID: a.ID, OpenedAt: st.Positions[a.Symbol].PutOpenedAt, MaxLimit: 3}
	b := &closeTestBroker{fakeBroker: fakeBroker{snapshot: s}, candidate: a}
	clock := func() time.Time { return now }
	store := &fakeStore{}
	if handled, preview, err := RunPriorityClose(t.Context(), c, &st, store, b, s, false, clock); err != nil || !handled || preview == nil || preview.CloseLimit != 3 || b.submissions != 0 || len(st.Intents) != 0 {
		t.Fatal("paused priority preview traded or lost its cap", err)
	}
	// A separate funded entry must not jump ahead even when a slot is free.
	_, _, _, q, _ := fixture()
	q.Symbol = "AMD"
	entry, ok := Select(c, q.Symbol, q.Kind, []Quote{q}, 0, now)
	if !ok {
		t.Fatal("entry fixture invalid")
	}
	if plan, err := Plan(c, st, s, []Candidate{entry}, now); err != nil || len(plan) != 0 {
		t.Fatal("new entry overtook priority close", err)
	}
	if handled, _, err := RunPriorityClose(t.Context(), c, &st, store, b, s, true, clock); err != nil || !handled || b.submissions != 1 || len(st.Intents) != 1 {
		t.Fatal("priority close was not submitted exactly once", err)
	}
	in := st.Intents[a.Symbol]
	s.Orders = []Order{{ID: in.OrderID, Ref: in.Ref, Symbol: a.Symbol, OptionID: a.ID, Quantity: 1, Filled: 1, State: "filled", Side: "buy", Effect: "close", CreatedAt: now}}
	if err := ReconcileForConfig(c, &st, s, now); err != nil {
		t.Fatal(err)
	}
	// Broker fill alone is insufficient while the position is still present.
	if _, _, err := RunPriorityClose(t.Context(), c, &st, store, b, s, true, clock); err == nil || b.submissions != 1 {
		t.Fatal("position reporting lag permitted reentry or replay")
	}
	s.Options = nil
	if err := ReconcileForConfig(c, &st, s, now); err != nil {
		t.Fatal(err)
	}
	if st.CompletedOrders[in.OrderID].Filled != 1 || len(st.Intents) != 0 {
		t.Fatal("filled completion was not durable")
	}
	if handled, _, err := RunPriorityClose(t.Context(), c, &st, store, b, s, true, clock); err != nil || handled || b.submissions != 1 {
		t.Fatal("completed priority was replayed", err)
	}
	if plan, err := Plan(c, st, s, []Candidate{entry}, now); err != nil || len(plan) != 1 {
		t.Fatal("confirmed close did not unlock fresh reentry", err)
	}
	// A later lifecycle in the same contract does not inherit this exit target.
	st.Positions[a.Symbol] = Position{PutID: a.ID, PutOpenedAt: now.Add(time.Minute)}
	if pending, err := PriorityClosePending(c, st); err != nil || pending {
		t.Fatal("old priority applied to a future lifecycle", err)
	}
}

func TestPriorityProfitCloseFailureFences(t *testing.T) {
	for _, change := range []string{"ask_above_cap", "wrong_contract", "stale", "unknown_outcome", "cancelled", "wrong_lifecycle", "missing_lifecycle"} {
		t.Run(change, func(t *testing.T) {
			c, st, s, a, now := closeFixture()
			c.PriorityProfitClose = &ProfitCloseTarget{Symbol: a.Symbol, OptionID: a.ID, OpenedAt: st.Positions[a.Symbol].PutOpenedAt, MaxLimit: 3}
			b := &closeTestBroker{fakeBroker: fakeBroker{snapshot: s}, candidate: a}
			switch change {
			case "ask_above_cap":
				b.candidate.Ask = 4
			case "wrong_contract":
				b.candidate.ID = "other"
			case "stale":
				b.candidate.At = now.Add(-time.Minute)
			case "unknown_outcome":
				b.submitErr = context.DeadlineExceeded
			case "cancelled":
				st.CompletedOrders["cancelled-close"] = OrderRecord{Closing: true, Ref: "cancelled", Symbol: a.Symbol, OptionID: a.ID, At: now}
			case "wrong_lifecycle":
				c.PriorityProfitClose.OpenedAt = now.Add(-time.Hour)
			case "missing_lifecycle":
				delete(st.Positions, a.Symbol)
			}
			clock := func() time.Time { return now }
			if handled, _, err := RunPriorityClose(t.Context(), c, &st, &fakeStore{}, b, s, true, clock); !handled || err == nil {
				t.Fatal("failed priority did not block entry cycle", err)
			}
			want := 0
			if change == "unknown_outcome" {
				want = 1
				if len(st.Intents) != 1 {
					t.Fatal("uncertain close lost intent")
				}
				_, _, _ = RunPriorityClose(t.Context(), c, &st, &fakeStore{}, b, s, true, clock)
			}
			if b.submissions != want {
				t.Fatal("invalid close or automatic retry reached transport")
			}
		})
	}
}
