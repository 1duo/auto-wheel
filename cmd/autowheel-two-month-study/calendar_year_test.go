package main

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"autowheel/internal/wheel"
)

func TestMultiyearCalendarsClosuresAndEarlyClocks(t *testing.T) {
	for _, tc := range []struct {
		year, sessions int
		first, last    string
	}{
		{2024, 128, "2024-07-01", "2024-12-31"}, {2025, 250, "2025-01-02", "2025-12-31"},
	} {
		sessions, entries, err := calendarYearSchedule(tc.year)
		if err != nil || len(sessions) != tc.sessions || sessions[0] != tc.first || sessions[len(sessions)-1] != tc.last || len(entries) < 26 {
			t.Fatalf("incorrect annual coverage %d: sessions=%d entries=%d %v", tc.year, len(sessions), len(entries), err)
		}
		for _, date := range []string{"2024-03-29", "2025-01-09", "2025-04-18", "2025-07-04"} {
			if slices.Contains(sessions, date) {
				t.Fatal("closed market session included", date)
			}
		}
	}
	for _, tc := range []struct {
		date   string
		expiry bool
		hour   int
	}{
		{"2024-03-28", true, 16}, {"2024-07-03", false, 13}, {"2025-04-17", true, 16}, {"2025-07-03", true, 13}, {"2025-12-24", false, 13},
	} {
		d, _ := time.ParseInLocation("2006-01-02", tc.date, wheel.Eastern)
		at, open := wheel.SessionClose(d)
		if !open || at.Hour() != tc.hour || diagnosticExpiry(d) != tc.expiry {
			t.Fatal("incorrect expiry or closing clock", tc.date)
		}
	}
}

func TestMultiyearPresetsIgnoreFutureAndBlockHeldSplit(t *testing.T) {
	for _, p := range calendarYearProfiles() {
		in := fixture()
		a := &account{}
		before := portfolioCandidates(in, 0, a, p, 6, 0)
		in.Days[0].Close, in.Days[0].Execution = sample{}, nil
		for i := 1; i < len(in.Days); i++ {
			in.Days[i] = day{}
		}
		in.ReferenceCloses, in.ExpiryCloses = nil, nil
		if after := portfolioCandidates(in, 0, a, p, 6, 0); !reflect.DeepEqual(before, after) {
			t.Fatal("future data changed selected quotes", p.ID)
		}
	}
	in := fixture()
	in.ReferenceCloses = map[string]officialClose{}
	for _, d := range in.Days {
		in.ReferenceCloses[d.Date] = officialClose{Native: 10000 * 10_000_000}
	}
	p := baseline(wheel.SelectionConservative)
	// The first filled put remains open the following session. Applying a
	// corporate action cannot silently reset that liability to a flat account.
	actions := []historicalAction{{Symbol: in.Symbol, Date: in.Days[1].Date, Kind: "split", Source: "synthetic"}}
	if _, err := simulatePortfolioWithActions([]input{in}, p, 8_500_000, 6, 0, 15_000_000, nil, nil, actions); err == nil {
		t.Fatal("held unsupported split produced performance")
	}
}
