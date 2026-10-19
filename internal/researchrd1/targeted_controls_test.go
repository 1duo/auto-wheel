package researchrd1

import (
	"testing"
	"time"

	"autowheel/internal/wheel"
)

func TestTargetedCooldownUsesCalendarDaysAcrossDST(t *testing.T) {
	s := &ExperimentSource{targetedControls: TargetedControls{LossReentryDays: 31},
		stockCosts: map[string]int64{}, reentryAfter: map[string]time.Time{}}
	opened := time.Date(2025, 2, 3, 15, 30, 0, 0, wheel.Eastern)
	if err := s.observeTargetedRecord(Record{Effect: Effect{Action: "buy_stock", Symbol: "X", At: opened, Price: 10000, Fee: 100}}); err != nil {
		t.Fatal(err)
	}
	closed := time.Date(2025, 3, 7, 16, 0, 0, 0, wheel.Eastern)
	if err := s.observeTargetedRecord(Record{Effect: Effect{Action: "resolve", Symbol: "X", At: closed, Outcome: "called_away", Contract: Contract{Strike: 9900}, Fee: 100}}); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2025, 4, 7, 0, 0, 0, 0, wheel.Eastern)
	if !s.reentryAfter["X"].Equal(want) {
		t.Fatalf("cooldown = %v; want %v", s.reentryAfter["X"], want)
	}
	if _, known := s.stockCosts["X"]; known {
		t.Fatal("sold lot retained")
	}
	if err := s.observeTargetedRecord(Record{Effect: Effect{Action: "buy_stock", Symbol: "Y", At: opened, Price: 10000, Fee: 100}}); err != nil {
		t.Fatal(err)
	}
	if err := s.observeTargetedRecord(Record{Effect: Effect{Action: "resolve", Symbol: "Y", At: closed, Outcome: "called_away", Contract: Contract{Strike: 11000}, Fee: 100}}); err != nil {
		t.Fatal(err)
	}
	if !s.reentryAfter["Y"].IsZero() {
		t.Fatal("profitable sale created a cooldown")
	}
}
