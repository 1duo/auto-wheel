package stockhistory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Bounded resource guards matching the existing 10k/16MiB pilot.
// These are local resource guards only, not billing or coverage proof.
const (
	MaxFileBytes = 16 << 20
	MaxLineBytes = 1 << 20
	MaxRecords   = 10000
)

// MINI MBP-1 wire constants for this bounded component.
const (
	RTypeMiniMBP1    = 1
	PublisherMiniMBP = 95
)

// UndefinedPriceRaw is the INT64_MAX sentinel for an undefined price.
// UndefinedPriceNano preserves it as int64 without guessing a value.
const UndefinedPriceRaw = "9223372036854775807"
const UndefinedPriceNano = int64(1<<63 - 1)

// MaxUint32 bounds wire uint32 fields without binary floating point.
const maxUint32 = int64(1<<32 - 1)

// Frozen fifteen-symbol universe for the Apr6-Oct2 study groundwork.
// Exact uppercase members only; order matches the acquisition scope.
var frozenUniverse = []string{
	"NFLX", "TSLA", "NVDA", "NBIS", "IREN", "NOW", "AMD", "AMZN",
	"GOOGL", "META", "MSFT", "PLTR", "UBER", "AVGO", "ORCL",
}

// Action is a MINI MBP-1 record action.
type Action string

const (
	ActionAdd    Action = "A"
	ActionChange Action = "C"
	ActionModify Action = "M"
	ActionReset  Action = "R"
	ActionTrade  Action = "T"
)

var validActions = []string{"A", "C", "M", "R", "T"}
var validSides = []string{"A", "B", "N"}

// Structural flags are allowed on intermediate and final event records.
// LAST closes an event; 192 is LAST+TOB, not SNAPSHOT.
const lastFlag = int64(128)

var supportedFlags = []int64{0, 16, 64, 80, 128, 144, 192, 208}

// Status labels a per-symbol observation. Absent symbols are simply absent
// from the Snapshot map and are never synthesized.
type Status string

const (
	StatusFresh   Status = "fresh"
	StatusStale   Status = "stale"
	StatusInvalid Status = "invalid"
)

// Invalid reasons emitted on StatusInvalid observations.
const (
	ReasonMissingPrice       = "missing_price"
	ReasonUndefinedPrice     = "undefined_price"
	ReasonNonpositiveBid     = "nonpositive_bid"
	ReasonNonpositiveAsk     = "nonpositive_ask"
	ReasonZeroSize           = "zero_size"
	ReasonCrossed            = "crossed"
	ReasonReset              = "reset"
	ReasonTradeBookAmbiguity = "trade_book_ambiguity"
	ReasonTieAmbiguity       = "tie_ambiguity"
	ReasonIncompleteEvent    = "incomplete_event"
)

var (
	ErrFileTooLarge     = errors.New("stockhistory: file too large")
	ErrLineTooLong      = errors.New("stockhistory: line too long")
	ErrTooManyRecords   = errors.New("stockhistory: too many records")
	ErrOutOfOrder       = errors.New("stockhistory: out-of-order capture")
	ErrInstrumentDrift  = errors.New("stockhistory: instrument drift")
	ErrUnqualifiedFlags = errors.New("stockhistory: unqualified flags")
	ErrInvalidRecord    = errors.New("stockhistory: invalid record")
	ErrIncompleteEvent  = errors.New("stockhistory: incomplete event")
)

// BookLevel is the parsed top-of-book (levels[0]).
type BookLevel struct {
	BidPxNano    int64
	AskPxNano    int64
	BidSz        int64
	AskSz        int64
	BidCt        int64
	AskCt        int64
	BidRaw       string
	AskRaw       string
	BidMissing   bool
	AskMissing   bool
	BidUndefined bool
	AskUndefined bool
	BidSzMissing bool
	AskSzMissing bool
}

// Event preserves actual capture time, original source event time, and
// immutable instrument identity. Book holds levels[0]; top-level
// price/size/side are retained for audit but do not define the book.
// Raw source clocks and exact integer nanoprices are preserved.
type Event struct {
	Symbol         string
	TsRecv         time.Time
	TsEvent        time.Time
	TsRecvRaw      string
	TsEventRaw     string
	TsRecvNano     int64
	TsEventNano    int64
	Rtype          int
	PublisherID    int64
	InstrumentID   int64
	Action         Action
	Side           string
	PriceNano      int64
	PriceRaw       string
	PriceUndefined bool
	Size           int64
	Flags          int64
	Depth          int64
	Sequence       int64
	TsInDelta      int64
	Book           BookLevel
}

