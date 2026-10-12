package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAvailablePeriodRejectsMissingOrMixedCalendars(t *testing.T) {
	inputs := []input{{Start: "2026-06-29", End: "2026-07-01", Days: []day{{Date: "2026-06-29"}, {Date: "2026-06-30"}, {Date: "2026-07-01"}}}}
	got, err := clipPortfolioInputs(inputs, "2026-06-29", "2026-06-30")
	if err != nil || len(got[0].Days) != 2 || got[0].End != "2026-06-30" || inputs[0].End != "2026-07-01" || len(inputs[0].Days) != 3 {
		t.Fatal("period clipping lost the frozen boundary or changed original input", err)
	}
	if _, err := clipPortfolioInputs(inputs, "2026-06-28", "2026-06-30"); err == nil {
		t.Fatal("missing requested boundary was silently shortened")
	}
	if _, err := clipPortfolioInputs(append(inputs, input{Days: []day{{Date: "2026-06-29"}}}), "2026-06-29", "2026-06-30"); err == nil {
		t.Fatal("inconsistent symbol calendars admitted")
	}
}

func TestAvailablePresetsCannotBypassCalendarYearActionGates(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "study-v2", "inputs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(input{Schema: "AutoWheelCalendarYearEndpointInputV1", Symbol: "NVDA"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "NVDA.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	err = runPortfolioScope(root, filepath.Join(root, "result.json"), 8_000_000, 15_000_000, true, "", "")
	if err == nil || !strings.Contains(err.Error(), "calendar-year action gates cannot be bypassed") {
		t.Fatal("calendar-year input admitted through the unqualified 2026 path", err)
	}
}
