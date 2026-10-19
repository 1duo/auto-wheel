"""Verify cumulative forward/sensitivity cases without importing account state.

Input-prefix checks run before a charged extension. Journal-prefix checks run
after it. Both cases must independently pass money, availability and sampled
schedule arithmetic; source and fitted-policy admission remain external.
"""
from datetime import date, datetime, timedelta

from rd1_availability_verify import analyze as availability_analyze
from rd1_financial_verify import (_check_hash, _loads, _parse_journal,
    _read_bounded, MAX_JOURNAL_BYTES, MAX_RESULT_BYTES, verify)
from rd1_metrics import NY, SCENARIOS, analyze as metrics_analyze, at, session_calendar
from rd1_storage import PROTOCOL_SHA256, canonical, require, sha


def input_prefix(old, new):
    fields={'schema','protocol_sha256','admission_status','admission_receipt_sha256','unqualified_inputs',
        'calendar_source_sha256','clearing_source_sha256','first_session','last_session','exchange_days',
        'clearing_days','observations','opening_policies','tariffs','standard_terms','dividends',
        'regular_session_references','unsupported_actions'}
    require(set(old)==set(new)==fields, 'carry packet fields differ')
    require(old['schema']==new['schema']=='AutoWheelRD1ReplayPacketV1'
            and old['protocol_sha256']==new['protocol_sha256']==PROTOCOL_SHA256
            and old['admission_status']==new['admission_status']=='admitted_under_RD1'
            and old['unqualified_inputs']==new['unqualified_inputs']==[], 'unadmitted carry packet')
    old_end,new_end=old['last_session'],new['last_session']
    require(old['first_session']==new['first_session'] and old['first_session'].startswith('2023-')
            and old_end[:4] in ('2023','2024') and int(new_end[:4])==int(old_end[:4])+1,
            'forward extension must retain its 2023 origin and add the next declared year')
    for packet in (old,new):
        sessions,first,last=session_calendar(packet['exchange_days'])
        year=packet['last_session'][:4]
        require(first<=date(2023,1,1) and last>=date(int(year),12,31)
                and packet['first_session']==min(d for d in sessions if d.startswith('2023-'))
                and packet['last_session']==max(d for d in sessions if d.startswith(year+'-')),
                'forward case omits a declared first or final annual session')
    sessions,_,_=session_calendar(old['exchange_days'])
    boundary=sessions[old_end][1]-timedelta(minutes=1)
    for key in ('exchange_days','clearing_days'):
        before,after=old[key],new[key]
        require(isinstance(before,list) and before and isinstance(after,list)
                and len(after)>=len(before) and canonical(before)==canonical(after[:len(before)]),
                'previous complete calendar envelope changed: '+key)
    def preserve(key, identity, effective):
        before={identity(row):row for row in old[key]};after={identity(row):row for row in new[key]}
        require(len(before)==len(old[key]) and len(after)==len(new[key]), 'duplicate carry facts: '+key)
        require(all(k in after and canonical(value)==canonical(after[k]) for k,value in before.items()),
                'previous admitted fact removed or changed: '+key)
        require({k for k,v in after.items() if effective(v)<=boundary}
                =={k for k,v in before.items() if effective(v)<=boundary},
                'earlier effective fact appeared only in extension: '+key)
    preserve('observations',lambda x:at(x['at']),lambda x:at(x['at']))
    day_at=lambda value:datetime.fromisoformat(value).replace(tzinfo=NY)
    preserve('tariffs',lambda x:x['dated_fees']['date'],lambda x:day_at(x['dated_fees']['date']))
    preserve('standard_terms',lambda x:(x['symbol'],at(x['from']),at(x['through'])),lambda x:at(x['from']))
    preserve('dividends',lambda x:x['id'],lambda x:at(x['ex_at']))
    preserve('regular_session_references',lambda x:(x['symbol'],x['day']),lambda x:at(x['at']))
    preserve('unsupported_actions',lambda x:(x['symbol'],at(x['at'])),lambda x:at(x['at']))
    preserve('opening_policies',lambda x:x['from'],lambda x:day_at(x['from']))
    return dict(prior_last_session=old_end,extended_last_session=new_end,boundary_at=boundary.isoformat(),
                original_first_session=old['first_session'],seed_reinitialized_between_years=False,
                prior_full_calendar_envelopes_preserved=True,prior_input_facts_preserved=True)


