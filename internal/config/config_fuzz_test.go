package config_test

import (
	"bytes"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/testhelpers"
)

// The fuzz tests of the configuration files write the raw bytes of one file
// of a configuration directory beside valid companions of the other files,
// and load the directory twice: as a node loads it, and as a reload of
// files that did not change loads it again. Load reads only the
// configuration directory and the module directories that the test gives
// it in place of those that the node settings state, so no input makes it
// read a directory of this machine. A node reads the files that a check or
// a destination names only when it applies the configuration. See
// docs/changing-mmwatchdog/testing.md, "Fuzz tests for a release".

// configurationFileLimit is the most bytes that Load reads of one
// configuration file.
const configurationFileLimit = 1024 * 1024

// fuzzNodeName is the name of the machine in the alert titles that a
// validation warning measures.
const fuzzNodeName = "app-server-1"

// fuzzMinimalNodeSettings are the four settings that every node states.
const fuzzMinimalNodeSettings = `keep_undelivered_alerts_for: 24h
undelivered_alerts_content_size_limit: 8MiB
alert_severity_when_this_node_goes_silent: sos
alert_severity_when_this_node_has_rebooted: info
keep_measurement_history_for: 7d
measurement_history_size_limit: 256MiB
`

// The companions of a fuzzed file: node settings with an HTTPS receiver, one check group, and one alert
// destination, which a check can name.
const fuzzCompanionNodeSettings = fuzzMinimalNodeSettings + `heartbeat_receiver:
  listen_on_address: 127.0.0.1:8443
  tls_certificate_file: /etc/mmwatchdog/tls/heartbeat_receiver.pem
  tls_private_key_file: /etc/mmwatchdog/tls/heartbeat_receiver.key
`

const fuzzCompanionCheckGroups = `check_groups:
  machine:
    check_every: 30s
    time_limit_per_run: 5s
    alert_after_failing_for: 1m
    recovered_after_healthy_for: 30s
    checks:
      memory_is_below_90_percent:
        node_resource: memory
        fail_when_measurement_above:
          memory_used_percent: 90
        alert_severity: warn
`

const fuzzCompanionAlertDestinations = `destinations:
  operations:
    remind_every: never
    webhook:
      url: https://alerts.example.com/mmwatchdog
      bearer_token_file: /etc/mmwatchdog/secrets/webhook
    alert_severities: [warn, sos]
`

// fuzzApplicationCheckGroup has named checks of core modules and of the
// module queued_jobs of the project's own, a check's own waiting time, a
// check that names its destination, a check that another check keeps
// quiet, and a restart.
const fuzzApplicationCheckGroup = `check_groups:
  myapp:
    check_every: 30s
    time_limit_per_run: 5s
    alert_after_failing_for: 1m
    recovered_after_healthy_for: 30s
    checks:
      myapp_service_is_running:
        systemd_unit_state:
          systemd_unit: myapp.service
          fail_when: the_unit_is_not_active
        alert_severity: sos
        send_alerts_only_to_destinations: [operations]
      myapp_answers_health_request:
        http:
          url: http://127.0.0.1:8080/health
          expected_http_status_codes: [200]
        alert_severity: warn
        alert_after_failing_for: 2m
        no_alerts_or_recovery_actions_while_these_checks_fail: [myapp/myapp_service_is_running]
      queued_jobs_are_below_500:
        queued_jobs:
          queue_file_path: /var/lib/myapp/pending
        fail_when_measurement_above:
          queued_jobs: 500
        alert_severity: warn
    restart_systemd_unit:
      max_attempts: 3
      min_time_between_attempts: 1m
      reset_attempts_after_healthy_for: 10m
      systemd_unit: myapp.service
      triggered_by_checks: [myapp_service_is_running]
      restart_after_failing_for: 1m
`

const fuzzHeartbeatCheckGroup = `check_groups:
  nightly_backup:
    heartbeat:
      max_time_between_heartbeats: 26h
      expected_bearer_token_file: /etc/mmwatchdog/secrets/nightly_backup_heartbeat
    alert_severity: warn
`

