"""Focused checks for clipping, timestamp precision and mapping scope."""
import copy
from datetime import date
import importlib.util
from pathlib import Path
from types import SimpleNamespace
import unittest

import native_book_window_boundary as boundary
from batch_acquisition_proof import utc_ns

spec = importlib.util.spec_from_file_location('book_adapter', Path(__file__).with_name('audit-native-book-batch.py'))
adapter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(adapter)


class WindowBoundaryTests(unittest.TestCase):
    def setUp(self):
        self.symbol = 'NFLX  260410P00100000'
        self.query = dict(dataset='OPRA.PILLAR', schema='cmbp-1', symbols=[self.symbol], stype_in='raw_symbol',
                          start='2026-04-06T13:34:30Z', end='2026-04-06T13:35:00.000000001Z')
        self.cut, self.age = utc_ns('2026-04-06T13:35:00Z'), 30_000_000_000
        self.metadata = SimpleNamespace(dataset='OPRA.PILLAR', schema='cmbp-1', stype_in='raw_symbol',
              stype_out='instrument_id', start=utc_ns(self.query['start']), end=self.cut + 1,
              version=1, ts_out=False, limit=None, symbols=[self.symbol], partial=[], not_found=[],
              mappings={self.symbol: [dict(start_date=date(2026, 4, 6), end_date=date(2026, 4, 7), symbol='7')]})

    def bind(self, query=None, metadata=None):
        return boundary.window_binding(query or self.query, metadata or self.metadata, self.cut, self.age, adapter)

    def test_closed_end_is_one_nanosecond_after_cut(self):
        got = self.bind()
        self.assertEqual(got['source_end_exclusive_ns'], self.cut + 1)
        self.assertEqual(got['instruments'], [7])
        self.assertFalse(got['acquisition_proof_verified'])
        self.assertFalse(got['source_qualified'])
        for end in ('2026-04-06T13:35:00Z', '2026-04-06T13:35:00.000001Z'):
            query = dict(self.query, end=end)
            with self.subTest(end=end), self.assertRaises(ValueError):
                self.bind(query=query)

    def test_copied_full_day_header_and_narrower_metadata_rejected(self):
        for field, value in (('start', float(self.metadata.start)), ('start', utc_ns('2026-04-06T00:00:00Z')),
                             ('start', self.metadata.start + 1), ('end', self.cut),
                             ('end', utc_ns('2026-04-07T00:00:00Z'))):
            metadata = copy.deepcopy(self.metadata)
            setattr(metadata, field, value)
            with self.subTest(field=field, value=value), self.assertRaises(ValueError):
                self.bind(metadata=metadata)

    def test_missing_or_ambiguous_identity_rejected(self):
        for field, value in (('symbols', []), ('partial', None), ('partial', [self.symbol]), ('not_found', [self.symbol]), ('mappings', {}),
                             ('mappings', {self.symbol: self.metadata.mappings[self.symbol] * 2})):
            metadata = copy.deepcopy(self.metadata)
            setattr(metadata, field, value)
            with self.subTest(field=field), self.assertRaises(ValueError):
                self.bind(metadata=metadata)

    def test_codec_truncation_scope_and_bool_clocks_rejected(self):
        for field, value in (('version', True), ('version', 4), ('ts_out', 0), ('limit', True), ('limit', 1),
                             ('schema', 'mbp-1'), ('stype_out', 'raw_symbol')):
            metadata = copy.deepcopy(self.metadata)
            setattr(metadata, field, value)
            with self.subTest(field=field), self.assertRaises(ValueError):
                self.bind(metadata=metadata)
        with self.assertRaises(ValueError):
            boundary.window_binding(self.query, self.metadata, True, self.age, adapter)


if __name__ == '__main__':
    unittest.main()
