"""Forward account preflight; exact annual choices and charged prior case.

Selected-account IDs identify the already specified chronological procedure.
They do not add designs to the nine-profile training library. Source and fit
admission require separately retained, hash-bound evidence receipts.
"""
from pathlib import Path

from rd1_availability_verify import analyze as availability_analyze
from rd1_attempt_verify import analyze as attempt_analyze
from rd1_terminal_verify import analyze as terminal_analyze
from rd1_carry import input_prefix, journal_prefix
from rd1_effect_verify import analyze as effect_analyze
from rd1_financial_verify import _loads, verify
from rd1_metrics import SCENARIOS, analyze as metrics_analyze, session_calendar
from rd1_storage import PROTOCOL_SHA256, canonical, require, sha
from rd1_trials import case_identity, locked, read as read_ledger


def annual_policies(case,packet,admission,read_ref):
    parts=case['profile_id'].split('-')
    require(len(parts)==3 and parts[0]=='rd1' and parts[1] in ('conservative','balanced','aggressive')
            and parts[2] in ('selected','hold'), 'forward case is neither a selected account nor its hold baseline')
    mode,kind=parts[1:];end=int(case['end'][:4]);years=list(range(2023,end+1))
    require(end in (2023,2024,2025), 'forward account outside declared three years')
    rows=admission['annual_policy_evidence']
    require(type(rows) is list and [x['year'] for x in rows]==years
            and len(packet['opening_policies'])==len(years), 'missing or repeated annual policy evidence')
    sessions,first,last=session_calendar(packet['exchange_days'])
    require(str(first)<='2023-01-01' and str(last)>=f'{end}-12-31'
            and case['start']==min(x for x in sessions if x.startswith('2023-'))
            and case['end']==max(x for x in sessions if x.startswith(str(end)+'-')),
            'forward account was reseeded or omits declared annual sessions')
    refs=[]
    for year,row,policy in zip(years,rows,packet['opening_policies'],strict=True):
        selection=_loads(read_ref(row['stock_selection'],16<<20));refs.append(row['stock_selection'])
        cutoff=f'{year-1}-12-31'
        require(selection['protocol_sha256']==PROTOCOL_SHA256 and selection['cutoff']==cutoff
                and selection['option_returns_used'] is False
                and type(selection['selected']) is list and len(selection['selected'])<=10
                and len(set(selection['selected']))==len(selection['selected']),
                'wrong annual stock-only formation or duplicated symbols')
        symbols=selection['selected'];exit_rule='hold';fit_status='fixed_hold_baseline'
        if kind=='selected':
            fit=_loads(read_ref(row['fit_receipt'],4<<20));proof=_loads(read_ref(row['fit_admission'],4<<20))
            refs.extend((row['fit_receipt'],row['fit_admission']))
            require(fit['schema']=='AutoWheelRD1PolicyFitArithmeticV1' and fit['protocol_sha256']==PROTOCOL_SHA256
                    and fit['mode']==mode and fit['cutoff']==cutoff
                    and proof['schema']=='AutoWheelRD1PolicyAdmissionV1'
                    and proof['status']=='admitted_under_RD1' and proof['unqualified_inputs']==[]
                    and proof['protocol_sha256']==PROTOCOL_SHA256 and proof['mode']==mode
                    and proof['cutoff']==cutoff and proof['fit_receipt_sha256']==row['fit_receipt']['sha256'],
                    'annual fitted choice lacks matching complete external admission')
            training=proof['source_artifacts']
            require(type(training) is list and 0<len(training)<=2000, 'fit admission lacks retained evidence')
            for ref in training:read_ref(ref,32<<20);refs.append(ref)
            fit_status=fit['status']
            if fit_status=='no_qualified_choice':
                require(fit['selected_profile_id'] is None
                        and fit['forward_new_openings']=='abstain_preserve_existing_opening_rules',
                        'no-choice fit is not a complete abstention decision')
                symbols=[]
            else:
                require(fit_status=='selected_conditional_on_external_admission'
                        and fit['selected_profile_id'] in (f'rd1-{mode}-hold',f'rd1-{mode}-tp50',f'rd1-{mode}-tp75'),
                        'incomplete training cannot manufacture a forward cash win')
                exit_rule=fit['selected_profile_id'].split('-')[2]
        require(policy=={'from':f'{year}-01-01','through':f'{year}-12-31','symbols':symbols,
                         'profile':dict(mode=mode,exit=exit_rule),
                         'stock_selection_sha256':row['stock_selection']['sha256']},
                'opening policy differs from admitted annual choice/universe')
    return dict(mode=mode,account_kind=kind,years=years,retained_evidence=refs)


