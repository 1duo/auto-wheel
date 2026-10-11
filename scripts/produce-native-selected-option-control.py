#!/usr/bin/env python3
"""Selected option projection control over a complete original native session.

The rotating masks are deterministic implementation controls, not causal strategy
decisions, order coverage, source qualification, replay frames or performance."""
import argparse
from datetime import datetime, timezone
import gzip
import hashlib
import importlib.metadata
import importlib.util
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile

from batch_acquisition_proof import file_hash, read_json

BASE = Path("/home/yuduo/Projects/AutoWheel-research")
ONE_SEC_NANOS = 1_000_000_000
EXPECTED_DECISIONS = 74
EXPECTED_WINDOWS = 23100


def fingerprint(info):
    return (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns,
            info.st_ctime_ns, info.st_mode, info.st_uid, info.st_nlink)


def control_plan(windows, reverse):
    ids = sorted(reverse)
    if not 1 <= len(ids) <= 2000:
        raise ValueError("complete bounded option-query universe required")
    plan = []
    for index, window in enumerate(windows):
        offset = (index // 300) % len(ids)
        width = min(1 + (index // 77) % 3, len(ids))
        mask = [ids[(offset + j) % len(ids)] for j in range(width)]
        plan.append(dict(Window=dict(window), Instruments=mask))
    return plan


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("proof", "ledger", "audit-directory", "binary", "cuts", "output-directory"):
        parser.add_argument("--" + name, type=Path, required=True)
    args = parser.parse_args()
    if importlib.metadata.version("databento-dbn") != "0.71.0":
        raise ValueError("pinned native decoder required")
    import databento_dbn as dbn
    root = Path(__file__).resolve().parents[1]
    adapter_path = root / "scripts/audit-native-book-batch.py"
    spec = importlib.util.spec_from_file_location("native_adapter", adapter_path)
    adapter = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(adapter)
    old_producer_path = root / "scripts/frozen-study-option-cuts.py"
    old_spec = importlib.util.spec_from_file_location("frozen_producer", old_producer_path)
    old_producer = importlib.util.module_from_spec(old_spec)
    old_spec.loader.exec_module(old_producer)
    output = args.output_directory
    if not output.is_absolute() or output.parent != BASE or output.exists():
        raise ValueError("fresh private research output required")
    adapter.private_directory(BASE)
    adapter.private_file(args.cuts)
    if (not args.binary.is_absolute() or args.binary.resolve() != args.binary
            or not args.binary.is_relative_to(BASE)):
        raise ValueError("private executable scope differs")
    adapter.private_directory(args.binary.parent)
    binary_info = args.binary.lstat()
    if (not stat.S_ISREG(binary_info.st_mode) or binary_info.st_uid != os.getuid()
            or binary_info.st_nlink != 1 or stat.S_IMODE(binary_info.st_mode) != 0o700):
        raise ValueError("private unprivileged executable required")
    proof, manifest, start, end = adapter.acquired_source(args.proof, args.ledger, root)
    if (proof["query"]["dataset"], proof["query"]["schema"]) != ("OPRA.PILLAR", "cmbp-1") or not 1 <= len(proof["query"]["symbols"]) <= 2000:
        raise ValueError("accepted bounded option-query universe required")
    fs = os.statvfs(BASE)
    if fs.f_bavail * fs.f_frsize < 16 << 30:
        raise ValueError("offline capacity reserve unavailable")
    audit_path = args.audit_directory / "audit.json"
    reference_path = args.audit_directory / "reference.json"
    for path in (audit_path, reference_path):
        adapter.private_file(path)
    audit, reference = read_json(audit_path), read_json(reference_path)
    if (len(audit["files"]) != 1 or audit["source_gate_passed"] is not False
            or audit["schema_version"] != "AutoWheelNativeBookSourceAuditV1"
            or audit["decoder_version"] != "0.71.0" or audit["upgrade_policy"] != "AS_IS"
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
            or any(audit["query_" + key] != query[key] or reference["query_" + key] != query[key]
                   for key in ("dataset", "schema", "start", "end"))):
        raise ValueError("acquired scope differs")
    cuts_document = read_json(args.cuts)
    cuts, cut_sources = old_producer.frozen_cuts(cuts_document, query, start, end, root)
    if cuts_document["decision_cuts"] != EXPECTED_DECISIONS or len(cuts) != EXPECTED_DECISIONS + 1:
        raise ValueError("frozen decision count differs")
    decisions = cuts[:EXPECTED_DECISIONS]
    if decisions[0] + EXPECTED_WINDOWS * ONE_SEC_NANOS != cuts[-1]:
        raise ValueError("first-decision to closing GFD scope differs")
    windows = [{"From": decisions[0] + k * ONE_SEC_NANOS,
                "Through": decisions[0] + (k + 1) * ONE_SEC_NANOS}
               for k in range(EXPECTED_WINDOWS)]
    if len(windows) != EXPECTED_WINDOWS:
        raise ValueError("execution window count differs")
    for entry in windows:
        if (type(entry["From"]) is not int or type(entry["Through"]) is not int
                or entry["Through"] - entry["From"] != ONE_SEC_NANOS):
            raise ValueError("execution window shape differs")
    for idx in range(1, len(windows)):
        if not windows[idx - 1]["Through"] <= windows[idx]["From"]:
            raise ValueError("execution windows overlap or unordered")
    if windows[0]["From"] != decisions[0] or windows[-1]["Through"] != cuts[-1]:
        raise ValueError("complete GFD endpoints differ")
    for decision in decisions:
        if (decision - decisions[0]) % ONE_SEC_NANOS or not decisions[0] <= decision < cuts[-1]:
            raise ValueError("frozen decision absent from GFD grid")
    source = Path(proof["source_directory"]) / member["source_filename"]
    adapter.private_file(source)
    if (file_hash(source) != member["source_compressed_sha256"]
            or reference["vector_jsonl_sha256"] != member["vector_jsonl_sha256"]):
        raise ValueError("complete native source differs")
    reducer_path = root / "internal/causalbook/intervals.go"
    stream_path = root / "internal/nativebook/stream.go"
    cli_path = root / "research/simulation/2026-10-05/cycle58-selected-execution-reference/main.go"
    relevant = list(dict.fromkeys([Path(p) if Path(p).is_absolute() else root / p for p in manifest]
                                 + [root / p for p in cut_sources]
                                 + [args.proof, args.ledger, audit_path, reference_path, args.binary, args.cuts,
                                    Path(__file__).resolve(), adapter_path, old_producer_path,
                                    reducer_path, root / "internal/causalbook/selected_intervals.go", root / "internal/causalbook/observe.go", stream_path, cli_path]))
    before = {p: file_hash(p) for p in relevant}
    output.mkdir(mode=0o700)
    adapter.private_directory(output)
    partial = output / "intervals.jsonl.gz.partial"
    stage = output / "intervals.jsonl.gz"
    report_path = output / "report.json"
    fd = os.open(source, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    processes = []
    executable_fd = None
    committed = False
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600
                or info.st_uid != os.getuid() or info.st_nlink != 1):
            raise ValueError("private native descriptor differs")
        with os.fdopen(os.dup(fd), "rb") as checked:
            if hashlib.file_digest(checked, "sha256").hexdigest() != before[source]:
                raise ValueError("native descriptor differs")
            checked.seek(0)
        zstd = subprocess.Popen(["/usr/bin/zstd", "-q", "-f", "-d", "-c", "--", "/dev/fd/" + str(fd)],
                                pass_fds=(fd,), stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        processes.append(zstd)
        prefix = zstd.stdout.read(8)
        if len(prefix) != 8 or prefix[:3] != b"DBN" or prefix[3] not in (1, 2, 3):
            raise ValueError("native prelude differs")
        metadata_length = int.from_bytes(prefix[4:], "little")
        if not 100 <= metadata_length <= (16 << 20) - 8:
            raise ValueError("native metadata bound")
        header = prefix + zstd.stdout.read(metadata_length)
        if len(header) != metadata_length + 8:
            raise ValueError("native metadata truncated")
        decoder = dbn.DBNDecoder(upgrade_policy=dbn.VersionUpgradePolicy.AS_IS, compression=dbn.Compression.NONE)
        decoded = decoder.write_and_decode(header)
        if decoder.buffer() or len(decoded) != 1 or not isinstance(decoded[0], dbn.Metadata):
            raise ValueError("native metadata decode differs")
        metadata = decoded[0]
        if (metadata.dataset != query["dataset"] or str(metadata.schema) != query["schema"]
                or metadata.start != start or metadata.end != end or metadata.limit not in (None, 0)
                or str(metadata.stype_in) != "raw_symbol" or str(metadata.stype_out) != "instrument_id"
                or metadata.version != member["native_version"] or type(metadata.ts_out) is not bool
                or not adapter.metadata_symbols_match(list(metadata.symbols), query["symbols"])):
            raise ValueError("native query scope differs")
        day = datetime.fromtimestamp(start // 1000000000, timezone.utc).date()
        _, reverse = adapter.active_mappings(metadata, day, set(query["symbols"]))
        expected = dict(Schema=query["schema"], Version=metadata.version, TsOut=metadata.ts_out,
                        Records=member["records"], NativeBytes=member["source_uncompressed_bytes"],
                        MetadataBytes=len(header), NativeSHA256=list(bytes.fromhex(member["source_uncompressed_sha256"])),
                        MetadataSHA256=list(hashlib.sha256(header).digest()))
        plan = control_plan(windows, reverse)
        selected_count = sum(len(entry["Instruments"]) for entry in plan)
        plan_path = output / "selection-plan.json"
        adapter.write_json(plan_path, dict(schema="AutoWheelSelectedOptionControlPlanV1",
            control_only=True, causal_selection_qualified=False, selected_windows=plan))
        plan_sha = file_hash(plan_path)
        descriptor = dict(expected=expected, scope=dict(Dataset=query["dataset"], UTCStart=start, Instruments=sorted(reverse)),
                          selected_windows=plan, max_age_nanos=cuts_document["max_age_nanos"], mappings=reverse)
        if type(descriptor["max_age_nanos"]) is not int:
            raise ValueError("max age shape differs")
        with tempfile.TemporaryDirectory(prefix="descriptor-", dir=output) as temp:
            descriptor_path = Path(temp) / "expected.json"
            adapter.write_json(descriptor_path, descriptor)
            stage_fd = os.open(partial, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            error_path = Path(temp) / "producer-error.txt"
            error_fd = os.open(error_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            try:
                executable_fd = os.open(args.binary, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
                executable_info = os.fstat(executable_fd)
                if (not stat.S_ISREG(executable_info.st_mode) or executable_info.st_uid != os.getuid()
                        or executable_info.st_nlink != 1 or stat.S_IMODE(executable_info.st_mode) != 0o700):
                    raise ValueError("private executable descriptor differs")
                with os.fdopen(os.dup(executable_fd), "rb") as checked:
                    if hashlib.file_digest(checked, "sha256").hexdigest() != before[args.binary]:
                        raise ValueError("executable descriptor differs")
                go = subprocess.Popen(["/proc/self/fd/" + str(executable_fd), str(descriptor_path), str(stage_fd)],
                                      pass_fds=(stage_fd, executable_fd), stdin=subprocess.PIPE,
                                      stdout=subprocess.PIPE, stderr=error_fd)
                processes.append(go)
                go.stdin.write(header)
                for raw in iter(lambda: zstd.stdout.read(1 << 20), b""):
                    go.stdin.write(raw)
                go.stdin.close()
                go.stdin = None
                result_bytes, _ = go.communicate()
                if zstd.wait() != 0 or go.returncode != 0 or len(result_bytes) > 16384:
                    raise ValueError("complete native interval stream differs")
            finally:
                os.close(stage_fd)
                os.close(error_fd)
                if error_path.exists():
                    error_bytes = error_path.read_bytes()
                    permitted = {("offline execution interval producer failed: " + code + "\n").encode()
                                 for code in ("io_or_descriptor", "scope", "record_semantics", "capture_order_or_window",
                                              "expected_evidence", "native_integrity")}
                    if error_bytes in permitted:
                        import sys
                        sys.stderr.buffer.write(error_bytes)
        def reject_constant(_):
            raise ValueError("nonfinite producer result")
        result = json.loads(result_bytes, parse_constant=reject_constant)
        expected_keys = {"schema", "records_processed", "native_bytes", "native_sha256",
                         "metadata_sha256", "windows", "observations", "source_universe_instruments",
                         "output_projection", "causal_selection_qualified",
                         "continuous_pending_order_coverage_qualified", "raw_valid_observations",
                         "receive_fresh_observations", "event_fresh_observations",
                         "minimum_present_observations", "interval_jsonl_sha256", "retimed_rows",
                         "full_source_qualification", "replay_frames_produced", "historical_performance_available"}
        if not isinstance(result, dict) or set(result) != expected_keys:
            raise ValueError("producer result shape differs")
        header_sha = hashlib.sha256(header).hexdigest()
        instrument_count = len(reverse)
        for key in ("raw_valid_observations", "receive_fresh_observations",
                    "event_fresh_observations", "minimum_present_observations"):
            value = result.get(key)
            if type(value) is not int or not 0 <= value <= selected_count:
                raise ValueError("interval observation bound differs")
        if not (result["event_fresh_observations"] <= result["receive_fresh_observations"]
                <= result["raw_valid_observations"] <= result["minimum_present_observations"]):
            raise ValueError("interval fact totals inconsistent")
        if any(type(result[key]) is not int for key in ("records_processed", "native_bytes", "windows", "observations")):
            raise ValueError("producer numeric type differs")
        interval_hash = result.get("interval_jsonl_sha256")
        if (not isinstance(interval_hash, str) or len(interval_hash) != 64
                or any(c not in "0123456789abcdef" for c in interval_hash)):
            raise ValueError("interval payload hash differs")
        if (result.get("schema") != "AutoWheelSelectedNativeExecutionIntervalV1"
                or result.get("records_processed") != member["records"]
                or result.get("native_bytes") != member["source_uncompressed_bytes"]
                or result.get("native_sha256") != member["source_uncompressed_sha256"]
                or result.get("metadata_sha256") != header_sha
                or result.get("source_universe_instruments") != instrument_count
                or result.get("output_projection") != "selected_native_instrument_ids"
                or result.get("causal_selection_qualified") is not False
                or result.get("continuous_pending_order_coverage_qualified") is not False
                or result.get("windows") != EXPECTED_WINDOWS
                or result.get("observations") != selected_count
                or result.get("full_source_qualification") is not False
                or result.get("replay_frames_produced") is not False
                or result.get("historical_performance_available") is not False
                or result.get("retimed_rows") != 0
                or type(result.get("retimed_rows")) is not int):
            raise ValueError("complete totals or source stability differs")
        adapter.private_file(partial)
        payload_fd = os.open(partial, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        try:
            payload_info = os.fstat(payload_fd)
            if (not stat.S_ISREG(payload_info.st_mode) or payload_info.st_uid != os.getuid()
                    or stat.S_IMODE(payload_info.st_mode) != 0o600 or payload_info.st_nlink != 1
                    or payload_info.st_size <= 0):
                raise ValueError("private payload descriptor differs")
            plain_hash, plain_bytes = hashlib.sha256(), 0
            with os.fdopen(os.dup(payload_fd), "rb") as checked:
                with gzip.GzipFile(fileobj=checked) as zipped:
                    for raw in iter(lambda: zipped.read(1 << 20), b""):
                        plain_bytes += len(raw)
                        if plain_bytes > selected_count * 4096 + EXPECTED_WINDOWS * 512:
                            raise ValueError("interval expansion bound exceeded")
                        plain_hash.update(raw)
            if (plain_bytes == 0 or plain_hash.hexdigest() != result["interval_jsonl_sha256"]
                    or fingerprint(os.fstat(payload_fd)) != fingerprint(payload_info)):
                raise ValueError("complete plaintext payload differs")
        finally:
            os.close(payload_fd)
        if (fingerprint(os.fstat(fd)) != fingerprint(info)
                or fingerprint(os.fstat(executable_fd)) != fingerprint(executable_info)
                or any(file_hash(p) != digest for p, digest in before.items())
                or file_hash(plan_path) != plan_sha):
            raise ValueError("source or descriptor stability differs")
        os.rename(partial, stage)
        result.update(observed_at=datetime.now(timezone.utc).isoformat(), query_dataset=query["dataset"], query_schema=query["schema"],
                      acquisition_proof_sha256=before[args.proof], audit_sha256=before[audit_path],
                      independent_native_reference_sha256=before[reference_path], cuts_sha256=before[args.cuts],
                      old_producer_sha256=before[old_producer_path],
                      selected_reducer_sha256=before[root / "internal/causalbook/selected_intervals.go"],
                      selected_plan_sha256=plan_sha, selected_plan_file=plan_path.name,
                      mask_policy="Clock-driven rotating one-to-three-instrument control; not a strategy selection.",
                      reducer_sha256=before[reducer_path], raw_state_sha256=before[root / "internal/causalbook/observe.go"],
                      native_stream_sha256=before[stream_path],
                      cli_sha256=before[cli_path],
                      producer_sha256=before[Path(__file__).resolve()],
                      binary_sha256=before[args.binary], interval_compressed_sha256=file_hash(stage),
                      interval_compressed_bytes=stage.stat().st_size, interval_file=stage.name,
                      interval_plain_bytes=plain_bytes, gzip_eof_verified=True, actual_native_pass_complete=True,
                      window_policy="23100 adjacent one-second control windows from the frozen session first decision through close; every original native record is checked, with selected output only.",
                      limitations=["Rotating mask control only; not causal strategy selection or pending-order coverage.",
                                   "Dated terms, roster, Greeks, lifecycle and full126 execution remain unqualified.",
                                   "No fill, continuous execution, terms, Greeks, lifecycle, or performance qualification is asserted."])
        adapter.write_json(report_path, result)
        directory_fd = os.open(output, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
        committed = True
        print(json.dumps(result))
    finally:
        for process in processes:
            if process.poll() is None:
                process.kill()
                process.wait()
            for stream in (process.stdin, process.stdout, process.stderr):
                if stream is not None:
                    stream.close()
        if executable_fd is not None:
            os.close(executable_fd)
        os.close(fd)
        if not committed:
            for owned_path in (partial, stage, report_path):
                if owned_path.exists():
                    owned_path.unlink()


if __name__ == "__main__":
    try:
        main()
    except Exception:
        import sys
        print("private execution interval producer failed closed", file=sys.stderr)
        raise SystemExit(1)
