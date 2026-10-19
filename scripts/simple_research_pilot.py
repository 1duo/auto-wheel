"""Bounded exploratory pilot on retained quotes; no network or broker client."""
from datetime import date,datetime,timedelta,timezone
import fcntl
import json
import os
from pathlib import Path
import resource
import sys
import time

from rd1_dispatch import checked,execute
from rd1_financial_verify import _loads,_parse_journal
from rd1_metrics import NY
from rd1_metrics import SEED,at,integer,series_metrics,session_calendar
from rd1_storage import canonical,require,root,sha,write
from simple_research_verify import ASSUMPTION,analyze

MODES=('conservative','balanced','aggressive')
EXITS=('hold','tp50','tp75')


def run(spec):
    r=root('rd1-261017-0748');old=root('rd1-261016-2037');consumed={}
    def usage():
        obs=spec['observation'];age=(datetime.now(timezone.utc)-datetime.fromisoformat(obs['recorded_at'])).total_seconds()
        require(obs['goal']['status']=='active' and 0<=age<=900
            and 24429+obs['goal']['timeUsedSeconds']+age+120<72000,'remaining research budget or admission clock')
    usage()
    def read(ref,limit=32<<20):
        p=Path(ref['file']);raw=checked(p,ref['sha256'],limit,allow_empty=True)
        if 'bytes' in ref:require(len(raw)==ref['bytes'],'pilot artifact size differs')
        consumed[str(p)]=dict(ref,bytes=len(raw));return raw
    def original(path,pin):return read(dict(file=str(path),sha256=pin))
    checkpoint=_loads(original(old/'checkpoints/checkpoint-v18.json',
        'ac7210ff14a50e8b1080b6efcd7782630e84209decc447bea94b9587697aa3bd'))
    evidence={v['file']:v for v in checkpoint['evidence']}
    # Resource timings in this pinned metadata may be fractional seconds;
    # financial packets and journals keep the exact-number decoder below.
    audit=json.loads(read(evidence[str(old/'pilot-sampled-input-audit-v1/summary.json')]))
    require(audit['passed'] and audit['independently_decoded_stock_books']==520
        and audit['independently_decoded_option_books']==11080,'retained independent source audit differs')
    for artifact in audit['artifacts']:
        for item in _loads(read(artifact)):
            p=Path(item['file']);checked(p,item['sha256'],64<<20,retain=False)
            consumed[str(p)]=item
    stage=_loads(original(old/'pilot-sampled-input-stage-v1/sampled-inputs.json',
        '4a4097b681f62cdb8e90d2dc5840821360abd1094d1413a64591c6ab701a4e30'))
    build=_loads(read(spec['build']))
    for item in build['source_artifacts']:read(item)
    read(build['binary_archive'],64<<20)
    checked(build['binary']['file'],build['binary']['sha256'],64<<20,lexar=False,retain=False)
    plan=read(spec['plan']);plan_sha=sha(plan)
    require(spec['designed_at'].startswith('2026-') and len(stage['observations'])==260,'pilot design or sample envelope')
    days={v['date']:v for v in stage['exchange_sessions']};calendar=[];day=date(2021,1,1)
    while day<=date(2021,4,16):
        ds=str(day);row=dict(date=ds)
        opened=ds in days if ds<='2021-04-07' else day.weekday()<5
        if opened:
            cl=datetime.fromisoformat(ds+'T'+(days[ds]['close'] if ds in days else '16:00:00')).replace(tzinfo=NY)
            row.update(open_at=cl.replace(hour=9,minute=30).isoformat(),close_at=cl.isoformat())
        calendar.append(row);day+=timedelta(days=1)
    require(sum('open_at' in v and v['date']<='2021-04-07' for v in calendar)==65,'pilot session denominator')
    dividends=[{k:v[k] for k in ('id','symbol','ex_at','known_at','amount_for_100_shares_cents','pay_at','available_at','source_sha256')}
        for v in stage['dividends']]
    references=[{k:v[k] for k in ('symbol','day','at','price_micros','source_packet_sha256')}
        for _,v in sorted(stage['regular_session_reference_components'].items())]
    scope=dict(schema='AutoWheelSimpleExploratoryPilotScopeV1',source_stage_sha256='4a4097b681f62cdb8e90d2dc5840821360abd1094d1413a64591c6ab701a4e30',
        contract_assumption=ASSUMPTION,missing_reference_facts=['historical_deliverable_intervals','held_action_completeness'],
        actual_broker_fees_asserted=False,actual_broker_fills_asserted=False,strategy_recommendation_qualified=False,
        calendar_scope='reviewed_2021_pilot_sessions_and_known_following_weekdays_for_reentry_calendar',
        source_audit_complete_for_prices_not_contract_economics=True)
    out=r/'pilot-inputs-v1';out.mkdir(mode=0o700)
    scope_ref=write(out/'scope.json',scope)
    cohort_ref=write(out/'fixed-pilot-symbols.json',dict(symbols=['AAPL','MSFT'],purpose='mechanics_not_universe_selection'))
    packets={}
    for mode in MODES:
        for exit_rule in EXITS:
            profile=mode+'-'+exit_rule
            data=dict(schema='AutoWheelSimpleResearchDataV1',protocol_sha256=plan_sha,
                admission_status='observed_pilot_prices_with_explicit_contract_assumption',admission_receipt_sha256=scope_ref['sha256'],
                unqualified_inputs=[],calendar_source_sha256=sha(canonical(calendar)),clearing_source_sha256=sha(canonical(stage['clearing_days'])),
                first_session=stage['first_session'],last_session=stage['last_session'],exchange_days=calendar,
                clearing_days=stage['clearing_days'],observations=stage['observations'],
                opening_policies=[dict(**{'from':'2021-01-01','through':'2021-12-31'},symbols=['AAPL','MSFT'],
                    profile=dict(mode=mode,exit=exit_rule),stock_selection_sha256=cohort_ref['sha256'])],
                tariffs=[],standard_terms=[],dividends=dividends,regular_session_references=references,unsupported_actions=[])
            packet=dict(schema='AutoWheelSimpleResearchPacketV1',modeled_costs=dict(plan_sha256=plan_sha,
                designed_at=spec['designed_at'],scale=1),contract_assumption=ASSUMPTION,sampled_data=data)
            packets[profile]=(packet,write(out/(profile+'.json'),packet))
    write(out/'consumed-sources.json',list(consumed.values()))
    for name in ('reservations','dispatch','cases'): (r/name).mkdir(mode=0o700,exist_ok=True)
    lock=os.open(r/'financial.lock',os.O_RDWR|os.O_CREAT|os.O_NOFOLLOW,0o600)
    fd=lock;fcntl.flock(fd,fcntl.LOCK_EX|fcntl.LOCK_NB)
    results=[]
    try:
        for profile,(packet,packet_ref) in packets.items():
            usage();case_id='exploratory-pilot-'+profile+'-v1'
            existing=list((r/'reservations').glob('*.json'))
            require(len(existing)<399 and all((r/'dispatch'/p.stem/'terminal.json').is_file() for p in existing),
                'case budget or unresolved prior reservation')
            dispatch=r/'dispatch'/case_id;dispatch.mkdir(mode=0o700)
            write(r/'reservations'/(case_id+'.json'),dict(case_id=case_id,purpose='exploratory_pilot',
                profile=profile,packet=packet_ref,build=spec['build'],plan_sha256=plan_sha,
                contract_assumption=ASSUMPTION,combined_charged_cases=2+len(existing),max_runtime_seconds=120))
            resources=None;report=None;verification=None;error=None
            try:
                resources=execute([build['binary']['file'],'-experiment','-packet',packet_ref['file'],
                    '-packet-sha256',packet_ref['sha256'],'-out',str(r/'cases'/case_id)],120,dispatch/'stdout',dispatch/'stderr')
                require(resources['exit_code']==0 and not resources['stopped_reason'],'pilot process incomplete')
                artifacts={}
                for name in ('journal.jsonl','result.json','diagnostics.jsonl'):
                    p=r/'cases'/case_id/name;raw=p.read_bytes()
                    ref=dict(file=str(p),sha256=sha(raw),bytes=len(raw));read(ref);artifacts[name]=ref
                j,z,d=(artifacts[n] for n in ('journal.jsonl','result.json','diagnostics.jsonl'))
                records=_parse_journal(read(j));result=_loads(read(z));diagnostics=[_loads(x) for x in read(d).splitlines()]
                report=analyze(packet,records,result,diagnostics,j['file'],j['sha256'],z['file'],z['sha256'])
                verification=write(dispatch/'verification.json',report)
            except Exception as e:
                report=None;verification=None;error=type(e).__name__+':'+str(e)[:500]
            terminal=dict(case_id=case_id,profile=profile,status='complete_exploratory_arithmetic' if report else 'failed',
                resources=resources,error=error,verification=verification if report else None,
                contract_and_action_history_qualified=False,strategy_selection_qualified=False)
            ref=write(dispatch/'terminal.json',terminal);results.append(dict(terminal=ref,**terminal))
            if report:
                m=report['metrics'];f=m['financial_components']
                print(json.dumps(dict(case_id=case_id,status=terminal['status'],profit_cents=m['net_profit_cents'],
                    near_close_drawdown=m['maximum_near_close_drawdown']['fraction'],fees_cents=f['fees_cents'],
                    assignment_rate=f['assignment_rate'],callaway_rate=f['callaway_rate'],
                    net_realized_share=f['net_realized_share'],resources=resources)),flush=True)
            else:
                print(json.dumps(dict(case_id=case_id,status='failed',error=error,resources=resources)),flush=True)
                break
    finally:os.close(fd)
    summary=write(r/'exploratory-pilot-v1-summary.json',dict(schema='AutoWheelSimpleExploratoryPilotSummaryV1',cases=results,
        new_cases_charged=len(results),complete_cases=sum(x['status']=='complete_exploratory_arithmetic' for x in results),
        all_nine_complete=len(results)==9 and all(x['status']=='complete_exploratory_arithmetic' for x in results),
        contract_assumption=ASSUMPTION,actual_historical_contracts_qualified=False,winner_selected=False,full_goal_complete=False))
    print(json.dumps(dict(summary=summary)),flush=True)


