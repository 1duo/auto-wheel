# Strategy evaluation after tax

Every conservative, balanced and aggressive evaluation must include the same
versioned tax model (`scripts/strategy_tax.py`). A pretax-only run can diagnose
execution, but cannot support a performance promotion. The command
`scripts/evaluate-strategy-tax.py` consumes SHA-256-bound RD1 journal/result
files and writes a new private report. Independent finance, source, scheduling
and cash-funding verification is still required; the tax report alone grants
no trading authority.

Optimize economic wealth after tax relative to whole-account risk. Premium
collection and completed-cycle loss frequency are useful diagnostics, but can
hide capped recoveries, retained losses and taxes. Report marked and hypothetical
terminal-liquidation after-tax wealth together, sampled drawdown, annual and
half-year outcomes, completed net stock-cycle losses, open cycle losses, holding
duration, concentration, call coverage and tax-funded cash headroom. Returns
are unannualized unless explicitly marked otherwise. Do not pool overlapping
account histories as independent market cycles or call inspected data blind.

Also report year-end/final-date liquidation wealth from
`scripts/strategy_tax_valuation.py`. A stock sale can recognize gains earned in
earlier years: tax-recognition NAV can then show weak current-year performance
even when after-tax economic wealth rose. Annual liquidation-value snapshots
include latent gains/losses and closing fees at both endpoints, without resetting
or trading the actual strategy. They use the observed prefix at each cutoff;
replacement trades after that cutoff remain censored. Keep these valuations
separate from cash/tax-funding NAV and the predeclared sampled-drawdown screen.

The common research scenarios treat all stock gains and dividends as short-term
or ordinary income: 37% federal regular tax plus 3.8% NIIT, and a separate
13.3% California stress increment. These fixed marginal scenarios are applied
consistently across history; they are not personal historical returns or an
assumption of California residency. No SALT deduction, long-term preference or
qualified-dividend benefit is claimed.

Written option premiums are deferred until close/expiry; assigned puts reduce
stock tax basis and assigned calls increase sale proceeds. Fees enter the
appropriate basis/proceeds once. Capital gains/losses net by year, with a
$3,000 annual ordinary-income capital-loss deduction and excess carryforward.
The same-symbol, whole-lot wash-sale approximation moves disallowed loss into
the next acquisition's basis when applicable. Accounting lookahead never enters
strategy signals. Also report an upper-tax stress calculation denying all
capital-loss benefits; this bound is not a statement of tax law. No outside,
spouse or IRA trades are modeled. Qualified-call/straddle treatment and the
last 30 days of possible replacement trades remain limitations. NIIT treatment
of the ordinary net-capital-loss deduction is conservatively simplified.

Sources: [IRS Publication 550](https://www.irs.gov/publications/p550),
[capital gains/losses](https://www.irs.gov/taxtopics/tc409),
[NIIT](https://www.irs.gov/individuals/net-investment-income-tax), and
[California rates](https://www.ftb.ca.gov/forms/2025/2025-540-tax-rate-schedules.pdf).

The October 18 user-confirmed capital policy is $200,000 total committed capital,
with no independent per-symbol allocation ceiling. Commitment is outstanding
put strike collateral plus stock acquisition capital, including acquisition
fees. A redundant per-symbol bound equal to the account ceiling preserves the
existing bounded-money schema. It does not allocate one third of the ceiling
to each symbol. Actual settled cash, existing holds, the configured reserve,
three managed symbols and one contract per symbol still apply. The larger
ceiling is neither a deposit nor permission to use margin or expected premium.
Broker available cash already excludes its collateral holds; do not subtract
those holds twice.

Capital-policy studies keep selection, timing, exit and roster rules fixed and
compare matched old/new limits. Tax-funded research additionally earmarks a
causal 54.1% bound on positive realized income without loss credits before new
exposure. Such a simulation guard is not a live tax ledger. Distinguish it from
pretax cash admission with a retrospective tax overlay; disclose every funding
breach instead of claiming an unfunded path is executable after tax.

Performance promotion requires useful after-tax return/risk across both carried
and fresh account histories, fixed fee/execution/assignment sensitivities,
reconciled losses and exact reproduction. Preserve negative findings and old
screens. The explicitly requested capital change and any strategy-mode change
are separate decisions. Production changes retain exact own inventory, peer
retirement, durable intents and authenticated release/config-bound activation.
Never change a journal's financial policy or erase its history by hand.

The $200,000 release uses a $10 physical-delivery allowance rather than the old
$1 allowance. This is an acquisition/call-floor ceiling, not a charge fabricated
as paid PnL. Each new call must also cover the reviewed SEC stock-sale rate,
normal stock TAF and a one-cent CAT ceiling at its own strike notional; stock
appreciation can exceed the acquisition ceiling. Current published charges at
$200,000 fit within $4.15 under these round-up assumptions. Review this evidence
when published rates change. Historical $1/$2 cash fees stay explicitly modeled;
the observed conservative floor decisions were checked separately against the
larger live allowance. Sources: [SEC FY2026 advisory](https://www.sec.gov/rules-regulations/fee-rate-advisories/2026-2)
and [Robinhood fee schedule](https://cdn.robinhood.com/assets/robinhood/legal/RHF%20Fee%20Schedule.pdf).
