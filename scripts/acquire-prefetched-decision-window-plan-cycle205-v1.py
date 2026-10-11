"""Finite bounded prefetch of the accepted complete decision plan.

Use candidate large-journal Go CLIs and the unchanged financial/proof checks.
One whole-run financial snapshot before/after; no per-query journal copies.
Keep zero-window spending holds and use a distinct original full-day query.
Queue up to twelve jobs by default, but serialize every financial send,
download, seal and native finalization. Retain the original serial controller.
This acquires originals only: full execution sources and all gates stay pending.
"""
import argparse
from collections import Counter
from datetime import datetime, timedelta, timezone
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import time
import traceback

from batch_acquisition_proof import canonical_query, file_hash, job_matches, read_json
from decision_window_metadata import save, save_raw
from decision_window_metadata_retry import fetch
from research_batch_history import BatchHistory, DecisionPlan, PROTECTED
from research_history_reader import read_qualification_manifest

BASE = Path('/home/yuduo/Projects/AutoWheel-research')
ROOT = BASE / 'source-afa3075'
D = ROOT / 'research/simulation/2026-10-05'
LEDGER = Path('/home/yuduo/.local/share/autowheel-research/databento-budget.json')
KEY = Path('/home/yuduo/.config/autowheel/databento-pilot.key')
PYTHON = BASE / 'dbn-env-0.71.0/bin/python'
ORIGINAL = BASE / 'cycle88-one-book-window-acquisition-20261008/ledger-after.private.json'
PLAN = BASE / 'cycle91-decision-plan-reader-checks-20261008/decision-plan/decision-plan.json.gz'
TOOLS_RECEIPT = BASE / 'cycle115-quote-recovery-tools-20261008/receipt.json'
TOOLS_SHA = '0a5f9b95d84822aeeb8f1deb0ad6ee575c2ca1cc81c890d0270bdac1683e6a52'
ORIGINAL_SHA = 'b24e80323d69fc56d58539b5d335e27db1ee65e45e1ebd477c7892a5ed25b902'
CONTROLLER_SHA = '6fe3db82974cf536dd55917a6180215fad175db63cd7b0b44741dc2e8594f328'
SEALER_SHA = '8c19a8b911bd95f0d1a12ab5cdcf52e7095a543e93aad53eb6ff36b589fd0040'
ROOT_RESERVE = 6 << 30
EXTERNAL_RESERVE = 16 << 30


def require(condition, reason):
    if not condition:
        raise ValueError(reason)


def load(path, name, digest):
    require(path.resolve() == path and file_hash(path) == digest, 'pinned acquisition helper differs')
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
    return module


def timestamp(ns):
    require(type(ns) is int and ns >= 0, 'integer query clock required')
    seconds, fraction = divmod(ns, 1_000_000_000)
    value = (datetime(1970, 1, 1, tzinfo=timezone.utc) + timedelta(seconds=seconds)).strftime('%Y-%m-%dT%H:%M:%S')
    return value + ('.' + f'{fraction:09d}' if fraction else '') + 'Z'


def identities(query):
    wire = canonical_query(query)
    return hashlib.sha256(wire).hexdigest(), hashlib.sha256(wire + b'\0/batch.submit_job').hexdigest()


def query_for(plan, row):
    part = plan.value['partitions'][row[0]];path = Path(part['query_path'])
    require(file_hash(path) == part['query_sha256'], 'original partition query changed')
    full = read_json(path);qh, op = identities(full)
    require((qh, op) == (part['full_query_hash'], part['full_operation_id']), 'original full-day identity differs')
    require(len(full['symbols']) == part['symbols'], 'complete original raw symbol roster required')
    window = dict(full, start=timestamp(row[3]), end=timestamp(row[4]))
    require(plan.window(window) == row, 'accepted exact window identity differs')
    return part, full, window


