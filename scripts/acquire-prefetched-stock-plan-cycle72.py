#!/usr/bin/env python3
"""Queue only the frozen all15 stock jobs; serialize every financial action.

Reserved-safe fork of acquire-prefetched-book-plan.py. Uses the pinned
existing statistics controller and preflight helpers. No provider HTTP in
this file, financial retry, hold release, or source/replay qualification.
All originals and CLI logs remain in new private MSI directories.
Research-only; never qualify sources, Greeks, frames, or P&L.

Statistics coverage: permits quote acquisition only when the canonical
frozen 543-operation statistics set is exactly 542 acquired plus the
explicitly protected unknown reserved operation
01593d27c3a667a42b8736bba57c377e98dd6f19949f4601e2c08ef505abbf5f
(query SHA 57c057ea1bd4e1420f3f93e948e790c06edea97e888d14249c83f01b56d9efe7;
Sep18-statistics002), or all 543 acquired. Every statistics row endpoint
(/batch.submit_job) and exact canonical query hash is revalidated; any
fresh/missing/wrong status/wrong hash/other canonical reserved operation
fails execution. Reports 542+1 distinctly. Holds both
.statistics-reserved-safe.lock and cycle-book-controller.lock through the
entire execute run with exact process-basename guards. No new financial
retry or release; no archive/retrieve retry.
"""
import argparse
from collections import Counter
from datetime import date as _date, datetime, timezone, timedelta
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

OLD_CONTROLLER_SHA = "b4eeaa15e796f37be006bf7ae679e00a22592ea8f1d9647fc53119d2f494c11a"
OPTION_MANIFEST_SHA = "ccf48c229ce29db62e0d4aaa20cb06c733fdf4e528cf2d79f4da0e71a00adb2b"
STOCK_MANIFEST_SHA = "25056b1d6d41d3a97e9ffb7700cf157b3f195a1eff085b1bc8f2f5a4ba9b5333"
BINDER_SHA256 = "5b76796da56b637f4e674e54ba0f1db7905e0f56cc74e18b078637c4834ab2e4"
BINDER_RELATIVE = Path("cycle30-bindfs-runtime-20261006/runtime/usr/bin/bindfs")
PREDECESSOR_PID = 3940348
RESERVE_BYTES = 6 << 30
LOCK_NAME = "cycle-book-controller.lock"
RESERVED_SAFE_LOCK_NAME = ".statistics-reserved-safe.lock"
BATCH_SUBMIT_ENDPOINT = "/batch.submit_job"
PROTECTED_RESERVED_OPERATION = "01593d27c3a667a42b8736bba57c377e98dd6f19949f4601e2c08ef505abbf5f"
PROTECTED_QUERY_SHA = "57c057ea1bd4e1420f3f93e948e790c06edea97e888d14249c83f01b56d9efe7"
PROTECTED_QUERY_LABEL = "Sep18-statistics002"
THIS_RUNNER_BASENAME = "acquire-prefetched-stock-plan-cycle72.py"
STATISTICS_ACQUISITION_RUNNERS = frozenset({
    "acquire-prefetched-stock-plan-cycle72.py",
    "acquire-prefetched-book-plan-current-finalizer.py",
    "acquire-selective-statistics-plan.py",
    "acquire-selective-statistics-plan-large-manifest.py",
    "acquire-selective-statistics-plan-reserved-safe.py",
    "acquire-prefetched-statistics-plan.py",
    "acquire-prefetched-statistics-plan-large-manifest.py",
    "acquire-prefetched-statistics-plan-reserved-safe.py",
    "acquire-prefetched-book-plan.py",
    "acquire-prefetched-book-plan-reserved-safe.py",
    "acquire-statistics-disjoint-overlay.py",
})
PREPAID_RECOVERY_RUNNERS = frozenset({
    "recover-prepaid-books-cycle66.py",
    "recover-prepaid-books-cycle70.py",
    "recover-prepaid-books-cycle52.py",
    "recover-prepaid-statistics-run.py",
    "recover-prepaid-statistics-run-v2.py",
})
RESEARCH_JOURNAL_CLIS = frozenset({
    "cycle26-autowheel-batch-finalize-20261006",
    "cycle52-autowheel-batch-finalize-20261007",
    "autowheel-batch",
    "autowheel-batch-retrieve",
    "autowheel-batch-finalize",
    "autowheel-batch-files",
    "autowheel-batch-planned-retrieve",
})
EXTERNAL_DRIVE = Path("/media/yuduo/Lexar1")
BOOK_BASE = Path("/home/yuduo/Projects/AutoWheel-research")
FILE_TOOLS = {
    "file_plan": ("cycle50-book-tools-20261007/autowheel-batch-files", "14ef873577dd79e529221314accde999eb1c9101dfc025536fd807fbe5286a39"),
    "planned_retrieve": ("cycle50-book-tools-20261007/autowheel-batch-planned-retrieve", "e84bb722b3b501d487da520ce692f43aa6b373781d22935949a7adb7d01a0e0f"),
}
STOCK_UNIVERSE = ["NFLX", "TSLA", "NVDA", "NBIS", "IREN", "NOW", "AMD", "AMZN",
                  "GOOGL", "META", "MSFT", "PLTR", "UBER", "AVGO", "ORCL"]
