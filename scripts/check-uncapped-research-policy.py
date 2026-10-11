#!/usr/bin/env python3
"""Narrow controls for the authorized uncapped research monetary contract.

Exercises only the monetary-contract helpers from check-simulation-completion:
legacy $10 cap, version-2 totals above $10B, malformed headers, bool/float and
overflow rejection, exact user authorization including future dating, retained
unknown exclusion proof, and used source-hash/receipt misbindings.

Uses temporary private files for synthetic test data clearly marked as
SYNTHETIC TEST DATA. No full test suite, no simulation run, no source-gate or
performance manufacture. Unknown HTTP504-style holds remain reserved/disclosed
and excluded from replay in these controls. Actual qdata migration is owned by
main; this runner never touches real ledgers, accounts, credentials, or network.
"""
import argparse
import datetime as dt
import hashlib
import importlib.util
import json
import os
import tempfile
from pathlib import Path


def baseline_module():
    path = Path(__file__).resolve().with_name("check-simulation-completion.py")
    spec = importlib.util.spec_from_file_location("autowheel_completion", path)
    if spec is None or spec.loader is None:
        raise ValueError("baseline completion checker unavailable")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


BASE = baseline_module()
SYNTHETIC_MARKER = "SYNTHETIC TEST DATA - UNCAPPED POLICY CONTROL - NOT FOR TRADING"
MAX_INT64 = 9223372036854775807


def synth_hash(label):
    return hashlib.sha256(("synthetic-uncapped-control:" + label).encode()).hexdigest()


def make_entry(op, qh, rh, cost, reserved, at, status, data_sha="", nbytes=0):
    entry = {
        "operation_id": op,
        "query_hash": qh,
        "endpoint": "/timeseries.get_range",
        "cost_nano": cost,
        "reserved_nano": reserved,
        "at": at,
        "response_hash": rh,
        "status": status,
    }
    if status == "complete":
        entry["data_sha256"] = data_sha
        entry["bytes"] = nbytes
    return entry


def valid_at():
    return "2026-10-06T15:00:35Z"


def correct_auth():
    return {
        "schema_version": "AutoWheelResearchSpendingAuthorizationV1",
        "mode": "uncapped",
        "scope": "historical_databento_research",
        "authorized_at": "2026-10-06T15:00:35Z",
        "instruction": BASE.EXPECTED_AUTHORIZATION_INSTRUCTION,
        "cap_nanodollars": None,
        "production_trading_authorization_changed": False,
        "legacy_reservations_policy": BASE.EXPECTED_AUTHORIZATION_LEGACY_POLICY,
        "source_qualification_required": True,
    }

def control_legacy_cap(results):
    op, qh, rh = synth_hash("legacy-op"), synth_hash("legacy-q"), synth_hash("legacy-r")
    dh = synth_hash("legacy-data")
    good = {"version": 1, "lifetime_nano": 10000000000,
            "reservations": [make_entry(op, qh, rh, 100, 110, valid_at(), "complete", dh, 10)]}
    results.append({"control": "legacy_capped_valid", "expected": True,
                    "observed": BASE.valid_ledger(good) and BASE.is_capped_ledger(good),
                    "note": "SYNTHETIC TEST DATA legacy v1 small complete passes"})
    bad_cost = 9100000000
    bad_reserved = bad_cost + (bad_cost + 9) // 10
    bad = {"version": 1, "lifetime_nano": 10000000000,
           "reservations": [make_entry(synth_hash("legacy-big-op"), synth_hash("legacy-big-q"),
                                       synth_hash("legacy-big-r"), bad_cost, bad_reserved, valid_at(), "complete", dh, 10)]}
    results.append({"control": "legacy_cap_exceeded_rejected", "expected": True,
                    "observed": not BASE.valid_ledger(bad),
                    "note": "SYNTHETIC TEST DATA v1 single hold above $10B still fails"})
    two = {"version": 1, "lifetime_nano": 10000000000,
           "reservations": [
               make_entry(synth_hash("legacy-a"), synth_hash("legacy-qa"), synth_hash("legacy-ra"),
                          5000000000, 5500000000, valid_at(), "complete", dh, 10),
               make_entry(synth_hash("legacy-b"), synth_hash("legacy-qb"), synth_hash("legacy-rb"),
                          5000000000, 5500000000, valid_at(), "complete", dh, 10)]}
    results.append({"control": "legacy_sum_exceeded_rejected", "expected": True,
                    "observed": not BASE.valid_ledger(two),
                    "note": "SYNTHETIC TEST DATA v1 sum above $10B still fails"})


