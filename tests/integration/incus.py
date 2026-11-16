#!/usr/bin/env python3
"""Run the VM suites on Incus VMs on this Linux machine, for every target
distribution at once (Python standard library).

Every guest is a copy of a prepared base of its distribution, made on first
use from an image copied once into Incus's local store; a run never asks the
image server. Each job slot gets two new copies, mmwatchdog-dev and mmwatchdog-peer, runs
suites on them over SSH as the cloud guests of cloud.py, and deletes them
when the slot is done. The ownership manifest supports cleanup even after
this process dies.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
import fcntl
import json
import os
from pathlib import Path
import random
import re
import shlex
import signal
import subprocess
import threading
import time
import traceback
import uuid

import testenv

# all.py's name would hide Python's all() here.
import importlib
suite_runner = importlib.import_module('all')

ROOT = testenv.root()
# The Incus VM image of each of all.py's TARGET_DISTRIBUTIONS: the cloud
# variant, which, like a cloud server, has cloud-init.
IMAGES = {
    'debian12': 'debian/12/cloud', 'debian13': 'debian/13/cloud',
    'ubuntu2404': 'ubuntu/noble/cloud', 'ubuntu2604': 'ubuntu/resolute/cloud',
    # Arch Linux is the current rolling release; openSUSE Leap is the newest
    # release that Incus's image server has. The names are MMX's.
    'archlinux': 'archlinux/cloud', 'opensuseleap161': 'opensuse/16.1/cloud',
    # The Red Hat family: Rocky Linux 9 and AlmaLinux 10, with SELinux enforcing.
    'rocky9': 'rockylinux/9/cloud', 'almalinux10': 'almalinux/10/cloud',
    # Fedora, whose dnf is dnf 5.
    'fedora45': 'fedora/45/cloud',
}
# The package manager of each distribution's image; it picks the guest's
# preparation and package servers.
PACKAGE_MANAGERS = {
    'debian12': 'apt', 'debian13': 'apt', 'ubuntu2404': 'apt', 'ubuntu2604': 'apt',
    'archlinux': 'pacman', 'opensuseleap161': 'zypper', 'rocky9': 'dnf', 'almalinux10': 'dnf',
    'fedora45': 'dnf',
}
MARKER = 'mmwatchdog-disposable-integration-v1'
# The guests are VMs, never containers: the suites read what the guest's own
# kernel says, such as its I/O wait, its memory pressure, its disk device,
# its out-of-memory kills, and its clock, and they load kernel modules for
# ZFS, RAID, and NFS. A container shares this machine's kernel.
#
# The size of the cloud guest (cloud.py): the scenarios of the checks of
# common services install database servers and download the images of MySQL
# and MongoDB. The fleet workflow's joined node is smaller.
# Writes do not wait for this machine's disk (io.cache=unsafe): MMWatchdog commits
# every observation, and a package manager asks for the same after every
# file; waiting for the disk while other guests write made checks miss their
# turns. A guest is thrown away after its run, so a damaged disk after a
# power loss of this machine costs nothing.
MAIN_SIZE = {'limits.cpu': '2', 'limits.memory': '4GiB'}
PEER_SIZE = {'limits.cpu': '1', 'limits.memory': '1GiB'}
ROOT_DISK = ['-d', 'root,size=40GiB', '-d', 'root,io.cache=unsafe']
# A guest pair holds this many of the shared guest slots: an MMWatchdog pair holds
# 5 GiB of this machine's memory while it runs.
SHARED_SLOTS_PER_PAIR = 2

# dnf (Rocky Linux and AlmaLinux): each repository of the distribution's
# files names its servers with a mirrorlist, and its main server in a
# commented-out baseurl. Such a repository gets a baseurl with the main
# server and then Hetzner's copy, in place of the mirrorlist; dnf takes a
# file from the next URL when one fails. Everything else, such as enabled,
# gpgcheck, and gpgkey, stays. Hetzner's copies carry the advisories
# (updateinfo) that dnf_security_updates needs. A repository whose
# commented-out baseurl names another server, such as AlmaLinux's debug and
# source repositories on vault.almalinux.org, keeps its mirrorlist.
# The names and the approach are MMX's.
DNF_PACKAGE_SERVERS = {
    # The start of the commented-out baseurl, and what replaces it, in order.
    'rocky9': ('http://dl.rockylinux.org/$contentdir/',
               ('https://dl.rockylinux.org/$contentdir/', 'https://mirror.hetzner.com/rockylinux/')),
    'almalinux10': ('https://repo.almalinux.org/almalinux/',
                    ('https://repo.almalinux.org/almalinux/', 'https://mirror.hetzner.com/almalinux/')),
    # Fedora's repositories name their servers with a metalink, and their
    # main server in a commented-out baseurl on the placeholder host
    # download.example. Hetzner's copy has no "linux" directory.
    'fedora45': ('http://download.example/pub/fedora/linux/',
                 ('https://dl.fedoraproject.org/pub/fedora/linux/', 'https://mirror.hetzner.com/fedora/')),
}
PREPARE = {
    'apt': 'export DEBIAN_FRONTEND=noninteractive; apt-get update -q && apt-get install -yq openssh-server python3',
    # Arch Linux is a rolling release: the guest's package databases and
    # packages must come from one moment, so a base upgrades all of its
    # packages (pacman -Syu) and never installs from a database alone.
    'pacman': 'pacman -Syu --noconfirm --needed openssh python && systemctl enable sshd',
    'zypper': 'zypper --non-interactive refresh && zypper --non-interactive install openssh python3 && systemctl enable sshd',
    # dnf leaves a package mirror that stalls after 10 seconds, so that the
    # mirror does not hold up the guest's preparation. tar: the Red Hat
    # family's images have none, and the suites unpack archives with it.
    # dnf-plugins-core: needs-restarting, for reboot_after_updates. The images
    # replace sshd_config with one line, which drops its Include of
    # sshd_config.d and with it the distribution's UsePAM yes; a reinstall
    # restores the package's file.
    'dnf': "grep -q '^minrate=' /etc/dnf/dnf.conf || printf 'minrate=200k\\ntimeout=10\\n' >> /etc/dnf/dnf.conf; "
           'dnf install -yq openssh-server python3 tar && systemctl enable sshd && '
           'if rpm -V openssh-server | grep -q "/etc/ssh/sshd_config$"; then '
           'rm -f /etc/ssh/sshd_config && dnf reinstall -yq openssh-server; fi',
}
# With SELinux enforcing, the Incus agent in a VM, which the harness uses to
# prepare each copy, runs as init_t from its configuration disk and may not
# open its vsock socket or make its socket in /dev/incus.
INCUS_AGENT_POLICY = ('(allow init_t self (vsock_socket (create bind listen accept connect getattr setattr '
                      'getopt setopt read write shutdown)))\n'
                      '(allow init_t device_t (dir (search write add_name remove_name)))\n'
                      '(allow init_t device_t (sock_file (create getattr setattr write unlink)))\n')
# pacman (Arch Linux): /etc/pacman.d/mirrorlist names the servers in the
# order pacman tries them, and pacman takes a file from the next server when
# one fails.
PACMAN_MIRRORLIST = ('Server = https://geo.mirror.pkgbuild.com/$repo/os/$arch\n'
                     'Server = http://mirror.hetzner.com/archlinux/$repo/os/$arch\n')
# A guest that upgrades itself holds its package manager while a scenario
# needs it, and changes the packages that a scenario counts.
# (Arch Linux and openSUSE Leap upgrade nothing by themselves.)
NO_AUTOMATIC_UPGRADES = ('for unit in apt-daily.timer apt-daily-upgrade.timer unattended-upgrades.service dnf-makecache.timer; '
                         'do systemctl disable --now $unit 2>/dev/null; done; true')
FAILED_UNITS = 'systemctl --failed --no-legend; systemctl list-jobs --no-legend'


def incus(*args, check=True, timeout=600):
    # incus launch and incus init read an instance configuration from a
    # standard input that is not a terminal, and wait for its end: from a
    # pipe that stays open, such as an SSH session's, they wait forever.
    p = subprocess.run(['incus', *map(str, args)], text=True, timeout=timeout, stdin=subprocess.DEVNULL,
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if check and p.returncode:
        raise RuntimeError(f'incus {" ".join(map(str, args))}: {(p.stderr or p.stdout).strip()}')
    return p


def inside(name, script, check=True, timeout=1800):
    return incus('exec', name, '--', '/bin/sh', '-c', script, check=check, timeout=timeout)


def wait_running(name, seconds=300):
    """Wait until systemd in the guest has finished starting; fail unless every
    unit started."""
    deadline = time.monotonic() + seconds
    while True:
        # Right after the start, the guest's agent does not answer yet, or
        # systemd reports "offline".
        state = inside(name, 'systemctl is-system-running --wait', check=False).stdout.strip()
        if state not in ('', 'offline') or time.monotonic() > deadline:
            break
        time.sleep(0.5)
    if state != 'running':
        failed = inside(name, FAILED_UNITS, check=False).stdout.strip()
        raise RuntimeError(f'{name}: systemd is {state or "not answering"}: {failed}')


def dnf_repositories_with_package_servers(text, main_server, servers):
    """The text of a dnf .repo file in which each repository whose
    commented-out baseurl starts with main_server gets a baseurl with the
    servers in order, in place of its mirrorlist or metalink; and how many
    repositories changed."""
    sections = re.split(r'(?m)^(?=\[)', text)
    changed = 0
    for index, section in enumerate(sections):
        lines = section.splitlines(keepends=True)
        commented = [line for line in lines if re.match(r'#\s*baseurl=' + re.escape(main_server), line)]
        if len(commented) != 1:
            continue
        path = re.sub(r'#\s*baseurl=' + re.escape(main_server), '', commented[0]).strip()
        baseurl = 'baseurl=' + ' '.join(server + path for server in servers) + '\n'
        sections[index] = ''.join(baseurl if line is commented[0] else line for line in lines
                                  if not line.startswith(('mirrorlist=', 'metalink=')))
        changed += 1
    return ''.join(sections), changed


def set_dnf_package_servers(name, distribution):
    """Give a new base of the Red Hat family its ordered package servers
    (DNF_PACKAGE_SERVERS), and fetch dnf's metadata from them, which shows
    at once that the servers answer."""
    main_server, servers = DNF_PACKAGE_SERVERS[distribution]
    changed = 0
    # Fedora (dnf 5) keeps its repository files in /usr/share/dnf5/repos.d;
    # a copy in /etc/yum.repos.d replaces the one of the same name.
    inside(name, 'if [ -d /usr/share/dnf5/repos.d ]; then cp -n /usr/share/dnf5/repos.d/*.repo /etc/yum.repos.d/; fi')
    for path in inside(name, 'ls -1 /etc/yum.repos.d/*.repo').stdout.split():
        text, changed_in_the_file = dnf_repositories_with_package_servers(inside(name, 'cat ' + shlex.quote(path)).stdout, main_server, servers)
        if changed_in_the_file:
            inside(name, f'printf %s {shlex.quote(text)} > {shlex.quote(path)}')
            changed += changed_in_the_file
    if not changed:
        raise RuntimeError(f'{name}: no repository of the image names {main_server} in a commented-out baseurl')
    fetched = inside(name, 'dnf -q makecache', check=False, timeout=600)
    if fetched.returncode:
        raise RuntimeError(f'{name}: dnf could not fetch its metadata: {(fetched.stdout + fetched.stderr).strip()}')


def selinux_enforcing(name):
    """The Red Hat family's VM images have SELinux off and unlabeled files;
    a default installation enforces it. The Incus agent needs a policy of its
    own for that (INCUS_AGENT_POLICY)."""
    inside(name, 'dnf install -yq selinux-policy-targeted policycoreutils >/dev/null; '
           f"printf '%s' '{INCUS_AGENT_POLICY}' > /root/mmwatchdog_test_policy.cil && semodule -i /root/mmwatchdog_test_policy.cil; "
           "sed -i 's/^SELINUX=.*/SELINUX=permissive/' /etc/selinux/config; touch /.autorelabel")
    # One boot in permissive mode labels the files and boots again; booting
    # enforcing on unlabeled files leaves systemd in kernel_t.
    incus('restart', name)
    wait_running(name, seconds=600)
    inside(name, "sed -i 's/^SELINUX=.*/SELINUX=enforcing/' /etc/selinux/config")
    incus('restart', name)
    wait_running(name)
    if inside(name, 'getenforce').stdout.strip() != 'Enforcing':
        raise RuntimeError(name + ': SELinux is not enforcing')


def image_alias(distribution):
    return 'mmwatchdog-test-image-vm-' + distribution


def base_name(distribution):
    return 'mmwatchdog-test-base-vm-' + distribution


def local_image(distribution):
    """Copy the distribution's image into Incus's local store once, so that
    making a base never depends on the image server."""
    alias = image_alias(distribution)
    if incus('image', 'info', alias, check=False).returncode != 0:
        incus('image', 'copy', 'images:' + IMAGES[distribution], 'local:', '--alias', alias, '--vm', timeout=1800)
    return alias


def prepare_base(distribution):
    """Make the stopped base that each job slot's guests are copied from."""
    name = base_name(distribution)
    started = time.monotonic()
    new = name + '-new'
    incus('delete', '--force', new, check=False)
    alias = local_image(distribution)
    # Secure Boot is off, as on a cloud server.
    size = [item for key, value in MAIN_SIZE.items() for item in ('-c', f'{key}={value}')]
    # (Incus refuses to start the Arch Linux image with Secure Boot on.)
    incus('init', alias, new, '--vm', *size, *ROOT_DISK, '-c', 'security.secureboot=false')
    # The VM images take the agent's configuration from this disk.
    incus('config', 'device', 'add', new, 'agent', 'disk', 'source=agent:config')
    incus('start', new)
    try:
        # A first boot does more, and the bases are often made together.
        wait_running(new, seconds=900)
        manager = PACKAGE_MANAGERS[distribution]
        if manager == 'dnf':
            set_dnf_package_servers(new, distribution)
        if manager == 'pacman':
            inside(new, "printf '%s' '" + PACMAN_MIRRORLIST + "' > /etc/pacman.d/mirrorlist")
        inside(new, PREPARE[manager], timeout=1800)
        inside(new, NO_AUTOMATIC_UPGRADES)
        # The harness logs in as root with each run's key, as on a cloud
        # server; the drop-in comes first, so its setting wins.
        inside(new, "printf 'PermitRootLogin prohibit-password\\n' > /etc/ssh/sshd_config.d/00-mmwatchdog-test.conf; "
               f'printf "%s\\n" {MARKER} > /etc/mmwatchdog-test-vm; '
               # cloud-init has done its one job; later boots skip it.
               'if [ -d /etc/cloud ]; then touch /etc/cloud/cloud-init.disabled; fi')
        if manager == 'dnf':
            selinux_enforcing(new)
        # A copy's agent would rewrite hostname, hosts, network files, and
        # cloud-init's seed from the image's templates on its first boot,
        # which SELinux forbids it; each copy sets its hostname instead.
        metadata = json.loads(incus('query', f'/1.0/instances/{new}/metadata').stdout)
        metadata['templates'] = {}
        incus('query', '-X', 'PUT', f'/1.0/instances/{new}/metadata', '--data', json.dumps(metadata))
        # Each copy makes its own machine ID on its first boot, as a server
        # from a cloud image does; the DHCP client ID, of Debian for one,
        # and so the address, comes from it. Last, because every boot writes one.
        inside(new, ': > /etc/machine-id')
        incus('stop', new)
        incus('delete', '--force', name, check=False)
        incus('move', new, name)
    except BaseException:
        incus('delete', '--force', new, check=False)
        raise
    print(f'{distribution}: base prepared in {time.monotonic() - started:.0f}s', flush=True)
    return name


