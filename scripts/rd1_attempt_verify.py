"""Reconstruct every scheduled RD1 policy decision from supplied inputs.

The independent money/availability interpreter supplies inventory snapshots;
this module reconstructs signal choices, attempts and nonfinancial outcomes.
It does not establish original-source coverage, actual broker fills, compulsory
event completeness or a full research qualification.
"""
from collections import Counter
from datetime import date,timedelta

from rd1_availability_verify import availability
from rd1_effect_verify import contract_choice,opening_fee
from rd1_financial_verify import (_check_hash,_loads,_parse_journal,_read_bounded,
    MAX_JOURNAL_BYTES,MAX_RESULT_BYTES,MAX_LINE_BYTES,verify)
from rd1_metrics import NY,SCENARIOS,at,integer,ratio,session_calendar
from rd1_storage import PROTOCOL_SHA256,canonical,require,sha
from fractions import Fraction

POLICY=('sell_put','sell_call','buy_stock','close_short')


def price(book,buy,scenario):
    bid=integer(book['bid_cents'],-(1<<63),(1<<63)-1)
    ask=integer(book['ask_cents'],-(1<<63),(1<<63)-1)
    value=ask if buy else bid
    if scenario=='extra_half_spread_cost':
        require(0<=bid<=ask and 0<ask<=1_000_000_000,'undefined cost shock')
        value+=(1 if buy else -1)*((ask-bid+1)//2)
    require(value<=1_000_000_000 and (not buy or value>0),'undefined policy price')
    return value


def entry_valid(book):
    bid=integer(book['bid_cents'],-(1<<63),(1<<63)-1)
    ask=integer(book['ask_cents'],-(1<<63),(1<<63)-1)
    return 0<bid<=ask<=1_000_000_000 and 20_000*(ask-bid)<=1500*(ask+bid)


def identity(effect):
    return dict(symbol=effect['symbol'],action=effect['action'],contract=effect['contract'],
        profile=effect['profile'],at=at(effect['at']).isoformat(),
        signal_at=at(effect['signal_at']).isoformat())


def diagnostic_identity(row):
    required={'at','symbol','stage','reason','source_packet_sha256'}
    require(type(row) is dict and required<=set(row)<=required|{'contract'},'diagnostic fields differ')
    value=dict(row);value['at']=at(row['at']).isoformat()
    return value


def reconstruct(records,result,packet,diagnostics,scenario,trace,*,source_model=None):
    """Pure audit using snapshots of an already reconciled financial journal."""
    require(scenario in SCENARIOS,'unknown decision scenario')
    limits=getattr(source_model,'capital_limits',(8_000_000,2_666_666))
    require(limits in ((8_000_000,2_666_666),(20_000_000,20_000_000)),'unknown decision capital policy')
    account_cap,symbol_cap=limits
    sessions,_,_=session_calendar(packet['exchange_days']);days=list(sessions)
    declared={row['date']:row for row in packet['exchange_days']}
    samples={at(row['at']):row for row in packet['observations']}
    tariffs={row['dated_fees']['date']:row for row in packet['tariffs']}
    require(len(samples)==len(packet['observations']) and len(tariffs)==len(packet['tariffs']),
        'duplicate decision source coordinate')
    marks={row['effect']['ref']:i for i,row in enumerate(records) if row['effect']['action']=='mark'}
    fills={};actual_fill_count=0
    for i,row in enumerate(records):
        e=row['effect']
        if e['action'] in POLICY:
            fills.setdefault(at(e['at']),[]).append(i);actual_fill_count+=1
    expected=[];pending=[];counts=Counter();opportunities=Counter();symbols={};fingerprint=[]
    def standard(symbol,clock):
        if any(x['symbol']==symbol and at(x['at'])<=clock for x in packet['unsupported_actions']):return False
        if source_model is not None:return source_model.standard(symbol,clock)
        terms=[x for x in packet['standard_terms'] if x['symbol']==symbol
            and at(x['from'])<=clock<=at(x['through']) and at(x['known_at'])<=clock]
        return len(terms)==1 and terms[0]['shares']==terms[0]['multiplier']==100
    def policy(day):
        rows=[p for p in packet['opening_policies'] if p['from']<=day<=p['through']]
        require(len(rows)==1 and len(rows[0]['symbols'])<=10 and len(set(rows[0]['symbols']))==len(rows[0]['symbols']),
            'ambiguous opening universe')
        return rows[0]
    def tariff(clock):
        if source_model is not None:return source_model.tariff(clock)
        value=tariffs[str(clock.astimezone(NY).date())]
        require(at(value['dated_fees']['known_at'])<=clock,'decision tariff was not known')
        return value
    def log(clock,symbol,stage,reason,source,contract=None):
        row=dict(at=clock.isoformat(),symbol=symbol,stage=stage,reason=reason,source_packet_sha256=source)
        if contract is not None:row['contract']=contract
        expected.append(row);counts[stage+':'+reason]+=1
    def opportunity(symbol,day,reason):
        opportunities[reason]+=1;symbols.setdefault(symbol,Counter())[reason]+=1
        fingerprint.append(dict(day=day,symbol=symbol,disposition=reason))
    def schedule(day,clock):
        if scenario=='execution_next_session':
            index=days.index(day);require(index+1<len(days),'next attempt session unknown')
            return sessions[days[index+1]][1]-timedelta(minutes=30)
        return clock+timedelta(minutes=5 if scenario=='execution_5m' else 1)
    def snapshot(i):
        require(i in trace['snapshots'],'decision state snapshot absent')
        return trace['snapshots'][i]
    checked_fills=0;attempts=0
    path_days=[d for d in days if packet['first_session']<=d<=packet['last_session']]
    require(path_days,'empty decision path')
    for day in path_days:
        signal=sessions[day][1]-timedelta(minutes=31)
        execution=sessions[day][1]-timedelta(minutes=26 if scenario=='execution_5m' else 30)
        before=samples[signal];snap=snapshot(marks['signal-'+day]);ps=snap['positions'];p=policy(day)
        current=p['profile'];universe=p['symbols'];order=sorted(ps)+[s for s in universe if s not in ps]
        monday=date.fromisoformat(day)-timedelta(days=date.fromisoformat(day).weekday())
        require(all(str(monday+timedelta(days=i)) in declared for i in range((date.fromisoformat(day)-monday).days)),
            'weekly decision depends on an unknown preceding day')
        weekly=not any(str(monday)<=d<day for d in sessions)
        for symbol in order:
            pos=ps.get(symbol);profile=pos['profile'] if pos else current
            require(profile['mode'] in ('conservative','balanced','aggressive') and profile['exit'] in ('hold','tp50','tp75','tp90','puttp50','puttp75','puttp90'),
                'unknown decision profile')
            if any(e['symbol']==symbol for e in pending):
                opportunity(symbol,day,'waiting_for_prior_attempt');continue
            fees=tariff(signal)['dated_fees'];contract=None;action=None;rejection=None
            if pos and pos['short']:
                contract=pos['short'];book=before['option_books'][contract['id']]
                ask=integer(book['ask_cents'],1,1_000_000_000)
                if profile['exit']!='hold' and not (profile['exit'].startswith('puttp') and contract['kind']=='call'):ask=price(book,True,scenario)
                active_exit=profile['exit']!='hold' and not (profile['exit'].startswith('puttp') and contract['kind']=='call')
                trigger=active_exit and 100*(pos['net']-100*ask-fees['option_close_cents'])>=int(profile['exit'].removeprefix('put')[2:])*pos['net']
                if not trigger:
                    opportunity(symbol,day,'hold_short_no_close_trigger');continue
                action='close_short'
            elif not weekly:
                opportunity(symbol,day,'outside_weekly_entry_schedule');continue
            else:
                fence=snap['fences'].get(symbol)
                if fence and (days.index(day)<fence[0] or signal<fence[1]):rejection=('signal','reentry_fence')
                elif pos and signal<pos['available']:rejection=('signal','shares_unavailable')
                elif pos is None and source_model is not None and hasattr(source_model,'targeted_reentry_blocked') \
                        and source_model.targeted_reentry_blocked(symbol,signal):rejection=('policy_rejection','stock_loss_reentry_cooldown')
                elif symbol not in before['stock_books'] or symbol not in before['observed_menus']:
                    rejection=('data_abstention','missing_stock_or_menu')
                elif not entry_valid(before['stock_books'][symbol]):rejection=('invalid_book','signal_stock')
                else:
                    kind='call' if pos or profile['mode']=='aggressive' else 'put'
                    mode=source_model.selection_mode(profile['mode'],kind) if source_model is not None else profile['mode']
                    try:contract=contract_choice(before['observed_menus'][symbol],day,symbol,kind,before['stock_books'][symbol],mode)
                    except ValueError as error:
                        require(str(error) in ('filled entry has no observed eligible contract','filled entry has no observed strike at target'),
                            'ambiguous observed contract menu')
                        rejection=('data_abstention','no_observed_contract_at_target')
                    if contract:
                        book=before['option_books'].get(contract['id'])
                        if book is None or not standard(symbol,signal):rejection=('data_abstention','chosen_quote_or_deliverable_unknown')
                        elif not entry_valid(book):rejection=('invalid_book','chosen_signal_option_or_credit')
                        else:
                            credit=price(book,False,scenario)
                            if 100*credit<=opening_fee(tariff(signal),max(credit,0)):
                                rejection=('invalid_book','chosen_signal_option_or_credit')
                            else:action='buy_stock' if pos is None and profile['mode']=='aggressive' else 'sell_'+kind
                    if action=='sell_call' and source_model is not None and hasattr(source_model,'call_allowed') \
                            and not source_model.call_allowed(pos,contract,signal):
                        rejection=('policy_rejection','call_below_stock_acquisition_cost')
            if rejection:
                log(signal,symbol,*rejection,before['source_packet_sha256'],contract)
                opportunity(symbol,day,'signal_'+rejection[0]+':'+rejection[1]);continue
            require(action is not None,'decision has no terminal disposition')
            pending.append(dict(symbol=symbol,action=action,contract=contract,profile=profile,
                signal_at=signal.isoformat(),at=schedule(day,signal).isoformat(),
                source_packet_sha256=before['source_packet_sha256']))
            opportunity(symbol,day,'scheduled_'+action)
        obs=samples[execution];rows=fills.get(execution,[]);cursor=0;remaining=[]
        execution_index=marks['execution-'+day];nav_index=marks['NAV-'+day]
        require(marks['signal-'+day]+1==execution_index and execution_index<nav_index
            and rows==list(range(execution_index+1,nav_index)),
            'unexpected or misplaced financial events between execution and NAV')
        for intent in pending:
            if at(intent['at'])>execution:remaining.append(intent);continue
            require(at(intent['at'])==execution,'overdue attempt')
            attempts+=1;symbol=intent['symbol'];action=intent['action'];c=intent['contract']
            snap=snapshot(rows[cursor] if cursor<len(rows) else marks['NAV-'+day]);ps=snap['positions'];pos=ps.get(symbol)
            reason=None;stage='unfilled'
            if scenario=='execution_next_session' and action=='close_short' and (not pos or pos['short']!=c):
                reason='contract_resolved_before_delayed_attempt'
            elif scenario=='execution_next_session' and action in ('sell_put','buy_stock') and (symbol not in policy(day)['symbols'] or intent['profile']!=policy(day)['profile']):
                reason='opening_policy_changed_before_delayed_attempt'
            elif any(at(h['from'])<=execution<at(h['through']) for h in declared[day].get('declared_trading_halts',[])):
                reason='declared_trading_halt'
            else:
                t=tariff(execution);book=(obs['stock_books'].get(symbol) if action=='buy_stock' else obs['option_books'].get(c['id']))
                if book is None:reason='missing_execution_book'
                elif action=='close_short' and not (0<book['ask_cents']<=1_000_000_000):reason='invalid_close_ask'
                elif action!='close_short' and (symbol not in obs['stock_books'] or not entry_valid(obs['stock_books'][symbol]) or not entry_valid(book)):
                    reason='invalid_execution_entry_book'
                elif action!='close_short' and not standard(symbol,execution):reason='execution_deliverable_unknown'
                else:
                    try:value=price(book,action in ('buy_stock','close_short'),scenario)
                    except ValueError:reason='undefined_execution_cost_shock'
                    if reason is None:
                        fee=(t['stock_buy_fee_cents'] if action=='buy_stock' else
                            t['dated_fees']['option_close_cents'] if action=='close_short' else opening_fee(t,max(value,0)))
                        if action in ('sell_put','sell_call') and 100*value<=fee:reason='nonpositive_execution_credit'
                        elif action=='sell_call' and source_model is not None and hasattr(source_model,'call_allowed') \
                                and not source_model.call_allowed(pos,c,execution):reason='call_below_stock_acquisition_cost'
                        elif source_model is not None and hasattr(source_model,'targeted_capacity_blocked') \
                                and source_model.targeted_capacity_blocked(ps,symbol):
                            stage='risk_rejection';reason='targeted_two_symbol_limit'
                        else:
                            settled=snap['cash']-sum(x['amount'] for x in trace['locks'][:snap['lock_count']] if execution<x['until'])
                            collateral=sum(100*x['short']['strike_cents'] for x in ps.values() if x['short'] and x['short']['kind']=='put')
                            exposure=sum(x['cost'] if x['shares'] else 100*x['short']['strike_cents'] for x in ps.values())
                            debit=100*value+fee if action in ('buy_stock','close_short') else fee
                            addition=100*c['strike_cents'] if action=='sell_put' else debit if action=='buy_stock' else 0
                            if action=='close_short':funded=settled-collateral+(100*c['strike_cents'] if c['kind']=='put' else 0)>=debit
                            else:funded=(settled-debit-collateral-(addition if action=='sell_put' else 0)>=1_680_944
                                and exposure+addition<=account_cap and (pos['cost'] if pos else 0)+addition<=symbol_cap
                                and (pos is not None or len(ps)<3))
                            if not funded:stage='risk_rejection';reason='cash_reserve_exposure_or_slots'
                            elif source_model is not None and hasattr(source_model,'targeted_tax_funded') \
                                    and not source_model.targeted_tax_funded(settled,collateral,debit,addition,action,c,
                                        rows[cursor] if cursor<len(rows) else marks['NAV-'+day]):
                                stage='risk_rejection';reason='targeted_highest_ST_tax_reserve'
                            elif action=='sell_put' and source_model is not None and hasattr(source_model,'entry_risk_allowed') \
                                    and not source_model.entry_risk_allowed(snap,obs,c,value,fee,execution):
                                stage='risk_rejection';reason='entry_minus40_risk_budget'
                            else:
                                require(cursor<len(rows),'eligible policy fill omitted from financial journal')
                                e=records[rows[cursor]]['effect']
                                require(identity(e)==identity(intent) and e['ref']=='policy-'+str(at(intent['signal_at']).astimezone(NY).date())+'-'+symbol
                                    and e['source_packet_sha256']==obs['source_packet_sha256'] and e['price_cents']==value and e['fee_cents']==fee,
                                    'policy fill differs from reconstructed attempt or its order')
                                cursor+=1;checked_fills+=1;stage='modeled_fill';reason=action
            log(execution,symbol,stage,reason,obs['source_packet_sha256'],c)
        require(cursor==len(rows),'unexpected financial policy fill')
        pending=remaining
    actual=[diagnostic_identity(x) for x in diagnostics]
    require(actual==expected,'durable diagnostics differ from reconstructed ordered decisions')
    reported=result['diagnostic_counts']
    require(type(reported) is dict and all(type(v) is int and v>0 for v in reported.values()) and reported==dict(counts),
        'result diagnostic counts differ from durable diagnostics')
    require([identity(e) for e in (result.get('pending_attempts') or [])]==[identity(e) for e in pending]
        and all(e['source_packet_sha256']==p['source_packet_sha256'] for e,p in zip(result.get('pending_attempts') or [],pending,strict=True)),
        'censored queue differs from reconstructed intents')
    require(checked_fills==actual_fill_count,'unconsumed policy fills')
    return dict(scheduled_symbol_session_decisions=sum(opportunities.values()),decision_counts=dict(sorted(opportunities.items())),
        decisions_by_symbol={s:dict(sorted(v.items())) for s,v in sorted(symbols.items())},
        ordered_decision_dispositions_sha256=sha(canonical(fingerprint)),diagnostic_counts=dict(sorted(counts.items())),
        execution_attempts=attempts,modeled_fills=checked_fills,censored_future_attempts=len(pending),
        modeled_fill_rate=ratio(Fraction(checked_fills,attempts)) if attempts else None,
        fill_rate_unavailable_reason=None if attempts else 'no_execution_attempts',
        fill_rate_denominator='all_reconstructed_execution_attempts_including_risk_rejections_and_unfilled_attempts_excluding_censored_plans',
        every_scheduled_policy_decision_reconstructed=True,original_source_coverage_qualified=False,
        compulsory_event_completeness_verified=False,actual_broker_fill_rate_claim=False)


def analyze(journal_path,journal_sha256,result_path,result_sha256,packet_path,packet_sha256,
            diagnostics_path,diagnostics_sha256,*,expected_sensitivity='primary'):
    report=dict(schema='AutoWheelRD1DecisionArithmeticV1',status='insufficient',checks=None,
        historical_performance_qualified=False,source_admission_performed=False)
    if not verify(journal_path,journal_sha256,result_path,result_sha256)['qualified']:return report
    try:
        def read(path,pin,limit):
            raw=_read_bounded(path,limit,'decision input');_check_hash(raw,pin,'decision input');return raw
        packet=_loads(read(packet_path,packet_sha256,256<<20));result=_loads(read(result_path,result_sha256,MAX_RESULT_BYTES))
        require(packet['schema']=='AutoWheelRD1ReplayPacketV1' and packet['protocol_sha256']==PROTOCOL_SHA256
            and result.get('sensitivity','primary')==expected_sensitivity,'unbound decision packet/scenario')
        records=_parse_journal(read(journal_path,journal_sha256,MAX_JOURNAL_BYTES))
        raw=read(diagnostics_path,diagnostics_sha256,32<<20)
        require(not raw or raw.endswith(b'\n'),'unterminated diagnostic record')
        lines=raw.splitlines();require(len(lines)<=100000 and all(0<len(x)<=MAX_LINE_BYTES for x in lines),'unbounded diagnostic lines')
        diagnostics=[_loads(x) for x in lines];trace={}
        availability(records,result['checkpoint_state'],packet['exchange_days'],packet['clearing_days'],
            scenario=expected_sensitivity,dividend_terms=packet['dividends'],trace=trace)
        report['checks']=reconstruct(records,result,packet,diagnostics,expected_sensitivity,trace)
        report.update(status='arithmetic_complete_pending_external_admission',diagnostics_sha256=diagnostics_sha256)
    except Exception as error:report.update(status='failed',checks=None,error=str(error)[:240])
    return report
