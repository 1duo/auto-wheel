package quotehistory

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

// Bounded limits matching the existing 10k/16MiB pilot. These are local
// resource guards only, not billing or coverage proof.
const (
	MaxEvents    = 10000
	MaxFileBytes = 16 << 20
	MaxLineBytes = 1 << 20
)

// Wire constants for this bounded component.
const (
	RTypeCMBP1     = 177
	SupportedFlags = 192
	SymbolLen      = 21
)

// UndefinedPriceRaw is the INT64_MAX sentinel for an undefined price.
// UndefinedPriceNano preserves it as int64 without guessing a value.
const UndefinedPriceRaw = "9223372036854775807"
const UndefinedPriceNano = int64(1<<63 - 1)

// Action is a CMBP-1 record action.
type Action string

const (
	ActionAdd    Action = "A"
	ActionTrade  Action = "T"
	ActionChange Action = "C"
	ActionModify Action = "M"
	ActionReset  Action = "R"
)

var validActions = []string{"A", "T", "C", "M", "R"}

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
	ReasonMissingPrice   = "missing_price"
	ReasonUndefinedPrice = "undefined_price"
	ReasonNonpositiveBid = "nonpositive_bid"
	ReasonNonpositiveAsk = "nonpositive_ask"
	ReasonZeroSize       = "zero_size"
	ReasonCrossed        = "crossed"
	ReasonReset          = "reset"
)

var (
	ErrFileTooLarge     = errors.New("quotehistory: file too large")
	ErrLineTooLong      = errors.New("quotehistory: line too long")
	ErrTooManyRecords   = errors.New("quotehistory: too many records")
	ErrOutOfOrder       = errors.New("quotehistory: out-of-order capture")
	ErrInstrumentDrift  = errors.New("quotehistory: instrument drift")
	ErrUnqualifiedFlags = errors.New("quotehistory: unqualified flags")
	ErrInvalidRecord    = errors.New("quotehistory: invalid record")
)

// BookLevel is the parsed top-of-book (levels[0]).
type BookLevel struct {
	BidPxNano    int64
	AskPxNano    int64
	BidSz        int64
	AskSz        int64
	BidPb        int64
	AskPb        int64
	BidRaw       string
	AskRaw       string
	BidMissing   bool
	AskMissing   bool
	BidUndefined bool
	AskUndefined bool
}

// Event preserves actual capture time, original source event time, and immutable
// instrument identity. Book holds levels[0]; top-level price/size/side are
// retained for audit but do not define the book.
type Event struct {
	Symbol         string
	TsRecv         time.Time
	TsEvent        time.Time
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
	TsInDelta      int64
	Book           BookLevel
}

// Observation is the per-symbol quote-availability state at asOf.
// At is quote capture-availability; EventAt is the OPRA consolidator timestamp.
// TradeAt/TradeEventAt track the last included trade without refreshing At.
// Invalid observations carry zero prices/sizes and a reason; valid ones
// carry the book with Status fresh (age <= maxAge) or stale.
type Observation struct {
	Symbol        string
	InstrumentID  int64
	PublisherID   int64
	At            time.Time
	EventAt       time.Time
	TradeAt       time.Time
	TradeEventAt  time.Time
	BidPxNano     int64
	AskPxNano     int64
	BidSz         int64
	AskSz         int64
	Status        Status
	InvalidReason string
}

// raw wire types use pointers so omitted/null never silently becomes zero.
type rawHeader struct {
	TsEvent      *string `json:"ts_event"`
	Rtype        *int64  `json:"rtype"`
	PublisherID  *int64  `json:"publisher_id"`
	InstrumentID *int64  `json:"instrument_id"`
}

type rawLevel struct {
	BidPx *string `json:"bid_px"`
	AskPx *string `json:"ask_px"`
	BidSz *int64  `json:"bid_sz"`
	AskSz *int64  `json:"ask_sz"`
	BidPb *int64  `json:"bid_pb"`
	AskPb *int64  `json:"ask_pb"`
}

