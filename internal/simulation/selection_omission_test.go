package simulation

import (
	"math"
	"strings"
	"testing"
	"time"

	"autowheel/internal/wheel"
)

func omitIDs(t *testing.T, f *Frame, ids []string, reason string) {
	t.Helper()
	keep := f.Quotes[:0]
	drop := map[string]bool{}
	for _, id := range ids {
		drop[id] = true
	}
	for _, q := range f.Quotes {
		if !drop[q.ID] {
			keep = append(keep, q)
		}
	}
	f.Quotes = keep
	for _, id := range ids {
		f.SelectionOmissions = append(f.SelectionOmissions, SelectionOmission{OptionID: id, Reason: reason})
	}
}

func setValidOmissionProof(t *testing.T, r *replay, f *Frame) {
	t.Helper()
	priorNow := r.now
	r.now = f.At
	snap := r.snapshot()
	bound := selectionBoundConfig(r)
	r.now = priorNow
	f.SelectionProof = &SelectionProof{
		At:             f.At,
		StateSHA256:    SelectionStateHash(r.state),
		SnapshotSHA256: SelectionSnapshotHash(snap),
		ConfigSHA256:   SelectionConfigHash(bound),
		SourceSHA256:   strings.Repeat("b", 64),
		Complete:       true,
	}
}

func callIDs(f Frame) []string {
	var out []string
	for _, q := range f.Quotes {
		if q.Kind == "call" {
			out = append(out, q.ID)
		}
	}
	return out
}

func putIDs(f Frame) []string {
	var out []string
	for _, q := range f.Quotes {
		if q.Kind == "put" {
			out = append(out, q.ID)
		}
	}
	return out
}

func TestSelectionOmissionInactivePreservesWinner(t *testing.T) {
	r, f := selectionReplay(t)
	calls := callIDs(f)
	omitIDs(t, &f, calls, SelectionReasonInactiveKind)
	setValidOmissionProof(t, r, &f)
	beforeCash := r.cash
	if err := r.step(f); err != nil {
		t.Fatalf("inactive omissions blocked: %v", err)
	}
	if r.metrics.Submissions != 1 || r.pending == nil || r.pending.Candidate.ID != "fixture-NVDA-put" {
		t.Fatalf("inactive omission changed winner: %+v", r.pending)
	}
	if r.cash != beforeCash {
		t.Fatal("inactive omission moved cash without fill")
	}
	for _, id := range calls {
		if _, ok := r.quotes[id]; ok {
			t.Fatalf("omitted %s leaked into books", id)
		}
		if _, ok := r.catalog[id]; ok {
			t.Fatalf("omitted %s leaked into catalog", id)
		}
	}
}

func TestSelectionOmissionMissingActiveBlocks(t *testing.T) {
	r, f := selectionReplay(t)
	// Remove winner without proving omission.
	keep := f.Quotes[:0]
	for _, q := range f.Quotes {
		if q.ID != "fixture-NVDA-put" {
			keep = append(keep, q)
		}
	}
	f.Quotes = keep
	if err := r.step(f); err == nil {
		t.Fatal("missing unproved active ID accepted")
	}
}

