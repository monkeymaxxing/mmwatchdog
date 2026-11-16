package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	modulepages "mmwatchdog/docs/reference/modules"
	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/daemon"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
)

// TestMain gives every test the same short host name: a title names the
// node, and a long host name, as on some CI runners, shortens titles.
func TestMain(m *testing.M) {
	hostName = func() (string, error) { return "app01", nil }
	os.Exit(m.Run())
}

func TestReadableDiagnosticIncludesOutput(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	os.Mkdir(filepath.Join(directory, "check_groups"), 0700)
	os.WriteFile(filepath.Join(directory, "mmwatchdog_node_settings.yml"), []byte(nodeSettingsOfATest(directory)), 0600)
	os.WriteFile(filepath.Join(directory, "alert_destinations.yml"), []byte("destinations: {}\n"), 0600)
	path := filepath.Join(directory, "check_groups", "checks.yml")
	config := map[string]any{"check_groups": map[string]any{"app": map[string]any{"check_every": "30s", "time_limit_per_run": "5s", "alert_after_failing_for": "1m", "recovered_after_healthy_for": "30s", "alert_severity": "warn", "shell": map[string]any{
		"script": "printf 'checking database'; printf 'connection refused' >&2; exit 7", "run_as_user_account": u.Username,
	}}}}
	b, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	original := os.Stdout
	os.Stdout = writer
	defer func() { os.Stdout = original }()
	err = run([]string{"check-now", "app", "--without-daemon", "--config-dir", directory})
	writer.Close()
	var code exitStatus
	if !errors.As(err, &code) || code != 1 {
		t.Fatalf("failed diagnostic exit: %v", err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"checking database", "connection refused", "Exit code: 7"} {
		if !strings.Contains(string(output), want) {
			t.Errorf("missing %q in readable diagnosis: %s", want, output)
		}
	}
}

// Alert settings live in alert_destinations.yml beside the main file, and a staged copy
// of either file can be validated on its own.
func TestValidateRuntimeRequiresLocalCredentials(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "check_groups"), 0700)
	os.WriteFile(filepath.Join(dir, "mmwatchdog_node_settings.yml"), []byte(nodeSettingsOfATest(dir)), 0600)
	path, alerts, token := filepath.Join(dir, "check_groups", "checks.yml"), filepath.Join(dir, "alert_destinations.yml"), filepath.Join(dir, "token")
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for file, content := range map[string]any{
		path:   map[string]any{"check_groups": map[string]any{"app": map[string]any{"check_every": "30s", "time_limit_per_run": "5s", "alert_after_failing_for": "1m", "recovered_after_healthy_for": "30s", "alert_severity": "warn", "shell": map[string]any{"script": "true", "run_as_user_account": u.Username}}}},
		alerts: map[string]any{"destinations": map[string]any{"ops": map[string]any{"webhook": map[string]any{"url": "https://example.com/events", "bearer_token_file": token}, "remind_every": "4h", "alert_severities": []string{"info", "warn", "sos"}}}},
	} {
		b, err := json.Marshal(content)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"--config-dir", dir}} {
		if err := run(append([]string{"validate", "--config-only"}, args...)); err != nil {
			t.Fatal("authoring validation requires target credentials", args, err)
		}
		if err := run(append([]string{"validate"}, args...)); err == nil || !strings.Contains(err.Error(), "alert_destinations.yml") || !strings.Contains(err.Error(), "destinations.ops.webhook.bearer_token_file") {
			t.Fatal("runtime validation did not reject the missing token", args, err)
		}
	}
	if err := run([]string{"validate", "--alerts", alerts, "--config", path}); err == nil {
		t.Fatal("validated an alert settings file together with an explicit configuration")
	}
	if err := os.WriteFile(token, []byte("test-webhook-token-value"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--config-dir", dir}} {
		if err := run(append([]string{"validate"}, args...)); err != nil {
			t.Fatal("runtime validation rejected valid credentials", args, err)
		}
	}
}

// requiredNodeSettings are the settings that every node states.
const requiredNodeSettings = "alert_severity_when_this_node_goes_silent: sos\nalert_severity_when_this_node_has_rebooted: info\nkeep_measurement_history_for: 7d\nmeasurement_history_size_limit: 256MiB\nkeep_undelivered_alerts_for: 24h\nundelivered_alerts_content_size_limit: 8MiB\n"

// nodeSettingsOfATest are the node settings of a test whose configuration
// directory is configDir: the settings that every node states, and the
// node's three directories for modules below configDir, so that no test
// reads the modules of the machine that it runs on.
func nodeSettingsOfATest(configDir string) string {
	return requiredNodeSettings + "official_extra_module_dir: " + filepath.Join(configDir, "official_extra_modules") + "\nexternal_module_dir: " + filepath.Join(configDir, "external_modules") + "\nmodule_state_dir: " + filepath.Join(configDir, "module_state") + "\n"
}

// timingsOfACheckGroup are the four timings that every check group states,
// for a check group that is written on one line.
const timingsOfACheckGroup = "check_every: 30s, time_limit_per_run: 5s, alert_after_failing_for: 1m, recovered_after_healthy_for: 30s"

// writeFiles writes the files of a test by their paths below dir, each in
// the directory that its path names. A file named module_executable can be
// run.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0644)
		if filepath.Base(name) == "module_executable" {
			mode = 0755
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
}

// The descriptions and the code of the modules that the tests of the module
// commands install.
const (
	queuedJobsDescription = `summary: Counts the jobs that wait in a queue file.
used_as: check
inputs:
  queue_file_path:
    type: string
    required: true
    description: The absolute path of the file that holds the number of waiting jobs.
  queue_type:
    type: string
    required: false
    allowed_values: [mail, print]
    description: The kind of queue.
measurements:
  queued_jobs:
    unit: count
    description: The number of jobs that wait.
examples:
  - description: Fail when more than 500 jobs wait.
    config: |
      queued_jobs:
        queue_file_path: /var/lib/myapp/pending
      fail_when_measurement_above:
        queued_jobs: 500
      alert_severity: warn
`
	queuedJobsScript = `def run_check(inputs, file_contents):
    queue_file = read_file(inputs["queue_file_path"])
    if queue_file.error:
        return {"status": "unknown", "message": "MMWatchdog cannot read the queue file: " + queue_file.error}
    waiting = queue_file.content.strip()
    return {"status": "ok", "message": waiting + " queued jobs", "measurements": {"queued_jobs": int(waiting)}}
`
	legacyBackupDescription = `summary: Finds the backup sets that are too old.
used_as: check
one_incident_for_each_finding: true
examples:
  - description: Alert for each backup set that is too old.
    config: |
      legacy_backup: {}
      alert_severity: warn
`
	slackDescription = `summary: Delivers each alert to a Slack channel.
used_as: alert_destination
inputs:
  webhook_url_file:
    type: string
    required: true
    description: The file that holds the address of the webhook.
examples:
  - description: Send every alert to the team's channel.
    config: |
      slack:
        webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team
      alert_severities: [info, warn, sos]
`
	slackScript = "def deliver_alert(alert, inputs, file_contents):\n    return {\"status\": \"delivered\", \"message\": \"Slack accepted the message\"}\n"
)

// nodeWithModules writes the configuration directory of a node and its two
// module directories: the official extra module slack, and two modules of
// the project's own, the script queued_jobs and the executable
// legacy_backup. The node settings name the two directories. checks and
// destinations are the content of the two other configuration files.
func nodeWithModules(t *testing.T, checks, destinations string) (configDir, officialExtraModuleDir, externalModuleDir string) {
	t.Helper()
	configDir, officialExtraModuleDir, externalModuleDir = t.TempDir(), t.TempDir(), t.TempDir()
	writeFiles(t, officialExtraModuleDir, map[string]string{"shipped_with_mmwatchdog_version": version + "\n", "README.md": "The official extra modules of MMWatchdog.\n",
		"slack/module_description.yml": slackDescription, "slack/module.star": slackScript})
	writeFiles(t, externalModuleDir, map[string]string{"queued_jobs/module_description.yml": queuedJobsDescription, "queued_jobs/module.star": queuedJobsScript,
		"legacy_backup/module_description.yml": legacyBackupDescription, "legacy_backup/module_executable": "#!/bin/bash\n"})
	writeFiles(t, configDir, map[string]string{
		"mmwatchdog_node_settings.yml": requiredNodeSettings + "official_extra_module_dir: " + officialExtraModuleDir + "\nexternal_module_dir: " + externalModuleDir + "\nmodule_state_dir: " + filepath.Join(configDir, "module_state") + "\n",
		"check_groups/myapp.yml":       checks,
		"alert_destinations.yml":       destinations,
	})
	return configDir, officialExtraModuleDir, externalModuleDir
}

// A deploy stages the new module directories and validates the new
// configuration against them, before anything live changes: mmwatchdog validate
// --external-module-dir DIR and --official-extra-module-dir DIR look for
// modules in DIR, and not in the directories of the node settings, with and
// without node prerequisite checks.
func TestValidateAgainstOtherModuleDirectories(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	dir, _, installed := nodeWithModules(t, "", "destinations: {}\n")
	writeFiles(t, dir, map[string]string{"check_groups/myapp.yml": "check_groups: {queue: {" + timingsOfACheckGroup + ", alert_severity: warn, queue_length: {run_as_user_account: " + u.Username + ", run_from_directory: " + dir + "}}}\n"})
	candidate, release := t.TempDir(), t.TempDir()
	writeFiles(t, candidate, map[string]string{"queue_length/module_description.yml": strings.ReplaceAll(legacyBackupDescription, "legacy_backup", "queue_length"), "queue_length/module_executable": "#!/bin/bash\n"})
	err = run([]string{"validate", "--config-only", "--config-dir", dir})
	if errorCode(err) != 1 || !strings.Contains(err.Error(), "myapp.yml:1:") || !strings.Contains(err.Error(), `: check_groups.queue.queue_length: unknown field "queue_length": the field name matches neither a setting of a check nor an installed module.`) || !strings.Contains(err.Error(), " or in "+installed+" (legacy_backup, queued_jobs)") {
		t.Fatal("validation did not look in the node's own directory:", err)
	}
	for _, runtime := range [][]string{{}, {"--config-only"}} {
		if err := run(append([]string{"validate", "--config-dir", dir, "--external-module-dir", candidate}, runtime...)); err != nil {
			t.Fatal("validation did not find the module in the given directory:", runtime, err)
		}
	}
	// A path can be relative to the directory of the command.
	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(here, candidate)
	if err != nil || filepath.IsAbs(relative) {
		t.Fatal(relative, err)
	}
	output, err := capture(t, func() error {
		return run([]string{"validate", "--config-dir", dir, "--external-module-dir", relative, "--json"})
	})
	if err != nil {
		t.Fatal("validation did not find the module in the directory of a relative path:", err)
	}
	// The configuration that the command prints keeps the node's setting.
	var valid struct {
		Config struct {
			ExternalModuleDir string `json:"external_module_dir"`
		} `json:"config"`
	}
	if err := json.Unmarshal([]byte(output), &valid); err != nil || valid.Config.ExternalModuleDir != installed {
		t.Fatal(output, err)
	}
	// Validation on the node finds a module whose executable cannot be run.
	if err := os.Chmod(filepath.Join(candidate, "queue_length", "module_executable"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"validate", "--config-only", "--config-dir", dir, "--external-module-dir", candidate}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"validate", "--config-dir", dir, "--external-module-dir", candidate}); errorCode(err) != 1 || !strings.Contains(err.Error(), "check_groups.queue.queue_length: "+filepath.Join(candidate, "queue_length", "module_executable")+" is not an executable file") {
		t.Fatal(err)
	}
	// The official extra modules of another release than this mmwatchdog program.
	writeFiles(t, release, map[string]string{"shipped_with_mmwatchdog_version": "0.9.0\n", "slack/module_description.yml": slackDescription, "slack/module.star": slackScript})
	err = run([]string{"validate", "--config-only", "--config-dir", dir, "--external-module-dir", candidate, "--official-extra-module-dir", release})
	if errorCode(err) != 1 || err.Error() != "the official extra modules in "+release+" were shipped with MMWatchdog 0.9.0, and this mmwatchdog program is MMWatchdog "+version+". One MMWatchdog release holds both: deploy that release again" {
		t.Fatal(err)
	}
	for _, option := range []string{"--external-module-dir", "--official-extra-module-dir"} {
		if err := run([]string{"validate", "--config-only", "--config-dir", dir, option, ""}); errorCode(err) != 4 || err.Error() != option+" requires a directory" {
			t.Fatal(err)
		}
	}
}

