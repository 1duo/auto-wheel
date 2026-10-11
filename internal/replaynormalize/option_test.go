package replaynormalize

import (
	"errors"
	"math"
	"testing"
	"time"

	"autowheel/internal/causalbook"
	"autowheel/internal/nativebook"
)

func optionBookFixture() (causalbook.Snapshot, causalbook.Observation, OptionBookBinding) {
	start := uint64(time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC).UnixNano())
	cut := start + 13*3_600_000_000_000 + 35*60_000_000_000 + 17
	snapshot := causalbook.Snapshot{Dataset: "OPRA.PILLAR", UTCStart: start, Through: cut, MaxAgeNanos: 30_000_000_000}
	book := nativebook.Record{Index: 7, Version: 2, Length: 80, RType: 177,
		PublisherID: 30, InstrumentID: 123, TsRecv: cut - 7, TsEvent: cut - 9,
		Action: 'M', Side: 'N', Flags: 192, TsInDelta: -11,
		BidPx: 101 * NativeNanosPerCent, AskPx: 103 * NativeNanosPerCent,
		BidSz: 17, AskSz: 19, BidPublisher: 20, AskPublisher: 61}
	book.Raw[42] = 9 // Preserve uninterpreted original bytes too.
	observation := causalbook.Observation{InstrumentID: 123, HasBook: true, Book: book,
		BookAvailableAt: cut - 1, Status: causalbook.Fresh, HasBookUpdate: true,
		LastBookUpdateRecv: book.TsRecv, AgeWindowCovered: true}
	binding := OptionBookBinding{RawSymbol: "NFLX  260410P00100000", InstrumentID: 123, UTCStart: start, SourceSHA256: [32]byte{1}}
	return snapshot, observation, binding
}

func TestOptionBookPreservesLiteralInputs(t *testing.T) {
	snapshot, observation, binding := optionBookFixture()
	value, err := OptionBook(snapshot, observation, binding)
	if err != nil {
		t.Fatal(err)
	}
	if value.Bid != 101 || value.Ask != 103 || value.RawBidSize != 17 || value.RawAskSize != 19 ||
		value.RawBidPublisher != 20 || value.RawAskPublisher != 61 || value.Original != observation.Book ||
		value.SourceSHA256 != binding.SourceSHA256 || value.RawSymbol != binding.RawSymbol || value.InstrumentID != 123 ||
		value.Policy != RawReceiveClockV1 || uint64(value.At.UnixNano()) != observation.Book.TsRecv ||
		value.BookReceiveAt != observation.Book.TsRecv || value.BookEventAt != observation.Book.TsEvent ||
		value.BookAvailableAt != observation.BookAvailableAt || value.ObservedAt != snapshot.Through {
		t.Fatal("literal source input changed")
	}
	observation.Book.Raw[42]++
	if value.Original.Raw[42] != 9 {
		t.Fatal("output aliases caller state")
	}
	observation.Book = value.Original
	for _, publisher := range []uint16{20, 29, 31, 37, 61, 108, 109} {
		observation.Book.BidPublisher = publisher
		if _, err := OptionBook(snapshot, observation, binding); err != nil {
			t.Fatalf("known wire venue %d rejected: %v", publisher, err)
		}
	}
	observation.Book.Flags = 193 // Documented reserved bit remains literal.
	observation.Book.Length, observation.Book.HasTsOut = 88, true
	observation.Book.TsOut = math.MaxUint64 // Transport clock does not refresh age.
	observation.Book.TsRecv = snapshot.Through - snapshot.MaxAgeNanos
	observation.Book.TsEvent = observation.Book.TsRecv
	observation.LastBookUpdateRecv = observation.Book.TsRecv
	if _, err := OptionBook(snapshot, observation, binding); err != nil {
		t.Fatalf("inclusive age, reserved bit or optional output clock rejected: %v", err)
	}
}