def journal_prefix(old_raw,new_raw,boundary,old_last):
    require(old_raw.endswith(b'\n') and new_raw.startswith(old_raw) and len(new_raw)>len(old_raw),
            'extended journal does not reproduce the exact prior bytes')
    old_rows=_parse_journal(old_raw);new_rows=_parse_journal(new_raw)
    require(len(new_rows)>len(old_rows), 'extension has no additional journal events')
    final=old_rows[-1]['effect']
    require(final['action']=='mark' and final['ref']=='NAV-'+old_last
            and at(final['at'])==at(boundary), 'prior journal ends away from its final near-close mark')
    require(at(new_rows[len(old_rows)]['effect']['at'])>at(boundary), 'extension inserts an event before its boundary')
    return dict(prior_journal_records=len(old_rows),extended_journal_records=len(new_rows),
                exact_prefix_sha256=sha(old_raw),exact_prefix_bytes=len(old_raw))


def review(previous, extended):
    """Hash-pinned arithmetic review; declarations cannot admit their own inputs.

    Each mapping contains packet/journal/result references and a charged case.
    The dispatcher must independently bind their ledger and source admissions.
    """
    report=dict(schema='AutoWheelRD1CarryArithmeticV1',status='insufficient',checks=None,
        source_and_fit_admission_performed=False,historical_performance_qualified=False,
        checkpoint_imported=False,method='cumulative_2023_seed_exact_input_and_journal_prefix')
    try:
        def read(ref,limit):
            raw=_read_bounded(ref['file'],limit,'carry input');_check_hash(raw,ref['sha256'],'carry input');return raw
        before,after=previous['case'],extended['case']
        require(before['purpose']==after['purpose'] and before['purpose'] in ('forward','sensitivity')
                and before['sensitivity']==after['sensitivity'] and before['sensitivity'] in SCENARIOS
                and (before['sensitivity']=='primary') == (before['purpose']=='forward')
                and all(before[k]==after[k] for k in ('profile_id','start','implementation_sha256','model_sha256')),
                'forward account or implementation changed')
        packets=[_loads(read(x['packet'],256<<20)) for x in (previous,extended)]
        for item,packet in zip((previous,extended),packets,strict=True):
            require(item['packet']['sha256']==item['case']['input_sha256']
                    and packet['first_session']==item['case']['start'] and packet['last_session']==item['case']['end'],
                    'charged carry packet differs')
            args=(item['journal']['file'],item['journal']['sha256'],item['result']['file'],item['result']['sha256'])
            require(verify(*args)['qualified'], 'carry account lacks independent money verification')
            more=(*args,item['packet']['file'],item['packet']['sha256'])
            require(availability_analyze(*more,expected_sensitivity=before['sensitivity'])['status']=='arithmetic_complete_pending_external_admission'
                    and metrics_analyze(*more,expected_sensitivity=before['sensitivity'])['status']=='arithmetic_complete_pending_external_admission',
                    'carry account lacks availability or complete sampled schedule verification')
        checks=input_prefix(*packets)
        old_result=_loads(read(previous['result'],MAX_RESULT_BYTES))
        require(after['parent_state_sha256']==sha(canonical(old_result['checkpoint_state'])),
                'extension is not bound to the prior complete account state')
        checks.update(journal_prefix(read(previous['journal'],MAX_JOURNAL_BYTES),
            read(extended['journal'],MAX_JOURNAL_BYTES),checks['boundary_at'],packets[0]['last_session']))
        report.update(status='arithmetic_complete_pending_external_admission',checks=checks,
                      previous=previous,extended=extended)
    except Exception as error:report.update(status='failed',error=str(error)[:240],checks=None)
    return report
