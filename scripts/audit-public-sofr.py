#!/usr/bin/env python3
"""Offline candidate rate ledger; it cannot qualify replay sources or trading.

An overnight realized rate held flat is an explicit research proxy for future
discounting. Eligibility starts after the whole expected publication day, not at
an invented precise publication instant. The current historical API and policy
snapshot still require independent historical publication/vintage validation.
"""
import argparse
import datetime as dt
from decimal import Decimal, localcontext
import hashlib
import json
import math
import os
from pathlib import Path
from zoneinfo import ZoneInfo

BASE = Path("research/simulation/2026-10-05/public-inputs")
SPEC = Path("research/simulation/2026-10-05/spec.jsonl")
SPEC_SHA = "2ff4ae3765f4102a67382d645164ae4dfb6e036cc7ea3b113eab6dc24f4eac98"
PINNED = {
    "nyfed-sofr-20260401-20261002.json": "58ed6c618696d16806fe69683fd76d313078a9944e4047369318431b707909ec",
    "nyfed-sofr-methodology.html": "f1b3d0c9f78f1f41984dac56ccdbe822e1f13ce3506dc787df7f146ee3346f93",
    "sifma-holiday-schedule.html": "cf2a676bb1140615d8e3f18eac1a7783d03ec875473acfd7bb3f22f64cbe0ca7",
    "nyfed-sofr-good-friday-2026.html": "674d5f2a8a8ad37183692ec2fcabf97a208ca157efb0d4823a3e51a8af2e8be2",
    "nyfed-sofr-july-3-2026.html": "b91386893ec5393956f75d8a4d7cc05041f8adf81c0950048819b94beb3b83ba",
    "nyfed-reference-rate-policy-index.html": "68617440674a5af2ce68c4c68a2cdfb9e9a6f94479b3b9b8323aa82fa560d969",
}
# Bounded Apr1-Oct5 calendar, including the separately announced Apr3 exception.
# It is not the general NYFed office calendar or an inferred future calendar.
NO_SOFR = {dt.date.fromisoformat(x) for x in (
    "2026-04-03", "2026-05-25", "2026-06-19", "2026-07-03", "2026-09-07")}
EASTERN = ZoneInfo("America/New_York")
START, END = dt.date(2026, 4, 1), dt.date(2026, 10, 2)


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def unique(pairs):
    out = {}
    for key, value in pairs:
        if key in out:
            raise ValueError("duplicate JSON field")
        out[key] = value
    return out


