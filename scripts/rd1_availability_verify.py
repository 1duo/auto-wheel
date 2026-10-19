"""Independent RD1 resource timing audit of a reconciled journal.

This verifies supplied-calendar arithmetic, cash/share locks, reserve admission,
notice and reentry timing. It does not qualify calendars, fees, source prices,
deliverables, strategy signals, or actual broker availability.
"""
from datetime import date, timedelta
from copy import deepcopy

from rd1_financial_verify import (_check_hash, _loads, _parse_journal,
    _read_bounded, MAX_JOURNAL_BYTES, MAX_RESULT_BYTES, verify)
from rd1_metrics import NY, SEED, SCENARIOS, at, integer, session_calendar
from rd1_storage import PROTOCOL_SHA256, require


def calendars(exchange, clearing):
    sessions, _, _ = session_calendar(exchange)
    require(type(clearing) is list and 0 < len(clearing) <= 4000,
            'bounded explicit clearing calendar required')
    days = {};prior = None
    for row in clearing:
        day = date.fromisoformat(row['date'])
        require(str(day) == row['date'] and (prior is None or day == prior+timedelta(days=1))
                and type(row['open']) is bool, 'missing, repeated or ambiguous clearing day')
        prior = day;release = row.get('available_at')
        require(bool(release) == row['open'], 'clearing clock differs from open declaration')
        if release:
            value = at(release).astimezone(NY)
            require(day.weekday() < 5 and value.date() == day
                    and (value.hour,value.minute,value.second,value.microsecond) == (9,30,0,0),
                    'unsupported modeled clearing availability clock')
        days[str(day)] = at(release) if release else None
    return sessions, days


def settlement(clock, flow, days):
    day = clock.astimezone(NY).date()
    require(str(day) in days and flow in ('option','stock','physical'),
            'transaction outside reviewed clearing calendar')
    remaining = 1 if flow == 'option' or str(day) >= '2024-05-28' else 2
    while remaining:
        day += timedelta(days=1)
        require(str(day) in days, 'missing clearing day; availability remains unknown')
        if days[str(day)] is not None:remaining -= 1
    return days[str(day)]


def delay_availability(base, days):
    """Advance one supplied clearing day, preserving a later notice clock."""
    local = base.astimezone(NY); day = local.date()
    while True:
        day += timedelta(days=1)
        require(str(day) in days, 'extra availability day unknown')
        if days[str(day)] is not None:
            next_open = days[str(day)]
            same_clock = next_open.astimezone(NY).replace(hour=local.hour,minute=local.minute,
                second=local.second,microsecond=local.microsecond)
            return max(next_open,same_clock)


