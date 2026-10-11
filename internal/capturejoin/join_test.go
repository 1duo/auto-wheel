package capturejoin

// Join binding checks only; they do not qualify source evidence nor mirror the status reducer.

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"

	"autowheel/internal/causalbook"
	"autowheel/internal/nativebook"
)

func day() uint64 { return 10 * causalbook.DayNanos }
func obsFor(ids []uint32) []causalbook.Observation {
	out := make([]causalbook.Observation, len(ids))
	for i := range ids {
		out[i] = causalbook.Observation{InstrumentID: ids[i]}
	}
	return out
}
func snapFor(ds string, utc, cut, maxAge uint64, ids []uint32) causalbook.Snapshot {
	return causalbook.Snapshot{Dataset: ds, UTCStart: utc, Through: cut, MaxAgeNanos: maxAge, Observations: obsFor(ids)}
}
func TestOverlapRejectedBeforeReads(t *testing.T) {
	ctx := t.Context()
	utc, cut := day(), day()+1000
	reads := new(int)
	mk := func() Source {
		return Source{Scope: causalbook.Scope{Dataset: "OPRA.PILLAR", UTCStart: utc, Instruments: []uint32{3, 7}}, Next: func() (causalbook.Snapshot, error) {
			*reads++
			return snapFor("OPRA.PILLAR", utc, cut, 1, []uint32{3, 7}), nil
		}}
	}
	emitted := new(int)
	_, err := Join(ctx, []Source{mk(), mk()}, []uint64{cut}, 1, func(CaptureSet) error { *emitted++; return nil })
	if !errors.Is(err, ErrOverlap) || *reads != 0 || *emitted != 0 {
		t.Fatalf("overlap must block reads: %v reads=%d emitted=%d", err, *reads, *emitted)
	}
}
func TestNumericCollisionAcrossDatasetsAccepted(t *testing.T) {
	ctx := t.Context()
	utc, cut := day(), day()+2000
	book := nativebook.Record{InstrumentID: 7, TsEvent: utc + 5, TsRecv: cut - 10, Flags: 192, BidPx: 1, AskPx: 2, BidSz: 1, AskSz: 1}
	s1 := causalbook.Snapshot{Dataset: "OPRA.PILLAR", UTCStart: utc, Through: cut, MaxAgeNanos: 9, Observations: []causalbook.Observation{{InstrumentID: 7, HasBook: true, Book: book, BookAvailableAt: cut - 10, Status: causalbook.Fresh}}}
	s2 := causalbook.Snapshot{Dataset: "EQUS.MINI", UTCStart: utc, Through: cut, MaxAgeNanos: 9, Observations: []causalbook.Observation{{InstrumentID: 7, HasTrade: true, Trade: nativebook.Record{InstrumentID: 7, TsEvent: ^uint64(0)}, Status: causalbook.TradeOnly}}}
	mk := func(s causalbook.Snapshot, ds string) Source {
		n := new(int)
		return Source{Scope: causalbook.Scope{Dataset: ds, UTCStart: utc, Instruments: []uint32{7}}, Next: func() (causalbook.Snapshot, error) {
			*n++
			if *n == 1 {
				return s, nil
			}
			return causalbook.Snapshot{}, io.EOF
		}}
	}
	var got []CaptureSet
	sum, err := Join(ctx, []Source{mk(s1, "OPRA.PILLAR"), mk(s2, "EQUS.MINI")}, []uint64{cut}, 9, func(cs CaptureSet) error { got = append(got, cs); return nil })
	if err != nil || len(got) != 1 || got[0].Snapshots[0].Observations[0].Book != book || got[0].Snapshots[0].Observations[0].Status != causalbook.Fresh || got[0].Snapshots[1].Observations[0].Status != causalbook.TradeOnly || sum.Sets != 1 {
		t.Fatalf("cross-dataset collision failed: %v %+v", err, sum)
	}
}
func TestMissingMisalignedNeverEmitted(t *testing.T) {
	ctx := t.Context()
	utc, cut := day(), day()+3000
	good := func() Source {
		n := new(int)
		return Source{Scope: causalbook.Scope{Dataset: "OPRA.PILLAR", UTCStart: utc, Instruments: []uint32{1}}, Next: func() (causalbook.Snapshot, error) {
			*n++
			if *n > 1 {
				return causalbook.Snapshot{}, io.EOF
			}
			return snapFor("OPRA.PILLAR", utc, cut, 1, []uint32{1}), nil
		}}
	}
	badThrough := Source{Scope: causalbook.Scope{Dataset: "EQUS.MINI", UTCStart: utc, Instruments: []uint32{9}}, Next: func() (causalbook.Snapshot, error) { return snapFor("EQUS.MINI", utc, cut+1, 1, []uint32{9}), nil }}
	missing := Source{Scope: causalbook.Scope{Dataset: "EQUS.MINI", UTCStart: utc, Instruments: []uint32{9}}, Next: func() (causalbook.Snapshot, error) { return causalbook.Snapshot{}, io.EOF }}
	for _, srcs := range [][]Source{{good(), badThrough}, {good(), missing}} {
		emitted := 0
		if _, err := Join(ctx, srcs, []uint64{cut}, 1, func(CaptureSet) error { emitted++; return nil }); !errors.Is(err, ErrCapture) || emitted != 0 {
			t.Fatalf("missing/misaligned must block emit: %v emitted=%d", err, emitted)
		}
	}
}
func TestTrailingErrorZeroSummary(t *testing.T) {
	ctx := t.Context()
	utc := day()
	cuts := []uint64{utc + 10, utc + 20}
	mk := func(ds string, id uint32) Source {
		n := new(int)
		return Source{Scope: causalbook.Scope{Dataset: ds, UTCStart: utc, Instruments: []uint32{id}}, Next: func() (causalbook.Snapshot, error) {
			*n++
			if *n <= len(cuts) {
				return snapFor(ds, utc, cuts[*n-1], 1, []uint32{id}), nil
			}
			return causalbook.Snapshot{}, errors.New("trailing boom")
		}}
	}
	emitted := new(int)
	sum, err := Join(ctx, []Source{mk("OPRA.PILLAR", 1), mk("EQUS.MINI", 2)}, cuts, 1, func(CaptureSet) error { *emitted++; return nil })
	if !errors.Is(err, ErrTrailing) || sum != (Summary{}) || *emitted != len(cuts) {
		t.Fatalf("trailing must zero summary: %v %+v %d", err, sum, *emitted)
	}
}
func TestFinalCallbackCancellationZeroSummary(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	utc := day()
	cuts := []uint64{utc + 10, utc + 20}
	mk := func() Source {
		n := new(int)
		return Source{Scope: causalbook.Scope{Dataset: "OPRA.PILLAR", UTCStart: utc, Instruments: []uint32{1}}, Next: func() (causalbook.Snapshot, error) {
			*n++
			if *n <= len(cuts) {
				return snapFor("OPRA.PILLAR", utc, cuts[*n-1], 1, []uint32{1}), nil
			}
			return causalbook.Snapshot{}, io.EOF
		}}
	}
	sum, err := Join(ctx, []Source{mk()}, cuts, 1, func(cs CaptureSet) error {
		if cs.Through == cuts[len(cuts)-1] {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || sum != (Summary{}) {
		t.Fatalf("final cancel must zero summary: %v %+v", err, sum)
	}
}

func TestEOFWithPacketRejected(t *testing.T) {
	utc, cut := day(), day()+10
	packet := snapFor("OPRA.PILLAR", utc, cut, 1, []uint32{1})
	n := 0
	source := Source{Scope: causalbook.Scope{Dataset: packet.Dataset, UTCStart: utc, Instruments: []uint32{1}}, Next: func() (causalbook.Snapshot, error) {
		n++
		if n == 1 {
			return packet, nil
		}
		return packet, io.EOF
	}}
	sum, err := Join(t.Context(), []Source{source}, []uint64{cut}, 1, func(CaptureSet) error { return nil })
	if !errors.Is(err, ErrTrailing) || sum != (Summary{}) {
		t.Fatalf("packet with EOF accepted: %v %+v", err, sum)
	}
}

func TestCallbackMutationPreservesInputAndCount(t *testing.T) {
	utc, cut := day(), day()+10
	packet := snapFor("OPRA.PILLAR", utc, cut, 1, []uint32{1, 2})
	n := 0
	source := Source{Scope: causalbook.Scope{Dataset: packet.Dataset, UTCStart: utc, Instruments: []uint32{1, 2}}, Next: func() (causalbook.Snapshot, error) {
		n++
		if n == 1 {
			return packet, nil
		}
		return causalbook.Snapshot{}, io.EOF
	}}
	sum, err := Join(t.Context(), []Source{source}, []uint64{cut}, 1, func(set CaptureSet) error {
		set.Snapshots[0].Observations[0].InstrumentID = 999
		set.Snapshots[0].Observations = nil
		return nil
	})
	if err != nil || sum.Observations != 2 || packet.Observations[0].InstrumentID != 1 {
		t.Fatalf("callback altered input/count: %v %+v", err, sum)
	}
}
func TestCallerMutationDoesNotAlterBound(t *testing.T) {
	ctx := t.Context()
	utc := day()
	ids := []uint32{7, 3}
	cuts := []uint64{utc + 10, utc + 20}
	origCuts := slices.Clone(cuts)
	first := new(int)
	src := Source{Scope: causalbook.Scope{Dataset: "OPRA.PILLAR", UTCStart: utc, Instruments: ids}, Next: func() (causalbook.Snapshot, error) {
		*first++
		if *first == 1 {
			ids[0] = 999
			cuts[0] = utc
		}
		if *first <= len(origCuts) {
			return snapFor("OPRA.PILLAR", utc, origCuts[*first-1], 1, []uint32{3, 7}), nil
		}
		return causalbook.Snapshot{}, io.EOF
	}}
	var through []uint64
	sum, err := Join(ctx, []Source{src}, cuts, 1, func(cs CaptureSet) error { through = append(through, cs.Through); return nil })
	if err != nil || sum.Sets != 2 || through[0] != origCuts[0] || through[1] != origCuts[1] {
		t.Fatalf("mutation altered bound: %v %+v %v", err, sum, through)
	}
}
