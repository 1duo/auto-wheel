package quotehistory

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func mustSummarize(t *testing.T, evs []Event, symbol string, from, through time.Time, maxAge time.Duration) IntervalSummary {
	t.Helper()
	out, err := SummarizeInterval(evs, symbol, from, through, maxAge)
	if err != nil {
		t.Fatalf("SummarizeInterval failed: %v", err)
	}
	return out
}

func TestSummarizeQuietValidSeedNoRetiming(t *testing.T) {
	t0 := baseTime()
	from := t0.Add(10 * time.Second)
	through := t0.Add(20 * time.Second)
	evs := readLines(t, marshal(t, validMap(t0, t0, "A")))
	out := mustSummarize(t, evs, testSymbol, from, through, time.Minute)
	if !out.SeedPresent || !out.EndPresent {
		t.Fatalf("expected seed/end present: %+v", out)
	}
	if !out.BooksValid || !out.QuotesFresh {
		t.Fatalf("quiet valid should prove: %+v", out)
	}
	if !out.StartAt.Equal(t0) || !out.EndAt.Equal(t0) {
		t.Fatalf("retimed: StartAt=%v EndAt=%v want %v", out.StartAt, out.EndAt, t0)
	}
	if !out.StartEventAt.Equal(t0) || !out.EndEventAt.Equal(t0) {
		t.Fatalf("event times retimed: %+v", out)
	}
	if out.MinBidNano != 1_400_000_000 || out.MinBidSize != 10 {
		t.Fatalf("minima wrong: %d/%d", out.MinBidNano, out.MinBidSize)
	}
	if out.BookEvents != 0 || out.TradeEvents != 0 {
		t.Fatalf("counts wrong: %+v", out)
	}
	if out.EndObservation.Status != StatusFresh {
		t.Fatalf("end should be fresh: %+v", out.EndObservation)
	}
	if !out.From.Equal(from) || !out.Through.Equal(through) {
		t.Fatalf("bounds not echoed: %+v", out)
	}
}

func TestSummarizeHiddenDipsLatchFalse(t *testing.T) {
	t0 := baseTime()
	t1 := t0.Add(time.Second)
	t2 := t1.Add(time.Second)
	through := t2.Add(5 * time.Second)
	cases := map[string]func(map[string]any){
		"zero size": func(m map[string]any) {
			m["levels"] = []any{map[string]any{
				"bid_px": "1400000000", "ask_px": "1600000000",
				"bid_sz": 0, "ask_sz": 12, "bid_pb": 1, "ask_pb": 1,
			}}
		},
		"missing bid": func(m map[string]any) {
			m["levels"] = []any{map[string]any{
				"bid_px": "", "ask_px": "1600000000",
				"bid_sz": 10, "ask_sz": 12, "bid_pb": 1, "ask_pb": 1,
			}}
		},
		"crossed": func(m map[string]any) {
			m["levels"] = []any{map[string]any{
				"bid_px": "1600000000", "ask_px": "1400000000",
				"bid_sz": 10, "ask_sz": 10, "bid_pb": 1, "ask_pb": 1,
			}}
		},
		"sentinel": func(m map[string]any) {
			m["levels"] = []any{map[string]any{
				"bid_px": UndefinedPriceRaw, "ask_px": "1600000000",
				"bid_sz": 10, "ask_sz": 12, "bid_pb": 1, "ask_pb": 1,
			}}
		},
		"nonpositive bid": func(m map[string]any) {
			m["levels"] = []any{map[string]any{
				"bid_px": "0", "ask_px": "1600000000",
				"bid_sz": 10, "ask_sz": 12, "bid_pb": 1, "ask_pb": 1,
			}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			seed := validMap(t0, t0, "A")
			dip := validMap(t1, t1, "C")
			mutate(dip)
			rec := validMap(t2, t2, "C")
			evs := readLines(t, marshal(t, seed), marshal(t, dip), marshal(t, rec))
			out := mustSummarize(t, evs, testSymbol, t0, through, time.Minute)
			if out.BooksValid || out.QuotesFresh {
				t.Fatalf("dip %s should latch false: %+v", name, out)
			}
			if out.MinBidNano != 0 || out.MinBidSize != 0 {
				t.Fatalf("dip %s minima should be zero: %+v", name, out)
			}
			if out.BookEvents != 2 {
				t.Fatalf("expected 2 book events, got %d", out.BookEvents)
			}
			if !out.EndAt.Equal(t2) {
				t.Fatalf("EndAt=%v want %v", out.EndAt, t2)
			}
			if out.EndObservation.Status != StatusFresh {
				t.Fatalf("recovered end should be fresh: %+v", out.EndObservation)
			}
			if out.EndObservation.BidPxNano != 1_400_000_000 {
				t.Fatalf("end book wrong: %+v", out.EndObservation)
			}
		})
	}
	t.Run("reset", func(t *testing.T) {
		seed := validMap(t0, t0, "A")
		reset := validMap(t1, t1, "R")
		rec := validMap(t2, t2, "C")
		evs := readLines(t, marshal(t, seed), marshal(t, reset), marshal(t, rec))
		out := mustSummarize(t, evs, testSymbol, t0, through, time.Minute)
		if out.BooksValid || out.QuotesFresh {
			t.Fatalf("reset dip should latch false: %+v", out)
		}
		if out.MinBidNano != 0 || out.MinBidSize != 0 {
			t.Fatalf("reset minima should be zero: %+v", out)
		}
		if out.BookEvents != 2 {
			t.Fatalf("expected 2 book events, got %d", out.BookEvents)
		}
		if !out.EndAt.Equal(t2) || out.EndObservation.Status != StatusFresh {
			t.Fatalf("recovered end wrong: %+v", out)
		}
	})
}

