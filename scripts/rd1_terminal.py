"""Frozen-inventory contractual terminal scenarios, not option repricing.

Inputs come from an independently reconciled sample and externally admitted
standard terms/fees. This arithmetic does not confer that source admission.
No interpolated option marks, IV/rates, future trades or future dividends enter.
"""
from fractions import Fraction
import re

from rd1_storage import PROTOCOL_SHA256, canonical, require, sha

MAX_MONEY = 100_000_000_000


def integer(value, low=0, high=MAX_MONEY):
    require(type(value) is int and low <= value <= high, 'bounded exact integer required')
    return value


def ratio(value): return dict(numerator=value.numerator, denominator=value.denominator)


def terminal_scenarios(snapshot):
    """Evaluate 0%, -20%, -40% on one frozen standard-contract inventory.

    Loss is max(0, sampled NAV - shocked terminal equity)/sampled NAV. Also
    expose zero-gap terminal equity and shock-only differences to distinguish
    contractual time-value runoff from the stock shock. All strictly ITM shorts
    deliver at strike; equality lapses. This payoff convention is distinct from
    the primary replay's one-cent exercise/notice clock and is not an observed
    assignment reconstruction.

    Funding uses currently settled cash and never counts contingent call-away
    proceeds, dividend receivables or unsettled premium. All-in delivery fees
    must be explicit for each remaining short; no second stock-trade fee is
    added. Unknown fees/terms/marks produce insufficient scenario evidence.
    """
    result = dict(schema='AutoWheelRD1TerminalContractualArithmeticV1', protocol_sha256=PROTOCOL_SHA256,
        status='insufficient', metrics=None, reason=None,
        source_or_account_admission_performed=False, historical_performance_qualified=False,
        contemporaneous_option_NAV=False, IV_model_used=False, account_processes_started=0)
    try:
        require(type(snapshot) is dict and set(snapshot) == {'sample_ref', 'nav_cents', 'economic_cash_cents',
            'settled_cash_cents', 'receivables_cents', 'positions'}, 'complete frozen scenario snapshot required')
        require(type(snapshot['sample_ref']) is str and 1 <= len(snapshot['sample_ref']) <= 128,
                'scenario sample identity absent')
        nav = integer(snapshot['nav_cents'], 1); cash = integer(snapshot['economic_cash_cents'])
        settled = integer(snapshot['settled_cash_cents'], 0, cash); receivables = integer(snapshot['receivables_cents'])
        positions = snapshot['positions']; require(type(positions) is list and len(positions) <= 3,
            'scenario inventory exceeds frozen managed-symbol cap')
        symbols = set(); base = cash+receivables
        for position in positions:
            require(type(position) is dict and set(position) == {'symbol', 'shares', 'stock_bid_cents',
                'standard_terms_sha256', 'short'}, 'complete frozen position required')
            symbol = position['symbol']; require(type(symbol) is str and re.fullmatch('[A-Z]{1,5}', symbol)
                and symbol not in symbols, 'duplicate/invalid scenario symbol'); symbols.add(symbol)
            require(type(position['standard_terms_sha256']) is str
                and re.fullmatch('[0-9a-f]{64}', position['standard_terms_sha256']), 'standard terms evidence missing')
            shares = position['shares']; require(type(shares) is int and shares in (0, 100), 'unsupported deliverable quantity')
            spot = integer(position['stock_bid_cents'], 1, MAX_MONEY//100)
            base += shares*spot
            short = position['short']
            if short is not None:
                require(type(short) is dict and set(short) == {'kind', 'strike_cents', 'ask_cents',
                    'all_in_delivery_fee_cents', 'delivery_fee_source_sha256'}, 'complete short fee/mark/terms required')
                require(short['kind'] in ('put', 'call') and shares == (100 if short['kind'] == 'call' else 0),
                        'uncovered call or put/stock mixture unsupported')
                integer(short['strike_cents'], 1, MAX_MONEY//100)
                base -= 100*integer(short['ask_cents'], 1, MAX_MONEY//100)
                integer(short['all_in_delivery_fee_cents'], 0, 10000)
                require(type(short['delivery_fee_source_sha256']) is str
                    and re.fullmatch('[0-9a-f]{64}', short['delivery_fee_source_sha256']), 'dated delivery fee evidence missing')
            else: require(shares == 100, 'empty scenario position')
        require(base == nav and base <= MAX_MONEY, 'scenario does not reconcile to observed sampled NAV')
        scenarios = {}
        for gap in (0, 20, 40):
            equity = Fraction(cash+receivables); fees = 0; strike_funding = 0; assignments = []
            for position in positions:
                spot = Fraction(position['stock_bid_cents']*(100-gap), 100)
                equity += position['shares']*spot
                short = position['short']
                if short is None: continue
                strike = short['strike_cents']
                intrinsic = max(Fraction(0), strike-spot if short['kind'] == 'put' else spot-strike)
                if intrinsic == 0: continue
                fee = short['all_in_delivery_fee_cents']; fees += fee
                equity -= 100*intrinsic+fee
                if short['kind'] == 'put': strike_funding += 100*strike
                assignments.append(dict(symbol=position['symbol'], kind=short['kind'], strike_cents=strike,
                                        all_in_delivery_fee_cents=fee))
            require(equity.denominator == 1 and -MAX_MONEY <= equity <= MAX_MONEY,
                    'contractual aggregate cents outside exact envelope')
            required = strike_funding+fees
            scenarios['unchanged' if gap == 0 else 'minus'+str(gap)] = dict(
                stock_move_percent=-gap, terminal_equity_cents=equity.numerator,
                loss_from_sampled_NAV_cents=max(0, nav-equity.numerator),
                loss_fraction_of_sampled_NAV=ratio(Fraction(max(0, nav-equity.numerator), nav)),
                assignment_strike_cash_cents=strike_funding, delivery_fees_cents=fees,
                settled_cash_required_cents=required, currently_settled_cash_cents=settled,
                conservative_current_settled_funding_passed=settled >= required,
                short_funding_cents=max(0, required-settled), physical_delivery_assumptions=assignments)
        for value in scenarios.values():
            value['shock_only_equity_change_from_unchanged_cents'] = (
                value['terminal_equity_cents']-scenarios['unchanged']['terminal_equity_cents'])
        funded = all(x['conservative_current_settled_funding_passed'] for x in scenarios.values())
        result.update(status='arithmetic_complete_pending_external_admission' if funded else 'insufficient',
            reason=None if funded else 'current_settled_funding_insufficient_do_not_assume_contingent_proceeds',
            metrics=scenarios if funded else None, diagnostics=scenarios, snapshot_sha256=sha(canonical(snapshot)),
            sample_ref=snapshot['sample_ref'],
            scope='contractual_terminal_payoffs_not_contemporaneous_option_NAV_or_a_worst_loss_bound',
            funding_scope='conservative_current_settled_cash_check_not_a_future_settlement_path',
            fees_scope='explicit_all_in_delivery_fees_only_no_fee_on_retained_unsold_stock',
            receivables_scope='already_earned_receivables_only_no_future_dividends_or_interest')
    except (KeyError, TypeError, ValueError) as error:
        result.update(status='insufficient', metrics=None, reason=str(error)[:240])
    return result


def summarize_terminal_samples(snapshots, expected_sample_refs):
    """Take each shock's maximum loss across every declared near-close sample.

    Missing or unfunded samples keep the path insufficient; do not retain a
    favorable subset or replace a missing stress result by zero.
    """
    require(type(snapshots) is list and 1 <= len(snapshots) <= 1600
            and type(expected_sample_refs) is list
            and len(set(expected_sample_refs)) == len(expected_sample_refs)
            and [x.get('sample_ref') for x in snapshots] == expected_sample_refs,
            'missing, duplicate or reordered terminal scenario sample')
    rows = [terminal_scenarios(x) for x in snapshots]
    incomplete = [dict(sample_ref=ref, reason=row['reason']) for ref, row in zip(expected_sample_refs, rows)
                  if row['status'] != 'arithmetic_complete_pending_external_admission']
    result = dict(schema='AutoWheelRD1TerminalSeriesArithmeticV1', protocol_sha256=PROTOCOL_SHA256,
        status='insufficient' if incomplete else 'arithmetic_complete_pending_external_admission',
        samples=len(rows), incomplete_samples=incomplete, maximum_terminal_stress_loss=None,
        snapshots_sha256=sha(canonical(snapshots)), expected_sample_refs_sha256=sha(canonical(expected_sample_refs)),
        source_or_account_admission_performed=False, historical_performance_qualified=False)
    if not incomplete:
        maximum = {}
        for gap in ('minus20', 'minus40'):
            def loss(row):
                v = row['metrics'][gap]['loss_fraction_of_sampled_NAV']
                return Fraction(v['numerator'], v['denominator'])
            worst = max(rows, key=loss)
            maximum[gap] = dict(loss=ratio(loss(worst)), sample_ref=worst['sample_ref'])
        result.update(maximum_terminal_stress_loss={g:v['loss'] for g, v in maximum.items()}, worst_samples=maximum)
    return result
