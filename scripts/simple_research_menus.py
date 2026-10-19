"""Collect only unattempted queries from immutable weekly option-menu plans."""
import fcntl
import json
import os
import sys
import urllib.request

from rd1_capture import capture_batch
from rd1_dispatch import checked
from rd1_storage import canonical,require,root,sha,write
from simple_research_capture import budget_check


def run(spec):
    r=root('rd1-261017-0748');old=root('rd1-261016-2037');budget_check(r,spec['observation'],spec['timeout_seconds'])
    with urllib.request.urlopen('http://127.0.0.1:25543/v3/terminal/mdds/status',timeout=8) as response:
        require(response.status==200 and response.read(2048)==b'CONNECTED','Theta terminal is not connected')
    summary=json.loads(checked(r/'long-stock-stage-v3/summary.json',
        '68317a286ed3ba1d785a7526cc504a2aef6a07206d2fdb608c6573534a74474a',1<<20))
    year=spec['year'];require(type(year) is int and 2021<=year<=2026,'menu year')
    report=next(v for v in summary['years'] if v['year']==year);plan_ref=report['weekly_menu_plan']
    plan=json.loads(checked(plan_ref['file'],plan_ref['sha256'],4<<20))
    require(not set(plan['symbols'])&{'QCOM','META','FB'} and plan['option_scores_used'] is False,'menu plan changed scope')
    prior={}
    for base in (old,r):
        for path in (base/'captures').glob('*/*.intent.json'):
            pin=sha(canonical(json.loads(path.read_bytes())['query']))
            receipt=path.with_name(path.name.replace('.intent.json','.receipt.json'))
            prior.setdefault(pin,[]).append(receipt)
    pending=[];blocked=[];completed=0
    for query in plan['queries']:
        previous=prior.get(sha(canonical(query)),[])
        good=[p for p in previous if p.is_file() and json.loads(p.read_bytes())['status']=='captured']
        if good:
            for path in good:
                receipt=json.loads(path.read_bytes())
                require(receipt['complete'] and receipt['http_status']==200,'completed menu transport differs')
                checked(path.with_name(path.name.replace('.receipt.json','.raw')),receipt['sha256'],64<<20,retain=False)
            completed+=1
        elif previous:blocked.append(dict(query=query,attempted_receipts=[str(p) for p in previous]))
        else:pending.append(query)
    require(1<=spec['limit']<=320,'excessive menu batch')
    if not pending:
        print(json.dumps(dict(year=year,remaining_unattempted=0,blocked_queries=len(blocked),previous_completed=completed,
            source_or_strategy_admission_performed=False)));return
    selected=pending[:spec['limit']];locks=[]
    try:
        for directory in (old,r):
            fd=os.open(directory/'capture.lock',os.O_WRONLY|os.O_CREAT|os.O_NOFOLLOW,0o600)
            fcntl.flock(fd,fcntl.LOCK_EX|fcntl.LOCK_NB);locks.append(fd)
        selection=write(r/(spec['batch_id']+'-selection.json'),dict(plan=plan_ref,selected_queries=selected,
            previous_completed=completed,blocked_queries=blocked,remaining_unattempted=len(pending)-len(selected),
            unresolved_coordinates_remain_in_original_plan=True,terminal_connected_before_batch=True))
        result=capture_batch(dict(spec,queries=selected,concurrency=4),r,budget_check=budget_check,maximum_concurrency=4,maximum_queries=320)
        print(json.dumps(dict(year=year,selection=selection,remaining_unattempted=len(pending)-len(selected),
            blocked_queries=len(blocked),batch_complete=result['captured']==result['scheduled'],
            captured=result['captured'],scheduled=result['scheduled'],source_or_strategy_admission_performed=False)),flush=True)
    finally:
        for fd in locks:os.close(fd)


if __name__=='__main__':run(json.load(sys.stdin))
