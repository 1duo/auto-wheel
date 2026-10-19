package researchrd1

import (
	"testing"
	"time"

	"autowheel/internal/wheel"
)

func TestTargetedDecemberCooldownScope(t *testing.T) {
	for _, month := range []time.Month{time.November, time.December} {
		s := &ExperimentSource{targetedControls: TargetedControls{LossReentryDays: 31, DecemberOnly: true}, stockCosts: map[string]int64{}, reentryAfter: map[string]time.Time{}}
		closed := time.Date(2025, month, 5, 16, 0, 0, 0, wheel.Eastern)
		if err := s.observeTargetedRecord(Record{Effect: Effect{Action: "buy_stock", Symbol: "X", At: closed.AddDate(0, -1, 0), Price: 10000, Fee: 100}}); err != nil {
			t.Fatal(err)
		}
		if err := s.observeTargetedRecord(Record{Effect: Effect{Action: "resolve", Symbol: "X", At: closed, Outcome: "called_away", Contract: Contract{Strike: 9900}, Fee: 100}}); err != nil {
			t.Fatal(err)
		}
		if s.reentryAfter["X"].IsZero() != (month == time.November) {
			t.Fatalf("unexpected month %v fence: %v", month, s.reentryAfter["X"])
		}
	}
}

func TestTargetedGrowthCallsPreservePutTarget(t *testing.T) {
	s := &ExperimentSource{targetedControls: TargetedControls{GrowthCalls: true}}
	menu := []Contract{
		{ID: "c103", Symbol: "X", Expiry: "2025-02-07", Kind: "call", Strike: 10300},
		{ID: "c105", Symbol: "X", Expiry: "2025-02-07", Kind: "call", Strike: 10500},
		{ID: "p95", Symbol: "X", Expiry: "2025-02-07", Kind: "put", Strike: 9500},
		{ID: "p90", Symbol: "X", Expiry: "2025-02-07", Kind: "put", Strike: 9000},
	}
	for kind, want := range map[string]string{"call": "c105", "put": "p95"} {
		got, err := s.Choose(Profile{Mode: "balanced", Exit: "tp75"}, kind, "2025-01-06", "X", Book{Bid: 10000, Ask: 10000}, menu)
		if err != nil || got == nil || got.ID != want {
			t.Fatalf("%s = %v, %v; want %s", kind, got, err, want)
		}
	}
}
