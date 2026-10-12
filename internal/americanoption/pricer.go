// Package americanoption is an offline research American option solver
// for q=0 with explicit deterministic positive cash dividends at exact
// time.Time instants. No-cash calls at nonnegative rates use the analytical
// no-early-exercise identity; all other cases use the FD obstacle solver.
//
// Scope: synthetic numeric research only. Model arithmetic uses USD floats;
// ledger money is never computed here. An explicit empty dividend schedule
// is valid SYNTHETIC numeric input (explicit zero synthetic cash), with no
// causal or real-world zero-dividend claim.
//
// Method: log-spot uniform grid containing spot and strike, Black-Scholes
// log generator (central when off-diagonals are nonnegative, otherwise
// upwind drift), Crank-Nicolson with implicit-Euler (Rannacher) damping
// around terminal payoff and each dividend jump, American obstacle
// A v >= b, v >= intrinsic via bounded projected SOR with residual check,
// and explicit cash backward jump
// V_before(S) = max(intrinsic(S), V_after(max(0,S-D))) with linear
// interpolation in S (approximate, no exactness claim).
//
// Boundaries: lower absorbing-0 (call 0, put K*max(1,exp(-r*tau)));
// upper put 0; upper call max over immediate, maturity, and each pre-cash
// linear payoff Smax-PVcash-Kdisc. Smin is 8-sigma plus cash plus guard
// remote (probabilistically tiny); Smax remote. Boundary, space, time, and
// jump errors are measured sources, not declared proof. Do not infer a full
// source Greek gate passed.
package americanoption

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
)

// ModelID identifies this offline research American FD engine.
// It is distinct from the European continuous-q model.
const ModelID = "American-cashDiv-q0-analyticCall-FD-v2"

// Kind is the option exercise kind (American).
type Kind string

const (
	// Call is an American call.
	Call Kind = "call"
	// Put is an American put.
	Put Kind = "put"
)

// CashDividend is a deterministic positive cash amount paid at an exact
// ex-div instant. Amounts are mathematical USD floats, never ledger money.
type CashDividend struct {
	AmountUSD float64
	ExAt      time.Time
}

// Input prices an American option with q=0 and discrete cash dividends.
type Input struct {
	Kind      Kind
	SpotUSD   float64
	StrikeUSD float64
	Sigma     float64
	Rate      float64
	Now       time.Time
	Expiry    time.Time
	Dividends []CashDividend
}

// Grid controls discretization. TimeSteps and SpaceSteps are interval
// counts (not node counts).
type Grid struct {
	TimeSteps  int
	SpaceSteps int
}

// Value is the mathematical price and delta (no rounding to cents,
// no clamping to manufacture correctness).
type Value struct {
	PriceUSD float64
	Delta    float64
}

var (
	// ErrInvalidInput rejects missing, ambiguous, or out-of-envelope data.
	ErrInvalidInput = errors.New("americanoption: invalid input")
	// ErrNonConverged reports numerical failure (PSOR, bounds, non-finite).
	ErrNonConverged = errors.New("americanoption: non-converged")
)

const (
	maxSpotStrike = 10_000_000.0
	minSigma      = 1e-4
	maxSigma      = 5.0
	minRate       = -0.05
	maxRate       = 1.0
	maxDivs       = 16
	minSteps      = 256
	maxSteps      = 8192
	// maxWork bounds TimeSteps*SpaceSteps to refuse large work instead of OOM.
	// Memory is O(SpaceSteps); work is O(TimeSteps*SpaceSteps*PSORiters).
	// 4096x4096 is allowed; 8192x8192 is refused.
	maxWork       = 16_777_216
	yearNs  int64 = 365 * 24 * 3600 * 1_000_000_000
	tenorNs int64 = 9 * 24 * 3600 * 1_000_000_000
)

func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

