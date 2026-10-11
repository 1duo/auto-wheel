#!/usr/bin/env python3
"""Queue a bounded window of statistics jobs; serialize every financial action.

Uses the pinned reserved-safe preflight and Go CLIs. No provider HTTP in this file,
financial retry, hold release, or source/replay qualification. All originals and
CLI logs remain in a new private MSI directory. Reserved canonical operations are
protected, never retried, and excluded from new financial work.
"""
import argparse
from collections import Counter
import copy
from datetime import datetime, timezone
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import stat
import sys
import time


PREFLIGHT_SHA = "ea16e637810707eecf217ae2a60a9d72c0277a1a3f16e49007eec8b9eee197c9"
PREFLIGHT_NAME = "acquire-selective-statistics-plan-reserved-safe.py"


LOCK_NAME = ".statistics-reserved-safe.lock"
BATCH_SUBMIT_ENDPOINT = "/batch.submit_job"
STATISTICS_ACQUISITION_RUNNERS = frozenset({
    "acquire-selective-statistics-plan.py",
    "acquire-selective-statistics-plan-large-manifest.py",
    "acquire-selective-statistics-plan-reserved-safe.py",
    "acquire-prefetched-statistics-plan.py",
    "acquire-prefetched-statistics-plan-large-manifest.py",
    "acquire-prefetched-statistics-plan-reserved-safe.py",
    "acquire-prefetched-book-plan.py",
})
PREPAID_RECOVERY_RUNNERS = frozenset({
    "recover-prepaid-statistics-run.py",
    "recover-prepaid-statistics-run-v2.py",
})
RESEARCH_JOURNAL_CLIS = frozenset({
    "autowheel-batch",
    "autowheel-batch-retrieve",
    "autowheel-batch-finalize",
    "autowheel-batch-files",
    "autowheel-batch-planned-retrieve",
})