type rawRecord struct {
	TsRecv    *string     `json:"ts_recv"`
	Hd        *rawHeader  `json:"hd"`
	Action    *string     `json:"action"`
	Side      *string     `json:"side"`
	Price     *string     `json:"price"`
	Size      *int64      `json:"size"`
	Flags     *int64      `json:"flags"`
	TsInDelta *int64      `json:"ts_in_delta"`
	Levels    *[]rawLevel `json:"levels"`
	Symbol    *string     `json:"symbol"`
}

// ReadCMBP parses one JSON object per line into events in file order.
// It enforces MaxFileBytes, MaxLineBytes, MaxEvents, nondecreasing ts_recv,
// rtype 177, 21-byte symbols, nonzero instrument IDs with no per-symbol
// drift, ts_event <= ts_recv, known actions, and flags 192.
func ReadCMBP(r io.Reader) ([]Event, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: nil reader", ErrInvalidRecord)
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read failed", ErrInvalidRecord)
	}
	if len(data) > MaxFileBytes {
		return nil, ErrFileTooLarge
	}
	lines := bytes.Split(data, []byte{'\n'})
	events := make([]Event, 0, min(len(lines), 64))
	seen := make(map[string]int64)
	var prev time.Time
	var havePrev bool
	lineNo := 0
	for _, raw := range lines {
		// Strip a trailing CR so CRLF inputs stay bounded identically.
		line := bytes.TrimSuffix(raw, []byte{'\r'})
		if len(line) > MaxLineBytes {
			return nil, ErrLineTooLong
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		lineNo++
		if len(events)+1 > MaxEvents {
			return nil, ErrTooManyRecords
		}
		ev, err := parseRecord(line, lineNo)
		if err != nil {
			return nil, err
		}
		if havePrev && ev.TsRecv.Before(prev) {
			return nil, fmt.Errorf("line %d: %w", lineNo, ErrOutOfOrder)
		}
		if id, ok := seen[ev.Symbol]; ok && id != ev.InstrumentID {
			return nil, fmt.Errorf("line %d: %w", lineNo, ErrInstrumentDrift)
		}
		seen[ev.Symbol] = ev.InstrumentID
		prev, havePrev = ev.TsRecv, true
		events = append(events, ev)
	}
	if err := validateEvents(events); err != nil {
		return nil, err
	}
	return events, nil
}

// Snapshot folds events into per-raw-symbol observations as of asOf.
// Events with TsRecv after asOf are excluded from state but their
// schema/ordering/drift is still validated fail-closed. maxAge bounds
// fresh vs stale; age is asOf-At with exact <= boundary fresh.
func Snapshot(events []Event, asOf time.Time, maxAge time.Duration) (map[string]Observation, error) {
	if asOf.IsZero() {
		return nil, fmt.Errorf("%w: zero asOf", ErrInvalidRecord)
	}
	if maxAge < 0 {
		return nil, fmt.Errorf("%w: negative maxAge", ErrInvalidRecord)
	}
	if err := validateEvents(events); err != nil {
		return nil, err
	}
	out := make(map[string]Observation)
	for _, ev := range events {
		if ev.TsRecv.After(asOf) {
			continue
		}
		switch ev.Action {
		case ActionTrade:
			prev, ok := out[ev.Symbol]
			if !ok {
				continue
			}
			prev.TradeAt = ev.TsRecv
			prev.TradeEventAt = ev.TsEvent
			out[ev.Symbol] = prev
		case ActionReset:
			var tradeAt, tradeEventAt time.Time
			if prev, ok := out[ev.Symbol]; ok {
				tradeAt, tradeEventAt = prev.TradeAt, prev.TradeEventAt
			}
			out[ev.Symbol] = Observation{
				Symbol:        ev.Symbol,
				InstrumentID:  ev.InstrumentID,
				PublisherID:   ev.PublisherID,
				At:            ev.TsRecv,
				EventAt:       ev.TsEvent,
				TradeAt:       tradeAt,
				TradeEventAt:  tradeEventAt,
				Status:        StatusInvalid,
				InvalidReason: ReasonReset,
			}
		case ActionAdd, ActionChange, ActionModify:
			var tradeAt, tradeEventAt time.Time
			if prev, ok := out[ev.Symbol]; ok {
				tradeAt, tradeEventAt = prev.TradeAt, prev.TradeEventAt
			}
			reason, valid := classifyBook(ev.Book)
			if !valid {
				out[ev.Symbol] = Observation{
					Symbol:        ev.Symbol,
					InstrumentID:  ev.InstrumentID,
					PublisherID:   ev.PublisherID,
					At:            ev.TsRecv,
					EventAt:       ev.TsEvent,
					TradeAt:       tradeAt,
					TradeEventAt:  tradeEventAt,
					Status:        StatusInvalid,
					InvalidReason: reason,
				}
				continue
			}
			status := StatusFresh
			if asOf.Sub(ev.TsRecv) > maxAge {
				status = StatusStale
			}
			out[ev.Symbol] = Observation{
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
			}
		default:
			return nil, fmt.Errorf("%w: unknown action %q", ErrInvalidRecord, string(ev.Action))
		}
	}
	return out, nil
}