def action(history, row, part):
    window = history.by_operation.get(row[6]);full = history.by_operation.get(part['full_operation_id'])
    for entry, digest in ((window, row[5]), (full, part['full_query_hash'])):
        if entry is not None:
            require(entry['endpoint'] == '/batch.submit_job' and entry['query_hash'] == digest,
                    'existing financial identity differs')
    if window is not None and window['status'] == 'acquired':
        return 'reuse_acquired_window'
    if full is not None and full['status'] == 'acquired':
        return 'reuse_acquired_full_day'
    if window is not None:
        return 'protected_existing_window'
    require(row[7] != 'reuse_acquired_full_day', 'original reused full-day acquisition disappeared')
    require(row[7] == 'fresh_not_submitted', 'acquired plan operation cannot become fresh')
    history.fresh(row[6])
    return 'fresh'


def admission(history, metadata, root_free, external_free):
    require(len(history.by_operation) < 100000 - 128 and len(history.snapshot.raw) < (64 << 20) - (1 << 20),
            'bounded journal financial headroom unavailable')
    count, size = metadata['record_count'], metadata['billable_bytes']
    require(type(count) is int and type(size) is int and 0 <= count < (1 << 63) and 0 <= size < (1 << 63),
            'bounded admission metadata required')
    if count == 0:
        return 'full_day_fallback_required', 0
    # Conservative pre-send allowance. Actual file-plan totals are checked again
    # after ACK. This is an admission estimate, not proof of whole-study capacity.
    allowance = max(size * 2, count * 96) + (32 << 20)
    require(allowance <= 64 << 30, 'single-source admission bound exceeded')
    require(root_free >= ROOT_RESERVE and external_free >= allowance + EXTERNAL_RESERVE,
            'source admission would consume a storage reserve')
    return 'positive_window_admitted', allowance



def storage_source_admitted(duration_ns, allowance, total=None, native_files=None):
    require(type(duration_ns) is int and 0 < duration_ns <= 30_000_000_001,
            'this finite wave requires exact decision windows; retain any full-day fallback lead')
    require(type(allowance) is int and 0 < allowance <= 2 << 30,
            'finite source allowance exceeded; no billable request authorized by this wave')
    if total is not None:
        require(type(total) is int and 0 < total <= 2 << 30 and type(native_files) is int and native_files == 1,
                'actual file plan exceeds finite native/vector allowance; preserve prepaid job')


def output_scope(path):
    require(path.is_absolute() and path.parent == BASE and path.resolve() == path
            and re.fullmatch(r'cycle[0-9]+-decision-acquisition-[A-Za-z0-9._-]+', path.name)
            and len(path.name) <= 100 and not path.exists(), 'fresh finite research output required')


