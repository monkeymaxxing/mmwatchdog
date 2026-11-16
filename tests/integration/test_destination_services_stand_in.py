"""The service fixture answers quiet verification without counting a delivery."""
import http.client
import json
from pathlib import Path
import tempfile
import threading
import unittest
from unittest.mock import patch

import destination_services_stand_in as services


class DestinationVerificationTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        replacement = patch.object(services, 'ROOT', self.root)
        replacement.start()
        self.addCleanup(replacement.stop)
        self.server = services.ThreadingHTTPServer(('127.0.0.1', 0), services.Handler)
        thread = threading.Thread(target=self.server.serve_forever)
        thread.start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(thread.join)
        self.addCleanup(self.server.shutdown)

    def request(self, method, path, payload=None, headers=None):
        headers = dict(headers or {})
        if isinstance(payload, dict):
            payload = json.dumps(payload)
            headers['Content-Type'] = 'application/json'
        connection = http.client.HTTPConnection(*self.server.server_address)
        try:
            connection.request(method, path, payload, headers)
            response = connection.getresponse()
            return response.status, response.read().decode()
        finally:
            connection.close()

    def test_all_verification_requests_accept_valid_and_refuse_bad_credentials(self):
        discord = services.DISCORD_WEBHOOK_PATH.replace('/api/', '/api/v10/')
        requests = [
            ('slack', 'POST', services.SLACK_WEBHOOK_PATH, 'mmwatchdog verify', {'Content-Type': 'application/json'}, 400, 'invalid_payload', '/services/removed'),
            ('telegram', 'POST', '/bot' + services.TELEGRAM_BOT_TOKEN + '/getChat', {'chat_id': services.TELEGRAM_CHAT_ID}, {}, 200, '"ok": true', '/botwrong/getChat'),
            ('pushover', 'POST', '/1/users/validate.json', 'token=' + services.PUSHOVER_APPLICATION_TOKEN + '&user=' + services.PUSHOVER_USER_OR_GROUP_KEY, {'Content-Type': 'application/x-www-form-urlencoded'}, 200, '"status": 1', None),
            ('discord', 'GET', discord, None, {}, 200, '"type": 1', discord + '-wrong'),
            ('mattermost', 'POST', services.MATTERMOST_WEBHOOK_PATH, {'text': 'MMWatchdog checks this webhook. Mattermost does not post this text.', 'channel': 'mmwatchdog!verify'}, {}, 404, '"status_code": 404', '/hooks/removed'),
        ]
        for service, method, path, payload, headers, expected_status, expected_body, bad_path in requests:
            with self.subTest(service=service):
                status, body = self.request(method, path, payload, headers)
                self.assertEqual(status, expected_status, body)
                self.assertIn(expected_body, body)
                if service == 'pushover':
                    bad = self.request(method, path, 'token=wrong&user=' + services.PUSHOVER_USER_OR_GROUP_KEY, headers)
                    self.assertEqual(bad[0], 400, bad)
                    self.assertIn('"token": "invalid"', bad[1])
                else:
                    bad = self.request(method, bad_path, payload, headers)
                    self.assertGreaterEqual(bad[0], 400, bad)
                    self.assertLess(bad[0], 500, bad)
        self.assertFalse((self.root / 'destination-services-stand-in.jsonl').exists(), 'verification was recorded as a delivery')
        self.assertEqual(len((self.root / 'destination-services-verifications.jsonl').read_text().splitlines()), 2 * len(requests))
