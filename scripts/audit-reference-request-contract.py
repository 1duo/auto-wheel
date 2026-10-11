#!/usr/bin/env python3
"""Audit public provider semantics and prepare a non-executing study request.

This reads pinned public source code/catalogs only. It never imports the SDK,
reads credentials, makes a request, allocates symbols or modifies a journal.
"""
import argparse
import ast
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
REL = Path("research/simulation/2026-10-05")
PINS = {
    "spec.jsonl": "2ff4ae3765f4102a67382d645164ae4dfb6e036cc7ea3b113eab6dc24f4eac98",
    "cycle49-reference-sdk/corporate.py": "9ec588b95db94297e870faf4a8f31f1392dc0d322c8375de08e9900574d53eb3",
    "cycle49-reference-sdk/security.py": "31ad5930fee63d7e2e1f595b5b91fc34af4142184d14c3317b5b9070f1201c91",
    "cycle49-reference-catalog/list_events.json": "ce3bd6087cfe7ca42be005378eea5902291e2619d2987dd6efbe6fb423729897",
    "cycle49-reference-catalog/list_enums.json": "ab94266efa763f0976cc8959c659ad74d34e535754d52a20aad7f0796eaa2fbb",
}
SYMBOLS = ["NFLX", "TSLA", "NVDA", "NBIS", "IREN", "NOW", "AMD", "AMZN", "GOOGL", "META", "MSFT", "PLTR", "UBER", "AVGO", "ORCL"]


def unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate metadata field")
        result[key] = value
    return result