// Price returns the mathematical American price and delta.
func Price(in Input, grid Grid) (Value, error) {
	T, divT, totalCash, err := validate(in, grid)
	if err != nil {
		return Value{}, err
	}
	// With q=0, no cash jumps and r>=0, waiting dominates exercising a
	// call at every instant. Its American price and spot delta therefore
	// equal the European analytical values. This exact model identity
	// avoids artificial upwind diffusion at low volatility. Never apply
	// it to puts, negative-rate calls, or a nonempty cash schedule.
	// Healy (2021), Proposition 1: https://arxiv.org/abs/2109.15157.
	// Explicit zero model cash does not establish historical dividend absence.
	if in.Kind == Call && in.Rate >= 0 && len(in.Dividends) == 0 {
		return analyticNoCashCall(in, T)
	}
	r := in.Rate
	sig := in.Sigma
	s0 := in.SpotUSD
	k := in.StrikeUSD
	call := in.Kind == Call
	N := grid.SpaceSteps
	M := grid.TimeSteps

	// Space grid: log-spot containing spot and strike.
	x, S, iSpot, _, intrinsic, err := buildSpaceGrid(s0, k, sig, T, r, totalCash, N, call)
	if err != nil {
		return Value{}, err
	}

	// Time partition including exact ns event times.
	times, damp := buildTimePartition(T, divT, M)

	// Dirichlet boundaries at each time node.
	lowB, highB := buildBoundaries(times, T, divT, in.Dividends, r, k, S[N], call)

	// Log-generator coefficients (fixed across steps).
	a, bdiag, c := buildGenerator(x, sig, r)

	// Terminal payoff.
	vNext := make([]float64, N+1)
	copy(vNext, intrinsic)
	// Enforce terminal Dirichlet consistency at interior? Payoff already
	// equals intrinsic; boundaries at maturity equal intrinsic by construction
	// of vNext (payoff), independent of lowB/highB[M].
	vCur := make([]float64, N+1)
	rhs := make([]float64, N+1)
	guess := make([]float64, N+1)

	tol := 1e-9 * max(1.0, k, s0)
	if s0 == k && ((!call && r > 0) || (call && r < 0)) && sig/(2*math.Abs(r)) < math.Sqrt(T) {
		// A micro-price in a thin contact layer needs a tighter absolute
		// residual to resolve its delta. Keep the ordinary tolerance elsewhere.
		tol *= .01
	}

	// Backward loop over forward intervals [times[i], times[i+1]].
	for i := M - 1; i >= 0; i-- {
		dt := times[i+1] - times[i]
		if !(dt > 0) || math.IsNaN(dt) || math.IsInf(dt, 0) {
			return Value{}, fmt.Errorf("bad dt: %w", ErrNonConverged)
		}
		useImplicit := damp[i]
		// Build RHS for interior 1..N-1.
		if useImplicit {
			// A v = vNext with boundary move.
			for j := 1; j <= N-1; j++ {
				rhs[j] = vNext[j]
			}
			rhs[1] += dt * a[1] * lowB[i]
			rhs[N-1] += dt * c[N-1] * highB[i]
		} else {
			half := 0.5 * dt
			for j := 1; j <= N-1; j++ {
				rhs[j] = vNext[j] + half*(a[j]*vNext[j-1]+(bdiag[j]-r)*vNext[j]+c[j]*vNext[j+1])
			}
			// Next-time boundary contributions are already in the stencil
			// above via vNext[0], vNext[N] for j=1,N-1. Add current-time
			// boundary move: +0.5*dt*a1*lowB[i] etc.
			rhs[1] += half * a[1] * lowB[i]
			rhs[N-1] += half * c[N-1] * highB[i]
		}
		// Initial guess: max(continuation, intrinsic).
		for j := 1; j <= N-1; j++ {
			if vNext[j] > intrinsic[j] {
				guess[j] = vNext[j]
			} else {
				guess[j] = intrinsic[j]
			}
		}
		// Boundary contributions have already moved to rhs. The solver's
		// boundary entries remain zero; using lowB/highB here would count twice.
		if err := psor(a, bdiag, c, r, dt, useImplicit, rhs, intrinsic, guess, tol); err != nil {
			return Value{}, err
		}
		// Assemble full vector.
		vCur[0] = lowB[i]
		for j := 1; j <= N-1; j++ {
			vCur[j] = guess[j]
		}
		vCur[N] = highB[i]
		// Enforce obstacle at boundaries? Boundaries are Dirichlet by
		// construction and dominate intrinsic there (Smin tiny, Smax remote);
		// still project to avoid 1-ulp violation breaking bound checks.
		if vCur[0] < intrinsic[0] {
			vCur[0] = intrinsic[0]
		}
		if vCur[N] < intrinsic[N] {
			vCur[N] = intrinsic[N]
		}
		copy(vNext, vCur)

		// Cash backward jump when times[i] is a dividend instant.
		if isDivNode(i, times, divT) {
			amt := divAmountAt(times[i], divT, in.Dividends)
			applyCashJump(vNext, S, intrinsic, amt)
		}
		for _, v := range vNext {
			if !finite(v) {
				return Value{}, fmt.Errorf("non-finite grid value: %w", ErrNonConverged)
			}
		}
	}

	price := vNext[iSpot]
	if !finite(price) {
		return Value{}, fmt.Errorf("non-finite price: %w", ErrNonConverged)
	}
	// Arbitrage bound checks (math prices with tolerance, not cents).
	intr0 := intrinsic[iSpot]
	upperTol := tol * 10
	if call {
		if price < intr0-upperTol {
			return Value{}, fmt.Errorf("call below intrinsic: %w", ErrNonConverged)
		}
		if price > s0+upperTol {
			return Value{}, fmt.Errorf("call above spot: %w", ErrNonConverged)
		}
	} else {
		capPut := k * max(1.0, math.Exp(-r*T))
		if price < intr0-upperTol {
			return Value{}, fmt.Errorf("put below intrinsic: %w", ErrNonConverged)
		}
		if price > capPut+upperTol {
			return Value{}, fmt.Errorf("put above discounted strike: %w", ErrNonConverged)
		}
	}
	delta := centralDelta(vNext, S, iSpot)
	if !finite(delta) {
		return Value{}, fmt.Errorf("non-finite delta: %w", ErrNonConverged)
	}
	return Value{PriceUSD: price, Delta: delta}, nil
}

