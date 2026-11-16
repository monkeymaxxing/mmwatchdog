#!/usr/bin/env python3
"""Create/remove one explicitly requested, MMWatchdog-owned test VM of
--distribution (Debian 13 by default), and with --with-peer a second one for
the fleet workflow's joined node. The VMs are of the Hetzner server type
--server-type, and the suites run natively on its processor architecture:
AMD64 (such as cx23) or ARM64 (such as cax11)."""
import argparse
import ipaddress
import json
import os
from pathlib import Path
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
import testenv

ROOT = testenv.root()
STATE = ROOT / 'remote.json'
SSH_IDENTITY = Path.home() / '.ssh/id_rsa'
# Hetzner's image of each of all.py's TARGET_DISTRIBUTIONS for AMD64 (x86)
# servers, and for ARM64 servers. The server type's architecture chooses
# between them.
IMAGES_FOR_SERVER_ARCHITECTURE = {
    'amd64': {
        'debian12': 114690387, 'debian13': 310554929,
        'ubuntu2404': 161547269, 'ubuntu2604': 387894169,
    },
    'arm64': {
        'debian12': 114690389, 'debian13': 310557660,
        'ubuntu2404': 161547270, 'ubuntu2604': 387894171,
    },
}
# How Hetzner names a server type's processor architecture, and how Go
# names it.
GO_ARCHITECTURE_OF_HETZNER_ARCHITECTURE = {'x86': 'amd64', 'arm': 'arm64'}
# Helsinki first; the others when it has no room.
LOCATIONS = ('hel1', 'fsn1', 'nbg1')


def api(method, path, data=None):
    token = os.environ.get('MMWATCHDOG_TEST_HETZNER_API_TOKEN')
    if not token:
        raise RuntimeError('MMWATCHDOG_TEST_HETZNER_API_TOKEN is not set; set it to a Hetzner Cloud API token with read and write access to the test project')
    request = urllib.request.Request('https://api.hetzner.cloud/v1' + path,
        data=None if data is None else json.dumps(data).encode(), method=method,
        headers={'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json'})
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            body = response.read()
            return json.loads(body) if body else {}
    except urllib.error.HTTPError as error:
        if method in {'GET', 'DELETE'} and error.code == 404:
            return None
        # Never print a creation response: it may contain an initial password.
        detail = json.loads(error.read()).get('error', {}).get('code', '')
        raise RuntimeError(f'Hetzner {method} {path}: HTTP {error.code} {detail}') from None


def controller_rules(controller_address):
    """The firewall rules that let the controller machine reach a test VM:
    SSH, and MMX's HTTPS API on port 7443. MMX deploys over HTTPS where it
    can and takes SSH only after an HTTPS connection fails: a firewall that
    dropped port 7443 would make MMX wait for a time-out at every fleet
    stage, 13 to 30 seconds for each deploy instead of a few."""
    return [{'direction': 'in', 'protocol': 'tcp', 'port': port, 'source_ips': [controller_address + '/32'],
             'description': 'MMWatchdog test controller ' + what} for port, what in (('22', 'SSH'), ('7443', 'MMX HTTPS'))]


def save(state):
    temporary = STATE.with_suffix('.tmp')
    temporary.write_text(json.dumps(state, indent=2) + '\n')
    temporary.chmod(0o600)
    temporary.replace(STATE)


