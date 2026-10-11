package wheel

import (
	"testing"
	"time"
)

// Exchange-published closures and boundaries, independent of broker quote age.
func TestTradingWindowExchangeCalendar(t *testing.T) {
	for _, date := range []string{"2026-01-01", "2026-01-19", "2026-02-16", "2026-04-03", "2026-05-25", "2026-06-19", "2026-07-03", "2026-09-07", "2026-11-26", "2026-12-25", "2027-06-18", "2027-12-24", "2028-07-04"} {
		t.Run(date, func(t *testing.T) {
			at, err := time.ParseInLocation("2006-01-02 15:04:05", date+" 11:00:00", Eastern)
			if err != nil {
				t.Fatal(err)
			}
			if TradingWindow(at) {
				t.Fatal("exchange holiday admitted an order")
			}
		})
	}
	for _, tc := range []struct {
		at   string
		open bool
	}{
		{"2026-11-27 12:44:59", true},
		{"2026-11-27 12:45:00", false},
		{"2026-11-27 13:00:01", false},
		{"2026-12-24 12:44:59", true},
		{"2026-12-24 12:45:00", false},
		{"2027-11-26 13:00:01", false},
		{"2028-07-03 12:44:59", true},
		{"2028-07-03 12:45:00", false},
		{"2028-11-24 13:00:01", false},
		{"2027-12-31 11:00:00", true}, // NYSE does not observe Saturday New Year's Day on Friday.
		{"2026-10-05 09:34:59", false},
		{"2026-10-05 09:35:00", true},
		{"2026-10-05 15:44:59", true},
		{"2026-10-05 15:45:00", false},
		{"2026-10-04 11:00:00", false},
		{"2026-03-09 09:35:00", true}, // Daylight-saving transition.
		{"2026-11-02 09:35:00", true},
		{"2025-10-06 11:00:00", false}, // Calendar coverage must be explicit.
		{"2029-10-05 11:00:00", false},
	} {
		t.Run(tc.at, func(t *testing.T) {
			at, err := time.ParseInLocation("2006-01-02 15:04:05", tc.at, Eastern)
			if err != nil {
				t.Fatal(err)
			}
			if got := TradingWindow(at.UTC()); got != tc.open {
				t.Fatalf("window = %v, want %v", got, tc.open)
			}
		})
	}
}

func TestExecuteCalendarBlocksFreshQuotes(t *testing.T) {
	for _, date := range []string{"2026-11-27 13:00:01", "2026-04-03 11:00:00"} {
		t.Run(date, func(t *testing.T) {
			c, st, s, q, _ := fixture()
			now, err := time.ParseInLocation("2006-01-02 15:04:05", date, Eastern)
			if err != nil {
				t.Fatal(err)
			}
			s.At, q.At, q.UnderlyingAt = now, now, now
			q.Expiry = now.AddDate(0, 0, 7).Format("2006-01-02")
			a, ok := Select(c, q.Symbol, q.Kind, []Quote{q}, 0, now)
			if !ok {
				t.Fatal("regression must supply an otherwise eligible fresh candidate")
			}
			broker := &fakeBroker{snapshot: s, quotes: []Quote{q}}
			if err := Execute(t.Context(), c, &st, &fakeStore{}, broker, a, func() time.Time { return now }); err == nil {
				t.Fatal("closed exchange admitted execution")
			}
			if broker.submissions != 0 || len(st.Intents) != 0 {
				t.Fatal("closed exchange reached transport or created a submission intent")
			}
		})
	}
}
