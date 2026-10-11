#!/usr/bin/env python3
"""Acquire two disjoint statistics children covering one missing frozen partition.

Research-only controller for the Sep18 2000-symbol reserved partition. The
original unknown financial row is preserved forever untouched: no parent
retry, release, or synthetic ACK. Two genuine disjoint 1000-symbol children
with identical dataset/schema/stype_in/start/end give an explicit exact union
ordered parent[:1000] / parent[1000:]. Original 543-query manifest is never
modified. No Go changes, no provider HTTP in this file, no financial retry,
hold release, or source/replay qualification. All originals and CLI logs stay
in a new private MSI directory. Frozen 543 coverage is kept distinct from the
2 children; this run never claims parent acquired or full-study qualified.

Reuses pinned helpers from acquire-prefetched-statistics-plan-reserved-safe.py
(SHA c6a984b5f63c85f23fd2f25fdf359ab1efbd1519380c7209169ce62812b9719e) and its
load_preflight() (pins ea16e637810707eecf217ae2a60a9d72c0277a1a3f16e49007eec8b9eee197c9):
appended/finalized journal transitions, observe_readonly (<=3 fresh paths),
TOOLS/Binary pins, CODE/source pins, canonical_query/digest/ledger/private/
save/run/read_qualification and batch-proof validator pins. Never calls the
full stats preflight.main() which only plans the original manifest.
"""
import argparse
from collections import Counter
from datetime import datetime, timezone
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

RESERVED_SAFE_CONTROLLER_SHA = "c6a984b5f63c85f23fd2f25fdf359ab1efbd1519380c7209169ce62812b9719e"
RESERVED_SAFE_CONTROLLER_NAME = "acquire-prefetched-statistics-plan-reserved-safe.py"
BOOK_HELPER_NAME = "acquire-prefetched-book-plan.py"
BOOK_HELPER_SHA = "4529d7ed56bcaf4187af653c01567ac85c424e04892566ee605dbc058e8fd984"

ORIGINAL_MANIFEST_SHA = "ccf48c229ce29db62e0d4aaa20cb06c733fdf4e528cf2d79f4da0e71a00adb2b"
PARENT_FILE_SHA = "dd72d518f59fb800d0291b01920d159e09a41950f8523785f283093715ec70a2"
PARENT_QUERY_SHA = "57c057ea1bd4e1420f3f93e948e790c06edea97e888d14249c83f01b56d9efe7"
PARENT_OPERATION_ID = "01593d27c3a667a42b8736bba57c377e98dd6f19949f4601e2c08ef505abbf5f"
PARENT_FILENAME = "2026-09-18-statistics-002.json"
PARENT_DATASET = "OPRA.PILLAR"
PARENT_SCHEMA = "statistics"
PARENT_STYPE = "raw_symbol"
PARENT_START = "2026-09-18T00:00:00Z"
PARENT_END = "2026-09-19T00:00:00Z"
PARENT_SYMBOLS = 2000
CHILD_SYMBOLS_EACH = 1000

PLAN_SCHEMA = "AutoWheelDisjointStatisticsOverlayPlanV1"
PARTITION_METHOD = "ordered_symbol_halves_same_full_day"
PARENT_STATUS = "reserved_unknown_untouched"
CHILD_FILENAMES = ("2026-09-18-statistics-overlay-000.json", "2026-09-18-statistics-overlay-001.json")
CHILD_DATE = "2026-09-18"

EXPECTED_BASE = Path("/home/yuduo/Projects/AutoWheel-research")
ORIGINAL_MANIFEST_RELATIVE = Path("cycle24-selective-acquisition-queries-20261006/manifest.json")
OVERLAY_PLAN_RELATIVE = Path("cycle51-statistics-disjoint-overlay-plan-20261007/manifest.json")
EXPECTED_LEDGER = Path("/home/yuduo/.local/share/autowheel-research/databento-budget.json")
EXPECTED_KEY = Path("/home/yuduo/.config/autowheel/databento-pilot.key")
EXPECTED_PYTHON_RELATIVE = Path("dbn-env-0.71.0/bin/python")
QUALIFICATION_RELATIVE = Path("research/simulation/2026-10-05/databento-qualification.json")

