package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"autowheel/internal/contractpolicy"
	"autowheel/internal/robinhood"
	"autowheel/internal/wheel"
)

type fixedCycleStore struct{ raw []byte }

func (s *fixedCycleStore) Save(st wheel.State) error {
	if err := st.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(st)
	s.raw = raw
	return err
}

func (*fixedCycleStore) ArchiveFixedBatch(wheel.FixedBatch) (string, error) {
	return strings.Repeat("c", 64), nil
}

func fixedCycleFixture(t *testing.T) (wheel.Config, *wheel.FixedController, *wheel.State, time.Time) {
	t.Helper()
	at := time.Date(2026, 10, 19, 15, 29, 0, 0, wheel.Eastern)
	c := wheel.DefaultConfig()
	c.Account, c.Symbols = "12345678", []string{"MSFT", "AAPL", "AMD", "INTC", "ORCL"}
	c.OrderCoveragePolicy, c.TradePulseStateDir = "exclusive_standard", "/peer"
	c.RetiredTradePulse, c.ExclusiveOrdersSince = "/peer/supervisor/cross-host-handoff-sealed.json", at.AddDate(0, 0, -30)
	c.Reserve = 1_680_944
	c.FixedPolicy = &wheel.FixedPolicyConfig{Spec: wheel.FixedPolicySpec{ID: wheel.FixedConservativePolicy, Account: c.Account,
		Limits:          contractpolicy.CapitalLimits{Account: 8_000_000, Symbol: 2_666_666, Reserve: 1_680_944},
		OptionFeeBudget: c.Fee, PhysicalFeeBudget: 100, MaxQuoteAge: c.MaxQuoteAgeSeconds,
		FeeSourceSHA: strings.Repeat("a", 64), FeeKnownAt: at.Add(-24 * time.Hour)},
		Roster: wheel.FixedRoster{Symbols: c.Symbols, SourceSHA: strings.Repeat("b", 64), KnownAt: at.Add(-time.Hour), From: "2026-01-01", Through: "2026-12-31"}}
	st := new(wheel.NewState())
	st.Account = c.Account
	control, err := wheel.NewFixedController(c.FixedPolicy.Spec, st, new(fixedCycleStore))
	if err != nil {
		t.Fatal(err)
	}
	bound := at.Add(-time.Hour)
	snapshot := wheel.Snapshot{Account: c.Account, At: bound, OrderCoveragePolicy: "exclusive_standard", AvailableCash: 8_404_717}
	if err = control.BindNative(c, snapshot, bound); err != nil {
		t.Fatal(err)
	}
	return c, control, st, at
}

type fixedParallelReader struct {
	entered chan string
	release chan struct{}
	active  atomic.Int32
	maximum atomic.Int32
	calls   atomic.Int32
}

func (b *fixedParallelReader) ObserveFixedPolicy(ctx context.Context, mode, kind, symbol string, clock func() time.Time) (robinhood.FixedMarketObservation, error) {
	b.calls.Add(1)
	n := b.active.Add(1)
	defer b.active.Add(-1)
	for prior := b.maximum.Load(); n > prior; prior = b.maximum.Load() {
		if b.maximum.CompareAndSwap(prior, n) {
			break
		}
	}
	b.entered <- symbol
	select {
	case <-ctx.Done():
		return robinhood.FixedMarketObservation{}, ctx.Err()
	case <-b.release:
	}
	if symbol == "AAPL" {
		return robinhood.FixedMarketObservation{}, errors.New("missing source book")
	}
	q := contractpolicy.Contract{ID: symbol + "-put", Symbol: symbol, Kind: kind, Expiry: "2026-11-20", Strike: 9000}
	return robinhood.FixedMarketObservation{At: clock(), Mode: mode, Kind: kind, Symbol: symbol,
		Choice: new(q), Menu: []contractpolicy.Contract{q}, MenuContracts: 1,
		Stock: contractpolicy.Book{Bid: 9999, Ask: 10001}, Option: contractpolicy.Book{Bid: 100, Ask: 110},
		BidAt: clock(), AskAt: clock(), OptionAt: clock(), Tick: 5, BidSize: 1, MarketFresh: true, EntryBookUsable: true, Structural100: true}, nil
}

