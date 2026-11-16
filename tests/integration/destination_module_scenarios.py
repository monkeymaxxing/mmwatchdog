"""A destination whose module is not core, on a real node, with the example
module of a team's own, the Starlark script examples/modules/alarm_system,
and with a module that is an executable."""
from datetime import datetime
import json
import re
import shlex
import vm

MODULES = vm.EXTERNAL_MODULES
EXAMPLE = 'alarm_system'
# The suite's capture receiver stands for the team's alarm system: it records
# each JSON that is posted to it, with the path and the Authorization header.
ALARM_SYSTEM = 'http://127.0.0.1:18080'
TOKEN_FILE = '/etc/mmwatchdog/secrets/alarm-system-token'
# A module of this scenario only, an executable. It answers failed, and its
# message says as which account it runs, what its helper is, and whether it
# can read the file that its input names.
HOW_IT_RUNS = 'report_how_the_module_runs'
HOW_IT_RUNS_SCRIPT = '''#!/bin/bash
request=$("$MMWATCHDOG_MODULE_HELPER" module-json request --inputs api_token_file) || exit 1
path=$(printf '%s' "$request" | "$MMWATCHDOG_MODULE_HELPER" module-json get --type string /inputs/api_token_file) || exit 1
access="cannot read"
[[ -r $path ]] && access="can read"
"$MMWATCHDOG_MODULE_HELPER" module-json result --status failed --message "the module runs as $(id -un) with the helper $MMWATCHDOG_MODULE_HELPER and $access $path"
'''