// mmwatchdog describe-module NAME --reference-only prints what a module is and what
// a check or a destination can state for it, in the same form for every
// kind of module. Without --reference-only it prints the module's whole
// page: for a core module the page built into mmwatchdog, for an official extra
// module its README.md, and for a module of the project's own its
// reference, then its README.md. Without a name it lists every module that
// the node has, the core modules by topic. --json gives the same to a
// program, with the page in the field page.
func TestDescribeModuleCommand(t *testing.T) {
	dir, official, external := nodeWithModules(t, "check_groups: {app: {"+timingsOfACheckGroup+", shell: 'true', alert_severity: warn}}\n", "destinations: {}\n")
	describe := func(arguments ...string) string {
		t.Helper()
		output, err := capture(t, func() error {
			return run(append(append([]string{"describe-module"}, arguments...), "--config-dir", dir))
		})
		if err != nil {
			t.Fatal(arguments, err)
		}
		return output
	}
	want := "queued_jobs: a module of the project's own (Starlark), used as a check\n" +
		"Counts the jobs that wait in a queue file.\n" +
		"Files: " + external + "/queued_jobs/\n" +
		"\nInputs\n" +
		"  queue_file_path   string         required   The absolute path of the file that holds the number of waiting jobs.\n" +
		"  queue_type        mail | print   optional   The kind of queue.\n" +
		"\nMeasurements\n" +
		"  queued_jobs   count   The number of jobs that wait.\n" +
		"\nExample: Fail when more than 500 jobs wait.\n" +
		"  queued_jobs:\n    queue_file_path: /var/lib/myapp/pending\n  fail_when_measurement_above:\n    queued_jobs: 500\n  alert_severity: warn\n"
	if got := describe("queued_jobs"); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if got := describe("queued_jobs", "--reference-only"); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	for name, lines := range map[string][]string{
		"legacy_backup": {"legacy_backup: a module of the project's own (executable), used as a check\nFinds the backup sets that are too old.\nFiles: " + external + "/legacy_backup/\n" +
			"MMWatchdog does not see what an executable does on this node. The local node must provide everything the executable needs.\n" +
			"One run of the check can report several findings, and each finding is its own incident.\n\nExample: Alert for each backup set that is too old.\n  legacy_backup: {}\n  alert_severity: warn\n"},
		"slack":      {"slack: an official extra module (Starlark, shipped with MMWatchdog " + version + "), used as an alert destination\nDelivers each alert to a Slack channel.\nFiles: " + official + "/slack/\n\nInputs\n  webhook_url_file   string   required   The file that holds the address of the webhook.\n"},
		"http":       {"http: a core module (built into MMWatchdog), used as a check\nSends one HTTP or HTTPS request", "\nInputs\n  url ", "  method                                      GET | HEAD           optional   The request method, GET by default. HEAD cannot check response body content.", "\nExample: An application answers its health request with 200 and the word ready.\n  http:\n    url: http://127.0.0.1:8080/health\n"},
		"ssh":        {"ssh: a core module (built into MMWatchdog), used as a check\nAsks an SSH server for its greeting, the line with which every SSH server starts. The check logs in nowhere.\n\nValue\n  (the value of ssh)   string   required   The address of the SSH server, as host:port.\n\nExample: The SSH server of this machine answers.\n  ssh: 127.0.0.1:22\n  alert_severity: sos\n"},
		"tcp":        {"tcp: a core module (built into MMWatchdog), used as a check\nOpens a TCP connection.", "\nValue\n  (the value of tcp)", "The address alone, as host:port, in place of the mapping of inputs.", "\nInputs\n  address ", "\nExample: The database port of this machine accepts a connection.\n  tcp: 127.0.0.1:5432\n  alert_severity: sos\n"},
		"node_clock": {"\nInputs\n  fail_when   the_clock_is_not_synchronized   required   The check fails while the clock is not synchronized; write the_clock_is_not_synchronized, the only value.\n"},
		"email":      {"email: a core module (built into MMWatchdog), used as an alert destination\n", "  smtp_connection_security                    starttls | implicit_tls | unencrypted   required   How the connection to the mail server is protected."},
	} {
		got := describe(name, "--reference-only")
		for _, line := range lines {
			if !strings.Contains(got, line) {
				t.Errorf("mmwatchdog describe-module %s lacks %q:\n%s", name, line, got)
			}
		}
		if core := name != "legacy_backup" && name != "slack"; core == strings.Contains(got, "Files: ") {
			t.Errorf("mmwatchdog describe-module %s:\n%s", name, got)
		}
	}
	var one struct {
		Name                         string `json:"name"`
		Kind                         string `json:"kind"`
		Directory                    string `json:"directory"`
		ShippedWithMMWatchdogVersion string `json:"shipped_with_mmwatchdog_version"`
		Description                  struct {
			Summary string `json:"summary"`
			UsedAs  string `json:"used_as"`
			Inputs  []struct {
				Name          string `json:"name"`
				Type          string `json:"type"`
				Required      bool   `json:"required"`
				AllowedValues []any  `json:"allowed_values"`
			} `json:"inputs"`
			Measurements []struct{ Name, Unit string } `json:"measurements"`
			Examples     []struct {
				Description   string `json:"description"`
				Configuration string `json:"config"`
			} `json:"examples"`
		} `json:"description"`
	}
	if err := json.Unmarshal([]byte(describe("queued_jobs", "--json")), &one); err != nil || one.Name != "queued_jobs" || one.Kind != "project_starlark" || one.Directory != filepath.Join(external, "queued_jobs") || one.ShippedWithMMWatchdogVersion != "" ||
		one.Description.UsedAs != "check" || len(one.Description.Inputs) != 2 || one.Description.Inputs[0].Name != "queue_file_path" || !one.Description.Inputs[0].Required || one.Description.Inputs[1].Type != "string" || len(one.Description.Inputs[1].AllowedValues) != 2 ||
		len(one.Description.Measurements) != 1 || one.Description.Measurements[0].Unit != "count" || len(one.Description.Examples) != 1 || !strings.HasPrefix(one.Description.Examples[0].Configuration, "queued_jobs:\n") {
		t.Fatalf("%+v %v", one, err)
	}
	if err := json.Unmarshal([]byte(describe("slack", "--json")), &one); err != nil || one.Kind != "official_extra" || one.ShippedWithMMWatchdogVersion != version || one.Description.UsedAs != "alert_destination" {
		t.Fatalf("%+v %v", one, err)
	}

	// Without a name: every module of the node, by kind. A module that MMWatchdog
	// cannot use is in the list with what is wrong with it.
	writeFiles(t, external, map[string]string{"unfinished/module_description.yml": legacyBackupDescription})
	list := describe()
	// The column of the names is as wide as the longest name of its kind.
	for _, line := range []string{`\n  every_systemd_unit +a check +Watches every systemd unit of the node at once\.`, `\n  webhook +an alert destination +Posts each alert event as JSON to a web address\.\n`} {
		if !regexp.MustCompile(line).MatchString(list) {
			t.Errorf("mmwatchdog describe-module lacks a line that matches %s:\n%s", line, list)
		}
	}
	for _, line := range []string{
		"Core modules (built into MMWatchdog), by topic\n\nServices and protocols\n  http ",
		"\n\nAlert destinations\n  webhook ",
		"\n\nOfficial extra modules (Starlark, shipped with MMWatchdog " + version + ")\n  slack   an alert destination   Delivers each alert to a Slack channel.\n",
		"\n\nThe project's own modules\n  legacy_backup   a check   Finds the backup sets that are too old.\n  queued_jobs     a check   Counts the jobs that wait in a queue file.\n  unfinished      MMWatchdog cannot use this module: the module unfinished has no code: " + filepath.Join(external, "unfinished") + " must hold the script module.star or the executable module_executable\n",
		"\n\nmmwatchdog describe-module NAME prints the whole page of one module: what it checks or where it delivers alerts, its inputs, measurements, and examples, how it works, its errors, and why it works this way. Add --reference-only for only the module's inputs, measurements, and examples.\n",
	} {
		if !strings.Contains(list, line) {
			t.Errorf("mmwatchdog describe-module lacks %q:\n%s", line, list)
		}
	}
	var all struct {
		Modules []struct {
			Name    string `json:"name"`
			Kind    string `json:"kind"`
			Topic   string `json:"topic"`
			Problem string `json:"problem"`
		} `json:"modules"`
	}
	if err := json.Unmarshal([]byte(describe("--json")), &all); err != nil || len(all.Modules) < 14 || all.Modules[0].Name != "http" || all.Modules[0].Kind != "core" || all.Modules[0].Topic != "Services and protocols" || all.Modules[len(all.Modules)-1].Name != "unfinished" || all.Modules[len(all.Modules)-1].Problem == "" {
		t.Fatalf("%+v %v", all, err)
	}
	// The two options take the place of the node's directories.
	other := t.TempDir()
	writeFiles(t, other, map[string]string{"file_age/module_description.yml": strings.ReplaceAll(legacyBackupDescription, "legacy_backup", "file_age"), "file_age/module.star": queuedJobsScript})
	if got := describe("file_age", "--external-module-dir", other); !strings.HasPrefix(got, "file_age: a module of the project's own (Starlark), used as a check\n") {
		t.Fatal(got)
	}
	// A name that no module has, and a module that MMWatchdog cannot use.
	for name, want := range map[string]string{
		"file_age":   "no module is named file_age: MMWatchdog has no core module of that name, and no directory of that name is in " + official + " (slack) or in " + external + " (legacy_backup, queued_jobs, unfinished)",
		"unfinished": "the module unfinished has no code: " + filepath.Join(external, "unfinished") + " must hold the script module.star or the executable module_executable",
	} {
		if err := run([]string{"describe-module", name, "--config-dir", dir}); errorCode(err) != 4 || err.Error() != want {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// mmwatchdog describe-module NAME prints a module's whole page. A core module's
// page is built into mmwatchdog; the page of a module that is not core is the
// README.md in its directory, after the reference for a module of the
// project's own. --json puts the page in the field page.
func TestDescribeModulePrintsThePage(t *testing.T) {
	dir, official, external := nodeWithModules(t, "", "destinations: {}\n")
	describe := func(arguments ...string) string {
		t.Helper()
		output, err := capture(t, func() error {
			return run(append(append([]string{"describe-module"}, arguments...), "--config-dir", dir))
		})
		if err != nil {
			t.Fatal(arguments, err)
		}
		return output
	}
	httpPage, hasPage := modulepages.CoreModulePage("http")
	if !hasPage || !strings.HasPrefix(httpPage, "# `http`\n") {
		t.Fatalf("the page of http is not built into mmwatchdog; run go generate ./docs/reference/modules: %q", httpPage)
	}
	if got := describe("http"); got != httpPage {
		t.Fatalf("mmwatchdog describe-module http printed\n%s", got)
	}
	const slackPage, queuedJobsPage = "# `slack`\n\nThe page of slack.\n", "# `queued_jobs`\n\nThe page of queued_jobs.\n"
	writeFiles(t, official, map[string]string{"slack/README.md": slackPage})
	writeFiles(t, external, map[string]string{"queued_jobs/README.md": queuedJobsPage})
	if got := describe("slack"); got != slackPage {
		t.Fatalf("mmwatchdog describe-module slack printed\n%s", got)
	}
	reference := describe("queued_jobs", "--reference-only")
	if got := describe("queued_jobs"); got != reference+"\n"+queuedJobsPage || !strings.HasPrefix(reference, "queued_jobs: a module of the project's own (Starlark)") {
		t.Fatalf("mmwatchdog describe-module queued_jobs printed\n%s", got)
	}
	for _, tc := range []struct {
		arguments []string
		page      string
	}{
		{[]string{"http", "--json"}, httpPage},
		{[]string{"slack", "--json"}, slackPage},
		{[]string{"queued_jobs", "--json"}, queuedJobsPage},
		{[]string{"http", "--json", "--reference-only"}, ""},
	} {
		var one struct {
			Name string  `json:"name"`
			Page *string `json:"page"`
		}
		if err := json.Unmarshal([]byte(describe(tc.arguments...)), &one); err != nil || one.Name != tc.arguments[0] || (tc.page == "") != (one.Page == nil) || one.Page != nil && *one.Page != tc.page {
			t.Errorf("%v: %+v %v", tc.arguments, one, err)
		}
	}
	err := run([]string{"describe-module", "--reference-only", "--config-dir", dir})
	if errorCode(err) != 4 || err.Error() != "--reference-only needs the name of a module: mmwatchdog describe-module NAME --reference-only" {
		t.Fatal(err)
	}
}

// mmwatchdog module-usage lists the modules that the node's configuration names:
// the kind of each, the place of its files, and every place that calls it.
// A module that is an executable is marked. mmwatchdog module-usage NAME shows one
// module, and --json gives the same to a program.
func TestModuleUsageCommand(t *testing.T) {
	checks := "check_groups:\n" +
		"  myapp:\n    check_every: 30s\n    time_limit_per_run: 5s\n    alert_after_failing_for: 1m\n    recovered_after_healthy_for: 30s\n    checks:\n" +
		"      myapp_answers_health_request:\n        http: {url: 'http://127.0.0.1:8080/health'}\n        alert_severity: sos\n" +
		"      job_queue_is_short:\n        queued_jobs:\n          queue_file_path: /var/lib/myapp/pending\n        fail_when_measurement_above: {queued_jobs: 500}\n        alert_severity: warn\n" +
		"  backup: {" + timingsOfACheckGroup + ", legacy_backup: {}, alert_severity: warn}\n" +
		"  proxy: {" + timingsOfACheckGroup + ", http: {url: 'http://127.0.0.1/'}, alert_severity: warn}\n"
	destinations := "destinations:\n  team_chat:\n    remind_every: never\n    slack:\n      webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team\n    alert_severities: [info, warn, sos]\n  ops:\n    remind_every: never\n    webhook: {url: 'https://example.com/mmwatchdog'}\n    alert_severities: [sos]\n"
	dir, official, external := nodeWithModules(t, checks, destinations)
	checkFile, destinationFile := filepath.Join(dir, "check_groups", "myapp.yml"), filepath.Join(dir, "alert_destinations.yml")
	usage := func(arguments ...string) string {
		t.Helper()
		output, err := capture(t, func() error { return run(append(append([]string{"module-usage"}, arguments...), "--config-dir", dir)) })
		if err != nil {
			t.Fatal(arguments, err)
		}
		return output
	}
	indent := strings.Repeat(" ", len("legacy_backup")+5)
	want := "Core modules (built into MMWatchdog)\n" +
		"  http            " + checkFile + ":9, " + checkFile + ":17\n" +
		"  webhook         " + destinationFile + ":9\n" +
		"\nOfficial extra modules (Starlark, shipped with MMWatchdog " + version + ")\n" +
		"  slack           " + official + "/slack/\n" + indent + destinationFile + ":4\n" +
		"\nThe project's own modules\n" +
		"  legacy_backup   " + external + "/legacy_backup/   executable\n" + indent + checkFile + ":16\n" +
		indent + "MMWatchdog does not see what an executable does on this node.\n" + indent + "The local node must provide everything the executable needs.\n" +
		"  queued_jobs     " + external + "/queued_jobs/   Starlark\n" + indent + checkFile + ":12\n"
	if got := usage(); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if got, want := usage("queued_jobs"), "The project's own modules\n  queued_jobs   "+external+"/queued_jobs/   Starlark\n"+strings.Repeat(" ", len("queued_jobs")+5)+checkFile+":12\n"; got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if err := run([]string{"module-usage", "slak", "--config-dir", dir}); errorCode(err) != 4 || err.Error() != "no check or alert destination in configuration directory "+dir+" uses module slak" {
		t.Fatal(err)
	}
	var all struct {
		Modules []struct {
			Name                         string `json:"name"`
			Kind                         string `json:"kind"`
			Directory                    string `json:"directory"`
			ShippedWithMMWatchdogVersion string `json:"shipped_with_mmwatchdog_version"`
			Description                  *struct {
				UsedAs string `json:"used_as"`
			} `json:"description"`
			CalledAt []struct {
				File string `json:"file"`
				Line int    `json:"line"`
			} `json:"called_at"`
		} `json:"modules"`
		AlertDestinationsComeFromTheHub *bool `json:"alert_destinations_come_from_the_hub"`
	}
	if err := json.Unmarshal([]byte(usage("--json")), &all); err != nil || len(all.Modules) != 5 || all.AlertDestinationsComeFromTheHub == nil || *all.AlertDestinationsComeFromTheHub {
		t.Fatalf("%+v %v", all, err)
	}
	names := []string{}
	for _, module := range all.Modules {
		names = append(names, module.Name+" "+module.Kind)
		if module.Description == nil || len(module.CalledAt) == 0 || (module.Kind == "core") != (module.Directory == "") {
			t.Errorf("%+v", module)
		}
	}
	if strings.Join(names, ", ") != "http core, webhook core, slack official_extra, legacy_backup project_executable, queued_jobs project_starlark" || all.Modules[2].ShippedWithMMWatchdogVersion != version ||
		all.Modules[0].CalledAt[1].File != checkFile || all.Modules[0].CalledAt[1].Line != 17 || all.Modules[4].Directory != filepath.Join(external, "queued_jobs") {
		t.Fatalf("%v %+v", names, all)
	}
	// The directories of the options take the place of the node's own.
	if err := run([]string{"module-usage", "--config-dir", dir, "--external-module-dir", t.TempDir()}); err == nil || !strings.Contains(err.Error(), `unknown field "queued_jobs"`) {
		t.Fatal(err)
	}
	// A node that has joined a hub gets its destinations from the hub, so the
	// command lists the modules of the node's checks and says so.
	if err := os.Remove(destinationFile); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string]string{"mmwatchdog_node_settings.yml": requiredNodeSettings + "official_extra_module_dir: " + official + "\nexternal_module_dir: " + external + "\njoin_fleet_hub: {join_code_file: /etc/mmwatchdog/secrets/hub-join}\n"})
	joined := usage()
	if !strings.HasPrefix(joined, "Core modules (built into MMWatchdog)\n  http ") || strings.Contains(joined, "slack") || !strings.Contains(joined, "  queued_jobs     "+external+"/queued_jobs/   Starlark\n") ||
		!strings.HasSuffix(joined, "\n\nThis node has joined a hub, and its alert destinations come from the hub. Run mmwatchdog module-usage on the hub to see the modules of the destinations.\n") {
		t.Fatal(joined)
	}
	if err := json.Unmarshal([]byte(usage("--json")), &all); err != nil || !*all.AlertDestinationsComeFromTheHub {
		t.Fatalf("%+v %v", all, err)
	}
}

// mmwatchdog status shows, below a check whose module is not core, which kind of
// module that is, because the configuration does not show it. The JSON
// status names the module and its kind for every check.
func TestStatusShowsTheKindOfAModuleThatIsNotCore(t *testing.T) {
	check := func(name, module, kind string) incidents.CheckStatus {
		return incidents.CheckStatus{CheckState: incidents.CheckState{CheckGroup: "myapp", Check: name}, Module: module, ModuleKind: kind, State: "ok"}
	}
	b, _ := json.Marshal(map[string]any{"summary": map[string]any{"exit_code": 0, "attention": []any{}}, "checks": map[string]any{
		"myapp/job_queue_is_short": check("job_queue_is_short", "queued_jobs", "project_starlark"),
		"myapp/backup_is_recent":   check("backup_is_recent", "legacy_backup", "project_executable"),
		"myapp/pool_is_healthy":    check("pool_is_healthy", "zfs_pool", "official_extra"),
		"myapp/myapp_answers":      check("myapp_answers", "http", "core"),
		"myapp/heartbeat":          check("heartbeat", "", ""),
	}})
	text, err := capture(t, func() error { return printStatus(b, "", false) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"myapp/job_queue_is_short     ok          ", "\n  module queued_jobs: the project's own, Starlark\n", "\n  module legacy_backup: the project's own, executable\n", "\n  module zfs_pool: official extra\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("mmwatchdog status lacks %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "  module ") != 3 {
		t.Fatalf("mmwatchdog status names the module of a check whose module is core, or of a check without a module:\n%s", text)
	}
	out, err := capture(t, func() error { return printStatus(b, "", true) })
	for _, want := range []string{`"module": "queued_jobs"`, `"module_kind": "project_starlark"`, `"module": "http"`, `"module_kind": "core"`, `"module_kind": "official_extra"`, `"module_kind": "project_executable"`} {
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("the JSON status lacks %s: %s (%v)", want, out, err)
		}
	}
}

// withStandardInput runs fn while the standard input of the command holds
// text.
func withStandardInput(t *testing.T, text string, fn func() error) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "standard-input")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	original := os.Stdin
	os.Stdin = input
	defer func() { os.Stdin = original }()
	return fn()
}

