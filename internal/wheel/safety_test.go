package wheel

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func fixture() (Config, State, Snapshot, Quote, time.Time) {
	c := DefaultConfig()
	c.Account = "12345678"
	now := time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC)
	st := NewState()
	st.Account = c.Account
	s := Snapshot{Account: c.Account, At: now, AvailableCash: 8_000_000}
	q := Quote{Option: Option{ID: "put-contract", Symbol: "NVDA", Kind: "put", Expiry: "2026-10-09", Strike: 20000}, At: now, UnderlyingAt: now, Spot: 21000, Bid: 200, Ask: 210, Tick: 1, Delta: -.25, OI: 1000, BidSize: 5, Standard: true, Tradable: true}
	return c, st, s, q, now
}

func TestMoneyExact(t *testing.T) {
	for _, s := range []string{"-1", "NaN", "1e3", ".50", "1.001", "9999999999999999", "1.2.3", "+1"} {
		if _, err := ParseCents(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
	for s, want := range map[string]Cents{"0": 0, "80000.0000": 8_000_000, "12.3": 1230, "0.01": 1} {
		got, err := ParseCents(s)
		if err != nil || got != want {
			t.Errorf("%s: %d %v", s, got, err)
		}
	}
}

func TestPlanRejectsOutOfRangeCapital(t *testing.T) {
	for _, name := range []string{"overflowing_stock_total", "excess_aggregate", "pending_put", "unsettled_put"} {
		t.Run(name, func(t *testing.T) {
			c, st, s, q, now := fixture()
			switch name {
			case "overflowing_stock_total":
				// Each row fits its field bounds, but the former unchecked total
				// wrapped int64 and manufactured capacity for a new put.
				for i := range 93 {
					s.Shares = append(s.Shares, Shares{Symbol: fmt.Sprintf("EXT%d", i), Quantity: 1_000_000, Available: 1_000_000, Cost: MaxMoney})
				}
			case "excess_aggregate":
				for _, symbol := range []string{"EXTA", "EXTB", "EXTC"} {
					s.Shares = append(s.Shares, Shares{Symbol: symbol, Quantity: 1, Available: 1, Cost: MaxMoney / 2})
				}
			case "pending_put":
				s.Orders = []Order{{Symbol: "EXT", Kind: "put", Side: "sell", Effect: "open", State: "queued", Quantity: 3, Strike: MaxMoney / 100}}
			case "unsettled_put":
				st.Positions["EXT"] = Position{PutID: "unsettled", PutStrike: MaxMoney}
			}
			a, ok := Select(c, q.Symbol, "put", []Quote{q}, 0, now)
			if !ok {
				t.Fatal("candidate fixture is invalid")
			}
			if plan, err := Plan(c, st, s, []Candidate{a}, now); err == nil || len(plan) != 0 {
				t.Fatalf("unproven capital allowed a plan: %v, %v", plan, err)
			}
		})
	}
}

func TestNearestExpiryLiquidityAndExit(t *testing.T) {
	c, _, _, q, now := fixture()
	if _, ok := Select(c, q.Symbol, "put", []Quote{q}, 0, now); !ok {
		t.Fatal("valid put rejected")
	}
	stale := q
	stale.At = now.Add(-31 * time.Second)
	if _, ok := Select(c, q.Symbol, "put", []Quote{stale}, 0, now); ok {
		t.Fatal("stale quote accepted")
	}
	thursday := q
	thursday.Expiry = "2026-10-08"
	if a, ok := Select(c, q.Symbol, "put", []Quote{thursday, q}, 0, now); !ok || a.Expiry != q.Expiry {
		t.Fatal("daily Thursday confused with weekly put")
	}
	if _, ok := Select(c, q.Symbol, "put", nil, 0, now); ok {
		t.Fatal("empty expiration set accepted")
	}
	illiquid := q
	illiquid.Bid = 0
	later := q
	later.Expiry = "2026-10-16"
	if _, ok := Select(c, q.Symbol, "put", []Quote{illiquid, later}, 0, now); ok {
		t.Fatal("liquidity failure moved expiry")
	}
	call := q
	call.ID = "daily-call"
	call.Kind = "call"
	call.Expiry = "2026-10-06"
	call.Strike = 20500
	call.Bid = 600
	call.Ask = 620
	call.Delta = .7
	other := call
	other.ID = "premium"
	other.Delta = .55
	other.Bid = 700
	other.Ask = 710
	a, ok := Select(c, q.Symbol, "call", []Quote{other, call}, 22000, now)
	if !ok || a.ID != call.ID || a.CallAwayPnL >= 0 {
		t.Fatal("daily below-cost exit not chosen/disclosed")
	}
	c.AllowBelowCost = false
	if _, ok := Select(c, q.Symbol, "call", []Quote{call}, 22000, now); ok {
		t.Fatal("below-cost call under protected policy")
	}
}

func TestSlotsCashCoverage(t *testing.T) {
	c, st, s, q, now := fixture()
	c.SymbolLimitScope = "account"
	a, _ := Select(c, q.Symbol, "put", []Quote{q}, 0, now)
	candidates := []Candidate{a}
	for _, symbol := range []string{"NOW", "AMZN", "AMD"} {
		x := a
		x.Symbol = symbol
		candidates = append(candidates, x)
	}
	plan, err := Plan(c, st, s, candidates, now)
	if err != nil || len(plan) != 3 {
		t.Fatalf("slots: %d %v", len(plan), err)
	}
	limited := c
	limited.MaxCollateral = a.Collateral * 2
	plan, err = Plan(limited, st, s, candidates, now)
	if err != nil || len(plan) != 0 {
		t.Fatal("a reduced account ceiling bypassed its per-symbol allocation")
	}
	s.AvailableCash = a.Collateral * 2
	plan, err = Plan(c, st, s, candidates, now)
	if err != nil || len(plan) != 1 {
		t.Fatalf("cash/fees: %d %v", len(plan), err)
	}
	s.AvailableCash = 8_000_000
	s.Shares = []Shares{{Symbol: "META", Quantity: 1, Available: 1, Cost: 100}}
	plan, err = Plan(c, st, s, candidates, now)
	if err != nil || len(plan) != 2 {
		t.Fatalf("unknown inventory: %d %v", len(plan), err)
	}
	s.Options = []Option{{ID: "existing", Symbol: "NFLX", Kind: "put", Quantity: 1, Strike: 70000}}
	plan, err = Plan(c, st, s, candidates, now)
	if err != nil || len(plan) != 0 {
		t.Fatal("gross collateral ignored")
	}
	s.Options = nil
	s.Shares = []Shares{{Symbol: "NVDA", Quantity: 100, Available: 99, Cost: 22000}}
	st.Positions["NVDA"] = Position{OwnedShares: 100, Cost: 22000, Provenance: "receipt-123"}
	a.Kind = "call"
	plan, err = Plan(c, st, s, []Candidate{a}, now)
	if err != nil || len(plan) != 0 {
		t.Fatal("unavailable shares cover call")
	}
	s.Shares[0].Available = 100
	plan, err = Plan(c, st, s, []Candidate{a}, now)
	if err != nil || len(plan) != 1 {
		t.Fatal("covered call rejected")
	}
}

type fakeStore struct{ saves, failAt int }

func (s *fakeStore) Save(State) error {
	s.saves++
	if s.saves == s.failAt {
		return errors.New("disk failed")
	}
	return nil
}

type fakeBroker struct {
	snapshot    Snapshot
	quotes      []Quote
	submissions int
	submitErr   error
}

func (b *fakeBroker) Snapshot(context.Context) (Snapshot, error)              { return b.snapshot, nil }
func (b *fakeBroker) Quotes(context.Context, string, string) ([]Quote, error) { return b.quotes, nil }
func (b *fakeBroker) Review(context.Context, Candidate) error                 { return nil }
func (b *fakeBroker) Submit(context.Context, Candidate, string) (string, error) {
	b.submissions++
	return "order-1", b.submitErr
}

func TestDurableIntentAndNoReplay(t *testing.T) {
	c, st, s, q, now := fixture()
	a, _ := Select(c, q.Symbol, "put", []Quote{q}, 0, now)
	b := &fakeBroker{snapshot: s, quotes: []Quote{q}}
	store := &fakeStore{failAt: 2}
	if err := Execute(t.Context(), c, &st, store, b, a, func() time.Time { return now }); err == nil || b.submissions != 0 {
		t.Fatal("submission without durable intent")
	}
	st = NewState()
	st.Account = c.Account
	store = &fakeStore{}
	b.submitErr = errors.New("timeout")
	if err := Execute(t.Context(), c, &st, store, b, a, func() time.Time { return now }); err == nil || len(st.Intents) != 1 {
		t.Fatal("ambiguous intent lost")
	}
	if err := Execute(t.Context(), c, &st, store, b, a, func() time.Time { return now }); err == nil || b.submissions != 1 {
		t.Fatal("ambiguous order replayed")
	}
	st = NewState()
	st.Account = c.Account
	b.submitErr = ErrNotSubmitted
	if err := Execute(t.Context(), c, &st, &fakeStore{}, b, a, func() time.Time { return now }); !errors.Is(err, ErrNotSubmitted) || len(st.Intents) != 0 {
		t.Fatal("a conclusive pre-transport pause left a false ambiguous write")
	}
}

func TestAssignmentSettlementAndManualDrift(t *testing.T) {
	_, st, s, _, now := fixture()
	st.Positions["NVDA"] = Position{PutID: "own-put", PutStrike: 20000, PutOpenedAt: now.Add(-72 * time.Hour), Provenance: "own fill"}
	s.Shares = []Shares{{Symbol: "NVDA", Quantity: 100, Available: 100, Cost: 20000}}
	if err := Reconcile(&st, s, now); err != nil || st.Positions["NVDA"].OwnedShares != 0 {
		t.Fatal("assignment delay bypassed")
	}
	if err := Reconcile(&st, s, now.Add(25*time.Hour)); err != nil || st.Positions["NVDA"].OwnedShares != 100 {
		t.Fatalf("assignment: %v", err)
	}
	p := st.Positions["NVDA"]
	p.CallID = "own-filled-call"
	st.Positions["NVDA"] = p
	s.Shares = nil
	if err := Reconcile(&st, s, now.Add(26*time.Hour)); err != nil || len(st.Positions) != 1 {
		t.Fatal("call-away released immediately")
	}
	if err := Reconcile(&st, s, now.Add(51*time.Hour)); err != nil || len(st.Positions) != 0 {
		t.Fatal("call-away failed to settle")
	}
	st.Positions["NVDA"] = Position{PutID: "own-put", PutStrike: 20000, PutOpenedAt: now.Add(-72 * time.Hour), MissingSince: now.Add(-25 * time.Hour), Provenance: "own fill"}
	s.Shares = []Shares{{Symbol: "NVDA", Quantity: 100, Available: 100, Cost: 20000}}
	s.Orders = []Order{{Symbol: "NVDA", CreatedAt: now.Add(-48 * time.Hour), State: "filled"}}
	if err := Reconcile(&st, s, now); err == nil {
		t.Fatal("manual equity order adopted")
	}
}

func TestCancellationPreservesFillAndRef(t *testing.T) {
	c, st, s, q, now := fixture()
	a, _ := Select(c, q.Symbol, "put", []Quote{q}, 0, now)
	st.Intents["NVDA"] = Intent{Ref: "ref-1", Candidate: a, At: now}
	s.Orders = []Order{{ID: "order-1", Ref: "ref-1", Symbol: "NVDA", OptionID: q.ID, Side: "sell", Effect: "open", Quantity: 1, Filled: 1, State: "cancelled"}}
	s.Options = []Option{q.Option}
	s.Options[0].Quantity = 1
	if err := Reconcile(&st, s, now); err != nil || st.Positions["NVDA"].PutID != q.ID || len(st.Intents) != 0 {
		t.Fatal("cancellation lost fill")
	}
	st.Intents["NVDA"] = Intent{Ref: "ref-1", Candidate: a}
	s.Orders = append(s.Orders, s.Orders[0])
	if err := Reconcile(&st, s, now); err == nil {
		t.Fatal("duplicate ref accepted")
	}
}

func TestManagedSlotsAndReceiptRecovery(t *testing.T) {
	c, st, s, q, now := fixture()
	c.SymbolLimitScope = "managed"
	s.ExternalSymbols = []string{"INTC", "MU", "AAPL"}
	a, _ := Select(c, q.Symbol, "put", []Quote{q}, 0, now)
	plan, err := Plan(c, st, s, []Candidate{a}, now)
	if err != nil || len(plan) != 1 {
		t.Fatal("external fractional holdings occupied managed slots", err)
	}
	limit := c.MaxCollateral
	c.MaxCollateral = a.Collateral + c.Fee
	s.ExternalCapital = 1
	plan, err = Plan(c, st, s, []Candidate{a}, now)
	if err != nil || len(plan) != 0 {
		t.Fatal("external capital bypassed the account capital ceiling", err)
	}
	c.MaxCollateral = limit
	s.ExternalCapital = 0
	s.ExternalSymbols = append(s.ExternalSymbols, q.Symbol)
	plan, err = Plan(c, st, s, []Candidate{a}, now)
	if err != nil || len(plan) != 0 {
		t.Fatal("new trade admitted in quarantined symbol")
	}
	s.ExternalSymbols = nil
	st.Intents[q.Symbol] = Intent{Ref: "request", OrderID: "receipt", Candidate: a, At: now}
	s.Orders = []Order{{ID: "receipt", Symbol: q.Symbol, OptionID: q.ID, Side: "sell", Effect: "open", Quantity: 1, Filled: 1, State: "filled"}}
	s.Options = []Option{q.Option}
	s.Options[0].Quantity = 1
	if err := Reconcile(&st, s, now); err != nil || len(st.Intents) != 0 || st.Positions[q.Symbol].PutID != q.ID {
		t.Fatal("confirmed ID could not reconcile without ref echo", err)
	}
	s.ExternalSymbols = []string{q.Symbol}
	if err := Reconcile(&st, s, now); err == nil {
		t.Fatal("fractional drift of owned inventory allowed")
	}
	st = NewState()
	st.Account = c.Account
	st.Intents[q.Symbol] = Intent{Ref: "lost", Candidate: a, At: now}
	if err := Reconcile(&st, s, now); err == nil {
		t.Fatal("lost receipt guessed by contract identity")
	}
}

func TestOwnCallSettlementAndInventoryDrift(t *testing.T) {
	_, st, s, _, now := fixture()
	st.Positions["NVDA"] = Position{OwnedShares: 100, Cost: 20000, InventoryAt: now.Add(-72 * time.Hour), CallID: "own-call", CallExpiry: "2026-10-02", Provenance: "own put assignment"}
	s.Shares = []Shares{{Symbol: "NVDA", Quantity: 100, Available: 100, Cost: 20000}}
	if err := Reconcile(&st, s, now); err != nil || st.Positions["NVDA"].MissingSince.IsZero() {
		t.Fatal("disappeared call skipped settlement hold", err)
	}
	if err := Reconcile(&st, s, now.Add(25*time.Hour)); err != nil || st.Positions["NVDA"].CallID != "" {
		t.Fatal("settled unassigned call did not release coverage", err)
	}
	s.Orders = []Order{{Symbol: "NVDA", CreatedAt: now.Add(-100 * time.Hour), LastTransactionAt: now.Add(-time.Hour), State: "filled"}}
	if err := Reconcile(&st, s, now); err == nil {
		t.Fatal("old manual order filled after ownership was ignored")
	}
	s.Orders = nil
	s.Shares = nil
	if err := Reconcile(&st, s, now); err == nil {
		t.Fatal("unexplained shares disappearance treated as call-away")
	}
}

func TestEquityActivityRequiresTransactionTime(t *testing.T) {
	for _, phase := range []string{"assignment", "owned"} {
		for _, activity := range []string{"missing_time", "known_old", "known_late", "other_symbol", "own_option"} {
			t.Run(phase+"/"+activity, func(t *testing.T) {
				_, st, s, _, now := fixture()
				verified := now.Add(-72 * time.Hour)
				p := Position{OwnedShares: 100, Cost: 20000, InventoryAt: verified, Provenance: "own assignment"}
				if phase == "assignment" {
					p = Position{PutID: "own-put", PutStrike: 20000, PutOpenedAt: verified, MissingSince: now.Add(-25 * time.Hour), Provenance: "own fill"}
				}
				st.Positions["NVDA"] = p
				s.Shares = []Shares{{Symbol: "NVDA", Quantity: 100, Available: 100, Cost: 20000}}
				o := Order{Symbol: "NVDA", CreatedAt: verified.Add(-time.Hour), State: "filled"}
				switch activity {
				case "known_old":
					o.LastTransactionAt = verified.Add(-time.Minute)
				case "known_late":
					o.LastTransactionAt = verified.Add(time.Minute)
				case "other_symbol":
					o.Symbol = "AAPL"
				case "own_option":
					o.OptionID = "own-put"
				}
				s.Orders = []Order{o}
				err := Reconcile(&st, s, now)
				blocked := activity == "missing_time" || activity == "known_late"
				if blocked {
					if err == nil || st.Positions["NVDA"] != p {
						t.Fatalf("unproven equity activity did not preserve ownership boundary: %+v, %v", st.Positions["NVDA"], err)
					}
				} else if err != nil || st.Positions["NVDA"].OwnedShares != 100 {
					t.Fatalf("unrelated or known old history blocked: %+v, %v", st.Positions["NVDA"], err)
				}
			})
		}
	}
}

func TestUntrackedOptionsFreeze(t *testing.T) {
	_, st, s, q, now := fixture()
	s.Options = []Option{q.Option}
	s.Options[0].Quantity = 1
	if err := Reconcile(&st, s, now); err == nil {
		t.Fatal("external short option was silently admitted")
	}
	s.Options = nil
	s.Orders = []Order{{ID: "external", Symbol: q.Symbol, OptionID: q.ID, Quantity: 1, State: "confirmed"}}
	if err := Reconcile(&st, s, now); err == nil {
		t.Fatal("external working option order was silently admitted")
	}
}
