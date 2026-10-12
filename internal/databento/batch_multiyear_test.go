package databento

import "testing"

func TestMultiyearAcquisitionScopeDoesNotWidenFrozenStudy(t *testing.T) {
	q := BatchQuery{Study: "calendar-2024-2025-v1", Dataset: "OPRA.PILLAR", Schema: "definition", Symbols: []string{"NVDA.OPT", "IREN.OPT"}, StypeIn: "parent", Start: "2024-01-02T00:00:00Z", End: "2024-01-02T14:35:00Z"}
	if q.Validate() != nil {
		t.Fatal("explicit historical scope rejected")
	}
	legacy := q
	legacy.Study = ""
	if legacy.Validate() == nil {
		t.Fatal("frozen 2026 allowlist silently widened")
	}
	official := BatchQuery{Study: q.Study, Dataset: "EQUS.SUMMARY", Schema: "ohlcv-1d", Symbols: []string{"NVDA", "NOW"}, StypeIn: "raw_symbol", Start: "2024-07-01T00:00:00Z", End: "2024-09-01T00:00:00Z"}
	if official.Validate() != nil {
		t.Fatal("bounded available official-close range rejected")
	}
	official.Start = "2024-06-01T00:00:00Z"
	if official.Validate() == nil {
		t.Fatal("unavailable official-close history admitted")
	}
	for _, change := range []func(*BatchQuery){
		func(q *BatchQuery) { q.Study = "unknown" },
		func(q *BatchQuery) { q.Symbols = []string{"TSLA.OPT"} },
		func(q *BatchQuery) { q.Start = "2023-12-31T00:00:00Z" },
		func(q *BatchQuery) { q.End = "2024-01-03T14:35:00Z" },
		func(q *BatchQuery) { q.Schema = "cmbp-1" }, // Broad parent tick purchase remains prohibited.
	} {
		bad := q
		change(&bad)
		if bad.Validate() == nil {
			t.Fatal("out-of-scope acquisition admitted")
		}
	}
}
