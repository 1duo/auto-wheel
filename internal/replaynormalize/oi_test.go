package replaynormalize

import (
	"errors"
	"math"
	"testing"

	"autowheel/internal/causaloi"
)

func oiFixture() (causaloi.Snapshot, causaloi.Observation, OIBinding) {
	const start = uint64(1_775_433_600_000_000_000)
	cut := start + 60_000_000_017
	r := causaloi.Record{Index: 8, NativeVersion: 1, RawSymbol: "SYNTH 260410P00100000", PublisherID: 30, InstrumentID: 7,
		ReceiveAt: cut, EventAt: cut - 99, ReferenceAt: math.MaxUint64, Quantity: 0, StatType: 9, UpdateAction: 1, Rtype: 24}
	s := causaloi.Snapshot{UTCStart: start, Through: cut, SourceSHA256: [32]byte{1}}
	o := causaloi.Observation{RawSymbol: r.RawSymbol, Status: causaloi.ReferenceMissing, HasRecord: true, Record: r,
		Quantity: new(r.Quantity), Copies: 18}
	return s, o, OIBinding{RawSymbol: r.RawSymbol, InstrumentID: r.InstrumentID, UTCStart: start, SourceSHA256: s.SourceSHA256}
}

func TestOpenInterestPreservesZeroAndLiteralClocks(t *testing.T) {
	s, o, b := oiFixture()
	v, err := OpenInterest(s, o, b)
	if err != nil || v.Quantity != 0 || v.EconomicReferenceAt != nil || v.Original != o.Record ||
		uint64(v.PublishedAt.UnixNano()) != o.Record.ReceiveAt || v.PublishedAt.Location().String() != "UTC" ||
		v.Policy != CapturedOIPublicationV1 || v.Copies != 18 || v.SourceSHA256 != s.SourceSHA256 || v.ObservedAt != s.Through {
		t.Fatal("observed zero, inclusive cut or original provenance lost", err)
	}
	// A later cut must not refresh publication or invent a reference date.
	s.Through += 10_000_000_000
	o.Record.Quantity = 11
	o.Quantity = new(int32(11))
	v, err = OpenInterest(s, o, b)
	if err != nil || v.Quantity != 11 || uint64(v.PublishedAt.UnixNano()) != o.Record.ReceiveAt || v.EconomicReferenceAt != nil {
		t.Fatal("quantity summed, publication refreshed or reference invented", err)
	}
	o.Status = causaloi.ReferencePresent
	o.Record.ReferenceAt = s.UTCStart - 1 // A literal earlier economic date is preserved.
	o.ReferenceAt = new(o.Record.ReferenceAt)
	v, err = OpenInterest(s, o, b)
	if err != nil || v.EconomicReferenceAt == nil || uint64(v.EconomicReferenceAt.UnixNano()) != o.Record.ReferenceAt {
		t.Fatal("known economic clock lost", err)
	}
	*o.ReferenceAt = 0
	*o.Quantity = 99
	if uint64(v.EconomicReferenceAt.UnixNano()) != v.Original.ReferenceAt || v.Quantity != 11 {
		t.Fatal("caller pointer mutation altered converted value")
	}
}

