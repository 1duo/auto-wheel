package americanoption

import (
	"errors"
	"math"
	"testing"
	"time"
)

func validInput() Input {
	now := time.Date(2026, time.April, 6, 13, 35, 0, 0, time.UTC)
	return Input{
		Kind:      Call,
		SpotUSD:   100,
		StrikeUSD: 100,
		Sigma:     0.3,
		Rate:      0.05,
		Now:       now,
		Expiry:    now.Add(5 * 24 * time.Hour),
	}
}

func TestRejectInvalidInputs(t *testing.T) {
	base := validInput()
	grid := Grid{TimeSteps: 256, SpaceSteps: 256}
	cases := map[string]func(*Input, *Grid){
		"bad kind":         func(in *Input, _ *Grid) { in.Kind = "euro" },
		"zero spot":        func(in *Input, _ *Grid) { in.SpotUSD = 0 },
		"strike over 10m":  func(in *Input, _ *Grid) { in.StrikeUSD = 10_000_001 },
		"sigma tiny":       func(in *Input, _ *Grid) { in.Sigma = 1e-5 },
		"sigma huge":       func(in *Input, _ *Grid) { in.Sigma = 6 },
		"rate low":         func(in *Input, _ *Grid) { in.Rate = -0.06 },
		"rate high":        func(in *Input, _ *Grid) { in.Rate = 1.1 },
		"expiry not after": func(in *Input, _ *Grid) { in.Expiry = in.Now },
		"tenor 10 days": func(in *Input, _ *Grid) {
			in.Expiry = in.Now.Add(10 * 24 * time.Hour)
		},
		"zero dividend": func(in *Input, _ *Grid) {
			in.Dividends = []CashDividend{{AmountUSD: 0, ExAt: in.Now.Add(24 * time.Hour)}}
		},
		"div at now": func(in *Input, _ *Grid) {
			in.Dividends = []CashDividend{{AmountUSD: 1, ExAt: in.Now}}
		},
		"div at expiry": func(in *Input, _ *Grid) {
			in.Dividends = []CashDividend{{AmountUSD: 1, ExAt: in.Expiry}}
		},
		"unsorted divs": func(in *Input, _ *Grid) {
			in.Dividends = []CashDividend{
				{AmountUSD: 1, ExAt: in.Now.Add(48 * time.Hour)},
				{AmountUSD: 1, ExAt: in.Now.Add(24 * time.Hour)},
			}
		},
		"duplicate divs": func(in *Input, _ *Grid) {
			ex := in.Now.Add(24 * time.Hour)
			in.Dividends = []CashDividend{{AmountUSD: 1, ExAt: ex}, {AmountUSD: 1, ExAt: ex}}
		},
		"time steps small":  func(_ *Input, g *Grid) { g.TimeSteps = 100 },
		"space steps large": func(_ *Input, g *Grid) { g.SpaceSteps = 9000 },
		"work too large": func(_ *Input, g *Grid) {
			g.TimeSteps, g.SpaceSteps = 8192, 8192
		},
	}
	for name, mut := range cases {
		in := base
		g := grid
		mut(&in, &g)
		if _, err := Price(in, g); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s: err=%v, want ErrInvalidInput", name, err)
		}
	}
}

func TestObstacleAndBounds(t *testing.T) {
	now := time.Date(2026, time.April, 6, 13, 35, 0, 0, time.UTC)
	for _, kind := range []Kind{Call, Put} {
		in := Input{Kind: kind, SpotUSD: 100, StrikeUSD: 100, Sigma: 0.3, Rate: 0.05,
			Now: now, Expiry: now.Add(2 * 24 * time.Hour)}
		got, err := Price(in, Grid{TimeSteps: 256, SpaceSteps: 256})
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if !math.IsNaN(got.PriceUSD) && !math.IsInf(got.PriceUSD, 0) {
		} else {
			t.Fatalf("%s: non-finite price %v", kind, got.PriceUSD)
		}
		if got.PriceUSD < -1e-6 {
			t.Fatalf("%s: negative price %v", kind, got.PriceUSD)
		}
		if kind == Call {
			if got.PriceUSD > in.SpotUSD+1e-6 {
				t.Fatalf("call %v above spot", got.PriceUSD)
			}
		} else {
			T := float64(in.Expiry.Sub(in.Now).Nanoseconds()) / float64(yearNs)
			capPut := in.StrikeUSD * max(1.0, math.Exp(-in.Rate*T))
			if got.PriceUSD > capPut+1e-6 {
				t.Fatalf("put %v above cap %v", got.PriceUSD, capPut)
			}
		}
	}
}

func bsmEuroCall(s, k, r, sig, t float64) float64 {
	d1 := (math.Log(s/k) + (r+0.5*sig*sig)*t) / (sig * math.Sqrt(t))
	d2 := d1 - sig*math.Sqrt(t)
	n := func(x float64) float64 { return 0.5 * math.Erfc(-x/math.Sqrt2) }
	return s*n(d1) - k*math.Exp(-r*t)*n(d2)
}

