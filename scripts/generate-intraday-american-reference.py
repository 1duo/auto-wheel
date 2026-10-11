#!/usr/bin/env python3
"""Synthetic intraday American reference via exact linear clock (QuantLib 1.40).
Conditional linear-clock proof accepted for synthetic flat r,q0,sigma,
deterministic-cash American price/delta ONLY. No historical/source/IV/
production claim. Mathematical instants only, not exchange-calendar claims.
Binding: FdBlackScholesVanillaEngine(process,DividendSchedule,tGrid,xGrid,
dampingSteps=2,schemeDesc=Douglas(),cashDividendModel=default Spot) with
explicit cash schedule; Actual365Fixed; TARGET() unmodified; q=0 flat plus
FixedDividend jumps. No exercise flag; no rho/theta/vega transfer; no cents
round. Do NOT round/drop intraday times. Persist exact real-UTC RFC3339-Z
stamps; integer epoch-ns via datetime whole-seconds plus fraction padded to
9 digits (never float total_seconds). Virtual eval q.Date(6,4,2026). Alt
clock g/2 diagnostic only, not exact/source proof. Out-of-range and future-
cash controls are expected rejections, recorded separately, never priced.
"""

import argparse
import datetime as dtm
import hashlib
import importlib.util
import json
import math
import os
import platform
import re
import sys
import time

PINNED_QUANTLIB_VERSION = "1.40"
EVAL_RFC3339 = "2026-04-06T13:35:00Z"
VY, VM, VD = 2026, 4, 6
STD_TGRID, STD_XGRID = 320, 600
REF_TGRID, REF_XGRID = 640, 1200
DAMPING_STEPS = 2
NS_PER_DAY = 86400 * 1000000000
EXPECTED_IDS = [
    "AM-01",
    "AM-02",
    "AM-07",
    "AM-08",
    "AM-11",
    "AM-12",
    "AM-19",
    "AM-20",
    "AM-23",
    "AM-24",
    "AM-27",
    "AM-30",
]
RFC_RE = re.compile(r"^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(\.\d{1,9})?Z$")


class DateMappingRangeError(ValueError):
    """Exact virtual dates cannot be represented; timestamps must not be snapped."""


class CashTimeError(ValueError):
    """The cash event is outside the strictly open pricing horizon."""


def _validate_cash_offset(di, dT):
    if not 0 < di < dT:
        raise CashTimeError("cash ex-date must lie strictly between eval and maturity")


def _sha256_file(p, chunk=65536):
    h = hashlib.sha256()
    with open(p, "rb") as f:
        while True:
            b = f.read(chunk)
            if not b:
                break
            h.update(b)
    return h.hexdigest()


def _sanitize_error(e, limit=300):
    n = type(e).__name__
    try:
        t = str(e)
    except Exception:
        t = ""
    return n, " ".join(t.split())[:limit]


def _ql_version_string(ql):
    for a in ("__version__", "__quantlib_version__", "version"):
        v = getattr(ql, a, None)
        if isinstance(v, str) and v:
            return v
    try:
        return str(ql.QuantLib.version())
    except Exception:
        return ""


def _intrinsic(kind, s, k):
    return max(s - k, 0.0) if kind == "call" else max(k - s, 0.0)


def _parse_rfc3339_strict_to_ns(s):
    m = RFC_RE.fullmatch(s)
    if not m:
        raise ValueError("strict RFC3339-Z required")
    Y, Mo, D, H, Mi, S = map(int, m.groups()[:6])
    frac = m.group(7)
    nanos = int(frac[1:].ljust(9, "0")) if frac else 0
    d = dtm.date(Y, Mo, D)
    if not (0 <= H <= 23 and 0 <= Mi <= 59 and 0 <= S <= 59):
        raise ValueError("bad time; leap seconds require an explicit clock model")
    days = d.toordinal() - dtm.date(1970, 1, 1).toordinal()
    secs = days * 86400 + H * 3600 + Mi * 60 + S
    return secs * 1000000000 + nanos


def _rfc_for_day_time(day, hh, mm, frac=""):
    return "2026-04-%02dT%02d:%02d:00%sZ" % (day, hh, mm, frac)


def _make_engine(ql, proc, sched, tg, xg):
    return ql.FdBlackScholesVanillaEngine(
        proc, sched, tg, xg, DAMPING_STEPS, ql.FdmSchemeDesc.Douglas()
    )