func TestFixedCaptureParallelBoundAndLateSourceAbstention(t *testing.T) {
	for _, late := range []bool{false, true} {
		config, control, st, at := fixedCycleFixture(t)
		if err := control.Begin(config.FixedPolicy.Roster, at); err != nil {
			t.Fatal(err)
		}
		var nanos atomic.Int64
		nanos.Store(at.UnixNano())
		clock := func() time.Time { return time.Unix(0, nanos.Load()).In(wheel.Eastern) }
		reader := &fixedParallelReader{entered: make(chan string, 5), release: make(chan struct{})}
		done := make(chan error, 1)
		go func() { done <- captureFixed(t.Context(), control, st, reader, clock) }()
		for range 3 {
			select {
			case <-reader.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("source reads did not run three at a time")
			}
		}
		if late {
			nanos.Store(at.Add(time.Minute + time.Nanosecond).UnixNano())
		}
		close(reader.release)
		if err := <-done; err != nil || reader.maximum.Load() != 3 || reader.calls.Load() != 5 || st.Fixed.Batch.Phase != "sealed" {
			t.Fatal("capture did not seal the complete bounded cohort", err, reader.maximum.Load(), reader.calls.Load())
		}
		for _, signal := range st.Fixed.Batch.Signals {
			if late && (signal.Phase != "abstained" || signal.Reason != "capture_interrupted") ||
				!late && signal.Symbol == "AAPL" && signal.Phase != "abstained" || !late && signal.Symbol != "AAPL" && signal.Phase != "ready" {
				t.Fatal("late/missing source created a replacement signal", signal)
			}
		}
	}
}

func TestFixedConfigRedactionAndSelectionFence(t *testing.T) {
	c, _, st, at := fixedCycleFixture(t)
	public := c.PublicCopy()
	raw, err := json.Marshal(public)
	if err != nil || strings.Contains(string(raw), c.Account) || c.FixedPolicy.Spec.Account != c.Account {
		t.Fatal("nested fixed account leaked or redaction mutated live config", err)
	}
	public.FixedPolicy.Roster.Symbols[0] = "META"
	if c.Symbols[0] != "MSFT" {
		t.Fatal("public roster aliases immutable live configuration")
	}
	if _, err := c.WithSelectionMode(wheel.SelectionBalanced); err == nil {
		t.Fatal("legacy mode switch rewrote fixed financial policy")
	}
	if _, err := wheel.Plan(c, *st, wheel.Snapshot{}, nil, at); err == nil {
		t.Fatal("legacy planner accepted fixed config")
	}
	c.FixedPolicy.Spec.Limits.Account--
	if c.Validate() == nil {
		t.Fatal("native and fixed cash limits can silently diverge")
	}
}

func TestFixedReadinessCannotReuseLegacyApproval(t *testing.T) {
	c, _, _, _ := fixedCycleFixture(t)
	now := time.Now()
	s := status{Config: c.PublicCopy(), LiveCapable: true,
		Broker:        &wheel.Snapshot{At: now, OrderCoveragePolicy: "exclusive_standard"},
		LiveReadiness: &liveReadiness{At: now, Ready: true, OrderCoveragePolicy: "exclusive_standard", CoverageWarning: wheel.StandardOrderWarning}}
	if activationReady(s) {
		t.Fatal("legacy readiness authorized the fixed policy")
	}
	s.LiveReadiness.Policy = wheel.FixedConservativePolicy
	if !activationReady(s) {
		t.Fatal("fixed readiness policy binding was not recognized")
	}
}

func TestFixedBindingRejectsUnexplainedTerminalActivity(t *testing.T) {
	c, _, _, at := fixedCycleFixture(t)
	st := wheel.NewState()
	st.Account = c.Account
	store := new(fixedCycleStore)
	control, err := wheel.NewFixedController(c.FixedPolicy.Spec, &st, store)
	if err != nil {
		t.Fatal(err)
	}
	s := wheel.Snapshot{At: at, Account: c.Account, OrderCoveragePolicy: "exclusive_standard",
		Orders: []wheel.Order{{ID: "outside", OptionID: "peer", Symbol: "HOOD", Quantity: 1, State: "cancelled", CreatedAt: at.Add(-time.Hour)}}}
	if control.BindNative(c, s, at) == nil || st.Fixed != nil || len(store.raw) != 0 {
		t.Fatal("flat but unexplained account activity was accepted as an exclusive handover")
	}
}

func TestFixedSchedulerKeepsWeeklyClockAndEarlyClose(t *testing.T) {
	c, _, _, at := fixedCycleFixture(t)
	for _, test := range []struct {
		at   time.Time
		want time.Duration
	}{
		{at.Add(-10 * time.Second), 10 * time.Second}, {at.Add(20 * time.Second), 40 * time.Second},
		{at.Add(time.Minute), 15 * time.Second}, {at.Add(6 * time.Minute), 5 * time.Minute},
		{time.Date(2026, 11, 27, 12, 28, 50, 0, wheel.Eastern), 5 * time.Minute}, // Friday is not that week's first session.
	} {
		if got := cycleDelay(c, test.at); got != test.want {
			t.Fatal("scheduler missed/fabricated the weekly clock", test.at, got, test.want)
		}
	}
}