func TestSelectionOmissionMalformedProofOverlap(t *testing.T) {
	cases := map[string]func(*Frame, *replay){
		"missing_proof": func(f *Frame, r *replay) { f.SelectionProof = nil },
		"empty_id": func(f *Frame, r *replay) {
			f.SelectionOmissions[0].OptionID = ""
		},
		"duplicate": func(f *Frame, r *replay) {
			f.SelectionOmissions = append(f.SelectionOmissions, f.SelectionOmissions[0])
		},
		"not_listed": func(f *Frame, r *replay) {
			f.SelectionOmissions[0].OptionID = "absent-roster-id"
		},
		"unknown_omitted_expiry": func(f *Frame, r *replay) {
			for i := range f.Chains {
				if f.Chains[i].Symbol == "AMD" && f.Chains[i].Kind == "call" {
					f.Chains[i].Expiry = "unknown"
				}
			}
		},
		"quote_overlap": func(f *Frame, r *replay) {
			f.SelectionOmissions = []SelectionOmission{{OptionID: f.Quotes[0].ID, Reason: SelectionReasonInactiveKind}}
		},
		"wrong_reason_active_as_inactive": func(f *Frame, r *replay) {
			// Active put claimed inactive.
			f.SelectionOmissions = nil
			omitIDs(t, f, []string{"fixture-NVDA-put"}, SelectionReasonInactiveKind)
		},
		"wrong_reason_inactive_as_global": func(f *Frame, r *replay) {
			f.SelectionOmissions = nil
			omitIDs(t, f, []string{"fixture-NVDA-call"}, SelectionReasonGlobalIntent)
			setValidOmissionProof(t, r, f)
		},
		"superfluous_proof": func(f *Frame, r *replay) {
			f.SelectionOmissions = nil
			setValidOmissionProof(t, r, f)
		},
		"stale_hash": func(f *Frame, r *replay) {
			f.SelectionProof.StateSHA256 = strings.Repeat("c", 64)
		},
		"bad_source_upper": func(f *Frame, r *replay) {
			f.SelectionProof.SourceSHA256 = strings.Repeat("B", 64)
		},
		"incomplete_proof": func(f *Frame, r *replay) {
			f.SelectionProof.Complete = false
		},
		"wrong_time": func(f *Frame, r *replay) {
			f.SelectionProof.At = f.At.Add(time.Second)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r, f := selectionReplay(t)
			if name != "quote_overlap" && name != "wrong_reason_active_as_inactive" && name != "wrong_reason_inactive_as_global" && name != "superfluous_proof" {
				omitIDs(t, &f, []string{"fixture-AMD-call"}, SelectionReasonInactiveKind)
			}
			setValidOmissionProof(t, r, &f)
			mutate(&f, r)
			if err := r.step(f); err == nil {
				t.Fatal("malformed omission/proof accepted")
			} else if r.metrics.Submissions != 0 {
				t.Fatal("malformed omission submitted")
			}
		})
	}
	// NoRecent overlap: omission overlaps an exclusion ID.
	t.Run("exclusion_overlap", func(t *testing.T) {
		r, f := selectionReplay(t)
		excludeIDs(t, r, &f, []string{"fixture-AMD-put"})
		omitIDs(t, &f, []string{"fixture-AMD-put"}, SelectionReasonInactiveKind)
		if err := r.step(f); err == nil {
			t.Fatal("omission overlapping exclusion accepted")
		}
	})
	// Non-decision omissions/proofs reject.
	t.Run("non_decision", func(t *testing.T) {
		r, f := selectionReplay(t)
		if err := r.step(f); err != nil {
			t.Fatal(err)
		}
		market := fixtureFrame(f.At.Add(time.Second), "market")
		market.SelectionOmissions = []SelectionOmission{{OptionID: "fixture-AMD-call", Reason: SelectionReasonInactiveKind}}
		if err := r.step(market); err == nil {
			t.Fatal("non-decision omission accepted")
		}
		market2 := fixtureFrame(f.At.Add(2*time.Second), "market")
		r.now = market2.At
		market2.SelectionProof = &SelectionProof{At: market2.At, Complete: true, SourceSHA256: strings.Repeat("b", 64)}
		if err := r.step(market2); err == nil {
			t.Fatal("non-decision proof accepted")
		}
	})
}

func fullSlotsFixture(t *testing.T, withCall bool) (*replay, Frame) {
	t.Helper()
	r, f := selectionReplay(t)
	at := f.At
	for _, q := range f.Quotes {
		if q.Kind != "put" || (q.Symbol != "NVDA" && q.Symbol != "AMD" && q.Symbol != "IREN") || (withCall && q.Symbol == "NVDA") {
			continue
		}
		r.shorts[q.ID] = q.Option
		r.state.Positions[q.Symbol] = wheel.Position{PutID: q.ID, PutStrike: q.Strike, PutOpenedAt: at.Add(-time.Hour), Provenance: "full-slot fixture"}
	}
	if withCall {
		r.state.Positions["NVDA"] = wheel.Position{OwnedShares: 100, Cost: 10000, Provenance: "full-slot call fixture", InventoryAt: at.Add(-time.Hour)}
		r.shares["NVDA"] = inventory{Cost: 10000, SettledAt: at}
	}
	return r, f
}

