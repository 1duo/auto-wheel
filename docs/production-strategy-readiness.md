# Research candidates to production

## Authorized existing-inventory transition, October 18

The user authorized rollout while managing the existing normal one-lot AutoWheel
lifecycles. They no longer need to finish before the policy switch. The private
configuration binds `import_legacy_state_sha256` to the exact original account
journal. `check-transition` produces this canonical digest without broker calls
or financial mutations; it requires one completed own-put receipt per lifecycle,
resolved submissions, and settled own inventory. A covered call also needs its
own completed opening receipt.

The single native fixed controller imports only after fresh broker account,
option metadata, stock cost/availability and standard-order ownership agree.
It retains the full original journal as immutable provenance inside its new
binding. Existing puts and covered calls retain their exact contracts; assignment
and callaway retain the reporting delay. Imported holdings count toward the
same three slots and account cash/exposure limits. New calls on their settled
stock and new puts use the fixed conservative policy and weekly clock. The
old order writer stays fenced. The import exception cannot authorize another
lifecycle, external shares, unknown submissions or peer inventory. TradePulse
retirement, mask, native seal and lifetime lock remain required.

An inherited own 100-share lot may already exceed the new entry ceilings.
A covered call on that exact lot may still proceed because it adds no stock
or cash-collateral exposure. Its fee, cash reserve, availability and acquisition
floor remain mandatory. The entire inherited exposure still counts when
admitting new puts; this management exception cannot fund another entry.

Readiness now explicitly reports non-trading integration compatibility. Actual
source timestamps remain intact. Outside market hours, old positive books can
qualify the exact identity, standard deliverable and bounded broker fee/collateral
review against a fresh account. Compatibility previews cannot be submitted or
become weekly signals. Actual execution still requires fresh market books,
fresh account facts, the original single-use signal, current broker review and
the post-authentication guard. This permits a qualified weekend activation
while all real orders remain inside the reviewed trading clock.

The earlier flat-only migration proposal below is superseded by this exact own
inventory transition. Historical research labels and returns are unchanged.

The latest request starts a production preparation phase (October 18 UTC checkpoint). The historical
results justify engineering and a bounded launch evaluation. They do not prove
future returns, a future completed-loss probability, or live execution fidelity.
Preserve every historical result and rejection; do not rewrite the old screens.

The earlier word "unqualified" covered two different questions. A strategy can
miss a research comparison, such as matching the old conservative drawdown or
improving in the lower quartile of paired half-years, while remaining a useful
candidate for controlled deployment. Production readiness instead requires a
specified policy, faithful implementation, usable market data, exact cash and
ownership controls, compatible recovery and an explicitly activated release.
Good historical returns alone do not establish those operational facts.

## Frozen engineering shortlist

| Candidate | 2021–October 9, 2026 return / sampled DD | Fresh 2023–2025 return / sampled DD | Completed losing stock cycles, full span |
|---|---:|---:|---:|
| Conservative stock-cost floor, hold | 32.22% / 13.86% | 18.68% / 10.77% | 0/11 |
| Balanced stock-cost floor, TP75 | 50.58% / 14.84% | 27.03% / 17.00% | 0/16 |
| Aggressive direct stock, TP75 | 70.96% / 12.92% | 48.20% / 9.99% | 18/64 |

The lower-risk conservative entry guard remains an alternative: 16.58% / 9.20%
full span and 7.08% / 3.68% forward. Start engineering with floor-hold because it
uses the simplest tested holding rule and has the highest tested conservative
full-period profit within the observed below-20% completed-loss range. It has
materially higher downside than the entry guard; this choice is not a claim
that it is safer. Hold also avoids adding a generic closing-order lifecycle
before the entry, ownership and assignment bridge is established.

The conservative and balanced floors retain open losing inventory. Full-span
open stock-cycle PnL is −$6,751.65 and −$4,838.30, respectively; aggressive has
−$830.10. These losses are already marked in NAV. Zero completed losses can
reflect retained losers and small denominators; it is not loss prevention.

