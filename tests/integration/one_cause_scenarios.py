"""One cause, one alert on a real node: a check names the check of its cause
with no_alerts_or_recovery_actions_while_these_checks_fail. While the cause
fails, MMWatchdog sends no alert for the check and does not restart its unit."""
import json
import time
import vm

UNIT = 'mmwatchdog-one-cause.service'
FRAGMENT = '/etc/mmwatchdog/check_groups/one_cause.yml'
STARTS = '/var/tmp/mmwatchdog-fixture/one-cause-starts'
DATABASE_DOWN = '/var/tmp/mmwatchdog-fixture/one-cause-database-down'
APP_DOWN = '/var/tmp/mmwatchdog-fixture/one-cause-app-down'
DATABASE = 'one_cause_database/database_is_up'
APP = 'one_cause_app/app_answers'
SETTING = 'no_alerts_or_recovery_actions_while_these_checks_fail'
# The line of the validation warning for a check that names a check with a
# lower alert_severity.
LOWER_SEVERITY = '  ' + APP + ' has alert_severity sos and names ' + DATABASE + ', which has alert_severity warn\n'


def check_groups(named, database_severity='sos', app_severity='warn'):
    """The database's check and the application's check, which names the
    checks in named as its causes and whose failure restarts the unit."""
    timings = {'check_every': '300ms', 'time_limit_per_run': '200ms', 'alert_after_failing_for': '1s', 'recovered_after_healthy_for': '600ms'}
    # The incident of the database's check opens at its first failed run.
    # Only observed health ends the failing time of a check whose incident
    # is open, so a run that a busy guest cannot make changes nothing here.
    return json.dumps({'check_groups': {
        'one_cause_database': dict(timings, alert_after_failing_for='0s', checks={
            'database_is_up': {'shell': 'test ! -e ' + DATABASE_DOWN, 'alert_severity': database_severity}}),
        'one_cause_app': dict(timings, checks={
            'app_answers': {'shell': 'test ! -e ' + APP_DOWN, 'alert_severity': app_severity, SETTING: named}},
            restart_systemd_unit={'systemd_unit': UNIT, 'triggered_by_checks': ['app_answers'], 'restart_after_failing_for': '500ms',
                                  'max_attempts': 2, 'min_time_between_attempts': '500ms', 'reset_attempts_after_healthy_for': '10m'}),
    }}, indent=2)


