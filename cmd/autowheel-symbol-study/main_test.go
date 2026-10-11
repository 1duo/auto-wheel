package main

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"autowheel/internal/wheel"
)

func TestUnpricedTrainingProfileCannotPromoteOrInventMetrics(t *testing.T) {
	in := syntheticTraining()
	for _, d := range in.Days {
		if d.Entry != nil {
			d.Entry.Quotes[0].Ask = 27
		}
	}
	// The tighter spread can choose a different put. Remove only that put's
	// entry-day close, while preserving all entry books and the baseline mark.
	p := baseline(wheel.SelectionConservative)
	p.SpreadBPS = 1000
	q, _, _, _, ok := selectContract(in.Symbol, p, 200, *in.Days[0].Entry, 0)
	b, _, _, _, baseOK := selectContract(in.Symbol, baseline(wheel.SelectionConservative), 200, *in.Days[0].Entry, 0)
	if !ok || !baseOK || q.Contract == b.Contract {
		t.Fatal("fixture must select distinct tight-spread and baseline contracts")
	}
	in.Days[0].Close.Quotes = slices.DeleteFunc(slices.Clone(in.Days[0].Close.Quotes), func(v quote) bool { return v.Contract == q.Contract })
	chosen, grid, err := chooseTraining(in, wheel.SelectionConservative)
	if err != nil || chosen.ID != "baseline" || len(grid) != 18 {
		t.Fatalf("incomplete alternatives must retain a covered baseline: %v %+v", err, chosen)
	}
	missing := 0
	for _, m := range grid {
		if m.CoverageError == "" {
			continue
		}
		missing++
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if _, present := fields["net_pnl_cents"]; present || m.Eligible {
			t.Fatal("unpriced profiles must not have invented P&L or be eligible")
		}
	}
	if missing == 0 {
		t.Fatal("fixture must exercise missing-mark coverage")
	}
}

func syntheticTraining() input {
	in := input{Symbol: "NVDA"}
	for _, dates := range [][2]string{{"2026-09-01", "2026-09-04"}, {"2026-09-08", "2026-09-11"}, {"2026-09-14", "2026-09-18"}} {
		at, _ := time.Parse(time.RFC3339, dates[0]+"T13:35:00Z")
		expiry, _ := time.Parse("2006-01-02", dates[1])
		quotes := []quote{}
		for _, values := range [][3]int64{{20250, 24, 26}, {20500, 37, 38}, {20750, 62, 64}} {
			contract := "NVDA  " + expiry.Format("060102") + "P00"
			// These are explicitly synthetic fixture books, not study evidence.
			if values[0] == 20250 {
				contract += "202500"
			} else if values[0] == 20500 {
				contract += "205000"
			} else {
				contract += "207500"
			}
			quotes = append(quotes, quote{Contract: contract, Kind: "put", Expiry: dates[1], Strike: values[0], Bid: values[1], Ask: values[2], BidSize: 10, AskSize: 10, ReceiveNS: at.UnixNano(), EventNS: at.UnixNano()})
		}
		s := sample{At: at, Stock: stock{At: at, Bid: 21725, Ask: 21730}, Quotes: quotes}
		in.Days = append(in.Days, day{Date: dates[0], Entry: new(s), Close: s})
		in.Days = append(in.Days, day{Date: dates[1], Close: sample{Stock: stock{Bid: 22000, Ask: 22005}}})
	}
	return in
}

func TestTrainingCannotReadValidationWeeks(t *testing.T) {
	in := syntheticTraining()
	winner, grid, err := chooseTraining(in, wheel.SelectionConservative)
	if err != nil {
		t.Fatal(err)
	}
	if len(grid) != 18 || !grid[0].Eligible {
		t.Fatalf("fixture must exercise a populated training grid: %+v", grid)
	}
	// Invalid held-out inventory would fail simulation if the optimizer read it.
	in.Days = append(in.Days, day{Date: "2026-09-21", Entry: new(sample{Stock: stock{Bid: -1}})})
	after, newGrid, err := chooseTraining(in, wheel.SelectionConservative)
	if err != nil || winner != after || !slices.Equal(grid, newGrid) {
		t.Fatalf("validation data leaked into training choice: %v", err)
	}
}

