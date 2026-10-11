#!/usr/bin/env python3
"""Inspect the complete first-day definition classification fields privately.

Tests an alternative source lead without assuming that a populated field proves
standard terms. Emits aggregate field-presence counts, never individual rows.
"""
from collections import Counter
from datetime import datetime, timezone
import argparse
import hashlib
import importlib.metadata
import importlib.util
import json
from pathlib import Path
import subprocess
import sys

from batch_acquisition_proof import file_hash, read_json

ROOT = Path(__file__).resolve().parents[1]
BASE = Path("/home/yuduo/Projects/AutoWheel-research")
FIELDS = ("cfi", "security_type", "unit_of_measure", "currency", "settl_currency", "underlying")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-directory", type=Path, required=True)
    parser.add_argument("--output-directory", type=Path, required=True)
    args = parser.parse_args()
    if importlib.metadata.version("databento-dbn") != "0.71.0":
        raise ValueError("pinned decoder required")
    import databento_dbn as dbn
    helper_path = ROOT/"scripts/verify-native-execution-intervals.py"
    spec = importlib.util.spec_from_file_location("classification_private_input", helper_path)
    helper = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(helper)
    if args.source_directory != BASE/"full-all15-definitions-verified-20261006" or args.output_directory.parent != BASE or args.output_directory.exists():
        raise ValueError("fixed acquired source and fresh private output required")
    qualification_path = ROOT/"research/simulation/2026-10-05/databento-qualification.json"
    census_path = ROOT/"research/simulation/2026-10-05/cycle22-definition-batch-census.json"
    qualification, census = read_json(qualification_path), read_json(census_path)
    expected = {row["path"]:row["sha256"] for row in qualification["source_files"]}
    if file_hash(census_path) != expected[str(census_path.relative_to(ROOT))] or census["decoder_version"] != "0.71.0" or census["upgrade_policy"] != "AS_IS":
        raise ValueError("completed native census binding differs")
    members = [row for row in census["files"] if row["filename"] == "opra-pillar-20260406.definition.dbn.zst"]
    if len(members) != 1:
        raise ValueError("single first-day source required")
    member = members[0]
    source = args.source_directory/member["filename"]
    if file_hash(source) != expected[str(source)] or file_hash(source) != member["compressed_sha256"] or source.stat().st_size != member["compressed_bytes"]:
        raise ValueError("original first-day definition bytes differ")
    relevant = [Path(__file__).resolve(), helper_path, census_path, qualification_path, source]
    before = {path:file_hash(path) for path in relevant}
    native_hash, native_bytes, records, metadata_count = hashlib.sha256(), 0, 0, 0
    counts = {field:Counter(absent=0, empty=0, populated_string=0, non_string=0) for field in FIELDS}
    decoder = dbn.DBNDecoder(upgrade_policy=dbn.VersionUpgradePolicy.AS_IS, compression=dbn.Compression.NONE)
    process = None
    try:
        with helper.private_input(source) as original:
            if hashlib.file_digest(original, "sha256").hexdigest() != before[source]:
                raise ValueError("source descriptor differs")
            original.seek(0)
            process = subprocess.Popen(["/usr/bin/zstd", "-q", "-f", "-d", "-c", "--", "/dev/fd/"+str(original.fileno())],
                                       pass_fds=(original.fileno(),), stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
            while raw := process.stdout.read(128<<10):
                native_hash.update(raw)
                native_bytes += len(raw)
                if native_bytes > member["uncompressed_bytes"]:
                    raise ValueError("native expansion exceeded")
                for row in decoder.write_and_decode(raw):
                    if isinstance(row, dbn.Metadata):
                        metadata_count += 1
                        if (row.dataset != "OPRA.PILLAR" or str(row.schema) != "definition"
                                or row.version != member["metadata"]["version"]
                                or row.start != member["metadata"]["start"] or row.end != member["metadata"]["end"]
                                or row.limit not in (None, 0) or list(row.symbols) != member["metadata"]["symbols"]):
                            raise ValueError("first-day metadata scope differs")
                        continue
                    if type(row).__name__ != "InstrumentDefMsgV1":
                        raise ValueError("unproved native record class")
                    records += 1
                    for field in FIELDS:
                        if not hasattr(row, field):
                            bucket = "absent"
                        else:
                            value = getattr(row, field)
                            bucket = ("empty" if not value.strip() else "populated_string") if isinstance(value, str) else "non_string"
                        counts[field][bucket] += 1
                if len(decoder.buffer()) > 16<<20:
                    raise ValueError("decoder remainder bound")
            if (process.wait() != 0 or decoder.buffer() or metadata_count != 1 or records != 50202
                    or member["record_types"] != {"InstrumentDefMsgV1": records}
                    or native_bytes != member["uncompressed_bytes"] or native_hash.hexdigest() != member["uncompressed_sha256"]):
                raise ValueError("complete native identity/count/EOF differs")
        if any(file_hash(path) != value for path,value in before.items()):
            raise ValueError("code, census or source changed")
        result = dict(schema="AutoWheelFirstDayDefinitionClassificationPresenceV1", observed_at=datetime.now(timezone.utc).isoformat(),
                      script_sha256=before[Path(__file__).resolve()], census_sha256=before[census_path],
                      compressed_source_sha256=before[source], native_source_sha256=native_hash.hexdigest(),
                      native_bytes=native_bytes, records_inspected=records, field_presence={field:dict(value) for field,value in counts.items()},
                      complete_first_day_native_pass=True, all_126_sessions_inspected=False,
                      classification_semantics_qualified=False, per_series_terms_qualified=False,
                      replay_frames_produced=False, full_source_qualification=False, historical_performance_available=False,
                      provider_requests=0, financial_requests=0,
                      limitations=["First-day field-presence audit only; no ISO classification semantics or per-series terms inferred.",
                                   "Empty/absent fields cannot establish standard deliverables, currency, tick, activation, weekly eligibility or tradability."])
        args.output_directory.mkdir(mode=0o700)
        output = args.output_directory/"report.json"
        import os
        fd = os.open(output, os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "w") as file:
            json.dump(result, file, indent=2)
            file.write("\n")
            file.flush()
            os.fsync(file.fileno())
        print(json.dumps(result))
    finally:
        if process is not None:
            if process.poll() is None:
                process.kill()
                process.wait()
            process.stdout.close()


if __name__ == "__main__":
    try:
        main()
    except Exception:
        print("definition classification audit failed closed", file=sys.stderr)
        raise SystemExit(1)
