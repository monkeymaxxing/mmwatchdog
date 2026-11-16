package testhelpers

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
)

// A script that a check can use, and one that a destination can use.
const (
	ScriptOfACheck       = "def run_check(inputs, file_contents):\n    return {\"status\": \"ok\", \"message\": \"fine\"}\n"
	ScriptOfADestination = "def deliver_alert(alert, inputs, file_contents):\n    return {\"status\": \"delivered\", \"message\": \"sent\"}\n"
)

// OfficialExtraModules writes a directory of official extra modules that
// were shipped with the version given: one Starlark module for each name.
func OfficialExtraModules(t *testing.T, shippedWithVersion string, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	if shippedWithVersion != "" {
		if e := os.WriteFile(filepath.Join(dir, checksandalerts.ShippedWithVersionFile), []byte(shippedWithVersion+"\n"), 0644); e != nil {
			t.Fatal(e)
		}
	}
	for _, name := range names {
		InstallStarlarkModule(t, dir, name, TestModuleDescription(name, checksandalerts.UsedAsAlertDestination, nil, nil), ScriptOfADestination)
	}
	return dir
}

// NeedSourceCheckout skips a test of the repository's files where only the
// compiled test binary runs, such as the native Linux guest.
func NeedSourceCheckout(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("../../README.md"); err != nil {
		t.Skip("needs the source checkout; runs with go test")
	}
}

// TestModuleDescription is the module_description.yml of a test's module
// named name: what the module is used as, its inputs by name with the type
// of each, and for a check its measurements by name with the unit of each.
// No input is required, so the one example calls the module without inputs.
// more are further lines of the description, such as
// "one_incident_for_each_finding: true".
func TestModuleDescription(name, usedAs string, inputs, measurements map[string]string, more ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "summary: A module of a test.\nused_as: %s\n", usedAs)
	if len(inputs) > 0 {
		b.WriteString("inputs:\n")
	}
	for _, input := range checksandalerts.Names(inputs) {
		fmt.Fprintf(&b, "  %s: {type: %s, required: false, description: An input of a test.}\n", input, inputs[input])
	}
	if len(measurements) > 0 {
		b.WriteString("measurements:\n")
	}
	for _, measurement := range checksandalerts.Names(measurements) {
		fmt.Fprintf(&b, "  %s: {unit: %s, description: A measurement of a test.}\n", measurement, measurements[measurement])
	}
	for _, line := range more {
		b.WriteString(line + "\n")
	}
	fmt.Fprintf(&b, "examples:\n  - description: Call the module.\n    config: |\n      %s: {}\n", name)
	return b.String()
}

// InstallModule writes the module name into dir, which stands for a node's
// external_module_dir, as an executable: its description, and script as the
// Bash script module_executable.
func InstallModule(t *testing.T, dir, name, description, script string) {
	t.Helper()
	writeModuleFiles(t, dir, name, description, checksandalerts.ModuleExecutableFile, "#!/bin/bash\n"+script+"\n", 0755)
}

// InstallStarlarkModule writes the module name into dir as a Starlark
// script: its description, and script as module.star.
func InstallStarlarkModule(t *testing.T, dir, name, description, script string) {
	t.Helper()
	writeModuleFiles(t, dir, name, description, checksandalerts.ModuleScriptFile, script, 0644)
}

func writeModuleFiles(t *testing.T, dir, name, description, codeFile, code string, mode os.FileMode) {
	t.Helper()
	module := filepath.Join(dir, name)
	if e := os.MkdirAll(module, 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(module, checksandalerts.ModuleDescriptionFile), []byte(description), 0644); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(module, codeFile)
	if mode&0111 != 0 {
		WriteStandInProgram(t, path, code)
		return
	}
	// A file that was there keeps its mode when it is written again.
	WriteNotExecutableFile(t, path, code)
	if e := os.Chmod(path, mode); e != nil {
		t.Fatal(e)
	}
}

