#!/usr/bin/env python3
"""Classify all daily mapping/definition differences without inferring tradability."""
import argparse
from collections import Counter
from datetime import date, datetime, timezone
import gzip
import hashlib
import importlib.metadata
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile


IDENTITY_SHA = "d6eeb33ec9000310f231a42371edf639e055ab1a7e6afe049370fd1d567f4e7d"


def load_script(path, name, sha):
    if hashlib.sha256(path.read_bytes()).hexdigest() != sha:
        raise ValueError("audit dependency changed")
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def array_hash(values):
    return hashlib.sha256(json.dumps(values, separators=(",", ":"), ensure_ascii=True).encode()).hexdigest()


def expiry_bucket(days):
    return "expired" if days < 0 else "same_day" if days == 0 else "DTE1_8" if days <= 8 else "later"


def native_metadata(path, dbn):
    """Read only the original header; full-file decoding is independently bound."""
    decoder = dbn.DBNDecoder(upgrade_policy=dbn.VersionUpgradePolicy.AS_IS,
                             compression=dbn.Compression.NONE)
    with tempfile.TemporaryFile() as errors:
        proc = subprocess.Popen(["/usr/bin/zstd", "-q", "-d", "-c", "--", str(path)],
                                stdout=subprocess.PIPE, stderr=errors)
        received = 0
        try:
            while received <= 32 << 20:
                chunk = proc.stdout.read(128 << 10)
                if not chunk:
                    raise ValueError("metadata not found")
                received += len(chunk)
                for row in decoder.write_and_decode(chunk):
                    if isinstance(row, dbn.Metadata):
                        return row
                    raise ValueError("record before metadata")
            raise ValueError("metadata exceeds audit bound")
        finally:
            proc.stdout.close()
            if proc.poll() is None:
                proc.terminate()
            proc.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--identity-audit", type=Path, required=True)
    parser.add_argument("--census", type=Path, required=True)
    parser.add_argument("--receipt", type=Path, required=True)
    parser.add_argument("--query", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.output.exists():
        raise ValueError("audit output already exists")
    identity = load_script(Path(__file__).with_name("audit-definition-identities.py"), "identity_audit", IDENTITY_SHA)
    source = load_script(Path(__file__).with_name("audit-definition-batch.py"), "batch_audit", identity.DEPENDENCY_SHA)
    if importlib.metadata.version("databento-dbn") != "0.71.0":
        raise ValueError("pinned decoder required")
    import databento_dbn as dbn
    receipt, receipt_sha, query_sha, job, files = source.verified_source(args.receipt, args.query)
    audit, audit_sha = source.read_json(args.identity_audit)
    census, census_sha = source.read_json(args.census)
    if (audit.get("script_sha256") != IDENTITY_SHA or audit.get("receipt_sha256") != receipt_sha
            or audit.get("query_file_sha256") != query_sha or audit.get("records") != job.get("record_count")
            or audit.get("source_gate_passed") is not False or audit.get("replay_frames_produced") is not False
            or len(audit.get("partitions", [])) != 126 or census.get("receipt_sha256") != receipt_sha
            or census.get("records") != audit["records"] or census.get("script_sha256") != identity.DEPENDENCY_SHA):
        raise ValueError("original census or identity audit binding differs")
    rows_by_file = {f["filename"]: f for f in census["files"]}
    sources_by_file = {p.name: (p, sha) for p, sha, size in files}
    roots = {s[:-4] for s in receipt["query"]["symbols"]}
    results, totals = [], Counter()
    for part in audit["partitions"]:
        day = date.fromisoformat(part["date"])
        original, compressed_sha = sources_by_file[part["source_filename"]]
        reference = rows_by_file[original.name]
        if (compressed_sha != part["source_compressed_sha256"]
                or reference["uncompressed_sha256"] != part["source_uncompressed_sha256"]):
            raise ValueError("partition differs from fully decoded corpus")
        meta = native_metadata(original, dbn)
        if (meta.version != 1 or meta.dataset != "OPRA.PILLAR" or str(meta.schema) != "definition"
                or meta.start != identity.midnight(day) or meta.end != identity.midnight(day) + identity.DAY
                or meta.symbols != receipt["query"]["symbols"] or meta.limit is not None
                or str(meta.stype_in) != "parent" or str(meta.stype_out) != "instrument_id"):
            raise ValueError("native metadata scope differs")
        cm = reference["metadata"]
        if (array_hash(meta.partial) != cm["partial_sha256"] or len(meta.partial) != cm["partial_count"]
                or array_hash(meta.not_found) != cm["not_found_sha256"] or len(meta.not_found) != cm["not_found_count"]):
            raise ValueError("native resolution arrays differ from original census")
        mappings = identity.active_mappings(meta, day, roots)
        name = day.isoformat() + ".identities.jsonl.gz"
        if part["identity_file"] != name:
            raise ValueError("identity filename differs")
        path = args.identity_audit.parent / name
        if path.is_symlink() or not path.is_file() or path.stat().st_size != part["identity_bytes"] or source.file_hash(path) != part["identity_sha256"]:
            raise ValueError("identity file differs")
        seen, stream_hash, count, size = {}, hashlib.sha256(), 0, 0
        with gzip.open(path, "rb") as stream:
            while line := stream.readline(4097):
                size += len(line)
                if len(line) > 4096 or not line.endswith(b"\n") or size > 64 << 20:
                    raise ValueError("identity stream exceeds bounds")
                stream_hash.update(line)
                values = json.loads(line)
                if not isinstance(values, list) or len(values) != 9 or values[0] != 30:
                    raise ValueError("identity shape differs")
                raw, instrument = values[2], values[1]
                if raw in seen or mappings.get(raw) != instrument:
                    raise ValueError("identity duplicates or differs from mapping")
                seen[raw] = instrument
                count += 1
        if count != part["records"] or stream_hash.hexdigest() != part["identity_jsonl_sha256"]:
            raise ValueError("identity record proof differs")
        missing = sorted(set(mappings) - set(seen))
        buckets, root_buckets, missing_identity = Counter(), {}, []
        for raw in missing:
            ticker, expiry, kind, strike = identity.osi_identity(raw, roots)
            days = (expiry - day).days
            bucket = expiry_bucket(days)
            buckets[bucket] += 1
            root_buckets.setdefault(ticker, Counter())[bucket] += 1
            missing_identity.append([raw, mappings[raw], days])
        if len(missing) != part["active_mappings_without_definition"]:
            raise ValueError("mapping difference count changed")
        totals.update(buckets)
        results.append({"date": day.isoformat(), "mapped": len(mappings), "observed_definitions": count,
                        "mapped_without_definition": len(missing), "missing_expiry_buckets": dict(buckets),
                        "missing_root_expiry_buckets": {k: dict(v) for k, v in root_buckets.items()},
                        "missing_identities_sha256": array_hash(missing_identity),
                        "missing_DTE1_8": [x for x in missing_identity if 1 <= x[2] <= 8],
                        "resolution_arrays_match_original_census": True,
                        "all_observed_definitions_captured_by_first_decision": part["observed_root_counts"] == part["captured_by_first_decision_root_counts"]})
        print(json.dumps({"date": day.isoformat(), "missing": len(missing), "expiry_buckets": dict(buckets)}), flush=True)
    result = {"schema_version": "AutoWheelDefinitionMappingGapAuditV1", "observed_at": datetime.now(timezone.utc).isoformat(),
              "status": "observed_unqualified", "script_sha256": source.file_hash(Path(__file__)),
              "identity_script_sha256": IDENTITY_SHA, "source_verifier_sha256": identity.DEPENDENCY_SHA,
              "identity_audit_sha256": audit_sha, "census_summary_sha256": census_sha, "receipt_sha256": receipt_sha,
              "job_id": receipt["job_id"], "partitions": results, "missing_expiry_bucket_totals": dict(totals),
              "source_gate_passed": False, "replay_frames_produced": False, "historical_performance_available": False,
              "limitations": ["A date-level mapping can remain resolvable after expiry; mapping existence is not a listed/tradable contract declaration.",
                              "No mapping difference was silently discarded; expiry/DTE buckets use raw OSI dates, not inferred listing or settlement clocks.",
                              "Missing deliverable, tick, activation, weekly and quantity fields remain undefined; source gates stay pending."]}
    identity.write_json(args.output, result)
    print(json.dumps({"partitions": len(results), "missing_expiry_bucket_totals": dict(totals), "source_gate_passed": False}), flush=True)


if __name__ == "__main__":
    main()