The old relative-risk and take-profit-consistency failures remain reported.
Do not claim that balanced is inside the earlier 1.25× allowance in every
window, or that TP75 improved every market regime. This phase distinguishes
research findings from operational launch gates; it does not silently alter a
user's risk tolerance or turn those findings into statistical validation.

## Production baseline and policy fidelity

The observed installed release is `priority-ebcd7520b452`. Its archived source
and executable are frozen on Lexar. The isolated `production-policy-bridge`
worktree starts from that installed source revision, preserving the separate
dirty primary workspace. Compare the whole archive manifest before producing
a release; neither the stale primary checkout nor a newer unrelated remote
checkout is a safe deployment base.

The installed policy is legacy conservative delta selection, with at most
eight days to expiration and its current ten symbols. The research policies
select a 21–45-day expiry closest to 30 days, then use the underlying bid/ask
midpoint and fixed strike distance. Conservative is 90% puts / 105% calls;
balanced is 95% puts / 103% calls. Experimental aggressive buys stock directly
and uses 105% calls. Its frozen base RD1 ancestor's 101% call rule is not the
experimental aggressive policy that produced these results.

Use one exact-integer contract kernel, before checking option liquidity.
Preserve expiry ties, strike ties, missing-target behavior and exclusions of
QCOM, META and FB. A bad quote for the chosen identity skips an opening; it
cannot trigger a search for a nicer historical fill. The new contract kernel
and historical parity command are the first implemented bridge components.
The opt-in native fixed-policy path now uses those components. Installed
production still uses its old configuration; the new path has not been activated.

Select at the signal clock and refresh that same identity before execution.
The historical account does not replace its signal contract when the stock
midpoint moves during the execution delay. `RefreshFixedIdentity` now enforces
that distinction, including exact metadata and listed-expiration checks.
Its six targeted scenarios cover a moved stock price, changed or duplicated
metadata, a missing expiration, zero bid and a stale stock-side timestamp.

A read-only probe has confirmed that the official broker exposes underlying
bid/ask prices with separate `venue_bid_time` and `venue_ask_time`, plus listed
chain expirations. That removes a source-format uncertainty. Weekend samples
are stale for trading; they establish fields, not current executable prices.
Greek and OI values must not be fabricated to emulate a delta strategy. The
fixed-distance strategy does not use those values in its contract choice.

The new `ObserveFixedPolicy` adapter now reads every page of the eligible
instrument menu, applies the same kernel and quotes only the chosen identity.
It retains both underlying-side clocks, option clock, displayed bid size,
supported tick and standard-chain structure. It returns a separate market
observation, never an executable native `wheel.Candidate`. Its account-specific
deliverable-review and trading-authority flags remain false.

The real AMD conservative-put observation succeeded: 486 contracts across the
eligible expirations, eight paginated market calls, no account/order calls.
It selected the November 13, 2026 $470 put from a $527.00/$527.50 underlying
book. Its Friday source clocks were correctly marked stale on Saturday, so
this is a source/decision integration check, not a trading signal. The $47,000
cash collateral would also exceed the historical $26,666.66 per-symbol budget;
the selector must not replace it with a cheaper strike to force an opening.
Do not interpret quote availability as affordability or launch readiness.

The read-only CLI has an explicit four-tool market allowlist and saves each
request/response privately on Lexar. It cannot call account, broker-review,
approval, order or activation operations through that adapter. Targeted checks
cover stale/missing side clocks, zero chosen liquidity, conflicting identities,
duplicate identities/expirations, unknown cash deliverables and denial of
non-market operations. The legacy quote/deliverable check still passes.

The adapter now checks the underlying spread as well as the option spread.
That corrects the bridge to the existing research rule; it does not alter any
saved financial path. Both use a maximum 15% spread relative to midpoint.

The current Go 1.27.2 verifier matches all 21,270 contract choices and 18,858
admission checks across the six full-history/forward shortlist paths: 9,953
unique consulted book pairs, 969 call-floor checks, 1,013 reviewed weekly entry
dates in 2024–2026, and 6,923 funding checks. Funding includes actual filled-entry
frames and one-cent counterfactual boundaries; this does not claim that the
native controller has reproduced every rejected funding attempt. The original
decision audits were reconstructed for all six saved paths, including their
rejected opportunities. Eight targeted Linux Go checks pass. Overlapping paths
and repeated economic inputs are not independent statistical observations.

