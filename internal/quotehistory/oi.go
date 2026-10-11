// Package quotehistory OI groundwork: bounded offline OPRA statistics
// open-interest parser and causal duplicate-publisher summarizer.
//
// This file is groundwork for a full study, not source qualification. It
// never trades, never touches financial controls, and performs no network,
// credential, or data-purchase work. It makes no P&L, fill, coverage,
// listing, weekly, deliverable, Greek, or replay claim.
//
// Wire scope mirrors the actual purchased statistics JSONL shape:
// root{ts_recv, hd{ts_event, rtype, publisher_id, instrument_id}, ts_ref,
// price, quantity, sequence, ts_in_delta, stat_type, channel_id,
// update_action, stat_flags, symbol}. All rows count toward resource and
// ordering bounds even when stat_type != 9; non-OI rows are ignored
// economically by SnapshotOpenInterest.
//
// Supported stat_type values cover the observed full-sample set:
// 9 (open interest) plus price statistics 1,4,5,6,7,8,11,12, which are
// counted for bounds/order but never affect OI state.
//
// Causal rules:
//   - Capture (ts_recv) is the known-at time. OI ts_event must not exceed it.
//     Ignored price-statistic rows preserve source-clock skew without
//     changing OI state or its timestamps.
//   - ts_ref UINT64_MAX ("18446744073709551615") is explicit absence
//     (HasRef false). A missing reference never invents a historical as-of.
//     When present, the reference must not be future relative to capture.
//   - OI quantity 0 is legitimate; negatives and INT64_MAX sentinel fail.
//   - For OI only update_action 1 and stat_flags 0 are supported; any other
//     OI delete/flags value fails rather than retaining a stale value.
//   - Price is a signed nanodollar string validated strictly but never
//     treated as a market price; SnapshotOpenInterest exposes no price,
//     Greek, completeness, or replay input.
//   - Per-symbol instrument identity is immutable; capture is nondecreasing
//     in file order; same-capture order is file order deterministic.
//
// Minimal API:
//
//	events, err := quotehistory.ReadStatistics(r)
//	oi, err := quotehistory.SnapshotOpenInterest(events, asOf)
package quotehistory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
)

// Statistics wire constants.
const (
	// RTypeStatistics is the only supported statistics rtype.
	RTypeStatistics = 24
	// StatTypeOpenInterest is the only stat_type that affects OI state.
	StatTypeOpenInterest = 9
	// OIUpdateActionAdd is the only supported OI update_action.
	OIUpdateActionAdd = 1
	// OISupportedStatFlags is the only supported OI stat_flags.
	OISupportedStatFlags = 0
	// QuantityMissingSentinel is the INT64_MAX unused-quantity sentinel.
	QuantityMissingSentinel = int64(1<<63 - 1)
	// RefMissingRaw is the UINT64_MAX ts_ref missing sentinel.
	RefMissingRaw = "18446744073709551615"
)

// supportedStatTypes covers the observed full-sample set: OI 9 plus price
// statistics 1,4,5,6,7,8,11,12. Other values fail as unqualified.
var supportedStatTypes = []int64{1, 4, 5, 6, 7, 8, 9, 11, 12}

// ErrQuantityConflict reports publisher-copy disagreement for one symbol.
// Copies are never summed; disagreement fails closed.
var ErrQuantityConflict = errors.New("quotehistory: open-interest quantity conflict")

// StatEvent preserves actual capture time, original source event time, and
// immutable instrument identity for one statistics row. Price is retained
// for wire audit only and never defines OI state. Quantity is meaningful
// only when StatType == StatTypeOpenInterest; other rows are counted for
// bounds/order and ignored economically.
type StatEvent struct {
	Symbol       string
	TsRecv       time.Time
	TsEvent      time.Time
	Rtype        int
	PublisherID  int64
	InstrumentID int64
	HasRef       bool
	RefTime      time.Time
	PriceRaw     string
	PriceNano    int64
	Quantity     int64
	Sequence     int64
	TsInDelta    int64
	StatType     int64
	ChannelID    int64
	UpdateAction int64
	StatFlags    int64
}

