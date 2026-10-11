#!/usr/bin/env python3
"""Retrieve acknowledged statistics jobs only; never make a financial request."""
import argparse
from datetime import datetime, timezone
import hashlib
import importlib.util
import json
from pathlib import Path
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", type=Path, required=True)
    parser.add_argument("--source-run", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    path = Path(__file__).with_name("acquire-prefetched-statistics-plan.py")
    spec = importlib.util.spec_from_file_location("prefetch", path)
    m = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(m)
    p = m.load_preflight()
    ledger = Path("/home/yuduo/.local/share/autowheel-research/databento-budget.json")
    _, expected = p.ledger(ledger)
    initial = p.read_json(args.source_run / "ledger-before.private.json")
    if expected["reservations"][:len(initial["reservations"])] != initial["reservations"]:
        raise ValueError("original journal prefix differs")
    jobs = []
    for out in sorted(args.source_run.glob("2026-*-statistics-*")):
        if not out.is_dir():
            continue
        ack = p.read_json(out / "submit/ack.json")
        operation = ack["operation_id"]
        query = args.base / "cycle24-selective-acquisition-queries-20261006" / (out.name + ".json")
        wire = p.canonical_query(p.read_json(query))
        if p.digest(wire + b"\0/batch.submit_job") != operation:
            raise ValueError("original query and acknowledgement differ")
        matches = [r for r in expected["reservations"] if r["operation_id"] == operation]
        if len(matches) != 1 or matches[0]["query_hash"] != p.digest(wire):
            raise ValueError("original held query missing")
        jobs.append((out, query, operation, matches[0]["status"]))
    if not jobs:
        raise ValueError("no original acknowledged jobs")
    args.output.mkdir(mode=0o700)
    p.save(args.output / "ledger-before.private.json", expected)
    p.save(args.output / "plan.json", {"schema": "AutoWheelPrepaidStatisticsRecoveryRunV1",
           "jobs": len(jobs), "source_run": str(args.source_run), "financial_requests": 0,
           "runner_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
           "prefetch_sha256": hashlib.sha256(path.read_bytes()).hexdigest(), "source_qualified": False})
    tools = {k: args.base / v[0] for k, v in p.TOOLS.items()}
    for name, (_, wanted) in p.TOOLS.items():
        if hashlib.sha256(tools[name].read_bytes()).hexdigest() != wanted:
            raise ValueError("accepted research binary differs")
    completed, skipped = [], 0
    for original, query, operation, status in jobs:
        if status == "acquired":
            skipped += 1
            continue
        if status != "reserved":
            raise ValueError("prepaid row status differs")
        out = args.output / original.name
        out.mkdir(mode=0o700)
        common = ["--query", query, "--ack", original / "submit/ack.json",
                  "--ack-body", original / "submit/job.json", "--key-file",
                  Path("/home/yuduo/.config/autowheel/databento-pilot.key")]
        for poll in range(1080):
            if p.ledger(ledger)[1] != expected:
                raise ValueError("unexpected journal mutation")
            receipt = m.observe_readonly(p, tools["retrieve"], common, out, "observe-" + str(poll))
            if receipt["state"] == "done":
                break
            if receipt["state"] not in ("queued", "processing"):
                raise ValueError("unknown original prepaid state")
            time.sleep(20)
        else:
            raise ValueError("prepaid deadline; retain all holds")
        if p.run(tools["retrieve"], [*common, "--output", out / "acquired", "--timeout", "30m", "--download"], out, "download") != 0:
            raise ValueError("prepaid download stopped")
        if p.ledger(ledger)[1] != expected:
            raise ValueError("unexpected journal mutation")
        proof = out / "proof.json"
        if p.run(tools["finalize"], ["--query", query, "--ack", original / "submit/ack.json",
                 "--ack-body", original / "submit/job.json", "--source", out / "acquired", "--proof", proof,
                 "--python", args.base / "dbn-env-0.71.0/bin/python", "--ledger", ledger, "--timeout", "30m"], out, "finalize") != 0:
            raise ValueError("offline native verification stopped")
        audit = p.read_json(proof)
        _, current = p.ledger(ledger)
        m.finalized(expected, current, operation, p.digest(p.private(proof)), audit["total_bytes"])
        expected = current
        result = {"query": original.name, "records": audit["native"]["records"],
                  "all_eof_verified": audit["native"]["all_eof_verified"],
                  "proof_sha256": p.digest(p.private(proof)), "financial_requests": 0,
                  "source_qualified": False}
        p.save(out / "aggregate.json", result)
        completed.append(result)
        print(json.dumps({"completed": len(completed), "required": len(jobs) - skipped, **result}), flush=True)
    p.save(args.output / "summary.json", {"schema": "AutoWheelPrepaidStatisticsRecoveryCompleteV1",
           "observed_at": datetime.now(timezone.utc).isoformat(), "completed": completed,
           "already_acquired_skipped": skipped, "financial_requests": 0, "financial_retries": 0,
           "source_qualified": False})


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(json.dumps({"stopped": True, "error_type": type(error).__name__,
                          "financial_requests": 0, "holds_released": False}), flush=True)
        raise SystemExit(1)
