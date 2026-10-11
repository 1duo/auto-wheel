"""Free, fixed-endpoint metadata for capacity admission, never source absence."""
import base64
from datetime import datetime, timezone
import hashlib
import http.client
import importlib.util
import json
import os
from pathlib import Path
import ssl
import urllib.parse

from batch_acquisition_proof import canonical_query, file_hash

CAPACITY_HELPER_SHA = '53f7ce5c2e70dd131b17d397be2075f5f351cc04926c8e0833071ae5a4f5500d'
ENDPOINTS = {'record_count': '/v0/metadata.get_record_count',
             'billable_bytes': '/v0/metadata.get_billable_size'}
MAX_INT64 = (1 << 63) - 1


def save_raw(path, raw):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'wb') as stream:
        stream.write(raw);stream.flush();os.fsync(stream.fileno())
    fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def save(path, value):
    save_raw(path, (json.dumps(value, indent=2, sort_keys=True) + '\n').encode())


def load_key_helper():
    path = Path(__file__).with_name('estimate-replay-capacity.py')
    if path.resolve() != path or file_hash(path) != CAPACITY_HELPER_SHA:
        raise ValueError('original private metadata credential helper differs')
    spec = importlib.util.spec_from_file_location('original_capacity_key_helper', path)
    module = importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
    return module


def parse_integer(raw, status):
    if status != 200 or not 0 < len(raw) <= 4096:
        raise ValueError('free metadata response status or size')
    value = json.loads(raw)
    if type(value) is not int or not 0 <= value <= MAX_INT64:
        raise ValueError('free metadata integer bound')
    return value


def fetch(query, directory, *, connection_factory=http.client.HTTPSConnection):
    wire = canonical_query(query)
    if (query['dataset'], query['schema'], query['stype_in']) != ('OPRA.PILLAR', 'cmbp-1', 'raw_symbol'):
        raise ValueError('frozen option BOOK metadata scope required')
    helper = load_key_helper();helper.priv_dir(directory)
    key = helper.load_key()
    authorization = 'Basic ' + base64.b64encode((key + ':').encode()).decode();del key
    context = ssl.create_default_context();context.minimum_version = ssl.TLSVersion.TLSv1_2
    form = urllib.parse.urlencode({**{k: query[k] for k in ('dataset', 'schema', 'stype_in', 'start', 'end')},
                                  'symbols': ','.join(query['symbols'])}).encode()
    operation = hashlib.sha256(wire + b'\0/batch.submit_job').hexdigest()
    values, responses = {}, {}
    for label, endpoint in ENDPOINTS.items():
        # At most one request per endpoint in this attempt. No redirects, proxy,
        # paid range request, retry, financial reservation, or query limit.
        save(directory / (label + '-intent.json'), dict(endpoint=endpoint, operation_id=operation,
             query_hash=hashlib.sha256(wire).hexdigest(), request_attempts=1, financial_requests=0))
        connection = connection_factory('hist.databento.com', 443, context=context, timeout=45)
        try:
            connection.request('POST', endpoint, body=form, headers={'Authorization': authorization,
                               'Content-Type': 'application/x-www-form-urlencoded', 'Accept': 'application/json'})
            response = connection.getresponse();status = response.status;raw = response.read(4097)
        except Exception:
            save(directory / (label + '-failure.json'), dict(error_class='transport', automatic_retry=False))
            raise ValueError('free metadata transport stopped') from None
        finally:
            connection.close()
        if len(raw) <= 4096:
            save_raw(directory / (label + '-response.private.json'), raw)
        values[label] = parse_integer(raw, status)
        responses[label] = dict(http_status=status, endpoint=endpoint, bytes=len(raw),
                                sha256=hashlib.sha256(raw).hexdigest())
    report = dict(schema='AutoWheelDecisionWindowCapacityMetadataV1', query=query, operation_id=operation,
        query_hash=hashlib.sha256(wire).hexdigest(), observed_at=datetime.now(timezone.utc).isoformat(),
        **values, responses=responses, metadata_requests=2, financial_requests=0,
        short_window_metadata_may_overestimate=True, counts_are_not_native_completeness=True,
        source_absence_proven=False, source_qualified=False, automatic_retry=False,
        api_documentation='https://databento.com/docs/api-reference-historical?historical=http')
    save(directory / 'receipt.json', report)
    return report