func analyticNoCashCall(in Input, T float64) (Value, error) {
	width := in.Sigma * math.Sqrt(T)
	// Separate logs avoid underflow in SpotUSD/StrikeUSD for extreme inputs.
	d1 := (math.Log(in.SpotUSD) - math.Log(in.StrikeUSD) + (in.Rate+.5*in.Sigma*in.Sigma)*T) / width
	d2 := d1 - width
	cdf := func(x float64) float64 { return .5 * math.Erfc(-x/math.Sqrt2) }
	value := Value{
		PriceUSD: in.SpotUSD*cdf(d1) - in.StrikeUSD*math.Exp(-in.Rate*T)*cdf(d2),
		Delta:    cdf(d1),
	}
	if !finite(value.PriceUSD) || !finite(value.Delta) || value.PriceUSD < 0 || value.PriceUSD > in.SpotUSD || value.Delta < 0 || value.Delta > 1 {
		return Value{}, fmt.Errorf("analytical call cannot be represented: %w", ErrNonConverged)
	}
	return value, nil
}

// validate checks the envelope and returns tenor/contact data.
// Times use integer nanoseconds throughout; no Duration*i scaling.
func validate(in Input, grid Grid) (T float64, divT []float64, totalCash float64, err error) {
	if grid.TimeSteps < minSteps || grid.TimeSteps > maxSteps || grid.SpaceSteps < minSteps || grid.SpaceSteps > maxSteps {
		return 0, nil, 0, fmt.Errorf("grid %dx%d outside [%d,%d]: %w", grid.TimeSteps, grid.SpaceSteps, minSteps, maxSteps, ErrInvalidInput)
	}
	if int64(grid.TimeSteps)*int64(grid.SpaceSteps) > maxWork {
		return 0, nil, 0, fmt.Errorf("grid work %d exceeds %d: %w", int64(grid.TimeSteps)*int64(grid.SpaceSteps), maxWork, ErrInvalidInput)
	}
	if in.Kind != Call && in.Kind != Put {
		return 0, nil, 0, fmt.Errorf("bad kind %q: %w", in.Kind, ErrInvalidInput)
	}
	if !finite(in.SpotUSD) || in.SpotUSD <= 0 || in.SpotUSD > maxSpotStrike {
		return 0, nil, 0, fmt.Errorf("spot out of envelope: %w", ErrInvalidInput)
	}
	if !finite(in.StrikeUSD) || in.StrikeUSD <= 0 || in.StrikeUSD > maxSpotStrike {
		return 0, nil, 0, fmt.Errorf("strike out of envelope: %w", ErrInvalidInput)
	}
	if !finite(in.Sigma) || in.Sigma < minSigma || in.Sigma > maxSigma {
		return 0, nil, 0, fmt.Errorf("sigma out of envelope: %w", ErrInvalidInput)
	}
	if !finite(in.Rate) || in.Rate < minRate || in.Rate > maxRate {
		return 0, nil, 0, fmt.Errorf("rate out of envelope: %w", ErrInvalidInput)
	}
	if in.Now.IsZero() || in.Expiry.IsZero() {
		return 0, nil, 0, fmt.Errorf("zero time: %w", ErrInvalidInput)
	}
	if !in.Expiry.After(in.Now) {
		return 0, nil, 0, fmt.Errorf("expiry must be after now: %w", ErrInvalidInput)
	}
	totalNs := in.Expiry.Sub(in.Now).Nanoseconds()
	if totalNs <= 0 {
		return 0, nil, 0, fmt.Errorf("nonpositive tenor: %w", ErrInvalidInput)
	}
	if totalNs > tenorNs {
		return 0, nil, 0, fmt.Errorf("tenor exceeds 9 days: %w", ErrInvalidInput)
	}
	if len(in.Dividends) > maxDivs {
		return 0, nil, 0, fmt.Errorf("too many dividends: %w", ErrInvalidInput)
	}
	T = float64(totalNs) / float64(yearNs)
	if !(T > 0) || math.IsNaN(T) || math.IsInf(T, 0) {
		return 0, nil, 0, fmt.Errorf("bad tenor: %w", ErrInvalidInput)
	}
	divT = make([]float64, len(in.Dividends))
	for i, d := range in.Dividends {
		if !finite(d.AmountUSD) || d.AmountUSD <= 0 {
			return 0, nil, 0, fmt.Errorf("dividend amount must be finite positive: %w", ErrInvalidInput)
		}
		if d.ExAt.IsZero() {
			return 0, nil, 0, fmt.Errorf("zero ex-div time: %w", ErrInvalidInput)
		}
		if !d.ExAt.After(in.Now) || !d.ExAt.Before(in.Expiry) {
			return 0, nil, 0, fmt.Errorf("ex-div must satisfy Now<ExAt<Expiry: %w", ErrInvalidInput)
		}
		if i > 0 && !d.ExAt.After(in.Dividends[i-1].ExAt) {
			return 0, nil, 0, fmt.Errorf("dividends must be strictly ascending unique: %w", ErrInvalidInput)
		}
		ns := d.ExAt.Sub(in.Now).Nanoseconds()
		if ns <= 0 || ns >= totalNs {
			return 0, nil, 0, fmt.Errorf("dividend ns outside (0,T): %w", ErrInvalidInput)
		}
		divT[i] = float64(ns) / float64(yearNs)
		if !finite(divT[i]) || divT[i] <= 0 || divT[i] >= T || (i > 0 && divT[i] <= divT[i-1]) {
			return 0, nil, 0, fmt.Errorf("dividend times collapse in the numerical clock: %w", ErrNonConverged)
		}
		totalCash += d.AmountUSD
	}
	if !finite(totalCash) {
		return 0, nil, 0, fmt.Errorf("bad cash total: %w", ErrInvalidInput)
	}
	return T, divT, totalCash, nil
}

