"""Exact RD1 journal analytics; no source admission, replay or trading action.

Expected shortfall is descriptive: the exact worst 5% probability mass of the
observed daily returns, weighting its boundary observation fractionally. Cash
turnover uses gross executed premium/stock cash, excluding fees and dividends.
"""
from datetime import date, timedelta
from fractions import Fraction
from zoneinfo import ZoneInfo

from rd1_financial_verify import (_check_hash, _loads, _parse_journal, _parse_time,
    _read_bounded, MAX_JOURNAL_BYTES, MAX_RESULT_BYTES, verify)
from rd1_storage import PROTOCOL_SHA256, canonical, require, sha

SEED = 8_404_717
NY = ZoneInfo('America/New_York')
SCENARIOS = ('primary','execution_5m','execution_next_session','extra_half_spread_cost',
             'daily_ITM_assignment','pin_exercise','pin_no_exercise','settlement_plus_one_day')


def ratio(value):return dict(numerator=value.numerator, denominator=value.denominator)


def integer(value, low=0, high=100_000_000_000):
    require(type(value) is int and low <= value <= high, 'bounded exact metric integer required')
    return value


def at(value):
    parsed = _parse_time(value)
    require(parsed is not None, 'explicit supported metric timestamp required')
    return parsed


def session_calendar(rows):
    require(type(rows) is list and 0 < len(rows) <= 4000, 'bounded explicit exchange calendar required')
    sessions = {};prior = None
    for row in rows:
        day = date.fromisoformat(row['date'])
        require(str(day) == row['date'] and (prior is None or day == prior+timedelta(days=1)),
                'missing, repeated or unordered calendar day')
        prior = day;opened = row.get('open_at');closed = row.get('close_at')
        require(bool(opened) == bool(closed), 'partially defined session')
        if not opened:continue
        op, cl = at(opened), at(closed);local_open, local_close = op.astimezone(NY), cl.astimezone(NY)
        require(day.weekday() < 5 and local_open.date() == local_close.date() == day
                and (local_open.hour,local_open.minute,local_open.second,local_open.microsecond) == (9,30,0,0)
                and (local_close.hour,local_close.minute,local_close.second,local_close.microsecond) in ((13,0,0,0),(16,0,0,0)),
                'unsupported session clocks')
        sessions[str(day)] = (op,cl)
    return sessions, date.fromisoformat(rows[0]['date']), date.fromisoformat(rows[-1]['date'])


def expected_shortfall(returns):
    require(returns and all(isinstance(x, Fraction) for x in returns), 'complete exact daily returns required')
    ordered = sorted(returns);mass = Fraction(len(returns),20);whole = mass.numerator//mass.denominator
    total = sum(ordered[:whole], Fraction(0))
    if mass > whole:total += (mass-whole)*ordered[whole]
    mean = total/mass
    return dict(confidence=ratio(Fraction(19,20)),left_tail_mean_return=ratio(mean),
        nonnegative_expected_shortfall_loss=ratio(max(Fraction(0),-mean)),
        observed_days=len(returns),tail_observation_mass=ratio(mass),
        definition='exact_empirical_worst_5_percent_mass_fractional_boundary_weight',
        predictive_tail_or_VaR_claim=False)


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


