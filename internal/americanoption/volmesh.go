package americanoption

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
)

// VolMeshModelID identifies an explicitly declared piecewise-linear numerical
// curve. Its exact set inversion does not enclose the continuous American PDE,
// prove interpolation accuracy, or qualify historical Greeks.
const VolMeshModelID = "ResearchPiecewiseLinearVolMeshV1"

var ErrEmptyCompatibleSet = errors.New("americanoption: empty compatible volatility set")

// VolNode supplies price and delta on the same volatility mesh. Values are
// mathematical USD and sensitivity, never ledger money. The binary64 values
// define exact rational vertices of the declared surrogate curve.
type VolNode struct {
	Sigma    float64 `json:"sigma"`
	PriceUSD float64 `json:"price_math_usd"`
	Delta    float64 `json:"delta"`
}

// VolMesh owns a validated complete [1e-4,5] mesh. Constructing this mathematical
// curve does not verify the origin, FD convergence or source causality of nodes.
type VolMesh struct {
	nodes  []VolNode
	digest string
}

// ClosedInterval is an outward-rounded enclosure of an exact rational interval.
type ClosedInterval struct {
	Lower float64 `json:"lower"`
	Upper float64 `json:"upper"`
}

// MarketPriceInterval preserves the full observed bid/ask interval. Allowances
// are explicitly supplied modeling assumptions, not demonstrated error bounds.
// Zero bid is allowed for synthetic diagnostics; production quote rules are
// applied by the separate source/selection layer.
type MarketPriceInterval struct {
	BidCents          int64   `json:"bid_cents"`
	AskCents          int64   `json:"ask_cents"`
	PriceAllowanceUSD float64 `json:"price_allowance_math_usd"`
	DeltaAllowance    float64 `json:"delta_allowance"`
}

// CompatibleVolSet includes every compatible segment, including decreasing and
// flat ones. It never returns an arbitrary point IV or point delta. Completeness
// is restricted to the declared surrogate; it is not historical qualification.
type CompatibleVolSet struct {
	Model               string              `json:"model"`
	CurveSHA256         string              `json:"curve_sha256"`
	Observation         MarketPriceInterval `json:"observation"`
	SigmaIntervals      []ClosedInterval    `json:"sigma_intervals"`
	DeltaIntervals      []ClosedInterval    `json:"delta_intervals"`
	DeltaEnvelope       ClosedInterval      `json:"delta_envelope"`
	HistoricalQualified bool                `json:"historical_greeks_qualified"`
}

// NewVolMesh validates and copies the curve. An unobserved volatility region
// cannot be silently omitted: the first and last nodes must cover the domain.
func NewVolMesh(nodes []VolNode) (*VolMesh, error) {
	if len(nodes) < 2 || len(nodes) > 4097 || nodes[0].Sigma != minSigma || nodes[len(nodes)-1].Sigma != maxSigma {
		return nil, fmt.Errorf("volatility mesh must cover [1e-4,5] with 2..4097 nodes: %w", ErrInvalidInput)
	}
	for i, n := range nodes {
		if !finite(n.Sigma) || !finite(n.PriceUSD) || !finite(n.Delta) || n.PriceUSD < 0 || (i > 0 && n.Sigma <= nodes[i-1].Sigma) {
			return nil, fmt.Errorf("invalid volatility mesh node: %w", ErrInvalidInput)
		}
	}
	owned := slices.Clone(nodes)
	raw, err := json.Marshal(struct {
		Model string    `json:"model"`
		Nodes []VolNode `json:"nodes"`
	}{VolMeshModelID, owned})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	return &VolMesh{nodes: owned, digest: hex.EncodeToString(digest[:])}, nil
}

type rationalInterval struct{ lower, upper *big.Rat }

func rationalFloat(v float64) *big.Rat { return new(big.Rat).SetFloat64(v) }

// affine evaluates an exact rational vertex interpolation at fraction t.
func affine(a, b float64, t *big.Rat) *big.Rat {
	left := rationalFloat(a)
	diff := new(big.Rat).Sub(rationalFloat(b), left)
	return new(big.Rat).Add(left, new(big.Rat).Mul(t, diff))
}

