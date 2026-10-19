"""Derive frozen terminal stress snapshots from reconciled RD1 cases.

No option repricing or future market data is introduced. Current dated delivery
fees are held fixed for each sample's hypothetical contractual payoff. Actual
future fees, assignments, time paths, broker availability and losses are not
predicted by these scenarios.
"""
import re
from datetime import timedelta

from rd1_availability_verify import availability
from rd1_cash_control import CASH,PACKET_SHA,analyze as cash_analyze
from rd1_effect_verify import sale_fee
from rd1_financial_verify import (_check_hash,_loads,_parse_journal,_read_bounded,
    MAX_JOURNAL_BYTES,MAX_RESULT_BYTES,verify)
from rd1_metrics import NY,SCENARIOS,SEED,at,integer,session_calendar
from rd1_stock_control import STOCK
from rd1_storage import PROTOCOL_SHA256,require
from rd1_terminal import summarize_terminal_samples


def evidence_hash(value):
    require(type(value) is str and re.fullmatch('[0-9a-f]{64}',value),'terminal source evidence hash unknown')
    return value


def snapshots(records,packet,trace,kind):
    """Pure derivation; money/availability verification must precede use."""
    require(kind in ('wheel',CASH,STOCK),'unknown terminal source scope')
    sessions,_,_=session_calendar(packet['exchange_days'])
    expected=['NAV-'+d for d in sessions if packet['first_session']<=d<=packet['last_session']]
    samples={at(row['at']):row for row in packet.get('observations',[])}
    tariffs={row['dated_fees']['date']:row for row in packet.get('tariffs',[])}
    require(len(samples)==len(packet.get('observations',[])) and len(tariffs)==len(packet.get('tariffs',[])),
        'duplicate terminal source coordinate')
    output=[];receivables=0
    for index,record in enumerate(records):
        e=record['effect'];receivables+=integer(record['receivable_delta_cents'],-100_000_000_000)
        if e['action']!='mark' or not e['ref'].startswith('NAV-'):continue
        clock=at(e['at']);day=str(clock.astimezone(NY).date())
        require(day in sessions and e['ref']=='NAV-'+day and clock==sessions[day][1]-timedelta(minutes=1),
            'terminal sample is not the declared near-close')
        if kind==CASH:
            require(record['cash_after_cents']==record['mark']['NAV_cents']==SEED and receivables==0,
                'cash-only terminal sample changes capital or receivables')
            output.append(dict(sample_ref=e['ref'],nav_cents=SEED,economic_cash_cents=SEED,
                settled_cash_cents=SEED,receivables_cents=0,positions=[]));continue
        obs=samples[clock];snap=trace['snapshots'][index];cash=integer(snap['cash'])
        require(e['source_packet_sha256']==obs['source_packet_sha256'] and cash==record['cash_after_cents'],
            'terminal NAV source/cash snapshot differs')
        locked=sum(x['amount'] for x in trace['locks'][:snap['lock_count']] if clock<x['until'])
        settled=integer(cash-locked,0,cash);positions=[]
        for symbol,p in sorted(snap['positions'].items()):
            require(not any(row['symbol']==symbol and at(row['at'])<=clock for row in packet['unsupported_actions']),
                'terminal inventory has an unsupported action')
            rows=packet['standard_terms'] if kind=='wheel' else packet['identity_intervals']
            terms=[row for row in rows if row['symbol']==symbol and at(row['from'])<=clock<=at(row['through'])
                and at(row['known_at'])<=clock]
            require(len(terms)==1,'terminal identity/deliverable interval unknown or ambiguous')
            term=terms[0]
            require(term['shares']==term['multiplier']==100 if kind=='wheel' else term['security_kind']=='US_common_stock',
                'unsupported terminal stock or deliverable quantity')
            source=evidence_hash(term['source_sha256'])
            # A cash-secured put needs a contemporaneous stock bid for this
            # stress, even though that stock is not held in the NAV journal.
            book=obs['stock_books'][symbol]
            spot=integer(book['bid_cents'],1,1_000_000_000);ask=integer(book['ask_cents'],1,1_000_000_000)
            require(spot<=ask,'terminal underlying book is crossed')
            if p['shares']:
                require(record['mark']['stock_books'][symbol]==book,'terminal owned-stock mark differs from its source')
            short=None
            if p['short'] is not None:
                require(kind=='wheel','stock control unexpectedly contains an option')
                c=p['short'];require(c['symbol']==symbol and c['expiry']>=day,'terminal short identity is expired or different')
                quote=obs['option_books'][c['id']]
                require(record['mark']['option_books'][c['id']]==quote,'terminal short liability differs from the observed mark')
                tariff=tariffs[day];fees=tariff['dated_fees']
                require(at(fees['known_at'])<=clock,'terminal delivery tariff was not yet known')
                delivery=integer(fees['physical_exercise_cents'],0,10000)
                if c['kind']=='call':delivery+=sale_fee(tariff['physical_stock_sale'],100,100*integer(c['strike_cents'],1,1_000_000_000))
                short=dict(kind=c['kind'],strike_cents=c['strike_cents'],ask_cents=integer(quote['ask_cents'],1,1_000_000_000),
                    all_in_delivery_fee_cents=integer(delivery,0,10000),delivery_fee_source_sha256=evidence_hash(fees['source_sha256']))
            positions.append(dict(symbol=symbol,shares=p['shares'],stock_bid_cents=spot,
                standard_terms_sha256=source,short=short))
        output.append(dict(sample_ref=e['ref'],nav_cents=integer(record['mark']['NAV_cents']),economic_cash_cents=cash,
            settled_cash_cents=settled,receivables_cents=integer(receivables),positions=positions))
    require(expected and [x['sample_ref'] for x in output]==expected,'missing, repeated or reordered terminal near-close sample')
    return output,expected


