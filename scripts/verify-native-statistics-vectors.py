#!/usr/bin/env python3
"""Compare every delivered native statistics row with its private vector.

This independent read pass also checks the actual pre-decision OI copies
against dated definition identities. It reports observations, never a full
source gate, missing OI defaults, replay frames, or historical performance.
"""
import argparse
from collections import Counter
from datetime import date, datetime, timezone
import gzip
import hashlib
import importlib.metadata
import json
import os
from pathlib import Path
import stat
import subprocess

from batch_acquisition_proof import file_hash, read_json, utc_ns


def safe_file(path):
    info = path.lstat()
    if (not path.is_absolute() or path.resolve() != path or not stat.S_ISREG(info.st_mode)
            or info.st_uid != os.getuid() or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600):
        raise ValueError("private regular source required")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-directory", type=Path, required=True)
    parser.add_argument("--audit-directory", type=Path, required=True)
    parser.add_argument("--identity-directory", type=Path, required=True)
    parser.add_argument("--cutoff", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if importlib.metadata.version("databento-dbn") != "0.71.0" or args.output.exists():
        raise ValueError("pinned decoder and new output required")
    import databento_dbn as dbn
    if dict(dbn.StatMsgV1._dtypes).get("quantity") != "i4" or dbn.StatMsgV1.size_hint != 64:
        raise ValueError("native V1 layout differs")
    audit_path, receipt_path = args.audit_directory / "audit.json", args.source_directory / "receipt.json"
    safe_file(audit_path)
    safe_file(receipt_path)
    audit, receipt = read_json(audit_path), read_json(receipt_path)
    query = receipt["query"]
    if (audit.get("schema_version") != "AutoWheelNativeStatisticsSourceAuditV1" or audit.get("source_gate_passed") is not False
            or audit.get("decoder_version") != "0.71.0" or audit.get("upgrade_policy") != "AS_IS"
            or query.get("dataset") != "OPRA.PILLAR" or query.get("schema") != "statistics"
            or query.get("stype_in") != "raw_symbol" or len(audit["files"]) != 1
            or audit["receipt_sha256"] != file_hash(receipt_path) or receipt.get("all_verified") is not True):
        raise ValueError("statistics audit binding differs")
    start, end, cutoff = utc_ns(query["start"]), utc_ns(query["end"]), utc_ns(args.cutoff)
    if end - start != 86_400_000_000_000 or start % 86_400_000_000_000 or not start <= cutoff < end:
        raise ValueError("one UTC day and causal cutoff required")
    day = datetime.fromtimestamp(start // 1_000_000_000, timezone.utc).date()
    member = audit["files"][0]
    if any(Path(member[k]).name != member[k] for k in ["source_filename", "vector_file"]):
        raise ValueError("invalid member name")
    native_path, vector_path = args.source_directory / member["source_filename"], args.audit_directory / member["vector_file"]
    safe_file(native_path)
    safe_file(vector_path)
    if (file_hash(native_path) != member["source_compressed_sha256"] or file_hash(vector_path) != member["vector_sha256"]
            or native_path.stat().st_size != member["source_compressed_bytes"] or vector_path.stat().st_size != member["vector_bytes"]):
        raise ValueError("source/vector binding differs")
    before = {p: file_hash(p) for p in [audit_path, receipt_path, native_path, vector_path]}
    decoder = dbn.DBNDecoder(upgrade_policy=dbn.VersionUpgradePolicy.AS_IS, compression=dbn.Compression.NONE)
    metadata, records, expanded, vector_bytes = None, 0, 0, 0
    reverse, oi = {}, {}
    earliest_oi, sentinel_count, before_first = None, 0, 0
    vector_hash, decoded_hash = hashlib.sha256(), hashlib.sha256()
    fd = os.open(native_path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as native:
        compressed_hash = hashlib.sha256()
        for raw in iter(lambda: native.read(1 << 20), b""):
            compressed_hash.update(raw)
        if compressed_hash.hexdigest() != member["source_compressed_sha256"]:
            raise ValueError("native descriptor hash differs")
        native.seek(0)
        process = subprocess.Popen(["/usr/bin/zstd", "-q", "-f", "-d", "-c", "--", "/dev/fd/" + str(native.fileno())],
                                   pass_fds=(native.fileno(),), stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        try:
            with gzip.open(vector_path, "rb") as vectors:
                for raw in iter(lambda: process.stdout.read(128 << 10), b""):
                    expanded += len(raw)
                    if expanded > 8 << 30:
                        raise ValueError("expansion bound")
                    decoded_hash.update(raw)
                    for row in decoder.write_and_decode(raw):
                        if isinstance(row, dbn.Metadata):
                            if metadata is not None or records != 0 or row.version not in (1, 2):
                                raise ValueError("metadata/version")
                            if (row.dataset != "OPRA.PILLAR" or str(row.schema) != "statistics" or row.start != start or row.end != end
                                    or list(row.symbols) != query["symbols"] or str(row.stype_in) != "raw_symbol"
                                    or str(row.stype_out) != "instrument_id" or row.limit not in (None, 0)):
                                raise ValueError("metadata scope")
                            metadata = row
                            for symbol, intervals in row.mappings.items():
                                if symbol not in query["symbols"]:
                                    raise ValueError("unqueried mapping")
                                for interval in intervals:
                                    if (type(interval["start_date"]) is not date or type(interval["end_date"]) is not date
                                            or not interval["start_date"] < interval["end_date"]):
                                        raise ValueError("mapping dates")
                                    if interval["start_date"] <= day < interval["end_date"]:
                                        instrument = int(interval["symbol"])
                                        if instrument in reverse or symbol in reverse.values() or not 0 < instrument < 1 << 32:
                                            raise ValueError("ambiguous mapping")
                                        reverse[instrument] = symbol
                            continue
                        if metadata is None or type(row).__name__ != "StatMsgV1" or row.instrument_id not in reverse:
                            raise ValueError("native record identity")
                        line = vectors.readline(4097)
                        vector_bytes += len(line)
                        if not line.endswith(b"\n") or len(line) > 4096 or vector_bytes > 4 << 30:
                            raise ValueError("vector record bound")
                        vector_hash.update(line)
                        expected = [records, metadata.version, reverse[row.instrument_id], row.publisher_id, row.instrument_id,
                                    row.ts_recv, row.ts_event, row.ts_ref, row.price, row.quantity, row.sequence,
                                    row.ts_in_delta, int(row.stat_type), row.channel_id, int(row.update_action), row.stat_flags, int(row.rtype)]
                        actual = json.loads(line)
                        if actual != expected or len(actual) != 17 or any(type(value) is not int for i, value in enumerate(actual) if i != 2):
                            raise ValueError("native/vector field mismatch")
                        sentinel_count += row.quantity == 2147483647
                        if int(row.stat_type) == 9:
                            if (not start <= row.ts_recv < end or not 0 <= row.quantity < 2147483647 or row.ts_event > row.ts_recv
                                    or int(row.update_action) != 1 or row.stat_flags != 0):
                                raise ValueError("OI reference unsupported")
                            earliest_oi = row.ts_recv if earliest_oi is None else min(earliest_oi, row.ts_recv)
                            if row.ts_recv <= cutoff:
                                oi.setdefault(reverse[row.instrument_id], {})[row.publisher_id] = (
                                    records, row.instrument_id, row.quantity, row.ts_recv, row.ts_event)
                        records += 1
                        if records > member["records"]:
                            raise ValueError("record count bound")
                    if len(decoder.buffer()) > 16 << 20:
                        raise ValueError("decoder residual")
                if process.wait() != 0 or decoder.buffer() or vectors.read(1) != b"" or metadata is None:
                    raise ValueError("native/vector EOF")
        finally:
            if process.poll() is None:
                process.kill()
                process.wait()
            process.stdout.close()
    if (records != member["records"] or records != audit["provider_record_count"] or records != audit["records"]
            or vector_hash.hexdigest() != member["vector_jsonl_sha256"] or decoded_hash.hexdigest() != member["source_uncompressed_sha256"]
            or expanded != member["source_uncompressed_bytes"] or sentinel_count != member["undefined_sentinel_counts"].get("quantity", 0)):
        raise ValueError("native audit totals differ")
    identity_audit_path = args.identity_directory / "audit.json"
    identities = read_json(identity_audit_path)
    parts = [p for p in identities["partitions"] if p["date"] == day.isoformat()]
    if len(parts) != 1 or Path(parts[0]["identity_file"]).name != parts[0]["identity_file"]:
        raise ValueError("definition day differs")
    part = parts[0]
    identity_path = args.identity_directory / part["identity_file"]
    safe_file(identity_path)
    if file_hash(identity_path) != part["identity_sha256"]:
        raise ValueError("definition source differs")
    before[identity_audit_path], before[identity_path] = file_hash(identity_audit_path), file_hash(identity_path)
    definitions, definition_hash, definition_count = {}, hashlib.sha256(), 0
    with gzip.open(identity_path, "rb") as source:
        while line := source.readline(4097):
            if len(line) > 4096 or not line.endswith(b"\n"):
                raise ValueError("definition vector bound")
            definition_hash.update(line)
            row = json.loads(line)
            if len(row) != 9 or row[2] in definitions:
                raise ValueError("definition shape or duplicate")
            if row[3] <= cutoff:
                definitions[row[2]] = row[1]
            definition_count += 1
            if definition_count > 1_000_000:
                raise ValueError("definition count bound")
    if definition_count != part["records"] or definition_hash.hexdigest() != part["identity_jsonl_sha256"]:
        raise ValueError("definition count/hash differs")
    copies = Counter()
    for symbol, publishers in oi.items():
        instruments, quantities = {event[1] for event in publishers.values()}, {event[2] for event in publishers.values()}
        if len(instruments) != 1 or len(quantities) != 1 or definitions.get(symbol) != next(iter(instruments)):
            raise ValueError("OI copy or definition mismatch")
        copies[len(publishers)] += 1
    if earliest_oi is not None:
        with gzip.open(vector_path, "rb") as source:
            while line := source.readline(4097):
                if len(line) > 4096 or not line.endswith(b"\n"):
                    raise ValueError("vector bound")
                row = json.loads(line)
                before_first += row[12] == 9 and row[5] <= earliest_oi - 1
    if before_first != 0 or any(file_hash(path) != digest for path, digest in before.items()):
        raise ValueError("source changed or prepublication OI invented")
    result = {"schema_version": "AutoWheelNativeStatisticsVectorReferenceV1", "observed_at": datetime.now(timezone.utc).isoformat(),
              "reference_script_sha256": file_hash(Path(__file__)), "audit_sha256": file_hash(audit_path), "receipt_sha256": file_hash(receipt_path),
              "records_compared_field_for_field": records, "vector_fields_per_record": 17, "vector_jsonl_sha256": vector_hash.hexdigest(),
              "cutoff": args.cutoff, "OI_symbols_at_cutoff": len(oi), "queried_symbols": len(query["symbols"]),
              "queried_symbols_without_OI_at_cutoff": len(set(query["symbols"]) - set(oi)), "first_OI_capture_ns": earliest_oi,
              "OI_rows_one_ns_before_first_capture": before_first, "publisher_copies_distribution": dict(copies),
              "publisher_quantity_disagreements": 0, "definition_ID_join_mismatches": 0, "definition_identity_sha256": part["identity_sha256"],
              "native_int32_quantity_sentinels_preserved": sentinel_count, "retimed_rows": 0, "source_gate_passed": False,
              "full_study_coverage_proved": False, "historical_performance_available": False}
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as output:
        output.write((json.dumps(result, indent=2, sort_keys=True) + "\n").encode())
        output.flush()
        os.fsync(output.fileno())
    print(json.dumps(result))


if __name__ == "__main__":
    try:
        main()
    except Exception:
        print("native statistics vector reference failed", file=__import__("sys").stderr)
        raise SystemExit(1)
