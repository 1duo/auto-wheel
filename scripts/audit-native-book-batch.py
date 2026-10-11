#!/usr/bin/env python3
"""Offline, lossless book vectors from an explicitly acquired research batch.

OPRA CMBP-1 and EQUS.MINI MBP-1 are separate source types. This adapter
preserves file order, native integers, sentinels and venue fields. It never
produces replay frames, fixes clock/order anomalies, or qualifies a source.
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
import re
import shutil
import stat
import subprocess
import sys

from batch_acquisition_proof import (CODE_FILES, canonical_query, file_hash,
                                     read_json, utc_ns, validate_batch_acquisition,
                                     native_verifier_path, book_expansion_limit)

DECODER_VERSION = "0.71.0"
COMMON_FIELDS = ["row_index", "native_version", "raw_symbol", "publisher_id",
                 "instrument_id", "ts_recv", "ts_event", "ts_out", "price",
                 "size", "action", "side", "flags", "ts_in_delta",
                 "bid_px", "ask_px", "bid_sz", "ask_sz", "rtype"]
TAIL_FIELDS = {"cmbp-1": ["bid_pb", "ask_pb"],
               "mbp-1": ["bid_ct", "ask_ct", "depth", "sequence"]}


def private_file(path):
    if not path.is_absolute() or path.resolve() != path:
        raise ValueError("unsafe evidence path")
    info = path.lstat()
    if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
            or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600):
        raise ValueError("private evidence required")


def private_directory(path):
    info = path.lstat()
    if (not path.is_absolute() or path.resolve() != path or
            not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid()
            or stat.S_IMODE(info.st_mode) != 0o700):
        raise ValueError("private directory required")


def sync_directory(path):
    fd = os.open(path, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def write_json(path, value):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as output:
        output.write((json.dumps(value, indent=2, allow_nan=False) + "\n").encode())
        output.flush()
        os.fsync(output.fileno())
    sync_directory(path.parent)


def acquired_source(proof_path, ledger_path, root):
    """Use the completion checker's exact acquisition binding, without gates."""
    private_file(proof_path)
    private_file(ledger_path)
    proof, ledger = read_json(proof_path), read_json(ledger_path)
    entries = [e for e in ledger["reservations"]
               if e["operation_id"] == proof["reservation"]["operation_id"]]
    if (len(entries) != 1 or ledger.get("version") != 2 or
            ledger.get("authorization_mode") != "uncapped" or ledger.get("lifetime_nano") != 0):
        raise ValueError("uncapped acquired journal required")
    entry = entries[0]
    expected = {str(proof_path): entry.get("data_sha256")}
    for path_key, hash_key in [("query_path", "query_file_sha256"),
                               ("acknowledgement_path", "acknowledgement_file_sha256"),
                               ("ack_body_path", "ack_body_sha256")]:
        expected[proof[path_key]] = proof[hash_key]
    directory = Path(proof["source_directory"])
    private_directory(directory)
    for name, hash_key in [("receipt.json", "receipt_sha256"),
                           ("job-source.json", "job_sha256"),
                           ("files-source.json", "file_list_sha256")]:
        expected[str(directory / name)] = proof[hash_key]
    for member in proof["files"]:
        if not isinstance(member.get("filename"), str) or re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,254}", member["filename"]) is None:
            raise ValueError("invalid member name")
        path = str(directory / member["filename"])
        if path in expected:
            raise ValueError("duplicate member")
        expected[path] = member["sha256"]
    manifest = {}
    for name, digest in expected.items():
        path = Path(name)
        private_file(path)
        if file_hash(path) != digest:
            raise ValueError("source digest differs")
        manifest[name] = {"sha256": digest, "bytes": path.stat().st_size}
    verifier = native_verifier_path(proof)
    for name in list(dict.fromkeys(CODE_FILES + [verifier])):
        path = root / name
        digest = file_hash(path)
        if name == verifier and digest != proof["native_verifier_sha256"]:
            raise ValueError("native acquisition verifier differs")
        manifest[name] = {"sha256": digest, "bytes": path.stat().st_size}
    ok, _ = validate_batch_acquisition(entry, str(proof_path), str(directory / "receipt.json"),
                                      manifest, set(manifest), root)
    if not ok:
        raise ValueError("acquisition proof is not valid")
    query = proof["query"]
    canonical_query(query)
    pair = query["dataset"], query["schema"]
    if pair not in {("OPRA.PILLAR", "cmbp-1"), ("EQUS.MINI", "mbp-1")} or query["stype_in"] != "raw_symbol":
        raise ValueError("unsupported book scope")
    start, end = utc_ns(query["start"]), utc_ns(query["end"])
    if end - start != 86_400_000_000_000 or start % 86_400_000_000_000 != 0:
        raise ValueError("exact UTC day required")
    return proof, manifest, start, end


