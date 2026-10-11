#!/usr/bin/env python3
"""Full-native option book candidate pass and independent closed-capture comparison.

Original licensed records and candidates remain private on MSI. Raw sizes retain their native units; opaque reserved bytes are not in the capture projection and remain bound by the original native hash. No provider,
broker, replay, qualification or financial requests. The clock policy and
subcent rejections are explicit; no rejected price or time is repaired.
"""
import argparse
from datetime import datetime, timezone
import gzip
import hashlib
import importlib.metadata
import json
from pathlib import Path
import os
import subprocess
import sys

from batch_acquisition_proof import file_hash, read_json
import importlib.util


def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


ROOT = Path(__file__).resolve().parents[1]
BASE = Path("/home/yuduo/Projects/AutoWheel-research")
POLICY = "raw_book_receive_with_event_age_v1"
SECOND = 1_000_000_000
NANOS_PER_CENT = 10_000_000


def rfc3339(nanos):
    seconds, fraction = divmod(nanos, SECOND)
    suffix = ("."+f"{fraction:09d}".rstrip("0")) if fraction else ""
    return datetime.fromtimestamp(seconds, timezone.utc).strftime("%Y-%m-%dT%H:%M:%S")+suffix+"Z"


def independent(row, cut, age, reverse, source_hash, day):
    ident, has_book, book, available, has_trade, trade, pending, status, has_update, last_update, covered, zero_updates = row
    clocks = [book[5], book[6], available] if has_book else None
    venues = set(range(20, 38)) - {30} | {61, 108, 109}
    if (not has_book or not has_update or pending or status != "fresh_raw_book" or not covered or zero_updates
            or book[4] != ident or book[1] not in (1, 2, 3) or book[18] != 177 or book[3] != 30
            or book[10] not in ("A", "C", "M") or book[11] not in ("A", "B", "N")
            or book[12] & ~(128|64|1) or book[12] & (128|64) != (128|64)
            or book[16] in (0, (1<<32)-1) or book[17] in (0, (1<<32)-1)
            or book[14] > book[15] or book[19] not in venues or book[20] not in venues or last_update != book[5]):
        return [ident, "book", None, clocks]
    recv, event = book[5], book[6]
    if recv < day or available < recv or available > cut or recv > cut or not event or event > recv or cut-recv > age or cut-event > age:
        return [ident, "clock", None, clocks]
    bid, ask = book[14], book[15]
    if any(price <= 0 or price == (1<<63)-1 or price%NANOS_PER_CENT for price in (bid, ask)):
        return [ident, "price", None, clocks]
    return [ident, "accepted", dict(raw_symbol=reverse[ident], instrument_id=ident, source_sha256=source_hash,
                                   clock_policy=POLICY, at=rfc3339(recv), bid_cents=bid//NANOS_PER_CENT,
                                   ask_cents=ask//NANOS_PER_CENT, raw_bid_size=book[16], raw_ask_size=book[17],
                                   raw_bid_publisher=book[19], raw_ask_publisher=book[20], book_receive_at=recv,
                                   book_event_at=event, book_available_at=available, observed_at=cut, original_vector=book), clocks]


def same_json(left, right):
    return json.dumps(left, separators=(",", ":"), sort_keys=True, allow_nan=False) == json.dumps(right, separators=(",", ":"), sort_keys=True, allow_nan=False)