// Observation is the per-symbol quote-availability state at asOf.
// At is the original quote capture; EventAt is its source engine timestamp.
// AvailableAt is the completed event's capture, never a quote-age substitute.
// TradeAt/TradeEventAt track the last included trade without refreshing At.
// Invalid observations carry zero prices/sizes and a reason; valid ones
// carry the book with Status fresh (age <= maxAge) or stale.
// TieCount counts completed events sharing a completion capture (1 otherwise).
// Equivalent books retain a real quote witness with the latest quote capture,
// keeping the first witness when quote captures also tie.
type Observation struct {
	Symbol        string
	InstrumentID  int64
	PublisherID   int64
	At            time.Time
	EventAt       time.Time
	AvailableAt   time.Time
	TradeAt       time.Time
	TradeEventAt  time.Time
	BidPxNano     int64
	AskPxNano     int64
	BidSz         int64
	AskSz         int64
	Status        Status
	InvalidReason string
	TieCount      int
	TieEquivalent bool
}

// Series is a small immutable ordered event set. Events are private;
// Events returns a clone for audit. Snapshot never mutates Series.
type Series struct {
	events []Event
}

// Len reports the number of stored events.
func (s Series) Len() int {
	return len(s.events)
}

// Events returns a cloned audit copy of stored events, never an alias.
// Mutating the result never affects the Series or later Snapshots.
func (s Series) Events() []Event {
	return slices.Clone(s.events)
}

// Raw wire types use pointers so omitted/null never silently becomes zero,
// except book price/size cells where nil explicitly means missing and
// invalidates the newest quote.
type rawMiniHeader struct {
	TsEvent      *string `json:"ts_event"`
	Rtype        *int64  `json:"rtype"`
	PublisherID  *int64  `json:"publisher_id"`
	InstrumentID *int64  `json:"instrument_id"`
}

type rawMiniLevel struct {
	BidPx *string `json:"bid_px"`
	AskPx *string `json:"ask_px"`
	BidSz *int64  `json:"bid_sz"`
	AskSz *int64  `json:"ask_sz"`
	BidCt *int64  `json:"bid_ct"`
	AskCt *int64  `json:"ask_ct"`
}

type rawMiniRecord struct {
	TsRecv    *string         `json:"ts_recv"`
	Hd        *rawMiniHeader  `json:"hd"`
	Action    *string         `json:"action"`
	Side      *string         `json:"side"`
	Depth     *int64          `json:"depth"`
	Price     *string         `json:"price"`
	Size      *int64          `json:"size"`
	Flags     *int64          `json:"flags"`
	TsInDelta *int64          `json:"ts_in_delta"`
	Sequence  *int64          `json:"sequence"`
	Levels    *[]rawMiniLevel `json:"levels"`
	Symbol    *string         `json:"symbol"`
}

// ReadMiniMBP parses one JSON object per line into an immutable Series in
// file order. It enforces MaxFileBytes, MaxLineBytes, MaxRecords,
// nondecreasing ts_recv, exact frozen namespace with stable one-to-one
// symbol/ID, ts_event <= ts_recv, known actions/sides, zero
// depth/sequence/delta/counts, exactly one level, and supported flags.
func ReadMiniMBP(r io.Reader) (Series, error) {
	if r == nil {
		return Series{}, fmt.Errorf("%w: nil reader", ErrInvalidRecord)
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxFileBytes+1))
	if err != nil {
		return Series{}, fmt.Errorf("%w: read failed", ErrInvalidRecord)
	}
	if len(data) > MaxFileBytes {
		return Series{}, ErrFileTooLarge
	}
	events := make([]Event, 0, 64)
	symToID := make(map[string]int64)
	idToSym := make(map[int64]string)
	var prev time.Time
	var havePrev bool
	lineNo := 0
	for raw := range bytes.SplitSeq(data, []byte{'\n'}) {
		line := bytes.TrimSuffix(raw, []byte{'\r'})
		if len(line) > MaxLineBytes {
			return Series{}, ErrLineTooLong
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		lineNo++
		if len(events)+1 > MaxRecords {
			return Series{}, ErrTooManyRecords
		}
		ev, err := parseMiniRecord(line, lineNo)
		if err != nil {
			return Series{}, err
		}
		if havePrev && ev.TsRecv.Before(prev) {
			return Series{}, fmt.Errorf("line %d: %w", lineNo, ErrOutOfOrder)
		}
		if id, ok := symToID[ev.Symbol]; ok && id != ev.InstrumentID {
			return Series{}, fmt.Errorf("line %d: %w", lineNo, ErrInstrumentDrift)
		}
		if sym, ok := idToSym[ev.InstrumentID]; ok && sym != ev.Symbol {
			return Series{}, fmt.Errorf("line %d: %w", lineNo, ErrInstrumentDrift)
		}
		symToID[ev.Symbol] = ev.InstrumentID
		idToSym[ev.InstrumentID] = ev.Symbol
		prev, havePrev = ev.TsRecv, true
		events = append(events, ev)
	}
	if err := validateMiniEvents(events); err != nil {
		return Series{}, err
	}
	open := make(map[string]bool)
	for _, ev := range events {
		open[ev.Symbol] = ev.Flags&lastFlag == 0
	}
	for _, pending := range open {
		if pending {
			return Series{}, ErrIncompleteEvent
		}
	}
	return Series{events: slices.Clone(events)}, nil
}

