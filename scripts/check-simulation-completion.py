#!/usr/bin/env python3
"""Completion gate for the frozen six-month research goal; blocked is a failure.

Source qualification is an independent reviewed audit, not inferred from a
successful API response or from the replay's own coverage declarations.
"""
import argparse
import datetime as dt
import hashlib
import importlib.util
import json
import math
import re
from pathlib import Path

SYMBOLS = ["NFLX", "TSLA", "NVDA", "NBIS", "IREN", "NOW", "AMD", "AMZN",
           "GOOGL", "META", "MSFT", "PLTR", "UBER", "AVGO", "ORCL"]
GATES = ["point_in_time_chain_roster", "event_nbbo_and_sizes",
         "causal_open_interest", "causal_greeks", "raw_stock_quotes",
         "corporate_actions_and_deliverables", "continuous_execution_coverage",
         "settlement_and_lifecycle_model", "independent_review"]
FROZEN_SPEC_SHA256 = "2ff4ae3765f4102a67382d645164ae4dfb6e036cc7ea3b113eab6dc24f4eac98"
STRATEGY_FILES = ["internal/wheel/" + n for n in ("types.go", "strategy.go", "engine.go", "calendar.go")]
ENGINE_FILES = ["internal/simulation/types.go", "internal/simulation/replay.go",
                "internal/simulation/selection_omission.go",
                "internal/simulation/fixture.go", "cmd/autowheel-sim/main.go"]
MAX_INT64 = 9223372036854775807
CAPPED_LIFETIME_NANO = 10_000_000_000
UNCAPPED_VERSION = 2
UNCAPPED_LIFETIME_NANO = 0
UNCAPPED_AUTHORIZATION_MODE = "uncapped"
EXPECTED_AUTHORIZATION_SCHEMA = "AutoWheelResearchSpendingAuthorizationV1"
EXPECTED_AUTHORIZATION_SCOPE = "historical_databento_research"
EXPECTED_AUTHORIZATION_INSTRUCTION = "Let\u2019s lift credit limit (rm $10 limit) unblock and resume goal"
EXPECTED_AUTHORIZATION_LEGACY_POLICY = "Preserve all reservations and unresolved attempts; no release, retry or inferred completion."


def code_digest(root, paths, basename=False):
    result = hashlib.sha256()
    for name in paths:
        raw = (root / name).read_bytes()
        label = Path(name).name if basename else name
        result.update(label.encode() + b"\0" + str(len(raw)).encode() + b"\0" + raw)
    return result.hexdigest()


def read_json(path):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError("duplicate JSON field")
            result[key] = value
        return result
    return json.loads(path.read_text(), object_pairs_hook=unique,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite JSON")))


def digest(path):
    result = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b""):
            result.update(chunk)
    return result.hexdigest()


def is_hash(value):
    return isinstance(value, str) and len(value) == 64 and all(
        character in "0123456789abcdefABCDEF" for character in value)


def valid_time(value):
    if not isinstance(value, str):
        return None
    match = re.fullmatch(r"(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})", value)
    if not match:
        return None
    # Python 3.9 accepts only microseconds; Go receipts use RFC3339Nano.
    fractional = "." + match[2][:6].ljust(6, "0") if match[2] else ""
    offset = "+00:00" if match[3] == "Z" else match[3]
    try:
        at = dt.datetime.fromisoformat(match[1] + fractional + offset)
        return at if at.tzinfo and at.year > 1 else None
    except ValueError:
        return None


def is_capped_ledger(ledger):
    """Narrow pure header check for legacy capped v1 (exact 3 keys)."""
    return (isinstance(ledger, dict)
            and type(ledger.get("version")) is int and ledger.get("version") == 1
            and type(ledger.get("lifetime_nano")) is int and ledger.get("lifetime_nano") == CAPPED_LIFETIME_NANO
            and "authorization_mode" not in ledger
            and set(ledger) == {"version", "lifetime_nano", "reservations"})


def is_uncapped_ledger(ledger):
    """Narrow pure header check for explicit uncapped v2 (exact 4 keys)."""
    return (isinstance(ledger, dict)
            and type(ledger.get("version")) is int and ledger.get("version") == UNCAPPED_VERSION
            and type(ledger.get("lifetime_nano")) is int and ledger.get("lifetime_nano") == UNCAPPED_LIFETIME_NANO
            and ledger.get("authorization_mode") == UNCAPPED_AUTHORIZATION_MODE
            and set(ledger) == {"version", "lifetime_nano", "authorization_mode", "reservations"})


