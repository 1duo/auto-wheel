# American Greeks Qualification — Synthetic Date-Aligned Reference

Status: synthetic numerical reference only. NOT production/live and NOT
historical performance. This document and its generator do NOT claim full
Greek source gate passed. Any remaining actual unqualified inputs block any
historical report.

## 1. Purpose

Provide an independent synthetic numerical reference to qualify a later Go
American pricing/IV/delta implementation. The reference is deliberately small,
deterministic, and synthetic so a future Go engine can be checked for
numerical correctness on identical inputs. It is never broker-equivalence and
never a fill/ask/cash claim.

The retained design review and independent verification are in
`research/simulation/2026-10-05/american-greeks-initial-design-review.md` and
`american-greeks-design-verification.md`. They explain the rejected numerical
assumptions and their corrections.

## 2. Pinned primary source and engine binding

Pinned QuantLib version: `1.40` exactly. The generator fails unless
`QuantLib.__version__ == "1.40"`.

Primary sources pinned at v1.40:

- `https://raw.githubusercontent.com/lballabio/QuantLib/v1.40/ql/pricingengines/vanilla/fdblackscholesvanillaengine.cpp`
- `https://raw.githubusercontent.com/lballabio/QuantLib/v1.40/ql/methods/finitedifferences/utilities/fdmdividendhandler.cpp`

Modern binding used:

- `FdBlackScholesVanillaEngine(process, DividendSchedule, tGrid, xGrid, dampingSteps=0, schemeDesc=Douglas(), localVol=False, illegalLocalVolOverwrite=-NullReal, cashDividendModel=Spot)`
- Explicit cash schedule is always passed (possibly empty). `cashDividendModel`
  is left at its documented default `Spot`.
- `q.Option.Call` / `q.Option.Put`, `PlainVanillaPayoff` + `VanillaOption` +
  `AmericanExercise`.

Explicitly NOT used:

- `BinomialVanillaEngine` is a continuous-`q` engine, not a discrete-cash
  dividend reference.
- Older `FDAmericanEngine` / `FDDividendAmericanEngine` names are not accepted.
- No engine name is accepted without the pinned header/doc proof above; the
  comparison remains numerical-correctness-on-identical-inputs only.

## 3. Synthetic market design (all inputs explicitly synthetic)

- Fixed evaluation date `2026-04-06`. No holidays modified.
- `Actual365Fixed` day counter. Calendar `TARGET()` unmodified; used only as a
  synthetic day-count anchor, not as a trading calendar claim.
- Expiry `= evaluationDate + DTE` calendar days by integer-day addition (no
  business-day shift).
- `AmericanExercise(evaluationDate, expiryDate)`: continuous from evaluation
  through maturity.
- `BlackScholesMertonProcess` with explicit `q=0` flat yield plus a discrete
  `FixedDividend` cash schedule. `q=0` plus discrete cash is the whole
  dividend model. An empty schedule is explicit zero synthetic cash inputs,
  not a real-world zero-dividend assumption.
- DATE-ALIGNED synthetic tenors only. The QuantLib wheel 1.40
  high-resolution `Date` constructor is unsupported, so intraday timestamp
  qualification is out of scope for this oracle. Do not work around by
  dropping real intraday source timestamps. The later Go engine requires
  separate intraday reference qualification. [The intraday construction](american-intraday-reference.md)
  retains exact timestamps with a conditional mathematical clock and records
  solver numerical error; historical input and IV qualification remain pending.

## 4. Synthetic matrix (30 cases, within 20–36 budget)

Spot/strike/sigma/rate/dividends are synthetic. No marketing or performance
metrics are attached.

Coverage: DTE `1, 2, 5, 8`; ATM/ITM/OTM plus far/deep specials; sigma
`0.15, 0.30, 0.60`; `r` in `-0.05, 0.00, 0.05`; cash dividends `0.5` / `2.0`
at known ex-dates strictly between evaluation and maturity; six 8-day cases
carry two cash dividends inside 8 days.

Base spot is `100` synthetic USD except the deep special. Moneyness for the
base: calls ATM `K=100`, ITM `K=90`, OTM `K=110`; puts ATM `K=100`, ITM
`K=110`, OTM `K=90`.