func TestSelectionOmissionFullSlotsNoOrder(t *testing.T) {
	r, f := fullSlotsFixture(t, false)
	beforeCash := r.cash
	// Held puts keep real quotes; all other puts omitted as full_slots, calls inactive.
	var fullIDs []string
	for _, id := range putIDs(f) {
		if id != "fixture-NVDA-put" && id != "fixture-AMD-put" && id != "fixture-IREN-put" {
			fullIDs = append(fullIDs, id)
		}
	}
	omitIDs(t, &f, fullIDs, SelectionReasonFullSlots)
	omitIDs(t, &f, callIDs(f), SelectionReasonInactiveKind)
	setValidOmissionProof(t, r, &f)
	if err := r.step(f); err != nil {
		t.Fatalf("full-slot omissions blocked: %v", err)
	}
	if r.metrics.Submissions != 0 || r.pending != nil {
		t.Fatal("full slots created an order")
	}
	if r.cash != beforeCash {
		t.Fatal("full slots moved cash without fill")
	}
}

func TestSelectionOmissionFullSlotsReadyCallRejects(t *testing.T) {
	r, f := fullSlotsFixture(t, true)
	var fullIDs []string
	for _, id := range putIDs(f) {
		if id != "fixture-AMD-put" && id != "fixture-IREN-put" {
			// NVDA put is inactive (owned shares) so skip; other puts active.
			if id == "fixture-NVDA-put" {
				continue
			}
			fullIDs = append(fullIDs, id)
		}
	}
	omitIDs(t, &f, fullIDs, SelectionReasonFullSlots)
	// NVDA calls are active (owned) and eligible; omitting one as full_slots must reject.
	for _, q := range f.Quotes {
		if q.Symbol == "NVDA" && q.Kind == "call" {
			omitIDs(t, &f, []string{q.ID}, SelectionReasonFullSlots)
			break
		}
	}
	// Remaining calls inactive.
	var remainingCalls []string
	omitted := map[string]bool{}
	for _, o := range f.SelectionOmissions {
		omitted[o.OptionID] = true
	}
	for _, q := range f.Quotes {
		if q.Kind == "call" && !omitted[q.ID] {
			// NVDA put is inactive, but this is call loop; AMD/IREN calls inactive.
			if q.Symbol != "NVDA" {
				remainingCalls = append(remainingCalls, q.ID)
			}
		}
	}
	omitIDs(t, &f, remainingCalls, SelectionReasonInactiveKind)
	// NVDA put is inactive (owned shares).
	omitIDs(t, &f, []string{"fixture-NVDA-put"}, SelectionReasonInactiveKind)
	setValidOmissionProof(t, r, &f)
	if err := r.step(f); err == nil {
		t.Fatal("full slots with ready covered call accepted")
	} else if r.metrics.Submissions != 0 {
		t.Fatal("ready-call full slot submitted")
	}
}

func TestSelectionOmissionExternalBusyNeverFull(t *testing.T) {
	r, f := selectionReplay(t)
	r.report.Spec.PeerSymbols = []string{"AMD", "IREN", "MSFT"}
	omitIDs(t, &f, putIDs(f), SelectionReasonFullSlots)
	omitIDs(t, &f, callIDs(f), SelectionReasonInactiveKind)
	setValidOmissionProof(t, r, &f)
	if err := r.step(f); err == nil {
		t.Fatal("external busy-only accepted as full slots")
	}
}

