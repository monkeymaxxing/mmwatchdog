#!/usr/bin/env python3
"""The web application of the web service fleet example: a small stand-in
for a real web application, which the example's MMWatchdog watches. nginx serves it at
http://app.example.com/ and passes these paths to it on 127.0.0.1:8000:

  /health            "ready", or HTTP 503 "maintenance" while the file
                     /var/lib/web-application/maintenance-mode exists
  /internal/status   the status page, behind nginx's basic authentication:
                     active_workers and stalled_workers, which the file
                     /var/lib/web-application/stalled-workers holds
  /allocate          takes memory and keeps it, as a leak would, until the
                     machine's memory is used to ?until_memory_used_percent=
"""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

STATE = Path('/var/lib/web-application')
# What the leak holds, so that the memory stays in use.
leaked_memory = []


def memory_kibibytes(field):
    for line in Path('/proc/meminfo').read_text().splitlines():
        name, value = line.split(':', 1)
        if name == field:
            return int(value.split()[0])
    raise KeyError(field)


def memory_used_percent():
    """As MMWatchdog's node_resource memory measures it: 100 x (1 - MemAvailable / MemTotal)."""
    return 100 * (1 - memory_kibibytes('MemAvailable') / memory_kibibytes('MemTotal'))


class Handler(BaseHTTPRequestHandler):
    def answer(self, status, text):
        body = text.encode()
        self.send_response(status)
        self.send_header('Content-Type', 'text/plain; charset=utf-8')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        url = urlparse(self.path)
        if url.path == '/health':
            if (STATE / 'maintenance-mode').exists():
                self.answer(503, 'maintenance\n')
            else:
                self.answer(200, 'ready\n')
        elif url.path == '/internal/status':
            stalled = (STATE / 'stalled-workers').read_text().strip()
            self.answer(200, f'active_workers: 4\nstalled_workers: {stalled}\n')
        elif url.path == '/allocate':
            target = float(parse_qs(url.query)['until_memory_used_percent'][0])
            # 64 MiB at a time, each byte written, so that the memory is in use.
            while memory_used_percent() < target:
                leaked_memory.append(bytearray(b'\x01') * (64 * 1024 * 1024))
            self.answer(200, f'memory_used_percent is {memory_used_percent():.1f}\n')
        else:
            self.answer(404, 'not found\n')

    def log_message(self, *args):
        pass


ThreadingHTTPServer(('127.0.0.1', 8000), Handler).serve_forever()
