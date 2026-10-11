#!/usr/bin/env python3
"""Independent synthetic American numerical reference (date-aligned, QuantLib 1.40).

Purpose: generate an independent synthetic numerical reference to qualify a
later Go American pricing/IV/delta implementation. NOT production/live and NOT
historical performance. This oracle does NOT claim full Greek source gate
passed.

Primary pinned sources (v1.40, modern FD engine family):
- https://raw.githubusercontent.com/lballabio/QuantLib/v1.40/ql/pricingengines/vanilla/fdblackscholesvanillaengine.cpp
- https://raw.githubusercontent.com/lballabio/QuantLib/v1.40/ql/methods/finitedifferences/utilities/fdmdividendhandler.cpp

Binding used here (runtime-confirmed on private install):
  FdBlackScholesVanillaEngine(process, DividendSchedule, tGrid, xGrid,
      dampingSteps=0, schemeDesc=Douglas(), localVol=False,
      illegalLocalVolOverwrite=-NullReal, cashDividendModel=Spot)
This script passes an explicit cash schedule (possibly empty) and leaves
cashDividendModel at its documented default Spot. It uses q.Option.Call/Put,
PlainVanillaPayoff + AmericanExercise. BinomialVanillaEngine is a continuous-q
engine and is NOT used as a cash-dividend reference. Older FDAmericanEngine /
FDDividendAmericanEngine names are NOT accepted.

Synthetic design (all parameters explicitly synthetic):
- Fixed evaluation date 2026-04-06, no holidays modified.
- Actual365Fixed day counter; calendar TARGET() unmodified (synthetic use only).
- Expiry = evaluationDate + DTE calendar days (integer-day addition, no
  business-day shift). AmericanExercise(evaluationDate, expiryDate) is
  continuous from evaluation through maturity.
- BlackScholesMertonProcess with explicit q=0 flat dividend yield plus a
  discrete FixedDividend cash schedule. An empty schedule means explicit zero
  synthetic cash inputs, NOT a real-world zero-dividend assumption.
- DATE-ALIGNED synthetic tenors only. The QuantLib wheel 1.40 high-resolution
  Date constructor is unsupported; this oracle does NOT qualify intraday
  timestamps. Do NOT work around by dropping real intraday source timestamps.
  A later Go engine will require separate intraday reference qualification.

Numerical design:
- Standard grid 320 time / 600 space; refined grid 640 time / 1200 space.
- 2 damping steps; default Douglas scheme (FdmSchemeDesc.Douglas()).
- Both grid prices stored in math USD (full double, NOT rounded to cents) to
  avoid vega/cents ambiguity. Deltas for both grids, absolute differences,
  and intrinsic stored. Failures are source-independent numerical diagnostics,
  not dismissed.
- No exercise flag is emitted: the engine does not expose continuation versus
  intrinsic through a verified algorithm here, and Vcum > Vex+D is NOT an
  exercise criterion (it is a value decomposition, not the decision rule).
- Grid agreement alone is NOT complete exact numerical proof.
- Short-dated near-intrinsic cases are included to note IV non-identification
  (vega -> 0, one cent maps to an interval of sigmas); they must not be read
  as identified IV round-trips.
- One special deep-ITM negative-rate put illustrates that value can exceed K
  (S=0 limit is K*exp(-rT) > K when r<0), so a K-only upper bound is invalid.

Output: JSON with allow_nan=False. Errors are caught per case with sanitized
type/message (inputs are synthetic; no secrets exist in this script). No
Databento/financial/broker/prototype imports, no future inputs, no historical
Greek claims. Runtime elapsed is measured with perf_counter, not estimated.
Records QuantLib version, Python version, platform, script SHA-256, and binary
SHA-256 when easily accessible via ql._QuantLib.__file__.
"""
import argparse
import hashlib
import json
import math
import os
import platform
import sys
import time

PINNED_QUANTLIB_VERSION = "1.40"