// ModulesIn returns the modules of a node whose external_module_dir is dir
// and that has no official extra modules.
func ModulesIn(t *testing.T, dir string) *checksandalerts.ModuleCatalog {
	t.Helper()
	modules, e := checksandalerts.LoadModuleCatalog(checksandalerts.ModuleDirs{External: dir})
	if e != nil {
		t.Fatal(e)
	}
	return modules
}

// ModuleCheck returns a check that calls the module name of dir with these
// inputs, as the account of the test. The call is validated against the
// module's description, as Load validates it, so the check knows its module.
func ModuleCheck(t *testing.T, dir, name string, inputs map[string]any) checksandalerts.Check {
	t.Helper()
	call := &checksandalerts.ModuleCall{Name: name, Inputs: inputs, ExecOptions: OwnOptions(t)}
	if problem := call.Validate(checksandalerts.InputValidation{UsedAs: checksandalerts.UsedAsCheck, Modules: ModulesIn(t, dir)}); problem != nil {
		t.Fatalf("the check of the module %s is not valid: %s: %s", name, problem.Path, problem.Message)
	}
	return checksandalerts.Check{Inputs: call, AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
}

// QueuedJobsDescription and QueuedJobsScript are the module of the
// project's own that the design shows: it reads the number of waiting jobs
// from a file and reports it as a measurement.
const QueuedJobsDescription = `summary: Counts the jobs that wait in a queue file.
used_as: check
inputs:
  queue_file_path:
    type: string
    required: true
    description: The absolute path of the file that holds the number of waiting jobs.
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

const QueuedJobsScript = `def run_check(inputs, file_contents):
    queue_file = read_file(inputs["queue_file_path"])
    if queue_file.error:
        return {"status": "unknown", "message": "MMWatchdog cannot read the queue file: " + queue_file.error}
    waiting = queue_file.content.strip()
    if not waiting.isdigit():
        return {"status": "unknown", "message": "the queue file does not hold a whole number"}
    return {
        "status": "ok",
        "message": waiting + " queued jobs",
        "measurements": {"queued_jobs": int(waiting)},
    }
`

// LoadExample loads the example of a module as a whole configuration: as
// one named check of a check group, or as one destination. modules are the
// directories of the modules that are not core.
func LoadExample(t *testing.T, modules checksandalerts.ModuleDirs, usedAs string, example checksandalerts.ModuleExample) error {
	t.Helper()
	indented := func(spaces int) string {
		return strings.Repeat(" ", spaces) + strings.ReplaceAll(strings.TrimSuffix(example.Configuration, "\n"), "\n", "\n"+strings.Repeat(" ", spaces)) + "\n"
	}
	directory := t.TempDir()
	node := "keep_undelivered_alerts_for: 24h\nundelivered_alerts_content_size_limit: 8MiB\nalert_severity_when_this_node_goes_silent: sos\nalert_severity_when_this_node_has_rebooted: info\nkeep_measurement_history_for: 7d\nmeasurement_history_size_limit: 256MiB\n"
	groups, alerts := "check_groups:\n  example:\n    check_every: 30s\n    time_limit_per_run: 5s\n    alert_after_failing_for: 1m\n    recovered_after_healthy_for: 30s\n    checks:\n      ", "destinations: {}\n"
	if usedAs == checksandalerts.UsedAsCheck {
		groups += "the_example:\n" + indented(8)
	} else {
		groups += "companion: {shell: 'true', alert_severity: warn}\n"
		alerts = "destinations:\n  the_example:\n" + indented(4)
	}
	if e := os.Mkdir(filepath.Join(directory, "check_groups"), 0700); e != nil {
		t.Fatal(e)
	}
	for file, content := range map[string]string{"mmwatchdog_node_settings.yml": node, "alert_destinations.yml": alerts, filepath.Join("check_groups", "example.yml"): groups} {
		if e := os.WriteFile(filepath.Join(directory, file), []byte(content), 0600); e != nil {
			t.Fatal(e)
		}
	}
	_, e := config.LoadWithModuleDirs(directory, modules)
	return e
}
