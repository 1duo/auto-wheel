package simulation

import (
	"strings"
	"testing"
	"time"

	"autowheel/internal/wheel"
)

// Helpers for bounded no-recent-book observations. These synthetic fixtures
// carry no price, size, delta, OI or quote timestamp.

func exclusionObservation(at time.Time, maxAgeSeconds int, id string) NoRecentBook {
	return NoRecentBook{
		OptionID:     id,
		From:         at.Add(-time.Duration(maxAgeSeconds) * time.Second),
		Through:      at,
		SourceSHA256: strings.Repeat("a", 64),
		Complete:     true,
	}
}

// excludeIDs removes the listed IDs from f.Quotes and adds validated
// no-recent-book observations. f.Chains is left intact so the roster stays
// complete; each listed ID then has exactly one real quote OR one exclusion.
func excludeIDs(t *testing.T, r *replay, f *Frame, ids []string) {
	t.Helper()
	keep := f.Quotes[:0]
	excluding := map[string]bool{}
	for _, id := range ids {
		excluding[id] = true
	}
	for _, q := range f.Quotes {
		if !excluding[q.ID] {
			keep = append(keep, q)
		}
	}
	f.Quotes = keep
	for _, id := range ids {
		f.NoRecentBook = append(f.NoRecentBook, exclusionObservation(f.At, r.report.Spec.Config.MaxQuoteAgeSeconds, id))
	}
}

func TestSelectionExclusionAllExcludedSkips(t *testing.T) {
	r, f := selectionReplay(t)
	var puts []string
	for _, q := range f.Quotes {
		if q.Kind == "put" {
			puts = append(puts, q.ID)
		}
	}
	excludeIDs(t, r, &f, puts)
	if err := r.step(f); err != nil {
		t.Fatalf("all-excluded nearest should skip, not block: %v", err)
	}
	if r.metrics.Submissions != 0 {
		t.Fatal("all-excluded nearest must skip without submitting")
	}
	if len(r.selectionStubs) != len(puts) {
		t.Fatal("transient adapters missing for excluded IDs")
	}
	for id, stub := range r.selectionStubs {
		if stub.Standard || stub.Tradable || !stub.At.IsZero() || !stub.UnderlyingAt.IsZero() || stub.Bid != 0 || stub.Ask != 0 {
			t.Fatalf("adapter for %s fabricated market fields", id)
		}
		if _, ok := r.quotes[id]; ok {
			t.Fatalf("excluded ID %s leaked into book storage", id)
		}
		if _, ok := r.catalog[id]; ok {
			t.Fatalf("excluded ID %s leaked into catalog", id)
		}
	}
}

func TestSelectionExclusionNearestPreventsLaterAdvance(t *testing.T) {
	r, f := selectionReplay(t)
	base := f.Quotes[0]
	if base.ID != "fixture-NVDA-put" {
		t.Fatalf("unexpected first fixture quote %s", base.ID)
	}
	// Build a later fresh expiry alongside the nearest. Nearest uses Thursday
	// weekly eligibility; later keeps the original Friday expiry.
	later := base
	later.ID += "-later"
	later.GreeksAt = f.At
	f.Quotes[0].Expiry = "2026-04-09"
	// Keep the nearest quote fresh in the copy before excluding it; the
	// exclusion itself proves it had no fresh book without fabricating one.
	f.Quotes = append(f.Quotes, later)
	for i := range f.Chains {
		if f.Chains[i].Symbol == "NVDA" && f.Chains[i].Kind == "put" {
			f.Chains[i].Expiry = "2026-04-09"
		}
	}
	f.Chains = append(f.Chains, Chain{Symbol: "NVDA", Kind: "put", Expiry: later.Expiry, At: f.At, Weekly: later.Weekly, Contracts: []string{later.ID}})
	excludeIDs(t, r, &f, []string{base.ID})
	if err := r.step(f); err != nil {
		t.Fatalf("excluded nearest should skip, not block: %v", err)
	}
	if r.metrics.Submissions != 0 {
		t.Fatal("excluded nearest must prevent a later fresh expiry trade")
	}
}

func TestSelectionExclusionRealCandidateStillSelected(t *testing.T) {
	r, f := selectionReplay(t)
	// AMD puts are already ineligible on OI; excluding one must not change
	// the fresh NVDA winner.
	excludeIDs(t, r, &f, []string{"fixture-AMD-put"})
	if err := r.step(f); err != nil {
		t.Fatal(err)
	}
	if r.metrics.Submissions != 1 {
		t.Fatalf("real current candidate lost: submissions=%d", r.metrics.Submissions)
	}
	if r.pending == nil || r.pending.Candidate.ID != "fixture-NVDA-put" {
		t.Fatalf("wrong candidate survived exclusion: %+v", r.pending)
	}
}