def registered_ssh_key_id():
    """The ID of the SSH key that MMWATCHDOG_TEST_HETZNER_SSH_KEY_NAME names
    in the Hetzner Cloud project. Every create request names it, so that
    Hetzner makes no root password. Where ~/.ssh/id_rsa.pub exists, it must
    be that key."""
    key_name = os.environ.get('MMWATCHDOG_TEST_HETZNER_SSH_KEY_NAME')
    if not key_name:
        raise RuntimeError('MMWATCHDOG_TEST_HETZNER_SSH_KEY_NAME is not set; set it to the name of an SSH key registered in the Hetzner Cloud test project')
    keys = [key for key in api('GET', '/ssh_keys?name=' + urllib.parse.quote(key_name))['ssh_keys'] if key['name'] == key_name]
    if len(keys) != 1:
        raise RuntimeError('MMWATCHDOG_TEST_HETZNER_SSH_KEY_NAME must name exactly one registered SSH key in the Hetzner Cloud test project')
    public = SSH_IDENTITY.with_suffix('.pub')
    if public.exists() and keys[0]['public_key'].split()[:2] != public.read_text().split()[:2]:
        raise RuntimeError('the SSH key that MMWATCHDOG_TEST_HETZNER_SSH_KEY_NAME names must match ~/.ssh/id_rsa.pub on this machine')
    return keys[0]['id']


def architecture_and_location(server_type):
    """The processor architecture of a Hetzner server type, as Go names it,
    and the first of LOCATIONS where Hetzner has that type now."""
    types = [t for t in api('GET', '/server_types?name=' + urllib.parse.quote(server_type))['server_types'] if t['name'] == server_type]
    if len(types) != 1:
        raise RuntimeError('Hetzner has no server type ' + server_type)
    available = {place['name'] for place in types[0]['locations'] if place['available']}
    location = next((place for place in LOCATIONS if place in available), None)
    if location is None:
        raise RuntimeError(f'Hetzner has no {server_type} available in ' + ', '.join(LOCATIONS))
    return GO_ARCHITECTURE_OF_HETZNER_ARCHITECTURE[types[0]['architecture']], location


def up(purpose='standalone-test', server_type='cx23', with_peer=False, distribution='debian13'):
    if STATE.exists():
        raise RuntimeError('remote.json already exists; inspect or remove the owned VM first')
    # The VMs' login is this machine's own SSH identity.
    if not SSH_IDENTITY.is_file() or not SSH_IDENTITY.with_suffix('.pub').is_file():
        raise RuntimeError('missing established SSH identity: ' + str(SSH_IDENTITY))
    registered_key = registered_ssh_key_id()
    # vm.py checks the architecture on each VM, and every program that a
    # suite builds for the VMs is built for it.
    architecture, location = architecture_and_location(server_type)
    name = 'mmwatchdog-test-' + uuid.uuid4().hex[:10]
    labels = {'project': 'mmwatchdog', 'purpose': purpose, 'owner': name}
    credentials = ROOT / 'credentials'
    credentials.mkdir(mode=0o700, exist_ok=True)
    key = SSH_IDENTITY
    host_key = credentials / 'host'
    if not host_key.exists():
        subprocess.run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(host_key)], check=True)
    with urllib.request.urlopen('https://api.ipify.org', timeout=30) as response:
        address = str(ipaddress.IPv4Address(response.read().decode().strip()))
    state = {'name': name, 'labels': labels, 'created_at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()), 'arch': architecture,
             'distribution': distribution}
    save(state)
    try:
        firewall = api('POST', '/firewalls', {'name': name, 'labels': labels, 'rules': controller_rules(address)})
        state['firewall_id'] = firewall['firewall']['id']; save(state)
        # Pre-generated host key lets the first SSH connection verify identity.
        userdata = '#cloud-config\n' + json.dumps({
            'ssh_pwauth': False,
            # The harness names the main guest in each guest's /etc/hosts,
            # and a reboot must not write that file anew.
            'manage_etc_hosts': False,
            'ssh_keys': {'ed25519_private': host_key.read_text(), 'ed25519_public': host_key.with_suffix('.pub').read_text().strip()},
            'write_files': [{'path': '/etc/mmwatchdog-test-vm', 'content': 'mmwatchdog-disposable-integration-v1\n', 'permissions': '0644'}],
        })
        known = credentials / 'known_hosts'
        known.write_text('')
        known.chmod(0o600)

        def create(server_name, id_field, node_field):
            result = api('POST', '/servers', {'name': server_name, 'server_type': server_type,
                'image': IMAGES_FOR_SERVER_ARCHITECTURE[architecture][distribution],
                'location': location, 'labels': labels, 'user_data': userdata,
                'ssh_keys': [registered_key],
                'firewalls': [{'firewall': state['firewall_id']}],
                'public_net': {'enable_ipv4': True, 'enable_ipv6': False}})
            server = result['server']
            state[id_field] = server['id']; save(state)
            ip = server['public_net']['ipv4']['ip']
            with known.open('a') as hosts:
                hosts.write(ip + ' ' + host_key.with_suffix('.pub').read_text())
            state[node_field] = {'address': ip, 'ssh_port': 22, 'ssh_user': 'root', 'ssh_identity_file': str(key),
                                 'ssh_known_hosts': str(known), 'ssh_config': '/dev/null'}
            save(state)

        create(name, 'server_id', 'node')
        created = {'name': name, 'server_id': state['server_id'], 'arch': architecture}
        if with_peer:
            # The fleet workflow's joined node. The two guests reach each
            # other on every TCP port; nobody else reaches more than the
            # controller rules allow.
            state['peer_name'] = name + '-peer'; save(state)
            create(state['peer_name'], 'peer_server_id', 'peer_node')
            created['peer_server_id'] = state['peer_server_id']
            api('POST', f'/firewalls/{state["firewall_id"]}/actions/set_rules', {'rules': [
                *controller_rules(address),
                {'direction': 'in', 'protocol': 'tcp', 'port': '1-65535',
                 'source_ips': [state['node']['address'] + '/32', state['peer_node']['address'] + '/32'], 'description': 'MMWatchdog test guests reach each other'}]})
        print(json.dumps(created), flush=True)
    except BaseException:
        down()
        raise


