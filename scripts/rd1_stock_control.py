"""Scoped buy-and-hold admission and independent stock-control arithmetic."""
from datetime import timedelta

from rd1_attempt_verify import diagnostic_identity,entry_valid
from rd1_availability_verify import availability
from rd1_cash_control import CALENDAR_SHA
from rd1_financial_verify import (_check_hash,_loads,_parse_journal,_read_bounded,
    MAX_JOURNAL_BYTES,MAX_RESULT_BYTES,MAX_LINE_BYTES,verify)
from rd1_metrics import SEED,at,cash_exposure,integer,inventory_metrics,series_metrics,session_calendar
from rd1_storage import PROTOCOL_SHA256,canonical,require,sha

STOCK='stock_buy_hold'
STATUS='admitted_stock_control_under_RD1'
FIELDS={'schema','control','protocol_sha256','admission_status','admission_receipt_sha256',
    'unqualified_inputs','calendar_source_sha256','clearing_source_sha256','first_session','last_session',
    'exchange_days','stock_selection_sha256','stock_selection_cutoff','ranked_symbols','clearing_days',
    'observations','initial_purchase_fee','identity_intervals','dividends','unsupported_actions'}


def preflight(case,packet,admission,packet_ref,admission_ref,read_review,read_source):
    require(case['purpose']=='control' and case['profile_id']==STOCK and case['sensitivity']=='primary'
        and case['parent_state_sha256']=='0'*64 and case['model_sha256']==PROTOCOL_SHA256
        and case['start']=='2023-01-03' and case['end']=='2025-12-31'
        and case['input_sha256']==packet_ref['sha256'],'stock control case identity changed')
    require(set(packet)==FIELDS and packet['schema']=='AutoWheelRD1StockControlPacketV1'
        and packet['control']==STOCK and packet['protocol_sha256']==PROTOCOL_SHA256
        and packet['admission_status']==STATUS and packet['unqualified_inputs']==[]
        and packet['admission_receipt_sha256']==admission_ref['sha256']
        and packet['calendar_source_sha256']==CALENDAR_SHA
        and packet['stock_selection_cutoff']=='2022-12-31'
        and packet['first_session']==case['start'] and packet['last_session']==case['end'],
        'unadmitted or unscoped stock-only packet')
    body=dict(packet);del body['admission_receipt_sha256']
    require(admission['schema']=='AutoWheelRD1StockControlAdmissionV1'
        and admission['protocol_sha256']==PROTOCOL_SHA256 and admission['status']==STATUS
        and admission['unqualified_inputs']==[] and admission['scope']=='original_universe_first_three_funded_stock_control_only'
        and admission['cost_basis']=='verified_historical_customer_stock_purchase_fees'
        and admission['option_sources_admitted'] is False
        and admission['packet_body_sha256']==sha(canonical(body)),'stock control admission scope differs')
    selection_ref=admission['stock_selection'];proof_ref=admission['stock_selection_admission']
    selection=_loads(read_review(selection_ref,16<<20));proof=_loads(read_review(proof_ref,4<<20))
    require(packet['stock_selection_sha256']==selection_ref['sha256']
        and selection['protocol_sha256']==PROTOCOL_SHA256 and selection['cutoff']=='2022-12-31'
        and selection['option_returns_used'] is False and selection['selected']==packet['ranked_symbols']
        and len(selection['selected'])<=10 and len(set(selection['selected']))==len(selection['selected'])
        and proof['schema']=='AutoWheelRD1StockSelectionAdmissionV1'
        and proof['status']=='admitted_under_RD1' and proof['protocol_sha256']==PROTOCOL_SHA256
        and proof['cutoff']=='2022-12-31' and proof['selection_sha256']==selection_ref['sha256']
        and proof['unqualified_inputs']==[], 'stock control requires an admitted 2023 formation; diagnostics are insufficient')
    for source_list in (admission['source_artifacts'],proof['source_artifacts']):
        require(type(source_list) is list and 0<len(source_list)<=2000,'stock control source originals absent')
        for source in source_list:read_source(source,512<<20)
    require(sha(canonical(packet['exchange_days']))==CALENDAR_SHA
        and sha(canonical(packet['clearing_days']))==packet['clearing_source_sha256'],'stock calendars differ from reviewed bodies')
    sessions,first,last=session_calendar(packet['exchange_days'])
    require(str(first)=='2023-01-01' and str(last)=='2025-12-31' and len(sessions)==752
        and min(sessions)==case['start'] and max(sessions)==case['end'],'stock control omits matched forward sessions')
    fee=packet['initial_purchase_fee'];integer(fee['fee_cents'],0,10000)
    require(fee['date']==case['start'] and at(fee['known_at'])<=sessions[case['start']][1]-timedelta(minutes=31),
        'initial stock purchase tariff is not causal')
    return dict(schema='AutoWheelRD1StockControlPreflightV1',control=STOCK,sessions=752,
        original_formation=selection_ref,original_formation_admission=proof_ref,
        option_quotes_or_fees_required=False,stock_prices_identity_actions_dividends_and_purchase_fee_required=True,
        annual_reselection=False,later_entry_or_reinvestment=False)


