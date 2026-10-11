// Package causaloi folds audited native V1 statistics into provisional OI
// observations. It does not qualify a roster, contract terms, replay or trading.
package causaloi

import (
	"cmp"
	"context"
	"errors"
	"io"
	"math"
	"slices"
)

const (
	DayNanos      uint64 = 86_400_000_000_000
	MaxSymbols           = 2000
	MaxPublishers        = 64
	MaxCuts              = 86400
	MaxRecords           = 10_000_000
)

var (
	ErrScope    = errors.New("causaloi: invalid attested scope")
	ErrRecord   = errors.New("causaloi: invalid native record")
	ErrIdentity = errors.New("causaloi: identity changed or outside scope")
	ErrCapture  = errors.New("causaloi: capture order or window invalid")
	ErrSource   = errors.New("causaloi: source count or completion invalid")
	ErrCapacity = errors.New("causaloi: publisher capacity exceeded")
)

// Record retains all seventeen fields of the AS_IS StatMsgV1 vector. Quantity
// is signed int32; its undefined sentinel is different from the JSON pilot.
type Record struct {
	Index         uint64
	NativeVersion uint8
	RawSymbol     string
	PublisherID   uint16
	InstrumentID  uint32
	ReceiveAt     uint64
	EventAt       uint64
	ReferenceAt   uint64
	Price         int64
	Quantity      int32
	Sequence      uint32
	SendDelta     int32
	StatType      uint16
	ChannelID     uint16
	UpdateAction  uint8
	Flags         uint8
	Rtype         uint8
}

type Definition struct {
	InstrumentID uint32
	ReceiveAt    uint64
	EventAt      uint64
}

// Binding supplies the daily mapping and an optional captured definition.
// Neither a mapping nor a definition by itself establishes tradability/terms.
// A missing mapping/definition is preserved, never removed from the universe.
type Binding struct {
	RawSymbol    string
	InstrumentID uint32
	Definition   *Definition
}

// Source is one attested UTC-day vector. Next must return the literal io.EOF
// with a zero Record after every original row. Hash/count/acquisition/native
// EOF and before/after stability still need verification by the outer wrapper.
type Source struct {
	UTCStart        uint64
	NativeVersion   uint8
	ExpectedRecords uint64
	SourceSHA256    [32]byte
	Bindings        []Binding
	Next            func(context.Context) (Record, error)
}

type Status string

const (
	NotObserved       Status = "not_observed"
	UnsupportedCopy   Status = "unsupported_latest_copy"
	CopyConflict      Status = "publisher_copy_conflict"
	DefinitionUnknown Status = "definition_join_unknown"
	ReferenceMissing  Status = "observed_reference_missing"
	ReferencePresent  Status = "observed_reference_present"
)

// Observation keeps the chosen original row even when its quantity is withheld.
// Nil Quantity is unknown; a non-nil zero is an observed zero. Nil ReferenceAt
// does not invent a reference/reporting date. All fields remain provisional.
type Observation struct {
	RawSymbol   string
	Status      Status
	HasRecord   bool
	Record      Record
	Quantity    *int32
	ReferenceAt *uint64
	Copies      uint16
}

type Snapshot struct {
	UTCStart     uint64
	Through      uint64
	SourceSHA256 [32]byte
	Observations []Observation
}

type Summary struct {
	Records             uint64
	OpenInterestRecords uint64
	Snapshots           uint64
	Observations        uint64
}

type state struct {
	binding    Binding
	instrument uint32
	copies     map[uint16]Record
}

