// Package simulation causal selection-omission support.
//
// Research-only schema permitting explicitly proved omissions of ranking data
// causally irrelevant to any NEW order. No price/size/OI/delta/freshness is
// fabricated. All 15 CompleteSymbols and full put/call Chains identities are
// retained; every listed ID must be exactly one real Quote, NoRecentBook, or
// validated SelectionOmission. Missing unproved IDs still block. Omitted IDs
// never become market quotes, catalog entries, fills, or marks.
//
// Hash domains (reproducible by an acquisition adapter):
//
//   - SelectionProofVersion prefixes every hashed envelope.
//   - StateSHA256 = hex(sha256(json.Marshal({version, state}))) where state is
//     the exact post-fill/lifecycle/Reconcile r.state (Version, Account,
//     Positions and Intents maps; Go encoding/json sorts map keys deterministically).
//   - SnapshotSHA256 = hex(sha256(json.Marshal({version, account_number, snapshot})))
//     where snapshot is the exact post-Reconcile r.snapshot() (At=T,
//     AvailableCash, Shares sorted by symbol, Options sorted by ID, Orders in
//     replay order, ExternalSymbols, ExternalCapital; no quotes) and account is
//     snap.Account (bound explicitly because wheel.Snapshot.Account is
//     json:"-" and would otherwise not bind the account).
//   - ConfigSHA256 = hex(sha256(json.Marshal({version, config}))) where config
//     is the account-bound frozen config (Spec.Config with Account set to
//     r.state.Account).
//   - Proof At must exactly equal the scheduled decision T (which also equals
//     snapshot At). SourceSHA256 must be lowercase 64-hex with Complete=true,
//     but hash/flag alone qualifies nothing; independent input/source audit is
//     still required for every source gate.
//
// Allowed reasons (conservative only):
//
//   - inactive_kind: listed kind differs from the post-reconcile active kind
//     (call iff OwnedShares==100 else put). Never selected.
//   - global_intent: post-transition len(Intents)>0 AND actual
//     wheel.Plan(bound, state, snapshot, nil, T) returns precisely the global
//     fence error. Every missing active ID except held/pending may be omitted.
//   - full_slots: post-transition no intents, Plan(nil) succeeds,
//     len(active)>=MaxSymbols with Plan-exact active/busy sets, and no eligible
//     settled owned call (state OwnedShares==100 + snapshot Quantity==100,
//     Available>=100 + state Cost>0 + not busy; note Cost comes from
//     st.Positions, not snapshot shares).
package simulation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"autowheel/internal/wheel"
)

// SelectionProofVersion prefixes every selection-proof hash envelope so future
// schema changes cannot silently reuse old digests.
const SelectionProofVersion = "AutoWheelSelectionProofV1"

// Conservative omission reasons.
const (
	SelectionReasonInactiveKind = "inactive_kind"
	SelectionReasonGlobalIntent = "global_intent"
	SelectionReasonFullSlots    = "full_slots"
)

// selectionFenceError must exactly match wheel.Plan's global intent fence.
// Earlier invalid-account/cash/exposure errors never satisfy global_intent.
const selectionFenceError = "order submission awaiting broker reconciliation"

