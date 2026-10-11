package causaloi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"testing"
)

const testRaw = "TSLA  260422C00255000"

func fixture(records []Record) Source {
	index := 0
	return Source{UTCStart: DayNanos, NativeVersion: 1, ExpectedRecords: uint64(len(records)),
		SourceSHA256: [32]byte{1}, Bindings: []Binding{{RawSymbol: testRaw, InstrumentID: 1,
			Definition: new(Definition{InstrumentID: 1, ReceiveAt: DayNanos + 90, EventAt: DayNanos + 89})}},
		Next: func(context.Context) (Record, error) {
			if index == len(records) {
				return Record{}, io.EOF
			}
			r := records[index]
			index++
			return r, nil
		}}
}

func record(index uint64, publisher uint16, at uint64, quantity int32) Record {
	return Record{Index: index, NativeVersion: 1, RawSymbol: testRaw, PublisherID: publisher,
		InstrumentID: 1, ReceiveAt: DayNanos + at, EventAt: DayNanos + at - 1,
		ReferenceAt: math.MaxUint64, Quantity: quantity, StatType: 9, UpdateAction: 1, Rtype: 24}
}

func TestClosedCutsAndLatestCopies(t *testing.T) {
	rows := []Record{record(0, 30, 100, 0), record(1, 31, 100, 0), record(2, 30, 101, 1),
		record(3, 30, 102, 2), record(4, 31, 102, 2), record(5, 30, 103, 2), record(6, 31, 105, 9)}
	rows[3].UpdateAction = 2
	var views []Snapshot
	result, err := Reduce(t.Context(), fixture(rows), []uint64{DayNanos + 99, DayNanos + 100, DayNanos + 101, DayNanos + 102, DayNanos + 103, DayNanos + 104},
		func(s Snapshot) error { views = append(views, s); return nil })
	if err != nil || result.Records != 7 || result.Snapshots != 6 || result.Observations != 6 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if views[0].Observations[0].Status != NotObserved || views[0].Observations[0].Quantity != nil {
		t.Fatal("prepublication value invented")
	}
	zero := views[1].Observations[0]
	if zero.Quantity == nil || *zero.Quantity != 0 || zero.Copies != 2 || zero.Record.Index != 1 || zero.ReferenceAt != nil {
		t.Fatal("closed boundary/zero/reference/tie changed")
	}
	if views[2].Observations[0].Status != CopyConflict || views[2].Observations[0].Quantity != nil {
		t.Fatal("conflicting copies summed or chosen")
	}
	if views[3].Observations[0].Status != UnsupportedCopy || views[3].Observations[0].Quantity != nil {
		t.Fatal("delete retained previous usable quantity")
	}
	for _, view := range views[4:] {
		if q := view.Observations[0].Quantity; q == nil || *q != 2 {
			t.Fatal("recovery or future exclusion changed")
		}
	}
}

func TestUnknownNativeValues(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Record)
	}{
		{"int32 sentinel", func(r *Record) { r.Quantity = math.MaxInt32 }},
		{"negative", func(r *Record) { r.Quantity = -1 }},
		{"event lead", func(r *Record) { r.EventAt = r.ReceiveAt + 1 }},
		{"event missing", func(r *Record) { r.EventAt = math.MaxUint64 }},
		{"reference lead", func(r *Record) { r.ReferenceAt = r.ReceiveAt + 1 }},
		{"flags", func(r *Record) { r.Flags = 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := record(0, 30, 100, 7)
			tc.edit(&r)
			var got Observation
			_, err := Reduce(t.Context(), fixture([]Record{r}), []uint64{DayNanos + 100}, func(s Snapshot) error { got = s.Observations[0]; return nil })
			if err != nil || got.Status != UnsupportedCopy || got.Quantity != nil || got.Record != r {
				t.Fatalf("record lost/unknown value used: %+v %v", got, err)
			}
		})
	}
	price := record(0, 30, 100, math.MaxInt32)
	price.StatType = 1
	price.EventAt = math.MaxUint64
	var got Observation
	_, err := Reduce(t.Context(), fixture([]Record{price}), []uint64{DayNanos + 100}, func(s Snapshot) error { got = s.Observations[0]; return nil })
	if err != nil || got.Status != NotObserved {
		t.Fatal("price statistics changed OI")
	}
}

