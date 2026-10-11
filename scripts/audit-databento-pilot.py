#!/usr/bin/env python3
"""Audit already-downloaded pilot files. No network, credentials or trading."""
import argparse
from collections import Counter
import datetime as dt
import hashlib
import json
from pathlib import Path

MISSING = {
    "contract_multiplier": 2147483647,
    "min_price_increment": 9223372036854775807,
    "activation": 18446744073709551615,
    "maturity_week": 255,
    "ts_ref": 18446744073709551615,
}


def read_json(path):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError("duplicate JSON field")
            result[key] = value
        return result
    return json.loads(path.read_text(), object_pairs_hook=unique,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite JSON")))


def source_manifest(path):
    result = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b""):
            result.update(chunk)
    return {"path": str(path.resolve()), "bytes": path.stat().st_size,
            "sha256": result.hexdigest()}


def iso_ns(value):
    if value is None:
        return None
    return dt.datetime.fromtimestamp(value // 1_000_000_000, dt.timezone.utc).strftime(
        "%Y-%m-%dT%H:%M:%S") + ".%09dZ" % (value % 1_000_000_000)


def records(path):
    count = 0
    classes, statistics, expirations, publishers = Counter(), Counter(), Counter(), Counter()
    missing = Counter()
    symbols, instruments = set(), set()
    first = last = None
    with path.open() as stream:
        for line in stream:
            record = json.loads(line)
            if not isinstance(record, dict):
                raise ValueError("expected a JSONL record object")
            count += 1
            hd = record.get("hd", {})
            # Definitions carry raw_symbol; map_symbols adds symbol to other
            # schemas. Both refer to the same historical OSI identity.
            symbol = record.get("raw_symbol") or record.get("symbol")
            if symbol:
                symbols.add(symbol)
            if hd.get("instrument_id") is not None:
                instruments.add(hd["instrument_id"])
            if record.get("instrument_class"):
                classes[record["instrument_class"]] += 1
            if record.get("stat_type") is not None:
                statistics[str(record["stat_type"])] += 1
            if record.get("expiration") is not None:
                expirations[str(record["expiration"])] += 1
            if hd.get("publisher_id") is not None:
                publishers[str(hd["publisher_id"])] += 1
            for field, sentinel in MISSING.items():
                if field in record and int(record[field]) == sentinel:
                    missing[field] += 1
            if record.get("ts_recv") is not None:
                at = int(record["ts_recv"])
                if at != 18446744073709551615:
                    first = at if first is None else min(first, at)
                    last = at if last is None else max(last, at)
    return {"records": count, "raw_symbols": len(symbols), "instrument_ids": len(instruments),
            "instrument_classes": dict(classes), "stat_types": dict(statistics),
            "expiration_count": len(expirations), "publishers": dict(publishers),
            "missing_sentinel_counts": dict(missing), "first_capture_at": iso_ns(first),
            "last_capture_at": iso_ns(last)}


def audit(root, ledger_path, extra_samples=()):
    ledger = read_json(ledger_path)
    reservations = ledger["reservations"]
    by_operation = {r["operation_id"]: r for r in reservations}
    samples, files = [], [source_manifest(ledger_path)]
    directories = [root / ("pilot-NFLX-" + schema + "-20261005")
                   for schema in ("definition", "statistics", "cmbp-1")]
    for name in extra_samples:
        if Path(name).name != name or name in (".", ".."):
            raise ValueError("extra sample must be a directory name under root")
        directories.append(root / name)
    audited_operations = set()
    for directory in directories:
        quote_path = directory / "quote.json"
        quote = read_json(quote_path)
        schema = quote["query"]["schema"]
        files.append(source_manifest(quote_path))
        source = directory / "source.jsonl"
        receipt_path = directory / ("receipt.json" if source.is_file() else "attempt.json")
        receipt = read_json(receipt_path)
        if receipt["OperationID"] in audited_operations:
            raise ValueError("duplicate sample operation")
        audited_operations.add(receipt["OperationID"])
        files.append(source_manifest(receipt_path))
        reservation = by_operation[receipt["OperationID"]]
        sample = {"schema": schema, "directory": directory.name,
                  "query": quote["query"], "quote": quote["quote"],
                  "receipt": receipt, "reservation": reservation, "received": source.is_file()}
        if source.is_file():
            manifest = source_manifest(source)
            files.append(manifest)
            if manifest["sha256"] != receipt["ResponseSHA256"] or manifest["bytes"] != receipt["Bytes"]:
                raise ValueError("receipt/source mismatch")
            if manifest["sha256"] != reservation["data_sha256"] or reservation["status"] != "complete":
                raise ValueError("ledger/source mismatch")
            sample["source"] = manifest
            sample["audit"] = records(source)
            sample["reached_requested_limit"] = sample["audit"]["records"] == quote["query"]["limit"]
        samples.append(sample)
    used = sum(r["reserved_nano"] for r in reservations)
    if ledger["lifetime_nano"] != 10_000_000_000 or not 0 <= used <= ledger["lifetime_nano"]:
        raise ValueError("budget evidence invalid")
    return {
        "schema_version": "AutoWheelDatabentoPilotAuditV1", "status": "partial_unqualified",
        "observed_at": dt.datetime.now(dt.timezone.utc).isoformat(), "host": "msi",
        "budget": {"cap_nanodollars": ledger["lifetime_nano"], "reserved_nanodollars": used,
                   "remaining_nanodollars": ledger["lifetime_nano"] - used,
                   "ambiguous_requests": sum(r["status"] != "complete" for r in reservations),
                   "billable_requests": len(reservations), "actual_billed_cost": "unverified"},
        "source_files": files, "samples": samples,
        "attempts_without_sample_artifacts": sorted(set(by_operation) - audited_operations),
        "findings": [
            "The request limit bounds these samples; it does not prove the full chain or six-month coverage.",
            "Definition capture proves observation at capture time, not the actual first-listing date.",
            "Missing multiplier, tick, activation and maturity-week sentinels must not be normalized as valid values.",
            "OPRA expiration is a UTC-midnight date; exchange exercise/close time requires an audited calendar mapping.",
            "Open-interest publication capture is available; missing ts_ref must not be used as a historical as-of timestamp.",
            "Open interest may repeat by venue; values must not be summed without a normalization audit.",
            "The HTTP504 quote attempt was not retried. Its financial outcome is unknown and its budget hold is retained.",
            "Stock quotes, corporate actions, American-option Greeks and continuous execution coverage remain unqualified.",
        ],
        "historical_performance": None,
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--ledger", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--extra-sample", action="append", default=[],
                        help="additional receipt/source directory name under root")
    args = parser.parse_args()
    result = audit(args.root, args.ledger, args.extra_sample)
    with args.output.open("x") as stream:
        json.dump(result, stream, indent=2, allow_nan=False)
        stream.write("\n")
    print(json.dumps({"status": result["status"], "budget": result["budget"],
                      "sample_records": {s["directory"]: s.get("audit", {}).get("records", 0)
                                         for s in result["samples"]}}))


if __name__ == "__main__":
    main()
