#!/usr/bin/env python3
"""Normalize every attested snapshot partition and independently check all cuts.

Offline only. Licensed records/candidates stay private on MSI. This component
does not produce frames, qualify the study or read the live financial journal.
"""
import argparse
from collections import Counter
from datetime import datetime, time, timedelta, timezone
import gzip
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
from zoneinfo import ZoneInfo

ROOT = Path(__file__).resolve().parents[1]
POLICY = "captured_OI_receive_availability_v1"
DAY = 86_400_000_000_000
UNDEF = (1 << 64) - 1
MANIFEST_SHA = "ccf48c229ce29db62e0d4aaa20cb06c733fdf4e528cf2d79f4da0e71a00adb2b"


def sha(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def read(path):
    with path.open("rb") as source:
        raw = source.read((32 << 20) + 1)
    if not raw or len(raw) > 32 << 20:
        raise ValueError("metadata bound exceeded")
    return json.loads(raw)


def save(path, value):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as output:
        json.dump(value, output, indent=2, allow_nan=False)
        output.write("\n")
        output.flush()
        os.fsync(output.fileno())


def timestamp(nanos):
    seconds, fraction = divmod(nanos, 1_000_000_000)
    suffix = "." + f"{fraction:09d}".rstrip("0") if fraction else ""
    return datetime.fromtimestamp(seconds, timezone.utc).strftime("%Y-%m-%dT%H:%M:%S") + suffix + "Z"


def independent(raw, copies, binding, cut):
    """Reconstruct latest-copy semantics from all original seventeen fields."""
    if not copies:
        return [raw, "not_observed", "binding", None, 0, None, None, None]
    row = max(copies.values(), key=lambda item: (item[5], item[0]))
    bad = any(item[14] != 1 or item[15] != 0 or not 0 <= item[9] < (1 << 31) - 1 or
              not 0 < item[6] <= item[5] or item[7] != UNDEF and not 0 < item[7] <= item[5]
              for item in copies.values())
    conflict = len({item[4] for item in copies.values()}) != 1 or len({item[9] for item in copies.values()}) != 1
    definition = binding["Definition"]
    if bad:
        status = "unsupported_latest_copy"
    elif conflict:
        status = "publisher_copy_conflict"
    elif (definition is None or not definition["InstrumentID"] or definition["InstrumentID"] != row[4] or
          not 0 < definition["EventAt"] <= definition["ReceiveAt"] <= cut):
        status = "definition_join_unknown"
    else:
        status = "observed_reference_missing" if row[7] == UNDEF else "observed_reference_present"
    if not status.startswith("observed_reference_") or row[11] != 0:
        return [raw, status, "oi", row, len(copies), None, None, None]
    return [raw, status, "accepted", row, len(copies), row[9], timestamp(row[5]),
            None if row[7] == UNDEF else timestamp(row[7])]


def compare(vector, candidates, config, expected_records):
    bindings = {item["RawSymbol"]: item for item in config["bindings"]}
    symbols, cuts = sorted(bindings), config["cuts"]
    latest, counts, reasons = {}, Counter(), Counter()
    original_hash, candidate_hash = hashlib.sha256(), hashlib.sha256()
    records, oi_records, cut_index, observations, previous = 0, 0, 0, 0, None
    with gzip.open(vector, "rb") as originals, gzip.open(candidates, "rb") as normalized:
        def emit():
            nonlocal cut_index, observations
            line = normalized.readline(2 << 20)
            if not line.endswith(b"\n") or len(line) >= 2 << 20:
                raise ValueError("candidate line/count bound")
            candidate_hash.update(line)
            packet = json.loads(line)
            cut = cuts[cut_index]
            rows = [independent(raw, latest.get(raw, {}), bindings[raw], cut) for raw in symbols]
            if packet != [cut, POLICY, config["vector_jsonl_sha256"], rows]:
                raise ValueError("independent original/candidate projection differs")
            counts.update(row[1] for row in rows)
            reasons.update(row[2] for row in rows)
            observations += len(rows)
            cut_index += 1

        while line := originals.readline(4097):
            if len(line) > 4096 or not line.endswith(b"\n"):
                raise ValueError("native vector line bound")
            original_hash.update(line)
            row = json.loads(line)
            if (len(row) != 17 or row[0] != records or row[1] != config["native_version"] or row[2] not in bindings or
                    row[16] != 24 or not config["utc_start"] <= row[5] < config["utc_start"] + DAY or
                    previous is not None and row[5] < previous):
                raise ValueError("original order, identity or partition differs")
            while cut_index < len(cuts) and row[5] > cuts[cut_index]:
                emit()
            if row[12] == 9:
                oi_records += 1
                latest.setdefault(row[2], {})[row[3]] = row
                if len(latest[row[2]]) > 64:
                    raise ValueError("publisher capacity exceeded")
            previous = row[5]
            records += 1
            if records > expected_records:
                raise ValueError("original count exceeded")
        while cut_index < len(cuts):
            emit()
        if normalized.read(1) or records != expected_records or original_hash.hexdigest() != config["vector_jsonl_sha256"]:
            raise ValueError("original/candidate count, full EOF or hash differs")
    return dict(records=records, OI_records=oi_records, cuts=cut_index, observations=observations, status_counts=dict(counts),
                normalization_counts=dict(reasons), candidate_jsonl_sha256=candidate_hash.hexdigest())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("base", "source-summary", "binary", "output"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--source-summary-sha256", required=True)
    args = parser.parse_args()
    if (args.output.exists() or args.output.parent != args.base or not args.binary.is_relative_to(args.base) or
            any(path.resolve() != path for path in (args.base, args.source_summary, args.binary, args.output))):
        raise ValueError("canonical inputs and fresh output required")
    for path, mode in ((args.base, 0o700), (args.binary, 0o700)):
        info = path.stat()
        if info.st_uid != os.getuid() or info.st_mode & 0o777 != mode:
            raise ValueError("private research namespace or executable required")
    sources = {}

    def bind(path, expected=None):
        path = Path(path)
        # Only the previously attested decoder interpreter is an allowed alias.
        if not path.is_file() or path != args.base / "dbn-env-0.71.0/bin/python" and (path.is_symlink() or path.resolve() != path):
            raise ValueError("unsafe source")
        digest = sha(path)
        if expected is not None and digest != expected or str(path) in sources and sources[str(path)] != digest:
            raise ValueError("source hash differs")
        sources[str(path)] = digest
        return digest

    bind(args.source_summary, args.source_summary_sha256)
    attested = read(args.source_summary)
    covered = attested["coverage"]
    if (attested["schema"] != "AutoWheelAllAcquiredStatisticsOIComponentV1" or attested["go_python_views_equal"] is not True or
            covered["required_partitions"] != 543 or covered["expected_session_dates"] != 126 or
            covered["acquired_partitions"] != covered["audited_partitions"] or attested["fields_per_record"] != 17 or
            any(attested[key] is not False for key in ("source_gate_passed", "full_study_coverage_proved", "historical_performance_available"))):
        raise ValueError("attested provisional scope differs")
    by_hash, configs = {}, {}
    for source in attested["source_files"]:
        path = Path(source["path"])
        bind(path, source["sha256"])
        by_hash.setdefault(source["sha256"], []).append(path)
        if path.name.endswith(".scope.json"):
            config = read(path)
            key = (config["utc_start"], tuple(sorted(item["RawSymbol"] for item in config["bindings"])))
            if key in configs:
                raise ValueError("duplicate captured scope")
            configs[key] = (path, config)
    manifest_path = args.base / "cycle24-selective-acquisition-queries-20261006/manifest.json"
    bind(manifest_path, MANIFEST_SHA)
    planned = {item["sha256"]: item for item in read(manifest_path)["queries"] if item["schema"] == "statistics"}
    if len(planned) != 543 or len({item["date"] for item in planned.values()}) != 126:
        raise ValueError("full frozen plan differs")
    for path in (args.binary, Path(__file__).resolve(), ROOT / "internal/replaynormalize/oi.go", ROOT / "internal/replaynormalize/stock.go",
                 ROOT / "internal/causaloi/oi.go", ROOT / "internal/simulation/types.go", ROOT / "research/simulation/2026-10-05/cycle46-OI-normalizer/main.go"):
        bind(path)
    args.output.mkdir(mode=0o700)
    results, counts, reasons, queries = [], Counter(), Counter(), set()
    for entry in attested["results"]:
        item = planned[entry["query_sha256"]]
        if item["date"] != entry["date"] or item["sha256"] in queries:
            raise ValueError("duplicate or different query")
        queries.add(item["sha256"])
        query_path = manifest_path.parent / item["filename"]
        bind(query_path, item["sha256"])
        query = read(query_path)
        start = int(datetime.strptime(item["date"], "%Y-%m-%d").replace(tzinfo=timezone.utc).timestamp()) * 1_000_000_000
        config_path, config = configs[(start, tuple(sorted(query["symbols"])))]
        origin = datetime.combine(datetime.strptime(item["date"], "%Y-%m-%d").date(), time(), ZoneInfo("America/New_York"))
        cuts = [int((origin + timedelta(minutes=n)).timestamp()) * 1_000_000_000 for n in [*range(575, 945, 5), 960]]
        if (config["schema"] != "AutoWheelNativeOIScopeV1" or config["dataset"] != "OPRA.PILLAR" or
                config["cuts"] != cuts or config["expected_records"] != entry["records"] or len(config["bindings"]) != len(query["symbols"])):
            raise ValueError("frozen closed capture scope differs")
        vectors = [path for path in by_hash[config["vector_sha256"]] if path.name.endswith(".vectors.jsonl.gz")]
        if len(vectors) != 1:
            raise ValueError("attested vector source ambiguous")
        vector = vectors[0]
        output = args.output / item["filename"].removesuffix(".json")
        with (args.output / (item["filename"] + ".private.log")).open("xb") as log:
            os.chmod(log.name, 0o600)
            if subprocess.run([str(args.binary), "--config", str(config_path), "--vectors", str(vector), "--output-directory", str(output)], stdout=log, stderr=log).returncode:
                raise ValueError("private Go normalization failed")
        report_path, candidates = output / "report.json", output / "OI-candidates.jsonl.gz"
        bind(report_path)
        report = read(report_path)
        compared = compare(vector, candidates, config, entry["records"])
        if (report["schema"] != "AutoWheelNativeOINormalizationV1" or report["clock_policy"] != POLICY or
                report["summary"]["Records"] != entry["records"] or report["summary"]["Snapshots"] != 75 or
                report["summary"]["OpenInterestRecords"] != compared["OI_records"] or
                report["summary"]["Observations"] != compared["observations"] or compared["observations"] != entry["observations"] or
                report["status_counts"] != compared["status_counts"] or compared["status_counts"] != entry["status_counts"] or
                report["normalization_counts"] != compared["normalization_counts"] or
                report["views_jsonl_sha256"] != compared["candidate_jsonl_sha256"] or report["views_sha256"] != bind(candidates) or
                report["config_sha256"] != sources[str(config_path)] or report["vector_sha256"] != config["vector_sha256"] or
                report["vector_jsonl_sha256"] != config["vector_jsonl_sha256"] or report["financial_requests"] != 0 or
                any(report[key] is not False for key in ("source_gate_passed", "historical_performance_available", "replay_frames_produced"))):
            raise ValueError("whole candidate comparison or report policy differs")
        counts.update(compared["status_counts"])
        reasons.update(compared["normalization_counts"])
        results.append(dict(date=item["date"], query_sha256=item["sha256"], private_directory=str(output), **compared))
        print(json.dumps(dict(partitions_normalized=len(results), snapshot_partitions=covered["audited_partitions"], observations_compared=sum(r["observations"] for r in results))), flush=True)
    if (len(results) != covered["audited_partitions"] or len(configs) != len(results) or dict(counts) != attested["status_counts"] or
            sum(r["records"] for r in results) != attested["native_records_compared"] or
            sum(r["observations"] for r in results) != attested["contract_cut_observations_compared"]):
        raise ValueError("complete attested snapshot differs")
    for path, digest in sources.items():
        if sha(Path(path)) != digest:
            raise ValueError("source changed during normalization")
    result = dict(schema="AutoWheelOINormalizerEquivalenceV1", observed_at=datetime.now(timezone.utc).isoformat(), coverage=covered,
                  original_native_records=sum(r["records"] for r in results), contract_cut_observations=sum(r["observations"] for r in results),
                  status_counts=dict(counts), normalization_counts=dict(reasons), clock_policy=POLICY, independently_compared=True,
                  source_gate_passed=False, full_source_qualification=False, replay_frames_produced=False, historical_performance_available=False,
                  economic_reference_dates_invented=0, financial_requests=0, live_journal_modified=False, licensed_outputs_retained_private=True,
                  results=results, source_files=[dict(path=p, sha256=s) for p, s in sorted(sources.items())])
    save(args.output / "summary.json", result)
    print(json.dumps({k: v for k, v in result.items() if k not in {"source_files", "results", "coverage", "status_counts"}}), flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception:
        print("OI candidate comparison failed; private evidence retained", file=sys.stderr)
        raise SystemExit(1)
