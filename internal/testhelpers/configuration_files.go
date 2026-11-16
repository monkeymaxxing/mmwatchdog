package testhelpers

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"mmwatchdog/internal/checksandalerts"
)

// ConfigFile writes text as the files of a new configuration directory,
// as WriteConfigText does, and returns the directory.
func ConfigFile(t *testing.T, text string) string {
	t.Helper()
	directory := t.TempDir()
	if err := WriteConfigText(directory, text); err != nil {
		t.Fatal(err)
	}
	return directory
}

// ConfigFileAsWritten writes text with none of the settings filled in that
// every configuration must state, for the tests of those settings.
func ConfigFileAsWritten(t *testing.T, text string) string {
	t.Helper()
	directory := t.TempDir()
	if err := writeConfigFiles(directory, text, false, false); err != nil {
		t.Fatal(err)
	}
	return directory
}

// ConfigFileWithoutSeverities writes text and fills in only the timings
// that its check groups leave out, for the tests of the alert severity
// settings.
func ConfigFileWithoutSeverities(t *testing.T, text string) string {
	t.Helper()
	directory := t.TempDir()
	if err := writeConfigFiles(directory, text, false, true); err != nil {
		t.Fatal(err)
	}
	return directory
}

// WriteConfigText writes text as the configuration directory's files.
// Every check must state its alert_severity, every check group its four
// timings, and every node its two severities; tests about other features
// get TestTimings and TestNodeSeverities filled in where they leave them
// out, and warn on each check that can alert. Every configuration that
// names no directory for modules gets the test's own (TestModuleDirs), so no
// test reads the modules of the machine that it runs on.
func WriteConfigText(directory, text string) error {
	return writeConfigFiles(directory, text, true, true)
}

// Keep authored line numbers while separating settings from check groups.
func writeConfigFiles(directory, text string, fillSeverities, fillTimings bool) error {
	if err := os.MkdirAll(filepath.Join(directory, "check_groups"), 0700); err != nil {
		return err
	}
	settings, groups := []string{}, []string{}
	inGroups := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "check_groups:") {
			inGroups = true
		} else if len(line) > 0 && line[0] != ' ' && line[0] != '#' && strings.Contains(line, ":") {
			inGroups = false
		}
		if inGroups {
			groups = append(groups, line)
			settings = append(settings, "")
		} else {
			settings = append(settings, line)
			groups = append(groups, "")
		}
	}
	nodeText, groupText := strings.Join(settings, "\n"), strings.Join(groups, "\n")
	if strings.TrimSpace(strings.Join(filterComments(settings), "\n")) == "" {
		nodeText = "{}\n"
	}
	if strings.TrimSpace(groupText) == "" {
		groupText = "{}\n"
	}
	nodeText = fillYAML(nodeText, func(root *yaml.Node) bool { return fillModuleDirs(root, directory) })
	if fillSeverities {
		nodeText, groupText = fillYAML(nodeText, fillNodeSeverity), fillYAML(groupText, fillCheckSeverities)
	}
	if fillTimings {
		nodeText = fillYAML(nodeText, func(root *yaml.Node) bool {
			filled := false
			for key, value := range map[string]string{"keep_undelivered_alerts_for": "24h", "undelivered_alerts_content_size_limit": "8MiB"} {
				if yamlValue(root, key) == nil {
					addYAMLKey(root, key, value)
					filled = true
				}
			}
			return filled
		})
		groupText = fillYAML(groupText, fillCheckGroupTimings)
	}
	if err := os.WriteFile(filepath.Join(directory, "mmwatchdog_node_settings.yml"), []byte(nodeText), 0600); err != nil {
		return err
	}
	// A node that has not joined a hub must have its destinations file. A
	// test about other features runs with no destination; a test about
	// destinations writes its own file afterward.
	alerts := filepath.Join(directory, "alert_destinations.yml")
	if _, err := os.Stat(alerts); os.IsNotExist(err) && !strings.Contains(nodeText, "join_fleet_hub") {
		if err := os.WriteFile(alerts, []byte("destinations: {}\n"), 0600); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(directory, "check_groups", "checks.yml"), []byte(groupText), 0600)
}

// checkKinds are the keys that make a mapping a check: the core modules and
// a heartbeat. A key that is no setting makes a mapping a check too: it is
// the name of a module that is not core (moduleThatIsNotCore).
var checkKinds = append(checksandalerts.CheckModuleNames(func(*checksandalerts.CheckModule) bool { return true }), "heartbeat")

// moduleThatIsNotCore reports that the mapping of a check group or of a
// named check has a key that is no setting of either: the name of a module
// that is not core.
func moduleThatIsNotCore(check *yaml.Node) bool {
	settings := checksandalerts.SettingNamesBesideAModule()
	for i := 0; i+1 < len(check.Content); i += 2 {
		if key := check.Content[i].Value; !slices.Contains(settings, key) && !slices.Contains(checkKinds, key) {
			return true
		}
	}
	return false
}

