#!/usr/bin/env python3
"""A stand-in for five services that official extra modules deliver alerts
to, in the VM scenarios: Slack's incoming webhooks, Telegram's Bot API,
Pushover's Message API, Discord's webhooks, and Mattermost's incoming webhooks. Python standard library
only.

It listens on port 18470 over HTTPS and answers as the services document:

- Slack: a POST of JSON with a text to the webhook's address gets the status
  code 200 and the text "ok". An address that is not the webhook's gets 404
  and "no_service", as a removed webhook does. Every path that belongs to
  none of the other services is Slack's.
- Telegram: a POST of JSON to /bot<token>/sendMessage gets JSON with "ok":
  true. A wrong token gets 401 "Unauthorized", a chat that the bot does not
  know 400 "Bad Request: chat not found", and a text of more than 4,096
  UTF-16 code units 400 "Bad Request: message is too long".
- Pushover: a POST of a message to /1/messages.json gets JSON with
  "status": 1, and with a receipt for an emergency message. A POST of a form
  with the application token to /1/receipts/cancel_by_tag/<tag>.json stops
  the repeats of the emergency messages with that tag. A wrong application
  token gets 400 "application token is invalid", and a wrong user or group
  key 400 "user identifier is invalid".
- Discord: a POST of JSON to /api/v10/webhooks/<id>/<token>?wait=true gets
  200 and the message that Discord saved, with its id; without wait=true,
  204 and no text. A webhook that is not there gets 404 "Unknown Webhook",
  a wrong token 401 "Invalid Webhook Token", and a content of more than
  2,000 characters 400 "Invalid Form Body".

Verification uses Discord GET, Telegram getChat, Pushover user validation,
Slack malformed JSON, and Mattermost with an impossible channel.
The answers distinguish valid credentials from refused credentials without
accepting a message or event. Verification requests are recorded separately in
destination-services-verifications.jsonl.

Each delivery request is recorded as one line of destination-services-stand-in.jsonl: the service, the HTTP method, the path
with its query, the content type, the payload (JSON, or the fields of a
form), the status code of the answer, the address the connection came from,
and, for a connection from the guest itself, the account of the program
that opened it.
"""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import pwd
import socket
import ssl
import threading
from urllib.parse import parse_qs, parse_qsl, urlsplit

ROOT = Path('/var/tmp/mmwatchdog-fixture')
PORT = 18470
SLACK_WEBHOOK_PATH = '/services/T00000000/B00000000/SlackFixtureWebhookSecret001'
TELEGRAM_BOT_TOKEN = '123456789:AAE-TelegramFixtureBotToken_001'
TELEGRAM_CHAT_ID = '-1001234567890'
# The synthetic credentials below use the forms that their services
# give them: two keys of 30 letters and digits, and a webhook's number
# and token.
PUSHOVER_APPLICATION_TOKEN = 'PushoverFixtureAppToken0000042'
PUSHOVER_USER_OR_GROUP_KEY = 'PushoverFixtureGroupKey0000042'
DISCORD_WEBHOOK_ID = '123456789012345678'
DISCORD_WEBHOOK_TOKEN = 'mmwatchdogDiscordFixtureWebhookToken-Secret_1'
DISCORD_WEBHOOK_PATH = '/api/webhooks/' + DISCORD_WEBHOOK_ID + '/' + DISCORD_WEBHOOK_TOKEN
MATTERMOST_WEBHOOK_PATH = '/hooks/mattermostfixturehook00001'
lock = threading.Lock()


def slack(path, payload, headers, method):
    if path != SLACK_WEBHOOK_PATH:
        return 404, 'text/plain', 'no_service'
    if not isinstance(payload, dict):
        return 400, 'text/plain', 'invalid_payload'
    if not payload.get('text'):
        return 400, 'text/plain', 'no_text'
    return 200, 'text/plain', 'ok'


