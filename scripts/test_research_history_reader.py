"""Focused fixture-only checks for purpose-specific large history readers."""
import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest

from batch_acquisition_proof import read_json
from research_history_reader import (JOURNAL_BYTES, QUALIFICATION_BYTES,
                                     read_financial_journal, read_qualification_manifest)


class HistoryReaderTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name).resolve()
        self.addCleanup(self.temp.cleanup)

    def write(self, value, name='journal.json'):
        path = self.root / name
        raw = value if isinstance(value, bytes) else json.dumps(value, separators=(',', ':')).encode()
        path.write_bytes(raw)
        path.chmod(0o600)
        return path

    def row(self, index):
        return dict(operation_id=hashlib.sha256(str(index).encode()).hexdigest(), query_hash='2' * 64,
                    endpoint='/batch.submit_job', cost_nano=1, reserved_nano=2, at='2026-04-06T12:00:00Z',
                    response_hash='3' * 64, status='reserved')

    def journal(self, rows):
        return dict(version=2, lifetime_nano=0, authorization_mode='uncapped', reservations=rows)

    def test_large_journal_retains_unknown_hold_and_legacy_receipt_limit(self):
        rows = [self.row(i) for i in range(12_000)]
        path = self.write(self.journal(rows))
        raw = path.read_bytes()
        self.assertGreater(len(raw), 4 << 20)
        value = read_financial_journal(path)
        self.assertEqual(value.raw, raw)
        self.assertEqual(value.value['reservations'], rows)
        self.assertEqual(value.sha256, hashlib.sha256(raw).hexdigest())
        with self.assertRaisesRegex(ValueError, 'size'):
            read_json(path)
        self.assertEqual(path.read_bytes(), raw)

    def test_policy_margin_duplicate_boolean_and_receipt_rejections(self):
        original = self.journal([self.row(1)])
        mutations = []
        for key, value in (('cost_nano', True), ('reserved_nano', 1), ('bytes', True),
                           ('status', 'complete'), ('at', '2999-01-01T00:00:00Z'), ('extra', 1)):
            item = json.loads(json.dumps(original));item['reservations'][0][key] = value;mutations.append(item)
        duplicate = self.journal([self.row(1), self.row(1)]);mutations.append(duplicate)
        old = dict(original, version=1, lifetime_nano=10_000_000_000, authorization_mode='');mutations.append(old)
        for index, value in enumerate(mutations):
            with self.subTest(index=index), self.assertRaises(ValueError):
                read_financial_journal(self.write(value, str(index) + '.json'))

    def test_overflow_and_legacy_size_bound(self):
        row = self.row(1);row.update(cost_nano=(1 << 63) - 1, reserved_nano=(1 << 63) - 1)
        with self.assertRaises(ValueError):
            read_financial_journal(self.write(self.journal([row]), 'overflow.json'))
        legacy = dict(version=1, lifetime_nano=10_000_000_000, reservations=[], padding='x' * (1 << 20))
        # Unknown fields are also forbidden; an oversized valid legacy envelope
        # uses whitespace so the byte policy is exercised independently.
        legacy.pop('padding')
        raw = json.dumps(legacy).encode() + b' ' * (1 << 20)
        with self.assertRaisesRegex(ValueError, 'legacy byte bound'):
            read_financial_journal(self.write(raw, 'legacy.json'))

    def test_entry_cap_before_rows_and_physical_limits_before_read(self):
        with self.assertRaisesRegex(ValueError, 'entry bound'):
            read_financial_journal(self.write(self.journal([{}] * 100_001)))
        for name, limit, reader in (('oversized-ledger', JOURNAL_BYTES, read_financial_journal),
                                    ('oversized-qualification', QUALIFICATION_BYTES, read_qualification_manifest)):
            path = self.root / name
            with path.open('wb') as stream:
                stream.truncate(limit + 1)
            path.chmod(0o600)
            with self.assertRaisesRegex(ValueError, 'byte bound'):
                reader(path)

    def test_large_qualification_with_original_small_reader_preserved(self):
        document = dict(source_files=[dict(path='source.json', sha256='a' * 64)], padding='x' * (33 << 20))
        path = self.write(document, 'qualification.json')
        self.assertEqual(read_qualification_manifest(path).value, document)
        with self.assertRaisesRegex(ValueError, 'size'):
            read_json(path)

    def test_duplicate_nonfinite_dirty_and_conflicting_source_paths(self):
        for index, raw in enumerate((b'{"source_files":[],"source_files":[]}',
                                     b'{"source_files":[],"x":NaN}', b'{"source_files":[]} {}')):
            with self.subTest(index=index), self.assertRaises(ValueError):
                read_qualification_manifest(self.write(raw, str(index) + '.json'))
        for value in (['x', 'x'], ['a/../b'], ['./source'], ['']):
            document = dict(source_files=[dict(path=v, sha256='a' * 64) for v in value])
            with self.subTest(value=value), self.assertRaises(ValueError):
                read_qualification_manifest(self.write(document, 'source-' + str(len(value)) + '.json'))

    def test_private_permissions_symlink_and_hardlink(self):
        path = self.write(self.journal([self.row(1)]))
        path.chmod(0o644)
        with self.assertRaises(ValueError):
            read_financial_journal(path)
        path.chmod(0o600)
        link = self.root / 'symbolic';link.symlink_to(path)
        with self.assertRaises(ValueError):
            read_financial_journal(link)
        hard = self.root / 'hard';os.link(path, hard)
        with self.assertRaises(ValueError):
            read_financial_journal(path)

    def test_completed_exact_money_and_actual_snapshot_optional(self):
        row = self.row(1);row.update(status='acquired', data_sha256='4' * 64, bytes=1)
        value = self.journal([row]);path = self.write(value)
        self.assertEqual(read_financial_journal(path).value, value)
        actual = os.environ.get('AUTOWHEEL_HISTORY_TEST_SNAPSHOT')
        if actual:
            raw = Path(actual).read_bytes()
            copied = self.write(raw, 'actual-copy.json')
            snapshot = read_financial_journal(copied)
            self.assertEqual(snapshot.raw, raw)
            self.assertEqual(len(snapshot.value['reservations']), 708)
            protected = [v for v in snapshot.value['reservations'] if v['operation_id'] ==
                         '01593d27c3a667a42b8736bba57c377e98dd6f19949f4601e2c08ef505abbf5f']
            self.assertEqual([v['status'] for v in protected], ['reserved'])
            self.assertEqual(Path(actual).read_bytes(), raw)


if __name__ == '__main__':
    unittest.main()
