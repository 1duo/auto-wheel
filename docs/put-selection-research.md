# AutoWheel put selection: premium within a risk boundary

Status: implemented offline research variant requested by the user. Use a research default
of maximum absolute put delta 0.15 and no positive delta floor (strictly
negative, fresh, identified put delta is required). The user can override the
cap with 0.10 or 0.20 in a separately recorded study. No new live selection policy has been activated, and
the existing six-month baseline remains frozen.

The objective is the highest net premium per collateral dollar per calendar
day within a defined risk-proxy ceiling. The absolute lowest-risk put could
leave almost no premium. A cap makes the trade-off explicit; no contract is
required to sit exactly at it.

## Existing behavior

`wheel.Select` pins the nearest eligible weekly put expiry, 1–8 calendar days
away, and applies contract, quote-age, liquidity, spread and positive-premium
gates. Default absolute put delta is 0.15–0.35; the highest net bid premium per
collateral dollar/day wins. `wheel.Plan` applies cash, ownership and symbol
constraints. Calls precede puts to release existing stock.

The proposed variant replaces the existing 0.15 lower bound with zero while
retaining strictly negative observed delta. Minimum bid, positive net premium,
fees and liquidity gates remain. This is a simple research policy, not evidence
that zero is an optimal floor. The replay admits zero only for the explicitly
named offline variant; production `Config.Validate` continues to reject it. Missing or
unidentified delta must never become a synthetic low-risk value.

## Smallest policy to evaluate

1. Retain the 15-symbol universe, nearest weekly expiry, three slots,
   cash-secured collateral and one-contract orders.
2. Require fresh causal put delta and an out-of-the-money strike. Reject
   contracts above the configured absolute-delta cap.
3. Retain all quote, spread, open-interest, size, tick, fee and freshness gates.
4. Maximize net bid premium / collateral / calendar DTE. Retain existing
   deterministic collateral/contract-ID tie breakers for an exact yield tie.
5. Skip if the nearest expiry has no candidate. Avoid forcing a trade by
   moving closer to the money or to a later expiry.
6. Retain the agreed aggressive covered-call exit for owned shares. The put
   cap does not constrain covered-call exit delta.

This capped-yield policy limits a sensitivity proxy. It does not minimize a
measured assignment probability. Price jumps, dividends, exercise decisions
and after-hours moves affect outcomes. Report net profit, assignment frequency,
losses and idle cash together; collected premium alone is not performance.

## PutFinder reference

[PutFinder's methodology](https://putfinder.com/methodology) combines liquidity,
yield versus strike buffer, underlying quality/volatility premium/trend and an
earnings multiplier. Its main put window is 14–45 DTE. Exact weights are not
fixed public constants. Adopting those factors needs additional point-in-time
sources and an explicit model. AutoWheel's weekly policy can borrow the
filter-then-rank approach without claiming the same score.

Its covered-call comparison aims to keep owned shares, using lower delta bands
and out-of-the-money strikes. That differs from the user's explicit AutoWheel
goal of releasing capital quickly, including below-cost call strikes. Preserve
AutoWheel's aggressive exit rather than copying those call bands. PutFinder
also documents a Black–Scholes fallback when delta is missing. The frozen
AutoWheel study requires independently qualified causal American-option Greeks;
the existing European prototype is insufficient to satisfy that contract.

Show delta, net premium, secured cash, DTE, strike buffer and net breakeven in
the explanation. Strike buffer is `(spot - strike) / spot`; one-contract filled
put breakeven is `strike - net premium / 100`. These figures neither protect
against a gap nor predict assignment. Avoid calling `1 - abs(delta)` a historical
win rate. [OIC delta](https://www.optionseducation.org/advancedconcepts/delta)
and [assignment guidance](https://www.optionseducation.org/referencelibrary/faq/options-assignment)
explain the proxy and early-assignment limits.

AutoWheel currently has no earnings gate or qualified historical earnings
source. An unknown date cannot be called event-free. An earnings exclusion
through expiry would be a further explicit strategy/data change, with causal
publication times and a stated response to an unknown date.

## Research and activation

Preserve `research/simulation/2026-10-05/spec.jsonl` and its completion predicate.
The separate 0.15-cap specification (local evidence omitted from this source repository)
has SHA-256 `2e81d451c599706495c5316d9a3a2f7efe6d80125d8072ae9da6ba86da4a56f1`
and declares `research_variant: weekly_put_risk_cap_v1`. Only this named
variant permits a zero put floor and caps of 0.10, 0.15 or 0.20. All other
strategy settings must match defaults, with the existing explicit $1/$2 fee
sensitivity. Unknown variants, unrelated parameter drift and production
account/state/coordinator bindings are rejected. Both selection and the actual
production `Execute` pre-submit path receive the same offline study config.

The baseline completion check cannot accept this variant. The separate
`scripts/check-put-risk-variant-completion.py` pins this study's spec/hash/name
and reuses every baseline coverage, source, budget, independent-review and
accounting requirement. It requires a separate review binding this exact study
and replay. Those full historical inputs and independent completion audit do
not exist yet. No baseline gate is relaxed.

Targeted Linux checks passed cap equality, over-cap rejection, low-delta
inclusion, nearest-expiry skipping, deterministic ties, Greek guards,
configuration isolation and unchanged aggressive calls. A separate synthetic
lifecycle check retained physical assignment, settlement and accounting. These
checks validate software behavior, not historical returns. Compare using the
same qualified causal sources and full-chain ranking inputs; disclose
retrospective symbol-selection bias.

Databento authenticates on msi, but the complete April 6–October 2 corpus and
all nine source gates remain unqualified. There is no historical comparison,
return or assignment-rate claim. The user removed the historical-data spending
cap on October 6; the actual msi research ledger now carries explicit durable
uncapped authorization. Any
production configuration must pass authenticated readiness, release/config
binding and shared execution ownership/cash gates.
