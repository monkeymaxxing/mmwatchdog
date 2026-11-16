#!/usr/bin/env python3
"""Run the scenarios of a whole monitoring setup on the two guests of a job
slot: build the services that the setup watches, deploy MMWatchdog with the
setup's configuration on each guest, and cause failures for real, waiting
for MMWatchdog's alerts. The suite web_service_fleet_example of the catalog runs
the example examples/web-service-fleet this way.

    python3 tests/integration/vm.py monitoring-setup-scenarios --scenarios-file PATH
    python3 tests/integration/incus.py --distribution debian13 --monitoring-setup-scenarios-file PATH

The second command runs a setup that is not in the catalog, such as one
that is not in this repository, on new Incus guests.

The scenario file
-----------------

A JSON file, such as web_service_fleet_example/scenarios.json. Each path in
it is relative to the file's directory, and the driver copies that whole
directory to each guest as /var/tmp/mmwatchdog-monitoring-setup. Every command of
the file runs as root from there.

    {
      "guests": {
        "mmwatchdog-dev": {"mmwatchdog_configuration_directory": "../../../examples/web-service-fleet/application_server",
                   "host_names": ["application-server.web-service.internal", "app.example.com"],
                   "setup_scripts": ["setup/web_server.sh"]},
        "mmwatchdog-peer": {"mmwatchdog_configuration_directory": "../../../examples/web-service-fleet/utility_host",
                    "host_names": ["utility-host.web-service.internal"],
                    "setup_scripts": []}
      },
      "every_check_healthy_within_seconds": 60,
      "no_alert_while_healthy_for_seconds": 15,
      "allowed_alerts": [
        {"node": "mmwatchdog-peer", "type": "reminder", "reason": "why such an alert may arrive although no step waits for it"}
      ],
      "scenarios": [
        {"name": "web_server_killed",
         "steps": [
           {"guest": "mmwatchdog-dev", "run": "systemctl kill --signal=SIGKILL nginx.service"},
           {"wait_for_alerts": [
              {"node": "mmwatchdog-dev", "check_group": "web_server", "check": "web_server_is_running", "type": "failed",
               "alert_severity": "sos"}],
            "within_seconds": 15},
           {"guest": "mmwatchdog-dev", "wait_until_succeeds": "systemctl is-active --quiet nginx.service", "within_seconds": 15}]}
      ]
    }

- guests: mmwatchdog-dev, or mmwatchdog-dev and mmwatchdog-peer, the slot's two guests. mmwatchdog-dev has
  2 CPUs and 4 GiB of memory, and mmwatchdog-peer 1 CPU and 1 GiB. Each guest runs
  a standalone MMWatchdog of its own, and has:
  - mmwatchdog_configuration_directory: the directory whose files become /etc/mmwatchdog
    on the guest: mmwatchdog_node_settings.yml, alert_destinations.yml, and
    check_groups/. An optional mmwatchdog_external_modules/ in it holds modules of
    the setup's own, which the deploy installs.
  - host_names: the names that every guest's /etc/hosts gives this
    guest's address, so that the checks of the other guest reach it by
    name. An empty list adds none. vm.py up names mmwatchdog-dev
    mmwatchdog-dev.mmwatchdog-test.internal on both guests in any case.
  - setup_scripts: the scripts that install and start the services that
    the setup watches, run with sh in this order. An empty list runs none.
- every_check_healthy_within_seconds: how long every check of every guest
  may take to become healthy, with every alert delivered: before the quiet
  check and at the end of each scenario.
- no_alert_while_healthy_for_seconds: how long MMWatchdog must stay quiet in the
  quiet check.
- allowed_alerts: alerts that may arrive although no step waits for them,
  each with its reason. An entry matches as an expected alert does. An
  empty list allows none.
- scenarios: each with a unique name, which names its result file, and its
  steps.

A step has exactly one of five keys:

- {"guest": G, "run": COMMAND}: runs the command once on guest G. It must
  exit 0.
- {"guest": G, "wait_until_succeeds": COMMAND, "within_seconds": N}: runs
  the command again every half second until it exits 0, for at most N
  seconds.
- {"wait_for_alerts": [ALERT, ...], "within_seconds": N}: waits at most N
  seconds until each expected alert has arrived at a receiver of any
  guest, each one as a separate alert.
- {"guest": G, "reboot": true, "within_seconds": N}: reboots guest G and
  waits until it runs again and its MMWatchdog answers, in at most N seconds for
  both.
- {"guest": G, "wait_for_reboot": true, "within_seconds": N}: waits at most
  N seconds until guest G has rebooted on its own, such as by a recovery
  command, and runs again with its MMWatchdog answering. A reboot in an earlier
  step of the scenario counts: the driver compares with G's boot when the
  scenario started, or after its last reboot or wait_for_reboot step.

A command is shell text, such as "sh step_commands/block_traffic.sh". A
waiting step is the only way to wait; there is no sleep. While a guest
reboots, a waiting step goes on waiting, and a run step on that guest
fails. A transient unit of systemd-run needs the absolute path of a script,
below /var/tmp/mmwatchdog-monitoring-setup.

An expected alert lists fields of MMWatchdog's alert event, and an alert matches
when it has every field with that value: node (the guest whose MMWatchdog sends
the alert), check_group, check, type, and alert_severity when it matters.
type is MMWatchdog's (docs/changing-mmwatchdog/design/alerts.md, "Alert events"). An alert without a
check, such as rebooted, has no check_group and no check. MMWatchdog delivers an
alert at least once, so the driver counts each event ID once.

Every alert destination of the setup is the webhook receiver of the
acceptance suite (fixture.py) on the same guest, at
http://127.0.0.1:18080/events or a path below it, such as
/events/on_call_pager. The receiver records each alert with its path in
/var/tmp/mmwatchdog-fixture/webhooks.jsonl, so a step's command can check which
destination got an alert. Port 18080 belongs to it.

What a run does
---------------

1. It checks the scenario file, waits for the guests, writes the host
   names, and builds MMWatchdog and MMX.
2. On each guest, in the order of the file: it starts the webhook
   receiver, copies the scenario file's directory, and runs the setup
   scripts.
3. Once every guest is set up, it deploys MMWatchdog on each guest as the
   install page teaches (docs/how-to/install-on-one-node.md): the shipped MMX project
   examples/deploy, playbook hub_and_standalone, with the guest's
   mmwatchdog_configuration_directory as its files.
4. The quiet check: every check of every guest becomes healthy within
   every_check_healthy_within_seconds, and then no alert arrives and no
   check leaves the healthy state for no_alert_while_healthy_for_seconds.
5. Each scenario in turn: its steps, then every check of every guest
   healthy again with every alert delivered. An alert that no step of the
   scenario waited for fails the scenario, unless allowed_alerts allows it.
   So does a reboot of a guest that no reboot or wait_for_reboot step
   accounts for, such as a second reboot of a reboot loop. After a failed
   scenario, the next one runs only when every check is healthy again in
   time.

It prints one PASS or FAIL line for each scenario, and writes
<test environment>/<name>/results/<scenario>.json, with each step's
seconds, the alerts that arrived, and for a failure its reason and what
mmwatchdog status and mmwatchdog events showed, and summary.json. <name> is the name of the
scenario file's directory. The webhook receiver, MMWatchdog, and the copy of the
directory survive a reboot of a guest; so does a service whose setup
script enables it.
"""
import json
from pathlib import Path
import shlex
import shutil
import subprocess
import time