def suite(cli, status, state, events, reports, wait, mark):
    vm.write('/etc/systemd/system/' + UNIT, '''[Unit]
Description=MMWatchdog one cause, one alert fixture
[Service]
ExecStart=/bin/sh -c 'echo started >> ''' + STARTS + '''; exec /bin/sleep infinity'
''')
    vm.console('rm -f ' + DATABASE_DOWN + ' ' + APP_DOWN + '; systemctl daemon-reload; systemctl start ' + UNIT)
    vm.write(FRAGMENT, check_groups([DATABASE]))
    assert LOWER_SEVERITY not in cli('validate', 0).stdout
    reloaded = cli('reload', 0).stdout
    assert reloaded.startswith('The configuration is active.\n') and LOWER_SEVERITY not in reloaded, reloaded

    def starts():
        return int(vm.console('wc -l < ' + STARTS).stdout)

    def attempts_used():
        return status()['systemd_unit_restart_attempts'][UNIT]['attempts_used']

    def alerts(check_group):
        return [r['event'] for r in reports() if r['accepted'] and r['event'].get('check_group') == check_group]

    def alert_types(check_group):
        return [e['type'] for e in alerts(check_group)]

    def failing(key):
        return state(key)['state'] in ('confirming_failure', 'failed')

    def database_stops_and_the_app_fails_with_it():
        """Returns the incident of the application's check, which opened
        while the database's check was failing."""
        vm.console('touch ' + DATABASE_DOWN)
        wait(lambda: failing(DATABASE))
        vm.console('touch ' + APP_DOWN)
        wait(lambda: state(APP)['failure_incident'].get('incident_id'))
        wait(lambda: state(DATABASE)['failure_incident'].get('incident_id'))
        # The alert and the restart of the application's check are both
        # long due now.
        time.sleep(1.5)
        return state(APP)['failure_incident']['incident_id']

    quiet_sentence = APP + ' is failing; no alert and no recovery action while ' + DATABASE + ' fails'
    try:
        wait(lambda: state(DATABASE)['state'] == 'ok' and state(APP)['state'] == 'ok')
        assert '  No alerts or recovery actions while these checks fail: ' + DATABASE + '\n' in cli('status one_cause_app', 0).stdout
        assert '  While this check fails, no alerts or recovery actions for: ' + APP + '\n' in cli('status one_cause_database', 0).stdout
        started = starts()

        # The database stops, and the application fails with it. MMWatchdog sends
        # one alert, for the database, and leaves the application's unit
        # alone.
        incident = database_stops_and_the_app_fails_with_it()
        wait(lambda: alert_types('one_cause_database') == ['failed'])
        cause = alerts('one_cause_database')[0]
        assert cause['no_alerts_or_recovery_actions_for_these_checks_while_this_check_fails'] == [APP], cause
        assert cause['number_of_checks_with_no_alerts_or_recovery_actions_while_this_check_fails'] == 1, cause
        assert '\nno alerts and no recovery actions for these checks while this check fails: ' + APP in cause['body'], cause['body']
        assert alert_types('one_cause_app') == [], alert_types('one_cause_app')
        assert attempts_used() == 0 and starts() == started, (attempts_used(), starts(), started)
        quiet = state(APP)
        assert quiet['state'] == 'failed' and quiet['no_alerts_or_recovery_actions_now_while_check_fails'] == DATABASE, quiet
        assert quiet_sentence + '\n' in cli('status').stdout
        recorded = [e for e in events() if e.get('incident_id') == incident]
        assert [e['type'] for e in recorded] == ['failed'] and recorded[0]['message'].endswith('; no alert while ' + DATABASE + ' fails') and 'title' not in recorded[0], recorded
        mark('while the check of its cause fails, a failing check sends no alert and restarts nothing; the alert of the cause names it, and mmwatchdog status says what MMWatchdog holds back')

        # The database is healthy again, and the application still fails:
        # its next run decides. The alert that became due goes out for the
        # incident that opened in the quiet time, and MMWatchdog restarts the unit.
        vm.console('rm ' + DATABASE_DOWN)
        wait(lambda: alert_types('one_cause_database') == ['failed', 'recovered'])
        wait(lambda: alert_types('one_cause_app') == ['failed'])
        assert alerts('one_cause_app')[0]['incident_id'] == incident, alerts('one_cause_app')
        wait(lambda: attempts_used() >= 1 and starts() > started)
        assert quiet_sentence not in cli('status').stdout
        vm.console('rm ' + APP_DOWN)
        wait(lambda: alert_types('one_cause_app') == ['failed', 'recovered'])
        mark('when the cause is healthy again, the next run of the quiet check decides: a check that still fails alerts and its unit is restarted')

        # The same outage, and the application is healthy again before the
        # database is: MMWatchdog sends nothing about the application.
        wait(lambda: state(DATABASE)['state'] == 'ok' and state(APP)['state'] == 'ok')
        cli('reset-recovery-attempts one_cause_app', 0)
        started = starts()
        incident = database_stops_and_the_app_fails_with_it()
        vm.console('rm ' + APP_DOWN)
        wait(lambda: state(APP)['state'] == 'ok')
        vm.console('rm ' + DATABASE_DOWN)
        wait(lambda: alert_types('one_cause_database') == ['failed', 'recovered', 'failed', 'recovered'])
        wait(lambda: state(DATABASE)['state'] == 'ok' and not state(APP).get('no_alerts_or_recovery_actions_now_while_check_fails') and not state(APP).get('no_alerts_or_recovery_actions_until_next_run_because_check_was_failing'))
        time.sleep(1)
        assert alert_types('one_cause_app') == ['failed', 'recovered'], alert_types('one_cause_app')
        assert attempts_used() == 0 and starts() == started, (attempts_used(), starts(), started)
        # Newest first: the incident opened and closed in the records only.
        assert [e['type'] for e in events() if e.get('incident_id') == incident] == ['recovered', 'failed']
        mark('a quiet check that is healthy again when its cause is sends nothing: its incident is only recorded')

        # The application's check is sos and names a check that is only a
        # warning: while the database's check fails, the one alert is a
        # warning. The configuration is valid, and mmwatchdog validate and mmwatchdog reload
        # both say so.
        wait(lambda: state(DATABASE)['state'] == 'ok' and state(APP)['state'] == 'ok')
        vm.write(FRAGMENT, check_groups([DATABASE], database_severity='warn', app_severity='sos'))
        validated = cli('validate', 0).stdout
        assert validated.startswith('Valid configuration: ') and LOWER_SEVERITY in validated, validated
        reloaded = cli('reload', 0).stdout
        assert reloaded.startswith('The configuration is active.\n') and LOWER_SEVERITY in reloaded, reloaded
        assert json.loads(cli('validate --json', 0).stdout)['checks_that_name_a_check_with_a_lower_alert_severity'] == [
            {'check_group': 'one_cause_app', 'check': 'app_answers', 'alert_severity': 'sos', 'named_check': DATABASE, 'named_check_alert_severity': 'warn'}]
        mark('a check that names a check with a lower alert_severity is valid, and mmwatchdog validate and mmwatchdog reload both print the validation warning that names both checks and both severities')

        # A name that matches no check of the node is refused.
        vm.write(FRAGMENT, check_groups(['one_cause_database/database_is_down']))
        rejected = cli('reload', 4)
        assert SETTING + ': this node has no check named "one_cause_database/database_is_down": the checks of the check group one_cause_database are ' + DATABASE in rejected.stderr, rejected.stderr
        mark('a check that names a cause that the node does not have is refused')
    finally:
        vm.console('rm -f ' + FRAGMENT + ' ' + DATABASE_DOWN + ' ' + APP_DOWN)
        cli('reload')
        vm.console('systemctl stop ' + UNIT)
