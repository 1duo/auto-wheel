"""Targeted adversarial controls; journals, jobs and native proofs are fixtures."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import decision_window_metadata as metadata
from batch_acquisition_proof import file_hash, read_json
from research_batch_history import BatchHistory, PROTECTED

SCRIPT = Path(__file__).with_name('acquire-decision-window-plan.py')
spec = importlib.util.spec_from_file_location('finite_window_acquisition', SCRIPT)
m = importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
SCRIPTS = SCRIPT.parent


def fixture_row(index):
    return dict(operation_id=hashlib.sha256(('fixture-finance-' + str(index)).encode()).hexdigest(),
        query_hash='2' * 64, endpoint='/batch.submit_job', cost_nano=0, reserved_nano=0,
        at='2026-04-06T12:00:00Z', response_hash='3' * 64, status='reserved')


class FakeProvider:
    """Model Go's exact held/acquired transitions, never a real provider."""
    def __init__(self, runner, original_stock_controller, window_records=2, fail_send=False):
        self.runner, self.controller = runner, original_stock_controller
        self.window_records, self.fail_send, self.calls = window_records, fail_send, []

    def call(self, name, args, directory, phase):
        self.calls.append(name)
        values = {str(args[i]): Path(args[i + 1]) for i in range(len(args) - 1)
                  if str(args[i]).startswith('--') and not str(args[i + 1]).startswith('--')}
        query = read_json(values['--query']);qh, op = m.identities(query)
        if name == 'autowheel-batch':
            assert (directory / 'financial-intent.json').is_file()
            target = values['--output'];target.mkdir(mode=0o700)
            body = dict(id='synthetic-' + op[:16], state='queued');raw = (json.dumps(body) + '\n').encode()
            metadata.save_raw(target / 'job.json', raw)
            ack = dict(operation_id=op, query_hash=qh, endpoint='/batch.submit_job', cost_nano=1,
                reserved_nano=2, response_hash=hashlib.sha256(raw).hexdigest(), bytes=len(raw),
                status_code=200, at='2026-04-06T13:35:01Z', job_id=body['id'], state='queued')
            journal = read_json(m.LEDGER);row = {k: ack[k] for k in ('operation_id', 'query_hash', 'endpoint',
                                                                  'cost_nano', 'reserved_nano', 'at')}
            row.update(status='reserved', response_hash='4' * 64);journal['reservations'].append(row)
            replace(m.LEDGER, journal)
            if self.fail_send:
                raise ValueError('synthetic ACK lost; preserve hold')
            metadata.save(target / 'ack.json', ack)
            metadata.save(target / 'quote.json', dict(quote=dict(ResponseHash='4' * 64)))
        elif name == 'autowheel-batch-planned-retrieve':
            source = values['--output'];source.mkdir(mode=0o700)
            _, job = self.job(directory, query)
            metadata.save(source / 'job-source.json', job)
            metadata.save(source / 'receipt.json', dict(state='done', all_verified=True, total_bytes=1,
                files=[dict(filename='fixture.raw', bytes=1, sha256='5' * 64)]))
        elif name == 'autowheel-batch-finalize':
            proof = dict(query=query, query_file_sha256=file_hash(values['--query']), total_bytes=1,
                         native=dict(all_eof_verified=True, records=2), provider_record_count=2, source_qualified=False)
            metadata.save(values['--proof'], proof)
            journal = read_json(m.LEDGER)
            next(row for row in journal['reservations'] if row['operation_id'] == op).update(
                status='acquired', data_sha256=file_hash(values['--proof']), bytes=1)
            replace(m.LEDGER, journal)
        else:
            raise AssertionError(name)

    def job(self, directory, query):
        ack = read_json(directory / 'submit/ack.json')
        records = self.window_records if query['start'] == self.runner.test_window['start'] else 2
        return ack, dict(query, id=ack['job_id'], state='done', record_count=records,
                         stype_out='instrument_id', encoding='dbn', compression='zstd', split_duration='day')

    def observe(self, preflight, binary, common, directory, label):
        query = read_json(Path(common[1]));ack, job = self.job(directory, query)
        source = directory / (label + '-read-0');source.mkdir(mode=0o700)
        metadata.save(source / 'job-source.json', job)
        receipt = dict(state='done', job_sha256=file_hash(source / 'job-source.json'))
        metadata.save(source / 'receipt.json', receipt)
        return receipt

    def file_plan(self, preflight, binary, common, directory, label):
        query = read_json(Path(common[1]));qh, op = m.identities(query)
        source = directory / (label + '-read-0');source.mkdir(mode=0o700)
        metadata.save(source / 'files-source.json', dict(synthetic=True))
        return source, dict(schema_version='AutoWheelBatchFilePlanV1', ready=True, metadata_only=True,
            source_qualified=False, observation=dict(all_verified=False, state='done', operation_id=op, query_hash=qh),
            files=[dict(filename='fixture.raw', bytes=1, sha256='5' * 64)], total_bytes=1, file_list_sha256='6' * 64)

    def seal(self, binary, args, directory, phase):
        source, output = Path(args[args.index('--source') + 1]), Path(args[args.index('--output') + 1])
        metadata.save(output, dict(source_directory=str(source), read_only=True, source_bytes_preserved=True))
        return 0


