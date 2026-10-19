"""Bounded stock-history collection for the frozen, excluded-symbol lists."""
from datetime import date,datetime,timedelta,timezone
import fcntl
import json
import os
import sys
import urllib.request

from rd1_capture import capture_batch
from rd1_dispatch import checked
from rd1_inputs import session_days
from rd1_storage import PARENT,canonical,require,root,sha,write


RENAME=date(2025,1,21)


def vendor_symbol(symbol,day):
    return 'XYZ' if symbol=='SQ' and day>=RENAME else symbol


def logical_symbol(params):
    symbol=params.get('symbol')
    if symbol=='XYZ':
        require(date.fromisoformat(str(params.get('start_date',params.get('date'))))>=RENAME,
            'Block source alias predates reviewed rename')
        return 'SQ'
    return symbol


def source_symbols(summary,year):
    return list(dict.fromkeys(symbol for cutoff,symbols in sorted(summary['annual_lists'].items())
        if cutoff<str(year)+'0101' for symbol in symbols))


def budget_check(run,observation,timeout):
    goal=observation['goal']
    age=(datetime.now(timezone.utc)-datetime.fromisoformat(observation['recorded_at'])).total_seconds()
    require(goal['status']=='active' and goal['createdAt']==1792248519 and 0<=age<=900
        and 0<timeout<=900 and 24429+goal['timeUsedSeconds']+age+timeout<72000,
        'source collection budget or observation expired')


