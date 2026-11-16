"""The official extra destination modules slack, telegram,
pushover, discord, and mattermost against stand-ins for their
services inside the guest."""
import json
import re
import shlex
import time
import destination_services_stand_in
import vm

# The stand-in's certificate, in a guest's system trust store: MMWatchdog's primitive
# http_request, which the modules send with, verifies a server's certificate
# with that store.
# The name under which the guests trust the certificate of the stand-in.
CERTIFICATE_OF_THE_TEST = 'mmwatchdog-test-destination-services-stand-in'
UNIT = '/etc/systemd/system/mmwatchdog-test-destination-services-stand-in.service'
MODULE_DIRECTORY = vm.OFFICIAL_EXTRA_MODULES
SLACK_WEBHOOK_FILE = '/etc/mmwatchdog/secrets/slack-webhook-team'
TELEGRAM_BOT_TOKEN_FILE = '/etc/mmwatchdog/secrets/telegram-bot'
MENTION = '<@U024BE7LH>'
PUSHOVER_APPLICATION_TOKEN_FILE = '/etc/mmwatchdog/secrets/pushover-application-token'
PUSHOVER_USER_OR_GROUP_KEY_FILE = '/etc/mmwatchdog/secrets/pushover-on-call-group-key'
DISCORD_WEBHOOK_FILE = '/etc/mmwatchdog/secrets/discord-webhook-team'
MATTERMOST_VERIFICATION_WEBHOOK_FILE = '/etc/mmwatchdog/secrets/mattermost-verification-webhook'
# While this file is there, the check of the destinations of the people on
# call fails.
ON_CALL_CHECK_FAILS_WHILE_THIS_FILE_IS_THERE = '/var/tmp/mmwatchdog-fixture/on-call-fail'
# The destinations of the people on call, one for each of the two modules,
# by the module that each names.
ON_CALL_DESTINATION_OF_MODULE = {'pushover': 'pushover_on_call', 'discord': 'discord_team_channel'}
# The status code with which each of the two services answers a request
# that it accepted.
ACCEPTED_HTTP_STATUS_CODE = {'pushover': 200, 'discord': 200}
# The characters that Discord reads as formatting outside a code block, and
# that the module discord writes with a backslash in front.
DISCORD_FORMATTING_CHARACTERS = '\\*_~`|<>['


def install_modules(guest=vm.NAME):
    """Install the official extra modules from the repository, as a deploy
    does. The destination modules are scripts that MMWatchdog runs itself, so they
    need nothing else on the guest."""
    vm.install_official_extra_modules(guest=guest)


def start_destination_services_stand_in(names, guests=(vm.NAME,)):
    """Start the stand-in for the services on the first of guests, with
    a certificate for the host names in names, which every one of guests
    trusts through its system trust store."""
    host = guests[0]
    vm.write('/var/tmp/mmwatchdog-fixture/destination_services_stand_in.py', (vm.REPO / 'tests/integration/destination_services_stand_in.py').read_text(), guest=host)
    vm.console('rm -f /var/tmp/mmwatchdog-fixture/destination-services-stand-in.jsonl /var/tmp/mmwatchdog-fixture/destination-services-verifications.jsonl; openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=mmwatchdog-test-destination-services-stand-in -addext subjectAltName='
               + ','.join('DNS:' + name for name in names)
               + ' -keyout /var/tmp/mmwatchdog-fixture/destination-services-stand-in-key.pem -out /var/tmp/mmwatchdog-fixture/destination-services-stand-in-cert.pem >/dev/null 2>&1', guest=host)
    certificate = vm.console('cat /var/tmp/mmwatchdog-fixture/destination-services-stand-in-cert.pem', guest=host).stdout
    for guest in guests:
        vm.trust_certificate(CERTIFICATE_OF_THE_TEST, certificate, guest=guest)
    vm.write(UNIT, '[Unit]\nDescription=MMWatchdog test stand-in for the services of the destination modules\n[Service]\n'
             'ExecStart=/usr/bin/python3 /var/tmp/mmwatchdog-fixture/destination_services_stand_in.py\nRestart=no\n', guest=host)
    vm.console('systemctl daemon-reload; systemctl restart mmwatchdog-test-destination-services-stand-in', guest=host)
    listening = "ss -ltn | grep -q ':%d '" % destination_services_stand_in.PORT
    deadline = time.monotonic() + 10
    while vm.console(listening, check=False, guest=host).returncode:
        assert time.monotonic() < deadline, vm.console('journalctl -u mmwatchdog-test-destination-services-stand-in -n 20 --no-pager', check=False, guest=host).stdout
        time.sleep(0.2)