// Snapshot folds Series events into per-symbol observations as of asOf.
// Events with TsRecv after asOf are excluded from state but their
// schema/ordering/drift was already validated fail-closed at read and is
// revalidated here. maxAge bounds fresh vs stale; age is asOf-At with exact
// <= boundary fresh. The returned map holds observations only, never a
// coverage declaration or replay input. Snapshot never mutates Series or
// caller-visible data and is causal and repeatable.
func (s Series) Snapshot(asOf time.Time, maxAge time.Duration) (map[string]Observation, error) {
	if asOf.IsZero() {
		return nil, fmt.Errorf("%w: zero asOf", ErrInvalidRecord)
	}
	if maxAge <= 0 {
		return nil, fmt.Errorf("%w: nonpositive maxAge", ErrInvalidRecord)
	}
	if err := validateMiniEvents(s.events); err != nil {
		return nil, err
	}
	// Group included events per symbol in file order.
	perSymbol := make(map[string][]Event)
	for _, ev := range s.events {
		if ev.TsRecv.After(asOf) {
			continue
		}
		perSymbol[ev.Symbol] = append(perSymbol[ev.Symbol], ev)
	}
	out := make(map[string]Observation)
	for sym, list := range perSymbol {
		var cur *Observation
		var has bool
		var bundle []Event
		var completed [][]Event
		flush := func() {
			if len(completed) == 0 {
				return
			}
			cur, has = applyCompletedMini(cur, has, completed, asOf, maxAge)
			completed = nil
		}
		for _, ev := range list {
			bundle = append(bundle, ev)
			if ev.Flags&lastFlag == 0 {
				continue
			}
			if len(completed) > 0 {
				previousEnd := completed[len(completed)-1]
				if !ev.TsRecv.Equal(previousEnd[len(previousEnd)-1].TsRecv) {
					flush()
				}
			}
			completed = append(completed, bundle)
			bundle = nil
		}
		flush()
		if len(bundle) > 0 {
			// Only the known prefix is examined. A later future LAST record
			// cannot make an incomplete book eligible at this cutoff.
			ev := bundle[len(bundle)-1]
			var tradeAt, tradeEventAt time.Time
			if has && cur != nil {
				tradeAt, tradeEventAt = cur.TradeAt, cur.TradeEventAt
			}
			cur = new(Observation{Symbol: sym, InstrumentID: ev.InstrumentID,
				PublisherID: ev.PublisherID, At: ev.TsRecv, EventAt: ev.TsEvent,
				TradeAt: tradeAt, TradeEventAt: tradeEventAt,
				Status: StatusInvalid, InvalidReason: ReasonIncompleteEvent})
			has = true
		}
		if has && cur != nil {
			out[sym] = *cur
		}
	}
	return out, nil
}

// Fold one source event atomically in its retained record order. LAST is
// the availability clock, distinct from the original quote-update clock.
func foldMiniEvent(prev *Observation, has bool, bundle []Event, asOf time.Time, maxAge time.Duration) (*Observation, bool) {
	cur := prev
	for _, ev := range bundle {
		cur, has = applySingleMini(cur, has, ev, asOf, maxAge)
	}
	if has && cur != nil {
		copy := *cur
		copy.AvailableAt = bundle[len(bundle)-1].TsRecv
		for _, ev := range bundle {
			if ev.Action == ActionTrade {
				copy.TradeAt, copy.TradeEventAt = ev.TsRecv, ev.TsEvent
			}
		}
		cur = &copy
	}
	return cur, has
}