EVAL_YEAR, EVAL_MONTH, EVAL_DAY = 2026, 4, 6

STD_TGRID, STD_XGRID = 320, 600
REF_TGRID, REF_XGRID = 640, 1200
DAMPING_STEPS = 2

# Deterministic synthetic matrix: 30 cases in 20-36 budget.
# kind: "call" | "put"; spot/strike in synthetic USD; dte in calendar days;
# sigma: annualized Black vol; rate: flat risk-free; divs: [(amount, ex_day)].
# ex_day is day-of-month in April 2026 and must satisfy EVAL_DAY < ex_day < EVAL_DAY+dte.
# Amounts are synthetic cash USD (0.5 / 2.0 where present).
CASES = [
    {"id": "AM-01", "kind": "call", "spot": 100.0, "strike": 100.0, "dte": 1, "sigma": 0.30, "rate": 0.05, "divs": []},
    {"id": "AM-02", "kind": "put", "spot": 100.0, "strike": 100.0, "dte": 1, "sigma": 0.30, "rate": 0.05, "divs": []},
    {"id": "AM-03", "kind": "call", "spot": 100.0, "strike": 110.0, "dte": 1, "sigma": 0.15, "rate": 0.00, "divs": []},
    {"id": "AM-04", "kind": "put", "spot": 100.0, "strike": 90.0, "dte": 1, "sigma": 0.15, "rate": 0.00, "divs": []},
    {"id": "AM-05", "kind": "call", "spot": 100.0, "strike": 90.0, "dte": 2, "sigma": 0.30, "rate": 0.05, "divs": []},
    {"id": "AM-06", "kind": "put", "spot": 100.0, "strike": 110.0, "dte": 2, "sigma": 0.30, "rate": 0.05, "divs": []},
    {"id": "AM-07", "kind": "call", "spot": 100.0, "strike": 100.0, "dte": 2, "sigma": 0.60, "rate": 0.00, "divs": [(0.5, 7)]},
    {"id": "AM-08", "kind": "put", "spot": 100.0, "strike": 100.0, "dte": 2, "sigma": 0.60, "rate": 0.00, "divs": [(0.5, 7)]},
    {"id": "AM-09", "kind": "call", "spot": 100.0, "strike": 100.0, "dte": 5, "sigma": 0.15, "rate": 0.05, "divs": []},
    {"id": "AM-10", "kind": "put", "spot": 100.0, "strike": 100.0, "dte": 5, "sigma": 0.15, "rate": 0.05, "divs": []},
    {"id": "AM-11", "kind": "call", "spot": 100.0, "strike": 90.0, "dte": 5, "sigma": 0.30, "rate": 0.00, "divs": [(2.0, 9)]},
    {"id": "AM-12", "kind": "put", "spot": 100.0, "strike": 110.0, "dte": 5, "sigma": 0.30, "rate": 0.00, "divs": [(2.0, 9)]},
    {"id": "AM-13", "kind": "call", "spot": 100.0, "strike": 110.0, "dte": 5, "sigma": 0.60, "rate": -0.05, "divs": [(0.5, 8)]},
    {"id": "AM-14", "kind": "put", "spot": 100.0, "strike": 90.0, "dte": 5, "sigma": 0.60, "rate": -0.05, "divs": [(0.5, 8)]},
    {"id": "AM-15", "kind": "call", "spot": 100.0, "strike": 100.0, "dte": 5, "sigma": 0.30, "rate": -0.05, "divs": [(0.5, 9)]},
    {"id": "AM-16", "kind": "put", "spot": 100.0, "strike": 100.0, "dte": 5, "sigma": 0.30, "rate": -0.05, "divs": [(0.5, 9)]},
    {"id": "AM-17", "kind": "call", "spot": 100.0, "strike": 100.0, "dte": 8, "sigma": 0.15, "rate": 0.05, "divs": []},
    {"id": "AM-18", "kind": "put", "spot": 100.0, "strike": 100.0, "dte": 8, "sigma": 0.15, "rate": 0.05, "divs": []},
    {"id": "AM-19", "kind": "call", "spot": 100.0, "strike": 90.0, "dte": 8, "sigma": 0.30, "rate": 0.05, "divs": [(0.5, 8), (2.0, 11)]},
    {"id": "AM-20", "kind": "put", "spot": 100.0, "strike": 110.0, "dte": 8, "sigma": 0.30, "rate": 0.05, "divs": [(0.5, 8), (2.0, 11)]},
    {"id": "AM-21", "kind": "call", "spot": 100.0, "strike": 110.0, "dte": 8, "sigma": 0.60, "rate": 0.00, "divs": [(0.5, 9), (0.5, 12)]},
    {"id": "AM-22", "kind": "put", "spot": 100.0, "strike": 90.0, "dte": 8, "sigma": 0.60, "rate": 0.00, "divs": [(0.5, 9), (0.5, 12)]},
    {"id": "AM-23", "kind": "call", "spot": 100.0, "strike": 100.0, "dte": 8, "sigma": 0.30, "rate": -0.05, "divs": [(2.0, 8), (0.5, 11)]},
    {"id": "AM-24", "kind": "put", "spot": 100.0, "strike": 100.0, "dte": 8, "sigma": 0.30, "rate": -0.05, "divs": [(2.0, 8), (0.5, 11)]},
    {"id": "AM-25", "kind": "call", "spot": 100.0, "strike": 90.0, "dte": 8, "sigma": 0.15, "rate": -0.05, "divs": []},
    {"id": "AM-26", "kind": "put", "spot": 100.0, "strike": 110.0, "dte": 8, "sigma": 0.15, "rate": -0.05, "divs": []},
    {"id": "AM-27", "kind": "put", "spot": 0.01, "strike": 100.0, "dte": 8, "sigma": 0.30, "rate": -0.05, "divs": []},
    {"id": "AM-28", "kind": "call", "spot": 100.0, "strike": 150.0, "dte": 1, "sigma": 0.15, "rate": 0.05, "divs": []},
    {"id": "AM-29", "kind": "put", "spot": 100.0, "strike": 50.0, "dte": 1, "sigma": 0.15, "rate": 0.05, "divs": []},
    {"id": "AM-30", "kind": "call", "spot": 100.0, "strike": 100.0, "dte": 2, "sigma": 0.15, "rate": -0.05, "divs": [(2.0, 7)]},
]


