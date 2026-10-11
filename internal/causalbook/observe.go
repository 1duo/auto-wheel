// Package causalbook reduces complete native sources into raw capture-time
// observations. It supplies no contract terms, Greeks, replay frames, or fills.
package causalbook

import (
	"context"
	"errors"
	"io"
	"math"
	"slices"

	"autowheel/internal/nativebook"
)

const (
	DayNanos       uint64 = 86_400_000_000_000
	MaxInstruments        = 2000
	MaxCuts               = 86400
	lastFlag       uint8  = 128
	// Provider bit 0 is documented as reserved/internal and safe to ignore.
	// Preserve it in the raw record; reject snapshot/clock/gap/publisher bits.
	structuralFlags uint8 = 128 | 64 | 16 | 1
)

var (
	ErrScope   = errors.New("causalbook: invalid attested scope")
	ErrRecord  = errors.New("causalbook: unsupported record semantics")
	ErrCapture = errors.New("causalbook: invalid capture order or window")
)

// Scope must be attested from the acquired query and pinned native metadata.
// Instruments identifies records only: it is not an intraday listed roster.
// One Reduce call accepts one dataset and one complete UTC partition.
type Scope struct {
	Dataset     string
	UTCStart    uint64
	Instruments []uint32
}

type Status string

const (
	Unseen       Status = "unseen"
	TradeOnly    Status = "trade_only"
	Pending      Status = "incomplete_event"
	Reset        Status = "reset"
	Undefined    Status = "undefined_price"
	Nonpositive  Status = "nonpositive_price"
	Empty        Status = "zero_size"
	Crossed      Status = "crossed"
	ClockUnknown Status = "event_clock_undefined"
	ClockLead    Status = "event_clock_lead"
	Ambiguous    Status = "trade_book_ambiguity"
	UnknownDepth Status = "book_depth_unqualified"
	Fresh        Status = "fresh_raw_book"
	Stale        Status = "stale_raw_book"
)

// Observation preserves the last actual BOOK and TRADE records separately,
// including their original clocks, sentinels, venue fields, and reserved bytes.
// BookAvailableAt is the receive time of the event's final record; it does not
// replace Book.TsRecv or refresh quote age. Invalid books retain their raw data.
type Observation struct {
	InstrumentID               uint32
	HasBook                    bool
	Book                       nativebook.Record
	BookAvailableAt            uint64
	HasTrade                   bool
	Trade                      nativebook.Record
	PendingEvent               bool
	Status                     Status
	HasBookUpdate              bool
	LastBookUpdateRecv         uint64
	AgeWindowCovered           bool
	ZeroBookUpdatesInAgeWindow bool
}

// Snapshot is a raw, unqualified declaration at a CLOSED receive-time cut.
// Its ordered observations include unseen mapped IDs without inventing quotes.
// It must never be used as NoRecentBook without separate roster/coverage proof.
type Snapshot struct {
	Dataset      string
	UTCStart     uint64
	Through      uint64
	MaxAgeNanos  uint64
	Observations []Observation
}

type Summary struct {
	Native                nativebook.Summary
	Snapshots             uint64
	BookRecords           uint64
	TradeRecords          uint64
	CompletedEvents       uint64
	PendingEventsAtEOF    uint64
	EqualCaptureRecords   uint64
	EventClockRegressions uint64
	ClockLeads            uint64
	UndefinedEventClocks  uint64
}

type state struct {
	book, trade                 nativebook.Record
	hasBook, hasTrade           bool
	availableAt                 uint64
	pending, changed, ambiguous bool
	working                     nativebook.Record
	workingAmbiguous            bool
	hasBookUpdate               bool
	lastBookUpdate              uint64
	hasEventClock               bool
	lastEventClock              uint64
}

