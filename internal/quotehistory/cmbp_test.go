package quotehistory

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testSymbol    = "NFLX  260410C00100000"
	testSymbolPut = "NFLX  260410P00100000"
)

func baseTime() time.Time {
	return time.Date(2026, 4, 6, 13, 30, 0, 0, time.UTC)
}

func fmtNano(t time.Time) string {
	return strconv.FormatInt(t.UnixNano(), 10)
}

func validMap(tsRecv, tsEvent time.Time, action string) map[string]any {
	return map[string]any{
		"ts_recv": fmtNano(tsRecv),
		"hd": map[string]any{
			"ts_event":      fmtNano(tsEvent),
			"rtype":         177,
			"publisher_id":  1,
			"instrument_id": 4242,
		},
		"action":      action,
		"side":        "B",
		"price":       "1500000000",
		"size":        10,
		"flags":       192,
		"ts_in_delta": 0,
		"levels": []any{
			map[string]any{
				"bid_px": "1400000000",
				"ask_px": "1600000000",
				"bid_sz": 10,
				"ask_sz": 12,
				"bid_pb": 1,
				"ask_pb": 1,
			},
		},
		"symbol": testSymbol,
	}
}

func marshal(t *testing.T, m map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func readLines(t *testing.T, lines ...string) []Event {
	t.Helper()
	joined := strings.Join(lines, "\n") + "\n"
	evs, err := ReadCMBP(strings.NewReader(joined))
	if err != nil {
		t.Fatalf("ReadCMBP failed: %v", err)
	}
	return evs
}

func mustSnapshot(t *testing.T, evs []Event, asOf time.Time, maxAge time.Duration) map[string]Observation {
	t.Helper()
	out, err := Snapshot(evs, asOf, maxAge)
	if err != nil {
		t.Fatalf("Snapshot failed: %v", err)
	}
	return out
}

func TestSnapshotExcludesFutureCapture(t *testing.T) {
	t0 := baseTime()
	t1 := t0.Add(5 * time.Second)
	early := validMap(t0, t0, "A")
	late := validMap(t1, t1, "A")
	late["levels"] = []any{map[string]any{
		"bid_px": "9400000000", "ask_px": "9600000000",
		"bid_sz": 10, "ask_sz": 12, "bid_pb": 1, "ask_pb": 1,
	}}
	evs := readLines(t, marshal(t, early), marshal(t, late))
	out := mustSnapshot(t, evs, t0, time.Minute)
	obs, ok := out[testSymbol]
	if !ok {
		t.Fatal("expected symbol present")
	}
	if !obs.At.Equal(t0) {
		t.Fatalf("future capture leaked: At=%v want %v", obs.At, t0)
	}
	if obs.BidPxNano != 1_400_000_000 || obs.AskPxNano != 1_600_000_000 {
		t.Fatalf("future book leaked: bid=%d ask=%d", obs.BidPxNano, obs.AskPxNano)
	}
	if got := slices.Sorted(maps.Keys(out)); !slices.Equal(got, []string{testSymbol}) {
		t.Fatalf("unexpected keys %v", got)
	}
}

func TestTradeDoesNotRefreshStaleAge(t *testing.T) {
	t0 := baseTime()
	tTrade := t0.Add(35 * time.Second)
	asOf := t0.Add(40 * time.Second)
	q := validMap(t0, t0, "A")
	tr := validMap(tTrade, tTrade, "T")
	tr["side"] = "N"
	tr["price"] = "1550000000"
	evs := readLines(t, marshal(t, q), marshal(t, tr))
	out := mustSnapshot(t, evs, asOf, 30*time.Second)
	obs := out[testSymbol]
	if obs.Status != StatusStale {
		t.Fatalf("trade refreshed age: status=%q At=%v", obs.Status, obs.At)
	}
	if !obs.At.Equal(t0) {
		t.Fatalf("At moved by trade: %v", obs.At)
	}
	if obs.TradeAt.IsZero() || !obs.TradeAt.Equal(tTrade) {
		t.Fatalf("TradeAt not tracked: %v", obs.TradeAt)
	}
}

func TestTradeDoesNotResurrectInvalid(t *testing.T) {
	t0 := baseTime()
	t1 := t0.Add(time.Second)
	q := validMap(t0, t0, "A")
	q["levels"] = []any{map[string]any{
		"bid_px": "1600000000", "ask_px": "1400000000",
		"bid_sz": 10, "ask_sz": 10, "bid_pb": 1, "ask_pb": 1,
	}}
	tr := validMap(t1, t1, "T")
	tr["side"] = "N"
	tr["price"] = "1550000000"
	evs := readLines(t, marshal(t, q), marshal(t, tr))
	out := mustSnapshot(t, evs, t1, time.Minute)
	obs := out[testSymbol]
	if obs.Status != StatusInvalid {
		t.Fatalf("trade resurrected invalid book: %+v", obs)
	}
	if obs.BidPxNano != 0 || obs.AskPxNano != 0 {
		t.Fatalf("invalid emitted guessed prices: %d/%d", obs.BidPxNano, obs.AskPxNano)
	}
	if obs.TradeAt.IsZero() {
		t.Fatal("expected TradeAt tracked")
	}
}

func TestInvalidUpdateClearsValid(t *testing.T) {
	t0 := baseTime()
	t1 := t0.Add(time.Second)
	good := validMap(t0, t0, "A")
	bad := validMap(t1, t1, "C")
	bad["levels"] = []any{map[string]any{
		"bid_px": "1400000000", "ask_px": "1600000000",
		"bid_sz": 0, "ask_sz": 12, "bid_pb": 1, "ask_pb": 1,
	}}
	evs := readLines(t, marshal(t, good), marshal(t, bad))
	out := mustSnapshot(t, evs, t1, time.Minute)
	obs := out[testSymbol]
	if obs.Status != StatusInvalid || obs.InvalidReason != ReasonZeroSize {
		t.Fatalf("invalid update did not clear valid: %+v", obs)
	}
	if obs.BidPxNano != 0 || obs.AskPxNano != 0 {
		t.Fatalf("cleared state kept guessed prices: %+v", obs)
	}
	if !obs.At.Equal(t1) {
		t.Fatalf("At not advanced to invalidating quote: %v", obs.At)
	}
}

func TestResetAlwaysInvalidates(t *testing.T) {
	t0 := baseTime()
	t1 := t0.Add(time.Second)
	good := validMap(t0, t0, "A")
	reset := validMap(t1, t1, "R")
	// Old book still printed on R; Snapshot must ignore it.
	evs := readLines(t, marshal(t, good), marshal(t, reset))
	out := mustSnapshot(t, evs, t1, time.Minute)
	obs := out[testSymbol]
	if obs.Status != StatusInvalid || obs.InvalidReason != ReasonReset {
		t.Fatalf("R did not invalidate: %+v", obs)
	}
	if obs.BidPxNano != 0 || obs.AskPxNano != 0 {
		t.Fatalf("R kept printed prices: %+v", obs)
	}
}

func TestExact30sBoundary(t *testing.T) {
	t0 := baseTime()
	q := validMap(t0, t0, "A")
	evs := readLines(t, marshal(t, q))
	fresh, err := Snapshot(evs, t0.Add(30*time.Second), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if fresh[testSymbol].Status != StatusFresh {
		t.Fatalf("exact 30s should be fresh: %+v", fresh[testSymbol])
	}
	stale, err := Snapshot(evs, t0.Add(30*time.Second+time.Nanosecond), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if stale[testSymbol].Status != StatusStale {
		t.Fatalf("30s+1ns should be stale: %+v", stale[testSymbol])
	}
}

func TestReadRejectsOutOfOrder(t *testing.T) {
	t0 := baseTime()
	t1 := t0.Add(time.Second)
	a := marshal(t, validMap(t1, t1, "A"))
	b := marshal(t, validMap(t0, t0, "A"))
	if _, err := ReadCMBP(strings.NewReader(a + "\n" + b + "\n")); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("expected out-of-order, got %v", err)
	}
	// Snapshot must also fail closed on caller-built out-of-order slices.
	evs := readLines(t, marshal(t, validMap(t0, t0, "A")))
	evs = append(evs, evs[0])
	evs[1].TsRecv = t0.Add(-time.Second)
	evs[1].TsEvent = t0.Add(-time.Second)
	if _, err := Snapshot(evs, t1, time.Minute); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("expected snapshot out-of-order, got %v", err)
	}
	// Equal captures are allowed.
	evs = readLines(t, marshal(t, validMap(t0, t0, "A")), marshal(t, validMap(t0, t0, "C")))
	if _, err := Snapshot(evs, t0, time.Minute); err != nil {
		t.Fatalf("equal captures should pass: %v", err)
	}
}

func TestRejectsInstrumentDrift(t *testing.T) {
	t0 := baseTime()
	t1 := t0.Add(time.Second)
	a := validMap(t0, t0, "A")
	b := validMap(t1, t1, "A")
	b["hd"] = map[string]any{
		"ts_event": fmtNano(t1), "rtype": 177, "publisher_id": 1, "instrument_id": 9999,
	}
	if _, err := ReadCMBP(strings.NewReader(marshal(t, a) + "\n" + marshal(t, b) + "\n")); !errors.Is(err, ErrInstrumentDrift) {
		t.Fatalf("expected drift, got %v", err)
	}
	evs := readLines(t, marshal(t, a))
	dup := evs[0]
	dup.TsRecv, dup.TsEvent = t1, t1
	dup.InstrumentID = 9999
	if _, err := Snapshot(append(evs, dup), t1, time.Minute); !errors.Is(err, ErrInstrumentDrift) {
		t.Fatalf("expected snapshot drift, got %v", err)
	}
	// Distinct symbols may use distinct IDs.
	c := validMap(t1, t1, "A")
	c["symbol"] = testSymbolPut
	c["hd"] = map[string]any{
		"ts_event": fmtNano(t1), "rtype": 177, "publisher_id": 1, "instrument_id": 7777,
	}
	evs = readLines(t, marshal(t, a), marshal(t, c))
	out := mustSnapshot(t, evs, t1, time.Minute)
	if len(out) != 2 {
		t.Fatalf("expected 2 symbols, got %d", len(out))
	}
}

func TestRejectsMalformedRequiredFields(t *testing.T) {
	t0 := baseTime()
	cases := map[string]func(map[string]any){
		"omit size":        func(m map[string]any) { delete(m, "size") },
		"null size":        func(m map[string]any) { m["size"] = nil },
		"omit flags":       func(m map[string]any) { delete(m, "flags") },
		"omit ts_in_delta": func(m map[string]any) { delete(m, "ts_in_delta") },
		"missing hd":       func(m map[string]any) { delete(m, "hd") },
		"null hd":          func(m map[string]any) { m["hd"] = nil },
		"bad rtype":        func(m map[string]any) { m["hd"].(map[string]any)["rtype"] = 176 },
		"missing rtype":    func(m map[string]any) { delete(m["hd"].(map[string]any), "rtype") },
		"zero instrument":  func(m map[string]any) { m["hd"].(map[string]any)["instrument_id"] = 0 },
		"missing symbol":   func(m map[string]any) { delete(m, "symbol") },
		"short symbol":     func(m map[string]any) { m["symbol"] = "NFLX" },
		"unknown action":   func(m map[string]any) { m["action"] = "X" },
		"missing side":     func(m map[string]any) { delete(m, "side") },
		"empty price":      func(m map[string]any) { m["price"] = "" },
		"bad price":        func(m map[string]any) { m["price"] = "1.2.3" },
		"missing levels":   func(m map[string]any) { delete(m, "levels") },
		"empty levels":     func(m map[string]any) { m["levels"] = []any{} },
		"two levels": func(m map[string]any) {
			m["levels"] = []any{
				map[string]any{"bid_px": "1400000000", "ask_px": "1600000000", "bid_sz": 1, "ask_sz": 1, "bid_pb": 1, "ask_pb": 1},
				map[string]any{"bid_px": "1390000000", "ask_px": "1610000000", "bid_sz": 1, "ask_sz": 1, "bid_pb": 1, "ask_pb": 1},
			}
		},
		"null bid_px":   func(m map[string]any) { m["levels"].([]any)[0].(map[string]any)["bid_px"] = nil },
		"omit bid_sz":   func(m map[string]any) { delete(m["levels"].([]any)[0].(map[string]any), "bid_sz") },
		"negative size": func(m map[string]any) { m["levels"].([]any)[0].(map[string]any)["bid_sz"] = -1 },
		"event after recv": func(m map[string]any) {
			m["hd"].(map[string]any)["ts_event"] = fmtNano(t0.Add(time.Second))
		},
		"bad ts_recv": func(m map[string]any) { m["ts_recv"] = "not-a-time" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := validMap(t0, t0, "A")
			mutate(m)
			if _, err := ReadCMBP(strings.NewReader(marshal(t, m) + "\n")); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestSentinelInvalidates(t *testing.T) {
	t0 := baseTime()
	m := validMap(t0, t0, "A")
	m["levels"] = []any{map[string]any{
		"bid_px": UndefinedPriceRaw, "ask_px": "1600000000",
		"bid_sz": 10, "ask_sz": 12, "bid_pb": 1, "ask_pb": 1,
	}}
	evs := readLines(t, marshal(t, m))
	if !evs[0].Book.BidUndefined {
		t.Fatal("sentinel flag not preserved")
	}
	out := mustSnapshot(t, evs, t0, time.Minute)
	obs := out[testSymbol]
	if obs.Status != StatusInvalid || obs.InvalidReason != ReasonUndefinedPrice {
		t.Fatalf("sentinel should invalidate: %+v", obs)
	}
	if obs.BidPxNano != 0 || obs.AskPxNano != 0 {
		t.Fatalf("sentinel leaked guessed price: %+v", obs)
	}
	// Empty price is missing, also invalid without a record error.
	m2 := validMap(t0, t0, "A")
	m2["levels"] = []any{map[string]any{
		"bid_px": "", "ask_px": "1600000000",
		"bid_sz": 10, "ask_sz": 12, "bid_pb": 1, "ask_pb": 1,
	}}
	evs2 := readLines(t, marshal(t, m2))
	out2 := mustSnapshot(t, evs2, t0, time.Minute)
	if out2[testSymbol].InvalidReason != ReasonMissingPrice {
		t.Fatalf("empty should be missing_price: %+v", out2[testSymbol])
	}
}

func TestOverflowRejected(t *testing.T) {
	t0 := baseTime()
	// A wire nanodollar value above int64 must fail.
	m := validMap(t0, t0, "A")
	m["levels"] = []any{map[string]any{
		"bid_px": "9223372037000000000", "ask_px": "1600000000",
		"bid_sz": 10, "ask_sz": 12, "bid_pb": 1, "ask_pb": 1,
	}}
	if _, err := ReadCMBP(strings.NewReader(marshal(t, m) + "\n")); err == nil {
		t.Fatal("expected overflow error")
	}
	// Decimal dollar text is not the unpretty integer wire format.
	m2 := validMap(t0, t0, "A")
	m2["levels"] = []any{map[string]any{
		"bid_px": "1.0000000001", "ask_px": "1600000000",
		"bid_sz": 10, "ask_sz": 12, "bid_pb": 1, "ask_pb": 1,
	}}
	if _, err := ReadCMBP(strings.NewReader(marshal(t, m2) + "\n")); err == nil {
		t.Fatal("expected inexact-precision error")
	}
}

func TestWireUnitsAndAmbiguity(t *testing.T) {
	t0 := baseTime()
	valid := marshal(t, validMap(t0, t0, "A"))
	ev := readLines(t, valid)[0]
	if ev.PriceNano != 1_500_000_000 || ev.Book.BidPxNano != 1_400_000_000 || !ev.TsRecv.Equal(t0) {
		t.Fatal("integer nanodollar/nanosecond wire values were rescaled")
	}
	for _, input := range []string{
		strings.Replace(valid, `"flags":192`, `"flags":0,"flags":192`, 1),
		strings.Replace(valid, `"flags":192`, `"FLAGS":0,"flags":192`, 1),
		strings.Replace(valid, `"bid_px":"1400000000"`, `"bid_px":"1.40"`, 1),
		strings.Replace(valid, `"ts_recv":"`+fmtNano(t0)+`"`, `"ts_recv":"18446744073709551615"`, 1),
	} {
		if _, err := ReadCMBP(strings.NewReader(input)); err == nil {
			t.Fatal("ambiguous or wrong wire encoding accepted")
		}
	}
	for _, mutate := range []func(map[string]any){
		func(m map[string]any) { m["hd"].(map[string]any)["instrument_id"] = int64(1 << 32) },
		func(m map[string]any) { m["size"] = int64(1 << 32) },
		func(m map[string]any) { m["symbol"] = "NFLX  260410X00100000" },
	} {
		m := validMap(t0, t0, "A")
		mutate(m)
		if _, err := ReadCMBP(strings.NewReader(marshal(t, m))); err == nil {
			t.Fatal("invalid wire identity or integer width accepted")
		}
	}
	evs := readLines(t, valid)
	evs[0].Book.BidPxNano++
	if _, err := Snapshot(evs, t0, time.Minute); err == nil {
		t.Fatal("mutated parsed price bypassed wire validation")
	}
}

func TestUnsupportedFlagsRejected(t *testing.T) {
	t0 := baseTime()
	m := validMap(t0, t0, "A")
	m["flags"] = 0
	if _, err := ReadCMBP(strings.NewReader(marshal(t, m) + "\n")); !errors.Is(err, ErrUnqualifiedFlags) {
		t.Fatalf("expected unqualified flags, got %v", err)
	}
	evs := readLines(t, marshal(t, validMap(t0, t0, "A")))
	evs[0].Flags = 193
	if _, err := Snapshot(evs, t0, time.Minute); !errors.Is(err, ErrUnqualifiedFlags) {
		t.Fatalf("expected snapshot unqualified flags, got %v", err)
	}
}

func TestBoundsEnforced(t *testing.T) {
	t0 := baseTime()
	// Record limit.
	var sb strings.Builder
	for i := range MaxEvents + 1 {
		ts := t0.Add(time.Duration(i) * time.Millisecond)
		sb.WriteString(marshal(t, validMap(ts, ts, "A")))
		sb.WriteString("\n")
	}
	if _, err := ReadCMBP(strings.NewReader(sb.String())); !errors.Is(err, ErrTooManyRecords) {
		t.Fatalf("expected too-many-records, got %v", err)
	}
	// Line limit.
	if _, err := ReadCMBP(strings.NewReader(strings.Repeat("x", MaxLineBytes+1) + "\n")); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("expected line-too-long, got %v", err)
	}
	// Locked quotes stay valid.
	m := validMap(t0, t0, "A")
	m["levels"] = []any{map[string]any{
		"bid_px": "1500000000", "ask_px": "1500000000",
		"bid_sz": 5, "ask_sz": 5, "bid_pb": 1, "ask_pb": 1,
	}}
	out := mustSnapshot(t, readLines(t, marshal(t, m)), t0, time.Minute)
	if out[testSymbol].Status != StatusFresh {
		t.Fatalf("locked should be valid fresh: %+v", out[testSymbol])
	}
	// Trade-only symbols create no observation.
	tr := validMap(t0, t0, "T")
	tr["side"] = "N"
	out = mustSnapshot(t, readLines(t, marshal(t, tr)), t0, time.Minute)
	if len(out) != 0 {
		t.Fatalf("trade must not create observation: %v", out)
	}
	// Future malformed events still fail closed even though excluded.
	good := validMap(t0, t0, "A")
	evs, err := ReadCMBP(strings.NewReader(marshal(t, good) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	_ = evs
	manual := readLines(t, marshal(t, good))
	futureBad := manual[0]
	futureBad.TsRecv = t0.Add(time.Hour)
	futureBad.TsEvent = t0.Add(time.Hour)
	futureBad.Action = "X"
	if _, err := Snapshot(append(manual, futureBad), t0, time.Minute); err == nil {
		t.Fatal("future malformed should fail closed")
	}
}