def checked_reserved_amount(cost):
    """Pure margin helper: exact cost+(cost+9)//10 or None if invalid/overflow. Rejects bool/float."""
    if type(cost) is not int or cost < 0 or cost > MAX_INT64:
        return None
    if cost == 0:
        return 0
    margin = cost // 10 + (1 if cost % 10 else 0)
    if cost > MAX_INT64 - margin:
        return None
    reserved = cost + margin
    if reserved < 0 or reserved > MAX_INT64 or reserved != cost + (cost + 9) // 10:
        return None
    return reserved


def valid_spending_authorization_content(doc, now):
    """Pure authorization content check (no manifest I/O). `now` must be tz-aware datetime."""
    try:
        if not isinstance(doc, dict):
            return False
        if set(doc) != {"schema_version", "mode", "scope", "authorized_at", "instruction",
                        "cap_nanodollars", "production_trading_authorization_changed",
                        "legacy_reservations_policy", "source_qualification_required"}:
            return False
        if doc.get("schema_version") != EXPECTED_AUTHORIZATION_SCHEMA:
            return False
        if doc.get("mode") != UNCAPPED_AUTHORIZATION_MODE:
            return False
        if doc.get("scope") != EXPECTED_AUTHORIZATION_SCOPE:
            return False
        if doc.get("instruction") != EXPECTED_AUTHORIZATION_INSTRUCTION:
            return False
        if "cap_nanodollars" not in doc or doc["cap_nanodollars"] is not None:
            return False
        if doc.get("production_trading_authorization_changed") is not False:
            return False
        if doc.get("legacy_reservations_policy") != EXPECTED_AUTHORIZATION_LEGACY_POLICY:
            return False
        if doc.get("source_qualification_required") is not True:
            return False
        at = valid_time(doc.get("authorized_at"))
        if not at:
            return False
        if not isinstance(now, dt.datetime) or now.tzinfo is None:
            return False
        if at > now:
            return False
        return True
    except (AttributeError, TypeError, ValueError):
        return False


def receipt_content_matches(receipt, entry):
    """Pure receipt-to-ledger binding. Uppercase Go fields; CostNano <= cost basis (not assumed equal)."""
    try:
        if not isinstance(receipt, dict) or not isinstance(entry, dict):
            return False
        if receipt.get("OperationID") != entry.get("operation_id"):
            return False
        if receipt.get("QueryHash") != entry.get("query_hash"):
            return False
        if receipt.get("Endpoint") != "/timeseries.get_range":
            return False
        if receipt.get("Endpoint") != entry.get("endpoint"):
            return False
        if not is_hash(receipt.get("ResponseSHA256")):
            return False
        if receipt.get("ResponseSHA256") != entry.get("data_sha256"):
            return False
        rb, eb = receipt.get("Bytes"), entry.get("bytes")
        if type(rb) is not int or type(eb) is not int or rb <= 0 or eb <= 0 or rb != eb:
            return False
        if type(receipt.get("StatusCode")) is not int or receipt.get("StatusCode") != 200:
            return False
        rr, er = receipt.get("ReservedNano"), entry.get("reserved_nano")
        if type(rr) is not int or type(er) is not int or rr != er:
            return False
        rc, ec = receipt.get("CostNano"), entry.get("cost_nano")
        if type(rc) is not int or type(ec) is not int or rc < 0 or ec < 0 or rc > ec:
            return False
        if not is_hash(receipt.get("OperationID")) or not is_hash(receipt.get("QueryHash")):
            return False
        return True
    except (AttributeError, TypeError):
        return False


def acquisition_exclusion_matches(excluded, pending_set):
    """Pure exclusion check: excluded list exact-equals pending set, unique hashes."""
    try:
        if not isinstance(excluded, list):
            return False
        if not isinstance(pending_set, set):
            pending_set = set(pending_set)
        if len(excluded) != len(set(excluded)):
            return False
        if not all(isinstance(x, str) and is_hash(x) for x in excluded):
            return False
        if set(excluded) != set(pending_set):
            return False
        return True
    except (TypeError, AttributeError):
        return False