class Runner:
    def __init__(self, output, plan, original, history, controller, preflight, tools):
        self.output, self.plan, self.original, self.history = output, plan, original, history
        self.controller, self.preflight, self.tools = controller, preflight, tools
        self.old = controller.load_old_controller()
        self.financial_attempts = 0;self.results = [];self.deadline = time.monotonic() + 86400
        self.pending = {};self.last_submit = None;self.max_pending = 12;self.peak_pending = 0

    def runtime(self, phase, **values):
        state = dict(pid=os.getpid(), phase=phase, financial_attempts=self.financial_attempts,
                     completed_windows=len(self.results), automatic_financial_retry=False,
                     pending_jobs=len(self.pending), pending_admission_bytes=self.pending_bytes(),
                     maximum_observed_pending_jobs=self.peak_pending,
                     observed_at=datetime.now(timezone.utc).isoformat(), source_qualified=False, finished=False)
        state.update(values)
        temporary = self.output / 'runtime.next.json';save(temporary, state)
        os.replace(temporary, self.output / 'runtime.json')
        fd = os.open(self.output, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)

    def check(self):
        require(time.monotonic() < self.deadline, 'finite controller deadline; preserve all existing jobs and holds')
        current = BatchHistory(LEDGER)
        require(current.snapshot.value == self.history.snapshot.value, 'unrelated financial journal mutation')
        require(file_hash(ORIGINAL) == ORIGINAL_SHA, 'original immutable708 snapshot changed')
        require(current.snapshot.value['reservations'][:708] == self.original.snapshot.value['reservations'],
                'original708 financial fields changed')
        require(current.by_operation[PROTECTED] == self.original.by_operation[PROTECTED], 'unknown original hold changed')
        self.plan.unchanged()
        return current

    def call(self, name, args, directory, phase):
        path, digest = self.tools[name]
        require(file_hash(path) == digest, 'source-bound candidate tool changed')
        result = self.preflight.run(path, args, directory, phase)
        require(result == 0, 'acquisition phase stopped; retain attempt and hold without retry')

    def free(self, query, directory):
        self.check();directory.mkdir(mode=0o700)
        result = fetch(query, directory)
        self.check()
        return result

    def pending_bytes(self, exclude=None):
        return sum(value['allowance'] for key, value in self.pending.items() if key != exclude)

    def submit(self, query, directory, label, metadata):
        require(len(self.pending) < self.max_pending, 'bounded pending-job ceiling reached')
        query_hash, operation = identities(query)
        self.history.fresh(operation);self.check()
        fsroot, fsext = os.statvfs(BASE), os.statvfs(self.controller.BULK_VIEW)
        disposition, allowance = admission(self.history, metadata, fsroot.f_bavail * fsroot.f_frsize,
                                           fsext.f_bavail * fsext.f_frsize - self.pending_bytes())
        storage_source_admitted(self.plan.window(query)[4] - self.plan.window(query)[3], allowance)
        require(disposition == 'positive_window_admitted', 'positive source metadata required before financial send')
        query_path = directory / 'query.json';save(query_path, query)
        save(directory / 'financial-intent.json', dict(operation_id=operation, query_hash=query_hash,
            query_file_sha256=file_hash(query_path), financial_attempts=1, automatic_retry=False,
            journal_sha256_before=self.history.snapshot.sha256, admission_bytes=allowance,
            other_pending_admission_bytes=self.pending_bytes(),
            metadata_receipt_sha256=file_hash(directory / (label + '-metadata') / 'receipt.json'),
            complete_study_capacity_proven=False, source_qualified=False))
        # The immutable fsynced intent precedes the sole possible submit call.
        if self.last_submit is not None:
            remaining = 4 - (time.monotonic() - self.last_submit)
            if remaining > 0:
                time.sleep(remaining)
        self.check()
        fsroot, fsext = os.statvfs(BASE), os.statvfs(self.controller.BULK_VIEW)
        admission(self.history, metadata, fsroot.f_bavail * fsroot.f_frsize,
                  fsext.f_bavail * fsext.f_frsize - self.pending_bytes())
        self.financial_attempts += 1
        self.last_submit = time.monotonic()
        self.runtime('single_financial_send', operation_id=operation)
        self.call('autowheel-batch', ['--query', query_path, '--output', directory / 'submit',
                  '--key-file', KEY, '--submit'], directory, 'submit')
        acknowledgement = read_json(directory / 'submit/ack.json')
        current = BatchHistory(LEDGER)
        self.old.appended(self.history.snapshot.value, current.snapshot.value, operation, query_hash,
            acknowledgement, read_json(directory / 'submit/quote.json')['quote']['ResponseHash'],
            self.preflight.private(directory / 'submit/job.json'))
        self.history = current;self.check()
        common = ['--query', query_path, '--ack', directory / 'submit/ack.json',
                  '--ack-body', directory / 'submit/job.json', '--key-file', KEY]
        held = dict(operation=operation, query_hash=query_hash, query=query, directory=directory,
                    acknowledgement=acknowledgement, common=common, allowance=allowance,
                    started=time.monotonic(), polls=0, done_receipt=None,
                    pins={str(path):file_hash(path) for path in [query_path,
                        directory / 'financial-intent.json', directory / 'submit/ack.json',
                        directory / 'submit/quote.json', directory / 'submit/job.json']})
        self.pending[operation] = held
        self.peak_pending = max(self.peak_pending, len(self.pending))
        self.runtime('acknowledged_job_queued', operation_id=operation, job_id=acknowledgement['job_id'])
        return held

    def held(self, held):
        operation = held['operation']
        require(self.pending.get(operation) is held, 'exact pending acknowledgement required')
        self.check()
        require(all(file_hash(Path(path)) == digest for path, digest in held['pins'].items()),
                'held query, intent or acknowledgement changed')
        query = read_json(held['directory'] / 'query.json')
        require(query == held['query'] and identities(query) == (held['query_hash'], operation)
                and self.history.by_operation[operation]['status'] == 'reserved', 'held identity changed')

    def observe(self, held):
        self.held(held)
        require(time.monotonic() - held['started'] < 7200, 'acknowledged job observation deadline')
        directory, common = held['directory'], held['common']
        receipt = self.old.observe_readonly(self.preflight, self.tools['autowheel-batch-retrieve'][0],
                                           common, directory, 'observe-' + str(held['polls']))
        held['polls'] += 1;self.held(held)
        observations = [p.parent for p in directory.glob('observe-*-read-*/receipt.json')
                        if read_json(p) == receipt]
        require(len(observations) == 1, 'exact acknowledged job observation required')
        job_path = observations[0] / 'job-source.json';job = read_json(job_path)
        require(file_hash(job_path) == receipt['job_sha256'] and job['state'] == receipt['state']
                and job_matches(job, held['query'], held['acknowledgement']['job_id']),
                'acknowledged job scope changed')
        if receipt['state'] == 'done':
            held['done_receipt'] = receipt
        return receipt

    def acquire(self, query, directory, label, metadata):
        return self.finish(self.submit(query, directory, label, metadata))

    def finish(self, held):
        self.held(held)
        query, directory, query_hash, operation = (held[key] for key in ['query','directory','query_hash','operation'])
        acknowledgement, common = held['acknowledgement'], held['common']
        query_path = directory / 'query.json'
        self.runtime('observing_acknowledged_job', operation_id=operation, job_id=acknowledgement['job_id'])
        receipt = held['done_receipt']
        while receipt is None:
            receipt = self.observe(held)
            if receipt['state'] == 'done':
                break
            require(receipt['state'] in ('queued', 'processing'), 'acknowledged job state unknown')
            time.sleep(20);receipt = None
        observations = [p.parent for p in directory.glob('observe-*-read-*/receipt.json')
                        if read_json(p) == receipt]
        require(len(observations) == 1, 'exact terminal job observation required')
        job_path = observations[0] / 'job-source.json';job = read_json(job_path)
        require(file_hash(job_path) == receipt['job_sha256'] and job['id'] == acknowledgement['job_id']
                and job_matches(job, query, acknowledgement['job_id'], done=True)
                and type(job.get('record_count')) is int and job['record_count'] >= 0,
                'actual completed job record count required')
        if job['record_count'] == 0:
            save(directory / 'zero-job-observation.json', dict(operation_id=operation, query_hash=query_hash,
                original_job_path=str(job_path), job_sha256=file_hash(job_path),
                receipt_sha256=file_hash(observations[0] / 'receipt.json'), acknowledged_job_done=True,
                provider_record_count=0, native_absence_proven=False, original_hold_retained=True,
                financial_source_usable=False, source_qualified=False))
            self.check()
            del self.pending[operation]
            return dict(status='zero_job_hold_retained', operation_id=operation, original_job_path=str(job_path))
        self.runtime('inspecting_actual_file_plan', operation_id=operation)
        file_dir, file_plan = self.controller.observe_file_plan(self.preflight,
            self.tools['autowheel-batch-files'][0], common, directory, 'file-plan')
        total = self.controller.file_plan_total(file_plan, operation, query_hash)
        storage_source_admitted(self.plan.window(query)[4] - self.plan.window(query)[3], held['allowance'],
            total, sum(v['filename'].endswith('.dbn.zst') for v in file_plan['files']))
        fsroot, fsext = os.statvfs(BASE), os.statvfs(self.controller.BULK_VIEW)
        require(0 < total <= 64 << 30 and fsroot.f_bavail * fsroot.f_frsize >= ROOT_RESERVE
                and fsext.f_bavail * fsext.f_frsize >= total + self.pending_bytes(exclude=operation) + EXTERNAL_RESERVE,
                'actual acknowledged file plan exceeds storage capacity; preserve prepaid job')
        self.check()
        source = self.controller.BULK_VIEW / (self.output.name + '-retrieve-' + operation[:20])
        require(not source.exists() and not (BASE / source.name).exists(), 'fresh source namespace required')
        self.call('autowheel-batch-planned-retrieve', [*common, '--output', source, '--timeout', '1h',
                  '--planned-files', file_dir / 'files-source.json'], directory, 'download')
        self.controller.same_download_plan(file_plan, read_json(source / 'receipt.json'));self.check()
        sealer = ROOT / 'scripts/seal-external-research-source-scoped.py'
        require(file_hash(sealer) == SEALER_SHA, 'qualified scoped source sealer differs')
        self.runtime('sealing_original_download', operation_id=operation)
        require(self.preflight.run(PYTHON, [sealer, '--source', source, '--output', directory / 'seal.json'],
                                   directory, 'seal') == 0, 'external seal stopped; keep downloaded originals')
        seal = read_json(directory / 'seal.json');source = Path(seal['source_directory'])
        require(source.is_mount() and seal['read_only'] and seal['source_bytes_preserved'], 'read-only sealed source required')
        downloaded_job = read_json(source / 'job-source.json')
        require(job_matches(downloaded_job, query, acknowledgement['job_id'], done=True)
                and downloaded_job.get('record_count') == job['record_count'],
                'downloaded terminal job scope or count changed')
        proof_path = directory / 'proof.json'
        self.runtime('offline_native_finalization', operation_id=operation)
        self.call('autowheel-batch-finalize', ['--query', query_path, '--ack', directory / 'submit/ack.json',
            '--ack-body', directory / 'submit/job.json', '--source', source, '--proof', proof_path,
            '--python', PYTHON, '--ledger', LEDGER, '--timeout', '30m'], directory, 'finalize')
        proof = read_json(proof_path);current = BatchHistory(LEDGER)
        self.old.finalized(self.history.snapshot.value, current.snapshot.value, operation, file_hash(proof_path), proof['total_bytes'])
        self.history = current;self.check()
        require(proof['query'] == query and proof['query_file_sha256'] == file_hash(query_path)
                and proof['native']['all_eof_verified'] and proof['native']['records'] == job['record_count']
                and proof['provider_record_count'] == job['record_count'] and not proof['source_qualified'],
                'unchanged native acquisition proof differs')
        del self.pending[operation]
        return dict(status='acquired', operation_id=operation, proof_path=str(proof_path),
                    proof_sha256=file_hash(proof_path), native_records=job['record_count'])

    def window(self, row, index):
        part, full, query = query_for(self.plan, row)
        self.check();disposition = action(self.history, row, part)
        if disposition != 'fresh':
            return dict(window_operation_id=row[6], status=disposition)
        directory = self.output / f'window-{index:06d}';directory.mkdir(mode=0o700)
        metadata = self.free(query, directory / 'window-metadata')
        count = metadata['record_count']
        if count > 0:
            acquired = self.acquire(query, directory, 'window', metadata)
            if acquired['status'] == 'acquired':
                return dict(window_operation_id=row[6], **acquired)
        else:
            save(directory / 'zero-metadata-observation.json', dict(window_operation_id=row[6],
                metadata_sha256=file_hash(directory / 'window-metadata/receipt.json'),
                financial_requests=0, native_absence_proven=False, source_qualified=False))
        return self.fallback(row, part, full, directory, count)

    def fallback(self, row, part, full, directory, count):
        self.check()
        # Separate, globally deduplicated original full UTC day; never resend
        # or complete a zero/ambiguous window's held operation.
        full_op = part['full_operation_id'];existing = self.history.by_operation.get(full_op)
        if existing is not None:
            require(existing['status'] == 'acquired' and existing['query_hash'] == part['full_query_hash'],
                    'full-day fallback already attempted and unresolved; never retry')
            return dict(window_operation_id=row[6], status='reuse_acquired_full_day', source_operation_id=full_op)
        fallback = directory / 'full-day-fallback';fallback.mkdir(mode=0o700)
        full_metadata = self.free(full, fallback / 'full-day-metadata')
        require(full_metadata['record_count'] > 0, 'full-day source metadata empty; no invented source absence')
        acquired = self.acquire(full, fallback, 'full-day', full_metadata)
        require(acquired['status'] == 'acquired', 'zero full-day fallback hold retained; source coverage unresolved')
        return dict(window_operation_id=row[6], status='full_day_fallback_acquired', source=acquired,
                    zero_window_hold_retained=count > 0, source_qualified=False)

    def record_result(self, index, value):
        save(self.output / f'window-result-{index:06d}.json', value)
        self.results.append(value)

    def prefetched(self, rows):
        next_index = 0;work = {}
        while next_index < len(rows) or work:
            self.check()
            while next_index < len(rows) and len(self.pending) < self.max_pending:
                index = next_index;row = rows[index];next_index += 1
                part, full, query = query_for(self.plan, row)
                disposition = action(self.history, row, part)
                if disposition != 'fresh':
                    source_operation = part['full_operation_id'] if disposition == 'reuse_acquired_full_day' else row[6]
                    self.record_result(index, dict(window_operation_id=row[6], status=disposition,
                                                   source_operation_id=source_operation));continue
                directory = self.output / f'window-{index:06d}';directory.mkdir(mode=0o700)
                metadata = self.free(query, directory / 'window-metadata')
                if metadata['record_count'] == 0:
                    save(directory / 'zero-metadata-observation.json', dict(window_operation_id=row[6],
                        metadata_sha256=file_hash(directory / 'window-metadata/receipt.json'),
                        financial_requests=0, native_absence_proven=False, source_qualified=False))
                    self.record_result(index, self.fallback(row, part, full, directory, 0));continue
                held = self.submit(query, directory, 'window', metadata)
                work[held['operation']] = dict(held=held, row=row, part=part, full=full, index=index)
            completed = False
            for operation, item in list(work.items()):
                held = item['held']
                if self.observe(held)['state'] != 'done':
                    continue
                acquired = self.finish(held)
                if acquired['status'] == 'zero_job_hold_retained':
                    value = self.fallback(item['row'], item['part'], item['full'], held['directory'], 1)
                else:
                    value = dict(window_operation_id=item['row'][6], **acquired)
                self.record_result(item['index'], value);del work[operation];completed = True
            if work and not completed:
                self.runtime('waiting_for_prefetched_jobs');time.sleep(20)
        require(not self.pending, 'all admitted jobs must have explicit terminal outcomes')