// mmwatchdog run-starlark-module DIRECTORY runs the script of one module once: it
// reads one request on its standard input and writes the result to its
// standard output. The daemon starts this command for every run of a check
// and for every delivery whose module is a script. An error in the script
// ends the command with an exit status other than zero and names the line.
func TestRunStarlarkModuleCommand(t *testing.T) {
	modules := t.TempDir()
	queue := filepath.Join(modules, "pending")
	writeFiles(t, modules, map[string]string{"pending": "780\n", "queued_jobs/module.star": queuedJobsScript, "slack/module.star": slackScript,
		"broken/module.star": "def run_check(inputs, file_contents):\n    return {\"status\": \"ok\", \"message\": inputs[\"queue\"]}\n"})
	request := `{"protocol":1,"inputs":{"queue_file_path":"` + queue + `"},"state_dir":"` + t.TempDir() + `"}`
	runScript := func(module, request string) (string, error) {
		t.Helper()
		return capture(t, func() error {
			return withStandardInput(t, request, func() error { return run([]string{"run-starlark-module", filepath.Join(modules, module)}) })
		})
	}
	if output, err := runScript("queued_jobs", request); err != nil || output != `{"measurements":{"queued_jobs":780},"message":"780 queued jobs","protocol":1,"status":"ok"}`+"\n" {
		t.Fatal(output, err)
	}
	// A request with an alert is a delivery.
	delivery := `{"protocol":1,"alert":{"title":"app-1: myapp/http failed","title_with_alert_severity":"SOS: app-1: myapp/http failed"},"inputs":{},"file_contents":{}}`
	if output, err := runScript("slack", delivery); err != nil || output != `{"message":"Slack accepted the message","protocol":1,"status":"delivered"}`+"\n" {
		t.Fatal(output, err)
	}
	for _, tc := range []struct{ module, request, want string }{
		{"broken", request, filepath.Join(modules, "broken", "module.star") + `:2: key "queue" not in dict`},
		{"slack", request, filepath.Join(modules, "slack", "module.star") + " defines no function run_check(inputs, file_contents)"},
		{"queued_jobs", delivery, filepath.Join(modules, "queued_jobs", "module.star") + " defines no function deliver_alert(alert, inputs, file_contents)"},
		{"queued_jobs", `{"protocol":2,"inputs":{}}`, "the module request must use protocol: 1"},
		{"queued_jobs", "", "the request is not one JSON object: EOF"},
		{"no_such_module", request, "open " + filepath.Join(modules, "no_such_module", "module.star") + ": no such file or directory"},
	} {
		if output, err := runScript(tc.module, tc.request); err == nil || errorCode(err) == 0 || err.Error() != tc.want || output != "" {
			t.Errorf("%s: %q %v, want %s", tc.module, output, err, tc.want)
		}
	}
	if err := run([]string{"run-starlark-module"}); errorCode(err) != 4 || err.Error() != "run-starlark-module requires the directory of a module, which holds the script module.star" {
		t.Fatal(err)
	}
}

// mmwatchdog module-json result writes the result of an executable module: each
// measurement as its name and its number, without a unit, and each finding
// as its ID and its message.
func TestModuleJSONResultCommand(t *testing.T) {
	output, err := capture(t, func() error {
		return run([]string{"module-json", "result", "--status", "failed", "--message", "1 certificate expires within 14 days", "--measurement", "days_left", "3", "--finding", "www.example.com", "the certificate of www.example.com expires in 3 days"})
	})
	if err != nil || output != `{"protocol":1,"status":"failed","message":"1 certificate expires within 14 days","measurements":{"days_left":3},"findings":[{"id":"www.example.com","message":"the certificate of www.example.com expires in 3 days"}]}`+"\n" {
		t.Fatal(output, err)
	}
	// The unit of a measurement is in the module's description, so a third
	// word after --measurement is no unit.
	if err := run([]string{"module-json", "result", "--status", "ok", "--message", "780 queued jobs", "--measurement", "queued_jobs", "780", "count"}); errorCode(err) != 4 {
		t.Fatal(err)
	}
}

// MMWatchdog runs with no destination only when alert_destinations.yml says
// destinations: {}. mmwatchdog validate and mmwatchdog status then say that this node sends
// alerts to nobody, and mmwatchdog status names a check's destinations.
func TestNoDestinationIsSaidPlainly(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "check_groups"), 0700)
	os.WriteFile(filepath.Join(dir, "mmwatchdog_node_settings.yml"), []byte(nodeSettingsOfATest(dir)), 0600)
	os.WriteFile(filepath.Join(dir, "check_groups", "checks.yml"), []byte("check_groups: {app: {check_every: 30s, time_limit_per_run: 5s, alert_after_failing_for: 1m, recovered_after_healthy_for: 30s, systemd_unit_state: {systemd_unit: myapp.service, fail_when: the_unit_is_not_active}, alert_severity: warn}}\n"), 0600)
	if err := run([]string{"validate", "--config-dir", dir}); err == nil || !strings.Contains(err.Error(), `alert_destinations.yml: alert destinations file is missing. Create the file with a destination, or write "destinations: {}"`) {
		t.Fatal("a missing destinations file was accepted", err)
	}
	os.WriteFile(filepath.Join(dir, "alert_destinations.yml"), []byte("destinations: {}\n"), 0600)
	output, err := capture(t, func() error { return run([]string{"validate", "--config-dir", dir}) })
	if err != nil || output != "Valid configuration: 1 check group, 0 alert destinations\nThis node sends alerts to nobody.\napp/systemd_unit_state: no destination receives alerts with alert_severity warn; add the severity to a destination's alert_severities, or change the check's send_alerts_only_to_destinations.\n" {
		t.Fatalf("%q %v", output, err)
	}
	names, nobody := []string{"mail_administrator_chat", "mail_admin_chat"}, []string{}
	named := incidents.CheckStatus{CheckState: incidents.CheckState{CheckGroup: "mail", Check: "postfix", SendAlertsOnlyToDestinations: &names}, State: "ok", AlertDestinationsTheHubDoesNotHave: []string{"mail_admin_chat"}}
	silent := incidents.CheckStatus{CheckState: incidents.CheckState{CheckGroup: "mail", Check: "queue", SendAlertsOnlyToDestinations: &nobody}, State: "ok"}
	status, _ := json.Marshal(map[string]any{"summary": map[string]any{"state": "ok"}, "checks": map[string]incidents.CheckStatus{"mail/postfix": named, "mail/queue": silent}, "sends_alerts_to_nobody": true})
	output, err = capture(t, func() error { return printStatus(status, "mail", false) })
	for _, want := range []string{
		"  Sends alerts only to: mail_administrator_chat, mail_admin_chat\n",
		"  The hub has no destination named mail_admin_chat. Destinations listed in send_alerts_only_to_destinations that the hub knows still receive alerts. If the hub knows none of the listed destinations, the check's alerts go to every destination whose alert_severities include the alert's severity.\n",
		"  Sends alerts to no destination (send_alerts_only_to_destinations: [])\n",
		"This node sends alerts to nobody.\n",
	} {
		if err != nil || !strings.Contains(output, want) {
			t.Fatalf("missing %q in %q (%v)", want, output, err)
		}
	}
}

// mmwatchdog validate names every check whose alert title would be longer than 120
// characters on this node, with the title as MMWatchdog would send it. The
// configuration stays valid.
func TestValidateNamesAlertTitlesThatWillBeShortened(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "check_groups"), 0700)
	os.WriteFile(filepath.Join(dir, "mmwatchdog_node_settings.yml"), []byte(nodeSettingsOfATest(dir)), 0600)
	os.WriteFile(filepath.Join(dir, "alert_destinations.yml"), []byte("destinations: {ops: {remind_every: never, webhook: {url: 'https://example.com/events'}, alert_severities: [warn, sos]}}\n"), 0600)
	// Whatever this machine is called, the long check name needs a cut and
	// the short one, in the same check group, does not.
	group, long := strings.Repeat("g", 20), strings.Repeat("c", 64)
	checks := "check_groups:\n  " + group + ":\n    check_every: 30s\n    time_limit_per_run: 5s\n    alert_after_failing_for: 1m\n    recovered_after_healthy_for: 30s\n    checks:\n" +
		"      " + long + ": {systemd_unit_state: {systemd_unit: myapp.service, fail_when: the_unit_is_not_active}, alert_severity: warn}\n      short: {systemd_unit_state: {systemd_unit: myapp.service, fail_when: the_unit_is_not_active}, alert_severity: warn}\n"
	os.WriteFile(filepath.Join(dir, "check_groups", "checks.yml"), []byte(checks), 0600)
	output, err := capture(t, func() error { return run([]string{"validate", "--config-dir", dir}) })
	if err != nil {
		t.Fatal(err)
	}
	node, _ := hostName()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 3 || lines[0] != "Valid configuration: 1 check group, 1 alert destination" ||
		lines[1] != "An alert title has at most 120 characters. For node "+node+", MMWatchdog will shorten the alert titles listed below; shorten the check group name or check name to keep the full title:" ||
		!strings.HasPrefix(lines[2], "  "+group+"/"+long+": the check's longest alert title has ") || !strings.HasSuffix(lines[2], "…, now SOS") {
		t.Fatalf("%q", output)
	}
	output, err = capture(t, func() error { return run([]string{"validate", "--json", "--config-dir", dir}) })
	var result struct {
		Shortened []checksandalerts.AlertTitleThatWillBeShortened `json:"alert_titles_that_will_be_shortened"`
	}
	if err == nil {
		err = json.Unmarshal([]byte(output), &result)
	}
	if err != nil || len(result.Shortened) != 1 || result.Shortened[0].Check != long || result.Shortened[0].Characters <= 120 || len([]rune(result.Shortened[0].Title)) != 120 {
		t.Fatal(result, err)
	}
}

