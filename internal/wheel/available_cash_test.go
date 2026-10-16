package wheel

import (
	"strings"
	"testing"
	"time"
)

func TestAvailableCashRemovesFixedAndSlotCeilings(t *testing.T) {
	c, st, s, q, now := fixture()
	c.MaxCollateral = 0
	q.Strike, q.Spot = 85000, 90000
	s.AvailableCash = 9_000_000
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	a, ok := SelectFunded(c, q.Symbol, q.Kind, []Quote{q}, 0, s.AvailableCash, now)
	if !ok {
		t.Fatal("available cash above the former account ceiling was blocked")
	}
	if plan, err := Plan(c, st, s, []Candidate{a}, now); err != nil || len(plan) != 1 {
		t.Fatal("available cash admission failed", err)
	}
	// Existing collateral is already excluded from broker available cash.
	st.Positions["AMZN"] = Position{PutID: "held", PutStrike: 50000, PutExpiry: q.Expiry}
	s.Options = []Option{{ID: "held", Symbol: "AMZN", Kind: "put", Strike: 50000, Expiry: q.Expiry, Quantity: 1}}
	if plan, err := Plan(c, st, s, []Candidate{a}, now); err != nil || len(plan) != 1 {
		t.Fatal("broker cash was reduced by an existing hold twice", err)
	}
	for _, value := range []Cents{-1, MaxMoney + 1} {
		c.MaxCollateral = value
		if c.Validate() == nil {
			t.Fatal("invalid ceiling accepted")
		}
	}
	c.MaxCollateral = 0
	c.CoordinatorSocket, c.CoordinatorPolicy = "/private/coordinator.sock", strings.Repeat("a", 64)
	if c.Validate() == nil {
		t.Fatal("unqualified shared capital policy accepted")
	}
}

func TestAvailableCashFiltersFeesReserveAndKeepsNearestExpiry(t *testing.T) {
	c, _, _, q, now := fixture()
	c.MaxCollateral, c.Reserve = 0, 500
	cash := q.Strike*100 + c.Fee + c.Reserve
	if _, ok := SelectFunded(c, q.Symbol, q.Kind, []Quote{q}, 0, cash, now); !ok {
		t.Fatal("exact funding boundary blocked")
	}
	for _, amount := range []Cents{cash - 1, -1, MaxMoney + 1} {
		if _, ok := SelectFunded(c, q.Symbol, q.Kind, []Quote{q}, 0, amount, now); ok {
			t.Fatal("invalid or insufficient cash admitted")
		}
	}
	smaller := q
	smaller.ID, smaller.Strike, smaller.Bid, smaller.Ask = "funded", 19000, 100, 110
	if a, ok := SelectFunded(c, q.Symbol, q.Kind, []Quote{q, smaller}, 0, cash-1, now); !ok || a.ID != smaller.ID {
		t.Fatal("unfunded best candidate hid a funded contract")
	}
	smaller.Expiry = "2026-10-16"
	if _, ok := SelectFunded(c, q.Symbol, q.Kind, []Quote{q, smaller}, 0, cash-1, now); ok {
		t.Fatal("insufficient cash silently moved expiry")
	}
}

func TestAvailableCashPreservesSlotsAndAggregateCash(t *testing.T) {
	c, st, s, q, now := fixture()
	c.MaxCollateral = 0
	a, _ := Select(c, q.Symbol, q.Kind, []Quote{q}, 0, now)
	var candidates []Candidate
	for _, symbol := range []string{"NVDA", "AMD", "AMZN", "IREN"} {
		x := a
		x.Symbol = symbol
		candidates = append(candidates, x)
	}
	s.AvailableCash = 9_000_000
	if plan, err := Plan(c, st, s, candidates, now); err != nil || len(plan) != 3 {
		t.Fatal("three-slot limit changed", err)
	}
	s.AvailableCash = (a.Collateral+c.Fee)*2 - 1
	if plan, err := Plan(c, st, s, candidates, now); err != nil || len(plan) != 1 {
		t.Fatal("aggregate cash or fee budget exceeded", err)
	}
}

func TestAvailableCashRechecksAfterReview(t *testing.T) {
	c, st, s, q, now := standardFixture()
	c.MaxCollateral = 0
	a, _ := Select(c, q.Symbol, q.Kind, []Quote{q}, 0, now)
	b := &changingReviewBroker{fakeBroker: fakeBroker{snapshot: s, quotes: []Quote{q}}}
	b.afterReview = func(b *changingReviewBroker) { b.snapshot.AvailableCash = a.Collateral + c.Fee - 1 }
	if err := Execute(t.Context(), c, &st, &fakeStore{}, b, a, func() time.Time { return now }); err == nil || b.submissions != 0 || len(st.Intents) != 0 {
		t.Fatal("cash lost during review reached transport", err)
	}
}