def validate_acquisition_audit(audit, entries_by_id, pending_set, manifest, verified, root, expected_ledger_sha):
    """Narrow acquisition-exclusion plus used-receipt proof (reads files, no writes/network). Returns (ok, reason)."""
    try:
        if not isinstance(audit, dict):
            return False, "audit not a dict"
        if set(audit) != {"status", "ledger_sha256", "rationale",
                          "excluded_unresolved_operation_ids", "used_databento_sources"}:
            return False, "audit keys"
        if audit.get("status") != "passed":
            return False, "audit status"
        ledger_sha = audit.get("ledger_sha256")
        if not is_hash(ledger_sha) or ledger_sha != expected_ledger_sha:
            return False, "ledger sha"
        rationale = audit.get("rationale")
        if not isinstance(rationale, str) or not rationale.strip():
            return False, "rationale"
        excluded = audit.get("excluded_unresolved_operation_ids")
        if not acquisition_exclusion_matches(excluded, pending_set):
            return False, "excluded"
        used = audit.get("used_databento_sources")
        if not isinstance(used, list):
            return False, "used not list"
        if pending_set and len(used) < 1:
            return False, "need used when unresolved exists"
        seen_used = set()
        for binding in used:
            if not isinstance(binding, dict) or set(binding) != {"operation_id", "source_path", "receipt_path"}:
                return False, "binding keys"
            op = binding.get("operation_id")
            src = binding.get("source_path")
            rcp = binding.get("receipt_path")
            if not is_hash(op) or not isinstance(src, str) or not isinstance(rcp, str):
                return False, "binding types"
            if op in seen_used:
                return False, "duplicate used"
            seen_used.add(op)
            if op in pending_set:
                return False, "used is reserved"
            entry = entries_by_id.get(op)
            if not isinstance(entry, dict) or entry.get("status") not in ("complete", "acquired"):
                return False, "used not complete"
            if src not in verified or rcp not in verified:
                return False, "not verified"
            if src == rcp:
                return False, "src==rcp"
            if src not in manifest or rcp not in manifest:
                return False, "not in manifest"
            if entry.get("status") == "acquired":
                if "scripts/batch_acquisition_proof.py" not in verified:
                    return False, "batch verifier not manifested"
                module_path = root / "scripts/batch_acquisition_proof.py"
                module_spec = importlib.util.spec_from_file_location("autowheel_batch_proof", module_path)
                if module_spec is None or module_spec.loader is None:
                    return False, "batch verifier unavailable"
                module = importlib.util.module_from_spec(module_spec)
                module_spec.loader.exec_module(module)
                ok, reason = module.validate_batch_acquisition(entry, src, rcp, manifest, verified, root)
                if not ok:
                    return False, reason
                continue
            if manifest.get(src, {}).get("sha256") != entry.get("data_sha256"):
                return False, "source hash"
            src_path = Path(src) if Path(src).is_absolute() else root / src
            rcp_path = Path(rcp) if Path(rcp).is_absolute() else root / rcp
            try:
                if not src_path.is_file() or not rcp_path.is_file():
                    return False, "file missing"
                if digest(src_path) != entry.get("data_sha256"):
                    return False, "source digest"
                if src_path.stat().st_size != entry.get("bytes"):
                    return False, "bytecount"
                receipt = read_json(rcp_path)
                if not receipt_content_matches(receipt, entry):
                    return False, "receipt mismatch"
            except (OSError, ValueError, TypeError):
                return False, "file read"
        # Every acquired batch needs its full proof, including batches whose
        # records were filtered out during normalization. An unproved status
        # cannot remove an unknown attempt from the unresolved exclusions.
        if any(entry.get("status") == "acquired" and op not in seen_used
               for op, entry in entries_by_id.items()):
            return False, "acquired operation unproved"
        return True, ""
    except (AttributeError, TypeError, ValueError):
        return False, "exception"


