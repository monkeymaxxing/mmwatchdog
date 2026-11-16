#!/usr/bin/env python3
"""Run MMWatchdog's VM suites on the two guests of one test environment, which
incus.py, hetzner.py, or cloud.py created; Python standard library only.

The guests are named in the file remote.json in the test environment
(MMWATCHDOG_TEST_ROOT), with their addresses, SSH logins, SSH host keys,
distribution, and processor architecture. The harness reaches them over SSH
and verifies their host keys.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import time
import urllib.request
import testenv

REPO = Path(__file__).resolve().parents[2]
ROOT = testenv.root()
# The file of the test environment's guests: incus.py and hetzner.py write
# one into each job slot, and cloud.py into its test root.
REMOTE_FILE = ROOT / 'remote.json'
REMOTE = json.loads(REMOTE_FILE.read_text()) if REMOTE_FILE.exists() else None
# What a suite says when its test environment has no guests.
NO_GUESTS_MESSAGE = (
    str(REMOTE_FILE) + ' does not exist. The VM suites run only on the guests that '
    'tests/integration/incus.py, tests/integration/hetzner.py, or tests/integration/cloud.py '
    'creates; each writes remote.json into the test environment (MMWATCHDOG_TEST_ROOT) of the guests. '
    'Start the suites through incus.py or hetzner.py, or set MMWATCHDOG_TEST_ROOT to the test root of cloud.py up.')
# The processor architecture of the guests, as Go names it: amd64 or arm64.
# Every program that a suite builds for a guest is built for it.
ARCH = REMOTE['arch'] if REMOTE else None
# The distribution of the guests, one of all.py's TARGET_DISTRIBUTIONS, such
# as debian13. It names the files of a suite's results.
DISTRIBUTION = REMOTE['distribution'] if REMOTE else None
# How uname -m names each architecture.
MACHINE_OF_ARCHITECTURE = {'amd64': 'x86_64', 'arm64': 'aarch64'}
NAME = 'mmwatchdog-dev'
# The fleet workflow's joined node: a smaller guest of the same
# distribution beside NAME.
PEER = 'mmwatchdog-peer'
# The name by which every guest reaches the guest NAME, from the guest's
# /etc/hosts (up).
MAIN_GUEST_HOST = NAME + '.mmwatchdog-test.internal'
MARKER = 'mmwatchdog-disposable-integration-v1'
# When the step of a suite's setup before the current one ended: at first,
# when this command started.
SETUP_STEP_ENDED = [time.monotonic()]


# Where the machine that runs the suites keeps what they download, for every
# test run on that machine: outside every test environment, so that each new
# test environment and run slot finds it there.
DOWNLOADS_OF_EVERY_TEST_RUN = Path('~/.cache/mmwatchdog-test-downloads').expanduser()


def download_release_archive(url, expected_sha256):
    """The archive at url, on this machine: downloaded once into DOWNLOADS_OF_EVERY_TEST_RUN and checked against its SHA-256, then taken from there."""
    name = url.rsplit('/', 1)[1]
    path = DOWNLOADS_OF_EVERY_TEST_RUN / name
    if path.exists():
        return path
    DOWNLOADS_OF_EVERY_TEST_RUN.mkdir(parents=True, exist_ok=True)
    # A name of this process's own, so that two runs that download at the
    # same time never write one file.
    partial = path.with_name(name + '.downloading-' + str(os.getpid()))
    digest = hashlib.sha256()
    try:
        with urllib.request.urlopen(url, timeout=60) as response, partial.open('wb') as file:
            while chunk := response.read(1 << 20):
                digest.update(chunk)
                file.write(chunk)
        assert digest.hexdigest() == expected_sha256, (name, digest.hexdigest())
        partial.rename(path)
    finally:
        partial.unlink(missing_ok=True)
    return path


def setup_step_done(step):
    """Print the seconds that a step of a suite's setup took, as a
    scenario's line does, so that the log shows where the setup spends its
    time before the first scenario."""
    print(f'SETUP {time.monotonic() - SETUP_STEP_ENDED[0]:4.0f}s {step}', flush=True)
    SETUP_STEP_ENDED[0] = time.monotonic()

ENV = dict(os.environ)
CONTROL_SOCKETS = Path.home() / '.ssh'

def cmd(args, *, cwd=None, input=None, check=True, timeout=300, inherit_lock=False, extra_env=None):
    env = dict(ENV, **(extra_env or {}))
    env.pop(testenv.LOCK_FD, None)
    fd = testenv.inherited_fd(ROOT) if inherit_lock else None
    if fd is not None:
        env[testenv.LOCK_FD] = str(fd)
    p = subprocess.run([str(a) for a in args], cwd=cwd, env=env, input=input,
                       pass_fds=() if fd is None else (fd,), text=True,
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
    if check and p.returncode:
        raise RuntimeError(f'{shlex.join(str(a) for a in args)}\n{p.stdout}\n{p.stderr}')
    return p

def remote_node(guest=NAME):
    """The SSH connection of a guest from remote.json: mmwatchdog-dev, or mmwatchdog-peer,
    which incus.py and hetzner.py always create and cloud.py up only with
    --with-peer."""
    if REMOTE is None:
        raise RuntimeError(NO_GUESTS_MESSAGE)
    if guest == NAME:
        return REMOTE['node']
    if guest == PEER and 'peer_node' in REMOTE:
        return REMOTE['peer_node']
    raise RuntimeError('this test environment has no guest ' + guest + '; cloud.py up --with-peer creates the second VM')

def ssh_options(guest=NAME):
    node = remote_node(guest)
    # Share one connection to a cloud guest instead of a new handshake per
    # step. %C keys the socket to this node; the master exits after 60 idle
    # seconds, so an interrupted run cannot leave it behind. The socket is in
    # ~/.ssh, named for this test environment: below a deep test root its
    # path passed the 104 to 108 characters that a socket's path may have.
    return ['-F', '/dev/null', '-o', 'BatchMode=yes', '-o', 'IdentitiesOnly=yes',
            '-o', 'StrictHostKeyChecking=yes', '-o', 'ConnectTimeout=5',
            '-o', 'UserKnownHostsFile=' + node['ssh_known_hosts'], '-i', node['ssh_identity_file'],
            '-o', 'Port=' + str(node['ssh_port']), '-o', 'ControlMaster=auto',
            '-o', 'ControlPath=' + str(CONTROL_SOCKETS / ('mmwatchdog-' + hashlib.sha256(str(ROOT).encode()).hexdigest()[:12] + '-%C')),
            '-o', 'ControlPersist=60']

def ssh_target(guest=NAME):
    return remote_node(guest)['ssh_user'] + '@' + remote_node(guest)['address']

def ssh_args(guest=NAME):
    return ['ssh', *ssh_options(guest), ssh_target(guest)]

def console(script, check=True, timeout=300, guest=NAME):
    return cmd(ssh_args(guest) + [script], check=check, timeout=timeout)

def write(path, content, mode='0644', guest=NAME):
    # mkdir, not install -d: install gives a directory that is there the
    # permissions of a new one, and so takes the sticky bit from /var/tmp.
    console('mkdir -p ' + shlex.quote(str(Path(path).parent)), guest=guest)
    cmd(ssh_args(guest) + ['tee ' + shlex.quote(path)], input=content)
    console('chmod ' + mode + ' ' + shlex.quote(path), guest=guest)

def each_unit(action, units):
    """The shell text that runs systemctl with the action for each of the units by itself, whether the unit is there or not. A systemd from release 260 does nothing for a list of units of which one is not there."""
    return 'for unit in ' + ' '.join(units) + '; do systemctl ' + action + ' $unit 2>/dev/null; done; true'


def signal_main_process(unit, signal, guest=NAME, check=True):
    """Send a signal to the main process of a unit. systemd names the option --kill-whom from release 252, and --kill-who before."""
    return console('systemctl kill --kill-whom=main --signal=' + signal + ' ' + unit + ' 2>/dev/null || systemctl kill --kill-who=main --signal=' + signal + ' ' + unit, guest=guest, check=check)


def hostname(guest=NAME):
    """The guest's own name, as its kernel holds it. Not every distribution has the program hostname."""
    return console('cat /proc/sys/kernel/hostname', guest=guest).stdout.strip()