def large_capture_binding(producer, reference, expected):
    for document in (producer, reference):
        if any(document.get(key) != value for key, value in expected.items()):
            raise ValueError("large-history capture admission provenance differs")
        if any(document.get(key) is not False for key in
               ("full_source_qualification", "replay_frames_produced", "historical_performance_available")):
            raise ValueError("capture must remain source-only evidence")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("proof", "ledger", "audit-directory", "cuts", "capture-directory", "binary", "output-directory"):
        parser.add_argument("--"+name, type=Path, required=True)
    for name in ("decision-plan", "native-reference", "capture-reference"):
        parser.add_argument("--"+name, type=Path, required=True)
    args = parser.parse_args()
    if importlib.metadata.version("databento-dbn") != "0.71.0":
        raise ValueError("pinned AS_IS decoder required")
    import databento_dbn as dbn
    adapter_path = ROOT/"scripts/audit-native-book-batch.py"
    common_path = ROOT/"scripts/verify-native-execution-intervals.py"
    old_path = ROOT/"scripts/verify-closed-window-option-captures-cycle187.py"
    adapter, common, old = module("option_book_candidate_adapter", adapter_path), module("option_book_candidate_common", common_path), module("option_book_candidate_reference", old_path)
    for directory in (BASE, args.audit_directory, args.capture_directory, args.binary.parent):
        adapter.private_directory(directory)
    if (args.output_directory.exists() or not args.output_directory.is_relative_to(BASE)
            or args.output_directory.parent != BASE or not args.binary.is_relative_to(BASE)):
        raise ValueError("fresh private output scope required")
    from native_closed_window_admission_cycle187 import ADMISSION_SCHEMA, admit_source
    admitted = admit_source(args.proof, args.ledger, args.decision_plan,
                            args.audit_directory, args.native_reference, ROOT)
    proof, manifest = admitted.book.proof, admitted.book.manifest
    start, end = admitted.book.start_ns, admitted.book.end_ns
    query = proof["query"]
    audit_path, native_reference_path = admitted.audit_path, admitted.reference_path
    audit, native_ref = admitted.audit, admitted.reference
    member = audit["files"][0]
    source = Path(proof["source_directory"])/member["source_filename"]
    captures, capture_report_path = (args.capture_directory/name for name in ("snapshots.jsonl.gz", "report.json"))
    capture_ref_path = args.capture_reference
    capture_report, capture_ref = read_json(capture_report_path), read_json(capture_ref_path)
    large_capture_binding(capture_report, capture_ref, dict(
        admission_schema=ADMISSION_SCHEMA, financial_snapshot_sha256=file_hash(args.ledger),
        decision_plan_sha256=file_hash(args.decision_plan),
        admission_script_sha256=file_hash(ROOT/"scripts/native_closed_window_admission_cycle187.py"),
        producer_sha256=file_hash(ROOT/"scripts/produce-closed-window-option-captures-cycle187.py")))
    cuts_doc = read_json(args.cuts)
    cuts, cut_sources = old._frozen_cuts(cuts_doc, query, start, end, ROOT)
    original_binary = BASE/"cycle187-closed-window-tools-v1-20261009/closed-window-producer"
    if (capture_ref["schema_version"] != "AutoWheelClosedWindowNativeCausalObservationVerificationV1"
            or capture_ref["reference_script_sha256"] != file_hash(old_path)
            or capture_ref["producer_report_sha256"] != file_hash(capture_report_path)
            or capture_ref["acquisition_proof_sha256"] != file_hash(args.proof)
            or capture_ref["audit_sha256"] != file_hash(audit_path)
            or capture_ref["independent_native_reference_sha256"] != file_hash(native_reference_path)
            or capture_ref["records_reconstructed"] != member["records"]
            or capture_ref["snapshots_compared"] != 1 or capture_ref["producer_snapshots_match"] is not True
            or capture_ref["producer_counters_match"] is not True
            or capture_ref["snapshot_compressed_sha256"] != file_hash(captures)
            or capture_ref["snapshot_compressed_bytes"] != captures.stat().st_size
            or capture_ref["snapshot_jsonl_sha256"] != capture_report["snapshot_jsonl_sha256"]
            or capture_ref["binary_sha256"] != file_hash(original_binary)
            or capture_report["binary_sha256"] != capture_ref["binary_sha256"]
            or capture_ref["cuts_sha256"] != file_hash(args.cuts)
            or capture_ref["vector_jsonl_sha256"] != member["vector_jsonl_sha256"]
            or capture_ref["full_source_qualification"] is not False):
        raise ValueError("completed independent capture binding differs")
    code = [Path(__file__).resolve(), adapter_path, common_path, old_path, ROOT/"scripts/produce-closed-window-option-captures-cycle187.py",
            ROOT/"internal/replaynormalize/option.go", ROOT/"research/simulation/2026-10-05/cycle187-closed-window-normalizer/main.go",
            ROOT/"internal/causalwindow/observe.go", ROOT/"internal/nativebook/stream.go", ROOT/"internal/replaynormalize/oi.go",
            ROOT/"internal/replaynormalize/stock.go"]
    relevant = list(dict.fromkeys([Path(path) if Path(path).is_absolute() else ROOT/path for path in manifest] + list(admitted.extra_paths)
                                 + [ROOT/path for path in cut_sources] + code + [args.proof, args.ledger, args.cuts,
                                    audit_path, native_reference_path, source, captures, capture_report_path,
                                    capture_ref_path, args.binary, original_binary]))
    for path in (args.proof, args.ledger, args.cuts, audit_path, native_reference_path, source, captures, capture_report_path, capture_ref_path):
        adapter.private_file(path)
    for path in (args.binary, original_binary):
        with common.private_input(path, 0o700):
            pass
    before = {path: file_hash(path) for path in relevant}
    if before[source] != member["source_compressed_sha256"]:
        raise ValueError("original compressed option-book source differs")
    args.output_directory.mkdir(mode=0o700)
    descriptor_path = args.output_directory/"descriptor.json"
    stage, published = args.output_directory/"candidates.stage.jsonl.gz", args.output_directory/"option-book-candidates.jsonl.gz"
    decompressor, normalizer, stage_fd = None, None, None
    try:
        with common.private_input(source) as native:
            if hashlib.file_digest(native, "sha256").hexdigest() != before[source]:
                raise ValueError("source descriptor differs")
            native.seek(0)
            decompressor = subprocess.Popen(["/usr/bin/zstd", "-q", "-f", "-d", "-c", "--", "/dev/fd/"+str(native.fileno())],
                                             pass_fds=(native.fileno(),), stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
            stream = decompressor.stdout
            prefix = stream.read(8)
            if len(prefix) != 8 or prefix[:3] != b"DBN" or prefix[3] not in (1, 2, 3):
                raise ValueError("native prelude differs")
            size = int.from_bytes(prefix[4:], "little")
            if not 100 <= size <= (16<<20)-8:
                raise ValueError("metadata bound")
            header = prefix+stream.read(size)
            if len(header) != size+8:
                raise ValueError("metadata truncated")
            decoder = dbn.DBNDecoder(upgrade_policy=dbn.VersionUpgradePolicy.AS_IS, compression=dbn.Compression.NONE)
            values = decoder.write_and_decode(header)
            if decoder.buffer() or len(values) != 1 or not isinstance(values[0], dbn.Metadata):
                raise ValueError("metadata decode differs")
            metadata = values[0]
            if (metadata.dataset != query["dataset"] or str(metadata.schema) != query["schema"]
                    or metadata.start != start or metadata.end != end or metadata.limit not in (None, 0)
                    or str(metadata.stype_in) != "raw_symbol" or str(metadata.stype_out) != "instrument_id"
                    or metadata.version != member["native_version"] or type(metadata.ts_out) is not bool
                    or not adapter.metadata_symbols_match(list(metadata.symbols), query["symbols"])):
                raise ValueError("native metadata scope differs")
            _, reverse = adapter.active_mappings(metadata, datetime.fromtimestamp(start//SECOND, timezone.utc).date(), set(query["symbols"]))
            if not 1 <= len(reverse) <= 2000:
                raise ValueError("frozen option_book universe differs")
            descriptor = dict(expected=dict(Schema="cmbp-1", Version=metadata.version, TsOut=metadata.ts_out,
                                           Records=member["records"], NativeBytes=member["source_uncompressed_bytes"],
                                           MetadataBytes=len(header), NativeSHA256=list(bytes.fromhex(member["source_uncompressed_sha256"])),
                                           MetadataSHA256=list(hashlib.sha256(header).digest())),
                              scope=dict(Dataset="OPRA.PILLAR", UTCStart=cuts_doc["utc_start_ns"], SourceStart=start, SourceEnd=end, Instruments=sorted(reverse)),
                              cuts=cuts, max_age_nanos=cuts_doc["max_age_nanos"], mappings={str(key):value for key,value in reverse.items()})
            adapter.write_json(descriptor_path, descriptor)
            descriptor_sha = file_hash(descriptor_path)
            stage_fd = os.open(stage, os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW, 0o600)
            with common.private_input(args.binary, 0o700) as binary:
                if hashlib.file_digest(binary, "sha256").hexdigest() != before[args.binary]:
                    raise ValueError("binary descriptor differs")
                normalizer = subprocess.Popen(["/dev/fd/"+str(binary.fileno()), str(descriptor_path), str(stage_fd)],
                                              pass_fds=(binary.fileno(), stage_fd), stdin=subprocess.PIPE,
                                              stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                native_hash, native_bytes = hashlib.sha256(header), len(header)
                normalizer.stdin.write(header)
                while raw := stream.read(1<<20):
                    native_bytes += len(raw)
                    if native_bytes > member["source_uncompressed_bytes"]:
                        raise ValueError("native expansion exceeded")
                    native_hash.update(raw)
                    normalizer.stdin.write(raw)
                normalizer.stdin.close()
                footer = normalizer.stdout.read(16385)
                diagnostics = normalizer.stderr.read(1025)
                if normalizer.wait() != 0 or diagnostics or len(footer) > 16384 or decompressor.wait() != 0:
                    raise ValueError("native producer or decompressor failed")
                report = json.loads(footer)
            os.close(stage_fd)
            stage_fd = None
            if (native_bytes != member["source_uncompressed_bytes"] or native_hash.hexdigest() != member["source_uncompressed_sha256"]
                    or report["schema"] != "AutoWheelNativeOptionBookCandidateNormalizationV1" or report["records_processed"] != member["records"]
                    or report["native_bytes"] != native_bytes or report["native_sha256"] != native_hash.hexdigest()
                    or report["metadata_sha256"] != hashlib.sha256(header).hexdigest() or report["snapshots"] != 1
                    or report["observations"] != len(reverse) or report["accepted"]+report["rejected"] != len(reverse)
                    or report["clock_policy"] != POLICY or report["retimed_rows"] != 0 or report["prices_rounded"] != 0
                    or any(report[key] is not False for key in ("replay_frames_produced", "full_source_qualification", "historical_performance_available"))):
                raise ValueError("complete candidate producer contract differs")
        counts = dict(accepted=0, rejected=0)
        reasons = dict(book=0, clock=0, price=0)
        with common.private_input(captures) as capture_file, common.private_input(stage) as stage_file:
            with gzip.GzipFile(fileobj=capture_file) as originals, gzip.GzipFile(fileobj=stage_file) as candidates:
                original_packets, candidate_packets = common.Packets(originals), common.Packets(candidates)
                for cut in cuts:
                    original, candidate = original_packets.next(), candidate_packets.next()
                    if original[:4] != ["OPRA.PILLAR", cuts_doc["utc_start_ns"], cut, cuts_doc["max_age_nanos"]] or len(original) != 5 or len(original[4]) != len(reverse):
                        raise ValueError("closed capture scope differs")
                    normalized = []
                    for ident, row in zip(sorted(reverse), original[4], strict=True):
                        if len(row) != 12 or row[0] != ident:
                            raise ValueError("closed capture identity differs")
                        for present, vector in ((row[1], row[2]), (row[4], row[5])):
                            if present:
                                old._validate_vector(vector, vector[0], metadata.version, reverse, "cmbp-1", metadata.ts_out, start, end)
                            elif vector is not None:
                                raise ValueError("absent capture record has payload")
                        value = independent(row, cut, cuts_doc["max_age_nanos"], reverse, member["source_uncompressed_sha256"], cuts_doc["utc_start_ns"])
                        normalized.append(value)
                        reason = value[1]
                        counts["accepted" if reason=="accepted" else "rejected"] += 1
                        if reason != "accepted":
                            reasons[reason] += 1
                    expected = ["OPRA.PILLAR", cuts_doc["utc_start_ns"], cut, cuts_doc["max_age_nanos"], POLICY, normalized]
                    if not same_json(candidate, expected):
                        raise ValueError("independent candidate option-book packet differs")
                original_packets.eof()
                candidate_packets.eof()
                if (original_packets.hash.hexdigest() != capture_ref["snapshot_jsonl_sha256"]
                        or candidate_packets.hash.hexdigest() != report["candidate_jsonl_sha256"]
                        or any(report[key] != value for key,value in counts.items()) or report["rejection_reasons"] != reasons):
                    raise ValueError("complete candidate plaintext or counters differ")
        if any(file_hash(path) != value for path,value in before.items()) or file_hash(descriptor_path) != descriptor_sha:
            raise ValueError("source, descriptor or code changed")
        admitted.unchanged()
        compressed_sha, compressed_bytes = file_hash(stage), stage.stat().st_size
        os.rename(stage, published)
        adapter.sync_directory(args.output_directory)
        result = dict(schema="AutoWheelClosedWindowOptionBookCandidateNormalizerAuditV1",
                      admission_schema=ADMISSION_SCHEMA, financial_snapshot_sha256=before[args.ledger],
                      decision_plan_sha256=before[args.decision_plan],
                      admission_script_sha256=before[ROOT/"scripts/native_closed_window_admission_cycle187.py"], observed_at=datetime.now(timezone.utc).isoformat(),
                      script_sha256=before[Path(__file__).resolve()], normalizer_sha256=before[ROOT/"internal/replaynormalize/option.go"],
                      cli_sha256=before[ROOT/"research/simulation/2026-10-05/cycle187-closed-window-normalizer/main.go"], binary_sha256=before[args.binary],
                      acquisition_proof_sha256=before[args.proof], audit_sha256=before[audit_path],
                      independent_native_reference_sha256=before[native_reference_path], independent_capture_reference_sha256=before[capture_ref_path],
                      independent_capture_report_sha256=before[capture_report_path], cuts_sha256=before[args.cuts],
                      native_sha256=native_hash.hexdigest(), source_compressed_sha256=before[source],
                      candidate_compressed_sha256=compressed_sha, candidate_compressed_bytes=compressed_bytes,
                      candidate_jsonl_sha256=candidate_packets.hash.hexdigest(), descriptor_sha256=descriptor_sha,
                      records_processed=report["records_processed"], snapshots_compared=1, observations_compared=len(reverse), mapped_instruments=len(reverse),
                      accepted=counts["accepted"], rejected=counts["rejected"], rejection_reasons=reasons,
                      actual_native_pass_complete=True, independent_packets_match=True, independent_counters_match=True,
                      compressed_and_plaintext_eof_verified=True, clock_policy=POLICY, retimed_rows=0, prices_rounded=0,
                      source_start_ns=start,source_end_ns=end,closed_cut_ns=cuts[0],full_day_coverage=False,
                      full_source_qualification=False, replay_frames_produced=False, historical_performance_available=False,
                      limitations=["Candidate option_book values only; receipt identity is not a historical source qualification.",
                                   "The disclosed receive-clock policy also requires fresh original event clocks; neither clock is calibrated to broker timestamps.",
                                   "Subcent or invalid/stale books remain explicit blockers. No trade or previous quote substitutes for them.",
                                   "Full contract units, currency, tick rules, terms, dated venue roster, causal OI and Greeks, pending-order/GFD, lifecycle and complete frozen source coverage remain required."])
        adapter.write_json(args.output_directory/"report.json", result)
        print(json.dumps(result))
    finally:
        if stage_fd is not None:
            os.close(stage_fd)
        for process in (normalizer, decompressor):
            if process is not None:
                if process.poll() is None:
                    process.kill()
                    process.wait()
                for name in ("stdin", "stdout", "stderr"):
                    stream = getattr(process, name, None)
                    if stream is not None:
                        stream.close()


if __name__ == "__main__":
    try:
        main()
    except Exception:
        print("option_book candidate normalizer audit failed closed", file=sys.stderr)
        raise SystemExit(1)
