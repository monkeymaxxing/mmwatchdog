package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/testhelpers"
)

// usageModules writes the two module directories of a node that has one
// official extra module, slack, and two modules of the project's own: the
// script queued_jobs and the executable legacy_backup.
func usageModules(t *testing.T) checksandalerts.ModuleDirs {
	t.Helper()
	dirs := checksandalerts.ModuleDirs{OfficialExtra: testhelpers.OfficialExtraModules(t, checksandalerts.Version, "slack"), External: t.TempDir()}
	testhelpers.InstallStarlarkModule(t, dirs.External, "queued_jobs", testhelpers.QueuedJobsDescription, testhelpers.QueuedJobsScript)
	testhelpers.InstallModule(t, dirs.External, "legacy_backup", testhelpers.TestModuleDescription("legacy_backup", checksandalerts.UsedAsCheck, nil, nil), "true")
	return dirs
}

// mmwatchdog describe-module NAME shows one module by its name, whatever its kind:
// a core module with its built-in description, and a module of one of the
// two directories with its kind, the place of its files, and the
// description of its file.
func TestDescribeModule(t *testing.T) {
	dirs := usageModules(t)
	for name, want := range map[string]checksandalerts.ModuleInfo{
		"http":          {Name: "http", Kind: "core", Topic: "Services and protocols", Description: &checksandalerts.HTTPDescription},
		"webhook":       {Name: "webhook", Kind: "core", Topic: "Alert destinations", Description: &checksandalerts.WebhookDescription},
		"slack":         {Name: "slack", Kind: "official_extra", Directory: filepath.Join(dirs.OfficialExtra, "slack"), ShippedWithMMWatchdogVersion: checksandalerts.Version},
		"queued_jobs":   {Name: "queued_jobs", Kind: "project_starlark", Directory: filepath.Join(dirs.External, "queued_jobs")},
		"legacy_backup": {Name: "legacy_backup", Kind: "project_executable", Directory: filepath.Join(dirs.External, "legacy_backup")},
	} {
		got, e := config.DescribeModule(dirs, name)
		if e != nil || got.Description == nil || got.Problem != "" {
			t.Fatal(name, got, e)
		}
		if want.Description == nil {
			want.Description = got.Description
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %+v\nwant %+v", name, got, want)
		}
	}
	queuedJobs, _ := config.DescribeModule(dirs, "queued_jobs")
	if d := queuedJobs.Description; d.Summary != "Counts the jobs that wait in a queue file." || d.UsedAs != "check" || !reflect.DeepEqual(d.Inputs, []checksandalerts.ModuleInput{{Name: "queue_file_path", Type: "string", Required: true, Description: "The absolute path of the file that holds the number of waiting jobs."}}) ||
		!reflect.DeepEqual(d.Measurements, []checksandalerts.ModuleMeasurement{{Name: "queued_jobs", Unit: "count", Description: "The number of jobs that wait."}}) || len(d.Examples) != 1 {
		t.Fatalf("%+v", d)
	}
	// A core module needs no directory, and a name that no module has says
	// where MMWatchdog looked.
	nowhere := checksandalerts.ModuleDirs{OfficialExtra: filepath.Join(t.TempDir(), "official"), External: filepath.Join(t.TempDir(), "own")}
	if got, e := config.DescribeModule(nowhere, "systemd_unit_state"); e != nil || got.Kind != "core" || !reflect.DeepEqual(got.Description.InputNames(), []string{"systemd_unit", "fail_when"}) {
		t.Fatal(got, e)
	}
	if _, e := config.DescribeModule(nowhere, "queued_jobs"); e == nil || e.Error() != "no module is named queued_jobs: MMWatchdog has no core module of that name, and no directory of that name is in "+nowhere.OfficialExtra+", which does not exist or in "+nowhere.External+", which does not exist" {
		t.Fatal(e)
	}
	// A module that MMWatchdog cannot use, and directories that MMWatchdog refuses.
	if e := os.Remove(filepath.Join(dirs.External, "legacy_backup", checksandalerts.ModuleDescriptionFile)); e != nil {
		t.Fatal(e)
	}
	if _, e := config.DescribeModule(dirs, "legacy_backup"); e == nil || !strings.HasPrefix(e.Error(), "the module legacy_backup has no description: ") {
		t.Fatal(e)
	}
	if _, e := config.DescribeModule(checksandalerts.ModuleDirs{OfficialExtra: testhelpers.OfficialExtraModules(t, "0.9.0", "slack")}, "slack"); e == nil || !strings.Contains(e.Error(), "were shipped with MMWatchdog 0.9.0, and this mmwatchdog program is MMWatchdog 0.1.0-dev") {
		t.Fatal(e)
	}
}