| ID | Kind | S | K | DTE | σ | r | Cash divs (synthetic) |
|----|------|---|---|-----|------|-------|------------------------|
| AM-01 | call | 100 | 100 | 1 | 0.30 | 0.05 | — (explicit zero) |
| AM-02 | put | 100 | 100 | 1 | 0.30 | 0.05 | — |
| AM-03 | call | 100 | 110 | 1 | 0.15 | 0.00 | — (near-intrinsic IV note) |
| AM-04 | put | 100 | 90 | 1 | 0.15 | 0.00 | — (near-intrinsic IV note) |
| AM-05 | call | 100 | 90 | 2 | 0.30 | 0.05 | — |
| AM-06 | put | 100 | 110 | 2 | 0.30 | 0.05 | — |
| AM-07 | call | 100 | 100 | 2 | 0.60 | 0.00 | 0.5 @ 2026-04-07 |
| AM-08 | put | 100 | 100 | 2 | 0.60 | 0.00 | 0.5 @ 2026-04-07 |
| AM-09 | call | 100 | 100 | 5 | 0.15 | 0.05 | — |
| AM-10 | put | 100 | 100 | 5 | 0.15 | 0.05 | — |
| AM-11 | call | 100 | 90 | 5 | 0.30 | 0.00 | 2.0 @ 2026-04-09 |
| AM-12 | put | 100 | 110 | 5 | 0.30 | 0.00 | 2.0 @ 2026-04-09 |
| AM-13 | call | 100 | 110 | 5 | 0.60 | -0.05 | 0.5 @ 2026-04-08 |
| AM-14 | put | 100 | 90 | 5 | 0.60 | -0.05 | 0.5 @ 2026-04-08 |
| AM-15 | call | 100 | 100 | 5 | 0.30 | -0.05 | 0.5 @ 2026-04-09 |
| AM-16 | put | 100 | 100 | 5 | 0.30 | -0.05 | 0.5 @ 2026-04-09 |
| AM-17 | call | 100 | 100 | 8 | 0.15 | 0.05 | — |
| AM-18 | put | 100 | 100 | 8 | 0.15 | 0.05 | — |
| AM-19 | call | 100 | 90 | 8 | 0.30 | 0.05 | 0.5 @ 2026-04-08, 2.0 @ 2026-04-11 |
| AM-20 | put | 100 | 110 | 8 | 0.30 | 0.05 | 0.5 @ 2026-04-08, 2.0 @ 2026-04-11 |
| AM-21 | call | 100 | 110 | 8 | 0.60 | 0.00 | 0.5 @ 2026-04-09, 0.5 @ 2026-04-12 |
| AM-22 | put | 100 | 90 | 8 | 0.60 | 0.00 | 0.5 @ 2026-04-09, 0.5 @ 2026-04-12 |
| AM-23 | call | 100 | 100 | 8 | 0.30 | -0.05 | 2.0 @ 2026-04-08, 0.5 @ 2026-04-11 |
| AM-24 | put | 100 | 100 | 8 | 0.30 | -0.05 | 2.0 @ 2026-04-08, 0.5 @ 2026-04-11 |
| AM-25 | call | 100 | 90 | 8 | 0.15 | -0.05 | — |
| AM-26 | put | 100 | 110 | 8 | 0.15 | -0.05 | — |
| AM-27 | put | 0.01 | 100 | 8 | 0.30 | -0.05 | — (deep-ITM negative-rate bound special) |
| AM-28 | call | 100 | 150 | 1 | 0.15 | 0.05 | — (flat-price IV non-identified) |
| AM-29 | put | 100 | 50 | 1 | 0.15 | 0.05 | — (flat-price IV non-identified) |
| AM-30 | call | 100 | 100 | 2 | 0.15 | -0.05 | 2.0 @ 2026-04-07 |

Notes:

- Every cash ex-date above lies strictly between `2026-04-06` and its expiry.
  DTE-1 cases carry empty schedules because no whole date lies strictly
  between evaluation and maturity.
- AM-27 plus the `S=0` limit `K*exp(-rT) = 100.11` for `K=100, r=-0.05,`
  `T=8/365` shows an American put value can exceed `K`; a `K`-only upper
  bound would falsely reject a valid price.
