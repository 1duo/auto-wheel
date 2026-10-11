"""Read-only acquisition startup using bounded journal and manifest readers.

Verifies the accepted complete decision plan, current global financial history,
candidate CLI identities and preserved original holds. It never reads a key,
submits a job, creates per-query journal copies, or qualifies rollout capacity.
"""
import argparse
from collections import Counter
import json
import os
from pathlib import Path
import stat

from batch_acquisition_proof import file_hash, read_json
from research_batch_history import BatchHistory, DecisionPlan, PROTECTED
from research_history_reader import read_qualification_manifest

TOOLS_RECEIPT_SHA = 'ac1325e1a8bf186cdaf0bf618552c91bc6b70e0c71946c770e0c3d02c613042a'
ORIGINAL_SNAPSHOT_SHA = 'b24e80323d69fc56d58539b5d335e27db1ee65e45e1ebd477c7892a5ed25b902'


def classify_window(history, row):
    entry = history.by_operation.get(row[6])
    if entry is not None:
        if entry['endpoint'] != '/batch.submit_job' or entry['query_hash'] != row[5]:
            raise ValueError('existing window financial identity differs')
        return 'acquired_existing_window' if entry['status'] == 'acquired' else 'protected_existing_window'
    if row[7] == 'reuse_acquired_full_day':
        original = history.acquired(row[8])
        if original['operation_id'] != row[8]:
            raise ValueError('original full-day source identity differs')
        return 'acquired_existing_full_day'
    if row[7] != 'fresh_not_submitted':
        raise ValueError('existing plan outcome cannot become a new submission')
    history.fresh(row[6])
    return 'fresh_not_submitted'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('source-root', 'plan', 'ledger', 'original-ledger-snapshot', 'qualification', 'tools-receipt', 'output'):
        parser.add_argument('--' + name, type=Path, required=True)
    args = parser.parse_args()
    for path in vars(args).values():
        if not path.is_absolute() or path != Path(os.path.normpath(path)) or path.resolve() != path:
            raise ValueError('clean absolute preflight paths required')
    current = BatchHistory(args.ledger);original = BatchHistory(args.original_ledger_snapshot)
    if len(original.by_operation) != 708 or original.snapshot.sha256 != ORIGINAL_SNAPSHOT_SHA:
        raise ValueError('original708 financial snapshot required')
    original_rows = original.snapshot.value['reservations']
    if current.snapshot.value['reservations'][:708] != original_rows or current.by_operation.get(PROTECTED) != original.by_operation[PROTECTED]:
        raise ValueError('all original financial fields and unresolved hold must remain unchanged')
    plan = DecisionPlan(args.plan)
    qualification = read_qualification_manifest(args.qualification)
    known = {v['path']: v['sha256'] for v in qualification.value['source_files']}
    if file_hash(args.tools_receipt) != TOOLS_RECEIPT_SHA:
        raise ValueError('accepted isolated research tool component receipt differs')
    receipt = read_json(args.tools_receipt)
    if not receipt['journal_size_component_accepted'] or receipt['uncapped_file_bytes'] != 64 << 20 or receipt['maximum_entries'] != 100000:
        raise ValueError('accepted bounded research tools required')
    expected_names = {'autowheel-batch', 'autowheel-batch-finalize', 'autowheel-batch-files',
                      'autowheel-batch-retrieve', 'autowheel-batch-planned-retrieve'}
    if {v['name'] for v in receipt['tools']} != expected_names or len(receipt['tools']) != 5:
        raise ValueError('complete research tool set required')
    for tool in receipt['tools']:
        path = Path(tool['path']);info = path.lstat()
        if (path.resolve() != path or not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o700 or file_hash(path) != tool['sha256']
                or known.get(str(path)) != tool['sha256']):
            raise ValueError('manifested candidate research binary differs')
    counts = Counter(classify_window(current, row) for row in plan.value['windows'])
    current.unchanged();original.unchanged();plan.unchanged()
    if read_qualification_manifest(args.qualification).raw != qualification.raw:
        raise ValueError('qualification changed during preflight')
    report = dict(schema='AutoWheelLargeHistoryDecisionAcquisitionPreflightV1', decision_windows=40725,
                  dispositions=dict(counts), accepted_plan_sha256=plan.sha256,
                  current_journal_sha256=current.snapshot.sha256, original708_snapshot_sha256=original.snapshot.sha256,
                  original708_rows_and_unknown_hold_preserved=True, whole_run_journal_reader_integrated=True,
                  large_qualification_reader_integrated=True, source_bound_candidate_tools_verified=5,
                  per_query_financial_journal_copies_created=0, current_qualification_sha256=qualification.sha256,
                  financial_execution_implemented=False, financial_expansion_ready=False,
                  remaining_before_expansion=['bounded acquisition controller and scoped external storage integration',
                    'complete decision-source capacity admission and original financial transition controls',
                    'explicit treatment of any zero-record provider window without fabricated acquisition or absence'],
                  full_replay_query_plan_complete=False, full_study_capacity_proven=False, source_gate_passed=False,
                  historical_performance_available=False, provider_requests=0, financial_requests=0,
                  live_journal_writes=0, credentials_read=False, financial_retry=False)
    parent = args.output.parent.lstat()
    if not stat.S_ISDIR(parent.st_mode) or parent.st_uid != os.getuid() or stat.S_IMODE(parent.st_mode) != 0o700:
        raise ValueError('private preflight output directory required')
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'wb') as stream:
        stream.write((json.dumps(report, indent=2, sort_keys=True) + '\n').encode());stream.flush();os.fsync(stream.fileno())
    print(json.dumps(report))


if __name__ == '__main__':
    main()
