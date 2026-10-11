#!/usr/bin/env python3
"""Reconcile the private join against immutable queries and prior mapping gaps."""
import argparse
from collections import Counter
from datetime import datetime, timezone
import gzip
import hashlib
import importlib.util
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
DIRECTORY = ROOT / "research/simulation/2026-10-05"
MANIFEST_SHA = "ccf48c229ce29db62e0d4aaa20cb06c733fdf4e528cf2d79f4da0e71a00adb2b"


def sha(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", type=Path, required=True)
    parser.add_argument("--join", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    identity = module("term_join", ROOT / "scripts/join-occ-contract-term-evidence.py")
    titles = module("term_titles", ROOT / "scripts/enumerate-occ-term-candidates.py")
    # Bounded identity and title controls include actual failure cases that could
    # accidentally transfer another product's terms or accept an adjusted root.
    assert titles.classify("NVDA ETF - Option Symbol: NVDU") == (["NVDA"], [])
    assert titles.classify("NFLX - Adjusted Option Symbols: NFLX1") == (["NFLX"], [])
    assert titles.classify("NFLX - Option Symbol: NFLX") == (["NFLX"], ["NFLX"])
    assert titles.classify("SNOW - Option Symbol: SNOW") == ([], [])
    assert identity.osi("TSLA  260422C00255000")[2:] == ("C", 255_000_000_000)
    malformed = ["TSLA1 260422C00255000", "TSLA  260422X00255000",
                 "TSLA  261332C00255000", "TSLA  260422C0025500", "TSLA\t 260422C00255000"]
    for raw in malformed:
        try:
            identity.osi(raw)
        except ValueError:
            continue
        raise ValueError("malformed OSI accepted")
    manifest_path = args.base / "cycle24-selective-acquisition-queries-20261006/manifest.json"
    if sha(manifest_path) != MANIFEST_SHA:
        raise ValueError("frozen acquisition manifest differs")
    manifest = json.loads(manifest_path.read_bytes())
    summary_path = args.join / "summary.json"
    summary = json.loads(summary_path.read_bytes())
    prior_path = DIRECTORY / "cycle23-definition-mapping-gap-audit.json"
    qualification = json.loads((DIRECTORY / "databento-qualification.json").read_bytes())
    if sha(prior_path) != qualification["source_progress"]["full_definition_mapping_gap_audit"]["audit_sha256"]:
        raise ValueError("prior mapping audit differs")
    prior = {x["date"]: x for x in json.loads(prior_path.read_bytes())["partitions"]}
    requested = {}
    sources = {str(manifest_path): MANIFEST_SHA, str(summary_path): sha(summary_path),
               str(prior_path): sha(prior_path), str(Path(__file__)): sha(Path(__file__)),
               str(ROOT / "scripts/join-occ-contract-term-evidence.py"): sha(ROOT / "scripts/join-occ-contract-term-evidence.py"),
               str(ROOT / "scripts/enumerate-occ-term-candidates.py"): sha(ROOT / "scripts/enumerate-occ-term-candidates.py")}
    for query in manifest["queries"]:
        if query["schema"] != "statistics":
            continue
        p = manifest_path.parent / query["filename"]
        if sha(p) != query["sha256"]:
            raise ValueError("query original differs")
        values = json.loads(p.read_bytes())["symbols"]
        bucket = requested.setdefault(query["date"], set())
        if len(set(values)) != len(values) or bucket.intersection(values):
            raise ValueError("query roster overlap")
        bucket.update(values)
        sources[str(p)] = query["sha256"]
    counts, daily = Counter(), []
    term_fields = ("premium_dollar_multiplier", "shares_per_contract", "minimum_tick",
                   "exercise_style", "settlement_days", "known_at")
    for audit in summary["daily_audits"]:
        day, p = audit["date"], Path(audit["output_path"])
        if p.parent != args.join or sha(p) != audit["output_sha256"]:
            raise ValueError("private join original differs")
        sources[str(p)] = audit["output_sha256"]
        seen, missing, buckets = set(), set(), Counter()
        with gzip.open(p, "rt") as source:
            for line in source:
                row = json.loads(line)
                raw = row["raw_symbol"]
                if raw in seen or raw not in requested[day] or row["date"] != day:
                    raise ValueError("join identity/date/uniqueness differs")
                seen.add(raw)
                if any(row[k] is not None for k in term_fields) or row["source_gate_passed"] is not False:
                    raise ValueError("unknown term default or qualification introduced")
                if row["status"] != "unqualified_evidence_join" or row["next_session_ESS_use"] != "retrospective_reconciliation_only":
                    raise ValueError("join evidence status differs")
                definition = row["definition_identity"]
                if definition is None:
                    if row["definition_observed_by_first_decision"] is not False:
                        raise ValueError("missing definition claimed observed")
                    expiry = datetime.strptime(raw[6:12], "%y%m%d").date()
                    dte = (expiry - datetime.strptime(day, "%Y-%m-%d").date()).days
                    if not 0 <= dte <= 8:
                        raise ValueError("missing subject outside scope")
                    bucket = "same_day" if dte == 0 else "DTE1_8"
                    buckets[bucket] += 1
                    if dte:
                        missing.add(raw)
                elif definition[2] != raw or row["definition_observed_by_first_decision"] is not True:
                    raise ValueError("captured definition join differs")
                if row["ESS_exact_product_records"] != 0 or row["next_session_ESS_exact_product_records"] != 0:
                    raise ValueError("unexpected ESS root reference requires review")
        previous = prior[day]
        if seen != requested[day] or missing != {x[0] for x in previous["missing_DTE1_8"]}:
            raise ValueError("requested scope or prior missing identities differ")
        if any(buckets[k] != previous["missing_expiry_buckets"].get(k, 0) for k in ("same_day", "DTE1_8")):
            raise ValueError("prior missing same-day/DTE1_8 count differs")
        if sum(buckets.values()) != audit["definition_missing"]:
            raise ValueError("missing count differs from join summary")
        counts.update(buckets)
        counts["contract_days"] += len(seen)
        daily.append(dict(date=day, contract_days=len(seen), missing_definition=dict(buckets)))
    if len(daily) != 126 or counts != Counter(contract_days=955003, DTE1_8=54, same_day=9):
        raise ValueError("full scope totals differ")
    for path, digest in sources.items():
        if sha(Path(path)) != digest:
            raise ValueError("source changed during audit")
    report = dict(schema="AutoWheelOCCContractTermJoinAuditV1", observed_at=datetime.now(timezone.utc).isoformat(),
                  full_query_roster_exactly_retained=True, prior_mapping_gap_identity_counts_match=True,
                  contract_days=955003, daily_partitions=126, missing_definition_contract_days=63,
                  missing_definition_DTE1_8=54, missing_definition_same_day=9,
                  unknown_terms_remain_null=True, public_title_controls_passed=4, OSI_controls_passed=6,
                  licensed_rows_retained_private=True, per_series_terms_qualified=False,
                  source_gate_passed=False, historical_performance_available=False, financial_requests=0,
                  daily_counts=daily, source_files=[dict(path=p, sha256=s) for p,s in sorted(sources.items())])
    with args.output.open("x") as output:
        json.dump(report, output, indent=2, allow_nan=False)
        output.write("\n")
    print(json.dumps({k:v for k,v in report.items() if k not in {"daily_counts", "source_files"}}))


if __name__ == "__main__":
    main()