def series_metrics(points, exchange_days, first, last, seed=SEED, *, scenario='primary'):
    require(type(scenario) is str and scenario in SCENARIOS, 'unknown or combined metric scenario')
    integer(seed,1);sessions,calendar_first,calendar_last = session_calendar(exchange_days)
    require(first in sessions and last in sessions and first <= last, 'path endpoints are not reviewed sessions')
    days = [d for d in sessions if first <= d <= last]
    expected = {}
    for day in days:
        for label,minutes in (('signal',31),('execution',26 if scenario=='execution_5m' else 30),('NAV',1)):
            expected[label+'-'+day] = (sessions[day][1]-timedelta(minutes=minutes),label,day)
    require(type(points) is list and len(points) == len(expected), 'missing or extra sampled NAV mark')
    seen = set();near = [];prior = None
    for point in points:
        ref = point['ref'];clock = at(point['at']);integer(point['nav_cents'])
        require(ref in expected and ref not in seen and clock == expected[ref][0]
                and (prior is None or clock > prior), 'wrong, repeated or reordered NAV clock')
        seen.add(ref);prior = clock
        if expected[ref][1] == 'NAV':near.append(dict(point,day=expected[ref][2]))
    require(seen == set(expected) and len(near) == len(days), 'incomplete sampled schedule')
    returns = [];previous = seed;blocks = {};undefined = []
    for point in near:
        value = point['nav_cents'];day = date.fromisoformat(point['day']);name = f'{day.year}-H{1 if day.month<=6 else 2}'
        if name not in blocks:blocks[name] = dict(id=name,starting_nav_cents=previous,ending_nav_cents=value,sessions=0)
        blocks[name]['ending_nav_cents'] = value;blocks[name]['sessions'] += 1
        rate = Fraction(value-previous,previous) if previous else None
        if rate is None:undefined.append(point['day'])
        returns.append(dict(date=point['day'],return_fraction=ratio(rate) if rate is not None else None,
                            unavailable_reason=None if rate is not None else 'zero_previous_near_close_NAV'))
        previous = value
    for name,block in blocks.items():
        year = int(name[:4]);half = int(name[-1])
        begin = date(year,1 if half==1 else 7,1);finish = date(year,6,30) if half==1 else date(year,12,31)
        known_sessions = [d for d in sessions if str(begin) <= d <= str(finish)]
        block['complete'] = (calendar_first <= begin and calendar_last >= finish and bool(known_sessions)
            and first <= known_sessions[0] and last >= known_sessions[-1] and block['sessions'] == len(known_sessions))
        opening = block['starting_nav_cents']
        block['unannualized_return'] = ratio(Fraction(block['ending_nav_cents']-opening,opening)) if opening else None
        block['unavailable_reason'] = None if opening else 'zero_starting_NAV'
    years = {};prior_nav = seed
    for point in near:
        year = point['day'][:4]
        if year not in years:years[year] = dict(year=year,starting_nav_cents=prior_nav,sessions=0)
        years[year]['ending_nav_cents'] = point['nav_cents'];years[year]['sessions'] += 1;prior_nav = point['nav_cents']
    for year,item in years.items():
        known = [d for d in sessions if d.startswith(year+'-')]
        item['complete_calendar_year'] = (calendar_first <= date(int(year),1,1)
            and calendar_last >= date(int(year),12,31) and first <= known[0] and last >= known[-1]
            and item['sessions'] == len(known))
        opening = item['starting_nav_cents'];ending = item['ending_nav_cents']
        item['unannualized_return'] = ratio(Fraction(ending-opening,opening)) if opening else None
        item['net_NAV_change_cents'] = ending-opening
        item['unavailable_reason'] = None if opening else 'zero_starting_NAV'
    sampled = drawdown(points,seed);near_dd = drawdown(near,seed)
    dd = Fraction(sampled['fraction']['numerator'],sampled['fraction']['denominator'])
    net = Fraction(previous-seed,seed)
    return dict(start=first,end=last,sensitivity=scenario,seed_cents=seed,ending_nav_cents=previous,net_profit_cents=previous-seed,
        net_return=ratio(Fraction(previous-seed,seed)),sessions=len(days),daily_returns=returns,
        maximum_sampled_drawdown=sampled,maximum_near_close_drawdown=near_dd,
        return_over_maximum_sampled_drawdown=ratio(net/dd) if dd else None,
        return_drawdown_ratio_unavailable_reason=None if dd else 'zero_sampled_drawdown',
        expected_shortfall=expected_shortfall([Fraction(x['return_fraction']['numerator'],x['return_fraction']['denominator']) for x in returns]) if not undefined else None,
        expected_shortfall_unavailable_reason='zero_previous_NAV_on:'+','.join(undefined) if undefined else None,
        halfyears=list(blocks.values()),years=list(years.values()),sampled_schedule_sha256=sha(canonical([
            dict(ref=p['ref'],at=at(p['at']).isoformat()) for p in points])),
        exact_closing_drawdown=None,intraday_maximum_drawdown=None,
        unavailable_drawdown_reason='only_declared_signal_execution_and_near_close_samples_are_observed')


