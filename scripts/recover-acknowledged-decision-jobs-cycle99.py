"""Recover five remaining C97 ACKs after seven C98 finalizations without a submission tool or financial retry."""
import argparse
from copy import deepcopy
import importlib.util
import json
import os
from pathlib import Path
import sys
import time
import urllib.request

from batch_acquisition_proof import file_hash, read_json
from decision_window_metadata import save, save_raw
from research_batch_history import BatchHistory, DecisionPlan, acquired_book
from research_history_reader import read_qualification_manifest

BASE = Path('/home/yuduo/Projects/AutoWheel-research')
ROOT = BASE / 'source-afa3075'
PRIOR = BASE / 'cycle97-decision-acquisition-prefetch-20261008'
SOURCE_SHA = '2dd952691e996fd6d0e90d936780ea9e141ccf8f5fd0d6d33dc810fbf62c7c7c'
source = Path(__file__).with_name('acquire-prefetched-decision-window-plan.py')
assert file_hash(source) == SOURCE_SHA
spec = importlib.util.spec_from_file_location('preserved_decision_acquisition', source)
m = importlib.util.module_from_spec(spec);spec.loader.exec_module(m)


def resume(runner, original, target):
    """Bind existing money/ACK/body to private copies; never call submit."""
    runner.check()
    query = read_json(original / 'query.json');qh, operation = m.identities(query)
    runner.plan.window(query)
    entry = runner.history.by_operation[operation]
    m.require(entry['status'] == 'reserved', 'only the exact unresolved ACK may be recovered')
    acknowledgement = read_json(original / 'submit/ack.json')
    quote = read_json(original / 'submit/quote.json');body = runner.preflight.private(original / 'submit/job.json')
    before = deepcopy(runner.history.snapshot.value)
    index = next(i for i,row in enumerate(before['reservations']) if row['operation_id'] == operation)
    retained = before['reservations'].pop(index);after = deepcopy(before);after['reservations'].append(retained)
    runner.old.appended(before, after, operation, qh, acknowledgement, quote['quote']['ResponseHash'], body)
    intent = read_json(original / 'financial-intent.json')
    m.require(intent['operation_id'] == operation and intent['query_hash'] == qh
        and intent['query_file_sha256'] == file_hash(original / 'query.json')
        and intent['metadata_receipt_sha256'] == file_hash(original / 'window-metadata/receipt.json')
        and type(intent['admission_bytes']) is int and 0 < intent['admission_bytes'] <= 64 << 30,
        'original durable financial intent differs')
    m.require(operation not in runner.pending, 'existing ACK cannot be queued twice')
    target.mkdir(mode=0o700);(target / 'submit').mkdir(mode=0o700);(target / 'window-metadata').mkdir(mode=0o700)
    names = ['query.json','financial-intent.json','submit/ack.json','submit/quote.json','submit/job.json','window-metadata/receipt.json']
    for name in names:
        raw = runner.preflight.private(original / name);save_raw(target / name, raw)
        m.require(file_hash(target / name) == file_hash(original / name), 'retained ACK copy differs')
    held = dict(operation=operation, query_hash=qh, query=query, directory=target,
        acknowledgement=acknowledgement, common=['--query',target / 'query.json','--ack',target / 'submit/ack.json',
            '--ack-body',target / 'submit/job.json','--key-file',m.KEY], allowance=intent['admission_bytes'],
        started=time.monotonic(), polls=0, done_receipt=None,
        pins={str(target / name):file_hash(target / name) for name in names[:5]})
    runner.pending[operation] = held;runner.peak_pending = max(runner.peak_pending, len(runner.pending))
    runner.held(held);return held


def production():
    value = json.loads(urllib.request.urlopen('http://127.0.0.1:9841/status', timeout=10).read())
    m.require(value['mode'] == 'read_only' and value['halted'], 'production must remain read_only/halted')


