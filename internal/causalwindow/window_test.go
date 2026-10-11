package causalwindow

import (
	"autowheel/internal/causalbook"
	"autowheel/internal/nativebook"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
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

func TestClosedWindowBoundaries(t *testing.T) {
	cut := testStart + 100*second
	scope := Scope{Dataset: "OPRA.PILLAR", UTCStart: testStart,
		SourceStart: cut - WindowAge, SourceEnd: cut + 1, Instruments: []uint32{1, 2}}
	for _, action := range []byte{'A', 'T', 'R'} {
		t.Run(string(action), func(t *testing.T) {
			early := testRecord(nativebook.CMBP1, scope.SourceStart, 'A')
			atCut := testRecord(nativebook.CMBP1, cut, action)
			data, want := testStream(nativebook.CMBP1, false, early, atCut)
			var actual, reference Snapshot
			summary, err := Reduce(t.Context(), bytes.NewReader(data), want, scope, []uint64{cut}, WindowAge,
				func(s Snapshot) error { actual = s; return nil })
			if err != nil {
				t.Fatal(err)
			}
			daily, err := causalbook.Reduce(t.Context(), bytes.NewReader(data), want,
				causalbook.Scope{Dataset: scope.Dataset, UTCStart: scope.UTCStart, Instruments: scope.Instruments},
				[]uint64{cut}, WindowAge, func(s Snapshot) error { reference = s; return nil })
			if err != nil || !reflect.DeepEqual(actual, reference) || !reflect.DeepEqual(summary, daily) {
				t.Fatalf("window parity: %v", err)
			}
			if actual.Observations[0].Book.TsRecv > cut || !actual.Observations[1].AgeWindowCovered ||
				!actual.Observations[1].ZeroBookUpdatesInAgeWindow {
				t.Fatal("closed cutoff or declared age coverage changed")
			}
		})
	}
	for _, change := range []func(*Scope){
		func(s *Scope) { s.SourceStart++ }, func(s *Scope) { s.SourceEnd-- },
		func(s *Scope) { s.SourceEnd++ }, func(s *Scope) { s.SourceStart = s.UTCStart - 1 },
		func(s *Scope) { s.SourceEnd = s.UTCStart + DayNanos + 1 },
		func(s *Scope) { s.UTCStart++ }, func(s *Scope) { s.Dataset = "EQUS.MINI" },
		func(s *Scope) { s.Instruments = []uint32{1, 1} },
	} {
		altered := scope
		change(&altered)
		_, err := Reduce(t.Context(), bytes.NewReader(nil), nativebook.Expected{Schema: nativebook.CMBP1}, altered,
			[]uint64{cut}, WindowAge, func(Snapshot) error { return nil })
		if !errors.Is(err, ErrScope) {
			t.Fatalf("unbound window accepted: %v", err)
		}
	}
	for _, cuts := range [][]uint64{nil, {cut - 1}, {cut + 1}, {cut, cut + 1}} {
		_, err := Reduce(t.Context(), bytes.NewReader(nil), nativebook.Expected{Schema: nativebook.CMBP1}, scope,
			cuts, WindowAge, func(Snapshot) error { return nil })
		if !errors.Is(err, ErrScope) {
			t.Fatalf("unbound cuts accepted: %v", err)
		}
	}
	for _, recv := range []uint64{scope.SourceStart - 1, scope.SourceEnd} {
		data, want := testStream(nativebook.CMBP1, false, testRecord(nativebook.CMBP1, recv, 'A'))
		sum, err := Reduce(t.Context(), bytes.NewReader(data), want, scope, []uint64{cut}, WindowAge, func(Snapshot) error { return nil })
		if !errors.Is(err, ErrCapture) || !reflect.DeepEqual(sum, Summary{}) {
			t.Fatal("outside-window receive accepted")
		}
	}
	data, want := testStream(nativebook.CMBP1, false, testRecord(nativebook.CMBP1, cut, 'A'))
	want.NativeSHA256[0] ^= 1
	sum, err := Reduce(t.Context(), bytes.NewReader(data), want, scope, []uint64{cut}, WindowAge, func(Snapshot) error { return nil })
	if err == nil || !reflect.DeepEqual(sum, Summary{}) {
		t.Fatal("unverified EOF committed")
	}
}
