package main

import "testing"

func TestSaleRequiresFeeBeforePremium(t *testing.T) {
	put := quote{Kind: "put", Strike: 10000}
	for _, cash := range []int64{1_000_000, 1_000_099} {
		a := account{Cash: cash}
		if err := a.sell("2026-08-03", put, 100, 20, -.1, .4); err == nil || a.Cash != cash || a.Short != nil || len(a.Trades) != 0 {
			t.Fatal("unfunded fee used future premium or mutated account", cash, err)
		}
	}
	a := account{Cash: 1_000_100}
	if err := a.sell("2026-08-03", put, 100, 20, -.1, .4); err != nil || a.Cash != 1_002_000 || a.PeakCommitted != 1_000_000 {
		t.Fatal("fully funded put accounting failed", err, a)
	}
	call := quote{Kind: "call", Strike: 9000}
	b := account{Cash: 99, Shares: 100, Basis: 10000}
	if err := b.sell("2026-08-03", call, 100, 20, .7, .4); err == nil || b.Cash != 99 {
		t.Fatal("covered call fee was not funded before premium", err)
	}
	b.Cash = 100
	if err := b.sell("2026-08-03", call, 100, 20, .7, .4); err != nil || b.PeakCommitted != 1_000_000 {
		t.Fatal("covered call must retain share cost once, independent of call strike", err)
	}
}
