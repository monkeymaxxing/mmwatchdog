"""The node's own checks on a real Linux guest: the further resources of the
node, disk devices, network interfaces, filesystems that are not mounted,
every mounted filesystem, one program of a systemd unit, Unix sockets, ping,
the clock, out-of-memory kills, files, a file on a network filesystem whose
server is gone, log lines, the system log destination, security updates, a
required reboot, a software RAID array, and the temperature."""
import hashlib
import json
import re
import shlex
import time
import vm

FIXTURE = '/var/tmp/mmwatchdog-fixture'
# The program on a SUSE guest that reads zypper's own answers for the test, independently of the checks.
ZYPPER_HELPER = FIXTURE + '/zypper_helper.py'
ZYPPER_HELPER_TEXT = r'''import configparser, glob, os, re, subprocess, sys
import xml.etree.ElementTree as ET


def command(*arguments, check=True):
    done = subprocess.run(arguments, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env={'PATH': '/usr/sbin:/usr/bin:/sbin:/bin', 'LC_ALL': 'C'})
    if check and done.returncode != 0:
        sys.exit('%s: %s%s' % (' '.join(arguments), done.stdout, done.stderr))
    return done


def zypper(*arguments, check=True):
    return command('zypper', '--non-interactive', '--no-refresh', *arguments, check=check)


def patches(*options):
    """Every security patch that zypper lists as root, as a dictionary from the patch's name to its element."""
    listed = zypper('--xmlout', 'list-patches', '--category', 'security', *options).stdout
    return {update.get('name'): update for update in ET.fromstring(listed).iter('update') if update.get('kind') == 'patch'}


def describe(update):
    return update.get('name') + ' (' + update.get('severity') + '): ' + update.find('summary').text.strip()


def vercmp(first, second):
    return int(command('rpm', '--eval', '%{lua:print(rpm.vercmp("' + first + '", "' + second + '"))}').stdout)


if sys.argv[1] == 'repository-directories':
    # The cache directories of the enabled repositories, read from the repository files by configparser.
    parser = configparser.ConfigParser(interpolation=None)
    for path in sorted(glob.glob('/etc/zypp/repos.d/*.repo')):
        parser.read(path)
    enabled = sorted(section for section in parser.sections() if parser[section].get('enabled', '1') == '1')
    existing = sorted(os.listdir('/var/cache/zypp/raw'))
    assert enabled == existing, (enabled, existing)
    print('\n'.join(enabled))
elif sys.argv[1] == 'waiting':
    # The security patches that wait, one description for each, sorted.
    print('\n'.join(sorted(describe(update) for update in patches().values() if update.get('status') == 'needed')))
elif sys.argv[1] == 'install-an-older-package':
    # Choose, from the security patches that do not wait, a package that is
    # not installed and has a version older than the one that a patch fixes,
    # install that version, and keep it when the patch then waits.
    for name, update in sorted(patches('--all').items()):
        if update.get('status') != 'not-needed' or update.get('restart') == 'true' or re.search(r'kernel|nvidia', update.find('summary').text, re.I):
            continue
        conflicts = [line.split() for line in zypper('patch-info', name).stdout.splitlines() if re.match(r'\s+\S+\.x86_64 < ', line)]
        for package_and_architecture, _, fixed in conflicts:
            package = package_and_architecture.rsplit('.', 1)[0]
            if command('rpm', '-q', package, check=False).returncode == 0 or package.endswith(('-devel', '-debuginfo', '-debugsource')):
                continue
            versions = [solvable.get('edition') for solvable in ET.fromstring(zypper('--xmlout', 'search', '--details', '--match-exact', package).stdout).iter('solvable')
                        if solvable.get('arch') == 'x86_64' and vercmp(solvable.get('edition'), fixed) < 0]
            if not versions:
                continue
            installed = zypper('install', '--oldpackage', package + '=' + sorted(versions)[0], check=False)
            if installed.returncode == 0 and name in {n for n, u in patches().items() if u.get('status') == 'needed'}:
                print(package)
                print(name)
                sys.exit(0)
            zypper('remove', '--clean-deps', package, check=False)
    sys.exit('no security patch with an older package that can be installed')
elif sys.argv[1] == 'end-of-the-release':
    # endoflife as a child of product, not the one of codestream.
    print(ET.parse('/etc/products.d/baseproduct').getroot().find('endoflife').text.strip())
'''
# The program on a guest of the Red Hat family that reads dnf's own answers for the test, independently of the checks.
DNF_HELPER = FIXTURE + '/dnf_helper.py'
DNF_HELPER_TEXT = r'''import configparser, glob, json, os, re, subprocess, sys


def command(*arguments, check=True):
    done = subprocess.run(arguments, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env={'PATH': '/usr/sbin:/usr/bin:/sbin:/bin', 'LC_ALL': 'C'})
    if check and done.returncode != 0:
        sys.exit('%s: %s%s' % (' '.join(arguments), done.stdout, done.stderr))
    return done


# dnf 5 (Fedora) is the program dnf5, and dnf is a link to it; its cache is /var/cache/libdnf5.
FIVE = os.path.basename(os.path.realpath('/usr/bin/dnf')) == 'dnf5'
CACHE = '/var/cache/libdnf5' if FIVE else '/var/cache/dnf'
REPOSITORY_FILES = ['/etc/yum.repos.d/*.repo', '/usr/share/dnf5/repos.d/*.repo'] if FIVE else ['/etc/yum.repos.d/*.repo']


def enabled_repositories():
    # The ids of the enabled repositories. dnf 5 lists them itself, wherever their files are.
    if FIVE:
        return sorted(repository['id'] for repository in json.loads(command('dnf', '--quiet', '--cacheonly', 'repo', 'list', '--enabled', '--json').stdout))
    # dnf 4: read from the repository files by configparser; a repository without enabled= is enabled.
    parser = configparser.ConfigParser(interpolation=None, strict=False)
    for path in sorted(glob.glob('/etc/yum.repos.d/*.repo')):
        parser.read(path)
    return sorted(section for section in parser.sections() if parser[section].get('enabled', '1').lower() in ('1', 'true', 'yes', 'on'))


def cache_directories(repository):
    return sorted(name for name in os.listdir(CACHE) if re.fullmatch(re.escape(repository) + '-[0-9a-f]{16}', name))


def waiting():
    # The packages with a security update that dnf would install, read as root with dnf's own listing.
    listed = command('dnf', '--quiet', '--cacheonly', 'updateinfo', 'list', '--security', '--updates').stdout
    names = set()
    for line in listed.splitlines():
        # dnf 5 writes a header and then: advisory, type, severity, package, date, time.
        if line.strip() and not (FIVE and line.startswith('Name ')):
            names.add(re.fullmatch(r'(.+)-(?:\d+:)?[^-]+-[^-]+\.[A-Za-z0-9_]+', line.split()[3 if FIVE else 2]).group(1))
    return sorted(names)


if sys.argv[1] == 'repositories':
    print('\n'.join(enabled_repositories()))
elif sys.argv[1] == 'directories':
    # Each enabled repository has a cache directory of its id with a hash; print "id directory" for each.
    for repository in enabled_repositories():
        directories = cache_directories(repository)
        assert directories, repository
        for directory in directories:
            print(repository, directory)
elif sys.argv[1] == 'hosts':
    text = ' '.join(open(path).read() for pattern in REPOSITORY_FILES for path in glob.glob(pattern))
    # Every server that a repository names, also the second server of a baseurl.
    print('\n'.join(sorted(set(re.findall(r'https?://([^/\s$]+)', text)))))
elif sys.argv[1] == 'waiting':
    print('\n'.join(waiting()))
elif sys.argv[1] == 'waiting-advisories':
    # The security advisories with an update that dnf would install, read as root with dnf's own
    # listing: the check reports one finding for each.
    listed = command('dnf', '--quiet', '--cacheonly', 'updateinfo', 'list', '--security', '--updates').stdout
    print('\n'.join(sorted({line.split()[0] for line in listed.splitlines() if line.strip() and not (FIVE and line.startswith('Name '))})))
elif sys.argv[1] == 'downgrade-a-package':
    # From the packages with a security advisory whose update is installed, take
    # one of which the repositories hold an older version, downgrade it to the
    # version before, and keep it when the advisory then waits for it.
    listed = command('dnf', '--quiet', '--cacheonly', 'updateinfo', 'list', '--security', '--installed').stdout
    names = sorted({re.fullmatch(r'(.+)-(?:\d+:)?[^-]+-[^-]+\.[A-Za-z0-9_]+', line.split()[3 if FIVE else -1]).group(1) for line in listed.splitlines() if len(line.split()) == (6 if FIVE else 3)})
    avoid = ('kernel', 'glibc', 'systemd', 'dnf', 'libdnf', 'python', 'rpm', 'selinux', 'openssh', 'NetworkManager', 'grub', 'dracut', 'shim', 'cloud-init', 'openssl', 'libcurl', 'curl', 'sudo', 'bash', 'coreutils', 'util-linux', 'ca-certificates', 'crypto-policies', 'gnupg', 'libxcrypt', 'pam', 'incus')
    # Only a package of which the repositories hold an older version can be downgraded.
    versions = {}
    for line in command('dnf', '--quiet', '--cacheonly', 'list', '--showduplicates', '--available' if FIVE else 'available').stdout.splitlines():
        fields = line.split()
        if len(fields) == 3:
            versions.setdefault(fields[0].rsplit('.', 1)[0], set()).add(fields[1])
    tried = 0
    for name in names:
        if name.startswith(avoid) or len(versions.get(name, ())) < 2:
            continue
        tried += 1
        if tried > 25:
            break
        if command('dnf', '--quiet', '--assumeyes', 'downgrade', name, check=False).returncode == 0 and name in waiting():
            print(name)
            sys.exit(0)
        command('dnf', '--quiet', '--assumeyes', 'upgrade', name, check=False)
    sys.exit('no package with a security advisory that can be downgraded so that the advisory waits (tried %d)' % tried)
'''
FRAGMENT = '/etc/mmwatchdog/check_groups/node_scenarios.yml'
# A filesystem that is mounted for the test of a filesystem that must be
# mounted, and three small filesystems on loop devices for the test of every
# mounted filesystem. vm.reset unmounts them after an interruption.
MOUNTED = FIXTURE + '/must-be-mounted'
FILESYSTEMS = {name: FIXTURE + '/filesystem-' + name for name in ['filling', 'read-only', 'ignored']}
# A network interface without a cable, which the test turns on and off.
INTERFACE = 'mmwatchdog0'
UNIT = 'mmwatchdog-two-programs.service'
# The transient units of the scenarios, which vm.reset stops after an
# interruption, and the time services that the clock scenario stops.
TRANSIENT_UNITS = ['mmwatchdog-holds-memory', 'mmwatchdog-uses-cpu', 'mmwatchdog-listens', 'mmwatchdog-out-of-memory', 'mmwatchdog-writes-journal', 'mmwatchdog-test-cisa-and-osv']
TIME_SERVICES = 'systemd-timesyncd.service chrony.service chronyd.service'
ALERT_DESTINATIONS = '/etc/mmwatchdog/alert_destinations.yml'
# The stand-in for CISA's list and OSV.dev of the known exploited flaws
# check, the name of its certificate in the guest's trust store, the line
# in /etc/hosts that sends both host names to it, and a vendor's apt
# repository.
CISA_AND_OSV_DIR = FIXTURE + '/cisa-and-osv'
CISA_AND_OSV_CERTIFICATE_NAME = 'mmwatchdog-test-cisa-and-osv'
CISA_AND_OSV_HOSTS_LINE = '127.0.0.1 raw.githubusercontent.com api.osv.dev # mmwatchdog-test-cisa-and-osv'
VENDOR_REPOSITORY_FILE = '/etc/apt/sources.list.d/mmwatchdog-test-vendor.list'
# The software RAID array of the test, on two loop devices.
ARRAY = '/dev/md/mmwatchdogtest'
# The kernel hooks of Debian and Ubuntu that write /run/reboot-required for
# a new kernel, which the reboot_after_updates check looks for.
REBOOT_REQUIRED_HOOKS = ['/etc/kernel/postinst.d/unattended-upgrades', '/etc/kernel/postinst.d/update-notifier',
                         '/etc/kernel/postinst.d/reboot-notifier', '/usr/share/kernel/postinst.d/touch-reboot-required']
# A directory that the guest exports over NFS to itself, and the place where
# the guest mounts it: the stand-in for a filesystem of another machine. The
# mount is outside the fixture directory, so nothing that removes the
# fixture walks into a filesystem that does not answer.
EXPORTED = FIXTURE + '/exported-over-nfs'
NETWORK_FILESYSTEM = '/mnt/mmwatchdog-network-filesystem'
EXPORTS = '/etc/exports.d/mmwatchdog-test.exports'
# Sets the largest error that the clock can have, in microseconds, as the
# kernel keeps it: at 16 seconds the kernel counts the clock as not
# synchronized. A time service sets the same number each time it sets the
# clock.
SET_LARGEST_CLOCK_ERROR = """import ctypes, sys
class timeval(ctypes.Structure):
    _fields_ = [('seconds', ctypes.c_long), ('microseconds', ctypes.c_long)]
class timex(ctypes.Structure):
    _fields_ = [('modes', ctypes.c_uint), ('offset', ctypes.c_long), ('freq', ctypes.c_long), ('maxerror', ctypes.c_long), ('esterror', ctypes.c_long),
                ('status', ctypes.c_int), ('constant', ctypes.c_long), ('precision', ctypes.c_long), ('tolerance', ctypes.c_long), ('time', timeval),
                ('tick', ctypes.c_long), ('ppsfreq', ctypes.c_long), ('jitter', ctypes.c_long), ('shift', ctypes.c_int), ('stabil', ctypes.c_long),
                ('jitcnt', ctypes.c_long), ('calcnt', ctypes.c_long), ('errcnt', ctypes.c_long), ('stbcnt', ctypes.c_long), ('tai', ctypes.c_int),
                ('padding', ctypes.c_int * 11)]
ADJ_MAXERROR = 0x0004
state = timex(modes=ADJ_MAXERROR, maxerror=int(sys.argv[1]))
if ctypes.CDLL(None, use_errno=True).adjtimex(ctypes.byref(state)) < 0:
    raise OSError(ctypes.get_errno(), 'adjtimex')
"""


