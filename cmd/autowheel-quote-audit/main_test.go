package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"autowheel/internal/quotehistory"
)

func TestControlIdentityMatchesActualAcquisition(t *testing.T) {
	q := controlQuery{Dataset: "OPRA.PILLAR", Schema: "cmbp-1", Symbols: []string{"NFLX  260410P00081000", "NFLX  260410P00090000"}, StypeIn: "raw_symbol", Start: "2026-04-06T13:30:00Z", End: "2026-04-06T13:40:00Z", Limit: 10000}
	query, operation, err := controlIdentity(q)
	if err != nil || query != "0e233f5fe7deaaea7980515d20cdb9d29c932877c9ffc8891193fa832748598d" || operation != "586e0107462bab2b6d1934462c9eeda50a4a7f961b221b2bbcf0837a150d266f" {
		t.Fatal("canonical identities differ from receipt-bound acquisition")
	}
	for name, change := range map[string]func(*controlQuery){
		"time": func(q *controlQuery) { q.End = "2026-04-06T16:00:00Z" },
		"cap":  func(q *controlQuery) { q.Limit-- },
		"symbol_order": func(q *controlQuery) {
			q.Symbols = []string{q.Symbols[1], q.Symbols[0]}
		},
	} {
		t.Run(name, func(t *testing.T) {
			modified := q
			change(&modified)
			qh, op, err := controlIdentity(modified)
			if err != nil || qh == query || op == operation {
				t.Fatal("changed query retained the receipt identity")
			}
		})
	}
}

func TestQuoteWindowPreservesPrewindowState(t *testing.T) {
	at := time.Date(2026, 4, 6, 13, 35, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, action, price string
		want                quotehistory.Status
	}{
		{"invalid", "A", quotehistory.UndefinedPriceRaw, quotehistory.StatusInvalid},
		{"reset", "R", "1000000000", quotehistory.StatusInvalid},
		{"stale", "A", "1000000000", quotehistory.StatusStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := at.Add(-150 * time.Second)
			source := auditRecord(old, "NFLX  260410P00081000", tc.action, tc.price) +
				auditRecord(at.Add(-31*time.Second), "NFLX  260410P00090000", "A", "1000000000") +
				auditRecord(at.Add(-10*time.Second), "NFLX  260410P00081000", "T", "1000000000") +
				auditRecord(at.Add(-time.Second), "NFLX  260410P00090000", "A", "1000000000") +
				auditRecord(at.Add(time.Second), "NFLX  260410P00081000", "A", "1000000000")
			events, err := quotehistory.ReadCMBP(strings.NewReader(source))
			if err != nil {
				t.Fatal(err)
			}
			seed, window := quoteWindow(events, at.Add(-30*time.Second), at)
			if len(seed) != 2 || len(window) != 2 || seed[0] != events[0] || seed[1] != events[1] {
				t.Fatal("seed must retain original prior book events and exclude trades/future events")
			}
			full, err := quotehistory.Snapshot(events, at, 30*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			unseeded, err := quotehistory.Snapshot(window, at, 30*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			seeded, err := quotehistory.Snapshot(append(seed, window...), at, 30*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if quoteStatesEqual(full, unseeded) || !quoteStatesEqual(full, seeded) {
				t.Fatal("unseeded window must differ; original-state seed must match")
			}
			q := seeded["NFLX  260410P00081000"]
			if q.Status != tc.want || !q.At.Equal(old) || !q.EventAt.Equal(old) {
				t.Fatal("prior quote status and original capture/event time must survive")
			}
		})
	}
}

func TestQuoteWindowBoundaryAndLatestSeed(t *testing.T) {
	at := time.Date(2026, 4, 6, 13, 35, 0, 0, time.UTC)
	start := at.Add(-30 * time.Second)
	source := auditRecord(start.Add(-2*time.Second), "NFLX  260410P00081000", "A", "1000000000") +
		auditRecord(start.Add(-time.Second), "NFLX  260410P00081000", "A", quotehistory.UndefinedPriceRaw) +
		auditRecord(start, "NFLX  260410P00090000", "A", "1000000000") +
		auditRecord(at, "NFLX  260410P00090000", "T", "1000000000")
	events, err := quotehistory.ReadCMBP(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	seed, window := quoteWindow(events, start, at)
	if len(seed) != 1 || seed[0] != events[1] || len(window) != 2 || window[0] != events[2] || window[1] != events[3] {
		t.Fatal("retain only latest prior book; window boundaries must be inclusive")
	}
	state, err := quotehistory.Snapshot(append(seed, window...), at, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	q := state["NFLX  260410P00090000"]
	if q.Status != quotehistory.StatusFresh || !q.At.Equal(start) {
		t.Fatal("trade must not refresh the quote at its exact age boundary")
	}
}

func TestAuditIntervalsDistinguishesInteriorDipFromLowSeed(t *testing.T) {
	at := time.Date(2026, 4, 6, 13, 35, 0, 0, time.UTC)
	const symbol = "NFLX  260410P00090000"
	for _, tc := range []struct {
		name, initialBid, middleBid string
		initialSize, middleSize     int
		lowerThanEnd, interior      bool
	}{
		{"low_seed", "1000000000", "1000000000", 1, 1, true, false},
		{"interior_dip", "1500000000", "1000000000", 5, 1, true, true},
		{"unchanged", "1500000000", "1500000000", 5, 5, false, false},
		{"invalid_then_recovered", "1500000000", quotehistory.UndefinedPriceRaw, 5, 1, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := func(at time.Time, bid string, size int) string {
				return strings.Replace(auditRecord(at, symbol, "A", bid), `"bid_sz":1`, fmt.Sprintf(`"bid_sz":%d`, size), 1)
			}
			source := record(at, tc.initialBid, tc.initialSize) + record(at.Add(500*time.Millisecond), tc.middleBid, tc.middleSize) + record(at.Add(time.Second), "1500000000", 5)
			events, err := quotehistory.ReadCMBP(strings.NewReader(source))
			if err != nil {
				t.Fatal(err)
			}
			checks, err := auditIntervals(events, []string{symbol}, at)
			if err != nil || len(checks) != 30 {
				t.Fatalf("interval audit: %v, count=%d", err, len(checks))
			}
			first := checks[0]
			if !first.EndpointEqual || first.LowerBidThanEndpoint != tc.lowerThanEnd || first.LowerSizeThanEndpoint != tc.lowerThanEnd || first.InteriorBidDip != tc.interior || first.InteriorSizeDip != tc.interior {
				t.Fatalf("wrong boundary interpretation: %+v", first)
			}
		})
	}
}

func auditRecord(at time.Time, symbol, action, bid string) string {
	id := 81
	if symbol == "NFLX  260410P00090000" {
		id = 90
	}
	return fmt.Sprintf(`{"ts_recv":"%d","hd":{"ts_event":"%d","rtype":177,"publisher_id":30,"instrument_id":%d},"action":"%s","side":"N","price":"1000000000","size":1,"flags":192,"ts_in_delta":0,"levels":[{"bid_px":"%s","ask_px":"2000000000","bid_sz":1,"ask_sz":1,"bid_pb":30,"ask_pb":30}],"symbol":"%s"}`+"\n", at.UnixNano(), at.UnixNano(), id, action, bid, symbol)
}
