"""The email destination against loopback-only Mailpit servers inside each guest."""
from datetime import datetime, timezone
import json
import shlex
import subprocess
import tarfile
import time
import vm

MAILPIT_RELEASE = '1.31.6'
MAILPIT_ARCHIVE_SHA256 = {
    'amd64': '10fa12ccc610f653d4ed4df848c1817f9935dcf4e9a6a458a19ab0fa02093f91',
    'arm64': 'de07955b783236d9129da76da9a5970f1777834a18b2c07611de957be784dafe',
}
# Every caller uses this table. Each server enforces one connection mode,
# so the accepting SMTP port proves the connection security of a delivery.
MAILPIT_SERVERS = {
    'starttls_with_login': {'smtp_port': 18025, 'http_api_port': 18081,
                            'required_connection_flags': ['--smtp-require-starttls']},
    'implicit_tls_with_login': {'smtp_port': 18465, 'http_api_port': 18082,
                                'required_connection_flags': ['--smtp-require-tls']},
    'unencrypted_without_login': {'smtp_port': 18026, 'http_api_port': 18083,
                                  'required_connection_flags': []},
}
FIXTURE = '/var/tmp/mmwatchdog-fixture'
MAILPIT_BINARY = '/usr/local/bin/mmwatchdog-test-mailpit'
MAILPIT_CERTIFICATE = FIXTURE + '/mailpit-certificate.pem'
MAILPIT_PRIVATE_KEY = FIXTURE + '/mailpit-private-key.pem'
MAILPIT_AUTH_FILE = FIXTURE + '/mailpit-auth'
# MMWatchdog verifies the server's localhost certificate with the guest's
# system trust store. Start the daemon after installing the certificate.
CERTIFICATE_OF_THE_TEST = 'mmwatchdog-mail-test'
SMTP_LOGIN_USER, SMTP_LOGIN_PASSWORD = 'mmwatchdog-alerts', 'mailpit-test-password'
PASSWORD_FILE = '/etc/mmwatchdog/secrets/smtp-password'
SENDER = 'mmwatchdog@node.example.com'


def start_mail_servers(guests=(vm.NAME,)):
    """Install one Mailpit binary and start three transient units in each guest, with a localhost certificate trusted by that guest."""
    archive = vm.download_release_archive(
        'https://github.com/axllent/mailpit/releases/download/v' + MAILPIT_RELEASE
        + '/mailpit-linux-' + vm.ARCH + '.tar.gz', MAILPIT_ARCHIVE_SHA256[vm.ARCH])
    # Extract the binary here and copy it through the SSH connection.
    with tarfile.open(archive) as bundle:
        binary = bundle.extractfile('mailpit').read()
    for guest in guests:
        subprocess.run(vm.ssh_args(guest) + ['mkdir -p ' + FIXTURE + ' && cat > ' + MAILPIT_BINARY + ' && chmod 0755 ' + MAILPIT_BINARY],
                       input=binary, check=True, capture_output=True, timeout=60)
        vm.console('openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=localhost -addext subjectAltName=DNS:localhost'
                   ' -keyout ' + MAILPIT_PRIVATE_KEY + ' -out ' + MAILPIT_CERTIFICATE
                   + ' >/dev/null 2>&1 && chmod 0600 ' + MAILPIT_PRIVATE_KEY, guest=guest)
        vm.trust_certificate(CERTIFICATE_OF_THE_TEST, vm.console('cat ' + MAILPIT_CERTIFICATE, guest=guest).stdout, guest=guest)
        vm.write(MAILPIT_AUTH_FILE, SMTP_LOGIN_USER + ':' + SMTP_LOGIN_PASSWORD + '\n', '0600', guest=guest)
        start_mailpit_processes(guest=guest)