func TestInactiveProfileCannotBeatObservedTrading(t *testing.T) {
	active := metrics{Profile: baseline(wheel.SelectionBalanced), Eligible: true, Score: -100}
	inactive := metrics{Profile: baseline(wheel.SelectionBalanced), Eligible: false, Score: 0}
	if trainingCompare(active, inactive) >= 0 {
		t.Fatal("cash-only profile must not win by avoiding every entry")
	}
}

func TestUnobservedOrSlowerCallExitCannotPromote(t *testing.T) {
	base := result{Daily: []point{{Shares: 100}, {Shares: 0}}}
	unobserved := result{Daily: base.Daily}
	if callValidation(base, unobserved, true) {
		t.Fatal("put performance cannot validate an unexercised call setting")
	}
	slower := result{Trades: []trade{{Event: "modeled_sell_call"}}, Daily: []point{{Shares: 100}, {Shares: 100}}, EndingShares: 100}
	if callValidation(base, slower, true) {
		t.Fatal("exit tuning must not retain stock longer or leave extra shares")
	}
	observed := result{Trades: slower.Trades, Daily: base.Daily}
	if !callValidation(base, observed, true) {
		t.Fatal("observed exit with no additional holding time must be eligible")
	}
}

func TestValuationOnlyAskCannotAuthorizeSale(t *testing.T) {
	at := time.Date(2026, 9, 17, 20, 0, 0, 0, time.UTC)
	m := valuationMark{Contract: "NBIS  260918P00180000", Ask: 3, AskSize: 33,
		ReceiveNS: at.Add(-time.Minute).UnixNano(), EventNS: at.Add(-time.Minute).UnixNano(), Reason: "extended_window_observation"}
	s := sample{At: at, Stock: stock{At: at, Bid: 20000, Ask: 20005}, Marks: []valuationMark{m}}
	if err := validateValuationMarks(s, "NBIS"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, ok := selectContract("NBIS", baseline(wheel.SelectionBalanced), 100, s, 0); ok {
		t.Fatal("a valuation ask with no bid must never authorize a sale")
	}
	a := account{Cash: initialCash, Short: new(quote{Contract: m.Contract, Kind: "put", Strike: 18000})}
	p, err := a.mark(day{Date: "2026-09-17", Close: s})
	if err != nil || p.Liability != 300 || p.Equity != initialCash-300 {
		t.Fatalf("observed closing ask must mark the existing short exactly: %+v %v", p, err)
	}
	s.Marks[0].EventNS = at.Add(-6 * time.Minute).UnixNano()
	if validateValuationMarks(s, "NBIS") == nil {
		t.Fatal("stale observations must stay explicit gaps")
	}
}

func TestCrossSymbolBookRejectedAndAssignmentLossAccounted(t *testing.T) {
	at := time.Date(2026, 9, 1, 13, 35, 0, 0, time.UTC)
	s := sample{At: at, Stock: stock{At: at, Bid: 21725, Ask: 21730}, Quotes: []quote{{Contract: "NVDA  260904P00210000", Kind: "put", Expiry: "2026-09-04", Strike: 21000, Bid: 103, Ask: 106, BidSize: 1, AskSize: 1, ReceiveNS: at.UnixNano(), EventNS: at.UnixNano()}}}
	if validateSample(s, "NVDA") != nil || validateSample(s, "TSLA") == nil {
		t.Fatal("symbol identity must be enforced")
	}
	a := account{Cash: initialCash}
	if err := a.sell("2026-09-01", s.Quotes[0], 100, -.2, .4); err != nil {
		t.Fatal(err)
	}
	if err := a.settle("2026-09-04", 20000); err != nil {
		t.Fatal(err)
	}
	call := quote{Contract: "NVDA  260911C00200000", Kind: "call", Expiry: "2026-09-11", Strike: 20000, Bid: 100}
	if err := a.sell("2026-09-08", call, 100, .8, .4); err != nil {
		t.Fatal(err)
	}
	if err := a.settle("2026-09-11", 20500); err != nil {
		t.Fatal(err)
	}
	if a.Cash-initialCash != -79_900 || a.StockRealized != -100_000 || a.Premiums != 20_300 || a.Fees != 200 {
		t.Fatalf("call-away must include the stock loss: %+v", a)
	}
}
