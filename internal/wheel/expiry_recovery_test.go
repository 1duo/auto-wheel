package wheel

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMissingOptionWaitsForExpiry(t *testing.T) {
	for _, kind := range []string{"put", "call"} {
		for _, expiry := range []string{"future", "expired", "missing", "invalid"} {
			t.Run(kind+"/"+expiry, func(t *testing.T) {
				_, st, s, _, now := fixture()
				date := "2026-10-09"
				switch expiry {
				case "expired":
					date = "2026-10-02"
				case "missing":
					date = ""
				case "invalid":
					date = "2026-02-30"
				}
				p := Position{PutID: "own-put", PutExpiry: date, PutStrike: 20000, PutOpenedAt: now.Add(-72 * time.Hour), MissingSince: now.Add(-25 * time.Hour), Provenance: "own fill"}
				if kind == "call" {
					p = Position{OwnedShares: 100, Cost: 20000, InventoryAt: now.Add(-72 * time.Hour), CallID: "own-call", CallExpiry: date, MissingSince: now.Add(-25 * time.Hour), Provenance: "own assignment"}
					s.Shares = []Shares{{Symbol: "NVDA", Quantity: 100, Available: 100, Cost: 20000}}
				}
				st.Positions["NVDA"] = p
				err := Reconcile(&st, s, now)
				if expiry == "invalid" || expiry == "missing" {
					if err == nil || st.Positions["NVDA"] != p {
						t.Fatal("unknown expiry did not preserve exposure", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if expiry == "future" {
					if st.Positions["NVDA"] != p {
						t.Fatal("pre-expiry absence released collateral or coverage")
					}
				} else if kind == "put" {
					if len(st.Positions) != 0 {
						t.Fatal("expired put was not released after settlement hold")
					}
				} else if st.Positions["NVDA"].CallID != "" || st.Positions["NVDA"].OwnedShares != 100 {
					t.Fatal("expired call did not retain owned inventory")
				}
			})
		}
	}
}

func TestFilledContractPersistsExpiryAcrossRestart(t *testing.T) {
	for _, kind := range []string{"put", "call"} {
		t.Run(kind, func(t *testing.T) {
			_, st, s, q, now := fixture()
			q.Kind = kind
			if kind == "call" {
				st.Positions[q.Symbol] = Position{OwnedShares: 100, Cost: q.Strike, InventoryAt: now.Add(-time.Hour), Provenance: "own assignment"}
				s.Shares = []Shares{{Symbol: q.Symbol, Quantity: 100, Available: 100, Cost: q.Strike}}
			}
			st.Intents[q.Symbol] = Intent{Ref: "own-ref", Candidate: Candidate{Quote: q}, At: now}
			s.Orders = []Order{{ID: "own-order", Ref: "own-ref", Symbol: q.Symbol, OptionID: q.ID, Side: "sell", Effect: "open", Quantity: 1, Filled: 1, State: "filled"}}
			s.Options = []Option{q.Option}
			s.Options[0].Quantity = 1
			if err := Reconcile(&st, s, now); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(st)
			if err != nil {
				t.Fatal(err)
			}
			var restored State
			if err := json.Unmarshal(raw, &restored); err != nil {
				t.Fatal(err)
			}
			if err := restored.Validate(); err != nil {
				t.Fatal(err)
			}
			p := restored.Positions[q.Symbol]
			if kind == "put" && p.PutExpiry != q.Expiry || kind == "call" && p.CallExpiry != q.Expiry {
				t.Fatal("own fill expiry was lost on restart")
			}
		})
	}
}

func TestExpiryFenceUsesEndOfEasternDay(t *testing.T) {
	for _, date := range []string{"2026-03-08", "2026-10-09", "2026-11-01"} {
		end, err := time.ParseInLocation("2006-01-02", date, Eastern)
		if err != nil {
			t.Fatal(err)
		}
		end = end.AddDate(0, 0, 1)
		if elapsed, err := expiryDayElapsed(date, end.Add(-time.Nanosecond)); err != nil || elapsed {
			t.Fatal("expiration day still open", date, err)
		}
		if elapsed, err := expiryDayElapsed(date, end); err != nil || !elapsed {
			t.Fatal("expiration date boundary incorrect", date, err)
		}
	}
}