- AM-03/AM-04/AM-28/AM-29 are short-dated near-intrinsic cases. With
  `vega = S*sqrt(T)*phi(d1) -> 0` the price is flat at intrinsic over an
  interval of sigmas; one cent maps to many sigmas. They are IV
  non-identification illustrations, not identified round-trips.

## 5. Numerical output contract

- Grids: standard `320` time / `600` space; refined `640` time / `1200` space;
  `2` damping steps; default `Douglas` scheme.
- Stored per case: both grid prices in math USD (full double, never rounded
  to cents), both deltas, `|refined-std|` price and delta differences,
  intrinsic, grid descriptors, and explicit dividend specs. Math-only storage
  avoids vega/cents ambiguity.
- Failures are stored as source-independent numerical diagnostics
  (`status: error`, `error_type`, sanitized `error_message`) and are not
  dismissed. The generator returns a nonzero exit after retaining the output
  if any case fails.
- Metadata records measured `elapsed_seconds_measured` (perf_counter, not
  estimated), pinned vs actual QuantLib version, Python version/platform,
  script SHA-256, and QuantLib binary SHA-256 when easily accessible via
  `ql._QuantLib.__file__`.
- JSON is serialized with `allow_nan=False`.
- No exercise flag is emitted. The engine surface here does not expose
  continuation versus intrinsic through a verified algorithm, and
  `Vcum > Vex+D` is NOT an exercise criterion. A future flag must be
  `intrinsic >= continuation - eps` at the decision node, cross-checked
  against a reference exercise indicator with documented tolerance.
- Grid agreement alone is NOT complete exact numerical proof; it is one
  convergence signal among bounds, intervals, and identification checks.
- No Databento/financial/broker/prototype imports, no future inputs, no
  historical Greek claims.

CLI:

- `python3 scripts/generate-american-reference.py --output <NEW_FILE>`
- `--output` must be a new file: the program refuses to overwrite an existing
  path (exclusive `O_EXCL` creation, mode `0600`).

## 6. Recommended future Go implementation (plain design, not yet built)

For the later Go engine, use a plain finite-difference American obstacle
pricer with explicit cash jumps:

- Time-stepped spot grid from evaluation to maturity with the risk-free rate,
  `q=0`, and Black volatility per case.
- At each known cash ex-date, apply the jump `S -> S-D` (cash drop) before
  continuing diffusion. State this jump as approximate with interpolation
  error; do not claim exactness for ≤1 dividend. Document grid interpolation,
  boundary clipping, and any averaging as error sources with
  measured tests, with and without averaging.
- At every node enforce the American obstacle `V = max(continuation, intrinsic)`
  where `intrinsic = max(S-K,0)` for calls and `max(K-S,0)` for puts.
- Prove monotonicity `P(σ)`, convergence `|P(N)-P(2N)|`, and
  `|delta(N)-delta(2N)|` as measured criteria with tolerances and failure
  actions, never by construction or by analytic European analogy. Lattice
  effects plus interpolation plus obstacle plus rounding can produce local
  flat/reversal within half a cent.

## 7. Correct arbitrage bounds (binding for any later gate)

- Calls (`q=0`, no continuous yield): upper is spot `S`. For GBM the
  discounted stock is a martingale, so `call <= S` still holds when `r` is
  negative. Use conservative `S + tol`; do not discount `S` by `r`.
- Puts: upper is `K*max(1, exp(-rT)) + tol`. With `r>=0` this is `K`; with
  `r<0` (allowed down to `-0.05`) it is the discounted `K*exp(-rT)`. At `S=0`
  absorbing, the American put equals that discounted strike (e.g. `100.11`
  for `K=100, r=-0.05, T=8/365`, 11c above `K`). A `K`-only check would
  falsely reject it as `ErrNoArbitrage`.
- Lower bound is intrinsic in both cases. All bound checks use math prices
  with explicit tolerance, not cent-rounded mids.

## 8. Convergence, deltas, and IV identification

- Require price grid-convergence intervals and delta intervals
  (`|P_refined-P_std|`, `|delta_refined-delta_std|`), not a single fixed
  tolerance on one grid.
