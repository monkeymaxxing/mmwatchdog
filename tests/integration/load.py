#!/usr/bin/env python3
"""The large-fleet load run: one hub, and a generator guest whose synthetic
nodes join it and report on the fleet schedule. It records report latency,
the hub's CPU, memory and database size, and fleet-command latency. It checks
reporting failures, daemon restarts, and that a group going silent or failing
the same check yields a few rolled-up messages.

With --local it uses mmwatchdog-dev as hub and mmwatchdog-peer as generator, the two guests
of the test environment that cloud.py up --with-peer created, for a short
smoke run. Otherwise `up` creates two cloud VMs in this test root, `run` uses
them, and `down` deletes them.
"""
import argparse
import json
import os
from pathlib import Path
import shlex
import subprocess
import threading
import time
import testenv

REPO = Path(__file__).resolve().parents[2]
ROOT = testenv.root()
PORT = 8445


def ssh_options(node):
    return ['-F', '/dev/null', '-o', 'BatchMode=yes', '-o', 'IdentitiesOnly=yes',
            '-o', 'StrictHostKeyChecking=yes', '-o', 'ConnectTimeout=10',
            '-o', 'UserKnownHostsFile=' + node['ssh_known_hosts'], '-i', node['ssh_identity_file'],
            '-o', 'ControlMaster=auto', '-o', 'ControlPath=' + str(ROOT / 'ssh-%C'), '-o', 'ControlPersist=60']