BOOK_CODE = ["cmd/autowheel-batch/main.go", "cmd/autowheel-batch-retrieve/main.go",
             "cmd/autowheel-batch-finalize/main.go", "internal/databento/batch.go",
             "internal/databento/batch_retrieve.go", "internal/databento/batch_finalize.go",
             "internal/databento/ledger.go", "internal/databento/native_book_completeness.py",
             "scripts/batch_acquisition_proof.py", "scripts/archive-research-source.py",
             "scripts/restore-research-source.py", "scripts/release-research-backup.py",
             "internal/databento/batch_files.go", "cmd/autowheel-batch-files/main.go",
             "internal/databento/batch_planned_retrieve.go", "cmd/autowheel-batch-planned-retrieve/main.go"]
NAME_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,254}\Z")
OPT_STATS_RE = re.compile(r"2026-\d{2}-\d{2}-statistics-\d{3}\.json\Z")
OPT_BOOK_RE = re.compile(r"2026-\d{2}-\d{2}-cmbp-1-\d{3}\.json\Z")
STOCK_RE = re.compile(r"2026-\d{2}-\d{2}-stock-mbp-1\.json\Z")


def load_old_controller():
    path = Path(__file__).with_name("acquire-prefetched-statistics-plan-large-manifest.py")
    if path.is_symlink() or hashlib.sha256(path.read_bytes()).hexdigest() != OLD_CONTROLLER_SHA:
        raise ValueError("pinned book predecessor controller differs")
    spec = importlib.util.spec_from_file_location("book_old_controller", path)
    module = importlib.util.module_from_spec(spec)
    old_path = sys.path[:]
    try:
        sys.path.insert(0, str(path.resolve().parent))
        spec.loader.exec_module(module)
    finally:
        sys.path[:] = old_path
    return module


def predecessor_live(pid=PREDECESSOR_PID):
    return Path(f"/proc/{pid}").exists()


def require_predecessor_absent(pid=PREDECESSOR_PID):
    if predecessor_live(pid):
        raise ValueError("authoritative statistics predecessor still running; no book send")


def free_bytes(path):
    fs = os.statvfs(path)
    return fs.f_bavail * fs.f_frsize


def require_submit_capacity(base, external=EXTERNAL_DRIVE):
    if free_bytes(base) < RESERVE_BYTES or free_bytes(external) < RESERVE_BYTES:
        raise ValueError("research capacity reserve unavailable")


def require_download_fit(free_before_root, free_before_ext, total_bytes):
    if type(total_bytes) is not int or not 1 <= total_bytes <= 64 << 30:
        raise ValueError("downloaded job bound differs")
    if total_bytes + RESERVE_BYTES > free_before_root or total_bytes + RESERVE_BYTES > free_before_ext:
        raise ValueError("archive capacity reserve unavailable")


def staging_name(run_ns, stem):
    name = run_ns + "-retrieve-" + stem
    if not run_ns.startswith("cycle") or "-retrieve-" not in name or NAME_RE.fullmatch(name) is None:
        raise ValueError("book staging namespace invalid")
    return name


def _clean_absolute(path):
    if not path.is_absolute() or path != Path(os.path.normpath(path)):
        raise ValueError("absolute clean paths required")


def collect_book_operations(preflight, option_manifest_path, stock_manifest_path):
    option_raw = preflight.private(option_manifest_path)
    if hashlib.sha256(option_raw).hexdigest() != OPTION_MANIFEST_SHA:
        raise ValueError("immutable option acquisition plan differs")
    option_manifest = preflight.read_json(option_manifest_path)
    queries = option_manifest.get("queries", [])
    if (len(queries) != 1086 or option_manifest.get("daily_partitions") != 126
            or option_manifest.get("acquisition_symbol_days") != 955003):
        raise ValueError("frozen full-study option scope differs")
    counts = Counter(q.get("schema") for q in queries)
    if counts.get("cmbp-1") != 543 or counts.get("statistics") != 543:
        raise ValueError("frozen book/statistics split differs")
    dates = sorted({q.get("date") for q in queries})
    if len(dates) != 126 or dates[0] != "2026-04-06" or dates[-1] != "2026-10-02":
        raise ValueError("frozen session coverage differs")
    stock_raw = preflight.private(stock_manifest_path)
    if hashlib.sha256(stock_raw).hexdigest() != STOCK_MANIFEST_SHA:
        raise ValueError("immutable stock acquisition plan differs")
    stock_manifest = preflight.read_json(stock_manifest_path)
    stock_queries = stock_manifest.get("queries", [])
    if len(stock_queries) != 126 or stock_manifest.get("daily_partitions") != 126:
        raise ValueError("frozen stock scope differs")
    if list(stock_manifest.get("universe", [])) != STOCK_UNIVERSE:
        raise ValueError("frozen stock universe differs")
    stock_dates = sorted({q.get("date") for q in stock_queries})
    if stock_dates != dates:
        raise ValueError("stock session dates must match option session dates exactly")
    seen_ops = set()
    stats_ops = {}
    book_items = []
    for item in queries:
        schema = item.get("schema")
        filename, day = item.get("filename"), item.get("date")
        if schema == "statistics":
            if OPT_STATS_RE.fullmatch(filename) is None:
                raise ValueError("query basename invalid")
        elif schema == "cmbp-1":
            if OPT_BOOK_RE.fullmatch(filename) is None:
                raise ValueError("query basename invalid")
        else:
            raise ValueError("option schema scope differs")
        if not isinstance(item.get("sha256"), str) or not isinstance(item.get("bytes"), int):
            raise ValueError("query source binding differs")
        path = option_manifest_path.parent / filename
        raw = preflight.private(path)
        if len(raw) != item["bytes"] or hashlib.sha256(raw).hexdigest() != item["sha256"]:
            raise ValueError("query source binding differs")
        query = preflight.read_json(path)
        nxt = (_date.fromisoformat(day) + timedelta(days=1)).isoformat()
        if (query.get("dataset") != "OPRA.PILLAR" or query.get("schema") != schema
                or query.get("stype_in") != "raw_symbol"
                or not isinstance(query.get("symbols"), list)
                or len(query["symbols"]) != item.get("symbols")
                or not 1 <= len(query["symbols"]) <= 2000
                or query.get("start") != day + "T00:00:00Z"
                or query.get("end") != nxt + "T00:00:00Z"):
            raise ValueError("book query scope differs")
        wire = preflight.canonical_query(query)
        op = preflight.digest(wire + b"\0/batch.submit_job")
        if op in seen_ops:
            raise ValueError("duplicate planned canonical operation")
        seen_ops.add(op)
        qh = preflight.digest(wire)
        if schema == "statistics":
            stats_ops[op] = (item, path, qh)
        else:
            book_items.append(("cmbp-1", item, path, op, qh))
    for item in stock_queries:
        filename, day = item.get("filename"), item.get("date")
        if STOCK_RE.fullmatch(filename) is None or item.get("symbols") != 15:
            raise ValueError("stock query basename invalid")
        path = stock_manifest_path.parent / filename
        raw = preflight.private(path)
        if len(raw) != item["bytes"] or hashlib.sha256(raw).hexdigest() != item["sha256"]:
            raise ValueError("stock query source binding differs")
        query = preflight.read_json(path)
        nxt = (_date.fromisoformat(day) + timedelta(days=1)).isoformat()
        if (query != {"dataset": "EQUS.MINI", "schema": "mbp-1", "symbols": STOCK_UNIVERSE,
                      "stype_in": "raw_symbol", "start": day + "T00:00:00Z",
                      "end": nxt + "T00:00:00Z"}):
            raise ValueError("stock query scope differs")
        wire = preflight.canonical_query(query)
        op = preflight.digest(wire + b"\0/batch.submit_job")
        if op in seen_ops:
            raise ValueError("duplicate planned canonical operation")
        seen_ops.add(op)
        book_items.append(("stock-mbp-1", item, path, op, preflight.digest(wire)))
    if len(book_items) != 669 or len(stats_ops) != 543:
        raise ValueError("full book scope differs")
    return book_items, stats_ops, dates