// Reduce uses memory proportional to the bounded instrument universe and cuts,
// never to the source record count. The source's global receive order is checked;
// records are never sorted or retimed. Equal cuts include ALL equal-time records.
// Every emitted snapshot is provisional until Reduce AND the outer acquisition,
// decompression and source-stability checks succeed. Callers must stage output
// and discard it on ANY failure. Reduce returns a zero Summary on failure.
// Pending events at EOF remain explicit; they are not silently completed.
func Reduce(ctx context.Context, source io.Reader, expected nativebook.Expected, scope Scope,
	cuts []uint64, maxAge uint64, emit func(Snapshot) error) (Summary, error) {
	if ctx == nil || source == nil || emit == nil || scope.UTCStart == 0 || scope.UTCStart%DayNanos != 0 ||
		scope.UTCStart > math.MaxUint64-DayNanos || maxAge > DayNanos ||
		len(scope.Instruments) == 0 || len(scope.Instruments) > MaxInstruments ||
		len(cuts) == 0 || len(cuts) > MaxCuts ||
		!((scope.Dataset == "OPRA.PILLAR" && expected.Schema == nativebook.CMBP1) ||
			(scope.Dataset == "EQUS.MINI" && expected.Schema == nativebook.MBP1)) {
		return Summary{}, ErrScope
	}
	ids := slices.Clone(scope.Instruments)
	slices.Sort(ids)
	states := make(map[uint32]*state, len(ids))
	for _, id := range ids {
		if id == 0 || states[id] != nil {
			return Summary{}, ErrScope
		}
		states[id] = new(state)
	}
	cuts = slices.Clone(cuts)
	end := scope.UTCStart + DayNanos
	for i, cut := range cuts {
		if cut < scope.UTCStart || cut >= end || i > 0 && cut <= cuts[i-1] {
			return Summary{}, ErrScope
		}
	}
	var result Summary
	cutIndex := 0
	emitCut := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		cut := cuts[cutIndex]
		observations := make([]Observation, 0, len(ids))
		for _, id := range ids {
			s := states[id]
			covered := cut >= maxAge && cut-maxAge >= scope.UTCStart
			zeroUpdates := covered && (!s.hasBookUpdate || s.lastBookUpdate < cut-maxAge)
			observations = append(observations, Observation{
				InstrumentID: id, HasBook: s.hasBook, Book: s.book, BookAvailableAt: s.availableAt,
				HasTrade: s.hasTrade, Trade: s.trade, PendingEvent: s.pending,
				Status: s.status(cut, maxAge), HasBookUpdate: s.hasBookUpdate,
				LastBookUpdateRecv: s.lastBookUpdate, AgeWindowCovered: covered,
				ZeroBookUpdatesInAgeWindow: zeroUpdates,
			})
		}
		if err := emit(Snapshot{Dataset: scope.Dataset, UTCStart: scope.UTCStart,
			Through: cut, MaxAgeNanos: maxAge, Observations: observations}); err != nil {
			return err
		}
		result.Snapshots++
		cutIndex++
		return nil
	}
	var previous uint64
	hasPrevious := false
	native, err := nativebook.Stream(ctx, source, expected, func(record nativebook.Record) error {
		if record.TsRecv < scope.UTCStart || record.TsRecv >= end ||
			(hasPrevious && record.TsRecv < previous) {
			return ErrCapture
		}
		s := states[record.InstrumentID]
		if s == nil || record.PublisherID == 0 ||
			(record.Side != 'A' && record.Side != 'B' && record.Side != 'N') ||
			(record.Action == 'R' && record.Side != 'N') || record.Flags & ^structuralFlags != 0 {
			return ErrRecord
		}
		if expected.Schema == nativebook.CMBP1 {
			if record.Flags&(128|64) != 128|64 || record.Flags&16 != 0 {
				return ErrRecord
			}
		} else if record.PublisherID != 95 || record.Sequence != 0 ||
			record.TsInDelta != 0 || record.BidCount != 0 || record.AskCount != 0 {
			return ErrRecord
		}
		switch record.Action {
		case 'A', 'C', 'M', 'R', 'T':
		default:
			return ErrRecord
		}
		// A strictly later capture proves all equal-time records were consumed.
		for cutIndex < len(cuts) && record.TsRecv > cuts[cutIndex] {
			if err := emitCut(); err != nil {
				return err
			}
		}
		if hasPrevious && record.TsRecv == previous {
			result.EqualCaptureRecords++
		}
		previous, hasPrevious = record.TsRecv, true
		if record.TsEvent == math.MaxUint64 {
			result.UndefinedEventClocks++
		} else {
			if s.hasEventClock && record.TsEvent < s.lastEventClock {
				result.EventClockRegressions++
			}
			if record.TsEvent > record.TsRecv {
				result.ClockLeads++
			}
			s.lastEventClock, s.hasEventClock = record.TsEvent, true
		}
		if !s.pending {
			s.changed = false
			s.workingAmbiguous = s.ambiguous
		}
		s.pending = true
		if record.Action == 'T' {
			result.TradeRecords++
			s.trade, s.hasTrade = record, true
			base, known := s.book, s.hasBook
			if s.changed {
				base, known = s.working, true
			}
			// A changed BBO carried by a trade is not an actual BOOK update.
			// Preserve the old record and flag uncertainty until a BOOK update.
			if known && base.Action != 'R' && !sameLevels(base, record, expected.Schema) {
				s.workingAmbiguous = true
			}
		} else {
			result.BookRecords++
			s.working, s.changed, s.workingAmbiguous = record, true, false
			s.lastBookUpdate, s.hasBookUpdate = record.TsRecv, true
		}
		if record.Flags&lastFlag != 0 {
			result.CompletedEvents++
			if s.changed {
				s.book, s.hasBook, s.availableAt = s.working, true, record.TsRecv
			}
			s.ambiguous = s.workingAmbiguous
			s.pending, s.changed = false, false
		}
		return nil
	})
	if err != nil {
		return Summary{}, err
	}
	for cutIndex < len(cuts) {
		if err := ctx.Err(); err != nil {
			return Summary{}, err
		}
		if err := emitCut(); err != nil {
			return Summary{}, err
		}
	}
	for _, s := range states {
		if s.pending {
			result.PendingEventsAtEOF++
		}
	}
	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}
	result.Native = native
	return result, nil
}