def valid_ledger(ledger):
    # Accepts legacy capped v1 (exact version1/lifetime10B/3 keys, <=10B cap) OR explicit uncapped v2
    # (exact version2/lifetime0/mode uncapped/4 keys, <=MaxInt64). Rejects every other header combo;
    # bool/float rejected via type-is-int; margin and sum checked.
    if is_capped_ledger(ledger):
        cap = CAPPED_LIFETIME_NANO
    elif is_uncapped_ledger(ledger):
        cap = MAX_INT64
    else:
        return False
    entries = ledger.get("reservations")
    if not isinstance(entries, list) or len(entries) > 100000:
        return False
    seen, total = set(), 0
    allowed = {"operation_id", "query_hash", "endpoint", "cost_nano", "reserved_nano",
               "at", "response_hash", "status", "data_sha256", "bytes"}
    for entry in entries:
        if not isinstance(entry, dict) or not set(entry).issubset(allowed):
            return False
        if not all(is_hash(entry.get(field)) for field in ("operation_id", "query_hash", "response_hash")):
            return False
        if entry["operation_id"] in seen:
            return False
        endpoint = entry.get("endpoint")
        if endpoint != "/timeseries.get_range":
            # A submission ACK stays reserved. A separate acquisition proof
            # may record acquired bytes; it cannot assert invoice or source
            # qualification and is verified separately for any used source.
            if (not is_uncapped_ledger(ledger) or endpoint != "/batch.submit_job"
                    or entry.get("status") not in ("reserved", "acquired")):
                return False
        elif entry.get("status") == "acquired":
            return False
        seen.add(entry["operation_id"])
        cost, reserved = entry.get("cost_nano"), entry.get("reserved_nano")
        if type(cost) is not int or type(reserved) is not int or cost < 0 or cost > MAX_INT64:
            return False
        if reserved < 0 or reserved > cap:
            return False
        expect = checked_reserved_amount(cost)
        if expect is None or reserved != expect:
            return False
        at = valid_time(entry.get("at"))
        if not at or at > dt.datetime.now(dt.timezone.utc) + dt.timedelta(minutes=2):
            return False
        if entry.get("status") in ("complete", "acquired"):
            if not is_hash(entry.get("data_sha256")) or type(entry.get("bytes")) is not int or entry["bytes"] <= 0:
                return False
            if entry["bytes"] > MAX_INT64:
                return False
        else:
            if entry.get("status") != "reserved" or entry.get("data_sha256", "") != "":
                return False
            if "bytes" in entry and (type(entry["bytes"]) is not int or entry["bytes"] != 0):
                return False
        if total > cap - reserved:
            return False
        total += reserved
        if total < 0 or total > cap:
            return False
    return True


def valid_inventory(state):
    if not isinstance(state, dict) or type(state.get("version")) is not int or state.get("version") != 1:
        return False
    positions, intents = state.get("positions"), state.get("intents")
    if not isinstance(positions, dict) or not isinstance(intents, dict) or len(positions) > 3 or len(intents) > 3:
        return False
    maximum = 1_000_000_000
    for symbol, position in positions.items():
        if not isinstance(symbol, str) or not 1 <= len(symbol) <= 5 or not isinstance(position, dict):
            return False
        owned, cost = position.get("owned_shares"), position.get("cost_cents", 0)
        if type(owned) is not int or owned not in (0, 100) or type(cost) is not int or not 0 <= cost <= maximum:
            return False
        if owned == 100 and (cost == 0 or not position.get("provenance") or position.get("put_id")):
            return False
        if position.get("put_id"):
            strike = position.get("put_strike_cents")
            if (type(strike) is not int or not 0 < strike <= maximum
                    or not valid_time(position.get("put_opened_at")) or not position.get("provenance")):
                return False
    for symbol, intent in intents.items():
        if not isinstance(intent, dict) or not isinstance(intent.get("candidate"), dict):
            return False
        candidate = intent["candidate"]
        strike = candidate.get("strike_cents")
        if (symbol != candidate.get("symbol") or not intent.get("ref_id") or not candidate.get("id")
                or type(strike) is not int or not 0 < strike <= maximum or not valid_time(intent.get("at"))):
            return False
    return True


