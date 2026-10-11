"""Large native books: bounded streaming EOF/count verification only.

Legacy native_batch_completeness.py remains immutable for earlier proofs.
The provider count bounds this verifier's expansion and actual record count.
"""
import datetime as dt
import hashlib
import importlib.metadata
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys

MAX_NATIVE_BOOK_BYTES = 64 << 30
MAX_METADATA_BYTES = 16 << 20
MAX_BOOK_RECORD_BYTES = 88  # 80-byte CMBP-1/MBP-1 plus optional ts_out.


def expansion_limit(records):
    if type(records) is not int or not 0 < records <= ((1 << 63) - 1 - MAX_METADATA_BYTES) // MAX_BOOK_RECORD_BYTES:
        raise ValueError('record count bound')
    return min(MAX_NATIVE_BOOK_BYTES, records * MAX_BOOK_RECORD_BYTES + MAX_METADATA_BYTES)


def unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError('duplicate')
        result[key] = value
    return result


def ns(value):
    match = re.fullmatch(r'(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?Z', value)
    if not match:
        raise ValueError('time')
    second = dt.datetime.strptime(match[1], '%Y-%m-%dT%H:%M:%S').replace(tzinfo=dt.timezone.utc)
    return int(second.timestamp()) * 1_000_000_000 + int((match[2] or '').ljust(9, '0') or '0')


def verify_file(directory, member, query, dbn):
    name, size, expected = member['filename'], member['bytes'], member['sha256']
    if set(member) != {'filename', 'bytes', 'sha256'} or not isinstance(name, str) or re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,254}', name) is None or not name.endswith('.' + query['schema'] + '.dbn.zst'):
        raise ValueError('member')
    if type(size) is not int or not 0 < size <= 8 << 30 or not isinstance(expected, str) or re.fullmatch('[0-9a-f]{64}', expected) is None:
        raise ValueError('member proof')
    fd = os.open(directory / name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, 'rb') as source:
        info = os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600 or info.st_size != size:
            raise ValueError('source')
        compressed = hashlib.sha256()
        for raw in iter(lambda: source.read(1 << 20), b''):
            compressed.update(raw)
        if compressed.hexdigest() != expected:
            raise ValueError('compressed hash')
        source.seek(0)
        decoder = dbn.DBNDecoder(upgrade_policy=dbn.VersionUpgradePolicy.AS_IS, compression=dbn.Compression.NONE)
        # Pass the already checked descriptor, so replacing a path cannot swap
        # the source between its hash check and decompression.
        # -f permits the intentional /dev/fd symlink; -c always writes stdout.
        proc = subprocess.Popen(['/usr/bin/zstd', '-q', '-f', '-d', '-c', '--', '/dev/fd/' + str(source.fileno())],
                                pass_fds=(source.fileno(),), stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        metadata, count, total = None, 0, 0
        decoded_hash = hashlib.sha256()
        kinds = {'cmbp-1': {'CMBP1Msg'}, 'mbp-1': {'MBP1Msg'}}
        try:
            for raw in iter(lambda: proc.stdout.read(128 << 10), b''):
                total += len(raw)
                if total > expansion_limit(query['record_count']):
                    raise ValueError('expansion')
                decoded_hash.update(raw)
                for row in decoder.write_and_decode(raw):
                    if isinstance(row, dbn.Metadata):
                        if metadata is not None or count != 0 or row.version not in (1, 2, 3):
                            raise ValueError('metadata')
                        if row.dataset != query['dataset'] or str(row.schema) != query['schema'] or str(row.stype_out) != 'instrument_id':
                            raise ValueError('scope')
                        if row.limit is not None and (type(row.limit) is not int or row.limit != 0):
                            raise ValueError('truncated scope')
                        if not ns(query['start']) <= row.start < row.end <= ns(query['end']):
                            raise ValueError('metadata range')
                        metadata = row
                    else:
                        if metadata is None or type(row).__name__ not in kinds[query['schema']]:
                            raise ValueError('record kind')
                        count += 1
                        if count > query['record_count']:
                            raise ValueError('record count bound')
                if len(decoder.buffer()) > 16 << 20:
                    raise ValueError('residual bound')
            if proc.wait() != 0 or decoder.buffer() or metadata is None:
                raise ValueError('compressed or native EOF')
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()
            proc.stdout.close()
        return {'filename': name, 'compressed_sha256': expected, 'compressed_bytes': size,
                'uncompressed_sha256': decoded_hash.hexdigest(), 'uncompressed_bytes': total,
                'records': count, 'native_version': metadata.version, 'eof_verified': True}


def main():
    raw = sys.stdin.buffer.read((4 << 20) + 1)
    if not raw or len(raw) > 4 << 20 or importlib.metadata.version('databento-dbn') != '0.71.0':
        raise ValueError('input or decoder')
    query = json.loads(raw, object_pairs_hook=unique,
                       parse_constant=lambda _: (_ for _ in ()).throw(ValueError('nonfinite')))
    if set(query) != {'source_directory', 'dataset', 'schema', 'start', 'end', 'files', 'record_count'}:
        raise ValueError('input fields')
    allowed = {('OPRA.PILLAR', 'cmbp-1'), ('EQUS.MINI', 'mbp-1')}
    if (query['dataset'], query['schema']) not in allowed or not ns(query['start']) < ns(query['end']):
        raise ValueError('scope')
    expansion_limit(query['record_count'])
    directory = Path(query['source_directory'])
    info = directory.lstat()
    if not directory.is_absolute() or directory.resolve() != directory or not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700:
        raise ValueError('directory')
    files = query['files']
    if not isinstance(files, list) or not 1 <= len(files) <= 4096:
        raise ValueError('file count')
    if len({f['filename'] for f in files}) != len(files) or sum(f['bytes'] for f in files) > 64 << 30:
        raise ValueError('file set')
    import databento_dbn as dbn
    rows = [verify_file(directory, member, query, dbn) for member in files]
    if sum(row['records'] for row in rows) != query['record_count']:
        raise ValueError('provider record count mismatch')
    print(json.dumps({'schema_version': 'AutoWheelNativeBookCompletenessV1', 'decoder_version': '0.71.0',
                      'upgrade_policy': 'AS_IS', 'files': rows, 'records': sum(row['records'] for row in rows),
                      'all_eof_verified': True}, separators=(',', ':')))


if __name__ == '__main__':
    try:
        main()
    except Exception:
        print('native batch completeness verification failed', file=sys.stderr)
        raise SystemExit(1)
