#!/usr/bin/env python3
"""Independent native reduction of every produced April-6 option interval.

Full source integrity/order and closed seeds have separate completed references.
This pass consumes and hashes all original native bytes and independently
decodes every in-window record. Gaps use proven closed seeds, never endpoints
from the interval producer. No provider calls, credentials, replay frames or P&L.
"""
import argparse
from contextlib import contextmanager
from datetime import datetime, timezone
import gzip
import hashlib
import importlib.metadata
import importlib.util
import json
import os
from pathlib import Path
import stat
import struct
import subprocess
import sys

from batch_acquisition_proof import file_hash, read_json

BASE = Path("/home/yuduo/Projects/AutoWheel-research")
SECOND = 1_000_000_000
MAX_LINE = 4 << 20
WIRE = struct.Struct("<BBHIQqIBBBxQi4xqqIIH2xH2x")
RECV = struct.Struct("<Q")
SENDING = struct.Struct("<Q")


def load_module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def fingerprint(info):
    return (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns,
            info.st_ctime_ns, info.st_mode, info.st_uid, info.st_nlink)


@contextmanager
def private_input(path, mode=0o600):
    if not path.is_absolute() or path.resolve() != path:
        raise ValueError("canonical input required")
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != mode
                or info.st_uid != os.getuid() or info.st_nlink != 1):
            raise ValueError("private regular descriptor required")
        with os.fdopen(os.dup(fd), "rb") as source:
            yield source
        if fingerprint(os.fstat(fd)) != fingerprint(info):
            raise ValueError("input descriptor changed")
    finally:
        os.close(fd)


class Packets:
    def __init__(self, source):
        self.source = source
        self.hash = hashlib.sha256()
        self.bytes = 0

    def next(self):
        raw = self.source.readline(MAX_LINE + 1)
        if not raw or len(raw) > MAX_LINE or not raw.endswith(b"\n"):
            raise ValueError("bounded packet missing")
        self.bytes += len(raw)
        if self.bytes > 8 << 30:
            raise ValueError("packet expansion bound")
        self.hash.update(raw)
        return json.loads(raw)

    def eof(self):
        if self.source.read(1):
            raise ValueError("extra packet")


def native_vector(raw, offset, index, metadata, reverse):
    (length, rtype, pub, inst, event, price, size, action, side, flags, recv, delta,
     bid, ask, bidsz, asksz, bidpub, askpub) = WIRE.unpack_from(raw, offset)
    if length * 4 != 80 + 8 * metadata.ts_out or rtype != 177 or inst not in reverse:
        raise ValueError("native wire identity differs")
    out = SENDING.unpack_from(raw, offset + 80)[0] if metadata.ts_out else None
    return [index, metadata.version, reverse[inst], pub, inst, recv, event, out,
            price, size, chr(action), chr(side), flags, delta, bid, ask, bidsz,
            asksz, rtype, bidpub, askpub]


