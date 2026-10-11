package americanoption

import (
	"math"
	"testing"
	"time"
)

func TestLowVolATMZeroRateEuropeanAgreement(t *testing.T) {
	// With zero rates and no dividends, early exercise adds no value. At
	// S=K, the independent European formula simplifies to an erf expression.
	now := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)
	for _, sigma := range []float64{.0001, .006325, .01255, .025, .05} {
		z := sigma * math.Sqrt(1.0/365) / (2 * math.Sqrt2)
		wantPrice := 100 * math.Erf(z)
		for _, kind := range []Kind{Call, Put} {
			in := Input{Kind: kind, SpotUSD: 100, StrikeUSD: 100, Sigma: sigma, Now: now, Expiry: now.Add(24 * time.Hour)}
			got, err := Price(in, Grid{TimeSteps: 320, SpaceSteps: 901})
			if err != nil {
				t.Fatalf("sigma %g %s: %v", sigma, kind, err)
			}
			wantDelta := .5 + .5*math.Erf(z)
			if kind == Put {
				wantDelta--
			}
			if math.Abs(got.PriceUSD-wantPrice) > .0001 || math.Abs(got.Delta-wantDelta) > .0001 {
				t.Fatalf("sigma %g %s: %+v versus European price %.12g delta %.12g", sigma, kind, got, wantPrice, wantDelta)
			}
		}
	}
}

func TestLowVolATMPositiveRateGridConvergence(t *testing.T) {
	now := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)
	for _, sigma := range []float64{.0001, .006325, .01255, .025, .05} {
		in := Input{Kind: Put, SpotUSD: 100, StrikeUSD: 100, Sigma: sigma, Rate: .05, Now: now, Expiry: now.Add(24 * time.Hour)}
		a, err := Price(in, Grid{TimeSteps: 320, SpaceSteps: 900})
		if err != nil {
			t.Fatal(err)
		}
		b, err := Price(in, Grid{TimeSteps: 640, SpaceSteps: 1800})
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(a.PriceUSD-b.PriceUSD) > .001 || math.Abs(a.Delta-b.Delta) > .001 {
			t.Fatalf("sigma %g: grid disagreement %+v versus %+v", sigma, a, b)
		}
	}
}
