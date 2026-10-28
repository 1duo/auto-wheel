#!/usr/bin/env python3
"""Targeted offline safety checks for the read-only OpenD collector."""
from decimal import Decimal
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import socket
import tempfile
import threading
import unittest

spec = importlib.util.spec_from_file_location('opend_audit', Path(__file__).with_name('moomoo-opend-audit.py'))
audit = importlib.util.module_from_spec(spec)
spec.loader.exec_module(audit)
BOUND = {'broker': 'moomoo', 'region': 'US', 'deployment_id': 'autowheel-moomoo', 'account_id': '123456'}


class TransportTest(unittest.TestCase):
    def exchange(self, raw, change=None):
        client, server = socket.socketpair()
        self.addCleanup(client.close)
        self.addCleanup(server.close)

        def respond():
            # The request is tiny; receive its exact frame before replying.
            reader = audit.Client(server)
            import time
            deadline = time.monotonic() + 2
            head = audit.HEADER.unpack(reader.read(audit.HEADER.size, deadline))
            reader.read(head[5], deadline)
            fields = [b'FT', head[1], 1, 0, head[4], len(raw), hashlib.sha1(raw).digest(), bytes(8)]
            if change:
                change(fields)
            server.sendall(audit.HEADER.pack(*fields) + raw)
            server.shutdown(socket.SHUT_WR)

        thread = threading.Thread(target=respond)
        thread.start()
        self.addCleanup(thread.join, 3)
        return audit.Client(client).request(2101, {'header': {}})

    def test_exact_source_decimal(self):
        raw = b'{"retType":0,"s2c":{"funds":{"cash":123.456789012}}}'
        received, parsed, _ = self.exchange(raw)
        self.assertEqual(received, raw)
        self.assertEqual(parsed['s2c']['funds']['cash'], Decimal('123.456789012'))

    def test_integrity_and_header_rejections(self):
        for index, wrong in [(0, b'XX'), (1, 2202), (2, 0), (3, 1), (4, 99),
                             (5, audit.MAX_BODY + 1), (6, bytes(20)), (7, b'x' * 8)]:
            with self.subTest(index=index), self.assertRaises(audit.AuditError):
                self.exchange(b'{"retType":0,"s2c":{}}', lambda h: h.__setitem__(index, wrong))

    def test_truncation(self):
        with self.assertRaises(audit.AuditError):
            self.exchange(b'{}', lambda h: h.__setitem__(5, 10))

    def test_write_protocol_never_sent(self):
        class NoIO:
            def __getattr__(self, _):
                raise AssertionError('forbidden protocol reached network')
        for protocol in (2005, 2202, 2205, 2227, True, '2101'):
            with self.subTest(protocol=protocol), self.assertRaises(audit.AuditError):
                audit.Client(NoIO()).request(protocol, {})


class EvidenceTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()

    def test_strict_json(self):
        for raw in (b'{"retType":0,"retType":1}', b'{"x":NaN}', b'{"x":Infinity}',
                    b'{}{}', b'[]', b'{"x":"\xff"}'):
            with self.subTest(raw=raw), self.assertRaises(audit.AuditError):
                audit.decode(raw)

    def test_private_seal(self):
        path = self.root / 'seal'
        audit.write_new(path, json.dumps(BOUND).encode())
        self.assertEqual(audit.binding(path), BOUND)
        path.chmod(0o640)
        with self.assertRaises(audit.AuditError):
            audit.binding(path)
        link = self.root / 'link'
        link.symlink_to(path)
        with self.assertRaises(OSError):
            audit.binding(link)

    def test_response_account_identity(self):
        expected = {'trdEnv': 1, 'accID': '123456', 'trdMarket': 2}
        audit.check_header({'retType': 0, 's2c': {'header': dict(expected, accID=123456)}}, expected)
        for change in ({'accID': '654321'}, {'trdEnv': 0}, {'trdEnv': True},
                       {'trdMarket': 1}, {'jpAccType': 1}):
            with self.subTest(change=change), self.assertRaises(audit.AuditError):
                audit.check_header({'retType': 0, 's2c': {'header': dict(expected, **change)}}, expected)

    def fake_client(self, account='123456'):
        class Fake:
            def __init__(self):
                self.calls = []

            def request(self, protocol, request):
                self.calls.append(protocol)
                if protocol == 1001:
                    data = {'retType': 0, 's2c': {'connAESKey': 'never-persist-this-key'}}
                elif protocol == 2001:
                    data = {'retType': 0, 's2c': {'accList': [{'accID': account, 'trdEnv': 1,
                        'securityFirm': 2, 'trdMarketAuthList': [2]}]}}
                else:
                    data = {'retType': 0, 's2c': {'header': request['header']}}
                raw = json.dumps(data).encode()
                return raw, data, 'fixture-request-hash'
        return Fake()

    def test_wrong_account_stops_before_scoped_reads(self):
        client = self.fake_client('654321')
        manifest = {'complete': False, 'reads': []}
        with self.assertRaises(audit.AuditError):
            audit.collect(client, BOUND, '2026-10-26', self.root, manifest)
        self.assertEqual(client.calls, [1001, 2001])
        self.assertFalse(manifest['complete'])

    def test_packet_fidelity_and_handshake_omission(self):
        client = self.fake_client()
        manifest = {'complete': False, 'reads': [], 'execution_qualified': False}
        audit.collect(client, BOUND, '2026-10-26', self.root, manifest)
        self.assertEqual(client.calls, [1001, 2001, 2101, 2102, 2201, 2226])
        self.assertTrue(manifest['complete'])
        self.assertFalse(manifest['execution_qualified'])
        for item in manifest['reads']:
            path = self.root / (item['name'] + '.json')
            self.assertEqual(hashlib.sha256(path.read_bytes()).hexdigest(), item['response_sha256'])
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        for path in self.root.iterdir():
            self.assertNotIn(b'never-persist-this-key', path.read_bytes())


if __name__ == '__main__':
    unittest.main()