def control_version2_totals(results):
    dh = synth_hash("v2-data")
    c1, r1 = 5000000000, 5500000000
    c2, r2 = 5000000000, 5500000000
    big = {"version": 2, "lifetime_nano": 0, "authorization_mode": "uncapped",
           "reservations": [
               make_entry(synth_hash("v2-a"), synth_hash("v2-qa"), synth_hash("v2-ra"), c1, r1, valid_at(), "complete", dh, 10),
               make_entry(synth_hash("v2-b"), synth_hash("v2-qb"), synth_hash("v2-rb"), c2, r2, valid_at(), "complete", dh, 10)]}
    total = r1 + r2
    results.append({"control": "version2_totals_above_10B_allowed", "expected": True,
                    "observed": BASE.valid_ledger(big) and BASE.is_uncapped_ledger(big) and total > 10000000000,
                    "note": "SYNTHETIC TEST DATA v2 total %d above $10B structurally valid; cap removal still requires manifested auth plus audit" % total})


def control_malformed_headers(results):
    base_entry = make_entry(synth_hash("h-op"), synth_hash("h-q"), synth_hash("h-r"), 100, 110, valid_at(), "complete", synth_hash("h-d"), 5)
    cases = [
        ("v1_with_mode", {"version": 1, "lifetime_nano": 10000000000, "authorization_mode": "uncapped", "reservations": [base_entry]}),
        ("v2_wrong_lifetime", {"version": 2, "lifetime_nano": 10000000000, "authorization_mode": "uncapped", "reservations": [base_entry]}),
        ("v2_wrong_mode", {"version": 2, "lifetime_nano": 0, "authorization_mode": "capped", "reservations": [base_entry]}),
        ("v2_missing_mode", {"version": 2, "lifetime_nano": 0, "reservations": [base_entry]}),
        ("v1_extra_mode_empty", {"version": 1, "lifetime_nano": 10000000000, "authorization_mode": "", "reservations": [base_entry]}),
        ("version3", {"version": 3, "lifetime_nano": 0, "authorization_mode": "uncapped", "reservations": [base_entry]}),
        ("lifetime_minus1", {"version": 2, "lifetime_nano": -1, "authorization_mode": "uncapped", "reservations": [base_entry]}),
    ]
    for name, ledger in cases:
        results.append({"control": "malformed_header_rejected:" + name, "expected": True,
                        "observed": not BASE.valid_ledger(ledger),
                        "note": "SYNTHETIC TEST DATA wrong policy/version/header combo rejected"})


