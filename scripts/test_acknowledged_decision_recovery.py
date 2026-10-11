"""Synthetic exact-ACK recovery controls; never use a provider or credential."""
import importlib.util
from pathlib import Path
import unittest
from unittest.mock import patch

import test_decision_window_acquisition as fixture
from batch_acquisition_proof import read_json
from research_batch_history import BatchHistory

spec = importlib.util.spec_from_file_location('exact_ack_recovery', Path(__file__).with_name('recover-acknowledged-decision-jobs.py'))
r = importlib.util.module_from_spec(spec);spec.loader.exec_module(r)
fixture.m = r.m


class RecoveryTests(unittest.TestCase):
    def setUp(self):
        self.fixture = fixture.AcquisitionTests();self.fixture.setUp();self.addCleanup(self.fixture.doCleanups)
        self.runner, self.provider = self.fixture.runner()
        self.runner.plan.window = lambda query: self.fixture.row
        self.original = self.runner.output / 'original';self.original.mkdir(mode=0o700)
        meta = self.runner.free(self.fixture.window, self.original / 'window-metadata')
        self.runner.submit(self.fixture.window, self.original, 'window', meta)
        self.runner.pending.clear();self.runner.financial_attempts = 0

    def test_exact_existing_ack_finishes_without_new_financial_send(self):
        snapshot = BatchHistory(self.fixture.ledger).snapshot.value
        held = r.resume(self.runner, self.original, self.runner.output / 'resumed')
        with patch.object(Path, 'is_mount', return_value=True):
            result = self.runner.finish(held)
        current = BatchHistory(self.fixture.ledger).snapshot.value
        self.assertEqual(result['status'], 'acquired')
        self.assertEqual(len(current['reservations']), len(snapshot['reservations']))
        self.assertEqual(current['reservations'][:708], snapshot['reservations'][:708])
        self.assertEqual(self.runner.financial_attempts, 0)
        self.assertEqual(self.provider.calls.count('autowheel-batch'), 1)
        self.assertNotIn('autowheel-batch', self.runner.tools)
        for name in ['query.json','financial-intent.json','submit/ack.json','submit/quote.json','submit/job.json']:
            self.assertEqual((self.original / name).read_bytes(), (held['directory'] / name).read_bytes())

    def test_wrong_original_ack_or_intent_stops_before_observation(self):
        path = self.original / 'submit/ack.json';value = read_json(path);value['query_hash'] = '9' * 64
        fixture.replace(path, value)
        with self.assertRaisesRegex(ValueError, 'acknowledgement'):
            r.resume(self.runner, self.original, self.runner.output / 'wrong')
        self.assertEqual(self.provider.calls, ['autowheel-batch'])
        self.assertEqual(BatchHistory(self.fixture.ledger).by_operation[self.fixture.row[6]]['status'], 'reserved')

    def test_existing_ack_cannot_be_resumed_twice(self):
        r.resume(self.runner, self.original, self.runner.output / 'first')
        with self.assertRaisesRegex(ValueError, 'queued twice'):
            r.resume(self.runner, self.original, self.runner.output / 'second')
        self.assertEqual(self.provider.calls, ['autowheel-batch'])


if __name__ == '__main__':
    unittest.main(verbosity=2)
