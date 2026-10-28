#!/usr/bin/env python3
"""Private, one-shot OpenD evidence collection; never a trading adapter.

Uses the official JSON framing protocol without the SDK's float/DataFrame
conversion. Raw JSON is evidence only: OpenD itself may serialize doubles.
"""
import argparse
import datetime as dt
from decimal import Decimal
import hashlib
import json
import os
from pathlib import Path
import re
import socket
import stat
import struct
import time
import uuid

HEADER = struct.Struct('<2sIBBII20s8s')
READ_PROTOCOLS = frozenset((1001, 2001, 2101, 2102, 2201, 2226))
MAX_BODY = 16 * 1024 * 1024
ADDRESS = ('127.0.0.1', 11112)


class AuditError(Exception):
    pass


def decode(raw):
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise AuditError('duplicate_json_key')
            result[key] = value
        return result

    def nonfinite(_):
        raise AuditError('nonfinite_json')

    try:
        value = json.loads(raw.decode('utf-8'), parse_float=Decimal,
                           parse_constant=nonfinite, object_pairs_hook=pairs)
    except (ValueError, UnicodeError, RecursionError):
        raise AuditError('invalid_json') from None
    if not isinstance(value, dict):
        raise AuditError('invalid_json_object')
    return value


def private_file(path):
    # The deployment seal is read without opening OAuth or operator credentials.
    path = Path(path)
    for parent in path.parents:
        if parent.is_symlink():
            raise AuditError('symlink_parent')
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                or info.st_mode & 0o077 or info.st_size > 4096):
            raise AuditError('unsafe_private_file')
        with os.fdopen(fd, 'rb', closefd=False) as stream:
            return stream.read(4097)
    finally:
        os.close(fd)


def binding(path):
    data = decode(private_file(path))
    if (set(data) != {'broker', 'region', 'deployment_id', 'account_id'}
            or data['broker'] != 'moomoo' or data['region'] != 'US'
            or data['deployment_id'] != 'autowheel-moomoo'):
        raise AuditError('wrong_deployment_binding')
    account = data['account_id']
    if (not isinstance(account, str) or not re.fullmatch(r'[1-9][0-9]{3,19}', account)
            or int(account) > 2**64 - 1):
        raise AuditError('invalid_account_binding')
    return data


class Client:
    def __init__(self, sock):
        self.sock = sock
        self.serial = 0

    def read(self, count, deadline):
        result = bytearray()
        while len(result) < count:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise AuditError('response_deadline')
            self.sock.settimeout(remaining)
            chunk = self.sock.recv(count - len(result))
            if not chunk:
                raise AuditError('truncated_response')
            result.extend(chunk)
        return bytes(result)

    def request(self, protocol, c2s):
        if type(protocol) is not int or protocol not in READ_PROTOCOLS:
            raise AuditError('protocol_not_read_only')
        body = json.dumps({'c2s': c2s}, separators=(',', ':'), allow_nan=False).encode()
        if len(body) > MAX_BODY:
            raise AuditError('request_too_large')
        self.serial += 1
        deadline = time.monotonic() + 10
        self.sock.settimeout(10)
        self.sock.sendall(HEADER.pack(b'FT', protocol, 1, 0, self.serial,
                                     len(body), hashlib.sha1(body).digest(), bytes(8)) + body)
        head = HEADER.unpack(self.read(HEADER.size, deadline))
        magic, actual_protocol, fmt, version, serial, size, digest, reserved = head
        if (magic != b'FT' or actual_protocol != protocol or fmt != 1
                or version != 0 or serial != self.serial or reserved != bytes(8)):
            raise AuditError('response_header_mismatch')
        if not 0 < size <= MAX_BODY:
            raise AuditError('response_too_large')
        raw = self.read(size, deadline)
        if hashlib.sha1(raw).digest() != digest:
            raise AuditError('response_digest_mismatch')
        return raw, decode(raw), hashlib.sha256(body).hexdigest()


def success(data):
    if type(data.get('retType')) is not int or data['retType'] != 0:
        raise AuditError('provider_read_rejected')
    if not isinstance(data.get('s2c'), dict):
        raise AuditError('response_data_missing')
    return data['s2c']


def account_id(value):
    if type(value) is int and value > 0:
        return str(value)
    if isinstance(value, str) and re.fullmatch(r'[1-9][0-9]{0,19}', value):
        return value
    raise AuditError('invalid_response_account')


def match_account(data, expected):
    rows = success(data).get('accList')
    if not isinstance(rows, list) or not all(isinstance(row, dict) for row in rows):
        raise AuditError('invalid_account_list')
    matches = [row for row in rows if account_id(row.get('accID')) == expected]
    if len(matches) != 1:
        raise AuditError('account_not_uniquely_matched')
    row = matches[0]
    markets = row.get('trdMarketAuthList')
    if (type(row.get('trdEnv')) is not int or row['trdEnv'] != 1
            or type(row.get('securityFirm')) is not int or row['securityFirm'] != 2
            or not isinstance(markets, list)
            or not all(type(market) is int for market in markets) or 2 not in markets):
        raise AuditError('account_not_real_futuinc_us')


