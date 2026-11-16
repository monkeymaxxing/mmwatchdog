package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/testhelpers"
)

func TestRequiredInterfaceSettings(t *testing.T) {
	t.Parallel()
	const configuration = `fleet_hub_role:
  listen_on_address: '0.0.0.0:8445'
  address_for_nodes: hub.example.com:8445
  report_every: 30s
  alert_after_hub_unreachable_for: 3m
  send_alert_without_hub_after_waiting_for: 30s
  undelivered_alerts_of_nodes_content_size_limit: 32MiB
  keep_measurement_history_of_nodes_for: 7d
  measurement_history_of_nodes_size_limit: 20GiB
check_groups:
  app:
    check_every: 30s
    time_limit_per_run: 5s
    alert_after_failing_for: 1m
    recovered_after_healthy_for: 30s
    http: {url: 'https://example.com'}
    alert_severity: warn
    restart_systemd_unit:
      systemd_unit: app.service
      triggered_by_checks: [http]
      restart_after_failing_for: 1m
      max_attempts: 3
      min_time_between_attempts: 1m
      reset_attempts_after_healthy_for: 10m
    recovery_command:
      script: /usr/local/bin/recover-app
      triggered_by_checks: [http]
      run_after_failing_for: 1m
      time_limit_per_attempt: 1m
      max_attempts: 3
      min_time_between_attempts: 1m
      reset_attempts_after_healthy_for: 10m
`
	cases := []struct{ file, setting, value, path string }{
		{"mmwatchdog_node_settings.yml", "keep_undelivered_alerts_for", "24h", "keep_undelivered_alerts_for"},
		{"mmwatchdog_node_settings.yml", "undelivered_alerts_content_size_limit", "8MiB", "undelivered_alerts_content_size_limit"},
		{"mmwatchdog_node_settings.yml", "listen_on_address", "'0.0.0.0:8445'", "fleet_hub_role.listen_on_address"},
		{"mmwatchdog_node_settings.yml", "report_every", "30s", "fleet_hub_role.report_every"},
		{"mmwatchdog_node_settings.yml", "alert_after_hub_unreachable_for", "3m", "fleet_hub_role.alert_after_hub_unreachable_for"},
		{"mmwatchdog_node_settings.yml", "send_alert_without_hub_after_waiting_for", "30s", "fleet_hub_role.send_alert_without_hub_after_waiting_for"},
		{"mmwatchdog_node_settings.yml", "undelivered_alerts_of_nodes_content_size_limit", "32MiB", "fleet_hub_role.undelivered_alerts_of_nodes_content_size_limit"},
	}
	for _, action := range []string{"restart_systemd_unit", "recovery_command"} {
		for _, setting := range []struct{ name, value string }{{"max_attempts", "3"}, {"min_time_between_attempts", "1m"}, {"reset_attempts_after_healthy_for", "10m"}} {
			cases = append(cases, struct{ file, setting, value, path string }{"check_groups/checks.yml", setting.name, setting.value, "check_groups.app." + action + "." + setting.name})
		}
	}
	for _, setting := range []struct{ name, value string }{{"triggered_by_checks", "[http]"}, {"time_limit_per_attempt", "1m"}} {
		cases = append(cases, struct{ file, setting, value, path string }{"check_groups/checks.yml", setting.name, setting.value, "check_groups.app.recovery_command." + setting.name})
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			directory := testhelpers.ConfigFile(t, configuration)
			if _, err := config.Load(directory); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, tc.file)
			written, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			text := string(written)
			needle := tc.setting + ": " + tc.value + "\n"
			start := 0
			if strings.Contains(tc.path, "recovery_command.") {
				start = strings.Index(text, "recovery_command:")
			}
			index := strings.Index(text[start:], needle)
			if index < 0 {
				t.Fatal("missing test setting", text)
			}
			index += start
			// Remove the whole setting line, keeping the rest of the block valid.
			beginning := strings.LastIndex(text[:index], "\n") + 1
			text = text[:beginning] + text[index+len(needle):]
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			_, err = config.Load(directory)
			if err == nil || !strings.Contains(err.Error(), tc.path+": is required:") {
				t.Fatal(err)
			}
		})
	}
}

