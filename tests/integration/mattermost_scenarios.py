"""The official extra module mattermost against a real Mattermost server in
the guest, as a part of the services suite on Debian and Ubuntu.

The server is Mattermost's Team Edition, which is open source: the release
archive that Mattermost publishes, with a database in the guest's own
PostgreSQL, so that no outside account is needed. The machine that runs the
suite downloads the archive once, checks it against the SHA-256 that
Mattermost publishes, and keeps it for every later run; each run copies it
into the guest. The test sets the server up without a click, through mmctl,
Mattermost's own command-line program, in its local mode: an administrator,
a team, a channel, two more people in it, and an incoming webhook. It reads
the channel's posts back, and how often each person was mentioned, through
Mattermost's REST API, with the login of the administrator."""
import itertools
import json
import re
import shlex
import subprocess
import time
import testenv
import vm

# The release of Mattermost that the test runs, and the SHA-256 that
# Mattermost publishes beside the archive of each processor architecture
# (https://releases.mattermost.com/<release>/<archive>.sha256).
MATTERMOST_RELEASE = '11.11.1'
MATTERMOST_ARCHIVE_SHA256 = {
    'amd64': 'c9237b0b54dce805a63e053c0a0516511bc8a2e47e9a06dd11050c2a0d79f616',
    'arm64': 'e3228c8cc6b5aedce835f68ae21ac5626473850aaf4d63003094de4b1350166b',
}
# The parts of the archive that the guest leaves out: the plugins that come
# with Mattermost, which the test turns off, and the web application for
# people's browsers. They are 450 of the archive's 640 MB.
LEFT_OUT_OF_THE_ARCHIVE = ['mattermost/prepackaged_plugins', 'mattermost/client']

