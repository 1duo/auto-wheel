#!/usr/bin/env python3
"""Offline comparison of dated public SOFR vintages with the frozen rate candidate.

No network, credentials, broker or financial requests. Daily vintage dates are
not intraday publication timestamps; the flat future-rate assumption remains.
"""
import argparse
import csv
from datetime import date, datetime, timedelta, timezone
from decimal import Decimal
import hashlib
import io
import json
import os
from pathlib import Path
import re
import sys
import urllib.parse

ROOT = Path(__file__).resolve().parents[1]
RELATIVE = Path("research/simulation/2026-10-05")
CANDIDATE_SHA = "ac0f8dd3173e505338fe9afa545f9553929808db16e27068d241a18f8f15f156"
SPEC_SHA = "2ff4ae3765f4102a67382d645164ae4dfb6e036cc7ea3b113eab6dc24f4eac98"
ACQUISITION_SHA = "b3d69527ac2e014791265dafa42b0e24c4881bf6fc47c9f063179324f5b90dd9"
POLICY = "previous_calendar_day_vintage_at_frozen_decision_cuts_v1"


def unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate metadata field")
        result[key] = value
    return result


def parse(raw, vintage, known):
    if not 0 < len(raw) <= 1 << 20:
        raise ValueError("CSV bound")
    reader = csv.reader(io.StringIO(raw.decode("utf-8-sig"), newline=""), strict=True)
    if next(reader, None) != ["observation_date", "SOFR_" + vintage.replace("-", "")]:
        raise ValueError("dated series header differs")
    seen, values = set(), {}
    for row in reader:
        if len(row) != 2 or len(seen) >= 200:
            raise ValueError("CSV row bound")
        observed = date.fromisoformat(row[0])
        if observed.isoformat() != row[0] or not "2026-04-01" <= row[0] <= "2026-10-02" or row[0] in seen:
            raise ValueError("observation date differs")
        seen.add(row[0])
        if row[1] in ("", "."):
            continue
        if (not re.fullmatch(r"-?[0-9]+(?:\.[0-9]{1,10})?", row[1]) or row[0] not in known or
                known[row[0]]["expected_publication_date"] > vintage):
            raise ValueError("unsupported or future observation")
        value = Decimal(row[1])
        if not Decimal(-5) <= value <= Decimal(50) or value != Decimal(known[row[0]]["percent_act360"]):
            raise ValueError("archived rate differs from reviewed candidate")
        values[row[0]] = value
    expected = {observed for observed, item in known.items() if item["expected_publication_date"] <= vintage}
    if set(values) != expected:
        raise ValueError("complete available observation set differs")
    return values


def controls(raw, vintage, known, current):
    original = list(csv.reader(io.StringIO(raw.decode("utf-8-sig"))))
    present = next(row for row in original[1:] if row[1] not in ("", "."))
    future = next(row for row in current[1:] if row[1] not in ("", ".") and known[row[0]]["expected_publication_date"] > vintage)
    results = []
    for name in ("current_vintage_header", "future_value", "duplicate_date", "nonfinite_value", "unknown_date", "different_rate", "missing_value"):
        rows = [list(row) for row in original]
        if name == "current_vintage_header":
            rows[0][1] = "SOFR_20261006"
        elif name == "future_value":
            rows = [rows[0], *[row for row in rows[1:] if row[0] != future[0]], list(future)]
        elif name == "duplicate_date":
            rows.append(list(present))
        elif name == "missing_value":
            rows = [row for row in rows if row[0] != present[0]]
        else:
            changed = list(present)
            changed[1] = "NaN" if name == "nonfinite_value" else str(Decimal(changed[1]) + 1)
            if name == "unknown_date":
                changed[0] = "2026-04-03"  # Reviewed no-SOFR date.
            rows = [rows[0], *[row for row in rows[1:] if row[0] != changed[0] and row[0] != present[0]], changed]
        text = io.StringIO(newline="")
        csv.writer(text).writerows(rows)
        try:
            parse(text.getvalue().encode(), vintage, known)
        except ValueError:
            results.append(dict(name=name, rejected=True))
        else:
            raise ValueError("mutation accepted")
    return results