def stop_destination_services_stand_in(guests=(vm.NAME,)):
    """Stop the stand-in, take its certificate out of the system trust store
    of each of guests, and remove the official extra modules there."""
    for guest in guests:
        vm.console('systemctl stop mmwatchdog-test-destination-services-stand-in 2>/dev/null; rm -rf ' + ' '.join([UNIT, MODULE_DIRECTORY]) + '; systemctl daemon-reload; true', guest=guest)
        vm.distrust_certificate(CERTIFICATE_OF_THE_TEST, guest=guest)


def received(service, guest=vm.NAME, *, verifications=False):
    """Deliveries for one service in order, or its separate verification log."""
    log = 'destination-services-verifications.jsonl' if verifications else 'destination-services-stand-in.jsonl'
    lines = vm.console('cat /var/tmp/mmwatchdog-fixture/' + log, check=False, guest=guest).stdout.splitlines()
    return [record for record in (json.loads(line) for line in lines if line) if record['service'] == service]


def as_slack_gets_it(first_line, body=''):
    """An alert as the slack module sends it: the first line, and below it the
    body in a code block. Slack reads &, <, and > as control characters."""
    def escaped(text):
        return text.replace('&', '&amp;').replace('<', '&lt;').replace('>', '&gt;')
    if not body:
        return escaped(first_line)
    return escaped(first_line) + '\n```\n' + escaped(body) + '\n```'


def as_discord_gets_it(first_line, body=''):
    """An alert as the discord module sends it: the first line with a
    backslash in front of each character that Discord reads as formatting,
    and below it the body in a code block, where Discord reads none."""
    escaped = ''.join('\\' + character if character in DISCORD_FORMATTING_CHARACTERS else character for character in first_line)
    if not body:
        return escaped
    return escaped + '\n```\n' + body + '\n```'


def on_call_destinations(server):
    """The destinations of the modules pushover and discord, which send
    to the stand-in at server. Like the destinations of the chats, they take no severity, so only the check
    on_call_alert and the notices reach them."""
    inputs = {
        'pushover': {'application_token_file': PUSHOVER_APPLICATION_TOKEN_FILE, 'user_or_group_key_file': PUSHOVER_USER_OR_GROUP_KEY_FILE,
                     'repeat_sos_alerts_every': '5m', 'stop_repeating_sos_alerts_after': '1h', 'api_server_url': server},
        'discord': {'webhook_url_file': DISCORD_WEBHOOK_FILE},
    }
    return {name: {'remind_every': 'never', module: inputs[module], 'alert_severities': []} for module, name in ON_CALL_DESTINATION_OF_MODULE.items()}


def on_call_credential_files(server):
    """The content of each file of the credentials of the two destinations
    that the stand-in at server accepts."""
    return {PUSHOVER_APPLICATION_TOKEN_FILE: destination_services_stand_in.PUSHOVER_APPLICATION_TOKEN, PUSHOVER_USER_OR_GROUP_KEY_FILE: destination_services_stand_in.PUSHOVER_USER_OR_GROUP_KEY,
            DISCORD_WEBHOOK_FILE: server + destination_services_stand_in.DISCORD_WEBHOOK_PATH}


# An sos check that sends its alerts to the two destinations alone.
ON_CALL_CHECK_GROUP = {
    'check_every': '300ms', 'time_limit_per_run': '200ms', 'alert_after_failing_for': '0s', 'recovered_after_healthy_for': '0s',
    'shell': 'echo "the queue of payments is full"; test ! -e ' + ON_CALL_CHECK_FAILS_WHILE_THIS_FILE_IS_THERE, 'alert_severity': 'sos',
    'send_alerts_only_to_destinations': sorted(ON_CALL_DESTINATION_OF_MODULE.values())}