The first admission-audit attempt stopped at its one-million-input guard while
reading unrelated quote rows. The retained second attempt checks the books
actually consulted by every saved decision, without deleting required dates,
positions or rejections. No new financial case, replay or provider request was
needed for these comparisons.

The frozen 2026 opening cohort is AMD, ORCL, INTC, CRM, BAC, XOM, WMT, BA,
JNJ and CVX. Keep its source hash and stock-only selection origin. A live
quote failure skips a symbol; it does not authorize a new ROI-based universe.

## P1: conservative, paused release first

The following are actual integration requirements, not reasons to run another
parameter search:

1. Connect signal-time selection and refresh of the same contract to the native
   controller. Use the broker's complete eligible identity menu, exact underlying bid/ask prices and
   both side timestamps. Preserve the research spread rule and check current
   positive displayed size, supported tick, standard deliverable and permission.
2. Preserve the original entry/reentry cadence and opening rules. Compare
   actual entry timestamps, not just contract IDs. Keep holding, fees, settlement,
   funding and callaway accounting independently reconciled. Historical modeled
   65-cent option fees and $1 physical fees are assumptions; live broker fees
   require an actual price or a declared bounded budget, never a historical-fee label.
   Signal 31 minutes before the actual regular close on the first reviewed
   session of the ISO week; primary execution is one minute later. The five-minute
   sensitivity is an outer execution-delay boundary, not permission for repeated
   decisions. Preserve a single signal cohort across execution and restarts.
   Managed symbols precede new entries in alphabetical order; new symbols follow
   the frozen stock-only roster order. Do not rank the new policy by option ROI.
3. Reconstruct the exact acquisition basis and fee treatment for the stock-cost
   call floor. A broker average share price is not automatically the model's
   gross acquisition basis. The floor protects the modeled stock-sale leg,
   not the complete stock/option cycle or portfolio drawdown.
4. Keep one account, one contract/100-share lot per symbol, at most three managed
   symbols, cash-secured puts and covered calls. Bind the proposed capital,
   per-symbol allocation and reserve to the release/config before activation.
   Do not silently restore or remove the installed legacy entry cap: its current
   zero fixed ceiling is a different policy from the historical $80,000 ceiling,
   $26,666.66 per-symbol budget and $16,809.44 reserve.
   Opening fees need free cash. They are not added to a put's strike exposure
   when applying the research allocation limit. The new `CanOpen` checks that
   distinction and removes existing holds exactly once. Calls also fund their
   fee from existing cash; expected new premium is not available funding.
5. Protect existing AutoWheel lifecycles using the authorized exact-journal
   transition described above. Preserve the original ownership receipts and
   contracts; do not force liquidation or apply a new TP rule to them.
   Bind the new policy and cost evidence to that verified journal. Its weekly
   signal queue needs a persistent consumed/expired state; a restart cannot
   create a fresh signal from a later price. Do not weaken the unknown-submission
   fence to make the queue advance. Handling multiple confirmed broker working
   orders requires separate ownership/cash evidence and a targeted check.
6. Preserve the verified native TradePulse retirement, its persistent service
   mask, account binding, lifetime lock and restricted standard-order coverage
   warning. Native readiness observed for the old policy is not approval of a
   new strategy. Keep unknown/ambiguous positions, fees, cash, ownership and
   submissions blocking orders; persist each ref_id before sending and never
   automatically retry submission.
7. Build the exact candidate release with Go 1.27.2 and capture executable,
   source and configuration hashes. Run only the targeted selection, freshness,
   cash/fee, ownership, old-journal and activation checks needed for the changes.
   Reuse historical account results unless execution semantics change; then
   replay the affected exact policy without relabelling old runs.
8. Install only paused after the candidate passes those gates. Recheck live
   broker facts and obtain authenticated native activation bound to that exact
   release/config. Keep a schema-compatible paused rollback. Pausing does not
   cancel orders the broker has already accepted.