def start_mailpit_processes(guest=vm.NAME):
    """Start the three servers from installed files, also after a fleet guest reboot."""
    for server_name, server in MAILPIT_SERVERS.items():
        unit = 'mmwatchdog-mailpit-' + server_name.replace('_', '-')
        arguments = [MAILPIT_BINARY, '--smtp', '127.0.0.1:' + str(server['smtp_port']),
                     '--listen', '127.0.0.1:' + str(server['http_api_port']),
                     '--disable-version-check', '--smtp-disable-rdns']
        if server['required_connection_flags']:
            arguments += ['--smtp-tls-cert', MAILPIT_CERTIFICATE, '--smtp-tls-key', MAILPIT_PRIVATE_KEY,
                          '--smtp-auth-file', MAILPIT_AUTH_FILE, *server['required_connection_flags']]
        vm.console('systemd-run --quiet --collect --unit ' + unit + ' ' + shlex.join(arguments), guest=guest)
        ready = 'python3 -c ' + shlex.quote(
            'import urllib.request; urllib.request.urlopen("http://127.0.0.1:'
            + str(server['http_api_port']) + '/readyz", timeout=1).read()')
        deadline = time.monotonic() + 10
        while vm.console(ready, check=False, guest=guest).returncode:
            assert time.monotonic() < deadline, vm.console('journalctl -u ' + unit + ' -n 20 --no-pager', check=False, guest=guest).stdout
            time.sleep(0.2)


def stop_mail_servers(guests=(vm.NAME,)):
    """Stop the transient units, which deletes Mailpit's temporary databases, and remove installed files and trust."""
    units = ['mmwatchdog-mailpit-' + name.replace('_', '-') for name in MAILPIT_SERVERS]
    for guest in guests:
        vm.console(vm.each_unit('stop', units), guest=guest)
        vm.console('rm -f ' + ' '.join([MAILPIT_BINARY, MAILPIT_CERTIFICATE, MAILPIT_PRIVATE_KEY, MAILPIT_AUTH_FILE]), guest=guest)
        vm.distrust_certificate(CERTIFICATE_OF_THE_TEST, guest=guest)


def received(guest=vm.NAME):
    """Read all messages through each guest-local API, oldest first, retaining the scenario's envelope and decoded message fields."""
    # The guests have Python, but do not necessarily have curl. Fetch raw
    # headers too: API-decoded text alone cannot prove the MIME encoding.
    program = 'servers = ' + repr(list(MAILPIT_SERVERS.values())) + '\n' + r'''import email
from email import policy
import json
import urllib.request
records = []
for server in servers:
    base = 'http://127.0.0.1:' + str(server['http_api_port'])
    def read(path):
        with urllib.request.urlopen(base + path, timeout=5) as response:
            return response.read()
    # A suite sends far fewer messages than one page holds.
    page = json.loads(read('/api/v1/messages?limit=1000'))
    assert len(page['messages']) == page['total'], page['total']
    for summary in page['messages']:
        path = '/api/v1/message/' + summary['ID']
        detail = json.loads(read(path))
        message = email.message_from_bytes(read(path + '/raw'), policy=policy.default)
        assert (message.get_content_type(), message.get_content_charset(), message['Content-Transfer-Encoding']) == ('text/plain', 'utf-8', 'quoted-printable'), detail
        record = {
            'port': server['smtp_port'], 'mail_from': detail['ReturnPath'],
            'recipients': [person['Address'] for field in ('To', 'Cc', 'Bcc') for person in detail[field]],
            'logged_in_as': detail['Username'] or None,
            'subject': detail['Subject'], 'text': detail['Text'].replace('\r\n', '\n'),
            'to': ', '.join(person['Address'] for person in detail['To']),
            'sender': detail['From']['Address'], 'message_id': '<' + detail['MessageID'] + '>',
        }
        records.append((summary['Created'], record))
print(json.dumps(records))
'''
    records = json.loads(vm.cmd(vm.ssh_args(guest) + ['python3 -'], input=program).stdout)
    return [record for _, record in sorted(records, key=lambda item: datetime.fromisoformat(item[0]))]


