package wheel

import (
	"testing"
	"time"
)

func TestReconcileContractIdentity(t *testing.T) {
	for _, phase := range []string{"intent", "put", "call"} {
		for _, fault := range []string{"valid", "kind", "expiry", "strike", "duplicate", "quantity"} {
			if phase == "call" && fault == "strike" {
				continue // Legacy call positions retain kind and expiry, not strike.
			}
			t.Run(phase+"/"+fault, func(t *testing.T) {
				_, st, s, q, now := fixture()
				s.Options = []Option{q.Option}
				s.Options[0].Quantity = 1
				switch phase {
				case "intent":
					st.Intents[q.Symbol] = Intent{Ref: "own-ref", Candidate: Candidate{Quote: q}, At: now}
					s.Orders = []Order{{ID: "own-order", Ref: "own-ref", Symbol: q.Symbol, OptionID: q.ID, Side: "sell", Effect: "open", Quantity: 1, State: "queued"}}
				case "put":
					st.Positions[q.Symbol] = Position{PutID: q.ID, PutStrike: q.Strike, PutExpiry: q.Expiry, PutOpenedAt: now.Add(-time.Hour), Provenance: "own fill"}
				case "call":
					s.Options[0].Kind = "call"
					st.Positions[q.Symbol] = Position{OwnedShares: 100, Cost: q.Strike, InventoryAt: now.Add(-time.Hour), CallID: q.ID, CallExpiry: q.Expiry, Provenance: "own assignment"}
					s.Shares = []Shares{{Symbol: q.Symbol, Quantity: 100, Available: 100, Cost: q.Strike}}
				}
				switch fault {
				case "kind":
					if phase == "call" {
						s.Options[0].Kind = "put"
					} else {
						s.Options[0].Kind = "call"
					}
				case "expiry":
					s.Options[0].Expiry = "2026-10-16"
				case "strike":
					s.Options[0].Strike++
				case "duplicate":
					s.Options = append(s.Options, s.Options[0])
				case "quantity":
					s.Options[0].Quantity = 2
				}
				err := Reconcile(&st, s, now)
				if fault == "valid" && err != nil {
					t.Fatal(err)
				}
				if fault != "valid" && err == nil {
					t.Fatal("ambiguous or changed owned contract accepted")
				}
			})
		}
	}
}
