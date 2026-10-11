#!/usr/bin/env python3
"""Require truthful terminal evidence for all twelve original prepaid jobs.

Reuse accepted native EOF proofs; inspect immutable source mounts and their
sealed manifests without decoding multi-gigabyte sources again. No HTTP,
credentials, journal writes, mount changes, submission, or hold release.
"""
import hashlib
import importlib.util
import os
from pathlib import Path
import stat

BASE = Path('/home/yuduo/Projects/AutoWheel-research')
ROOT = BASE / 'source-afa3075'
PREVIOUS = BASE / 'cycle70-prepaid-book-recovery-20261008'
SUCCESSOR = BASE / 'cycle74-prepaid-book-recovery-20261008'
SUCCESSOR_SCRIPT = ROOT / 'scripts/recover-prepaid-books-cycle74.py'
SUCCESSOR_SHA = '5a95fed17bb92ab58fca6ca455ae052cc186277b71194fae84a9bd7dc427ad61'
SEAL_SHA = 'ac894a4ec6f3d3db9c8638150a02c8f1104072eac0b63954f0278dd709dbb578'

def load_successor():
    if SUCCESSOR_SCRIPT.resolve() != SUCCESSOR_SCRIPT or hashlib.sha256(SUCCESSOR_SCRIPT.read_bytes()).hexdigest() != SUCCESSOR_SHA:
        raise ValueError('accepted prepaid successor differs')
    spec = importlib.util.spec_from_file_location('qualified_prepaid_successor', SUCCESSOR_SCRIPT)
    module = importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
    return module

def require_terminal(preflight, run_name, program, exit_code):
    runtime = preflight.read_json(BASE / (run_name + '-supervisor-20261008') / 'runtime.json')
    if runtime.get('finished') is not True or type(runtime.get('exit_code')) is not int or runtime['exit_code'] != exit_code:
        raise ValueError('required prepaid terminal outcome unavailable')
    for key, name in (('supervisor_pid', 'supervisor.py'), ('recovery_pid', program)):
        if type(runtime.get(key)) is not int or runtime[key] <= 0:
            raise ValueError('prepaid terminal actor identity differs')
        process = Path('/proc', str(runtime[key]), 'cmdline')
        if process.exists() and any(Path(os.fsdecode(v)).name == name for v in process.read_bytes().split(b'\0') if v):
            raise ValueError('prepaid terminal actor remains live')
    return runtime

def verify_external(preflight, module, out, proof):
    storage = module.load_external_storage()
    intent = preflight.read_json(out / 'seal.json.intent.json')
    stage, lower = storage.manifest_scope(intent)
    if stage != BASE / (SUCCESSOR.name + '-retrieve-' + out.name) or proof['source_directory'] != str(stage):
        raise ValueError('external prepaid canonical source differs')
    storage.verifier.verify_mount(stage, lower)
    for phase, filename in (('seal', 'seal.json'), ('verify', 'seal-verify.json')):
        report = preflight.read_json(out / filename)
        if (report.get('schema') != 'AutoWheelExternalRawSourceSealV1' or report.get('phase') != phase
                or report.get('source_directory') != str(stage) or report.get('physical_archive_directory') != str(lower)
                or report.get('files_verified') != len(intent['files']) or report.get('bytes_verified') != intent['total_bytes']
                or any(report.get(k) is not True for k in ('source_bytes_preserved', 'final_source_path_preserved',
                    'final_path_established_before_native_proof', 'read_only', 'data_only_noexec'))
                or report.get('root_bulk_bytes_created') != 0 or report.get('original_data_copies_deleted') != 0
                or any(report.get(k) is not False for k in ('journal_changed', 'source_guard_changed', 'source_qualified'))
                or report.get('financial_requests') != 0):
            raise ValueError('external prepaid seal verification differs')
    if preflight.digest(preflight.private(stage / 'receipt.json')) != proof['receipt_sha256']:
        raise ValueError('external prepaid receipt binding differs')
    for member in proof['files']:
        stored = intent['files'][member['filename']]
        path = stage / member['filename']; info = path.lstat()
        if (path.resolve() != path or not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600
                or info.st_size != member['bytes'] or stored['bytes'] != member['bytes']
                or stored['sha256'] != member['sha256']):
            raise ValueError('external prepaid retained native file differs')

def require_original_financial_identity(initial, current):
    """Allow proof fields alone to change on the twelve formerly reserved rows."""
    if len(initial['reservations']) != 590 or len(current['reservations']) < 590:
        raise ValueError('original financial window size differs')
    for index, (original, actual) in enumerate(zip(initial['reservations'], current['reservations'][:590])):
        normalized = dict(actual)
        if index >= 575 and original['status'] == 'reserved' and actual.get('status') == 'acquired':
            for key in ('status', 'data_sha256', 'bytes'):
                if key in original: normalized[key] = original[key]
                else: normalized.pop(key, None)
        if normalized != original:
            raise ValueError('original financial reservation fields changed')

