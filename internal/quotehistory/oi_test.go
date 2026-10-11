package quotehistory

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func oiStatMap(tsRecv, tsEvent time.Time, symbol string, publisher, instrument, quantity, statType int64) map[string]any {
	return map[string]any{
		"ts_recv": fmtNano(tsRecv),
		"hd": map[string]any{
			"ts_event":      fmtNano(tsEvent),
			"rtype":         24,
			"publisher_id":  publisher,
			"instrument_id": instrument,
		},
		"ts_ref":        RefMissingRaw,
		"price":         "0",
		"quantity":      quantity,
		"sequence":      1,
		"ts_in_delta":   0,
		"stat_type":     statType,
		"channel_id":    1,
		"update_action": 1,
		"stat_flags":    0,
		"symbol":        symbol,
	}
}

func oiReadStats(t *testing.T, lines ...string) []StatEvent {
	t.Helper()
	joined := strings.Join(lines, "\n") + "\n"
	evs, err := ReadStatistics(strings.NewReader(joined))
	if err != nil {
		t.Fatalf("ReadStatistics failed: %v", err)
	}
	return evs
}

func oiMustSnapshot(t *testing.T, evs []StatEvent, asOf time.Time) map[string]OpenInterest {
	t.Helper()
	out, err := SnapshotOpenInterest(evs, asOf)
	if err != nil {
		t.Fatalf("SnapshotOpenInterest failed: %v", err)
	}
	return out
}

func TestSnapshotOpenInterestCausalFutureExclusion(t *testing.T) {
	t0 := baseTime()
	t1 := t0.Add(5 * time.Second)
	early := oiStatMap(t0, t0, testSymbol, 1, 4242, 100, 9)
	late := oiStatMap(t1, t1, testSymbol, 1, 4242, 200, 9)
	evs := oiReadStats(t, marshal(t, early), marshal(t, late))
	earlyOut := oiMustSnapshot(t, evs, t0)
	oi, ok := earlyOut[testSymbol]
	if !ok {
		t.Fatal("expected symbol present at t0")
	}
	if oi.Quantity != 100 {
		t.Fatalf("future OI leaked: quantity=%d want 100", oi.Quantity)
	}
	if !oi.At.Equal(t0) || !oi.EventAt.Equal(t0) {
		t.Fatalf("times not original capture: At=%v EventAt=%v", oi.At, oi.EventAt)
	}
	lateOut := oiMustSnapshot(t, evs, t1)
	if lateOut[testSymbol].Quantity != 200 {
		t.Fatalf("later OI not visible at t1: %+v", lateOut[testSymbol])
	}
	if !lateOut[testSymbol].At.Equal(t1) {
		t.Fatalf("At not advanced: %v", lateOut[testSymbol].At)
	}
}

func TestSnapshotOpenInterestFirst1030VsLaterStats(t *testing.T) {
	morning := time.Date(2026, 4, 6, 10, 30, 0, 0, time.UTC)
	afternoon := baseTime()
	oi := oiStatMap(morning, morning, testSymbol, 3, 4242, 50, 9)
	// Later price statistics must not affect morning OI state.
	lines := []string{marshal(t, oi)}
	for _, st := range []int64{1, 4, 5, 6, 7, 8, 11, 12} {
		// OPRA source clocks can lead capture clocks. These ignored rows
		// retain their actual timestamps and cannot retime the OI snapshot.
		m := oiStatMap(afternoon, afternoon.Add(500*time.Microsecond), testSymbol, 3, 4242, 999, st)
		lines = append(lines, marshal(t, m))
	}
	all := oiReadStats(t, lines...)
	if !all[1].TsEvent.Equal(afternoon.Add(500 * time.Microsecond)) {
		t.Fatal("ignored source timestamp was rewritten")
	}
	out := oiMustSnapshot(t, all, afternoon)
	got, ok := out[testSymbol]
	if !ok {
		t.Fatal("morning OI missing after later stats")
	}
	if got.Quantity != 50 {
		t.Fatalf("later non-OI altered OI: %d", got.Quantity)
	}
	if !got.At.Equal(morning) {
		t.Fatalf("At retimed by later stats: %v want %v", got.At, morning)
	}
	if got.Copies != 1 {
		t.Fatalf("Copies=%d want 1", got.Copies)
	}
}

