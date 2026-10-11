#!/usr/bin/env python3
"""Offline snapshot audit of ALL acquired members of the frozen 543-query plan.

No credentials, live-journal mutation, financial call or trading. Cache reuse
is content-bound; a partial acquired snapshot never establishes study coverage.
"""
import argparse
from collections import Counter
from datetime import datetime, time, timedelta, timezone
import gzip
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
from zoneinfo import ZoneInfo

ROOT = Path(__file__).resolve().parents[1]
DIRECTORY = ROOT / "research/simulation/2026-10-05"
PINS = {
    "audit-acquired-statistics-scope.py": "01c8c3b9c9713227c64cb3b716dbc656d6e16ba245021d1e794cbdb6e341c206",
    "compare-native-oi-scope.py": "b98a103ea986b262972bb1691df000097793f543e4a843ebd1b1ca5d7b4de04e",
    "audit-native-statistics-batch.py": "eddad4fae1deac4ac481ebe2ada53409945bccd320e2efd402918237695391c2",
    "verify-native-statistics-vectors.py": "a27d6394c422f3bdc56130542074123ddec3ecc912f27a647d6ed99464c6695c",
    "batch_acquisition_proof.py": "380f6fd3a0e3b6ebee08117ee8559835029844ecfcab9ee9327f61d4a094c38e",
}