def suite(cli, status, state, events, wait, mark, alerts):
    """alerts is the content of alert_destinations.yml before and after."""
    node = vm.hostname()
    fragment = '/etc/mmwatchdog/check_groups/chat.yml'
    server = 'https://localhost:%d' % destination_services_stand_in.PORT
    webhook = server + destination_services_stand_in.SLACK_WEBHOOK_PATH
    removed_webhook = server + '/services/T00000000/B00000000/SlackFixtureRemovedWebhook001'
    token, chat = destination_services_stand_in.TELEGRAM_BOT_TOKEN, destination_services_stand_in.TELEGRAM_CHAT_ID
    secrets = [destination_services_stand_in.SLACK_WEBHOOK_PATH, removed_webhook, token]
    credential_files = on_call_credential_files(server)
    credential_files[MATTERMOST_VERIFICATION_WEBHOOK_FILE] = server + destination_services_stand_in.MATTERMOST_WEBHOOK_PATH

    def destination(module, inputs, **more):
        return {'remind_every': 'never', module: inputs, 'alert_severities': [], **more}

    # Destinations that take no severity, so only the check and the notices
    # below reach them: a Slack channel with a mention, a second destination
    # for the same channel that takes only the title, a Telegram chat, and a
    # Telegram chat that the bot does not know.
    telegram = {'bot_token_file': TELEGRAM_BOT_TOKEN_FILE, 'chat_id': chat, 'bot_api_server_url': server}
    destinations = dict(
        alerts['destinations'],
        team_chat=destination('slack', {'webhook_url_file': SLACK_WEBHOOK_FILE, 'mention': MENTION}),
        team_chat_title_only=destination('slack', {'webhook_url_file': SLACK_WEBHOOK_FILE}, send_only_the_alert_title=True),
        on_call_phone=destination('telegram', telegram),
        mattermost_team_chat=destination('mattermost', {'webhook_url_file': MATTERMOST_VERIFICATION_WEBHOOK_FILE}),
        **on_call_destinations(server),
    )

    def texts(service, title):
        """The texts of the accepted messages of one service whose first line ends with title."""
        return [r['payload']['text'] for r in received(service) if r['http_status_code'] == 200 and r['payload']['text'].split('\n')[0].endswith(title)]

    start_destination_services_stand_in(['localhost'])
    try:
        install_modules()
        vm.write(SLACK_WEBHOOK_FILE, webhook + '\n', '0600')
        vm.write(TELEGRAM_BOT_TOKEN_FILE, token + '\n', '0600')
        for path, content in credential_files.items():
            vm.write(path, content + '\n', '0600')
        vm.write('/etc/mmwatchdog/alert_destinations.yml', json.dumps({'destinations': destinations}, indent=2))
        # The check prints the three characters that Slack reads as control
        # characters, so they are in the body of its alert.
        vm.write(fragment, json.dumps({'check_groups': {'chat_alert': {
            'check_every': '300ms', 'time_limit_per_run': '200ms', 'alert_after_failing_for': '0s', 'recovered_after_healthy_for': '0s',
            'shell': 'echo "queue <main> is full & blocked -> restart"; test ! -e /var/tmp/mmwatchdog-fixture/chat-fail', 'alert_severity': 'warn',
            'send_alerts_only_to_destinations': ['team_chat', 'team_chat_title_only', 'on_call_phone']},
            'on_call_alert': ON_CALL_CHECK_GROUP}}))
        # Runtime validation finds every module and reads every file.
        cli('validate', 0)
        reloaded = cli('reload', 0)
        successes = {
            'team_chat': 'The webhook connection and certificate were verified; its address and credential were not checked',
            'team_chat_title_only': 'The webhook connection and certificate were verified; its address and credential were not checked',
            'on_call_phone': 'Telegram accepted the verification; no message was sent',
            'mattermost_team_chat': 'Mattermost accepted the verification; no message was sent',
            'pushover_on_call': 'Pushover accepted the verification; no message was sent',
            'discord_team_channel': 'Discord accepted the verification; no message was sent',
        }
        for name, message in successes.items():
            assert 'Destination ' + name + ': ' + message + '\n' in reloaded.stdout, reloaded.stdout
        for service in ('telegram', 'pushover', 'discord', 'mattermost'):
            assert received(service, verifications=True), service
            assert not received(service), received(service)
        bad_credentials = [
            ('mattermost_team_chat', MATTERMOST_VERIFICATION_WEBHOOK_FILE, server + '/hooks/' + 'z' * 26, credential_files[MATTERMOST_VERIFICATION_WEBHOOK_FILE],
             'Mattermost does not accept this webhook; replace the address in webhook_url_file'),
            ('on_call_phone', TELEGRAM_BOT_TOKEN_FILE, token + 'wrong', token,
             'Telegram does not know this bot token; replace the content of bot_token_file'),
            ('pushover_on_call', PUSHOVER_APPLICATION_TOKEN_FILE, 'x' * 30, credential_files[PUSHOVER_APPLICATION_TOKEN_FILE],
             'Pushover does not accept the application token or user key; correct application_token_file or user_or_group_key_file'),
            ('discord_team_channel', DISCORD_WEBHOOK_FILE, credential_files[DISCORD_WEBHOOK_FILE] + '-wrong', credential_files[DISCORD_WEBHOOK_FILE],
             'Discord does not know this webhook or its token; replace the address in webhook_url_file'),
        ]
        for name, path, wrong, correct, message in bad_credentials:
            vm.write(path, wrong + '\n', '0600')
            refused = cli('reload', 1)
            assert 'Destination ' + name + ': failed: ' + message + '\n' in refused.stdout, refused.stdout
            assert 'the new configuration is active' in refused.stderr, refused.stderr
            vm.write(path, correct + '\n', '0600')
            restored = cli('reload', 0)
            assert 'Destination ' + name + ': ' + successes[name] + '\n' in restored.stdout, restored.stdout
        mark('reload verifies the official extra destinations without a delivery, and reports the module-specific refusal for each bad credential; restoring the credential verifies it again')
        # Each is an official extra module, shipped with this build of MMWatchdog.
        used = {m['name']: m for m in json.loads(cli('module-usage --json', 0).stdout)['modules']}
        names = ['slack', 'telegram', 'mattermost', *ON_CALL_DESTINATION_OF_MODULE]
        assert [(used[name]['kind'], used[name]['directory'], used[name]['shipped_with_mmwatchdog_version']) for name in names] == [
            ('official_extra', MODULE_DIRECTORY + '/' + name, '0.1.0-dev') for name in names], used
        # The modules of one kind stand in the order of their names.
        usage = cli('module-usage', 0).stdout
        assert re.search(r'^Official extra modules \(Starlark, shipped with MMWatchdog 0\.1\.0-dev\)\n  discord +', usage, re.M), usage
        for name in names:
            assert re.search(r'^  ' + name + ' +' + re.escape(MODULE_DIRECTORY) + '/' + name + '/$', usage, re.M), usage
        wait(lambda: state('chat_alert/shell')['state'] == 'ok')
        vm.console('touch /var/tmp/mmwatchdog-fixture/chat-fail')
        title = node + ': chat_alert/shell failed'
        wait(lambda: len(texts('slack', title)) == 2 and len(texts('telegram', title)) == 1)
        event = next(e for e in events() if e.get('check_group') == 'chat_alert' and e['type'] == 'failed')
        assert event['title'] == title and '<main> is full & blocked ->' in event['body'], event
        assert event['title_with_alert_severity'] == 'Warning: ' + title, event
        # Slack gets the mention, the title with the alert's severity in
        # front, and below it the body in a code block, with &, <, and > of
        # the alert written as Slack asks; the destination that takes only
        # the title gets that first line.
        assert sorted(texts('slack', title)) == sorted([MENTION + ' ' + as_slack_gets_it('Warning: ' + title, event['body']), 'Warning: ' + title]), (texts('slack', title), event)
        posted = [r for r in received('slack') if r['payload']['text'].startswith(MENTION)][0]
        assert posted['path'] == destination_services_stand_in.SLACK_WEBHOOK_PATH and posted['content_type'] == 'application/json' and list(posted['payload']) == ['text'], posted
        # Telegram gets the same first line and the body as they are, for
        # its chat.
        sent = [r for r in received('telegram') if r['http_status_code'] == 200][0]
        assert sent['payload'] == {'chat_id': chat, 'text': 'Warning: ' + title + '\n' + event['body'], 'link_preview_options': {'is_disabled': True}}, (sent, event)
        assert sent['path'] == '/bot' + token + '/sendMessage' and sent['content_type'] == 'application/json', sent
        vm.console('rm /var/tmp/mmwatchdog-fixture/chat-fail')
        title = node + ': chat_alert/shell is ok again'
        wait(lambda: len(texts('slack', title)) == 2 and len(texts('telegram', title)) == 1)
        recovered = next(e for e in events() if e.get('check_group') == 'chat_alert' and e['type'] == 'recovered')
        assert texts('telegram', title) == ['Resolved warning: ' + title + '\n' + recovered['body']], (texts('telegram', title), recovered)
        assert MENTION + ' ' + as_slack_gets_it('Resolved warning: ' + title, recovered['body']) in texts('slack', title), (texts('slack', title), recovered)
        mark('the official extra modules slack and telegram deliver a check\'s failure and its recovery, with the alert\'s severity in front of the title, Warning for the failure and Resolved warning for the recovery: Slack gets the mention and that line, and below it the body in a code block, with &, <, and > written as Slack asks, and a destination that takes only the title gets that line; Telegram gets the line and the body as plain text for its chat')

        def send_notice(title, name, *more, code=0):
            return cli('send-notice --alert-severity info --title ' + shlex.quote(title) + ' --destination ' + name
                       + ''.join(' ' + shlex.quote(word) for word in more), code)

        def shown(*outputs):
            """Fail when anything MMWatchdog printed holds a secret of the two destinations."""
            for output in outputs:
                assert not [secret for secret in secrets if secret in output], output

        title = 'Test of the chat destinations. Nothing is wrong.'
        sent = send_notice(title, 'team_chat', '--body', 'Sent by the acceptance suite.')
        assert sent.stdout == 'Notice sent to the destination team_chat:\n  team_chat   delivered\n', sent.stdout
        assert texts('slack', title) == [MENTION + ' ' + as_slack_gets_it('Info: ' + node + ': ' + title, 'Sent by the acceptance suite.')], texts('slack', title)
        sent = send_notice(title, 'on_call_phone')
        assert sent.stdout == 'Notice sent to the destination on_call_phone:\n  on_call_phone   delivered\n', sent.stdout
        assert texts('telegram', title) == ['Info: ' + node + ': ' + title], texts('telegram', title)
        # A webhook that was removed: Slack refuses the message, and the
        # command and the status say why, without the webhook's address. MMWatchdog
        # reads the file for every delivery, so the queued notice goes out
        # once the file holds a webhook that works, with no reload.
        title = 'After the webhook was removed'
        vm.write(SLACK_WEBHOOK_FILE, removed_webhook + '\n', '0600')
        refused = send_notice(title, 'team_chat', code=1)
        why = 'the module slack reported: Slack answered 404: no_service. The address of the webhook does not work any more.'
        assert refused.stdout.startswith('Notice sent to the destination team_chat:\n  team_chat   failed: ' + why), refused.stdout
        assert 'the notice was not delivered at the first attempt to 1 of 1 destinations. MMWatchdog keeps the notice queued and tries again' in refused.stderr, refused.stderr
        printed = cli('status').stdout
        line = [l for l in printed.splitlines() if l.startswith('Destination team_chat: ')]
        assert len(line) == 1 and line[0].startswith('Destination team_chat: 1 alert is not delivered. MMWatchdog tries again at ') and 'The latest attempt failed: ' + why in line[0], printed
        failing = [f for f in status()['delivery']['failing_destinations'] if f['destination'] == 'team_chat']
        assert len(failing) == 1 and failing[0]['undelivered_alerts'] == 1 and failing[0]['latest_attempt_error'].startswith(why), failing
        shown(refused.stdout, refused.stderr, printed, cli('status --json').stdout)
        vm.write(SLACK_WEBHOOK_FILE, webhook + '\n', '0600')
        wait(lambda: texts('slack', title) == [MENTION + ' Info: ' + node + ': ' + title])
        wait(lambda: not [f for f in status()['delivery']['failing_destinations'] if f['destination'] == 'team_chat'])
        # Add the deliberately unknown chat only after the successful reload.
        destinations['unknown_chat'] = destination('telegram', dict(telegram, chat_id='-1009999999999'))
        vm.write('/etc/mmwatchdog/alert_destinations.yml', json.dumps({'destinations': destinations}, indent=2))
        refused = cli('reload', 1)
        assert 'Destination unknown_chat: failed: Telegram does not know this chat; correct chat_id and add the bot to the chat' in refused.stdout, refused.stdout
        # A chat that the bot does not know: Telegram refuses the message.
        refused = send_notice('For a chat that the bot does not know', 'unknown_chat', code=1)
        assert refused.stdout.startswith('Notice sent to the destination unknown_chat:\n  unknown_chat   failed: the module telegram reported: '
                                         'Telegram answered 400: Bad Request: chat not found. Check chat_id, and that the bot is a member of the chat.'), refused.stdout
        shown(refused.stdout, refused.stderr, cli('status').stdout, cli('status --json').stdout, vm.console('journalctl -u mmwatchdog --since -5min --no-pager', check=False).stdout)
        mark('mmwatchdog send-notice through the modules slack and telegram prints delivered; a webhook that Slack no longer knows and a chat that Telegram does not know fail the first attempt with the service\'s answer and what to do, in the command and in mmwatchdog status, without the webhook\'s address or the bot\'s token; the queued notice arrives once the file holds a webhook that works, without a reload')
        on_call_scenarios(cli, status, state, events, wait, mark, send_notice, server)
    finally:
        vm.write('/etc/mmwatchdog/alert_destinations.yml', json.dumps(alerts, indent=2))
        vm.console('rm -rf ' + ' '.join([fragment, SLACK_WEBHOOK_FILE, TELEGRAM_BOT_TOKEN_FILE, '/var/tmp/mmwatchdog-fixture/chat-fail',
                                         *credential_files, ON_CALL_CHECK_FAILS_WHILE_THIS_FILE_IS_THERE]))
        cli('reload')
        stop_destination_services_stand_in()


