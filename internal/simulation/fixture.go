package simulation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"autowheel/internal/wheel"
)

// AccountingFixture is a deliberately invented five-session path. Its purpose
// is to catch premium-only accounting, ownership delays and call-ranking errors.
// It is never a source of six-month or ticker performance estimates.
func AccountingFixture() io.Reader {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	spec := Spec{Schema: "AutoWheelReplaySpecV1", Mode: "fixture", Start: "2026-04-06", End: "2026-04-10", InitialCash: 8_000_000, Source: "Invented deterministic accounting fixture; no historical observations", CorporateActionsAudited: true, Config: wheel.DefaultConfig()}
	e.Encode(spec)
	day, _ := time.ParseInLocation("2006-01-02", spec.Start, wheel.Eastern)
	for index := range 5 {
		d := day.AddDate(0, 0, index)
		for minute := 575; minute < 945; minute += 5 {
			at := d.Add(time.Duration(minute) * time.Minute)
			f := fixtureFrame(at, "decision")
			f.CompleteSymbols = spec.Config.Symbols
			e.Encode(f)
			if minute == 575 && (index == 0 || index == 2) {
				for second := 1; second <= 6; second++ {
					f := fixtureFrame(at.Add(time.Duration(second)*time.Second), "market")
					e.Encode(f)
				}
			}
		}
		f := fixtureFrame(d.Add(16*time.Hour), "valuation")
		if index == 0 {
			f.Lifecycles = []Lifecycle{{OptionID: "fixture-NVDA-put", Outcome: "assigned", SettledAt: nextSettlement(f.At), Assumption: "invented early put assignment to test physical settlement"}}
		}
		if index == 3 {
			f.Lifecycles = []Lifecycle{{OptionID: "fixture-NVDA-call", Outcome: "called_away", SettledAt: nextSettlement(f.At), Assumption: "invented expiry assignment at a below-cost strike"}}
		}
		e.Encode(f)
	}
	return &b
}

func fixtureFrame(at time.Time, kind string) Frame {
	f := Frame{At: at, Kind: kind}
	listed := time.Date(2026, 4, 2, 9, 30, 0, 0, wheel.Eastern)
	for _, symbol := range wheel.DefaultConfig().Symbols {
		spot := wheel.Cents(10500)
		if at.In(wheel.Eastern).Day() >= 7 || at.In(wheel.Eastern).Hour() >= 16 {
			spot = 9700
		}
		f.Stocks = append(f.Stocks, Stock{Symbol: symbol, At: at, Bid: spot, Ask: spot + 1})
		for _, right := range []string{"put", "call"} {
			expiry := "2026-04-10"
			strike := wheel.Cents(10000)
			bid := wheel.Cents(100)
			delta := -.25
			if right == "call" {
				expiry = "2026-04-09"
				strike = 9500
				bid = 300
				delta = .85
				if at.In(wheel.Eastern).Day() > 9 {
					expiry = "2026-04-13"
				}
			}
			oi := 0
			if symbol == "NVDA" {
				oi = 1000
			}
			q := Quote{Quote: wheel.Quote{Option: wheel.Option{ID: fmt.Sprintf("fixture-%s-%s", symbol, right), Symbol: symbol, Kind: right, Expiry: expiry, Strike: strike, Quantity: 1}, At: at, UnderlyingAt: at, Spot: spot, Bid: bid, Ask: bid + 10, Tick: 1, Delta: delta, OI: oi, BidSize: 10, Standard: true, Tradable: true, Weekly: right == "put"}, ListedAt: listed, OIAt: at.Add(-time.Hour), GreeksAt: at}
			if expiry == "2026-04-13" {
				q.ID += "-2026-04-13"
			}
			f.Quotes = append(f.Quotes, q)
			if right == "call" && symbol == "NVDA" {
				// A larger premium must not override delta-first call selection.
				q.ID = "fixture-NVDA-call-larger-premium"
				if expiry == "2026-04-13" {
					q.ID += "-2026-04-13"
				}
				q.Strike = 10000
				q.Delta = .60
				q.Bid = 400
				q.Ask = 410
				f.Quotes = append(f.Quotes, q)
			}
		}
	}
	if kind == "decision" {
		indices := map[string]int{}
		for _, q := range f.Quotes {
			key := q.Symbol + "/" + q.Kind + "/" + q.Expiry
			i, exists := indices[key]
			if !exists {
				i = len(f.Chains)
				indices[key] = i
				f.Chains = append(f.Chains, Chain{Symbol: q.Symbol, Kind: q.Kind, Expiry: q.Expiry, At: at, Weekly: q.Weekly})
			}
			f.Chains[i].Contracts = append(f.Chains[i].Contracts, q.ID)
		}
	}
	return f
}
