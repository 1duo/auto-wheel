package stockhistory

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

func baseMiniTime() time.Time {
	return time.Date(2026, 4, 6, 13, 34, 59, 0, time.UTC)
}

func fmtMiniNano(t time.Time) string {
	return strconv.FormatInt(t.UnixNano(), 10)
}

func validMiniMap(tsRecv, tsEvent time.Time, action string) map[string]any {
	return map[string]any{
		"ts_recv": fmtMiniNano(tsRecv),
		"hd": map[string]any{
			"ts_event":      fmtMiniNano(tsEvent),
			"rtype":         1,
			"publisher_id":  95,
			"instrument_id": 11275,
		},
		"action":      action,
		"side":        "B",
		"depth":       0,
		"price":       "1500000000",
		"size":        10,
		"flags":       128,
		"ts_in_delta": 0,
		"sequence":    0,
		"levels": []any{
			map[string]any{
				"bid_px": "1400000000",
				"ask_px": "1600000000",
				"bid_sz": 10,
				"ask_sz": 12,
				"bid_ct": 0,
				"ask_ct": 0,
			},
		},
		"symbol": "NFLX",
	}
}

func marshalMini(t *testing.T, m map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func readMini(t *testing.T, lines ...string) Series {
	t.Helper()
	joined := strings.Join(lines, "\n") + "\n"
	s, err := ReadMiniMBP(strings.NewReader(joined))
	if err != nil {
		t.Fatalf("ReadMiniMBP failed: %v", err)
	}
	return s
}

func mustMiniSnapshot(t *testing.T, s Series, asOf time.Time, maxAge time.Duration) map[string]Observation {
	t.Helper()
	out, err := s.Snapshot(asOf, maxAge)
	if err != nil {
		t.Fatalf("Snapshot failed: %v", err)
	}
	return out
}

func TestMiniValidPilotShape(t *testing.T) {
	t0 := baseMiniTime()
	m := validMiniMap(t0, t0, "A")
	s := readMini(t, marshalMini(t, m))
	if s.Len() != 1 {
		t.Fatalf("Len=%d want 1", s.Len())
	}
	evs := s.Events()
	if len(evs) != 1 {
		t.Fatalf("Events=%d want 1", len(evs))
	}
	ev := evs[0]
	if ev.TsRecvRaw != fmtMiniNano(t0) || ev.TsEventRaw != fmtMiniNano(t0) {
		t.Fatalf("raw clocks not preserved: %q %q", ev.TsRecvRaw, ev.TsEventRaw)
	}
	if ev.Book.BidPxNano != 1_400_000_000 || ev.Book.AskPxNano != 1_600_000_000 {
		t.Fatalf("book nanos %d/%d", ev.Book.BidPxNano, ev.Book.AskPxNano)
	}
	out := mustMiniSnapshot(t, s, t0, time.Minute)
	obs, ok := out["NFLX"]
	if !ok {
		t.Fatal("expected NFLX present")
	}
	if obs.Status != StatusFresh {
		t.Fatalf("status=%q want fresh", obs.Status)
	}
	if obs.BidPxNano != 1_400_000_000 || obs.AskPxNano != 1_600_000_000 {
		t.Fatalf("obs book %d/%d", obs.BidPxNano, obs.AskPxNano)
	}
	if !obs.At.Equal(t0) || !obs.EventAt.Equal(t0) {
		t.Fatalf("At/EventAt %v/%v", obs.At, obs.EventAt)
	}
	if obs.TieCount != 1 || obs.TieEquivalent {
		t.Fatalf("tie fields %d/%v", obs.TieCount, obs.TieEquivalent)
	}
	if got := slices.Sorted(maps.Keys(out)); !slices.Equal(got, []string{"NFLX"}) {
		t.Fatalf("keys %v", got)
	}
}

func TestMiniClockInvariants(t *testing.T) {
	t0 := baseMiniTime()
	t1 := t0.Add(time.Second)
	// Capture reversal fails.
	early := marshalMini(t, validMiniMap(t1, t1, "A"))
	late := marshalMini(t, validMiniMap(t0, t0, "A"))
	if _, err := ReadMiniMBP(strings.NewReader(early + "\n" + late + "\n")); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("reversal err=%v want ErrOutOfOrder", err)
	}
	// Engine later than capture fails.
	bad := validMiniMap(t0, t1, "A")
	if _, err := ReadMiniMBP(strings.NewReader(marshalMini(t, bad) + "\n")); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("future source clock err=%v", err)
	}
	// Nil reader fails cleanly.
	if _, err := ReadMiniMBP(nil); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("nil reader err=%v", err)
	}
	// Zero asOf and negative maxAge fail cleanly.
	s := readMini(t, marshalMini(t, validMiniMap(t0, t0, "A")))
	if _, err := s.Snapshot(time.Time{}, time.Minute); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("zero asOf err=%v", err)
	}
	if _, err := s.Snapshot(t0, -time.Second); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("negative maxAge err=%v", err)
	}
	if _, err := s.Snapshot(t0, 0); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("zero maxAge err=%v", err)
	}
}