def addresses(guest=NAME):
    """The addresses of the guest's network interfaces, without those of its loopback interface."""
    return console("ip -o addr show scope global | awk '{print $4}' | cut -d/ -f1", guest=guest).stdout.split()


def distribution(guest=NAME):
    """The ID and the VERSION_ID of the guest's /etc/os-release, such as ('debian', '13'). A rolling release, such as Arch Linux, has no VERSION_ID: ''."""
    values = dict(line.split('=', 1) for line in console('cat /etc/os-release', guest=guest).stdout.splitlines() if '=' in line)
    return values['ID'].strip('"'), values.get('VERSION_ID', '').strip('"')


# The package manager of each distribution, by the ID of its /etc/os-release.
PACKAGE_MANAGERS = {'debian': 'apt', 'ubuntu': 'apt', 'arch': 'pacman', 'opensuse-leap': 'zypper', 'rocky': 'dnf', 'almalinux': 'dnf', 'fedora': 'dnf'}
# The install command of each package manager; the package names follow it.
# pacman installs from a fresh database (-Sy), because the guest's base is
# up to a day old and Arch Linux's mirrors keep only the newest packages.
INSTALL_COMMANDS = {
    'apt': 'export DEBIAN_FRONTEND=noninteractive; apt-get -o DPkg::Lock::Timeout=600 update -qq && apt-get -o DPkg::Lock::Timeout=600 install -y -qq ',
    'pacman': 'pacman -Sy --noconfirm --needed ',
    'zypper': 'zypper --non-interactive install --no-recommends ',
    'dnf': 'dnf install -y -q ',
}
# The name that a package has where it differs from the Debian name, which a
# scenario writes.
PACKAGE_NAMES = {
    'pacman': {'nfs-kernel-server': 'nfs-utils', 'ca-certificates': 'ca-certificates-utils'},
    'zypper': {},
    'dnf': {'nfs-kernel-server': 'nfs-utils'},
}
# The shell text sets the trust-store directory and rebuild command of each
# package manager's distributions: where the system takes in a certificate
# of the test, and the command that builds the bundle again.
TRUST_STORES = {
    'apt': 'added=/usr/local/share/ca-certificates; build="update-ca-certificates --fresh"; ',
    'pacman': 'added=/etc/ca-certificates/trust-source/anchors; build="update-ca-trust"; ',
    'zypper': 'added=/etc/pki/trust/anchors; build="update-ca-certificates --fresh"; ',
    'dnf': 'added=/etc/pki/ca-trust/source/anchors; build="update-ca-trust"; ',
}


