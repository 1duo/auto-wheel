#!/usr/bin/env python3
"""Verify public OCC daily ESS structure without inferring standard terms."""
import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import re

ROOT = Path(__file__).resolve().parents[1]
DIRECTORY = Path("research/simulation/2026-10-05/cycle39-public-primary")
SYMBOLS = {"NFLX", "TSLA", "NVDA", "NBIS", "IREN", "NOW", "AMD", "AMZN", "GOOGL", "META", "MSFT", "PLTR", "UBER", "AVGO", "ORCL"}


def parse(data, day):
    lines = data.decode("ascii").splitlines()
    if not lines or any(len(line) != 136 for line in lines):
        raise ValueError("ESS line width differs")
    records, pages, groups, total = [], [], {}, None
    date = datetime.strptime(day, "%Y-%m-%d").strftime("%m/%d/%y")
    columns = ["REC  PROD    PKND  SRTK  ONN  CMPN   CMPN  SECU                    UNIT       SETL   STRK     FIXED      PROCESS   SETTLE",
               "ID   ID      ID    CURN  GRP  TOTAL  NBR   ID                      QTY        METH   PCT      AMT        DATE      DATE"]
    i = 0
    while i < len(lines):
        line = lines[i].rstrip()
        if total is not None:
            raise ValueError("ESS records after final total")
        if line.startswith("1THE OPTIONS CLEARING CORPORATION"):
            match = re.fullmatch(r"1THE OPTIONS CLEARING CORPORATION - CHICAGO, ILLINOIS\s+TIME (\d{2}:\d{2}:\d{2})\s+DATE (\d{2}/\d{2}/\d{2})\s+PAGE\s+(\d+)", line)
            if not match or match[2] != date or int(match[3]) != len(pages) + 1 or i + 3 >= len(lines):
                raise ValueError("ESS page/date header differs")
            datetime.strptime(match[1], "%H:%M:%S")
            activity = re.fullmatch(r" NON-STANDARD SETTLEMENTS MRD REPORT\s+ACTIVITY DATE (\d{2}/\d{2}/\d{2})\s+PROGRAM-ID DLVC1910AS\s+V\.008", lines[i + 1].rstrip())
            if not activity or activity[1] != date or [x.strip() for x in lines[i + 2:i + 4]] != columns:
                raise ValueError("ESS activity/column headers differ")
            pages.append(match[1])
            i += 4
            continue
        if line.startswith("0706"):
            fields = line.split()
            if len(fields) != 14 or fields[0] != "0706" or not pages:
                raise ValueError("ESS record width/type differs")
            rid, product, kind, currency, code, count, number, security, qty, method, pct, amount, process, settlement = fields
            if (kind != "OSTK" or code not in {"EB", "EF", "EL", "EU", "NC"} or method not in {"MON", "CNS", "BTB"}
                    or not re.fullmatch(r"[A-Z0-9]+", product) or not re.fullmatch(r"[A-Z0-9]+", security)
                    or not re.fullmatch(r"[A-Z]{3}", currency)
                    or not all(re.fullmatch(r"\d+", x) for x in (count, number, qty, pct))
                    or not re.fullmatch(r"\d+\.\d{6}", amount) or process != day.replace("-", "")):
                raise ValueError("ESS record field differs")
            if settlement != "00000000":
                datetime.strptime(settlement, "%Y%m%d")
            key = (product, kind, currency, code)
            group = groups.setdefault(key, {})
            if int(number) in group or not 1 <= int(number) <= int(count) <= 99:
                raise ValueError("ESS component identity differs")
            group[int(number)] = fields
            records.append(fields)
        else:
            match = re.fullmatch(r"-\s+REPORT DETAIL TOTAL\s+(\d+)", line)
            if not match:
                raise ValueError("unrecognized ESS line")
            total = int(match[1])
        i += 1
    if total != len(records) or not records or len(set(pages)) != 1:
        raise ValueError("ESS total/page time differs")
    incomplete = []
    for key, group in groups.items():
        counts = {int(x[5]) for x in group.values()}
        if len(counts) != 1:
            raise ValueError("ESS component totals disagree")
        if set(group) != set(range(1, next(iter(counts)) + 1)):
            incomplete.append(dict(product_id=key[0], components_stated=next(iter(counts)),
                                   components_present=sorted(group),
                                   study_identity_reference=key[0] in SYMBOLS or any(x[7] in SYMBOLS for x in group.values())))
    return dict(records=len(records), product_groups=len(groups), pages=len(pages),
                report_time=pages[0], exact_study_product_records=sum(x[1] in SYMBOLS for x in records),
                study_security_component_records=sum(x[7] in SYMBOLS for x in records),
                incomplete_component_groups=incomplete)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    source = ROOT / DIRECTORY / "occ-daily-ess-acquisition.json"
    raw = source.read_bytes()
    acquisition = json.loads(raw)
    days = acquisition["session_dates"] + acquisition["supplemental_dates"]
    if len(days) != 128 or days != sorted(set(days)) or len(acquisition["receipts"]) != len(days) or acquisition["source_gate_passed"] is not False:
        raise ValueError("ESS acquisition scope differs")
    files = {str(source.relative_to(ROOT)): hashlib.sha256(raw).hexdigest(),
             str(Path(__file__).relative_to(ROOT)): hashlib.sha256(Path(__file__).read_bytes()).hexdigest()}
    audits = []
    for day, receipt in zip(days, acquisition["receipts"]):
        path = Path(receipt["path"])
        names = {"occ-ess-" + day.replace("-", "") + suffix for suffix in (".response", "-retry1.response")}
        full = ROOT / path
        if path.parent != DIRECTORY or path.name not in names or full.resolve() != full or receipt["status"] != 200 or receipt["url"] != "https://marketdata.theocc.com/ess-reports?reportDate=" + day.replace("-", ""):
            raise ValueError("ESS original identity differs")
        data = full.read_bytes()
        sha = hashlib.sha256(data).hexdigest()
        if len(data) != receipt["bytes"] or sha != receipt["sha256"]:
            raise ValueError("ESS original hash/size differs")
        files[str(path)] = sha
        audits.append(dict(date=day, **parse(data, day)))
    if any(hashlib.sha256((ROOT / path).read_bytes()).hexdigest() != sha for path, sha in files.items()):
        raise ValueError("ESS source changed during audit")
    report = dict(schema="AutoWheelOCCDailyESSAuditV1", observed_at=datetime.now(timezone.utc).isoformat(),
                  reports_verified=128, frozen_session_reports_verified=126, supplemental_reports_verified=2,
                  records_verified=sum(x["records"] for x in audits), daily_audits=audits,
                  report_dates_headers_and_totals_verified=True,
                  all_delivery_component_groups_complete=not any(x["incomplete_component_groups"] for x in audits),
                  incomplete_component_group_days=sum(len(x["incomplete_component_groups"]) for x in audits),
                  incomplete_study_identity_group_days=sum(z["study_identity_reference"] for x in audits for z in x["incomplete_component_groups"]),
                  original_report_identity_component_verified=True, known_at=None,
                  historical_vintage_qualified=False, negative_result_proof=False,
                  per_series_terms_qualified=False, source_gate_passed=False,
                  historical_performance_available=False, financial_requests=0,
                  source_files=[dict(path=path, sha256=sha) for path, sha in sorted(files.items())],
                  limitations=["Report dates, process dates and printed times are assertions in a retrospective download, not proof of first public availability.",
                               "OCC FAQ describes prior-day eligibility and an effective-date exclusion; reconcile dated adjustment memos before using absence.",
                               "Match exact product IDs and definition identities; delivery-security or title matches alone do not establish terms for the study series.",
                               "Missing reported delivery components remain unresolved; structure and total checks do not prove every settlement component is present."])
    with args.output.open("x") as output:
        json.dump(report, output, indent=2, allow_nan=False)
        output.write("\n")
    print(json.dumps({key: value for key, value in report.items() if key not in {"daily_audits", "source_files"}}))


if __name__ == "__main__":
    main()
