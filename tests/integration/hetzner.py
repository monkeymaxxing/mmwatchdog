#!/usr/bin/env python3
"""Run the VM suites on disposable Hetzner Cloud servers, for several
distributions at once; always delete the servers (Python standard library).

Each job slot gets two new servers of one distribution, mmwatchdog-dev and mmwatchdog-peer,
runs suites on them over SSH as the guests of cloud.py, and deletes them when
the slot is done. The servers are AMD64 (x86) servers, or ARM64 servers with
--server-architecture arm64. Requires MMWATCHDOG_TEST_HETZNER_API_TOKEN (a
Hetzner Cloud API token) and MMWATCHDOG_TEST_HETZNER_SSH_KEY_NAME (an SSH key
registered in that Cloud project). The harness logs in as root with a key
that it makes for the run and deletes with the servers, so it runs also
where there is no ~/.ssh/id_rsa, such as on a test server; every create
request still names the registered key, so that Hetzner makes no root
password. The ownership manifest supports cleanup even after this process
dies.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
import importlib
import ipaddress
import json
from pathlib import Path
import signal
import subprocess
import threading
import time
import traceback
import urllib.request
import uuid

import cloud
import testenv

# all.py's name would hide Python's all() here.
suite_runner = importlib.import_module('all')

ROOT = testenv.root()
# Two CPUs and 4 GiB, the size of the Incus guest: the cheaper type first,
# the other when Hetzner has sold out of it. Hetzner's ARM64 servers have
# one such type.
SERVER_TYPES = {'amd64': ('cx23', 'cpx22'), 'arm64': ('cax11',)}
# The distributions Hetzner has an image of (cloud.py); all.py's other target
# distributions run only on Incus.
HETZNER_DISTRIBUTIONS = tuple(cloud.IMAGES_FOR_SERVER_ARCHITECTURE['amd64'])
LOCATIONS = cloud.LOCATIONS
# Default limit for one run; --max-servers-at-once overrides it.
DEFAULT_MAX_SERVERS_AT_ONCE = 6
UNAVAILABLE_CODES = {'resource_unavailable', 'placement_error'}
MARKER = 'mmwatchdog-disposable-integration-v1'


def owned(run_name):
    found = cloud.api('GET', '/servers?label_selector=project=mmwatchdog,run=' + run_name + '&per_page=50')
    return found['servers'] if found else []


def owned_firewalls(run_name):
    found = cloud.api('GET', '/firewalls?label_selector=project=mmwatchdog,run=' + run_name + '&per_page=50')
    return found['firewalls'] if found else []


def save(path, data):
    path.write_text(json.dumps(data, indent=2) + '\n')
    path.chmod(0o600)


def delete_and_wait(kind, items):
    """Delete servers or firewalls, and wait until they are gone."""
    for item in items:
        for attempt in range(30):
            try:
                cloud.api('DELETE', f'/{kind}/{item["id"]}')
                break
            except RuntimeError as error:
                # A firewall stays in use for a moment after its servers are gone.
                if 'resource_in_use' not in str(error) or attempt == 29:
                    raise
                time.sleep(2)
    deadline = time.monotonic() + 180
    while any(cloud.api('GET', f'/{kind}/{item["id"]}') for item in items):
        if time.monotonic() > deadline:
            raise RuntimeError(f'Hetzner {kind} were not deleted: ' + ', '.join(i['name'] for i in items))
        time.sleep(2)


def cleanup(path):
    """Delete a run's servers and firewalls, found by the run's label, and
    check that none is left; then delete the run's login key."""
    manifest = json.loads(path.read_text())
    if manifest.get('project') != 'mmwatchdog':
        raise RuntimeError('the manifest is not one of MMWatchdog\'s test runs')
    # Query by the run's label as well: a creation may have succeeded before
    # its response arrived.
    delete_and_wait('servers', owned(manifest['run']))
    delete_and_wait('firewalls', owned_firewalls(manifest['run']))
    if owned(manifest['run']) or owned_firewalls(manifest['run']):
        raise RuntimeError(f'Hetzner servers left; run hetzner.py --cleanup-from-manifest {path}')
    manifest['cleanup_verified'] = True
    save(path, manifest)
    # The login key opened only these servers.
    for key in ('login-key', 'login-key.pub'):
        (path.parent / key).unlink(missing_ok=True)
    print('Cleanup verified: no servers or firewalls of this run remain', flush=True)