def package_manager(guest=NAME):
    """The guest's package manager: apt, pacman, zypper, or dnf."""
    return PACKAGE_MANAGERS[distribution(guest)[0]]


def distribution_package_name(name, manager):
    return PACKAGE_NAMES.get(manager, {}).get(name, name)


def install_packages(packages, guest=NAME, timeout=900):
    """Install the named packages on the guest with its package manager. The
    names are Debian's (PACKAGE_NAMES gives the others)."""
    manager = package_manager(guest)
    names = [distribution_package_name(name, manager) for name in packages.split()]
    console(INSTALL_COMMANDS[manager] + ' '.join(names) + ' >/dev/null', guest=guest, timeout=timeout)


def trust_certificate(name, certificate, guest=NAME):
    """Add a certificate to the trust store of the guest's operating system, under a name of the test. The store is built anew, also when an interrupted run left an earlier certificate under the same name."""
    write('/var/tmp/' + name + '.crt', certificate, guest=guest)
    console(TRUST_STORES[package_manager(guest)] + 'install -d -m 0755 $added && install -m 0644 /var/tmp/' + name + '.crt $added/' + name + '.crt && rm -f /var/tmp/' + name + '.crt && $build >/dev/null 2>&1', guest=guest)


def distrust_certificate(name, guest=NAME):
    """Take a certificate of the test out of the trust store of the guest's operating system, when it is there."""
    console(TRUST_STORES[package_manager(guest)] + 'if [ -e $added/' + name + '.crt ]; then rm -f $added/' + name + '.crt; $build >/dev/null 2>&1; fi; true', guest=guest)