def active_mappings(metadata, day, queried):
    if not isinstance(metadata.mappings, dict):
        raise ValueError("mapping shape")
    active, reverse = {}, {}
    for raw, intervals in metadata.mappings.items():
        if raw not in queried or not isinstance(intervals, list):
            raise ValueError("unqueried mapping")
        for interval in intervals:
            if not isinstance(interval, dict):
                raise ValueError("mapping interval shape")
            start, end, instrument = interval.get("start_date"), interval.get("end_date"), interval.get("symbol")
            if type(start) is not date or type(end) is not date or not start < end:
                raise ValueError("mapping interval dates")
            if not isinstance(instrument, str) or re.fullmatch(r"[1-9][0-9]{0,9}", instrument) is None or int(instrument) >= 1 << 32:
                raise ValueError("mapping instrument")
            if start <= day < end:
                instrument = int(instrument)
                if raw in active or instrument in reverse:
                    raise ValueError("ambiguous daily mapping")
                active[raw], reverse[instrument] = instrument, raw
    return active, reverse


def integer(value, bits, signed=False):
    if isinstance(value, bool) or not isinstance(value, int):
        raise ValueError("noninteger native field")
    low = -(1 << (bits - 1)) if signed else 0
    high = 1 << (bits - 1) if signed else 1 << bits
    if not low <= value < high:
        raise ValueError("native integer width")
    return value


def metadata_symbols_match(actual, requested):
    """Native metadata identifies a roster; request order remains journal-bound."""
    return (isinstance(actual, list) and isinstance(requested, list)
            and all(isinstance(value, str) for value in actual + requested)
            and len(actual) == len(requested) == len(set(actual))
            and set(actual) == set(requested))


def vector(row, index, version, reverse, schema, ts_out):
    instrument = integer(row.instrument_id, 32)
    if instrument not in reverse or len(row.levels) != 1:
        raise ValueError("unmapped instrument or level count")
    # Pinned native getters return Action/Side enums, whose string form is
    # the original one-byte wire code; they are not Python strings.
    action, side = str(row.action), str(row.side)
    if len(action) != 1 or len(side) != 1 or not action.isascii() or not side.isascii():
        raise ValueError("native action/side width")
    values = [index, version, reverse[instrument], integer(row.publisher_id, 16),
              instrument, integer(row.ts_recv, 64), integer(row.ts_event, 64),
              integer(row.ts_out, 64) if ts_out else None, integer(row.price, 64, True),
              integer(row.size, 32), action, side, integer(row.flags, 8),
              integer(row.ts_in_delta, 32, True), integer(row.bid_px_00, 64, True),
              integer(row.ask_px_00, 64, True), integer(row.bid_sz_00, 32), integer(row.ask_sz_00, 32), int(row.rtype)]
    if schema == "cmbp-1":
        if int(row.rtype) != 177:
            raise ValueError("native CMBP-1 rtype")
        values.extend([integer(row.bid_pb_00, 16), integer(row.ask_pb_00, 16)])
    else:
        if int(row.rtype) != 1:
            raise ValueError("native MBP-1 rtype")
        values.extend([integer(row.bid_ct_00, 32), integer(row.ask_ct_00, 32),
                       integer(row.depth, 8), integer(row.sequence, 32)])
    return values


