"""Read-only acquisition/audit binding to one whole-run financial snapshot.

Keep small purchase inputs and receipts on the original4MiB reader. Large
journals are parsed once, indexed without modifying a financial field, and
shared across source audits. This module cannot submit or release a hold.
"""
from dataclasses import dataclass
import gzip
import hashlib
import importlib.util
import json
from pathlib import Path
import re

from batch_acquisition_proof import (CODE_FILES, canonical_query, file_hash,
                                     native_verifier_path, read_json, utc_ns,
                                     validate_batch_acquisition)
from research_history_reader import read_financial_journal

ORIGINAL_ADAPTER_SHA = '3fa7f96d870ed202079169d6041bd5e57d0447eeaa488c1d5c9970c855335491'
PROTECTED = '01593d27c3a667a42b8736bba57c377e98dd6f19949f4601e2c08ef505abbf5f'
PLAN_SHA = '930fc038e1295708b950ba1a39f47c72c6e53c13b6f3a18bcc2c0b76db3f2a35'
MAX_PLAN_BYTES = 64 << 20


def load_original_adapter(root):
    path = root / 'scripts/audit-native-book-batch.py'
    if path.is_symlink() or file_hash(path) != ORIGINAL_ADAPTER_SHA:
        raise ValueError('original source-bound native adapter differs')
    spec = importlib.util.spec_from_file_location('preserved_native_book_adapter', path)
    module = importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
    return module


def _unique(pairs):
    out = {}
    for key, value in pairs:
        if key in out:
            raise ValueError('duplicate plan field')
        out[key] = value
    return out


def _nonfinite(_):
    raise ValueError('nonfinite plan field')


class DecisionPlan:
    """The previously independently verified complete immutable decision plan."""
    def __init__(self, path):
        self.path = Path(path)
        if self.path.is_symlink() or file_hash(self.path) != PLAN_SHA:
            raise ValueError('accepted complete decision plan differs')
        with gzip.open(self.path, 'rb') as stream:
            raw = stream.read(MAX_PLAN_BYTES + 1)
            if not raw or len(raw) > MAX_PLAN_BYTES or stream.read(1):
                raise ValueError('complete bounded decision plan required')
        self.value = json.loads(raw, object_pairs_hook=_unique, parse_constant=_nonfinite)
        if (self.value['schema'] != 'AutoWheelFrozenDecisionWindowPlanV1'
                or len(self.value['windows']) != 40725 or len(self.value['partitions']) != 543
                or self.value['sessions'] != 126 or self.value['full_replay_query_plan_complete'] is not False):
            raise ValueError('complete decision-only scope required')
        self.by_operation = {v[6]: v for v in self.value['windows']}
        if len(self.by_operation) != 40725:
            raise ValueError('duplicate decision operation')
        self.sha256 = PLAN_SHA

    def window(self, query):
        wire = canonical_query(query)
        op = hashlib.sha256(wire + b'\0/batch.submit_job').hexdigest()
        row = self.by_operation.get(op)
        if row is None or row[5] != hashlib.sha256(wire).hexdigest():
            raise ValueError('query outside independently verified decision plan')
        if utc_ns(query['start']) != row[3] or utc_ns(query['end']) != row[4] or row[3] != row[2] - 30_000_000_000 or row[4] != row[2] + 1:
            raise ValueError('exact declared closed window required')
        return row

    def unchanged(self):
        if file_hash(self.path) != self.sha256:
            raise ValueError('accepted decision plan changed')