class SharedGuestSlots:
    """Lock files that every test run on this machine shares, in a directory
    that the machine's administrator fills with files named slot-*. A guest
    pair holds some of them while it runs, so that the runs of several
    projects together never start more guests than the machine has room for.
    The kernel gives a lock back when its process ends, even by a kill.

    While several runs want slots, each takes no more than its fair share,
    the slots divided by those runs; a run alone takes them all. Without the
    share, the run that came first kept every slot that it gave back, and a
    run that came later waited until the first had ended. A run that holds
    or wants slots holds a lock on its own file in runs/, which is how the
    others count it."""

    def __init__(self, directory):
        self.directory = Path(directory)
        self.files = sorted(Path(directory).glob('slot-*'))
        if not self.files:
            raise RuntimeError(f'{directory} has no files named slot-*')
        self.runs = Path(directory) / 'runs'
        self.runs.mkdir(exist_ok=True)
        self.own_marker_path = self.runs / f'{os.getpid()}-{uuid.uuid4().hex[:8]}'
        self.own_marker = None
        self.lock = threading.Lock()
        self.held = 0
        self.wanting = 0

    def runs_that_want_slots(self):
        """This run and the others that hold or want slots now."""
        count = 1
        for path in self.runs.iterdir():
            if path == self.own_marker_path:
                continue
            try:
                with path.open() as marker:
                    fcntl.flock(marker, fcntl.LOCK_SH | fcntl.LOCK_NB)
            except BlockingIOError:
                count += 1
            except FileNotFoundError:
                pass
        return count

    def mark_or_unmark(self):
        """Hold this run's file in runs/ exactly while it holds or wants slots."""
        if (self.held or self.wanting) and self.own_marker is None:
            self.own_marker = self.own_marker_path.open('a')
            fcntl.flock(self.own_marker, fcntl.LOCK_EX)
        elif not (self.held or self.wanting) and self.own_marker is not None:
            self.own_marker.close()
            self.own_marker = None
            self.own_marker_path.unlink(missing_ok=True)

    def acquire(self, count, holder, give_up):
        """Hold count slots; return their open files, or None once give_up()
        is true. A pair that cannot get all of its slots holds none, so that
        two runs never wait for each other's half."""
        with self.lock:
            self.wanting += 1
            self.mark_or_unmark()
        try:
            while True:
                share = len(self.files) // self.runs_that_want_slots()
                with self.lock:
                    if self.held + count <= max(share, count):
                        held = self.take(count, holder)
                        if held:
                            self.held += count
                            return held
                if give_up():
                    return None
                # Runs that wait try again at random times, so that no run
                # keeps every slot that another run gives back.
                time.sleep(random.uniform(1, 3))
        finally:
            with self.lock:
                self.wanting -= 1
                self.mark_or_unmark()

    def take(self, count, holder):
        held = []
        for path in random.sample(self.files, len(self.files)):
            handle = path.open('r+')
            try:
                fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                handle.close()
                continue
            held.append(handle)
            if len(held) == count:
                for handle in held:
                    handle.truncate(0)
                    handle.write(holder + '\n')
                    handle.flush()
                return held
        for handle in held:
            handle.close()
        return None

    def release(self, held):
        for handle in held:
            handle.truncate(0)
            handle.close()
        with self.lock:
            self.held -= len(held)
            self.mark_or_unmark()


