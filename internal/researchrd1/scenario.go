package researchrd1

import (
	"errors"
	"slices"
	"time"
)

// Scenario selects exactly one frozen sensitivity. It never changes the nine
// profiles, source admissions or transaction tariffs and cannot combine shocks.
type Scenario string

const (
	Primary          Scenario = "primary"
	ExecutionFive    Scenario = "execution_5m"
	ExecutionNext    Scenario = "execution_next_session"
	ExtraHalfSpread  Scenario = "extra_half_spread_cost"
	DailyITM         Scenario = "daily_ITM_assignment"
	PinExercise      Scenario = "pin_exercise"
	PinNoExercise    Scenario = "pin_no_exercise"
	AvailabilityNext Scenario = "settlement_plus_one_day"
)

func (s Scenario) Validate() error {
	if !slices.Contains([]Scenario{Primary, ExecutionFive, ExecutionNext, ExtraHalfSpread,
		DailyITM, PinExercise, PinNoExercise, AvailabilityNext}, s) {
		return errors.New("unknown or combined RD1 sensitivity")
	}
	return nil
}

func (s Scenario) AccountModel() Model {
	if s == AvailabilityNext {
		return Model{SettlementExtraDays: 1}
	}
	return Model{}
}

func (s Scenario) executionAt(a *Account, dayClose, signal time.Time) (time.Time, error) {
	if s == ExecutionNext {
		next, _, err := a.sessions.NextSession(signal)
		return next.CloseAt.Add(-30 * time.Minute), err
	}
	if s == ExecutionFive {
		return signal.Add(5 * time.Minute), nil
	}
	return dayClose.Add(-30 * time.Minute), nil
}

// policyPrice preserves the observed book and applies a separately labeled
// hypothetical price shock. Whole per-unit cents require adverse rounding of
// an odd-cent half-spread before the 100-share multiplier. NAV never uses this
// price, and contractual strike deliveries have no quote-spread surcharge.
func (s Scenario) policyPrice(b Book, buy bool) (int64, error) {
	price := b.Bid
	if buy {
		price = b.Ask
	}
	if s == ExtraHalfSpread {
		if !b.Valid() {
			return 0, errors.New("RD1 half-spread cost requires an observed noncrossed book")
		}
		extra := (b.Ask - b.Bid + 1) / 2
		if buy {
			price += extra
		} else {
			price -= extra
		}
	}
	if price > MaxMoney/100 || buy && price <= 0 {
		return 0, errors.New("RD1 hypothetical execution price outside money envelope")
	}
	return price, nil
}

func (s Scenario) resolution(c Contract, priceMicros int64) (string, error) {
	primary, err := expiryOutcome(c, priceMicros)
	if err != nil {
		return "", err
	}
	difference := priceMicros - c.Strike*10_000
	withinPin := max(difference, -difference)*1000 <= priceMicros
	if s == PinNoExercise && withinPin {
		return "expired", nil
	}
	if s == PinExercise && withinPin || s == DailyITM && (c.Kind == "call" && difference > 0 || c.Kind == "put" && difference < 0) {
		if c.Kind == "put" {
			return "assigned", nil
		}
		return "called_away", nil
	}
	return primary, nil
}