def availability(records, state, exchange, clearing, *, scenario='primary', dividend_terms=None, trace=None, capital_limits=(8_000_000,2_666_666)):
    """Pure timing audit; financial verification must precede use of its output."""
    sessions, days = calendars(exchange, clearing)
    require(capital_limits in ((8_000_000,2_666_666),(20_000_000,20_000_000)), 'unknown capital policy')
    account_cap,symbol_cap=capital_limits
    require(type(scenario) is str and scenario in SCENARIOS, 'unknown or combined availability scenario')
    def delay(base):return delay_availability(base,days) if scenario=='settlement_plus_one_day' else base
    def settle(clock,flow):return delay(settlement(clock,flow,days))
    supplied_dividends = {d['id']:d for d in dividend_terms} if dividend_terms is not None else None
    require(dividend_terms is None or len(supplied_dividends)==len(dividend_terms),'duplicate source dividend')
    indices = {day:i for i,day in enumerate(sessions)}
    declared_days = {row['date']:row for row in exchange}
    positions = {};locks = [];fences = {};last_policy = {};dividends = {}
    require(trace is None or type(trace) is dict and not trace, 'availability trace must start empty')
    if trace is not None:trace.update(snapshots={},locks=locks)
    cash = SEED;prior = None;checked_policies = 0;checked_resolutions = 0
    def resources(clock):
        settled = cash-sum(x['amount'] for x in locks if clock < x['until'])
        collateral = sum(100*p['short']['strike_cents'] for p in positions.values()
                         if p['short'] and p['short']['kind'] == 'put')
        require(0 <= collateral <= settled, 'unavailable cash or uncovered put collateral')
        exposure = sum(p['cost'] if p['shares'] else 100*p['short']['strike_cents']
                       for p in positions.values())
        return settled, collateral, exposure
    for record_index,record in enumerate(records):
        e = record['effect'];action = e['action'];clock = at(e['at']);symbol = e.get('symbol')
        require(prior is None or clock >= prior, 'reordered resource event');prior = clock
        settled, collateral, exposure = resources(clock)
        if trace is not None and (action in ('sell_put','sell_call','buy_stock','close_short') or action=='mark'):
            trace['snapshots'][record_index]=dict(cash=cash,lock_count=len(locks),
                positions=deepcopy(positions),fences=dict(fences))
        if action == 'mark':
            require(record['cash_after_cents'] == cash, 'mark changes economic cash')
            continue
        local_day = str(clock.astimezone(NY).date());p = positions.get(symbol)
        policy = action in ('sell_put','sell_call','buy_stock','close_short')
        if action != 'pay_dividend':require(local_day in sessions, 'effect on non-session day')
        if policy:
            opened, closed = sessions[local_day];index = indices[local_day]
            signal = at(e['signal_at']);signal_day = str(signal.astimezone(NY).date())
            require(signal_day in sessions and signal == sessions[signal_day][1]-timedelta(minutes=31)
                    and clock == closed-timedelta(minutes=26 if scenario=='execution_5m' else 30)
                    and index == indices[signal_day]+(1 if scenario=='execution_next_session' else 0),
                    'policy left its separately declared signal/execution buckets')
            require(last_policy.get(symbol) != index, 'second policy transaction in one session')
            require(all(not (at(h['from']) <= clock < at(h['through']))
                        for h in declared_days[local_day].get('declared_trading_halts',[])),
                    'financial execution during declared halt')
            if action != 'close_short':
                monday = signal.astimezone(NY).date()
                monday -= timedelta(days=monday.weekday())
                while str(monday) < signal_day:
                    require(str(monday) in declared_days, 'weekly entry has unknown preceding day')
                    require(str(monday) not in sessions, 'entry after an earlier session of its week')
                    monday += timedelta(days=1)
                if symbol in fences:
                    require(indices[signal_day] >= fences[symbol][0] and signal >= fences[symbol][1],
                            'entry crosses intervening-session or notice fence')
                addition = 100*e['contract']['strike_cents'] if action == 'sell_put' else 0
                debit = integer(e['fee_cents'])
                if action == 'buy_stock':addition = debit = 100*e['price_cents']+debit
                reserve = collateral+(addition if action == 'sell_put' else 0)
                require(settled-debit-reserve >= 1_680_944 and exposure+addition <= account_cap
                        and (p['cost'] if p else 0)+addition <= symbol_cap
                        and (p is not None or len(positions) < 3), 'entry resource limit violated')
            else:
                require(p and p['short'], 'managed close lacks an owned short')
                other = collateral-(100*p['short']['strike_cents'] if p['short']['kind']=='put' else 0)
                require(settled-other >= 100*e['price_cents']+e['fee_cents'],
                        'managed close uses unavailable funds')
            last_policy[symbol] = index;checked_policies += 1
        if action in ('sell_put','sell_call'):
            if action == 'sell_put':
                require(p is None, 'put starts an occupied lifecycle')
                p = dict(shares=0,cost=0,available=None,short=None,profile=e.get('profile'),net=0);positions[symbol] = p
            else:
                require(p and p['shares'] == 100 and p['short'] is None
                        and p['available'] is not None and clock >= p['available'],
                        'call uses unavailable shares')
            p['short'] = e['contract']
            p['net'] = 100*e['price_cents']-e['fee_cents']
            locks.append(dict(ref=e['ref'],amount=100*e['price_cents'],until=settle(clock,'option')))
        elif action == 'buy_stock':
            require(p is None, 'stock purchase starts an occupied lifecycle')
            positions[symbol] = dict(shares=100,cost=100*e['price_cents']+e['fee_cents'],
                                     available=settle(clock,'stock'),short=None,profile=e.get('profile'),net=0)
        elif action == 'close_short':
            p['short'] = None;fences[symbol] = (indices[local_day]+2,clock)
            if not p['shares']:del positions[symbol]
        elif action == 'resolve':
            require(p and p['short'] == e['contract'] and clock == sessions[local_day][1]
                    and (e['contract']['expiry'] == local_day or
                         scenario=='daily_ITM_assignment' and e['contract']['expiry'] > local_day
                         and e['outcome'] in ('assigned','called_away')), 'exercise identity/clock differs')
            following = next((day for day in sessions if day > local_day),None)
            require(following is not None, 'missing following exchange session for expiry notice')
            notice = sessions[following][0]+timedelta(minutes=5)
            require(at(e['notice_at']) == notice, 'expiry notice is not next session open plus five minutes')
            fences[symbol] = (indices[local_day]+2,notice);p['short'] = None
            if e['outcome'] == 'expired':
                if not p['shares']:
                    locks.append(dict(ref=e['ref'],amount=100*e['contract']['strike_cents'],until=delay(notice)))
                else:p['available'] = max(p['available'],delay(notice))
            else:
                require(e['outcome'] in ('assigned','called_away') and e['fee_cents'] <= settled-collateral,
                        'physical exercise lacks settled fee funds')
                release = delay(max(settlement(clock,'physical',days),notice))
                if e['outcome'] == 'assigned':
                    p.update(shares=100,cost=100*e['contract']['strike_cents']+e['fee_cents'],available=release)
                else:
                    require(p['shares'] == 100 and clock >= p['available'], 'call-away uses unavailable shares')
                    p.update(shares=0,cost=0)
                    locks.append(dict(ref=e['ref'],amount=100*e['contract']['strike_cents'],until=release))
            if not p['shares']:del positions[symbol]
            checked_resolutions += 1
        elif action == 'dividend':
            require(e['dividend_id'] not in dividends, 'repeated dividend entitlement')
            require(scenario!='settlement_plus_one_day' or supplied_dividends is not None,
                    'source dividend availability needed for extra-day audit')
            if supplied_dividends is not None:
                term = supplied_dividends.get(e['dividend_id'])
                require(term and term['symbol']==symbol and p and p['shares']==100
                        and at(term['ex_at'])==clock and at(term['known_at'])<=clock
                        and term['amount_for_100_shares_cents']==e['price_cents']
                        and at(term['pay_at'])==at(e['payment_at'])
                        and delay(at(term['available_at']))==at(e['available_at']),
                        'modeled dividend clock/amount differs from dated source terms')
            dividends[e['dividend_id']] = e
        elif action == 'pay_dividend':
            d = dividends.get(e['dividend_id'])
            require(d and clock == at(d['payment_at']), 'unknown or mistimed dividend payment')
            if clock < at(d['available_at']):
                locks.append(dict(ref=e['ref'],amount=e['price_cents'],until=at(d['available_at'])))
        else:raise ValueError('unsupported timing action')
        cash = integer(record['cash_after_cents']);resources(clock)
    require(set(state['positions']) == set(positions), 'final lifecycle set differs')
    for symbol,p in positions.items():
        reported = state['positions'][symbol];release = reported.get('stock_available_at')
        require(reported['shares'] == p['shares'] and reported['stock_acquisition_cents'] == p['cost']
                and (at(release) if release else None) == p['available'], 'final stock availability differs')
    reported = state['cash_locks'] or []
    require(len(reported) == len(locks) and all(x['ref'] == y['ref'] and x['amount_cents'] == y['amount']
                and at(x['available_at']) == y['until'] for x,y in zip(reported,locks,strict=True)),
            'final cash lock history differs')
    require(state['last_policy_session'] == last_policy, 'final policy-session map differs')
    require(set(state['reentry_fences']) == set(fences), 'final reentry-fence set differs')
    for symbol,(index,clock) in fences.items():
        reported = state['reentry_fences'][symbol]
        require(reported['earliest_session'] == index and at(reported['earliest_at']) == clock,
                'final reentry fence differs')
    return dict(sensitivity=scenario,checked_policy_fills=checked_policies,checked_expiry_resolutions=checked_resolutions,
                cash_lock_events=len(locks),final_settled_cash_cents=resources(prior)[0],
                final_put_collateral_cents=resources(prior)[1])