def control_bool_float_overflow(results):
    op, qh, rh, dh = synth_hash("b-op"), synth_hash("b-q"), synth_hash("b-r"), synth_hash("b-d")
    bool_cost = {"version": 2, "lifetime_nano": 0, "authorization_mode": "uncapped",
                 "reservations": [make_entry(op, qh, rh, True, 110, valid_at(), "complete", dh, 5)]}
    results.append({"control": "bool_cost_rejected", "expected": True,
                    "observed": not BASE.valid_ledger(bool_cost) and BASE.checked_reserved_amount(True) is None,
                    "note": "SYNTHETIC TEST DATA bool is not int"})
    float_entry = {"operation_id": op, "query_hash": qh, "endpoint": "/timeseries.get_range",
                   "cost_nano": 100, "reserved_nano": 110.0, "at": valid_at(),
                   "response_hash": rh, "status": "complete", "data_sha256": dh, "bytes": 5}
    float_ledger = {"version": 2, "lifetime_nano": 0, "authorization_mode": "uncapped", "reservations": [float_entry]}
    results.append({"control": "float_reserved_rejected", "expected": True,
                    "observed": not BASE.valid_ledger(float_ledger),
                    "note": "SYNTHETIC TEST DATA float is not int"})
    bool_bytes = make_entry(synth_hash("bb-op"), synth_hash("bb-q"), synth_hash("bb-r"), 100, 110, valid_at(), "complete", dh, True)
    bool_bytes_ledger = {"version": 2, "lifetime_nano": 0, "authorization_mode": "uncapped", "reservations": [bool_bytes]}
    results.append({"control": "bool_bytes_rejected", "expected": True,
                    "observed": not BASE.valid_ledger(bool_bytes_ledger),
                    "note": "SYNTHETIC TEST DATA bool bytes rejected"})
    pending_bool = make_entry(op, qh, rh, 100, 110, valid_at(), "reserved")
    pending_bool["bytes"] = False
    results.append({"control": "pending_bool_zero_bytes_rejected", "expected": True,
                    "observed": not BASE.valid_ledger({"version": 2, "lifetime_nano": 0,
                        "authorization_mode": "uncapped", "reservations": [pending_bool]}),
                    "note": "SYNTHETIC TEST DATA false is not an integer zero"})
    results.append({"control": "overflow_margin_rejected", "expected": True,
                    "observed": BASE.checked_reserved_amount(MAX_INT64) is None,
                    "note": "SYNTHETIC TEST DATA MaxInt64 margin overflow rejected"})
    huge_cost = 8000000000000000000
    huge_reserved = huge_cost + (huge_cost + 9) // 10
    overflow_ledger = {"version": 2, "lifetime_nano": 0, "authorization_mode": "uncapped",
                       "reservations": [
                           make_entry(synth_hash("o-a"), synth_hash("o-qa"), synth_hash("o-ra"), huge_cost, huge_reserved, valid_at(), "complete", dh, 5),
                           make_entry(synth_hash("o-b"), synth_hash("o-qb"), synth_hash("o-rb"), huge_cost, huge_reserved, valid_at(), "complete", dh, 5)]}
    results.append({"control": "overflow_sum_rejected", "expected": True,
                    "observed": not BASE.valid_ledger(overflow_ledger),
                    "note": "SYNTHETIC TEST DATA MaxInt64 sum overflow rejected"})