// Different completed events at the same capture have no qualified sequence.
// Equal quote outcomes plus trades that echo them commute; all other mixed
// outcomes stay ambiguous. Intermediate records inside one event are ordered
// by its completion marker and are not independent timestamp ties.
func applyCompletedMini(prev *Observation, has bool, bundles [][]Event, asOf time.Time, maxAge time.Duration) (*Observation, bool) {
	if len(bundles) == 1 {
		return foldMiniEvent(prev, has, bundles[0], asOf, maxAge)
	}
	var chosen *Observation
	var tradeOnly []Event
	var allTrades []Event
	ambiguous := false
	for _, bundle := range bundles {
		changesBook := false
		for _, ev := range bundle {
			if ev.Action != ActionTrade {
				changesBook = true
			} else {
				allTrades = append(allTrades, ev)
			}
		}
		if !changesBook {
			tradeOnly = append(tradeOnly, bundle...)
			continue
		}
		candidate, present := foldMiniEvent(prev, has, bundle, asOf, maxAge)
		if !present || candidate == nil || candidate.Status == StatusInvalid {
			ambiguous = true
			continue
		}
		if chosen != nil && !sameMiniBook(*chosen, *candidate) {
			ambiguous = true
		}
		if chosen == nil || candidate.At.After(chosen.At) {
			chosen = candidate
		}
	}
	if chosen == nil && !ambiguous {
		// Trade-only events never restore or refresh a quote, so their
		// eligibility effects commute even without a sequence.
		cur := prev
		for _, bundle := range bundles {
			cur, has = foldMiniEvent(cur, has, bundle, asOf, maxAge)
		}
		if has && cur != nil {
			copy := *cur
			copy.TieCount, copy.TieEquivalent = len(bundles), false
			cur = &copy
		}
		return cur, has
	}
	if chosen != nil {
		for _, ev := range tradeOnly {
			_, valid := classifyMiniBook(ev.Book)
			if !valid || ev.Book.BidPxNano != chosen.BidPxNano || ev.Book.AskPxNano != chosen.AskPxNano ||
				ev.Book.BidSz != chosen.BidSz || ev.Book.AskSz != chosen.AskSz {
				ambiguous = true
			}
		}
	}
	lastBundle := bundles[len(bundles)-1]
	last := lastBundle[len(lastBundle)-1]
	if ambiguous || chosen == nil {
		chosen = new(Observation{Symbol: last.Symbol, InstrumentID: last.InstrumentID,
			PublisherID: last.PublisherID, At: last.TsRecv, EventAt: last.TsEvent,
			AvailableAt: last.TsRecv, Status: StatusInvalid, InvalidReason: ReasonTieAmbiguity})
		if has && prev != nil {
			chosen.TradeAt, chosen.TradeEventAt = prev.TradeAt, prev.TradeEventAt
		}
	}
	chosen.TieCount = len(bundles)
	chosen.TieEquivalent = !ambiguous
	for _, ev := range allTrades {
		chosen.TradeAt, chosen.TradeEventAt = ev.TsRecv, ev.TsEvent
	}
	return chosen, true
}

func sameMiniBook(a, b Observation) bool {
	return a.BidPxNano == b.BidPxNano && a.AskPxNano == b.AskPxNano && a.BidSz == b.BidSz && a.AskSz == b.AskSz
}

func applySingleMini(prev *Observation, has bool, ev Event, asOf time.Time, maxAge time.Duration) (*Observation, bool) {
	switch ev.Action {
	case ActionTrade:
		if !has || prev == nil {
			return prev, false
		}
		next := *prev
		next.TradeAt = ev.TsRecv
		next.TradeEventAt = ev.TsEvent
		if prev.Status == StatusInvalid {
			return &next, true
		}
		_, valid := classifyMiniBook(ev.Book)
		if !valid {
			next.Status = StatusInvalid
			next.InvalidReason = ReasonTradeBookAmbiguity
			next.BidPxNano, next.AskPxNano, next.BidSz, next.AskSz = 0, 0, 0, 0
			return &next, true
		}
		if ev.Book.BidPxNano != prev.BidPxNano || ev.Book.AskPxNano != prev.AskPxNano || ev.Book.BidSz != prev.BidSz || ev.Book.AskSz != prev.AskSz {
			next.Status = StatusInvalid
			next.InvalidReason = ReasonTradeBookAmbiguity
			next.BidPxNano, next.AskPxNano, next.BidSz, next.AskSz = 0, 0, 0, 0
			return &next, true
		}
		return &next, true
	case ActionReset:
		var tradeAt, tradeEventAt time.Time
		if has && prev != nil {
			tradeAt, tradeEventAt = prev.TradeAt, prev.TradeEventAt
		}
		next := Observation{
			Symbol:        ev.Symbol,
			InstrumentID:  ev.InstrumentID,
			PublisherID:   ev.PublisherID,
			At:            ev.TsRecv,
			EventAt:       ev.TsEvent,
			TradeAt:       tradeAt,
			TradeEventAt:  tradeEventAt,
			Status:        StatusInvalid,
			InvalidReason: ReasonReset,
			TieCount:      1,
		}
		return &next, true
	case ActionAdd, ActionChange, ActionModify:
		var tradeAt, tradeEventAt time.Time
		if has && prev != nil {
			tradeAt, tradeEventAt = prev.TradeAt, prev.TradeEventAt
		}
		reason, valid := classifyMiniBook(ev.Book)
		if !valid {
			next := Observation{
				Symbol:        ev.Symbol,
				InstrumentID:  ev.InstrumentID,
				PublisherID:   ev.PublisherID,
				At:            ev.TsRecv,
				EventAt:       ev.TsEvent,
				TradeAt:       tradeAt,
				TradeEventAt:  tradeEventAt,
				Status:        StatusInvalid,
				InvalidReason: reason,
				TieCount:      1,
			}
			return &next, true
		}
		status := StatusFresh
		if asOf.Sub(ev.TsRecv) > maxAge {
			status = StatusStale
		}
		next := Observation{
			Symbol:       ev.Symbol,
			InstrumentID: ev.InstrumentID,
			PublisherID:  ev.PublisherID,
			At:           ev.TsRecv,
			EventAt:      ev.TsEvent,
			TradeAt:      tradeAt,
			TradeEventAt: tradeEventAt,
			BidPxNano:    ev.Book.BidPxNano,
			AskPxNano:    ev.Book.AskPxNano,
			BidSz:        ev.Book.BidSz,
			AskSz:        ev.Book.AskSz,
			Status:       status,
			TieCount:     1,
		}
		return &next, true
	default:
		return prev, has
	}
}

