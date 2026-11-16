"""Run the sustained guest workload under MMWatchdog's whole-environment lock."""
import json
import vm


def suite(sustained):
    vm.up()
    assert vm.console('cat /etc/mmwatchdog-test-vm').stdout.strip() == vm.MARKER
    vm.standalone()
    vm.write('/var/tmp/mmwatchdog-fixture/server.py', (vm.REPO / 'tests/integration/fixture.py').read_text())
    vm.write('/etc/systemd/system/mmwatchdog-fixture.service', '''[Unit]
Description=MMWatchdog sustained-test receiver
[Service]
ExecStart=/usr/bin/python3 /var/tmp/mmwatchdog-fixture/server.py
Restart=no
''')
    vm.console('systemctl daemon-reload; systemctl restart mmwatchdog-fixture')
    vm.write('/var/tmp/mmwatchdog-fixture/soak.py', (vm.REPO / 'tests/integration/soak_guest.py').read_text())
    vm.console('rm -f /var/tmp/mmwatchdog-fixture/soak-result.json /var/tmp/mmwatchdog-fixture/soak-samples.jsonl')
    print(f'Workload on {vm.ARCH}: ' + ('coverage, then sustained steady state' if sustained else 'stop after coverage'), flush=True)
    # The workload runs in a systemd scope, so that "vm.py stop" and the
    # line below can end it by its name. A scope's program is a child of
    # this command and writes to it directly.
    try:
        result = vm.console('systemd-run --scope --quiet --collect --unit=mmwatchdog-soak '
                            '/usr/bin/python3 /var/tmp/mmwatchdog-fixture/soak.py' + (' --sustained' if sustained else ''),
                            timeout=1800 if sustained else 240)
    finally:
        vm.console('systemctl stop mmwatchdog-soak.scope', check=False)
    report = json.loads(result.stdout)
    (vm.ROOT / 'soak-result.json').write_text(json.dumps(report, indent=2) + '\n')
    (vm.ROOT / 'soak-samples.jsonl').write_text(vm.console('cat /var/tmp/mmwatchdog-fixture/soak-samples.jsonl').stdout)
    print(json.dumps(report, indent=2), flush=True)
