"""One declared historical BOOK window, using existing guarded Go tools.

Requires terminal stock acquisition, both existing writer locks, the exact
707-entry terminal snapshot and a fresh operation/output. Financial submission
occurs at most once; any failure retains its attempt and spending hold. Only
observation of that acknowledged job can repeat. No recurring subscription,
live trading, bulk-drive writes, changed legacy helpers or source qualification.
"""
import argparse
from datetime import datetime, timezone
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import stat
import sys
import time
import traceback
import urllib.request

BASE = Path('/home/yuduo/Projects/AutoWheel-research')
ROOT = BASE / 'source-afa3075'
D = ROOT / 'research/simulation/2026-10-05'
OUT = BASE / 'cycle88-one-book-window-acquisition-20261008'
UPSTREAM = BASE / 'cycle74-stock-acquisition-supervisor-20261008'
STOCK = BASE / 'cycle74-stock-acquisition-20261008'
LEDGER = Path('/home/yuduo/.local/share/autowheel-research/databento-budget.json')
KEY = Path('/home/yuduo/.config/autowheel/databento-pilot.key')
PYTHON = BASE / 'dbn-env-0.71.0/bin/python'
QUERY = D / 'cycle87-window-boundary-evidence/proposed-query.json'
QUERY_SHA = '5c7466dc44c12eab5eb913f84cbd7bd1cdaed04db4d3522615edb03067727211'
OPERATION = 'fef32e79bfd60d0db6d8d91cc57640f82df501ea353c75be8b40a3c866d24431'
QUERY_HASH = 'c4557ba33c003b113dc95bb8f70ba96062d22dab4f3a81a98c3e6d932c4152ab'
CONTROLLER_SHA = '6fe3db82974cf536dd55917a6180215fad175db63cd7b0b44741dc2e8594f328'
PROTECTED = '01593d27c3a667a42b8736bba57c377e98dd6f19949f4601e2c08ef505abbf5f'


def require(condition, reason):
    if not condition:
        raise ValueError(reason)


