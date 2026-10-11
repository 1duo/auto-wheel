"""Focused controls for shared whole-run history acquisition/audit integration."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest

from batch_acquisition_proof import read_json
from research_batch_history import BatchHistory, DecisionPlan, PROTECTED


class BatchHistoryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()

    def row(self, index):
        return dict(operation_id=hashlib.sha256(('only-a-test-fixture-' + str(index)).encode()).hexdigest(),
                    query_hash='2' * 64, endpoint='/batch.submit_job', cost_nano=0, reserved_nano=0,
                    at='2026-04-06T12:00:00Z', response_hash='3' * 64, status='reserved')

    def journal(self, count=2):
        rows = [self.row(i) for i in range(count)];rows[0]['operation_id'] = PROTECTED
        return dict(version=2, lifetime_nano=0, authorization_mode='uncapped', reservations=rows)

    def write(self, value, name='fixture-journal.json'):
        path = self.root / name
        path.write_bytes((json.dumps(value, separators=(',', ':')) + '\n').encode());path.chmod(0o600)
        return path

    def test_large_history_index_preserves_rows_and_small_reader_limit(self):
        original = self.journal(52_000);path = self.write(original);raw = path.read_bytes()
        history = BatchHistory(path)
        self.assertGreater(len(raw), 4 << 20)
        self.assertEqual(len(history.by_operation), 52_000)
        self.assertEqual(history.snapshot.value, original)
        self.assertEqual(history.by_operation[PROTECTED], original['reservations'][0])
        history.fresh('f' * 64);history.unchanged()
        with self.assertRaisesRegex(ValueError, 'size'):
            read_json(path)
        self.assertEqual(path.read_bytes(), raw)

    def test_every_existing_operation_prohibits_submission(self):
        original = self.journal(3)
        original['reservations'][1].update(status='acquired', data_sha256='4' * 64, bytes=1)
        history = BatchHistory(self.write(original))
        for row in original['reservations']:
            with self.subTest(status=row['status']), self.assertRaisesRegex(ValueError, 'never resubmit'):
                history.fresh(row['operation_id'])
        self.assertEqual(history.acquired(original['reservations'][1]['operation_id']), original['reservations'][1])
        with self.assertRaisesRegex(ValueError, 'acquired batch'):
            history.acquired(PROTECTED)

    def test_original_unknown_hold_cannot_disappear_or_finalize(self):
        for changed in ('missing', 'acquired'):
            value = self.journal()
            if changed == 'missing':
                value['reservations'].pop(0)
            else:
                value['reservations'][0].update(status='acquired', data_sha256='4' * 64, bytes=1)
            history = BatchHistory(self.write(value, changed + '.json'))
            with self.assertRaisesRegex(ValueError, 'must remain reserved'):
                history.fresh('f' * 64)

    def test_conflicting_duplicate_before_index_and_changed_snapshot(self):
        value = self.journal();value['reservations'].append(dict(value['reservations'][0], response_hash='5' * 64))
        with self.assertRaisesRegex(ValueError, 'duplicate financial operation'):
            BatchHistory(self.write(value, 'duplicate.json'))
        original = self.journal();path = self.write(original);history = BatchHistory(path)
        original['reservations'][1]['response_hash'] = '5' * 64;self.write(original)
        with self.assertRaisesRegex(ValueError, 'snapshot changed'):
            history.unchanged()

    def test_unaccepted_plan_rejected_before_query_or_audit(self):
        path = self.root / 'unaccepted.json.gz';path.write_bytes(b'not a source plan')
        with self.assertRaisesRegex(ValueError, 'accepted complete decision plan differs'):
            DecisionPlan(path)

    def test_acquisition_window_disposition_and_identity_controls(self):
        root = Path(__file__).resolve().parent
        spec = importlib.util.spec_from_file_location('candidate_acquisition_preflight', root / 'preflight-decision-window-acquisition.py')
        module = importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
        original = self.journal(3)
        original['reservations'][1].update(status='acquired', data_sha256='4' * 64, bytes=1)
        history = BatchHistory(self.write(original))
        row = [0, 0, 0, 0, 0, '2' * 64, original['reservations'][2]['operation_id'], 'fresh_not_submitted', None]
        self.assertEqual(module.classify_window(history, row), 'protected_existing_window')
        row[6] = original['reservations'][1]['operation_id']
        self.assertEqual(module.classify_window(history, row), 'acquired_existing_window')
        row[6] = 'f' * 64;row[7:] = ['reuse_acquired_full_day', original['reservations'][1]['operation_id']]
        self.assertEqual(module.classify_window(history, row), 'acquired_existing_full_day')
        row[7:] = ['fresh_not_submitted', None]
        self.assertEqual(module.classify_window(history, row), 'fresh_not_submitted')
        row[6] = original['reservations'][2]['operation_id'];row[5] = '8' * 64
        with self.assertRaisesRegex(ValueError, 'identity differs'):
            module.classify_window(history, row)

    @unittest.skipUnless(os.environ.get('AUTOWHEEL_BATCH_HISTORY_SNAPSHOT'), 'actual MSI source integration')
    def test_actual_native_prepare_with_large_fixture_and_unchanged_originals(self):
        source_root = Path(os.environ['AUTOWHEEL_BATCH_HISTORY_SOURCE_ROOT'])
        snapshot_path = Path(os.environ['AUTOWHEEL_BATCH_HISTORY_SNAPSHOT'])
        proof_path = Path(os.environ['AUTOWHEEL_BATCH_HISTORY_PROOF'])
        plan_path = Path(os.environ['AUTOWHEEL_BATCH_HISTORY_PLAN'])
        original_raw = snapshot_path.read_bytes();original = json.loads(original_raw)
        value = json.loads(original_raw)
        value['reservations'] += [self.row(i + 100_000) for i in range(52_000)]
        fixture = self.write(value, 'actual-plus-synthetic-large-fixture.json')
        spec = importlib.util.spec_from_file_location('large_native_integration', source_root / 'scripts/audit-native-book-batch-large-history.py')
        module = importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
        history, plan, source = module.prepare(proof_path, fixture, source_root, plan_path)
        self.assertEqual(history.snapshot.value['reservations'][:708], original['reservations'])
        self.assertEqual(len(history.by_operation), 52_708)
        self.assertEqual(source.scope, 'declared_closed_decision_window_acquisition')
        self.assertEqual(source.proof['provider_record_count'], 305_427)
        self.assertEqual(source.cut_ns, 1775482500000000000)
        self.assertEqual(source.entry['operation_id'], 'fef32e79bfd60d0db6d8d91cc57640f82df501ea353c75be8b40a3c866d24431')
        import databento_dbn as dbn
        adapter = module.load_original_adapter(source_root)
        module.window_metadata(source, adapter, dbn)
        with self.assertRaisesRegex(ValueError, 'short window requires'):
            module.prepare(proof_path, fixture, source_root)
        history.unchanged();plan.unchanged()
        self.assertEqual(snapshot_path.read_bytes(), original_raw)
        self.assertEqual(history.by_operation[PROTECTED], original['reservations'][next(
            i for i, row in enumerate(original['reservations']) if row['operation_id'] == PROTECTED)])


if __name__ == '__main__':
    unittest.main()