def analyze(journal_path,journal_sha256,result_path,result_sha256,packet_path,packet_sha256, *, expected_sensitivity='primary'):
    financial = verify(journal_path,journal_sha256,result_path,result_sha256)
    report = dict(schema='AutoWheelRD1AvailabilityArithmeticV1',status='insufficient',checks=None,
        financial_verification=financial,source_calendar_fee_and_strategy_admission_performed=False,
        historical_performance_qualified=False,availability_model=expected_sensitivity)
    if not financial['qualified']:return report
    try:
        def read(path,pin,limit):
            raw = _read_bounded(path,limit,'availability input');_check_hash(raw,pin,'availability input');return raw
        packet = _loads(read(packet_path,packet_sha256,256<<20))
        require(packet['schema'] == 'AutoWheelRD1ReplayPacketV1'
                and packet['protocol_sha256'] == PROTOCOL_SHA256, 'unbound RD1 availability packet')
        result = _loads(read(result_path,result_sha256,MAX_RESULT_BYTES))
        require(expected_sensitivity in SCENARIOS and result.get('sensitivity','primary') == expected_sensitivity,
                'result sensitivity differs from the independently bound case')
        require(at(result['checkpoint_state']['at']) == at(result['last_near_close_at']),
                'account state extends past its declared near-close cutoff')
        records = _parse_journal(read(journal_path,journal_sha256,MAX_JOURNAL_BYTES))
        report['checks'] = availability(records,result['checkpoint_state'],packet['exchange_days'],packet['clearing_days'],
            scenario=expected_sensitivity,dividend_terms=packet['dividends'])
        report['checks']['censored_pending_attempts']=pending_attempts(result,packet,expected_sensitivity)
        report['status'] = 'arithmetic_complete_pending_external_admission'
    except Exception as error:
        report.update(status='failed',checks=None,error=str(error)[:240])
    return report