func TestSelectionOmissionGlobalFenceAndStaleLift(t *testing.T) {
	t.Run("fence_blocks_with_proof", func(t *testing.T) {
		r, f := selectionReplay(t)
		if err := r.step(f); err != nil {
			t.Fatal(err)
		}
		if r.pending == nil || len(r.state.Intents) == 0 {
			t.Fatal("fixture did not create fence")
		}
		next := fixtureFrame(r.decisions[1], "decision")
		next.CompleteSymbols = r.report.Spec.Config.Symbols
		pendingID := r.pending.Candidate.ID
		// Keep execution alive without filling: lower bid below candidate.
		for i := range next.Quotes {
			if next.Quotes[i].ID == pendingID {
				next.Quotes[i].Bid = r.pending.Candidate.Bid - 1
				next.Quotes[i].Ask = next.Quotes[i].Bid + 10
			}
		}
		r.pending.LastQuote = next.At.Add(-time.Second)
		var globalIDs []string
		for _, q := range next.Quotes {
			if q.Kind == "put" && q.ID != pendingID {
				globalIDs = append(globalIDs, q.ID)
			}
		}
		// Remove global puts, keep pending real.
		keep := next.Quotes[:0]
		keepSet := map[string]bool{pendingID: true}
		for _, q := range next.Quotes {
			if q.Kind == "call" || keepSet[q.ID] {
				keep = append(keep, q)
			}
		}
		// Calls inactive; pending put real; other puts global.
		next.Quotes = keep
		for _, id := range globalIDs {
			next.SelectionOmissions = append(next.SelectionOmissions, SelectionOmission{OptionID: id, Reason: SelectionReasonGlobalIntent})
		}
		var callOmit []string
		for _, q := range keep {
			if q.Kind == "call" {
				callOmit = append(callOmit, q.ID)
			}
		}
		// Rebuild keep without calls.
		keep2 := next.Quotes[:0]
		for _, q := range next.Quotes {
			if q.Kind != "call" {
				keep2 = append(keep2, q)
			}
		}
		next.Quotes = keep2
		for _, id := range callOmit {
			next.SelectionOmissions = append(next.SelectionOmissions, SelectionOmission{OptionID: id, Reason: SelectionReasonInactiveKind})
		}
		// Proof must bind post (fence retained). Post equals current plus same Intent.
		setValidOmissionProof(t, r, &next)
		blockedBefore := r.metrics.BlockedCycles
		if err := r.step(next); err != nil {
			t.Fatalf("global fence omissions blocked: %v", err)
		}
		if r.metrics.BlockedCycles != blockedBefore+1 || r.metrics.Submissions != 1 {
			t.Fatal("global fence did not count BlockedCycles exactly")
		}
	})
	t.Run("stale_proof_after_fill_lift_rejects", func(t *testing.T) {
		r, first := selectionReplay(t)
		if err := r.step(first); err != nil {
			t.Fatal(err)
		}
		if r.pending == nil {
			t.Fatal("fixture did not submit")
		}
		next := fixtureFrame(r.decisions[1], "decision")
		next.CompleteSymbols = r.report.Spec.Config.Symbols
		r.report.Spec.Mode = "historical_quote_replay"
		r.pending.LastQuote = next.At.Add(-time.Second)
		r.pending.DwellStart = next.At.Add(-5 * time.Second)
		exec := coveredExecutionFrame(r, next.At, next.At)
		next.ExecutionCoverage = exec.ExecutionCoverage
		for i := range next.Quotes {
			if next.Quotes[i].ID == r.pending.Candidate.ID {
				next.Quotes[i].ObservedAt = next.At
			}
		}
		omitIDs(t, &next, []string{"fixture-AMD-put"}, SelectionReasonGlobalIntent)
		omitIDs(t, &next, []string{"fixture-AMD-call"}, SelectionReasonInactiveKind)
		setValidOmissionProof(t, r, &next) // exact pre-fill proof, intentionally stale
		err := r.step(next)
		if err == nil || !strings.Contains(err.Error(), "stale or false selection state proof") {
			t.Fatalf("expected post-fill state-proof rejection, got %v", err)
		}
		if r.metrics.Fills != 1 || r.pending != nil || len(r.state.Intents) != 0 || r.metrics.Submissions != 1 {
			t.Fatal("test did not exercise actual same-frame fill and fence lift")
		}
	})
}

func TestSelectionOmissionHeldPendingNeverOmit(t *testing.T) {
	t.Run("held_short", func(t *testing.T) {
		r, f := selectionReplay(t)
		q := f.Quotes[0]
		r.shorts[q.ID] = q.Option
		omitIDs(t, &f, []string{q.ID}, SelectionReasonInactiveKind)
		if err := r.step(f); err == nil {
			t.Fatal("omission covered held short")
		}
	})
	t.Run("pending", func(t *testing.T) {
		r, f := selectionReplay(t)
		if err := r.step(f); err != nil {
			t.Fatal(err)
		}
		pendingID := r.pending.Candidate.ID
		next := fixtureFrame(r.decisions[1], "decision")
		next.CompleteSymbols = r.report.Spec.Config.Symbols
		omitIDs(t, &next, []string{pendingID}, SelectionReasonInactiveKind)
		r.pending.LastQuote = next.At.Add(-time.Second)
		if err := r.step(next); err == nil {
			t.Fatal("omission covered pending order")
		}
	})
	t.Run("position_put", func(t *testing.T) {
		r, f := selectionReplay(t)
		r.state.Positions["NVDA"] = wheel.Position{PutID: "fixture-NVDA-put", PutStrike: 10000, PutOpenedAt: f.At.Add(-time.Hour), Provenance: "held test"}
		omitIDs(t, &f, []string{"fixture-NVDA-put"}, SelectionReasonGlobalIntent)
		setValidOmissionProof(t, r, &f)
		if err := r.step(f); err == nil {
			t.Fatal("omission covered held position ID")
		}
	})
}