// Compatible finds the complete preimage of the disclosed market interval and
// the complete delta image, segment by segment. Exact rational arithmetic avoids
// false emptiness at knots or inward endpoint rounding from floating division.
func (m *VolMesh) Compatible(observation MarketPriceInterval) (CompatibleVolSet, error) {
	if m == nil || len(m.nodes) < 2 || observation.BidCents < 0 || observation.AskCents < observation.BidCents || !finite(observation.PriceAllowanceUSD) || observation.PriceAllowanceUSD < 0 || !finite(observation.DeltaAllowance) || observation.DeltaAllowance < 0 {
		return CompatibleVolSet{}, fmt.Errorf("invalid market interval or mesh: %w", ErrInvalidInput)
	}
	allow := rationalFloat(observation.PriceAllowanceUSD)
	low := new(big.Rat).Sub(new(big.Rat).SetFrac64(observation.BidCents, 100), allow)
	high := new(big.Rat).Add(new(big.Rat).SetFrac64(observation.AskCents, 100), allow)
	deltaAllow := rationalFloat(observation.DeltaAllowance)
	zero, one := new(big.Rat), new(big.Rat).SetInt64(1)
	var sigmas, deltas []rationalInterval
	for i := range len(m.nodes) - 1 {
		a, b := m.nodes[i], m.nodes[i+1]
		p0, p1 := rationalFloat(a.PriceUSD), rationalFloat(b.PriceUSD)
		lower, upper := new(big.Rat), new(big.Rat).SetInt64(1)
		diff := new(big.Rat).Sub(p1, p0)
		if diff.Sign() == 0 {
			if p0.Cmp(low) < 0 || p0.Cmp(high) > 0 {
				continue
			}
		} else {
			lower.Quo(new(big.Rat).Sub(low, p0), diff)
			upper.Quo(new(big.Rat).Sub(high, p0), diff)
			if lower.Cmp(upper) > 0 {
				lower, upper = upper, lower
			}
			if lower.Cmp(zero) < 0 {
				lower = new(big.Rat)
			}
			if upper.Cmp(one) > 0 {
				upper = new(big.Rat).SetInt64(1)
			}
			if lower.Cmp(upper) > 0 {
				continue
			}
		}
		sigmas = append(sigmas, rationalInterval{affine(a.Sigma, b.Sigma, lower), affine(a.Sigma, b.Sigma, upper)})
		// Find extrema before merging sigma intervals. A connected compatible
		// component may cross knots and have an interior delta extremum.
		d0, d1 := affine(a.Delta, b.Delta, lower), affine(a.Delta, b.Delta, upper)
		if d0.Cmp(d1) > 0 {
			d0, d1 = d1, d0
		}
		deltas = append(deltas, rationalInterval{new(big.Rat).Sub(d0, deltaAllow), new(big.Rat).Add(d1, deltaAllow)})
	}
	if len(sigmas) == 0 {
		return CompatibleVolSet{}, ErrEmptyCompatibleSet
	}
	sigmas, deltas = mergeRationalIntervals(sigmas), mergeRationalIntervals(deltas)
	result := CompatibleVolSet{Model: VolMeshModelID, CurveSHA256: m.digest, Observation: observation}
	var err error
	result.SigmaIntervals, err = outwardIntervals(sigmas)
	if err != nil {
		return CompatibleVolSet{}, err
	}
	result.DeltaIntervals, err = outwardIntervals(deltas)
	if err != nil {
		return CompatibleVolSet{}, err
	}
	result.DeltaEnvelope = ClosedInterval{result.DeltaIntervals[0].Lower, result.DeltaIntervals[len(result.DeltaIntervals)-1].Upper}
	return result, nil
}

func mergeRationalIntervals(intervals []rationalInterval) []rationalInterval {
	slices.SortFunc(intervals, func(a, b rationalInterval) int { return a.lower.Cmp(b.lower) })
	merged := intervals[:0]
	for _, interval := range intervals {
		if len(merged) == 0 || merged[len(merged)-1].upper.Cmp(interval.lower) < 0 {
			merged = append(merged, interval)
			continue
		}
		if interval.upper.Cmp(merged[len(merged)-1].upper) > 0 {
			merged[len(merged)-1].upper = interval.upper
		}
	}
	return merged
}

func outwardIntervals(intervals []rationalInterval) ([]ClosedInterval, error) {
	result := make([]ClosedInterval, 0, len(intervals))
	for _, interval := range intervals {
		lower, err := outwardFloat(interval.lower, false)
		if err != nil {
			return nil, err
		}
		upper, err := outwardFloat(interval.upper, true)
		if err != nil {
			return nil, err
		}
		result = append(result, ClosedInterval{lower, upper})
	}
	return result, nil
}

func outwardFloat(value *big.Rat, upper bool) (float64, error) {
	f, exact := value.Float64()
	if !finite(f) {
		return 0, fmt.Errorf("compatible range cannot be represented: %w", ErrNonConverged)
	}
	if !exact {
		comparison := rationalFloat(f).Cmp(value)
		if (upper && comparison < 0) || (!upper && comparison > 0) {
			direction := math.Inf(-1)
			if upper {
				direction = math.Inf(1)
			}
			f = math.Nextafter(f, direction)
		}
	}
	if !finite(f) {
		return 0, fmt.Errorf("outward range overflows: %w", ErrNonConverged)
	}
	return f, nil
}
