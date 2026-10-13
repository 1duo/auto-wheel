package main

import "testing"

func TestRobinhoodFeesDatedAndRounded(t *testing.T) {
	f := portfolioFeeSchedule{Robinhood: true}
	for _, tc := range []struct {
		date                 string
		price, option, stock int64
	}{
		{"2026-04-03", 100, 4, 2},
		{"2026-04-04", 100, 5, 2}, // $100 stock sale SEC waived, TAF 2c.
		{"2026-04-06", 500, 6, 2}, // $500 stock sale SEC waived, options not.
		{"2026-04-06", 501, 6, 4},
		{"2026-09-30", 10000, 25, 23},
		{"2026-10-01", 10000, 25, 21},         // TAF pause changes stock fee only.
		{"2026-10-09", 1_000_000, 2064, 2060}, // Highest validated principal.
	} {
		if got := f.optionSale(tc.date, tc.price); got != tc.option {
			t.Errorf("%s option %d: got %d want %d", tc.date, tc.price, got, tc.option)
		}
		if got := f.stockDeliverySale(tc.date, tc.price); got != tc.stock {
			t.Errorf("%s stock %d: got %d want %d", tc.date, tc.price, got, tc.stock)
		}
	}
}

func TestRobinhoodAdmissionsReserveIndividualFees(t *testing.T) {
	accounts := map[string]*account{"A": {}, "B": {}}
	candidates := []portfolioCandidate{
		{Symbol: "A", Quote: quote{Kind: "put", Strike: 10000}, Fee: 5},
		{Symbol: "B", Quote: quote{Kind: "put", Strike: 10000}, Fee: 25},
	}
	if got := portfolioAdmissionsWithFees(candidates, accounts, 2_000_029, 15_000_000); len(got) != 1 {
		t.Fatal("second option must reserve its own fee before premium arrives")
	}
	if got := portfolioAdmissionsWithFees(candidates, accounts, 2_000_030, 15_000_000); len(got) != 2 {
		t.Fatal("exact cash boundary must fund both individual fees")
	}
}

func TestRobinhoodFeesRejectUnverifiedDatesAndOverrides(t *testing.T) {
	in := fixture()
	in.Days[0].Date = "2025-12-31"
	if _, err := simulatePortfolioUsingFees([]input{in}, profile{}, 8_404_717, portfolioFeeSchedule{Robinhood: true}, 0, 15_000_000, nil, nil, nil); err == nil {
		t.Fatal("2026 Robinhood schedule must not silently price another year")
	}
	if _, err := simulatePortfolioUsingFees([]input{fixture()}, profile{}, 8_404_717, portfolioFeeSchedule{Robinhood: true, Fixed: 6}, 0, 15_000_000, nil, nil, nil); err == nil {
		t.Fatal("flat fee override accepted")
	}
}
