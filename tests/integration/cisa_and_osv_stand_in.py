#!/usr/bin/env python3
"""HTTPS stand-in for CISA's list of known exploited vulnerabilities and for
OSV.dev, using only the Python standard library. It answers both host names,
raw.githubusercontent.com and api.osv.dev, from answers.json, which the test replaces
between runs:

- cisa: the status and body of CISA's list.
- osv_status: when not 200, the status of every answer to a batch.
- flaws: for each ecosystem and source package, the flaw IDs of OSV.dev.
- records: each record that /v1/vulns/<id> returns.
- ids_per_page: how many flaw IDs one query gets on one page; a query with
  more gets a next_page_token, as OSV.dev answers.

It appends each batch body to batches.jsonl. All files, including
server.crt, server.key, and the ready marker, live beside this script on
the guest. It listens on 127.0.0.1:443.
"""
import http.server
import json
import pathlib
import ssl

root = pathlib.Path(__file__).parent
CISA_PATH = '/cisagov/kev-data/main/known_exploited_vulnerabilities.json'


class CISAAndOSV(http.server.BaseHTTPRequestHandler):
    def host(self):
        """The host name that the request asks for, without a port."""
        return self.headers.get('Host', '').split(':')[0]

    def answer(self, status, body):
        encoded = json.dumps(body).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    def do_GET(self):
        answers = json.loads((root / 'answers.json').read_text())
        host = self.host()
        if host == 'raw.githubusercontent.com' and self.path == CISA_PATH:
            self.answer(answers['cisa']['status'], answers['cisa']['body'])
        elif host == 'api.osv.dev' and self.path.startswith('/v1/vulns/'):
            record = answers['records'].get(self.path.removeprefix('/v1/vulns/'))
            if record:
                self.answer(200, record)
            else:
                self.answer(404, {'code': 5, 'message': 'Bug not found.'})
        else:
            self.answer(404, {'message': 'not a path of the stand-in: ' + host + self.path})

    def do_POST(self):
        answers = json.loads((root / 'answers.json').read_text())
        body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
        if self.host() != 'api.osv.dev' or self.path != '/v1/querybatch' or self.headers.get('Content-Type') != 'application/json':
            self.answer(404, {'message': 'not a path of the stand-in'})
            return
        with (root / 'batches.jsonl').open('ab') as batches:
            batches.write(body + b'\n')
        if answers['osv_status'] != 200:
            self.answer(answers['osv_status'], {'code': 14, 'message': 'The test makes OSV.dev unavailable.'})
            return
        results = []
        for query in json.loads(body)['queries']:
            ids = answers['flaws'].get(query['package']['ecosystem'], {}).get(query['package']['name'], [])
            start = int(query.get('page_token', 'after-0').removeprefix('after-'))
            end = min(start + answers['ids_per_page'], len(ids))
            result = {}
            if end > start:
                result['vulns'] = [{'id': flaw, 'modified': '2026-10-26T07:05:12Z'} for flaw in ids[start:end]]
            if end < len(ids):
                result['next_page_token'] = 'after-' + str(end)
            results.append(result)
        self.answer(200, {'results': results})


def main():
    server = http.server.HTTPServer(('127.0.0.1', 443), CISAAndOSV)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(root / 'server.crt', root / 'server.key')
    server.socket = context.wrap_socket(server.socket, server_side=True)
    (root / 'ready').touch()
    server.serve_forever()


if __name__ == '__main__':
    main()