def sha(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def load_module(name):
    path = ROOT / "scripts" / name
    if path.is_symlink() or sha(path) != PINS[name]:
        raise ValueError("pinned offline helper changed")
    spec = importlib.util.spec_from_file_location(name.removesuffix(".py").replace("-", "_"), path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def read_scope(path):
    # The cumulative qualification manifest outgrew the financial-proof reader's
    # 4 MiB limit. Give only this aggregate metadata a separate explicit bound.
    def unique(pairs):
        out = {}
        for key, value in pairs:
            if key in out:
                raise ValueError("duplicate scope metadata field")
            out[key] = value
        return out
    with path.open("rb") as source:
        raw = source.read((32 << 20) + 1)
    if not raw or len(raw) > 32 << 20:
        raise ValueError("scope metadata size exceeded")
    return json.loads(raw, object_pairs_hook=unique,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite scope metadata")))


def coverage(planned, acquired, audited):
    if not audited <= acquired <= set(planned):
        raise ValueError("coverage set outside frozen plan")
    dates = sorted({item[0]["date"] for item in planned.values()})
    parts = []
    for day in dates:
        expected = {op for op, value in planned.items() if value[0]["date"] == day}
        parts.append(dict(date=day, expected=len(expected), acquired=len(expected & acquired),
                          audited=len(expected & audited), complete=expected <= audited))
    missing = sorted(planned[op][0]["filename"] for op in set(planned) - audited)
    return dict(required_partitions=len(planned), acquired_partitions=len(acquired),
                audited_partitions=len(audited), expected_session_dates=len(dates),
                complete_session_dates=sum(p["complete"] for p in parts), per_date=parts,
                missing_query_files=missing, full_plan_audited=not missing)


def select(planned, ledger, candidates, read):
    rows = ledger["reservations"]
    if len({r["operation_id"] for r in rows}) != len(rows):
        raise ValueError("duplicate journal operation")
    eligible = [r for r in rows if r["status"] == "acquired" and r["operation_id"] in planned]
    if len({r["data_sha256"] for r in eligible}) != len(eligible):
        raise ValueError("one acquired proof cannot represent multiple operations")
    acquired = {r["data_sha256"]:r for r in eligible}
    selected = {}
    seen = set()
    for path in candidates:
        path = path.absolute()
        if path in seen:
            continue
        seen.add(path)
        if path.is_symlink() or not path.is_file() or path.resolve() != path:
            raise ValueError("unsafe proof candidate")
        digest = sha(path)
        if digest not in acquired:
            continue
        row = acquired[digest]
        op = row["operation_id"]
        proof = read(path)
        if (proof.get("schema_version") != "AutoWheelBatchAcquisitionProofV1" or
                proof["query"] != planned[op][2] or proof["query_file_sha256"] != planned[op][0]["sha256"] or
                proof["native"]["all_eof_verified"] is not True):
            raise ValueError("acquired proof/query differs")
        if op in selected and selected[op][3] != digest:
            raise ValueError("conflicting proof for acquired operation")
        selected.setdefault(op, (row, path, proof, digest))
    wanted = {r["operation_id"] for r in eligible}
    if set(selected) != wanted or not wanted:
        raise ValueError("snapshot-acquired planned proof coverage incomplete")
    return selected


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("base", "ledger-snapshot", "python", "binary", "reuse-native-summary", "reuse-go-summary", "output"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--reuse-native-sha256", required=True)
    parser.add_argument("--reuse-go-sha256", required=True)
    args = parser.parse_args()
    if args.output.exists() or any(p.resolve() != p for p in (args.base, args.ledger_snapshot, args.binary,
                                                            args.reuse_native_summary, args.reuse_go_summary, args.output)):
        raise ValueError("canonical inputs and fresh output required")
    if args.python != args.base / "dbn-env-0.71.0/bin/python":
        raise ValueError("pinned native decoder environment required")
    proof_module = load_module("batch_acquisition_proof.py")
    reference = load_module("audit-acquired-statistics-scope.py")
    comparison = load_module("compare-native-oi-scope.py")
    read = proof_module.read_json
    sources = {}

    def bind(path, expected=None):
        path = Path(path)
        path = path if path.is_absolute() else ROOT / path
        # The pinned venv interpreter is intentionally a symlink. Its exact
        # name and bytes must match the accepted native audit; data stays strict.
        if not path.is_file() or path != args.python and (path.is_symlink() or path.resolve() != path):
            raise ValueError("unsafe source")
        digest = sha(path)
        if expected is not None and digest != expected or str(path) in sources and sources[str(path)] != digest:
            raise ValueError("input hash changed")
        sources[str(path)] = digest
        return digest

    qualification_path = DIRECTORY / "databento-qualification.json"
    qualification = read_scope(qualification_path)
    bind(qualification_path)
    known = {entry["path"]:entry for entry in qualification["source_files"]}
    manifest_path = args.base / "cycle24-selective-acquisition-queries-20261006/manifest.json"
    bind(manifest_path, reference.MANIFEST_SHA)
    bind(DIRECTORY / "spec.jsonl", reference.SPEC_SHA)
    bind(ROOT / "internal/simulation/replay.go", reference.REPLAY_SHA)
    if json.loads((DIRECTORY / "spec.jsonl").read_text().splitlines()[0])["strategy_config"]["poll_seconds"] != 300:
        raise ValueError("frozen decision cadence differs")
    planned = {}
    for item in read(manifest_path)["queries"]:
        if item["schema"] != "statistics":
            continue
        path = manifest_path.parent / item["filename"]
        bind(path, item["sha256"])
        query = read(path)
        wire = proof_module.canonical_query(query)
        op = hashlib.sha256(wire + b"\0/batch.submit_job").hexdigest()
        if op in planned:
            raise ValueError("duplicate frozen query")
        planned[op] = (item, path, query)
    if len(planned) != 543 or len({x[0]["date"] for x in planned.values()}) != 126:
        raise ValueError("frozen full-plan scope differs")
    ledger = read(args.ledger_snapshot)
    bind(args.ledger_snapshot)
    if (type(ledger["version"]) is not int or ledger["version"] != 2 or ledger["authorization_mode"] != "uncapped" or
            type(ledger["lifetime_nano"]) is not int or ledger["lifetime_nano"] != 0):
        raise ValueError("authorized immutable snapshot required")
    acquired_hashes = {r.get("data_sha256") for r in ledger["reservations"] if r["status"] == "acquired"}
    candidates = []
    for entry in qualification["source_files"]:
        if entry["sha256"] in acquired_hashes:
            p = Path(entry["path"])
            candidates.append(p if p.is_absolute() else ROOT / p)
    candidates.extend((args.base / "cycle38-statistics-prefetch-final-20261007").glob("*/proof.json"))
    candidates.extend((args.base / "cycle46-prepaid-statistics-recovery-20261007").glob("*/proof.json"))
    candidates.extend((args.base / "cycle46-statistics-prefetch-20261007").glob("*/proof.json"))
    selected = select(planned, ledger, candidates, read)

    def validate_original(row, path, proof, digest):
        entries = {str(path):dict(sha256=bind(path, digest))}

        def add(name, expected=None):
            p = Path(name)
            if expected is None:
                if name not in known:
                    raise ValueError("unbound financial verifier source")
                expected = known[name]["sha256"]
            entries[name] = dict(sha256=bind(p, expected))

        for code in proof_module.CODE_FILES:
            add(code)
        add(proof_module.native_verifier_path(proof), proof["native_verifier_sha256"])
        for name, field in (("query_path", "query_file_sha256"), ("acknowledgement_path", "acknowledgement_file_sha256"),
                            ("ack_body_path", "ack_body_sha256")):
            add(proof[name], proof[field])
        directory = Path(proof["source_directory"])
        for name, field in (("receipt.json", "receipt_sha256"), ("job-source.json", "job_sha256"),
                            ("files-source.json", "file_list_sha256")):
            add(str(directory / name), proof[field])
        for member in proof["files"]:
            add(str(directory / member["filename"]), member["sha256"])
        passed, _ = proof_module.validate_batch_acquisition(row, str(path), str(directory / "receipt.json"),
                                                           entries, set(entries), ROOT)
        if not passed:
            raise ValueError("original acquisition proof failed independent binding")

    for row, path, proof, digest in selected.values():
        validate_original(row, path, proof, digest)
    native_cache = read(args.reuse_native_summary)
    go_cache = read(args.reuse_go_summary)
    bind(args.reuse_native_summary, args.reuse_native_sha256)
    bind(args.reuse_go_summary, args.reuse_go_sha256)
    if (native_cache["schema"] != "AutoWheelAcquiredStatisticsScopeAuditV1" or go_cache["schema"] != "AutoWheelNativeOIEquivalenceV1" or
            native_cache["acquired_partitions"] != 44 or go_cache["acquired_partitions"] != 44 or
            go_cache["go_python_views_equal"] is not True):
        raise ValueError("pinned cache scope differs")
    cache_hashes = set()
    for cache in (native_cache, go_cache):
        for key in ("source_gate_passed", "full_study_coverage_proved", "historical_performance_available"):
            if cache[key] is not False:
                raise ValueError("cache improperly claims qualification")
        for item in cache["source_files"]:
            bind(item["path"], item["sha256"])
            cache_hashes.add(item["sha256"])
    native_inputs = {item["path"]:item["sha256"] for item in native_cache["source_files"]}
    go_inputs = {item["path"]:item["sha256"] for item in go_cache["source_files"]}
    if str(args.python) not in native_inputs or str(args.binary) not in go_inputs:
        raise ValueError("decoder/exporter not the previously validated executable")
    bind(args.python, native_inputs[str(args.python)])
    bind(args.binary, go_inputs[str(args.binary)])
    for path in (args.binary, args.python, Path(__file__), ROOT / "internal/causaloi/oi.go",
                 ROOT / "cmd/autowheel-native-oi/main.go", *(ROOT / "scripts" / name for name in PINS)):
        bind(path)
    old_native = {r["query_file_sha256"]:r for r in native_cache["results"]}
    old_go = {r["query_sha256"]:r for r in go_cache["results"]}
    if len(old_native) != 44 or len(old_go) != 44 or set(old_native) != set(old_go):
        raise ValueError("duplicate/mismatched cached queries")
    identities = args.base / "full-definition-identities-20261006"
    identity_audit_path = ROOT / qualification["source_progress"]["full_definition_identities"]["audit_path"]
    bind(identity_audit_path, qualification["source_progress"]["full_definition_identities"]["audit_sha256"])
    official = read(identity_audit_path)["partitions"]
    bind(identities / "audit.json")
    if read(identities / "audit.json")["partitions"] != official:
        raise ValueError("definition partition binding differs")
    parts = {p["date"]:p for p in official}
    args.output.mkdir(mode=0o700)
    results, totals, definitions_cache, audited = [], Counter(), {}, set()
    for op, (row, proof_path, proof, proof_sha) in sorted(selected.items(), key=lambda x:planned[x[0]][0]["filename"]):
        item, query_path, query = planned[op]
        day = item["date"]
        local = datetime.combine(datetime.strptime(day, "%Y-%m-%d").date(), time(), ZoneInfo("America/New_York"))
        cuts = [int((local + timedelta(minutes=n)).timestamp()) * 1_000_000_000 for n in [*range(575,945,5),960]]
        if len(cuts) != 75:
            raise ValueError("frozen closed-cut schedule differs")
        if item["sha256"] in old_native:
            projection, go_entry = old_native[item["sha256"]], old_go[item["sha256"]]
            if proof_sha not in cache_hashes or projection["records"] != proof["native"]["records"] or go_entry["records"] != projection["records"]:
                raise ValueError("cached original proof/count binding differs")
            private = Path(projection["private_views_path"]).parent
            audit = read(private / "audit.json")
            if audit["receipt_sha256"] != proof["receipt_sha256"] or audit["query_file_sha256"] != item["sha256"]:
                raise ValueError("cached receipt/query binding differs")
            bind(private / "audit.json")
            bind(projection["private_views_path"], projection["private_views_sha256"])
            compared = comparison.compare(Path(projection["private_views_path"]), Path(go_entry["private_go_directory"])/"OI-views.jsonl.gz",
                                          cuts, sorted(query["symbols"]), sources)
            if compared["status_counts"] != projection["status_counts"] or compared["observations"] != go_entry["observations"]:
                raise ValueError("cached complete-view comparison differs")
            reused = True
        else:
            native = args.output / (item["filename"].removesuffix(".json") + ".native")
            receipt = Path(proof["source_directory"]) / "receipt.json"
            commands = [[str(args.python), str(ROOT/"scripts/audit-native-statistics-batch.py"), "--receipt", str(receipt),
                         "--query", str(query_path), "--output-directory", str(native)],
                        [str(args.python), str(ROOT/"scripts/verify-native-statistics-vectors.py"), "--source-directory", proof["source_directory"],
                         "--audit-directory", str(native), "--identity-directory", str(identities), "--cutoff", day+"T13:35:00Z",
                         "--output", str(native/"independent-reference.json")]]
            for index, command in enumerate(commands):
                with (args.output/(item["filename"]+".pass"+str(index)+".private.log")).open("xb") as log:
                    os.chmod(log.name, 0o600)
                    if subprocess.run(command, stdout=log, stderr=log).returncode != 0:
                        raise ValueError("native independent pass failed; evidence retained privately")
            if day not in definitions_cache:
                part = parts[day]
                path = identities / part["identity_file"]
                bind(path, part["identity_sha256"])
                definitions, count, digest = {}, 0, hashlib.sha256()
                with gzip.open(path, "rb") as source:
                    for line in source:
                        if len(line) > 4096 or not line.endswith(b"\n"):
                            raise ValueError("definition line bound")
                        digest.update(line)
                        value = json.loads(line)
                        if len(value) != 9 or value[2] in definitions:
                            raise ValueError("definition identity ambiguous")
                        definitions[value[2]] = value
                        count += 1
                if count != part["records"] or digest.hexdigest() != part["identity_jsonl_sha256"]:
                    raise ValueError("definition decoded count/hash/EOF differs")
                definitions_cache[day] = definitions
            definitions = definitions_cache[day]
            projection = reference.project(native, query, definitions, cuts, sources)
            ref = native / "independent-reference.json"
            bind(ref)
            independent = read(ref)
            if independent["records_compared_field_for_field"] != projection["records"] or independent["vector_fields_per_record"] != 17 or (
                    projection["queried_symbols"]-projection["first_cut_status_counts"].get("not_observed",0) != independent["OI_symbols_at_cutoff"]):
                raise ValueError("independent first-cut/native comparison differs")
            member = read(native/"audit.json")["files"][0]
            start = int(datetime.strptime(day,"%Y-%m-%d").replace(tzinfo=timezone.utc).timestamp())*1_000_000_000
            bindings = []
            for raw in sorted(query["symbols"]):
                definition = definitions.get(raw)
                bindings.append(dict(RawSymbol=raw, InstrumentID=0, Definition=None if definition is None else
                                     dict(InstrumentID=definition[1], ReceiveAt=definition[3], EventAt=definition[4])))
            config = dict(schema="AutoWheelNativeOIScopeV1", dataset="OPRA.PILLAR", utc_start=start, native_version=member["native_version"],
                          expected_records=member["records"], vector_sha256=member["vector_sha256"], vector_jsonl_sha256=member["vector_jsonl_sha256"],
                          bindings=bindings, cuts=cuts)
            config_path = args.output/(item["filename"]+".scope.json")
            reference.save(config_path,config)
            bind(config_path)
            go_out = args.output/(item["filename"].removesuffix(".json")+".go")
            with (args.output/(item["filename"]+".go.private.log")).open("xb") as log:
                os.chmod(log.name,0o600)
                if subprocess.run([str(args.binary),"--config",str(config_path),"--vectors",str(native/member["vector_file"]),
                                   "--output-directory",str(go_out)],stdout=log,stderr=log).returncode != 0:
                    raise ValueError("Go export failed; evidence retained privately")
            bind(go_out/"report.json")
            go = read(go_out/"report.json")
            compared = comparison.compare(Path(projection["private_views_path"]),go_out/"OI-views.jsonl.gz",cuts,sorted(query["symbols"]),sources)
            if (go["summary"] != dict(Records=member["records"], OpenInterestRecords=member["counts_by_stat_type"]["9"],Snapshots=75,Observations=len(bindings)*75) or
                    go["status_counts"] != projection["status_counts"] or compared["status_counts"] != projection["status_counts"] or
                    go["views_jsonl_sha256"] != compared["go_decoded_sha256"] or go["views_sha256"] != sources[str(go_out/"OI-views.jsonl.gz")] or
                    go["config_sha256"] != sources[str(config_path)] or go["source_gate_passed"] is not False or go["historical_performance_available"] is not False or
                    go["financial_requests"] != 0):
                raise ValueError("Go independent view/source policy differs")
            reused = False
        results.append(dict(date=day, query_file=item["filename"], query_sha256=item["sha256"], proof_sha256=proof_sha,
                            records=projection["records"], native_cache_reused=reused, **compared))
        totals.update(compared["status_counts"])
        audited.add(op)
        print(json.dumps(dict(audited_partitions=len(audited), snapshot_acquired_partitions=len(selected), full_plan_partitions=543,
                              observations_compared=sum(x["observations"] for x in results))),flush=True)
    covered = coverage(planned,set(selected),audited)
    if covered["audited_partitions"] != len(selected) or covered["full_plan_audited"] != (len(selected)==543):
        raise ValueError("snapshot/full-plan coverage differs")
    for path,digest in sources.items():
        if sha(Path(path)) != digest:
            raise ValueError("source changed during audit")
    sources.pop(str(qualification_path))
    report = dict(schema="AutoWheelAllAcquiredStatisticsOIComponentV1",observed_at=datetime.now(timezone.utc).isoformat(),
                  coverage=covered,native_records_compared=sum(r["records"] for r in results), fields_per_record=17,
                  contract_cut_observations_compared=sum(r["observations"] for r in results), closed_cuts_per_partition=75,
                  native_cache_partitions_reused=sum(r["native_cache_reused"] for r in results),new_native_partitions_audited=sum(not r["native_cache_reused"] for r in results),
                  go_python_views_equal=True,status_counts=dict(totals),financial_requests=0,live_journal_modified=False,
                  source_gate_passed=False,full_study_coverage_proved=False,historical_performance_available=False,
                  licensed_outputs_retained_private=True,results=results,source_files=[dict(path=p,sha256=s) for p,s in sorted(sources.items())])
    reference.save(args.output/"summary.json",report)
    print(json.dumps({k:v for k,v in report.items() if k not in {"results","source_files","status_counts","coverage"}},allow_nan=False),flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception:
        print("all-acquired statistics/OI audit failed; private evidence retained",file=sys.stderr)
        raise SystemExit(1)
