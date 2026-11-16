"""Install and update the actual release bundle using its shipped MMX workflow."""
import hashlib
import json
from pathlib import Path
import shutil
import tarfile
import time
import mmx_build
import vm

VERSION = '0.1.0-dev.1'
# The four timings that every check group states, for the check groups whose
# timing no step depends on.
TIMINGS = {'check_every': '30s', 'time_limit_per_run': '5s', 'alert_after_failing_for': '1m', 'recovered_after_healthy_for': '30s'}
# The settings that every node states: its two severities, and how much
# measurement history it keeps.
REQUIRED_NODE_SETTINGS = {'keep_undelivered_alerts_for': '24h', 'undelivered_alerts_content_size_limit': '8MiB', 'alert_severity_when_this_node_goes_silent': 'sos', 'alert_severity_when_this_node_has_rebooted': 'info',
                          'keep_measurement_history_for': '7d', 'measurement_history_size_limit': '256MiB'}
FIXTURE_UNIT = """[Unit]
Description=MMWatchdog test fixture
[Service]
ExecStart=/usr/bin/python3 /var/tmp/mmwatchdog-fixture/server.py
Restart=no
[Install]
WantedBy=multi-user.target
"""


def suite(mark=print):
    root = vm.ROOT
    vm.reset()
    vm.setup_step_done('the fixtures of earlier suites are reset')
    provenance = mmx_build.build()
    vm.setup_step_done('MMX is built')
    vm.cmd(['python3', vm.REPO / 'scripts/release.py', VERSION, '--allow-dirty'],
           inherit_lock=True, timeout=600)
    vm.setup_step_done('the release bundles are built')
    output = root / 'releases' / VERSION
    metadata = json.loads((output / 'build.json').read_text())
    work = None
    # Each run unpacks the release anew, so nothing that an earlier run put
    # into the project, such as a module, is deployed by this one.
    shutil.rmtree(root / 'release-work', ignore_errors=True)
    for name, digest in metadata['artifacts'].items():
        archive = output / name
        assert hashlib.sha256(archive.read_bytes()).hexdigest() == digest
        with tarfile.open(archive) as tar:
            prefix = name.removesuffix('.tar.gz')
            manifest = json.load(tar.extractfile(prefix + '/manifest.json'))
            for relative, checksum in manifest['sha256'].items():
                assert hashlib.sha256(tar.extractfile(prefix + '/' + relative).read()).hexdigest() == checksum, relative
            # The project's module directory and the official extra modules,
            # which the workflows install on every node, are in the bundle.
            # The official extra modules carry the version of the release,
            # and each is a script with its description. A program keeps its
            # executable bit.
            assert {'mmwatchdog_external_modules/README.md', 'official_extra_modules/README.md', 'official_extra_modules/shipped_with_mmwatchdog_version',
                    'official_extra_modules/slack/module.star', 'official_extra_modules/slack/module_description.yml',
                    'official_extra_modules/telegram/module.star', 'official_extra_modules/telegram/module_description.yml'} <= set(manifest['sha256']), sorted(manifest['sha256'])
            assert tar.extractfile(prefix + '/official_extra_modules/shipped_with_mmwatchdog_version').read().decode() == VERSION + '\n'
            for relative in ['mmwatchdog', 'examples/modules/file_age/module_executable']:
                assert tar.getmember(prefix + '/' + relative).mode == 0o755, relative
            if manifest['arch'] == vm.ARCH:
                tar.extractall(root / 'release-work', filter='data')
                work = root / 'release-work' / prefix
    assert work
    mark('release bundles verify all file hashes and contain both Linux architectures')
    (work / 'inventories/hub_and_standalone.yml').write_text(json.dumps({'vars': {'mmwatchdog_config_source_dir': 'files/hub_and_standalone', 'mmwatchdog_secret_files': []}, 'nodes': {'mmwatchdog': vm.connection()}}))
    configuration_directory = work / 'files/hub_and_standalone'
    # A webhook receiver records the deploys' maintenance alerts.
    vm.write('/var/tmp/mmwatchdog-fixture/server.py', (vm.REPO / 'tests/integration/fixture.py').read_text())
    vm.write('/etc/systemd/system/mmwatchdog-fixture.service', FIXTURE_UNIT)
    vm.console('systemctl daemon-reload; systemctl start mmwatchdog-fixture')
    (configuration_directory / 'alert_destinations.yml').write_text(json.dumps({'destinations': {'capture': {'remind_every': 'never', 'webhook': {'url': 'http://127.0.0.1:18080/events'}, 'time_limit_per_delivery_attempt': '1s', 'alert_severities': ['info', 'warn', 'sos']}}}))
    # A first install: no MMWatchdog unit or executable yet.
    vm.console('rm -f /etc/systemd/system/mmwatchdog.service /usr/local/bin/mmwatchdog; systemctl daemon-reload')
    mmx = root / 'mmx-bin/mmx'
    apply_seconds = []
    vault = {}  # the vault passphrase, once the secret files scenario makes a vault
    def apply(expected=0):
        started = time.monotonic()
        result = vm.cmd([mmx, 'run', 'hub_and_standalone', '--inventory', 'hub_and_standalone', '--json'],
                        extra_env={'MMX_RUNNER_DIR': str(root / 'mmx-bin'),
                                   'MMX_STATE_DIR': str(root / 'release-mmx-state'), **vault},
                        cwd=work, check=False, timeout=300)
        elapsed = time.monotonic() - started
        apply_seconds.append(round(elapsed, 3))
        print(f'MMX apply {len(apply_seconds)}: {elapsed:.3f}s (exit {result.returncode})', flush=True)
        (root / 'release-deploy-result.json').write_text(result.stdout)
        assert (result.returncode == 0) == (expected == 0), result.stdout[-8000:] + result.stderr
        return result
    def dryrun():
        """mmx dryrun of the playbook: the exit code and each node task's status and change."""
        result = vm.cmd([mmx, 'dryrun', 'hub_and_standalone', '--inventory', 'hub_and_standalone', '--json'],
                        extra_env={'MMX_RUNNER_DIR': str(root / 'mmx-bin'),
                                   'MMX_STATE_DIR': str(root / 'release-mmx-state'), **vault},
                        cwd=work, check=False, timeout=300)
        (root / 'release-dryrun-result.json').write_text(result.stdout)
        tasks = [(r['task'], r['status'], r['change']) for entry in json.loads(result.stdout)['entries']
                 for r in (entry['response'] or {}).get('results') or []]
        return result.returncode, tasks
    def invocation():
        return vm.console('systemctl show mmwatchdog --property=InvocationID --value').stdout.strip()
    def cli(args):
        return vm.console('/usr/local/bin/mmwatchdog ' + args, check=False)
    def ready():
        end = time.monotonic() + 20
        while time.monotonic() < end:
            result = cli('config --json')
            if result.returncode == 0: return json.loads(result.stdout)
            time.sleep(.2)
        raise AssertionError(result.stderr)
    def wait(predicate, what):
        end = time.monotonic() + 20
        while time.monotonic() < end:
            if value := predicate(): return value
            time.sleep(.2)
        raise AssertionError(what)
    def delivered():
        """The workflow's maintenance alerts and the deployment check's alerts at the webhook."""
        lines = vm.console('cat /var/tmp/mmwatchdog-fixture/webhooks.jsonl', check=False).stdout.splitlines()
        return [e for e in (json.loads(line)['event'] for line in lines if line) if e.get('check_group') in {'every_check_group', 'deployment'}]
    apply()
    ready()
    assert vm.console('id -gn mmwatchdog').stdout.strip() == 'mmwatchdog'
    assert vm.console('stat -c "%a %U:%G" /var/lib/mmwatchdog-checks').stdout.strip() == '700 mmwatchdog:mmwatchdog'
    assert vm.console('systemctl is-enabled mmwatchdog').stdout.strip() == 'enabled'
    version = json.loads(cli('version --json').stdout)
    assert version['version'] == VERSION and version['arch'] == vm.ARCH
    assert version['revision'] == metadata['revision'] and version['modified'] == metadata['modified']
    first = invocation()
    assert not [e for e in json.loads(cli('events --json').stdout) if e['type'].startswith('maintenance_')]
    unchanged = json.loads(apply().stdout)
    # A skipped fleet stage, such as write_secret_files without secret
    # files, has no response.
    staged = {result['task']: result for entry in unchanged['entries']
              for result in (entry['response'] or {}).get('results') or []}
    for task in ('stage_config', 'stage_official_extra_modules', 'stage_mmwatchdog_external_modules'):
        assert staged[task]['status'] == 'succeeded' and staged[task]['change'] == 'unchanged', staged[task]
    assert invocation() == first, 'unchanged deployment restarted MMWatchdog'
    assert not [e for e in json.loads(cli('events --json').stdout) if e['type'].startswith('maintenance_')]
    assert not delivered(), 'unchanged deployment sent maintenance alerts'
    code, tasks = dryrun()
    # A dryrun of an unchanged project exits 0: every node task has a dryrun
    # command or a way to tell that nothing would change.
    assert code == 0, (code, [t for t in tasks if t[1:] != ('succeeded', 'unchanged')])
    mark('mmx dryrun of an unchanged project exits 0, with every node task checked')
    mark('shipped MMX workflow installs release; unchanged deployment reports all three staging copies unchanged and preserves the running instance')
    home_check = dict(REQUIRED_NODE_SETTINGS, alert_destinations=vm.NO_DESTINATION, check_groups={'home': dict(
        TIMINGS, shell='test "$HOME" = /var/lib/mmwatchdog-checks && test ! -r /var/lib/mmwatchdog/state.db && stamp=$(mktemp "$HOME/mmwatchdog-test.XXXXXX") && rm "$stamp"', alert_severity='warn')})
    vm.write_configuration(home_check, '/var/tmp/mmwatchdog-fixture/home')
    home = cli('check-now home --without-daemon --config-dir /var/tmp/mmwatchdog-fixture/home --json')
    assert home.returncode == 0, home.stdout + home.stderr
    assert json.loads(home.stdout)['shell']['status'] == 'ok'
    vm.console('install -d -m 0700 -o mmwatchdog -g mmwatchdog /var/tmp/mmwatchdog-fixture/unsafe-state')
    unsafe = dict(home_check, state_dir='/var/tmp/mmwatchdog-fixture/unsafe-state', socket='/var/tmp/mmwatchdog-fixture/unsafe.sock')
    vm.write_configuration(unsafe, '/var/tmp/mmwatchdog-fixture/unsafe')
    rejected = vm.console('timeout 3s /usr/local/bin/mmwatchdog daemon --config-dir /var/tmp/mmwatchdog-fixture/unsafe', check=False)
    assert rejected.returncode == 4 and 'state directory' in rejected.stderr, rejected.stdout + rejected.stderr
    vm.console('test ! -e /var/tmp/mmwatchdog-fixture/unsafe-state/state.db')
    mark('default checks have a usable private home and cannot read daemon state; direct startup rejects check-owned state before opening the database')
    config = dict(REQUIRED_NODE_SETTINGS, check_groups={'deployment': {
        'check_every': '800ms', 'time_limit_per_run': '400ms', 'alert_after_failing_for': '0s', 'recovered_after_healthy_for': '30s',
        'shell': 'exit 7', 'alert_severity': 'warn'}})
    vm.write_configuration_files(configuration_directory, config)
    # A dryrun with a changed check group exits 2 and lists the change.
    code, tasks = dryrun()
    assert code == 2, (code, [t for t in tasks if t[1:] != ('succeeded', 'unchanged')])
    changes = {task for task, status, change in tasks if change == 'would_change'}
    assert {'stage_config', 'install_monitoring/changes/install_config'} <= changes, tasks
    assert not [t for t in tasks if t[1] != 'succeeded' and not t[0].startswith('install_monitoring/') or t[2] == 'unknown'], tasks
    assert invocation() == first, 'a dryrun restarted MMWatchdog'
    mark('mmx dryrun with a changed check group exits 2 and lists the change, and changes nothing live')
    apply()
    assert invocation() != first
    assert 'deployment' in ready()['check_groups']
    end = time.monotonic() + 10
    incident = None
    while time.monotonic() < end:
        status = json.loads(cli('status --json').stdout)
        incident = status['checks']['deployment/shell']['failure_incident'].get('incident_id')
        if incident: break
        time.sleep(.2)
    assert incident
    # Only the restarting update opens a window; neither the first install
    # nor an unchanged deployment announces maintenance.
    alerts = wait(lambda: (a := delivered()) and a[-1]['type'] == 'failed' and a, 'the check failure reaches the webhook after the deploy')
    assert [(e['type'], e.get('check_group')) for e in alerts] == [('maintenance_on', 'every_check_group'), ('maintenance_off', 'every_check_group')] + [('failed', 'deployment')], alerts
    assert all(e['maintenance']['reason'] == 'mmwatchdog deploy' for e in alerts[:2]), alerts
    assert 'every_check_group' not in json.loads(cli('status --json').stdout)['maintenance']
    mark('a first install opens no maintenance window; an update deploy, including its daemon restart, runs inside a whole-node window whose start and end alerts carry the deploy reason, ahead of the failure it reveals')
    # A person's own whole-node window stays: the deploy ends only a window
    # that it opened itself.
    def maintenance_events(kind):
        return [e for e in json.loads(cli('events --json').stdout) if e['type'] == kind]
    off_before = len(maintenance_events('maintenance_off'))
    on_before = len(maintenance_events('maintenance_on'))
    persons_command = 'maintenance on --every-check-group --for 1h --reason "planned work"'
    assert cli(persons_command).returncode == 0
    persons_window = json.loads(cli('status --json').stdout)['maintenance']['every_check_group']
    assert persons_window['reason'] == 'planned work', persons_window
    during_planned_work = dict(config, check_groups=dict(config['check_groups'], planned_work_probe=dict(TIMINGS, shell='true', alert_severity='warn')))
    vm.write_configuration_files(configuration_directory, during_planned_work)
    planned = invocation()
    apply(); ready()
    assert invocation() != planned, 'the deploy of a changed configuration did not restart MMWatchdog'
    assert 'planned_work_probe' in ready()['check_groups']
    window_after = json.loads(cli('status --json').stdout)['maintenance'].get('every_check_group')
    assert window_after == persons_window, ('the deploy changed or ended the whole-node window', persons_window, window_after)
    assert len(maintenance_events('maintenance_off')) == off_before, 'the deploy ended the whole-node window'
    assert len(maintenance_events('maintenance_on')) == on_before + 1, 'the deploy opened a window of its own'
    assert cli('maintenance off --every-check-group').returncode == 0
    vm.write_configuration_files(configuration_directory, config)
    apply(); ready()
    assert 'planned_work_probe' not in ready()['check_groups']
    mark('a deploy during a whole-node window that a person opened keeps that window, with its end and reason, and opens none of its own')
    before = invocation()
    def installed_configuration():
        return vm.console('find /etc/mmwatchdog -type f -exec sha256sum {} + | sort').stdout
    installed = installed_configuration()
    (configuration_directory / 'check_groups/node.yml').write_text('check_groups: {invalid: {typo: true}}\n')
    apply(1)
    assert invocation() == before
    assert installed_configuration() == installed
    assert 'every_check_group' not in json.loads(cli('status --json').stdout)['maintenance']
    # Individually valid check-group files must also form a valid complete set.
    vm.write_configuration_files(configuration_directory, config)
    collision = dict(TIMINGS, shell='true', alert_severity='warn')
    (configuration_directory / 'check_groups/conflict.yml').write_text(json.dumps({'check_groups': {'collision': collision}}))
    conflicting = dict(config, check_groups=dict(config['check_groups'], collision=collision))
    vm.write_configuration_files(configuration_directory, conflicting)
    apply(1)
    assert invocation() == before
    assert installed_configuration() == installed
    (configuration_directory / 'check_groups/conflict.yml').unlink()
    vm.write_configuration_files(configuration_directory, config)
    mark('invalid check-group configuration and duplicate candidate check groups are rejected before replacing live configuration; rejected candidates open no maintenance window')
    vm.console('install -d -m 0700 /etc/mmwatchdog/secrets; openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=localhost -keyout /etc/mmwatchdog/secrets/key.pem -out /etc/mmwatchdog/secrets/cert.pem >/dev/null 2>&1; chmod 600 /etc/mmwatchdog/secrets/key.pem')
    vm.write('/etc/mmwatchdog/secrets/backup', 'deployment-heartbeat-token\n', '0600')
    installed = installed_configuration()  # Include the credentials just provisioned.
    receiver = {'listen_on_address': '127.0.0.1:18444', 'tls_certificate_file': '/etc/mmwatchdog/secrets/cert.pem', 'tls_private_key_file': '/etc/mmwatchdog/secrets/key.pem'}
    for token, key in [('/etc/mmwatchdog/secrets/missing', receiver['tls_private_key_file']),
                       ('/etc/mmwatchdog/secrets/backup', '/etc/mmwatchdog/secrets/missing-key')]:
        candidate = dict(config, heartbeat_receiver=dict(receiver, tls_private_key_file=key),
                         check_groups=dict(config['check_groups'], backup={'heartbeat': {'max_time_between_heartbeats': '1h', 'expected_bearer_token_file': token}, 'alert_severity': 'sos'}))
        vm.write_configuration_files(configuration_directory, candidate)
        apply(1)
        assert invocation() == before, 'invalid runtime credentials restarted the daemon'
        assert installed_configuration() == installed
        assert 'deployment' in ready()['check_groups']
        assert json.loads(cli('status --json').stdout)['checks']['deployment/shell']['failure_incident']['incident_id'] == incident
    vm.write_configuration_files(configuration_directory, config)
    installed_alerts = vm.console('cat /etc/mmwatchdog/alert_destinations.yml').stdout
    # remind_every takes any positive interval, such as 30m; 0s is refused.
    def alerts(token, remind_every='30m'):
        (configuration_directory / 'alert_destinations.yml').write_text(json.dumps({'destinations': {'ops': {
            'webhook': {'url': 'http://127.0.0.1:9/mmwatchdog', 'bearer_token_file': token}, 'remind_every': remind_every, 'alert_severities': ['sos']}}}))
    for token, remind_every in [('/etc/mmwatchdog/secrets/missing', '30m'), ('/etc/mmwatchdog/secrets/backup', '0s')]:
        alerts(token, remind_every)
        apply(1)
        assert invocation() == before, 'invalid alert settings restarted the daemon'
        assert vm.console('cat /etc/mmwatchdog/alert_destinations.yml').stdout == installed_alerts
        assert installed_configuration() == installed
    mark('missing heartbeat tokens, TLS keys and alert token files, and a remind_every of 0s, are rejected before replacing configuration or restarting the live daemon')
    alerts('/etc/mmwatchdog/secrets/backup')
    apply()
    assert invocation() != before
    assert ready()['alerts']['destinations'] == {'ops': {'webhook': {'url': 'http://127.0.0.1:9/mmwatchdog', 'bearer_token_file': '/etc/mmwatchdog/secrets/backup'}, 'time_limit_per_delivery_attempt': '5s', 'remind_every': '30m', 'alert_severities': ['sos']}}
    before = invocation()
    vm.console('test -f /etc/mmwatchdog/secrets/backup')
    # A removed source file must not survive in staging or in the live set.
    (configuration_directory / 'check_groups/temporary.yml').write_text(json.dumps({'check_groups': {'temporary': dict(TIMINGS, shell='true', alert_severity='warn')}}))
    apply(); ready()
    (configuration_directory / 'check_groups/temporary.yml').unlink()
    apply(); ready()
    assert 'temporary' not in ready()['check_groups']
    vm.console('test ! -e /etc/mmwatchdog/check_groups/temporary.yml; test -f /etc/mmwatchdog/secrets/backup')
    before = invocation()
    mark('alert settings, with a remind_every of 30m, activate with the complete configuration; removed check groups disappear while secrets remain')
    # The inventory variable selects the node's complete configuration set.
    # Switching back must remove checks that belong only to the second set.
    second_configuration_directory = work / 'files/second_config_set'
    shutil.copytree(configuration_directory, second_configuration_directory)
    (second_configuration_directory / 'check_groups/second.yml').write_text(json.dumps({
        'check_groups': {'second': dict(TIMINGS, shell='true', alert_severity='warn')}}))
    original_check_groups = set(ready()['check_groups'])
    inventory_path = work / 'inventories/hub_and_standalone.yml'
    inventory = json.loads(inventory_path.read_text())
    inventory['vars']['mmwatchdog_config_source_dir'] = 'files/second_config_set'
    inventory_path.write_text(json.dumps(inventory))
    apply()
    assert set(ready()['check_groups']) == original_check_groups | {'second'}
    inventory['vars']['mmwatchdog_config_source_dir'] = 'files/hub_and_standalone'
    inventory_path.write_text(json.dumps(inventory))
    apply()
    assert set(ready()['check_groups']) == original_check_groups
    vm.console('test ! -e /etc/mmwatchdog/check_groups/second.yml; test -f /etc/mmwatchdog/secrets/backup')
    before = invocation()
    mark('the node gets exactly the check groups of mmwatchdog_config_source_dir; switching back removes the extra check group and preserves secrets')
    # Secret files from the MMX vault. The inventory lists the file names,
    # and the vault secret mmwatchdog_secret_files holds their contents.
    assert not (work / 'secrets_vault.age').exists(), 'the deploys so far needed no vault'
    vault['MMX_VAULT_PASSWORD'] = 'deployment-suite-vault'
    editor = root / 'release-vault-editor.sh'
    def secret_files(contents):
        # mmx secrets edit opens the vault in $EDITOR; this editor writes the
        # whole vault, as a person would in a text editor.
        editor.write_text("#!/bin/sh\ncat > \"$1\" <<'VAULT'\n" + json.dumps({'mmwatchdog_secret_files': contents}) + '\nVAULT\n')
        editor.chmod(0o755)
        vm.cmd([mmx, 'secrets', 'edit'], extra_env=dict(vault, EDITOR=str(editor), VISUAL=str(editor)), cwd=work, timeout=60)
    token = '/etc/mmwatchdog/secrets/deploy-webhook-token'
    secret_files({'deploy-webhook-token': 'first-token'})
    inventory['vars']['mmwatchdog_secret_files'] = ['deploy-webhook-token']
    inventory_path.write_text(json.dumps(inventory))
    # Validation reads the token file, so the run must write it first.
    alerts(token)
    apply()
    assert vm.console(f'stat -c "%a %U:%G" {token}').stdout.strip() == '600 root:root'
    assert vm.console(f'cat {token}').stdout == 'first-token\n'
    assert invocation() != before
    before = invocation()
    apply()
    assert invocation() == before, 'an unchanged secret file restarted MMWatchdog'
    secret_files({'deploy-webhook-token': 'second-token'})
    apply()
    assert vm.console(f'cat {token}').stdout == 'second-token\n'
    assert invocation() != before, 'a changed secret file did not restart MMWatchdog'
    alerts('/etc/mmwatchdog/secrets/backup')
    inventory['vars']['mmwatchdog_secret_files'] = []
    inventory_path.write_text(json.dumps(inventory))
    apply()
    before = invocation()
    mark('mmwatchdog_secret_files writes each listed secret file from the vault, mode 0600, before validation; a project without secret files needs no vault; only a changed secret file restarts MMWatchdog')
    # Modules that are not core. Every deploy installs the official extra
    # modules of the release and the project's own modules, each kind in a
    # directory of its own on the node. The official module of these steps is
    # one that the suite adds to the unpacked release, so the steps do not
    # depend on which official extra modules a release holds.
    official_modules = work / 'official_extra_modules'
    modules = work / 'mmwatchdog_external_modules'
    installed_directory = {'official': '/usr/local/lib/mmwatchdog/official_extra_modules', 'own': '/usr/local/lib/mmwatchdog/external_modules'}
    candidate_directory = {'official': '/usr/local/lib/mmwatchdog/candidate_official_extra_modules', 'own': '/usr/local/lib/mmwatchdog/candidate_external_modules'}
    def probe_script(text, measurements=''):
        return 'def run_check(inputs, file_contents):\n    return {"status": "ok", "message": "' + text + '"' + measurements + '}\n'
    def probe_executable(text):
        return '#!/bin/bash\ncat >/dev/null\nprintf \'{"protocol":1,"status":"ok","message":"' + text + '"}\'\n'
    def write_module(directory, name, text, executable=False, measurements=None):
        (directory / name).mkdir(parents=True, exist_ok=True)
        (directory / name / 'module_description.yml').write_text(vm.module_description(name, measurements=measurements))
        if executable:
            # The project holds the executable without the permission to run
            # it, as a checkout on another machine can. The workflow sets it.
            (directory / name / 'module_executable').write_text(probe_executable(text))
            (directory / name / 'module_executable').chmod(0o644)
        else:
            (directory / name / 'module.star').write_text(probe_script(text, ''.join(', "measurements": {"%s": 2}' % measurement for measurement in measurements or {})))
    def modules_on_node(which):
        return vm.console('find ' + installed_directory[which] + " -mindepth 1 -maxdepth 1 -type d -printf '%f\\n' | sort").stdout.split()
    def file_on_node(path):
        return vm.console('stat -c "%a %U:%G" ' + path + ' && cat ' + path, check=False).stdout
    def module_directories():
        """Which directories the node's two module directories are: a replaced one is another."""
        return vm.console('stat -c %i ' + installed_directory['official'] + ' ' + installed_directory['own']).stdout.split()
    def probe(check_group, module):
        return json.loads(cli('check-now ' + check_group + ' --json').stdout)[module]
    # The deploys so far installed every official extra module of the
    # release with the version of the release, as root's files, although no
    # check names one. The project of the release has no module of its own:
    # its module directory holds only the README.
    released = sorted(path.name for path in official_modules.iterdir() if path.is_dir())
    assert {'slack', 'telegram'} <= set(released), released
    assert modules_on_node('official') == released, modules_on_node('official')
    assert file_on_node(installed_directory['official'] + '/shipped_with_mmwatchdog_version') == '644 root:root\n' + VERSION + '\n'
    for name in released:
        for file_name in ['module.star', 'module_description.yml']:
            assert file_on_node(installed_directory['official'] + '/' + name + '/' + file_name) == '644 root:root\n' + (official_modules / name / file_name).read_text(), (name, file_name)
    assert vm.console('ls -A ' + installed_directory['own']).stdout.split() == ['README.md']
    listed = json.loads(cli('describe-module --json').stdout)['modules']
    assert [(m['name'], m['shipped_with_mmwatchdog_version']) for m in listed if m['kind'] == 'official_extra'] == [(name, VERSION) for name in released], listed
    assert not [m for m in listed if m['kind'] not in ('core', 'official_extra')], listed
    # An unchanged deploy leaves the node's module directories as they are.
    unchanged = module_directories()
    apply()
    assert module_directories() == unchanged and invocation() == before
    # A newer release brings a new official module, and the project adds two
    # modules of its own: a script and an executable. Each arrives with the
    # check that names it, in one deploy: validation looks at the modules of
    # this deploy, not at the modules that the node has.
    write_module(official_modules, 'deployment_probe', 'the probe of the release')
    write_module(modules, 'project_probe', 'the probe of the project')
    write_module(modules, 'project_probe_executable', 'the executable probe of the project', executable=True)
    probing = dict(config, check_groups=dict(config['check_groups'],
                                             probe=dict(TIMINGS, deployment_probe={}, alert_severity='warn'),
                                             own=dict(TIMINGS, project_probe={}, alert_severity='warn'),
                                             own_executable=dict(TIMINGS, project_probe_executable={}, alert_severity='warn')))
    vm.write_configuration_files(configuration_directory, probing)
    apply(); ready()
    with_probes = {'official': sorted(released + ['deployment_probe']), 'own': ['project_probe', 'project_probe_executable']}
    assert {which: modules_on_node(which) for which in with_probes} == with_probes
    assert file_on_node(installed_directory['official'] + '/deployment_probe/module.star') == '644 root:root\n' + probe_script('the probe of the release')
    assert file_on_node(installed_directory['own'] + '/project_probe/module.star') == '644 root:root\n' + probe_script('the probe of the project')
    assert file_on_node(installed_directory['own'] + '/project_probe_executable/module_executable') == '755 root:root\n' + probe_executable('the executable probe of the project')
    assert probe('probe', 'deployment_probe')['status'] == 'ok' and probe('probe', 'deployment_probe')['message'] == 'the probe of the release', probe('probe', 'deployment_probe')
    assert probe('own', 'project_probe')['message'] == 'the probe of the project', probe('own', 'project_probe')
    assert probe('own_executable', 'project_probe_executable')['message'] == 'the executable probe of the project', probe('own_executable', 'project_probe_executable')
    kinds = {m['name']: m['kind'] for m in json.loads(cli('module-usage --json').stdout)['modules']}
    assert (kinds['deployment_probe'], kinds['project_probe'], kinds['project_probe_executable']) == ('official_extra', 'project_starlark', 'project_executable'), kinds
    before = invocation()
    # A changed module restarts MMWatchdog, which reads the description of each
    # module when it starts: the official module of a newer release, and the
    # project's own, which now reports a measurement that its new description
    # lists. Nothing but the modules changed.
    write_module(official_modules, 'deployment_probe', 'the second probe of the release')
    write_module(modules, 'project_probe', 'the second probe of the project', measurements={'probes': 'count'})
    apply(); ready()
    assert invocation() != before, 'changed modules did not restart MMWatchdog'
    second = {'probe': 'the second probe of the release', 'own': 'the second probe of the project'}
    assert probe('probe', 'deployment_probe')['message'] == second['probe'], probe('probe', 'deployment_probe')
    assert probe('own', 'project_probe')['message'] == second['own'] and probe('own', 'project_probe')['measurements'] == {'probes': {'value': 2, 'unit': 'count'}}, probe('own', 'project_probe')
    before = invocation()
    apply()
    assert invocation() == before, 'an unchanged deploy after a module change restarted MMWatchdog'
    installed = installed_configuration()
    def refused_before_live_changes(*messages):
        """A deploy that fails with these messages and leaves the node's configuration, modules, and MMWatchdog as they were."""
        on_node, directories = {which: modules_on_node(which) for which in installed_directory}, module_directories()
        refused = apply(1)
        for message in messages:
            assert message in refused.stdout, (message, refused.stdout[-4000:])
        assert invocation() == before and installed_configuration() == installed
        assert {which: modules_on_node(which) for which in installed_directory} == on_node and module_directories() == directories
        assert probe('probe', 'deployment_probe')['message'] == second['probe'] and probe('own', 'project_probe')['message'] == second['own']
    def with_check(name, **check):
        return dict(probing, check_groups=dict(probing['check_groups'], **{name: dict(TIMINGS, alert_severity='warn', **check)}))
    where_mmwatchdog_looked = ('MMWatchdog has no core module of that name, and no directory of that name is in ' + candidate_directory['official'] + ' ('
                       + ', '.join(with_probes['official']) + ') or in ' + candidate_directory['own'] + ' (')
    # A module that neither the release nor the project holds. MMWatchdog says where
    # it looked: in the two directories of this deploy.
    vm.write_configuration_files(configuration_directory, with_check('lost', not_in_the_project={}))
    refused_before_live_changes('unknown field \\"not_in_the_project\\": the field name matches neither a setting of a check nor an installed module. ' + where_mmwatchdog_looked)
    # A configuration that states another module directory than the one in
    # which the workflow installs modules: its node would look for its
    # modules where the workflow puts none.
    for setting, directory in [('external_module_dir', installed_directory['own']), ('official_extra_module_dir', installed_directory['official'])]:
        vm.write_configuration_files(configuration_directory, dict(probing, **{setting: '/usr/local/lib/mmwatchdog/modules_elsewhere'}))
        refused_before_live_changes('mmwatchdog_node_settings.yml states an ' + setting + ' other than ' + directory)
    # A module's directory without the module's code, and one without the
    # module's description, that a check names.
    (modules / 'without_code').mkdir()
    (modules / 'without_code/module_description.yml').write_text(vm.module_description('without_code'))
    vm.write_configuration_files(configuration_directory, with_check('incomplete', without_code={}))
    refused_before_live_changes('the module without_code has no code: ' + candidate_directory['own'] + '/without_code must hold the script module.star or the executable module_executable')
    shutil.rmtree(modules / 'without_code')
    write_module(modules, 'without_description', 'a probe without a description')
    (modules / 'without_description/module_description.yml').unlink()
    vm.write_configuration_files(configuration_directory, with_check('incomplete', without_description={}))
    refused_before_live_changes('the module without_description has no description: ' + candidate_directory['own'] + '/without_description must hold the file module_description.yml')
    shutil.rmtree(modules / 'without_description')
    vm.write_configuration_files(configuration_directory, probing)
    # A module of the project that has the name of an official extra module,
    # or of a core module, also when no check names it.
    for name, taken in [('deployment_probe', 'the official extra module deployment_probe, which every deploy installs'), ('http', 'the core module http')]:
        write_module(modules, name, 'a module of the project under a name that is taken')
        refused_before_live_changes('the module ' + name + ' in ' + candidate_directory['own'] + ' has the name of ' + taken + '. A name means one module on every node: give your module another name')
        shutil.rmtree(modules / name)
    # A module that the project removes while its configuration still names
    # it, although the node still has the module from the last deploy.
    held = work / 'project_probe_held'
    shutil.move(modules / 'project_probe', held)
    refused_before_live_changes('unknown field \\"project_probe\\": the field name matches neither a setting of a check nor an installed module. ')
    shutil.move(held, modules / 'project_probe')
    # Official extra modules of another MMWatchdog release than the mmwatchdog program of
    # the deploy: one release holds both.
    (official_modules / 'shipped_with_mmwatchdog_version').write_text('9.9.9\n')
    refused_before_live_changes('the official extra modules in ' + candidate_directory['official'] + ' were shipped with MMWatchdog 9.9.9, and this mmwatchdog program is MMWatchdog ' + VERSION
                                + '. One MMWatchdog release holds both: deploy that release again')
    (official_modules / 'shipped_with_mmwatchdog_version').write_text(VERSION + '\n')
    apply(); ready()
    assert invocation() == before
    # A deploy that was ended while it replaced a module directory left the
    # node without that directory. The next deploy puts the modules back.
    for which in installed_directory:
        vm.console('mv ' + installed_directory[which] + ' ' + installed_directory[which] + '.mmwatchdog-old')
    apply()
    assert {which: modules_on_node(which) for which in with_probes} == with_probes and invocation() == before
    assert probe('own', 'project_probe')['message'] == second['own'] and probe('probe', 'deployment_probe')['message'] == second['probe']
    for which in installed_directory:
        vm.console('test ! -e ' + installed_directory[which] + '.mmwatchdog-old && test ! -e ' + installed_directory[which] + '.mmwatchdog-new')
    # A node has exactly the modules of the deploy: a module that the project
    # removed and a file that someone left in a module directory go, and an
    # official module that someone edited on the node is the release's again.
    vm.console('install -m 0755 /dev/null ' + installed_directory['own'] + '/left_by_hand; echo "# edited on the node" >> ' + installed_directory['official'] + '/deployment_probe/module.star')
    shutil.rmtree(modules / 'project_probe')
    shutil.rmtree(modules / 'project_probe_executable')
    without_own = dict(config, check_groups=dict(config['check_groups'], probe=probing['check_groups']['probe']))
    vm.write_configuration_files(configuration_directory, without_own)
    apply(); ready()
    assert 'own' not in ready()['check_groups']
    assert modules_on_node('official') == with_probes['official'] and vm.console('ls -A ' + installed_directory['own']).stdout.split() == ['README.md']
    assert file_on_node(installed_directory['official'] + '/deployment_probe/module.star') == '644 root:root\n' + probe_script(second['probe'])
    # A missing source directory must fail before any installed module or
    # the running daemon changes, even when no check needs a module of its own.
    unchanged = module_directories()
    installed_modules = {which: vm.console('find ' + directory + " -type f -exec sha256sum {} + | sort").stdout
                         for which, directory in installed_directory.items()}
    before = invocation()
    saved_modules = work / 'saved_mmwatchdog_external_modules'
    modules.rename(saved_modules)
    refused = apply(1)
    assert 'directory source mmwatchdog_external_modules:' in refused.stdout + refused.stderr, refused.stdout + refused.stderr
    assert module_directories() == unchanged and invocation() == before
    assert {which: vm.console('find ' + directory + " -type f -exec sha256sum {} + | sort").stdout
            for which, directory in installed_directory.items()} == installed_modules
    saved_modules.rename(modules)
    vm.write_configuration_files(configuration_directory, config)
    apply(); ready()
    before = invocation()
    mark('every deploy installs the official extra modules of the release, with the version of the release, and the project\'s own modules from mmwatchdog_external_modules, each kind in its own directory, and an unchanged deploy leaves both directories and MMWatchdog as they are; a new official module, new modules of the project, a script and an executable, and the checks that name them arrive in one deploy, and the workflow makes the executable runnable; a changed module restarts MMWatchdog, which then knows the module\'s new description; a module that neither the release nor the project holds, another module directory in the node settings, a named module without code or without description, a module of the project under the name of an official extra module or of a core module, and official extra modules of another release than the mmwatchdog program each fail the deploy before anything live changes; a node has exactly the modules of the deploy; a missing mmwatchdog_external_modules source directory fails before installed modules or the running daemon change')
    # A unit update and then a binary update must activate even with unchanged config.
    unit = (work / 'mmwatchdog.service').read_text()
    (work / 'mmwatchdog.service').write_text(unit.replace('[Service]', '[Service]\nEnvironment=MMWATCHDOG_DEPLOY_TEST=1'))
    apply(); ready()
    assert invocation() != before
    before = invocation()
    # An update of MMWatchdog brings the mmwatchdog program and the official extra modules
    # of the new release together. The new program alone is refused before
    # anything live changes, because the official extra modules of the
    # project are those of the release before.
    shutil.copyfile(root / ('bin/mmwatchdog-linux-' + vm.ARCH), work / 'mmwatchdog')
    refused = apply(1)
    assert 'were shipped with MMWatchdog ' + VERSION + ', and this mmwatchdog program is MMWatchdog 0.1.0-dev. One MMWatchdog release holds both: deploy that release again' in refused.stdout, refused.stdout[-4000:]
    assert invocation() == before and json.loads(cli('version --json').stdout)['version'] == VERSION
    shutil.copyfile(vm.REPO / 'official_extra_modules/shipped_with_mmwatchdog_version', official_modules / 'shipped_with_mmwatchdog_version')
    apply(); ready()
    assert invocation() != before
    assert json.loads(cli('version --json').stdout)['version'] == '0.1.0-dev'
    assert file_on_node(installed_directory['official'] + '/shipped_with_mmwatchdog_version') == '644 root:root\n0.1.0-dev\n'
    status = json.loads(cli('status --json').stdout)
    assert status['checks']['deployment/shell']['failure_incident']['incident_id'] == incident
    apply()
    current = invocation()
    vm.console('systemctl stop mmwatchdog')
    apply(); ready()
    assert invocation() and invocation() != current
    mark('configuration, unit and binary updates activate; incidents survive updates; unchanged deployment starts a stopped daemon')
    report = {'mmx': provenance, 'apply_seconds': apply_seconds, 'release': metadata, 'installed_version': json.loads(cli('version --json').stdout),
              'platform': vm.console('cat /etc/os-release; uname -m').stdout,
              'time': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())}
    (root / ('release-validation-' + vm.DISTRIBUTION + '-' + vm.ARCH + '.json')).write_text(json.dumps(report, indent=2) + '\n')
