#!/usr/bin/env python3
"""Build an acquisition superset; never produce a causal replay chain."""
import argparse
from collections import Counter
from datetime import date, datetime, time, timedelta, timezone
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re

IDENTITY_SHA = "1f94acda64271ba781fa5138929075a2c6b103ce99674ed1426c8356bf85aa6e"
GAP_SHA = "6fd77c9bea3ac21f5161aa6698de70acd3c8038b4be8cf274370977765f8a201"
QUERY_SHA = "2ce4eb3a03da2df1aa216db27da182b3420dd4af8a3eb2d55827ec859bd0aca8"
OSI = re.compile(r"([A-Z0-9]{1,6} *)([0-9]{6})([PC])([0-9]{8})", re.ASCII)
DAY_NS = 86_400_000_000_000


def load_bound(path, expected):
    if path.is_symlink() or path.stat().st_size > 16 << 20:
        raise ValueError("audit input invalid")
    raw = path.read_bytes()
    if hashlib.sha256(raw).hexdigest() != expected:
        raise ValueError("audit input binding differs")
    return json.loads(raw)


def parse_osi(raw, roots):
    match = OSI.fullmatch(raw) if isinstance(raw, str) and len(raw) == 21 else None
    if match is None or len(match[1]) != 6 or match[1].rstrip() not in roots:
        raise ValueError("OSI scope differs")
    text = match[2]
    expiry = date(2000 + int(text[:2]), int(text[2:4]), int(text[4:]))
    strike = int(match[4]) * 1_000_000
    if strike <= 0:
        raise ValueError("OSI strike invalid")
    return match[1].rstrip(), expiry, match[3], strike


def midnight(day):
    return int(datetime.combine(day, time(), timezone.utc).timestamp()) * 1_000_000_000