def ssh(node, script, check=True, timeout=300):
    if node['ssh_user'] != 'root':
        script = 'sudo -n /bin/sh -c ' + shlex.quote(script)
    p = subprocess.run(['ssh', *ssh_options(node), '-p', str(node['ssh_port']), node['ssh_user'] + '@' + node['address'], script],
                       text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
    if check and p.returncode:
        raise RuntimeError(f'{node["address"]}: {script}\n{p.stdout}\n{p.stderr}')
    return p


def install(node, source, target, mode='0755'):
    temporary = '/tmp/' + Path(target).name + '.upload'
    subprocess.run(['scp', *ssh_options(node), '-P', str(node['ssh_port']), str(source),
                    node['ssh_user'] + '@' + node['address'] + ':' + temporary], check=True, capture_output=True)
    ssh(node, f'install -D -m {mode} {temporary} {target} && rm {temporary}')


def write(node, target, content, mode='0644'):
    source = ROOT / ('upload-' + Path(target).name)
    source.write_text(content)
    install(node, source, target, mode)
    source.unlink()


def build(hub_arch, generator_arch):
    """Build mmwatchdog for the hub's processor architecture and the load generator
    for the generator's."""
    out = {}
    for name, package, arch in [('mmwatchdog', './cmd/mmwatchdog', hub_arch), ('mmwatchdog-load', './tests/load', generator_arch)]:
        out[name] = ROOT / 'bin' / f'{name}-linux-{arch}'
        subprocess.run(['go', 'build', '-trimpath', '-ldflags=-s -w', '-o', str(out[name]), package], cwd=REPO, check=True,
                       env=dict(os.environ, CGO_ENABLED='0', GOOS='linux', GOARCH=arch))
    return out


def setup_hub(hub, address, every, binary, memory_max):
    """A fresh hub with a local webhook receiver, and MMWatchdog limited to memory_max if set."""
    ssh(hub, 'for unit in mmwatchdog mmwatchdog-fixture; do systemctl stop $unit 2>/dev/null; done; rm -rf /var/lib/mmwatchdog /etc/mmwatchdog /var/tmp/mmwatchdog-fixture; '
             'mkdir -p /var/tmp/mmwatchdog-fixture; id mmwatchdog >/dev/null 2>&1 || useradd --system --home-dir /var/lib/mmwatchdog-checks mmwatchdog; true')
    install(hub, binary, '/usr/local/bin/mmwatchdog')
    write(hub, '/var/tmp/mmwatchdog-fixture/server.py', (REPO / 'tests/integration/fixture.py').read_text())
    write(hub, '/etc/systemd/system/mmwatchdog-fixture.service',
          '[Service]\nExecStart=/usr/bin/python3 /var/tmp/mmwatchdog-fixture/server.py\n')
    write(hub, '/etc/systemd/system/mmwatchdog.service', (REPO / 'examples/mmwatchdog.service').read_text())
    write(hub, '/etc/systemd/system/mmwatchdog.service.d/load.conf', f'[Service]\nMemoryMax={memory_max or "infinity"}\n')
    write(hub, '/etc/mmwatchdog/mmwatchdog_node_settings.yml', json.dumps({'keep_undelivered_alerts_for': '24h', 'undelivered_alerts_content_size_limit': '8MiB', 'fleet_hub_role': {'alert_after_hub_unreachable_for': '3m', 'send_alert_without_hub_after_waiting_for': '30s', 'undelivered_alerts_of_nodes_content_size_limit': '32MiB', 'listen_on_address': '0.0.0.0:8445', 'address_for_nodes': address, 'report_every': every,
                                                                              'keep_measurement_history_of_nodes_for': '7d', 'measurement_history_of_nodes_size_limit': '20GiB'},
                                                           'alert_severity_when_this_node_goes_silent': 'sos', 'alert_severity_when_this_node_has_rebooted': 'info',
                                                           'keep_measurement_history_for': '7d', 'measurement_history_size_limit': '256MiB'}))
    write(hub, '/etc/mmwatchdog/check_groups/node.yml', json.dumps({'check_groups': {'cpu': {'only_record_readings': True,
        'check_every': '30s', 'time_limit_per_run': '5s', 'alert_after_failing_for': '1m', 'recovered_after_healthy_for': '30s', 'node_resource': 'cpu'}}}))
    write(hub, '/etc/mmwatchdog/alert_destinations.yml', json.dumps({'destinations': {'capture': {'remind_every': 'never', 'webhook': {'url': 'http://127.0.0.1:18080/events'}, 'time_limit_per_delivery_attempt': '5s', 'alert_severities': ['info', 'warn', 'sos']}}}))
    ssh(hub, 'systemctl daemon-reload && systemctl start mmwatchdog-fixture mmwatchdog')
    for _ in range(60):
        p = ssh(hub, '/usr/local/bin/mmwatchdog fleet join-code', check=False)
        if p.returncode == 0:
            return p.stdout.strip()
        time.sleep(1)
    raise RuntimeError('the hub did not start: ' + ssh(hub, 'journalctl -u mmwatchdog -n 20 --no-pager').stdout)


SAMPLE = r'''pid=$(systemctl show -p MainPID --value mmwatchdog)
echo "$(date +%s.%N) $(awk '{print $14+$15}' /proc/$pid/stat) $(awk '/VmRSS|VmHWM/{print $2}' /proc/$pid/status | tr '\n' ' ')$(systemctl show -p MemoryCurrent --value mmwatchdog) $(for f in fleet.db fleet.db-wal state.db state.db-wal; do stat -c %s /var/lib/mmwatchdog/$f 2>/dev/null || echo 0; done | tr '\n' ' ')"'''
TIMED = r'''s=$(date +%s%N); /usr/local/bin/mmwatchdog {0} >/dev/null; e=$(date +%s%N); echo $(( (e-s)/1000000 ))'''


def sample(hub, samples, commands, stop):
    """Hub resources every 10 seconds, and fleet-command latency every 30."""
    last = 0
    while not stop.wait(10):
        fields = ssh(hub, SAMPLE, check=False).stdout.split()
        if len(fields) >= 7:
            at, ticks, hwm, rss, cgroup, *sizes = fields
            samples.append({'at': float(at), 'cpu_ticks': int(ticks), 'rss_kib': int(rss), 'hwm_kib': int(hwm),
                            'cgroup_bytes': int(cgroup), 'fleet_db': sum(map(int, sizes[:2])), 'state_db': sum(map(int, sizes[2:]))})
        if time.monotonic() - last >= 30:
            last = time.monotonic()
            row = {'at': time.time()}
            for name, args in [('fleet_json', 'fleet --json'), ('fleet', 'fleet'), ('status_json', 'status --json')]:
                p = ssh(hub, TIMED.format(args), check=False)
                row[name + '_ms'] = int(p.stdout.strip()) if p.stdout.strip().isdigit() else None
            commands.append(row)


def alert_groups(delivered, group):
    """The messages for each group change, and how many nodes they cover."""
    out = {}
    for label, match in [('silence', lambda e: e.get('check_group') == 'node_reports'),
                         ('fail', lambda e: e.get('check_group') == 'app' and e.get('check') == 'http')]:
        for kind in ['failed', 'recovered']:
            messages = [e for e in delivered if match(e) and e['type'] == kind]
            covered = sum(e.get('node_count') or 1 for e in messages)
            out[label + '_' + kind] = {'messages': len(messages), 'nodes': covered, 'expected': group,
                                       'texts': [e['message'][:160] for e in messages]}
    return out


def assert_load_result(result):
    """Check the recorded load-run results."""
    assert int(result['hub']['restarts']) == 0, ('unexpected hub daemon restart', result['hub']['restarts'])
    most = result['most_failed_reports_in_a_row_of_one_node']
    assert most < 2, ('sustained reporting failure', result['node_with_most_failed_reports_in_a_row'],
                      most, 'failed report attempts in a row')
    # A rejected alert also explains missing grouped messages; name it first.
    for line in result['phases']:
        errors = line.get('alerts', {}).get('errors')
        assert not errors, ('failed alert posts', line['phase'], errors)
    for key, row in result['alert_groups'].items():
        assert row['nodes'] == row['expected'] and row['messages'] <= 5, (key, row)


def run(hub, generator, address, hub_arch, generator_arch, args):
    binaries = build(hub_arch, generator_arch)
    code = setup_hub(hub, address, args.every, binaries['mmwatchdog'], args.memory_max)
    install(generator, binaries['mmwatchdog-load'], '/usr/local/bin/mmwatchdog-load')
    # Every report is a new connection from one address; widen the port range
    # its TIME_WAIT sockets use.
    ssh(generator, 'sysctl -q -w net.ipv4.ip_local_port_range="1024 65000"')
    flags = ['-code', code, '-nodes', args.nodes, '-every', args.every, '-silence', args.group, '-fail', args.group,
             '-steady', args.steady, '-silence-for', args.silence_for, '-fail-for', args.fail_for, '-settle', args.settle]
    samples, commands, lines, stop = [], [], [], threading.Event()
    sampler = threading.Thread(target=sample, args=(hub, samples, commands, stop))
    sampler.start()
    command = 'exec /usr/local/bin/mmwatchdog-load ' + shlex.join(str(f) for f in flags)
    if generator['ssh_user'] != 'root':
        command = 'sudo -n /bin/sh -c ' + shlex.quote(command)
    log = ROOT / 'load-generator.jsonl'
    started = time.time()
    with log.open('w') as out:
        process = subprocess.Popen(['ssh', *ssh_options(generator), '-p', str(generator['ssh_port']),
                                    generator['ssh_user'] + '@' + generator['address'], command],
                                   text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        for line in process.stdout:
            out.write(line)
            out.flush()
            print(line.strip()[:300], flush=True)
            try:
                lines.append(json.loads(line))
            except json.JSONDecodeError:
                pass
        code = process.wait()
    # Hold the silence alerts the generator's exit would bring.
    ssh(hub, '/usr/local/bin/mmwatchdog maintenance on node_reports --for 1h --reason load-run-end')
    stop.set()
    sampler.join()
    if code:
        raise RuntimeError('the generator failed; see ' + str(log))
    # A combined message goes out when its minute ends.
    for _ in range(40):
        delivered = [json.loads(line)['event'] for line in
                     ssh(hub, 'cat /var/tmp/mmwatchdog-fixture/webhooks.jsonl', check=False).stdout.splitlines() if line]
        if all(r['nodes'] >= r['expected'] for r in alert_groups(delivered, args.group).values()):
            break
        time.sleep(3)
    # Its exit code is the worst node's, and the generator's nodes have stopped.
    fleet = json.loads(ssh(hub, '/usr/local/bin/mmwatchdog fleet --json', check=False).stdout)
    # The end line counts the alerts still in flight when the phases ended.
    phases = [l for l in lines if l.get('ended') or l.get('phase') == 'join' and 'ok' in l or l.get('phase') == 'end']
    # The final summary includes reports still in flight when the phases ended.
    reporting = next(l for l in lines if l.get('phase') == 'end')
    cpu = []
    for a, b in zip(samples, samples[1:]):
        cpu.append({'at': b['at'], 'cpu_percent': round((b['cpu_ticks'] - a['cpu_ticks']) / (b['at'] - a['at']), 1)})
    steady = [c['cpu_percent'] for c in cpu if phase_at(lines, c['at']) == 'steady']
    result = {
        'nodes': args.nodes, 'report_every': args.every, 'group': args.group, 'started': started,
        'hub': {'address': address, 'memory_max': args.memory_max, 'cpus': int(ssh(hub, 'nproc').stdout),
                'memory_total_mib': int(ssh(hub, "awk '/MemTotal/{print int($2/1024)}' /proc/meminfo").stdout),
                'cpu_steady_percent_median': sorted(steady)[len(steady) // 2] if steady else None,
                'cpu_percent_max': max((c['cpu_percent'] for c in cpu), default=None),
                'rss_mib_max': max((s['rss_kib'] for s in samples), default=0) / 1024,
                'hwm_mib': max((s['hwm_kib'] for s in samples), default=0) / 1024,
                'cgroup_mib_max': max((s['cgroup_bytes'] for s in samples), default=0) / 2**20,
                'fleet_db_mib': samples[-1]['fleet_db'] / 2**20 if samples else None,
                'state_db_mib': samples[-1]['state_db'] / 2**20 if samples else None,
                'restarts': ssh(hub, 'systemctl show -p NRestarts --value mmwatchdog').stdout.strip()},
        'most_failed_reports_in_a_row_of_one_node': reporting['most_failed_reports_in_a_row_of_one_node'],
        'node_with_most_failed_reports_in_a_row': reporting['node_with_most_failed_reports_in_a_row'],
        'fleet_commands_ms': {k: summarize([c[k] for c in commands if c.get(k) is not None])
                              for k in ['fleet_json_ms', 'fleet_ms', 'status_json_ms']},
        'phases': phases,
        'fleet_nodes': len(fleet['nodes']),
        'alert_groups': alert_groups(delivered, args.group),
        'samples': samples, 'cpu': cpu, 'commands': commands,
    }
    (ROOT / 'load-result.json').write_text(json.dumps(result, indent=2) + '\n')
    brief = {k: v for k, v in result.items() if k not in ('samples', 'cpu', 'commands')}
    print(json.dumps(brief, indent=2))
    assert_load_result(result)
    print('Load run passed; result: ' + str(ROOT / 'load-result.json'), flush=True)


def phase_at(lines, at):
    """The generator phase at a wall-clock time."""
    current = None
    for l in lines:
        if 'started' in l and l['started'] <= at:
            current = l['phase']
    return current


def summarize(values):
    values = sorted(values)
    if not values:
        return None
    return {'n': len(values), 'median': values[len(values) // 2], 'max': values[-1]}


def cloud_node(role):
    return json.loads((ROOT / role / 'remote.json').read_text())


def cloud(command, role, *options):
    env = dict(os.environ, MMWATCHDOG_TEST_ROOT=str(ROOT / role))
    env.pop(testenv.LOCK_FD, None)
    subprocess.run(['python3', str(REPO / 'tests/integration/cloud.py'), command, *options], env=env, check=True)


def wait_ready(node):
    for _ in range(90):
        if ssh(node, 'cat /etc/mmwatchdog-test-vm', check=False).returncode == 0:
            break
        time.sleep(2)
    else:
        raise RuntimeError(node['address'] + ' did not become ready')
    ssh(node, 'cloud-init status --wait >/dev/null; true', timeout=600)


def up(hub_server_type):
    for role, server_type in [('hub', hub_server_type), ('generator', 'cx23')]:
        if not (ROOT / role / 'remote.json').exists():
            cloud('up', role, '--purpose', 'load-' + role, '--server-type', server_type)
    import cloud as hetzner
    hub, generator = cloud_node('hub'), cloud_node('generator')
    firewall = hetzner.api('GET', f'/firewalls/{hub["firewall_id"]}')['firewall']
    rules = [r for r in firewall['rules'] if r.get('port') != str(PORT)]
    rules.append({'direction': 'in', 'protocol': 'tcp', 'port': str(PORT),
                  'source_ips': [generator['node']['address'] + '/32'], 'description': 'MMWatchdog load generator reports'})
    hetzner.api('POST', f'/firewalls/{hub["firewall_id"]}/actions/set_rules', {'rules': rules})
    for state in [hub, generator]:
        wait_ready(state['node'])
    print('Load hub ' + hub['node']['address'] + ' and generator ' + generator['node']['address'] + ' ready', flush=True)


if __name__ == '__main__':
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('command', choices=['up', 'run', 'down'])
    p.add_argument('--hub-server-type', default='cx23', help='up: the hub\'s Hetzner server type')
    p.add_argument('--memory-max', default='2G', help='run: the hub\'s systemd MemoryMax; empty for none')
    p.add_argument('--local', action='store_true', help='run: use mmwatchdog-dev and mmwatchdog-peer')
    p.add_argument('--nodes', type=int, default=10000)
    p.add_argument('--group', type=int, default=1000, help='nodes that go silent, and that fail the same check')
    p.add_argument('--every', default='30s', help="the hub's report_every")
    p.add_argument('--steady', default='5m')
    p.add_argument('--silence-for', default='3m')
    p.add_argument('--fail-for', default='2m')
    p.add_argument('--settle', default='2m')
    args = p.parse_args()
    with testenv.locked(ROOT):
        if args.command == 'up':
            up(args.hub_server_type)
        elif args.command == 'down':
            for role in ['generator', 'hub']:
                if (ROOT / role / 'remote.json').exists():
                    cloud('down', role)
        elif args.local:
            import vm
            vm.up(vm.NAME)
            vm.up(vm.PEER)
            run(vm.connection(vm.NAME), vm.connection(vm.PEER), f'{vm.MAIN_GUEST_HOST}:{PORT}', vm.ARCH, vm.ARCH, args)
        else:
            hub, generator = cloud_node('hub'), cloud_node('generator')
            run(hub['node'], generator['node'], f'{hub["node"]["address"]}:{PORT}', hub['arch'], generator['arch'], args)