func validateMiniEvents(events []Event) error {
	symToID := make(map[string]int64)
	idToSym := make(map[int64]string)
	for i, ev := range events {
		if ev.TsRecv.IsZero() || ev.TsEvent.IsZero() {
			return fmt.Errorf("event %d: %w: missing timestamp", i, ErrInvalidRecord)
		}
		if ev.TsEvent.After(ev.TsRecv) {
			return fmt.Errorf("event %d: %w: ts_event after ts_recv", i, ErrInvalidRecord)
		}
		if ev.Rtype != RTypeMiniMBP1 {
			return fmt.Errorf("event %d: %w: bad rtype", i, ErrInvalidRecord)
		}
		if ev.PublisherID != PublisherMiniMBP {
			return fmt.Errorf("event %d: %w: bad publisher", i, ErrInvalidRecord)
		}
		if err := validateMiniSymbol(ev.Symbol); err != nil {
			return fmt.Errorf("event %d: %w", i, err)
		}
		if ev.InstrumentID <= 0 || ev.InstrumentID > maxUint32 {
			return fmt.Errorf("event %d: %w: instrument outside uint32", i, ErrInvalidRecord)
		}
		if !slices.Contains(validActions, string(ev.Action)) {
			return fmt.Errorf("event %d: %w: unknown action", i, ErrInvalidRecord)
		}
		if !slices.Contains(validSides, ev.Side) {
			return fmt.Errorf("event %d: %w: invalid side", i, ErrInvalidRecord)
		}
		if !slices.Contains(supportedFlags, ev.Flags) {
			return fmt.Errorf("event %d: %w: flags %d", i, ErrUnqualifiedFlags, ev.Flags)
		}
		if ev.Depth != 0 || ev.Sequence != 0 || ev.TsInDelta != 0 {
			return fmt.Errorf("event %d: %w: depth/sequence/delta must be zero", i, ErrInvalidRecord)
		}
		if ev.Book.BidCt != 0 || ev.Book.AskCt != 0 {
			return fmt.Errorf("event %d: %w: counts must be zero", i, ErrInvalidRecord)
		}
		if ev.Size < 0 || ev.Size > maxUint32 {
			return fmt.Errorf("event %d: %w: size outside uint32", i, ErrInvalidRecord)
		}
		if ev.Book.BidSzMissing || ev.Book.AskSzMissing {
			// Missing sizes are invalid books, not wire errors; bounds
			// already enforced for present sizes below.
		} else if ev.Book.BidSz < 0 || ev.Book.BidSz > maxUint32 || ev.Book.AskSz < 0 || ev.Book.AskSz > maxUint32 {
			return fmt.Errorf("event %d: %w: book size outside uint32", i, ErrInvalidRecord)
		}
		if ev.PriceRaw == "" && !ev.PriceUndefined {
			return fmt.Errorf("event %d: %w: missing price", i, ErrInvalidRecord)
		}
		nano, err := parseMiniPriceNano(ev.PriceRaw)
		if err != nil || nano != ev.PriceNano || ev.PriceUndefined != (nano == UndefinedPriceNano) {
			return fmt.Errorf("event %d: %w: inconsistent wire price", i, ErrInvalidRecord)
		}
		if !ev.PriceUndefined && ev.PriceNano <= 0 {
			return fmt.Errorf("event %d: %w: nonpositive top price", i, ErrInvalidRecord)
		}
		if !ev.Book.BidMissing {
			nano, err := parseMiniPriceNano(ev.Book.BidRaw)
			if err != nil || nano != ev.Book.BidPxNano || ev.Book.BidUndefined != (nano == UndefinedPriceNano) {
				return fmt.Errorf("event %d: %w: inconsistent bid_px", i, ErrInvalidRecord)
			}
		} else if ev.Book.BidRaw != "" || ev.Book.BidPxNano != 0 || ev.Book.BidUndefined {
			return fmt.Errorf("event %d: %w: inconsistent missing bid", i, ErrInvalidRecord)
		}
		if !ev.Book.AskMissing {
			nano, err := parseMiniPriceNano(ev.Book.AskRaw)
			if err != nil || nano != ev.Book.AskPxNano || ev.Book.AskUndefined != (nano == UndefinedPriceNano) {
				return fmt.Errorf("event %d: %w: inconsistent ask_px", i, ErrInvalidRecord)
			}
		} else if ev.Book.AskRaw != "" || ev.Book.AskPxNano != 0 || ev.Book.AskUndefined {
			return fmt.Errorf("event %d: %w: inconsistent missing ask", i, ErrInvalidRecord)
		}
		if ev.TsRecvNano <= 0 || ev.TsRecvNano >= UndefinedPriceNano || ev.TsEventNano <= 0 || ev.TsEventNano >= UndefinedPriceNano {
			return fmt.Errorf("event %d: %w: bad clock nanos", i, ErrInvalidRecord)
		}
		if ev.TsRecvRaw == "" || ev.TsEventRaw == "" {
			return fmt.Errorf("event %d: %w: missing raw clock", i, ErrInvalidRecord)
		}
		if i > 0 && ev.TsRecv.Before(events[i-1].TsRecv) {
			return fmt.Errorf("event %d: %w", i, ErrOutOfOrder)
		}
		if id, ok := symToID[ev.Symbol]; ok && id != ev.InstrumentID {
			return fmt.Errorf("event %d: %w", i, ErrInstrumentDrift)
		}
		if sym, ok := idToSym[ev.InstrumentID]; ok && sym != ev.Symbol {
			return fmt.Errorf("event %d: %w", i, ErrInstrumentDrift)
		}
		symToID[ev.Symbol] = ev.InstrumentID
		idToSym[ev.InstrumentID] = ev.Symbol
	}
	return nil
}

