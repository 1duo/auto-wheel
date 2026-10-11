#!/usr/bin/env python3
"""Retain a recent private job index bound to an original ambiguous attempt.

A negative result never proves the financial request failed. No submit or release.
Copied from the retained broad read-only reconciler; transport bounds unchanged.
"""
import argparse
import base64
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import urllib.error
import urllib.parse
import urllib.request


def private_file(path):
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600:
        raise ValueError("private input permissions invalid")
    parent = path.parent.lstat()
    if not stat.S_ISDIR(parent.st_mode) or stat.S_IMODE(parent.st_mode) != 0o700:
        raise ValueError("private input parent permissions invalid")
    return path.read_bytes()


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON key")
        result[key] = value
    return result


def persist(path, payload):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as stream:
        stream.write(payload)
        stream.flush()
        os.fsync(stream.fileno())


def instant(value):
    if not isinstance(value, str):
        return None
    try:
        result = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None
    return result.astimezone(dt.timezone.utc) if result.tzinfo else None


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def symbol_matches(symbols, expected):
    # The provider returns either a JSON array or a comma-delimited string.
    # OSI spaces are meaningful; never strip or reorder an exact match.
    if isinstance(symbols, str):
        symbols = symbols.split(",")
    valid = (isinstance(symbols, list) and all(isinstance(s, str) and s for s in symbols)
             and len(symbols) <= 2000 and len(symbols) == len(set(symbols)))
    return valid and symbols == expected, valid and sorted(symbols) == sorted(expected)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--key-file", type=Path, required=True)
    parser.add_argument("--ledger", type=Path, required=True)
    parser.add_argument("--query", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--attempt", type=Path, required=True)
    args = parser.parse_args()
    attempt_raw = private_file(args.attempt)
    attempt = json.loads(attempt_raw, object_pairs_hook=unique_object)
    attempted_at = instant(attempt.get("at"))
    if attempted_at is None:
        raise ValueError("original attempt time invalid")
    since = (attempted_at - dt.timedelta(seconds=60)).isoformat()
    ledger = private_file(args.ledger)
    ledger_hash = hashlib.sha256(ledger).hexdigest()
    key = private_file(args.key_file).decode("ascii").strip()
    if not key or "\n" in key or ":" in key:
        raise ValueError("credential invalid")
    query_bytes = args.query.read_bytes()
    query = json.loads(query_bytes, object_pairs_hook=unique_object)
    from batch_acquisition_proof import canonical_query
    wire = canonical_query(query)
    operation = hashlib.sha256(wire + b"\0/batch.submit_job").hexdigest()
    rows = [r for r in json.loads(ledger, object_pairs_hook=unique_object)["reservations"]
            if r["operation_id"] == operation]
    if (len(rows) != 1 or rows[0]["status"] != "reserved"
            or attempt["operation_id"] != operation
            or attempt["query_hash"] != hashlib.sha256(wire).hexdigest()
            or any(attempt[k] != rows[0][k] for k in
                   ("operation_id", "query_hash", "endpoint", "cost_nano", "reserved_nano", "at"))):
        raise ValueError("original ambiguous attempt and query differ")
    args.output.mkdir(mode=0o700)
    params = urllib.parse.urlencode({
        "states": "queued,processing,done,expired",
        "since": since,
        "short": "False",
    })
    request = urllib.request.Request(
        "https://hist.databento.com/v0/batch.list_jobs?" + params,
        headers={"Authorization": "Basic " + base64.b64encode((key + ":").encode()).decode()},
        method="GET",
    )
    try:
        response = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect()).open(request, timeout=45)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        status = response.status
        raw = response.read(4 * 1024 * 1024 + 1)
    if len(raw) > 4 * 1024 * 1024:
        raise ValueError("job index exceeds bound")
    persist(args.output / "response.private.json", raw)
    summary = {
        "schema": "AutoWheelRecentReadOnlyBatchReconciliationV2",
        "since": since, "attempt_sha256": hashlib.sha256(attempt_raw).hexdigest(),
        "original_operation_id": operation,
        "negative_result_proves_absence": False,
        "observed_at": dt.datetime.now(dt.timezone.utc).isoformat(),
        "method": "GET", "endpoint": "/batch.list_jobs",
        "http_status": status, "response_sha256": hashlib.sha256(raw).hexdigest(),
        "response_bytes": len(raw), "query_file_sha256": hashlib.sha256(query_bytes).hexdigest(),
        "ledger_sha256_before": ledger_hash, "job_count": None,
        "index_query_fields_complete": False,
        "candidates": [], "submission_count": 0, "retry_count": 0,
        "hold_release_count": 0, "financial_outcome_resolved": False,
    }
    if status == 200:
        jobs = json.loads(raw, object_pairs_hook=unique_object)
        if not isinstance(jobs, list) or len(jobs) > 512:
            raise ValueError("job index shape invalid")
        summary["job_count"] = len(jobs)
        summary["index_query_fields_complete"] = all(
            isinstance(job, dict) and all(field in job for field in query)
            for job in jobs
        )
        for job in jobs:
            if not isinstance(job, dict):
                raise ValueError("job entry invalid")
            if job.get("dataset") != query["dataset"] or job.get("schema") != query["schema"]:
                continue
            job_id = job.get("id")
            if not isinstance(job_id, str) or not re.fullmatch(r"[A-Z0-9_-]{1,128}", job_id):
                raise ValueError("candidate ID invalid")
            symbols = job.get("symbols")
            exact_symbols, exact_set = symbol_matches(symbols, query["symbols"])
            candidate = {
                "job_id": job_id, "state": job.get("state"),
                "exact_symbol_order_match": exact_symbols,
                "exact_symbol_set_match": exact_set,
                "stype_in_match": job.get("stype_in") == query["stype_in"],
                "start_match": instant(job.get("start")) == instant(query["start"]) and instant(job.get("start")) is not None,
                "end_match": instant(job.get("end")) == instant(query["end"]) and instant(job.get("end")) is not None,
                "entry_sha256": hashlib.sha256(json.dumps(job, sort_keys=True, separators=(",", ":")).encode()).hexdigest(),
            }
            if candidate["state"] not in ("queued", "processing", "done", "expired"):
                raise ValueError("candidate state invalid")
            candidate["exact_query_match"] = all(candidate[field] for field in (
                "exact_symbol_order_match", "stype_in_match", "start_match", "end_match"))
            summary["candidates"].append(candidate)
    after = private_file(args.ledger)
    summary["ledger_sha256_after"] = hashlib.sha256(after).hexdigest()
    if after != ledger:
        raise ValueError("ledger changed during read-only reconciliation")
    payload = (json.dumps(summary, indent=2, sort_keys=True) + "\n").encode()
    persist(args.output / "summary.json", payload)
    print(payload.decode(), end="")


if __name__ == "__main__":
    main()
