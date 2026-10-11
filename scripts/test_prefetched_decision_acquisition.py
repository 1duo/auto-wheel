"""Adversarial synthetic controls for serialized financial writes and prefetch."""
import importlib.util
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import test_decision_window_acquisition as fixture
from batch_acquisition_proof import file_hash, read_json
from research_batch_history import BatchHistory

SCRIPT = Path(__file__).with_name('acquire-prefetched-decision-window-plan.py')
spec = importlib.util.spec_from_file_location('prefetched_window_acquisition', SCRIPT)
m = importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
fixture.m = m


class PrefetchTests(fixture.AcquisitionTests):
    def second(self):
        row = list(self.row);row[1] = 1
        for index in (2, 3, 4):
            row[index] += 300_000_000_000
        query = dict(self.window, start=m.timestamp(row[3]), end=m.timestamp(row[4]))
        row[5], row[6] = m.identities(query)
        return row, query

    def submitted(self, runner, query, label):
        directory = runner.output / label;directory.mkdir(mode=0o700)
        value = runner.free(query, directory / 'window-metadata')
        with patch.object(m.time, 'sleep'):
            return runner.submit(query, directory, 'window', value)

    def test_wrong_terminal_job_scope_keeps_hold_and_blocks_download(self):
        runner, provider = self.runner();held = self.submitted(runner, self.window, 'wrong-job')
        observe = provider.observe
        def wrong(*args):
            receipt = observe(*args);path = held['directory'] / 'observe-0-read-0/job-source.json'
            job = read_json(path);job['id'] = 'different-acknowledgement'
            fixture.replace(path, job);receipt['job_sha256'] = file_hash(path)
            fixture.replace(path.parent / 'receipt.json', receipt);return receipt
        runner.old.observe_readonly = wrong
        with self.assertRaisesRegex(ValueError, 'acknowledged job scope'):
            runner.finish(held)
        self.assertEqual(provider.calls, ['autowheel-batch'])
        self.assertEqual(BatchHistory(self.ledger).by_operation[self.row[6]]['status'], 'reserved')

    def test_queue_two_before_download_and_finalize_ready_non_tail(self):
        runner, provider = self.runner();row, query = self.second()
        observe = provider.observe;seen = []
        def queued_first(*args):
            receipt = observe(*args);directory = args[3]
            if directory.name == 'window-000000' and args[4] == 'observe-0':
                path = directory / 'observe-0-read-0/job-source.json'
                job = read_json(path);job['state'] = 'queued';fixture.replace(path, job)
                receipt.update(state='queued', job_sha256=file_hash(path))
                fixture.replace(path.parent / 'receipt.json', receipt)
            return receipt
        call = provider.call
        def record(*args):
            call(*args)
            if args[0] == 'autowheel-batch-finalize':
                current = BatchHistory(self.ledger)
                seen.append((current.by_operation[self.row[6]]['status'], current.by_operation[row[6]]['status']))
        runner.call = record;runner.old.observe_readonly = queued_first
        def scope(plan, selected):
            return self.part, self.full, self.window if selected[6] == self.row[6] else query
        with patch.object(m, 'query_for', side_effect=scope), patch.object(m.time, 'sleep'), \
                patch.object(Path, 'is_mount', return_value=True):
            runner.prefetched([self.row, row])
        self.assertEqual(provider.calls[:2], ['autowheel-batch', 'autowheel-batch'])
        self.assertEqual(seen, [('reserved', 'acquired'), ('acquired', 'acquired')])
        self.assertEqual(runner.pending, {});self.assertEqual(len(runner.results), 2)
        self.assertEqual(BatchHistory(self.ledger).snapshot.value['reservations'][:708],
                         BatchHistory(self.original).snapshot.value['reservations'])

    def test_pending_storage_is_subtracted_before_second_financial_send(self):
        runner, provider = self.runner();_, query = self.second()
        allowance = (32 << 20) + 320
        external = m.EXTERNAL_RESERVE + 2 * allowance - 1
        def capacity(path):
            return SimpleNamespace(f_bavail=external if path == self.bulk else 1 << 40, f_frsize=1)
        with patch.object(m.os, 'statvfs', side_effect=capacity):
            self.submitted(runner, self.window, 'first')
            with self.assertRaisesRegex(ValueError, 'storage reserve'):
                self.submitted(runner, query, 'second')
        self.assertEqual(provider.calls, ['autowheel-batch'])
        self.assertEqual(len(runner.pending), 1)
        self.assertEqual(len(BatchHistory(self.ledger).by_operation), 709)

    def test_actual_file_plan_reserves_capacity_for_other_pending_jobs(self):
        runner, provider = self.runner();_, query = self.second()
        first = self.submitted(runner, self.window, 'first')
        second = self.submitted(runner, query, 'second')
        external = m.EXTERNAL_RESERVE + second['allowance']
        def capacity(path):
            return SimpleNamespace(f_bavail=external if path == self.bulk else 1 << 40, f_frsize=1)
        with patch.object(m.os, 'statvfs', side_effect=capacity):
            with self.assertRaisesRegex(ValueError, 'actual acknowledged file plan'):
                runner.finish(first)
        self.assertEqual(provider.calls, ['autowheel-batch', 'autowheel-batch'])
        self.assertEqual(len(runner.pending), 2)
        self.assertTrue(all(BatchHistory(self.ledger).by_operation[h['operation']]['status'] == 'reserved'
                            for h in (first, second)))

    def test_pending_ceiling_and_exact_ack_deadline_stop_without_send_or_download(self):
        runner, provider = self.runner();runner.max_pending = 1;_, query = self.second()
        held = self.submitted(runner, self.window, 'first')
        with self.assertRaisesRegex(ValueError, 'pending-job ceiling'):
            self.submitted(runner, query, 'second')
        held['started'] = m.time.monotonic() - 7201
        with self.assertRaisesRegex(ValueError, 'observation deadline'):
            runner.finish(held)
        self.assertEqual(provider.calls, ['autowheel-batch'])
        self.assertEqual(BatchHistory(self.ledger).by_operation[held['operation']]['status'], 'reserved')

    def test_changed_acknowledgement_is_rejected_before_observation(self):
        runner, provider = self.runner();held = self.submitted(runner, self.window, 'first')
        path = held['directory'] / 'submit/ack.json';value = read_json(path)
        value['job_id'] = 'different-acknowledgement';fixture.replace(path, value)
        with self.assertRaisesRegex(ValueError, 'acknowledgement changed'):
            runner.finish(held)
        self.assertEqual(provider.calls, ['autowheel-batch'])

    def test_second_unknown_send_preserves_both_holds_without_retry(self):
        runner, provider = self.runner();_, query = self.second()
        self.submitted(runner, self.window, 'first');provider.fail_send = True
        with self.assertRaisesRegex(ValueError, 'ACK lost'):
            self.submitted(runner, query, 'second')
        current = BatchHistory(self.ledger)
        self.assertEqual(len(current.by_operation), 710)
        self.assertTrue(all(row['status'] == 'reserved' for row in current.snapshot.value['reservations'][708:]))
        with self.assertRaisesRegex(ValueError, 'unrelated financial journal mutation'):
            runner.check()
        self.assertEqual(provider.calls, ['autowheel-batch', 'autowheel-batch'])

    def test_prefetched_zero_job_reuses_distinct_completed_full_day_and_keeps_hold(self):
        runner, provider = self.runner(records=0);row, query = self.second()
        runner.max_pending = 2
        third = list(row);third[1] += 1
        for index in (2, 3, 4):
            third[index] += 300_000_000_000
        third_query = dict(query, start=m.timestamp(third[3]), end=m.timestamp(third[4]))
        third[5], third[6] = m.identities(third_query)
        job = provider.job
        def empty_windows(directory, selected):
            ack, value = job(directory, selected)
            if selected['start'] != self.full['start']:
                value['record_count'] = 0
            return ack, value
        provider.job = empty_windows
        def scope(plan, selected):
            queries = {self.row[6]:self.window, row[6]:query, third[6]:third_query}
            return self.part, self.full, queries[selected[6]]
        with patch.object(m, 'query_for', side_effect=scope), patch.object(m.time, 'sleep'), \
                patch.object(Path, 'is_mount', return_value=True):
            runner.prefetched([self.row, row, third])
        current = BatchHistory(self.ledger)
        self.assertEqual(current.by_operation[self.row[6]]['status'], 'reserved')
        self.assertEqual(current.by_operation[self.part['full_operation_id']]['status'], 'acquired')
        self.assertEqual(current.by_operation[row[6]]['status'], 'reserved')
        self.assertEqual(provider.calls.count('autowheel-batch'), 3)
        self.assertEqual(runner.pending, {});self.assertEqual(len(runner.results), 3)
        self.assertEqual(runner.results[1]['status'], 'reuse_acquired_full_day')
        self.assertEqual(runner.results[1]['source_operation_id'], self.part['full_operation_id'])
        self.assertEqual(runner.results[2]['status'], 'reuse_acquired_full_day')
        self.assertEqual(runner.results[2]['source_operation_id'], self.part['full_operation_id'])
        self.assertEqual(runner.peak_pending, 2)
        self.assertFalse(read_json(runner.output / 'window-000000/zero-job-observation.json')['financial_source_usable'])


if __name__ == '__main__':
    unittest.main(verbosity=2)