def control_authorization(results):
    now = dt.datetime(2026, 10, 6, 16, 0, 0, tzinfo=dt.timezone.utc)
    good = correct_auth()
    results.append({"control": "authorization_exact_passes", "expected": True,
                    "observed": BASE.valid_spending_authorization_content(good, now),
                    "note": "SYNTHETIC TEST DATA exact manifested instruction passes; real cap removal still needs manifested file"})
    ascii_instruction = dict(good, instruction="Let's lift credit limit (rm $10 limit) unblock and resume goal")
    results.append({"control": "authorization_ascii_apostrophe_rejected", "expected": True,
                    "observed": not BASE.valid_spending_authorization_content(ascii_instruction, now),
                    "note": "SYNTHETIC TEST DATA ASCII apostrophe differs from user U+2019 text"})
    future = dict(good, authorized_at="2026-10-06T16:15:00Z")
    results.append({"control": "authorization_future_rejected", "expected": True,
                    "observed": not BASE.valid_spending_authorization_content(future, now),
                    "note": "SYNTHETIC TEST DATA future authorization rejected"})
    near_future = dict(good, authorized_at=(now + dt.timedelta(seconds=1)).isoformat())
    results.append({"control": "authorization_one_second_future_rejected", "expected": True,
                    "observed": not BASE.valid_spending_authorization_content(near_future, now),
                    "note": "SYNTHETIC TEST DATA authorization must not be future dated"})
    for name, mutate in [
        ("wrong_mode", {"mode": "capped"}),
        ("wrong_scope", {"scope": "live_trading"}),
        ("wrong_schema", {"schema_version": "WrongV1"}),
        ("cap_zero", {"cap_nanodollars": 0}),
        ("production_true", {"production_trading_authorization_changed": True}),
        ("production_one", {"production_trading_authorization_changed": 1}),
    ]:
        bad = dict(good)
        bad.update(mutate)
        results.append({"control": "authorization_rejected:" + name, "expected": True,
                        "observed": not BASE.valid_spending_authorization_content(bad, now),
                        "note": "SYNTHETIC TEST DATA wrong authorization field rejected"})
    missing = dict(good)
    del missing["instruction"]
    results.append({"control": "authorization_missing_key_rejected", "expected": True,
                    "observed": not BASE.valid_spending_authorization_content(missing, now),
                    "note": "SYNTHETIC TEST DATA missing key rejected"})
    extra = dict(good, extra_field=1)
    results.append({"control": "authorization_extra_key_rejected", "expected": True,
                    "observed": not BASE.valid_spending_authorization_content(extra, now),
                    "note": "SYNTHETIC TEST DATA extra key rejected"})

def control_exclusion(results):
    a, b = synth_hash("pending-a"), synth_hash("pending-b")
    pending = {a, b}
    results.append({"control": "exclusion_exact_passes", "expected": True,
                    "observed": BASE.acquisition_exclusion_matches([a, b], pending),
                    "note": "SYNTHETIC TEST DATA excluded equals pending; unknown stays reserved"})
    results.append({"control": "exclusion_missing_rejected", "expected": True,
                    "observed": not BASE.acquisition_exclusion_matches([a], pending),
                    "note": "SYNTHETIC TEST DATA missing pending exclusion rejected"})
    results.append({"control": "exclusion_extra_rejected", "expected": True,
                    "observed": not BASE.acquisition_exclusion_matches([a, b, synth_hash("x")], pending),
                    "note": "SYNTHETIC TEST DATA extra exclusion rejected"})
    results.append({"control": "exclusion_duplicate_rejected", "expected": True,
                    "observed": not BASE.acquisition_exclusion_matches([a, a], {a}),
                    "note": "SYNTHETIC TEST DATA duplicate exclusion rejected"})
    results.append({"control": "exclusion_empty_with_pending_rejected", "expected": True,
                    "observed": not BASE.acquisition_exclusion_matches([], pending),
                    "note": "SYNTHETIC TEST DATA empty exclusion with unresolved rejected"})


def control_batch_submission(results):
    row = make_entry(synth_hash("batch-op"), synth_hash("batch-q"),
                     synth_hash("batch-quote"), 1000000000, 1100000000,
                     valid_at(), "reserved")
    row["endpoint"] = "/batch.submit_job"
    ledger = {"version": 2, "lifetime_nano": 0, "authorization_mode": "uncapped",
              "reservations": [row]}
    results.append({"control": "uncapped_batch_submission_journal_valid", "expected": True,
                    "observed": BASE.valid_ledger(ledger),
                    "note": "SYNTHETIC TEST DATA accepted job stays reserved"})
    legacy = {"version": 1, "lifetime_nano": 10000000000, "reservations": [row]}
    results.append({"control": "capped_batch_submission_rejected", "expected": True,
                    "observed": not BASE.valid_ledger(legacy),
                    "note": "SYNTHETIC TEST DATA bulk needs explicit uncapped policy"})
    forged = dict(row, status="complete", data_sha256=synth_hash("batch-ack"), bytes=1)
    results.append({"control": "batch_ack_as_completed_data_rejected", "expected": True,
                    "observed": not BASE.valid_ledger(dict(ledger, reservations=[forged])),
                    "note": "SYNTHETIC TEST DATA job ACK cannot complete market-data evidence"})