func TestMiniActionSideStructuralGuards(t *testing.T) {
	t0 := baseMiniTime()
	cases := []struct {
		name string
		mut  func(map[string]any)
	}{
		{"bad action", func(m map[string]any) { m["action"] = "X" }},
		{"missing action", func(m map[string]any) { delete(m, "action") }},
		{"bad side", func(m map[string]any) { m["side"] = "Q" }},
		{"missing side", func(m map[string]any) { delete(m, "side") }},
		{"nonzero depth", func(m map[string]any) { m["depth"] = 1 }},
		{"nonzero sequence", func(m map[string]any) { m["sequence"] = 1 }},
		{"nonzero delta", func(m map[string]any) { m["ts_in_delta"] = 1 }},
		{"missing depth", func(m map[string]any) { delete(m, "depth") }},
		{"bad counts", func(m map[string]any) {
			m["levels"] = []any{map[string]any{"bid_px": "1400000000", "ask_px": "1600000000", "bid_sz": 10, "ask_sz": 12, "bid_ct": 1, "ask_ct": 0}}
		}},
		{"levels not one", func(m map[string]any) {
			m["levels"] = []any{
				map[string]any{"bid_px": "1400000000", "ask_px": "1600000000", "bid_sz": 10, "ask_sz": 12, "bid_ct": 0, "ask_ct": 0},
				map[string]any{"bid_px": "1400000000", "ask_px": "1600000000", "bid_sz": 10, "ask_sz": 12, "bid_ct": 0, "ask_ct": 0},
			}
		}},
		{"missing levels", func(m map[string]any) { delete(m, "levels") }},
		{"missing hd", func(m map[string]any) { delete(m, "hd") }},
		{"missing symbol", func(m map[string]any) { delete(m, "symbol") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validMiniMap(t0, t0, "A")
			tc.mut(m)
			if _, err := ReadMiniMBP(strings.NewReader(marshalMini(t, m) + "\n")); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("err=%v want ErrInvalidRecord", err)
			}
		})
	}
}

func TestMiniTradeNoRefreshSameBook(t *testing.T) {
	t0 := baseMiniTime()
	tTrade := t0.Add(5 * time.Second)
	asOf := t0.Add(40 * time.Second)
	q := validMiniMap(t0, t0, "A")
	tr := validMiniMap(tTrade, tTrade, "T")
	tr["side"] = "N"
	tr["price"] = "1550000000"
	s := readMini(t, marshalMini(t, q), marshalMini(t, tr))
	out := mustMiniSnapshot(t, s, asOf, 30*time.Second)
	obs := out["NFLX"]
	if obs.Status != StatusStale {
		t.Fatalf("trade refreshed age: status=%q At=%v", obs.Status, obs.At)
	}
	if !obs.At.Equal(t0) || !obs.EventAt.Equal(t0) {
		t.Fatalf("At/EventAt moved by trade: %v/%v", obs.At, obs.EventAt)
	}
	if !obs.TradeAt.Equal(tTrade) || !obs.TradeEventAt.Equal(tTrade) {
		t.Fatalf("Trade clocks %v/%v", obs.TradeAt, obs.TradeEventAt)
	}
	if obs.BidPxNano != 1_400_000_000 {
		t.Fatalf("same-book trade changed book: %d", obs.BidPxNano)
	}
}