const fuzzDirectCheckGroup = `check_groups:
  backup_receipt:
    check_every: 5m
    time_limit_per_run: 10s
    alert_after_failing_for: 10m
    recovered_after_healthy_for: 5m
    shell:
      script: test -f /srv/backups/last-success
    alert_severity: warn
`

// In fuzzCircleOfQuietChecks two checks keep each other quiet.
const fuzzCircleOfQuietChecks = `check_groups:
  web:
    check_every: 30s
    time_limit_per_run: 5s
    alert_after_failing_for: 1m
    recovered_after_healthy_for: 30s
    checks:
      nginx_is_running:
        systemd_unit_state:
          systemd_unit: nginx.service
          fail_when: the_unit_is_not_active
        alert_severity: sos
        no_alerts_or_recovery_actions_while_these_checks_fail: [web/myapp_is_running]
      myapp_is_running:
        systemd_unit_state:
          systemd_unit: myapp.service
          fail_when: the_unit_is_not_active
        alert_severity: sos
        no_alerts_or_recovery_actions_while_these_checks_fail: [web/nginx_is_running]
`

const fuzzNodeSettingsWithStorageAndNodeGroups = `socket: /run/mmwatchdog/mmwatchdog.sock
state_dir: /var/lib/mmwatchdog
max_concurrent_checks: 4
alert_severity_when_this_node_goes_silent: warn
raise_to_sos_after_this_node_is_silent_for: 30m
alert_severity_when_this_node_has_rebooted: info
this_node_is_in_node_groups: [web, production]
raise_to_sos_when_every_node_of_these_node_groups_is_silent: [web]
keep_measurement_history_for: 14d
measurement_history_size_limit: 1GiB
keep_event_history_for: 48h
event_history_content_size_limit: 32MiB
keep_undelivered_alerts_for: 48h
undelivered_alerts_content_size_limit: 16MiB
state_database_size_limit: 256MiB
`

const fuzzHubNodeSettings = fuzzMinimalNodeSettings + `fleet_hub_role:
  send_alert_without_hub_after_waiting_for: 30s
  undelivered_alerts_of_nodes_content_size_limit: 32MiB
  listen_on_address: '0.0.0.0:8445'
  address_for_nodes: hub.example.com:7443
  report_every: 30s
  alert_after_hub_unreachable_for: 3m
  raise_to_sos_when_silent_nodes_reach_percent: 5
  keep_measurement_history_of_nodes_for: 7d
  measurement_history_of_nodes_size_limit: 20GiB
`

const fuzzJoinedNodeSettings = fuzzMinimalNodeSettings + `join_fleet_hub:
  join_code_file: /etc/mmwatchdog/join_code
  prefer_this_node_as_hub_outage_alerter: true
`

// FuzzCheckGroupConfigurationFile loads raw bytes as the one check group
// file of a configuration.
func FuzzCheckGroupConfigurationFile(f *testing.F) {
	// Each seed file, with whether Load accepts it.
	checkGroupFiles := map[string]bool{
		fuzzCompanionCheckGroups:  true,
		fuzzApplicationCheckGroup: true,
		fuzzHeartbeatCheckGroup:   true,
		fuzzDirectCheckGroup:      true,
		paddedWithAComment(fuzzCompanionCheckGroups, configurationFileLimit):   true,
		paddedWithAComment(fuzzCompanionCheckGroups, configurationFileLimit+1): false,
		"":                   false,
		"check_groups: {}\n": false,
		strings.Replace(fuzzCompanionCheckGroups, "    check_every: 30s\n", "", 1):                                                               false,
		strings.Replace(fuzzCompanionCheckGroups, "time_limit_per_run: 5s", "time_limit_per_run: 1m", 1):                                         false,
		strings.Replace(fuzzCompanionCheckGroups, "check_every: 30s", "check_every: &every 30s", 1):                                              false,
		strings.Replace(fuzzCompanionCheckGroups, "    time_limit_per_run: 5s\n", "    time_limit_per_run: 5s\n    time_limit_per_run: 5s\n", 1): false,
		strings.Replace(fuzzCompanionCheckGroups, "node_resource: memory", "no_such_module: memory", 1):                                          false,
		fuzzCompanionCheckGroups + "---\n" + fuzzCompanionCheckGroups:                                                                            false,
		fuzzCircleOfQuietChecks: false,
	}
	for _, file := range checksandalerts.Names(checkGroupFiles) {
		if len(file) < configurationFileLimit {
			f.Add([]byte(file), uint8(0))
		}
	}
	f.Add([]byte(fuzzCompanionCheckGroups), uint8(1))
	f.Add([]byte(fuzzCompanionCheckGroups), uint8(2))
	addRepositoryExamples(f, "../../examples/check_groups/*.yml", "../../examples/*/*/check_groups/*.yml", "../../examples/*/*/*/check_groups/*.yml")
	f.Fuzz(func(t *testing.T, checkGroupFile []byte, boundary uint8) {
		checkGroupFile = fuzzConfigurationFileAtLimit(checkGroupFile, boundary)
		directory, moduleDirs := fuzzConfigurationDirectory(t, false)
		if err := os.WriteFile(filepath.Join(directory, "check_groups", "checks.yml"), checkGroupFile, 0600); err != nil {
			t.Fatal(err)
		}
		checkLoadOfFuzzedFile(t, directory, moduleDirs, checkGroupFile, checkGroupFiles)
	})
}

