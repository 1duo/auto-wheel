#!/usr/bin/env python3
"""Independent bounded reconstruction of raw closed-cut snapshots.

Uses only already-verified native vector JSONL.gz and the original compressed
source header for identity. No provider calls, credentials, builds, tests, or
data execution. Compares every Go-produced snapshot one at a time.

Reference is an actual reduction using independently proven rows; it does not
qualify source terms, listing, Greeks, lifecycle, fills, or performance.
"""
import argparse
from collections import Counter
from datetime import datetime, timedelta, timezone
import gzip
import hashlib
import importlib.metadata
import importlib.util
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
from zoneinfo import ZoneInfo

from batch_acquisition_proof import file_hash, read_json

DECODER_VERSION = "0.71.0"
DAY_NANOS = 86_400_000_000_000
MAX_UINT64 = (1 << 64) - 1
MAX_INT64 = (1 << 63) - 1
MIN_INT64 = -(1 << 63)
MAX_UINT32 = (1 << 32) - 1
STRUCTURAL_FLAGS = 128 | 64 | 16 | 1
LAST_FLAG = 128

# Go causalbook.Status strings (canonical snapshot format).
ST_PENDING = "incomplete_event"
ST_UNSEEN = "unseen"
ST_TRADE_ONLY = "trade_only"
ST_RESET = "reset"
ST_DEPTH = "book_depth_unqualified"
ST_AMBIGUOUS = "trade_book_ambiguity"
ST_CLOCK_UNDEF = "event_clock_undefined"
ST_CLOCK_LEAD = "event_clock_lead"
ST_UNDEF_PRICE = "undefined_price"
ST_NONPOSITIVE = "nonpositive_price"
ST_EMPTY = "zero_size"
ST_CROSSED = "crossed"
ST_STALE = "stale_raw_book"
ST_FRESH = "fresh_raw_book"


def _private_file(path):
    info = path.lstat()
    if (not path.is_absolute() or path.resolve() != path
            or not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
            or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600):
        raise ValueError("private regular input required: " + str(path))


def _private_directory(path):
    info = path.lstat()
    if (not path.is_absolute() or path.resolve() != path
            or not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid()
            or stat.S_IMODE(info.st_mode) != 0o700):
        raise ValueError("private directory required: " + str(path))


def _sync_directory(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def _write_json(path, value):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "wb") as output:
        output.write((json.dumps(value, indent=2, allow_nan=False) + "\n").encode())
        output.flush()
        os.fsync(output.fileno())
    _sync_directory(path.parent)


def _is_hash(value):
    return (isinstance(value, str) and len(value) == 64
            and all(c in "0123456789abcdef" for c in value))


def _frozen_cuts(document, query, start, end, root):
    path = root / "scripts/frozen-study-option-cuts.py"
    spec = importlib.util.spec_from_file_location("frozen_option_capture_interface", path)
    interface = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(interface)
    return interface.verify_frozen_cuts(document, query, start, end, root)


def _check_int(value, low, high):
    if isinstance(value, bool) or not isinstance(value, int) or not low <= value <= high:
        raise ValueError("native integer width")
    return value


