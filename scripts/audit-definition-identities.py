#!/usr/bin/env python3
"""Derive private daily definition identities, preserving capture clocks and unknown terms.

These are observed identities, not qualified listed/tradable replay chains.
"""
import argparse
from collections import Counter
from datetime import date, datetime, time, timedelta, timezone
import gzip
import hashlib
import importlib.metadata
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
from zoneinfo import ZoneInfo


DEPENDENCY_SHA = "a2a58832a5d297b2872e152aa283d1cb1fe2a819473b10be058b6b9245d31694"
FIELDS = ["publisher_id", "instrument_id", "raw_symbol", "ts_recv", "ts_event",
          "strike_price_nanodollars", "expiration_date_timestamp_ns",
          "instrument_class", "security_update_action"]
UNKNOWN_TERMS = ["contract_multiplier", "min_price_increment", "activation",
                 "maturity_week", "unit_of_measure_qty"]
NANOSECOND = 1_000_000_000
DAY = 86_400 * NANOSECOND
OSI = re.compile(r"([A-Z0-9]{1,6} *)([0-9]{6})([PC])([0-9]{8})", re.ASCII)


def packed(value):
    return (json.dumps(value, separators=(",", ":"), ensure_ascii=True) + "\n").encode()


def midnight(day):
    return int(datetime.combine(day, time(), timezone.utc).timestamp()) * NANOSECOND


def osi_identity(symbol, roots):
    match = OSI.fullmatch(symbol) if isinstance(symbol, str) and len(symbol) == 21 else None
    if match is None or len(match[1]) != 6 or match[1].rstrip() not in roots:
        raise ValueError("invalid or out-of-scope OSI")
    raw_date = match[2]
    expiry = date(2000 + int(raw_date[:2]), int(raw_date[2:4]), int(raw_date[4:]))
    strike = int(match[4]) * 1_000_000
    if strike <= 0:
        raise ValueError("nonpositive OSI strike")
    return match[1].rstrip(), expiry, match[3], strike


def active_mappings(meta, day, roots):
    """Daily mapping intervals are identity joins, never intraday listing clocks."""
    active, reverse = {}, {}
    for raw, intervals in meta.mappings.items():
        osi_identity(raw, roots)
        for interval in intervals:
            start, end = interval["start_date"], interval["end_date"]
            symbol = interval["symbol"]
            if type(start) is not date or type(end) is not date or start >= end:
                raise ValueError("invalid native mapping interval")
            if not isinstance(symbol, str) or not re.fullmatch(r"[1-9][0-9]{0,9}", symbol):
                raise ValueError("invalid mapped instrument ID")
            instrument = int(symbol)
            if not 0 < instrument < 2**32:
                raise ValueError("mapped ID outside uint32")
            if start <= day < end:
                if raw in active or instrument in reverse:
                    raise ValueError("overlapping or ambiguous daily identity mapping")
                active[raw], reverse[instrument] = instrument, raw
    return active


def row_identity(row, day, roots, mappings, seen, dbn):
    root, expiry, kind, strike = osi_identity(row.raw_symbol, roots)
    if row.publisher_id != 30 or str(row.instrument_class) != kind or str(row.security_update_action) != "A":
        raise ValueError("unsupported definition publisher, class or update action")
    if row.strike_price != strike or row.expiration != midnight(expiry):
        raise ValueError("OSI/native strike or date differs")
    if mappings.get(row.raw_symbol) != row.instrument_id or row.instrument_id in seen:
        raise ValueError("definition has no exact daily mapping or repeats an ID")
    lower = midnight(day)
    if not lower <= row.ts_recv < lower + DAY or row.ts_event == dbn.UNDEF_TIMESTAMP:
        raise ValueError("definition capture or event clock invalid")
    sentinels = [2147483647, dbn.UNDEF_PRICE, dbn.UNDEF_TIMESTAMP, 255, dbn.UNDEF_PRICE]
    if any(getattr(row, key, None) != sentinel for key, sentinel in zip(UNKNOWN_TERMS, sentinels)):
        raise ValueError("term field differs from the audited missing-field corpus")
    seen.add(row.instrument_id)
    return root, [row.publisher_id, row.instrument_id, row.raw_symbol, row.ts_recv,
                  row.ts_event, row.strike_price, row.expiration, kind, "A"]


