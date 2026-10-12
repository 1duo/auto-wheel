package simulation

import (
	"testing"
	"time"

	"autowheel/internal/wheel"
)

func settlementClock(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.ParseInLocation("2006-01-02 15:04", value, wheel.Eastern)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func TestNextSettlementUsesClearingCalendar(t *testing.T) {
	for _, tc := range []struct{ trade, settle string }{
		{"2024-10-11 16:00", "2024-10-15 09:30"},
		{"2024-11-08 16:00", "2024-11-12 09:30"},
		{"2025-01-08 16:00", "2025-01-09 09:30"},
		{"2025-01-10 16:00", "2025-01-13 09:30"},
		{"2025-04-17 16:00", "2025-04-21 09:30"},
		{"2025-10-10 16:00", "2025-10-14 09:30"},
		{"2025-10-13 10:00", "2025-10-14 09:30"},
		{"2025-11-10 16:00", "2025-11-12 09:30"},
		{"2025-12-31 16:00", "2026-01-02 09:30"},
		{"2026-10-09 16:00", "2026-10-13 09:30"},
		{"2026-11-10 16:00", "2026-11-12 09:30"},
	} {
		t.Run(tc.trade, func(t *testing.T) {
			if got := nextSettlement(settlementClock(t, tc.trade)); !got.Equal(settlementClock(t, tc.settle)) {
				t.Fatalf("settlement = %v; want %s", got, tc.settle)
			}
		})
	}
	for _, value := range []string{"2023-12-15 16:00", "2026-12-31 16:00", "2027-06-01 16:00"} {
		at := settlementClock(t, value)
		if got := nextSettlement(at); !got.Equal(at.Add(1000 * 24 * time.Hour)) {
			t.Fatalf("unknown settlement calendar unlocked funds at %v", got)
		}
	}
}

func TestSettlementKeepsCashAndSharesLockedOnBankHoliday(t *testing.T) {
	settled := nextSettlement(settlementClock(t, "2025-10-10 16:00"))
	r := replay{cash: 100_000, credits: []credit{{Amount: 10_000, SettledAt: settled}},
		shares: map[string]inventory{"NVDA": {Cost: 10_000, SettledAt: settled}}}
	for _, at := range []string{"2025-10-13 10:00", "2025-10-14 09:29"} {
		r.now = settlementClock(t, at)
		s := r.snapshot()
		if s.AvailableCash != 90_000 || s.Shares[0].Available != 0 {
			t.Fatalf("settlement reused unsettled cash/shares: %+v", s)
		}
	}
	r.now = settled
	s := r.snapshot()
	if s.AvailableCash != 100_000 || s.Shares[0].Available != 100 {
		t.Fatalf("settled resources remain locked: %+v", s)
	}
}

func TestPhysicalLifecycleRejectsBankHolidaySettlement(t *testing.T) {
	for _, outcome := range []string{"assigned", "called_away"} {
		t.Run(outcome, func(t *testing.T) {
			kind := "put"
			if outcome == "called_away" {
				kind = "call"
			}
			r := replay{now: settlementClock(t, "2025-11-07 16:00"), cash: 2_000_000,
				shorts: map[string]wheel.Option{"test": {ID: "test", Symbol: "NVDA", Kind: kind, Strike: 10_000}},
				shares: map[string]inventory{}}
			if outcome == "called_away" {
				r.shares["NVDA"] = inventory{Cost: 10_000}
			}
			ownedBefore := len(r.shares)
			if err := r.lifecycle(Lifecycle{OptionID: "test", Outcome: outcome, Assumption: "modeled",
				SettledAt: settlementClock(t, "2025-11-11 09:30")}); err == nil {
				t.Fatal("accepted physical settlement on a banking holiday")
			}
			if r.cash != 2_000_000 || len(r.shorts) != 1 || len(r.shares) != ownedBefore {
				t.Fatal("rejected settlement changed ownership or cash")
			}
			if err := r.lifecycle(Lifecycle{OptionID: "test", Outcome: outcome, Assumption: "modeled",
				SettledAt: settlementClock(t, "2025-11-10 09:30")}); err != nil {
				t.Fatalf("valid settlement rejected: %v", err)
			}
		})
	}
}