def private_directory(path):
    info = path.lstat()
    if (not stat.S_ISDIR(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o700
            or info.st_uid != os.getuid() or path.resolve() != path):
        raise ValueError("private research base directory required")


def live_writers_absent():
    """Reject live acquisition/recovery writers and journal CLIs before output."""
    self_pid = os.getpid()
    proc = Path("/proc")
    if not proc.is_dir():
        raise ValueError("Linux process inventory required")
    for cmdline in proc.glob("[0-9]*/cmdline"):
        try:
            pid = int(cmdline.parent.name)
        except ValueError:
            continue
        if pid == self_pid:
            continue
        try:
            if cmdline.stat().st_uid != os.getuid():
                continue
            raw = cmdline.read_bytes()
        except FileNotFoundError:
            continue
        except OSError:
            raise ValueError("owned process inventory unavailable")
        if not raw:
            continue
        fields = raw.split(b"\0")
        decoded = []
        basenames = []
        for field in fields:
            if not field:
                continue
            try:
                entry = os.fsdecode(field)
            except Exception:
                continue
            decoded.append(entry)
            basenames.append(Path(entry).name)
        if not decoded:
            continue
        names = set(basenames)
        if "--execute" in decoded and (names & STATISTICS_ACQUISITION_RUNNERS):
            raise ValueError("live statistics/book acquisition writer still active")
        if ("recover-prepaid-statistics-run.py" in names
                or ("--execute" in decoded and (names & PREPAID_RECOVERY_RUNNERS))):
            raise ValueError("live prepaid recovery writer still active")
        if names & RESEARCH_JOURNAL_CLIS:
            raise ValueError("active research financial/journal CLI still active")


def acquire_reserved_safe_lock(base):
    private_directory(base)
    lock_path = base / LOCK_NAME
    fd = os.open(lock_path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC, 0o600)
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600
                or info.st_uid != os.getuid() or info.st_nlink != 1):
            raise ValueError("private reserved-safe lock identity differs")
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        return fd
    except Exception:
        try:
            os.close(fd)
        except OSError:
            pass
        raise ValueError("reserved-safe acquisition lock already held")


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
    path = Path(__file__).with_name(PREFLIGHT_NAME)
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
    for path in [args.base, args.source_root, args.manifest, args.ledger, args.key_file, args.python, args.output]:
        if not path.is_absolute() or path != Path(os.path.normpath(path)):
            raise ValueError("absolute clean paths required")
    # Guard before preflight creates output; exact basenames, skip self PID.
    live_writers_absent()
    reserved_safe_lock_fd = acquire_reserved_safe_lock(args.base)
    try:
        original_argv = sys.argv
        preflight = load_preflight()
        # The existing complete 543-query validation runs before any financial send.
        argv = [str(Path(__file__))]
        for name in ("base", "source-root", "manifest", "ledger", "key-file", "python", "output", "start-date", "end-date"):
            argv.extend(["--" + name, str(getattr(args, name.replace("-", "_")))])
        try:
            sys.argv = argv
            preflight.main(shared_lock_fd=reserved_safe_lock_fd)
        finally:
            sys.argv = original_argv
        expected = preflight.read_json(args.output / "ledger-before.private.json")
        if preflight.ledger(args.ledger)[1] != expected:
            raise ValueError("journal changed after preflight")
        plan = preflight.read_json(args.output / "plan.json")
        runtime = {**plan, "schema": "AutoWheelReservedSafePrefetchedStatisticsAcquisitionRunV1",
                   "execute": args.execute, "max_pending": args.max_pending,
                   "minimum_submit_spacing_seconds": 4, "poll_spacing_seconds": 20,
                   "pending_job_deadline_seconds": 21600, "financial_actions_serial": True,
                   "original_preflight_sha256": PREFLIGHT_SHA,
                   "runner_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest()}
        preflight.save(args.output / "runtime-plan.json", runtime)
        if not args.execute:
            print(json.dumps(runtime), flush=True)
            return
        rows = {r["operation_id"]: r for r in expected["reservations"]}
        todo = []
        protected = []
        for item in preflight.read_json(args.manifest)["queries"]:
            if item["schema"] != "statistics" or not args.start_date <= item["date"] < args.end_date:
                continue
            query = args.manifest.parent / item["filename"]
            wire = preflight.canonical_query(preflight.read_json(query))
            query_hash = preflight.digest(wire)
            operation = preflight.digest(wire + b"\0/batch.submit_job")
            if operation not in rows:
                todo.append((item, query, operation, query_hash))
            else:
                row = rows[operation]
                if row.get("query_hash") != query_hash or row.get("endpoint") != BATCH_SUBMIT_ENDPOINT:
                    raise ValueError("validated pending scope differs; prior financial binding mismatch")
                status = row.get("status")
                if status == "acquired":
                    continue
                elif status == "reserved":
                    protected.append({"query": item["filename"], "date": item["date"],
                                      "operation_id": operation, "query_hash": query_hash})
                else:
                    raise ValueError("validated pending scope differs; prior financial outcome")
        if len(todo) != plan["required_queries"]:
            raise ValueError("validated pending scope differs")
        if plan.get("protected_reserved_queries") != len(protected):
            raise ValueError("validated protected reserved scope differs")
        planned_protected = {v["operation_id"] for v in plan.get("protected_reserved_identities", [])}
        if {v["operation_id"] for v in protected} != planned_protected:
            raise ValueError("validated protected reserved identities differ")
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
                "pending_queries": len(active), "required_queries": len(todo),
                "protected_reserved_queries": len(protected),
                "financial_retry": False, "holds_released": False,
                "inspection_required": True})
            print(json.dumps({"stopped": True, "phase": phase,
                "completed_queries": len(completed), "financial_retry": False, "holds_released": False}), flush=True)
            raise
    finally:
        try:
            os.close(reserved_safe_lock_fd)
        except OSError:
            pass



if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(json.dumps({"status": "stopped", "error_type": type(error).__name__,
            "inspect_private_evidence": True}), flush=True)
        sys.exit(1)
