package causalbook

import (
	"context"
	"io"
	"math"
	"slices"

	"autowheel/internal/nativebook"
)

// Window covers (From, Through], with the complete closed state at From carried
// into the interval. Windows may be adjacent, but must not overlap.
type Window struct {
	From, Through uint64
}

// IntervalObservation contains native facts, not simulation.ExecutionCoverage.
// RawBooksValid does not establish standard deliverables or tradability.
// Minimum values are meaningful only when HasBidMinimum is true. Invalid
// nonreset books retain their native minima; absent/reset books invent none.
type IntervalObservation struct {
	HasBidMinimum      bool
	MinimumBidPx       int64
	MinimumBidSize     uint32
	RawBooksValid      bool
	ReceiveQuotesFresh bool
	EventQuotesFresh   bool
	Endpoint           Observation
}

type IntervalSnapshot struct {
	Dataset      string
	UTCStart     uint64
	From         uint64
	Through      uint64
	MaxAgeNanos  uint64
	Observations []IntervalObservation
}

type IntervalSummary struct {
	Native       nativebook.Summary
	Windows      uint64
	Observations uint64
}

type intervalState struct {
	state
	lastTouched uint64
	facts       IntervalObservation
}

// ReduceIntervals consumes the complete original source, using bounded memory
// proportional to instruments and windows. Every completed state in a window
// contributes, including equal-capture-time transient losses. Quiet periods age
// both original clocks before an update can restore endpoint freshness. Partial
// events spanning receive time invalidate continuity; same-time event parts
// publish only on completion. The closed endpoint remains exact raw state.
//
// Output is provisional until native EOF/digest, cancellation, and the outer
// acquisition/decompression/source-stability checks succeed. Callers must stage
// it and discard it on any error. Errors return a zero IntervalSummary.
func ReduceIntervals(ctx context.Context, source io.Reader, expected nativebook.Expected,
	scope Scope, windows []Window, maxAge uint64, emit func(IntervalSnapshot) error) (IntervalSummary, error) {
	if ctx == nil || source == nil || emit == nil || scope.UTCStart == 0 || scope.UTCStart%DayNanos != 0 ||
		scope.UTCStart > math.MaxUint64-DayNanos || maxAge > DayNanos ||
		len(scope.Instruments) == 0 || len(scope.Instruments) > MaxInstruments ||
		len(windows) == 0 || len(windows) > MaxCuts ||
		!((scope.Dataset == "OPRA.PILLAR" && expected.Schema == nativebook.CMBP1) ||
			(scope.Dataset == "EQUS.MINI" && expected.Schema == nativebook.MBP1)) {
		return IntervalSummary{}, ErrScope
	}
	ids := slices.Clone(scope.Instruments)
	slices.Sort(ids)
	states := make(map[uint32]*intervalState, len(ids))
	for _, id := range ids {
		if id == 0 || states[id] != nil {
			return IntervalSummary{}, ErrScope
		}
		states[id] = new(intervalState)
	}
	windows = slices.Clone(windows)
	end := scope.UTCStart + DayNanos
	for i, w := range windows {
		if w.From < scope.UTCStart || w.Through >= end || w.Through <= w.From ||
			w.Through-w.From > 1_000_000_000 || i > 0 && w.From < windows[i-1].Through {
			return IntervalSummary{}, ErrScope
		}
	}
	var result IntervalSummary
	index, active := 0, false
	start := func() {
		for _, id := range ids {
			s := states[id]
			s.facts = IntervalObservation{RawBooksValid: true, ReceiveQuotesFresh: true, EventQuotesFresh: true}
			s.fold(windows[index].From, maxAge)
		}
		active = true
	}
	finish := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		w := windows[index]
		observations := make([]IntervalObservation, 0, len(ids))
		for _, id := range ids {
			s := states[id]
			s.fold(w.Through, maxAge)
			covered := w.Through >= maxAge && w.Through-maxAge >= scope.UTCStart
			s.facts.Endpoint = Observation{InstrumentID: id, HasBook: s.hasBook, Book: s.book,
				BookAvailableAt: s.availableAt, HasTrade: s.hasTrade, Trade: s.trade,
				PendingEvent: s.pending, Status: s.status(w.Through, maxAge),
				HasBookUpdate: s.hasBookUpdate, LastBookUpdateRecv: s.lastBookUpdate,
				AgeWindowCovered:           covered,
				ZeroBookUpdatesInAgeWindow: covered && (!s.hasBookUpdate || s.lastBookUpdate < w.Through-maxAge)}
			observations = append(observations, s.facts)
		}
		if err := emit(IntervalSnapshot{Dataset: scope.Dataset, UTCStart: scope.UTCStart,
			From: w.From, Through: w.Through, MaxAgeNanos: maxAge, Observations: observations}); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		result.Windows++
		result.Observations += uint64(len(ids))
		index++
		active = false
		return nil
	}
	// Strict comparisons close a boundary only after ALL equal-time records.
	advance := func(next uint64) error {
		for index < len(windows) {
			if !active {
				if next <= windows[index].From {
					return nil
				}
				start()
			}
			if next <= windows[index].Through {
				return nil
			}
			if err := finish(); err != nil {
				return err
			}
		}
		return nil
	}
	var previous uint64
	hasPrevious := false
	native, err := nativebook.Stream(ctx, source, expected, func(r nativebook.Record) error {
		if r.TsRecv < scope.UTCStart || r.TsRecv >= end || hasPrevious && r.TsRecv < previous {
			return ErrCapture
		}
		s := states[r.InstrumentID]
		if s == nil || r.PublisherID == 0 || (r.Side != 'A' && r.Side != 'B' && r.Side != 'N') ||
			(r.Action == 'R' && r.Side != 'N') || r.Flags & ^structuralFlags != 0 {
			return ErrRecord
		}
		if expected.Schema == nativebook.CMBP1 {
			if r.Flags&(128|64) != 128|64 || r.Flags&16 != 0 {
				return ErrRecord
			}
		} else if r.PublisherID != 95 || r.Sequence != 0 || r.TsInDelta != 0 || r.BidCount != 0 || r.AskCount != 0 {
			return ErrRecord
		}
		switch r.Action {
		case 'A', 'C', 'M', 'R', 'T':
		default:
			return ErrRecord
		}
		if err := advance(r.TsRecv); err != nil {
			return err
		}
		if active && r.TsRecv > max(windows[index].From, s.lastTouched) {
			s.fold(r.TsRecv, maxAge)
		}
		s.apply(r, expected.Schema)
		if active && !s.pending {
			s.fold(r.TsRecv, maxAge)
		}
		previous, hasPrevious = r.TsRecv, true
		return nil
	})
	if err != nil {
		return IntervalSummary{}, err
	}
	if err := advance(end); err != nil {
		return IntervalSummary{}, err
	}
	if err := ctx.Err(); err != nil {
		return IntervalSummary{}, err
	}
	result.Native = native
	return result, nil
}

