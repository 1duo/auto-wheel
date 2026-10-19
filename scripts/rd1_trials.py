"""RD1-only immutable case reservations. No process dispatch or broker access.

Reservation is a charged case even if the caller crashes before running it.
An unfinished reservation fences admission until explicitly terminalized; it is
never inferred successful, resumed or automatically retried by this module.
"""
from contextlib import contextmanager
import fcntl
import json
import os
import re
import stat

from rd1_storage import PROTOCOL_SHA256, budget, canonical, require, sha, write

GENESIS_SHA256 = '5eb0634b73a51dbe581bc32bee5b079abbe0c697b3a55b5b06b6c8369447625e'
PROFILE_IDS = frozenset('rd1-'+m+'-'+e for m in ('conservative','balanced','aggressive')
                        for e in ('hold','tp50','tp75'))
# These identify the three predeclared annual-selection account procedures,
# not additional fitted designs. The immutable genesis keeps its nine profiles.
FORWARD_IDS = frozenset('rd1-'+m+'-selected' for m in ('conservative','balanced','aggressive'))
PURPOSES = frozenset(('pilot','training','forward','final_fit','control','sensitivity','reproduction','failure_fixture'))
TERMINALS = frozenset(('complete','incomplete','insufficient','rejected','failed'))


def digest(value):
    require(isinstance(value,str) and re.fullmatch('[0-9a-f]{64}',value),'invalid case hash')
    return value


def case_identity(case):
    require(set(case)=={'case_id','purpose','profile_id','start','end','input_sha256',
                       'implementation_sha256','model_sha256','parent_state_sha256','sensitivity'},
            'case identity fields differ')
    profiles=PROFILE_IDS | {'cash_zero_interest','stock_buy_hold'}
    if case['purpose'] in ('forward','sensitivity','reproduction'):profiles |= FORWARD_IDS
    require(case['purpose'] in PURPOSES and case['profile_id'] in profiles,
            'unknown RD1 case purpose/profile')
    require(re.fullmatch('[a-z0-9][a-z0-9_-]{0,127}',case['case_id']) is not None,
            'invalid case ID')
    from datetime import date
    for k in ('start','end'):
        require(date.fromisoformat(case[k]).isoformat()==case[k], 'invalid case date')
    require('2021-01-01'<=case['start']<=case['end']<='2026-10-09','case outside frozen RD1 history')
    for k in ('input_sha256','implementation_sha256','model_sha256','parent_state_sha256'):
        digest(case[k])
    require(case['sensitivity'] in ('primary','execution_5m','execution_next_session',
            'extra_half_spread_cost','daily_ITM_assignment','pin_exercise','pin_no_exercise',
            'settlement_plus_one_day','missing_held_mark_fixture'), 'unknown sensitivity')
    return sha(canonical(case))


def fold(records):
    """Verify every link and transition; pure so the verifier can reuse framing.

    Financial correctness must be checked independently from these case counts.
    """
    require(1<=len(records)<=801,'case ledger size envelope')
    first=records[0]
    require(sha(canonical(first))==GENESIS_SHA256,'case genesis differs')
    require(first['sequence']==0 and first['charged_cases']==0 and first['live_replays']==0
            and first['max_charged_cases']==400 and first['protocol_sha256']==PROTOCOL_SHA256,
            'invalid RD1 genesis')
    previous=sha(canonical(first));charged=0;cases={};live=None
    for sequence,row in enumerate(records[1:],1):
        require(row['sequence']==sequence and row['previous_sha256']==previous
                and row['protocol_sha256']==PROTOCOL_SHA256,'case ledger link differs')
        require(row['kind'] in ('reserved','terminal'),'unknown case ledger transition')
        identity=row['case'];key=case_identity(identity)
        require(key==row['identity_sha256'],'case identity differs')
        case_id=identity['case_id']
        if row['kind']=='reserved':
            require(live is None and case_id not in cases and charged<400,'duplicate/live/budget case reservation')
            require(type(row['max_runtime_seconds']) is int and 0<row['max_runtime_seconds']<=900,
                    'case runtime envelope')
            charged+=1;live=case_id;cases[case_id]=dict(identity=identity,status='reserved')
        else:
            require(live==case_id and cases[case_id]['identity']==identity,'terminal has no exact live reservation')
            require(row['status'] in TERMINALS,'unknown terminal status')
            digest(row['result_sha256'])
            require(isinstance(row['reason'],str) and 0<len(row['reason'])<=2000,'terminal reason missing')
            cases[case_id]['status']=row['status'];live=None
        require(row['charged_cases']==charged and row['live_replays']==int(live is not None),
                'case counts differ')
        previous=sha(canonical(row))
    return dict(sequence=len(records)-1,previous_sha256=previous,charged_cases=charged,
                live_case=live,cases=cases)