def suite(cli, status, state, events, received_by_webhook, wait, mark):
    quick = {'check_every': '1s', 'time_limit_per_run': '700ms', 'alert_after_failing_for': '0s', 'recovered_after_healthy_for': '0s'}
    groups = {}

    def reload():
        vm.write(FRAGMENT, json.dumps({'check_groups': groups}, indent=2))
        cli('reload', 0)

    def last(name):
        return state(name)['last_observation']

    def reports():
        """The alerts that the webhook of the suite has received, oldest first."""
        return [received['event'] for received in received_by_webhook() if received.get('event')]

    def now(group):
        """One run of a check group with mmwatchdog check-now, by the name of each check."""
        return json.loads(cli('check-now ' + group + ' --json').stdout)

    try:
        # The further resources of the node: the time that programs wait
        # for a disk, the memory pressure, and the open files.
        groups['io_wait'] = dict(quick, node_resource='io_wait', only_record_readings=True)
        groups['open_files'] = dict(quick, node_resource='open_files', fail_when_measurement_above={'open_files_used_percent': 99}, alert_severity='warn')
        groups['memory_pressure'] = dict(quick, node_resource='memory_pressure', only_record_readings=True)
        reload()
        wait(lambda: last('io_wait/node_resource').get('measurements') and last('open_files/node_resource').get('measurements'))
        io_wait = last('io_wait/node_resource')
        assert io_wait['measurements']['io_wait_percent']['unit'] == 'percent' and 0 <= io_wait['measurements']['io_wait_percent']['value'] <= 100, io_wait
        assert io_wait['message'].startswith('io_wait_percent is ') and ' over ' in io_wait['message'] and io_wait['sample_window_ns'] > 0, io_wait
        # The kernel's own numbers, read just before and just after the check.
        before, measured, after = vm.console('cat /proc/sys/fs/file-nr; echo ---; /usr/local/bin/mmwatchdog check-now open_files --json; echo ---; cat /proc/sys/fs/file-nr').stdout.split('---')
        used = [100 * (int(allocated) - int(unused)) / int(limit) for allocated, unused, limit in (text.split() for text in (before, after))]
        open_files = json.loads(measured)['node_resource']
        assert open_files['status'] == 'ok' and min(used) - 0.5 <= open_files['measurements']['open_files_used_percent']['value'] <= max(used) + 0.5, (open_files, used)
        assert ' (fails above 99)' in open_files['message'], open_files
        pressure = last('memory_pressure/node_resource')
        if vm.console('cat /proc/pressure/memory', check=False).returncode == 0:
            assert pressure['status'] == 'ok' and 0 <= pressure['measurements']['memory_pressure_percent']['value'] <= 100, pressure
        else:
            assert pressure['status'] == 'unknown' and pressure['message'] == 'the kernel of this machine does not provide the memory pressure (/proc/pressure/memory)', pressure
        mark('the I/O wait, the open files against the kernel\'s own numbers, and the memory pressure of the node are measured, each under a name that carries its unit')

        # A disk device: the device that holds the root filesystem. Writing
        # 64 MiB to it shows in the data written.
        # --nofsroot: findmnt adds the subvolume to the device of a btrfs
        # filesystem, which a node can have, and lsblk knows the device alone.
        device = vm.console('lsblk -ndo PKNAME "$(findmnt --nofsroot -no SOURCE /)"').stdout.strip() or vm.console('basename "$(findmnt --nofsroot -no SOURCE /)"').stdout.strip()
        groups['disk_device'] = dict(quick, check_every='2s', node_disk_device='/dev/' + device, only_record_readings=True)
        groups['no_such_disk'] = dict(quick, node_disk_device='/dev/mmwatchdog-no-such-disk', only_record_readings=True)
        reload()
        wait(lambda: last('disk_device/node_disk_device').get('measurements'))
        disk = last('disk_device/node_disk_device')
        assert set(disk['measurements']) == {'disk_device_busy_percent', 'read_mib_per_second', 'write_mib_per_second'}, disk
        assert disk['measurements']['disk_device_busy_percent']['unit'] == 'percent' and disk['measurements']['write_mib_per_second']['unit'] == 'MiB per second', disk
        assert disk['message'].startswith('disk_device_busy_percent is ') and '; read_mib_per_second is ' in disk['message'] and ' over ' in disk['message'], disk
        vm.console('dd if=/dev/zero of=' + FIXTURE + '/written bs=1M count=64 oflag=direct status=none')
        wait(lambda: any(json.loads(m)['write_mib_per_second']['value'] > 5 for (m,) in history_rows("SELECT measurements FROM readings WHERE check_group='disk_device' ORDER BY id DESC LIMIT 12")))
        missing = last('no_such_disk/node_disk_device')
        assert missing['status'] == 'unknown' and missing['message'].startswith('MMWatchdog cannot resolve the device path /dev/mmwatchdog-no-such-disk: '), missing
        mark('a disk device\'s busy time and its data read and written are measured from the kernel\'s counters, a write of 64 MiB shows in the readings, and a device that does not exist is unknown')

        # A network interface: the one that carries the default route, and a
        # dummy interface that the test turns off and on.
        uplink = vm.console("ip -o route show default | sed -n 's/.* dev \\([^ ]*\\).*/\\1/p' | head -1").stdout.strip()
        vm.console('ip link add ' + INTERFACE + ' type dummy; ip link set ' + INTERFACE + ' up')
        groups['uplink'] = dict(quick, node_network_interface=uplink, only_record_readings=True)
        groups['dummy'] = dict(quick, node_network_interface=INTERFACE, alert_severity='sos')
        groups['dummy_recorded'] = dict(quick, node_network_interface=INTERFACE, only_record_readings=True)
        groups['no_such_interface'] = dict(quick, node_network_interface='nosuch0', only_record_readings=True)
        reload()
        wait(lambda: last('uplink/node_network_interface').get('measurements') and last('dummy/node_network_interface')['status'] == 'ok')
        traffic = last('uplink/node_network_interface')
        assert {'received_mbit_per_second', 'sent_mbit_per_second'} <= set(traffic['measurements']) and traffic['measurements']['sent_mbit_per_second']['unit'] == 'Mbit per second', traffic
        # The interface of a virtual machine may report no speed: then the
        # measurement is left out, and never zero.
        speed = vm.console('cat /sys/class/net/' + uplink + '/speed', check=False)
        if speed.returncode == 0 and speed.stdout.strip().isdigit() and int(speed.stdout) > 0:
            assert traffic['measurements']['link_speed_mbit_per_second'] == {'value': int(speed.stdout), 'unit': 'Mbit per second'}, traffic
        else:
            assert 'link_speed_mbit_per_second' not in traffic['measurements'], traffic
        vm.console('ip link set ' + INTERFACE + ' down')
        wait(lambda: state('dummy/node_network_interface')['failure_incident'].get('incident_id'))
        # A guest that stalls for a moment misses one run of a check that
        # runs every second: wait for the run that shows the link.
        down = wait(lambda: last('dummy/node_network_interface')['status'] == 'failed' and last('dummy/node_network_interface'))
        assert down['message'].startswith('network interface ' + INTERFACE + ' has no link'), down
        recorded = state('dummy_recorded/node_network_interface')
        assert recorded['last_observation']['status'] == 'ok' and recorded['last_observation']['message'].startswith('network interface ' + INTERFACE + ' has no link; ') and not recorded['failure_incident'].get('incident_id'), recorded
        vm.console('ip link set ' + INTERFACE + ' up')
        wait(lambda: not state('dummy/node_network_interface')['failure_incident'].get('incident_id'))
        gone = last('no_such_interface/node_network_interface')
        assert gone['status'] == 'unknown' and gone['message'] == 'network interface nosuch0 does not exist', gone
        mark('a network interface\'s traffic and link speed are measured; an interface without a link fails its check that states an alert_severity and recovers with the link, a check that only records notes it, and an interface that does not exist is unknown')

        # A filesystem that must be mounted. Nothing mounted at its path
        # fails a check that states an alert_severity, and MMWatchdog never reports
        # the numbers of the filesystem that holds the empty directory.
        vm.console('mkdir -p ' + MOUNTED + '; mount -t tmpfs -o size=1m tmpfs ' + MOUNTED)
        groups['must_be_mounted'] = dict(quick, node_filesystem=MOUNTED, alert_severity='sos')
        groups['mounted_recorded'] = dict(quick, node_filesystem=MOUNTED, only_record_readings=True)
        reload()
        wait(lambda: last('must_be_mounted/node_filesystem')['status'] == 'ok' and last('mounted_recorded/node_filesystem').get('measurements'))
        vm.console('umount ' + MOUNTED)
        wait(lambda: state('must_be_mounted/node_filesystem')['failure_incident'].get('incident_id'))
        unmounted = last('must_be_mounted/node_filesystem')
        assert unmounted['status'] == 'failed' and unmounted['message'] == 'no filesystem is mounted at ' + MOUNTED and not unmounted.get('measurements'), unmounted
        wait(lambda: last('mounted_recorded/node_filesystem')['status'] == 'unknown')
        recorded = state('mounted_recorded/node_filesystem')
        assert recorded['last_observation']['message'] == 'no filesystem is mounted at ' + MOUNTED and not recorded['last_observation'].get('measurements') and not recorded['cannot_check_incident'].get('incident_id'), recorded
        vm.console('mount -t tmpfs -o size=1m tmpfs ' + MOUNTED)
        wait(lambda: not state('must_be_mounted/node_filesystem')['failure_incident'].get('incident_id') and last('mounted_recorded/node_filesystem')['status'] == 'ok')
        mark('a node_filesystem check that states an alert_severity fails while nothing is mounted at its path and recovers when the filesystem is mounted again; a check that only records is unknown there and records no reading of another filesystem')

        # Every mounted filesystem. Three small filesystems on loop devices
        # stand for filesystems on disk devices: one fills up, one is
        # remounted read-only, and one is in filesystems_to_ignore.
        for name, path in FILESYSTEMS.items():
            image = FIXTURE + '/image-' + name
            vm.console('truncate -s 32M ' + image + '; mkfs.ext4 -q -F ' + image + '; mkdir -p ' + path + '; mount -o loop ' + image + ' ' + path)
        every = 'filesystems/every_filesystem_under_80_percent'
        groups['filesystems'] = dict(quick, check_every='2s', time_limit_per_run='1500ms', checks={'every_filesystem_under_80_percent': {
            'every_disk_and_network_filesystem': {'filesystems_to_ignore': [FILESYSTEMS['ignored']]}, 'fail_when_measurement_above': {'space_used_percent': 80}, 'alert_severity': 'warn'}})
        reload()
        wait(lambda: last(every)['status'] == 'ok')
        healthy = last(every)
        assert ' within the limits (not counting 1 in filesystems_to_ignore: ' + FILESYSTEMS['ignored'] + ')' in healthy['message'] and not healthy.get('findings') and not healthy.get('measurements'), healthy
        # The filesystems of the guest itself are covered: its root
        # filesystem is one of those within the limits.
        covered = int(healthy['message'].split()[0])
        # --nofsroot: a btrfs filesystem with subvolumes, which a node can
        # have, is one filesystem that is mounted at several paths.
        on_disk = int(vm.console("findmnt --nofsroot -rno SOURCE,FSTYPE | awk '$1 ~ /^\\/dev\\// && $2 !~ /^(squashfs|iso9660|udf|erofs)$/ {print $1}' | sort -u | wc -l").stdout)
        assert covered == on_disk - 1 and covered >= 3, (healthy, on_disk)
        seen = len(reports())
        for name in ['filling', 'ignored']:
            vm.console('dd if=/dev/zero of=' + FILESYSTEMS[name] + '/data bs=1M count=27 status=none; sync')
        wait(lambda: state(every)['findings'].get(FILESYSTEMS['filling'], {}).get('failure_incident', {}).get('incident_id'))
        vm.console('mount -o remount,ro ' + FILESYSTEMS['read-only'])
        wait(lambda: state(every)['findings'].get(FILESYSTEMS['read-only'], {}).get('failure_incident', {}).get('incident_id'))
        failing = state(every)
        assert set(failing['findings']) == {FILESYSTEMS['filling'], FILESYSTEMS['read-only']} and not failing['failure_incident'].get('incident_id') and failing['one_incident_for_each_finding'], failing
        assert failing['last_observation']['message'].startswith('2 filesystems fail: ' + FILESYSTEMS['filling'] + ', ' + FILESYSTEMS['read-only']), failing
        messages = {finding['id']: finding['message'] for finding in failing['last_observation']['findings']}
        assert messages[FILESYSTEMS['filling']].startswith(FILESYSTEMS['filling'] + ': space_used_percent is ') and messages[FILESYSTEMS['filling']].endswith(' (fails above 80)'), messages
        assert messages[FILESYSTEMS['read-only']].startswith(FILESYSTEMS['read-only'] + ' is mounted read-only; space_used_percent is '), messages
        def of_the_filesystems(event_type):
            return [e for e in reports()[seen:] if e.get('check_group') == 'filesystems' and e['type'] == event_type]
        alerts = wait(lambda: len(of_the_filesystems('failed')) >= 2 and of_the_filesystems('failed'))
        assert sorted(e['finding'] for e in alerts) == sorted([FILESYSTEMS['filling'], FILESYSTEMS['read-only']]), alerts
        assert all(e['title'].endswith('filesystems/every_filesystem_under_80_percent: ' + e['finding'] + ' failed') for e in alerts), alerts
        # Each filesystem recovers by itself.
        vm.console('rm ' + FILESYSTEMS['filling'] + '/data; sync')
        wait(lambda: set(state(every)['findings']) == {FILESYSTEMS['read-only']})
        recovered = wait(lambda: of_the_filesystems('recovered'))
        assert [e['finding'] for e in recovered] == [FILESYSTEMS['filling']] and recovered[0]['message'] == 'the filesystem at ' + FILESYSTEMS['filling'] + ' no longer fails this check', recovered
        vm.console('mount -o remount,rw ' + FILESYSTEMS['read-only'])
        wait(lambda: not state(every).get('findings') and last(every)['status'] == 'ok')
        # mmwatchdog validate warns about a filesystem that has a check of its own.
        groups['own'] = dict(quick, node_filesystem=FILESYSTEMS['filling'], fail_when_measurement_above={'space_used_percent': 95}, alert_severity='sos')
        vm.write(FRAGMENT, json.dumps({'check_groups': groups}, indent=2))
        warned = cli('validate', 0).stdout
        assert 'An every_disk_and_network_filesystem check alerts for every filesystem beyond the check\'s limits and outside the check\'s filesystems_to_ignore list.' in warned, warned
        assert '  ' + every + ': ' + FILESYSTEMS['filling'] + ' is missing from filesystems_to_ignore; own/node_filesystem also checks the filesystem\n' in warned, warned
        del groups['own'], groups['filesystems']
        reload()
        mark('an every_disk_and_network_filesystem check covers the filesystems of the guest that are on disk devices without a list of them; a filesystem that fills up and one that is remounted read-only each get their own alert and their own recovery, an ignored filesystem that is full sends nothing, and mmwatchdog validate warns about a filesystem that also has a check of its own')

        # One program of a unit, and the number of its processes. The unit
        # runs a shell with two sleeping programs.
        # The shell runs a script file, so its own command line does not
        # hold the text that the check looks for.
        vm.write(FIXTURE + '/two_programs.sh', 'sleep 86401 &\nsleep 86402 &\nwait\n')
        vm.write('/etc/systemd/system/' + UNIT, '''[Unit]
Description=MMWatchdog fixture of a unit that runs two programs
[Service]
ExecStart=/bin/sh /var/tmp/mmwatchdog-fixture/two_programs.sh
KillMode=control-group
''')
        vm.console('systemctl daemon-reload; systemctl start ' + UNIT)
        groups['programs'] = dict(quick, checks={
            'unit_is_running': {'systemd_unit_state': {'systemd_unit': UNIT, 'fail_when': 'the_unit_is_not_active'}, 'alert_severity': 'sos'},
            # The check of the program waits two seconds before it alerts, so
            # the check of the unit has seen a stop of the unit by then.
            'first_program_is_running': {'systemd_unit_commands': {'systemd_unit': UNIT, 'fail_when_no_command_contains': 'sleep 86401'}, 'alert_severity': 'sos',
                                         'alert_after_failing_for': '2s',
                                         'no_alerts_or_recovery_actions_while_these_checks_fail': ['programs/unit_is_running']},
            'processes': {'only_record_readings': True, 'systemd_unit_resource': {'systemd_unit': UNIT, 'resource': 'processes'}}})
        reload()
        wait(lambda: last('programs/first_program_is_running')['status'] == 'ok' and last('programs/processes').get('measurements'))
        running = last('programs/first_program_is_running')
        process = vm.console("pgrep --exact --full 'sleep 86401'").stdout.strip()
        assert running['message'] == 'systemd unit ' + UNIT + ' runs sleep 86401 (process ' + process + ')', (running, process)
        assert last('programs/processes')['measurements']['process_count'] == {'value': 3, 'unit': 'processes'}, last('programs/processes')
        seen = len(reports())
        vm.console('kill ' + process)
        wait(lambda: state('programs/first_program_is_running')['failure_incident'].get('incident_id'))
        ended = state('programs/first_program_is_running')['failure_incident']['evidence']['observation']
        assert ended['message'] == 'systemd unit ' + UNIT + ' runs no command that contains sleep 86401', ended
        assert ended['details'][0] == 'the commands that the unit runs:' and any(line.startswith('  sleep 86402 (process ') for line in ended['details']) and len(ended['details']) == 3, ended
        alert = wait(lambda: [e for e in reports()[seen:] if e.get('check') == 'first_program_is_running' and e['type'] == 'failed'])[0]
        assert '\nthe commands that the unit runs:\n  ' in alert['body'] and '\n  sleep 86402 (process ' in alert['body'], alert
        wait(lambda: last('programs/processes')['measurements']['process_count']['value'] == 2)
        assert last('programs/unit_is_running')['status'] == 'ok'
        vm.console('systemctl restart ' + UNIT)
        wait(lambda: not state('programs/first_program_is_running')['failure_incident'].get('incident_id'))
        # The alert of the recovery arrives a moment after the check's
        # state shows it. What follows counts the alerts from here on.
        wait(lambda: [e for e in reports()[seen:] if e.get('check') == 'first_program_is_running' and e['type'] == 'recovered'])
        # A stopped unit runs no command. The check of the unit reports the
        # stop, and the check of the program stays quiet.
        seen = len(reports())
        vm.console('systemctl stop ' + UNIT)
        wait(lambda: state('programs/unit_is_running')['failure_incident'].get('incident_id') and last('programs/first_program_is_running')['status'] == 'failed')
        stopped = last('programs/first_program_is_running')
        assert stopped['message'] == 'systemd unit ' + UNIT + ' runs no command that contains sleep 86401: systemd unit ' + UNIT + ' is inactive', stopped
        wait(lambda: last('programs/processes')['status'] == 'unknown')
        time.sleep(4)
        assert not [e for e in reports()[seen:] if e.get('check') == 'first_program_is_running'], reports()[seen:]
        del groups['programs']
        reload()
        mark('a systemd_unit_commands check is healthy while its unit runs a command that contains the text, fails when that program is killed with the unit\'s other commands in the alert, fails quietly for a stopped unit whose own check reports the stop, and systemd_unit_resource counts the unit\'s processes')

        # The commands that use the most of a resource, in the alert of a
        # check of the node. A program that holds 96 MiB is among those that
        # use the most memory, and one that keeps a processor busy leads
        # those that used the most CPU. The program that holds memory leaves
        # a file once all of it is written, and the checks start after that:
        # an alert of a program that still writes its memory names less.
        vm.write(FIXTURE + '/holds_memory.py', 'import time\nheld = bytearray(96 * 1024 * 1024)\nfor i in range(0, len(held), 4096): held[i] = 1\n'
                 'open(' + repr(FIXTURE + '/holds_memory.ready') + ', "w").close()\ntime.sleep(86400)\n')
        vm.write(FIXTURE + '/uses_cpu.py', 'while True: pass\n')
        vm.console('rm -f ' + FIXTURE + '/holds_memory.ready; systemd-run --quiet --unit mmwatchdog-holds-memory python3 ' + FIXTURE + '/holds_memory.py; systemd-run --quiet --unit mmwatchdog-uses-cpu python3 ' + FIXTURE + '/uses_cpu.py; '
                   'for attempt in $(seq 300); do [ -e ' + FIXTURE + '/holds_memory.ready ] && exit 0; sleep 0.1; done; exit 1')
        groups['commands'] = dict(quick, check_every='2s', time_limit_per_run='1500ms', checks={
            'memory': {'node_resource': 'memory', 'fail_when_measurement_above': {'memory_used_percent': 1}, 'alert_severity': 'info'},
            'cpu': {'node_resource': 'cpu', 'fail_when_measurement_above': {'cpu_used_percent': 1}, 'alert_severity': 'info'},
            'load': {'node_resource': 'load', 'fail_when_measurement_above': {'load_average_1m': 0}, 'alert_severity': 'info'},
            'memory_within_its_limit': {'node_resource': 'memory', 'fail_when_measurement_above': {'memory_used_percent': 100}, 'alert_severity': 'info'}})
        seen = len(reports())
        reload()
        wait(lambda: all(state('commands/' + check)['failure_incident'].get('incident_id') for check in ['memory', 'cpu']), timeout=40)
        # The alert names each program and its process ID, never its arguments,
        # which can hold a password: the scenario finds its programs by their IDs.
        holder = vm.console('systemctl show --value -p MainPID mmwatchdog-holds-memory').stdout.strip()
        busy = vm.console('systemctl show --value -p MainPID mmwatchdog-uses-cpu').stdout.strip()
        memory = state('commands/memory')['failure_incident']['evidence']['observation']['details']
        assert memory[0] == 'the commands that use the most memory:' and len(memory) == 6, memory
        amounts = [int(line.split()[0]) for line in memory[1:]]
        assert amounts == sorted(amounts, reverse=True) and all(' MiB: ' in line and ' (process ' in line for line in memory[1:]), memory
        assert any(line.endswith(': python3 (process ' + holder + ')') and int(line.split()[0]) >= 96 for line in memory[1:]), (holder, memory)
        assert not any(FIXTURE in line for line in memory), memory
        # A later run of the CPU check covers the whole time since the run
        # before, in which the program was busy all the time.
        def cpu_details():
            details = last('commands/cpu').get('details') or ['', '']
            return details if details[1].endswith(': python3 (process ' + busy + ')') else None
        cpu = wait(cpu_details)
        assert cpu[0] == 'the commands that used the most CPU between the two CPU readings:' and cpu[1].split()[1] == 'percent:' and float(cpu[1].split()[0]) > 10, cpu
        body = wait(lambda: [e for e in reports()[seen:] if e.get('check_group') == 'commands' and e.get('check') == 'memory' and e['type'] == 'failed'])[0]['body']
        assert '\nthe commands that use the most memory:\n  ' in body and 'python3 (process ' + holder + ')' in body and FIXTURE not in body, body
        load = now('commands')['load']
        if load['status'] == 'failed':
            assert load['details'][0] == 'the commands with the most threads that run or wait for a disk now:' and any(line.endswith('(process ' + busy + ')') for line in load['details'][1:]), load
        assert not last('commands/memory_within_its_limit').get('details'), last('commands/memory_within_its_limit')
        shown = cli('status commands').stdout
        assert '  the commands that use the most memory:\n' in shown, shown
        mark('the alert of a failed check of the node\'s memory or CPU names in its body the programs that use the most of that resource, by name and process ID without their arguments, the highest first, with a program that holds 96 MiB and one that keeps a processor busy among them; a check within its limit names none')
        vm.console('systemctl stop mmwatchdog-holds-memory mmwatchdog-uses-cpu')
        del groups['commands']

        # A Unix socket: a program listens on it, is killed and leaves its
        # socket file behind, and the file is removed.
        socket_path = FIXTURE + '/app.sock'
        vm.write(FIXTURE + '/listens.py', 'import socket, time\nlistener = socket.socket(socket.AF_UNIX)\nlistener.bind(' + repr(socket_path) + ')\nlistener.listen()\ntime.sleep(86400)\n')
        vm.console('rm -f ' + socket_path + '; systemd-run --quiet --unit mmwatchdog-listens python3 ' + FIXTURE + '/listens.py')
        groups['socket'] = dict(quick, unix_socket=socket_path, alert_severity='sos')
        reload()
        wait(lambda: last('socket/unix_socket')['status'] == 'ok')
        assert last('socket/unix_socket')['message'] == 'Unix socket connection accepted'
        vm.console('systemctl kill --signal=KILL mmwatchdog-listens; test -S ' + socket_path)
        wait(lambda: last('socket/unix_socket')['status'] == 'failed')
        assert last('socket/unix_socket')['message'] == 'no program accepts connections on the Unix socket ' + socket_path, last('socket/unix_socket')
        vm.console('rm ' + socket_path)
        wait(lambda: last('socket/unix_socket')['message'] == socket_path + ' does not exist')
        del groups['socket']
        mark('a unix_socket check is healthy while a program accepts connections on the socket, and fails for a socket file that the program left behind and for a missing socket file, each with its own message')

        # Ping: the guest answers itself, over IP version 4 and 6. An address
        # on the network of the dummy interface never answers.
        vm.console('ip addr add 10.203.0.1/24 dev ' + INTERFACE)
        groups['ping'] = dict(quick, checks={
            'itself': {'ping': '127.0.0.1', 'fail_when_measurement_above': {'round_trip_milliseconds': 500}, 'alert_severity': 'warn'},
            'itself_over_ip_version_6': {'ping': '::1', 'alert_severity': 'warn'},
            'itself_by_name': {'ping': 'localhost', 'alert_severity': 'warn'},
            'nobody': {'ping': '10.203.0.2', 'alert_severity': 'warn'},
            'no_such_name': {'ping': 'no-such-machine.invalid', 'alert_severity': 'warn'}})
        reload()
        wait(lambda: all(last('ping/' + check)['status'] != 'unknown' and last('ping/' + check).get('message') for check in groups['ping']['checks']))
        itself = last('ping/itself')
        assert itself['status'] == 'ok' and itself['measurements']['round_trip_milliseconds']['unit'] == 'milliseconds' and 0 < itself['measurements']['round_trip_milliseconds']['value'] < 500, itself
        assert itself['message'].startswith('round_trip_milliseconds is ') and ' (fails above 500); 127.0.0.1 answered a ping request in ' in itself['message'], itself
        assert last('ping/itself_over_ip_version_6')['status'] == 'ok' and last('ping/itself_over_ip_version_6')['message'].startswith('::1 answered a ping request in '), last('ping/itself_over_ip_version_6')
        assert last('ping/itself_by_name')['status'] == 'ok' and last('ping/itself_by_name')['message'].startswith('localhost ('), last('ping/itself_by_name')
        nobody = last('ping/nobody')
        assert nobody['status'] == 'failed' and nobody['message'] == '10.203.0.2 did not answer 3 ping requests within 700ms (time_limit_per_run)' and not nobody.get('measurements'), nobody
        assert last('ping/no_such_name')['status'] == 'failed' and last('ping/no_such_name')['message'].startswith('MMWatchdog cannot find the address of no-such-machine.invalid: '), last('ping/no_such_name')
        # The node has no ping program that MMWatchdog could have run.
        del groups['ping']
        mark('a ping check gets an answer from the guest itself over IP version 4 and 6 and by name, with the round trip time as a measurement that takes a limit, and fails for an address that never answers and for a name without an address')

        # The clock. The check agrees with timedatectl. With the time
        # services stopped, the largest error of the clock is set by hand to
        # 16 seconds, at which the kernel counts the clock as not
        # synchronized, and back.
        vm.write(FIXTURE + '/set_largest_clock_error.py', SET_LARGEST_CLOCK_ERROR)
        groups['clock'] = dict(quick, node_clock={'fail_when': 'the_clock_is_not_synchronized'}, alert_severity='warn')
        reload()
        wait(lambda: last('clock/node_clock')['status'] in {'ok', 'failed'})
        running = vm.console('systemctl is-active ' + TIME_SERVICES + ' | grep -c "^active$"', check=False).stdout.strip()
        follows_a_time_server = vm.console('timedatectl show --property NTP --value').stdout.strip() == 'yes'
        vm.console(vm.each_unit('stop', TIME_SERVICES.split()))
        try:
            vm.console('python3 ' + FIXTURE + '/set_largest_clock_error.py 1000')
            wait(lambda: last('clock/node_clock')['status'] == 'ok')
            assert vm.console('timedatectl show --property NTPSynchronized --value').stdout.strip() == 'yes'
            assert last('clock/node_clock')['message'] == 'the clock of this machine follows a time server'
            vm.console('python3 ' + FIXTURE + '/set_largest_clock_error.py 16000000')
            wait(lambda: last('clock/node_clock')['status'] == 'failed')
            assert vm.console('timedatectl show --property NTPSynchronized --value').stdout.strip() == 'no'
            assert last('clock/node_clock')['message'] == 'the kernel reports that the clock of this machine does not follow a time server'
            vm.console('python3 ' + FIXTURE + '/set_largest_clock_error.py 1000')
            wait(lambda: last('clock/node_clock')['status'] == 'ok')
        finally:
            vm.console('for unit in ' + TIME_SERVICES + '; do if systemctl is-enabled --quiet $unit 2>/dev/null; then systemctl start $unit; fi; done')
        # A guest whose clock the machine below it keeps runs no time
        # service. A guest that follows a time server runs one that the
        # scenario stopped.
        assert running != '0' or not follows_a_time_server, 'the guest runs no time service that the scenario knows'
        del groups['clock']
        mark('a node_clock check gives the answer of timedatectl: healthy while the kernel\'s largest error of the clock is small, and failed once that error is 16 seconds')

        # An out-of-memory kill. A unit with a memory limit of 20 MiB runs a
        # program that takes 200 MiB, and the kernel ends that program.
        groups['out_of_memory'] = dict(quick, out_of_memory_kills={'fail_when': 'the_kernel_kills_a_process_for_lack_of_memory'}, alert_severity='warn')
        del groups['out_of_memory']['alert_after_failing_for']
        reload()
        wait(lambda: last('out_of_memory/out_of_memory_kills')['message'] == 'the kernel ended no process to free memory since the run before')
        seen = len(reports())
        vm.write(FIXTURE + '/takes_memory.py', 'import time\ntaken = b"x" * (200 * 1024 * 1024)\ntime.sleep(60)\n')
        vm.console('systemd-run --quiet --unit mmwatchdog-out-of-memory --property MemoryMax=20M --property MemorySwapMax=0 python3 ' + FIXTURE + '/takes_memory.py')
        killed = wait(lambda: [e for e in reports()[seen:] if e.get('check_group') == 'out_of_memory' and e['type'] == 'failed'])[0]
        assert killed['message'].startswith('the kernel ended python3 (process ') and killed['message'].endswith(' UTC to free memory, because its control group reached its memory limit'), killed
        assert vm.console('systemctl show --property Result --value mmwatchdog-out-of-memory').stdout.strip() == 'oom-kill'
        wait(lambda: [e for e in reports()[seen:] if e.get('check_group') == 'out_of_memory' and e['type'] == 'recovered'])
        vm.console('systemctl reset-failed mmwatchdog-out-of-memory 2>/dev/null; true')
        # The count survives a restart of MMWatchdog: a kill while MMWatchdog is stopped is
        # found by the first run after the start.
        seen = len(reports())
        vm.console('systemctl stop mmwatchdog; systemd-run --quiet --unit mmwatchdog-out-of-memory --property MemoryMax=20M --property MemorySwapMax=0 python3 ' + FIXTURE + '/takes_memory.py; '
                   'while [ "$(systemctl show --property Result --value mmwatchdog-out-of-memory)" != oom-kill ]; do sleep 0.2; done; systemctl reset-failed mmwatchdog-out-of-memory; systemctl start mmwatchdog')
        wait(lambda: [e for e in reports()[seen:] if e.get('check_group') == 'out_of_memory' and e['type'] == 'failed'])
        del groups['out_of_memory']
        mark('an out_of_memory_kills check fails in the run after the kernel ended a process of a unit that reached its memory limit, names the command, its process ID, the time, and the cause, recovers in the next run, and finds a kill that happened while MMWatchdog was stopped')

        # Files: a backup that must be fresh and not empty, a settings file
        # that must stay as it is, a directory of job files with its owner
        # and permissions, and a flag that must not exist.
        backup, settings_file, flag, jobs_directory = FIXTURE + '/database.sql.gz', FIXTURE + '/server_settings', FIXTURE + '/maintenance.flag', FIXTURE + '/jobs.d'
        vm.console('rm -f ' + backup + ' ' + flag + "; printf 'PermitRootLogin no\\n' > " + settings_file + '; chown root:root ' + settings_file + '; chmod 0600 ' + settings_file
                   + '; mkdir -p ' + jobs_directory + '; chown root:root ' + jobs_directory + '; chmod 0755 ' + jobs_directory)
        checksum = hashlib.sha256(b'PermitRootLogin no\n').hexdigest()
        groups['files'] = dict(quick, checks={
            'backup_is_fresh_and_not_empty': {'file': {'path': backup}, 'fail_when_measurement_above': {'file_age_hours': 26}, 'fail_when_measurement_below': {'file_size_mib': 1}, 'alert_severity': 'sos'},
            'settings_are_unchanged': {'file': {'path': settings_file, 'expected_owner': 'root', 'expected_group': 'root', 'expected_permissions_mode': '0600', 'expected_sha256_checksum': checksum}, 'alert_severity': 'warn'},
            'jobs_directory_is_as_expected': {'file': {'path': jobs_directory, 'expected_owner': 'root', 'expected_group': 'root', 'expected_permissions_mode': '0755'}, 'alert_severity': 'warn'},
            'flag_is_gone': {'file': {'path': flag, 'must_not_exist': True}, 'alert_severity': 'sos'}})
        reload()

        def file_check(name, status, message):
            """Wait until the check has the status and a message that starts with the text."""
            try:
                return wait(lambda: last('files/' + name)['status'] == status and last('files/' + name)['message'].startswith(message) and last('files/' + name))
            except AssertionError:
                raise AssertionError((name, status, message, last('files/' + name))) from None
        file_check('backup_is_fresh_and_not_empty', 'failed', backup + ' does not exist')
        vm.console('head -c 2097152 /dev/zero > ' + backup)
        fresh = file_check('backup_is_fresh_and_not_empty', 'ok', 'file_age_hours is 0 (fails above 26); file_size_mib is 2 (fails below 1); ' + backup + ' exists')
        assert fresh['measurements']['file_size_mib'] == {'value': 2, 'unit': 'MiB'} and set(fresh['measurements']) == {'file_age_minutes', 'file_age_hours', 'file_age_days', 'file_size_mib'}, fresh
        vm.console("touch -d '27 hours ago' " + backup)
        file_check('backup_is_fresh_and_not_empty', 'failed', 'file_age_hours is 27 (fails above 26); file_size_mib is 2 (fails below 1); ')
        vm.console('head -c 1024 /dev/zero > ' + backup)
        file_check('backup_is_fresh_and_not_empty', 'failed', 'file_age_hours is 0 (fails above 26); file_size_mib is 0 (fails below 1); ')
        assert file_check('settings_are_unchanged', 'ok', '')['message'] == settings_file + ' is as expected'
        vm.console('chmod 0644 ' + settings_file + '; chown mmwatchdog:mmwatchdog ' + settings_file)
        file_check('settings_are_unchanged', 'failed', settings_file + ' is owned by the user account mmwatchdog, expected root; ' + settings_file + ' belongs to the group mmwatchdog, expected root; ' + settings_file + ' has permissions mode 0644, expected 0600')
        vm.console('chmod 0600 ' + settings_file + '; chown root:root ' + settings_file + "; printf 'PermitRootLogin yes\\n' > " + settings_file)
        file_check('settings_are_unchanged', 'failed', settings_file + ' has the SHA-256 checksum ' + hashlib.sha256(b'PermitRootLogin yes\n').hexdigest() + ', expected ' + checksum)
        directory = file_check('jobs_directory_is_as_expected', 'ok', jobs_directory + ' is as expected')
        assert set(directory['measurements']) == {'file_age_minutes', 'file_age_hours', 'file_age_days'}, directory
        vm.console('chmod 0777 ' + jobs_directory)
        file_check('jobs_directory_is_as_expected', 'failed', jobs_directory + ' has permissions mode 0777, expected 0755')
        file_check('flag_is_gone', 'ok', flag + ' does not exist')
        # The system log destination gets the alert of the flag check and
        # its recovery, beside the webhook of the suite.
        original_destinations = vm.console('cat ' + ALERT_DESTINATIONS).stdout
        destinations = json.loads(original_destinations)
        destinations['destinations']['machine_log'] = {'remind_every': 'never', 'system_log': {}, 'alert_severities': ['info', 'warn', 'sos']}
        vm.write(ALERT_DESTINATIONS, json.dumps(destinations, indent=2))
        reload()
        started = vm.console('date +%s').stdout.strip()

        def logged():
            written = vm.console('journalctl --identifier mmwatchdog-alert --since @' + started + ' --output json --no-pager').stdout
            return [(entry['MESSAGE'], entry['PRIORITY']) for entry in map(json.loads, written.splitlines()) if 'files/flag_is_gone' in entry['MESSAGE']]
        vm.console('touch ' + flag)
        file_check('flag_is_gone', 'failed', flag + ' exists')
        node = vm.hostname()
        wait(lambda: logged() == [('SOS: ' + node + ': files/flag_is_gone failed', '2')])
        vm.console('rm ' + flag)
        wait(lambda: logged() == [('SOS: ' + node + ': files/flag_is_gone failed', '2'), ('Resolved SOS: ' + node + ': files/flag_is_gone is ok again', '6')])
        vm.write(ALERT_DESTINATIONS, original_destinations)
        del groups['files']
        reload()
        mark('a file check fails for a missing, a too old, and a too small backup file, names each property of a settings file that differs from what is expected, checks the owner, the group, and the permissions of a directory, and fails while a flag file exists')
        mark('a system_log destination writes the failure of a check and its recovery to the journal of the guest, each as one line with the alert\'s severity in front, under the identifier mmwatchdog-alert, with the priorities critical and informational')

        # Files on a filesystem of another machine that is gone. The guest
        # exports a directory over NFS to itself and mounts it, and the test
        # stops the NFS server: the kernel then holds every question about
        # a file there without end. The node has eight places for a run,
        # and eight checks of eight such files take them all. Each run
        # gives its place back, so the other checks of the node go on.
        # The packages of this scenario and of the RAID array below, in one
        # refresh of the package lists.
        if vm.console('command -v exportfs && command -v mdadm', check=False).returncode != 0:
            vm.install_packages('nfs-kernel-server mdadm')
        reports_on_nfs = ['report-' + str(number) for number in range(1, 9)]
        vm.console('mkdir -p ' + EXPORTED + ' ' + NETWORK_FILESYSTEM + ' /etc/exports.d; for report in ' + ' '.join(reports_on_nfs) + "; do printf 'the report of the night\\n' > " + EXPORTED + '/$report; done; '
                   "printf '%s 127.0.0.1(rw,sync,no_subtree_check,no_root_squash)\\n' " + EXPORTED + ' > ' + EXPORTS + '; systemctl start nfs-server; exportfs -ra; '
                   # Fedora's nfs-mountd, started with the server, ends at once because the file /var/lib/nfs/etab
                   # does not exist yet, and exportfs makes it: when mountd does not answer, it starts again.
                   'if command -v rpcinfo >/dev/null && ! timeout 5 rpcinfo -t 127.0.0.1 mountd 3 >/dev/null 2>&1; then systemctl restart nfs-mountd; fi; '
                   'mount -t nfs -o vers=3,hard,noac,nolock 127.0.0.1:' + EXPORTED + ' ' + NETWORK_FILESYSTEM, timeout=180)
        assert json.loads(cli('validate --json', 0).stdout)['config']['max_concurrent_checks'] == len(reports_on_nfs)
        # The check groups that the scenarios above left behind are done.
        groups.clear()
        groups['network_files'] = dict(quick, check_every='2s', time_limit_per_run='1s', checks={
            report: {'file': {'path': NETWORK_FILESYSTEM + '/' + report}, 'alert_severity': 'warn'} for report in reports_on_nfs})
        groups['local_file'] = dict(quick, file={'path': settings_file}, alert_severity='info')
        reload()

        def network_files(status, message, timeout=40):
            """Wait until each of the eight checks has the status and a message that starts with the text."""
            def all_of_them():
                observations = [last('network_files/' + report) for report in reports_on_nfs]
                return all(o['status'] == status and o['message'].startswith(message) for o in observations) and observations
            try:
                return wait(all_of_them, timeout=timeout)
            except AssertionError:
                raise AssertionError((status, message, [last('network_files/' + report) for report in reports_on_nfs])) from None
        network_files('ok', NETWORK_FILESYSTEM + '/report-')
        wait(lambda: last('local_file/file')['status'] == 'ok')
        seen = len(reports())
        vm.console('systemctl stop nfs-server')
        # The first alert of each check says what happened to its run.
        cannot_check = wait(lambda: len(alerts := {e['check']: e for e in reversed(reports()[seen:]) if e.get('check_group') == 'network_files' and e['type'] == 'cannot_check'}) == len(reports_on_nfs) and alerts, timeout=90)
        for report in reports_on_nfs:
            assert cannot_check[report]['message'] == 'the check run did not finish within the time limit of 1s. MMWatchdog stopped waiting for the run to finish', cannot_check[report]
        waiting = network_files('unknown', 'the check run that started at ')
        assert all(o['message'].endswith(' UTC has not finished. MMWatchdog will start no new run of the check until the earlier run finishes') for o in waiting), waiting
        assert any('a file on a filesystem of another machine that does not answer' in line for line in waiting[0]['details']), waiting[0]
        # Every place was taken, and every run has given its place back:
        # the other check of the node runs on its schedule again.
        wait(lambda: last('local_file/file')['status'] == 'ok')
        earlier = last('local_file/file')['at']
        wait(lambda: (local := last('local_file/file'))['at'] > earlier and local['status'] == 'ok')
        # mmwatchdog check-now starts no second run that would wait in the same way.
        diagnostic = now('network_files')
        assert all(diagnostic[report]['status'] == 'unknown' and ' has not finished. MMWatchdog will start no new run of the check until the earlier run finishes' in diagnostic[report]['message'] for report in reports_on_nfs), diagnostic
        # The server is back: the old runs end, and the next runs count.
        vm.console('systemctl start nfs-server')
        network_files('ok', NETWORK_FILESYSTEM + '/report-', timeout=300)
        wait(lambda: len({e['check'] for e in reports()[seen:] if e.get('check_group') == 'network_files' and e['type'] == 'can_check_again'}) == len(reports_on_nfs))
        del groups['network_files'], groups['local_file']
        reload()
        vm.console('umount -f ' + NETWORK_FILESYSTEM)
        mark('eight file checks of files on an NFS filesystem whose server stops take all eight places for a run that the node has; each is unknown and its first alert says that its run did not end, each run gives its place back so that the other check of the node goes on, each later run and mmwatchdog check-now start no second run, and the checks are healthy again when the server is back')

        # Log lines of a file: a matching line, a line that arrives while MMWatchdog
        # is stopped, and a rotated log.
        log = FIXTURE + '/application.log'
        vm.console("printf 'FATAL: an old problem\\n' > " + log)
        # New events alert at once even with a one-hour delay on the group.
        groups['log'] = dict(quick, alert_after_failing_for='1h', recovered_after_healthy_for='0s', checks={
            'log_has_no_fatal_lines': {'log_lines': {'log_file_path': log, 'fail_when_a_new_line_contains': 'FATAL'}, 'alert_severity': 'warn'}})
        reload()

        def fatal_lines(since):
            return [e for e in reports()[since:] if e.get('check') == 'log_has_no_fatal_lines' and e['type'] == 'failed']
        wait(lambda: last('log/log_has_no_fatal_lines')['message'].startswith('none of 0 new lines in ' + log))
        seen = len(reports())
        vm.console("printf 'INFO: started\\nFATAL: cannot open database\\n' >> " + log)
        found = wait(lambda: fatal_lines(seen))[0]
        assert found['message'] == '1 new line in ' + log + ' contains "FATAL"' and found['body'].splitlines()[2] == 'FATAL: cannot open database', found
        assert not fatal_lines(seen)[1:]
        wait(lambda: last('log/log_has_no_fatal_lines')['status'] == 'ok')
        seen = len(reports())
        vm.console("systemctl stop mmwatchdog; printf 'FATAL: while MMWatchdog was stopped\\n' >> " + log + '; systemctl start mmwatchdog')
        assert 'FATAL: while MMWatchdog was stopped' in wait(lambda: fatal_lines(seen))[0]['body']
        wait(lambda: last('log/log_has_no_fatal_lines')['status'] == 'ok')
        seen = len(reports())
        # The program writes its last words, and then starts a new file
        # under the name. MMWatchdog reads the end of the old file and the new one.
        vm.console("printf 'FATAL: the last words before the rotation\\n' >> " + log + '; mv ' + log + ' ' + log + ".1; printf 'FATAL: first line after the rotation\\n' > " + log)

        def bodies():
            return ' '.join(e['body'] for e in fatal_lines(seen))
        wait(lambda: 'FATAL: first line after the rotation' in bodies())
        assert 'FATAL: the last words before the rotation' in bodies(), bodies()
        del groups['log']

        # Log lines of a journal: a unit writes what the scenario puts into a
        # named pipe, and then nothing.
        pipe = FIXTURE + '/says'
        vm.console('rm -f ' + pipe + '; mkfifo ' + pipe + "; systemd-run --quiet --unit mmwatchdog-writes-journal sh -c 'while true; do cat " + pipe + "; done'")
        vm.console("printf 'worker started\\n' > " + pipe)
        groups['journal'] = dict(quick, checks={
            'worker_log_has_no_fatal_lines': {'log_lines': {'journal_of_systemd_unit': 'mmwatchdog-writes-journal.service', 'fail_when_a_new_line_matches_regex': '^(ERROR|FATAL): '}, 'alert_severity': 'warn'},
            'worker_still_writes': {'log_lines': {'journal_of_systemd_unit': 'mmwatchdog-writes-journal.service', 'fail_when_no_new_line_for': '4s'}, 'alert_severity': 'sos'},
            'unknown_unit': {'log_lines': {'journal_of_systemd_unit': 'mmwatchdog-no-such-unit.service', 'fail_when_a_new_line_contains': 'x'}, 'alert_severity': 'info'}})
        reload()
        wait(lambda: last('journal/worker_log_has_no_fatal_lines')['message'].startswith('none of 0 new lines in the journal of mmwatchdog-writes-journal.service matches'))
        seen = len(reports())
        vm.console("printf 'INFO: FATAL: only at the start of a line\\nFATAL: lost the queue\\n' > " + pipe)
        matched = wait(lambda: [e for e in reports()[seen:] if e.get('check') == 'worker_log_has_no_fatal_lines' and e['type'] == 'failed'])[0]
        assert matched['message'] == '1 new line in the journal of mmwatchdog-writes-journal.service matches the regular expression ^(ERROR|FATAL): ' and 'FATAL: lost the queue' in matched['body'] and 'only at the start' not in matched['body'], matched
        silent = wait(lambda: last('journal/worker_still_writes')['status'] == 'failed' and last('journal/worker_still_writes'), timeout=40)
        assert silent['message'].startswith('the journal of mmwatchdog-writes-journal.service got no new line for ') and silent['message'].endswith(' since the newest line (fails after 4s)'), silent
        vm.console("printf 'working again\\n' > " + pipe)
        wait(lambda: last('journal/worker_still_writes')['status'] == 'ok')
        # A line of 6000 characters: journalctl gives such a message whole
        # only when it is asked to, and MMWatchdog asks.
        wait(lambda: last('journal/worker_log_has_no_fatal_lines')['status'] == 'ok')
        seen = len(reports())
        vm.console("{ printf 'FATAL: a long line '; head -c 6000 /dev/zero | tr '\\0' x; printf '\\n'; } > " + pipe)
        long_line = wait(lambda: [e for e in reports()[seen:] if e.get('check') == 'worker_log_has_no_fatal_lines' and e['type'] == 'failed'])[0]
        assert 'FATAL: a long line xxxx' in long_line['body'], long_line
        assert last('journal/unknown_unit')['status'] == 'unknown' and last('journal/unknown_unit')['message'] == 'systemd unit mmwatchdog-no-such-unit.service not found', last('journal/unknown_unit')
        del groups['journal']
        reload()
        mark('a log_lines check of a file starts at the end of the log, fails for a new line that contains the text and shows the line in the alert, finds a line that was written while MMWatchdog was stopped, and reads the last lines of a rotated log before the first lines of the new file')
        mark('a log_lines check of the journal of a systemd unit fails for a new line that matches the regular expression, fails when the unit wrote nothing for the stated time and recovers with the next line, reads a line of 6000 characters whole, and is unknown for a unit that systemd does not know')

        def becomes(name, status, message=''):
            """Wait until the check has the status and a message that starts with the text."""
            try:
                return wait(lambda: last(name)['status'] == status and last(name)['message'].startswith(message) and last(name), timeout=40)
            except AssertionError:
                raise AssertionError((name, status, message, last(name))) from None

        # Security updates and a required reboot, from the real package
        # manager of the guest.
        package_manager = vm.package_manager()
        pretty_name = vm.console('. /etc/os-release; printf "%s" "$PRETTY_NAME"').stdout.strip()
        # apt-get took longer than 4.5 seconds to list 114 waiting updates on
        # Hetzner's smallest ARM64 server; 14 seconds leaves room, and runs
        # every 15 seconds still fit several times in becomes()'s 40 seconds.
        slow = dict(quick, check_every='15s', time_limit_per_run='14s')
        # zypper reads the package database of a guest that other programs use at the same time.
        security_check = {'zypper': {'zypper_security_patches': {'fail_when': 'a_security_patch_waits_to_be_installed'}},
                          'dnf': {'dnf_security_updates': {'fail_when': 'a_security_update_waits_to_be_installed'}}}.get(
                              package_manager, {'apt_security_updates': {'fail_when': 'a_security_update_waits_to_be_installed'}})
        # The checks of zypper and dnf run on demand (check-now), once an hour otherwise: the package database of a guest that other programs use at the same time.
        groups['software'] = dict(slow if package_manager not in ('zypper', 'dnf') else dict(quick, check_every='1h', time_limit_per_run='30s'), checks={
            'no_security_update_waits': dict(security_check, alert_severity='info'),
            'node_needs_no_reboot': {'reboot_after_updates': {'fail_when': 'the_node_must_reboot_to_use_installed_updates'}, 'alert_severity': 'info'},
            'release_gets_security_fixes': {'distribution_release_security_support': {'security_support': 'regular', 'fail_when_security_support_ends_within': '180d'}, 'alert_severity': 'info'}})

        def now_is(check, status, message=''):
            """One run of the check with mmwatchdog check-now, which runs it at once
            rather than at its next turn, up to check_every later: the check
            has the status and a message that starts with the text."""
            observed = now('software')[check]
            assert observed['status'] == status and observed['message'].startswith(message), (check, status, message, observed)
            return observed

        def finds_the_security_updates(waiting):
            """MMWatchdog must find the packages that the test counted itself, in the way that an administrator would."""
            if waiting:
                noun = '1 security update waits' if len(waiting) == 1 else str(len(waiting)) + ' security updates wait'
                updates = becomes('software/no_security_update_waits', 'failed', noun + ' to be installed: ' + waiting[0])
                assert all(source in updates['message'] for source in waiting[:20]), (updates, waiting)
            else:
                becomes('software/no_security_update_waits', 'ok', 'no security update waits to be installed')

        if package_manager == 'apt':
            # apt writes the success stamp through a Post-Invoke-Success
            # hook. A guest without the hook file, as Debian, gets the two
            # lines that the check's message names, as an administrator
            # would write them; Ubuntu's package update-notifier-common has
            # that file. apt itself must then write the stamp.
            stamp = '/var/lib/apt/periodic/update-success-stamp'
            vm.console("test -e /etc/apt/apt.conf.d/15update-stamp || printf '%s\\n' 'APT::Update::Post-Invoke-Success {\"touch " + stamp + "\";};' 'APT::Periodic::Update-Package-Lists \"1\";' > /etc/apt/apt.conf.d/15update-stamp")
            vm.console('rm -f ' + stamp + '; export DEBIAN_FRONTEND=noninteractive; apt-get -o DPkg::Lock::Timeout=600 update -qq; test -f ' + stamp, timeout=600)
            # The source packages of the packages that an upgrade takes from a security archive,
            # read with dpkg-query: the check reports one finding for each. A package that the
            # upgrade installs anew counts by its own name.
            waiting = sorted(set(vm.console("apt-get --simulate dist-upgrade | grep '^Inst ' | grep -i security | cut -d ' ' -f 2 | cut -d : -f 1 | while read p; do"
                                            " s=$(dpkg-query --show --showformat='${db:Status-Status} ${source:Package}' \"$p\" 2>/dev/null);"
                                            " case \"$s\" in 'installed '*) echo \"${s#installed }\";; *) echo \"$p\";; esac; done", check=False).stdout.split()))
            vm.console('rm -f /run/reboot-required /run/reboot-required.pkgs; mv ' + stamp + ' ' + stamp + '.of-the-test')
            reload()
            now_is('no_security_update_waits', 'unknown', 'apt has no usable successful-refresh stamp at ' + stamp + ', so MMWatchdog cannot know which updates wait: have apt write this stamp after each successful refresh; the module page apt_security_updates, section "Set up", shows how (mmwatchdog describe-module apt_security_updates)')
            vm.console('mv ' + stamp + '.of-the-test ' + stamp)
            finds_the_security_updates(waiting)
            # Lists of available packages that nobody refreshed for nine days.
            stamps = '$(find /var/lib/apt/lists -maxdepth 1 -type f -name "*_Packages*") $(ls /var/lib/apt/periodic/update-success-stamp 2>/dev/null)'
            vm.console("touch -d '9 days ago' " + stamps)
            evidence = 'the stamp /var/lib/apt/periodic/update-success-stamp, which apt writes after a successful refresh, is 9 days old'
            now_is('no_security_update_waits', 'unknown', evidence)
            vm.console('touch ' + stamps)
            now_is('no_security_update_waits', 'failed' if waiting else 'ok')
            # Ubuntu Pro's own local inventory must find a fix that apt cannot
            # install. Select the fixture from the current ESM Apps lists rather
            # than naming a package whose fix might later enter the free archive.
            # The Ubuntu Pro client takes about 2 seconds, so the check has a
            # check group of its own with a time limit that a busy guest meets.
            groups['ubuntu_pro'] = dict(quick, check_every='1h', time_limit_per_run='2m', checks={
                'security_fixes_that_only_ubuntu_pro_delivers': {
                    'ubuntu_pro_security_fixes': {'fail_when': 'the_node_misses_a_security_fix_that_only_ubuntu_pro_delivers'},
                    'alert_severity': 'warn'}})
            reload()

            def pro_now_is(status, message=''):
                observed = now('ubuntu_pro')['security_fixes_that_only_ubuntu_pro_delivers']
                assert observed['status'] == status and observed['message'].startswith(message), (status, message, observed)
                return observed

            distribution_id = vm.console('. /etc/os-release; printf "%s" "$ID"').stdout.strip()
            if distribution_id == 'ubuntu':
                wait(lambda: vm.console('systemctl show esm-cache.service --property=ActiveState --value').stdout.strip() == 'inactive', timeout=180)
                pro_now_is('ok', 'this node misses no security fix that only Ubuntu Pro delivers')
                selector = FIXTURE + '/select_esm_package.py'
                vm.write(selector, r'''import glob, json, re, subprocess


def command(*arguments):
    return subprocess.run(arguments, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env={'PATH': '/usr/sbin:/usr/bin:/sbin:/bin', 'LC_ALL': 'C'})


installed = set(command('dpkg-query', '--show', '--showformat=${db:Status-Status}\t${Package}\n').stdout.splitlines())
candidates = []
for path in glob.glob('/var/lib/ubuntu-advantage/apt-esm/var/lib/apt/lists/*_apps_*_Packages'):
    with open(path) as lists:
        for stanza in lists.read().split('\n\n'):
            fields = dict(line.split(': ', 1) for line in stanza.splitlines() if ': ' in line and not line.startswith(' '))
            if all(key in fields for key in ('Package', 'Version', 'Installed-Size')):
                candidates.append((int(fields['Installed-Size']), fields['Package'], fields['Version']))
for size, package, esm_version in sorted(candidates):
    if 'installed\t' + package in installed:
        continue
    policy = command('apt-cache', 'policy', package)
    match = re.search(r'^\s*Candidate:\s*(\S+)', policy.stdout, re.MULTILINE)
    if not match or match[1] == '(none)':
        continue
    if command('dpkg', '--compare-versions', match[1], 'lt', esm_version).returncode != 0:
        continue
    simulated = command('apt-get', '-s', 'install', package)
    installs = re.findall(r'^Inst (\S+)', simulated.stdout, re.MULTILINE)
    if simulated.returncode == 0 and 1 <= len(installs) <= 4:
        print(json.dumps({'package': package, 'esm_version': esm_version}))
        break
else:
    raise SystemExit('No uninstalled ESM Apps package has an older archive candidate and an apt-get -s install of at most 4 packages')
''')
                selected = json.loads(vm.console('python3 ' + shlex.quote(selector), timeout=600).stdout)
                package = selected['package']
                installed_before = set(vm.console("dpkg-query --show --showformat='${db:Status-Status}\t${Package}\n' | grep '^installed' | cut -f2").stdout.split())
                vm.install_packages(package)
                source = vm.console("dpkg-query --show --showformat='${source:Package}' " + shlex.quote(package)).stdout.strip() or package
                affected = pro_now_is('failed')
                findings = {finding['id']: finding['message'] for finding in affected['findings']}
                assert source in findings and package in findings[source] and 'ESM Apps' in findings[source], (selected, source, affected)
                installed_after = set(vm.console("dpkg-query --show --showformat='${db:Status-Status}\t${Package}\n' | grep '^installed' | cut -f2").stdout.split())
                # Remove only the packages introduced by this fixture, including
                # dependencies that might themselves have an ESM-only fix.
                introduced = installed_after - installed_before
                assert package in introduced, (package, introduced)
                vm.console('DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=600 remove -y ' + ' '.join(shlex.quote(name) for name in sorted(introduced)), timeout=600)
                pro_now_is('ok', 'this node misses no security fix that only Ubuntu Pro delivers')
            else:
                pro_now_is('unknown', 'Ubuntu Pro exists only on Ubuntu, and this node runs Debian')
            mark('ubuntu_pro_security_fixes reports one source finding for an ESM Apps fix of a dynamically selected installed package on Ubuntu, is healthy after the package is removed, and is unknown on Debian')
            del groups['ubuntu_pro']

            # A missing /run/reboot-required says that no reboot is needed
            # only where a kernel hook writes the file for a new kernel. A
            # guest without one, as a minimal Debian, is unknown until the
            # test installs unattended-upgrades, without automatic updates.
            hooks = vm.console('for hook in ' + ' '.join(REBOOT_REQUIRED_HOOKS) + '; do if [ -x $hook ]; then echo $hook; fi; done').stdout.split()
            if not hooks:
                now_is('node_needs_no_reboot', 'unknown', 'no program on this node writes /run/reboot-required when a new kernel is installed, so MMWatchdog cannot know whether the node needs a reboot: install the package unattended-upgrades')
                vm.console("echo 'unattended-upgrades unattended-upgrades/enable_auto_updates boolean false' | debconf-set-selections")
                vm.install_packages('unattended-upgrades')
                hooks = ['/etc/kernel/postinst.d/unattended-upgrades']
            vm.console('rm -f /run/reboot-required /run/reboot-required.pkgs')
            becomes('software/node_needs_no_reboot', 'ok', 'this node needs no reboot')
            # The guest's hook writes the file as the kernel's package has
            # it do for a new kernel, with the name of a package.
            vm.console('DPKG_MAINTSCRIPT_PACKAGE=linux-image-of-the-test ' + hooks[0] + ' 0.0-of-the-test /boot/vmlinuz-0.0-of-the-test')
            now_is('node_needs_no_reboot', 'failed', 'this node needs a reboot for 1 package: ')
            vm.console('rm -f /run/reboot-required /run/reboot-required.pkgs')
            now_is('node_needs_no_reboot', 'ok')
            # Read the guest's release date independently, by the CSV header.
            distribution_id = vm.console('. /etc/os-release; printf "%s" "$ID"').stdout.strip()
            dates_path = '/usr/share/distro-info/' + distribution_id + '.csv'
            if vm.console('test -f ' + dates_path, check=False).returncode != 0:
                vm.install_packages('distro-info-data')
            vm.console('mv ' + dates_path + ' ' + dates_path + '.of-the-test')
            now_is('release_gets_security_fixes', 'unknown', 'MMWatchdog cannot read ' + dates_path + ', which holds the dates of the releases: install the package distro-info-data')
            vm.console('mv ' + dates_path + '.of-the-test ' + dates_path)
            distribution_name = vm.console('. /etc/os-release; printf "%s" "$PRETTY_NAME"').stdout.strip()
            support_choices = {
                'debian': [('regular', 'eol', "Debian's regular security support"),
                           ('debian_lts', 'eol-lts', 'Debian LTS')],
                'ubuntu': [('regular', 'eol-server', "Ubuntu's standard security maintenance"),
                           ('ubuntu_pro', 'eol-esm', "Ubuntu Pro's Expanded Security Maintenance")],
            }
            support_inputs = groups['software']['checks']['release_gets_security_fixes']['distribution_release_security_support']
            forced_failure = None
            for security_support, support_column, support_name in support_choices[distribution_id]:
                support_data = vm.console(
                    '. /etc/os-release; awk -F, -v series="$VERSION_CODENAME" -v support_column=' + support_column +
                    " 'NR == 1 {for (i = 1; i <= NF; i++) column[$i] = i; next} "
                    '$column["series"] == series {date = $column[support_column]; fallback = (date == ""); '
                    'if (fallback) date = $column["eol"]; print date; print fallback; exit}' +
                    "' " + dates_path).stdout.splitlines()
                assert len(support_data) == 2 and support_data[0], (dates_path, distribution_name, security_support, support_data)
                support_date, fallback = support_data
                if fallback == '1':
                    support_name = "the release has no end date of " + support_name + ", and the release's support"
                # Use the guest's clock and UTC midnight, independently of the check.
                end_seconds = int(vm.console('date -u -d ' + support_date + ' +%s').stdout.strip())
                guest_seconds = int(vm.console('date -u +%s').stdout.strip())
                seconds_left = end_seconds - guest_seconds
                expected_status = 'failed' if seconds_left <= 180 * 86400 else 'ok'
                support_message = ('this node runs ' + distribution_name + ': ' + support_name +
                                   (' ended on ' if seconds_left < 0 else ' ends on ') + support_date +
                                   (', ' if seconds_left < 0 else ', in '))
                support_inputs['security_support'] = security_support
                reload()
                observed = now_is('release_gets_security_fixes', expected_status, support_message)
                if seconds_left < 0:
                    assert observed['message'].endswith(' ago'), observed
                if 180 * 86400 < seconds_left <= 3650 * 86400:
                    forced_failure = security_support, support_message
            assert forced_failure, (distribution_id, 'no support date between 180d and 3650d')
            # A value of the other distribution cannot apply to this node.
            other_value, other_distribution = ('ubuntu_pro', 'Ubuntu') if distribution_id == 'debian' else ('debian_lts', 'Debian')
            support_inputs['security_support'] = other_value
            reload()
            now_is('release_gets_security_fixes', 'unknown', 'security_support: ' + other_value + ' applies only to ' + other_distribution + ', and this node runs ' + distribution_name)
            support_inputs['security_support'] = forced_failure[0]
            support_inputs['fail_when_security_support_ends_within'] = '3650d'
            reload()
            now_is('release_gets_security_fixes', 'failed', forced_failure[1])
            mark('an apt_security_updates check finds on the real apt of the guest, which wrote the success stamp through the hook that the module page named by the message of the check shows, the security updates that the test counts itself, and is unknown without a successful-refresh stamp or when that stamp is nine days old; a reboot_after_updates check is unknown on a guest where no kernel hook writes /run/reboot-required, and fails with the package that the guest\'s hook names; a distribution_release_security_support check uses the release date read independently from the guest, is unknown without the file of distro-info-data, matches the independently calculated status and support name for regular and for the longer support of the distribution with 180d, is unknown for the value of the other distribution, including ended support, and fails with 3650d for a date more than 180 days away')
            del groups['software']
        elif package_manager == 'zypper':
            vm.write(ZYPPER_HELPER, ZYPPER_HELPER_TEXT)

            def helper(*arguments):
                return vm.console('python3 ' + ZYPPER_HELPER + ' ' + ' '.join(arguments), timeout=900).stdout.strip()

            def waiting_patches():
                """The security patches that wait, read as root with zypper's own listing, independently of the check."""
                return [line for line in helper('waiting').splitlines() if line]

            def finds_the_waiting_patches():
                waiting = waiting_patches()
                if not waiting:
                    return now_is('no_security_update_waits', 'ok', 'no security patch waits to be installed')
                noun = '1 security patch waits' if len(waiting) == 1 else str(len(waiting)) + ' security patches wait'
                observed = now_is('no_security_update_waits', 'failed', noun + ' to be installed: ')
                assert all(patch in observed['message'] or ' (needs a reboot)' in observed['message'] for patch in waiting[:20]), (observed, waiting)
                assert sorted(finding['id'] for finding in observed['findings']) == sorted(line.split(' (')[0] for line in waiting), (observed, waiting)
                return observed

            # openSUSE Leap's cloud image refreshes nothing by itself. Set up the daily refresh as
            # the module page shows, from the page's own text, start it, and see that the enabled
            # repositories are exactly the directories of zypper's cache.
            page = (vm.REPO / 'internal/checksandalerts/check_zypper_security_patches.md').read_text()
            units = re.findall(r'```ini\n# (/etc/systemd/system/\S+)\n(.*?)```', page, re.S)
            assert [path for path, _ in units] == ['/etc/systemd/system/zypper-refresh-daily.service', '/etc/systemd/system/zypper-refresh-daily.timer'], units
            for path, text in units:
                vm.write(path, '# ' + path + '\n' + text)
            vm.console('systemctl daemon-reload && systemd-analyze verify ' + ' '.join(path for path, _ in units) + ' && systemctl start zypper-refresh-daily.service && systemctl enable --now zypper-refresh-daily.timer && systemctl is-active zypper-refresh-daily.timer', timeout=900)
            repositories = helper('repository-directories').splitlines()
            assert repositories and all(vm.console('test -f /var/cache/zypp/raw/' + shlex.quote(r) + '/repodata/repomd.xml', check=False).returncode == 0 for r in repositories), repositories
            vm.console('rm -f /run/reboot-needed')
            reload()
            finds_the_waiting_patches()
            # An older version of a package that a security patch covers, chosen now from the
            # repositories of the guest: the patch then waits.
            package, patch = helper('install-an-older-package').splitlines()
            try:
                assert patch in [line.split(' (')[0] for line in waiting_patches()], (package, patch)
                observed = finds_the_waiting_patches()
                assert patch + ' (' in observed['message'], (patch, observed)
                # The metadata of one repository that nobody refreshed for nine days.
                path = '/var/cache/zypp/raw/' + repositories[0] + '/repodata/repomd.xml'
                vm.console("touch -d '9 days ago' " + shlex.quote(path))
                refresh_hint = ', so MMWatchdog cannot know which patches wait: set up a daily refresh, a systemd timer that runs zypper --non-interactive refresh, as the page of zypper_security_patches shows (mmwatchdog describe-module zypper_security_patches)'
                now_is('no_security_update_waits', 'unknown', path + ', which zypper writes after a successful refresh of the repository ' + repositories[0] + ', is 9 days old' + refresh_hint)
                # A refresh of a repository that has not changed touches the file again.
                vm.console('zypper --non-interactive refresh', timeout=900)
                finds_the_waiting_patches()
                # A repository without its metadata.
                vm.console('mv ' + shlex.quote(path) + ' ' + shlex.quote(path) + '.of-the-test')
                now_is('no_security_update_waits', 'unknown', 'the repository ' + repositories[0] + ' has no usable ' + path + ', which zypper writes after a successful refresh of the repository' + refresh_hint)
                vm.console('mv ' + shlex.quote(path) + '.of-the-test ' + shlex.quote(path))
            finally:
                vm.console('zypper --non-interactive remove --clean-deps ' + shlex.quote(package), check=False, timeout=900)
            finds_the_waiting_patches()
            # zypper creates /run/reboot-needed when a package with a reboot hint is installed:
            # the kernel's package again, in the version that runs.
            now_is('node_needs_no_reboot', 'ok', 'this node needs no reboot')
            vm.console('zypper --non-interactive --no-refresh install --force kernel-default', timeout=900)
            vm.console('test -e /run/reboot-needed')
            now_is('node_needs_no_reboot', 'failed', 'this node needs a reboot (/run/reboot-needed exists)')
            vm.console('rm -f /run/reboot-needed')
            now_is('node_needs_no_reboot', 'ok', 'this node needs no reboot')
            # The release's end is the endoflife of the product, read independently with Python's XML parser.
            support_date = helper('end-of-the-release')
            product_file = '/etc/products.d/baseproduct'
            distribution_name = pretty_name
            end_seconds = int(vm.console('date -u -d ' + support_date + ' +%s').stdout.strip())
            guest_seconds = int(vm.console('date -u +%s').stdout.strip())
            seconds_left = end_seconds - guest_seconds
            support_message = ('this node runs ' + distribution_name + ": SUSE's regular security support" +
                               (' ended on ' if seconds_left < 0 else ' ends on ') + support_date + (', ' if seconds_left < 0 else ', in '))
            support_inputs = groups['software']['checks']['release_gets_security_fixes']['distribution_release_security_support']
            now_is('release_gets_security_fixes', 'failed' if seconds_left <= 180 * 86400 else 'ok', support_message)
            assert 180 * 86400 < seconds_left <= 3650 * 86400, ('the test needs a release that ends in more than 180 days and less than 3650', support_date)
            support_inputs['fail_when_security_support_ends_within'] = '3650d'
            reload()
            now_is('release_gets_security_fixes', 'failed', support_message)
            support_inputs['fail_when_security_support_ends_within'] = '180d'
            for other_value, other_distribution in [('debian_lts', 'Debian'), ('ubuntu_pro', 'Ubuntu')]:
                support_inputs['security_support'] = other_value
                reload()
                now_is('release_gets_security_fixes', 'unknown', 'security_support: ' + other_value + ' applies only to ' + other_distribution + ', and this node runs ' + distribution_name)
            support_inputs['security_support'] = 'regular'
            reload()
            vm.console('mv ' + product_file + ' ' + product_file + '.of-the-test')
            try:
                now_is('release_gets_security_fixes', 'unknown', 'MMWatchdog cannot read ' + product_file + ', the product file of this release')
            finally:
                vm.console('mv ' + product_file + '.of-the-test ' + product_file)
            vm.console('systemctl disable --now zypper-refresh-daily.timer; rm -f /etc/systemd/system/zypper-refresh-daily.service /etc/systemd/system/zypper-refresh-daily.timer; systemctl daemon-reload')
            mark('a zypper_security_patches check, after the module page\'s own daily refresh service and timer ran against the real repositories whose cache directories the test compares with the enabled repositories, reports the security patches that zypper lists as root, fails with the name, severity, and summary of the patch for an older version of a package that the test chooses at run time from the repositories, is unknown for metadata of nine days and for metadata that is missing, and is healthy again after the refresh and the removal of the package; a reboot_after_updates check is healthy without /run/reboot-needed and fails after zypper reinstalls the kernel\'s package; a distribution_release_security_support check uses the endoflife of the product file read independently, is unknown for security_support values of other distributions and without the product file')
            groups['ubuntu_pro'] = dict(quick, check_every='1h', time_limit_per_run='2m', checks={
                'security_fixes_that_only_ubuntu_pro_delivers': {
                    'ubuntu_pro_security_fixes': {'fail_when': 'the_node_misses_a_security_fix_that_only_ubuntu_pro_delivers'},
                    'alert_severity': 'warn'}})
            reload()
            observed = now('ubuntu_pro')['security_fixes_that_only_ubuntu_pro_delivers']
            assert observed['status'] == 'unknown' and observed['message'] == 'Ubuntu Pro exists only on Ubuntu, and this node runs ' + pretty_name and not observed.get('findings'), observed
            del groups['ubuntu_pro']
            mark('an ubuntu_pro_security_fixes check is unknown on ' + pretty_name + ', and its message says that Ubuntu Pro exists only on Ubuntu')
            del groups['software']
        elif package_manager == 'dnf':
            vm.write(DNF_HELPER, DNF_HELPER_TEXT)

            def helper(*arguments):
                return vm.console('python3 ' + DNF_HELPER + ' ' + ' '.join(arguments), timeout=1800).stdout.strip()

            refresh_hint = ', so MMWatchdog cannot know which updates wait: enable the refresh timer of dnf, systemctl enable --now dnf-makecache.timer, as the page of dnf_security_updates shows (mmwatchdog describe-module dnf_security_updates)'
            # Fedora's dnf is dnf 5, which keeps its package data in another cache, has no stamp
            # (and so cannot be fooled by one), and refreshes a repository only when its package data is
            # older than the repository's own metadata_expire: 14 days for one of Fedora's repositories.
            fedora = vm.distribution()[0] == 'fedora'
            cache = '/var/cache/libdnf5' if fedora else '/var/cache/dnf'
            stamp = '/var/cache/dnf/last_makecache'
            old_days = 20 if fedora else 9
            all_package_data = cache + '/*/repodata/*primary.*'
            # dnf 4: makecache --timer waits dnf's metadata_timer_sync after its last run (3 hours),
            # so the refresh that the test wants starts with a stamp that is old.
            age_the_stamp = "touch -d '9 days ago' " + stamp + '; ' if not fedora else ''

            def refresh_the_package_data(check=True):
                """The refresh that the timer starts, with package data that is old enough to be refreshed."""
                age = "touch -d '%d days ago' " % old_days + all_package_data + '; ' if fedora else age_the_stamp
                return vm.console(age + 'systemctl start dnf-makecache.service', check=check, timeout=900)

            def package_data_is_old():
                observed = now_is('no_security_update_waits', 'unknown', 'the package data of the repository ')
                if fedora:
                    assert ', is %d days old (the limit for this repository is ' % old_days in observed['message'] and observed['message'].endswith(' days)' + refresh_hint), observed
                else:
                    assert observed['message'].endswith(', is 9 days old' + refresh_hint), observed

            def finds_the_waiting_updates(expected=None):
                """The security advisories that wait, read as root with dnf's own listing, independently of the check: one finding for each."""
                waiting = helper('waiting-advisories').splitlines() if expected is None else expected
                if not waiting:
                    return now_is('no_security_update_waits', 'ok', 'no security update waits to be installed')
                noun = '1 security advisory waits' if len(waiting) == 1 else str(len(waiting)) + ' security advisories wait'
                observed = now_is('no_security_update_waits', 'failed', noun + ' to be installed: ')
                assert all(advisory in observed['message'] for advisory in waiting[:20]), (observed, waiting)
                assert sorted(finding['id'] for finding in observed['findings']) == sorted(waiting), (observed, waiting)
                return observed

            if fedora:
                # Fedora installs dnf's plugins (dnf5-plugins). The test takes the package away and gives it back.
                vm.console('dnf -y -q remove dnf5-plugins >/dev/null', timeout=900)
                assert 'Unknown argument "needs-restarting"' in vm.console('dnf needs-restarting --json', check=False).stderr
                reload()
                now_is('node_needs_no_reboot', 'unknown', "the dnf of this node has no command needs-restarting, which says whether the node needs a reboot: install the package of dnf's plugins, with dnf install dnf5-plugins")
                vm.install_packages('dnf5-plugins')
            else:
                # The reboot check needs dnf's plugins, which Rocky Linux and AlmaLinux do not install.
                assert 'No such command' in vm.console('dnf needs-restarting --reboothint', check=False).stderr
                reload()
                now_is('node_needs_no_reboot', 'unknown', "the dnf of this node has no command needs-restarting, which says whether the node needs a reboot: install the package of dnf's plugins, with dnf install dnf-plugins-core")
                vm.install_packages('dnf-plugins-core')
            now_is('node_needs_no_reboot', 'ok', 'this node needs no reboot')

            # dnf's refresh timer: the page of the module says how to turn it on. The refresh
            # that the timer starts must reach every enabled repository, and touch the
            # package data in exactly one directory of each repository's id.
            page = (vm.REPO / 'internal/checksandalerts/check_dnf_security_updates.md').read_text()
            assert 'systemctl enable --now dnf-makecache.timer' in page
            vm.console('systemctl enable --now dnf-makecache.timer && systemctl is-active dnf-makecache.timer')
            repositories = helper('repositories').splitlines()
            directories = [line.split() for line in helper('directories').splitlines()]
            assert repositories and sorted({repository for repository, _ in directories}) == repositories, (repositories, directories)
            vm.console("touch -d '%d days ago' " % old_days + all_package_data)
            refresh_the_package_data()
            refreshed = vm.console("find " + cache + " -path '*/repodata/*primary.*' -newermt '1 hour ago' -printf '%h\\n' | sed 's#/repodata$##; s#.*/##' | sort").stdout.split()
            # One refreshed directory for each enabled repository, and each directory is the one of its id.
            assert sorted(name.rsplit('-', 1)[0] for name in refreshed) == repositories and all(name in [d for _, d in directories] for name in refreshed), (refreshed, directories, repositories)
            # Right after a run, the timer's refresh asks no server: it touches no package data.
            # (dnf 4: the stamp is new. dnf 5: the package data is younger than every repository's metadata_expire.)
            if fedora:
                times = "find " + cache + " -path '*/repodata/*primary.*' -printf '%p %T@\\n' | sort"
                before = vm.console(times).stdout
                vm.console('systemctl start dnf-makecache.service', timeout=900)
                assert vm.console(times).stdout == before
            else:
                vm.console("touch -d '9 days ago' " + all_package_data + '; systemctl start dnf-makecache.service', timeout=900)
                assert int(vm.console("find " + cache + " -path '*/repodata/*primary.*' -newermt '1 hour ago' | wc -l").stdout) == 0
            refresh_the_package_data()
            vm.console('rm -f /run/reboot-required')
            reload()
            finds_the_waiting_updates()

            # A security update that waits: a package that has a security advisory, chosen now
            # from the guest, in the version before the update.
            # The distribution's mirrors may hold no older version of any such package,
            # as Rocky Linux 9 and AlmaLinux 10 on 2026-11-17: the run then says so and
            # goes on with the steps that need no waiting update.
            found = vm.console('python3 ' + DNF_HELPER + ' downgrade-a-package', check=False, timeout=1800)
            package = found.stdout.strip() if found.returncode == 0 else ''
            if not package:
                print('NOT RUN a waiting security update of a downgraded package: ' + (found.stderr or found.stdout).strip(), flush=True)
            try:
                if package:
                    assert package in helper('waiting').splitlines(), package
                    observed = finds_the_waiting_updates()
                    assert any(package in finding['message'] for finding in observed['findings']), (package, observed)

                # Package data that nobody refreshed for nine days (dnf 5: twenty, because
                # one of Fedora's repositories keeps its package data 14 days). The stamp
                # of dnf 4's refresh does not decide: dnf makecache --timer touches it also
                # when the refresh fails, so a node whose refresh always fails would look
                # fresh. Both are tested: the package data is old, the refresh fails,
                # and the check stays unknown while the stamp is new. dnf 5 has no stamp:
                # its failed refresh touches nothing.
                vm.console("touch -d '%d days ago' " % old_days + all_package_data)
                first_repository = directories[0][0]
                package_data_is_old()
                hosts = helper('hosts').splitlines()
                assert hosts, hosts
                hosts_line = '127.0.0.1 ' + ' '.join(hosts) + ' # mmwatchdog-test-dnf'
                vm.console("cp /etc/hosts /etc/hosts.of-the-test && printf '%s\\n' " + shlex.quote(hosts_line) + ' >> /etc/hosts')
                try:
                    failed = refresh_the_package_data(check=False)
                    assert failed.returncode != 0, 'the refresh with the servers blocked must fail'
                    if fedora:
                        # dnf 5 touched no package data for the failed refresh.
                        assert int(vm.console("find " + cache + " -path '*/repodata/*primary.*' -newermt '1 minute ago' | wc -l").stdout) == 0
                    else:
                        # dnf 4 has touched its stamp for the failed refresh, as the code of the check says.
                        assert int(vm.console("find " + stamp + " -newermt '1 minute ago' | wc -l").stdout) == 1
                finally:
                    # Where systemd-resolved runs (Fedora), it answers from its cache of the blocked names
                    # for a moment longer: the test waits until each name has its real address again.
                    vm.console('mv /etc/hosts.of-the-test /etc/hosts; resolvectl flush-caches 2>/dev/null; for host in ' + ' '.join(hosts) +
                               '; do timeout 30 sh -c "until getent hosts $0 | grep -qv ^127.0.0.1; do sleep 0.5; done" $host; done; true')
                package_data_is_old()
                # A refresh that succeeds touches the package data again.
                refresh_the_package_data()
                finds_the_waiting_updates()
                # A repository without package data in the cache.
                moved = [directory for repository, directory in directories if repository == first_repository]
                vm.console(' && '.join('mv ' + cache + '/' + directory + ' ' + cache + '/' + directory + '.of-the-test' for directory in moved))
                try:
                    if fedora:
                        now_is('no_security_update_waits', 'unknown', 'MMWatchdog cannot ask dnf about its repositories: Cache-only enabled but no cache for repository "' + first_repository + '"')
                    else:
                        now_is('no_security_update_waits', 'unknown', 'the repository ' + first_repository + ' has no package data in dnf\'s cache (a file *primary.* in /var/cache/dnf/' + first_repository + '-HASH/repodata)' + refresh_hint)
                finally:
                    vm.console(' && '.join('mv ' + cache + '/' + directory + '.of-the-test ' + cache + '/' + directory for directory in moved))
                finds_the_waiting_updates()
            finally:
                if package:
                    vm.console('dnf --quiet --assumeyes upgrade ' + shlex.quote(package), check=False, timeout=900)
            finds_the_waiting_updates()

            # dnf's reboot hint: the C library installed again since the boot. When the
            # mirrors no longer hold the installed version (Rocky Linux 9 and AlmaLinux 10
            # on 2026-11-17), its update gives the same hint.
            vm.console('dnf -y -q reinstall glibc >/dev/null 2>&1 || dnf -y -q upgrade glibc >/dev/null 2>&1', timeout=900)
            assert vm.console('dnf --quiet --disablerepo="*" needs-restarting --reboothint', check=False).returncode == 1
            observed = now_is('node_needs_no_reboot', 'failed', 'this node needs a reboot (dnf needs-restarting)' if fedora else 'this node needs a reboot (dnf needs-restarting --reboothint)')
            assert any('glibc' in line for line in observed.get('details', [])), observed

            # The end of the release is the SUPPORT_END of /etc/os-release, read here by the shell.
            support_date = vm.console('. /etc/os-release; printf "%s" "$SUPPORT_END"').stdout.strip()
            distribution_name = pretty_name
            end_seconds = int(vm.console('date -u -d ' + shlex.quote(support_date) + ' +%s').stdout.strip())
            guest_seconds = int(vm.console('date -u +%s').stdout.strip())
            seconds_left = end_seconds - guest_seconds
            support_message = ('this node runs ' + distribution_name + ': the regular security support that the release states' +
                               (' ended on ' if seconds_left < 0 else ' ends on ') + support_date + (', ' if seconds_left < 0 else ', in '))
            support_inputs = groups['software']['checks']['release_gets_security_fixes']['distribution_release_security_support']
            now_is('release_gets_security_fixes', 'failed' if seconds_left <= 180 * 86400 else 'ok', support_message)
            assert 180 * 86400 < seconds_left <= 3650 * 86400, ('the test needs a release that ends in more than 180 days and less than 3650', support_date)
            support_inputs['fail_when_security_support_ends_within'] = '3650d'
            reload()
            now_is('release_gets_security_fixes', 'failed', support_message)
            support_inputs['fail_when_security_support_ends_within'] = '180d'
            for other_value, other_distribution in [('debian_lts', 'Debian'), ('ubuntu_pro', 'Ubuntu')]:
                support_inputs['security_support'] = other_value
                reload()
                now_is('release_gets_security_fixes', 'unknown', 'security_support: ' + other_value + ' applies only to ' + other_distribution + ', and this node runs ' + distribution_name)
            support_inputs['security_support'] = 'regular'
            reload()
            # The file that /etc/os-release is: on Rocky Linux and AlmaLinux a link to /usr/lib/os-release, and
            # on Fedora's cloud image a file of its own.
            os_release = vm.console('readlink -f /etc/os-release').stdout.strip()
            vm.console('cp -p ' + os_release + ' ' + os_release + '.of-the-test && sed -i /^SUPPORT_END=/d ' + os_release)
            try:
                now_is('release_gets_security_fixes', 'unknown', '/etc/os-release gives no SUPPORT_END for this release, which the release package of ' + distribution_name + ' installs')
            finally:
                vm.console('mv ' + os_release + '.of-the-test ' + os_release + '; restorecon ' + os_release)
            now_is('release_gets_security_fixes', 'ok', support_message)
            # The reboot ends the hint; the guest then starts again with the daemon.
            vm.reboot()
            # The test's fixture server (it takes the alerts) is not enabled for boot.
            vm.console('systemctl start mmwatchdog-fixture.service')
            reload()
            now_is('node_needs_no_reboot', 'ok', 'this node needs no reboot')
            mark('a dnf_security_updates check, after the module page\'s own command turned on the refresh timer, whose refresh touches the package data of exactly the cache directory of each enabled repository, reports the security updates that dnf lists as root, fails with the package that the test chooses at run time and installs in the version before its security update, is unknown for package data of nine days, also when a failed refresh has touched dnf\'s stamp, and for missing package data, and is healthy again after the refresh and the update; a reboot_after_updates check is unknown without dnf-plugins-core, healthy after it, fails with glibc after the C library was installed anew, and is healthy after the guest started again; a distribution_release_security_support check uses the SUPPORT_END of /etc/os-release read independently, is unknown for security_support values of other distributions and without SUPPORT_END')
            groups['ubuntu_pro'] = dict(quick, check_every='1h', time_limit_per_run='2m', checks={
                'security_fixes_that_only_ubuntu_pro_delivers': {
                    'ubuntu_pro_security_fixes': {'fail_when': 'the_node_misses_a_security_fix_that_only_ubuntu_pro_delivers'},
                    'alert_severity': 'warn'}})
            reload()
            observed = now('ubuntu_pro')['security_fixes_that_only_ubuntu_pro_delivers']
            assert observed['status'] == 'unknown' and observed['message'] == 'Ubuntu Pro exists only on Ubuntu, and this node runs ' + pretty_name and not observed.get('findings'), observed
            del groups['ubuntu_pro']
            mark('an ubuntu_pro_security_fixes check is unknown on ' + pretty_name + ', and its message says that Ubuntu Pro exists only on Ubuntu')
            del groups['software']
        else:
            # Only Debian, Ubuntu, the Red Hat family, and SUSE have a package manager that these checks know: each of them answers before it
            # asks the package manager, so this guest needs none of apt's
            # files, dpkg, or Ubuntu Pro.
            groups['ubuntu_pro'] = dict(quick, check_every='1h', time_limit_per_run='2m', checks={
                'security_fixes_that_only_ubuntu_pro_delivers': {
                    'ubuntu_pro_security_fixes': {'fail_when': 'the_node_misses_a_security_fix_that_only_ubuntu_pro_delivers'},
                    'alert_severity': 'warn'}})
            reload()
            for group, check, message in [
                    ('software', 'no_security_update_waits', 'MMWatchdog does not know the package manager of ' + pretty_name + ': it knows only that of Debian and Ubuntu (apt_security_updates), that of the Red Hat family (dnf_security_updates), and that of SUSE (zypper_security_patches)'),
                    ('software', 'node_needs_no_reboot', 'MMWatchdog does not know how ' + pretty_name + ' says that a node needs a reboot: it knows only Debian, Ubuntu, the Red Hat family, and SUSE'),
                    ('software', 'release_gets_security_fixes', 'MMWatchdog does not know the security support of ' + pretty_name + ': it knows only that of Debian, Ubuntu, the Red Hat family, and SUSE'),
                    ('ubuntu_pro', 'security_fixes_that_only_ubuntu_pro_delivers', 'Ubuntu Pro exists only on Ubuntu, and this node runs ' + pretty_name)]:
                observed = now(group)[check]
                assert observed['status'] == 'unknown' and observed['message'] == message and not observed.get('findings'), (check, message, observed)
            del groups['ubuntu_pro']
            mark('an apt_security_updates, a reboot_after_updates, a distribution_release_security_support, and an ubuntu_pro_security_fixes check are unknown on ' + pretty_name + ', each with the message that MMWatchdog knows only the package manager, the reboot signal, or the security support of Debian, Ubuntu, and SUSE, or that Ubuntu Pro exists only on Ubuntu')
            del groups['software']

        if package_manager == 'apt':
            # The real package tools, a vendor repository, and one HTTPS
            # stand-in for CISA's list and OSV.dev. The stand-in's certificate
            # enters the system trust store and /etc/hosts sends both host names
            # to it: no input of the check weakens TLS or changes an address.
            stand_in = CISA_AND_OSV_DIR
            vm.console('mkdir -p ' + stand_in)
            try:
                for program, package in [('openssl', 'openssl'), ('update-ca-certificates', 'ca-certificates'), ('sudo', 'sudo')]:
                    if vm.console('command -v ' + program, check=False).returncode != 0:
                        vm.install_packages(package)
                vm.console('openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=api.osv.dev '
                           '-addext subjectAltName=DNS:raw.githubusercontent.com,DNS:api.osv.dev -keyout ' + stand_in + '/server.key '
                           '-out ' + stand_in + '/server.crt >/dev/null 2>&1')
                vm.trust_certificate(CISA_AND_OSV_CERTIFICATE_NAME, vm.console('cat ' + stand_in + '/server.crt').stdout)
                # MMWatchdog reads the system trust store once, when it starts.
                vm.console("printf '%s\\n' " + shlex.quote(CISA_AND_OSV_HOSTS_LINE) + ' >> /etc/hosts; systemctl restart mmwatchdog')
                vm.write(stand_in + '/cisa_and_osv_stand_in.py', (vm.REPO / 'tests/integration/cisa_and_osv_stand_in.py').read_text())

                # A flat file repository with an explicit third-party Origin.
                architecture = vm.console('dpkg --print-architecture').stdout.strip()
                vendor_dir = stand_in + '/vendor'
                vm.write(vendor_dir + '/package/DEBIAN/control',
                         'Package: hello\nVersion: 99.0-vendor\nArchitecture: ' + architecture + '\n'
                         'Maintainer: MMWatchdog tests <tests@example.com>\nDescription: A vendor package for the node suite\n')
                vm.console('mkdir -p ' + vendor_dir + '/repository; dpkg-deb --build ' + vendor_dir + '/package ' + vendor_dir + '/repository/hello.deb >/dev/null')
                size, digest = vm.console('stat -c %s ' + vendor_dir + '/repository/hello.deb; sha256sum ' + vendor_dir + '/repository/hello.deb').stdout.splitlines()
                vm.write(vendor_dir + '/repository/Packages',
                         'Package: hello\nVersion: 99.0-vendor\nArchitecture: ' + architecture + '\n'
                         'Maintainer: MMWatchdog tests <tests@example.com>\nDescription: A vendor package for the node suite\n'
                         'Filename: ./hello.deb\nSize: ' + size.strip() + '\nSHA256: ' + digest.split()[0] + '\n\n')
                packages_path = vendor_dir + '/repository/Packages'
                packages_size, packages_md5, packages_sha256 = vm.console(
                    'stat -c %s ' + packages_path + '; md5sum ' + packages_path + '; sha256sum ' + packages_path).stdout.splitlines()
                vm.write(vendor_dir + '/repository/Release',
                         'Origin: MMWatchdog test vendor\nLabel: MMWatchdog test vendor\nArchitectures: ' + architecture + '\n'
                         'MD5Sum:\n ' + packages_md5.split()[0] + ' ' + packages_size.strip() + ' Packages\n'
                         'SHA256:\n ' + packages_sha256.split()[0] + ' ' + packages_size.strip() + ' Packages\n')
                vm.write(VENDOR_REPOSITORY_FILE, 'deb [trusted=yes] file:' + vendor_dir + '/repository ./\n')
                vm.console('export DEBIAN_FRONTEND=noninteractive; apt-get -o DPkg::Lock::Timeout=600 update -qq && '
                           'apt-get -o DPkg::Lock::Timeout=600 install -y -qq hello=99.0-vendor >/dev/null', timeout=600)

                # Read the guest's facts independently, rather than deriving
                # expected names and versions from the check's own answer.
                distribution_id, version_id = vm.distribution()
                lts = 'LTS' in vm.console('. /etc/os-release; printf "%s" "$VERSION"').stdout
                if distribution_id == 'debian':
                    main_ecosystem, pro_ecosystem = 'Debian:' + version_id, None
                    record_prefix, tracker = 'DEBIAN-', 'https://security-tracker.debian.org/tracker/'
                else:
                    main_ecosystem = 'Ubuntu:' + version_id + (':LTS' if lts else '')
                    pro_ecosystem = 'Ubuntu:Pro:' + version_id + ':LTS' if lts else None
                    record_prefix, tracker = 'UBUNTU-', 'https://ubuntu.com/security/'
                ecosystems = [main_ecosystem] + ([pro_ecosystem] if pro_ecosystem else [])
                kernel_release = vm.console('uname -r').stdout.strip()
                kernel_fields = vm.console("dpkg-query --show --showformat='${source:Package}\\t${source:Version}\\t${Built-Using}\\n' linux-image-" + shlex.quote(kernel_release)).stdout.strip().split('\t')
                kernel_source, kernel_version = kernel_fields[:2]
                if len(kernel_fields) > 2 and kernel_fields[2]:
                    kernel_source, kernel_version = kernel_fields[2].split(',')[0].strip().removesuffix(')').split(' (= ', 1)
                installed = {}
                for binary in ['sudo', 'bash', 'coreutils']:
                    installed[binary] = vm.console("dpkg-query --show --showformat='${source:Package}\\t${source:Version}' " + binary).stdout.strip().split('\t')
                sudo_source, sudo_version = installed['sudo']
                bash_source, bash_version = installed['bash']
                coreutils_source, coreutils_version = installed['coreutils']

                def flaw(number):
                    return record_prefix + 'CVE-2026-' + str(number)

                def record(number, *entries):
                    """An OSV.dev record of one flaw, with an entry of (source, ecosystem, events) for each package."""
                    return {'id': flaw(number), 'upstream': ['CVE-2026-' + str(number)], 'affected': [
                        {'package': {'name': source, 'ecosystem': ecosystem}, 'ranges': [{'type': 'ECOSYSTEM', 'events': events}]}
                        for source, ecosystem, events in entries]}

                def fixed_in(version):
                    return [{'introduced': '0'}, {'fixed': version}]

                no_fix_yet = [{'introduced': '0'}]
                # The flaw of the running kernel on CISA's list is on the third
                # page of the kernel's flaws; flaw 19999 and the 2000x flaws are
                # not on CISA's list. hello is the vendor's package.
                flaws = {main_ecosystem: {
                    sudo_source: [flaw(10000), flaw(10001), flaw(19999)],
                    kernel_source: [flaw(20001), flaw(20002), flaw(20003), flaw(20004), flaw(10002)],
                    bash_source: [flaw(10003)],
                    'hello': [flaw(10006)]}}
                records = {
                    flaw(10000): record(10000, (sudo_source, main_ecosystem, fixed_in(sudo_version))),
                    flaw(10001): record(10001, (sudo_source, main_ecosystem, fixed_in(sudo_version + '+test1'))),
                    flaw(10002): record(10002, (kernel_source, main_ecosystem, fixed_in(kernel_version + '+test1'))),
                    flaw(10003): record(10003, (bash_source, main_ecosystem, no_fix_yet)),
                    flaw(10006): record(10006, ('hello', main_ecosystem, no_fix_yet))}
                # Each finding's identity with its reason, installed version,
                # and whether it is the running kernel.
                expected = {
                    'CVE-2026-10001 ' + sudo_source: ('fixed version ' + sudo_version + '+test1 waits to be installed', sudo_version, False),
                    'CVE-2026-10002 ' + kernel_source: ('fixed version ' + kernel_version + '+test1 waits to be installed', kernel_version, True),
                    'CVE-2026-10003 ' + bash_source: ('no fix yet', bash_version, False)}
                if pro_ecosystem:
                    # Ubuntu files some flaws only under Ubuntu Pro, and an
                    # installed fix of Ubuntu Pro is healthy.
                    flaws[pro_ecosystem] = {coreutils_source: [flaw(10004)], bash_source: [flaw(10005)]}
                    flaws[main_ecosystem][bash_source].append(flaw(10005))
                    records[flaw(10004)] = record(10004, (coreutils_source, pro_ecosystem, fixed_in(coreutils_version + '+test1')))
                    records[flaw(10005)] = record(10005, (bash_source, main_ecosystem, no_fix_yet), (bash_source, pro_ecosystem, fixed_in(bash_version)))
                    expected['CVE-2026-10004 ' + coreutils_source] = ('fix needs Ubuntu Pro, version ' + coreutils_version + '+test1', coreutils_version, False)

                def stand_in_answers(listed=(10000, 10001, 10002, 10003, 10004, 10005, 10006), cisa_status=200, osv_status=200, osv_flaws=None):
                    """Replace the stand-in's answers, with CISA's list of the flaws numbered in listed, and forget the batches it received."""
                    cisa = {'title': 'CISA Catalog of Known Exploited Vulnerabilities', 'catalogVersion': '2026.10.26', 'dateReleased': '2026-10-26T22:24:24.6101Z', 'count': len(listed),
                            'vulnerabilities': [{'cveID': 'CVE-2026-' + str(number), 'vendorProject': 'Test', 'product': 'Test',
                                                 'vulnerabilityName': 'An exploited flaw of the test', 'dateAdded': '2026-10-26'} for number in listed]}
                    answers = {'cisa': {'status': cisa_status, 'body': cisa}, 'osv_status': osv_status,
                               'flaws': flaws if osv_flaws is None else osv_flaws, 'records': records, 'ids_per_page': 2}
                    vm.write(stand_in + '/answers.new', json.dumps(answers))
                    vm.console('mv ' + stand_in + '/answers.new ' + stand_in + '/answers.json; rm -f ' + stand_in + '/batches.jsonl')

                stand_in_answers()
                vm.console('systemd-run --unit=mmwatchdog-test-cisa-and-osv --property=Type=exec /usr/bin/python3 ' + stand_in + '/cisa_and_osv_stand_in.py')
                wait(lambda: vm.console('test -f ' + stand_in + '/ready', check=False).returncode == 0)
                groups['installed_packages'] = dict(quick, check_every='1h', time_limit_per_run='1m', checks={
                    'known_exploited_flaws': {
                        'known_exploited_vulnerabilities_in_installed_packages': {'fail_when': 'an_installed_package_is_affected'},
                        'alert_severity': 'warn'}})
                reload()

                def flaws_now_are(expected_status, expected_message):
                    observed = now('installed_packages')['known_exploited_flaws']
                    assert observed['status'] == expected_status and observed['message'].startswith(expected_message), observed
                    return observed

                affected = flaws_now_are('failed', str(len(expected)) + ' known exploited flaws affect installed packages: ')
                findings = {finding['id']: finding['message'] for finding in affected['findings']}
                assert findings == {
                    identity: identity.split()[0] + ' in ' + identity.split()[1] + ' ' + version + (', the running kernel' if running else '') + ': ' + reason
                    + ' (CISA: An exploited flaw of the test; ' + tracker + identity.split()[0] + ')'
                    for identity, (reason, version, running) in expected.items()}, (findings, expected)

                # Names only: each query holds a source package's name and an
                # ecosystem of the release, and OSV.dev's page token when it asks
                # for a further page. The first round asks every source package
                # in every ecosystem. The batches can hold more than one run, since
                # the reload and the test's own check can both run the check, so
                # the queries are compared as a set; the Go tests show that one
                # run asks each query once.
                batches = [json.loads(line) for line in vm.console('cat ' + stand_in + '/batches.jsonl').stdout.splitlines()]
                first_round, names = [], set()
                for batch in batches:
                    assert set(batch) == {'queries'} and len(batch['queries']) <= 1000, (set(batch), len(batch['queries']))
                    for query in batch['queries']:
                        assert set(query) <= {'package', 'page_token'} and set(query['package']) == {'name', 'ecosystem'}, query
                        assert query['package']['ecosystem'] in ecosystems and re.fullmatch('[a-z0-9][a-z0-9+.-]+', query['package']['name']), query
                        names.add(query['package']['name'])
                        if 'page_token' not in query:
                            first_round.append((query['package']['name'], query['package']['ecosystem']))
                assert set(first_round) == {(name, ecosystem) for name in names for ecosystem in ecosystems}, first_round
                assert {sudo_source, kernel_source, bash_source, coreutils_source} <= names and 'hello' not in names, names
                assert any('page_token' in query for batch in batches for query in batch['queries']), 'the check asked for no further page'

                stand_in_answers(listed=(10000,))
                healthy = flaws_now_are('ok', '')
                assert healthy['message'] == ("no installed package has a flaw on CISA's list of known exploited vulnerabilities of 2026-10-26: MMWatchdog searched OSV.dev for "
                                              + str(len(names)) + ' source packages of ' + ' and '.join(ecosystems)), healthy
                # OSV.dev answers a release that it does not know with no flaws;
                # flaws filed only under Ubuntu Pro do not show that it knows the
                # release.
                for answers, message in [
                        ({'osv_flaws': {pro_ecosystem: flaws[pro_ecosystem]} if pro_ecosystem else {}},
                         'OSV.dev knows no flaw of any of the ' + str(len(names)) + ' source packages in ' + main_ecosystem + ', so it may not cover this release'),
                        ({'osv_status': 503}, "api.osv.dev answered HTTP 503 for OSV.dev's flaws of the installed source packages"),
                        ({'cisa_status': 503}, "raw.githubusercontent.com answered HTTP 503 for CISA's list of known exploited vulnerabilities")]:
                    stand_in_answers(**answers)
                    observed = flaws_now_are('unknown', message)
                    assert observed['message'] == message and not observed.get('findings'), observed
                mark('a known_exploited_vulnerabilities_in_installed_packages check downloads CISA\'s list and asks OSV.dev over HTTPS, page by page, with only the real guest\'s source package names in ' + ' and '.join(ecosystems)
                     + ', with the running kernel\'s source package and without a package from a vendor apt origin; it compares the versions on the guest and reports ' + str(len(expected)) + ' findings with their reasons, is healthy without an affected flaw, '
                     'and is unknown when OSV.dev knows no flaw of the release, when OSV.dev answers HTTP 503, and when CISA does')
            finally:
                try:
                    if 'installed_packages' in groups:
                        del groups['installed_packages']
                        reload()
                finally:
                    remove_cisa_and_osv_stand_in()
                    vm.console('systemctl restart mmwatchdog')
        else:
            # Only Debian and Ubuntu: the check answers before it asks CISA or
            # OSV.dev, so the guest needs no stand-in and no package tools.
            groups['installed_packages'] = dict(quick, check_every='1h', time_limit_per_run='1m', checks={
                'known_exploited_flaws': {
                    'known_exploited_vulnerabilities_in_installed_packages': {'fail_when': 'an_installed_package_is_affected'},
                    'alert_severity': 'warn'}})
            reload()
            observed = now('installed_packages')['known_exploited_flaws']
            expected = {
                'dnf': 'MMWatchdog does not check ' + pretty_name + ' for known exploited flaws, because the vulnerability data of the Red Hat family is incomplete (see the page Limits on each distribution in the MMWatchdog documentation): the check dnf_security_updates alerts on every waiting security update',
                'zypper': 'MMWatchdog does not check ' + pretty_name + ' for known exploited flaws, because the vulnerability data of this SUSE release is incomplete (see the page Limits on each distribution in the MMWatchdog documentation): the check zypper_security_patches alerts on every waiting security patch',
            }.get(package_manager, 'MMWatchdog does not know the installed packages of ' + pretty_name + ': it knows only those of Debian and Ubuntu (see the page Limits on each distribution in the MMWatchdog documentation)')
            assert observed['status'] == 'unknown' and observed['message'] == expected and not observed.get('findings'), observed
            del groups['installed_packages']
            reload()
            mark('a known_exploited_vulnerabilities_in_installed_packages check is unknown on ' + pretty_name + ', and its message gives the reason and, on the Red Hat family and SUSE, names the check of the waiting security fixes')

        # A software RAID array of two loop devices: whole, with a failed
        # device, rebuilt, and stopped.
        if vm.console('command -v mdadm', check=False).returncode != 0:
            vm.install_packages('mdadm')
        first, second = (vm.console('truncate -s 64M ' + FIXTURE + '/raid-' + name + '; losetup --find --show ' + FIXTURE + '/raid-' + name).stdout.strip() for name in ['a', 'b'])
        vm.console('yes | mdadm --create ' + ARRAY + ' --run --level=1 --raid-devices=2 --metadata=1.2 ' + first + ' ' + second + ' >/dev/null 2>&1; mdadm --wait ' + ARRAY + '; true', timeout=300)
        groups['raid'] = dict(quick, checks={
            'array_has_all_its_devices': {'mdadm_array': ARRAY, 'alert_severity': 'sos'},
            'array_that_was_never_there': {'mdadm_array': '/dev/md/mmwatchdog-no-such-array', 'alert_severity': 'info'}})
        reload()
        becomes('raid/array_has_all_its_devices', 'ok', ARRAY + ' (raid1) has all expected devices: 2 of 2 devices work')
        becomes('raid/array_that_was_never_there', 'failed', 'MMWatchdog cannot resolve the device path /dev/md/mmwatchdog-no-such-array: ')
        seen = len(reports())
        vm.console('mdadm ' + ARRAY + ' --fail ' + second + ' >/dev/null 2>&1')
        degraded = becomes('raid/array_has_all_its_devices', 'failed', ARRAY + ' (raid1) is degraded: 1 of 2 devices work; the kernel reports as faulty: ' + second.rsplit('/', 1)[1])
        alert = wait(lambda: [e for e in reports()[seen:] if e.get('check') == 'array_has_all_its_devices' and e['type'] == 'failed'])[0]
        assert alert['alert_severity'] == 'sos' and alert['message'] == degraded['message'], alert
        vm.console('mdadm ' + ARRAY + ' --remove ' + second + ' >/dev/null 2>&1')
        becomes('raid/array_has_all_its_devices', 'failed', ARRAY + ' (raid1) is degraded: 1 of 2 devices work')
        vm.console('mdadm ' + ARRAY + ' --add ' + second + ' >/dev/null 2>&1; mdadm --wait ' + ARRAY + '; true', timeout=300)
        becomes('raid/array_has_all_its_devices', 'ok', ARRAY + ' (raid1) has all expected devices: 2 of 2 devices work')
        vm.console('mdadm --stop ' + ARRAY + ' >/dev/null 2>&1')
        stopped_array = becomes('raid/array_has_all_its_devices', 'failed')
        assert ARRAY in stopped_array['message'], stopped_array
        vm.console('losetup --detach ' + first + ' ' + second)
        del groups['raid']
        mark('an mdadm_array check of a real RAID 1 array on two loop devices is healthy while the array is whole, fails with the faulty device when one fails and while the device is missing, recovers after the rebuild, and fails when the array is stopped')

        # The temperature: a virtual machine has no sensor of the processors.
        groups['temperature'] = dict(quick, node_temperature='coretemp', fail_when_measurement_above={'highest_temperature_celsius': 85}, alert_severity='warn')
        reload()
        becomes('temperature/node_temperature', 'unknown', 'this machine has no temperature sensor named coretemp; ')
        del groups['temperature']
        reload()
        mark('a node_temperature check is unknown on a guest that has no temperature sensor of the name, and says which sensors the guest has')
    finally:
        vm.console(vm.each_unit('stop', [*TRANSIENT_UNITS, UNIT]) + '; ' + vm.each_unit('reset-failed', TRANSIENT_UNITS) + '; rm -f ' + FRAGMENT + ' /etc/systemd/system/' + UNIT + '; systemctl daemon-reload; ip link del ' + INTERFACE + ' 2>/dev/null; true')
        if 'original_destinations' in locals():
            vm.write(ALERT_DESTINATIONS, original_destinations)
        vm.console('systemctl is-active --quiet mmwatchdog || systemctl start mmwatchdog')
        cli('reload', 0)
        unmount()
        remove_array()