func TestStatusUsesDaemonSummary(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		for _, code := range []int{0, 1, 3} {
			t.Run(fmt.Sprintf("exit %d/json=%t", code, asJSON), func(t *testing.T) {
				check := map[string]any{"check_group": "app", "check": "shell", "state": "failed", "failure_incident": map[string]any{"incident_id": "open-incident"}, "last_observation": map[string]any{"status": "unknown", "message": "last observation"}}
				b, err := json.Marshal(map[string]any{"summary": map[string]any{"exit_code": code, "attention": []any{}}, "checks": map[string]any{"app/shell": check}})
				if err != nil {
					t.Fatal(err)
				}
				reader, writer, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
				original := os.Stdout
				os.Stdout = writer
				err = printStatus(b, "", asJSON)
				os.Stdout = original
				writer.Close()
				var got exitStatus
				if code == 0 && err != nil || code != 0 && (!errors.As(err, &got) || int(got) != code) {
					t.Fatalf("exit %d: %v", code, err)
				}
				output, err := io.ReadAll(reader)
				if err != nil {
					t.Fatal(err)
				}
				want := "no current result; the failure alert stays open: last observation"
				if asJSON {
					want = fmt.Sprintf(`"exit_code": %d`, code)
				}
				if !strings.Contains(string(output), want) {
					t.Fatal(string(output))
				}
			})
		}
	}
}

// capture returns what fn prints to standard output.
func capture(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	// The output is read while fn writes it: a module page is larger than
	// the buffer of a pipe.
	type readResult struct {
		output []byte
		err    error
	}
	read := make(chan readResult)
	go func() {
		output, err := io.ReadAll(reader)
		read <- readResult{output, err}
	}()
	original := os.Stdout
	os.Stdout = writer
	err = fn()
	os.Stdout = original
	writer.Close()
	result := <-read
	if result.err != nil {
		t.Fatal(result.err)
	}
	return string(result.output), err
}

// Healthy nodes compress to a count; others expand, and text a node sent
// cannot write terminal escapes.
func TestFleetViewCompressesHealthyServers(t *testing.T) {
	now := time.Now().UTC()
	report := func(code int, attention ...incidents.Attention) *fleet.NodeStatusReport {
		return &fleet.NodeStatusReport{Summary: incidents.StatusSummary{ExitCode: code, Attention: append([]incidents.Attention{}, attention...)}, NodeResources: map[string]checksandalerts.Measurement{"disk": {Value: 71, Unit: "percent"}}}
	}
	fleet := fleet.Fleet{ExitCode: 1, Address: "util:8445", OutageAlerters: []string{"app-2", "app-3"}, Nodes: []fleet.FleetNode{
		{Name: "app-1", State: "reporting", LastReport: now, NodeStatusReport: report(0)},
		{Name: "app-2", State: "reporting", ExitCode: 1, OutageAlerter: true, PreferOutageAlerter: true, LastReport: now, NodeStatusReport: report(1, incidents.Attention{Check: "web/http", State: "failed", Since: now, Message: "bad \x1b[2J gateway"}, incidents.Attention{Check: "web/disk", State: "unknown", Message: "has not run yet"})},
		{Name: "app-3", State: "silent", OutageAlerter: true, ExitCode: 1, LastReport: now.Add(-5 * time.Minute), NodeStatusReport: report(0)},
	}}
	b, err := json.Marshal(fleet)
	if err != nil {
		t.Fatal(err)
	}
	output, err := capture(t, func() error { return printFleet(b, false, now) })
	var code exitStatus
	if !errors.As(err, &code) || code != 1 {
		t.Fatal("fleet exit", err)
	}
	for _, want := range []string{"3 nodes, 1 healthy", "app-2", "disk 71%", "web/http failed", "bad �[2J gateway", "web/disk unknown: has not run yet", "app-3", "silent", "(5m0s ago)", "Hub-outage alerter nodes: app-2 (prefer_this_node_as_hub_outage_alerter is enabled), app-3"} {
		if !strings.Contains(output, want) {
			t.Errorf("missing %q: %s", want, output)
		}
	}
	// A check that has not run yet has no time, which shows as no date at all.
	if strings.Contains(output, "app-1") || strings.Contains(output, "\x1b") || strings.Contains(output, "0001-01-01") {
		t.Fatal(output)
	}
	output, _ = capture(t, func() error { return printFleet(b, true, now) })
	if strings.Contains(output, `"since": "0001-01-01`) || !strings.Contains(output, `\u001b[2J`) || !strings.Contains(output, `"app-1"`) || !strings.Contains(output, `"hub_outage_alerter_nodes": [`) || strings.Contains(output, `"hub_outage_alerter_node":`) {
		t.Fatal("JSON output", output)
	}
}

// mmwatchdog fleet says how far back the hub's measurement history of nodes reaches
// when its size limit cut it short. The line is information: it does not
// change the exit code. The view of one node group leaves it out, and the
// JSON carries the times and both settings.
func TestFleetViewShowsAMeasurementHistoryOfNodesThatTheSizeLimitCutShort(t *testing.T) {
	now := time.Now().UTC()
	oldest := now.Add(-(5*24*time.Hour + 3*time.Hour + 10*time.Minute))
	history := store.MeasurementHistoryOfNodesStatus{OldestReadingAt: &oldest, NewestReadingAt: &now, KeepMeasurementHistoryOfNodesFor: "7d", MeasurementHistoryOfNodesSizeLimit: "20GiB", CutShortByMeasurementHistoryOfNodesSizeLimit: true}
	report := &fleet.NodeStatusReport{Summary: incidents.StatusSummary{ExitCode: 0, Attention: []incidents.Attention{}}}
	fleet := fleet.Fleet{Address: "util:8445", Nodes: []fleet.FleetNode{{Name: "app-1", State: "reporting", LastReport: now, NodeGroups: []string{"frankfurt"}, NodeStatusReport: report}}, MeasurementHistoryOfNodes: history}
	view := func() []byte {
		b, _ := json.Marshal(fleet)
		return b
	}
	sentence := "measurement history of nodes reaches back 5 days 3 hours; the size limit of 20 GiB prevented keeping the requested 7 days\n"
	output, err := capture(t, func() error { return printFleet(view(), false, now) })
	if err != nil || !strings.Contains(output, "\n"+sentence) {
		t.Fatal(output, err)
	}
	output, err = capture(t, func() error { return printFleet(view(), true, now) })
	for _, want := range []string{`"measurement_history_of_nodes": {`, `"oldest_reading_at": "`, `"keep_measurement_history_of_nodes_for": "7d"`, `"measurement_history_of_nodes_size_limit": "20GiB"`, `"cut_short_by_measurement_history_of_nodes_size_limit": true`} {
		if err != nil || !strings.Contains(output, want) {
			t.Fatalf("missing %q in %q (%v)", want, output, err)
		}
	}
	fleet.NodeGroup = "frankfurt"
	if output, err = capture(t, func() error { return printFleet(view(), false, now) }); err != nil || strings.Contains(output, "measurement history") {
		t.Fatal(output, err)
	}
	fleet.NodeGroup, fleet.MeasurementHistoryOfNodes.CutShortByMeasurementHistoryOfNodesSizeLimit = "", false
	if output, err = capture(t, func() error { return printFleet(view(), false, now) }); err != nil || strings.Contains(output, "measurement history") {
		t.Fatal(output, err)
	}
}

// mmwatchdog fleet shows the node groups of each node that it lists. The view of one
// node group says which node group it shows and leaves out what is about the
// whole fleet. mmwatchdog fleet NODE shows the node's node groups, and the JSON
// carries them as node_groups.
func TestFleetViewShowsNodeGroups(t *testing.T) {
	now := time.Now().UTC()
	report := &fleet.NodeStatusReport{Summary: incidents.StatusSummary{ExitCode: 0, Attention: []incidents.Attention{}}}
	nodes := []fleet.FleetNode{
		{Name: "app-1", State: "reporting", LastReport: now, NodeGroups: []string{"application_servers", "frankfurt"}, NodeStatusReport: report},
		{Name: "app-2", State: "silent", ExitCode: 1, OutageAlerter: true, LastReport: now.Add(-5 * time.Minute), NodeGroups: []string{"application_servers", "frankfurt"}, NodeStatusReport: report},
		{Name: "old-1", State: "silent", ExitCode: 1, LastReport: now.Add(-5 * time.Minute), NodeGroups: []string{}, NodeStatusReport: report},
	}
	refused := fleet.RefusedJoins{Count: 2, Last: now, Address: "10.0.0.9"}
	whole, _ := json.Marshal(fleet.Fleet{ExitCode: 1, Address: "util:8445", Nodes: nodes, RefusedJoins: refused})
	output, err := capture(t, func() error { return printFleet(whole, false, now) })
	var code exitStatus
	if !errors.As(err, &code) || code != 1 {
		t.Fatal(err)
	}
	for _, want := range []string{"Fleet at util:8445: 3 nodes, 1 healthy\n", "(5m0s ago)\n  Node groups: application_servers, frankfurt\n", "Hub-outage alerter nodes: app-2\n", "Refused joins: 2 "} {
		if !strings.Contains(output, want) {
			t.Errorf("missing %q in %q", want, output)
		}
	}
	if strings.Count(output, "Node groups:") != 1 {
		t.Fatal(output)
	}
	group, _ := json.Marshal(fleet.Fleet{ExitCode: 1, Address: "util:8445", NodeGroup: "application_servers", Nodes: nodes[:2], RefusedJoins: refused})
	output, err = capture(t, func() error { return printFleet(group, false, now) })
	if !errors.As(err, &code) || code != 1 || !strings.HasPrefix(output, "Node group application_servers of the fleet at util:8445: 2 nodes, 1 healthy\napp-2 ") || strings.Contains(output, "Hub-outage alerter") || strings.Contains(output, "Refused joins") {
		t.Fatal(output, err)
	}
	if output, _ = capture(t, func() error { return printFleet(group, true, now) }); !strings.Contains(output, `"node_group": "application_servers"`) || !strings.Contains(output, `"this_node_is_in_node_groups": [`) {
		t.Fatal(output)
	}
	one, _ := json.Marshal(nodes[0])
	if output, err = capture(t, func() error { return printNode(one, false, now) }); err != nil || !strings.Contains(output, "\n  Node groups: application_servers, frankfurt\n") {
		t.Fatal(output, err)
	}
	none, _ := json.Marshal(nodes[2])
	if output, _ = capture(t, func() error { return printNode(none, false, now) }); !strings.Contains(output, "\n  Node groups: none\n") {
		t.Fatal(output)
	}
}

func TestLoginUser(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for content, want := range map[string]string{u.Uid + "\n": u.Username, "4294967295": "", "": ""} {
		path := filepath.Join(dir, "loginuid")
		os.WriteFile(path, []byte(content), 0600)
		if got := loginUser(path); got != want {
			t.Errorf("loginuid %q: got %q, want %q", content, got, want)
		}
	}
	if got := loginUser(filepath.Join(dir, "absent")); got != "" {
		t.Fatal(got)
	}
}

// Fleet subcommands validate their own arguments; rotation is a separate command.
func TestFleetCommandArguments(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "absent.sock")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"fleet", "forget"}, "fleet forget requires a node name"},
		{[]string{"fleet", "forget", "app-2", "--socket", socket}, "absent.sock"},
		{[]string{"fleet", "rotate-join-code", "--socket", socket}, "absent.sock"},
		{[]string{"fleet", "app-2", "--rotate"}, "flag provided but not defined"},
		{[]string{"fleet", "forget", "app-2", "app-3"}, "unexpected arguments"},
		{[]string{"fleet", "--node-group", "application_servers", "--socket", socket}, "absent.sock"},
		{[]string{"fleet", "app-2", "--node-group", "application_servers"}, "mmwatchdog fleet --node-group shows the nodes of one node group and takes nothing else: remove app-2 or --node-group"},
		{[]string{"fleet", "join-code", "--node-group", "application_servers"}, "mmwatchdog fleet --node-group shows the nodes of one node group and takes nothing else: remove join-code or --node-group"},
		{[]string{"fleet", "--node-group", ""}, "--node-group requires the name of a node group"},
		{[]string{"status", "--node-group", "application_servers"}, "flag provided but not defined"},
	} {
		if err := run(tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v", tc.args, err)
		}
	}
}

func TestCommandErrorsAndRetiredNames(t *testing.T) {
	for _, args := range [][]string{
		{"test", "app"}, {"reset", "app"}, {"json", "get", "x", "string"},
		{"validate", "--config", "x"}, {"validate", "--fragments-dir", "x"}, {"validate", "--alerts", "x"},
		{"check-now"}, {"check-now", "app", "--config-dir", "x"}, {"check-now", "app", "--without-daemon", "--count-as-scheduled-run"},
		{"maintenance", "app", "--for", "1h"}, {"maintenance", "on", "--all", "--for", "1h"},
		{"maintenance", "on", "app"}, {"maintenance", "on", "app", "--for", "0s"}, {"maintenance", "on", "app", "--for", "-1h"},
		{"maintenance", "on", "app", "--every-check-group", "--for", "1h"}, {"maintenance", "off", "app", "--for", "0s"}, {"maintenance", "off", "app", "--reason", "done"},
		{"fleet", "join-code", "--rotate"}, {"heartbeat", "app", "--url", "https://example.com", "--token-file", "x"},
		{"send-notice"}, {"send-notice", "--title", "x"}, {"send-notice", "--alert-severity", "sos"}, {"send-notice", "x", "--alert-severity", "sos", "--title", "x"},
		{"send-notice", "--alert-severity", "sos", "--title", "x", "--json"},
		{"validate", "--external-module-dir", ""}, {"validate", "--official-extra-module-dir", ""}, {"check-now", "app", "--without-daemon", "--external-module-dir", "/x"},
		{"describe-module", "http", "tcp"}, {"describe-module", "--config-only"}, {"module-usage", "http", "tcp"}, {"module-usage", "--socket", "/x"}, {"run-starlark-module"}, {"run-starlark-module", "/x", "--json"},
		{"module-json", "result", "--status", "ok", "--message", "x", "--measurement", "x", "1", "count"},
	} {
		if err := run(args); errorCode(err) != 4 {
			t.Fatalf("%v: %v (exit %d)", args, err, errorCode(err))
		}
	}
	missing := filepath.Join(t.TempDir(), "missing")
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"validate", "--config-dir", missing}, 1},
		{[]string{"check-now", "app", "--without-daemon", "--config-dir", missing}, 4},
		{[]string{"status", "--socket", missing}, 4}, {[]string{"reload", "--socket", missing}, 4},
		// A notice that cannot be queued is not sent: a command error.
		{[]string{"send-notice", "--alert-severity", "sos", "--title", "x", "--socket", missing}, 4},
	} {
		if err := run(tc.args); errorCode(err) != tc.code {
			t.Fatal(tc.args, err)
		}
	}
	if errorCode(context.Canceled) != 4 {
		t.Fatal("cancellation without Ctrl-C is a command error")
	}
}