func TestSelectionExclusionWindowHashGuards(t *testing.T) {
	cases := map[string]func(*Frame, *replay){
		"exact_left_boundary": func(f *Frame, r *replay) {
			f.NoRecentBook[0].From = f.NoRecentBook[0].From.Add(-time.Nanosecond)
		},
		"exact_through": func(f *Frame, r *replay) {
			f.NoRecentBook[0].Through = f.At.Add(-time.Nanosecond)
		},
		"future_through": func(f *Frame, r *replay) {
			f.NoRecentBook[0].Through = f.At.Add(time.Second)
			f.NoRecentBook[0].From = f.At.Add(time.Second).Add(-30 * time.Second)
		},
		"missing_id": func(f *Frame, r *replay) {
			f.NoRecentBook[0].OptionID = ""
		},
		"missing_window": func(f *Frame, r *replay) {
			f.NoRecentBook[0].From = time.Time{}
		},
		"incomplete": func(f *Frame, r *replay) {
			f.NoRecentBook[0].Complete = false
		},
		"bad_hash_short": func(f *Frame, r *replay) {
			f.NoRecentBook[0].SourceSHA256 = "abc"
		},
		"bad_hash_upper": func(f *Frame, r *replay) {
			f.NoRecentBook[0].SourceSHA256 = strings.Repeat("A", 64)
		},
		"bad_hash_nibbles": func(f *Frame, r *replay) {
			f.NoRecentBook[0].SourceSHA256 = strings.Repeat("z", 64)
		},
		"duplicate": func(f *Frame, r *replay) {
			f.NoRecentBook = append(f.NoRecentBook, f.NoRecentBook[0])
		},
		"overlap": func(f *Frame, r *replay) {
			// Do not remove the real quote; exclusion overlaps it.
			f.NoRecentBook = []NoRecentBook{exclusionObservation(f.At, r.report.Spec.Config.MaxQuoteAgeSeconds, f.Quotes[0].ID)}
		},
		"not_listed": func(f *Frame, r *replay) {
			f.NoRecentBook[0].OptionID = "absent-roster-id"
		},
		"missing_unproved": func(f *Frame, r *replay) {
			// Remove a quote without proving it excluded.
			f.Quotes = f.Quotes[1:]
			f.NoRecentBook = nil
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r, f := selectionReplay(t)
			if name != "overlap" && name != "missing_unproved" {
				excludeIDs(t, r, &f, []string{"fixture-AMD-put"})
			} else if name == "overlap" {
				// mutate builds the overlapping observation itself.
			} else {
				// missing_unproved removes without exclusion; nothing to add.
			}
			mutate(&f, r)
			if err := r.step(f); err == nil {
				t.Fatal("malformed exclusion accepted")
			} else if r.metrics.Submissions != 0 {
				t.Fatal("malformed exclusion submitted")
			}
		})
	}
}

func TestSelectionExclusionHeldPendingNonDecisionRejected(t *testing.T) {
	t.Run("held_short", func(t *testing.T) {
		r, f := selectionReplay(t)
		q := f.Quotes[0]
		r.shorts[q.ID] = q.Option
		excludeIDs(t, r, &f, []string{q.ID})
		if err := r.step(f); err == nil {
			t.Fatal("exclusion covered a held short")
		}
	})
	t.Run("held_position", func(t *testing.T) {
		r, f := selectionReplay(t)
		r.state.Positions["NVDA"] = wheel.Position{PutID: "fixture-NVDA-put", PutStrike: 10000, Provenance: "held-short test"}
		excludeIDs(t, r, &f, []string{"fixture-NVDA-put"})
		if err := r.step(f); err == nil {
			t.Fatal("exclusion covered a held position ID")
		}
	})
	t.Run("pending", func(t *testing.T) {
		r, f := selectionReplay(t)
		// Establish a pending order for the fixture winner, then attempt to
		// exclude that same ID on the next scheduled decision.
		if err := r.step(f); err != nil {
			t.Fatal(err)
		}
		if r.pending == nil {
			t.Fatal("fixture did not submit")
		}
		pendingID := r.pending.Candidate.ID
		next := fixtureFrame(r.decisions[1], "decision")
		next.CompleteSymbols = r.report.Spec.Config.Symbols
		excludeIDs(t, r, &next, []string{pendingID})
		// Keep continuous execution coverage so the scope gate is reached:
		// the pending order still blocks the exclusion.
		r.pending.LastQuote = next.At.Add(-time.Second)
		if err := r.step(next); err == nil {
			t.Fatal("exclusion covered a pending order ID")
		}
	})
	t.Run("non_decision", func(t *testing.T) {
		r, f := selectionReplay(t)
		if err := r.step(f); err != nil {
			t.Fatal(err)
		}
		market := fixtureFrame(f.At.Add(time.Second), "market")
		market.NoRecentBook = []NoRecentBook{exclusionObservation(market.At, r.report.Spec.Config.MaxQuoteAgeSeconds, "fixture-AMD-put")}
		if err := r.step(market); err == nil {
			t.Fatal("non-decision exclusion accepted")
		}
	})
}