def completed_account(reference,run,read_ref,read_source,*,allowed_purposes=('forward','sensitivity')):
    """Freshly verify an exact charged account and its original-source binding."""
    receipt=_loads(read_ref(reference,4<<20));before=receipt['case'];case_identity(before)
    require(receipt['schema']=='AutoWheelRD1DispatchReceiptV1'
            and receipt['resources'] and receipt['resources']['exit_code']==0
            and not receipt['resources']['stopped_reason'], 'completed account process did not finish cleanly')
    require(set(allowed_purposes)<= {'forward','sensitivity','pilot'}
            and before['purpose'] in allowed_purposes and before['model_sha256']==PROTOCOL_SHA256
            and before['sensitivity'] in SCENARIOS
            and (before['sensitivity']!='primary')==(before['purpose']=='sensitivity'),
            'completed account is outside the declared primary/scenario envelope')
    require(Path(reference['file'])==run/'dispatch'/before['case_id']/'receipt.json',
            'prior receipt outside its exact dispatch directory')
    with locked(run):records,ledger=read_ledger(run)
    completed=ledger['cases'].get(before['case_id'])
    terminals=[row for row in records if row.get('kind')=='terminal' and row['case']['case_id']==before['case_id']]
    require(completed and completed['status']=='complete' and completed['identity']==before and len(terminals)==1
            and terminals[0]['result_sha256']==reference['sha256'] and receipt['status']=='complete',
            'prior forward case is not an exact completed ledger result')
    old_spec=_loads(read_ref(receipt['invocation'],4<<20))
    require(old_spec['case']==before and old_spec['build']['sha256']==before['implementation_sha256'],
            'completed invocation case/build identity differs')
    require(sha(read_ref(old_spec['build'],4<<20))==before['implementation_sha256'],
            'completed build receipt bytes differ')
    old_packet=_loads(read_ref(old_spec['packet'],256<<20))
    require(old_spec['packet']['sha256']==before['input_sha256']
            and old_packet['admission_receipt_sha256']==old_spec['admission']['sha256'],
            'prior charged input or admission differs')
    old_admission=_loads(read_ref(old_spec['admission'],4<<20))
    body=dict(old_packet);del body['admission_receipt_sha256']
    require(old_admission['schema']=='AutoWheelRD1InputAdmissionV1'
            and old_admission['protocol_sha256']==PROTOCOL_SHA256
            and old_admission['status']=='admitted_under_RD1' and old_admission['unqualified_inputs']==[]
            and old_admission['cost_basis']=='verified_historical_customer_fees'
            and old_admission['packet_body_sha256']==sha(canonical(body)),
            'prior complete source-admission binding differs')
    sources=old_admission['source_artifacts']
    require(type(sources) is list and 0<len(sources)<=2000, 'prior source-admission originals absent')
    for source in sources:read_source(source,512<<20)
    if before['purpose']=='pilot':
        _,mode,exit_rule=before['profile_id'].split('-')
        require(old_packet['opening_policies'] and all(row['profile']==dict(mode=mode,exit=exit_rule)
                for row in old_packet['opening_policies']), 'completed pilot changed its fixed profile')
        plan=dict(mode=mode,account_kind='fixed_pilot')
    else:plan=annual_policies(before,old_packet,old_admission,read_ref)
    by_name={Path(x['file']).name:x for x in receipt['artifacts']}
    require(len(by_name)==len(receipt['artifacts'])==3
            and set(by_name)=={'journal.jsonl','result.json','diagnostics.jsonl'},
            'prior account lacks unique journal, diagnostics or final state')
    require(all(Path(by_name[name]['file'])==run/'cases'/before['case_id']/name
                for name in by_name), 'completed outputs outside exact case directory')
    read_ref(by_name['journal.jsonl'],32<<20)
    read_ref(by_name['diagnostics.jsonl'],32<<20)
    old_result=_loads(read_ref(by_name['result.json'],4<<20))
    args=(by_name['journal.jsonl']['file'],by_name['journal.jsonl']['sha256'],
          by_name['result.json']['file'],by_name['result.json']['sha256'])
    more=(*args,old_spec['packet']['file'],old_spec['packet']['sha256'])
    require(verify(*args)['qualified']
            and availability_analyze(*more,expected_sensitivity=before['sensitivity'])['status']=='arithmetic_complete_pending_external_admission'
            and effect_analyze(*more,expected_sensitivity=before['sensitivity'])['status']=='arithmetic_complete_pending_external_admission'
            and attempt_analyze(*more,by_name['diagnostics.jsonl']['file'],by_name['diagnostics.jsonl']['sha256'],
                expected_sensitivity=before['sensitivity'])['status']=='arithmetic_complete_pending_external_admission'
            and terminal_analyze(*more,expected_sensitivity=before['sensitivity'])['status']=='arithmetic_complete_pending_external_admission'
            and metrics_analyze(*more,expected_sensitivity=before['sensitivity'])['status']=='arithmetic_complete_pending_external_admission',
            'prior account no longer passes independent pre-charge verification')
    return dict(reference=reference,case=before,spec=old_spec,packet=old_packet,admission=old_admission,
        result=old_result,plan=plan,artifacts=receipt['artifacts'],account=dict(case=before,packet=old_spec['packet'],
            journal=by_name['journal.jsonl'],result=by_name['result.json']))