FIXTURE = '/var/tmp/mmwatchdog-fixture'
MATTERMOST_DIRECTORY = FIXTURE + '/mattermost'
# The system account that runs the server, and the transient unit of the
# server, which clean_up stops after an interruption.
MATTERMOST_ACCOUNT = 'mmwatchdog-test-mattermost'
MATTERMOST_UNIT = 'mmwatchdog-mattermost'
# The name under which the guest reaches the server over HTTPS. /etc/hosts
# gives it the guest's own address, and the server's certificate, which the
# test makes, is in the guest's system trust store under the name of the
# test, as a real server's certificate of a public authority would be.
MATTERMOST_HOST = 'mattermost.mmwatchdog-test.internal'
MATTERMOST_SERVER = 'https://' + MATTERMOST_HOST + ':8065'
CERTIFICATE_OF_THE_TEST = 'mmwatchdog-mattermost-test'
MATTERMOST_CERTIFICATE = MATTERMOST_DIRECTORY + '/server-certificate.pem'
MATTERMOST_PRIVATE_KEY = MATTERMOST_DIRECTORY + '/server-private-key.pem'
# The Unix socket through which mmctl runs as an administrator, without a
# login: Mattermost's local mode.
LOCAL_MODE_SOCKET = MATTERMOST_DIRECTORY + '/local-mode.socket'
# The database of the server in the guest's PostgreSQL, and its role.
DATABASE_AND_ROLE = 'mattermost'
DATABASE_PASSWORD = 'mattermost-database-password-of-the-mmwatchdog-test'
# The people of the server: the administrator, who owns the webhook and reads
# the posts; the person whom the destination's mention names; and a person
# in the channel whom no alert names, whose count of mentions shows a
# mention that an alert's text made by itself.
ADMINISTRATOR, ON_CALL_PERSON, OTHER_CHANNEL_MEMBER = 'mmwatchdog-test-admin', 'on-call-person', 'other-channel-member'
PASSWORD_OF_EVERY_PERSON = 'password-of-the-mmwatchdog-mattermost-test-1'
TEAM, CHANNEL = 'mmwatchdog-test-team', 'alerts'
MENTION = '@' + ON_CALL_PERSON
# The settings of the server, as Mattermost takes them from its environment.
# The server speaks HTTPS itself, takes the incoming webhooks, and turns
# local mode on. It runs no plugin and asks no server of Mattermost's on the
# internet: no security alerts, no notices, no telemetry, and no push
# notifications.
MATTERMOST_SETTINGS = {
    'MM_SQLSETTINGS_DRIVERNAME': 'postgres',
    'MM_SQLSETTINGS_DATASOURCE': 'postgres://' + DATABASE_AND_ROLE + ':' + DATABASE_PASSWORD + '@127.0.0.1:5432/' + DATABASE_AND_ROLE + '?sslmode=disable&connect_timeout=10',
    'MM_SQLSETTINGS_MAXOPENCONNS': '20',
    'MM_SQLSETTINGS_MAXIDLECONNS': '10',
    'MM_SERVICESETTINGS_SITEURL': MATTERMOST_SERVER,
    'MM_SERVICESETTINGS_LISTENADDRESS': '127.0.0.1:8065',
    'MM_SERVICESETTINGS_CONNECTIONSECURITY': 'TLS',
    'MM_SERVICESETTINGS_TLSCERTFILE': MATTERMOST_CERTIFICATE,
    'MM_SERVICESETTINGS_TLSKEYFILE': MATTERMOST_PRIVATE_KEY,
    'MM_SERVICESETTINGS_ENABLEINCOMINGWEBHOOKS': 'true',
    'MM_SERVICESETTINGS_ENABLELOCALMODE': 'true',
    'MM_SERVICESETTINGS_LOCALMODESOCKETLOCATION': LOCAL_MODE_SOCKET,
    'MM_SERVICESETTINGS_ENABLESECURITYFIXALERT': 'false',
    'MM_SERVICESETTINGS_ENABLELINKPREVIEWS': 'false',
    'MM_PLUGINSETTINGS_ENABLE': 'false',
    'MM_LOGSETTINGS_ENABLEFILE': 'false',
    'MM_LOGSETTINGS_ENABLEDIAGNOSTICS': 'false',
    'MM_LOGSETTINGS_ENABLESENTRY': 'false',
    'MM_EMAILSETTINGS_SENDPUSHNOTIFICATIONS': 'false',
    'MM_ANNOUNCEMENTSETTINGS_ADMINNOTICESENABLED': 'false',
    'MM_ANNOUNCEMENTSETTINGS_USERNOTICESENABLED': 'false',
}
# A program of the test in the guest that sends one request to the server
# and prints the answer as JSON: the HTTP status code, the session token that
# a login answers with, and the body. A server that does not answer yet
# gives the status code 0.
REST_CLIENT = FIXTURE + '/mattermost_rest_client.py'
REST_CLIENT_PROGRAM = '''import json, sys, urllib.error, urllib.request
method, address, session_token = sys.argv[1:4]
data = sys.stdin.buffer.read()
headers = {'Content-Type': 'application/json'}
if session_token:
    headers['Authorization'] = 'Bearer ' + session_token
request = urllib.request.Request(address, data=data or None, method=method, headers=headers)
try:
    with urllib.request.urlopen(request, timeout=30) as response:
        answer = {'http_status_code': response.status, 'token': response.headers.get('Token', ''), 'body': response.read().decode()}
except urllib.error.HTTPError as error:
    answer = {'http_status_code': error.code, 'token': '', 'body': error.read().decode()}
except urllib.error.URLError as error:
    answer = {'http_status_code': 0, 'token': '', 'body': str(error.reason)}
print(json.dumps(answer))
'''
WEBHOOK_URL_FILE = '/etc/mmwatchdog/secrets/mattermost-webhook'
FRAGMENT = '/etc/mmwatchdog/check_groups/mattermost.yml'
FAIL_FILE = FIXTURE + '/mattermost-fail'
# What the check of the scenario prints, which becomes the body of its
# alert: text that Mattermost would read as a mention of everyone in the
# channel, as bold text, as the end of a code block, as HTML, and as an HTML
# entity.
AWKWARD_OUTPUT = ['the deploy told @channel: *bold* <b>stop</b> &amp; wait', '```', 'after three backticks']
# The most characters that Mattermost keeps in one post. It splits a longer
# text of a webhook into several posts, and the module cuts its message to
# this length.
MOST_CHARACTERS_OF_A_POST = 16383
# Characters of one, two, three, and four bytes in UTF-8, for a body that
# the module must cut.
MULTI_BYTE_TEXT = 'Grüße aus Köln, 東京 🚀 '
ZERO_WIDTH_SPACE = '​'


