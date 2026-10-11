# Simulation selection omissions — causal, research-only

Status: additive research schema with bounded Linux runtime checks. Frozen strategy, config, spec, success command,
9324 decisions / 126 valuations, and all 9 source gates are unchanged.
No price, size, OI, delta, or freshness is fabricated.

## Schema

`Frame` gains:

- `selection_omissions`: `[{option_id, reason}]` with
  `reason ∈ {inactive_kind, global_intent, full_slots}`.
- `selection_proof`: required whenever omissions are supplied; `{at, state_sha256, snapshot_sha256,
  config_sha256, source_sha256, complete}` binding the exact scheduled `T` to
  the post-fill/lifecycle/Reconcile portfolio.

Every listed `Chain` ID must still be exactly one real `Quote`, validated
`NoRecentBook`, or validated `SelectionOmission`. Missing unproved IDs block.
`CompleteSymbols` (all 15) and full put/call `Chains` identities are retained.
Roster expiries must parse and must not precede their capture past expiry close.
The engine fingerprint includes the omission helper in both the Go report and
the completion checker; no coverage or source-qualification condition changed.
Persistence rejects cross-frame identity drift via `seenRoster` and `catalog`.
Omitted IDs never enter `quotes`, `catalog`, fills, `ExecutionCoverage`, or
marks. No roster adapters are created for omissions; existing `NoRecentBook`
identity-only adapters (always ineligible, never stored as market facts) are
preserved for active scopes, including pre-submit `Select` no-advance.

Non-decision frames reject any omission or proof. Superfluous proof without
omissions rejects. Empty, duplicate, unlisted, or overlapping (quote/exclusion)
IDs reject. Held or pending IDs always require real quotes and can never be
omitted, even inactive-kind — validated against both pre-transition capture
and post-fill/lifecycle/Reconcile state, snapshot options/orders, and same-frame
lifecycles. Valuation and forced-close sources are never suppressed.

## Predicates (post-transition, exact)

Active kind per symbol is `call` iff post-Reconcile `OwnedShares==100` else
`put` (uses `st.Positions[s].Cost`, not snapshot cost, for call eligibility).

- `inactive_kind`: listed kind differs from active kind. Never selected.
  Alone, the frame still runs ordinary active `Select` + `Plan` + `Execute`
  with full market inputs and Greek gates.
- `global_intent`: `len(state.Intents)>0` and actual
  `wheel.Plan(bound, state, snapshot, nil, T)` returns precisely
  `order submission awaiting broker reconciliation` (earlier
  account/cash/exposure errors block instead). Every missing active ID except
  held/pending may be omitted. `BlockedCycles` is counted exactly; the scheduled
  decision is consumed. Pre-frame intent cleared by same-frame fill/GFD/
  Reconcile cannot excuse omission — proof must match post hashes.
- `full_slots`: no intents, `Plan(nil)` succeeds, Plan-exact `active/busy`
  sets satisfy `len(active)>=MaxSymbols` (`Plan` already rejects `>Max`, so
  success implies `==Max`), and no eligible settled owned call exists
  (`state OwnedShares==100` + snapshot `Quantity==100`, `Available>=100` +
  `state Cost>0` + not busy). Active/busy are derived exactly from external
  symbols by limit scope, positive shares by ownership, all options,
  non-terminal orders, and `PutID/OwnedShares/MissingSince` — never
  `busy==active`, cash-only, price proxies, OI/delta guesses, or unknown
  source fields. With a ready covered call, full-slot omissions reject.
  `Select` ranking is skipped; `Plan(nil)` is called after all
  roster/proof/quote/source shape checks.

## Proof hashes (reproducible)

`SelectionProofVersion = AutoWheelSelectionProofV1` prefixes every envelope.
`json.Marshal` uses deterministic Go encoding (sorted map keys, RFC3339Nano
times, snapshot shares/options sorted, orders in replay order).

- `state_sha256 = hex(sha256(json({version, state})))` with exact post
  `r.state`.
- `snapshot_sha256 = hex(sha256(json({version, account_number, snapshot})))` with
  exact post `r.snapshot()` (no quotes) and `account=snap.Account` bound
  explicitly because `Snapshot.Account` is `json:"-"`.
- `config_sha256 = hex(sha256(json({version, config})))` with account-bound
  frozen config (`Spec.Config` + `Account=state.Account`).
- `at` must exactly equal scheduled `T` (also `snapshot.At`).
- `source_sha256` must be lowercase 64-hex with `complete=true`.

Use exported `SelectionStateHash`, `SelectionSnapshotHash`,
`SelectionConfigHash`. Encoding errors return an empty string, never a hash of
empty bytes; that value cannot pass proof validation. Hash/flag alone passes no source gate; independent
input/source audit remains required.

## Pitfalls fixed

- Call eligibility uses `st.Positions[s].Cost`, not snapshot share cost.
- External `busy`-only (managed scope) is not `active`; `len(active)>=Max`
  is required, not `busy` count.
- `Plan(nil)` outcome distinguishes fence vs earlier errors; stale pre-fill
  proofs fail exact post matches.
- Inactive omissions never suppress active ranking; global/full skip ranking
  only after full shape validation and still require held/pending/lifecycle
  real quotes.

## Bounded validation

Targeted Linux checks passed in 1.574s for the new schema and physical wheel
accounting; the affected nearest-expiry/pre-submit checks passed in 0.120s, and
malformed expiry plus nearest-expiry checks passed in 0.811s. Covered: inactive
preserves winner/cash; missing active blocks; malformed/duplicate/overlap,
wrong reason, stale/false hashes, superfluous/non-decision proofs reject;
full-slot all-shorts creates no order with unchanged cash; ready call rejects;
external busy-only never full; pending-fill lift rejects stale proof; held/
pending and valuation/forced-close cannot omit; hash determinism plus
account/config/time binding holds. These are implementation checks, not source
qualification or historical performance.