class Run:
    """One invocation: its ownership manifest, and the servers it holds."""

    def __init__(self, max_servers_at_once, server_architecture):
        self.ssh_key_id = cloud.registered_ssh_key_id()
        self.server_architecture = server_architecture
        self.name = 'mmwatchdog-' + time.strftime('%Y%m%d-%H%M%S') + '-' + uuid.uuid4().hex[:6]
        self.labels = {'project': 'mmwatchdog', 'run': self.name}
        self.directory = ROOT / 'cloud-runs' / self.name
        self.directory.mkdir(parents=True, mode=0o700)
        self.manifest_path = self.directory / 'ownership.json'
        save(self.manifest_path, {'project': 'mmwatchdog', 'run': self.name})
        print('Ownership manifest: ' + str(self.manifest_path), flush=True)
        self.login_key = self.directory / 'login-key'
        subprocess.run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-C', 'mmwatchdog-test ' + self.name,
                        '-f', str(self.login_key)], check=True)
        with urllib.request.urlopen('https://api.ipify.org', timeout=30) as response:
            self.controller_address = str(ipaddress.IPv4Address(response.read().decode().strip()))
        # A pair takes two of the run's servers; slots wait for room.
        self.pair_places = threading.Semaphore(max(1, max_servers_at_once // 2))
        self.stopping = threading.Event()
        self.built_once = suite_runner.build_mmwatchdog_and_mmx_once(ROOT)


def cloud_config(host_key, role, login_key):
    """The cloud-init settings that make a new server like an Incus guest."""
    return {
        # root gets the run's login key beside the registered key.
        'disable_root': False,
        'ssh_authorized_keys': [Path(str(login_key) + '.pub').read_text().strip()],
        # The guest's role is its hostname, as for the Incus guests: MMWatchdog puts
        # a node's name in each alert title, which the server's long name
        # would shorten.
        'hostname': role,
        'ssh_pwauth': False,
        # The harness names the main guest in each guest's /etc/hosts, and a
        # reboot must not write that file anew.
        'manage_etc_hosts': False,
        'ssh_keys': {'ed25519_private': host_key.read_text(),
                     'ed25519_public': host_key.with_suffix('.pub').read_text().strip()},
        'packages': ['python3'],
        'write_files': [{'path': '/etc/mmwatchdog-test-vm', 'content': MARKER + '\n', 'permissions': '0644'}],
        'runcmd': [
            # A server that upgrades itself holds its package manager while a
            # scenario needs it.
            'for unit in apt-daily.timer apt-daily-upgrade.timer unattended-upgrades.service; '
            'do systemctl disable --now $unit 2>/dev/null; done; true',
        ],
    }


def create_pair(run, distribution, slot):
    """Create the slot's firewall and two servers; return the remote.json
    content. Delete what was created when a step fails."""
    name = f'{run.name}-{distribution}-{slot}'
    directory = run.directory / f'{distribution}-{slot}'
    directory.mkdir(mode=0o700, exist_ok=True)
    host_key = directory / 'host-key'
    subprocess.run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(host_key)], check=True)
    known = directory / 'known_hosts'
    known.write_text('')
    known.chmod(0o600)
    firewall = cloud.api('POST', '/firewalls', {'name': name, 'labels': run.labels,
                                                'rules': cloud.controller_rules(run.controller_address)})['firewall']
    # vm.py checks the architecture on each server, and every program that a
    # suite builds for the servers is built for it.
    remote = {'name': name, 'arch': run.server_architecture, 'distribution': distribution}
    user_data = {role: '#cloud-config\n' + json.dumps(cloud_config(host_key, role, run.login_key))
                 for role in ('mmwatchdog-dev', 'mmwatchdog-peer')}
    host_key.unlink()
    server_types = SERVER_TYPES[run.server_architecture]
    image = cloud.IMAGES_FOR_SERVER_ARCHITECTURE[run.server_architecture][distribution]
    for role, field in (('mmwatchdog-dev', 'node'), ('mmwatchdog-peer', 'peer_node')):
        for server_type, location in [(t, place) for t in server_types for place in LOCATIONS]:
            try:
                created = cloud.api('POST', '/servers', {
                    'name': f'{name}-{role.removeprefix("mmwatchdog-")}', 'server_type': server_type, 'location': location,
                    'image': image, 'labels': run.labels, 'user_data': user_data[role],
                    # The registered key keeps Hetzner from making a root password.
                    'ssh_keys': [run.ssh_key_id], 'firewalls': [{'firewall': firewall['id']}],
                    'public_net': {'enable_ipv4': True, 'enable_ipv6': False}})
                break
            except RuntimeError as error:
                if not any(code in str(error) for code in UNAVAILABLE_CODES):
                    raise
        else:
            raise RuntimeError(f'no {" or ".join(server_types)} available in ' + ', '.join(LOCATIONS))
        address = created['server']['public_net']['ipv4']['ip']
        with known.open('a') as hosts:
            hosts.write(address + ' ' + host_key.with_suffix('.pub').read_text())
        remote[field] = {'address': address, 'ssh_port': 22, 'ssh_user': 'root',
                         'ssh_identity_file': str(run.login_key), 'ssh_known_hosts': str(known),
                         'ssh_config': '/dev/null'}
    # The two servers reach each other on every TCP port; nobody else
    # reaches more than the controller rules allow.
    cloud.api('POST', f'/firewalls/{firewall["id"]}/actions/set_rules', {'rules': [
        *cloud.controller_rules(run.controller_address),
        {'direction': 'in', 'protocol': 'tcp', 'port': '1-65535',
         'source_ips': [remote['node']['address'] + '/32', remote['peer_node']['address'] + '/32'],
         'description': 'MMWatchdog test guests reach each other'}]})
    for field in ('node', 'peer_node'):
        wait_until_ready(run, remote[field])
    return remote