import deployment
import mmx_build
import vm

# Where each guest holds its copy of the scenario file's directory. Every
# command of the file runs as root from this directory.
SETUP_DIRECTORY_ON_GUEST = '/var/tmp/mmwatchdog-monitoring-setup'
# What the webhook receiver has taken, one JSON record per line.
WEBHOOKS = '/var/tmp/mmwatchdog-fixture/webhooks.jsonl'
# The end of each line that the driver writes into a guest's /etc/hosts.
HOSTS_LINE_MARKER = ' # mmwatchdog-monitoring-setup'
GUEST_KEYS = {'mmwatchdog_configuration_directory', 'host_names', 'setup_scripts'}
STEP_KINDS = ('run', 'wait_until_succeeds', 'wait_for_alerts', 'reboot', 'wait_for_reboot')
SECONDS_BETWEEN_POLLS = 0.5
# How long one poll of a guest waits for its answer: a guest that reboots
# does not answer, and the poll then counts as not done yet.
SECONDS_FOR_AN_ANSWER = 20


class ScenarioFailed(Exception):
    pass


def run(scenarios_file):
    scenarios_file = Path(scenarios_file).resolve()
    source = scenarios_file.parent
    setup = json.loads(scenarios_file.read_text())
    check_scenario_file(scenarios_file, setup)
    guests = list(setup['guests'])
    work = vm.ROOT / source.name
    results = work / 'results'
    shutil.rmtree(work, ignore_errors=True)
    results.mkdir(parents=True)

    # incus.py gives every job slot two new guests, so nothing of an
    # earlier suite is there to reset.
    for guest in guests:
        vm.up(guest)
    write_host_names(setup)
    vm.setup_step_done('the guests are ready')
    vm.build()
    mmx_build.build()
    vm.setup_step_done('MMWatchdog and MMX are built')
    for guest, settings in setup['guests'].items():
        start_webhook_receiver(guest)
        vm.console('rm -rf ' + SETUP_DIRECTORY_ON_GUEST, guest=guest)
        vm.cmd(['scp', '-r', *vm.ssh_options(guest), source, vm.ssh_target(guest) + ':' + SETUP_DIRECTORY_ON_GUEST])
        for script in settings['setup_scripts']:
            code, output = run_on_guest(guest, 'sh ' + shlex.quote(script), timeout=1800)
            assert code == 0, f'the setup script {script} on {guest} exited with {code}:\n' + output[-6000:]
            vm.setup_step_done(f'the setup script {script} ran on {guest}')
    # Every service of the setup runs before any MMWatchdog starts: a guest's MMWatchdog
    # may watch a service of the other guest.
    for guest, settings in setup['guests'].items():
        deploy_mmwatchdog(guest, source / settings['mmwatchdog_configuration_directory'], work)
        vm.setup_step_done(f'MMWatchdog is deployed on {guest} with the files of {settings["mmwatchdog_configuration_directory"]}')

    # Alerts already compared with what the file expects or allows, by
    # event ID. Every alert that arrives belongs to the step that is running.
    accounted = set()
    summary = []
    started = time.monotonic()

    runs = [('quiet_check', lambda: run_quiet_check(setup, guests, accounted))]
    runs += [(scenario['name'], lambda scenario=scenario: run_scenario(setup, scenario, guests, accounted))
             for scenario in setup['scenarios']]
    healthy = True
    for name, run_one in runs:
        if not healthy:
            summary.append({'scenario': name, 'result': 'NOT RUN', 'seconds': 0})
            continue
        outcome = run_one()
        (results / (name + '.json')).write_text(json.dumps(outcome, indent=2) + '\n')
        summary.append({'scenario': name, 'result': outcome['result'], 'seconds': outcome['seconds']})
        print(f'{outcome["result"]} {outcome["seconds"]:4.0f}s {name}', flush=True)
        if outcome['result'] == 'FAIL':
            print('   ' + outcome['failure'][:3000], flush=True)
            # The next scenario runs only from a healthy system, and the
            # alerts of this one do not count for it.
            try:
                wait_until_every_check_is_healthy(guests, setup['every_check_healthy_within_seconds'])
            except ScenarioFailed as error:
                print('   Not every check is healthy after the failure, so the next scenarios do not run: ' + str(error)[:1000], flush=True)
                healthy = False
            accounted.update(alert['id'] for alert in received_alerts(guests))
    # What MMWatchdog did besides alerting, such as each restart of a unit.
    for guest in guests:
        events, error = mmwatchdog_json(guest, 'events --json')
        (results / f'mmwatchdog-events-of-{guest}.json').write_text(json.dumps(events if error is None else error, indent=2) + '\n')
    failed = [entry for entry in summary if entry['result'] != 'PASS']
    (results / 'summary.json').write_text(json.dumps({
        'monitoring_setup': source.name, 'distribution': vm.DISTRIBUTION, 'scenarios': summary,
        'seconds_of_quiet_check_and_scenarios': round(time.monotonic() - started, 1)}, indent=2) + '\n')
    print(f'{len(summary) - len(failed)} of {len(summary)} passed in {time.monotonic() - started:.0f}s; results: {results}', flush=True)
    if failed:
        raise SystemExit(1)