func TestCheckNowExitPrecedenceAndMaintenanceOutput(t *testing.T) {
	directory := t.TempDir()
	socket := filepath.Join(os.TempDir(), "mmwatchdog-cli-"+incidents.ID()+".sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(socket)
	received := make(chan string, 10)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Method + " " + r.URL.Path
		switch r.URL.Path {
		case "/check-now/app":
			if r.URL.RawQuery == "count_as_scheduled_run=true" {
				json.NewEncoder(w).Encode(map[string]checksandalerts.Observation{"a": {Status: "ok", Message: "the restore drill passed"}})
				return
			}
			json.NewEncoder(w).Encode(map[string]checksandalerts.Observation{"a": {Status: "unknown"}, "b": {Status: "failed"}})
		case "/maintenance/on/app":
			json.NewEncoder(w).Encode(map[string]any{"maintenance": checksandalerts.Window{Until: time.Date(2026, 9, 30, 15, 2, 0, 0, time.UTC)}})
		case "/maintenance/off/app", "/maintenance/off/worker_1,worker_2,worker_3", "/maintenance/off/every_check_group":
			json.NewEncoder(w).Encode(map[string]any{"maintenance": nil})
		case "/maintenance/on/worker_1,worker_2,worker_3", "/maintenance/on/every_check_group":
			json.NewEncoder(w).Encode(map[string]any{"maintenance": checksandalerts.Window{Until: time.Date(2026, 9, 30, 14, 12, 0, 0, time.UTC)}})
		default:
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":"change could not be committed"}`)
		}
	}))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	capture := func(args []string) (string, error) {
		t.Helper()
		file, err := os.CreateTemp(directory, "output")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		original := os.Stdout
		os.Stdout = file
		defer func() { os.Stdout = original }()
		err = run(append(args, "--socket", socket))
		file.Seek(0, 0)
		b, _ := io.ReadAll(file)
		return string(b), err
	}
	for _, args := range [][]string{{"check-now", "app"}, {"check-now", "app", "--json"}} {
		if _, err := capture(args); errorCode(err) != 1 {
			t.Fatal(err)
		}
	}
	// With --count-as-scheduled-run the daemon runs the scheduled runs, and the output says that they counted.
	if output, err := capture([]string{"check-now", "app", "--count-as-scheduled-run"}); err != nil ||
		output != "app/a                    ok       the restore drill passed\nThe runs counted as scheduled runs for check group app; run mmwatchdog status app to see the resulting check status.\n" {
		t.Fatalf("%q %v", output, err)
	}
	output, err := capture([]string{"maintenance", "on", "app", "--for", "1h", "--reason", "fixing"})
	if err != nil {
		t.Fatal(err)
	}
	if output != "app is in maintenance until 2026-09-30 15:02 UTC (1h).\n"+
		"  MMWatchdog keeps running the checks of app and shows the results in mmwatchdog status.\n"+
		"  During maintenance, MMWatchdog sends no alert and attempts no systemd unit restart or recovery command for app.\n"+
		"  To end the maintenance early: sudo mmwatchdog maintenance off app\n" {
		t.Fatal(output)
	}
	output, err = capture([]string{"maintenance", "off", "app"})
	if err != nil || output != "app is out of maintenance. Alerts, restarts and recovery commands resume after fresh checks.\n" {
		t.Fatal(output, err)
	}
	// One command covers several check groups, and a window can last for days.
	output, err = capture([]string{"maintenance", "on", "worker_1", "worker_2", "worker_3", "--for", "30d", "--reason", "deploy v1.4"})
	if err != nil || output != "worker_1, worker_2, worker_3 are in maintenance until 2026-09-30 14:12 UTC (30d).\n"+
		"  MMWatchdog keeps running the checks of the check groups in maintenance and shows the results in mmwatchdog status.\n"+
		"  During maintenance, MMWatchdog sends no alert and attempts no systemd unit restart or recovery command for the check groups in maintenance.\n"+
		"  To end the maintenance early: sudo mmwatchdog maintenance off worker_1 worker_2 worker_3\n" {
		t.Fatal(output, err)
	}
	output, err = capture([]string{"maintenance", "off", "worker_1", "worker_2", "worker_3"})
	if err != nil || output != "worker_1, worker_2, worker_3 are out of maintenance. Alerts, restarts and recovery commands resume after fresh checks.\n" {
		t.Fatal(output, err)
	}
	output, err = capture([]string{"maintenance", "on", "--every-check-group", "--for", "15m"})
	if err != nil || output != "Every check group on this node is in maintenance until 2026-09-30 14:12 UTC (15m).\n"+
		"  MMWatchdog keeps running the checks of every check group and shows the results in mmwatchdog status.\n"+
		"  During maintenance, MMWatchdog sends no alert and attempts no systemd unit restart or recovery command for the check groups in maintenance.\n"+
		"  To end the maintenance early: sudo mmwatchdog maintenance off --every-check-group\n" {
		t.Fatal(output, err)
	}
	output, err = capture([]string{"maintenance", "off", "--every-check-group"})
	if err != nil || output != "The maintenance for every check group on this node is off. Alerts, restarts and recovery commands resume after fresh checks. A check group that has its own maintenance window stays in maintenance until that window ends.\n" {
		t.Fatal(output, err)
	}
	for _, args := range [][]string{{"maintenance", "on", "--for", "1h"}, {"maintenance", "on", "app", "--every-check-group", "--for", "1h"}, {"maintenance", "on", "app,web", "--for", "1h"}, {"maintenance", "on", "app"}, {"maintenance", "on", "app", "--for", "0s"}} {
		if _, err := capture(args); errorCode(err) != 4 {
			t.Fatal(args, err)
		}
	}
	if _, err := capture([]string{"reload"}); errorCode(err) != 4 {
		t.Fatal(err)
	}
	// The commands the CLI refused sent nothing to the daemon.
	if len(received) != 10 {
		t.Fatal(len(received))
	}
}

// mmwatchdog send-notice prints where the notice went and the result of the first
// delivery attempt to each destination. It exits 0 when every attempt
// succeeded or no destination takes the notice, 1 when an attempt did not
// succeed, and 4 when the daemon refused the notice.
func TestSendNoticeOutput(t *testing.T) {
	socket := filepath.Join(os.TempDir(), "mmwatchdog-cli-"+incidents.ID()+".sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(socket)
	var answer store.NoticeResult
	var received store.NoticeRequest
	refuse := ""
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/send-notice" || json.NewDecoder(r.Body).Decode(&received) != nil {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if refuse != "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": refuse})
			return
		}
		json.NewEncoder(w).Encode(answer)
	}))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	send := func(args ...string) (string, error) {
		t.Helper()
		return capture(t, func() error { return run(append([]string{"send-notice", "--socket", socket}, args...)) })
	}
	delivered := func(name string) store.NoticeDelivery {
		return store.NoticeDelivery{Destination: name, Result: store.NoticeDelivered}
	}
	const title = "Test of the alert destinations. Nothing is wrong."

	answer = store.NoticeResult{Deliveries: []store.NoticeDelivery{delivered("team_chat"), delivered("on_call_pager")}}
	output, err := send("--alert-severity", "sos", "--title", title, "--body", "more")
	if err != nil || output != "Notice sent to every destination that takes sos:\n  team_chat       delivered\n  on_call_pager   delivered\n" {
		t.Fatalf("%q %v", output, err)
	}
	if received != (store.NoticeRequest{AlertSeverity: "sos", Title: title, Body: "more"}) {
		t.Fatalf("%+v", received)
	}

	answer = store.NoticeResult{Deliveries: []store.NoticeDelivery{delivered("team_chat")}}
	output, err = send("--alert-severity", "info", "--title", title, "--destination", "team_chat")
	if err != nil || output != "Notice sent to the destination team_chat:\n  team_chat   delivered\n" || received.Destination != "team_chat" {
		t.Fatalf("%q %v %+v", output, err, received)
	}

	answer = store.NoticeResult{Deliveries: []store.NoticeDelivery{}}
	output, err = send("--alert-severity", "info", "--title", title)
	if err != nil || output != "No destination takes info, so MMWatchdog sent the notice to nobody. The notice is recorded in mmwatchdog events.\n" {
		t.Fatalf("%q %v", output, err)
	}

	// A failed first attempt, and a destination with earlier alerts that it
	// has not taken: exit code 1, and MMWatchdog keeps trying.
	answer = store.NoticeResult{Deliveries: []store.NoticeDelivery{
		{Destination: "on_call_pager", Result: store.NoticeFailed, Error: "webhook returned HTTP 401"},
		{Destination: "mail", Result: store.NoticeWaitsBehindEarlierAlerts, Error: "webhook returned HTTP 503", EarlierUndeliveredAlerts: 3},
		delivered("team_chat"),
	}}
	output, err = send("--alert-severity", "sos", "--title", title)
	if errorCode(err) != 1 || err.Error() != "the notice was not delivered at the first attempt to 2 of 3 destinations. MMWatchdog keeps the notice queued and tries again" ||
		output != "Notice sent to every destination that takes sos:\n"+
			"  on_call_pager   failed: webhook returned HTTP 401\n"+
			"  mail            not attempted yet: 3 earlier alerts are not delivered, and the latest attempt failed: webhook returned HTTP 503\n"+
			"  team_chat       delivered\n" {
		t.Fatalf("%q %v", output, err)
	}

	// A node that has joined a hub learns whether the hub accepted the notice.
	answer = store.NoticeResult{SentThroughFleetHubAt: "hub.example.com:8445", SendAlertWithoutHubAfterWaitingFor: "30s", Deliveries: []store.NoticeDelivery{delivered("hub")}}
	output, err = send("--alert-severity", "warn", "--title", title)
	if err != nil || output != "Notice sent to the MMWatchdog fleet hub at hub.example.com:8445, which delivers it to every destination that takes warn:\n"+
		"  MMWatchdog fleet hub   accepted\n"+
		"To see the result for each destination, send the notice from the fleet hub: run the same mmwatchdog send-notice command there.\n" {
		t.Fatalf("%q %v", output, err)
	}
	answer.Deliveries = []store.NoticeDelivery{{Destination: "hub", Result: store.NoticeFailed, Error: "hub returned HTTP 503: alert could not be saved"}}
	output, err = send("--alert-severity", "warn", "--title", title, "--destination", "team_chat")
	if errorCode(err) != 1 || !strings.Contains(err.Error(), "if the hub has not accepted the notice after 30s (send_alert_without_hub_after_waiting_for), the local node sends the notice directly to the alert destinations") ||
		output != "Notice sent to the MMWatchdog fleet hub at hub.example.com:8445, which delivers it to the destination team_chat:\n"+
			"  MMWatchdog fleet hub   did not accept the notice: hub returned HTTP 503: alert could not be saved\n" {
		t.Fatalf("%q %v", output, err)
	}

	// The daemon's refusal is a command error, with the daemon's words.
	refuse = "the title would have 123 characters with the node's name in front (\"app-1: \"), and a title has at most 120: shorten --title by 3 characters"
	if output, err = send("--alert-severity", "sos", "--title", title); errorCode(err) != 4 || err.Error() != refuse || output != "" {
		t.Fatalf("%q %v", output, err)
	}
}

func TestReadableRecoveryAttemptAndMaintenanceStatus(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "status")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	original := os.Stdout
	os.Stdout = file
	defer func() { os.Stdout = original }()
	b, _ := json.Marshal(map[string]any{
		"summary": incidents.StatusSummary{}, "systemd_unit_restart_attempts": map[string]daemon.RecoveryAttemptStatus{"myapp.service": {AttemptsUsed: 1, MaxAttempts: "3", LastResult: "restart requested"}},
		"recovery_command_attempts": map[string]daemon.RecoveryAttemptStatus{"root_disk": {AttemptsUsed: 1, MaxAttempts: "2", LastResult: "exit 0"}},
		"maintenance":               map[string]checksandalerts.Window{"myapp": {Until: time.Date(2026, 9, 30, 15, 2, 0, 0, time.UTC), User: "matt", Reason: "fixing the migration by hand"}},
	})
	if err := printStatus(b, "", false); err != nil {
		t.Fatal(err)
	}
	file.Seek(0, 0)
	output, _ := io.ReadAll(file)
	for _, text := range []string{"Systemd unit restarts myapp.service: 1 of 3 attempts used; restart requested", "Recovery command root_disk: 1 of 2 attempts used; exit 0", "Maintenance on: myapp until 2026-09-30 15:02 UTC, turned on by matt: fixing the migration by hand"} {
		if !strings.Contains(string(output), text) {
			t.Fatal(string(output))
		}
	}
}

// Only Ctrl-C forces exit 130. SIGTERM still cancels the command normally.
func TestCommandSignalExit(t *testing.T) {
	if socket := os.Getenv("MMWATCHDOG_SIGNAL_TEST_SOCKET"); socket != "" {
		os.Exit(errorCode(run([]string{"status", "--socket", socket})))
	}
	for _, tc := range []struct {
		signal os.Signal
		code   int
	}{{os.Interrupt, 130}, {syscall.SIGTERM, 4}} {
		t.Run(tc.signal.String(), func(t *testing.T) {
			directory, err := os.MkdirTemp("/tmp", "mmwatchdog-signals-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(directory)
			socket := filepath.Join(directory, "daemon.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			ready := make(chan struct{})
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(ready)
				<-r.Context().Done()
			})}
			go server.Serve(listener)
			defer server.Close()
			// The command is a new process of the test program, which takes
			// seconds to start on a busy machine.
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCommandSignalExit$")
			command.Env = append(os.Environ(), "MMWATCHDOG_SIGNAL_TEST_SOCKET="+socket)
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			defer command.Process.Kill()
			select {
			case <-ready:
			case <-ctx.Done():
				t.Fatal("command did not reach daemon", ctx.Err())
			}
			if err := command.Process.Signal(tc.signal); err != nil {
				t.Fatal(err)
			}
			err = command.Wait()
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != tc.code {
				t.Fatalf("got %v; want exit %d", err, tc.code)
			}
		})
	}
}