# The two directories in which a node holds the modules that are not built
# into MMWatchdog: the official extra modules of the MMWatchdog release, and the modules of
# the project's own.
OFFICIAL_EXTRA_MODULES = '/usr/local/lib/mmwatchdog/official_extra_modules'
EXTERNAL_MODULES = '/usr/local/lib/mmwatchdog/external_modules'


def write_module(name, files, directory=EXTERNAL_MODULES, guest=NAME):
    """Write the directory of one module on a node, in place of what was
    there. files maps each file name to its text; the file module_executable
    becomes executable."""
    console('rm -rf ' + shlex.quote(directory + '/' + name), guest=guest)
    for file_name, text in files.items():
        write(directory + '/' + name + '/' + file_name, text, '0755' if file_name == 'module_executable' else '0644', guest=guest)


def module_files(source):
    """The files of one module's directory on this machine, as write_module takes them."""
    return {path.name: path.read_text() for path in sorted(Path(source).iterdir()) if path.is_file()}


def module_description(name, used_as='check', inputs=None, measurements=None, one_incident_for_each_finding=False):
    """The module_description.yml of a module that a scenario writes itself.
    inputs maps the name of each input to its type, and no input is required;
    measurements maps the name of each measurement to its unit."""
    description = {
        'summary': 'A module of the MMWatchdog test suite.',
        'used_as': used_as,
        'inputs': {input_name: {'type': input_type, 'required': False, 'description': 'An input of the test suite.'} for input_name, input_type in (inputs or {}).items()},
        'examples': [{'description': 'The module without inputs.', 'config': name + ': {}\n'}],
    }
    if measurements:
        description['measurements'] = {measurement: {'unit': unit, 'description': 'A measurement of the test suite.'} for measurement, unit in measurements.items()}
    if one_incident_for_each_finding:
        description['one_incident_for_each_finding'] = True
    return json.dumps(description, indent=2) + '\n'


def install_official_extra_modules(guest=NAME):
    """Put the official extra modules of the repository on a node, as a
    deploy does: every module, and the version that they were shipped with."""
    console('rm -rf ' + OFFICIAL_EXTRA_MODULES, guest=guest)
    source = REPO / 'official_extra_modules'
    write(OFFICIAL_EXTRA_MODULES + '/shipped_with_mmwatchdog_version', (source / 'shipped_with_mmwatchdog_version').read_text(), guest=guest)
    for module in sorted(path for path in source.iterdir() if path.is_dir()):
        write_module(module.name, module_files(module), directory=OFFICIAL_EXTRA_MODULES, guest=guest)


# A node that has not joined a hub must have alert_destinations.yml; a test
# node that alerts nobody states so.
NO_DESTINATION = {'destinations': {}}


def configuration_files(configuration):
    """Split a test's configuration into its authored files: the node
    settings, the check groups, and, when the configuration states
    alert_destinations, the destinations file."""
    settings = {key: value for key, value in configuration.items() if key not in ('check_groups', 'alert_destinations')}
    files = {'mmwatchdog_node_settings.yml': json.dumps(settings, indent=2),
             'check_groups/node.yml': json.dumps({'check_groups': configuration['check_groups']}, indent=2)}
    if 'alert_destinations' in configuration:
        files['alert_destinations.yml'] = json.dumps(configuration['alert_destinations'], indent=2)
    return files