P1 production writing remains conservative only. Balanced needs general
fee-inclusive TP75 close/reconciliation support; the installed one-off priority
put close is not that policy. Aggressive additionally needs a qualified direct
stock-acquisition capability and ownership lifecycle. Do not bypass the existing
strategy/coordinator permission boundary or enable either mode by renaming a
preset. They can use the same contract kernel during read-only integration.

## Evidence and launch labels

The conservative journal/queue layer is now implemented. It stores a versioned
policy binding in the existing atomic account journal. Initial binding requires
either flat inventory or the explicit exact own-journal transition above;
unresolved submissions and working orders block either transition.
A capture reservation precedes provider reads. Each original signal
is then either durably prepared or explicitly abstained, sealed once and claimed
at most once in frozen roster order. An invalid first observation cannot be
replaced by a later favorable quote. Interrupted capture retains only choices
already durable; missing entries become abstentions. Expiry never cancels an
order already accepted by the broker. Unknown submissions still freeze claims.

Covered-call signal records retain an acquisition ceiling from the reconciled
own put-assignment price plus the policy-bound physical fee allowance. A second
allowance covers call-away in the floor test. This is explicitly a declared fee
ceiling, not an actual paid-fee/realized-PnL record; it cannot appropriate an old
lot or offset acquisition cost with premiums/dividends. Both the signal and
execution gates must remain positive after the declared option-fee allowance.
Fee budgets can make operational eligibility stricter than the historical
modeled fees. Historical returns are not a current native-engine replay claim.

Completed weekly cohorts are archived immutably before the next cohort replaces
them; corruption or an archive failure prevents replacement. Journal saves are
bounded to the existing 1 MiB restart read limit. New code reads legacy journals
with no fixed binding, while an old strict parser rejects a nonempty new binding.
The legacy order path is also explicitly fenced for a bound fixed journal.
Rollback must therefore use a reader compatible with this versioned schema and
remain paused; stripping the binding or forcing an older reader is not rollback.

The native daemon now has a separate opt-in `fixed_policy` configuration block
containing its financial spec and source-bound roster. Native account, reserve,
capital and option-fee fields must agree with that spec. Its per-symbol limit is
explicit. The fixed block owns expiry, distance, spread and cadence rules; the
legacy delta/OI/ROI fields do not select contracts on this path. Legacy selection
and submission are fenced, mode switches are rejected, and discretionary profit
close controls are unavailable. The menubar describes floor + hold separately.
Public status and configuration reports redact the nested account as well.
This first policy binding rejects changes to its financial digest even while
flat. A future capital/mode change requires an explicit qualified journal-policy
handover; editing the configuration or stripping a binding is not that handover.
Source evidence and the next dated roster can refresh without changing the
financial policy, while every weekly batch retains its own source binding.

The serialized daemon reserves capture before reading sources, runs at most three
source reads concurrently and saves each result as it arrives. Its scheduler
wakes for the weekly signal and execution clocks instead of relying on a
five-minute poll to land inside a one-minute capture bucket. Late or interrupted
source results abstain; they never create a later replacement signal.

The dedicated native executor claims once, reconciles fresh account facts,
refreshes the same identity, obtains account-specific deliverable and actual
fee/collateral review, then reconciles cash and inventory again. It persists the
exact ref_id before transport. The existing submission/activation mutex also
checks the weekly deadline immediately before transport. Unknown outcomes retain
the intent; only a proved pre-transport rejection can clear an unsent intent.
An uncertain journal save fences the controller and requires restart from disk.

The final transport review found and corrected two concrete gaps: the old MCP
mutex serialized the nominally parallel readers, and authentication could outlive
the earlier freshness/deadline check. Read RPCs now reserve independent request
IDs and run concurrently; connection changes, reviews and writes stay exclusive.
The guard runs again after authentication, checking both stock-side clocks,
option clock, account-snapshot age, the durable intent and the weekly deadline.
The HTTP context is also bounded by the earliest source expiry/deadline, covering
connection setup. Only errors proved to precede financial HTTP transport may be
classified unsent; financial transport errors remain unknown with no replay.