- IV qualification needs a local identifiability check at the observed price.
  Build a compatible price interval from the observed quote precision and
  numerical error, then bound the sigma and delta sets compatible with that
  interval. A wide price difference between the global sigma endpoints does
  not rule out a locally flat region near intrinsic value. Low local vega is
  a warning; accept only if the compatible delta interval is narrow enough
  to determine eligibility and ranking without crossing a policy boundary.
  Otherwise return `ErrNonIdentified` and block that decision. Missing or
  unidentified delta cannot prove that a contract is ineligible.
- Apply any `|σhat-σ|` round-trip only on the identified set with a justified
  price-error bound. Tick handling stays in selection logic. Add an explicit
  flat-price rejection case (e.g. deep 1-DTE ITM/OTM at low vol).
- Do not use arbitrary fixed vol tolerances on cent-rounded mids when
  `vega -> 0`.

## 9. Selection-gate preservation and source causality

- Evaluate every production non-delta candidate Greek scope, including losers.
  A premium/collateral/day proxy cannot prove delta-ineligibility, and unknown
  delta cannot prove ineligibility. No top-k proxy pruning before IV/delta is
  computed unless proxy exclusion provably implies failure of a non-delta
  filter. Preserve nearest-expiry-skip and no-advance rules as stated.
- Source causality still required before any historical report: causal
  spot/rate/div/expiry binding with `SourceSHA256` / `knownAt`, complete
  `listed_chains` / `complete_chain_symbols`, and a complete cash-declaration
  audit. The audit models only known expectations (declared amounts/ex-dates);
  it is not a perfect future forecast.
- Still pending and blocking: tick multiplier, corporate actions
  (`corporate_actions_audited=false`), lifecycle/settlement, rate/stock, and
  OPRA sources. American model prices are math only, not fills, not ask
  liability, not account cash.

No measured performance or full study cost is claimed here. Survivor counts,
per-price timings, and totals remain unmeasured estimates until a measured
audit replaces them.

## 10. Go synthetic numerical diagnostics

The offline Go American obstacle pricer now exists in `internal/americanoption`.
Its audit command compares all 30 date-aligned and 12 intraday pinned cases.
The initial 320/600 versus 640/1200 time/space grids failed six fixed price
grid-convergence diagnostics. These failures remain in
`american-go-initial-20261006.json`; no tolerance was relaxed.

The main agent reproduced an extreme-input grid-index panic and invalid
non-finite residual acceptance, added focused controls, and corrected both.
On Linux Go 1.26.8 the final nine targeted checks passed. A finer 320/900 versus
640/1800 comparison passed all 42 unchanged diagnostics in 11.139 measured
seconds. Maximum refined/reference differences were $0.000283 for price and
0.0000159 for delta; maximum two-grid differences were $0.000889 and 0.0000607.
These grids were selected after the initial failure. Actual-source fingerprints
and all outputs are bound in `american-go-numerical-validation-20261006.json`
beneath `research/simulation/2026-10-05/`.

The original worker result was not accepted; the main agent owns the fixes and
isolated validation. A completed review of the original source confirmed the
residual issues. Its findings and the main disposition are retained. Explicit
float dividend-clock ordering and eliminated-boundary zeroing were added;
four relevant checks passed and the final 42-case audit passed in 11.230 seconds.
Independent acceptance of the final pure offline component passed; the review
binds all four actual source digests and the final audit. The finite matrix
does not establish global error bounds, volatility monotonicity, compatible
IV/delta interval enclosure, exercise decisions or causal market inputs. The
command emits `historical_greeks_qualified=false`. No live strategy imports this
research solver, and both historical studies remain blocked.
# Separate compatibility-set research component

The independently reviewed `ResearchPiecewiseLinearVolMeshV1` utility enumerates
compatible volatility and delta sets for a disclosed mathematical curve. Its
six targeted Linux controls pass. It does not construct FD vertices, qualify
numerical allowances or causal inputs, or feed production or replay selection.
See [the model and exact-inversion scope](volatility-compatibility-sets.md).

The prior 42-case pricing audit and digest identify the pricing/harness source
set at that audit. The new utility is separately hash-bound and reviewed; the
pricing implementation and those original audit artifacts are unchanged.