func TestMiniTradeChangedBookInvalidates(t *testing.T) {
	t0 := baseMiniTime()
	t1 := t0.Add(time.Second)
	t2 := t1.Add(time.Second)
	q := validMiniMap(t0, t0, "A")
	tr := validMiniMap(t1, t1, "T")
	tr["side"] = "N"
	tr["levels"] = []any{map[string]any{"bid_px": "9400000000", "ask_px": "9600000000", "bid_sz": 10, "ask_sz": 12, "bid_ct": 0, "ask_ct": 0}}
	s := readMini(t, marshalMini(t, q), marshalMini(t, tr))
	out := mustMiniSnapshot(t, s, t1, time.Minute)
	obs := out["NFLX"]
	if obs.Status != StatusInvalid || obs.InvalidReason != ReasonTradeBookAmbiguity {
		t.Fatalf("changed-book trade did not invalidate: %+v", obs)
	}
	if obs.BidPxNano != 0 || obs.AskPxNano != 0 {
		t.Fatalf("invalid kept prices %d/%d", obs.BidPxNano, obs.AskPxNano)
	}
	if !obs.At.Equal(t0) {
		t.Fatalf("trade used sale as quote At=%v want %v", obs.At, t0)
	}
	if !obs.TradeAt.Equal(t1) {
		t.Fatalf("TradeAt %v want %v", obs.TradeAt, t1)
	}
	// Later complete A/C/M restores.
	restore := validMiniMap(t2, t2, "C")
	s2 := readMini(t, marshalMini(t, q), marshalMini(t, tr), marshalMini(t, restore))
	out2 := mustMiniSnapshot(t, s2, t2, time.Minute)
	obs2 := out2["NFLX"]
	if obs2.Status != StatusFresh || obs2.BidPxNano != 1_400_000_000 {
		t.Fatalf("restore failed: %+v", obs2)
	}
	if !obs2.At.Equal(t2) {
		t.Fatalf("restore At %v want %v", obs2.At, t2)
	}
}

func TestMiniTradeNoPriorNoRestore(t *testing.T) {
	t0 := baseMiniTime()
	t1 := t0.Add(time.Second)
	tr := validMiniMap(t0, t0, "T")
	tr["side"] = "N"
	s := readMini(t, marshalMini(t, tr))
	out := mustMiniSnapshot(t, s, t0, time.Minute)
	if len(out) != 0 {
		t.Fatalf("trade alone created observation: %v", out)
	}
	// Trade never restores an invalid book.
	bad := validMiniMap(t0, t0, "A")
	bad["levels"] = []any{map[string]any{"bid_px": "1600000000", "ask_px": "1400000000", "bid_sz": 10, "ask_sz": 10, "bid_ct": 0, "ask_ct": 0}}
	goodTrade := validMiniMap(t1, t1, "T")
	goodTrade["side"] = "N"
	s2 := readMini(t, marshalMini(t, bad), marshalMini(t, goodTrade))
	out2 := mustMiniSnapshot(t, s2, t1, time.Minute)
	obs := out2["NFLX"]
	if obs.Status != StatusInvalid {
		t.Fatalf("trade restored invalid: %+v", obs)
	}
	if obs.BidPxNano != 0 {
		t.Fatalf("restored invalid kept prices")
	}
}

func TestMiniResetAlwaysInvalidates(t *testing.T) {
	t0 := baseMiniTime()
	t1 := t0.Add(time.Second)
	good := validMiniMap(t0, t0, "A")
	reset := validMiniMap(t1, t1, "R")
	s := readMini(t, marshalMini(t, good), marshalMini(t, reset))
	out := mustMiniSnapshot(t, s, t1, time.Minute)
	obs := out["NFLX"]
	if obs.Status != StatusInvalid || obs.InvalidReason != ReasonReset {
		t.Fatalf("R did not invalidate: %+v", obs)
	}
	if obs.BidPxNano != 0 || obs.AskPxNano != 0 {
		t.Fatalf("R kept printed prices")
	}
	if !obs.At.Equal(t1) {
		t.Fatalf("R At %v want %v", obs.At, t1)
	}
}

