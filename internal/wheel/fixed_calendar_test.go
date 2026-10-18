package wheel

import (
	"testing"
	"time"
)

func TestFixedEntryCalendar(t *testing.T) {
	for _, row := range []struct {
		date, signal string
	}{
		{"2026-10-05", "15:29"}, {"2026-09-08", "15:29"}, // Monday Labor Day.
		{"2026-01-02", ""}, // Thursday Jan 1 closed; Monday–Wednesday were open.
		{"2024-01-02", "15:29"}, {"2026-10-06", ""},
		{"2028-07-03", "12:29"}, // Reviewed early close on the week's first session.
		{"2026-10-04", ""}, {"2026-09-07", ""}, {"2029-01-02", ""},
		{"2026-11-27", ""}, // Shortened Friday is not first session of its week.
	} {
		day, err := time.ParseInLocation(time.DateOnly, row.date, Eastern)
		if err != nil {
			t.Fatal(err)
		}
		signal, execution, latest, ok := FixedEntryTimes(day)
		if ok != (row.signal != "") {
			t.Fatal("weekly calendar changed", row.date, ok)
		}
		if ok && (signal.Format("15:04") != row.signal || execution.Sub(signal) != time.Minute || latest.Sub(signal) != 5*time.Minute) {
			t.Fatal("signal/execution delay changed", row.date, signal, execution, latest)
		}
	}
}