// mmwatchdog describe-module without a name lists every module that a node has: the
// core modules, checks first, then the official extra modules, then the
// project's own, each in order of name. A module that MMWatchdog cannot use is
// listed with what is wrong with it.
func TestListModules(t *testing.T) {
	t.Parallel()
	dirs := usageModules(t)
	if e := os.Mkdir(filepath.Join(dirs.External, "unfinished"), 0755); e != nil {
		t.Fatal(e)
	}
	modules, e := config.ListModules(dirs)
	if e != nil {
		t.Fatal(e)
	}
	names, kinds := []string{}, []string{}
	for _, module := range modules {
		names, kinds = append(names, module.Name), append(kinds, module.Kind)
		if (module.Description == nil) != (module.Name == "unfinished") || (module.Problem != "") != (module.Name == "unfinished") {
			t.Errorf("%s: %+v", module.Name, module)
		}
	}
	core := len(checksandalerts.CoreCheckModules) + len(checksandalerts.CoreDestinationModules)
	if !reflect.DeepEqual(names[:len(checksandalerts.CoreCheckModules)], checksandalerts.CheckModuleNames(func(*checksandalerts.CheckModule) bool { return true })) || names[0] != "http" || !reflect.DeepEqual(names[len(checksandalerts.CoreCheckModules):core], checksandalerts.DestinationModuleNames()) || names[core-1] != "mmhotline" ||
		!reflect.DeepEqual(names[core:], []string{"slack", "legacy_backup", "queued_jobs", "unfinished"}) || !reflect.DeepEqual(kinds[core:], []string{"official_extra", "project_executable", "project_starlark", ""}) {
		t.Fatalf("%v\n%v", names, kinds)
	}
	for _, kind := range kinds[:core] {
		if kind != "core" {
			t.Fatal(kinds)
		}
	}
	unfinished := modules[len(modules)-1]
	if unfinished.Directory != filepath.Join(dirs.External, "unfinished") || unfinished.Problem != "the module unfinished has no code: "+unfinished.Directory+" must hold the script module.star or the executable module_executable" {
		t.Fatalf("%+v", unfinished)
	}
	if slack := modules[core]; slack.ShippedWithMMWatchdogVersion != checksandalerts.Version || slack.Directory != filepath.Join(dirs.OfficialExtra, "slack") || slack.Description.UsedAs != "alert_destination" {
		t.Fatalf("%+v", slack)
	}
	// A node without the two directories has the core modules.
	if modules, e = config.ListModules(checksandalerts.ModuleDirs{OfficialExtra: filepath.Join(t.TempDir(), "official"), External: filepath.Join(t.TempDir(), "own")}); e != nil || len(modules) != core {
		t.Fatal(modules, e)
	}
	if _, e = config.ListModules(checksandalerts.ModuleDirs{OfficialExtra: testhelpers.OfficialExtraModules(t, "0.9.0")}); e == nil {
		t.Fatal("MMWatchdog listed the official extra modules of another version")
	}
}

