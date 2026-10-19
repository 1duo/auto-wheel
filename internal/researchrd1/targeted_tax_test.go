package researchrd1

import "testing"

func TestTargetedTaxReserveDefersAssignedPremiumIntoBasis(t *testing.T) {
	s := &ExperimentSource{targetedControls: TargetedControls{TaxReserve: true}}
	for _, e := range []Effect{
		{Action: "sell_put", Symbol: "X", Price: 100, Fee: 65},
		{Action: "resolve", Symbol: "X", Outcome: "assigned", Contract: Contract{Strike: 10000}, Fee: 100},
	} {
		if err := s.observeTargetedRecord(Record{Effect: e}); err != nil {
			t.Fatal(err)
		}
	}
	if s.taxReserveBound != 0 || s.taxPositions["X"].basis != 990165 {
		t.Fatalf("assigned put tax = %d, basis = %d", s.taxReserveBound, s.taxPositions["X"].basis)
	}
	for _, e := range []Effect{
		{Action: "sell_call", Symbol: "X", Price: 50, Fee: 65},
		{Action: "resolve", Symbol: "X", Outcome: "called_away", Contract: Contract{Strike: 10000}, Fee: 100},
	} {
		if err := s.observeTargetedRecord(Record{Effect: e}); err != nil {
			t.Fatal(err)
		}
	}
	if s.taxReserveBound != 7938 {
		t.Fatalf("tax reserve = %d; want 7938", s.taxReserveBound)
	}
	if len(s.taxPositions) != 0 {
		t.Fatal("tax inventory remains after callaway")
	}
}
