"""Join a second guest to the installed daemon acting as hub.

The peer guest is deployed with the shipped joined-node MMX workflow, which
takes the hub's join code as a secret, and reaches the hub over the guests'
network. The hub sets short fleet timings, so silence, the hub-outage alerter, and direct
delivery take seconds.
"""
from datetime import datetime
import hashlib
import json
import re
import secrets
import shlex
import shutil
import time
import destination_services_stand_in
import official_extra_destination_module_scenarios
import email_scenarios
import lifecycle
import vm

HUB = vm.MAIN_GUEST_HOST + ':18445'
CAPTURE = 'http://' + vm.MAIN_GUEST_HOST + ':18080/events'
TOKEN = '/etc/mmwatchdog/secrets/alerts'
# A second webhook destination that takes only sos alerts; its bearer token
# tells its deliveries apart at the capture endpoint.
SOS_TOKEN = '/etc/mmwatchdog/secrets/sos-only-alerts'
SEVERITY_CHECKS = '/etc/mmwatchdog/check_groups/severity.yml'
# Each node submits email to its own loopback-only Mailpit STARTTLS server.
MAIL_SERVER = 'localhost:' + str(email_scenarios.MAILPIT_SERVERS['starttls_with_login']['smtp_port'])
MAIL_PASSWORD_FILE = email_scenarios.PASSWORD_FILE
MAIL_RECIPIENTS = ['oncall@example.com', 'ops@example.com']
# The Slack and Pushover stand-in runs on the hub guest, under a name
# both guests reach and whose certificate both guests trust.
DESTINATION_SERVICES_HOST = vm.MAIN_GUEST_HOST
SLACK_WEBHOOK = 'https://' + DESTINATION_SERVICES_HOST + ':' + str(destination_services_stand_in.PORT) + destination_services_stand_in.SLACK_WEBHOOK_PATH
PUSHOVER_API_SERVER_URL = 'https://' + DESTINATION_SERVICES_HOST + ':' + str(destination_services_stand_in.PORT)
PUSHOVER_APPLICATION_TOKEN_FILE = official_extra_destination_module_scenarios.PUSHOVER_APPLICATION_TOKEN_FILE
PUSHOVER_USER_OR_GROUP_KEY_FILE = official_extra_destination_module_scenarios.PUSHOVER_USER_OR_GROUP_KEY_FILE
# The check group of the scenario of one incident per check across the
# fleet, on the hub and on the peer, and the files that make its checks fail.
ONE_INCIDENT_PER_CHECK_GROUP = 'one_incident_per_check'
ONE_INCIDENT_PER_CHECK_FILE = '/etc/mmwatchdog/check_groups/one_incident_per_check.yml'
ONE_INCIDENT_PER_CHECK_FAILS_WHILE_THIS_FILE_IS_THERE = '/var/tmp/mmwatchdog-fixture/one-incident-per-check-{check}-fails'
# The scenarios of grouping by exact finding and of setting a finding aside.
# A module of the project's own, on the hub and on the peer, reports the
# finding FLAW with the exact finding that EXACT_FINDING_FILE on its guest
# holds, and no finding while the file is not there, so the suite decides
# when each node reports which exact finding.
EXACT_FINDING_MODULE = 'fleet_suite_exact_finding'
EXACT_FINDING_FILE = '/var/tmp/mmwatchdog-fleet-suite-exact-finding'
EXACT_FINDING_CHECK_GROUP_FILE = '/etc/mmwatchdog/check_groups/exact_finding.yml'
EXACT_FINDING_CHECK = 'exact_finding/flaw'
FLAW = 'CVE-2026-0001 fleetlib'
EXACT_FINDING_MODULE_EXECUTABLE = '''#!/bin/bash
"$MMWATCHDOG_MODULE_HELPER" module-json request >/dev/null || exit 1
exact_finding=$(cat ''' + EXACT_FINDING_FILE + ''' 2>/dev/null)
if [[ -z $exact_finding ]]; then
  exec "$MMWATCHDOG_MODULE_HELPER" module-json result --status ok --message 'fleetlib has no known flaw'
fi
exec "$MMWATCHDOG_MODULE_HELPER" module-json result --status failed --message 'fleetlib has a known flaw' \\
  --finding ''' + shlex.quote(FLAW) + ''' 'fleetlib has the flaw CVE-2026-0001' --exact-finding ''' + shlex.quote(FLAW) + ''' "$exact_finding"
'''
SET_ASIDE_FINDINGS_FILE = '/etc/mmwatchdog/set_aside_findings.yml'
# The fields of an alert that MMHotline's alert intake knows. It refuses a
# request with any other field.
MMHOTLINE_ALERT_FIELDS = {'sender_alert_id', 'sender_incident_key', 'severity', 'title', 'message', 'happened_at'}
# Product minimums: silence still requires three missed reports (3 seconds),
# the hub-outage alerter two (2 seconds), and direct delivery waits another second.
TIMINGS = {'report_every': '1s', 'alert_after_hub_unreachable_for': '2s', 'send_alert_without_hub_after_waiting_for': '1s'}
# Both guests reach the fake MMHotline over HTTP at the hub guest's address.
MMHOTLINE_SECRET = '5a' * 32
MMHOTLINE_ALERT_SOURCE_ID = 'mmwatchdog_fleet_hub'
MMHOTLINE_SECRET_FILE = '/etc/mmwatchdog/secrets/mmhotline-signing-secret'
MMHOTLINE_FILES = (MMHOTLINE_SECRET_FILE + ' /etc/systemd/system/mmwatchdog-mmhotline.service '
                   '/var/tmp/mmwatchdog-fixture/mmhotline_fixture.py '
                   '/var/tmp/mmwatchdog-fixture/mmhotline-secret /var/tmp/mmwatchdog-fixture/mmhotline.jsonl')
# The four timings of the peer's check groups.
PEER_TIMINGS = {'check_every': '1s', 'time_limit_per_run': '500ms', 'alert_after_failing_for': '1m', 'recovered_after_healthy_for': '30s'}
PEER_NODE_GROUPS = ['application_servers', 'frankfurt']
# The measurement history of the joined nodes on the hub, and a node's own.
HUB_HISTORY = '/var/lib/mmwatchdog/measurement_history_of_nodes.db'
PEER_HISTORY = '/var/lib/mmwatchdog/measurement_history.db'
HUB_NODE_GROUPS = ['operations', 'frankfurt']
# The client of the hub's node API, on the peer guest: it runs synthetic
# joined nodes and sends requests that the hub must refuse.
HUB_CLIENT = '/var/tmp/mmwatchdog-fixture/fleet_hub_client.py'
# The synthetic joined nodes of the scenario of fleet alert grouping.
GROUPING_NODES = ['grouping-node-' + str(number) for number in range(1, 6)]
PEER_CONFIG = {'keep_undelivered_alerts_for': '24h', 'undelivered_alerts_content_size_limit': '8MiB',
    'join_fleet_hub': {'join_code_file': '/etc/mmwatchdog/secrets/fleet-hub-join-code'},
    'this_node_is_in_node_groups': PEER_NODE_GROUPS,
    'alert_severity_when_this_node_goes_silent': 'warn',
    # The peer's reboot notice goes out as warn, which the suite checks below.
    'alert_severity_when_this_node_has_rebooted': 'warn',
    'keep_measurement_history_for': '7d',
    'measurement_history_size_limit': '256MiB',
    # The memory check only records its readings: the peer sends them to the
    # hub with its reports.
    'check_groups': {'peer': dict(PEER_TIMINGS, shell='true', alert_severity='warn'),
                     'peer_memory': dict(PEER_TIMINGS, node_resource='memory', only_record_readings=True)},
}