// buildSpaceGrid returns log nodes x, spot nodes S, spot index, strike index,
// and intrinsic payoff. The grid contains both spot and strike nodes.
func buildSpaceGrid(s0, k, sig, T, rate, totalCash float64, N int, call bool) (x, S []float64, iSpot, iStrike int, intrinsic []float64, err error) {
	logS := math.Log(s0)
	logK := math.Log(k)
	volW := 8 * sig * math.Sqrt(T)
	cashW := math.Log1p(totalCash / min(s0, k))
	const guard = 0.5
	W := volW + cashW + guard
	lo := min(logS, logK) - W
	hi := max(logS, logK) + W
	if !finite(lo) || !finite(hi) || !(hi > lo) || (logS == logK && s0 != k) {
		return nil, nil, 0, 0, nil, fmt.Errorf("unrepresentable log-space domain: %w", ErrNonConverged)
	}
	L := math.Abs(logS - logK)
	x = make([]float64, N+1)
	if s0 == k {
		i0 := N / 2
		// Keep the wide boundary domain, but resolve the payoff kink on its
		// diffusion scale. Uniform spacing across the fixed guard leaves too
		// few central nodes at low volatility. The existing nonuniform
		// generator and delta formula apply; there is no sigma cutoff.
		scale := sig * math.Sqrt(T)
		// With a carry incentive to exercise, the low-volatility contact
		// layer can be much narrower than the terminal diffusion width.
		// Its sigma-squared/rate scale is a resolution choice, not an
		// asserted exercise boundary or a global error bound.
		if (!call && rate > 0) || (call && rate < 0) {
			scale = min(scale, sig*sig/(2*math.Abs(rate)))
		}
		left := math.Asinh((logS - lo) / scale)
		right := math.Asinh((hi - logS) / scale)
		for i := range x {
			y := float64(i-i0) * left / float64(i0)
			if i > i0 {
				y = float64(i-i0) * right / float64(N-i0)
			}
			x[i] = logS + scale*math.Sinh(y)
		}
		x[i0] = logS
		iSpot = i0
		iStrike = i0
	} else {
		dxTarget := (hi - lo) / float64(N)
		kk := int(math.Round(L / dxTarget))
		kk = max(1, kk)
		kk = min(kk, N-1)
		placed := false
		for ; kk >= 1; kk-- {
			dx := L / float64(kk)
			mn := min(logS, logK)
			mx := max(logS, logK)
			// Check float counts before converting them to indices. Extreme
			// finite inputs can produce counts outside the integer range.
			lowCount := math.Ceil((mn - lo) / dx)
			highCount := math.Ceil((hi - mx) / dx)
			if !finite(lowCount) || !finite(highCount) || lowCount < 1 || highCount < 1 || lowCount > float64(N-kk) || highCount > float64(N-kk) {
				continue
			}
			wLow, wHigh := int(lowCount), int(highCount)
			if kk+wLow+wHigh <= N {
				xLow := mn - float64(wLow)*dx
				for i := range x {
					x[i] = xLow + float64(i)*dx
				}
				// Snap both to exact logs (1-ulp non-uniformity at most).
				var iMn, iMx int
				iMn = wLow
				iMx = wLow + kk
				x[iMn] = mn
				x[iMx] = mx
				if logS < logK {
					iSpot, iStrike = iMn, iMx
				} else {
					iSpot, iStrike = iMx, iMn
				}
				placed = true
				break
			}
		}
		if !placed {
			// Tiny-L fallback: uniform N-1 intervals aligned to spot, then
			// insert strike. Exactly N intervals, one non-uniform kink.
			n0 := N - 1
			dxU := (hi - lo) / float64(n0)
			iS := n0 / 2
			xLow := logS - float64(iS)*dxU
			base := make([]float64, n0+1)
			for i := range base {
				base[i] = xLow + float64(i)*dxU
			}
			base[iS] = logS
			pos, found := slices.BinarySearch(base, logK)
			x = make([]float64, N+1)
			switch {
			case pos <= 0:
				x[0] = logK
				copy(x[1:], base[:N])
				iStrike = 0
				iSpot = iS + 1
			case pos >= len(base):
				copy(x[:N], base[:N])
				x[N] = logK
				iStrike = N
				// spot shifts only if insertion before it; here after, unchanged.
				iSpot = iS
			default:
				if found {
					// Already exactly on a node; extend to N intervals.
					copy(x[:n0+1], base)
					// Extend upper by one dxU to reach N intervals.
					x[N] = base[n0] + dxU
					iSpot = iS
					iStrike = pos
				} else {
					copy(x[:pos], base[:pos])
					x[pos] = logK
					copy(x[pos+1:], base[pos:N])
					iStrike = pos
					if pos <= iS {
						iSpot = iS + 1
					} else {
						iSpot = iS
					}
				}
			}
			x[iSpot] = logS
			x[iStrike] = logK
		}
	}
	S = make([]float64, N+1)
	for i, xv := range x {
		S[i] = math.Exp(xv)
	}
	S[iSpot] = s0
	S[iStrike] = k
	if iSpot <= 0 || iSpot >= N {
		return nil, nil, 0, 0, nil, fmt.Errorf("spot is not an interior node: %w", ErrNonConverged)
	}
	for i := range x {
		if !finite(x[i]) || !finite(S[i]) || S[i] <= 0 || (i > 0 && (x[i] <= x[i-1] || S[i] <= S[i-1])) {
			return nil, nil, 0, 0, nil, fmt.Errorf("non-finite or non-increasing space grid: %w", ErrNonConverged)
		}
	}
	intrinsic = make([]float64, N+1)
	if call {
		for i, s := range S {
			intrinsic[i] = max(0.0, s-k)
		}
	} else {
		for i, s := range S {
			intrinsic[i] = max(0.0, k-s)
		}
	}
	return x, S, iSpot, iStrike, intrinsic, nil
}