def effects(records,result,packet,diagnostics):
    """Pure source/arithmetic audit; exact money and availability precede use."""
    sessions,_,_=session_calendar(packet['exchange_days']);first=packet['first_session']
    signal=sessions[first][1]-timedelta(minutes=31);execution=signal+timedelta(minutes=1)
    cutoff=at(result['last_near_close_at']);samples={at(x['at']):x for x in packet['observations']}
    require(len(samples)==len(packet['observations']),'duplicate stock sample coordinate')
    fee=packet['initial_purchase_fee'];amount=integer(fee['fee_cents'],0,10000)
    require(fee['date']==first and at(fee['known_at'])<=signal,'initial stock fee was not known')
    require(not any(at(h['from'])<=execution<at(h['through']) for h in next(d for d in packet['exchange_days'] if d['date']==first).get('declared_trading_halts',[])),
        'stock allocation was halted')
    def identity(symbol,clock):
        require(not any(a['symbol']==symbol and at(a['at'])<=clock for a in packet['unsupported_actions']),
            'unsupported held or entering stock action')
        rows=[x for x in packet['identity_intervals'] if x['symbol']==symbol and x['security_kind']=='US_common_stock'
            and at(x['from'])<=clock<=at(x['through']) and at(x['known_at'])<=clock]
        require(len(rows)==1,'unknown or ambiguous common-stock identity/action interval')
    expected=[];chosen=[];cost=0;required_buys=[]
    names=packet['ranked_symbols'];require(len(names)<=10 and len(set(names))==len(names),'ambiguous original stock rank')
    for symbol in names:
        if len(chosen)==3:break
        for clock in (signal,execution):
            identity(symbol,clock);require(symbol in samples[clock]['stock_books'] and entry_valid(samples[clock]['stock_books'][symbol]),
                'unknown/invalid initial stock book; original rank cannot be replaced')
        price=integer(samples[execution]['stock_books'][symbol]['ask_cents'],1,1_000_000_000)
        debit=100*price+amount;funded=debit<=2_666_666 and cost+debit<=min(8_000_000,SEED-1_680_944)
        stage,reason=('modeled_fill','buy_stock') if funded else ('risk_rejection','initial_stock_capacity')
        expected.append(dict(at=execution.isoformat(),symbol=symbol,stage=stage,reason=reason,
            source_packet_sha256=samples[execution]['source_packet_sha256']))
        if funded:chosen.append(symbol);cost+=debit;required_buys.append((symbol,price,amount))
    require([diagnostic_identity(x) for x in diagnostics]==expected,'stock allocation diagnostics differ from fully priced initial ranks')
    counts={}
    for row in expected:
        key=row['stage']+':'+row['reason'];counts[key]=counts.get(key,0)+1
    require(result['diagnostic_counts']==counts and all(type(v) is int for v in result['diagnostic_counts'].values()),
        'stock control diagnostic totals differ')
    buys=[];owned={};entitled={};payments=set();terms={d['id']:d for d in packet['dividends']}
    require(len(terms)==len(packet['dividends']),'duplicate stock dividend identity')
    for record in records:
        e=record['effect'];action=e['action'];clock=at(e['at']);symbol=e.get('symbol')
        if action=='mark':
            obs=samples[clock]
            for name in owned:identity(name,clock)
            require(e['source_packet_sha256']==obs['source_packet_sha256']
                and (record['mark']['option_books'] or {})=={}
                and record['mark']['stock_books']=={s:obs['stock_books'][s] for s in owned},
                'stock NAV book differs from exact held-side source')
        elif action=='buy_stock':
            require(clock==execution and at(e['signal_at'])==signal and e['ref']=='stock-control-'+first+'-'+symbol
                and e['profile']==dict(mode='aggressive',exit='hold') and symbol not in owned
                and e['source_packet_sha256']==samples[execution]['source_packet_sha256']
                and not any((e.get('contract') or {}).values()),'stock control reentered, changed timing or supplied an option')
            buys.append((symbol,e['price_cents'],e['fee_cents']));owned[symbol]=clock
        elif action in ('dividend','pay_dividend'):
            d=terms[e['dividend_id']]
            require(e['symbol']==d['symbol'] and e['price_cents']==d['amount_for_100_shares_cents']
                and e['fee_cents']==0 and e['source_packet_sha256']==d['source_sha256'],
                'stock dividend source/amount differs')
            if action=='dividend':
                require(symbol in owned and owned[symbol]<clock==at(d['ex_at']) and at(d['known_at'])<=clock
                    and at(e['payment_at'])==at(d['pay_at']) and at(e['available_at'])==at(d['available_at'])
                    and e['dividend_id'] not in entitled,'stock dividend entitlement differs')
                entitled[e['dividend_id']]=d
            else:
                require(e['dividend_id'] in entitled and clock==at(d['pay_at']) and e['dividend_id'] not in payments,
                    'stock dividend payment is unknown, mistimed or repeated')
                payments.add(e['dividend_id'])
        else:raise ValueError('stock control contains an option, sale or unsupported transaction')
    require(buys==required_buys and set(result['checkpoint_state']['positions'])==set(chosen),
        'stock control omits an initial funded name or replaces an original holding')
    expected_entitlements={d['id'] for d in packet['dividends'] if d['symbol'] in owned
        and owned[d['symbol']]<at(d['ex_at'])<=cutoff}
    require(set(entitled)==expected_entitlements and payments=={key for key in expected_entitlements if at(terms[key]['pay_at'])<=cutoff},
        'stock control omitted a due dividend entitlement or payment')
    return dict(original_funded_names=chosen,fully_priced_initial_attempts=len(expected),modeled_purchases=len(buys),
        initial_risk_rejections=len(expected)-len(buys),initial_traded_stock_cash_cents=sum(100*p for _,p,_ in buys),
        retained_original_names=True,annual_reselection=False,dividend_entitlements=len(entitled),dividend_payments=len(payments),
        supplied_dividend_event_completeness_checked=True,source_dividend_coverage_qualified=False,
        option_data_or_option_fee_assumptions_used=False)


