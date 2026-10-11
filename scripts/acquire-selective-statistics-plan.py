#!/usr/bin/env python3
"""Acquire immutable statistics partitions through the existing Go research CLIs.

No direct provider transport, financial retry, hold release, or replay qualification.
All CLI output and original evidence stay in a new private directory on MSI.
"""
import argparse
from collections import Counter
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import time

from batch_acquisition_proof import canonical_query, read_json

MANIFEST_SHA = "ccf48c229ce29db62e0d4aaa20cb06c733fdf4e528cf2d79f4da0e71a00adb2b"
TOOLS = {
    "batch": ("cycle26-research-tools-20261006/autowheel-batch", "cbfeef447782c048da427d7b15fee740c6e444edeab5e89033346e311e575c5b"),
    "retrieve": ("cycle26-research-tools-20261006/autowheel-batch-retrieve", "c1f2ba5a6357db9dddf308c7017409167ca87013c7e6b48f9dc993f255e61a4e"),
    "finalize": ("cycle26-autowheel-batch-finalize-20261006", "236129cbe96a3e8db6818b566a333a83ff89f8263df55aba1dca7b792885bed0"),
}
CODE = ["cmd/autowheel-batch/main.go", "cmd/autowheel-batch-retrieve/main.go",
        "cmd/autowheel-batch-finalize/main.go", "internal/databento/batch.go",
        "internal/databento/batch_retrieve.go", "internal/databento/batch_finalize.go",
        "internal/databento/ledger.go", "internal/databento/native_batch_completeness.py",
        "scripts/batch_acquisition_proof.py"]


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def private(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        st = os.fstat(fd)
        parent = path.parent.lstat()
        if (not stat.S_ISREG(st.st_mode) or stat.S_IMODE(st.st_mode) != 0o600
                or st.st_uid != os.getuid() or st.st_nlink != 1
                or not stat.S_ISDIR(parent.st_mode) or stat.S_IMODE(parent.st_mode) != 0o700
                or parent.st_uid != os.getuid() or st.st_size > 4 << 20):
            raise ValueError("private evidence permissions")
        with os.fdopen(fd, "rb", closefd=False) as source:
            raw = source.read((4 << 20) + 1)
        if not raw or len(raw) > 4 << 20:
            raise ValueError("private evidence size")
        return raw
    finally:
        os.close(fd)


def save(path, value):
    raw = (json.dumps(value, indent=2, sort_keys=True) + "\n").encode()
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "wb") as target:
        target.write(raw)
        target.flush()
        os.fsync(target.fileno())
    fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def ledger(path):
    raw = private(path)
    value = read_json(path)
    if value.get("version") != 2 or value.get("authorization_mode") != "uncapped" or value.get("lifetime_nano") != 0:
        raise ValueError("uncapped v2 authorization required")
    rows = value["reservations"]
    if len({r["operation_id"] for r in rows}) != len(rows):
        raise ValueError("duplicate financial operation")
    return raw, value