The client uses fresh HTTP/1 connections and disables body rewind on stateful
requests and OAuth refresh. This avoids hidden cached-connection/HTTP2 retries
inside the standard HTTP transport. The same transport is used for non-trading
readiness reads/reviews. Three source callers can still run concurrently.
Local HTTP fixtures prove overlapping reads, independent SSE identities, fresh
connections, authentication-time expiry and no post-send retry. These are
client-level proofs, not observations of current broker throughput. The negotiated
[MCP HTTP specification](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports)
permits multiple client connections; actual provider qualification remains open.

Exact metadata for each owned open option survives restart, including a covered
call's strike and chain. Reconciliation retains the existing disclosed own-put
assignment inference and at-least-24-hour reporting delay. Put and stock reporting
that overlaps freezes openings. Unresolved settlement globally delays openings;
available cash already excludes existing collateral holds. Stock exposure uses
gross acquisition price plus the declared fee ceiling. Those conservative
operational bounds and actual fill/reporting delays can change the live path;
the historical returns are not a replay of this native adapter.

Fixed-policy readiness requires an actual current opening to pass the source,
deliverable, funding and broker fee review. A compatibility preview neither
captures nor claims the weekly signal and cannot be passed to the executor.
Readiness is tagged with the fixed policy, so legacy readiness cannot authorize
it. Actual execution repeats current checks and remains release/config-bound.

Native fixture checks cover the whole put → fill → settled assignment → covered
call → settled callaway lifecycle, metadata drift, changed cash after review,
stale books, a missed deadline, failed intent durability, lost responses and
restart without resubmission. These are integration fixtures, not new financial
simulations or evidence of live broker fills. Fresh broker qualification of the
exact candidate, compatible paused rollback and release/config approval remain
required before installation or activation. No installed trading software,
configuration, activation or holdings were changed by this preparation.

Use three precise labels:

- **Historical candidate:** complete declared comparisons with limitations and
  rejected screens visible. This is the shortlist's current performance status.
- **Engineering-ready, paused:** exact production policy implementation and
  the relevant targeted integration/recovery checks pass for a bound release.
- **Live enabled:** fresh native readiness and authenticated activation pass
  for that installed release, configuration and account.

No arbitrary number of paper days converts a historical candidate into a
guaranteed profitable strategy. Read-only shadow decisions should first verify
the native data mapping and decisions; after an authorized bounded launch,
observe actual fills, fees, slippage, assignment, callaway, unsettled cash and
recovery. Assess those events and risk before expanding capital or modes.
The earlier 60-session proposal is evidence collection, not an automatic
activation gate or a substitute for enough completed stock cycles.

Public fee review on October 18 UTC confirms that the research's 65-cent option
and $1 physical charges are modeled costs. Robinhood separately publishes zero
exercise/assignment commissions, regulatory charges, and a professional-customer
option charge. Its help page describes a fourth-quarter TAF suspension while
the fee PDF lists ordinary TAF rates. Retain both; use broker review or a declared
conservative budget, rather than choosing the lowest published amount or
assuming the account's classification. Zero exercise commission does not prove
zero all-in stock-sale cost. [Options fees](https://robinhood.com/us/en/support/articles/360001214663/),
[trading-fee help](https://robinhood.com/us/en/support/articles/trading-fees-on-robinhood/),
[fee schedule](https://cdn.robinhood.com/assets/robinhood/legal/RHF%20Fee%20Schedule.pdf).

At this checkpoint the source baseline, read-only probe, contract kernel,
21,270 matching frozen selector decisions, 18,858 admission comparisons, weekly
clock calculation, exact-identity refresh and real paginated market observation
are implemented and verified. P1's source/menu and arithmetic components are
complete. The versioned journal/queue, declared assignment-cost ceiling, dedicated
native execution/reconciliation and timed daemon path are implemented. The exact
Linux source/build verification and the remaining release/recovery qualification
must be bound to the final code snapshot. Actual fresh account-specific broker
qualification is still pending. There is no native balanced TP75 or aggressive
stock-entry implementation yet. Source changes remain isolated in the managed
worktree; production stays unchanged and paused.
