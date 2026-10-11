"""Read-only acquired-batch evidence checks; no invoice or source qualification."""
import datetime as dt
import hashlib
import json
from pathlib import Path
import re

PROOF_KEYS = set('schema_version reservation query acknowledgement query_path acknowledgement_path ack_body_path source_directory query_file_sha256 acknowledgement_file_sha256 ack_body_sha256 job_sha256 file_list_sha256 receipt_sha256 job_id files total_bytes provider_record_count native_verifier_sha256 native verified_at invoice_cost_verified source_qualified'.split())
NATIVE_KEYS = set('schema_version decoder_version upgrade_policy files records all_eof_verified'.split())
NATIVE_FILE_KEYS = set('filename compressed_sha256 compressed_bytes uncompressed_sha256 uncompressed_bytes records native_version eof_verified'.split())
ACK_KEYS = set('operation_id query_hash endpoint cost_nano reserved_nano response_hash bytes status_code at job_id state'.split())
RECEIPT_KEYS = set('query operation_id query_hash job_id state ack_sha256 job_sha256 file_list_sha256 files total_bytes observed_at completed_at all_verified'.split())
CODE_FILES = ['internal/databento/ledger.go', 'internal/databento/batch_finalize.go',
              'internal/databento/native_batch_completeness.py', 'cmd/autowheel-batch-finalize/main.go',
              'scripts/batch_acquisition_proof.py', 'scripts/check-simulation-completion.py']
MAX_INT64 = (1 << 63) - 1
SYMBOLS = {'NFLX', 'TSLA', 'NVDA', 'NBIS', 'IREN', 'NOW', 'AMD', 'AMZN',
           'GOOGL', 'META', 'MSFT', 'PLTR', 'UBER', 'AVGO', 'ORCL'}


def native_verifier_path(proof):
    schema = proof.get('native', {}).get('schema_version')
    if schema == 'AutoWheelNativeBookCompletenessV1':
        if (proof['query']['dataset'], proof['query']['schema']) not in {('OPRA.PILLAR', 'cmbp-1'), ('EQUS.MINI', 'mbp-1')}:
            raise ValueError('book verifier scope')
        return 'internal/databento/native_book_completeness.py'
    if schema == 'AutoWheelNativeBatchCompletenessV1':
        return 'internal/databento/native_batch_completeness.py'
    raise ValueError('native verifier schema')


def book_expansion_limit(records):
    if not integer(records, 1, (MAX_INT64 - (16 << 20)) // 88):
        raise ValueError('book record count bound')
    return min(64 << 30, records * 88 + (16 << 20))


def is_hash(value):
    return isinstance(value, str) and re.fullmatch('[0-9a-f]{64}', value) is not None


def integer(value, lower=0, upper=MAX_INT64):
    return type(value) is int and lower <= value <= upper


def utc_ns(value):
    match = re.fullmatch(r'(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?Z', value)
    if not match:
        raise ValueError('timestamp')
    second = dt.datetime.strptime(match[1], '%Y-%m-%dT%H:%M:%S').replace(tzinfo=dt.timezone.utc)
    return int(second.timestamp()) * 1_000_000_000 + int((match[2] or '').ljust(9, '0') or '0')


def read_json(path):
    def unique(pairs):
        out = {}
        for key, value in pairs:
            if key in out:
                raise ValueError('duplicate')
            out[key] = value
        return out
    with path.open('rb') as source:
        raw = source.read((4 << 20) + 1)
    if not raw or len(raw) > 4 << 20:
        raise ValueError('size')
    return json.loads(raw, object_pairs_hook=unique,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError('nonfinite')))


def file_hash(path):
    result = hashlib.sha256()
    with path.open('rb') as source:
        for chunk in iter(lambda: source.read(1 << 20), b''):
            result.update(chunk)
    return result.hexdigest()


