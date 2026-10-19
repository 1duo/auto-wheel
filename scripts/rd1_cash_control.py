"""The frozen zero-transaction cash control; no general wheel admission."""
from rd1_financial_verify import (_check_hash,_loads,_parse_journal,_read_bounded,
    MAX_JOURNAL_BYTES,MAX_RESULT_BYTES,verify)
from rd1_metrics import SEED,cash_exposure,inventory_metrics,series_metrics,session_calendar
from rd1_storage import PROTOCOL_SHA256,canonical,require,sha

CASH='cash_zero_interest'
PACKET_SHA='e68ae9c9d3f2d7658a08f7c8616b5c01537ff52f83696f7410403785acf5422a'
ADMISSION_SHA='fff0a0db5a4b8a3055960a795db18703fac8ac25e455d023b3f667bb0ea5c43e'
CALENDAR_SHA='5e696affb38597241e01033ca86c015fb81bb935544ea9b26ea2914e71a764b7'


def preflight(case,packet,admission,packet_ref,admission_ref,read_source):
    require(case['purpose']=='control' and case['profile_id']==CASH and case['sensitivity']=='primary'
            and case['parent_state_sha256']=='0'*64 and case['model_sha256']==PROTOCOL_SHA256
            and case['start']=='2023-01-03' and case['end']=='2025-12-31'
            and case['input_sha256']==packet_ref['sha256']==PACKET_SHA
            and admission_ref['sha256']==ADMISSION_SHA, 'cash case changed its frozen account or admitted schedule')
    fields={'schema','control','protocol_sha256','admission_status','unqualified_inputs','calendar_source_sha256',
            'first_session','last_session','exchange_days','admission_receipt_sha256'}
    require(set(packet)==fields and packet['schema']=='AutoWheelRD1CashControlPacketV1'
            and packet['control']==CASH and packet['protocol_sha256']==PROTOCOL_SHA256
            and packet['admission_status']=='admitted_cash_control_under_RD1' and packet['unqualified_inputs']==[]
            and packet['admission_receipt_sha256']==ADMISSION_SHA and packet['calendar_source_sha256']==CALENDAR_SHA
            and packet['first_session']==case['start'] and packet['last_session']==case['end'], 'unadmitted cash-only packet')
    body=dict(packet);del body['admission_receipt_sha256']
    require(admission['schema']=='AutoWheelRD1CashInputAdmissionV1'
            and admission['protocol_sha256']==PROTOCOL_SHA256
            and admission['status']=='admitted_cash_control_under_RD1' and admission['unqualified_inputs']==[]
            and admission['scope']=='zero_transactions_zero_interest_cash_control_only'
            and admission['cost_basis']=='no_transactions_no_customer_tariff_required'
            and admission['stock_option_customer_fee_or_wheel_sources_admitted'] is False
            and admission['packet_body_sha256']==sha(canonical(body)), 'cash admission scope or packet body differs')
    sources=admission['source_artifacts'];require(type(sources) is list and 0<len(sources)<=32,'cash source originals missing')
    for source in sources:read_source(source,8<<20)
    require(sha(canonical(packet['exchange_days']))==CALENDAR_SHA, 'cash calendar differs from reviewed daily original')
    sessions,first,last=session_calendar(packet['exchange_days'])
    require(str(first)=='2023-01-01' and str(last)=='2025-12-31' and len(sessions)==752
            and min(sessions)==case['start'] and max(sessions)==case['end'], 'cash schedule omits full forward history')
    return dict(schema='AutoWheelRD1CashControlPreflightV1',control=CASH,calendar_source_sha256=CALENDAR_SHA,
        admission_receipt_sha256=ADMISSION_SHA,sessions=752,customer_tariffs_required=False,
        stock_option_sources_qualified=False,zero_transactions=True,interest_cents=0)


def analyze(journal_path,journal_sha256,result_path,result_sha256,packet_path,packet_sha256):
    financial=verify(journal_path,journal_sha256,result_path,result_sha256)
    report=dict(schema='AutoWheelRD1CashControlVerificationV1',status='insufficient',metrics=None,
        financial_verification=financial,descriptive_control_only=True,wheel_strategy_qualification_performed=False)
    if not financial['qualified']:return report
    try:
        def read(path,pin,limit):
            raw=_read_bounded(path,limit,'cash verification input');_check_hash(raw,pin,'cash verification input');return raw
        require(packet_sha256==PACKET_SHA, 'cash verifier requires its exact source-admitted calendar packet')
        packet=_loads(read(packet_path,packet_sha256,4<<20))
        result=_loads(read(result_path,result_sha256,MAX_RESULT_BYTES))
        rows=_parse_journal(read(journal_path,journal_sha256,MAX_JOURNAL_BYTES))
        require(result.get('control')==CASH and result.get('sensitivity')=='primary' and result['status']=='complete',
                'cash result lacks the separate control/primary label')
        points=[]
        for row in rows:
            e=row['effect'];mark=row['mark']
            require(e['action']=='mark' and e['source_packet_sha256']==CALENDAR_SHA
                    and mark['stock_books']==mark['option_books']=={}
                    and mark['NAV_cents']==row['cash_after_cents']==SEED
                    and row['fees_after_cents']==row['cash_delta_cents']==row['receivable_delta_cents']==0,
                    'cash control contains a transaction, price, fee or changed capital')
            points.append(dict(ref=e['ref'],at=e['at'],nav_cents=mark['NAV_cents']))
        state=result['checkpoint_state']
        require(all(state[name]=={} for name in ('positions','option_sales','reentry_fences','last_policy_session','dividends'))
                and not state['cash_locks'] and state['economic_cash_cents']==state['seed_cents']==SEED
                and state['fees_cents']==state['realized_stock_pnl_cents']==0,
                'cash control retained an unexpected asset, liability, lock or fee')
        metrics=series_metrics(points,packet['exchange_days'],packet['first_session'],packet['last_session'])
        require(metrics['sessions']==result['sessions_marked']==752 and len(rows)==2256
                and metrics['net_profit_cents']==result['net_profit_cents']==0, 'cash complete schedule or fixed-capital invariant differs')
        metrics.update(inventory_metrics(rows));metrics['financial_components']=financial['metrics']
        sessions,_,_=session_calendar(packet['exchange_days'])
        metrics['cash_exposure']=cash_exposure(rows,sessions[packet['first_session']][0].isoformat(),result['last_near_close_at'])
        metrics['control']=CASH;metrics['interest_cents']=0;metrics['economic_transactions']=0
        report.update(status='arithmetic_complete_pending_external_admission',metrics=metrics,
            packet_sha256=packet_sha256,journal_sha256=journal_sha256,result_sha256=result_sha256,
            all_cash_settled_by_frozen_flat_seed_model=True,model_clock_marks_are_not_market_quotes=True)
    except Exception as error:report.update(reason='cash_control_rejected:'+type(error).__name__+':'+str(error)[:240])
    return report
