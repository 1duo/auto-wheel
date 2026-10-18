"""Exact-cent research tax scenarios, not a personal tax return or tax engine.

Tax accounting uses future replacement acquisitions only to measure wash-loss
deferral. It never changes a policy decision. All gains and dividends receive
the requested short-term/ordinary treatment. A second scenario denies every
capital-loss benefit to expose dependence on unresolved tax loss treatment.
"""
from collections import Counter, defaultdict
from datetime import date
from fractions import Fraction
import hashlib
import json
from pathlib import Path


SEED = 8_404_717
MODEL_VERSION = "highest-ST-ordinary-v1"


def integer(value):
    require(type(value) is int and abs(value)<=100_000_000_000, "bounded integer cents required")
    return value


def ratio(value):
    return dict(numerator=value.numerator, denominator=value.denominator)


def drawdown(points, seed):
    peak = seed;peak_at = None;worst = Fraction(0);maximum_loss = 0
    detail = dict(fraction=ratio(worst),loss_cents=0,peak_nav_cents=seed,peak_at=None,trough_at=None)
    for point in points:
        value = integer(point['nav_cents']);clock = point['at']
        if value > peak:peak = value;peak_at = clock
        loss = peak-value;maximum_loss = max(maximum_loss,loss);fraction = Fraction(loss,peak)
        if fraction > worst:
            worst = fraction;detail = dict(fraction=ratio(fraction),loss_cents=loss,peak_nav_cents=peak,
                                           peak_at=peak_at,trough_at=clock)
    detail['maximum_dollar_drawdown_cents'] = maximum_loss
    detail['peak_clock_null_means_initial_seed'] = True
    return detail



def require(ok, message):
    if not ok:
        raise ValueError(message)


def read(ref):
    raw = Path(ref['file']).read_bytes()
    require(hashlib.sha256(raw).hexdigest() == ref['sha256'], 'source hash mismatch')
    return raw


def rounded_tax(cents, basis_points):
    require(type(cents) is int and type(basis_points) is int, 'integer tax inputs required')
    value = (abs(cents) * basis_points + 5000) // 10000
    return value if cents >= 0 else -value


def tax_schedule(years, ca=0, deny_losses=False):
    carry = 0
    total = 0
    out = []
    for year, row in sorted(years.items()):
        incoming = carry
        capital = row['positive_capital_cents'] if deny_losses else row['capital_cents'] - carry
        deduction = min(300_000, -capital) if capital < 0 else 0
        carry = max(0, -capital - deduction) if not deny_losses else 0
        regular = rounded_tax(max(0, capital), 3700 + ca) - rounded_tax(deduction, 3700 + ca)
        niit = rounded_tax(max(0, capital), 380)
        dividend = rounded_tax(row['dividend_cents'], 4080 + ca)
        charge = regular + niit + dividend
        total += charge
        out.append(dict(year=year, **row, incoming_capital_loss_carry_cents=incoming,
                        taxable_net_capital_cents=capital, ordinary_loss_deduction_cents=deduction,
                        outgoing_capital_loss_carry_cents=carry, modeled_tax_cents=charge))
    return total, out