// mmwatchdog status shows where the next MMHotline request goes, where one last
// worked, and each problem in plain words. The daemon names the JSON fields
// with mmwatchdog.MMHotlineStatus, the type the printer reads, and
// internal/daemon's TestMMHotlineOnAStandaloneNode checks those names in the
// daemon's own status.
func TestStatusShowsMMHotline(t *testing.T) {
	seconds, worked := int64(-420), time.Date(2026, 10, 1, 14, 2, 0, 0, time.UTC)
	status := map[string]any{"summary": map[string]any{"exit_code": 0, "attention": []any{}}, "checks": map[string]any{}, "mmhotline_destinations": []fleet.MMHotlineStatus{{
		Destination: "on_call", URL: "https://hotline.example.com", AlertSourceID: "mmwatchdog_fleet_hub", NodeClockAheadSeconds: &seconds, LastSignedAnswerAt: &worked,
		DeliveryProblems: []string{"MMHotline gave no signed answer (HTTP 502). Trying again at 14:05 UTC."},
		ClockWarning:     "This node's clock is 7 minutes behind MMHotline's. Fix time sync (for example systemd-timesyncd or chrony).",
	}, {Destination: "other_team", URL: "http://other.example.com", DeliveryProblems: []string{}}}}
	body, _ := json.Marshal(status)
	output, err := capture(t, func() error { return printStatus(body, "", false) })
	for _, want := range []string{"MMHotline on_call: https://hotline.example.com last gave a signed answer at 14:02 UTC", "MMHotline on_call: MMHotline gave no signed answer (HTTP 502). Trying again at 14:05 UTC.", "MMHotline on_call: This node's clock is 7 minutes behind MMHotline's.", "MMHotline other_team: http://other.example.com has not given a signed answer yet"} {
		if err != nil || !strings.Contains(output, want) {
			t.Fatal(output, err)
		}
	}
	output, err = capture(t, func() error { return printStatus(body, "", true) })
	if err != nil || !strings.Contains(output, `"node_clock_ahead_of_mmhotline_seconds": -420`) || !strings.Contains(output, `"mmhotline_destinations": [`) || !strings.Contains(output, `"delivery_problems"`) {
		t.Fatal(output, err)
	}
	status["mmhotline_destinations"] = []fleet.MMHotlineStatus{}
	body, _ = json.Marshal(status)
	output, err = capture(t, func() error { return printStatus(body, "", true) })
	if err != nil || !strings.Contains(output, `"mmhotline_destinations": []`) {
		t.Fatal(output, err)
	}
}

func TestStatusShowsAlertSeverity(t *testing.T) {
	check := incidents.CheckStatus{CheckState: incidents.CheckState{CheckGroup: "myapp", Check: "http",
		AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn", RaiseToSosAfterFailingFor: "30m"},
		FailureIncident:       incidents.Track{Incident: "incident", AlertSeverity: "sos"}}, State: "failed"}
	b, _ := json.Marshal(map[string]any{"summary": map[string]any{"exit_code": 0, "attention": []any{}}, "checks": map[string]any{"myapp/http": check}})
	text, err := capture(t, func() error { return printStatus(b, "myapp", false) })
	want := "  Alert severity: warn\n  Raise to SOS after failing for: 30m\n  Alert severity of the open failure incident: sos\n"
	if err != nil || !strings.Contains(text, want) {
		t.Fatal(text, err)
	}
	// The check that watches a node for silence has the node's
	// raise_to_sos_after_this_node_is_silent_for and its
	// raise_to_sos_when_every_node_of_these_node_groups_is_silent.
	silence := incidents.CheckStatus{CheckState: incidents.CheckState{CheckGroup: "node_reports", Check: "app-2",
		AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn", RaiseToSosAfterFailingFor: "10m"}}, State: "ok",
		RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent: []string{"jobs_servers", "frankfurt"}}
	b, _ = json.Marshal(map[string]any{"summary": map[string]any{"exit_code": 0, "attention": []any{}}, "checks": map[string]any{"node_reports/app-2": silence}})
	text, err = capture(t, func() error { return printStatus(b, "node_reports", false) })
	if err != nil || !strings.Contains(text, "  Alert severity: warn\n  Raise to SOS after no report for: 10m\n  Raise to SOS when every node of these node groups is silent: jobs_servers, frankfurt\n") || strings.Contains(text, "Raise to SOS after failing for") {
		t.Fatal(text, err)
	}
	text, err = capture(t, func() error { return printStatus(b, "node_reports", true) })
	if err != nil || !strings.Contains(text, `"raise_to_sos_when_every_node_of_these_node_groups_is_silent": [`) {
		t.Fatal(text, err)
	}
}

// mmwatchdog status says how far back the measurement history reaches when its size
// limit prevented keeping the requested keep_measurement_history_for, and says nothing while
// the history reaches as far back as the node keeps readings. The JSON status
// carries the times of the oldest and the newest reading and both settings.
func TestStatusShowsAMeasurementHistoryThatTheSizeLimitCutShort(t *testing.T) {
	oldest := time.Now().UTC().Add(-(5*24*time.Hour + 3*time.Hour + 10*time.Minute))
	newest := time.Now().UTC()
	history := store.MeasurementHistoryStatus{OldestReadingAt: &oldest, NewestReadingAt: &newest, KeepMeasurementHistoryFor: "7d", MeasurementHistorySizeLimit: "256MiB", CutShortByMeasurementHistorySizeLimit: true}
	status := func() []byte {
		b, _ := json.Marshal(map[string]any{"summary": map[string]any{"exit_code": 0, "attention": []any{}}, "checks": map[string]any{}, "measurement_history": history})
		return b
	}
	text, err := capture(t, func() error { return printStatus(status(), "", false) })
	if err != nil || !strings.Contains(text, "\nmeasurement history reaches back 5 days 3 hours; the size limit of 256 MiB prevented keeping the requested 7 days\n") {
		t.Fatal(text, err)
	}
	out, err := capture(t, func() error { return printStatus(status(), "", true) })
	for _, want := range []string{`"measurement_history": {`, `"oldest_reading_at": "`, `"newest_reading_at": "`, `"keep_measurement_history_for": "7d"`, `"measurement_history_size_limit": "256MiB"`, `"cut_short_by_measurement_history_size_limit": true`} {
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q (%v)", want, out, err)
		}
	}
	history.CutShortByMeasurementHistorySizeLimit = false
	if text, err = capture(t, func() error { return printStatus(status(), "", false) }); err != nil || strings.Contains(text, "measurement history") {
		t.Fatal(text, err)
	}
	// A node that has stored no reading yet has no times to show.
	history = store.MeasurementHistoryStatus{KeepMeasurementHistoryFor: "7d", MeasurementHistorySizeLimit: "256MiB"}
	if out, err = capture(t, func() error { return printStatus(status(), "", true) }); err != nil || strings.Contains(out, "oldest_reading_at") || !strings.Contains(out, `"cut_short_by_measurement_history_size_limit": false`) {
		t.Fatal(out, err)
	}
}

// mmwatchdog status says in a full sentence when MMWatchdog keeps a check quiet that fails,
// that it cannot check, or whose recovery alert waits, because a check that
// it names is failing, or was failing until the quiet check's next run. mmwatchdog
// status CHECK_GROUP shows the checks that a check names and the checks that
// name it, and the JSON status carries all of it.
// The status command reads the check's timing and failing time from the
// daemon's JSON to say what a confirming check with a healthy latest run
// waits for.
func TestStatusSaysWhatAConfirmingCheckWaitsFor(t *testing.T) {
	since := time.Date(2026, 10, 28, 10, 41, 7, 0, time.UTC)
	timing := checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "10s", AlertAfterFailingFor: "2m", RecoveredAfterHealthyFor: "1m"}
	answers := incidents.CheckStatus{CheckState: incidents.CheckState{CheckGroup: "pgbouncer", Check: "pgbouncer_answers", Timing: timing, FailingSince: since, HealthySince: since.Add(time.Minute),
		Last: checksandalerts.Observation{Status: "ok", Message: "command exited 0"}}, State: "confirming_failure"}
	b, _ := json.Marshal(map[string]any{"summary": map[string]any{"exit_code": 0, "attention": []any{}}, "checks": map[string]any{"pgbouncer/pgbouncer_answers": answers}})
	text, err := capture(t, func() error { return printStatus(b, "", false) })
	want := "pgbouncer/pgbouncer_answers  confirming_failure the latest run is healthy, but runs have failed since 10:41:07 UTC; the check counts as failing until it has been healthy for 1m (recovered_after_healthy_for), so that a check that fails on and off still alerts: command exited 0\n"
	if err != nil || !strings.Contains(text, want) {
		t.Fatalf("missing %q in %q (%v)", want, text, err)
	}
}

func TestStatusShowsAQuietCheck(t *testing.T) {
	since := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	names := []string{"myapp/unit", "postgres/running"}
	failing := incidents.CheckStatus{CheckState: incidents.CheckState{CheckGroup: "myapp", Check: "http", FailingSince: since, QuietWhileTheseChecksFail: names, QuietNowWhileCheckFails: "postgres/running"}, State: "confirming_failure"}
	unknown := incidents.CheckStatus{CheckState: incidents.CheckState{CheckGroup: "myapp", Check: "queue", UnknownSince: since, QuietWhileTheseChecksFail: names, QuietNowWhileCheckFails: "postgres/running"}, State: "unknown"}
	healthy := incidents.CheckStatus{CheckState: incidents.CheckState{CheckGroup: "myapp", Check: "disk", QuietWhileTheseChecksFail: names, QuietNowWhileCheckFails: "postgres/running"}, State: "ok"}
	okAgain := incidents.CheckStatus{CheckState: incidents.CheckState{CheckGroup: "myapp", Check: "login", QuietNowWhileCheckFails: "postgres/running", FailureIncident: incidents.Track{Announced: "incident"}}, State: "ok"}
	untilNextRun := incidents.CheckStatus{CheckState: incidents.CheckState{CheckGroup: "shop", Check: "http", FailingSince: since}, QuietUntilNextRunAfterCheckFailed: "cache/running", State: "failed"}
	cause := incidents.CheckStatus{CheckState: incidents.CheckState{CheckGroup: "postgres", Check: "running", FailingSince: since, KeepsTheseChecksQuiet: []string{"myapp/disk", "myapp/http", "myapp/queue"}}, State: "confirming_failure"}
	b, _ := json.Marshal(map[string]any{"summary": map[string]any{"exit_code": 0, "attention": []any{}},
		"checks": map[string]any{"myapp/http": failing, "myapp/queue": unknown, "myapp/disk": healthy, "myapp/login": okAgain, "shop/http": untilNextRun, "postgres/running": cause}})
	text, err := capture(t, func() error { return printStatus(b, "", false) })
	want := "myapp/http is failing; no alert and no recovery action while postgres/running fails\n" +
		"myapp/login is ok again; no recovery alert while postgres/running fails\n" +
		"MMWatchdog cannot check myapp/queue; no alert while postgres/running fails\n" +
		"shop/http is failing; no alert and no recovery action until its next run, because cache/running was failing\nAlert deliveries:"
	if err != nil || !strings.Contains(text, want) || strings.Contains(text, "myapp/disk is") {
		t.Fatal(text, err)
	}
	text, err = capture(t, func() error { return printStatus(b, "myapp", false) })
	for _, want := range []string{
		"  No alerts or recovery actions while these checks fail: myapp/unit, postgres/running\n",
		"  While this check fails, no alerts or recovery actions for: myapp/disk, myapp/http, myapp/queue\n",
	} {
		if err != nil || !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q (%v)", want, text, err)
		}
	}
	out, err := capture(t, func() error { return printStatus(b, "", true) })
	for _, want := range []string{
		`"no_alerts_or_recovery_actions_while_these_checks_fail": [`,
		`"no_alerts_or_recovery_actions_now_while_check_fails": "postgres/running"`,
		`"no_alerts_or_recovery_actions_until_next_run_because_check_was_failing": "cache/running"`,
		`"no_alerts_or_recovery_actions_for_these_checks_while_this_check_fails": [`,
	} {
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q (%v)", want, out, err)
		}
	}
}

// mmwatchdog validate names every filesystem that has its own node_filesystem check
// with a limit and is missing from the filesystems_to_ignore of an
// every_disk_and_network_filesystem check.
func TestFilesystemsMissingFromFilesystemsToIgnoreOnTheCommandLine(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "check_groups"), 0700)
	os.WriteFile(filepath.Join(dir, "mmwatchdog_node_settings.yml"), []byte(nodeSettingsOfATest(dir)), 0600)
	os.WriteFile(filepath.Join(dir, "alert_destinations.yml"), []byte("destinations: {}\n"), 0600)
	checks := "check_groups:\n  filesystems:\n    check_every: 1m\n    time_limit_per_run: 10s\n    alert_after_failing_for: 10m\n    recovered_after_healthy_for: 5m\n    checks:\n" +
		"      every_filesystem_is_below_90_percent: {every_disk_and_network_filesystem: {filesystems_to_ignore: [/boot/efi]}, fail_when_measurement_above: {space_used_percent: 90}, alert_severity: warn}\n" +
		"      root_filesystem_is_below_95_percent: {node_filesystem: /, fail_when_measurement_above: {space_used_percent: 95}, alert_severity: sos}\n"
	os.WriteFile(filepath.Join(dir, "check_groups", "checks.yml"), []byte(checks), 0600)
	output, err := capture(t, func() error { return run([]string{"validate", "--config-dir", dir}) })
	want := "Valid configuration: 1 check group, 0 alert destinations\nThis node sends alerts to nobody.\nfilesystems/every_filesystem_is_below_90_percent: no destination receives alerts with alert_severity warn; add the severity to a destination's alert_severities, or change the check's send_alerts_only_to_destinations.\nfilesystems/root_filesystem_is_below_95_percent: no destination receives alerts with alert_severity sos; add the severity to a destination's alert_severities, or change the check's send_alerts_only_to_destinations.\n" +
		"An every_disk_and_network_filesystem check alerts for every filesystem beyond the check's limits and outside the check's filesystems_to_ignore list. The filesystems listed below also have a node_filesystem check with a limit, so a full filesystem would send two alerts. Add each listed filesystem to filesystems_to_ignore:\n" +
		"  filesystems/every_filesystem_is_below_90_percent: / is missing from filesystems_to_ignore; filesystems/root_filesystem_is_below_95_percent also checks the filesystem\n"
	if err != nil || output != want {
		t.Fatalf("%q %v", output, err)
	}
	output, err = capture(t, func() error { return run([]string{"validate", "--json", "--config-dir", dir}) })
	var result struct {
		Missing []checksandalerts.FilesystemMissingFromFilesystemsToIgnore `json:"filesystems_missing_from_filesystems_to_ignore"`
	}
	if err == nil {
		err = json.Unmarshal([]byte(output), &result)
	}
	if err != nil || len(result.Missing) != 1 || result.Missing[0].Filesystem != "/" || result.Missing[0].NodeFilesystemCheck != "filesystems/root_filesystem_is_below_95_percent" {
		t.Fatal(result, err)
	}
	// The filesystem is in the list: no warning.
	os.WriteFile(filepath.Join(dir, "check_groups", "checks.yml"), []byte(strings.Replace(checks, "[/boot/efi]", "[/boot/efi, /]", 1)), 0600)
	if output, err = capture(t, func() error { return run([]string{"validate", "--config-dir", dir}) }); err != nil || strings.Contains(output, "filesystems_to_ignore") {
		t.Fatalf("%q %v", output, err)
	}
}

