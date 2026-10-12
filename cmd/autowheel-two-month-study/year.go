package main

import (
	"errors"
	"math"
	"slices"
	"time"

	"autowheel/internal/wheel"
)

func diagnosticExpiry(date time.Time) bool {
	// Expiries are civil date labels; UTC midnight must not become yesterday ET.
	year, month, day := date.Date()
	date = time.Date(year, month, day, 12, 0, 0, 0, wheel.Eastern)
	if _, open := wheel.SessionClose(date); !open {
		return false
	}
	if date.Weekday() == time.Friday {
		return true
	}
	for next := date.AddDate(0, 0, 1); next.Weekday() <= time.Friday && next.Weekday() != time.Sunday; next = next.AddDate(0, 0, 1) {
		if _, open := wheel.SessionClose(next); open {
			return false
		}
		if next.Weekday() == time.Friday {
			return true
		}
	}
	return false
}

func yearOrderFunded(q quote, cash, fee int64) bool {
	if fee < 0 || cash < fee {
		return false
	}
	if q.Kind == "put" {
		return validMoney(q.Strike) && q.Strike*100 <= initialCash && q.Strike*100 <= cash-fee
	}
	return q.Kind == "call"
}

// The new scope is explicit. Older frozen studies retain their own schedules.
func yearToDateSchedule() (sessions, entries []string) {
	start := time.Date(2026, time.January, 2, 9, 35, 0, 0, wheel.Eastern)
	end := time.Date(2026, time.October, 9, 9, 35, 0, 0, wheel.Eastern)
	lastYear, lastWeek := 0, 0
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		if !wheel.TradingWindow(d) {
			continue
		}
		date := d.Format("2006-01-02")
		sessions = append(sessions, date)
		year, week := d.ISOWeek()
		if (year != lastYear || week != lastWeek) && d.Weekday() != time.Friday {
			entries = append(entries, date)
		}
		lastYear, lastWeek = year, week
	}
	return sessions, entries
}

func validateYearToDateInput(in input) error {
	if in.Start != "2026-01-02" || in.End != "2026-10-09" || !slices.Contains(finished2026Symbols, in.Symbol) || len(in.Sources) == 0 {
		return errors.New("incorrect year-to-date diagnostic scope")
	}
	sessions, entries := yearToDateSchedule()
	if len(in.ReferenceCloses) != len(sessions) {
		return errors.New("year-to-date regime references incomplete")
	}
	for _, date := range sessions {
		ref, ok := in.ReferenceCloses[date]
		label, err := time.Parse("2006-01-02", date)
		if !ok || err != nil || ref.Date != date || ref.Symbol != in.Symbol || ref.Native <= 0 || ref.Native > 1_000_000*10_000_000 || ref.LabelNS != label.UnixNano() || len(in.Sources[ref.Source]) != 64 {
			return errors.New("invalid year-to-date regime reference")
		}
	}
	return validateStudySchedule(in, sessions, entries)
}

// A frozen research policy uses only the twenty completed sessions preceding
// the decision. It cannot inspect today's close or the eventual option outcome.
// Official bar dates are observations; publication clocks remain unqualified.
func cycleProfile(in input, index int) (profile, string, bool) {
	p := baseline(wheel.SelectionConservative)
	p.ID = "cycle-adaptive-warmup"
	if index < 21 {
		return p, "warmup", true
	}
	prices := make([]float64, 21)
	for i := range prices {
		prices[i] = float64(in.ReferenceCloses[in.Days[index-21+i].Date].Native)
	}
	returns := make([]float64, 20)
	mean, sumPrice, high := 0.0, 0.0, 0.0
	for i := range returns {
		returns[i] = math.Log(prices[i+1] / prices[i])
		mean += returns[i] / 20
		sumPrice += prices[i+1]
		high = max(high, prices[i+1])
	}
	variance := 0.0
	for _, r := range returns {
		variance += (r - mean) * (r - mean) / 19
	}
	volatility := math.Sqrt(variance * 252)
	last, trend := prices[20], prices[20]/prices[0]-1
	if last/high-1 <= -.10 || volatility >= 1 {
		p.ID = "cycle-adaptive-stress-exit"
		return p, "stress", false
	}
	if trend <= -.03 && last < sumPrice/20 {
		p.ID = "cycle-adaptive-falling-exit"
		return p, "falling", false
	}
	p = baseline(wheel.SelectionBalanced)
	p.PutCeiling = .25
	p.CallCeiling = .70
	p.ID = "cycle-adaptive-sideways"
	if trend >= .03 && last > sumPrice/20 {
		p.CallCeiling, p.ID = .60, "cycle-adaptive-rising"
		return p, "rising", true
	}
	return p, "sideways", true
}
