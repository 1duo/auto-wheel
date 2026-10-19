package researchcampaign

import (
	"errors"
	"strings"
	"time"

	"autowheel/internal/wheel"
)

// SaleFeeRule describes an explicitly bound, cumulative order fee model. The
// rational components charge cents per notional cent and cents per share,
// respectively. Components round once on cumulative fills. Empty ShareRounding
// preserves upward rounding; "nearest" rejects an unspecified half-cent tie.
// The share component is capped when ShareCap is nonzero. Partial fills pay only the
// increase since the prior fill. This is not a default broker tariff: the dated
// rates, aggregation rule and applicability need independent source review.
type SaleFeeRule struct {
	FlatFee             wheel.Cents `json:"flat_fee_cents"`
	NotionalNumerator   int64       `json:"notional_numerator"`
	NotionalDenominator int64       `json:"notional_denominator"`
	NotionalWaiver      wheel.Cents `json:"notional_waiver_cents,omitzero"`
	ShareNumerator      int64       `json:"share_numerator"`
	ShareDenominator    int64       `json:"share_denominator"`
	ShareCap            wheel.Cents `json:"share_cap_cents"`
	ShareRounding       string      `json:"share_rounding,omitempty"`
	WaiveSubcentShare   bool        `json:"waive_subcent_share,omitzero"`
}

type DatedFees struct {
	Date             string       `json:"date"`
	KnownAt          time.Time    `json:"known_at"`
	SourceSHA        string       `json:"source_sha256"`
	OptionOpen       wheel.Cents  `json:"option_open_cents"`
	OptionSale       *SaleFeeRule `json:"option_sale,omitempty"`
	OptionClose      wheel.Cents  `json:"option_close_cents"`
	PhysicalExercise wheel.Cents  `json:"physical_exercise_cents"`
	StockSale        SaleFeeRule  `json:"stock_sale"`
}

// FeeSchedule has no fallback tariff. A receipt hash binds a source; it does not
// prove its publication clock, broker applicability or aggregation semantics.
type FeeSchedule struct {
	days map[string]DatedFees
}

func NewFeeSchedule(days []DatedFees) (*FeeSchedule, error) {
	if len(days) == 0 || len(days) > 4000 {
		return nil, errors.New("fee schedule requires 1..4000 explicit consecutive days")
	}
	f := &FeeSchedule{days: make(map[string]DatedFees, len(days))}
	var prior time.Time
	for _, day := range days {
		date, err := time.ParseInLocation(time.DateOnly, day.Date, wheel.Eastern)
		if err != nil || date.Format(time.DateOnly) != day.Date || date.Year() < 2020 || date.Year() > 2027 || (!prior.IsZero() && !date.Equal(prior.AddDate(0, 0, 1))) || day.KnownAt.IsZero() || len(day.SourceSHA) != 64 || strings.Trim(day.SourceSHA, "0123456789abcdef") != "" || day.OptionOpen < 0 || day.OptionOpen > 10000 || day.OptionClose < 0 || day.OptionClose > 10000 || day.PhysicalExercise < 0 || day.PhysicalExercise > 10000 {
			return nil, errors.New("invalid, missing or ambiguous dated fees")
		}
		if err := day.StockSale.Validate(); err != nil {
			return nil, err
		}
		if day.OptionSale != nil {
			if day.OptionOpen != 0 {
				return nil, errors.New("option opening fees require one unambiguous flat or sale rule")
			}
			if err := day.OptionSale.Validate(); err != nil {
				return nil, err
			}
			day.OptionSale = new(*day.OptionSale)
		}
		f.days[day.Date] = day
		prior = date
	}
	return f, nil
}

func (f *FeeSchedule) At(at time.Time) (DatedFees, error) {
	if f == nil || at.IsZero() {
		return DatedFees{}, errors.New("missing fee schedule or clock")
	}
	day, known := f.days[at.In(wheel.Eastern).Format(time.DateOnly)]
	if !known || day.KnownAt.After(at) {
		return DatedFees{}, errors.New("fees missing or not yet causally known")
	}
	if day.OptionSale != nil {
		day.OptionSale = new(*day.OptionSale)
	}
	return day, nil
}

// OpeningFee applies to exactly one short contract at its gross fill premium.
// OptionSale uses one contract as the unit for its per-unit component. This
// describes a supplied dated rule, without qualifying its source or interval.
func (f DatedFees) OpeningFee(gross wheel.Cents) (wheel.Cents, error) {
	if gross <= 0 || gross > wheel.MaxMoney {
		return 0, errors.New("invalid one-contract opening premium")
	}
	if f.OptionSale == nil {
		if f.OptionOpen < 0 || f.OptionOpen > 10000 {
			return 0, errors.New("invalid flat opening fee")
		}
		return f.OptionOpen, nil
	}
	if f.OptionOpen != 0 {
		return 0, errors.New("ambiguous opening fee rules")
	}
	return f.OptionSale.Cumulative(1, gross)
}

func (f SaleFeeRule) Validate() error {
	if f.FlatFee < 0 || f.FlatFee > 10000 || f.ShareCap < 0 || f.ShareCap > 10000 || f.NotionalWaiver < 0 || f.NotionalWaiver > wheel.MaxMoney || f.NotionalNumerator < 0 || f.NotionalNumerator > 1_000_000 || f.ShareNumerator < 0 || f.ShareNumerator > 1_000_000 || f.NotionalDenominator < 1 || f.NotionalDenominator > 1_000_000_000_000 || f.ShareDenominator < 1 || f.ShareDenominator > 1_000_000_000_000 || (f.ShareRounding != "" && f.ShareRounding != "nearest") || (f.WaiveSubcentShare && f.ShareRounding != "nearest") {
		return errors.New("invalid bounded stock fee rule")
	}
	return nil
}

func (f SaleFeeRule) Cumulative(shares int, notional wheel.Cents) (wheel.Cents, error) {
	if err := f.Validate(); err != nil {
		return 0, err
	}
	if shares < 0 || shares > 100 || notional < 0 || notional > wheel.MaxMoney || (shares == 0) != (notional == 0) {
		return 0, errors.New("invalid cumulative stock fills")
	}
	if shares == 0 {
		return 0, nil
	}
	// MaxMoney * the bounded numerator is at most 10^17, within int64.
	ceil := func(value, denominator int64) wheel.Cents {
		quotient := value / denominator
		if value%denominator != 0 {
			quotient++
		}
		return wheel.Cents(quotient)
	}
	notionalFee := ceil(int64(notional)*f.NotionalNumerator, f.NotionalDenominator)
	if notional <= f.NotionalWaiver {
		notionalFee = 0
	}
	shareFee := ceil(int64(shares)*f.ShareNumerator, f.ShareDenominator)
	if f.ShareRounding == "nearest" {
		value := int64(shares) * f.ShareNumerator
		if f.WaiveSubcentShare && value < f.ShareDenominator {
			shareFee = 0
		} else {
			quotient, remainder := value/f.ShareDenominator, value%f.ShareDenominator
			if remainder*2 == f.ShareDenominator {
				return 0, errors.New("nearest-cent fee has an unqualified half-cent tie")
			}
			if remainder*2 > f.ShareDenominator {
				quotient++
			}
			shareFee = wheel.Cents(quotient)
		}
	}
	if f.ShareCap > 0 {
		shareFee = min(shareFee, f.ShareCap)
	}
	if notionalFee > wheel.MaxMoney-f.FlatFee || shareFee > wheel.MaxMoney-f.FlatFee-notionalFee {
		return 0, errors.New("stock fee exceeds money envelope")
	}
	return f.FlatFee + notionalFee + shareFee, nil
}
