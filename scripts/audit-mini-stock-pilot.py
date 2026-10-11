#!/usr/bin/env python3
"""Offline, facts-only audit of the single approved NFLX MINI stock pilot.

Does not acquire data, normalize replay inputs, qualify full coverage, or emit
prices/performance. Licensed response bytes remain in the private source directory.
"""

import argparse
import collections
import hashlib
import json
import re
from datetime import datetime, timezone
from decimal import Decimal, ROUND_CEILING, localcontext
from pathlib import Path


def require(condition, message):
    if not condition:
        raise ValueError(message)


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "duplicate JSON key")
        result[key] = value
    return result


def read(path, maximum):
    with path.open("rb") as file:
        raw = file.read(maximum + 1)
    require(len(raw) <= maximum, "source exceeds audit resource bound")
    return raw


def decode(raw):
    return json.loads(raw, object_pairs_hook=unique_object)


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def epoch_ns(value):
    require(isinstance(value, str) and value.isascii() and value.isdecimal(),
            "timestamp/price must be an exact unsigned integer string")
    number = int(value)
    require(0 < number < 2**63 - 1, "undefined, zero or overflow timestamp/price")
    return number


def utc_ns(value):
    seconds, nanos = divmod(value, 1_000_000_000)
    return datetime.fromtimestamp(seconds, timezone.utc).strftime("%Y-%m-%dT%H:%M:%S") + f".{nanos:09d}Z"


