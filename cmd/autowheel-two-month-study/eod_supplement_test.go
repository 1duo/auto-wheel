package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEODSupplementBindsActualResponse(t *testing.T) {
	observation := eodObservation{Date: "2026-03-02", Contract: "PLTR  260306C00134000", Ask: 1205}
	response := `{"response":[{"contract":{"symbol":"PLTR","expiration":"2026-03-06","right":"CALL","strike":134.000},"data":[{"created":"2026-03-02T17:15:22.733","ask":12.05,"bid":11.90,"ask_size":17,"bid_size":20,"ask_condition":50,"bid_condition":50}]}]}`
	if ask, err := validateEODSupplement(observation, json.RawMessage(response)); err != nil || ask != observation.Ask {
		t.Fatal("valid EOD response rejected", err)
	}
	// A short liability uses the observed ask. A known empty bid is valid EOD
	// valuation data and does not claim that a sell order could execute.
	zeroBid := strings.Replace(strings.Replace(response, `"bid":11.90`, `"bid":0`, 1), `"bid_size":20`, `"bid_size":0`, 1)
	if _, err := validateEODSupplement(observation, json.RawMessage(zeroBid)); err != nil {
		t.Fatal("known empty bid invalidated an observed ask", err)
	}
	for _, replacement := range [][2]string{
		{`"ask":12.05`, `"ask":0`},
		{`"ask":12.05`, `"ask":12.06`},
		{`"bid":11.90`, `"bid":13.00`},
		{`"symbol":"PLTR"`, `"symbol":"NVDA"`},
		{`"right":"CALL"`, `"right":"PUT"`},
		{`"strike":134.000`, `"strike":135.000`},
		{`"expiration":"2026-03-06"`, `"expiration":"2026-03-13"`},
		{`"created":"2026-03-02T17:15:22.733"`, `"created":"2026-03-02T15:15:22.733"`},
		{`"created":"2026-03-02T17:15:22.733"`, `"created":"2026-03-03T17:15:22.733"`},
		{`"ask_size":17`, `"ask_size":0`},
		{`"bid_condition":50`, `"bid_condition":51`},
	} {
		if _, err := validateEODSupplement(observation, json.RawMessage(strings.Replace(response, replacement[0], replacement[1], 1))); err == nil {
			t.Fatal("EOD response mismatch accepted", replacement)
		}
	}
}

func TestPortfolioRejectsAmbiguousDividendFacts(t *testing.T) {
	in := fixture()
	f := portfolioDividend{ID: "own-dividend", Symbol: in.Symbol, Amount: "0.25", ExDate: "2026-08-03", PayDate: "2026-08-14"}
	if _, err := simulatePortfolio([]input{in}, baseline("conservative"), 8_000_000, 6, 0, 8_000_000, []portfolioDividend{f, f}, nil); err == nil {
		t.Fatal("duplicate dividend was counted twice")
	}
	for _, amount := range []string{"1/4", "2.5e-1", "NaN", "-0.25"} {
		f.Amount = amount
		if _, err := portfolioDividendCents(f, 100); err == nil {
			t.Fatal("non-decimal dividend accepted", amount)
		}
	}
}
