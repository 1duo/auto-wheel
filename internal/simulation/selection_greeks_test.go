package simulation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"autowheel/internal/wheel"
)

func selectionReplay(t *testing.T) (*replay, Frame) {
	t.Helper()
	var spec Spec
	if err := json.NewDecoder(AccountingFixture()).Decode(&spec); err != nil {
		t.Fatal(err)
	}
	r := &replay{report: Report{Spec: spec}}
	if err := r.init(); err != nil {
		t.Fatal(err)
	}
	f := fixtureFrame(r.decisions[0], "decision")
	f.CompleteSymbols = spec.Config.Symbols
	return r, f
}

func TestSelectionGreeksExcludeOnlyProvenNonCandidates(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		r, f := selectionReplay(t)
		for i := range f.Quotes {
			f.Quotes[i].GreeksAt = time.Time{}
			if f.Quotes[i].ID == "fixture-NVDA-put" {
				if invalid {
					f.Quotes[i].At = f.At.Add(-150 * time.Second)
					f.Quotes[i].Bid, f.Quotes[i].Ask, f.Quotes[i].BidSize = 0, 0, 0
				} else {
					f.Quotes[i].GreeksAt = f.At
				}
			}
		}
		if err := r.step(f); err != nil {
			t.Fatal(err)
		}
		want := 1
		if invalid {
			want = 0
		}
		if r.metrics.Submissions != want || len(r.quotes) != len(f.Quotes) {
			t.Fatal("noncandidate Greek exemption changed selection or omitted a roster ID")
		}
	}
}

func TestSelectionGreeksRequireEveryRankingRelevantContract(t *testing.T) {
	for _, tc := range []string{"missing_winner", "stale_winner", "missing_lower_rank", "missing_out_of_band"} {
		t.Run(tc, func(t *testing.T) {
			r, f := selectionReplay(t)
			q := &f.Quotes[0]
			switch tc {
			case "missing_winner":
				q.GreeksAt = time.Time{}
			case "stale_winner":
				q.GreeksAt = f.At.Add(-30*time.Second - time.Nanosecond)
			case "missing_out_of_band":
				q.Delta, q.GreeksAt = -.9, time.Time{}
			case "missing_lower_rank":
				loser := *q
				loser.ID += "-lower"
				loser.Bid, loser.Ask, loser.GreeksAt = 80, 90, time.Time{}
				f.Quotes = append(f.Quotes, loser)
				for i := range f.Chains {
					if f.Chains[i].Symbol == q.Symbol && f.Chains[i].Kind == q.Kind {
						f.Chains[i].Contracts = append(f.Chains[i].Contracts, loser.ID)
					}
				}
			}
			if err := r.step(f); err == nil || !strings.Contains(err.Error(), "selection Greeks") || r.metrics.Submissions != 0 {
				t.Fatal("unverified delta could affect eligibility/ranking")
			}
		})
	}
}

func TestSelectionGreeksDoNotAdvancePastIlliquidNearest(t *testing.T) {
	r, f := selectionReplay(t)
	later := f.Quotes[0]
	later.ID += "-later"
	later.GreeksAt = time.Time{}
	f.Quotes[0].Expiry = "2026-04-09"
	f.Quotes[0].BidSize, f.Quotes[0].GreeksAt = 0, time.Time{}
	for i := range f.Chains {
		if f.Chains[i].Symbol == "NVDA" && f.Chains[i].Kind == "put" {
			f.Chains[i].Expiry = "2026-04-09"
		}
	}
	f.Quotes = append(f.Quotes, later)
	f.Chains = append(f.Chains, Chain{Symbol: "NVDA", Kind: "put", Expiry: later.Expiry, At: f.At, Weekly: true, Contracts: []string{later.ID}})
	if err := r.step(f); err != nil {
		t.Fatal(err)
	}
	if r.metrics.Submissions != 0 {
		t.Fatal("missing Greeks in a later expiry must not trigger an advance")
	}
}

func TestSelectionGreeksCoverBothCallRankingInputs(t *testing.T) {
	for _, id := range []string{"fixture-NVDA-call", "fixture-NVDA-call-larger-premium"} {
		r, f := selectionReplay(t)
		r.now = f.At
		// Isolate decision validation for already verified owned shares.
		r.state.Positions["NVDA"] = wheel.Position{OwnedShares: 100, Cost: 10000, Provenance: "invented call-Greek test"}
		r.shares["NVDA"] = inventory{Cost: 10000, SettledAt: f.At}
		for i := range f.Quotes {
			if f.Quotes[i].ID == id {
				f.Quotes[i].GreeksAt = time.Time{}
			}
			r.quotes[f.Quotes[i].ID] = f.Quotes[i]
		}
		if err := r.decide(f); err == nil || !strings.Contains(err.Error(), "selection Greeks") || r.metrics.Submissions != 0 {
			t.Fatal("missing call delta could change delta-first or premium-tie ranking")
		}
	}
}

