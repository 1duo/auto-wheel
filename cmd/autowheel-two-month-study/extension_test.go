package main

import (
	"strings"
	"testing"
	"time"
)

func TestPennyLimitsUseSubmittedPriceThreshold(t *testing.T) {
	for _, tc := range []struct{ bid, allowance, want int64 }{
		{299, 0, 299}, {300, 0, 300}, {300, 1, 299},
		{305, 1, 300}, {785, 1, 780}, {1445, 1, 1440}, {24, 1, 23},
	} {
		d := fixture().Days[0]
		q := d.Entry.Quotes[0]
		q.Bid = tc.bid
		d.Execution.Books[0].Quote.Bid = tc.bid
		d.Execution.Books[0].Quote.Ask = tc.bid + 5
		o, filled := modelOrder(d, q, tc.allowance)
		if !filled || o.Limit != tc.want || o.Limit%pennyIncrement(o.Limit) != 0 {
			t.Fatalf("bid=%d allowance=%d: %+v filled=%v", tc.bid, tc.allowance, o, filled)
		}
	}
}

func TestThreeMonthCalendarAndMonthlyCarry(t *testing.T) {
	in := input{Schema: "AutoWheelThreeMonthEndpointInputV1", Symbol: "NVDA", Sources: map[string]string{"official": strings.Repeat("a", 64)}, ExpiryCloses: map[string]officialClose{}}
	first, _ := time.Parse("2006-01-02", "2026-07-01")
	for date := first; date.Format("2006-01-02") <= "2026-09-30"; date = date.AddDate(0, 0, 1) {
		name := date.Format("2006-01-02")
		if date.Weekday() == time.Saturday || date.Weekday() == time.Sunday || name == "2026-07-03" || name == "2026-09-07" {
			continue
		}
		cut, _ := time.Parse(time.RFC3339, name+"T20:00:00Z")
		d := day{Date: name, Close: sample{At: cut, Stock: stock{At: cut.Add(-time.Second), Bid: 22000, Ask: 22001}}}
		if date.Weekday() == time.Monday || name == "2026-07-01" || name == "2026-09-08" {
			entry := cut.Add(-6*time.Hour - 25*time.Minute)
			d.Entry = new(sample{At: entry, Stock: stock{At: entry.Add(-time.Second), Bid: 22000, Ask: 22001}})
			d.Execution = new(execution{At: entry.Add(time.Minute), NotBeforeNS: entry.Add(time.Second).UnixNano()})
		}
		if date.Weekday() == time.Friday || name == "2026-07-02" {
			in.ExpiryCloses[name] = officialClose{Date: name, Symbol: in.Symbol, Native: 22000 * 10_000_000, LabelNS: date.UnixNano(), Source: "official"}
		}
		in.Days = append(in.Days, d)
	}
	if err := validateEndpointInput(in); err != nil {
		t.Fatal(err)
	}
	if len(in.Days) != 64 || len(in.ExpiryCloses) != 13 || in.Days[1].Date != "2026-07-02" {
		t.Fatal("wrong holiday session schedule")
	}
	q := fixture().Days[0].Entry.Quotes[0]
	q.Contract, q.Expiry = "NVDA  260702P00202500", "2026-07-02"
	if err := validateQuote(q, in.Symbol, fixture().Days[0].Entry.At, 30*time.Second); err != nil {
		t.Fatal("actual Thursday holiday expiry rejected", err)
	}
	r := endpointResult{Daily: []endpointPoint{{Date: "2026-07-31", Equity: new(initialCash + 100)}, {Date: "2026-08-31", Equity: new(initialCash - 200)}, {Date: "2026-09-30", Equity: new(initialCash + 500)}}}
	m := endpointPeriods(r)
	if len(m) != 3 || *m["2026-08"].Start != initialCash+100 || *m["2026-08"].PnL != -300 || *m["2026-09"].PnL != 700 {
		t.Fatal("monthly boundaries reset or fail to reconcile", m)
	}
}
