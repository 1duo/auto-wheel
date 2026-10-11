#!/usr/bin/env python3
"""Audit five pinned public declarations; never emit a pricing dividend schedule.

These current issuer pages provide positive cash-declaration facts. Their
record dates are not treated as ex-dates, and their displayed calendar dates
are not turned into historical known-at timestamps. No network or credentials.
"""

import argparse
import hashlib
import json
import os
import stat
from html.parser import HTMLParser
from pathlib import Path


SOURCE_DIRECTORY = Path("research/simulation/2026-10-05/public-inputs/dividend-declarations")
MAX_SOURCE_BYTES = 4 * 1024 * 1024
# Amounts are exact cents per common share, separate from numerical-model USD.
SOURCES = (
    ("nvda-q1-fy2027", "NVDA", "9576b92d195262ca072b3edd8c1979e0816b0f243744cb1493ab9b4b6561936e",
     "https://nvidianews.nvidia.com/news/nvidia-announces-financial-results-for-first-quarter-fiscal-2027",
     "May 20, 2026", "2026-05-20", 25, "2026-06-04", "2026-06-26",
     "NVIDIA is increasing its quarterly cash dividend from $0.01 per share to $0.25 per share of common stock, which will be paid on June 26, 2026, to all shareholders of record on June 4, 2026."),
    ("nvda-q2-fy2027", "NVDA", "3c6ec3d35d1cd950dbdbdd937b778c9a9e091e93fed695b713e84f656c8c14bf",
     "https://nvidianews.nvidia.com/news/nvidia-announces-financial-results-for-second-quarter-fiscal-2027",
     "August 26, 2026", "2026-08-26", 25, "2026-09-10", "2026-10-01",
     "NVIDIA will pay its next quarterly cash dividend of $0.25 per share on October 1, 2026, to all shareholders of record on September 10, 2026."),
    ("orcl-q3-fy2026", "ORCL", "4003db13bed6914a2c844bf84dc1110f7ea2a8c08dd7bf5e62332fe591f74426",
     "https://www.oracle.com/news/announcement/q3fy26-earnings-release-2026-03-10/",
     "Austin, Texas—Mar 10, 2026", "2026-03-10", 50, "2026-04-09", "2026-04-24",
     "The board of directors declared a quarterly cash dividend of $0.50 per share of outstanding common stock. This dividend will be paid to stockholders of record as of the close of business on April 9, 2026, with a payment date of April 24, 2026."),
    ("orcl-q4-fy2026", "ORCL", "26abdfc7810300e985d294174f64d3a46cfa1a6fdb8d6e5624b064abb3f297a6",
     "https://www.oracle.com/news/announcement/q4fy26-earnings-release-2026-06-10/",
     "Austin, Texas—Jun 10, 2026", "2026-06-10", 50, "2026-07-10", "2026-07-24",
     "The board of directors declared a quarterly cash dividend of $0.50 per share of outstanding common stock. This dividend will be paid to stockholders of record as of the close of business on July 10, 2026, with a payment date of July 24, 2026."),
    ("orcl-q1-fy2027", "ORCL", "4db6fa11e9dc8ad4e1a2e132467f8f71fce8d62dfe57636c4198aad37333979a",
     "https://www.oracle.com/news/announcement/q1fy27-earnings-release-2026-09-10/",
     "Austin, Texas—Sep 10, 2026", "2026-09-10", 50, "2026-10-09", "2026-10-23",
     "The board of directors declared a quarterly cash dividend of $0.50 per share of outstanding common stock. This dividend will be paid to stockholders of record as of the close of business on October 9, 2026, with a payment date of October 23, 2026."),
)


class VisibleText(HTMLParser):
    def __init__(self):
        super().__init__()
        self.parts = []
        self.skip_depth = 0

    def handle_starttag(self, tag, attrs):
        if tag in ("script", "style"):
            self.skip_depth += 1

    def handle_endtag(self, tag):
        if tag in ("script", "style"):
            self.skip_depth = max(0, self.skip_depth - 1)

    def handle_data(self, data):
        if not self.skip_depth:
            self.parts.append(data)


def parse_source(raw, expected_sha, date_anchor, declaration_anchor):
    if len(raw) > MAX_SOURCE_BYTES or hashlib.sha256(raw).hexdigest() != expected_sha:
        raise ValueError("source differs from the pinned issuer page")
    parser = VisibleText()
    parser.feed(raw.decode("utf-8", errors="strict"))
    text = " ".join(" ".join(parser.parts).split())
    if text.count(date_anchor) != 1 or text.count(declaration_anchor) != 1:
        raise ValueError("issuer declaration/date evidence missing or ambiguous")


def read_source(path):
    # NOFOLLOW and regular-file validation reject substituted symlinks/FIFOs.
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as source:
        if not stat.S_ISREG(os.fstat(source.fileno()).st_mode):
            raise ValueError("source must be a regular file")
        return source.read(MAX_SOURCE_BYTES + 1)


def audit(root):
    facts = []
    controls = []
    for name, symbol, digest, url, date_anchor, date, cents, record, pay, declaration in SOURCES:
        raw = read_source(root / SOURCE_DIRECTORY / (name + ".html"))
        parse_source(raw, digest, date_anchor, declaration)
        facts.append({
            "id": name, "symbol": symbol, "common_share_cash_cents": cents,
            "displayed_announcement_date": date,
            "record_date": record, "payment_date": pay,
            "ex_date": None, "known_at_utc": None,
            "source_path": str(SOURCE_DIRECTORY / (name + ".html")),
            "source_url": url, "source_sha256": digest,
            "historical_capture": False, "source_qualified": False,
        })
        # Real-source mutation is rejected before any evidence extraction.
        try:
            parse_source(raw + b"\n", digest, date_anchor, declaration)
        except ValueError:
            controls.append({"id": name, "control": "source_mutation", "rejected": True})
        else:
            raise ValueError("source mutation unexpectedly accepted")
        # Even a caller-supplied correct hash cannot manufacture an absent date.
        try:
            parse_source(raw, digest, "2099-01-01", declaration)
        except ValueError:
            controls.append({"id": name, "control": "missing_date_evidence", "rejected": True})
        else:
            raise ValueError("missing date unexpectedly accepted")
    return {
        "schema_version": "AutoWheelPublicDividendPositiveEvidenceV1",
        "script_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
        "declared_symbols": ["NVDA", "ORCL"], "declaration_count": len(facts),
        "facts": facts, "controls": controls,
        "historical_greeks_qualified": False, "source_qualified": False,
        "complete_declaration_audit": False,
        "pricing_schedule_emitted": False, "no_dividend_inference": False,
        "scope": "Five positive common-share declarations only. Current page captures and displayed dates do not establish historical availability. Record dates are not ex-dates. No complete symbol/period, absence, corporate-action, model or historical-performance qualification.",
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path("."))
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    result = audit(args.root)
    raw = (json.dumps(result, indent=2, allow_nan=False) + "\n").encode()
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as output:
        output.write(raw)
        output.flush()
        os.fsync(output.fileno())
    print(f"{len(result['facts'])} positive declarations; {len(result['controls'])} rejection controls; historical qualification pending")


if __name__ == "__main__":
    main()