// buildTimePartition distributes M intervals across dividend segments with
// at least 2 steps per segment; nodes include each exact ns event time.
// damp[i] marks implicit-Euler damping for the last 2 forward steps of each
// segment (first 2 backward steps from payoff/jump).
func buildTimePartition(T float64, divT []float64, M int) (times []float64, damp []bool) {
	segs := len(divT) + 1
	bounds := make([]float64, segs+1)
	bounds[0] = 0
	copy(bounds[1:], divT)
	bounds[segs] = T
	n := make([]int, segs)
	for i := range n {
		n[i] = 2
	}
	rem := M - 2*segs
	lengths := make([]float64, segs)
	for j := 0; j < segs; j++ {
		lengths[j] = bounds[j+1] - bounds[j]
	}
	// Distribute remainder proportionally to segment length.
	extras := make([]int, segs)
	if rem > 0 {
		frac := make([]float64, segs)
		for j := range lengths {
			ideal := float64(rem) * lengths[j] / T
			extras[j] = int(math.Floor(ideal))
			frac[j] = ideal - float64(extras[j])
		}
		left := rem
		for _, e := range extras {
			left -= e
		}
		order := make([]int, segs)
		for j := range order {
			order[j] = j
		}
		slices.SortFunc(order, func(p, q int) int {
			return cmp.Or(cmp.Compare(frac[q], frac[p]), cmp.Compare(p, q))
		})
		for j := 0; j < left; j++ {
			extras[order[j%len(order)]]++
		}
		for j := range n {
			n[j] += extras[j]
		}
	}
	times = make([]float64, M+1)
	damp = make([]bool, M)
	idx := 0
	times[0] = 0
	for j := 0; j < segs; j++ {
		dt := lengths[j] / float64(n[j])
		for s := 0; s < n[j]; s++ {
			times[idx+1] = bounds[j] + float64(s+1)*dt
			// Damping for last 2 forward steps of this segment.
			if s >= n[j]-2 {
				damp[idx] = true
			}
			idx++
		}
	}
	times[M] = T
	// Restore exact event times at segment ends (avoid cumulative error).
	cum := 0
	for j := 0; j < segs; j++ {
		cum += n[j]
		times[cum] = bounds[j+1]
	}
	return times, damp
}

