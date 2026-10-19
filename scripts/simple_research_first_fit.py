"""Fixed-library conditional fits for scoped source review; never select a winner."""
from datetime import date,datetime,timedelta
from decimal import Decimal
import fcntl
import json
import os
from pathlib import Path
import re
import resource
import sys

from rd1_dispatch import checked,execute
from rd1_financial_verify import _loads,_parse_journal
from rd1_inputs import choose
from rd1_metrics import NY
from rd1_storage import canonical,require,root,sha,write
from simple_research_capture import budget_check
from simple_research_contract_plan import TARGETS
from simple_research_verify import CALL_FLOOR,CONDITIONAL,CONDITIONAL_PATH,analyze

MODES=('conservative','balanced','aggressive')
EXITS=('hold','tp50','tp75')


def run(spec):
    resource.setrlimit(resource.RLIMIT_AS,(4<<30,4<<30))
    r=root('rd1-261017-0748');budget_check(r,spec['observation'],600);consumed={}
    last_year=spec.get('last_year',2022)
    require(type(last_year) is int and 2022<=last_year<=2026,'fixed fit history differs')
    rule=spec.get('call_eligibility','')
    require(type(rule) is str and rule in ('',CALL_FLOOR),'unregistered call-eligibility experiment')
    export_only=spec.get('export_inputs_only',False)
    require(type(export_only) is bool and not (export_only and rule),'invalid source-only export scope')
    years=tuple(range(2021,last_year+1));label=f'conditional-fit-2021-{last_year}'+('-stock-cost-floor' if rule else '')
    revision=spec.get('revision','v1')
    require(re.fullmatch(r'v[1-9][0-9]?',revision),'invalid immutable fit revision')
    assumption=CONDITIONAL if last_year==2022 and not rule else CONDITIONAL_PATH
    def read(ref,limit=4<<20):
        raw=checked(ref['file'],ref['sha256'],limit,allow_empty=True)
        require('bytes' not in ref or len(raw)==ref['bytes'],'first-fit artifact size differs')
        consumed[ref['file']]=dict(ref,bytes=len(raw));return raw
    def original(path,pin,limit=4<<20):return json.loads(read(dict(file=str(r/path),sha256=pin),limit))
    def artifact(summary,name,limit=4<<20):
        refs=[v for v in summary['artifacts'] if v['file'].endswith('/'+name)]
        require(len(refs)==1,'ambiguous component artifact: '+name)
        return json.loads(read(refs[0],limit)),refs[0]
    build=json.loads(read(spec['build']));plan_raw=read(spec['plan']);plan_sha=sha(plan_raw)
    require(spec['designed_at'].startswith('2026-'),'model design clock is not historical')
    source=read(spec['controller']);require(source==Path(__file__).read_bytes(),'controller source differs')
    for ref in build['source_artifacts']:read(ref)
    read(build['binary_archive'],64<<20)
    checked(build['binary']['file'],build['binary']['sha256'],64<<20,lexar=False,retain=False)
    stocks=original('long-stock-stage-v3/summary.json','68317a286ed3ba1d785a7526cc504a2aef6a07206d2fdb608c6573534a74474a')
    observations={};sessions={};opening=[]
    for year in range(2021,min(last_year+1,2026)+1):
        row=next(v for v in stocks['years'] if v['year']==year)
        value=json.loads(read(row['stock_stage']))
        require(not value['missing_samples'] and not value['invalid_samples'],'stock data incomplete')
        for day in value['exchange_sessions']:sessions[day['date']]=day
        if year>last_year:continue
        require(len(value['opening_symbols'])==10 and not set(value['symbols'])&{'QCOM','META','FB'},'opening universe differs')
        opening.append(dict(**{'from':f'{year}-01-01','through':f'{year}-12-31'},
            symbols=value['opening_symbols'],stock_selection_sha256=row['stock_stage']['sha256']))
        for obs in value['observations']:
            require(obs['at'] not in observations,'duplicate stock clock')
            observations[obs['at']]=dict(obs,option_books={},observed_menus={})
    last_session='2026-10-09' if last_year==2026 else max(k for k in sessions if k[:4]==str(last_year))
    expected_dates=sorted(k for k in sessions if '2021-01-01'<=k<=last_session)
    require(len(observations)==4*len(expected_dates) and (last_year!=2022 or len(expected_dates)==503),
        'fixed-fit observation schedule differs')
    calendar=[];day=date(2021,1,1)
    calendar_end=date(2026,10,9) if last_year==2026 else date(last_year+1,1,31)
    while day<=calendar_end:
        ds=str(day);row=dict(date=ds)
        if ds in sessions:
            close=datetime.fromisoformat(ds+'T'+sessions[ds]['close']).replace(tzinfo=NY)
            row.update(open_at=close.replace(hour=9,minute=30).isoformat(),close_at=close.isoformat())
        calendar.append(row);day+=timedelta(days=1)
    ignored=[];combined={};menu_proofs=[];unobserved=[]
    quote_pins={2021:'b2681d471796638525abe3d84ad241d91a2d144975bb5440a14560dbe9be95f6',
        2022:'9eea114be6b9b1df0b737f42abf43b13d5d86d5b2fb01d24275bc39699e30620'}
    contract_pins={2021:'89d3dea879c5d3bbb2c3d78c2e463f81737c06b27109944a913d4b0faa421a19',
        2022:'ed97ed319975a5f10db1d0ed020719a51c8226bed602b4f4a642cb008225cd16'}
    for year,value in spec.get('additional_years',{}).items():
        year=int(year);require(year in years and year>2022,'additional fit source outside declared years')
        quote_pins[year]=value['quote_audit_sha256'];contract_pins[year]=value['contract_plan_sha256']
    require(set(quote_pins)==set(contract_pins)==set(years),'fixed fit source components incomplete')
    expected_menus=0
    for year in years:
        override=spec.get('additional_years',{}).get(str(year),{}).get('quote_audit')
        if override:
            require(override['sha256']==quote_pins[year],'quote recovery summary binding differs')
            audit=json.loads(read(override))
        else:audit=original(f'quote-audit-{year}-v1/summary.json',quote_pins[year])
        require(audit['schema']=='AutoWheelSimpleQuoteAuditV1' and audit['year']==year
            and audit['contract_plan']['sha256']==contract_pins[year],'quote audit scope differs')
        require(audit['invalid_source_responses']==0,'holding quote source malformed')
        if audit['missing_potential_samples']:
            require(spec.get('allow_unheld_quote_gaps') is True and year==2024 and last_year>=2024,
                'unreviewed potential quote gaps')
            missing,missing_ref=artifact(audit,'missing-potential-samples.json')
            require(len(missing)==audit['missing_potential_samples']<=636
                and all(k.split('|')[1][:10]=='2024-06-21' for k in missing),
                'potential gap differs from reviewed June21 source issue')
            unobserved.append(dict(year=year,coordinates=missing,source=missing_ref))
        index,index_ref=artifact(audit,'quote-index.json',128<<20)
        for key,value in index.items():
            cid,stamp=key.split('|');clock=datetime.fromisoformat(stamp).replace(tzinfo=NY).isoformat()
            if clock not in observations:continue
            compact={k:v for k,v in value.items() if k!='raw_sha256'}
            if key in combined:require(combined[key]==compact,'overlapping quote books disagree')
            combined[key]=compact
            if 'invalid_reason' in value:
                ignored.append(dict(contract_id=cid,at=clock,reason=value['invalid_reason'],source=index_ref));continue
            observations[clock]['option_books'][cid]={k:value[k] for k in ('bid_cents','ask_cents')}
        contract=original(f'contract-plan-{year}-v1/summary.json',contract_pins[year])
        expected_menus+=contract['weekly_menus']
        menus,menu_ref=artifact(contract,'eligible-signal-menus.json',32<<20)
        choices,_=artifact(contract,'choices.json')
        fixed={(v['day'],v['symbol'],v['mode'],v['kind']):v['chosen'] for v in choices}
        for menu in menus:
            obs=observations[menu['at']];symbol=menu['symbol'];stock=obs['stock_books'][symbol]
            spot2=stock['bid_cents']+stock['ask_cents'];full=menu['menu']
            selected={fixed[menu['day'],symbol,mode,kind]['id'] for mode,kind,_,_ in TARGETS
                if fixed[menu['day'],symbol,mode,kind] is not None}
            reduced=[c for c in full if c['id'] in selected]
            def raw(values):return [dict(symbol=c['symbol'],expiration=c['expiry'],right=c['kind'].upper(),
                strike=Decimal(c['strike_cents'])/100) for c in values]
            exact=True
            for mode,kind,num,den in TARGETS:
                a=choose(raw(full),menu['day'],symbol,kind,spot2,num,den)
                b=choose(raw(reduced),menu['day'],symbol,kind,spot2,num,den)
                require(a==fixed[menu['day'],symbol,mode,kind],'fixed contract identity changed')
                exact=exact and a==b
            retained=reduced if exact else full
            obs['observed_menus'][symbol]=retained
            for cid in selected:
                expected={k:menu['signal_books'][cid][k] for k in ('bid_cents','ask_cents')};actual=obs['option_books'].get(cid)
                if actual is not None:require(actual==expected,'menu and holding signal quotes disagree')
            menu_proofs.append(dict(day=menu['day'],symbol=symbol,full_count=len(full),retained_count=len(retained),
                all_five_fixed_choices_preserved=True,compacted=exact,source=menu_ref))
    require(len(menu_proofs)==expected_menus,'weekly menu count differs')
    for obs in observations.values():
        require(len(obs['option_books'])<=500,'option observation exceeds existing bound')
        obs['source_packet_sha256']=sha(canonical(dict(stock_component=obs['source_packet_sha256'],
            quote_audit_summaries=quote_pins,contract_plan_summaries=contract_pins,
            at=obs['at'],option_books=obs['option_books'],observed_menus=obs['observed_menus'])))
    clearing=original('clearing-model-2021-2022-v1/summary.json','8d13c42751a2fde09c7e61a1a160ba6e086f7b086ba50989c717752c9d6c0d4d')
    clearing_days,clearing_ref=artifact(clearing,'clearing-days.json')
    dividend_review=original('cashflow-field-review-2021-2022-v1/summary.json','fa487e2bfae989e9ea8ec0d2744008c13ad60f08b7cce69522b541a0b7a6c12d')
    dividends,_=artifact(dividend_review,'dividends.json')
    reference_review=original('reference-field-review-2021-2022-v1/summary.json','d5b31da807a8830d8d76cde7acd633fd150e9fd32e3b87ea808664cef7944e7b')
    references,_=artifact(reference_review,'reference-index.json')
    require(len(references)==5532 and all(v['conditional_reference_admitted'] for v in references.values()),'reference review incomplete')
    clearing_by_day={v['date']:v for v in clearing_days};clearing_refs=[clearing_ref]
    for year in years[2:]:
        component=spec['additional_years'][str(year)]
        more=original(f'clearing-model-{year}-v1/summary.json',component['clearing_sha256'])
        values,ref=artifact(more,'clearing-days.json');clearing_refs.append(ref)
        for value in values:
            require(value['date'] not in clearing_by_day or clearing_by_day[value['date']]==value,
                'overlapping clearing components disagree')
            clearing_by_day[value['date']]=value
        more=original(f'cashflow-field-review-{year}-v1/summary.json',component['cashflow_sha256'])
        values,_=artifact(more,'dividends.json');dividends.extend(values)
        more=original(f'reference-field-review-{year}-v1/summary.json',component['reference_sha256'])
        values,_=artifact(more,'reference-index.json')
        require(not set(values)&set(references) and all(v['conditional_reference_admitted'] for v in values.values()),
            'overlapping or unreviewed reference components')
        references.update(values)
    clearing_days=[v for _,v in sorted(clearing_by_day.items())]
    require(len({v['id'] for v in dividends})==len(dividends),'duplicate dividend component')
    refs=[{k:v[k] for k in ('symbol','day','at','price_micros','source_packet_sha256')} for _,v in sorted(references.items())]
    dirname='conditional-fit-inputs-'+revision if last_year==2022 and not rule else label+'-inputs-'+revision
    out=r/(dirname+('-source-export' if export_only else ''));out.mkdir(mode=0o700)
    proof=write(out/'menu-compaction-proof.json',menu_proofs)
    invalid=write(out/'invalid-potential-books.json',ignored)
    absent=write(out/'unobserved-potential-books.json',unobserved)
    scope=write(out/'scope.json',dict(schema='AutoWheelSimpleConditionalFirstFitScopeV1',
        first_session='2021-01-04',last_session=last_session,sessions=len(expected_dates),designs=9,contract_assumption=assumption,
        purpose='actual_path_inventory_for_scoped_contract_and_action_review',
        historical_deliverable_intervals_qualified=False,held_action_completeness_qualified=False,
        actual_broker_fees_or_fills_asserted=False,strategy_selection_qualified=False,
        menu_compaction_proof=proof,invalid_potential_quotes_omitted_not_imputed=invalid,
        unobserved_potential_quotes_not_imputed=absent,all_nine_designs_retained_despite_source_gaps=True,
        realization_caps_applied=False,missing_held_marks_stop=True))
    data=dict(schema='AutoWheelSimpleResearchDataV1',protocol_sha256=plan_sha,
        admission_status='conditional_observed_prices_pending_contract_action_review',admission_receipt_sha256=scope['sha256'],
        unqualified_inputs=[],calendar_source_sha256=sha(canonical(calendar)),
        clearing_source_sha256=clearing_ref['sha256'] if last_year==2022 else sha(canonical(clearing_refs)),
        first_session='2021-01-04',last_session=last_session,exchange_days=calendar,clearing_days=clearing_days,
        observations=[v for _,v in sorted(observations.items())],opening_policies=opening,tariffs=[],standard_terms=[],
        dividends=dividends,regular_session_references=refs,unsupported_actions=[])
    write(out/'consumed-sources.json',list(consumed.values()))
    if export_only:
        template=write(out/'source-template.json',dict(schema='AutoWheelSimpleResearchInputTemplateV1',
            sampled_data=data,scope=scope,modeled_costs=dict(plan_sha256=plan_sha,designed_at=spec['designed_at'],scale=1),
            contract_assumption=assumption,opening_profiles_not_selected=True,valid_replay_packet=False,
            source_or_strategy_admission_performed=False,financial_cases=0))
        print(json.dumps(dict(source_template=template,financial_cases=0)),flush=True)
        return
    fd=os.open(r/'financial.lock',os.O_RDWR|os.O_CREAT|os.O_NOFOLLOW,0o600)
    fcntl.flock(fd,fcntl.LOCK_EX|fcntl.LOCK_NB);results=[];halt=False
    try:
        for mode in MODES:
            for exit_rule in EXITS:
                budget_check(r,spec['observation'],180)
                for policy in data['opening_policies']:policy['profile']=dict(mode=mode,exit=exit_rule)
                packet=dict(schema='AutoWheelSimpleResearchPacketV1',modeled_costs=dict(plan_sha256=plan_sha,
                    designed_at=spec['designed_at'],scale=1),contract_assumption=assumption,sampled_data=data)
                if rule:packet['call_eligibility']=rule
                packet_ref=write(out/(mode+'-'+exit_rule+'.json'),packet)
                existing=list((r/'reservations').glob('*.json'))
                require(len(existing)<399 and all((r/'dispatch'/p.stem/'terminal.json').is_file() for p in existing),
                    'case budget or unresolved prior reservation')
                case_id=label+'-'+mode+'-'+exit_rule+'-'+revision
                dispatch=r/'dispatch'/case_id;dispatch.mkdir(mode=0o700)
                write(r/'reservations'/(case_id+'.json'),dict(case_id=case_id,purpose='conditional_path_review',
                    packet=packet_ref,build=spec['build'],controller=spec['controller'],scope=scope,
                    combined_charged_cases=2+len(existing),max_runtime_seconds=180))
                resources=None;report=None;verification=None;error=None;artifacts={}
                try:
                    directory=r/'cases'/case_id
                    resources=execute([build['binary']['file'],'-experiment','-packet',packet_ref['file'],
                        '-packet-sha256',packet_ref['sha256'],'-out',str(directory)],180,dispatch/'stdout',dispatch/'stderr')
                    for name in ('journal.jsonl','result.json','diagnostics.jsonl'):
                        p=directory/name
                        if p.exists():
                            raw=p.read_bytes();ref=dict(file=str(p),sha256=sha(raw),bytes=len(raw));read(ref,32<<20);artifacts[name]=ref
                    require(resources['exit_code']==0 and not resources['stopped_reason'],'conditional replay incomplete; inspect retained result')
                    j,z,d=(artifacts[n] for n in ('journal.jsonl','result.json','diagnostics.jsonl'))
                    records=_parse_journal(read(j,32<<20));result=_loads(read(z))
                    report=analyze(packet,records,result,[_loads(x) for x in read(d,32<<20).splitlines()],
                        j['file'],j['sha256'],z['file'],z['sha256'])
                    verification=write(dispatch/'verification.json',report)
                except Exception as e:error=type(e).__name__+':'+str(e)[:500];report=None
                terminal=dict(case_id=case_id,profile=mode+'-'+exit_rule,
                    status='complete_conditional_arithmetic' if report else 'failed',resources=resources,
                    verification=verification if report else None,error=error,artifacts=artifacts,
                    historical_contract_and_action_admission=False,winner_selected=False)
                ref=write(dispatch/'terminal.json',terminal);results.append(dict(terminal=ref,**terminal))
                compact=dict(case_id=case_id,status=terminal['status'],error=error,resources=resources)
                if report:
                    metrics=report['metrics'];f=metrics['financial_components']
                    compact.update(profit_cents=metrics['net_profit_cents'],near_close_drawdown=metrics['maximum_near_close_drawdown']['fraction'],
                        fees_cents=f['fees_cents'],assignment_rate=f['assignment_rate'],callaway_rate=f['callaway_rate'],
                        net_realized_share=f['net_realized_share'])
                print(json.dumps(compact),flush=True)
                # Account-specific unknown marks must remain failed paths; the
                # other frozen designs still answer independent path questions.
                if error:
                    failed_result=json.loads(read(artifacts['result.json'])) if 'result.json' in artifacts else {}
                    source_reason=failed_result.get('reason','')
                    expected_unknown=bool(resources and not resources.get('stopped_reason')
                        and failed_result.get('status')=='incomplete'
                        and ('missing' in source_reason.lower() or source_reason=='RD1 held option mark absent'))
                    if not expected_unknown:halt=True;break
            if halt:break
    finally:os.close(fd)
    summary=write(r/(label+'-'+revision+'-summary.json'),dict(schema='AutoWheelSimpleConditionalFirstFitSummaryV1',
        cases=results,charged_cases=len(results),complete_cases=sum(v['status']=='complete_conditional_arithmetic' for v in results),
        contract_assumption=assumption,full_source_admission=False,selection_performed=False,full_goal_complete=False))
    print(json.dumps(dict(summary=summary)),flush=True)


if __name__=='__main__':run(json.load(sys.stdin))