// FuzzNodeSettingsFile loads raw bytes as the node settings of a
// configuration.
func FuzzNodeSettingsFile(f *testing.F) {
	// Each seed file, with whether Load accepts it.
	nodeSettingsFiles := map[string]bool{
		fuzzCompanionNodeSettings:                true,
		fuzzMinimalNodeSettings:                  true,
		fuzzNodeSettingsWithStorageAndNodeGroups: true,
		fuzzHubNodeSettings:                      true,
		fuzzJoinedNodeSettings:                   true,
		paddedWithAComment(fuzzMinimalNodeSettings, configurationFileLimit):   false,
		paddedWithAComment(fuzzMinimalNodeSettings, configurationFileLimit+1): false,
		"":     false,
		"{}\n": false,
		strings.Replace(fuzzMinimalNodeSettings, "keep_measurement_history_for: 7d\n", "", 1):                    false,
		fuzzMinimalNodeSettings + "socket: run/mmwatchdog.sock\n":                                                false,
		fuzzMinimalNodeSettings + "max_concurrent_checks: 0\n":                                                   false,
		fuzzMinimalNodeSettings + "max_concurrent_checks: !!int 8\n":                                             false,
		fuzzMinimalNodeSettings + "event_history_content_size_limit: 64MiB\nstate_database_size_limit: 128MiB\n": false,
		fuzzHubNodeSettings + "join_fleet_hub:\n  join_code_file: /etc/mmwatchdog/join_code\n":                   false,
	}
	// The file of exactly 1 MiB is accepted.
	nodeSettingsFiles[paddedWithAComment(fuzzMinimalNodeSettings, configurationFileLimit)] = true
	for _, file := range checksandalerts.Names(nodeSettingsFiles) {
		if len(file) < configurationFileLimit {
			f.Add([]byte(file), uint8(0))
		}
	}
	f.Add([]byte(fuzzMinimalNodeSettings), uint8(1))
	f.Add([]byte(fuzzMinimalNodeSettings), uint8(2))
	addRepositoryExamples(f, "../../examples/mmwatchdog_node_settings.yml", "../../examples/*/*/mmwatchdog_node_settings.yml", "../../examples/*/*/*/mmwatchdog_node_settings.yml")
	f.Fuzz(func(t *testing.T, nodeSettingsFile []byte, boundary uint8) {
		nodeSettingsFile = fuzzConfigurationFileAtLimit(nodeSettingsFile, boundary)
		// The companions follow the node settings as
		// testhelpers.WriteConfigText does: a node that names join_fleet_hub
		// has no alert_destinations.yml.
		directory, moduleDirs := fuzzConfigurationDirectory(t, bytes.Contains(nodeSettingsFile, []byte("join_fleet_hub")))
		if err := os.WriteFile(filepath.Join(directory, "mmwatchdog_node_settings.yml"), nodeSettingsFile, 0600); err != nil {
			t.Fatal(err)
		}
		checkLoadOfFuzzedFile(t, directory, moduleDirs, nodeSettingsFile, nodeSettingsFiles)
	})
}

