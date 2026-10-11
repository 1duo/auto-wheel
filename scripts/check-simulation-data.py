#!/usr/bin/env python3
"""Small read-only entitlement probes; never downloads a corpus or trades.

Uses the existing loopback Theta Terminal without reading credentials. Captures
bounded responses at both ends of the requested window. A successful probe is
not proof of complete point-in-time chains or corporate-action coverage.
"""
import argparse
import datetime as dt
import hashlib
import json
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    opener = urllib.request.build_opener(NoRedirect())
    probes = []
    for date, expiry in (("2026-04-06", "2026-04-10"), ("2026-10-02", "2026-10-09")):
        for endpoint in ("quote", "greeks/first_order", "open_interest"):
            query = dict(symbol="NFLX", expiration=expiry, date=date, strike_range=1, format="json")
            if endpoint != "open_interest":
                query.update(interval="1s", start_time="09:35:00", end_time="09:35:00")
            url = "http://127.0.0.1:25503/v3/option/history/" + endpoint + "?" + urllib.parse.urlencode(query)
            try:
                response = opener.open(url, timeout=8)
            except urllib.error.HTTPError as error:
                response = error
            except (urllib.error.URLError, TimeoutError):
                probes.append(dict(endpoint=endpoint, query=query, status=None, error="Terminal unavailable or request timed out"))
                continue
            with response:
                payload = response.read(4097)
                probes.append(dict(endpoint=endpoint, query=query, status=response.code,
                                   response_preview=payload[:4096].decode("utf-8", errors="replace"),
                                   truncated=len(payload)>4096,
                                   captured_bytes_sha256=hashlib.sha256(payload).hexdigest()))
    verdict = dict(schema_version="AutoWheelHistoricalDataProbeV1",
                   observed_at=dt.datetime.now(dt.timezone.utc).isoformat(),
                   window=dict(start="2026-04-06", end="2026-10-02"),
                   provider="Existing ThetaData v3 loopback terminal", probes=probes,
                   historical_performance_available=False,
                   note="Entitlement samples only. Complete 15-symbol listing/quote/Greek/OI/raw-stock/action coverage is separately required.")
    destination = Path(args.output)
    with destination.open("x", encoding="utf-8") as f:
        json.dump(verdict, f, indent=2, allow_nan=False)
        f.write("\n")
    print(f"Saved {len(probes)} bounded probes to {destination}; statuses: {[p['status'] for p in probes]}")


if __name__ == "__main__":
    main()