func parseMiniRecord(line []byte, lineNo int) (Event, error) {
	if err := uniqueMiniJSONKeys(line); err != nil {
		return Event{}, fmt.Errorf("line %d: %w: duplicate or malformed json", lineNo, ErrInvalidRecord)
	}
	var raw rawMiniRecord
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return Event{}, fmt.Errorf("line %d: %w: bad json", lineNo, ErrInvalidRecord)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return Event{}, fmt.Errorf("line %d: %w: trailing data", lineNo, ErrInvalidRecord)
	}
	if raw.TsRecv == nil || *raw.TsRecv == "" {
		return Event{}, fmt.Errorf("line %d: %w: missing ts_recv", lineNo, ErrInvalidRecord)
	}
	tsRecvRaw := *raw.TsRecv
	tsRecv, tsRecvNano, err := parseMiniWireTime(tsRecvRaw)
	if err != nil || tsRecv.IsZero() {
		return Event{}, fmt.Errorf("line %d: %w: bad ts_recv", lineNo, ErrInvalidRecord)
	}
	if raw.Hd == nil {
		return Event{}, fmt.Errorf("line %d: %w: missing hd", lineNo, ErrInvalidRecord)
	}
	if raw.Hd.TsEvent == nil || *raw.Hd.TsEvent == "" {
		return Event{}, fmt.Errorf("line %d: %w: missing ts_event", lineNo, ErrInvalidRecord)
	}
	tsEventRaw := *raw.Hd.TsEvent
	tsEvent, tsEventNano, err := parseMiniWireTime(tsEventRaw)
	if err != nil || tsEvent.IsZero() {
		return Event{}, fmt.Errorf("line %d: %w: bad ts_event", lineNo, ErrInvalidRecord)
	}
	if tsEvent.After(tsRecv) {
		return Event{}, fmt.Errorf("line %d: %w: ts_event after ts_recv", lineNo, ErrInvalidRecord)
	}
	if raw.Hd.Rtype == nil {
		return Event{}, fmt.Errorf("line %d: %w: missing rtype", lineNo, ErrInvalidRecord)
	}
	if *raw.Hd.Rtype != RTypeMiniMBP1 {
		return Event{}, fmt.Errorf("line %d: %w: bad rtype", lineNo, ErrInvalidRecord)
	}
	if raw.Hd.PublisherID == nil {
		return Event{}, fmt.Errorf("line %d: %w: missing publisher_id", lineNo, ErrInvalidRecord)
	}
	if *raw.Hd.PublisherID != PublisherMiniMBP {
		return Event{}, fmt.Errorf("line %d: %w: bad publisher_id", lineNo, ErrInvalidRecord)
	}
	if raw.Hd.InstrumentID == nil {
		return Event{}, fmt.Errorf("line %d: %w: missing instrument_id", lineNo, ErrInvalidRecord)
	}
	if *raw.Hd.InstrumentID <= 0 || *raw.Hd.InstrumentID > maxUint32 {
		return Event{}, fmt.Errorf("line %d: %w: bad instrument_id", lineNo, ErrInvalidRecord)
	}
	if raw.Action == nil || *raw.Action == "" {
		return Event{}, fmt.Errorf("line %d: %w: missing action", lineNo, ErrInvalidRecord)
	}
	if !slices.Contains(validActions, *raw.Action) {
		return Event{}, fmt.Errorf("line %d: %w: unknown action", lineNo, ErrInvalidRecord)
	}
	if raw.Side == nil || *raw.Side == "" {
		return Event{}, fmt.Errorf("line %d: %w: missing side", lineNo, ErrInvalidRecord)
	}
	if !slices.Contains(validSides, *raw.Side) {
		return Event{}, fmt.Errorf("line %d: %w: invalid side", lineNo, ErrInvalidRecord)
	}
	if raw.Depth == nil {
		return Event{}, fmt.Errorf("line %d: %w: missing depth", lineNo, ErrInvalidRecord)
	}
	if *raw.Depth != 0 {
		return Event{}, fmt.Errorf("line %d: %w: depth must be zero", lineNo, ErrInvalidRecord)
	}
	if raw.Price == nil {
		return Event{}, fmt.Errorf("line %d: %w: missing price", lineNo, ErrInvalidRecord)
	}
	priceRaw := *raw.Price
	if priceRaw == "" {
		return Event{}, fmt.Errorf("line %d: %w: empty price", lineNo, ErrInvalidRecord)
	}
	priceUndefined := priceRaw == UndefinedPriceRaw
	var priceNano int64
	if priceUndefined {
		priceNano = UndefinedPriceNano
	} else {
		nano, err := parseMiniPriceNano(priceRaw)
		if err != nil {
			return Event{}, fmt.Errorf("line %d: %w: bad price", lineNo, ErrInvalidRecord)
		}
		if nano <= 0 {
			return Event{}, fmt.Errorf("line %d: %w: nonpositive price", lineNo, ErrInvalidRecord)
		}
		priceNano = nano
	}
	if raw.Size == nil {
		return Event{}, fmt.Errorf("line %d: %w: missing size", lineNo, ErrInvalidRecord)
	}
	if *raw.Size < 0 || *raw.Size > maxUint32 {
		return Event{}, fmt.Errorf("line %d: %w: bad size", lineNo, ErrInvalidRecord)
	}
	if raw.Flags == nil {
		return Event{}, fmt.Errorf("line %d: %w: missing flags", lineNo, ErrInvalidRecord)
	}
	if !slices.Contains(supportedFlags, *raw.Flags) {
		return Event{}, fmt.Errorf("line %d: %w: flags %d", lineNo, ErrUnqualifiedFlags, *raw.Flags)
	}
	if raw.TsInDelta == nil {
		return Event{}, fmt.Errorf("line %d: %w: missing ts_in_delta", lineNo, ErrInvalidRecord)
	}
	if *raw.TsInDelta != 0 {
		return Event{}, fmt.Errorf("line %d: %w: ts_in_delta must be zero", lineNo, ErrInvalidRecord)
	}
	if raw.Sequence == nil {
		return Event{}, fmt.Errorf("line %d: %w: missing sequence", lineNo, ErrInvalidRecord)
	}
	if *raw.Sequence != 0 {
		return Event{}, fmt.Errorf("line %d: %w: sequence must be zero", lineNo, ErrInvalidRecord)
	}
	if raw.Levels == nil {
		return Event{}, fmt.Errorf("line %d: %w: missing levels", lineNo, ErrInvalidRecord)
	}
	if len(*raw.Levels) != 1 {
		return Event{}, fmt.Errorf("line %d: %w: levels must hold exactly one entry", lineNo, ErrInvalidRecord)
	}
	book, err := parseMiniLevel((*raw.Levels)[0], lineNo)
	if err != nil {
		return Event{}, err
	}
	if raw.Symbol == nil || *raw.Symbol == "" {
		return Event{}, fmt.Errorf("line %d: %w: missing symbol", lineNo, ErrInvalidRecord)
	}
	if err := validateMiniSymbol(*raw.Symbol); err != nil {
		return Event{}, fmt.Errorf("line %d: %w", lineNo, err)
	}
	return Event{
		Symbol:         *raw.Symbol,
		TsRecv:         tsRecv,
		TsEvent:        tsEvent,
		TsRecvRaw:      tsRecvRaw,
		TsEventRaw:     tsEventRaw,
		TsRecvNano:     tsRecvNano,
		TsEventNano:    tsEventNano,
		Rtype:          RTypeMiniMBP1,
		PublisherID:    *raw.Hd.PublisherID,
		InstrumentID:   *raw.Hd.InstrumentID,
		Action:         Action(*raw.Action),
		Side:           *raw.Side,
		PriceNano:      priceNano,
		PriceRaw:       priceRaw,
		PriceUndefined: priceUndefined,
		Size:           *raw.Size,
		Flags:          *raw.Flags,
		Depth:          *raw.Depth,
		Sequence:       *raw.Sequence,
		TsInDelta:      *raw.TsInDelta,
		Book:           book,
	}, nil
}