def check_scenario_file(scenarios_file, setup):
    """Refuse a scenario file that the driver would misread, before any guest work."""
    source = scenarios_file.parent
    guests = list(setup['guests'])
    assert guests in ([vm.NAME], [vm.NAME, vm.PEER]), f'{scenarios_file}: guests must be {vm.NAME}, or {vm.NAME} and {vm.PEER}; it has {guests}'
    for guest, settings in setup['guests'].items():
        assert set(settings) == GUEST_KEYS, f'{scenarios_file}: the guest {guest} must have exactly {", ".join(sorted(GUEST_KEYS))}; it has {", ".join(sorted(settings))}'
        configuration = source / settings['mmwatchdog_configuration_directory']
        assert (configuration / 'mmwatchdog_node_settings.yml').is_file(), f'{scenarios_file}: {configuration} of {guest} has no mmwatchdog_node_settings.yml'
        assert isinstance(settings['host_names'], list), f'{scenarios_file}: host_names of {guest} must be a list'
        for script in settings['setup_scripts']:
            assert (source / script).is_file(), f'{scenarios_file}: the setup script {script} of {guest} is missing'
    for key in ('every_check_healthy_within_seconds', 'no_alert_while_healthy_for_seconds', 'allowed_alerts', 'scenarios'):
        assert key in setup, f'{scenarios_file}: {key} is missing'
    for allowed in setup['allowed_alerts']:
        assert allowed.get('reason'), f'{scenarios_file}: allowed_alerts {allowed} has no reason'
    names = [scenario['name'] for scenario in setup['scenarios']]
    assert len(set(names)) == len(names) and 'quiet_check' not in names, f'{scenarios_file}: scenario names must be unique and not quiet_check: {names}'
    for scenario in setup['scenarios']:
        for step in scenario['steps']:
            kinds = [kind for kind in STEP_KINDS if kind in step]
            assert len(kinds) == 1, f'scenario {scenario["name"]}: a step has exactly one of {", ".join(STEP_KINDS)}: {step}'
            if kinds[0] != 'wait_for_alerts':
                assert step.get('guest') in guests, f'scenario {scenario["name"]}: the step {step} names no guest of the setup'
            if kinds[0] != 'run':
                assert step.get('within_seconds', 0) > 0, f'scenario {scenario["name"]}: the step {step} has no within_seconds'
            if kinds[0] in ('reboot', 'wait_for_reboot'):
                assert step[kinds[0]] is True, f'scenario {scenario["name"]}: {kinds[0]} takes true: {step}'