def release_archive():
    """The release archive of Mattermost for the guests' architecture, on this machine: downloaded once and checked against its SHA-256, then cached for later runs."""
    name = 'mattermost-team-' + MATTERMOST_RELEASE + '-linux-' + vm.ARCH + '.tar.gz'
    return vm.download_release_archive('https://releases.mattermost.com/' + MATTERMOST_RELEASE + '/' + name,
                                       MATTERMOST_ARCHIVE_SHA256[vm.ARCH])


def rest(method, path, data=None, session_token=''):
    """Send one request to the server from inside the guest, and return its HTTP status code, its session token, and its body as JSON where it is JSON."""
    answer = json.loads(vm.cmd(vm.ssh_args() + ['python3 ' + REST_CLIENT + ' ' + method + ' ' + shlex.quote(MATTERMOST_SERVER + path) + ' ' + shlex.quote(session_token)],
                               input=json.dumps(data) if data is not None else '').stdout)
    try:
        answer['body'] = json.loads(answer['body'])
    except ValueError:
        pass
    return answer


def mmctl(arguments):
    """Run mmctl in local mode as an administrator of the server, and return the one object that it prints as JSON, or None when it prints nothing."""
    printed = vm.console('MMCTL_LOCAL_SOCKET_PATH=' + LOCAL_MODE_SOCKET + ' ' + MATTERMOST_DIRECTORY + '/bin/mmctl --local --json --suppress-warnings ' + arguments).stdout.strip()
    if not printed:
        return None
    # mmctl prints a list of the objects that a command made.
    result = json.loads(printed)
    return result[0] if isinstance(result, list) and len(result) == 1 else result