def _price_clock(ql, cal, dc, veval, nt, items, kind, spot, strike, rp, sp, tg, xg):
    lo = ql.Date.minDate().serialNumber()
    hi = ql.Date.maxDate().serialNumber()
    base = veval.serialNumber()
    if not (lo <= base + nt <= hi):
        raise DateMappingRangeError("virtual expiry outside QL Date bounds; never snap")
    for _, ni in items:
        if not (lo <= base + ni <= hi):
            raise DateMappingRangeError(
                "virtual cash outside QL Date bounds; never suppress"
            )
    exp = veval + int(nt)
    sched = []
    for amt, ni in items:
        sched.append(ql.FixedDividend(float(amt), veval + int(ni)))
    sh = ql.QuoteHandle(ql.SimpleQuote(float(spot)))
    qh = ql.YieldTermStructureHandle(ql.FlatForward(veval, 0.0, dc))
    rh = ql.YieldTermStructureHandle(ql.FlatForward(veval, float(rp), dc))
    vh = ql.BlackVolTermStructureHandle(ql.BlackConstantVol(veval, cal, float(sp), dc))
    proc = ql.BlackScholesMertonProcess(sh, qh, rh, vh)
    ot = ql.Option.Call if kind == "call" else ql.Option.Put
    opt = ql.VanillaOption(
        ql.PlainVanillaPayoff(ot, float(strike)), ql.AmericanExercise(veval, exp)
    )
    opt.setPricingEngine(_make_engine(ql, proc, sched, tg, xg))
    return float(opt.NPV()), float(opt.delta()), exp