func TestOpenInterestRejectsUnknownAndMalformedInputs(t *testing.T) {
	cases := []struct {
		name string
		edit func(*causaloi.Snapshot, *causaloi.Observation, *OIBinding)
		err  error
	}{
		{"hash_zero", func(s *causaloi.Snapshot, _ *causaloi.Observation, _ *OIBinding) { s.SourceSHA256 = [32]byte{} }, ErrBinding},
		{"hash_mismatch", func(_ *causaloi.Snapshot, _ *causaloi.Observation, b *OIBinding) { b.SourceSHA256[0]++ }, ErrBinding},
		{"zero_day", func(s *causaloi.Snapshot, _ *causaloi.Observation, _ *OIBinding) { s.UTCStart = 0 }, ErrBinding},
		{"unaligned_day", func(s *causaloi.Snapshot, _ *causaloi.Observation, _ *OIBinding) { s.UTCStart++ }, ErrBinding},
		{"overflow_day", func(s *causaloi.Snapshot, _ *causaloi.Observation, _ *OIBinding) {
			s.UTCStart = math.MaxUint64 / causaloi.DayNanos * causaloi.DayNanos
		}, ErrBinding},
		{"other_day", func(_ *causaloi.Snapshot, _ *causaloi.Observation, b *OIBinding) { b.UTCStart += causaloi.DayNanos }, ErrBinding},
		{"overflow_cut", func(s *causaloi.Snapshot, _ *causaloi.Observation, _ *OIBinding) { s.Through = math.MaxUint64 }, ErrBinding},
		{"next_day_cut", func(s *causaloi.Snapshot, _ *causaloi.Observation, _ *OIBinding) {
			s.Through = s.UTCStart + causaloi.DayNanos
		}, ErrBinding},
		{"before_day_cut", func(s *causaloi.Snapshot, _ *causaloi.Observation, _ *OIBinding) { s.Through = s.UTCStart - 1 }, ErrBinding},
		{"malformed_symbol", func(_ *causaloi.Snapshot, _ *causaloi.Observation, b *OIBinding) {
			b.RawSymbol = "SYNTH260410P00100000"
		}, ErrBinding},
		{"observation_symbol", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) {
			o.RawSymbol = "OTHER 260410P00100000"
		}, ErrBinding},
		{"original_symbol", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) {
			o.Record.RawSymbol = "OTHER 260410P00100000"
		}, ErrBinding},
		{"identity", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.Record.InstrumentID++ }, ErrBinding},
		{"zero_identity", func(_ *causaloi.Snapshot, _ *causaloi.Observation, b *OIBinding) { b.InstrumentID = 0 }, ErrBinding},
		{"not_observed", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.Status = causaloi.NotObserved }, ErrOI},
		{"conflict", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.Status = causaloi.CopyConflict }, ErrOI},
		{"unsupported", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.Status = causaloi.UnsupportedCopy }, ErrOI},
		{"unknown_definition", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) {
			o.Status = causaloi.DefinitionUnknown
		}, ErrOI},
		{"missing_record", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.HasRecord = false }, ErrOI},
		{"missing_quantity", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.Quantity = nil }, ErrOI},
		{"negative", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) {
			o.Record.Quantity = -1
			o.Quantity = new(int32(-1))
		}, ErrOI},
		{"sentinel", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) {
			o.Record.Quantity = math.MaxInt32
			o.Quantity = new(int32(math.MaxInt32))
		}, ErrOI},
		{"quantity_disagreement", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.Quantity = new(int32(1)) }, ErrOI},
		{"no_copies", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.Copies = 0 }, ErrOI},
		{"many_copies", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.Copies = 65 }, ErrOI},
		{"native_version", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.Record.NativeVersion = 3 }, ErrOI},
		{"rtype", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.Record.Rtype = 1 }, ErrOI},
		{"stat_type", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.Record.StatType = 1 }, ErrOI},
		{"publisher", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.Record.PublisherID = 0 }, ErrOI},
		{"index_bound", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) {
			o.Record.Index = causaloi.MaxRecords
		}, ErrOI},
		{"delete", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.Record.UpdateAction = 2 }, ErrOI},
		{"flags", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.Record.Flags = 1 }, ErrOI},
		{"send_delta", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.Record.SendDelta = -1 }, ErrOI},
		{"future_receive", func(s *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.Record.ReceiveAt = s.Through + 1 }, ErrClock},
		{"zero_receive", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.Record.ReceiveAt = 0 }, ErrClock},
		{"overflow_receive", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.Record.ReceiveAt = math.MaxUint64 }, ErrClock},
		{"before_day_receive", func(s *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.Record.ReceiveAt = s.UTCStart - 1 }, ErrClock},
		{"zero_event", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.Record.EventAt = 0 }, ErrClock},
		{"future_event", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) {
			o.Record.EventAt = o.Record.ReceiveAt + 1
		}, ErrClock},
		{"overflow_event", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.Record.EventAt = math.MaxUint64 }, ErrClock},
		{"missing_ref_shape", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.Record.ReferenceAt = 1 }, ErrOI},
		{"missing_ref_pointer", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) { o.ReferenceAt = new(uint64(1)) }, ErrOI},
		{"known_ref_pointer_absent", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) {
			o.Status = causaloi.ReferencePresent
			o.Record.ReferenceAt = 1
		}, ErrOI},
		{"known_ref_pointer_disagrees", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) {
			o.Status = causaloi.ReferencePresent
			o.Record.ReferenceAt = 1
			o.ReferenceAt = new(uint64(2))
		}, ErrOI},
		{"known_ref_zero", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) {
			o.Status = causaloi.ReferencePresent
			o.Record.ReferenceAt = 0
			o.ReferenceAt = new(uint64(0))
		}, ErrClock},
		{"known_ref_future", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) {
			o.Status = causaloi.ReferencePresent
			o.Record.ReferenceAt = o.Record.ReceiveAt + 1
			o.ReferenceAt = new(o.Record.ReferenceAt)
		}, ErrClock},
		{"known_ref_sentinel", func(_ *causaloi.Snapshot, o *causaloi.Observation, _ *OIBinding) {
			o.Status = causaloi.ReferencePresent
			o.ReferenceAt = new(uint64(math.MaxUint64))
		}, ErrClock},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, o, b := oiFixture()
			tc.edit(&s, &o, &b)
			if value, err := OpenInterest(s, o, b); !errors.Is(err, tc.err) || value != (OIValue{}) {
				t.Fatal("invalid input admitted or partial value escaped", err)
			}
		})
	}
}