// SelectionStateHash hashes the exact post-transition journal state.
func SelectionStateHash(st wheel.State) string {
	type envelope struct {
		Version string      `json:"version"`
		State   wheel.State `json:"state"`
	}
	b, err := json.Marshal(envelope{Version: SelectionProofVersion, State: st})
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// SelectionSnapshotHash hashes the exact post-transition non-quote snapshot.
// The account is bound explicitly because wheel.Snapshot.Account is json:"-".
func SelectionSnapshotHash(snap wheel.Snapshot) string {
	type envelope struct {
		Version  string         `json:"version"`
		Account  string         `json:"account_number"`
		Snapshot wheel.Snapshot `json:"snapshot"`
	}
	b, err := json.Marshal(envelope{Version: SelectionProofVersion, Account: snap.Account, Snapshot: snap})
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// SelectionConfigHash hashes the account-bound frozen strategy config.
func SelectionConfigHash(cfg wheel.Config) string {
	type envelope struct {
		Version string       `json:"version"`
		Config  wheel.Config `json:"config"`
	}
	b, err := json.Marshal(envelope{Version: SelectionProofVersion, Config: cfg})
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func isLowerHex64(s string) bool {
	return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == ""
}

// selectionBoundConfig returns the account-bound frozen config used by Plan.
func selectionBoundConfig(r *replay) wheel.Config {
	c := r.report.Spec.Config
	c.Account = r.state.Account
	return c
}

// selectionActiveBusy recomputes Plan's exact active/busy sets (strategy.go
// Plan: external symbols by limit scope, positive shares by ownership, all
// options, non-terminal orders, state PutID/OwnedShares/MissingSince).
// It never uses price proxies, OI/delta guesses, cash-only shortcuts, or
// busy==active assumptions.
func selectionActiveBusy(c wheel.Config, st wheel.State, s wheel.Snapshot) (active, busy map[string]bool) {
	active = map[string]bool{}
	busy = map[string]bool{}
	for _, symbol := range s.ExternalSymbols {
		busy[symbol] = true
		if c.SymbolLimitScope != "managed" {
			active[symbol] = true
		}
	}
	for _, p := range s.Shares {
		if p.Quantity > 0 {
			if c.SymbolLimitScope != "managed" || st.Positions[p.Symbol].OwnedShares > 0 || st.Positions[p.Symbol].PutID != "" {
				active[p.Symbol] = true
			} else {
				busy[p.Symbol] = true
			}
		}
	}
	for _, p := range s.Options {
		active[p.Symbol] = true
		busy[p.Symbol] = true
	}
	for _, o := range s.Orders {
		if o.Terminal() {
			continue
		}
		active[o.Symbol] = true
		busy[o.Symbol] = true
	}
	for symbol, p := range st.Positions {
		if p.PutID != "" || p.OwnedShares > 0 || !p.MissingSince.IsZero() {
			active[symbol] = true
		}
		if p.PutID != "" || !p.MissingSince.IsZero() {
			busy[symbol] = true
		}
	}
	return active, busy
}

// selectionHasEligibleCall reports whether Plan could still select a covered
// call on an already-active owned symbol. Cost comes from st.Positions (Plan's
// call filter), not snapshot shares. Settled means snapshot Quantity==100 and
// Available>=100.
func selectionHasEligibleCall(st wheel.State, s wheel.Snapshot, busy map[string]bool) bool {
	shareBySymbol := map[string]wheel.Shares{}
	for _, h := range s.Shares {
		shareBySymbol[h.Symbol] = h
	}
	for symbol, pos := range st.Positions {
		if pos.OwnedShares != 100 || pos.Cost <= 0 {
			continue
		}
		h, ok := shareBySymbol[symbol]
		if !ok || h.Quantity != 100 || h.Available < 100 {
			continue
		}
		if busy[symbol] {
			continue
		}
		return true
	}
	return false
}

// validateSelectionProof requires exact At/binding matches and well-formed
// lowercase 64-hex complete source evidence. Stale pre-fill hashes fail
// because r.state/snapshot are post-transition.
func (r *replay) validateSelectionProof(f Frame, snap wheel.Snapshot, bound wheel.Config) error {
	p := f.SelectionProof
	if p == nil {
		return errors.New("missing selection proof for omission")
	}
	if p.At.IsZero() || !p.At.Equal(f.At) {
		return errors.New("invalid selection proof time; require exact scheduled T")
	}
	if !p.Complete {
		return errors.New("incomplete selection proof coverage")
	}
	if !isLowerHex64(p.SourceSHA256) {
		return errors.New("invalid selection proof source hash")
	}
	for _, h := range []string{p.StateSHA256, p.SnapshotSHA256, p.ConfigSHA256} {
		if !isLowerHex64(h) {
			return errors.New("invalid selection proof binding hash")
		}
	}
	if p.StateSHA256 != SelectionStateHash(r.state) {
		return errors.New("stale or false selection state proof")
	}
	if p.SnapshotSHA256 != SelectionSnapshotHash(snap) {
		return errors.New("stale or false selection snapshot proof")
	}
	if p.ConfigSHA256 != SelectionConfigHash(bound) {
		return errors.New("stale or false selection config proof")
	}
	return nil
}

// validateSelectionOmissions checks shape, roster membership, drift, overlap,
// held/pending scope (both pre-transition capture and post-fill/lifecycle/
// Reconcile context), reason correctness, and proof binding. It never creates
// market quotes, catalog entries, or selection adapters; omitted IDs stay
// omitted. Non-decision frames reject any omission or proof.
func (r *replay) validateSelectionOmissions(f Frame, chainByID map[string]exclusionIdentity, quoteIDs, exclusionIDs map[string]bool, snap wheel.Snapshot, bound wheel.Config) (map[string]string, bool, bool, error) {
	omitted := map[string]string{}
	if f.Kind != "decision" {
		if len(f.SelectionOmissions) > 0 || f.SelectionProof != nil {
			return nil, false, false, errors.New("selection omissions/proofs only allowed on scheduled decision frames")
		}
		return omitted, false, false, nil
	}
	if len(f.SelectionOmissions) == 0 {
		if f.SelectionProof != nil {
			return nil, false, false, errors.New("superfluous selection proof without omissions")
		}
		return omitted, false, false, nil
	}
	seen := map[string]bool{}
	hasGlobal, hasFull := false, false
	// Post-transition guard: held/pending/final-source IDs always require REAL
	// quotes and can never be omitted, even inactive-kind.
	postGuard := map[string]bool{}
	for id := range r.shorts {
		postGuard[id] = true
	}
	for _, pos := range r.state.Positions {
		if pos.PutID != "" {
			postGuard[pos.PutID] = true
		}
		if pos.CallID != "" {
			postGuard[pos.CallID] = true
		}
	}
	if r.pending != nil {
		postGuard[r.pending.Candidate.ID] = true
		if r.pending.Order.OptionID != "" {
			postGuard[r.pending.Order.OptionID] = true
		}
	}
	for _, op := range snap.Options {
		postGuard[op.ID] = true
	}
	for _, o := range snap.Orders {
		if o.OptionID != "" {
			postGuard[o.OptionID] = true
		}
	}
	for _, l := range f.Lifecycles {
		if l.OptionID != "" {
			postGuard[l.OptionID] = true
		}
	}
	for _, o := range f.SelectionOmissions {
		if o.OptionID == "" {
			return nil, false, false, errors.New("invalid selection omission; empty ID")
		}
		if seen[o.OptionID] {
			return nil, false, false, errors.New("duplicate selection omission")
		}
		seen[o.OptionID] = true
		ident, ok := chainByID[o.OptionID]
		if !ok {
			return nil, false, false, errors.New("selection omission for absent roster ID")
		}
		if quoteIDs[o.OptionID] {
			return nil, false, false, errors.New("selection omission overlaps real quote")
		}
		if exclusionIDs[o.OptionID] {
			return nil, false, false, errors.New("selection omission overlaps no-recent-book exclusion")
		}
		if !slices.Contains([]string{SelectionReasonInactiveKind, SelectionReasonGlobalIntent, SelectionReasonFullSlots}, o.Reason) {
			return nil, false, false, errors.New("invalid selection omission reason")
		}
		if old, ok := r.catalog[o.OptionID]; ok && (old.Symbol != ident.Symbol || old.Kind != ident.Kind || old.Expiry != ident.Expiry) {
			return nil, false, false, errors.New("contract identity reused or changed across snapshots")
		}
		if prior, ok := r.seenRoster[o.OptionID]; ok && prior != ident {
			return nil, false, false, errors.New("contract identity reused or changed across snapshots")
		}
		// Both pre-transition and post-transition held/pending block omission.
		if r.omissionRequiredBefore[o.OptionID] {
			return nil, false, false, errors.New("selection omission cannot cover held short or pending order ID")
		}
		if postGuard[o.OptionID] {
			return nil, false, false, errors.New("selection omission cannot cover held short or pending order ID")
		}
		activeKind := "put"
		if r.state.Positions[ident.Symbol].OwnedShares == 100 {
			activeKind = "call"
		}
		switch o.Reason {
		case SelectionReasonInactiveKind:
			if ident.Kind == activeKind {
				return nil, false, false, errors.New("wrong selection omission reason; active kind requires causal fence proof")
			}
		case SelectionReasonGlobalIntent:
			if ident.Kind != activeKind {
				return nil, false, false, errors.New("wrong selection omission reason; inactive kind must use inactive_kind")
			}
			hasGlobal = true
		case SelectionReasonFullSlots:
			if ident.Kind != activeKind {
				return nil, false, false, errors.New("wrong selection omission reason; inactive kind must use inactive_kind")
			}
			hasFull = true
		}
		omitted[o.OptionID] = o.Reason
	}
	if err := r.validateSelectionProof(f, snap, bound); err != nil {
		return nil, false, false, err
	}
	return omitted, hasGlobal, hasFull, nil
}
