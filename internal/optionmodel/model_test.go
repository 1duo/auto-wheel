package optionmodel

import (
	"errors"
	"math"
	"testing"
	"time"
)

// Canonical fixture: S100 K100 r.05 q0 sigma.2 T1 (365-day year).
func canonTimes() (now, expiry time.Time) {
	now = time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	return now, now.Add(365 * 24 * time.Hour)
}

func canonPriceInput(typ OptionType) PriceInput {
	now, expiry := canonTimes()
	return PriceInput{
		Type: typ, SpotCents: 10000, StrikeCents: 10000,
		Sigma: 0.2, RiskFree: 0.05, DivYield: 0,
		HasRate: true, HasDiv: true,
		Expiry: expiry, Now: now,
		SpotKnownAt: now, RateKnownAt: now, DivKnownAt: now,
	}
}

func canonIVInput(typ OptionType, midCents int64) IVInput {
	now, expiry := canonTimes()
	return IVInput{
		Type: typ, SpotCents: 10000, StrikeCents: 10000, ObservedMidCents: midCents,
		RiskFree: 0.05, DivYield: 0, HasRate: true, HasDiv: true,
		Expiry: expiry, Now: now,
		SpotKnownAt: now, OptionKnownAt: now, RateKnownAt: now, DivKnownAt: now,
	}
}

func TestPriceCanonicalCall(t *testing.T) {
	got, err := Price(canonPriceInput(Call))
	if err != nil {
		t.Fatalf("Price call: %v", err)
	}
	if got.Model != ModelID {
		t.Fatalf("Model = %q, want %q", got.Model, ModelID)
	}
	if got.PriceCents != 1045 {
		t.Fatalf("PriceCents = %d, want 1045", got.PriceCents)
	}
	if math.Abs(got.Delta-0.63683) > 1e-4 {
		t.Fatalf("Delta = %v, want 0.63683", got.Delta)
	}
}

func TestPriceCanonicalPut(t *testing.T) {
	got, err := Price(canonPriceInput(Put))
	if err != nil {
		t.Fatalf("Price put: %v", err)
	}
	if got.PriceCents != 557 {
		t.Fatalf("PriceCents = %d, want 557", got.PriceCents)
	}
	if math.Abs(got.Delta+0.36317) > 1e-4 {
		t.Fatalf("Delta = %v, want -0.36317", got.Delta)
	}
}

func TestImpliedVolRoundTrip(t *testing.T) {
	for _, typ := range []OptionType{Call, Put} {
		pr, err := Price(canonPriceInput(typ))
		if err != nil {
			t.Fatalf("Price %s: %v", typ, err)
		}
		iv, err := ImpliedVol(canonIVInput(typ, pr.PriceCents))
		if err != nil {
			t.Fatalf("ImpliedVol %s: %v", typ, err)
		}
		if math.Abs(iv.ImpliedVol-0.2) > 1e-3 {
			t.Fatalf("IV %s = %v, want 0.2", typ, iv.ImpliedVol)
		}
		back := canonPriceInput(typ)
		back.Sigma = iv.ImpliedVol
		re, err := Price(back)
		if err != nil {
			t.Fatalf("reprice %s: %v", typ, err)
		}
		if d := re.PriceCents - pr.PriceCents; d < -1 || d > 1 {
			t.Fatalf("reprice %s drifted %d cents (mid %d)", typ, d, pr.PriceCents)
		}
	}
}

func TestRejectBadTimestamps(t *testing.T) {
	now, _ := canonTimes()
	px := canonPriceInput(Call)
	px.SpotKnownAt = now.Add(time.Second)
	if _, err := Price(px); !errors.Is(err, ErrFutureInput) {
		t.Fatalf("future spot: err = %v, want ErrFutureInput", err)
	}
	px = canonPriceInput(Call)
	px.SpotKnownAt = now.Add(-31 * time.Second)
	if _, err := Price(px); !errors.Is(err, ErrStaleInput) {
		t.Fatalf("stale spot: err = %v, want ErrStaleInput", err)
	}
	iv := canonIVInput(Call, 1045)
	iv.OptionKnownAt = now.Add(time.Second)
	if _, err := ImpliedVol(iv); !errors.Is(err, ErrFutureInput) {
		t.Fatalf("future option: err = %v, want ErrFutureInput", err)
	}
	iv = canonIVInput(Call, 1045)
	iv.OptionKnownAt = now.Add(-31 * time.Second)
	if _, err := ImpliedVol(iv); !errors.Is(err, ErrStaleInput) {
		t.Fatalf("stale option: err = %v, want ErrStaleInput", err)
	}
}

func TestRejectMissingRateDivEvidence(t *testing.T) {
	px := canonPriceInput(Call)
	px.HasRate = false
	if _, err := Price(px); !errors.Is(err, ErrMissingProvenance) {
		t.Fatalf("missing r: err = %v, want ErrMissingProvenance", err)
	}
	px = canonPriceInput(Call)
	px.HasDiv = false
	if _, err := Price(px); !errors.Is(err, ErrMissingProvenance) {
		t.Fatalf("missing q: err = %v, want ErrMissingProvenance", err)
	}
	px = canonPriceInput(Call)
	px.RateKnownAt = time.Time{}
	if _, err := Price(px); !errors.Is(err, ErrMissingProvenance) {
		t.Fatalf("zero rate known-at: err = %v, want ErrMissingProvenance", err)
	}
	iv := canonIVInput(Call, 1045)
	iv.HasRate = false
	if _, err := ImpliedVol(iv); !errors.Is(err, ErrMissingProvenance) {
		t.Fatalf("IV missing r: err = %v, want ErrMissingProvenance", err)
	}
}

func TestRejectBoundsAndNonFinite(t *testing.T) {
	if _, err := ImpliedVol(canonIVInput(Call, 20000)); !errors.Is(err, ErrNoArbitrage) {
		t.Fatalf("wide mid: err = %v, want ErrNoArbitrage", err)
	}
	if _, err := ImpliedVol(canonIVInput(Call, 9990)); !errors.Is(err, ErrNotBracketed) {
		t.Fatalf("unbracketed mid: err = %v, want ErrNotBracketed", err)
	}
	now, _ := canonTimes()
	huge := canonPriceInput(Put)
	huge.SpotCents = 100
	huge.StrikeCents = 8000000000000000000
	huge.RiskFree = -0.05
	huge.Expiry = now.Add(5 * 365 * 24 * time.Hour)
	if _, err := Price(huge); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("overflow put: err = %v, want ErrInvalidInput", err)
	}
	bad := canonPriceInput(Call)
	bad.RiskFree = math.Inf(1)
	if _, err := Price(bad); !errors.Is(err, ErrNonFinite) {
		t.Fatalf("inf rate: err = %v, want ErrNonFinite", err)
	}
}
