"""Collect declared last-reported regular-session stock trade references."""
from datetime import date,timedelta
import fcntl
import json
import os
from pathlib import Path
import sys
import urllib.request

from rd1_capture import capture_batch
from rd1_dispatch import checked
from rd1_storage import canonical,require,root,sha,write
from simple_research_capture import budget_check,logical_symbol,vendor_symbol


def run(spec):
    r=root('rd1-261017-0748');old=root('rd1-261016-2037')
    budget_check(r,spec['observation'],spec['timeout_seconds'])
    with urllib.request.urlopen('http://127.0.0.1:25543/v3/terminal/mdds/status',timeout=8) as response:
        require(response.status==200 and response.read(2048)==b'CONNECTED','Theta terminal disconnected')
    summary=json.loads(checked(r/'long-stock-stage-v3/summary.json',
        '68317a286ed3ba1d785a7526cc504a2aef6a07206d2fdb608c6573534a74474a',1<<20))
    item=next(x for x in summary['years'] if x['year']==spec['year'])
    ref=item['stock_stage'];stock=json.loads(checked(ref['file'],ref['sha256'],4<<20))
    symbols=stock['symbols'];sessions=stock['exchange_sessions']
    require(2021<=spec['year']<=2026 and not set(symbols)&{'QCOM','META','FB'},'reference scope differs')
    covered={};attempted=set()
    for base in (old,r):
        for p in (base/'captures').glob('*/*.intent.json'):
            attempted.add(sha(canonical(json.loads(p.read_bytes())['query'])))
        for p in (base/'captures').glob('*/*.receipt.json'):
            receipt=json.loads(p.read_bytes());q=receipt['query'];params=q['params']
            if q['path']!='/v3/stock/at_time/trade' or params.get('venue')!='utp_cta':continue
            if receipt['status']!='captured' or not receipt['complete'] or receipt['http_status']!=200:continue
            first=date.fromisoformat(params.get('start_date',params.get('date')))
            last=date.fromisoformat(params.get('end_date',params.get('date')))
            days=covered.setdefault((logical_symbol(params),params['time_of_day']),set())
            while first<=last:days.add(first);first+=timedelta(days=1)
    queries=[];blocked=[]
    def admit(symbol,days,clock):
        if not days:return
        q=dict(path='/v3/stock/at_time/trade',params=dict(symbol=vendor_symbol(symbol,days[0]),
            start_date=days[0].strftime('%Y%m%d'),end_date=days[-1].strftime('%Y%m%d'),
            venue='utp_cta',time_of_day=clock,format='json'))
        (blocked if sha(canonical(q)) in attempted else queries).append(q)
    for symbol in symbols:
        for clock in ('16:00:00','13:00:00'):
            group=[];prior=None
            for row in sessions:
                if row['close']!=clock:continue
                day=date.fromisoformat(row['date']);key=(day.year,day.month,vendor_symbol(symbol,day))
                if day in covered.get((symbol,clock),set()):
                    admit(symbol,group,clock);group=[];prior=None;continue
                if group and (key!=prior or clock=='13:00:00'):
                    admit(symbol,group,clock);group=[]
                group.append(day);prior=key
            admit(symbol,group,clock)
    require(1<=spec['limit']<=320 and len({sha(canonical(q)) for q in queries})==len(queries),
        'duplicate or excessive reference queries')
    if not queries:
        print(json.dumps(dict(year=spec['year'],remaining_unattempted=0,blocked_queries=len(blocked),scheduled=0,captured=0)));return
    locks=[]
    try:
        for base in (old,r):
            fd=os.open(base/'capture.lock',os.O_WRONLY|os.O_CREAT|os.O_NOFOLLOW,0o600)
            fcntl.flock(fd,fcntl.LOCK_EX|fcntl.LOCK_NB);locks.append(fd)
        selected=queries[:spec['limit']]
        selection=write(r/(spec['batch_id']+'-selection.json'),dict(stock_stage=ref,selected_queries=selected,
            remaining_unattempted=len(queries)-len(selected),blocked_queries=blocked,
            reference_is_last_reported_trade_not_official_OCC_or_exchange_close=True,
            reuse_is_transport_coverage_not_trade_condition_qualification=True))
        result=capture_batch(dict(spec,queries=selected,concurrency=4),r,
            budget_check=budget_check,maximum_concurrency=4,maximum_queries=320)
        print(json.dumps(dict(year=spec['year'],selection=selection,remaining_unattempted=len(queries)-len(selected),
            blocked_queries=len(blocked),captured=result['captured'],scheduled=result['scheduled'],financial_cases=0)))
    finally:
        for fd in locks:os.close(fd)


if __name__=='__main__':run(json.load(sys.stdin))