def cost_checks(spec):
    """Replay the three fixed hold baselines with doubled, explicit model costs."""
    r=root('rd1-261017-0748')
    def read(ref,limit=32<<20):return checked(ref['file'],ref['sha256'],limit,allow_empty=True)
    summary=json.loads(read(spec['pilot_summary']))
    require(summary['all_nine_complete'],'incomplete pilot baselines')
    source=read(spec['controller'])
    require(sha(Path(__file__).read_bytes())==sha(source),'running cost controller differs from archived source')
    fd=os.open(r/'financial.lock',os.O_RDWR|os.O_CREAT|os.O_NOFOLLOW,0o600)
    fcntl.flock(fd,fcntl.LOCK_EX|fcntl.LOCK_NB)
    results=[]
    try:
        inputs=r/'pilot-double-cost-inputs-v1';inputs.mkdir(mode=0o700)
        for mode in MODES:
            age=(datetime.now(timezone.utc)-datetime.fromisoformat(spec['observation']['recorded_at'])).total_seconds()
            require(spec['observation']['goal']['status']=='active' and 0<=age<=900
                and 24429+spec['observation']['goal']['timeUsedSeconds']+age+120<72000,'cost-check budget or clock')
            base='exploratory-pilot-'+mode+'-hold-v1'
            base_ref=next(v['terminal'] for v in summary['cases'] if v['case_id']==base)
            terminal=json.loads(read(base_ref))
            require(terminal['status']=='complete_exploratory_arithmetic','baseline is incomplete')
            reservation=json.loads((r/'reservations'/(base+'.json')).read_bytes())
            packet=json.loads(read(reservation['packet']));build=json.loads(read(reservation['build']))
            for artifact in build['source_artifacts']:read(artifact)
            read(build['binary_archive'],64<<20)
            checked(build['binary']['file'],build['binary']['sha256'],64<<20,lexar=False,retain=False)
            require(packet['modeled_costs']['scale']==1,'baseline already changed costs')
            packet['modeled_costs']['scale']=2
            packet_ref=write(inputs/(mode+'-hold.json'),packet)
            existing=list((r/'reservations').glob('*.json'))
            require(len(existing)<399 and all((r/'dispatch'/p.stem/'terminal.json').is_file() for p in existing),
                'case budget or unresolved reservation')
            case_id='exploratory-pilot-'+mode+'-hold-double-cost-v1'
            out=r/'dispatch'/case_id;out.mkdir(mode=0o700)
            write(r/'reservations'/(case_id+'.json'),dict(case_id=case_id,purpose='exploratory_cost_sensitivity',
                baseline=base_ref,packet=packet_ref,build=reservation['build'],controller=spec['controller'],
                combined_charged_cases=2+len(existing),max_runtime_seconds=120))
            resources=None;verification=None;error=None;report=None
            try:
                directory=r/'cases'/case_id
                resources=execute([build['binary']['file'],'-experiment','-packet',packet_ref['file'],
                    '-packet-sha256',packet_ref['sha256'],'-out',str(directory)],120,out/'stdout',out/'stderr')
                require(resources['exit_code']==0 and not resources['stopped_reason'],'cost check incomplete')
                j=directory/'journal.jsonl';z=directory/'result.json';d=directory/'diagnostics.jsonl'
                report=analyze(packet,_parse_journal(j.read_bytes()),_loads(z.read_bytes()),
                    [_loads(x) for x in d.read_bytes().splitlines()],str(j),sha(j.read_bytes()),str(z),sha(z.read_bytes()))
                prior=json.loads(read(terminal['verification']))
                comparison=dict(baseline_profit_cents=prior['metrics']['net_profit_cents'],
                    doubled_cost_profit_cents=report['metrics']['net_profit_cents'],
                    profit_change_cents=report['metrics']['net_profit_cents']-prior['metrics']['net_profit_cents'],
                    baseline_fees_cents=prior['metrics']['financial_components']['fees_cents'],
                    doubled_fees_cents=report['metrics']['financial_components']['fees_cents'],
                    near_close_drawdown=report['metrics']['maximum_near_close_drawdown']['fraction'])
                verification=write(out/'verification.json',dict(report,comparison=comparison))
            except Exception as e:
                report=None;error=type(e).__name__+':'+str(e)[:500]
            result=dict(case_id=case_id,status='complete_exploratory_arithmetic' if report else 'failed',
                resources=resources,error=error,verification=verification if report else None,
                comparison=comparison if report else None,contract_and_action_history_qualified=False,
                strategy_selection_qualified=False)
            ref=write(out/'terminal.json',result);results.append(dict(terminal=ref,**result))
            print(json.dumps(result),flush=True)
            if not report:break
    finally:os.close(fd)
    ref=write(r/'exploratory-pilot-double-cost-v1-summary.json',dict(cases=results,
        all_three_complete=len(results)==3 and all(v['status']=='complete_exploratory_arithmetic' for v in results),
        winner_selected=False))
    print(json.dumps(dict(summary=ref)),flush=True)


