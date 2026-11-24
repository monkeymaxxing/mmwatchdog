"""Verify recovery waits for systemd reload and stop jobs to finish, that MMWatchdog
leaves a stopped unit stopped and restarts a failed one, that a unit systemd
keeps restarting fails its crash-loop check, and that each failed systemd
unit is its own incident of an every_systemd_unit check."""
import json
import re
import time
import vm


def suite(mark=print):
    unit = 'mmwatchdog-lifecycle.service'
    fragment = '/etc/mmwatchdog/check_groups/lifecycle.yml'
    vm.write('/etc/systemd/system/' + unit, '''[Unit]
Description=MMWatchdog systemd transition fixture
[Service]
ExecStart=/bin/sh -c 'echo started >> /var/tmp/mmwatchdog-fixture/lifecycle-starts; exec /bin/sleep infinity'
ExecReload=/bin/sleep 4
ExecStop=/bin/sleep 4
TimeoutStopSec=10
''')
    vm.console('systemctl daemon-reload; systemctl start ' + unit)
    vm.write(fragment, '''check_groups:
  lifecycle:
    check_every: 800ms
    time_limit_per_run: 400ms
    alert_after_failing_for: 0s
    recovered_after_healthy_for: 0s
    checks:
      systemd_unit_state: {systemd_unit_state: {systemd_unit: mmwatchdog-lifecycle.service, fail_when: the_unit_is_not_active}, alert_severity: sos}
    restart_systemd_unit:
      reset_attempts_after_healthy_for: 10m
      systemd_unit: mmwatchdog-lifecycle.service
      triggered_by_checks: [systemd_unit_state]
      restart_after_failing_for: 0s
      max_attempts: 2
      min_time_between_attempts: 500ms
''')
    vm.console('/usr/local/bin/mmwatchdog reload')
    def status():
        return json.loads(vm.console('/usr/local/bin/mmwatchdog status --json', check=False).stdout)
    def wait_state(expected):
        end = time.monotonic() + 20
        while time.monotonic() < end:
            data = status()
            check = data['checks']['lifecycle/systemd_unit_state']
            if check['last_observation']['message'] == 'systemd unit ' + unit + ' is ' + expected:
                return data
            time.sleep(.2)
        raise AssertionError((expected, check))
    def starts():
        return int(vm.console('wc -l < /var/tmp/mmwatchdog-fixture/lifecycle-starts').stdout)
    def history():
        return json.loads(vm.console('/usr/local/bin/mmwatchdog events lifecycle --json').stdout)
    earlier = set()
    def events(kind):
        return [e for e in history() if e['type'] == kind and e['id'] not in earlier]
    try:
        wait_state('active')
        # An earlier run of this suite in the same guest leaves its events and
        # its used restart attempt behind; neither counts for this run.
        earlier.update(e['id'] for e in history())
        vm.console('/usr/local/bin/mmwatchdog reset-recovery-attempts lifecycle')
        started = starts()
        # A reloading unit runs, so its check is healthy. A deactivating unit
        # does not run, so its check fails, and MMWatchdog waits for systemd's stop.
        for action, state, check_status in [('reload', 'reloading', 'ok'), ('stop', 'deactivating', 'failed')]:
            vm.console('systemctl ' + action + ' --no-block ' + unit)
            observed = wait_state(state)
            assert observed['checks']['lifecycle/systemd_unit_state']['last_observation']['status'] == check_status, observed
            assert observed['systemd_unit_restart_attempts'][unit]['attempts_used'] == 0, observed
            time.sleep(1)
            assert status()['systemd_unit_restart_attempts'][unit]['attempts_used'] == 0
            if action == 'reload': wait_state('active')
        assert starts() == started
        mark('a reloading unit passes its systemd_unit_state check; a deactivating unit fails it, and MMWatchdog waits for the stop job without consuming systemd unit restart attempts')

        # The stop job finishes and systemd reports the unit as inactive: it
        # was stopped on request. MMWatchdog does not start a stopped unit. The check
        # stays failed, and MMWatchdog records once that it withheld the restart.
        stopped = wait_state('stopped. systemd reports no failure: the unit was stopped on request, or its program ended normally. MMWatchdog did not start the unit.')
        assert stopped['checks']['lifecycle/systemd_unit_state']['failure_incident'].get('incident_id'), stopped
        end = time.monotonic() + 10
        while not events('systemd_unit_restart_withheld') and time.monotonic() < end:
            time.sleep(.2)
        # Many scheduler ticks pass; MMWatchdog still starts nothing and records nothing more.
        time.sleep(2)
        withheld = events('systemd_unit_restart_withheld')
        assert [e['message'] for e in withheld] == ['no restart: systemd unit ' + unit + ' is stopped, and MMWatchdog never starts a stopped unit'], withheld
        assert not events('systemd_unit_restart_requested'), events('systemd_unit_restart_requested')
        assert status()['systemd_unit_restart_attempts'][unit]['attempts_used'] == 0
        assert starts() == started
        assert vm.console('systemctl is-active ' + unit, check=False).stdout.strip() == 'inactive'
        mark('MMWatchdog leaves a unit that was stopped on request stopped: its check stays failed, no restart attempt is used, and one event records the withheld restart')

        # A unit whose program is killed is failed, not stopped: MMWatchdog restarts it.
        vm.console('systemctl start ' + unit)
        wait_state('active')
        assert starts() == started + 1
        vm.signal_main_process(unit, 'KILL')
        end = time.monotonic() + 20
        while status()['systemd_unit_restart_attempts'][unit]['attempts_used'] != 1 and time.monotonic() < end:
            time.sleep(.2)
        restarted = wait_state('active')
        assert restarted['systemd_unit_restart_attempts'][unit]['attempts_used'] == 1, restarted
        assert starts() == started + 2
        assert [e['message'] for e in events('systemd_unit_restart_requested')] == ['restart systemd unit ' + unit + ', attempt 1']
        # Newest first: the failure of the killed unit, with what systemd reported.
        assert events('failed')[0]['message'] == 'systemd unit ' + unit + ' failed (systemd result: signal)', events('failed')
        assert len(events('systemd_unit_restart_withheld')) == 1
        mark('MMWatchdog restarts a unit that systemd reports as failed after its program was killed')
    finally:
        vm.console('rm -f ' + fragment)
        vm.console('/usr/local/bin/mmwatchdog reload')
        vm.console('systemctl stop ' + unit)

    # A crash loop looks active at almost every observation; the restarts
    # check counts systemd's automatic restarts instead.
    unit = 'mmwatchdog-crashloop.service'
    stable = '/var/tmp/mmwatchdog-fixture/crashloop-stable'
    vm.write('/etc/systemd/system/' + unit, '''[Unit]
Description=MMWatchdog crash-loop fixture
StartLimitIntervalSec=0
[Service]
ExecStart=/bin/sh -c 'test -e ''' + stable + ''' && exec /bin/sleep infinity; sleep 0.2; exit 3'
Restart=always
RestartSec=400ms
''')
    vm.console('rm -f ' + stable + '; systemctl daemon-reload; systemctl start ' + unit)
    vm.write(fragment, '''check_groups:
  crashloop:
    check_every: 300ms
    time_limit_per_run: 200ms
    alert_after_failing_for: 1s
    recovered_after_healthy_for: 0s
    checks:
      systemd_unit_crash_loop:
        systemd_unit_crash_loop: {systemd_unit: mmwatchdog-crashloop.service, automatic_restarts_above: 2, automatic_restarts_within: 4s}
        alert_severity: warn
''')
    vm.console('/usr/local/bin/mmwatchdog reload')
    def restarts():
        return status()['checks']['crashloop/systemd_unit_crash_loop']
    def wait(predicate):
        end = time.monotonic() + 20
        check = None
        while time.monotonic() < end:
            try:
                check = restarts()
                if predicate(check):
                    return check
            except (json.JSONDecodeError, KeyError):
                pass  # the daemon may be restarting
            time.sleep(.2)
        raise AssertionError(check)
    try:
        evidence = wait(lambda c: c['failure_incident'].get('evidence'))['failure_incident']['evidence']['observation']
        assert evidence['status'] == 'failed' and evidence['measurements']['automatic_restarts']['value'] > 2, evidence
        assert re.fullmatch(r'\d+ automatic restarts in the last 4s; last exit: exit-code \(status 3\)', evidence['message']), evidence
        # The saved history keeps the incident open across an MMWatchdog restart.
        incident = restarts()['failure_incident']['incident_id']
        vm.console('systemctl restart mmwatchdog')
        after = wait(lambda c: not c['needs_fresh_observation'])
        assert after['failure_incident']['incident_id'] == incident and after['last_observation']['measurements']['automatic_restarts']['value'] > 2, after
        vm.console('touch ' + stable)
        wait(lambda c: not c['failure_incident'].get('incident_id') and c['last_observation']['message'] == '0 automatic restarts in the last 4s')
        history = json.loads(vm.console('/usr/local/bin/mmwatchdog events crashloop --json').stdout)
        assert [e['type'] for e in history if e.get('incident_id') == incident] == ['recovered', 'failed'], history
        mark('a unit systemd keeps restarting fails its crash-loop check, with the count and last exit as evidence, stays failed across an MMWatchdog restart, and recovers once the window empties')
    finally:
        vm.console('rm -f ' + fragment)
        vm.console('/usr/local/bin/mmwatchdog reload')
        vm.console('systemctl stop ' + unit + '; rm -f ' + stable)

    failed_units(fragment, status, mark)