func parseMiniLevel(lv rawMiniLevel, lineNo int) (BookLevel, error) {
	var b BookLevel
	if lv.BidCt == nil {
		return BookLevel{}, fmt.Errorf("line %d: %w: missing bid_ct", lineNo, ErrInvalidRecord)
	}
	if lv.AskCt == nil {
		return BookLevel{}, fmt.Errorf("line %d: %w: missing ask_ct", lineNo, ErrInvalidRecord)
	}
	if *lv.BidCt != 0 || *lv.AskCt != 0 {
		return BookLevel{}, fmt.Errorf("line %d: %w: counts must be zero", lineNo, ErrInvalidRecord)
	}
	b.BidCt, b.AskCt = *lv.BidCt, *lv.AskCt
	if lv.BidPx == nil {
		b.BidMissing = true
	} else {
		raw := *lv.BidPx
		if raw == UndefinedPriceRaw {
			b.BidUndefined = true
			b.BidPxNano = UndefinedPriceNano
			b.BidRaw = raw
		} else {
			nano, err := parseMiniPriceNano(raw)
			if err != nil {
				return BookLevel{}, fmt.Errorf("line %d: %w: bad bid_px", lineNo, ErrInvalidRecord)
			}
			b.BidPxNano = nano
			b.BidRaw = raw
		}
	}
	if lv.AskPx == nil {
		b.AskMissing = true
	} else {
		raw := *lv.AskPx
		if raw == UndefinedPriceRaw {
			b.AskUndefined = true
			b.AskPxNano = UndefinedPriceNano
			b.AskRaw = raw
		} else {
			nano, err := parseMiniPriceNano(raw)
			if err != nil {
				return BookLevel{}, fmt.Errorf("line %d: %w: bad ask_px", lineNo, ErrInvalidRecord)
			}
			b.AskPxNano = nano
			b.AskRaw = raw
		}
	}
	if lv.BidSz == nil {
		b.BidSzMissing = true
	} else {
		if *lv.BidSz < 0 || *lv.BidSz > maxUint32 {
			return BookLevel{}, fmt.Errorf("line %d: %w: bad bid_sz", lineNo, ErrInvalidRecord)
		}
		b.BidSz = *lv.BidSz
	}
	if lv.AskSz == nil {
		b.AskSzMissing = true
	} else {
		if *lv.AskSz < 0 || *lv.AskSz > maxUint32 {
			return BookLevel{}, fmt.Errorf("line %d: %w: bad ask_sz", lineNo, ErrInvalidRecord)
		}
		b.AskSz = *lv.AskSz
	}
	return b, nil
}

