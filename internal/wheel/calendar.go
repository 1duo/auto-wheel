package wheel

import "time"

// Reviewed NYSE/NASDAQ closures for 2026-2028.
// Outside coverage all orders are blocked; no inferred future calendar.
// Sources: https://www.nyse.com/trade/hours-calendars and
// https://nasdaqtrader.com/Trader.aspx?id=Calendar (reviewed 2026-10-04).
var exchangeHolidays = map[string]struct{}{
	"2026-01-01": {}, "2026-01-19": {}, "2026-02-16": {}, "2026-04-03": {},
	"2026-05-25": {}, "2026-06-19": {}, "2026-07-03": {}, "2026-09-07": {},
	"2026-11-26": {}, "2026-12-25": {},
	"2027-01-01": {}, "2027-01-18": {}, "2027-02-15": {}, "2027-03-26": {},
	"2027-05-31": {}, "2027-06-18": {}, "2027-07-05": {}, "2027-09-06": {},
	"2027-11-25": {}, "2027-12-24": {},
	"2028-01-17": {}, "2028-02-21": {}, "2028-04-14": {}, "2028-05-29": {},
	"2028-06-19": {}, "2028-07-04": {}, "2028-09-04": {}, "2028-11-23": {},
	"2028-12-25": {},
}

// 13:00 ET early closes with a 12:45 order cutoff.
var earlyCloses = map[string]struct{}{
	"2026-11-27": {},
	"2026-12-24": {},
	"2027-11-26": {},
	"2028-07-03": {},
	"2028-11-24": {},
}

// CalendarKnown reports whether explicit calendar coverage exists for now.
func CalendarKnown(now time.Time) bool {
	y := now.In(Eastern).Year()
	return y >= 2026 && y <= 2028
}

func isExchangeHoliday(date string) bool {
	_, ok := exchangeHolidays[date]
	return ok
}

func isEarlyClose(date string) bool {
	_, ok := earlyCloses[date]
	return ok
}
