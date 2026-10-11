#!/usr/bin/env python3
"""Offline native/vector audit and provisional OI views for acquired partitions.

This consumes a fixed journal snapshot; it never reads credentials, submits,
finalizes, or alters the live financial writer. Licensed outputs stay private.
The output is evidence for normalization, not qualified replay input.
"""
import argparse
from collections import Counter
from datetime import datetime, time, timedelta, timezone
import gzip
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
from zoneinfo import ZoneInfo

from batch_acquisition_proof import canonical_query

ROOT = Path(__file__).resolve().parents[1]
DIRECTORY = ROOT / "research/simulation/2026-10-05"
MANIFEST_SHA = "ccf48c229ce29db62e0d4aaa20cb06c733fdf4e528cf2d79f4da0e71a00adb2b"
SPEC_SHA = "2ff4ae3765f4102a67382d645164ae4dfb6e036cc7ea3b113eab6dc24f4eac98"
REPLAY_SHA = "1e3a75597569b7bfdb2a169d93a58a6baeba85897c9c32ef0fb1b21848cb8023"
UNDEF_TS = (1 << 64) - 1
UNDEF_QTY = (1 << 31) - 1


def sha(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def save(path, value):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as output:
        json.dump(value, output, indent=2, allow_nan=False)
        output.write("\n")
        output.flush()
        os.fsync(output.fileno())


def status(row):
    if row[14] != 1 or row[15] != 0:
        return "unsupported_update_or_flags"
    if not 0 <= row[9] < UNDEF_QTY:
        return "unknown_quantity"
    if not 0 < row[6] <= row[5] < UNDEF_TS:
        return "unqualified_event_clock"
    if row[7] != UNDEF_TS and not 0 < row[7] <= row[5]:
        return "unqualified_reference_clock"
    return "observed_reference_missing" if row[7] == UNDEF_TS else "observed_reference_present"


def controls():
    row = [0, 1, "TSLA  260422C00255000", 30, 1, 100, 99, UNDEF_TS, 0, 0, 1, 0, 9, 0, 1, 0, 24]
    assert status(row) == "observed_reference_missing"  # A genuine zero stays zero.
    for index, value, expected in [(9, UNDEF_QTY, "unknown_quantity"),
                                   (6, 101, "unqualified_event_clock"),
                                   (7, 101, "unqualified_reference_clock"),
                                   (14, 2, "unsupported_update_or_flags"),
                                   (15, 1, "unsupported_update_or_flags")]:
        changed = row.copy()
        changed[index] = value
        assert status(changed) == expected


def project(audit_directory, query, definitions, cuts, sources):
    audit_path = audit_directory / "audit.json"
    audit = json.loads(audit_path.read_bytes())
    if audit["source_gate_passed"] is not False or len(audit["files"]) != 1:
        raise ValueError("one unqualified native statistics member required")
    member = audit["files"][0]
    vector = audit_directory / member["vector_file"]
    if sha(vector) != member["vector_sha256"] or member["capture_order_regressions"] != 0:
        raise ValueError("vector source/order differs")
    sources[str(audit_path)], sources[str(vector)] = sha(audit_path), member["vector_sha256"]
    requested, latest, counts = set(query["symbols"]), {}, Counter()
    query_order = sorted(requested)
    staged, final = audit_directory / "OI-views.stage.jsonl.gz", audit_directory / "OI-views.jsonl.gz"
    record_count, previous, cut_index, digest = 0, None, 0, hashlib.sha256()
    first_counts = None

    def emit(output, cut):
        nonlocal first_counts
        values, distribution = [], Counter()
        for raw in query_order:
            copies = latest.get(raw, {})
            definition = definitions.get(raw)
            state, chosen, quantity = "not_observed", None, None
            if copies:
                chosen = max(copies.values(), key=lambda row: (row[5], row[0]))
                states = {status(row) for row in copies.values()}
                if any(not value.startswith("observed_reference_") for value in states):
                    state = "unsupported_latest_copy"
                elif len({row[4] for row in copies.values()}) != 1 or len({row[9] for row in copies.values()}) != 1:
                    state = "publisher_copy_conflict"
                elif definition is None or definition[1] != chosen[4] or not definition[4] <= definition[3] <= cut:
                    state = "definition_join_unknown"
                else:
                    state, quantity = status(chosen), chosen[9]
            distribution[state] += 1
            # Quantities, identities and original clocks never leave this private file.
            values.append([raw, state, chosen[4] if chosen else None, quantity,
                           chosen[5] if chosen else None, chosen[6] if chosen else None,
                           None if chosen is None or chosen[7] == UNDEF_TS else chosen[7], len(copies)])
        output.write((json.dumps([cut, values], separators=(",", ":"), allow_nan=False) + "\n").encode())
        counts.update(distribution)
        if first_counts is None:
            first_counts = dict(distribution)

    with staged.open("xb") as stored:
        os.chmod(staged, 0o600)
        with gzip.GzipFile(filename="", fileobj=stored, mode="wb", mtime=0) as output:
            with gzip.open(vector, "rb") as source:
                for line in source:
                    if len(line) > 4096 or not line.endswith(b"\n"):
                        raise ValueError("vector line bound")
                    digest.update(line)
                    row = json.loads(line)
                    if len(row) != 17 or row[0] != record_count or row[2] not in requested or row[16] != 24:
                        raise ValueError("vector row identity differs")
                    if previous is not None and row[5] < previous:
                        raise ValueError("capture order regression")
                    while cut_index < len(cuts) and row[5] > cuts[cut_index]:
                        emit(output, cuts[cut_index])
                        cut_index += 1
                    if row[12] == 9:
                        latest.setdefault(row[2], {})[row[3]] = row
                    previous = row[5]
                    record_count += 1
                    if record_count > member["records"]:
                        raise ValueError("vector record count exceeded")
                while cut_index < len(cuts):
                    emit(output, cuts[cut_index])
                    cut_index += 1
        stored.flush()
        os.fsync(stored.fileno())
    if record_count != member["records"] or digest.hexdigest() != member["vector_jsonl_sha256"] or sha(vector) != member["vector_sha256"]:
        raise ValueError("native vector count/hash/EOF differs")
    staged.rename(final)
    sources[str(final)] = sha(final)
    return dict(records=record_count, cuts=len(cuts), queried_symbols=len(requested),
                status_counts=dict(counts), first_cut_status_counts=first_counts,
                private_views_path=str(final), private_views_sha256=sources[str(final)])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("base", "ledger-snapshot", "python", "output"):
        parser.add_argument("--" + name, type=Path, required=True)
    args = parser.parse_args()
    if args.output.exists() or args.base.resolve() != args.base:
        raise ValueError("canonical base and fresh output required")
    controls()
    qualification_path = DIRECTORY / "databento-qualification.json"
    qualification = json.loads(qualification_path.read_bytes())
    manifest_path = args.base / "cycle24-selective-acquisition-queries-20261006/manifest.json"
    spec_path, replay_path = DIRECTORY / "spec.jsonl", ROOT / "internal/simulation/replay.go"
    if sha(manifest_path) != MANIFEST_SHA or sha(spec_path) != SPEC_SHA or sha(replay_path) != REPLAY_SHA:
        raise ValueError("frozen full-window acquisition/decision scope differs")
    spec = json.loads(spec_path.read_text().splitlines()[0])
    if spec["strategy_config"]["poll_seconds"] != 300:
        raise ValueError("frozen decision cadence differs")
    ledger = json.loads(args.ledger_snapshot.read_bytes())
    if ledger["authorization_mode"] != "uncapped" or ledger["lifetime_nano"] != 0:
        raise ValueError("authorized snapshot required")
    acquired = {row["data_sha256"]:row for row in ledger["reservations"] if row["status"] == "acquired"}
    planned = {}
    sources = {str(p):sha(p) for p in (qualification_path, manifest_path, spec_path, replay_path,
                                     args.ledger_snapshot, Path(__file__), args.python,
                                     ROOT / "scripts/audit-native-statistics-batch.py",
                                     ROOT / "scripts/verify-native-statistics-vectors.py")}
    for item in json.loads(manifest_path.read_bytes())["queries"]:
        if item["schema"] != "statistics":
            continue
        path = manifest_path.parent / item["filename"]
        if sha(path) != item["sha256"]:
            raise ValueError("frozen query differs")
        query = json.loads(path.read_bytes())
        op = hashlib.sha256(canonical_query(query) + b"\0/batch.submit_job").hexdigest()
        planned[op] = (item, path, query)
        sources[str(path)] = item["sha256"]
    selected = {}
    for entry in qualification["source_files"]:
        if entry["sha256"] not in acquired:
            continue
        path = Path(entry["path"])
        path = path if path.is_absolute() else ROOT / path
        if sha(path) != entry["sha256"]:
            raise ValueError("acquired proof differs")
        proof = json.loads(path.read_bytes())
        if proof.get("schema_version") != "AutoWheelBatchAcquisitionProofV1" or proof["query"]["schema"] != "statistics":
            continue
        row = acquired[entry["sha256"]]
        op = row["operation_id"]
        if op not in planned or op in selected or proof["query"] != planned[op][2] or proof["native"]["all_eof_verified"] is not True:
            raise ValueError("acquired proof/query scope differs")
        selected[op] = (path, proof)
        sources[str(path)] = entry["sha256"]
    if len(selected) != 44:
        raise ValueError("immutable cycle40 acquired44 scope differs")
    identities = Path(qualification["source_progress"]["full_definition_identities"]["private_directory"])
    identity_audit_path = ROOT / qualification["source_progress"]["full_definition_identities"]["audit_path"]
    if sha(identity_audit_path) != qualification["source_progress"]["full_definition_identities"]["audit_sha256"]:
        raise ValueError("definition identity audit differs")
    official_parts = json.loads(identity_audit_path.read_bytes())["partitions"]
    private_identity_audit = identities / "audit.json"
    if json.loads(private_identity_audit.read_bytes())["partitions"] != official_parts:
        raise ValueError("private/reference definition partition binding differs")
    parts = {p["date"]:p for p in official_parts}
    sources[str(identity_audit_path)] = sha(identity_audit_path)
    sources[str(private_identity_audit)] = sha(private_identity_audit)
    args.output.mkdir(mode=0o700)
    results, total = [], Counter()
    for op, (proof_path, proof) in sorted(selected.items(), key=lambda x:planned[x[0]][0]["filename"]):
        item, query_path, query = planned[op]
        day = item["date"]
        out = args.output / item["filename"].removesuffix(".json")
        receipt = Path(proof["source_directory"]) / "receipt.json"
        if sha(receipt) != proof["receipt_sha256"]:
            raise ValueError("acquired receipt differs")
        commands = [[str(args.python), str(ROOT / "scripts/audit-native-statistics-batch.py"),
                     "--receipt", str(receipt), "--query", str(query_path), "--output-directory", str(out)],
                    [str(args.python), str(ROOT / "scripts/verify-native-statistics-vectors.py"),
                     "--source-directory", proof["source_directory"], "--audit-directory", str(out),
                     "--identity-directory", str(identities), "--cutoff", day + "T13:35:00Z",
                     "--output", str(out / "independent-reference.json")]]
        for index, command in enumerate(commands):
            with (args.output / (item["filename"] + ".pass" + str(index) + ".private.log")).open("xb") as log:
                os.chmod(log.name, 0o600)
                if subprocess.run(command, stdout=log, stderr=log).returncode != 0:
                    raise ValueError("native statistics pass failed; private evidence retained")
        part = parts[day]
        identity_path = identities / part["identity_file"]
        if sha(identity_path) != part["identity_sha256"]:
            raise ValueError("definition original differs")
        sources[str(identity_path)] = part["identity_sha256"]
        definitions, count, identity_hash = {}, 0, hashlib.sha256()
        requested_set = set(query["symbols"])
        with gzip.open(identity_path, "rb") as source:
            for line in source:
                identity_hash.update(line)
                row = json.loads(line)
                if row[2] in requested_set:
                    if row[2] in definitions:
                        raise ValueError("ambiguous captured definition")
                    definitions[row[2]] = row
                count += 1
        if count != part["records"] or identity_hash.hexdigest() != part["identity_jsonl_sha256"]:
            raise ValueError("definition decoded hash/count differs")
        local = datetime.combine(datetime.strptime(day, "%Y-%m-%d").date(), time(), ZoneInfo("America/New_York"))
        cuts = [int((local + timedelta(minutes=n)).timestamp()) * 1_000_000_000 for n in [*range(575,945,5),960]]
        if len(cuts) != 75 or datetime.fromtimestamp(cuts[0]//1_000_000_000,timezone.utc).strftime('%H:%M:%S') != '13:35:00':
            raise ValueError("frozen schedule/zone differs")
        projection = project(out, query, definitions, cuts, sources)
        ref_path = out / "independent-reference.json"
        reference = json.loads(ref_path.read_bytes())
        if reference["records_compared_field_for_field"] != projection["records"] or reference["vector_fields_per_record"] != 17:
            raise ValueError("independent native comparison differs")
        if (projection["queried_symbols"] - projection["first_cut_status_counts"].get("not_observed", 0)
                != reference["OI_symbols_at_cutoff"]):
            raise ValueError("independent first-cut OI subject count differs")
        sources[str(ref_path)] = sha(ref_path)
        results.append(dict(date=day, query_file_sha256=item["sha256"], **projection,
                            reference_path=str(ref_path), reference_sha256=sources[str(ref_path)]))
        total.update(projection["status_counts"])
        print(json.dumps(dict(completed_partitions=len(results), required_partitions=44,
                              native_records=sum(x["records"] for x in results), source_gate_passed=False)),flush=True)
    # Qualification may be checkpointed only after this bounded audit terminates.
    for path, digest in sources.items():
        if sha(Path(path)) != digest:
            raise ValueError("source changed during audit")
    sources.pop(str(qualification_path))  # Avoid a qualification-manifest self hash.
    report = dict(schema="AutoWheelAcquiredStatisticsScopeAuditV1", observed_at=datetime.now(timezone.utc).isoformat(),
                  acquired_partitions=44, full_plan_partitions=543, session_dates=len({x["date"] for x in results}),
                  native_records_compared=sum(x["records"] for x in results), fields_per_record=17,
                  contract_cut_observations=sum(x["queried_symbols"]*75 for x in results),status_counts=dict(total),
                  snapshot_cuts_per_partition=75, clock_quantity_update_controls_passed=6,
                  licensed_outputs_retained_private=True, missing_OI_defaulted=False, publisher_copies_summed=False,
                  source_gate_passed=False, full_study_coverage_proved=False, historical_performance_available=False,
                  financial_requests=0, results=results, source_files=[dict(path=p,sha256=s) for p,s in sorted(sources.items())])
    save(args.output / "summary.json", report)
    print(json.dumps({k:v for k,v in report.items() if k not in {"results","source_files","status_counts"}}),flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception:
        print("acquired statistics scope audit failed; private evidence retained",file=sys.stderr)
        raise SystemExit(1)