// mmwatchdog validate names every file of a credential that other accounts of the
// machine can use, with the setting that names the file.
func TestSecretFilePermissionsOnTheCommandLine(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	writeFiles(t, dir, map[string]string{
		"mmwatchdog_node_settings.yml": nodeSettingsOfATest(dir),
		"check_groups/checks.yml":      "check_groups: {example: {" + timingsOfACheckGroup + ", http: {url: 'https://example.com'}, alert_severity: warn}}",
		"alert_destinations.yml":       "destinations: {example: {webhook: {url: 'https://example.com', bearer_token_file: '" + token + "'}, remind_every: never, alert_severities: [warn]}}",
		"token":                        "x",
	})
	os.Chmod(token, 0644)
	if _, err := capture(t, func() error { return run([]string{"validate", "--config-dir", dir}) }); err == nil || !strings.Contains(err.Error(), "dangerously_let_every_account_read_secret: true") {
		t.Fatal(err)
	}
	if _, err := capture(t, func() error { return run([]string{"validate", "--config-only", "--config-dir", dir}) }); err != nil {
		t.Fatal(err)
	}
	os.Chmod(token, 0600)
	if output, err := capture(t, func() error { return run([]string{"validate", "--json", "--config-dir", dir}) }); err != nil {
		t.Fatal(output, err)
	}
}

// mmwatchdog validate names every systemd unit that has its own systemd_unit_state
// check and is missing from an every_systemd_unit check's
// systemd_units_to_ignore. mmwatchdog status and mmwatchdog fleet NODE show that check with
// one line for each failed unit.
func TestFailedSystemdUnitsOnTheCommandLine(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "check_groups"), 0700)
	os.WriteFile(filepath.Join(dir, "mmwatchdog_node_settings.yml"), []byte(nodeSettingsOfATest(dir)), 0600)
	os.WriteFile(filepath.Join(dir, "alert_destinations.yml"), []byte("destinations: {}\n"), 0600)
	timings := "    check_every: 1m\n    time_limit_per_run: 5s\n    alert_after_failing_for: 2m\n    recovered_after_healthy_for: 1m\n"
	checks := "check_groups:\n  failed_systemd_units:\n" + timings + "    checks:\n      machine_has_no_failed_systemd_unit: {every_systemd_unit: {fail_when: a_systemd_unit_has_failed, systemd_units_to_ignore: [cron.service]}, alert_severity: warn}\n" +
		"  myapp:\n" + timings + "    checks:\n      myapp_service_is_running: {systemd_unit_state: {systemd_unit: myapp.service, fail_when: the_unit_is_not_active}, alert_severity: sos}\n"
	os.WriteFile(filepath.Join(dir, "check_groups", "checks.yml"), []byte(checks), 0600)
	output, err := capture(t, func() error { return run([]string{"validate", "--config-dir", dir}) })
	want := "Valid configuration: 2 check groups, 0 alert destinations\nThis node sends alerts to nobody.\nfailed_systemd_units/machine_has_no_failed_systemd_unit: no destination receives alerts with alert_severity warn; add the severity to a destination's alert_severities, or change the check's send_alerts_only_to_destinations.\nmyapp/myapp_service_is_running: no destination receives alerts with alert_severity sos; add the severity to a destination's alert_severities, or change the check's send_alerts_only_to_destinations.\n" +
		"An every_systemd_unit check alerts for every failed systemd unit outside the check's systemd_units_to_ignore list. The units listed below also have a systemd_unit_state check, so a unit failure would send two alerts. Add each listed unit to systemd_units_to_ignore:\n" +
		"  failed_systemd_units/machine_has_no_failed_systemd_unit: myapp.service is missing from systemd_units_to_ignore; myapp/myapp_service_is_running also checks the systemd unit\n"
	if err != nil || output != want {
		t.Fatalf("%q %v", output, err)
	}
	output, err = capture(t, func() error { return run([]string{"validate", "--json", "--config-dir", dir}) })
	var result struct {
		Missing []checksandalerts.SystemdUnitMissingFromUnitsToIgnore `json:"systemd_units_missing_from_systemd_units_to_ignore"`
	}
	if err == nil {
		err = json.Unmarshal([]byte(output), &result)
	}
	if err != nil || len(result.Missing) != 1 || result.Missing[0].SystemdUnit != "myapp.service" || result.Missing[0].SystemdUnitStateCheck != "myapp/myapp_service_is_running" {
		t.Fatal(result, err)
	}
	// The unit is in the list: no warning.
	os.WriteFile(filepath.Join(dir, "check_groups", "checks.yml"), []byte(strings.Replace(checks, "[cron.service]", "[cron.service, myapp.service]", 1)), 0600)
	if output, err = capture(t, func() error { return run([]string{"validate", "--config-dir", dir}) }); err != nil || strings.Contains(output, "systemd_units_to_ignore") {
		t.Fatalf("%q %v", output, err)
	}

	message := "systemd unit backup.service failed (systemd result: exit-code)"
	unit := map[string]any{"check_group": "failed_systemd_units", "check": "machine_has_no_failed_systemd_unit", "finding": "backup.service", "state": "failed",
		"last_observation": map[string]any{"status": "failed", "message": message},
		"failure_incident": map[string]any{"incident_id": "incident-of-backup", "alert_severity": "warn", "evidence": map[string]any{"incident_id": "incident-of-backup", "observation": map[string]any{"status": "failed", "message": message}}}}
	confirming := map[string]any{"finding": "certbot.service", "state": "confirming_failure", "last_observation": map[string]any{"status": "failed", "message": "systemd unit certbot.service failed (systemd result: signal)"}}
	check := map[string]any{"check_group": "failed_systemd_units", "check": "machine_has_no_failed_systemd_unit", "state": "failed", "one_incident_for_each_finding": true,
		"last_observation": map[string]any{"status": "failed", "message": "2 systemd units failed: backup.service, certbot.service"},
		"findings":         map[string]any{"backup.service": unit, "certbot.service": confirming}}
	key := "failed_systemd_units/machine_has_no_failed_systemd_unit"
	status, _ := json.Marshal(map[string]any{"summary": map[string]any{"exit_code": 1, "attention": []any{}}, "checks": map[string]any{key: check}})
	output, err = capture(t, func() error { return printStatus(status, "failed_systemd_units", false) })
	for _, want := range []string{
		key + " failed       2 systemd units failed: backup.service, certbot.service\n",
		"  backup.service             failed       " + message + "\n",
		"  certbot.service            confirming_failure systemd unit certbot.service failed (systemd result: signal)\n",
		"  Open failure incident of backup.service, alert severity warn:\n  Confirmed incident: incident-of-backup\n",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q in %q (%v)", want, output, err)
		}
	}
	// The JSON keeps each unit with its own state.
	output, _ = capture(t, func() error { return printStatus(status, "", true) })
	var printed struct {
		Checks map[string]struct {
			Findings map[string]struct {
				State string `json:"state"`
			} `json:"findings"`
		} `json:"checks"`
	}
	if err = json.Unmarshal([]byte(output), &printed); err != nil || printed.Checks[key].Findings["certbot.service"].State != "confirming_failure" {
		t.Fatalf("%q %v", output, err)
	}
	node, _ := json.Marshal(fleet.FleetNode{Name: "app-2", State: "reporting", ExitCode: 1, NodeStatusReport: &fleet.NodeStatusReport{Checks: map[string]fleet.ReportedCheck{key: {State: "failed", Message: "1 systemd unit failed: backup.service",
		Findings: map[string]fleet.ReportedCheck{"backup.service": {State: "failed", Message: message, FailureIncident: &incidents.Track{Incident: "incident-of-backup", AlertSeverity: "warn"}}}}}},
		Events: []checksandalerts.Event{{CheckGroup: "failed_systemd_units", Check: "machine_has_no_failed_systemd_unit", Finding: "backup.service", Type: "failed", Message: message}}})
	output, _ = capture(t, func() error { return printNode(node, false, time.Now()) })
	for _, want := range []string{
		"  backup.service             failed       " + message + "\n",
		"  Open failure incident of backup.service, alert severity warn:\n",
		" failed               " + key + ": backup.service " + message + "\n",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q in %q", want, output)
		}
	}
}

// mmwatchdog validate names every check that names a check with a lower
// alert_severity in no_alerts_or_recovery_actions_while_these_checks_fail,
// and mmwatchdog reload prints the validation warnings of the configuration that the
// daemon activated.
func TestValidationWarningsOnTheCommandLine(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "check_groups"), 0700)
	os.WriteFile(filepath.Join(dir, "mmwatchdog_node_settings.yml"), []byte(nodeSettingsOfATest(dir)), 0600)
	os.WriteFile(filepath.Join(dir, "alert_destinations.yml"), []byte("destinations: {}\n"), 0600)
	timings := "    check_every: 1m\n    time_limit_per_run: 5s\n    alert_after_failing_for: 2m\n    recovered_after_healthy_for: 1m\n"
	checks := "check_groups:\n  postgres:\n" + timings + "    checks:\n      postgres_service_is_running: {systemd_unit_state: {systemd_unit: postgresql.service, fail_when: the_unit_is_not_active}, alert_severity: warn}\n" +
		"  myapp:\n" + timings + "    checks:\n      myapp_answers_health_request: {tcp: '127.0.0.1:8080', alert_severity: sos, no_alerts_or_recovery_actions_while_these_checks_fail: [postgres/postgres_service_is_running]}\n"
	os.WriteFile(filepath.Join(dir, "check_groups", "checks.yml"), []byte(checks), 0600)
	warning := "While a check named in another check's no_alerts_or_recovery_actions_while_these_checks_fail fails, MMWatchdog sends one alert, for the named check and with the named check's alert_severity. Each check listed below names a check that has a lower alert_severity than the listed check. So while the named check fails, a destination that takes only the listed check's higher alert_severity gets no alert. Give the named check the alert_severity that the named check's failure deserves:\n" +
		"  myapp/myapp_answers_health_request has alert_severity sos and names postgres/postgres_service_is_running, which has alert_severity warn\n"
	output, err := capture(t, func() error { return run([]string{"validate", "--config-dir", dir}) })
	if want := "Valid configuration: 2 check groups, 0 alert destinations\nThis node sends alerts to nobody.\nmyapp/myapp_answers_health_request: no destination receives alerts with alert_severity sos; add the severity to a destination's alert_severities, or change the check's send_alerts_only_to_destinations.\npostgres/postgres_service_is_running: no destination receives alerts with alert_severity warn; add the severity to a destination's alert_severities, or change the check's send_alerts_only_to_destinations.\n" + warning; err != nil || output != want {
		t.Fatalf("%q %v", output, err)
	}
	output, err = capture(t, func() error { return run([]string{"validate", "--json", "--config-dir", dir}) })
	var result struct {
		Valid bool                                                 `json:"valid"`
		Lower []config.CheckThatNamesACheckWithALowerAlertSeverity `json:"checks_that_name_a_check_with_a_lower_alert_severity"`
	}
	if err == nil {
		err = json.Unmarshal([]byte(output), &result)
	}
	if err != nil || !result.Valid || len(result.Lower) != 1 || result.Lower[0].NamedCheck != "postgres/postgres_service_is_running" || result.Lower[0].NamedCheckAlertSeverity != "warn" || result.Lower[0].AlertSeverity != "sos" {
		t.Fatal(output, err)
	}
	// The named check has the same severity: no warning.
	os.WriteFile(filepath.Join(dir, "check_groups", "checks.yml"), []byte(strings.Replace(checks, "alert_severity: warn", "alert_severity: sos", 1)), 0600)
	if output, err = capture(t, func() error { return run([]string{"validate", "--config-dir", dir}) }); err != nil || strings.Contains(output, "lower") {
		t.Fatalf("%q %v", output, err)
	}

	socket := filepath.Join(os.TempDir(), "mmwatchdog-cli-"+incidents.ID()+".sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(socket)
	reply := daemon.ReloadResult{Reloaded: true}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/reload" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(reply)
	}))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	if output, err = capture(t, func() error { return run([]string{"reload", "--socket", socket}) }); err != nil || output != "The configuration is active.\n" {
		t.Fatalf("%q %v", output, err)
	}
	reply.ChecksThatNameACheckWithALowerAlertSeverity = result.Lower
	reply.SystemdUnitsMissingFromSystemdUnitsToIgnore = []checksandalerts.SystemdUnitMissingFromUnitsToIgnore{{CheckGroup: "failed_systemd_units", Check: "machine_has_no_failed_systemd_unit", SystemdUnit: "myapp.service", SystemdUnitStateCheck: "myapp/myapp_service_is_running"}}
	output, err = capture(t, func() error { return run([]string{"reload", "--socket", socket}) })
	if err != nil || !strings.HasPrefix(output, "The configuration is active.\nAn every_systemd_unit check alerts for every failed systemd unit") || !strings.HasSuffix(output, "myapp/myapp_service_is_running also checks the systemd unit\n"+warning) {
		t.Fatalf("%q %v", output, err)
	}
	reply.NewOrChangedDestinations = []string{"team_chat"}
	reply.DestinationVerifications = []daemon.DestinationVerificationResult{{Destination: "team_chat", DestinationVerification: checksandalerts.DestinationVerification{CanVerifyWithoutMessage: true, VerificationMessage: "credential refused"}}}
	output, err = capture(t, func() error { return run([]string{"reload", "--socket", socket}) })
	command := `sudo mmwatchdog send-notice --alert-severity info --title "Test of the destination team_chat. Nothing is wrong." --destination team_chat`
	if errorCode(err) != 1 || !strings.Contains(output, "The configuration is active.") || !strings.Contains(output, "Destination team_chat: failed: credential refused") || !strings.Contains(output, command) {
		t.Fatal(output, err)
	}

	for _, accepted := range []bool{false, true} {
		reply.DestinationVerifications[0].AcceptsAddressAndCredential = accepted
		output, err = capture(t, func() error { return run([]string{"reload", "--json", "--socket", socket}) })
		var object map[string]any
		expectedCode := 1
		if accepted {
			expectedCode = 0
		}
		if errorCode(err) != expectedCode || json.Unmarshal([]byte(output), &object) != nil || !reflect.DeepEqual(object["new_or_changed_destinations"], []any{"team_chat"}) {
			t.Fatal(output, err)
		}
	}

}

