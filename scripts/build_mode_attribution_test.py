"""Targeted synthetic accounting controls, never historical performance."""
import copy
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('build-mode-attribution.py')
spec = importlib.util.spec_from_file_location('mode_attribution', SCRIPT)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


def quote(identity, kind, strike, at, ask=310):
    return dict(id=identity, symbol='NFLX', kind=kind, quantity=1,
                strike_cents=strike, at=at, ask_cents=ask, standard=True)


def trade(at, action, identity, price, flow=0, fee=0):
    return dict(at=at, symbol='NFLX', action=action, option_id=identity,
                price_cents=price, cash_flow_cents=flow, fee_cents=fee)


def synthetic(open_short=False, assigned_only=False):
    a, b, c = '2026-04-06T09:35:00-04:00', '2026-04-06T09:35:06-04:00', '2026-04-06T16:00:00-04:00'
    header = dict(schema_version='AutoWheelReplaySpecV1', mode='fixture',
                  strategy_config=dict(symbols=['NFLX', 'TSLA'], fee_per_contract_cents=100),
                  initial_cash_cents=8_000_000)
    frames = [header, dict(at=a, kind='decision', quotes=[quote('put', 'put', 10000, a)]),
              dict(at=b, kind='market', quotes=[quote('put', 'put', 10000, b)])]
    trades = [trade(a, 'submit', 'put', 200), trade(b, 'sell_put', 'put', 200, 19900, 100)]
    if open_short:
        frames.append(dict(at=c, kind='valuation', quotes=[quote('put', 'put', 10000, c)]))
        points = [dict(at=c, economic_cash_cents=8019900, stock_value_cents=0, option_liability_cents=31000,
                       equity_cents=7988900, available_cash_cents=7019900, committed_capital_cents=1000000)]
        metrics = dict(submissions=1, fills=1, expired_orders=0, assignments=0, call_aways=0,
                       premiums_cents=20000, fees_cents=100, dividends_cents=0, stock_realized_cents=0,
                       net_pnl_cents=-11100, ending_equity_cents=7988900)
    else:
        trades.append(trade(c, 'assigned', 'put', 10000, -1000000))
        frames.append(dict(at=c, kind='valuation', stocks=[dict(symbol='NFLX', at=c, bid_cents=9700)]))
        points = [dict(at=c, economic_cash_cents=7019900, stock_value_cents=970000, option_liability_cents=0,
                       equity_cents=7989900, available_cash_cents=7019900, committed_capital_cents=1000000)]
        metrics = dict(submissions=1, fills=1, expired_orders=0, assignments=1, call_aways=0,
                       premiums_cents=20000, fees_cents=100, dividends_cents=0, stock_realized_cents=0,
                       net_pnl_cents=-10100, ending_equity_cents=7989900)
        if not assigned_only:
            d, e, f, g = '2026-04-07T09:35:00-04:00', '2026-04-07T09:35:06-04:00', '2026-04-07T10:00:00-04:00', '2026-04-07T16:00:00-04:00'
            frames.extend([dict(at=d, kind='decision', quotes=[quote('call', 'call', 9500, d)]),
                           dict(at=e, kind='market', quotes=[quote('call', 'call', 9500, e)]),
                           dict(at=f, kind='market', dividend_cents=500, dividend_symbol='NFLX'),
                           dict(at=g, kind='valuation')])
            trades.extend([trade(d, 'submit', 'call', 300), trade(e, 'sell_call', 'call', 300, 29900, 100),
                           trade(g, 'called_away', 'call', 9500, 950000)])
            points.append(dict(at=g, economic_cash_cents=8000300, stock_value_cents=0, option_liability_cents=0,
                               equity_cents=8000300, available_cash_cents=7050300, committed_capital_cents=0))
            metrics.update(submissions=2, fills=2, call_aways=1, premiums_cents=50000, fees_cents=200,
                           dividends_cents=500, stock_realized_cents=-50000, net_pnl_cents=300, ending_equity_cents=8000300)
    raw = b''.join((json.dumps(frame) + '\n').encode() for frame in frames)
    report = dict(status='fixture', spec=header, performance=metrics, trades=trades,
                  equity_curve=points, observed_valuations=len(points), input_sha256=hashlib.sha256(raw).hexdigest())
    return report, raw


