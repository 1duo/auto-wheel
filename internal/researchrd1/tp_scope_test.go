package researchrd1

import "testing"

func TestConservativeTPExactFeeBoundaryAndScope(t *testing.T) {
	for _, target := range []struct {
		exit     string
		captured int64
	}{{"tp50", 50}, {"tp75", 75}, {"tp90", 90}, {"puttp50", 50}, {"puttp75", 75}, {"puttp90", 90}} {
		p := Profile{Mode: "conservative", Exit: target.exit}
		ask := 100 - target.captured
		at, err := TakeProfit(p, 10_000, ask, 0)
		if err != nil || !at {
			t.Fatal("exact threshold missed", target.exit, err)
		}
		below, err := TakeProfit(p, 10_000, ask, 1)
		if err != nil || below {
			t.Fatal("closing fee erased the exact boundary", target.exit, err)
		}
		put := profitProfile(p, "put")
		call := profitProfile(p, "call")
		if put != p {
			t.Fatal("put scope changed opening policy")
		}
		got, err := TakeProfit(call, 10_000, 1, 0)
		want := target.exit == "tp50" || target.exit == "tp75" || target.exit == "tp90"
		if err != nil || got != want {
			t.Fatal("put-only scope closed a call", target.exit, err)
		}
	}
	if _, err := TakeProfit(Profile{Mode: "conservative", Exit: "tp74"}, 10_000, 1, 0); err == nil {
		t.Fatal("unregistered threshold accepted")
	}
}