def read_json(raw):
    return json.loads(raw, object_pairs_hook=unique, parse_float=Decimal,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite JSON")))


def source_bytes(root, relative, expected):
    path = root / relative
    if path.is_symlink() or not path.is_file() or path.stat().st_size > 2 * 1024 * 1024:
        raise ValueError("invalid bounded source file: " + str(relative))
    raw = path.read_bytes()
    if sha(raw) != expected:
        raise ValueError("pinned source hash differs: " + str(relative))
    return raw


def publication_date(effective):
    if not START <= effective <= END:
        raise ValueError("effective date outside the reviewed calendar")
    value = effective + dt.timedelta(days=1)
    while value.weekday() >= 5 or value in NO_SOFR:
        value += dt.timedelta(days=1)
    if value > dt.date(2026, 10, 5):
        raise ValueError("publication calendar not reviewed")
    return value


def continuous_proxy(percent):
    # One-day ACT/360 simple accrual matched to continuous ACT/365 discounting.
    # Repeating this one-day conversion is a flat future-rate assumption; it is
    # not the actual SOFR holiday accrual, a Treasury yield, or an OIS curve.
    if type(percent) not in (Decimal, int) or not Decimal(-5) <= percent <= Decimal(50):
        raise ValueError("invalid numerical rate")
    value = float(percent)
    rate = 365 * math.log1p(value / 100 / 360)
    with localcontext() as ctx:
        ctx.prec = 45
        reference = Decimal(365) * (Decimal(1) + Decimal(percent) / 100 / 360).ln()
    if not math.isfinite(rate) or abs(rate - float(reference)) > 1e-15:
        raise ValueError("rate convention cross-check failed")
    return rate


def normalize(data):
    if not isinstance(data, dict) or set(data) != {"refRates"} or not isinstance(data["refRates"], list):
        raise ValueError("wrong rate envelope")
    expected = {START + dt.timedelta(days=n) for n in range((END - START).days + 1)
                if (START + dt.timedelta(days=n)).weekday() < 5 and START + dt.timedelta(days=n) not in NO_SOFR}
    seen, rows = set(), []
    fields = {"effectiveDate", "type", "percentRate", "percentPercentile1", "percentPercentile25",
              "percentPercentile75", "percentPercentile99", "volumeInBillions", "revisionIndicator"}
    for row in data["refRates"]:
        if not isinstance(row, dict) or set(row) != fields or row["type"] != "SOFR" or row["revisionIndicator"] != "":
            raise ValueError("unexpected rate row or revision requiring separate evidence")
        date = dt.date.fromisoformat(row["effectiveDate"])
        if date.isoformat() != row["effectiveDate"] or date in seen or date not in expected:
            raise ValueError("ambiguous or non-calendar rate date")
        seen.add(date)
        pub = publication_date(date)
        after = dt.datetime.combine(pub + dt.timedelta(days=1), dt.time(), EASTERN)
        rows.append({"effective_date": date.isoformat(), "percent_act360": str(row["percentRate"]),
                     "expected_publication_date": pub.isoformat(),
                     "eligible_after_model_utc": after.astimezone(dt.timezone.utc).isoformat(),
                     "continuous_act365_flat_proxy": continuous_proxy(row["percentRate"]),
                     "source_sha256": PINNED["nyfed-sofr-20260401-20261002.json"]})
    if seen != expected or len(rows) != 128:
        raise ValueError("full bounded effective-date coverage missing")
    return sorted(rows, key=lambda r: r["effective_date"])


def select(rows, at):
    if at.tzinfo is None:
        raise ValueError("selection clock must be explicit")
    eligible = [r for r in rows if dt.datetime.fromisoformat(r["eligible_after_model_utc"]) <= at]
    if not eligible:
        raise ValueError("no eligible past rate")
    return max(eligible, key=lambda r: r["effective_date"])


def run_controls(rows, source):
    controls = []
    for instant, wanted in (("2026-04-06T13:35:00+00:00", "2026-04-01"),
                            ("2026-04-07T03:59:59.999999+00:00", "2026-04-01"),
                            ("2026-04-07T04:00:00+00:00", "2026-04-02"),
                            ("2026-07-06T13:35:00+00:00", "2026-07-01"),
                            ("2026-10-02T19:40:00+00:00", "2026-09-30")):
        got = select(rows, dt.datetime.fromisoformat(instant))["effective_date"]
        if got != wanted:
            raise ValueError("causal calendar control failed")
        controls.append({"at": instant, "selected_effective_date": got, "passed": True})
    # Preserve a real source row in each adversarial mutation; no hash bypass in
    # the actual CLI. These controls separately test the normalization logic.
    mutations = []
    for label, mutate in (
        ("missing_observation", lambda a: a.pop()),
        ("duplicate_date", lambda a: a.append(dict(a[0]))),
        ("future_holiday_date", lambda a: a[0].update(effectiveDate="2026-07-03")),
        ("revision_requires_evidence", lambda a: a[0].update(revisionIndicator="*")),
        ("boolean_rate", lambda a: a[0].update(percentRate=True)),
        ("wrong_series", lambda a: a[0].update(type="EFFR")),
    ):
        changed = {"refRates": [dict(x) for x in source["refRates"]]}
        mutate(changed["refRates"])
        try:
            normalize(changed)
        except ValueError:
            mutations.append({"control": label, "rejected": True})
        else:
            raise ValueError("invalid source control accepted: " + label)
    return controls, mutations


def audit(root):
    raw = {name: source_bytes(root, BASE / name, digest) for name, digest in PINNED.items()}
    spec = read_json(source_bytes(root, SPEC, SPEC_SHA))
    if (spec["start_date"], spec["end_date"], spec["strategy_config"]["poll_seconds"]) != ("2026-04-06", "2026-10-02", 300):
        raise ValueError("rate schedule differs from frozen replay")
    source = read_json(raw["nyfed-sofr-20260401-20261002.json"])
    rows = normalize(source)
    boundary, mutation = run_controls(rows, source)
    sessions = []
    date = dt.date(2026, 4, 6)
    while date <= END:
        if date.weekday() < 5 and date not in NO_SOFR:
            first = dt.datetime.combine(date, dt.time(9, 35), EASTERN)
            last = dt.datetime.combine(date, dt.time(15, 40), EASTERN)
            a, b = select(rows, first), select(rows, last)
            if a != b:
                raise ValueError("rate changes within a modeled trading session")
            sessions.append({"session": date.isoformat(), "first_decision_utc": first.astimezone(dt.timezone.utc).isoformat(),
                             "last_decision_utc": last.astimezone(dt.timezone.utc).isoformat(),
                             "decision_count": 74, "rate": a})
        date += dt.timedelta(days=1)
    if len(sessions) != 126 or sum(s["decision_count"] for s in sessions) != 9324:
        raise ValueError("candidate rate schedule does not cover frozen session count")
    return {"schema_version": "AutoWheelPublicRateCandidateV1", "source_qualified": False,
            "historical_greeks_qualified": False, "replay_observed_decisions": 0,
            "paid_request": False, "source_sha256": PINNED, "frozen_spec_sha256": SPEC_SHA,
            "script_sha256": sha(Path(__file__).read_bytes()), "observation_count": len(rows),
            "candidate_rate_sessions": len(sessions), "candidate_rate_decisions": 9324,
            "conversion": "365*log1p(percent_act360/100/360); repeating one-day accrual as a flat future proxy",
            "availability_model": "Expected publication date from reviewed SOFR calendar, eligible only at the next midnight America/New_York; this is a conservative modeled boundary, not a captured publication timestamp",
            "scope": "Actual official API observations with bounded calendar and numerical-convention controls only. Current policy/history snapshot still needs independent historical-vintage validation. No term-curve, causal-Greek, strategy-performance, replay-source or production qualification.",
            "boundary_controls": boundary, "source_mutation_controls": mutation,
            "observations": rows, "sessions": sessions}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    result = audit(args.root)
    raw = (json.dumps(result, indent=2, allow_nan=False) + "\n").encode()
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as f:
        f.write(raw)
        f.flush()
        os.fsync(f.fileno())
    print("128 observations; candidate rate coverage 126 sessions/9324 decisions; 11 controls passed; source qualification pending")


if __name__ == "__main__":
    main()