def on_call_scenarios(cli, status, state, events, wait, mark, send_notice, server):
    """The modules pushover and discord
    on the node, against the stand-in at server: a check's failure and its
    recovery, a notice, and a refused answer of each service. suite has
    installed the modules, written their destinations, the files of their
    credentials, and the check group on_call_alert, and reloaded."""
    node = vm.hostname()
    destinations = list(ON_CALL_DESTINATION_OF_MODULE.values())
    # The module asks for version 10 of Discord's API, and for an answer
    # only after Discord saved the message.
    discord_path = destination_services_stand_in.DISCORD_WEBHOOK_PATH.replace('/api/webhooks/', '/api/v10/webhooks/') + '?wait=true'

    def accepted(service):
        return [r for r in received(service) if r['http_status_code'] == ACCEPTED_HTTP_STATUS_CODE[service]]

    # What each service accepted for an alert, by the alert's title with its
    # severity in front.
    def pushover_messages(title):
        # Pushover shows the title above the body, and an alert without a
        # body as its message.
        return [r['payload'] for r in accepted('pushover') if r['path'] == '/1/messages.json' and r['payload'].get('title', r['payload']['message']) == title]

    def discord_messages(title):
        return [r for r in accepted('discord') if r['payload']['content'].split('\n')[0] == as_discord_gets_it(title)]

    def at_every_service(title):
        return pushover_messages(title) and discord_messages(title)

    # The check fails and recovers. Each module closes what the failure
    # opened, by the fleet incident key that every alert about the check
    # carries: 32 lowercase hexadecimal digits, the same on every node.
    wait(lambda: state('on_call_alert/shell')['state'] == 'ok')
    vm.console('touch ' + ON_CALL_CHECK_FAILS_WHILE_THIS_FILE_IS_THERE)
    failing = 'SOS: ' + node + ': on_call_alert/shell failed'
    wait(lambda: at_every_service(failing))
    failed = next(e for e in events() if e.get('check_group') == 'on_call_alert' and e['type'] == 'failed')
    assert failed['title_with_alert_severity'] == failing and failed['node'] == node and 'the queue of payments is full' in failed['body'], failed
    incident, fleet_incident_key = failed['incident_id'], failed.get('fleet_incident_key', '')
    assert re.fullmatch('[0-9a-f]{32}', fleet_incident_key), failed
    vm.console('rm ' + ON_CALL_CHECK_FAILS_WHILE_THIS_FILE_IS_THERE)
    resolved = 'Resolved SOS: ' + node + ': on_call_alert/shell is ok again'
    wait(lambda: pushover_messages(resolved) and discord_messages(resolved))
    recovered = next(e for e in events() if e.get('check_group') == 'on_call_alert' and e['type'] == 'recovered')
    # One node failed, so the destinations whose services keep incidents get
    # the node's own recovery as the closing alert of the check.
    assert recovered['title_with_alert_severity'] == resolved and recovered['incident_id'] == incident and recovered.get('fleet_incident_key') == fleet_incident_key, (recovered, failed)
    # Pushover: the emergency message with the check's tag; at the recovery,
    # first the stop of its repeats by that tag, then the recovery at normal
    # priority.
    tag = 'mmwatchdog_fleet_' + fleet_incident_key
    keys = {'token': destination_services_stand_in.PUSHOVER_APPLICATION_TOKEN, 'user': destination_services_stand_in.PUSHOVER_USER_OR_GROUP_KEY}
    emergency = pushover_messages(failing)[0]
    assert emergency == dict(keys, priority=2, title=failing, message=failed['body'], monospace=1, retry=300, expire=3600, tags=tag), (emergency, failed)
    requests = accepted('pushover')
    stops = [i for i, r in enumerate(requests) if r['path'] == '/1/receipts/cancel_by_tag/' + tag + '.json']
    recoveries = [i for i, r in enumerate(requests) if r['path'] == '/1/messages.json' and r['payload'].get('title') == resolved]
    assert stops and stops[0] < recoveries[0], requests
    assert requests[stops[0]]['content_type'] == 'application/x-www-form-urlencoded' and requests[stops[0]]['payload'] == {'token': destination_services_stand_in.PUSHOVER_APPLICATION_TOKEN}, requests[stops[0]]
    assert requests[recoveries[0]]['payload'] == dict(keys, priority=0, title=resolved, message=recovered['body'], monospace=1), (requests[recoveries[0]], recovered)
    # Discord: both messages through the webhook.
    for title, event in [(failing, failed), (resolved, recovered)]:
        posted = discord_messages(title)[0]
        assert posted['path'] == discord_path and posted['content_type'] == 'application/json', posted
        assert posted['payload'] == {'content': as_discord_gets_it(title, event['body']), 'allowed_mentions': {'parse': []}, 'flags': 4}, (posted, event)
    # Every request came from a module that MMWatchdog ran as the account mmwatchdog.
    accounts = {r['client_account'] for service in ON_CALL_DESTINATION_OF_MODULE for r in received(service)}
    assert accounts == {'mmwatchdog'}, accounts
    mark('the official extra modules pushover and discord, installed in ' + MODULE_DIRECTORY + ' and run as the account mmwatchdog, pass mmwatchdog validate, reach the stand-in over HTTPS through the system trust store, and deliver an sos check\'s failure and its recovery by the fleet incident key of the real event: Pushover the emergency message with the tag mmwatchdog_fleet_<fleet_incident_key>, then a stop of its repeats by that tag before the recovery message; Discord both messages through the webhook, with version 10 of the API and wait=true')

    printed = []
    title = 'Test of the on-call destinations. Nothing is wrong.'
    for name in destinations:
        sent = send_notice(title, name)
        assert sent.stdout == 'Notice sent to the destination ' + name + ':\n  ' + name + '   delivered\n', sent.stdout
        printed += [sent.stdout, sent.stderr]
    assert at_every_service('Info: ' + node + ': ' + title), vm.console('tail -n 8 /var/tmp/mmwatchdog-fixture/destination-services-stand-in.jsonl').stdout
    # A credential that the service does not know, in the file of each
    # destination: the service refuses the notice, and the command and the
    # status say why, without the credential. MMWatchdog reads the file for every
    # delivery, so the queued notice goes out once the file holds the
    # credential that the service knows, with no reload.
    unknown_application_token = 'PushoverFixtureUnknownToken003'
    token_of_a_deleted_webhook = 'mmwatchdogDiscordFixtureDeletedWebhook_2'
    refusals = [
        # The destination, its file, the content that the service refuses,
        # the service's answer as MMWatchdog shows it, and the module's advice.
        ('pushover_on_call', PUSHOVER_APPLICATION_TOKEN_FILE, unknown_application_token,
         'the module pushover reported: Pushover answered 400: application token is invalid, see https://pushover.net/api.',
         ' Pushover does not know this application token. Copy it from pushover.net into the file that application_token_file names.'),
        ('discord_team_channel', DISCORD_WEBHOOK_FILE, server + '/api/webhooks/123456789012345679/' + token_of_a_deleted_webhook,
         'the module discord reported: Discord answered 404: Unknown Webhook (code 10015).',
         ' The webhook was deleted, or the address in the file is wrong. Write the address of a webhook from Discord into the file that webhook_url_file names.'),
    ]
    title = 'After the credential was replaced'
    for name, path, refused_content, answer, advice in refusals:
        vm.write(path, refused_content + '\n', '0600')
        refused = send_notice(title, name, code=1)
        assert refused.stdout.startswith('Notice sent to the destination ' + name + ':\n  ' + name + '   failed: ' + answer + advice), refused.stdout
        assert 'the notice was not delivered at the first attempt to 1 of 1 destinations. MMWatchdog keeps the notice queued and tries again' in refused.stderr, refused.stderr
        printed += [refused.stdout, refused.stderr]
    shown, shown_as_json = cli('status').stdout, cli('status --json').stdout
    failing_destinations = {f['destination']: f for f in json.loads(shown_as_json)['delivery']['failing_destinations']}
    for name, _, _, answer, _ in refusals:
        line = [l for l in shown.splitlines() if l.startswith('Destination ' + name + ': ')]
        assert len(line) == 1 and line[0].startswith('Destination ' + name + ': 1 alert is not delivered. MMWatchdog tries again at ') and 'The latest attempt failed: ' + answer in line[0], shown
        failing_destination = failing_destinations.get(name, {})
        assert failing_destination.get('undelivered_alerts') == 1 and failing_destination['latest_attempt_error'].startswith(answer), failing_destinations
    printed += [shown, shown_as_json]
    credential_files = on_call_credential_files(server)
    for _, path, _, _, _ in refusals:
        vm.write(path, credential_files[path] + '\n', '0600')
    wait(lambda: at_every_service('Info: ' + node + ': ' + title))
    wait(lambda: not [f for f in status()['delivery']['failing_destinations'] if f['destination'] in destinations])
    credentials = [destination_services_stand_in.PUSHOVER_APPLICATION_TOKEN,
                   destination_services_stand_in.PUSHOVER_USER_OR_GROUP_KEY, destination_services_stand_in.DISCORD_WEBHOOK_TOKEN,
                   unknown_application_token, token_of_a_deleted_webhook]
    printed += [cli('status').stdout, cli('events --json').stdout, vm.console('journalctl -u mmwatchdog --since -5min --no-pager', check=False).stdout]
    leaked = [(credential, output[max(0, output.find(credential) - 200):output.find(credential) + 200]) for output in printed for credential in credentials if credential in output]
    assert not leaked, leaked
    mark('mmwatchdog send-notice through the modules pushover and discord prints delivered; an application token that Pushover does not know, and a deleted Discord webhook fail the first attempt with the service\'s answer and what to do, in the command and as a failing destination in mmwatchdog status and mmwatchdog status --json; the queued notice arrives once the file holds the credential that works, without a reload; no output of MMWatchdog and no line of its journal holds an application token, a user or group key, or a webhook token')