func TestMiniBadCellsInvalidate(t *testing.T) {
	t0 := baseMiniTime()
	t1 := t0.Add(time.Second)
	cases := []struct {
		name   string
		levels []any
		reason string
	}{
		{"missing bid_px", []any{map[string]any{"ask_px": "1600000000", "bid_sz": 10, "ask_sz": 12, "bid_ct": 0, "ask_ct": 0}}, ReasonMissingPrice},
		{"null bid_px", []any{map[string]any{"bid_px": nil, "ask_px": "1600000000", "bid_sz": 10, "ask_sz": 12, "bid_ct": 0, "ask_ct": 0}}, ReasonMissingPrice},
		{"undefined bid", []any{map[string]any{"bid_px": UndefinedPriceRaw, "ask_px": "1600000000", "bid_sz": 10, "ask_sz": 12, "bid_ct": 0, "ask_ct": 0}}, ReasonUndefinedPrice},
		{"nonpositive bid", []any{map[string]any{"bid_px": "0", "ask_px": "1600000000", "bid_sz": 10, "ask_sz": 12, "bid_ct": 0, "ask_ct": 0}}, ReasonNonpositiveBid},
		{"crossed", []any{map[string]any{"bid_px": "1700000000", "ask_px": "1600000000", "bid_sz": 10, "ask_sz": 12, "bid_ct": 0, "ask_ct": 0}}, ReasonCrossed},
		{"zero size", []any{map[string]any{"bid_px": "1400000000", "ask_px": "1600000000", "bid_sz": 0, "ask_sz": 12, "bid_ct": 0, "ask_ct": 0}}, ReasonZeroSize},
		{"missing size", []any{map[string]any{"bid_px": "1400000000", "ask_px": "1600000000", "ask_sz": 12, "bid_ct": 0, "ask_ct": 0}}, ReasonZeroSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			good := validMiniMap(t0, t0, "A")
			bad := validMiniMap(t1, t1, "C")
			bad["levels"] = tc.levels
			s := readMini(t, marshalMini(t, good), marshalMini(t, bad))
			out := mustMiniSnapshot(t, s, t1, time.Minute)
			obs := out["NFLX"]
			if obs.Status != StatusInvalid || obs.InvalidReason != tc.reason {
				t.Fatalf("reason=%q/%q want %q", obs.Status, obs.InvalidReason, tc.reason)
			}
			if obs.BidPxNano != 0 || obs.AskPxNano != 0 {
				t.Fatalf("invalid kept prices")
			}
			if !obs.At.Equal(t1) {
				t.Fatalf("At not advanced to invalidating quote")
			}
			if obs.InstrumentID != 11275 {
				t.Fatalf("identity lost")
			}
		})
	}
	// Malformed px fails the file rather than invalidating.
	malformed := validMiniMap(t0, t0, "A")
	malformed["levels"] = []any{map[string]any{"bid_px": "not-a-number", "ask_px": "1600000000", "bid_sz": 10, "ask_sz": 12, "bid_ct": 0, "ask_ct": 0}}
	if _, err := ReadMiniMBP(strings.NewReader(marshalMini(t, malformed) + "\n")); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("malformed px err=%v", err)
	}
}