def check(report, qualification, root, *,
          frozen_spec_path="research/simulation/2026-10-05/spec.jsonl",
          frozen_spec_sha256=FROZEN_SPEC_SHA256, research_variant=""):
    gaps = []
    def require(ok, message):
        if not ok:
            gaps.append(message)
    require(report.get("status") == "counterfactual", "historical replay has no qualified performance")
    require(report.get("schema_version") == "AutoWheelReplayReportV1", "report schema missing")
    require(not report.get("blockers"), "replay blockers remain")
    spec = report.get("spec", {})
    frozen_path = root / frozen_spec_path
    require(digest(frozen_path) == frozen_spec_sha256, "frozen specification changed")
    frozen = read_json(frozen_path)
    require(spec.get("research_variant", "") == research_variant, "study variant mismatch")
    config = dict(spec.get("strategy_config", {}))
    if config.get("account_number") == "":
        config.pop("account_number")
    require(config == frozen["strategy_config"], "frozen strategy settings changed")
    require(spec.get("peer_reserved_cash_cents") == 0 and not spec.get("peer_reserved_symbols"),
            "base-case peer scenario changed")
    require(spec.get("mode") == "historical_quote_replay", "fixture cannot satisfy historical goal")
    require((spec.get("start_date"), spec.get("end_date")) == ("2026-04-06", "2026-10-02"), "window changed")
    require(spec.get("initial_cash_cents") == 8_000_000, "starting capital changed")
    require(spec.get("strategy_config", {}).get("symbols") == SYMBOLS, "frozen universe changed")
    require(spec.get("corporate_actions_audited") is True, "corporate action audit missing")
    for kind, count in (("decisions", 9324), ("valuations", 126)):
        require(report.get("expected_" + kind) == count and report.get("observed_" + kind) == count,
                kind + " coverage incomplete")
    require(qualification.get("status") == "qualified", "source qualification incomplete")
    require(qualification.get("schema_version") == "AutoWheelDatabentoQualificationV1", "qualification schema missing")
    require(qualification.get("symbols") == SYMBOLS, "source universe incomplete")
    require(qualification.get("window") == {"start": "2026-04-06", "end": "2026-10-02"}, "source window incomplete")
    for name in GATES:
        gate = qualification.get("gates", {}).get(name, {})
        require(gate.get("status") == "passed" and bool(gate.get("evidence_files")), name + " not verified")
    budget = qualification.get("budget", {})
    used = budget.get("reserved_nanodollars")
    budget_mode = budget.get("authorization_mode")
    if budget_mode == UNCAPPED_AUTHORIZATION_MODE:
        require(type(used) is int and 0 <= used <= MAX_INT64, "uncapped cumulative budget unproved")
        require("cap_nanodollars" in budget and budget["cap_nanodollars"] is None
                and "remaining_nanodollars" in budget and budget["remaining_nanodollars"] is None,
                "uncapped budget cap must be null")
        require(type(budget.get("ambiguous_requests")) is int and budget["ambiguous_requests"] >= 0,
                "uncapped ambiguous count invalid")
    else:
        require(type(used) is int and 0 <= used <= 10_000_000_000, "$10 cumulative budget unproved")
        require(type(budget.get("cap_nanodollars")) is int and budget.get("cap_nanodollars") == 10_000_000_000
                and type(budget.get("ambiguous_requests")) is int and budget.get("ambiguous_requests") == 0,
                "budget authorization or financial receipts unresolved")
    files = qualification.get("source_files", [])
    require(bool(files), "immutable source manifest missing")
    verified = set()
    for record in files:
        try:
            path = Path(record["path"])
            if not path.is_absolute():
                path = root / path
            matches = path.is_file() and digest(path) == record["sha256"]
            require(matches, "source hash mismatch: " + record["path"])
            if matches:
                verified.add(record["path"])
        except (OSError, KeyError, TypeError):
            gaps.append("source file unavailable or malformed")
    for name in GATES:
        require(set(qualification.get("gates", {}).get(name, {}).get("evidence_files", [])).issubset(verified),
                name + " evidence unavailable")
    for field in ("input_sha256", "strategy_sha256", "simulation_engine_sha256"):
        value = report.get(field)
        require(isinstance(value, str) and len(value) == 64 and qualification.get(field) == value,
                field + " not independently bound")
    require(report.get("strategy_sha256") == code_digest(root, STRATEGY_FILES, True), "strategy source mismatch")
    require(report.get("simulation_engine_sha256") == code_digest(root, ENGINE_FILES), "simulation source mismatch")
    normalized = qualification.get("normalized_input", {})
    require(normalized.get("path") in verified and normalized.get("sha256") == report.get("input_sha256"),
            "normalized replay input not verified")
    manifest = {item["path"]: item for item in files}
    require(len(manifest) == len(files), "duplicate source manifest paths")
    require(manifest.get(normalized.get("path"), {}).get("sha256") == report.get("input_sha256"),
            "replay input hash differs from manifest")
    # An arbitrary input file marked 'passed' is not an independent audit.
    # Require a separate review artifact binding the exact input, code and
    # report, with a finding and cited source evidence for each source gate.
    review_info = qualification.get("independent_review", {})
    review_path = review_info.get("path")
    require(review_path in verified and review_path != normalized.get("path"),
            "separate independent review artifact missing")
    if review_path in verified:
        path = Path(review_path)
        review = read_json(path if path.is_absolute() else root / path)
        require(review.get("schema_version") == "AutoWheelSourceReviewV1"
                and review.get("status") == "passed" and bool(review.get("reviewer")),
                "independent source review not accepted")
        require(review.get("frozen_spec_sha256") == frozen_spec_sha256,
                "independent review specification mismatch")
        for field in ("input_sha256", "strategy_sha256", "simulation_engine_sha256"):
            require(review.get(field) == report.get(field), "review does not bind " + field)
        report_path = review_info.get("report_path")
        require(report_path in verified and manifest.get(report_path, {}).get("sha256")
                == review.get("report_sha256"), "review does not bind replay report")
        if report_path in verified:
            path = Path(report_path)
            require(read_json(path if path.is_absolute() else root / path) == report,
                    "reviewed report differs from evaluated report")
        for name in GATES[:-1]:
            finding = review.get("findings", {}).get(name, {})
            evidence = finding.get("evidence_files", [])
            declared = qualification.get("gates", {}).get(name, {}).get("evidence_files", [])
            require(finding.get("status") == "passed" and bool(finding.get("rationale"))
                    and bool(evidence) and set(evidence).issubset(verified)
                    and set(evidence).issubset(declared) and review_path not in evidence
                    and report_path not in evidence and normalized.get("path") not in evidence,
                    name + " has no independent source finding")
        require(qualification.get("gates", {}).get("independent_review", {}).get("evidence_files")
                == [review_path], "independent review gate not bound to review artifact")
    ledger_path = budget.get("ledger_path")
    require(ledger_path in verified, "durable budget ledger evidence missing")
    if ledger_path in verified:
        path = Path(ledger_path)
        ledger = read_json(path if path.is_absolute() else root / path)
        require(valid_ledger(ledger), "durable budget ledger invalid")
        entries = ledger.get("reservations", []) if isinstance(ledger, dict) else []
        if is_uncapped_ledger(ledger):
            # Authorized uncapped monetary contract: cap removal only with actual manifested
            # user authorization; unknown 504 stays reserved/disclosed and excluded from replay;
            # financial completion requires explicit pending exclusion plus used-receipt proof.
            # Existing full independent-review qualification above remains required separately.
            require(budget_mode == UNCAPPED_AUTHORIZATION_MODE, "uncapped budget mode differs from ledger")
            require(type(used) is int and 0 <= used <= MAX_INT64, "uncapped cumulative budget unproved")
            require("cap_nanodollars" in budget and budget["cap_nanodollars"] is None
                    and "remaining_nanodollars" in budget and budget["remaining_nanodollars"] is None,
                    "uncapped budget cap must be null")
            require(all(type(r.get("reserved_nano")) is int and 0 <= r.get("reserved_nano", -1) <= MAX_INT64 for r in entries),
                    "durable budget entries invalid")
            total = 0
            overflow = False
            for r in entries:
                v = r.get("reserved_nano")
                if type(v) is not int or v < 0 or v > MAX_INT64 or total > MAX_INT64 - v:
                    overflow = True
                    break
                total += v
            require(not overflow and total == used, "budget total differs from durable ledger")
            pending_ids = [r.get("operation_id") for r in entries if isinstance(r, dict) and r.get("status") == "reserved"]
            require(type(budget.get("ambiguous_requests")) is int and budget["ambiguous_requests"] == len(pending_ids),
                    "uncapped ambiguous count differs from ledger")
            require(budget.get("actual_billed_cost") == "unverified", "invoice outcomes must remain disclosed unverified")
            auth_path = budget.get("authorization_path")
            require(auth_path in verified, "uncapped spending authorization missing or invalid")
            if auth_path in verified:
                try:
                    ap = Path(auth_path)
                    auth_doc = read_json(ap if ap.is_absolute() else root / ap)
                    now = dt.datetime.now(dt.timezone.utc)
                    require(valid_spending_authorization_content(auth_doc, now),
                            "uncapped spending authorization missing or invalid")
                except (OSError, ValueError, TypeError, AttributeError):
                    require(False, "uncapped spending authorization missing or invalid")
            try:
                rpath = review_info.get("path") if isinstance(review_info, dict) else None
                if rpath in verified:
                    rp = Path(rpath)
                    review_doc = read_json(rp if rp.is_absolute() else root / rp)
                    audit = review_doc.get("acquisition_audit")
                    entries_by_id = {r.get("operation_id"): r for r in entries if isinstance(r, dict)}
                    pending_set = set(pending_ids)
                    expected_ledger_sha = manifest.get(ledger_path, {}).get("sha256")
                    ok, _ = validate_acquisition_audit(audit, entries_by_id, pending_set,
                                                      manifest, verified, root, expected_ledger_sha)
                    require(ok, "unresolved data acquisition not excluded with used-receipt proof")
                else:
                    require(False, "unresolved data acquisition not excluded with used-receipt proof")
            except (OSError, ValueError, TypeError, AttributeError, KeyError):
                require(False, "unresolved data acquisition not excluded with used-receipt proof")
        else:
            require(all(type(r.get("reserved_nano")) is int and r["reserved_nano"] >= 0 for r in entries),
                    "durable budget entries invalid")
            require(sum(r.get("reserved_nano", 10_000_000_001) for r in entries) == used,
                    "budget total differs from durable ledger")
            require(all(r.get("status") == "complete" and r.get("data_sha256") for r in entries),
                    "ambiguous financial requests remain")
    perf = report.get("performance") or {}
    require(bool(perf), "performance metrics absent")
    require(all(type(perf.get(k)) is int for k in ("ending_equity_cents", "net_pnl_cents")), "equity metrics invalid")
    require(all(type(perf.get(k)) is int for k in ("premiums_cents", "fees_cents", "stock_realized_cents",
                "dividends_cents", "submissions", "fills", "expired_orders", "assignments", "call_aways", "blocked_cycles")),
            "accounting or trade metrics invalid")
    if type(perf.get("ending_equity_cents")) is int and type(perf.get("net_pnl_cents")) is int:
        require(perf["ending_equity_cents"] - 8_000_000 == perf["net_pnl_cents"], "equity/P&L mismatch")
    require(all(isinstance(perf.get(k), (float, int)) and not isinstance(perf.get(k), bool)
                and math.isfinite(perf[k]) for k in ("return_pct", "max_drawdown_pct")), "return metrics invalid")
    curve = report.get("equity_curve", [])
    require(len(curve) == 126, "daily equity curve incomplete")
    if curve:
        try:
            times = [valid_time(point["at"]) for point in curve]
            require(all(times) and all(a < b for a, b in zip(times, times[1:])),
                    "equity timestamps invalid or repeated")
            equities = [point["equity_cents"] for point in curve]
            require(all(type(value) is int for value in equities), "equity curve values invalid")
            require(all(point["equity_cents"] == point["economic_cash_cents"] + point["stock_value_cents"]
                        - point["option_liability_cents"] for point in curve), "daily equity accounting mismatch")
            require(equities[-1] == perf.get("ending_equity_cents"), "ending equity differs from curve")
            peak, drawdown = 8_000_000, 0
            for equity in equities:
                peak = max(peak, equity)
                drawdown = max(drawdown, (peak - equity) / peak * 100)
            require(abs(perf["return_pct"] - perf["net_pnl_cents"] / 8_000_000 * 100) < 1e-8,
                    "return differs from P&L")
            require(abs(perf["max_drawdown_pct"] - drawdown) < 1e-8, "drawdown differs from curve")
        except (TypeError, KeyError, ValueError):
            gaps.append("equity curve malformed")
    require(valid_inventory(report.get("end_state")), "ending inventory absent or invalid")
    return gaps


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report", type=Path, required=True)
    parser.add_argument("--qualification", type=Path, required=True)
    args = parser.parse_args()
    try:
        gaps = check(read_json(args.report), read_json(args.qualification), Path.cwd())
    except (OSError, ValueError, TypeError, AttributeError) as error:
        gaps = ["completion evidence unavailable: " + type(error).__name__]
    print(json.dumps({"complete": not gaps, "units_remaining": len(gaps), "gaps": gaps}, allow_nan=False))
    raise SystemExit(1 if gaps else 0)


if __name__ == "__main__":
    main()