def reproduction_plan(case,original,packet,admission,build):
    """Reproduce one frozen primary segment; no fitting or new data selection."""
    before=original['case'];case_identity(case)
    require(case['purpose']=='reproduction' and before['purpose']=='forward'
            and case['sensitivity']==before['sensitivity']=='primary'
            and case['case_id']!=before['case_id']
            and all(case[k]==before[k] for k in before if k not in ('purpose','case_id')),
            'reproduction changes the original forward account identity')
    require(all(value==original['spec'][name] for name,value in
                (('packet',packet),('admission',admission),('build',build))),
            'reproduction does not use exact original packet, admission and build references')
    return dict(original_forward_receipt=original['reference'],original_case=before,
        exact_original_artifacts=original['artifacts'],same_seed_data_policies_and_implementation=True,
        refitted=False,source_admission_performed=False)


def reproduced_outputs(proof,artifacts):
    """Byte-identical financial outputs, excluding variable process resources."""
    def outputs(values):
        by_name={Path(row['file']).name:(row['sha256'],row['bytes']) for row in values}
        require(len(by_name)==len(values)==3
                and set(by_name)=={'journal.jsonl','diagnostics.jsonl','result.json'},
                'reproduction lacks three unique output artifacts')
        return by_name
    require(outputs(proof['exact_original_artifacts'])==outputs(artifacts),
            'reproduction output bytes differ from the original forward case')
    return dict(status='arithmetic_complete_pending_external_admission',
        exact_journal_result_and_diagnostics_reproduced=True,
        original_forward_receipt=proof['original_forward_receipt'],resource_timing_compared=False,
        historical_strategy_qualification_performed=False)


def sensitivity_plan(case,packet_ref,paired,chain):
    """Pure exact-pair/chain checks. Fresh charged-result verification is external."""
    require(case['purpose']=='sensitivity' and case['sensitivity'] in SCENARIOS[1:]
            and chain and chain[0]['case']['end'].startswith('2025-'),
            'sensitivities require the full serialized primary forward procedure')
    for node in chain:
        before=node['case']
        require(before['purpose']=='forward' and before['sensitivity']=='primary'
                and all(before[k]==case[k] for k in ('profile_id','start','implementation_sha256','model_sha256')),
                'sensitivity changes primary account, model, or implementation')
    require(paired['case']['end']==case['end'] and packet_ref['sha256']==case['input_sha256']
            ==paired['spec']['packet']['sha256']
            and chain[-1]['reference']==paired['reference'],
            'sensitivity inputs differ from its exact primary chain counterpart')
    return dict(primary_2025_receipt=chain[0]['reference'],paired_primary_receipt=paired['reference'],
        frozen_primary_policy_choices=True,identical_primary_packet_sha256=case['input_sha256'],
        original_primary_chain_reverified=True,refitted_under_shock=False)


def preflight(spec,packet,admission,run,read_ref,read_source=None):
    case=spec['case'];plan=annual_policies(case,packet,admission,read_ref)
    source_read=read_source or read_ref
    scenario_proof=None
    if case['purpose']=='sensitivity':
        require('paired_primary_forward_receipt' in spec and 'frozen_primary_forward_2025_receipt' in spec,
                'sensitivity lacks its paired and full-2025 primary receipts')
        paired=completed_account(spec['paired_primary_forward_receipt'],run,read_ref,source_read)
        head=completed_account(spec['frozen_primary_forward_2025_receipt'],run,read_ref,source_read)
        chain=[head]
        require(head['case']['end'][:4]=='2025', 'full primary procedure through 2025 missing')
        while int(chain[-1]['case']['end'][:4])>int(case['end'][:4]):
            child=chain[-1];prior_ref=child['spec'].get('previous_forward_receipt')
            require(prior_ref is not None and len(chain)<3,'primary forward chain is missing or cyclic')
            parent=paired if prior_ref==paired['reference'] else completed_account(prior_ref,run,read_ref,source_read)
            proof=input_prefix(parent['packet'],child['packet'])
            require(child['case']['parent_state_sha256']==sha(canonical(parent['result']['checkpoint_state'])),
                    'primary forward chain state binding changed')
            journal_prefix(read_ref(parent['account']['journal'],32<<20),read_ref(child['account']['journal'],32<<20),
                           proof['boundary_at'],parent['case']['end'])
            chain.append(parent)
        scenario_proof=sensitivity_plan(case,spec['packet'],paired,chain)
    previous_ref=spec.get('previous_forward_receipt')
    if int(case['end'][:4])==2023:
        require(previous_ref is None and case['parent_state_sha256']=='0'*64,
                'initial 2023 forward case must start at the common flat seed')
        return dict(plan=plan,previous=None,sensitivity_primary_binding=scenario_proof)
    require(previous_ref is not None, 'forward extension lacks a prior charged account')
    old=completed_account(previous_ref,run,read_ref,source_read);before=old['case']
    require(all(before[k]==case[k] for k in
        ('purpose','profile_id','start','implementation_sha256','model_sha256','sensitivity')),
        'forward account, sensitivity, or implementation changed')
    proof=input_prefix(old['packet'],packet)
    require(case['parent_state_sha256']==sha(canonical(old['result']['checkpoint_state'])),
            'extension not bound to exact prior account state')
    return dict(plan=plan,input_prefix=proof,previous=old['account'],sensitivity_primary_binding=scenario_proof)