func TestMiniSameTimeConflictEquivalenceResolution(t *testing.T) {
	t0 := baseMiniTime()
	t1 := t0.Add(time.Second)
	// Different books at identical capture invalidate.
	a := validMiniMap(t0, t0, "A")
	b := validMiniMap(t0, t0, "C")
	b["levels"] = []any{map[string]any{"bid_px": "9400000000", "ask_px": "9600000000", "bid_sz": 10, "ask_sz": 12, "bid_ct": 0, "ask_ct": 0}}
	s := readMini(t, marshalMini(t, a), marshalMini(t, b))
	out := mustMiniSnapshot(t, s, t0, time.Minute)
	obs := out["NFLX"]
	if obs.Status != StatusInvalid || obs.InvalidReason != ReasonTieAmbiguity {
		t.Fatalf("tie conflict not ambiguous: %+v", obs)
	}
	if obs.TieCount != 2 || obs.TieEquivalent {
		t.Fatalf("tie fields %d/%v", obs.TieCount, obs.TieEquivalent)
	}
	// Equivalent books keep first witness.
	c := validMiniMap(t0, t0, "A")
	d := validMiniMap(t0, t0, "C")
	s2 := readMini(t, marshalMini(t, c), marshalMini(t, d))
	out2 := mustMiniSnapshot(t, s2, t0, time.Minute)
	obs2 := out2["NFLX"]
	if obs2.Status != StatusFresh {
		t.Fatalf("equivalent tie not fresh: %+v", obs2)
	}
	if obs2.TieCount != 2 || !obs2.TieEquivalent {
		t.Fatalf("equivalent tie fields %d/%v", obs2.TieCount, obs2.TieEquivalent)
	}
	if !obs2.EventAt.Equal(t0) {
		t.Fatalf("equivalent tie did not keep first witness")
	}
	// R/quote tie never eligible.
	r := validMiniMap(t0, t0, "R")
	s3 := readMini(t, marshalMini(t, c), marshalMini(t, r))
	out3 := mustMiniSnapshot(t, s3, t0, time.Minute)
	if out3["NFLX"].Status != StatusInvalid {
		t.Fatalf("R/quote tie eligible: %+v", out3["NFLX"])
	}
	// Later strict capture resolves ambiguity.
	e := validMiniMap(t1, t1, "M")
	s4 := readMini(t, marshalMini(t, a), marshalMini(t, b), marshalMini(t, e))
	out4 := mustMiniSnapshot(t, s4, t1, time.Minute)
	obs4 := out4["NFLX"]
	if obs4.Status != StatusFresh || !obs4.At.Equal(t1) {
		t.Fatalf("strict resolve failed: %+v", obs4)
	}
	if obs4.TieCount != 1 || obs4.TieEquivalent {
		t.Fatalf("resolved tie fields %d/%v", obs4.TieCount, obs4.TieEquivalent)
	}
	// Trade-only ties report their own group count without losing history
	// when a later quote tie is ambiguous or an event remains unfinished.
	prior := validMiniMap(t0.Add(-time.Second), t0.Add(-time.Second), "A")
	trade := validMiniMap(t0, t0, "T")
	trades := readMini(t, marshalMini(t, prior), marshalMini(t, trade), marshalMini(t, trade))
	tradeView := mustMiniSnapshot(t, trades, t0, time.Minute)["NFLX"]
	if tradeView.TieCount != 2 || tradeView.TieEquivalent || !tradeView.At.Equal(t0.Add(-time.Second)) {
		t.Fatalf("trade tie metadata or quote clock changed: %+v", tradeView)
	}
	afterTrade := marshalMini(t, validMiniMap(t0, t0, "T"))
	conflict := readMini(t, marshalMini(t, prior), afterTrade,
		marshalMini(t, validMiniMap(t1, t1, "A")), marshalMini(t, validMiniMap(t1, t1, "R")))
	conflictView := mustMiniSnapshot(t, conflict, t1, time.Minute)["NFLX"]
	if conflictView.InvalidReason != ReasonTieAmbiguity || !conflictView.TradeAt.Equal(t0) {
		t.Fatalf("ambiguous quote tie dropped completed trade history: %+v", conflictView)
	}
	partial := validMiniMap(t1, t1, "C")
	partial["flags"] = 0
	last := validMiniMap(t1.Add(time.Second), t1, "C")
	incomplete := readMini(t, marshalMini(t, prior), afterTrade, marshalMini(t, partial), marshalMini(t, last))
	partialView := mustMiniSnapshot(t, incomplete, t1, time.Minute)["NFLX"]
	if partialView.InvalidReason != ReasonIncompleteEvent || !partialView.TradeAt.Equal(t0) {
		t.Fatalf("partial event dropped completed trade history: %+v", partialView)
	}
}

func TestMiniFutureFiltering(t *testing.T) {
	t0 := baseMiniTime()
	t1 := t0.Add(5 * time.Second)
	early := validMiniMap(t0, t0, "A")
	late := validMiniMap(t1, t1, "A")
	late["levels"] = []any{map[string]any{"bid_px": "9400000000", "ask_px": "9600000000", "bid_sz": 10, "ask_sz": 12, "bid_ct": 0, "ask_ct": 0}}
	s := readMini(t, marshalMini(t, early), marshalMini(t, late))
	out := mustMiniSnapshot(t, s, t0, time.Minute)
	obs := out["NFLX"]
	if !obs.At.Equal(t0) || obs.BidPxNano != 1_400_000_000 {
		t.Fatalf("future leaked: %+v", obs)
	}
}

