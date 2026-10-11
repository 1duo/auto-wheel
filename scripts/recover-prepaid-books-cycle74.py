#!/usr/bin/env python3
"""Recover the remaining three original acknowledged jobs; never submit or release holds.

Use the freshly built finalizer with its native book verifier. Retain the incomplete original
download, use a new private namespace, serialize finalization,
download on Lexar, then establish a final readonly path before native finalization. No bulk root copy.
"""
import argparse
import ast
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import stat
import time

BASE = Path('/home/yuduo/Projects/AutoWheel-research')
ROOT = BASE / 'source-afa3075'
RUN = BASE / 'cycle56-full-book-acquisition-20261007'
LEDGER = Path('/home/yuduo/.local/share/autowheel-research/databento-budget.json')
KEY = Path('/home/yuduo/.config/autowheel/databento-pilot.key')
PYTHON = BASE / 'dbn-env-0.71.0/bin/python'
BOOK_SHA = '827704179dd2c157d19f547bc9ab922ce1623c0e09d5ae64d368c7464b35bc15'
FINALIZER = BASE / 'cycle52-autowheel-batch-finalize-20261007'
FINALIZER_SHA = '119eecab3d887a4fb3d99b03546e3adb8303a934f8828bc7ddbea4c0a9349736'
PREVIOUS = BASE / 'cycle70-prepaid-book-recovery-20261008'
BULK = BASE / 'cycle73-bulk-storage-20261008'
SEAL = ROOT / 'scripts/seal-external-research-source-cycle74-v2.py'
SEAL_SHA = 'ac894a4ec6f3d3db9c8638150a02c8f1104072eac0b63954f0278dd709dbb578'


def load_book():
    path = ROOT / 'scripts/acquire-prefetched-book-plan-current-finalizer.py'
    if path.is_symlink() or hashlib.sha256(path.read_bytes()).hexdigest() != BOOK_SHA:
        raise ValueError('sealed book controller differs')
    spec = importlib.util.spec_from_file_location('sealed_book', path)
    book = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(book)
    book.PREPAID_RECOVERY_RUNNERS |= {Path(__file__).name}
    book.STATISTICS_ACQUISITION_RUNNERS |= {'acquire-prefetched-book-plan-current-finalizer.py'}
    return book


def checked_binary(p, path, digest):
    info = path.lstat()
    if (path.resolve() != path or not stat.S_ISREG(info.st_mode)
            or info.st_uid != os.getuid() or info.st_nlink != 1
            or stat.S_IMODE(info.st_mode) != 0o755 or p.digest(path.read_bytes()) != digest):
        raise ValueError('sealed executable differs')
    return path


def previous_complete(p, row, original, item):
    """Reuse native and immutable archive evidence, never recreate an acquisition."""
    out = PREVIOUS / original.name
    aggregate = p.read_json(out / 'aggregate.json')
    proof_raw = p.private(out / 'proof.json')
    proof = p.read_json(out / 'proof.json')
    if (p.digest(proof_raw) != row['data_sha256'] or aggregate['proof_sha256'] != row['data_sha256']
            or aggregate['query'] != item['filename'] or aggregate['all_eof_verified'] is not True
            or aggregate['financial_requests'] != 0 or proof['source_qualified'] is not False
            or proof['native']['all_eof_verified'] is not True
            or proof['native']['records'] != proof['provider_record_count']
            or aggregate['records'] != proof['provider_record_count']
            or proof['query_file_sha256'] != item['sha256']
            or proof['reservation']['operation_id'] != row['operation_id']):
        raise ValueError('previous acquired native proof differs')
    archive = p.read_json(out / 'archive.json')
    restore = p.read_json(out / 'restore.json')
    release = p.read_json(out / 'release.json')
    stage = Path(proof['source_directory'])
    if stage != BASE / (PREVIOUS.name + '-retrieve-' + original.name):
        raise ValueError('previous source scope differs')
    mounts = [v.split() for v in Path('/proc/mounts').read_text().splitlines()]
    mount = next((v for v in mounts if v[1] == str(stage)), None)
    if (archive['original_paths_and_bytes_preserved'] is not True or archive['read_only'] is not True
            or restore['archive_only'] is not True or restore['journal_changed'] is not False
            or release['files_released'] != restore['files_verified']
            or release['files_released'] != len(archive['files'])
            or release['bytes_released'] != restore['bytes_verified']
            or release['bytes_released'] != archive['total_bytes']
            or mount is None or mount[0] != archive['physical_archive_directory']
            or not {'ro', 'nosuid', 'nodev', 'noexec', 'default_permissions'}.issubset(set(mount[3].split(',')))
            or 'allow_other' in mount[3].split(',')
            or p.digest(p.private(stage / 'receipt.json')) != proof['receipt_sha256']):
        raise ValueError('previous archive proof differs')
    for member in proof['files']:
        stored = archive['files'][member['filename']]
        path = stage / member['filename']; info = path.lstat()
        if (path.resolve() != path or not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600
                or info.st_size != member['bytes'] or stored['bytes'] != member['bytes']
                or stored['sha256'] != member['sha256']):
            raise ValueError('previous retained native file differs')
    return dict(**aggregate, proof_path=str(out / 'proof.json'), recovery_output=str(out),
                storage_scheme='legacy_private_readonly_archive')


