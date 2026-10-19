package researchrd1

import "testing"

func TestRD1Rules(t *testing.T) {
	p := Profile{Mode: "conservative", Exit: "tp50"}
	menu := []Contract{
		{ID: "later", Symbol: "AAPL", Expiry: "2021-02-05", Kind: "put", Strike: 11500},
		{ID: "early-high", Symbol: "AAPL", Expiry: "2021-01-29", Kind: "put", Strike: 11600},
		{ID: "early-low", Symbol: "AAPL", Expiry: "2021-01-29", Kind: "put", Strike: 11500},
	}
	c, err := Choose(p, "put", "2021-01-04", "AAPL", Book{Bid: 12900, Ask: 12901}, menu)
	if err != nil || c == nil {
		t.Fatalf("identity choice: %v %v", c, err)
	}
	// Jan29 is 24 days away and Feb5 32, so the nearer 30-DTE expiry wins.
	if c.ID != "later" {
		t.Fatalf("nearest DTE must precede strike selection: %v", c)
	}
	if Capacity(5_000_000, 2_000_000, 0, 0, 0, 2_000_000, 0, true) == false {
		t.Fatal("funded put rejected")
	}
	if Capacity(3_000_000, 2_000_000, 0, 0, 0, 2_000_000, 0, true) {
		t.Fatal("put collateral evades reserve")
	}
	if Capacity(3_000_000, 0, 0, 0, 2_000_000, 2_000_000, 0, true) {
		t.Fatal("direct stock purchase evades reserve")
	}
	if Capacity(8_000_000, 0, 0, 0, 0, 2_000_000, 3, true) {
		t.Fatal("fourth symbol admitted")
	}
	for _, tc := range []struct {
		ask, fee int64
		want     bool
	}{{49, 100, true}, {50, 100, false}} {
		got, err := TakeProfit(p, 10_000, tc.ask, tc.fee)
		if err != nil || got != tc.want {
			t.Fatalf("net capture %v: %v %v", tc, got, err)
		}
	}
	if _, err := NAV(1_000, 0, []Holding{{Shares: 100}}); err == nil {
		t.Fatal("missing stock mark accepted")
	}
	if _, err := NAV(1_000, 0, []Holding{{Short: new(Book{})}}); err == nil {
		t.Fatal("missing option ask accepted")
	}
	crossed := Book{Bid: 249, Ask: 230}
	if crossed.EntryValid() {
		t.Fatal("crossed entry admitted")
	}
	if nav, err := NAV(100_000, 0, []Holding{{Short: new(crossed)}}); err != nil || nav != 77_000 {
		t.Fatalf("observed short ask must remain a liability: %d %v", nav, err)
	}
	nav, err := NAV(2_000_000, 2050, []Holding{{Shares: 100, Stock: new(Book{Bid: 12000, Ask: 12001}), Short: new(Book{Bid: 49, Ask: 50})}})
	if err != nil || nav != 3_197_050 {
		t.Fatalf("whole NAV/receivable: %d %v", nav, err)
	}
}
