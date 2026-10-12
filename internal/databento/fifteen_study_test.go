package databento

import (
	"fmt"
	"testing"
)

func TestFifteenStudyScope(t *testing.T) {
	for _, symbol := range []string{"NBIS", "RKLB", "COIN", "MSTR", "HOOD"} {
		for _, scope := range [][3]string{{"OPRA.PILLAR", "definition", "parent"}, {"OPRA.PILLAR", "cmbp-1", "raw_symbol"}, {"EQUS.MINI", "mbp-1", "raw_symbol"}, {"EQUS.SUMMARY", "ohlcv-1d", "raw_symbol"}} {
			q := BatchQuery{Study: "fifteen-high-iv-2026-v1", Dataset: scope[0], Schema: scope[1], StypeIn: scope[2], Symbols: []string{symbol}, Start: "2026-01-05T00:00:00Z", End: "2026-01-06T00:00:00Z"}
			if q.Dataset == "OPRA.PILLAR" {
				if q.StypeIn == "parent" {
					q.Symbols[0] += ".OPT"
				} else {
					q.Symbols[0] = fmt.Sprintf("%-6s260109P00100000", symbol)
				}
			}
			if err := q.Validate(); err != nil {
				t.Fatalf("%s/%s: %v", symbol, q.Schema, err)
			}
			for _, study := range []string{"", "calendar-2024-2025-v1", "unknown"} {
				bad := q
				bad.Study = study
				if bad.Validate() == nil {
					t.Fatalf("scope escaped its named study: %s/%s", symbol, study)
				}
			}
			bad := q
			bad.End = "2026-04-01T00:00:00Z"
			if bad.Validate() == nil {
				t.Fatal("unbounded research slice accepted")
			}
			bad = q
			bad.Start, bad.End = "2026-10-10T00:00:00Z", "2026-10-11T00:00:00Z"
			if bad.Validate() == nil {
				t.Fatal("date outside frozen coverage accepted")
			}
		}
	}
	q := BatchQuery{Study: "fifteen-high-iv-2026-v1", Dataset: "EQUS.SUMMARY", Schema: "ohlcv-1d", StypeIn: "raw_symbol", Symbols: []string{"NVDA"}, Start: "2026-01-01T00:00:00Z", End: "2026-03-01T00:00:00Z"}
	if q.Validate() == nil {
		t.Fatal("unrelated symbol accepted in added-symbol study")
	}
}
