package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPinnedReferenceInputsRetainCashAndIntradayClock(t *testing.T) {
	date, err := loadReference("../../research/simulation/2026-10-05/american-reference-20261006.json", dateReferenceSHA, 30)
	if err != nil {
		t.Fatal(err)
	}
	in, err := inputFor(date.Cases[6], "date_aligned", date.Date)
	if err != nil || len(in.Dividends) != 1 || in.Dividends[0].AmountUSD != .5 || in.Dividends[0].ExAt.Format(time.RFC3339Nano) != "2026-04-07T00:00:00Z" || in.Expiry.Sub(in.Now) != 48*time.Hour {
		t.Fatal("date reference lost its explicit cash schedule or tenor", in, err)
	}
	ref, err := loadReference("../../research/simulation/2026-10-05/american-intraday-reference-20261006.json", intradayReferenceSHA, 12)
	if err != nil {
		t.Fatal(err)
	}
	in, err = inputFor(ref.Cases[2], "intraday", "")
	if err != nil || len(in.Dividends) != 1 || in.Dividends[0].AmountUSD != .5 || in.Dividends[0].ExAt.Format(time.RFC3339Nano) != "2026-04-07T13:30:00Z" || in.Now.Format(time.RFC3339Nano) != "2026-04-06T13:35:00Z" || in.Expiry.Format(time.RFC3339Nano) != "2026-04-08T20:00:00Z" {
		t.Fatal("intraday reference was snapped or lost cash timing", in, err)
	}
	ref.Cases[2].CashReal[0].ExAt = "2026-04-07T13:30:00.000000001Z"
	in, err = inputFor(ref.Cases[2], "intraday", "")
	if err != nil || in.Dividends[0].ExAt.Nanosecond() != 1 {
		t.Fatal("nanosecond event changed during the numerical input adapter", in, err)
	}
}

func TestPinnedReferenceRejectsTampering(t *testing.T) {
	raw, err := os.ReadFile("../../research/simulation/2026-10-05/american-reference-20261006.json")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "tampered.json")
	if err := os.WriteFile(p, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadReference(p, dateReferenceSHA, 30); err == nil {
		t.Fatal("unbound numerical reference accepted")
	}
}
