"""A charged, expected-incomplete missing-held-mark control over a real pilot.

The unchanged admitted packet and baseline outputs are freshly verified before
reservation. A deterministic wrapper hides one book; this module never admits
data, assigns fees, mutates originals or publishes full-period performance.
"""
from pathlib import Path

from rd1_financial_verify import _loads,_parse_journal
from rd1_forward import completed_account
from rd1_metrics import at,session_calendar
from rd1_storage import require,sha
from rd1_trials import case_identity

FIXTURE='missing_held_mark_fixture'


def plan(case,original,packet_ref,admission_ref,build_ref,journal_raw,diagnostics_raw):
    """Pure framing; actual preflight must establish the completed ledger case."""
    case_identity(case);before=original['case'];packet=original['packet']
    require(case['purpose']=='failure_fixture' and case['sensitivity']==FIXTURE
            and before['purpose']=='pilot' and before['sensitivity']=='primary'
            and before['start']=='2021-01-04' and before['end']=='2021-04-07'
            and before['parent_state_sha256']=='0'*64 and case['case_id']!=before['case_id']
            and all(case[k]==before[k] for k in before if k not in ('purpose','case_id','sensitivity')),
            'failure fixture changes the original pilot account or history')
    require(all(value==original['spec'][name] for name,value in
                (('packet',packet_ref),('admission',admission_ref),('build',build_ref))),
            'failure fixture requires exact original packet, admission and build references')
    sessions,_,_=session_calendar(packet['exchange_days'])
    require(sum(before['start']<=day<=before['end'] for day in sessions)==65
            and packet['opening_policies'] and all(len(row['symbols'])==2
                and set(row['symbols'])=={'AAPL','MSFT'} for row in packet['opening_policies']),
            'failure baseline is not the complete declared two-symbol pilot')
    rows=_parse_journal(journal_raw);lines=journal_raw.splitlines(keepends=True)
    require(len(rows)==len(lines) and journal_raw.endswith(b'\n'), 'baseline journal framing differs')
    chosen=next((i for i,row in enumerate(rows) if row['effect']['action']=='mark'
                 and row['mark']['option_books']),None)
    require(chosen is not None and chosen>0, 'completed pilot has no held-option mark to remove')
    target=rows[chosen];clock=at(target['effect']['at']);contract_id=min(target['mark']['option_books'])
    obs=[row for row in packet['observations'] if at(row['at'])==clock]
    require(len(obs)==1 and target['mark']['option_books'][contract_id]==obs[0]['option_books'][contract_id],
            'fault target does not identify an actually observed held original book')
    prefix=b''.join(lines[:chosen]);positions={};near=[]
    for row in rows[:chosen]:
        e=row['effect'];action=e['action'];symbol=e.get('symbol')
        if action=='buy_stock':positions[symbol]=dict(shares=100,short=None)
        elif action=='sell_put':positions[symbol]=dict(shares=0,short=e['contract'])
        elif action=='sell_call':positions[symbol]['short']=e['contract']
        elif action in ('close_short','resolve'):
            p=positions[symbol];p['short']=None
            if action=='resolve' and e['outcome']=='assigned':p['shares']=100
            if action=='resolve' and e['outcome']=='called_away' or not p['shares']:del positions[symbol]
        elif action=='mark' and e['ref'].startswith('NAV-'):near.append(row)
    require(set(target['mark']['stock_books'])=={name for name,p in positions.items() if p['shares']}
            and set(target['mark']['option_books'])=={p['short']['id'] for p in positions.values() if p['short']},
            'fault target does not match the reconstructed prefix inventory')
    diagnostic_lines=diagnostics_raw.splitlines(keepends=True)
    diagnostic_prefix=b''.join(line for line in diagnostic_lines if at(_loads(line)['at'])<clock)
    require(diagnostics_raw.startswith(diagnostic_prefix), 'baseline diagnostics are not a chronological prefix')
    return dict(schema='AutoWheelRD1MissingHeldMarkPlanV1',baseline_pilot_receipt=original['reference'],
        baseline_case=before,fault_at=target['effect']['at'],fault_contract_id=contract_id,
        fault_mark_ref=target['effect']['ref'],expected_reason='RD1 held option mark absent',
        journal_prefix_sha256=sha(prefix),journal_prefix_bytes=len(prefix),journal_prefix_records=chosen,
        diagnostics_prefix_sha256=sha(diagnostic_prefix),diagnostics_prefix_bytes=len(diagnostic_prefix),
        expected_cash_cents=rows[chosen-1]['cash_after_cents'],expected_fees_cents=rows[chosen-1]['fees_after_cents'],
        expected_state_at=rows[chosen-1]['effect']['at'],expected_positions=positions,
        expected_sessions_marked=len(near),expected_last_near_close=near[-1] if near else None,
        unchanged_original_packet_sha256=packet_ref['sha256'],original_source_bytes_changed=False,
        historical_performance_generated=False,source_admission_performed=False)