def stock_benchmark(spec):
    """Same initial lots, no calls: a descriptive exact-money pilot comparator."""
    resource.setrlimit(resource.RLIMIT_AS,(4<<30,4<<30))
    r=root('rd1-261017-0748')
    def read(ref):return checked(ref['file'],ref['sha256'],32<<20,allow_empty=True)
    require(sha(Path(__file__).read_bytes())==sha(read(spec['controller'])),'benchmark controller differs')
    summary=json.loads(read(spec['pilot_summary']))
    ref=next(v['terminal'] for v in summary['cases'] if v['case_id']=='exploratory-pilot-aggressive-hold-v1')
    terminal=json.loads(read(ref));require(terminal['status']=='complete_exploratory_arithmetic','incomplete baseline')
    reservation=json.loads((r/'reservations'/(terminal['case_id']+'.json')).read_bytes())
    packet=json.loads(read(reservation['packet']));data=packet['sampled_data']
    require(packet['contract_assumption']==ASSUMPTION and packet['modeled_costs']['scale']==1,'pilot model changed')
    sessions,_,_=session_calendar(data['exchange_days']);first=data['first_session']
    execution=sessions[first][1]-timedelta(minutes=30)
    samples={at(v['at']):v for v in data['observations']};entry=samples[execution]['stock_books']
    symbols=data['opening_policies'][0]['symbols'];costs={s:integer(entry[s]['ask_cents'],1) for s in symbols}
    require(len(symbols)==2 and set(symbols)=={'AAPL','MSFT'} and all(100*p+100<=2_666_666 for p in costs.values()),
        'benchmark initial lots differ or exceed capacity')
    fees=200;debit=integer(sum(100*p for p in costs.values())+fees)
    require(debit<=SEED-1_680_944,'benchmark reserve exceeded')
    fd=os.open(r/'financial.lock',os.O_RDWR|os.O_CREAT|os.O_NOFOLLOW,0o600);fcntl.flock(fd,fcntl.LOCK_EX|fcntl.LOCK_NB)
    try:
        age=(datetime.now(timezone.utc)-datetime.fromisoformat(spec['observation']['recorded_at'])).total_seconds()
        require(spec['observation']['goal']['status']=='active' and 0<=age<=900
            and 24429+spec['observation']['goal']['timeUsedSeconds']+age+120<72000,'benchmark budget or clock')
        existing=list((r/'reservations').glob('*.json'))
        require(len(existing)<399 and all((r/'dispatch'/p.stem/'terminal.json').is_file() for p in existing),
            'case budget or unresolved reservation')
        case_id='exploratory-pilot-stock-hold-v1';out=r/'dispatch'/case_id;out.mkdir(mode=0o700)
        write(r/'reservations'/(case_id+'.json'),dict(case_id=case_id,purpose='exploratory_stock_control',baseline=ref,
            packet=reservation['packet'],controller=spec['controller'],combined_charged_cases=2+len(existing),
            initial_lots={s:100 for s in symbols},modeled_costs=packet['modeled_costs']))
        began=time.monotonic();report=None;error=None
        try:
            points=[];rows=[]
            for day,(opening,close) in sessions.items():
                if not first<=day<=data['last_session']:continue
                for label,minutes in (('signal',31),('execution',30),('NAV',1)):
                    clock=close-timedelta(minutes=minutes);obs=samples[clock];owned=clock>=execution
                    accrued=paid=market=gain=0
                    if owned:
                        for symbol in symbols:
                            b=obs['stock_books'][symbol]
                            bid=integer(b['bid_cents'],1);ask=integer(b['ask_cents'],bid)
                            market=integer(market+100*bid);gain+=100*(bid-costs[symbol])
                        for dividend in data['dividends']:
                            require(dividend['symbol'] in symbols and at(dividend['known_at'])<=at(dividend['ex_at']),
                                'benchmark dividend identity or announcement clock')
                            if execution<at(dividend['ex_at'])<=clock:
                                accrued+=integer(dividend['amount_for_100_shares_cents'])
                                if at(dividend['pay_at'])<=clock:paid+=dividend['amount_for_100_shares_cents']
                    cash=integer(SEED-debit+paid if owned else SEED)
                    receivable=integer(accrued-paid);nav=integer(cash+receivable+market)
                    require(nav==SEED+gain+accrued-(fees if owned else 0),'independent stock PnL identity failed')
                    point=dict(ref=label+'-'+day,at=clock.isoformat(),nav_cents=nav);points.append(point)
                    rows.append(dict(point,cash_cents=cash,receivable_cents=receivable,stock_value_cents=market,
                        source_packet_sha256=obs['source_packet_sha256']))
            metrics=series_metrics(points,data['exchange_days'],first,data['last_session'])
            require(metrics['sessions']==65 and len(points)==195 and time.monotonic()-began<60,'benchmark scope/runtime')
            prior=json.loads(read(terminal['verification']))
            report=dict(schema='AutoWheelSimpleExploratoryStockBenchmarkV1',metrics=metrics,
                initial_purchase_asks_cents=costs,shares_per_symbol=100,fees_cents=fees,
                dividends_cents=accrued,final_cash_cents=cash,final_receivable_cents=receivable,
                all_marks_reconciled_by_cash_inventory_and_independent_capital_gain_identity=True,
                aggressive_hold_profit_cents=prior['metrics']['net_profit_cents'],
                aggressive_minus_stock_hold_profit_cents=prior['metrics']['net_profit_cents']-metrics['net_profit_cents'],
                stock_action_history_qualified=False,strategy_selection_qualified=False,
                fees_are_modeled=True,interest_cents=0,final_liquidation_fee_charged=False)
            write(out/'marks.json',rows);verification=write(out/'verification.json',report)
        except Exception as e:report=None;error=type(e).__name__+':'+str(e)[:500]
        terminal_ref=write(out/'terminal.json',dict(case_id=case_id,status='complete_exploratory_stock_arithmetic' if report else 'failed',
            error=error,verification=verification if report else None,elapsed_seconds=time.monotonic()-began))
        print(json.dumps(dict(terminal=terminal_ref,error=error,profit_cents=report['metrics']['net_profit_cents'] if report else None,
            aggressive_minus_stock_hold_profit_cents=report['aggressive_minus_stock_hold_profit_cents'] if report else None,
            near_close_drawdown=report['metrics']['maximum_near_close_drawdown']['fraction'] if report else None)))
    finally:os.close(fd)


if __name__=='__main__':
    spec=json.load(sys.stdin);operation=spec.get('operation')
    if operation=='double_cost_hold':cost_checks(spec)
    elif operation=='stock_hold_benchmark':stock_benchmark(spec)
    else:run(spec)
