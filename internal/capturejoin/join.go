// Package capturejoin joins raw closed-capture snapshots at identical cuts, separate from frozen replay.
// It never qualifies terms/roster/Greeks/execution/fills/P&L and never retimes or clock-sorts.
// Outer attestation (acquisition/compressed/hash/ref/stability) is required; Join proves no execution interval.
package capturejoin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"

	"autowheel/internal/causalbook"
)

// MaxSources bounds sources; each <=MaxInstruments, cuts <=MaxCuts; memory is one bounded capture.
const MaxSources = 32

var (
	ErrScope    = errors.New("capturejoin: invalid scope or cuts")
	ErrOverlap  = errors.New("capturejoin: overlapping dataset instrument")
	ErrCapture  = errors.New("capturejoin: invalid capture snapshot")
	ErrTrailing = errors.New("capturejoin: invalid trailing data")
)

// Source supplies exactly one Snapshot per cut, then real io.EOF.
type Source struct {
	Scope causalbook.Scope
	Next  func() (causalbook.Snapshot, error)
}

// CaptureSet groups parts at one identical closed cut; header facts stay separate per Snapshot.
type CaptureSet struct {
	Through   uint64
	Snapshots []causalbook.Snapshot
}

// Summary counts output; valid only after outer checks. Zero on ANY error.
type Summary struct {
	Sets         uint64
	Snapshots    uint64
	Observations uint64
}
type boundSource struct {
	dataset string
	utc     uint64
	ids     []uint32
	next    func() (causalbook.Snapshot, error)
}
type overlapKey struct {
	dataset string
	id      uint32
}

// Join reads ALL sources for one cut before emit, clones observations, preserves raw clocks/fields/status
// without normalization, and requires exact dataset/UTCStart/Through/maxAge plus sorted ID count/identity.
// BOOK/TRADE InstrumentID must match when present. All callbacks are provisional until every real EOF
// and outer checks succeed; Join returns zero Summary on ANY error including cancellation.
func Join(ctx context.Context, sources []Source, cuts []uint64, maxAge uint64, emit func(CaptureSet) error) (Summary, error) {
	if ctx == nil || emit == nil || len(sources) == 0 || len(sources) > MaxSources || len(cuts) == 0 || len(cuts) > causalbook.MaxCuts || maxAge > causalbook.DayNanos {
		return Summary{}, ErrScope
	}
	want := slices.Clone(cuts)
	bounds := make([]*boundSource, len(sources))
	var utc uint64
	for i := range len(sources) {
		if sources[i].Next == nil {
			return Summary{}, ErrScope
		}
		ds := sources[i].Scope.Dataset
		if ds != "OPRA.PILLAR" && ds != "EQUS.MINI" {
			return Summary{}, ErrScope
		}
		us := sources[i].Scope.UTCStart
		if us == 0 || us%causalbook.DayNanos != 0 || us > math.MaxUint64-causalbook.DayNanos {
			return Summary{}, ErrScope
		}
		if i == 0 {
			utc = us
		} else if us != utc {
			return Summary{}, ErrScope
		}
		ids := slices.Clone(sources[i].Scope.Instruments)
		if len(ids) == 0 || len(ids) > causalbook.MaxInstruments {
			return Summary{}, ErrScope
		}
		slices.Sort(ids)
		for j := range ids {
			if ids[j] == 0 || (j > 0 && ids[j] == ids[j-1]) {
				return Summary{}, ErrScope
			}
		}
		b := new(boundSource)
		b.dataset, b.utc, b.ids, b.next = ds, us, ids, sources[i].Next
		bounds[i] = b
	}
	end := utc + causalbook.DayNanos
	for i := range want {
		if want[i] < utc || want[i] >= end || (i > 0 && want[i] <= want[i-1]) {
			return Summary{}, ErrScope
		}
	}
	seen := make(map[overlapKey]struct{})
	for _, b := range bounds {
		for _, id := range b.ids {
			k := overlapKey{b.dataset, id}
			if _, ok := seen[k]; ok {
				return Summary{}, ErrOverlap
			}
			seen[k] = struct{}{}
		}
	}
	var out Summary
	for ci := range len(want) {
		if err := ctx.Err(); err != nil {
			return Summary{}, err
		}
		cut := want[ci]
		parts := make([]causalbook.Snapshot, len(bounds))
		var observations uint64
		for si := range len(bounds) {
			if err := ctx.Err(); err != nil {
				return Summary{}, err
			}
			snap, err := bounds[si].next()
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return Summary{}, err
				}
				return Summary{}, fmt.Errorf("%w: source %d cut %d: %w", ErrCapture, si, cut, err)
			}
			if snap.Dataset != bounds[si].dataset || snap.UTCStart != utc || snap.Through != cut || snap.MaxAgeNanos != maxAge || len(snap.Observations) != len(bounds[si].ids) {
				return Summary{}, fmt.Errorf("%w: source %d cut %d header", ErrCapture, si, cut)
			}
			for oi := range snap.Observations {
				o := snap.Observations[oi]
				if o.InstrumentID != bounds[si].ids[oi] {
					return Summary{}, fmt.Errorf("%w: source %d cut %d identity", ErrCapture, si, cut)
				}
				if o.HasBook && o.Book.InstrumentID != o.InstrumentID {
					return Summary{}, fmt.Errorf("%w: book instrument", ErrCapture)
				}
				if o.HasTrade && o.Trade.InstrumentID != o.InstrumentID {
					return Summary{}, fmt.Errorf("%w: trade instrument", ErrCapture)
				}
			}
			parts[si] = causalbook.Snapshot{Dataset: snap.Dataset, UTCStart: snap.UTCStart, Through: snap.Through, MaxAgeNanos: snap.MaxAgeNanos, Observations: slices.Clone(snap.Observations)}
			observations += uint64(len(snap.Observations))
		}
		if err := ctx.Err(); err != nil {
			return Summary{}, err
		}
		if err := emit(CaptureSet{Through: cut, Snapshots: parts}); err != nil {
			return Summary{}, err
		}
		out.Sets++
		out.Snapshots += uint64(len(parts))
		out.Observations += observations
	}
	for si := range len(bounds) {
		if err := ctx.Err(); err != nil {
			return Summary{}, err
		}
		snap, err := bounds[si].next()
		if err == nil {
			return Summary{}, fmt.Errorf("%w: source %d extra data", ErrTrailing, si)
		}
		// A wrapped EOF can carry a read/transport failure. Only the literal
		// sentinel with no packet certifies this source protocol's completion.
		if err != io.EOF {
			if errors.Is(err, io.EOF) {
				return Summary{}, fmt.Errorf("%w: source %d wrapped eof: %w", ErrTrailing, si, err)
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return Summary{}, err
			}
			return Summary{}, fmt.Errorf("%w: source %d trailing: %w", ErrTrailing, si, err)
		}
		if snap.Dataset != "" || snap.UTCStart != 0 || snap.Through != 0 || snap.MaxAgeNanos != 0 || len(snap.Observations) != 0 {
			return Summary{}, fmt.Errorf("%w: source %d packet with eof", ErrTrailing, si)
		}
	}
	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}
	return out, nil
}