// fuzzConfigurationDirectory writes the companions as a valid configuration
// directory, and the module queued_jobs of the project's own, so that a
// check group can name a module that is not core. A node that joins a hub
// gets its alert settings from the hub, and Load refuses its own
// alert_destinations.yml. It returns the directory and the module
// directories for Load: an official extra module directory that does not
// exist, which holds no module, and the directory with queued_jobs.
func fuzzConfigurationDirectory(t *testing.T, nodeJoinsAHub bool) (string, checksandalerts.ModuleDirs) {
	t.Helper()
	directory := testhelpers.ConfigFile(t, fuzzCompanionNodeSettings+fuzzCompanionCheckGroups)
	alertDestinations := filepath.Join(directory, "alert_destinations.yml")
	if nodeJoinsAHub {
		if err := os.Remove(alertDestinations); err != nil {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(alertDestinations, []byte(fuzzCompanionAlertDestinations), 0600); err != nil {
		t.Fatal(err)
	}
	officialExtraModuleDir, externalModuleDir, _ := testhelpers.TestModuleDirs(directory)
	testhelpers.InstallStarlarkModule(t, externalModuleDir, "queued_jobs", testhelpers.QueuedJobsDescription, testhelpers.QueuedJobsScript)
	return directory, checksandalerts.ModuleDirs{OfficialExtra: officialExtraModuleDir, External: externalModuleDir}
}

// checkLoadOfFuzzedFile loads the directory twice and checks what Load
// decided about file, the fuzzed file. seeds says of each seed file whether
// Load accepts it.
func checkLoadOfFuzzedFile(t *testing.T, directory string, moduleDirs checksandalerts.ModuleDirs, file []byte, seeds map[string]bool) {
	t.Helper()
	first, firstErr := config.LoadWithModuleDirs(directory, moduleDirs)
	second, secondErr := config.LoadWithModuleDirs(directory, moduleDirs)
	if (firstErr == nil) != (secondErr == nil) {
		t.Fatalf("two loads of the same files disagree: first %v; second %v", firstErr, secondErr)
	}
	if wantAccepted, isSeed := seeds[string(file)]; isSeed && (firstErr == nil) != wantAccepted {
		t.Fatalf("a seed file: accepted %v, want %v: %v", firstErr == nil, wantAccepted, firstErr)
	}
	if firstErr != nil {
		return
	}
	if len(file) > configurationFileLimit {
		t.Fatalf("Load accepted a file of %d bytes; it reads at most %d", len(file), configurationFileLimit)
	}
	if firstLoad, secondLoad := semanticConfiguration(t, first), semanticConfiguration(t, second); firstLoad != secondLoad {
		t.Fatalf("two loads of the same files give two configurations:\n%s\n%s", firstLoad, secondLoad)
	}
	checkAcceptedConfiguration(t, first)
}

// semanticConfiguration is what a node takes from a loaded configuration,
// as JSON: the settings with where each one is written, the identity of
// each check, which decides whether the check keeps its state on a reload,
// and the validation warnings that depend on the configuration alone. The
// warning about credential files looks at files of this machine, so it is
// left out.
func semanticConfiguration(t *testing.T, c *config.Config) string {
	t.Helper()
	identities := map[string]string{}
	for key, definition := range config.CheckDefinitions(c) {
		identities[key] = definition.Identity()
	}
	written, err := json.Marshal(struct {
		Configuration                               *config.Config
		CheckIdentities                             map[string]string
		AlertTitlesThatWillBeShortened              []checksandalerts.AlertTitleThatWillBeShortened
		SystemdUnitsMissingFromSystemdUnitsToIgnore []checksandalerts.SystemdUnitMissingFromUnitsToIgnore
		FilesystemsMissingFromFilesystemsToIgnore   []checksandalerts.FilesystemMissingFromFilesystemsToIgnore
		ChecksThatNameACheckWithALowerAlertSeverity []config.CheckThatNamesACheckWithALowerAlertSeverity
	}{c, identities, c.AlertTitlesThatWillBeShortened(fuzzNodeName), c.SystemdUnitsMissingFromUnitsToIgnore(), c.FilesystemsMissingFromFilesystemsToIgnore(), c.ChecksThatNameACheckWithALowerAlertSeverity()})
	if err != nil {
		t.Fatalf("an accepted configuration cannot be written as JSON: %v", err)
	}
	return string(written)
}

// checkAcceptedConfiguration checks what the parts of MMWatchdog that use a
// configuration rely on once Load has accepted it.
func checkAcceptedConfiguration(t *testing.T, c *config.Config) {
	t.Helper()
	for setting, path := range map[string]string{"socket": c.Socket, "state_dir": c.StateDir, "official_extra_module_dir": c.OfficialExtraModuleDir, "external_module_dir": c.ExternalModuleDir, "module_state_dir": c.ModuleStateDir} {
		if !filepath.IsAbs(path) {
			t.Fatalf("%s is not an absolute path: %q", setting, path)
		}
	}
	if c.MaxConcurrentChecks < 1 {
		t.Fatalf("max_concurrent_checks is %d, so no check could run", c.MaxConcurrentChecks)
	}
	// The hub checks the same settings in a join request and in every node
	// status report.
	if problem := c.NodeFleetSettings().Problem(); problem != "" {
		t.Fatalf("the hub would refuse the fleet settings of this node, which %s", problem)
	}
	limits := c.StorageLimits()
	if limits.KeepEventHistoryFor <= 0 || limits.KeepUndeliveredAlertsFor <= 0 || limits.EventHistoryBytes <= 0 || limits.UndeliveredAlertsBytes <= 0 {
		t.Fatalf("a storage limit is not positive: %+v", limits)
	}
	// state.db may grow to at least twice what its stores hold. The sum is
	// computed without the overflow of int64.
	need := new(big.Int).Add(big.NewInt(limits.EventHistoryBytes), big.NewInt(limits.UndeliveredAlertsBytes))
	need.Add(need, big.NewInt(limits.UndeliveredAlertsOfNodesBytes))
	need.Mul(need, big.NewInt(2))
	if big.NewInt(limits.StateDatabaseBytes).Cmp(need) < 0 {
		t.Fatalf("state_database_size_limit is %d bytes, less than twice what the stores in state.db hold, %s bytes", limits.StateDatabaseBytes, need)
	}
	if hub := c.FleetHubRole; hub != nil {
		if c.JoinFleetHub != nil {
			t.Fatal("a hub joins another hub")
		}
		// Every joined node checks the fleet timings that the hub sends it.
		if setting, problem := hub.FleetTimings.Problem(); problem != "" {
			t.Fatalf("a joined node would refuse the fleet_hub_role.%s of the hub: %s", setting, problem)
		}
		if hubLimits := hub.HubLimits(); hubLimits.KeepEventHistoryOfNodesFor <= 0 || hubLimits.EventHistoryOfNodesBytes <= 0 || hubLimits.KeepMeasurementHistoryOfNodesFor.Value() <= 0 {
			t.Fatalf("a storage limit of the hub is not positive: %+v", hubLimits)
		}
	}
	if c.JoinFleetHub != nil && !filepath.IsAbs(c.JoinFleetHub.JoinCodeFile) {
		t.Fatalf("join_fleet_hub.join_code_file is not an absolute path: %q", c.JoinFleetHub.JoinCodeFile)
	}
	definitions := config.CheckDefinitions(c)
	keys := checksandalerts.Names(definitions)
	if len(keys) == 0 {
		t.Fatal("an accepted configuration has no check")
	}
	checkWithIdentity := map[string]string{}
	for _, key := range keys {
		d := definitions[key]
		if !checksandalerts.NamePattern.MatchString(d.CheckGroup) || !checksandalerts.NamePattern.MatchString(d.Name) {
			t.Fatalf("%s: the name of the check group or of the check is not valid", key)
		}
		// Identity writes the definition as JSON and ignores an error, so a
		// definition that cannot be written would share its identity.
		if _, err := json.Marshal(d); err != nil {
			t.Fatalf("%s: the definition cannot be written as JSON: %v", key, err)
		}
		// The identity decides which check keeps which state on a reload.
		identity := d.Identity()
		if other := checkWithIdentity[identity]; other != "" {
			t.Fatalf("the checks %s and %s have one identity", other, key)
		}
		checkWithIdentity[identity] = key
		if d.Heartbeat != nil {
			if config.SeverityRank(d.Severity.AlertSeverity) < 0 {
				t.Fatalf("%s: a heartbeat has the alert_severity %q", key, d.Severity.AlertSeverity)
			}
			continue
		}
		if d.Timing.CheckEvery.Value() < config.ScheduleInterval || d.Timing.TimeLimitPerRun.Value() <= 0 || d.Timing.TimeLimitPerRun.Value() > d.Timing.CheckEvery.Value() {
			t.Fatalf("%s: the check cannot be scheduled with check_every %q and time_limit_per_run %q", key, d.Timing.CheckEvery, d.Timing.TimeLimitPerRun)
		}
		// A waiting time that does not read as a duration would act as 0s.
		for _, waiting := range []checksandalerts.Duration{d.Timing.AlertAfterFailingFor, d.Timing.RecoveredAfterHealthyFor} {
			if value, err := checksandalerts.ParseDuration(string(waiting)); err != nil || value < 0 {
				t.Fatalf("%s: the waiting time %q is not a duration of 0s or more", key, waiting)
			}
		}
		// A check that can fail, and every check with a limit, alerts with a
		// severity; without one, its alert would go to no destination.
		if check := c.CheckGroups[d.CheckGroup].Checks[d.Name].Check; (!check.RecordsOnly() || check.HasLimit()) && config.SeverityRank(check.AlertSeverity) < 0 {
			t.Fatalf("%s: a check that can fail has the alert_severity %q", key, check.AlertSeverity)
		}
		for _, cause := range d.QuietWhileTheseChecksFail {
			if _, exists := definitions[cause]; !exists {
				t.Fatalf("%s: is kept quiet by %s, which is no check of this node", key, cause)
			}
		}
	}
	// No checks keep each other quiet in a circle: while all of them fail,
	// MMWatchdog would send no alert for any of them.
	visiting, visited := map[string]bool{}, map[string]bool{}
	var visit func(key string)
	visit = func(key string) {
		if visiting[key] {
			t.Fatalf("the checks that keep %s quiet lead back to it", key)
		}
		if visited[key] {
			return
		}
		visiting[key] = true
		for _, cause := range definitions[key].QuietWhileTheseChecksFail {
			visit(cause)
		}
		visiting[key], visited[key] = false, true
	}
	for _, key := range keys {
		visit(key)
	}
}

// paddedWithAComment returns text with a comment line at its end that makes
// it size bytes long.
func paddedWithAComment(text string, size int) string {
	return text + "#" + strings.Repeat("x", size-len(text)-2) + "\n"
}

// addRepositoryExamples adds the files of the repository's examples that
// the patterns name as seeds. A compiled test program that runs without the
// source checkout finds none.
func addRepositoryExamples(f *testing.F, patterns ...string) {
	f.Helper()
	for _, pattern := range patterns {
		files, err := filepath.Glob(pattern)
		if err != nil {
			f.Fatal(err)
		}
		for _, file := range files {
			text, err := os.ReadFile(file)
			if err != nil {
				f.Fatal(err)
			}
			f.Add(text, uint8(0))
		}
	}
}

// fuzzConfigurationFileAtLimit tests the file-size boundary without putting
// megabytes of padding into the corpus that Go mutates and minimizes.
func fuzzConfigurationFileAtLimit(file []byte, boundary uint8) []byte {
	if boundary != 1 && boundary != 2 {
		return file
	}
	size := configurationFileLimit + int(boundary) - 1
	if len(file) >= size {
		return file
	}
	return append(bytes.Clone(file), bytes.Repeat([]byte(" "), size-len(file))...)
}
