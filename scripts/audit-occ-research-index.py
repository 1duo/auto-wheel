#!/usr/bin/env python3
"""Verify retained public OCC index/original integrity without qualifying terms.

The 16 individual keyword exports are observations, not an exhaustive search.
Current content, title matches and retained PDFs cannot prove absence of an
adjustment or assign standard deliverables to every historical option series.
"""
import argparse
import csv
from datetime import datetime, timezone
import hashlib
import io
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
DIRECTORY = Path("research/simulation/2026-10-05/cycle23-public-primary")
KEYWORDS = {"NFLX", "NVDA", "TSLA", "ServiceNow", "AVGO", "AMZN", "NBIS", "IREN",
            "AMD", "GOOGL", "META", "MSFT", "PLTR", "UBER", "ORCL", "NOW"}
ORIGINALS = {54623, 54716, 57556, 57651, 57868, 58711, 58730, 58914, 59074, 59381, 59598}


def digest(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def scoped(relative):
    path = Path(relative)
    if path.is_absolute() or path.parent != DIRECTORY or path.name in ("", ".", ".."):
        raise ValueError("OCC evidence scope differs")
    full = ROOT/path
    if full.resolve() != full or not full.is_file():
        raise ValueError("OCC original must be a regular canonical file")
    return full


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    index_path = ROOT/DIRECTORY/"occ-browser-exports.json"
    originals_path = ROOT/DIRECTORY/"occ-original-receipts.json"
    terms_path = ROOT/"research/simulation/2026-10-05/cycle23-public-term-observations.json"
    before = {path: digest(path) for path in (Path(__file__).resolve(), index_path, originals_path, terms_path)}
    index, originals, terms = (json.loads(path.read_bytes()) for path in (index_path, originals_path, terms_path))
    if (index["schema_version"] != "AutoWheelOCCBrowserSearchExportV1" or index["status"] != "retained_unqualified"
            or index["source_gate_passed"] is not False or originals["full_source_qualification"] is not False
            or originals["schema_version"] != "AutoWheelOCCOriginalReceiptsV1" or terms["source_gate_passed"] is not False):
        raise ValueError("unqualified public evidence required")
    queries, rows, total, seen = index["individual_queries"], {}, 0, set()
    if len(queries) != 16:
        raise ValueError("individual OCC query count differs")
    for query in queries:
        keyword = query["keyword"]
        if keyword not in KEYWORDS or keyword in seen:
            raise ValueError("OCC keyword universe differs")
        seen.add(keyword)
        if (query["posted_start"] != "2024-01-01" or query["posted_end"] != "2026-10-06"
                or query["ui_filter_and_result_count_verified"] is not True
                or type(query["ui_result_count"]) is not int or type(query["export_rows"]) is not int
                or query["ui_result_count"] != query["export_rows"]):
            raise ValueError("OCC observed query scope differs")
        path = scoped(query["export_path"])
        data = path.read_bytes()
        before[path] = hashlib.sha256(data).hexdigest()
        if before[path] != query["export_sha256"] or len(data) != query["export_bytes"]:
            raise ValueError("OCC export original differs")
        reader = csv.reader(io.StringIO(data.decode("utf-8-sig"), newline=""), strict=True)
        if [value.strip() for value in next(reader)] != ["number", "post date", "ex/eff date", "title"]:
            raise ValueError("OCC CSV header differs")
        count, query_numbers = 0, set()
        for row in reader:
            if len(row) != 4:
                raise ValueError("OCC CSV row width differs")
            number, posted, effective, title = (value.strip() for value in row)
            if not number.isascii() or not number.isdigit() or not number or int(number) <= 0 or number in query_numbers or not title:
                raise ValueError("OCC CSV identity differs")
            query_numbers.add(number)
            posted_day = datetime.strptime(posted, "%b-%d-%Y").date()
            if not "2024-01-01" <= posted_day.isoformat() <= "2026-10-06":
                raise ValueError("OCC export posted date outside observed scope")
            if effective:
                datetime.strptime(effective, "%b-%d-%Y")
            normalized = (posted, effective, title)
            if number in rows and rows[number] != normalized:
                raise ValueError("overlapping OCC exports disagree")
            rows[number] = normalized
            count += 1
        if count != query["export_rows"]:
            raise ValueError("OCC exported row count differs")
        total += count
        if keyword == "NFLX" and not {"57556", "57651"} <= query_numbers:
            raise ValueError("OCC known split positive control missing")
    if seen != KEYWORDS or len(rows) != 707 or index["unique_memo_rows"] != len(rows):
        raise ValueError("OCC index union differs")
    seen_originals = set()
    for original in originals["originals"]:
        number = original["number"]
        if type(number) is not int or number not in ORIGINALS or number in seen_originals or str(number) not in rows:
            raise ValueError("OCC original index binding differs")
        seen_originals.add(number)
        if original["memo_number_confirmed_in_original"] is not True or original["source_gate_passed"] is not False:
            raise ValueError("OCC original recorded disposition differs")
        path = scoped(original["path"])
        if path.name != "occ-"+str(number)+".pdf" or original["url"] != "https://infomemo.theocc.com/infomemos?number="+str(number):
            raise ValueError("OCC original identity route differs")
        data = path.read_bytes()
        before[path] = hashlib.sha256(data).hexdigest()
        if before[path] != original["sha256"] or len(data) != original["bytes"] or not data.startswith(b"%PDF-"):
            raise ValueError("retained OCC PDF bytes differ")
    if seen_originals != ORIGINALS or len(terms["original_OCC_splits"]) != 5:
        raise ValueError("retained OCC original count differs")
    # This binds previously reviewed candidate facts, without re-parsing PDF
    # tables or claiming that keyword coverage proves all-series terms.
    for candidate in terms["original_OCC_splits"]:
        if candidate["number"] not in seen_originals or candidate["premium_dollar_multiplier"] != 100 or candidate["shares_per_new_contract"] != 100:
            raise ValueError("candidate memo-to-original link differs")
    if any(digest(path) != value for path, value in before.items()):
        raise ValueError("OCC evidence changed during audit")
    report = dict(schema="AutoWheelRetainedOCCIndexIntegrityV1", observed_at=datetime.now(timezone.utc).isoformat(),
                  individual_keyword_exports_verified=len(queries), exported_rows_verified=total,
                  unique_memo_index_rows=len(rows), retained_pdf_originals_verified=len(seen_originals),
                  previously_reviewed_positive_split_candidates=5, index_integrity_verified=True,
                  pdf_tables_reparsed_this_pass=False, exhaustive_corporate_action_search=False,
                  negative_result_proof=False, per_series_terms_qualified=False,
                  point_in_time_roster_qualified=False, source_gate_passed=False,
                  historical_performance_available=False, provider_requests=0, financial_requests=0,
                  source_files=[dict(path=str(path.relative_to(ROOT)), sha256=value) for path, value in sorted(before.items())],
                  limitations=["Individual keyword exports retain 2024-2026 index observations, not a complete historical contract census.",
                               "NOW and ETF/title matches must not be blindly attributed to the frozen common-stock roots.",
                               "Candidate split terms apply only to the reviewed adjustments; other series, revisions, tradability, ticks and dates still require evidence."])
    with args.output.open("x") as output:
        json.dump(report, output, indent=2, allow_nan=False)
        output.write("\n")
    print(json.dumps({key: value for key, value in report.items() if key != "source_files"}))


if __name__ == "__main__":
    main()
