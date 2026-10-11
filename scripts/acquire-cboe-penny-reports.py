#!/usr/bin/env python3
"""Retain the full study's public Cboe penny reports without causal qualification."""
import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
CALENDAR = ROOT / "research/simulation/2026-10-05/cycle40-contract-term-join-summary.json"
CALENDAR_SHA = "883d6b46fdd59d7858bad4f5d0aa319937c5ae52408994ce7c4b82e12881d8ec"
URL_BASE = "https://cdn.cboe.com/resources/us/options/market-statistics/penny-tick-type/cone/cone_options_rpt_penny_tick_type_"
MAX_BYTES = 2 * 1024 * 1024


def digest(data):
    return hashlib.sha256(data).hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", type=Path, required=True)
    args = parser.parse_args()
    raw = CALENDAR.read_bytes()
    if digest(raw) != CALENDAR_SHA:
        raise ValueError("frozen full-study calendar source differs")
    days = [x["date"] for x in json.loads(raw)["daily_audits"]]
    if (len(days) != 126 or days != sorted(set(days)) or days[0] != "2026-04-06"
            or days[-1] != "2026-10-02"):
        raise ValueError("full126-session calendar differs")
    for day in days:
        if datetime.strptime(day, "%Y-%m-%d").date().isoformat() != day:
            raise ValueError("noncanonical study date")
    directory = args.directory.resolve()
    directory.mkdir(parents=True, exist_ok=True)
    receipts, last_start = [], 0.0
    for index, day in enumerate(days, 1):
        name = "cone-penny-" + day + ".csv"
        path = directory / name
        receipt_path = directory / (name + ".receipt.json")
        url = URL_BASE + day.replace("-", "") + ".csv"
        if receipt_path.exists():
            receipt = json.loads(receipt_path.read_bytes())
            data = path.read_bytes()
            if (receipt["date"] != day or receipt["url"] != url or receipt["final_url"] != url
                    or receipt["path"] != str(path) or receipt["status"] != 200
                    or receipt["bytes"] != len(data) or len(data) > MAX_BYTES
                    or receipt["sha256"] != digest(data)
                    or not data.startswith(b"OSI Root,Underlying Description,Tick Type")):
                raise ValueError("retained response differs or failed; no automatic GET retry")
        else:
            # Refuse partial local capture rather than replacing an original.
            if path.exists():
                raise ValueError("unreceipted original retained; explicit recovery required")
            time.sleep(max(0.0, 1.0 - (time.monotonic() - last_start)))
            last_start = time.monotonic()
            request = urllib.request.Request(url, headers={"User-Agent": "Mozilla/5.0 AutoWheelResearch/1.0"})
            try:
                response = urllib.request.urlopen(request, timeout=20)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                data = response.read(MAX_BYTES + 1)
                receipt = dict(date=day, url=url, final_url=response.geturl(), status=response.status,
                               content_type=response.headers.get("Content-Type"),
                               http_last_modified=response.headers.get("Last-Modified"),
                               http_date=response.headers.get("Date"), http_etag=response.headers.get("ETag"),
                               captured_at=datetime.now(timezone.utc).isoformat(),
                               path=str(path), bytes=len(data), sha256=digest(data),
                               historical_first_availability=None, source_gate_passed=False)
            with path.open("xb") as output:
                output.write(data)
            with receipt_path.open("x") as output:
                json.dump(receipt, output, indent=2, allow_nan=False)
                output.write("\n")
            if (receipt["status"] != 200 or receipt["final_url"] != url or len(data) > MAX_BYTES
                    or not data.startswith(b"OSI Root,Underlying Description,Tick Type")):
                raise ValueError("public capture stopped; original response retained")
        receipts.append(receipt)
        if index % 20 == 0:
            print(json.dumps(dict(reports_retained=index, required_reports=126, financial_requests=0)), flush=True)
    # CSV semantics and coverage require the independent offline audit.
    report = dict(schema="AutoWheelCboePennyReportAcquisitionV1", observed_at=datetime.now(timezone.utc).isoformat(),
                  session_dates=days, calendar_path=str(CALENDAR), calendar_sha256=CALENDAR_SHA,
                  script_path=str(Path(__file__).resolve()), script_sha256=digest(Path(__file__).read_bytes()),
                  receipts=receipts, source_gate_passed=False, causal_tick_rules_qualified=False,
                  per_series_terms_qualified=False, historical_performance_available=False, financial_requests=0)
    with (directory / "acquisition.json").open("x") as output:
        json.dump(report, output, indent=2, allow_nan=False)
        output.write("\n")
    print(json.dumps(dict(reports_retained=len(receipts), source_gate_passed=False, financial_requests=0)))


if __name__ == "__main__":
    main()