def write_host_names(setup):
    """Give each guest's host names its address in the /etc/hosts of every guest."""
    lines = [vm.remote_node(guest)['address'] + ' ' + ' '.join(settings['host_names']) + HOSTS_LINE_MARKER
             for guest, settings in setup['guests'].items() if settings['host_names']]
    for guest in setup['guests']:
        vm.console(f"sed -i '/{HOSTS_LINE_MARKER}$/d' /etc/hosts; "
                   + ''.join(f'echo {shlex.quote(line)} >> /etc/hosts; ' for line in lines), guest=guest)


def start_webhook_receiver(guest):
    """The acceptance suite's webhook receiver, which records every alert in
    WEBHOOKS. It is enabled, so that it runs again after a reboot."""
    vm.write('/var/tmp/mmwatchdog-fixture/server.py', (vm.REPO / 'tests/integration/fixture.py').read_text(), guest=guest)
    vm.write('/etc/systemd/system/mmwatchdog-fixture.service', deployment.FIXTURE_UNIT, guest=guest)
    vm.console('systemctl daemon-reload; systemctl enable --now mmwatchdog-fixture 2>/dev/null', guest=guest)


def deploy_mmwatchdog(guest, configuration, work):
    """Deploy MMWatchdog on the guest as docs/how-to/install-on-one-node.md teaches from a source
    checkout: the shipped project examples/deploy with the Linux program,
    the unit, and the official extra modules beside it, and the guest's
    configuration directory as files/hub_and_standalone, which becomes
    /etc/mmwatchdog."""
    deploy_project = work / ('deploy-' + guest)
    shutil.copytree(vm.REPO / 'examples/deploy', deploy_project)
    shutil.copytree(vm.REPO / 'official_extra_modules', deploy_project / 'official_extra_modules')
    shutil.copy(vm.ROOT / ('bin/mmwatchdog-linux-' + vm.ARCH), deploy_project / 'mmwatchdog')
    shutil.copy(vm.REPO / 'examples/mmwatchdog.service', deploy_project / 'mmwatchdog.service')
    files = deploy_project / 'files/hub_and_standalone'
    shutil.rmtree(files)
    shutil.copytree(configuration, files, ignore=shutil.ignore_patterns('mmwatchdog_external_modules'))
    own_modules = configuration / 'mmwatchdog_external_modules'
    if own_modules.is_dir():
        shutil.rmtree(deploy_project / 'mmwatchdog_external_modules')
        shutil.copytree(own_modules, deploy_project / 'mmwatchdog_external_modules')
    (deploy_project / 'inventories/hub_and_standalone.yml').write_text(json.dumps({'vars': {'mmwatchdog_secret_files': [], 'mmwatchdog_config_source_dir': 'files/hub_and_standalone'}, 'nodes': {guest: vm.connection(guest)}}))
    for command in ('validate', 'run'):
        result = vm.cmd([vm.ROOT / 'mmx-bin/mmx', command, 'hub_and_standalone', '--inventory', 'hub_and_standalone', '--json'],
                        cwd=deploy_project, check=False, timeout=600,
                        extra_env={'MMX_RUNNER_DIR': str(vm.ROOT / 'mmx-bin'), 'MMX_STATE_DIR': str(work / 'mmx-state')})
        (work / f'deploy-{guest}-mmx-{command}.json').write_text(result.stdout)
        assert result.returncode == 0, (f'mmx {command} hub_and_standalone for {guest} exited with {result.returncode}:\n'
                                        + result.stdout[-6000:] + result.stderr[-3000:])