def require_complete(preflight, book_items, current):
    previous = require_terminal(preflight, 'cycle70-prepaid-book-recovery', 'recover-prepaid-books-cycle70.py', 1)
    runtime = require_terminal(preflight, 'cycle74-prepaid-book-recovery', 'recover-prepaid-books-cycle74.py', 0)
    if (runtime.get('runner_sha256') != SUCCESSOR_SHA or runtime.get('financial_submit_requests') != 0
            or runtime.get('remaining_original_jobs') != 3):
        raise ValueError('prepaid successor runtime binding differs')
    summary = preflight.read_json(SUCCESSOR / 'summary.json')
    initial = preflight.read_json(PREVIOUS / 'ledger-before.private.json')
    require_original_financial_identity(initial, current)
    completed = summary.get('completed', [])
    if (summary.get('schema') != 'AutoWheelPrepaidBookRecoverySuccessorV1'
            or summary.get('complete') is not True or summary.get('execute') is not True
            or summary.get('runner_sha256') != SUCCESSOR_SHA or summary.get('storage_adapter_sha256') != SEAL_SHA
            or summary.get('acknowledged_reserved_jobs') != 3 or summary.get('previous_recovery_completed_jobs') != 9
            or summary.get('original_reserved_window') != 12 or summary.get('original_completed_jobs') != 3
            or len(completed) != 12 or summary.get('financial_requests') != 0
            or summary.get('financial_retries') != 0 or summary.get('holds_released') is not False
            or summary.get('source_qualified') is not False or len(initial['reservations']) != 590
            or current['reservations'][:575] != initial['reservations'][:575]):
        raise ValueError('full original prepaid successor binding differs')
    reserved = {r['operation_id'] for r in initial['reservations'][575:] if r['status'] == 'reserved'}
    planned = {v[1]['filename']: v for v in book_items if v[3] in reserved}
    if (len(reserved) != 12 or len(planned) != 12 or len({v['query'] for v in completed}) != 12
            or {v['query'] for v in completed} != set(planned)):
        raise ValueError('full original prepaid completion scope differs')
    module = load_successor()
    rows = {r['operation_id']: r for r in current['reservations']}
    legacy = external = 0
    for result in completed:
        kind, item, query, operation, query_hash = planned[result['query']]
        legacy_scheme = result.get('storage_scheme') == 'legacy_private_readonly_archive'
        external_scheme = result.get('storage_scheme') == 'external_no_copy_readonly_seal'
        if not (legacy_scheme or external_scheme):
            raise ValueError('prepaid storage scheme differs')
        out = (PREVIOUS if legacy_scheme else SUCCESSOR) / item['filename'].removesuffix('.json')
        if result.get('recovery_output') != str(out) or result.get('proof_path') != str(out / 'proof.json'):
            raise ValueError('prepaid evidence path differs')
        proof_raw = preflight.private(out / 'proof.json'); proof = preflight.read_json(out / 'proof.json')
        row = rows[operation]
        if (preflight.digest(proof_raw) != result['proof_sha256'] or proof['query_file_sha256'] != item['sha256']
                or proof['native']['all_eof_verified'] is not True
                or proof['native']['records'] != proof['provider_record_count']
                or proof['provider_record_count'] != result['records'] or proof['source_qualified'] is not False
                or proof['reservation']['operation_id'] != operation
                or row['status'] != 'acquired' or row['endpoint'] != '/batch.submit_job'
                or row['query_hash'] != query_hash or row['data_sha256'] != result['proof_sha256']
                or row['bytes'] != proof['total_bytes'] or result['compressed_bytes'] != proof['total_bytes']
                or result['kind'] != kind or result['financial_requests'] != 0
                or result['all_eof_verified'] is not True or result['source_qualified'] is not False):
            raise ValueError('original prepaid native or journal proof differs')
        if legacy_scheme:
            accepted = module.previous_complete(preflight, row, BASE / 'cycle56-full-book-acquisition-20261007' / out.name, item)
            if accepted != result:
                raise ValueError('legacy prepaid completion differs from reused proof')
            legacy += 1
        else:
            if preflight.read_json(out / 'aggregate.json') != result:
                raise ValueError('external prepaid completion differs from aggregate')
            verify_external(preflight, module, out, proof); external += 1
    if legacy != 9 or external != 3:
        raise ValueError('prepaid successor completion split differs')
    return dict(original_reserved_window=12, legacy_native_archive_jobs=legacy, external_native_sealed_jobs=external,
        previous_terminal_exit_code=previous['exit_code'], successor_terminal_exit_code=runtime['exit_code'],
        successor_summary_path=str(SUCCESSOR / 'summary.json'),
        successor_summary_sha256=preflight.digest(preflight.private(SUCCESSOR / 'summary.json')),
        native_evidence_reused=True, financial_requests=0, source_qualified=False)