func TestOptionBookWithholdsUnsafeInputs(t *testing.T) {
	tests := []struct {
		name string
		edit func(*causalbook.Snapshot, *causalbook.Observation, *OptionBookBinding)
		want error
	}{
		{"wrong_dataset", func(s *causalbook.Snapshot, _ *causalbook.Observation, _ *OptionBookBinding) { s.Dataset = "EQUS.MINI" }, ErrBinding},
		{"zero_day", func(s *causalbook.Snapshot, _ *causalbook.Observation, _ *OptionBookBinding) { s.UTCStart = 0 }, ErrBinding},
		{"misaligned_day", func(s *causalbook.Snapshot, _ *causalbook.Observation, _ *OptionBookBinding) { s.UTCStart++ }, ErrBinding},
		{"unrepresentable_day", func(s *causalbook.Snapshot, _ *causalbook.Observation, _ *OptionBookBinding) {
			s.UTCStart = uint64(math.MaxInt64/causalbook.DayNanos) * causalbook.DayNanos
		}, ErrBinding},
		{"bound_day_differs", func(_ *causalbook.Snapshot, _ *causalbook.Observation, b *OptionBookBinding) {
			b.UTCStart += causalbook.DayNanos
		}, ErrBinding},
		{"empty_hash", func(_ *causalbook.Snapshot, _ *causalbook.Observation, b *OptionBookBinding) {
			b.SourceSHA256 = [32]byte{}
		}, ErrBinding},
		{"malformed_symbol", func(_ *causalbook.Snapshot, _ *causalbook.Observation, b *OptionBookBinding) { b.RawSymbol = "NFLX" }, ErrBinding},
		{"zero_identity", func(_ *causalbook.Snapshot, _ *causalbook.Observation, b *OptionBookBinding) { b.InstrumentID = 0 }, ErrBinding},
		{"observation_identity", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) { o.InstrumentID++ }, ErrBinding},
		{"cut_before_day", func(s *causalbook.Snapshot, _ *causalbook.Observation, _ *OptionBookBinding) {
			s.Through = s.UTCStart - 1
		}, ErrBinding},
		{"cut_after_day", func(s *causalbook.Snapshot, _ *causalbook.Observation, _ *OptionBookBinding) {
			s.Through = s.UTCStart + causalbook.DayNanos
		}, ErrBinding},
		{"no_age", func(s *causalbook.Snapshot, _ *causalbook.Observation, _ *OptionBookBinding) { s.MaxAgeNanos = 0 }, ErrBinding},
		{"age_beyond_spec", func(s *causalbook.Snapshot, _ *causalbook.Observation, _ *OptionBookBinding) { s.MaxAgeNanos++ }, ErrBinding},
		{"age_window_before_day", func(s *causalbook.Snapshot, _ *causalbook.Observation, _ *OptionBookBinding) {
			s.Through = s.UTCStart + s.MaxAgeNanos - 1
		}, ErrBinding},
		{"absent_book", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) { o.HasBook = false }, ErrBook},
		{"no_update", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) { o.HasBookUpdate = false }, ErrBook},
		{"pending", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) { o.PendingEvent = true }, ErrBook},
		{"trade_only", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) {
			o.Status = causalbook.TradeOnly
		}, ErrBook},
		{"unknown_depth", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) {
			o.Status = causalbook.UnknownDepth
		}, ErrBook},
		{"incomplete_window", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) {
			o.AgeWindowCovered = false
		}, ErrBook},
		{"zero_updates", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) {
			o.ZeroBookUpdatesInAgeWindow = true
		}, ErrBook},
		{"record_identity", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) { o.Book.InstrumentID++ }, ErrBook},
		{"native_version", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) { o.Book.Version = 4 }, ErrBook},
		{"wrong_schema", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) { o.Book.RType = 1 }, ErrBook},
		{"wrong_length", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) { o.Book.Length = 88 }, ErrBook},
		{"foreign_event_publisher", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) { o.Book.PublisherID = 95 }, ErrBook},
		{"foreign_bid_venue", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) {
			o.Book.BidPublisher = 95
		}, ErrBook},
		{"consolidated_ask_venue", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) {
			o.Book.AskPublisher = 30
		}, ErrBook},
		{"unknown_ask_venue", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) {
			o.Book.AskPublisher = math.MaxUint16
		}, ErrBook},
		{"depth", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) { o.Book.Depth = 1 }, ErrBook},
		{"sequence", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) { o.Book.Sequence = 1 }, ErrBook},
		{"count", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) { o.Book.BidCount = 1 }, ErrBook},
		{"trade_replacement", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) { o.Book.Action = 'T' }, ErrBook},
		{"bad_side", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) { o.Book.Side = 'X' }, ErrBook},
		{"snapshot_flag", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) { o.Book.Flags |= 32 }, ErrBook},
		{"no_last_flag", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) {
			o.Book.Flags &= ^uint8(128)
		}, ErrBook},
		{"no_top_flag", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) {
			o.Book.Flags &= ^uint8(64)
		}, ErrBook},
		{"zero_bid_size", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) { o.Book.BidSz = 0 }, ErrBook},
		{"undefined_ask_size", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) {
			o.Book.AskSz = math.MaxUint32
		}, ErrBook},
		{"crossed", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) {
			o.Book.AskPx = o.Book.BidPx - NativeNanosPerCent
		}, ErrBook},
		{"update_clock_differs", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) { o.LastBookUpdateRecv++ }, ErrBook},
		{"receive_before_day", func(s *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) {
			o.Book.TsRecv = s.UTCStart - 1
			o.LastBookUpdateRecv = o.Book.TsRecv
		}, ErrClock},
		{"future_receive", func(s *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) {
			o.Book.TsRecv = s.Through + 1
			o.LastBookUpdateRecv = o.Book.TsRecv
		}, ErrClock},
		{"completion_before_receive", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) {
			o.BookAvailableAt = o.Book.TsRecv - 1
		}, ErrClock},
		{"future_completion", func(s *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) {
			o.BookAvailableAt = s.Through + 1
		}, ErrClock},
		{"zero_event", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) { o.Book.TsEvent = 0 }, ErrClock},
		{"leading_event", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) {
			o.Book.TsEvent = o.Book.TsRecv + 1
		}, ErrClock},
		{"stale_receive", func(s *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) {
			o.Book.TsRecv = s.Through - s.MaxAgeNanos - 1
			o.LastBookUpdateRecv = o.Book.TsRecv
		}, ErrClock},
		{"stale_event", func(s *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) {
			o.Book.TsEvent = s.Through - s.MaxAgeNanos - 1
		}, ErrClock},
		{"undefined_bid", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) {
			o.Book.BidPx = math.MaxInt64
			o.Book.AskPx = math.MaxInt64
		}, ErrPrice},
		{"zero_bid", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) { o.Book.BidPx = 0 }, ErrPrice},
		{"subcent_ask", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *OptionBookBinding) { o.Book.AskPx++ }, ErrPrice},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot, observation, binding := optionBookFixture()
			test.edit(&snapshot, &observation, &binding)
			value, err := OptionBook(snapshot, observation, binding)
			if !errors.Is(err, test.want) || value != (OptionBookValue{}) {
				t.Fatalf("rejection returned value or unexpected error: %v", err)
			}
		})
	}
}