// classifyMiniBook reports validity for a quote update. Missing and
// sentinel prices, nonpositive quotes, zero or missing sizes, and crossed
// books invalidate; locked (bid==ask) books stay valid.
func classifyMiniBook(b BookLevel) (string, bool) {
	if b.BidMissing || b.AskMissing || b.BidSzMissing || b.AskSzMissing {
		if b.BidSzMissing || b.AskSzMissing {
			return ReasonZeroSize, false
		}
		return ReasonMissingPrice, false
	}
	if b.BidUndefined || b.AskUndefined {
		return ReasonUndefinedPrice, false
	}
	if b.BidPxNano <= 0 {
		return ReasonNonpositiveBid, false
	}
	if b.AskPxNano <= 0 {
		return ReasonNonpositiveAsk, false
	}
	if b.BidSz == 0 || b.AskSz == 0 {
		return ReasonZeroSize, false
	}
	if b.BidPxNano > b.AskPxNano {
		return ReasonCrossed, false
	}
	return "", true
}

func validateMiniSymbol(s string) error {
	if !slices.Contains(frozenUniverse, s) {
		return fmt.Errorf("%w: bad symbol %q", ErrInvalidRecord, s)
	}
	return nil
}

// uniqueMiniJSONKeys rejects repeated object keys before decoding the wire
// shape, including case variants that struct decoding would merge.
func uniqueMiniJSONKeys(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 32 {
			return errors.New("json nesting too deep")
		}
		tok, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || name != strings.ToLower(name) || seen[name] {
					return errors.New("duplicate or noncanonical object key")
				}
				// encoding/json also folds some Unicode letters onto ASCII
				// field names (for example long s). Wire keys are ASCII.
				for _, letter := range name {
					if letter > 127 {
						return errors.New("non-ASCII object key")
					}
				}
				seen[name] = true
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing json")
	}
	return nil
}

// parseMiniPriceNano reads the provider's pretty_px=false integer
// nanodollars. Scaling an integer wire value as dollar text would corrupt
// every price.
func parseMiniPriceNano(s string) (int64, error) {
	if s == "" || strings.TrimSpace(s) != s || strings.HasPrefix(s, "+") {
		return 0, errors.New("invalid integer price")
	}
	return strconv.ParseInt(s, 10, 64)
}

// parseMiniWireTime reads pretty_ts=false nanosecond strings, rejecting the
// undefined INT64_MAX sentinel, zero, negatives, and values outside int64
// time. It preserves the raw string and returns both time and nanos.
func parseMiniWireTime(s string) (time.Time, int64, error) {
	if s == "" {
		return time.Time{}, 0, errors.New("missing integer timestamp")
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return time.Time{}, 0, errors.New("invalid integer timestamp")
		}
	}
	ns, err := strconv.ParseInt(s, 10, 64)
	if err != nil || ns <= 0 || ns >= UndefinedPriceNano {
		return time.Time{}, 0, errors.New("invalid integer timestamp")
	}
	return time.Unix(0, ns).UTC(), ns, nil
}