func TestSnapshotOpenInterest18CopyAgreementNonSumming(t *testing.T) {
	t0 := time.Date(2026, 4, 6, 10, 30, 1, 0, time.UTC)
	lines := make([]string, 0, 18)
	for i := range 18 {
		ts := t0.Add(time.Duration(i) * time.Millisecond)
		m := oiStatMap(ts, ts, testSymbol, int64(20+i), 4242, 42, 9)
		lines = append(lines, marshal(t, m))
	}
	evs := oiReadStats(t, lines...)
	asOf := t0.Add(time.Second)
	out := oiMustSnapshot(t, evs, asOf)
	oi, ok := out[testSymbol]
	if !ok {
		t.Fatal("expected symbol present")
	}
	if oi.Quantity != 42 {
		t.Fatalf("copies summed or lost: quantity=%d want 42", oi.Quantity)
	}
	if oi.Copies != 18 {
		t.Fatalf("Copies=%d want 18", oi.Copies)
	}
	wantAt := t0.Add(17 * time.Millisecond)
	if !oi.At.Equal(wantAt) {
		t.Fatalf("At=%v want latest copy %v", oi.At, wantAt)
	}
	if oi.InstrumentID != 4242 {
		t.Fatalf("instrument=%d want 4242", oi.InstrumentID)
	}
}

func TestSnapshotOpenInterestDisagreement(t *testing.T) {
	t0 := baseTime()
	a := oiStatMap(t0, t0, testSymbol, 20, 4242, 10, 9)
	b := oiStatMap(t0.Add(time.Millisecond), t0.Add(time.Millisecond), testSymbol, 21, 4242, 11, 9)
	evs := oiReadStats(t, marshal(t, a), marshal(t, b))
	if _, err := SnapshotOpenInterest(evs, t0.Add(time.Second)); !errors.Is(err, ErrQuantityConflict) {
		t.Fatalf("expected quantity conflict, got %v", err)
	}
}

func TestSnapshotOpenInterestLegitimateZero(t *testing.T) {
	t0 := baseTime()
	m := oiStatMap(t0, t0, testSymbol, 1, 4242, 0, 9)
	evs := oiReadStats(t, marshal(t, m))
	out := oiMustSnapshot(t, evs, t0)
	oi, ok := out[testSymbol]
	if !ok {
		t.Fatal("zero OI missing")
	}
	if oi.Quantity != 0 {
		t.Fatalf("zero OI altered: %d", oi.Quantity)
	}
	if oi.Copies != 1 {
		t.Fatalf("Copies=%d want 1", oi.Copies)
	}
}

