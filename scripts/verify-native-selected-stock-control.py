#!/usr/bin/env python3
"""Independent full-native reconstruction of the selected stock projection control.

Retains original acquisition, metadata, mappings, compressed/native hashes, EOF
and source stability checks. Controls do not establish causal strategy masks."""
import argparse
from datetime import datetime, timezone
import gzip
import hashlib
import importlib.metadata
import json
from pathlib import Path
import subprocess
import sys

from batch_acquisition_proof import file_hash, read_json

# Explicit loading allows the hyphenated research scripts to keep their stable
# source hashes. Do not modify the accepted option reference for stock support.
import importlib.util


def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


ROOT = Path(__file__).resolve().parents[1]
COMMON_PATH = ROOT / "scripts/verify-native-execution-intervals.py"
OLD_PATH = ROOT / "scripts/verify-native-causal-observations.py"
common = module("stock_interval_common", COMMON_PATH)
old = module("stock_interval_status", OLD_PATH)
CUTS_PATH = ROOT / "scripts/frozen-study-stock-cuts.py"
cut_interface = module("stock_study_cut_interface", CUTS_PATH)
SELECTED_PATH = ROOT / "scripts/selected-native-interval-reference.py"
selected_reference = module("selected_stock_reconstruction", SELECTED_PATH)






