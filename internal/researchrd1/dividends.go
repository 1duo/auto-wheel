package researchrd1

import (
	"cmp"
	"errors"
	"slices"

	"autowheel/internal/researchcampaign"
	"autowheel/internal/wheel"
)

func validateDividends(terms []ScheduledDividend, sessions *researchcampaign.SessionCalendar) error {
	seen := map[string]bool{}
	for _, d := range terms {
		if d.ID == "" || seen[d.ID] || d.Symbol == "" || d.ExAt.IsZero() || d.KnownAt.IsZero() || d.KnownAt.After(d.ExAt) || d.Amount <= 0 || d.Amount > MaxMoney/100 || d.PayAt.Before(d.ExAt) || d.AvailableAt.Before(d.PayAt) || !sourceHash(d.SourceSHA) {
			return errors.New("RD1 dividend terms unknown/unsupported")
		}
		day, _, err := sessions.Session(d.ExAt)
		if err != nil || !d.ExAt.Equal(day.OpenAt) {
			return errors.New("RD1 ex-dividend event is not at reviewed session open")
		}
		pay := d.PayAt.In(wheel.Eastern)
		if pay.Hour() != 9 || pay.Minute() != 30 || pay.Second() != 0 || pay.Nanosecond() != 0 {
			return errors.New("RD1 packet supports only declared 09:30 modeled dividend payment clocks")
		}
		seen[d.ID] = true
	}
	return nil
}

func dividendEffects(terms []ScheduledDividend, day researchcampaign.ExchangeDay, state State) ([]Effect, error) {
	byID := make(map[string]ScheduledDividend, len(terms))
	for _, d := range terms {
		byID[d.ID] = d
	}
	var effects []Effect
	for id, d := range state.Dividends {
		if d.Paid || d.PayAt.After(day.OpenAt) {
			continue
		}
		term, known := byID[id]
		if !known || term.Symbol != d.Symbol || term.Amount != d.Amount || !term.PayAt.Equal(d.PayAt) || !sourceHash(term.SourceSHA) || d.PayAt.Before(state.At) {
			return nil, errors.New("RD1 overdue or source-unbound dividend payment")
		}
		effects = append(effects, Effect{Ref: "payment-" + id, Action: "pay_dividend", Symbol: d.Symbol, At: d.PayAt, Price: d.Amount, DividendID: id, SourceSHA: term.SourceSHA})
	}
	for _, d := range terms {
		if d.ExAt.Equal(day.OpenAt) && state.Positions[d.Symbol].Shares == 100 {
			effects = append(effects, Effect{Ref: "entitlement-" + d.ID, Action: "dividend", Symbol: d.Symbol, At: d.ExAt, Price: d.Amount, DividendID: d.ID, PayAt: d.PayAt, AvailableAt: d.AvailableAt, SourceSHA: d.SourceSHA})
			if d.PayAt.Equal(d.ExAt) {
				effects = append(effects, Effect{Ref: "payment-" + d.ID, Action: "pay_dividend", Symbol: d.Symbol, At: d.PayAt, Price: d.Amount, DividendID: d.ID, SourceSHA: d.SourceSHA})
			}
		}
	}
	slices.SortStableFunc(effects, func(a, b Effect) int {
		return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.Ref, b.Ref))
	})
	return effects, nil
}