class BatchHistory:
    def __init__(self, path):
        self.path = Path(path)
        self.snapshot = read_financial_journal(self.path)
        value = self.snapshot.value
        if (value['version'], value['lifetime_nano'], value.get('authorization_mode')) != (2, 0, 'uncapped'):
            raise ValueError('original uncapped financial snapshot required')
        self.by_operation = {v['operation_id']: v for v in value['reservations']}

    def unchanged(self):
        if read_financial_journal(self.path).raw != self.snapshot.raw:
            raise ValueError('whole-run financial snapshot changed')

    def acquired(self, operation):
        row = self.by_operation.get(operation)
        if row is None or row['endpoint'] != '/batch.submit_job' or row['status'] != 'acquired':
            raise ValueError('original acquired batch operation required')
        return row

    def fresh(self, operation):
        # Reserved and completed operations both prohibit a new submission.
        if operation in self.by_operation:
            raise ValueError('financial operation already exists; never resubmit')
        protected = self.by_operation.get(PROTECTED)
        if protected is None or protected['status'] != 'reserved':
            raise ValueError('original unresolved hold must remain reserved')


@dataclass(frozen=True)
class AcquiredBook:
    proof: dict
    manifest: dict
    entry: dict
    start_ns: int
    end_ns: int
    scope: str
    cut_ns: int | None
    history_sha256: str


def acquired_book(proof_path, history, root, *, plan=None):
    """Use the unchanged V1 checker with a large indexed financial snapshot."""
    adapter = load_original_adapter(root)
    proof_path = Path(proof_path)
    adapter.private_file(proof_path)
    proof = read_json(proof_path)
    entry = history.acquired(proof['reservation']['operation_id'])
    directory = Path(proof['source_directory']);adapter.private_directory(directory)
    expected = {str(proof_path): entry['data_sha256']}

    def expect(name, digest):
        if name in expected or not isinstance(name, str) or not isinstance(digest, str):
            raise ValueError('duplicate or malformed original evidence path')
        expected[name] = digest

    for path_key, hash_key in (('query_path', 'query_file_sha256'),
                               ('acknowledgement_path', 'acknowledgement_file_sha256'), ('ack_body_path', 'ack_body_sha256')):
        expect(proof[path_key], proof[hash_key])
    for name, hash_key in (('receipt.json', 'receipt_sha256'), ('job-source.json', 'job_sha256'), ('files-source.json', 'file_list_sha256')):
        expect(str(directory / name), proof[hash_key])
    for member in proof['files']:
        if not isinstance(member['filename'], str) or re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,254}', member['filename']) is None:
            raise ValueError('original source filename')
        expect(str(directory / member['filename']), member['sha256'])
    manifest = {}
    for name, digest in expected.items():
        path = Path(name);adapter.private_file(path)
        if file_hash(path) != digest:
            raise ValueError('retained original source digest differs')
        manifest[name] = dict(sha256=digest, bytes=path.stat().st_size)
    verifier = native_verifier_path(proof)
    for name in dict.fromkeys(CODE_FILES + [verifier]):
        path = root / name
        manifest[name] = dict(sha256=file_hash(path), bytes=path.stat().st_size)
    ok, reason = validate_batch_acquisition(entry, str(proof_path), str(directory / 'receipt.json'), manifest, set(manifest), root)
    if not ok:
        raise ValueError('original financial/acquisition proof invalid: ' + reason)
    query = proof['query'];canonical_query(query)
    if (query['dataset'], query['schema']) not in {('OPRA.PILLAR', 'cmbp-1'), ('EQUS.MINI', 'mbp-1')} or query['stype_in'] != 'raw_symbol':
        raise ValueError('declared book scope required')
    start, end = utc_ns(query['start']), utc_ns(query['end'])
    cut = None
    if end - start == 86_400_000_000_000 and start % 86_400_000_000_000 == 0:
        scope = 'complete_UTC_day_acquisition'
    elif plan is not None:
        row = plan.window(query);cut = row[2];scope = 'declared_closed_decision_window_acquisition'
    else:
        raise ValueError('short window requires the accepted complete decision plan')
    history.unchanged()
    if plan is not None:
        plan.unchanged()
    return AcquiredBook(proof, manifest, entry, start, end, scope, cut, history.snapshot.sha256)
