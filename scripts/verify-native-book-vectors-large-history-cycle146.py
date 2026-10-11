#!/usr/bin/env python3
"""Independent complete native/vector comparison; no replay qualification."""
import argparse
from datetime import date, datetime, timezone
import gzip
import hashlib
import importlib.metadata
import json
import os
from pathlib import Path
import stat
import subprocess
import sys

from batch_acquisition_proof import book_expansion_limit, file_hash, read_json, utc_ns


def private_file(path):
    info = path.lstat()
    if (not path.is_absolute() or path.resolve() != path or not stat.S_ISREG(info.st_mode)
            or info.st_uid != os.getuid() or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600):
        raise ValueError("private regular input required")



def large_binding(audit, receipt, book, history, audited_history, plan):
    query = book.proof["query"]
    if (audit.get("schema_version") != "AutoWheelLargeHistoryNativeBookAuditV1"
            or audit.get("script_sha256") != "6f8e3f6fe85a5c77c781f5b28182d4cb158229fa96778bb557c01af3e5fead08"
            or audit.get("original_adapter_sha256") != "3fa7f96d870ed202079169d6041bd5e57d0447eeaa488c1d5c9970c855335491"
            or audit.get("decoder_version") != "0.71.0" or audit.get("upgrade_policy") != "AS_IS"
            or audit.get("source_gate_passed") is not False
            or audit.get("historical_performance_available") is not False
            or len(audit.get("files", [])) != 1
            or audit.get("operation_id") != book.entry["operation_id"]
            or audit.get("acquisition_proof_sha256") != book.entry["data_sha256"]
            or audit.get("financial_snapshot_sha256") != audited_history.snapshot.sha256
            or audit.get("financial_snapshot_entries") != len(audited_history.by_operation)
            or any(history.by_operation.get(op) != row for op, row in audited_history.by_operation.items())
            or audit.get("decision_plan_sha256") != plan.sha256
            or audit.get("query_scope") != book.scope or book.scope != "complete_UTC_day_acquisition"
            or audit.get("source_start_ns") != book.start_ns or audit.get("source_end_ns") != book.end_ns
            or audit.get("closed_cut_ns") is not None or book.cut_ns is not None
            or audit.get("records") != book.proof["provider_record_count"]
            or audit.get("provider_record_count") != book.proof["provider_record_count"]
            or receipt.get("query") != query or receipt.get("all_verified") is not True
            or receipt.get("operation_id") != book.entry["operation_id"]
            or receipt.get("query_hash") != book.entry["query_hash"]
            or receipt.get("job_id") != book.proof["job_id"]):
        raise ValueError("large-history acquisition/audit binding differs")
    native = book.proof["native"]["files"]
    member = audit["files"][0]
    if (len(native) != 1 or member["source_filename"] != native[0]["filename"]
            or member["records"] != native[0]["records"]
            or member["source_compressed_sha256"] != native[0]["compressed_sha256"]
            or member["source_uncompressed_sha256"] != native[0]["uncompressed_sha256"]
            or member["source_uncompressed_bytes"] != native[0]["uncompressed_bytes"]
            or member["all_eof_verified"] is not True
            or any(member[key] != 0 for key in ("capture_order_regressions", "clock_leads", "partial_count", "not_found_count"))):
        raise ValueError("large-history native member differs")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-directory", type=Path, required=True)
    parser.add_argument("--audit-directory", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    for name in ("proof", "ledger-snapshot", "decision-plan"):
        parser.add_argument("--" + name, type=Path, required=True)
    args = parser.parse_args()
    if importlib.metadata.version("databento-dbn") != "0.71.0" or args.output.exists():
        raise ValueError("pinned decoder and new output required")
    parent = args.output.parent
    info = parent.lstat()
    if (not args.output.is_absolute() or parent.resolve() != parent or not stat.S_ISDIR(info.st_mode)
            or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700):
        raise ValueError("private output directory required")
    import databento_dbn as dbn
    audit_path, receipt_path = args.audit_directory / "audit.json", args.source_directory / "receipt.json"
    for path in (audit_path, receipt_path, Path(__file__).resolve()):
        private_file(path)
    audit, receipt = read_json(audit_path), read_json(receipt_path)
    query = receipt["query"]
    pair = query["dataset"], query["schema"]
    from research_batch_history import BatchHistory, DecisionPlan, acquired_book
    root = Path(__file__).resolve().parent.parent
    for path in (args.proof, args.ledger_snapshot, args.decision_plan):
        private_file(path)
    history = BatchHistory(args.ledger_snapshot)
    plan = DecisionPlan(args.decision_plan)
    book = acquired_book(args.proof, history, root, plan=plan)
    if Path(book.proof["source_directory"]) != args.source_directory:
        raise ValueError("original source directory differs")
    audited_path = Path(audit["financial_snapshot_path"])
    private_file(audited_path)
    audited_history = BatchHistory(audited_path)
    large_binding(audit, receipt, book, history, audited_history, plan)
    if pair not in {("OPRA.PILLAR", "cmbp-1"), ("EQUS.MINI", "mbp-1")} or query["stype_in"] != "raw_symbol":
        raise ValueError("book source binding differs")
    start, end = utc_ns(query["start"]), utc_ns(query["end"])
    if end - start != 86400000000000 or start % 86400000000000:
        raise ValueError("exact UTC day required")
    day = datetime.fromtimestamp(start // 1000000000, timezone.utc).date()
    member = audit["files"][0]
    for key in ("source_filename", "vector_file"):
        if not isinstance(member[key], str) or Path(member[key]).name != member[key]:
            raise ValueError("invalid member name")
    source_path = args.source_directory / member["source_filename"]
    vector_path = args.audit_directory / member["vector_file"]
    for path in (source_path, vector_path):
        private_file(path)
    relevant = [audit_path, receipt_path, source_path, vector_path, Path(__file__).resolve(),
                args.proof, args.ledger_snapshot, audited_path, args.decision_plan,
                root / "scripts/research_batch_history.py", root / "scripts/research_history_reader.py"]
    relevant += [Path(p) if Path(p).is_absolute() else root / p for p in book.manifest]
    before = {path: file_hash(path) for path in dict.fromkeys(relevant)}
    if (before[source_path] != member["source_compressed_sha256"]
            or before[vector_path] != member["vector_sha256"] or vector_path.stat().st_size != member["vector_bytes"]):
        raise ValueError("native/vector digest differs")
    receipt_members = [m for m in receipt["files"] if m["filename"] == member["source_filename"]]
    if (len(receipt_members) != 1 or receipt_members[0]["sha256"] != before[source_path]
            or receipt_members[0]["bytes"] != source_path.stat().st_size):
        raise ValueError("source receipt differs")
    decoder = dbn.DBNDecoder(upgrade_policy=dbn.VersionUpgradePolicy.AS_IS, compression=dbn.Compression.NONE)
    metadata, reverse, records, expanded, vector_bytes = None, {}, 0, 0, 0
    decoded_hash, vector_hash = hashlib.sha256(), hashlib.sha256()
    fd = os.open(source_path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as source:
        info = os.fstat(source.fileno())
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1
                or stat.S_IMODE(info.st_mode) != 0o600 or info.st_size != receipt_members[0]["bytes"]):
            raise ValueError("source descriptor differs")
        compressed = hashlib.sha256()
        for raw in iter(lambda: source.read(1 << 20), b""):
            compressed.update(raw)
        if compressed.hexdigest() != before[source_path]:
            raise ValueError("source descriptor hash differs")
        source.seek(0)
        process = subprocess.Popen(["/usr/bin/zstd", "-q", "-f", "-d", "-c", "--", "/dev/fd/" + str(source.fileno())],
                                   pass_fds=(source.fileno(),), stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        try:
            with gzip.open(vector_path, "rb") as vectors:
                for raw in iter(lambda: process.stdout.read(128 << 10), b""):
                    expanded += len(raw)
                    if expanded > book_expansion_limit(member["records"]):
                        raise ValueError("decoded expansion bound")
                    decoded_hash.update(raw)
                    for row in decoder.write_and_decode(raw):
                        if isinstance(row, dbn.Metadata):
                            if (metadata is not None or records or row.version != member["native_version"]
                                    or row.version not in (1, 2, 3) or row.dataset != query["dataset"]
                                    or str(row.schema) != query["schema"] or row.start != start or row.end != end
                                    or str(row.stype_in) != "raw_symbol" or str(row.stype_out) != "instrument_id"
                                    or row.limit not in (None, 0) or type(row.ts_out) is not bool
                                    or len(row.symbols) != len(query["symbols"]) or len(set(row.symbols)) != len(row.symbols)
                                    or set(row.symbols) != set(query["symbols"])):
                                raise ValueError("native metadata scope differs")
                            metadata, seen = row, set()
                            for symbol, intervals in row.mappings.items():
                                if symbol not in query["symbols"]:
                                    raise ValueError("unqueried mapping")
                                for interval in intervals:
                                    if (type(interval["start_date"]) is not date or type(interval["end_date"]) is not date
                                            or not interval["start_date"] < interval["end_date"]):
                                        raise ValueError("mapping dates")
                                    if interval["start_date"] <= day < interval["end_date"]:
                                        instrument = int(interval["symbol"])
                                        if instrument in reverse or symbol in seen or not 0 < instrument < 1 << 32:
                                            raise ValueError("ambiguous mapping")
                                        reverse[instrument] = symbol
                                        seen.add(symbol)
                            continue
                        kind = "CMBP1Msg" if query["schema"] == "cmbp-1" else "MBP1Msg"
                        if metadata is None or type(row).__name__ != kind or row.instrument_id not in reverse or len(row.levels) != 1:
                            raise ValueError("native identity/type differs")
                        expected = [records, metadata.version, reverse[row.instrument_id], row.publisher_id, row.instrument_id,
                                    row.ts_recv, row.ts_event, row.ts_out if metadata.ts_out else None, row.price, row.size,
                                    str(row.action), str(row.side), row.flags, row.ts_in_delta, row.bid_px_00, row.ask_px_00,
                                    row.bid_sz_00, row.ask_sz_00, int(row.rtype)]
                        if query["schema"] == "cmbp-1":
                            expected.extend([row.bid_pb_00, row.ask_pb_00])
                        else:
                            expected.extend([row.bid_ct_00, row.ask_ct_00, row.depth, row.sequence])
                        line = vectors.readline(4097)
                        vector_bytes += len(line)
                        if not line.endswith(b"\n") or len(line) > 4096 or vector_bytes > 512 << 30:
                            raise ValueError("vector record bound")
                        # Canonical bytes prove original integers, null ts_out, enums,
                        # sentinels, and row/file order without tolerant equality.
                        if line != (json.dumps(expected, separators=(",", ":"), ensure_ascii=True) + "\n").encode():
                            raise ValueError("native/vector field mismatch")
                        vector_hash.update(line)
                        records += 1
                        if records > member["records"]:
                            raise ValueError("record count bound")
                    if len(decoder.buffer()) > 16 << 20:
                        raise ValueError("decoder residual")
                if process.wait() != 0 or decoder.buffer() or vectors.read(1) or metadata is None:
                    raise ValueError("native/vector EOF differs")
        finally:
            if process.poll() is None:
                process.kill()
                process.wait()
            process.stdout.close()
    if (records != member["records"] or records != audit["records"] or records != audit["provider_record_count"]
            or expanded != member["source_uncompressed_bytes"] or decoded_hash.hexdigest() != member["source_uncompressed_sha256"]
            or vector_hash.hexdigest() != member["vector_jsonl_sha256"] or any(file_hash(p) != h for p, h in before.items())):
        raise ValueError("native audit totals/input stability differ")
    history.unchanged(); audited_history.unchanged(); plan.unchanged()
    result = dict(schema_version="AutoWheelLargeHistoryNativeBookVectorReferenceV1", observed_at=datetime.now(timezone.utc).isoformat(),
                  reference_script_sha256=before[Path(__file__).resolve()], audit_sha256=before[audit_path], receipt_sha256=before[receipt_path],
                  records_compared_field_for_field=records, vector_fields_per_record=21 if query["schema"] == "cmbp-1" else 23,
                  vector_jsonl_sha256=vector_hash.hexdigest(), query_dataset=query["dataset"], query_schema=query["schema"],
                  query_start=query["start"], query_end=query["end"], queried_symbols=len(query["symbols"]),
                  retimed_rows=0, full_source_qualification=False, historical_performance_available=False,
                  acquisition_proof_sha256=before[args.proof], financial_snapshot_sha256=history.snapshot.sha256,
                  audited_financial_snapshot_sha256=audited_history.snapshot.sha256,
                  decision_plan_sha256=plan.sha256, original_reference_script_sha256="841c580f63a55292884a6090f811fc6e3121dbbc0017be130ec5220c59dbb17a")
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as output:
        output.write((json.dumps(result, indent=2) + "\n").encode())
        output.flush()
        os.fsync(output.fileno())
    directory_fd = os.open(args.output.parent, os.O_RDONLY)
    try:
        os.fsync(directory_fd)
    finally:
        os.close(directory_fd)
    print(json.dumps(result))


if __name__ == "__main__":
    try:
        main()
    except Exception:
        print("native book vector reference failed", file=sys.stderr)
        raise SystemExit(1)
