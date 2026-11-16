#!/usr/bin/env python3
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import ssl
import threading

ROOT=Path('/var/tmp/mmwatchdog-fixture')
CERT=Path('/etc/mmwatchdog/secrets/cert.pem')
lock=threading.Lock()
# The alerts that this receiver has taken, each by its path, its bearer
# token, and its Idempotency-Key: two destinations of a node can have one
# path and differ in their tokens. MMWatchdog sends an alert again when no answer
# came in time, with the same key, and a receiver that keeps to that
# contract takes it once.
taken=set()
class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path=='/private':
            allowed=self.headers.get('Host')=='app.internal' and self.headers.get('Authorization')=='Bearer mmwatchdog-integration-http-token'
            self.send_response(200 if allowed else 401)
            self.end_headers()
            self.wfile.write(b'ready' if allowed else b'denied')
            return
        failed=(ROOT/'fail').exists()
        self.send_response(503 if failed else 200)
        self.end_headers()
        self.wfile.write(b'broken' if failed else b'ready')
    def do_POST(self):
        data=self.rfile.read(int(self.headers.get('Content-Length','0')))
        rejected=(ROOT/'reject').exists()
        key=(self.path,self.headers.get('Authorization'),self.headers.get('Idempotency-Key'))
        with lock:
            again=key[2] is not None and key in taken
            if not again:
                with (ROOT/'webhooks.jsonl').open('a') as f:
                    f.write(json.dumps({'accepted':not rejected,'path':self.path,'authorization':self.headers.get('Authorization'),'event':json.loads(data)})+'\n')
                if not rejected:
                    taken.add(key)
        self.send_response(204 if again or not rejected else 503)
        self.end_headers()
    def log_message(self,*args): pass
# The acceptance suite's certificate also serves HTTPS, as a private CA would.
if CERT.exists():
    secure=ThreadingHTTPServer(('127.0.0.1',18443),Handler)
    context=ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(CERT,'/etc/mmwatchdog/secrets/key.pem')
    secure.socket=context.wrap_socket(secure.socket,server_side=True)
    threading.Thread(target=secure.serve_forever,daemon=True).start()
ThreadingHTTPServer(('0.0.0.0',18080),Handler).serve_forever()
