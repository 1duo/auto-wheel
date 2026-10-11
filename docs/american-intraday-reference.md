# American Intraday Reference — Exact Linear-Clock Synthetic Construction

Status: synthetic numerical reference only. NOT production/live and NOT
historical performance. This document and its generator do NOT claim full
Greek source gate passed, do NOT claim historical/source/IV qualification,
and do NOT claim production readiness. Any remaining unqualified input
blocks any historical report.

Generator: `scripts/generate-intraday-american-reference.py`. The mathematical
clock transformation is conditional on the flat model below; numerical and
source acceptance are separate.

## 1. Purpose and conditional scope

Provide a synthetic intraday numerical reference on identical economics for
the precise flat model — flat `r`, `q=0`, flat `sigma`, spot/strike `S,K`
unchanged, deterministic known cash `FixedDividend D_i` at exact real
instants `t_i`, maturity `T` — as opposed to timestamp-dropping.

Conditional linear-clock proof is accepted for synthetic flat `r,q0,sigma`,
deterministic-cash American price/delta ONLY. It does not transfer
`rho/theta/vega`, does not prove historical causality, and does not prove
robust numerics. Grid agreement and different-clock agreement are
diagnostics only, not exact/source proof.

## 2. Pinned primary source and engine binding

Pinned QuantLib version: `1.40` exactly. The generator fails unless
`QuantLib.__version__ == "1.40"`.

Primary sources pinned at v1.40 (same family as date-aligned oracle):

- `https://raw.githubusercontent.com/lballabio/QuantLib/v1.40/ql/pricingengines/vanilla/fdblackscholesvanillaengine.cpp`
- `https://raw.githubusercontent.com/lballabio/QuantLib/v1.40/ql/methods/finitedifferences/utilities/fdmdividendhandler.cpp`

Modern binding used (explicit cash schedule, same as old oracle):

- `FdBlackScholesVanillaEngine(process, DividendSchedule, tGrid, xGrid, dampingSteps=2, schemeDesc=Douglas(), cashDividendModel=default Spot)`
- `q.Option.Call/Put`, `PlainVanillaPayoff` + `VanillaOption` + `AmericanExercise`.
- `BinomialVanillaEngine` is continuous-`q` only and is NOT a cash-dividend reference.
- Older `FDAmericanEngine` / `FDDividendAmericanEngine` names are NOT accepted.

Grids: standard `320` time / `600` space; refined `640` time / `1200` space;
`2` damping steps; default `Douglas` scheme. Same refined grid is reused for
the alternative clock.

Main inspection of pinned v1.40 sources confirms maturity uses `process.time`,
with the risk-free reference date and day counter; cash jump times use the same
`yearFraction`. The explicit Spot dividend schedule is retained and no business
day adjustment is applied to these times. However, the numerical solver adds
an extra stop `1e-5` virtual years after each cash date, and the mesher uses
`max(2, floor(24*maturity))` intermediate steps. These absolute virtual-time
choices are not scale invariant. Consequently the mathematical equivalence
must not be called exact equivalence of finite-grid engine outputs. The two
grids and alternative-clock comparisons measure resulting numerical changes;
passing them is a bounded diagnostic, not complete numerical/source proof.

Pinned implementation evidence:

- [Step conditions](https://raw.githubusercontent.com/lballabio/QuantLib/v1.40/ql/methods/finitedifferences/stepconditions/fdmstepconditioncomposite.cpp)
- [Spot mesher](https://raw.githubusercontent.com/lballabio/QuantLib/v1.40/ql/methods/finitedifferences/meshers/fdmblackscholesmesher.cpp)

## 3. Synthetic intraday instants (mathematical, not exchange claims)

- Evaluation: `2026-04-06T13:35:00Z` (aware-UTC).
- Maturity: calendar date `eval+DTE` at `20:00Z` (e.g. DTE 1 → `2026-04-07T20:00:00Z`).
- Cash: each given April day at `13:30Z` (e.g. `(0.5,7)` → `2026-04-07T13:30:00Z`).
- These are mathematical instants, not real exchange-calendar claims. No
  exchange-close map, no trading-calendar minute-alignment, and no real
  historical date is claimed.
- Exact parse: strict RFC3339 `Z` with up to 9 fractional ns digits
  (`YYYY-MM-DDTHH:MM:SS[.1-9 digits]Z`). Integer epoch-ns uses datetime whole
  seconds only plus fraction padded to 9 digits; never `float total_seconds`.
- Do NOT round/drop intraday times. Dropping/snapping fractions to the
  nearest day changes `T_phys`, total variance, discount, and `t_i/T` ratios
  — different economics. Dividing by `g` preserves them.
- Virtual dates are computational only, never real historical dates.

## 4. Shared matrix (12 cases, dynamic import)

The 12 cases are imported dynamically via `importlib` from the existing date
generator `scripts/generate-american-reference.py`; the matrix is not copied.
Only these IDs are priced; any missing ID fails:

`AM-01, AM-02, AM-07, AM-08, AM-11, AM-12, AM-19, AM-20, AM-23, AM-24, AM-27, AM-30`.

Original physical `spot/strike/vol/rate/amounts` are kept fully explicit per
case (same values as the date-aligned matrix). Both source hashes are
persisted: this generator's `script_sha256` and the imported
`source_matrix_sha256`, plus pinned `1.40` vs actual version, QuantLib binary
SHA when accessible via `ql._QuantLib.__file__`, Python version/
implementation/platform, and measured `perf_counter` elapsed. Both-hash
persistence reflects the shared-matrix import.

| ID | Kind | S | K | DTE | σ | r | Cash (synthetic) |
|----|------|---|---|-----|------|-------|------------------|
| AM-01 | call | 100 | 100 | 1 | 0.30 | 0.05 | — |
| AM-02 | put | 100 | 100 | 1 | 0.30 | 0.05 | — |
| AM-07 | call | 100 | 100 | 2 | 0.60 | 0.00 | 0.5 @ Apr 7 |
| AM-08 | put | 100 | 100 | 2 | 0.60 | 0.00 | 0.5 @ Apr 7 |
| AM-11 | call | 100 | 90 | 5 | 0.30 | 0.00 | 2.0 @ Apr 9 |
| AM-12 | put | 100 | 110 | 5 | 0.30 | 0.00 | 2.0 @ Apr 9 |
| AM-19 | call | 100 | 90 | 8 | 0.30 | 0.05 | 0.5 @ Apr 8, 2.0 @ Apr 11 |
| AM-20 | put | 100 | 110 | 8 | 0.30 | 0.05 | 0.5 @ Apr 8, 2.0 @ Apr 11 |
| AM-23 | call | 100 | 100 | 8 | 0.30 | -0.05 | 2.0 @ Apr 8, 0.5 @ Apr 11 |
| AM-24 | put | 100 | 100 | 8 | 0.30 | -0.05 | 2.0 @ Apr 8, 0.5 @ Apr 11 |
| AM-27 | put | 0.01 | 100 | 8 | 0.30 | -0.05 | — (deep special) |
| AM-30 | call | 100 | 100 | 2 | 0.15 | -0.05 | 2.0 @ Apr 7 |

With intraday hours above, each `delta_T_ns` and each strict
`0 < delta_div_ns < delta_T_ns` is an integer. `g = gcd(delta_T_ns,
delta_1_ns, ...)`. Rational `kappa = g/(86400*1e9)` is stored as reduced
`kappa_num/kappa_den` plus `kappa_float`. `N_T = delta_T_ns/g`,
`N_i = delta_i_ns/g` are integers.

Virtual mapping: eval `q.Date(6,4,2026)`; expiry `eval+N_T` integer days;
cash `eval+N_i`; `TARGET()` unmodified; `Actual365Fixed`; all anchors
(`Settings.evaluationDate`, both `FlatForward` reference dates,
`BlackConstantVol` reference date, `AmericanExercise(eval,expiry)` start)
equal virtual eval. Rescaled `r'=r*kappa`, `sigma'=sigma*sqrt(kappa)`,
`q'=0`, amounts/spot/strike unchanged full double. `qDate.min/max` serials
are checked before constructing; any out-of-range virtual date rejects
without time snapping and NEVER suppresses a cash event.

## 5. Proof: constant clock preserves American price and delta

Physical years `p = u/(365*86400*1e9)` for ns offset `u`. Virtual days
`v=u/g`, virtual years `y=v/365`. So `p = kappa*y`, `dp = kappa*dy`.

Between dividends, physical risk-neutral `dS/S = r*dp + sigma*dW_p` with
`Var(dW_p)=dp`. Define `W_hat_y = W_{p=kappa*y}/sqrt(kappa)`; by Brownian
scaling it is a Brownian in `y`. Then
`dS/S = r*kappa*dy + sigma*sqrt(kappa)*dW_hat = r'*dy + sigma'*dW_hat`,
exactly the virtual `BlackScholesMertonProcess(r',q'=0,sigma')` law.

Consequences:

- Total variance `sigma'^2*N_T/365 = sigma^2*kappa*N_T/365 = sigma^2*T_phys`.
  Exact for the mathematical model. Floating-point encoding and the solver
  choices described above produce numerical error; retain full-double params
  and judge by measured intervals, never bit equality.
- Total discount `r'*N_T/365 = r*T_phys`, so `exp(-r'*y)=exp(-r*p)`.
- Ex-ratios `delta_i/delta_T = N_i/N_T` preserved exactly by dividing by `g`.
- Cash jump `S -> S-D_i` at `t_i`: amounts unchanged, deterministic times map
  bijectively; the operator commutes with the linear clock.
- Obstacle `intrinsic(S)=max(S-K,0)/max(K-S,0)` has no time unit; unchanged.
- Stopping: `tau_phys <-> tau_virt=(tau_phys-t0)/g` is a positive-linear
  bijection of `[t0,T]` onto `[0,N_T]` days. Filtration `F_hat_y=F_{kappa*y}`
  preserves the stopping property. Hence
  `V_phys(S,t)=sup_tau E[e^{-r(tau-t)} I(S_tau)] = sup_hat E[e^{-r'(hat-y)} I(S_hat)]=V_virt(S,y)`.
- PDE form: `min(-d_p V - L_p V + rV, V-I)=0`,
  `L_p=rS d_S+0.5 sigma^2 S^2 d_SS`, jump `V(t_i-,S)=V(t_i+,S-D_i)`. With
  `y=p/kappa`, `d_p=(1/kappa)d_y`, `L_p=(1/kappa)L'`, `r=(1/kappa)r'`.
  Multiply the LCP by `kappa>0`: sign preserved, obstacle unchanged, same jump.
- Delta `dV/dS` at the corresponding slice is invariant because `V` and `S`
  values are identical. Same `S`-argument gives `Gamma=d2V/dS2`. This is the
  only Greek transfer claimed.

What does NOT transfer: `rho_phys = kappa*dV/dr'_virt`,
`vega_phys = sqrt(kappa)*vega_virt`, `theta` is per-year with different year
lengths (`theta_phys=(1/kappa)*theta_virt` up to sign/daycount). Comparing
virtual `rho/theta/vega` to physical without `kappa` rescaling is forbidden.
Scope is price+delta only. `Actual365Fixed` is mandatory (`N/365` counts
every calendar day including Feb 29, matching `ns/(365*86400*1e9)`);
`Actual/Actual` would re-weight leap periods and break invariance. Tiny or
negative `r'` (e.g. physical `-0.05` scaled by `kappa`) keeps its sign; do
not floor at zero — the date-aligned numerical matrix exercised `-0.05` with
`FlatForward`; it did not qualify historical rate inputs.

`Vcum > Vex+D` remains invalid as an exercise decision rule in the virtual
clock too. No exercise flag is emitted here.

## 6. Alternative clock and controls

Each priced case also computes an independent alternative clock `g/2` when
`g%2==0` and the doubled virtual dates remain within QL bounds, using the
same refined `640x1200` grid with its own `kappa/r'/sigma'` and virtual
dates. Persisted differences `|alt-primary|` price/delta are a numerical
different-clocks agreement diagnostic, not exact/source proof.

Exact out-of-range control (recorded separately, never priced): a 5-day case
with cash fraction `1ns` (`2026-04-09T13:30:00.000000001Z`) gives `gcd=1` and
huge virtual days (`delta_T_ns` itself) far beyond QL `maxDate`; the
generator rejects without time snapping and records original timestamps with
`rejected-as-expected`.

Wrong-future-cash control (recorded separately, never masqueraded as a
priced case): cash strictly outside `(t0,T)` (e.g. Apr 9 cash vs Apr 8
maturity) rejects strictly via the `0<delta_div<delta_T` gate and is recorded
as `rejected-as-expected`. Only the specific cash-time error counts as the
expected rejection; unrelated failures increment `failure_count`.

## 7. Output contract

Per priced case: real-UTC stamps, integer `eval_ns/maturity_ns`,
`delta_T_ns/delta_div_ns`, `g_ns`, `kappa_num/kappa_den/kappa_float`,
virtual ISOs, full-double `r'/sigma'` with `q'=0`, both-grid math-USD prices
(full double, never cents-rounded) and dimensionless deltas, finite checks,
intrinsic lower bound and `S` / `K*max(1,exp(-rT))` upper bound checks,
`|refined-std|` price/delta diffs, grid descriptors, and the `g/2` diagnostic.
No exercise flag; no `rho/theta/vega`; no cents rounding.

Failures are stored per case as sanitized `error_type/error_message`
(source-independent numerical diagnostic; not dismissed). JSON uses
`allow_nan=False`, exclusive `O_EXCL` new file mode `0600` (refuses to
overwrite), `fsync` failure reports an error instead of silently overwriting,
and a nonzero exit follows after the file is retained whenever any expected
case fails. Payload states `failure_count`, not success-always.

CLI: `python3 scripts/generate-intraday-american-reference.py --output <NEW_FILE>`.

## 8. Main-observed probe (not this run's benchmark)

Main observed one actual probe: `S=105, K=100, r=0.05, sigma=0.30, cash 2.0`
with Apr 8 cash vs Apr 10 expiry — clocks `300s/150s` gave price
`5.041052222829439/5.041052222804863` and delta
`0.9839027998017725/0.9839027998518648`. Do NOT call that probe a benchmark
from the matrix run. The pinned source inspection in section 2 records the
remaining solver clock dependence.

## 9. What remains outside the proof (still blocking)

Passing this construction does not pass the Greek source gate and does not
authorize shared trading, historical reports, or production use:

- Outside flat inputs: time-varying `r/sigma`, real `q` behavior, borrow
  constraints, transaction costs, exercise cutoff/assignment frictions,
  discrete exercise calendar.
- Historical causality missing: real dividend forecast/source, declaration
  `KnownAt`, complete cash-declaration audit, corporate actions
  (`corporate_actions_audited=false`), splits/mergers, real rate
  source/term structure, OPRA/book source, tick multiplier,
  settlement/lifecycle, stock/rate sources.
- Exchange map missing: the `13:35/13:30/20:00Z` instants are synthetic math,
  not a verified exchange-close/ex-date map.
- IV missing: no implied-vol identification is claimed; short-dated
  near-intrinsic flat-price intervals still require an identifiability
  pre-check in any later IV gate.
- Robust numerics missing: ADI/Douglas stability, damping, mesher width,
  spatial interpolation at `S-D`, boundary clipping, and FP re-encoding are
  judged by measured `|refined-std|` and `|alt-primary|` intervals with
  tolerances and failure actions, never by construction.

Observation deployment does not authorize shared trading. All AGENTS.md gates
(paused default, broker-facts override, authenticated readiness, qualified
single execution coordinator) remain binding.