// OpenInterest is the causal per-symbol OI state at asOf. At is the latest
// included capture (known-at); EventAt is the corresponding OPRA
// consolidator timestamp. Copies counts agreeing publisher copies; Quantity
// is never a sum. Absent symbols are simply absent from the Snapshot map.
type OpenInterest struct {
	Symbol       string
	InstrumentID int64
	Quantity     int64
	At           time.Time
	EventAt      time.Time
	Copies       int
}

type rawStatRecord struct {
	TsRecv       *string    `json:"ts_recv"`
	Hd           *rawHeader `json:"hd"`
	TsRef        *string    `json:"ts_ref"`
	Price        *string    `json:"price"`
	Quantity     *int64     `json:"quantity"`
	Sequence     *int64     `json:"sequence"`
	TsInDelta    *int64     `json:"ts_in_delta"`
	StatType     *int64     `json:"stat_type"`
	ChannelID    *int64     `json:"channel_id"`
	UpdateAction *int64     `json:"update_action"`
	StatFlags    *int64     `json:"stat_flags"`
	Symbol       *string    `json:"symbol"`
}

// ReadStatistics parses one JSON object per line into events in file order.
// It enforces MaxFileBytes, MaxLineBytes, MaxEvents across all rows
// including non-OI, nondecreasing ts_recv, rtype 24, 21-byte OSI symbols,
// nonzero instrument IDs with no per-symbol drift, OI ts_event <= ts_recv,
// supported stat_type values, ts_in_delta 0, OI quantity/update/flag rules,
// and ts_ref future checks.
func ReadStatistics(r io.Reader) ([]StatEvent, error) {
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
	events := make([]StatEvent, 0, 64)
	seen := make(map[string]int64)
	var prev time.Time
	var havePrev bool
	lineNo := 0
	for raw := range bytes.SplitSeq(data, []byte{'\n'}) {
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
		ev, err := parseStatRecord(line, lineNo)
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
	if err := validateStatEvents(events); err != nil {
		return nil, err
	}
	return events, nil
}

// SnapshotOpenInterest folds validated statistics events into per-raw-symbol
// OI state as of asOf. It validates every supplied event before filtering
// so future identity/order drift fails closed. For each symbol it keeps the
// last event per publisher with TsRecv <= asOf and StatType 9 in file order,
// requires all latest publisher quantities to agree, and never sums copies.
// Non-OI rows never affect state. Missing symbols stay absent.
func SnapshotOpenInterest(events []StatEvent, asOf time.Time) (map[string]OpenInterest, error) {
	if asOf.IsZero() {
		return nil, fmt.Errorf("%w: zero asOf", ErrInvalidRecord)
	}
	if len(events) > MaxEvents {
		return nil, ErrTooManyRecords
	}
	if err := validateStatEvents(events); err != nil {
		return nil, err
	}
	type latest struct {
		ev  StatEvent
		idx int
	}
	perSymbol := make(map[string]map[int64]latest)
	for idx, ev := range events {
		if ev.StatType != StatTypeOpenInterest {
			continue
		}
		if ev.TsRecv.After(asOf) {
			continue
		}
		m, ok := perSymbol[ev.Symbol]
		if !ok {
			m = make(map[int64]latest)
			perSymbol[ev.Symbol] = m
		}
		m[ev.PublisherID] = latest{ev: ev, idx: idx}
	}
	out := make(map[string]OpenInterest)
	for _, symbol := range slices.Sorted(maps.Keys(perSymbol)) {
		m := perSymbol[symbol]
		var agreed int64
		first := true
		for _, l := range m {
			if first {
				agreed, first = l.ev.Quantity, false
				continue
			}
			if l.ev.Quantity != agreed {
				return nil, fmt.Errorf("%w: symbol %q", ErrQuantityConflict, symbol)
			}
		}
		// Latest capture among agreeing copies; same-capture tie breaks by
		// later file order for determinism.
		var best StatEvent
		bestIdx := -1
		var haveBest bool
		for _, l := range m {
			if !haveBest || l.ev.TsRecv.After(best.TsRecv) || (l.ev.TsRecv.Equal(best.TsRecv) && l.idx > bestIdx) {
				best, bestIdx, haveBest = l.ev, l.idx, true
			}
		}
		if !haveBest {
			continue
		}
		out[symbol] = OpenInterest{
			Symbol:       symbol,
			InstrumentID: best.InstrumentID,
			Quantity:     agreed,
			At:           best.TsRecv,
			EventAt:      best.TsEvent,
			Copies:       len(m),
		}
	}
	return out, nil
}

func validateStatEvents(events []StatEvent) error {
	seen := make(map[string]int64)
	identities := make(map[int64]string)
	for i, ev := range events {
		if ev.TsRecv.IsZero() || ev.TsEvent.IsZero() {
			return fmt.Errorf("event %d: %w: missing timestamp", i, ErrInvalidRecord)
		}
		if ev.StatType == StatTypeOpenInterest && ev.TsEvent.After(ev.TsRecv) {
			return fmt.Errorf("event %d: %w: ts_event after ts_recv", i, ErrInvalidRecord)
		}
		if ev.Rtype != RTypeStatistics {
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
		if ev.Sequence < 0 || ev.Sequence > 1<<32-1 {
			return fmt.Errorf("event %d: %w: sequence outside uint32", i, ErrInvalidRecord)
		}
		if ev.ChannelID < 0 || ev.ChannelID > 1<<16-1 {
			return fmt.Errorf("event %d: %w: channel outside uint16", i, ErrInvalidRecord)
		}
		if ev.UpdateAction < 0 || ev.UpdateAction > 1<<8-1 || ev.StatFlags < 0 || ev.StatFlags > 1<<8-1 {
			return fmt.Errorf("event %d: %w: action/flags outside uint8", i, ErrInvalidRecord)
		}
		if !slices.Contains(supportedStatTypes, ev.StatType) {
			return fmt.Errorf("event %d: %w: unsupported stat_type %d", i, ErrInvalidRecord, ev.StatType)
		}
		if ev.TsInDelta != 0 {
			return fmt.Errorf("event %d: %w: ts_in_delta must be 0", i, ErrInvalidRecord)
		}
		if ev.PriceRaw == "" {
			return fmt.Errorf("event %d: %w: missing price", i, ErrInvalidRecord)
		}
		nano, err := parsePriceNano(ev.PriceRaw)
		if err != nil || nano != ev.PriceNano {
			return fmt.Errorf("event %d: %w: inconsistent wire price", i, ErrInvalidRecord)
		}
		if ev.HasRef {
			if ev.RefTime.IsZero() {
				return fmt.Errorf("event %d: %w: missing ref time", i, ErrInvalidRecord)
			}
			if ev.RefTime.After(ev.TsRecv) {
				return fmt.Errorf("event %d: %w: ts_ref future", i, ErrInvalidRecord)
			}
		} else if !ev.RefTime.IsZero() {
			return fmt.Errorf("event %d: %w: inconsistent missing ref", i, ErrInvalidRecord)
		}
		if ev.StatType == StatTypeOpenInterest {
			if ev.Quantity < 0 || ev.Quantity == QuantityMissingSentinel {
				return fmt.Errorf("event %d: %w: bad OI quantity", i, ErrInvalidRecord)
			}
			if ev.UpdateAction != OIUpdateActionAdd {
				return fmt.Errorf("event %d: %w: unsupported OI update_action %d", i, ErrInvalidRecord, ev.UpdateAction)
			}
			if ev.StatFlags != OISupportedStatFlags {
				return fmt.Errorf("event %d: %w: flags %d", i, ErrUnqualifiedFlags, ev.StatFlags)
			}
		}
		if i > 0 && ev.TsRecv.Before(events[i-1].TsRecv) {
			return fmt.Errorf("event %d: %w", i, ErrOutOfOrder)
		}
		if id, ok := seen[ev.Symbol]; ok && id != ev.InstrumentID {
			return fmt.Errorf("event %d: %w", i, ErrInstrumentDrift)
		}
		if symbol, ok := identities[ev.InstrumentID]; ok && symbol != ev.Symbol {
			return fmt.Errorf("event %d: %w: instrument maps to multiple symbols", i, ErrInstrumentDrift)
		}
		seen[ev.Symbol] = ev.InstrumentID
		identities[ev.InstrumentID] = ev.Symbol
	}
	return nil
}

func parseStatRecord(line []byte, lineNo int) (StatEvent, error) {
	if err := uniqueJSONKeys(line); err != nil {
		return StatEvent{}, fmt.Errorf("line %d: %w: duplicate or malformed json", lineNo, ErrInvalidRecord)
	}
	var raw rawStatRecord
	dec := json.NewDecoder(bytes.NewReader(line))
	if err := dec.Decode(&raw); err != nil {
		return StatEvent{}, fmt.Errorf("line %d: %w: bad json", lineNo, ErrInvalidRecord)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return StatEvent{}, fmt.Errorf("line %d: %w: trailing data", lineNo, ErrInvalidRecord)
	}
	if raw.TsRecv == nil || strings.TrimSpace(*raw.TsRecv) == "" {
		return StatEvent{}, fmt.Errorf("line %d: %w: missing ts_recv", lineNo, ErrInvalidRecord)
	}
	tsRecv, err := parseWireTime(strings.TrimSpace(*raw.TsRecv))
	if err != nil || tsRecv.IsZero() {
		return StatEvent{}, fmt.Errorf("line %d: %w: bad ts_recv", lineNo, ErrInvalidRecord)
	}
	if raw.Hd == nil {
		return StatEvent{}, fmt.Errorf("line %d: %w: missing hd", lineNo, ErrInvalidRecord)
	}
	if raw.Hd.TsEvent == nil || strings.TrimSpace(*raw.Hd.TsEvent) == "" {
		return StatEvent{}, fmt.Errorf("line %d: %w: missing ts_event", lineNo, ErrInvalidRecord)
	}
	tsEvent, err := parseWireTime(strings.TrimSpace(*raw.Hd.TsEvent))
	if err != nil || tsEvent.IsZero() {
		return StatEvent{}, fmt.Errorf("line %d: %w: bad ts_event", lineNo, ErrInvalidRecord)
	}
	if raw.Hd.Rtype == nil {
		return StatEvent{}, fmt.Errorf("line %d: %w: missing rtype", lineNo, ErrInvalidRecord)
	}
	if *raw.Hd.Rtype != RTypeStatistics {
		return StatEvent{}, fmt.Errorf("line %d: %w: bad rtype", lineNo, ErrInvalidRecord)
	}
	if raw.Hd.PublisherID == nil {
		return StatEvent{}, fmt.Errorf("line %d: %w: missing publisher_id", lineNo, ErrInvalidRecord)
	}
	if *raw.Hd.PublisherID < 0 {
		return StatEvent{}, fmt.Errorf("line %d: %w: bad publisher_id", lineNo, ErrInvalidRecord)
	}
	if raw.Hd.InstrumentID == nil {
		return StatEvent{}, fmt.Errorf("line %d: %w: missing instrument_id", lineNo, ErrInvalidRecord)
	}
	if *raw.Hd.InstrumentID <= 0 {
		return StatEvent{}, fmt.Errorf("line %d: %w: bad instrument_id", lineNo, ErrInvalidRecord)
	}
	if raw.TsRef == nil {
		return StatEvent{}, fmt.Errorf("line %d: %w: missing ts_ref", lineNo, ErrInvalidRecord)
	}
	tsRefRaw := strings.TrimSpace(*raw.TsRef)
	if tsRefRaw == "" {
		return StatEvent{}, fmt.Errorf("line %d: %w: empty ts_ref", lineNo, ErrInvalidRecord)
	}
	var hasRef bool
	var refTime time.Time
	if tsRefRaw == RefMissingRaw {
		hasRef = false
	} else {
		rt, err := parseWireTime(tsRefRaw)
		if err != nil || rt.IsZero() {
			return StatEvent{}, fmt.Errorf("line %d: %w: bad ts_ref", lineNo, ErrInvalidRecord)
		}
		if rt.After(tsRecv) {
			return StatEvent{}, fmt.Errorf("line %d: %w: ts_ref future", lineNo, ErrInvalidRecord)
		}
		hasRef, refTime = true, rt
	}
	if raw.Price == nil {
		return StatEvent{}, fmt.Errorf("line %d: %w: missing price", lineNo, ErrInvalidRecord)
	}
	priceRaw := strings.TrimSpace(*raw.Price)
	if priceRaw == "" {
		return StatEvent{}, fmt.Errorf("line %d: %w: empty price", lineNo, ErrInvalidRecord)
	}
	priceNano, err := parsePriceNano(priceRaw)
	if err != nil {
		return StatEvent{}, fmt.Errorf("line %d: %w: bad price", lineNo, ErrInvalidRecord)
	}
	if raw.Quantity == nil {
		return StatEvent{}, fmt.Errorf("line %d: %w: missing quantity", lineNo, ErrInvalidRecord)
	}
	if raw.Sequence == nil {
		return StatEvent{}, fmt.Errorf("line %d: %w: missing sequence", lineNo, ErrInvalidRecord)
	}
	if *raw.Sequence < 0 {
		return StatEvent{}, fmt.Errorf("line %d: %w: bad sequence", lineNo, ErrInvalidRecord)
	}
	if raw.TsInDelta == nil {
		return StatEvent{}, fmt.Errorf("line %d: %w: missing ts_in_delta", lineNo, ErrInvalidRecord)
	}
	if *raw.TsInDelta != 0 {
		return StatEvent{}, fmt.Errorf("line %d: %w: ts_in_delta must be 0", lineNo, ErrInvalidRecord)
	}
	if raw.StatType == nil {
		return StatEvent{}, fmt.Errorf("line %d: %w: missing stat_type", lineNo, ErrInvalidRecord)
	}
	if !slices.Contains(supportedStatTypes, *raw.StatType) {
		return StatEvent{}, fmt.Errorf("line %d: %w: unsupported stat_type %d", lineNo, ErrInvalidRecord, *raw.StatType)
	}
	if raw.ChannelID == nil {
		return StatEvent{}, fmt.Errorf("line %d: %w: missing channel_id", lineNo, ErrInvalidRecord)
	}
	if *raw.ChannelID < 0 {
		return StatEvent{}, fmt.Errorf("line %d: %w: bad channel_id", lineNo, ErrInvalidRecord)
	}
	if raw.UpdateAction == nil {
		return StatEvent{}, fmt.Errorf("line %d: %w: missing update_action", lineNo, ErrInvalidRecord)
	}
	if raw.StatFlags == nil {
		return StatEvent{}, fmt.Errorf("line %d: %w: missing stat_flags", lineNo, ErrInvalidRecord)
	}
	if *raw.StatType == StatTypeOpenInterest {
		if tsEvent.After(tsRecv) {
			return StatEvent{}, fmt.Errorf("line %d: %w: ts_event after ts_recv", lineNo, ErrInvalidRecord)
		}
		if *raw.Quantity < 0 || *raw.Quantity == QuantityMissingSentinel {
			return StatEvent{}, fmt.Errorf("line %d: %w: bad OI quantity", lineNo, ErrInvalidRecord)
		}
		if *raw.UpdateAction != OIUpdateActionAdd {
			return StatEvent{}, fmt.Errorf("line %d: %w: unsupported OI update_action %d", lineNo, ErrInvalidRecord, *raw.UpdateAction)
		}
		if *raw.StatFlags != OISupportedStatFlags {
			return StatEvent{}, fmt.Errorf("line %d: %w: flags %d", lineNo, ErrUnqualifiedFlags, *raw.StatFlags)
		}
	}
	if raw.Symbol == nil || *raw.Symbol == "" {
		return StatEvent{}, fmt.Errorf("line %d: %w: missing symbol", lineNo, ErrInvalidRecord)
	}
	if err := validateSymbol(*raw.Symbol); err != nil {
		return StatEvent{}, fmt.Errorf("line %d: %w", lineNo, err)
	}
	return StatEvent{
		Symbol:       *raw.Symbol,
		TsRecv:       tsRecv,
		TsEvent:      tsEvent,
		Rtype:        RTypeStatistics,
		PublisherID:  *raw.Hd.PublisherID,
		InstrumentID: *raw.Hd.InstrumentID,
		HasRef:       hasRef,
		RefTime:      refTime,
		PriceRaw:     priceRaw,
		PriceNano:    priceNano,
		Quantity:     *raw.Quantity,
		Sequence:     *raw.Sequence,
		TsInDelta:    *raw.TsInDelta,
		StatType:     *raw.StatType,
		ChannelID:    *raw.ChannelID,
		UpdateAction: *raw.UpdateAction,
		StatFlags:    *raw.StatFlags,
	}, nil
}