class Run:
    """One invocation: its ownership manifest, login key, bases, and the
    guest pairs that it runs at once on this machine."""

    def __init__(self, pairs_at_once, shared_slots):
        self.name = 'mmwatchdog-' + time.strftime('%Y%m%d-%H%M%S') + '-' + uuid.uuid4().hex[:6]
        self.directory = ROOT / 'incus-runs' / self.name
        self.directory.mkdir(parents=True, mode=0o700)
        self.manifest_path = self.directory / 'ownership.json'
        save(self.manifest_path, {'run': self.name})
        print('Ownership manifest: ' + str(self.manifest_path), flush=True)
        # A key for this run's guests only; cleanup deletes it with them.
        self.login_key = self.directory / 'login-key'
        subprocess.run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-C', 'mmwatchdog-test ' + self.name,
                        '-f', str(self.login_key)], check=True)
        self.lock = threading.Lock()
        self.base_locks = {}
        self.base_failures = {}
        self.stopping = threading.Event()
        self.pair_places = threading.Semaphore(pairs_at_once)
        self.shared_slots = shared_slots
        self.built_once = suite_runner.build_mmwatchdog_and_mmx_once(ROOT)

    def base(self, distribution):
        """The base of this distribution, prepared once also when several
        slots ask at the same time."""
        with self.lock:
            guard = self.base_locks.setdefault(distribution, threading.Lock())
        with guard:
            # A base that failed once fails the run's other slots with the
            # same error, instead of each trying again.
            if distribution in self.base_failures:
                raise RuntimeError(f'the base could not be prepared: {self.base_failures[distribution]}')
            name = base_name(distribution)
            with lock_of_base(self.shared_slots, name):
                if incus('info', name, check=False).returncode != 0:
                    try:
                        prepare_base(distribution)
                    except Exception as error:
                        self.base_failures[distribution] = error
                        raise
            return name