def replace(path, value):
    temporary = path.with_name(path.name + '.next');metadata.save(temporary, value);os.replace(temporary, path)


class AcquisitionTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory();self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve();self.root.chmod(0o700)
        self.repo = self.root / 'source';self.repo.mkdir(mode=0o700)
        (self.repo / 'scripts').mkdir(mode=0o700)
        shutil.copyfile(SCRIPTS / 'seal-external-research-source-scoped.py',
                        self.repo / 'scripts/seal-external-research-source-scoped.py')
        self.bulk = self.root / 'bulk';self.bulk.mkdir(mode=0o700)
        self.ledger = self.root / 'ledger.json';self.original = self.root / 'original.json'
        rows = [fixture_row(i) for i in range(708)];rows[0]['operation_id'] = PROTECTED
        value = dict(version=2, authorization_mode='uncapped', lifetime_nano=0, reservations=rows)
        metadata.save(self.ledger, value);metadata.save(self.original, value)
        self.old = m.load(SCRIPTS / 'acquire-prefetched-stock-plan-cycle74.py', 'fixture_original_controller', m.CONTROLLER_SHA)
        self.window = dict(dataset='OPRA.PILLAR', schema='cmbp-1', symbols=['TSLA  260410P00010000'],
                           stype_in='raw_symbol', start='2026-04-06T13:34:30Z', end='2026-04-06T13:35:00.000000001Z')
        self.full = dict(self.window, start='2026-04-06T00:00:00Z', end='2026-04-07T00:00:00Z')
        qh, op = m.identities(self.window);fqh, fop = m.identities(self.full)
        self.row = [0, 0, 1775482500000000000, 1775482470000000000, 1775482500000000001, qh, op, 'fresh_not_submitted', None]
        self.part = dict(full_operation_id=fop, full_query_hash=fqh)
        self.patches = patch.multiple(m, BASE=self.root, ROOT=self.repo, LEDGER=self.ledger, ORIGINAL=self.original,
                                     ORIGINAL_SHA=file_hash(self.original), PYTHON=Path(sys.executable))
        self.patches.start();self.addCleanup(self.patches.stop)

    def runner(self, records=2, fail_send=False):
        capacity = patch.object(m.os, 'statvfs', return_value=SimpleNamespace(f_bavail=1 << 40, f_frsize=1))
        capacity.start();self.addCleanup(capacity.stop)
        output = self.root / 'cycle93-decision-acquisition-fixture';output.mkdir(mode=0o700)
        controller = SimpleNamespace(load_old_controller=self.old.load_old_controller, BULK_VIEW=self.bulk,
            file_plan_total=self.old.file_plan_total, same_download_plan=self.old.same_download_plan)
        preflight = self.old.load_old_controller().load_preflight()
        r = m.Runner(output, SimpleNamespace(unchanged=lambda: None), BatchHistory(self.original),
                     BatchHistory(self.ledger), controller, preflight,
                     {'autowheel-batch-retrieve': (Path('synthetic-retrieve'), ''), 'autowheel-batch-files': (Path('synthetic-files'), '')})
        r.test_window = self.window;provider = FakeProvider(r, self.old, records, fail_send)
        r.call = provider.call;r.old.observe_readonly = provider.observe
        controller.observe_file_plan = provider.file_plan
        r.preflight = SimpleNamespace(private=preflight.private, run=provider.seal)
        def free(query, directory):
            r.check();directory.mkdir(mode=0o700)
            value = dict(record_count=2, billable_bytes=160, synthetic=True)
            metadata.save(directory / 'receipt.json', value);return value
        r.free = free
        return r, provider

    def test_timestamp_exact_and_output_namespace_pre_send(self):
        self.assertEqual(m.timestamp(self.row[3]), self.window['start'])
        self.assertEqual(m.timestamp(self.row[4]), self.window['end'])
        for name in ('cycleX-decision-acquisition-fixture', 'cycle93-decision-acquisition-!'):
            with self.assertRaises(ValueError):
                m.output_scope(self.root / name)

    def test_existing_reserved_completed_and_conflicting_identities(self):
        h = BatchHistory(self.ledger)
        self.assertEqual(m.action(h, self.row, self.part), 'fresh')
        row = dict(fixture_row(900), operation_id=self.row[6], query_hash=self.row[5])
        h.by_operation[self.row[6]] = row
        self.assertEqual(m.action(h, self.row, self.part), 'protected_existing_window')
        row['status'] = 'acquired'
        self.assertEqual(m.action(h, self.row, self.part), 'reuse_acquired_window')
        row['query_hash'] = '7' * 64
        with self.assertRaises(ValueError):
            m.action(h, self.row, self.part)

    def test_capacity_and_journal_headroom_fail_before_submit(self):
        h = BatchHistory(self.ledger);meta = dict(record_count=300000, billable_bytes=24000000)
        self.assertEqual(m.admission(h, meta, m.ROOT_RESERVE, 1 << 40)[0], 'positive_window_admitted')
        for root_free, ext_free in ((m.ROOT_RESERVE - 1, 1 << 40), (m.ROOT_RESERVE, m.EXTERNAL_RESERVE)):
            with self.assertRaises(ValueError):
                m.admission(h, meta, root_free, ext_free)
        with self.assertRaises(ValueError):
            m.admission(h, dict(record_count=1 << 40, billable_bytes=0), 1 << 40, 1 << 40)
        self.assertEqual(m.admission(h, dict(record_count=0, billable_bytes=0), 0, 0), ('full_day_fallback_required', 0))
        h.by_operation = {str(i): {} for i in range(100000 - 128)}
        with self.assertRaises(ValueError):
            m.admission(h, meta, 1 << 40, 1 << 40)

    def test_positive_pipeline_preserves_all_original_financial_fields(self):
        runner, provider = self.runner();directory = runner.output / 'positive';directory.mkdir(mode=0o700)
        value = runner.free(self.window, directory / 'window-metadata')
        with patch.object(Path, 'is_mount', return_value=True):
            result = runner.acquire(self.window, directory, 'window', value)
        self.assertEqual(result['status'], 'acquired')
        self.assertEqual(runner.financial_attempts, 1)
        self.assertEqual(BatchHistory(self.ledger).snapshot.value['reservations'][:708], BatchHistory(self.original).snapshot.value['reservations'])
        self.assertEqual(provider.calls, ['autowheel-batch', 'autowheel-batch-planned-retrieve', 'autowheel-batch-finalize'])
        self.assertFalse(any(runner.output.glob('**/ledger-before.private.json')))

    def test_failed_send_hold_survives_and_same_or_fresh_runner_cannot_retry(self):
        runner, provider = self.runner(fail_send=True);directory = runner.output / 'failed';directory.mkdir(mode=0o700)
        value = runner.free(self.window, directory / 'window-metadata')
        with self.assertRaisesRegex(ValueError, 'synthetic ACK lost'):
            runner.acquire(self.window, directory, 'window', value)
        self.assertEqual(BatchHistory(self.ledger).by_operation[self.row[6]]['status'], 'reserved')
        with self.assertRaises(ValueError):
            runner.acquire(self.window, directory, 'window', value)
        runner.history = BatchHistory(self.ledger)
        with self.assertRaises(ValueError):
            runner.acquire(self.window, directory, 'window', value)
        self.assertEqual(provider.calls, ['autowheel-batch'])

    def test_zero_actual_job_keeps_hold_and_uses_distinct_positive_full_day(self):
        runner, provider = self.runner(records=0)
        with patch.object(m, 'query_for', return_value=(self.part, self.full, self.window)), \
                patch.object(Path, 'is_mount', return_value=True):
            result = runner.window(self.row, 0)
        history = BatchHistory(self.ledger)
        self.assertEqual(result['status'], 'full_day_fallback_acquired')
        self.assertTrue(result['zero_window_hold_retained'])
        self.assertEqual(history.by_operation[self.row[6]]['status'], 'reserved')
        self.assertEqual(history.by_operation[self.part['full_operation_id']]['status'], 'acquired')
        self.assertEqual(provider.calls.count('autowheel-batch'), 2)
        self.assertEqual(provider.calls.count('autowheel-batch-finalize'), 1)
        self.assertEqual(m.action(history, self.row, self.part), 'reuse_acquired_full_day')
        observation = read_json(runner.output / 'window-000000/zero-job-observation.json')
        self.assertFalse(observation['native_absence_proven'])
        self.assertFalse(observation['financial_source_usable'])

    def test_unrelated_financial_change_and_deadline_stop_before_send(self):
        runner, provider = self.runner()
        value = read_json(self.ledger);value['reservations'][1]['response_hash'] = '8' * 64;replace(self.ledger, value)
        with self.assertRaises(ValueError):
            runner.check()
        runner.deadline = 0
        with self.assertRaises(ValueError):
            runner.check()
        self.assertEqual(provider.calls, [])

    def test_wrong_terminal_job_scope_keeps_hold_and_blocks_download(self):
        runner, provider = self.runner()
        directory = runner.output / 'wrong-job';directory.mkdir(mode=0o700)
        value = runner.free(self.window, directory / 'window-metadata')
        observe = provider.observe
        def wrong_job(*args):
            receipt = observe(*args)
            path = directory / 'observe-0-read-0/job-source.json'
            job = read_json(path);job['symbols'] = ['A DIFFERENT SYMBOL'];replace(path, job)
            receipt['job_sha256'] = file_hash(path)
            replace(path.parent / 'receipt.json', receipt)
            return receipt
        runner.old.observe_readonly = wrong_job
        with self.assertRaisesRegex(ValueError, 'completed job record count'):
            runner.acquire(self.window, directory, 'window', value)
        self.assertEqual(provider.calls, ['autowheel-batch'])
        self.assertEqual(BatchHistory(self.ledger).by_operation[self.row[6]]['status'], 'reserved')

    def test_free_metadata_fixed_endpoints_zero_is_not_source_absence(self):
        calls = []
        class Connection:
            def __init__(self, host, port, **kwargs):
                self.host, self.port = host, port
            def request(self, method, endpoint, **kwargs):
                calls.append((self.host, self.port, method, endpoint, kwargs['body']))
            def getresponse(self):
                return SimpleNamespace(status=200, read=lambda bound: b'0')
            def close(self):
                pass
        directory = self.root / 'metadata';directory.mkdir(mode=0o700)
        helper = SimpleNamespace(priv_dir=lambda p: None, load_key=lambda: 'synthetic-fixture-key')
        with patch.object(metadata, 'load_key_helper', return_value=helper):
            report = metadata.fetch(self.window, directory, connection_factory=Connection)
        self.assertEqual([v[3] for v in calls], list(metadata.ENDPOINTS.values()))
        self.assertTrue(all(v[:3] == ('hist.databento.com', 443, 'POST') for v in calls))
        self.assertFalse(report['source_absence_proven']);self.assertEqual(report['financial_requests'], 0)
        self.assertNotIn('synthetic-fixture-key', (directory / 'receipt.json').read_text())
        for raw, status in ((b'true', 200), (b'1.5', 200), (b'-1', 200), (b'1', 302), (b'0' * 4097, 200)):
            with self.assertRaises(ValueError):
                metadata.parse_integer(raw, status)


if __name__ == '__main__':
    unittest.main(verbosity=2)