def review(records,packet,trace,kind):
    report=dict(schema='AutoWheelRD1TerminalInputArithmeticV1',status='insufficient',metrics=None,
        original_source_admission_performed=False,account_processes_started=0,historical_performance_qualified=False,
        fees_scope='snapshot_dated_all_in_delivery_fees_held_fixed_for_counterfactual_payoff_not_actual_future_customer_fees',
        scope='all_declared_near_close_frozen_inventory_payoffs_not_contemporaneous_option_NAV_or_tail_loss_bound')
    try:
        values,expected=snapshots(records,packet,trace,kind)
        result=summarize_terminal_samples(values,expected)
        report.update(status=result['status'],terminal_series=result,samples=len(values),
            metrics=dict(maximum_terminal_stress_loss=result['maximum_terminal_stress_loss'])
                if result['maximum_terminal_stress_loss'] is not None else None,
            reason=None if result['status']=='arithmetic_complete_pending_external_admission' else 'missing_or_unfunded_terminal_sample')
    except (ValueError,KeyError,TypeError) as error:report.update(reason='terminal_input_rejected:'+str(error)[:240])
    return report


def analyze(journal_path,journal_sha256,result_path,result_sha256,packet_path,packet_sha256,*,expected_sensitivity='primary'):
    financial=verify(journal_path,journal_sha256,result_path,result_sha256)
    report=dict(schema='AutoWheelRD1TerminalVerificationV1',status='insufficient',metrics=None,
        financial_verification=financial,historical_performance_qualified=False,source_admission_performed=False)
    if not financial['qualified']:return report
    try:
        def read(path,pin,limit):
            raw=_read_bounded(path,limit,'terminal input');_check_hash(raw,pin,'terminal input');return raw
        packet=_loads(read(packet_path,packet_sha256,256<<20));result=_loads(read(result_path,result_sha256,MAX_RESULT_BYTES))
        require(packet['protocol_sha256']==PROTOCOL_SHA256 and expected_sensitivity in SCENARIOS
            and result.get('sensitivity','primary')==expected_sensitivity,'terminal packet/scenario differs')
        records=_parse_journal(read(journal_path,journal_sha256,MAX_JOURNAL_BYTES));trace={}
        if result.get('control')==CASH:
            require(expected_sensitivity=='primary' and packet_sha256==PACKET_SHA
                and cash_analyze(journal_path,journal_sha256,result_path,result_sha256,packet_path,packet_sha256)['status']=='arithmetic_complete_pending_external_admission',
                'unverified or changed cash control supplied as terminal inventory')
            kind=CASH
        else:
            kind=STOCK if result.get('control')==STOCK else 'wheel'
            require(packet['schema']==('AutoWheelRD1StockControlPacketV1' if kind==STOCK else 'AutoWheelRD1ReplayPacketV1')
                and packet['admission_status']==('admitted_stock_control_under_RD1' if kind==STOCK else 'admitted_under_RD1')
                and packet['unqualified_inputs']==[],'terminal input lacks the correct stock/wheel source scope')
            require(kind=='wheel' or expected_sensitivity=='primary','stock control scenario differs')
            availability(records,result['checkpoint_state'],packet['exchange_days'],packet['clearing_days'],
                scenario=expected_sensitivity,dividend_terms=packet['dividends'],trace=trace)
        derived=review(records,packet,trace,kind)
        report.update(status=derived['status'],metrics=derived['metrics'],derivation=derived,control_scope=kind,
            packet_sha256=packet_sha256,journal_sha256=journal_sha256,result_sha256=result_sha256,
            independent_money_and_availability_verified=True)
    except Exception as error:report.update(reason='terminal_verification_rejected:'+type(error).__name__+':'+str(error)[:240])
    return report
