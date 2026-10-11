"""Read-only bounds for growing research journals and qualification manifests.

Small purchase inputs and source receipts keep their existing readers. These
functions neither create a journal nor change a hold, policy or source gate.
"""
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import stat

JOURNAL_BYTES = 64 << 20
CAPPED_JOURNAL_BYTES = 1 << 20
QUALIFICATION_BYTES = 512 << 20
MAX_JOURNAL_ENTRIES = 100_000
MAX_SOURCE_ENTRIES = 1_500_000
MAX_INT64 = (1 << 63) - 1
HASH = re.compile(r'[0-9a-fA-F]{64}')
ROW_KEYS = set('operation_id query_hash endpoint cost_nano reserved_nano at response_hash status'.split())
ROW_OPTIONAL_KEYS = {'data_sha256', 'bytes'}


@dataclass(frozen=True)
class Snapshot:
    raw: bytes
    value: dict

    @property
    def sha256(self):
        return hashlib.sha256(self.raw).hexdigest()


def _unique(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError('duplicate JSON field')
        value[key] = item
    return value


def _nonfinite(_):
    raise ValueError('nonfinite JSON value')


def _snapshot(path, limit, *, private):
    path = Path(path)
    if not path.is_absolute() or path != Path(os.path.normpath(path)) or path.resolve() != path:
        raise ValueError('clean absolute evidence path required')
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        before = os.fstat(fd)
        if (not stat.S_ISREG(before.st_mode) or before.st_uid != os.getuid()
                or before.st_nlink != 1 or not 0 < before.st_size <= limit):
            raise ValueError('evidence ownership, shape or byte bound')
        if private:
            parent = path.parent.lstat()
            if (stat.S_IMODE(before.st_mode) != 0o600 or not stat.S_ISDIR(parent.st_mode)
                    or parent.st_uid != os.getuid() or stat.S_IMODE(parent.st_mode) != 0o700):
                raise ValueError('private journal and directory required')
        with os.fdopen(fd, 'rb', closefd=False) as stream:
            raw = stream.read(limit + 1)
        after = os.fstat(fd)
        fields = ('st_dev', 'st_ino', 'st_size', 'st_mtime_ns', 'st_ctime_ns', 'st_nlink', 'st_mode', 'st_uid')
        if len(raw) != before.st_size or any(getattr(before, k) != getattr(after, k) for k in fields):
            raise ValueError('evidence changed during bounded read')
    finally:
        os.close(fd)
    value = json.loads(raw, object_pairs_hook=_unique, parse_constant=_nonfinite)
    if not isinstance(value, dict):
        raise ValueError('JSON document required')
    return Snapshot(raw, value)


def _integer(value, upper=MAX_INT64):
    return type(value) is int and 0 <= value <= upper


def _hash(value):
    return isinstance(value, str) and HASH.fullmatch(value) is not None


def read_financial_journal(path):
    snapshot = _snapshot(path, JOURNAL_BYTES, private=True)
    value = snapshot.value
    if not {'version', 'lifetime_nano', 'reservations'} <= value.keys() or value.keys() - {
            'version', 'lifetime_nano', 'authorization_mode', 'reservations'}:
        raise ValueError('journal envelope')
    if type(value['version']) is not int or type(value['lifetime_nano']) is not int:
        raise ValueError('integer journal policy required')
    capped = (value['version'], value['lifetime_nano'], value.get('authorization_mode', '')) == (1, 10_000_000_000, '')
    uncapped = (value['version'], value['lifetime_nano'], value.get('authorization_mode')) == (2, 0, 'uncapped')
    if not (capped or uncapped) or (capped and len(snapshot.raw) > CAPPED_JOURNAL_BYTES):
        raise ValueError('journal policy or legacy byte bound')
    rows = value['reservations']
    if not isinstance(rows, list) or len(rows) > MAX_JOURNAL_ENTRIES:
        raise ValueError('journal entry bound')
    seen, total = set(), 0
    future = datetime.now(timezone.utc) + timedelta(minutes=2)
    for row in rows:
        if not isinstance(row, dict) or not ROW_KEYS <= row.keys() or row.keys() - ROW_KEYS - ROW_OPTIONAL_KEYS:
            raise ValueError('reservation envelope')
        if not all(_hash(row[k]) for k in ('operation_id', 'query_hash', 'response_hash')):
            raise ValueError('reservation identity')
        if row['operation_id'] in seen:
            raise ValueError('duplicate financial operation')
        seen.add(row['operation_id'])
        batch = row['endpoint'] == '/batch.submit_job'
        if row['endpoint'] not in {'/timeseries.get_range', '/batch.submit_job'} or (capped and batch):
            raise ValueError('reservation endpoint or policy')
        if row['status'] not in ({'reserved', 'acquired'} if batch else {'reserved', 'complete'}):
            raise ValueError('reservation status')
        cost, reserved = row['cost_nano'], row['reserved_nano']
        if not _integer(cost) or not _integer(reserved) or cost + (cost + 9) // 10 != reserved:
            raise ValueError('exact integer reservation margin')
        total += reserved
        if total > (10_000_000_000 if capped else MAX_INT64):
            raise ValueError('journal total bound')
        if row['status'] == 'reserved':
            if row.get('data_sha256', '') != '' or type(row.get('bytes', 0)) is not int or row.get('bytes', 0) != 0:
                raise ValueError('pending receipt')
        elif not _hash(row.get('data_sha256')) or not _integer(row.get('bytes')) or row['bytes'] == 0:
            raise ValueError('completed receipt')
        stamp = row['at']
        if not isinstance(stamp, str) or re.fullmatch(r'\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})', stamp) is None:
            raise ValueError('reservation time')
        parsed = datetime.fromisoformat(stamp.replace('Z', '+00:00'))
        if parsed <= datetime.min.replace(tzinfo=timezone.utc) or parsed > future:
            raise ValueError('reservation time bound')
    return snapshot


def read_qualification_manifest(path):
    snapshot = _snapshot(path, QUALIFICATION_BYTES, private=False)
    rows = snapshot.value.get('source_files')
    if not isinstance(rows, list) or len(rows) > MAX_SOURCE_ENTRIES:
        raise ValueError('qualification source entry bound')
    seen = set()
    for row in rows:
        if not isinstance(row, dict) or not isinstance(row.get('path'), str) or not row['path'] or not _hash(row.get('sha256')):
            raise ValueError('qualification source shape')
        path = row['path']
        if path != os.path.normpath(path) or '..' in Path(path).parts or path in seen:
            raise ValueError('qualification source path or duplicate')
        seen.add(path)
    return snapshot