def run(spec):
    r=root('rd1-261017-0748');old=root('rd1-261016-2037')
    budget_check(r,spec['observation'],spec['timeout_seconds'])
    with urllib.request.urlopen('http://127.0.0.1:25543/v3/terminal/mdds/status',timeout=8) as response:
        require(response.status==200 and response.read(2048)==b'CONNECTED','Theta terminal is not connected')
    recovery=set()
    if spec.get('reviewed_failed_batch'):
        require(spec['reviewed_failed_batch']=='simple-stock-2021-b1-v1','unreviewed source recovery')
        failed=r/'captures'/spec['reviewed_failed_batch']
        receipts=list(failed.glob('*.receipt.json'))
        require(len(receipts)==40,'reviewed empty-transport batch differs')
        for path in receipts:
            receipt=json.loads(path.read_bytes())
            require(receipt['status']=='transport_error' and receipt['bytes']==0 and receipt['http_status'] is None
                and receipt['error_type']=='URLError' and receipt['complete'] is False
                and receipt['sha256']==sha(b''),'recovery contains market data or a different failure')
            checked(path.with_name(path.name.replace('.receipt.json','.raw')),sha(b''),1,allow_empty=True)
            recovery.add(sha(canonical(receipt['query'])))
    summary=json.loads(checked(r/'universe-summary.json',
        'b59a2d7684ca5c0e42d9ed7f1fc3d4ca75b8d18c10b3ed38683454b862f730e6',1<<20))
    year=spec['year'];require(type(year) is int and 2021<=year<=2026,'source year')
    calendar_path=PARENT/'available-history-20261013-calendar-expirations-v1'/f'{year-2020:03}.receipt.json'
    calendar_receipt=json.loads(calendar_path.read_bytes())
    require(calendar_receipt['request']==dict(path='/v3/calendar/year_holidays',params=dict(year=year,format='json'))
        and calendar_receipt['status']=='captured' and calendar_receipt['complete'] and calendar_receipt['http_status']==200,
        'retained calendar request differs')
    calendar_raw=checked(calendar_path.with_name(calendar_path.name.replace('.receipt.json','.raw')),
        calendar_receipt['sha256'],1<<20)
    require(len(calendar_raw)==calendar_receipt['bytes'],'calendar size differs')
    end=date(2026,10,9) if year==2026 else date(year,12,31)
    sessions=session_days(json.loads(calendar_raw)['response'],str(date(year,1,1)),str(end))
    open_days={date.fromisoformat(row['date']) for row in sessions}
    opening_symbols=summary['annual_lists'][str(year-1)+'1231'];symbols=source_symbols(summary,year)
    require(len(opening_symbols)==10 and len(symbols)<=50 and not set(symbols)&{'QCOM','META','FB'},'excluded symbol or list differs')
    rename_ref=None
    if year>=2025 and 'SQ' in symbols:
        path=old/'fixed-cohort-stock-windows-v2/rename-original-decoded.pdf'
        pin='40353660978610180d87a4040f76eaf209d81fa0398d3656492ef69af8ca4687'
        raw=checked(path,pin,2<<20);rename_ref=dict(file=str(path),sha256=pin,bytes=len(raw))
    coverage={s:set() for s in symbols};half_coverage={s:set() for s in symbols};sources=[];attempted=set()
    for base in (old,r):
        for intent in (base/'captures').glob('*/*.intent.json'):
            pin=sha(canonical(json.loads(intent.read_bytes())['query']))
            if base==r and intent.parent.name==spec.get('reviewed_failed_batch') and pin in recovery:continue
            attempted.add(pin)
        for path in (base/'captures').glob('*/*.receipt.json'):
            receipt=json.loads(path.read_bytes());q=receipt['query'];p=q['params'];symbol=logical_symbol(p)
            if q['path']!='/v3/stock/history/quote' or symbol not in coverage:continue
            if not all(p.get(k)==v for k,v in dict(venue='utp_cta',interval='1m').items()):continue
            window=p.get('start_time'),p.get('end_time')
            if window not in (('15:29:00','15:59:00'),('12:29:00','12:59:00')):continue
            target=coverage if window[0]=='15:29:00' else half_coverage
            if receipt['status']!='captured' or not receipt['complete'] or receipt['http_status']!=200:continue
            first=date.fromisoformat(str(p.get('start_date',p.get('date'))))
            last=date.fromisoformat(str(p.get('end_date',p.get('date'))))
            if first.year!=year or last.year!=year:continue
            raw=path.with_name(path.name.replace('.receipt.json','.raw'))
            checked(raw,receipt['sha256'],64<<20,retain=False)
            sources.append(dict(file=str(path),sha256=sha(path.read_bytes()),bytes=path.stat().st_size))
            while first<=last:target[symbol].add(first);first+=timedelta(days=1)
    queries=[]
    for month in range(1,end.month+1):
        for symbol in symbols:
            day=date(year,month,1);last=min((day.replace(day=28)+timedelta(days=4)).replace(day=1)-timedelta(days=1),end)
            if year==2021 and month==1:day=date(2021,1,4)
            while day<=last:
                if day in coverage[symbol]:day+=timedelta(days=1);continue
                first=day
                while day<=last and day not in coverage[symbol]:day+=timedelta(days=1)
                active=sorted(d for d in open_days if first<=d<day)
                if not active:continue
                groups=([d for d in active if d<RENAME],[d for d in active if d>=RENAME]) if symbol=='SQ' else (active,)
                for group in groups:
                    if not group:continue
                    q=dict(path='/v3/stock/history/quote',params=dict(symbol=vendor_symbol(symbol,group[0]),start_date=group[0].strftime('%Y%m%d'),
                        end_date=group[-1].strftime('%Y%m%d'),venue='utp_cta',interval='1m',
                        start_time='15:29:00',end_time='15:59:00',format='json'))
                    require(sha(canonical(q)) not in attempted,'previous failed query needs receipt review, not retry')
                    queries.append(q)
    for row in sessions:
        if row['close']!='13:00:00':continue
        day=date.fromisoformat(row['date'])
        for symbol in symbols:
            if day in half_coverage[symbol]:continue
            q=dict(path='/v3/stock/history/quote',params=dict(symbol=vendor_symbol(symbol,day),date=day.strftime('%Y%m%d'),venue='utp_cta',
                interval='1m',start_time='12:29:00',end_time='12:59:00',format='json'))
            require(sha(canonical(q)) not in attempted,'previous half-day query needs review')
            queries.append(q)
    require(1<=spec['limit']<=80,'excessive source batch')
    if not queries:
        print(json.dumps(dict(year=year,scheduled=0,captured=0,remaining_unattempted=0,
            source_or_strategy_admission_performed=False)));return
    selected=queries[:spec['limit']]
    locks=[]
    try:
        for directory in (old,r):
            fd=os.open(directory/'capture.lock',os.O_WRONLY|os.O_CREAT|os.O_NOFOLLOW,0o600)
            fcntl.flock(fd,fcntl.LOCK_EX|fcntl.LOCK_NB);locks.append(fd)
        plan=dict(spec,queries=selected,concurrency=4)
        write(r/('stock-'+spec['batch_id']+'-reuse.json'),dict(reused_receipts=sources,
            pending_monthly_requests=len(queries),selected_requests=len(selected),stock_prices_qualified=False,
            calendar_original_sha256=calendar_receipt['sha256'],calendar_receipt_sha256=sha(calendar_path.read_bytes()),
            opening_symbols=opening_symbols,source_symbols_including_possible_prior_holdings=symbols,reviewed_Block_rename=rename_ref,
            terminal_connected_before_batch=True,manually_reviewed_empty_transport_recovery=spec.get('reviewed_failed_batch')))
        capture_batch(plan,r,budget_check=budget_check,maximum_concurrency=4,
            reviewed_query_hashes=recovery.intersection(sha(canonical(q)) for q in selected))
    finally:
        for fd in locks:os.close(fd)


if __name__=='__main__':run(json.load(sys.stdin))