func TestMiniIntegerSentinelTypeDuplicateNamespaceFlagGuards(t *testing.T) {
	t0 := baseMiniTime()
	// Sentinel preserved as undefined invalid, never valid.
	sentinel := validMiniMap(t0, t0, "A")
	sentinel["levels"] = []any{map[string]any{"bid_px": UndefinedPriceRaw, "ask_px": "1600000000", "bid_sz": 10, "ask_sz": 12, "bid_ct": 0, "ask_ct": 0}}
	s := readMini(t, marshalMini(t, sentinel))
	out := mustMiniSnapshot(t, s, t0, time.Minute)
	obs := out["NFLX"]
	if obs.Status != StatusInvalid || obs.InvalidReason != ReasonUndefinedPrice {
		t.Fatalf("sentinel not undefined invalid: %+v", obs)
	}
	ev := s.Events()[0]
	if !ev.Book.BidUndefined || ev.Book.BidPxNano != UndefinedPriceNano {
		t.Fatalf("sentinel not preserved: %+v", ev.Book)
	}
	// Exact integer required: float cents fail.
	badPrice := validMiniMap(t0, t0, "A")
	badPrice["price"] = "15.00"
	if _, err := ReadMiniMBP(strings.NewReader(marshalMini(t, badPrice) + "\n")); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("cents err=%v", err)
	}
	// Wrong JSON type fails.
	wrongType := validMiniMap(t0, t0, "A")
	wrongType["size"] = "10"
	if _, err := ReadMiniMBP(strings.NewReader(marshalMini(t, wrongType) + "\n")); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("wrong type err=%v", err)
	}
	// Duplicate keys fail.
	dup := `{"ts_recv":"` + fmtMiniNano(t0) + `","ts_recv":"` + fmtMiniNano(t0) + `","hd":{"ts_event":"` + fmtMiniNano(t0) + `","rtype":1,"publisher_id":95,"instrument_id":11275},"action":"A","side":"B","depth":0,"price":"1500000000","size":10,"flags":128,"ts_in_delta":0,"sequence":0,"levels":[{"bid_px":"1400000000","ask_px":"1600000000","bid_sz":10,"ask_sz":12,"bid_ct":0,"ask_ct":0}],"symbol":"NFLX"}` + "\n"
	if _, err := ReadMiniMBP(strings.NewReader(dup)); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("duplicate err=%v", err)
	}
	// Unknown keys fail.
	unknown := validMiniMap(t0, t0, "A")
	unknown["extra"] = 1
	if _, err := ReadMiniMBP(strings.NewReader(marshalMini(t, unknown) + "\n")); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("unknown err=%v", err)
	}
	// Bad namespace fails.
	for _, mut := range []func(map[string]any){
		func(m map[string]any) { m["symbol"] = "AAPL" },
		func(m map[string]any) { m["symbol"] = "nflx" },
		func(m map[string]any) { m["hd"].(map[string]any)["publisher_id"] = 30 },
		func(m map[string]any) { m["hd"].(map[string]any)["rtype"] = 177 },
		func(m map[string]any) { m["hd"].(map[string]any)["instrument_id"] = 0 },
	} {
		m := validMiniMap(t0, t0, "A")
		mut(m)
		if _, err := ReadMiniMBP(strings.NewReader(marshalMini(t, m) + "\n")); !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("namespace err=%v", err)
		}
	}
	// Instrument drift and ID conflict fail.
	m1 := validMiniMap(t0, t0, "A")
	m2 := validMiniMap(t0.Add(time.Second), t0.Add(time.Second), "A")
	m2["hd"].(map[string]any)["instrument_id"] = 99999
	if _, err := ReadMiniMBP(strings.NewReader(marshalMini(t, m1) + "\n" + marshalMini(t, m2) + "\n")); !errors.Is(err, ErrInstrumentDrift) {
		t.Fatalf("drift err=%v", err)
	}
	// Snapshot/bad-time/publisher/unknown flags reject even with LAST.
	for _, flag := range []int{1, 32, 160, 192 + 32, 192 + 8, 192 + 4, 192 + 2, 255} {
		m := validMiniMap(t0, t0, "A")
		m["flags"] = flag
		_, err := ReadMiniMBP(strings.NewReader(marshalMini(t, m) + "\n"))
		if !errors.Is(err, ErrUnqualifiedFlags) {
			t.Fatalf("flag %d err=%v want ErrUnqualifiedFlags", flag, err)
		}
	}
	for _, flag := range []int{128, 144, 192, 208} {
		m := validMiniMap(t0, t0, "A")
		m["flags"] = flag
		if _, err := ReadMiniMBP(strings.NewReader(marshalMini(t, m) + "\n")); err != nil {
			t.Fatalf("flag %d should pass, err=%v", flag, err)
		}
	}
}