def alert_time(event):
    """The time of an alert as an email's text gives it."""
    return datetime.fromisoformat(event['at']).astimezone(timezone.utc).strftime('%Y-%m-%d %H:%M:%S') + ' UTC'


def suite(cli, state, events, wait, mark, alerts):
    """alerts is the content of alert_destinations.yml before and after."""
    node = vm.hostname()
    fragment = '/etc/mmwatchdog/check_groups/email.yml'
    candidate = '/var/tmp/mmwatchdog-fixture/email-config'
    username, password = SMTP_LOGIN_USER, SMTP_LOGIN_PASSWORD
    starttls_server = 'localhost:%d' % MAILPIT_SERVERS['starttls_with_login']['smtp_port']

    def by_subject(subject):
        return [m for m in received() if m['subject'] == subject]

    def to(messages, *recipients):
        [message] = [m for m in messages if m['recipients'] == list(recipients)]
        return message

    def destination(settings, **more):
        return {'remind_every': 'never', 'email': {'from': SENDER, **settings}, 'time_limit_per_delivery_attempt': '10s', 'alert_severities': [], **more}

    # Four email destinations that take no severity, so only the check and
    # the notices below reach them: a mailbox behind STARTTLS and a login, an
    # address that turns each email into an SMS and so takes only the title,
    # the mail server of this same machine without encryption or a login, and
    # a mailbox behind a connection that is encrypted from its first byte.
    login = {'login_user': username, 'login_password_file': PASSWORD_FILE}
    mailbox = {'smtp_server': starttls_server, 'smtp_connection_security': 'starttls', 'to': ['oncall@example.com', 'ops@example.com'], **login}
    destinations = dict(
        alerts['destinations'],
        mailbox=destination(mailbox),
        on_call_sms=destination(dict(mailbox, to=['15551234567@sms-gateway.example.com']), send_only_the_alert_title=True),
        local_mail=destination({'smtp_server': 'localhost:%d' % MAILPIT_SERVERS['unencrypted_without_login']['smtp_port'], 'smtp_connection_security': 'unencrypted', 'to': ['root@localhost']}),
        mailbox_implicit_tls=destination(dict(mailbox, smtp_server='localhost:%d' % MAILPIT_SERVERS['implicit_tls_with_login']['smtp_port'],
                                              smtp_connection_security='implicit_tls', to=['implicit@example.com'])),
    )
    try:
        start_mail_servers()
        # The running daemon read the system trust store before the mail
        # server's certificate was in it.
        vm.console('systemctl restart mmwatchdog')
        wait(lambda: state('accepted/shell')['last_observation']['status'] == 'ok')
        vm.write(PASSWORD_FILE, password + '\n', '0600')
        vm.write('/etc/mmwatchdog/alert_destinations.yml', json.dumps({'destinations': destinations}, indent=2))
        vm.write(fragment, json.dumps({'check_groups': {'email_alert': {
            'check_every': '300ms', 'time_limit_per_run': '200ms', 'alert_after_failing_for': '0s', 'recovered_after_healthy_for': '0s',
            'shell': 'test ! -e /var/tmp/mmwatchdog-fixture/email-fail', 'alert_severity': 'warn',
            'send_alerts_only_to_destinations': ['mailbox', 'on_call_sms', 'local_mail']}}}))
        cli('reload', 0)
        wait(lambda: state('email_alert/shell')['state'] == 'ok')
        vm.console('touch /var/tmp/mmwatchdog-fixture/email-fail')
        failure = wait(lambda: len(m := by_subject('Warning: ' + node + ': email_alert/shell failed')) == 3 and m)
        event = next(e for e in events() if e.get('check_group') == 'email_alert' and e['type'] == 'failed')
        assert event['title'] == node + ': email_alert/shell failed' and event['title_with_alert_severity'] == 'Warning: ' + event['title'] and event['body'].startswith(event['message'] + '\nfailing since '), event
        closing_lines = 'Time of the alert: ' + alert_time(event) + '\nIncident ID: ' + event['incident_id'] + '\nEvent ID: ' + event['id'] + '\n'
        # One message for both recipients, over an encrypted connection and
        # after a login: the title with the alert's severity in front is the
        # subject, and the text is the body
        # followed by the time and the IDs.
        mailed = to(failure, 'oncall@example.com', 'ops@example.com')
        assert mailed['text'] == event['body'].rstrip('\n') + '\n\n' + closing_lines, (mailed, event)
        assert mailed['message_id'] == '<' + event['id'] + '@node.example.com>', mailed
        assert mailed['to'] == 'oncall@example.com, ops@example.com' and mailed['sender'] == SENDER and mailed['mail_from'] == SENDER, mailed
        # This server accepts mail only after STARTTLS, so arrival proves encryption.
        assert mailed['port'] == MAILPIT_SERVERS['starttls_with_login']['smtp_port'] and mailed['logged_in_as'] == username, mailed
        # The destination that asks for the title only gets no text at all.
        short = to(failure, '15551234567@sms-gateway.example.com')
        assert short['text'] == '' and short['message_id'] == mailed['message_id'] and short['port'] == MAILPIT_SERVERS['starttls_with_login']['smtp_port'] and short['logged_in_as'] == username, short
        # The mail server of the same machine needs no encryption and no login.
        local = to(failure, 'root@localhost')
        assert local['text'] == mailed['text'] and local['port'] == MAILPIT_SERVERS['unencrypted_without_login']['smtp_port'] and local['logged_in_as'] is None, local
        vm.console('rm /var/tmp/mmwatchdog-fixture/email-fail')
        recovery = wait(lambda: len(m := by_subject('Resolved warning: ' + node + ': email_alert/shell is ok again')) == 3 and m)
        recovered = next(e for e in events() if e.get('check_group') == 'email_alert' and e['type'] == 'recovered')
        mailed = to(recovery, 'oncall@example.com', 'ops@example.com')
        assert recovered['incident_id'] == event['incident_id'] and mailed['message_id'] == '<' + recovered['id'] + '@node.example.com>', (mailed, recovered)
        assert mailed['text'] == recovered['body'].rstrip('\n') + '\n\nTime of the alert: ' + alert_time(recovered) + '\nIncident ID: ' + event['incident_id'] + '\nEvent ID: ' + recovered['id'] + '\n', mailed
        assert to(recovery, '15551234567@sms-gateway.example.com')['text'] == '', recovery
        mark('an email destination gets a check\'s failure and its recovery over STARTTLS after a login, as one message for all its recipients: the title with the alert\'s severity in front is the subject, Warning for the failure and Resolved warning for the recovery, and the text is the body followed by the time, the incident ID, and the event ID; a destination that asks for the title only gets an email without text; the mail server of the same machine gets the alert without encryption and without a login')

        def send_notice(title, name, *more, code=0):
            return cli('send-notice --alert-severity info --title ' + shlex.quote(title) + ' --destination ' + name
                       + ''.join(' ' + shlex.quote(word) for word in more), code)

        def notice_event(title):
            return next(e for e in events() if e['type'] == 'notice' and e.get('title') == node + ': ' + title)

        title = 'Test of the email destination. Nothing is wrong.'
        sent = send_notice(title, 'mailbox', '--body', 'Sent by the acceptance suite.')
        assert sent.stdout == 'Notice sent to the destination mailbox:\n  mailbox   delivered\n', sent.stdout
        [notice] = by_subject('Info: ' + node + ': ' + title)
        recorded = notice_event(title)
        # A notice has no incident, so its email has no line for one.
        assert notice['text'] == 'Sent by the acceptance suite.\n\nTime of the alert: ' + alert_time(recorded) + '\nEvent ID: ' + recorded['id'] + '\n', notice
        assert notice['message_id'] == '<' + recorded['id'] + '@node.example.com>' and notice['recipients'] == ['oncall@example.com', 'ops@example.com'], notice
        # A connection encrypted from its first byte, and a title with
        # characters outside ASCII, which a mail program reads back as typed.
        title = 'Datenbank überlastet … bitte prüfen'
        sent = send_notice(title, 'mailbox_implicit_tls')
        assert sent.stdout == 'Notice sent to the destination mailbox_implicit_tls:\n  mailbox_implicit_tls   delivered\n', sent.stdout
        [notice] = by_subject('Info: ' + node + ': ' + title)
        # This server accepts mail only over TLS from the first byte.
        assert notice['port'] == MAILPIT_SERVERS['implicit_tls_with_login']['smtp_port'] and notice['logged_in_as'] == username, notice
        assert notice['recipients'] == ['implicit@example.com'] and notice['text'] == 'Time of the alert: ' + alert_time(notice_event(title)) + '\nEvent ID: ' + notice_event(title)['id'] + '\n', notice
        # A wrong password fails the first attempt, and the command says why.
        # MMWatchdog reads the password file for every delivery, so the queued notice
        # goes out once the file is right, with no reload.
        title = 'After a wrong password'
        vm.write(PASSWORD_FILE, 'not the password\n', '0600')
        failed = send_notice(title, 'mailbox', code=1)
        assert failed.stdout.startswith('Notice sent to the destination mailbox:\n  mailbox   failed: the mail server ' + starttls_server
                                        + ' refused the login of ' + username + ': 535 5.7.8 '), failed.stdout
        assert 'the notice was not delivered at the first attempt to 1 of 1 destinations. MMWatchdog keeps the notice queued and tries again' in failed.stderr, failed.stderr
        assert 'not the password' not in failed.stdout + failed.stderr and not by_subject('Info: ' + node + ': ' + title)
        vm.write(PASSWORD_FILE, password + '\n', '0600')
        arrived = wait(lambda: by_subject('Info: ' + node + ': ' + title))
        assert len(arrived) == 1 and arrived[0]['logged_in_as'] == username and arrived[0]['message_id'] == '<' + notice_event(title)['id'] + '@node.example.com>', arrived
        mark('mmwatchdog send-notice to an email destination prints delivered, also over a connection that is encrypted from its first byte and with a title outside ASCII; a wrong password fails the first attempt with the mail server\'s answer and exit code 1, and the queued notice arrives once the password file is right, without a reload')

        def validate(options, change):
            """Validate a copy of the configuration whose mailbox destination has these changed settings."""
            vm.write(candidate + '/alert_destinations.yml', json.dumps({'destinations': dict(destinations, mailbox=destination(dict(mailbox, **change)))}))
            return cli('validate ' + options + '--config-dir ' + candidate)

        vm.console('rm -rf ' + candidate + '; cp -a /etc/mmwatchdog ' + candidate)
        # A password file that is missing is found on the node, before any alert needs it.
        missing = {'login_password_file': '/etc/mmwatchdog/secrets/no-such-smtp-password'}
        accepted = validate('--config-only ', missing)
        assert accepted.returncode == 0, accepted.stderr
        refused = validate('', missing)
        assert refused.returncode == 1 and 'destinations.mailbox.email.login_password_file: ' in refused.stderr and 'no-such-smtp-password' in refused.stderr, refused.stderr
        # The operator can protect an unencrypted SMTP login with a private network.
        accepted = validate('', {'smtp_server': 'smtp.example.com:25', 'smtp_connection_security': 'unencrypted'})
        assert accepted.returncode == 0, accepted.stderr
        accepted = validate('', {})
        assert accepted.returncode == 0, accepted.stderr
        mark('mmwatchdog validate names the password file of an email destination when the file is missing; mmwatchdog validate accepts an unencrypted connection with a password to any mail server')
    finally:
        vm.write('/etc/mmwatchdog/alert_destinations.yml', json.dumps(alerts, indent=2))
        vm.console('rm -rf ' + fragment + ' ' + candidate + ' ' + PASSWORD_FILE + ' /var/tmp/mmwatchdog-fixture/email-fail')
        cli('reload')
        stop_mail_servers()
