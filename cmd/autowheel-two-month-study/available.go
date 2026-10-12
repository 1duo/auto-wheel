package main

import (
	"errors"
	"slices"
	"strings"
	"time"
)

var finished2026Symbols = []string{"NVDA", "IREN", "NOW", "AMD", "AMZN", "GOOGL", "META", "MSFT", "PLTR", "AVGO", "NBIS", "RKLB", "COIN", "MSTR", "HOOD"}

func availableStudySymbols(value string) ([]string, error) {
	if value == "" {
		return slices.Clone(portfolioSymbols), nil
	}
	symbols := strings.Split(value, ",")
	if len(symbols) > 15 {
		return nil, errors.New("available-data universe exceeds fifteen symbols")
	}
	seen := make(map[string]bool, len(symbols))
	for _, symbol := range symbols {
		if !slices.Contains(finished2026Symbols, symbol) || seen[symbol] {
			return nil, errors.New("available-data symbols must be distinct exact members of the frozen 2026 research universe")
		}
		seen[symbol] = true
	}
	return symbols, nil
}

// clipPortfolioInputs preserves the original inputs and rejects absent or
// inconsistent calendars. Validation of full source coverage happens first.
func clipPortfolioInputs(inputs []input, first, last string) ([]input, error) {
	start, startErr := time.Parse("2006-01-02", first)
	end, endErr := time.Parse("2006-01-02", last)
	if startErr != nil || endErr != nil || first != start.Format("2006-01-02") || last != end.Format("2006-01-02") || start.After(end) || len(inputs) == 0 {
		return nil, errors.New("invalid available-data period")
	}
	base := inputs[0].Days
	lo := slices.IndexFunc(base, func(d day) bool { return d.Date == first })
	hi := slices.IndexFunc(base, func(d day) bool { return d.Date == last })
	if lo < 0 || hi < lo {
		return nil, errors.New("requested period is not present in finished data")
	}
	result := slices.Clone(inputs)
	for i, in := range inputs {
		if len(in.Days) != len(base) {
			return nil, errors.New("inconsistent portfolio calendars")
		}
		for j, d := range in.Days {
			if d.Date != base[j].Date {
				return nil, errors.New("inconsistent portfolio calendars")
			}
		}
		result[i].Start, result[i].End = first, last
		result[i].Days = in.Days[lo : hi+1]
	}
	return result, nil
}