def _ql_version_string(ql):
    for attr in ("__version__", "__quantlib_version__", "version"):
        val = getattr(ql, attr, None)
        if isinstance(val, str) and val:
            return val
    try:
        return str(ql.QuantLib.version())
    except Exception:
        return ""


def _sha256_bytes(data):
    return hashlib.sha256(data).hexdigest()


def _sha256_file(path, chunk=65536):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        while True:
            block = handle.read(chunk)
            if not block:
                break
            digest.update(block)
    return digest.hexdigest()


def _sanitize_error(exc, limit=300):
    name = type(exc).__name__
    try:
        text = str(exc)
    except Exception:
        text = ""
    text = " ".join(text.split())[:limit]
    return name, text


def _intrinsic(kind, spot, strike):
    if kind == "call":
        return max(spot - strike, 0.0)
    return max(strike - spot, 0.0)


def _make_engine(ql, process, div_schedule, t_grid, x_grid):
    # Pinned modern FD name. Older FDAmericanEngine / FDDividendAmericanEngine
    # names are not accepted. BinomialVanillaEngine is continuous-q only and is
    # not used as a cash-dividend reference here.
    # Confirmed runtime order: (process, DividendSchedule, tGrid, xGrid,
    # dampingSteps, schemeDesc, ...); cashDividendModel stays at documented
    # default Spot.
    scheme = ql.FdmSchemeDesc.Douglas()
    return ql.FdBlackScholesVanillaEngine(
        process, div_schedule, t_grid, x_grid, DAMPING_STEPS, scheme
    )