@contextmanager
def locked(run):
    fd=os.open(run/'case-ledger.lock',os.O_RDWR|os.O_CREAT|os.O_NOFOLLOW,0o600)
    try:
        info=os.fstat(fd)
        require(stat.S_ISREG(info.st_mode) and info.st_uid==os.getuid()
                and stat.S_IMODE(info.st_mode)&0o077==0,'case lock lost privacy')
        fcntl.flock(fd,fcntl.LOCK_EX)
        yield
    finally:
        os.close(fd)


def read(run):
    from rd1_storage import PARENT, PHYSICAL
    directory=run/'case-ledger';records=[]
    files=sorted(directory.iterdir())
    require(all(re.fullmatch('[0-9]{6}\\.json',p.name) for p in files),'unrecognized ledger entry')
    for number,path in enumerate(files):
        info=path.lstat()
        require(path.name==f'{number:06}.json' and stat.S_ISREG(info.st_mode)
                and not path.is_symlink() and 0<info.st_size<=32768,'unsafe/missing ledger entry')
        raw=path.read_bytes();value=json.loads(raw)
        require(raw==canonical(value) and sha(raw)==sha((PHYSICAL/path.relative_to(PARENT)).read_bytes()),
                'noncanonical/physical ledger differs')
        records.append(value)
    return records,fold(records)


def reserve(run,observation,case,max_runtime_seconds):
    """Call immediately before a bounded replay. Reserve success is not a fill."""
    identity=case_identity(case)
    with locked(run):
        usage=budget(run,observation,max_runtime_seconds)
        records,state=read(run)
        row=dict(schema='AutoWheelRD1CaseLedgerEventV1',sequence=state['sequence']+1,
                 previous_sha256=state['previous_sha256'],protocol_sha256=PROTOCOL_SHA256,
                 kind='reserved',case=case,identity_sha256=identity,
                 charged_cases=state['charged_cases']+1,live_replays=1,
                 max_runtime_seconds=max_runtime_seconds,admission_observation=observation,
                 active_seconds_used_at_admission=usage['active_seconds_used'])
        fold(records+[row])
        return write(run/'case-ledger'/f"{row['sequence']:06}.json",row)


def terminal(run,case_id,status,result_sha256,reason):
    """Record an observed result or explicit failure; no automatic relaunch."""
    with locked(run):
        records,state=read(run)
        require(state['live_case']==case_id,'terminal does not own active reservation')
        case=state['cases'][case_id]['identity']
        row=dict(schema='AutoWheelRD1CaseLedgerEventV1',sequence=state['sequence']+1,
                 previous_sha256=state['previous_sha256'],protocol_sha256=PROTOCOL_SHA256,
                 kind='terminal',case=case,identity_sha256=case_identity(case),
                 charged_cases=state['charged_cases'],live_replays=0,
                 status=status,result_sha256=result_sha256,reason=reason)
        fold(records+[row])
        return write(run/'case-ledger'/f"{row['sequence']:06}.json",row)