def telegram(path, payload, headers, method):
    def refused(code, description):
        return code, 'application/json', json.dumps({'ok': False, 'error_code': code, 'description': description})
    token, _, method = path[len('/bot'):].partition('/')
    if token != TELEGRAM_BOT_TOKEN:
        return refused(401, 'Unauthorized')
    if method not in ('sendMessage', 'getChat'):
        return refused(404, 'Not Found')
    if not isinstance(payload, dict) or str(payload.get('chat_id')) != TELEGRAM_CHAT_ID:
        return refused(400, 'Bad Request: chat not found')
    if method == 'getChat':
        return 200, 'application/json', json.dumps({'ok': True, 'result': {'id': int(TELEGRAM_CHAT_ID), 'type': 'supergroup', 'title': 'MMWatchdog fixture chat'}})
    text = payload.get('text')
    if not isinstance(text, str) or not text:
        return refused(400, 'Bad Request: message text is empty')
    if len(text.encode('utf-16-le')) // 2 > 4096:
        return refused(400, 'Bad Request: message is too long')
    result = {'message_id': 1, 'date': 0, 'chat': {'id': int(TELEGRAM_CHAT_ID), 'type': 'supergroup'}, 'text': text}
    return 200, 'application/json', json.dumps({'ok': True, 'result': result})


def pushover(path, payload, headers, method):
    def refused(error, **fields):
        return 400, 'application/json', json.dumps(dict(fields, errors=[error], status=0, request='5042853c-402d-4a18-abcb-168734a801de'))
    accepted = {'status': 1, 'request': '647d2300-702c-4b38-8b2f-d56326ae460b'}
    if not isinstance(payload, dict) or payload.get('token') != PUSHOVER_APPLICATION_TOKEN:
        return refused('application token is invalid, see https://pushover.net/api', token='invalid')
    if path.startswith('/1/receipts/cancel_by_tag/') and path.endswith('.json'):
        return 200, 'application/json', json.dumps(dict(accepted, canceled=1))
    if path not in ('/1/messages.json', '/1/users/validate.json'):
        return 404, 'application/json', json.dumps({'errors': ['not found'], 'status': 0})
    if payload.get('user') != PUSHOVER_USER_OR_GROUP_KEY:
        return refused('user identifier is invalid', user='invalid')
    if path == '/1/users/validate.json':
        return 200, 'application/json', json.dumps(dict(accepted, devices=['fixture-phone'], licenses=[]))
    if not payload.get('message'):
        return refused('message cannot be blank', message='cannot be blank')
    if payload.get('priority') == 2:
        if not isinstance(payload.get('retry'), int) or payload['retry'] < 30:
            return refused('retry is invalid', retry='invalid')
        if not isinstance(payload.get('expire'), int) or payload['expire'] > 10800:
            return refused('expire is invalid', expire='invalid')
        return 200, 'application/json', json.dumps(dict(accepted, receipt='rLqVNmUXW6nKXSf3WZibBoTL2ZsMa1'))
    return 200, 'application/json', json.dumps(accepted)


def discord(path, payload, headers, method):
    def refused(code, message, discord_code, **fields):
        return code, 'application/json', json.dumps(dict(fields, message=message, code=discord_code))
    address = urlsplit(path)
    parts = address.path.split('/')
    if parts[:4] != ['', 'api', 'v10', 'webhooks'] or len(parts) != 6:
        return refused(404, '404: Not Found', 0)
    if parts[4] != DISCORD_WEBHOOK_ID:
        return refused(404, 'Unknown Webhook', 10015)
    if parts[5] != DISCORD_WEBHOOK_TOKEN:
        return refused(401, 'Invalid Webhook Token', 50027)
    if method == 'GET':
        return 200, 'application/json', json.dumps({'id': DISCORD_WEBHOOK_ID, 'type': 1, 'channel_id': '1210000000000000002', 'name': 'MMWatchdog fixture', 'token': DISCORD_WEBHOOK_TOKEN})
    if not isinstance(payload, dict) or not payload.get('content'):
        return refused(400, 'Cannot send an empty message', 50006)
    if len(payload['content']) > 2000:
        return refused(400, 'Invalid Form Body', 50035, errors={'content': {'_errors': [{'code': 'BASE_TYPE_MAX_LENGTH', 'message': 'Must be 2000 or fewer in length.'}]}})
    if parse_qs(address.query).get('wait') != ['true']:
        return 204, 'text/plain', ''
    saved = {'id': '1290000000000000001', 'type': 0, 'content': payload['content'], 'channel_id': '1210000000000000002', 'webhook_id': DISCORD_WEBHOOK_ID,
             'flags': payload.get('flags', 0), 'mentions': [], 'mention_roles': [], 'mention_everyone': False}
    return 200, 'application/json', json.dumps(saved)