def audit_shard(directory, member, query, start, end, output, dbn):
    path = directory / member["filename"]
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    target = output / (path.name[:-8] + ".vectors.jsonl.gz")
    records, expanded, last, regressions, leads = 0, 0, None, 0, 0
    metadata, active, reverse = None, None, None
    actions, sides, flags, publishers, missing = Counter(), Counter(), Counter(), Counter(), Counter()
    observed = set()
    decoded_hash, vector_hash = hashlib.sha256(), hashlib.sha256()
    with os.fdopen(fd, "rb") as source:
        info = os.fstat(source.fileno())
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1
                or stat.S_IMODE(info.st_mode) != 0o600 or info.st_size != member["compressed_bytes"]):
            raise ValueError("source descriptor differs")
        compressed_hash = hashlib.sha256()
        for raw in iter(lambda: source.read(1 << 20), b""):
            compressed_hash.update(raw)
        if compressed_hash.hexdigest() != member["compressed_sha256"]:
            raise ValueError("compressed source differs")
        source.seek(0)
        decoder = dbn.DBNDecoder(upgrade_policy=dbn.VersionUpgradePolicy.AS_IS, compression=dbn.Compression.NONE)
        outfd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        try:
            proc = subprocess.Popen(["/usr/bin/zstd", "-q", "-f", "-d", "-c", "--", "/dev/fd/" + str(source.fileno())],
                                    pass_fds=(source.fileno(),), stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        except Exception:
            os.close(outfd)
            raise
        try:
            with os.fdopen(outfd, "wb") as stored:
                with gzip.GzipFile(filename="", mode="wb", fileobj=stored, mtime=0, compresslevel=6) as encoded:
                    for raw in iter(lambda: proc.stdout.read(128 << 10), b""):
                        expanded += len(raw)
                        if expanded > book_expansion_limit(member["records"]):
                            raise ValueError("native expansion bound")
                        decoded_hash.update(raw)
                        for row in decoder.write_and_decode(raw):
                            if isinstance(row, dbn.Metadata):
                                if metadata is not None or records != 0 or row.version != member["native_version"]:
                                    raise ValueError("native metadata/version")
                                if (row.dataset != query["dataset"] or str(row.schema) != query["schema"]
                                        or row.start != start or row.end != end or row.limit not in (None, 0)
                                        or not metadata_symbols_match(list(row.symbols), query["symbols"]) or str(row.stype_in) != "raw_symbol"
                                        or str(row.stype_out) != "instrument_id" or type(row.ts_out) is not bool):
                                    raise ValueError("native metadata scope")
                                metadata = row
                                day = datetime.fromtimestamp(start // 1_000_000_000, timezone.utc).date()
                                active, reverse = active_mappings(row, day, set(query["symbols"]))
                                continue
                            expected_class = "CMBP1Msg" if query["schema"] == "cmbp-1" else "MBP1Msg"
                            if metadata is None or type(row).__name__ != expected_class:
                                raise ValueError("native record class")
                            values = vector(row, records, metadata.version, reverse, query["schema"], metadata.ts_out)
                            recv, event = values[5:7]
                            if not start <= recv < end:
                                raise ValueError("capture outside query")
                            regressions += last is not None and recv < last
                            leads += event != int(dbn.UNDEF_TIMESTAMP) and event > recv
                            last = recv
                            observed.add(values[2])
                            actions[values[10]] += 1
                            sides[values[11]] += 1
                            flags[str(values[12])] += 1
                            publishers[str(values[3])] += 1
                            for name, value, sentinel in [("ts_event", event, int(dbn.UNDEF_TIMESTAMP)),
                                                          ("price", values[8], int(dbn.UNDEF_PRICE)),
                                                          ("bid_px", values[14], int(dbn.UNDEF_PRICE)),
                                                          ("ask_px", values[15], int(dbn.UNDEF_PRICE))]:
                                missing[name] += value == sentinel
                            line = (json.dumps(values, separators=(",", ":"), ensure_ascii=True) + "\n").encode()
                            encoded.write(line)
                            vector_hash.update(line)
                            records += 1
                            if records > member["records"] or stored.tell() > 8 << 30:
                                raise ValueError("vector count/size bound")
                        if len(decoder.buffer()) > 16 << 20:
                            raise ValueError("decoder residual bound")
                    if proc.wait() != 0 or decoder.buffer() or metadata is None:
                        raise ValueError("native/compressed EOF")
                stored.flush()
                if stored.tell() > 8 << 30:
                    raise ValueError("final vector size bound")
                os.fsync(stored.fileno())
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()
            proc.stdout.close()
    if records != member["records"] or expanded != member["uncompressed_bytes"] or decoded_hash.hexdigest() != member["uncompressed_sha256"]:
        raise ValueError("native acquisition count/hash differs")
    sync_directory(output)
    return {"source_filename": path.name, "source_compressed_sha256": member["compressed_sha256"],
            "source_uncompressed_sha256": decoded_hash.hexdigest(), "source_uncompressed_bytes": expanded,
            "records": records, "native_version": metadata.version, "ts_out_present": metadata.ts_out,
            "vector_file": target.name, "vector_sha256": file_hash(target), "vector_bytes": target.stat().st_size,
            "vector_jsonl_sha256": vector_hash.hexdigest(), "capture_order_regressions": regressions,
            "clock_leads": leads, "counts_by_action": dict(actions), "counts_by_side": dict(sides),
            "counts_by_flags": dict(flags), "counts_by_publisher": dict(publishers),
            "undefined_sentinel_counts": dict(missing), "active_mapping_count": len(active),
            "active_mappings_without_record": len(set(active) - observed),
            "partial_count": len(metadata.partial), "not_found_count": len(metadata.not_found),
            "partial_sha256": hashlib.sha256(json.dumps(list(metadata.partial), separators=(",", ":")).encode()).hexdigest(),
            "not_found_sha256": hashlib.sha256(json.dumps(list(metadata.not_found), separators=(",", ":")).encode()).hexdigest(),
            "all_eof_verified": True}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--proof", type=Path, required=True)
    parser.add_argument("--ledger", type=Path, required=True)
    parser.add_argument("--output-directory", type=Path, required=True)
    args = parser.parse_args()
    root = Path(__file__).resolve().parent.parent
    if importlib.metadata.version("databento-dbn") != DECODER_VERSION:
        raise ValueError("pinned decoder required")
    script_sha = file_hash(Path(__file__))
    proof, manifest, start, end = acquired_source(args.proof, args.ledger, root)
    manifest[str(Path(__file__).resolve())] = {"sha256": script_sha, "bytes": Path(__file__).stat().st_size}
    private_directory(args.output_directory.parent)
    if args.output_directory.exists() or shutil.disk_usage(args.output_directory.parent).free < 10 << 30:
        raise ValueError("new output and disk headroom required")
    os.mkdir(args.output_directory, 0o700)
    import databento_dbn as dbn
    rows = []
    for member in proof["native"]["files"]:
        rows.append(audit_shard(Path(proof["source_directory"]), member, proof["query"], start, end, args.output_directory, dbn))
    for name, member in manifest.items():
        path = Path(name) if Path(name).is_absolute() else root / name
        if file_hash(path) != member["sha256"] or path.stat().st_size != member["bytes"]:
            raise ValueError("source changed during audit")
    query = proof["query"]
    report = {"schema_version": "AutoWheelNativeBookSourceAuditV1", "status": "observed_unqualified",
              "observed_at": datetime.now(timezone.utc).isoformat(), "script_sha256": script_sha,
              "decoder_version": DECODER_VERSION, "upgrade_policy": "AS_IS",
              "acquisition_proof_sha256": file_hash(args.proof), "query_hash": proof["reservation"]["query_hash"],
              "operation_id": proof["reservation"]["operation_id"], "job_id": proof["job_id"],
              "query_dataset": query["dataset"], "query_schema": query["schema"], "query_start": query["start"],
              "query_end": query["end"], "query_symbol_count": len(query["symbols"]),
              "vector_fields": COMMON_FIELDS + TAIL_FIELDS[query["schema"]], "files": rows,
              "records": sum(row["records"] for row in rows), "provider_record_count": proof["provider_record_count"],
              "source_gate_passed": False, "replay_frames_produced": False, "historical_performance_available": False,
              "limitations": ["Source order, clocks, sentinels, native action/side/flags and venue fields are preserved; anomalies are counted without correction.",
                              "Daily ID mappings and partial/not-found lists do not prove first listing or tradability.",
                              "OPRA CMBP-1 and the derived EQUS.MINI book retain distinct dataset provenance.",
                              "No fill, continuous execution, terms, Greeks, lifecycle, or performance qualification is asserted."]}
    if report["records"] != report["provider_record_count"]:
        raise ValueError("provider count differs")
    write_json(args.output_directory / "audit.json", report)
    print(json.dumps({"records": report["records"], "source_gate_passed": False}))


if __name__ == "__main__":
    try:
        main()
    except Exception:
        print("native book source audit failed", file=sys.stderr)
        raise SystemExit(1)
