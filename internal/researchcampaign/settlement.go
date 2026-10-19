// Package researchcampaign contains offline campaign accounting primitives.
// It has no broker, production account binding, or source-qualification authority.
package researchcampaign

import (
	"errors"
	"fmt"
	"time"

	"autowheel/internal/wheel"
)

// SettlementFlow identifies the transaction whose resources are being settled.
type SettlementFlow string

const (
	OptionPremium    SettlementFlow = "option_premium"
	StockTrade       SettlementFlow = "stock_trade"
	PhysicalExercise SettlementFlow = "physical_exercise"
)

// ClearingDay must include closed days too. AvailableAt is an explicit modeled
// resource-availability clock, never a historical broker receipt.
type ClearingDay struct {
	Date        string    `json:"date"`
	Open        bool      `json:"open"`
	AvailableAt time.Time `json:"available_at,omitzero"`
}

// SettlementCalendar preserves a complete supplied daily envelope. Construction
// checks its structure; independently reviewed sources are still required.
type SettlementCalendar struct {
	days map[string]ClearingDay
}

// NewSettlementCalendar rejects missing, duplicate, unordered, or ambiguous days.
// It does not infer holidays from an exchange calendar or fill omitted dates.
func NewSettlementCalendar(days []ClearingDay) (*SettlementCalendar, error) {
	if len(days) == 0 || len(days) > 4000 {
		return nil, errors.New("clearing calendar must contain 1..4000 explicit days")
	}
	c := &SettlementCalendar{days: make(map[string]ClearingDay, len(days))}
	var prior time.Time
	for _, day := range days {
		date, err := time.ParseInLocation(time.DateOnly, day.Date, wheel.Eastern)
		if err != nil || date.Format(time.DateOnly) != day.Date || date.Year() < 2020 || date.Year() > 2027 {
			return nil, errors.New("invalid campaign clearing date")
		}
		if !prior.IsZero() && !date.Equal(prior.AddDate(0, 0, 1)) {
			return nil, errors.New("clearing calendar must include every consecutive calendar day exactly once")
		}
		if day.Open {
			if date.Weekday() == time.Saturday || date.Weekday() == time.Sunday || day.AvailableAt.IsZero() || day.AvailableAt.In(wheel.Eastern).Format(time.DateOnly) != day.Date {
				return nil, errors.New("open clearing day has an invalid availability clock")
			}
		} else if !day.AvailableAt.IsZero() {
			return nil, errors.New("closed clearing day cannot release resources")
		}
		c.days[day.Date] = day
		prior = date
	}
	return c, nil
}

// Next returns the next reviewed availability clock, applying T+2 to stock and
// physical exercise before 2024-05-28, T+1 thereafter, and T+1 to option premiums.
// Sources: SEC release 2024-62 and OIC Understanding T+1 Conversion. Missing
// calendar evidence returns an error so callers must keep resources locked.
func (c *SettlementCalendar) Next(transactionAt time.Time, flow SettlementFlow) (time.Time, error) {
	if c == nil || transactionAt.IsZero() {
		return time.Time{}, errors.New("missing transaction or clearing calendar")
	}
	local := transactionAt.In(wheel.Eastern)
	date := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, wheel.Eastern)
	if _, known := c.days[date.Format(time.DateOnly)]; !known {
		return time.Time{}, errors.New("transaction is outside reviewed clearing calendar")
	}
	delay := 1
	switch flow {
	case OptionPremium:
	case StockTrade, PhysicalExercise:
		if date.Format(time.DateOnly) < "2024-05-28" {
			delay = 2
		}
	default:
		return time.Time{}, errors.New("unknown settlement flow")
	}
	for {
		date = date.AddDate(0, 0, 1)
		day, known := c.days[date.Format(time.DateOnly)]
		if !known {
			return time.Time{}, fmt.Errorf("missing clearing day %s; resources remain locked", date.Format(time.DateOnly))
		}
		if day.Open {
			delay--
			if delay == 0 {
				return day.AvailableAt, nil
			}
		}
	}
}