class AccountingControls(unittest.TestCase):
    def test_below_cost_exit_dividend_and_zero_trade_symbol(self):
        report, raw = synthetic()
        result = module.attribute(report, io.BytesIO(raw))
        row, zero = result['rows']
        self.assertEqual((row['net_pnl_cents'], row['premiums_cents'], row['fees_cents'], row['stock_realized_cents'], row['dividends_cents']),
                         (300, 50000, 200, -50000, 500))
        self.assertEqual(row['max_peak_to_trough_pnl_cents'], 10100)
        self.assertEqual((row['ending_economic_shares'], row['ending_short_contracts']), (0, 0))
        self.assertEqual(result['ending_available_cash_cents'], 7050300)
        self.assertTrue(zero['zero_trade_symbol'])
        self.assertEqual(zero['net_pnl_cents'], 0)
        self.assertEqual(result['status'], 'fixture')

    def test_open_short_is_a_liability(self):
        report, raw = synthetic(open_short=True)
        row = module.attribute(report, io.BytesIO(raw))['rows'][0]
        self.assertEqual((row['ending_option_liability_cents'], row['net_pnl_cents'], row['ending_short_contracts']), (31000, -11100, 1))

    def test_assigned_economic_inventory_is_marked_at_bid(self):
        report, raw = synthetic(assigned_only=True)
        row = module.attribute(report, io.BytesIO(raw))['rows'][0]
        self.assertEqual((row['ending_economic_shares'], row['ending_share_cost_cents'], row['ending_stock_value_cents']), (100, 10000, 970000))
        self.assertEqual(row['net_pnl_cents'], -10100)

    def test_accepts_go_serialized_empty_account_without_ignoring_changes(self):
        report, raw = synthetic()
        report['spec'] = copy.deepcopy(report['spec'])
        report['spec']['strategy_config']['account_number'] = ''
        self.assertEqual(module.attribute(report, io.BytesIO(raw))['rows'][0]['net_pnl_cents'], 300)
        for account in ('1234', None):
            report['spec']['strategy_config']['account_number'] = account
            with self.assertRaises(ValueError):
                module.attribute(report, io.BytesIO(raw))

    def test_forced_close_expiry_and_unfilled_day_order(self):
        for action in ('forced_closed', 'expired', 'day_order_expired'):
            with self.subTest(action=action):
                report, raw = synthetic(open_short=True)
                at = report['equity_curve'][0]['at']
                if action == 'forced_closed':
                    report['trades'].append(trade(at, action, 'put', 310, -31100, 100))
                    expected_cash, expected_pnl = 7988800, -11200
                    report['performance'].update(fees_cents=200)
                elif action == 'expired':
                    report['trades'].append(trade(at, action, 'put', 10000))
                    expected_cash, expected_pnl = 8019900, 19900
                else:
                    report['trades'] = [report['trades'][0], trade(at, action, 'put', 200)]
                    expected_cash, expected_pnl = 8000000, 0
                    report['performance'].update(fills=0, expired_orders=1, premiums_cents=0, fees_cents=0)
                report['equity_curve'][0].update(economic_cash_cents=expected_cash, equity_cents=expected_cash,
                                                stock_value_cents=0, option_liability_cents=0)
                report['performance'].update(net_pnl_cents=expected_pnl, ending_equity_cents=expected_cash)
                row = module.attribute(report, io.BytesIO(raw))['rows'][0]
                self.assertEqual((row['net_pnl_cents'], row['ending_short_contracts']), (expected_pnl, 0))
                self.assertEqual(row['zero_trade_symbol'], action == 'day_order_expired')

    def test_rejects_tampered_or_missing_accounting_evidence(self):
        for case in ('cash', 'fee', 'total', 'hash', 'action', 'post_nav_event', 'spec'):
            with self.subTest(case=case):
                report, raw = synthetic()
                if case == 'cash':
                    report['trades'][1]['cash_flow_cents'] += 1
                elif case == 'fee':
                    report['trades'][1]['fee_cents'] = True
                elif case == 'total':
                    report['performance']['net_pnl_cents'] += 1
                elif case == 'hash':
                    report['input_sha256'] = '0' * 64
                elif case == 'action':
                    report['trades'][1]['action'] = 'roll'
                elif case == 'spec':
                    report['spec'] = copy.deepcopy(report['spec'])
                    report['spec']['initial_cash_cents'] += 1
                else:
                    frames = [json.loads(line) for line in raw.splitlines()]
                    later = '2026-04-07T16:00:01-04:00'
                    frames.append(dict(at=later, kind='lifecycle', quotes=[quote('put2', 'put', 10000, later)]))
                    report['trades'].append(trade(later, 'sell_put', 'put2', 200, 19900, 100))
                    raw = b''.join((json.dumps(frame) + '\n').encode() for frame in frames)
                    report['input_sha256'] = hashlib.sha256(raw).hexdigest()
                with self.assertRaises(ValueError):
                    module.attribute(report, io.BytesIO(raw))

    def test_rejects_missing_and_future_closing_marks(self):
        for case in ('missing_short', 'future_stock'):
            report, raw = synthetic(open_short=case == 'missing_short', assigned_only=case == 'future_stock')
            frames = [json.loads(line) for line in raw.splitlines()]
            if case == 'missing_short':
                frames[-1]['quotes'] = []
            else:
                frames[-1]['stocks'][0]['at'] = '2026-04-06T16:00:01-04:00'
            raw = b''.join((json.dumps(frame) + '\n').encode() for frame in frames)
            report['input_sha256'] = hashlib.sha256(raw).hexdigest()
            with self.assertRaises(ValueError):
                module.attribute(report, io.BytesIO(raw))

    def test_preserves_native_nanosecond_order_and_freshness_boundary(self):
        self.assertEqual(module.timestamp('2026-04-06T16:00:00.000000001-04:00')
                         - module.timestamp('2026-04-06T20:00:00Z'), 1)
        report, raw = synthetic(assigned_only=True)
        frames = [json.loads(line) for line in raw.splitlines()]
        frames[3:3] = [dict(at='2026-04-06T09:35:06.000000001-04:00', kind='market'),
                       dict(at='2026-04-06T09:35:06.000000002-04:00', kind='market')]
        raw = b''.join((json.dumps(frame) + '\n').encode() for frame in frames)
        report['input_sha256'] = hashlib.sha256(raw).hexdigest()
        self.assertEqual(module.attribute(report, io.BytesIO(raw))['rows'][0]['net_pnl_cents'], -10100)
        for invalid_at in ('2026-04-06T16:00:00.000000001-04:00', '2026-04-06T15:59:29.999999999-04:00'):
            frames[-1]['stocks'][0]['at'] = invalid_at
            raw = b''.join((json.dumps(frame) + '\n').encode() for frame in frames)
            report['input_sha256'] = hashlib.sha256(raw).hexdigest()
            with self.assertRaises(ValueError):
                module.attribute(report, io.BytesIO(raw))

    def test_command_refuses_fixture_and_self_attested_report_without_a_table(self):
        for self_attested in (False, True):
            with self.subTest(self_attested=self_attested), tempfile.TemporaryDirectory() as folder:
                report, _ = synthetic()
                if self_attested:
                    report['status'] = 'counterfactual'
                    report['spec']['strategy_config']['selection_mode'] = 'conservative'
                root = Path(folder)
                r, q, output = root / 'report.json', root / 'qualification.json', root / 'attribution.json'
                r.write_text(json.dumps(report))
                q.write_text(json.dumps(dict(status='qualified' if self_attested else 'pending')))
                command = [sys.executable, str(SCRIPT), '--conservative-report', str(r), '--balanced-report', str(r),
                           '--conservative-qualification', str(q), '--balanced-qualification', str(q), '--output', str(output)]
                result = subprocess.run(command, capture_output=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(output.exists())
                expected = b'required completion checker did not pass' if self_attested else b'complete qualified historical report required'
                self.assertIn(expected, result.stderr)


if __name__ == '__main__':
    unittest.main()