// MeasurementKinds are the kinds that only record their readings when the
// check states no limit.
var MeasurementKinds = func() map[string]bool {
	kinds := map[string]bool{}
	for _, name := range checksandalerts.CheckModuleNames(func(m *checksandalerts.CheckModule) bool { return m.Measures }) {
		kinds[name] = true
	}
	return kinds
}()

// FlowTimings are TestTimings for a check group written on one line.
const FlowTimings = "check_every: 30s, time_limit_per_run: 30s, alert_after_failing_for: 1m, recovered_after_healthy_for: 30s"

// TestTimings are the four timings a test's check group gets where the test
// states none.
var TestTimings = map[string]string{"check_every": "30s", "time_limit_per_run": "30s", "alert_after_failing_for": "1m", "recovered_after_healthy_for": "30s"}

// TestActionWaitingTimes names the required waiting time of each recovery
// action. A test that states none gets 0s, so the action is due at the
// first failed run.
var TestActionWaitingTimes = map[string]string{"restart_systemd_unit": "restart_after_failing_for", "recovery_command": "run_after_failing_for"}

// TestNodeSeverities are the two severities and the two settings of the
// measurement history that a test's node settings get where the test states
// none.
var TestNodeSeverities = map[string]string{"alert_severity_when_this_node_goes_silent": "sos", "alert_severity_when_this_node_has_rebooted": "info",
	"keep_measurement_history_for": "7d", "measurement_history_size_limit": "256MiB", "keep_undelivered_alerts_for": "24h", "undelivered_alerts_content_size_limit": "8MiB"}

// fillYAML applies fill to text's root mapping and returns the text again,
// unchanged when fill adds nothing or text does not parse.
func fillYAML(text string, fill func(root *yaml.Node) bool) string {
	var document yaml.Node
	if strings.Contains(text, "\n---") || yaml.Unmarshal([]byte(text), &document) != nil || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return text
	}
	// Encoding drops leading blank lines, which keep authored line numbers.
	leading := text[:len(text)-len(strings.TrimLeft(text, "\n"))]
	if !fill(document.Content[0]) {
		return text
	}
	var b strings.Builder
	encoder := yaml.NewEncoder(&b)
	encoder.SetIndent(2)
	if encoder.Encode(&document) != nil {
		return text
	}
	return leading + b.String()
}

func yamlValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func addYAMLKey(m *yaml.Node, key, value string) {
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, &yaml.Node{Kind: yaml.ScalarNode, Value: value})
}

// fillModuleDirs gives the node settings the three directories for modules
// below the test's configuration directory, where they state none.
func fillModuleDirs(root *yaml.Node, configDir string) bool {
	officialExtraModuleDir, externalModuleDir, moduleStateDir := TestModuleDirs(configDir)
	filled := false
	for _, setting := range [][2]string{{"official_extra_module_dir", officialExtraModuleDir}, {"external_module_dir", externalModuleDir}, {"module_state_dir", moduleStateDir}} {
		if yamlValue(root, setting[0]) == nil {
			addYAMLKey(root, setting[0], setting[1])
			filled = true
		}
	}
	return filled
}

func fillNodeSeverity(root *yaml.Node) bool {
	filled := false
	for _, key := range checksandalerts.Names(TestNodeSeverities) {
		if yamlValue(root, key) == nil {
			addYAMLKey(root, key, TestNodeSeverities[key])
			filled = true
		}
	}
	// A hub states how much measurement history of its nodes it keeps.
	if hub := yamlValue(root, "fleet_hub_role"); hub != nil && hub.Kind == yaml.MappingNode {
		for _, key := range checksandalerts.Names(TestHubHistory) {
			if yamlValue(hub, key) == nil {
				addYAMLKey(hub, key, TestHubHistory[key])
				filled = true
			}
		}
	}
	return filled
}

// TestHubHistory are the two settings of the measurement history of nodes
// that a test's fleet_hub_role gets where the test states none.
var TestHubHistory = map[string]string{"keep_measurement_history_of_nodes_for": "7d", "measurement_history_of_nodes_size_limit": "20GiB", "report_every": "30s", "alert_after_hub_unreachable_for": "3m", "send_alert_without_hub_after_waiting_for": "30s", "undelivered_alerts_of_nodes_content_size_limit": "32MiB"}