func sameLevels(a, b nativebook.Record, schema nativebook.Schema) bool {
	if a.BidPx != b.BidPx || a.AskPx != b.AskPx || a.BidSz != b.BidSz || a.AskSz != b.AskSz {
		return false
	}
	if schema == nativebook.CMBP1 {
		return a.BidPublisher == b.BidPublisher && a.AskPublisher == b.AskPublisher
	}
	return a.BidCount == b.BidCount && a.AskCount == b.AskCount
}

func (s *state) status(cut, maxAge uint64) Status {
	if s.pending {
		return Pending
	}
	if !s.hasBook {
		if s.hasTrade {
			return TradeOnly
		}
		return Unseen
	}
	b := s.book
	if b.Action == 'R' {
		return Reset
	}
	// Depth describes the event's originating level, not the supplied BBO's
	// array index. Preserve nonzero native values, but withhold book usability
	// until their dataset semantics are independently qualified. Resets and
	// trades still retain their explicit non-quote meanings.
	if b.Depth != 0 {
		return UnknownDepth
	}
	if s.ambiguous {
		return Ambiguous
	}
	if b.TsEvent == math.MaxUint64 {
		return ClockUnknown
	}
	if b.TsEvent > b.TsRecv {
		return ClockLead
	}
	if b.BidPx == math.MaxInt64 || b.AskPx == math.MaxInt64 {
		return Undefined
	}
	if b.BidPx <= 0 || b.AskPx <= 0 {
		return Nonpositive
	}
	if b.BidSz == 0 || b.AskSz == 0 {
		return Empty
	}
	if b.BidPx > b.AskPx {
		return Crossed
	}
	if cut-b.TsRecv > maxAge {
		return Stale
	}
	return Fresh
}
