package contractpolicy

import "testing"

func TestFixedPolicyBoundaries(t *testing.T) {
	stock := Book{Bid: 10000, Ask: 10001} // A half-cent midpoint must not be rounded.
	put := func(id, expiry string, strike int64) Contract {
		return Contract{ID: id, Symbol: "AAPL", Expiry: expiry, Kind: "put", Strike: strike}
	}
	menu := []Contract{put("weekly", "2026-10-23", 9000), put("over", "2026-11-16", 9001),
		put("z", "2026-11-16", 9000), put("a", "2026-11-16", 9000)}
	got, err := Choose("conservative", "put", "2026-10-17", "AAPL", stock, menu)
	if err != nil || got == nil || got.ID != "a" {
		t.Fatal("fixed target, midpoint or stable identity tie changed", got, err)
	}
	if _, err = Choose("conservative", "put", "2026-10-17", "AAPL", stock, append(menu, menu[0])); err == nil {
		t.Fatal("duplicate identity accepted")
	}
	if _, err = Choose("aggressive", "put", "2026-10-17", "AAPL", stock, menu); err == nil {
		t.Fatal("aggressive acquired stock through a put")
	}
	if _, err = Choose("conservative", "put", "2026-10-17", "META", stock, menu); err == nil {
		t.Fatal("excluded issuer accepted")
	}
	// Select the closest eligible expiry first. Do not move to another expiry
	// merely because the target strike is absent at the chosen date.
	got, err = Choose("conservative", "put", "2026-10-17", "AAPL", Book{10000, 10000},
		[]Contract{put("near-without-target", "2026-11-16", 10000), put("later-target", "2026-11-20", 9000)})
	if err != nil || got != nil {
		t.Fatal("missing target silently moved expiry", got, err)
	}
}