func TestScriptInputValidationUsesConfigurationLocation(t *testing.T) {
	t.Parallel()
	for _, usedAs := range []string{checksandalerts.UsedAsCheck, checksandalerts.UsedAsAlertDestination} {
		t.Run(usedAs, func(t *testing.T) {
			modules := t.TempDir()
			script := testhelpers.ScriptOfACheck
			if usedAs == checksandalerts.UsedAsAlertDestination {
				script = testhelpers.ScriptOfADestination
			}
			script += "\ndef validate_inputs(inputs):\n    if inputs[\"count\"] < 1:\n        return \"count: add count: 1 or more\"\n    return None\n"
			testhelpers.InstallStarlarkModule(t, modules, "input_example", testhelpers.TestModuleDescription("input_example", usedAs, map[string]string{"count": "integer"}, nil), script)
			load := func(count string) (*config.Config, error) {
				directory := testhelpers.ConfigFile(t, "external_module_dir: "+modules+"\ncheck_groups: {app: {shell: 'true'}}\n")
				if usedAs == checksandalerts.UsedAsCheck {
					// Use the normal helper for complete group timings.
					if err := testhelpers.WriteConfigText(directory, "external_module_dir: "+modules+"\ncheck_groups: {app: {input_example: {count: "+count+"}}}\n"); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.WriteFile(filepath.Join(directory, "alert_destinations.yml"), []byte("destinations:\n  example:\n    input_example:\n      count: "+count+"\n    alert_severities: [warn]\n    remind_every: never\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return config.Load(directory)
			}
			if _, err := load("1"); err != nil {
				t.Fatal(err)
			}
			_, err := load("0")
			if err == nil || !strings.Contains(err.Error(), "input_example.count: add count: 1 or more") || !strings.Contains(err.Error(), ".yml:") {
				t.Fatal(err)
			}
			if _, err = load("wrong"); err == nil || !strings.Contains(err.Error(), "expected a whole number") {
				t.Fatal(err)
			}
		})
	}
}

func TestExplicitRecordingAndRequiredModuleInputs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ check, want string }{
		{"node_resource: memory", "only_record_readings: is required without alert_severity"},
		{"only_record_readings: true\n    http: {url: 'https://example.com'}", "only_record_readings: only a measurement check can record readings"},
		{"only_record_readings: true\n    node_resource: memory\n    alert_severity: warn", "only_record_readings: a check that only records readings has no limits or alert severity"},
		{"tls: {address: 'example.com:443'}\n    alert_severity: warn", "tls.fail_when_certificate_expires_within: is required:"},
		{"postgresql: {database_server_host: database.example.com, connection_security: verified_tls, login_role: mmwatchdog, login_password_file: /etc/mmwatchdog/secrets/database}\n    alert_severity: warn", "postgresql.database_server_port: is required with database_server_host:"},
		{"mysql: {database_server_host: database.example.com, connection_security: verified_tls, login_user: mmwatchdog, login_password_file: /etc/mmwatchdog/secrets/database}\n    alert_severity: warn", "mysql.database_server_port: is required with database_server_host:"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			directory := testhelpers.ConfigFile(t, "check_groups: {app: {shell: 'true'}}")
			text := "check_groups:\n  app:\n    check_every: 30s\n    time_limit_per_run: 5s\n    alert_after_failing_for: 1m\n    recovered_after_healthy_for: 30s\n    " + tc.check + "\n"
			if err := os.WriteFile(filepath.Join(directory, "check_groups", "checks.yml"), []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := config.Load(directory)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatal(err)
			}
		})
	}
	directory := testhelpers.ConfigFile(t, "check_groups: {app: {shell: 'true'}}")
	if err := os.WriteFile(filepath.Join(directory, "alert_destinations.yml"), []byte("destinations: {example: {webhook: {url: 'https://example.com'}, alert_severities: [warn]}}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(directory); err == nil || !strings.Contains(err.Error(), "destinations.example.remind_every: is required:") {
		t.Fatal(err)
	}
}

func TestListeningAddressesNameTheirHost(t *testing.T) {
	t.Parallel()
	for _, setting := range []string{"fleet_hub_role", "heartbeat_receiver"} {
		for _, address := range []string{"", ":8445", "0.0.0.0:8445", "10.0.0.5:8445", "[::]:8445"} {
			t.Run(setting+"/"+address, func(t *testing.T) {
				block := setting + ": {listen_on_address: '" + address + "'"
				if setting == "fleet_hub_role" {
					block += ", address_for_nodes: 'hub.example.com:8445', report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, keep_measurement_history_of_nodes_for: 7d, measurement_history_of_nodes_size_limit: 1GiB"
				} else {
					block += ", tls_certificate_file: /etc/mmwatchdog/secrets/cert.pem, tls_private_key_file: /etc/mmwatchdog/secrets/key.pem"
				}
				_, err := config.Load(testhelpers.ConfigFile(t, block+"}\ncheck_groups: {app: {check_every: 30s, time_limit_per_run: 5s, alert_after_failing_for: 1m, recovered_after_healthy_for: 30s, shell: 'true', alert_severity: warn}}\n"))
				if address == "" || address == ":8445" {
					if err == nil || !strings.Contains(err.Error(), setting+".listen_on_address:") || !strings.Contains(err.Error(), "10.0.0.5:8445") || !strings.Contains(err.Error(), "0.0.0.0:8445") || strings.Contains(err.Error(), ":5432") {
						t.Fatalf("expected listening-address examples: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