def sha(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def terminal_stock(runtime, summary, snapshot, current, operation, stock_process_live):
    """Pure pre-send guard; the outer boundary binds the actual private files."""
    require(runtime.get('finished') is True and type(runtime.get('exit_code')) is int and
            runtime['exit_code'] == 0 and type(runtime.get('execute_attempts')) is int and runtime['execute_attempts'] == 1 and
            runtime.get('actual_twelve_job_terminal_success_verified') is True, 'stock acquisition not terminal-successful')
    require(not stock_process_live, 'stock acquisition child still live')
    require(summary.get('complete_stock_acquisition_run') is True and
            all(type(summary.get(k)) is int for k in ('completed_queries', 'selected_stock_operations', 'excluded_option_book_operations')) and
            summary.get('completed_queries') == 117 and summary.get('selected_stock_operations') == 126 and
            summary.get('excluded_option_book_operations') == 543 and len(summary.get('completed', [])) == 117,
            'full stock terminal summary required')
    require(len(snapshot.get('reservations', [])) == 707 and current == snapshot, 'terminal journal snapshot differs')
    require(type(current.get('version')) is int and current['version'] == 2 and current.get('authorization_mode') == 'uncapped' and
            type(current.get('lifetime_nano')) is int and current['lifetime_nano'] == 0, 'uncapped journal required')
    require(not any(e['operation_id'] == operation for e in current['reservations']),
            'operation already present; inspect existing evidence, never resubmit')
    protected = [e for e in current['reservations'] if e['operation_id'] == PROTECTED]
    require(len(protected) == 1 and protected[0]['status'] == 'reserved', 'unknown original hold not retained')


def load_controller():
    path = ROOT / 'scripts/acquire-prefetched-stock-plan-cycle74.py'
    require(sha(path) == CONTROLLER_SHA, 'accepted controller differs')
    spec = importlib.util.spec_from_file_location('accepted_stock_controller', path)
    module = importlib.util.module_from_spec(spec)
    original = sys.path[:]
    try:
        sys.path.insert(0, str(ROOT / 'scripts'))
        spec.loader.exec_module(module)
    finally:
        sys.path[:] = original
    return module


def child_live(runtime):
    path = Path('/proc') / str(runtime['stock_pid']) / 'cmdline'
    try:
        return any(Path(os.fsdecode(v)).name == 'acquire-prefetched-stock-plan-cycle74.py'
                   for v in path.read_bytes().split(b'\0') if v)
    except FileNotFoundError:
        return False


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--execute', action='store_true')
    args = parser.parse_args()
    require(args.execute, 'explicit --execute required; this purchases one historical window')
    controller = load_controller()
    controller.private_directory(BASE)
    controller.private_directory(ROOT)
    controller.live_writers_absent()
    locks = []
    created_output = False
    try:
        locks.append(controller.acquire_reserved_safe_lock(BASE))
        locks.append(controller.acquire_book_lock(BASE))
        controller.live_writers_absent()
        old = controller.load_old_controller()
        preflight = old.load_preflight()
        runtime = preflight.read_json(UPSTREAM / 'runtime.json')
        summary = preflight.read_json(STOCK / 'summary.json')
        snapshot_raw = preflight.private(STOCK / 'ledger-after.private.json')
        snapshot = preflight.read_json(STOCK / 'ledger-after.private.json')
        _, current = controller.ledger_snapshot(preflight, LEDGER)
        terminal_stock(runtime, summary, snapshot, current, OPERATION, child_live(runtime))
        require(sha(QUERY) == QUERY_SHA, 'declared query bytes differ')
        query = preflight.read_json(QUERY)
        wire = preflight.canonical_query(query)
        require(preflight.digest(wire) == QUERY_HASH and preflight.digest(wire + b'\0/batch.submit_job') == OPERATION,
                'declared canonical operation differs')
        require(len(query['symbols']) == 2000 and query['start'] == '2026-04-06T13:34:30Z' and
                query['end'] == '2026-04-06T13:35:00.000000001Z', 'declared full first-cut window differs')
        known = {e['path']: e['sha256'] for e in preflight.read_qualification(D / 'databento-qualification.json')['source_files']}
        for name in controller.BOOK_CODE:
            require(sha(ROOT / name) == known.get(name), 'accepted source differs')
        tools = {}
        for name, (relative, expected) in {**preflight.TOOLS, **controller.FILE_TOOLS}.items():
            path = BASE / relative
            require(path.resolve() == path and path.is_file() and sha(path) == expected, 'accepted binary differs')
            tools[name] = path
        tools['finalize'] = BASE / 'cycle52-autowheel-batch-finalize-20261007'
        require(sha(tools['finalize']) == '119eecab3d887a4fb3d99b03546e3adb8303a934f8828bc7ddbea4c0a9349736', 'bounded book finalizer differs')
        require(not OUT.exists() and os.statvfs(BASE).f_bavail * os.statvfs(BASE).f_frsize > (6 << 30) + (1 << 30),
                'fresh ext4 output and reserve required')
        production = json.loads(urllib.request.urlopen('http://127.0.0.1:9841/status', timeout=10).read())
        require(production['mode'] == 'read_only' and production['halted'], 'production must remain read-only/halted')
        OUT.mkdir(mode=0o700)
        created_output = True
        state = dict(pid=os.getpid(), phase='before_single_financial_send', automatic_retry=False,
                     execute_attempts=0, financial_retry=False, source_qualified=False,
                     operation_id=OPERATION, query_hash=QUERY_HASH, journal_writes_by_python=0)

        def update(phase, **values):
            state.update(phase=phase, **values)
            temp = OUT / 'runtime.next.json'
            preflight.save(temp, state)
            os.replace(temp, OUT / 'runtime.json')

        expected = current

        def check_journal():
            require(controller.ledger_snapshot(preflight, LEDGER)[1] == expected, 'unrelated journal mutation')
            require(sha(STOCK / 'ledger-after.private.json') == preflight.digest(snapshot_raw), 'immutable stock snapshot changed')

        preflight.save(OUT / 'ledger-before.private.json', current)
        preflight.save(OUT / 'intent.json', dict(operation_id=OPERATION, query_hash=QUERY_HASH, query_file_sha256=QUERY_SHA,
                       immutable_stock_snapshot_sha256=preflight.digest(snapshot_raw), original_entries=707,
                       actual_stock_terminal_success_verified=True, maximum_financial_attempts=1,
                       financial_retry=False, source_qualified=False, started_at=datetime.now(timezone.utc).isoformat()))
        update('single_financial_send', execute_attempts=1)
        result = preflight.run(tools['batch'], ['--query', QUERY, '--output', OUT / 'submit', '--key-file', KEY, '--submit'], OUT, 'submit')
        require(result == 0, 'financial attempt stopped; retain attempt and hold, never retry')
        ack = preflight.read_json(OUT / 'submit/ack.json')
        _, current = controller.ledger_snapshot(preflight, LEDGER)
        old.appended(expected, current, OPERATION, QUERY_HASH, ack,
                     preflight.read_json(OUT / 'submit/quote.json')['quote']['ResponseHash'],
                     preflight.private(OUT / 'submit/job.json'))
        expected = current
        common = ['--query', QUERY, '--ack', OUT / 'submit/ack.json', '--ack-body', OUT / 'submit/job.json', '--key-file', KEY]
        update('observing_acknowledged_prepaid_job', acknowledged=True)
        start = time.monotonic()
        poll = 0
        while True:
            require(time.monotonic() - start < 7200, 'prepaid job deadline; preserve existing job and hold')
            check_journal()
            receipt = old.observe_readonly(preflight, tools['retrieve'], common, OUT, 'observe-' + str(poll))
            check_journal()
            poll += 1
            if receipt['state'] == 'done':
                break
            require(receipt['state'] in ('queued', 'processing'), 'unknown prepaid state')
            time.sleep(20)
        plan_dir, plan = controller.observe_file_plan(preflight, tools['file_plan'], common, OUT, 'file-plan')
        total = controller.file_plan_total(plan, OPERATION, QUERY_HASH)
        require(total <= 1 << 30 and os.statvfs(BASE).f_bavail * os.statvfs(BASE).f_frsize > total + (6 << 30),
                'actual pilot file plan does not fit ext4 reserve; preserve prepaid job')
        check_journal()
        update('downloading_acknowledged_files')
        source = OUT / 'source'
        require(preflight.run(tools['planned_retrieve'], [*common, '--output', source, '--timeout', '1h',
                    '--planned-files', plan_dir / 'files-source.json'], OUT, 'download') == 0, 'download stopped; retain partial originals')
        controller.same_download_plan(plan, preflight.read_json(source / 'receipt.json'))
        check_journal()
        update('offline_native_count_and_acquisition_finalization')
        proof = OUT / 'proof.json'
        require(preflight.run(tools['finalize'], ['--query', QUERY, '--ack', OUT / 'submit/ack.json',
                    '--ack-body', OUT / 'submit/job.json', '--source', source, '--proof', proof,
                    '--python', PYTHON, '--ledger', LEDGER, '--timeout', '30m'], OUT, 'finalize') == 0,
                'native acquisition verification stopped; preserve spending hold')
        value = preflight.read_json(proof)
        _, current = controller.ledger_snapshot(preflight, LEDGER)
        old.finalized(expected, current, OPERATION, sha(proof), value['total_bytes'])
        expected = current
        check_journal()
        preflight.save(OUT / 'ledger-after.private.json', current)
        require(value['query_file_sha256'] == QUERY_SHA and value['query'] == query and
                value['native']['all_eof_verified'] and value['native']['records'] == value['provider_record_count'] and
                value['source_qualified'] is False, 'acquisition proof differs')
        preflight.save(OUT / 'summary.json', dict(schema='AutoWheelOneNativeBookWindowAcquisitionV1',
             operation_id=OPERATION, query_hash=QUERY_HASH, query_file_sha256=QUERY_SHA,
             proof_sha256=sha(proof), original_journal_entries_preserved=707, journal_entries=708,
             native_records=value['provider_record_count'], compressed_bytes=value['total_bytes'],
             financial_attempts=1, financial_retry=False, unknown_original_hold_preserved=True,
             source_qualified=False, actual_window_metadata_and_parity_verified=False,
             historical_performance_available=False, production_changed=False,
             immutable_stock_snapshot_unchanged=True, finished_at=datetime.now(timezone.utc).isoformat()))
        update('complete', finished=True, exit_code=0)
        print(json.dumps(dict(operation_id=OPERATION, native_records=value['provider_record_count'],
                             financial_attempts=1, source_qualified=False)))
    except Exception as error:
        if created_output:
            controller.load_old_controller().load_preflight().save(OUT / 'failure.json', dict(error_type=type(error).__name__,
                 reason=str(error) if isinstance(error, ValueError) else None,
                 traceback_locations=[dict(filename=Path(v.filename).name, line=v.lineno)
                     for v in traceback.extract_tb(error.__traceback__)[-3:]], automatic_retry=False,
                 financial_retry=False, existing_attempt_and_hold_preserved=True))
            if 'update' in locals():
                update('stopped_preserve_evidence', finished=True, exit_code=1)
        raise
    finally:
        for fd in reversed(locks):
            os.close(fd)


if __name__ == '__main__':
    os.umask(0o077)
    try:
        main()
    except Exception:
        raise SystemExit(1)