def acquire_book_lock(base):
    lock_path = base / LOCK_NAME
    fd = os.open(lock_path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC, 0o600)
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600
                or info.st_uid != os.getuid() or info.st_nlink != 1):
            raise ValueError("private single-owner book lock required")
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except Exception:
        os.close(fd)
        raise ValueError("book controller already holds the execution lock")
    return fd


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
    lock_path = base / RESERVED_SAFE_LOCK_NAME
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


def summarize_statistics_coverage(stats_ops, rows):
    """Count acquired/protected/fresh/other/wrong without enforcing completeness."""
    acquired = 0
    protected = []
    fresh = []
    other_reserved = []
    wrong = []
    for op, (item, _path, qh) in stats_ops.items():
        row = rows.get(op)
        if row is None:
            fresh.append(op)
            continue
        if row.get("endpoint") != BATCH_SUBMIT_ENDPOINT or row.get("query_hash") != qh:
            wrong.append(op)
            continue
        status = row.get("status")
        if status == "acquired":
            acquired += 1
        elif (status == "reserved" and op == PROTECTED_RESERVED_OPERATION
                and qh == PROTECTED_QUERY_SHA and row.get("query_hash") == PROTECTED_QUERY_SHA):
            protected.append({"operation_id": op, "query_hash": qh,
                              "filename": item.get("filename"), "date": item.get("date"),
                              "label": PROTECTED_QUERY_LABEL})
        elif status == "reserved":
            other_reserved.append(op)
        else:
            wrong.append(op)
    return acquired, protected, fresh, other_reserved, wrong


def check_statistics_coverage(stats_ops, rows):
    """Permit only 542 acquired + protected reserved, or all 543 acquired."""
    if len(stats_ops) != 543:
        raise ValueError("frozen statistics scope differs")
    entry = stats_ops.get(PROTECTED_RESERVED_OPERATION)
    if entry is None:
        raise ValueError("protected reserved operation not in frozen scope")
    _item, _path, expected_qh = entry
    if expected_qh != PROTECTED_QUERY_SHA:
        raise ValueError("protected reserved query hash differs")
    acquired, protected, fresh, other_reserved, wrong = summarize_statistics_coverage(stats_ops, rows)
    if fresh or wrong or other_reserved:
        raise ValueError("statistics coverage blocked; unresolved or unexpected status")
    if acquired == 543 and len(protected) == 0:
        return acquired, None
    if acquired == 542 and len(protected) == 1:
        return acquired, protected[0]
    raise ValueError("statistics coverage incomplete; 542+1 or 543 acquired required")


def private_directory(path):
    info = path.lstat()
    if (path.resolve() != path or not stat.S_ISDIR(info.st_mode)
            or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700):
        raise ValueError("private owned directory required")