def wait_until_ready(run, node):
    """Wait until cloud-init has finished on a new server."""
    ssh = ['ssh', '-F', '/dev/null', '-o', 'BatchMode=yes', '-o', 'IdentitiesOnly=yes', '-o', 'StrictHostKeyChecking=yes',
           '-o', 'ConnectTimeout=5', '-o', 'UserKnownHostsFile=' + node['ssh_known_hosts'], '-i', node['ssh_identity_file'],
           'root@' + node['address']]
    deadline = time.monotonic() + 900
    while True:
        if run.stopping.is_set():
            raise RuntimeError('the run is stopping')
        result = subprocess.run([*ssh, 'cloud-init status --wait'], text=True, capture_output=True, timeout=900)
        # 2: cloud-init finished with a recoverable error, which its log has.
        if result.returncode in (0, 2):
            break
        # 255: SSH cannot connect yet.
        if result.returncode != 255:
            raise RuntimeError(f'cloud-init on {node["address"]} exited {result.returncode}: {result.stdout[-1000:]}')
        if time.monotonic() > deadline:
            raise RuntimeError(f'the server {node["address"]} did not become ready')
        time.sleep(3)


@contextmanager
def server_pair(run, distribution, slot, pending):
    """all.py's guests: two new servers for one job slot, deleted when the
    slot is done. Yield None when the suites ran out while the slot waited
    for room, or when the servers failed; the other slots then run them."""
    placed = False
    try:
        while not (placed := run.pair_places.acquire(timeout=5)):
            if pending.empty() or run.stopping.is_set():
                break
        if not placed:
            yield None
            return
        started = time.monotonic()
        remote = None
        try:
            remote = create_pair(run, distribution, slot)
            run.built_once.wait()
            with suite_runner.OUTPUT_LOCK:
                print(f'{distribution}: slot {slot} servers ready in {time.monotonic() - started:.0f}s', flush=True)
        except Exception as error:
            with suite_runner.OUTPUT_LOCK:
                print(f'{distribution}: slot {slot} has no servers: {error}', flush=True)
        yield remote
    finally:
        if placed:
            name = f'{run.name}-{distribution}-{slot}'
            delete_and_wait('servers', [s for s in owned(run.name) if s['name'].startswith(name + '-')])
            delete_and_wait('firewalls', [f for f in owned_firewalls(run.name) if f['name'] == name])
            run.pair_places.release()