class Reference:
    """A bounded, independent CMBP reducer initialized only from proven seeds."""
    def __init__(self, old, seeds, produced, cuts, start, max_age, metadata, reverse):
        self.old, self.seeds, self.produced = old, seeds, produced
        self.cuts, self.start, self.max_age = cuts, start, max_age
        self.metadata, self.reverse = metadata, reverse
        self.ids = sorted(reverse)
        self.windows = [(cut + k * SECOND, cut + (k + 1) * SECOND)
                        for cut in cuts[:74] for k in range(6)]
        self.index, self.active, self.seed_index = 0, False, 0
        self.states = {}
        self.counts = dict(windows=0, observations=0, raw_valid_observations=0,
                           receive_fresh_observations=0, event_fresh_observations=0,
                           minimum_present_observations=0)
        self.records_reduced = 0
        self.previous_active_receive = None

    def seed(self):
        packet = self.seeds.next()
        cut = self.cuts[self.seed_index]
        if (not isinstance(packet, list) or len(packet) != 5
                or packet[:4] != ["OPRA.PILLAR", self.start, cut, self.max_age]
                or not isinstance(packet[4], list) or len(packet[4]) != len(self.ids)):
            raise ValueError("closed seed scope differs")
        states = {}
        for ident, row in zip(self.ids, packet[4], strict=True):
            if (not isinstance(row, list) or len(row) != 12 or row[0] != ident
                    or any(type(row[i]) is not bool for i in (1, 4, 6, 8, 10, 11))
                    or row[6] or type(row[3]) is not int or type(row[9]) is not int
                    or row[3] > cut or row[9] > cut):
                raise ValueError("closed seed shape differs")
            for present, vec in ((row[1], row[2]), (row[4], row[5])):
                if present:
                    self.old._validate_vector(vec, vec[0], self.metadata.version,
                                              self.reverse, "cmbp-1", self.metadata.ts_out,
                                              self.start, self.start + self.old.DAY_NANOS)
                    if vec[4] != ident or vec[5] > cut:
                        raise ValueError("future or mismatched seed record")
                elif vec is not None:
                    raise ValueError("absent seed record has payload")
            s = dict(book=row[2], trade=row[5], avail=row[3], ambiguous=row[7] == self.old.ST_AMBIGUOUS,
                     has_update=row[8], last_update=row[9], last_touched=cut)
            if self.status(s, cut) != row[7]:
                raise ValueError("seed status cannot reconstruct state")
            covered = cut >= self.max_age and cut - self.max_age >= self.start
            if row[10] != covered or row[11] != (covered and (not row[8] or row[9] < cut-self.max_age)):
                raise ValueError("seed coverage differs")
            states[ident] = s
        self.seed_index += 1
        return states

    def status(self, s, at):
        return self.old._status_for(s["book"] is not None, s["book"], s["trade"] is not None,
                                    False, s["ambiguous"], at, self.max_age)

    def fold(self, s, at):
        facts, book = s["facts"], s["book"]
        if book is not None and book[10] != "R":
            if not facts[0]:
                facts[0:3] = [True, book[14], book[16]]
            else:
                facts[1], facts[2] = min(facts[1], book[14]), min(facts[2], book[16])
        valid = self.status(s, at) in (self.old.ST_FRESH, self.old.ST_STALE) and book[6] != 0 and book[5] <= at
        facts[3] = facts[3] and valid
        facts[4] = facts[4] and valid and at-book[5] <= self.max_age
        facts[5] = facts[5] and valid and book[6] <= at and at-book[6] <= self.max_age

    def start_window(self):
        if self.index % 6 == 0:
            self.states = self.seed()
        at = self.windows[self.index][0]
        for s in self.states.values():
            s["facts"] = [False, 0, 0, True, True, True]
            self.fold(s, at)
        self.active = True

    def finish_window(self):
        first, through = self.windows[self.index]
        covered = through >= self.max_age and through-self.max_age >= self.start
        rows = []
        for ident in self.ids:
            s = self.states[ident]
            self.fold(s, through)
            endpoint = [ident, s["book"] is not None, s["book"], s["avail"], s["trade"] is not None,
                        s["trade"], False, self.status(s, through), s["has_update"], s["last_update"],
                        covered, covered and (not s["has_update"] or s["last_update"] < through-self.max_age)]
            facts = s["facts"]
            rows.append([ident, *facts, endpoint])
            for key, value in zip(("minimum_present_observations", "raw_valid_observations",
                                   "receive_fresh_observations", "event_fresh_observations"),
                                  (facts[0], facts[3], facts[4], facts[5]), strict=True):
                self.counts[key] += int(value)
        expected = ["OPRA.PILLAR", self.start, first, through, self.max_age, rows]
        actual = self.produced.next()
        # Preserve JSON integer/bool/float distinctions; Python equality alone
        # would treat True as 1 and an integral float as an integer.
        if json.dumps(actual, ensure_ascii=False, separators=(",", ":"), allow_nan=False) != json.dumps(expected, ensure_ascii=False, separators=(",", ":"), allow_nan=False):
            raise ValueError("independent interval packet differs")
        self.counts["windows"] += 1
        self.counts["observations"] += len(self.ids)
        self.index += 1
        self.active = False

    def advance(self, next_receive):
        while self.index < len(self.windows):
            if not self.active:
                if next_receive <= self.windows[self.index][0]:
                    return
                self.start_window()
            if next_receive <= self.windows[self.index][1]:
                return
            self.finish_window()

    def apply(self, vec):
        recv, ident = vec[5], vec[4]
        if self.previous_active_receive is not None and recv < self.previous_active_receive:
            raise ValueError("active receive order regressed")
        self.previous_active_receive = recv
        self.old._validate_vector(vec, vec[0], self.metadata.version, self.reverse, "cmbp-1",
                                  self.metadata.ts_out, self.start, self.start + self.old.DAY_NANOS)
        s = self.states[ident]
        if recv > max(self.windows[self.index][0], s["last_touched"]):
            self.fold(s, recv)
        if vec[10] == "T":
            s["trade"] = vec
            if s["book"] is not None and s["book"][10] != "R" and not self.old._same_levels(s["book"], vec, "cmbp-1"):
                s["ambiguous"] = True
        else:
            s.update(book=vec, avail=recv, ambiguous=False, has_update=True, last_update=recv)
        s["last_touched"] = recv
        self.fold(s, recv)
        self.records_reduced += 1

    def consume_chunk(self, raw, base_index, length):
        # Binary-searching gaps relies on separately proven complete receive
        # ordering of THESE exact source bytes. It supplies no new order claim.
        offset, count = 0, len(raw)//length
        while offset < count and self.index < len(self.windows):
            recv = RECV.unpack_from(raw, offset*length+32)[0]
            self.advance(recv)
            if self.index >= len(self.windows):
                break
            if not self.active:
                target = self.windows[self.index][0]
                low, high = offset, count
                while low < high:
                    middle = (low+high)//2
                    if RECV.unpack_from(raw, middle*length+32)[0] <= target:
                        low = middle+1
                    else:
                        high = middle
                offset = low
                continue
            self.apply(native_vector(raw, offset*length, base_index+offset, self.metadata, self.reverse))
            offset += 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("proof", "ledger", "audit-directory", "cuts", "producer-directory", "binary", "seed-directory", "output"):
        parser.add_argument("--"+name, type=Path, required=True)
    args = parser.parse_args()
    if importlib.metadata.version("databento-dbn") != "0.71.0":
        raise ValueError("pinned decoder required")
    import databento_dbn as dbn
    root = Path(__file__).resolve().parents[1]
    adapter_path, old_path = root/"scripts/audit-native-book-batch.py", root/"scripts/verify-native-causal-observations.py"
    adapter, old = load_module("interval_native_adapter", adapter_path), load_module("interval_seed_reference", old_path)
    for directory in (BASE, args.audit_directory, args.seed_directory, args.producer_directory, args.output.parent):
        adapter.private_directory(directory)
    if args.output.exists() or args.output.parent != args.producer_directory or not args.producer_directory.is_relative_to(BASE):
        raise ValueError("fresh private producer-scoped output required")
    proof, manifest, start, end = adapter.acquired_source(args.proof, args.ledger, root)
    query = proof["query"]
    if (query["dataset"], query["schema"]) != ("OPRA.PILLAR", "cmbp-1"):
        raise ValueError("single-event option source required")
    audit_path, native_reference_path = args.audit_directory/"audit.json", args.audit_directory/"reference.json"
    audit, native_ref = read_json(audit_path), read_json(native_reference_path)
    if (len(audit["files"]) != 1 or audit["schema_version"] != "AutoWheelNativeBookSourceAuditV1"
            or audit["decoder_version"] != "0.71.0" or audit["upgrade_policy"] != "AS_IS"
            or audit["acquisition_proof_sha256"] != file_hash(args.proof)
            or native_ref["audit_sha256"] != file_hash(audit_path)
            or native_ref["records_compared_field_for_field"] != audit["records"]
            or native_ref["receipt_sha256"] != proof["receipt_sha256"]
            or native_ref["reference_script_sha256"] != file_hash(root/"scripts/verify-native-book-vectors.py")):
        raise ValueError("native reference binding differs")
    member = audit["files"][0]
    source = Path(proof["source_directory"])/member["source_filename"]
    cuts_doc = read_json(args.cuts)
    cuts, cut_sources = old._frozen_cuts(cuts_doc, query, start, end, root)
    seed_path, seed_report_path, seed_reference_path = (args.seed_directory/name for name in ("snapshots.jsonl.gz", "report.json", "reference.json"))
    produced_path, producer_report_path = args.producer_directory/"intervals.jsonl.gz", args.producer_directory/"report.json"
    seed_report, seed_ref, producer = read_json(seed_report_path), read_json(seed_reference_path), read_json(producer_report_path)
    original_binary = BASE/"cycle31-causal-observations-tools-20261006/causal-observation-producer"
    if (seed_ref["schema_version"] != "AutoWheelNativeCausalObservationVerificationV1"
            or seed_ref["reference_script_sha256"] != file_hash(old_path)
            or seed_ref["producer_report_sha256"] != file_hash(seed_report_path)
            or seed_ref["acquisition_proof_sha256"] != file_hash(args.proof)
            or seed_ref["audit_sha256"] != file_hash(audit_path)
            or seed_ref["independent_native_reference_sha256"] != file_hash(native_reference_path)
            or seed_ref["records_reconstructed"] != member["records"]
            or seed_ref["snapshots_compared"] != 75 or seed_ref["producer_snapshots_match"] is not True
            or seed_ref["producer_counters_match"] is not True
            or seed_ref["snapshot_compressed_sha256"] != file_hash(seed_path)
            or seed_ref["snapshot_compressed_bytes"] != seed_path.stat().st_size
            or seed_ref["snapshot_jsonl_sha256"] != seed_report["snapshot_jsonl_sha256"]
            or seed_ref["binary_sha256"] != file_hash(original_binary)
            or seed_report["binary_sha256"] != seed_ref["binary_sha256"]
            or seed_ref["cuts_sha256"] != file_hash(args.cuts)
            or seed_ref["vector_jsonl_sha256"] != member["vector_jsonl_sha256"]
            or seed_ref["full_source_qualification"] is not False):
        raise ValueError("completed closed-seed reference differs")
    code = [adapter_path, old_path, Path(__file__).resolve(), root/"scripts/produce-native-causal-observations.py",
            root/"scripts/produce-native-execution-intervals.py", root/"internal/causalbook/observe.go",
            root/"internal/causalbook/intervals.go", root/"internal/nativebook/stream.go",
            root/"research/simulation/2026-10-05/cycle34-execution-reference/main.go"]
    relevant = list(dict.fromkeys([Path(p) if Path(p).is_absolute() else root/p for p in manifest]
                                 + [root/p for p in cut_sources] + code + [args.proof, args.ledger, args.cuts,
                                    audit_path, native_reference_path, seed_path, seed_report_path, seed_reference_path,
                                    source, args.binary, original_binary, produced_path, producer_report_path]))
    for p in (args.proof, args.ledger, args.cuts, audit_path, native_reference_path, seed_path,
              seed_report_path, seed_reference_path, source, produced_path, producer_report_path):
        adapter.private_file(p)
    with private_input(args.binary, 0o700):
        pass
    with private_input(original_binary, 0o700):
        pass
    before = {p:file_hash(p) for p in relevant}
    if (producer["schema"] != "AutoWheelNativeExecutionIntervalV1" or producer["actual_native_pass_complete"] is not True
            or producer["records_processed"] != member["records"] or producer["native_sha256"] != member["source_uncompressed_sha256"]
            or producer["binary_sha256"] != before[args.binary] or producer["producer_sha256"] != before[root/"scripts/produce-native-execution-intervals.py"]
            or producer["reducer_sha256"] != before[root/"internal/causalbook/intervals.go"]
            or producer["raw_state_sha256"] != before[root/"internal/causalbook/observe.go"]
            or producer["cli_sha256"] != before[root/"research/simulation/2026-10-05/cycle34-execution-reference/main.go"]
            or producer["acquisition_proof_sha256"] != before[args.proof] or producer["audit_sha256"] != before[audit_path]
            or producer["interval_compressed_sha256"] != before[produced_path]
            or producer["interval_compressed_bytes"] != produced_path.stat().st_size
            or producer["windows"] != 444 or producer["retimed_rows"] != 0
            or any(producer[k] is not False for k in ("full_source_qualification", "replay_frames_produced", "historical_performance_available"))):
        raise ValueError("complete interval producer binding differs")
    if before[source] != member["source_compressed_sha256"]:
        raise ValueError("original compressed source differs")
    native_hash, native_bytes, records = hashlib.sha256(), 0, 0
    process = None
    try:
        with private_input(source) as native, private_input(seed_path) as seed_file, private_input(produced_path) as interval_file:
            if hashlib.file_digest(native, "sha256").hexdigest() != before[source]:
                raise ValueError("native descriptor digest differs")
            native.seek(0)
            process = subprocess.Popen(["/usr/bin/zstd", "-q", "-f", "-d", "-c", "--", "/dev/fd/"+str(native.fileno())],
                                       pass_fds=(native.fileno(),), stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
            stream = process.stdout
            prefix = stream.read(8)
            if len(prefix) != 8 or prefix[:3] != b"DBN" or prefix[3] not in (1,2,3):
                raise ValueError("native prelude differs")
            meta_size = int.from_bytes(prefix[4:], "little")
            if not 100 <= meta_size <= (16<<20)-8:
                raise ValueError("metadata bound")
            header = prefix + stream.read(meta_size)
            if len(header) != meta_size+8:
                raise ValueError("metadata truncated")
            decoder = dbn.DBNDecoder(upgrade_policy=dbn.VersionUpgradePolicy.AS_IS, compression=dbn.Compression.NONE)
            values = decoder.write_and_decode(header)
            if decoder.buffer() or len(values) != 1 or not isinstance(values[0], dbn.Metadata):
                raise ValueError("metadata decode differs")
            metadata = values[0]
            if (metadata.dataset != query["dataset"] or str(metadata.schema) != query["schema"]
                    or metadata.start != start or metadata.end != end or metadata.limit not in (None,0)
                    or str(metadata.stype_in) != "raw_symbol" or str(metadata.stype_out) != "instrument_id"
                    or metadata.version != member["native_version"] or type(metadata.ts_out) is not bool
                    or not adapter.metadata_symbols_match(list(metadata.symbols), query["symbols"])
                    or producer["metadata_sha256"] != hashlib.sha256(header).hexdigest()):
                raise ValueError("native metadata scope differs")
            _, reverse = adapter.active_mappings(metadata, datetime.fromtimestamp(start//SECOND, timezone.utc).date(), set(query["symbols"]))
            if not 0 < len(reverse) <= 2000 or producer["observations"] != 444*len(reverse):
                raise ValueError("instrument universe differs")
            length = 80 + 8*metadata.ts_out
            native_hash.update(header); native_bytes += len(header)
            with gzip.GzipFile(fileobj=seed_file) as seeds, gzip.GzipFile(fileobj=interval_file) as intervals:
                seed_packets, interval_packets = Packets(seeds), Packets(intervals)
                reference = Reference(old, seed_packets, interval_packets, cuts, start, cuts_doc["max_age_nanos"], metadata, reverse)
                while raw := stream.read(length*16384):
                    if len(raw)%length:
                        raise ValueError("native trailing partial record")
                    native_hash.update(raw); native_bytes += len(raw)
                    reference.consume_chunk(raw, records, length)
                    records += len(raw)//length
                    if records > member["records"] or native_bytes > member["source_uncompressed_bytes"]:
                        raise ValueError("native size or record count exceeded")
                if process.wait() != 0 or records != member["records"] or native_bytes != member["source_uncompressed_bytes"] or native_hash.hexdigest() != member["source_uncompressed_sha256"]:
                    raise ValueError("complete native digest or EOF differs")
                reference.advance(end)
                if reference.index != 444 or reference.seed_index != 74:
                    raise ValueError("interval schedule incomplete")
                reference.seed()  # Consume and verify the original closing seed.
                seed_packets.eof(); interval_packets.eof()
                if seed_packets.hash.hexdigest() != seed_ref["snapshot_jsonl_sha256"] or interval_packets.hash.hexdigest() != producer["interval_jsonl_sha256"]:
                    raise ValueError("complete packet plaintext differs")
                if any(producer[k] != value for k,value in reference.counts.items()):
                    raise ValueError("interval aggregate totals differ")
        if any(file_hash(p) != h for p,h in before.items()):
            raise ValueError("source or code stability differs")
        result = dict(schema_version="AutoWheelNativeExecutionIntervalVerificationV1",
                      observed_at=datetime.now(timezone.utc).isoformat(), reference_script_sha256=before[Path(__file__).resolve()],
                      acquisition_proof_sha256=before[args.proof], audit_sha256=before[audit_path],
                      independent_native_reference_sha256=before[native_reference_path], cuts_sha256=before[args.cuts],
                      closed_seed_reference_sha256=before[seed_reference_path], closed_seed_report_sha256=before[seed_report_path],
                      producer_report_sha256=before[producer_report_path], binary_sha256=before[args.binary],
                      source_compressed_sha256=before[source], native_sha256=native_hash.hexdigest(),
                      interval_compressed_sha256=before[produced_path], interval_jsonl_sha256=interval_packets.hash.hexdigest(),
                      records_consumed=records, records_reduced_within_windows=reference.records_reduced,
                      windows_compared=reference.counts["windows"], observations_compared=reference.counts["observations"],
                      producer_packets_match=True, producer_counters_match=True, gzip_eof_verified=True,
                      prior_full_receive_order_reference_bound=True, active_receive_order_verified=True,
                      full_receive_order_reverified_this_pass=False, retimed_rows=0,
                      full_source_qualification=False, replay_frames_produced=False, historical_performance_available=False,
                      limitations=["Raw interval validation only; terms, tradability, Greeks, lifecycle and full GFD coverage remain unqualified.",
                                   "Complete source receive ordering and closed seeds have separately completed, hash-bound references.",
                                   "This reference hashes every original native byte and decodes every in-window record; binary-searching gaps relies on that prior complete receive-order proof."])
        adapter.write_json(args.output, result)
        print(json.dumps(result))
    finally:
        if process is not None:
            if process.poll() is None:
                process.kill(); process.wait()
            process.stdout.close()


if __name__ == "__main__":
    try:
        main()
    except Exception:
        print("native execution interval reference failed closed", file=sys.stderr)
        raise SystemExit(1)
