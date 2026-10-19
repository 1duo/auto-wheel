"""Bounded read-only Theta capture; private Lexar originals, no retry/overwrite.

An HTTP receipt proves integrity, never market completeness or actual fills.
Every submitted query has an immutable intent; interrupted queries need review.
"""
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import fcntl
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

from rd1_storage import budget, canonical, require, root, sha, verify_storage, write

ENDPOINTS = {'/v3/stock/history/quote', '/v3/stock/history/trade', '/v3/stock/history/eod',
             '/v3/stock/at_time/trade',
             '/v3/option/at_time/quote',
             '/v3/option/history/quote', '/v3/option/list/contracts/quote',
             '/v3/calendar/year_holidays'}
MAX_BODY = 64 << 20


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args):
        return None


def now():
    return datetime.now(timezone.utc).isoformat()


def validate(query):
    require(set(query) == {'path', 'params'} and query['path'] in ENDPOINTS, 'nonhistorical endpoint')
    params = query['params']
    require(isinstance(params, dict) and params.get('format') == 'json'
            and all(type(k) is str and type(v) in (str, int) for k, v in params.items()), 'query encoding')
    if query['path'] == '/v3/calendar/year_holidays':
        require(set(params) == {'year', 'format'} and 2020 <= int(params['year']) <= 2027, 'calendar scope')
    else:
        require(re.fullmatch('[A-Z]{1,5}', params.get('symbol', '')) is not None, 'symbol scope')
        allowed = {'symbol','format','date','start_date','end_date','start_time','end_time',
                   'interval','venue','expiration','strike','right','max_dte','strike_range','time_of_day'}
        require(set(params) <= allowed and ('date' in params or {'start_date','end_date'} <= set(params)),
                'unbounded or unrecognized query')
        if query['path'] == '/v3/stock/history/quote':
            require(params.get('venue') == 'utp_cta' and params.get('interval') == '1m', 'stock SIP/interval')
        if query['path'] == '/v3/option/history/quote':
            require(params.get('interval') == '1m' and 'expiration' in params, 'option interval/identity')
        if query['path'] == '/v3/option/at_time/quote':
            require('expiration' in params and re.fullmatch(r'[0-9]{2}:[0-9]{2}:00(?:\.000)?',params.get('time_of_day','')),
                    'option as-of identity or minute boundary')