def inventory_metrics(records):
    """Read verified economic inventory at near-close observations only."""
    positions = {};cycles = [];active = {};owned = covered = turnover = 0
    for record in records:
        e = record['effect'];action = e['action'];symbol = e.get('symbol');clock = e['at']
        if action == 'mark':
            if not e['ref'].startswith('NAV-'):continue
            for name,p in positions.items():
                if not p['shares']:continue
                owned += 100
                if p['short'] and p['short']['kind']=='call':covered += 100
                cycle = active[name];cycle['owned_sessions'] += 1
                bid = integer(record['mark']['stock_books'][name]['bid_cents'],1)
                cycle['underwater_sessions'] += 100*bid < p['cost']
            continue
        if action in ('sell_put','sell_call','close_short','buy_stock'):
            turnover += 100*integer(e['price_cents'])
        if action == 'sell_put':positions[symbol] = dict(shares=0,cost=0,short=e['contract'])
        elif action == 'sell_call':positions[symbol]['short'] = e['contract']
        elif action == 'buy_stock':
            cost = 100*e['price_cents']+e['fee_cents']
            positions[symbol] = dict(shares=100,cost=cost,short=None)
            active[symbol] = dict(symbol=symbol,opened_at=clock,origin='direct_stock_purchase',
                                 acquisition_cost_cents=cost,owned_sessions=0,underwater_sessions=0)
        elif action == 'close_short':
            positions[symbol]['short'] = None
            if positions[symbol]['shares']==0:del positions[symbol]
        elif action == 'resolve':
            outcome = e['outcome'];p = positions[symbol];p['short'] = None
            if outcome in ('assigned','called_away'):turnover += 100*e['contract']['strike_cents']
            if outcome == 'assigned':
                cost = 100*e['contract']['strike_cents']+e['fee_cents'];p.update(shares=100,cost=cost)
                active[symbol] = dict(symbol=symbol,opened_at=clock,origin='put_assignment',
                    acquisition_cost_cents=cost,owned_sessions=0,underwater_sessions=0)
            elif outcome == 'called_away':
                cycle = active.pop(symbol);proceeds = 100*e['contract']['strike_cents']-e['fee_cents']
                cycles.append(dict(cycle,closed_at=clock,status='completed',stock_only_realized_pnl_cents=proceeds-p['cost']))
                del positions[symbol]
            elif p['shares']==0:del positions[symbol]
    cycles += [dict(value,closed_at=None,status='censored',stock_only_realized_pnl_cents=None) for value in active.values()]
    return dict(gross_traded_cash_cents=integer(turnover),turnover_over_seed=ratio(Fraction(turnover,SEED)),
        turnover_definition='gross_executed_premium_and_stock_cash_including_physical_strike_delivery_excluding_fees_dividends',
        owned_share_session_call_coverage=dict(owned_share_sessions=owned,covered_share_sessions=covered,
            ratio=ratio(Fraction(covered,owned)) if owned else None,reason=None if owned else 'no_owned_share_session_observations'),
        stock_cycles=cycles,completed_stock_cycles=sum(x['status']=='completed' for x in cycles),
        censored_stock_cycles=sum(x['status']=='censored' for x in cycles),
        underwater_definition='near_close_stock_bid_value_below_acquisition_cost_including_entry_or_assignment_fee',
        coverage_and_holding_time_scope='near_close_owned_share_session_observations_not_continuous_intraday_time')


def cash_exposure(records,opening_at,cutoff_at):
    """Exact ACT/365F area under the verified economic-cash timeline."""
    prior = at(opening_at);finish = at(cutoff_at);cash = SEED;area = 0
    require(prior < finish, 'cash exposure needs a positive matched interval')
    for record in records:
        clock = at(record['effect']['at']);require(prior <= clock <= finish, 'cash event outside matched interval')
        elapsed = clock-prior;micros = (elapsed.days*86400+elapsed.seconds)*1_000_000+elapsed.microseconds
        area += cash*micros;cash = integer(record['cash_after_cents']);prior = clock
    elapsed = finish-prior;area += cash*((elapsed.days*86400+elapsed.seconds)*1_000_000+elapsed.microseconds)
    return dict(economic_cash_cents_years=ratio(Fraction(area,365*86400*1_000_000)),
        opening_at=opening_at,cutoff_at=cutoff_at,day_count='ACT/365F',
        interest_eligibility='hypothetical_all_economic_cash_including_reserved_and_unsettled_not_verified_broker_sweep_eligibility',
        interest_reinvested=False,policy_paths_recomputed=False)


