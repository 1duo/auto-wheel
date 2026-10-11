package causalbook

import (
	"context"
	"io"
	"slices"

	"autowheel/internal/nativebook"
)

// MaxSelectedObservations bounds copied projection masks to 16 MiB of IDs.
// It is a memory bound, not a strategy position or collateral limit.
const MaxSelectedObservations = 1 << 22

// SelectedWindow requests raw observations for a subset of the attested native
// instrument universe. Selection changes output only: every original record,
// including records outside these windows and instruments, is still validated.
// The outer harness must bind the mask to causal strategy decisions and prove
// continuous coverage of each pending order. A mask alone proves neither.
type SelectedWindow struct {
	Window      Window
	Instruments []uint32
}

// ReduceSelectedIntervals projects the verified interval reducer onto selected
// instruments without changing its book state, clocks or integrity checks.
// Masks are copied before reading, and output IDs are sorted and unique.
// Summary.Observations counts selected output; Summary.Native attests the full
// source. All output remains provisional until this function and the outer
// acquisition/decompression/source-stability checks succeed. An error returns
// a zero summary, even if provisional callbacks have already occurred.
func ReduceSelectedIntervals(ctx context.Context, source io.Reader, expected nativebook.Expected,
	scope Scope, plan []SelectedWindow, maxAge uint64, emit func(IntervalSnapshot) error) (IntervalSummary, error) {
	if ctx == nil || source == nil || emit == nil || len(plan) == 0 || len(plan) > MaxCuts ||
		len(scope.Instruments) == 0 || len(scope.Instruments) > MaxInstruments {
		return IntervalSummary{}, ErrScope
	}
	total := 0
	for _, entry := range plan {
		if len(entry.Instruments) == 0 || len(entry.Instruments) > len(scope.Instruments) ||
			len(entry.Instruments) > MaxSelectedObservations-total {
			return IntervalSummary{}, ErrScope
		}
		total += len(entry.Instruments)
	}
	universe := make(map[uint32]bool, len(scope.Instruments))
	for _, id := range scope.Instruments {
		if id == 0 || universe[id] {
			return IntervalSummary{}, ErrScope
		}
		universe[id] = true
	}
	windows := make([]Window, len(plan))
	masks := make([][]uint32, len(plan))
	for i, entry := range plan {
		ids := slices.Clone(entry.Instruments)
		slices.Sort(ids)
		for j, id := range ids {
			if !universe[id] || j > 0 && id == ids[j-1] {
				return IntervalSummary{}, ErrScope
			}
		}
		windows[i], masks[i] = entry.Window, ids
	}
	index := 0
	summary, err := ReduceIntervals(ctx, source, expected, scope, windows, maxAge,
		func(snapshot IntervalSnapshot) error {
			if index >= len(masks) || snapshot.From != windows[index].From || snapshot.Through != windows[index].Through {
				return ErrScope
			}
			mask := masks[index]
			projected := make([]IntervalObservation, 0, len(mask))
			for _, observation := range snapshot.Observations {
				if _, present := slices.BinarySearch(mask, observation.Endpoint.InstrumentID); present {
					projected = append(projected, observation)
				}
			}
			if len(projected) != len(mask) {
				return ErrScope
			}
			snapshot.Observations = projected
			if err := emit(snapshot); err != nil {
				return err
			}
			index++
			return nil
		})
	if err != nil {
		return IntervalSummary{}, err
	}
	if index != len(plan) || summary.Windows != uint64(len(plan)) {
		return IntervalSummary{}, ErrScope
	}
	summary.Observations = uint64(total)
	return summary, nil
}
