package replaynormalize

import (
	"math"
	"time"

	"autowheel/internal/causalbook"
	"autowheel/internal/nativebook"
	"autowheel/internal/wheel"
)

// OPRAConsolidatedPublisher is the DBN 0.71.0 OPRA.PILLAR.OPRA identifier.
// Recognizing its wire value does not establish historical venue eligibility.
const OPRAConsolidatedPublisher uint16 = 30

func isOPRAQuoteVenue(publisher uint16) bool {
	// These are wire identifiers in DBN 0.71.0, not a dated venue roster.
	return publisher >= 20 && publisher <= 37 && publisher != OPRAConsolidatedPublisher ||
		publisher == 61 || publisher == 108 || publisher == 109
}

type OptionBookBinding struct {
	RawSymbol    string
	InstrumentID uint32
	UTCStart     uint64
	SourceSHA256 [32]byte
}

// OptionBookValue retains raw quantity units. It is not a wheel.Quote: currency,
// contract units, tick rules, terms, roster, OI and Greeks need separate proof.
type OptionBookValue struct {
	RawSymbol       string
	InstrumentID    uint32
	SourceSHA256    [32]byte
	Policy          string
	At              time.Time
	Bid             wheel.Cents
	Ask             wheel.Cents
	RawBidSize      uint32
	RawAskSize      uint32
	RawBidPublisher uint16
	RawAskPublisher uint16
	BookReceiveAt   uint64
	BookEventAt     uint64
	BookAvailableAt uint64
	ObservedAt      uint64
	Original        nativebook.Record
}

// OptionBook converts one separately attested CLOSED CMBP observation. The
// caller must prove acquisition, full native EOF/hash, mapping and capture
// membership; a hash or symbol label alone is no such proof. Every rejection
// returns a zero value. Trades and event completion never refresh quote age.
func OptionBook(snapshot causalbook.Snapshot, observation causalbook.Observation, binding OptionBookBinding) (OptionBookValue, error) {
	if snapshot.Dataset != "OPRA.PILLAR" || snapshot.UTCStart == 0 ||
		snapshot.UTCStart%causalbook.DayNanos != 0 || snapshot.UTCStart > math.MaxInt64-causalbook.DayNanos ||
		binding.UTCStart != snapshot.UTCStart || binding.SourceSHA256 == ([32]byte{}) ||
		!validOISymbol(binding.RawSymbol) || binding.InstrumentID == 0 || observation.InstrumentID != binding.InstrumentID ||
		snapshot.Through < snapshot.UTCStart || snapshot.Through >= snapshot.UTCStart+causalbook.DayNanos ||
		snapshot.MaxAgeNanos == 0 || snapshot.MaxAgeNanos > 30_000_000_000 ||
		snapshot.Through-snapshot.UTCStart < snapshot.MaxAgeNanos {
		return OptionBookValue{}, ErrBinding
	}
	book := observation.Book
	length := uint8(80)
	if book.HasTsOut {
		length = 88
	}
	if !observation.HasBook || !observation.HasBookUpdate || observation.PendingEvent ||
		observation.Status != causalbook.Fresh || !observation.AgeWindowCovered || observation.ZeroBookUpdatesInAgeWindow ||
		book.InstrumentID != binding.InstrumentID || book.Version < 1 || book.Version > 3 ||
		book.RType != 177 || book.Length != length || book.PublisherID != OPRAConsolidatedPublisher ||
		book.Depth != 0 || book.Sequence != 0 || book.BidCount != 0 || book.AskCount != 0 ||
		(book.Action != 'A' && book.Action != 'C' && book.Action != 'M') ||
		(book.Side != 'A' && book.Side != 'B' && book.Side != 'N') ||
		book.Flags & ^uint8(128|64|1) != 0 || book.Flags&(128|64) != 128|64 ||
		book.BidSz == 0 || book.AskSz == 0 || book.BidSz == math.MaxUint32 || book.AskSz == math.MaxUint32 || book.BidPx > book.AskPx ||
		!isOPRAQuoteVenue(book.BidPublisher) || !isOPRAQuoteVenue(book.AskPublisher) ||
		observation.LastBookUpdateRecv != book.TsRecv {
		return OptionBookValue{}, ErrBook
	}
	if book.TsRecv < snapshot.UTCStart || book.TsRecv > snapshot.Through ||
		observation.BookAvailableAt < book.TsRecv || observation.BookAvailableAt > snapshot.Through ||
		book.TsEvent == 0 || book.TsEvent > book.TsRecv ||
		snapshot.Through-book.TsRecv > snapshot.MaxAgeNanos || snapshot.Through-book.TsEvent > snapshot.MaxAgeNanos {
		return OptionBookValue{}, ErrClock
	}
	bid, err := ExactCents(book.BidPx)
	if err != nil {
		return OptionBookValue{}, err
	}
	ask, err := ExactCents(book.AskPx)
	if err != nil {
		return OptionBookValue{}, err
	}
	return OptionBookValue{RawSymbol: binding.RawSymbol, InstrumentID: binding.InstrumentID, SourceSHA256: binding.SourceSHA256,
		Policy: RawReceiveClockV1, At: time.Unix(0, int64(book.TsRecv)).UTC(), Bid: bid, Ask: ask,
		RawBidSize: book.BidSz, RawAskSize: book.AskSz, RawBidPublisher: book.BidPublisher, RawAskPublisher: book.AskPublisher,
		BookReceiveAt: book.TsRecv, BookEventAt: book.TsEvent, BookAvailableAt: observation.BookAvailableAt,
		ObservedAt: snapshot.Through, Original: book}, nil
}
