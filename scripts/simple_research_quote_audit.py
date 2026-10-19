"""Audit exact selected-series samples; missing liabilities stay unknown."""
from collections import Counter
from datetime import date,datetime,timedelta
from decimal import Decimal
import json
from pathlib import Path
import resource
import sys
import time

from rd1_dispatch import checked
from rd1_inputs import cents,contract,option_mark,receipt_rows
from rd1_storage import require,root,sha,write
from simple_research_capture import budget_check,logical_symbol


def run(spec):
    resource.setrlimit(resource.RLIMIT_AS,(4<<30,4<<30));began=time.monotonic()
    r=root('rd1-261017-0748');old=root('rd1-261016-2037');budget_check(r,spec['observation'],600)
    ref=spec['contract_plan'];plan=json.loads(checked(ref['file'],ref['sha256'],1<<20))
    require(plan['schema']=='AutoWheelSimpleContractPlanV1' and plan['year']==spec['year'],'quote-audit plan differs')
    source=next(v for v in plan['artifacts'] if v['file'].endswith('/required-contracts.json'))
    required=json.loads(checked(source['file'],source['sha256'],4<<20));by_id={x['contract']['id']:x for x in required}
    require(len(by_id)==plan['distinct_required_contracts'],'quote-audit contract denominator differs')
    summary=json.loads(checked(r/'long-stock-stage-v3/summary.json',
        '68317a286ed3ba1d785a7526cc504a2aef6a07206d2fdb608c6573534a74474a',1<<20))
    sessions={}
    for item in summary['years']:
        source=item['stock_stage'];stock=json.loads(checked(source['file'],source['sha256'],4<<20))
        sessions.update({d['date']:d['close'] for d in stock['exchange_sessions']})
    expected=set()
    for cid,item in by_id.items():
        for day,close in sessions.items():
            if not item['first_choice_day']<=day<=item['through']:continue
            end=datetime.fromisoformat(day+'T'+close)
            for minutes in (31,30,26,1):expected.add(cid+'|'+(end-timedelta(minutes=minutes)).isoformat(timespec='milliseconds'))
    index={};sources=[];invalid_sources=[];conditions=Counter();invalid=Counter();crossed=0;source_rows=0
    for base in (old,r):
        for path in sorted((base/'captures').glob('*/*.receipt.json')):
            receipt=json.loads(path.read_bytes());q=receipt['query'];p=q['params']
            if q['path']!='/v3/option/history/quote' or p.get('expiration')=='*' or p.get('strike') in (None,'','*'):continue
            if p.get('interval')!='1m' or not all(isinstance(p.get(k),str) for k in ('start_time','end_time')):continue
            symbol=logical_symbol(p);expiry=str(date.fromisoformat(p['expiration']));k=cents(Decimal(p['strike']))
            cid=f"{symbol:<6}{date.fromisoformat(expiry):%y%m%d}{p['right'][0].upper()}{k*10:08d}"
            if cid not in by_id:continue
            raw_path=path.with_name(path.name.replace('.receipt.json','.raw'))
            source=dict(file=str(raw_path),sha256=receipt['sha256'],bytes=receipt['bytes'])
            if receipt['status']!='captured':invalid_sources.append(dict(source=source,query=q,reason='transport_incomplete'));continue
            try:_,groups=receipt_rows(path)
            except ValueError as error:
                invalid_sources.append(dict(source=source,query=q,reason=type(error).__name__+':'+str(error)[:240]));continue
            require(len(groups)==1,'exact-contract quote source has ambiguous identities: '+str(path))
            group=groups[0];raw=group['contract'];c=contract(dict(raw,symbol=symbol))
            require(c==by_id[cid]['contract'] and raw['symbol']==p['symbol'],'query and quote identity differ: '+str(path))
            first=date.fromisoformat(p.get('start_date',p.get('date')));last=date.fromisoformat(p.get('end_date',p.get('date')))
            sources.append(source)
            for row in group['data']:
                source_rows+=1;stamp=datetime.fromisoformat(row['timestamp'])
                require(stamp.tzinfo is None and first<=stamp.date()<=last
                    and p['start_time']<=stamp.strftime('%H:%M:%S')<=p['end_time'],'quote is outside its requested range')
                key=cid+'|'+row['timestamp']
                if key not in expected:continue
                for side in ('bid','ask'):conditions[str(row.get(side+'_condition','not_supplied'))]+=1
                try:
                    b=option_mark(row,stamp);value={k:b[k] for k in ('bid_cents','ask_cents','crossed','entry_book_valid')}
                    crossed+=b['crossed']
                except ValueError as error:
                    reason=str(error);invalid[reason]+=1;value=dict(invalid_reason=reason)
                if key in index:
                    require({k:v for k,v in index[key].items() if k!='raw_sha256'}==value,
                        'conflicting original option sample at '+key)
                else:index[key]=dict(value,raw_sha256=receipt['sha256'])
            require(time.monotonic()-began<540,'quote audit exceeded its bounded runtime')
    missing=sorted(expected-index.keys());out=r/f"quote-audit-{spec['year']}-v1";out.mkdir(mode=0o700)
    refs=[write(out/'quote-index.json',index),write(out/'missing-potential-samples.json',missing),
        write(out/'invalid-source-responses.json',invalid_sources),write(out/'consumed-originals.json',sources)]
    result=dict(schema='AutoWheelSimpleQuoteAuditV1',year=spec['year'],contract_plan=ref,
        distinct_required_contracts=len(by_id),expected_sampled_books=len(expected),observed_sampled_books=len(index),
        missing_potential_samples=len(missing),invalid_sampled_books=dict(invalid),invalid_source_responses=len(invalid_sources),
        crossed_observations=crossed,source_quote_rows=source_rows,complete_originals=len(sources),quote_condition_counts=dict(conditions),
        quote_condition_reference='https://thetadata.net/docs/Articles/Errors-Exchanges-Conditions/Quote-Conditions.html',
        all_potential_held_marks_valid=not missing and not invalid and not invalid_sources,
        actual_held_paths_or_strategy_performance_verified=False,financial_cases=0,artifacts=refs,
        elapsed_seconds=time.monotonic()-began,peak_RSS_KiB=resource.getrusage(resource.RUSAGE_SELF).ru_maxrss)
    print(json.dumps(dict(summary=write(out/'summary.json',result),report=result)),flush=True)


if __name__=='__main__':run(json.load(sys.stdin))