func (s *intervalState) fold(at, maxAge uint64) {
	if s.hasBook && s.book.Action != 'R' {
		if !s.facts.HasBidMinimum {
			s.facts.HasBidMinimum = true
			s.facts.MinimumBidPx, s.facts.MinimumBidSize = s.book.BidPx, s.book.BidSz
		} else {
			s.facts.MinimumBidPx = min(s.facts.MinimumBidPx, s.book.BidPx)
			s.facts.MinimumBidSize = min(s.facts.MinimumBidSize, s.book.BidSz)
		}
	}
	status := s.status(at, maxAge)
	valid := (status == Fresh || status == Stale) && s.book.TsEvent != 0 && s.book.TsRecv <= at
	s.facts.RawBooksValid = s.facts.RawBooksValid && valid
	s.facts.ReceiveQuotesFresh = s.facts.ReceiveQuotesFresh && valid && at-s.book.TsRecv <= maxAge
	s.facts.EventQuotesFresh = s.facts.EventQuotesFresh && valid && s.book.TsEvent <= at && at-s.book.TsEvent <= maxAge
}

func (s *intervalState) apply(r nativebook.Record, schema nativebook.Schema) {
	if !s.pending {
		s.changed = false
		s.workingAmbiguous = s.ambiguous
	}
	s.pending = true
	if r.Action == 'T' {
		s.trade, s.hasTrade = r, true
		base, known := s.book, s.hasBook
		if s.changed {
			base, known = s.working, true
		}
		if known && base.Action != 'R' && !sameLevels(base, r, schema) {
			s.workingAmbiguous = true
		}
	} else {
		s.working, s.changed, s.workingAmbiguous = r, true, false
		s.lastBookUpdate, s.hasBookUpdate = r.TsRecv, true
	}
	if r.Flags&lastFlag != 0 {
		if s.changed {
			s.book, s.hasBook, s.availableAt = s.working, true, r.TsRecv
		}
		s.ambiguous = s.workingAmbiguous
		s.pending, s.changed = false, false
	}
	s.lastTouched = r.TsRecv
}
