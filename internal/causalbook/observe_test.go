package causalbook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"slices"
	"testing"

	"autowheel/internal/nativebook"
)

const second uint64 = 1_000_000_000
const testStart uint64 = 1_775_433_600_000_000_000 // 2026-04-06 UTC; synthetic data.

func testRecord(schema nativebook.Schema, at uint64, action byte) nativebook.Record {
	r := nativebook.Record{InstrumentID: 1, TsRecv: at, TsEvent: at - second,
		Action: action, Side: 'N', PublisherID: 30, Price: 105, Size: 1, Flags: 192,
		BidPx: 100, AskPx: 110, BidSz: 2, AskSz: 3, BidPublisher: 30, AskPublisher: 35}
	if schema == nativebook.MBP1 {
		r.PublisherID, r.Flags = 95, 128
	}
	return r
}

// The opaque header is a synthetic integrity attestation, not provider evidence.
// Native layout/metadata decoding has separate pinned official reference tests.
func testStream(schema nativebook.Schema, out bool, records ...nativebook.Record) ([]byte, nativebook.Expected) {
	data := make([]byte, nativebook.MinMetadataBytes)
	copy(data, "DBN")
	data[3] = 1
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	headerHash := sha256.Sum256(data)
	for _, r := range records {
		length := 80
		if out {
			length = 88
		}
		wire := make([]byte, length)
		wire[0], wire[1] = byte(length/4), 177
		if schema == nativebook.MBP1 {
			wire[1] = 1
		}
		le := binary.LittleEndian
		le.PutUint16(wire[2:4], r.PublisherID)
		le.PutUint32(wire[4:8], r.InstrumentID)
		le.PutUint64(wire[8:16], r.TsEvent)
		le.PutUint64(wire[16:24], uint64(r.Price))
		le.PutUint32(wire[24:28], r.Size)
		wire[28], wire[29], wire[30] = r.Action, r.Side, r.Flags
		le.PutUint64(wire[32:40], r.TsRecv)
		le.PutUint32(wire[40:44], uint32(r.TsInDelta))
		le.PutUint64(wire[48:56], uint64(r.BidPx))
		le.PutUint64(wire[56:64], uint64(r.AskPx))
		le.PutUint32(wire[64:68], r.BidSz)
		le.PutUint32(wire[68:72], r.AskSz)
		if schema == nativebook.CMBP1 {
			le.PutUint16(wire[72:74], r.BidPublisher)
			le.PutUint16(wire[76:78], r.AskPublisher)
		} else {
			wire[31] = r.Depth
			le.PutUint32(wire[44:48], r.Sequence)
			le.PutUint32(wire[72:76], r.BidCount)
			le.PutUint32(wire[76:80], r.AskCount)
		}
		if out {
			le.PutUint64(wire[80:88], r.TsOut)
		}
		data = append(data, wire...)
	}
	return data, nativebook.Expected{Schema: schema, Version: 1, TsOut: out,
		Records: uint64(len(records)), NativeBytes: uint64(len(data)),
		MetadataBytes: nativebook.MinMetadataBytes, MetadataSHA256: headerHash,
		NativeSHA256: sha256.Sum256(data)}
}

