#!/usr/bin/env python3
"""Build 30 attribution rows only from two qualified, complete mode portfolios.

This is accounting over an existing replay, never an isolated-symbol backtest.
The calculation can be exercised with synthetic fixtures by imported tests;
the command accepts only complete historical mode reports. No broker access.
"""
import argparse
import calendar
from datetime import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys


def read_json(raw):
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise ValueError('duplicate JSON field')
            result[key] = value
        return result
    return json.loads(raw, object_pairs_hook=pairs,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError('non-finite JSON')))


def cents(value):
    if type(value) is not int or abs(value) > 100_000_000_000:
        raise ValueError('invalid integer money/count')
    return value


def timestamp(value):
    # datetime's microsecond precision cannot distinguish native receive times
    # or the exact engine freshness boundary. Keep the full RFC3339 nanoseconds.
    parts = re.fullmatch(r'(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})', value)
    if parts is None:
        raise ValueError('invalid RFC3339 nanosecond timestamp')
    whole, fraction, offset = parts.groups()
    result = datetime.fromisoformat(whole + offset.replace('Z', '+00:00'))
    return calendar.timegm(result.utctimetuple()) * 1_000_000_000 + int((fraction or '').ljust(9, '0'))


def attribute(report, source):
    """Reconstruct attribution in one bounded pass over the exact replay input.

    No source is qualified here. The CLI runs the existing completion checker
    first; this function separately reconciles every closing NAV and metric.
    """
    if report['status'] not in ('fixture', 'counterfactual') or not report.get('performance'):
        raise ValueError('performance unavailable')
    config = report['spec']['strategy_config']
    symbols = config['symbols']
    if not symbols or len(symbols) != len(set(symbols)):
        raise ValueError('invalid symbol universe')
    initial = cents(report['spec']['initial_cash_cents'])
    if initial <= 0:
        raise ValueError('invalid capital')
    rows = {symbol: dict(symbol=symbol, submissions=0, fills=0, expired_orders=0,
                        assignments=0, call_aways=0, premiums_cents=0, fees_cents=0,
                        dividends_cents=0, stock_realized_cents=0, cash_flow_cents=0)
            for symbol in symbols}
    shares, shorts, quotes, stocks = {}, {}, {}, {}
    peak, drawdown = dict.fromkeys(symbols, 0), dict.fromkeys(symbols, 0)
    trades = iter(report.get('trades', []))
    next_trade = next(trades, None)
    points = iter(report['equity_curve'])
    next_point = next(points, None)
    digest, previous, final, last_economic_event = hashlib.sha256(), None, None, None
    count = 0
    for line_number in range(1, 1_000_000_001):
        raw = source.readline((32 << 20) + 1)
        if not raw:
            break
        if len(raw) > 32 << 20:
            raise ValueError('replay line exceeds engine bound')
        digest.update(raw)
        frame = read_json(raw)
        if line_number == 1:
            if frame.get('schema_version') != 'AutoWheelReplaySpecV1':
                raise ValueError('missing replay specification')
            def normalized_spec(value):
                result = dict(value)
                result['peer_reserved_cash_cents'] = result.get('peer_reserved_cash_cents', 0)
                result['peer_reserved_symbols'] = result.get('peer_reserved_symbols') or []
                result['research_variant'] = result.get('research_variant', '')
                config = dict(result['strategy_config'])
                # Go marshals this required string even when an offline input
                # omits it. Mirror the completion checker's empty-only rule.
                if config.get('account_number') == '':
                    config.pop('account_number')
                result['strategy_config'] = config
                return result
            if normalized_spec(frame) != normalized_spec(report['spec']):
                raise ValueError('replay specification differs from report')
            continue
        at = timestamp(frame['at'])
        if previous is not None and at <= previous:
            raise ValueError('frame time reversal')
        previous = at
        if frame['kind'] == 'decision':
            quotes.clear()
        for quote in frame.get('quotes', []):
            quotes[quote['id']] = quote
        for stock in frame.get('stocks', []):
            stocks[stock['symbol']] = stock
        while next_trade is not None and timestamp(next_trade['at']) <= at:
            trade = next_trade
            if timestamp(trade['at']) != at or trade['symbol'] not in rows:
                raise ValueError('trade is outside its frame or universe')
            row = rows[trade['symbol']]
            action, identity = trade['action'], trade['option_id']
            price, fee, flow = (cents(trade[key]) for key in ('price_cents', 'fee_cents', 'cash_flow_cents'))
            if price < 0 or fee < 0:
                raise ValueError('negative price or fee')
            if action in ('submit', 'day_order_expired'):
                if flow or fee:
                    raise ValueError('unfilled order has a cash flow')
                row['submissions' if action == 'submit' else 'expired_orders'] += 1
            elif action in ('sell_put', 'sell_call'):
                quote = quotes.get(identity)
                if (quote is None or quote['symbol'] != trade['symbol']
                        or quote['kind'] != action.removeprefix('sell_')
                        or cents(quote['quantity']) != 1 or quote['standard'] is not True
                        or identity in shorts or flow != price * 100 - fee
                        or fee != config['fee_per_contract_cents']):
                    raise ValueError('fill terms or cash flow differ')
                if action == 'sell_call' and trade['symbol'] not in shares:
                    raise ValueError('uncovered call')
                shorts[identity] = dict(symbol=trade['symbol'], kind=quote['kind'], strike=cents(quote['strike_cents']))
                row['fills'] += 1
                row['premiums_cents'] += price * 100
                row['fees_cents'] += fee
            elif action in ('assigned', 'called_away', 'expired', 'forced_closed'):
                owned = shorts.pop(identity, None)
                if owned is None or owned['symbol'] != trade['symbol']:
                    raise ValueError('unowned lifecycle')
                symbol = trade['symbol']
                if action == 'assigned':
                    if owned['kind'] != 'put' or symbol in shares or price != owned['strike'] or flow != -price * 100 or fee:
                        raise ValueError('assignment accounting differs')
                    shares[symbol] = price
                    row['assignments'] += 1
                elif action == 'called_away':
                    if owned['kind'] != 'call' or symbol not in shares or price != owned['strike'] or flow != price * 100 or fee:
                        raise ValueError('call-away accounting differs')
                    row['stock_realized_cents'] += (price - shares.pop(symbol)) * 100
                    row['call_aways'] += 1
                elif action == 'expired':
                    if flow or fee:
                        raise ValueError('expiry has a cash flow')
                else:
                    if flow != -price * 100 - fee or fee != config['fee_per_contract_cents']:
                        raise ValueError('forced-close accounting differs')
                    row['fees_cents'] += fee
            else:
                raise ValueError('unknown trade action')
            row['cash_flow_cents'] += flow
            if action not in ('submit', 'day_order_expired'):
                last_economic_event = at
            next_trade = next(trades, None)
        dividend = cents(frame.get('dividend_cents', 0))
        if dividend:
            symbol = frame['dividend_symbol']
            if dividend < 0 or symbol not in shares or symbol not in rows:
                raise ValueError('dividend lacks owned stock')
            rows[symbol]['dividends_cents'] += dividend
            rows[symbol]['cash_flow_cents'] += dividend
            last_economic_event = at
        if frame['kind'] != 'valuation':
            continue
        if next_point is None or timestamp(next_point['at']) != at:
            raise ValueError('closing valuation differs from report')
        for row in rows.values():
            row.update(ending_stock_value_cents=0, ending_option_liability_cents=0)
        for symbol in shares:
            stock = stocks.get(symbol)
            if stock is None or not 0 <= at - timestamp(stock['at']) <= 30_000_000_000:
                raise ValueError('missing fresh stock closing mark')
            bid = cents(stock['bid_cents'])
            if bid <= 0:
                raise ValueError('invalid stock closing bid')
            rows[symbol]['ending_stock_value_cents'] = bid * 100
        for identity, owned in shorts.items():
            quote = quotes.get(identity)
            if (quote is None or quote['symbol'] != owned['symbol'] or quote['kind'] != owned['kind']
                    or cents(quote['strike_cents']) != owned['strike']
                    or not 0 <= at - timestamp(quote['at']) <= 30_000_000_000):
                raise ValueError('missing fresh short-option closing mark')
            ask = cents(quote['ask_cents'])
            if ask <= 0:
                raise ValueError('invalid short-option closing ask')
            rows[owned['symbol']]['ending_option_liability_cents'] += ask * 100
        cash = initial + sum(row['cash_flow_cents'] for row in rows.values())
        stock_value = sum(row['ending_stock_value_cents'] for row in rows.values())
        liability = sum(row['ending_option_liability_cents'] for row in rows.values())
        if (cash, stock_value, liability, cash + stock_value - liability) != tuple(
                cents(next_point[key]) for key in ('economic_cash_cents', 'stock_value_cents', 'option_liability_cents', 'equity_cents')):
            raise ValueError('symbol accounting does not reconcile to closing NAV')
        for symbol, row in rows.items():
            pnl = row['cash_flow_cents'] + row['ending_stock_value_cents'] - row['ending_option_liability_cents']
            row['net_pnl_cents'] = pnl
            peak[symbol] = max(peak[symbol], pnl)
            drawdown[symbol] = max(drawdown[symbol], peak[symbol] - pnl)
        count += 1
        final = next_point
        next_point = next(points, None)
    else:
        raise ValueError('replay frame bound exceeded')
    if digest.hexdigest() != report['input_sha256']:
        raise ValueError('replay input hash differs')
    if next_trade is not None or next_point is not None or final is None or count != report['observed_valuations']:
        raise ValueError('incomplete trade or valuation coverage')
    if last_economic_event is not None and last_economic_event > timestamp(final['at']):
        raise ValueError('economic event after the final NAV; ending attribution is undefined')
    metrics = report['performance']
    for key in ('submissions', 'fills', 'expired_orders', 'assignments', 'call_aways',
                'premiums_cents', 'fees_cents', 'dividends_cents', 'stock_realized_cents', 'net_pnl_cents'):
        if sum(row[key] for row in rows.values()) != cents(metrics[key]):
            raise ValueError('symbol totals differ from portfolio ' + key)
    if cents(final['equity_cents']) != cents(metrics['ending_equity_cents']):
        raise ValueError('ending equity differs')
    for symbol, row in rows.items():
        row.update(ending_economic_shares=100 if symbol in shares else 0,
                   ending_share_cost_cents=shares.get(symbol, 0),
                   ending_short_contracts=sum(owned['symbol'] == symbol for owned in shorts.values()),
                   portfolio_return_contribution_pct=row['net_pnl_cents'] / initial * 100,
                   max_peak_to_trough_pnl_cents=drawdown[symbol], zero_trade_symbol=row['fills'] == 0)
    return dict(status=report['status'], input_sha256=digest.hexdigest(),
                rows=[rows[symbol] for symbol in symbols], portfolio_metrics=metrics,
                ending_economic_cash_cents=final['economic_cash_cents'],
                ending_available_cash_cents=final['available_cash_cents'],
                max_closing_buying_power_utilization_pct=max(cents(point['committed_capital_cents']) for point in report['equity_curve']) / initial * 100)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for mode in ('conservative', 'balanced'):
        parser.add_argument('--' + mode + '-report', type=Path, required=True)
        parser.add_argument('--' + mode + '-qualification', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    if args.output.exists():
        raise ValueError('output must be new')
    root = Path(__file__).resolve().parents[1]
    script_sha = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
    checker = Path(__file__).with_name('check-selection-mode-completion.py').resolve()
    checker_sha = hashlib.sha256(checker.read_bytes()).hexdigest()
    baseline_path = checker.with_name('check-simulation-completion.py')
    baseline_sha = hashlib.sha256(baseline_path.read_bytes()).hexdigest()
    results, bindings = {}, []
    for mode in ('conservative', 'balanced'):
        report_path = getattr(args, mode + '_report')
        qualification_path = getattr(args, mode + '_qualification')
        report_raw, qualification_raw = report_path.read_bytes(), qualification_path.read_bytes()
        report, qualification = read_json(report_raw), read_json(qualification_raw)
        if (report.get('status') != 'counterfactual' or not report.get('performance')
                or report['spec']['strategy_config'].get('selection_mode') != mode
                or qualification.get('status') != 'qualified'):
            raise ValueError(mode + ': complete qualified historical report required')
        command = [sys.executable, str(checker), '--mode', mode, '--report', str(report_path.resolve()),
                   '--qualification', str(qualification_path.resolve())]
        completed = subprocess.run(command, cwd=root, capture_output=True)
        if completed.returncode:
            raise ValueError(mode + ': required completion checker did not pass')
        receipt = read_json(completed.stdout)
        if (receipt.get('complete') is not True or receipt.get('mode') != mode
                or receipt.get('units_remaining') != 0 or receipt.get('gaps') != []
                or hashlib.sha256(checker.read_bytes()).hexdigest() != checker_sha
                or hashlib.sha256(baseline_path.read_bytes()).hexdigest() != baseline_sha):
            raise ValueError(mode + ': invalid completion receipt')
        path = Path(qualification['normalized_input']['path'])
        if not path.is_absolute():
            path = root / path
        with path.open('rb') as source:
            results[mode] = attribute(report, source)
        if report_path.read_bytes() != report_raw or qualification_path.read_bytes() != qualification_raw:
            raise ValueError('reviewed input changed during attribution')
        bindings.append(dict(mode=mode, report_sha256=hashlib.sha256(report_raw).hexdigest(),
                             qualification_sha256=hashlib.sha256(qualification_raw).hexdigest(),
                             normalized_input_sha256=report['input_sha256'],
                             completion_command=command[1:], completion_receipt=receipt,
                             mode_checker_sha256=checker_sha, baseline_checker_sha256=baseline_sha))
    rows = [dict(mode=mode, **row) for mode in ('conservative', 'balanced') for row in results[mode]['rows']]
    if len(rows) != 30:
        raise ValueError('expected 30 symbol-by-mode attribution rows')
    if hashlib.sha256(Path(__file__).read_bytes()).hexdigest() != script_sha:
        raise ValueError('attribution source changed during execution')
    output = dict(schema_version='AutoWheelModePortfolioAttributionV1', status='counterfactual',
                  script_sha256=script_sha, bindings=bindings,
                  rows=rows, portfolios={mode: {k: v for k, v in result.items() if k != 'rows'} for mode, result in results.items()},
                  assumptions=['Rows attribute two full 15-symbol portfolios with at most three managed symbols; they are not isolated-symbol results.',
                               'Return contribution uses the entire $80,000 account denominator, not symbol-specific allocated capital.',
                               'Symbol drawdown is peak-to-trough cumulative P&L in cents at session closes; it is not additive portfolio percentage drawdown.',
                               'Economic shares and short contracts are reconstructed from modeled fills/lifecycles; they are not broker-confirmed inventory.',
                               'Buying-power utilization is observed at session closes; its intraday maximum may be higher.'])
    raw = (json.dumps(output, indent=2, sort_keys=True, allow_nan=False) + '\n').encode()
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'wb') as stream:
        stream.write(raw)
        stream.flush()
        os.fsync(stream.fileno())
    fd = os.open(args.output.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)
    print(json.dumps(dict(rows=30, portfolio_modes=2, isolated_symbol_results=False)))


if __name__ == '__main__':
    main()