def run(binary, args, directory, phase):
    # Never print CLI stdout: it can contain provider identifiers or raw errors.
    logs = []
    try:
        for suffix in ("stdout", "stderr"):
            fd = os.open(directory / (phase + "." + suffix + ".private.log"),
                         os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            logs.append(os.fdopen(fd, "wb"))
        process = subprocess.run([str(binary), *map(str, args)], stdout=logs[0], stderr=logs[1], check=False)
        for stream in logs:
            stream.flush()
            os.fsync(stream.fileno())
        return process.returncode
    finally:
        for stream in logs:
            stream.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", type=Path, required=True)
    parser.add_argument("--source-root", type=Path, required=True)
    parser.add_argument("--manifest", type=Path, required=True)
    parser.add_argument("--ledger", type=Path, required=True)
    parser.add_argument("--key-file", type=Path, required=True)
    parser.add_argument("--python", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--start-date", default="2026-04-07")
    parser.add_argument("--end-date", default="2026-10-03")
    parser.add_argument("--execute", action="store_true")
    args = parser.parse_args()
    for path in [args.base, args.source_root, args.manifest, args.ledger, args.key_file, args.python, args.output]:
        if not path.is_absolute() or path != Path(os.path.normpath(path)):
            raise ValueError("absolute clean paths required")
    if not "2026-04-06" <= args.start_date < args.end_date <= "2026-10-03":
        raise ValueError("date scope invalid")
    original, before = ledger(args.ledger)
    raw = private(args.manifest)
    if digest(raw) != MANIFEST_SHA:
        raise ValueError("immutable acquisition plan differs")
    manifest = read_json(args.manifest)
    source_manifest = {v["path"]: v["sha256"] for v in read_json(args.source_root / "research/simulation/2026-10-05/databento-qualification.json")["source_files"]}
    for name in CODE:
        if digest((args.source_root / name).read_bytes()) != source_manifest.get(name):
            raise ValueError("accepted acquisition source differs")
    binaries = {}
    for name, (relative, expected) in TOOLS.items():
        path = args.base / relative
        if path.is_symlink() or not path.is_file() or digest(path.read_bytes()) != expected:
            raise ValueError("accepted research binary differs")
        binaries[name] = path
    queries = [v for v in manifest["queries"] if v["schema"] == "statistics"]
    if len(queries) != 543 or manifest["daily_partitions"] != 126 or manifest["acquisition_symbol_days"] != 955003:
        raise ValueError("frozen full-study scope differs")
    rows = {v["operation_id"]: v for v in before["reservations"]}
    pending, skipped, seen = [], 0, set()
    for item in queries:
        if not re.fullmatch(r"2026-\d{2}-\d{2}-statistics-\d{3}\.json", item["filename"]):
            raise ValueError("query basename invalid")
        path = args.manifest.parent / item["filename"]
        raw = private(path)
        if len(raw) != item["bytes"] or digest(raw) != item["sha256"]:
            raise ValueError("query source binding differs")
        query = read_json(path)
        if (query["dataset"] != "OPRA.PILLAR" or query["schema"] != "statistics"
                or query["stype_in"] != "raw_symbol" or len(query["symbols"]) != item["symbols"]
                or query["start"] != item["date"] + "T00:00:00Z"):
            raise ValueError("statistics query scope differs")
        wire = canonical_query(query)
        op = digest(wire + b"\0/batch.submit_job")
        if op in seen:
            raise ValueError("duplicate planned canonical operation")
        seen.add(op)
        if not args.start_date <= item["date"] < args.end_date:
            continue
        if op in rows:
            if rows[op]["status"] != "acquired":
                raise ValueError("selected query has a prior financial outcome; no retry")
            skipped += 1
        else:
            pending.append((item, path, op))
    args.output.mkdir(mode=0o700)
    save(args.output / "ledger-before.private.json", before)
    plan = {"schema": "AutoWheelStatisticsAcquisitionRunV1", "manifest_sha256": MANIFEST_SHA,
            "start_date": args.start_date, "end_date_exclusive": args.end_date,
            "required_queries": len(pending), "already_acquired_skipped": skipped,
            "execute": args.execute, "uncapped": True, "initial_journal_sha256": digest(original),
            "financial_retry": False, "source_qualified": False, "historical_performance_available": False}
    save(args.output / "plan.json", plan)
    print(json.dumps(plan), flush=True)
    if not args.execute:
        if private(args.ledger) != original:
            raise ValueError("ledger changed during preflight")
        return
    expected_rows = before["reservations"]
    completed = []
    for item, query, op in pending:
        phase = "preflight"
        try:
            _, current = ledger(args.ledger)
            if current["reservations"] != expected_rows:
                raise ValueError("unexpected financial journal mutation")
            if os.statvfs(args.base).f_bavail * os.statvfs(args.base).f_frsize < 6 << 30:
                raise ValueError("research capacity reserve unavailable")
            name = item["filename"].removesuffix(".json")
            out = args.output / name
            out.mkdir(mode=0o700)
            # Durable intent precedes the CLI's durable hold and sole financial send.
            save(out / "intent.json", {"query_file_sha256": item["sha256"], "operation_id": op, "financial_retry": False})
            submit = out / "submit"
            phase = "submit"
            if run(binaries["batch"], ["--query", query, "--output", submit, "--key-file", args.key_file, "--submit"], out, phase) != 0:
                raise ValueError("financial attempt stopped; evidence and any hold preserved")
            ack = read_json(submit / "ack.json")
            if ack["operation_id"] != op or ack["status_code"] != 200:
                raise ValueError("new acknowledgement binding differs")
            common = ["--query", query, "--ack", submit / "ack.json", "--ack-body", submit / "job.json", "--key-file", args.key_file]
            # Poll existing prepaid job only; observations are immutable and never financial retries.
            for poll in range(90):
                phase = "observe-" + str(poll)
                observation = out / phase
                if run(binaries["retrieve"], [*common, "--output", observation, "--timeout", "1m"], out, phase) != 0:
                    raise ValueError("prepaid observation failed; hold preserved")
                receipt = read_json(observation / "receipt.json")
                if receipt["state"] == "done":
                    break
                if receipt["state"] not in ("queued", "processing"):
                    raise ValueError("unknown prepaid job state")
                time.sleep(20)
            else:
                raise ValueError("prepaid job still pending; hold preserved")
            phase = "download"
            acquired = out / "acquired"
            if run(binaries["retrieve"], [*common, "--output", acquired, "--timeout", "30m", "--download"], out, phase) != 0:
                raise ValueError("download stopped; partial private evidence retained")
            proof_path = out / "proof.json"
            phase = "finalize"
            if run(binaries["finalize"], ["--query", query, "--ack", submit / "ack.json", "--ack-body", submit / "job.json",
                    "--source", acquired, "--proof", proof_path, "--python", args.python, "--ledger", args.ledger, "--timeout", "30m"], out, phase) != 0:
                raise ValueError("offline acquisition verification failed; hold preserved")
            proof = read_json(proof_path)
            _, after = ledger(args.ledger)
            additions = [v for v in after["reservations"] if v["operation_id"] == op]
            if (len(after["reservations"]) != len(expected_rows) + 1 or after["reservations"][:-1] != expected_rows
                    or len(additions) != 1 or after["reservations"][-1] != additions[0]
                    or additions[0]["status"] != "acquired" or additions[0]["data_sha256"] != digest(private(proof_path))
                    or proof["query_file_sha256"] != item["sha256"] or proof["native"]["all_eof_verified"] is not True
                    or proof["native"]["records"] != proof["provider_record_count"] or proof["source_qualified"] is not False):
                raise ValueError("finalized original binding or journal prefix differs")
            expected_rows = after["reservations"]
            result = {"query": item["filename"], "date": item["date"], "proof_sha256": digest(private(proof_path)),
                      "records": proof["native"]["records"], "compressed_bytes": proof["total_bytes"],
                      "quoted_cost_nano": additions[0]["cost_nano"], "all_eof_verified": True,
                      "journal_entries": len(expected_rows), "original_journal_prefix_unchanged": True,
                      "source_qualified": False, "historical_performance_available": False}
            save(out / "aggregate.json", result)
            completed.append(result)
            print(json.dumps({"completed_queries": len(completed), "required_queries": len(pending), **result}), flush=True)
        except Exception:
            save(args.output / "interruption.json", {"phase": phase, "query": item["filename"],
                 "completed_queries": len(completed), "financial_retry": False, "holds_released": False,
                 "inspection_required": True})
            print(json.dumps({"stopped": True, "phase": phase, "completed_queries": len(completed), "financial_retry": False, "holds_released": False}), flush=True)
            raise
    final_raw, final = ledger(args.ledger)
    save(args.output / "ledger-after.private.json", final)
    save(args.output / "summary.json", {**plan, "completed_queries": len(completed), "complete_acquisition_run": True,
         "records": sum(v["records"] for v in completed), "compressed_bytes": sum(v["compressed_bytes"] for v in completed),
         "quoted_cost_nano": sum(v["quoted_cost_nano"] for v in completed), "journal_sha256": digest(final_raw),
         "journal_status_counts": dict(Counter(v["status"] for v in final["reservations"])),
         "original_journal_prefix_unchanged": final["reservations"][:len(before["reservations"])] == before["reservations"],
         "completed": completed, "observed_at": datetime.now(timezone.utc).isoformat()})
    print(json.dumps({"complete_acquisition_run": True, "completed_queries": len(completed), "journal_entries": len(final["reservations"])}), flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        # Do not forward exception contents, provider output, licensed rows, or identifiers.
        print(json.dumps({"status": "stopped", "error_type": type(error).__name__, "inspect_private_evidence": True}), flush=True)
        sys.exit(1)
