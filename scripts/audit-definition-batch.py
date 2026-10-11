#!/usr/bin/env python3
"""Census verified OPRA definitions; never fill missing terms or emit replay frames."""
import argparse
from collections import Counter
import hashlib
import importlib.metadata
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
from datetime import datetime, timezone


def unique_pairs(pairs):
    obj = {}
    for key, value in pairs:
        if key in obj:
            raise ValueError("duplicate JSON key")
        obj[key] = value
    return obj


def read_json(path):
    raw = path.read_bytes()
    if not raw or len(raw) > 4 << 20:
        raise ValueError("bounded evidence size invalid")
    return json.loads(raw, object_pairs_hook=unique_pairs), hashlib.sha256(raw).hexdigest()


def file_hash(path):
    h = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def verified_source(receipt_path, query_path):
    receipt, receipt_hash = read_json(receipt_path)
    query, query_hash = read_json(query_path)
    if receipt.get("all_verified") is not True or receipt.get("state") != "done" or receipt.get("query") != query:
        raise ValueError("acquisition is not verified for the declared query")
    if query.get("dataset") != "OPRA.PILLAR" or query.get("schema") != "definition" or query.get("start") != "2026-04-06T00:00:00Z" or query.get("end") != "2026-10-03T00:00:00Z":
        raise ValueError("unexpected definition study")
    base = receipt_path.parent
    job, job_hash = read_json(base / "job-source.json")
    manifest, manifest_hash = read_json(base / "files-source.json")
    if job_hash != receipt.get("job_sha256") or manifest_hash != receipt.get("file_list_sha256") or job.get("id") != receipt.get("job_id") or job.get("state") != "done":
        raise ValueError("job or file manifest binding differs")
    for key in ("dataset", "schema", "stype_in"):
        if job.get(key) != query.get(key):
            raise ValueError("job scope differs")
    if job.get("symbols") not in (query["symbols"], ",".join(query["symbols"])):
        raise ValueError("job symbols differ")
    files = receipt.get("files")
    if not isinstance(files, list) or not 1 <= len(files) <= 4096 or not isinstance(manifest, list) or len(files) != len(manifest):
        raise ValueError("file set is incomplete")
    seen = set()
    total = 0
    data = []
    for f, m in zip(files, manifest):
        name = f.get("filename")
        size = f.get("bytes")
        sha = f.get("sha256")
        if not isinstance(name, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,254}", name) or name in seen:
            raise ValueError("unsafe or duplicate file")
        if type(size) is not int or not 0 <= size <= 8 << 30 or not isinstance(sha, str) or not re.fullmatch(r"[0-9a-f]{64}", sha):
            raise ValueError("file proof invalid")
        if m.get("filename") != name or type(m.get("size")) is not int or m["size"] != size or m.get("hash") != "sha256:" + sha:
            raise ValueError("manifest file proof differs")
        path = base / name
        if path.is_symlink() or not path.is_file() or path.stat().st_size != size or file_hash(path) != sha:
            raise ValueError("file bytes differ from the provider manifest")
        seen.add(name)
        total += size
        if total > 64 << 30:
            raise ValueError("file set exceeds the acquisition bound")
        if name.endswith(".definition.dbn.zst"):
            data.append((path, sha, size))
    if not data or type(receipt.get("total_bytes")) is not int or total != receipt["total_bytes"]:
        raise ValueError("data files or total byte proof invalid")
    return receipt, receipt_hash, query_hash, job, data


