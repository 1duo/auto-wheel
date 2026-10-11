#!/usr/bin/env python3
"""Audit current public originals as positive facts and a filing index only.

Never infer ex-dates, first-public availability, dividend absence, or a model
schedule. Decimal per-share amounts remain exact, including half-cent amounts.
"""
import argparse
from datetime import datetime, timezone
from decimal import Decimal
import hashlib
from html.parser import HTMLParser
import json
import os
from pathlib import Path
import re
import stat

BASE = Path("research/simulation/2026-10-05/public-inputs/dividend-declarations")
CIKS = dict(NFLX=1065280, TSLA=1318605, NVDA=1045810, NBIS=1513845,
            IREN=1878848, NOW=1373715, AMD=2488, AMZN=1018724, GOOGL=1652044,
            META=1326801, MSFT=789019, PLTR=1321655, UBER=1543151,
            AVGO=1730168, ORCL=1341439)
# name, amount, record, pay, explicit ex-date, original hash, UTC publisher time.
DECLARATIONS = (
    ("msft-mar10", "0.91", "May 21, 2026", "June 11, 2026", "May 21, 2026",
     "ffa3b355090c8fd6555a8e5cfe72e8cd9b6584123186f8e28e629117f8186747", "2026-03-10T21:32:24+00:00"),
    ("msft-jun10", "0.91", "Aug. 20, 2026", "Sept. 10, 2026", "Aug. 20, 2026",
     "74552cbecaa4039576f7a97b81313a9f99aa33e6e4fcb1559ebaf3c2e7a490a8", "2026-06-10T20:07:58+00:00"),
    ("msft-sep15", "0.98", "Nov. 19, 2026", "Dec. 10, 2026", "Nov. 19, 2026",
     "7ed92147146c016ee95be519bd5937f7c02216f3b075d0ce55dfa76843be1936", "2026-09-15T22:06:42+00:00"),
    ("meta-feb12", "0.525", "March 16, 2026", "March 26, 2026", None,
     "99bb477eda9a1cd23840f6608ae89485fcde099ca22654b290492d47d4249c4c", "2026-02-12T21:05:00+00:00"),
    ("meta-may28", "0.525", "June 15, 2026", "June 25, 2026", None,
     "cee0caf79acd44a2cd085294a5217d88730844fa0b89680a92a70c37eb419b9f", "2026-05-28T21:56:00+00:00"),
    ("meta-sep10", "0.525", "September 21, 2026", "September 28, 2026", None,
     "4f99ecf85b285a03262ebdaafd2f63cd38decf062f7abd90d425102d91707f93", "2026-09-10T23:01:00+00:00"),
    ("avgo-sep02", "0.65", "September 21, 2026", "September 30, 2026", None,
     "0aab7c6382ea0bc279afcaaf11208f0f9579b208735177bb7d90b42f791f5fcf", "2026-09-02T20:15:00+00:00"),
)


class Paragraphs(HTMLParser):
    def __init__(self):
        super().__init__()
        self.in_paragraph = False
        self.text, self.paragraphs, self.timestamps = [], [], []

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if tag == "p":
            self.in_paragraph, self.text = True, []
        if tag == "meta" and attrs.get("property") == "article:published_time":
            self.timestamps.append(attrs["content"])

    def handle_endtag(self, tag):
        if tag == "p" and self.in_paragraph:
            self.paragraphs.append(" ".join(" ".join(self.text).split()))
            self.in_paragraph = False

    def handle_data(self, data):
        if self.in_paragraph:
            self.text.append(data)