def _price_one_case(ql, eval_date, calendar, day_counter, case):
    kind = case["kind"]
    spot = float(case["spot"])
    strike = float(case["strike"])
    dte = int(case["dte"])
    sigma = float(case["sigma"])
    rate = float(case["rate"])

    expiry = eval_date + dte
    for amount, ex_day in case["divs"]:
        if not (EVAL_DAY < int(ex_day) < EVAL_DAY + dte):
            raise ValueError("cash ex-date must lie strictly between eval and maturity")

    spot_handle = ql.QuoteHandle(ql.SimpleQuote(spot))
    # q=0 is an explicit synthetic process input, not a real-world assumption.
    div_handle = ql.YieldTermStructureHandle(ql.FlatForward(eval_date, 0.0, day_counter))
    risk_handle = ql.YieldTermStructureHandle(ql.FlatForward(eval_date, rate, day_counter))
    vol_handle = ql.BlackVolTermStructureHandle(
        ql.BlackConstantVol(eval_date, calendar, sigma, day_counter)
    )
    process = ql.BlackScholesMertonProcess(spot_handle, div_handle, risk_handle, vol_handle)

    div_schedule = []
    div_specs_out = []
    for amount, ex_day in case["divs"]:
        ex_date = ql.Date(int(ex_day), EVAL_MONTH, EVAL_YEAR)
        div_schedule.append(ql.FixedDividend(float(amount), ex_date))
        div_specs_out.append(
            {"amount_math_usd": float(amount), "ex_date": ex_date.ISO(), "kind": "FixedDividend"}
        )

    option_type = ql.Option.Call if kind == "call" else ql.Option.Put
    payoff_std = ql.PlainVanillaPayoff(option_type, strike)
    payoff_ref = ql.PlainVanillaPayoff(option_type, strike)
    # Continuous American exercise from evaluation through maturity.
    exercise_std = ql.AmericanExercise(eval_date, expiry)
    exercise_ref = ql.AmericanExercise(eval_date, expiry)
    option_std = ql.VanillaOption(payoff_std, exercise_std)
    option_ref = ql.VanillaOption(payoff_ref, exercise_ref)
    option_std.setPricingEngine(_make_engine(ql, process, div_schedule, STD_TGRID, STD_XGRID))
    option_ref.setPricingEngine(_make_engine(ql, process, div_schedule, REF_TGRID, REF_XGRID))

    price_std = float(option_std.NPV())
    delta_std = float(option_std.delta())
    price_ref = float(option_ref.NPV())
    delta_ref = float(option_ref.delta())

    for value in (price_std, delta_std, price_ref, delta_ref):
        if not math.isfinite(value):
            raise ValueError("non-finite FD result")

    intrinsic = _intrinsic(kind, spot, strike)
    return {
        "id": case["id"],
        "kind": kind,
        "spot_synthetic": spot,
        "strike_synthetic": strike,
        "dte_calendar_days": dte,
        "expiry_iso": expiry.ISO(),
        "sigma_synthetic": sigma,
        "rate_synthetic": rate,
        "div_yield_synthetic": 0.0,
        "cash_dividends_synthetic": div_specs_out,
        "empty_cash_schedule_is_explicit_zero_synthetic": len(div_specs_out) == 0,
        "price_std_math_usd": price_std,
        "delta_std": delta_std,
        "price_refined_math_usd": price_ref,
        "delta_refined": delta_ref,
        "price_abs_diff_refined_minus_std_math_usd": abs(price_ref - price_std),
        "delta_abs_diff_refined_minus_std": abs(delta_ref - delta_std),
        "intrinsic_math_usd": intrinsic,
        "grid_std": {"t": STD_TGRID, "x": STD_XGRID, "damping_steps": DAMPING_STEPS, "scheme": "Douglas"},
        "grid_refined": {"t": REF_TGRID, "x": REF_XGRID, "damping_steps": DAMPING_STEPS, "scheme": "Douglas"},
        "status": "ok",
    }