def sync_directory(path):
    fd = os.open(path, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def write_json(path, value):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as output:
        output.write((json.dumps(value, indent=2) + "\n").encode())
        output.flush()
        os.fsync(output.fileno())
    sync_directory(path.parent)


def audit_shard(path, compressed_sha, day, query, output, dbn, dependency, previous_ids):
    decoder = dbn.DBNDecoder(upgrade_policy=dbn.VersionUpgradePolicy.AS_IS,
                             compression=dbn.Compression.NONE)
    meta, mappings = None, None
    seen, observed_raw = set(), set()
    counts, before_first, terms = Counter(), Counter(), Counter()
    first_ns = int(datetime.combine(day, time(9, 35), ZoneInfo("America/New_York")).timestamp()) * NANOSECOND
    low = high = last = None
    regressions = source_clock_leads = cross_day_id_changes = records = raw_bytes = 0
    stream_hash, decoded_hash = hashlib.sha256(), hashlib.sha256()
    target = output / (day.isoformat() + ".identities.jsonl.gz")
    fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as stored, tempfile.TemporaryFile() as errors:
        with gzip.GzipFile(filename="", mode="wb", fileobj=stored, mtime=0, compresslevel=3) as encoded:
            proc = subprocess.Popen(["/usr/bin/zstd", "-q", "-d", "-c", "--", str(path)],
                                    stdout=subprocess.PIPE, stderr=errors)
            try:
                for chunk in iter(lambda: proc.stdout.read(128 << 10), b""):
                    raw_bytes += len(chunk)
                    if raw_bytes > 8 << 30:
                        raise ValueError("uncompressed definition shard too large")
                    decoded_hash.update(chunk)
                    for row in decoder.write_and_decode(chunk):
                        if isinstance(row, dbn.Metadata):
                            if meta is not None or records:
                                raise ValueError("duplicate or late DBN metadata")
                            meta = row
                            if (row.version != 1 or row.dataset != "OPRA.PILLAR" or str(row.schema) != "definition"
                                    or row.start != midnight(day) or row.end != midnight(day) + DAY
                                    or row.limit is not None or row.symbols != query["symbols"]
                                    or str(row.stype_in) != "parent" or str(row.stype_out) != "instrument_id"):
                                raise ValueError("unexpected native DBN scope/version")
                            mappings = active_mappings(row, day, {s[:-4] for s in query["symbols"]})
                            continue
                        if meta is None or type(row).__name__ != "InstrumentDefMsgV1":
                            raise ValueError("missing metadata or unexpected record type")
                        root, values = row_identity(row, day, {s[:-4] for s in query["symbols"]}, mappings, seen, dbn)
                        encoded.write(packed(values))
                        stream_hash.update(packed(values))
                        records += 1
                        if records > 1_000_000 or stored.tell() > 128 << 20:
                            raise ValueError("daily identity output exceeds the audit bound")
                        counts[root] += 1
                        if row.ts_recv <= first_ns:
                            before_first[root] += 1
                        if last is not None and row.ts_recv < last:
                            regressions += 1
                        if row.ts_event > row.ts_recv:
                            source_clock_leads += 1
                        low = row.ts_recv if low is None else min(low, row.ts_recv)
                        high = row.ts_recv if high is None else max(high, row.ts_recv)
                        last = row.ts_recv
                        observed_raw.add(row.raw_symbol)
                        if row.instrument_id in previous_ids and previous_ids[row.instrument_id] != row.raw_symbol:
                            cross_day_id_changes += 1
                        previous_ids[row.instrument_id] = row.raw_symbol
                        terms.update(UNKNOWN_TERMS)
                    if len(decoder.buffer()) > 16 << 20:
                        raise ValueError("DBN residual exceeds bound")
                proc.stdout.close()
                if proc.wait() != 0 or decoder.buffer() or meta is None or not records:
                    raise ValueError("invalid or truncated compressed/DBN source")
            finally:
                if proc.poll() is None:
                    proc.kill()
                    proc.wait()
                proc.stdout.close()
        stored.flush()
        os.fsync(stored.fileno())
    sync_directory(output)
    partial, not_found = set(meta.partial), set(meta.not_found)
    return {"date": day.isoformat(), "source_filename": path.name, "source_compressed_sha256": compressed_sha,
            "source_uncompressed_sha256": decoded_hash.hexdigest(), "records": records,
            "identity_file": target.name, "identity_sha256": dependency.file_hash(target),
            "identity_jsonl_sha256": stream_hash.hexdigest(), "identity_bytes": target.stat().st_size,
            "observed_root_counts": dict(counts), "captured_by_first_decision_root_counts": dict(before_first),
            "min_ts_recv": low, "max_ts_recv": high, "capture_order_regressions": regressions,
            "source_clock_lead_rows": source_clock_leads, "id_raw_changes_across_partitions": cross_day_id_changes,
            "active_mapping_count": len(mappings), "active_mappings_without_definition": len(set(mappings) - observed_raw),
            "partial_count": len(meta.partial), "partial_sha256": hashlib.sha256(packed(meta.partial)).hexdigest(),
            "partial_with_active_mapping": len(partial & set(mappings)), "partial_with_definition": len(partial & observed_raw),
            "not_found_count": len(meta.not_found), "not_found_sha256": hashlib.sha256(packed(meta.not_found)).hexdigest(),
            "not_found_with_definition": len(not_found & observed_raw), "undefined_field_counts": dict(terms)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--receipt", type=Path, required=True)
    parser.add_argument("--query", type=Path, required=True)
    parser.add_argument("--output-directory", type=Path, required=True)
    args = parser.parse_args()
    dependency_path = Path(__file__).with_name("audit-definition-batch.py")
    if hashlib.sha256(dependency_path.read_bytes()).hexdigest() != DEPENDENCY_SHA:
        raise ValueError("reviewed source verifier dependency changed")
    spec = importlib.util.spec_from_file_location("definition_batch_audit", dependency_path)
    dependency = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(dependency)
    if importlib.metadata.version("databento-dbn") != "0.71.0":
        raise ValueError("pinned decoder is required")
    import databento_dbn as dbn
    receipt, receipt_sha, query_sha, job, files = dependency.verified_source(args.receipt, args.query)
    query = receipt["query"]
    expected = []
    day = date(2026, 4, 6)
    holidays = {date(2026, 5, 25), date(2026, 6, 19), date(2026, 7, 3), date(2026, 9, 7)}
    while day <= date(2026, 10, 2):
        if day.weekday() < 5 and day not in holidays:
            expected.append(day)
        day += timedelta(days=1)
    declared = [(date.fromisoformat(p.name[12:20][:4] + "-" + p.name[12:20][4:6] + "-" + p.name[12:20][6:]), p, sha)
                for p, sha, size in files if re.fullmatch(r"opra-pillar-[0-9]{8}\.definition\.dbn\.zst", p.name)]
    declared.sort()
    if len(declared) != len(files) or [x[0] for x in declared] != expected or len(expected) != 126:
        raise ValueError("daily partition set differs from the frozen126-session calendar")
    if shutil.disk_usage(args.output_directory.parent).free < 2 << 30:
        raise ValueError("research disk headroom below2GiB")
    os.mkdir(args.output_directory, 0o700)
    results, previous = [], {}
    for day, path, sha in declared:
        result = audit_shard(path, sha, day, query, args.output_directory, dbn, dependency, previous)
        results.append(result)
        print(json.dumps({"date": day.isoformat(), "records": result["records"],
                          "active_mappings_without_definition": result["active_mappings_without_definition"]}), flush=True)
    count = sum(r["records"] for r in results)
    if type(job.get("record_count")) is not int or count != job["record_count"]:
        raise ValueError("decoded records differ from provider count")
    audit = {"schema_version": "AutoWheelObservedDefinitionIdentitiesV1", "status": "observed_identity_audit_unqualified",
             "observed_at": datetime.now(timezone.utc).isoformat(), "script_sha256": dependency.file_hash(Path(__file__)),
             "verifier_dependency_sha256": DEPENDENCY_SHA, "decoder_version": "0.71.0", "native_version": 1,
             "receipt_sha256": receipt_sha, "query_file_sha256": query_sha, "job_id": receipt["job_id"],
             "operation_id": receipt["operation_id"], "query_hash": receipt["query_hash"],
             "identity_jsonl_fields": FIELDS, "undefined_terms_omitted_not_defaulted": UNKNOWN_TERMS,
             "records": count, "provider_record_count_matches": True, "daily_partitions": len(results),
             "partitions": results, "replay_frames_produced": False, "source_gate_passed": False,
             "historical_performance_available": False,
             "limitations": ["Daily ID joins and observed captures do not prove first listing, active tradability, standard deliverables, minimum ticks or weekly program membership.",
                             "Identity files retain original record order and timestamps; consumers must use ts_recv causally and handle any reported regressions.",
                             "Metadata partial/not_found fields describe symbol resolution, never absence of a listed chain or a delisting.",
                             "Expiration timestamps retain UTC date precision, not an exchange expiry/settlement clock."]}
    write_json(args.output_directory / "audit.json", audit)
    print(json.dumps({"daily_partitions": len(results), "records": count, "source_gate_passed": False}), flush=True)


if __name__ == "__main__":
    main()