def decode(raw):
    return json.loads(raw, object_pairs_hook=unique,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite metadata")))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.output.exists() or args.output.parent.resolve() != ROOT / REL:
        raise ValueError("fresh bounded output required")
    sources, originals = [], {}
    for name, expected in PINS.items():
        path = ROOT / REL / name
        if path.is_symlink() or not path.is_file() or not 0 < path.stat().st_size <= 4 << 20:
            raise ValueError("bounded regular original required")
        raw = path.read_bytes()
        if hashlib.sha256(raw).hexdigest() != expected:
            raise ValueError("reviewed primary source differs")
        originals[name] = raw
        sources.append(dict(path=str(path.relative_to(ROOT)), sha256=expected))
    own = Path(__file__).resolve()
    own_hash = hashlib.sha256(own.read_bytes()).hexdigest()
    sources.append(dict(path=str(own.relative_to(ROOT)), sha256=own_hash))
    spec = decode(originals["spec.jsonl"])
    if spec["strategy_config"]["symbols"] != SYMBOLS:
        raise ValueError("frozen full universe differs")
    tree = ast.parse(originals["cycle49-reference-sdk/corporate.py"])
    cls = next(node for node in tree.body if isinstance(node, ast.ClassDef) and node.name == "CorporateActionsHttpAPI")
    function = next(node for node in cls.body if isinstance(node, ast.FunctionDef) and node.name == "get_range")
    arguments = function.args.args
    defaults = dict(zip([node.arg for node in arguments[-len(function.args.defaults):]], function.args.defaults))
    if (ast.literal_eval(defaults["pit"]) is not False or ast.literal_eval(defaults["allocate_isins"]) is not True or
            ast.literal_eval(defaults["index"]) != "event_date"):
        raise ValueError("reviewed SDK defaults differ")
    payload = next(node.value for node in function.body
                   if isinstance(node, ast.AnnAssign) and isinstance(node.target, ast.Name) and node.target.id == "data")
    wire_keys = {ast.literal_eval(key) for key in payload.keys}
    if "pit" in wire_keys or not {"start", "end", "index", "symbols", "stype_in", "allocate_isins", "compression"} <= wire_keys:
        raise ValueError("raw request/client PIT separation differs")
    calls = [node for node in ast.walk(function) if isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)]
    if not any(node.func.attr == "groupby" and node.args and ast.literal_eval(node.args[0]) == "event_unique_id" for node in calls):
        raise ValueError("default revision reduction differs")
    if not any(node.func.attr == "agg" and node.args and ast.literal_eval(node.args[0]) == "last" for node in calls):
        raise ValueError("default nullable-field aggregation differs")
    events = decode(originals["cycle49-reference-catalog/list_events.json"])
    enums = decode(originals["cycle49-reference-catalog/list_enums.json"])
    if len(events) != 60 or len(enums) != 235 or {item["code"] for item in enums["ACTION"]} != {"I", "U", "C", "D", "P", "Q"}:
        raise ValueError("complete observed public catalog differs")
    fields = {(item["group"], item["name"]) for item in events["DIV"]["fields"]}
    if not {("rate_info", "gross_dividend"), ("date_info", "declaration_date"),
            ("event_info", "approx_flag"), ("event_info", "tbaflag"),
            ("event_info", "recind_cash_div"), ("event_info", "nil_dividend")} <= fields:
        raise ValueError("cash dividend certainty/cancellation metadata differs")
    if "EQS" not in {item["code"] for item in enums["SECTYPE"]}:
        raise ValueError("observed equity type absent")
    form = dict(start="1970-01-01T00:00:00Z", end="2026-10-03T00:00:00Z", index="ts_record",
                symbols=",".join(SYMBOLS), stype_in="nasdaq_symbol", allocate_isins=True, compression="zstd")
    for name, raw in originals.items():
        if (ROOT / REL / name).read_bytes() != raw:
            raise ValueError("source changed during audit")
    if hashlib.sha256(own.read_bytes()).hexdigest() != own_hash:
        raise ValueError("auditor changed during run")
    report = dict(schema="AutoWheelReferenceRequestContractV1", observed_at=datetime.now(timezone.utc).isoformat(),
                  public_catalog_events=60, public_catalog_enum_groups=235, SDK_source_contract_verified=True,
                  prepared_request=dict(method="POST", url="https://hist.databento.com/v0/corporate_actions.get_range", form=form),
                  execution_allowed=False, source_record_requests=0, financial_requests=0, credentials_used=False,
                  symbols=SYMBOLS, requested_lower_bound_is_not_a_coverage_claim=True,
                  raw_history_required=True, client_PIT_flag_not_sent_to_server=True,
                  default_latest_aggregation_may_mix_nullable_fields=True,
                  event_date_or_ex_date_range_filter_forbidden=True,
                  no_post_study_revisions_needed_for_causal_decisions=True,
                  discovery_without_allocation_cannot_prove_event_absence=True,
                  source_availability_clock_semantics_require_confirmation=True,
                  source_qualified=False, historical_greeks_qualified=False, replay_frames_produced=False,
                  historical_performance_available=False, source_files=sources,
                  acceptance_requirements=[
                      "Confirm subscription/price, symbol-allocation authorization and complete provider coverage; no annual contract enrolled by this audit.",
                      "Dispatch any financial or allocation request only after the sole owned writer is authoritatively terminal, with retained attempt/response/source evidence.",
                      "Retain all raw atomic revisions, cancellations and null fields; never use default latest groupby(last) to construct historical rows.",
                      "Build pre-Apr6 state from provider history; ts_record indexing retains already-known events whose ex-dates lie beyond Oct2.",
                      "Confirm per-version availability clock semantics, then reject versions with record/creation availability after each decision; calendar declaration dates are not intraday timestamps.",
                      "Join exact causal stock identities/currency/listing and reject TBA, approximate, cancelled, unsupported or ambiguous cash benefits.",
                      "Use explicit verified ex-dates; do not shift record dates or infer dates, amounts, missing events, option shares or premium multipliers.",
                  ])
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "w") as output:
        json.dump(report, output, indent=2, allow_nan=False)
        output.write("\n")
        output.flush()
        os.fsync(output.fileno())
    print(json.dumps({key: value for key, value in report.items() if key not in ("source_files", "acceptance_requirements", "prepared_request")}))


if __name__ == "__main__":
    main()
