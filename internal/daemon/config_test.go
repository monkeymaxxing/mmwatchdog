package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/testhelpers"
)

func TestRuntimeCredentialValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, field string }{
		{"valid", ""},
		{"missing TLS key", "heartbeat_receiver"},
		{"mismatched TLS key", "heartbeat_receiver"},
		{"missing heartbeat token", "check_groups.backup.heartbeat.expected_bearer_token_file"},
		{"invalid heartbeat token", "check_groups.backup.heartbeat.expected_bearer_token_file"},
		{"duplicate heartbeat token", "must use a different token for each heartbeat check group"},
		{"missing webhook token", "destinations.ops.webhook.bearer_token_file"},
		{"missing HTTP token", "check_groups.api.http.bearer_token_file"},
		{"invalid HTTP basic authentication", "check_groups.admin.http.basic_auth_file"},
		{"HTTP CA without certificates", "check_groups.api.http.trusted_certificate_authority_file"},
		{"missing TLS CA", "check_groups.mail.tls.trusted_certificate_authority_file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cert, key := testhelpers.TestCertificate(t, dir)
			token, other, basic := filepath.Join(dir, "backup-token"), filepath.Join(dir, "other-token"), filepath.Join(dir, "basic")
			for path, value := range map[string]string{token: "test-backup-token-value", other: "test-other-token-value", basic: "monitor:password"} {
				if err := os.WriteFile(path, []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			path := testhelpers.ConfigFile(t, fmt.Sprintf(`heartbeat_receiver:
  listen_on_address: 127.0.0.1:8444
  tls_certificate_file: %q
  tls_private_key_file: %q
check_groups:
  backup: {heartbeat: {max_time_between_heartbeats: 26h, expected_bearer_token_file: %q}}
  other: {heartbeat: {max_time_between_heartbeats: 26h, expected_bearer_token_file: %q}}
  api: {http: {url: 'https://10.0.0.5/health', bearer_token_file: %q, trusted_certificate_authority_file: %q}}
  admin: {http: {url: 'https://10.0.0.5/admin', basic_auth_file: %q}}
  mail: {tls: {fail_when_certificate_expires_within: 30d, address: 'mail.example.com:465', trusted_certificate_authority_file: %q}}
`, cert, key, token, other, other, cert, basic, cert))
			alerts := fmt.Sprintf("destinations:\n  ops: {remind_every: never, webhook: {url: https://example.com/events, bearer_token_file: %q}, alert_severities: [info, warn, sos]}\n", token)
			if err := os.WriteFile(filepath.Join(path, "alert_destinations.yml"), []byte(alerts), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			c.StateDir = filepath.Join(dir, "state")
			switch tc.name {
			case "missing TLS key":
				c.HeartbeatReceiver.TLSPrivateKeyFile = filepath.Join(dir, "absent")
			case "mismatched TLS key":
				_, c.HeartbeatReceiver.TLSPrivateKeyFile = testhelpers.TestCertificate(t, t.TempDir())
			case "missing heartbeat token":
				c.CheckGroups["backup"].Heartbeat.ExpectedBearerTokenFile = filepath.Join(dir, "absent")
			case "invalid heartbeat token":
				if err := os.WriteFile(token, []byte("short"), 0600); err != nil {
					t.Fatal(err)
				}
			case "duplicate heartbeat token":
				if err := os.WriteFile(other, []byte("test-backup-token-value"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing webhook token":
				c.Alerts.Destinations["ops"].Inputs.(*checksandalerts.Webhook).BearerTokenFile = filepath.Join(dir, "absent")
			case "missing HTTP token":
				c.CheckGroups["api"].Checks["http"].Inputs.(*checksandalerts.HTTP).BearerTokenFile = filepath.Join(dir, "absent")
			case "invalid HTTP basic authentication":
				if err := os.WriteFile(basic, []byte("no-separator"), 0600); err != nil {
					t.Fatal(err)
				}
			case "HTTP CA without certificates":
				c.CheckGroups["api"].Checks["http"].Inputs.(*checksandalerts.HTTP).TrustedCertificateAuthorityFile = token
			case "missing TLS CA":
				c.CheckGroups["mail"].Checks["tls"].Inputs.(*checksandalerts.TLS).TrustedCertificateAuthorityFile = filepath.Join(dir, "absent")
			}
			err = ValidateRuntime(c)
			if tc.field == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("expected %s error, got %v", tc.field, err)
			}
			if _, err := os.Stat(c.StateDir); !os.IsNotExist(err) {
				t.Fatal("validation touched daemon state", err)
			}
		})
	}
}

// A file that holds a password, a token, or a private key is for the
// account that reads it. mmwatchdog validate warns about each such file that the
// configuration names and that its group or other accounts can use.
func TestSecretFilePermissions(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("a-token"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		mode    os.FileMode
		access  checksandalerts.SecretFileAccess
		refusal string
	}{
		{0600, checksandalerts.SecretFileAccess{}, ""},
		{0603, checksandalerts.SecretFileAccess{}, ""},
		{0630, checksandalerts.SecretFileAccess{}, ""},
		{0640, checksandalerts.SecretFileAccess{}, "secret_readable_by_group: true"},
		{0604, checksandalerts.SecretFileAccess{}, "dangerously_let_every_account_read_secret: true"},
		{0640, checksandalerts.SecretFileAccess{SecretReadableByGroup: true}, ""},
		{0604, checksandalerts.SecretFileAccess{DangerouslyLetEveryAccountReadSecret: true}, ""},
		{0644, checksandalerts.SecretFileAccess{SecretReadableByGroup: true}, "dangerously_let_every_account_read_secret: true"},
		{0644, checksandalerts.SecretFileAccess{DangerouslyLetEveryAccountReadSecret: true}, "secret_readable_by_group: true"},
		{0644, checksandalerts.SecretFileAccess{SecretReadableByGroup: true, DangerouslyLetEveryAccountReadSecret: true}, ""},
	} {
		if err := os.Chmod(path, test.mode); err != nil {
			t.Fatal(err)
		}
		err := test.access.ValidateSecretFile(path)
		if test.refusal == "" {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		for _, part := range []string{path, fmt.Sprintf("%04o", test.mode), test.refusal, "chmod 600 " + path} {
			if err == nil || !strings.Contains(err.Error(), part) {
				t.Fatalf("mode %04o: expected %s, got %v", test.mode, part, err)
			}
		}
	}
}

func TestConfigStrict(t *testing.T) {
	t.Parallel()
	cases := []string{
		"check_groups: {a: {shell: true}}",
		"check_groups: {a: {shell: echo ok, typo: 1}}",
		"check_groups: {a: {shell: {script: echo ok, success_exit_codes: [0, 0]}}}",
		"check_groups: {a: {shell: {script: echo ok, success_exit_codes: [256]}}}",
		"check_groups: {a: {shell: {script: echo ok, success_exit_codes: []}}}",
		"check_groups: {a: {shell: {script: echo ok, env: {BASH_ENV: /tmp/startup}}}}",
		"check_groups: {a: {check_every: 1s, time_limit_per_run: 2s, shell: echo ok}}",
		"check_groups: {a: {check_every: 50ms, time_limit_per_run: 20ms, shell: echo ok}}",
		"check_groups: {a: {alert_after_failing_for: -1s, shell: echo ok}}",
		"check_groups: {a: {recovered_after_healthy_for: -1s, shell: echo ok}}",
		"check_groups: {a: {check_every: 1week, shell: echo ok}}",
		"check_groups: {a: {check_every: 1.5d, shell: echo ok}}",
		"check_groups: {a: {shell: echo ok}, a: {shell: echo bad}}",
		"check_groups: {a: &a {shell: echo ok}, b: *a}",
		"check_groups: {a: {http: {url: https://example.com, response_body_matches_regex: '['}}}",
		"check_groups: {a: {shell: !!str echo ok}}",
		"check_groups: {a: {shell: echo ok}}\n---\ncheck_groups: {}",
		"check_groups: {a: {'../bad': {}}}",
		"check_groups: {a: {checks: {r: {systemd_unit_state: {systemd_unit: test.service, fail_when: the_unit_is_not_active}, shell: echo ok}}}}",
		"check_groups: {a: {check_every: 1s, time_limit_per_run: 2s, shell: echo ok}}",
		// Heartbeats have no executions or confirmation periods.
		"heartbeat_receiver: {listen_on_address: '127.0.0.1:8444', tls_certificate_file: /c, tls_private_key_file: /k}\ncheck_groups: {b: {check_every: 1m, heartbeat: {max_time_between_heartbeats: 1h, expected_bearer_token_file: /t}}}",
		"heartbeat_receiver: {listen_on_address: '127.0.0.1:8444', tls_certificate_file: /c, tls_private_key_file: /k}\ncheck_groups: {b: {alert_after_failing_for: 1h, heartbeat: {max_time_between_heartbeats: 1h, expected_bearer_token_file: /t}}}",
		"heartbeat_receiver: {listen_on_address: '127.0.0.1:8444', tls_certificate_file: /c, tls_private_key_file: /k}\ncheck_groups: {b: {fail_when_measurement_above: {age_hours: 26}, heartbeat: {max_time_between_heartbeats: 1h, expected_bearer_token_file: /t}}}",
		// Ports must be usable.
		"heartbeat_receiver: {listen_on_address: '127.0.0.1:84444', tls_certificate_file: /c, tls_private_key_file: /k}\ncheck_groups: {a: {shell: echo ok}}",
		"heartbeat_receiver: {listen_on_address: '127.0.0.1:', tls_certificate_file: /c, tls_private_key_file: /k}\ncheck_groups: {a: {shell: echo ok}}",
		"check_groups: {a: {tcp: '127.0.0.1:99999'}}",
		"check_groups: {a: {tls: {fail_when_certificate_expires_within: 30d, address: 'example.com:0'}}}",
		// Defaults fill only omitted settings.
		"check_groups: {a: {checks: {r: {systemd_unit_state: {systemd_unit: a.service, fail_when: the_unit_is_not_active}}}, restart_systemd_unit: {min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, systemd_unit: a.service, triggered_by_checks: [r], max_attempts: 0}}}",
		"check_groups: {a: {checks: {r: {systemd_unit_state: {systemd_unit: a.service, fail_when: the_unit_is_not_active}}}, restart_systemd_unit: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, systemd_unit: a.service, triggered_by_checks: []}}}",
		"check_groups: {a: {http: {url: https://example.com, expected_http_status_codes: []}}}",
		"check_groups: {a: {http: {url: https://example.com, expected_http_status_codes: [99]}}}",
		"check_groups: {a: {http: {url: https://example.com, expected_http_status_codes: [200, 200]}}}",
		"check_groups: {a: {http: {url: https://example.com, expected_http_status_codes: 200}}}",
		// Recovery commands take the shell options and the restart budget rules.
		"check_groups: {a: {shell: 'true', recovery_command: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, triggered_by_checks: [shell], time_limit_per_attempt: 1m}}}",
		"check_groups: {a: {shell: 'true', recovery_command: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, triggered_by_checks: [shell], script: fix, time_limit_per_attempt: 0s}}}",
		"check_groups: {a: {shell: 'true', recovery_command: {min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, time_limit_per_attempt: 1m, triggered_by_checks: [shell], script: fix, max_attempts: 0}}}",
		"check_groups: {a: {shell: 'true', recovery_command: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, time_limit_per_attempt: 1m, script: fix, triggered_by_checks: []}}}",
		"check_groups: {a: {shell: 'true', recovery_command: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, time_limit_per_attempt: 1m, script: fix, triggered_by_checks: [service]}}}",
		"check_groups: {a: {checks: {r: {systemd_unit_state: {systemd_unit: a.service, fail_when: the_unit_is_not_active}}}, recovery_command: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, time_limit_per_attempt: 1m, script: fix, triggered_by_checks: [crash_loop]}}}",
		"check_groups: {a: {shell: 'true', recovery_command: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, time_limit_per_attempt: 1m, triggered_by_checks: [shell], script: fix, run_from_directory: relative}}}",
		"check_groups: {a: {shell: 'true', recovery_command: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, time_limit_per_attempt: 1m, triggered_by_checks: [shell], script: fix, env: {BASH_ENV: /tmp/startup}}}}",
		"check_groups: {a: {shell: 'true', recovery_command: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, time_limit_per_attempt: 1m, triggered_by_checks: [shell], script: fix, success_exit_codes: [0]}}}",
		"check_groups: {a: {only_record_readings: true, node_resource: cpu, recovery_command: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, time_limit_per_attempt: 1m, script: fix}}}",
		"check_groups: {a: {checks: {cpu: {only_record_readings: true, node_resource: cpu}}, recovery_command: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, time_limit_per_attempt: 1m, script: fix, triggered_by_checks: [cpu]}}}",
		"heartbeat_receiver: {listen_on_address: '127.0.0.1:8444', tls_certificate_file: /c, tls_private_key_file: /k}\ncheck_groups: {b: {heartbeat: {max_time_between_heartbeats: 1h, expected_bearer_token_file: /t}, recovery_command: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, time_limit_per_attempt: 1m, triggered_by_checks: [heartbeat], script: fix}}}",
		// HTTP secrets come from files, never configuration.
		"check_groups: {a: {http: {url: 'https://user:secret@example.com'}}}",
		"check_groups: {a: {http: {url: https://example.com, headers: {authorization: 'Bearer secret'}}}}",
		"check_groups: {a: {http: {url: https://example.com, bearer_token_file: /t, basic_auth_file: /b}}}",
		"check_groups: {a: {http: {url: https://example.com, bearer_token_file: secrets/t}}}",
		"check_groups: {a: {http: {url: https://example.com, basic_auth_file: secrets/b}}}",
		"check_groups: {a: {http: {url: https://example.com, trusted_certificate_authority_file: ca.pem}}}",
		"check_groups: {a: {tls: {fail_when_certificate_expires_within: 30d, address: 'example.com:443', trusted_certificate_authority_file: ca.pem}}}",
		"check_groups: {a: {http: {url: https://example.com, method: POST}}}",
		"check_groups: {a: {http: {url: https://example.com, method: HEAD, response_body_contains: ready}}}",
		"check_groups: {a: {http: {url: https://example.com, headers: {X-Check: a, x-check: b}}}}",
		"check_groups: {a: {http: {url: https://example.com, headers: {'Bad Header': a}}}}",
		"check_groups: {a: {http: {url: https://example.com, headers: {X-Check: \"a\\nb\"}}}}",
	}
	for _, text := range cases {
		t.Run(text, func(t *testing.T) {
			if _, e := config.Load(testhelpers.ConfigFile(t, text)); e == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}

// A check group states its four timings; a duration can count days. The
// shell options keep their defaults, and an explicit list of exit codes
// replaces the default.
func TestConfigDefaultsAndExplicitCodes(t *testing.T) {
	t.Parallel()
	c, e := config.Load(testhelpers.ConfigFileAsWritten(t, `alert_severity_when_this_node_goes_silent: sos
alert_severity_when_this_node_has_rebooted: info
keep_measurement_history_for: 90d
measurement_history_size_limit: 2GiB
check_groups:
  replica:
    check_every: 1m
    time_limit_per_run: 10s
    alert_after_failing_for: 0s
    recovered_after_healthy_for: 2d
    shell:
      script: exit 2
      success_exit_codes: [2]
    alert_severity: warn
keep_undelivered_alerts_for: 24h
undelivered_alerts_content_size_limit: 8MiB
`))
	if e != nil {
		t.Fatal(e)
	}
	ch := c.CheckGroups["replica"]
	s := ch.Checks["shell"].Inputs.(*checksandalerts.Shell)
	if ch.Timing != (checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "10s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "2d"}) || ch.RecoveredAfterHealthyFor.Value() != 48*time.Hour || s.RunAsUserAccount != "mmwatchdog" || s.RunFromDirectory != "/" || len(s.SuccessExitCodes) != 1 || s.SuccessExitCodes[0] != 2 {
		t.Fatalf("%+v %+v", ch, s)
	}
	if c.AlertSeverityWhenThisNodeHasRebooted != "info" {
		t.Fatal(c.AlertSeverityWhenThisNodeHasRebooted)
	}
	if c.KeepMeasurementHistoryFor.Value() != 90*24*time.Hour || c.MeasurementHistorySizeLimit.Bytes() != 2*1024*1024*1024 {
		t.Fatal(c.KeepMeasurementHistoryFor, c.MeasurementHistorySizeLimit)
	}
}

// Every check group that MMWatchdog runs states its four timings, and every node
// its two severities and how much measurement history it keeps: none of
// them has a default.
func TestRequiredSettingsHaveNoDefault(t *testing.T) {
	t.Parallel()
	node := map[string]string{"alert_severity_when_this_node_goes_silent": "sos", "alert_severity_when_this_node_has_rebooted": "warn",
		"keep_measurement_history_for": "7d", "measurement_history_size_limit": "256MiB", "keep_undelivered_alerts_for": "24h", "undelivered_alerts_content_size_limit": "8MiB"}
	missing := map[string]string{
		"keep_undelivered_alerts_for":           "keep_undelivered_alerts_for: is required:",
		"undelivered_alerts_content_size_limit": "undelivered_alerts_content_size_limit: is required:",
		"keep_measurement_history_for":          "keep_measurement_history_for: is required: how long this node keeps the readings of its measurements, such as 7d",
		"measurement_history_size_limit":        "measurement_history_size_limit: is required: how much of this node's disk the readings of its measurements may take, such as 256MiB",
	}
	group := map[string]string{"check_every": "30s", "time_limit_per_run": "5s", "alert_after_failing_for": "1m", "recovered_after_healthy_for": "30s"}
	text := func(without string) string {
		text := ""
		for _, key := range checksandalerts.Names(node) {
			if key != without {
				text += key + ": " + node[key] + "\n"
			}
		}
		text += "check_groups:\n  app:\n"
		for _, key := range checksandalerts.Names(group) {
			if key != without {
				text += "    " + key + ": " + group[key] + "\n"
			}
		}
		return text + "    shell: 'true'\n    alert_severity: warn\n"
	}
	if _, e := config.Load(testhelpers.ConfigFileAsWritten(t, text(""))); e != nil {
		t.Fatal(e)
	}
	for _, key := range checksandalerts.Names(node) {
		if key == "keep_undelivered_alerts_for" || key == "undelivered_alerts_content_size_limit" {
			continue
		}
		want := missing[key]
		if want == "" {
			want = key + ": required: info, warn, or sos"
		}
		if _, e := config.Load(testhelpers.ConfigFileAsWritten(t, text(key))); e == nil || !strings.Contains(e.Error(), want) {
			t.Fatal(key, e)
		}
	}
	// Both settings of the measurement history have a value that MMWatchdog can use.
	for line, want := range map[string]string{
		"keep_measurement_history_for: 0s":      "keep_measurement_history_for: must be positive",
		"keep_measurement_history_for: forever": "a duration such as 30s, 5m, 26h, or 7d",
		"measurement_history_size_limit: 0MiB":  "a size: a whole number with MiB or GiB, such as 256MiB",
		"measurement_history_size_limit: 256":   "a size",
		"measurement_history_size_limit: 256MB": "a size: a whole number with MiB or GiB, such as 256MiB",
	} {
		key, _, _ := strings.Cut(line, ":")
		if _, e := config.Load(testhelpers.ConfigFileAsWritten(t, line+"\n"+text(key))); e == nil || !strings.Contains(e.Error(), want) {
			t.Fatal(line, e)
		}
	}
	for _, key := range checksandalerts.Names(group) {
		if _, e := config.Load(testhelpers.ConfigFileAsWritten(t, text(key))); e == nil || !strings.Contains(e.Error(), "check_groups.app."+key+": is required: how ") || !strings.Contains(e.Error(), "checks.yml:8:") {
			t.Fatal(key, e)
		}
	}
}

// Every unit check names its systemd unit, and so does the restart;
// nothing is generated. One check group can hold checks of several units
// and of the node, and a machine check can trigger the restart of a unit.
func TestSystemdUnitChecks(t *testing.T) {
	t.Parallel()
	timings := "    check_every: 30s\n    time_limit_per_run: 5s\n    alert_after_failing_for: 1m\n    recovered_after_healthy_for: 30s\n"
	c, err := config.Load(testhelpers.ConfigFile(t, "check_groups:\n  app:\n"+timings+`    checks:
      running: {systemd_unit_state: {systemd_unit: app.service, fail_when: the_unit_is_not_active}, alert_severity: sos}
      crash_loop: {systemd_unit_crash_loop: {systemd_unit: app.service, automatic_restarts_above: 2, automatic_restarts_within: 1h}, alert_severity: warn}
      worker_memory: {systemd_unit_resource: {systemd_unit: worker.service, resource: memory}, fail_when_measurement_above: {memory_used_mib: 512}, alert_severity: warn}
      memory: {node_resource: memory, fail_when_measurement_above: {memory_used_percent: 95}, alert_severity: warn}
    restart_systemd_unit: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, systemd_unit: app.service, triggered_by_checks: [running, memory]}
  watched_only:
`+timings+`    checks:
      health: {http: {url: 'http://127.0.0.1/'}, alert_severity: warn}
  direct:
`+timings+`    systemd_unit_state:
      systemd_unit: direct.service
      fail_when: the_unit_is_not_active
    alert_severity: sos
`))
	if err != nil {
		t.Fatal(err)
	}
	app := c.CheckGroups["app"]
	if r, _ := app.Checks["crash_loop"].Inputs.(*checksandalerts.SystemdUnitCrashLoop); r == nil || r.AutomaticRestartsAbove != 2 || r.AutomaticRestartsWithin != "1h" || r.SystemdUnit != "app.service" || app.Checks["running"].SystemdUnitName() != "app.service" {
		t.Fatalf("%+v", app.Checks)
	}
	if r := app.RestartSystemdUnit; r.SystemdUnit != "app.service" || !reflect.DeepEqual(r.TriggeredByChecks, []string{"running", "memory"}) || r.Attempts != "3" || r.MinTimeBetweenAttempts != "1m" || r.ResetAttemptsAfterHealthyFor != "10m" {
		t.Fatalf("%+v", r)
	}
	if !reflect.DeepEqual(app.SystemdUnits(), []string{"app.service", "worker.service"}) || app.RestartUnit() != "app.service" || c.CheckGroups["watched_only"].RestartUnit() != "" || len(c.CheckGroups["watched_only"].SystemdUnits()) != 0 {
		t.Fatal(app.SystemdUnits())
	}
	defs := config.CheckDefinitions(c)
	if len(defs) != 6 || defs["direct/systemd_unit_state"].Check.SystemdUnitName() != "direct.service" || defs["app/crash_loop"].Severity.AlertSeverity != "warn" {
		t.Fatalf("%+v", defs)
	}
	if c.CheckSource("app", "crash_loop").Line != 9 || c.CheckSource("direct", "systemd_unit_state").Line != 25 {
		t.Fatal(c.CheckSource("app", "crash_loop"), c.CheckSource("direct", "systemd_unit_state"))
	}
	for _, tc := range []struct{ text, field string }{
		{"check_groups: {a: {checks: {r: {systemd_unit_state: {fail_when: the_unit_is_not_active}}}}}", "check_groups.a.checks.r.systemd_unit_state.systemd_unit: is required"},
		{"check_groups: {a: {checks: {r: {systemd_unit_state: {systemd_unit: a, fail_when: the_unit_is_not_active}}}}}", "check_groups.a.checks.r.systemd_unit_state.systemd_unit: must be the name of a systemd unit with its kind, such as myapp.service, backup.timer, or myapp.path"},
		{"check_groups: {a: {checks: {m: {only_record_readings: true, systemd_unit_resource: {resource: memory}}}}}", "check_groups.a.checks.m.systemd_unit_resource.systemd_unit: is required"},
		{"check_groups: {a: {checks: {m: {only_record_readings: true, systemd_unit_resource: {systemd_unit: a, resource: memory}}}}}", "check_groups.a.checks.m.systemd_unit_resource.systemd_unit: must be a systemd .service unit name"},
		{"check_groups: {a: {checks: {c: {systemd_unit_crash_loop: {systemd_unit: a.service, automatic_restarts_above: 1.5, automatic_restarts_within: 1h}}}}}", "expected an integer"},
		{"check_groups: {a: {checks: {c: {systemd_unit_crash_loop: {systemd_unit: a.service, automatic_restarts_above: -1, automatic_restarts_within: 1h}}}}}", "check_groups.a.checks.c.systemd_unit_crash_loop.automatic_restarts_above"},
		{"check_groups: {a: {checks: {c: {systemd_unit_crash_loop: {systemd_unit: a.service, automatic_restarts_above: 2}}}}}", "check_groups.a.checks.c.systemd_unit_crash_loop.automatic_restarts_within: is required"},
		{"check_groups: {a: {checks: {c: {systemd_unit_crash_loop: {systemd_unit: a.service, automatic_restarts_within: 1h}}}}}", "check_groups.a.checks.c.systemd_unit_crash_loop.automatic_restarts_above: is required"},
		{"check_groups: {a: {checks: {c: {systemd_unit_crash_loop: {automatic_restarts_above: 2, automatic_restarts_within: 1h}}}}}", "check_groups.a.checks.c.systemd_unit_crash_loop.systemd_unit: is required"},
		{"check_groups: {a: {checks: {c: {systemd_unit_crash_loop: {systemd_unit: a.service, automatic_restarts_above: 2, automatic_restarts_within: 1h}}}, recovery_command: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, time_limit_per_attempt: 1m, script: fix}}}", "check_groups.a.recovery_command.triggered_by_checks: is required: the checks whose failure calls for the command"},
		{"check_groups: {a: {checks: {c: {systemd_unit_crash_loop: {systemd_unit: a.service, automatic_restarts_above: 2, within: 1h}}}}}", `unknown field "within"`},
		{"check_groups: {a: {checks: {h: {shell: 'true'}}, restart_systemd_unit: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, systemd_unit: a.service}}}", "check_groups.a.restart_systemd_unit.triggered_by_checks: is required"},
		{"check_groups: {a: {shell: 'true', restart_systemd_unit: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, triggered_by_checks: [shell]}}}", "check_groups.a.restart_systemd_unit.systemd_unit: is required"},
		{"check_groups: {a: {shell: 'true', restart_systemd_unit: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, systemd_unit: a, triggered_by_checks: [shell]}}}", "check_groups.a.restart_systemd_unit.systemd_unit: must be a systemd .service unit name"},
		{"check_groups: {a: {checks: {r: {systemd_unit_state: {systemd_unit: a.service, fail_when: the_unit_is_not_active}}}, restart_systemd_unit: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, systemd_unit: a.service, triggered_by_checks: [missing]}}}", "check_groups.a.restart_systemd_unit.triggered_by_checks.0"},
		{"check_groups: {a: {service: a.service}}", `unknown field "service"`},
	} {
		if _, err := config.Load(testhelpers.ConfigFile(t, tc.text)); err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Fatalf("%s: %v", tc.text, err)
		}
	}
}

// HTTP request options load as written; the method defaults to GET and the
// accepted status codes to 200.
func TestHTTPRequestSettings(t *testing.T) {
	t.Parallel()
	c, e := config.Load(testhelpers.ConfigFile(t, `check_groups:
  api: {http: {url: 'https://10.0.0.5/health', headers: {Host: app.example.com, X-Check: mmwatchdog}, bearer_token_file: /etc/mmwatchdog/secrets/api, trusted_certificate_authority_file: /etc/mmwatchdog/secrets/ca.pem}}
  ping: {http: {url: 'https://10.0.0.5/ping', method: HEAD, basic_auth_file: /etc/mmwatchdog/secrets/ping, expected_http_status_codes: [200, 302]}}
`))
	if e != nil {
		t.Fatal(e)
	}
	api, ping := c.CheckGroups["api"].Checks["http"].Inputs.(*checksandalerts.HTTP), c.CheckGroups["ping"].Checks["http"].Inputs.(*checksandalerts.HTTP)
	if api.Method != "GET" || api.Headers["Host"] != "app.example.com" || api.TrustedCertificateAuthorityFile != "/etc/mmwatchdog/secrets/ca.pem" || ping.Method != "HEAD" || ping.BasicAuthFile != "/etc/mmwatchdog/secrets/ping" {
		t.Fatalf("%+v %+v", api, ping)
	}
	if !reflect.DeepEqual(api.ExpectedHTTPStatusCodes, []int{200}) || !reflect.DeepEqual(ping.ExpectedHTTPStatusCodes, []int{200, 302}) {
		t.Fatal(api.ExpectedHTTPStatusCodes, ping.ExpectedHTTPStatusCodes)
	}
}

// A recovery command states its triggers and attempt budget. Its account
// and working directory keep the safe defaults unless explicitly stated.
func TestRecoveryCommandSettings(t *testing.T) {
	t.Parallel()
	c, e := config.Load(testhelpers.ConfigFile(t, `check_groups:
  root_filesystem:
    node_filesystem: /
    fail_when_measurement_above: {space_used_percent: 90}
    recovery_command:
      max_attempts: 3
      min_time_between_attempts: 1m
      reset_attempts_after_healthy_for: 10m
      time_limit_per_attempt: 1m
      triggered_by_checks: [node_filesystem]
      script: find /var/cache/myapp -type f -mtime +1 -delete
  worker:
    checks:
      running: {systemd_unit_state: {systemd_unit: worker.service, fail_when: the_unit_is_not_active}}
      restarts: {systemd_unit_crash_loop: {systemd_unit: worker.service, automatic_restarts_above: 2, automatic_restarts_within: 1h}}
      cpu: {only_record_readings: true, systemd_unit_resource: {systemd_unit: worker.service, resource: cpu}}
      queue: {shell: 'test -z "$(ls /srv/queue)"'}
    recovery_command: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, time_limit_per_attempt: 1m, triggered_by_checks: [queue, running], script: /usr/local/bin/clear-queue}
  crashy:
    checks:
      crash_loop: {systemd_unit_crash_loop: {systemd_unit: crashy.service, automatic_restarts_above: 2, automatic_restarts_within: 1h}}
    recovery_command: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, time_limit_per_attempt: 1m, script: /usr/local/bin/clear-lock, triggered_by_checks: [crash_loop]}
  myapp:
    checks:
      health: {http: {url: 'http://127.0.0.1:8080/health'}}
    recovery_command: {script: /usr/local/bin/clear-cache, run_as_user_account: myapp, run_from_directory: /srv, env: {CACHE: /srv/cache}, time_limit_per_attempt: 2m, triggered_by_checks: [health], max_attempts: 2, min_time_between_attempts: 10m, reset_attempts_after_healthy_for: 1h}
`))
	if e != nil {
		t.Fatal(e)
	}
	filesystem, worker, myapp := c.CheckGroups["root_filesystem"].RecoveryCommand, c.CheckGroups["worker"].RecoveryCommand, c.CheckGroups["myapp"].RecoveryCommand
	if !reflect.DeepEqual(filesystem.TriggeredByChecks, []string{"node_filesystem"}) || filesystem.RunAsUserAccount != "mmwatchdog" || filesystem.RunFromDirectory != "/" || filesystem.TimeLimitPerAttempt != "1m" || filesystem.Attempts != "3" || filesystem.MinTimeBetweenAttempts != "1m" || filesystem.ResetAttemptsAfterHealthyFor != "10m" {
		t.Fatalf("explicit recovery settings and account defaults: %+v", filesystem)
	}
	// The explicit trigger list leaves out the crash loop and readings-only check.
	if !reflect.DeepEqual(worker.TriggeredByChecks, []string{"queue", "running"}) || !reflect.DeepEqual(c.CheckGroups["crashy"].RecoveryCommand.TriggeredByChecks, []string{"crash_loop"}) {
		t.Fatal("explicit triggered_by_checks names the recovery checks:", worker.TriggeredByChecks)
	}
	if !reflect.DeepEqual(myapp.TriggeredByChecks, []string{"health"}) || myapp.RunAsUserAccount != "myapp" || myapp.RunFromDirectory != "/srv" || myapp.Env["CACHE"] != "/srv/cache" || myapp.TimeLimitPerAttempt != "2m" || myapp.Attempts != "2" || myapp.MinTimeBetweenAttempts != "10m" || myapp.ResetAttemptsAfterHealthyFor != "1h" {
		t.Fatalf("authored: %+v", myapp)
	}
}
func TestConfigReportsLine(t *testing.T) {
	t.Parallel()
	_, e := config.Load(testhelpers.ConfigFile(t, "check_groups:\n  app:\n    shell:\n      script: echo ok\n      surprise: false\n"))
	if e == nil || !strings.Contains(e.Error(), ":5:") {
		t.Fatal(e)
	}
}

func TestSemanticErrorsIdentifyFieldsAndFragments(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text, field string
		line        int
	}{
		{"socket: relative\ncheck_groups: {a: {shell: true | cat}}", "socket", 1},
		{"check_groups:\n  app:\n    restart_systemd_unit:\n      max_attempts: 3\n      reset_attempts_after_healthy_for: 10m\n      systemd_unit: app.service\n      triggered_by_checks: [r]\n      min_time_between_attempts: 0s\n    checks: {r: {systemd_unit_state: {systemd_unit: app.service, fail_when: the_unit_is_not_active}}}\n", "check_groups.app.restart_systemd_unit.min_time_between_attempts", 8},
		{"check_groups:\n  app:\n    shell: 'true'\n    recovery_command:\n      max_attempts: 3\n      reset_attempts_after_healthy_for: 10m\n      time_limit_per_attempt: 1m\n      triggered_by_checks: [shell]\n      script: fix\n      min_time_between_attempts: 0s\n", "check_groups.app.recovery_command.min_time_between_attempts", 10},
		{"check_groups:\n  app:\n    http:\n      url: https://example.com\n      response_body_matches_regex: '['\n", "check_groups.app.http.response_body_matches_regex", 5},
		{"check_groups:\n  app:\n    checks:\n      port:\n        tcp: invalid\n", "check_groups.app.checks.port.tcp", 5},
		{"check_groups:\n  app:\n    shell:\n      script: true\n      success_exit_codes: [0, 256]\n", "expected a string", 4},
		{"check_groups:\n  app:\n    shell:\n      script: 'true'\n      success_exit_codes: [0, 256]\n", "check_groups.app.shell.success_exit_codes", 5},
		{"check_groups:\n  app:\n    check_every: 1ms\n    shell: 'true'\n", "check_groups.app.check_every", 3},
		{"check_groups:\n  app:\n    restart_systemd_unit:\n      max_attempts: 3\n      min_time_between_attempts: 1m\n      reset_attempts_after_healthy_for: 10m\n      systemd_unit: app.service\n      triggered_by_checks: [missing]\n    checks: {r: {systemd_unit_state: {systemd_unit: app.service, fail_when: the_unit_is_not_active}}}\n", "check_groups.app.restart_systemd_unit.triggered_by_checks.0", 8},
		{"check_groups:\n  app:\n    shell: 'true'\n    recovery_command:\n      max_attempts: 3\n      min_time_between_attempts: 1m\n      reset_attempts_after_healthy_for: 10m\n      time_limit_per_attempt: 1m\n      script: fix\n      triggered_by_checks: [missing]\n", "check_groups.app.recovery_command.triggered_by_checks.0", 10},
		{"check_groups:\n  app:\n    node_resource: memory\n    fail_when_measurement_above: {memory_used_percent: .inf}\n", "finite number", 4},
		{"check_groups:\n  app:\n    node_resource: memory\n    fail_when_measurement_above:\n      load_average_5m: 5\n", "check_groups.app.fail_when_measurement_above.load_average_5m: this check has no such measurement; it measures memory_used_percent", 5},
		{"check_groups:\n  app:\n    node_resource: load\n    fail_when_measurement_below:\n      load_average_1m_per_core: -1\n", "check_groups.app.fail_when_measurement_below.load_average_1m_per_core: expected a number of 0 or more", 5},
		{"check_groups:\n  app:\n    only_record_readings: true\n    node_filesystem: srv\n", "check_groups.app.node_filesystem", 4},
		{"check_groups:\n  app:\n    checks:\n      api:\n        http:\n          url: https://example.com\n          expected_http_status_codes: [200, 600]\n", "check_groups.app.checks.api.http.expected_http_status_codes", 7},
		{"check_groups:\n  app:\n    checks:\n      unit:\n        only_record_readings: true\n        systemd_unit_resource:\n          systemd_unit: app.service\n          resource: disk\n", "check_groups.app.checks.unit.systemd_unit_resource.resource: must be cpu, memory, or processes", 8},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			path := testhelpers.ConfigFile(t, tc.text)
			_, err := config.Load(path)
			if err == nil || !strings.Contains(err.Error(), filepath.Join(path, map[bool]string{true: "mmwatchdog_node_settings.yml", false: "check_groups/checks.yml"}[tc.field == "socket"])+":"+strconv.Itoa(tc.line)+":") || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("%s: %v", tc.text, err)
			}
		})
	}

}

// A setting of the wrong shape gets an error that names the file, the line
// and column, and the path of the setting, and says what was expected and
// what was found.
func TestShapeErrorsNameThePathOfTheSetting(t *testing.T) {
	t.Parallel()
	check := func(name, module string) string {
		return "check_groups:\n  web:\n    checks:\n      " + name + ":\n        " + module + "\n"
	}
	dns := "dns:\n          dns_server: '10.0.0.2:53'\n          record_name: app.example.com\n          record_type: A\n          expected_record_values: 10.0.0.5"
	for _, tc := range []struct{ groups, want string }{
		{check("ssh_answers", "ssh: {address: 'web01:22'}"), "checks.yml:5:9: check_groups.web.checks.ssh_answers.ssh: expected a string, found a mapping"},
		{check("socket_answers", "unix_socket: {path: /run/myapp.sock}"), "checks.yml:5:9: check_groups.web.checks.socket_answers.unix_socket: expected a string, found a mapping"},
		{check("web01_answers", "ping: {host: web01}"), "checks.yml:5:9: check_groups.web.checks.web01_answers.ping: expected a string, found a mapping"},
		{check("status_answers", "udp: 127.0.0.1:7001"), `checks.yml:5:9: check_groups.web.checks.status_answers.udp: expected a mapping, found the string "127.0.0.1:7001"`},
		{check("port_is_open", "tcp: [127.0.0.1:80]"), "checks.yml:5:9: check_groups.web.checks.port_is_open.tcp: expected a mapping, or an address as host:port, found a list"},
		{check("name_resolves", dns), `checks.yml:9:11: check_groups.web.checks.name_resolves.dns.expected_record_values: expected a list, found the string "10.0.0.5"`},
		{check("mailboxes_answer", "imap: mail.example.com:993"), `checks.yml:5:9: check_groups.web.checks.mailboxes_answer.imap: expected a mapping, found the string "mail.example.com:993"`},
		{check("mailboxes_answer", "pop3: mail.example.com:995"), `checks.yml:5:9: check_groups.web.checks.mailboxes_answer.pop3: expected a mapping, found the string "mail.example.com:995"`},
		{check("mail_is_accepted", "smtp: mail.example.com:587"), `checks.yml:5:9: check_groups.web.checks.mail_is_accepted.smtp: expected a mapping, found the string "mail.example.com:587"`},
		{check("certificate_is_valid", "tls: {fail_when_certificate_expires_within: 30d, host_port: 'example.com:443'}"), `checks.yml:5:58: check_groups.web.checks.certificate_is_valid.tls.host_port: unknown field "host_port": the inputs of the module tls are address, server_name, trusted_certificate_authority_file, fail_when_certificate_expires_within`},
		{check("certificate_is_valid", "tls: {address: 'example.com:443', fail_when_certificate_expires_within: -1d}"), `checks.yml:5:43: check_groups.web.checks.certificate_is_valid.tls.fail_when_certificate_expires_within: expected a duration of 0s or more, such as 30s, 5m, 26h, or 7d, found the string "-1d"`},
		{check("certificate_is_valid", "tls: {address: 'example.com:443', fail_when_certificate_expires_within: -720h}"), `checks.yml:5:43: check_groups.web.checks.certificate_is_valid.tls.fail_when_certificate_expires_within: expected a duration of 0s or more, such as 30s, 5m, 26h, or 7d, found the string "-720h"`},
		{check("certificate_is_valid", "tls: {address: 'example.com:443', fail_when_certificate_expires_within: a month}"), `checks.yml:5:43: check_groups.web.checks.certificate_is_valid.tls.fail_when_certificate_expires_within: expected a duration such as 30s, 5m, 26h, or 7d, found the string "a month"`},
		{check("myapp_runs", "shell: !!str 'true'"), "checks.yml:5:9: check_groups.web.checks.myapp_runs.shell: expected a value without explicit YAML tags, found the explicit YAML tag !!str"},
		{"check_groups:\n  web:\n    checks:\n      a: &shared {shell: 'true'}\n      b: *shared\n", "checks.yml:4:7: check_groups.web.checks.a: expected a value without YAML aliases or anchors, found the YAML anchor &shared"},
		{"check_groups:\n  web:\n    checks:\n      <<: {a: {shell: 'true'}}\n", "checks.yml:4:7: check_groups.web.checks: expected an ordinary string key, found the YAML merge key <<"},
		{"check_groups:\n  web:\n    checks:\n      myapp_runs:\n        shell: 'true'\n        shell: 'false'\n", `checks.yml:6:9: check_groups.web.checks.myapp_runs.shell: duplicate key "shell": it is also written on line 5`},
		{"check_groups:\n  web:\n    shell: 'true'\n    recovery_command: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, time_limit_per_attempt: 1m, script: fix, triggered_by_checks: [7]}\n", "checks.yml:4:174: check_groups.web.recovery_command.triggered_by_checks.0: expected a string, found the whole number 7"},
		{"check_groups:\n  - web\n", "checks.yml:1:1: check_groups: expected a mapping, found a list"},
	} {
		directory := testhelpers.ConfigFileAsWritten(t, tc.groups)
		if _, e := config.Load(directory); e == nil || e.Error() != filepath.Join(directory, "check_groups", tc.want) {
			t.Errorf("%s:\n got %v\nwant %s", tc.groups, e, tc.want)
		}
	}
	// A setting of the node and an alert destination.
	directory := testhelpers.ConfigFileAsWritten(t, "measurement_history_size_limit: 256MB\ncheck_groups: {}\n")
	if _, e := config.Load(directory); e == nil || e.Error() != filepath.Join(directory, `mmwatchdog_node_settings.yml:1:1: measurement_history_size_limit: expected a size: a whole number with MiB or GiB, such as 256MiB, found the string "256MB"`) {
		t.Error(e)
	}
	for destinations, want := range map[string]string{
		"destinations:\n  log:\n    remind_every: never\n    system_log:\n    alert_severities: [sos]\n":                    "alert_destinations.yml:4:5: destinations.log.system_log: the module system_log takes no inputs and is written system_log: {}",
		"destinations:\n  log:\n    remind_every: never\n    system_log: {facility: daemon}\n    alert_severities: [sos]\n": `alert_destinations.yml:4:18: destinations.log.system_log.facility: unknown field "facility": the module system_log takes no inputs`,
	} {
		directory := testhelpers.ConfigFileAsWritten(t, "check_groups: {}\n")
		if err := os.WriteFile(filepath.Join(directory, "alert_destinations.yml"), []byte(destinations), 0600); err != nil {
			t.Fatal(err)
		}
		if _, e := config.Load(directory); e == nil || e.Error() != filepath.Join(directory, want) {
			t.Errorf("%s:\n got %v\nwant %s", destinations, e, want)
		}
	}
}

func TestMovingConfigPreservesIncidentAndOriginalSource(t *testing.T) {
	t.Parallel()
	p := testhelpers.ConfigFile(t, "check_groups:\n  app:\n    alert_after_failing_for: 0s\n    shell: 'false'\n")
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	s := incidents.FreshSnapshot()
	now := time.Now()
	s.Reconcile(c, config.CheckDefinitions(c), now)
	st := s.Checks["app/shell"]
	st.Observe(checksandalerts.Observation{Status: "failed", At: now, Message: "failed"}, c.CheckGroups["app"].Timing, false)
	id := st.FailureIncident.Incident
	testhelpers.WriteConfigText(p, "# new comment\ncheck_groups:\n  app:\n    alert_after_failing_for: 0s\n    shell: 'false'\n")
	c, err = config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if events := s.Reconcile(c, config.CheckDefinitions(c), now); len(events) != 0 {
		t.Fatal(events)
	}
	if st.FailureIncident.Incident != id || st.Source.Line != 5 || st.FailureIncident.Evidence.Source.Line != 4 {
		t.Fatalf("%+v %+v", st, st.FailureIncident.Evidence)
	}
}

func TestConfigurationDirectory(t *testing.T) {
	t.Parallel()
	directory := testhelpers.ConfigFile(t, "check_groups: {cpu: {only_record_readings: true, node_resource: cpu}}\n")
	second := filepath.Join(directory, "check_groups", "app.yml")
	if err := os.WriteFile(second, []byte("check_groups: {app: {"+testhelpers.FlowTimings+", shell: 'true', alert_severity: warn}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(directory)
	if err != nil || len(c.CheckGroups) != 2 {
		t.Fatal(c, err)
	}
	if err := os.WriteFile(second, []byte("check_groups: {cpu: {"+testhelpers.FlowTimings+", shell: 'true', alert_severity: warn}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(directory); err == nil || !strings.Contains(err.Error(), second+":1:") || !strings.Contains(err.Error(), filepath.Join(directory, "check_groups", "checks.yml")+":1:") {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(directory); err == nil || !strings.Contains(err.Error(), second+": expected a check_groups: mapping") {
		t.Fatal(err)
	}
	os.Remove(second)
	for _, name := range []string{"wrong.yaml", "notes.txt", "hidden"} {
		path := filepath.Join(directory, "check_groups", name)
		os.WriteFile(path, []byte("check_groups: {}"), 0600)
		if _, err := config.Load(directory); err == nil || !strings.Contains(err.Error(), path) {
			t.Fatal(name, err)
		}
		os.Remove(path)
	}
	os.WriteFile(filepath.Join(directory, "check_groups", "checks.yml"), []byte("check_groups: {}"), 0600)
	if _, err := config.Load(directory); err == nil {
		t.Fatal("accepted empty check groups")
	}
	os.WriteFile(filepath.Join(directory, "mmwatchdog_node_settings.yml"), []byte("check_groups: {}"), 0600)
	if _, err := config.Load(directory); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatal(err)
	}
}

// The identity of a check with a module that is not core is the module's
// name, its inputs, and its run settings, as the configuration writes them.
// The node's module directories are no part of it: a module can move to
// another directory, or from one kind to another, and every check that
// calls it keeps its incidents. A changed input makes a new check.
func TestTheModuleDirectoriesAreNoPartOfAChecksIdentity(t *testing.T) {
	t.Parallel()
	modules, moved := t.TempDir(), t.TempDir()
	description := testhelpers.TestModuleDescription("queued_jobs", checksandalerts.UsedAsCheck, map[string]string{"queue_file_path": "string"}, nil)
	testhelpers.InstallModule(t, modules, "queued_jobs", description, "true")
	testhelpers.InstallStarlarkModule(t, moved, "queued_jobs", description, "def run_check(inputs, file_contents):\n    return {\"status\": \"ok\", \"message\": \"fine\"}\n")
	directory := testhelpers.ConfigFile(t, `check_groups:
  endpoint: {http: {url: 'http://127.0.0.1:8080'}, alert_after_failing_for: 0s}
  script: {shell: 'false', alert_after_failing_for: 0s}
  queue: {queued_jobs: {queue_file_path: /var/lib/myapp/pending}, alert_after_failing_for: 0s}
`)
	c, err := config.LoadWithModuleDirs(directory, checksandalerts.ModuleDirs{External: modules})
	if err != nil {
		t.Fatal(err)
	}
	s := incidents.FreshSnapshot()
	now := time.Now()
	s.Reconcile(c, config.CheckDefinitions(c), now)
	incidents := map[string]string{}
	for key, state := range s.Checks {
		state.Observe(checksandalerts.Observation{Status: "failed", At: now, Message: "unhealthy"}, c.CheckGroups[state.CheckGroup].Timing, false)
		incidents[key] = state.FailureIncident.Incident
	}
	if c, err = config.LoadWithModuleDirs(directory, checksandalerts.ModuleDirs{External: moved}); err != nil || c.CheckGroups["queue"].Checks["queued_jobs"].ModuleKind() != checksandalerts.ModuleKindProjectStarlark {
		t.Fatal(c, err)
	}
	if events := s.Reconcile(c, config.CheckDefinitions(c), now); len(events) != 0 || s.Checks["queue/queued_jobs"].FailureIncident.Incident != incidents["queue/queued_jobs"] {
		t.Fatal("a module in another directory and of another kind made a new check:", events)
	}
	c.CheckGroups["queue"].Checks["queued_jobs"].Inputs.(*checksandalerts.ModuleCall).Inputs["queue_file_path"] = "/var/lib/myapp/waiting"
	events := s.Reconcile(c, config.CheckDefinitions(c), now)
	if len(events) != 1 || events[0].CheckGroup != "queue" || events[0].Type != "closed_without_recovery" {
		t.Fatal(events)
	}
	for _, key := range []string{"endpoint/http", "script/shell"} {
		if s.Checks[key].FailureIncident.Incident != incidents[key] {
			t.Fatal("unrelated check changed", key)
		}
	}
	if s.Checks["queue/queued_jobs"].FailureIncident.Incident != "" {
		t.Fatal("changed module retained its incident")
	}
}

// Runtime validation checks that shell and module checks can start on
// the target host: the user account and directory exist, a module's
// executable can be run, a module's script is one that MMWatchdog can run, and MMWatchdog
// can make the state directory of a check whose module is not core.
func TestValidateRuntimeChecksExecution(t *testing.T) {
	t.Parallel()
	own := testhelpers.OwnOptions(t)
	runAs := "run_as_user_account: " + own.RunAsUserAccount + ", run_from_directory: " + own.RunFromDirectory
	modules := t.TempDir()
	testhelpers.InstallModule(t, modules, "probe", testhelpers.TestModuleDescription("probe", checksandalerts.UsedAsCheck, nil, nil), "true")
	testhelpers.InstallStarlarkModule(t, modules, "script_probe", testhelpers.TestModuleDescription("script_probe", checksandalerts.UsedAsCheck, nil, nil), "def run_check(inputs, file_contents):\n    return {\"status\": \"ok\", \"message\": \"fine\"}\n")
	testhelpers.InstallModule(t, modules, "plain", testhelpers.TestModuleDescription("plain", checksandalerts.UsedAsCheck, nil, nil), "true")
	testhelpers.WriteNotExecutableFile(t, filepath.Join(modules, "plain", checksandalerts.ModuleExecutableFile), "#!/bin/bash\ntrue\n")
	for _, tc := range []struct{ text, field string }{
		{"check_groups: {a: {shell: {script: 'true', " + runAs + "}}}", ""},
		{"check_groups: {a: {checks: {m: {probe: {" + runAs + "}}}}}", ""},
		{"check_groups: {a: {checks: {m: {script_probe: {" + runAs + "}}}}}", ""},
		{"check_groups: {a: {shell: {script: 'true', run_as_user_account: mmwatchdog-no-such-user, run_from_directory: " + own.RunFromDirectory + "}}}", "check_groups.a.shell.run_as_user_account"},
		{"check_groups: {a: {shell: {script: 'true', run_as_user_account: " + own.RunAsUserAccount + ", run_from_directory: /no/such/dir}}}", "check_groups.a.shell.run_from_directory"},
		{"check_groups: {a: {shell: {script: 'true', " + runAs + "}, recovery_command: {script: fix, run_as_user_account: mmwatchdog-no-such-user, run_from_directory: " + own.RunFromDirectory + "}}}", "check_groups.a.recovery_command.run_as_user_account"},
		{"check_groups: {a: {shell: {script: 'true', " + runAs + "}, recovery_command: {script: fix, run_as_user_account: " + own.RunAsUserAccount + ", run_from_directory: /no/such/dir}}}", "check_groups.a.recovery_command.run_from_directory"},
		{"check_groups: {a: {checks: {m: {plain: {" + runAs + "}}}}}", "check_groups.a.checks.m.plain"},
		{"check_groups: {a: {plain: {" + runAs + "}}}", "check_groups.a.plain"},
		{"check_groups: {a: {checks: {m: {probe: {run_as_user_account: mmwatchdog-no-such-user, run_from_directory: " + own.RunFromDirectory + "}}}}}", "check_groups.a.checks.m.probe.run_as_user_account"},
		{"check_groups: {a: {checks: {m: {script_probe: {run_as_user_account: " + own.RunAsUserAccount + ", run_from_directory: /no/such/dir}}}}}", "check_groups.a.checks.m.script_probe.run_from_directory"},
	} {
		c, e := config.Load(testhelpers.ConfigFile(t, "external_module_dir: "+modules+"\n"+tc.text))
		if e != nil {
			t.Fatal(e)
		}
		e = ValidateRuntime(c)
		if tc.field == "" && e != nil || tc.field != "" && (e == nil || !strings.Contains(e.Error(), tc.field+":")) {
			t.Errorf("%s: got %v, want field %q", tc.text, e, tc.field)
		}
	}
	// A module that the node does not have is refused when the configuration
	// is loaded, before anything about the node is checked.
	if _, e := config.Load(testhelpers.ConfigFile(t, "external_module_dir: "+modules+"\ncheck_groups: {a: {checks: {m: {missing: {}}}}}")); e == nil || !strings.Contains(e.Error(), `unknown field "missing"`) {
		t.Fatal(e)
	}
	// MMWatchdog reads a script without running it: text that is no Starlark, a name
	// that nothing defines, and a script without the function that MMWatchdog calls
	// stop a deploy, where they would make every run of the check unknown.
	for name, script := range map[string]string{
		"wrong_syntax":   "def run_check(inputs, file_contents):\n    return {\"status\": \"ok\", \"message\": \"fine\"\n",
		"undefined_name": "def run_check(inputs, file_contents):\n    queue = open(inputs[\"queue_file_path\"])\n    return {\"status\": \"ok\", \"message\": queue.read()}\n",
		"no_function":    "def check(inputs):\n    return {\"status\": \"ok\", \"message\": \"fine\"}\n",
		"other_function": testhelpers.ScriptOfADestination,
	} {
		testhelpers.InstallStarlarkModule(t, modules, name, testhelpers.TestModuleDescription(name, checksandalerts.UsedAsCheck, nil, nil), script)
	}
	for name, want := range map[string]string{
		"wrong_syntax":   ":3: got outdent, want '}'",
		"undefined_name": ":2: undefined: open",
		"no_function":    " defines no function run_check(inputs, file_contents)",
		"other_function": " defines no function run_check(inputs, file_contents)",
	} {
		c, e := config.Load(testhelpers.ConfigFile(t, "external_module_dir: "+modules+"\ncheck_groups: {a: {checks: {m: {"+name+": {"+runAs+"}}}}}"))
		if e != nil {
			t.Fatal(e)
		}
		if e = ValidateRuntime(c); e == nil || !strings.HasSuffix(e.Error(), ": check_groups.a.checks.m."+name+": "+filepath.Join(modules, name, checksandalerts.ModuleScriptFile)+want) {
			t.Errorf("%s: got %v, want %s", name, e, want)
		}
	}
	// MMWatchdog makes the node's module_state_dir itself, in a directory that is
	// there. Only a check whose module is not core needs it.
	nowhere := filepath.Join(t.TempDir(), "no", "such", "directory")
	for check, want := range map[string]string{
		"shell: {script: 'true', " + runAs + "}":       "",
		"probe: {" + runAs + "}":                       "module_state_dir: " + filepath.Dir(nowhere) + " must be an existing directory, so that MMWatchdog can create " + nowhere + " in that directory",
		"checks: {m: {script_probe: {" + runAs + "}}}": "module_state_dir: " + filepath.Dir(nowhere) + " must be an existing directory, so that MMWatchdog can create " + nowhere + " in that directory",
	} {
		c, e := config.Load(testhelpers.ConfigFile(t, "external_module_dir: "+modules+"\nmodule_state_dir: "+nowhere+"\ncheck_groups: {a: {"+check+"}}"))
		if e != nil {
			t.Fatal(e)
		}
		if e = ValidateRuntime(c); want == "" && e != nil || want != "" && (e == nil || !strings.HasSuffix(e.Error(), want)) {
			t.Errorf("%s: got %v, want %q", check, e, want)
		}
	}
}

// Each destination delivers independently, and no maximum bounds their
// number or the number of checks that run at the same moment.
func TestNoMaximumOnDestinationsAndConcurrentChecks(t *testing.T) {
	t.Parallel()
	text := func(n int) string {
		s := "destinations:\n"
		for i := 0; i < n; i++ {
			s += fmt.Sprintf("  d%d: {remind_every: never, webhook: {url: 'https://example.com/%d'}, alert_severities: [sos]}\n", i, i)
		}
		return s
	}
	loadDestinations := func(n int) (*config.Config, error) {
		directory := testhelpers.ConfigFile(t, "check_groups: {app: {shell: 'true'}}")
		if err := os.WriteFile(filepath.Join(directory, "alert_destinations.yml"), []byte(text(n)), 0600); err != nil {
			t.Fatal(err)
		}
		return config.Load(directory)
	}
	if c, e := loadDestinations(40); e != nil || len(c.Alerts.Destinations) != 40 {
		t.Fatal(e)
	}
	if c, e := config.Load(testhelpers.ConfigFile(t, "max_concurrent_checks: 500\ncheck_groups: {app: {shell: 'true'}}")); e != nil || c.MaxConcurrentChecks != 500 {
		t.Fatal(e)
	}
	if _, e := config.Load(testhelpers.ConfigFile(t, "max_concurrent_checks: 0\ncheck_groups: {app: {shell: 'true'}}")); e == nil || !strings.Contains(e.Error(), "max_concurrent_checks: expected a whole number of at least 1") {
		t.Fatal(e)
	}
}

// Growing stores state queue retention and use defaults for event limits: a
// time and a size for the event history and for the undelivered alerts, a
// size for state.db, and on a hub a time and two sizes for what it keeps for
// its nodes. A size is a whole number with MiB or GiB.
func TestStorageSettings(t *testing.T) {
	t.Parallel()
	group := "check_groups: {a: {shell: 'true'}}\n"
	c, e := config.Load(testhelpers.ConfigFile(t, group))
	if e != nil || c.StorageLimits() != limitsWithoutHub(config.DefaultStorageLimits) || c.KeepEventHistoryFor != "24h" || c.EventHistoryContentSizeLimit != "16MiB" || c.StateDatabaseSizeLimit != "128MiB" {
		t.Fatal("defaults", c, e)
	}
	c, e = config.Load(testhelpers.ConfigFile(t, "keep_event_history_for: 7d\nevent_history_content_size_limit: 64MiB\nkeep_undelivered_alerts_for: 2d\nundelivered_alerts_content_size_limit: 32MiB\nstate_database_size_limit: 1GiB\n"+group))
	if e != nil || c.StorageLimits() != (config.StorageLimits{KeepEventHistoryFor: 7 * 24 * time.Hour, EventHistoryBytes: 64 * 1024 * 1024, KeepUndeliveredAlertsFor: 48 * time.Hour, UndeliveredAlertsBytes: 32 * 1024 * 1024, StateDatabaseBytes: 1024 * 1024 * 1024, UndeliveredAlertsOfNodesBytes: 0}) {
		t.Fatal("stated", c, e)
	}
	hub := "fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'util:8445'}\n"
	c, e = config.Load(testhelpers.ConfigFile(t, hub+group))
	if e != nil || c.StorageLimits() != config.DefaultStorageLimits || c.FleetHubRole.HubLimits() != config.DefaultHubLimits || c.FleetHubRole.EventHistoryOfNodesContentSizeLimit != "4GiB" {
		t.Fatal("hub defaults", c, e)
	}
	c, e = config.Load(testhelpers.ConfigFile(t, "state_database_size_limit: 1GiB\nfleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'util:8445', keep_event_history_of_nodes_for: 30d, event_history_of_nodes_content_size_limit: 20GiB, undelivered_alerts_of_nodes_content_size_limit: 256MiB}\n"+group))
	if e != nil || c.FleetHubRole.HubLimits() != (config.HubLimits{KeepEventHistoryOfNodesFor: 30 * 24 * time.Hour, EventHistoryOfNodesBytes: 20 * 1024 * 1024 * 1024, KeepMeasurementHistoryOfNodesFor: "7d", MeasurementHistoryOfNodesSizeLimit: "20GiB"}) || c.StorageLimits().UndeliveredAlertsOfNodesBytes != 256*1024*1024 {
		t.Fatal("hub stated", c, e)
	}
	for text, want := range map[string]string{
		"event_history_content_size_limit: 16\n" + group:     "expected a size",
		"event_history_content_size_limit: 16MB\n" + group:   "expected a size: a whole number with MiB or GiB, such as 256MiB",
		"event_history_content_size_limit: 1.5GiB\n" + group: "expected a size",
		"event_history_content_size_limit: 0MiB\n" + group:   "expected a size",
		"event_history_content_size_limit: ''\n" + group:     "expected a size",
		"keep_event_history_for: 0s\n" + group:               "keep_event_history_for: must be positive",
		"keep_undelivered_alerts_for: 0s\n" + group:          "keep_undelivered_alerts_for: must be positive",
		"state_database_size_limit: 32MiB\n" + group:         "state_database_size_limit: must be at least 48MiB, twice the sum of event_history_content_size_limit and undelivered_alerts_content_size_limit",
		"event_history_content_size_limit: 1GiB\n" + group:   "state_database_size_limit: must be at least 2064MiB",
		hub + "state_database_size_limit: 64MiB\n" + group:   "state_database_size_limit: must be at least 112MiB, twice the sum of event_history_content_size_limit, undelivered_alerts_content_size_limit, and fleet_hub_role.undelivered_alerts_of_nodes_content_size_limit",
		"fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'util:8445', keep_event_history_of_nodes_for: 0s}\n" + group:            "fleet_hub_role.keep_event_history_of_nodes_for: must be positive",
		"fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'util:8445', event_history_of_nodes_content_size_limit: big}\n" + group: "expected a size",
		"keep_event_history_of_nodes_for: 7d\n" + group: `unknown field "keep_event_history_of_nodes_for"`,
	} {
		if _, e := config.Load(testhelpers.ConfigFile(t, text)); e == nil || !strings.Contains(e.Error(), want) {
			t.Errorf("%q: got %v, want %q", text, e, want)
		}
	}
	for text, want := range map[string]int64{"1MiB": 1024 * 1024, "256MiB": 256 * 1024 * 1024, "20GiB": 20 * 1024 * 1024 * 1024} {
		if got, e := checksandalerts.ParseSize(text); e != nil || got != want {
			t.Error(text, got, e)
		}
	}
	for _, text := range []string{"", "MiB", "1", "1KiB", "1mib", "-1MiB", "+1MiB", " 1MiB", "1 MiB", "1.0MiB", "9999999999999GiB"} {
		if _, e := checksandalerts.ParseSize(text); e == nil {
			t.Error("accepted the size", text)
		}
	}
}

// Alert destinations come only from alert_destinations.yml beside the main file, with
// the same strict validation and source-aware errors.
func TestAlertSettingsFile(t *testing.T) {
	t.Parallel()
	path := testhelpers.ConfigFile(t, "check_groups: {a: {shell: echo ok}}\n")
	alerts := filepath.Join(path, "alert_destinations.yml")
	// MMWatchdog runs with no destination only when the file says so. A missing
	// file is an error that shows both choices, so a person who forgot the
	// destinations finds out.
	c, e := config.Load(path)
	if e != nil || c.Alerts.Destinations == nil || len(c.Alerts.Destinations) != 0 {
		t.Fatal("destinations: {} is not a valid choice of no destination", c, e)
	}
	os.Remove(alerts)
	if _, e = config.Load(path); e == nil || e.Error() != alerts+`: alert destinations file is missing. Create the file with a destination, or write "destinations: {}" into the file to run MMWatchdog with no destination. With no destination, MMWatchdog runs checks, restarts units, and runs recovery commands, and sends alerts to nobody` {
		t.Fatal("a missing alert_destinations.yml was accepted, or its error does not show both choices:", e)
	}
	for _, empty := range []string{"", "# no destination yet\n", "{}\n"} {
		os.WriteFile(alerts, []byte(empty), 0600)
		if _, e = config.Load(path); e == nil || !strings.Contains(e.Error(), `expected a destinations: mapping; write "destinations: {}" to run MMWatchdog with no destination`) {
			t.Fatalf("%q: a file without destinations was accepted: %v", empty, e)
		}
	}
	os.WriteFile(alerts, []byte("destinations:\n  ops: {remind_every: never, webhook: {url: 'https://example.com/events'}, alert_severities: [info, warn, sos]}\n  oncall: {webhook: {url: 'https://example.com/pager'}, remind_every: 4h, alert_severities: [sos]}\n"), 0600)
	if c, e = config.Load(path); e != nil || c.Alerts.Destinations["ops"].TimeLimitPerDeliveryAttempt != "5s" || c.Alerts.Destinations["ops"].RemindEvery != "never" || c.Alerts.Destinations["oncall"].RemindEvery != "4h" {
		t.Fatal("alert_destinations.yml destinations were not loaded with their timeout and explicit remind_every", c, e)
	}
	ok := "check_groups: {a: {shell: echo ok}}\n"
	for _, tc := range []struct{ main, alerts, want string }{
		{ok + "destinations: {ops: {webhook: {url: 'https://example.com'}}}\n", "destinations: {}\n", `mmwatchdog_node_settings.yml:2:1: destinations: unknown field "destinations"`},
		{ok, "webhooks:\n  ops: {url: 'https://example.com'}\n", `alert_destinations.yml:1:1: webhooks: unknown field "webhooks"`},
		{ok, "destinations:\n  ops:\n    webhook: {url: 'ftp://example.com'}\n", "alert_destinations.yml:3:15: destinations.ops.webhook.url"},
		{ok, "destinations: {ops: {webhook: {url: 'https://example.com', typo: 1}}}\n", `alert_destinations.yml:1:60: destinations.ops.webhook.typo: unknown field "typo": the inputs of the module webhook are dangerously_skip_server_certificate_check, trusted_certificate_authority_file, url, bearer_token_file`},
		{ok, "destinations: {ops: {time_limit_per_delivery_attempt: 5s}}\n", "destinations.ops: expected one module: webhook, email, system_log, or mmhotline"},
		{ok, "destinations: {ops: {email: {smtp: 'smtp.example.com:587'}}}\n", `alert_destinations.yml:1:30: destinations.ops.email.smtp: unknown field "smtp": the inputs of the module email are dangerously_skip_server_certificate_check, trusted_certificate_authority_file, smtp_server, smtp_connection_security, from, to, login_user, login_password_file`},
		{ok, "destinations: {ops: {slack: {webhook_url_file: /etc/mmwatchdog/secrets/slack}}}\n", `alert_destinations.yml:1:22: destinations.ops.slack: unknown field "slack": the field name matches neither a setting of a destination nor an installed module.`},
		{ok, "destinations: {ops: {webhook: {url: 'https://example.com'}, remind_every: 0s, alert_severities: [sos]}}\n", "destinations.ops.remind_every: must be a positive duration or never"},
		{"check_groups: {every_check_group: {shell: echo ok}}\n", "destinations: {}\n", "check_groups.every_check_group: name is reserved for whole-node maintenance"},
	} {
		testhelpers.WriteConfigText(path, tc.main)
		os.WriteFile(alerts, []byte(tc.alerts), 0600)
		if _, e := config.Load(path); e == nil || !strings.Contains(e.Error(), tc.want) {
			t.Errorf("got %v, want %q", e, tc.want)
		}
	}
}

// A node that joins a hub gets its alert settings only from the hub, and
// the hub's queue on a joined node has a name no destination can take.
func TestOneSourceOfAlertSettings(t *testing.T) {
	t.Parallel()
	path := testhelpers.ConfigFile(t, "join_fleet_hub: {join_code_file: /etc/mmwatchdog/secrets/hub-join}\ncheck_groups: {a: {shell: echo ok}}\n")
	alerts := filepath.Join(path, "alert_destinations.yml")
	os.WriteFile(alerts, []byte("destinations: {}\n"), 0600)
	if _, e := config.Load(path); e == nil || !strings.Contains(e.Error(), "join_fleet_hub: remove "+alerts) {
		t.Fatal(e)
	}
	testhelpers.WriteConfigText(path, "check_groups: {a: {shell: echo ok}}\n")
	os.WriteFile(alerts, []byte("destinations: {hub: {webhook: {url: 'https://example.com'}}}\n"), 0600)
	if _, e := config.Load(path); e == nil || !strings.Contains(e.Error(), "destinations.hub: name is reserved for the fleet hub") {
		t.Fatal(e)
	}
}

// A hub names the address nodes connect to and may set the fleet timings;
// a node names its join code file, which runtime validation reads.
func TestFleetSettings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ text, want string }{
		{"fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'util.example.com:8445'}\ncheck_groups: {a: {shell: echo ok}}\n", ""},
		{"join_fleet_hub: {join_code_file: /etc/mmwatchdog/secrets/hub-join}\ncheck_groups: {a: {shell: echo ok}}\n", ""},
		{"fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: ':8445'}\ncheck_groups: {a: {shell: echo ok}}\n", "fleet_hub_role.address_for_nodes: needs the host name"},
		{"fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'util.example.com:0'}\ncheck_groups: {a: {shell: echo ok}}\n", "fleet_hub_role.address_for_nodes: port must be"},
		{"join_fleet_hub: {join_code_file: secrets/hub-join}\ncheck_groups: {a: {shell: echo ok}}\n", "join_fleet_hub.join_code_file: must be absolute"},
		{"fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'util:8445'}\njoin_fleet_hub: {join_code_file: /etc/mmwatchdog/secrets/hub-join}\ncheck_groups: {a: {shell: echo ok}}\n", "join_fleet_hub: a hub cannot join another hub"},
		{"join_fleet_hub: {join_code_file: /etc/mmwatchdog/secrets/hub-join, prefer_this_node_as_hub_outage_alerter: true}\ncheck_groups: {a: {shell: echo ok}}\n", ""},
		{"fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'util:8445'}\nprefer_this_node_as_hub_outage_alerter: true\ncheck_groups: {a: {shell: echo ok}}\n", "unknown field \"prefer_this_node_as_hub_outage_alerter\""},
		{"check_groups: {node_reports: {shell: echo ok}}\n", "check_groups.node_reports: name is reserved for the built-in node_reports check group"},
		{"fleet_hub_role: {undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'util:8445', report_every: 3s, alert_after_hub_unreachable_for: 6s, send_alert_without_hub_after_waiting_for: 1s}\ncheck_groups: {a: {shell: echo ok}}\n", ""},
		{"fleet_hub_role: {alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'util:8445', report_every: 500ms}\ncheck_groups: {a: {shell: echo ok}}\n", "fleet_hub_role.report_every: expected a duration of at least 1s"},
		{"fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'util:8445', send_alert_without_hub_after_waiting_for: 500ms}\ncheck_groups: {a: {shell: echo ok}}\n", "fleet_hub_role.send_alert_without_hub_after_waiting_for: expected a duration of at least 1s"},
		// A fleet timing has no upper bound.
		{"fleet_hub_role: {undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'util:8445', report_every: 30m, alert_after_hub_unreachable_for: 2d, send_alert_without_hub_after_waiting_for: 1h}\ncheck_groups: {a: {shell: echo ok}}\n", ""},
		{"fleet_hub_role: {alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'util:8445', report_every: 2m}\ncheck_groups: {a: {shell: echo ok}}\n", "fleet_hub_role.alert_after_hub_unreachable_for: must be at least twice report_every"},
	} {
		if _, e := config.Load(testhelpers.ConfigFile(t, tc.text)); tc.want == "" && e != nil || tc.want != "" && (e == nil || !strings.Contains(e.Error(), tc.want)) {
			t.Errorf("%s: got %v, want %q", tc.text, e, tc.want)
		}
	}
	if c, e := config.Load(testhelpers.ConfigFile(t, "fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'util:8445'}\ncheck_groups: {a: {shell: echo ok}}\n")); e != nil || c.FleetHubRole.FleetTimings != config.DefaultFleetTimings {
		t.Fatal("explicit fleet timings", c, e)
	}
	valid := fleet.JoinCode{Address: "util.example.com:8445", Fingerprint: strings.Repeat("ab", 32), Secret: strings.Repeat("cd", 32)}
	for code, want := range map[string]string{
		valid.String():               "",
		"  " + valid.String() + "\n": "",
		strings.Replace(valid.String(), "mmwatchdog-join", "join", 1): "expected a join code",
		strings.Replace(valid.String(), ":8445", "", 1):               "invalid hub address",
		valid.String()[:len(valid.String())-2]:                        "invalid fingerprint or secret",
	} {
		path := testhelpers.ConfigFile(t, "check_groups: {a: {tcp: '127.0.0.1:1'}}\n")
		c, e := config.Load(path)
		if e != nil {
			t.Fatal(e)
		}
		c.JoinFleetHub = &config.JoinFleetHub{JoinCodeFile: filepath.Join(filepath.Dir(path), "hub-join")}
		os.WriteFile(c.JoinFleetHub.JoinCodeFile, []byte(code), 0600)
		if e = ValidateRuntime(c); want == "" && e != nil || want != "" && (e == nil || !strings.Contains(e.Error(), "join_fleet_hub") || !strings.Contains(e.Error(), want)) {
			t.Errorf("%q: got %v, want %q", code, e, want)
		}
	}
}

// A check group with one check writes the check directly on the group, and
// the check is named after its module, also a module that is not core.
func TestDirectCheckNames(t *testing.T) {
	t.Parallel()
	modules := t.TempDir()
	testhelpers.InstallModule(t, modules, "queued_jobs", testhelpers.TestModuleDescription("queued_jobs", checksandalerts.UsedAsCheck, nil, nil), "true")
	for _, tc := range []struct{ kind, value string }{
		{"http", "{url: 'https://example.com'}"}, {"tcp", "'127.0.0.1:80'"}, {"tls", "{address: 'example.com:443', fail_when_certificate_expires_within: 30d}"}, {"shell", "'true'"}, {"queued_jobs", "{}"},
		{"node_resource", "memory"}, {"node_filesystem", "/"}, {"systemd_unit_state", "{systemd_unit: app.service, fail_when: the_unit_is_not_active}"}, {"systemd_unit_resource", "{systemd_unit: app.service, resource: cpu}"},
		{"systemd_unit_crash_loop", "{systemd_unit: app.service, automatic_restarts_above: 2, automatic_restarts_within: 1h}"},
		{"every_systemd_unit", "{fail_when: a_systemd_unit_has_failed, systemd_units_to_ignore: []}"},
	} {
		directory := testhelpers.ConfigFile(t, "check_groups:\n  app:\n    "+tc.kind+": "+tc.value+"\n")
		c, err := config.LoadWithModuleDirs(directory, checksandalerts.ModuleDirs{External: modules})
		if err != nil {
			t.Fatal(err)
		}
		definitions := config.CheckDefinitions(c)
		def, ok := definitions["app/"+tc.kind]
		if !ok || len(definitions) != 1 || def.Name != tc.kind || def.Check.ModuleName() != tc.kind {
			t.Fatal(definitions)
		}
		source := c.CheckSource("app", tc.kind)
		if source.File != filepath.Join(directory, "check_groups", "checks.yml") || source.Line != 3 {
			t.Fatal(source)
		}
	}
}

func TestTheRetiredHubWatcherSettingNameSaysWhatToUseInstead(t *testing.T) {
	t.Parallel()
	_, err := config.Load(testhelpers.ConfigFile(t, "join_fleet_hub: {join_code_file: /etc/mmwatchdog/secrets/hub-join, prefer_this_node_as_hub_watcher: true}\ncheck_groups: {a: {shell: 'true'}}"))
	if err == nil || !strings.Contains(err.Error(), "prefer_this_node_as_hub_watcher was renamed to prefer_this_node_as_hub_outage_alerter; use the new name") {
		t.Fatal(err)
	}
}

// Retired names are negative fixtures, never aliases in the configuration schema.
func TestRetiredConfigurationNamesAreUnknown(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ text, field string }{
		{"defaults: {}\ncheck_groups: {a: {shell: 'true'}}", "defaults"},
		{"concurrency: 8\ncheck_groups: {a: {shell: 'true'}}", "concurrency"},
		{"module_dir: /tmp\ncheck_groups: {a: {shell: 'true'}}", "module_dir"},
		{"hub: {}\ncheck_groups: {a: {shell: 'true'}}", "hub"},
		{"join_hub: /tmp/code\ncheck_groups: {a: {shell: 'true'}}", "join_hub"},
		{"prefer_this_node_as_hub_outage_alerter: true\ncheck_groups: {a: {shell: 'true'}}", "prefer_this_node_as_hub_outage_alerter"},
		{"check_groups: {a: {every: 1s, shell: 'true'}}", "every"},
		{"check_groups: {a: {timeout: 1s, shell: 'true'}}", "timeout"},
		{"check_groups: {a: {module: {name: queue}}}", "module"},
		{"check_groups: {a: {external_module: {name: queue}}}", "external_module"},
		{"check_groups: {a: {crash_loop_check: false}}", "crash_loop_check"},
		{"check_groups: {a: {checks: {c: {systemd_unit_crash_loop: {above: 2}}}}}", "above"},
		{"check_groups: {a: {http: {url: 'https://example.com', status: 200}}}", "status"},
		{"check_groups: {a: {http: {url: 'https://example.com', contains: x}}}", "contains"},
		{"check_groups: {a: {http: {url: 'https://example.com', not_contains: x}}}", "not_contains"},
		{"check_groups: {a: {http: {url: 'https://example.com', matches: x}}}", "matches"},
		{"check_groups: {a: {http: {url: 'https://example.com', token_file: /t}}}", "token_file"},
		{"check_groups: {a: {tls: {fail_when_certificate_expires_within: 30d, address: 'example.com:443', expires_within: 1h}}}", "expires_within"},
		{"check_groups: {a: {heartbeat: {within: 1h}}}", "within"},
		{"check_groups: {a: {heartbeat: {token_file: /t}}}", "token_file"},
		{"check_groups: {a: {checks: {r: {systemd_unit_state: {systemd_unit: a.service, fail_when: the_unit_is_not_active}}}, restart_systemd_unit: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, when: [service]}}}", "when"},
		{"check_groups: {a: {checks: {r: {systemd_unit_state: {systemd_unit: a.service, fail_when: the_unit_is_not_active}}}, restart_systemd_unit: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, attempts: 3}}}", "attempts"},
		{"check_groups: {a: {checks: {r: {systemd_unit_state: {systemd_unit: a.service, fail_when: the_unit_is_not_active}}}, restart_systemd_unit: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, cooldown: 1m}}}", "cooldown"},
		{"check_groups: {a: {checks: {r: {systemd_unit_state: {systemd_unit: a.service, fail_when: the_unit_is_not_active}}}, restart_systemd_unit: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, reset_after_healthy: 1m}}}", "reset_after_healthy"},
		{"check_groups: {a: {shell: 'true', recovery_command: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, time_limit_per_attempt: 1m, triggered_by_checks: [shell], script: fix, timeout: 1m}}}", "timeout"},
		{"fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, address: 'util:8445'}\ncheck_groups: {a: {shell: 'true'}}", "address"},
		// The names before the review of real-world monitoring use.
		{"check_defaults: {check_every: 30s}\ncheck_groups: {a: {shell: 'true'}}", "check_defaults"},
		{"check_groups: {a: {fail_after: 1m, shell: 'true'}}", "fail_after"},
		{"check_groups: {a: {recover_after: 1m, shell: 'true'}}", "recover_after"},
		{"check_groups: {a: {shell: 'true', raise_to_sos_after: 1h}}", "raise_to_sos_after"},
		{"check_groups: {a: {systemd_unit: a.service, checks: {r: {systemd_unit_state: {systemd_unit: a.service, fail_when: the_unit_is_not_active}}}}}", "systemd_unit"},
		{"check_groups: {a: {only_record_readings: true, node_resource: memory, above_percent: 90}}", "above_percent"},
		{"check_groups: {a: {http: {url: 'https://example.com', expected_http_status_code: 200}}}", "expected_http_status_code"},
		{"check_groups: {a: {http: {url: 'https://example.com', ca_file: /ca.pem}}}", "ca_file"},
		{"check_groups: {a: {shell: {script: 'true', user: myapp}}}", "user"},
		{"check_groups: {a: {shell: {script: 'true', dir: /srv}}}", "dir"},
		{"check_groups: {a: {shell: 'true', recovery_command: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, time_limit_per_attempt: 1m, triggered_by_checks: [shell], script: fix, time_limit: 1m}}}", "time_limit"},
		{"heartbeat_receiver: {listen: '127.0.0.1:8444'}\ncheck_groups: {a: {shell: 'true'}}", "listen"},
		{"heartbeat_receiver: {cert_file: /c}\ncheck_groups: {a: {shell: 'true'}}", "cert_file"},
		{"heartbeat_receiver: {key_file: /k}\ncheck_groups: {a: {shell: 'true'}}", "key_file"},
		{"fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'util:8445', hub_unreachable_after: 3m}\ncheck_groups: {a: {shell: 'true'}}", "hub_unreachable_after"},
		{"fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'util:8445', direct_alert_fallback_after: 30s}\ncheck_groups: {a: {shell: 'true'}}", "direct_alert_fallback_after"},
		{"join_fleet_hub: {join_code_file: /etc/mmwatchdog/secrets/hub-join, prefer_hub_outage_alerter: true}\ncheck_groups: {a: {shell: 'true'}}", "prefer_hub_outage_alerter"},
	} {
		if _, err := config.Load(testhelpers.ConfigFile(t, tc.text)); err == nil || !strings.Contains(err.Error(), "unknown field "+strconv.Quote(tc.field)) {
			t.Fatalf("%s: %v", tc.text, err)
		}
	}
	for _, field := range []string{"timeout", "token_file"} {
		directory := testhelpers.ConfigFile(t, "check_groups: {a: {shell: 'true'}}")
		destination := "destinations: {ops: {webhook: {url: 'https://example.com'}, timeout: 1s}}"
		if field == "token_file" {
			destination = "destinations: {ops: {webhook: {url: 'https://example.com', token_file: /t}}}"
		}
		os.WriteFile(filepath.Join(directory, "alert_destinations.yml"), []byte(destination), 0600)
		if _, err := config.Load(directory); err == nil || !strings.Contains(err.Error(), "unknown field "+strconv.Quote(field)) {
			t.Fatal(err)
		}
	}
}

