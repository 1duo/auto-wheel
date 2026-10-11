#!/usr/bin/env python3
"""Independent full-native MBP-1 stock execution-interval reconstruction.

Consumes every original record, including gaps and unfinished multipart events.
Shares only the already-audited reference's packet/privacy utilities, interval
clock/minimum rules, and strict vector/status predicates. No closed seeds,
provider calls, credentials, replay frames, contract qualification or P&L.
"""
import argparse
from datetime import datetime, timezone
import gzip
import hashlib
import importlib.metadata
import json
from pathlib import Path
import struct
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
WIRE = struct.Struct("<BBHIQqIBBBBQiIqqIIII")


def native_vector(raw, offset, index, metadata, reverse):
    (length, rtype, pub, inst, event, price, size, action, side, flags, depth,
     recv, delta, sequence, bid, ask, bidsz, asksz, bidct, askct) = WIRE.unpack_from(raw, offset)
    if length * 4 != 80 + 8 * metadata.ts_out or rtype != 1 or inst not in reverse:
        raise ValueError("stock native wire identity differs")
    out = common.SENDING.unpack_from(raw, offset + 80)[0] if metadata.ts_out else None
    return [index, metadata.version, reverse[inst], pub, inst, recv, event, out,
            price, size, chr(action), chr(side), flags, delta, bid, ask, bidsz,
            asksz, rtype, bidct, askct, depth, sequence]