def write_private(path, data):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)
    try:
        os.chmod(path, 0o600)
    except OSError:
        pass


def build_temp_audit_case(tmp, mutate=None):
    op_complete = synth_hash("used-op")
    op_pending = synth_hash("pending-504-op")
    q_complete = synth_hash("used-q")
    q_pending = synth_hash("pending-q")
    r_complete = synth_hash("used-r")
    r_pending = synth_hash("pending-r")
    source_bytes = (SYNTHETIC_MARKER + "\nused-body:" + op_complete + "\n").encode()
    source_sha = hashlib.sha256(source_bytes).hexdigest()
    cost, reserved = 250000000, 275000000
    nbytes = len(source_bytes)
    ledger_bytes = nbytes
    ledger_data_sha = source_sha
    if mutate == "bytecount_mismatch":
        ledger_bytes = nbytes + 1
    ledger = {
        "version": 2,
        "lifetime_nano": 0,
        "authorization_mode": "uncapped",
        "reservations": [
            make_entry(op_complete, q_complete, r_complete, cost, reserved, valid_at(), "complete", ledger_data_sha, ledger_bytes),
            {"operation_id": op_pending, "query_hash": q_pending, "endpoint": "/timeseries.get_range",
             "cost_nano": cost, "reserved_nano": reserved, "at": valid_at(),
             "response_hash": r_pending, "status": "reserved"},
        ],
    }
    receipt = {
        "OperationID": op_complete,
        "QueryHash": q_complete,
        "Endpoint": "/timeseries.get_range",
        "CostNano": 894070,
        "ReservedNano": reserved,
        "ResponseSHA256": source_sha,
        "Bytes": nbytes,
        "StatusCode": 200,
        "At": valid_at(),
    }
    if mutate == "receipt_reserved_mismatch":
        receipt["ReservedNano"] = reserved + 1
    elif mutate == "receipt_status_500":
        receipt["StatusCode"] = 500
    elif mutate == "receipt_cost_above_basis":
        receipt["CostNano"] = cost + 1
    elif mutate == "receipt_hash_wrong":
        receipt["ResponseSHA256"] = synth_hash("wrong")
    elif mutate == "bytecount_mismatch":
        receipt["Bytes"] = nbytes
        receipt["ResponseSHA256"] = source_sha
    src_rel = "synthetic/source.bin"
    rcp_rel = "synthetic/receipt.json"
    ledger_rel = "synthetic/ledger.json"
    src_path = Path(tmp) / src_rel
    rcp_path = Path(tmp) / rcp_rel
    ledger_path = Path(tmp) / ledger_rel
    body = source_bytes
    if mutate == "source_hash_mismatch":
        body = (SYNTHETIC_MARKER + "\nMUTATED\n").encode()
    write_private(src_path, body)
    rcp_raw = json.dumps(receipt, separators=(",", ":")).encode() + b"\n"
    write_private(rcp_path, rcp_raw)
    ledger_raw = json.dumps(ledger, separators=(",", ":")).encode() + b"\n"
    write_private(ledger_path, ledger_raw)
    actual_src_sha = hashlib.sha256(src_path.read_bytes()).hexdigest()
    manifest = {
        src_rel: {"path": src_rel, "sha256": actual_src_sha},
        rcp_rel: {"path": rcp_rel, "sha256": hashlib.sha256(rcp_raw).hexdigest()},
        ledger_rel: {"path": ledger_rel, "sha256": hashlib.sha256(ledger_raw).hexdigest()},
    }
    verified = set()
    for rel, item in manifest.items():
        p = Path(tmp) / rel
        if p.is_file() and hashlib.sha256(p.read_bytes()).hexdigest() == item["sha256"]:
            verified.add(rel)
    if mutate == "used_reserved" or mutate == "pending_receipt_as_body":
        used = [{"operation_id": op_pending, "source_path": src_rel, "receipt_path": rcp_rel}]
    elif mutate == "no_used":
        used = []
    else:
        used = [{"operation_id": op_complete, "source_path": src_rel, "receipt_path": rcp_rel}]
    audit = {
        "status": "passed",
        "ledger_sha256": manifest[ledger_rel]["sha256"],
        "rationale": "SYNTHETIC TEST DATA exclusion rationale: unknown 504 stays reserved, used receipt bound",
        "excluded_unresolved_operation_ids": [op_pending],
        "used_databento_sources": used,
    }
    if mutate == "excluded_missing":
        audit["excluded_unresolved_operation_ids"] = []
    entries_by_id = {r["operation_id"]: r for r in ledger["reservations"]}
    pending_set = {op_pending}
    return audit, entries_by_id, pending_set, manifest, verified, Path(tmp), manifest[ledger_rel]["sha256"]


