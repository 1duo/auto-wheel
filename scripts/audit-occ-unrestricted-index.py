#!/usr/bin/env python3
"""Bind OCC browser partitions to observed counts; do not qualify series terms."""
import argparse
import csv
from datetime import datetime, timezone
import hashlib
import io
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
DIRECTORY = Path("research/simulation/2026-10-05/cycle39-public-primary")
PRIOR = Path("research/simulation/2026-10-05/cycle23-public-primary")
SCOPES = [("2024-01-01", "2026-10-06", 5975, 2500),
          ("2024-01-01", "2024-12-31", 1937, 1937),
          ("2025-01-01", "2025-12-31", 2243, 2243),
          ("2026-01-01", "2026-10-06", 1795, 1795)]


def read(relative, before):
    path = Path(relative)
    if path.is_absolute() or path.parent not in (DIRECTORY, PRIOR):
        raise ValueError("unexpected OCC source scope")
    full = ROOT / path
    if full.resolve() != full or not full.is_file():
        raise ValueError("OCC source must be a canonical regular file")
    data = full.read_bytes()
    before[path.as_posix()] = hashlib.sha256(data).hexdigest()
    return data


def parse(data, start, end):
    reader = csv.reader(io.StringIO(data.decode("utf-8-sig"), newline=""), strict=True)
    if [value.strip() for value in next(reader)] != ["number", "post date", "ex/eff date", "title"]:
        raise ValueError("OCC CSV header differs")
    rows = {}
    for row in reader:
        if len(row) != 4:
            raise ValueError("OCC CSV row width differs")
        number, posted, effective, title = (value.strip() for value in row)
        if not number or not number.isascii() or not number.isdigit() or int(number) <= 0 or number in rows or not title:
            raise ValueError("OCC CSV identity differs")
        day = datetime.strptime(posted, "%b-%d-%Y").date().isoformat()
        if not start <= day <= end:
            raise ValueError("OCC posted date outside observed scope")
        if effective:
            datetime.strptime(effective, "%b-%d-%Y")
        rows[number] = (posted, effective, title)
    return rows


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    before = {str(Path(__file__).relative_to(ROOT)): hashlib.sha256(Path(__file__).read_bytes()).hexdigest()}
    receipt = json.loads(read(DIRECTORY / "occ-unrestricted-browser-exports.json", before))
    if (receipt["schema"] != "AutoWheelOCCUnrestrictedBrowserExportsV1"
            or receipt["source_gate_passed"] is not False or receipt["historical_vintage_qualified"] is not False
            or receipt["per_series_terms_qualified"] is not False
            or receipt["search_url"] != "https://infomemo.theocc.com/infomemo/search-memo"
            or receipt["export_url"] != "https://infomemo.theocc.com/infomemo/exportmemo"
            or len(receipt["queries"]) != len(SCOPES)):
        raise ValueError("unqualified browser receipt differs")
    union, broad, counts = {}, {}, []
    for i, (query, scope) in enumerate(zip(receipt["queries"], SCOPES)):
        start, end, count, exported = scope
        if (query["posted_start"], query["posted_end"], query["ui_result_count"], query["export_rows"]) != scope:
            raise ValueError("observed OCC scope/count differs")
        if (query["ui_filter_and_result_count_verified"] is not True or query["source_gate_passed"] is not False
                or query["raw_server_total_retained"] is not False or query["categories_filter"] != "unrestricted"
                or any(query[key] != "" for key in ("keyword", "title_filter", "memo_filter", "effective_filter"))):
            raise ValueError("OCC search restrictions differ")
        data = read(query["path"], before)
        if len(data) != query["bytes"] or hashlib.sha256(data).hexdigest() != query["sha256"]:
            raise ValueError("OCC original export differs")
        rows = parse(data, start, end)
        if len(rows) != exported:
            raise ValueError("OCC CSV count differs")
        if i == 0:
            broad = rows
        else:
            if union.keys() & rows.keys():
                raise ValueError("disjoint yearly OCC scopes overlap")
            union.update(rows)
            counts.append(exported)
    if len(union) != 5975 or sum(counts) != receipt["queries"][0]["ui_result_count"]:
        raise ValueError("partition union differs from broad observed count")
    if any(union.get(number) != row for number, row in broad.items()):
        raise ValueError("truncated broad export disagrees with partition union")
    prior_receipt = json.loads(read(PRIOR / "occ-browser-exports.json", before))
    prior_rows = {}
    for query in prior_receipt["individual_queries"]:
        data = read(query["export_path"], before)
        if len(data) != query["export_bytes"] or hashlib.sha256(data).hexdigest() != query["export_sha256"]:
            raise ValueError("retained keyword export differs")
        rows = parse(data, "2024-01-01", "2026-10-06")
        if len(rows) != query["export_rows"]:
            raise ValueError("retained keyword count differs")
        for number, row in rows.items():
            if union.get(number) != row or (number in prior_rows and prior_rows[number] != row):
                raise ValueError("retained keyword row absent or changed in unrestricted union")
            prior_rows[number] = row
    if len(prior_rows) != 707 or not {"54623", "54716", "57556", "57651", "57868"} <= union.keys():
        raise ValueError("retained memo/split controls differ")
    originals = json.loads(read(PRIOR / "occ-original-receipts.json", before))
    if len(originals["originals"]) != 11 or any(str(x["number"]) not in union for x in originals["originals"]):
        raise ValueError("retained original IDs absent from unrestricted union")
    if any(hashlib.sha256((ROOT / path).read_bytes()).hexdigest() != sha for path, sha in before.items()):
        raise ValueError("OCC evidence changed during audit")
    report = dict(schema="AutoWheelOCCUnrestrictedIndexAuditV1", observed_at=datetime.now(timezone.utc).isoformat(),
                  yearly_export_counts=counts, unique_memo_rows=5975, broad_export_rows=2500,
                  broad_export_truncated=True, partition_union_matches_observed_broad_count=True,
                  retained_keyword_rows_agree=707, retained_original_ids_present=11,
                  positive_split_ids_present=5, index_enumeration_component_verified=True,
                  posted_start="2024-01-01", posted_end="2026-10-06",
                  exhaustive_corporate_action_search=False, negative_result_proof=False,
                  historical_vintage_qualified=False, per_series_terms_qualified=False,
                  source_gate_passed=False, historical_performance_available=False,
                  financial_requests=0, provider_requests=0,
                  source_files=[dict(path=path, sha256=sha) for path, sha in sorted(before.items())],
                  limitations=["Count equality is tied to contemporaneously observed Safari filters/counts; raw server counts were not retained.",
                               "Index completeness within posted scope does not establish memo content, affected roots, revisions, pre-2024 carry-in terms or historical availability.",
                               "Nonstandard terms still require dated special-settlement reports and adjustment documents; standard OCC rules alone do not identify every series."])
    with args.output.open("x") as output:
        json.dump(report, output, indent=2, allow_nan=False)
        output.write("\n")
    print(json.dumps({key: value for key, value in report.items() if key != "source_files"}))


if __name__ == "__main__":
    main()
