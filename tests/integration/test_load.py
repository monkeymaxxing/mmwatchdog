"""Load-run verdict checks; no guests or cloud servers are needed."""
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('load', Path(__file__).with_name('load.py'))
load = importlib.util.module_from_spec(spec)
spec.loader.exec_module(load)


PHASES = ('steady', 'silence', 'settle-silence', 'fail', 'settle-fail')


def recorded_result():
    return {
        'nodes': 500, 'group': 100,
        'hub': {'restarts': '0'},
        'most_failed_reports_in_a_row_of_one_node': 0,
        'node_with_most_failed_reports_in_a_row': '',
        'alert_groups': {
            key: {'nodes': 100, 'expected': 100, 'messages': 2}
            for key in ('silence_failed', 'silence_recovered', 'fail_failed', 'fail_recovered')
        },
        # The failure alerts go in the fail phase, the recovery alerts in
        # settle-fail. The join line has no alerts; the end line has those
        # still in flight when the phases ended.
        'phases': [{'phase': 'join', 'ok': 500, 'errors': {}}] + [
            {'phase': name, 'ended': True, 'ok': 500, 'errors': {},
             'alerts': {'ok': 100 if name in ('fail', 'settle-fail') else 0, 'errors': {}}}
            for name in PHASES
        ] + [{'phase': 'end', 'most_failed_reports_in_a_row_of_one_node': 0,
              'node_with_most_failed_reports_in_a_row': '', 'alerts': {'ok': 0, 'errors': {}}}],
    }


def phase_line(result, name):
    return next(line for line in result['phases'] if line['phase'] == name)


class LoadVerdictTests(unittest.TestCase):
    def test_healthy_run_and_a_failure_repaired_by_the_next_report_pass(self):
        for maximum in (0, 1):
            with self.subTest(failed_reports_in_a_row=maximum):
                result = recorded_result()
                result['most_failed_reports_in_a_row_of_one_node'] = maximum
                load.assert_load_result(result)

    def test_unexpected_daemon_restart_fails(self):
        result = recorded_result()
        result['hub']['restarts'] = '1'
        with self.assertRaisesRegex(AssertionError, 'restart'):
            load.assert_load_result(result)

    def test_two_or_more_consecutive_failed_reports_fail(self):
        for maximum in (2, 3):
            with self.subTest(failed_reports_in_a_row=maximum):
                result = recorded_result()
                result['most_failed_reports_in_a_row_of_one_node'] = maximum
                result['node_with_most_failed_reports_in_a_row'] = 'load-00042'
                with self.assertRaisesRegex(AssertionError, 'report'):
                    load.assert_load_result(result)

    def test_deliberate_silence_does_not_require_reports_from_silent_nodes(self):
        result = recorded_result()
        result['phases'] = [{'phase': 'silence', 'ended': True, 'ok': 400, 'errors': {}}]
        load.assert_load_result(result)

    def test_incomplete_or_excessive_grouped_alerts_fail(self):
        for key in recorded_result()['alert_groups']:
            for field, value in [('nodes', 99), ('nodes', 101), ('messages', 6)]:
                with self.subTest(alert_group=key, field=field, value=value):
                    result = recorded_result()
                    result['alert_groups'][key][field] = value
                    with self.assertRaises(AssertionError):
                        load.assert_load_result(result)

    def assert_fails_naming(self, result, phase, errors):
        with self.assertRaises(AssertionError) as raised:
            load.assert_load_result(result)
        message = str(raised.exception)
        self.assertIn(repr(phase), message)
        for error, count in errors.items():
            self.assertIn(error, message)
            self.assertIn(str(count), message)

    def test_any_alert_post_error_fails_and_names_the_error_and_its_count(self):
        # Every grouped message arrived; the failed posts still fail the run.
        for phase, errors in [
            ('fail', {'HTTP 400': 1234}),  # failure alerts
            ('settle-fail', {'HTTP 400': 1234}),  # recovery alerts
            ('steady', {'connection': 1234}),
            ('settle-silence', {'timeout': 1234}),
            ('silence', {'HTTP 503': 1234, 'connection': 567}),
            ('end', {'HTTP 400': 1234}),  # still in flight at shutdown
        ]:
            with self.subTest(phase=phase, errors=errors):
                result = recorded_result()
                phase_line(result, phase)['alerts']['errors'] = errors
                self.assert_fails_naming(result, phase, errors)

    def test_an_alert_post_error_is_named_before_missing_grouped_messages(self):
        result = recorded_result()
        phase_line(result, 'fail')['alerts']['errors'] = {'HTTP 400': 1234}
        result['alert_groups']['fail_failed'].update(nodes=0, messages=0)
        self.assert_fails_naming(result, 'fail', {'HTTP 400': 1234})
