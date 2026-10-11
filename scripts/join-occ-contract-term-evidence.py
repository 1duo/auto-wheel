#!/usr/bin/env python3
"""Join the full near-DTE roster to definitions and retrospective OCC evidence.

Licensed contract rows stay private. Missing facts remain null; this join never
asserts standard terms, historical availability, roster eligibility or P&L.
"""
import argparse
from collections import Counter
from datetime import datetime, time, timezone
import gzip
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re

ROOT = Path(__file__).resolve().parents[1]
DIRECTORY = ROOT / "research/simulation/2026-10-05"
SYMBOLS = {"NFLX", "TSLA", "NVDA", "NBIS", "IREN", "NOW", "AMD", "AMZN", "GOOGL", "META", "MSFT", "PLTR", "UBER", "AVGO", "ORCL"}
MANIFEST_SHA = "ccf48c229ce29db62e0d4aaa20cb06c733fdf4e528cf2d79f4da0e71a00adb2b"
FIELDS = ["publisher_id", "instrument_id", "raw_symbol", "ts_recv", "ts_event", "strike_price_nanodollars", "expiration_date_timestamp_ns", "instrument_class", "security_update_action"]


def digest(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def osi(raw):
    if not isinstance(raw, str) or len(raw) != 21 or not raw.isascii():
        raise ValueError("raw OSI width differs")
    root = raw[:6].rstrip()
    if root not in SYMBOLS or raw[:6] != root.ljust(6) or not re.fullmatch(r"\d{6}[CP]\d{8}", raw[6:]):
        raise ValueError("raw OSI identity outside frozen universe")
    expiry = datetime.strptime("20" + raw[6:12], "%Y%m%d").date()
    return root, expiry, raw[12], int(raw[13:]) * 1_000_000


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.output.exists() or args.base.resolve() != args.base:
        raise ValueError("fresh private output and canonical base required")
    manifest_path = args.base / "cycle24-selective-acquisition-queries-20261006/manifest.json"
    if digest(manifest_path) != MANIFEST_SHA:
        raise ValueError("frozen acquisition manifest differs")
    manifest = json.loads(manifest_path.read_bytes())
    qualification = json.loads((DIRECTORY / "databento-qualification.json").read_bytes())
    identity_path = ROOT / qualification["source_progress"]["full_definition_identities"]["audit_path"]
    if digest(identity_path) != qualification["source_progress"]["full_definition_identities"]["audit_sha256"]:
        raise ValueError("definition identity audit differs")
    identity = json.loads(identity_path.read_bytes())
    if identity["identity_jsonl_fields"] != FIELDS or identity["daily_partitions"] != 126 or identity["source_gate_passed"] is not False:
        raise ValueError("identity audit shape differs")
    identities = Path(qualification["source_progress"]["full_definition_identities"]["private_directory"])
    ess_path = DIRECTORY / "cycle39-public-primary/occ-daily-ess-acquisition.json"
    ess_audit_path = DIRECTORY / "cycle39-public-primary/occ-daily-ess-audit.json"
    ess = json.loads(ess_path.read_bytes())
    ess_audit = json.loads(ess_audit_path.read_bytes())
    module_path = ROOT / "scripts/audit-occ-ess-reports.py"
    module_spec = importlib.util.spec_from_file_location("occ_ess_audit", module_path)
    module = importlib.util.module_from_spec(module_spec)
    module_spec.loader.exec_module(module)
    expected_sources = {x["path"]:x["sha256"] for x in ess_audit["source_files"]}
    if expected_sources[str(module_path.relative_to(ROOT))] != digest(module_path) or expected_sources[str(ess_path.relative_to(ROOT))] != digest(ess_path):
        raise ValueError("OCC audit/source binding differs")
    sources = {str(manifest_path): MANIFEST_SHA, str(identity_path):digest(identity_path),
               str(ess_path):digest(ess_path), str(ess_audit_path):digest(ess_audit_path),
               str(module_path):digest(module_path), str(Path(__file__)):digest(Path(__file__))}
    products = {}
    for receipt in ess["receipts"]:
        p = ROOT / receipt["path"]
        if p.resolve() != p or digest(p) != receipt["sha256"]:
            raise ValueError("OCC original differs")
        day = datetime.strptime(receipt["url"].split("reportDate=")[-1], "%Y%m%d").date().isoformat()
        data = p.read_bytes()
        module.parse(data, day)
        products[day] = Counter(line.split()[1] for line in data.decode("ascii").splitlines() if line.startswith("0706"))
        sources[str(p)] = receipt["sha256"]
    roster = {}
    for query in manifest["queries"]:
        if query["schema"] != "statistics":
            continue
        p = manifest_path.parent / query["filename"]
        if digest(p) != query["sha256"]:
            raise ValueError("immutable query differs")
        value = json.loads(p.read_bytes())
        current = roster.setdefault(query["date"], set())
        if len(value["symbols"]) != len(set(value["symbols"])) or current.intersection(value["symbols"]):
            raise ValueError("statistics roster partitions overlap")
        current.update(value["symbols"])
        sources[str(p)] = query["sha256"]
    days = sorted(roster)
    if days != ess["session_dates"] or len(days) != 126 or sum(map(len, roster.values())) != 955003:
        raise ValueError("full near-DTE contract-day scope differs")
    if [p["date"] for p in identity["partitions"]] != days:
        raise ValueError("identity partition dates differ")
    args.output.mkdir(mode=0o700)
    totals, daily, bytes_out = Counter(), [], Counter()
    for index, partition in enumerate(identity["partitions"]):
        day = partition["date"]
        next_day = days[index + 1] if index + 1 < len(days) else "2026-10-05"
        p = identities / partition["identity_file"]
        if p.resolve() != p or digest(p) != partition["identity_sha256"]:
            raise ValueError("licensed identity original differs")
        sources[str(p)] = partition["identity_sha256"]
        matched, count, uncompressed = {}, 0, hashlib.sha256()
        with gzip.open(p, "rb") as source:
            for line in source:
                if len(line) > 1_048_576:
                    raise ValueError("identity line exceeds bound")
                uncompressed.update(line)
                row = json.loads(line)
                if not isinstance(row, list) or len(row) != len(FIELDS) or row[0] != 30 or row[8] != "A":
                    raise ValueError("definition row shape/category differs")
                count += 1
                raw = row[2]
                if raw not in roster[day]:
                    continue
                root, expiry, side, strike = osi(raw)
                if side != row[7] or strike != row[5] or datetime.fromtimestamp(row[6] // 1_000_000_000, timezone.utc).date() != expiry:
                    raise ValueError("raw OSI and definition terms disagree")
                if raw in matched and [matched[raw][k] for k in (0,1,2,5,6,7,8)] != [row[k] for k in (0,1,2,5,6,7,8)]:
                    raise ValueError("definition identity changes within captured day")
                if raw not in matched or row[3] < matched[raw][3]:
                    matched[raw] = row
        if count != partition["records"] or uncompressed.hexdigest() != partition["identity_jsonl_sha256"]:
            raise ValueError("identity decoded count/hash differs")
        cut = int(datetime.combine(datetime.strptime(day, "%Y-%m-%d").date(), time(13,35), timezone.utc).timestamp()) * 1_000_000_000
        counters = Counter()
        path = args.output / (day + ".term-evidence.jsonl.gz")
        with path.open("xb") as raw_output:
            os.chmod(path,0o600)
            with gzip.GzipFile(filename="", fileobj=raw_output, mode="wb", mtime=0) as output:
                for raw in sorted(roster[day]):
                    root, expiry, side, strike = osi(raw)
                    if not 0 <= (expiry - datetime.strptime(day,"%Y-%m-%d").date()).days <= 8:
                        raise ValueError("contract outside frozen near-DTE scope")
                    row = matched.get(raw)
                    available = row is not None and row[3] <= cut and row[4] <= row[3]
                    current, later = products[day][root], products[next_day][root]
                    counters["contract_days"] += 1
                    counters["definition_observed"] += row is not None
                    counters["definition_observed_by_first_decision"] += available
                    counters["definition_missing"] += row is None
                    counters["exact_ESS_product_reference_current_or_next"] += bool(current or later)
                    evidence = dict(date=day, raw_symbol=raw, root=root,
                                    definition_identity=row, definition_observed_by_first_decision=available,
                                    ESS_report_date=day, ESS_exact_product_records=current,
                                    next_session_ESS_report_date=next_day, next_session_ESS_exact_product_records=later,
                                    next_session_ESS_use="retrospective_reconciliation_only",
                                    premium_dollar_multiplier=None, shares_per_contract=None, minimum_tick=None,
                                    exercise_style=None, settlement_days=None, known_at=None,
                                    status="unqualified_evidence_join", source_gate_passed=False)
                    output.write((json.dumps(evidence,separators=(",",":"),allow_nan=False)+"\n").encode())
        daily.append(dict(date=day, **counters, output_path=str(path), output_sha256=digest(path)))
        totals.update(counters)
        if (index + 1) % 20 == 0:
            print(json.dumps({"joined_sessions":index+1,"contract_days":totals["contract_days"],"source_gate_passed":False}),flush=True)
    if totals["contract_days"] != 955003:
        raise ValueError("joined contract-day total differs")
    for path,sha in sources.items():
        if digest(Path(path)) != sha:
            raise ValueError("source changed during join")
    report = dict(schema="AutoWheelOCCContractTermEvidenceJoinV1",observed_at=datetime.now(timezone.utc).isoformat(),
                  daily_partitions=126,**totals,daily_audits=daily,licensed_rows_retained_private=True,
                  per_series_terms_qualified=False,point_in_time_roster_qualified=False,source_gate_passed=False,
                  historical_performance_available=False,financial_requests=0,
                  source_files=[dict(path=p,sha256=s) for p,s in sorted(sources.items())],
                  limitations=["Exact ESS product absence is a retrospective observation, not a standard-term or historical-availability proof.",
                               "Mapping-only subjects without captured definitions remain explicit and are not dropped from the full acquisition universe.",
                               "Generic terms, minimum tick and first tradability still require corroboration; no missing field is defaulted."])
    path = args.output / "summary.json"
    with path.open("x") as output:
        os.chmod(path,0o600);json.dump(report,output,indent=2,allow_nan=False);output.write("\n")
    print(json.dumps({k:v for k,v in report.items() if k not in {"source_files","daily_audits"}}))


if __name__ == "__main__":
    main()