// buildBoundaries computes Dirichlet values per time node.
func buildBoundaries(times []float64, T float64, divT []float64, divs []CashDividend, r, k, sMax float64, call bool) (low, high []float64) {
	M := len(times) - 1
	low = make([]float64, M+1)
	high = make([]float64, M+1)
	for i, t := range times {
		tau := T - t
		if tau < 0 {
			tau = 0
		}
		if call {
			low[i] = 0
			// Upper: max over immediate, maturity, each pre-cash exercise.
			best := sMax - k
			discT := math.Exp(-r * tau)
			pvAll := 0.0
			for j, tj := range divT {
				if tj > t {
					pvAll += divs[j].AmountUSD * math.Exp(-r*(tj-t))
				}
			}
			cand := sMax - pvAll - k*discT
			best = max(best, cand)
			for _, tj := range divT {
				if tj <= t {
					continue
				}
				pvBefore := 0.0
				for q, tq := range divT {
					if tq > t && tq < tj {
						pvBefore += divs[q].AmountUSD * math.Exp(-r*(tq-t))
					}
				}
				cj := sMax - pvBefore - k*math.Exp(-r*(tj-t))
				best = max(best, cj)
			}
			best = max(best, 0.0)
			high[i] = best
		} else {
			low[i] = k * max(1.0, math.Exp(-r*tau))
			high[i] = 0
		}
	}
	return low, high
}