func TestReduceRawCaptureStates(t *testing.T) {
	scope := Scope{Dataset: "OPRA.PILLAR", UTCStart: testStart, Instruments: []uint32{2, 1}}
	book := testRecord(nativebook.CMBP1, testStart+10*second, 'A')
	trade := testRecord(nativebook.CMBP1, testStart+20*second, 'T')
	next := testRecord(nativebook.CMBP1, trade.TsRecv, 'A')
	next.BidPx, next.AskPx = 105, 115
	last := next
	last.Action, last.TsRecv, last.TsEvent = 'T', testStart+21*second, testStart+20*second
	data, expected := testStream(nativebook.CMBP1, false, book, trade, next, last)
	cuts := []uint64{testStart + 9*second, book.TsRecv, next.TsRecv, testStart + 30*second, testStart + 30*second + 1}
	var snapshots []Snapshot
	summary, err := Reduce(t.Context(), bytes.NewReader(data), expected, scope, cuts, 10*second,
		func(s Snapshot) error { snapshots = append(snapshots, s); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if summary.Native.Records != 4 || summary.BookRecords != 2 || summary.TradeRecords != 2 ||
		summary.EqualCaptureRecords != 1 || summary.CompletedEvents != 4 || len(snapshots) != 5 {
		t.Fatalf("summary: %+v", summary)
	}
	for i, want := range []Status{Unseen, Fresh, Fresh, Fresh, Stale} {
		if snapshots[i].Observations[0].InstrumentID != 1 || snapshots[i].Observations[0].Status != want ||
			snapshots[i].Observations[1].Status != Unseen {
			t.Fatalf("cut %d: %+v", i, snapshots[i])
		}
	}
	at := snapshots[3].Observations[0]
	if at.Book.Index != 2 || at.Book.TsRecv != next.TsRecv || at.Trade.Index != 3 || at.Book.BidPx != 105 ||
		at.ZeroBookUpdatesInAgeWindow || !snapshots[4].Observations[0].ZeroBookUpdatesInAgeWindow {
		t.Fatal("equal capture or closed age boundary changed")
	}
	if snapshots[0].Observations[0].AgeWindowCovered || snapshots[0].Observations[0].ZeroBookUpdatesInAgeWindow {
		t.Fatal("source does not cover window before partition start")
	}
	// Emitted data never aliases reducer state or caller-owned scope/cuts.
	snapshots[1].Observations[0].Book.BidPx = 999
	if snapshots[2].Observations[0].Book.BidPx != 105 || scope.Instruments[0] != 2 || cuts[0] != testStart+9*second {
		t.Fatal("snapshot or caller input mutated")
	}

	t.Run("events_trade_reset_and_pending", func(t *testing.T) {
		scope.Dataset = "EQUS.MINI"
		first := testRecord(nativebook.MBP1, testStart+second, 'A')
		middle := testRecord(nativebook.MBP1, testStart+2*second, 'A')
		middle.Flags = 0
		middle.BidPx = 90
		end := middle
		end.Action, end.TsRecv = 'T', testStart+3*second
		end.TsEvent, end.Flags = end.TsRecv-second, 128
		reset := testRecord(nativebook.MBP1, testStart+4*second, 'R')
		uncertain := testRecord(nativebook.MBP1, testStart+6*second, 'T')
		uncertain.BidPx = 95
		restore := testRecord(nativebook.MBP1, testStart+5*second, 'A')
		trailing := testRecord(nativebook.MBP1, testStart+7*second, 'A')
		trailing.Flags = 0
		data, expected := testStream(nativebook.MBP1, false, first, middle, end, reset, restore, uncertain, trailing)
		cuts := []uint64{middle.TsRecv, end.TsRecv, reset.TsRecv, restore.TsRecv, uncertain.TsRecv, trailing.TsRecv}
		var got []Snapshot
		sum, err := Reduce(t.Context(), bytes.NewReader(data), expected, scope, cuts, 30*second, func(s Snapshot) error { got = append(got, s); return nil })
		if err != nil {
			t.Fatal(err)
		}
		for i, want := range []Status{Pending, Fresh, Reset, Fresh, Ambiguous, Pending} {
			if got[i].Observations[0].Status != want {
				t.Fatalf("cut %d: %s want %s", i, got[i].Observations[0].Status, want)
			}
		}
		if got[1].Observations[0].Book.TsRecv != middle.TsRecv || got[1].Observations[0].BookAvailableAt != end.TsRecv ||
			got[4].Observations[0].Book.TsRecv != restore.TsRecv || sum.PendingEventsAtEOF != 1 {
			t.Fatal("event completion or trade changed quote clock")
		}
	})

	t.Run("uint64_clocks_and_event_regressions", func(t *testing.T) {
		scope.UTCStart = (uint64(math.MaxInt64)/DayNanos + 1) * DayNanos
		scope.Dataset = "OPRA.PILLAR"
		first := testRecord(nativebook.CMBP1, scope.UTCStart+2*second, 'A')
		first.TsOut = math.MaxUint64
		secondRecord := first
		secondRecord.TsRecv += second
		secondRecord.TsEvent -= second
		undefined := secondRecord
		undefined.TsRecv += second
		undefined.TsEvent = math.MaxUint64
		lead := secondRecord
		lead.TsRecv += 2 * second
		lead.TsEvent = lead.TsRecv + 1
		data, expected := testStream(nativebook.CMBP1, true, first, secondRecord, undefined, lead)
		var got []Snapshot
		sum, err := Reduce(t.Context(), bytes.NewReader(data), expected, scope, []uint64{secondRecord.TsRecv, undefined.TsRecv, lead.TsRecv}, 30*second, func(s Snapshot) error { got = append(got, s); return nil })
		if err != nil {
			t.Fatal(err)
		}
		if sum.EventClockRegressions != 1 || sum.UndefinedEventClocks != 1 || sum.ClockLeads != 1 ||
			got[0].Observations[0].Book.TsRecv <= math.MaxInt64 || got[0].Observations[0].Book.TsOut != math.MaxUint64 ||
			got[1].Observations[0].Status != ClockUnknown || got[2].Observations[0].Status != ClockLead {
			t.Fatal("raw clock fidelity")
		}
	})
}

func TestReduceRejectsUnboundAndIncompleteInput(t *testing.T) {
	scope := Scope{Dataset: "OPRA.PILLAR", UTCStart: testStart, Instruments: []uint32{1}}
	book := testRecord(nativebook.CMBP1, testStart+10*second, 'A')
	cuts := []uint64{testStart + 5*second, testStart + 11*second}
	for _, test := range []struct {
		name   string
		change func(*nativebook.Record)
		want   error
	}{
		{"unknown_identity", func(r *nativebook.Record) { r.InstrumentID = 3 }, ErrRecord},
		{"unknown_action", func(r *nativebook.Record) { r.Action = 'F' }, ErrRecord},
		{"unknown_side", func(r *nativebook.Record) { r.Side = '?' }, ErrRecord},
		{"bad_clock_flag", func(r *nativebook.Record) { r.Flags |= 8 }, ErrRecord},
		{"gap_flag", func(r *nativebook.Record) { r.Flags |= 4 }, ErrRecord},
		{"snapshot_flag", func(r *nativebook.Record) { r.Flags |= 32 }, ErrRecord},
		{"publisher_specific_flag", func(r *nativebook.Record) { r.Flags |= 2 }, ErrRecord},
		{"missing_cmp_final", func(r *nativebook.Record) { r.Flags = 64 }, ErrRecord},
		{"undefined_receive", func(r *nativebook.Record) { r.TsRecv = math.MaxUint64 }, ErrCapture},
		{"outside_partition", func(r *nativebook.Record) { r.TsRecv = testStart + DayNanos }, ErrCapture},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := book
			test.change(&r)
			data, expected := testStream(nativebook.CMBP1, false, r)
			sum, err := Reduce(t.Context(), bytes.NewReader(data), expected, scope, cuts, 30*second, func(Snapshot) error { return nil })
			if !errors.Is(err, test.want) || !reflect.DeepEqual(sum, Summary{}) {
				t.Fatalf("sum %+v, error %v", sum, err)
			}
		})
	}
	for _, name := range []string{"receive_regression", "truncated", "bad_digest", "trailing", "callback", "cancel"} {
		t.Run(name, func(t *testing.T) {
			later := book
			later.TsRecv += second
			rows := []nativebook.Record{book, later}
			if name == "receive_regression" {
				rows[1].TsRecv = book.TsRecv - 1
			}
			data, expected := testStream(nativebook.CMBP1, false, rows...)
			if name == "truncated" {
				data = data[:len(data)-1]
			}
			if name == "bad_digest" {
				expected.NativeSHA256[0] ^= 1
			}
			if name == "trailing" {
				data = append(data, 0)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			emitted := 0
			sum, err := Reduce(ctx, bytes.NewReader(data), expected, scope, cuts, 30*second, func(Snapshot) error {
				emitted++
				if name == "callback" {
					return errors.New("synthetic emitter failure")
				}
				if name == "cancel" {
					cancel()
				}
				return nil
			})
			if err == nil || !reflect.DeepEqual(sum, Summary{}) {
				t.Fatalf("accepted %s", name)
			}
			if name != "truncated" && emitted == 0 {
				t.Fatal("did not exercise provisional emission")
			}
		})
	}
	for _, name := range []string{"duplicate_identity", "too_many_identities", "duplicate_cut", "outside_cut", "wrong_dataset", "overflow_partition"} {
		t.Run(name, func(t *testing.T) {
			s := scope
			c := slices.Clone(cuts)
			switch name {
			case "duplicate_identity":
				s.Instruments = []uint32{1, 1}
			case "too_many_identities":
				s.Instruments = make([]uint32, MaxInstruments+1)
			case "duplicate_cut":
				c[1] = c[0]
			case "outside_cut":
				c[0] = testStart - 1
			case "wrong_dataset":
				s.Dataset = "EQUS.MINI"
			case "overflow_partition":
				s.UTCStart = math.MaxUint64 / DayNanos * DayNanos
			}
			data, expected := testStream(nativebook.CMBP1, false, book)
			sum, err := Reduce(t.Context(), bytes.NewReader(data), expected, s, c, 30*second, func(Snapshot) error { return nil })
			if !errors.Is(err, ErrScope) || !reflect.DeepEqual(sum, Summary{}) {
				t.Fatalf("accepted %s: %v", name, err)
			}
		})
	}
}

func TestReduceEmptySourceAndFinalCancellation(t *testing.T) {
	data, expected := testStream(nativebook.CMBP1, false)
	scope := Scope{Dataset: "OPRA.PILLAR", UTCStart: testStart, Instruments: []uint32{1}}
	cut := []uint64{testStart + 30*second}
	var got Snapshot
	sum, err := Reduce(t.Context(), bytes.NewReader(data), expected, scope, cut, 30*second,
		func(s Snapshot) error { got = s; return nil })
	if err != nil || sum.Native.Records != 0 || sum.Snapshots != 1 || got.Observations[0].HasBook ||
		got.Observations[0].Status != Unseen || !got.Observations[0].ZeroBookUpdatesInAgeWindow {
		t.Fatal("empty complete raw source was not represented explicitly")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sum, err = Reduce(ctx, bytes.NewReader(data), expected, scope, cut, 30*second,
		func(Snapshot) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(sum, Summary{}) {
		t.Fatal("final callback cancellation committed a summary")
	}
}

func TestReduceDocumentedFlagsAndRawInvalidStates(t *testing.T) {
	for _, schema := range []nativebook.Schema{nativebook.CMBP1, nativebook.MBP1} {
		t.Run(string(schema), func(t *testing.T) {
			scope := Scope{Dataset: "OPRA.PILLAR", UTCStart: testStart, Instruments: []uint32{1}}
			if schema == nativebook.MBP1 {
				scope.Dataset = "EQUS.MINI"
			}
			for _, tc := range []struct {
				name   string
				want   Status
				change func(*nativebook.Record)
			}{
				{"reserved_bit", Fresh, func(r *nativebook.Record) { r.Flags |= 1 }},
				{"locked", Fresh, func(r *nativebook.Record) { r.AskPx = r.BidPx }},
				{"undefined", Undefined, func(r *nativebook.Record) { r.BidPx = math.MaxInt64 }},
				{"nonpositive", Nonpositive, func(r *nativebook.Record) { r.BidPx = 0 }},
				{"empty", Empty, func(r *nativebook.Record) { r.BidSz = 0 }},
				{"crossed", Crossed, func(r *nativebook.Record) { r.BidPx = r.AskPx + 1 }},
				{"trade_only", TradeOnly, func(r *nativebook.Record) { r.Action = 'T' }},
			} {
				t.Run(tc.name, func(t *testing.T) {
					r := testRecord(schema, testStart+10*second, 'A')
					tc.change(&r)
					data, expected := testStream(schema, false, r)
					var got Snapshot
					_, err := Reduce(t.Context(), bytes.NewReader(data), expected, scope, []uint64{r.TsRecv}, 5*second,
						func(s Snapshot) error { got = s; return nil })
					if err != nil || got.Observations[0].Status != tc.want {
						t.Fatalf("%v, %s", err, got.Observations[0].Status)
					}
					if r.Action != 'T' && (got.Observations[0].Book.Flags != r.Flags || got.Observations[0].Book.BidPx != r.BidPx ||
						got.Observations[0].ZeroBookUpdatesInAgeWindow) {
						t.Fatal("invalid raw book became absence or lost original fields")
					}
				})
			}
		})
	}
}

func TestReduceNativeDepthIsPreservedAndWithheld(t *testing.T) {
	for _, tc := range []struct {
		action byte
		want   Status
	}{
		{'R', Reset}, {'T', TradeOnly}, {'A', UnknownDepth},
	} {
		t.Run(string(tc.action), func(t *testing.T) {
			r := testRecord(nativebook.MBP1, testStart+10*second, tc.action)
			r.Depth = 255
			if tc.action == 'R' {
				r.Side = 'N'
			}
			data, expected := testStream(nativebook.MBP1, false, r)
			scope := Scope{Dataset: "EQUS.MINI", UTCStart: testStart, Instruments: []uint32{1}}
			var got Snapshot
			_, err := Reduce(t.Context(), bytes.NewReader(data), expected, scope, []uint64{r.TsRecv}, 5*second,
				func(s Snapshot) error { got = s; return nil })
			if err != nil || got.Observations[0].Status != tc.want {
				t.Fatalf("%v, %+v", err, got)
			}
			o := got.Observations[0]
			if tc.action == 'T' && (!o.HasTrade || o.Trade.Depth != 255 || o.HasBook) ||
				tc.action != 'T' && (!o.HasBook || o.Book.Depth != 255) {
				t.Fatal("native depth lost or trade became a book")
			}
		})
	}
}
