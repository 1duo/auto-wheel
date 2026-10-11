package replaynormalize

import (
	"errors"
	"math"
	"testing"

	"autowheel/internal/causalbook"
	"autowheel/internal/nativebook"
)

func stockFixture() (causalbook.Snapshot, causalbook.Observation, StockBinding) {
	const start = uint64(1_775_433_600_000_000_000)
	cut := start + 60_000_000_000
	book := nativebook.Record{Version: 1, RType: 1, PublisherID: 95, InstrumentID: 1,
		TsRecv: cut - 5_000_000_001, TsEvent: cut - 5_000_000_099,
		Action: 'A', Side: 'N', Flags: 128, BidPx: 100 * NativeNanosPerCent, AskPx: 101 * NativeNanosPerCent, BidSz: 2, AskSz: 3}
	snapshot := causalbook.Snapshot{Dataset: "EQUS.MINI", UTCStart: start, Through: cut, MaxAgeNanos: 30_000_000_000}
	observation := causalbook.Observation{InstrumentID: 1, HasBook: true, Book: book, BookAvailableAt: book.TsRecv + 10,
		HasBookUpdate: true, LastBookUpdateRecv: book.TsRecv, AgeWindowCovered: true, Status: causalbook.Fresh}
	return snapshot, observation, StockBinding{Symbol: "SYNTHETIC", InstrumentID: 1, UTCStart: start, SourceSHA256: [32]byte{1}}
}

func TestStockExactPriceAndClock(t *testing.T) {
	snapshot, observation, binding := stockFixture()
	value, err := Stock(snapshot, observation, binding)
	if err != nil || value.Stock.Bid != 100 || value.Stock.Ask != 101 ||
		uint64(value.Stock.At.UnixNano()) != observation.Book.TsRecv || value.Stock.At.Location().String() != "UTC" ||
		value.BookAvailableAt != observation.BookAvailableAt || value.BookEventAt != observation.Book.TsEvent ||
		value.Policy != RawReceiveClockV1 || value.SourceSHA256 != binding.SourceSHA256 {
		t.Fatal("exact price, original clocks or binding lost")
	}
	// At the inclusive age boundary both clocks are admissible. Completion at
	// the cut cannot refresh an older quote, even with a declared Fresh status.
	observation.Book.TsRecv = snapshot.Through - snapshot.MaxAgeNanos
	observation.Book.TsEvent = observation.Book.TsRecv
	observation.LastBookUpdateRecv = observation.Book.TsRecv
	observation.BookAvailableAt = snapshot.Through
	if _, err := Stock(snapshot, observation, binding); err != nil {
		t.Fatal(err)
	}
	observation.Book.TsEvent--
	if value, err := Stock(snapshot, observation, binding); !errors.Is(err, ErrClock) || value != (StockValue{}) {
		t.Fatal("old event clock admitted or partial value escaped")
	}
}

func TestStockRejections(t *testing.T) {
	cases := []struct {
		name string
		edit func(*causalbook.Snapshot, *causalbook.Observation, *StockBinding)
		err  error
	}{
		{"source_hash", func(_ *causalbook.Snapshot, _ *causalbook.Observation, b *StockBinding) { b.SourceSHA256 = [32]byte{} }, ErrBinding},
		{"wrong_partition", func(_ *causalbook.Snapshot, _ *causalbook.Observation, b *StockBinding) { b.UTCStart++ }, ErrBinding},
		{"symbol", func(_ *causalbook.Snapshot, _ *causalbook.Observation, b *StockBinding) { b.Symbol = "NVDA.opt" }, ErrBinding},
		{"sentinel_cut", func(s *causalbook.Snapshot, _ *causalbook.Observation, _ *StockBinding) { s.Through = math.MaxUint64 }, ErrBinding},
		{"unknown_depth", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *StockBinding) { o.Book.Depth = 1 }, ErrBook},
		{"pending", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *StockBinding) { o.PendingEvent = true }, ErrBook},
		{"trade_substitution", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *StockBinding) { o.Book.Action = 'T' }, ErrBook},
		{"mismatched_id", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *StockBinding) { o.Book.InstrumentID++ }, ErrBook},
		{"future_completion", func(s *causalbook.Snapshot, o *causalbook.Observation, _ *StockBinding) {
			o.BookAvailableAt = s.Through + 1
		}, ErrClock},
		{"early_completion", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *StockBinding) {
			o.BookAvailableAt = o.Book.TsRecv - 1
		}, ErrClock},
		{"event_sentinel", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *StockBinding) {
			o.Book.TsEvent = math.MaxUint64
		}, ErrClock},
		{"event_zero", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *StockBinding) { o.Book.TsEvent = 0 }, ErrClock},
		{"event_lead", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *StockBinding) {
			o.Book.TsEvent = o.Book.TsRecv + 1
		}, ErrClock},
		{"crossed", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *StockBinding) {
			o.Book.AskPx = o.Book.BidPx - 1
		}, ErrBook},
		{"subcent_bid", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *StockBinding) { o.Book.BidPx++ }, ErrPrice},
		{"sentinel_price", func(_ *causalbook.Snapshot, o *causalbook.Observation, _ *StockBinding) { o.Book.AskPx = math.MaxInt64 }, ErrPrice},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, observation, binding := stockFixture()
			tc.edit(&snapshot, &observation, &binding)
			if value, err := Stock(snapshot, observation, binding); !errors.Is(err, tc.err) || value != (StockValue{}) {
				t.Fatal("invalid source admitted or partial value escaped", err)
			}
		})
	}
}

func TestStockNoCentRounding(t *testing.T) {
	for _, price := range []int64{0, -1, math.MinInt64, math.MaxInt64, NativeNanosPerCent - 1, NativeNanosPerCent + 1} {
		if cents, err := ExactCents(price); !errors.Is(err, ErrPrice) || cents != 0 {
			t.Fatal("invalid or inexact price admitted")
		}
	}
	large := int64(math.MaxInt64 / NativeNanosPerCent * NativeNanosPerCent)
	if cents, err := ExactCents(large); err != nil || int64(cents) != large/NativeNanosPerCent {
		t.Fatal("exact large value lost or overflowed")
	}
}
