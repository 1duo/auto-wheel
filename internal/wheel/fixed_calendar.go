package wheel

import "time"

// FixedEntryTimes uses the first reviewed session of the ISO week. It never
// treats an unknown preceding calendar day as a holiday. The signal is 31
// minutes before the actual regular close, with primary execution one minute
// later and an outer five-minute sensitivity boundary. These times are not
// authorization to submit or to retry an order.
func FixedEntryTimes(date time.Time) (signal, execution, latest time.Time, ok bool) {
	close, known := SessionClose(date)
	if !known {
		return
	}
	year, week := date.In(Eastern).ISOWeek()
	for prior := date.In(Eastern).AddDate(0, 0, -1); ; prior = prior.AddDate(0, 0, -1) {
		py, pw := prior.ISOWeek()
		if py != year || pw != week {
			break
		}
		if !CalendarKnown(prior) {
			return
		}
		if _, open := SessionClose(prior); open {
			return
		}
	}
	return close.Add(-31 * time.Minute), close.Add(-30 * time.Minute), close.Add(-26 * time.Minute), true
}