def write_configuration(configuration, directory='/etc/mmwatchdog', guest=NAME):
    for relative, content in configuration_files(configuration).items():
        write(str(Path(directory) / relative), content, guest=guest)


def write_configuration_files(directory, configuration):
    for relative, content in configuration_files(configuration).items():
        path = Path(directory) / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)


def copy(source, target):
    cmd(['scp', *ssh_options(), source, ssh_target() + ':' + target])

# Keep every boot of a suite in the journals saved after a failure.
# journald (Storage=auto) writes on disk once the directory exists, and
# the flush moves this boot's entries there. Rocky Linux 9 and AlmaLinux 10
# keep the journal in /run, which a reboot empties; the directory gets the
# owner, mode, and SELinux label of the distribution's own (restorecon
# exists only where SELinux does).
PERSISTENT_JOURNAL = ('if [ ! -d /var/log/journal ]; then mkdir -p /var/log/journal && systemd-tmpfiles --create --prefix /var/log/journal && '
                      '{ restorecon -R /var/log/journal 2>/dev/null; true; } && journalctl --flush; fi')


# A guest boots with the time of its emulated hardware clock, which counts
# whole seconds and is up to about two seconds behind. The guest's time
# service steps the clock to the right time at its first synchronization,
# which on Ubuntu and Debian comes about half a minute after the boot: in
# the middle of a suite. MMWatchdog times its waits on the monotonic clock, so such
# a step changes none of them. But the times that MMWatchdog records and shows
# would jump, and a suite would see a step of the clock that it did not
# make itself; the scenarios of a clock step make their own (set_clock). So
# a guest takes this machine's time, which follows a time server, before a
# suite uses it and after each boot, unless the guest's clock has
# synchronized with a time server already. Its time service then finds the
# clock right to a few milliseconds and corrects it gradually, without a
# step.
def set_clock_from_this_machine(guest=NAME):
    """Set the guest's clock to this machine's time, unless the guest's
    clock has synchronized with a time server already."""
    if console('timedatectl show --property NTPSynchronized --value', guest=guest).stdout.strip() == 'yes':
        return
    set_clock(guest)


def set_clock(guest=NAME, seconds_ahead_of_this_machine=0):
    """Set the guest's clock to this machine's time plus
    seconds_ahead_of_this_machine, also when the guest's clock has
    synchronized with a time server. A scenario of a step of the clock
    steps it with a number other than 0, and puts it back with 0."""
    console('date --set=@' + format(time.time() + seconds_ahead_of_this_machine, '.6f'), guest=guest)


def up(guest=NAME):
    """Wait until the guest has its disposable-test marker and cloud-init has
    finished, check that its processor architecture is the one of
    remote.json, name the main guest in its /etc/hosts, and set its clock
    (set_clock_from_this_machine)."""
    for attempt in range(90):
        result = console('cat /etc/mmwatchdog-test-vm', check=False, guest=guest)
        if result.returncode == 0:
            assert result.stdout.strip() == MARKER
            break
        time.sleep(2)
    else: raise RuntimeError('MMWatchdog test guest ' + guest + ' did not become ready')
    machine = console('uname -m', guest=guest).stdout.strip()
    assert machine == MACHINE_OF_ARCHITECTURE[ARCH], guest + ' is ' + machine + ', and remote.json says ' + ARCH
    # An Incus guest is a copy of a base on which cloud-init has done
    # its one job; a cloud server runs it at its first boot.
    if console('test -e /etc/cloud/cloud-init.disabled', check=False, guest=guest).returncode != 0:
        cloud = console('cloud-init status --wait --format=json', check=False, timeout=300, guest=guest)
        details = json.loads(cloud.stdout)
        assert cloud.returncode in {0, 2} and details['status'] == 'done' and not details.get('errors'), details
    console("sed -i '/ # mmwatchdog-test-guest$/d' /etc/hosts; echo '" + REMOTE['node']['address'] + ' ' + MAIN_GUEST_HOST + " # mmwatchdog-test-guest' >> /etc/hosts", guest=guest)
    console(PERSISTENT_JOURNAL, guest=guest)
    set_clock_from_this_machine(guest)
    print('Native ' + ARCH.upper() + ' ' + DISTRIBUTION + ' MMWatchdog guest ' + guest + ' of ' + REMOTE['name'] + ' ready; pinned SSH host key', flush=True)