func TestSelectionGreeksFutureNonCandidateStillFails(t *testing.T) {
	r, f := selectionReplay(t)
	for i := range f.Quotes {
		if f.Quotes[i].OI == 0 {
			f.Quotes[i].GreeksAt = f.At.Add(time.Nanosecond)
			break
		}
	}
	if err := r.step(f); err == nil || !strings.Contains(err.Error(), "future selection Greeks") {
		t.Fatal("future input must fail even if currently noncandidate")
	}
}

func TestSelectionDeltaGuardMatchesProductionFilters(t *testing.T) {
	_, f := selectionReplay(t)
	c := wheel.DefaultConfig()
	for name, mutate := range map[string]func(*wheel.Quote){
		"valid":           func(q *wheel.Quote) {},
		"nonstandard":     func(q *wheel.Quote) { q.Standard = false },
		"not_tradable":    func(q *wheel.Quote) { q.Tradable = false },
		"low_bid":         func(q *wheel.Quote) { q.Bid = c.MinBid - 1 },
		"zero_size":       func(q *wheel.Quote) { q.BidSize = 0 },
		"low_oi":          func(q *wheel.Quote) { q.OI = c.MinOI - 1 },
		"zero_tick":       func(q *wheel.Quote) { q.Tick = 0 },
		"zero_premium":    func(q *wheel.Quote) { q.Tick = q.Bid + 1 },
		"crossed":         func(q *wheel.Quote) { q.Ask = q.Bid - 1 },
		"wide_spread":     func(q *wheel.Quote) { q.Ask = q.Bid * 2 },
		"stale_book":      func(q *wheel.Quote) { q.At = f.At.Add(-31 * time.Second) },
		"stale_spot":      func(q *wheel.Quote) { q.UnderlyingAt = f.At.Add(-31 * time.Second) },
		"itm_put":         func(q *wheel.Quote) { q.Strike = q.Spot },
		"0dte":            func(q *wheel.Quote) { q.Expiry = "2026-04-06" },
		"too_far":         func(q *wheel.Quote) { q.Expiry = "2026-04-17" },
		"nonweekly_thurs": func(q *wheel.Quote) { q.Expiry, q.Weekly = "2026-04-09", false },
		"weekly_thurs":    func(q *wheel.Quote) { q.Expiry, q.Weekly = "2026-04-09", true },
	} {
		t.Run(name, func(t *testing.T) {
			q := f.Quotes[0].Quote
			mutate(&q)
			quotes := []wheel.Quote{q}
			nearest := nearestSelectionExpiry(c, q.Symbol, q.Kind, quotes, f.At)
			got := selectionUsesDelta(c, q, q.Symbol, q.Kind, nearest, 0, f.At)
			_, want := wheel.Select(c, q.Symbol, q.Kind, quotes, 0, f.At)
			if got != want {
				t.Fatal("delta-validation guard diverged from production filters with a verified in-band delta")
			}
		})
	}
}

func TestReplayRejectsZeroAskMoneyFlows(t *testing.T) {
	r, f := selectionReplay(t)
	q := f.Quotes[0]
	q.Ask, q.Bid, q.BidSize = 0, 0, 0
	r.now = f.At
	r.quotes[q.ID], r.shorts[q.ID] = q, q.Option
	if _, err := r.mark(); err == nil {
		t.Fatal("invalid zero ask erased a held liability")
	}
	before := r.cash
	if err := r.lifecycle(Lifecycle{OptionID: q.ID, Outcome: "forced_closed", SettledAt: f.At, Assumption: "invented invalid-ask check"}); err == nil || r.cash != before {
		t.Fatal("invalid zero ask financed a forced close")
	}
	if _, held := r.shorts[q.ID]; !held {
		t.Fatal("failed close removed held short")
	}
	q.Ask = wheel.Cents(110)
	r.quotes[q.ID] = q
	point, err := r.mark()
	if err != nil || point.OptionLiability != 11_000 {
		t.Fatal("a positive fresh ask must still value the full liability")
	}
}
