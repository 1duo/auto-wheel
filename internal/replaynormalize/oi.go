package replaynormalize

import (
	"errors"
	"math"
	"time"

	"autowheel/internal/causaloi"
)

// CapturedOIPublicationV1 uses the literal capture receive time as an
// availability bound. It does not assert earliest publication or broker receipt.
const CapturedOIPublicationV1 = "captured_OI_receive_availability_v1"

var ErrOI = errors.New("replaynormalize: reported open interest unavailable or invalid")

type OIBinding struct {
	RawSymbol    string
	InstrumentID uint32
	UTCStart     uint64
	SourceSHA256 [32]byte
}

// OIValue is a candidate, not a complete Quote or a source qualification.
// A nil economic reference is unknown; an observed Quantity zero is real zero.
type OIValue struct {
	RawSymbol           string
	Quantity            int
	PublishedAt         time.Time
	EconomicReferenceAt *time.Time
	InstrumentID        uint32
	SourceSHA256        [32]byte
	Policy              string
	ObservedAt          uint64
	Original            causaloi.Record
	Copies              uint16
}

func validOISymbol(raw string) bool {
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
	for i := range len(raw) - 6 {
		at := i + 6
		if at != 12 && (raw[at] < '0' || raw[at] > '9') {
			return false
		}
	}
	return true
}

// OpenInterest converts one separately attested CLOSED observation. Its caller
// must still prove complete acquisition, native EOF/hash, publisher agreement,
// daily mapping, definitions and cut membership. A hash or label alone proves
// none of those facts. Every rejection returns a zero value, never a substitute.
func OpenInterest(snapshot causaloi.Snapshot, observation causaloi.Observation, binding OIBinding) (OIValue, error) {
	r := observation.Record
	if snapshot.UTCStart == 0 || snapshot.UTCStart%causaloi.DayNanos != 0 ||
		snapshot.UTCStart > math.MaxInt64-causaloi.DayNanos || binding.UTCStart != snapshot.UTCStart ||
		snapshot.Through < snapshot.UTCStart || snapshot.Through >= snapshot.UTCStart+causaloi.DayNanos ||
		snapshot.SourceSHA256 == ([32]byte{}) || binding.SourceSHA256 != snapshot.SourceSHA256 ||
		!validOISymbol(binding.RawSymbol) || binding.RawSymbol != observation.RawSymbol ||
		binding.InstrumentID == 0 || r.InstrumentID != binding.InstrumentID || r.RawSymbol != binding.RawSymbol {
		return OIValue{}, ErrBinding
	}
	if !observation.HasRecord || observation.Quantity == nil || observation.Copies == 0 || observation.Copies > causaloi.MaxPublishers ||
		(observation.Status != causaloi.ReferenceMissing && observation.Status != causaloi.ReferencePresent) ||
		(r.NativeVersion != 1 && r.NativeVersion != 2) || r.Rtype != 24 || r.StatType != 9 || r.PublisherID == 0 || r.Index >= causaloi.MaxRecords ||
		r.UpdateAction != 1 || r.Flags != 0 || r.SendDelta != 0 || r.Quantity < 0 || r.Quantity == math.MaxInt32 || *observation.Quantity != r.Quantity {
		return OIValue{}, ErrOI
	}
	if r.ReceiveAt < snapshot.UTCStart || r.ReceiveAt > snapshot.Through || r.EventAt == 0 || r.EventAt > r.ReceiveAt {
		return OIValue{}, ErrClock
	}
	var reference *time.Time
	if observation.Status == causaloi.ReferenceMissing {
		if r.ReferenceAt != math.MaxUint64 || observation.ReferenceAt != nil {
			return OIValue{}, ErrOI
		}
	} else {
		if observation.ReferenceAt == nil || *observation.ReferenceAt != r.ReferenceAt {
			return OIValue{}, ErrOI
		}
		if r.ReferenceAt == 0 || r.ReferenceAt > r.ReceiveAt {
			return OIValue{}, ErrClock
		}
		reference = new(time.Unix(0, int64(r.ReferenceAt)).UTC())
	}
	return OIValue{RawSymbol: binding.RawSymbol, Quantity: int(r.Quantity), PublishedAt: time.Unix(0, int64(r.ReceiveAt)).UTC(),
		EconomicReferenceAt: reference, InstrumentID: r.InstrumentID, SourceSHA256: binding.SourceSHA256,
		Policy: CapturedOIPublicationV1, ObservedAt: snapshot.Through, Original: r, Copies: observation.Copies}, nil
}
