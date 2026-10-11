#!/usr/bin/env python3
"""Bounded offline native statistics source audit with private lossless vectors.

Supports only OPRA.PILLAR statistics raw_symbol single UTC day inside
2026-04-06 to 2026-10-03 with 1 to 2000 distinct strict 21 byte OSI symbols
from the frozen 15 member universe. Validates receipt and provider files,
decodes native StatMsgV1 with pinned decoder AS_IS, preserves source order,
and writes private deterministic gzip JSONL integer vectors.

This is a source audit, not a replay chain or qualification. It produces no
replay frames, no performance claim, no OI state, no coverage inference, and
no tradability inference. Undefined native sentinels remain undefined.
"""
import argparse
import gzip
import hashlib
import importlib.metadata
import json
import os
import re
import shutil
import subprocess
import tempfile
from collections import Counter
from datetime import date
from datetime import datetime
from datetime import timedelta
from datetime import timezone
from pathlib import Path
FROZEN_UNIVERSE = ["NFLX", "TSLA", "NVDA", "NBIS", "IREN", "NOW", "AMD", "AMZN", "GOOGL", "META", "MSFT", "PLTR", "UBER", "AVGO", "ORCL"]
BATCH_START = "2026-04-06T00:00:00Z"
BATCH_END = "2026-10-03T00:00:00Z"
VECTOR_FIELDS = ["row_index", "native_version", "raw_symbol", "publisher_id", "instrument_id", "ts_recv", "ts_event", "ts_ref", "price", "quantity", "sequence", "ts_in_delta", "stat_type", "channel_id", "update_action", "stat_flags", "rtype"]
SCHEMA_VERSION = "AutoWheelNativeStatisticsSourceAuditV1"
DECODER_VERSION = "0.71.0"
def unique_pairs(pairs):
    obj = {}
    for key, value in pairs:
        if key in obj:
            raise ValueError("duplicate JSON key")
        obj[key] = value
    return obj
def read_json(path):
    if path.is_symlink():
        raise ValueError("symlink evidence not allowed")
    with path.open("rb") as source:
        raw = source.read((4 << 20) + 1)
    if not raw or len(raw) > 4 << 20:
        raise ValueError("bounded evidence size invalid")
    return json.loads(raw, object_pairs_hook=unique_pairs), hashlib.sha256(raw).hexdigest()
def file_hash(path):
    h = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()
def packed(value):
    return (json.dumps(value, separators=(",", ":"), ensure_ascii=True) + "\n").encode()
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
def parse_osi(symbol):
    if not isinstance(symbol, str) or len(symbol) != 21:
        raise ValueError("invalid OSI length")
    for ch in symbol:
        code = ord(ch)
        if code < 32 or code > 126:
            raise ValueError("invalid OSI bytes")
    root = symbol[:6]
    if root[0] == " ":
        raise ValueError("invalid OSI root")
    seen_space = False
    for i in range(6):
        c = root[i]
        if c == " ":
            seen_space = True
            continue
        if seen_space:
            raise ValueError("invalid OSI root padding")
        if not (("A" <= c <= "Z") or ("0" <= c <= "9")):
            raise ValueError("invalid OSI root charset")
    expiry_raw = symbol[6:12]
    for i in range(6):
        c = expiry_raw[i]
        if not ("0" <= c <= "9"):
            raise ValueError("invalid OSI expiry digits")
    try:
        expiry = date(2000 + int(expiry_raw[:2]), int(expiry_raw[2:4]), int(expiry_raw[4:6]))
    except Exception:
        raise ValueError("invalid OSI expiry calendar")
    right = symbol[12]
    if right != "C" and right != "P":
        raise ValueError("invalid OSI right")
    strike_raw = symbol[13:21]
    for i in range(8):
        c = strike_raw[i]
        if not ("0" <= c <= "9"):
            raise ValueError("invalid OSI strike digits")
    strike = int(strike_raw)
    if strike <= 0:
        raise ValueError("nonpositive OSI strike")
    trimmed = root.strip()
    if trimmed not in FROZEN_UNIVERSE:
        raise ValueError("OSI root outside frozen universe")
    return trimmed, expiry, right, strike
