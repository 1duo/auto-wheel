package researchcampaign

import (
	"strings"
	"testing"
	"time"

	"autowheel/internal/wheel"
)

func TestRecoveredCalendarDoesNotInferMissingSessions(t *testing.T) {
	at := time.Date(2026, 10, 20, 9, 30, 0, 0, wheel.Eastern)
	calendar, err := NewSessionCalendar([]ExchangeDay{{Date: "2026-10-20", OpenAt: at, CloseAt: at.Add(390 * time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := calendar.FirstSessionOfWeek(at); err == nil {
		t.Fatal("unknown Monday was treated as a holiday")
	}
	if _, _, err := calendar.NextSession(at); err == nil {
		t.Fatal("missing next session was invented")
	}
	if _, err := NewSessionCalendar([]ExchangeDay{{Date: "2026-10-20", OpenAt: at, CloseAt: at.Add(390 * time.Minute),
		Halts: []TradingHalt{{From: at.Add(time.Hour), Through: at.Add(2 * time.Hour), EvidenceSHA256: strings.Repeat("z", 64)}}}}); err == nil {
		t.Fatal("unbound halt admitted")
	}
}

func TestRecoveredSettlementAndFeeBoundaries(t *testing.T) {
	start := time.Date(2024, 5, 24, 9, 30, 0, 0, wheel.Eastern)
	var days []ClearingDay
	for d := start; !d.After(start.AddDate(0, 0, 7)); d = d.AddDate(0, 0, 1) {
		day := ClearingDay{Date: d.Format(time.DateOnly), Open: d.Weekday() != time.Saturday && d.Weekday() != time.Sunday && d.Day() != 27}
		if day.Open {
			day.AvailableAt = d
		}
		days = append(days, day)
	}
	calendar, err := NewSettlementCalendar(days)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []struct {
		at   time.Time
		flow SettlementFlow
		want string
	}{
		{start, StockTrade, "2024-05-29"}, {start, OptionPremium, "2024-05-28"},
		{start.AddDate(0, 0, 4), PhysicalExercise, "2024-05-29"},
	} {
		got, err := calendar.Next(v.at, v.flow)
		if err != nil || got.Format(time.DateOnly) != v.want {
			t.Fatal("settlement transition changed", got, err)
		}
	}
	rule := SaleFeeRule{NotionalDenominator: 1, ShareNumerator: 1, ShareDenominator: 2, ShareRounding: "nearest"}
	if _, err := rule.Cumulative(1, 100); err == nil {
		t.Fatal("unqualified half-cent tie rounded silently")
	}
	if _, err := rule.Cumulative(100, wheel.MaxMoney+1); err == nil {
		t.Fatal("unbounded fee notional admitted")
	}
}
