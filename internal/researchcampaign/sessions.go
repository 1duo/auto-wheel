package researchcampaign

import (
	"errors"
	"slices"
	"time"

	"autowheel/internal/wheel"
)

// ExchangeDay is supplied evidence, including explicit closed days. This type
// does not infer 2020-2023 holidays or extend the production trading calendar.
type ExchangeDay struct {
	Date    string        `json:"date"`
	OpenAt  time.Time     `json:"open_at,omitzero"`
	CloseAt time.Time     `json:"close_at,omitzero"`
	Halts   []TradingHalt `json:"declared_trading_halts,omitempty"`
}

// TradingHalt is an explicit supplied market closure, not an inferred quote gap.
// The evidence hash binds its declaration; causal halt-feed qualification remains
// separate. [From, Through) excludes native actions and executable NAV samples.
type TradingHalt struct {
	From           time.Time `json:"from"`
	Through        time.Time `json:"through"`
	EvidenceSHA256 string    `json:"evidence_sha256"`
}

type exchangeSession struct {
	ExchangeDay
	Index int
}

type SessionCalendar struct {
	days map[string]exchangeSession
}

func NewSessionCalendar(days []ExchangeDay) (*SessionCalendar, error) {
	if len(days) == 0 || len(days) > 4000 {
		return nil, errors.New("exchange calendar requires 1..4000 explicit consecutive days")
	}
	c := &SessionCalendar{days: make(map[string]exchangeSession, len(days))}
	var prior time.Time
	index := -1
	for _, day := range days {
		date, err := time.ParseInLocation(time.DateOnly, day.Date, wheel.Eastern)
		if err != nil || date.Format(time.DateOnly) != day.Date || date.Year() < 2020 || date.Year() > 2027 || (!prior.IsZero() && !date.Equal(prior.AddDate(0, 0, 1))) {
			return nil, errors.New("invalid, missing or repeated exchange calendar day")
		}
		if day.OpenAt.IsZero() != day.CloseAt.IsZero() {
			return nil, errors.New("exchange day must be completely open or explicitly closed")
		}
		day.Halts = slices.Clone(day.Halts)
		if len(day.Halts) > 20 || day.OpenAt.IsZero() && len(day.Halts) > 0 {
			return nil, errors.New("halt requires an explicit bounded open session")
		}
		var previousEnd time.Time
		for _, halt := range day.Halts {
			if halt.From.Before(day.OpenAt) || !halt.Through.After(halt.From) || !halt.Through.Before(day.CloseAt) || halt.From.Before(previousEnd) || !checkpointHash(halt.EvidenceSHA256) {
				return nil, errors.New("invalid, overlapping or unbound trading halt")
			}
			previousEnd = halt.Through
		}
		if !day.OpenAt.IsZero() {
			open, close := day.OpenAt.In(wheel.Eastern), day.CloseAt.In(wheel.Eastern)
			if date.Weekday() == time.Saturday || date.Weekday() == time.Sunday || !open.Equal(date.Add(9*time.Hour+30*time.Minute)) || close.Minute() != 0 || close.Second() != 0 || close.Nanosecond() != 0 || (close.Hour() != 13 && close.Hour() != 16) || close.Format(time.DateOnly) != day.Date {
				return nil, errors.New("unsupported exchange session clocks")
			}
			index++
		}
		c.days[day.Date] = exchangeSession{ExchangeDay: day, Index: index}
		prior = date
	}
	return c, nil
}

func (c *SessionCalendar) halted(at time.Time) bool {
	return c.haltedWindow(at, at)
}

func (c *SessionCalendar) haltedWindow(from, through time.Time) bool {
	if c == nil {
		return false
	}
	day := c.days[through.In(wheel.Eastern).Format(time.DateOnly)]
	for _, halt := range day.Halts {
		if !through.Before(halt.From) && from.Before(halt.Through) {
			return true
		}
	}
	return false
}

func (c *SessionCalendar) Session(at time.Time) (ExchangeDay, int, error) {
	if c == nil || at.IsZero() {
		return ExchangeDay{}, 0, errors.New("missing exchange calendar or clock")
	}
	day, known := c.days[at.In(wheel.Eastern).Format(time.DateOnly)]
	if !known || day.OpenAt.IsZero() {
		return ExchangeDay{}, 0, errors.New("unknown or closed exchange day")
	}
	result := day.ExchangeDay
	result.Halts = slices.Clone(result.Halts)
	return result, day.Index, nil
}

// FirstSessionOfWeek distinguishes explicit closed days from missing calendar
// evidence. A truncated calendar cannot turn an unknown Monday into a holiday.
func (c *SessionCalendar) FirstSessionOfWeek(at time.Time) (bool, error) {
	day, _, err := c.Session(at)
	if err != nil {
		return false, err
	}
	local := day.OpenAt.In(wheel.Eastern)
	year, week := local.ISOWeek()
	for prior := local.AddDate(0, 0, -1); ; prior = prior.AddDate(0, 0, -1) {
		py, pw := prior.ISOWeek()
		if year != py || week != pw {
			return true, nil
		}
		previous, known := c.days[prior.Format(time.DateOnly)]
		if !known {
			return false, errors.New("weekly entry needs every preceding calendar day in its week")
		}
		if !previous.OpenAt.IsZero() {
			return false, nil
		}
	}
}

// NextSession returns the next explicitly reviewed exchange session. It never
// infers holidays, extends the calendar or substitutes a banking business day.
func (c *SessionCalendar) NextSession(at time.Time) (ExchangeDay, int, error) {
	if c == nil || at.IsZero() {
		return ExchangeDay{}, 0, errors.New("missing next-session calendar/clock")
	}
	if _, known := c.days[at.In(wheel.Eastern).Format(time.DateOnly)]; !known {
		return ExchangeDay{}, 0, errors.New("current day is outside reviewed calendar")
	}
	for next := at.In(wheel.Eastern).AddDate(0, 0, 1); ; next = next.AddDate(0, 0, 1) {
		day, known := c.days[next.Format(time.DateOnly)]
		if !known {
			return ExchangeDay{}, 0, errors.New("next session is outside reviewed calendar")
		}
		if !day.OpenAt.IsZero() {
			result := day.ExchangeDay
			result.Halts = slices.Clone(result.Halts)
			return result, day.Index, nil
		}
	}
}

// Decision matches native five-minute scanning: 09:35 through the last
// five-minute boundary strictly before close minus 15 minutes.
func (c *SessionCalendar) Decision(at time.Time) (int, error) {
	day, index, err := c.Session(at)
	if err != nil {
		return 0, err
	}
	local := at.In(wheel.Eastern)
	if at.Before(day.OpenAt.Add(5*time.Minute)) || !at.Before(day.CloseAt.Add(-15*time.Minute)) || local.Minute()%5 != 0 || local.Second() != 0 || local.Nanosecond() != 0 || c.halted(at) {
		return 0, errors.New("not a native five-minute decision")
	}
	return index, nil
}
