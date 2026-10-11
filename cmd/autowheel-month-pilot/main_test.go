package main

import "testing"

func TestAssignmentBelowBasisCallAwayAccounting(t *testing.T) {
	a := account{Cash: initialCash}
	put := quote{Contract: "NVDA  260904P00210000", Kind: "put", Expiry: "2026-09-04", Strike: 21000, Bid: 103}
	if err := a.sell("2026-09-01", put, 100, -.2, .7); err != nil {
		t.Fatal(err)
	}
	if err := a.settle("2026-09-04", 20500); err != nil {
		t.Fatal(err)
	}
	if a.Cash != 5_910_200 || a.Shares != 100 || a.Basis != 21000 || a.Assignments != 1 {
		t.Fatalf("assignment state: %+v", a)
	}
	call := quote{Contract: "NVDA  260911C00205000", Kind: "call", Expiry: "2026-09-11", Strike: 20500, Bid: 110}
	if err := a.sell("2026-09-08", call, 100, .8, .7); err != nil {
		t.Fatal(err)
	}
	if err := a.settle("2026-09-11", 20600); err != nil {
		t.Fatal(err)
	}
	if a.Cash-initialCash != -28_900 || a.Premiums != 21_300 || a.Fees != 200 || a.StockRealized != -50_000 || a.Shares != 0 || a.CallAways != 1 || a.Short != nil {
		t.Fatalf("positive premiums must still report the stock loss: %+v", a)
	}
	cash := initialCash
	for _, trade := range a.Trades {
		cash += trade.Cashflow
	}
	if cash != a.Cash {
		t.Fatal("trade cashflows do not reconcile")
	}
}

func TestEndMonthAskLiabilityAndMissingMark(t *testing.T) {
	a := account{Cash: initialCash}
	q := quote{Contract: "NVDA  261002P00210000", Kind: "put", Expiry: "2026-10-02", Strike: 21000, Bid: 100, Ask: 130}
	if err := a.sell("2026-09-28", q, 100, -.2, .5); err != nil {
		t.Fatal(err)
	}
	d := day{Date: "2026-09-30", Close: sample{Stock: stock{Bid: 22000, Ask: 22005}, Quotes: []quote{q}}}
	p, err := a.mark(d)
	if err != nil {
		t.Fatal(err)
	}
	if p.Equity-initialCash != -3100 || p.Liability != 13000 || p.Committed != 2_100_000 || p.Available != 5_909_900 {
		t.Fatalf("open premium must not become profit without its ask liability: %+v", p)
	}
	d.Close.Quotes = nil
	if _, err = a.mark(d); err == nil {
		t.Fatal("missing held-short mark must fail, not become zero liability")
	}
}
