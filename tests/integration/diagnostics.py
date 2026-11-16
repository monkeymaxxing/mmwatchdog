"""Failure evidence and destructive fault exercises, confined to the MMWatchdog guest."""
import json
import shlex
import time
import vm


def suite(cli, status, state, events, reports, wait, mark, config):
    # A fragment supplies the source identity throughout validation and inspection.
    fragment = '/etc/mmwatchdog/check_groups/diagnostics.yml'
    vm.write(fragment, '''check_groups:
  diagnostic:
    check_every: 800ms
    time_limit_per_run: 400ms
    alert_after_failing_for: 0s
    recovered_after_healthy_for: 0s
    shell: |
      if [ -f /var/tmp/mmwatchdog-fixture/diagnostic-fail ]; then
        printf 'queue backlog 780\\n'
        printf 'upstream unavailable\\n' >&2
        exit 7
      fi
    alert_severity: warn
''')
    cli('reload', 0)
    wait(lambda: state('diagnostic/shell')['last_observation']['status'] == 'ok')
    vm.console('touch /var/tmp/mmwatchdog-fixture/diagnostic-fail')
    failed = wait(lambda: state('diagnostic/shell')['failure_incident'].get('evidence'))
    incident = failed['incident_id']
    assert failed['observation']['exit_code'] == 7
    assert failed['source']['file'] == fragment and failed['source']['line'] == 7
    assert 'upstream unavailable' in cli('status diagnostic', 1).stdout
    wait(lambda: any(r['accepted'] and r['event'].get('incident_id') == incident for r in reports()))
    vm.console('rm /var/tmp/mmwatchdog-fixture/diagnostic-fail')
    wait(lambda: not state('diagnostic/shell')['failure_incident'].get('incident_id'))
    # SIGKILL exercises WAL recovery and systemd restart, without a daemon flush.
    vm.signal_main_process('mmwatchdog.service', 'KILL')
    wait(lambda: cli('status diagnostic --json').returncode == 0)
    retained = json.loads(cli('events diagnostic --json', 0).stdout)
    original = next(e for e in retained if e['type'] == 'failed' and e['incident_id'] == incident)
    assert original['evidence'] == failed
    assert 'queue backlog 780' in cli('events diagnostic', 0).stdout
    delivered = next(r['event'] for r in reports() if r['accepted'] and r['event'].get('incident_id') == incident)
    assert delivered['evidence'] == failed
    mark('check-group file source and failure evidence survive recovery and SIGKILL; CLI and webhook retain matching diagnostics')
    vm.write(fragment, '''check_groups:
  diagnostic:
    check_every: 800ms
    time_limit_per_run: 400ms
    alert_after_failing_for: 0s
    recovered_after_healthy_for: 0s
    http:
      url: invalid
''')
    rejected = cli('reload', 4)
    assert fragment + ':8:' in rejected.stderr and 'check_groups.diagnostic.http.url' in rejected.stderr, rejected.stderr
    assert state('diagnostic/shell')['source']['file'] == fragment
    vm.console('rm ' + shlex.quote(fragment))
    cli('reload', 0)
    mark('semantic check-group file error points to the exact field and rejected reload preserves active inspection')

    # A recovery command that fixes its check runs as the mmwatchdog account once per
    # confirmed failure, within its allowed recovery attempts, and maintenance holds it.
    cache = '/var/tmp/mmwatchdog-fixture/cache'
    vm.console('install -d -o mmwatchdog -g mmwatchdog ' + cache + '; rm -f ' + cache + '/*')
    vm.write(fragment, '''check_groups:
  cache:
    check_every: 300ms
    time_limit_per_run: 200ms
    alert_after_failing_for: 0s
    recovered_after_healthy_for: 0s
    shell: test ! -e %(cache)s/full
    alert_severity: warn
    recovery_command:
      reset_attempts_after_healthy_for: 10m
      time_limit_per_attempt: 1m
      triggered_by_checks: [shell]
      script: |
        echo "$MMWATCHDOG_CHECK_GROUP $MMWATCHDOG_CHECK $MMWATCHDOG_INCIDENT_ID $(id -un)" >> %(cache)s/runs
        rm %(cache)s/full
        echo cleared
      run_after_failing_for: 0s
      max_attempts: 2
      min_time_between_attempts: 300ms
''' % {'cache': cache})
    cli('reload', 0)
    wait(lambda: state('cache/shell')['state'] == 'ok')

    def runs():
        return vm.console('cat ' + cache + '/runs 2>/dev/null', check=False).stdout.splitlines()

    def fill(expected_runs):
        vm.console('touch ' + cache + '/full')
        wait(lambda: len(runs()) == expected_runs and state('cache/shell')['state'] == 'ok')

    fill(1)
    incident = next(e['incident_id'] for e in events() if e.get('check_group') == 'cache' and e['type'] == 'failed')
    assert runs() == ['cache shell ' + incident + ' mmwatchdog'], runs()
    result = next(e for e in json.loads(cli('events cache --json', 0).stdout) if e['type'] == 'recovery_command_result')
    assert result['incident_id'] == incident and result['evidence']['observation']['exit_code'] == 0, result
    assert result['evidence']['observation']['stdout'] == 'cleared\n', result
    readable = cli('events cache', 0).stdout
    assert 'recovery command finished with exit code 0' in readable and 'cleared' in readable, readable
    assert status()['recovery_command_attempts']['cache']['last_result'] == 'exit 0'
    fill(2)
    vm.console('touch ' + cache + '/full')
    wait(lambda: state('cache/shell')['failure_incident'].get('incident_id'))
    time.sleep(1.5)
    assert len(runs()) == 2 and status()['recovery_command_attempts']['cache']['attempts_used'] == 2, runs()
    cli('reset-recovery-attempts cache', 0)
    wait(lambda: len(runs()) == 3 and state('cache/shell')['state'] == 'ok')
    cli('maintenance on cache --for 30s', 0)
    vm.console('touch ' + cache + '/full')
    wait(lambda: state('cache/shell')['failure_incident'].get('incident_id'))
    time.sleep(1.5)
    assert len(runs()) == 3, runs()
    cli('maintenance off cache', 0)
    wait(lambda: len(runs()) == 4 and state('cache/shell')['state'] == 'ok')
    assert not [r for r in reports() if r['event']['type'] in ('recovery_command_started', 'recovery_command_result')]
    vm.console('rm ' + shlex.quote(fragment))
    cli('reload', 0)
    mark('recovery command runs as mmwatchdog once per confirmed failure within its allowed recovery attempts, shows in events, stays local, and waits for maintenance and reset')

    # A recovery command waits for its own run_after_failing_for, not for the
    # alert. Here it runs at once and fixes its check long before the 30s of
    # alert_after_failing_for, so no incident opens and no alert goes out.
    vm.console('rm -f ' + cache + '/*')
    vm.write(fragment, '''check_groups:
  cache_before_alert:
    check_every: 300ms
    time_limit_per_run: 200ms
    alert_after_failing_for: 30s
    recovered_after_healthy_for: 0s
    shell: test ! -e %(cache)s/full
    alert_severity: warn
    recovery_command:
      reset_attempts_after_healthy_for: 10m
      time_limit_per_attempt: 1m
      triggered_by_checks: [shell]
      script: |
        echo "incident=$MMWATCHDOG_INCIDENT_ID" >> %(cache)s/runs
        rm %(cache)s/full
      run_after_failing_for: 0s
      max_attempts: 2
      min_time_between_attempts: 300ms
''' % {'cache': cache})
    cli('reload', 0)
    wait(lambda: state('cache_before_alert/shell')['state'] == 'ok')
    vm.console('touch ' + cache + '/full')
    wait(lambda: len(runs()) == 1 and state('cache_before_alert/shell')['state'] == 'ok')
    # No incident was open when the command ran, so MMWATCHDOG_INCIDENT_ID was empty.
    assert runs() == ['incident='], runs()
    before_alert = json.loads(cli('events cache_before_alert --json', 0).stdout)
    assert 'incident_id' not in next(e for e in before_alert if e['type'] == 'recovery_command_started'), before_alert
    assert not [e for e in before_alert if e['type'] == 'failed'], before_alert
    assert not state('cache_before_alert/shell')['failure_incident'].get('incident_id')
    assert status()['recovery_command_attempts']['cache_before_alert']['attempts_used'] == 1
    assert not [r for r in reports() if r['event'].get('check_group') == 'cache_before_alert'], 'an alert went out for a failure that the recovery command fixed'
    vm.console('rm ' + shlex.quote(fragment))
    cli('reload', 0)
    mark('a recovery command with a run_after_failing_for shorter than alert_after_failing_for runs before the alert, without an incident ID, and a failure it fixes sends no alert')

    # A recovery command with the same waiting time as the alert runs with
    # the run of its check that opens the incident. The runs of another check
    # group, every 100ms, land between the runs of the check every 300ms, and
    # decide nothing about it. So the command gets the incident's ID, and
    # the failure alert goes out although the command fixes the check.
    vm.console('rm -f ' + cache + '/*')
    vm.write(fragment, '''check_groups:
  cache_equal_waits:
    check_every: 300ms
    time_limit_per_run: 200ms
    alert_after_failing_for: 1s
    recovered_after_healthy_for: 0s
    shell: test ! -e %(cache)s/full
    alert_severity: warn
    recovery_command:
      reset_attempts_after_healthy_for: 10m
      time_limit_per_attempt: 1m
      triggered_by_checks: [shell]
      script: |
        echo "incident=$MMWATCHDOG_INCIDENT_ID" >> %(cache)s/runs
        rm %(cache)s/full
      run_after_failing_for: 1s
      max_attempts: 2
      min_time_between_attempts: 300ms
  runs_in_between:
    check_every: 100ms
    time_limit_per_run: 100ms
    alert_after_failing_for: 1m
    recovered_after_healthy_for: 0s
    shell: "true"
    alert_severity: info
''' % {'cache': cache})
    cli('reload', 0)
    wait(lambda: state('cache_equal_waits/shell')['state'] == 'ok')
    vm.console('touch ' + cache + '/full')
    wait(lambda: len(runs()) == 1 and state('cache_equal_waits/shell')['state'] == 'ok')
    equal_waits = json.loads(cli('events cache_equal_waits --json', 0).stdout)
    failed = [e for e in equal_waits if e['type'] == 'failed']
    assert len(failed) == 1, equal_waits
    assert runs() == ['incident=' + failed[0]['incident_id']], (runs(), equal_waits)
    wait(lambda: any(r['accepted'] and r['event']['type'] == 'failed' and r['event'].get('incident_id') == failed[0]['incident_id'] for r in reports()))
    vm.console('rm ' + shlex.quote(fragment))
    cli('reload', 0)
    mark('a recovery command with the same run_after_failing_for as alert_after_failing_for runs with the run that opens the incident, while another check group runs in between, and the alert goes out')

    # Keep a real start limit engaged; neither MMWatchdog's systemd unit restart attempts nor reset clears it.
    vm.write('/etc/systemd/system/mmwatchdog-limited.service', '''[Unit]
Description=MMWatchdog start-limit fixture
StartLimitIntervalSec=60
StartLimitBurst=1
[Service]
Type=oneshot
ExecStart=/bin/sh -c 'echo started >> /var/tmp/mmwatchdog-fixture/limited-starts; exit 1'
''')
    vm.console('systemctl daemon-reload; systemctl reset-failed mmwatchdog-limited.service; systemctl start mmwatchdog-limited.service || true')
    vm.write(fragment, '''check_groups:
  limited:
    check_every: 300ms
    time_limit_per_run: 150ms
    alert_after_failing_for: 0s
    recovered_after_healthy_for: 500ms
    checks:
      systemd_unit_state: {systemd_unit_state: {systemd_unit: mmwatchdog-limited.service, fail_when: the_unit_is_not_active}, alert_severity: sos}
    restart_systemd_unit:
      reset_attempts_after_healthy_for: 10m
      systemd_unit: mmwatchdog-limited.service
      triggered_by_checks: [systemd_unit_state]
      restart_after_failing_for: 0s
      max_attempts: 2
      min_time_between_attempts: 300ms
''')
    cli('reload', 0)
    wait(lambda: status()['systemd_unit_restart_attempts'].get('mmwatchdog-limited.service', {}).get('attempts_used') == 2)
    assert vm.console('wc -l < /var/tmp/mmwatchdog-fixture/limited-starts').stdout.strip() == '1'
    assert 'Start request repeated too quickly' in vm.console('journalctl -u mmwatchdog-limited.service -n 20 --no-pager').stdout
    limited = state('limited/systemd_unit_state')
    assert limited['failure_incident'].get('incident_id')
    # systemd reports the unit as failed, which MMWatchdog restarts; it leaves a stopped unit alone.
    assert limited['last_observation']['message'].startswith('systemd unit mmwatchdog-limited.service failed (systemd result: '), limited
    cli('reset-recovery-attempts limited', 0)
    wait(lambda: status()['systemd_unit_restart_attempts']['mmwatchdog-limited.service']['attempts_used'] == 2)
    assert vm.console('wc -l < /var/tmp/mmwatchdog-fixture/limited-starts').stdout.strip() == '1'
    vm.console('rm ' + shlex.quote(fragment))
    cli('reload', 0)
    vm.console('systemctl stop mmwatchdog-limited.service; systemctl reset-failed mmwatchdog-limited.service')
    mark('systemd start-limit remains authoritative across MMWatchdog restart attempts and explicit recovery-attempt reset')

    # An activating unit is allowed to finish; MMWatchdog must not restart it mid-start.
    vm.write('/etc/systemd/system/mmwatchdog-starting.service', '''[Unit]
Description=MMWatchdog slow-start fixture
[Service]
Type=oneshot
ExecStart=/bin/sh -c 'echo started >> /var/tmp/mmwatchdog-fixture/starting-starts; sleep 5'
RemainAfterExit=yes
''')
    vm.console('systemctl daemon-reload; systemctl start --no-block mmwatchdog-starting.service')
    vm.write(fragment, '''check_groups:
  starting:
    check_every: 300ms
    time_limit_per_run: 150ms
    alert_after_failing_for: 0s
    recovered_after_healthy_for: 0s
    checks:
      systemd_unit_state: {systemd_unit_state: {systemd_unit: mmwatchdog-starting.service, fail_when: the_unit_is_not_active}, alert_severity: sos}
    restart_systemd_unit:
      reset_attempts_after_healthy_for: 10m
      systemd_unit: mmwatchdog-starting.service
      triggered_by_checks: [systemd_unit_state]
      restart_after_failing_for: 0s
      max_attempts: 2
      min_time_between_attempts: 300ms
''')
    cli('reload', 0)
    wait(lambda: state('starting/systemd_unit_state')['last_observation']['message'] == 'systemd unit mmwatchdog-starting.service is activating')
    assert status()['systemd_unit_restart_attempts']['mmwatchdog-starting.service']['attempts_used'] == 0
    wait(lambda: state('starting/systemd_unit_state')['state'] == 'ok')
    assert vm.console('wc -l < /var/tmp/mmwatchdog-fixture/starting-starts').stdout.strip() == '1'
    vm.console('rm ' + shlex.quote(fragment))
    cli('reload', 0)
    vm.console('systemctl stop mmwatchdog-starting.service')
    mark('systemd activating state defers recovery and resolves only after a fresh active observation')

    disk_full(wait, mark)


