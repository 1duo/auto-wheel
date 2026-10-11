"""Offline decision BOOK plan, retaining all existing financial identities.

This plan covers closed decision/valuation captures only. It is not the full
pending-order, inventory or lifecycle acquisition plan and cannot qualify a
source gate. No provider transport or financial submission is implemented.
"""
import argparse
from collections import Counter
from datetime import datetime, timezone
import gzip
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import stat

from batch_acquisition_proof import canonical_query, read_json, utc_ns
from research_history_reader import read_financial_journal, read_qualification_manifest

MANIFEST_SHA = 'ccf48c229ce29db62e0d4aaa20cb06c733fdf4e528cf2d79f4da0e71a00adb2b'
CALENDAR_SHA = 'b472c8203ab3081468beec7d9d810f6e51483af07a3024e948bbf412741255a9'
PROTECTED = '01593d27c3a667a42b8736bba57c377e98dd6f19949f4601e2c08ef505abbf5f'
PILOT = 'fef32e79bfd60d0db6d8d91cc57640f82df501ea353c75be8b40a3c866d24431'
NANO = 1_000_000_000
WINDOW_COLUMNS = ['partition_index', 'cut_index', 'cut_ns', 'start_ns', 'end_ns',
                  'query_hash', 'operation_id', 'disposition', 'reuse_operation_id']
FIXED = dict(encoding='dbn', compression='zstd', stype_out='instrument_id', split_duration='day',
             split_symbols=False, pretty_px=False, pretty_ts=False, map_symbols=False, delivery='download')