// mmwatchdog status prints one line for each failing destination, after the line of
// the notification queue, and mmwatchdog fleet NODE prints the same lines from the
// node's report. The latest delivery error is printed on its own only when
// no destination's line shows it.
func TestStatusShowsFailingDestinations(t *testing.T) {
	at := time.Date(2026, 9, 29, 14, 5, 30, 0, time.UTC)
	delivery := store.DeliverySummary{Pending: 4, LastError: "the module slack reported: Slack answered 404: the webhook does not exist", FailingDestinations: []store.FailingDestination{
		{Destination: "on_call_mailbox", UndeliveredAlerts: 1, LatestAttemptError: "the mail server smtp.example.com:587 cannot be reached: dial tcp 192.0.2.10:587: i/o timeout", NextAttemptAt: at},
		{Destination: "team_chat", UndeliveredAlerts: 3, LatestAttemptError: "the module slack reported: Slack answered 404: the webhook does not exist", NextAttemptAt: at.Add(time.Minute)},
	}}
	status, _ := json.Marshal(map[string]any{"summary": map[string]any{"exit_code": 0, "attention": []any{}}, "checks": map[string]any{}, "delivery": delivery})
	text, err := capture(t, func() error { return printStatus(status, "", false) })
	want := "Alert deliveries: 4 waiting, 0 dropped\n" +
		"Destination on_call_mailbox: 1 alert is not delivered. MMWatchdog tries again at 14:05 UTC. The latest attempt failed: the mail server smtp.example.com:587 cannot be reached: dial tcp 192.0.2.10:587: i/o timeout\n" +
		"Destination team_chat: 3 alerts are not delivered. MMWatchdog tries again at 14:06 UTC. The latest attempt failed: the module slack reported: Slack answered 404: the webhook does not exist\n"
	if err != nil || !strings.Contains(text, want) {
		t.Fatalf("%v\n got %s\nwant %s", err, text, want)
	}
	out, err := capture(t, func() error { return printStatus(status, "", true) })
	for _, field := range []string{`"failing_destinations": [`, `"destination": "team_chat"`, `"undelivered_alerts": 3`, `"latest_attempt_error": "the module slack reported: Slack answered 404: the webhook does not exist"`, `"next_attempt_at": "2026-09-29T14:06:30Z"`} {
		if err != nil || !strings.Contains(out, field) {
			t.Fatalf("status JSON lacks %s: %s (%v)", field, out, err)
		}
	}
	healthy, _ := json.Marshal(map[string]any{"summary": map[string]any{"exit_code": 0, "attention": []any{}}, "checks": map[string]any{}, "delivery": store.DeliverySummary{FailingDestinations: []store.FailingDestination{}}})
	if text, _ = capture(t, func() error { return printStatus(healthy, "", false) }); !strings.Contains(text, "Alert deliveries: 0 waiting, 0 dropped\n") || strings.Contains(text, "Destination ") {
		t.Fatal(text)
	}

	now := time.Now().UTC()
	node := func(delivery store.DeliverySummary) []byte {
		b, _ := json.Marshal(fleet.FleetNode{Name: "app-1", State: "reporting", LastReport: now, NodeStatusReport: &fleet.NodeStatusReport{Delivery: delivery}})
		return b
	}
	text, _ = capture(t, func() error { return printNode(node(delivery), false, now) })
	if !strings.Contains(text, "  Alert deliveries: 4 waiting, 0 dropped\n  Destination on_call_mailbox: 1 alert is not delivered. MMWatchdog tries again at 14:05 UTC.") ||
		!strings.Contains(text, "\n  Destination team_chat: 3 alerts are not delivered.") || strings.Contains(text, "Last delivery error:") {
		t.Fatal(text)
	}
	// The latest error is the mmhotline destination's, which has no line.
	delivery.LastError = "MMHotline gave no signed answer (HTTP 502)."
	if text, _ = capture(t, func() error { return printNode(node(delivery), false, now) }); !strings.Contains(text, "\n  Last delivery error: MMHotline gave no signed answer (HTTP 502).\n") {
		t.Fatal(text)
	}
	if text, _ = capture(t, func() error { return printNode(node(store.DeliverySummary{}), false, now) }); strings.Contains(text, "Last delivery error:") || strings.Contains(text, "Destination ") {
		t.Fatal(text)
	}
}

func TestHeartbeatTrustedCertificateAuthorityFlag(t *testing.T) {
	directory := t.TempDir()
	const token = "a-long-heartbeat-test-token"
	tokenPath, authorityPath := filepath.Join(directory, "token"), filepath.Join(directory, "authority.pem")
	received := make(chan string, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/heartbeats/backup" || r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(400)
			return
		}
		received <- r.URL.Path
		w.WriteHeader(204)
	}))
	defer server.Close()
	if err := os.WriteFile(tokenPath, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(authorityPath, certificate, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tokenPath, 0644); err != nil {
		t.Fatal(err)
	}
	warning := filepath.Join(directory, "standard-error")
	stderr, err := os.Create(warning)
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = stderr
	defer func() { os.Stderr = original; stderr.Close() }()
	output, err := capture(t, func() error {
		return run([]string{"heartbeat", "backup", "--receiver-url", server.URL, "--bearer-token-file", tokenPath, "--trusted-certificate-authority-file", authorityPath})
	})
	os.Stderr = original
	stderr.Close()
	logged, readErr := os.ReadFile(warning)
	wantWarning := "warning: " + tokenPath + " has permissions mode 0644: every account and the file's group can read the token; run chmod 600 " + tokenPath + " so that only the account of this job can read it\n"
	if readErr != nil || string(logged) != wantWarning {
		t.Fatalf("%s: %v", logged, readErr)
	}
	if err != nil || output != "Heartbeat accepted: backup\n" {
		t.Fatal(output, err)
	}
	select {
	case <-received:
	default:
		t.Fatal("the receiver got no heartbeat")
	}
}

func TestConfigJSONDoesNotExposeCredentialFingerprint(t *testing.T) {
	destination := checksandalerts.Destination{Inputs: &checksandalerts.Webhook{URL: "https://example.com", BearerTokenFile: "/etc/mmwatchdog/secrets/token"}, RemindEvery: "never", AlertSeverities: []string{"warn"}}
	destination.CaptureCredentialContent(map[string]string{"bearer_token_file": "a-weak-test-password"})
	if destination.CredentialContentFingerprint == "" {
		t.Fatal("the active destination must capture its credential for the comparison")
	}
	socket := filepath.Join(os.TempDir(), "mmwatchdog-cli-"+incidents.ID()+".sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(socket)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/config" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"destinations": map[string]checksandalerts.Destination{"chat": destination}}); err != nil {
			t.Error(err)
		}
	}))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	output, err := capture(t, func() error { return run([]string{"config", "--json", "--socket", socket}) })
	if err != nil || !json.Valid([]byte(output)) || !strings.Contains(output, `"chat"`) || strings.Contains(output, "credential_content_fingerprint") || strings.Contains(output, destination.CredentialContentFingerprint) || strings.Contains(output, "a-weak-test-password") {
		t.Fatalf("configuration exposed a credential or its fingerprint: %s; %v", output, err)
	}
}

func TestConfigOnlyDoesNotReadSavedFleetRouting(t *testing.T) {
	dir := t.TempDir()
	settings := fleet.FleetSettings{Destinations: map[string]checksandalerts.Destination{"team": {Inputs: &checksandalerts.Webhook{URL: "https://example.com/alerts"}, RemindEvery: "never", AlertSeverities: []string{"warn"}}}, FileContentsByDestination: map[string]map[string]string{}, Fleet: config.DefaultFleetTimings, AlertSeverityWhenTheHubGoesSilent: "sos"}
	settings.Version = settings.ComputeVersion()
	saved, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string]string{
		"mmwatchdog_node_settings.yml":     nodeSettingsOfATest(dir) + "state_dir: " + dir + "/state\njoin_fleet_hub: {join_code_file: " + dir + "/missing-join-code}\n",
		"check_groups/app.yml":             "check_groups: {app: {" + timingsOfACheckGroup + ", checks: {cause: {http: {url: 'https://example.com/cause'}, alert_severity: warn, send_alerts_only_to_destinations: []}, quiet: {http: {url: 'https://example.com/quiet'}, alert_severity: warn, no_alerts_or_recovery_actions_while_these_checks_fail: [app/cause]}}}}",
		"state/" + fleet.FleetSettingsFile: string(saved),
	})
	loaded, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.ValidateRouting(loaded); err == nil {
		t.Fatal("saved hub settings must refuse the silent cause during node validation")
	}
	if output, err := capture(t, func() error { return run([]string{"validate", "--config-only", "--config-dir", dir}) }); err != nil || strings.Contains(output, "no destination receives") {
		t.Fatal(output, err)
	}
}

// On a joined node, mmwatchdog maintenance says whether the MMWatchdog fleet
// hub got the change, and exits 1 when it did not, so a playbook does not
// reboot a node whose window the hub does not know.
func TestMaintenanceSaysWhetherTheHubGotTheWindow(t *testing.T) {
	for _, tc := range []struct {
		action     string
		everyGroup bool
		accepted   bool
		hubError   string
		output     string
		code       int
		message    string
	}{
		{"on", true, true, "", "  The MMWatchdog fleet hub has the window, so it sends no alert that this node stopped reporting until the window ends.\n", 0, ""},
		{"on", false, true, "", "  The MMWatchdog fleet hub received the maintenance change.\n", 0, ""},
		{"off", true, true, "", "  The MMWatchdog fleet hub received the maintenance change.\n", 0, ""},
		// A standalone node reports to no hub.
		{"on", true, false, "", "", 0, ""},
		{"on", true, false, "dial tcp 192.0.2.10:8445: connection refused", "", 1, "maintenance is active on the local node, but the MMWatchdog fleet hub did not receive the node's report; the hub does not know about the maintenance window and may alert that the node stopped reporting: dial tcp 192.0.2.10:8445: connection refused"},
		{"off", true, false, "dial tcp 192.0.2.10:8445: connection refused", "", 1, "maintenance has ended on the local node, but the MMWatchdog fleet hub did not receive the node's report; the hub learns that maintenance ended from the node's next report: dial tcp 192.0.2.10:8445: connection refused"},
	} {
		output, err := capture(t, func() error { return maintenanceReportedToTheHub(tc.action, tc.everyGroup, tc.accepted, tc.hubError) })
		if output != tc.output || errorCode(err) != tc.code || (err != nil) != (tc.message != "") || err != nil && err.Error() != tc.message {
			t.Errorf("%+v: output %q, error %v (exit %d)", tc, output, err, errorCode(err))
		}
	}
}

func TestHeartbeatRefusalSaysWhatTheStatusCodeCanMean(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) }))
	defer server.Close()
	directory := t.TempDir()
	tokenPath, authorityPath := filepath.Join(directory, "token"), filepath.Join(directory, "authority.pem")
	if err := os.WriteFile(tokenPath, []byte("a-long-heartbeat-test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(authorityPath, certificate, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := capture(t, func() error {
		return run([]string{"heartbeat", "backup", "--receiver-url", server.URL, "--bearer-token-file", tokenPath, "--trusted-certificate-authority-file", authorityPath})
	})
	want := "the receiver refused the heartbeat (HTTP status code 401): the token does not match, or the receiver has no heartbeat check group named backup"
	if err == nil || err.Error() != want {
		t.Fatalf("%v", err)
	}
}

// A count of one is written as "1 node", "1 check group", and "1 attempt".
func TestCountsOfOneUseTheSingularNoun(t *testing.T) {
	now := time.Now().UTC()
	one, _ := json.Marshal(fleet.Fleet{Address: "util:8445", Nodes: []fleet.FleetNode{{Name: "app-1", State: "reporting", LastReport: now}}})
	output, _ := capture(t, func() error { return printFleet(one, false, now) })
	if !strings.HasPrefix(output, "Fleet at util:8445: 1 node, ") {
		t.Fatalf("fleet count line: %q", output)
	}
	if got := countOf(0, "node") + "|" + countOf(2, "node"); got != "0 nodes|2 nodes" {
		t.Fatal(got)
	}
}

func TestUsageListsReadingsConfigDirAndHidesConfigJSON(t *testing.T) {
	output, err := capture(t, func() error { return run([]string{"--help"}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "mmwatchdog config [--json]") || !strings.Contains(output, "mmwatchdog config\n") {
		t.Error("config always prints JSON, so its usage line has no --json")
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "readings ") && !strings.Contains(line, "[--config-dir DIR]") {
			t.Errorf("readings usage line lacks --config-dir: %s", line)
		}
	}
}

func TestMaintenanceHelpExitsZero(t *testing.T) {
	for _, flagName := range []string{"--help", "-h"} {
		if err := run([]string{"maintenance", flagName}); err != nil || errorCode(err) != 0 {
			t.Errorf("maintenance %s: %v", flagName, err)
		}
	}
}

// socketServer answers every request with the given body on a Unix socket.
func socketServer(t *testing.T, body string) string {
	t.Helper()
	directory, e := os.MkdirTemp("", "mmws")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	path := filepath.Join(directory, "s.sock")
	l, e := net.Listen("unix", path)
	if e != nil {
		t.Fatal(e)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) })}
	go server.Serve(l)
	t.Cleanup(func() { server.Close() })
	return path
}

func TestFleetForgetPrintsJSONWhenAsked(t *testing.T) {
	socket := socketServer(t, "{}")
	output, err := capture(t, func() error { return run([]string{"fleet", "forget", "app-2", "--json", "--socket", socket}) })
	var got map[string]string
	if err != nil || json.Unmarshal([]byte(output), &got) != nil || got["removed_node"] != "app-2" {
		t.Fatal(output, err)
	}
}