def install_and_start(archive):
    """Put Mattermost into the guest, give it a database in the guest's PostgreSQL and a certificate that the guest trusts, start it, and wait until it answers."""
    vm.console("sed -i '/ # " + CERTIFICATE_OF_THE_TEST + "$/d' /etc/hosts; echo '127.0.0.1 " + MATTERMOST_HOST + ' # ' + CERTIFICATE_OF_THE_TEST + "' >> /etc/hosts; "
               'id -u ' + MATTERMOST_ACCOUNT + ' >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin ' + MATTERMOST_ACCOUNT + '; '
               'rm -rf ' + MATTERMOST_DIRECTORY)
    # The archive goes through the SSH connection straight into tar, so the
    # guest never holds it.
    with archive.open('rb') as file:
        subprocess.run(vm.ssh_args() + ['tar -xz -C ' + FIXTURE + ''.join(' --exclude=' + part for part in LEFT_OUT_OF_THE_ARCHIVE)],
                       stdin=file, check=True, capture_output=True, timeout=600)
    # With SELinux enforcing (Rocky Linux and AlmaLinux), systemd runs no program from /var/tmp:
    # the program gets the label of a program, as a file in /usr/local/bin has.
    vm.console('if [ "$(getenforce 2>/dev/null)" = Enforcing ]; then chcon -t bin_t ' + MATTERMOST_DIRECTORY + '/bin/mattermost; fi')
    vm.write(REST_CLIENT, REST_CLIENT_PROGRAM)
    vm.console('openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=' + MATTERMOST_HOST + ' -addext subjectAltName=DNS:' + MATTERMOST_HOST +
               ' -keyout ' + MATTERMOST_PRIVATE_KEY + ' -out ' + MATTERMOST_CERTIFICATE + ' >/dev/null 2>&1 && chmod 0600 ' + MATTERMOST_PRIVATE_KEY + ' && chown -R ' + MATTERMOST_ACCOUNT + ': ' + MATTERMOST_DIRECTORY)
    vm.trust_certificate(CERTIFICATE_OF_THE_TEST, vm.console('cat ' + MATTERMOST_CERTIFICATE).stdout)
    # A new database for each run.
    vm.console('runuser -u postgres -- psql --no-psqlrc --quiet --command ' + shlex.quote('DROP DATABASE IF EXISTS ' + DATABASE_AND_ROLE) +
               ' --command ' + shlex.quote('DROP ROLE IF EXISTS ' + DATABASE_AND_ROLE) +
               # An older server (PostgreSQL 13 of Rocky Linux 9) keeps the password in a form that its pg_hba.conf does not take over the network.
               ' --command ' + shlex.quote("SET password_encryption = 'scram-sha-256'") +
               ' --command ' + shlex.quote("CREATE ROLE " + DATABASE_AND_ROLE + " LOGIN PASSWORD '" + DATABASE_PASSWORD + "'") +
               ' --command ' + shlex.quote('CREATE DATABASE ' + DATABASE_AND_ROLE + ' OWNER ' + DATABASE_AND_ROLE))
    vm.console('systemd-run --quiet --unit ' + MATTERMOST_UNIT + ' --uid ' + MATTERMOST_ACCOUNT + ' --gid ' + MATTERMOST_ACCOUNT +
               ' -p WorkingDirectory=' + MATTERMOST_DIRECTORY + ' -p LimitNOFILE=49152 ' +
               ' '.join(shlex.quote('--setenv=' + name + '=' + value) for name, value in MATTERMOST_SETTINGS.items()) +
               ' ' + MATTERMOST_DIRECTORY + '/bin/mattermost')
    # The first start makes the tables of the new database. The wait ends as
    # soon as the server answers.
    deadline = time.monotonic() + 300
    while (answer := rest('GET', '/api/v4/system/ping'))['http_status_code'] != 200:
        running = vm.console('systemctl is-active --quiet ' + MATTERMOST_UNIT, check=False).returncode == 0
        if not running or time.monotonic() > deadline:
            raise AssertionError(('Mattermost did not start', answer, vm.console('journalctl -u ' + MATTERMOST_UNIT + ' -n 30 --no-pager --output cat', check=False).stdout))
        time.sleep(0.2)
    assert answer['body']['status'] == 'OK', answer


def set_up_the_team():
    """Make the administrator, the two other people, the team, and its channel, with all three people in both, and log the administrator in. Return the IDs of the people and of the channel, and the administrator's session token."""
    people = {}
    for username in (ADMINISTRATOR, ON_CALL_PERSON, OTHER_CHANNEL_MEMBER):
        administrator = ' --system-admin' if username == ADMINISTRATOR else ''
        people[username] = mmctl('user create --username ' + username + ' --email ' + username + '@mmwatchdog-test.internal --password ' + PASSWORD_OF_EVERY_PERSON +
                                 ' --email-verified --disable-welcome-email' + administrator)['id']
    mmctl('team create --name ' + TEAM + " --display-name 'MMWatchdog test team'")
    mmctl('team users add ' + TEAM + ' ' + ' '.join(people))
    channel_id = mmctl('channel create --team ' + TEAM + ' --name ' + CHANNEL + ' --display-name Alerts')['id']
    mmctl('channel users add ' + TEAM + ':' + CHANNEL + ' ' + ' '.join(people))
    login = rest('POST', '/api/v4/users/login', {'login_id': ADMINISTRATOR, 'password': PASSWORD_OF_EVERY_PERSON})
    assert login['http_status_code'] == 200 and login['token'], login
    return {'people': people, 'channel_id': channel_id, 'session_token': login['token']}


def new_webhook(mattermost):
    """Make an incoming webhook of the administrator for the channel, and return its ID and its address."""
    webhook_id = mmctl('webhook create-incoming --channel ' + mattermost['channel_id'] + ' --user ' + mattermost['people'][ADMINISTRATOR] + " --display-name 'MMWatchdog alerts' --lock-to-channel")['id']
    return webhook_id, MATTERMOST_SERVER + '/hooks/' + webhook_id