def selected_effective_date(item, values, known):
    if (not values or max(values) != item["rate"]["effective_date"] or
            item["rate"] != known[max(values)] or
            type(item["rate"]["percent_act360"]) is not str or
            Decimal(item["rate"]["percent_act360"]) != values[max(values)]):
        raise ValueError("latest archived observation differs from frozen candidate")
    return max(values)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if (args.output.exists() or args.source.resolve() != args.source.absolute() or
            args.source.parent.resolve() != ROOT / RELATIVE or args.output.parent.resolve() != ROOT / RELATIVE):
        raise ValueError("canonical sources and fresh bounded output required")
    sources = {}

    def read(path, expected=None):
        path = path if path.is_absolute() else ROOT / path
        if path.is_symlink() or not path.is_file() or path.stat().st_size > 4 << 20:
            raise ValueError("bounded original source required")
        raw = path.read_bytes()
        digest = hashlib.sha256(raw).hexdigest()
        if expected is not None and digest != expected or str(path) in sources and sources[str(path)] != digest:
            raise ValueError("source digest differs")
        sources[str(path)] = digest
        return raw

    def metadata(path, expected=None):
        return json.loads(read(path, expected), object_pairs_hook=unique,
                          parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite metadata")))

    read(ROOT / RELATIVE / "spec.jsonl", SPEC_SHA)
    candidate = metadata(ROOT / RELATIVE / "public-inputs/sofr-candidate-audit-20261006.json", CANDIDATE_SHA)
    known = {item["effective_date"]: item for item in candidate["observations"]}
    if len(known) != 128 or len(candidate["sessions"]) != 126 or candidate["candidate_rate_decisions"] != 9324:
        raise ValueError("full frozen candidate scope differs")
    acquired = metadata(args.source / "receipt.json")
    if (acquired["schema"] != "AutoWheelALFREDSOFRDailyVintageAcquisitionV1" or acquired["vintage_count"] != 128 or
            len(acquired["receipts"]) != 128 or acquired["spec_sha256"] != SPEC_SHA or acquired["candidate_sha256"] != CANDIDATE_SHA or
            acquired["credentials_used"] is not False or acquired["financial_requests"] != 0):
        raise ValueError("public acquisition scope differs")
    if acquired["script_sha256"] != ACQUISITION_SHA:
        raise ValueError("reviewed acquisition version differs")
    read(ROOT / "scripts/acquire-alfred-sofr-vintages.py", ACQUISITION_SHA)
    read(Path(__file__).resolve())
    expected = {(date.fromisoformat(item["session"]) - timedelta(days=1)).isoformat() for item in candidate["sessions"]}
    expected |= {"2026-04-02", "2026-10-06"}
    vintages, originals = {}, {}
    for receipt in acquired["receipts"]:
        vintage = receipt["vintage_date"]
        if vintage not in expected or vintage in vintages:
            raise ValueError("duplicate or unrequested vintage")
        fields = dict(id="SOFR", cosd="2026-04-01", coed="2026-10-02", vintage_date=vintage, revision_date=vintage, mode="alfred")
        url = "https://alfred.stlouisfed.org/graph/alfredgraph.csv?" + urllib.parse.urlencode(fields)
        path = args.source / (vintage + ".csv")
        if (receipt["request_url"] != url or receipt["method"] != "GET" or receipt["http_status"] != 200 or
                receipt["credentials_used"] is not False or receipt["financial_requests"] != 0 or
                Path(receipt["source_path"]).absolute() != path.absolute()):
            raise ValueError("original request identity differs")
        if metadata(args.source / (vintage + ".receipt.json")) != receipt:
            raise ValueError("original receipt differs")
        raw = read(path, receipt["source_sha256"])
        if len(raw) != receipt["source_bytes"]:
            raise ValueError("source byte count differs")
        originals[vintage] = raw
        vintages[vintage] = parse(raw, vintage, known)
    if set(vintages) != expected or set(vintages["2026-10-06"]) != set(known) or set(vintages["2026-04-02"]) != {"2026-04-01"}:
        raise ValueError("full scope or future-suppression boundary differs")
    selections = []
    for item in candidate["sessions"]:
        vintage = (date.fromisoformat(item["session"]) - timedelta(days=1)).isoformat()
        values = vintages[vintage]
        selected_effective_date(item, values, known)
        selections.append(dict(session=item["session"], vintage_date=vintage,
                               effective_date=max(values), source_sha256=sources[str((args.source / (vintage + ".csv")).absolute())],
                               first_decision_utc=item["first_decision_utc"], last_decision_utc=item["last_decision_utc"]))
    mutated = controls(originals["2026-04-02"], "2026-04-02", known,
                       list(csv.reader(io.StringIO(originals["2026-10-06"].decode("utf-8-sig")))))
    first = candidate["sessions"][0]
    changed = dict(first, rate=dict(first["rate"]))
    changed["rate"]["percent_act360"] = str(Decimal(changed["rate"]["percent_act360"]) + 1)
    try:
        selected_effective_date(changed, vintages[selections[0]["vintage_date"]], known)
    except ValueError:
        mutated.append(dict(name="different_session_rate", rejected=True))
    else:
        raise ValueError("session rate mutation accepted")
    for path, digest in sources.items():
        if hashlib.sha256(Path(path).read_bytes()).hexdigest() != digest:
            raise ValueError("source changed during audit")
    if sum(map(len, vintages.values())) != sum(sum(item["expected_publication_date"] <= vintage for item in known.values()) for vintage in expected):
        raise ValueError("full available-cell accounting differs")
    result = dict(schema="AutoWheelALFREDSOFRDailyVintageComparisonV2", observed_at=datetime.now(timezone.utc).isoformat(),
                  vintage_count=128, session_dates_compared=126, decision_cuts_supported=9324, closing_valuation_cuts_supported=126,
                  daily_vintage_comparison_verified=True, complete_available_sets_verified=True, reviewed_acquisition_version_verified=True,
                  original_observations_compared=sum(map(len, vintages.values())),
                  full_series_observations_compared=128, candidate_selection_and_values_match=True,
                  availability_policy=POLICY, intraday_publication_timestamps_invented=0, missing_rates_filled=0,
                  mutation_controls=mutated, mutation_controls_passed=len(mutated), selections=selections,
                  future_rate_model="Previously disclosed flat one-day ACT/360-to-continuous-ACT/365 proxy; not an observed OIS forward curve",
                  source_qualified=False, historical_greeks_qualified=False, replay_frames_produced=False, historical_performance_available=False,
                  credentials_used=False, financial_requests=0, source_files=[dict(path=path, sha256=digest) for path, digest in sorted(sources.items())])
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "w") as output:
        json.dump(result, output, indent=2, allow_nan=False)
        output.write("\n")
        output.flush()
        os.fsync(output.fileno())
    print(json.dumps({key: value for key, value in result.items() if key not in ("source_files", "selections", "mutation_controls")}), flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception:
        print("public SOFR daily vintage comparison failed closed", file=sys.stderr)
        raise SystemExit(1)
