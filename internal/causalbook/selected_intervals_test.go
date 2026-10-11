package causalbook

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"reflect"
	"testing"

	"autowheel/internal/nativebook"
)

func TestSelectedIntervalsSwitchPreservesEarlierUpdatesAndQuietAge(t *testing.T) {
	from := testStart + 60*second
	first := testRecord(nativebook.CMBP1, from-5*second, 'A')
	secondBook := testRecord(nativebook.CMBP1, from-2*second, 'A')
	secondBook.InstrumentID = 2
	update := secondBook
	update.TsRecv, update.TsEvent = from+second/2, from+second/2
	update.BidPx, update.BidSz, update.TsOut = 77, 4, math.MaxUint64
	dip := first
	dip.TsRecv, dip.TsEvent = from+3*second/2, from+3*second/2
	dip.BidPx, dip.BidSz = 80, 1
	recovery := dip
	recovery.BidPx, recovery.BidSz = 102, 3
	data, expected := testStream(nativebook.CMBP1, true, first, secondBook, update, dip, recovery)
	plan := []SelectedWindow{
		{Window{from, from + second}, []uint32{1}},
		{Window{from + second, from + 2*second}, []uint32{2}},
		{Window{from + 31*second, from + 32*second}, []uint32{1}},
	}
	var got []IntervalSnapshot
	summary, err := ReduceSelectedIntervals(t.Context(), bytes.NewReader(data), expected,
		Scope{"OPRA.PILLAR", testStart, []uint32{2, 1}}, plan, 30*second,
		func(snapshot IntervalSnapshot) error { got = append(got, snapshot); return nil })
	if err != nil || summary.Native.Records != 5 || summary.Windows != 3 || summary.Observations != 3 || len(got) != 3 {
		t.Fatalf("selected reduction: %+v %v", summary, err)
	}
	for i, id := range []uint32{1, 2, 1} {
		if len(got[i].Observations) != 1 || got[i].Observations[0].Endpoint.InstrumentID != id {
			t.Fatal("wrong instrument projected")
		}
	}
	carried := got[1].Observations[0]
	if carried.MinimumBidPx != 77 || carried.MinimumBidSize != 4 || carried.Endpoint.Book.Index != 2 ||
		carried.Endpoint.Book.TsOut != math.MaxUint64 || carried.Endpoint.Book.TsRecv != update.TsRecv ||
		!carried.RawBooksValid || !carried.ReceiveQuotesFresh || !carried.EventQuotesFresh {
		t.Fatal("updates before an instrument became selected were lost or retimed")
	}
	aged := got[2].Observations[0]
	if aged.MinimumBidPx != 102 || aged.MinimumBidSize != 3 || !aged.RawBooksValid ||
		aged.ReceiveQuotesFresh || aged.EventQuotesFresh || aged.Endpoint.Status != Stale ||
		aged.Endpoint.Book.TsRecv != recovery.TsRecv || aged.Endpoint.Book.TsEvent != recovery.TsEvent {
		t.Fatal("selection gap refreshed a quiet quote or hid aging")
	}
}

func TestSelectedIntervalsPreserveEqualTimeLossAndCanonicalIDs(t *testing.T) {
	from := testStart + 60*second
	first := testRecord(nativebook.CMBP1, from-5*second, 'A')
	other := first
	other.InstrumentID, other.TsRecv, other.TsEvent = 2, from-4*second, from-4*second
	dip := first
	dip.TsRecv, dip.TsEvent = from+second, from+second
	dip.BidPx, dip.BidSz = 80, 0
	recovery := dip
	recovery.BidPx, recovery.BidSz = 105, 3
	data, expected := testStream(nativebook.CMBP1, false, first, other, dip, recovery)
	var got IntervalSnapshot
	_, err := ReduceSelectedIntervals(t.Context(), bytes.NewReader(data), expected,
		Scope{"OPRA.PILLAR", testStart, []uint32{2, 1}},
		[]SelectedWindow{{Window{from, from + second}, []uint32{2, 1}}}, 30*second,
		func(snapshot IntervalSnapshot) error { got = snapshot; return nil })
	if err != nil || len(got.Observations) != 2 || got.Observations[0].Endpoint.InstrumentID != 1 ||
		got.Observations[1].Endpoint.InstrumentID != 2 {
		t.Fatalf("projection order: %+v %v", got, err)
	}
	loss := got.Observations[0]
	if !loss.HasBidMinimum || loss.MinimumBidPx != 80 || loss.MinimumBidSize != 0 || loss.RawBooksValid ||
		loss.Endpoint.Status != Fresh || loss.Endpoint.Book.BidPx != 105 {
		t.Fatal("closed endpoint recovery concealed a transient bid/size loss")
	}
}

type selectedNoRead struct{ calls int }

func (source *selectedNoRead) Read([]byte) (int, error) {
	source.calls++
	return 0, io.EOF
}