def suite(mark=print):
    peer_guest = vm.PEER

    def mmwatchdog(args, code=None, guest=vm.NAME):
        p = vm.console('/usr/local/bin/mmwatchdog ' + args, check=False, guest=guest)
        if code is not None:
            assert p.returncode == code, (args, p.returncode, p.stdout, p.stderr)
        return p

    def peer(args, code=None):
        return mmwatchdog(args, code, peer_guest)

    def fleet(node=''):
        return json.loads(mmwatchdog('fleet ' + node + ' --json').stdout)

    def peer_status():
        return json.loads(peer('status --json').stdout)

    def wait(predicate, what, timeout=15):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            try:
                value = predicate()
                if value:
                    return value
            except (json.JSONDecodeError, KeyError, StopIteration):
                pass
            time.sleep(0.5)
        raise AssertionError(what + '\n' + mmwatchdog('fleet --json').stdout
                             + vm.console('journalctl -u mmwatchdog -n 30 --no-pager', check=False).stdout
                             + vm.console('journalctl -u mmwatchdog -n 30 --no-pager', check=False, guest=peer_guest).stdout)

    def node(name):
        return next(s for s in fleet()['nodes'] if s['name'] == name)

    def fleet_key(readable):
        check_key = hashlib.sha256(readable.encode()).hexdigest()[:32]
        identity = 'fleet:' + code.split()[2]
        return hashlib.sha256((identity + '\x00' + check_key).encode()).hexdigest()[:32]

    def delivered(predicate):
        """The alerts that reached the webhook and match predicate, with their token."""
        lines = vm.console('cat /var/tmp/mmwatchdog-fixture/webhooks.jsonl', check=False).stdout.splitlines()
        return [dict(r['event'], authorization=r['authorization'])
                for r in (json.loads(line) for line in lines if line) if predicate(r['event'])]

    def alerts(reason):
        """The peer's alerts with this maintenance reason that reached the webhook."""
        return delivered(lambda e: e['node'] == name and (e.get('maintenance') or {}).get('reason') == reason)

    def fleet_alert(node, check, kind):
        return lambda: delivered(lambda e: e['node'] == node and e.get('check_group') == 'node_reports'
                                 and e.get('check') == check and e['type'] == kind)

    def peer_log():
        """The running peer daemon's log, without earlier runs'."""
        return vm.console('journalctl --no-pager _SYSTEMD_INVOCATION_ID="$(systemctl show -p InvocationID --value mmwatchdog)"',
                          check=False, guest=peer_guest).stdout

    def wait_for_the_hub_before_sending_directly(duration):
        """Set the hub's send_alert_without_hub_after_waiting_for, and wait
        until the peer holds it in its copy of the fleet settings."""
        config['fleet_hub_role']['send_alert_without_hub_after_waiting_for'] = duration
        vm.write('/etc/mmwatchdog/mmwatchdog_node_settings.yml', json.dumps(config))
        mmwatchdog('reload', 0)
        wait(lambda: json.loads(vm.console('cat /var/lib/mmwatchdog/fleet-settings-from-the-hub.json', guest=peer_guest).stdout)['fleet']
             .get('send_alert_without_hub_after_waiting_for') == duration, 'the peer holds the hub\'s new wait before it sends directly')

    def restart_hub_and_wait_for_the_peer():
        """Restart the hub's daemon, and wait until the peer has reported to
        the restarted hub."""
        received = node(name)['last_report_received_at']
        vm.console('systemctl restart mmwatchdog')
        wait(lambda: node(name)['last_report_received_at'] != received, 'the peer reports to the restarted hub')

    def mmhotline_received(route):
        """What the fake MMHotline endpoint received on this route."""
        lines = vm.console('cat /var/tmp/mmwatchdog-fixture/mmhotline.jsonl', check=False).stdout.splitlines()
        return [r['body'] for r in (json.loads(line) for line in lines if line) if r['route'] == route]

    def reboot_alerts():
        return delivered(lambda e: e['node'] == name and e['type'] == 'rebooted')

    def readings(database, query, guest=vm.NAME):
        """Rows of a database of MMWatchdog, such as a measurement history, read on the guest that holds it."""
        script = "import json, sqlite3; print(json.dumps(sqlite3.connect('file:" + database + "?mode=ro', uri=True).execute(" + repr(query) + ").fetchall()))"
        return json.loads(vm.console('python3 -c ' + shlex.quote(script), guest=guest).stdout)

    def hub_holds_what_the_peer_sent():
        """The hub holds every reading of the peer up to the last one that it
        received, each once; returns the sequence number of that reading."""
        received, on_hub = readings(HUB_HISTORY, "SELECT (SELECT sequence_number FROM received WHERE node='%s'), (SELECT COUNT(*) FROM readings WHERE node='%s')" % (name, name))[0]
        on_peer = readings(PEER_HISTORY, 'SELECT COUNT(*) FROM readings WHERE id <= %d' % (received or 0), guest=peer_guest)[0][0]
        assert received and on_hub == on_peer, (received, on_hub, on_peer)
        return received

    def reset_peer():
        vm.console(vm.each_unit('stop', ['mmwatchdog', 'mmx-https-api']) + '; systemctl reset-failed mmwatchdog 2>/dev/null; '
                   'rm -rf /etc/mmwatchdog /var/lib/mmwatchdog /var/lib/mmwatchdog-module-state /run/mmwatchdog /usr/local/lib/mmwatchdog /var/lib/mmx/jobs; true', guest=peer_guest)

    work = vm.ROOT / 'fleet-deploy'

    def deploy(code, config, expected=0):
        """Apply the shipped joined-node workflow to the peer with this join code."""
        shutil.rmtree(work, ignore_errors=True)
        shutil.copytree(vm.REPO / 'examples/deploy', work)
        # A release holds the official extra modules beside the workflows.
        shutil.copytree(vm.REPO / 'official_extra_modules', work / 'official_extra_modules')
        shutil.copy(vm.ROOT / ('bin/mmwatchdog-linux-' + vm.ARCH), work / 'mmwatchdog')
        shutil.copy(vm.REPO / 'examples/mmwatchdog.service', work / 'mmwatchdog.service')
        vm.write_configuration_files(work / 'files/join_hub', config)
        (work / 'inventories/hub_members.yml').write_text(json.dumps({'vars': {'mmwatchdog_config_source_dir': 'files/join_hub', 'mmwatchdog_secret_files': []}, 'nodes': {peer_guest: vm.connection(peer_guest)}}))
        # The guide keeps the code in the vault; the test binds it to the environment.
        with (work / 'mmx_project_settings.yml').open('a') as f:
            f.write('\nsecrets_from_env:\n  mmwatchdog_join_code: MMWATCHDOG_JOIN_CODE\n')
        p = vm.cmd([vm.ROOT / 'mmx-bin/mmx', 'run', 'join_hub', '--inventory', 'hub_members', '--json'],
                   cwd=work, check=False, timeout=300, extra_env={'MMWATCHDOG_JOIN_CODE': code,
                       'MMX_RUNNER_DIR': str(vm.ROOT / 'mmx-bin'),
                       'MMX_STATE_DIR': str(vm.ROOT / 'fleet-mmx-state')})
        assert p.returncode == expected, p.stdout[-8000:] + p.stderr
        (vm.ROOT / 'fleet-deploy-result.json').write_text(p.stdout)

    vm.up(peer_guest)
    name = vm.hostname(peer_guest)
    reset_peer()
    # A peer that an interrupted run left with its daemon and its state
    # starts that daemon with the guest, and the daemon delivers what it
    # still held, such as the end of a deploy window, to the receiver of
    # this run. The reset has stopped it, so the receiver's record starts
    # anew here.
    vm.console(': > /var/tmp/mmwatchdog-fixture/webhooks.jsonl')
    original = vm.console('cat /etc/mmwatchdog/mmwatchdog_node_settings.yml').stdout
    original_alerts = vm.console('cat /etc/mmwatchdog/alert_destinations.yml').stdout
    config = json.loads(original)
    config['fleet_hub_role'] = dict(TIMINGS, undelivered_alerts_of_nodes_content_size_limit='32MiB', listen_on_address='0.0.0.0:18445', address_for_nodes=HUB, keep_measurement_history_of_nodes_for='7d', measurement_history_of_nodes_size_limit='1GiB')
    config['this_node_is_in_node_groups'] = HUB_NODE_GROUPS
    hub_alerts = {'destinations': {'capture': {'webhook': {'url': CAPTURE, 'bearer_token_file': TOKEN}, 'time_limit_per_delivery_attempt': '1s', 'remind_every': '1h',
                                               'alert_severities': ['info', 'warn', 'sos']}}}
    try:
        # Before either daemon starts, so each trusts the mail server's
        # certificate from its first connection.
        email_scenarios.start_mail_servers(guests=(vm.NAME, peer_guest))
        vm.write(TOKEN, 'first-alert-token\n', '0600')
        vm.write('/etc/mmwatchdog/alert_destinations.yml', json.dumps(hub_alerts))
        vm.write('/etc/mmwatchdog/mmwatchdog_node_settings.yml', json.dumps(config))
        assert 'requires restarting MMWatchdog' in mmwatchdog('reload', 4).stderr
        vm.console('systemctl restart mmwatchdog')
        hub_name = vm.hostname()
        wait(lambda: node(hub_name)['state'] == 'reporting', 'hub lists itself')
        code = mmwatchdog('fleet join-code', 0).stdout.strip()
        assert code.startswith('mmwatchdog-join ' + HUB + ' '), code
        # A standalone node's alert settings, which the workflow removes.
        vm.write('/etc/mmwatchdog/alert_destinations.yml', '{"destinations": {}}', guest=peer_guest)
        deploy('invalid-join-code', PEER_CONFIG, expected=1)
        assert vm.console('cat /etc/mmwatchdog/secrets/fleet-hub-join-code', guest=peer_guest).stdout == 'invalid-join-code\n'
        assert vm.console('cat /etc/mmwatchdog/alert_destinations.yml', guest=peer_guest).stdout == '{"destinations": {}}'
        vm.console('test ! -e /etc/mmwatchdog/mmwatchdog_node_settings.yml', guest=peer_guest)
        mark('an invalid join code rejects the first candidate before live configuration is installed or alert destinations are removed')
        deploy(code, PEER_CONFIG)
        vm.console('test ! -e /etc/mmwatchdog/alert_destinations.yml', guest=peer_guest)
        # A first install had no MMWatchdog running, so the workflow opened no window.
        assert not [e for e in json.loads(peer('events --json', 0).stdout) if e['type'].startswith('maintenance_')]
        assert vm.console('stat -c %a /etc/mmwatchdog/secrets/fleet-hub-join-code', guest=peer_guest).stdout.strip() == '600'
        # A new node joins with its first report at once.
        peer_node = wait(lambda: (s := node(name))['state'] == 'reporting' and s['exit_code'] == 0 and s,
                           'peer joins and reports healthy')
        assert peer_node['report']['node_resources']['memory']['unit'] == 'percent', peer_node
        assert vm.console('stat -c %a /var/lib/mmwatchdog/hub-credential.json', guest=peer_guest).stdout.strip() == '600'
        link = wait(lambda: (l := peer_status()['joined_fleet_hub']).get('fleet_settings_version') and l, 'peer holds the fleet settings')
        assert link['this_node_name'] == name and link['last_accepted_report_at'] and not link.get('last_error'), link
        assert vm.console('stat -c %a /var/lib/mmwatchdog/fleet-settings-from-the-hub.json', guest=peer_guest).stdout.strip() == '600'
        held = json.loads(vm.console('cat /var/lib/mmwatchdog/fleet-settings-from-the-hub.json', guest=peer_guest).stdout)
        assert held['destinations']['capture']['remind_every'] == '1h', held['destinations']
        assert 'Fleet hub: ' + HUB + ' as ' + name + '; last report' in peer('status').stdout
        vm.write('/etc/mmwatchdog/alert_destinations.yml', '{"destinations": {}}', guest=peer_guest)
        assert 'remove /etc/mmwatchdog/alert_destinations.yml' in peer('validate', 1).stderr
        vm.console('rm /etc/mmwatchdog/alert_destinations.yml', guest=peer_guest)
        wait(lambda: name in fleet()['hub_outage_alerter_nodes'], 'the hub chooses the peer as its hub-outage alerter')
        wait(lambda: peer_status()['checks']['node_reports/to_hub']['state'] == 'ok', 'the hub-outage alerter holds the hub check')
        assert peer_status()['summary']['exit_code'] == 0
        # Joining changes none of the peer's own checks: it runs the checks of
        # its configuration, and as the hub-outage alerter node node_reports/to_hub
        # besides.
        assert sorted(peer_status()['checks']) == ['node_reports/to_hub', 'peer/shell', 'peer_memory/node_resource'], peer_status()['checks']
        text = mmwatchdog('fleet').stdout
        assert text.startswith('Fleet at ' + HUB + ':') and 'Hub-outage alerter nodes: ' + name in text, text
        effective = json.loads(peer('config --json', 0).stdout)
        assert effective['fleet_timings_from_hub'] == TIMINGS, effective
        assert 'capture' in effective['alerts']['destinations'] and 'file_contents_by_destination' not in effective, effective
        retained = json.loads(peer('events peer --last 7d --json', 0).stdout)
        assert all(e.get('check_group') == 'peer' or 'peer' in e.get('check_groups', []) for e in retained), retained
        detail = json.loads(mmwatchdog('fleet ' + name + ' --last 7d --json', 0).stdout)
        assert detail['name'] == name and isinstance(detail['events'], list), detail
        mark('the joined-node MMX workflow installs a second guest with the hub\'s join code and without alert_destinations.yml; it joins over the network, reports its status, node resources, and hub connection, and keeps a private copy of the fleet settings')
        # Node groups: each node says which node groups it is in, and the hub
        # keeps no list of its own.
        assert node(name)['this_node_is_in_node_groups'] == PEER_NODE_GROUPS and node(hub_name)['this_node_is_in_node_groups'] == HUB_NODE_GROUPS, fleet()
        assert '\n  Node groups: ' + ', '.join(PEER_NODE_GROUPS) + '\n' in mmwatchdog('fleet ' + name, 0).stdout
        one_group = json.loads(mmwatchdog('fleet --node-group application_servers --json', 0).stdout)
        assert one_group['node_group'] == 'application_servers' and [s['name'] for s in one_group['nodes']] == [name], one_group
        # The hub of this suite has failing checks of its own, so the whole
        # fleet and the node group that the hub is in exit 1, and the node
        # group of the healthy peer alone exits 0.
        both = mmwatchdog('fleet --node-group frankfurt', 1).stdout
        assert both.startswith('Node group frankfurt of the fleet at ' + HUB + ': 2 nodes, 1 healthy\n' + hub_name + ' '), both
        assert '\n  Node groups: ' + ', '.join(HUB_NODE_GROUPS) + '\n' in both and 'Hub-outage alerter' not in both, both
        assert 'Hub-outage alerter nodes: ' + name in mmwatchdog('fleet', 1).stdout
        unknown = mmwatchdog('fleet --node-group berlin', 4)
        assert 'no node is in a node group named "berlin"; the node groups of this fleet are application_servers, frankfurt, operations' in unknown.stderr, unknown.stderr
        assert 'takes nothing else: remove ' + name + ' or --node-group' in mmwatchdog('fleet ' + name + ' --node-group frankfurt', 4).stderr
        mark('a node states its node groups in this_node_is_in_node_groups, also the hub; mmwatchdog fleet shows them, mmwatchdog fleet --node-group shows the nodes of one node group, and a name that no node states is an error that lists the node groups of the fleet')
        # Measurement history on the hub: the peer sends the readings of its
        # memory check with its reports, and the hub keeps them under the
        # peer's name in a file of its own.
        wait(lambda: readings(HUB_HISTORY, "SELECT COUNT(*) FROM readings WHERE node='%s'" % name)[0][0] > 2, 'the peer\'s readings reach the hub')
        assert vm.console('stat -c %a ' + HUB_HISTORY).stdout.strip() == '600'
        group, check, measurements = readings(HUB_HISTORY, "SELECT check_group, check_name, measurements FROM readings WHERE node='%s' ORDER BY id DESC LIMIT 1" % name)[0]
        memory = json.loads(measurements)['memory_used_percent']
        assert (group, check) == ('peer_memory', 'node_resource') and memory['unit'] == 'percent' and 0 < memory['value'] < 100, (group, check, measurements)
        # The hub's own readings stay in its own measurement history.
        assert readings(HUB_HISTORY, "SELECT COUNT(*) FROM readings WHERE node!='%s'" % name)[0][0] == 0
        hub_holds_what_the_peer_sent()
        node_readings = json.loads(mmwatchdog('fleet readings ' + name + ' peer_memory node_resource --last 1h --json', 0).stdout)
        measurement = node_readings['nodes'][0]['measurements'][0]
        assert node_readings['nodes'][0]['node'] == name and measurement['measurement'] == 'memory_used_percent', node_readings
        assert measurement['summary']['reading_count'] > 0 and all(reading['limit_status'] == 'only records' for reading in measurement['readings']), node_readings
        text = mmwatchdog('fleet readings ' + name + ' peer_memory --last 1h', 0).stdout
        assert 'node_resource\nmemory_used_percent, percent · only records' in text, text
        compressed = mmwatchdog('fleet readings --node-group application_servers peer_memory node_resource --last 1h', 0).stdout
        assert '1 healthy, none beyond the limit: ' + name in compressed and '  ' + name + '   ' not in compressed, compressed
        node_group_readings = json.loads(mmwatchdog('fleet readings --node-group application_servers peer_memory node_resource --json', 0).stdout)
        assert node_group_readings['nodes_with_healthy_readings_count'] == 1 and node_group_readings['nodes'][0]['node'] == name, node_group_readings
        kept = fleet()['measurement_history_of_nodes']
        assert kept['keep_measurement_history_of_nodes_for'] == '7d' and kept['measurement_history_of_nodes_size_limit'] == '1GiB', kept
        assert kept['oldest_reading_at'] and kept['newest_reading_at'] and not kept['cut_short_by_measurement_history_of_nodes_size_limit'], kept
        assert 'measurement history of nodes' not in mmwatchdog('fleet').stdout
        mark('a joined node sends its readings to the hub with its reports; the hub keeps them under the node\'s name in a private file of its own, copies none of its own readings, and says in the fleet view how far back they reach')
        # Without SUDO_USER, the maintenance user is the Linux login user.
        login = vm.console('getent passwd "$(cat /proc/self/loginuid)" | cut -d: -f1', guest=peer_guest).stdout.strip()
        assert login, 'console has no login user'
        vm.console('env -u SUDO_USER /usr/local/bin/mmwatchdog maintenance on --every-check-group --for 5m --reason fleet-test', guest=peer_guest)
        window = peer_status()['maintenance']['every_check_group']
        assert window['user'] == login, (window, login)
        started = wait(lambda: alerts('fleet-test'), 'the peer\'s alert reaches the webhook through the hub', timeout=10)
        assert started[0]['type'] == 'maintenance_on', started
        assert started[0]['authorization'] == 'Bearer first-alert-token', started
        # The peer logs each alert it delivers directly instead.
        assert 'goes directly' not in peer_log()
        wait(lambda: node(name)['report']['maintenance']['every_check_group']['user'] == login, 'peer reports its window')

        def peer_events():
            d = fleet(name)
            return d if [e for e in d['events'] if e['type'] == 'maintenance_on'] else None

        detail = wait(peer_events, 'peer events reach the hub')
        assert detail['events'][-1]['type'] == 'node_joined', detail['events']
        assert detail['report']['checks']['peer/shell']['state'] in {'ok', 'unknown'}, detail
        peer('maintenance off --every-check-group', 0)
        mark('alerts reach the destinations through the hub; reports carry maintenance windows and local events; the maintenance user falls back to the login user')
        # A notice from the peer goes through the hub, and the command on the
        # peer says whether the hub accepted it.
        sent = peer("send-notice --alert-severity warn --title 'failover: this machine is now the active one' --body 'from the fleet suite'", 0)
        assert sent.stdout == ('Notice sent to the MMWatchdog fleet hub at ' + HUB + ', which delivers it to every destination that takes warn:\n'
                               '  MMWatchdog fleet hub   accepted\nTo see the result for each destination, send the notice from the fleet hub: run the same mmwatchdog send-notice command there.\n'), sent.stdout
        notice = wait(lambda: delivered(lambda e: e['type'] == 'notice' and e['node'] == name), 'the peer\'s notice reaches the webhook through the hub', timeout=10)
        assert notice[0]['title'] == name + ': failover: this machine is now the active one' and notice[0]['body'] == 'from the fleet suite', notice
        assert notice[0]['alert_severity'] == 'warn' and not notice[0].get('node_count'), notice
        sent = peer("send-notice --alert-severity info --title 'for one destination' --destination capture", 0)
        assert 'which delivers it to the destination capture:\n  MMWatchdog fleet hub   accepted\n' in sent.stdout, sent.stdout
        wait(lambda: delivered(lambda e: e['type'] == 'notice' and e.get('title') == name + ': for one destination'), 'the hub delivers the peer\'s notice to the named destination', timeout=10)
        refused = peer('send-notice --alert-severity info --title x --destination pager', 4)
        assert 'the MMWatchdog fleet hub has no destination named "pager"; its destinations are: capture' in refused.stderr, refused.stderr
        # On the hub, the command prints the result for each destination.
        sent = mmwatchdog("send-notice --alert-severity warn --title 'from the hub'", 0)
        assert sent.stdout == 'Notice sent to every destination that takes warn:\n  capture   delivered\n', sent.stdout
        mark('a notice from a joined node goes through the hub, also to one destination by name, and the command says that the hub accepted it; on the hub the command prints the result for each destination')
        # A failed systemd unit on the peer: the alert that names the unit
        # goes through the hub, and the hub shows the unit under its check.
        failed_unit = lifecycle.FAILED_UNIT_FIXTURES[0]
        repaired = lifecycle.failed_unit_fixture(failed_unit, guest=peer_guest)
        units_key = 'failed_systemd_units/machine_has_no_failed_systemd_unit'
        units_file = '/etc/mmwatchdog/check_groups/failed_systemd_units.yml'
        vm.write(units_file, json.dumps({'check_groups': {'failed_systemd_units': {
            'check_every': '1s', 'time_limit_per_run': '1s', 'alert_after_failing_for': '1s', 'recovered_after_healthy_for': '1s',
            'checks': {'machine_has_no_failed_systemd_unit': {
                'every_systemd_unit': {'fail_when': 'a_systemd_unit_has_failed', 'systemd_units_to_ignore': lifecycle.already_failed_units(guest=peer_guest)}, 'alert_severity': 'warn'}}}}}), guest=peer_guest)
        try:
            peer('reload', 0)
            wait(lambda: peer_status()['checks'][units_key]['state'] == 'ok', 'the peer\'s failed-units check is healthy')
            vm.console('systemctl start ' + failed_unit, check=False, guest=peer_guest)

            def unit_alerts(kind):
                return delivered(lambda e: e['node'] == name and e.get('finding') == failed_unit and e['type'] == kind)

            failed = wait(lambda: unit_alerts('failed'), 'the alert of the peer\'s failed unit reaches the webhook through the hub')
            assert failed[0]['check_group'] == 'failed_systemd_units' and failed[0]['check'] == 'machine_has_no_failed_systemd_unit', failed
            assert failed[0]['title'] == name + ': ' + units_key + ': ' + failed_unit + ' failed', failed
            assert failed[0]['body'].startswith('systemd unit ' + failed_unit + ' failed (systemd result: exit-code)'), failed
            # A report sent before the incident opened shows the unit in its failing time.
            reported = wait(lambda: (unit := fleet(name)['report']['checks'][units_key].get('findings', {}).get(failed_unit, {})).get('failure_incident') and unit,
                            'the peer reports its failed unit with its incident')
            assert reported['state'] == 'failed' and reported['failure_incident']['incident_id'] == failed[0]['incident_id'], reported
            assert re.search(r'^  ' + re.escape(failed_unit) + r' +failed +systemd unit ', mmwatchdog('fleet ' + name).stdout, re.M)
            # The incident is the peer's own: mmwatchdog status on the peer shows the
            # unit with the incident that the alert names, and exits 1.
            own = peer_status()['checks'][units_key]['findings'][failed_unit]
            assert own['failure_incident']['incident_id'] == failed[0]['incident_id'], own
            peer('status', 1)
            # Another program on the hub, such as MMHotline's dashboard, reads
            # the fleet view through a socket unit of its own: for each
            # connection from its account, systemd runs exactly this command as
            # root, with a read-only file system, no network, and no capabilities.
            view_socket = '/run/mmwatchdog-test-fleet-view.sock'
            view_units = '/etc/systemd/system/mmwatchdog-test-fleet-view'
            vm.console('id mmhotline >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin mmhotline')
            vm.write(view_units + '.socket', '[Socket]\nListenStream=' + view_socket + '\nSocketUser=mmhotline\nSocketGroup=mmhotline\n'
                     'SocketMode=0600\nAccept=yes\n')
            vm.write(view_units + '@.service', '[Service]\nExecStart=/usr/local/bin/mmwatchdog fleet --json\nStandardInput=socket\n'
                     'StandardOutput=socket\nStandardError=journal\nPrivateNetwork=yes\nNoNewPrivileges=yes\nProtectSystem=strict\n'
                     'ProtectHome=yes\nPrivateTmp=yes\nPrivateDevices=yes\nCapabilityBoundingSet=\nRestrictAddressFamilies=AF_UNIX\n')
            read_view = ('import socket, sys\ns = socket.socket(socket.AF_UNIX)\ns.connect(' + repr(view_socket) + ')\n'
                         'sys.stdout.buffer.write(b"".join(iter(lambda: s.recv(65536), b"")))\n')
            try:
                vm.console('systemctl daemon-reload; systemctl start mmwatchdog-test-fleet-view.socket')
                read = json.loads(vm.console('runuser -u mmhotline -- python3 -c ' + shlex.quote(read_view)).stdout)
                entry = [n for n in read['nodes'] if n['name'] == name][0]
                assert entry['state'] == 'reporting' and entry['exit_code'] == 1 and read['hub_outage_alerter_nodes'] == [name], (entry, read['hub_outage_alerter_nodes'])
                assert [a for a in entry['report']['summary']['attention'] if a['check'] == units_key and a.get('finding') == failed_unit and a['state'] == 'failed'], entry['report']['summary']
                refused = vm.console('runuser -u mmhotline -- /usr/local/bin/mmwatchdog fleet forget ' + name, check=False)
                assert refused.returncode != 0 and name in [n['name'] for n in fleet()['nodes']], refused
            finally:
                vm.console('systemctl stop mmwatchdog-test-fleet-view.socket; rm -f ' + view_units + '.socket ' + view_units + '@.service; '
                           'systemctl daemon-reload; userdel mmhotline 2>/dev/null; true')
            mark('another account on the hub reads the fleet view through a socket unit that runs exactly mmwatchdog fleet --json as root without capabilities: the peer with its failed unit under report.summary.attention, and the hub-outage alerter nodes; the same account cannot forget the peer')
            vm.console('touch ' + repaired + '; systemctl start ' + failed_unit, guest=peer_guest)
            recovered = wait(lambda: unit_alerts('recovered'), 'the recovery of the peer\'s unit reaches the webhook through the hub')
            assert recovered[0]['incident_id'] == failed[0]['incident_id'] and recovered[0]['title'] == name + ': ' + units_key + ': ' + failed_unit + ' is ok again', recovered
            wait(lambda: not fleet(name)['report']['checks'][units_key].get('findings'), 'the peer reports no failed unit')
            peer('status', 0)
        finally:
            vm.console('rm -f ' + units_file + ' ' + repaired + '; systemctl reset-failed ' + failed_unit + ' 2>/dev/null; true', guest=peer_guest)
            peer('reload', 0)
        wait(lambda: node(name)['exit_code'] == 0 and units_key not in fleet(name)['report']['checks'], 'the peer reports healthy without the check')
        mark('a failed systemd unit on a joined node sends its alert and its recovery through the hub with the unit in the title, and the hub shows the unit under its check; '
             'the incident and the exit code of mmwatchdog status are the node\'s own, and joining added only node_reports/to_hub to its checks')
        vm.write(TOKEN, 'second-alert-token\n', '0600')
        reloaded = mmwatchdog('reload', 0).stdout
        # A new token for the same address is the same destination: reload
        # does not name it as changed, and its queued alerts stay.
        assert 'Destination capture:' not in reloaded, reloaded
        wait(lambda: peer_status()['joined_fleet_hub']['fleet_settings_version'] != link['fleet_settings_version'], 'peer fetches the changed settings')
        peer('maintenance on peer --for 5m --reason credential-test', 0)
        changed = wait(lambda: alerts('credential-test'), 'the peer\'s alert goes out with the new credential', timeout=10)
        assert changed[0]['authorization'] == 'Bearer second-alert-token', changed
        peer('maintenance off peer', 0)
        mark('a changed alert token on the hub applies after its reload without counting as another destination, and nodes fetch it with the settings within a report interval')
        vm.console('systemctl restart mmwatchdog')
        restored = wait(lambda: node(name), 'the restarted hub serves its saved fleet')
        assert restored['state'] in {'reporting', 'waiting_for_first_report'}
        received = restored['last_report_received_at']
        wait(lambda: node(name)['last_report_received_at'] != received, 'peer reports to the restarted hub')
        mark('a restarted hub keeps its nodes, which keep reporting with their credentials')
        # An update deploy runs inside the workflow's whole-node window.
        release = dict(PEER_CONFIG, check_groups=dict(PEER_CONFIG['check_groups'], release=dict(PEER_TIMINGS, shell='true', alert_severity='warn')))
        deploy(code, release)
        assert 'every_check_group' not in peer_status()['maintenance']
        wait(lambda: [e for e in fleet(name)['events'] if e['type'] == 'maintenance_off'
                      and e['maintenance']['reason'] == 'mmwatchdog deploy'], 'the deploy\'s window end reaches the hub')
        started = wait(lambda: alerts('mmwatchdog deploy'), 'the deploy\'s start alert reaches the webhook through the hub', timeout=10)
        assert [(e['type'], e.get('check_group')) for e in started] == [('maintenance_on', 'every_check_group')], started
        mark('the joined-node workflow opens no window on a first install and runs an update inside a whole-node window: the hub sends its start alert and receives its end')
        # A reboot inside a whole-node window.
        # The command returns once the hub has the window, so a reboot may
        # follow at once, without waiting for the peer's next report.
        held = vm.console("SUDO_USER=deployer /usr/local/bin/mmwatchdog maintenance on --every-check-group --for 5m --reason 'deploy v2'", guest=peer_guest).stdout
        assert 'The MMWatchdog fleet hub has the window, so it sends no alert that this node stopped reporting until the window ends.' in held, held
        assert ((node(name)['report'].get('maintenance') or {}).get('every_check_group') or {}).get('reason') == 'deploy v2', node(name)
        # Hold the peer down past its silence deadline before rebooting, so
        # only the window can keep the hub quiet.
        vm.console('systemctl stop mmwatchdog', guest=peer_guest)
        wait(lambda: node(name)['state'] == 'silent', 'the hub sees the peer go silent')
        vm.reboot(peer_guest)
        # Transient units do not survive reboot; the binary and trusted certificate do.
        email_scenarios.start_mailpit_processes(guest=peer_guest)
        rebooted = wait(lambda: (s := fleet(name))['state'] == 'reporting' and 'release/shell' in s['report']['checks'] and s,
                        'the peer reports again after its reboot', timeout=30)
        assert rebooted['report']['maintenance']['every_check_group']['reason'] == 'deploy v2', rebooted['report']['maintenance']
        vm.console('/usr/local/bin/mmwatchdog maintenance off --every-check-group', guest=peer_guest)
        wait(lambda: not (node(name)['report'].get('maintenance') or {}).get('every_check_group'), 'the peer reports its window ended')
        time.sleep(4)
        silence_alerts = delivered(lambda e: e.get('check_group') == 'node_reports' and e.get('check') == name)
        assert not silence_alerts, ('silence alert during the deploy', silence_alerts, vm.console('date --utc +%FT%T.%N; timedatectl show -p NTPSynchronized', check=False).stdout,
                                    vm.console('date --utc +%FT%T.%N; timedatectl show -p NTPSynchronized', check=False, guest=peer_guest).stdout)
        deploy_alerts = alerts('deploy v2')
        assert [e['type'] for e in deploy_alerts] == ['maintenance_on'], deploy_alerts
        # The window keeps the reboot local; the first install and restarts sent none.
        assert not reboot_alerts(), 'reboot alert during the deploy'
        assert [e for e in json.loads(peer('events --json', 0).stdout) if e['type'] == 'rebooted'], 'the peer recorded no reboot'
        wait(lambda: name in fleet()['hub_outage_alerter_nodes'], 'the peer watches the hub again')
        mark('mmwatchdog maintenance on returns once the hub has the window; a node rebooted inside a whole-node window stays quiet: the window holds its silence alert and keeps its reboot alert local, and it reports again with the window and the new release')
        # An email destination on the hub. Its password file is on the hub
        # only; the peer gets the password with its copy of the fleet settings.
        vm.write(MAIL_PASSWORD_FILE, email_scenarios.SMTP_LOGIN_PASSWORD + '\n', '0600')
        mailbox = {'remind_every': 'never', 'email': {'smtp_server': MAIL_SERVER, 'smtp_connection_security': 'starttls', 'from': 'mmwatchdog@fleet.example.com', 'to': MAIL_RECIPIENTS,
                             'login_user': email_scenarios.SMTP_LOGIN_USER, 'login_password_file': MAIL_PASSWORD_FILE},
                   'time_limit_per_delivery_attempt': '2s', 'alert_severities': ['info']}
        # A destination that delivers through the official extra module
        # slack. The file with the webhook's address is on the hub only; the
        # peer gets its content with its copy of the fleet settings. The
        # module itself is on both: the peer's deploy installed it with
        # every official extra module, and the hub, which this suite does
        # not deploy, gets it here. The module is a script that MMWatchdog runs
        # itself, so neither node needs anything else for it.
        official_extra_destination_module_scenarios.start_destination_services_stand_in([DESTINATION_SERVICES_HOST], guests=(vm.NAME, peer_guest))
        official_extra_destination_module_scenarios.install_modules()
        on_peer = vm.console('stat -c "%a %U:%G" ' + official_extra_destination_module_scenarios.MODULE_DIRECTORY + '/slack/module.star && cat ' + official_extra_destination_module_scenarios.MODULE_DIRECTORY + '/slack/module.star', guest=peer_guest).stdout
        assert on_peer == '644 root:root\n' + (vm.REPO / 'official_extra_modules/slack/module.star').read_text(), on_peer
        vm.write(official_extra_destination_module_scenarios.SLACK_WEBHOOK_FILE, SLACK_WEBHOOK + '\n', '0600')
        team_chat = {'remind_every': 'never', 'slack': {'webhook_url_file': official_extra_destination_module_scenarios.SLACK_WEBHOOK_FILE}, 'alert_severities': ['info']}
        # A destination whose module keeps incidents of its own, for the
        # scenario of one incident per check across the fleet further down.
        # It takes no severity, so only the checks that name it reach it.
        vm.write(PUSHOVER_APPLICATION_TOKEN_FILE, destination_services_stand_in.PUSHOVER_APPLICATION_TOKEN + '\n', '0600')
        vm.write(PUSHOVER_USER_OR_GROUP_KEY_FILE, destination_services_stand_in.PUSHOVER_USER_OR_GROUP_KEY + '\n', '0600')
        on_call_pager = {'remind_every': 'never', 'pushover': {'application_token_file': PUSHOVER_APPLICATION_TOKEN_FILE, 'user_or_group_key_file': PUSHOVER_USER_OR_GROUP_KEY_FILE, 'repeat_sos_alerts_every': '5m', 'stop_repeating_sos_alerts_after': '1h', 'api_server_url': PUSHOVER_API_SERVER_URL}, 'alert_severities': []}
        vm.write('/etc/mmwatchdog/alert_destinations.yml', json.dumps({'destinations': dict(hub_alerts['destinations'], mailbox=mailbox, team_chat=team_chat, on_call_pager=on_call_pager)}))
        reloaded = mmwatchdog('reload', 0)
        assert 'Destination on_call_pager: Pushover accepted the verification; no message was sent' in reloaded.stdout, reloaded.stdout
        verifications = official_extra_destination_module_scenarios.received('pushover', verifications=True)
        assert verifications and all(r['http_status_code'] == 200 and r['path'] == '/1/users/validate.json' and r['payload'] == {'token': destination_services_stand_in.PUSHOVER_APPLICATION_TOKEN, 'user': destination_services_stand_in.PUSHOVER_USER_OR_GROUP_KEY} for r in verifications), verifications
        held = wait(lambda: (h := json.loads(vm.console('cat /var/lib/mmwatchdog/fleet-settings-from-the-hub.json', guest=peer_guest).stdout)).get('file_contents_by_destination', {}).get('mailbox') and h,
                    'the peer holds the mail server password')
        assert 'credential_content_fingerprint' not in json.dumps(held), held
        assert held['file_contents_by_destination']['mailbox'] == {'login_password_file': email_scenarios.SMTP_LOGIN_PASSWORD} and held['destinations']['mailbox']['email']['login_password_file'] == MAIL_PASSWORD_FILE, held
        vm.console('test ! -e ' + MAIL_PASSWORD_FILE, guest=peer_guest)
        assert held['file_contents_by_destination']['team_chat'] == {'webhook_url_file': SLACK_WEBHOOK}, held
        assert held['file_contents_by_destination']['on_call_pager'] == {'application_token_file': destination_services_stand_in.PUSHOVER_APPLICATION_TOKEN, 'user_or_group_key_file': destination_services_stand_in.PUSHOVER_USER_OR_GROUP_KEY}, held
        vm.console('test ! -e ' + official_extra_destination_module_scenarios.SLACK_WEBHOOK_FILE + ' && test ! -e ' + PUSHOVER_APPLICATION_TOKEN_FILE + ' && test ! -e ' + PUSHOVER_USER_OR_GROUP_KEY_FILE, guest=peer_guest)
        vm.console('systemctl stop mmwatchdog')
        # With the hub down, the window is on on the peer, and the command
        # says that the hub does not know of it and exits 1.
        unknown = peer('maintenance on peer --for 5m --reason fallback-test', 1)
        assert 'MMWatchdog fleet hub did not receive the node\'s report; the hub does not know about the maintenance window' in unknown.stderr and peer_status()['maintenance']['peer']['reason'] == 'fallback-test', unknown
        direct = wait(lambda: alerts('fallback-test'), 'the peer delivers directly while the hub is down')
        assert 'goes directly' in peer_log()
        assert direct[0]['type'] == 'maintenance_on', direct
        assert direct[0]['authorization'] == 'Bearer second-alert-token', direct
        # The peer sends the email itself, to its own Mailpit server, after a
        # login with the password that only its copy of the settings holds.
        mailed = wait(lambda: [m for m in email_scenarios.received(guest=peer_guest) if 'fallback-test' in m['text']], 'the peer sends the email itself while the hub is down')
        assert mailed[0]['subject'] == 'Info: ' + name + ': maintenance on for peer' and mailed[0]['message_id'] == '<' + direct[0]['id'] + '@fleet.example.com>', mailed
        # This server accepts mail only after STARTTLS; arrival proves encryption.
        assert mailed[0]['recipients'] == MAIL_RECIPIENTS and mailed[0]['port'] == email_scenarios.MAILPIT_SERVERS['starttls_with_login']['smtp_port'] and mailed[0]['logged_in_as'] == email_scenarios.SMTP_LOGIN_USER, mailed
        assert not [m for m in email_scenarios.received() if m['message_id'] == mailed[0]['message_id']], mailed
        # The peer runs its own slack module, which posts to the webhook
        # whose address only the peer's copy of the settings holds.
        posted = wait(lambda: [r for r in official_extra_destination_module_scenarios.received('slack') if 'fallback-test' in r['payload']['text']], 'the peer runs its own slack module while the hub is down')
        assert posted[0]['payload']['text'].startswith('Info: ' + name + ': maintenance on for peer\n') and posted[0]['http_status_code'] == 200, posted
        assert posted[0]['client_address'] in vm.addresses(peer_guest), posted
        mark('while the hub is down, mmwatchdog maintenance on on a node turns the window on, says that the hub does not know of it, and exits 1; a node delivers its alerts directly with its copy of the settings, including the changed credential; it sends an email destination\'s message itself, logging in with the mail server password from that copy; it runs its own installed slack module with the webhook address from that copy')
        # The hub-outage alerter sends directly at alert_after_hub_unreachable_for after its last report.
        down = wait(fleet_alert(name, 'to_hub', 'failed'), 'the hub-outage alerter alerts that the hub is unreachable')
        assert down[0]['message'].startswith('no report from this node has reached the MMWatchdog fleet hub since '), down
        assert down[0]['title'] == name + ' cannot reach the MMWatchdog fleet hub' and down[0]['body'].startswith(down[0]['message']), down
        # The hub-outage alerter's journal names each alert that it delivered, where it
        # went, and that the destination accepted it.
        accepted = r'delivered the alert "%s" to alert destination \S+, which accepted it'
        wait(lambda: re.search(accepted % ('[^"]*' + re.escape(name + ' cannot reach the MMWatchdog fleet hub')), peer_log()),
             'the hub-outage alerter\'s journal names its direct alert and the destination that accepted it')
        resolutions_before = len(re.findall(accepted % 'Resolved [^"]*', peer_log()))
        # The peer keeps running its own checks while the hub is down.
        observed = peer_status()['checks']['peer_memory/node_resource']['last_observation']['at']
        wait(lambda: peer_status()['checks']['peer_memory/node_resource']['last_observation']['at'] != observed, 'the peer runs its checks while the hub is down')
        vm.console('systemctl start mmwatchdog')
        wait(fleet_alert(name, 'to_hub', 'recovered'), 'the hub-outage alerter\'s next report resolves its hub incident')
        wait(lambda: len(re.findall(accepted % 'Resolved [^"]*', peer_log())) > resolutions_before,
             'the hub-outage alerter\'s journal names the resolution and the destination that accepted it')
        # The returning hub gets the peer's current report, with the window
        # that the peer opened while the hub was down, and the event of that
        # window, which the peer still holds.
        wait(lambda: [e for e in fleet(name)['events'] if e['type'] == 'maintenance_on' and (e.get('maintenance') or {}).get('reason') == 'fallback-test'],
             'the returning hub gets the event that the peer recorded while the hub was down')
        assert node(name)['report']['maintenance']['peer']['reason'] == 'fallback-test', node(name)['report']['maintenance']
        mark('the hub\'s hub-outage alerter alerts directly after alert_after_hub_unreachable_for of failed reports and recovers on its next report, '
             'and its journal names each alert and resolution with the destination that accepted it; '
             'the peer runs its checks while the hub is down, and the returning hub gets its current report and the event that it recorded meanwhile')
        # While the hub was down the peer kept storing its readings. Now it
        # sends what the hub missed, in order and each once.
        missed = readings(PEER_HISTORY, 'SELECT MAX(id) FROM readings', guest=peer_guest)[0][0]
        wait(lambda: readings(HUB_HISTORY, "SELECT sequence_number FROM received WHERE node='%s'" % name)[0][0] >= missed, 'the peer sends the readings that the hub missed')
        hub_holds_what_the_peer_sent()
        mark('after a hub outage a node sends the readings that the hub missed, in order and each once')
        # A hub that cannot save. Two triggers in the hub's databases make its
        # writes fail as a full disk would: every write of a report into
        # fleet.db, also of the hub's own, which would otherwise clear the
        # storage problem that the peer's report raised, and every write of
        # an alert of the peer into the alert intake of state.db, which only
        # the alerts of other nodes raise and clear. The hub answers the
        # peer's reports and alerts with 503 and saves nothing of them, but
        # counts each authenticated report as contact. The peer, the hub
        # hub-outage alerter node, also alerts directly that its reports do not reach the
        # hub, which is why this scenario comes after the scenario of the
        # hub-outage alerter.
        hub_storage_failure = 'a storage failure that the fleet suite causes'
        hub_storage_triggers = [('/var/lib/mmwatchdog/fleet.db', 'fleet_suite_fails_every_report', 'BEFORE UPDATE ON nodes'),
                                ('/var/lib/mmwatchdog/state.db', 'fleet_suite_fails_the_peers_alerts', "BEFORE INSERT ON intake WHEN NEW.node = '%s'" % name)]

        def change_hub_database(database, statements):
            script = 'import sqlite3; c = sqlite3.connect(%r, timeout=30); c.executescript(%r); c.close()' % (database, statements)
            vm.console('python3 -c ' + shlex.quote(script))

        def peer_on_the_hub():
            """What fleet.db on the hub holds of the peer: its row, with its saved
            report and the cursor of its events, and the number and the newest
            of its events."""
            return readings('/var/lib/mmwatchdog/fleet.db', "SELECT (SELECT hex(data) FROM nodes WHERE name='%s'), (SELECT COUNT(*) FROM events WHERE node='%s'), (SELECT MAX(id) FROM events WHERE node='%s')" % (name, name, name))[0]

        def storage_test_events_on_the_hub():
            return [e for e in fleet(name)['events'] if e['type'] == 'maintenance_on' and (e.get('maintenance') or {}).get('reason') == 'storage-test']

        peer('maintenance off peer', 0)
        wait(lambda: 'peer' not in (node(name)['report'].get('maintenance') or {}), 'the hub holds the peer\'s report without a window')
        earlier_silence_alerts = {e['id'] for e in delivered(lambda e: e.get('check_group') == 'node_reports' and e.get('check') == name)}
        try:
            for database, trigger, when in hub_storage_triggers:
                change_hub_database(database, 'CREATE TRIGGER ' + trigger + ' ' + when + " BEGIN SELECT RAISE(FAIL, '" + hub_storage_failure + "'); END;")
            report_refused = wait(lambda: '503' in (e := peer_status()['joined_fleet_hub'].get('last_error', '')) and e, 'the hub answers the peer\'s report with 503')
            assert 'report could not be saved' in report_refused, report_refused
            # From here on the hub saves nothing of the peer.
            held = peer_on_the_hub()
            assert hub_storage_failure in fleet()['storage_error'] and 'Storage: ' in mmwatchdog('fleet').stdout, fleet()['storage_error']
            # An alert that the hub cannot save goes out directly. The hub
            # cannot save the report with the window either, and the command
            # says so.
            refused = peer('maintenance on peer --for 5m --reason storage-test', 1)
            assert 'does not know about the maintenance window' in refused.stderr and 'HTTP 503' in refused.stderr, refused
            direct = wait(lambda: alerts('storage-test'), 'the peer delivers its alert directly while the hub cannot save it')
            assert direct[0]['type'] == 'maintenance_on', direct
            assert [line for line in peer_log().splitlines() if 'goes directly' in line and 'HTTP 503' in line], peer_log()[-4000:]
            assert 'alert intake: ' in json.loads(mmwatchdog('status --json').stdout)['storage_error']
            # The hub counts the peer's reports as contact past three report
            # intervals after the refused report, and sends no silence alert.
            refused_at = datetime.fromisoformat(node(name)['last_report_received_at'])
            wait(lambda: (datetime.fromisoformat(node(name)['last_report_received_at']) - refused_at).total_seconds() >= 4, 'the hub counts the peer\'s reports as contact')
            assert node(name)['state'] == 'reporting', node(name)
            assert not delivered(lambda e: e.get('check_group') == 'node_reports' and e.get('check') == name and e['id'] not in earlier_silence_alerts), 'a silence alert while the hub could not save'
            # The hub saved nothing of the reports that it refused: neither the
            # report with the window nor the window's event, and the cursor of
            # the peer's events stays where it was.
            assert peer_on_the_hub() == held, (peer_on_the_hub(), held)
            assert 'peer' not in (node(name)['report'].get('maintenance') or {}) and not storage_test_events_on_the_hub(), node(name)['report'].get('maintenance')
        finally:
            for database, trigger, _ in hub_storage_triggers:
                change_hub_database(database, 'DROP TRIGGER IF EXISTS ' + trigger + ';')
        # The next report is saved whole: the window and its event, which the
        # peer still holds, reach the hub with it.
        wait(lambda: storage_test_events_on_the_hub() and not fleet()['storage_error'], 'the hub saves the peer\'s report and its event again')
        assert peer_on_the_hub()[1] > held[1] and node(name)['report']['maintenance']['peer']['reason'] == 'storage-test', (peer_on_the_hub(), held)
        assert not peer_status()['joined_fleet_hub'].get('last_error'), peer_status()['joined_fleet_hub']
        peer('maintenance off peer', 0)
        wait(lambda: not json.loads(mmwatchdog('status --json').stdout)['storage_error'], 'the hub saves the peer\'s alerts again')
        wait(lambda: peer_status()['checks']['node_reports/to_hub']['state'] == 'ok', 'the peer\'s reports reach the hub again')
        mark('a hub that cannot save answers reports and alerts with 503 and shows the storage problem; the node delivers its alert directly, the hub counts its reports as contact and saves none of them, '
             'its cursor stays, and the next report that it can save brings the window and the event that it missed')
        # Two scenarios with nodes that do not report: the stopped peer, and
        # five synthetic joined nodes. Maintenance of the hub's node_reports
        # holds their silence alerts. fleet_hub_client.py on the peer guest
        # sends the hub's node API what a joined node sends.
        vm.write(HUB_CLIENT, (vm.REPO / 'tests/integration/fleet_hub_client.py').read_text(), guest=peer_guest)

        def hub_client(requests):
            """Send requests to the hub from the peer guest with fleet_hub_client.py, and return its answers."""
            return json.loads(vm.cmd(vm.ssh_args(peer_guest) + ['python3 ' + HUB_CLIENT + ' ' + shlex.quote(code)], input=json.dumps(requests)).stdout)

        def fleet_incident_key(check_group, check):
            return fleet_key(check_group + '/' + check + ' failed')

        def alert_now():
            return time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())

        grouped_alerts_held = 'fleet_suite_holds_the_grouped_alerts'
        mmwatchdog('maintenance on node_reports --for 10m --reason fleet-suite-nodes-that-do-not-report', 0)
        try:
            # Reports and alerts are untrusted input, also from a joined node
            # with its own credential. With the peer's daemon stopped, so that
            # nothing else changes what the hub holds of the peer, the peer
            # guest sends the hub requests with the peer's credential that are
            # not valid, each with a mark that would show if the hub kept any
            # of it. The hub refuses each with 400, after it has checked the
            # credential: a wrong one gets 401.
            vm.console('systemctl stop mmwatchdog', guest=peer_guest)
            held, contact = peer_on_the_hub(), node(name)['last_report_received_at']
            forged = 'forged_by_the_fleet_suite'
            peer_credential = '/var/lib/mmwatchdog/hub-credential.json'
            report = {'daemon_start_id': secrets.token_hex(16), 'report_sequence_number': 1, 'event_history_id': secrets.token_hex(16), 'events_after_sequence_number': -1,
                      'measurement_history_id': secrets.token_hex(16), 'readings_after_sequence_number': -1, 'summary': {'exit_code': 1, 'attention': []},
                      'alert_severity_when_this_node_goes_silent': 'warn', 'this_node_is_in_node_groups': [forged]}
            alert = {'id': secrets.token_hex(16), 'fleet_incident_key': fleet_incident_key('untrusted_input', forged), 'node': hub_name, 'check_group': 'untrusted_input',
                     'check': forged, 'type': 'failed', 'alert_severity': 'warn', 'at': alert_now(), 'message': forged}
            refused = hub_client([
                {'path': '/v1/report', 'credential_file': peer_credential, 'body': dict(report, forged_field=forged)},
                {'path': '/v1/report', 'credential_file': peer_credential, 'body': dict(report, alert_severity_when_this_node_goes_silent='loud')},
                {'path': '/v1/report', 'credential_file': peer_credential, 'raw_body_parts': [['not json ' + forged, 1]]},
                # One KiB more than the 4 MiB of a report.
                {'path': '/v1/report', 'credential_file': peer_credential, 'raw_body_parts': [['{"daemon_start_id":"', 1], ['x', 4 * 1024 * 1024 + 1024], ['"}', 1]]},
                {'path': '/v1/alerts', 'credential_file': peer_credential, 'body': dict(alert, forged_field=forged)},
                # One KiB more than the 64 KiB of an alert.
                {'path': '/v1/alerts', 'credential_file': peer_credential, 'body': dict(alert, message=forged + 'x' * (65 * 1024))},
                {'path': '/v1/report', 'credential': 'a' * 64, 'body': report}])
            assert [r['http_status_code'] for r in refused] == [400] * 6 + [401], refused
            assert all(r['answer'].get('error') for r in refused), refused
            # The hub holds of the peer what it held before, counts none of the
            # requests as contact, and saved none of the alerts.
            assert peer_on_the_hub() == held and node(name)['last_report_received_at'] == contact, (peer_on_the_hub(), held, node(name)['last_report_received_at'], contact)
            assert readings('/var/lib/mmwatchdog/state.db', "SELECT COUNT(*) FROM intake WHERE CAST(event AS TEXT) LIKE '%" + forged + "%'") == [[0]]
            assert forged not in json.dumps(fleet(name)) and not delivered(lambda e: e.get('check_group') == 'untrusted_input'), fleet(name)
            vm.console('systemctl start mmwatchdog', guest=peer_guest)
            wait(lambda: node(name)['last_report_received_at'] != contact and node(name)['state'] == 'reporting' and node(name)['this_node_is_in_node_groups'] == PEER_NODE_GROUPS, 'the peer reports again')
            mark('the hub refuses a report or an alert from a joined node with its own credential that has an unknown field, an invalid value, no JSON, or is larger than its limit, with 400, '
                 'and holds of the node what it held before; a wrong credential gets 401')
            # Fleet alert grouping across several nodes. Five synthetic joined
            # nodes send failures of one check group. The first alert of an
            # alert grouping key goes out at once; matching alerts that follow
            # within the grouping minute wait for one combined message. The
            # scenario holds the matching alerts itself, with a trigger in the
            # hub's state.db that refuses to queue a message of the check group
            # for a destination, until both are in the alert intake, so they
            # wait together however long this takes. Then a hub restart, which
            # forgets when each key last sent a message, ends the minute.
            joined = hub_client([{'path': '/v1/join', 'body': {'name': node_name, 'secret': code.split()[3], 'alert_severity_when_this_node_goes_silent': 'warn'}}
                                 for node_name in GROUPING_NODES])
            assert [j['http_status_code'] for j in joined] == [200] * len(GROUPING_NODES), joined
            credentials = {node_name: j['answer']['credential'] for node_name, j in zip(GROUPING_NODES, joined)}

            def grouping_alert(node_name, check, alert_severity):
                return {'path': '/v1/alerts', 'credential': credentials[node_name], 'body': {
                    'id': secrets.token_hex(16), 'fleet_incident_key': fleet_incident_key('fleet_alert_grouping', check), 'node': node_name,
                    'check_group': 'fleet_alert_grouping', 'check': check, 'type': 'failed', 'alert_severity': alert_severity, 'at': alert_now(), 'message': check + ' fails'}}

            def grouped(predicate):
                return delivered(lambda e: e.get('check_group') == 'fleet_alert_grouping' and predicate(e))

            assert [a['http_status_code'] for a in hub_client([grouping_alert('grouping-node-1', 'http', 'warn')])] == [200]
            first = wait(lambda: grouped(lambda e: e['node'] == 'grouping-node-1'), 'the first alert of the check goes out at once')
            assert first[0]['title'] == 'grouping-node-1: fleet_alert_grouping/http failed' and not first[0].get('nodes'), first
            # Another alert severity and another check are other alert grouping
            # keys: the first alert of each goes out at once, by itself.
            assert [a['http_status_code'] for a in hub_client([grouping_alert('grouping-node-4', 'http', 'sos'), grouping_alert('grouping-node-5', 'tcp', 'warn')])] == [200, 200]
            sos = wait(lambda: grouped(lambda e: e['node'] == 'grouping-node-4'), 'the first sos alert of the check goes out at once')
            other_check = wait(lambda: grouped(lambda e: e['node'] == 'grouping-node-5'), 'the first alert of the other check goes out at once')
            assert sos[0]['alert_severity'] == 'sos' and sos[0]['check'] == 'http' and not sos[0].get('nodes'), sos
            assert other_check[0]['alert_severity'] == 'warn' and other_check[0]['check'] == 'tcp' and not other_check[0].get('nodes'), other_check
            change_hub_database('/var/lib/mmwatchdog/state.db', 'CREATE TRIGGER ' + grouped_alerts_held + " BEFORE INSERT ON outbox WHEN json_extract(NEW.event, '$.check_group') = 'fleet_alert_grouping'"
                                " BEGIN SELECT RAISE(FAIL, 'the fleet suite holds the alerts of fleet_alert_grouping'); END;")
            assert [a['http_status_code'] for a in hub_client([grouping_alert('grouping-node-2', 'http', 'warn'), grouping_alert('grouping-node-3', 'http', 'warn')])] == [200, 200]
            in_intake = readings('/var/lib/mmwatchdog/state.db', "SELECT node FROM intake WHERE node IN ('grouping-node-2', 'grouping-node-3') ORDER BY seq")
            assert in_intake == [['grouping-node-2'], ['grouping-node-3']], in_intake
            assert not grouped(lambda e: e['node'] in ('grouping-node-2', 'grouping-node-3') or e.get('nodes')), 'a held alert went out'
            # The restart comes while the trigger holds the messages, so the
            # restarted hub plans the message of both waiting alerts together.
            restart_hub_and_wait_for_the_peer()
            change_hub_database('/var/lib/mmwatchdog/state.db', 'DROP TRIGGER ' + grouped_alerts_held + ';')
            combined = wait(lambda: grouped(lambda e: e.get('nodes')), 'the hub sends the waiting alerts of two nodes as one message')
            assert len(combined) == 1 and combined[0]['nodes'] == ['grouping-node-2', 'grouping-node-3'] and combined[0]['node_count'] == 2, combined
            assert combined[0]['alert_severity'] == 'warn' and combined[0]['check'] == 'http' and combined[0]['type'] == 'failed', combined
            assert combined[0]['title'] == 'fleet_alert_grouping/http failed on 2 nodes', combined
            assert combined[0]['message'] == 'fleet_alert_grouping/http failed on 2 nodes: grouping-node-2, grouping-node-3', combined
            assert not grouped(lambda e: e['node'] in ('grouping-node-2', 'grouping-node-3')), 'a grouped alert also went out by itself'
            mark('fleet alert grouping across nodes: the first alert of a check goes out at once, the next matching alerts of two nodes become one message that names both nodes, '
                 'and an alert of another alert severity or another check goes out by itself')
        finally:
            vm.console('systemctl start mmwatchdog', guest=peer_guest)
            change_hub_database('/var/lib/mmwatchdog/state.db', 'DROP TRIGGER IF EXISTS ' + grouped_alerts_held + ';')
            for node_name in GROUPING_NODES:
                mmwatchdog('fleet forget ' + node_name)
            mmwatchdog('maintenance off node_reports', 0)
        # One incident per check across the fleet. The hub's destination
        # on_call_pager names the module pushover, whose service keeps
        # incidents of its own: it gets the failure of every node, and one
        # closing alert when the last node has recovered. The destination
        # capture keeps no incidents and gets every alert of every node. The
        # hub queues each alert for all its destinations in one step, so
        # once capture has a node's recovery, the hub has also decided what
        # on_call_pager gets for it; and it delivers to each destination in
        # order, so an early closing message would reach Pushover before
        # the last recovery's closing message. Both nodes have the check group with two
        # checks, which recover in the two orders. The hub's checks are sos
        # and the peer's warn: fleet alert grouping holds the second failure
        # of a check at one severity for the rest of the grouping minute, and
        # never makes alerts of different severities one message.
        recovers_on_the_peer_first, recovers_on_the_hub_first = 'peer_recovers_first', 'hub_recovers_first'
        one_incident_checks = [recovers_on_the_peer_first, recovers_on_the_hub_first]

        def fails_while_this_file_is_there(check):
            return ONE_INCIDENT_PER_CHECK_FAILS_WHILE_THIS_FILE_IS_THERE.format(check=check)

        def one_incident_check_group(alert_severity):
            return {'check_groups': {ONE_INCIDENT_PER_CHECK_GROUP: {
                'check_every': '500ms', 'time_limit_per_run': '500ms', 'alert_after_failing_for': '0s', 'recovered_after_healthy_for': '0s',
                'checks': {check: {'shell': 'test ! -e ' + fails_while_this_file_is_there(check), 'alert_severity': alert_severity,
                                   'send_alerts_only_to_destinations': ['capture', 'on_call_pager']} for check in one_incident_checks}}}}

        def group_and_check(check):
            return ONE_INCIDENT_PER_CHECK_GROUP + '/' + check

        def pushover_requests_by_check():
            """The messages and cancellations Pushover accepted for each check, in order."""
            received = official_extra_destination_module_scenarios.received('pushover')
            assert all(r['http_status_code'] == 200 for r in received), received
            return {check: [r for r in received
                            if (r['path'] == '/1/messages.json' and group_and_check(check) in r['payload'].get('title', ''))
                            or r['path'] == '/1/receipts/cancel_by_tag/mmwatchdog_fleet_' + fleet_key(group_and_check(check) + ' failed') + '.json']
                    for check in one_incident_checks}

        def own_alert_at_capture(node_name, check, kind):
            return delivered(lambda e: e['node'] == node_name and e.get('check_group') == ONE_INCIDENT_PER_CHECK_GROUP and e.get('check') == check and e['type'] == kind)

        def failure_event(events, node_name, check):
            return next(e for e in events if e.get('node') == node_name and e.get('check_group') == ONE_INCIDENT_PER_CHECK_GROUP and e.get('check') == check and e['type'] == 'failed')

        try:
            vm.write(ONE_INCIDENT_PER_CHECK_FILE, json.dumps(one_incident_check_group('sos')))
            vm.write(ONE_INCIDENT_PER_CHECK_FILE, json.dumps(one_incident_check_group('warn')), guest=peer_guest)
            mmwatchdog('reload', 0)
            peer('reload', 0)

            def healthy_on_both_nodes():
                on_hub, on_peer = json.loads(mmwatchdog('status --json').stdout)['checks'], peer_status()['checks']
                return all(on_hub[group_and_check(c)]['state'] == 'ok' and on_peer[group_and_check(c)]['state'] == 'ok' for c in one_incident_checks)

            wait(healthy_on_both_nodes, 'both checks are healthy on both nodes')
            every_file = ' '.join(fails_while_this_file_is_there(check) for check in one_incident_checks)
            vm.console('touch ' + every_file)
            vm.console('mkdir -p /var/tmp/mmwatchdog-fixture && touch ' + every_file, guest=peer_guest)
            wait(lambda: all({r['payload'].get('title') for r in requests} == {
                'SOS: ' + hub_name + ': ' + group_and_check(check) + ' failed',
                'Warning: ' + name + ': ' + group_and_check(check) + ' failed'}
                for check, requests in pushover_requests_by_check().items()),
                 'the failure of each check on both nodes reaches Pushover')
            hub_events, peer_events = json.loads(mmwatchdog('events --json', 0).stdout), json.loads(peer('events --json', 0).stdout)
            fleet_incident_key = {}
            for check, requests in pushover_requests_by_check().items():
                on_hub, on_peer = failure_event(hub_events, hub_name, check), failure_event(peer_events, name, check)
                fleet_incident_key[check] = key = on_hub.get('fleet_incident_key', '')
                assert key == fleet_key(group_and_check(check) + ' failed') and on_peer.get('fleet_incident_key') == key, (on_hub, on_peer)
                # These short bodies fit without Pushover's 1,024-character cut.
                assert len(on_hub['body']) <= 1024 and len(on_peer['body']) <= 1024, (on_hub, on_peer)
                # Only the sos failure is an emergency and carries the check's tag.
                expected = [
                    dict(token=destination_services_stand_in.PUSHOVER_APPLICATION_TOKEN,
                         user=destination_services_stand_in.PUSHOVER_USER_OR_GROUP_KEY,
                         priority=2, title=on_hub['title_with_alert_severity'], message=on_hub['body'],
                         monospace=1, retry=300, expire=3600, tags='mmwatchdog_fleet_' + key),
                    dict(token=destination_services_stand_in.PUSHOVER_APPLICATION_TOKEN,
                         user=destination_services_stand_in.PUSHOVER_USER_OR_GROUP_KEY,
                         priority=0, title=on_peer['title_with_alert_severity'], message=on_peer['body'],
                         monospace=1),
                ]
                assert all(r['path'] == '/1/messages.json' and r['content_type'] == 'application/json' for r in requests), requests
                assert sorted((r['payload'] for r in requests), key=lambda m: m['title']) == sorted(expected, key=lambda m: m['title']), (requests, on_hub, on_peer)
            failures_at_pushover = pushover_requests_by_check()
            assert fleet_incident_key[recovers_on_the_peer_first] != fleet_incident_key[recovers_on_the_hub_first], fleet_incident_key
            # The first recovery of each check: one on the peer, the other on
            # the hub. capture gets both; Pushover gets nothing.
            vm.console('rm ' + fails_while_this_file_is_there(recovers_on_the_peer_first), guest=peer_guest)
            vm.console('rm ' + fails_while_this_file_is_there(recovers_on_the_hub_first))
            first_recoveries = wait(lambda: (on_peer := own_alert_at_capture(name, recovers_on_the_peer_first, 'recovered'))
                                    and (on_hub := own_alert_at_capture(hub_name, recovers_on_the_hub_first, 'recovered')) and on_peer + on_hub,
                                    'capture gets the first recovery of each check, the peer\'s of one and the hub\'s of the other')
            assert {(e['check'], e['fleet_incident_key']) for e in first_recoveries} == {(check, fleet_incident_key[check]) for check in one_incident_checks}, first_recoveries
            assert pushover_requests_by_check() == failures_at_pushover, pushover_requests_by_check()
            # The last recovery cancels the emergency repeats by tag, then
            # sends a closing message at normal priority that names both nodes.
            vm.console('rm ' + fails_while_this_file_is_there(recovers_on_the_peer_first))
            vm.console('rm ' + fails_while_this_file_is_there(recovers_on_the_hub_first), guest=peer_guest)
            both_nodes = ', '.join(sorted([hub_name, name]))

            def closed_on_every_node():
                return all('Resolved SOS: ' + group_and_check(check) + ' is ok on every node again'
                           in [r['payload'].get('title') for r in requests]
                           for check, requests in pushover_requests_by_check().items())

            wait(closed_on_every_node, 'the last recovery of each check cancels its repeats and sends the fleet closing message')
            record = pushover_requests_by_check()
            for check, requests in record.items():
                assert requests[:2] == failures_at_pushover[check] and len(requests) == 4, requests
                cancellation, closing = requests[2:]
                assert cancellation['path'] == '/1/receipts/cancel_by_tag/mmwatchdog_fleet_' + fleet_incident_key[check] + '.json', cancellation
                assert cancellation['content_type'] == 'application/x-www-form-urlencoded' and cancellation['payload'] == {'token': destination_services_stand_in.PUSHOVER_APPLICATION_TOKEN}, cancellation
                assert closing['path'] == '/1/messages.json' and closing['content_type'] == 'application/json', closing
                message = closing['payload']
                assert message == dict(token=destination_services_stand_in.PUSHOVER_APPLICATION_TOKEN,
                                       user=destination_services_stand_in.PUSHOVER_USER_OR_GROUP_KEY,
                                       priority=0, title='Resolved SOS: ' + group_and_check(check) + ' is ok on every node again',
                                       message=message['message'], monospace=1), message
                assert message['message'].split('\n')[0] == group_and_check(check) + ' is ok on every node again (was failing on ' + both_nodes + ')', message
        finally:
            for guest in (vm.NAME, peer_guest):
                vm.console('rm -f ' + ONE_INCIDENT_PER_CHECK_FILE + ' ' + ' '.join(fails_while_this_file_is_there(check) for check in one_incident_checks), guest=guest)
            mmwatchdog('reload', 0)
            peer('reload', 0)
        # Nothing more reached Pushover while the nodes reloaded without the checks.
        assert pushover_requests_by_check() == record, (pushover_requests_by_check(), record)
        mark('a destination of the hub whose service keeps incidents, pushover, gets both failures of each check on the hub and a joined node: '
             'the hub sends an emergency message tagged mmwatchdog_fleet_<fleet_incident_key>, the same key as the peer event; the peer sends a normal-priority warning; '
             'the first recovery sends Pushover nothing while capture gets it; the last recovery cancels the tag once and sends one normal-priority closing message naming both nodes; '
             'the same whether the peer or hub recovers first, with nothing more when both nodes reload without the checks')
        vm.console('systemctl stop mmwatchdog', guest=peer_guest)
        silent = wait(fleet_alert(hub_name, name, 'failed'), 'the hub alerts that the peer stopped reporting')
        assert silent[0]['message'].startswith(name + ' has sent no report to the hub since '), silent
        assert silent[0]['title'] == name + ' stopped reporting to the MMWatchdog fleet hub' and silent[0]['body'].startswith(silent[0]['message']), silent
        assert node(name)['state'] == 'silent'
        # A silent node keeps its node groups, and the view of its node group
        # exits with its code.
        in_group = mmwatchdog('fleet --node-group application_servers', 1).stdout
        assert in_group.startswith('Node group application_servers of the fleet at ' + HUB + ': 1 node, 0 healthy\n' + name + ' '), in_group
        assert '\n  Node groups: ' + ', '.join(PEER_NODE_GROUPS) + '\n' in in_group and 'Hub-outage alerter' not in in_group, in_group
        vm.console('systemctl start mmwatchdog', guest=peer_guest)
        back = wait(fleet_alert(hub_name, name, 'recovered'), 'the peer\'s next report resolves its silence incident')
        assert back[0]['message'] == name + ' is reporting again', back
        assert back[0]['title'] == name + ' reports to the MMWatchdog fleet hub again' and back[0]['body'] == back[0]['message'], back
        mark('the hub alerts when a node misses three report intervals and when it reports again')
        # A step of the hub's clock forward by a minute, while the peer
        # reports every second, sends no silence alert: the hub counts the
        # three report intervals after the peer's last report on the
        # monotonic clock. A count on the system clock would find every
        # deadline passed at once. Every certificate of the suite stays
        # valid a minute later: openssl makes them valid for a day from the
        # moment it made them, and MMWatchdog its hub's for a hundred years.
        wait(lambda: node(name)['state'] == 'reporting', 'the peer reports')

        def silence_incidents():
            return {e['id'] for e in json.loads(mmwatchdog('events node_reports --json', 0).stdout) if e.get('check') == name and e['type'] == 'failed'}

        before = silence_incidents()
        # The clock goes back to this machine's time also when a step
        # fails, so no later suite on the guest sees it ahead.
        vm.set_clock(seconds_ahead_of_this_machine=60)
        try:
            # Five report intervals. On the system clock, the hub would open
            # the silence incident at its next tick, a tenth of a second
            # after the step.
            time.sleep(5)
            assert node(name)['state'] == 'reporting', node(name)
            assert silence_incidents() == before, ('a silence incident after the hub\'s clock stepped forward', silence_incidents() - before)
        finally:
            vm.set_clock()
        mark('a step of the hub\'s clock forward by a minute sends no silence alert for a node that reports every second, and the fleet view shows the node reporting')
        assert not reboot_alerts(), 'reboot alert after a daemon restart'
        # network-online.target, after which MMWatchdog starts, does not wait for an
        # address everywhere. If the daemon starts before the network is
        # ready, its first attempt to reach the hub can fail. With the default
        # send_alert_without_hub_after_waiting_for (30s), a server sends the
        # alert through the hub at the next attempt, a second later; the
        # suite's 1s would send it directly. So the peer waits as long as
        # such a server while it reboots.
        wait_for_the_hub_before_sending_directly('30s')
        vm.reboot(peer_guest)
        # Transient units do not survive reboot; the binary and trusted certificate do.
        email_scenarios.start_mailpit_processes(guest=peer_guest)
        reboots = wait(reboot_alerts, 'the peer\'s reboot alert reaches the webhook through the hub', timeout=30)
        assert not reboots[0].get('check_group') and reboots[0]['message'].startswith(name + ' rebooted at '), reboots
        assert '; MMWatchdog last ran at ' in reboots[0]['message'] and 'goes directly' not in peer_log(), (reboots, peer_log()[-4000:])
        # The notice has the peer's alert_severity_when_this_node_has_rebooted.
        assert reboots[0]['alert_severity'] == 'warn', reboots
        received = node(name)['last_report_received_at']
        vm.console('systemctl restart mmwatchdog', guest=peer_guest)
        wait(lambda: node(name)['last_report_received_at'] != received, 'the restarted peer reports again')
        time.sleep(4)
        assert len(reboot_alerts()) == 1, reboot_alerts()
        wait_for_the_hub_before_sending_directly(TIMINGS['send_alert_without_hub_after_waiting_for'])
        mark('a reboot outside a whole-node window sends one rebooted alert through the hub, at the node\'s alert_severity_when_this_node_has_rebooted; daemon restarts within a boot send none')
        rotated = mmwatchdog('fleet rotate-join-code', 0).stdout.strip()
        assert rotated != code and rotated.rsplit(' ', 1)[0] == code.rsplit(' ', 1)[0], rotated
        assert mmwatchdog('fleet join-code', 0).stdout.strip() == rotated
        received = node(name)['last_report_received_at']
        wait(lambda: node(name)['last_report_received_at'] != received, 'the peer keeps reporting after rotation')
        mark('rotating the join code keeps joined nodes reporting with their credentials')
        assert 'Removed node ' + name + ' from the fleet;' in mmwatchdog('fleet forget ' + name, 0).stdout
        assert readings(HUB_HISTORY, "SELECT (SELECT COUNT(*) FROM readings), (SELECT COUNT(*) FROM received)") == [[0, 0]]
        assert name not in [s['name'] for s in fleet()['nodes']] and not fleet()['hub_outage_alerter_nodes']
        assert 'node_reports/' + name not in json.loads(mmwatchdog('status --json').stdout)['checks']
        # Still running, the peer's credential is refused, and its old code joins nothing.
        wait(lambda: 'invalid join code' in peer_status()['joined_fleet_hub'].get('last_error', ''), 'the old code is refused')
        assert fleet()['refused_joins']['count'] >= 1 and name not in [s['name'] for s in fleet()['nodes']]
        deploy(rotated, release)
        wait(lambda: node(name)['state'] == 'reporting', 'the peer joins again with the new code')
        # It sends the readings that it still has, from the oldest on.
        retained = readings(PEER_HISTORY, 'SELECT MAX(id) FROM readings', guest=peer_guest)[0][0]
        wait(lambda: (readings(HUB_HISTORY, "SELECT sequence_number FROM received WHERE node='%s'" % name) or [[0]])[0][0] >= retained, 'the peer sends its readings again')
        hub_holds_what_the_peer_sent()
        # Its old join event went with its data; the peer resends its retained local events.
        assert [e['type'] for e in fleet(name)['events']].count('node_joined') == 1
        wait(lambda: name in fleet()['hub_outage_alerter_nodes'], 'the peer becomes the hub-outage alerter again')
        mark('forgetting a running node deletes it, its credential, and its readings on the hub; it joins again visibly, only with the new join code delivered by MMX, and sends the readings that it still has')
        vm.console('systemctl stop mmwatchdog && rm -f /var/lib/mmwatchdog/fleet.db* && systemctl start mmwatchdog')
        wait(lambda: node(hub_name)['state'] == 'reporting', 'the new hub lists itself')
        moved = mmwatchdog('fleet join-code', 0).stdout.strip()
        assert moved.split()[2] != rotated.split()[2], (moved, rotated)
        wait(lambda: 'certificate does not match' in peer_status()['joined_fleet_hub'].get('last_error', ''),
             'the peer refuses the new hub\'s certificate')
        refused = peer('maintenance on peer --for 5m --reason move-test', 1)
        assert 'does not know about the maintenance window' in refused.stderr and 'certificate does not match' in refused.stderr, refused
        wait(lambda: alerts('move-test'), 'the peer alerts directly until it has the new join code')
        deploy(moved, release)
        code = moved  # Expected incident keys now use the new hub's certificate identity.
        wait(lambda: node(name)['state'] == 'reporting', 'the peer joins the new hub')
        mark('a hub with a new identity: nodes alert directly until MMX delivers its join code, then join it')
        # MMHotline runs on every address of the hub guest, so the peer
        # can also send directly while the hub's monitoring process is down.
        vm.write('/var/tmp/mmwatchdog-fixture/mmhotline_fixture.py', (vm.REPO / 'tests/integration/mmhotline_fixture.py').read_text())
        vm.write('/var/tmp/mmwatchdog-fixture/mmhotline-secret', MMHOTLINE_SECRET + '\n', '0600')
        vm.write('/etc/systemd/system/mmwatchdog-mmhotline.service', '[Unit]\nDescription=Fake MMHotline\n[Service]\n'
                 'ExecStart=/usr/bin/python3 /var/tmp/mmwatchdog-fixture/mmhotline_fixture.py\nRestart=no\n')
        vm.console('systemctl daemon-reload; systemctl start mmwatchdog-mmhotline')
        # systemctl returns before the program listens.
        deadline = time.monotonic() + 10
        while vm.console("ss -ltn | grep -q ':8484 '", check=False).returncode:
            assert time.monotonic() < deadline, vm.console('journalctl -u mmwatchdog-mmhotline -n 20 --no-pager', check=False).stdout
            time.sleep(0.2)
        vm.write(MMHOTLINE_SECRET_FILE, MMHOTLINE_SECRET + '\n', '0600')
        mmhotline_address = vm.console('getent hosts ' + vm.MAIN_GUEST_HOST, guest=peer_guest).stdout.split()[0]
        mmhotline_url = 'http://' + mmhotline_address + ':8484'
        vm.write('/etc/mmwatchdog/alert_destinations.yml', 'destinations:\n  capture:\n    webhook: {url: \'' + CAPTURE + '\', bearer_token_file: ' + TOKEN +
                 '}\n    time_limit_per_delivery_attempt: 1s\n    remind_every: 1h\n    alert_severities: [info, warn, sos]\n'
                 '  mailbox:\n    email:\n      smtp_server: ' + MAIL_SERVER + '\n      smtp_connection_security: starttls\n      from: mmwatchdog@fleet.example.com\n'
                 '      to: [' + ', '.join(MAIL_RECIPIENTS) + ']\n      login_user: ' + email_scenarios.SMTP_LOGIN_USER + '\n      login_password_file: ' + MAIL_PASSWORD_FILE +
                 '\n    remind_every: never\n    time_limit_per_delivery_attempt: 2s\n    alert_severities: [info]\n'
                 '  mmhotline_on_call:\n    mmhotline:\n      mmhotline_url: ' + mmhotline_url +
                 '\n      mmhotline_alert_source_id: "' + MMHOTLINE_ALERT_SOURCE_ID + '"\n      mmhotline_signing_secret_file: ' + MMHOTLINE_SECRET_FILE +
                 '\n    alert_severities: [info, warn, sos]\n')
        # The address is the hub guest's own, which the peer reaches too, so
        # validation gives no loopback warning.
        validated = mmwatchdog('validate', 0)
        assert '127.0.0.1 points to the joined node' not in validated.stdout + validated.stderr, validated
        mmwatchdog('reload', 0)
        wait(lambda: [a for a in mmhotline_received('check-in') if a['sent_by_node_name'] == hub_name],
             'the first check-in reaches MMHotline at once', timeout=10)
        mark('the hub validates and reloads the MMHotline destination with a private signing secret file; its first check-in arrives at once')
        # The MMX deploy above restarted the peer inside its own whole-node window.
        # While that window's alerts still wait for direct delivery, the peer
        # sends a later alert of the same check directly too, to keep their order,
        # and the hub would not send it. So the peer's queue must be empty first.
        wait(lambda: peer_status()['delivery']['queued_notifications'] == 0, 'the peer has delivered the alerts of the deploy')
        peer('maintenance on --every-check-group --for 5m --reason mmhotline-window', 0)
        notice = wait(lambda: [a for a in mmhotline_received('alert-intake') if a['title'] == name + ': maintenance on for the whole node'], 'the peer\'s notice reaches MMHotline through the hub')
        assert notice[0]['severity'] == 'info' and 'sender_incident_key' not in notice[0], notice
        # The hub also sends the peer's alert to its email destination.
        mailed = wait(lambda: [m for m in email_scenarios.received() if 'mmhotline-window' in m['text']], 'the hub sends the peer\'s alert by email')
        assert mailed[0]['subject'] == 'Info: ' + name + ': maintenance on for the whole node' and mailed[0]['logged_in_as'] == email_scenarios.SMTP_LOGIN_USER, mailed
        assert mailed[0]['port'] == email_scenarios.MAILPIT_SERVERS['starttls_with_login']['smtp_port'], mailed
        assert not [m for m in email_scenarios.received(guest=peer_guest) if m['message_id'] == mailed[0]['message_id']], mailed
        # The hub sends a check-in every 30 seconds, and at once when
        # it reloads. Once the hub has the peer's report with the window, a
        # reload brings the next check-in forward instead of waiting
        # up to 30 seconds for it.
        wait(lambda: ((node(name)['report'].get('maintenance') or {}).get('every_check_group') or {}).get('reason') == 'mmhotline-window',
             'the hub sees the peer\'s window')
        # Check-ins sent earlier can list the deploy's window of the
        # step before, which the peer's earlier report still held, so the
        # wait looks for this window's reason.
        mmwatchdog('reload', 0)
        wait(lambda: [a for a in mmhotline_received('check-in')
                      if [(n['node_name'], n.get('reason')) for n in a.get('nodes_in_whole_node_maintenance', [])] == [(name, 'mmhotline-window')]],
             'a check-in lists the peer\'s window', timeout=45)
        status = json.loads(mmwatchdog('status --json').stdout)
        destination = status['mmhotline_destinations'][0]
        assert destination['destination'] == 'mmhotline_on_call' and destination.get('last_signed_answer_at'), status
        assert destination['delivery_problems'] == [], status
        mark('the hub sends the peer\'s alerts to MMHotline as signed generic alerts, verifies signed receipts, and lists the peer\'s whole-node window in its check-ins; it sends the same alert to its email destination')
        peer('maintenance off --every-check-group', 0)
        wait(lambda: not (node(name)['report'].get('maintenance') or {}).get('every_check_group'), 'the hub sees the peer\'s window end')

        def mmhotline_alert(title):
            return lambda: [a for a in mmhotline_received('alert-intake') if a['title'] == title]

        # A silence failure and its resolution through the hub. Earlier steps
        # sent silence alerts too, and fleet alert grouping would hold these
        # for the rest of its one-minute window after them. A hub restart
        # forgets when it last sent each kind of alert, so the hub restarts
        # first, and the peer reports to it again.
        restart_hub_and_wait_for_the_peer()
        vm.console('systemctl stop mmwatchdog', guest=peer_guest)
        silent = wait(mmhotline_alert(name + ' stopped reporting to the MMWatchdog fleet hub'), 'the hub sends the silence failure to MMHotline', timeout=75)
        assert silent[0]['severity'] == 'warn' and silent[0]['sender_incident_key'] == fleet_key('node_reports stopped reporting'), silent
        vm.console('systemctl start mmwatchdog', guest=peer_guest)
        back = wait(mmhotline_alert(name + ' reports to the MMWatchdog fleet hub again'), 'the hub resolves the silence incident in the words that name the node', timeout=75)
        assert len(back) == 1 and back[0]['severity'] == 'resolved-warn' and back[0]['sender_incident_key'] == fleet_key('node_reports stopped reporting'), back
        mark('the hub sends a joined node\'s silence to MMHotline at the node\'s alert_severity_when_this_node_goes_silent, warn, and one resolution when it reports again')
        held = wait(lambda: (h := json.loads(vm.console('cat /var/lib/mmwatchdog/fleet-settings-from-the-hub.json', guest=peer_guest).stdout)).get('file_contents_by_destination', {}).get('mmhotline_on_call') and h, 'the peer holds the signing secret')
        assert held['file_contents_by_destination']['mmhotline_on_call'] == {'mmhotline_signing_secret_file': MMHOTLINE_SECRET}, held
        assert held['destinations']['mmhotline_on_call']['mmhotline']['mmhotline_alert_source_id'] == MMHOTLINE_ALERT_SOURCE_ID, held
        wait(lambda: name in fleet()['hub_outage_alerter_nodes'], 'the peer watches the hub again')
        vm.console('systemctl stop mmwatchdog')
        refused = peer('maintenance on peer --for 5m --reason mmhotline-fallback', 1)
        assert 'does not know about the maintenance window' in refused.stderr, refused
        direct = wait(mmhotline_alert(name + ': maintenance on for peer'), 'the peer sends straight to MMHotline while the hub is down')
        assert direct[0]['message'].endswith('Sent directly by the MMWatchdog node ' + name + ' because the MMWatchdog fleet hub did not accept the alert in time.'), direct
        unreachable = wait(mmhotline_alert(name + ' cannot reach the MMWatchdog fleet hub'), 'the hub-outage alerter sends its sos alert straight to MMHotline')
        assert unreachable[0]['severity'] == 'sos' and unreachable[0]['sender_incident_key'] == fleet_key('node_reports/to_hub failed') and 'Sent directly by' in unreachable[0]['message'], unreachable
        vm.console('systemctl start mmwatchdog')
        resolved = wait(mmhotline_alert('Reports from ' + name + ' reach the MMWatchdog fleet hub again'), 'the returning hub resolves the hub-outage alerter\'s incident', timeout=75)
        assert len(resolved) == 1 and resolved[0]['severity'] == 'resolved-sos' and resolved[0]['sender_incident_key'] == fleet_key('node_reports/to_hub failed'), resolved
        mark('a joined node holds the hub\'s signing secret and sends alerts the hub did not accept straight to MMHotline; the returning hub resolves them')
        # Alert severity: a destination that takes only sos, a hub check
        # raised to sos by time, and the count escalation of silent nodes.
        # The check alerts after failing for 3s and is raised after failing
        # for 5s: both count from the start of its failing time.
        vm.write(SOS_TOKEN, 'sos-only-alert-token\n', '0600')
        vm.console('printf %s ' + shlex.quote('  sos_only:\n    webhook: {url: \'' + CAPTURE + '\', bearer_token_file: ' + SOS_TOKEN +
                   '}\n    remind_every: never\n    time_limit_per_delivery_attempt: 1s\n    alert_severities: [sos]\n') + ' >> /etc/mmwatchdog/alert_destinations.yml')
        vm.write(SEVERITY_CHECKS, json.dumps({'check_groups': {'severity_by_time': {
            'check_every': '1s', 'time_limit_per_run': '500ms', 'alert_after_failing_for': '3s', 'recovered_after_healthy_for': '500ms',
            'alert_severity': 'warn', 'raise_to_sos_after_failing_for': '5s', 'shell': 'false'}}}))
        config['fleet_hub_role']['raise_to_sos_when_silent_nodes_reach_percent'] = 100
        vm.write('/etc/mmwatchdog/mmwatchdog_node_settings.yml', json.dumps(config))
        mmwatchdog('reload', 0)

        def sos_only(predicate):
            return [e for e in delivered(predicate) if e['authorization'] == 'Bearer sos-only-alert-token']

        def to_capture(predicate):
            return [e for e in delivered(predicate) if e['authorization'] == 'Bearer second-alert-token']

        def by_time(kind):
            return lambda e: e['node'] == hub_name and e.get('check_group') == 'severity_by_time' and e['type'] == kind

        settings = json.loads(mmwatchdog('status --json').stdout)['checks']['severity_by_time/shell']['alert_severity_settings']
        assert settings == {'alert_severity': 'warn', 'raise_to_sos_after_failing_for': '5s'}, settings
        assert '  Alert severity: warn\n  Raise to SOS after failing for: 5s\n' in mmwatchdog('status severity_by_time').stdout
        raised = wait(lambda: sos_only(by_time('failure_alert_severity_raised')), 'the check raised to sos reaches the sos-only destination', timeout=75)
        assert raised[0]['alert_severity'] == 'sos' and raised[0]['message'] == 'severity_by_time/shell has been failing for 5s, so this alert is now SOS (raise_to_sos_after_failing_for: 5s)', raised
        failed = wait(lambda: [e for e in to_capture(by_time('failed')) if e['incident_id'] == raised[0]['incident_id']], 'the warn alert reaches capture', timeout=75)
        assert failed[0]['alert_severity'] == 'warn', failed
        # The warn alert went out 3s into the failing time, so the raise at 5s
        # follows it by about 2s, not by another 5s.
        raised_after_alert = (datetime.fromisoformat(raised[0]['at']) - datetime.fromisoformat(failed[0]['at'])).total_seconds()
        assert 0 < raised_after_alert < 5, (raised_after_alert, failed[0], raised[0])
        assert not sos_only(by_time('failed')), 'a warn failure reached the sos-only destination'
        key = fleet_key('severity_by_time/shell failed')
        sos_at_mmhotline = wait(lambda: [a for a in mmhotline_received('alert-intake') if a.get('sender_incident_key') == key and a['severity'] == 'sos'], 'the raise reaches MMHotline', timeout=75)
        assert sos_at_mmhotline[0]['title'] == hub_name + ': severity_by_time/shell still failing, now SOS', sos_at_mmhotline
        assert [a['severity'] for a in mmhotline_received('alert-intake') if a.get('sender_incident_key') == key] == ['warn', 'sos']
        vm.console('rm ' + SEVERITY_CHECKS)
        mmwatchdog('reload', 0)
        retired = wait(lambda: sos_only(by_time('closed_without_recovery')), 'the retirement goes to the destination that got the raise', timeout=75)
        assert retired[0]['alert_severity'] == 'sos' and retired[0]['alert_severities_sent_for_incident'] == ['warn', 'sos'], retired
        closed = wait(lambda: [a for a in mmhotline_received('alert-intake') if a.get('sender_incident_key') == key and a['severity'].startswith('resolved-')], 'MMHotline gets the resolution', timeout=75)
        assert len(closed) == 1 and closed[0]['severity'] == 'resolved-sos', closed
        assert [a['severity'] for a in mmhotline_received('alert-intake') if a.get('sender_incident_key') == key] == ['warn', 'sos', 'resolved-sos']
        mark('a check failing for longer than raise_to_sos_after_failing_for is raised to sos, counted from the start of its failing time and not from its alert: a destination that takes only sos gets the raise as its first message and the retirement after it, and MMHotline gets the warn, the sos for the same key, and resolved-sos; mmwatchdog status shows the settings')
        hub_checks = json.loads(mmwatchdog('status --json').stdout)['checks']
        assert hub_checks['node_reports/' + name]['alert_severity_settings'] == {'alert_severity': 'warn'}, hub_checks['node_reports/' + name]
        vm.console('systemctl stop mmwatchdog', guest=peer_guest)

        def silence_alert(kind):
            return lambda e: e['node'] == hub_name and e.get('check_group') == 'node_reports' and e.get('check') == name and e['type'] == kind

        escalated = wait(lambda: sos_only(silence_alert('failure_alert_severity_raised')), 'the count escalation reaches the sos-only destination', timeout=75)
        assert escalated[0]['alert_severity'] == 'sos' and escalated[0]['message'] == '1 of 1 nodes are not reporting to the hub at the same time, so this alert is now SOS (raise_to_sos_when_silent_nodes_reach_percent: 100)', escalated
        assert not sos_only(silence_alert('failed'))
        warned = wait(lambda: [e for e in to_capture(silence_alert('failed')) if e['incident_id'] == escalated[0]['incident_id']], 'the warn silence alert reaches capture', timeout=75)
        assert warned[0]['alert_severity'] == 'warn', warned
        loud = wait(mmhotline_alert(name + ' still not reporting to the MMWatchdog fleet hub, now SOS'), 'the escalated silence reaches MMHotline', timeout=75)
        assert loud[0]['severity'] == 'sos' and loud[0]['sender_incident_key'] == fleet_key('node_reports stopped reporting'), loud
        vm.console('systemctl start mmwatchdog', guest=peer_guest)
        again = wait(lambda: [e for e in sos_only(silence_alert('recovered')) if e['incident_id'] == escalated[0]['incident_id']], 'the recovery goes to the sos-only destination', timeout=75)
        assert again[0]['alert_severities_sent_for_incident'] == ['warn', 'sos'], again
        silence_resolved = wait(lambda: [a for a in mmhotline_received('alert-intake') if a['title'] == name + ' reports to the MMWatchdog fleet hub again' and a['severity'] == 'resolved-sos'], 'MMHotline gets resolved-sos for the silence', timeout=75)
        assert len(silence_resolved) == 1 and silence_resolved[0]['sender_incident_key'] == fleet_key('node_reports stopped reporting'), silence_resolved
        mark('a joined node going silent alerts at its own alert_severity_when_this_node_goes_silent, warn; raise_to_sos_when_silent_nodes_reach_percent raises it to sos, which a destination that takes only sos gets first, and MMHotline gets the sos and then resolved-sos')
        # A silent node raised to sos by time. The count raises nothing any
        # more, and the peer states the time after which its silence alert
        # becomes sos. The hub states one too, for the hub-outage alerter node, and
        # its own silence alert starts as a warning. The hub restarts with
        # the changed settings, rather than reloads them: the silence alerts
        # of the scenario before would hold this scenario's alerts in fleet
        # alert grouping for the rest of their minute, and a restart forgets
        # them.
        del config['fleet_hub_role']['raise_to_sos_when_silent_nodes_reach_percent']
        config['alert_severity_when_this_node_goes_silent'] = 'warn'
        config['raise_to_sos_after_this_node_is_silent_for'] = '4s'
        config['send_silence_alerts_of_this_node_only_to_destinations'] = ['capture']
        vm.write('/etc/mmwatchdog/mmwatchdog_node_settings.yml', json.dumps(config))
        restart_hub_and_wait_for_the_peer()
        peer_settings = json.loads(vm.console('cat /etc/mmwatchdog/mmwatchdog_node_settings.yml', guest=peer_guest).stdout)
        peer_settings['raise_to_sos_after_this_node_is_silent_for'] = '5s'
        vm.write('/etc/mmwatchdog/mmwatchdog_node_settings.yml', json.dumps(peer_settings), guest=peer_guest)
        peer('reload', 0)
        raise_by_time = {'alert_severity': 'warn', 'raise_to_sos_after_failing_for': '5s'}
        wait(lambda: json.loads(mmwatchdog('status --json').stdout)['checks']['node_reports/' + name]['alert_severity_settings'] == raise_by_time,
             'the peer\'s report carries its raise_to_sos_after_this_node_is_silent_for to the hub')
        assert '  Alert severity: warn\n  Raise to SOS after no report for: 5s\n' in mmwatchdog('status node_reports').stdout
        assert node(name)['report']['raise_to_sos_after_this_node_is_silent_for'] == '5s', node(name)['report']
        wait(lambda: json.loads(vm.console('cat /var/lib/mmwatchdog/fleet-settings-from-the-hub.json', guest=peer_guest).stdout).get('raise_to_sos_after_the_hub_is_silent_for') == '4s',
             'the peer holds the hub\'s raise_to_sos_after_this_node_is_silent_for')
        wait(lambda: json.loads(vm.console('cat /var/lib/mmwatchdog/fleet-settings-from-the-hub.json', guest=peer_guest).stdout).get('send_silence_alerts_of_the_hub_only_to_destinations') == ['capture'],
             'the peer holds the destinations of the hub\'s silence alerts')
        wait(lambda: name in fleet()['hub_outage_alerter_nodes'], 'the peer watches the hub')
        wait(lambda: peer_status()['checks']['node_reports/to_hub']['alert_severity_settings'] == {'alert_severity': 'warn', 'raise_to_sos_after_failing_for': '4s'},
             'the hub check of the hub-outage alerter node has the hub\'s silence severity and raise time')
        known = {e['id'] for e in delivered(lambda e: e.get('check_group') == 'node_reports')}

        def new_silence_alert(node_name, check, kind):
            return lambda: [e for e in delivered(lambda e: e['node'] == node_name and e.get('check_group') == 'node_reports'
                                                 and e.get('check') == check and e['type'] == kind) if e['id'] not in known]

        vm.console('systemctl stop mmwatchdog', guest=peer_guest)
        # The destination that takes only sos gets the raise as its first
        # message of this incident.
        raised = wait(lambda: [e for e in new_silence_alert(hub_name, name, 'failure_alert_severity_raised')() if e['authorization'] == 'Bearer sos-only-alert-token'],
                      'the hub raises the silent peer to sos by time', timeout=90)
        assert raised[0]['alert_severity'] == 'sos' and raised[0]['title'] == name + ' still not reporting to the MMWatchdog fleet hub, now SOS', raised
        assert raised[0]['message'] == name + ' has sent no report to the hub for 5s, so this alert is now SOS (raise_to_sos_after_this_node_is_silent_for: 5s)', raised
        opened = [e for e in new_silence_alert(hub_name, name, 'failed')() if e['incident_id'] == raised[0]['incident_id']]
        # The time counts from the last report, including missed intervals.
        last_report = datetime.fromisoformat(node(name)['last_report_received_at'])
        assert opened and opened[0]['alert_severity'] == 'warn' and (datetime.fromisoformat(raised[0]['at']) - last_report).total_seconds() >= 5, (opened, raised, last_report)
        assert not sos_only(lambda e: e['id'] == opened[0]['id']), 'the warning reached the destination that takes only sos'
        vm.console('systemctl start mmwatchdog', guest=peer_guest)
        again = wait(lambda: [e for e in new_silence_alert(hub_name, name, 'recovered')() if e['incident_id'] == raised[0]['incident_id']], 'the raised peer reports again', timeout=90)
        assert again[0]['alert_severity'] == 'sos' and again[0]['alert_severities_sent_for_incident'] == ['warn', 'sos'], again
        mark('a node that states raise_to_sos_after_this_node_is_silent_for is raised to sos when it has been silent that long, counted from its last report; the alert is a warning before and the recovery says both severities; mmwatchdog status on the hub shows the time')
        # A hub restart inside a node's silence. The peer's time is longer
        # now, so that the hub can stop before the raise is due. The hub is
        # down for longer than that time, and the peer is back before the hub
        # is: the restarted hub gives the peer a fresh three intervals and
        # raises nothing, and the peer's report resolves the warning.
        # The hub stops within about two seconds after the incident opened,
        # long before the raise is due eight seconds after it, and stays
        # down for ten.
        peer_settings['raise_to_sos_after_this_node_is_silent_for'] = '8s'
        vm.write('/etc/mmwatchdog/mmwatchdog_node_settings.yml', json.dumps(peer_settings), guest=peer_guest)
        peer('reload', 0)
        wait(lambda: json.loads(mmwatchdog('status --json').stdout)['checks']['node_reports/' + name]['alert_severity_settings'].get('raise_to_sos_after_failing_for') == '8s',
             'the peer\'s report carries its longer time to the hub')
        vm.console('systemctl stop mmwatchdog', guest=peer_guest)
        silent_incident = wait(lambda: json.loads(mmwatchdog('status --json').stdout)['checks']['node_reports/' + name]['failure_incident'].get('incident_id'),
                               'the hub opens the silence incident of the stopped peer')
        vm.console('systemctl stop mmwatchdog')
        time.sleep(10)
        vm.console('systemctl start mmwatchdog', guest=peer_guest)
        vm.console('systemctl start mmwatchdog')
        resolved = wait(lambda: [e for e in new_silence_alert(hub_name, name, 'recovered')() if e['incident_id'] == silent_incident],
                        'the restarted hub resolves the silence incident with the peer\'s report', timeout=90)
        assert resolved[0]['alert_severity'] == 'warn' and resolved[0]['alert_severities_sent_for_incident'] == ['warn'], resolved
        assert not [e for e in new_silence_alert(hub_name, name, 'failure_alert_severity_raised')() if e['incident_id'] == silent_incident], 'the restarted hub raised a node that was reporting again'
        mark('a hub that restarts while a node\'s silence incident is open raises nothing before the node\'s fresh three intervals are over, and the node\'s report resolves the warning')
        known = {e['id'] for e in delivered(lambda e: e.get('check_group') == 'node_reports')}
        wait(lambda: name in fleet()['hub_outage_alerter_nodes'] and peer_status()['checks']['node_reports/to_hub']['state'] == 'ok', 'the peer watches the hub again', timeout=30)
        vm.console('systemctl stop mmwatchdog')
        unreachable = wait(new_silence_alert(name, 'to_hub', 'failure_alert_severity_raised'), 'the hub-outage alerter node raises its alert about the hub to sos by time', timeout=30)
        assert unreachable[0]['alert_severity'] == 'sos' and unreachable[0]['title'] == name + ' still cannot reach the MMWatchdog fleet hub, now SOS', unreachable
        assert unreachable[0]['message'] == 'no report from this node has reached the MMWatchdog fleet hub for 4s, so this alert is now SOS (the hub\'s raise_to_sos_after_this_node_is_silent_for: 4s)', unreachable
        warned = [e for e in new_silence_alert(name, 'to_hub', 'failed')() if e['incident_id'] == unreachable[0]['incident_id']]
        last_report = datetime.fromisoformat(peer_status()['joined_fleet_hub']['last_accepted_report_at'])
        assert warned and warned[0]['alert_severity'] == 'warn' and (datetime.fromisoformat(unreachable[0]['at']) - last_report).total_seconds() >= 4, (warned, unreachable, last_report)
        vm.console('systemctl start mmwatchdog')
        wait(lambda: [e for e in new_silence_alert(name, 'to_hub', 'recovered')() if e['incident_id'] == unreachable[0]['incident_id']], 'the hub-outage alerter node reaches the hub again', timeout=30)
        hub_outage = delivered(lambda e: e.get('incident_id') == unreachable[0]['incident_id'])
        assert {e['authorization'] for e in hub_outage} == {'Bearer second-alert-token'}, hub_outage
        assert {e['type'] for e in hub_outage} >= {'failed', 'failure_alert_severity_raised', 'recovered'}, hub_outage
        assert not sos_only(lambda e: e.get('incident_id') == unreachable[0]['incident_id']), 'the hub silence alert reached an unnamed destination'
        assert all(e.get('send_alerts_only_to_destinations') == ['capture'] for e in hub_outage), hub_outage
        mark('the hub\'s own raise_to_sos_after_this_node_is_silent_for reaches the hub-outage alerter node with the fleet settings and raises the hub-outage alerter\'s alert about the hub to sos after that time; the warning, raise and recovery reach only capture, the hub\'s named silence destination')
        # One alert per exact finding across the fleet, and setting a
        # finding aside. The check of the flaw names capture, which keeps no
        # incidents, and the MMHotline destination, and no other. The hub's
        # shared tick for nodes that an announced exact finding newly
        # affects comes each minute, within the suite's waits. The peer
        # waits 30 seconds for the hub before it sends an alert itself, so
        # a slow answer of the hub cannot send the peer's alerts to the
        # destinations directly.
        flaw_key = fleet_key(EXACT_FINDING_CHECK + ': ' + FLAW + ' failed')
        exact_finding = {version: 'Debian:13 ' + FLAW + ' ' + version + '; fixed version: none' for version in ('1.0-1', '1.0-2', '1.0-3', '1.0-4')}
        set_aside_reason = 'no service on these nodes loads fleetlib'
        set_aside_entry = {'check': EXACT_FINDING_CHECK, 'finding': FLAW, 'exact_finding': exact_finding['1.0-3'], 'reason': set_aside_reason,
                           'investigation_note': 'triage_agent_notes/2026-11-24_fleetlib_on_the_fleet_suite_peer.md', 'set_aside_by': 'the fleet suite', 'set_aside_on': '2026-11-24'}
        update_title = EXACT_FINDING_CHECK + ': ' + FLAW + ': 1 node added'

        def report_exact_finding(text, guest=vm.NAME):
            """Make the module on the guest report FLAW with the exact finding
            text, or no finding for None. The new file takes the old one's
            place in one step, so no run reads it half written."""
            if text is None:
                vm.console('rm -f ' + EXACT_FINDING_FILE, guest=guest)
                return
            vm.write(EXACT_FINDING_FILE + '.new', text, guest=guest)
            vm.console('mv ' + EXACT_FINDING_FILE + '.new ' + EXACT_FINDING_FILE, guest=guest)

        def flaw_at_capture(kind=None):
            """The alerts of the flaw's key that reached capture, in order."""
            return delivered(lambda e: e.get('fleet_incident_key') == flaw_key and (kind is None or e['type'] == kind))

        def flaw_at_mmhotline():
            """The alerts of the flaw's key that reached MMHotline, in order."""
            return [a for a in mmhotline_received('alert-intake') if a.get('sender_incident_key') == flaw_key]

        def first_line(mmhotline_alert):
            return mmhotline_alert['message'].split('\n')[0]

        def flaw_state(status):
            return status['checks'][EXACT_FINDING_CHECK]['state']

        try:
            for guest in (vm.NAME, peer_guest):
                vm.write_module(EXACT_FINDING_MODULE, {'module_description.yml': vm.module_description(EXACT_FINDING_MODULE, one_incident_for_each_finding=True),
                                                       'module_executable': EXACT_FINDING_MODULE_EXECUTABLE}, guest=guest)
                vm.write(EXACT_FINDING_CHECK_GROUP_FILE, json.dumps({'check_groups': {'exact_finding': {
                    'check_every': '1s', 'time_limit_per_run': '1s', 'alert_after_failing_for': '0s', 'recovered_after_healthy_for': '0s',
                    'checks': {'flaw': {EXACT_FINDING_MODULE: {}, 'alert_severity': 'warn', 'send_alerts_only_to_destinations': ['capture', 'mmhotline_on_call']}}}}}), guest=guest)
            config['fleet_hub_role']['send_newly_affected_nodes_every'] = '1m'
            wait_for_the_hub_before_sending_directly('30s')
            peer('reload', 0)
            wait(lambda: flaw_state(json.loads(mmwatchdog('status --json').stdout)) == 'ok' and flaw_state(peer_status()) == 'ok', 'the check of the flaw is healthy on both nodes')
            # The hub reports the flaw first: its alert goes out at once.
            report_exact_finding(exact_finding['1.0-1'])
            opened = wait(lambda: flaw_at_capture('failed'), 'the hub\'s failure reaches capture at once')
            opened_seen = time.monotonic()
            assert len(opened) == 1 and opened[0]['node'] == hub_name and opened[0]['exact_finding'] == exact_finding['1.0-1'] and opened[0]['alert_severity'] == 'warn', opened
            # The peer reports the same exact finding more than a minute
            # later, past the minute of fleet alert grouping. The pause makes
            # that gap; the suite waits for nothing during it.
            time.sleep(max(0, opened_seen + 61 - time.monotonic()))
            report_exact_finding(exact_finding['1.0-1'], guest=peer_guest)
            joined = wait(lambda: flaw_at_capture('finding_nodes_changed'), 'the shared tick sends one update that adds the peer', timeout=90)
            assert len(joined) == 1 and joined[0]['message'] == update_title and joined[0]['alert_severity'] == 'warn', joined
            assert joined[0]['nodes'] == [name] and joined[0]['node_count'] == 1 and not joined[0].get('recovered_nodes'), joined
            assert joined[0]['nodes_by_exact_finding'] == [{'exact_finding': exact_finding['1.0-1'], 'nodes': [name], 'node_count': 1}], joined
            # The hub took the peer's failure before the update that names
            # the peer, so a failure alert of the peer's own would be here by now.
            assert len(flaw_at_capture('failed')) == 1, flaw_at_capture('failed')
            # An exact finding not announced before goes out at once, as the
            # peer's exact_finding_changed itself.
            report_exact_finding(exact_finding['1.0-2'], guest=peer_guest)
            changed = wait(lambda: flaw_at_capture('exact_finding_changed'), 'the peer\'s new exact finding goes out at once', timeout=30)
            assert len(changed) == 1 and changed[0]['node'] == name and changed[0]['exact_finding'] == exact_finding['1.0-2'] and changed[0]['alert_severity'] == 'warn', changed
            # Both nodes recover: one closing alert covers both.
            report_exact_finding(None)
            report_exact_finding(None, guest=peer_guest)
            both_nodes = sorted([hub_name, name])
            closed = wait(lambda: flaw_at_capture('recovered'), 'one closing alert of the key covers both nodes', timeout=30)
            assert len(closed) == 1 and closed[0]['nodes'] == both_nodes and closed[0]['node_count'] == 2 and closed[0]['alert_severity'] == 'warn', closed
            assert closed[0]['message'] == EXACT_FINDING_CHECK + ': ' + FLAW + ' is ok on every node again (was failing on ' + ', '.join(both_nodes) + ')', closed
            # The closing alert comes after every other alert of the key. An
            # update that names only nodes that are ok again goes out when the
            # shared tick falls between the two recoveries; it is info.
            recovered_only = [e for e in flaw_at_capture('finding_nodes_changed') if not e.get('node_count')]
            assert all(e['alert_severity'] == 'info' and e.get('recovered_nodes') for e in recovered_only), recovered_only
            sequence = [e['type'] for e in flaw_at_capture() if e not in recovered_only]
            assert sequence == ['failed', 'finding_nodes_changed', 'exact_finding_changed', 'recovered'], flaw_at_capture()
            mark('one exact finding on two nodes more than a minute apart sends one failure alert; the later node goes out in one finding_nodes_changed update on the shared tick of send_newly_affected_nodes_every, '
                 'a new exact finding goes out at once as exact_finding_changed, and one closing alert names both nodes')
            # The same flow at MMHotline: only fields that MMHotline knows,
            # each request with its own sender_alert_id and the fleet key, and
            # the problem in the same words on the first line of every
            # warning, with no node name.
            wait(lambda: [a for a in flaw_at_mmhotline() if a['severity'].startswith('resolved-')], 'MMHotline gets the resolution of the key', timeout=30)
            at_mmhotline = flaw_at_mmhotline()
            assert all(set(a) <= MMHOTLINE_ALERT_FIELDS for a in at_mmhotline), at_mmhotline
            assert len({a['sender_alert_id'] for a in at_mmhotline}) == len(at_mmhotline), at_mmhotline
            recovered_only = [a for a in at_mmhotline if a['severity'] == 'info']
            assert all(first_line(a) == EXACT_FINDING_CHECK + ': ' + FLAW + ': 1 node ok again' for a in recovered_only), recovered_only
            raised = [a for a in at_mmhotline if a['severity'] != 'info']
            assert [a['severity'] for a in raised] == ['warn', 'warn', 'warn', 'resolved-warn'], at_mmhotline
            assert [first_line(a) for a in raised[:3]] == [exact_finding['1.0-1'], update_title, exact_finding['1.0-2']], raised
            assert not [a for a in raised[:3] if hub_name in first_line(a) or name in first_line(a)], raised
            assert 'on 1 node: ' + name in raised[1]['message'], raised[1]
            mark('MMHotline gets the same flow as signed alerts with only the fields that it knows and sender_incident_key equal to the fleet key: the first alert, the update, '
                 'and the new exact finding as warnings whose first line names the problem without a node name, and resolved-warn when both nodes recover')
            # Set a finding aside. The key opens again with the peer's
            # failure, which goes out at once.
            at_mmhotline_before = len(flaw_at_mmhotline())
            report_exact_finding(exact_finding['1.0-3'], guest=peer_guest)
            announced = wait(lambda: [e for e in flaw_at_capture('failed') if e['exact_finding'] == exact_finding['1.0-3']], 'the peer\'s failure opens the key again at once', timeout=30)
            assert len(announced) == 1 and announced[0]['node'] == name, announced
            vm.write(SET_ASIDE_FINDINGS_FILE, json.dumps({'set_aside_findings': [set_aside_entry]}))
            mmwatchdog('reload', 0)
            put_aside = wait(lambda: flaw_at_capture('closed_without_recovery'), 'the announced finding closes as set aside', timeout=30)
            assert len(put_aside) == 1 and put_aside[0]['node'] == name and put_aside[0]['exact_finding'] == exact_finding['1.0-3'], put_aside
            assert put_aside[0]['message'] == 'Set aside after investigation: ' + set_aside_reason and put_aside[0]['alert_severity'] == 'warn', put_aside
            # The listing counts nodes from their status reports, and the
            # peer's report of the finding can follow its alert by up to
            # report_every.
            def listing_that_covers_the_peer():
                listing = json.loads(mmwatchdog('fleet set-aside-findings --json', 0).stdout)['set_aside_findings']
                return listing if any(entry['covered_node_count'] for entry in listing) else None
            listing = wait(listing_that_covers_the_peer, 'the set-aside listing covers the peer once its report arrives', timeout=30)
            assert len(listing) == 1 and {field: listing[0][field] for field in set_aside_entry} == set_aside_entry, listing
            assert listing[0]['covered_node_count'] == 1 and listing[0]['covered_nodes'] == [name], listing
            assert listing[0]['matches_no_node_now'] is False and listing[0]['nodes_that_cannot_check_now'] == 0, listing
            # The fleet view lists the finding apart, out of the peer's
            # attention; the peer itself, which does not know the verdict,
            # still shows the finding as failing.
            in_fleet = wait(lambda: (n := node(name))['set_aside_findings'] and n, 'the fleet view lists the peer\'s set-aside finding')
            assert in_fleet['set_aside_findings'] == [{'check': EXACT_FINDING_CHECK, 'finding': FLAW, 'exact_finding': exact_finding['1.0-3'], 'reason': set_aside_reason}], in_fleet['set_aside_findings']
            assert not [a for a in in_fleet['report']['summary']['attention'] if a.get('finding') == FLAW], in_fleet['report']['summary']
            assert fleet()['set_aside_finding_count'] == 1 and fleet()['nodes_with_set_aside_findings'] == 1, fleet()
            assert flaw_state(peer_status()) == 'failed', peer_status()['checks'][EXACT_FINDING_CHECK]
            mark('a set-aside entry for the exact finding of an announced finding sends one closed_without_recovery, "Set aside after investigation: REASON"; '
                 'mmwatchdog fleet set-aside-findings lists the entry with the node that it covers, and the fleet view counts the finding apart from the node\'s attention')
            # The peer's exact finding moves off the entry: one opening alert
            # names the new exact finding and the earlier verdict.
            report_exact_finding(exact_finding['1.0-4'], guest=peer_guest)
            reopened = wait(lambda: [e for e in flaw_at_capture('failed') if e['exact_finding'] == exact_finding['1.0-4']], 'the changed exact finding opens the key again', timeout=30)
            assert len(reopened) == 1 and reopened[0]['node'] == name and reopened[0]['alert_severity'] == 'warn', reopened
            assert reopened[0]['message'] == exact_finding['1.0-4'] + '; an earlier exact finding was set aside: ' + set_aside_reason, reopened
            # An empty list is valid and sets nothing aside.
            vm.write(SET_ASIDE_FINDINGS_FILE, json.dumps({'set_aside_findings': []}))
            mmwatchdog('reload', 0)
            assert json.loads(mmwatchdog('fleet set-aside-findings --json', 0).stdout)['set_aside_findings'] == []
            report_exact_finding(None, guest=peer_guest)
            wait(lambda: [e for e in flaw_at_capture('recovered') if e['node'] == name], 'the peer\'s recovery closes the key', timeout=30)
            # Neither the set aside nor the move off the entry sent an
            # exact_finding_changed of its own.
            assert len(flaw_at_capture('exact_finding_changed')) == 1, flaw_at_capture('exact_finding_changed')
            wait(lambda: len([a for a in flaw_at_mmhotline()[at_mmhotline_before:] if a['severity'].startswith('resolved-')]) == 2, 'MMHotline gets the resolution of the key', timeout=30)
            at_mmhotline = flaw_at_mmhotline()[at_mmhotline_before:]
            assert [(a['severity'], first_line(a)) for a in at_mmhotline[:3]] == [
                ('warn', exact_finding['1.0-3']), ('resolved-warn', 'Set aside after investigation: ' + set_aside_reason), ('warn', exact_finding['1.0-4'])], at_mmhotline
            mark('an exact finding that moves off a set-aside entry sends one opening alert, "EXACT_FINDING; an earlier exact finding was set aside: REASON", and MMHotline gets the set aside as resolved-warn and the new exact finding as a warning; '
                 'set_aside_findings: [] is valid and sets nothing aside')
            vm.console('rm -f ' + SET_ASIDE_FINDINGS_FILE)
            config['fleet_hub_role'].pop('send_newly_affected_nodes_every')
            wait_for_the_hub_before_sending_directly(TIMINGS['send_alert_without_hub_after_waiting_for'])
        finally:
            vm.console('rm -f ' + SET_ASIDE_FINDINGS_FILE)
            for guest in (vm.NAME, peer_guest):
                vm.console('rm -rf ' + ' '.join([EXACT_FINDING_CHECK_GROUP_FILE, EXACT_FINDING_FILE, EXACT_FINDING_FILE + '.new', vm.EXTERNAL_MODULES + '/' + EXACT_FINDING_MODULE]), guest=guest)
            config['fleet_hub_role'].pop('send_newly_affected_nodes_every', None)
            config['fleet_hub_role']['send_alert_without_hub_after_waiting_for'] = TIMINGS['send_alert_without_hub_after_waiting_for']
            vm.write('/etc/mmwatchdog/mmwatchdog_node_settings.yml', json.dumps(config))
            mmwatchdog('reload', 0)
            peer('reload', 0)
    finally:
        vm.console('systemctl stop mmwatchdog-mmhotline 2>/dev/null; rm -f ' + MMHOTLINE_FILES + '; systemctl daemon-reload; true')
        email_scenarios.stop_mail_servers((vm.NAME, peer_guest))
        official_extra_destination_module_scenarios.stop_destination_services_stand_in((vm.NAME, peer_guest))
        # Leave no fleet state behind, so a rerun joins afresh.
        reset_peer()
        vm.console('systemctl stop mmwatchdog 2>/dev/null; rm -f /var/lib/mmwatchdog/fleet.db* ' + HUB_HISTORY + '* ' + TOKEN + ' ' + SOS_TOKEN + ' ' + SEVERITY_CHECKS + ' ' + MAIL_PASSWORD_FILE + ' ' + official_extra_destination_module_scenarios.SLACK_WEBHOOK_FILE + ' ' + PUSHOVER_APPLICATION_TOKEN_FILE + ' ' + PUSHOVER_USER_OR_GROUP_KEY_FILE + '; true')
        vm.write('/etc/mmwatchdog/mmwatchdog_node_settings.yml', original)
        vm.write('/etc/mmwatchdog/alert_destinations.yml', original_alerts)
        vm.console('systemctl start mmwatchdog')
