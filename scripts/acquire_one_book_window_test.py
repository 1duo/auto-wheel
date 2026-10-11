"""Only new pre-send guard cases; legacy financial controls are reused."""
import copy
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('pilot', Path(__file__).with_name('acquire-one-book-window.py'))
pilot = importlib.util.module_from_spec(spec)
spec.loader.exec_module(pilot)


class SendGuardTests(unittest.TestCase):
    def setUp(self):
        self.runtime = dict(finished=True, exit_code=0, execute_attempts=1, actual_twelve_job_terminal_success_verified=True)
        self.summary = dict(complete_stock_acquisition_run=True, completed_queries=117,
                            selected_stock_operations=126, excluded_option_book_operations=543, completed=list(range(117)))
        self.snapshot = dict(version=2, authorization_mode='uncapped', lifetime_nano=0,
              reservations=[dict(operation_id=str(i), status='acquired') for i in range(706)] +
                           [dict(operation_id=pilot.PROTECTED, status='reserved')])

    def check(self, runtime=None, summary=None, current=None, live=False):
        pilot.terminal_stock(runtime or self.runtime, summary or self.summary, self.snapshot,
                             current or self.snapshot, pilot.OPERATION, live)

    def test_exact_terminal_snapshot_allows_one_fresh_operation(self):
        self.check()
        with self.assertRaises(ValueError):
            self.check(live=True)
        for field, value in (('finished', False), ('exit_code', 1), ('exit_code', False), ('execute_attempts', 0), ('execute_attempts', True),
                             ('actual_twelve_job_terminal_success_verified', False)):
            with self.subTest(field=field), self.assertRaises(ValueError):
                self.check(runtime=dict(self.runtime, **{field: value}))

    def test_incomplete_stock_scope_and_mutated_journal_rejected(self):
        for field, value in (('completed_queries', 116), ('completed_queries', 117.0), ('selected_stock_operations', 125),
                             ('excluded_option_book_operations', 0), ('completed', [])):
            with self.subTest(field=field), self.assertRaises(ValueError):
                self.check(summary=dict(self.summary, **{field: value}))
        changed = copy.deepcopy(self.snapshot)
        changed['reservations'][0]['status'] = 'reserved'
        with self.assertRaises(ValueError):
            self.check(current=changed)

    def test_existing_unknown_operation_and_released_hold_rejected(self):
        for action in ('duplicate', 'released'):
            snapshot = copy.deepcopy(self.snapshot)
            if action == 'duplicate':
                snapshot['reservations'][0] = dict(operation_id=pilot.OPERATION, status='reserved')
            else:
                snapshot['reservations'][-1]['status'] = 'acquired'
            with self.subTest(action=action), self.assertRaises(ValueError):
                pilot.terminal_stock(self.runtime, self.summary, snapshot, snapshot, pilot.OPERATION, False)


if __name__ == '__main__':
    unittest.main()