def load_external_storage():
    if SEAL.resolve() != SEAL or hashlib.sha256(SEAL.read_bytes()).hexdigest() != SEAL_SHA:
        raise ValueError('accepted external storage adapter differs')
    spec = importlib.util.spec_from_file_location('sealed_external_storage', SEAL)
    seal = importlib.util.module_from_spec(spec); spec.loader.exec_module(seal)
    seal.lib = seal.load(seal.LIBRARY, 'sealed_storage_library', seal.LIBRARY_SHA)
    seal.verifier = seal.load(seal.VERIFIER, 'sealed_storage_verifier', seal.VERIFIER_SHA)
    seal.verifier.lib = seal.lib; seal.verifier.ALLOWLIST = (seal.VIEW.name,)
    seal.verify_parent()
    return seal


def make_plan(b, p, before):
    stopped = p.read_json(BASE / 'cycle70-prepaid-book-recovery-supervisor-20261008/runtime.json')
    if stopped['finished'] is not True or type(stopped['exit_code']) is not int or stopped['exit_code'] != 1:
        raise ValueError('previous recovery must be authoritatively stopped')
    for pid in (stopped['supervisor_pid'], stopped['recovery_pid']):
        path = Path('/proc', str(pid), 'cmdline')
        if path.exists() and any(Path(os.fsdecode(v)).name in ('supervisor.py', 'recover-prepaid-books-cycle70.py') for v in path.read_bytes().split(b'\0') if v):
            raise ValueError('previous recovery actor still live')
    initial_previous = p.read_json(PREVIOUS / 'ledger-before.private.json')
    prior_plan = p.read_json(PREVIOUS / 'plan.json')
    if (len(initial_previous['reservations']) != 590 or prior_plan['acknowledged_reserved_jobs'] != 12
            or prior_plan['runner_sha256'] != 'ae1cf586cbd288e8e387d7bdb3373af37f56f4900be27df4d1650c0543c9aa0a'):
        raise ValueError('previous recovery window differs')
    prior_reserved = {v['operation_id'] for v in initial_previous['reservations'][575:] if v['status'] == 'reserved'}
    if len(prior_reserved) != 12:
        raise ValueError('previous reserved scope differs')
    original = p.read_json(RUN / 'ledger-before.private.json')
    runtime = p.read_json(RUN / 'runtime-plan.json')
    interrupted = p.read_json(RUN / 'interruption.json')
    if (before['reservations'][:len(original['reservations'])] != original['reservations']
            or len(original['reservations']) != 575 or len(before['reservations']) != 590
            or runtime['runner_sha256'] != BOOK_SHA or runtime['execute'] is not True
            or interrupted['phase'] != 'download' or interrupted['completed_queries'] != 3
            or interrupted['submitted_queries'] != 15 or interrupted['pending_queries'] != 12
            or interrupted['financial_retry'] is not False or interrupted['holds_released'] is not False):
        raise ValueError('owned stopped run differs')
    # Both immutable manifests are independently hash-checked by the sealed
    # controller; these are their accepted full-study paths.
    option = BASE / 'cycle24-selective-acquisition-queries-20261006/manifest.json'
    stock = BASE / 'cycle27-stock-daily-acquisition-queries-20261006/manifest.json'
    items, stats_ops, _ = b.collect_book_operations(p, option, stock)
    rows = {r['operation_id']: r for r in before['reservations']}
    b.check_statistics_coverage(stats_ops, rows)
    planned = {op: (kind, item, query, qh) for kind, item, query, op, qh in items}
    jobs, acquired, seen, previous = [], 0, set(), []
    for out in sorted(RUN.glob('2026-*')):
        b.private_directory(out)
        intent = p.read_json(out / 'intent.json')
        op = intent['operation_id']
        if op in seen or op not in planned or op not in rows or intent['financial_retry'] is not False:
            raise ValueError('original operation differs')
        seen.add(op)
        kind, item, query, qh = planned[op]
        row = rows[op]
        ack = p.read_json(out / 'submit/ack.json')
        body = p.private(out / 'submit/job.json')
        quote = p.read_json(out / 'submit/quote.json')
        if (out.name + '.json' != item['filename'] or intent['query_file_sha256'] != item['sha256']
                or intent['kind'] != kind
                or row['query_hash'] != qh or row['endpoint'] != '/batch.submit_job'
                or ack['status_code'] != 200 or ack['response_hash'] != p.digest(body)
                or ack['bytes'] != len(body) or ack['query_hash'] != qh
                or row['response_hash'] != quote['quote']['ResponseHash']
                or any(ack[k] != row[k] for k in
                       ('operation_id', 'query_hash', 'endpoint', 'cost_nano', 'reserved_nano', 'at'))):
            raise ValueError('original acknowledgement differs')
        if row['status'] == 'acquired':
            if op in prior_reserved:
                previous.append(previous_complete(p, row, out, item))
            elif p.digest(p.private(out / 'proof.json')) != row['data_sha256']:
                raise ValueError('acquired original proof differs')
            acquired += 1
        elif row['status'] == 'reserved':
            if op not in prior_reserved:
                raise ValueError('remaining operation outside original prepaid window')
            old_stage = BASE / b.staging_name(RUN.name, out.name)
            if old_stage.exists():
                if item['filename'] != interrupted['query']:
                    raise ValueError('unexpected earlier staged source')
                b.private_directory(old_stage)
                # An interrupted download without a verified receipt is never
                # accepted or overwritten. Preserve it and retrieve the same
                # acknowledged prepaid job into a fresh private namespace.
                if (old_stage / 'receipt.json').exists():
                    receipt = p.read_json(old_stage / 'receipt.json')
                    if (receipt['state'] != 'done' or receipt['all_verified'] is not True
                            or receipt['operation_id'] != op or receipt['query_hash'] != qh):
                        raise ValueError('original verified download differs')
                    stage = old_stage
                else:
                    stage = None
            else:
                stage = None
            jobs.append((out, kind, item, query, op, qh, stage))
        else:
            raise ValueError('unexpected operation status')
    if acquired != 12 or len(previous) != 9 or len(jobs) != 3 or len(seen) != 15 or any(v[6] is not None for v in jobs):
        raise ValueError('exact remaining prepaid window differs')
    b.previous_completed = previous
    jobs.sort(key=lambda r: (r[6] is None, r[1] != 'stock-mbp-1', r[2]['filename']))
    return jobs


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--output', type=Path, required=True)
    ap.add_argument('--execute', action='store_true')
    args = ap.parse_args()
    b = load_book()
    for path in (BASE, ROOT, RUN):
        b.private_directory(path)
    if (args.output.parent != BASE or args.output.resolve() != args.output or args.output.exists()
            or re.fullmatch(r'cycle74-prepaid-book-recovery(?:-dry)?-20261008', args.output.name) is None):
        raise ValueError('fresh bounded recovery namespace required')
    b.live_writers_absent()
    fds = []
    try:
        fds.append(b.acquire_reserved_safe_lock(BASE))
        fds.append(b.acquire_book_lock(BASE))
        b.live_writers_absent()
        old = b.load_old_controller()
        p = old.load_preflight()
        qualification = p.read_qualification(ROOT / 'research/simulation/2026-10-05/databento-qualification.json')
        expected_sources = {r['path']: r['sha256'] for r in qualification['source_files']}
        for name in b.BOOK_CODE:
            path = ROOT / name
            if path.is_symlink() or path.resolve() != path or p.digest(path.read_bytes()) != expected_sources.get(name):
                raise ValueError('accepted book source differs')
        tools = {name: checked_binary(p, BASE / path, digest) for name, (path, digest)
                 in (p.TOOLS | b.FILE_TOOLS).items() if name != 'finalize'}
        tools['finalize'] = checked_binary(p, FINALIZER, FINALIZER_SHA)
        native = (ROOT / 'internal/databento/native_book_completeness.py').read_bytes()
        if native not in FINALIZER.read_bytes():
            raise ValueError('current native book verifier absent from executable')
        binder = checked_binary(p, BASE / b.BINDER_RELATIVE, b.BINDER_SHA256)
        archive_path = ROOT / 'scripts/archive-research-source.py'
        archive_spec = importlib.util.spec_from_file_location('sealed_archive', archive_path)
        archive = importlib.util.module_from_spec(archive_spec)
        archive_spec.loader.exec_module(archive)
        archive.directory(b.EXTERNAL_DRIVE)
        archive.verify_backing_mount()
        _, expected = b.ledger_snapshot(p, LEDGER)
        jobs = make_plan(b, p, expected)
        load_external_storage()
        args.output.mkdir(mode=0o700)
        p.save(args.output / 'ledger-before.private.json', expected)
        report = {'schema': 'AutoWheelPrepaidBookRecoverySuccessorV1', 'execute': args.execute,
                  'runner_sha256': p.digest(Path(__file__).read_bytes()),
                  'finalizer_sha256': FINALIZER_SHA, 'acknowledged_reserved_jobs': len(jobs),
                  'original_completed_jobs': 3, 'previous_recovery_completed_jobs': 9,
                  'original_reserved_window': 12, 'bulk_parent': str(BULK),
                  'storage_adapter_sha256': SEAL_SHA, 'financial_requests': 0,
                  'financial_retries': 0, 'holds_released': False, 'source_qualified': False}
        p.save(args.output / 'plan.json', report)
        print(json.dumps(report), flush=True)
        if not args.execute:
            return
        completed = list(b.previous_completed)

        def check():
            b.live_writers_absent()
            if b.ledger_snapshot(p, LEDGER)[1] != expected:
                raise ValueError('unexpected journal mutation')

        for original, kind, item, query, op, qh, stage in jobs:
            check()
            out = args.output / original.name
            out.mkdir(mode=0o700)
            common = ['--query', query, '--ack', original / 'submit/ack.json',
                      '--ack-body', original / 'submit/job.json', '--key-file', KEY]
            if stage is None:
                name = b.staging_name(args.output.name, original.name)
                download = BULK / name
                stage = BASE / name
                if stage.exists() or stage.is_symlink() or download.exists() or download.is_symlink():
                    raise ValueError('fresh download namespace required')
                for poll in range(1080):
                    check()
                    receipt = old.observe_readonly(p, tools['retrieve'], common, out, 'observe-' + str(poll))
                    if receipt['state'] == 'done':
                        break
                    if receipt['state'] not in ('queued', 'processing'):
                        raise ValueError('unknown prepaid state')
                    time.sleep(20)
                else:
                    raise ValueError('prepaid job deadline; hold preserved')
                plan_dir, plan = b.observe_file_plan(p, tools['file_plan'], common, out, 'files')
                total = b.file_plan_total(plan, op, qh)
                check()
                b.require_download_fit(b.free_bytes(BULK), b.free_bytes(b.EXTERNAL_DRIVE), total)
                if p.digest(p.private(plan_dir / 'files-source.json')) != plan['file_list_sha256']:
                    raise ValueError('planned original file list differs')
                if p.run(tools['planned_retrieve'], [*common, '--output', download, '--timeout', '3h',
                         '--planned-files', plan_dir / 'files-source.json'], out, 'download') != 0:
                    raise ValueError('prepaid download stopped')
                b.same_download_plan(plan, p.read_json(download / 'receipt.json'))
                check()
                if p.run(PYTHON, [SEAL, '--phase', 'seal', '--source', download,
                         '--output', out / 'seal.json'], out, 'seal') != 0:
                    raise ValueError('external source sealing stopped; retain original data')
                sealed = p.read_json(out / 'seal.json')
                if (sealed['source_directory'] != str(stage) or sealed['read_only'] is not True
                        or sealed['root_bulk_bytes_created'] != 0 or sealed['journal_changed'] is not False
                        or sealed['source_bytes_preserved'] is not True or sealed['source_qualified'] is not False):
                    raise ValueError('external readonly source binding differs')
            check()
            total = p.read_json(stage / 'receipt.json')['total_bytes']
            if total + b.RESERVE_BYTES > b.free_bytes(b.EXTERNAL_DRIVE):
                raise ValueError('durable archive capacity unavailable')
            proof_path = out / 'proof.json'
            if p.run(tools['finalize'], ['--query', query, '--ack', original / 'submit/ack.json',
                     '--ack-body', original / 'submit/job.json', '--source', stage, '--proof', proof_path,
                     '--python', PYTHON, '--ledger', LEDGER, '--timeout', '30m'], out, 'finalize') != 0:
                raise ValueError('offline book finalization stopped')
            proof = p.read_json(proof_path)
            proof_sha = p.digest(p.private(proof_path))
            if (proof['query_file_sha256'] != item['sha256'] or proof['native']['all_eof_verified'] is not True
                    or proof['native']['records'] != proof['provider_record_count'] or proof['source_qualified'] is not False):
                raise ValueError('native proof binding differs')
            _, current = b.ledger_snapshot(p, LEDGER)
            old.finalized(expected, current, op, proof_sha, proof['total_bytes'])
            expected = current
            check()
            if (p.digest(p.private(proof_path)) != proof_sha or proof['source_directory'] != str(stage)
                    or p.digest(p.private(stage / 'receipt.json')) != proof['receipt_sha256']):
                raise ValueError('final readonly source proof changed')
            if p.run(PYTHON, [SEAL, '--phase', 'verify', '--intent', out / 'seal.json.intent.json',
                     '--output', out / 'seal-verify.json'], out, 'seal-verify') != 0:
                raise ValueError('external source verification stopped; retain evidence')
            check()
            result = {'query': item['filename'], 'kind': kind, 'records': proof['native']['records'],
                      'compressed_bytes': proof['total_bytes'], 'proof_sha256': proof_sha,
                      'proof_path': str(proof_path), 'recovery_output': str(out),
                      'storage_scheme': 'external_no_copy_readonly_seal',
                      'financial_requests': 0, 'all_eof_verified': True, 'source_qualified': False}
            p.save(out / 'aggregate.json', result)
            completed.append(result)
            print(json.dumps({'completed': len(completed), 'required': 12, **result}), flush=True)
        if len(completed) != 12 or len({v['query'] for v in completed}) != 12:
            raise ValueError('full original prepaid completion scope differs')
        p.save(args.output / 'ledger-after.private.json', expected)
        p.save(args.output / 'summary.json', dict(report, completed=completed, complete=True))
    finally:
        for fd in reversed(fds):
            os.close(fd)


if __name__ == '__main__':
    try:
        main()
    except Exception as exc:
        # Only literal validation messages from reviewed source files can leave
        # private logs. Never emit provider errors, paths, keys, or identifiers.
        fixed = set()
        for path in (Path(__file__), ROOT / 'scripts/acquire-prefetched-book-plan-current-finalizer.py',
                     ROOT / 'scripts/acquire-selective-statistics-plan-large-manifest.py'):
            for node in ast.walk(ast.parse(path.read_text())):
                if (isinstance(node, ast.Call) and isinstance(node.func, ast.Name)
                        and node.func.id == 'ValueError' and len(node.args) == 1
                        and isinstance(node.args[0], ast.Constant)
                        and isinstance(node.args[0].value, str)):
                    fixed.add(node.args[0].value)
        print(json.dumps({'stopped': True, 'error_type': type(exc).__name__,
                          'fixed_error': str(exc) if str(exc) in fixed else 'unclassified',
                          'financial_requests': 0, 'holds_released': False}), flush=True)
        raise SystemExit(1)