// limitsWithoutHub is the limits of a node that is not a hub: nothing bounds an
// alert intake it does not have.
func limitsWithoutHub(l config.StorageLimits) config.StorageLimits {
	l.UndeliveredAlertsOfNodesBytes = 0
	return l
}

// A named check may state its own two waiting times. Where it states none,
// those of its check group apply, and the effective configuration says
// where each one is stated. A direct check is the group mapping, so the
// check group's waiting times are its own.
func TestACheckStatesItsOwnWaitingTimes(t *testing.T) {
	t.Parallel()
	c, err := config.Load(testhelpers.ConfigFile(t, `check_groups:
  myapp:
    check_every: 30s
    time_limit_per_run: 5s
    alert_after_failing_for: 5m
    recovered_after_healthy_for: 1m
    checks:
      myapp_service_is_running: {systemd_unit_state: {systemd_unit: myapp.service, fail_when: the_unit_is_not_active}}
      myapp_memory_is_below_512_mib:
        systemd_unit_resource: {systemd_unit: myapp.service, resource: memory}
        fail_when_measurement_above: {memory_used_mib: 512}
        alert_after_failing_for: 10m
        recovered_after_healthy_for: 5m
      myapp_answers_health_request:
        http: {url: 'http://127.0.0.1:8080/health'}
        alert_after_failing_for: 0s
  database_port:
    check_every: 1m
    time_limit_per_run: 5s
    alert_after_failing_for: 2m
    recovered_after_healthy_for: 30s
    tcp: 127.0.0.1:5432
`))
	if err != nil {
		t.Fatal(err)
	}
	defs := config.CheckDefinitions(c)
	for key, want := range map[string]checksandalerts.Timing{
		"myapp/myapp_service_is_running":      {CheckEvery: "30s", TimeLimitPerRun: "5s", AlertAfterFailingFor: "5m", RecoveredAfterHealthyFor: "1m"},
		"myapp/myapp_memory_is_below_512_mib": {CheckEvery: "30s", TimeLimitPerRun: "5s", AlertAfterFailingFor: "10m", RecoveredAfterHealthyFor: "5m"},
		"myapp/myapp_answers_health_request":  {CheckEvery: "30s", TimeLimitPerRun: "5s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "1m"},
		"database_port/tcp":                   {CheckEvery: "1m", TimeLimitPerRun: "5s", AlertAfterFailingFor: "2m", RecoveredAfterHealthyFor: "30s"},
	} {
		if defs[key].Timing != want {
			t.Errorf("%s: %+v, want %+v", key, defs[key].Timing, want)
		}
	}
	checks := c.CheckGroups["myapp"].Checks
	for name, want := range map[string][2]string{
		"myapp_service_is_running":      {"check_group", "check_group"},
		"myapp_memory_is_below_512_mib": {"check", "check"},
		"myapp_answers_health_request":  {"check", "check_group"},
	} {
		if got := [2]string{checks[name].AlertAfterFailingForStatedOn, checks[name].RecoveredAfterHealthyForStatedOn}; got != want {
			t.Errorf("%s: stated on %v, want %v", name, got, want)
		}
	}
	// The effective configuration shows each check's waiting times.
	b, _ := json.Marshal(checks["myapp_answers_health_request"])
	if !strings.Contains(string(b), `"alert_after_failing_for":"0s","recovered_after_healthy_for":"1m","alert_after_failing_for_stated_on":"check","recovered_after_healthy_for_stated_on":"check_group"`) {
		t.Fatal(string(b))
	}
	for text, want := range map[string]string{
		"check_groups: {a: {checks: {b: {shell: 'true', alert_after_failing_for: -1s}}}}":     `check_groups.a.checks.b.alert_after_failing_for: expected a duration of 0s or more, such as 30s, 5m, 26h, or 7d, found the string "-1s"`,
		"check_groups: {a: {checks: {b: {shell: 'true', recovered_after_healthy_for: -1s}}}}": `check_groups.a.checks.b.recovered_after_healthy_for: expected a duration of 0s or more, such as 30s, 5m, 26h, or 7d, found the string "-1s"`,
		"check_groups: {a: {checks: {b: {shell: 'true', alert_after_failing_for: soon}}}}":    "expected a duration",
		"check_groups: {a: {checks: {b: {shell: 'true', check_every: 10s}}}}":                 `unknown field "check_every"`,
		"check_groups: {a: {checks: {b: {shell: 'true', time_limit_per_run: 1s}}}}":           `unknown field "time_limit_per_run"`,
	} {
		if _, err := config.Load(testhelpers.ConfigFile(t, text)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", text, err, want)
		}
	}
}

// A restart and a recovery command each state their own waiting time, with
// no default, and max_attempts is a whole number from 1 or unlimited.
func TestRecoveryActionWaitingTimesAndAttempts(t *testing.T) {
	t.Parallel()
	head := "alert_severity_when_this_node_goes_silent: sos\nalert_severity_when_this_node_has_rebooted: info\nkeep_measurement_history_for: 7d\nmeasurement_history_size_limit: 256MiB\nkeep_undelivered_alerts_for: 24h\nundelivered_alerts_content_size_limit: 8MiB\ncheck_groups:\n  a:\n    check_every: 30s\n    time_limit_per_run: 5s\n    alert_after_failing_for: 5m\n    recovered_after_healthy_for: 1m\n    shell: 'true'\n    alert_severity: warn\n"
	c, err := config.Load(testhelpers.ConfigFileAsWritten(t, head+"    restart_systemd_unit: {min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, systemd_unit: a.service, triggered_by_checks: [shell], restart_after_failing_for: 1m, max_attempts: unlimited}\n    recovery_command: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, time_limit_per_attempt: 1m, triggered_by_checks: [shell], script: fix, run_after_failing_for: 0s}\n"))
	if err != nil {
		t.Fatal(err)
	}
	group := c.CheckGroups["a"]
	if group.RestartSystemdUnit.RestartAfterFailingFor != "1m" || group.RestartSystemdUnit.Attempts != "unlimited" || group.RecoveryCommand.RunAfterFailingFor != "0s" || group.RecoveryCommand.Attempts != "3" {
		t.Fatalf("%+v %+v", group.RestartSystemdUnit, group.RecoveryCommand)
	}
	policies := incidents.RecoveryPolicies(c, nil)
	if p := policies["a.service"]; p.Attempts != checksandalerts.NoAttemptLimit || p.After["a/shell"] != time.Minute {
		t.Fatalf("%+v", p)
	}
	if p := policies["a"]; p.Attempts != 3 || p.After["a/shell"] != 0 {
		t.Fatalf("%+v", p)
	}
	b, _ := json.Marshal(group.RestartSystemdUnit)
	if !strings.Contains(string(b), `"max_attempts":"unlimited"`) {
		t.Fatal(string(b))
	}
	if b, _ = json.Marshal(group.RecoveryCommand); !strings.Contains(string(b), `"max_attempts":3`) {
		t.Fatal(string(b))
	}
	for action, want := range map[string]string{
		"    restart_systemd_unit: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, systemd_unit: a.service, triggered_by_checks: [shell]}\n":                                         "check_groups.a.restart_systemd_unit.restart_after_failing_for: is required: how long a check in triggered_by_checks must be failing before MMWatchdog restarts the unit",
		"    recovery_command: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, time_limit_per_attempt: 1m, script: fix}\n":                                                           "check_groups.a.recovery_command.run_after_failing_for: is required: how long a check in triggered_by_checks must be failing before MMWatchdog runs the command",
		"    restart_systemd_unit: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, systemd_unit: a.service, triggered_by_checks: [shell], restart_after_failing_for: -1m}\n":         `check_groups.a.restart_systemd_unit.restart_after_failing_for: expected a duration of 0s or more, such as 30s, 5m, 26h, or 7d, found the string "-1m"`,
		"    recovery_command: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, time_limit_per_attempt: 1m, triggered_by_checks: [shell], script: fix, run_after_failing_for: -1m}\n": `check_groups.a.recovery_command.run_after_failing_for: expected a duration of 0s or more, such as 30s, 5m, 26h, or 7d, found the string "-1m"`,
		"    restart_systemd_unit: {min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, systemd_unit: a.service, triggered_by_checks: [shell], restart_after_failing_for: 1m, max_attempts: 0}\n":          "check_groups.a.restart_systemd_unit.max_attempts: expected a whole number of at least 1, or unlimited",
		"    restart_systemd_unit: {min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, systemd_unit: a.service, triggered_by_checks: [shell], restart_after_failing_for: 1m, max_attempts: -2}\n":         "check_groups.a.restart_systemd_unit.max_attempts: expected a whole number of at least 1, or unlimited",
		"    restart_systemd_unit: {min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, systemd_unit: a.service, triggered_by_checks: [shell], restart_after_failing_for: 1m, max_attempts: forever}\n":    "check_groups.a.restart_systemd_unit.max_attempts: expected a whole number of at least 1, or unlimited",
		"    restart_systemd_unit: {min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, systemd_unit: a.service, triggered_by_checks: [shell], restart_after_failing_for: 1m, max_attempts: 1.5}\n":        "expected a whole number of attempts such as 3, or unlimited",
		"    recovery_command: {min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, time_limit_per_attempt: 1m, triggered_by_checks: [shell], script: fix, run_after_failing_for: 1m, max_attempts: ''}\n": "check_groups.a.recovery_command.max_attempts: expected a whole number of at least 1, or unlimited",
	} {
		if _, err := config.Load(testhelpers.ConfigFileAsWritten(t, head+action)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", strings.TrimSpace(action), err, want)
		}
	}
}

func TestReloadRefusesMissingFilesBeforeActivation(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("a-long-reload-test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"example": {Inputs: &checksandalerts.Webhook{URL: "https://example.com", BearerTokenFile: token}, RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities}}
	writeConfig(t, path, c)
	d, err := NewDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	active := d.config
	c.Alerts.Destinations["example"].Inputs.(*checksandalerts.Webhook).BearerTokenFile = token + "-missing"
	writeConfig(t, path, c)
	_, err = d.reload()
	const hint = "The file named in the error is missing on the machine running mmwatchdog. On a machine other than the monitored node, check only the configuration files: mmwatchdog validate --config-only"
	if err == nil || !strings.HasSuffix(err.Error(), hint) || d.config != active {
		t.Fatal(err, d.config == active)
	}
}

func TestReloadValidatesScriptInputsBeforeActivation(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	script := testhelpers.ScriptOfACheck + "\ndef validate_inputs(inputs):\n    if inputs[\"count\"] < 1:\n        return \"count: add count: 1 or more\"\n    return None\n"
	testhelpers.InstallStarlarkModule(t, c.ExternalModuleDir, "input_example", testhelpers.TestModuleDescription("input_example", checksandalerts.UsedAsCheck, map[string]string{"count": "integer"}, nil), script)
	call := &checksandalerts.ModuleCall{Name: "input_example", Inputs: map[string]any{"count": 1}, ExecOptions: primitives.ExecOptions{RunAsUserAccount: account.Username, RunFromDirectory: "/"}}
	c.CheckGroups["inputs"] = checksandalerts.CheckGroup{Check: checksandalerts.Check{Inputs: call}}
	writeConfig(t, path, c)
	d, err := NewDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	active := d.config
	call.Inputs["count"] = 0
	writeConfig(t, path, c)
	_, err = d.reload()
	if err == nil || !strings.Contains(err.Error(), "input_example.count: add count: 1 or more") || d.config != active {
		t.Fatal(err)
	}
}

func TestRuntimeSecretPermissionsThroughEveryOwningBlock(t *testing.T) {
	t.Parallel()
	for _, owner := range []string{"heartbeat_receiver", "heartbeat", "join_fleet_hub", "mmhotline", "check", "destination"} {
		t.Run(owner, func(t *testing.T) {
			t.Parallel()
			directory, c := daemonConfig(t)
			secret := filepath.Join(directory, "secret")
			content := "a-token-of-at-least-16-bytes"
			certificate, key := testhelpers.TestCertificate(t, t.TempDir())
			if owner == "heartbeat_receiver" {
				secret = key
			}
			if owner == "join_fleet_hub" {
				content = (fleet.JoinCode{Address: "hub.example.com:8445", Fingerprint: strings.Repeat("a", 64), Secret: strings.Repeat("b", 64)}).String()
			}
			if owner == "mmhotline" {
				content = strings.Repeat("a", 64)
			}
			if owner != "heartbeat_receiver" {
				if err := os.WriteFile(secret, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			setAccess := func(access checksandalerts.SecretFileAccess) {
				switch owner {
				case "heartbeat_receiver":
					c.HeartbeatReceiver = &config.HeartbeatReceiver{SecretFileAccess: access, ListenOnAddress: "127.0.0.1:8444", TLSCertificateFile: certificate, TLSPrivateKeyFile: secret}
				case "heartbeat":
					c.HeartbeatReceiver = &config.HeartbeatReceiver{ListenOnAddress: "127.0.0.1:8444", TLSCertificateFile: certificate, TLSPrivateKeyFile: key}
					c.CheckGroups["backup"] = checksandalerts.CheckGroup{Check: checksandalerts.Check{SecretFileAccess: access}, Heartbeat: &checksandalerts.Heartbeat{MaxTimeBetweenHeartbeats: "1h", ExpectedBearerTokenFile: secret}}
				case "join_fleet_hub":
					c.JoinFleetHub = &config.JoinFleetHub{SecretFileAccess: access, JoinCodeFile: secret}
				case "mmhotline":
					c.Alerts.Destinations = map[string]checksandalerts.Destination{"ops": {Inputs: &checksandalerts.MMHotlineDestination{SecretFileAccess: access, URL: "https://hotline.example.com", AlertSourceID: strings.Repeat("b", 32), SigningSecretFile: secret}, AlertSeverities: testhelpers.AllSeverities}}
				case "check":
					c.CheckGroups["api"] = checksandalerts.CheckGroup{Check: checksandalerts.Check{SecretFileAccess: access, Inputs: &checksandalerts.HTTP{URL: "https://example.com", BearerTokenFile: secret}}}
				case "destination":
					c.Alerts.Destinations = map[string]checksandalerts.Destination{"ops": {SecretFileAccess: access, Inputs: &checksandalerts.Webhook{URL: "https://example.com", BearerTokenFile: secret}, RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities}}
				}
			}
			for _, mode := range []os.FileMode{0640, 0604, 0644} {
				if err := os.Chmod(secret, mode); err != nil {
					t.Fatal(err)
				}
				for _, allowed := range []bool{false, true} {
					access := checksandalerts.SecretFileAccess{}
					if allowed {
						access.SecretReadableByGroup = mode&0040 != 0
						access.DangerouslyLetEveryAccountReadSecret = mode&0004 != 0
					}
					setAccess(access)
					writeConfig(t, directory, c)
					loaded, err := config.Load(directory)
					if err != nil {
						t.Fatal(err)
					}
					err = ValidateRuntime(loaded)
					if allowed {
						if err != nil {
							t.Fatalf("%s %04o allowed: %v", owner, mode, err)
						}
						continue
					}
					if err == nil {
						t.Fatalf("%s %04o was not refused", owner, mode)
					}
					for _, part := range []string{secret, fmt.Sprintf("%04o", mode), "chmod 600 " + secret} {
						if !strings.Contains(err.Error(), part) {
							t.Fatalf("%s: missing %q: %v", owner, part, err)
						}
					}
					if mode&0040 != 0 && !strings.Contains(err.Error(), "secret_readable_by_group: true") {
						t.Fatal(err)
					}
					if mode&0004 != 0 && !strings.Contains(err.Error(), "dangerously_let_every_account_read_secret: true") {
						t.Fatal(err)
					}
					// Startup reads the same file but does not refuse its permissions.
					daemon, err := NewDaemon(directory)
					if err != nil {
						t.Fatalf("%s %04o did not start: %v", owner, mode, err)
					}
					daemon.Close()
				}
			}
		})
	}
}

func TestStartupLogsEachExposedSecretOnceAndContinues(t *testing.T) {
	// The logger is process-wide; parallel tests wait until this test ends.
	directory, c := daemonConfig(t)
	first, second := filepath.Join(directory, "first-token"), filepath.Join(directory, "second-token")
	for _, path := range []string{first, second} {
		os.WriteFile(path, []byte("x"), 0600)
		os.Chmod(path, 0644)
	}
	c.Alerts.Destinations = map[string]checksandalerts.Destination{}
	for _, name := range []string{"first", "same_file_again", "second"} {
		path := first
		if name == "second" {
			path = second
		}
		c.Alerts.Destinations[name] = checksandalerts.Destination{Inputs: &checksandalerts.Webhook{URL: "https://example.com", BearerTokenFile: path}, RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities}
	}
	writeConfig(t, directory, c)
	loaded, err := config.Load(directory)
	if err != nil {
		t.Fatal(err)
	}
	expected := loaded.SecretFilePermissionErrors()
	if len(expected) != 2 {
		t.Fatal(expected)
	}
	var logged bytes.Buffer
	original := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(original)
	daemon, err := NewDaemon(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	for _, problem := range expected {
		if count := strings.Count(logged.String(), problem.Error()); count != 1 {
			t.Fatalf("expected one log for %s, got %d: %s", problem, count, logged.String())
		}
	}
}

func TestModulePublicCertificateAuthorityFiles(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	authority := filepath.Join(path, "public-ca.pem")
	if err := os.WriteFile(authority, []byte("public certificate bundle"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(authority, 0644); err != nil {
		t.Fatal(err)
	}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for _, usedAs := range []string{checksandalerts.UsedAsCheck, checksandalerts.UsedAsAlertDestination} {
		name := "public_ca_" + usedAs
		script := `def run_check(inputs, file_contents):
    return {"status": "ok" if file_contents["trusted_certificate_authority_file"] == "public certificate bundle" else "failed", "message": "read public certificate bundle"}

def deliver_alert(alert, inputs, file_contents):
    return {"status": "delivered", "message": "accepted"}

def verify_destination(inputs, file_contents):
    return (file_contents["trusted_certificate_authority_file"] == "public certificate bundle", "read public certificate bundle")
`
		testhelpers.InstallStarlarkModule(t, c.ExternalModuleDir, name, testhelpers.TestModuleDescription(name, usedAs, map[string]string{"trusted_certificate_authority_file": "string"}, nil), script)
		call := &checksandalerts.ModuleCall{Name: name, Inputs: map[string]any{"trusted_certificate_authority_file": authority}, ExecOptions: primitives.ExecOptions{RunAsUserAccount: account.Username, RunFromDirectory: "/"}}
		if usedAs == checksandalerts.UsedAsCheck {
			c.CheckGroups["public_ca"] = checksandalerts.CheckGroup{Check: checksandalerts.Check{Inputs: call}}
		} else {
			c.Alerts.Destinations = map[string]checksandalerts.Destination{"public_ca": {Inputs: call, RemindEvery: "never", TimeLimitPerDeliveryAttempt: "1m", AlertSeverities: testhelpers.AllSeverities}}
		}
	}
	writeConfig(t, path, c)
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRuntime(loaded); err != nil {
		t.Fatal(err)
	}
	check := loaded.CheckGroups["public_ca"].Checks["public_ca_check"].Inputs.(*checksandalerts.ModuleCall)
	observation := check.Run(context.Background(), &checksandalerts.CheckRun{})
	if observation.Status != "ok" {
		t.Fatal(observation)
	}
	result := (checksandalerts.DeliveryOf{Destination: loaded.Alerts.Destinations["public_ca"], Modules: loaded.Modules}).VerifyDestination(context.Background())
	if !result.AcceptsAddressAndCredential {
		t.Fatal(result)
	}
}
