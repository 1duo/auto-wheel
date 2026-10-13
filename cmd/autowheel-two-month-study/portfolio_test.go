package main

import (
	"reflect"
	"slices"
	"testing"

	"autowheel/internal/wheel"
)

func TestPortfolioAdmissionsSecureSharedCashAndSlots(t *testing.T) {
	accounts := map[string]*account{"A": {}, "B": {}, "C": {}, "D": {}}
	all := []portfolioCandidate{}
	for _, symbol := range []string{"A", "B", "C", "D"} {
		all = append(all, portfolioCandidate{Symbol: symbol, Quote: quote{Kind: "put", Strike: 20000}, Numerator: 2000, Denominator: 2000000})
	}
	if got := portfolioAdmissions(all, accounts, 6_000_000, 6, 8_000_000); len(got) != 2 {
		t.Fatalf("upfront fees must block third put: %d", len(got))
	}
	if got := portfolioAdmissions(all, accounts, 8_500_000, 6, 8_000_000); len(got) != 3 {
		t.Fatalf("three slots required, got %d", len(got))
	}
	accounts["D"].Shares, accounts["D"].Basis = 100, 10000
	if got := portfolioAdmissions(all, accounts, 8_500_000, 6, 8_000_000); len(got) != 2 {
		t.Fatalf("uncovered assigned inventory must occupy a slot: %d", len(got))
	}
	accounts["D"].Shares, accounts["D"].Basis = 0, 0
	accounts["D"].Short = &quote{Kind: "put", Strike: 70000}
	if got := portfolioAdmissions(all, accounts, 8_500_000, 6, 8_000_000); len(got) != 0 {
		t.Fatal("shared cash/collateral reservation ignored")
	}
}

func TestPortfolioROIRankingAndExactDividend(t *testing.T) {
	all := []portfolioCandidate{{Symbol: "A", Numerator: 300, Denominator: 10000}, {Symbol: "B", Numerator: 400, Denominator: 20000}, {Symbol: "C", Numerator: 900, Denominator: 10000}}
	slices.SortFunc(all, portfolioROICompare)
	if all[0].Symbol != "C" || all[1].Symbol != "A" {
		t.Fatal("rank must compare ROI, not dollar premium")
	}
	n, err := portfolioDividendCents(portfolioDividend{Amount: "0.525", ExDate: "2026-06-15", PayDate: "2026-06-25"}, 100)
	if err != nil || n != 5250 {
		t.Fatalf("exact fractional-cent dividend: %d %v", n, err)
	}
	if _, err := portfolioDividendCents(portfolioDividend{Amount: "0.52501"}, 100); err == nil {
		t.Fatal("inexact monetary amount accepted")
	}
}

func TestPortfolioHigherCeilingDoesNotCreateCash(t *testing.T) {
	accounts := map[string]*account{"A": {}, "B": {}}
	all := []portfolioCandidate{{Symbol: "A", Quote: quote{Kind: "put", Strike: 40000}}, {Symbol: "B", Quote: quote{Kind: "put", Strike: 44000}}}
	if got := portfolioAdmissions(all, accounts, 8_500_000, 6, 8_000_000); len(got) != 1 {
		t.Fatal("old collateral ceiling ignored")
	}
	if got := portfolioAdmissions(all, accounts, 8_500_000, 6, 15_000_000); len(got) != 2 {
		t.Fatal("requested ceiling should permit funded trades")
	}
	all[1].Quote.Strike = 45000
	if got := portfolioAdmissions(all, accounts, 8_500_000, 6, 15_000_000); len(got) != 1 {
		t.Fatal("raised ceiling must not fund unsecured puts")
	}
}

func TestPortfolioSymbolLimitMatchesNativeFeeAndPreservesOwnedExit(t *testing.T) {
	accounts := map[string]*account{"A": {}, "B": {}, "C": {Shares: 100, Basis: 40000}}
	all := []portfolioCandidate{
		{Symbol: "A", Quote: quote{Kind: "put", Strike: 27000}, Fee: 6},
		{Symbol: "A", Quote: quote{Kind: "put", Strike: 26000}, Fee: 6},
		{Symbol: "B", Quote: quote{Kind: "put", Strike: 20000}, Fee: 6},
		{Symbol: "C", Quote: quote{Kind: "call", Strike: 35000}, Fee: 6},
	}
	limit := int64(wheel.DefaultConfig().SymbolCollateralLimit())
	got := portfolioAdmissionsWithLimits(all, accounts, 8_000_000, 8_000_000, limit)
	if len(got) != 2 || got[0].Quote.Strike != 26000 || got[1].Quote.Kind != "call" {
		t.Fatal("symbol cap or aggregate budget bypassed, or owned exit blocked", got)
	}
	accounts["C"].Shares = 0
	got = portfolioAdmissionsWithLimits(all[:2], accounts, 8_000_000, 8_000_000, 2_600_006)
	if len(got) != 1 || got[0].Quote.Strike != 26000 {
		t.Fatal("exact symbol fee boundary rejected")
	}
	if got := portfolioAdmissionsWithLimits(all[:2], accounts, 8_000_000, 8_000_000, 2_600_005); len(got) != 0 {
		t.Fatal("entry fee bypassed symbol budget")
	}
}

func TestPortfolioSelectionIgnoresExecutionAndFutureData(t *testing.T) {
	in := fixture()
	p := baseline(wheel.SelectionConservative)
	a := &account{}
	before := portfolioCandidates(in, 0, a, p, 6, 0)
	if len(before) == 0 {
		t.Fatal("fixture must have eligible ROI candidates")
	}
	in.Days[0].Close = sample{}
	in.Days[0].Execution = nil
	for i := 1; i < len(in.Days); i++ {
		in.Days[i] = day{}
	}
	in.ReferenceCloses = nil
	in.ExpiryCloses = nil
	after := portfolioCandidates(in, 0, a, p, 6, 0)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("execution, same-day close or future data influenced the decision")
	}
}