def check_header(data, expected):
    actual = success(data).get('header')
    if (not isinstance(actual, dict)
            or set(actual) - {'trdEnv', 'accID', 'trdMarket', 'jpAccType'}
            or account_id(actual.get('accID')) != expected['accID']
            or type(actual.get('trdEnv')) is not int or actual['trdEnv'] != expected['trdEnv']
            or type(actual.get('trdMarket')) is not int or actual['trdMarket'] != expected['trdMarket']
            or 'jpAccType' in actual and (type(actual['jpAccType']) is not int or actual['jpAccType'] != 0)):
        raise AuditError('account_response_header_mismatch')


def now():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def write_new(path, raw):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'wb') as stream:
        stream.write(raw)
        stream.flush()
        os.fsync(stream.fileno())


def save_manifest(directory, manifest):
    temporary = directory / ('.manifest-' + str(uuid.uuid4()))
    write_new(temporary, (json.dumps(manifest, indent=2) + '\n').encode())
    os.replace(temporary, directory / 'manifest.json')
    fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def evidence_directory(root):
    root = Path(root)
    # Refuse symlink components before mkdir as well as on the resulting path.
    for parent in (root, *root.parents):
        if parent.is_symlink():
            raise AuditError('symlink_evidence_directory')
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    for path in (root.parent, root):
        info = path.stat()
        if info.st_uid != os.getuid() or info.st_mode & 0o077:
            raise AuditError('unsafe_evidence_directory')
    directory = root / str(uuid.uuid4())
    directory.mkdir(mode=0o700)
    return directory


def collect(client, bound, date, directory, manifest):
    # InitConnect can include session encryption keys; never retain its payload.
    _, response, _ = client.request(1001, {'clientVer': 100,
        'clientID': 'autowheel-read-only-audit', 'recvNotify': False, 'pushProtoFmt': 1})
    success(response)

    def capture(name, protocol, request):
        started = now()
        raw, data, request_hash = client.request(protocol, request)
        write_new(directory / (name + '.json'), raw)
        manifest['reads'].append({'name': name, 'protocol_id': protocol,
            'started_at': started, 'finished_at': now(),
            'request_sha256': request_hash, 'response_sha256': hashlib.sha256(raw).hexdigest()})
        save_manifest(directory, manifest)
        return data

    account = bound['account_id']
    accounts = capture('accounts', 2001, {'userID': '0', 'needGeneralSecAccount': True})
    match_account(accounts, account)
    header = {'trdEnv': 1, 'accID': account, 'trdMarket': 2}
    requests = [
        ('funds', 2101, {'header': header, 'refreshCache': True, 'currency': 2}),
        ('positions', 2102, {'header': header, 'refreshCache': True}),
        ('orders', 2201, {'header': header, 'refreshCache': True}),
        ('cash-flow', 2226, {'header': header, 'clearingDate': date, 'cashFlowDirection': 0}),
    ]
    for name, protocol, request in requests:
        check_header(capture(name, protocol, request), header)
    manifest['complete'] = True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--clearing-date', required=True, help='One explicit US clearing date (YYYY-MM-DD)')
    args = parser.parse_args()
    try:
        date = dt.date.fromisoformat(args.clearing_date)
        if date.isoformat() != args.clearing_date:
            raise ValueError()
    except ValueError:
        parser.error('clearing date must be YYYY-MM-DD')
    os.umask(0o077)
    directory = None
    manifest = None
    try:
        bound = binding(Path.home() / '.local/state/autowheel-moomoo/moomoo-deployment.json')
        directory = evidence_directory(Path.home() / '.local/state/autowheel-moomoo-opend/evidence')
        manifest = {'source': 'moomoo-opend-json', 'address': '127.0.0.1:11112',
            'binding': bound, 'clearing_date': args.clearing_date, 'started_at': now(),
            'complete': False, 'execution_qualified': False, 'reads': [],
            'warning': 'Non-atomic raw evidence; source floating-point semantics, coverage and settlement remain unqualified.'}
        save_manifest(directory, manifest)
        with socket.create_connection(ADDRESS, timeout=10) as sock:
            collect(Client(sock), bound, args.clearing_date, directory, manifest)
        manifest['finished_at'] = now()
        save_manifest(directory, manifest)
        print('Read-only OpenD evidence captured; execution remains unqualified.')
        print(directory)
        return 0
    except (AuditError, OSError, ValueError, RecursionError) as error:
        if directory is not None and manifest is not None:
            manifest['complete'] = False
            manifest['finished_at'] = now()
            manifest['error'] = str(error) if isinstance(error, AuditError) else 'transport_or_filesystem_failure'
            try:
                save_manifest(directory, manifest)
            except OSError:
                pass  # The last durable manifest already says incomplete.
        print('OpenD qualification read failed; no trading readiness was granted.')
        if directory is not None:
            print(directory)
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
