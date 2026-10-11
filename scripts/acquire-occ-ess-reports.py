#!/usr/bin/env python3
"""Acquire dated public OCC ESS originals without qualifying historical terms."""
import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
DIRECTORY = ROOT / "research/simulation/2026-10-05/cycle39-public-primary"
PLAN_SHA = "ccf48c229ce29db62e0d4aaa20cb06c733fdf4e528cf2d79f4da0e71a00adb2b"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--calendar", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--retry-public-errors", action="store_true",
                        help="allow one fresh GET for a retained public HTTP5xx response")
    args = parser.parse_args()
    raw = args.calendar.read_bytes()
    calendar = json.loads(raw)
    days = calendar["session_dates"]
    if (calendar["manifest_sha256"] != PLAN_SHA or len(days) != 126
            or days != sorted(set(days)) or days[0] != "2026-04-06" or days[-1] != "2026-10-02"):
        raise ValueError("frozen acquisition calendar differs")
    for day in days:
        if datetime.strptime(day, "%Y-%m-%d").date().isoformat() != day:
            raise ValueError("noncanonical date")
    wanted = days + ["2026-10-05", "2026-10-06"]
    initial = json.loads((DIRECTORY / "occ-terms-route-receipts.json").read_bytes())
    receipts = {x["url"].split("reportDate=")[-1]: x for x in initial["receipts"]
                if "?reportDate=" in x["url"]}
    started = time.monotonic()
    for n, day in enumerate(wanted, 1):
        compact = day.replace("-", "")
        name = "occ-ess-" + compact + ".response"
        path = DIRECTORY / name
        previous = None
        saved = path.with_suffix(".receipt.json")
        if compact not in receipts and saved.exists():
            receipts[compact] = json.loads(saved.read_bytes())
        if compact in receipts:
            x = receipts[compact]
            data = path.read_bytes()
            if (x["path"] != str(path.relative_to(ROOT)) or len(data) != x["bytes"]
                    or hashlib.sha256(data).hexdigest() != x["sha256"]):
                raise ValueError("existing ESS capture differs")
            if x["status"] == 200 and data.startswith(b"1THE OPTIONS CLEARING CORPORATION"):
                continue
            if not args.retry_public_errors or not 500 <= x["status"] <= 599:
                raise ValueError("retained public error needs explicit bounded GET recovery")
            previous = x
            path = DIRECTORY / ("occ-ess-" + compact + "-retry1.response")
            if path.exists() or path.with_suffix(".receipt.json").exists():
                recovered = json.loads(path.with_suffix(".receipt.json").read_bytes())
                recovered_data = path.read_bytes()
                if (recovered.get("previous_response") != previous or recovered["status"] != 200
                        or recovered["path"] != str(path.relative_to(ROOT)) or recovered["url"] != previous["url"]
                        or len(recovered_data) != recovered["bytes"]
                        or hashlib.sha256(recovered_data).hexdigest() != recovered["sha256"]
                        or not recovered_data.startswith(b"1THE OPTIONS CLEARING CORPORATION")):
                    raise ValueError("public GET recovery already attempted; preserve its outcome")
                receipts[compact] = recovered
                continue
        time.sleep(max(0, 1 - (time.monotonic() - started)))
        started = time.monotonic()
        url = "https://marketdata.theocc.com/ess-reports?reportDate=" + compact
        request = urllib.request.Request(url, headers={"User-Agent": "Mozilla/5.0 AutoWheelResearch/1.0"})
        try:
            response = urllib.request.urlopen(request, timeout=20)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            data = response.read(4 * 1024 * 1024 + 1)
            status, final, content_type = response.status, response.geturl(), response.headers.get("Content-Type")
        if len(data) > 4 * 1024 * 1024:
            raise ValueError("ESS response exceeds bound")
        x = dict(url=url, final_url=final, status=status, content_type=content_type,
                 path=str(path.relative_to(ROOT)), bytes=len(data), sha256=hashlib.sha256(data).hexdigest(),
                 captured_at=datetime.now(timezone.utc).isoformat(), source_gate_passed=False,
                 historical_vintage_qualified=False)
        if previous is not None:
            x["previous_response"] = previous
        with path.open("xb") as output:
            output.write(data)
        with path.with_suffix(".receipt.json").open("x") as output:
            json.dump(x, output, indent=2, allow_nan=False)
            output.write("\n")
        receipts[compact] = x
        if status != 200 or not data.startswith(b"1THE OPTIONS CLEARING CORPORATION"):
            raise ValueError("ESS acquisition stopped; original response retained")
        if n % 20 == 0:
            print(json.dumps({"reports_retained": n, "required_reports": len(wanted), "financial_requests": 0}), flush=True)
    report = dict(schema="AutoWheelOCCDailyESSAcquisitionV1", observed_at=datetime.now(timezone.utc).isoformat(),
                  session_dates=days, supplemental_dates=wanted[-2:], calendar_sha256=hashlib.sha256(raw).hexdigest(),
                  acquisition_manifest_sha256=PLAN_SHA, script_sha256=hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                  receipts=[receipts[day.replace("-", "")] for day in wanted],
                  source_gate_passed=False, historical_vintage_qualified=False,
                  per_series_terms_qualified=False, historical_performance_available=False, financial_requests=0)
    with args.output.open("x") as output:
        json.dump(report, output, indent=2, allow_nan=False)
        output.write("\n")
    print(json.dumps({"reports_retained": len(wanted), "financial_requests": 0, "source_gate_passed": False}))


if __name__ == "__main__":
    main()