def reboot(guest=NAME):
    """Start the guest's operating system again, wait until it runs: its boot ID is a new one and systemd has started its units, and set its clock (set_clock_from_this_machine)."""
    assert console('cat /etc/mmwatchdog-test-vm', guest=guest).stdout.strip() == MARKER
    before = console('cat /proc/sys/kernel/random/boot_id', guest=guest).stdout.strip()
    console('systemctl reboot', check=False, guest=guest)
    deadline = time.monotonic() + 600

    def boot():
        # A guest that is going down or coming up can leave the question
        # without an answer.
        try:
            return console('cat /proc/sys/kernel/random/boot_id', check=False, timeout=20, guest=guest).stdout.strip()
        except subprocess.TimeoutExpired:
            return ''
    while boot() in {'', before}:
        if time.monotonic() > deadline:
            raise RuntimeError('the guest ' + guest + ' did not start again')
        time.sleep(2)
    console('systemctl is-system-running --wait', check=False, timeout=300, guest=guest)
    set_clock_from_this_machine(guest)

def reset():
    assert console('cat /etc/mmwatchdog-test-vm').stdout.strip() == MARKER
    console(each_unit('stop', ['mmwatchdog-soak.scope', 'mmwatchdog-peer']) + '; systemctl reset-failed mmwatchdog-peer 2>/dev/null; true')
    signal_main_process('mmwatchdog-storage.service', 'CONT', check=False)
    console(each_unit('stop', ['mmwatchdog.service', 'mmwatchdog-fixture.service', 'mmwatchdog-storage.service', 'mmwatchdog-storage-target.service', 'mmwatchdog-limited.service', 'mmwatchdog-starting.service', 'mmwatchdog-resources.service', 'mmwatchdog-lifecycle.service',
                               'mmwatchdog-crashloop.service', 'mmwatchdog-one-cause.service']))
    # Stop every Mailpit server and remove its certificate after an interruption.
    import email_scenarios
    email_scenarios.stop_mail_servers()
    # So does an interrupted scenario of the official extra destination
    # modules, with the stand-in for their services.
    console("systemctl stop mmwatchdog-test-destination-services-stand-in.service 2>/dev/null; rm -f /etc/systemd/system/mmwatchdog-test-destination-services-stand-in.service")
    distrust_certificate('mmwatchdog-test-destination-services-stand-in')
    console("for m in full readonly; do if mountpoint -q /var/tmp/mmwatchdog-fixture/$m; then umount /var/tmp/mmwatchdog-fixture/$m; fi; done")
    # An interrupted scenario of the node's own checks leaves its mounts,
    # its network interface, and its units.
    import node_scenarios
    import service_scenarios
    service_scenarios.clean_up()
    node_scenarios.unmount()
    node_scenarios.remove_array()
    node_scenarios.remove_cisa_and_osv_stand_in()
    console(each_unit('stop', [*node_scenarios.TRANSIENT_UNITS, node_scenarios.UNIT]) + '; ' + each_unit('reset-failed', node_scenarios.TRANSIENT_UNITS) + '; '
            "rm -f /etc/systemd/system/" + node_scenarios.UNIT + "; ip link del " + node_scenarios.INTERFACE + " 2>/dev/null; "
            "for unit in " + node_scenarios.TIME_SERVICES + "; do if systemctl is-enabled --quiet $unit 2>/dev/null; then systemctl start $unit; fi; done; true")
    console(each_unit('reset-failed', ['mmwatchdog.service', 'mmwatchdog-storage.service', 'mmwatchdog-storage-target.service', 'mmwatchdog-limited.service', 'mmwatchdog-starting.service', 'mmwatchdog-resources.service', 'mmwatchdog-lifecycle.service', 'mmwatchdog-crashloop.service',
                                       'mmwatchdog-one-cause.service', 'mmwatchdog-failed-backup.service', 'mmwatchdog-failed-certbot.service', 'mmwatchdog-failed-ignored.service']))
    console("rm -rf /var/lib/mmwatchdog /var/lib/mmwatchdog-module-state /var/lib/mmwatchdog-peer /run/mmwatchdog /etc/mmwatchdog /etc/mmwatchdog-peer /var/tmp/mmwatchdog-fixture /usr/local/lib/mmwatchdog; mkdir -p /var/tmp/mmwatchdog-fixture")
    # MMX keeps node job records, including deployed binaries, for its 24-hour
    # history retention. Repeated suites would otherwise fill the guest disk.
    # MMX's HTTPS endpoint then starts again where an earlier suite's deploy
    # installed it, so that the next deploy reaches the node over HTTPS as
    # the first one does, instead of trying HTTPS for each fleet stage and
    # falling back to SSH.
    console("systemctl stop mmx-https-api.service 2>/dev/null || true; rm -rf /var/lib/mmx/jobs; systemctl start mmx-https-api.service 2>/dev/null || true")