def _validate_vector(vec, index, version, reverse, schema, ts_out_present, start, end):
    if not isinstance(vec, list):
        raise ValueError("vector shape")
    want = 21 if schema == "cmbp-1" else 23
    if len(vec) != want:
        raise ValueError("vector field count")
    (rowidx, ver, rawsym, pub, inst, recv, event, out, price, size,
     action, side, flags, delta, bidpx, askpx, bidsz, asksz, rtype) = vec[0:19]
    if type(rowidx) is not int or rowidx != index:
        raise ValueError("vector row index")
    if type(ver) is not int or ver != version:
        raise ValueError("vector version")
    if not isinstance(rawsym, str):
        raise ValueError("vector raw symbol")
    _check_int(pub, 1, 0xFFFF)
    _check_int(inst, 1, MAX_UINT32)
    if inst not in reverse or reverse[inst] != rawsym:
        raise ValueError("vector identity")
    _check_int(recv, start, end - 1)
    _check_int(event, 0, MAX_UINT64)
    if ts_out_present:
        _check_int(out, 0, MAX_UINT64)
    elif out is not None:
        raise ValueError("vector ts_out")
    _check_int(price, MIN_INT64, MAX_INT64)
    _check_int(size, 0, MAX_UINT32)
    if not isinstance(action, str) or len(action) != 1 or action not in ("A", "C", "M", "R", "T"):
        raise ValueError("vector action")
    if not isinstance(side, str) or len(side) != 1 or side not in ("A", "B", "N"):
        raise ValueError("vector side")
    if action == "R" and side != "N":
        raise ValueError("vector reset side")
    _check_int(flags, 0, 0xFF)
    if flags & ~STRUCTURAL_FLAGS != 0:
        raise ValueError("vector flags")
    _check_int(delta, -(1 << 31), (1 << 31) - 1)
    _check_int(bidpx, MIN_INT64, MAX_INT64)
    _check_int(askpx, MIN_INT64, MAX_INT64)
    _check_int(bidsz, 0, MAX_UINT32)
    _check_int(asksz, 0, MAX_UINT32)
    _check_int(rtype, 0, 0xFF)
    if schema == "cmbp-1":
        if rtype != 177:
            raise ValueError("vector CMBP rtype")
        _check_int(vec[19], 0, 0xFFFF)
        _check_int(vec[20], 0, 0xFFFF)
        if flags & (128 | 64) != (128 | 64) or flags & 16 != 0:
            raise ValueError("vector CMBP flags require LAST+TOB and no MBP")
    else:
        if rtype != 1:
            raise ValueError("vector MBP rtype")
        _check_int(vec[19], 0, MAX_UINT32)
        _check_int(vec[20], 0, MAX_UINT32)
        _check_int(vec[21], 0, 0xFF)
        _check_int(vec[22], 0, MAX_UINT32)
        if pub != 95 or vec[22] != 0 or delta != 0 or vec[19] != 0 or vec[20] != 0:
            raise ValueError("vector stock pub95/seqdelta/counts0")
    return (pub, inst, recv, event, out, price, size, action, side, flags, delta,
            bidpx, askpx, bidsz, asksz, rtype)


def _same_levels(book, trade, schema):
    # book/trade are full preserved vector arrays.
    if (book[14] != trade[14] or book[15] != trade[15]
            or book[16] != trade[16] or book[17] != trade[17]):
        return False
    if schema == "cmbp-1":
        return book[19] == trade[19] and book[20] == trade[20]
    return book[19] == trade[19] and book[20] == trade[20]