def exact_C205_scope(fresh,ledger_sha,qualification_sha,admitted):
    require(ledger_sha=='265298122b10e33464ccd0b39f5cc3bedc79edaee50e8e3552043c009ad1f64a','C205 starting journal changed')
    require(qualification_sha=='7f2949c24107bbcfcd0d6ed051ee9e916b2b5a200b173203f5f162f2b82a245c','C205 qualification checkpoint changed')
    require(admitted['schema']=='AutoWheelTwelveUnsubmittedWindowAdmissionV2' and admitted['all_twelve_admitted'] is True,
            'C205 actual read-only admission receipt required')
    results=admitted['results'];require(len(results)==len(fresh)==12,'C205 exact twelve-window scope required')
    ids=[r['operation_id'] for r in results]
    require(len(set(ids))==12 and [r[6] for r in fresh]==ids,'C205 selected operations changed')
    for row,result in zip(fresh,results):
        require(row[0]==result['partition_index'] and row[1]==result['cut_index'] and row[2]==result['cut_ns']
                and row[5]==result['query_hash'],'C205 frozen cut or query identity changed')
        metadata=result['metadata']
        require(result['admitted'] is True and type(metadata['financial_requests']) is int and metadata['financial_requests']==0
                and metadata['operation_id']==row[6] and metadata['query_hash']==row[5]
                and type(metadata['record_count']) is int and metadata['record_count']>0
                and type(result['admission_bytes']) is int and 0<result['admission_bytes']<=2<<30,
                'C205 finite read-only admission changed')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--max-windows', type=int, default=16)
    parser.add_argument('--max-pending', type=int, default=12)
    parser.add_argument('--execute', action='store_true')
    args = parser.parse_args()
    require(1 <= args.max_windows <= 16, 'finite window ceiling required')
    require(1 <= args.max_pending <= 2, 'bounded prefetch ceiling required')
    output_scope(args.output)
    controller = load(ROOT / 'scripts/acquire-prefetched-stock-plan-cycle74.py', 'frozen_stock_controller', CONTROLLER_SHA)
    controller.private_directory(BASE);controller.private_directory(ROOT);controller.live_writers_absent()
    locks = [];runner = None
    try:
        locks += [controller.acquire_reserved_safe_lock(BASE)]
        locks += [controller.acquire_book_lock(BASE)]
        controller.live_writers_absent()
        original, history, plan = BatchHistory(ORIGINAL), BatchHistory(LEDGER), DecisionPlan(PLAN)
        require(original.snapshot.sha256 == ORIGINAL_SHA and len(original.by_operation) == 708,
                'original immutable708 snapshot required')
        require(history.snapshot.value['reservations'][:708] == original.snapshot.value['reservations']
                and history.by_operation[PROTECTED] == original.by_operation[PROTECTED], 'original financial fields or hold changed')
        qualification = read_qualification_manifest(D / 'databento-qualification.json')
        known = {v['path']: v['sha256'] for v in qualification.value['source_files']}
        for name in controller.BOOK_CODE + ['scripts/research_batch_history.py', 'scripts/research_history_reader.py',
                     'scripts/research_source_quiescence.py', 'scripts/seal-external-research-source-scoped.py']:
            require(file_hash(ROOT / name) == known.get(name), 'manifested original or integrated source differs')
        require(file_hash(TOOLS_RECEIPT) == TOOLS_SHA, 'candidate tools receipt differs')
        receipt = read_json(TOOLS_RECEIPT);tools = {}
        require(receipt['journal_size_component_accepted'] and receipt['uncapped_file_bytes'] == 64 << 20
                and receipt['maximum_entries'] == 100000 and len(receipt['tools']) == 5, 'qualified bounded tools required')
        for tool in receipt['tools']:
            path = Path(tool['path']);info = path.lstat()
            require(path.resolve() == path and stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid()
                    and info.st_nlink == 1 and stat.S_IMODE(info.st_mode) == 0o700
                    and file_hash(path) == tool['sha256'] == known.get(str(path)), 'manifested private candidate binary differs')
            require(tool['name'] not in tools, 'duplicate candidate tool')
            tools[tool['name']] = path, tool['sha256']
        require(set(tools) == {'autowheel-batch', 'autowheel-batch-finalize', 'autowheel-batch-files',
                              'autowheel-batch-retrieve', 'autowheel-batch-planned-retrieve'}, 'complete candidate tool names required')
        upstream = read_json(BASE / 'cycle74-stock-acquisition-supervisor-20261008/runtime.json')
        require(upstream['finished'] and upstream['exit_code'] == 0 and upstream['execute_attempts'] == 1,
                'original stock acquisition must remain terminal')
        sealer = load(ROOT / 'scripts/seal-external-research-source-scoped.py', 'scoped_source_sealer', SEALER_SHA)
        stored = sealer.load_original();stored.lib = stored.load(stored.LIBRARY, 'source_storage_library', stored.LIBRARY_SHA)
        stored.verify_parent()
        preflight = controller.load_old_controller().load_preflight()
        fresh, counts = [], Counter()
        for row in plan.value['windows']:
            part = plan.value['partitions'][row[0]];kind = action(history, row, part);counts[kind] += 1
            if kind == 'fresh' and len(fresh) < args.max_windows:
                query_for(plan, row);fresh.append(row)
        history.unchanged();original.unchanged();plan.unchanged()
        admitted_path=BASE/'cycle203-twelve-unsent-window-admission-v2-20261009/receipt.json'
        require(file_hash(admitted_path)=='54f1baf7ff0bb19024097095e56ca68c9b0a01dec7863317cb300ce379594254',
                'C205 immutable admission receipt changed')
        require(file_hash(ROOT/'scripts/acquire-prefetched-decision-window-plan-storage-cycle136.py')==
                'db28500455c3d895f36dbcf4188c811b45bfc5bbd2f0765afdcc59fa62fd387e','accepted parent changed')
        exact_C205_scope(fresh,history.snapshot.sha256,qualification.sha256,read_json(admitted_path))
        args.output.mkdir(mode=0o700)
        save_raw(args.output / 'ledger-before.private.json', history.snapshot.raw)
        save(args.output / 'intent.json', dict(schema='AutoWheelFiniteDecisionAcquisitionIntentV1',
            plan_sha256=plan.sha256, qualification_sha256=qualification.sha256, initial_journal_sha256=history.snapshot.sha256,
            maximum_windows=args.max_windows, maximum_financial_attempts=2 * len(fresh), execute=args.execute,
            maximum_pending_jobs=args.max_pending, minimum_submit_spacing_seconds=4,
            selected_window_operations=[v[6] for v in fresh], complete_decision_dispositions=dict(counts),
            per_query_journal_copies=0, global_financial_deduplication=True, automatic_financial_retry=False,
            controller_sha256=file_hash(Path(__file__)), metadata_helper_sha256=file_hash(ROOT / 'scripts/decision_window_metadata.py'),
            metadata_retry_helper_sha256=file_hash(ROOT / 'scripts/decision_window_metadata_retry.py'),
            original708_snapshot_sha256=ORIGINAL_SHA, source_qualified=False, full_replay_query_plan_complete=False))
        runner = Runner(args.output, plan, original, history, controller, preflight, tools)
        runner.max_pending = args.max_pending
        runner.runtime('startup_verified')
        if args.execute:
            runner.prefetched(fresh)
        runner.check()
        save_raw(args.output / 'ledger-after.private.json', runner.history.snapshot.raw)
        save(args.output / 'summary.json', dict(schema='AutoWheelFiniteDecisionAcquisitionV1', finished=True,
            execute=args.execute, exit_code=0, selected_windows=len(fresh), results=runner.results,
            financial_attempts=runner.financial_attempts, financial_retry=False,
            maximum_pending_jobs=args.max_pending, outstanding_pending_jobs=len(runner.pending),
            maximum_observed_pending_jobs=runner.peak_pending,
            ending_journal_entries=len(runner.history.by_operation), original708_rows_and_unknown_hold_preserved=True,
            per_query_financial_journal_copies=0, source_qualified=False, full_study_capacity_proven=False,
            full_replay_query_plan_complete=False, historical_performance_available=False, production_changed=False))
        runner.runtime('complete', finished=True, exit_code=0)
        print(json.dumps(dict(selected_windows=len(fresh), financial_attempts=runner.financial_attempts,
                              ending_journal_entries=len(runner.history.by_operation), execute=args.execute)))
    except Exception as error:
        if runner is not None:
            save(args.output / 'failure.json', dict(error_type=type(error).__name__,
                reason=str(error) if isinstance(error, ValueError) else None, automatic_financial_retry=False,
                financial_attempts=runner.financial_attempts, existing_attempt_and_hold_preserved=True,
                traceback_locations=[dict(filename=Path(v.filename).name, line=v.lineno)
                                     for v in traceback.extract_tb(error.__traceback__)[-3:]]))
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