def run_queue(run, distribution, jobs, suites):
    """Run one distribution's suites; return all.py's status."""
    label = distribution + ': '
    try:
        return suite_runner.run_suites(
            ROOT / distribution, jobs, suites=suites, label=label,
            guests=lambda slot, pending: server_pair(run, distribution, slot, pending))
    except Exception:
        with suite_runner.OUTPUT_LOCK:
            print(f'{label}runner failed\n{traceback.format_exc()}', flush=True)
        return 1


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    # Hetzner has no image of Arch Linux or openSUSE Leap, which only incus.py tests.
    parser.add_argument('--distribution', action='append', choices=HETZNER_DISTRIBUTIONS,
                        help='repeat for each distribution to test; default: all of them')
    parser.add_argument('--server-architecture', choices=sorted(SERVER_TYPES), default='amd64',
                        help="the test servers' processor architecture: amd64 (Hetzner's "
                             f"{', '.join(SERVER_TYPES['amd64'])}) or arm64 (Hetzner's "
                             f"{', '.join(SERVER_TYPES['arm64'])}); default amd64")
    parser.add_argument('--suite', action='append', choices=[name for name, _ in suite_runner.SUITES],
                        help='repeat to run only these suites; default: all of them')
    parser.add_argument('--pairs-per-distribution', type=int, default=2,
                        help='job slots per distribution, each with its two servers; default 2')
    parser.add_argument('--max-servers-at-once', type=int, default=DEFAULT_MAX_SERVERS_AT_ONCE,
                        help=f'the most servers this run holds at once; default {DEFAULT_MAX_SERVERS_AT_ONCE}; '
                             'choose a value that fits the available Hetzner project quota')
    parser.add_argument('--cleanup-from-manifest', type=Path, metavar='OWNERSHIP_JSON',
                        help="delete a run's servers again, from its ownership.json")
    args = parser.parse_args()
    if min(args.pairs_per_distribution, args.max_servers_at_once) < 1:
        parser.error('pairs per distribution and servers at once must be positive')
    distributions = args.distribution or list(HETZNER_DISTRIBUTIONS)
    with testenv.locked(ROOT):
        if args.cleanup_from_manifest:
            cleanup(args.cleanup_from_manifest.resolve())
            return 0
        suites = tuple(s for s in suite_runner.SUITES if not args.suite or s[0] in args.suite)
        suites_of_each_distribution = suite_runner.suites_by_distribution(suites, distributions)
        run = Run(args.max_servers_at_once, args.server_architecture)

        def interrupted(signum, frame):
            raise KeyboardInterrupt
        for sig in (signal.SIGTERM, signal.SIGINT):
            signal.signal(sig, interrupted)
        started = time.monotonic()
        pool = ThreadPoolExecutor(max_workers=len(suites_of_each_distribution))
        try:
            futures = [(d, pool.submit(run_queue, run, d, args.pairs_per_distribution, chosen))
                       for d, chosen in suites_of_each_distribution.items()]
            failed = sorted({d for d, future in futures if future.result()})
        except BaseException:
            run.stopping.set()
            raise
        finally:
            # A second Ctrl-C or SIGTERM must not cut the deletion short.
            for sig in (signal.SIGTERM, signal.SIGINT):
                signal.signal(sig, signal.SIG_IGN)
            pool.shutdown(wait=True)
            cleanup(run.manifest_path)
        print(f'Hetzner run {run.name} on {run.server_architecture.upper()} servers: '
              f'{len(suites_of_each_distribution) - len(failed)} of {len(suites_of_each_distribution)} distributions passed'
              + (', failed: ' + ', '.join(failed) if failed else '') + f'; {time.monotonic() - started:.0f}s',
              flush=True)
        return int(bool(failed))


if __name__ == '__main__':
    raise SystemExit(main())