def control_used_bindings(results):
    with tempfile.TemporaryDirectory(prefix="autowheel-uncapped-ctrl-") as tmp:
        try:
            os.chmod(tmp, 0o700)
        except OSError:
            pass
        audit, by_id, pending, manifest, verified, root, ledger_sha = build_temp_audit_case(tmp)
        ok, _ = BASE.validate_acquisition_audit(audit, by_id, pending, manifest, verified, root, ledger_sha)
        results.append({"control": "used_binding_correct_passes", "expected": True, "observed": ok,
                        "note": "SYNTHETIC TEST DATA correct used hash/bytecount/receipt passes; unknown excluded"})
        for name in ["source_hash_mismatch", "bytecount_mismatch", "receipt_reserved_mismatch", "receipt_status_500",
                     "receipt_cost_above_basis", "receipt_hash_wrong", "used_reserved",
                     "pending_receipt_as_body", "no_used", "excluded_missing"]:
            with tempfile.TemporaryDirectory(prefix="autowheel-uncapped-ctrl-") as tmp2:
                try:
                    os.chmod(tmp2, 0o700)
                except OSError:
                    pass
                a, b, p, m, v, r, s = build_temp_audit_case(tmp2, mutate=name)
                ok2, _ = BASE.validate_acquisition_audit(a, b, p, m, v, r, s)
                results.append({"control": "misbinding_rejected:" + name, "expected": True,
                                "observed": not ok2,
                                "note": "SYNTHETIC TEST DATA " + name + " rejected; no fake completion"})


def run_all():
    results = []
    control_legacy_cap(results)
    control_version2_totals(results)
    control_malformed_headers(results)
    control_bool_float_overflow(results)
    control_authorization(results)
    control_exclusion(results)
    control_batch_submission(results)
    control_used_bindings(results)
    passed = sum(1 for r in results if r["expected"] == r["observed"])
    total = len(results)
    summary = {
        "synthetic_marker": SYNTHETIC_MARKER,
        "controls_passed": passed,
        "controls_total": total,
        "all_expected_behaviors_observed": passed == total,
        "scope_notes": [
            "cap removal only with actual manifested user authorization; structural v2 validity alone does not authorize spending",
            "no passing source gate or performance manufactured; full independent-review qualification remains required separately",
            "unknown 504-style hold remains reserved/disclosed and excluded from replay; never marked complete",
            "invoices remain disclosed unverified; data acquisition fees excluded from simulated performance",
            "actual qdata migration owned by main; this runner uses only synthetic temp private files",
        ],
        "results": results,
    }
    return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--json", type=Path, required=False, default=None)
    args = parser.parse_args()
    summary = run_all()
    output = json.dumps(summary, indent=2, allow_nan=False) + "\n"
    if args.json is not None:
        with args.json.open("x", encoding="utf-8") as handle:
            handle.write(output)
    print(output, end="")
    raise SystemExit(0 if summary["all_expected_behaviors_observed"] else 1)


if __name__ == "__main__":
    main()
