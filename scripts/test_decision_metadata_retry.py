"""Targeted free-transport controls; all HTTP responses are synthetic."""
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import decision_window_metadata_retry as retry


class MetadataRetryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory();self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name);self.query = dict(dataset='OPRA.PILLAR', schema='cmbp-1',
            stype_in='raw_symbol', symbols=['TSLA  260410P00010000'],
            start='2026-04-06T13:34:30Z', end='2026-04-06T13:35:00.000000001Z')
        helper = patch.object(retry.original, 'load_key_helper', return_value=SimpleNamespace(
            priv_dir=lambda path:None, load_key=lambda:'synthetic-fixture-key'))
        helper.start();self.addCleanup(helper.stop)
        sleep = patch.object(retry.time, 'sleep');sleep.start();self.addCleanup(sleep.stop)

    def factory(self, failures, body=b'2', status=200):
        self.calls = [];owner = self
        class Connection:
            def __init__(self, host, port, **kwargs):
                assert (host, port) == ('hist.databento.com', 443)
            def request(self, method, endpoint, **kwargs):
                owner.calls.append((method, endpoint))
                if len(owner.calls) <= failures:
                    raise TimeoutError('synthetic free endpoint transport')
            def getresponse(self):
                return SimpleNamespace(status=status, read=lambda bound:body)
            def close(self):
                pass
        return Connection

    def test_one_transient_free_failure_preserves_attempt_then_succeeds(self):
        value = retry.fetch(self.query, self.root, connection_factory=self.factory(1))
        self.assertEqual(value['metadata_observation_attempts'], 2)
        self.assertEqual(value['financial_requests'], 0);self.assertFalse(value['financial_retry'])
        self.assertEqual(len(self.calls), 3)
        self.assertTrue((self.root / 'transport-observation-000/record_count-failure.json').exists())
        self.assertTrue(all(endpoint in retry.original.ENDPOINTS.values() for _,endpoint in self.calls))

    def test_transport_exhaustion_is_bounded_and_retains_all_failures(self):
        with self.assertRaisesRegex(ValueError, 'transport exhausted'):
            retry.fetch(self.query, self.root, connection_factory=self.factory(10))
        self.assertEqual(len(self.calls), 3)
        self.assertEqual(len(list(self.root.glob('transport-observation-*/record_count-failure.json'))), 3)
        self.assertTrue((self.root / 'transport-failure.json').exists())
        self.assertFalse((self.root / 'receipt.json').exists())

    def test_invalid_http_response_is_never_retried(self):
        with self.assertRaisesRegex(ValueError, 'response status'):
            retry.fetch(self.query, self.root, connection_factory=self.factory(0, status=403))
        self.assertEqual(len(self.calls), 1)
        self.assertTrue((self.root / 'transport-observation-000/record_count-response.private.json').exists())


if __name__ == '__main__':
    unittest.main(verbosity=2)
