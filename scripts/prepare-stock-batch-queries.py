#!/usr/bin/env python3
"""Partition the unchanged 15-symbol stock scope into 126 frozen UTC days.

This prepares queries without credentials, HTTP, purchases, or qualification.
The earlier ambiguous full-window stock operation is never resubmitted.
"""
import argparse
from datetime import date, timedelta
import hashlib
import json
import os
from pathlib import Path

from batch_acquisition_proof import canonical_query, read_json

SESSION_MANIFEST_SHA = "ccf48c229ce29db62e0d4aaa20cb06c733fdf4e528cf2d79f4da0e71a00adb2b"
UNIVERSE = ["NFLX", "TSLA", "NVDA", "NBIS", "IREN", "NOW", "AMD", "AMZN", "GOOGL",
            "META", "MSFT", "PLTR", "UBER", "AVGO", "ORCL"]


def persist(path, value):
    raw = (json.dumps(value, indent=2) + "\n").encode()
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as stream:
        stream.write(raw)
        stream.flush()
        os.fsync(stream.fileno())
    return {"filename": path.name, "sha256": hashlib.sha256(raw).hexdigest(), "bytes": len(raw)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--session-manifest", type=Path, required=True)
    parser.add_argument("--reference-query", type=Path, required=True)
    parser.add_argument("--output-directory", type=Path, required=True)
    args = parser.parse_args()
    raw = args.session_manifest.read_bytes()
    if hashlib.sha256(raw).hexdigest() != SESSION_MANIFEST_SHA:
        raise ValueError("frozen session manifest differs")
    sessions = read_json(args.session_manifest)
    query = read_json(args.reference_query)
    expected = {"dataset": "EQUS.MINI", "schema": "mbp-1", "symbols": UNIVERSE,
                "stype_in": "raw_symbol", "start": "2026-04-06T00:00:00Z", "end": "2026-10-03T00:00:00Z"}
    if query != expected:
        raise ValueError("frozen stock reference differs")
    canonical_query(query)
    days = [part["date"] for part in sessions["partitions"]]
    if len(days) != 126 or days != sorted(set(days)) or days[0] != "2026-04-06" or days[-1] != "2026-10-02":
        raise ValueError("frozen session coverage differs")
    args.output_directory.mkdir(mode=0o700)
    entries = []
    for text in days:
        day = date.fromisoformat(text)
        if day.isoformat() != text or day.weekday() >= 5:
            raise ValueError("frozen session date invalid")
        daily = dict(query, start=text + "T00:00:00Z", end=(day + timedelta(days=1)).isoformat() + "T00:00:00Z")
        canonical_query(daily)
        entry = persist(args.output_directory / (text + "-stock-mbp-1.json"), daily)
        entry.update(date=text, symbols=15)
        entries.append(entry)
    manifest = {"schema_version": "AutoWheelStockDailyAcquisitionQueriesV1", "session_manifest_sha256": SESSION_MANIFEST_SHA,
                "reference_query_sha256": hashlib.sha256(args.reference_query.read_bytes()).hexdigest(),
                "script_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                "queries": entries, "daily_partitions": len(entries), "universe": UNIVERSE,
                "paid_requests": 0, "source_gate_passed": False, "historical_performance_available": False,
                "limitations": ["Each query is distinct from the retained ambiguous full-window attempt.",
                                "EQUS.MINI is derived BBO data; dataset provenance is preserved.",
                                "Query preparation proves no acquisition, execution, lifecycle, or source qualification."]}
    persist(args.output_directory / "manifest.json", manifest)
    fd = os.open(args.output_directory, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)
    print(json.dumps({"daily_partitions": len(entries), "symbols_per_day": 15, "paid_requests": 0}))


if __name__ == "__main__":
    main()
