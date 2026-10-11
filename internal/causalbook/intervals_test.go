package causalbook

import (
	"bytes"
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"autowheel/internal/nativebook"
)

func intervalFixture(t *testing.T, schema nativebook.Schema, w []Window, records ...nativebook.Record) ([]IntervalSnapshot, IntervalSummary) {
	t.Helper()
	dataset := "OPRA.PILLAR"
	if schema == nativebook.MBP1 {
		dataset = "EQUS.MINI"
	}
	data, expected := testStream(schema, true, records...)
	var result []IntervalSnapshot
	summary, err := ReduceIntervals(t.Context(), bytes.NewReader(data), expected,
		Scope{Dataset: dataset, UTCStart: testStart, Instruments: []uint32{1}}, w, 30*second,
		func(s IntervalSnapshot) error { result = append(result, s); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return result, summary
}

func TestIntervalsTransientAndClosedBoundaries(t *testing.T) {
	from := testStart + 60*second
	initial := testRecord(nativebook.CMBP1, from-5*second, 'A')
	initial.TsEvent = initial.TsRecv
	dip := testRecord(nativebook.CMBP1, from+second/2, 'A')
	dip.BidPx, dip.BidSz = 80, 1
	recovery := dip
	recovery.BidPx, recovery.BidSz = 105, 3
	recovery.TsOut = math.MaxUint64
	got, summary := intervalFixture(t, nativebook.CMBP1, []Window{{from, from + second}}, initial, dip, recovery)
	o := got[0].Observations[0]
	if !o.HasBidMinimum || o.MinimumBidPx != 80 || o.MinimumBidSize != 1 || !o.RawBooksValid ||
		!o.ReceiveQuotesFresh || !o.EventQuotesFresh || o.Endpoint.Book.Index != 2 ||
		o.Endpoint.Book.TsOut != math.MaxUint64 || summary.Windows != 1 || summary.Observations != 1 {
		t.Fatal("equal-time transient loss or original fields were discarded")
	}
	// All From updates are closed before the carried state is initialized.
	dip.TsRecv, recovery.TsRecv = from, from
	dip.TsEvent, recovery.TsEvent = from, from
	got, _ = intervalFixture(t, nativebook.CMBP1, []Window{{from, from + second}}, initial, dip, recovery)
	if got[0].Observations[0].MinimumBidPx != recovery.BidPx {
		t.Fatal("pre-interval From dip was counted after its closed recovery")
	}
	// ALL Through updates belong to the interval, even if the last recovers.
	dip.TsRecv, recovery.TsRecv = from+second, from+second
	dip.TsEvent, recovery.TsEvent = from+second, from+second
	got, _ = intervalFixture(t, nativebook.CMBP1, []Window{{from, from + second}}, initial, dip, recovery)
	if got[0].Observations[0].MinimumBidPx != 80 {
		t.Fatal("closed Through dip was omitted")
	}
}

func TestIntervalsQuietAgingAndSeparateClocks(t *testing.T) {
	from := testStart + 60*second
	old := testRecord(nativebook.CMBP1, from-30*second, 'A')
	old.TsEvent = old.TsRecv
	restore := testRecord(nativebook.CMBP1, from+second/2, 'A')
	restore.TsEvent = restore.TsRecv
	got, _ := intervalFixture(t, nativebook.CMBP1, []Window{{from, from + second}}, old, restore)
	o := got[0].Observations[0]
	if !o.RawBooksValid || o.ReceiveQuotesFresh || o.EventQuotesFresh || o.Endpoint.Status != Fresh {
		t.Fatal("endpoint recovery concealed a stale quiet period")
	}
	old.TsRecv, old.TsEvent = from-1, from-31*second
	got, _ = intervalFixture(t, nativebook.CMBP1, []Window{{from, from + second}}, old)
	o = got[0].Observations[0]
	if !o.RawBooksValid || !o.ReceiveQuotesFresh || o.EventQuotesFresh || o.Endpoint.Book.TsEvent != old.TsEvent {
		t.Fatal("event freshness was replaced by receive freshness")
	}
	for _, event := range []uint64{0, math.MaxUint64, old.TsRecv + 1} {
		old.TsEvent = event
		got, _ = intervalFixture(t, nativebook.CMBP1, []Window{{from, from + second}}, old)
		o = got[0].Observations[0]
		if o.RawBooksValid || o.EventQuotesFresh || o.Endpoint.Book.TsEvent != event {
			t.Fatal("unusable event clock became a fresh quote")
		}
	}
}

func TestIntervalsTradeResetAndPartialEvents(t *testing.T) {
	from := testStart + 60*second
	initial := testRecord(nativebook.MBP1, from-5*second, 'A')
	initial.TsEvent = initial.TsRecv
	trade := testRecord(nativebook.MBP1, from+second/2, 'T')
	trade.BidPx = 95
	got, _ := intervalFixture(t, nativebook.MBP1, []Window{{from, from + second}}, initial, trade)
	o := got[0].Observations[0]
	if o.RawBooksValid || o.Endpoint.Status != Ambiguous || o.Endpoint.Book.TsRecv != initial.TsRecv ||
		o.MinimumBidPx != initial.BidPx || o.Endpoint.Trade.Index != 1 {
		t.Fatal("trade refreshed/replaced the actual book")
	}
	reset := testRecord(nativebook.MBP1, from+second/2, 'R')
	reset.BidPx, reset.BidSz = 0, 0
	got, _ = intervalFixture(t, nativebook.MBP1, []Window{{from, from + second}}, reset)
	if got[0].Observations[0].HasBidMinimum || got[0].Observations[0].RawBooksValid {
		t.Fatal("missing/reset book invented a zero quote")
	}
	partial := testRecord(nativebook.MBP1, from+second/2, 'A')
	partial.Flags, partial.BidPx = 0, 80
	last := partial
	last.Action, last.Flags = 'T', 128
	got, _ = intervalFixture(t, nativebook.MBP1, []Window{{from, from + second}}, initial, partial, last)
	o = got[0].Observations[0]
	if !o.RawBooksValid || o.MinimumBidPx != 80 || o.Endpoint.BookAvailableAt != last.TsRecv ||
		o.Endpoint.Book.TsRecv != partial.TsRecv {
		t.Fatal("same-capture partial completion did not publish the event")
	}
	last.TsRecv++
	got, _ = intervalFixture(t, nativebook.MBP1, []Window{{from, from + second}}, initial, partial, last)
	if got[0].Observations[0].RawBooksValid || got[0].Observations[0].Endpoint.PendingEvent {
		t.Fatal("positive-duration incomplete event disappeared")
	}
	got, _ = intervalFixture(t, nativebook.MBP1, []Window{{from, from + second}}, initial, partial)
	if got[0].Observations[0].RawBooksValid || !got[0].Observations[0].Endpoint.PendingEvent {
		t.Fatal("pending EOF event became complete")
	}
}

func TestIntervalsRejectScopeAndSourceFailures(t *testing.T) {
	from := testStart + 60*second
	scope := Scope{Dataset: "OPRA.PILLAR", UTCStart: testStart, Instruments: []uint32{1}}
	book := testRecord(nativebook.CMBP1, from-5*second, 'A')
	data, expected := testStream(nativebook.CMBP1, false, book)
	for _, windows := range [][]Window{
		nil, {{from, from}}, {{from, from + second + 1}}, {{from, from + second}, {from, from + second}},
		{{testStart - 1, testStart}}, {{testStart + DayNanos - second, testStart + DayNanos}},
	} {
		s, err := ReduceIntervals(t.Context(), bytes.NewReader(data), expected, scope, windows, 30*second,
			func(IntervalSnapshot) error { t.Fatal("invalid scope emitted"); return nil })
		if !errors.Is(err, ErrScope) || !reflect.DeepEqual(s, IntervalSummary{}) {
			t.Fatal("invalid interval scope accepted")
		}
	}
	for _, kind := range []string{"digest", "trailing", "identity", "flags", "order", "callback", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			book := book
			if kind == "identity" {
				book.InstrumentID = 2
			}
			if kind == "flags" {
				book.Flags |= 4
			}
			records := []nativebook.Record{book}
			if kind == "order" {
				prior := book
				prior.TsRecv--
				records = append(records, prior)
			}
			data, expected := testStream(nativebook.CMBP1, false, records...)
			if kind == "digest" {
				expected.NativeSHA256[0] ^= 1
			}
			if kind == "trailing" {
				data = append(data, 1)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s, err := ReduceIntervals(ctx, bytes.NewReader(data), expected, scope, []Window{{from, from + second}}, 30*second,
				func(IntervalSnapshot) error {
					if kind == "callback" {
						return errors.New("synthetic callback failure")
					}
					if kind == "cancel" {
						cancel()
					}
					return nil
				})
			if err == nil || !reflect.DeepEqual(s, IntervalSummary{}) {
				t.Fatal("failed provisional stream returned an accepted summary")
			}
		})
	}
}

func TestIntervalsCallbackCannotMutateStateOrAccounting(t *testing.T) {
	from := testStart + 60*second
	book := testRecord(nativebook.CMBP1, from-5*second, 'A')
	data, expected := testStream(nativebook.CMBP1, false, book)
	scope := Scope{Dataset: "OPRA.PILLAR", UTCStart: testStart, Instruments: []uint32{2, 1}}
	windows := []Window{{from, from + second}, {from + second, from + 2*second}}
	seen := 0
	s, err := ReduceIntervals(t.Context(), bytes.NewReader(data), expected, scope, windows, 30*second,
		func(s IntervalSnapshot) error {
			if s.Observations[0].Endpoint.InstrumentID != 1 || s.Observations[0].Endpoint.Book.BidPx != 100 {
				t.Fatal("prior callback changed reducer state")
			}
			s.Observations[0].Endpoint.Book.BidPx = 999
			s.Observations[0].MinimumBidPx = 999
			clear(s.Observations)
			seen++
			return nil
		})
	if err != nil || seen != 2 || s.Windows != 2 || s.Observations != 4 || scope.Instruments[0] != 2 ||
		windows[0].From != from {
		t.Fatalf("mutation or accounting: %+v %v", s, err)
	}
}