def main():
    parser = argparse.ArgumentParser();parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--bindings-sha', required=True);args = parser.parse_args()
    bindings_path = ROOT / 'research/simulation/2026-10-05/cycle99-recovery-code-bindings.json'
    m.require(file_hash(bindings_path) == args.bindings_sha, 'source recovery bindings differ')
    bindings = read_json(bindings_path)
    for name, digest in bindings.items():
        m.require(file_hash(ROOT / name) == digest, 'source-bound recovery code differs')
    m.output_scope(args.output);production()
    for path in [PRIOR / 'runtime.json', BASE / 'cycle97-prefetch-controller-component-20261008/runtime.json',
                 BASE / 'cycle98-exact-job-recovery-component-20261008/runtime.json',
                 BASE / 'cycle98-decision-acquisition-recovery-20261008/runtime.json']:
        runtime = read_json(path);m.require(runtime['finished'] and runtime['exit_code'] == 1, 'original attempt must remain stopped')
        process = Path('/proc') / str(runtime['pid']) / 'cmdline'
        m.require(not process.exists() or not process.read_bytes(), 'original attempt still live')
    controller = m.load(ROOT / 'scripts/acquire-prefetched-stock-plan-cycle74.py', 'recovery_lock_controller', m.CONTROLLER_SHA)
    controller.live_writers_absent();locks = [];runner = None
    try:
        locks.append(controller.acquire_reserved_safe_lock(BASE));locks.append(controller.acquire_book_lock(BASE))
        controller.live_writers_absent()
        history, original = BatchHistory(m.LEDGER), BatchHistory(m.ORIGINAL)
        prior_before = BatchHistory(PRIOR / 'ledger-before.private.json')
        m.require(len(history.by_operation) == 727 and len(prior_before.by_operation) == 715
            and history.snapshot.value['reservations'][:715] == prior_before.snapshot.value['reservations'],
            'exact original727-row recovery journal required')
        plan = DecisionPlan(m.PLAN);known = {v['path']:v['sha256'] for v in read_qualification_manifest(m.D / 'databento-qualification.json').value['source_files']}
        receipt = read_json(m.TOOLS_RECEIPT);m.require(file_hash(m.TOOLS_RECEIPT) == m.TOOLS_SHA, 'candidate tools receipt differs')
        tools = {}
        for item in receipt['tools']:
            path = Path(item['path']);m.require(file_hash(path) == item['sha256'] == known.get(str(path)), 'qualified source tool differs')
            if item['name'] != 'autowheel-batch':
                tools[item['name']] = path, item['sha256']
        m.require(len(tools) == 4 and 'autowheel-batch' not in tools, 'recovery must not possess a submission tool')
        all_directories = sorted(p.parent.parent for p in PRIOR.glob('window-*/submit/ack.json'))
        m.require(len(all_directories) == 12, 'exact twelve original acknowledgements required')
        original_operations = {m.identities(read_json(path / 'query.json'))[1] for path in all_directories}
        m.require(original_operations == {v['operation_id'] for v in history.snapshot.value['reservations'][715:]}, 'exact original twelve operations required')
        existing = [];seen = set()
        for path in sorted((BASE / 'cycle98-decision-acquisition-recovery-20261008').glob('original-ack-*/proof.json')):
            book = acquired_book(path, history, ROOT, plan=plan)
            m.require(book.entry['operation_id'] in original_operations and book.entry['operation_id'] not in seen, 'existing finalized source differs')
            seen.add(book.entry['operation_id'])
            existing.append(dict(status='acquired',operation_id=book.entry['operation_id'],proof_path=str(path),proof_sha256=file_hash(path)))
        m.require(len(existing) == 7 and seen == {v['operation_id'] for v in history.snapshot.value['reservations'][715:] if v['status']=='acquired'}, 'exact seven existing finalized sources required')
        directories = [path for path in all_directories if history.by_operation[m.identities(read_json(path / 'query.json'))[1]]['status']=='reserved']
        m.require(len(directories) == 5, 'exact five remaining acknowledged holds required')
        args.output.mkdir(mode=0o700);save_raw(args.output / 'ledger-before.private.json', history.snapshot.raw)
        preflight = controller.load_old_controller().load_preflight()
        runner = m.Runner(args.output, plan, original, history, controller, preflight, tools)
        held = [resume(runner, path, args.output / f'original-ack-{index:03d}') for index,path in enumerate(directories)]
        m.require({v['operation'] for v in held} == {v['operation_id'] for v in history.snapshot.value['reservations'][715:] if v['status']=='reserved'}, 'unrelated held operation cannot be recovered')
        fsroot, fsext = os.statvfs(BASE), os.statvfs(controller.BULK_VIEW)
        m.require(fsroot.f_bavail * fsroot.f_frsize >= m.ROOT_RESERVE
            and fsext.f_bavail * fsext.f_frsize >= runner.pending_bytes() + m.EXTERNAL_RESERVE, 'prepaid recovery storage reserve unavailable')
        save(args.output / 'intent.json', dict(original_attempt=str(PRIOR), original_acknowledgements=12,remaining_original_acknowledgements=5,reused_existing_sources=existing,
            operations=[v['operation'] for v in held], financial_requests=0, financial_retry=False,
            submission_tool_available=False, original727_journal_sha256=history.snapshot.sha256))
        for index, value in enumerate(held):
            result = runner.finish(value);runner.record_result(index, result)
        runner.check();production()
        save_raw(args.output / 'ledger-after.private.json', runner.history.snapshot.raw)
        save(args.output / 'summary.json', dict(finished=True, exit_code=0, results=runner.results, reused_existing_sources=existing,
            financial_requests=0, financial_retry=False, prior_financial_attempts=12,
            original715_financial_fields_and_unknown_hold_preserved=True, original_acknowledgements_preserved=True,
            ending_journal_entries=len(runner.history.by_operation), source_gate_passed=False,
            historical_performance_available=False, production_read_only_halted=True))
        runner.runtime('complete', finished=True, exit_code=0)
    except Exception as error:
        if runner is not None:
            save(args.output / 'failure.json', dict(error_type=type(error).__name__, reason=str(error) if isinstance(error, ValueError) else None,
                financial_requests=0, financial_retry=False, original_holds_and_sources_preserved=True))
            runner.runtime('stopped_preserve_evidence', finished=True, exit_code=1)
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