def canonical_query(query):
    if set(query) != set('dataset schema symbols stype_in start end'.split()):
        raise ValueError('query keys')
    start, end = utc_ns(query['start']), utc_ns(query['end'])
    if not utc_ns('2026-04-06T00:00:00Z') <= start < end <= utc_ns('2026-10-03T00:00:00Z'):
        raise ValueError('frozen range')
    symbols = query['symbols']
    if not isinstance(symbols, list) or not symbols or len(set(symbols)) != len(symbols):
        raise ValueError('symbols')
    raw_opra = query['dataset'] == 'OPRA.PILLAR' and query['stype_in'] == 'raw_symbol'
    if raw_opra:
        if query['schema'] not in {'definition', 'statistics', 'cmbp-1'} or len(symbols) > 2000:
            raise ValueError('scope')
        for symbol in symbols:
            if not isinstance(symbol, str) or re.fullmatch(r'[A-Z ]{6}[0-9]{6}[PC][0-9]{8}', symbol) is None or symbol[:6].rstrip() not in SYMBOLS or symbol[13:] == '00000000':
                raise ValueError('raw symbol')
            date = dt.datetime.strptime(symbol[6:12], '%y%m%d')
            if date.strftime('%y%m%d') != symbol[6:12]:
                raise ValueError('contract date')
    elif query['dataset'] == 'OPRA.PILLAR' and query['stype_in'] == 'parent':
        if query['schema'] not in {'definition', 'statistics'} or len(symbols) > 15 or any(s not in {v + '.OPT' for v in SYMBOLS} for s in symbols):
            raise ValueError('parent scope')
    elif query['dataset'] == 'EQUS.MINI' and query['stype_in'] == 'raw_symbol':
        if query['schema'] != 'mbp-1' or len(symbols) > 15 or any(s not in SYMBOLS for s in symbols):
            raise ValueError('stock scope')
    else:
        raise ValueError('dataset scope')
    full_window = query['start'] == '2026-04-06T00:00:00Z' and query['end'] == '2026-10-03T00:00:00Z'
    if (raw_opra or not full_window) and end - start > 86_400_000_000_000:
        raise ValueError('slice scope')
    # Match the Go struct field order, regardless of input JSON key order.
    wire = {key: query[key] for key in ['dataset', 'schema', 'symbols', 'stype_in', 'start', 'end']}
    wire.update(encoding='dbn', compression='zstd', stype_out='instrument_id', split_duration='day',
                split_symbols=False, pretty_px=False, pretty_ts=False, map_symbols=False, delivery='download')
    return json.dumps(wire, separators=(',', ':'), ensure_ascii=False).encode()


def job_matches(job, query, job_id, done=False):
    if job.get('id') != job_id or job.get('state') not in (['done'] if done else ['queued', 'processing', 'done']):
        return False
    if any(job.get(key) != query[key] for key in ['dataset', 'schema', 'stype_in']):
        return False
    fixed = {'stype_out': 'instrument_id', 'encoding': 'dbn', 'compression': 'zstd', 'split_duration': 'day'}
    if any(job.get(key) != value for key, value in fixed.items()):
        return False
    if job.get('symbols') not in [query['symbols'], ','.join(query['symbols'])]:
        return False
    if any(utc_ns(job.get(key)) != utc_ns(query[key]) for key in ['start', 'end']):
        return False
    if job.get('limit') is not None:
        return False
    if job.get('delivery') not in (None, 'download') or job.get('mode') not in (None, 'historical'):
        return False
    return all(job.get(key) is None or job.get(key) is False or (type(job.get(key)) is str and job[key] == 'false')
               for key in ['split_symbols', 'pretty_px', 'pretty_ts', 'map_symbols'])