def on_guest(guest, command, timeout=SECONDS_FOR_AN_ANSWER):
    """(the result of a command on the guest, None) or (None, why there is
    none) when the guest does not answer, such as while it reboots. SSH
    exits with 255 when it cannot reach the guest."""
    try:
        result = vm.console(command, check=False, timeout=timeout, guest=guest)
    except subprocess.TimeoutExpired:
        return None, f'{guest} did not answer within {timeout} s'
    if result.returncode == 255:
        return None, f'{guest} does not answer: {result.stderr.strip()[-300:]}'
    return result, None


def run_on_guest(guest, command, timeout=300):
    """(exit code, output) of a command of the scenario file, run as root on
    the guest from its copy of the file's directory; exit code None when
    the guest does not answer."""
    result, error = on_guest(guest, 'cd ' + SETUP_DIRECTORY_ON_GUEST + ' || exit 1\n' + command, timeout)
    return (None, error) if result is None else (result.returncode, result.stdout + result.stderr)


def boot_id(guest):
    """The guest's boot ID, which a reboot changes, or None while it does not answer."""
    result, _ = on_guest(guest, 'cat /proc/sys/kernel/random/boot_id')
    return result.stdout.strip() if result and result.returncode == 0 else None


def received_alerts(guests):
    """Every alert that a receiver of the guests accepted, once by its event
    ID: MMWatchdog delivers at least once. A line that the receiver is still
    writing has no newline yet and waits for the next read."""
    alerts = {}
    for guest in guests:
        result, _ = on_guest(guest, 'cat ' + WEBHOOKS + ' 2>/dev/null; true')
        if result is None:
            continue
        for line in result.stdout.split('\n')[:-1]:
            entry = json.loads(line)
            if entry['accepted']:
                alerts.setdefault(entry['event']['id'], entry['event'])
    return list(alerts.values())


def matches(alert, expected):
    """Whether the alert has every field that the expected alert states."""
    return all(alert.get(field) == value for field, value in expected.items() if field != 'reason')


def short(alert):
    """The fields of an alert that a reader compares with the scenario file."""
    return {field: alert.get(field) for field in ('node', 'check_group', 'check', 'type', 'alert_severity', 'message') if alert.get(field) is not None}


def wait(what, poll, within_seconds):
    """Poll until poll() says done, for at most within_seconds. poll returns
    (done, description of the current state); the failure names both."""
    deadline = time.monotonic() + within_seconds
    while True:
        done, state = poll()
        if done:
            return state
        if time.monotonic() > deadline:
            raise ScenarioFailed(f'{what}: not within {round(within_seconds, 1)} s. Last state: {state}')
        time.sleep(SECONDS_BETWEEN_POLLS)


def mmwatchdog_json(guest, command):
    """The JSON that an mmwatchdog command prints on the guest, or why there is none,
    such as while a scenario restarts MMWatchdog or reboots its guest."""
    result, error = on_guest(guest, '/usr/local/bin/mmwatchdog ' + command)
    if result is None:
        return None, error
    try:
        return json.loads(result.stdout), None
    except ValueError:
        return None, f'mmwatchdog {command} exited with {result.returncode}: {result.stderr.strip()[-500:]}'


