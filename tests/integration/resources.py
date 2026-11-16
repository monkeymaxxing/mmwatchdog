"""Observe real CPU/memory, including a service's worker process, on Linux."""
import json
import re
import shlex
import time
import vm

# A small isolated tmpfs remounted read-only; vm.reset unmounts it after an interruption.
READONLY = '/var/tmp/mmwatchdog-fixture/readonly'
# The node's measurement history, and its settings file.
HISTORY = '/var/lib/mmwatchdog/measurement_history.db'
NODE_SETTINGS = '/etc/mmwatchdog/mmwatchdog_node_settings.yml'
# A module whose every run reports 100 measurements, about 8 KiB of
# readings, so that a small size limit is reached in seconds. The module is
# an executable, and its description lists each measurement with its unit.
MANY_MEASUREMENTS = '''#!/bin/bash
cat >/dev/null
printf '{"protocol":1,"status":"ok","message":"100 measurements","measurements":{'
separator=
for i in $(seq 1 100); do
  printf '%s"queue_%03d_depth_of_a_queue_with_a_long_name_that_takes_room":%d' "$separator" "$i" "$i"
  separator=,
done
printf '}}'
'''
MANY_MEASUREMENT_NAMES = ['queue_%03d_depth_of_a_queue_with_a_long_name_that_takes_room' % i for i in range(1, 101)]
# Two modules that keep state between runs. The script stores the number of
# its runs with write_state. The executable keeps the number in a file of
# its own in the state directory that MMWatchdog names in the request, and reports
# the account that owns the directory.
COUNTS_ITS_RUNS = '''def run_check(inputs, file_contents):
    runs = (read_state() or 0) + 1
    write_state(runs)
    return {"status": "ok", "message": "run %d" % runs, "measurements": {"runs": runs}}
'''
COUNTS_ITS_RUNS_IN_A_FILE = '''#!/bin/bash
set -o pipefail
request=$("$MMWATCHDOG_MODULE_HELPER" module-json request) || exit 1
state_dir=$(printf '%s' "$request" | "$MMWATCHDOG_MODULE_HELPER" module-json get --type string /state_dir) || exit 1
runs=$(( $(cat "$state_dir/runs" 2>/dev/null || echo 0) + 1 ))
printf '%s\\n' "$runs" > "$state_dir/runs" || exit 1
"$MMWATCHDOG_MODULE_HELPER" module-json result --status ok \\
  --message "run $runs as $(id -un) in $state_dir, which belongs to $(stat -c '%U %a' "$state_dir")" --measurement runs "$runs"
'''


def history_rows(query):
    """Rows of the node's measurement history, read on the guest."""
    script = "import json, sqlite3; print(json.dumps(sqlite3.connect('file:" + HISTORY + "?mode=ro', uri=True).execute(" + repr(query) + ").fetchall()))"
    return json.loads(vm.console('python3 -c ' + shlex.quote(script)).stdout)