// buildGenerator returns tridiagonal log-generator coefficients.
// Central when all off-diagonals are nonnegative, else upwind drift
// (first-order, not guaranteed vol-monotone; documented limitation).
func buildGenerator(x []float64, sig, r float64) (a, b, c []float64) {
	N := len(x) - 1
	a = make([]float64, N+1)
	b = make([]float64, N+1)
	c = make([]float64, N+1)
	mu := r - 0.5*sig*sig
	sig2 := sig * sig
	ac := make([]float64, N+1)
	cc := make([]float64, N+1)
	bc := make([]float64, N+1)
	for j := 1; j <= N-1; j++ {
		h1 := x[j] - x[j-1]
		h2 := x[j+1] - x[j]
		den := h1 * h2 * (h1 + h2)
		ac[j] = (-mu*h2*h2 + sig2*h2) / den
		cc[j] = (mu*h1*h1 + sig2*h1) / den
		bc[j] = mu*(h2*h2-h1*h1)/den - sig2*(h1+h2)/den
	}
	centralOK := true
	for j := 1; j <= N-1; j++ {
		if ac[j] < -1e-14 || cc[j] < -1e-14 {
			centralOK = false
			break
		}
	}
	if centralOK {
		copy(a, ac)
		copy(b, bc)
		copy(c, cc)
		return a, b, c
	}
	for j := 1; j <= N-1; j++ {
		h1 := x[j] - x[j-1]
		h2 := x[j+1] - x[j]
		aDiff := sig2 / (h1 * (h1 + h2))
		cDiff := sig2 / (h2 * (h1 + h2))
		bDiff := -sig2 / (h1 * h2)
		switch {
		case mu > 0:
			a[j] = aDiff
			c[j] = cDiff + mu/h2
			b[j] = bDiff - mu/h2
		case mu < 0:
			a[j] = aDiff - mu/h1
			c[j] = cDiff
			b[j] = bDiff + mu/h1
		default:
			a[j] = ac[j]
			b[j] = bc[j]
			c[j] = cc[j]
		}
	}
	return a, b, c
}

// psor solves the obstacle LCP A x >= rhs, x >= intrinsic with projected SOR.
// A = I - dt*(1-theta)*(L - r), rhs built by caller; useImplicit selects
// theta (true = backward Euler, false = Crank-Nicolson).
func psor(a, bdiag, c []float64, r, dt float64, useImplicit bool, rhs, intrinsic, x []float64, tol float64) error {
	omega := 1.25
	const maxIter = 2000
	// This vector contains only interior unknowns. Dirichlet terms are
	// already in rhs; zero these entries explicitly to prevent double counting.
	x[0], x[len(x)-1] = 0, 0
	// Diagonal of A for interior j=1..N-1 (x index j, interior idx j-1).
	Adiag := make([]float64, len(a))
	for j := 1; j < len(a)-1; j++ {
		if useImplicit {
			Adiag[j] = 1 - dt*(bdiag[j]-r)
		} else {
			Adiag[j] = 1 - 0.5*dt*(bdiag[j]-r)
		}
		if Adiag[j] <= 0 || !finite(Adiag[j]) {
			return fmt.Errorf("degenerate diagonal: %w", ErrNonConverged)
		}
	}
	var Alo, Ahi []float64
	Alo = make([]float64, len(a))
	Ahi = make([]float64, len(a))
	for j := 1; j < len(a)-1; j++ {
		if useImplicit {
			Alo[j] = -dt * a[j]
			Ahi[j] = -dt * c[j]
		} else {
			Alo[j] = -0.5 * dt * a[j]
			Ahi[j] = -0.5 * dt * c[j]
		}
		if !finite(Alo[j]) || !finite(Ahi[j]) {
			return fmt.Errorf("non-finite off-diagonal: %w", ErrNonConverged)
		}
		// Concentrated grids can have strongly coupled diffusion rows.
		// Use a faster relaxation inside (0,2), while retaining the same
		// finite iteration cap and mandatory complementarity residual.
		if (math.Abs(Alo[j])+math.Abs(Ahi[j]))/Adiag[j] > .98 {
			omega = 1.9
		}
	}
	updateTol := tol * 0.01
	residTol := tol
	for it := 0; it < maxIter; it++ {
		maxUpd := 0.0
		for j := 1; j <= len(a)-2; j++ {
			old := x[j]
			sum := rhs[j] - Alo[j]*x[j-1]
			// x[j+1] still holds previous-iteration value on first pass
			// through j ascending: standard forward SOR sweep.
			sum -= Ahi[j] * x[j+1]
			unproj := (1-omega)*old + omega*sum/Adiag[j]
			if !finite(unproj) {
				return fmt.Errorf("non-finite projected SOR update: %w", ErrNonConverged)
			}
			proj := max(unproj, intrinsic[j])
			x[j] = proj
			if d := math.Abs(proj - old); d > maxUpd {
				maxUpd = d
			}
		}
		if maxUpd < updateTol {
			if checkResidual(Alo, Adiag, Ahi, x, rhs, intrinsic, residTol) {
				return nil
			}
		}
		if it == maxIter-1 {
			break
		}
	}
	// Final residual check; update norm alone never suffices.
	if checkResidual(Alo, Adiag, Ahi, x, rhs, intrinsic, residTol) {
		return nil
	}
	return fmt.Errorf("projected SOR did not converge: %w", ErrNonConverged)
}

