package main

import (
	"slices"
	"testing"
	"time"

	"autowheel/internal/wheel"
)

func TestYearToDateScheduleAndScope(t *testing.T) {
	sessions, entries := yearToDateSchedule()
	if len(sessions) != 194 || len(entries) != 40 || sessions[0] != "2026-01-02" || sessions[len(sessions)-1] != "2026-10-09" {
		t.Fatalf("incorrect year-to-date coverage: %d sessions, %d entries", len(sessions), len(entries))
	}
	for _, holiday := range []string{"2026-01-19", "2026-02-16", "2026-04-03", "2026-06-19", "2026-07-03", "2026-09-07"} {
		if slices.Contains(sessions, holiday) {
			t.Fatalf("holiday included: %s", holiday)
		}
	}
	in := input{Schema: "AutoWheelYearToDateEndpointInputV1", Symbol: "NVDA", Start: "2026-01-02", End: "2026-10-09", Sources: map[string]string{"observed": "0000000000000000000000000000000000000000000000000000000000000000"}, ReferenceCloses: map[string]officialClose{}}
	for _, date := range sessions {
		at, err := time.ParseInLocation("2006-01-02 15:04", date+" 16:00", wheel.Eastern)
		if err != nil {
			t.Fatal(err)
		}
		d := day{Date: date, Close: sample{At: at, Stock: stock{At: at.Add(-time.Second), Bid: 10000, Ask: 10001}}}
		if slices.Contains(entries, date) {
			decision := at.Add(-6*time.Hour - 25*time.Minute)
			d.Entry = new(sample{At: decision, Stock: stock{At: decision.Add(-time.Second), Bid: 10000, Ask: 10001}})
			d.Execution = new(execution{At: decision.Add(time.Minute), NotBeforeNS: decision.Add(time.Second).UnixNano()})
		}
		in.Days = append(in.Days, d)
		label, _ := time.Parse("2006-01-02", date)
		in.ReferenceCloses[date] = officialClose{Date: date, Symbol: "NVDA", Native: 10000 * 10_000_000, LabelNS: label.UnixNano(), Source: "observed"}
	}
	if err := validateYearToDateInput(in); err != nil {
		t.Fatal(err)
	}
	in.Days = in.Days[1:]
	if validateYearToDateInput(in) == nil {
		t.Fatal("missing January session accepted")
	}
	in.End = "2026-12-31"
	if validateYearToDateInput(in) == nil {
		t.Fatal("future sessions accepted")
	}
}

func TestYearCyclePolicyCannotReadFuture(t *testing.T) {
	sessions, _ := yearToDateSchedule()
	in := input{ReferenceCloses: map[string]officialClose{}}
	for i, date := range sessions {
		in.Days = append(in.Days, day{Date: date})
		in.ReferenceCloses[date] = officialClose{Native: int64(10000+i*50) * 10_000_000}
	}
	before, regime, permit := cycleProfile(in, 30)
	if regime != "rising" || !permit || before.CallCeiling != .60 || before.PutCeiling != .25 {
		t.Fatalf("rising policy: %+v %s %v", before, regime, permit)
	}
	for i := 30; i < len(sessions); i++ {
		in.ReferenceCloses[sessions[i]] = officialClose{Native: 1}
	}
	after, label, allow := cycleProfile(in, 30)
	if before != after || label != regime || allow != permit {
		t.Fatal("future closes changed an earlier decision")
	}
	in.ReferenceCloses[sessions[29]] = officialClose{Native: 7000 * 10_000_000}
	p, label, allow := cycleProfile(in, 30)
	if label != "stress" || allow || p.CallCeiling != .85 {
		t.Fatal("stress policy failed to preserve cash entries and cover owned shares")
	}
}

func TestYearHolidayExpirations(t *testing.T) {
	for _, date := range []string{"2026-04-02", "2026-06-18", "2026-07-02"} {
		expiry, err := time.Parse("2006-01-02", date)
		if err != nil || !diagnosticExpiry(expiry) {
			t.Fatalf("holiday expiry rejected: %s", date)
		}
	}
	expiry, _ := time.Parse("2006-01-02", "2026-04-01")
	if diagnosticExpiry(expiry) {
		t.Fatal("ordinary Wednesday accepted as weekly expiry")
	}
}

func TestDiagnosticExpiryPreservesCivilDate(t *testing.T) {
	for _, tc := range []struct {
		date string
		want bool
	}{
		{"2024-03-28", true}, {"2024-03-29", false},
		{"2025-07-03", true}, {"2025-07-04", false},
		{"2026-04-02", true}, {"2026-04-03", false},
		{"2026-06-18", true}, {"2026-07-02", true},
		{"2026-04-01", false}, {"2026-04-04", false},
	} {
		for _, location := range []*time.Location{time.UTC, wheel.Eastern} {
			date, err := time.ParseInLocation("2006-01-02", tc.date, location)
			if err != nil || diagnosticExpiry(date) != tc.want {
				t.Fatalf("civil expiry %s in %s: want %v, error %v", tc.date, location, tc.want, err)
			}
		}
	}
}

func TestYearCapitalAndPrePremiumFunding(t *testing.T) {
	q := quote{Kind: "put", Strike: 80000}
	if yearOrderFunded(q, initialCash, 100) {
		t.Fatal("premium incorrectly allowed to fund the upfront fee")
	}
	if !yearOrderFunded(q, initialCash+100, 100) {
		t.Fatal("fully secured order rejected")
	}
	q.Strike++
	if yearOrderFunded(q, initialCash*2, 100) {
		t.Fatal("compounded cash allowed commitment above $80,000")
	}
	if yearOrderFunded(quote{Kind: "call"}, 99, 100) {
		t.Fatal("covered-call fee not funded")
	}
}