func TestReadStatisticsRejectsBadOI(t *testing.T) {
	t0 := baseTime()
	cases := map[string]func(map[string]any){
		"omit quantity":     func(m map[string]any) { delete(m, "quantity") },
		"sentinel quantity": func(m map[string]any) { m["quantity"] = int64(1<<63 - 1) },
		"negative quantity": func(m map[string]any) { m["quantity"] = int64(-1) },
		"omit ts_ref":       func(m map[string]any) { delete(m, "ts_ref") },
		"empty ts_ref":      func(m map[string]any) { m["ts_ref"] = "" },
		"omit price":        func(m map[string]any) { delete(m, "price") },
		"bad price":         func(m map[string]any) { m["price"] = "1.2.3" },
		"omit sequence":     func(m map[string]any) { delete(m, "sequence") },
		"omit stat_type":    func(m map[string]any) { delete(m, "stat_type") },
		"unsupported type":  func(m map[string]any) { m["stat_type"] = int64(99) },
		"zero type":         func(m map[string]any) { m["stat_type"] = int64(0) },
		"delete action":     func(m map[string]any) { m["update_action"] = int64(2) },
		"zero action":       func(m map[string]any) { m["update_action"] = int64(0) },
		"omit action":       func(m map[string]any) { delete(m, "update_action") },
		"omit stat_flags":   func(m map[string]any) { delete(m, "stat_flags") },
		"bad rtype":         func(m map[string]any) { m["hd"].(map[string]any)["rtype"] = 177 },
		"missing hd":        func(m map[string]any) { delete(m, "hd") },
		"bad ts_recv":       func(m map[string]any) { m["ts_recv"] = "not-a-time" },
		"short symbol":      func(m map[string]any) { m["symbol"] = "TEST" },
		"event after recv": func(m map[string]any) {
			m["hd"].(map[string]any)["ts_event"] = fmtNano(t0.Add(time.Second))
		},
		"future ref": func(m map[string]any) {
			m["ts_ref"] = fmtNano(t0.Add(time.Second))
		},
		"nonzero delta": func(m map[string]any) { m["ts_in_delta"] = int64(5) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := oiStatMap(t0, t0, testSymbol, 1, 4242, 7, 9)
			mutate(m)
			if _, err := ReadStatistics(strings.NewReader(marshal(t, m) + "\n")); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
	t.Run("flags nonzero", func(t *testing.T) {
		m := oiStatMap(t0, t0, testSymbol, 1, 4242, 7, 9)
		m["stat_flags"] = int64(1)
		if _, err := ReadStatistics(strings.NewReader(marshal(t, m) + "\n")); !errors.Is(err, ErrUnqualifiedFlags) {
			t.Fatalf("expected unqualified flags, got %v", err)
		}
		// Snapshot must also reject mutated flags rather than retaining stale.
		evs := oiReadStats(t, marshal(t, oiStatMap(t0, t0, testSymbol, 1, 4242, 7, 9)))
		evs[0].StatFlags = 1
		if _, err := SnapshotOpenInterest(evs, t0); !errors.Is(err, ErrUnqualifiedFlags) {
			t.Fatalf("expected snapshot flags error, got %v", err)
		}
	})
	t.Run("duplicate keys", func(t *testing.T) {
		valid := marshal(t, oiStatMap(t0, t0, testSymbol, 1, 4242, 7, 9))
		dup := strings.Replace(valid, `"stat_type":9`, `"stat_type":9,"stat_type":9`, 1)
		if _, err := ReadStatistics(strings.NewReader(dup + "\n")); err == nil {
			t.Fatal("expected duplicate-key error")
		}
		caseDup := strings.Replace(valid, `"quantity":7`, `"QUANTITY":0,"quantity":7`, 1)
		if _, err := ReadStatistics(strings.NewReader(caseDup + "\n")); err == nil {
			t.Fatal("expected case-variant duplicate error")
		}
	})
}

func TestSnapshotOpenInterestRefMissingIsAbsence(t *testing.T) {
	t0 := baseTime()
	m := oiStatMap(t0, t0, testSymbol, 1, 4242, 15, 9)
	evs := oiReadStats(t, marshal(t, m))
	if evs[0].HasRef {
		t.Fatal("sentinel ts_ref should be absence")
	}
	if !evs[0].RefTime.IsZero() {
		t.Fatalf("missing ref should be zero time: %v", evs[0].RefTime)
	}
	out := oiMustSnapshot(t, evs, t0)
	oi := out[testSymbol]
	// Capture is known-at; missing ref never invents a historical as-of.
	if !oi.At.Equal(t0) {
		t.Fatalf("At=%v want capture %v", oi.At, t0)
	}
	// Present ref at event time is valid and still known-at capture.
	m2 := oiStatMap(t0, t0, testSymbol, 1, 4242, 15, 9)
	m2["ts_ref"] = fmtNano(t0)
	evs2 := oiReadStats(t, marshal(t, m2))
	if !evs2[0].HasRef || !evs2[0].RefTime.Equal(t0) {
		t.Fatalf("present ref not preserved: %+v", evs2[0])
	}
	out2 := oiMustSnapshot(t, evs2, t0)
	if !out2[testSymbol].At.Equal(t0) {
		t.Fatalf("At moved by present ref: %v", out2[testSymbol].At)
	}
}

func TestSnapshotOpenInterestGlobalOrderDriftAndBound(t *testing.T) {
	t0 := baseTime()
	t1 := t0.Add(time.Second)
	t.Run("out of order with non-OI", func(t *testing.T) {
		a := marshal(t, oiStatMap(t1, t1, testSymbol, 1, 4242, 5, 9))
		b := marshal(t, oiStatMap(t0, t0, testSymbol, 1, 4242, 5, 1))
		if _, err := ReadStatistics(strings.NewReader(a + "\n" + b + "\n")); !errors.Is(err, ErrOutOfOrder) {
			t.Fatalf("expected out-of-order, got %v", err)
		}
		// Caller-built out-of-order slice must fail even when the bad row
		// would be excluded by asOf in a naive filter-first pass.
		ordered := oiReadStats(t, marshal(t, oiStatMap(t0, t0, testSymbol, 1, 4242, 5, 9)), marshal(t, oiStatMap(t1, t1, testSymbol, 1, 4242, 6, 9)))
		mut := slices.Clone(ordered)
		mut[1].TsRecv = t0.Add(-time.Second)
		mut[1].TsEvent = t0.Add(-time.Second)
		if _, err := SnapshotOpenInterest(mut, t1); !errors.Is(err, ErrOutOfOrder) {
			t.Fatalf("expected snapshot out-of-order, got %v", err)
		}
	})
	t.Run("drift across OI and non-OI", func(t *testing.T) {
		a := oiStatMap(t0, t0, testSymbol, 1, 4242, 5, 9)
		b := oiStatMap(t1, t1, testSymbol, 1, 9999, 5, 1)
		if _, err := ReadStatistics(strings.NewReader(marshal(t, a) + "\n" + marshal(t, b) + "\n")); !errors.Is(err, ErrInstrumentDrift) {
			t.Fatalf("expected drift, got %v", err)
		}
		evs := oiReadStats(t, marshal(t, a))
		dup := evs[0]
		dup.TsRecv, dup.TsEvent = t1, t1
		dup.InstrumentID = 9999
		dup.StatType = 1
		if _, err := SnapshotOpenInterest(append(evs, dup), t1); !errors.Is(err, ErrInstrumentDrift) {
			t.Fatalf("expected snapshot drift, got %v", err)
		}
	})
	t.Run("non-OI counts toward bound", func(t *testing.T) {
		var sb strings.Builder
		for i := range MaxEvents + 1 {
			ts := t0.Add(time.Duration(i) * time.Millisecond)
			st := int64(9)
			if i%2 == 1 {
				st = 1
			}
			sb.WriteString(marshal(t, oiStatMap(ts, ts, testSymbol, 1, 4242, 5, st)))
			sb.WriteString("\n")
		}
		if _, err := ReadStatistics(strings.NewReader(sb.String())); !errors.Is(err, ErrTooManyRecords) {
			t.Fatalf("expected too-many-records, got %v", err)
		}
	})
	t.Run("future drift fails closed", func(t *testing.T) {
		good := oiReadStats(t, marshal(t, oiStatMap(t0, t0, testSymbol, 1, 4242, 5, 9)))
		futureBad := good[0]
		futureBad.TsRecv = t1.Add(time.Hour)
		futureBad.TsEvent = t1.Add(time.Hour)
		futureBad.InstrumentID = 9999
		if _, err := SnapshotOpenInterest(append(good, futureBad), t0); !errors.Is(err, ErrInstrumentDrift) {
			t.Fatalf("expected future drift to fail closed, got %v", err)
		}
	})
}

func TestSnapshotOpenInterestSameCaptureFileOrder(t *testing.T) {
	t0 := baseTime()
	first := oiStatMap(t0, t0, testSymbol, 1, 4242, 100, 9)
	second := oiStatMap(t0, t0, testSymbol, 1, 4242, 200, 9)
	evs := oiReadStats(t, marshal(t, first), marshal(t, second))
	out := oiMustSnapshot(t, evs, t0)
	if out[testSymbol].Quantity != 200 {
		t.Fatalf("same-capture last file order should win: %d", out[testSymbol].Quantity)
	}
	// Equal captures across publishers are allowed.
	a := oiStatMap(t0, t0, testSymbol, 1, 4242, 50, 9)
	b := oiStatMap(t0, t0, testSymbol, 2, 4242, 50, 9)
	evs2 := oiReadStats(t, marshal(t, a), marshal(t, b))
	out2 := oiMustSnapshot(t, evs2, t0)
	if out2[testSymbol].Copies != 2 || out2[testSymbol].Quantity != 50 {
		t.Fatalf("same-time agreeing copies should pass: %+v", out2[testSymbol])
	}
}

func TestStatisticsRejectsIdentityAliasingAndWireWidth(t *testing.T) {
	at := baseTime()
	initial := oiStatMap(at, at, testSymbol, 20, 4242, 100, 9)
	alias := oiStatMap(at.Add(time.Second), at.Add(time.Second), "NFLX  260410P00090000", 20, 4242, 100, 9)
	if _, err := ReadStatistics(strings.NewReader(marshal(t, initial) + "\n" + marshal(t, alias))); !errors.Is(err, ErrInstrumentDrift) {
		t.Fatalf("ambiguous instrument identity accepted: %v", err)
	}
	for field, value := range map[string]int64{"channel_id": 1 << 16, "update_action": 1 << 8, "stat_flags": -1} {
		t.Run(field, func(t *testing.T) {
			row := oiStatMap(at, at, testSymbol, 20, 4242, QuantityMissingSentinel, 1)
			row[field] = value
			if _, err := ReadStatistics(strings.NewReader(marshal(t, row))); err == nil {
				t.Fatal("non-OI row bypassed its wire-width bound")
			}
		})
	}
	events := oiReadStats(t, marshal(t, initial))
	mutated := events[0]
	mutated.Symbol = "NFLX  260410P00090000"
	mutated.TsRecv, mutated.TsEvent = at.Add(time.Second), at.Add(time.Second)
	if _, err := SnapshotOpenInterest(append(events, mutated), at); !errors.Is(err, ErrInstrumentDrift) {
		t.Fatalf("future identity alias bypassed snapshot validation: %v", err)
	}
}

func TestSnapshotOpenInterestMissingAbsentAndEmptyAsOf(t *testing.T) {
	t0 := baseTime()
	evs := oiReadStats(t, marshal(t, oiStatMap(t0, t0, testSymbol, 1, 4242, 9, 9)))
	out := oiMustSnapshot(t, evs, t0)
	if _, ok := out[testSymbolPut]; ok {
		t.Fatal("missing symbol should stay absent")
	}
	if _, err := SnapshotOpenInterest(evs, time.Time{}); err == nil {
		t.Fatal("zero asOf should fail")
	}
	// Trade-only analogue: non-OI-only symbols create no OI state.
	nonOI := oiReadStats(t, marshal(t, oiStatMap(t0, t0, testSymbolPut, 1, 7777, 0, 1)))
	out2 := oiMustSnapshot(t, nonOI, t0)
	if len(out2) != 0 {
		t.Fatalf("non-OI must not create OI state: %v", out2)
	}
	// Wire-mutated price bypass must fail.
	mut := slices.Clone(evs)
	mut[0].PriceNano++
	if _, err := SnapshotOpenInterest(mut, t0); err == nil {
		t.Fatal("mutated price should fail wire validation")
	}
}
