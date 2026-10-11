#!/usr/bin/env python3
"""Complete acquired DBN/Go comparison against a verified pinned vector digest."""
import argparse
from datetime import datetime, timezone
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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--proof", type=Path, required=True)
    parser.add_argument("--ledger", type=Path, required=True)
    parser.add_argument("--audit-directory", type=Path, required=True)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if importlib.metadata.version("databento-dbn") != "0.71.0" or args.output.exists():
        raise ValueError("pinned decoder and fresh output required")
    import databento_dbn as dbn
    root = Path(__file__).resolve().parents[1]
    adapter_path = root / "scripts/audit-native-book-batch.py"
    spec = importlib.util.spec_from_file_location("native_adapter", adapter_path)
    adapter = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(adapter)
    adapter.private_directory(args.output.parent)
    info = args.binary.lstat()
    if (not args.binary.is_absolute() or args.binary.resolve() != args.binary or not stat.S_ISREG(info.st_mode)
            or info.st_uid != os.getuid() or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o700):
        raise ValueError("private unprivileged executable required")
    proof, manifest, start, end = adapter.acquired_source(args.proof, args.ledger, root)
    audit_path, reference_path = args.audit_directory / "audit.json", args.audit_directory / "reference.json"
    for path in (audit_path, reference_path):
        adapter.private_file(path)
    audit, reference = read_json(audit_path), read_json(reference_path)
    if (len(audit["files"]) != 1 or audit["source_gate_passed"] is not False
            or audit["schema_version"] != "AutoWheelNativeBookSourceAuditV1"
            or audit["decoder_version"] != "0.71.0" or audit["upgrade_policy"] != "AS_IS"
            or audit["operation_id"] != proof["reservation"]["operation_id"]
            or audit["query_hash"] != proof["reservation"]["query_hash"]
            or reference["schema_version"] != "AutoWheelNativeBookVectorReferenceV1"
            or reference["audit_sha256"] != file_hash(audit_path)
            or reference["receipt_sha256"] != proof["receipt_sha256"]
            or reference["reference_script_sha256"] != file_hash(root / "scripts/verify-native-book-vectors.py")
            or reference["records_compared_field_for_field"] != audit["records"]
            or reference["full_source_qualification"] is not False):
        raise ValueError("independent reference binding differs")
    member = audit["files"][0]
    query = proof["query"]
    if (audit["job_id"] != proof["job_id"] or reference["queried_symbols"] != len(query["symbols"])
            or any(audit["query_" + key] != query[key] or reference["query_" + key] != query[key]
                   for key in ("dataset", "schema", "start", "end"))):
        raise ValueError("acquired scope differs")
    source = Path(proof["source_directory"]) / member["source_filename"]
    adapter.private_file(source)
    if (file_hash(source) != member["source_compressed_sha256"]
            or reference["vector_jsonl_sha256"] != member["vector_jsonl_sha256"]):
        raise ValueError("source/reference digest differs")
    relevant = list(dict.fromkeys([Path(p) if Path(p).is_absolute() else root / p for p in manifest] +
                                [args.proof, args.ledger, audit_path, reference_path, args.binary,
                                 Path(__file__).resolve(), adapter_path, root / "internal/nativebook/stream.go",
                                 root / "research/simulation/2026-10-05/cycle29-native-stream-reference/main.go"]))
    before = {path: file_hash(path) for path in relevant}
    fd = os.open(source, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    processes = []
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1
                or stat.S_IMODE(info.st_mode) != 0o600 or info.st_size != source.stat().st_size):
            raise ValueError("source descriptor differs")
        with os.fdopen(os.dup(fd), "rb") as checked:
            digest = hashlib.file_digest(checked, "sha256").hexdigest()
            checked.seek(0)
        if digest != before[source]:
            raise ValueError("source descriptor digest differs")
        zstd = subprocess.Popen(["/usr/bin/zstd", "-q", "-f", "-d", "-c", "--", "/dev/fd/" + str(fd)],
                                pass_fds=(fd,), stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        processes.append(zstd)
        prefix = zstd.stdout.read(8)
        if len(prefix) != 8 or prefix[:3] != b"DBN" or prefix[3] not in (1, 2, 3):
            raise ValueError("native prelude differs")
        metadata_length = int.from_bytes(prefix[4:], "little")
        if not 0 < metadata_length <= (16 << 20) - 8:
            raise ValueError("metadata bound")
        header = prefix + zstd.stdout.read(metadata_length)
        if len(header) != metadata_length + 8:
            raise ValueError("metadata truncated")
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
            raise ValueError("native scope differs")
        day = datetime.fromtimestamp(start // 1000000000, timezone.utc).date()
        _, reverse = adapter.active_mappings(metadata, day, set(query["symbols"]))
        expected = dict(Schema=query["schema"], Version=metadata.version, TsOut=metadata.ts_out,
                        Records=member["records"], NativeBytes=member["source_uncompressed_bytes"],
                        MetadataBytes=len(header), NativeSHA256=list(bytes.fromhex(member["source_uncompressed_sha256"])),
                        MetadataSHA256=list(hashlib.sha256(header).digest()))
        with tempfile.TemporaryDirectory(prefix="native-stream-", dir=args.output.parent) as temp:
            descriptor = Path(temp) / "expected.json"
            adapter.write_json(descriptor, dict(expected=expected, mappings=reverse,
                                                vector_sha256=member["vector_jsonl_sha256"]))
            go = subprocess.Popen([str(args.binary), str(descriptor)], stdin=subprocess.PIPE,
                                  stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
            processes.append(go)
            go.stdin.write(header)
            for raw in iter(lambda: zstd.stdout.read(1 << 20), b""):
                go.stdin.write(raw)
            go.stdin.close()
            go.stdin = None
            result_bytes, _ = go.communicate()
            if zstd.wait() != 0 or go.returncode != 0 or len(result_bytes) > 16384:
                raise ValueError("complete native/Go stream differs")
        result = json.loads(result_bytes)
        if (result["schema"] != "AutoWheelGoNativeBookStreamReferenceV1"
                or result["records_compared_field_for_field"] != member["records"]
                or result["native_sha256"] != member["source_uncompressed_sha256"]
                or result["vector_jsonl_sha256"] != reference["vector_jsonl_sha256"]
                or result["full_source_qualification"] is not False
                or any(file_hash(path) != digest for path, digest in before.items())):
            raise ValueError("reference totals/input stability differs")
        result.update(observed_at=datetime.now(timezone.utc).isoformat(),
                      vector_fields_per_record=21 if query["schema"] == "cmbp-1" else 23,
                      pinned_decoder_version="0.71.0", acquisition_proof_sha256=before[args.proof],
                      independent_python_reference_sha256=before[reference_path],
                      source_code_sha256=before[root / "internal/nativebook/stream.go"],
                      harness_sha256=before[Path(__file__).resolve()], binary_sha256=before[args.binary],
                      query_dataset=query["dataset"], query_schema=query["schema"])
        adapter.write_json(args.output, result)
        print(json.dumps(result))
    finally:
        for process in processes:
            if process.poll() is None:
                process.kill()
                process.wait()
            for stream in (process.stdin, process.stdout, process.stderr):
                if stream is not None:
                    stream.close()
        os.close(fd)


if __name__ == "__main__":
    try:
        main()
    except Exception:
        import sys
        print("Go native stream reference failed", file=sys.stderr)
        raise SystemExit(1)
