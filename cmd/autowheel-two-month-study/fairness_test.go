package main

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"autowheel/internal/wheel"
)

func fixture() input {
	in := input{Symbol: "NVDA"}
	for _, dates := range [][2]string{{"2026-08-03", "2026-08-07"}, {"2026-08-10", "2026-08-14"}, {"2026-08-31", "2026-09-04"}} {
		at, _ := time.Parse(time.RFC3339, dates[0]+"T13:35:00Z")
		exp, _ := time.Parse("2006-01-02", dates[1])
		quotes := []quote{}
		for _, v := range []struct {
			strike   int64
			osi      string
			bid, ask int64
		}{{20250, "00202500", 24, 26}, {20500, "00205000", 37, 38}, {20750, "00207500", 62, 64}} {
			quotes = append(quotes, quote{Contract: "NVDA  " + exp.Format("060102") + "P" + v.osi, Kind: "put", Expiry: dates[1], Strike: v.strike, Bid: v.bid, Ask: v.ask, BidSize: 10, AskSize: 10,
				ReceiveNS: at.Add(-time.Millisecond).UnixNano(), EventNS: at.Add(-time.Millisecond).UnixNano(), Flags: 128})
		}
		s := sample{At: at, Stock: stock{At: at.Add(-time.Millisecond), Bid: 21725, Ask: 21730}, Quotes: quotes}
		e := execution{At: at.Add(time.Minute), NotBeforeNS: at.Add(time.Second).UnixNano()}
		for _, q := range quotes {
			q.ReceiveNS = at.Add(2 * time.Second).UnixNano()
			q.EventNS = q.ReceiveNS
			e.Books = append(e.Books, executionBook{Contract: q.Contract, Status: "observed_first_book", Quote: new(q)})
		}
		in.Days = append(in.Days, day{Date: dates[0], Entry: new(s), Execution: new(e), Close: s})
		if dates[0] == "2026-08-31" {
			in.Days = append(in.Days, day{Date: "2026-09-01", Close: s})
		}
		in.Days = append(in.Days, day{Date: dates[1], Close: sample{Stock: stock{Bid: 22000, Ask: 22005}}})
	}
	return in
}

func TestTrainingNeverReadsSeptember(t *testing.T) {
	in := fixture()
	p, g, r, err := chooseTraining(in, wheel.SelectionConservative)
	if err != nil {
		t.Fatal(err)
	}
	if len(g) != 18 || !g[0].Eligible {
		t.Fatal("fixture must exercise a priced, active training grid")
	}
	for i, d := range in.Days {
		if d.Date >= "2026-09-01" {
			in.Days[i] = day{Date: d.Date, Entry: new(sample{Stock: stock{Bid: -100}})}
		}
	}
	x, h, s, err := chooseTraining(in, wheel.SelectionConservative)
	if err != nil || p != x || !slices.Equal(g, h) || !reflect.DeepEqual(r, s) {
		t.Fatal("September influenced August choice", err)
	}
}

func TestFirstExecutionBookOnlyAndFixedLimit(t *testing.T) {
	d := fixture().Days[0]
	q := d.Entry.Quotes[0]
	o, ok := modelOrder(d, q, 1)
	if !ok || o.Limit != q.Bid-1 {
		t.Fatal("fixed adverse limit not filled")
	}
	d.Execution.Books[0].Quote.Bid = q.Bid - 2
	// A later favorable book cannot rescue the first adverse observation.
	good := q
	good.Bid = 1000
	good.Ask = 1001
	good.ReceiveNS = d.Entry.At.Add(3 * time.Second).UnixNano()
	good.EventNS = good.ReceiveNS
	d.Execution.Books = append(d.Execution.Books, executionBook{Contract: q.Contract, Status: "observed_first_book", Quote: new(good)})
	if _, ok := modelOrder(d, q, 1); ok {
		t.Fatal("skipped an adverse first book")
	}
	d.Execution.Books[0].Quote.Bid = 1000
	d.Execution.Books[0].Quote.Ask = 1001
	o, ok = modelOrder(d, q, 1)
	if !ok || o.Limit != q.Bid-1 {
		t.Fatal("granted hindsight price improvement")
	}
	d.Execution.Books[0].Quote.ReceiveNS = d.Entry.At.UnixNano()
	if _, ok := modelOrder(d, q, 1); ok {
		t.Fatal("pre-order quote filled order")
	}
}

func TestPostDecisionDataCannotChooseContract(t *testing.T) {
	d := fixture().Days[0]
	p := baseline(wheel.SelectionConservative)
	a, _, _, _, ok := selectContract("NVDA", p, 200, *d.Entry, 0)
	if !ok {
		t.Fatal("no fixture decision")
	}
	for _, b := range d.Execution.Books {
		b.Quote.Bid = 1
		b.Quote.Ask = 1000
	}
	b, _, _, _, ok := selectContract("NVDA", p, 200, *d.Entry, 0)
	if !ok || a != b {
		t.Fatal("execution quotes influenced contract choice")
	}
}

func TestContinuousBoundaryAndAmbiguousExpiry(t *testing.T) {
	in := fixture()
	b := baseline(wheel.SelectionConservative)
	c := b
	c.PutFloor = .1
	c.ID = "candidate"
	r, err := simulate(in, b, c, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	aug := r.Daily[len(r.Daily)-3]
	sep := r.Daily[len(r.Daily)-2]
	if aug.Cash != sep.Cash || aug.Liability != sep.Liability || sep.Liability <= 0 {
		t.Fatal("open August option or cash reset at September boundary")
	}
	a := account{Cash: initialCash, Short: new(quote{Contract: "NVDA  260904P00205000", Kind: "put", Expiry: "2026-09-04", Strike: 20500})}
	if a.settle("2026-09-04", stock{Bid: 20499, Ask: 20501}) == nil || a.Short == nil {
		t.Fatal("ambiguous expiry chose a favorable state")
	}
	if err := a.settle("2026-09-04", stock{Bid: 20000, Ask: 20001}); err != nil || a.Shares != 100 || a.Basis != 20500 {
		t.Fatal("ITM assignment not carried", err)
	}
}

func TestUnpricedProfilesHaveNoMetrics(t *testing.T) {
	in := fixture()
	in.Days[0].Close.Quotes = nil
	p, g, _, err := chooseTraining(in, wheel.SelectionConservative)
	if err != nil || p.ID != "baseline" {
		t.Fatal("unpriced grid promoted", err)
	}
	n := 0
	for _, m := range g {
		if m.CoverageError != "" {
			n++
			raw, _ := json.Marshal(m)
			var fields map[string]any
			_ = json.Unmarshal(raw, &fields)
			if _, ok := fields["net_pnl_cents"]; ok || m.Eligible {
				t.Fatal("fabricated unpriced metrics")
			}
		}
	}
	if n == 0 {
		t.Fatal("missing mark fixture did not exercise failures")
	}
}

func TestModeledPriceDoesNotReplaceObservedBook(t *testing.T) {
	in := fixture()
	in.Days = in.Days[:len(in.Days)-1]
	p := baseline(wheel.SelectionConservative)
	r, err := simulate(in, p, p, 200, 1)
	if err != nil || r.EndingShort == nil {
		t.Fatal("fixture must retain an actual quoted short", err)
	}
	last := r.Orders[len(r.Orders)-1]
	if r.EndingShort.Bid != last.DecisionBid || last.Limit != last.DecisionBid-1 {
		t.Fatal("modeled execution price replaced actual source bid")
	}
}
