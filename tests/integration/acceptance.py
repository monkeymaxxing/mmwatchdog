"""Exercise the shipped CLI and daemon in the isolated MMWatchdog VM."""

from datetime import datetime
import hashlib
import json
import re
import shlex
import subprocess
import time
import mmx_build
import vm


GROUPS = ("acceptance", "diagnostics", "resources", "node", "services", "fleet", "native")


def native_tests():
    root = vm.ROOT
    env = dict(vm.ENV, CGO_ENABLED="0", GOOS="linux", GOARCH=vm.ARCH)
    print("Running the Go tests of every package natively in the Linux guest", flush=True)
    native_logs = []
    # One test program for each package that has tests: go test -c builds
    # one package at a time.
    packages = subprocess.run(
        ["go", "list", "-f", "{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}", "./internal/...", "./cmd/mmwatchdog"],
        cwd=vm.REPO, env=env, capture_output=True, text=True, check=True,
    ).stdout.split()
    for package in packages:
        name = package.replace("/", "-")
        binary = root / ("bin/" + name + "-tests-linux-" + vm.ARCH)
        subprocess.run(
            ["nice", "-n", "19", "go", "test", "-c", "-o", str(binary), package],
            cwd=vm.REPO,
            env=env,
            check=True,
        )
        target = "/tmp/" + name + "-tests"
        vm.copy(binary, target)
        # The tests keep their files in memory. Many of them commit to a
        # SQLite database hundreds of times, and on a guest whose /tmp is on
        # its disk the wait for the disk made the tests five times as long
        # as on Debian 13, whose /tmp is in memory. No test is about that
        # disk.
        native_logs.append(vm.console("TMPDIR=/dev/shm " + target + " -test.v", timeout=1800).stdout)
        vm.console("rm " + target)
        binary.unlink()
    (root / ("linux-tests-" + vm.DISTRIBUTION + "-" + vm.ARCH + ".txt")).write_text(
        "\n".join(native_logs)
    )
    print("Go test suite passes natively on " + vm.DISTRIBUTION + " " + vm.ARCH, flush=True)