def pending_attempts(result,packet,scenario):
    """Verify censored intent clocks without assuming a future fill or price."""
    pending=result.get('pending_attempts')
    if pending is None or pending==[]:return dict(count=0,future_fill_or_price_assumed=False)
    require(scenario=='execution_next_session' and type(pending) is list and 0<len(pending)<=13,
            'pending intents require the explicit next-session model')
    sessions,_,_=session_calendar(packet['exchange_days']);last=packet['last_session']
    require(last in sessions and at(result['last_near_close_at'])==sessions[last][1]-timedelta(minutes=1),
            'pending cutoff differs from the final declared near-close')
    following=next((day for day in sessions if day>last),None)
    require(following is not None, 'pending execution lacks its next declared exchange session')
    signal=sessions[last][1]-timedelta(minutes=31);execution=sessions[following][1]-timedelta(minutes=30)
    observations=[row for row in packet['observations'] if at(row['at'])==signal]
    require(len(observations)==1, 'pending signal bucket unknown or repeated')
    state=result['checkpoint_state'];seen=set();indices={day:i for i,day in enumerate(sessions)}
    declared={row['date'] for row in packet['exchange_days']}
    for intent in pending:
        symbol=intent['symbol'];action=intent['action'];ref=intent['ref']
        require(symbol not in seen and ref=='policy-'+last+'-'+symbol and ref not in state['used_refs']
                and action in ('sell_put','sell_call','buy_stock','close_short')
                and date.fromisoformat(intent['contract']['expiry'])>=date.fromisoformat(last)
                and state.get('last_policy_session',{}).get(symbol)!=indices[last]
                and at(intent['signal_at'])==signal and at(intent['at'])==execution
                and intent['source_packet_sha256']==observations[0]['source_packet_sha256']
                and intent['price_cents']==intent['fee_cents']==0,
                'pending intent changed its last signal, next session, source, reference or unfilled status')
        seen.add(symbol)
        if action!='close_short':
            monday=date.fromisoformat(last);monday-=timedelta(days=monday.weekday())
            while str(monday)<last:
                require(str(monday) in declared and str(monday) not in sessions,
                        'pending entry is not the first trading session of its week')
                monday+=timedelta(days=1)
            fence=state['reentry_fences'].get(symbol)
            require(not fence or indices[last]>=fence['earliest_session'] and signal>=at(fence['earliest_at']),
                    'pending entry crosses its intervening-session or notice fence')
            if action=='sell_call':
                p=state['positions'].get(symbol)
                require(p and p['shares']==100 and not p.get('short') and at(p['stock_available_at'])<=signal,
                        'pending call signal uses unavailable shares')
        else:
            p=state['positions'].get(symbol)
            require(p and p.get('short') and p['short']['contract']==intent['contract'],
                    'pending close lost its observed held contract')
    return dict(count=len(pending),last_signal_at=signal.isoformat(),scheduled_execution_at=execution.isoformat(),
        cutoff_at=result['last_near_close_at'],future_fill_or_price_assumed=False,
        future_resolution_or_policy_change_not_pulled_into_observed_path=True)