def read_bound(root, receipt):
    relative = Path(receipt["path"])
    if relative.is_absolute() or ".." in relative.parts or receipt["http_status"] != 200:
        raise ValueError("successful in-repository source required")
    fd = os.open(root / relative, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as stream:
        if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
            raise ValueError("regular original required")
        raw = stream.read((4 << 20) + 1)
    if len(raw) != receipt["bytes"] or len(raw) > 4 << 20 or hashlib.sha256(raw).hexdigest() != receipt["sha256"]:
        raise ValueError("captured original binding differs")
    return raw


def iso_date(text):
    words = text.replace(".", "").replace("Sept ", "September ").replace("Aug ", "August ").replace("Nov ", "November ").replace("Dec ", "December ")
    return datetime.strptime(words, "%B %d, %Y").date().isoformat()


def audit(root):
    receipts = []
    for folder in ("capture-20261007", "newswire-capture-20261007"):
        receipts.extend(json.loads((root / BASE / folder / "receipts.json").read_bytes())["receipts"])
    facts = []
    for name, amount, record, pay, ex_date, expected_sha, published in DECLARATIONS:
        folder = "capture-20261007" if name.startswith("msft") else "newswire-capture-20261007"
        candidates = [r for r in receipts if r["name"] == name and r.get("http_status") == 200
                      and r.get("path") == str(BASE / folder / (name + ".html"))]
        if len(candidates) != 1 or candidates[0]["sha256"] != expected_sha:
            raise ValueError("unique pinned successful declaration required")
        receipt = candidates[0]
        raw = read_bound(root, receipt)
        text = raw.decode("utf-8", "strict")
        parser = Paragraphs()
        parser.feed(text)
        paragraphs = [p for p in parser.paragraphs if ("quarterly dividend of $" + amount in p
                        or "quarterly cash dividend of $" + amount in p) and record in p and pay in p]
        if len(paragraphs) != 1:
            raise ValueError("unique declaration paragraph required")
        paragraph = paragraphs[0]
        record_prefix = r"(?:shareholders|stockholders) of record(?: as of the close of business| at the close of business \(5:00 p\.m\. Eastern Time\))? on "
        if (not re.search(record_prefix + re.escape(record) + r"\.", paragraph)
                or not re.search(r"payable(?: on)? " + re.escape(pay) + r"(?:,| to)", paragraph)):
            raise ValueError("record/payment date roles differ")
        if ex_date is not None and "The ex-dividend date will be " + ex_date + "." not in paragraph:
            raise ValueError("explicit ex-date evidence required")
        timestamps = parser.timestamps if name.startswith("msft") else re.findall(r'"datePublished"\s*:\s*"([^"]+)"', text)
        if len(timestamps) != 1:
            raise ValueError("unique publisher timestamp required")
        stamp = datetime.fromisoformat(timestamps[0])
        if stamp.tzinfo is None or stamp.astimezone(timezone.utc).isoformat() != published:
            raise ValueError("publisher timestamp differs or lacks timezone")
        if name.startswith("msft"):
            epochs = re.findall(r'<time\b[^>]*\bdatetime="(\d+)"', text)
            if len(epochs) != 1 or int(epochs[0]) != int(stamp.timestamp()):
                raise ValueError("publisher epoch and explicit timezone disagree")
        numerator, denominator = Decimal(amount).as_integer_ratio()
        facts.append({"id": name, "symbol": name.split("-")[0].upper(),
                      "declared_share_cash_usd_exact": amount,
                      "cash_usd_rational": {"numerator": numerator, "denominator": denominator},
                      "record_date": iso_date(record), "payment_date": iso_date(pay),
                      "ex_date": iso_date(ex_date) if ex_date else None,
                      "publisher_asserted_time_utc": published, "known_at_utc": None,
                      "source_url": receipt["requested_url"], "source_path": receipt["path"],
                      "source_sha256": expected_sha, "captured_at": receipt["observed_at"],
                      "declaration_paragraph_sha256": hashlib.sha256(paragraph.encode()).hexdigest(),
                      "historical_capture": False, "source_qualified": False})
    directory = root / BASE / "sec-enumeration-20261007"
    filings = []
    enumeration = json.loads((directory / "receipts.json").read_bytes())["receipts"]
    if len(enumeration) != len(CIKS) or {r["symbol"] for r in enumeration} != set(CIKS):
        raise ValueError("complete unique fifteen-entity index required")
    for receipt in enumeration:
        symbol = receipt["symbol"]
        value = json.loads(read_bound(root, receipt))
        if int(value["cik"]) != CIKS[symbol] or symbol not in value["tickers"]:
            raise ValueError("SEC entity binding differs")
        recent = value["filings"]["recent"]
        count = len(recent["accessionNumber"])
        if any(len(v) != count for v in recent.values()) or len(set(recent["accessionNumber"])) != count:
            raise ValueError("SEC parallel index or accession identity differs")
        older = [f for f in value["filings"]["files"] if f["filingFrom"] <= "2026-10-06" and f["filingTo"] >= "2025-12-01"]
        if older:
            raise ValueError("uncaptured older SEC page overlaps enumeration period")
        selected = []
        for index in range(count):
            form = recent["form"][index]
            if "2025-12-01" <= recent["filingDate"][index] <= "2026-10-06" and form.removesuffix("/A") in ("8-K", "10-Q", "10-K", "20-F", "6-K", "40-F"):
                selected.append({k: recent[k][index] for k in ("accessionNumber", "form", "filingDate", "acceptanceDateTime", "primaryDocument", "items")})
        filings.append({"symbol": symbol, "cik": CIKS[symbol], "source_sha256": receipt["sha256"],
                        "source_path": receipt["path"], "source_url": receipt["url"],
                        "recent_index_rows": count, "overlapping_uncaptured_older_pages": 0,
                        "selected_report_count": len(selected), "reports": selected,
                        "filing_contents_captured": False, "dividend_enumeration_complete": False})
    return {"schema": "AutoWheelPublicDividendInputsAuditV1",
            "script_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
            "positive_declarations": facts, "declaration_count": len(facts),
            "explicit_ex_date_count": sum(f["ex_date"] is not None for f in facts),
            "SEC_entity_count": len(filings), "SEC_report_count": sum(f["selected_report_count"] for f in filings),
            "SEC_index_period": {"start": "2025-12-01", "end": "2026-10-06"},
            "SEC_indices": filings, "pricing_schedule_emitted": False,
            "historical_availability_qualified": False, "no_dividend_inference": False,
            "complete_dividend_enumeration": False, "corporate_action_gate_passed": False,
            "scope": "Seven positive declarations and fifteen current SEC filing indices only. Publisher assertions and SEC acceptance metadata are preserved without assigning first-public availability. Four ex-dates remain unknown; record dates are not substituted. Complete historical dividend/corporate-action and common-equity class/model qualification remain pending."}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path("."))
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    result = audit(args.root)
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "w") as stream:
        json.dump(result, stream, indent=2, allow_nan=False)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    print(json.dumps({k: result[k] for k in ("schema", "declaration_count", "explicit_ex_date_count", "SEC_entity_count", "SEC_report_count", "historical_availability_qualified", "corporate_action_gate_passed")}))


if __name__ == "__main__":
    main()
