#!/usr/bin/env python3
"""Offline bindings and finite event facts for the approved 30-second control."""
import argparse
import collections
import json
import re
from decimal import Decimal, ROUND_CEILING, localcontext
from pathlib import Path

import importlib.util

helper_path = Path(__file__).with_name("audit-mini-stock-pilot.py")
helper_spec = importlib.util.spec_from_file_location("mini_pilot", helper_path)
helper = importlib.util.module_from_spec(helper_spec)
helper_spec.loader.exec_module(helper)
require, read, decode, digest = helper.require, helper.read, helper.decode, helper.digest
epoch_ns = helper.epoch_ns


def audit(source, ledger_path, prior_path, peer_source):
    query = {"dataset": "EQUS.MINI", "schema": "mbp-1", "symbols": ["NFLX"],
             "stype_in": "raw_symbol", "start": "2026-04-06T13:34:30Z",
             "end": "2026-04-06T13:35:00.000000001Z", "limit": 10000}
    start, cutoff = 1775482470000000000, 1775482500000000000
    bodies = {name: read(source / name, 16 << 20 if name == "source.jsonl" else 1 << 20)
              for name in ["quote.json", "receipt.json", "source.jsonl"]}
    quote, receipt = decode(bodies["quote.json"]), decode(bodies["receipt.json"])
    canonical = json.dumps(query, separators=(",", ":")).encode()
    query_hash, operation = digest(canonical), digest(canonical + b"\x00/timeseries.get_range")
    require(quote["query"] == query, "unapproved query")
    for item in [quote["quote"], receipt]:
        require(item["OperationID"] == operation and item["QueryHash"] == query_hash
                and item["Endpoint"] == "/timeseries.get_range", "operation identity mismatch")
    require(receipt["StatusCode"] == 200 and receipt["Bytes"] == len(bodies["source.jsonl"])
            and receipt["ResponseSHA256"] == digest(bodies["source.jsonl"]), "receipt/body mismatch")
    raw_cost = quote["quote"]["RawCost"]
    require(isinstance(raw_cost, str) and len(raw_cost) <= 52
            and re.fullmatch(r"[0-9]+(?:\.[0-9]+)?", raw_cost), "invalid exact decimal cost")
    with localcontext() as context:
        context.prec = 80
        quote_nano = int((Decimal(raw_cost) * 10**9).to_integral_value(rounding=ROUND_CEILING))
    require(type(receipt["CostNano"]) is int and type(quote["quote"]["CostNano"]) is int
            and 0 <= quote_nano < 2**63
            and receipt["CostNano"] == quote["quote"]["CostNano"] == quote_nano, "cost mismatch")
    ledger_raw, prior_raw = read(ledger_path, 1 << 20), read(prior_path, 1 << 20)
    ledger, prior = decode(ledger_raw), decode(prior_raw)
    require(ledger["lifetime_nano"] == prior["lifetime_nano"] == 10_000_000_000, "budget changed")
    require(len(prior["reservations"]) == 7 and len(ledger["reservations"]) == 8
            and ledger["reservations"][:7] == prior["reservations"], "prior holds changed")
    entry = ledger["reservations"][-1]
    basis = max(quote_nano, query["limit"] * 1020 * 2)
    hold = basis + (basis + 9) // 10
    require(entry["operation_id"] == operation and entry["query_hash"] == query_hash
            and entry["endpoint"] == "/timeseries.get_range" and entry["status"] == "complete"
            and entry["data_sha256"] == receipt["ResponseSHA256"] and entry["bytes"] == receipt["Bytes"]
            and entry["response_hash"] == quote["quote"]["ResponseHash"]
            and entry["cost_nano"] == basis and entry["reserved_nano"] == receipt["ReservedNano"] == hold,
            "ledger/query/receipt/hold mismatch")
    reserved = sum(item["reserved_nano"] for item in ledger["reservations"])
    require(reserved <= ledger["lifetime_nano"], "cumulative cap exceeded")
    rows = [decode(line) for line in bodies["source.jsonl"].splitlines() if line.strip()]
    require(0 < len(rows) < query["limit"], "empty or capped source")
    actions, flags = collections.Counter(), collections.Counter()
    previous_receive, previous_book = None, None
    group, complete, multiple, trades, trade_matches, ties = [], 0, 0, 0, 0, 0
    for row in rows:
        header, book = row["hd"], row["levels"][0]
        require(all(type(header[key]) is int for key in ["rtype", "publisher_id", "instrument_id"])
                and all(type(row[key]) is int for key in ["flags", "depth", "sequence", "ts_in_delta"])
                and all(type(book[key]) is int for key in ["bid_ct", "ask_ct"]),
                "integer wire fields required; booleans are not integers")
        require(row["symbol"] == "NFLX" and header["rtype"] == 1 and header["publisher_id"] == 95
                and header["instrument_id"] == 11275, "pilot identity changed")
        receive, event = epoch_ns(row["ts_recv"]), epoch_ns(header["ts_event"])
        require(start <= receive <= cutoff and event <= receive, "noncausal/out-of-window clock")
        require(previous_receive is None or receive >= previous_receive, "capture reversal")
        ties += receive == previous_receive
        previous_receive = receive
        require(row["action"] in ["A", "C", "M", "T"] and row["side"] in ["A", "B", "N"]
                and row["flags"] in [0, 128] and row["depth"] == row["sequence"] == row["ts_in_delta"] == 0,
                "different source semantics require separate qualification")
        require(len(row["levels"]) == 1 and book["bid_ct"] == book["ask_ct"] == 0, "invalid depth/counts")
        bid, ask = epoch_ns(book["bid_px"]), epoch_ns(book["ask_px"])
        require(bid <= ask and type(book["bid_sz"]) is int and type(book["ask_sz"]) is int
                and 0 < book["bid_sz"] <= 2**32 - 1 and 0 < book["ask_sz"] <= 2**32 - 1,
                "invalid book")
        payload = (bid, ask, book["bid_sz"], book["ask_sz"])
        if row["action"] == "T":
            trades += 1
            trade_matches += payload == previous_book
        else:
            previous_book = payload
        actions[row["action"]] += 1
        flags[str(row["flags"])] += 1
        group.append(row)
        if row["flags"] & 128:
            complete += 1
            if len(group) > 1:
                require(len(group) == 2 and [item["action"] for item in group] == ["T", "C"]
                        and [item["flags"] for item in group] == [0, 128]
                        and group[0]["ts_recv"] == group[1]["ts_recv"]
                        and group[0]["hd"]["ts_event"] == group[1]["hd"]["ts_event"],
                        "different multi-record group requires separate qualification")
                multiple += 1
            group = []
    require(not group, "unfinished EOF event")
    peer_raw = read(peer_source, 16 << 20)
    peer_rows = [decode(line) for line in peer_raw.splitlines() if line.strip()]
    suffix = [row for row in rows if epoch_ns(row["ts_recv"]) >= 1775482499000000000]
    require(peer_rows and suffix == peer_rows, "independent one-second suffix differs")
    return {"status": "bounded_control_bindings_passed", "auditor_sha256": digest(Path(__file__).read_bytes()),
            "helper_sha256": digest(helper_path.read_bytes()), "query": query, "operation_id": operation,
            "query_hash": query_hash, "private_source_directory": str(source),
            "source_artifacts": {name: {"sha256": digest(raw), "bytes": len(raw)} for name, raw in bodies.items()},
            "ledger": {"sha256": digest(ledger_raw), "prior_sha256": digest(prior_raw),
                       "original_seven_holds_unchanged": True, "attempts": 8, "new_hold_nano": hold,
                       "total_hold_nano": reserved, "unreserved_nano": 10_000_000_000 - reserved,
                       "invoice_charges_verified": False},
            "observations": {"records": len(rows), "record_cap_reached": False, "actions": dict(actions),
                             "flags": dict(flags), "completed_events": complete, "multi_record_TC_events": multiple,
                             "capture_ties": ties, "trades": trades, "trades_matching_previous_book": trade_matches,
                             "independent_peer_records": len(peer_rows), "peer_sha256": digest(peer_raw),
                             "peer_suffix_exact_equal": True},
            "full_source_gate_passed": False, "historical_input_produced": False,
            "historical_performance_available": False, "production_changed": False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ["source", "ledger", "prior-ledger", "peer-source", "output"]:
        parser.add_argument("--" + name, type=Path, required=True)
    args = parser.parse_args()
    report = audit(args.source, args.ledger, args.prior_ledger, args.peer_source)
    with args.output.open("x", encoding="utf-8") as output:
        output.write(json.dumps(report, indent=2) + "\n")
    print(json.dumps({"status": report["status"], "observations": report["observations"], "ledger": report["ledger"]}))


if __name__ == "__main__":
    main()