def suite(cli, status, state, events, reports, wait, mark, alerts):
    """alerts is the content of alert_destinations.yml before and after."""
    node = vm.hostname()
    fragment = '/etc/mmwatchdog/check_groups/destination_module.yml'
    candidate = '/var/tmp/mmwatchdog-fixture/destination-module-config'

    def module(name, inputs, **more):
        return {'remind_every': 'never', name: inputs, 'time_limit_per_delivery_attempt': '10s', 'alert_severities': [], **more}

    def alarm_system(path, **more):
        return module(EXAMPLE, {'alarm_system_url': ALARM_SYSTEM + path, 'api_token_file': TOKEN_FILE}, **more)

    def posted(path, alert_type):
        return [r for r in reports() if r['path'] == path and r['event'].get('type') == alert_type]

    def send_notice(title, name, code=0):
        return cli('send-notice --alert-severity info --title ' + shlex.quote(title) + ' --destination ' + name, code)

    def failing(name):
        return next((f for f in status()['delivery']['failing_destinations'] if f['destination'] == name), None)

    # Destinations that take no severity, so only the check and the notices
    # below reach them: the alarm system, the same alarm system for a receiver
    # that takes only the title, an alarm system where nothing listens, and
    # the module that reports how it runs.
    destinations = dict(
        alerts['destinations'],
        alarm_system=alarm_system('/alarm-system'),
        alarm_system_title_only=alarm_system('/alarm-system-title-only', send_only_the_alert_title=True),
        alarm_system_down=module(EXAMPLE, {'alarm_system_url': 'http://127.0.0.1:1/alarm-system', 'api_token_file': TOKEN_FILE}),
        how_it_runs=module(HOW_IT_RUNS, {'api_token_file': TOKEN_FILE}),
    )
    try:
        vm.write_module(EXAMPLE, vm.module_files(vm.REPO / 'examples/modules' / EXAMPLE))
        vm.write_module(HOW_IT_RUNS, {'module_description.yml': vm.module_description(HOW_IT_RUNS, used_as='alert_destination', inputs={'api_token_file': 'string'}),
                                      'module_executable': HOW_IT_RUNS_SCRIPT})
        # Only root can read the token: MMWatchdog reads the file and gives the module its content.
        vm.write(TOKEN_FILE, 'first-alarm-system-token\n', '0600')
        vm.write('/etc/mmwatchdog/alert_destinations.yml', json.dumps({'destinations': destinations}, indent=2))
        vm.write(fragment, json.dumps({'check_groups': {'module_alert': {
            'check_every': '300ms', 'time_limit_per_run': '200ms', 'alert_after_failing_for': '0s', 'recovered_after_healthy_for': '0s',
            'shell': 'test ! -e /var/tmp/mmwatchdog-fixture/module-fail', 'alert_severity': 'warn',
            'send_alerts_only_to_destinations': ['alarm_system', 'alarm_system_title_only']}}}))
        # Runtime validation finds both modules, their account, and the token file.
        cli('validate', 0)
        cli('reload', 0)
        wait(lambda: state('module_alert/shell')['state'] == 'ok')
        vm.console('touch /var/tmp/mmwatchdog-fixture/module-fail')
        [failure] = wait(lambda: posted('/alarm-system', 'failed'))
        [short] = wait(lambda: posted('/alarm-system-title-only', 'failed'))
        event = next(e for e in events() if e.get('check_group') == 'module_alert' and e['type'] == 'failed')
        # The script got the alert and the token from MMWatchdog and posted the alarm
        # system's JSON with the primitive http_request: the title, the body,
        # and the fields by which the alarm system matches a failure and its
        # recovery.
        assert event['title'] == node + ': module_alert/shell failed' and event['body'].startswith(event['message'] + '\nfailing since '), event
        assert failure['event'] == {'title': event['title'], 'alert_severity': 'warn', 'type': 'failed', 'incident_id': event['incident_id'],
                                    'body': event['body']}, (failure, event)
        assert failure['authorization'] == 'Bearer first-alarm-system-token', failure
        # The destination that asks for the title only gives its module an alert without a body.
        assert short['event'] == {'title': event['title'], 'alert_severity': 'warn', 'type': 'failed', 'incident_id': event['incident_id']}, short
        # MMWatchdog reads the file for every delivery, so a new token needs no reload.
        vm.write(TOKEN_FILE, 'second-alarm-system-token\n', '0600')
        vm.console('rm /var/tmp/mmwatchdog-fixture/module-fail')
        [recovery] = wait(lambda: posted('/alarm-system', 'recovered'))
        assert recovery['event']['incident_id'] == event['incident_id'] and recovery['event']['title'] == node + ': module_alert/shell is ok again', recovery
        assert recovery['authorization'] == 'Bearer second-alarm-system-token', recovery
        assert 'body' not in wait(lambda: posted('/alarm-system-title-only', 'recovered'))[0]['event']
        sent = send_notice('Test of the alarm system. Nothing is wrong.', 'alarm_system')
        assert sent.stdout == 'Notice sent to the destination alarm_system:\n  alarm_system   delivered\n', sent.stdout
        # A notice has no incident and, without --body, no body.
        [notice] = posted('/alarm-system', 'notice')
        assert notice['event'] == {'title': node + ': Test of the alarm system. Nothing is wrong.', 'alert_severity': 'info', 'type': 'notice'}, notice
        mark('a destination that delivers through a module gets a check\'s failure, its recovery, and a notice: the module of a team\'s own posts the title, the body, and the incident ID to the alarm system with the token that MMWatchdog read from a file, also after the token changed, with no reload; a destination that asks for the title only gives the module an alert without a body')

        # The module runs as the account mmwatchdog, with the mmwatchdog binary as its
        # helper, and cannot read the token file itself. Its failed answer
        # is the error of the attempt.
        how = ('the module ' + HOW_IT_RUNS + ' reported: the module runs as mmwatchdog with the helper /usr/local/bin/mmwatchdog and cannot read ' + TOKEN_FILE)
        failed = send_notice('How the module runs', 'how_it_runs', code=1)
        assert failed.stdout == 'Notice sent to the destination how_it_runs:\n  how_it_runs   failed: ' + how + '\n', failed.stdout
        assert 'the notice was not delivered at the first attempt to 1 of 1 destinations. MMWatchdog keeps the notice queued and tries again' in failed.stderr, failed.stderr
        # An alarm system where nothing listens: the primitive http_request
        # says why, and the script answers failed with that reason.
        unreachable = 'the module ' + EXAMPLE + ' reported: MMWatchdog could not reach the alarm system: dial tcp 127.0.0.1:1: connect: connection refused'
        down = send_notice('To an alarm system that is down', 'alarm_system_down', code=1)
        assert down.stdout == 'Notice sent to the destination alarm_system_down:\n  alarm_system_down   failed: ' + unreachable + '\n', down.stdout
        # mmwatchdog status names each failing destination, with the number of
        # alerts that wait for it, the next attempt, and the latest error.
        entry = wait(lambda: failing('how_it_runs'))
        assert entry['undelivered_alerts'] == 1 and entry['latest_attempt_error'] == how, entry
        assert datetime.fromisoformat(entry['next_attempt_at']), entry
        assert failing('alarm_system_down')['latest_attempt_error'] == unreachable, status()['delivery']
        assert not failing('alarm_system') and not failing('capture'), status()['delivery']
        text = cli('status').stdout
        assert re.search(r'^Destination how_it_runs: 1 alert is not delivered\. MMWatchdog tries again at \d\d:\d\d UTC\. The latest attempt failed: ' + re.escape(how) + '$', text, re.M), text
        assert re.search(r'^Destination alarm_system_down: 1 alert is not delivered\. MMWatchdog tries again at \d\d:\d\d UTC\. The latest attempt failed: ' + re.escape(unreachable) + '$', text, re.M), text
        behind = send_notice('Behind the first one', 'how_it_runs', code=1)
        assert '  how_it_runs   not attempted yet: 1 earlier alert is not delivered, and the latest attempt failed: ' + how in behind.stdout, behind.stdout
        assert re.search(r'^Destination how_it_runs: 2 alerts are not delivered\. ', cli('status').stdout, re.M), cli('status').stdout
        # The configuration names both modules the same way, and MMWatchdog says
        # which kind each is.
        used = {m['name']: m for m in json.loads(cli('module-usage --json', 0).stdout)['modules']}
        assert (used[EXAMPLE]['kind'], used[HOW_IT_RUNS]['kind']) == ('project_starlark', 'project_executable'), used
        assert [at['file'] for at in used[EXAMPLE]['called_at']] == ['/etc/mmwatchdog/alert_destinations.yml'] * 3, used[EXAMPLE]
        mark('a destination module that is an executable runs as the account mmwatchdog with the mmwatchdog binary as its helper and cannot read the credential file that MMWatchdog reads for it; a module that answers failed, an executable and a script that cannot reach its receiver, fails the attempt with what the module said, in mmwatchdog send-notice and as a line of mmwatchdog status with the destination, the number of undelivered alerts, and the next attempt')

        # A module that is taken off the node after the configuration was
        # loaded: the next attempt names it.
        vm.console('rm -rf ' + MODULES + '/' + HOW_IT_RUNS)
        missing = 'the module ' + HOW_IT_RUNS + ' is not installed on this node: ' + MODULES + '/' + HOW_IT_RUNS + '/module_executable is not an executable file'
        wait(lambda: (failing('how_it_runs') or {}).get('latest_attempt_error') == missing)
        assert 'The latest attempt failed: ' + missing in cli('status').stdout
        # A credential file that is gone: the attempt names the input.
        vm.console('rm ' + TOKEN_FILE)
        unread = send_notice('Without the token file', 'alarm_system', code=1)
        assert unread.stdout == ('Notice sent to the destination alarm_system:\n  alarm_system   failed: MMWatchdog cannot read the file of the input api_token_file of the module '
                                 + EXAMPLE + ': open ' + TOKEN_FILE + ': no such file or directory\n'), unread.stdout
        # mmwatchdog validate finds both on the node before any alert needs them:
        # the module that the node no longer has, and the file on the node.
        vm.console('rm -rf ' + candidate + '; cp -a /etc/mmwatchdog ' + candidate)
        refused = cli('validate --config-dir ' + candidate, 1)
        assert ('unknown field "' + HOW_IT_RUNS + '": the field name matches neither a setting of a destination nor an installed module. MMWatchdog has no core module of that name, and no directory of that name is in '
                + vm.OFFICIAL_EXTRA_MODULES) in refused.stderr and ' or in ' + MODULES + ' (' in refused.stderr, refused.stderr
        without_the_missing_module = {name: destination for name, destination in destinations.items() if name != 'how_it_runs'}
        vm.write(candidate + '/alert_destinations.yml', json.dumps({'destinations': without_the_missing_module}))
        cli('validate --config-only --config-dir ' + candidate, 0)
        refused = cli('validate --config-dir ' + candidate, 1)
        assert 'destinations.alarm_system.alarm_system.api_token_file: open ' + TOKEN_FILE + ': no such file or directory' in refused.stderr, refused.stderr
        vm.write(TOKEN_FILE, 'second-alarm-system-token\n', '0600')
        cli('validate --config-dir ' + candidate, 0)
        # mmwatchdog validate checks a destination against the description of its
        # module: a _file input must name an absolute path for MMWatchdog to read,
        # an input that the module does not have, a required
        # input that is missing, and a module that is no destination module.
        vm.write_module('file_age', vm.module_files(vm.REPO / 'examples/modules/file_age'))
        for invalid, problem in [
            (module(EXAMPLE, {'alarm_system_url': ALARM_SYSTEM, 'api_token_file': 'alarm-system-token'}), 'destinations.alarm_system.alarm_system.api_token_file: an input whose name ends in _file names a file that MMWatchdog reads and gives to the module. Set api_token_file to an absolute file path'),
            (module(EXAMPLE, {'alarm_system_url': ALARM_SYSTEM, 'api_token_file': TOKEN_FILE, 'channel': 'ops'}),
             'destinations.alarm_system.alarm_system.channel: unknown field "channel": the inputs of the module alarm_system are alarm_system_url, api_token_file'),
            (module(EXAMPLE, {'api_token_file': TOKEN_FILE}), 'destinations.alarm_system.alarm_system.alarm_system_url: is required: '),
            (module('file_age', {'file_path': '/x'}), 'destinations.alarm_system.file_age: the module file_age is used as a check, not as an alert destination: its description says used_as: check'),
        ]:
            vm.write(candidate + '/alert_destinations.yml', json.dumps({'destinations': dict(alerts['destinations'], alarm_system=invalid)}))
            refused = cli('validate --config-dir ' + candidate, 1)
            assert problem in refused.stderr, (problem, refused.stderr)
        mark('a module that the node does not have, a credential file that cannot be read, and a destination that does not fit the description of its module are named by mmwatchdog validate before any alert needs them, and a module or a file that goes missing later is named by the failed attempt in mmwatchdog send-notice and mmwatchdog status')
    finally:
        vm.write('/etc/mmwatchdog/alert_destinations.yml', json.dumps(alerts, indent=2))
        vm.console('rm -rf ' + fragment + ' ' + candidate + ' ' + TOKEN_FILE + ' /var/tmp/mmwatchdog-fixture/module-fail ' + MODULES + '/' + EXAMPLE + ' ' + MODULES + '/' + HOW_IT_RUNS + ' ' + MODULES + '/file_age')
        cli('reload')