def health(guests):
    """(every check of every guest is ok and every alert is delivered, what is not)."""
    not_yet = {}
    for guest in guests:
        status, error = mmwatchdog_json(guest, 'status --json')
        if error:
            not_yet[guest] = error
            continue
        states = {name: check['state'] for name, check in status['checks'].items() if check['state'] != 'ok'}
        queued = status['delivery']['queued_notifications']
        if states or queued:
            not_yet[guest] = {'checks_not_ok': states, 'queued_notifications': queued}
    return not not_yet, not_yet


def wait_until_every_check_is_healthy(guests, within_seconds):
    wait('every check healthy and every alert delivered', lambda: health(guests), within_seconds)


def unexpected_alerts(setup, guests, accounted):
    """The alerts that arrived since the last accounting and that the
    scenario file does not allow; every one that arrived is accounted now."""
    new = [alert for alert in received_alerts(guests) if alert['id'] not in accounted]
    accounted.update(alert['id'] for alert in new)
    return [alert for alert in new if not any(matches(alert, allowed) for allowed in setup['allowed_alerts'])]


def mmwatchdog_state(guests):
    """What each guest's MMWatchdog shows, for the result of a failed scenario."""
    return {guest: {'status': mmwatchdog_json(guest, 'status --json'), 'events': mmwatchdog_json(guest, 'events --json')} for guest in guests}


def boots_of(guests):
    """Each guest's boot ID now."""
    boots = {guest: boot_id(guest) for guest in guests}
    for guest, boot in boots.items():
        if boot is None:
            raise ScenarioFailed(f'{guest} does not answer')
    return boots


def fail_on_unexpected_reboots(guests, boots):
    """Fail when a guest has booted since the boot that the scenario knows
    of: no step rebooted it or waited for its reboot. A reboot loop fails here."""
    for guest in guests:
        now = boot_id(guest)
        if now != boots[guest]:
            raise ScenarioFailed(f'{guest} rebooted, and no step rebooted it or waited for that reboot: '
                                 f'the boot ID was {boots[guest]} and is {now}')


def wait_until_mmwatchdog_answers(guest, deadline, what):
    """Wait until MMWatchdog on the guest answers mmwatchdog status, by the monotonic deadline of a step."""
    def answers():
        _, error = mmwatchdog_json(guest, 'status --json')
        return error is None, error
    if time.monotonic() > deadline:
        raise ScenarioFailed(f'{what}: {guest} took longer than the step allows to start again')
    wait(f'{what}: MMWatchdog on {guest} to answer', answers, deadline - time.monotonic())


def run_quiet_check(setup, guests, accounted):
    """Every check healthy, then no alert while every check stays healthy."""
    started = time.monotonic()
    outcome = {'result': 'PASS', 'steps': []}
    try:
        boots = boots_of(guests)
        wait_until_every_check_is_healthy(guests, setup['every_check_healthy_within_seconds'])
        outcome['steps'].append({'step': 'every check healthy', 'seconds': round(time.monotonic() - started, 1)})
        quiet_until = time.monotonic() + setup['no_alert_while_healthy_for_seconds']
        while True:
            unexpected = unexpected_alerts(setup, guests, accounted)
            if unexpected:
                raise ScenarioFailed('alerts while every check was healthy: ' + json.dumps([short(a) for a in unexpected]))
            done, not_yet = health(guests)
            if not done:
                raise ScenarioFailed('a check stopped being healthy in the quiet time: ' + json.dumps(not_yet))
            if time.monotonic() > quiet_until:
                break
            time.sleep(SECONDS_BETWEEN_POLLS)
        fail_on_unexpected_reboots(guests, boots)
        outcome['steps'].append({'step': f'no alert while healthy for {setup["no_alert_while_healthy_for_seconds"]} s',
                                 'seconds': round(time.monotonic() - started, 1)})
    except ScenarioFailed as error:
        outcome.update(result='FAIL', failure=str(error), mmwatchdog_state=mmwatchdog_state(guests))
    outcome['seconds'] = round(time.monotonic() - started, 1)
    return outcome