def disk_full(wait, mark):
    # A tiny tmpfs creates actual ENOSPC without filling the guest or host disk.
    root = '/var/tmp/mmwatchdog-fixture/full'
    socket = '/run/mmwatchdog-storage.sock'
    vm.console('mkdir -p ' + root + '; mount -t tmpfs -o size=8m tmpfs ' + root)
    cfg = {'keep_undelivered_alerts_for': '24h', 'undelivered_alerts_content_size_limit': '8MiB', 'socket': socket, 'state_dir': root + '/state', 'alert_destinations': vm.NO_DESTINATION, 'alert_severity_when_this_node_goes_silent': 'sos',
           'alert_severity_when_this_node_has_rebooted': 'info',
           # The tmpfs holds 8 MiB, so the measurement history gets 1 MiB.
           'keep_measurement_history_for': '7d', 'measurement_history_size_limit': '1MiB',
           'heartbeat_receiver': {'listen_on_address': '127.0.0.1:18445', 'tls_certificate_file': '/etc/mmwatchdog/secrets/cert.pem', 'tls_private_key_file': '/etc/mmwatchdog/secrets/key.pem'},
           'check_groups': {
               'storage': {'check_every': '400ms', 'time_limit_per_run': '200ms', 'alert_after_failing_for': '0s', 'recovered_after_healthy_for': '0s',
                           'checks': {'health': {'shell': 'if [ -f /var/tmp/mmwatchdog-fixture/storage-fail ]; then printf "%8000s" evidence; exit 7; fi', 'alert_severity': 'warn'}},
                           'restart_systemd_unit': {'reset_attempts_after_healthy_for': '10m', 'systemd_unit': 'mmwatchdog-storage-target.service', 'triggered_by_checks': ['health'], 'restart_after_failing_for': '0s',
                                                    'max_attempts': 2, 'min_time_between_attempts': '1s'}},
               'receipt': {'heartbeat': {'max_time_between_heartbeats': '1h', 'expected_bearer_token_file': '/etc/mmwatchdog/secrets/backup'}, 'alert_severity': 'sos'}}}
    def cli(command, code=None):
        p = vm.console('/usr/local/bin/mmwatchdog ' + command + ' --socket ' + socket, check=False)
        if code is not None:
            assert p.returncode == code, (command, p.returncode, p.stdout, p.stderr)
        return p
    def status():
        return json.loads(cli('status --json').stdout)
    vm.write('/etc/systemd/system/mmwatchdog-storage-target.service', '''[Service]
ExecStart=/bin/sh -c 'echo started >> /var/tmp/mmwatchdog-fixture/storage-starts; exec sleep infinity'
''')
    vm.write_configuration(cfg, '/var/tmp/mmwatchdog-fixture/storage')
    vm.write('/etc/systemd/system/mmwatchdog-storage.service', '''[Service]
ExecStart=/usr/local/bin/mmwatchdog daemon --config-dir /var/tmp/mmwatchdog-fixture/storage
KillMode=control-group
''')
    try:
        vm.console('systemctl daemon-reload; systemctl start mmwatchdog-storage-target mmwatchdog-storage')
        wait(lambda: status()['checks']['storage/health']['state'] == 'ok')
        receipt_before = status()['checks']['receipt/heartbeat']['next_due_by']
        # Stop writes, truncate the WAL, then consume all remaining tmpfs blocks.
        vm.signal_main_process('mmwatchdog-storage.service', 'STOP')
        vm.console("python3 -c " + shlex.quote("import sqlite3; c=sqlite3.connect('" + root + "/state/state.db'); print(c.execute('PRAGMA wal_checkpoint(TRUNCATE)').fetchall()); c.close()"))
        vm.console('dd if=/dev/zero of=' + root + '/fill bs=4096 status=none 2>/dev/null || true')
        vm.console('touch /var/tmp/mmwatchdog-fixture/storage-fail')
        vm.signal_main_process('mmwatchdog-storage.service', 'CONT')
        wait(lambda: status().get('storage_error'))
        result = cli('status --json')
        snapshot = json.loads(result.stdout)
        assert snapshot['storage_error']
        # Confirmed failures take precedence over the storage problem.
        expected_exit = 1 if any(c['state'] in {'failed', 'confirming_recovery'}
                                 for c in snapshot['checks'].values()) else 3
        assert result.returncode == expected_exit, result.stdout
        assert vm.console('wc -l < /var/tmp/mmwatchdog-fixture/storage-starts').stdout.strip() == '1'
        # Use HTTPS directly so no sender retry can mask a false acknowledgment.
        code = vm.console("python3 -c " + shlex.quote('''import ssl, urllib.request, urllib.error
r=urllib.request.Request('https://127.0.0.1:18445/v1/heartbeats/receipt',data=b'',headers={'Authorization':'Bearer mmwatchdog-integration-heartbeat-token'})
try:
 print(urllib.request.urlopen(r,context=ssl.create_default_context(cafile='/etc/mmwatchdog/secrets/cert.pem')).status)
except urllib.error.HTTPError as e:
 print(e.code)
''')).stdout.strip()
        assert code == '503', code
        assert status()['checks']['receipt/heartbeat']['next_due_by'] == receipt_before
        vm.console('rm ' + root + '/fill')
        wait(lambda: not status().get('storage_error'))
        wait(lambda: status()['systemd_unit_restart_attempts']['mmwatchdog-storage-target.service']['attempts_used'] > 0)
        # The heartbeat sender talks HTTPS to the receiver, not the daemon's socket.
        p = vm.console('/usr/local/bin/mmwatchdog heartbeat receipt --receiver-url https://127.0.0.1:18445 --bearer-token-file /etc/mmwatchdog/secrets/backup --trusted-certificate-authority-file /etc/mmwatchdog/secrets/cert.pem', check=False)
        assert p.returncode == 0, (p.returncode, p.stdout, p.stderr)
        assert status()['checks']['receipt/heartbeat']['next_due_by'] != receipt_before
        retained = json.loads(cli('events storage --json', 0).stdout)
        assert len([e for e in retained if e['type'] == 'failed' and e['check'] == 'health']) == 1
        assert next(e for e in retained if e['type'] == 'failed')['evidence']['observation']['exit_code'] == 7
        mark('real disk exhaustion preserves inspection, withholds restarts and heartbeat acknowledgments, and recovers without duplicate incidents')
    finally:
        vm.signal_main_process('mmwatchdog-storage.service', 'CONT', check=False)
        vm.console(vm.each_unit('stop', ['mmwatchdog-storage', 'mmwatchdog-storage-target']))
        vm.console('umount ' + root)