func TestSummarizeSameCaptureFileOrder(t *testing.T) {
	t0 := baseTime()
	t1 := t0.Add(time.Second)
	through := t1.Add(5 * time.Second)
	low := validMap(t1, t1, "C")
	low["levels"] = []any{map[string]any{
		"bid_px": "1300000000", "ask_px": "1600000000",
		"bid_sz": 7, "ask_sz": 12, "bid_pb": 1, "ask_pb": 1,
	}}
	evs := readLines(t, marshal(t, validMap(t0, t0, "A")), marshal(t, validMap(t1, t1, "C")), marshal(t, low))
	out := mustSummarize(t, evs, testSymbol, t0, through, time.Minute)
	if !out.BooksValid || !out.QuotesFresh {
		t.Fatalf("same-capture valids should prove: %+v", out)
	}
	if out.BookEvents != 2 {
		t.Fatalf("expected 2 book events, got %d", out.BookEvents)
	}
	if out.MinBidNano != 1_300_000_000 || out.MinBidSize != 7 {
		t.Fatalf("minima should reflect lowest same-capture: %d/%d", out.MinBidNano, out.MinBidSize)
	}
	if !out.EndAt.Equal(t1) {
		t.Fatalf("EndAt=%v want %v", out.EndAt, t1)
	}

	bad := validMap(t1, t1, "C")
	bad["levels"] = []any{map[string]any{
		"bid_px": "1600000000", "ask_px": "1400000000",
		"bid_sz": 10, "ask_sz": 10, "bid_pb": 1, "ask_pb": 1,
	}}
	evs2 := readLines(t, marshal(t, validMap(t0, t0, "A")), marshal(t, validMap(t1, t1, "C")), marshal(t, bad))
	out2 := mustSummarize(t, evs2, testSymbol, t0, through, time.Minute)
	if out2.BooksValid || out2.QuotesFresh {
		t.Fatalf("same-capture invalid should latch false: %+v", out2)
	}
	if out2.MinBidNano != 0 || out2.MinBidSize != 0 {
		t.Fatalf("minima should be zero: %+v", out2)
	}
}

func TestSummarizeTradeOnlyNoSeedUnknown(t *testing.T) {
	t0 := baseTime()
	t1 := t0.Add(time.Second)
	through := t1.Add(5 * time.Second)
	tr := validMap(t1, t1, "T")
	tr["side"] = "N"
	tr["price"] = "1550000000"
	evs := readLines(t, marshal(t, tr))
	out := mustSummarize(t, evs, testSymbol, t0, through, time.Minute)
	if out.SeedPresent || out.EndPresent {
		t.Fatalf("trade-only no seed should be unknown: %+v", out)
	}
	if out.BooksValid || out.QuotesFresh {
		t.Fatalf("proofs should be false: %+v", out)
	}
	if out.MinBidNano != 0 || out.MinBidSize != 0 {
		t.Fatalf("minima should be zero: %+v", out)
	}
	if out.TradeEvents != 1 || out.BookEvents != 0 {
		t.Fatalf("counts wrong: %+v", out)
	}
	if !out.StartAt.IsZero() || !out.EndAt.IsZero() {
		t.Fatalf("times should be zero without seed: %+v", out)
	}
}

