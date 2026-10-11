#!/usr/bin/env python3
"""Normalize the complete statistics data union, reusing 199 attested outputs.

No credentials, HTTP, live journal, financial writes, trading, or invented
economic reference dates. Keep canonical542 and disjoint2 identities distinct;
the original unknown parent outcome remains unresolved. Candidates remain
provisional until the complete source and replay qualification finishes.
"""
import ast
from collections import Counter
from datetime import datetime, time, timedelta, timezone
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import stat
import subprocess
import time as clock
from zoneinfo import ZoneInfo

BASE=Path('/home/yuduo/Projects/AutoWheel-research')
ROOT=BASE/'source-afa3075'
D=ROOT/'research/simulation/2026-10-05'
CANON=BASE/'cycle51-statistics-next-scope-audit-20261007/summary.json'
CHILD=D/'cycle52-disjoint-statistics-native-OI-actual.json'
CACHE=BASE/'cycle48-OI-normalizer-equivalence-20261007/summary.json'
BINARY=BASE/'cycle46-OI-tools-20261007/OI-candidate-normalizer'
OUTPUT=BASE/'cycle53-full-statistics-OI-normalization-20261007'
MANIFEST=BASE/'cycle24-selective-acquisition-queries-20261006/manifest.json'
PARENT_SHA='dd72d518f59fb800d0291b01920d159e09a41950f8523785f283093715ec70a2'
RESERVE=16<<30


def sha(path):
    with path.open('rb') as f:return hashlib.file_digest(f,'sha256').hexdigest()


