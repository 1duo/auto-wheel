package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAvailablePeriodRejectsMissingOrMixedCalendars(t *testing.T) {
	inputs := []input{{Start: "2026-06-29", End: "2026-07-01", Days: []day{{Date: "2026-06-29"}, {Date: "2026-06-30"}, {Date: "2026-07-01"}}}}
	got, err := clipPortfolioInputs(inputs, "2026-06-29", "2026-06-30")
	if err != nil || len(got[0].Days) != 2 || got[0].End != "2026-06-30" || inputs[0].End != "2026-07-01" || len(inputs[0].Days) != 3 {
		t.Fatal("period clipping lost the frozen boundary or changed original input", err)
	}
	if _, err := clipPortfolioInputs(inputs, "2026-06-28", "2026-06-30"); err == nil {
		t.Fatal("missing requested boundary was silently shortened")
	}
	if _, err := clipPortfolioInputs(append(inputs, input{Days: []day{{Date: "2026-06-29"}}}), "2026-06-29", "2026-06-30"); err == nil {
		t.Fatal("inconsistent symbol calendars admitted")
	}
}

func TestAvailablePresetsCannotBypassCalendarYearActionGates(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "study-v2", "inputs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(input{Schema: "AutoWheelCalendarYearEndpointInputV1", Symbol: "NVDA"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "NVDA.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	err = runPortfolioScope(root, filepath.Join(root, "result.json"), 8_000_000, 15_000_000, true, "", "")
	if err == nil || !strings.Contains(err.Error(), "calendar-year action gates cannot be bypassed") {
		t.Fatal("calendar-year input admitted through the unqualified 2026 path", err)
	}
}

func TestAvailableUniverseIsBoundedAndDoesNotMutateDefault(t *testing.T) {
	got, err := availableStudySymbols(strings.Join(finished2026Symbols, ","))
	if err != nil || len(got) != 15 {
		t.Fatal("frozen expanded universe rejected", err)
	}
	for _, value := range []string{"NVDA,NVDA", "COIN,../../secret", "NVDA, COIN", "coin", "NVDA,", strings.Join(append(got, "NVDA"), ",")} {
		if _, err := availableStudySymbols(value); err == nil {
			t.Fatal("invalid research universe accepted", value)
		}
	}
	defaults, err := availableStudySymbols("")
	if err != nil || len(defaults) != 10 {
		t.Fatal("default universe changed", err)
	}
	defaults[0] = "COIN"
	if portfolioSymbols[0] != "NVDA" {
		t.Fatal("caller mutated default universe")
	}
	if err := runPortfolioUniverse("", "", 8_404_717, 15_000_000, false, "", "", "COIN", "conservative-roi-v1"); err == nil {
		t.Fatal("custom universe escaped available-data path")
	}
	if err := runPortfolioUniverse("", "", 8_404_717, 15_000_000, true, "", "", "COIN", "invented"); err == nil || !strings.Contains(err.Error(), "unknown frozen") {
		t.Fatal("unknown preset was accepted", err)
	}
}

func TestNOWOrderIncrementEffectiveDate(t *testing.T) {
	contract := "NOW   260702P00100000"
	for _, tc := range []struct {
		date        string
		price, want int64
	}{{"2026-06-30", 299, 5}, {"2026-06-30", 300, 10}, {"2026-07-01", 299, 1}, {"2026-07-01", 300, 5}} {
		if got := orderIncrement(tc.date, contract, tc.price); got != tc.want {
			t.Fatalf("%s at %d cents: increment %d, want %d", tc.date, tc.price, got, tc.want)
		}
	}
	if got := orderIncrement("2026-06-30", "NVDA  260702P00100000", 299); got != 1 {
		t.Fatal("NOW rule changed another symbol", got)
	}
	o, filled := modelOrder(day{Date: "2026-06-30", Entry: new(sample{})}, quote{Contract: contract, Bid: 55}, 1)
	if filled || o.Limit != 50 {
		t.Fatal("non-penny allowance was not rounded to a valid simple-order limit", o.Limit)
	}
}
