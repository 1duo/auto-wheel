"""Collect chosen-series holding windows, reusing prior capture intervals."""
from datetime import date,timedelta
from decimal import Decimal
import fcntl
import json
import os
from pathlib import Path
import sys
import urllib.request

from rd1_capture import capture_batch
from rd1_dispatch import checked
from rd1_inputs import cents
from rd1_storage import canonical,require,root,sha,write
from simple_research_capture import budget_check,logical_symbol,vendor_symbol


def run(spec):
    r=root('rd1-261017-0748');old=root('rd1-261016-2037');budget_check(r,spec['observation'],spec['timeout_seconds'])
    with urllib.request.urlopen('http://127.0.0.1:25543/v3/terminal/mdds/status',timeout=8) as response:
        require(response.status==200 and response.read(2048)==b'CONNECTED','Theta terminal is not connected')
    ref=spec['contract_plan'];plan_path=Path(ref['file'])
    require(plan_path==r/f"contract-plan-{spec['year']}-v1/summary.json",'contract-plan path differs')
    plan=json.loads(checked(plan_path,ref['sha256'],1<<20))
    require(plan['schema']=='AutoWheelSimpleContractPlanV1' and plan['year']==spec['year']
        and plan['option_side_price_or_future_return_optimization'] is False,'contract-plan scope differs')
    required_ref=next(v for v in plan['artifacts'] if v['file'].endswith('/required-contracts.json'))
    contracts=json.loads(checked(required_ref['file'],required_ref['sha256'],4<<20))
    require(len(contracts)==plan['distinct_required_contracts'],'contract denominator differs')
    summary=json.loads(checked(r/'long-stock-stage-v3/summary.json',
        '68317a286ed3ba1d785a7526cc504a2aef6a07206d2fdb608c6573534a74474a',1<<20))
    sessions=[]
    for year in summary['years']:
        source=year['stock_stage'];data=json.loads(checked(source['file'],source['sha256'],4<<20))
        sessions.extend(data['exchange_sessions'])
    coverage={};attempted=set()
    for base in (old,r):
        for path in (base/'captures').glob('*/*.intent.json'):
            attempted.add(sha(canonical(json.loads(path.read_bytes())['query'])))
        for path in (base/'captures').glob('*/*.receipt.json'):
            receipt=json.loads(path.read_bytes());q=receipt['query'];p=q['params']
            if q['path']!='/v3/option/history/quote' or p.get('expiration')=='*' or 'strike' not in p:continue
            window=p.get('start_time'),p.get('end_time')
            if p.get('interval')!='1m' or window not in (('15:29:00','15:59:00'),('12:29:00','12:59:00')):continue
            if receipt['status']!='captured' or not receipt['complete'] or receipt['http_status']!=200:continue
            key=(logical_symbol(p),str(date.fromisoformat(p['expiration'])),p['right'].lower(),cents(Decimal(p['strike'])),window)
            first=date.fromisoformat(p.get('start_date',p.get('date')));last=date.fromisoformat(p.get('end_date',p.get('date')))
            days=coverage.setdefault(key,set())
            while first<=last:days.add(first);first+=timedelta(days=1)
    queries=[];blocked=[]
    def admit(c,days,window):
        if not days:return
        q=dict(path='/v3/option/history/quote',params=dict(symbol=vendor_symbol(c['symbol'],days[0]),expiration=c['expiry'].replace('-',''),
            strike=f"{c['strike_cents']//100}.{c['strike_cents']%100:02d}",right=c['kind'],start_date=days[0].strftime('%Y%m%d'),
            end_date=days[-1].strftime('%Y%m%d'),interval='1m',start_time=window[0],end_time=window[1],format='json'))
        if sha(canonical(q)) in attempted:blocked.append(q)
        else:queries.append(q)
    for item in contracts:
        c=item['contract'];require(c['symbol'] not in ('QCOM','META','FB') and c['symbol'] in plan['source_symbols']
            and item['first_choice_day'][:4]==str(spec['year']) and item['through']<=min(c['expiry'],'2026-10-09'),
            'contract holding window differs')
        active=[v for v in sessions if item['first_choice_day']<=v['date']<=item['through']]
        for close,window in (('16:00:00',('15:29:00','15:59:00')),('13:00:00',('12:29:00','12:59:00'))):
            covered=coverage.get((c['symbol'],c['expiry'],c['kind'],c['strike_cents'],window),set());group=[];prior_key=None
            for row in active:
                if row['close']!=close:continue
                day=date.fromisoformat(row['date']);key=(day.year,day.month,vendor_symbol(c['symbol'],day))
                if day in covered:
                    admit(c,group,window);group=[];prior_key=None;continue
                if group and (key!=prior_key or close=='13:00:00'):
                    admit(c,group,window);group=[]
                group.append(day);prior_key=key
            admit(c,group,window)
    require(1<=spec['limit']<=320 and len({sha(canonical(q)) for q in queries})==len(queries),'duplicate or excessive mark queries')
    if not queries:
        print(json.dumps(dict(year=spec['year'],remaining_unattempted=0,blocked_queries=len(blocked),scheduled=0,captured=0)));return
    selected=queries[:spec['limit']];locks=[]
    try:
        for directory in (old,r):
            fd=os.open(directory/'capture.lock',os.O_WRONLY|os.O_CREAT|os.O_NOFOLLOW,0o600)
            fcntl.flock(fd,fcntl.LOCK_EX|fcntl.LOCK_NB);locks.append(fd)
        selection=write(r/(spec['batch_id']+'-selection.json'),dict(contract_plan=ref,selected_queries=selected,
            remaining_unattempted=len(queries)-len(selected),blocked_queries=blocked,
            reuse_is_transport_interval_coverage_not_quote_validation=True,source_or_strategy_admission_performed=False))
        result=capture_batch(dict(spec,queries=selected,concurrency=4),r,budget_check=budget_check,maximum_concurrency=4,maximum_queries=320)
        print(json.dumps(dict(year=spec['year'],selection=selection,remaining_unattempted=len(queries)-len(selected),
            blocked_queries=len(blocked),batch_complete=result['captured']==result['scheduled'],
            captured=result['captured'],scheduled=result['scheduled'],source_or_strategy_admission_performed=False)),flush=True)
    finally:
        for fd in locks:os.close(fd)


if __name__=='__main__':run(json.load(sys.stdin))
