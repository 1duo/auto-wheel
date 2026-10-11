#!/usr/bin/env python3
"""Offline private Go/Python OI equivalence for the immutable 44-partition audit."""

import argparse
from collections import Counter
from datetime import datetime, timezone
import gzip
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
EXPECTED = (44, 6_630_408, 5_883_900, 75)


def sha(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def save(path, value):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as output:
        json.dump(value, output, separators=(",", ":"), allow_nan=False)
        output.write("\n")
        output.flush()
        os.fsync(output.fileno())


def load(path, sources, expected=None):
    digest = sha(path)
    if expected is not None and digest != expected:
        raise ValueError("immutable source hash differs")
    sources[str(path)] = digest
    return json.loads(path.read_bytes())


def compare(left, right, expected_cuts, expected_symbols, sources):
    counts, records, cuts = Counter(), 0, 0
    hashes = [hashlib.sha256(), hashlib.sha256()]
    with gzip.open(left, "rb") as a, gzip.open(right, "rb") as b:
        while True:
            lines = [a.readline(2 << 20), b.readline(2 << 20)]
            if not lines[0] and not lines[1]:
                break
            if any(not line.endswith(b"\n") or len(line) >= 2 << 20 for line in lines):
                raise ValueError("view count/line/EOF differs")
            for digest, line in zip(hashes, lines):
                digest.update(line)
            rows = [json.loads(line) for line in lines]
            if rows[0] != rows[1] or cuts >= len(expected_cuts) or rows[0][0] != expected_cuts[cuts]:
                raise ValueError("Go/Python closed-cut view differs")
            values = rows[0][1]
            if len(values) != len(expected_symbols) or [v[0] for v in values] != expected_symbols:
                raise ValueError("requested identity roster differs")
            counts.update(v[1] for v in values)
            records += len(values)
            cuts += 1
    if cuts != len(expected_cuts):
        raise ValueError("missing cut or incomplete EOF")
    sources[str(left)], sources[str(right)] = sha(left), sha(right)
    return dict(cuts=cuts, observations=records, status_counts=dict(counts),
                python_decoded_sha256=hashes[0].hexdigest(), go_decoded_sha256=hashes[1].hexdigest())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("base", "source-summary", "binary", "output"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--source-summary-sha256", required=True)
    args = parser.parse_args()
    if args.output.exists() or any(p.resolve() != p for p in (args.base, args.binary, args.source_summary, args.output)):
        raise ValueError("canonical paths and fresh output required")
    sources = {}
    summary = load(args.source_summary, sources, args.source_summary_sha256)
    if (summary["acquired_partitions"], summary["native_records_compared"],
            summary["contract_cut_observations"], summary["snapshot_cuts_per_partition"]) != EXPECTED or any(
            summary[k] is not False for k in ("source_gate_passed", "full_study_coverage_proved", "historical_performance_available")):
        raise ValueError("immutable provisional comparison scope differs")
    for source in summary["source_files"]:
        path = Path(source["path"])
        if sha(path) != source["sha256"]:
            raise ValueError("upstream native/definition/query/reference source differs")
        sources[str(path)] = source["sha256"]
    for path in (args.binary, Path(__file__), ROOT / "internal/causaloi/oi.go",
                 ROOT / "cmd/autowheel-native-oi/main.go"):
        sources[str(path)] = sha(path)
    manifest_path = args.base / "cycle24-selective-acquisition-queries-20261006/manifest.json"
    manifest = load(manifest_path, sources)
    queries = {item["sha256"]: item for item in manifest["queries"] if item["schema"] == "statistics"}
    identity_directory = args.base / "full-definition-identities-20261006"
    identity_audit = load(identity_directory / "audit.json", sources)
    partitions = {p["date"]: p for p in identity_audit["partitions"]}
    args.output.mkdir(mode=0o700)
    results, totals, cached = [], Counter(), {}
    for entry in summary["results"]:
        item = queries[entry["query_file_sha256"]]
        day = entry["date"]
        if item["date"] != day:
            raise ValueError("query date differs")
        query_path = manifest_path.parent / item["filename"]
        query = load(query_path, sources, item["sha256"])
        native_directory = Path(entry["private_views_path"]).parent
        if native_directory.parent != args.source_summary.parent:
            raise ValueError("native view location outside attested scope")
        audit = load(native_directory / "audit.json", sources)
        if len(audit["files"]) != 1 or audit["source_gate_passed"] is not False or audit["query_file_sha256"] != item["sha256"]:
            raise ValueError("native query binding differs")
        member = audit["files"][0]
        vector = native_directory / member["vector_file"]
        if sha(vector) != member["vector_sha256"] or member["records"] != entry["records"]:
            raise ValueError("native vector hash/count differs")
        sources[str(vector)] = member["vector_sha256"]
        if day not in cached:
            part = partitions[day]
            path = identity_directory / part["identity_file"]
            if sha(path) != part["identity_sha256"]:
                raise ValueError("definition compressed hash differs")
            sources[str(path)] = part["identity_sha256"]
            definitions, count, digest = {}, 0, hashlib.sha256()
            with gzip.open(path, "rb") as source:
                for line in source:
                    if len(line) > 4096 or not line.endswith(b"\n"):
                        raise ValueError("definition line bound differs")
                    digest.update(line)
                    row = json.loads(line)
                    if len(row) != 9 or row[2] in definitions:
                        raise ValueError("captured definition identity ambiguous")
                    definitions[row[2]] = row
                    count += 1
            if count != part["records"] or digest.hexdigest() != part["identity_jsonl_sha256"]:
                raise ValueError("definition decoded count/hash/EOF differs")
            cached[day] = definitions
        definitions = cached[day]
        python_views = Path(entry["private_views_path"])
        if sha(python_views) != entry["private_views_sha256"]:
            raise ValueError("reference view hash differs")
        with gzip.open(python_views, "rb") as source:
            cuts = [json.loads(line)[0] for line in source]
        start = int(datetime.strptime(day, "%Y-%m-%d").replace(tzinfo=timezone.utc).timestamp()) * 1_000_000_000
        # InstrumentID zero avoids treating a missing or mismatched definition
        # as a daily mapping. The audited native reference separately verifies
        # every record's daily mapping; the reducer verifies identity stability.
        bindings = []
        for raw in sorted(query["symbols"]):
            d = definitions.get(raw)
            bindings.append(dict(RawSymbol=raw, InstrumentID=0, Definition=None if d is None else
                                 dict(InstrumentID=d[1], ReceiveAt=d[3], EventAt=d[4])))
        config = dict(schema="AutoWheelNativeOIScopeV1", dataset="OPRA.PILLAR", utc_start=start,
                      native_version=member["native_version"], expected_records=member["records"],
                      vector_sha256=member["vector_sha256"], vector_jsonl_sha256=member["vector_jsonl_sha256"],
                      bindings=bindings, cuts=cuts)
        config_path = args.output / (item["filename"].removesuffix(".json") + ".scope.json")
        save(config_path, config)
        sources[str(config_path)] = sha(config_path)
        output = args.output / item["filename"].removesuffix(".json")
        log_path = args.output / (item["filename"] + ".private.log")
        with log_path.open("xb") as log:
            os.chmod(log_path, 0o600)
            if subprocess.run([str(args.binary), "--config", str(config_path), "--vectors", str(vector),
                               "--output-directory", str(output)], stdout=log, stderr=log).returncode != 0:
                raise ValueError("Go export failed; private diagnostics retained")
        go = load(output / "report.json", sources)
        compared = compare(python_views, output / "OI-views.jsonl.gz", cuts, sorted(query["symbols"]), sources)
        if go["summary"] != dict(Records=member["records"], OpenInterestRecords=member["counts_by_stat_type"]["9"],
                                 Snapshots=75, Observations=entry["queried_symbols"]*75) or (
                compared["status_counts"] != entry["status_counts"] or go["status_counts"] != compared["status_counts"] or
                go["views_jsonl_sha256"] != compared["go_decoded_sha256"] or
                go["views_sha256"] != sources[str(output / "OI-views.jsonl.gz")] or
                go["config_sha256"] != sources[str(config_path)] or go["source_gate_passed"] is not False or
                go["historical_performance_available"] is not False or go["financial_requests"] != 0):
            raise ValueError("Go aggregate/source-policy result differs")
        totals.update(compared["status_counts"])
        results.append(dict(date=day, records=member["records"], query_sha256=item["sha256"],
                            private_go_directory=str(output), **compared))
        print(json.dumps(dict(completed_partitions=len(results), required_partitions=44,
                              observations_compared=sum(v["observations"] for v in results))), flush=True)
    if len(results) != 44 or sum(v["observations"] for v in results) != EXPECTED[2] or dict(totals) != summary["status_counts"]:
        raise ValueError("whole bounded comparison scope differs")
    for path, digest in sources.items():
        if sha(Path(path)) != digest:
            raise ValueError("source changed during comparison")
    report = dict(schema="AutoWheelNativeOIEquivalenceV1", observed_at=datetime.now(timezone.utc).isoformat(),
                  acquired_partitions=44, full_plan_partitions=543, native_records_compared=EXPECTED[1],
                  contract_cut_observations_compared=EXPECTED[2], cuts_per_partition=75,
                  go_python_views_equal=True, status_counts=dict(totals), financial_requests=0,
                  source_gate_passed=False, full_study_coverage_proved=False, historical_performance_available=False,
                  licensed_outputs_retained_private=True, results=results,
                  source_files=[dict(path=p, sha256=s) for p,s in sorted(sources.items())])
    save(args.output / "summary.json", report)
    print(json.dumps({k:v for k,v in report.items() if k not in {"results", "source_files", "status_counts"}}), flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception:
        print("native OI equivalence failed; private evidence retained", file=sys.stderr)
        raise SystemExit(1)