func TestSelectionExclusionNoFakeFieldsLeaked(t *testing.T) {
	r, f := selectionReplay(t)
	excludeIDs(t, r, &f, []string{"fixture-AMD-put", "fixture-IREN-put"})
	beforeCash := r.cash
	if err := r.step(f); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"fixture-AMD-put", "fixture-IREN-put"} {
		if _, ok := r.quotes[id]; ok {
			t.Fatalf("excluded %s in book storage", id)
		}
		if _, ok := r.catalog[id]; ok {
			t.Fatalf("excluded %s in catalog", id)
		}
		stub, ok := r.selectionStubs[id]
		if !ok {
			t.Fatalf("missing adapter for %s", id)
		}
		if stub.Standard || stub.Tradable || !stub.At.IsZero() || stub.Bid != 0 || stub.Ask != 0 || stub.Spot != 0 || stub.OI != 0 || stub.BidSize != 0 {
			t.Fatalf("adapter for %s carries fake market fields", id)
		}
		for _, trade := range r.report.Trades {
			if trade.OptionID == id {
				t.Fatalf("excluded %s entered money flow", id)
			}
		}
	}
	if r.cash != beforeCash {
		t.Fatal("exclusion moved cash without a fill")
	}
	// Exclusions cannot value held shorts or finance forced closes: a held
	// short without a fresh real ask still blocks valuation.
	held := f.Quotes[0]
	held.Bid, held.Ask, held.BidSize = 0, 0, 0
	r.quotes[held.ID] = held
	r.shorts[held.ID] = held.Option
	if _, err := r.mark(); err == nil {
		t.Fatal("exclusion or zero book valued a held short")
	}
	delete(r.shorts, held.ID)
}

func TestSelectionExclusionIdentityDriftRejected(t *testing.T) {
	r, f := selectionReplay(t)
	excludeIDs(t, r, &f, []string{"fixture-NVDA-put"})
	if err := r.step(f); err != nil {
		t.Fatal(err)
	}
	next := fixtureFrame(r.decisions[1], "decision")
	next.CompleteSymbols = r.report.Spec.Config.Symbols
	// Same ID reappears as a real quote but claimed under a different expiry
	// roster scope; the persistent roster identity must reject reuse.
	for i := range next.Quotes {
		if next.Quotes[i].ID == "fixture-NVDA-put" {
			next.Quotes[i].Expiry = "2026-04-11"
		}
	}
	for i := range next.Chains {
		if next.Chains[i].Symbol == "NVDA" && next.Chains[i].Kind == "put" {
			next.Chains[i].Expiry = "2026-04-11"
		}
	}
	if err := r.step(next); err == nil {
		t.Fatal("first-exclusion identity drift accepted")
	}
}

func TestSelectionExclusionPreSubmitStubsRequired(t *testing.T) {
	r, f := selectionReplay(t)
	base := f.Quotes[0]
	later := base
	later.ID += "-later"
	later.GreeksAt = f.At
	f.Quotes[0].Expiry = "2026-04-09"
	f.Quotes = append(f.Quotes, later)
	for i := range f.Chains {
		if f.Chains[i].Symbol == "NVDA" && f.Chains[i].Kind == "put" {
			f.Chains[i].Expiry = "2026-04-09"
		}
	}
	f.Chains = append(f.Chains, Chain{Symbol: "NVDA", Kind: "put", Expiry: later.Expiry, At: f.At, Weekly: later.Weekly, Contracts: []string{later.ID}})
	excludeIDs(t, r, &f, []string{base.ID})
	if err := r.step(f); err != nil {
		t.Fatal(err)
	}
	if r.metrics.Submissions != 0 {
		t.Fatal("excluded nearest advanced")
	}
	quotes, err := simBroker{r: r}.Quotes(t.Context(), "NVDA", "put")
	if err != nil {
		t.Fatal(err)
	}
	foundStub := false
	for _, q := range quotes {
		if q.ID == base.ID {
			foundStub = true
			if q.Standard || q.Tradable || !q.At.IsZero() {
				t.Fatal("pre-submit stub fabricated market eligibility")
			}
		}
	}
	if !foundStub {
		t.Fatal("pre-submit path dropped the excluded nearest expiry")
	}
	if _, ok := wheel.Select(r.report.Spec.Config, "NVDA", "put", quotes, 0, f.At); ok {
		t.Fatal("pre-submit re-selection advanced past the excluded nearest")
	}
	// Without the stub the same real-only books would advance to the later
	// expiry, proving the stub is what prevents the silent advance.
	var realOnly []wheel.Quote
	for _, q := range quotes {
		if q.ID != base.ID {
			realOnly = append(realOnly, q)
		}
	}
	if _, ok := wheel.Select(r.report.Spec.Config, "NVDA", "put", realOnly, 0, f.At); !ok {
		t.Fatal("expected later expiry to be selectable without its excluded nearest guard")
	}
}

func TestSelectionExclusionDefaultFramesUnchanged(t *testing.T) {
	r, f := selectionReplay(t)
	if len(f.NoRecentBook) != 0 {
		t.Fatal("fixture gained exclusions")
	}
	if err := r.step(f); err != nil {
		t.Fatal(err)
	}
	if len(r.selectionStubs) != 0 {
		t.Fatal("default frame created adapters")
	}
	if r.metrics.Submissions != 1 {
		t.Fatalf("default selection changed: %+v", r.metrics)
	}
}
