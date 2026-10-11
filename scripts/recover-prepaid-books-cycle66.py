#!/usr/bin/env python3
"""Recover cycle56's exact 12 acknowledged jobs; never submit or release holds.

Use the freshly built finalizer with its native book verifier. Retain the incomplete original
download, use a new private namespace, serialize finalization,
and archive/verify before releasing only a redundant local staging backup.
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


def make_plan(b, p, before):
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
    jobs, acquired, seen = [], 0, set()
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
            if p.digest(p.private(out / 'proof.json')) != row['data_sha256']:
                raise ValueError('acquired original proof differs')
            acquired += 1
        elif row['status'] == 'reserved':
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
    if acquired != 3 or len(jobs) != 12 or len(seen) != 15:
        raise ValueError('exact prepaid window differs')
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
            or re.fullmatch(r'cycle66-prepaid-book-recovery(?:-dry)?-20261007', args.output.name) is None):
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
        args.output.mkdir(mode=0o700)
        p.save(args.output / 'ledger-before.private.json', expected)
        report = {'schema': 'AutoWheelPrepaidBookRecoveryV1', 'execute': args.execute,
                  'runner_sha256': p.digest(Path(__file__).read_bytes()),
                  'finalizer_sha256': FINALIZER_SHA, 'acknowledged_reserved_jobs': len(jobs),
                  'original_completed_jobs': 3, 'financial_requests': 0,
                  'financial_retries': 0, 'holds_released': False, 'source_qualified': False}
        p.save(args.output / 'plan.json', report)
        print(json.dumps(report), flush=True)
        if not args.execute:
            return
        completed = []

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
                stage = BASE / b.staging_name(args.output.name, original.name)
                if stage.exists() or stage.is_symlink():
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
                b.require_download_fit(b.free_bytes(BASE), b.free_bytes(b.EXTERNAL_DRIVE), total)
                if p.digest(p.private(plan_dir / 'files-source.json')) != plan['file_list_sha256']:
                    raise ValueError('planned original file list differs')
                if p.run(tools['planned_retrieve'], [*common, '--output', stage, '--timeout', '3h',
                         '--planned-files', plan_dir / 'files-source.json'], out, 'download') != 0:
                    raise ValueError('prepaid download stopped')
                b.same_download_plan(plan, p.read_json(stage / 'receipt.json'))
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
            for script, label, more in (
                ('archive-research-source.py', 'archive', ['--source', stage, '--binder', binder]),
                ('restore-research-source.py', 'restore', ['--intent', out / 'archive.json.intent.json',
                                                         '--binder', binder, '--archive-only']),
                ('release-research-backup.py', 'release', ['--intent', out / 'archive.json.intent.json',
                                                          '--archive-report', out / 'archive.json'])):
                check()
                if (p.digest(p.private(proof_path)) != proof_sha
                        or proof['source_directory'] != str(stage)
                        or p.digest(p.private(stage / 'receipt.json')) != proof['receipt_sha256']):
                    raise ValueError('proof changed before archive step')
                if p.run(PYTHON, [ROOT / 'scripts' / script, *more, '--output', out / (label + '.json')], out, label) != 0:
                    raise ValueError('archive verification step stopped; retain evidence')
            check()
            result = {'query': item['filename'], 'kind': kind, 'records': proof['native']['records'],
                      'compressed_bytes': proof['total_bytes'], 'proof_sha256': proof_sha,
                      'financial_requests': 0, 'all_eof_verified': True, 'source_qualified': False}
            p.save(out / 'aggregate.json', result)
            completed.append(result)
            print(json.dumps({'completed': len(completed), 'required': len(jobs), **result}), flush=True)
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