@contextmanager
def lock_of_base(shared_slots, name):
    """The lock that every run on this machine holds while it looks for a
    base and prepares a missing one. Two runs at the same time can both find
    a base missing; without the lock both would prepare it, and the second
    would delete the base that the first made while the first copies guests
    from it. The lock file is in the directory of the shared guest slots,
    which every run on a test server uses, or else in the test environment."""
    directory = (shared_slots.directory if shared_slots else ROOT) / 'bases'
    directory.mkdir(parents=True, exist_ok=True)
    with (directory / (name + '.lock')).open('a') as handle:
        fcntl.flock(handle, fcntl.LOCK_EX)
        yield


def save(path, data):
    path.write_text(json.dumps(data, indent=2) + '\n')
    path.chmod(0o600)


def owned(run_name):
    listing = incus('list', 'user.mmwatchdog-test-run=' + run_name, '--format', 'json').stdout
    return [instance['name'] for instance in json.loads(listing)]


def cleanup(path):
    """Delete a run's guests, check that none is left, and delete its login key."""
    manifest = json.loads(path.read_text())
    for name in owned(manifest['run']):
        incus('delete', '--force', name, check=False)
    left = owned(manifest['run'])
    if left:
        raise RuntimeError(f'Incus guests left: {", ".join(left)}; run incus.py --cleanup-from-manifest {path}')
    manifest['cleanup_verified'] = True
    save(path, manifest)
    for key in ('login-key', 'login-key.pub'):
        (path.parent / key).unlink(missing_ok=True)
    print('Cleanup verified: no guests of this run remain', flush=True)