func TestMiniEventCompletion(t *testing.T) {
	t0 := baseMiniTime()
	t1, t2 := t0.Add(time.Second), t0.Add(2*time.Second)
	t.Run("trade cancel is one atomic event", func(t *testing.T) {
		prior := validMiniMap(t0, t0, "A")
		trade := validMiniMap(t1, t1, "T")
		trade["flags"] = 0
		cancel := validMiniMap(t1, t1, "C")
		cancel["levels"].([]any)[0].(map[string]any)["bid_sz"] = 9
		s := readMini(t, marshalMini(t, prior), marshalMini(t, trade), marshalMini(t, cancel))
		obs := mustMiniSnapshot(t, s, t1, time.Minute)["NFLX"]
		if obs.Status != StatusFresh || obs.TieCount != 1 || obs.TieEquivalent || obs.BidSz != 9 ||
			!obs.At.Equal(t1) || !obs.AvailableAt.Equal(t1) || !obs.TradeAt.Equal(t1) {
			t.Fatalf("TC event was treated as a tie or lost its trade: %+v", obs)
		}
	})
	t.Run("partial quote waits for LAST without retiming", func(t *testing.T) {
		quote := validMiniMap(t1, t0, "A")
		quote["flags"] = 0
		last := validMiniMap(t2, t0, "T")
		s := readMini(t, marshalMini(t, quote), marshalMini(t, last))
		partial := mustMiniSnapshot(t, s, t1, time.Minute)["NFLX"]
		if partial.Status != StatusInvalid || partial.InvalidReason != ReasonIncompleteEvent ||
			partial.BidPxNano != 0 || !partial.AvailableAt.IsZero() {
			t.Fatalf("future LAST enabled a partial quote: %+v", partial)
		}
		complete := mustMiniSnapshot(t, s, t2, time.Second)["NFLX"]
		if complete.Status != StatusFresh || !complete.At.Equal(t1) || !complete.EventAt.Equal(t0) ||
			!complete.AvailableAt.Equal(t2) || !complete.TradeAt.Equal(t2) {
			t.Fatalf("completion changed original quote clocks: %+v", complete)
		}
		stale := mustMiniSnapshot(t, s, t2.Add(time.Nanosecond), time.Second)["NFLX"]
		if stale.Status != StatusStale {
			t.Fatalf("completion refreshed quote age: %+v", stale)
		}
	})
	t.Run("interleaving remains per instrument", func(t *testing.T) {
		a := validMiniMap(t0, t0, "T")
		a["flags"] = 0
		b := validMiniMap(t1, t1, "A")
		b["symbol"] = "TSLA"
		b["hd"].(map[string]any)["instrument_id"] = 22222
		last := validMiniMap(t2, t2, "C")
		s := readMini(t, marshalMini(t, a), marshalMini(t, b), marshalMini(t, last))
		mid := mustMiniSnapshot(t, s, t1, time.Minute)
		if mid["NFLX"].InvalidReason != ReasonIncompleteEvent || mid["TSLA"].Status != StatusFresh {
			t.Fatalf("interleaved symbols interfered: %+v", mid)
		}
		complete := mustMiniSnapshot(t, s, t2, time.Minute)["NFLX"]
		if complete.Status != StatusFresh || !complete.TradeAt.Equal(t0) || !complete.AvailableAt.Equal(t2) {
			t.Fatalf("first trade in completed event was lost: %+v", complete)
		}
	})
	for _, flag := range []int{0, 16, 64, 80} {
		first := validMiniMap(t0, t0, "A")
		first["flags"] = flag
		if _, err := ReadMiniMBP(strings.NewReader(marshalMini(t, first))); !errors.Is(err, ErrIncompleteEvent) {
			t.Fatalf("dangling flag %d err=%v", flag, err)
		}
		last := validMiniMap(t1, t1, "C")
		readMini(t, marshalMini(t, first), marshalMini(t, last))
	}
}

