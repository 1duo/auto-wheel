#!/usr/bin/env python3
"""Queue a bounded window of statistics jobs; serialize every financial action.

Uses the pinned, existing preflight and Go CLIs. No provider HTTP in this file,
financial retry, hold release, or source/replay qualification. All originals and
CLI logs remain in a new private MSI directory.
"""
import argparse
from collections import Counter
import copy
from datetime import datetime, timezone
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import sys
import time


PREFLIGHT_SHA = "7ee317510c09f48f3dfaec9236f58aab75157bf1690a6fa27d26d626f2a53875"


def appended(expected, current, operation, query_hash, acknowledgement, quote_hash, body):
    """Only a single new held row may follow a successful financial send."""
    before, after = expected["reservations"], current["reservations"]
    if (len(after) != len(before) + 1 or after[:-1] != before
            or {k: v for k, v in current.items() if k != "reservations"}
            != {k: v for k, v in expected.items() if k != "reservations"}):
        raise ValueError("unexpected journal change after financial send")
    row = after[-1]
    if (row["operation_id"] != operation or row["query_hash"] != query_hash
            or row["endpoint"] != "/batch.submit_job" or row["status"] != "reserved"
            or acknowledgement["operation_id"] != operation
            or acknowledgement["status_code"] != 200
            or row["response_hash"] != quote_hash
            or acknowledgement["response_hash"] != hashlib.sha256(body).hexdigest()
            or acknowledgement["bytes"] != len(body)
            or any(row[k] != acknowledgement[k] for k in
                   ("query_hash", "endpoint", "cost_nano", "reserved_nano", "at"))):
        raise ValueError("new acknowledgement or reservation binding differs")


def finalized(expected, current, operation, proof_sha, total_bytes):
    """Allow only proof fields on this row, including when it is not the tail."""
    rows = expected["reservations"]
    indices = [i for i, row in enumerate(rows) if row["operation_id"] == operation]
    if len(indices) != 1 or rows[indices[0]]["status"] != "reserved":
        raise ValueError("original held operation missing")
    wanted = copy.deepcopy(expected)
    wanted["reservations"][indices[0]].update(
        status="acquired", data_sha256=proof_sha, bytes=total_bytes)
    if current != wanted:
        raise ValueError("offline proof changed unrelated or financial journal fields")


def load_preflight():
    path = Path(__file__).with_name("acquire-selective-statistics-plan.py")
    if path.is_symlink() or hashlib.sha256(path.read_bytes()).hexdigest() != PREFLIGHT_SHA:
        raise ValueError("pinned preflight differs")
    spec = importlib.util.spec_from_file_location("statistics_preflight", path)
    module = importlib.util.module_from_spec(spec)
    old_path = sys.path[:]
    try:
        sys.path.insert(0, str(path.resolve().parent))
        spec.loader.exec_module(module)
    finally:
        sys.path[:] = old_path
    return module