def tax_ledger(records, result, *, wash=True):
    scale = result.get('modeled_costs', {}).get('scale', 1)
    require(scale in (1,2), 'unknown modeled closing-fee scale')
    acquisitions = defaultdict(list)
    for index, row in enumerate(records):
        e = row['effect']
        if e['action'] == 'buy_stock' or e['action'] == 'resolve' and e['outcome'] == 'assigned':
            acquisitions[e['symbol']].append((index, date.fromisoformat(e['at'][:10])))
    deferred = defaultdict(int)
    positions = {}
    events = []
    marks = []
    wash_sales = []
    stock_sales = []
    calls = []
    symbol_capital = Counter()
    capital = dividends_paid = receivables = 0
    final_mark = None
    for index, row in enumerate(records):
        e = row['effect']; action = e['action']; symbol = e.get('symbol'); at = e['at']
        receivables += row['receivable_delta_cents']
        cap = div = raw_positive = 0
        if action == 'mark':
            final_mark = row['mark']
            collateral = sum(100*p['short']['contract']['strike_cents'] for p in positions.values()
                             if p['short'] and p['short']['contract']['kind'] == 'put')
            marks.append(dict(ref=e['ref'], at=at, nav_cents=final_mark['NAV_cents'],
                              economic_cash_cents=row['cash_after_cents'], put_collateral_cents=collateral))
            continue
        if action == 'buy_stock':
            require(symbol not in positions, 'overlapping stock acquisition')
            positions[symbol] = dict(basis=100*e['price_cents']+e['fee_cents']+deferred[index],
                                     shares=100, short=None)
        elif action in ('sell_put', 'sell_call'):
            if action == 'sell_put':
                require(symbol not in positions, 'overlapping put')
                positions[symbol] = dict(basis=0, shares=0, short=None)
            p = positions[symbol]
            require(p['short'] is None, 'overlapping short')
            p['short'] = dict(contract=e['contract'], net=100*e['price_cents']-e['fee_cents'])
            if action == 'sell_call':
                calls.append(dict(symbol=symbol, at=at, contract=e['contract']['id'],
                                  DTE=(date.fromisoformat(e['contract']['expiry'])-date.fromisoformat(at[:10])).days))
        elif action == 'close_short':
            p = positions[symbol]; short = p['short']
            require(short['contract'] == e['contract'], 'short identity mismatch')
            cap = short['net']-100*e['price_cents']-e['fee_cents']
            raw_positive = max(0, cap)
            p['short'] = None
            if not p['shares']:
                del positions[symbol]
        elif action == 'resolve':
            p = positions[symbol]; short = p['short']
            require(short['contract'] == e['contract'], 'resolution identity mismatch')
            if e['outcome'] == 'assigned':
                p['basis'] = 100*e['contract']['strike_cents']+e['fee_cents']-short['net']+deferred[index]
                p['shares'] = 100
            elif e['outcome'] == 'called_away':
                cap = 100*e['contract']['strike_cents']-e['fee_cents']+short['net']-p['basis']
                raw_positive = max(0, cap)
                sale = dict(symbol=symbol, at=at, tax_PnL_before_current_wash_cents=cap,
                            adjusted_stock_basis_cents=p['basis'], ref=e['ref'], replacement_at=None)
                if cap < 0:
                    day = date.fromisoformat(at[:10])
                    replacements = [(j, d) for j, d in acquisitions[symbol] if j > index and 0 <= (d-day).days <= 30]
                    if replacements:
                        j, replacement = replacements[0]
                        sale['replacement_at'] = str(replacement)
                        if wash:
                            deferred[j] += -cap
                            wash_sales.append(dict(sale, deferred_loss_cents=-cap))
                            cap = 0
                stock_sales.append(sale)
                p['shares'] = 0
            else:
                require(e['outcome'] == 'expired', 'unknown option outcome')
                cap = short['net']-e['fee_cents']
                raw_positive = max(0, cap)
            p['short'] = None
            if not p['shares']:
                del positions[symbol]
        elif action == 'pay_dividend':
            div = row['cash_delta_cents']
        else:
            require(action == 'dividend', 'unknown tax effect')
        require(all(abs(v) <= 100_000_000_000 for v in [cap, div, capital+cap, dividends_paid+div]), 'tax money envelope')
        capital += cap; dividends_paid += div; symbol_capital[symbol] += cap
        if cap or div or raw_positive:
            events.append(dict(index=index, at=at, capital_cents=cap, dividend_cents=div,
                               positive_capital_cents=raw_positive, action=action, symbol=symbol))
    require(final_mark is not None and final_mark['NAV_cents'] == result['last_near_close_NAV_cents'], 'terminal mark mismatch')
    unrealized_capital = 0
    closing_fees = 0
    terminal_gains = []
    for symbol, p in positions.items():
        if p['shares']:
            pnl = 100*final_mark['stock_books'][symbol]['bid_cents']-p['basis']
            unrealized_capital += pnl; closing_fees += 100*scale
            terminal_gains.append(pnl-100*scale)
        if p['short']:
            s = p['short']; pnl = s['net']-100*final_mark['option_books'][s['contract']['id']]['ask_cents']
            unrealized_capital += pnl; closing_fees += 65*scale
            terminal_gains.append(pnl-65*scale)
    require(capital+dividends_paid+receivables+unrealized_capital == result['net_profit_cents'],
            'tax basis/realized/unrealized identity does not reconcile with NAV')
    return dict(events=events, marks=marks, wash_sales=wash_sales, stock_sales=stock_sales,
                calls=calls, capital_cents=capital, dividends_paid_cents=dividends_paid,
                symbol_realized_capital_cents=dict(symbol_capital), remaining_receivables_cents=receivables,
                unrealized_capital_cents=unrealized_capital, liquidation_increment_capital_cents=sum(terminal_gains),
                liquidation_positive_capital_cents=sum(max(0, v) for v in terminal_gains),
                modeled_terminal_closing_fees_cents=closing_fees, tax_NAV_identity_difference_cents=0)


