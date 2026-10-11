#!/usr/bin/env python3
"""Enumerate exact-root title candidates; titles never prove adjustment absence."""
import argparse
import csv
from datetime import datetime, timezone
import hashlib
import io
import json
from pathlib import Path
import re

ROOT = Path(__file__).resolve().parents[1]
DIRECTORY = ROOT / "research/simulation/2026-10-05/cycle39-public-primary"
SYMBOLS = {"NFLX", "TSLA", "NVDA", "NBIS", "IREN", "NOW", "AMD", "AMZN", "GOOGL", "META", "MSFT", "PLTR", "UBER", "AVGO", "ORCL"}


def classify(title):
    mentions = sorted(s for s in SYMBOLS if re.search(r"(?<![A-Z0-9])" + s + r"(?![A-Z0-9])", title))
    sections = re.findall(r"(?:Adjusted )?Option Symbols?\s*:\s*([^*]+)", title, flags=re.I)
    tokens = set(re.findall(r"[A-Z0-9]+", " ".join(sections)))
    roots = sorted(SYMBOLS & tokens)
    return mentions, roots


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    receipt_path = DIRECTORY / "occ-unrestricted-browser-exports.json"
    audit_path = DIRECTORY / "occ-unrestricted-index-audit.json"
    receipt, audit = (json.loads(p.read_bytes()) for p in (receipt_path, audit_path))
    if audit["unique_memo_rows"] != 5975 or audit["index_enumeration_component_verified"] is not True or audit["source_gate_passed"] is not False:
        raise ValueError("unqualified full posted index required")
    sources = [dict(path=str(p.relative_to(ROOT)), sha256=hashlib.sha256(p.read_bytes()).hexdigest())
               for p in (receipt_path, audit_path, Path(__file__))]
    candidates, seen, direct = [], set(), []
    for query in receipt["queries"][1:]:
        path = ROOT / query["path"]
        if path.resolve() != path or path.parent != DIRECTORY:
            raise ValueError("index original outside retained scope")
        data = path.read_bytes()
        sha = hashlib.sha256(data).hexdigest()
        if sha != query["sha256"] or len(data) != query["bytes"]:
            raise ValueError("index original differs")
        sources.append(dict(path=str(path.relative_to(ROOT)), sha256=sha))
        reader = csv.DictReader(io.StringIO(data.decode("utf-8-sig"), newline=""))
        for row in reader:
            number = row["number"]
            if number in seen:
                raise ValueError("disjoint index scopes overlap")
            seen.add(number)
            title = row[" title"]
            mentions, roots = classify(title)
            if mentions or roots:
                candidate = dict(number=int(number), posted=datetime.strptime(row["post date"], "%b-%d-%Y").date().isoformat(),
                                 effective=datetime.strptime(row["ex/eff date"], "%b-%d-%Y").date().isoformat() if row["ex/eff date"] else None,
                                 title=title, title_mentions=mentions, primary_option_root_title_candidates=roots,
                                 memo_contents_reviewed_this_pass=False, source_gate_passed=False)
                candidates.append(candidate)
                if roots:
                    direct.append(candidate)
    if len(seen) != 5975 or len(candidates) != 79 or {x["number"] for x in direct} != {54623, 54716, 57556, 57651, 57868}:
        raise ValueError("candidate census/known split controls differ")
    report = dict(schema="AutoWheelOCCTermTitleCandidatesV1", observed_at=datetime.now(timezone.utc).isoformat(),
                  indexed_memos=5975, exact_symbol_title_mentions=79, primary_option_root_title_candidates=5,
                  title_mentions_without_primary_study_root=74, primary_candidates=sorted(direct, key=lambda x:x["number"]),
                  candidates=sorted(candidates, key=lambda x:x["number"]),
                  original_positive_split_ids_match=True, known_at=None,
                  title_matching_is_absence_proof=False, affected_series_qualified=False,
                  per_series_terms_qualified=False, source_gate_passed=False, historical_performance_available=False,
                  financial_requests=0, source_files=sources,
                  limitations=["Titles enumerate candidates only; no memo without an exact title mention is certified irrelevant.",
                               "ETF names may contain study tickers while their option roots differ; never transfer their terms to the common-stock strategy.",
                               "Numeric suffix absence cannot certify standard terms; historical ESS and dated adjustment contents must corroborate exact identities."])
    with args.output.open("x") as output:
        json.dump(report, output, indent=2, allow_nan=False)
        output.write("\n")
    print(json.dumps({k:v for k,v in report.items() if k not in {"candidates", "primary_candidates", "source_files"}}))


if __name__ == "__main__":
    main()
