"""Bind one acquired window to its exact frozen daily partition and cut."""
import importlib.util,json
from pathlib import Path
from batch_acquisition_proof import file_hash,read_json
from research_batch_history import DecisionPlan
BASE=Path('/home/yuduo/Projects/AutoWheel-research')
PLAN=BASE/'cycle91-decision-plan-reader-checks-20261008/decision-plan/decision-plan.json.gz'
DAY=86_400_000_000_000
def daily(root):
    p=root/'scripts/frozen-study-option-cuts.py'
    if p.is_symlink() or file_hash(p)!='76162e60e50f7ce6a099f9731e964787073bd5496aacd978f585d79ed5ab48fd':
        raise ValueError('accepted daily interface differs')
    spec=importlib.util.spec_from_file_location('closed_window_daily_cut_binding',p)
    module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module);return module
def scope(query,root):
    plan=DecisionPlan(PLAN);row=plan.window(query);part=plan.value['partitions'][row[0]]
    path=Path(part['query_path'])
    if path.is_symlink() or file_hash(path)!=part['query_sha256']:raise ValueError('accepted original partition differs')
    full=read_json(path)
    if any(query[k]!=full[k] for k in ('dataset','schema','stype_in','symbols')):raise ValueError('full frozen contract roster required')
    interface=daily(root);document=interface.make(full,root)
    day_start=row[3]//DAY*DAY
    a,_=interface.frozen_cuts(document,full,day_start,day_start+DAY,root)
    b,_=interface.verify_frozen_cuts(document,full,day_start,day_start+DAY,root)
    if a!=b or type(row[1]) is not int or not 0<=row[1]<75 or a[row[1]]!=row[2]:raise ValueError('frozen closed-cut index differs')
    plan.unchanged();return row,document,day_start
def make(query,root):
    row,full,day_start=scope(query,root)
    return dict(schema='AutoWheelFrozenClosedWindowOptionCutsV1',query_start=query['start'],query_end=query['end'],
        cuts_ns=[row[2]],max_age_nanos=30_000_000_000,utc_start_ns=day_start,
        source_start_ns=row[3],source_end_ns=row[4],window_operation_id=row[6],
        decision_cuts=int(row[1]<74),closing_valuation_cuts=int(row[1]==74),full_day_coverage=False,
        source_files=full['source_files']+[dict(path=str(PLAN),sha256=file_hash(PLAN)),
            dict(path='scripts/frozen-closed-window-option-cuts-cycle187.py',sha256=file_hash(Path(__file__)))],
        full_source_qualification=False)
def frozen_cuts(document,query,start,end,root):
    expected=make(query,root)
    canonical=lambda value:json.dumps(value,sort_keys=True,separators=(',',':'),allow_nan=False)
    if canonical(document)!=canonical(expected) or type(start) is not int or type(end) is not int or start!=expected['source_start_ns'] or end!=expected['source_end_ns']:
        raise ValueError('exact frozen one-cut window required')
    return expected['cuts_ns'],[r['path'] for r in expected['source_files']]
def verify_frozen_cuts(document,query,start,end,root):
    cuts,files=frozen_cuts(document,query,start,end,root)
    if len(cuts)!=1 or start!=cuts[0]-30_000_000_000 or end!=cuts[0]+1:raise ValueError('closed window interval differs')
    return cuts,files