# The units of the failed-unit scenarios. Each is a one-time job that fails
# until its marker file exists, like a nightly backup that fails and is then
# repaired and run again. vm.reset clears their failed state.
FAILED_UNIT_FIXTURES = ['mmwatchdog-failed-backup.service', 'mmwatchdog-failed-certbot.service', 'mmwatchdog-failed-ignored.service']


def failed_unit_fixture(unit, guest=vm.NAME):
    """Install a one-time job that fails until its marker file exists."""
    marker = '/var/tmp/mmwatchdog-fixture/' + unit + '.repaired'
    vm.write('/etc/systemd/system/' + unit, '''[Unit]
Description=MMWatchdog failed-unit fixture
[Service]
Type=oneshot
ExecStart=/usr/bin/test -e ''' + marker + '''
''', guest=guest)
    # The second guest of the fleet suite has no fixture directory yet.
    vm.console('mkdir -p /var/tmp/mmwatchdog-fixture; rm -f ' + marker + '; systemctl daemon-reload; systemctl reset-failed ' + unit + ' 2>/dev/null; true', guest=guest)
    return marker


def already_failed_units(guest=vm.NAME):
    """The units of the guest that are failed before a scenario starts. The
    scenario's check ignores them, so only its own units count."""
    listed = vm.console('systemctl list-units --state=failed --all --plain --full --no-legend --no-pager', check=False, guest=guest).stdout
    return sorted({line.split()[0] for line in listed.splitlines() if line.split()})