def down():
    if not STATE.exists():
        return
    state = json.loads(STATE.read_text())
    for kind, field, singular, name in [('servers', 'peer_server_id', 'server', state.get('peer_name')), ('servers', 'server_id', 'server', state['name']),
                                        ('firewalls', 'firewall_id', 'firewall', state['name'])]:
        if field not in state: continue
        path = f'/{kind}/{state[field]}'
        current = api('GET', path)
        if current is not None:
            if current[singular]['labels'] != state['labels'] or current[singular]['name'] != name:
                raise RuntimeError('cloud ownership does not match the recorded MMWatchdog lease')
            for attempt in range(30):
                try:
                    api('DELETE', path)
                    break
                except RuntimeError as error:
                    if 'resource_in_use' not in str(error) or attempt == 29: raise
                    time.sleep(2)
            for attempt in range(60):
                if api('GET', path) is None: break
                time.sleep(1)
            else: raise RuntimeError('cloud deletion has not completed: ' + path)
    state['deleted_at'] = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())
    (ROOT / 'cloud-cleanup.json').write_text(json.dumps(state, indent=2) + '\n')
    testenv.forget_guests(ROOT)
    STATE.unlink()
    print('MMWatchdog cloud servers and firewall deletion verified', flush=True)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['up', 'down'])
    parser.add_argument('--purpose', default='standalone-test', help='up: the VM\'s purpose label')
    parser.add_argument('--server-type', default='cx23',
                        help='up: the Hetzner server type of the VMs, which also decides their processor architecture: '
                             'cx23 (AMD64) or cax11 (ARM64), each with 2 CPUs and 4 GiB; default cx23')
    parser.add_argument('--with-peer', action='store_true', help='up: also create the second VM that the fleet workflow joins to the first')
    parser.add_argument('--distribution', choices=list(IMAGES_FOR_SERVER_ARCHITECTURE['amd64']), default='debian13',
                        help='up: the distribution of the VMs; default debian13')
    args = parser.parse_args()
    with testenv.locked(ROOT):
        up(args.purpose, args.server_type, args.with_peer, args.distribution) if args.command == 'up' else down()