func TestSelectedIntervalsRejectInvalidMasksBeforeReading(t *testing.T) {
	from := testStart + 60*second
	_, expected := testStream(nativebook.CMBP1, false)
	scope := Scope{"OPRA.PILLAR", testStart, []uint32{1, 2}}
	window := Window{from, from + second}
	for _, plan := range [][]SelectedWindow{
		nil, {{window, nil}}, {{window, []uint32{0}}}, {{window, []uint32{3}}},
		{{window, []uint32{1, 1}}}, {{Window{from, from}, []uint32{1}}},
		{{window, []uint32{1}}, {window, []uint32{2}}}, make([]SelectedWindow, MaxCuts+1),
	} {
		source := new(selectedNoRead)
		summary, err := ReduceSelectedIntervals(t.Context(), source, expected, scope, plan, 30*second,
			func(IntervalSnapshot) error { t.Fatal("invalid plan emitted"); return nil })
		if !errors.Is(err, ErrScope) || source.calls != 0 || !reflect.DeepEqual(summary, IntervalSummary{}) {
			t.Fatal("invalid projection read source or returned accepted output")
		}
	}
	ids := make([]uint32, MaxInstruments)
	for i := range len(ids) {
		ids[i] = uint32(i + 1)
	}
	oversized := make([]SelectedWindow, MaxSelectedObservations/MaxInstruments+1)
	for i := range len(oversized) {
		oversized[i] = SelectedWindow{Window{from + uint64(i)*second, from + uint64(i+1)*second}, ids}
	}
	source := new(selectedNoRead)
	summary, err := ReduceSelectedIntervals(t.Context(), source, expected,
		Scope{"OPRA.PILLAR", testStart, ids}, oversized, 30*second, func(IntervalSnapshot) error { return nil })
	if !errors.Is(err, ErrScope) || source.calls != 0 || !reflect.DeepEqual(summary, IntervalSummary{}) {
		t.Fatal("projection memory bound was bypassed")
	}
}

func TestSelectedIntervalsValidateUnselectedRecordsAfterWindows(t *testing.T) {
	from := testStart + 60*second
	first := testRecord(nativebook.CMBP1, from-5*second, 'A')
	later := first
	later.InstrumentID, later.TsRecv, later.TsEvent = 2, from+2*second, from+2*second
	bad := later
	bad.TsRecv, bad.TsEvent, bad.Flags = from+3*second, from+3*second, bad.Flags|4
	data, expected := testStream(nativebook.CMBP1, false, first, later, bad)
	provisional := 0
	summary, err := ReduceSelectedIntervals(t.Context(), bytes.NewReader(data), expected,
		Scope{"OPRA.PILLAR", testStart, []uint32{1, 2}},
		[]SelectedWindow{{Window{from, from + second}, []uint32{1}}}, 30*second,
		func(IntervalSnapshot) error { provisional++; return nil })
	if !errors.Is(err, ErrRecord) || provisional != 1 || !reflect.DeepEqual(summary, IntervalSummary{}) {
		t.Fatal("an unselected invalid record was hidden by a completed projection")
	}
}

func TestSelectedIntervalsDiscardSummaryOnLateFailure(t *testing.T) {
	from := testStart + 60*second
	for _, kind := range []string{"digest", "trailing", "callback", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			book := testRecord(nativebook.CMBP1, from-5*second, 'A')
			later := book
			later.TsRecv, later.TsEvent = from+2*second, from+2*second
			data, expected := testStream(nativebook.CMBP1, false, book, later)
			if kind == "digest" {
				expected.NativeSHA256[0] ^= 1
			}
			if kind == "trailing" {
				data = append(data, 0)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			callbackError := errors.New("selected callback failed")
			provisional := 0
			summary, err := ReduceSelectedIntervals(ctx, bytes.NewReader(data), expected,
				Scope{"OPRA.PILLAR", testStart, []uint32{1}},
				[]SelectedWindow{{Window{from, from + second}, []uint32{1}}}, 30*second,
				func(IntervalSnapshot) error {
					provisional++
					if kind == "callback" {
						return callbackError
					}
					if kind == "cancel" {
						cancel()
					}
					return nil
				})
			if err == nil || provisional != 1 || !reflect.DeepEqual(summary, IntervalSummary{}) {
				t.Fatal("provisional selected output acquired an accepted summary after failure")
			}
			if kind == "callback" && !errors.Is(err, callbackError) || kind == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("callback or cancellation error was discarded")
			}
		})
	}
}

func TestSelectedIntervalsCallbackCannotMutatePlanOrAccounting(t *testing.T) {
	from := testStart + 60*second
	book := testRecord(nativebook.CMBP1, from-5*second, 'A')
	data, expected := testStream(nativebook.CMBP1, false, book)
	scope := Scope{"OPRA.PILLAR", testStart, []uint32{2, 1}}
	plan := []SelectedWindow{
		{Window{from, from + second}, []uint32{2, 1}},
		{Window{from + second, from + 2*second}, []uint32{1}},
	}
	seen := 0
	summary, err := ReduceSelectedIntervals(t.Context(), bytes.NewReader(data), expected, scope, plan, 30*second,
		func(snapshot IntervalSnapshot) error {
			if snapshot.Observations[0].Endpoint.InstrumentID != 1 || snapshot.Observations[0].Endpoint.Book.BidPx != 100 {
				t.Fatal("callback changed the next selected observation")
			}
			scope.Instruments[0] = 999
			plan[1].Instruments[0] = 2
			plan[1].Window.From++
			snapshot.Observations[0].Endpoint.Book.BidPx = -1
			clear(snapshot.Observations)
			seen++
			return nil
		})
	if err != nil || seen != 2 || summary.Windows != 2 || summary.Observations != 3 || summary.Native.Records != 1 {
		t.Fatalf("mutable callback changed accepted accounting: %+v %v", summary, err)
	}
}