def audit(source, ledger_path, prior_ledger_path):
    query = {
        "dataset": "EQUS.MINI", "schema": "mbp-1", "symbols": ["NFLX"],
        "stype_in": "raw_symbol", "start": "2026-04-06T13:34:59Z",
        "end": "2026-04-06T13:35:00.000000001Z", "limit": 1000,
    }
    start_ns = 1775482499000000000
    decision_ns = 1775482500000000000
    end_ns = decision_ns + 1
    names = ["quote.json", "receipt.json", "source.jsonl"]
    bodies = {name: read(source / name, 16 << 20 if name == "source.jsonl" else 1 << 20) for name in names}
    quoted, receipt = decode(bodies["quote.json"]), decode(bodies["receipt.json"])
    ledger_raw, prior_raw = read(ledger_path, 1 << 20), read(prior_ledger_path, 1 << 20)
    ledger, prior = decode(ledger_raw), decode(prior_raw)
    require(quoted["query"] == query, "query differs from the approved pilot")
    canonical = json.dumps(query, separators=(",", ":")).encode()
    query_hash = digest(canonical)
    operation_id = digest(canonical + b"\x00/timeseries.get_range")
    for evidence in [quoted["quote"], receipt]:
        require(evidence["QueryHash"] == query_hash and evidence["OperationID"] == operation_id
                and evidence["Endpoint"] == "/timeseries.get_range", "query/operation identity mismatch")
    require(receipt["StatusCode"] == 200 and receipt["Bytes"] == len(bodies["source.jsonl"])
            and receipt["ResponseSHA256"] == digest(bodies["source.jsonl"]), "receipt/body mismatch")
    require(ledger["lifetime_nano"] == prior["lifetime_nano"] == 10_000_000_000, "lifetime budget changed")
    old = prior["reservations"]
    require(len(old) == 6 and ledger["reservations"][:len(old)] == old, "prior six immutable holds changed")
    require(len(ledger["reservations"]) == 7, "unexpected new operations")
    entry = ledger["reservations"][-1]
    require(entry["operation_id"] == operation_id and entry["query_hash"] == query_hash
            and entry["endpoint"] == "/timeseries.get_range" and entry["status"] == "complete"
            and entry["data_sha256"] == receipt["ResponseSHA256"] and entry["bytes"] == receipt["Bytes"]
            and entry["response_hash"] == quoted["quote"]["ResponseHash"], "ledger/query/source mismatch")
    raw_cost = quoted["quote"]["RawCost"]
    require(isinstance(raw_cost, str) and len(raw_cost) <= 52
            and re.fullmatch(r"[0-9]+(?:\.[0-9]+)?", raw_cost), "invalid exact decimal cost")
    with localcontext() as decimal_context:
        decimal_context.prec = 80
        quote_nano = int((Decimal(raw_cost) * 1_000_000_000).to_integral_value(rounding=ROUND_CEILING))
    require(type(quoted["quote"]["CostNano"]) is int and type(receipt["CostNano"]) is int
            and 0 <= quote_nano < 2**63 and receipt["CostNano"] == quoted["quote"]["CostNano"] == quote_nano,
            "receipt/quote/exact decimal cost mismatch")
    # Fixed $2/decimal-GB ceiling, 1020-byte maximum, checked pilot limit.
    record_bound_nano = (query["limit"] * 1020 * 2_000_000_000 + 999_999_999) // 1_000_000_000
    basis = max(quote_nano, record_bound_nano)
    hold = basis + (basis + 9) // 10
    require(entry["cost_nano"] == basis and entry["reserved_nano"] == receipt["ReservedNano"] == hold,
            "fixed ceiling reservation mismatch")
    reserved = sum(item["reserved_nano"] for item in ledger["reservations"])
    require(reserved <= ledger["lifetime_nano"], "cumulative budget exceeded")
    rows = [decode(line) for line in bodies["source.jsonl"].splitlines() if line.strip()]
    require(0 < len(rows) < query["limit"], "empty or record-capped sample")
    previous, last_book = None, None
    actions, sides, flags, identities = collections.Counter(), collections.Counter(), collections.Counter(), set()
    receives, events = [], []
    for row in rows:
        hd = row["hd"]
        require(row["symbol"] == "NFLX" and hd["rtype"] == 1 and hd["publisher_id"] == 95
                and hd["instrument_id"] == 11275, "observed pilot namespace/record identity changed")
        receive, event = epoch_ns(row["ts_recv"]), epoch_ns(hd["ts_event"])
        require(start_ns <= receive < end_ns and event <= receive, "out-of-window or future source clock")
        require(previous is None or receive >= previous, "capture time reversal")
        previous = receive
        require(row["action"] == "A" and row["side"] in ["A", "B"] and row["flags"] == 128
                and row["depth"] == 0 and row["sequence"] == 0 and row["ts_in_delta"] == 0,
                "different pilot action/flags; broader feed semantics require separate qualification")
        require(len(row["levels"]) == 1, "unexpected top-of-book depth")
        book = row["levels"][0]
        bid, ask = epoch_ns(book["bid_px"]), epoch_ns(book["ask_px"])
        require(bid <= ask and type(book["bid_sz"]) is int and type(book["ask_sz"]) is int
                and 0 < book["bid_sz"] <= 2**32 - 1 and 0 < book["ask_sz"] <= 2**32 - 1
                and book["bid_ct"] == book["ask_ct"] == 0, "invalid pilot book or aggregate counts")
        last_book = receive
        receives.append(receive)
        events.append(event)
        actions[row["action"]] += 1
        sides[row["side"]] += 1
        flags[str(row["flags"])] += 1
        identities.add((row["symbol"], hd["publisher_id"], hd["instrument_id"]))
    require(last_book <= decision_ns and decision_ns - last_book <= 30_000_000_000,
            "latest book not causal/fresh at the first decision")
    return {
        "schema_version": "AutoWheelMiniStockPilotAuditV1", "status": "bounded_pilot_observation_passed",
        "auditor_sha256": digest(Path(__file__).read_bytes()),
        "query": query, "operation_id": operation_id, "query_hash": query_hash,
        "private_source_directory": str(source),
        "source_artifacts": {name: {"sha256": digest(raw), "bytes": len(raw)} for name, raw in bodies.items()},
        "ledger": {"path": str(ledger_path), "sha256": digest(ledger_raw), "prior_sha256": digest(prior_raw),
                   "prior_six_entries_unchanged": True, "attempts": 7, "new_hold_nanodollars": hold,
                   "total_hold_nanodollars": reserved, "unreserved_nanodollars": 10_000_000_000 - reserved,
                   "invoice_charges_verified": False},
        "observations": {"records": len(rows), "record_cap_reached": False, "namespace_count": len(identities),
                         "first_capture": utc_ns(min(receives)), "last_capture": utc_ns(max(receives)),
                         "first_source_event": utc_ns(min(events)), "last_source_event": utc_ns(max(events)),
                         "capture_time_reversals": 0, "future_source_clock_records": 0,
                         "actions": dict(actions), "sides": dict(sides), "flags": dict(flags),
                         "invalid_books": 0, "latest_book_age_nanoseconds_at_first_decision": decision_ns - last_book},
        "source_contract": "MINI proprietary component-venue aggregated BBO; not represented as SIP NBBO",
        "limitations": ["Single-symbol one-second pilot only; no full-study coverage proof",
                        "Narrow observed identity/actions/flags only; not a general MBP-1 parser",
                        "No reset/trade/invalid/stale cases in this actual sample",
                        "Capture availability is preserved; no Robinhood arrival-clock claim",
                        "No replay input, prices, historical Greek qualification or performance emitted"],
        "full_source_gate_passed": False, "historical_performance_available": False,
        "production_changed": False,
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--ledger", type=Path, required=True)
    parser.add_argument("--prior-ledger", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    report = audit(args.source, args.ledger, args.prior_ledger)
    with args.output.open("x", encoding="utf-8") as file:
        file.write(json.dumps(report, indent=2) + "\n")
    print(json.dumps({"status": report["status"], "observations": report["observations"], "ledger": report["ledger"]}))


if __name__ == "__main__":
    main()