func TestMissingAndLateDefinitions(t *testing.T) {
	for _, missing := range []bool{true, false} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			source := fixture([]Record{record(0, 30, 100, 7)})
			if missing {
				source.Bindings[0].Definition = nil
			} else {
				source.Bindings[0].Definition.ReceiveAt = DayNanos + 102
			}
			var views []Snapshot
			_, err := Reduce(t.Context(), source, []uint64{DayNanos + 101, DayNanos + 103}, func(s Snapshot) error { views = append(views, s); return nil })
			if err != nil || views[0].Observations[0].Status != DefinitionUnknown || views[0].Observations[0].Quantity != nil {
				t.Fatal("unknown definition accepted")
			}
			if !missing && (views[1].Observations[0].Quantity == nil || *views[1].Observations[0].Quantity != 7) {
				t.Fatal("late definition retimed or lost")
			}
		})
	}
}

func TestAnyFailureDiscardsSummary(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Source, []Record)
	}{
		{"order", func(_ *Source, r []Record) { r[1].ReceiveAt = r[0].ReceiveAt - 1 }},
		{"future identity drift", func(_ *Source, r []Record) { r[1].InstrumentID = 2 }},
		{"wrong row index", func(_ *Source, r []Record) { r[1].Index = 3 }},
		{"short count", func(s *Source, _ []Record) { s.ExpectedRecords++ }},
		{"extra count", func(s *Source, _ []Record) { s.ExpectedRecords-- }},
		{"wrapped eof", func(s *Source, _ []Record) {
			next := s.Next
			s.Next = func(ctx context.Context) (Record, error) {
				r, e := next(ctx)
				if e != nil {
					return r, fmt.Errorf("trailer: %w", e)
				}
				return r, nil
			}
		}},
		{"eof payload", func(s *Source, _ []Record) {
			next := s.Next
			s.Next = func(ctx context.Context) (Record, error) {
				r, e := next(ctx)
				if e != nil {
					return Record{Quantity: 1}, e
				}
				return r, nil
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := []Record{record(0, 30, 100, 7), record(1, 30, 110, 7)}
			source := fixture(rows)
			tc.edit(&source, rows)
			result, err := Reduce(t.Context(), source, []uint64{DayNanos + 100}, func(Snapshot) error { return nil })
			if err == nil || result != (Summary{}) {
				t.Fatalf("partial output declared complete: %+v %v", result, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	result, err := Reduce(ctx, fixture([]Record{record(0, 30, 100, 7)}), []uint64{DayNanos + 100}, func(Snapshot) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) || result != (Summary{}) {
		t.Fatal("callback cancellation declared success")
	}
}

func TestCallbackCannotMutateState(t *testing.T) {
	source := fixture([]Record{record(0, 30, 100, 7)})
	cuts := []uint64{DayNanos + 100, DayNanos + 101}
	seen := 0
	_, err := Reduce(t.Context(), source, cuts, func(s Snapshot) error {
		if s.Observations[0].Quantity == nil || *s.Observations[0].Quantity != 7 {
			t.Fatal("state mutated")
		}
		*s.Observations[0].Quantity = 99
		s.Observations[0].Record.Quantity = 99
		source.Bindings[0].Definition.ReceiveAt = math.MaxUint64
		cuts[1] = 0
		seen++
		return nil
	})
	if err != nil || seen != 2 {
		t.Fatalf("alias leak: %v", err)
	}
}

func TestPublisherCapacityAndIdentityBijection(t *testing.T) {
	rows := make([]Record, MaxPublishers+1)
	for i := range len(rows) {
		rows[i] = record(uint64(i), uint16(i+1), 100+uint64(i), 7)
	}
	result, err := Reduce(t.Context(), fixture(rows), []uint64{DayNanos + 100}, func(Snapshot) error { return nil })
	if !errors.Is(err, ErrCapacity) || result != (Summary{}) {
		t.Fatal("publisher bound not enforced")
	}
	source := fixture([]Record{record(0, 30, 100, 7)})
	source.Bindings = append(source.Bindings, Binding{RawSymbol: "TSLA  260422P00255000", InstrumentID: 1})
	result, err = Reduce(t.Context(), source, []uint64{DayNanos + 100}, func(Snapshot) error { return nil })
	if !errors.Is(err, ErrScope) || result != (Summary{}) {
		t.Fatal("duplicate mapped instrument accepted")
	}
}