def paired_cash_yield_break_even(left_profit,right_profit,left,right):
    integer(left_profit,-100_000_000_000);integer(right_profit,-100_000_000_000)
    require((at(left['opening_at']),at(left['cutoff_at'])) == (at(right['opening_at']),at(right['cutoff_at']))
            and left['day_count'] == right['day_count'] == 'ACT/365F'
            and left['interest_eligibility'] == right['interest_eligibility'], 'cash-yield intervals or assumptions differ')
    def fraction(value):return Fraction(value['numerator'],value['denominator'])
    exposure = fraction(left['economic_cash_cents_years'])-fraction(right['economic_cash_cents_years'])
    profit = left_profit-right_profit
    return dict(left_minus_right_profit_cents=profit,left_minus_right_cash_cents_years=ratio(exposure),
        constant_annual_yield=ratio(Fraction(-profit)/exposure) if exposure else None,
        solution='unique' if exposure else 'all_yields_tie' if profit==0 else 'no_solution',
        unavailable_reason='zero_paired_cash_exposure_difference' if not exposure else None,
        fixed_paths_only=True,interest_reinvested=False,attainable_broker_yield_claim=False)


def analyze(journal_path,journal_sha256,result_path,result_sha256,packet_path,packet_sha256, *, expected_sensitivity='primary'):
    financial = verify(journal_path,journal_sha256,result_path,result_sha256)
    report = dict(schema='AutoWheelRD1CaseAnalyticsArithmeticV1',status='insufficient',metrics=None,
        protocol_sha256=PROTOCOL_SHA256,financial_verification=financial,source_qualification_performed=False,
        independent_policy_and_settlement_verification_complete=False,historical_performance_qualified=False)
    if not financial['qualified']:return report
    try:
        raw = _read_bounded(packet_path,256<<20,'packet');_check_hash(raw,packet_sha256,'packet');packet = _loads(raw)
        require(packet['protocol_sha256']==PROTOCOL_SHA256 and packet['admission_status']=='admitted_under_RD1'
                and packet['unqualified_inputs']==[],'packet admission unknown')
        raw = _read_bounded(journal_path,MAX_JOURNAL_BYTES,'journal');_check_hash(raw,journal_sha256,'journal')
        records = _parse_journal(raw)
        raw = _read_bounded(result_path,MAX_RESULT_BYTES,'result');_check_hash(raw,result_sha256,'result');result = _loads(raw)
        require(expected_sensitivity in SCENARIOS and result.get('sensitivity','primary') == expected_sensitivity,
                'result sensitivity differs from the independently bound case')
        points = [dict(ref=r['effect']['ref'],at=r['effect']['at'],nav_cents=r['mark']['NAV_cents'])
                  for r in records if r['effect']['action']=='mark']
        metrics = series_metrics(points,packet['exchange_days'],packet['first_session'],packet['last_session'],
                                 scenario=expected_sensitivity)
        require(metrics['net_profit_cents']==financial['metrics']['net_profit_cents']
                and metrics['sessions']==result['sessions_marked'],'money and schedule totals differ')
        metrics.update(inventory_metrics(records));metrics['financial_components'] = financial['metrics']
        metrics['censored_pending_attempts']=result.get('pending_attempts') or []
        metrics['censored_pending_count']=len(metrics['censored_pending_attempts'])
        metrics['pending_scope']='unexecuted_plans_beyond_cutoff_excluded_from_fills_fees_and_traded_cash'
        sessions,_,_ = session_calendar(packet['exchange_days'])
        metrics['cash_exposure'] = cash_exposure(records,sessions[packet['first_session']][0].isoformat(),
                                                result['last_near_close_at'])
        report.update(status='arithmetic_complete_pending_external_admission',metrics=metrics,
            journal_sha256=journal_sha256,result_sha256=result_sha256,packet_sha256=packet_sha256)
    except Exception as error:
        report.update(status='insufficient',metrics=None,reason='analytics_rejected:'+type(error).__name__+':'+str(error)[:240])
    return report
