#!/usr/bin/env python3
"""Price every frozen selective statistics query without purchasing data."""
import argparse
import base64
from datetime import datetime, timezone
from decimal import Decimal, ROUND_CEILING
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import urllib.error
import urllib.parse
import urllib.request

MANIFEST_SHA = "ccf48c229ce29db62e0d4aaa20cb06c733fdf4e528cf2d79f4da0e71a00adb2b"


def pairs_unique(pairs):
    obj = {}
    for key, value in pairs:
        if key in obj:
            raise ValueError("duplicate JSON key")
        obj[key] = value
    return obj


def private_input(path):
    if path.is_symlink() or stat.S_IMODE(path.stat().st_mode) != 0o600 or stat.S_IMODE(path.parent.stat().st_mode) != 0o700:
        raise ValueError("private input permissions invalid")
    return path.read_bytes()


def save(path, raw):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as stream:
        stream.write(raw)
        stream.flush()
        os.fsync(stream.fileno())
    fd = os.open(path.parent, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def encode(value):
    return (json.dumps(value, indent=2, sort_keys=True) + "\n").encode()


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", type=Path, required=True)
    parser.add_argument("--key-file", type=Path, required=True)
    parser.add_argument("--ledger", type=Path, required=True)
    parser.add_argument("--output-directory", type=Path, required=True)
    args = parser.parse_args()
    raw_manifest = args.manifest.read_bytes()
    if hashlib.sha256(raw_manifest).hexdigest() != MANIFEST_SHA:
        raise ValueError("frozen acquisition manifest differs")
    manifest = json.loads(raw_manifest, object_pairs_hook=pairs_unique)
    queries = [q for q in manifest["queries"] if q["schema"] == "statistics"]
    if len(queries) != 543 or manifest["daily_partitions"] != 126 or manifest["acquisition_symbol_days"] != 955003:
        raise ValueError("full study scope differs")
    ledger_before = private_input(args.ledger)
    key = private_input(args.key_file).decode("ascii").strip()
    if not key or ":" in key or "\n" in key:
        raise ValueError("credential invalid")
    out = args.output_directory
    if not out.exists():
        out.mkdir(mode=0o700)
    if out.is_symlink() or not out.is_dir() or stat.S_IMODE(out.stat().st_mode) != 0o700:
        raise ValueError("private output directory invalid")
    opener = urllib.request.build_opener(NoRedirect())
    responses, total, attempted, reused = [], Decimal(0), 0, 0
    interrupted = None
    for item in queries:
        name = item["filename"]
        if not re.fullmatch(r"2026-[0-9]{2}-[0-9]{2}-statistics-[0-9]{3}\.json", name):
            raise ValueError("query filename invalid")
        path = args.manifest.parent / name
        if path.is_symlink():
            raise ValueError("query is a symlink")
        raw_query = path.read_bytes()
        if len(raw_query) != item["bytes"] or hashlib.sha256(raw_query).hexdigest() != item["sha256"]:
            raise ValueError("query binding differs")
        query = json.loads(raw_query, object_pairs_hook=pairs_unique)
        if query["dataset"] != "OPRA.PILLAR" or query["schema"] != "statistics" or query["stype_in"] != "raw_symbol" or len(query["symbols"]) != item["symbols"] or not 1 <= item["symbols"] <= 2000:
            raise ValueError("query scope differs")
        receipt_path = out / (name.removesuffix(".json") + ".cost.json")
        body_path = out / (name.removesuffix(".json") + ".response.private.json")
        if receipt_path.exists():
            receipt = json.loads(private_input(receipt_path), object_pairs_hook=pairs_unique)
            body = private_input(body_path)
            if (receipt["query_file_sha256"] != item["sha256"] or receipt["http_status"] != 200
                    or hashlib.sha256(body).hexdigest() != receipt["response_sha256"]
                    or receipt["informational_not_issued_purchase_quote"] is not True):
                raise ValueError("existing metadata receipt differs")
            reused += 1
        else:
            if body_path.exists():
                raise ValueError("prior incomplete metadata response requires explicit disposition")
            fields = {k: query[k] for k in ["dataset", "schema", "stype_in", "start", "end"]}
            fields.update({"symbols": ",".join(query["symbols"]), "mode": "historical"})
            request = urllib.request.Request(
                "https://hist.databento.com/v0/metadata.get_cost",
                data=urllib.parse.urlencode(fields).encode(),
                headers={"Authorization": "Basic " + base64.b64encode((key + ":").encode()).decode(),
                         "Content-Type": "application/x-www-form-urlencoded"},
                method="POST",
            )
            attempted += 1
            try:
                try:
                    response = opener.open(request, timeout=30)
                except urllib.error.HTTPError as error:
                    response = error
                with response:
                    status, body = response.status, response.read(65537)
            except (urllib.error.URLError, TimeoutError, OSError):
                interrupted = {"query": name, "kind": "transport_failure_no_response", "paid": False}
                break
            if len(body) > 65536:
                raise ValueError("metadata response exceeds bound")
            save(body_path, body)
            if status != 200:
                interrupted = {"query": name, "kind": "http_failure", "http_status": status,
                               "response_sha256": hashlib.sha256(body).hexdigest(), "paid": False}
                break
            value = json.loads(body, parse_float=Decimal)
            if isinstance(value, bool) or not isinstance(value, (int, Decimal)) or not Decimal(value).is_finite() or value < 0:
                raise ValueError("metadata cost invalid")
            value = Decimal(value)
            nano = int((value * 1_000_000_000).to_integral_value(rounding=ROUND_CEILING))
            if nano > 2**63 - 1:
                raise ValueError("metadata cost exceeds representable bound")
            receipt = {"query_file": name, "query_file_sha256": item["sha256"],
                       "http_status": status, "endpoint": "/metadata.get_cost", "raw_cost_usd": str(value),
                       "cost_nanodollars_rounded_up": nano, "response_sha256": hashlib.sha256(body).hexdigest(),
                       "response_bytes": len(body), "observed_at": datetime.now(timezone.utc).isoformat(),
                       "informational_not_issued_purchase_quote": True}
            save(receipt_path, encode(receipt))
        body_value = json.loads(body, parse_float=Decimal)
        if isinstance(body_value, bool) or not isinstance(body_value, (int, Decimal)) or str(Decimal(body_value)) != receipt["raw_cost_usd"]:
            raise ValueError("metadata cost/body binding differs")
        value = Decimal(receipt["raw_cost_usd"])
        if not value.is_finite() or value < 0:
            raise ValueError("metadata receipt cost invalid")
        total += value
        responses.append(receipt)
        if len(responses) % 10 == 0 or len(responses) == len(queries):
            print(json.dumps({"priced_queries": len(responses), "required": len(queries),
                              "quoted_subtotal_usd": str(total), "paid_requests": 0}), flush=True)
    if private_input(args.ledger) != ledger_before:
        raise ValueError("research ledger changed during free pricing")
    summary = {"schema": "AutoWheelFullSelectiveStatisticsPricingV1", "manifest_sha256": MANIFEST_SHA,
               "observed_at": datetime.now(timezone.utc).isoformat(), "required_queries": len(queries),
               "priced_queries": len(responses), "complete_pricing": len(responses) == len(queries),
               "requests_attempted_this_run": attempted, "existing_responses_reused": reused,
               "quoted_total_or_subtotal_usd": str(total), "interruption": interrupted,
               "ledger_unchanged": True, "ledger_sha256": hashlib.sha256(ledger_before).hexdigest(),
               "paid_requests": 0, "source_gate_passed": False, "replay_frames_produced": False,
               "historical_performance_available": False, "responses": responses,
               "note": "Informational historical quotes; every future purchase requires a fresh issued Go quote. Zero cost is not absence proof."}
    save(out / ("summary-" + datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S%fZ") + ".json"), encode(summary))
    print(json.dumps({k: v for k, v in summary.items() if k != "responses"}), flush=True)


if __name__ == "__main__":
    main()
