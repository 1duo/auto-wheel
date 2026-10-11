package databento

import "testing"

func TestBatchSummaryBoundedDailyReferences(t *testing.T) {
	q := BatchQuery{Dataset: "EQUS.SUMMARY", Schema: "ohlcv-1d", Symbols: []string{"NVDA", "AMZN"}, StypeIn: "raw_symbol", Start: "2026-08-03T00:00:00Z", End: "2026-10-01T00:00:00Z"}
	if err := q.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*BatchQuery){
		func(q *BatchQuery) { q.Schema = "mbp-1" },
		func(q *BatchQuery) { q.Symbols = []string{"ALL_SYMBOLS"} },
		func(q *BatchQuery) { q.Symbols = []string{"NVDA", "NVDA"} },
		func(q *BatchQuery) { q.StypeIn = "parent" },
		func(q *BatchQuery) { q.Start = "2026-07-01T00:00:00Z" },
		func(q *BatchQuery) { q.Start = "2026-08-03T00:00:00.1Z" },
		func(q *BatchQuery) { q.End = "2026-09-30T20:00:00Z" },
		func(q *BatchQuery) { q.Dataset = "EQUS.MINI" },
	} {
		bad := q
		change(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatalf("accepted widened scope: %+v", bad)
		}
	}
}