def connection(guest=NAME):
    """The guest as a node of an MMX inventory."""
    return remote_node(guest)

def standalone():
    # Deployment uses a persistent MMX endpoint; prove MMWatchdog does not need it.
    assert console('cat /etc/mmwatchdog-test-vm').stdout.strip() == MARKER
    console('systemctl stop mmx-https-api.service')
    assert console('pgrep -x mmx-runner', check=False).returncode == 1

def build():
    print('Building MMWatchdog for the controller and Linux ARM64/AMD64', flush=True)
    cmd(['sh', REPO / 'scripts/build.sh'], cwd=REPO, inherit_lock=True, timeout=600)

def tests(group=None):
    from acceptance import suite
    suite(group)

def journal_file(suite, guest):
    """Where save_guest_journals puts the journal of one guest for a suite:
    beside the suite's log, <suite>.log, in the test environment."""
    return ROOT / (suite + '.journal-of-' + guest + '.log')


def save_guest_journals(since_unix_time, suite):
    """Save the systemd journal of each guest of this test environment from
    since_unix_time on, with every boot in that time, to journal_file. A
    guest that is not there, does not answer in time, or fails gets no file
    and a line that says why; the command then still saves the other's."""
    failures = 0
    for guest in (NAME, PEER):
        path = journal_file(suite, guest)
        path.unlink(missing_ok=True)
        try:
            if REMOTE is None or (guest == PEER and 'peer_node' not in REMOTE):
                print('No journal of ' + guest + ': this test environment has no guest ' + guest, flush=True)
                continue
            # Without --boot, journalctl reads every boot that the guest's
            # journal holds: the fleet suite and the node suite reboot guests.
            result = console('journalctl --no-pager --output=short-iso-precise --since=@' + str(int(since_unix_time)),
                             check=False, timeout=120, guest=guest)
            if result.returncode:
                raise RuntimeError('journalctl exited with ' + str(result.returncode) + ': ' + result.stderr.strip())
            path.write_text(result.stdout)
            print('Saved the journal of ' + guest + ' to ' + str(path), flush=True)
        except Exception as error:
            failures += 1
            print('No journal of ' + guest + ': ' + repr(error), flush=True)
    return 1 if failures else 0