// fillCheckGroupTimings gives every check group that MMWatchdog runs the timings it
// leaves out, and its recovery actions their waiting time. A heartbeat
// takes none.
func fillCheckGroupTimings(root *yaml.Node) bool {
	groups := yamlValue(root, "check_groups")
	if groups == nil || groups.Kind != yaml.MappingNode {
		return false
	}
	filled := false
	for i := 1; i < len(groups.Content); i += 2 {
		group := groups.Content[i]
		if group.Kind != yaml.MappingNode || yamlValue(group, "heartbeat") != nil {
			continue
		}
		for _, key := range checksandalerts.Names(TestTimings) {
			log := yamlValue(group, "log_lines")
			eventCheck := yamlValue(group, "out_of_memory_kills") != nil || (log != nil && yamlValue(log, "fail_when_no_new_line_for") == nil)
			if key == "alert_after_failing_for" && eventCheck {
				continue
			}
			if yamlValue(group, key) == nil {
				addYAMLKey(group, key, TestTimings[key])
				filled = true
			}
		}
		for _, action := range []string{"restart_systemd_unit", "recovery_command"} {
			settings := yamlValue(group, action)
			if settings == nil || settings.Kind != yaml.MappingNode {
				continue
			}
			for key, value := range map[string]string{"max_attempts": "3", "min_time_between_attempts": "1m", "reset_attempts_after_healthy_for": "10m"} {
				if yamlValue(settings, key) == nil {
					addYAMLKey(settings, key, value)
					filled = true
				}
			}
			if action == "recovery_command" {
				if yamlValue(settings, "time_limit_per_attempt") == nil {
					addYAMLKey(settings, "time_limit_per_attempt", "1m")
					filled = true
				}
				if yamlValue(settings, "triggered_by_checks") == nil {
					names := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
					if checks := yamlValue(group, "checks"); checks != nil && checks.Kind == yaml.MappingNode {
						for j := 0; j < len(checks.Content); j += 2 {
							check := checks.Content[j+1]
							if yamlValue(check, "only_record_readings") == nil && yamlValue(check, "systemd_unit_crash_loop") == nil {
								names.Content = append(names.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: checks.Content[j].Value})
							}
						}
					} else {
						for _, kind := range checkKinds {
							if yamlValue(group, kind) != nil {
								names.Content = append(names.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: kind})
							}
						}
					}
					if len(names.Content) > 0 {
						settings.Content = append(settings.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "triggered_by_checks"}, names)
						filled = true
					}
				}
			}
		}
		for _, action := range checksandalerts.Names(TestActionWaitingTimes) {
			if settings := yamlValue(group, action); settings != nil && settings.Kind == yaml.MappingNode && yamlValue(settings, TestActionWaitingTimes[action]) == nil {
				addYAMLKey(settings, TestActionWaitingTimes[action], "0s")
				filled = true
			}
		}
	}
	return filled
}

// fillCheckSeverities gives every check that can alert alert_severity: warn.
func fillCheckSeverities(root *yaml.Node) bool {
	groups := yamlValue(root, "check_groups")
	if groups == nil || groups.Kind != yaml.MappingNode {
		return false
	}
	filled := false
	fill := func(check *yaml.Node) {
		if check.Kind != yaml.MappingNode || yamlValue(check, "alert_severity") != nil {
			return
		}
		for _, kind := range checkKinds {
			value := yamlValue(check, kind)
			if value == nil {
				continue
			}
			if MeasurementKinds[kind] && yamlValue(check, "fail_when_measurement_above") == nil && yamlValue(check, "fail_when_measurement_below") == nil {
				if yamlValue(check, "only_record_readings") == nil {
					addYAMLKey(check, "only_record_readings", "true")
					filled = true
				}
				return // records only
			}
			addYAMLKey(check, "alert_severity", "warn")
			filled = true
			return
		}
		// A check whose module is not core always states its severity.
		if moduleThatIsNotCore(check) {
			addYAMLKey(check, "alert_severity", "warn")
			filled = true
		}
	}
	for i := 1; i < len(groups.Content); i += 2 {
		group := groups.Content[i]
		if group.Kind != yaml.MappingNode {
			continue
		}
		fill(group)
		if checks := yamlValue(group, "checks"); checks != nil && checks.Kind == yaml.MappingNode {
			for j := 1; j < len(checks.Content); j += 2 {
				fill(checks.Content[j])
			}
		}
	}
	return filled
}

func filterComments(lines []string) []string {
	out := []string{}
	for _, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			out = append(out, line)
		}
	}
	return out
}

// TestModuleDirs returns the three directories for modules of a test's node
// whose configuration directory is configDir: the official extra modules,
// the modules of the project's own, and the module state.
func TestModuleDirs(configDir string) (officialExtraModuleDir, externalModuleDir, moduleStateDir string) {
	return filepath.Join(configDir, "official_extra_modules"), filepath.Join(configDir, "external_modules"), filepath.Join(configDir, "module_state")
}

// AllSeverities is the alert_severities of a destination that takes
// every alert.
var AllSeverities = []string{"info", "warn", "sos"}