def sha(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def encode(value):
    return json.dumps(value, separators=(',', ':'), ensure_ascii=False, allow_nan=False).encode()


def timestamp(ns):
    if type(ns) is not int or ns < 0:
        raise ValueError('exact nanosecond timestamp required')
    seconds, fraction = divmod(ns, NANO)
    stamp = datetime.fromtimestamp(seconds, timezone.utc).strftime('%Y-%m-%dT%H:%M:%S')
    return stamp + ('.' + f'{fraction:09d}'.rstrip('0') if fraction else '') + 'Z'


def wire_renderer(query):
    """Validate each original roster once; cache only its fixed wire prefix."""
    original = canonical_query(query)
    prefix = (b'{"dataset":' + encode(query['dataset']) + b',"schema":' + encode(query['schema'])
              + b',"symbols":' + encode(query['symbols']) + b',"stype_in":' + encode(query['stype_in']) + b',"start":')
    suffix = encode(FIXED)[1:]

    def render(start, end):
        return prefix + encode(start) + b',"end":' + encode(end) + b',' + suffix

    if render(query['start'], query['end']) != original:
        raise ValueError('cached canonical query does not match existing source helper')
    return render, original


def window_identity(render, cut):
    start, end = cut - 30 * NANO, cut + 1
    wire = render(timestamp(start), timestamp(end))
    return start, end, hashlib.sha256(wire).hexdigest(), hashlib.sha256(wire + b'\0/batch.submit_job').hexdigest()


def save(path, raw):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'wb') as stream:
        stream.write(raw)
        stream.flush()
        os.fsync(stream.fileno())
    fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('base', 'source-root', 'ledger-snapshot', 'qualification', 'output'):
        parser.add_argument('--' + name, type=Path, required=True)
    args = parser.parse_args()
    for path in vars(args).values():
        if not path.is_absolute() or path != Path(os.path.normpath(path)) or path.resolve() != path:
            raise ValueError('clean absolute plan paths required')
    for path in (args.base, args.source_root, args.output.parent):
        info = path.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700:
            raise ValueError('private owned planning directory required')
    journal = read_financial_journal(args.ledger_snapshot)
    if (journal.value['version'], journal.value['lifetime_nano'], journal.value.get('authorization_mode')) != (2, 0, 'uncapped'):
        raise ValueError('original uncapped journal required')
    rows = {v['operation_id']: v for v in journal.value['reservations']}
    if len(rows) != 708 or rows[PROTECTED]['status'] != 'reserved' or rows[PILOT]['status'] != 'acquired':
        raise ValueError('exact retained original708 financial identities required')
    qualification = read_qualification_manifest(args.qualification)
    if len(qualification.value['gates']) != 9 or any(v['status'] != 'pending' for v in qualification.value['gates'].values()):
        raise ValueError('plan cannot change source gates')
    known = {v['path']: v['sha256'] for v in qualification.value['source_files']}
    manifest_path = args.base / 'cycle24-selective-acquisition-queries-20261006/manifest.json'
    if sha(manifest_path) != MANIFEST_SHA:
        raise ValueError('original full-day plan binding differs')
    manifest = read_json(manifest_path)
    books = [v for v in manifest['queries'] if v['schema'] == 'cmbp-1']
    if manifest['daily_partitions'] != 126 or len(books) != 543:
        raise ValueError('full study book partition count differs')
    calendar_path = args.source_root / 'scripts/frozen-study-stock-cuts.py'
    if sha(calendar_path) != CALENDAR_SHA:
        raise ValueError('frozen calendar binding differs')
    spec = importlib.util.spec_from_file_location('frozen_window_calendar', calendar_path)
    calendar = importlib.util.module_from_spec(spec);spec.loader.exec_module(calendar)
    sessions, bindings = calendar.scope(args.source_root)
    cuts_by_day = {v.isoformat(): calendar.make(v.isoformat(), args.source_root)['cuts_ns'] for v in sessions}
    if set(cuts_by_day) != {v['date'] for v in books} or any(len(v) != 75 for v in cuts_by_day.values()):
        raise ValueError('complete126-session75-cut scope required')
    bindings += [dict(path=str(manifest_path), sha256=MANIFEST_SHA),
                 dict(path=str(args.ledger_snapshot), sha256=journal.sha256)]
    for name in ('scripts/plan-frozen-decision-windows.py', 'scripts/research_history_reader.py',
                 'scripts/batch_acquisition_proof.py', 'scripts/frozen-study-option-cuts.py'):
        bindings.append(dict(path=name, sha256=sha(args.source_root / name)))
    proof_cache = {}

    def reuse_proof(entry):
        op = entry['operation_id']
        if op in proof_cache:
            return proof_cache[op]
        if entry['status'] != 'acquired':
            raise ValueError('reuse requires acquired source')
        choices = [v['path'] for v in qualification.value['source_files'] if v['sha256'] == entry['data_sha256']]
        choices.sort(key=lambda v: (not Path(v).is_absolute(), Path(v).name != 'proof.json', len(v), v))
        if not choices:
            raise ValueError('acquired proof absent from source manifest')
        name = choices[0];path = Path(name);path = path if path.is_absolute() else args.source_root / path
        if path.is_symlink() or not path.is_file() or sha(path) != entry['data_sha256']:
            raise ValueError('acquired proof digest differs')
        proof = read_json(path)
        original = dict(entry);original.update(status='reserved');original.pop('data_sha256');original.pop('bytes')
        wire = canonical_query(proof['query'])
        if (proof['reservation'] != original or hashlib.sha256(wire).hexdigest() != entry['query_hash']
                or hashlib.sha256(wire + b'\0/batch.submit_job').hexdigest() != op
                or proof['source_qualified'] is not False or proof['invoice_cost_verified'] is not False
                or proof['total_bytes'] != entry['bytes'] or not proof['native']['all_eof_verified']
                or proof['native']['records'] != proof['provider_record_count'] or proof['provider_record_count'] <= 0):
            raise ValueError('acquired original envelope or identity differs')
        required = [(proof['query_path'], proof['query_file_sha256']),
                    (proof['acknowledgement_path'], proof['acknowledgement_file_sha256']),
                    (proof['ack_body_path'], proof['ack_body_sha256'])]
        directory = Path(proof['source_directory'])
        required += [(str(directory / filename), proof[key]) for filename, key in (
            ('receipt.json', 'receipt_sha256'), ('job-source.json', 'job_sha256'), ('files-source.json', 'file_list_sha256'))]
        required += [(str(directory / v['filename']), v['sha256']) for v in proof['files']]
        for source, digest in required:
            alternative = str(Path(source).relative_to(args.source_root)) if Path(source).is_relative_to(args.source_root) else source
            if known.get(source, known.get(alternative)) != digest:
                raise ValueError('retained acquired original source binding missing')
        result = dict(operation_id=op, query_hash=entry['query_hash'], proof_path=name, proof_sha256=entry['data_sha256'],
                      query=proof['query'], native_records=proof['provider_record_count'], total_bytes=proof['total_bytes'],
                      original_source_bindings=dict(required), prior_acquisition_evidence_reused=True,
                      full_native_sources_rehashed_this_plan=False, full_source_qualification=False)
        proof_cache[op] = result
        return result

    partitions, windows, seen = [], [], set()
    for index, item in enumerate(books):
        path = manifest_path.parent / item['filename']
        if path.parent != manifest_path.parent or path.is_symlink() or sha(path) != item['sha256'] or path.stat().st_size != item['bytes']:
            raise ValueError('original roster partition differs')
        query = read_json(path)
        if query['schema'] != 'cmbp-1' or query['start'] != item['date'] + 'T00:00:00Z' or len(query['symbols']) != item['symbols']:
            raise ValueError('original partition query scope differs')
        render, wire = wire_renderer(query)
        full_op = hashlib.sha256(wire + b'\0/batch.submit_job').hexdigest()
        full_hash = hashlib.sha256(wire).hexdigest()
        full_entry = rows.get(full_op)
        if full_entry is not None and full_entry['query_hash'] != full_hash:
            raise ValueError('full-day financial identity differs')
        if full_entry is not None and full_entry['status'] == 'acquired':
            reuse_proof(full_entry)
        partitions.append(dict(index=index, date=item['date'], query_path=str(path), query_sha256=item['sha256'],
                               symbols=item['symbols'], full_query_hash=full_hash, full_operation_id=full_op,
                               full_day_existing_status=full_entry['status'] if full_entry else None))
        bindings.append(dict(path=str(path), sha256=item['sha256']))
        for cut_index, cut in enumerate(cuts_by_day[item['date']]):
            start, end, query_hash, operation = window_identity(render, cut)
            if operation in seen:
                raise ValueError('duplicate planned financial operation')
            seen.add(operation)
            if not utc_ns(query['start']) <= start < cut < end <= utc_ns(query['end']):
                raise ValueError('closed capture outside original daily partition')
            entry = rows.get(operation)
            if entry is not None:
                if entry['query_hash'] != query_hash:
                    raise ValueError('existing window financial identity differs')
                if entry['status'] == 'acquired':
                    reuse_proof(entry);disposition, reuse = 'reuse_acquired_window', operation
                else:
                    disposition, reuse = 'protected_existing_operation', None
            elif full_entry is not None and full_entry['status'] == 'acquired':
                disposition, reuse = 'reuse_acquired_full_day', full_op
            else:
                disposition, reuse = 'fresh_not_submitted', None
            windows.append([index, cut_index, cut, start, end, query_hash, operation, disposition, reuse])
    if len(windows) != 543 * 75 or sum(v[6] == PILOT for v in windows) != 1:
        raise ValueError('complete frozen decision plan or original pilot identity differs')
    if read_financial_journal(args.ledger_snapshot).raw != journal.raw or read_qualification_manifest(args.qualification).raw != qualification.raw:
        raise ValueError('planning inputs changed')
    counts = dict(Counter(v[7] for v in windows))
    plan = dict(schema='AutoWheelFrozenDecisionWindowPlanV1', sessions=126, original_full_day_partitions=543,
                symbols=calendar.SYMBOLS, window_columns=WINDOW_COLUMNS, partitions=partitions, windows=windows,
                captures_per_partition=75, decision_cuts_per_session=74, closing_valuation_cuts_per_session=1,
                max_age_nanos=30 * NANO, closed_cut_included_via_half_open_end=True,
                disposition_counts=counts, reuse_sources=list(proof_cache.values()), source_files=bindings,
                original708_journal_snapshot_sha256=journal.sha256, original_financial_rows_and_unknown_hold_preserved=True,
                planning_qualification_sha256=qualification.sha256, decision_window_plan_complete=True,
                selected_pending_order_inventory_lifecycle_plan_complete=False, full_replay_query_plan_complete=False,
                full_study_capacity_proven=False, source_gate_passed=False, historical_performance_available=False,
                financial_requests=0, live_journal_writes=0, subscription_requests=0, financial_retry=False)
    raw = encode(plan) + b'\n'
    if len(raw) > 64 << 20:
        raise ValueError('compact decision plan exceeds declared64MiB bound')
    args.output.mkdir(mode=0o700)
    compressed = gzip.compress(raw, mtime=0)
    save(args.output / 'decision-plan.json.gz', compressed)
    receipt = {k: v for k, v in plan.items() if k not in ('partitions', 'windows', 'reuse_sources', 'source_files')}
    receipt.update(window_count=len(windows), reused_full_day_sources=sum(v['full_day_existing_status'] == 'acquired' for v in partitions),
                   reuse_source_proofs=len(proof_cache), compressed_plan_sha256=hashlib.sha256(compressed).hexdigest(),
                   plan_sha256=hashlib.sha256(raw).hexdigest(), compressed_bytes=len(compressed), plan_bytes=len(raw),
                   observed_at=datetime.now(timezone.utc).isoformat())
    save(args.output / 'receipt.json', (json.dumps(receipt, indent=2, sort_keys=True) + '\n').encode())
    print(json.dumps(receipt))


if __name__ == '__main__':
    main()
