#!/usr/bin/env python3
"""Offline reconciliation of all126 public reports; missing roots stay unknown."""
import argparse
from collections import Counter
import csv
from datetime import datetime, timezone
from email.utils import parsedate_to_datetime
import hashlib
import io
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
CALENDAR = ROOT / "research/simulation/2026-10-05/cycle40-contract-term-join-summary.json"
CALENDAR_SHA = "883d6b46fdd59d7858bad4f5d0aa319937c5ae52408994ce7c4b82e12881d8ec"
URL_BASE = "https://cdn.cboe.com/resources/us/options/market-statistics/penny-tick-type/cone/cone_options_rpt_penny_tick_type_"
SYMBOLS = ("NFLX", "TSLA", "NVDA", "NBIS", "IREN", "NOW", "AMD", "AMZN", "GOOGL", "META", "MSFT", "PLTR", "UBER", "AVGO", "ORCL")
TYPES = {"Pennies to 3.00", "Pennies all"}


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def parse(data):
    if len(data) > 2 * 1024 * 1024:
        raise ValueError("report exceeds bounded size")
    reader = csv.reader(io.StringIO(data.decode("utf-8-sig")), strict=True)
    if next(reader, None) != ["OSI Root", "Underlying Description", "Tick Type"]:
        raise ValueError("report columns differ or response is not CSV")
    rows = {}
    for row in reader:
        if (len(row) != 3 or not row[0] or row[0] != row[0].strip() or row[0] in rows
                or not row[2] or row[2] != row[2].strip() or len(row[2]) > 128
                or (row[0] in SYMBOLS and row[2] not in TYPES)):
            raise ValueError("malformed, duplicate or unknown tick classification")
        rows[row[0]] = row[2]
    if not rows:
        raise ValueError("empty public report")
    return rows


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    directory = args.directory.resolve()
    original = directory / "acquisition.json"
    report = json.loads(original.read_bytes())
    if sha(CALENDAR) != CALENDAR_SHA:
        raise ValueError("frozen full-study calendar differs")
    days = [x["date"] for x in json.loads(CALENDAR.read_bytes())["daily_audits"]]
    if (len(days) != 126 or days != sorted(set(days)) or days[0] != "2026-04-06" or days[-1] != "2026-10-02"
            or report["session_dates"] != days or report["calendar_sha256"] != CALENDAR_SHA
            or len(report["receipts"]) != 126):
        raise ValueError("report calendar shrunk, duplicated or expanded")
    sources = {str(CALENDAR): CALENDAR_SHA, str(original): sha(original),
               str(Path(__file__).resolve()): sha(Path(__file__)),
               str(ROOT / "scripts/acquire-cboe-penny-reports.py"): sha(ROOT / "scripts/acquire-cboe-penny-reports.py")}
    acquisition_code = ROOT / "scripts/acquire-cboe-penny-reports.py"
    if report["script_sha256"] != sources[str(acquisition_code)]:
        # Preserve the exact original capture code when a later guard fix changes
        # the current tool. The saved original must match the sealed receipt hash.
        original_code = directory / "acquisition-code-original.py"
        if original_code.is_symlink() or sha(original_code) != report["script_sha256"]:
            raise ValueError("original acquisition code binding differs")
        sources[str(original_code)] = report["script_sha256"]
    daily, totals, per_symbol = [], Counter(), {s: Counter() for s in SYMBOLS}
    public_types = Counter()
    seen_paths = set()
    for day, receipt in zip(days, report["receipts"]):
        path = directory / ("cone-penny-" + day + ".csv")
        receipt_path = path.with_name(path.name + ".receipt.json")
        url = URL_BASE + day.replace("-", "") + ".csv"
        if (path.is_symlink() or receipt_path.is_symlink() or str(path) in seen_paths
                or receipt["date"] != day or receipt["url"] != url or receipt["final_url"] != url
                or receipt["path"] != str(path) or receipt["status"] != 200
                or receipt != json.loads(receipt_path.read_bytes()) or path.stat().st_size != receipt["bytes"]
                or sha(path) != receipt["sha256"] or receipt["historical_first_availability"] is not None
                or receipt["source_gate_passed"] is not False):
            raise ValueError("public original/receipt binding differs")
        seen_paths.add(str(path))
        data = path.read_bytes()
        rows = parse(data)
        public_types.update(rows.values())
        totals["nonstudy_uninterpreted_tick_type_rows"] += sum(s not in SYMBOLS and t not in TYPES for s, t in rows.items())
        classifications = {symbol: rows.get(symbol) for symbol in SYMBOLS}
        missing = [symbol for symbol, value in classifications.items() if value is None]
        first_cut = datetime.fromisoformat(day + "T13:35:00+00:00")
        modified = parsedate_to_datetime(receipt["http_last_modified"]) if receipt["http_last_modified"] else None
        if modified is not None and modified.tzinfo is None:
            raise ValueError("ambiguous HTTP modification clock")
        modified_after_cut = modified > first_cut if modified else None
        totals["public_rows"] += len(rows)
        totals["positive_symbol_days"] += len(SYMBOLS) - len(missing)
        totals["missing_symbol_days"] += len(missing)
        totals["HTTP_modified_after_first_cut"] += modified_after_cut is True
        totals["HTTP_modified_missing"] += modified is None
        for symbol, value in classifications.items():
            per_symbol[symbol][value or "unknown_not_in_penny_report"] += 1
        daily.append(dict(date=day, public_rows=len(rows), root_tick_classifications=classifications,
                          missing_roots=missing, HTTP_modified_after_first_cut=modified_after_cut,
                          historical_first_availability=None, causal_qualified=False))
        sources[str(path)] = receipt["sha256"]
        sources[str(receipt_path)] = sha(receipt_path)
    if totals["positive_symbol_days"] + totals["missing_symbol_days"] != 1890:
        raise ValueError("full15-symbol/date subject set differs")
    for path, value in sources.items():
        if sha(Path(path)) != value:
            raise ValueError("source changed during audit")
    result = dict(schema="AutoWheelCboePennyReportAuditV1", observed_at=datetime.now(timezone.utc).isoformat(),
                  reports_audited=126, required_symbol_days=1890, counters=dict(totals),
                  public_tick_type_counts=dict(public_types),
                  per_symbol={s: dict(c) for s, c in per_symbol.items()}, daily=daily,
                  source_files=[dict(path=str(Path(p).relative_to(ROOT)), sha256=s) for p, s in sorted(sources.items())],
                  full_requested_dates_retained=True, missing_roots_not_defaulted=True,
                  causal_tick_rules_qualified=False, per_series_terms_qualified=False,
                  source_gate_passed=False, historical_performance_available=False,
                  provider_financial_requests=0,
                  limitations=["Penny report absence does not classify a root as non-penny, non-tradable or standard.",
                               "HTTP Last-Modified is retrieval metadata, not proof of first publication or historical revision completeness.",
                               "Reports cover Cboe Options; minimum-tick rule semantics, routing applicability and known-at clocks still require corroboration.",
                               "No premium multiplier, deliverable, listing, weekly or currency field is inferred from a tick classification."])
    with args.output.open("x") as output:
        json.dump(result, output, indent=2, allow_nan=False)
        output.write("\n")
    print(json.dumps({k: v for k, v in result.items() if k not in {"daily", "source_files"}}))


if __name__ == "__main__":
    main()
