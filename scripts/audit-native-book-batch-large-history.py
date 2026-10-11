"""Native audit using an immutable whole-run journal and declared query scope.

The original full-day adapter and V1 proof checker remain unchanged. Native
vectors use their existing field, clock, EOF and metadata checks. Passing this
audit does not qualify execution, terms, Greeks, source gates or returns.
"""
import argparse
from datetime import datetime, timezone
import importlib.metadata
import os
from pathlib import Path
import shutil
import subprocess

from batch_acquisition_proof import file_hash
from research_batch_history import BatchHistory, DecisionPlan, acquired_book, load_original_adapter


def prepare(proof_path, ledger_path, root, plan_path=None):
    history = BatchHistory(ledger_path)
    plan = DecisionPlan(plan_path) if plan_path is not None else None
    source = acquired_book(proof_path, history, root, plan=plan)
    return history, plan, source


def window_metadata(source, adapter, dbn):
    """Require the accepted complete dated identity boundary before vectors."""
    import native_book_window_boundary as boundary
    for member in source.proof['native']['files']:
        path = Path(source.proof['source_directory']) / member['filename']
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        try:
            process = subprocess.Popen(['/usr/bin/zstd', '-q', '-f', '-d', '-c', '--', '/dev/fd/' + str(fd)],
                                       pass_fds=(fd,), stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
            try:
                prelude = process.stdout.read(8)
                if len(prelude) != 8 or prelude[:3] != b'DBN':
                    raise ValueError('complete native metadata prelude required')
                size = int.from_bytes(prelude[4:8], 'little')
                if not 100 <= size <= (16 << 20) - 8:
                    raise ValueError('native metadata byte bound')
                header = prelude + process.stdout.read(size)
                decoder = dbn.DBNDecoder(upgrade_policy=dbn.VersionUpgradePolicy.AS_IS, compression=dbn.Compression.NONE)
                rows = decoder.write_and_decode(header)
                if len(header) != size + 8 or decoder.buffer() or len(rows) != 1 or not isinstance(rows[0], dbn.Metadata):
                    raise ValueError('complete AS_IS metadata required')
                boundary.window_binding(source.proof['query'], rows[0], source.cut_ns, 30_000_000_000, adapter)
            finally:
                if process.poll() is None:
                    process.kill()
                process.wait();process.stdout.close()
        finally:
            os.close(fd)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--proof', type=Path, required=True)
    parser.add_argument('--ledger-snapshot', type=Path, required=True)
    parser.add_argument('--decision-plan', type=Path)
    parser.add_argument('--output-directory', type=Path, required=True)
    args = parser.parse_args()
    root = Path(__file__).resolve().parent.parent
    history, plan, source = prepare(args.proof, args.ledger_snapshot, root, args.decision_plan)
    adapter = load_original_adapter(root)
    if importlib.metadata.version('databento-dbn') != adapter.DECODER_VERSION:
        raise ValueError('pinned AS_IS native decoder required')
    adapter.private_directory(args.output_directory.parent)
    if args.output_directory.exists() or shutil.disk_usage(args.output_directory.parent).free < 10 << 30:
        raise ValueError('fresh output and native audit headroom required')
    args.output_directory.mkdir(mode=0o700)
    import databento_dbn as dbn
    if source.cut_ns is not None:
        window_metadata(source, adapter, dbn)
    rows = [adapter.audit_shard(Path(source.proof['source_directory']), member, source.proof['query'],
                                source.start_ns, source.end_ns, args.output_directory, dbn)
            for member in source.proof['native']['files']]
    for name, member in source.manifest.items():
        path = Path(name);path = path if path.is_absolute() else root / path
        if file_hash(path) != member['sha256'] or path.stat().st_size != member['bytes']:
            raise ValueError('retained source changed during native audit')
    history.unchanged()
    if plan is not None:
        plan.unchanged()
    report = dict(schema_version='AutoWheelLargeHistoryNativeBookAuditV1', status='observed_unqualified',
                  observed_at=datetime.now(timezone.utc).isoformat(), script_sha256=file_hash(Path(__file__)),
                  original_adapter_sha256=file_hash(root / 'scripts/audit-native-book-batch.py'),
                  decoder_version=adapter.DECODER_VERSION, upgrade_policy='AS_IS', files=rows,
                  records=sum(v['records'] for v in rows), provider_record_count=source.proof['provider_record_count'],
                  operation_id=source.entry['operation_id'], acquisition_proof_sha256=source.entry['data_sha256'],
                  financial_snapshot_path=str(history.path), financial_snapshot_sha256=source.history_sha256,
                  financial_snapshot_entries=len(history.by_operation), per_query_financial_snapshot_created=False,
                  query_scope=source.scope, source_start_ns=source.start_ns, source_end_ns=source.end_ns,
                  closed_cut_ns=source.cut_ns, decision_plan_sha256=plan.sha256 if plan else None,
                  source_gate_passed=False, pending_order_coverage_proven=False, replay_frames_produced=False,
                  historical_performance_available=False, provider_requests=0, financial_requests=0, live_journal_writes=0)
    if report['records'] != report['provider_record_count']:
        raise ValueError('complete native count differs')
    adapter.write_json(args.output_directory / 'audit.json', report)
    print(report['records'])


if __name__ == '__main__':
    main()