func TestSummarizeFreshnessGapRecoveredRemainsFalse(t *testing.T) {
	t0 := baseTime()
	tTrade := t0.Add(20 * time.Second)
	tRec := t0.Add(40 * time.Second)
	through := tRec.Add(5 * time.Second)
	seed := validMap(t0, t0, "A")
	tr := validMap(tTrade, tTrade, "T")
	tr["side"] = "N"
	tr["price"] = "1550000000"
	rec := validMap(tRec, tRec, "C")
	evs := readLines(t, marshal(t, seed), marshal(t, tr), marshal(t, rec))
	out := mustSummarize(t, evs, testSymbol, t0, through, 30*time.Second)
	if !out.BooksValid {
		t.Fatalf("all books valid, BooksValid should stay true: %+v", out)
	}
	if out.QuotesFresh {
		t.Fatalf("40s gap over 30s maxAge should stay false even though end fresh: %+v", out)
	}
	if out.MinBidNano != 1_400_000_000 || out.MinBidSize != 10 {
		t.Fatalf("valid-only minima should stay positive: %d/%d", out.MinBidNano, out.MinBidSize)
	}
	if out.TradeEvents != 1 || out.BookEvents != 1 {
		t.Fatalf("counts wrong: %+v", out)
	}
	if out.EndObservation.Status != StatusFresh {
		t.Fatalf("end should be fresh (5s age): %+v", out.EndObservation)
	}
	if !out.EndAt.Equal(tRec) {
		t.Fatalf("EndAt=%v want %v", out.EndAt, tRec)
	}
}

func TestSummarizeFutureBeyondThroughExcluded(t *testing.T) {
	t0 := baseTime()
	t1 := t0.Add(5 * time.Second)
	tFuture := t0.Add(time.Hour)
	through := t1.Add(5 * time.Second)
	early := validMap(t0, t0, "A")
	mid := validMap(t1, t1, "C")
	mid["levels"] = []any{map[string]any{
		"bid_px": "1500000000", "ask_px": "1600000000",
		"bid_sz": 9, "ask_sz": 12, "bid_pb": 1, "ask_pb": 1,
	}}
	future := validMap(tFuture, tFuture, "C")
	future["levels"] = []any{map[string]any{
		"bid_px": "9400000000", "ask_px": "9600000000",
		"bid_sz": 10, "ask_sz": 12, "bid_pb": 1, "ask_pb": 1,
	}}
	evs := readLines(t, marshal(t, early), marshal(t, mid), marshal(t, future))
	out := mustSummarize(t, evs, testSymbol, t0, through, time.Minute)
	if out.BookEvents != 1 {
		t.Fatalf("future should be excluded, BookEvents=%d", out.BookEvents)
	}
	if !out.EndAt.Equal(t1) {
		t.Fatalf("EndAt=%v want %v (future leaked)", out.EndAt, t1)
	}
	if out.MinBidNano != 1_400_000_000 {
		t.Fatalf("future minima leaked: %d", out.MinBidNano)
	}
	if out.EndObservation.BidPxNano != 1_500_000_000 {
		t.Fatalf("future book leaked: %+v", out.EndObservation)
	}
}

func TestSummarizeFutureMalformedFailsClosed(t *testing.T) {
	t0 := baseTime()
	t1 := t0.Add(time.Second)
	through := t1
	good := readLines(t, marshal(t, validMap(t0, t0, "A")))
	bad := good[0]
	bad.TsRecv = t1.Add(time.Hour)
	bad.TsEvent = t1.Add(time.Hour)
	bad.Action = "X"
	if _, err := SummarizeInterval(append(good, bad), testSymbol, t0, through, time.Minute); err == nil {
		t.Fatal("future malformed should fail closed")
	}
}

