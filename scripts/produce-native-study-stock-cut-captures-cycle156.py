#!/usr/bin/env python3
"""Produce private raw as-of observations from a complete acquired native file.

No provider calls, financial journal changes, source gates, replay frames or P&L.
"""
import argparse
from datetime import datetime, timedelta, timezone
import hashlib
import importlib.metadata
import importlib.util
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile
from zoneinfo import ZoneInfo

from batch_acquisition_proof import file_hash, read_json

BASE = Path("/home/yuduo/Projects/AutoWheel-research")


def frozen_cuts(document, query, start, end, root):
    """Use the preserved full-stock calendar; observation reduction is unchanged."""
    path = root / "scripts/frozen-study-stock-cuts.py"
    spec = importlib.util.spec_from_file_location("stock_cut_admission", path)
    helper = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(helper)
    cuts, paths = helper.frozen_cuts(document, query, start, end, root)
    return cuts, paths + ["scripts/produce-native-causal-observations.py"]


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
    cuts, cut_sources = frozen_cuts(cuts_document, query, start, end, root)
    source = Path(proof["source_directory"]) / member["source_filename"]
    adapter.private_file(source)
    if (file_hash(source) != member["source_compressed_sha256"]
            or reference["vector_jsonl_sha256"] != member["vector_jsonl_sha256"]):
        raise ValueError("complete native source differs")
    relevant = list(dict.fromkeys([Path(p) if Path(p).is_absolute() else root / p for p in manifest]
                                 + [root / p for p in cut_sources]
                                 + [args.proof, args.ledger, audit_path, reference_path, args.binary, args.cuts,
                                    Path(__file__).resolve(), adapter_path, root / "internal/causalbook/observe.go",
                                    root / "internal/nativebook/stream.go",
                                    root / "research/simulation/2026-10-05/cycle31-causal-reference/main.go"]))
    before = {p: file_hash(p) for p in relevant}
    output.mkdir(mode=0o700)
    adapter.private_directory(output)
    partial = output / "snapshots.jsonl.gz.partial"
    stage = output / "snapshots.jsonl.gz"
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
        descriptor = dict(expected=expected, scope=dict(Dataset=query["dataset"], UTCStart=start, Instruments=sorted(reverse)),
                          cuts=cuts, max_age_nanos=cuts_document["max_age_nanos"], mappings=reverse)
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
                    raise ValueError("complete native observation stream differs")
            finally:
                os.close(stage_fd)
                os.close(error_fd)
                if error_path.exists():
                    error_bytes = error_path.read_bytes()
                    permitted = {("offline causal observation producer failed: " + code + "\n").encode()
                                 for code in ("io_or_descriptor", "scope", "record_semantics", "capture_order_or_window",
                                              "expected_evidence", "native_integrity")}
                    if error_bytes in permitted:
                        import sys
                        sys.stderr.buffer.write(error_bytes)
        result = json.loads(result_bytes)
        if (result["schema"] != "AutoWheelNativeCausalObservationV1" or result["records_processed"] != member["records"]
                or result["native_sha256"] != member["source_uncompressed_sha256"]
                or result["snapshots"] != len(cuts) or result["observations"] != len(cuts) * len(reverse)
                or result["full_source_qualification"] is not False
                or any(file_hash(p) != digest for p, digest in before.items())):
            raise ValueError("complete totals or source stability differs")
        adapter.private_file(partial)
        os.rename(partial, stage)
        result.update(observed_at=datetime.now(timezone.utc).isoformat(), query_dataset=query["dataset"], query_schema=query["schema"],
                      acquisition_proof_sha256=before[args.proof], audit_sha256=before[audit_path],
                      independent_native_reference_sha256=before[reference_path], cuts_sha256=before[args.cuts],
                      reducer_sha256=before[root / "internal/causalbook/observe.go"], producer_sha256=before[Path(__file__).resolve()],
                      binary_sha256=before[args.binary], snapshot_compressed_sha256=file_hash(stage),
                      snapshot_compressed_bytes=stage.stat().st_size, snapshot_file=stage.name)
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
        # Only this invocation's uncommitted, freshly created staging is discarded.
        if not committed:
            for owned_path in (partial, stage, report_path):
                if owned_path.exists():
                    owned_path.unlink()


if __name__ == "__main__":
    try:
        main()
    except Exception:
        import sys
        print("private causal observation producer failed closed", file=sys.stderr)
        raise SystemExit(1)