def _status_for(has_book, book, has_trade, pending, ambiguous, cut, max_age):
    if pending:
        return ST_PENDING
    if not has_book:
        return ST_TRADE_ONLY if has_trade else ST_UNSEEN
    action = book[10]
    if action == "R":
        return ST_RESET
    if len(book) == 23 and book[21] != 0:
        return ST_DEPTH
    if ambiguous:
        return ST_AMBIGUOUS
    event, recv = book[6], book[5]
    if event == MAX_UINT64:
        return ST_CLOCK_UNDEF
    if event > recv:
        return ST_CLOCK_LEAD
    bidpx, askpx = book[14], book[15]
    if bidpx == MAX_INT64 or askpx == MAX_INT64:
        return ST_UNDEF_PRICE
    if bidpx <= 0 or askpx <= 0:
        return ST_NONPOSITIVE
    if book[16] == 0 or book[17] == 0:
        return ST_EMPTY
    if bidpx > askpx:
        return ST_CROSSED
    # Go uses uint64 wrap; cut >= recv holds for included books.
    if cut < recv or cut - recv > max_age:
        return ST_STALE
    return ST_FRESH


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("proof", "ledger", "audit-directory", "cuts", "producer-directory", "binary", "output"):
        parser.add_argument("--" + name, type=Path, required=True)
    args = parser.parse_args()
    for p in (args.proof, args.ledger, args.audit_directory, args.cuts, args.producer_directory, args.output):
        if not p.is_absolute():
            raise ValueError("absolute path required")
    if importlib.metadata.version("databento-dbn") != DECODER_VERSION:
        raise ValueError("pinned native decoder required")
    import databento_dbn as dbn
    root = Path(__file__).resolve().parents[1]
    adapter_path = root / "scripts/audit-native-book-batch.py"
    spec = importlib.util.spec_from_file_location("native_adapter", adapter_path)
    adapter = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(adapter)
    # Private scope: evidence files 0600/1-link/uid, directories 0700, fresh output.
    _private_directory(args.audit_directory)
    _private_directory(args.producer_directory)
    _private_directory(args.output.parent)
    if (not args.binary.is_absolute() or args.binary.resolve() != args.binary
            or not args.binary.is_relative_to(Path("/home/yuduo/Projects/AutoWheel-research"))):
        raise ValueError("private binary scope differs")
    _private_directory(args.binary.parent)
    binary_info = args.binary.lstat()
    if (not stat.S_ISREG(binary_info.st_mode) or binary_info.st_uid != os.getuid()
            or binary_info.st_nlink != 1 or stat.S_IMODE(binary_info.st_mode) != 0o700):
        raise ValueError("private binary required")
    if args.output.exists():
        raise ValueError("fresh output required")
    adapter.private_file(args.cuts)
    proof, manifest, start, end = adapter.acquired_source(args.proof, args.ledger, root)
    audit_path = args.audit_directory / "audit.json"
    reference_path = args.audit_directory / "reference.json"
    for p in (audit_path, reference_path):
        adapter.private_file(p)
    audit, reference = read_json(audit_path), read_json(reference_path)
    if (len(audit["files"]) != 1 or audit["source_gate_passed"] is not False
            or audit["schema_version"] != "AutoWheelNativeBookSourceAuditV1"
            or audit["decoder_version"] != DECODER_VERSION or audit["upgrade_policy"] != "AS_IS"
            or audit["operation_id"] != proof["reservation"]["operation_id"]
            or audit["query_hash"] != proof["reservation"]["query_hash"]
            or audit["acquisition_proof_sha256"] != file_hash(args.proof)
            or reference["schema_version"] != "AutoWheelNativeBookVectorReferenceV1"
            or reference["audit_sha256"] != file_hash(audit_path)
            or reference["receipt_sha256"] != proof["receipt_sha256"]
            or reference["records_compared_field_for_field"] != audit["records"]
            or reference["reference_script_sha256"] != file_hash(root / "scripts/verify-native-book-vectors.py")
            or reference["full_source_qualification"] is not False):
        raise ValueError("complete independent reference differs")
    member = audit["files"][0]
    query = proof["query"]
    if (audit["job_id"] != proof["job_id"] or reference["queried_symbols"] != len(query["symbols"])
            or any(audit["query_" + k] != query[k] or reference["query_" + k] != query[k]
                   for k in ("dataset", "schema", "start", "end"))):
        raise ValueError("acquired scope differs")
    if query["dataset"] not in ("OPRA.PILLAR", "EQUS.MINI") or query["schema"] not in ("cmbp-1", "mbp-1"):
        raise ValueError("book scope differs")
    if (query["dataset"] == "OPRA.PILLAR") != (query["schema"] == "cmbp-1"):
        raise ValueError("dataset/schema provenance differs")
    cuts_doc = read_json(args.cuts)
    cuts, cut_sources = _frozen_cuts(cuts_doc, query, start, end, root)
    max_age = cuts_doc["max_age_nanos"]
    if end - start != DAY_NANOS or start % DAY_NANOS != 0:
        raise ValueError("exact UTC day required")
    # Produced artifacts.
    producer_report_path = args.producer_directory / "report.json"
    produced_path = args.producer_directory / "snapshots.jsonl.gz"
    for p in (producer_report_path, produced_path):
        adapter.private_file(p)
    producer = read_json(producer_report_path)
    # Native evidence paths.
    source = Path(proof["source_directory"]) / member["source_filename"]
    vector_path = args.audit_directory / member["vector_file"]
    for p in (source, vector_path):
        adapter.private_file(p)
    if file_hash(source) != member["source_compressed_sha256"]:
        raise ValueError("complete native source differs")
    if file_hash(vector_path) != member["vector_sha256"]:
        raise ValueError("native vector digest differs")
    # Helpers/scripts bound before/after (code files are hashed, not private).
    producer_path = root / "scripts/produce-native-study-option-captures.py"
    verifier_helpers = root / "scripts/verify-native-book-vectors.py"
    batch_path = root / "scripts/batch_acquisition_proof.py"
    reducer_path = root / "internal/causalbook/observe.go"
    stream_path = root / "internal/nativebook/stream.go"
    ref_main_path = root / "research/simulation/2026-10-05/cycle31-causal-reference/main.go"
    self_path = Path(__file__).resolve()
    manifest_paths = [Path(n) if Path(n).is_absolute() else root / n for n in manifest]
    cut_paths = [root / p for p in cut_sources]
    relevant = list(dict.fromkeys(
        manifest_paths + cut_paths
        + [args.proof, args.ledger, audit_path, reference_path, args.cuts,
           producer_report_path, produced_path, source, vector_path,
           producer_path, adapter_path, verifier_helpers, batch_path,
           reducer_path, stream_path, ref_main_path, self_path, args.binary]))
    before = {p: file_hash(p) for p in relevant}
    # Strict producer SHA bindings for available inputs.
    for label, got, want in [
            ("acquisition_proof", producer.get("acquisition_proof_sha256"), before[args.proof]),
            ("audit", producer.get("audit_sha256"), before[audit_path]),
            ("reference", producer.get("independent_native_reference_sha256"), before[reference_path]),
            ("cuts", producer.get("cuts_sha256"), before[args.cuts]),
            ("reducer", producer.get("reducer_sha256"), before[reducer_path]),
            ("producer", producer.get("producer_sha256"), before[producer_path])]:
        if got != want:
            raise ValueError("producer " + label + " binding differs")
    binary_sha = producer.get("binary_sha256")
    if not _is_hash(binary_sha) or binary_sha != before[args.binary]:
        raise ValueError("producer binary binding differs")
    if (producer.get("schema") != "AutoWheelNativeCausalObservationV1"
            or producer.get("records_processed") != member["records"]
            or producer.get("native_sha256") != member["source_uncompressed_sha256"]
            or producer.get("native_bytes") != member["source_uncompressed_bytes"]
            or producer.get("snapshots") != len(cuts)
            or producer.get("full_source_qualification") is not False
            or producer.get("replay_frames_produced") is not False
            or producer.get("historical_performance_available") is not False
            or producer.get("query_dataset") != query["dataset"]
            or producer.get("query_schema") != query["schema"]
            or producer.get("snapshot_file") != produced_path.name
            or producer.get("snapshot_compressed_sha256") != before[produced_path]
            or type(producer.get("snapshot_compressed_bytes")) is not int
            or producer.get("snapshot_compressed_bytes") != produced_path.stat().st_size
            or not _is_hash(producer.get("snapshot_jsonl_sha256", ""))):
        raise ValueError("producer totals/binding differs")
    if "fixture" in str(produced_path).lower() or "generated" in str(produced_path).lower():
        raise ValueError("local fixture snapshots not qualified")
    # Identity universe from original source header only (proven full integrity).
    fd = os.open(source, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    zstd = None
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600
                or info.st_uid != os.getuid() or info.st_nlink != 1):
            raise ValueError("private native descriptor differs")
        with os.fdopen(os.dup(fd), "rb") as checked:
            digest = hashlib.sha256()
            for chunk in iter(lambda: checked.read(1 << 20), b""):
                digest.update(chunk)
            if digest.hexdigest() != before[source]:
                raise ValueError("native descriptor differs")
            checked.seek(0)
        zstd = subprocess.Popen(["/usr/bin/zstd", "-q", "-f", "-d", "-c", "--", "/dev/fd/" + str(fd)],
                                pass_fds=(fd,), stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        prefix = zstd.stdout.read(8)
        if len(prefix) != 8 or prefix[:3] != b"DBN" or prefix[3] not in (1, 2, 3):
            raise ValueError("native prelude differs")
        meta_len = int.from_bytes(prefix[4:], "little")
        if not 100 <= meta_len <= (16 << 20) - 8:
            raise ValueError("native metadata bound")
        header = prefix + zstd.stdout.read(meta_len)
        if len(header) != meta_len + 8:
            raise ValueError("native metadata truncated")
    finally:
        try:
            if zstd is not None:
                if zstd.poll() is None:
                    zstd.kill()
                try:
                    zstd.wait(timeout=5)
                except Exception:
                    pass
                try:
                    zstd.stdout.close()
                except Exception:
                    pass
        finally:
            os.close(fd)
    decoder = dbn.DBNDecoder(upgrade_policy=dbn.VersionUpgradePolicy.AS_IS, compression=dbn.Compression.NONE)
    decoded = decoder.write_and_decode(header)
    if decoder.buffer() or len(decoded) != 1 or not isinstance(decoded[0], dbn.Metadata):
        raise ValueError("native metadata decode differs")
    if hashlib.sha256(header).hexdigest() != producer.get("metadata_sha256"):
        raise ValueError("native metadata digest differs")
    metadata = decoded[0]
    if (metadata.dataset != query["dataset"] or str(metadata.schema) != query["schema"]
            or metadata.start != start or metadata.end != end or metadata.limit not in (None, 0)
            or str(metadata.stype_in) != "raw_symbol" or str(metadata.stype_out) != "instrument_id"
            or metadata.version != member["native_version"] or type(metadata.ts_out) is not bool
            or not adapter.metadata_symbols_match(list(metadata.symbols), query["symbols"])):
        raise ValueError("native query scope differs")
    day = datetime.fromtimestamp(start // 1_000_000_000, timezone.utc).date()
    _, reverse = adapter.active_mappings(metadata, day, set(query["symbols"]))
    if set(reverse) != set(int(v) for v in reverse) or any(v <= 0 or v >= 1 << 32 for v in reverse):
        raise ValueError("native identity universe differs")
    ids = sorted(reverse)
    if not 1 <= len(ids) <= 2000 or any(v == 0 for v in ids):
        raise ValueError("identity universe bound")
    if producer.get("observations") != len(cuts) * len(ids):
        raise ValueError("producer observation count differs")
    # NEVER derive universe from produced snapshots; header reverse is binding.
    schema = query["schema"]
    dataset = query["dataset"]
    # Bounded per-ID state (never full rows, never sort/retime).
    states = {i: {"has_book": False, "book": None, "avail": 0, "has_trade": False,
                  "trade": None, "pending": False, "changed": False, "ambiguous": False,
                  "working": None, "working_amb": False, "has_update": False,
                  "last_update": 0, "has_clock": False, "last_clock": 0} for i in ids}
    book_records = trade_records = completed = 0
    equal_records = regressions = leads = undefined_clocks = 0
    statuses = Counter()
    snapshots_compared = observations_compared = 0
    vector_hash = hashlib.sha256()
    snapshot_plain_hash = hashlib.sha256()
    produced_compressed_size = produced_path.stat().st_size
    if (produced_compressed_size > 512 << 20 or vector_path.stat().st_size > 64 << 30
            or vector_path.stat().st_size != member["vector_bytes"]):
        raise ValueError("evidence size bound")
    # Stream vectors + produced snapshots together, one snapshot at a time.
    cut_index = 0
    records = 0
    has_prev = False
    prev_recv = 0
    vector_bytes = 0
    snapshot_bytes = 0

    def emit_and_compare(cut):
        nonlocal snapshots_compared, observations_compared, snapshot_bytes
        # Build expected snapshot rows in sorted native ID order.
        rows = []
        for iid in ids:
            s = states[iid]
            covered = cut >= max_age and cut - max_age >= start
            zero = covered and (not s["has_update"] or s["last_update"] < cut - max_age)
            status = _status_for(s["has_book"], s["book"], s["has_trade"],
                                 s["pending"], s["ambiguous"], cut, max_age)
            statuses[status] += 1
            rows.append([iid, s["has_book"], s["book"], s["avail"], s["has_trade"],
                         s["trade"], s["pending"], status, s["has_update"],
                         s["last_update"], covered, zero])
        expected_top = [dataset, start, cut, max_age, rows]
        line = snap_stream.readline(128 << 20)
        if not line or not line.endswith(b"\n") or len(line) > 64 << 20:
            raise ValueError("produced snapshot bound")
        snapshot_bytes += len(line)
        if snapshot_bytes > 512 << 20:
            raise ValueError("produced snapshot total bound")
        snapshot_plain_hash.update(line)
        # Exact canonical snapshot arrays; trailing/extra rows fail below.
        actual = json.loads(line, parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite")))
        if line != (json.dumps(expected_top, separators=(",", ":"), ensure_ascii=True) + "\n").encode():
            raise ValueError("produced snapshot mismatch at cut %d" % cut)
        # Strict shape guard first: Python True==1, so type() must pin
        # bools vs ints, null vs list, and sorted native IDs before value compare.
        if (not isinstance(actual, list) or len(actual) != 5 or actual[0] != dataset
                or type(actual[1]) is not int or type(actual[2]) is not int
                or type(actual[3]) is not int or actual[1] != start or actual[2] != cut
                or actual[3] != max_age or not isinstance(actual[4], list)
                or len(actual[4]) != len(ids)):
            raise ValueError("produced snapshot envelope differs")
        for pos, row in enumerate(actual[4]):
            if (not isinstance(row, list) or len(row) != 12 or type(row[0]) is not int
                    or row[0] != ids[pos]
                    or type(row[1]) is not bool or type(row[4]) is not bool
                    or type(row[6]) is not bool or type(row[8]) is not bool
                    or type(row[10]) is not bool or type(row[11]) is not bool
                    or type(row[3]) is not int or type(row[9]) is not int
                    or not isinstance(row[7], str)
                    or (row[2] is not None and not isinstance(row[2], list))
                    or (row[5] is not None and not isinstance(row[5], list))
                    or (row[1] != (row[2] is not None)) or (row[4] != (row[5] is not None))):
                raise ValueError("produced snapshot row shape differs")
            for got_vec, want_vec in ((row[2], rows[pos][2]), (row[5], rows[pos][5])):
                if (got_vec is None) != (want_vec is None):
                    raise ValueError("produced snapshot vector presence differs")
                if got_vec is not None:
                    if len(got_vec) != len(want_vec):
                        raise ValueError("produced snapshot vector length differs")
                    for gi, wi in zip(got_vec, want_vec):
                        # Null only at ts_out (index 7); strings at 2/10/11.
                        if gi is None or wi is None:
                            if not (gi is None and wi is None):
                                raise ValueError("produced snapshot vector null differs")
                            continue
                        if isinstance(wi, str):
                            if not isinstance(gi, str) or gi != wi:
                                raise ValueError("produced snapshot vector string differs")
                            continue
                        # Python bool is int subclass: require exact int type.
                        if type(gi) is not int or type(wi) is not int or gi != wi:
                            raise ValueError("produced snapshot vector int differs")
        snapshots_compared += 1
        observations_compared += len(ids)

    with gzip.open(vector_path, "rb") as vec_stream, gzip.open(produced_path, "rb") as snap_stream:
        # Iterate vectors in file order; emit cuts whose closed boundary passed.
        # snap_stream is closed over by emit_and_compare (bounded, one line at a time).
        while True:
            raw = vec_stream.readline(4097)
            if not raw:
                break
            vector_bytes += len(raw)
            if not raw.endswith(b"\n") or len(raw) > 4096 or vector_bytes > 512 << 30:
                raise ValueError("vector record bound")
            vector_hash.update(raw)
            vec = json.loads(raw, parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite")))
            _validate_vector(vec, records, metadata.version, reverse, schema, metadata.ts_out, start, end)
            recv = vec[5]
            if recv < start or recv >= end or (has_prev and recv < prev_recv):
                raise ValueError("capture order regression")
            while cut_index < len(cuts) and recv > cuts[cut_index]:
                emit_and_compare(cuts[cut_index])
                cut_index += 1
            if has_prev and recv == prev_recv:
                equal_records += 1
            has_prev, prev_recv = True, recv
            inst = vec[4]
            s = states.get(inst)
            if s is None:
                raise ValueError("unmapped instrument")
            event = vec[6]
            if event == MAX_UINT64:
                undefined_clocks += 1
            else:
                if s["has_clock"] and event < s["last_clock"]:
                    regressions += 1
                if event > recv:
                    leads += 1
                s["last_clock"], s["has_clock"] = event, True
            if not s["pending"]:
                s["changed"] = False
                s["working_amb"] = s["ambiguous"]
            s["pending"] = True
            action = vec[10]
            if action == "T":
                trade_records += 1
                s["trade"], s["has_trade"] = list(vec), True
                base, known = (s["book"], s["has_book"]) if not s["changed"] else (s["working"], True)
                if known and base[10] != "R" and not _same_levels(base, vec, schema):
                    s["working_amb"] = True
            else:
                book_records += 1
                s["working"], s["changed"], s["working_amb"] = list(vec), True, False
                s["last_update"], s["has_update"] = recv, True
            if vec[12] & LAST_FLAG != 0:
                completed += 1
                if s["changed"]:
                    s["book"], s["has_book"], s["avail"] = s["working"], True, recv
                s["ambiguous"] = s["working_amb"]
                s["pending"], s["changed"] = False, False
            records += 1
            if records > member["records"]:
                raise ValueError("record count bound")
        if vec_stream.read(1) != b"":
            raise ValueError("native vector trailing bytes")
        # Remaining closed cuts at whole EOF.
        while cut_index < len(cuts):
            emit_and_compare(cuts[cut_index])
            cut_index += 1
        if snap_stream.read(1) != b"":
            raise ValueError("produced snapshot trailing bytes")
    if (records != member["records"] or records != audit["records"]
            or records != reference["records_compared_field_for_field"]
            or vector_hash.hexdigest() != member["vector_jsonl_sha256"]
            or vector_hash.hexdigest() != reference["vector_jsonl_sha256"]):
        raise ValueError("native vector count/hash differs")
    if snapshot_plain_hash.hexdigest() != producer.get("snapshot_jsonl_sha256"):
        raise ValueError("produced snapshot plain hash differs")
    pending_eof = sum(1 for s in states.values() if s["pending"])
    # Compare every straightforward counter/status total.
    for key, got, want in [
            ("records_processed", records, producer.get("records_processed")),
            ("snapshots", snapshots_compared, producer.get("snapshots")),
            ("observations", observations_compared, producer.get("observations")),
            ("book_records", book_records, producer.get("book_records")),
            ("trade_records", trade_records, producer.get("trade_records")),
            ("completed_events", completed, producer.get("completed_events")),
            ("pending_events_at_eof", pending_eof, producer.get("pending_events_at_eof")),
            ("equal_capture_records", equal_records, producer.get("equal_capture_records")),
            ("event_clock_regressions", regressions, producer.get("event_clock_regressions")),
            ("clock_leads", leads, producer.get("clock_leads")),
            ("undefined_event_clocks", undefined_clocks, producer.get("undefined_event_clocks"))]:
        if got != want:
            raise ValueError("producer counter differs: " + key)
    if dict(statuses) != dict(producer.get("statuses", {})):
        raise ValueError("producer status totals differ")
    if any(file_hash(p) != d for p, d in before.items()):
        raise ValueError("input stability differs")
    result = {
        "schema_version": "AutoWheelNativeCausalObservationVerificationV1",
        "observed_at": datetime.now(timezone.utc).isoformat(),
        "reference_script_sha256": before[self_path],
        "decoder_version": DECODER_VERSION, "upgrade_policy": "AS_IS",
        "acquisition_proof_sha256": before[args.proof],
        "audit_sha256": before[audit_path],
        "independent_native_reference_sha256": before[reference_path],
        "cuts_sha256": before[args.cuts],
        "reducer_sha256": before[reducer_path],
        "producer_sha256": before[producer_path],
        "binary_sha256": binary_sha,
        "producer_report_sha256": before[producer_report_path],
        "snapshot_compressed_sha256": before[produced_path],
        "snapshot_compressed_bytes": produced_compressed_size,
        "snapshot_jsonl_sha256": snapshot_plain_hash.hexdigest(),
        "vector_jsonl_sha256": vector_hash.hexdigest(),
        "query_dataset": dataset, "query_schema": schema,
        "query_start": query["start"], "query_end": query["end"],
        "queried_symbols": len(query["symbols"]),
        "native_version": metadata.version, "ts_out_present": metadata.ts_out,
        "max_age_nanos": max_age,
        "records_reconstructed": records,
        "snapshots_compared": snapshots_compared,
        "observations_compared": observations_compared,
        "book_records": book_records, "trade_records": trade_records,
        "completed_events": completed, "pending_events_at_eof": pending_eof,
        "equal_capture_records": equal_records,
        "event_clock_regressions": regressions, "clock_leads": leads,
        "undefined_event_clocks": undefined_clocks,
        "statuses": dict(statuses),
        "producer_counters_match": True, "producer_snapshots_match": True,
        "retimed_rows": 0, "full_source_qualification": False,
        "replay_frames_produced": False, "historical_performance_available": False,
        "limitations": [
            "Reference is an actual reduction using independently proven rows; it does not qualify source terms, listing, Greeks, lifecycle, fills, or performance.",
            "Identity universe comes only from the original source header via dated active mappings; produced snapshots never supply identity.",
            "Source full integrity was proven separately; this pass checks only the header for identity and streams verified vectors boundedly.",
            "No replay frames, execution coverage, or NoRecentBook inference is asserted."],
    }
    _write_json(args.output, result)
    print(json.dumps(result))


if __name__ == "__main__":
    try:
        main()
    except Exception:
        print("native causal observation verification failed closed", file=sys.stderr)
        raise SystemExit(1)
