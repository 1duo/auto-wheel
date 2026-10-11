#!/usr/bin/env python3
"""Acquire bounded public daily vintages; qualification is a separate audit."""
import argparse
from datetime import date, datetime, timedelta, timezone
import hashlib
import json
import os
from pathlib import Path
import sys
import time
import urllib.parse
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
RELATIVE = Path("research/simulation/2026-10-05")
SPEC_SHA = "2ff4ae3765f4102a67382d645164ae4dfb6e036cc7ea3b113eab6dc24f4eac98"
CANDIDATE_SHA = "ac0f8dd3173e505338fe9afa545f9553929808db16e27068d241a18f8f15f156"


def save(path, data):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "wb") as output:
        output.write(data)
        output.flush()
        os.fsync(output.fileno())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--resume", action="store_true")
    args = parser.parse_args()
    if args.output.parent.resolve() != ROOT / RELATIVE or args.output.is_symlink():
        raise ValueError("bounded canonical research output required")
    if args.resume:
        info = args.output.stat()
        if not args.output.is_dir() or info.st_uid != os.getuid() or info.st_mode & 0o7777 != 0o700:
            raise ValueError("private original directory required")
    elif args.output.exists():
        raise ValueError("fresh output required")
    spec = (ROOT / RELATIVE / "spec.jsonl").read_bytes()
    if hashlib.sha256(spec).hexdigest() != SPEC_SHA:
        raise ValueError("frozen full study differs")
    candidate_path = ROOT / RELATIVE / "public-inputs/sofr-candidate-audit-20261006.json"
    candidate_bytes = candidate_path.read_bytes()
    if hashlib.sha256(candidate_bytes).hexdigest() != CANDIDATE_SHA:
        raise ValueError("previous reviewed candidate differs")
    candidate = json.loads(candidate_bytes)
    sessions = candidate["sessions"]
    if len(sessions) != 126 or candidate["candidate_rate_decisions"] != 9324:
        raise ValueError("full rate schedule missing")
    vintages = sorted({(date.fromisoformat(item["session"]) - timedelta(days=1)).isoformat() for item in sessions}
                      | {"2026-04-02", "2026-10-06"})
    if len(vintages) != 128:
        raise ValueError("full study and boundary controls differ")
    if not args.resume:
        args.output.mkdir(mode=0o700)
    reports = []
    started = time.monotonic()
    for vintage in vintages:
        if time.monotonic() - started > 900:
            raise ValueError("bounded public acquisition deadline exceeded")
        fields = dict(id="SOFR", cosd="2026-04-01", coed="2026-10-02", vintage_date=vintage,
                      revision_date=vintage, mode="alfred")
        url = "https://alfred.stlouisfed.org/graph/alfredgraph.csv?" + urllib.parse.urlencode(fields)
        original_receipt = args.output / (vintage + ".receipt.json")
        if args.resume and original_receipt.exists():
            original = args.output / (vintage + ".csv")
            for path in (original_receipt, original):
                info = path.lstat()
                if (path.is_symlink() or not path.is_file() or info.st_uid != os.getuid() or
                        info.st_nlink != 1 or info.st_mode & 0o7777 != 0o600 or not 0 < info.st_size <= 1 << 20):
                    raise ValueError("private original source required")
            prior = json.loads(original_receipt.read_bytes())
            raw = original.read_bytes()
            if (prior["vintage_date"] != vintage or prior["request_url"] != url or prior["method"] != "GET" or
                    prior["http_status"] != 200 or prior["financial_requests"] != 0 or prior["credentials_used"] is not False or
                    prior["source_bytes"] != len(raw) or prior["source_sha256"] != hashlib.sha256(raw).hexdigest() or
                    Path(prior["source_path"]).absolute() != original.absolute()):
                raise ValueError("original request/source differs")
            reports.append(prior)
            continue
        receipt = dict(vintage_date=vintage, request_url=url, method="GET", credentials_used=False,
                       financial_requests=0, full_source_qualification=False, transport_attempts=[])
        try:
            request = urllib.request.Request(url)
            for attempt in range(2):
                try:
                    with urllib.request.urlopen(request, timeout=20) as response:
                        data = response.read((1 << 20) + 1)
                        if (response.status != 200 or response.geturl() != url or
                                response.headers.get_content_type() not in ("application/csv", "text/csv") or
                                not 0 < len(data) <= 1 << 20):
                            raise ValueError("bounded original CSV response required")
                    receipt["transport_attempts"].append(dict(attempt=attempt + 1, accepted=True))
                    break
                except (TimeoutError, urllib.error.URLError, ConnectionError) as error:
                    receipt["transport_attempts"].append(dict(attempt=attempt + 1, error_type=type(error).__name__))
                    if attempt == 1:
                        raise
                    time.sleep(1)
            path = args.output / (vintage + ".csv")
            save(path, data)
            receipt.update(source_path=str(path), source_sha256=hashlib.sha256(data).hexdigest(),
                           source_bytes=len(data), http_status=200, observed_at=datetime.now(timezone.utc).isoformat())
        except Exception as error:
            receipt.update(error_type=type(error).__name__, response_accepted=False)
            failure = args.output / (vintage + (".failure-resume.json" if args.resume else ".failure.json"))
            save(failure, (json.dumps(receipt, indent=2) + "\n").encode())
            raise ValueError("public vintage acquisition stopped; evidence retained") from None
        save(args.output / (vintage + ".receipt.json"), (json.dumps(receipt, indent=2) + "\n").encode())
        reports.append(receipt)
        if len(reports) % 16 == 0:
            print(json.dumps(dict(vintages_downloaded=len(reports), required_vintages=128,
                                  source_qualified=False, financial_requests=0)), flush=True)
        time.sleep(0.25)
    report = dict(schema="AutoWheelALFREDSOFRDailyVintageAcquisitionV1", observed_at=datetime.now(timezone.utc).isoformat(),
                  spec_sha256=SPEC_SHA, candidate_sha256=CANDIDATE_SHA,
                  script_sha256=hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                  sessions=126, decision_cuts=9324, vintage_count=len(reports), receipts=reports,
                  credentials_used=False, financial_requests=0, source_qualified=False,
                  historical_greeks_qualified=False, historical_performance_available=False)
    save(args.output / "receipt.json", (json.dumps(report, indent=2, allow_nan=False) + "\n").encode())
    print(json.dumps({key: value for key, value in report.items() if key != "receipts"}), flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception:
        print("public SOFR vintage acquisition failed closed", file=sys.stderr)
        raise SystemExit(1)