// checkResidual verifies complementarity: x>=g-tol, Ax-b>=-tol, and
// min(x-g, Ax-b) <= tol, with absolute residuals in mathematical USD.
func checkResidual(Alo, Adiag, Ahi, x, rhs, intrinsic []float64, tol float64) bool {
	if !finite(tol) || tol <= 0 {
		return false
	}
	for j := 1; j <= len(x)-2; j++ {
		ax := Adiag[j]*x[j] + Alo[j]*x[j-1] + Ahi[j]*x[j+1]
		res := ax - rhs[j]
		gap := x[j] - intrinsic[j]
		if !finite(gap) || !finite(res) {
			return false
		}
		if gap < -tol {
			return false
		}
		if res < -tol {
			return false
		}
		if min(gap, res) > tol {
			return false
		}
	}
	return true
}

// isDivNode reports whether forward time index i is a dividend instant.
// times[i] is the left node; the jump applies when leaving a segment whose
// right end is a dividend. We detect right ends via divT match.
func isDivNode(i int, times []float64, divT []float64) bool {
	if len(divT) == 0 {
		return false
	}
	t := times[i]
	for _, d := range divT {
		if t == d {
			return true
		}
	}
	return false
}

func divAmountAt(t float64, divT []float64, divs []CashDividend) float64 {
	for j, d := range divT {
		if d == t {
			return divs[j].AmountUSD
		}
	}
	return 0
}

// applyCashJump maps V_after to V_before via S-D with linear interpolation
// in S, then max with intrinsic. Values below Smin use the absorbing lower
// boundary (already stored in v[0]).
func applyCashJump(v, S, intrinsic []float64, amt float64) {
	N := len(v) - 1
	after := make([]float64, len(v))
	copy(after, v)
	for k := 0; k <= N; k++ {
		target := S[k] - amt
		var cont float64
		switch {
		case target <= S[0]:
			cont = after[0]
		case target >= S[N]:
			cont = after[N]
		default:
			pos, _ := slices.BinarySearch(S, target)
			if pos <= 0 {
				cont = after[0]
			} else if pos >= len(S) {
				cont = after[N]
			} else if S[pos] == target {
				cont = after[pos]
			} else {
				w := (target - S[pos-1]) / (S[pos] - S[pos-1])
				cont = after[pos-1] + w*(after[pos]-after[pos-1])
			}
		}
		v[k] = max(intrinsic[k], cont)
	}
}

// centralDelta differentiates via the quadratic log-grid stencil.
func centralDelta(v, S []float64, i int) float64 {
	// dV/dS = (1/S) dV/dx with non-uniform central dV/dx.
	// Caller guarantees 0 < i < N.
	// Reconstruct x from S would lose precision; use log spacing from S ratios.
	// h1 = ln(Si/Si-1), h2 = ln(Si+1/Si).
	h1 := math.Log(S[i] / S[i-1])
	h2 := math.Log(S[i+1] / S[i])
	den := h1 * h2 * (h1 + h2)
	dvdx := (-h2*h2*v[i-1] + (h2*h2-h1*h1)*v[i] + h1*h1*v[i+1]) / den
	return dvdx / S[i]
}
