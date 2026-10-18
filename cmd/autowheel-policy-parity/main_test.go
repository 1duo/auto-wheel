package main

import (
	"testing"

	"autowheel/internal/contractpolicy"
)

func TestAdmissionParityFixtureFraming(t *testing.T) {
	for _, f := range []fixture{
		{Check: "entry_book", Stock: contractpolicy.Book{Bid: 37, Ask: 43}, Allowed: new(true)},
		{Check: "entry_book", Stock: contractpolicy.Book{Bid: 100, Ask: 117}, Allowed: new(false)},
		{Check: "call_floor", Floor: &floorFixture{Strike: 10000, Acquisition: 999901, DeliveryFee: 100}, Allowed: new(false), Error: new(false)},
		{Check: "call_floor", Floor: &floorFixture{Strike: 10000}, Allowed: new(false), Error: new(true)},
		{Check: "calendar", Day: "2026-09-08", Signal: new("2026-09-08T15:29:00-04:00")},
		{Check: "calendar", Day: "2026-01-02", Signal: new("")},
	} {
		if err := compare(f); err != nil {
			t.Fatal("valid parity fixture rejected", f.Check, err)
		}
	}
	for _, f := range []fixture{{}, {Check: "entry_book"}, {Check: "capital"}, {Check: "call_floor"}, {Check: "calendar"}, {Check: "unknown"},
		{Check: "entry_book", Stock: contractpolicy.Book{Bid: 100, Ask: 117}, Allowed: new(true)},
		{Check: "calendar", Day: "2026-01-02", Signal: new("2026-01-02T15:29:00-05:00")}} {
		if err := compare(f); err == nil {
			t.Fatal("missing expectation or mismatch admitted", f.Check)
		}
	}
}