class Reference(common.Reference):
    """Full-stream stock state machine, retaining unfinished event state."""
    def __init__(self, produced, cuts, start, max_age, metadata, reverse):
        super().__init__(old, None, produced, cuts, start, max_age, metadata, reverse)
        self.states = {ident: dict(book=None, trade=None, avail=0, ambiguous=False,
                                  has_update=False, last_update=0, last_touched=0,
                                  pending=False, changed=False, working=None,
                                  working_ambiguous=False) for ident in self.ids}
        self.previous_receive = None
        self.records_consumed = 0

    def status(self, state, at):
        return old._status_for(state["book"] is not None, state["book"], state["trade"] is not None,
                               state["pending"], state["ambiguous"], at, self.max_age)

    def start_window(self):
        at = self.windows[self.index][0]
        for state in self.states.values():
            state["facts"] = [False, 0, 0, True, True, True]
            self.fold(state, at)
        self.active = True

    def finish_window(self):
        first, through = self.windows[self.index]
        covered = through >= self.max_age and through-self.max_age >= self.start
        rows = []
        for ident in self.ids:
            state = self.states[ident]
            self.fold(state, through)
            endpoint = [ident, state["book"] is not None, state["book"], state["avail"],
                        state["trade"] is not None, state["trade"], state["pending"],
                        self.status(state, through), state["has_update"], state["last_update"],
                        covered, covered and (not state["has_update"] or state["last_update"] < through-self.max_age)]
            facts = state["facts"]
            rows.append([ident, *facts, endpoint])
            for key, value in zip(("minimum_present_observations", "raw_valid_observations",
                                   "receive_fresh_observations", "event_fresh_observations"),
                                  (facts[0], facts[3], facts[4], facts[5]), strict=True):
                self.counts[key] += int(value)
        expected = ["EQUS.MINI", self.start, first, through, self.max_age, rows]
        # Compare serialized values to preserve integer/bool/float distinctions.
        canonical = lambda value: json.dumps(value, ensure_ascii=False, separators=(",", ":"), allow_nan=False)
        if canonical(self.produced.next()) != canonical(expected):
            raise ValueError("independent stock interval packet differs")
        self.counts["windows"] += 1
        self.counts["observations"] += len(self.ids)
        self.index += 1
        self.active = False

    def apply(self, vector):
        recv, ident = vector[5], vector[4]
        old._validate_vector(vector, self.records_consumed, self.metadata.version, self.reverse,
                             "mbp-1", self.metadata.ts_out, self.start, self.start+old.DAY_NANOS)
        if self.previous_receive is not None and recv < self.previous_receive:
            raise ValueError("complete stock receive order regressed")
        self.advance(recv)
        state = self.states[ident]
        if self.active and recv > max(self.windows[self.index][0], state["last_touched"]):
            self.fold(state, recv)
        if not state["pending"]:
            state["changed"] = False
            state["working_ambiguous"] = state["ambiguous"]
        state["pending"] = True
        if vector[10] == "T":
            state["trade"] = vector
            base = state["working"] if state["changed"] else state["book"]
            if base is not None and base[10] != "R" and not old._same_levels(base, vector, "mbp-1"):
                state["working_ambiguous"] = True
        else:
            state.update(working=vector, changed=True, working_ambiguous=False,
                         has_update=True, last_update=recv)
        if vector[12] & old.LAST_FLAG:
            if state["changed"]:
                state["book"], state["avail"] = state["working"], recv
            state["ambiguous"] = state["working_ambiguous"]
            state["pending"], state["changed"] = False, False
        state["last_touched"] = recv
        if self.active and not state["pending"]:
            self.fold(state, recv)
        self.previous_receive = recv
        self.records_consumed += 1
        self.records_reduced += int(self.active)

    def consume_chunk(self, raw, base_index, length):
        if base_index != self.records_consumed or len(raw) % length:
            raise ValueError("stock native record boundary differs")
        for offset in range(0, len(raw), length):
            self.apply(native_vector(raw, offset, self.records_consumed, self.metadata, self.reverse))


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
    cuts, cut_sources = old._frozen_cuts(cuts_doc, query, start, end, ROOT)
    produced_path, producer_report_path = args.producer_directory/"intervals.jsonl.gz", args.producer_directory/"report.json"
    producer = read_json(producer_report_path)
    code = [adapter_path, COMMON_PATH, OLD_PATH, Path(__file__).resolve(),
            ROOT/"scripts/produce-native-execution-intervals.py", ROOT/"internal/causalbook/observe.go",
            ROOT/"internal/causalbook/intervals.go", ROOT/"internal/nativebook/stream.go",
            ROOT/"research/simulation/2026-10-05/cycle34-execution-reference/main.go"]
    relevant = list(dict.fromkeys([Path(p) if Path(p).is_absolute() else ROOT/p for p in manifest]
                                 + [ROOT/p for p in cut_sources] + code + [args.proof, args.ledger, args.cuts,
                                    audit_path, native_reference_path, source, args.binary, produced_path, producer_report_path]))
    for path in (args.proof, args.ledger, args.cuts, audit_path, native_reference_path, source, produced_path, producer_report_path):
        adapter.private_file(path)
    with common.private_input(args.binary, 0o700):
        pass
    before = {path: file_hash(path) for path in relevant}
    if (producer["schema"] != "AutoWheelNativeExecutionIntervalV1" or producer["actual_native_pass_complete"] is not True
            or producer["records_processed"] != member["records"] or producer["native_sha256"] != member["source_uncompressed_sha256"]
            or producer["binary_sha256"] != before[args.binary] or producer["producer_sha256"] != before[ROOT/"scripts/produce-native-execution-intervals.py"]
            or producer["reducer_sha256"] != before[ROOT/"internal/causalbook/intervals.go"]
            or producer["raw_state_sha256"] != before[ROOT/"internal/causalbook/observe.go"]
            or producer["cli_sha256"] != before[ROOT/"research/simulation/2026-10-05/cycle34-execution-reference/main.go"]
            or producer["acquisition_proof_sha256"] != before[args.proof] or producer["audit_sha256"] != before[audit_path]
            or producer["interval_compressed_sha256"] != before[produced_path]
            or producer["interval_compressed_bytes"] != produced_path.stat().st_size
            or producer["windows"] != 444 or producer["retimed_rows"] != 0
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
            if len(reverse) != 15 or producer["observations"] != 444*len(reverse):
                raise ValueError("stock instrument universe differs")
            length = 80+8*metadata.ts_out
            native_hash.update(header)
            native_bytes += len(header)
            with gzip.GzipFile(fileobj=interval_file) as intervals:
                packets = common.Packets(intervals)
                reference = Reference(packets, cuts, start, cuts_doc["max_age_nanos"], metadata, reverse)
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
                reference.advance(end)
                if reference.index != 444 or reference.records_consumed != records:
                    raise ValueError("stock interval schedule incomplete")
                packets.eof()
                if packets.hash.hexdigest() != producer["interval_jsonl_sha256"]:
                    raise ValueError("stock interval plaintext differs")
                if any(producer[key] != value for key, value in reference.counts.items()):
                    raise ValueError("stock interval aggregate totals differ")
        if any(file_hash(path) != digest for path, digest in before.items()):
            raise ValueError("stock source or code stability differs")
        result = dict(schema_version="AutoWheelNativeStockExecutionIntervalVerificationV1",
                      observed_at=datetime.now(timezone.utc).isoformat(), reference_script_sha256=before[Path(__file__).resolve()],
                      common_reference_sha256=before[COMMON_PATH], status_reference_sha256=before[OLD_PATH],
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
                      limitations=["First-day six-second decision windows only; full GFD coverage remains missing.",
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