def _price_one(ql, cal, dc, veval, case):
    kind = case["kind"]
    spot = float(case["spot"])
    strike = float(case["strike"])
    dte = int(case["dte"])
    sigma = float(case["sigma"])
    rate = float(case["rate"])
    mat_rfc = _rfc_for_day_time(6 + dte, 20, 0)
    t0 = _parse_rfc3339_strict_to_ns(EVAL_RFC3339)
    tT = _parse_rfc3339_strict_to_ns(mat_rfc)
    dT = tT - t0
    if not (dT > 0):
        raise ValueError("maturity must be after eval")
    div_ns = []
    div_rfc = []
    for amt, exd in case["divs"]:
        r = _rfc_for_day_time(int(exd), 13, 30)
        ti = _parse_rfc3339_strict_to_ns(r)
        di = ti - t0
        _validate_cash_offset(di, dT)
        div_ns.append((float(amt), di, r))
        div_rfc.append({"amount_math_usd": float(amt), "real_utc": r})
    g = dT
    for _, di, _ in div_ns:
        g = math.gcd(g, di)
    dv = math.gcd(g, NS_PER_DAY)
    kn, kd = g // dv, NS_PER_DAY // dv
    kf = g / float(NS_PER_DAY)
    NT = dT // g
    items = [(a, di // g) for a, di, _ in div_ns]
    rp = rate * kf
    sp = sigma * math.sqrt(kf)
    pS, dS, expD = _price_clock(
        ql, cal, dc, veval, NT, items, kind, spot, strike, rp, sp, STD_TGRID, STD_XGRID
    )
    pR, dR, _ = _price_clock(
        ql, cal, dc, veval, NT, items, kind, spot, strike, rp, sp, REF_TGRID, REF_XGRID
    )
    for v in (pS, dS, pR, dR):
        if not math.isfinite(v):
            raise ValueError("non-finite FD result")
    intr = _intrinsic(kind, spot, strike)
    Tphys = dT / (365.0 * float(NS_PER_DAY))
    up = spot if kind == "call" else strike * max(1.0, math.exp(-rate * Tphys))
    if not (pS >= intr - 1e-6 and pR >= intr - 1e-6):
        raise ValueError("price below intrinsic bound")
    if not (pS <= up + 1e-4 and pR <= up + 1e-4):
        raise ValueError("price above arbitrage bound")
    if kind == "call":
        if not (-1e-3 <= dS <= 1.001 and -1e-3 <= dR <= 1.001):
            raise ValueError("delta outside call bound")
    else:
        if not (-1.001 <= dS <= 1e-3 and -1.001 <= dR <= 1e-3):
            raise ValueError("delta outside put bound")
    alt = {"applicable": False}
    if g % 2 == 0:
        g2 = g // 2
        NT2 = NT * 2
        items2 = [(a, ni * 2) for a, ni in items]
        kf2 = g2 / float(NS_PER_DAY)
        dv2 = math.gcd(g2, NS_PER_DAY)
        try:
            pA, dA, expA = _price_clock(
                ql,
                cal,
                dc,
                veval,
                NT2,
                items2,
                kind,
                spot,
                strike,
                rate * kf2,
                sigma * math.sqrt(kf2),
                REF_TGRID,
                REF_XGRID,
            )
            if not (math.isfinite(pA) and math.isfinite(dA)):
                raise ValueError("non-finite alt clock")
            if not intr - 1e-6 <= pA <= up + 1e-4:
                raise ValueError("alternative price outside arbitrage bounds")
            alt = {
                "applicable": True,
                "g_alt": g2,
                "kappa_num": g2 // dv2,
                "kappa_den": NS_PER_DAY // dv2,
                "kappa_float": kf2,
                "r_prime_alt": rate * kf2,
                "sigma_prime_alt": sigma * math.sqrt(kf2),
                "expiry_iso": expA.ISO(),
                "price_refined_alt_math_usd": pA,
                "delta_refined_alt": dA,
                "price_abs_diff_alt_minus_primary_math_usd": abs(pA - pR),
                "delta_abs_diff_alt_minus_primary": abs(dA - dR),
                "note": "different-clock agreement diagnostic only; not exact/source proof",
            }
        except DateMappingRangeError as e:
            n, t = _sanitize_error(e)
            alt = {
                "applicable": False,
                "reason": "alternative virtual dates out of range",
                "error_type": n,
                "error_message": t,
            }
    return {
        "id": case["id"],
        "kind": kind,
        "spot_physical": spot,
        "strike_physical": strike,
        "sigma_physical": sigma,
        "rate_physical": rate,
        "dte_calendar_days": dte,
        "eval_real_utc": EVAL_RFC3339,
        "maturity_real_utc": mat_rfc,
        "cash_real_utc": div_rfc,
        "eval_ns": t0,
        "maturity_ns": tT,
        "delta_T_ns": dT,
        "delta_div_ns": [di for _, di, _ in div_ns],
        "g_ns": g,
        "kappa_num": kn,
        "kappa_den": kd,
        "kappa_float": kf,
        "virtual_eval_iso": veval.ISO(),
        "virtual_expiry_iso": expD.ISO(),
        "virtual_cash_iso": [(veval + int(ni)).ISO() for _, ni in items],
        "r_prime": rp,
        "sigma_prime": sp,
        "q_prime": 0.0,
        "price_std_math_usd": pS,
        "delta_std": dS,
        "price_refined_math_usd": pR,
        "delta_refined": dR,
        "price_abs_diff_refined_minus_std_math_usd": abs(pR - pS),
        "delta_abs_diff_refined_minus_std": abs(dR - dS),
        "intrinsic_math_usd": intr,
        "alt_clock": alt,
        "status": "ok",
    }


def main():
    ap = argparse.ArgumentParser(
        description="Synthetic intraday American linear-clock reference (QuantLib 1.40). Writes NEW JSON exclusively."
    )
    ap.add_argument(
        "--output", required=True, help="NEW output JSON (exclusive, mode 0600)"
    )
    a = ap.parse_args()
    if os.path.exists(a.output):
        print("refusing to overwrite existing --output: %s" % a.output, file=sys.stderr)
        sys.exit(2)
    try:
        import QuantLib as ql
    except Exception as e:
        n, t = _sanitize_error(e)
        print("QuantLib import failed (%s): %s" % (n, t), file=sys.stderr)
        sys.exit(2)
    av = _ql_version_string(ql)
    if av != PINNED_QUANTLIB_VERSION:
        print(
            "pinned QuantLib mismatch: required %s, found %r"
            % (PINNED_QUANTLIB_VERSION, av),
            file=sys.stderr,
        )
        sys.exit(2)
    here = os.path.dirname(os.path.abspath(__file__))
    src = os.path.join(here, "generate-american-reference.py")
    try:
        src_sha = _sha256_file(src)
    except Exception as e:
        n, t = _sanitize_error(e)
        print("source matrix read failed (%s): %s" % (n, t), file=sys.stderr)
        sys.exit(2)
    try:
        sp = importlib.util.spec_from_file_location("am_src", src)
        mod = importlib.util.module_from_spec(sp)
        sp.loader.exec_module(mod)
        pairs = {c["id"]: c for c in mod.CASES}
    except Exception as e:
        n, t = _sanitize_error(e)
        print("shared matrix import failed (%s): %s" % (n, t), file=sys.stderr)
        sys.exit(2)
    miss = [i for i in EXPECTED_IDS if i not in pairs]
    if miss:
        print(
            "shared matrix missing IDs (any missing fail): %s" % ",".join(miss),
            file=sys.stderr,
        )
        sys.exit(2)
    try:
        veval = ql.Date(VD, VM, VY)
        cal = ql.TARGET()
        dc = ql.Actual365Fixed()
        ql.Settings.instance().evaluationDate = veval
    except Exception as e:
        n, t = _sanitize_error(e)
        print("QL date setup failed (%s): %s" % (n, t), file=sys.stderr)
        sys.exit(2)
    try:
        own_sha = _sha256_file(os.path.abspath(__file__))
    except Exception:
        own_sha = None
    bp = getattr(getattr(ql, "_QuantLib", None), "__file__", None)
    bs = None
    try:
        if isinstance(bp, str) and bp and os.path.exists(bp):
            bs = _sha256_file(bp)
        else:
            bp = None
    except Exception:
        bp = None
        bs = None
    st = time.perf_counter()
    out = []
    fail = 0
    for cid in EXPECTED_IDS:
        try:
            out.append(_price_one(ql, cal, dc, veval, pairs[cid]))
        except Exception as e:
            fail += 1
            n, t = _sanitize_error(e)
            c = pairs[cid]
            out.append(
                {
                    "id": cid,
                    "kind": c.get("kind", "?"),
                    "spot_physical": float(c.get("spot", 0.0)),
                    "strike_physical": float(c.get("strike", 0.0)),
                    "sigma_physical": float(c.get("sigma", 0.0)),
                    "rate_physical": float(c.get("rate", 0.0)),
                    "status": "error",
                    "error_type": n,
                    "error_message": t,
                    "note": "source-independent numerical diagnostic; not dismissed",
                }
            )
    ctrls = []
    try:
        rO = _rfc_for_day_time(11, 20, 0)
        rD = "2026-04-09T13:30:00.000000001Z"
        t0x = _parse_rfc3339_strict_to_ns(EVAL_RFC3339)
        dTx = _parse_rfc3339_strict_to_ns(rO) - t0x
        ddx = _parse_rfc3339_strict_to_ns(rD) - t0x
        gx = math.gcd(dTx, ddx)
        _validate_cash_offset(ddx, dTx)
        _price_clock(
            ql,
            cal,
            dc,
            veval,
            dTx // gx,
            [(2.0, ddx // gx)],
            "call",
            100.0,
            90.0,
            0.0 * (gx / float(NS_PER_DAY)),
            0.30 * math.sqrt(gx / float(NS_PER_DAY)),
            REF_TGRID,
            REF_XGRID,
        )
        ctrls.append(
            {
                "id": "CTRL-OUT-OF-RANGE-1NS",
                "expected": "reject",
                "status": "unexpected_priced",
                "eval_real_utc": EVAL_RFC3339,
                "maturity_real_utc": rO,
                "cash_real_utc": rD,
                "delta_T_ns": dTx,
                "delta_div_ns": ddx,
                "g_ns": gx,
            }
        )
        fail += 1
    except DateMappingRangeError as e:
        n, t = _sanitize_error(e)
        ctrls.append(
            {
                "id": "CTRL-OUT-OF-RANGE-1NS",
                "expected": "reject",
                "status": "rejected-as-expected",
                "eval_real_utc": EVAL_RFC3339,
                "maturity_real_utc": rO,
                "cash_real_utc": rD,
                "delta_T_ns": dTx,
                "delta_div_ns": ddx,
                "g_ns": gx,
                "error_type": n,
                "error_message": t,
            }
        )
    except Exception as e:
        fail += 1
        n, t = _sanitize_error(e)
        ctrls.append(
            {
                "id": "CTRL-OUT-OF-RANGE-1NS",
                "status": "unexpected_error",
                "error_type": n,
                "error_message": t,
            }
        )
    try:
        rM2 = _rfc_for_day_time(8, 20, 0)
        rF = _rfc_for_day_time(9, 13, 30)
        t0y = _parse_rfc3339_strict_to_ns(EVAL_RFC3339)
        dTy = _parse_rfc3339_strict_to_ns(rM2) - t0y
        ddy = _parse_rfc3339_strict_to_ns(rF) - t0y
        _validate_cash_offset(ddy, dTy)
        ctrls.append(
            {
                "id": "CTRL-FUTURE-CASH",
                "expected": "reject",
                "status": "unexpected_priced",
            }
        )
        fail += 1
    except CashTimeError as e:
        n, t = _sanitize_error(e)
        ctrls.append(
            {
                "id": "CTRL-FUTURE-CASH",
                "expected": "reject",
                "status": "rejected-as-expected",
                "eval_real_utc": EVAL_RFC3339,
                "maturity_real_utc": rM2,
                "cash_real_utc": rF,
                "error_type": n,
                "error_message": t,
            }
        )
    except Exception as e:
        fail += 1
        n, t = _sanitize_error(e)
        ctrls.append(
            {
                "id": "CTRL-FUTURE-CASH",
                "status": "unexpected_error",
                "error_type": n,
                "error_message": t,
            }
        )
    el = time.perf_counter() - st
    pay = {
        "generator": "scripts/generate-intraday-american-reference.py",
        "purpose": "Synthetic intraday numerical reference via exact linear clock on identical flat-model economics. NOT production/live, NOT historical performance. Does NOT claim source/IV/Greek gate passed.",
        "synthetic": True,
        "quantlib_pinned": PINNED_QUANTLIB_VERSION,
        "quantlib_actual": av,
        "python_version": platform.python_version(),
        "python_implementation": platform.python_implementation(),
        "platform": platform.platform(),
        "script_sha256": own_sha,
        "source_matrix_script": "scripts/generate-american-reference.py",
        "source_matrix_sha256": src_sha,
        "quantlib_binary_path": bp,
        "quantlib_binary_sha256": bs,
        "eval_real_utc": EVAL_RFC3339,
        "maturity_rule": "calendar date eval+DTE at 20:00Z",
        "cash_rule": "each given April day at 13:30Z",
        "instant_note": "Mathematical instants only, not real exchange-calendar claims. Virtual dates computational only, never real historical dates.",
        "day_counter": "Actual365Fixed",
        "calendar": "TARGET() unmodified (synthetic use only)",
        "exercise": "AmericanExercise(virtualEval, virtualExpiry), continuous",
        "process": "BlackScholesMertonProcess flat r'=r*kappa, q'=0, sigma'=sigma*sqrt(kappa); spot/strike/amounts unchanged",
        "engine": "FdBlackScholesVanillaEngine(process,DividendSchedule,tGrid,xGrid,dampingSteps=2,schemeDesc=Douglas(),cashDividendModel=default Spot)",
        "grids": {
            "standard": {
                "t": STD_TGRID,
                "x": STD_XGRID,
                "damping_steps": DAMPING_STEPS,
                "scheme": "Douglas",
            },
            "refined": {
                "t": REF_TGRID,
                "x": REF_XGRID,
                "damping_steps": DAMPING_STEPS,
                "scheme": "Douglas",
            },
        },
        "money_note": "Prices math USD full double, deltas dimensionless; no cents round; no exercise flag; no rho/theta/vega transfer.",
        "alt_clock_note": "g/2 refined agreement is numerical diagnostic only, not exact/source proof.",
        "elapsed_seconds_measured": el,
        "case_count": len(out),
        "failure_count": fail,
        "cases": out,
        "controls": ctrls,
    }
    enc = (json.dumps(pay, indent=2, allow_nan=False) + "\n").encode()
    fl = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    try:
        fd = os.open(a.output, fl, 0o600)
    except FileExistsError:
        print("refusing to overwrite existing --output: %s" % a.output, file=sys.stderr)
        sys.exit(2)
    try:
        with os.fdopen(fd, "wb") as h:
            h.write(enc)
            h.flush()
            try:
                os.fsync(h.fileno())
            except Exception as e:
                n, t = _sanitize_error(e)
                print("fsync failed (%s): %s" % (n, t), file=sys.stderr)
                sys.exit(2)
    except Exception as e:
        n, t = _sanitize_error(e)
        print("write failed (%s): %s" % (n, t), file=sys.stderr)
        sys.exit(2)
    try:
        os.chmod(a.output, 0o600)
    except Exception:
        pass
    if fail:
        print(
            "%d failures (failure_count=%d); diagnostics retained" % (fail, fail),
            file=sys.stderr,
        )
        sys.exit(1)


if __name__ == "__main__":
    main()