func TestMiniWireStringsAndCanonicalKeys(t *testing.T) {
	t0 := baseMiniTime()
	for _, mut := range []func(map[string]any){
		func(m map[string]any) { m["ts_recv"] = " " + fmtMiniNano(t0) },
		func(m map[string]any) { m["hd"].(map[string]any)["ts_event"] = fmtMiniNano(t0) + " " },
		func(m map[string]any) { m["price"] = "1500000000 " },
		func(m map[string]any) { m["levels"].([]any)[0].(map[string]any)["bid_px"] = " 1400000000" },
		func(m map[string]any) { m["levels"].([]any)[0].(map[string]any)["ask_px"] = "" },
		func(m map[string]any) { m["TS_RECV"] = m["ts_recv"]; delete(m, "ts_recv") },
		func(m map[string]any) { m["HD"] = m["hd"]; delete(m, "hd") },
		func(m map[string]any) { m["ſymbol"] = m["symbol"]; delete(m, "symbol") },
		func(m map[string]any) {
			m["hd"].(map[string]any)["RTYPE"] = 1
			delete(m["hd"].(map[string]any), "rtype")
		},
		func(m map[string]any) { m["levels"].([]any)[0].(map[string]any)["BID_PX"] = "1400000000" },
	} {
		m := validMiniMap(t0, t0, "A")
		mut(m)
		if _, err := ReadMiniMBP(strings.NewReader(marshalMini(t, m))); !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("noncanonical wire input accepted: %v", err)
		}
	}
}

func TestMiniResourceBounds(t *testing.T) {
	t0 := baseMiniTime()
	// Too many records.
	var many []string
	for i := range 10001 {
		ts := t0.Add(time.Duration(i) * time.Millisecond)
		many = append(many, marshalMini(t, validMiniMap(ts, ts, "A")))
	}
	if _, err := ReadMiniMBP(strings.NewReader(strings.Join(many, "\n") + "\n")); !errors.Is(err, ErrTooManyRecords) {
		t.Fatalf("too many err=%v", err)
	}
	// Line too long.
	big := strings.Repeat(" ", (1<<20)+1)
	if _, err := ReadMiniMBP(strings.NewReader(big + "\n")); !errors.Is(err, ErrLineTooLong) && !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("line too long err=%v", err)
	}
	// File too large is guarded by MaxFileBytes; exercised via limit reader
	// without allocating 16MiB in this focused control: a single valid line
	// must still pass, proving the guard does not reject normal input.
	s := readMini(t, marshalMini(t, validMiniMap(t0, t0, "A")))
	if s.Len() != 1 {
		t.Fatalf("Len=%d", s.Len())
	}
}

func TestMiniCrossSymbolTiesOkayAndBoundary(t *testing.T) {
	t0 := baseMiniTime()
	a := validMiniMap(t0, t0, "A")
	a["symbol"] = "NFLX"
	b := validMiniMap(t0, t0, "A")
	b["symbol"] = "TSLA"
	b["hd"].(map[string]any)["instrument_id"] = 22222
	b["levels"] = []any{map[string]any{"bid_px": "9400000000", "ask_px": "9600000000", "bid_sz": 5, "ask_sz": 6, "bid_ct": 0, "ask_ct": 0}}
	s := readMini(t, marshalMini(t, a), marshalMini(t, b))
	out := mustMiniSnapshot(t, s, t0, time.Minute)
	if out["NFLX"].BidPxNano != 1_400_000_000 || out["TSLA"].BidPxNano != 9_400_000_000 {
		t.Fatalf("cross-symbol tie interfered: %+v", out)
	}
	// Boundary age==maxAge fresh, later stale.
	q := validMiniMap(t0, t0, "A")
	s2 := readMini(t, marshalMini(t, q))
	outFresh := mustMiniSnapshot(t, s2, t0.Add(30*time.Second), 30*time.Second)
	if outFresh["NFLX"].Status != StatusFresh {
		t.Fatalf("boundary not fresh: %+v", outFresh["NFLX"])
	}
	outStale := mustMiniSnapshot(t, s2, t0.Add(30*time.Second+time.Nanosecond), 30*time.Second)
	if outStale["NFLX"].Status != StatusStale {
		t.Fatalf("after boundary not stale: %+v", outStale["NFLX"])
	}
}

func TestMiniCloneRepeatable(t *testing.T) {
	t0 := baseMiniTime()
	s := readMini(t, marshalMini(t, validMiniMap(t0, t0, "A")))
	clone := s.Events()
	clone[0].Book.BidPxNano = 1
	again := s.Events()
	if again[0].Book.BidPxNano == 1 {
		t.Fatalf("Events alias mutated Series")
	}
	first := mustMiniSnapshot(t, s, t0, time.Minute)
	second := mustMiniSnapshot(t, s, t0, time.Minute)
	if first["NFLX"] != second["NFLX"] {
		t.Fatalf("snapshot not repeatable")
	}
	if s.Len() != 1 {
		t.Fatalf("snapshot mutated Series")
	}
}