def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("proof", "ledger", "audit-directory", "cuts", "producer-directory", "binary", "output"):
        parser.add_argument("--"+name, type=Path, required=True)
    args = parser.parse_args()
    if importlib.metadata.version("databento-dbn") != "0.71.0":
        raise ValueError("pinned decoder required")
    import databento_dbn as dbn
    adapter_path = ROOT / "scripts/audit-native-book-batch.py"
    adapter = module("stock_interval_adapter", adapter_path)
    for directory in (common.BASE, args.audit_directory, args.producer_directory, args.output.parent, args.binary.parent):
        adapter.private_directory(directory)
    if args.output.exists() or args.output.parent != args.producer_directory or not args.producer_directory.is_relative_to(common.BASE):
        raise ValueError("fresh private producer-scoped output required")
    proof, manifest, start, end = adapter.acquired_source(args.proof, args.ledger, ROOT)
    query = proof["query"]
    if (query["dataset"], query["schema"]) != ("EQUS.MINI", "mbp-1"):
        raise ValueError("stock MBP-1 source required")
    audit_path, native_reference_path = args.audit_directory/"audit.json", args.audit_directory/"reference.json"
    audit, native_ref = read_json(audit_path), read_json(native_reference_path)
    if (len(audit["files"]) != 1 or audit["schema_version"] != "AutoWheelNativeBookSourceAuditV1"
            or audit["decoder_version"] != "0.71.0" or audit["upgrade_policy"] != "AS_IS"
            or audit["acquisition_proof_sha256"] != file_hash(args.proof)
            or native_ref["audit_sha256"] != file_hash(audit_path)
            or native_ref["records_compared_field_for_field"] != audit["records"]
            or native_ref["receipt_sha256"] != proof["receipt_sha256"]
            or native_ref["reference_script_sha256"] != file_hash(ROOT/"scripts/verify-native-book-vectors.py")):
        raise ValueError("native stock reference binding differs")
    member = audit["files"][0]
    source = Path(proof["source_directory"])/member["source_filename"]
    cuts_doc = read_json(args.cuts)
    cuts, cut_sources = cut_interface.verify_frozen_cuts(cuts_doc, query, start, end, ROOT)
    produced_path, producer_report_path = args.producer_directory/"intervals.jsonl.gz", args.producer_directory/"report.json"
    producer = read_json(producer_report_path)
    plan_path = args.producer_directory / "selection-plan.json"
    adapter.private_file(plan_path)
    plan_document = read_json(plan_path)
    if (set(plan_document) != {"schema", "control_only", "causal_selection_qualified", "selected_windows"}
            or plan_document["schema"] != "AutoWheelSelectedStockControlPlanV1"
            or plan_document["control_only"] is not True
            or plan_document["causal_selection_qualified"] is not False):
        raise ValueError("selected implementation control plan differs")
    code = [adapter_path, COMMON_PATH, OLD_PATH, CUTS_PATH, SELECTED_PATH,
            ROOT / "scripts/verify-native-full-study-GFD-stock-intervals.py",
            ROOT / "internal/causalbook/selected_intervals.go", Path(__file__).resolve(),
            ROOT/"scripts/produce-native-selected-stock-control.py", ROOT/"internal/causalbook/observe.go",
            ROOT/"internal/causalbook/intervals.go", ROOT/"internal/nativebook/stream.go",
            ROOT/"research/simulation/2026-10-05/cycle58-selected-execution-reference/main.go"]
    relevant = list(dict.fromkeys([Path(p) if Path(p).is_absolute() else ROOT/p for p in manifest]
                                 + [ROOT/p for p in cut_sources] + code + [args.proof, args.ledger, args.cuts,
                                    audit_path, native_reference_path, source, args.binary, produced_path, producer_report_path, plan_path]))
    for path in (args.proof, args.ledger, args.cuts, audit_path, native_reference_path, source, produced_path, producer_report_path):
        adapter.private_file(path)
    with common.private_input(args.binary, 0o700):
        pass
    before = {path: file_hash(path) for path in relevant}
    if (producer["schema"] != "AutoWheelSelectedNativeExecutionIntervalV1" or producer["actual_native_pass_complete"] is not True
            or producer["records_processed"] != member["records"] or producer["native_sha256"] != member["source_uncompressed_sha256"]
            or producer["binary_sha256"] != before[args.binary] or producer["producer_sha256"] != before[ROOT/"scripts/produce-native-selected-stock-control.py"]
            or producer["reducer_sha256"] != before[ROOT/"internal/causalbook/intervals.go"]
            or producer["raw_state_sha256"] != before[ROOT/"internal/causalbook/observe.go"]
            or producer["cli_sha256"] != before[ROOT/"research/simulation/2026-10-05/cycle58-selected-execution-reference/main.go"]
            or producer["acquisition_proof_sha256"] != before[args.proof] or producer["audit_sha256"] != before[audit_path]
            or producer["interval_compressed_sha256"] != before[produced_path]
            or producer["interval_compressed_bytes"] != produced_path.stat().st_size
            or producer["selected_plan_sha256"] != before[plan_path]
            or producer["selected_reducer_sha256"] != before[ROOT / "internal/causalbook/selected_intervals.go"]
            or producer["source_universe_instruments"] != 15
            or producer["output_projection"] != "selected_native_instrument_ids"
            or producer["causal_selection_qualified"] is not False
            or producer["continuous_pending_order_coverage_qualified"] is not False
            or producer["windows"] != 23100 or producer["retimed_rows"] != 0
            or any(producer[key] is not False for key in ("full_source_qualification", "replay_frames_produced", "historical_performance_available"))):
        raise ValueError("complete stock interval producer binding differs")
    if before[source] != member["source_compressed_sha256"]:
        raise ValueError("original compressed stock source differs")
    native_hash, native_bytes, records = hashlib.sha256(), 0, 0
    process = None
    try:
        with common.private_input(source) as native, common.private_input(produced_path) as interval_file:
            if hashlib.file_digest(native, "sha256").hexdigest() != before[source]:
                raise ValueError("stock source descriptor digest differs")
            native.seek(0)
            process = subprocess.Popen(["/usr/bin/zstd", "-q", "-f", "-d", "-c", "--", "/dev/fd/"+str(native.fileno())],
                                       pass_fds=(native.fileno(),), stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
            stream = process.stdout
            prefix = stream.read(8)
            if len(prefix) != 8 or prefix[:3] != b"DBN" or prefix[3] not in (1, 2, 3):
                raise ValueError("stock native prelude differs")
            meta_size = int.from_bytes(prefix[4:], "little")
            if not 100 <= meta_size <= (16<<20)-8:
                raise ValueError("stock metadata bound")
            header = prefix+stream.read(meta_size)
            if len(header) != meta_size+8:
                raise ValueError("stock metadata truncated")
            decoder = dbn.DBNDecoder(upgrade_policy=dbn.VersionUpgradePolicy.AS_IS, compression=dbn.Compression.NONE)
            values = decoder.write_and_decode(header)
            if decoder.buffer() or len(values) != 1 or not isinstance(values[0], dbn.Metadata):
                raise ValueError("stock metadata decode differs")
            metadata = values[0]
            if (metadata.dataset != query["dataset"] or str(metadata.schema) != query["schema"]
                    or metadata.start != start or metadata.end != end or metadata.limit not in (None, 0)
                    or str(metadata.stype_in) != "raw_symbol" or str(metadata.stype_out) != "instrument_id"
                    or metadata.version != member["native_version"] or type(metadata.ts_out) is not bool
                    or not adapter.metadata_symbols_match(list(metadata.symbols), query["symbols"])
                    or producer["metadata_sha256"] != hashlib.sha256(header).hexdigest()):
                raise ValueError("stock native metadata scope differs")
            _, reverse = adapter.active_mappings(metadata, datetime.fromtimestamp(start//common.SECOND, timezone.utc).date(), set(query["symbols"]))
            if len(reverse) != 15:
                raise ValueError("stock instrument universe differs")
            plan = plan_document["selected_windows"]
            if not isinstance(plan, list) or len(plan) != 23100:
                raise ValueError("complete selected control grid differs")
            ids = sorted(reverse)
            for index, entry in enumerate(plan):
                first = cuts[0] + index * common.SECOND
                width = index // 77 % 3 + 1
                head = index // 300 % 15
                expected = dict(Window=dict(From=first, Through=first + common.SECOND),
                                Instruments=[ids[(head + offset) % 15] for offset in range(width)])
                if entry != expected:
                    raise ValueError("independent clock-driven control mask differs")
            if producer["observations"] != sum(len(entry["Instruments"]) for entry in plan):
                raise ValueError("selected observation count differs")
            length = 80+8*metadata.ts_out
            native_hash.update(header)
            native_bytes += len(header)
            with gzip.GzipFile(fileobj=interval_file) as intervals:
                packets = common.Packets(intervals)
                reference = selected_reference.Reference(packets, plan, start, cuts_doc["max_age_nanos"], metadata, reverse)
                while raw := stream.read(length*16384):
                    if len(raw)%length:
                        raise ValueError("stock native trailing partial record")
                    native_hash.update(raw)
                    native_bytes += len(raw)
                    reference.consume_chunk(raw, records, length)
                    records += len(raw)//length
                    if records > member["records"] or native_bytes > member["source_uncompressed_bytes"]:
                        raise ValueError("stock native size or record count exceeded")
                if (process.wait() != 0 or records != member["records"] or native_bytes != member["source_uncompressed_bytes"]
                        or native_hash.hexdigest() != member["source_uncompressed_sha256"]):
                    raise ValueError("complete stock native digest or EOF differs")
                reconstruction = reference.finish_reconstruction(records)
                if reconstruction["windows"] != 23100:
                    raise ValueError("stock interval schedule incomplete")
                if packets.hash.hexdigest() != producer["interval_jsonl_sha256"]:
                    raise ValueError("stock interval plaintext differs")
                if any(producer[key] != value for key, value in reference.counts.items()):
                    raise ValueError("stock interval aggregate totals differ")
        if any(file_hash(path) != digest for path, digest in before.items()):
            raise ValueError("stock source or code stability differs")
        result = dict(schema_version="AutoWheelSelectedStockExecutionIntervalVerificationV1",
                      observed_at=datetime.now(timezone.utc).isoformat(), reference_script_sha256=before[Path(__file__).resolve()],
                      common_reference_sha256=before[COMMON_PATH], status_reference_sha256=before[OLD_PATH],
                      selected_reference_sha256=before[SELECTED_PATH], selected_plan_sha256=before[plan_path],
                      causal_selection_qualified=False, continuous_pending_order_coverage_qualified=False,
                      acquisition_proof_sha256=before[args.proof], audit_sha256=before[audit_path],
                      independent_native_reference_sha256=before[native_reference_path], cuts_sha256=before[args.cuts],
                      producer_report_sha256=before[producer_report_path], binary_sha256=before[args.binary],
                      source_compressed_sha256=before[source], native_sha256=native_hash.hexdigest(),
                      interval_compressed_sha256=before[produced_path], interval_jsonl_sha256=packets.hash.hexdigest(),
                      records_consumed=records, records_independently_reconstructed=reference.records_consumed,
                      records_reduced_within_windows=reference.records_reduced,
                      windows_compared=reference.counts["windows"], observations_compared=reference.counts["observations"],
                      producer_packets_match=True, producer_counters_match=True, gzip_eof_verified=True,
                      full_receive_order_reverified_this_pass=True, closed_seeds_used=False, retimed_rows=0,
                      full_source_qualification=False, replay_frames_produced=False, historical_performance_available=False,
                      limitations=["Selected stock projection control only; not causal strategy selection or pending-order coverage.",
                                   "Raw stock state and intervals only; terms, tradability, Greeks, lifecycle and clock normalization remain unqualified."])
        adapter.write_json(args.output, result)
        print(json.dumps(result))
    finally:
        if process is not None:
            if process.poll() is None:
                process.kill()
                process.wait()
            process.stdout.close()


if __name__ == "__main__":
    try:
        main()
    except Exception:
        print("native stock execution interval reference failed closed", file=sys.stderr)
        raise SystemExit(1)
