#!/usr/bin/env python3
"""A client of the MMWatchdog fleet hub's node API, for the fleet VM scenario.

It runs on a guest and sends requests to the hub as a joined node does: over
TLS, trusting only the certificate whose SHA-256 fingerprint the join code
names. The scenario uses it to run synthetic joined nodes, which join and
send alerts without an MMWatchdog of their own, and to send requests that the hub
must refuse with a joined node's own credential.

Usage: fleet_hub_client.py 'JOIN CODE' < requests.json

requests.json is one JSON list. Each request has:

  path             the hub's path: /v1/join, /v1/alerts, or /v1/report
  credential       the node credential that the request sends as its bearer
                   token, or
  credential_file  the hub-credential.json of a joined node, whose
                   credential the request sends
  body             a value that the client sends as JSON, or
  raw_body_parts   a list of [text, count] pairs: the body is each text
                   repeated count times, so that a request can be larger
                   than a limit of the hub without a large file

It prints one JSON list: for each request, its http_status_code and the
hub's answer, decoded when it is JSON. It uses only Python's standard library.
"""
import hashlib
import http.client
import json
import ssl
import sys


def post(address, fingerprint, request):
    host, port = address.rsplit(':', 1)
    # The hub's certificate is its own, signed by no certificate authority:
    # the join code names it by its fingerprint, which the client compares
    # before it sends anything.
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    context.check_hostname = False
    context.verify_mode = ssl.CERT_NONE
    connection = http.client.HTTPSConnection(host, int(port), context=context, timeout=30)
    try:
        connection.connect()
        if hashlib.sha256(connection.sock.getpeercert(binary_form=True)).hexdigest() != fingerprint:
            raise SystemExit('the hub at ' + address + ' has another certificate than the one that the join code names')
        if 'raw_body_parts' in request:
            body = ''.join(text * count for text, count in request['raw_body_parts']).encode()
        else:
            body = json.dumps(request['body']).encode()
        headers = {'Content-Type': 'application/json'}
        credential = request.get('credential')
        if 'credential_file' in request:
            with open(request['credential_file']) as f:
                credential = json.load(f)['credential']
        if credential:
            headers['Authorization'] = 'Bearer ' + credential
        connection.request('POST', request['path'], body=body, headers=headers)
        response = connection.getresponse()
        text = response.read().decode(errors='replace')
    finally:
        connection.close()
    try:
        answer = json.loads(text)
    except ValueError:
        answer = text
    return {'http_status_code': response.status, 'answer': answer}


def main():
    words = sys.argv[1].split()
    if len(words) != 4 or words[0] != 'mmwatchdog-join':
        raise SystemExit('expected a join code printed by mmwatchdog fleet join-code')
    address, fingerprint = words[1], words[2]
    print(json.dumps([post(address, fingerprint, request) for request in json.load(sys.stdin)]))


main()
