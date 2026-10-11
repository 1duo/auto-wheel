// Package replaynormalize converts audited raw observations into candidate
// replay values. It does not qualify sources or authorize trading or replay.
package replaynormalize

import (
	"errors"
	"math"
	"time"

	"autowheel/internal/causalbook"
	"autowheel/internal/simulation"
	"autowheel/internal/wheel"
)

const (
	NativeNanosPerCent int64 = 10_000_000
	// RawReceiveClockV1 uses the original BOOK receive timestamp for quote
	// age. Completion time only establishes availability; it never refreshes
	// the quote. Event time must independently satisfy the same age bound.
	RawReceiveClockV1 = "raw_book_receive_with_event_age_v1"
)

var (
	ErrBinding = errors.New("replaynormalize: invalid source identity or scope")
	ErrBook    = errors.New("replaynormalize: raw book unavailable or invalid")
	ErrClock   = errors.New("replaynormalize: unknown, future, stale or unrepresentable clock")
	ErrPrice   = errors.New("replaynormalize: invalid or subcent price")
)

// StockBinding is declared by the acquired-source adapter, never inferred from
// a price record. Its hash must be independently bound to the complete native
// source, mapping and closed-capture evidence before any replay can use it.
type StockBinding struct {
	Symbol       string
	InstrumentID uint32
	UTCStart     uint64
	SourceSHA256 [32]byte
}

// StockValue retains exact original clocks alongside candidate integer prices.
// Stock.At is BookReceiveAt. No clock is rounded, sorted or replaced by a cut.
type StockValue struct {
	Stock           simulation.Stock
	InstrumentID    uint32
	SourceSHA256    [32]byte
	Policy          string
	BookReceiveAt   uint64
	BookEventAt     uint64
	BookAvailableAt uint64
	ObservedAt      uint64
}

// ExactCents rejects native sentinels and subcent values; it never rounds a
// market quote toward a favorable fill or invents a price for missing data.
func ExactCents(value int64) (wheel.Cents, error) {
	if value <= 0 || value == math.MaxInt64 || value%NativeNanosPerCent != 0 {
		return 0, ErrPrice
	}
	return wheel.Cents(value / NativeNanosPerCent), nil
}

// Stock converts one independently attested closed MBP observation. A rejection
// returns a zero StockValue. Missing values remain blockers; callers may retain
// a rejection but must not substitute a trade, previous value or rounded price.
func Stock(snapshot causalbook.Snapshot, observation causalbook.Observation, binding StockBinding) (StockValue, error) {
	if snapshot.Dataset != "EQUS.MINI" || snapshot.UTCStart == 0 ||
		snapshot.UTCStart%causalbook.DayNanos != 0 || snapshot.UTCStart > math.MaxInt64-causalbook.DayNanos ||
		binding.UTCStart != snapshot.UTCStart || binding.SourceSHA256 == ([32]byte{}) ||
		binding.InstrumentID == 0 || observation.InstrumentID != binding.InstrumentID ||
		len(binding.Symbol) == 0 || len(binding.Symbol) > 16 ||
		snapshot.Through < snapshot.UTCStart || snapshot.Through >= snapshot.UTCStart+causalbook.DayNanos ||
		snapshot.MaxAgeNanos == 0 || snapshot.MaxAgeNanos > 30_000_000_000 ||
		snapshot.Through-snapshot.UTCStart < snapshot.MaxAgeNanos {
		return StockValue{}, ErrBinding
	}
	for _, char := range binding.Symbol {
		if char < 'A' || char > 'Z' {
			return StockValue{}, ErrBinding
		}
	}
	book := observation.Book
	if !observation.HasBook || !observation.HasBookUpdate || observation.PendingEvent ||
		observation.Status != causalbook.Fresh || !observation.AgeWindowCovered ||
		observation.ZeroBookUpdatesInAgeWindow || book.InstrumentID != binding.InstrumentID ||
		book.Version < 1 || book.Version > 3 || book.RType != 1 || book.PublisherID != 95 ||
		book.Depth != 0 || book.Sequence != 0 || book.TsInDelta != 0 || book.BidCount != 0 || book.AskCount != 0 ||
		(book.Action != 'A' && book.Action != 'C' && book.Action != 'M') ||
		(book.Side != 'A' && book.Side != 'B' && book.Side != 'N') || book.Flags & ^uint8(128|64|16|1) != 0 ||
		book.BidSz == 0 || book.AskSz == 0 || book.BidPx > book.AskPx ||
		observation.LastBookUpdateRecv != book.TsRecv {
		return StockValue{}, ErrBook
	}
	if book.TsRecv < snapshot.UTCStart || book.TsRecv > snapshot.Through ||
		observation.BookAvailableAt < book.TsRecv || observation.BookAvailableAt > snapshot.Through ||
		book.TsEvent == 0 || book.TsEvent > book.TsRecv ||
		snapshot.Through-book.TsRecv > snapshot.MaxAgeNanos || snapshot.Through-book.TsEvent > snapshot.MaxAgeNanos {
		return StockValue{}, ErrClock
	}
	bid, err := ExactCents(book.BidPx)
	if err != nil {
		return StockValue{}, err
	}
	ask, err := ExactCents(book.AskPx)
	if err != nil {
		return StockValue{}, err
	}
	return StockValue{Stock: simulation.Stock{Symbol: binding.Symbol, At: time.Unix(0, int64(book.TsRecv)).UTC(), Bid: bid, Ask: ask},
		InstrumentID: binding.InstrumentID, SourceSHA256: binding.SourceSHA256, Policy: RawReceiveClockV1,
		BookReceiveAt: book.TsRecv, BookEventAt: book.TsEvent, BookAvailableAt: observation.BookAvailableAt,
		ObservedAt: snapshot.Through}, nil
}