def failed_units(fragment, status, mark):
    """Each failed systemd unit is its own incident, with its own alert and
    its own recovery, and a unit in systemd_units_to_ignore is left out."""
    key = 'failed_systemd_units/machine_has_no_failed_systemd_unit'
    backup, certbot, ignored = FAILED_UNIT_FIXTURES
    markers = {unit: failed_unit_fixture(unit) for unit in FAILED_UNIT_FIXTURES}
    ignore = already_failed_units() + [ignored]

    def configure(units_to_ignore):
        vm.write(fragment, json.dumps({'check_groups': {'failed_systemd_units': {
            'check_every': '1s', 'time_limit_per_run': '1s', 'alert_after_failing_for': '2s', 'recovered_after_healthy_for': '2s',
            'checks': {'machine_has_no_failed_systemd_unit': {'every_systemd_unit': {'fail_when': 'a_systemd_unit_has_failed', 'systemd_units_to_ignore': units_to_ignore}, 'alert_severity': 'warn'}}}}}, indent=2))
        vm.console('/usr/local/bin/mmwatchdog reload')

    def check():
        return status()['checks'][key]

    def wait(predicate, what):
        end = time.monotonic() + 30
        value = None
        while time.monotonic() < end:
            try:
                value = check()
                if predicate(value):
                    return value
            except (json.JSONDecodeError, KeyError):
                pass  # the daemon may be restarting
            time.sleep(.3)
        raise AssertionError((what, value))

    def incident(value, unit):
        return value.get('findings', {}).get(unit, {}).get('failure_incident', {}).get('incident_id')

    # An earlier run of this suite in the same guest leaves its events behind;
    # they do not count for this run.
    earlier = set()

    def history():
        events = json.loads(vm.console('/usr/local/bin/mmwatchdog events failed_systemd_units --json').stdout)
        return [e for e in events if e['id'] not in earlier]

    node = vm.hostname()
    try:
        earlier.update(e['id'] for e in history())
        configure(ignore)
        wait(lambda c: c['state'] == 'ok' and c['last_observation']['message'].startswith('no failed systemd units'), 'the check is healthy')
        assert check()['one_incident_for_each_finding'], check()
        # A failed unit that the check ignores does not fail it.
        vm.console('systemctl start ' + ignored, check=False)
        left_out = wait(lambda c: ignored in c['last_observation']['message'], 'the ignored unit is failed')
        assert left_out['state'] == 'ok' and left_out['last_observation']['status'] == 'ok', left_out
        assert left_out['last_observation']['message'].startswith('no failed systemd units (not counting '), left_out
        time.sleep(3)
        assert check()['state'] == 'ok' and not check().get('findings'), check()
        mark('an every_systemd_unit check is healthy while no unit is failed, and a failed unit in systemd_units_to_ignore does not fail it')

        # The backup job fails: after alert_after_failing_for it has its own incident.
        vm.console('systemctl start ' + backup, check=False)
        first = wait(lambda c: incident(c, backup), 'the failed unit has an incident')
        assert first['state'] == 'failed' and not first['failure_incident'].get('incident_id'), first
        unit = first['findings'][backup]
        assert unit['state'] == 'failed' and unit['finding'] == backup, unit
        assert unit['last_observation']['message'] == 'systemd unit ' + backup + ' failed (systemd result: exit-code)', unit
        # A second unit fails while the first incident is open: it gets its own.
        vm.console('systemctl start ' + certbot, check=False)
        both = wait(lambda c: incident(c, certbot), 'the second failed unit has its own incident')
        assert incident(both, backup) == incident(first, backup) != incident(both, certbot), both
        failures = {e['finding']: e for e in history() if e['type'] == 'failed'}
        about = key + ': ' + backup
        assert failures[backup]['incident_id'] == incident(first, backup) and failures[backup]['alert_severity'] == 'warn', failures
        assert failures[backup]['title'] == node + ': ' + about + ' failed', failures[backup]
        assert failures[backup]['body'].startswith('systemd unit ' + backup + ' failed (systemd result: exit-code)\nfailing since '), failures[backup]
        assert failures[certbot]['incident_id'] == incident(both, certbot), failures
        # Inspection shows one line for each failed unit, and each open incident counts.
        shown = vm.console('/usr/local/bin/mmwatchdog status failed_systemd_units', check=False)
        assert shown.returncode == 1, shown
        for name in [backup, certbot]:
            assert re.search(r'^  ' + re.escape(name) + r' +failed +systemd unit ' + re.escape(name) + r' failed \(systemd result: exit-code\)$', shown.stdout, re.M), shown.stdout
            assert '  Open failure incident of ' + name + ', alert severity warn:' in shown.stdout, shown.stdout
        attention = status()['summary']['attention']
        assert sorted(a.get('finding') for a in attention if a['check'] == key) == [backup, certbot], attention
        now = vm.console('/usr/local/bin/mmwatchdog check-now failed_systemd_units --json', check=False)
        observed = json.loads(now.stdout)['machine_has_no_failed_systemd_unit']
        assert now.returncode == 1 and observed['findings'] == [{'id': unit, 'message': 'systemd unit ' + unit + ' failed (systemd result: exit-code)', 'exact_finding': unit + ' exit-code'} for unit in (backup, certbot)], now.stdout
        mark('each failed systemd unit is its own incident with its own alert that names the unit, a second unit fails while the first incident is open, and mmwatchdog status shows one line for each')

        # The backup is repaired and run again: only its incident recovers.
        vm.console('touch ' + markers[backup] + '; systemctl start ' + backup)
        repaired = wait(lambda c: backup not in c.get('findings', {}), 'the repaired unit recovers')
        assert incident(repaired, certbot) == incident(both, certbot) and repaired['state'] == 'failed', repaired
        recovered = [e for e in history() if e['type'] == 'recovered']
        assert [(e['finding'], e['incident_id']) for e in recovered] == [(backup, incident(first, backup))], recovered
        assert recovered[0]['message'] == 'systemd unit ' + backup + ' is no longer failed' and recovered[0]['title'] == node + ': ' + about + ' is ok again', recovered
        # A restart of MMWatchdog keeps the other incident open.
        vm.console('systemctl restart mmwatchdog')
        after = wait(lambda c: not c['needs_fresh_observation'], 'MMWatchdog observes again after its restart')
        assert incident(after, certbot) == incident(both, certbot), after
        mark('a repaired unit sends its own recovery while the incident of another failed unit stays open, also across a restart of MMWatchdog')

        # The other unit joins systemd_units_to_ignore. The check changed, so
        # its incident closes without a recovery.
        configure(ignore + [certbot])
        wait(lambda c: c['state'] == 'ok' and not c.get('findings') and c['last_observation']['status'] == 'ok', 'the check is healthy with the unit ignored')
        closed = [e for e in history() if e.get('incident_id') == incident(both, certbot) and e['type'] != 'failed']
        assert [(e['type'], e['finding']) for e in closed] == [('closed_without_recovery', certbot)], closed
        # With the names of this scenario the title can be longer than 120
        # characters. MMWatchdog then cuts the longest name, the check's, and keeps
        # the unit and the words that say what changed.
        # How much of the check's name stays depends on the length of the
        # node's name.
        title = closed[0]['title']
        start, end = node + ': failed_systemd_units/', ': ' + certbot + ' incident closed, maybe not fixed'
        assert title.startswith(start) and title.endswith(end), closed
        shown_check = title[len(start):len(title) - len(end)]
        assert closed[0]['check'].startswith(shown_check.rstrip('…')) and len(shown_check) >= 12, closed
        assert len(title) <= 120 and closed[0].get('title_was_shortened', False) == ('…' in title), closed
        # mmwatchdog validate names a unit that has its own systemd_unit_state check
        # and is missing from the list; the configuration stays valid.
        staged = '/var/tmp/mmwatchdog-fixture/staged-configuration'
        vm.console('rm -rf ' + staged + '; cp -a /etc/mmwatchdog ' + staged)
        vm.write(staged + '/check_groups/nightly_backup_job.yml', json.dumps({'check_groups': {'nightly_backup_job': {
            'check_every': '1s', 'time_limit_per_run': '1s', 'alert_after_failing_for': '2s', 'recovered_after_healthy_for': '2s',
            'checks': {'backup_service_is_running': {'systemd_unit_state': {'systemd_unit': backup, 'fail_when': 'the_unit_is_not_active'}, 'alert_severity': 'warn'}}}}}))
        validated = vm.console('/usr/local/bin/mmwatchdog validate --config-dir ' + staged)
        assert ('  ' + key + ': ' + backup + ' is missing from systemd_units_to_ignore; nightly_backup_job/backup_service_is_running also checks the systemd unit') in validated.stdout, validated.stdout
        assert 'a unit failure would send two alerts' in validated.stdout, validated.stdout
        mark('a unit added to systemd_units_to_ignore closes its incident without a recovery, and mmwatchdog validate names a unit with its own systemd_unit_state check that is missing from the list')
    finally:
        vm.console('rm -f ' + fragment)
        vm.console('/usr/local/bin/mmwatchdog reload')
        vm.console('systemctl reset-failed ' + ' '.join(FAILED_UNIT_FIXTURES) + ' 2>/dev/null; rm -f ' + ' '.join(markers.values()) + '; true')
