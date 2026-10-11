"""Independent offline reconstruction of every compact decision-plan row."""
import argparse
from collections import Counter
from datetime import date, datetime, time, timezone
import gzip
import hashlib
import json
import os
from pathlib import Path
from zoneinfo import ZoneInfo

from batch_acquisition_proof import read_json
from research_history_reader import read_financial_journal, read_qualification_manifest

MANIFEST_SHA = 'ccf48c229ce29db62e0d4aaa20cb06c733fdf4e528cf2d79f4da0e71a00adb2b'
PILOT = 'fef32e79bfd60d0db6d8d91cc57640f82df501ea353c75be8b40a3c866d24431'
PROTECTED = '01593d27c3a667a42b8736bba57c377e98dd6f19949f4601e2c08ef505abbf5f'
NANO = 1_000_000_000


def sha(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def unique(pairs):
    out = {}
    for key, value in pairs:
        if key in out:
            raise ValueError('duplicate plan field')
        out[key] = value
    return out


def ns_stamp(value):
    seconds = value // NANO
    micros = datetime.fromtimestamp(seconds, tz=timezone.utc).isoformat(timespec='seconds')[:-6]
    remainder = value % NANO
    return micros + ('.' + str(remainder).zfill(9).rstrip('0') if remainder else '') + 'Z'


def canonical(query):
    # Independent serializer matching the reviewed Go wire struct, without
    # importing the producer or its cached prefix/identity/timestamp helpers.
    wire = {key: query[key] for key in ('dataset', 'schema', 'symbols', 'stype_in', 'start', 'end')}
    wire.update(encoding='dbn', compression='zstd', stype_out='instrument_id', split_duration='day',
                split_symbols=False, pretty_px=False, pretty_ts=False, map_symbols=False, delivery='download')
    return json.dumps(wire, ensure_ascii=False, separators=(',', ':'), allow_nan=False).encode()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('source-root', 'plan', 'ledger-snapshot', 'qualification', 'output'):
        parser.add_argument('--' + name, type=Path, required=True)
    args = parser.parse_args()
    for path in vars(args).values():
        if not path.is_absolute() or path.resolve() != path:
            raise ValueError('clean absolute reference paths required')
    journal = read_financial_journal(args.ledger_snapshot)
    qualification = read_qualification_manifest(args.qualification)
    with gzip.open(args.plan, 'rb') as stream:
        raw = stream.read((64 << 20) + 1)
        if not raw or len(raw) > 64 << 20 or stream.read(1):
            raise ValueError('bounded complete plan gzip required')
    plan = json.loads(raw, object_pairs_hook=unique, parse_constant=lambda _: (_ for _ in ()).throw(ValueError('nonfinite')))
    if plan['schema'] != 'AutoWheelFrozenDecisionWindowPlanV1' or plan['original708_journal_snapshot_sha256'] != journal.sha256:
        raise ValueError('plan schema or immutable financial snapshot differs')
    if plan['planning_qualification_sha256'] != qualification.sha256:
        raise ValueError('original qualification snapshot differs')
    if len(plan['partitions']) != 543 or len(plan['windows']) != 40725 or plan['sessions'] != 126:
        raise ValueError('full frozen decision scope differs')
    if plan['window_columns'] != ['partition_index', 'cut_index', 'cut_ns', 'start_ns', 'end_ns',
                                  'query_hash', 'operation_id', 'disposition', 'reuse_operation_id']:
        raise ValueError('window column shape differs')
    for key in ('financial_requests', 'live_journal_writes', 'subscription_requests'):
        if type(plan[key]) is not int or plan[key] != 0:
            raise ValueError('offline-only plan required')
    for key in ('full_replay_query_plan_complete', 'selected_pending_order_inventory_lifecycle_plan_complete',
                'full_study_capacity_proven', 'source_gate_passed', 'historical_performance_available', 'financial_retry'):
        if plan[key] is not False:
            raise ValueError('decision plan cannot claim execution coverage, capacity or returns')
    rows = {v['operation_id']: v for v in journal.value['reservations']}
    if len(rows) != 708 or rows[PROTECTED]['status'] != 'reserved' or rows[PILOT]['status'] != 'acquired':
        raise ValueError('all original708 financial identities and holds required')
    known = {v['path']: v['sha256'] for v in qualification.value['source_files']}
    manifest_path = Path(plan['partitions'][0]['query_path']).parent / 'manifest.json'
    if sha(manifest_path) != MANIFEST_SHA:
        raise ValueError('full original manifest differs')
    manifest = read_json(manifest_path)
    books = [v for v in manifest['queries'] if v['schema'] == 'cmbp-1']
    if len(books) != 543 or manifest['daily_partitions'] != 126:
        raise ValueError('full manifest scope differs')
    for binding in plan['source_files']:
        path = Path(binding['path']);path = path if path.is_absolute() else args.source_root / path
        if sha(path) != binding['sha256']:
            raise ValueError('plan source binding differs')
    expected_sources, seen, counts = set(), set(), Counter()
    for index, (partition, original_item) in enumerate(zip(plan['partitions'], books, strict=True)):
        query_path = manifest_path.parent / original_item['filename']
        if (partition['index'] != index or partition['date'] != original_item['date']
                or partition['query_path'] != str(query_path) or partition['query_sha256'] != original_item['sha256']
                or sha(query_path) != original_item['sha256']):
            raise ValueError('partition roster or order differs')
        query = read_json(query_path)
        if len(query['symbols']) != partition['symbols'] or query['dataset'] != 'OPRA.PILLAR' or query['schema'] != 'cmbp-1':
            raise ValueError('full partition scope differs')
        full_wire = canonical(query)
        full_hash = hashlib.sha256(full_wire).hexdigest()
        full_operation = hashlib.sha256(full_wire + b'\0/batch.submit_job').hexdigest()
        full_entry = rows.get(full_operation)
        full_status = full_entry['status'] if full_entry else None
        if (partition['full_operation_id'] != full_operation or partition['full_query_hash'] != full_hash
                or partition['full_day_existing_status'] != full_status):
            raise ValueError('original full-day financial identity differs')
        day = date.fromisoformat(partition['date'])
        first = int(datetime.combine(day, time(9, 35), ZoneInfo('America/New_York')).timestamp()) * NANO
        close = int(datetime.combine(day, time(16), ZoneInfo('America/New_York')).timestamp()) * NANO
        for cut_index in range(75):
            row = plan['windows'][index * 75 + cut_index]
            cut = first + cut_index * 300 * NANO if cut_index < 74 else close
            start, end = cut - 30 * NANO, cut + 1
            if len(row) != 9 or any(type(v) is not int for v in row[:5]) or row[:5] != [index, cut_index, cut, start, end]:
                raise ValueError('independent closed-cut/window arithmetic differs')
            window_query = dict(query, start=ns_stamp(start), end=ns_stamp(end))
            wire = canonical(window_query)
            query_hash = hashlib.sha256(wire).hexdigest()
            operation = hashlib.sha256(wire + b'\0/batch.submit_job').hexdigest()
            if row[5:7] != [query_hash, operation] or operation in seen:
                raise ValueError('independent canonical identity or global deduplication differs')
            seen.add(operation)
            entry = rows.get(operation)
            if entry:
                if entry['query_hash'] != query_hash:
                    raise ValueError('existing window financial hash differs')
                disposition, reuse = ('reuse_acquired_window', operation) if entry['status'] == 'acquired' else ('protected_existing_operation', None)
            elif full_status == 'acquired':
                disposition, reuse = 'reuse_acquired_full_day', full_operation
            else:
                disposition, reuse = 'fresh_not_submitted', None
            if row[7:] != [disposition, reuse]:
                raise ValueError('reuse, protected operation or fresh disposition differs')
            counts[disposition] += 1
            if reuse:
                expected_sources.add(reuse)
    if dict(counts) != plan['disposition_counts'] or PILOT not in seen:
        raise ValueError('full counts or exact acquired pilot absent')
    sources = plan['reuse_sources']
    if len({v['operation_id'] for v in sources}) != len(sources) or {v['operation_id'] for v in sources} != expected_sources:
        raise ValueError('reuse source set differs')
    for source in sources:
        entry = rows[source['operation_id']]
        if entry['status'] != 'acquired' or source['proof_sha256'] != entry['data_sha256'] or known.get(source['proof_path']) != entry['data_sha256']:
            raise ValueError('reuse proof financial/source binding differs')
        path = Path(source['proof_path']);path = path if path.is_absolute() else args.source_root / path
        if sha(path) != entry['data_sha256']:
            raise ValueError('retained original proof digest differs')
        proof = read_json(path)
        if (proof['query'] != source['query'] or proof['native']['records'] != source['native_records']
                or proof['total_bytes'] != source['total_bytes'] or source['full_source_qualification'] is not False):
            raise ValueError('reuse original proof content differs')
    if read_financial_journal(args.ledger_snapshot).raw != journal.raw or read_qualification_manifest(args.qualification).raw != qualification.raw:
        raise ValueError('verification input changed')
    receipt = dict(schema='AutoWheelIndependentDecisionWindowPlanVerificationV1',
                   independent_complete_window_identities_verified=40725, original_partitions_verified=543,
                   sessions=126, exact_closed_cut_included=True, global_operation_deduplication_verified=True,
                   original708_financial_rows_and_unknown_hold_preserved=True, disposition_counts=dict(counts),
                   reuse_source_proofs_verified=len(sources), compressed_plan_sha256=sha(args.plan),
                   plan_sha256=hashlib.sha256(raw).hexdigest(), verifier_sha256=sha(Path(__file__).resolve()),
                   prior_full_native_verifications_reused=True, full_native_source_reaudit_run=False,
                   decision_window_plan_component_accepted=True, full_replay_query_plan_complete=False,
                   full_study_capacity_proven=False, source_gate_passed=False, historical_performance_available=False,
                   provider_requests=0, financial_requests=0, live_journal_writes=0, financial_retry=False)
    args.output.mkdir(mode=0o700)
    fd = os.open(args.output / 'receipt.json', os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'wb') as stream:
        stream.write((json.dumps(receipt, indent=2, sort_keys=True) + '\n').encode());stream.flush();os.fsync(stream.fileno())
    print(json.dumps(receipt))


if __name__ == '__main__':
    main()
