# Research candidates to production

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
They are not connected to live order submission yet.

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
5. Protect the three existing AutoWheel lifecycles, observed in AVGO, HOOD and
   MSTR. The simple migration is to keep their installed management semantics
   until broker-confirmed flatness, then switch opening policy. Do not adopt
   those positions into the research cohort, discard unresolved expiry evidence,
   force liquidation or apply a new TP rule to them. More complicated in-flight
   policy migration is unnecessary for P1.
   Bind the new policy and cost evidence to its journal while flat. Its weekly
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
policy binding in the existing atomic account journal and permits its initial
binding only when old journal lifecycles, broker inventory and working orders
are flat. A capture reservation precedes provider reads. Each original signal
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

These transitions are called by the serialized account controller. Source reads
may run in parallel, but journal updates remain serialized. This layer has no
broker submission capability. Timed daemon wiring, current broker reconciliation,
native pre-submit cash/ownership/deliverable review, and the full order/recovery
adapter are still incomplete. No native preset or release is approved by this
component implementation.

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
complete. The versioned native journal/queue and declared assignment-cost ceiling
layer is implemented. Daemon scheduling and broker/order-adapter integration,
account deliverable review, and release/recovery qualification remain open, along with
the remaining numbered integration gates. No installed production software or
configuration was changed or activated. Source code changes are isolated in the
managed worktree.