def census(path, sha, compressed_bytes, dbn):
    decoder = dbn.DBNDecoder(upgrade_policy=dbn.VersionUpgradePolicy.AS_IS, compression=dbn.Compression.NONE)
    metadata = []
    counts, roots, publishers, actions, undefined, absent = (Counter() for _ in range(6))
    ids = set()
    lower, upper = None, None
    uncompressed = 0
    raw_hash = hashlib.sha256()
    sentinels = {"contract_multiplier": 2147483647, "min_price_increment": dbn.UNDEF_PRICE, "activation": dbn.UNDEF_TIMESTAMP, "maturity_week": 255, "unit_of_measure_qty": dbn.UNDEF_PRICE}
    with tempfile.TemporaryFile() as error_stream:
        proc = subprocess.Popen(["/usr/bin/zstd", "-q", "-d", "-c", "--", str(path)], stdout=subprocess.PIPE, stderr=error_stream)
        try:
            for chunk in iter(lambda: proc.stdout.read(128 << 10), b""):
                uncompressed += len(chunk)
                if uncompressed > 8 << 30:
                    raise ValueError("definition shard expands beyond the audit bound")
                raw_hash.update(chunk)
                for row in decoder.write_and_decode(chunk):
                    if isinstance(row, dbn.Metadata):
                        metadata.append({"version": row.version, "dataset": row.dataset, "schema": str(row.schema), "start": row.start, "end": row.end, "limit": row.limit, "symbols": row.symbols, "partial": row.partial, "not_found": row.not_found, "stype_in": str(row.stype_in), "stype_out": str(row.stype_out)})
                        continue
                    name = type(row).__name__
                    counts[name] += 1
                    if name not in ("InstrumentDefMsg", "InstrumentDefMsgV1", "InstrumentDefMsgV2"):
                        continue
                    ids.add(row.instrument_id)
                    symbol = row.raw_symbol
                    root = symbol[:6].rstrip(" ") if len(symbol) == 21 and re.fullmatch(r"[A-Z0-9 ]{6}[0-9]{6}[PC][0-9]{8}", symbol) else "<non_OSI>"
                    roots[root] += 1
                    publishers[str(row.publisher_id)] += 1
                    actions[str(row.security_update_action)] += 1
                    recv = row.ts_recv
                    if recv == dbn.UNDEF_TIMESTAMP:
                        undefined["ts_recv"] += 1
                    else:
                        lower = recv if lower is None else min(lower, recv)
                        upper = recv if upper is None else max(upper, recv)
                    for field, sentinel in sentinels.items():
                        if not hasattr(row, field):
                            absent[field] += 1
                        elif getattr(row, field) == sentinel:
                            undefined[field] += 1

                if len(decoder.buffer()) > 16 << 20:
                    raise ValueError("DBN undecoded buffer exceeds the audit bound")
            proc.stdout.close()
            if proc.wait() != 0 or decoder.buffer():
                raise ValueError("compressed or DBN source is truncated or invalid")
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()
            proc.stdout.close()
    if len(metadata) != 1 or metadata[0]["dataset"] != "OPRA.PILLAR" or metadata[0]["schema"] != "definition" or metadata[0]["version"] not in (1, 2, 3):
        raise ValueError("DBN metadata scope or version invalid")
    return {"filename": path.name, "compressed_sha256": sha, "compressed_bytes": compressed_bytes, "uncompressed_sha256": raw_hash.hexdigest(), "uncompressed_bytes": uncompressed, "metadata": metadata[0], "record_types": dict(counts), "distinct_instrument_ids": len(ids), "raw_OSI_root_counts": dict(roots), "publishers": dict(publishers), "update_actions": dict(actions), "undefined_field_counts": dict(undefined), "absent_field_counts": dict(absent), "min_ts_recv": lower, "max_ts_recv": upper}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--receipt", type=Path, required=True)
    parser.add_argument("--query", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.output.exists():
        raise ValueError("audit output already exists")
    if importlib.metadata.version("databento-dbn") != "0.71.0":
        raise ValueError("pinned research decoder is required")
    import databento_dbn as dbn
    receipt, receipt_hash, query_hash, job, files = verified_source(args.receipt, args.query)
    results = [census(path, sha, size, dbn) for path, sha, size in files]
    records = sum(sum(row["record_types"].values()) for row in results)
    matched_job_count = type(job.get("record_count")) is int and job["record_count"] == records
    audit = {"schema_version": "AutoWheelDefinitionBatchCensusV1", "observed_at": datetime.now(timezone.utc).isoformat(), "status": "observed_unqualified", "script_sha256": file_hash(Path(__file__)), "decoder_version": "0.71.0", "upgrade_policy": "AS_IS", "receipt_sha256": receipt_hash, "query_file_sha256": query_hash, "job_id": receipt["job_id"], "operation_id": receipt["operation_id"], "query_hash": receipt["query_hash"], "file_count": len(receipt["files"]), "definition_shards": len(results), "records": records, "provider_record_count": job.get("record_count"), "provider_record_count_matches": matched_job_count, "files": results, "replay_frames_produced": False, "source_gate_passed": False, "historical_performance_available": False, "limitations": ["Capture witnesses and raw OSI roots do not prove listing time, tradability, standard deliverables, tick rules, or weekly program eligibility.", "Undefined source fields remain undefined; no defaults or source clock changes.", "A complete definition acquisition does not qualify quote, OI, Greeks, corporate-action, lifecycle, or replay coverage."]}
    if not matched_job_count:
        audit["limitations"].append("Provider record count differs or is not available; investigate before any full-corpus completeness claim.")
    raw = (json.dumps(audit, indent=2) + "\n").encode()
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as output:
        output.write(raw)
        output.flush()
        os.fsync(output.fileno())
    parent = os.open(args.output.parent, os.O_RDONLY)
    try:
        os.fsync(parent)
    finally:
        os.close(parent)
    print(json.dumps({"job_id": receipt["job_id"], "definition_shards": len(results), "records": records, "provider_record_count_matches": matched_job_count, "source_gate_passed": False}))


if __name__ == "__main__":
    main()