def run(spec):
    os.umask(0o077)
    run_root = root(spec['run_id'])
    # One RD1 source batch owns the shared initial two-request allowance. Hold
    # the lock across duplicate admission and all requests, including failures.
    fd = os.open(run_root/'capture.lock', os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        return capture_batch(spec, run_root)
    finally:
        os.close(fd)


def capture_batch(spec, run_root, *, budget_check=budget, maximum_concurrency=2, reviewed_query_hashes=(), maximum_queries=100):
    budget_check(run_root, spec['observation'], spec['timeout_seconds'])
    require(re.fullmatch('[a-z0-9-]{1,70}', spec['batch_id']) is not None, 'batch identity')
    require(type(maximum_concurrency) is int and 1 <= maximum_concurrency <= 4
            and type(spec['concurrency']) is int and 1 <= spec['concurrency'] <= maximum_concurrency,
            'source concurrency cap')
    queries = spec['queries']
    require(type(maximum_queries) is int and 1 <= maximum_queries <= 320, 'source batch cap')
    require(1 <= len(queries) <= maximum_queries and len({sha(canonical(q)) for q in queries}) == len(queries), 'duplicate/bounded queries')
    for q in queries:
        validate(q)
    # Refuse a previously attempted exact query across all RD1 capture batches.
    previous = set()
    for f in (run_root/'captures').glob('*/*.intent.json'):
        previous.add(sha(canonical(json.loads(f.read_bytes())['query'])))
    repeated=previous.intersection(sha(canonical(q)) for q in queries)
    require(repeated==set(reviewed_query_hashes), 'query already attempted; review its receipt')
    captures = run_root/'captures'; captures.mkdir(mode=0o700, exist_ok=True)
    directory = captures/spec['batch_id']; directory.mkdir(mode=0o700)
    write(directory/'plan.json', spec)
    started = time.monotonic()
    require(spec['timeout_seconds'] >= 70, 'timeout needs one complete transport timeout and write reserve')
    deadline = started + spec['timeout_seconds'] - 65
    transport_failed = threading.Event()
    def capture(item):
        i, query = item
        if transport_failed.is_set():
            return dict(index=i,status='not_started_transport_failure')
        if time.monotonic() >= deadline:
            return dict(index=i, status='not_started_deadline')
        verify_storage()
        write(directory/f'{i:03}.intent.json', dict(query=query, started_at=now()))
        path = directory/f'{i:03}.raw'
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        receipt = dict(index=i, query=query, http_status=None, bytes=0, complete=False, status='attempted')
        digest = hashlib.sha256(); response = None
        with os.fdopen(fd, 'wb') as f:
            try:
                opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
                url = 'http://127.0.0.1:25543'+query['path']+'?'+urllib.parse.urlencode(sorted(query['params'].items()))
                try:
                    response = opener.open(url, timeout=min(60, max(1, deadline-time.monotonic())))
                except urllib.error.HTTPError as error:
                    response = error
                receipt['http_status'] = response.status
                receipt['content_type'] = response.headers.get('Content-Type', '')
                length = response.headers.get('Content-Length')
                receipt['declared_content_length'] = int(length) if length is not None else None
                require(receipt['declared_content_length'] is None or receipt['declared_content_length'] >= 0,
                        'invalid response length')
                while time.monotonic() < deadline:
                    block = response.read(min(65536, MAX_BODY+1-receipt['bytes']))
                    if not block:
                        receipt['complete'] = True; break
                    f.write(block); digest.update(block); receipt['bytes'] += len(block)
                    if receipt['bytes'] > MAX_BODY:
                        break
                if receipt['complete'] and receipt['declared_content_length'] is not None and receipt['bytes'] != receipt['declared_content_length']:
                    receipt.update(complete=False, error_type='ContentLengthMismatch')
                receipt['status'] = ('captured' if response.status == 200 else 'http_error') if receipt['complete'] else 'incomplete'
            except (OSError, ValueError, urllib.error.URLError, http.client.HTTPException) as error:
                transport_failed.set()
                if isinstance(error, http.client.IncompleteRead):
                    partial = error.partial[:max(0, MAX_BODY+1-receipt['bytes'])]
                    f.write(partial); digest.update(partial); receipt['bytes'] += len(partial)
                receipt.update(status='transport_error', error_type=type(error).__name__)
            finally:
                if response is not None:
                    response.close()
                f.flush(); os.fsync(f.fileno())
        receipt.update(sha256=digest.hexdigest(), finished_at=now())
        require(sha(path.read_bytes()) == receipt['sha256'] and path.stat().st_size == receipt['bytes'], 'raw receipt differs')
        from rd1_storage import PHYSICAL, PARENT
        require(sha((PHYSICAL/path.relative_to(PARENT)).read_bytes()) == receipt['sha256'], 'physical raw differs')
        write(directory/f'{i:03}.receipt.json', receipt)
        print(json.dumps({k:receipt[k] for k in ('index','status','http_status','bytes')}), flush=True)
        return receipt
    with ThreadPoolExecutor(max_workers=spec['concurrency']) as pool:
        receipts = list(pool.map(capture, enumerate(queries)))
    summary = dict(schema='AutoWheelRD1CaptureBatchV1', batch_id=spec['batch_id'],
                   scheduled=len(queries), captured=sum(r['status']=='captured' for r in receipts),
                   results=receipts, wall_seconds=time.monotonic()-started, free_bytes=verify_storage(),
                   actual_fills_qualified=False, automatic_retries=False, financial_requests=0)
    write(directory/'summary.json', summary)
    print(json.dumps({k:summary[k] for k in ('batch_id','scheduled','captured','wall_seconds','free_bytes')}), flush=True)
    return summary


if __name__ == '__main__':
    run(json.load(sys.stdin))