def main():
    parser = argparse.ArgumentParser(
        description="Synthetic date-aligned American FD reference (QuantLib 1.40). "
        "Writes a NEW JSON file exclusively; never overwrites."
    )
    parser.add_argument("--output", required=True, help="NEW output JSON file (exclusive, mode 0600)")
    args = parser.parse_args()

    if os.path.exists(args.output):
        print("refusing to overwrite existing --output: %s" % args.output, file=sys.stderr)
        sys.exit(2)

    try:
        import QuantLib as ql
    except Exception as exc:
        name, text = _sanitize_error(exc)
        print("QuantLib import failed (%s): %s" % (name, text), file=sys.stderr)
        sys.exit(2)

    actual_version = _ql_version_string(ql)
    if actual_version != PINNED_QUANTLIB_VERSION:
        print(
            "pinned QuantLib version mismatch: required %s, found %r"
            % (PINNED_QUANTLIB_VERSION, actual_version),
            file=sys.stderr,
        )
        sys.exit(2)

    try:
        eval_date = ql.Date(EVAL_DAY, EVAL_MONTH, EVAL_YEAR)
        calendar = ql.TARGET()
        day_counter = ql.Actual365Fixed()
        ql.Settings.instance().evaluationDate = eval_date
    except Exception as exc:
        name, text = _sanitize_error(exc)
        print("QuantLib date setup failed (%s): %s" % (name, text), file=sys.stderr)
        sys.exit(2)

    try:
        script_sha = _sha256_file(os.path.abspath(__file__))
    except Exception:
        script_sha = None
    binary_path = None
    binary_sha = None
    try:
        binary_path = getattr(getattr(ql, "_QuantLib", None), "__file__", None)
        if isinstance(binary_path, str) and binary_path and os.path.exists(binary_path):
            binary_sha = _sha256_file(binary_path)
        else:
            binary_path = None
    except Exception:
        binary_path = None
        binary_sha = None

    start = time.perf_counter()
    results = []
    failures = 0
    for case in CASES:
        try:
            results.append(_price_one_case(ql, eval_date, calendar, day_counter, case))
        except Exception as exc:
            failures += 1
            name, text = _sanitize_error(exc)
            results.append(
                {
                    "id": case.get("id", "?"),
                    "kind": case.get("kind", "?"),
                    "spot_synthetic": float(case.get("spot", 0.0)),
                    "strike_synthetic": float(case.get("strike", 0.0)),
                    "dte_calendar_days": int(case.get("dte", 0)),
                    "sigma_synthetic": float(case.get("sigma", 0.0)),
                    "rate_synthetic": float(case.get("rate", 0.0)),
                    "div_yield_synthetic": 0.0,
                    "cash_dividends_synthetic": [
                        {"amount_math_usd": float(a), "ex_day_april_2026": int(d), "kind": "FixedDividend"}
                        for a, d in case.get("divs", [])
                    ],
                    "status": "error",
                    "error_type": name,
                    "error_message": text,
                    "note": "source-independent numerical diagnostic; not dismissed",
                }
            )
    elapsed = time.perf_counter() - start

    payload = {
        "generator": "scripts/generate-american-reference.py",
        "purpose": "Independent synthetic numerical reference to qualify later Go American "
        "pricing/IV/delta. NOT production/live and NOT historical performance. "
        "Does NOT claim full Greek source gate passed.",
        "synthetic": True,
        "all_parameters_explicitly_synthetic": True,
        "quantlib_pinned": PINNED_QUANTLIB_VERSION,
        "quantlib_actual": actual_version,
        "python_version": platform.python_version(),
        "python_implementation": platform.python_implementation(),
        "platform": platform.platform(),
        "script_sha256": script_sha,
        "quantlib_binary_path": binary_path,
        "quantlib_binary_sha256": binary_sha,
        "evaluation_date_iso": eval_date.ISO(),
        "evaluation_date_note": "Fixed synthetic date 2026-04-06; no holidays modified.",
        "day_counter": "Actual365Fixed",
        "calendar": "TARGET() unmodified (synthetic use only)",
        "expiry_rule": "evaluationDate + DTE calendar days (integer-day addition)",
        "exercise": "AmericanExercise(evaluationDate, expiryDate), continuous evaluation to maturity",
        "process": "BlackScholesMertonProcess with explicit q=0 flat yield plus discrete FixedDividend schedule",
        "empty_schedule_note": "Empty cash schedule is explicit zero synthetic inputs, not a real zero assumption.",
        "engine": "FdBlackScholesVanillaEngine(process, DividendSchedule, tGrid, xGrid, "
        "dampingSteps=2, schemeDesc=Douglas(), cashDividendModel=default Spot). "
        "BinomialVanillaEngine is continuous-q only and is not a cash-dividend reference. "
        "Older FDAmericanEngine names are not accepted.",
        "primary_sources": [
            "https://raw.githubusercontent.com/lballabio/QuantLib/v1.40/ql/pricingengines/vanilla/fdblackscholesvanillaengine.cpp",
            "https://raw.githubusercontent.com/lballabio/QuantLib/v1.40/ql/methods/finitedifferences/utilities/fdmdividendhandler.cpp",
        ],
        "tenor_scope": "DATE-ALIGNED synthetic tenors only; high-resolution intraday Date "
        "constructor unsupported in wheel 1.40, so intraday timestamp qualification is "
        "out of scope and must not be worked around by dropping real intraday source timestamps.",
        "grids": {
            "standard": {"t": STD_TGRID, "x": STD_XGRID, "damping_steps": DAMPING_STEPS, "scheme": "Douglas"},
            "refined": {"t": REF_TGRID, "x": REF_XGRID, "damping_steps": DAMPING_STEPS, "scheme": "Douglas"},
        },
        "money_note": "Prices are math USD; deltas are dimensionless (full double, not rounded) to avoid "
        "vega/cents ambiguity. Intrinsic included for bound checks.",
        "exercise_flag": "absent: no verified continuation-vs-intrinsic exposure; "
        "Vcum>Vex+D is not used as an exercise criterion.",
        "grid_agreement_note": "Grid agreement alone is not complete exact numerical proof.",
        "iv_note": "Short-dated near-intrinsic cases (AM-03, AM-04, AM-28, AM-29) are IV "
        "non-identification illustrations (flat price in sigma); they require an "
        "identifiability pre-check in any later IV gate, not a fixed vol tolerance.",
        "negative_rate_note": "AM-27 (deep-ITM negative-rate put) plus the S=0 limit "
        "K*exp(-rT)=100.11 for K=100,r=-0.05,T=8/365 shows value can exceed K; "
        "a K-only put upper bound is invalid.",
        "two_div_note": "AM-19 through AM-24 carry two cash dividends inside 8 calendar days "
        "(each ex-date strictly between eval date and maturity).",
        "elapsed_seconds_measured": elapsed,
        "case_count": len(results),
        "failure_count": failures,
        "cases": results,
    }
    encoded = json.dumps(payload, indent=2, sort_keys=False, allow_nan=False, ensure_ascii=False)
    encoded_bytes = (encoded + "\n").encode("utf-8")

    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    try:
        fd = os.open(args.output, flags, 0o600)
    except FileExistsError:
        print("refusing to overwrite existing --output: %s" % args.output, file=sys.stderr)
        sys.exit(2)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(encoded_bytes)
            handle.flush()
            try:
                os.fsync(handle.fileno())
            except Exception:
                pass
    except Exception as exc:
        name, text = _sanitize_error(exc)
        print("write failed (%s): %s" % (name, text), file=sys.stderr)
        sys.exit(2)
    try:
        os.chmod(args.output, 0o600)
    except Exception:
        pass
    if failures:
        print("%d numerical cases failed; diagnostics retained in --output" % failures, file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