def start_guest(run, distribution, slot, role, directory):
    """Copy the base into a new guest of this run, start it, and give it its
    own hostname, SSH host key, and the run's login key. Return its node
    entry for remote.json."""
    name = f'{run.name}-{distribution}-{slot}-{role.removeprefix("mmwatchdog-")}'
    size = MAIN_SIZE if role == 'mmwatchdog-dev' else PEER_SIZE
    incus('copy', run.base(distribution), name, '-c', 'user.mmwatchdog-test-run=' + run.name,
          *[item for key, value in size.items() for item in ('-c', f'{key}={value}')])
    try:
        incus('start', name)
        wait_running(name)
        key = Path(str(run.login_key) + '.pub').read_text().strip()
        # The guest's role is its hostname: MMWatchdog puts a node's name in each
        # alert title, which a long name would shorten.
        inside(name, f'hostnamectl set-hostname {role}; '
               'rm -f /etc/ssh/ssh_host_*; ssh-keygen -A >/dev/null; '
               'install -d -m 700 /root/.ssh; '
               f'printf "%s\\n" "{key}" > /root/.ssh/authorized_keys; chmod 600 /root/.ssh/authorized_keys; '
               '(systemctl restart ssh 2>/dev/null || systemctl restart sshd)')
        deadline = time.monotonic() + 60
        while True:
            state = json.loads(incus('query', f'/1.0/instances/{name}/state').stdout)
            # A VM's interface is named for its slot, such as enp5s0.
            address = next((a['address'] for interface, details in (state.get('network') or {}).items()
                            if interface != 'lo' for a in details['addresses'] if a['family'] == 'inet'), None)
            if address:
                break
            if time.monotonic() > deadline:
                raise RuntimeError(name + ' has no IPv4 address')
            time.sleep(0.5)
        host_key = ' '.join(inside(name, 'cat /etc/ssh/ssh_host_ed25519_key.pub').stdout.split()[:2])
    except BaseException:
        incus('delete', '--force', name, check=False)
        raise
    known = directory / (role + '.known_hosts')
    known.write_text(f'{address} {host_key}\n')
    known.chmod(0o600)
    return name, {'address': address, 'ssh_port': 22, 'ssh_user': 'root', 'ssh_identity_file': str(run.login_key),
                  'ssh_known_hosts': str(known), 'ssh_config': '/dev/null'}