RESERVED_SAFE_LOCK_NAME = ".statistics-reserved-safe.lock"
BOOK_LOCK_NAME = "cycle-book-controller.lock"
BATCH_SUBMIT_ENDPOINT = "/batch.submit_job"
RESERVE_BYTES = 6 << 30
# Active stats reserved execute observed on MSI: tool_session 9228, controller PID 4073081.
ACTIVE_STATS_CONTROLLER_PID = 4073081
ACTIVE_STATS_TOOL_SESSION = 9228

THIS_RUNNER_BASENAME = "acquire-statistics-disjoint-overlay.py"
STATISTICS_ACQUISITION_RUNNERS = frozenset({
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
NAME_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,254}\Z")


def load_reserved_controller():
    path = Path(__file__).with_name(RESERVED_SAFE_CONTROLLER_NAME)
    if path.is_symlink() or hashlib.sha256(path.read_bytes()).hexdigest() != RESERVED_SAFE_CONTROLLER_SHA:
        raise ValueError("pinned reserved-safe controller differs")
    spec = importlib.util.spec_from_file_location("overlay_reserved_controller", path)
    module = importlib.util.module_from_spec(spec)
    old_path = sys.path[:]
    try:
        sys.path.insert(0, str(path.resolve().parent))
        spec.loader.exec_module(module)
    finally:
        sys.path[:] = old_path
    return module


def load_book_helpers():
    path = Path(__file__).with_name(BOOK_HELPER_NAME)
    if path.is_symlink() or hashlib.sha256(path.read_bytes()).hexdigest() != BOOK_HELPER_SHA:
        raise ValueError("pinned file-planning helpers differ")
    spec = importlib.util.spec_from_file_location("overlay_book_helpers", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


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


def private_directory(path):
    info = path.lstat()
    if (not stat.S_ISDIR(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o700
            or info.st_uid != os.getuid() or path.resolve() != path):
        raise ValueError("private research base directory required")


def acquire_book_lock(base):
    private_directory(base)
    lock_path = base / BOOK_LOCK_NAME
    fd = os.open(lock_path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC, 0o600)
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600
                or info.st_uid != os.getuid() or info.st_nlink != 1):
            raise ValueError("private single-owner book lock required")
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        return fd
    except Exception:
        try:
            os.close(fd)
        except OSError:
            pass
        raise ValueError("book controller already holds the execution lock")


def require_no_active_stats_controller(pid=ACTIVE_STATS_CONTROLLER_PID):
    if Path(f"/proc/{pid}").exists():
        raise ValueError("active stats reserved controller still running; no overlay send")


def clean_absolute(path):
    if not path.is_absolute() or path != Path(os.path.normpath(path)):
        raise ValueError("absolute clean paths required")


def require_fixed_bindings(args):
    if args.base != EXPECTED_BASE:
        raise ValueError("fixed research base required")
    if args.source_root != args.base / "source-afa3075":
        raise ValueError("fixed source root required")
    if args.ledger != EXPECTED_LEDGER:
        raise ValueError("fixed research journal required")
    if args.key_file != EXPECTED_KEY:
        raise ValueError("fixed research key path required")
    if args.python != args.base / EXPECTED_PYTHON_RELATIVE:
        raise ValueError("fixed research python required")
    if args.plan != args.base / OVERLAY_PLAN_RELATIVE:
        raise ValueError("fixed overlay plan path required")
    private_directory(args.base)
    private_directory(args.source_root)
    if (args.output.parent != args.base or not args.output.name.startswith("cycle")
            or NAME_RE.fullmatch(args.output.name) is None or args.output.exists()
            or args.output.resolve() != args.output):
        raise ValueError("new cycle output namespace within base required")


def validate_original_manifest(preflight, manifest_path):
    raw = preflight.private(manifest_path)
    if hashlib.sha256(raw).hexdigest() != ORIGINAL_MANIFEST_SHA:
        raise ValueError("immutable acquisition plan differs")
    manifest = preflight.read_json(manifest_path)
    queries = manifest.get("queries", [])
    stats_items = [q for q in queries if q.get("schema") == "statistics"]
    book_items = [q for q in queries if q.get("schema") == "cmbp-1"]
    if len(stats_items) != 543 or len(book_items) != 543 or len(queries) != 1086:
        raise ValueError("frozen full-study scope differs")
    if manifest.get("daily_partitions") != 126:
        raise ValueError("frozen session coverage differs")
    dates = sorted({q.get("date") for q in queries})
    if len(dates) != 126 or dates[0] != "2026-04-06" or dates[-1] != "2026-10-02":
        raise ValueError("frozen session dates differ")
    stats_ops = {}
    seen = set()
    for item in stats_items:
        filename = item.get("filename")
        if not isinstance(filename, str) or re.fullmatch(r"2026-\d{2}-\d{2}-statistics-\d{3}\.json", filename) is None:
            raise ValueError("query basename invalid")
        if not isinstance(item.get("sha256"), str) or not isinstance(item.get("bytes"), int):
            raise ValueError("query source binding differs")
        if not isinstance(item.get("symbols"), int) or not isinstance(item.get("date"), str):
            raise ValueError("query source binding differs")
        path = manifest_path.parent / filename
        qraw = preflight.private(path)
        if len(qraw) != item["bytes"] or hashlib.sha256(qraw).hexdigest() != item["sha256"]:
            raise ValueError("query source binding differs")
        query = preflight.read_json(path)
        # No count/time inference: explicit start/end/dataset/schema/stype checks.
        if (query.get("dataset") != "OPRA.PILLAR" or query.get("schema") != "statistics"
                or query.get("stype_in") != "raw_symbol"
                or not isinstance(query.get("symbols"), list)
                or len(query["symbols"]) != item["symbols"]
                or query.get("start") != item["date"] + "T00:00:00Z"):
            raise ValueError("statistics query scope differs")
        wire = preflight.canonical_query(query)
        qh = preflight.digest(wire)
        op = preflight.digest(wire + b"\0/batch.submit_job")
        if op in seen:
            raise ValueError("duplicate planned canonical operation")
        seen.add(op)
        stats_ops[op] = (item, path, qh, query)
    # Light binding for book half: filenames/bytes/sha only, scope count already checked.
    for item in book_items:
        filename = item.get("filename")
        if not isinstance(filename, str) or re.fullmatch(r"2026-\d{2}-\d{2}-cmbp-1-\d{3}\.json", filename) is None:
            raise ValueError("query basename invalid")
        path = manifest_path.parent / filename
        qraw = preflight.private(path)
        if len(qraw) != item["bytes"] or hashlib.sha256(qraw).hexdigest() != item["sha256"]:
            raise ValueError("query source binding differs")
    return stats_ops


def validate_parent(preflight, parent_path):
    raw = preflight.private(parent_path)
    if hashlib.sha256(raw).hexdigest() != PARENT_FILE_SHA:
        raise ValueError("parent query file binding differs")
    query = preflight.read_json(parent_path)
    if set(query) != {"dataset", "schema", "symbols", "stype_in", "start", "end"}:
        raise ValueError("parent query keys differ")
    if (query["dataset"] != PARENT_DATASET or query["schema"] != PARENT_SCHEMA
            or query["stype_in"] != PARENT_STYPE or query["start"] != PARENT_START
            or query["end"] != PARENT_END):
        raise ValueError("parent query scope differs")
    symbols = query["symbols"]
    if not isinstance(symbols, list) or len(symbols) != PARENT_SYMBOLS or len(set(symbols)) != PARENT_SYMBOLS:
        raise ValueError("parent symbol scope differs")
    wire = preflight.canonical_query(query)
    qh = preflight.digest(wire)
    op = preflight.digest(wire + b"\0/batch.submit_job")
    if qh != PARENT_QUERY_SHA or op != PARENT_OPERATION_ID:
        raise ValueError("parent canonical identity differs")
    return query, symbols, qh, op


def validate_journal_coverage(preflight, stats_ops, ledger_path):
    _raw, journal = preflight.ledger(ledger_path)
    rows = {r["operation_id"]: r for r in journal["reservations"]}
    acquired = 0
    for op, (item, _path, qh, _query) in stats_ops.items():
        row = rows.get(op)
        if row is None:
            raise ValueError("statistics coverage incomplete; fresh operation")
        if row.get("endpoint") != BATCH_SUBMIT_ENDPOINT or row.get("query_hash") != qh:
            raise ValueError("validated pending scope differs; prior financial binding mismatch")
        status = row.get("status")
        if op == PARENT_OPERATION_ID:
            if status != "reserved" or qh != PARENT_QUERY_SHA:
                raise ValueError("parent financial row must stay reserved unknown untouched")
        elif status == "acquired":
            acquired += 1
        elif status == "reserved":
            raise ValueError("unexpected reserved operation; only parent may stay reserved")
        else:
            raise ValueError("validated pending scope differs; prior financial outcome")
    if acquired != 542:
        raise ValueError("statistics coverage incomplete; 542 acquired plus parent reserved required")
    return journal


def validate_plan_and_children(preflight, plan_path, parent_query, parent_symbols, stats_ops, journal_rows):
    praw = preflight.private(plan_path)
    plan = preflight.read_json(plan_path)
    if plan.get("schema") != PLAN_SCHEMA:
        raise ValueError("overlay plan schema differs")
    if plan.get("partition_method") != PARTITION_METHOD:
        raise ValueError("overlay partition method differs")
    if plan.get("financial_parent_status") != PARENT_STATUS:
        raise ValueError("overlay parent status differs")
    if plan.get("parent_operation_id") != PARENT_OPERATION_ID:
        raise ValueError("overlay parent operation differs")
    if plan.get("parent_query_hash") != PARENT_QUERY_SHA:
        raise ValueError("overlay parent query hash differs")
    if plan.get("original_manifest_sha256") != ORIGINAL_MANIFEST_SHA:
        raise ValueError("overlay original manifest binding differs")
    if plan.get("parent_query_sha256") != PARENT_FILE_SHA:
        raise ValueError("overlay parent file binding differs")
    if (plan.get("original_manifest_path") != str(EXPECTED_BASE / ORIGINAL_MANIFEST_RELATIVE)
            or plan.get("parent_query_path") != str(EXPECTED_BASE / ORIGINAL_MANIFEST_RELATIVE.parent / PARENT_FILENAME)
            or plan.get("original_required_statistics_partitions") != 543
            or plan.get("additional_data_charge_possible") is not True):
        raise ValueError("overlay original scope declaration differs")
    if plan.get("source_gate_passed") is not False:
        raise ValueError("overlay must not claim source qualification")
    if plan.get("historical_performance_available") is not False:
        raise ValueError("overlay must not claim historical performance")
    queries = plan.get("queries", [])
    if not isinstance(queries, list) or len(queries) != 2:
        raise ValueError("overlay requires exactly two children")
    seen_ops = set()
    children = []
    for index, entry in enumerate(queries):
        expected_name = CHILD_FILENAMES[index]
        if entry.get("filename") != expected_name:
            raise ValueError("overlay child filename differs")
        if entry.get("date") != CHILD_DATE or entry.get("symbols") != CHILD_SYMBOLS_EACH:
            raise ValueError("overlay child scope differs")
        if not isinstance(entry.get("sha256"), str) or not isinstance(entry.get("bytes"), int):
            raise ValueError("overlay child binding differs")
        if not isinstance(entry.get("query_hash"), str) or not isinstance(entry.get("operation_id"), str):
            raise ValueError("overlay child identity differs")
        child_path = plan_path.parent / expected_name
        craw = preflight.private(child_path)
        if len(craw) != entry["bytes"] or hashlib.sha256(craw).hexdigest() != entry["sha256"]:
            raise ValueError("overlay child file binding differs")
        child = preflight.read_json(child_path)
        if set(child) != {"dataset", "schema", "symbols", "stype_in", "start", "end"}:
            raise ValueError("overlay child query keys differ")
        if (child["dataset"] != PARENT_DATASET or child["schema"] != PARENT_SCHEMA
                or child["stype_in"] != PARENT_STYPE or child["start"] != PARENT_START
                or child["end"] != PARENT_END):
            raise ValueError("overlay child must keep same full-day scope")
        symbols = child["symbols"]
        if not isinstance(symbols, list) or len(symbols) != CHILD_SYMBOLS_EACH or len(set(symbols)) != CHILD_SYMBOLS_EACH:
            raise ValueError("overlay child symbol scope differs")
        want = parent_symbols[:1000] if index == 0 else parent_symbols[1000:]
        if symbols != want:
            raise ValueError("overlay child must be exact ordered parent half")
        wire = preflight.canonical_query(child)
        qh = preflight.digest(wire)
        op = preflight.digest(wire + b"\0/batch.submit_job")
        if qh != entry["query_hash"] or op != entry["operation_id"]:
            raise ValueError("overlay child canonical identity differs")
        if op in seen_ops or op == PARENT_OPERATION_ID or op in stats_ops:
            raise ValueError("duplicate or parent-overlapping overlay operation")
        seen_ops.add(op)
        if op in journal_rows:
            raise ValueError("overlay child already attempted; no resubmit")
        children.append((entry, child_path, op, qh))
    # Exact disjoint union, no gap/overlap, no outside scope.
    first = preflight.read_json(plan_path.parent / CHILD_FILENAMES[0])["symbols"]
    second = preflight.read_json(plan_path.parent / CHILD_FILENAMES[1])["symbols"]
    if len(first) != 1000 or len(second) != 1000:
        raise ValueError("overlay halves must be 1000 each")
    if set(first) & set(second):
        raise ValueError("overlay children overlap")
    if first + second != parent_symbols:
        raise ValueError("overlay union must equal exact ordered parent")
    if set(first + second) != set(parent_symbols) or len(set(first + second)) != 2000:
        raise ValueError("overlay union gap or outside scope")
    return plan, children


def validate_sources_and_binaries(preflight, base, source_root, book):
    qualification = preflight.read_qualification(source_root / QUALIFICATION_RELATIVE)
    source_manifest = {v["path"]: v["sha256"] for v in qualification["source_files"]}
    code = [*preflight.CODE, "internal/databento/batch_files.go", "cmd/autowheel-batch-files/main.go",
            "internal/databento/batch_planned_retrieve.go", "cmd/autowheel-batch-planned-retrieve/main.go"]
    for name in code:
        if preflight.digest((source_root / name).read_bytes()) != source_manifest.get(name):
            raise ValueError("accepted acquisition source differs")
    binaries = {}
    for name, (relative, expected) in {**preflight.TOOLS, **book.FILE_TOOLS}.items():
        path = base / relative
        info = path.lstat()
        if (path.resolve() != path or not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o755
                or preflight.digest(path.read_bytes()) != expected):
            raise ValueError("accepted research binary differs")
        binaries[name] = path
    return binaries


def bind_execution_inputs(preflight, args, children):
    qualification = preflight.read_qualification(args.source_root / QUALIFICATION_RELATIVE)
    known = {v["path"]: v["sha256"] for v in qualification["source_files"]}
    if len(known) != len(qualification["source_files"]):
        raise ValueError("duplicate accepted source identities")
    paths = [args.plan, *(path for _, path, _, _ in children),
             args.base / ORIGINAL_MANIFEST_RELATIVE,
             args.base / ORIGINAL_MANIFEST_RELATIVE.parent / PARENT_FILENAME,
             Path(__file__), Path(__file__).with_name(RESERVED_SAFE_CONTROLLER_NAME),
             Path(__file__).with_name(BOOK_HELPER_NAME)]
    bound = {}
    for path in paths:
        key = str(path.relative_to(args.source_root)) if path.is_relative_to(args.source_root) else str(path)
        expected = known.get(key)
        if not expected or path.is_symlink() or path.resolve() != path:
            raise ValueError("unaccepted overlay execution input")
        raw = preflight.private(path) if not path.is_relative_to(args.source_root) else path.read_bytes()
        if preflight.digest(raw) != expected:
            raise ValueError("accepted overlay execution input changed")
        bound[path] = expected
    return bound


def recheck_inputs(preflight, bound):
    for path, expected in bound.items():
        raw = path.read_bytes() if path.is_relative_to(EXPECTED_BASE / "source-afa3075") else preflight.private(path)
        if path.is_symlink() or path.resolve() != path or preflight.digest(raw) != expected:
            raise ValueError("execution source changed after admission")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("base", "source-root", "plan", "ledger", "key-file", "python", "output"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--execute", action="store_true")
    args = parser.parse_args()
    for p in (args.base, args.source_root, args.plan, args.ledger, args.key_file, args.python, args.output):
        clean_absolute(p)
    # Guard before preflight creates output; exact basenames, skip self PID. No env bypass.
    live_writers_absent()
    reserved_safe_fd = None
    book_fd = None
    if args.execute:
        require_no_active_stats_controller()
    controller = load_reserved_controller()
    book = load_book_helpers()
    preflight = controller.load_preflight()
    require_fixed_bindings(args)
    if args.execute:
        # Same stats lock plus existing book lock, before journal read/outputs, held full run.
        live_writers_absent()
        reserved_safe_fd = controller.acquire_reserved_safe_lock(args.base)
        try:
            book_fd = acquire_book_lock(args.base)
        except Exception:
            try:
                os.close(reserved_safe_fd)
            except OSError:
                pass
            reserved_safe_fd = None
            raise
        live_writers_absent()
        require_no_active_stats_controller()
    try:
        # Journal read only after guards (and locks on execute). Dry-run reads journal
        # for coverage but never reads credentials nor runs Go CLIs.
        manifest_path = args.base / ORIGINAL_MANIFEST_RELATIVE
        parent_path = manifest_path.parent / PARENT_FILENAME
        stats_ops = validate_original_manifest(preflight, manifest_path)
        parent_query, parent_symbols, parent_qh, parent_op = validate_parent(preflight, parent_path)
        _ledger_raw, journal_before = preflight.ledger(args.ledger)
        journal_rows = {r["operation_id"]: r for r in journal_before["reservations"]}
        validate_journal_coverage(preflight, stats_ops, args.ledger)
        plan, children = validate_plan_and_children(
            preflight, args.plan, parent_query, parent_symbols, stats_ops, journal_rows)
        binaries = validate_sources_and_binaries(preflight, args.base, args.source_root, book)
        bound = bind_execution_inputs(preflight, args, children) if args.execute else {}
        expected = journal_before
        initial_count = len(expected["reservations"])
        initial_raw = preflight.private(args.ledger)
        if preflight.ledger(args.ledger)[1] != expected:
            raise ValueError("journal changed during admission")
        args.output.mkdir(mode=0o700)
        preflight.save(args.output / "ledger-before.private.json", expected)
        preflight.save(args.output / "overlay-validation.json", {
            "schema": PLAN_SCHEMA,
            "original_manifest_sha256": ORIGINAL_MANIFEST_SHA,
            "parent_operation_id": PARENT_OPERATION_ID,
            "parent_query_hash": PARENT_QUERY_SHA,
            "parent_symbols": PARENT_SYMBOLS,
            "children": [{"filename": e["filename"], "symbols": e["symbols"],
                          "query_hash": e["query_hash"], "operation_id": e["operation_id"]} for e, _, _, _ in children],
            "union_exact_ordered": True,
            "disjoint": True,
            "parent_reserved_unknown_untouched": True,
            "frozen_543_distinct_from_children": True,
            "source_qualified": False,
            "historical_performance_available": False,
        })
        runtime = {"schema": "AutoWheelDisjointStatisticsOverlayAcquisitionRunV1",
                   "overlay_plan_schema": PLAN_SCHEMA,
                   "original_manifest_sha256": ORIGINAL_MANIFEST_SHA,
                   "parent_operation_id": PARENT_OPERATION_ID,
                   "parent_query_hash": PARENT_QUERY_SHA,
                   "partition_method": PARTITION_METHOD,
                   "financial_parent_status": PARENT_STATUS,
                   "required_children": 2,
                   "financial_actions_serial": True,
                   "max_pending": 1,
                   "minimum_submit_spacing_seconds": 4,
                   "poll_spacing_seconds": 20,
                   "pending_job_deadline_seconds": 21600,
                   "execute": args.execute,
                   "uncapped": True,
                   "initial_journal_sha256": preflight.digest(initial_raw),
                   "financial_retry": False,
                   "source_qualified": False,
                   "historical_performance_available": False,
                   "original_preflight_sha256": controller.PREFLIGHT_SHA,
                   "original_controller_sha256": RESERVED_SAFE_CONTROLLER_SHA,
                   "runner_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest()}
        runtime["execution_inputs_accepted"] = bool(bound)
        preflight.save(args.output / "runtime-plan.json", runtime)
        if not args.execute:
            runtime["journal_snapshot_only"] = True
            runtime["credentials_read"] = False
            runtime["financial_requests"] = 0
            print(json.dumps(runtime), flush=True)
            if preflight.private(args.ledger) != initial_raw:
                raise ValueError("ledger changed during preflight")
            return
        # Execute gate: journal unchanged, coverage unchanged, both locks held.
        live_writers_absent()
        if reserved_safe_fd is None or book_fd is None:
            raise ValueError("reserved-safe execution locks required")
        if preflight.ledger(args.ledger)[1] != expected:
            raise ValueError("unexpected financial journal mutation before execution")

        def check_journal():
            live_writers_absent()
            recheck_inputs(preflight, bound)
            if preflight.ledger(args.ledger)[1] != expected:
                raise ValueError("unexpected financial journal mutation")

        def capacity():
            fs = os.statvfs(args.base)
            if fs.f_bavail * fs.f_frsize < RESERVE_BYTES:
                raise ValueError("research capacity reserve unavailable")

        completed = []
        submitted = 0
        last_submit = float("-inf")
        phase = "initial"
        item = {"filename": None}
        active = None
        receipt = None
        try:
            for entry, query_path, operation, query_hash in children:
                item = entry
                receipt = None
                phase = "pre-submit"
                time.sleep(max(0, 4 - (time.monotonic() - last_submit)))
                check_journal()
                capacity()
                out = args.output / entry["filename"].removesuffix(".json")
                out.mkdir(mode=0o700)
                preflight.save(out / "intent.json", {"query_file_sha256": entry["sha256"],
                    "operation_id": operation, "financial_retry": False,
                    "parent_operation_id": PARENT_OPERATION_ID,
                    "parent_untouched": True})
                phase = "submit"
                last_submit = time.monotonic()
                submit = out / "submit"
                # Single cost+submit; never retry a financial send.
                if preflight.run(binaries["batch"], ["--query", query_path, "--output", submit,
                        "--key-file", args.key_file, "--submit"], out, phase) != 0:
                    raise ValueError("financial attempt stopped; evidence and hold preserved")
                acknowledgement = preflight.read_json(submit / "ack.json")
                _, current = preflight.ledger(args.ledger)
                quote_hash = preflight.read_json(submit / "quote.json")["quote"]["ResponseHash"]
                controller.appended(expected, current, operation, query_hash, acknowledgement,
                                    quote_hash, preflight.private(submit / "job.json"))
                expected = current
                submitted += 1
                print(json.dumps({"submitted_children": submitted, "required_children": 2,
                    "journal_entries": len(expected["reservations"])}), flush=True)
                active = {"item": entry, "query": query_path, "operation": operation,
                          "out": out, "submitted": time.monotonic(), "last_poll": float("-inf"), "poll": 0}
                # Serial observe: only GET, >=20s spacing, 6h deadline, <=3 fresh paths per transport.
                receipt = None
                while True:
                    phase = "pre-observe"
                    now = time.monotonic()
                    if now - active["submitted"] > 21600:
                        raise ValueError("prepaid job deadline; inspect existing job, never resubmit")
                    if now - active["last_poll"] < 20:
                        time.sleep(1)
                        continue
                    check_journal()
                    common = ["--query", query_path, "--ack", out / "submit/ack.json",
                              "--ack-body", out / "submit/job.json", "--key-file", args.key_file]
                    phase = "observe-" + str(active["poll"])
                    receipt = controller.observe_readonly(preflight, binaries["retrieve"], common, out, phase)
                    active["poll"] += 1
                    active["last_poll"] = time.monotonic()
                    if receipt["state"] in ("queued", "processing"):
                        continue
                    break
                if receipt["state"] != "done":
                    raise ValueError("unknown prepaid job state")
                check_journal()
                capacity()
                phase = "file-plan"
                plan_dir, file_plan = book.observe_file_plan(preflight, binaries["file_plan"], common, out, phase)
                total = book.file_plan_total(file_plan, operation, query_hash)
                fs = os.statvfs(args.base)
                if total < 1 or total + RESERVE_BYTES > fs.f_bavail * fs.f_frsize:
                    raise ValueError("planned download would consume research disk reserve")
                if preflight.digest(preflight.private(plan_dir / "files-source.json")) != file_plan["file_list_sha256"]:
                    raise ValueError("file plan original response binding differs")
                check_journal()
                phase = "download"
                acquired = out / "acquired"
                if preflight.run(binaries["planned_retrieve"], [*common, "--output", acquired,
                        "--timeout", "30m", "--planned-files", plan_dir / "files-source.json"], out, phase) != 0:
                    raise ValueError("download stopped; private evidence retained")
                check_journal()
                book.same_download_plan(file_plan, preflight.read_json(acquired / "receipt.json"))
                phase = "finalize"
                proof_path = out / "proof.json"
                if preflight.run(binaries["finalize"], ["--query", query_path, "--ack", out / "submit/ack.json",
                        "--ack-body", out / "submit/job.json", "--source", acquired, "--proof", proof_path,
                        "--python", args.python, "--ledger", args.ledger, "--timeout", "30m"], out, phase) != 0:
                    raise ValueError("offline acquisition verification stopped; hold preserved")
                proof = preflight.read_json(proof_path)
                proof_sha = preflight.digest(preflight.private(proof_path))
                if (proof["query_file_sha256"] != entry["sha256"]
                        or proof["native"]["all_eof_verified"] is not True
                        or proof["native"]["records"] != proof["provider_record_count"]
                        or proof["source_qualified"] is not False):
                    raise ValueError("native proof binding differs")
                _, current = preflight.ledger(args.ledger)
                controller.finalized(expected, current, operation, proof_sha, proof["total_bytes"])
                expected = current
                row = next(r for r in expected["reservations"] if r["operation_id"] == operation)
                result = {"query": entry["filename"], "date": entry["date"],
                          "proof_sha256": proof_sha, "records": proof["native"]["records"],
                          "compressed_bytes": proof["total_bytes"], "quoted_cost_nano": row["cost_nano"],
                          "all_eof_verified": True, "journal_entries": len(expected["reservations"]),
                          "original_journal_prefix_unchanged": True, "source_qualified": False,
                          "historical_performance_available": False}
                preflight.save(out / "aggregate.json", result)
                completed.append(result)
                active = None
                print(json.dumps({"completed_children": len(completed),
                    "required_children": 2, **result}), flush=True)
            check_journal()
            final_raw, final = preflight.ledger(args.ledger)
            preflight.save(args.output / "ledger-after.private.json", final)
            parent_row = next(r for r in final["reservations"] if r["operation_id"] == PARENT_OPERATION_ID)
            if parent_row.get("status") != "reserved" or parent_row.get("query_hash") != PARENT_QUERY_SHA:
                raise ValueError("parent financial row changed")
            summary = {**runtime, "completed_children": len(completed),
                "overlay_complete": True,
                "parent_operation_id": PARENT_OPERATION_ID,
                "parent_unknown_unchanged": True,
                "financial_parent_status": PARENT_STATUS,
                "records": sum(v["records"] for v in completed),
                "compressed_bytes": sum(v["compressed_bytes"] for v in completed),
                "quoted_cost_nano": sum(v["quoted_cost_nano"] for v in completed),
                "journal_sha256": preflight.digest(final_raw),
                "journal_status_counts": dict(Counter(v["status"] for v in final["reservations"])),
                "original_journal_prefix_unchanged": final["reservations"][:initial_count]
                    == preflight.read_json(args.output / "ledger-before.private.json")["reservations"],
                "additional_data_charge_possible": True,
                "original_543_coverage_qualified": False,
                "full_study_complete": False,
                "source_qualified": False,
                "historical_performance_available": False,
                "completed": completed,
                "observed_at": datetime.now(timezone.utc).isoformat()}
            preflight.save(args.output / "summary.json", summary)
            print(json.dumps({"overlay_complete": True, "completed_children": len(completed),
                "parent_unknown_unchanged": True,
                "journal_entries": len(final["reservations"])}), flush=True)
        except Exception:
            pending = 1 if active is not None else 0
            try:
                preflight.save(args.output / "interruption.json", {"phase": phase,
                    "query": item.get("filename"), "submitted_children": submitted,
                    "completed_children": len(completed), "pending_children": pending,
                    "required_children": 2, "financial_retry": False, "holds_released": False,
                    "parent_unknown_unchanged": True,
                    "unacknowledged_submit_outcome": phase == "submit",
                    "pending_acknowledged_children": pending,
                    "last_verified_provider_state": receipt.get("state") if receipt else None,
                    "no_resubmit_on_restart": True, "inspection_required": True})
            except Exception:
                pass
            print(json.dumps({"stopped": True, "phase": phase,
                "completed_children": len(completed), "financial_retry": False,
                "holds_released": False}), flush=True)
            raise
    finally:
        for fd in (book_fd, reserved_safe_fd):
            try:
                if fd is not None:
                    os.close(fd)
            except OSError:
                pass


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(json.dumps({"status": "stopped", "error_type": type(error).__name__,
            "inspect_private_evidence": True}), flush=True)
        sys.exit(1)
