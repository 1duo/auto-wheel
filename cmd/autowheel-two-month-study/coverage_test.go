package main

import (
	"reflect"
	"testing"

	"autowheel/internal/wheel"
)

func endpointFixture() input {
	in := fixture()
	in.ExpiryCloses = map[string]officialClose{}
	for _, d := range in.Days {
		if d.Entry == nil && d.Date != "2026-09-01" {
			in.ExpiryCloses[d.Date] = officialClose{Native: 22000 * 10_000_000}
		}
	}
	return in
}

func TestEndpointMissingInteriorMarkNeverChangesTrades(t *testing.T) {
	in := endpointFixture()
	b, err := simulateEndpoint(in, baseline(wheel.SelectionConservative), 100, 0)
	if err != nil || !b.EndpointComplete || !b.DailyComplete {
		t.Fatal("fixture must be priced", err)
	}
	in.Days[0].Close.Quotes = nil
	c, err := simulateEndpoint(in, baseline(wheel.SelectionConservative), 100, 0)
	if err != nil || !c.EndpointComplete || c.DailyComplete || c.Drawdown != nil || c.DrawdownPct != nil {
		t.Fatal("missing daily mark was invented", err)
	}
	if c.Daily[0].Liability != nil || c.Daily[0].Equity != nil || c.Daily[0].Missing == "" {
		t.Fatal("unknown daily equity must be null")
	}
	if !reflect.DeepEqual(b.Trades, c.Trades) || !reflect.DeepEqual(b.Orders, c.Orders) || *b.PnL != *c.PnL || c.Cash != b.Cash {
		t.Fatal("interior reporting gap changed strategy/accounting")
	}
	if c.Periods["2026-08"].Drawdown != nil || c.Periods["2026-08"].PnL == nil {
		t.Fatal("monthly boundary return and daily drawdown must be distinguished")
	}
}

func TestEndpointMissingFinalMarkCannotInventProfit(t *testing.T) {
	in := endpointFixture()
	in.Days = in.Days[:len(in.Days)-1]
	in.Days[len(in.Days)-1].Close.Quotes = nil
	r, err := simulateEndpoint(in, baseline(wheel.SelectionConservative), 100, 0)
	if err != nil || r.EndpointComplete || r.PnL != nil || r.EndEquity != nil || r.Return != nil || r.Liability != nil || r.PnLReconciled {
		t.Fatal("missing endpoint produced a return", err)
	}
	if !r.Accounting || r.Cash <= initialCash {
		t.Fatal("cash/premium ledger should still be known")
	}
}

func TestOfficialCloseCannotChooseEarlierOrder(t *testing.T) {
	in := endpointFixture()
	b, err := simulateEndpoint(in, baseline(wheel.SelectionConservative), 100, 0)
	if err != nil || len(b.Orders) == 0 {
		t.Fatal("fixture must trade", err)
	}
	ref := in.ExpiryCloses["2026-08-07"]
	ref.Native = 10000 * 10_000_000
	in.ExpiryCloses["2026-08-07"] = ref
	c, err := simulateEndpoint(in, baseline(wheel.SelectionConservative), 100, 0)
	if err != nil || len(c.Orders) == 0 || !reflect.DeepEqual(b.Orders[0], c.Orders[0]) || !reflect.DeepEqual(b.Trades[0], c.Trades[0]) {
		t.Fatal("later official close affected earlier selection/fill", err)
	}
	if c.Assignments == 0 {
		t.Fatal("reference must affect subsequent assignment state")
	}
}

func TestOfficialCloseKeepsSubcentExerciseThreshold(t *testing.T) {
	in := endpointFixture()
	q, _, _, _, ok := selectContract(in.Symbol, baseline(wheel.SelectionConservative), 100, *in.Days[0].Entry, 0)
	if !ok {
		t.Fatal("fixture must choose a put")
	}
	ref := in.ExpiryCloses["2026-08-07"]
	ref.Native = q.Strike*10_000_000 - 5_000_000
	in.ExpiryCloses["2026-08-07"] = ref
	a, err := simulateEndpoint(in, baseline(wheel.SelectionConservative), 100, 0)
	if err != nil || a.Assignments != 0 {
		t.Fatal("half-cent ITM must not be rounded into automatic exercise", err)
	}
	ref.Native = q.Strike*10_000_000 - 15_000_000
	in.ExpiryCloses["2026-08-07"] = ref
	b, err := simulateEndpoint(in, baseline(wheel.SelectionConservative), 100, 0)
	if err != nil || b.Assignments != 1 {
		t.Fatal("one-and-a-half-cent ITM must meet the threshold", err)
	}
}