def validate_batch_acquisition(entry, proof_path, receipt_path, manifest, verified, root):
    """Bind a compact acquired ledger entry to every original manifested source."""
    try:
        def source(name, expected=None):
            if not isinstance(name, str) or name not in verified or name not in manifest:
                raise ValueError('unmanifested source')
            path = Path(name)
            if not path.is_absolute():
                path = root / path
            if not path.is_file() or path.is_symlink() or path.resolve() != path.absolute():
                raise ValueError('unsafe source')
            actual = file_hash(path)
            if actual != manifest[name]['sha256'] or (expected is not None and actual != expected):
                raise ValueError('source digest')
            return path

        if entry.get('status') != 'acquired' or entry.get('endpoint') != '/batch.submit_job':
            return False, 'acquired batch required'
        proof = read_json(source(proof_path, entry.get('data_sha256')))
        receipt_file = source(receipt_path, proof.get('receipt_sha256'))
        receipt = read_json(receipt_file)
        if not isinstance(receipt, dict) or set(receipt) != RECEIPT_KEYS:
            return False, 'receipt envelope'
        if set(proof) != PROOF_KEYS or proof.get('schema_version') != 'AutoWheelBatchAcquisitionProofV1':
            return False, 'proof envelope'
        if proof['invoice_cost_verified'] is not False or proof['source_qualified'] is not False:
            return False, 'acquisition cannot assert qualification'
        now_ns = int(dt.datetime.now(dt.timezone.utc).timestamp() * 1_000_000_000)
        if not 0 < utc_ns(proof['verified_at']) <= now_ns + 120_000_000_000:
            return False, 'proof time'
        original = dict(entry)
        original['status'] = 'reserved'
        original.pop('data_sha256', None)
        original.pop('bytes', None)
        if proof['reservation'] != original:
            return False, 'immutable reservation'
        for code in CODE_FILES:
            source(code)
        verifier_path = native_verifier_path(proof)
        source(verifier_path, proof['native_verifier_sha256'])
        query_file = source(proof['query_path'], proof['query_file_sha256'])
        ack_file = source(proof['acknowledgement_path'], proof['acknowledgement_file_sha256'])
        ack_body = source(proof['ack_body_path'], proof['ack_body_sha256'])
        query, ack = read_json(query_file), read_json(ack_file)
        if not isinstance(ack, dict) or set(ack) != ACK_KEYS:
            return False, 'ack envelope'
        if query != proof['query'] or ack != proof['acknowledgement']:
            return False, 'original input'
        canonical = canonical_query(query)
        if hashlib.sha256(canonical).hexdigest() != entry['query_hash'] or hashlib.sha256(canonical + b'\0/batch.submit_job').hexdigest() != entry['operation_id']:
            return False, 'canonical identity'
        if any(ack.get(key) != entry[key] for key in ['operation_id', 'query_hash', 'endpoint', 'cost_nano', 'reserved_nano', 'at']):
            return False, 'ack reservation'
        if type(ack.get('status_code')) is not int or ack['status_code'] != 200 or not integer(ack.get('bytes'), 1, 1 << 20) or ack['bytes'] != ack_body.stat().st_size or ack.get('response_hash') != proof['ack_body_sha256']:
            return False, 'accepted ack body'
        if ack.get('job_id') != proof['job_id'] or not job_matches(read_json(ack_body), query, proof['job_id']):
            return False, 'original accepted job'
        if ack.get('state') != read_json(ack_body).get('state'):
            return False, 'ack state'
        directory = Path(proof['source_directory'])
        if not directory.is_absolute() or directory.is_symlink() or directory.resolve() != directory:
            return False, 'source directory'
        if receipt_file != directory / 'receipt.json':
            return False, 'receipt directory'
        job = read_json(source(str(directory / 'job-source.json'), proof['job_sha256']))
        plans = read_json(source(str(directory / 'files-source.json'), proof['file_list_sha256']))
        if not job_matches(job, query, proof['job_id'], done=True) or not integer(job.get('record_count'), 1) or job['record_count'] != proof['provider_record_count']:
            return False, 'done job scope or count'
        for key in ['operation_id', 'query_hash']:
            if receipt.get(key) != entry[key]:
                return False, 'receipt identity'
        if receipt.get('query') != query or receipt.get('job_id') != proof['job_id'] or receipt.get('state') != 'done' or receipt.get('all_verified') is not True:
            return False, 'receipt scope'
        if not utc_ns(entry['at']) <= utc_ns(receipt.get('observed_at')) <= utc_ns(receipt.get('completed_at')) <= utc_ns(proof['verified_at']):
            return False, 'acquisition chronology'
        for receipt_key, proof_key in [('ack_sha256', 'ack_body_sha256'), ('job_sha256', 'job_sha256'), ('file_list_sha256', 'file_list_sha256')]:
            if receipt.get(receipt_key) != proof[proof_key]:
                return False, 'receipt sources'
        files = proof['files']
        if not isinstance(files, list) or not 1 <= len(files) <= 4096 or files != receipt.get('files') or not isinstance(plans, list) or len(plans) != len(files):
            return False, 'complete file set'
        total, names, shards = 0, set(), []
        for member, plan in zip(files, plans):
            if not isinstance(member, dict) or set(member) != {'filename', 'bytes', 'sha256'}:
                return False, 'member keys'
            name, size, sha = member['filename'], member['bytes'], member['sha256']
            if not isinstance(name, str) or re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,254}', name) is None or name in names or name in {'receipt.json', 'job-source.json', 'files-source.json'}:
                return False, 'member name'
            if not integer(size, 0, 8 << 30) or not is_hash(sha):
                return False, 'member proof'
            if plan.get('filename') != name or type(plan.get('size')) is not int or plan['size'] != size or plan.get('hash') != 'sha256:' + sha:
                return False, 'provider member'
            if source(str(directory / name), sha).stat().st_size != size:
                return False, 'member bytecount'
            names.add(name)
            total += size
            if name.endswith('.' + query['schema'] + '.dbn.zst'):
                shards.append(member)
        if not shards or not integer(total, 1, 64 << 30) or not all(type(v) is int and v == total for v in [proof['total_bytes'], receipt.get('total_bytes'), entry.get('bytes')]):
            return False, 'complete bytes'
        native = proof['native']
        if not isinstance(native, dict) or set(native) != NATIVE_KEYS or native.get('schema_version') not in {'AutoWheelNativeBatchCompletenessV1', 'AutoWheelNativeBookCompletenessV1'} or native.get('decoder_version') != '0.71.0' or native.get('upgrade_policy') != 'AS_IS' or native.get('all_eof_verified') is not True:
            return False, 'native verifier'
        expanded_limit = book_expansion_limit(proof['provider_record_count']) if verifier_path.endswith('native_book_completeness.py') else 8 << 30
        rows = native.get('files')
        if not isinstance(rows, list) or len(rows) != len(shards):
            return False, 'native shard set'
        count = 0
        for row, member in zip(rows, shards):
            if not isinstance(row, dict) or set(row) != NATIVE_FILE_KEYS or row.get('filename') != member['filename'] or row.get('compressed_sha256') != member['sha256'] or type(row.get('compressed_bytes')) is not int or row['compressed_bytes'] != member['bytes']:
                return False, 'native member'
            if not is_hash(row.get('uncompressed_sha256')) or not integer(row.get('uncompressed_bytes'), 1, expanded_limit) or not integer(row.get('records'), 0) or not integer(row.get('native_version'), 1, 3) or row.get('eof_verified') is not True:
                return False, 'native EOF'
            count += row['records']
        if not integer(count, 1) or type(native.get('records')) is not int or native['records'] != count or type(proof['provider_record_count']) is not int or count != proof['provider_record_count']:
            return False, 'native count'
        return True, ''
    except (OSError, KeyError, ValueError, TypeError, AttributeError, OverflowError):
        return False, 'batch evidence unavailable or inconsistent'