def stop():
    """End the test command that holds this test environment or one of its job
    slots, then delete the guests of its incus.py runs and the servers of its
    hetzner.py runs. Nothing outside this environment is touched."""
    # The environment itself, the environment of each distribution that
    # incus.py or hetzner.py runs below it, and the job slots of each of them,
    # at <root>/<distribution>/slots/<n>.
    slots = sorted(slot for slot in ROOT.glob('*/slots/*') if slot.is_dir())
    environments = [ROOT, *sorted({slot.parent.parent for slot in slots}), *slots]
    for directory in environments:
        ended = testenv.stop_run(directory)
        if ended:
            print(f'Ended the test command of {directory}: {ended}', flush=True)
    failures = 0
    # An ended incus.py or hetzner.py run may not have deleted its guests;
    # its manifest says so.
    for runs, runner in (('incus-runs', 'incus.py'), ('cloud-runs', 'hetzner.py')):
        for manifest in sorted((ROOT / runs).glob('*/ownership.json')):
            if not json.loads(manifest.read_text()).get('cleanup_verified'):
                env = dict(os.environ)
                env.pop(testenv.LOCK_FD, None)
                code = subprocess.run([sys.executable, '-u', str(Path(__file__).with_name(runner)),
                                       '--cleanup-from-manifest', str(manifest)], env=env).returncode
                print(f'Guests of {manifest.parent.name}: ' + ('deleted' if code == 0 else
                      f'not deleted; run {runner} --cleanup-from-manifest {manifest}'), flush=True)
                failures += code != 0
    return 1 if failures else 0

if __name__ == '__main__':
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('command', choices=['up', 'test', 'deployment', 'soak', 'lifecycle', 'fleet', 'standalone', 'reset', 'stop',
                                       'save-guest-journals', 'monitoring-setup-scenarios'])
    from acceptance import GROUPS
    p.add_argument('--group', choices=GROUPS, help='test: run one independent group')
    p.add_argument('--prepare', action='store_true', help='soak: install a fresh test fixture first')
    p.add_argument('--sustained', action='store_true',
                   help='soak: after coverage, continue until event history has filled and half turned over')
    p.add_argument('--since-unix-time', type=int, metavar='SECONDS',
                   help='save-guest-journals: the journal entries from this time on')
    p.add_argument('--suite', help='save-guest-journals: the suite whose log the journals go beside, as <suite>.journal-of-<guest>.log')
    p.add_argument('--scenarios-file', type=Path,
                   help='monitoring-setup-scenarios: the scenario file of the whole monitoring setup to run '
                        '(tests/integration/monitoring_setup_scenarios.py)')
    args = p.parse_args()
    if args.group and args.command != 'test':
        p.error('--group requires test')
    if args.prepare and args.command != 'soak':
        p.error('--prepare requires soak')
    if (args.command == 'save-guest-journals') != (args.since_unix_time is not None and args.suite is not None):
        p.error('save-guest-journals requires --since-unix-time and --suite, and only it takes them')
    if (args.command == 'monitoring-setup-scenarios') != (args.scenarios_file is not None):
        p.error('monitoring-setup-scenarios requires --scenarios-file, and only it takes it')
    if args.command == 'stop':
        # Before the lock: the command to end is the one that holds it.
        sys.exit(stop())
    if args.command == 'save-guest-journals':
        # Before the lock: it only reads the guests, and a suite's command
        # that did not end must not keep the journals from being saved.
        sys.exit(save_guest_journals(args.since_unix_time, args.suite))
    if REMOTE is None:
        sys.exit(NO_GUESTS_MESSAGE)
    with testenv.locked(ROOT):
        if args.command == 'up': up()
        elif args.command == 'test': tests(args.group)
        elif args.command == 'deployment':
            up()
            setup_step_done('the guests are ready')
            build()
            setup_step_done('MMWatchdog is built')
            from deployment import suite
            suite()
        elif args.command == 'standalone': standalone()
        elif args.command == 'lifecycle':
            from lifecycle import suite
            suite()
        elif args.command == 'fleet':
            import mmx_build
            mmx_build.build()
            from fleet import suite
            suite()
        elif args.command == 'soak':
            if args.prepare:
                from acceptance import suite as prepare
                prepare('setup')
            from soak import suite
            suite(args.sustained)
        elif args.command == 'reset': reset()
        elif args.command == 'monitoring-setup-scenarios':
            import monitoring_setup_scenarios
            monitoring_setup_scenarios.run(args.scenarios_file)