def posts_from_webhooks(mattermost):
    """The messages of the posts that webhooks made in the channel, oldest first."""
    answer = rest('GET', '/api/v4/channels/' + mattermost['channel_id'] + '/posts?per_page=200', session_token=mattermost['session_token'])
    assert answer['http_status_code'] == 200, answer
    posts = [post for post in answer['body']['posts'].values() if post.get('props', {}).get('from_webhook') == 'true']
    return [post['message'] for post in sorted(posts, key=lambda post: post['create_at'])]


def mentions_of(mattermost, username):
    """How often the posts of the channel mentioned one person, as Mattermost counts it for that person's unread posts."""
    answer = rest('GET', '/api/v4/channels/' + mattermost['channel_id'] + '/members/' + mattermost['people'][username], session_token=mattermost['session_token'])
    assert answer['http_status_code'] == 200, answer
    return answer['body']['mention_count']


def as_mattermost_line(title):
    """The title of an alert as the module mattermost writes it in the first line (its README, "What it sends"): a backslash in front of \\ unless an ASCII letter or digit follows it, in front of `, *, ~, [, and ], in front of _ unless it stands between two letters or digits, and in front of the ! of <!; and a zero width space after an @ that no letter or digit precedes."""
    line = ''
    for index, character in enumerate(title):
        before = title[index - 1] if index > 0 else ''
        after = title[index + 1] if index + 1 < len(title) else ''
        if character == '\\' and after.isascii() and after.isalnum():
            line += character
        elif character in '\\`*~[]':
            line += '\\' + character
        elif character == '_' and not (before.isalnum() and after.isalnum()):
            line += '\\_'
        elif character == '!' and before == '<':
            line += '\\!'
        elif character == '@' and not before.isalnum():
            line += '@' + ZERO_WIDTH_SPACE
        else:
            line += character
    return line


def as_mattermost_gets_it(title_with_alert_severity, body='', mention=None):
    """An alert as the module mattermost posts it: the mention and the title in the first line, and below it the body as it is, in a code block whose fences are one backtick longer than the longest row of backticks in the body, and at least three. A longer message is cut to MOST_CHARACTERS_OF_A_POST characters, with "…" at the end of the body."""
    text = (mention + ' ' if mention else '') + as_mattermost_line(title_with_alert_severity)
    if body:
        fence = '`' * max(3, max((len(row) for row in re.findall('`+', body)), default=0) + 1)
        room_for_the_body = MOST_CHARACTERS_OF_A_POST - len(text) - 2 * len(fence) - 3
        if len(body) > room_for_the_body:
            body = body[:room_for_the_body - 1] + '…'
        text += '\n' + fence + '\n' + body + '\n' + fence
    return text