def persist(path, value):
    raw = (json.dumps(value, indent=2, sort_keys=True) + "\n").encode()
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as stream:
        stream.write(raw)
        stream.flush()
        os.fsync(stream.fileno())
    return {"filename": path.name, "bytes": len(raw), "sha256": hashlib.sha256(raw).hexdigest()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--identity-audit", type=Path, required=True)
    parser.add_argument("--mapping-gap-audit", type=Path, required=True)
    parser.add_argument("--reference-query", type=Path, required=True)
    parser.add_argument("--output-directory", type=Path, required=True)
    parser.add_argument("--schemas", default="statistics,cmbp-1")
    args = parser.parse_args()
    schemas = args.schemas.split(",")
    if not schemas or len(schemas) != len(set(schemas)) or set(schemas) - {"statistics", "cmbp-1"}:
        raise ValueError("acquisition schema outside scope")
    identity = load_bound(args.identity_audit, IDENTITY_SHA)
    gap = load_bound(args.mapping_gap_audit, GAP_SHA)
    reference = load_bound(args.reference_query, QUERY_SHA)
    roots = {symbol.removesuffix(".OPT") for symbol in reference["symbols"]}
    if len(roots) != 15 or gap["identity_audit_sha256"] != IDENTITY_SHA:
        raise ValueError("frozen scope binding differs")
    expected, day = [], date(2026, 4, 6)
    holidays = {date(2026, 5, 25), date(2026, 6, 19), date(2026, 7, 3), date(2026, 9, 7)}
    while day <= date(2026, 10, 2):
        if day.weekday() < 5 and day not in holidays:
            expected.append(day.isoformat())
        day += timedelta(days=1)
    parts, gaps = identity["partitions"], gap["partitions"]
    if len(expected) != 126 or [p["date"] for p in parts] != expected or [p["date"] for p in gaps] != expected:
        raise ValueError("partition scope differs")
    if identity["source_gate_passed"] is not False or gap["source_gate_passed"] is not False:
        raise ValueError("unqualified acquisition input required")
    args.output_directory.mkdir(mode=0o700)
    results, queries, prior_missing = [], [], {}
    observed_total = selected_total = 0
    for part, gap_part in zip(parts, gaps, strict=True):
        day = date.fromisoformat(part["date"])
        name = day.isoformat() + ".identities.jsonl.gz"
        if part["identity_file"] != name:
            raise ValueError("identity filename differs")
        path = args.identity_audit.parent / name
        if path.is_symlink() or path.stat().st_size != part["identity_bytes"] or path.stat().st_size > 128 << 20:
            raise ValueError("compressed identity input invalid")
        compressed = path.read_bytes()
        if hashlib.sha256(compressed).hexdigest() != part["identity_sha256"]:
            raise ValueError("compressed identity binding differs")
        count = size = 0
        stream_hash = hashlib.sha256()
        seen, seen_ids, selected = set(), set(), set()
        root_counts, selected_roots, selected_kinds, selected_dtes = Counter(), Counter(), Counter(), Counter()
        lower = midnight(day)
        with gzip.GzipFile(fileobj=io.BytesIO(compressed), mode="rb") as stream:
            while line := stream.readline(4097):
                size += len(line)
                if len(line) > 4096 or not line.endswith(b"\n") or size > 64 << 20:
                    raise ValueError("identity stream bound exceeded")
                stream_hash.update(line)
                row = json.loads(line)
                if not isinstance(row, list) or len(row) != 9 or any(type(row[i]) is not int for i in [0, 1, 3, 4, 5, 6]):
                    raise ValueError("identity row shape invalid")
                root, expiry, kind, strike = parse_osi(row[2], roots)
                if (row[0] != 30 or not 0 < row[1] < 2**32 or not lower <= row[3] < lower + DAY_NS
                        or not 0 <= row[4] <= row[3] or row[5] != strike or row[6] != midnight(expiry)
                        or row[7] != kind or row[8] != "A" or row[2] in seen or row[1] in seen_ids):
                    raise ValueError("identity fact differs")
                seen.add(row[2])
                seen_ids.add(row[1])
                root_counts[root] += 1
                count += 1
                if count > 1_000_000:
                    raise ValueError("record bound exceeded")
                if 0 <= (expiry - day).days <= 8:
                    selected.add(row[2])
        if (count != part["records"] or count != gap_part["observed_definitions"]
                or stream_hash.hexdigest() != part["identity_jsonl_sha256"]
                or dict(root_counts) != part["observed_root_counts"]):
            raise ValueError("identity count or stream binding differs")
        newly_missing = {}
        for entry in gap_part["missing_DTE1_8"]:
            if (not isinstance(entry, list) or len(entry) != 3 or type(entry[1]) is not int
                    or not 0 < entry[1] < 2**32 or type(entry[2]) is not int):
                raise ValueError("mapping gap identity invalid")
            raw, instrument, dte = entry
            _, expiry, _, _ = parse_osi(raw, roots)
            if (not 1 <= dte <= 8 or dte != (expiry - day).days or raw in seen
                    or instrument in seen_ids or raw in newly_missing):
                raise ValueError("mapping gap fact differs")
            newly_missing[raw] = (instrument, expiry)
            selected.add(raw)
        if len(newly_missing) != gap_part["missing_expiry_buckets"].get("DTE1_8", 0):
            raise ValueError("mapping gap count differs")
        expiry_union = {raw for raw, (_, expiry) in prior_missing.items() if expiry == day}
        selected.update(expiry_union)
        prior_missing.update(newly_missing)
        observed_selected = len(selected & seen)
        for raw in selected:
            root, expiry, kind, _ = parse_osi(raw, roots)
            dte = (expiry - day).days
            if not 0 <= dte <= 8:
                raise ValueError("selected acquisition scope differs")
            selected_roots[root] += 1
            selected_kinds[kind] += 1
            selected_dtes[str(dte)] += 1
        sorted_symbols = sorted(selected)
        daily_queries = []
        for schema in schemas:
            for start in range(0, len(sorted_symbols), 2000):
                symbols = sorted_symbols[start:start + 2000]
                q = {"dataset": "OPRA.PILLAR", "schema": schema, "symbols": symbols,
                     "stype_in": "raw_symbol", "start": day.isoformat() + "T00:00:00Z",
                     "end": (day + timedelta(days=1)).isoformat() + "T00:00:00Z"}
                entry = persist(args.output_directory / f"{day.isoformat()}-{schema}-{start // 2000:03d}.json", q)
                entry.update({"date": day.isoformat(), "schema": schema, "symbols": len(symbols)})
                queries.append(entry)
                daily_queries.append(entry["filename"])
        results.append({"date": day.isoformat(), "observed_records_verified": count,
                        "observed_contracts_selected": observed_selected, "acquisition_symbols": len(selected),
                        "selected_root_counts": dict(selected_roots), "selected_kind_counts": dict(selected_kinds),
                        "selected_dte_counts": dict(selected_dtes), "current_gap_DTE1_8_included": len(newly_missing),
                        "prior_gap_expiry_day_acquisition_union": sorted(expiry_union),
                        "same_day_gap_count_without_enumerated_identities": gap_part["missing_expiry_buckets"].get("same_day", 0),
                        "full_mapping_gap_bucket_counts": gap_part["missing_expiry_buckets"],
                        "expiry_union_current_day_mapping_verified": False, "queries": daily_queries})
        observed_total += count
        selected_total += len(selected)
        print(json.dumps({"date": day.isoformat(), "acquisition_symbols": len(selected), "queries": len(daily_queries)}), flush=True)
    if observed_total != identity["records"]:
        raise ValueError("corpus record count differs")
    manifest = {"schema": "AutoWheelSelectiveAcquisitionUnionV1", "status": "acquisition_superset_unqualified",
                "script_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                "identity_audit_sha256": IDENTITY_SHA, "mapping_gap_audit_sha256": GAP_SHA,
                "reference_query_sha256": QUERY_SHA, "partitions": results, "queries": queries,
                "daily_partitions": len(results), "observed_records_verified": observed_total,
                "acquisition_symbol_days": selected_total, "query_count": len(queries),
                "source_gate_passed": False, "replay_frames_produced": False,
                "historical_performance_available": False, "paid_requests": 0,
                "limitations": ["Retrospective acquisition union is never a causal listing or tradability decision.",
                                "Undefined contract terms remain unknown; no term defaults or performance are produced.",
                                "Prior missing identities on expiry day are query subjects, not proof of current-day mappings."]}
    persist(args.output_directory / "manifest.json", manifest)
    fd = os.open(args.output_directory, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)
    print(json.dumps({"partitions": len(results), "records_verified": observed_total,
                      "acquisition_symbol_days": selected_total, "queries": len(queries), "source_gate_passed": False}))


if __name__ == "__main__":
    main()