func validSymbol(raw string) bool {
	if len(raw) != 21 || raw[0] == ' ' || raw[12] != 'C' && raw[12] != 'P' {
		return false
	}
	padding := false
	for _, c := range raw[:6] {
		if c == ' ' {
			padding = true
		} else if padding || !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	for i := 6; i < len(raw); i++ {
		if i != 12 && (raw[i] < '0' || raw[i] > '9') {
			return false
		}
	}
	return true
}

func supported(r Record) bool {
	return r.UpdateAction == 1 && r.Flags == 0 && r.Quantity >= 0 && r.Quantity < math.MaxInt32 &&
		r.EventAt > 0 && r.EventAt <= r.ReceiveAt &&
		(r.ReferenceAt == math.MaxUint64 || r.ReferenceAt > 0 && r.ReferenceAt <= r.ReceiveAt)
}

func (s *state) observe(cut uint64) Observation {
	out := Observation{RawSymbol: s.binding.RawSymbol, Status: NotObserved, Copies: uint16(len(s.copies))}
	if len(s.copies) == 0 {
		return out
	}
	bad, conflict, first := false, false, true
	var quantity int32
	var instrument uint32
	for _, r := range s.copies {
		if !out.HasRecord || r.ReceiveAt > out.Record.ReceiveAt ||
			r.ReceiveAt == out.Record.ReceiveAt && r.Index > out.Record.Index {
			out.Record, out.HasRecord = r, true
		}
		bad = bad || !supported(r)
		if first {
			quantity, instrument, first = r.Quantity, r.InstrumentID, false
		} else if r.Quantity != quantity || r.InstrumentID != instrument {
			conflict = true
		}
	}
	d := s.binding.Definition
	switch {
	case bad:
		out.Status = UnsupportedCopy
	case conflict:
		out.Status = CopyConflict
	case d == nil || d.InstrumentID == 0 || d.InstrumentID != instrument ||
		d.EventAt == 0 || d.EventAt > d.ReceiveAt || d.ReceiveAt > cut:
		out.Status = DefinitionUnknown
	default:
		out.Quantity = new(quantity)
		out.Status = ReferenceMissing
		if out.Record.ReferenceAt != math.MaxUint64 {
			out.ReferenceAt = new(out.Record.ReferenceAt)
			out.Status = ReferencePresent
		}
	}
	return out
}

// Reduce uses bounded state, original source order and CLOSED receive-time cuts.
// ALL equal-time rows are included before a cut is emitted. Non-OI rows count
// toward integrity/order but never affect OI. Latest unsupported/deleted copies
// supersede previous values. Every callback is provisional until Reduce AND all
// outer source checks pass. On ANY error the returned Summary is zero, and the
// caller must discard staged output. Callback mutations cannot alter state.
func Reduce(ctx context.Context, source Source, cuts []uint64, emit func(Snapshot) error) (Summary, error) {
	if ctx == nil || source.Next == nil || emit == nil || source.UTCStart == 0 ||
		source.UTCStart%DayNanos != 0 || source.UTCStart > math.MaxUint64-DayNanos ||
		(source.NativeVersion != 1 && source.NativeVersion != 2) ||
		source.ExpectedRecords == 0 || source.ExpectedRecords > MaxRecords ||
		source.SourceSHA256 == ([32]byte{}) || len(source.Bindings) == 0 || len(source.Bindings) > MaxSymbols ||
		len(cuts) == 0 || len(cuts) > MaxCuts {
		return Summary{}, ErrScope
	}
	want := slices.Clone(cuts)
	end := source.UTCStart + DayNanos
	for i, cut := range want {
		if cut < source.UTCStart || cut >= end || i > 0 && cut <= want[i-1] {
			return Summary{}, ErrScope
		}
	}
	bindings := slices.Clone(source.Bindings)
	slices.SortFunc(bindings, func(a, b Binding) int { return cmp.Compare(a.RawSymbol, b.RawSymbol) })
	states := make(map[string]*state, len(bindings))
	reverse := make(map[uint32]string, len(bindings))
	for i, binding := range bindings {
		if !validSymbol(binding.RawSymbol) || i > 0 && binding.RawSymbol == bindings[i-1].RawSymbol {
			return Summary{}, ErrScope
		}
		if binding.Definition != nil {
			binding.Definition = new(*binding.Definition)
		}
		if binding.InstrumentID != 0 {
			if _, exists := reverse[binding.InstrumentID]; exists {
				return Summary{}, ErrScope
			}
			reverse[binding.InstrumentID] = binding.RawSymbol
		}
		states[binding.RawSymbol] = &state{binding: binding, instrument: binding.InstrumentID, copies: make(map[uint16]Record)}
	}
	var result Summary
	cutIndex := 0
	emitCut := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		cut := want[cutIndex]
		observations := make([]Observation, 0, len(bindings))
		for _, b := range bindings {
			observations = append(observations, states[b.RawSymbol].observe(cut))
		}
		if err := emit(Snapshot{UTCStart: source.UTCStart, Through: cut,
			SourceSHA256: source.SourceSHA256, Observations: observations}); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		result.Snapshots++
		result.Observations += uint64(len(observations))
		cutIndex++
		return nil
	}
	var previous uint64
	for {
		if err := ctx.Err(); err != nil {
			return Summary{}, err
		}
		r, err := source.Next(ctx)
		if err != nil {
			// Completion is an exact protocol sentinel, not a wrapped-error match.
			if err != io.EOF || r != (Record{}) || result.Records != source.ExpectedRecords {
				return Summary{}, errors.Join(ErrSource, err)
			}
			break
		}
		if result.Records >= source.ExpectedRecords || r.Index != result.Records ||
			r.NativeVersion != source.NativeVersion || r.Rtype != 24 || r.PublisherID == 0 || r.InstrumentID == 0 {
			return Summary{}, ErrRecord
		}
		if r.ReceiveAt < source.UTCStart || r.ReceiveAt >= end || result.Records > 0 && r.ReceiveAt < previous {
			return Summary{}, ErrCapture
		}
		s := states[r.RawSymbol]
		owner, mapped := reverse[r.InstrumentID]
		if s == nil || s.instrument != 0 && s.instrument != r.InstrumentID || mapped && owner != r.RawSymbol {
			return Summary{}, ErrIdentity
		}
		reverse[r.InstrumentID] = r.RawSymbol
		s.instrument = r.InstrumentID
		for cutIndex < len(want) && r.ReceiveAt > want[cutIndex] {
			if err := emitCut(); err != nil {
				return Summary{}, err
			}
		}
		if r.StatType == 9 {
			if _, exists := s.copies[r.PublisherID]; !exists && len(s.copies) >= MaxPublishers {
				return Summary{}, ErrCapacity
			}
			s.copies[r.PublisherID] = r
			result.OpenInterestRecords++
		}
		previous = r.ReceiveAt
		result.Records++
	}
	for cutIndex < len(want) {
		if err := emitCut(); err != nil {
			return Summary{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}
	return result, nil
}
