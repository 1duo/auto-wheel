package optionmodel

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// ModelID is EUROPEAN BSM with continuous q. No American/discrete-div
// qualification; Main qualifies that separately. Not wired into replay.
const ModelID = "BSM-European-continuous-q-v1"
const MaxQuoteAge = 30 * time.Second // spot/option staleness bound

const ( // deterministic math bounds; T in years, 365-day year
	yearSecs = 365 * 24 * 3600.0
	minSigma = 1e-4
	maxSigma = 5.0
	maxYears = 5.0
)

var (
	ErrMissingProvenance = errors.New("optionmodel: missing provenance")
	ErrFutureInput       = errors.New("optionmodel: input known after calculation")
	ErrStaleInput        = errors.New("optionmodel: stale quote")
	ErrNonFinite         = errors.New("optionmodel: non-finite input")
	ErrInvalidInput      = errors.New("optionmodel: invalid input")
	ErrNoArbitrage       = errors.New("optionmodel: arbitrage bound violated")
	ErrNotBracketed      = errors.New("optionmodel: vol not bracketed")
)

type OptionType string

const ( // European exercise only
	Call OptionType = "call"
	Put  OptionType = "put"
)

// PriceInput: money in integer cents (floats math-only, never ledger).
// Zero r/q must be explicit via HasRate/HasDiv + KnownAt; never assumed.
type PriceInput struct {
	Type                                 OptionType
	SpotCents, StrikeCents               int64
	Sigma, RiskFree, DivYield            float64
	HasRate, HasDiv                      bool
	Expiry, Now                          time.Time
	SpotKnownAt, RateKnownAt, DivKnownAt time.Time
}

// IVInput solves sigma from observed mid in integer cents.
type IVInput struct {
	Type                                                OptionType
	SpotCents, StrikeCents, ObservedMidCents            int64
	RiskFree, DivYield                                  float64
	HasRate, HasDiv                                     bool
	Expiry, Now                                         time.Time
	SpotKnownAt, OptionKnownAt, RateKnownAt, DivKnownAt time.Time
}

// PriceResult: GreeksAt=calculation time, LatestInputKnownAt=newest input.
type PriceResult struct {
	Model                        string
	PriceCents                   int64
	Delta                        float64
	GreeksAt, LatestInputKnownAt time.Time
}

// IVResult: solved vol + delta under Model name.
type IVResult struct {
	Model                        string
	ImpliedVol, Delta            float64
	GreeksAt, LatestInputKnownAt time.Time
}

func ckKnown(t, now time.Time) error {
	if t.IsZero() {
		return fmt.Errorf("zero known-at: %w", ErrMissingProvenance)
	}
	if t.After(now) {
		return fmt.Errorf("known-at after now: %w", ErrFutureInput)
	}
	return nil
}
func ckFresh(t, now time.Time) error {
	if err := ckKnown(t, now); err != nil {
		return err
	}
	if now.Sub(t) > MaxQuoteAge {
		return fmt.Errorf("quote older than 30s: %w", ErrStaleInput)
	}
	return nil
}
func ckRates(r, q float64, hr, hd bool) error {
	if !hr || !hd {
		return fmt.Errorf("r/q need explicit evidence: %w", ErrMissingProvenance)
	}
	if math.IsNaN(r) || math.IsInf(r, 0) || math.IsNaN(q) || math.IsInf(q, 0) {
		return fmt.Errorf("non-finite rate: %w", ErrNonFinite)
	}
	if r < -0.05 || r > 1.0 || q < 0 || q > 1.0 {
		return fmt.Errorf("rate out of range: %w", ErrInvalidInput)
	}
	return nil
}
func tenor(expiry, now time.Time) (float64, error) {
	if !expiry.After(now) {
		return 0, fmt.Errorf("expiry must be after now: %w", ErrInvalidInput)
	}
	t := expiry.Sub(now).Seconds() / yearSecs
	if t <= 0 || t > maxYears {
		return 0, fmt.Errorf("tenor out of range: %w", ErrInvalidInput)
	}
	return t, nil
}
func nCDF(x float64) float64 { return 0.5 * math.Erfc(-x/math.Sqrt2) }

// bsm returns dollar price and delta for a European call/put.
func bsm(call bool, s, k, r, q, sig, t float64) (float64, float64) {
	d1 := (math.Log(s/k) + (r-q+0.5*sig*sig)*t) / (sig * math.Sqrt(t))
	d2 := d1 - sig*math.Sqrt(t)
	ds, dk := s*math.Exp(-q*t), k*math.Exp(-r*t)
	if call {
		return ds*nCDF(d1) - dk*nCDF(d2), math.Exp(-q*t) * nCDF(d1)
	}
	return dk*nCDF(-d2) - ds*nCDF(-d1), math.Exp(-q*t) * (nCDF(d1) - 1)
}
func latest(ts []time.Time) time.Time {
	m := ts[0]
	for _, c := range ts[1:] {
		if c.After(m) {
			m = c
		}
	}
	return m
}