def main():
    if OUTPUT.exists() or BASE.resolve()!=BASE or stat.S_IMODE(BASE.stat().st_mode)!=0o700:
        raise ValueError('fresh private normalization namespace required')
    q=json.loads((D/'databento-qualification.json').read_bytes())
    known={r['path']:r['sha256'] for r in q['source_files']}
    sources={}
    def bind(path,expected=None):
        path=Path(path);path=path if path.is_absolute() else ROOT/path
        if not path.is_file() or (path!=BASE/'dbn-env-0.71.0/bin/python' and (path.is_symlink() or path.resolve()!=path)):
            raise ValueError('unsafe retained source')
        digest=sha(path)
        if expected is not None and digest!=expected or str(path) in sources and sources[str(path)]!=digest:
            raise ValueError('immutable source binding differs')
        sources[str(path)]=digest
        return digest
    def accepted(path):
        path=Path(path)
        relative=str(path.relative_to(ROOT)) if path.is_relative_to(ROOT) else str(path)
        expected=known.get(relative,known.get(str(path)))
        if expected is None:raise ValueError('unaccepted prerequisite source')
        return bind(path,expected)
    helper=ROOT/'scripts/audit-OI-replay-normalizer.py'
    bind(helper,'bb2c5d8a121d0a41cb3da2e44ff52131e6f5a1b9df5940ef0c6ecd63de71c46d')
    spec=importlib.util.spec_from_file_location('sealed_oi_normalizer_audit',helper)
    h=importlib.util.module_from_spec(spec);spec.loader.exec_module(h)
    for path in (CANON,CHILD,CACHE,BINARY,MANIFEST):accepted(path)
    info=BINARY.stat()
    if info.st_uid!=os.getuid() or stat.S_IMODE(info.st_mode)!=0o700 or sources[str(BINARY)]!='9314581a3458365601aac8c6d6670d7cc1c5c28b0ede2a3ffc446c4f33123c92':
        raise ValueError('pinned private Go normalizer differs')
    for path in ('internal/replaynormalize/oi.go','internal/replaynormalize/stock.go','internal/causaloi/oi.go',
                 'internal/simulation/types.go','research/simulation/2026-10-05/cycle46-OI-normalizer/main.go'):
        accepted(ROOT/path)
    bind(Path(__file__))
    canon,child,cache=map(h.read,(CANON,CHILD,CACHE))
    if (canon['schema']!='AutoWheelAllAcquiredStatisticsOIComponentV1' or canon['coverage']['required_partitions']!=543
            or canon['coverage']['audited_partitions']!=542 or canon['coverage']['expected_session_dates']!=126
            or canon['coverage']['missing_query_files']!=['2026-09-18-statistics-002.json']
            or child['schema']!='AutoWheelDisjointStatisticsNativeOIComponentV1' or child['children_audited']!=2
            or child['observations']!=150000 or not child['exact_disjoint_parent_data_scope_covered']
            or not canon['go_python_views_equal'] or not child['go_python_views_equal']
            or any(v['fields_per_record']!=17 for v in (canon,child))
            or any(v['source_gate_passed'] is not False or v['historical_performance_available'] is not False for v in (canon,child))):
        raise ValueError('complete attested statistics union differs')
    if (cache['schema']!='AutoWheelOINormalizerEquivalenceV1' or cache['coverage']['audited_partitions']!=199
            or not cache['independently_compared'] or cache['clock_policy']!=h.POLICY
            or cache['economic_reference_dates_invented']!=0 or cache['source_gate_passed'] is not False
            or cache['historical_performance_available'] is not False):
        raise ValueError('accepted normalizer cache differs')
    configs,by_hash={},{}
    for summary in (canon,child,cache):
        for item in summary['source_files']:
            path=Path(item['path']);bind(path,item['sha256'])
            if path not in by_hash.setdefault(item['sha256'],[]):by_hash[item['sha256']].append(path)
            if path.name.endswith('.scope.json'):
                config=h.read(path)
                key=(config['utc_start'],tuple(sorted(b['RawSymbol'] for b in config['bindings'])))
                if key in configs and configs[key][1]!=config:raise ValueError('conflicting normalization scopes')
                configs.setdefault(key,(path,config))
    planned={r['sha256']:r for r in h.read(MANIFEST)['queries'] if r['schema']=='statistics'}
    if len(planned)!=543:raise ValueError('frozen canonical plan differs')
    overlay=BASE/'cycle51-statistics-disjoint-overlay-plan-20261007'
    parent_path=MANIFEST.parent/'2026-09-18-statistics-002.json';bind(parent_path,PARENT_SHA);parent=h.read(parent_path)
    children=[]
    for i in range(2):
        path=overlay/('2026-09-18-statistics-overlay-%03d.json'%i);accepted(path)
        query=h.read(path);children.append(query)
        if (len(query['symbols'])!=1000 or query['symbols']!=parent['symbols'][i*1000:(i+1)*1000]
                or any(query[k]!=parent[k] for k in ('dataset','schema','stype_in','start','end'))):
            raise ValueError('exact parent data union differs')
        planned[sources[str(path)]]=dict(filename=path.name,date='2026-09-18',query_path=str(path),overlay_child=True)
    if set(children[0]['symbols'])&set(children[1]['symbols']):raise ValueError('replacement data overlaps')
    entries=[dict(r,overlay_child=False) for r in canon['results']]+[dict(r,date='2026-09-18',overlay_child=True) for r in child['results']]
    if len(entries)!=544 or len({r['query_sha256'] for r in entries})!=544 or sum(r['observations'] for r in entries)!=71625225:
        raise ValueError('full logical statistics dimensions differ')
    cached={r['query_sha256']:r for r in cache['results']}
    if len(cached)!=199 or not set(cached)<={r['query_sha256'] for r in entries}:
        raise ValueError('prior normalized scope shrank')
    if len(configs)!=544:raise ValueError('complete config source roster differs')
    OUTPUT.mkdir(mode=0o700)
    plan=dict(schema='AutoWheelFullOINormalizationPlanV1',canonical_partitions=542,disjoint_children=2,
              physical_partitions=544,logical_partitions=543,sessions=126,contract_days=955003,
              observations=71625225,reuse_partitions=199,fresh_partitions=345,clock_policy=h.POLICY,
              runner_sha256=sha(Path(__file__)),financial_requests=0,journal_writes=0,source_gate_passed=False)
    h.save(OUTPUT/'plan.json',plan)
    print(json.dumps(plan),flush=True)
    results,counts,reasons=[],Counter(),Counter()
    for entry in sorted(entries,key=lambda r:(r['date'],r['query_sha256'])):
        item=planned[entry['query_sha256']]
        query_path=Path(item.get('query_path',str(MANIFEST.parent/item['filename'])))
        bind(query_path,entry['query_sha256']);query=h.read(query_path)
        start=int(datetime.strptime(entry['date'],'%Y-%m-%d').replace(tzinfo=timezone.utc).timestamp())*1_000_000_000
        config_path,config=configs[(start,tuple(sorted(query['symbols'])))]
        origin=datetime.combine(datetime.strptime(entry['date'],'%Y-%m-%d').date(),time(),ZoneInfo('America/New_York'))
        cuts=[int((origin+timedelta(minutes=n)).timestamp())*1_000_000_000 for n in [*range(575,945,5),960]]
        if (config['schema']!='AutoWheelNativeOIScopeV1' or config['dataset']!='OPRA.PILLAR' or config['cuts']!=cuts
                or config['expected_records']!=entry['records'] or entry['observations']!=len(query['symbols'])*75):
            raise ValueError('closed candidate scope differs')
        vectors=[p for p in by_hash[config['vector_sha256']] if p.name.endswith('.vectors.jsonl.gz')]
        if len(vectors)!=1:raise ValueError('native vector source ambiguous')
        vector=vectors[0]
        reused=entry['query_sha256'] in cached
        if reused:
            old=cached[entry['query_sha256']];out=Path(old['private_directory']);report=h.read(out/'report.json')
            if old['records']!=entry['records'] or old['observations']!=entry['observations'] or old['status_counts']!=entry['status_counts']:
                raise ValueError('cache data scope/count differs')
            compared={k:old[k] for k in ('records','OI_records','cuts','observations','status_counts','normalization_counts','candidate_jsonl_sha256')}
            if report['config_sha256']!=sources[str(config_path)]:raise ValueError('cached original config differs')
        else:
            # Quote recovery also uses staging space. Pause this offline reader
            # at sixteen GiB free rather than compete with its six-GiB reserve.
            deadline=clock.monotonic()+21600
            while os.statvfs(BASE).f_bavail*os.statvfs(BASE).f_frsize<RESERVE:
                if clock.monotonic()>deadline:raise ValueError('offline normalization capacity deadline')
                clock.sleep(5)
            out=OUTPUT/item['filename'].removesuffix('.json')
            fd=os.open(OUTPUT/(item['filename']+'.private.log'),os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
            with os.fdopen(fd,'wb') as log:
                if subprocess.run([str(BINARY),'--config',str(config_path),'--vectors',str(vector),'--output-directory',str(out)],stdout=log,stderr=log).returncode:
                    raise ValueError('private Go normalization stopped')
            report=h.read(out/'report.json')
            compared=h.compare(vector,out/'OI-candidates.jsonl.gz',config,entry['records'])
        candidates=out/'OI-candidates.jsonl.gz';bind(out/'report.json')
        if (report['schema']!='AutoWheelNativeOINormalizationV1' or report['clock_policy']!=h.POLICY
                or report['summary']!=dict(Records=entry['records'],OpenInterestRecords=compared['OI_records'],Snapshots=75,Observations=entry['observations'])
                or compared['cuts']!=75 or compared['records']!=entry['records'] or compared['observations']!=entry['observations']
                or report['status_counts']!=compared['status_counts'] or report['status_counts']!=entry['status_counts']
                or report['normalization_counts']!=compared['normalization_counts'] or report['views_jsonl_sha256']!=compared['candidate_jsonl_sha256']
                or report['views_sha256']!=bind(candidates) or report['config_sha256']!=sources[str(config_path)]
                or report['vector_sha256']!=config['vector_sha256'] or report['vector_jsonl_sha256']!=config['vector_jsonl_sha256']
                or report['financial_requests']!=0 or any(report[k] is not False for k in ('source_gate_passed','historical_performance_available','replay_frames_produced'))):
            raise ValueError('independent full candidate or policy comparison differs')
        results.append(dict(date=entry['date'],query_sha256=entry['query_sha256'],proof_sha256=entry['proof_sha256'],
                            private_directory=str(out),overlay_child=entry['overlay_child'],cache_reused=reused,**compared))
        counts.update(compared['status_counts']);reasons.update(compared['normalization_counts'])
        print(json.dumps(dict(partitions_normalized=len(results),physical_partitions=544,
                             reused=sum(r['cache_reused'] for r in results),observations_compared=sum(r['observations'] for r in results))),flush=True)
    if (sum(r['cache_reused'] for r in results)!=199 or sum(r['records'] for r in results)!=80249087
            or sum(r['observations'] for r in results)!=71625225 or len({r['date'] for r in results})!=126):
        raise ValueError('full normalized study dimensions differ')
    for path,digest in sources.items():
        if sha(Path(path))!=digest:raise ValueError('source changed during full normalization')
    report=dict(plan,schema='AutoWheelFullOINormalizerEquivalenceV1',complete_data_scope=True,
                original_native_records=80249087,contract_cut_observations=71625225,independently_compared=True,
                status_counts=dict(counts),normalization_counts=dict(reasons),economic_reference_dates_invented=0,
                original_parent_financial_status='reserved_unknown_untouched',source_gate_passed=False,
                full_source_qualification=False,replay_frames_produced=False,historical_performance_available=False,
                licensed_outputs_retained_private=True,results=results,
                source_files=[dict(path=p,sha256=h) for p,h in sorted(sources.items())])
    h.save(OUTPUT/'summary.json',report)


if __name__=='__main__':
    try:main()
    except Exception as exc:
        literals={n.args[0].value for n in ast.walk(ast.parse(Path(__file__).read_text()))
                  if isinstance(n,ast.Call) and isinstance(n.func,ast.Name) and n.func.id=='ValueError'
                  and len(n.args)==1 and isinstance(n.args[0],ast.Constant) and isinstance(n.args[0].value,str)}
        print(json.dumps({'stopped':True,'error_type':type(exc).__name__,'fixed_error':str(exc) if str(exc) in literals else 'unclassified',
                          'financial_requests':0,'journal_writes':0,'private_failure_evidence_retained':True}),flush=True)
        raise SystemExit(1)