// mmwatchdog module-usage lists the modules that a node's configuration names: for
// each its kind, the place of its files, and every place that calls it, as
// file and line. The core modules come first, then the official extra
// modules, then the project's own, each in order of name.
func TestModuleUsage(t *testing.T) {
	t.Parallel()
	dirs := usageModules(t)
	// The configuration states everything itself, so the lines below are
	// the lines of the file.
	directory := testhelpers.ConfigFileAsWritten(t, `keep_undelivered_alerts_for: 24h
undelivered_alerts_content_size_limit: 8MiB
alert_severity_when_this_node_goes_silent: sos
alert_severity_when_this_node_has_rebooted: info
keep_measurement_history_for: 7d
measurement_history_size_limit: 256MiB
check_groups:
  myapp:
    check_every: 30s
    time_limit_per_run: 5s
    alert_after_failing_for: 1m
    recovered_after_healthy_for: 30s
    checks:
      myapp_service_is_running: {systemd_unit_state: {systemd_unit: myapp.service, fail_when: the_unit_is_not_active}, alert_severity: sos}
      myapp_answers_health_request:
        http: {url: 'http://127.0.0.1:8080/health'}
        alert_severity: sos
      job_queue_is_short:
        queued_jobs:
          queue_file_path: /var/lib/myapp/pending
        fail_when_measurement_above: {queued_jobs: 500}
        alert_severity: warn
  backup: {`+testhelpers.FlowTimings+`, legacy_backup: {}, alert_severity: warn}
  proxy: {`+testhelpers.FlowTimings+`, http: {url: 'http://127.0.0.1/'}, alert_severity: warn}
`)
	workers := filepath.Join(directory, "check_groups", "workers.yml")
	if e := os.WriteFile(workers, []byte("check_groups:\n  workers:\n    check_every: 30s\n    time_limit_per_run: 5s\n    alert_after_failing_for: 1m\n    recovered_after_healthy_for: 30s\n    checks:\n      worker_service_is_running:\n        systemd_unit_state:\n          systemd_unit: worker.service\n          fail_when: the_unit_is_not_active\n        alert_severity: warn\n"), 0600); e != nil {
		t.Fatal(e)
	}
	alerts := filepath.Join(directory, "alert_destinations.yml")
	if e := os.WriteFile(alerts, []byte("destinations:\n  team_chat:\n    remind_every: never\n    slack: {}\n    alert_severities: [info, warn, sos]\n  quiet_chat:\n    remind_every: never\n    slack: {}\n    alert_severities: []\n  ops:\n    remind_every: never\n    webhook: {url: 'https://example.com/mmwatchdog'}\n    alert_severities: [sos]\n"), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := config.LoadWithModuleDirs(directory, dirs)
	if e != nil {
		t.Fatal(e)
	}
	checks := filepath.Join(directory, "check_groups", "checks.yml")
	type use struct {
		name, kind, directory string
		calledAt              []string
	}
	got := []use{}
	for _, module := range c.ModuleUsage() {
		called := []string{}
		for _, at := range module.CalledAt {
			called = append(called, at.File+":"+strconv.Itoa(at.Line))
		}
		if module.Description == nil || module.Problem != "" || (module.ShippedWithMMWatchdogVersion != "") != (module.Kind == "official_extra") {
			t.Errorf("%+v", module)
		}
		got = append(got, use{module.Name, module.Kind, module.Directory, called})
	}
	want := []use{
		{"http", "core", "", []string{checks + ":16", checks + ":24"}},
		{"systemd_unit_state", "core", "", []string{checks + ":14", workers + ":9"}},
		{"webhook", "core", "", []string{alerts + ":12"}},
		{"slack", "official_extra", filepath.Join(dirs.OfficialExtra, "slack"), []string{alerts + ":4", alerts + ":8"}},
		{"legacy_backup", "project_executable", filepath.Join(dirs.External, "legacy_backup"), []string{checks + ":23"}},
		{"queued_jobs", "project_starlark", filepath.Join(dirs.External, "queued_jobs"), []string{checks + ":19"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	// A node that has joined a hub has no destinations of its own, so the
	// list holds the modules of its checks.
	joined := testhelpers.ConfigFile(t, "join_fleet_hub: {join_code_file: /etc/mmwatchdog/secrets/hub-join}\ncheck_groups:\n  backup:\n    legacy_backup: {}\n")
	if c, e = config.LoadWithModuleDirs(joined, dirs); e != nil {
		t.Fatal(e)
	}
	if uses := c.ModuleUsage(); len(uses) != 1 || uses[0].Name != "legacy_backup" || len(uses[0].CalledAt) != 1 || uses[0].CalledAt[0].Line != 4 {
		t.Fatalf("%+v", uses)
	}
}

// The commands that need no whole configuration take the two module
// directories from the node settings, and MMWatchdog's own where the node settings
// state none or cannot be read.
func TestNodeModuleDirs(t *testing.T) {
	t.Parallel()
	defaults := checksandalerts.ModuleDirs{OfficialExtra: "/usr/local/lib/mmwatchdog/official_extra_modules", External: "/usr/local/lib/mmwatchdog/external_modules"}
	directory := t.TempDir()
	if dirs := config.NodeModuleDirs(directory); dirs != defaults {
		t.Fatal("a directory without node settings:", dirs)
	}
	for settings, want := range map[string]checksandalerts.ModuleDirs{
		"official_extra_module_dir: /srv/mmwatchdog/official_extra_modules\nexternal_module_dir: /srv/mmwatchdog/external_modules\n": {OfficialExtra: "/srv/mmwatchdog/official_extra_modules", External: "/srv/mmwatchdog/external_modules"},
		"external_module_dir: /srv/mmwatchdog/external_modules\nalert_severity_when_this_node_goes_silent: sos\n":                    {OfficialExtra: defaults.OfficialExtra, External: "/srv/mmwatchdog/external_modules"},
		"alert_severity_when_this_node_goes_silent: sos\n":                                                                           defaults,
		"external_module_dir: mmwatchdog_external_modules\n":                                                                         defaults,
		"external_module_dir: [unclosed\n":                                                                                           defaults,
	} {
		if e := os.WriteFile(filepath.Join(directory, "mmwatchdog_node_settings.yml"), []byte(settings), 0600); e != nil {
			t.Fatal(e)
		}
		if dirs := config.NodeModuleDirs(directory); dirs != want {
			t.Errorf("%q: %v", settings, dirs)
		}
	}
	// The same two settings and module_state_dir are absolute paths in a
	// configuration that MMWatchdog loads, and state the directories of the node.
	for _, setting := range []string{"official_extra_module_dir", "external_module_dir", "module_state_dir"} {
		if _, e := config.Load(testhelpers.ConfigFile(t, setting+": relative/directory\ncheck_groups: {a: {shell: 'true'}}")); e == nil || !strings.HasSuffix(e.Error(), setting+": expected an absolute path") {
			t.Errorf("%s: %v", setting, e)
		}
	}
	modules, state := t.TempDir(), filepath.Join(t.TempDir(), "module_state")
	c, e := config.Load(testhelpers.ConfigFile(t, "external_module_dir: "+modules+"\nmodule_state_dir: "+state+"\ncheck_groups: {a: {shell: 'true'}}"))
	if e != nil || c.ExternalModuleDir != modules || c.ModuleStateDir != state || c.Modules.Dirs.External != modules {
		t.Fatal(c, e)
	}
	// A node that states none has MMWatchdog's own three directories.
	plain := t.TempDir()
	if e := os.Mkdir(filepath.Join(plain, "check_groups"), 0700); e != nil {
		t.Fatal(e)
	}
	for file, content := range map[string]string{"mmwatchdog_node_settings.yml": "keep_undelivered_alerts_for: 24h\nundelivered_alerts_content_size_limit: 8MiB\nalert_severity_when_this_node_goes_silent: sos\nalert_severity_when_this_node_has_rebooted: info\nkeep_measurement_history_for: 7d\nmeasurement_history_size_limit: 256MiB\n", "alert_destinations.yml": "destinations: {}\n",
		filepath.Join("check_groups", "checks.yml"): "check_groups: {a: {" + testhelpers.FlowTimings + ", shell: 'true', alert_severity: warn}}\n"} {
		if e := os.WriteFile(filepath.Join(plain, file), []byte(content), 0600); e != nil {
			t.Fatal(e)
		}
	}
	// The module directories of the machine that the test runs on are not
	// read here: the test gives its own.
	c, e = config.LoadWithModuleDirs(plain, checksandalerts.ModuleDirs{OfficialExtra: filepath.Join(plain, "official_extra_modules"), External: filepath.Join(plain, "external_modules")})
	if e != nil || c.OfficialExtraModuleDir != defaults.OfficialExtra || c.ExternalModuleDir != defaults.External || c.ModuleStateDir != "/var/lib/mmwatchdog-module-state" {
		t.Fatal(c, e)
	}
}
