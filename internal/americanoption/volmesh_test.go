package americanoption

import (
	"errors"
	"math"
	"math/big"
	"slices"
	"testing"
)

func meshForTest(t *testing.T, nodes []VolNode) *VolMesh {
	t.Helper()
	m, err := NewVolMesh(nodes)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestVolMeshNonmonotoneCompleteSets(t *testing.T) {
	m := meshForTest(t, []VolNode{{minSigma, 0, -.9}, {1, 4, -.7}, {2, 0, .1}, {maxSigma, 6, .2}})
	got, err := m.Compatible(MarketPriceInterval{BidCents: 100, AskCents: 200})
	if err != nil {
		t.Fatal(err)
	}
	wantSigma := []ClosedInterval{{.250075, .50005}, {1.5, 1.75}, {2.5, 3}}
	wantDelta := []ClosedInterval{{-.85, -.8}, {-.3, -.1}, {7.0 / 60, 2.0 / 15}}
	for name, pair := range map[string][2][]ClosedInterval{
		"sigma": {got.SigmaIntervals, wantSigma}, "delta": {got.DeltaIntervals, wantDelta},
	} {
		if len(pair[0]) != len(pair[1]) {
			t.Fatalf("%s components: got %v, want %v", name, pair[0], pair[1])
		}
		for i, interval := range pair[0] {
			if math.Abs(interval.Lower-pair[1][i].Lower) > 1e-14 || math.Abs(interval.Upper-pair[1][i].Upper) > 1e-14 {
				t.Fatalf("%s component %d: got %+v, want %+v", name, i, interval, pair[1][i])
			}
		}
	}
	if got.HistoricalQualified || got.Model != VolMeshModelID || len(got.CurveSHA256) != 64 {
		t.Fatalf("unexpected provenance: %+v", got)
	}
}

func TestVolMeshFlatPriceRetainsInteriorDeltaExtremum(t *testing.T) {
	m := meshForTest(t, []VolNode{{minSigma, 1, -.1}, {1, 1, -.9}, {maxSigma, 1, -.1}})
	got, err := m.Compatible(MarketPriceInterval{BidCents: 100, AskCents: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.SigmaIntervals, []ClosedInterval{{minSigma, maxSigma}}) ||
		!slices.Equal(got.DeltaIntervals, []ClosedInterval{{-.9, -.1}}) || got.DeltaEnvelope != (ClosedInterval{-.9, -.1}) {
		t.Fatalf("lost full flat-price domain or interior delta extremum: %+v", got)
	}
	if _, err := m.Compatible(MarketPriceInterval{BidCents: 101, AskCents: 102}); !errors.Is(err, ErrEmptyCompatibleSet) {
		t.Fatalf("incompatible flat curve: %v", err)
	}
}

func TestVolMeshClosedKnotsAndPartialAttainability(t *testing.T) {
	m := meshForTest(t, []VolNode{{minSigma, 1, -.5}, {1, 2, -.25}, {maxSigma, 1, -.5}})
	got, err := m.Compatible(MarketPriceInterval{BidCents: 200, AskCents: 200})
	if err != nil || !slices.Equal(got.SigmaIntervals, []ClosedInterval{{1, 1}}) || !slices.Equal(got.DeltaIntervals, []ClosedInterval{{-.25, -.25}}) {
		t.Fatalf("closed shared knot must appear once: %+v, %v", got, err)
	}
	got, err = m.Compatible(MarketPriceInterval{BidCents: 0, AskCents: 100})
	if err != nil || !slices.Equal(got.SigmaIntervals, []ClosedInterval{{minSigma, minSigma}, {maxSigma, maxSigma}}) {
		t.Fatalf("partly unattainable spread still has two compatible vertices: %+v, %v", got, err)
	}
	if _, err := m.Compatible(MarketPriceInterval{BidCents: 201, AskCents: 202}); !errors.Is(err, ErrEmptyCompatibleSet) {
		t.Fatalf("unattainable interval must be empty: %v", err)
	}
}

func TestVolMeshExactCentsAndOutwardEndpoints(t *testing.T) {
	// The compatible middle point has exact rational delta 1/3. Returning
	// binary64 1/3 as both endpoints would exclude that mathematical value.
	m := meshForTest(t, []VolNode{{minSigma, 0, 0}, {1, 0, 0}, {2, 3, 1}, {maxSigma, 3, 1}})
	got, err := m.Compatible(MarketPriceInterval{BidCents: 100, AskCents: 100})
	if err != nil || len(got.DeltaIntervals) != 1 {
		t.Fatalf("exact rational point: %+v, %v", got, err)
	}
	third := new(big.Rat).SetFrac64(1, 3)
	d := got.DeltaIntervals[0]
	if rationalFloat(d.Lower).Cmp(third) > 0 || rationalFloat(d.Upper).Cmp(third) < 0 || d.Lower == d.Upper {
		t.Fatalf("inward delta rounding excludes 1/3: %+v", d)
	}
	// 1 cent is exactly 1/100, whereas float64(.01) is slightly higher.
	m = meshForTest(t, []VolNode{{minSigma, 0, 0}, {1, 0, 0}, {2, 1, 1}, {maxSigma, 1, 1}})
	got, err = m.Compatible(MarketPriceInterval{BidCents: 1, AskCents: 1})
	if err != nil {
		t.Fatal(err)
	}
	cent := new(big.Rat).SetFrac64(1, 100)
	d = got.DeltaIntervals[0]
	if rationalFloat(d.Lower).Cmp(cent) > 0 || rationalFloat(d.Upper).Cmp(cent) < 0 {
		t.Fatalf("inexact decimal price narrowed the set: %+v", d)
	}
}

func TestVolMeshValidationOwnershipAndAllowances(t *testing.T) {
	nodes := []VolNode{{minSigma, 1, -.25}, {maxSigma, 1, -.25}}
	m := meshForTest(t, nodes)
	originalDigest := m.digest
	nodes[0].PriceUSD = 2
	got, err := m.Compatible(MarketPriceInterval{BidCents: 100, AskCents: 100, DeltaAllowance: .125})
	if err != nil || got.CurveSHA256 != originalDigest || got.DeltaEnvelope != (ClosedInterval{-.375, -.125}) {
		t.Fatalf("caller mutation or lost allowance: %+v, %v", got, err)
	}
	got, err = m.Compatible(MarketPriceInterval{BidCents: 125, AskCents: 125, PriceAllowanceUSD: .25})
	if err != nil || !slices.Equal(got.SigmaIntervals, []ClosedInterval{{minSigma, maxSigma}}) {
		t.Fatalf("closed price allowance not retained: %+v, %v", got, err)
	}
	for _, bad := range [][]VolNode{
		nil, {{minSigma, 1, 0}}, {{.1, 1, 0}, {maxSigma, 1, 0}},
		{{minSigma, 1, 0}, {1, 1, 0}, {1, 1, 0}, {maxSigma, 1, 0}},
		{{minSigma, -1, 0}, {maxSigma, 1, 0}},
		{{minSigma, 1, math.NaN()}, {maxSigma, 1, 0}},
		{{minSigma, 1, 0}, {math.Inf(1), 1, 0}, {maxSigma, 1, 0}},
	} {
		if _, err := NewVolMesh(bad); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("bad mesh accepted: %v, %v", bad, err)
		}
	}
	for _, bad := range []MarketPriceInterval{
		{BidCents: -1}, {BidCents: 2, AskCents: 1},
		{PriceAllowanceUSD: -.01}, {PriceAllowanceUSD: math.NaN()},
		{DeltaAllowance: -.01}, {DeltaAllowance: math.Inf(1)},
	} {
		if _, err := m.Compatible(bad); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("bad observation accepted: %+v, %v", bad, err)
		}
	}
	if _, err := (*VolMesh)(nil).Compatible(MarketPriceInterval{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("nil mesh: %v", err)
	}
	if _, err := new(VolMesh).Compatible(MarketPriceInterval{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero mesh: %v", err)
	}
}

func TestVolMeshAllowanceOverflowFailsClosed(t *testing.T) {
	m := meshForTest(t, []VolNode{{minSigma, 1, math.MaxFloat64}, {maxSigma, 1, math.MaxFloat64}})
	if _, err := m.Compatible(MarketPriceInterval{BidCents: 100, AskCents: 100, DeltaAllowance: math.MaxFloat64}); !errors.Is(err, ErrNonConverged) {
		t.Fatalf("nonrepresentable enclosure accepted: %v", err)
	}
}
