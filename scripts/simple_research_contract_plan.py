"""Freeze identities and required holding windows before longer account scores."""
from datetime import date,datetime,timedelta
from decimal import Decimal
import json
from pathlib import Path
import sys

from rd1_dispatch import checked
from rd1_inputs import cents,choose,contract,receipt_rows
from rd1_metrics import NY
from rd1_storage import canonical,require,root,sha,write
from simple_research_capture import budget_check,logical_symbol,vendor_symbol

TARGETS=(('conservative','put',9,10),('conservative','call',21,20),
    ('balanced','put',19,20),('balanced','call',103,100),('aggressive','call',21,20))


def run(spec):
    r=root('rd1-261017-0748');old=root('rd1-261016-2037');budget_check(r,spec['observation'],600)
    def read(ref,limit=32<<20):return checked(ref['file'],ref['sha256'],limit)
    summary=json.loads(checked(r/'long-stock-stage-v3/summary.json',
        '68317a286ed3ba1d785a7526cc504a2aef6a07206d2fdb608c6573534a74474a',1<<20))
    year=spec['year'];require(type(year) is int and 2021<=year<=2026,'contract-plan year')
    report=next(v for v in summary['years'] if v['year']==year)
    require(report['missing_stock_samples']==report['invalid_stock_samples']==0,'stock signal coverage incomplete')
    plan=json.loads(read(report['weekly_menu_plan']));stock=json.loads(read(plan['stock_stage']))
    prices={v['at']:v for v in stock['observations']};days={v['date']:v for v in stock['exchange_sessions']}
    paths=list(plan['reused_menu_receipts']);consumed=[];menus={};capture_index={};attempts={}
    for path in (r/'captures').glob('*/*.receipt.json'):
        value=json.loads(path.read_bytes())
        pin=sha(canonical(value['query']));attempts.setdefault(pin,[]).append(path)
        if value['status']=='captured':capture_index.setdefault(pin,[]).append(path)
    components=list(plan['partitioned_menu_components']);overrides={}
    for recovery_ref in spec.get('additional_menu_components',[]):
        reviewed=json.loads(read(recovery_ref,1<<20));component=reviewed['component'];value=json.loads(read(component,1<<20))
        key=value['day'],value['symbol']
        require(key not in overrides and value['day'][:4]==str(year) and value['symbol'] in plan['symbols']
            and reviewed['status']=='observed_strategy_relevant_menu_complete','additional menu recovery scope differs')
        overrides[key]=reviewed;components.append(dict(summary=recovery_ref,component=component))
    for query in plan['queries']:
        pin=sha(canonical(query));key=str(date.fromisoformat(query['params']['date'])),logical_symbol(query['params'])
        if key in overrides:
            originals=attempts.get(pin,[]);reviewed=overrides[key]
            require(len(originals)==1 and sha(originals[0].read_bytes())==reviewed['original_failure_receipt_sha256'],
                'additional recovery is not bound to the failed original query')
            original=json.loads(originals[0].read_bytes());bad=original['status']!='captured'
            if not bad:
                try:receipt_rows(originals[0])
                except ValueError:bad=True
            require(bad,'a recovery cannot replace a valid menu')
            continue
        matches=capture_index.get(pin,[])
        require(len(matches)==1,'planned menu is missing or has ambiguous attempts')
        path=matches[0];paths.append(dict(file=str(path),sha256=sha(path.read_bytes()),bytes=path.stat().st_size))
    def clock(day):
        return datetime.fromisoformat(day+'T'+days[day]['close'])-timedelta(minutes=31)
    for ref in paths:
        path=Path(ref['file']);read(ref,1<<20);receipt,groups=receipt_rows(path)
        p=receipt['query']['params'];symbol=logical_symbol(p);day=str(date.fromisoformat(p['date']))
        signal=clock(day);stamp=signal.isoformat(timespec='milliseconds');key=day,symbol
        require(receipt['query']['path']=='/v3/option/history/quote' and p['expiration']=='*'
            and p['max_dte']==45 and p['interval']=='1m' and p['start_time']==signal.strftime('%H:%M:%S')
            and p['end_time']==(signal+timedelta(minutes=1)).strftime('%H:%M:%S')
            and p['symbol']==vendor_symbol(symbol,date.fromisoformat(day)) and key not in menus,'menu framing differs')
        values=[];books={};seen=set()
        for group in groups:
            raw=group['contract'];require(raw['symbol']==p['symbol'],'source option root differs')
            normalized=dict(raw,symbol=symbol);c=contract(normalized)
            rows=[v for v in group['data'] if v['timestamp']==stamp]
            require(len(rows)<=1 and c['id'] not in seen,'conflicting menu identity or signal bucket');seen.add(c['id'])
            if not rows:continue
            values.append(normalized);row=rows[0]
            books[c['id']]=dict(bid_cents=cents(row['bid']),ask_cents=cents(row['ask']))
        menus[key]=(values,books,ref);consumed.append(ref)
    for recovery in components:
        reviewed=json.loads(read(recovery['summary'],1<<20));component=json.loads(read(recovery['component'],1<<20))
        day,symbol=component['day'],component['symbol'];key=day,symbol
        require(reviewed['status']=='observed_strategy_relevant_menu_complete' and reviewed['component']==recovery['component'] and key not in menus
            and component['signal_at']==clock(day).isoformat(timespec='milliseconds'),'partitioned menu differs')
        # Converting exact cents to a decimal string preserves the source units.
        values=[dict(symbol=c['symbol'],expiration=c['expiry'],right=c['kind'].upper(),
            strike=Decimal(c['strike_cents'])/100) for c in component['menu']]
        menus[key]=(values,component['signal_rows'],recovery['component']);consumed.extend([recovery['summary'],recovery['component']])
    weekly=[];prior=None
    for day in days:
        week=date.fromisoformat(day).isocalendar()[:2]
        if week!=prior:weekly.append(day)
        prior=week
    require(set(menus)=={(d,s) for d in weekly for s in plan['symbols']},'required weekly menu domain is incomplete')
    choices=[];required={};snapshots=[]
    for (day,symbol),(values,books,ref) in sorted(menus.items()):
        at=clock(day).replace(tzinfo=NY).isoformat();b=prices[at]['stock_books'][symbol];spot2=b['bid_cents']+b['ask_cents']
        retained=[]
        for raw in values:
            c=contract(raw);dte=(date.fromisoformat(c['expiry'])-date.fromisoformat(day)).days;k2=2*c['strike_cents']
            if 21<=dte<=45 and spot2*4<=k2*5<=spot2*6:retained.append(c)
        retained_ids={c['id'] for c in retained}
        snapshots.append(dict(day=day,symbol=symbol,at=at,source=ref,menu=retained,
            signal_books={cid:{k:v[k] for k in ('bid_cents','ask_cents')} for cid,v in books.items() if cid in retained_ids}))
        for mode,kind,n,d in TARGETS:
            chosen=choose(values,day,symbol,kind,spot2,n,d)
            require(chosen is None or chosen['id'] in retained_ids,'structural domain filter changes a choice')
            choices.append(dict(day=day,symbol=symbol,mode=mode,kind=kind,chosen=chosen))
            if chosen:
                cid=chosen['id'];previous=required.get(cid)
                if previous is None or day<previous['first_choice_day']:
                    required[cid]=dict(contract=chosen,first_choice_day=day,through=min(chosen['expiry'],'2026-10-09'))
    comparisons=0
    if year==2021:
        previous=json.loads(checked(old/'pilot-contract-plan-v1/choices.json',
            '47a779c3f9872f850c9ab2fadeda4635c8aa38da0930fe4cc2e8a8c67225fac4',4<<20))
        lookup={(v['day'],v['symbol'],v['mode'],v['kind']):v['chosen'] for v in previous}
        for value in choices:
            key=(value['day'],value['symbol'],value['mode'],value['kind'])
            original=(value['day'],value['symbol'],'conservative','call') if value['mode']=='aggressive' else key
            if original in lookup:
                require(value['chosen']==lookup[original],'longer input framing changes a retained pilot choice');comparisons+=1
        require(comparisons==140,'retained pilot identity comparison denominator differs')
    out=r/f'contract-plan-{year}-v1';out.mkdir(mode=0o700)
    refs=[write(out/'choices.json',choices),write(out/'eligible-signal-menus.json',snapshots),
        write(out/'required-contracts.json',list(required.values())),write(out/'consumed-menu-sources.json',consumed)]
    result=dict(schema='AutoWheelSimpleContractPlanV1',year=year,opening_symbols=plan['opening_symbols'],
        source_symbols=plan['symbols'],weekly_menus=len(menus),mode_side_choices=len(choices),distinct_required_contracts=len(required),
        retained_pilot_identity_comparisons=comparisons,artifacts=refs,
        menu_filter='predeclared_21_to_45_DTE_and_80_to_120_percent_spot_domain_only',
        contract_ID_convention='stable_logical_symbol_expiry_right_strike;original_vendor_roots_retained_in_sources',
        option_side_price_or_future_return_optimization=False,financial_cases=0,performance_generated=False,
        historical_contract_actions_dividends_and_clearing_admitted=False)
    print(json.dumps(dict(summary=write(out/'summary.json',result),report=result)),flush=True)


if __name__=='__main__':run(json.load(sys.stdin))