def analyze(journal_path,journal_sha256,result_path,result_sha256,packet_path,packet_sha256,
            diagnostics_path,diagnostics_sha256):
    financial=verify(journal_path,journal_sha256,result_path,result_sha256)
    report=dict(schema='AutoWheelRD1StockControlVerificationV1',status='insufficient',metrics=None,
        financial_verification=financial,descriptive_control_only=True,wheel_strategy_qualification_performed=False)
    if not financial['qualified']:return report
    try:
        def read(path,pin,limit):
            raw=_read_bounded(path,limit,'stock control input');_check_hash(raw,pin,'stock control input');return raw
        packet=_loads(read(packet_path,packet_sha256,256<<20));result=_loads(read(result_path,result_sha256,MAX_RESULT_BYTES))
        require(set(packet)==FIELDS and packet['schema']=='AutoWheelRD1StockControlPacketV1'
            and packet['protocol_sha256']==PROTOCOL_SHA256 and packet['admission_status']==STATUS
            and packet['control']==result.get('control')==STOCK and result['sensitivity']=='primary'
            and not result.get('pending_attempts') and not result['checkpoint_state']['option_sales'],
            'stock result changed its scoped packet, control or scenario')
        records=_parse_journal(read(journal_path,journal_sha256,MAX_JOURNAL_BYTES))
        raw=read(diagnostics_path,diagnostics_sha256,32<<20);lines=raw.splitlines()
        require((not raw or raw.endswith(b'\n')) and len(lines)<=100000 and all(0<len(x)<=MAX_LINE_BYTES for x in lines),
            'unbounded stock diagnostic log')
        availability_checks=availability(records,result['checkpoint_state'],packet['exchange_days'],packet['clearing_days'],dividend_terms=packet['dividends'])
        source_checks=effects(records,result,packet,[_loads(x) for x in lines])
        points=[dict(ref=r['effect']['ref'],at=r['effect']['at'],nav_cents=r['mark']['NAV_cents']) for r in records if r['effect']['action']=='mark']
        metrics=series_metrics(points,packet['exchange_days'],packet['first_session'],packet['last_session'])
        require(metrics['sessions']==result['sessions_marked']==752 and metrics['net_profit_cents']==financial['metrics']['net_profit_cents'],
            'stock control schedule or profit differs')
        metrics.update(inventory_metrics(records));metrics['financial_components']=financial['metrics']
        sessions,_,_=session_calendar(packet['exchange_days'])
        metrics.update(control=STOCK,initial_allocation=source_checks,interest_cents=0,
            cash_exposure=cash_exposure(records,sessions[packet['first_session']][0].isoformat(),result['last_near_close_at']))
        report.update(status='arithmetic_complete_pending_external_admission',metrics=metrics,
            source_effect_checks=source_checks,availability_checks=availability_checks,
            packet_sha256=packet_sha256,journal_sha256=journal_sha256,result_sha256=result_sha256,
            original_source_admission_performed=False)
    except Exception as error:report.update(reason='stock_control_rejected:'+type(error).__name__+':'+str(error)[:240])
    return report
