#!/usr/bin/env python3
"""Fake MMHotline for the fleet VM suite: authenticate requests and sign receipts.

Accepted alerts and check-ins are recorded by route in mmhotline.jsonl.
The root and HTTP port can be changed for a local protocol check.
"""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import hashlib
import hmac
import json
from pathlib import Path
import os
import re
import threading
import time

ROOT = Path(os.environ.get('MMHOTLINE_FIXTURE_ROOT', '/var/tmp/mmwatchdog-fixture'))
PORT = int(os.environ.get('MMHOTLINE_FIXTURE_PORT', '8484'))
MMHOTLINE_SECRET = bytes.fromhex((ROOT / 'mmhotline-secret').read_text().strip())
MMHOTLINE_ALERT_SOURCE_ID = 'mmwatchdog_fleet_hub'
lock = threading.Lock()


def now_text(offset=0):
    return time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime(time.time() + offset))


def record(route, body):
    with lock, (ROOT / 'mmhotline.jsonl').open('a') as f:
        f.write(json.dumps({'route': route, 'body': body}) + '\n')


class Handler(BaseHTTPRequestHandler):
    def answer(self, status, value, route=None, request_hmac=None):
        body = json.dumps(value).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        if route:
            timestamp = str(int(time.time()))
            signed = ('mmhotline-receipt.%s.%d.%s.%s.' % (route, status, request_hmac, timestamp)).encode() + body
            self.send_header('Mmhotline-Receipt-Signature', 'timestamp_unix_seconds=%s,hmac_sha256_hex=%s'
                             % (timestamp, hmac.new(MMHOTLINE_SECRET, signed, hashlib.sha256).hexdigest()))
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        data = self.rfile.read(int(self.headers.get('Content-Length', '0')))
        parts = self.path.split('/')
        signature = re.fullmatch(r'timestamp_unix_seconds=([0-9]+),hmac_sha256_hex=([0-9a-f]{64})',
                                 self.headers.get('Mmhotline-Signature', ''))
        if (len(parts) != 3 or parts[0] != '' or parts[1] not in ('alert-intake', 'check-in')
                or parts[2] != MMHOTLINE_ALERT_SOURCE_ID or signature is None):
            return self.answer(401, {'error': 'source authentication failed'})
        timestamp, request_hmac = signature.groups()
        expected = hmac.new(MMHOTLINE_SECRET, (timestamp + '.').encode() + data, hashlib.sha256).hexdigest()
        if abs(time.time() - int(timestamp)) > 300 or not hmac.compare_digest(expected, request_hmac):
            return self.answer(401, {'error': 'source authentication failed'})
        record(parts[1], json.loads(data))
        receipt = ({'mmhotline_alert_id': 'al_vm', 'already_received': False} if parts[1] == 'alert-intake'
                   else {'check_in_received_at': now_text()})
        self.answer(200, receipt, parts[1], request_hmac)

    def log_message(self, *args):
        pass


server = ThreadingHTTPServer(('0.0.0.0', PORT), Handler)
server.serve_forever()
