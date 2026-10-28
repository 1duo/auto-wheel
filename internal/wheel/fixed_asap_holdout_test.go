package wheel

import (
	"encoding/json"
	"testing"
	"time"
)

// These adversarial checks are separate from the cadence-selection panel and
// execution happy paths. They inspect forged durable state and calendar edges.
func TestASAPHoldoutDurableEvidenceCannotBeForged(t *testing.T) {
	for _, scenario := range []string{"ref", "deadline", "future_signal", "identity", "price", "roster", "six_slots", "old_policy", "unclaimed_phase", "future_scan"} {
		t.Run(scenario, func(t *testing.T) {
			c, st, _, config, b, now := asapFixture(t)
			if err := c.ExecuteASAP(t.Context(), config, b, b.evidence.Market, func() time.Time { return *now }); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(st)
			var forged State
			if err := json.Unmarshal(raw, &forged); err != nil {
				t.Fatal(err)
			}
			key := asapKey("MSFT", *now)
			a := forged.Fixed.Replenishment.Attempts[key]
			switch scenario {
			case "ref":
				a.Ref = "another-reference"
			case "deadline":
				a.Deadline = a.Deadline.Add(time.Minute)
			case "future_signal":
				a.Signal.ObservedAt = a.At.Add(time.Second)
			case "identity":
				a.Signal.Contract.ID = "other-contract"
			case "price":
				a.Signal.Option.Ask = 0
			case "roster":
				a.RosterSHA = ""
			case "six_slots":
				forged.Fixed.Spec.Limits.Slots = 6
				forged.Fixed.PolicySHA = forged.Fixed.Spec.Digest()
			case "old_policy":
				forged.Fixed.Spec.ID = FixedConservativeRestingTP75Policy
				forged.Fixed.Spec.Limits.Slots = 0
				forged.Fixed.PolicySHA = forged.Fixed.Spec.Digest()
			case "unclaimed_phase":
				a.Signal.Phase = "ready"
			case "future_scan":
				forged.Fixed.Replenishment.LastScanAt = forged.Fixed.Replenishment.Since.Add(-time.Second)
				forged.Fixed.Replenishment.LastCapacitySHA = a.RosterSHA
			}
			forged.Fixed.Replenishment.Attempts[key] = a
			if forged.Validate() == nil {
				t.Fatal("forged ASAP journal accepted", scenario)
			}
		})
	}
}

func TestASAPHoldoutCalendarAndNativeLimits(t *testing.T) {
	for _, tc := range []struct {
		day          string
		hour, minute int
		want         bool
	}{{"2026-10-20", 9, 34, false}, {"2026-10-20", 9, 35, true}, {"2026-10-20", 15, 44, true}, {"2026-10-20", 15, 45, false}, {"2026-11-27", 12, 44, true}, {"2026-11-27", 12, 45, false}, {"2026-11-26", 10, 0, false}, {"2026-10-24", 10, 0, false}, {"2029-10-22", 10, 0, false}} {
		day, _ := time.ParseInLocation(time.DateOnly, tc.day, Eastern)
		at := day.Add(time.Duration(tc.hour)*time.Hour + time.Duration(tc.minute)*time.Minute)
		if _, open := ASAPEntryDeadline(at); open != tc.want {
			t.Fatal("calendar edge changed", tc)
		}
	}
	c, st, _, config, b, now := asapFixture(t)
	if err := c.ExecuteASAP(t.Context(), config, b, b.evidence.Market, func() time.Time { return *now }); err != nil {
		t.Fatal(err)
	}
	asapFill(t, c, config, b, st, "MSFT")
	raw, _ := json.Marshal(st)
	var forged State
	_ = json.Unmarshal(raw, &forged)
	delete(forged.Fixed.Replenishment.Attempts, asapKey("MSFT", *now))
	if forged.Validate() == nil {
		t.Fatal("new owned put survived without original ASAP claim proof")
	}
	config.MaxSymbols = 6
	if config.Validate() == nil {
		t.Fatal("six-slot configuration accepted")
	}
	config.MaxSymbols = 5
	config.FixedPolicy.Spec.ID = FixedConservativeRestingTP75Policy
	config.FixedPolicy.Spec.Limits.Slots = 0
	if config.Validate() == nil {
		t.Fatal("legacy policy silently acquired five-slot authority")
	}
}

func TestASAPHoldoutInheritedCallAndRestingExit(t *testing.T) {
	for _, call := range []bool{false, true} {
		t.Run(map[bool]string{false: "put", true: "call"}[call], func(t *testing.T) {
			c, config, b := restingTPFixture(t, call)
			config.FixedPolicy.Spec.ID, config.FixedPolicy.Spec.Limits.Slots, config.MaxSymbols = FixedConservativeASAPPolicy, 5, 5
			next, err := EnableFixedASAP(*b.state, config, *b.now)
			if err != nil {
				t.Fatal(err)
			}
			*b.state = next
			c, err = NewFixedController(config.FixedPolicy.Spec, b.state, b.store)
			if err != nil {
				t.Fatal(err)
			}
			if err = c.ExecuteRestingTakeProfit(t.Context(), config, b, "MSFT", func() time.Time { return *b.now }); err != nil || b.submits != 1 {
				t.Fatal("ASAP policy lost its owned resting exit", err)
			}
			b.snapshot.Orders = append(b.snapshot.Orders, workingRestingOrder(b.state.Intents["MSFT"]))
			s, _ := b.Snapshot(t.Context())
			if !RestingTP75OrdersAcknowledged(config.FixedPolicy.Spec.ID, b.state.Intents, s) {
				t.Fatal("ASAP GTC not acknowledged")
			}
		})
	}
}
