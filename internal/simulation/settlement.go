package simulation

import (
	"time"

	"autowheel/internal/wheel"
)

// Money settlement follows the reviewed OCC/banking calendar, rather than the
// exchange trading calendar. Sources, reviewed 2026-10-12:
// https://infomemo.theocc.com/infomemos?number=53701 (2024)
// https://infomemo.theocc.com/infomemos?number=55737 (2025)
// https://infomemo.theocc.com/infomemos?number=57897 (2026)
// https://infomemo.theocc.com/infomemos?number=55826 (January 9, 2025)
// All other scheduled closures match the existing exchange calendar. Good
// Friday is an OCC holiday even though DTC offers limited settlement services.
var bankingHolidays = map[string]struct{}{
	"2024-10-14": {}, "2024-11-11": {},
	"2025-10-13": {}, "2025-11-11": {},
	"2026-10-12": {}, "2026-11-11": {},
}

func settlementDay(at time.Time) bool {
	d := at.In(wheel.Eastern)
	if d.Year() < 2024 || d.Year() > 2026 || d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
		return false
	}
	date := d.Format("2006-01-02")
	if date == "2025-01-09" {
		return true // Trading closed, but OCC money settlement remained open.
	}
	if _, closed := bankingHolidays[date]; closed {
		return false
	}
	_, open := wheel.SessionClose(d)
	return open
}