def validate_query(query):
    if not isinstance(query, dict):
        raise ValueError("query shape invalid")
    if set(query) != {"dataset", "schema", "symbols", "stype_in", "start", "end"}:
        raise ValueError("unexpected statistics query fields")
    if query.get("dataset") != "OPRA.PILLAR":
        raise ValueError("unexpected statistics dataset")
    if query.get("schema") != "statistics":
        raise ValueError("unexpected statistics schema")
    if query.get("stype_in") != "raw_symbol":
        raise ValueError("unexpected statistics stype_in")
    symbols = query.get("symbols")
    if not isinstance(symbols, list) or not 1 <= len(symbols) <= 2000:
        raise ValueError("statistics symbols out of bound")
    seen = set()
    for s in symbols:
        if not isinstance(s, str):
            raise ValueError("invalid OSI symbol type")
        parse_osi(s)
        if s in seen:
            raise ValueError("duplicate statistics symbol")
        seen.add(s)
    start_s = query.get("start")
    end_s = query.get("end")
    if not isinstance(start_s, str) or not isinstance(end_s, str):
        raise ValueError("statistics time shape invalid")
    try:
        start_dt = datetime.strptime(start_s, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
        end_dt = datetime.strptime(end_s, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
    except Exception:
        raise ValueError("statistics time must be canonical UTC Z")
    if (start_dt.strftime("%Y-%m-%dT%H:%M:%SZ") != start_s
            or end_dt.strftime("%Y-%m-%dT%H:%M:%SZ") != end_s):
        raise ValueError("statistics time must be canonical UTC Z")
    if not start_dt < end_dt:
        raise ValueError("statistics time range invalid")
    if (end_dt - start_dt) != timedelta(hours=24):
        raise ValueError("statistics query must span exactly one UTC day")
    if start_dt.hour != 0 or start_dt.minute != 0 or start_dt.second != 0:
        raise ValueError("statistics query must start at UTC midnight")
    if end_dt.hour != 0 or end_dt.minute != 0 or end_dt.second != 0:
        raise ValueError("statistics query must end at UTC midnight")
    try:
        batch_start = datetime.strptime(BATCH_START, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
        batch_end = datetime.strptime(BATCH_END, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
    except Exception:
        raise ValueError("frozen window invalid")
    if start_dt < batch_start or end_dt > batch_end:
        raise ValueError("statistics query outside frozen window")
    start_ns = int(start_dt.timestamp()) * 1000000000
    end_ns = int(end_dt.timestamp()) * 1000000000
    day = start_dt.date()
    return symbols, start_ns, end_ns, day

def source_utc_ns(text):
    match = re.fullmatch(r"([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\.([0-9]{1,9}))?Z", text) if isinstance(text, str) else None
    if match is None:
        raise ValueError("invalid provider UTC timestamp")
    stamp = datetime.strptime(match[1], "%Y-%m-%dT%H:%M:%S").replace(tzinfo=timezone.utc)
    return int(stamp.timestamp()) * 1000000000 + int((match[2] or "").ljust(9, "0"))

def verified_source(receipt_path, query_path):
    receipt, receipt_hash = read_json(receipt_path)
    query, query_hash = read_json(query_path)
    if receipt.get("all_verified") is not True:
        raise ValueError("acquisition is not verified")
    if receipt.get("state") != "done":
        raise ValueError("acquisition state not done")
    if receipt.get("query") != query:
        raise ValueError("acquisition is not verified for the declared query")
    symbols, start_ns, end_ns, day = validate_query(query)
    base = receipt_path.parent
    job, job_hash = read_json(base / "job-source.json")
    manifest, manifest_hash = read_json(base / "files-source.json")
    if job_hash != receipt.get("job_sha256"):
        raise ValueError("job binding differs")
    if manifest_hash != receipt.get("file_list_sha256"):
        raise ValueError("file manifest binding differs")
    if job.get("id") != receipt.get("job_id"):
        raise ValueError("job ID differs")
    if job.get("state") != "done":
        raise ValueError("job state not done")
    for key in ("dataset", "schema", "stype_in"):
        if job.get(key) != query.get(key):
            raise ValueError("job scope differs")
    if source_utc_ns(job.get("start")) != start_ns:
        raise ValueError("job start differs")
    if source_utc_ns(job.get("end")) != end_ns:
        raise ValueError("job end differs")
    job_symbols = job.get("symbols")
    if isinstance(job_symbols, list):
        if job_symbols != symbols:
            raise ValueError("job symbols differ")
    elif isinstance(job_symbols, str):
        if job_symbols != ",".join(symbols):
            raise ValueError("job symbols differ")
    else:
        raise ValueError("job symbols shape invalid")
    if "stype_out" in job and job.get("stype_out") != "instrument_id":
        raise ValueError("job stype_out differs")
    if "encoding" in job and job.get("encoding") != "dbn":
        raise ValueError("job encoding differs")
    if "compression" in job and job.get("compression") != "zstd":
        raise ValueError("job compression differs")
    if "split_duration" in job and job.get("split_duration") != "day":
        raise ValueError("job split differs")
    if "limit" in job and job.get("limit") is not None:
        raise ValueError("job limit must be null")
    record_count = job.get("record_count")
    if type(record_count) is not int or record_count <= 0:
        raise ValueError("provider record count invalid")
    files = receipt.get("files")
    if not isinstance(files, list) or not 1 <= len(files) <= 4096:
        raise ValueError("file set is incomplete")
    if not isinstance(manifest, list) or len(files) != len(manifest):
        raise ValueError("file set is incomplete")
    seen = set()
    total = 0
    data = []
    for f, m in zip(files, manifest):
        name = f.get("filename")
        size = f.get("bytes")
        sha = f.get("sha256")
        if not isinstance(name, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,254}", name):
            raise ValueError("unsafe file")
        if name in seen:
            raise ValueError("duplicate file")
        if type(size) is not int or not 0 <= size <= 8 << 30:
            raise ValueError("file proof invalid")
        if not isinstance(sha, str) or not re.fullmatch(r"[0-9a-f]{64}", sha):
            raise ValueError("file proof invalid")
        if m.get("filename") != name:
            raise ValueError("manifest file proof differs")
        if type(m.get("size")) is not int or m.get("size") != size:
            raise ValueError("manifest file proof differs")
        if m.get("hash") != "sha256:" + sha:
            raise ValueError("manifest file proof differs")
        path = base / name
        if path.is_symlink():
            raise ValueError("symlink not allowed")
        if not path.is_file():
            raise ValueError("file bytes missing")
        if path.stat().st_size != size:
            raise ValueError("file bytes differ")
        if file_hash(path) != sha:
            raise ValueError("file bytes differ")
        seen.add(name)
        total += size
        if total > 64 << 30:
            raise ValueError("file set exceeds bound")
        if name.endswith(".statistics.dbn.zst"):
            data.append((path, sha, size))
    if type(receipt.get("total_bytes")) is not int or total != receipt["total_bytes"]:
        raise ValueError("total byte proof invalid")
    if not data:
        raise ValueError("no statistics shards")
    expected_tag = day.strftime("%Y%m%d")
    dates = set()
    for path, sha, size in data:
        match = re.fullmatch(r"opra-pillar-([0-9]{8})\.statistics\.dbn\.zst", path.name)
        if match is None:
            raise ValueError("statistics shard name invalid")
        dates.add(match.group(1))
        if match.group(1) != expected_tag:
            raise ValueError("statistics shard day differs from query")
    if len(dates) != 1:
        raise ValueError("statistics shards span multiple days")
    return receipt, receipt_hash, query_hash, job, data, symbols, start_ns, end_ns, day
def active_mappings(meta, day, queried_set):
    mappings = getattr(meta, "mappings", None)
    if not isinstance(mappings, dict):
        raise ValueError("native mappings shape invalid")
    active = {}
    reverse = {}
    for raw, intervals in mappings.items():
        parse_osi(raw)
        if raw not in queried_set:
            raise ValueError("mapping contains unqueried OSI")
        if not isinstance(intervals, list):
            raise ValueError("invalid native mapping interval list")
        for interval in intervals:
            if isinstance(interval, dict):
                start = interval.get("start_date")
                end = interval.get("end_date")
                symbol = interval.get("symbol")
            else:
                start = getattr(interval, "start_date", None)
                end = getattr(interval, "end_date", None)
                symbol = getattr(interval, "symbol", None)
            if type(start) is not date or type(end) is not date or not start < end:
                raise ValueError("invalid native mapping interval")
            if not isinstance(symbol, str) or not re.fullmatch(r"[1-9][0-9]{0,9}", symbol):
                raise ValueError("invalid mapped instrument ID")
            instrument = int(symbol)
            if not 0 < instrument < 2 ** 32:
                raise ValueError("mapped ID outside uint32")
            if start <= day < end:
                if raw in active or instrument in reverse:
                    raise ValueError("overlapping or ambiguous daily mapping")
                active[raw] = instrument
                reverse[instrument] = raw
    return active, reverse
def audit_shard(path, compressed_sha, compressed_bytes, symbols, queried_set, start_ns, end_ns, day, output_dir, dbn):
    decoder = dbn.DBNDecoder(upgrade_policy=dbn.VersionUpgradePolicy.AS_IS, compression=dbn.Compression.NONE)
    meta = None
    active = None
    reverse = None
    meta_version = None
    partial_list = []
    not_found_list = []
    observed_raw = set()
    counts_by_stat = Counter()
    counts_by_pub = Counter()
    counts_by_action = Counter()
    counts_by_flags = Counter()
    undefined_counts = Counter()
    undefined_quantity_by_stat = Counter()
    version_counts = Counter()
    leads_by_stat = Counter()
    oi_leads = 0
    regressions = 0
    records = 0
    raw_bytes = 0
    low = None
    high = None
    last = None
    decoded_hash = hashlib.sha256()
    stream_hash = hashlib.sha256()
    stem = path.name
    if stem.endswith(".dbn.zst"):
        stem = stem[:-8]
    vector_name = stem + ".vectors.jsonl.gz"
    target = output_dir / vector_name
    undef_ts = int(dbn.UNDEF_TIMESTAMP)
    undef_price = int(dbn.UNDEF_PRICE)
    # StatMsgV1 (native DBN v1/v2) carries signed int32 quantity. The
    # library-wide UNDEF_STAT_QUANTITY belongs to the newer int64 record.
    undef_qty = 2 ** 31 - 1
    fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as stored:
        with gzip.GzipFile(filename="", mode="wb", fileobj=stored, mtime=0, compresslevel=6) as encoded:
            with tempfile.TemporaryFile() as errors:
                proc = subprocess.Popen(["/usr/bin/zstd", "-q", "-d", "-c", "--", str(path)], stdout=subprocess.PIPE, stderr=errors)
                try:
                    for chunk in iter(lambda: proc.stdout.read(128 << 10), b""):
                        raw_bytes += len(chunk)
                        if raw_bytes > 8 << 30:
                            raise ValueError("uncompressed shard too large")
                        decoded_hash.update(chunk)
                        for row in decoder.write_and_decode(chunk):
                            if isinstance(row, dbn.Metadata):
                                if meta is not None or records != 0:
                                    raise ValueError("duplicate or late DBN metadata")
                                meta = row
                                try:
                                    version = int(row.version)
                                except Exception:
                                    raise ValueError("invalid native version")
                                if version not in (1, 2):
                                    raise ValueError("unsupported native version")
                                meta_version = version
                                if row.dataset != "OPRA.PILLAR":
                                    raise ValueError("unexpected native dataset")
                                if str(row.schema) != "statistics":
                                    raise ValueError("unexpected native schema")
                                if int(row.start) != start_ns or int(row.end) != end_ns:
                                    raise ValueError("native time scope differs")
                                if row.limit is not None:
                                    raise ValueError("native limit must be null")
                                if list(row.symbols) != symbols:
                                    raise ValueError("native symbols differ")
                                if str(row.stype_in) != "raw_symbol" or str(row.stype_out) != "instrument_id":
                                    raise ValueError("native stype differs")
                                active, reverse = active_mappings(row, day, queried_set)
                                partial_list = list(row.partial)
                                not_found_list = list(row.not_found)
                                continue
                            if meta is None:
                                raise ValueError("missing metadata pre-record")
                            if type(row).__name__ != "StatMsgV1":
                                raise ValueError("unsupported native record")
                            try:
                                rtype_raw = getattr(row, "rtype", None)
                                if rtype_raw is None:
                                    hd = getattr(row, "hd", None)
                                    if hd is not None:
                                        rtype_raw = getattr(hd, "rtype", None)
                                rtype = int(rtype_raw)
                            except Exception:
                                raise ValueError("invalid rtype")
                            if rtype != 24:
                                raise ValueError("unsupported rtype")
                            try:
                                pub = int(getattr(row, "publisher_id"))
                                inst = int(getattr(row, "instrument_id"))
                            except Exception:
                                raise ValueError("invalid publisher or instrument")
                            if not 0 < pub <= 65535:
                                raise ValueError("publisher outside uint16 nonzero")
                            if not 0 < inst < 2 ** 32:
                                raise ValueError("instrument outside uint32")
                            if inst not in reverse:
                                raise ValueError("instrument without exact daily mapping")
                            raw_symbol = reverse[inst]
                            if active.get(raw_symbol) != inst:
                                raise ValueError("mapping mismatch")
                            try:
                                ts_recv = int(getattr(row, "ts_recv"))
                                ts_event = int(getattr(row, "ts_event"))
                                ts_ref = int(getattr(row, "ts_ref"))
                                price = int(getattr(row, "price"))
                                quantity = int(getattr(row, "quantity"))
                                sequence = int(getattr(row, "sequence"))
                                ts_in_delta = int(getattr(row, "ts_in_delta"))
                                stat_type = int(getattr(row, "stat_type"))
                                channel_id = int(getattr(row, "channel_id"))
                                update_action = int(getattr(row, "update_action"))
                                stat_flags = int(getattr(row, "stat_flags"))
                            except Exception:
                                raise ValueError("invalid native integer field")
                            if not start_ns <= ts_recv < end_ns:
                                raise ValueError("ts_recv outside query range")
                            if not 0 <= ts_event < 2 ** 64:
                                raise ValueError("ts_event outside uint64")
                            if not 0 <= ts_ref < 2 ** 64:
                                raise ValueError("ts_ref outside uint64")
                            if ts_event == undef_ts:
                                undefined_counts["ts_event"] += 1
                            if ts_ref == undef_ts:
                                undefined_counts["ts_ref"] += 1
                            if price == undef_price:
                                undefined_counts["price"] += 1
                            if quantity == undef_qty:
                                undefined_counts["quantity"] += 1
                                undefined_quantity_by_stat[str(stat_type)] += 1
                            if not -(2 ** 63) <= price < 2 ** 63:
                                raise ValueError("price outside int64")
                            if not -(2 ** 31) <= quantity < 2 ** 31:
                                raise ValueError("native V1 quantity outside int32")
                            if not 0 <= sequence < 2 ** 32:
                                raise ValueError("sequence outside uint32")
                            if not -(2 ** 31) <= ts_in_delta < 2 ** 31:
                                raise ValueError("ts_in_delta outside int32")
                            if not 0 <= stat_type <= 65535:
                                raise ValueError("stat_type outside uint16")
                            if not 0 <= channel_id <= 65535:
                                raise ValueError("channel outside uint16")
                            if not 0 <= update_action < 256:
                                raise ValueError("update_action outside uint8")
                            if not 0 <= stat_flags < 256:
                                raise ValueError("stat_flags outside uint8")
                            if ts_event != undef_ts and ts_event > ts_recv:
                                leads_by_stat[str(stat_type)] += 1
                                if stat_type == 9:
                                    oi_leads += 1
                            if last is not None and ts_recv < last:
                                regressions += 1
                            if low is None or ts_recv < low:
                                low = ts_recv
                            if high is None or ts_recv > high:
                                high = ts_recv
                            last = ts_recv
                            observed_raw.add(raw_symbol)
                            counts_by_stat[str(stat_type)] += 1
                            counts_by_pub[str(pub)] += 1
                            counts_by_action[str(update_action)] += 1
                            counts_by_flags[str(stat_flags)] += 1
                            values = [records, meta_version, raw_symbol, pub, inst, ts_recv, ts_event, ts_ref, price, quantity, sequence, ts_in_delta, stat_type, channel_id, update_action, stat_flags, rtype]
                            line = (json.dumps(values, separators=(",", ":"), ensure_ascii=True) + "\n").encode()
                            encoded.write(line)
                            stream_hash.update(line)
                            records += 1
                            version_counts[str(meta_version)] += 1
                            if records > 10000000 or stored.tell() > 1 << 30:
                                raise ValueError("vector output exceeds audit bound")
                        if len(decoder.buffer()) > 16 << 20:
                            raise ValueError("DBN residual exceeds bound")
                    proc.stdout.close()
                    if proc.wait() != 0 or decoder.buffer() or meta is None or records == 0:
                        raise ValueError("invalid or truncated compressed source")
                finally:
                    if proc.poll() is None:
                        proc.kill()
                        proc.wait()
                    try:
                        proc.stdout.close()
                    except Exception:
                        pass
        stored.flush()
        os.fsync(stored.fileno())
    sync_directory(output_dir)
    partial_set = set(partial_list)
    not_found_set = set(not_found_list)
    active_set = set(active.keys())
    result = {"source_filename": path.name, "source_compressed_sha256": compressed_sha, "source_compressed_bytes": compressed_bytes, "source_uncompressed_sha256": decoded_hash.hexdigest(), "source_uncompressed_bytes": raw_bytes, "native_version": meta_version, "native_version_counts": dict(version_counts), "records": records, "vector_file": vector_name, "vector_sha256": file_hash(target), "vector_jsonl_sha256": stream_hash.hexdigest(), "vector_bytes": target.stat().st_size, "min_ts_recv": low, "max_ts_recv": high, "capture_order_regressions": regressions, "clock_leads_by_stat_type": dict(leads_by_stat), "open_interest_clock_leads": oi_leads, "counts_by_stat_type": dict(counts_by_stat), "counts_by_publisher": dict(counts_by_pub), "counts_by_update_action": dict(counts_by_action), "counts_by_stat_flags": dict(counts_by_flags), "undefined_sentinel_counts": dict(undefined_counts), "active_mapping_count": len(active), "active_mappings_without_record": len(active_set - observed_raw), "partial_count": len(partial_list), "partial_sha256": hashlib.sha256(packed(partial_list)).hexdigest(), "partial_with_active_mapping": len(partial_set & active_set), "partial_with_record": len(partial_set & observed_raw), "not_found_count": len(not_found_list), "not_found_sha256": hashlib.sha256(packed(not_found_list)).hexdigest(), "not_found_with_record": len(not_found_set & observed_raw)}
    result["native_quantity_missing_sentinel"] = undef_qty
    result["undefined_quantity_by_stat_type"] = dict(undefined_quantity_by_stat)
    return result
def main():
    parser = argparse.ArgumentParser(description="Bounded offline native statistics source audit")
    parser.add_argument("--receipt", type=Path, required=True)
    parser.add_argument("--query", type=Path, required=True)
    parser.add_argument("--output-directory", type=Path, required=True)
    args = parser.parse_args()
    if args.output_directory.exists():
        raise ValueError("audit output already exists")
    if importlib.metadata.version("databento-dbn") != DECODER_VERSION:
        raise ValueError("pinned research decoder is required")
    import databento_dbn as dbn
    native_fields = dict(dbn.StatMsgV1._dtypes)
    if (native_fields.get("quantity") != "i4" or native_fields.get("stat_type") != "u2"
            or dbn.StatMsgV1.size_hint != 64):
        raise ValueError("pinned native V1 record layout differs")
    receipt, receipt_hash, query_hash, job, files, symbols, start_ns, end_ns, day = verified_source(args.receipt, args.query)
    queried_set = set(symbols)
    if shutil.disk_usage(args.output_directory.parent).free < 2 << 30:
        raise ValueError("research disk headroom below 2GiB")
    os.mkdir(args.output_directory, 0o700)
    results = []
    for path, sha, size in files:
        result = audit_shard(path, sha, size, symbols, queried_set, start_ns, end_ns, day, args.output_directory, dbn)
        results.append(result)
        print(json.dumps({"source": path.name, "records": result["records"]}), flush=True)
    total_records = sum(r["records"] for r in results)
    if total_records != job.get("record_count"):
        raise ValueError("decoded records differ from provider count")
    audit = {"schema_version": SCHEMA_VERSION, "observed_at": datetime.now(timezone.utc).isoformat(), "status": "observed_unqualified", "script_sha256": file_hash(Path(__file__)), "decoder_version": DECODER_VERSION, "upgrade_policy": "AS_IS", "receipt_sha256": receipt_hash, "query_file_sha256": query_hash, "job_id": receipt.get("job_id"), "operation_id": receipt.get("operation_id"), "query_hash": receipt.get("query_hash"), "query_dataset": "OPRA.PILLAR", "query_schema": "statistics", "query_stype_in": "raw_symbol", "query_start": receipt.get("query", {}).get("start"), "query_end": receipt.get("query", {}).get("end"), "query_symbol_count": len(symbols), "file_count": len(receipt.get("files")), "statistics_shards": len(results), "records": total_records, "provider_record_count": job.get("record_count"), "provider_record_count_matches": True, "vector_fields": VECTOR_FIELDS, "files": results, "replay_frames_produced": False, "source_gate_passed": False, "historical_performance_available": False, "limitations": ["Native vectors preserve source order and raw integers with no retiming or defaults.", "Undefined sentinels remain undefined. V1 quantity width is preserved unchanged with no version conversion and no missing OI invention.", "Capture regressions and clock leads are counted, not corrected. No ts_event ordering is assumed for non OI price rows.", "Partial and not_found counts describe symbol resolution only and do not prove tradability, listing, deliverables, ticks, or weekly eligibility.", "A complete source audit does not qualify quote, OI, Greeks, corporate actions, lifecycle, replay, coverage, or performance.", "No actual native data validation is claimed until the exact delivered corpus is inspected on Linux with the pinned decoder."]}
    write_json(args.output_directory / "audit.json", audit)
    print(json.dumps({"statistics_shards": len(results), "records": total_records, "source_gate_passed": False}), flush=True)
if __name__ == "__main__":
    main()