def ledger_snapshot(preflight, path):
    raw = preflight.private(path)
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError("duplicate financial field")
            result[key] = value
        return result
    value = json.loads(raw, object_pairs_hook=unique,
                       parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite financial field")))
    if (value.get("version") != 2 or value.get("authorization_mode") != "uncapped"
            or value.get("lifetime_nano") != 0
            or len({r["operation_id"] for r in value["reservations"]}) != len(value["reservations"])):
        raise ValueError("uncapped unique-operation financial journal required")
    return raw, value


def observe_file_plan(preflight, binary, common, out, label):
    for attempt in range(3):
        phase = label + "-read-" + str(attempt)
        observation = out / phase
        if preflight.run(binary, [*common, "--output", observation, "--timeout", "1m"], out, phase) == 0:
            return observation, preflight.read_json(observation / "file-plan-receipt.json")
        error = preflight.private(out / (phase + ".stderr.private.log"))
        if error.strip() != b"databento batch observation failed" or attempt == 2:
            raise ValueError("file plan observation stopped; evidence and hold preserved")
        time.sleep(10 * (attempt + 1))


def file_plan_total(plan, operation, query_hash):
    observation = plan.get("observation", {})
    if (plan.get("schema_version") != "AutoWheelBatchFilePlanV1"
            or plan.get("ready") is not True or plan.get("metadata_only") is not True
            or plan.get("source_qualified") is not False or observation.get("all_verified") is not False
            or observation.get("state") != "done" or observation.get("operation_id") != operation
            or observation.get("query_hash") != query_hash or not isinstance(plan.get("files"), list)
            or not plan["files"] or not re.fullmatch("[0-9a-f]{64}", plan.get("file_list_sha256", ""))):
        raise ValueError("file plan identity or metadata boundary differs")
    total = plan.get("total_bytes")
    if type(total) is not int or total != sum(f["bytes"] for f in plan["files"]):
        raise ValueError("file plan exact total differs")
    return total


def same_download_plan(plan, receipt):
    def identities(rows):
        values = {f["filename"]: (f["bytes"], f["sha256"]) for f in rows}
        if len(values) != len(rows):
            raise ValueError("duplicate planned file")
        return values
    if (receipt.get("state") != "done" or receipt.get("all_verified") is not True
            or receipt.get("total_bytes") != plan["total_bytes"]
            or identities(receipt.get("files", [])) != identities(plan["files"])):
        raise ValueError("download differs from the inspected file plan")


def select_stock_operations(book_items, dates):
    """Filter only after the complete immutable 669-operation plan is checked."""
    if (len(book_items) != 669 or len(dates) != 126
            or Counter(entry[0] for entry in book_items) != {"cmbp-1": 543, "stock-mbp-1": 126}):
        raise ValueError("complete frozen parent scope required before stock selection")
    selected = [entry for entry in book_items if entry[0] == "stock-mbp-1"]
    if (sorted(entry[1]["date"] for entry in selected) != list(dates)
            or len({entry[3] for entry in selected}) != 126):
        raise ValueError("stock selection must retain every frozen session exactly once")
    return selected


def require_prepaid_recovery_complete(preflight, book_items, current):
    """Require the original twelve jobs' terminal, native and archive evidence."""
    run = BOOK_BASE / "cycle70-prepaid-book-recovery-20261008"
    supervisor = preflight.read_json(BOOK_BASE / "cycle70-prepaid-book-recovery-supervisor-20261008/runtime.json")
    if (supervisor.get("finished") is not True or type(supervisor.get("exit_code")) is not int
            or supervisor["exit_code"] != 0):
        raise ValueError("original prepaid recovery must finish successfully first")
    summary = preflight.read_json(run / "summary.json")
    initial = preflight.read_json(run / "ledger-before.private.json")
    completed = summary.get("completed", [])
    if (summary.get("complete") is not True or summary.get("execute") is not True
            or summary.get("runner_sha256") != "ae1cf586cbd288e8e387d7bdb3373af37f56f4900be27df4d1650c0543c9aa0a"
            or summary.get("acknowledged_reserved_jobs") != 12 or len(completed) != 12
            or len(initial["reservations"]) != 590
            or current["reservations"][:575] != initial["reservations"][:575]):
        raise ValueError("original twelve-job recovery binding differs")
    reserved = {r["operation_id"] for r in initial["reservations"][575:] if r["status"] == "reserved"}
    planned = {entry[1]["filename"]: entry for entry in book_items if entry[3] in reserved}
    if (len(reserved) != 12 or len(planned) != 12
            or {r["query"] for r in completed} != set(planned)):
        raise ValueError("original prepaid completion scope differs")
    rows = {r["operation_id"]: r for r in current["reservations"]}
    mounts = [line.split() for line in Path("/proc/mounts").read_text().splitlines()]
    for result in completed:
        kind, item, query, operation, query_hash = planned[result["query"]]
        out = run / item["filename"].removesuffix(".json")
        proof_raw = preflight.private(out / "proof.json")
        proof = preflight.read_json(out / "proof.json")
        row = rows[operation]
        if (preflight.digest(proof_raw) != result["proof_sha256"]
                or proof["query_file_sha256"] != item["sha256"]
                or proof["native"]["all_eof_verified"] is not True
                or proof["native"]["records"] != proof["provider_record_count"]
                or proof["provider_record_count"] != result["records"]
                or proof["source_qualified"] is not False
                or row["status"] != "acquired" or row["endpoint"] != BATCH_SUBMIT_ENDPOINT
                or row["query_hash"] != query_hash or row["data_sha256"] != result["proof_sha256"]):
            raise ValueError("original prepaid native or journal proof differs")
        archive = preflight.read_json(out / "archive.json")
        restore = preflight.read_json(out / "restore.json")
        release = preflight.read_json(out / "release.json")
        stage = Path(proof["source_directory"])
        mount = next((m for m in mounts if m[1] == str(stage)), None)
        if (archive["original_paths_and_bytes_preserved"] is not True or archive["read_only"] is not True
                or restore["archive_only"] is not True or restore["journal_changed"] is not False
                or release["files_released"] != restore["files_verified"]
                or release["files_released"] != len(archive["files"])
                or release["bytes_released"] != restore["bytes_verified"]
                or release["bytes_released"] != archive["total_bytes"]
                or mount is None or mount[0] != archive["physical_archive_directory"]
                or not {"ro", "nosuid", "nodev", "noexec", "default_permissions"}.issubset(set(mount[3].split(",")))):
            raise ValueError("original prepaid archive recovery proof differs")
        for member in proof["files"]:
            archived = archive["files"][member["filename"]]
            source = stage / member["filename"]
            info = source.lstat()
            if (source.resolve() != source or not stat.S_ISREG(info.st_mode)
                    or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o600
                    or info.st_size != member["bytes"] or archived["bytes"] != member["bytes"]
                    or archived["sha256"] != member["sha256"]):
                raise ValueError("retained original prepaid source identity differs")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("base", "source-root", "option-manifest", "stock-manifest",
                 "ledger", "key-file", "python", "binder", "output"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--max-pending", type=int, default=12)
    parser.add_argument("--execute", action="store_true")
    args = parser.parse_args()
    if not 1 <= args.max_pending <= 12:
        raise ValueError("bounded pending window required")
    for p in (args.base, args.source_root, args.option_manifest, args.stock_manifest,
              args.ledger, args.key_file, args.python, args.binder, args.output):
        _clean_absolute(p)
    if args.execute:
        require_predecessor_absent()
    if args.base != BOOK_BASE or args.source_root != args.base / "source-afa3075":
        raise ValueError("research base directories required")
    private_directory(args.base)
    private_directory(args.source_root)
    if (args.output.parent != args.base or not args.output.name.startswith("cycle")
            or NAME_RE.fullmatch(args.output.name) is None or args.output.exists()
            or args.output.resolve() != args.output):
        raise ValueError("new cycle output namespace within base required")
    if args.binder != args.base / BINDER_RELATIVE or args.binder.is_symlink() or not args.binder.is_file():
        raise ValueError("pinned binder path differs")
    if hashlib.sha256(args.binder.read_bytes()).hexdigest() != BINDER_SHA256:
        raise ValueError("unqualified binder")
    binder_info = args.binder.lstat()
    if (args.binder.resolve() != args.binder or binder_info.st_uid != os.getuid()
            or binder_info.st_nlink != 1 or stat.S_IMODE(binder_info.st_mode) != 0o755):
        raise ValueError("private owned binder required")
    if (args.ledger != Path("/home/yuduo/.local/share/autowheel-research/databento-budget.json")
            or args.key_file != Path("/home/yuduo/.config/autowheel/databento-pilot.key")
            or args.python != args.base / "dbn-env-0.71.0/bin/python"):
        raise ValueError("fixed research binding required")
    # Reserved-safe fork: execute mode holds both the statistics reserved-safe
    # lock and the book lock from before any journal read/outputs through the
    # entire run. Dry-run stays lock-free and non-financial. No env bypass;
    # stale old-PID absence alone is insufficient.
    reserved_safe_fd = None
    book_fd = None
    if args.execute:
        # Exact process-basename guard before any outputs; fail closed if Linux
        # process inventory unavailable/unreadable. Skip self.
        live_writers_absent()
        reserved_safe_fd = acquire_reserved_safe_lock(args.base)
        try:
            book_fd = acquire_book_lock(args.base)
        except Exception:
            try:
                os.close(reserved_safe_fd)
            except OSError:
                pass
            reserved_safe_fd = None
            raise
        # Recheck under both locks before journal read/outputs. No bypass via
        # env, stale old-PID absence, or modes.
        live_writers_absent()
    old = load_old_controller()
    preflight = old.load_preflight()
    original_ledger_raw, before = ledger_snapshot(preflight, args.ledger)
    source_manifest = {v["path"]: v["sha256"] for v in
                       preflight.read_qualification(
                           args.source_root / "research/simulation/2026-10-05/databento-qualification.json")["source_files"]}
    for name in BOOK_CODE:
        if preflight.digest((args.source_root / name).read_bytes()) != source_manifest.get(name):
            raise ValueError("accepted acquisition source differs")
    archive_path = args.source_root / "scripts/archive-research-source.py"
    archive_spec = importlib.util.spec_from_file_location("book_archive_preflight", archive_path)
    archive = importlib.util.module_from_spec(archive_spec)
    archive_spec.loader.exec_module(archive)
    archive.directory(EXTERNAL_DRIVE)
    archive.verify_backing_mount()
    binaries = {}
    for name, (relative, expected) in preflight.TOOLS.items():
        path = args.base / relative
        if path.is_symlink() or not path.is_file() or preflight.digest(path.read_bytes()) != expected:
            raise ValueError("accepted research binary differs")
        binaries[name] = path
    if set(FILE_TOOLS) != {"file_plan", "planned_retrieve"}:
        raise ValueError("validated file-planning tools required")
    for name, (relative, expected) in FILE_TOOLS.items():
        path = args.base / relative
        info = path.lstat()
        if (path.resolve() != path or not stat.S_ISREG(info.st_mode)
                or info.st_uid != os.getuid() or info.st_nlink != 1
                or stat.S_IMODE(info.st_mode) != 0o755 or preflight.digest(path.read_bytes()) != expected):
            raise ValueError("accepted file-planning binary differs")
        binaries[name] = path
    # The old statistics preflight pins a legacy finalizer. Keep its inputs
    # immutable, but replace the executed book finalizer with this verified
    # artifact containing the current provider-count-bounded book verifier.
    finalizer = args.base / "cycle52-autowheel-batch-finalize-20261007"
    finalizer_raw = finalizer.read_bytes()
    finalizer_info = finalizer.lstat()
    if (finalizer.resolve() != finalizer or not stat.S_ISREG(finalizer_info.st_mode)
            or finalizer_info.st_uid != os.getuid() or finalizer_info.st_nlink != 1
            or stat.S_IMODE(finalizer_info.st_mode) != 0o755
            or preflight.digest(finalizer_raw) != "119eecab3d887a4fb3d99b03546e3adb8303a934f8828bc7ddbea4c0a9349736"
            or (args.source_root / "internal/databento/native_book_completeness.py").read_bytes() not in finalizer_raw):
        raise ValueError("current bounded book finalizer differs")
    binaries["finalize"] = finalizer
    book_items, stats_ops, _dates = collect_book_operations(preflight, args.option_manifest, args.stock_manifest)
    if args.execute:
        require_prepaid_recovery_complete(preflight, book_items, before)
    book_items = select_stock_operations(book_items, _dates)
    rows = {r["operation_id"]: r for r in before["reservations"]}
    todo, skipped, seen = [], 0, set()
    for kind, item, path, op, qh in book_items:
        if op in seen:
            raise ValueError("duplicate planned canonical operation")
        seen.add(op)
        if op in rows:
            if (rows[op]["status"] != "acquired" or rows[op]["endpoint"] != BATCH_SUBMIT_ENDPOINT
                    or rows[op]["query_hash"] != qh):
                raise ValueError("selected query has a prior financial outcome; no retry")
            skipped += 1
        else:
            todo.append((kind, item, path, op, qh))
    todo.sort(key=lambda entry: (entry[1]["date"], entry[0] != "stock-mbp-1", entry[1]["filename"]))
    run_ns = args.output.name
    for _, item, _, _, _ in todo:
        source = args.base / staging_name(run_ns, item["filename"].removesuffix(".json"))
        for path in (source, source.with_name(source.name + ".file-plan"),
                     source.with_name(source.name + ".preserved-cycle30"),
                     EXTERNAL_DRIVE / "AutoWheel-research-private-20261006" / source.name):
            if path.exists() or path.is_symlink():
                raise ValueError("fresh staging and archive namespace required")
    # Statistics coverage for reporting; strict enforcement only in execute.
    # Dry-run reports the current snapshot without failing while the
    # predecessor can advance. Execute requires exactly 542 acquired plus the
    # explicitly protected reserved op, or all 543 acquired, with every row
    # endpoint and exact canonical query hash revalidated.
    if args.execute:
        stats_acquired, stats_protected_entry = check_statistics_coverage(stats_ops, rows)
        stats_protected_count = 1 if stats_protected_entry is not None else 0
        stats_coverage = "543-acquired" if stats_protected_entry is None else "542+1-reserved"
    else:
        _acq, _prot_list, _fresh, _other, _wrong = summarize_statistics_coverage(stats_ops, rows)
        stats_acquired = _acq
        stats_protected_count = len(_prot_list)
        stats_protected_entry = _prot_list[0] if len(_prot_list) == 1 else None
        if _acq == 543 and stats_protected_count == 0 and not _fresh and not _other and not _wrong:
            stats_coverage = "543-acquired"
        elif _acq == 542 and stats_protected_count == 1 and not _fresh and not _other and not _wrong:
            stats_coverage = "542+1-reserved"
        else:
            stats_coverage = "incomplete-planning-snapshot"
    args.output.mkdir(mode=0o700)
    preflight.save(args.output / "ledger-before.private.json", before)
    plan = {"schema": "AutoWheelPrefetchedStockAcquisitionRunV1",
            "option_manifest_sha256": OPTION_MANIFEST_SHA, "stock_manifest_sha256": STOCK_MANIFEST_SHA,
            "full_book_operations": 669, "selected_stock_operations": 126,
            "excluded_option_book_operations": 543, "acquisition_scope": "EQUS.MINI/mbp-1 only",
            "full_frozen_option_book_coverage": False, "required_queries": len(todo),
            "already_acquired_skipped": skipped, "statistics_operations": 543,
            "statistics_acquired": stats_acquired,
            "statistics_protected_reserved": stats_protected_count,
            "statistics_coverage": stats_coverage,
            "protected_reserved_operation": PROTECTED_RESERVED_OPERATION,
            "protected_reserved_query_hash": PROTECTED_QUERY_SHA,
            "protected_reserved_label": PROTECTED_QUERY_LABEL,
            "protected_reserved_entry": stats_protected_entry,
            "reserved_safe_lock": RESERVED_SAFE_LOCK_NAME,
            "book_lock": LOCK_NAME,
            "execute": args.execute, "max_pending": args.max_pending,
            "uncapped": True, "initial_journal_sha256": preflight.digest(original_ledger_raw),
            "financial_retry": False, "source_qualified": False,
            "historical_performance_available": False}
    preflight.save(args.output / "plan.json", plan)
    runtime = {**plan, "schema": "AutoWheelPrefetchedStockAcquisitionRunV1",
               "minimum_submit_spacing_seconds": 4, "poll_spacing_seconds": 20,
               "pending_job_deadline_seconds": 21600, "financial_actions_serial": True,
               "predecessor_pid": PREDECESSOR_PID,
               "original_preflight_sha256": preflight.digest(
                   (Path(__file__).with_name("acquire-selective-statistics-plan-large-manifest.py")).read_bytes()),
               "original_controller_sha256": OLD_CONTROLLER_SHA,
               "binder_sha256": BINDER_SHA256,
               "runner_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest()}
    preflight.save(args.output / "runtime-plan.json", runtime)
    if not args.execute:
        # The predecessor can advance during this read-only planning pass.
        runtime["journal_snapshot_only"] = True
        runtime["credentials_read"] = False
        runtime["financial_requests"] = 0
        print(json.dumps(runtime), flush=True)
        return
    require_predecessor_absent()
    # Recheck writers under both locks before the financial gate; fail closed.
    # Legacy runners do not hold these locks, so PID absence plus the exact
    # basename guard remain mandatory; locks alone do not guard them.
    live_writers_absent()
    if ledger_snapshot(preflight, args.ledger)[1] != before:
        raise ValueError("unexpected financial journal mutation before execution")
    _gate_acquired, _gate_protected = check_statistics_coverage(stats_ops, rows)
    if (_gate_acquired != stats_acquired
            or (_gate_protected or None) != (stats_protected_entry or None)):
        raise ValueError("statistics coverage changed before execution")
    # Both locks already held from before journal read/outputs; kept through loop.
    if reserved_safe_fd is None or book_fd is None:
        raise ValueError("reserved-safe execution locks required")
    expected = before
    initial_count = len(expected["reservations"])
    active, completed, submitted = [], [], 0
    last_submit = float("-inf")
    phase, item = "initial", {"filename": None}
    scripts = {n: args.source_root / ("scripts/" + n) for n in
               ("archive-research-source.py", "restore-research-source.py", "release-research-backup.py")}

    def check_journal():
        live_writers_absent()
        if ledger_snapshot(preflight, args.ledger)[1] != expected:
            raise ValueError("unexpected financial journal mutation")

    try:
        while submitted < len(todo) or active:
            while submitted < len(todo) and len(active) < args.max_pending:
                kind, item, query, operation, query_hash = todo[submitted]
                phase = "pre-submit"
                time.sleep(max(0, 4 - (time.monotonic() - last_submit)))
                check_journal()
                require_submit_capacity(args.base, EXTERNAL_DRIVE)
                stem = item["filename"].removesuffix(".json")
                out = args.output / stem
                out.mkdir(mode=0o700)
                staging = args.base / staging_name(run_ns, stem)
                if staging.exists():
                    raise ValueError("fresh flat staging source required")
                preflight.save(out / "intent.json", {"query_file_sha256": item["sha256"],
                    "operation_id": operation, "kind": kind, "financial_retry": False})
                phase = "submit"
                last_submit = time.monotonic()
                submit = out / "submit"
                if preflight.run(binaries["batch"], ["--query", query, "--output", submit,
                        "--key-file", args.key_file, "--submit"], out, phase) != 0:
                    raise ValueError("financial attempt stopped; evidence and hold preserved")
                acknowledgement = preflight.read_json(submit / "ack.json")
                _, current = ledger_snapshot(preflight, args.ledger)
                quote_hash = preflight.read_json(submit / "quote.json")["quote"]["ResponseHash"]
                old.appended(expected, current, operation, query_hash, acknowledgement,
                             quote_hash, preflight.private(submit / "job.json"))
                expected = current
                active.append({"kind": kind, "item": item, "query": query,
                               "operation": operation, "out": out, "staging": staging,
                               "submitted": time.monotonic(), "last_poll": float("-inf"), "poll": 0})
                submitted += 1
                print(json.dumps({"submitted_queries": submitted, "required_queries": len(todo),
                    "pending_queries": len(active), "journal_entries": len(expected["reservations"])}), flush=True)
            for job in tuple(active):
                kind, item, query, operation, out, staging = (
                    job[k] for k in ("kind", "item", "query", "operation", "out", "staging"))
                phase = "pre-observe"
                now = time.monotonic()
                if now - job["submitted"] > 21600:
                    raise ValueError("prepaid job deadline; inspect existing job, never resubmit")
                if now - job["last_poll"] < 20:
                    continue
                check_journal()
                common = ["--query", query, "--ack", out / "submit/ack.json",
                          "--ack-body", out / "submit/job.json", "--key-file", args.key_file]
                phase = "observe-" + str(job["poll"])
                receipt = old.observe_readonly(preflight, binaries["retrieve"], common, out, phase)
                check_journal()
                job["poll"] += 1
                job["last_poll"] = time.monotonic()
                if receipt["state"] in ("queued", "processing"):
                    continue
                if receipt["state"] != "done":
                    raise ValueError("unknown prepaid job state")
                phase = "file-plan-" + str(job["poll"])
                plan_dir, file_plan = observe_file_plan(preflight, binaries["file_plan"], common, out, phase)
                check_journal()
                if file_plan.get("ready") is False and file_plan.get("observation", {}).get("state") in ("queued", "processing"):
                    continue
                total = file_plan_total(file_plan, operation, preflight.digest(preflight.canonical_query(preflight.read_json(query))))
                require_download_fit(free_bytes(args.base), free_bytes(EXTERNAL_DRIVE), total)
                if preflight.digest(preflight.private(plan_dir / "files-source.json")) != file_plan["file_list_sha256"]:
                    raise ValueError("file plan original response binding differs")
                phase = "download"
                if preflight.run(binaries["planned_retrieve"], [*common, "--output", staging,
                        "--timeout", "30m", "--planned-files", plan_dir / "files-source.json"], out, phase) != 0:
                    raise ValueError("download stopped; private evidence retained")
                check_journal()
                final_receipt = preflight.read_json(staging / "receipt.json")
                same_download_plan(file_plan, final_receipt)
                phase = "finalize"
                proof_path = out / "proof.json"
                if preflight.run(binaries["finalize"], ["--query", query, "--ack", out / "submit/ack.json",
                        "--ack-body", out / "submit/job.json", "--source", staging, "--proof", proof_path,
                        "--python", args.python, "--ledger", args.ledger, "--timeout", "30m"], out, phase) != 0:
                    raise ValueError("offline acquisition verification stopped; hold preserved")
                proof = preflight.read_json(proof_path)
                proof_sha = preflight.digest(preflight.private(proof_path))
                if (proof["query_file_sha256"] != item["sha256"]
                        or proof["native"]["all_eof_verified"] is not True
                        or proof["native"]["records"] != proof["provider_record_count"]
                        or proof["source_qualified"] is not False):
                    raise ValueError("native proof binding differs")
                _, current = ledger_snapshot(preflight, args.ledger)
                old.finalized(expected, current, operation, proof_sha, proof["total_bytes"])
                expected = current
                phase = "archive"
                if preflight.run(args.python, [scripts["archive-research-source.py"], "--source", staging,
                        "--binder", args.binder, "--output", out / "archive.json"], out, phase) != 0:
                    raise ValueError("research source archive stopped; evidence preserved")
                check_journal()
                archive_intent = out / "archive.json.intent.json"
                if preflight.digest(preflight.private(proof_path)) != proof_sha:
                    raise ValueError("source proof binding changed")
                if (proof["source_directory"] != str(staging)
                        or preflight.digest(preflight.private(staging / "receipt.json")) != proof["receipt_sha256"]):
                    raise ValueError("archived original source receipt binding differs")
                phase = "restore-verify"
                if preflight.run(args.python, [scripts["restore-research-source.py"], "--intent", archive_intent,
                        "--binder", args.binder, "--output", out / "restore.json", "--archive-only"],
                        out, phase) != 0:
                    raise ValueError("archive recovery verification stopped; evidence preserved")
                check_journal()
                phase = "release"
                if preflight.run(args.python, [scripts["release-research-backup.py"], "--intent", archive_intent,
                        "--archive-report", out / "archive.json", "--output", out / "release.json"],
                        out, phase) != 0:
                    raise ValueError("redundant backup release stopped; evidence preserved")
                check_journal()
                if preflight.digest(preflight.private(proof_path)) != proof_sha:
                    raise ValueError("source proof binding changed")
                row = next(r for r in expected["reservations"] if r["operation_id"] == operation)
                result = {"query": item["filename"], "date": item["date"], "kind": kind,
                          "proof_sha256": proof_sha, "records": proof["native"]["records"],
                          "compressed_bytes": proof["total_bytes"], "quoted_cost_nano": row["cost_nano"],
                          "all_eof_verified": True, "journal_entries": len(expected["reservations"]),
                          "original_journal_prefix_unchanged": True, "source_qualified": False,
                          "historical_performance_available": False}
                preflight.save(out / "aggregate.json", result)
                completed.append(result)
                active.remove(job)
                print(json.dumps({"completed_queries": len(completed), "required_queries": len(todo),
                    "pending_queries": len(active), **result}), flush=True)
            if active:
                time.sleep(1)
        check_journal()
        final_raw, final = ledger_snapshot(preflight, args.ledger)
        preflight.save(args.output / "ledger-after.private.json", final)
        preflight.save(args.output / "summary.json", {**runtime, "completed_queries": len(completed),
            "complete_acquisition_run": True, "complete_stock_acquisition_run": True, "full_book_operations": 669,
            "records": sum(v["records"] for v in completed),
            "compressed_bytes": sum(v["compressed_bytes"] for v in completed),
            "quoted_cost_nano": sum(v["quoted_cost_nano"] for v in completed),
            "journal_sha256": preflight.digest(final_raw),
            "journal_status_counts": dict(Counter(v["status"] for v in final["reservations"])),
            "original_journal_prefix_unchanged": final["reservations"][:initial_count]
                == preflight.read_json(args.output / "ledger-before.private.json")["reservations"],
            "completed": completed, "source_qualified": False,
            "historical_performance_available": False,
            "observed_at": datetime.now(timezone.utc).isoformat()})
        print(json.dumps({"complete_acquisition_run": True, "completed_queries": len(completed),
            "journal_entries": len(final["reservations"])}), flush=True)
    except Exception:
        try:
            preflight.save(args.output / "interruption.json", {"phase": phase, "query": item.get("filename"),
                "pending_queries": len(active), "submitted_queries": submitted,
                "completed_queries": len(completed), "financial_retry": False,
                "holds_released": False, "inspection_required": True,
                "statistics_acquired": stats_acquired,
                "statistics_protected_reserved": stats_protected_count,
                "statistics_coverage": stats_coverage,
                "protected_reserved_operation": PROTECTED_RESERVED_OPERATION,
                "source_qualified": False, "historical_performance_available": False})
        except Exception:
            pass
        print(json.dumps({"stopped": True, "phase": phase, "completed_queries": len(completed),
            "financial_retry": False, "holds_released": False}), flush=True)
        raise
    finally:
        for _fd in (book_fd, reserved_safe_fd):
            try:
                if _fd is not None:
                    os.close(_fd)
            except Exception:
                pass


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(json.dumps({"status": "stopped", "error_type": type(error).__name__,
            "inspect_private_evidence": True}), flush=True)
        sys.exit(1)