def suite(cli, status, events, wait, mark, setup_step_done):
    """The scenario: Mattermost in the guest, and MMWatchdog's alerts through the module mattermost to its channel. setup_step_done(step) prints the seconds of a step of the setup."""
    archive = release_archive()
    setup_step_done('the release archive of Mattermost ' + MATTERMOST_RELEASE + ' is on the machine that runs the suite')
    install_and_start(archive)
    setup_step_done('Mattermost runs in the guest, with a new database in the guest\'s PostgreSQL')
    mattermost = set_up_the_team()
    setup_step_done('Mattermost has an administrator, a team, and a channel with three people in it, and the administrator has logged in')
    node = vm.hostname()
    webhook_id, webhook = new_webhook(mattermost)
    alerts = json.loads(vm.console('cat /etc/mmwatchdog/alert_destinations.yml').stdout)

    def destination(inputs, **more):
        return {'remind_every': 'never', 'mattermost': inputs, 'alert_severities': [], **more}

    # Destinations that take no severity, so that only the check and the
    # notices below reach them: the channel with a mention, and a second
    # destination for the same channel that takes only the title.
    destinations = dict(alerts['destinations'],
                        team_chat=destination({'webhook_url_file': WEBHOOK_URL_FILE, 'mention': MENTION}),
                        team_chat_title_only=destination({'webhook_url_file': WEBHOOK_URL_FILE}, send_only_the_alert_title=True))

    def messages_with_title(title):
        """The messages of the posts whose first line ends with the title."""
        return [message for message in posts_from_webhooks(mattermost) if message.split('\n')[0].endswith(title)]

    def notice(title, code=0, body=''):
        return cli('send-notice --alert-severity info --title ' + shlex.quote(title) + (' --body ' + shlex.quote(body) if body else '') + ' --destination team_chat', code)

    def post_through_the_webhook(text):
        """Post a text to the channel through the webhook, from the guest and without MMWatchdog."""
        answer = rest('POST', '/hooks/' + webhook_id, {'text': text})
        assert answer == {'http_status_code': 200, 'token': '', 'body': 'ok'}, answer

    def what_arrived(send, expected_message):
        """Send one message to the destination team_chat, whose mention names the on-call person, and wait until Mattermost counted that mention. Return what arrived: the new posts, whether they are the one expected message, and how many mentions the other member of the channel got from them, which is none unless they mentioned the whole channel. Mattermost counts the mentions of one post for every person at once."""
        posts, on_call, other = len(posts_from_webhooks(mattermost)), mentions_of(mattermost, ON_CALL_PERSON), mentions_of(mattermost, OTHER_CHANNEL_MEMBER)
        send()
        wait(lambda: mentions_of(mattermost, ON_CALL_PERSON) == on_call + 1)
        arrived = posts_from_webhooks(mattermost)[posts:]
        return {'arrived_as_the_module_sent_it': arrived == [expected_message], 'mentions_of_the_other_member': mentions_of(mattermost, OTHER_CHANNEL_MEMBER) - other,
                'arrived': arrived, 'expected': expected_message}

    # The IDs of the webhooks, the secret part of each address.
    webhook_ids = [webhook_id]

    def shown(*outputs):
        """Fail when anything MMWatchdog printed holds the ID of a webhook."""
        for output in outputs:
            assert not [secret for secret in webhook_ids if secret in output], output
    try:
        vm.write(WEBHOOK_URL_FILE, webhook + '\n', '0600')
        vm.write('/etc/mmwatchdog/alert_destinations.yml', json.dumps({'destinations': destinations}, indent=2))
        vm.write(FRAGMENT, json.dumps({'check_groups': {'mattermost': {
            'check_every': '1s', 'time_limit_per_run': '900ms', 'alert_after_failing_for': '0s', 'recovered_after_healthy_for': '0s',
            'shell': 'printf \'%s\\n\' ' + ' '.join(shlex.quote(line) for line in AWKWARD_OUTPUT) + '; test ! -e ' + FAIL_FILE, 'alert_severity': 'warn',
            'send_alerts_only_to_destinations': ['team_chat', 'team_chat_title_only']}}}))
        cli('validate', 0)
        cli('reload', 0)
        wait(lambda: status()['checks']['mattermost/shell']['state'] == 'ok')
        vm.console('touch ' + FAIL_FILE)
        title = node + ': mattermost/shell failed'
        wait(lambda: len(messages_with_title(title)) == 2)
        failed = next(e for e in events() if e.get('check_group') == 'mattermost' and e['type'] == 'failed')
        assert failed['title_with_alert_severity'] == 'Warning: ' + title and all(line in failed['body'] for line in AWKWARD_OUTPUT), failed
        # The destination with the mention gets the mention and the first
        # line, and below it the body in a code block; the destination that
        # takes only the title gets the first line.
        assert sorted(messages_with_title(title)) == sorted([as_mattermost_gets_it('Warning: ' + title, failed['body'], MENTION), as_mattermost_gets_it('Warning: ' + title)]), (messages_with_title(title), failed)
        # Mattermost counted the mention, and nothing of the body mentioned
        # anybody: once the person of the mention has it, every count of the
        # post is done.
        wait(lambda: mentions_of(mattermost, ON_CALL_PERSON) == 1)
        assert mentions_of(mattermost, OTHER_CHANNEL_MEMBER) == 0
        vm.console('rm ' + FAIL_FILE)
        title = node + ': mattermost/shell is ok again'
        wait(lambda: len(messages_with_title(title)) == 2)
        recovered = next(e for e in events() if e.get('check_group') == 'mattermost' and e['type'] == 'recovered')
        assert sorted(messages_with_title(title)) == sorted([as_mattermost_gets_it('Resolved warning: ' + title, recovered['body'], MENTION), as_mattermost_gets_it('Resolved warning: ' + title)]), (messages_with_title(title), recovered)
        wait(lambda: mentions_of(mattermost, ON_CALL_PERSON) == 2)
        assert mentions_of(mattermost, OTHER_CHANNEL_MEMBER) == 0
        mark('the official extra module mattermost posts a check\'s failure and its recovery to a real Mattermost server: the destination with a mention gets the mention, the title with the alert\'s severity in front, and the body in a code block whose fences are longer than its three backticks, and a destination that takes only the title gets that line; Mattermost counts the mention, and the body\'s @channel, *bold*, three backticks, <b>, and &amp; stay text that mentions nobody')

        # What Mattermost itself does, without the module, so that the counts
        # below can see what they look for: @channel mentions the other
        # member, and so does Slack's <!here> while that member is online;
        # and a text one character longer than a post becomes two posts.
        other = mentions_of(mattermost, OTHER_CHANNEL_MEMBER)
        post_through_the_webhook('@channel: a post of the test, without MMWatchdog')
        wait(lambda: mentions_of(mattermost, OTHER_CHANNEL_MEMBER) == other + 1)
        other_member = mattermost['people'][OTHER_CHANNEL_MEMBER]
        online = rest('PUT', '/api/v4/users/' + other_member + '/status', {'user_id': other_member, 'status': 'online'}, session_token=mattermost['session_token'])
        assert online['http_status_code'] == 200, online
        post_through_the_webhook('<!here>: a post of the test, without MMWatchdog')
        wait(lambda: mentions_of(mattermost, OTHER_CHANNEL_MEMBER) == other + 2)
        posts = len(posts_from_webhooks(mattermost))
        post_through_the_webhook('x' * (MOST_CHARACTERS_OF_A_POST + 1))
        assert len(posts_from_webhooks(mattermost)) == posts + 2, posts_from_webhooks(mattermost)[posts:]

        # The same through the module, each a message of its own, so that a
        # failure shows what Mattermost did with each of them: a line of
        # three backticks in the body and a line of three tildes, each with
        # @channel after it, which Mattermost must read as inside the code
        # block; a notice whose title holds a mention of the channel in
        # Mattermost's notation and in Slack's, Markdown, and an @ inside a
        # word; and a body of characters of up to four bytes that the module
        # cuts to the most that a post holds, which must arrive as one post.
        arrived = {}
        for name, body in [('backticks', 'the deploy wrote:\n```\n@channel after a line of three backticks'),
                           ('tildes', 'the deploy wrote:\n~~~\n@channel after a line of three tildes')]:
            title = 'A body with a line of three ' + name
            arrived[name] = what_arrived(lambda: notice(title, body=body), as_mattermost_gets_it('Info: ' + node + ': ' + title, body, MENTION))
        title = '@channel *x* <!here> getty@tty1.service'
        arrived['title'] = what_arrived(lambda: notice(title), as_mattermost_gets_it('Info: ' + node + ': ' + title, mention=MENTION))
        long_body = ''.join(itertools.islice(itertools.cycle(MULTI_BYTE_TEXT), MOST_CHARACTERS_OF_A_POST + 1000))
        cut = as_mattermost_gets_it(failed['title_with_alert_severity'], long_body, MENTION)
        assert len(cut) == MOST_CHARACTERS_OF_A_POST and cut.endswith('…\n```'), len(cut)

        def run_the_module():
            """Run the module as MMWatchdog starts it, with an alert whose body is longer than a post."""
            request = {'protocol': 1, 'alert': dict(failed, body=long_body), 'inputs': {'mention': MENTION}, 'file_contents': {'webhook_url_file': webhook}}
            result = json.loads(vm.cmd(vm.ssh_args() + ['runuser -u mmwatchdog -- /usr/local/bin/mmwatchdog run-starlark-module ' + vm.OFFICIAL_EXTRA_MODULES + '/mattermost'], input=json.dumps(request)).stdout)
            assert result['status'] == 'delivered', result
        arrived['cut'] = what_arrived(run_the_module, cut)
        assert all(each['arrived_as_the_module_sent_it'] and each['mentions_of_the_other_member'] == 0 for each in arrived.values()), arrived
        mark('Mattermost reads the module\'s code block as the module means it, and mentions nobody for @channel after a line of three backticks or of three tildes in the body; a title with @channel, <!here>, *x*, and getty@tty1.service arrives as the module escaped it and mentions nobody, where the same @channel and <!here> without the module mention the channel; and a body of characters of up to four bytes that the module cut to 16,383 characters arrives as one post, where one character more makes two')

        # A webhook that was deleted in Mattermost: Mattermost refuses the
        # message, and the command and the status say so with Mattermost's
        # answer and without the webhook's address. MMWatchdog reads the file for
        # every delivery, so the queued notice goes out once the file holds
        # a webhook that works, with no reload.
        mmctl('webhook delete ' + webhook_id)
        title = 'After the webhook was deleted'
        refused = notice(title, code=1)
        # Mattermost answers 400, and its message names the webhook's ID.
        assert refused.stdout.startswith('Notice sent to the destination team_chat:\n  team_chat   failed: the module mattermost reported: ') and '400' in refused.stdout, refused.stdout
        assert 'the notice was not delivered at the first attempt to 1 of 1 destinations. MMWatchdog keeps the notice queued and tries again' in refused.stderr, refused.stderr
        printed = cli('status').stdout
        line = [l for l in printed.splitlines() if l.startswith('Destination team_chat: ')]
        assert len(line) == 1 and line[0].startswith('Destination team_chat: 1 alert is not delivered. MMWatchdog tries again at ') and 'The latest attempt failed: the module mattermost reported: ' in line[0], printed
        failing = [f for f in status()['delivery']['failing_destinations'] if f['destination'] == 'team_chat']
        assert len(failing) == 1 and failing[0]['undelivered_alerts'] == 1 and failing[0]['latest_attempt_error'].startswith('the module mattermost reported: '), failing
        webhook_id, webhook = new_webhook(mattermost)
        webhook_ids.append(webhook_id)
        vm.write(WEBHOOK_URL_FILE, webhook + '\n', '0600')
        wait(lambda: messages_with_title(title) == [as_mattermost_gets_it('Info: ' + node + ': ' + title, mention=MENTION)])
        wait(lambda: not [f for f in status()['delivery']['failing_destinations'] if f['destination'] == 'team_chat'])
        shown(refused.stdout, refused.stderr, printed, cli('status --json').stdout, vm.console('journalctl -u mmwatchdog --since -5min --no-pager', check=False).stdout)
        mark('a webhook that was deleted in Mattermost fails the first attempt of mmwatchdog send-notice with Mattermost\'s answer, in the command and in mmwatchdog status, without the webhook\'s address; the queued notice arrives once the file holds a webhook that works, without a reload')
    finally:
        vm.write('/etc/mmwatchdog/alert_destinations.yml', json.dumps(alerts, indent=2))
        vm.console('rm -f ' + ' '.join([FRAGMENT, WEBHOOK_URL_FILE, FAIL_FILE]))
        cli('reload', 0)


def clean_up():
    """Stop the server and take its name and its certificate out of the guest. vm.reset calls it, through the services suite's clean_up, after an interruption too; reset removes the server's files with the fixtures."""
    vm.console(vm.each_unit('stop', [MATTERMOST_UNIT]) + '; ' + vm.each_unit('reset-failed', [MATTERMOST_UNIT]) + "; sed -i '/ # " + CERTIFICATE_OF_THE_TEST + "$/d' /etc/hosts")
    vm.distrust_certificate(CERTIFICATE_OF_THE_TEST)