func TestSummarizeInvalidSymbolAndBoundsFail(t *testing.T) {
	t0 := baseTime()
	evs := readLines(t, marshal(t, validMap(t0, t0, "A")))
	for _, symbol := range []string{"", "NFLX", "NFLX  260410X00100000", "short"} {
		if _, err := SummarizeInterval(evs, symbol, t0, t0.Add(time.Second), time.Minute); err == nil {
			t.Fatalf("bad symbol %q should fail", symbol)
		}
	}
	if _, err := SummarizeInterval(evs, testSymbol, time.Time{}, t0.Add(time.Second), time.Minute); err == nil {
		t.Fatal("zero from should fail")
	}
	if _, err := SummarizeInterval(evs, testSymbol, t0, time.Time{}, time.Minute); err == nil {
		t.Fatal("zero through should fail")
	}
	if _, err := SummarizeInterval(evs, testSymbol, t0, t0, time.Minute); err == nil {
		t.Fatal("non-positive interval should fail")
	}
	if _, err := SummarizeInterval(evs, testSymbol, t0.Add(time.Second), t0, time.Minute); err == nil {
		t.Fatal("reversed interval should fail")
	}
	if _, err := SummarizeInterval(evs, testSymbol, t0, t0.Add(time.Second), -time.Second); err == nil {
		t.Fatal("negative maxAge should fail")
	}
}