func validateEvents(events []Event) error {
	seen := make(map[string]int64)
	for i, ev := range events {
		if ev.TsRecv.IsZero() || ev.TsEvent.IsZero() {
			return fmt.Errorf("event %d: %w: missing timestamp", i, ErrInvalidRecord)
		}
		if ev.TsEvent.After(ev.TsRecv) {
			return fmt.Errorf("event %d: %w: ts_event after ts_recv", i, ErrInvalidRecord)
		}
		if ev.Rtype != RTypeCMBP1 {
			return fmt.Errorf("event %d: %w: bad rtype", i, ErrInvalidRecord)
		}
		if err := validateSymbol(ev.Symbol); err != nil {
			return fmt.Errorf("event %d: %w", i, err)
		}
		if ev.InstrumentID == 0 {
			return fmt.Errorf("event %d: %w: zero instrument", i, ErrInvalidRecord)
		}
		if ev.InstrumentID < 0 || ev.InstrumentID > 1<<32-1 {
			return fmt.Errorf("event %d: %w: instrument outside uint32", i, ErrInvalidRecord)
		}
		if ev.PublisherID < 0 || ev.PublisherID > 1<<16-1 {
			return fmt.Errorf("event %d: %w: publisher outside uint16", i, ErrInvalidRecord)
		}
		if !slices.Contains(validActions, string(ev.Action)) {
			return fmt.Errorf("event %d: %w: unknown action", i, ErrInvalidRecord)
		}
		if !slices.Contains([]string{"A", "B", "N"}, ev.Side) {
			return fmt.Errorf("event %d: %w: invalid side", i, ErrInvalidRecord)
		}
		if ev.Flags != SupportedFlags {
			return fmt.Errorf("event %d: %w: flags %d", i, ErrUnqualifiedFlags, ev.Flags)
		}
		if ev.Size < 0 || ev.Size > 1<<32-1 || ev.TsInDelta < -1<<31 || ev.TsInDelta > 1<<31-1 {
			return fmt.Errorf("event %d: %w: size/delta outside wire bounds", i, ErrInvalidRecord)
		}
		if ev.Book.BidSz < 0 || ev.Book.BidSz > 1<<32-1 || ev.Book.AskSz < 0 || ev.Book.AskSz > 1<<32-1 || ev.Book.BidPb < 0 || ev.Book.BidPb > 1<<16-1 || ev.Book.AskPb < 0 || ev.Book.AskPb > 1<<16-1 {
			return fmt.Errorf("event %d: %w: book outside wire bounds", i, ErrInvalidRecord)
		}
		if ev.PriceRaw == "" && !ev.PriceUndefined {
			return fmt.Errorf("event %d: %w: missing price", i, ErrInvalidRecord)
		}
		prices := []struct {
			raw       string
			nano      int64
			missing   bool
			undefined bool
		}{
			{ev.PriceRaw, ev.PriceNano, false, ev.PriceUndefined},
			{ev.Book.BidRaw, ev.Book.BidPxNano, ev.Book.BidMissing, ev.Book.BidUndefined},
			{ev.Book.AskRaw, ev.Book.AskPxNano, ev.Book.AskMissing, ev.Book.AskUndefined},
		}
		for _, price := range prices {
			if price.missing {
				if price.raw != "" || price.nano != 0 || price.undefined {
					return fmt.Errorf("event %d: %w: inconsistent missing price", i, ErrInvalidRecord)
				}
				continue
			}
			nano, err := parsePriceNano(price.raw)
			if err != nil || nano != price.nano || price.undefined != (nano == UndefinedPriceNano) {
				return fmt.Errorf("event %d: %w: inconsistent wire price", i, ErrInvalidRecord)
			}
		}
		if i > 0 && ev.TsRecv.Before(events[i-1].TsRecv) {
			return fmt.Errorf("event %d: %w", i, ErrOutOfOrder)
		}
		if id, ok := seen[ev.Symbol]; ok && id != ev.InstrumentID {
			return fmt.Errorf("event %d: %w", i, ErrInstrumentDrift)
		}
		seen[ev.Symbol] = ev.InstrumentID
	}
	return nil
}