func TestNoDivCallMatchesEuropean(t *testing.T) {
	in := validInput() // 5-day ATM call, r=0.05 >= 0, no dividends.
	got, err := Price(in, Grid{TimeSteps: 1024, SpaceSteps: 1024})
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	T := float64(in.Expiry.Sub(in.Now).Nanoseconds()) / float64(yearNs)
	want := bsmEuroCall(in.SpotUSD, in.StrikeUSD, in.Rate, in.Sigma, T)
	if math.Abs(got.PriceUSD-want) > 0.02 {
		t.Fatalf("American call %v vs European %v (diff %v)", got.PriceUSD, want, got.PriceUSD-want)
	}
}

func TestExactSubsecondEventPreserved(t *testing.T) {
	now := time.Date(2026, time.April, 6, 13, 35, 0, 123456789, time.UTC)
	expiry := now.Add(2 * 24 * time.Hour).Add(987654321 * time.Nanosecond)
	ex1 := now.Add(24 * time.Hour).Add(111111111 * time.Nanosecond)
	in := Input{Kind: Call, SpotUSD: 100, StrikeUSD: 100, Sigma: 0.3, Rate: 0.05,
		Now: now, Expiry: expiry, Dividends: []CashDividend{{AmountUSD: 0.5, ExAt: ex1}}}
	if _, err := Price(in, Grid{TimeSteps: 256, SpaceSteps: 256}); err != nil {
		t.Fatalf("subsecond schedule rejected: %v", err)
	}
	// 1ns-distinct dividends remain distinct (no second rounding).
	in2 := in
	in2.Dividends = []CashDividend{{AmountUSD: 0.5, ExAt: ex1}, {AmountUSD: 0.5, ExAt: ex1.Add(time.Nanosecond)}}
	if _, err := Price(in2, Grid{TimeSteps: 256, SpaceSteps: 256}); err != nil {
		t.Fatalf("1ns-distinct divs rejected: %v", err)
	}
	// Duplicate ns instant rejected.
	in3 := in
	in3.Dividends = []CashDividend{{AmountUSD: 0.5, ExAt: ex1}, {AmountUSD: 0.5, ExAt: ex1}}
	if _, err := Price(in3, Grid{TimeSteps: 256, SpaceSteps: 256}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("duplicate ns divs: err=%v, want ErrInvalidInput", err)
	}
	// Tenor 9 days + 1ns is over envelope; exactly 9 days is allowed.
	nine := now.Add(9 * 24 * time.Hour)
	over := Input{Kind: Call, SpotUSD: 100, StrikeUSD: 100, Sigma: 0.3, Rate: 0, Now: now, Expiry: nine.Add(time.Nanosecond)}
	if _, err := Price(over, Grid{TimeSteps: 256, SpaceSteps: 256}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("9d+1ns: err=%v, want ErrInvalidInput", err)
	}
}

func TestNegativeRateDeepPutExceedsK(t *testing.T) {
	now := time.Date(2026, time.April, 6, 13, 35, 0, 0, time.UTC)
	in := Input{Kind: Put, SpotUSD: 0.01, StrikeUSD: 100, Sigma: 0.3, Rate: -0.05,
		Now: now, Expiry: now.Add(8 * 24 * time.Hour)}
	got, err := Price(in, Grid{TimeSteps: 512, SpaceSteps: 512})
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	if !(got.PriceUSD > 100) {
		t.Fatalf("deep negative-rate put %v should exceed K=100", got.PriceUSD)
	}
	T := float64(in.Expiry.Sub(in.Now).Nanoseconds()) / float64(yearNs)
	capPut := 100 * max(1.0, math.Exp(0.05*T))
	if got.PriceUSD > capPut+0.02 {
		t.Fatalf("put %v above discounted cap %v", got.PriceUSD, capPut)
	}
	if got.Delta < -1.05 || got.Delta > -0.95 {
		t.Fatalf("delta %v want near -1", got.Delta)
	}
}

func TestUnrepresentableSpaceGridRejected(t *testing.T) {
	in := validInput()
	in.SpotUSD = math.SmallestNonzeroFloat64
	in.Dividends = []CashDividend{{AmountUSD: 1, ExAt: in.Now.Add(time.Hour)}}
	if _, err := Price(in, Grid{TimeSteps: 256, SpaceSteps: 256}); !errors.Is(err, ErrNonConverged) {
		t.Fatalf("unrepresentable cash/grid ratio: %v, want ErrNonConverged", err)
	}
}

func TestResidualRejectsNonfiniteAndLargeContinuationSlack(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), 1000.0001} {
		if checkResidual([]float64{0, 0, 0}, []float64{0, 1, 0}, []float64{0, 0, 0}, []float64{0, value, 0}, []float64{0, 1000, 0}, []float64{0, 0, 0}, 1e-6) {
			t.Fatalf("accepted invalid absolute residual for value %v", value)
		}
	}
}