func TestSelectionOmissionValuationForcedClose(t *testing.T) {
	t.Run("valuation_rejects", func(t *testing.T) {
		r, f := selectionReplay(t)
		if err := r.step(f); err != nil {
			t.Fatal(err)
		}
		val := fixtureFrame(r.valuations[0], "valuation")
		val.SelectionOmissions = []SelectionOmission{{OptionID: "fixture-AMD-call", Reason: SelectionReasonInactiveKind}}
		if err := r.step(val); err == nil {
			t.Fatal("valuation omission accepted")
		}
	})
	t.Run("forced_close_needs_real_ask", func(t *testing.T) {
		r, f := selectionReplay(t)
		q := f.Quotes[0]
		r.shorts[q.ID] = q.Option
		r.quotes[q.ID] = q
		omitIDs(t, &f, []string{q.ID}, SelectionReasonInactiveKind)
		f.Lifecycles = []Lifecycle{{OptionID: q.ID, Outcome: "forced_closed", SettledAt: f.At.Add(time.Hour), ClosePrice: q.Ask, Assumption: "forced close test"}}
		if err := r.step(f); err == nil {
			t.Fatal("forced-close omission accepted")
		}
	})
}

func TestSelectionProofHashDeterminismAndBinding(t *testing.T) {
	r, f := selectionReplay(t)
	r.now = f.At
	baseState := r.state
	baseSnap := r.snapshot()
	baseCfg := selectionBoundConfig(r)
	// Snapshot inventory maps use stable symbol ordering.
	r.shares["AMD"] = inventory{Cost: 10000, SettledAt: f.At}
	r.shares["NVDA"] = inventory{Cost: 9500, SettledAt: f.At}
	snapA := r.snapshot()
	clear(r.shares)
	r.shares["NVDA"] = inventory{Cost: 9500, SettledAt: f.At}
	r.shares["AMD"] = inventory{Cost: 10000, SettledAt: f.At}
	snapB := r.snapshot()
	if SelectionSnapshotHash(snapA) != SelectionSnapshotHash(snapB) {
		t.Fatal("snapshot hash not map-deterministic")
	}
	clear(r.shares)
	// Account binding.
	altered := baseState
	altered.Account = "9999"
	if SelectionStateHash(baseState) == SelectionStateHash(altered) {
		t.Fatal("state hash ignores account")
	}
	snapAltered := baseSnap
	snapAltered.Account = "9999"
	if SelectionSnapshotHash(baseSnap) == SelectionSnapshotHash(snapAltered) {
		t.Fatal("snapshot hash ignores account")
	}
	cfgAltered := baseCfg
	cfgAltered.Fee = baseCfg.Fee + 1
	if SelectionConfigHash(baseCfg) == SelectionConfigHash(cfgAltered) {
		t.Fatal("config hash ignores fee")
	}
	snapTime := baseSnap
	snapTime.At = baseSnap.At.Add(time.Second)
	if SelectionSnapshotHash(baseSnap) == SelectionSnapshotHash(snapTime) {
		t.Fatal("snapshot hash ignores time")
	}
}

func TestSelectionProofRejectsUnencodableInputs(t *testing.T) {
	c := wheel.DefaultConfig()
	c.PutMaxDelta = math.NaN()
	if SelectionConfigHash(c) != "" {
		t.Fatal("unencodable config produced a proof hash")
	}
	st := wheel.NewState()
	st.Intents["NVDA"] = wheel.Intent{Candidate: wheel.Candidate{ReturnPerDay: math.Inf(1)}}
	if SelectionStateHash(st) != "" {
		t.Fatal("unencodable state produced a proof hash")
	}
}