func TestSummarizeExactBoundary(t *testing.T) {
	t0 := baseTime()
	evs := readLines(t, marshal(t, validMap(t0, t0, "A")))
	fresh, err := SummarizeInterval(evs, testSymbol, t0, t0.Add(30*time.Second), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.BooksValid || !fresh.QuotesFresh {
		t.Fatalf("exact 30s should prove: %+v", fresh)
	}
	if fresh.EndObservation.Status != StatusFresh {
		t.Fatalf("end exact should be fresh: %+v", fresh.EndObservation)
	}
	stale, err := SummarizeInterval(evs, testSymbol, t0, t0.Add(30*time.Second).Add(time.Nanosecond), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !stale.BooksValid || stale.QuotesFresh {
		t.Fatalf("30s+1ns should be stale but valid: %+v", stale)
	}
	if stale.EndObservation.Status != StatusStale {
		t.Fatalf("end +1ns should be stale: %+v", stale.EndObservation)
	}

	tRec := t0.Add(30 * time.Second)
	rec := validMap(tRec, tRec, "C")
	evs2 := readLines(t, marshal(t, validMap(t0, t0, "A")), marshal(t, rec))
	mid, err := SummarizeInterval(evs2, testSymbol, t0, tRec.Add(5*time.Second), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !mid.QuotesFresh {
		t.Fatalf("exact intermediate gap should stay fresh: %+v", mid)
	}
	late := t0.Add(30*time.Second + time.Nanosecond)
	recLate := validMap(late, late, "C")
	evs3 := readLines(t, marshal(t, validMap(t0, t0, "A")), marshal(t, recLate))
	midLate, err := SummarizeInterval(evs3, testSymbol, t0, late.Add(5*time.Second), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if midLate.QuotesFresh {
		t.Fatalf("intermediate +1ns should disqualify: %+v", midLate)
	}
}

func TestSummarizeRejectsOversized(t *testing.T) {
	t0 := baseTime()
	evs := make([]Event, 0, MaxEvents+1)
	for i := range MaxEvents + 1 {
		ts := t0.Add(time.Duration(i) * time.Millisecond)
		evs = append(evs, Event{
			Symbol:       testSymbol,
			TsRecv:       ts,
			TsEvent:      ts,
			Rtype:        RTypeCMBP1,
			PublisherID:  1,
			InstrumentID: 4242,
			Action:       ActionAdd,
			Side:         "B",
			PriceNano:    1_500_000_000,
			PriceRaw:     "1500000000",
			Size:         10,
			Flags:        SupportedFlags,
			Book: BookLevel{
				BidPxNano: 1_400_000_000,
				AskPxNano: 1_600_000_000,
				BidSz:     10,
				AskSz:     12,
				BidPb:     1,
				AskPb:     1,
				BidRaw:    "1400000000",
				AskRaw:    "1600000000",
			},
		})
	}
	if _, err := SummarizeInterval(evs, testSymbol, t0, t0.Add(time.Hour), time.Minute); !errors.Is(err, ErrTooManyRecords) {
		t.Fatalf("expected too-many-records, got %v", err)
	}
}

func TestSummarizeRejectsOutOfOrderAndDrift(t *testing.T) {
	t0 := baseTime()
	t1 := t0.Add(time.Second)
	evs := readLines(t, marshal(t, validMap(t0, t0, "A")), marshal(t, validMap(t1, t1, "A")))
	bad := slices.Clone(evs)
	bad[1].TsRecv = t0.Add(-time.Second)
	bad[1].TsEvent = t0.Add(-time.Second)
	if _, err := SummarizeInterval(bad, testSymbol, t0, t1.Add(time.Second), time.Minute); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("expected out-of-order, got %v", err)
	}
	drift := slices.Clone(evs)
	drift[1].InstrumentID = 9999
	drift[1].TsRecv = t1
	drift[1].TsEvent = t1
	if _, err := SummarizeInterval(drift, testSymbol, t0, t1.Add(time.Second), time.Minute); !errors.Is(err, ErrInstrumentDrift) {
		t.Fatalf("expected drift, got %v", err)
	}
}

func TestSummarizeRejectsBadWire(t *testing.T) {
	t0 := baseTime()
	t1 := t0.Add(time.Second)
	base := readLines(t, marshal(t, validMap(t0, t0, "A")), marshal(t, validMap(t1, t1, "A")))
	for name, mutate := range map[string]func(*Event){
		"flags":    func(e *Event) { e.Flags = 193 },
		"rtype":    func(e *Event) { e.Rtype = 176 },
		"action":   func(e *Event) { e.Action = "X" },
		"side":     func(e *Event) { e.Side = "Z" },
		"wire xor": func(e *Event) { e.Book.BidPxNano++ },
	} {
		dup := slices.Clone(base)
		mutate(&dup[1])
		if _, err := SummarizeInterval(dup, testSymbol, t0, t1.Add(time.Second), time.Minute); err == nil {
			t.Fatalf("%s should fail", name)
		}
	}
	if _, err := SummarizeInterval(base, testSymbol, t0, t1.Add(time.Second), time.Minute); err != nil {
		t.Fatalf("valid should pass: %v", err)
	}
}

func TestSummarizeWrongSymbolAbsentUnknown(t *testing.T) {
	t0 := baseTime()
	t1 := t0.Add(time.Second)
	evs := readLines(t, marshal(t, validMap(t0, t0, "A")), marshal(t, validMap(t1, t1, "C")))
	out := mustSummarize(t, evs, testSymbolPut, t0, t1.Add(5*time.Second), time.Minute)
	if out.SeedPresent || out.EndPresent {
		t.Fatalf("absent symbol should be unknown: %+v", out)
	}
	if out.BooksValid || out.QuotesFresh {
		t.Fatalf("absent proofs should be false: %+v", out)
	}
	if out.MinBidNano != 0 || out.MinBidSize != 0 {
		t.Fatalf("absent minima zero: %+v", out)
	}
	if out.BookEvents != 0 || out.TradeEvents != 0 {
		t.Fatalf("absent counts zero: %+v", out)
	}
}

func TestSummarizeTradeDoesNotRefreshTail(t *testing.T) {
	t0 := baseTime()
	tTrade := t0.Add(35 * time.Second)
	through := t0.Add(40 * time.Second)
	q := validMap(t0, t0, "A")
	tr := validMap(tTrade, tTrade, "T")
	tr["side"] = "N"
	tr["price"] = "1550000000"
	evs := readLines(t, marshal(t, q), marshal(t, tr))
	out := mustSummarize(t, evs, testSymbol, t0, through, 30*time.Second)
	if out.QuotesFresh {
		t.Fatalf("trade must not refresh tail: %+v", out)
	}
	if !out.BooksValid {
		t.Fatalf("books should stay valid: %+v", out)
	}
	if !out.EndAt.Equal(t0) {
		t.Fatalf("EndAt moved by trade: %v", out.EndAt)
	}
	if out.TradeEvents != 1 {
		t.Fatalf("TradeEvents=%d want 1", out.TradeEvents)
	}
	if out.EndObservation.Status != StatusStale {
		t.Fatalf("end should be stale: %+v", out.EndObservation)
	}
}