def preflight(spec,run,read_ref,read_source):
    require('baseline_pilot_receipt' in spec, 'failure fixture lacks a completed admitted pilot')
    original=completed_account(spec['baseline_pilot_receipt'],run,read_ref,read_source,allowed_purposes=('pilot',))
    refs={Path(row['file']).name:row for row in original['artifacts']}
    return plan(spec['case'],original,spec['packet'],spec['admission'],spec['build'],
                read_ref(refs['journal.jsonl'],32<<20),read_ref(refs['diagnostics.jsonl'],32<<20))


def verify_stop(proof,resources,journal_raw,diagnostics_raw,result,financial):
    """Expected stop succeeds as a control; its financial path stays incomplete."""
    require(resources and resources['exit_code']==1 and not resources['stopped_reason'],
            'fixture process did not exit at the deliberate missing-mark error')
    require(result['status']=='incomplete' and result['sensitivity']==FIXTURE
            and result['reason']==proof['expected_reason'] and result.get('net_profit_cents') is None,
            'fixture did not retain the declared incomplete stop without full-period profit')
    require(sha(journal_raw)==proof['journal_prefix_sha256'] and len(journal_raw)==proof['journal_prefix_bytes']
            and sha(diagnostics_raw)==proof['diagnostics_prefix_sha256']
            and len(diagnostics_raw)==proof['diagnostics_prefix_bytes'],
            'fixture changed history or continued after the missing held mark')
    state=result['checkpoint_state']
    require(state['sequence']==proof['journal_prefix_records']
            and state['economic_cash_cents']==proof['expected_cash_cents']
            and state['fees_cents']==proof['expected_fees_cents']
            and at(state['at'])==at(proof['expected_state_at'])
            and result['sessions_marked']==proof['expected_sessions_marked'],
            'fixture changed the stopped prefix cash, sequence or observation clock')
    positions=state['positions'];require(set(positions)==set(proof['expected_positions']), 'fixture erased a held lifecycle')
    for symbol,p in proof['expected_positions'].items():
        actual=positions[symbol];short=actual.get('short')
        require(actual['shares']==p['shares'] and (short['contract'] if short else None)==p['short'],
                'fixture erased or changed held shares/option liability')
    near=proof['expected_last_near_close']
    require((result.get('last_near_close_NAV_cents')==near['mark']['NAV_cents']
             and at(result['last_near_close_at'])==at(near['effect']['at'])) if near else
            result.get('last_near_close_NAV_cents') is None,
            'fixture promoted an unobserved near-close valuation')
    require(financial['qualified'] is False and financial['metrics'] is None,
            'incomplete fixture was given full-period financial metrics')
    return dict(schema='AutoWheelRD1MissingHeldMarkVerificationV1',expectation_status='passed',
        account_path_status='incomplete',exact_pre_fault_history_preserved=True,
        held_liability_preserved=True,failed_mark_or_later_financial_effects_recorded=False,
        full_period_metrics=None,historical_performance_qualified=False,
        baseline_pilot_receipt=proof['baseline_pilot_receipt'])