def suite(cli, status, state, events, wait, mark):
    unit = 'mmwatchdog-resources.service'
    vm.write('/var/tmp/mmwatchdog-fixture/worker.py', '''import subprocess, sys
# The main process waits; its child holds memory and consumes one CPU.
subprocess.run([sys.executable, '-c', 'buffer=bytearray(48*1024*1024)\\nwhile True: pass'], check=True)
''')
    vm.write('/etc/systemd/system/' + unit, '''[Unit]
Description=MMWatchdog resource measurement fixture
[Service]
ExecStart=/usr/bin/python3 /var/tmp/mmwatchdog-fixture/worker.py
CPUAccounting=yes
MemoryAccounting=yes
KillMode=control-group
''')
    vm.console('systemctl daemon-reload; systemctl start ' + unit)
    fragment = '/etc/mmwatchdog/check_groups/resources.yml'
    candidate = '/var/tmp/mmwatchdog-fixture/resources-config'
    # The timings of the acceptance fixture's other check groups.
    timings = {'check_every': '800ms', 'time_limit_per_run': '400ms', 'alert_after_failing_for': '1s', 'recovered_after_healthy_for': '500ms'}
    unit_cpu = {'systemd_unit_resource': {'systemd_unit': unit, 'resource': 'cpu'}}
    unit_memory = {'systemd_unit_resource': {'systemd_unit': unit, 'resource': 'memory'}}
    cfg = {'check_groups': {
        'resources': {'check_every': '1500ms', 'time_limit_per_run': '750ms',
                      'alert_after_failing_for': '0s', 'recovered_after_healthy_for': '0s',
                      'checks': {
                          'cpu': dict(unit_cpu, only_record_readings=True),
                          'memory': dict(unit_memory, only_record_readings=True),
                          'cpu_limit': dict(unit_cpu, fail_when_measurement_above={'cpu_used_percent': 1}, alert_severity='warn'),
                          'memory_limit': dict(unit_memory, fail_when_measurement_above={'memory_used_mib': 1}, alert_severity='warn')}},
        'node_cpu': dict(timings, check_every='1500ms', time_limit_per_run='750ms', node_resource='cpu', only_record_readings=True),
        'node_memory': dict(timings, node_resource='memory', only_record_readings=True),
        'node_swap': dict(timings, node_resource='swap', only_record_readings=True),
        # One load check measures the 1, 5, and 15 minute averages, each also
        # per core; one filesystem check measures space, inodes, and free space.
        'node_load': dict(timings, node_resource='load', only_record_readings=True),
        'root_filesystem': dict(timings, node_filesystem='/', only_record_readings=True),
    }}
    original_settings = vm.console('cat ' + NODE_SETTINGS).stdout
    settings = json.loads(original_settings)
    try:
        vm.write(fragment, json.dumps(cfg, indent=2))
        cli('reload', 0)
        wait(lambda: state('resources/cpu')['last_observation']['measurements']['cpu_used_percent']['value'] > 10)
        wait(lambda: state('resources/memory')['last_observation']['measurements']['memory_used_mib']['value'] > 48)
        wait(lambda: state('node_cpu/node_resource')['last_observation'].get('sample_window_ns', 0) > 500_000_000)
        wait(lambda: state('node_load/node_resource')['last_observation'].get('measurements'))
        wait(lambda: state('root_filesystem/node_filesystem')['last_observation'].get('measurements'))
        for name in ['resources/cpu', 'resources/memory', 'node_cpu/node_resource', 'node_memory/node_resource', 'node_swap/node_resource',
                     'node_load/node_resource', 'root_filesystem/node_filesystem']:
            # A check that missed one run in a slow moment is unknown until
            # its next run.
            wait(lambda: state(name)['state'] == 'ok')
            s = state(name)
            assert s['records_only'] and not s['failure_incident'].get('incident_id'), s
        cpu = state('resources/cpu')['last_observation']
        assert cpu['measurements']['cpu_used_percent']['unit'] == 'percent' and 10 < cpu['measurements']['cpu_used_percent']['value'] <= 100, cpu
        # A check without a limit names each reading; a CPU reading adds its sample window.
        assert cpu['message'].startswith('cpu_used_percent is ') and ' over ' in cpu['message'] and 'fails' not in cpu['message'], cpu
        assert state('resources/memory')['last_observation']['measurements']['memory_used_mib']['unit'] == 'MiB'
        memory = state('node_memory/node_resource')['last_observation']
        assert memory['message'].startswith('memory_used_percent is ') and memory['measurements']['memory_used_percent']['unit'] == 'percent', memory
        filesystem = state('root_filesystem/node_filesystem')['last_observation']
        assert filesystem['message'].startswith('space_used_percent is ') and '; inodes_used_percent is ' in filesystem['message'] and '; space_free_gib is ' in filesystem['message'], filesystem
        space, free = (filesystem['measurements'][name] for name in ['space_used_percent', 'space_free_gib'])
        assert space['unit'] == 'percent' and 0 < space['value'] < 100, space
        assert free['unit'] == 'GiB' and free['value'] > 0, free
        inodes = filesystem['measurements']['inodes_used_percent']
        assert inodes['unit'] == 'percent' and 0 < inodes['value'] < 100, inodes
        swap = state('node_swap/node_resource')['last_observation']['measurements']['swap_used_percent']
        assert swap['unit'] == 'percent' and 0 <= swap['value'] <= 100, swap
        # Per-core load is the kernel's 5-minute average over the online
        # processors. The kernel moves the average every five seconds, so the
        # guest reads it just before and just after the check, in one command:
        # the check's value lies between the two readings.
        before, measured, after = vm.console("cut -d ' ' -f 2 /proc/loadavg; echo ---; /usr/local/bin/mmwatchdog check-now node_load --json; echo ---; "
                                             "cut -d ' ' -f 2 /proc/loadavg; getconf _NPROCESSORS_ONLN").stdout.split('---')
        load = json.loads(measured)['node_resource']['measurements']
        assert set(load) == {'load_average_1m', 'load_average_5m', 'load_average_15m',
                             'load_average_1m_per_core', 'load_average_5m_per_core', 'load_average_15m_per_core'}, load
        lowest, highest = sorted([float(before), float(after.split()[0])])
        processors = int(after.split()[1])
        assert load['load_average_5m']['unit'] == 'load' and lowest - 0.005 <= load['load_average_5m']['value'] <= highest + 0.005, (load, before, after)
        per_core = load['load_average_5m_per_core']
        assert per_core['unit'] == 'load per core' and abs(per_core['value'] - load['load_average_5m']['value'] / processors) < 0.005, (load, processors)
        assert not [e for e in events() if e.get('check_group') == 'resources' and e['check'] in {'cpu', 'memory'} and e['type'] == 'failed']
        cli('check-now node_cpu --json', 0)
        mark('node CPU, memory, swap, load with per-core load, and a filesystem\'s space, inodes, and free space are recorded without limits; systemd unit measurements include CPU and memory consumed by child processes')

        # A filesystem remounted read-only fails every check of it that states
        # an alert_severity, with or without a limit, and still records the
        # readings.
        vm.console('mkdir -p ' + READONLY + '; mount -t tmpfs -o size=1m tmpfs ' + READONLY)
        timing = {'check_every': '500ms', 'time_limit_per_run': '250ms', 'alert_after_failing_for': '0s', 'recovered_after_healthy_for': '0s'}
        cfg['check_groups']['data_space'] = dict(timing, node_filesystem=READONLY, fail_when_measurement_above={'space_used_percent': 85}, alert_severity='sos')
        cfg['check_groups']['data_inodes'] = dict(timing, node_filesystem=READONLY, fail_when_measurement_above={'inodes_used_percent': 85}, alert_severity='warn')
        cfg['check_groups']['data_writable'] = dict(timing, node_filesystem=READONLY, alert_severity='warn')
        cfg['check_groups']['data_recorded'] = dict(timing, node_filesystem=READONLY, only_record_readings=True)
        alerting = ['data_space', 'data_inodes', 'data_writable']
        vm.write(fragment, json.dumps(cfg, indent=2))
        cli('reload', 0)
        names = [group + '/node_filesystem' for group in alerting]
        wait(lambda: all(state(n)['last_observation']['status'] == 'ok' for n in names))
        # records_only is left out of status JSON when it is false.
        assert not state('data_writable/node_filesystem').get('records_only') and state('data_recorded/node_filesystem')['records_only']
        vm.console('mount -o remount,ro ' + READONLY)
        wait(lambda: all(state(n)['failure_incident'].get('incident_id') for n in names))
        for name, measurement in [('data_space/node_filesystem', 'space_used_percent'), ('data_inodes/node_filesystem', 'inodes_used_percent'),
                                  ('data_writable/node_filesystem', 'space_used_percent')]:
            # The checks run twice a second, and a guest that stalls for a
            # moment misses one run: wait for the run that shows the mount.
            last = wait(lambda: (seen := state(name)['last_observation'])['status'] == 'failed' and seen['message'].startswith(READONLY + ' is mounted read-only; ') and seen)
            assert last['measurements'][measurement]['value'] < 85, last
        # A check that only records notes the read-only mount and never fails.
        wait(lambda: state('data_recorded/node_filesystem')['last_observation']['message'].startswith(READONLY + ' is mounted read-only; '))
        recorded = state('data_recorded/node_filesystem')
        assert recorded['last_observation']['status'] == 'ok' and not recorded['failure_incident'].get('incident_id') and recorded['state'] == 'ok', recorded
        vm.console('mount -o remount,rw ' + READONLY)
        wait(lambda: not any(state(n)['failure_incident'].get('incident_id') for n in names))
        for group in alerting:
            es = json.loads(cli('events ' + group + ' --json', 0).stdout)
            # Newest first.
            assert [e['type'] for e in es if e['type'] in {'failed', 'recovered'}] == ['recovered', 'failed'], es
        for group in alerting + ['data_recorded']:
            del cfg['check_groups'][group]
        vm.write(fragment, json.dumps(cfg, indent=2))
        cli('reload', 0)
        vm.console('umount ' + READONLY)
        mark('a read-only filesystem fails its checks that state an alert_severity, with a limit and without one, keeps the readings, and recovers once writable; a check that only records notes it without failing')

        wait(lambda: state('resources/cpu_limit')['failure_incident'].get('incident_id') and state('resources/memory_limit')['failure_incident'].get('incident_id'))
        evidence = state('resources/cpu_limit')['failure_incident']['evidence']['observation']
        assert evidence['measurements']['cpu_used_percent']['value'] > 1 and evidence['sample_window_ns'] > 0
        assert evidence['message'].startswith('cpu_used_percent is ') and ' (fails above 1) over ' in evidence['message'], evidence
        assert ' (fails above 1)' in state('resources/memory_limit')['failure_incident']['evidence']['observation']['message']
        assert 'MiB' in cli('status resources', 1).stdout
        mark('explicit CPU and memory limits create incidents with the readings, the limit in the message, and CPU sample windows, while checks that only record stay healthy')

        # Removing a limit retires its incident without fabricating recovery;
        # a check that only records takes no alert_severity.
        old = state('resources/cpu_limit')['failure_incident']['incident_id']
        del cfg['check_groups']['resources']['checks']['cpu_limit']['fail_when_measurement_above']
        del cfg['check_groups']['resources']['checks']['cpu_limit']['alert_severity']
        cfg['check_groups']['resources']['checks']['cpu_limit']['only_record_readings'] = True
        vm.write(fragment, json.dumps(cfg, indent=2))
        cli('reload', 0)
        wait(lambda: state('resources/cpu_limit')['state'] == 'ok')
        assert state('resources/cpu_limit')['records_only']
        es = json.loads(cli('events resources --json', 0).stdout)
        assert any(e['type'] == 'closed_without_recovery' and e.get('incident_id') == old for e in es)
        assert not any(e['type'] == 'recovered' and e.get('incident_id') == old for e in es)
        mark('selecting only_record_readings closes the old incident without a false recovery, and the check then only records')

        # Inactive units have unavailable measurements, not synthetic zero usage.
        vm.console('systemctl stop ' + unit)
        wait(lambda: state('resources/cpu')['state'] == 'unknown' and state('resources/memory')['state'] == 'unknown')
        assert not state('resources/memory')['last_observation'].get('measurements')
        vm.console('systemctl start ' + unit)
        wait(lambda: state('resources/cpu')['last_observation'].get('measurements', {}).get('cpu_used_percent', {}).get('value', 0) > 10)
        wait(lambda: state('resources/memory')['last_observation'].get('measurements', {}).get('memory_used_mib', {}).get('value', 0) > 48)
        vm.console('systemctl restart mmwatchdog')
        wait(lambda: state('resources/cpu')['state'] == 'ok')
        mark('systemd unit stop/restart and daemon restart discard stale CPU baselines and recover real measurements without invented zeros')

        # An ordinary successful backup produces a receipt. The example module
        # file_age, an executable in Bash that uses the JSON helper, measures
        # the receipt's age, and the check states the limit of 60 minutes.
        vm.write_module('file_age', vm.module_files(vm.REPO / 'examples/modules/file_age'))
        cfg['check_groups']['backup_receipt'] = dict(timings, file_age={'file_path': '/var/tmp/mmwatchdog-fixture/backup.complete'},
                                                     fail_when_measurement_above={'age_minutes': 60}, alert_severity='warn')
        vm.write(fragment, json.dumps(cfg, indent=2))
        cli('reload', 0)
        absent = json.loads(cli('check-now backup_receipt --json', 1).stdout)['file_age']
        assert absent['message'] == 'the file does not exist or is not a regular file', absent
        vm.console("printf 'backup data\\n' > /var/tmp/mmwatchdog-fixture/backup.part; test -s /var/tmp/mmwatchdog-fixture/backup.part && mv /var/tmp/mmwatchdog-fixture/backup.part /var/tmp/mmwatchdog-fixture/backup.data && touch /var/tmp/mmwatchdog-fixture/backup.complete")
        fresh = json.loads(cli('check-now backup_receipt --json', 0).stdout)['file_age']
        assert fresh['message'] == 'age_minutes is 0 (fails above 60); the module reported: the file is 0 minutes old', fresh
        assert fresh['measurements']['age_minutes'] == {'value': 0, 'unit': 'minutes'}, fresh
        vm.console("touch -d '2 hours ago' /var/tmp/mmwatchdog-fixture/backup.complete")
        stale = json.loads(cli('check-now backup_receipt --json', 1).stdout)['file_age']
        assert stale['status'] == 'failed' and stale['measurements']['age_minutes']['value'] >= 120, stale
        assert ' (fails above 60); the module reported: the file is ' in stale['message'], stale
        vm.console("touch -d '5 minutes' /var/tmp/mmwatchdog-fixture/backup.complete")
        future = json.loads(cli('check-now backup_receipt --json', 3).stdout)['file_age']
        assert future['status'] == 'unknown'
        # mmwatchdog validate checks what a check states against the description of
        # its module, before anything runs.
        vm.console('rm -rf ' + candidate + '; cp -a /etc/mmwatchdog ' + candidate)
        for check, problem in [
            (dict(file_age={'path': '/var/tmp/mmwatchdog-fixture/backup.complete'}), 'check_groups.backup_receipt.file_age.path: unknown field "path": the inputs of the module file_age are file_path'),
            (dict(file_age={}), 'check_groups.backup_receipt.file_age.file_path: is required: '),
            (dict(file_age={'file_path': 7}), 'check_groups.backup_receipt.file_age.file_path: expected a string'),
            (dict(file_age={'file_path': '/x'}, fail_when_measurement_above={'age_hours': 2}), 'check_groups.backup_receipt.fail_when_measurement_above.age_hours: this check has no such measurement; it measures age_minutes'),
            (dict(file_ages={'file_path': '/x'}), 'unknown field "file_ages": the field name matches neither a setting of a check nor an installed module. MMWatchdog has no core module of that name, and no directory of that name is in '),
        ]:
            invalid = dict(cfg, check_groups=dict(cfg['check_groups'], backup_receipt=dict(timings, alert_severity='warn', **check)))
            vm.write(candidate + '/check_groups/resources.yml', json.dumps(invalid, indent=2))
            refused = cli('validate --config-dir ' + candidate, 1)
            assert problem in refused.stderr, (problem, refused.stderr)
        vm.console('rm -rf ' + candidate)
        mark('the example module file_age, an executable, handles absent, fresh, and future-dated completion markers, and MMWatchdog fails the check when the age it measures passes the limit; mmwatchdog validate refuses an input that the module does not have, a missing required input, a value of the wrong type, a limit on a measurement that the module does not report, and a name that no module has')

        # State between runs. Each check whose module is not core has a
        # state directory of its own, which belongs to the account that runs
        # the module. A scheduled run finds what the run before left there.
        # mmwatchdog check-now works on a copy, so looking at a check never changes
        # what the next scheduled run sees.
        vm.write_module('counts_its_runs', {'module_description.yml': vm.module_description('counts_its_runs', measurements={'runs': 'count'}), 'module.star': COUNTS_ITS_RUNS})
        vm.write_module('counts_its_runs_in_a_file', {'module_description.yml': vm.module_description('counts_its_runs_in_a_file', measurements={'runs': 'count'}),
                                                       'module_executable': COUNTS_ITS_RUNS_IN_A_FILE})
        slow = dict(timings, check_every='2s', time_limit_per_run='1s')
        cfg['check_groups']['module_state'] = dict(slow, checks={
            'script': {'counts_its_runs': {}, 'alert_severity': 'info'}, 'executable': {'counts_its_runs_in_a_file': {}, 'alert_severity': 'info'}})
        vm.write(fragment, json.dumps(cfg, indent=2))
        cli('reload', 0)
        own = '/var/lib/mmwatchdog-module-state/module_state/executable'
        def runs(check):
            return state('module_state/' + check)['last_observation'].get('measurements', {}).get('runs', {}).get('value', 0)
        def kept_on_the_node():
            """The two counts as the state directories hold them: the script's, and the executable's."""
            return int(vm.console('cat /var/lib/mmwatchdog-module-state/module_state/script/state.json').stdout), int(vm.console('cat ' + own + '/runs').stdout)
        wait(lambda: runs('script') >= 2 and runs('executable') >= 2)
        message = state('module_state/executable')['last_observation']['message']
        assert re.fullmatch(r'run \d+ as mmwatchdog in ' + own + ', which belongs to mmwatchdog 700', message), message
        assert vm.console('stat -c "%U %a" /var/lib/mmwatchdog-module-state /var/lib/mmwatchdog-module-state/module_state').stdout.split('\n')[:2] == ['root 711', 'root 711']
        # mmwatchdog check-now through the daemon: the run counts on from the state
        # of the latest scheduled run, which may fall between the two reads.
        before = kept_on_the_node()
        now = json.loads(cli('check-now module_state --json', 0).stdout)
        after = kept_on_the_node()
        assert before[0] + 1 <= now['script']['measurements']['runs']['value'] <= after[0] + 1, (before, now, after)
        assert before[1] + 1 <= now['executable']['measurements']['runs']['value'] <= after[1] + 1, (before, now, after)
        # With MMWatchdog stopped, nothing but mmwatchdog check-now runs the modules. Each
        # run counts one on from the state, in a copy that belongs to the
        # module's account, and the state stays as it was.
        vm.console('systemctl stop mmwatchdog')
        kept = kept_on_the_node()
        for _ in range(2):
            now = json.loads(cli('check-now module_state --without-daemon --json', 0).stdout)
            assert (now['script']['measurements']['runs']['value'], now['executable']['measurements']['runs']['value']) == (kept[0] + 1, kept[1] + 1), (now, kept)
            assert re.fullmatch(r'run \d+ as mmwatchdog in /var/lib/mmwatchdog-module-state/\.check-now/module_state-executable-\w+, which belongs to mmwatchdog 700', now['executable']['message']), now
        assert kept_on_the_node() == kept
        assert vm.console('ls -A /var/lib/mmwatchdog-module-state/.check-now').stdout == ''
        # The state survives a restart of MMWatchdog: the counts go on.
        vm.console('systemctl start mmwatchdog')
        wait(lambda: runs('script') > kept[0] and runs('executable') > kept[1])
        # A check that changes is a new check and starts without state, and
        # the state of a check that is removed goes with it.
        cfg['check_groups']['module_state'] = dict(slow, checks={'script': {'counts_its_runs': {'env': {'TZ': 'UTC'}}, 'alert_severity': 'info'}})
        vm.write(fragment, json.dumps(cfg, indent=2))
        cli('reload', 0)
        wait(lambda: 0 < runs('script') <= kept[0])
        assert vm.console('ls /var/lib/mmwatchdog-module-state/module_state').stdout.split() == ['script']
        del cfg['check_groups']['module_state']
        vm.write(fragment, json.dumps(cfg, indent=2))
        cli('reload', 0)
        left = vm.console('ls -A /var/lib/mmwatchdog-module-state').stdout.split()
        assert 'module_state' not in left and 'backup_receipt' in left, left
        mark('a check whose module is not core keeps state between its runs in a directory of its own that only the account of the module can read, for a script through write_state and for an executable through the state_dir of its request; the state survives a restart of MMWatchdog, mmwatchdog check-now works on a copy and leaves it unchanged, and the state of a changed or removed check is removed')

        # Measurement history: every scheduled run of a check that measures
        # stores its readings on the node's own disk, for a check with a limit
        # and for one that only records, for a built-in check and for a module.
        assert vm.console('stat -c %a ' + HISTORY).stdout.strip() == '600'
        stored = {(group, check): runs for group, check, runs in history_rows('SELECT check_group, check_name, COUNT(*) FROM readings GROUP BY 1, 2')}
        # resources/cpu only records, resources/cpu_limit has a limit, and
        # queue_starlark uses a module of the acceptance fixture.
        for key in [('resources', 'cpu'), ('resources', 'cpu_limit'), ('resources', 'memory'), ('node_memory', 'node_resource'), ('node_load', 'node_resource'),
                    ('root_filesystem', 'node_filesystem'), ('queue_starlark', 'queued_jobs')]:
            assert stored.get(key, 0) > 1, (key, stored)
        at, measurements = history_rows("SELECT at, measurements FROM readings WHERE check_group='node_load' ORDER BY id DESC LIMIT 1")[0]
        load = json.loads(measurements)
        assert set(load) == {'load_average_1m', 'load_average_5m', 'load_average_15m', 'load_average_1m_per_core', 'load_average_5m_per_core', 'load_average_15m_per_core'}, load
        assert load['load_average_5m']['unit'] == 'load' and load['load_average_5m_per_core']['unit'] == 'load per core' and abs(at / 1e9 - time.time()) < 120, (load, at)
        history = status()['measurement_history']
        assert history['keep_measurement_history_for'] == '7d' and history['measurement_history_size_limit'] == '256MiB', history
        assert history['oldest_reading_at'] != history['newest_reading_at'] and not history['cut_short_by_measurement_history_size_limit'], history
        assert 'measurement history' not in cli('status').stdout
        # The history survives a daemon restart, and the sequence numbers go on.
        first, last = history_rows('SELECT MIN(id), MAX(id) FROM readings')[0]
        vm.console('systemctl restart mmwatchdog')
        wait(lambda: history_rows('SELECT MAX(id) FROM readings')[0][0] > last + 5)
        assert history_rows('SELECT MIN(id) FROM readings')[0][0] == first and status()['measurement_history']['oldest_reading_at'] == history['oldest_reading_at']
        mark('every scheduled run of a check that measures stores its readings in the node\'s measurement history, a private file of its own that survives a daemon restart; the JSON status says how far back it reaches')

        # The size limit: four checks of a module that reports 100
        # measurements every 200ms fill 1 MiB in about ten seconds. The
        # oldest readings go first, the file stays within the limit while
        # the readings keep coming, and mmwatchdog status says so.
        status_exit_code_before = status()['summary']['exit_code']
        vm.write_module('many_measurements', {'module_description.yml': vm.module_description('many_measurements', measurements={name: 'messages' for name in MANY_MEASUREMENT_NAMES}),
                                               'module_executable': MANY_MEASUREMENTS})
        cfg['check_groups']['many_measurements'] = {'check_every': '200ms', 'time_limit_per_run': '150ms', 'alert_after_failing_for': '1m', 'recovered_after_healthy_for': '30s',
                                                    'checks': {'queues_' + str(number): {'many_measurements': {}, 'alert_severity': 'info'} for number in range(1, 5)}}
        vm.write(fragment, json.dumps(cfg, indent=2))
        settings['measurement_history_size_limit'] = '1MiB'
        vm.write(NODE_SETTINGS, json.dumps(settings, indent=2))
        cli('reload', 0)
        wait(lambda: status()['measurement_history']['cut_short_by_measurement_history_size_limit'], timeout=120)
        time.sleep(2)
        size = int(vm.console('stat -c %s ' + HISTORY).stdout)
        assert size <= 1024 * 1024, size
        oldest, newest = history_rows('SELECT MIN(id), MAX(id) FROM readings')[0]
        assert oldest > first and newest > last, (oldest, newest, first, last)
        line = [text for text in cli('status').stdout.splitlines() if text.startswith('measurement history')]
        assert len(line) == 1 and re.fullmatch(r'measurement history reaches back \d+ (second|minute)s?( \d+ seconds?)?; the size limit of 1 MiB prevented keeping the requested 7 days', line[0]), line
        assert status()['summary']['exit_code'] == status_exit_code_before, status()['summary']
        mark('when the readings reach measurement_history_size_limit, the oldest go first and the file stays within the limit; mmwatchdog status says how far back the history reaches, and that line is no storage problem')

        # A shorter time applies at once after a reload: the readings that
        # are older than two seconds then go, also those of a check that
        # the reload removed, and not up to a minute later at the next
        # retention tick. The history holds about six seconds of readings
        # under its size limit. The time is taken before the reload, so
        # every reading two seconds older than it is too old when MMWatchdog
        # removes them.
        del cfg['check_groups']['many_measurements']
        vm.write(fragment, json.dumps(cfg, indent=2))
        settings['keep_measurement_history_for'] = '2s'
        vm.write(NODE_SETTINGS, json.dumps(settings, indent=2))
        two_seconds_before_the_reload = int(vm.console('date -u +%s%N').stdout) - 2 * 10**9
        older = "SELECT COUNT(*) FROM readings WHERE at < %d" % two_seconds_before_the_reload
        assert history_rows(older)[0][0] > 0 and history_rows(older + " AND check_group='many_measurements'")[0][0] > 0
        cli('reload', 0)
        wait(lambda: history_rows(older)[0][0] == 0, timeout=10)
        assert history_rows('SELECT COUNT(*) FROM readings')[0][0] > 0 and status()['measurement_history']['keep_measurement_history_for'] == '2s'
        mark('a shorter keep_measurement_history_for applies at once after a reload: the readings that are older go, also those of a check that the reload removed')
    finally:
        vm.write(NODE_SETTINGS, original_settings)
        vm.console('rm -rf ' + fragment + ' ' + candidate + ' ' + ' '.join(vm.EXTERNAL_MODULES + '/' + module for module in ['file_age', 'counts_its_runs', 'counts_its_runs_in_a_file', 'many_measurements']))
        cli('reload', 0)
        vm.console('systemctl stop ' + unit)
        vm.console('if mountpoint -q ' + READONLY + '; then umount ' + READONLY + '; fi')