def observe_readonly(preflight, binary, common, directory, label):
    """Retry only a failed read transport, with fresh immutable evidence paths."""
    for attempt in range(3):
        phase = label + "-read-" + str(attempt)
        observation = directory / phase
        result = preflight.run(binary, [*common, "--output", observation,
                               "--timeout", "1m"], directory, phase)
        if result == 0:
            return preflight.read_json(observation / "receipt.json")
        error = preflight.private(directory / (phase + ".stderr.private.log"))
        if error.strip() != b"databento batch observation failed" or attempt == 2:
            raise ValueError("prepaid observation stopped; preserve evidence and hold")
        time.sleep(10 * (attempt + 1))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("base", "source-root", "manifest", "ledger", "key-file", "python", "output"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--start-date", default="2026-04-08")
    parser.add_argument("--end-date", default="2026-10-03")
    parser.add_argument("--max-pending", type=int, default=12)
    parser.add_argument("--execute", action="store_true")
    args = parser.parse_args()
    if not 1 <= args.max_pending <= 24:
        raise ValueError("bounded pending window required")
    original_argv = sys.argv
    preflight = load_preflight()
    # The existing complete 543-query validation runs before any financial send.
    argv = [str(Path(__file__))]
    for name in ("base", "source-root", "manifest", "ledger", "key-file", "python", "output", "start-date", "end-date"):
        argv.extend(["--" + name, str(getattr(args, name.replace("-", "_")))])
    try:
        sys.argv = argv
        preflight.main()
    finally:
        sys.argv = original_argv
    expected = preflight.read_json(args.output / "ledger-before.private.json")
    if preflight.ledger(args.ledger)[1] != expected:
        raise ValueError("journal changed after preflight")
    plan = preflight.read_json(args.output / "plan.json")
    runtime = {**plan, "schema": "AutoWheelPrefetchedStatisticsAcquisitionRunV1",
               "execute": args.execute, "max_pending": args.max_pending,
               "minimum_submit_spacing_seconds": 4, "poll_spacing_seconds": 20,
               "pending_job_deadline_seconds": 21600, "financial_actions_serial": True,
               "original_preflight_sha256": PREFLIGHT_SHA,
               "runner_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest()}
    preflight.save(args.output / "runtime-plan.json", runtime)
    if not args.execute:
        print(json.dumps(runtime), flush=True)
        return
    existing = {r["operation_id"] for r in expected["reservations"]}
    todo = []
    for item in preflight.read_json(args.manifest)["queries"]:
        if item["schema"] != "statistics" or not args.start_date <= item["date"] < args.end_date:
            continue
        query = args.manifest.parent / item["filename"]
        wire = preflight.canonical_query(preflight.read_json(query))
        operation = preflight.digest(wire + b"\0/batch.submit_job")
        if operation not in existing:
            todo.append((item, query, operation, preflight.digest(wire)))
    if len(todo) != plan["required_queries"]:
        raise ValueError("validated pending scope differs")
    binaries = {k: args.base / v[0] for k, v in preflight.TOOLS.items()}
    initial_count = len(expected["reservations"])
    active, completed, submitted = [], [], 0
    last_submit = float("-inf")
    phase, item = "initial", {"filename": None}

    def check_journal():
        if preflight.ledger(args.ledger)[1] != expected:
            raise ValueError("unexpected financial journal mutation")

    def capacity():
        fs = os.statvfs(args.base)
        if fs.f_bavail * fs.f_frsize < 6 << 30:
            raise ValueError("research capacity reserve unavailable")

    try:
        while submitted < len(todo) or active:
            while submitted < len(todo) and len(active) < args.max_pending:
                item, query, operation, query_hash = todo[submitted]
                phase = "pre-submit"
                time.sleep(max(0, 4 - (time.monotonic() - last_submit)))
                check_journal()
                capacity()
                out = args.output / item["filename"].removesuffix(".json")
                out.mkdir(mode=0o700)
                preflight.save(out / "intent.json", {"query_file_sha256": item["sha256"],
                    "operation_id": operation, "financial_retry": False})
                phase = "submit"
                last_submit = time.monotonic()
                submit = out / "submit"
                if preflight.run(binaries["batch"], ["--query", query, "--output", submit,
                        "--key-file", args.key_file, "--submit"], out, phase) != 0:
                    raise ValueError("financial attempt stopped; evidence and hold preserved")
                acknowledgement = preflight.read_json(submit / "ack.json")
                _, current = preflight.ledger(args.ledger)
                quote_hash = preflight.read_json(submit / "quote.json")["quote"]["ResponseHash"]
                appended(expected, current, operation, query_hash, acknowledgement,
                         quote_hash, preflight.private(submit / "job.json"))
                expected = current
                active.append({"item": item, "query": query, "operation": operation,
                               "out": out, "submitted": time.monotonic(), "last_poll": float("-inf"), "poll": 0})
                submitted += 1
                print(json.dumps({"submitted_queries": submitted,
                    "required_queries": len(todo), "pending_queries": len(active),
                    "journal_entries": len(expected["reservations"])}), flush=True)
            for job in tuple(active):
                item, query, operation, out = (job[k] for k in ("item", "query", "operation", "out"))
                phase = "pre-observe"
                now = time.monotonic()
                if now - job["submitted"] > runtime["pending_job_deadline_seconds"]:
                    raise ValueError("prepaid job deadline; inspect existing job, never resubmit")
                if now - job["last_poll"] < 20:
                    continue
                check_journal()
                common = ["--query", query, "--ack", out / "submit/ack.json",
                          "--ack-body", out / "submit/job.json", "--key-file", args.key_file]
                phase = "observe-" + str(job["poll"])
                receipt = observe_readonly(preflight, binaries["retrieve"], common, out, phase)
                job["poll"] += 1
                job["last_poll"] = time.monotonic()
                if receipt["state"] in ("queued", "processing"):
                    continue
                if receipt["state"] != "done":
                    raise ValueError("unknown prepaid job state")
                capacity()
                phase = "download"
                acquired = out / "acquired"
                if preflight.run(binaries["retrieve"], [*common, "--output", acquired,
                        "--timeout", "30m", "--download"], out, phase) != 0:
                    raise ValueError("download stopped; private evidence retained")
                check_journal()
                phase = "finalize"
                proof_path = out / "proof.json"
                if preflight.run(binaries["finalize"], ["--query", query, "--ack", out / "submit/ack.json",
                        "--ack-body", out / "submit/job.json", "--source", acquired, "--proof", proof_path,
                        "--python", args.python, "--ledger", args.ledger, "--timeout", "30m"], out, phase) != 0:
                    raise ValueError("offline acquisition verification stopped; hold preserved")
                proof = preflight.read_json(proof_path)
                proof_sha = preflight.digest(preflight.private(proof_path))
                if (proof["query_file_sha256"] != item["sha256"]
                        or proof["native"]["all_eof_verified"] is not True
                        or proof["native"]["records"] != proof["provider_record_count"]
                        or proof["source_qualified"] is not False):
                    raise ValueError("native proof binding differs")
                _, current = preflight.ledger(args.ledger)
                finalized(expected, current, operation, proof_sha, proof["total_bytes"])
                expected = current
                row = next(r for r in expected["reservations"] if r["operation_id"] == operation)
                result = {"query": item["filename"], "date": item["date"], "proof_sha256": proof_sha,
                          "records": proof["native"]["records"], "compressed_bytes": proof["total_bytes"],
                          "quoted_cost_nano": row["cost_nano"], "all_eof_verified": True,
                          "journal_entries": len(expected["reservations"]),
                          "original_journal_prefix_unchanged": True, "source_qualified": False,
                          "historical_performance_available": False}
                preflight.save(out / "aggregate.json", result)
                completed.append(result)
                active.remove(job)
                print(json.dumps({"completed_queries": len(completed),
                    "required_queries": len(todo), "pending_queries": len(active), **result}), flush=True)
            if active:
                time.sleep(1)
        check_journal()
        final_raw, final = preflight.ledger(args.ledger)
        preflight.save(args.output / "ledger-after.private.json", final)
        preflight.save(args.output / "summary.json", {**runtime, "completed_queries": len(completed),
            "complete_acquisition_run": True, "records": sum(v["records"] for v in completed),
            "compressed_bytes": sum(v["compressed_bytes"] for v in completed),
            "quoted_cost_nano": sum(v["quoted_cost_nano"] for v in completed),
            "journal_sha256": preflight.digest(final_raw),
            "journal_status_counts": dict(Counter(v["status"] for v in final["reservations"])),
            "original_journal_prefix_unchanged": final["reservations"][:initial_count]
                == preflight.read_json(args.output / "ledger-before.private.json")["reservations"],
            "completed": completed, "observed_at": datetime.now(timezone.utc).isoformat()})
        print(json.dumps({"complete_acquisition_run": True,
            "completed_queries": len(completed), "journal_entries": len(final["reservations"])}), flush=True)
    except Exception:
        preflight.save(args.output / "interruption.json", {"phase": phase, "query": item["filename"],
            "submitted_queries": submitted, "completed_queries": len(completed),
            "pending_queries": len(active), "financial_retry": False, "holds_released": False,
            "inspection_required": True})
        print(json.dumps({"stopped": True, "phase": phase,
            "completed_queries": len(completed), "financial_retry": False, "holds_released": False}), flush=True)
        raise


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(json.dumps({"status": "stopped", "error_type": type(error).__name__,
            "inspect_private_evidence": True}), flush=True)
        sys.exit(1)