def run_scenario(setup, scenario, guests, accounted):
    """Each step in turn; then every check healthy and every alert
    delivered, and no alert that no step expected or the file allows."""
    started = time.monotonic()
    outcome = {'result': 'PASS', 'steps': []}
    known_before = set(accounted)
    try:
        # The boot of each guest that the scenario knows of: a reboot or a
        # waited-for reboot of a step changes it, and any other reboot fails
        # the scenario.
        boots = boots_of(guests)
        for number, step in enumerate(scenario['steps'], 1):
            step_started = time.monotonic()
            if 'run' in step:
                code, output = run_on_guest(step['guest'], step['run'])
                if code != 0:
                    raise ScenarioFailed(f'step {number}: {step["run"]!r} on {step["guest"]}: '
                                         + (output if code is None else f'exit {code}: {output[-2000:]}'))
            elif 'wait_until_succeeds' in step:
                def succeeds():
                    code, output = run_on_guest(step['guest'], step['wait_until_succeeds'], timeout=step['within_seconds'])
                    return code == 0, (output if code is None else f'exit {code}: {output[-1000:]}')
                wait(f'step {number}: {step["wait_until_succeeds"]!r} on {step["guest"]} to succeed', succeeds, step['within_seconds'])
            elif 'wait_for_alerts' in step:
                wait(f'step {number}: the expected alerts', lambda: expected_alerts_arrived(step['wait_for_alerts'], guests, accounted),
                     step['within_seconds'])
            elif 'reboot' in step:
                try:
                    vm.reboot(step['guest'])
                except (RuntimeError, subprocess.TimeoutExpired) as error:
                    raise ScenarioFailed(f'step {number}: the reboot of {step["guest"]}: {error}')
                boots[step['guest']] = boot_id(step['guest'])
                wait_until_mmwatchdog_answers(step['guest'], step_started + step['within_seconds'], f'step {number}')
            else:
                def rebooted():
                    now = boot_id(step['guest'])
                    if now is None:
                        return False, f'{step["guest"]} does not answer'
                    return (True, now) if now != boots[step['guest']] else (False, f'{step["guest"]} has not rebooted: boot ID {now}')
                boots[step['guest']] = wait(f'step {number}: {step["guest"]} to reboot', rebooted, step['within_seconds'])
                # As vm.reboot does after the reboot that it makes.
                vm.console('systemctl is-system-running --wait', check=False, timeout=300, guest=step['guest'])
                vm.set_clock_from_this_machine(step['guest'])
                wait_until_mmwatchdog_answers(step['guest'], step_started + step['within_seconds'], f'step {number}')
            seconds = round(time.monotonic() - step_started, 1)
            outcome['steps'].append({'step': step, 'seconds': seconds})
            print(f'   {seconds:5.1f}s step {number} of {scenario["name"]}', flush=True)
        wait_until_every_check_is_healthy(guests, setup['every_check_healthy_within_seconds'])
        fail_on_unexpected_reboots(guests, boots)
        unexpected = unexpected_alerts(setup, guests, accounted)
        if unexpected:
            raise ScenarioFailed('alerts that no step expected: ' + json.dumps([short(a) for a in unexpected]))
    except ScenarioFailed as error:
        outcome.update(result='FAIL', failure=str(error), mmwatchdog_state=mmwatchdog_state(guests))
    outcome['alerts'] = [alert for alert in received_alerts(guests) if alert['id'] not in known_before]
    outcome['seconds'] = round(time.monotonic() - started, 1)
    return outcome


def expected_alerts_arrived(expected, guests, accounted):
    """(each expected alert has a new alert of its own, what is still
    missing). The most specific expectation takes its alert first. The
    matched alerts are accounted once all have arrived."""
    new = [alert for alert in received_alerts(guests) if alert['id'] not in accounted]
    taken, missing = set(), []
    for wanted in sorted(expected, key=len, reverse=True):
        alert = next((a for a in new if a['id'] not in taken and matches(a, wanted)), None)
        if alert is None:
            missing.append(wanted)
        else:
            taken.add(alert['id'])
    if missing:
        return False, {'missing': missing, 'new_alerts': [short(a) for a in new]}
    accounted.update(taken)
    return True, None


if __name__ == '__main__':
    raise SystemExit('run monitoring_setup_scenarios.py through vm.py monitoring-setup-scenarios --scenarios-file PATH, '
                     'which a job slot of incus.py or hetzner.py starts')