@contextmanager
def guest_pair(run, distribution, slot, pending):
    """all.py's guests: two new guests for one job slot, deleted when the slot
    is done. Yield None when the guests could not start, or when the suites
    ran out while the slot waited for room; the other slots then run them."""
    names = []
    placed = False
    shared = None
    try:
        def give_up():
            return pending.empty() or run.stopping.is_set()
        while not (placed := run.pair_places.acquire(timeout=5)):
            if give_up():
                break
        if placed and run.shared_slots:
            shared = run.shared_slots.acquire(SHARED_SLOTS_PER_PAIR, f'{run.name} {distribution} slot {slot} PID {os.getpid()}',
                                              give_up)
        if not placed or (run.shared_slots and shared is None):
            yield None
            return
        started = time.monotonic()
        remote = None
        try:
            if run.stopping.is_set():
                raise RuntimeError('the run is stopping')
            directory = run.directory / f'{distribution}-{slot}'
            directory.mkdir(mode=0o700, exist_ok=True)
            # An Incus VM has the architecture of this machine, and the
            # test machines that run incus.py are AMD64.
            remote = {'name': f'{run.name}-{distribution}-{slot}', 'arch': 'amd64', 'distribution': distribution}
            for role, field in (('mmwatchdog-dev', 'node'), ('mmwatchdog-peer', 'peer_node')):
                name, remote[field] = start_guest(run, distribution, slot, role, directory)
                names.append(name)
            run.built_once.wait()
            with suite_runner.OUTPUT_LOCK:
                print(f'{distribution}: slot {slot} ready in {time.monotonic() - started:.1f}s', flush=True)
        except Exception as error:
            remote = None
            with suite_runner.OUTPUT_LOCK:
                print(f'{distribution}: slot {slot} has no guests: {error}', flush=True)
        yield remote
    finally:
        for name in names:
            incus('delete', '--force', name, check=False)
        if shared:
            run.shared_slots.release(shared)
        if placed:
            run.pair_places.release()