def after_tax(ledger, *, ca=0, deny_losses=False):
    years = defaultdict(lambda: dict(capital_cents=0, positive_capital_cents=0, dividend_cents=0))
    points = []
    cursor = 0
    minimum_cash_minus_tax_and_collateral = None
    for mark in ledger['marks']:
        while cursor < len(ledger['events']) and ledger['events'][cursor]['at'] <= mark['at']:
            e = ledger['events'][cursor]; row = years[e['at'][:4]]
            for k in row:
                row[k] += e[k]
            cursor += 1
        charge, _ = tax_schedule(years, ca, deny_losses)
        points.append(dict(ref=mark['ref'], at=mark['at'], nav_cents=mark['nav_cents']-charge))
        margin = mark['economic_cash_cents']-mark['put_collateral_cents']-max(0, charge)
        minimum_cash_minus_tax_and_collateral = margin if minimum_cash_minus_tax_and_collateral is None else min(margin, minimum_cash_minus_tax_and_collateral)
    charge, schedule = tax_schedule(years, ca, deny_losses)
    last = ledger['marks'][-1]
    liquid_years = {k: dict(v) for k, v in years.items()}
    row = liquid_years.setdefault(last['at'][:4], dict(capital_cents=0, positive_capital_cents=0, dividend_cents=0))
    row['capital_cents'] += ledger['liquidation_increment_capital_cents']
    row['positive_capital_cents'] += ledger['liquidation_positive_capital_cents']
    row['dividend_cents'] += ledger['remaining_receivables_cents']
    liquidation_tax, _ = tax_schedule(liquid_years, ca, deny_losses)
    liquidation_nav = last['nav_cents']-ledger['modeled_terminal_closing_fees_cents']-liquidation_tax
    year_returns = []
    previous = SEED
    by_year = {}
    for point in points:
        if point['ref'].startswith('NAV-'):
            by_year[point['at'][:4]] = point['nav_cents']
    for year, ending in by_year.items():
        year_returns.append(dict(year=year, return_pct=100*(ending-previous)/previous))
        previous = ending
    return dict(regular_rate_bps=3700+ca, NIIT_bps=380, deny_all_capital_loss_benefits=deny_losses,
                modeled_tax_cents=charge, tax_years=schedule, after_tax_return_pct=100*(points[-1]['nav_cents']-SEED)/SEED,
                after_tax_profit_cents=points[-1]['nav_cents']-SEED,
                maximum_sampled_after_tax_drawdown=drawdown(points, SEED),
                terminal_liquidation_after_tax_return_pct=100*(liquidation_nav-SEED)/SEED,
                terminal_liquidation_tax_cents=liquidation_tax,
                minimum_economic_cash_minus_tax_and_put_collateral_cents=minimum_cash_minus_tax_and_collateral,
                funding_scope='economic cash only; unsettled locks and rejected-order counterfactuals require separate checks',
                years=year_returns)


def analyze_records(records, result):
    """Tax arithmetic for a reconciled complete RD1 account; source admission is separate."""
    require(result.get("status")=="complete", "incomplete account cannot be scored")
    integer(result["net_profit_cents"])
    require(result["last_near_close_NAV_cents"]-result["net_profit_cents"]==SEED, "unsupported initial seed")
    ledger = tax_ledger(records, result)
    raw = tax_ledger(records, result, wash=False)
    scenarios = {}
    for name, ca in [('federal', 0), ('federal_CA', 1330)]:
        scenarios[name+'_carry'] = after_tax(ledger, ca=ca)
        scenarios[name+'_deny_losses'] = after_tax(raw, ca=ca, deny_losses=True)
    calls = ledger['calls']; sales = ledger['stock_sales']
    return dict(tax_model_version=MODEL_VERSION, pretax_profit_cents=result['net_profit_cents'],
                pretax_return_pct=100*result['net_profit_cents']/SEED,
                pretax_sampled_drawdown=drawdown(ledger['marks'], SEED),
                calls=len(calls), calls_DTE_30_or_less=sum(x['DTE'] <= 30 for x in calls),
                call_DTE_distribution=dict(sorted(Counter(x['DTE'] for x in calls).items())),
                closed_stock_tax_losses=sum(x['tax_PnL_before_current_wash_cents'] < 0 for x in sales),
                stock_sales=len(sales), wash_sale_count=len(ledger['wash_sales']),
                wash_deferred_loss_cents=sum(x['deferred_loss_cents'] for x in ledger['wash_sales']),
                tax_NAV_identity_difference_cents=ledger['tax_NAV_identity_difference_cents'],
                symbol_realized_capital_cents=ledger['symbol_realized_capital_cents'],
                scenarios=scenarios, detailed_ledger=ledger)
