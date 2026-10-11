#!/usr/bin/env python3
"""Recover acknowledged statistics jobs from a stopped run without financial requests."""
import argparse
from collections import Counter
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import stat
import sys
import time
from datetime import datetime, timezone

BASE = Path("/home/yuduo/Projects/AutoWheel-research")
ROOT = BASE / "source-afa3075"
RUN = BASE / "cycle46-statistics-prefetch-20261007"
LEDGER = Path("/home/yuduo/.local/share/autowheel-research/databento-budget.json")
KEY = Path("/home/yuduo/.config/autowheel/databento-pilot.key")
CONTROLLER_SHA = "b4eeaa15e796f37be006bf7ae679e00a22592ea8f1d9647fc53119d2f494c11a"


def unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON key")
        result[key] = value
    return result


def decode(raw):
    return json.loads(raw, object_pairs_hook=unique,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite JSON")))


def directory(path):
    info = path.lstat()
    if (not stat.S_ISDIR(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o700
            or info.st_uid != os.getuid() or path.resolve() != path):
        raise ValueError("private directory identity differs")


def journal(p):
    raw = p.private(LEDGER)
    value = decode(raw)
    if (value.get("version") != 2 or value.get("authorization_mode") != "uncapped"
            or value.get("lifetime_nano") != 0
            or not isinstance(value.get("reservations"), list)
            or len({r["operation_id"] for r in value["reservations"]}) != len(value["reservations"])):
        raise ValueError("uncapped journal identity differs")
    return raw, value


def writer_absent():
    for path in Path("/proc").glob("[0-9]*/cmdline"):
        try:
            if path.stat().st_uid != os.getuid():
                continue
            fields = path.read_bytes().split(b"\0")
        except FileNotFoundError:
            continue
        names = [Path(os.fsdecode(v)).name for v in fields if v]
        if ("--execute" in [os.fsdecode(v) for v in fields]
                and any(name.startswith("acquire-") and "statistics-plan" in name for name in names)):
            raise ValueError("statistics acquisition writer still active")
        if ("acquire-prefetched-book-plan.py" in names
                and "--execute" in [os.fsdecode(v) for v in fields]):
            raise ValueError("book acquisition writer still active")
        if any(name in ("autowheel-batch", "autowheel-batch-finalize",
                        "autowheel-batch-retrieve", "autowheel-batch-files",
                        "autowheel-batch-planned-retrieve") for name in names):
            raise ValueError("journal CLI still active")


def lock(path):
    fd = os.open(path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600
                or info.st_uid != os.getuid() or info.st_nlink != 1):
            raise ValueError("private recovery lock identity differs")
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        return fd
    except Exception:
        os.close(fd)
        raise


def load():
    path = ROOT / "scripts/acquire-prefetched-statistics-plan-large-manifest.py"
    if path.is_symlink() or hashlib.sha256(path.read_bytes()).hexdigest() != CONTROLLER_SHA:
        raise ValueError("accepted statistics controller differs")
    spec = importlib.util.spec_from_file_location("sealed_statistics_prefetch", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module, module.load_preflight()


def plan(p, ledger):
    source_initial = decode(p.private(RUN / "ledger-before.private.json"))
    if ledger["reservations"][:len(source_initial["reservations"])] != source_initial["reservations"]:
        raise ValueError("original financial prefix differs")
    runtime = decode(p.private(RUN / "runtime-plan.json"))
    interrupted = decode(p.private(RUN / "interruption.json"))
    if (runtime["runner_sha256"] != CONTROLLER_SHA or runtime["execute"] is not True
            or interrupted["phase"] != "submit" or interrupted["financial_retry"] is not False
            or interrupted["holds_released"] is not False):
        raise ValueError("owned stopped-run identity differs")
    manifest = BASE / "cycle24-selective-acquisition-queries-20261006/manifest.json"
    if p.digest(p.private(manifest)) != p.MANIFEST_SHA:
        raise ValueError("full frozen manifest differs")
    queries = {}
    dates = set()
    for item in decode(p.private(manifest))["queries"]:
        if item["schema"] != "statistics":
            continue
        if not re.fullmatch(r"2026-\d\d-\d\d-statistics-\d{3}\.json", item["filename"]):
            raise ValueError("canonical query filename differs")
        path = manifest.parent / item["filename"]
        raw = p.private(path)
        if p.digest(raw) != item["sha256"]:
            raise ValueError("canonical query bytes differ")
        wire = p.canonical_query(decode(raw))
        operation = p.digest(wire + b"\0/batch.submit_job")
        if operation in queries:
            raise ValueError("duplicate canonical query")
        queries[operation] = (item, path, p.digest(wire))
        dates.add(item["date"])
    if len(queries) != 543 or len(dates) != 126:
        raise ValueError("full frozen statistics scope differs")
    rows = {r["operation_id"]: r for r in ledger["reservations"]}
    jobs, skipped, unknown = [], 0, []
    acknowledged = 0
    for original in sorted(RUN.glob("2026-*-statistics-*")):
        directory(original)
        intent = decode(p.private(original / "intent.json"))
        operation = intent["operation_id"]
        if operation not in queries or operation not in rows or intent["financial_retry"] is not False:
            raise ValueError("original intent scope differs")
        item, query, query_hash = queries[operation]
        row = rows[operation]
        if (original.name + ".json" != item["filename"]
                or intent["query_file_sha256"] != item["sha256"]
                or row["query_hash"] != query_hash or row["endpoint"] != "/batch.submit_job"):
            raise ValueError("original intent and held query differ")
        ack_path = original / "submit/ack.json"
        if not ack_path.exists():
            if row["status"] != "reserved" or item["filename"] != interrupted["query"]:
                raise ValueError("unacknowledged operation identity differs")
            attempt = decode(p.private(original / "submit/attempt.json"))
            if attempt["operation_id"] != operation or attempt["query_hash"] != query_hash:
                raise ValueError("retained ambiguous attempt differs")
            unknown.append({"query": item["filename"], "operation_id": operation,
                            "original_hold_preserved": True, "financial_retry": False})
            continue
        ack = decode(p.private(ack_path))
        body = p.private(original / "submit/job.json")
        if (ack["operation_id"] != operation or ack["query_hash"] != query_hash
                or ack["endpoint"] != "/batch.submit_job" or ack["status_code"] != 200
                or ack["response_hash"] != p.digest(body) or ack["bytes"] != len(body)
                or any(ack[k] != row[k] for k in
                       ("operation_id", "query_hash", "endpoint", "cost_nano", "reserved_nano", "at"))
                or row["response_hash"] != decode(p.private(original / "submit/quote.json"))["quote"]["ResponseHash"]):
            raise ValueError("original acknowledgement and reservation differ")
        acknowledged += 1
        if row["status"] == "acquired":
            proof_raw = p.private(original / "proof.json")
            if row["data_sha256"] != p.digest(proof_raw):
                raise ValueError("previous acquisition proof differs")
            skipped += 1
        elif row["status"] == "reserved":
            jobs.append((original, query, operation, item))
        else:
            raise ValueError("original operation status differs")
    if (acknowledged != interrupted["submitted_queries"]
            or skipped != interrupted["completed_queries"]
            or len(jobs) != interrupted["pending_queries"] or len(unknown) != 1):
        raise ValueError("stopped-run counts differ")
    return jobs, skipped, unknown


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--execute", action="store_true")
    args = parser.parse_args()
    for path in (BASE, ROOT, RUN):
        directory(path)
    if (args.output.parent != BASE or not re.fullmatch(r"cycle50-prepaid-recovery(?:-\d+)?-20261007",
                                                     args.output.name) or args.output.exists()):
        raise ValueError("fresh recovery namespace required")
    writer_absent()
    fd = lock(BASE / ".prepaid-statistics-recovery.lock")
    try:
        m, p = load()
        qualification = p.read_qualification(ROOT / "research/simulation/2026-10-05/databento-qualification.json")
        expected_sources = {v["path"]: v["sha256"] for v in qualification["source_files"]}
        if len(expected_sources) != len(qualification["source_files"]):
            raise ValueError("duplicate qualification source")
        for relative in p.CODE:
            path = ROOT / relative
            if path.is_symlink() or p.digest(path.read_bytes()) != expected_sources.get(relative):
                raise ValueError("sealed acquisition source differs")
        tools = {}
        for name, (relative, wanted) in p.TOOLS.items():
            path = BASE / relative
            info = path.lstat()
            if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                    or info.st_nlink != 1 or path.resolve() != path
                    or info.st_mode & 0o022 or p.digest(path.read_bytes()) != wanted):
                raise ValueError("sealed research binary differs")
            tools[name] = path
        raw, expected = journal(p)
        jobs, skipped, unknown = plan(p, expected)
        args.output.mkdir(mode=0o700)
        p.save(args.output / "ledger-before.private.json", expected)
        report = {"schema": "AutoWheelPrepaidStatisticsRecoveryV2",
                  "observed_at": datetime.now(timezone.utc).isoformat(),
                  "runner_sha256": p.digest(Path(__file__).read_bytes()),
                  "controller_sha256": CONTROLLER_SHA,
                  "journal_sha256": p.digest(raw), "source_run": str(RUN),
                  "execute": args.execute, "acknowledged_reserved_jobs": len(jobs),
                  "already_acquired_skipped": skipped, "ambiguous_operation": unknown,
                  "financial_requests": 0, "financial_retries": 0, "holds_released": False,
                  "source_qualified": False, "historical_performance_available": False}
        p.save(args.output / "plan.json", report)
        print(json.dumps({k: v for k, v in report.items() if k != "ambiguous_operation"}), flush=True)
        if not args.execute:
            return
        completed = []
        for original, query, operation, item in jobs:
            writer_absent()
            out = args.output / original.name
            out.mkdir(mode=0o700)
            common = ["--query", query, "--ack", original / "submit/ack.json",
                      "--ack-body", original / "submit/job.json", "--key-file", KEY]
            for poll in range(1080):
                if journal(p)[1] != expected:
                    raise ValueError("unexpected journal mutation")
                receipt = m.observe_readonly(p, tools["retrieve"], common, out, "observe-" + str(poll))
                if receipt["state"] == "done":
                    break
                if receipt["state"] not in ("queued", "processing"):
                    raise ValueError("unknown prepaid state")
                time.sleep(20)
            else:
                raise ValueError("prepaid deadline; retain original hold")
            fs = os.statvfs(BASE)
            if fs.f_bavail * fs.f_frsize < 6 << 30:
                raise ValueError("research capacity reserve unavailable")
            if p.run(tools["retrieve"], [*common, "--output", out / "acquired",
                                       "--timeout", "30m", "--download"], out, "download") != 0:
                raise ValueError("prepaid download stopped")
            if journal(p)[1] != expected:
                raise ValueError("unexpected journal mutation")
            proof = out / "proof.json"
            if p.run(tools["finalize"], ["--query", query, "--ack", original / "submit/ack.json",
                     "--ack-body", original / "submit/job.json", "--source", out / "acquired",
                     "--proof", proof, "--python", BASE / "dbn-env-0.71.0/bin/python",
                     "--ledger", LEDGER, "--timeout", "30m"], out, "finalize") != 0:
                raise ValueError("offline native verification stopped")
            proof_raw = p.private(proof)
            audit = decode(proof_raw)
            if (audit["query_file_sha256"] != item["sha256"]
                    or audit["native"]["all_eof_verified"] is not True
                    or audit["native"]["records"] != audit["provider_record_count"]
                    or audit["source_qualified"] is not False):
                raise ValueError("native proof binding differs")
            _, current = journal(p)
            m.finalized(expected, current, operation, p.digest(proof_raw), audit["total_bytes"])
            expected = current
            result = {"query": item["filename"], "records": audit["native"]["records"],
                      "compressed_bytes": audit["total_bytes"],
                      "proof_sha256": p.digest(proof_raw), "all_eof_verified": True,
                      "financial_requests": 0, "source_qualified": False}
            p.save(out / "aggregate.json", result)
            completed.append(result)
            print(json.dumps({"completed": len(completed), "required": len(jobs), **result}), flush=True)
        p.save(args.output / "summary.json", {**report, "completed": completed,
               "journal_sha256_after": p.digest(journal(p)[0]), "complete": True,
               "observed_at": datetime.now(timezone.utc).isoformat()})
    finally:
        os.close(fd)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(json.dumps({"stopped": True, "error_type": type(error).__name__,
                          "financial_requests": 0, "holds_released": False,
                          "inspect_private_evidence": True}), flush=True)
        raise SystemExit(1)
