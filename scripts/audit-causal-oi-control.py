#!/usr/bin/env python3
"""Offline, metadata-only comparison of a private Go OI view to raw statistics.

No network, credentials, trading, source completeness or performance claims.
Licensed quantities stay in the source and private Go view; output omits them.
"""
import argparse
import datetime as dt
import hashlib
import json
from pathlib import Path

MAX_BYTES = 16 * 1024 * 1024
MAX_ROWS = 10_000


def read(path):
    with path.open("rb") as source:
        data = source.read(MAX_BYTES + 1)
    if len(data) > MAX_BYTES:
        raise ValueError("oversized control input")
    return data


def decode(data):
    def unique(pairs):
        out = {}
        for key, value in pairs:
            if key in out:
                raise ValueError("duplicate JSON field")
            out[key] = value
        return out
    return json.loads(data, object_pairs_hook=unique,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite JSON")))


def digest(data):
    return hashlib.sha256(data).hexdigest()


def nanos(text):
    # Retain all nine fractional digits; datetime alone truncates nanoseconds.
    base, fraction = text.removesuffix("Z").split(".") if "." in text else (text.removesuffix("Z"), "")
    stamp = dt.datetime.strptime(base, "%Y-%m-%dT%H:%M:%S").replace(tzinfo=dt.timezone.utc)
    if not text.endswith("Z") or len(fraction) > 9 or (fraction and not fraction.isdigit()):
        raise ValueError("expected UTC RFC3339Nano")
    return int(stamp.timestamp()) * 1_000_000_000 + int(fraction.ljust(9, "0") or "0")


def audit(sample, view_path, ledger_path):
    raw = read(sample / "source.jsonl")
    receipt = decode(read(sample / "receipt.json"))
    quoted = decode(read(sample / "quote.json"))
    query = quoted["query"]
    canonical = json.dumps({k: query[k] for k in
                            ("dataset", "schema", "symbols", "stype_in", "start", "end", "limit")},
                           separators=(",", ":"), ensure_ascii=False).encode()
    qhash = digest(canonical)
    operation = digest(canonical + b"\0/timeseries.get_range")
    if (query["dataset"], query["schema"], query["stype_in"]) != ("OPRA.PILLAR", "statistics", "raw_symbol"):
        raise ValueError("unexpected source scope")
    if receipt["StatusCode"] != 200 or receipt["Endpoint"] != "/timeseries.get_range":
        raise ValueError("unsuccessful source receipt")
    if (receipt["QueryHash"], receipt["OperationID"]) != (qhash, operation):
        raise ValueError("receipt query identity mismatch")
    if (quoted["quote"]["QueryHash"], quoted["quote"]["OperationID"]) != (qhash, operation):
        raise ValueError("cost quote query identity mismatch")
    if (receipt["ResponseSHA256"], receipt["Bytes"]) != (digest(raw), len(raw)):
        raise ValueError("receipt source mismatch")
    ledger_raw = read(ledger_path)
    ledger = decode(ledger_raw)
    entries = [r for r in ledger["reservations"] if r["operation_id"] == operation]
    if len(entries) != 1 or entries[0]["status"] != "complete":
        raise ValueError("missing complete source ledger entry")
    if (entries[0]["query_hash"], entries[0]["data_sha256"], entries[0]["bytes"]) != (qhash, digest(raw), len(raw)):
        raise ValueError("ledger source identity mismatch")
    view_raw = read(view_path)
    view = decode(view_raw)
    if view["schema_version"] != "AutoWheelCausalOIViewV1" or view["status"] != "partial_unqualified":
        raise ValueError("invalid bounded control view")
    if view["source_sha256"] != digest(raw):
        raise ValueError("view source mismatch")
    cutoff = nanos(view["as_of"])
    rows = [decode(line) for line in raw.splitlines() if line.strip()]
    if not 0 < len(rows) < query["limit"] <= MAX_ROWS or view["source_records"] != len(rows):
        raise ValueError("invalid below-cap control count")
    by_symbol = {}
    skewed = 0
    for i, row in enumerate(rows):
        capture, event = int(row["ts_recv"]), int(row["hd"]["ts_event"])
        if row["symbol"] not in query["symbols"] or row["hd"]["rtype"] != 24:
            raise ValueError("record outside query")
        if not nanos(query["start"]) <= capture < nanos(query["end"]):
            raise ValueError("capture outside source interval")
        if i and capture < int(rows[i - 1]["ts_recv"]):
            raise ValueError("source capture order mismatch")
        if row["stat_type"] != 9:
            skewed += event > capture
            continue
        if event > capture or int(row["ts_ref"]) != 2**64 - 1:
            raise ValueError("control OI timestamp/reference shape changed")
        if capture <= cutoff:
            by_symbol.setdefault(row["symbol"], {})[row["hd"]["publisher_id"]] = (i, row)
    expected = {}
    for symbol, copies in by_symbol.items():
        quantities = {row["quantity"] for _, row in copies.values()}
        if len(quantities) != 1:
            raise ValueError("publisher quantities conflict")
        _, last = max(copies.values(), key=lambda pair: (int(pair[1]["ts_recv"]), pair[0]))
        expected[symbol] = (last["hd"]["instrument_id"], next(iter(quantities)),
                            int(last["ts_recv"]), int(last["hd"]["ts_event"]), len(copies))
    if set(view["snapshot"]) != set(expected):
        raise ValueError("snapshot symbol set mismatch")
    for symbol, record in view["snapshot"].items():
        actual = (record["InstrumentID"], record["Quantity"], nanos(record["At"]),
                  nanos(record["EventAt"]), record["Copies"])
        if record["Symbol"] != symbol or actual != expected[symbol]:
            raise ValueError("snapshot value or original timestamp mismatch")
    return {"schema_version": "AutoWheelCausalOIControlAuditV1", "status": "partial_unqualified",
            "source_sha256": digest(raw), "view_sha256": digest(view_raw),
            "ledger_sha256": digest(ledger_raw), "operation_id": operation, "query_sha256": qhash,
            "as_of": view["as_of"], "source_records": len(rows), "matched_symbols": len(expected),
            "mismatches": 0, "publisher_copy_counts": sorted({v[4] for v in expected.values()}),
            "ignored_price_rows_with_source_clock_lead": skewed,
            "all_OI_reference_timestamps_explicitly_missing": True,
            "full_study_coverage_proved": False, "historical_performance": None,
            "new_paid_request_sent": False, "licensed_quantities_exported": False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--sample", required=True, type=Path)
    parser.add_argument("--view", required=True, type=Path)
    parser.add_argument("--ledger", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    result = audit(args.sample, args.view, args.ledger)
    # Metadata only. Refuse to overwrite an earlier control result.
    with args.output.open("x") as dest:
        json.dump(result, dest, indent=2)
        dest.write("\n")
    print("Matched %d causal OI symbols; zero mismatches; unqualified for replay." % result["matched_symbols"])


if __name__ == "__main__":
    main()