def suite(group=None):
    vm.up()
    vm.setup_step_done("the guests are ready")
    vm.build()
    vm.setup_step_done("MMWatchdog is built")
    if group == "native":
        native_tests()
        return
    vm.reset()
    vm.setup_step_done("the fixtures of earlier suites are reset")
    print("Building sibling MMX deployment binaries", flush=True)
    provenance = mmx_build.build()
    vm.setup_step_done("MMX is built")
    root = vm.ROOT
    work = root / "workflow"
    work.mkdir(exist_ok=True)
    passed = []

    marked = [time.monotonic()]

    def mark(name):
        # The seconds since the scenario before, in front of the name: a
        # reader of the log sees where a suite spends its time.
        passed.append(name)
        print(f"PASS {time.monotonic() - marked[0]:4.0f}s {name}", flush=True)
        marked[0] = time.monotonic()

    def setup_step_between_scenarios_done(step):
        # A step of setup after the first scenario, such as the installation
        # of the servers that the services suite checks, gets a line of its
        # own, so that its seconds do not count for the next scenario.
        print(f"SETUP {time.monotonic() - marked[0]:4.0f}s {step}", flush=True)
        marked[0] = time.monotonic()

    def cli(command, code=None):
        p = vm.console("/usr/local/bin/mmwatchdog " + command, check=False)
        if code is not None:
            assert p.returncode == code, (command, p.returncode, p.stdout, p.stderr)
        return p

    def status():
        p = cli("status --json")
        return json.loads(p.stdout)

    def wait(predicate, timeout=25):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                value = predicate()
                if value:
                    return value
            except (json.JSONDecodeError, KeyError):
                pass
            time.sleep(0.2)
        raise AssertionError(
            "timed out; status="
            + cli("status --json").stdout
            + "\n"
            + vm.console("journalctl -u mmwatchdog -n 30 --no-pager", check=False).stdout
        )

    def state(name):
        return status()["checks"][name]

    def events():
        return json.loads(cli("events --json", 0).stdout)

    def reports():
        p = vm.console("cat /var/tmp/mmwatchdog-fixture/webhooks.jsonl", check=False)
        return [json.loads(line) for line in p.stdout.splitlines() if line]

    vm.write(
        "/var/tmp/mmwatchdog-fixture/server.py",
        (vm.REPO / "tests/integration/fixture.py").read_text(),
    )
    vm.write(
        "/etc/systemd/system/mmwatchdog-fixture.service",
        """[Unit]
Description=MMWatchdog test fixture
[Service]
ExecStart=/usr/bin/python3 /var/tmp/mmwatchdog-fixture/server.py
Restart=no
[Install]
WantedBy=multi-user.target
""",
    )
    # The self-signed certificate is a private CA: checks trust it only through trusted_certificate_authority_file.
    vm.console(
        "install -d -m 0700 /etc/mmwatchdog/secrets; openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=localhost -addext subjectAltName=IP:127.0.0.1,DNS:localhost,DNS:app.internal -keyout /etc/mmwatchdog/secrets/key.pem -out /etc/mmwatchdog/secrets/cert.pem >/dev/null 2>&1; chmod 600 /etc/mmwatchdog/secrets/key.pem"
    )
    vm.console("systemctl daemon-reload; systemctl start mmwatchdog-fixture")
    vm.write("/etc/mmwatchdog/secrets/backup", "mmwatchdog-integration-heartbeat-token\n", "0600")
    vm.write("/etc/mmwatchdog/secrets/http-token", "mmwatchdog-integration-http-token\n", "0600")
    vm.write("/var/tmp/mmwatchdog-fixture/pending", "0\n")
    # Every check group that MMWatchdog runs states its four timings.
    timings = {
        "check_every": "800ms",
        "time_limit_per_run": "400ms",
        "alert_after_failing_for": "1s",
        "recovered_after_healthy_for": "500ms",
    }
    config = {'keep_undelivered_alerts_for': '24h', 'undelivered_alerts_content_size_limit': '8MiB',
        "alert_severity_when_this_node_goes_silent": "sos",
        "alert_severity_when_this_node_has_rebooted": "info",
        "keep_measurement_history_for": "7d",
        "measurement_history_size_limit": "256MiB",
        "heartbeat_receiver": {
            "listen_on_address": "127.0.0.1:18444",
            "tls_certificate_file": "/etc/mmwatchdog/secrets/cert.pem",
            "tls_private_key_file": "/etc/mmwatchdog/secrets/key.pem",
        },
        "check_groups": {
            "app": {
                **timings,
                "checks": {
                    "systemd_unit_state": {"systemd_unit_state": {"systemd_unit": "mmwatchdog-fixture.service", "fail_when": "the_unit_is_not_active"}, "alert_severity": "sos"},
                    "health": {
                        "http": {
                            "url": "http://127.0.0.1:18080/health",
                            "response_body_contains": "ready",
                        },
                        "alert_severity": "warn",
                    },
                },
                "restart_systemd_unit": {
                    "systemd_unit": "mmwatchdog-fixture.service",
                    "triggered_by_checks": ["systemd_unit_state", "health"],
                    # The same waiting time as the alert of this check group.
                    "restart_after_failing_for": "1s",
                    "max_attempts": 2,
                    "min_time_between_attempts": "2s",
                    "reset_attempts_after_healthy_for": "4s",
                },
            },
            "pipeline": {**timings, "shell": "false | cat", "alert_severity": "warn"},
            "accepted": {**timings, "shell": {"script": "exit 2", "success_exit_codes": [0, 2]}, "alert_severity": "warn"},
            # The modules only measure; MMWatchdog applies the limit of each check.
            # queued_jobs is a Starlark script and queued_jobs_in_go an executable.
            "queue_starlark": {
                **timings,
                "queued_jobs": {"queue_file_path": "/var/tmp/mmwatchdog-fixture/pending"},
                "fail_when_measurement_above": {"queued_jobs": 500},
                "alert_severity": "warn",
            },
            "queue_go": {
                **timings,
                "queued_jobs_in_go": {"queue_file_path": "/var/tmp/mmwatchdog-fixture/pending"},
                "fail_when_measurement_above": {"queued_jobs": 500},
                "alert_severity": "warn",
            },
            "backup": {
                "heartbeat": {"max_time_between_heartbeats": "4s", "expected_bearer_token_file": "/etc/mmwatchdog/secrets/backup"},
                "alert_severity": "sos",
            },
            "disk": {**timings, "node_filesystem": "/", "fail_when_measurement_above": {"space_used_percent": 99}, "alert_severity": "sos"},
            "memory": {**timings, "node_resource": "memory", "fail_when_measurement_above": {"memory_used_percent": 99}, "alert_severity": "warn"},
            "tcp": {**timings, "tcp": "127.0.0.1:18080", "alert_severity": "warn"},
            "identity": {**timings, "shell": "id -un", "alert_severity": "info"},
            "certificate": {
                **timings,
                "tls": {"address": "127.0.0.1:18444", "trusted_certificate_authority_file": "/etc/mmwatchdog/secrets/cert.pem", "fail_when_certificate_expires_within": "1h"},
                "alert_severity": "warn",
            },
            "expiring": {
                **timings,
                "tls": {"address": "127.0.0.1:18444", "trusted_certificate_authority_file": "/etc/mmwatchdog/secrets/cert.pem", "fail_when_certificate_expires_within": "48h"},
                "alert_severity": "warn",
            },
            # A long alert_after_failing_for keeps the token rotation below from alerting.
            "private": {
                **timings,
                "alert_after_failing_for": "1m",
                "http": {
                    "url": "https://127.0.0.1:18443/private",
                    "headers": {"Host": "app.internal"},
                    "bearer_token_file": "/etc/mmwatchdog/secrets/http-token",
                    "trusted_certificate_authority_file": "/etc/mmwatchdog/secrets/cert.pem",
                    "response_body_contains": "ready",
                },
                "alert_severity": "warn",
            },
        },
    }
    vm.write_configuration_files(work, config)
    alerts = {"destinations": {"capture": {'remind_every': 'never', "webhook": {"url": "http://127.0.0.1:18080/events"}, "time_limit_per_delivery_attempt": "1s", "alert_severities": ["info", "warn", "sos"]}}}
    (work / "alert_destinations.yml").write_text(json.dumps(alerts, indent=2))
    # The module that is a Go executable has its own build, never linked into MMWatchdog.
    env = dict(vm.ENV, CGO_ENABLED="0", GOOS="linux", GOARCH=vm.ARCH)
    subprocess.run(
        [
            "go",
            "build",
            "-ldflags=-s -w",
            "-o",
            str(root / "bin/queued_jobs_in_go"),
            str(vm.REPO / "examples/modules/queued_jobs_in_go/queued_jobs_in_go.go"),
        ],
        cwd=vm.REPO,
        env=env,
        check=True,
    )
    (work / "inventories").mkdir(exist_ok=True)
    (work / "playbooks").mkdir(exist_ok=True)
    node = vm.connection()
    (work / "inventories/acceptance.yml").write_text(json.dumps({"nodes": {"mmwatchdog": node}}))
    node_tasks = [
        {"id": "create_group", "group": {"name": "mmwatchdog", "system": True}},
        {"id": "create_check_user", "user": {"name": "mmwatchdog", "system": True, "primary_group": "mmwatchdog", "home": "/var/lib/mmwatchdog-checks", "create_home_directory": False}},
        {"id": "check_home", "directory": {"path": "/var/lib/mmwatchdog-checks", "owner": "mmwatchdog", "group": "mmwatchdog", "permissions_mode": "0700"}},
    ]
    for path, mode in [
        ("/etc/mmwatchdog", "0755"),
        ("/etc/mmwatchdog/check_groups", "0755"),
        ("/usr/local/lib/mmwatchdog", "0755"),
        ("/usr/local/lib/mmwatchdog/external_modules", "0755"),
        ("/usr/local/lib/mmwatchdog/external_modules/queued_jobs", "0755"),
        ("/usr/local/lib/mmwatchdog/external_modules/queued_jobs_in_go", "0755"),
    ]:
        node_tasks.append({"id": "directory_" + path.strip("/").replace("/", "_"), "directory": {"path": path, "permissions_mode": mode}})
    for path, source, mode in [
        ("/usr/local/bin/mmwatchdog", root / ("bin/mmwatchdog-linux-" + vm.ARCH), "0755"),
        # A module is a directory: its description, and its script or its executable.
        ("/usr/local/lib/mmwatchdog/external_modules/queued_jobs/module.star", vm.REPO / "examples/modules/queued_jobs/module.star", "0644"),
        ("/usr/local/lib/mmwatchdog/external_modules/queued_jobs/module_description.yml", vm.REPO / "examples/modules/queued_jobs/module_description.yml", "0644"),
        ("/usr/local/lib/mmwatchdog/external_modules/queued_jobs_in_go/module_executable", root / "bin/queued_jobs_in_go", "0755"),
        ("/usr/local/lib/mmwatchdog/external_modules/queued_jobs_in_go/module_description.yml", vm.REPO / "examples/modules/queued_jobs_in_go/module_description.yml", "0644"),
        ("/etc/systemd/system/mmwatchdog.service", vm.REPO / "examples/mmwatchdog.service", "0644"),
        ("/etc/mmwatchdog/alert_destinations.yml", work / "alert_destinations.yml", "0644"),
        ("/etc/mmwatchdog/mmwatchdog_node_settings.yml", work / "mmwatchdog_node_settings.yml", "0644"),
        ("/etc/mmwatchdog/check_groups/node.yml", work / "check_groups/node.yml", "0644"),
    ]:
        item = {"path": path, "source": str(source), "permissions_mode": mode}
        node_tasks.append({"id": "file_" + path.strip("/").replace("/", "_").replace(".", "_").replace("-", "_"), "file": item})
    node_tasks += [
        {"id": "validate_configuration", "shell": {"script": "/usr/local/bin/mmwatchdog validate --config-dir /etc/mmwatchdog", "changed_when": False}},
        {"id": "reload_systemd", "shell": {"script": "systemctl daemon-reload", "changed_when": False}},
        {"id": "start_daemon", "systemd_service": {"name": "mmwatchdog", "state": "running", "start_at_boot": True}},
    ]
    (work / "mmx_project_settings.yml").write_text(json.dumps({
        "project_name": "mmwatchdog_acceptance", "default_inventory": "acceptance",
    }))
    # One fleet stage runs the node tasks in order. A fleet stage of its own
    # for each task would cost MMX a round trip to the node for each of
    # them, up to half a second each, in both runs of every suite.
    playbook = {
        "fleet_stages": [
            {"id": "install", "nodes": "all", "node_tasks": node_tasks},
        ],
    }
    (work / "playbooks/acceptance.yml").write_text(json.dumps(playbook, indent=2))
    mmx = root / "mmx-bin/mmx"
    for command in ["validate", "run", "run"]:
        p = vm.cmd(
            [
                mmx,
                command,
                "acceptance",
                "--inventory",
                "acceptance",
                "--json",
            ],
            cwd=work,
            extra_env={"MMX_RUNNER_DIR": str(root / "mmx-bin"),
                       "MMX_STATE_DIR": str(root / "mmx-state")},
            timeout=300,
            check=False,
        )
        (root / (command + "-result.json")).write_text(p.stdout)
        if p.returncode:
            report = json.loads(p.stdout)
            failures = [
                {"fleet_stage": entry.get("fleet_stage"), "response": entry.get("response")}
                for entry in report.get("entries", [])
                if entry.get("response", {}).get("status") != "succeeded"
            ]
            raise AssertionError(json.dumps(failures, indent=2)[-6000:])
    wait(lambda: state("app/health")["last_observation"]["status"] == "ok")
    mark(
        "sibling MMX build deploys binary, validated config, external modules and systemd unit; repeat run succeeds"
    )
    vm.standalone()
    if group in {None, "acceptance"}:
        cli("check-now accepted --json", 0)
        assert json.loads(cli("check-now pipeline --json", 1).stdout)['shell']["exit_code"] == 1
        assert state("accepted/shell")["last_observation"]["exit_code"] == 2
        mark(
            "Bash pipelines use pipefail; configured success codes retain the actual exit code"
        )
        assert (
            json.loads(cli("check-now identity --json", 0).stdout)['shell']["stdout"].strip()
            == "mmwatchdog"
        )
        mark("custom checks execute as the unprivileged mmwatchdog account")
        # The two example modules do the same work: one is a Starlark script
        # that MMWatchdog runs itself, the other an executable that speaks the JSON
        # protocol. Each reports the measurement queued_jobs as a plain
        # number, MMWatchdog takes the unit from the module's description, and MMWatchdog
        # applies the check's limit of 500.
        modules = {"queue_starlark": "queued_jobs", "queue_go": "queued_jobs_in_go"}
        for name, module in modules.items():
            healthy = json.loads(cli("check-now " + name + " --json", 0).stdout)[module]
            assert healthy["message"] == "queued_jobs is 0 (fails above 500); the module reported: 0 queued jobs", healthy
        vm.write("/var/tmp/mmwatchdog-fixture/pending", "780\n")
        for name, module in modules.items():
            above = json.loads(cli("check-now " + name + " --json", 1).stdout)[module]
            assert above["status"] == "failed" and above["measurements"]["queued_jobs"] == {"value": 780, "unit": "count"}, above
            assert above["message"] == "queued_jobs is 780 (fails above 500); the module reported: 780 queued jobs", above
        vm.write("/var/tmp/mmwatchdog-fixture/pending", "0\n")
        mark(
            "a module of the project's own that is a Starlark script and one that is an independently built Go executable report a measurement; MMWatchdog takes its unit from the module's description and applies the check's limit to it for healthy and failed observations"
        )
        # The configuration names every module the same way, so MMWatchdog says which
        # kind each one is and where its files are.
        used = {m["name"]: m for m in json.loads(cli("module-usage --json", 0).stdout)["modules"]}
        assert used["http"]["kind"] == "core" and "directory" not in used["http"], used["http"]
        assert (used["queued_jobs"]["kind"], used["queued_jobs"]["directory"]) == ("project_starlark", "/usr/local/lib/mmwatchdog/external_modules/queued_jobs"), used["queued_jobs"]
        assert (used["queued_jobs_in_go"]["kind"], used["queued_jobs_in_go"]["directory"]) == ("project_executable", "/usr/local/lib/mmwatchdog/external_modules/queued_jobs_in_go"), used["queued_jobs_in_go"]
        assert [at["file"] for at in used["queued_jobs"]["called_at"]] == ["/etc/mmwatchdog/check_groups/node.yml"], used["queued_jobs"]
        usage = cli("module-usage", 0).stdout
        # The names stand in one column, as wide as the longest name.
        assert re.search(r"^  queued_jobs +/usr/local/lib/mmwatchdog/external_modules/queued_jobs/   Starlark$", usage, re.M), usage
        assert re.search(r"^  queued_jobs_in_go +/usr/local/lib/mmwatchdog/external_modules/queued_jobs_in_go/   executable$", usage, re.M), usage
        assert usage.count("MMWatchdog does not see what an executable does on this node.\n") == 1 and usage.count("The local node must provide everything the executable needs.\n") == 1, usage
        assert cli("module-usage tcp", 0).stdout.startswith("Core modules (built into MMWatchdog)\n  tcp   /etc/mmwatchdog/check_groups/node.yml:"), cli("module-usage tcp", 0).stdout
        assert "no check or alert destination in configuration directory /etc/mmwatchdog uses module slack" in cli("module-usage slack", 4).stderr
        assert (state("queue_starlark/queued_jobs")["module"], state("queue_starlark/queued_jobs")["module_kind"]) == ("queued_jobs", "project_starlark")
        assert state("tcp/tcp")["module_kind"] == "core"
        shown = cli("status").stdout
        assert "  module queued_jobs: the project's own, Starlark\n" in shown and "  module queued_jobs_in_go: the project's own, executable\n" in shown, shown
        assert "module tcp" not in shown, shown
        described = cli("describe-module queued_jobs", 0).stdout
        assert described.startswith("queued_jobs: a module of the project's own (Starlark), used as a check\nCounts the jobs that wait in a queue file.\nFiles: /usr/local/lib/mmwatchdog/external_modules/queued_jobs/\n"), described
        assert "  queue_file_path   string   required   " in described and "  queued_jobs   count   " in described, described
        assert cli("describe-module http --reference-only", 0).stdout.startswith("http: a core module (built into MMWatchdog), used as a check\n")
        page = cli("describe-module http", 0).stdout
        assert page.startswith("# `http`\n") and "\n## Inputs\n" in page and "\n## Why it works this way\n" in page, page
        mark(
            "mmwatchdog module-usage lists the modules that the node's configuration names, with the kind of each, its files, and the place of each call, and says of an executable that MMWatchdog does not see what it does; mmwatchdog status names the module and its kind under a check whose module is not core; mmwatchdog describe-module prints a core module's whole page, and with --reference-only its inputs and measurements"
        )
        wait(lambda: state("pipeline/shell")["failure_incident"]["incident_id"])
        old = state("pipeline/shell")["failure_incident"]["incident_id"]
        # Every alert has a title, one short line that is complete alone, and
        # a body: the message, since when the check has been failing, and an
        # excerpt of the evidence. The webhook gets both with the event.
        announced_failure = wait(lambda: [
            r["event"] for r in reports()
            if r["accepted"] and r["event"]["type"] == "failed" and r["event"].get("incident_id") == old
        ])[0]
        assert announced_failure["title"] == announced_failure["node"] + ": pipeline/shell failed", announced_failure
        assert "title_was_shortened" not in announced_failure, announced_failure
        body = announced_failure["body"].split("\n")
        assert body[0] == announced_failure["message"] and body[1].startswith("failing since ") and body[1].endswith(" UTC"), body
        assert "exit code: 1" in body, body
        assert announced_failure["evidence"]["failing_since"].startswith("20"), announced_failure
        mark("a failure alert reaches the webhook with a title that names the node and the check, and a body with the message, the start of the failing time, and the exit code")
        before = len(
            [e for e in events() if e.get("check_group") == "pipeline" and e["type"] == "failed"]
        )
        cli("check-now pipeline --json", 1)
        assert state("pipeline/shell")["failure_incident"]["incident_id"] == old
        assert (
            len([e for e in events() if e.get("check_group") == "pipeline" and e["type"] == "failed"])
            == before
        )
        mark("diagnostic test does not change incidents or send extra notifications")
        vm.write("/etc/mmwatchdog/mmwatchdog_node_settings.yml", "check_groups: {bad: {typo: true}}\n")
        rejected = cli("reload", 4)
        assert "unknown field" in rejected.stderr and '{"error":' not in rejected.stderr
        assert state("pipeline/shell")["failure_incident"]["incident_id"] == old
        vm.write_configuration(config)
        cli("reload", 0)
        assert state("pipeline/shell")["failure_incident"]["incident_id"] == old
        mark("invalid reload keeps monitoring; unchanged valid reload preserves incidents")
        # Read the same persisted history with and without a daemon. Reuse
        # this scenario's restart and its existing wait for fresh checks.
        reading_text = cli("readings memory node_resource --last 1h", 0).stdout
        assert "node_resource\nmemory_used_percent, percent · fails above 99" in reading_text, reading_text
        vm.console("systemctl stop mmwatchdog")
        stored_readings = json.loads(cli("readings memory node_resource --last 7d --json", 0).stdout)
        memory_readings = stored_readings["nodes"][0]["measurements"][0]
        assert memory_readings["summary"]["reading_count"] > 0, stored_readings
        assert all(reading["fail_when_measurement_above"] == 99 for reading in memory_readings["readings"]), stored_readings
        vm.console("systemctl start mmwatchdog")
        wait(lambda: state("pipeline/shell")["last_observation"]["status"] == "failed")
        assert state("pipeline/shell")["failure_incident"]["incident_id"] == old
        assert (
            len([e for e in events() if e.get("check_group") == "pipeline" and e["type"] == "failed"])
            == before
        )
        mark("daemon restart preserves incidents without duplicate failure events")
        # A destination's remind_every takes any interval. With 2s, the
        # destination gets the open pipeline incident again every two seconds.
        vm.write("/etc/mmwatchdog/alert_destinations.yml", json.dumps({"destinations": {"capture": dict(alerts["destinations"]["capture"], remind_every="2s")}}, indent=2))
        cli("reload", 0)

        def reminders():
            # Delivery is at least once, so a reminder may arrive twice.
            return list({
                r["event"]["id"]: r["event"]
                for r in reports()
                if r["accepted"] and r["event"]["type"] == "reminder" and r["event"].get("incident_id") == old
            }.values())

        reminded = wait(lambda: len(reminders()) >= 3 and reminders())
        vm.write("/etc/mmwatchdog/alert_destinations.yml", json.dumps(alerts, indent=2))
        cli("reload", 0)
        assert all(e["destination"] == "capture" and e["message"].startswith("pipeline/shell still failing since ") for e in reminded), reminded
        assert all(e["title"] == e["node"] + ": pipeline/shell still failing" and e["body"].startswith(e["message"]) for e in reminded), reminded
        reminded_at = [datetime.fromisoformat(e["at"]) for e in reminded]
        # datetime keeps microseconds, so allow for the digits it drops.
        assert all((later - earlier).total_seconds() >= 1.99 for earlier, later in zip(reminded_at, reminded_at[1:])), reminded_at
        mark("a destination with a remind_every of 2s gets an open incident again every two seconds")
        # Suppressed observations remain visible, with no replay after healthy maintenance.
        def maintenance(args):
            vm.console("SUDO_USER=deployer /usr/local/bin/mmwatchdog maintenance " + args)

        maintenance("on --every-check-group --for 30s --reason 'deploy v1.4'")
        window = status()["maintenance"]["every_check_group"]
        assert window["reason"] == "deploy v1.4" and window["user"] == "deployer", window
        vm.console("touch /var/tmp/mmwatchdog-fixture/fail")
        wait(lambda: state("app/health")["failure_incident"].get("incident_id"))
        assert status()["systemd_unit_restart_attempts"]["mmwatchdog-fixture.service"]["attempts_used"] == 0
        assert not [
            x
            for x in reports()
            if x["event"].get("check_group") == "app" and x["event"]["type"] == "failed"
        ]
        vm.console("rm /var/tmp/mmwatchdog-fixture/fail")
        wait(lambda: not state("app/health")["failure_incident"].get("incident_id"))
        cli("maintenance on --every-check-group --for 0s", 4)
        assert status()["maintenance"]["every_check_group"] == window
        maintenance("off --every-check-group")
        wait(lambda: not state("app/health")["needs_fresh_observation"])
        assert not [
            x
            for x in reports()
            if x["event"].get("check_group") == "app" and x["event"]["type"] == "failed"
        ]
        def maintenance_alerts():
            es = [x["event"] for x in reports() if x["accepted"] and x["event"].get("check_group") == "every_check_group"]
            return es if len(es) == 2 else None

        announced = wait(maintenance_alerts)
        assert [e["type"] for e in announced] == ["maintenance_on", "maintenance_off"], announced
        assert all(e["maintenance"]["reason"] == "deploy v1.4" and e["maintenance"]["user"] == "deployer" for e in announced), announced
        assert announced[1]["message"] == "maintenance turned off by deployer: deploy v1.4", announced
        mark(
            "whole-server maintenance alerts its start and end with reason and user, observes but suppresses alerts and recovery, and does not replay resolved failure"
        )
        # Frequent deploys can retain maintenance history without webhook noise.
        for end in ("off", "expiry"):
            reason = "quiet deploy " + end
            duration = "30s" if end == "off" else "2s"
            maintenance("on --every-check-group --for " + duration + " --reason '" + reason + "' --without-maintenance-on-and-off-alerts")
            assert status()["maintenance"]["every_check_group"]["without_maintenance_on_and_off_alerts"]
            assert "maintenance start and end are not announced" in vm.console("/usr/local/bin/mmwatchdog status", check=False).stdout
            if end == "off":
                vm.console("systemctl restart mmwatchdog")
                wait(lambda: status()["maintenance"]["every_check_group"]["without_maintenance_on_and_off_alerts"])
                maintenance("off --every-check-group")
            else:
                wait(lambda: "every_check_group" not in status()["maintenance"])
            quiet_events = [e for e in events() if e.get("maintenance", {}).get("reason") == reason]
            assert [e["type"] for e in quiet_events] == ["maintenance_off", "maintenance_on"], quiet_events
            assert all(e["maintenance"]["without_maintenance_on_and_off_alerts"] for e in quiet_events)
            # The notice is delivered after any earlier queued event, so
            # absence at the webhook is checked after delivery made progress.
            cli("send-notice --destination capture --alert-severity info --title 'quiet maintenance verification'", 0)
            assert not [r for r in reports() if r["event"].get("maintenance", {}).get("reason") == reason]
        mark("quiet maintenance survives restart, records start and end, and sends neither off nor expiry to any webhook")
        # Outage persists through bounded recovery, including restart of the monitoring daemon.
        vm.console("touch /var/tmp/mmwatchdog-fixture/reject /var/tmp/mmwatchdog-fixture/fail")
        wait(lambda: status()["systemd_unit_restart_attempts"]["mmwatchdog-fixture.service"]["attempts_used"] == 2)
        incident = state("app/health")["failure_incident"]["incident_id"]
        vm.console("systemctl restart mmwatchdog")
        wait(lambda: state("app/health")["last_observation"]["status"] == "failed")
        assert status()["systemd_unit_restart_attempts"]["mmwatchdog-fixture.service"]["attempts_used"] == 2
        vm.console("rm /var/tmp/mmwatchdog-fixture/fail")
        wait(lambda: not state("app/health")["failure_incident"].get("incident_id"))
        vm.console("rm /var/tmp/mmwatchdog-fixture/reject")

        def delivered():
            es = [
                r["event"]
                for r in reports()
                if r["accepted"] and r["event"].get("incident_id") == incident
            ]
            return es if any(e["type"] == "recovered" for e in es) else None

        delivered_events = wait(delivered, 45)
        assert [e["type"] for e in delivered_events] == ["failed", "recovered"], (
            delivered_events
        )
        failed_attempts = [
            r["event"]["id"]
            for r in reports()
            if r["event"].get("incident_id") == incident and r["event"]["type"] == "failed"
        ]
        assert len(set(failed_attempts)) == 1
        wait(lambda: status()["systemd_unit_restart_attempts"]["mmwatchdog-fixture.service"]["attempts_used"] == 0)
        mark(
            "shared systemd unit restart attempts survive daemon restart; ordered durable webhook retries retain event IDs; sustained health resets recovery attempts"
        )
        wait(lambda: state("backup/heartbeat")["failure_incident"].get("incident_id"))
        cli(
            "heartbeat backup --receiver-url https://127.0.0.1:18444 --bearer-token-file /etc/mmwatchdog/secrets/backup --trusted-certificate-authority-file /etc/mmwatchdog/secrets/cert.pem",
            0,
        )
        heartbeat = state("backup/heartbeat")
        assert not heartbeat["failure_incident"].get("incident_id")
        vm.console("systemctl restart mmwatchdog")
        wait(lambda: state("accepted/shell")["last_observation"]["status"] == "ok")
        assert state("backup/heartbeat")["next_due_by"] == heartbeat["next_due_by"]
        assert state("backup/heartbeat")["last_received_at"] == heartbeat["last_received_at"]
        mark(
            "authenticated HTTPS heartbeat resolves immediately and preserves receipt/deadline across restart"
        )
        vm.write("/etc/mmwatchdog/secrets/wrong", "wrong-heartbeat-token-for-test\n", "0600")
        receipt = state("backup/heartbeat")["last_received_at"]
        cli(
            "heartbeat backup --receiver-url https://127.0.0.1:18444 --bearer-token-file /etc/mmwatchdog/secrets/wrong --trusted-certificate-authority-file /etc/mmwatchdog/secrets/cert.pem",
            1,
        )
        assert state("backup/heartbeat")["last_received_at"] == receipt
        # A job of another account on the same node reports through the
        # heartbeat socket, with no certificate and no address.
        socket = "/run/mmwatchdog/heartbeat.sock"
        job = "runuser -u mmwatchdog-job -- "
        vm.console(
            "id mmwatchdog-job >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin mmwatchdog-job; "
            "install -d -m 0755 /var/tmp/mmwatchdog-job; chown mmwatchdog-job /var/tmp/mmwatchdog-job"
        )
        for file, token in (("good", "mmwatchdog-integration-heartbeat-token"), ("wrong", "wrong-heartbeat-token-for-test")):
            vm.write(f"/var/tmp/mmwatchdog-job/{file}", token + "\n", "0600")
            vm.console(f"chown mmwatchdog-job /var/tmp/mmwatchdog-job/{file}")
        send = lambda file: vm.console(f"{job}/usr/local/bin/mmwatchdog heartbeat backup --bearer-token-file /var/tmp/mmwatchdog-job/{file}", check=False)
        socket_mode = vm.console(f"stat -c '%a %U' {socket}").stdout.strip()
        assert socket_mode == "666 root", socket_mode
        receipt = state("backup/heartbeat")["last_received_at"]
        time.sleep(1.1)
        sent = send("good")
        assert sent.returncode == 0 and "Heartbeat accepted: backup" in sent.stdout, (sent.stdout, sent.stderr)
        assert state("backup/heartbeat")["last_received_at"] != receipt
        receipt = state("backup/heartbeat")["last_received_at"]
        refused = send("wrong")
        assert refused.returncode == 1 and "the token does not match" in refused.stderr, (refused.returncode, refused.stdout, refused.stderr)
        assert state("backup/heartbeat")["last_received_at"] == receipt
        mark("a job of another account reports a heartbeat through the heartbeat socket with no certificate; a wrong token is refused")
        # While MMWatchdog is stopped, systemd has removed /run/mmwatchdog and
        # no other account can put a listener at the socket's path.
        vm.console("systemctl stop mmwatchdog")
        assert vm.console("test -e /run/mmwatchdog", check=False).returncode != 0
        assert vm.console(f"{job}mkdir /run/mmwatchdog", check=False).returncode != 0
        bind = f"import socket; s = socket.socket(socket.AF_UNIX); s.bind('{socket}')"
        assert vm.console(f"{job}python3 -c {shlex.quote(bind)}", check=False).returncode != 0
        assert vm.console("test -e /run/mmwatchdog", check=False).returncode != 0
        gone = send("good")
        assert gone.returncode == 4 and "there is no heartbeat socket" in gone.stderr, (gone.returncode, gone.stdout, gone.stderr)
        # A socket that another account owns would collect the token, so the
        # sender refuses it. The directory is made by root here only to
        # stand in for what the account cannot do.
        impostor = (
            "import os, socket\n"
            f"s = socket.socket(socket.AF_UNIX); s.bind('{socket}'); s.listen(1); s.settimeout(6)\n"
            "try:\n"
            "    open('/var/tmp/mmwatchdog-job/collected', 'w').write(s.accept()[0].recv(4096).decode())\n"
            "except OSError:\n"
            "    open('/var/tmp/mmwatchdog-job/collected', 'w').write('')\n"
        )
        vm.write("/var/tmp/mmwatchdog-job/impostor.py", impostor)
        vm.console(
            "install -d -m 0755 -o mmwatchdog-job /run/mmwatchdog; "
            f"({job}python3 /var/tmp/mmwatchdog-job/impostor.py >/dev/null 2>&1 &) ; sleep 1"
        )
        impostor_refused = send("good")
        assert impostor_refused.returncode == 4 and "is not safe for the token" in impostor_refused.stderr, (impostor_refused.returncode, impostor_refused.stdout, impostor_refused.stderr)
        time.sleep(6)
        assert vm.console("cat /var/tmp/mmwatchdog-job/collected").stdout == "", "the sender sent its token to a socket that root does not own"
        vm.console("rm -rf /run/mmwatchdog")
        vm.console("systemctl start mmwatchdog")
        wait(lambda: state("accepted/shell")["last_observation"]["status"] == "ok")
        receipt = state("backup/heartbeat")["last_received_at"]
        time.sleep(1.1)
        sent = send("good")
        assert sent.returncode == 0, (sent.stdout, sent.stderr)
        assert state("backup/heartbeat")["last_received_at"] != receipt
        vm.console("rm -rf /var/tmp/mmwatchdog-job; userdel mmwatchdog-job")
        mark("with MMWatchdog stopped another account can create neither /run/mmwatchdog nor a listener at the heartbeat socket's path, and the sender fails plainly or refuses a socket that root does not own; after the restart the heartbeat works again")
        cli("check-now certificate --json", 0)
        cli("check-now expiring --json", 1)
        mark(
            "heartbeat rejects an incorrect token; TLS validates certificates against its certificate authority file and approaching expiry"
        )
        wait(lambda: state("private/http")["last_observation"]["status"] == "ok")
        vm.write("/etc/mmwatchdog/secrets/http-token", "rotated-http-token-value\n", "0600")
        assert "received 401" in json.loads(cli("check-now private --json", 1).stdout)['http']["message"]
        vm.console("rm /etc/mmwatchdog/secrets/http-token")
        assert json.loads(cli("check-now private --json", 3).stdout)['http']["status"] == "unknown"
        wait(lambda: state("private/http")["state"] == "unknown")
        cli("status private --json", 3)
        cli("status --json", 1)  # The pipeline failure takes precedence over unknown.
        vm.write("/etc/mmwatchdog/secrets/http-token", "mmwatchdog-integration-http-token\n", "0600")
        cli("check-now private --json", 0)
        mark(
            "HTTPS check sends its virtual host and a token read for each request, verified against a private CA; an unreadable token is unknown"
        )
        cli("maintenance on queue_starlark --for 2s", 0)
        expiry = status()["maintenance"]["queue_starlark"]
        vm.console("systemctl restart mmwatchdog")
        wait(lambda: state("accepted/shell")["last_observation"]["status"] == "ok")
        wait(lambda: "queue_starlark" not in status()["maintenance"])
        wait(lambda: not state("queue_starlark/queued_jobs")["needs_fresh_observation"])
        assert any(
            e["type"] == "maintenance_off" and e.get("check_group") == "queue_starlark"
            for e in events()
        )
        mark("maintenance expiry survives daemon restart and resumes fresh observations")
        # One command puts several check groups in maintenance, for longer
        # than a week: each gets a window with the same end, and the command
        # sends one alert for the start and one for the end.
        several = cli("maintenance on queue_starlark queue_go --for 30d --reason 'queue migration'", 0).stdout.splitlines()
        assert several[0].startswith("queue_starlark, queue_go are in maintenance until ") and several[0].endswith(" UTC (30d)."), several
        assert several[1:] == [
            "  MMWatchdog keeps running the checks of the check groups in maintenance and shows the results in mmwatchdog status.",
            "  During maintenance, MMWatchdog sends no alert and attempts no systemd unit restart or recovery command for the check groups in maintenance.",
            "  To end the maintenance early: sudo mmwatchdog maintenance off queue_starlark queue_go",
        ], several
        windows = status()["maintenance"]
        assert windows["queue_starlark"] == windows["queue_go"] and windows["queue_starlark"]["reason"] == "queue migration", windows
        # The whole node stands alone, and names are separated by spaces.
        cli("maintenance on queue_starlark --every-check-group --for 1h", 4)
        cli("maintenance on queue_starlark,queue_go --for 1h", 4)
        ended = cli("maintenance off queue_starlark queue_go", 0).stdout
        assert ended == "queue_starlark, queue_go are out of maintenance. Alerts, restarts and recovery commands resume after fresh checks.\n", ended
        assert not {"queue_starlark", "queue_go"} & set(status()["maintenance"]), status()["maintenance"]

        def queue_maintenance_alerts():
            es = [x["event"] for x in reports() if x["accepted"] and x["event"].get("check_groups") == ["queue_starlark", "queue_go"]]
            return es if len(es) == 2 else None

        together = wait(queue_maintenance_alerts)
        assert [e["type"] for e in together] == ["maintenance_on", "maintenance_off"], together
        assert all("check_group" not in e and e["maintenance"]["reason"] == "queue migration" for e in together), together
        # The one event belongs to the history of each check group it names.
        for name in ["queue_starlark", "queue_go"]:
            history = json.loads(cli("events " + name + " --json", 0).stdout)
            assert [e["type"] for e in history if e.get("check_groups") == ["queue_starlark", "queue_go"]] == ["maintenance_off", "maintenance_on"], history
        mark("one maintenance command covers several check groups for 30 days, with one alert for its start and one for its end, and refuses the whole node together with names")
        # A check that names its destinations sends every alert to exactly
        # those, whatever their alert_severities say. The destination
        # only_named takes no severity, so only this check reaches it, and
        # capture, which takes every severity, gets nothing of the check.
        named_fragment = "/etc/mmwatchdog/check_groups/named.yml"
        vm.write("/etc/mmwatchdog/alert_destinations.yml", json.dumps({"destinations": dict(alerts["destinations"], only_named={'remind_every': 'never',
            "webhook": {"url": "http://127.0.0.1:18080/only-named"}, "time_limit_per_delivery_attempt": "1s", "alert_severities": []})}, indent=2))

        def named_check(destinations):
            return json.dumps({"check_groups": {"named_destination": {
                "check_every": "300ms", "time_limit_per_run": "200ms", "alert_after_failing_for": "0s", "recovered_after_healthy_for": "0s",
                "shell": "test ! -e /var/tmp/mmwatchdog-fixture/named-fail", "alert_severity": "warn", "send_alerts_only_to_destinations": destinations}}})

        vm.write(named_fragment, named_check(["only_named"]))
        reloaded = cli("reload", 0).stdout
        assert 'Destination only_named:' in reloaded, reloaded
        assert 'sudo mmwatchdog send-notice --alert-severity info --title "Test of the destination only_named. Nothing is wrong." --destination only_named' in reloaded, reloaded
        assert not any(r['event'].get('type') == 'notice' and 'Test of the destination only_named.' in r['event'].get('title', '') for r in reports()), reports()
        wait(lambda: state("named_destination/shell")["state"] == "ok")
        assert "  Sends alerts only to: only_named\n" in cli("status named_destination", 0).stdout

        def named_alerts():
            return [(r["path"], r["event"]["type"]) for r in reports() if r["accepted"] and r["event"].get("check_group") == "named_destination"]

        vm.console("touch /var/tmp/mmwatchdog-fixture/named-fail")
        wait(lambda: named_alerts() == [("/only-named", "failed")])
        vm.console("rm /var/tmp/mmwatchdog-fixture/named-fail")
        wait(lambda: named_alerts() == [("/only-named", "failed"), ("/only-named", "recovered")])
        failed_by_name = [r["event"] for r in reports() if r["path"] == "/only-named"][0]
        assert failed_by_name["send_alerts_only_to_destinations"] == ["only_named"] and failed_by_name["alert_severity"] == "warn", failed_by_name
        # A name that alert_destinations.yml does not have is refused, so a
        # mistyped name never loses an alert.
        vm.write(named_fragment, named_check(["only_namd"]))
        rejected = cli("reload", 4)
        assert 'send_alerts_only_to_destinations: alert_destinations.yml has no destination named "only_namd"' in rejected.stderr, rejected.stderr
        # A check that names nobody is silent: its failure is recorded and sent nowhere.
        vm.write(named_fragment, named_check([]))
        cli("reload", 0)
        assert "  Sends alerts to no destination (send_alerts_only_to_destinations: [])\n" in cli("status named_destination", 0).stdout
        vm.console("touch /var/tmp/mmwatchdog-fixture/named-fail")
        wait(lambda: state("named_destination/shell")["failure_incident"].get("incident_id"))
        time.sleep(1)
        assert len(named_alerts()) == 2, named_alerts()
        assert [e for e in events() if e.get("check_group") == "named_destination" and e["type"] == "failed"]
        vm.console("rm -f /var/tmp/mmwatchdog-fixture/named-fail " + named_fragment)
        vm.write("/etc/mmwatchdog/alert_destinations.yml", json.dumps(alerts, indent=2))
        cli("reload", 0)
        mark("a check that names its destinations reaches exactly those, also one that takes no severity; a name that is not configured is refused; a check that names nobody sends no alert")
        # One cause, one alert: a check that names the check of its cause
        # is kept quiet while that check fails.
        from one_cause_scenarios import suite as one_cause_suite
        one_cause_suite(cli, status, state, events, reports, wait, mark)
        # MMWatchdog runs with no destination only when the file says so.
        vm.console("rm -rf /var/tmp/mmwatchdog-fixture/nobody; cp -a /etc/mmwatchdog /var/tmp/mmwatchdog-fixture/nobody; printf 'destinations: {}\\n' > /var/tmp/mmwatchdog-fixture/nobody/alert_destinations.yml")
        nobody = cli("validate --config-dir /var/tmp/mmwatchdog-fixture/nobody", 0).stdout
        assert "0 alert destinations\nThis node sends alerts to nobody.\n" in nobody, nobody
        assert 'no destination receives alerts with alert_severity warn' in nobody, nobody
        routing_warnings = json.loads(cli("validate --config-dir /var/tmp/mmwatchdog-fixture/nobody --json", 0).stdout)
        assert routing_warnings['checks_without_alert_destination'], routing_warnings
        vm.console("rm /var/tmp/mmwatchdog-fixture/nobody/alert_destinations.yml")
        missing = cli("validate --config-dir /var/tmp/mmwatchdog-fixture/nobody", 1).stderr
        assert 'alert_destinations.yml: alert destinations file is missing. Create the file with a destination, or write "destinations: {}" into the file to run MMWatchdog with no destination' in missing, missing
        mark("MMWatchdog runs with no destination only when alert_destinations.yml says destinations: {}; a missing file is an error that shows both choices")
        # mmwatchdog send-notice sends a one-time message the way a real alert goes,
        # and prints the result of the first delivery attempt to each
        # destination. Here capture takes warn and sos, only_named and
        # unreachable take no severity, and nothing listens at unreachable.
        vm.write("/etc/mmwatchdog/alert_destinations.yml", json.dumps({"destinations": {
            "capture": dict(alerts["destinations"]["capture"], alert_severities=["warn", "sos"]),
            "only_named": {'remind_every': 'never', "webhook": {"url": "http://127.0.0.1:18080/only-named"}, "time_limit_per_delivery_attempt": "1s", "alert_severities": []},
            "unreachable": {'remind_every': 'never', "webhook": {"url": "http://127.0.0.1:1/unreachable"}, "time_limit_per_delivery_attempt": "1s", "alert_severities": []},
        }}, indent=2))
        reloaded = cli("reload", 1).stdout
        assert 'Destination unreachable: failed:' in reloaded and 'The configuration is active.' in reloaded, reloaded
        node = vm.hostname()

        def send_notice(severity, title, *more, code=0):
            return cli("send-notice --alert-severity " + severity + " --title " + shlex.quote(title) + "".join(" " + shlex.quote(word) for word in more), code)

        def notices(title):
            return [(r["path"], r["event"]) for r in reports() if r["event"]["type"] == "notice" and r["event"].get("title") == node + ": " + title]

        test_title = "Test of the alert destinations. Nothing is wrong."
        sent = send_notice("warn", test_title, "--body", "Sent by the acceptance suite.")
        assert sent.stdout == "Notice sent to every destination that takes warn:\n  capture   delivered\n", sent.stdout
        [(path, notice)] = notices(test_title)
        assert path == "/events" and notice["alert_severity"] == "warn" and notice["body"] == "Sent by the acceptance suite." and notice["message"] == test_title, notice
        assert notice["node"] == node and not notice.get("check_group") and not notice.get("incident_id"), notice
        # One destination by name, though it takes no severity.
        sent = send_notice("info", "Only for one destination", "--destination", "only_named")
        assert sent.stdout == "Notice sent to the destination only_named:\n  only_named   delivered\n", sent.stdout
        assert [path for path, _ in notices("Only for one destination")] == ["/only-named"], notices("Only for one destination")
        # No destination takes info: the notice is recorded and sent to nobody.
        sent = send_notice("info", "For nobody")
        assert sent.stdout == "No destination takes info, so MMWatchdog sent the notice to nobody. The notice is recorded in mmwatchdog events.\n", sent.stdout
        assert not notices("For nobody") and [e for e in events() if e["type"] == "notice" and e.get("title") == node + ": For nobody"]
        # MMWatchdog never cuts a title that a person typed.
        too_long = "t" * (121 - len(node + ": "))
        refused = send_notice("warn", too_long, code=4)
        assert "the title would have 121 characters with the node's name in front" in refused.stderr and "shorten --title by 1 character" in refused.stderr, refused.stderr
        assert not [e for e in events() if e["type"] == "notice" and e.get("message") == too_long]
        refused = send_notice("warn", test_title, "--destination", "pager", code=4)
        assert 'alert_destinations.yml has no destination named "pager"; its destinations are: capture, only_named, unreachable' in refused.stderr, refused.stderr
        # A failed first attempt is printed with its error and exits 1; the
        # notice stays queued. The next notice for that destination waits
        # behind it, and the command says so at once.
        failed = send_notice("info", "To a destination that is down", "--destination", "unreachable", code=1)
        assert failed.stdout.startswith("Notice sent to the destination unreachable:\n  unreachable   failed: "), failed.stdout
        assert "the notice was not delivered at the first attempt to 1 of 1 destinations. MMWatchdog keeps the notice queued and tries again" in failed.stderr, failed.stderr
        behind = send_notice("info", "Behind the first one", "--destination", "unreachable", code=1)
        assert "  unreachable   not attempted yet: 1 earlier alert is not delivered, and the latest attempt failed: " in behind.stdout, behind.stdout
        assert status()["delivery"]["queued_notifications"] >= 2
        vm.write("/etc/mmwatchdog/alert_destinations.yml", json.dumps(alerts, indent=2))
        cli("reload", 0)
        mark("mmwatchdog send-notice delivers a notice with its title and body, to one named destination, or to nobody; refuses a title that is too long; and reports a failed first attempt with exit code 1")
        # A step of the system clock holds back no check run, no alert, and
        # no retry of a delivery: MMWatchdog times its waits on the monotonic clock.
        # The node's clock goes back by 30 seconds. MMWatchdog alerts and tries a
        # delivery again within 2 seconds, and each wait below allows 20: a
        # wait that counted on the system clock would last 30 seconds longer
        # and fail it. The suite's certificate is valid from the moment that
        # openssl made it, at the start of the suite, about a minute before
        # this scenario. The step comes once the certificate is older than
        # the step, so that it is valid at the earlier time too.
        clock_step = 30
        within_the_normal_time = 20
        certificate_age = 'echo $(( $(date +%s) - $(date -d "$(openssl x509 -noout -startdate -in /etc/mmwatchdog/secrets/cert.pem | cut -d= -f2)" +%s) ))'
        wait(lambda: int(vm.console(certificate_age).stdout) > clock_step + 5, clock_step + 10)

        def accepted(predicate):
            return [r["event"] for r in reports() if r["accepted"] and predicate(r["event"])]

        # A notice whose first attempt fails before the step is tried again
        # after it, a moment later.
        vm.console("touch /var/tmp/mmwatchdog-fixture/reject")
        queued_title = "Queued before the clock stepped back"
        send_notice("warn", queued_title, code=1)
        # The clock goes back to this machine's time also when a step
        # fails, so no later suite on the guest sees it behind.
        vm.set_clock(seconds_ahead_of_this_machine=-clock_step)
        try:
            vm.console("rm /var/tmp/mmwatchdog-fixture/reject")
            wait(lambda: accepted(lambda e: e["type"] == "notice" and e.get("title") == node + ": " + queued_title), within_the_normal_time)
            # A check whose next run was set before the step runs on time,
            # and its incident opens and alerts after alert_after_failing_for.
            vm.write("/var/tmp/mmwatchdog-fixture/pending", "780\n")
            incident = wait(lambda: state("queue_starlark/queued_jobs")["failure_incident"].get("incident_id"), within_the_normal_time)
            wait(lambda: accepted(lambda e: e["type"] == "failed" and e.get("incident_id") == incident), within_the_normal_time)
            vm.write("/var/tmp/mmwatchdog-fixture/pending", "0\n")
            wait(lambda: not state("queue_starlark/queued_jobs")["failure_incident"].get("incident_id") and not state("queue_go/queued_jobs_in_go")["failure_incident"].get("incident_id"))
        finally:
            vm.set_clock()
        mark("a step of the node's clock back by 30 seconds holds back no check run, no alert, and no retry of a delivery that failed before the step")
        # Email destinations, against Mailpit on loopback in the guest.
        from email_scenarios import suite as email_suite
        email_suite(cli, state, events, wait, mark, alerts)
        # Destination modules, with the example module of a team's own.
        from destination_module_scenarios import suite as destination_module_suite
        destination_module_suite(cli, status, state, events, reports, wait, mark, alerts)
        # The official extra destination modules, but mattermost, against
        # stand-ins for their services in the guest.
        from official_extra_destination_module_scenarios import suite as official_extra_destination_module_suite
        official_extra_destination_module_suite(cli, status, state, events, wait, mark, alerts)
        # Changes retire a check rather than fabricating health evidence.
        config["check_groups"]["pipeline"]["shell"] = "true"
        vm.write_configuration(config)
        cli("reload", 0)
        wait(lambda: state("pipeline/shell")["last_observation"]["status"] == "ok")
        assert [e for e in events() if e.get("check_group") == "pipeline" and e["type"] == "closed_without_recovery"]
        assert not [
            e for e in events() if e.get("check_group") == "pipeline" and e["type"] == "recovered"
        ]
        wait(lambda: any(r["accepted"] and r["event"].get("check_group") == "pipeline" and r["event"]["type"] == "closed_without_recovery" and r["event"].get("incident_id") == old for r in reports()))
        mark("changed check retires its old incident locally and through the webhook without claiming recovery")
        disk = state("disk/node_filesystem")["last_observation"]
        assert disk["status"] == "ok" and disk["message"].startswith("space_used_percent is ") and disk["message"].endswith(" (fails above 99)"), disk
        assert set(disk["measurements"]) == {"space_used_percent", "space_free_gib", "inodes_used_percent"}, disk
        memory = state("memory/node_resource")["last_observation"]
        assert memory["status"] == "ok" and memory["message"].startswith("memory_used_percent is ") and memory["message"].endswith(" (fails above 99)"), memory
        assert state("tcp/tcp")["last_observation"]["status"] == "ok"
        # MMX process has exited; all observations and recoveries above were standalone.
        mark("Linux host and TCP checks run with MMX absent at runtime")
    if group in {None, "diagnostics"}:
        from diagnostics import suite as diagnostics_suite
        diagnostics_suite(cli, status, state, events, reports, wait, mark, config)
    if group in {None, "resources"}:
        from resources import suite as resources_suite
        from lifecycle import suite as lifecycle_suite
        resources_suite(cli, status, state, events, wait, mark)
        lifecycle_suite(mark)
    if group in {None, "node"}:
        from node_scenarios import suite as node_suite
        node_suite(cli, status, state, events, reports, wait, mark)
    if group in {None, "services"}:
        from service_scenarios import suite as service_suite
        service_suite(cli, status, state, events, reports, wait, mark, setup_step_between_scenarios_done)
    if group in {None, "fleet"}:
        if "peer_node" not in vm.REMOTE:
            print("Skipping the fleet workflow, which needs a second guest: cloud.py up --with-peer creates it", flush=True)
        else:
            from fleet import suite as fleet_suite
            fleet_suite(mark)
    if group is None:
        native_tests()
        mark("Go test suite passes natively on " + vm.DISTRIBUTION + " " + vm.ARCH)
    report = {
        "passed": passed,
        "mmx": provenance,
        "mmwatchdog_sha256": hashlib.sha256(
            (root / ("bin/mmwatchdog-linux-" + vm.ARCH)).read_bytes()
        ).hexdigest(),
        "vm": vm.REMOTE["name"],
        "platform": vm.console("cat /etc/os-release; uname -m").stdout,
        "time": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }
    summary = root / ("summary-" + ((group + "-") if group else "") + vm.DISTRIBUTION + "-" + vm.ARCH + ".json")
    summary.write_text(json.dumps(report, indent=2) + "\n")
    print(f"{len(passed)} acceptance scenarios passed; report: {summary}", flush=True)