func parseRecord(line []byte, lineNo int) (Event, error) {
	if err := uniqueJSONKeys(line); err != nil {
		return Event{}, fmt.Errorf("line %d: %w: duplicate or malformed json", lineNo, ErrInvalidRecord)
	}
	var raw rawRecord
	dec := json.NewDecoder(bytes.NewReader(line))
	if err := dec.Decode(&raw); err != nil {
		return Event{}, fmt.Errorf("line %d: %w: bad json", lineNo, ErrInvalidRecord)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return Event{}, fmt.Errorf("line %d: %w: trailing data", lineNo, ErrInvalidRecord)
	}
	if raw.TsRecv == nil || strings.TrimSpace(*raw.TsRecv) == "" {
		return Event{}, fmt.Errorf("line %d: %w: missing ts_recv", lineNo, ErrInvalidRecord)
	}
	tsRecv, err := parseWireTime(strings.TrimSpace(*raw.TsRecv))
	if err != nil || tsRecv.IsZero() {
		return Event{}, fmt.Errorf("line %d: %w: bad ts_recv", lineNo, ErrInvalidRecord)
	}
	if raw.Hd == nil {
		return Event{}, fmt.Errorf("line %d: %w: missing hd", lineNo, ErrInvalidRecord)
	}
	if raw.Hd.TsEvent == nil || strings.TrimSpace(*raw.Hd.TsEvent) == "" {
		return Event{}, fmt.Errorf("line %d: %w: missing ts_event", lineNo, ErrInvalidRecord)
	}
	tsEvent, err := parseWireTime(strings.TrimSpace(*raw.Hd.TsEvent))
	if err != nil || tsEvent.IsZero() {
		return Event{}, fmt.Errorf("line %d: %w: bad ts_event", lineNo, ErrInvalidRecord)
	}
	if tsEvent.After(tsRecv) {
		return Event{}, fmt.Errorf("line %d: %w: ts_event after ts_recv", lineNo, ErrInvalidRecord)
	}
	if raw.Hd.Rtype == nil {
		return Event{}, fmt.Errorf("line %d: %w: missing rtype", lineNo, ErrInvalidRecord)
	}
	if *raw.Hd.Rtype != RTypeCMBP1 {
		return Event{}, fmt.Errorf("line %d: %w: bad rtype", lineNo, ErrInvalidRecord)
	}
	if raw.Hd.PublisherID == nil {
		return Event{}, fmt.Errorf("line %d: %w: missing publisher_id", lineNo, ErrInvalidRecord)
	}
	if *raw.Hd.PublisherID < 0 {
		return Event{}, fmt.Errorf("line %d: %w: bad publisher_id", lineNo, ErrInvalidRecord)
	}
	if raw.Hd.InstrumentID == nil {
		return Event{}, fmt.Errorf("line %d: %w: missing instrument_id", lineNo, ErrInvalidRecord)
	}
	if *raw.Hd.InstrumentID <= 0 {
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
	if raw.Price == nil {
		return Event{}, fmt.Errorf("line %d: %w: missing price", lineNo, ErrInvalidRecord)
	}
	priceRaw := strings.TrimSpace(*raw.Price)
	if priceRaw == "" {
		return Event{}, fmt.Errorf("line %d: %w: empty price", lineNo, ErrInvalidRecord)
	}
	priceUndefined := priceRaw == UndefinedPriceRaw
	var priceNano int64
	if priceUndefined {
		priceNano = UndefinedPriceNano
	} else {
		nano, err := parsePriceNano(priceRaw)
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
	if *raw.Size < 0 {
		return Event{}, fmt.Errorf("line %d: %w: bad size", lineNo, ErrInvalidRecord)
	}
	if raw.Flags == nil {
		return Event{}, fmt.Errorf("line %d: %w: missing flags", lineNo, ErrInvalidRecord)
	}
	if *raw.Flags != SupportedFlags {
		return Event{}, fmt.Errorf("line %d: %w: flags %d", lineNo, ErrUnqualifiedFlags, *raw.Flags)
	}
	if raw.TsInDelta == nil {
		return Event{}, fmt.Errorf("line %d: %w: missing ts_in_delta", lineNo, ErrInvalidRecord)
	}
	if raw.Levels == nil {
		return Event{}, fmt.Errorf("line %d: %w: missing levels", lineNo, ErrInvalidRecord)
	}
	if len(*raw.Levels) != 1 {
		return Event{}, fmt.Errorf("line %d: %w: levels must hold exactly one entry", lineNo, ErrInvalidRecord)
	}
	book, err := parseLevel((*raw.Levels)[0], lineNo)
	if err != nil {
		return Event{}, err
	}
	if raw.Symbol == nil || *raw.Symbol == "" {
		return Event{}, fmt.Errorf("line %d: %w: missing symbol", lineNo, ErrInvalidRecord)
	}
	if err := validateSymbol(*raw.Symbol); err != nil {
		return Event{}, fmt.Errorf("line %d: %w", lineNo, err)
	}
	return Event{
		Symbol:         *raw.Symbol,
		TsRecv:         tsRecv,
		TsEvent:        tsEvent,
		Rtype:          RTypeCMBP1,
		PublisherID:    *raw.Hd.PublisherID,
		InstrumentID:   *raw.Hd.InstrumentID,
		Action:         Action(*raw.Action),
		Side:           *raw.Side,
		PriceNano:      priceNano,
		PriceRaw:       priceRaw,
		PriceUndefined: priceUndefined,
		Size:           *raw.Size,
		Flags:          *raw.Flags,
		TsInDelta:      *raw.TsInDelta,
		Book:           book,
	}, nil
}

func parseLevel(lv rawLevel, lineNo int) (BookLevel, error) {
	var b BookLevel
	if lv.BidPx == nil {
		return BookLevel{}, fmt.Errorf("line %d: %w: missing bid_px", lineNo, ErrInvalidRecord)
	}
	if lv.AskPx == nil {
		return BookLevel{}, fmt.Errorf("line %d: %w: missing ask_px", lineNo, ErrInvalidRecord)
	}
	b.BidRaw = strings.TrimSpace(*lv.BidPx)
	b.AskRaw = strings.TrimSpace(*lv.AskPx)
	// Empty string means missing price: invalid book, not a record error.
	b.BidMissing = b.BidRaw == ""
	b.AskMissing = b.AskRaw == ""
	if !b.BidMissing {
		if b.BidRaw == UndefinedPriceRaw {
			b.BidUndefined = true
			b.BidPxNano = UndefinedPriceNano
		} else {
			nano, err := parsePriceNano(b.BidRaw)
			if err != nil {
				return BookLevel{}, fmt.Errorf("line %d: %w: bad bid_px", lineNo, ErrInvalidRecord)
			}
			b.BidPxNano = nano
		}
	}
	if !b.AskMissing {
		if b.AskRaw == UndefinedPriceRaw {
			b.AskUndefined = true
			b.AskPxNano = UndefinedPriceNano
		} else {
			nano, err := parsePriceNano(b.AskRaw)
			if err != nil {
				return BookLevel{}, fmt.Errorf("line %d: %w: bad ask_px", lineNo, ErrInvalidRecord)
			}
			b.AskPxNano = nano
		}
	}
	if lv.BidSz == nil {
		return BookLevel{}, fmt.Errorf("line %d: %w: missing bid_sz", lineNo, ErrInvalidRecord)
	}
	if lv.AskSz == nil {
		return BookLevel{}, fmt.Errorf("line %d: %w: missing ask_sz", lineNo, ErrInvalidRecord)
	}
	if lv.BidPb == nil {
		return BookLevel{}, fmt.Errorf("line %d: %w: missing bid_pb", lineNo, ErrInvalidRecord)
	}
	if lv.AskPb == nil {
		return BookLevel{}, fmt.Errorf("line %d: %w: missing ask_pb", lineNo, ErrInvalidRecord)
	}
	if *lv.BidSz < 0 || *lv.AskSz < 0 || *lv.BidPb < 0 || *lv.AskPb < 0 {
		return BookLevel{}, fmt.Errorf("line %d: %w: negative book size", lineNo, ErrInvalidRecord)
	}
	b.BidSz, b.AskSz, b.BidPb, b.AskPb = *lv.BidSz, *lv.AskSz, *lv.BidPb, *lv.AskPb
	return b, nil
}

// classifyBook reports validity for a quote update. Missing and sentinel
// prices, nonpositive quotes, zero sizes, and crossed books invalidate;
// locked (bid==ask) books stay valid.
func classifyBook(b BookLevel) (string, bool) {
	if b.BidMissing || b.AskMissing {
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

func validateSymbol(s string) error {
	if len(s) != SymbolLen {
		return fmt.Errorf("%w: bad symbol length", ErrInvalidRecord)
	}
	for i := range len(s) {
		if s[i] < 0x20 || s[i] > 0x7E {
			return fmt.Errorf("%w: bad symbol bytes", ErrInvalidRecord)
		}
	}
	root := strings.TrimRight(s[:6], " ")
	if root == "" || root+strings.Repeat(" ", 6-len(root)) != s[:6] {
		return fmt.Errorf("%w: bad OSI root", ErrInvalidRecord)
	}
	for i := range len(root) {
		if !(root[i] >= 'A' && root[i] <= 'Z' || root[i] >= '0' && root[i] <= '9') {
			return fmt.Errorf("%w: bad OSI root", ErrInvalidRecord)
		}
	}
	if s[12] != 'C' && s[12] != 'P' {
		return fmt.Errorf("%w: bad OSI right", ErrInvalidRecord)
	}
	if _, err := time.Parse("20060102", "20"+s[6:12]); err != nil {
		return fmt.Errorf("%w: bad OSI expiry", ErrInvalidRecord)
	}
	for i := range 8 {
		if s[13+i] < '0' || s[13+i] > '9' {
			return fmt.Errorf("%w: bad OSI strike", ErrInvalidRecord)
		}
	}
	return nil
}

// Go's JSON decoder accepts repeated fields. Reject ambiguity before decoding
// the wire shape, including case variants that struct decoding would merge.
func uniqueJSONKeys(data []byte) error {
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
				if !ok || seen[strings.ToLower(name)] {
					return errors.New("duplicate object key")
				}
				seen[strings.ToLower(name)] = true
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

// parsePriceNano reads the provider's pretty_px=false integer nanodollars.
// Scaling an integer wire value as dollar text would corrupt every price.
func parsePriceNano(s string) (int64, error) {
	if s == "" || strings.TrimSpace(s) != s || strings.HasPrefix(s, "+") {
		return 0, errors.New("invalid integer price")
	}
	return strconv.ParseInt(s, 10, 64)
}

// parseWireTime reads pretty_ts=false nanosecond strings, rejecting the
// undefined uint64 sentinel, zero, negatives and values outside int64 time.
func parseWireTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("missing integer timestamp")
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return time.Time{}, errors.New("invalid integer timestamp")
		}
	}
	ns, err := strconv.ParseInt(s, 10, 64)
	if err != nil || ns <= 0 {
		return time.Time{}, errors.New("invalid integer timestamp")
	}
	return time.Unix(0, ns).UTC(), nil
}