// Price deterministically prices a European option at Sigma.
func Price(in PriceInput) (PriceResult, error) {
	if in.Type != Call && in.Type != Put {
		return PriceResult{}, fmt.Errorf("bad type: %w", ErrInvalidInput)
	}
	if in.SpotCents <= 0 || in.StrikeCents <= 0 {
		return PriceResult{}, fmt.Errorf("nonpositive money: %w", ErrInvalidInput)
	}
	if math.IsNaN(in.Sigma) || math.IsInf(in.Sigma, 0) || in.Sigma < minSigma || in.Sigma > maxSigma {
		return PriceResult{}, fmt.Errorf("bad sigma: %w", ErrInvalidInput)
	}
	if err := ckFresh(in.SpotKnownAt, in.Now); err != nil {
		return PriceResult{}, err
	}
	if err := ckKnown(in.RateKnownAt, in.Now); err != nil {
		return PriceResult{}, err
	}
	if err := ckKnown(in.DivKnownAt, in.Now); err != nil {
		return PriceResult{}, err
	}
	if err := ckRates(in.RiskFree, in.DivYield, in.HasRate, in.HasDiv); err != nil {
		return PriceResult{}, err
	}
	t, err := tenor(in.Expiry, in.Now)
	if err != nil {
		return PriceResult{}, err
	}
	s, k := float64(in.SpotCents)/100, float64(in.StrikeCents)/100
	px, dl := bsm(in.Type == Call, s, k, in.RiskFree, in.DivYield, in.Sigma, t)
	if math.IsNaN(px) || math.IsInf(px, 0) || math.IsNaN(dl) || math.IsInf(dl, 0) || px < 0 {
		return PriceResult{}, fmt.Errorf("bad output: %w", ErrNonFinite)
	}
	ds, dk := s*math.Exp(-in.DivYield*t), k*math.Exp(-in.RiskFree*t)
	lo, hi := max(0.0, ds-dk), ds
	if in.Type != Call {
		lo, hi = max(0.0, dk-ds), dk
	}
	if px < lo-0.005 || px > hi+0.005 {
		return PriceResult{}, fmt.Errorf("price outside arbitrage bounds: %w", ErrNoArbitrage)
	}
	rounded := math.Round(px * 100)
	if math.IsNaN(rounded) || math.IsInf(rounded, 0) || rounded >= 1<<63 {
		return PriceResult{}, fmt.Errorf("price overflows int64 cents: %w", ErrInvalidInput)
	}
	return PriceResult{ModelID, int64(rounded), dl, in.Now, latest([]time.Time{in.SpotKnownAt, in.RateKnownAt, in.DivKnownAt})}, nil
}

// ImpliedVol bisects sigma from observed mid (fixed bracket, 100 steps).
func ImpliedVol(in IVInput) (IVResult, error) {
	if in.Type != Call && in.Type != Put {
		return IVResult{}, fmt.Errorf("bad type: %w", ErrInvalidInput)
	}
	if in.SpotCents <= 0 || in.StrikeCents <= 0 || in.ObservedMidCents <= 0 {
		return IVResult{}, fmt.Errorf("nonpositive money: %w", ErrInvalidInput)
	}
	if err := ckFresh(in.SpotKnownAt, in.Now); err != nil {
		return IVResult{}, err
	}
	if err := ckFresh(in.OptionKnownAt, in.Now); err != nil {
		return IVResult{}, err
	}
	if err := ckKnown(in.RateKnownAt, in.Now); err != nil {
		return IVResult{}, err
	}
	if err := ckKnown(in.DivKnownAt, in.Now); err != nil {
		return IVResult{}, err
	}
	if err := ckRates(in.RiskFree, in.DivYield, in.HasRate, in.HasDiv); err != nil {
		return IVResult{}, err
	}
	t, err := tenor(in.Expiry, in.Now)
	if err != nil {
		return IVResult{}, err
	}
	call := in.Type == Call
	s, k, mid := float64(in.SpotCents)/100, float64(in.StrikeCents)/100, float64(in.ObservedMidCents)/100
	ds, dk := s*math.Exp(-in.DivYield*t), k*math.Exp(-in.RiskFree*t)
	lo, hi := max(0.0, ds-dk), ds
	if !call {
		lo, hi = max(0.0, dk-ds), dk
	}
	if mid < lo-0.005 || mid > hi+0.005 {
		return IVResult{}, fmt.Errorf("mid outside arbitrage bounds: %w", ErrNoArbitrage)
	}
	pMin, _ := bsm(call, s, k, in.RiskFree, in.DivYield, minSigma, t)
	pMax, _ := bsm(call, s, k, in.RiskFree, in.DivYield, maxSigma, t)
	if math.IsNaN(pMin) || math.IsInf(pMin, 0) || math.IsNaN(pMax) || math.IsInf(pMax, 0) {
		return IVResult{}, fmt.Errorf("non-finite bracket: %w", ErrNonFinite)
	}
	if mid < pMin || mid > pMax {
		return IVResult{}, fmt.Errorf("mid outside sigma bracket: %w", ErrNotBracketed)
	}
	a, b, sig := minSigma, maxSigma, 0.0
	for range 100 {
		sig = 0.5 * (a + b)
		px, _ := bsm(call, s, k, in.RiskFree, in.DivYield, sig, t)
		if math.IsNaN(px) || math.IsInf(px, 0) {
			return IVResult{}, fmt.Errorf("non-finite bisection: %w", ErrNonFinite)
		}
		if math.Abs(px-mid) < 1e-7 {
			break
		}
		if px < mid {
			a = sig
		} else {
			b = sig
		}
	}
	_, dl := bsm(call, s, k, in.RiskFree, in.DivYield, sig, t)
	if math.IsNaN(dl) || math.IsInf(dl, 0) {
		return IVResult{}, fmt.Errorf("non-finite delta: %w", ErrNonFinite)
	}
	return IVResult{ModelID, sig, dl, in.Now, latest([]time.Time{in.SpotKnownAt, in.OptionKnownAt, in.RateKnownAt, in.DivKnownAt})}, nil
}