def remove_cisa_and_osv_stand_in():
    """Remove the stand-in for CISA and OSV.dev, the vendor repository, and
    their guest settings, also after an interrupted run."""
    vm.console('systemctl stop mmwatchdog-test-cisa-and-osv.service 2>/dev/null; '
               "sed -i '/# mmwatchdog-test-cisa-and-osv$/d' /etc/hosts; true")
    vm.distrust_certificate(CISA_AND_OSV_CERTIFICATE_NAME)
    # Reset must not remove a distribution's hello package when the vendor
    # fixture was never installed, or a run stopped before that installation.
    vm.console("if [ \"$(dpkg-query --show --showformat='${Version}' hello 2>/dev/null)\" = 99.0-vendor ]; then "
               'export DEBIAN_FRONTEND=noninteractive; apt-get -o DPkg::Lock::Timeout=600 purge -y -qq hello >/dev/null; fi', timeout=600)
    vm.console('rm -f ' + VENDOR_REPOSITORY_FILE + '; '
               'rm -rf ' + CISA_AND_OSV_DIR + '; '
               "find /var/lib/apt/lists -maxdepth 1 -name '*mmwatchdog-fixture*cisa-and-osv*vendor*' -delete 2>/dev/null; true")


def unmount():
    """Unmount what the scenarios mounted. vm.reset calls it after an interruption too."""
    for path in [MOUNTED, *FILESYSTEMS.values()]:
        vm.console('while mountpoint -q ' + path + '; do umount ' + path + '; done; true')
    # The NFS filesystem of the test may not answer: /proc/mounts shows it
    # without a question to its server, and a forced, lazy unmount needs
    # no answer.
    vm.console('if grep -q " ' + NETWORK_FILESYSTEM + ' " /proc/mounts; then umount -f -l ' + NETWORK_FILESYSTEM + '; fi; '
               'if [ -e ' + EXPORTS + ' ]; then rm -f ' + EXPORTS + '; exportfs -ra; fi; true')


def remove_array():
    """Stop the array of the test and free its loop devices. vm.reset calls it after an interruption too."""
    vm.console('if [ -e ' + ARRAY + ' ]; then mdadm --stop ' + ARRAY + ' >/dev/null 2>&1; fi; '
               'for device in $(losetup --associated ' + FIXTURE + '/raid-a --output NAME --noheadings 2>/dev/null) $(losetup --associated ' + FIXTURE + '/raid-b --output NAME --noheadings 2>/dev/null); do losetup --detach $device; done; '
               'rm -f /run/reboot-required.pkgs; true')


def history_rows(query):
    """Rows of the node's measurement history, read on the guest."""
    script = "import json, sqlite3; print(json.dumps(sqlite3.connect('file:/var/lib/mmwatchdog/measurement_history.db?mode=ro', uri=True).execute(" + repr(query) + ").fetchall()))"
    return json.loads(vm.console('python3 -c ' + shlex.quote(script)).stdout)
