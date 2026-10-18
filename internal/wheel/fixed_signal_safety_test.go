package wheel

import (
	"errors"
	"testing"
	"time"
)

func TestFixedSignalBadInputCannotBeRetriedOrTampered(t *testing.T) {
	for _, scenario := range []string{"stale_side", "zero_bid", "nonpositive_budgeted_credit", "wrong_identity", "wrong_tick", "duplicate_menu"} {
		t.Run(scenario, func(t *testing.T) {
			c, st, _, at, roster := fixedQueueFixture(t)
			if err := c.Begin(roster, at); err != nil {
				t.Fatal(err)
			}
			evidence := fixedQueueEvidence("MSFT", "put", at)
			switch scenario {
			case "stale_side":
				evidence.AskAt = at.Add(-31 * time.Second)
			case "zero_bid":
				evidence.Option.Bid = 0
			case "nonpositive_budgeted_credit":
				evidence.Option.Bid, evidence.Option.Ask, evidence.Tick = 1, 1, 1
			case "wrong_identity":
				evidence.Contract.ID = "replacement"
			case "wrong_tick":
				evidence.Tick = 3
			case "duplicate_menu":
				evidence.Menu = append(evidence.Menu, evidence.Menu[0])
			}
			if err := c.Record("MSFT", evidence, at); !errors.Is(err, ErrFixedSignalAbstained) {
				t.Fatal("bad first observation was not durably closed", err)
			}
			if st.Fixed.Batch.Signals[0].Phase != "abstained" {
				t.Fatal("bad observation left another price opportunity")
			}
			if err := c.Record("MSFT", fixedQueueEvidence("MSFT", "put", at), at.Add(time.Second)); err == nil {
				t.Fatal("bad first quote was replaced by a nicer later quote")
			}
		})
	}
	c, st, _, at, roster := fixedQueueFixture(t)
	if err := c.Begin(roster, at); err != nil {
		t.Fatal(err)
	}
	if err := c.Record("MSFT", fixedQueueEvidence("MSFT", "put", at), at); err != nil {
		t.Fatal(err)
	}
	st.Fixed.Batch.Signals[0].Contract.Strike++
	if err := st.Validate(); err == nil {
		t.Fatal("changed signal economics passed its saved digest")
	}
}

func TestFixedSignalUsesOwnAssignmentCeilingWithoutStrikeFallback(t *testing.T) {
	for _, cost := range []Cents{8000, 11000} {
		c, st, _, at, roster := fixedQueueFixture(t)
		if err := c.Begin(roster, at); err != nil {
			t.Fatal(err)
		}
		if err := c.Recover(at.Add(6 * time.Minute)); err != nil {
			t.Fatal(err)
		}
		st.Positions["AMD"] = Position{OwnedShares: 100, Cost: cost, Provenance: "autowheel put fill own-reconciled-ref",
			PutOpenedAt: at.Add(time.Minute), InventoryAt: at.AddDate(0, 0, 2)}
		later := at.AddDate(0, 0, 7)
		if err := c.Begin(roster, later); err != nil {
			t.Fatal(err)
		}
		evidence := fixedQueueEvidence("AMD", "call", later)
		higher := evidence.Contract
		higher.ID, higher.Strike = "higher-premium-substitute", 11500
		evidence.Menu = append(evidence.Menu, higher)
		err := c.Record("AMD", evidence, later)
		entry := st.Fixed.Batch.Signals[0]
		if cost == 11000 {
			if !errors.Is(err, ErrFixedSignalAbstained) || entry.Reason != "cost_floor" || entry.Contract != nil {
				t.Fatal("underwater cost floor selected a higher replacement call", entry, err)
			}
			if st.Positions["AMD"].OwnedShares != 100 {
				t.Fatal("cost floor disposed of owned inventory")
			}
		} else if err != nil || entry.Contract.ID != "AMD-call" || entry.AcquisitionCeiling != cost*100+c.spec.PhysicalFeeBudget || !fixedSHA(entry.BasisSHA) {
			t.Fatal("own stock price plus declared fee bound was not preserved", entry, err)
		}
	}
}
