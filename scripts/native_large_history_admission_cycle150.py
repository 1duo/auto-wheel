"""Admit actual large-history native references to unchanged closed-cut reducers."""
from dataclasses import dataclass
import importlib.util
from pathlib import Path

from batch_acquisition_proof import file_hash, read_json

REFERENCE_SHA = 'f54a5e2320e6e2059e7fa128be6cb0c776ee36d60c2c19d04b56c73c5e36bf9c'
ORIGINAL_REFERENCE_SHA = '841c580f63a55292884a6090f811fc6e3121dbbc0017be130ec5220c59dbb17a'
ADMISSION_SCHEMA = 'AutoWheelLargeHistoryReferenceAdmissionV1'


def reference_binding(reference, audit, book, history, audited_history, plan, audit_sha):
    """Reject mismatched receipts before any native data reduction starts."""
    query = book.proof['query']
    member = audit['files'][0]
    expected = {
        'schema_version': 'AutoWheelLargeHistoryNativeBookVectorReferenceV1',
        'reference_script_sha256': REFERENCE_SHA,
        'original_reference_script_sha256': ORIGINAL_REFERENCE_SHA,
        'audit_sha256': audit_sha,
        'receipt_sha256': book.proof['receipt_sha256'],
        'acquisition_proof_sha256': book.entry['data_sha256'],
        'financial_snapshot_sha256': history.snapshot.sha256,
        'audited_financial_snapshot_sha256': audited_history.snapshot.sha256,
        'decision_plan_sha256': plan.sha256,
        'vector_jsonl_sha256': member['vector_jsonl_sha256'],
    }
    if any(reference.get(key) != value for key, value in expected.items()):
        raise ValueError('completed native reference provenance differs')
    integers = {'records_compared_field_for_field': book.proof['provider_record_count'],
                'vector_fields_per_record': 21, 'queried_symbols': len(query['symbols']), 'retimed_rows': 0}
    if any(type(reference.get(key)) is not int or reference[key] != value for key, value in integers.items()):
        raise ValueError('completed native reference totals differ')
    if (reference['records_compared_field_for_field'] != audit['records']
            or reference['records_compared_field_for_field'] != member['records']
            or any(reference.get('query_' + key) != query[key] for key in ('dataset', 'schema', 'start', 'end'))
            or reference.get('full_source_qualification') is not False
            or reference.get('historical_performance_available') is not False
            or (query['dataset'], query['schema']) != ('OPRA.PILLAR', 'cmbp-1')):
        raise ValueError('completed option native reference scope differs')


@dataclass(frozen=True)
class Admission:
    book: object
    history: object
    audited_history: object
    plan: object
    audit_path: Path
    reference_path: Path
    audit: dict
    reference: dict
    extra_paths: tuple

    def unchanged(self):
        self.history.unchanged()
        self.audited_history.unchanged()
        self.plan.unchanged()


def admit_source(proof_path, snapshot_path, plan_path, audit_directory, reference_path, root):
    """Read the whole immutable journal; preserve its original acquired rows."""
    from research_batch_history import BatchHistory, DecisionPlan, acquired_book
    root = Path(root)
    helper = root / 'scripts/verify-native-book-vectors-large-history-cycle146.py'
    original = root / 'scripts/verify-native-book-vectors.py'
    if helper.is_symlink() or original.is_symlink() or file_hash(helper) != REFERENCE_SHA or file_hash(original) != ORIGINAL_REFERENCE_SHA:
        raise ValueError('reviewed native reference code differs')
    spec = importlib.util.spec_from_file_location('preserved_large_history_native_reference', helper)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    audit_path = Path(audit_directory) / 'audit.json'
    reference_path = Path(reference_path)
    for path in (Path(proof_path), Path(snapshot_path), Path(plan_path), audit_path, reference_path):
        module.private_file(path)
    history, plan = BatchHistory(snapshot_path), DecisionPlan(plan_path)
    book = acquired_book(proof_path, history, root, plan=plan)
    audit, reference = read_json(audit_path), read_json(reference_path)
    audited_path = Path(audit['financial_snapshot_path'])
    module.private_file(audited_path)
    audited_history = BatchHistory(audited_path)
    source_receipt_path = Path(book.proof['source_directory']) / 'receipt.json'
    module.private_file(source_receipt_path)
    source_receipt = read_json(source_receipt_path)
    module.large_binding(audit, source_receipt, book, history, audited_history, plan)
    reference_binding(reference, audit, book, history, audited_history, plan, file_hash(audit_path))
    extra = (Path(snapshot_path), audited_path, Path(plan_path), Path(__file__).resolve(), helper, original,
             root / 'scripts/research_batch_history.py', root / 'scripts/research_history_reader.py')
    return Admission(book, history, audited_history, plan, audit_path, reference_path, audit, reference, extra)