def mattermost(path, payload, headers, method):
    if path != MATTERMOST_WEBHOOK_PATH:
        status = 400
    elif isinstance(payload, dict) and payload.get('channel') == 'mmwatchdog!verify':
        status = 404
    else:
        return 200, 'text/plain', 'ok'
    return status, 'application/json', json.dumps({'id': 'web.incoming_webhook.general.app_error', 'message': 'Could not find the webhook or channel', 'status_code': status})


def is_verification(service, method, path, payload):
    if method == 'GET':
        return True
    if service == 'slack':
        return payload is None
    if service == 'telegram':
        return path.endswith('/getChat')
    if service == 'pushover':
        return path == '/1/users/validate.json'
    if service == 'mattermost':
        return isinstance(payload, dict) and payload.get('channel') == 'mmwatchdog!verify'
    return False


# The service of a request, by the start of its path. Slack comes last: it
# answers every other path, as Slack answers the address of a removed webhook.
SERVICES = [('/bot', 'telegram', telegram), ('/hooks/', 'mattermost', mattermost),
            ('/1/', 'pushover', pushover), ('/api/', 'discord', discord), ('/', 'slack', slack)]


def account_of_client(address, port):
    """The account of the program on this machine that opened the connection
    from address and port, from the kernel's table of TCP sockets, or None
    for a connection from another machine. The client's socket is in the
    table, established (01), while it waits for the answer."""
    try:
        local_address = '%08X:%04X' % (int.from_bytes(socket.inet_aton(address), 'little'), port)
        with open('/proc/net/tcp') as table:
            for line in table.readlines()[1:]:
                fields = line.split()
                if fields[1] == local_address and fields[3] == '01':
                    return pwd.getpwuid(int(fields[7])).pw_name
    except (OSError, KeyError):
        pass
    return None


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        self.answer_request()

    def do_POST(self):
        self.answer_request()

    def answer_request(self):
        data = self.rfile.read(int(self.headers.get('Content-Length', '0')))
        if self.headers.get('Content-Type') == 'application/x-www-form-urlencoded':
            payload = dict(parse_qsl(data.decode()))
        else:
            try:
                payload = json.loads(data)
            except ValueError:
                payload = None
        service, answer = next((service, answer) for start, service, answer in SERVICES if self.path.startswith(start))
        status, content_type, body = answer(self.path, payload, self.headers, self.command)
        log = 'destination-services-verifications.jsonl' if is_verification(service, self.command, self.path, payload) else 'destination-services-stand-in.jsonl'
        with lock, (ROOT / log).open('a') as f:
            f.write(json.dumps({'service': service, 'http_method': self.command, 'path': self.path, 'content_type': self.headers.get('Content-Type'), 'payload': payload,
                                'http_status_code': status, 'client_address': self.client_address[0],
                                'client_account': account_of_client(*self.client_address[:2])}) + '\n')
        self.send_response(status)
        self.send_header('Content-Type', content_type)
        self.send_header('Content-Length', str(len(body.encode())))
        self.end_headers()
        self.wfile.write(body.encode())

    def log_message(self, *args):
        pass


if __name__ == '__main__':
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(ROOT / 'destination-services-stand-in-cert.pem', ROOT / 'destination-services-stand-in-key.pem')
    server = ThreadingHTTPServer(('0.0.0.0', PORT), Handler)
    server.socket = context.wrap_socket(server.socket, server_side=True)
    server.serve_forever()