def run_queue(run, distribution, jobs, suites):
    """Run one distribution's suites; return all.py's status."""
    label = distribution + ': '
    try:
        return suite_runner.run_suites(
            ROOT / distribution, jobs, suites=suites, label=label,
            guests=lambda slot, pending: guest_pair(run, distribution, slot, pending))
    except Exception:
        with suite_runner.OUTPUT_LOCK:
            print(f'{label}runner failed\n{traceback.format_exc()}', flush=True)
        return 1


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--distribution', action='append', choices=suite_runner.TARGET_DISTRIBUTIONS,
                        help='repeat for each distribution to test; default: all of them')
    parser.add_argument('--suite', action='append', choices=[name for name, _ in suite_runner.SUITES],
                        help='repeat to run only these suites; default: all of them')
    parser.add_argument('--monitoring-setup-scenarios-file', type=Path, metavar='SCENARIOS_FILE',
                        help='in place of the suites of the catalog, run the scenarios of the whole monitoring setup that '
                             'this scenario file describes (tests/integration/monitoring_setup_scenarios.py), as a '
                             "suite named after the file's directory, such as a setup that is not in the repository")
    parser.add_argument('--pairs-per-distribution', type=int, default=3,
                        help='job slots per distribution, each with its two guests; default 3')
    parser.add_argument('--max-guest-pairs-at-once', type=int, default=max(1, (os.cpu_count() or 2) // 2),
                        help="the most job slots that have their two guests on this machine at once; "
                             "default: half of the machine's CPU count")
    parser.add_argument('--shared-guest-slots', type=Path, metavar='DIRECTORY',
                        help='a directory of lock files named slot-* that every test run on this machine shares; '
                             f'each guest pair holds {SHARED_SLOTS_PER_PAIR} of them while it runs')
    parser.add_argument('--prepare-bases', action='store_true',
                        help='make the bases of the selected distributions again, from the newest images, and stop')
    parser.add_argument('--cleanup-from-manifest', type=Path, metavar='OWNERSHIP_JSON',
                        help="delete a run's guests again, from its ownership.json")
    args = parser.parse_args()
    if min(args.pairs_per_distribution, args.max_guest_pairs_at_once) < 1:
        parser.error('pairs per distribution and pairs at once must be positive')
    if args.monitoring_setup_scenarios_file and args.suite:
        parser.error('--monitoring-setup-scenarios-file runs in place of the catalog; leave out --suite')
    distributions = args.distribution or list(suite_runner.TARGET_DISTRIBUTIONS)
    with testenv.locked(ROOT):
        if args.cleanup_from_manifest:
            cleanup(args.cleanup_from_manifest.resolve())
            return 0
        if args.prepare_bases:
            for distribution in distributions:
                incus('image', 'delete', image_alias(distribution), check=False)
            with ThreadPoolExecutor(max_workers=len(distributions)) as pool:
                failures = [f for f in pool.map(lambda d: _failure(prepare_base, d), distributions) if f]
            for failure in failures:
                print(failure, flush=True)
            return int(bool(failures))
        if args.monitoring_setup_scenarios_file:
            scenarios_file = args.monitoring_setup_scenarios_file.resolve()
            suites = (suite_runner.monitoring_setup_suite(scenarios_file.parent.name, scenarios_file),)
        else:
            suites = tuple(s for s in suite_runner.SUITES if not args.suite or s[0] in args.suite)
        suites_of_each_distribution = suite_runner.suites_by_distribution(suites, distributions)
        shared_slots = SharedGuestSlots(args.shared_guest_slots) if args.shared_guest_slots else None
        run = Run(args.max_guest_pairs_at_once, shared_slots)

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
        print(f'Incus run {run.name}: {len(suites_of_each_distribution) - len(failed)} of '
              f'{len(suites_of_each_distribution)} distributions passed'
              + (', failed: ' + ', '.join(failed) if failed else '') + f'; {time.monotonic() - started:.0f}s',
              flush=True)
        return int(bool(failed))


def _failure(function, *args):
    try:
        function(*args)
        return None
    except Exception as error:
        return f'{" ".join(args)}: FAILED: {error}'


if __name__ == '__main__':
    raise SystemExit(main())
