package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/store"
	"mmwatchdog/internal/testhelpers"
)

// severityConfig loads text, written as it is, and returns the alert
// severity settings of every check, by key.
func severityConfig(t *testing.T, text string) (*config.Config, map[string]checksandalerts.AlertSeveritySettings) {
	t.Helper()
	c, err := config.Load(testhelpers.ConfigFileWithoutSeverities(t, text))
	if err != nil {
		t.Fatal(err)
	}
	settings := map[string]checksandalerts.AlertSeveritySettings{}
	for key, d := range config.CheckDefinitions(c) {
		settings[key] = d.Severity
	}
	return c, settings
}

// Every check that can alert states its alert_severity on the check
// itself: a named check under checks, or a direct check or a heartbeat on
// its group mapping, which is the check. Nothing is inherited.
func TestAlertSeverityIsWrittenOnEachCheck(t *testing.T) {
	t.Parallel()
	_, p := severityConfig(t, `alert_severity_when_this_node_goes_silent: warn
alert_severity_when_this_node_has_rebooted: info
keep_measurement_history_for: 7d
measurement_history_size_limit: 256MiB
heartbeat_receiver: {listen_on_address: '127.0.0.1:8444', tls_certificate_file: /etc/mmwatchdog/cert.pem, tls_private_key_file: /etc/mmwatchdog/key.pem}
check_groups:
  myapp:
    checks:
      systemd_unit_state: {systemd_unit_state: {systemd_unit: myapp.service, fail_when: the_unit_is_not_active}, alert_severity: sos}
      health: {http: {url: 'http://127.0.0.1/'}, alert_severity: warn, raise_to_sos_after_failing_for: 30m}
      memory: {only_record_readings: true, systemd_unit_resource: {systemd_unit: myapp.service, resource: memory}}
  root_filesystem: {node_filesystem: /, fail_when_measurement_above: {space_used_percent: 90}, alert_severity: info}
  data_filesystem_is_writable: {node_filesystem: /srv/data, alert_severity: sos}
  scratch_filesystem: {only_record_readings: true, node_filesystem: /scratch}
  backup: {heartbeat: {max_time_between_heartbeats: 26h, expected_bearer_token_file: /etc/mmwatchdog/backup}, alert_severity: sos}
keep_undelivered_alerts_for: 24h
undelivered_alerts_content_size_limit: 8MiB
`)
	want := map[string]checksandalerts.AlertSeveritySettings{
		"myapp/systemd_unit_state":                    {AlertSeverity: "sos", RaiseToSosAfterFailingFor: ""},
		"myapp/health":                                {AlertSeverity: "warn", RaiseToSosAfterFailingFor: "30m"},
		"myapp/memory":                                {},
		"root_filesystem/node_filesystem":             {AlertSeverity: "info", RaiseToSosAfterFailingFor: ""},
		"data_filesystem_is_writable/node_filesystem": {AlertSeverity: "sos", RaiseToSosAfterFailingFor: ""},
		"scratch_filesystem/node_filesystem":          {},
		"backup/heartbeat":                            {AlertSeverity: "sos", RaiseToSosAfterFailingFor: ""},
	}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("got  %+v\nwant %+v", p, want)
	}
}

// Changing alert_severity keeps a check's incidents, unlike changing the
// check: a reload to a higher severity raises the open incident on the
// next scheduler tick, and a lower one never lowers it.
func TestChangedAlertSeverityKeepsAndRaisesIncidents(t *testing.T) {
	t.Parallel()
	d, p, c := severityDaemon(t, map[string]checksandalerts.Destination{"chat": {RemindEvery: "never", Inputs: &checksandalerts.Webhook{URL: "https://chat.example/"}, AlertSeverities: testhelpers.AllSeverities}})
	now := time.Now().UTC()
	observeApp(t, d, func(st *incidents.CheckState) []checksandalerts.Event {
		return st.Observe(failApp(now), checksandalerts.Timing{}, false)
	})
	id := d.snapshot.Checks["app/shell"].FailureIncident.Incident
	reload := func(severity string) []checksandalerts.Event {
		t.Helper()
		group := c.CheckGroups["app"]
		group.Check.AlertSeverity = severity
		c.CheckGroups["app"] = group
		writeConfig(t, p, c)
		if _, e := d.reload(); e != nil {
			t.Fatal(e)
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.raiseOpenIncidents(time.Now().UTC())
	}
	events := reload("sos")
	st := d.snapshot.Checks["app/shell"]
	if st.FailureIncident.Incident != id || !reflect.DeepEqual(eventSummary(events), []string{"failure_alert_severity_raised sos"}) || st.AlertSeveritySettings.AlertSeverity != "sos" {
		t.Fatal("the incident did not stay and rise", events, st.FailureIncident)
	}
	if events = reload("info"); len(events) != 0 || d.snapshot.Checks["app/shell"].FailureIncident.AlertSeverity != "sos" {
		t.Fatal(events, d.snapshot.Checks["app/shell"].FailureIncident)
	}
}

// Missing, empty, misplaced, and invalid alert severity settings are
// refused where they are written.
func TestAlertSeverityValidation(t *testing.T) {
	t.Parallel()
	node := "keep_undelivered_alerts_for: 24h\nundelivered_alerts_content_size_limit: 8MiB\nalert_severity_when_this_node_goes_silent: sos\nalert_severity_when_this_node_has_rebooted: info\nkeep_measurement_history_for: 7d\nmeasurement_history_size_limit: 256MiB\n"
	for text, want := range map[string]string{
		node + "check_groups: {myapp: {checks: {health: {http: {url: 'http://127.0.0.1/'}}}}}\n":                                                                       "check_groups.myapp.checks.health.alert_severity: required: info, warn, or sos",
		node + "check_groups: {a: {shell: 'true'}}\n":                                                                                                                  "check_groups.a.alert_severity: required: info, warn, or sos",
		node + "check_groups: {a: {shell: 'true', alert_severity: ''}}\n":                                                                                              "check_groups.a.alert_severity: required: info, warn, or sos",
		node + "check_groups: {a: {shell: 'true', alert_severity: SOS}}\n":                                                                                             "check_groups.a.alert_severity: required: info, warn, or sos",
		node + "check_groups: {a: {shell: 'true', alert_severity: warn, raise_to_sos_after_failing_for: 500ms}}\n":                                                     "check_groups.a.raise_to_sos_after_failing_for: expected a duration of at least 1s",
		node + "check_groups: {a: {shell: 'true', alert_severity: warn, raise_to_sos_after_failing_for: ''}}\n":                                                        "a duration",
		node + "check_groups: {a: {alert_severity: warn, checks: {b: {shell: 'true', alert_severity: warn}}}}\n":                                                       "check_groups.a.alert_severity: set alert_severity on each check under checks, not on the check group",
		node + "check_groups: {a: {raise_to_sos_after_failing_for: 1h, checks: {b: {shell: 'true', alert_severity: warn}}}}\n":                                         "check_groups.a.raise_to_sos_after_failing_for: set raise_to_sos_after_failing_for on each check under checks",
		node + "check_groups: {a: {node_resource: cpu, alert_severity: warn}}\n":                                                                                       "check_groups.a.alert_severity: a node_resource check needs a limit to alert",
		node + "check_groups: {a: {checks: {m: {only_record_readings: true, node_resource: cpu, raise_to_sos_after_failing_for: 1h}}}}\n":                              "check_groups.a.checks.m.only_record_readings: a check that only records readings has no limits or alert severity",
		node + "check_groups: {a: {systemd_unit_resource: {systemd_unit: a.service, resource: memory}, alert_severity: warn}}\n":                                       "check_groups.a.alert_severity: a systemd_unit_resource check needs a limit to alert",
		node + "check_groups: {a: {node_resource: cpu, fail_when_measurement_above: {cpu_used_percent: 90}}}\n":                                                        "check_groups.a.only_record_readings: is required without alert_severity",
		node + "check_groups: {a: {node_filesystem: /, fail_when_measurement_below: {space_free_gib: 10}}}\n":                                                          "check_groups.a.only_record_readings: is required without alert_severity",
		node + "check_groups: {a: {only_record_readings: true, node_filesystem: /, raise_to_sos_after_failing_for: 1h}}\n":                                             "check_groups.a.only_record_readings: a check that only records readings has no limits or alert severity",
		node + "check_groups: {a: {node_filesystem: /, alert_severity: loud}}\n":                                                                                       "check_groups.a.alert_severity: required: info, warn, or sos",
		node + "check_groups: {a: {fail_when_measurement_above: {memory_used_percent: 90}, checks: {m: {only_record_readings: true, node_resource: memory}}}}\n":       "check_groups.a.fail_when_measurement_above: set fail_when_measurement_above on each check under checks, not on the check group",
		"alert_severity_when_this_node_goes_silent: sos\ncheck_groups: {a: {shell: 'true', alert_severity: warn}}\n":                                                   "alert_severity_when_this_node_has_rebooted: required: info, warn, or sos",
		"alert_severity_when_this_node_goes_silent: sos\nalert_severity_when_this_node_has_rebooted: loud\ncheck_groups: {a: {shell: 'true', alert_severity: warn}}\n": "alert_severity_when_this_node_has_rebooted: required: info, warn, or sos",
		node + "check_groups: {a: {shell: 'true', alert_severity: warn, cannot_check_alert_severity: sos}}\n":                                                          `unknown field "cannot_check_alert_severity"`,
		"check_groups: {a: {shell: 'true', alert_severity: warn}}\n":                                                                                                   "alert_severity_when_this_node_goes_silent: required: info, warn, or sos",
		"alert_severity_when_this_node_goes_silent: ''\ncheck_groups: {a: {shell: 'true', alert_severity: warn}}\n":                                                    "alert_severity_when_this_node_goes_silent: required: info, warn, or sos",
		"alert_severity_when_this_node_goes_silent: loud\ncheck_groups: {a: {shell: 'true', alert_severity: warn}}\n":                                                  "alert_severity_when_this_node_goes_silent: required: info, warn, or sos",
		node + "fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'hub.test:8445', keep_measurement_history_of_nodes_for: 7d, measurement_history_of_nodes_size_limit: 20GiB, raise_to_sos_when_silent_nodes_reach_count: ''}\ncheck_groups: {a: {shell: 'true', alert_severity: warn}}\n":    "expected an integer",
		node + "fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'hub.test:8445', keep_measurement_history_of_nodes_for: 7d, measurement_history_of_nodes_size_limit: 20GiB, raise_to_sos_when_silent_nodes_reach_count: 0}\ncheck_groups: {a: {shell: 'true', alert_severity: warn}}\n":     "raise_to_sos_when_silent_nodes_reach_count: the number of silent nodes",
		node + "fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'hub.test:8445', keep_measurement_history_of_nodes_for: 7d, measurement_history_of_nodes_size_limit: 20GiB, raise_to_sos_when_silent_nodes_reach_percent: 101}\ncheck_groups: {a: {shell: 'true', alert_severity: warn}}\n": "raise_to_sos_when_silent_nodes_reach_percent: the percentage of silent nodes",
		node + "fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'hub.test:8445', keep_measurement_history_of_nodes_for: 7d, measurement_history_of_nodes_size_limit: 20GiB, raise_to_sos_when_silent_nodes_reach_count: many}\ncheck_groups: {a: {shell: 'true', alert_severity: warn}}\n":  "expected an integer",
		node + "fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'hub.test:8445', keep_measurement_history_of_nodes_for: 7d, measurement_history_of_nodes_size_limit: 20GiB, raise_to_sos_when_silent_nodes_reach_count: 2.5}\ncheck_groups: {a: {shell: 'true', alert_severity: warn}}\n":   "expected an integer",
	} {
		if _, err := config.Load(testhelpers.ConfigFileWithoutSeverities(t, text)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", text, err, want)
		}
	}
	path := testhelpers.ConfigFile(t, "check_groups: {a: {shell: 'true'}}\n")
	for alerts, want := range map[string]string{
		"destinations: {ops: {remind_every: never, webhook: {url: 'https://example.com'}}}\n":                     "destinations.ops.alert_severities: required: a list of distinct severities from info, warn, sos",
		"destinations: {ops: {remind_every: never, webhook: {url: 'https://example.com'}, alert_severities: }}\n": "expected a list",
		// An empty list is a choice: only the checks that name the destination reach it.
		"destinations: {ops: {remind_every: never, webhook: {url: 'https://example.com'}, alert_severities: []}}\n":          "",
		"destinations: {ops: {remind_every: never, webhook: {url: 'https://example.com'}, alert_severities: [sos, sos]}}\n":  "destinations.ops.alert_severities: required",
		"destinations: {ops: {remind_every: never, webhook: {url: 'https://example.com'}, alert_severities: [critical]}}\n":  "destinations.ops.alert_severities: required",
		"destinations: {ops: {remind_every: never, webhook: {url: 'https://example.com'}, alert_severities: [warn, sos]}}\n": "",
		"destinations: {ops: {remind_every: never, webhook: {url: 'https://example.com'}, alert_severities: [info]}}\n":      "",
	} {
		os.WriteFile(filepath.Join(path, "alert_destinations.yml"), []byte(alerts), 0600)
		_, err := config.Load(path)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%q: got %v, want %q", alerts, err, want)
		}
	}
	for _, value := range []string{"5", "0.5", "100"} {
		if c, err := config.Load(testhelpers.ConfigFile(t, "fleet_hub_role: {listen_on_address: '0.0.0.0:8445', address_for_nodes: 'hub.test:8445', raise_to_sos_when_silent_nodes_reach_percent: "+value+"}\ncheck_groups: {a: {shell: 'true'}}\n")); err != nil || c.FleetHubRole.RaiseToSosWhenSilentNodesReachPercent == nil {
			t.Errorf("%s: %v", value, err)
		}
	}
}

func eventSummary(events []checksandalerts.Event) []string {
	out := []string{}
	for _, e := range events {
		out = append(out, e.Type+" "+e.AlertSeverity)
	}
	return out
}

// A measurement check fails with its alert_severity at its limit; one that
// only records never alerts, not even when it cannot read its measurement.
func TestMeasurementChecksAlertAtTheirSeverity(t *testing.T) {
	t.Parallel()
	c, err := config.Load(testhelpers.ConfigFile(t, "check_groups: {disk: {alert_after_failing_for: 0s, node_filesystem: /, fail_when_measurement_above: {space_used_percent: 85}, alert_severity: sos}, cpu: {only_record_readings: true, alert_after_failing_for: 0s, node_resource: cpu}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	snap := incidents.FreshSnapshot()
	now := time.Now().UTC()
	snap.Reconcile(c, config.CheckDefinitions(c), now)
	o := checksandalerts.MeasurementObservation(c.CheckGroups["disk"].Checks["node_filesystem"].Check, map[string]checksandalerts.Measurement{"space_used_percent": {Value: 90, Unit: "percent"}}, nil)
	o.At = now
	if events := snap.Checks["disk/node_filesystem"].Observe(o, c.CheckGroups["disk"].Timing, false); !reflect.DeepEqual(eventSummary(events), []string{"failed sos"}) {
		t.Fatal(events)
	}
	cpu := snap.Checks["cpu/node_resource"]
	unknown := checksandalerts.Observation{Status: "unknown", Message: "cannot read counters", At: now}
	if events := cpu.Observe(unknown, c.CheckGroups["cpu"].Timing, false); len(events) != 0 || cpu.CannotCheckIncident.Incident != "" || cpu.Summary(now, false) != "unknown" {
		t.Fatal(events, cpu)
	}
}

// raise_to_sos_after_failing_for raises an incident whose check has been
// failing for that long, once.
// Maintenance holds the raise of an announced incident; an incident not
// yet announced takes the raised severity when it is.
func TestEscalationByTime(t *testing.T) {
	t.Parallel()
	c, err := config.Load(testhelpers.ConfigFile(t, "check_groups: {app: {shell: 'false', alert_after_failing_for: 0s, alert_severity: warn, raise_to_sos_after_failing_for: 30m}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	snap := incidents.FreshSnapshot()
	now := time.Now().UTC()
	snap.Reconcile(c, config.CheckDefinitions(c), now)
	st := snap.Checks["app/shell"]
	timing := c.CheckGroups["app"].Timing
	st.Observe(checksandalerts.Observation{Status: "failed", Message: "exit 1", At: now}, timing, false)
	if events := st.RaiseAfterTime(now.Add(29*time.Minute), false); len(events) != 0 {
		t.Fatal("raised early", events)
	}
	if events := st.RaiseAfterTime(now.Add(30*time.Minute), true); len(events) != 0 || st.FailureIncident.AlertSeverity != "warn" {
		t.Fatal("raised during maintenance", events)
	}
	events := st.RaiseAfterTime(now.Add(30*time.Minute), false)
	if !reflect.DeepEqual(eventSummary(events), []string{"failure_alert_severity_raised sos"}) || events[0].Incident != st.FailureIncident.Incident || events[0].Message != "app/shell has been failing for 30m, so this alert is now SOS (raise_to_sos_after_failing_for: 30m)" {
		t.Fatal(events)
	}
	if events = st.RaiseAfterTime(now.Add(time.Hour), false); len(events) != 0 {
		t.Fatal("raised twice", events)
	}
	// Reminders count from the raise and carry the incident's current
	// severity.
	ops := map[string]checksandalerts.Destination{"ops": {AlertSeverities: testhelpers.AllSeverities, RemindEvery: "1h"}}
	if events = st.Reminders(ops, now.Add(90*time.Minute-time.Second)); len(events) != 0 {
		t.Fatal("reminded before one remind_every after the raise", events)
	}
	if events = st.Reminders(ops, now.Add(90*time.Minute)); len(events) != 1 || events[0].AlertSeverity != "sos" {
		t.Fatal(events)
	}
	// An incident opened during maintenance rises unannounced and is
	// announced at its raised severity.
	snap = incidents.FreshSnapshot()
	snap.Reconcile(c, config.CheckDefinitions(c), now)
	st = snap.Checks["app/shell"]
	st.Observe(checksandalerts.Observation{Status: "failed", Message: "exit 1", At: now}, timing, true)
	if events = st.RaiseAfterTime(now.Add(31*time.Minute), true); len(events) != 0 || st.FailureIncident.AlertSeverity != "sos" {
		t.Fatal(events, st.FailureIncident)
	}
	events = st.Observe(checksandalerts.Observation{Status: "failed", Message: "exit 1", At: now.Add(32 * time.Minute)}, timing, false)
	if !reflect.DeepEqual(eventSummary(events), []string{"failed sos"}) || !reflect.DeepEqual(st.FailureIncident.AnnouncedAlertSeverities, []string{"sos"}) {
		t.Fatal(events)
	}
}

// When MMWatchdog cannot run a check, the cannot-check alert has the check's
// current severity, including a raise of its open failure, and
// raise_to_sos_after_failing_for raises it as it raises a failure.
func TestCannotCheckSeverity(t *testing.T) {
	t.Parallel()
	c, err := config.Load(testhelpers.ConfigFile(t, "check_groups: {app: {shell: 'false', alert_after_failing_for: 0s, alert_severity: warn, raise_to_sos_after_failing_for: 10m}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	timing := c.CheckGroups["app"].Timing
	unknown := func(at time.Time) checksandalerts.Observation {
		return checksandalerts.Observation{Status: "unknown", Message: "cannot run", At: at}
	}
	snap := incidents.FreshSnapshot()
	snap.Reconcile(c, config.CheckDefinitions(c), now)
	st := snap.Checks["app/shell"]
	if got := eventSummary(st.Observe(unknown(now), timing, false)); !reflect.DeepEqual(got, []string{"cannot_check warn"}) {
		t.Fatal(got)
	}
	if got := eventSummary(st.RaiseAfterTime(now.Add(10*time.Minute), false)); !reflect.DeepEqual(got, []string{"cannot_check_alert_severity_raised sos"}) {
		t.Fatal(got)
	}
	// A failure raised to sos makes a later cannot-check incident sos.
	snap = incidents.FreshSnapshot()
	snap.Reconcile(c, config.CheckDefinitions(c), now)
	st = snap.Checks["app/shell"]
	st.Observe(checksandalerts.Observation{Status: "failed", Message: "exit 1", At: now}, timing, false)
	st.RaiseAfterTime(now.Add(10*time.Minute), false)
	if got := eventSummary(st.Observe(unknown(now.Add(11*time.Minute)), timing, false)); !reflect.DeepEqual(got, []string{"cannot_check sos"}) {
		t.Fatal(got)
	}
}

// severityDaemon starts a standalone daemon with these destinations and one
// shell check, app/shell, that fails and recovers at once.
func severityDaemon(t *testing.T, destinations map[string]checksandalerts.Destination) (*Daemon, string, *config.Config) {
	t.Helper()
	p, c := daemonConfig(t)
	c.Alerts.Destinations = destinations
	c.CheckGroups["app"] = checksandalerts.CheckGroup{Timing: checksandalerts.Timing{AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}, Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "false"}}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, p, c
}

// queuedByDestination lists the queued deliveries as "type severity" by
// destination, in order.
func queuedByDestination(t *testing.T, d *Daemon) map[string][]string {
	t.Helper()
	rows, e := d.store.DB.Query("SELECT destination, json_extract(event,'$.type') || ' ' || json_extract(event,'$.alert_severity') FROM outbox ORDER BY seq")
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var name, alert string
		rows.Scan(&name, &alert)
		out[name] = append(out[name], alert)
	}
	return out
}

// observeApp applies observations to app/shell and records their events,
// with any extra events the step function returns.
func observeApp(t *testing.T, d *Daemon, step func(st *incidents.CheckState) []checksandalerts.Event) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if e := d.record(step(d.snapshot.Checks["app/shell"])); e != nil {
		t.Fatal(e)
	}
}

// setDestinations changes the destinations as a reload would.
func setDestinations(d *Daemon, destinations map[string]checksandalerts.Destination) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.config.Alerts.Destinations = destinations
}

func TestDestinationAlertSeverities(t *testing.T) {
	t.Parallel()
	d, _, _ := severityDaemon(t, map[string]checksandalerts.Destination{
		"pager": {RemindEvery: "never", Inputs: &checksandalerts.Webhook{URL: "https://pager.example/"}, AlertSeverities: []string{"sos"}},
		"chat":  {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://chat.example/"}},
		"warn":  {Inputs: &checksandalerts.Webhook{URL: "https://warn.example/"}, AlertSeverities: []string{"warn"}, RemindEvery: "1h"},
	})
	byDestination := func() map[string][]string { return queuedByDestination(t, d) }
	now := time.Now().UTC()
	d.mu.Lock()
	st := d.snapshot.Checks["app/shell"]
	timing := d.checks["app/shell"].def.Timing
	events := st.Observe(checksandalerts.Observation{Status: "failed", Message: "exit 1", At: now}, timing, false)
	events = append(events, st.Reminders(d.route().Destinations, now.Add(time.Hour))...)
	events = append(events, st.Raise(&st.FailureIncident, checksandalerts.FailureIncidentEvents, "sos", "test", now.Add(2*time.Hour), false)...)
	events = append(events, st.Reminders(d.route().Destinations, now.Add(3*time.Hour))...)
	events = append(events, st.Observe(checksandalerts.Observation{Status: "ok", Message: "exit 0", At: now.Add(4 * time.Hour)}, timing, false)...)
	// A warn incident that ends at warn never reaches the pager.
	events = append(events, st.Observe(checksandalerts.Observation{Status: "failed", Message: "exit 1", At: now.Add(5 * time.Hour)}, timing, false)...)
	events = append(events, st.Observe(checksandalerts.Observation{Status: "ok", Message: "exit 0", At: now.Add(6 * time.Hour)}, timing, false)...)
	events = append(events, incidents.MaintenanceEvent("maintenance_on", []string{"app"}, checksandalerts.Window{Until: now.Add(7 * time.Hour)}, now, "deploy"))
	err := d.record(events)
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	got := byDestination()
	want := map[string][]string{
		"pager": {"failure_alert_severity_raised sos", "recovered sos"},
		"chat":  {"failed warn", "failure_alert_severity_raised sos", "recovered sos", "failed warn", "recovered warn", "maintenance_on info"},
		"warn":  {"failed warn", "reminder warn", "recovered sos", "failed warn", "recovered warn"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
}

// Fleet alert grouping never puts alerts of different severities, or
// recoveries of incidents sent with different severities, in one message.
func TestFleetGroupingKeepsSeveritiesApart(t *testing.T) {
	t.Parallel()
	sink := captureAlerts(t)
	hub, _, _ := startHubWith(t, map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: sink.URL}}})
	now := time.Now().UTC()
	alert := func(node, kind, severity string, sent ...string) {
		e := checkAlert(kind, incidents.ID(), "web", "http", now)
		e.AlertSeverity, e.AlertSeveritiesSentForIncident = severity, sent
		acceptAlert(t, hub, node, e)
	}
	alert("app-1", "failed", "warn")
	groupAt(t, hub, now)
	alert("app-2", "failed", "warn")
	alert("app-3", "failed", "sos")
	alert("app-4", "failed", "sos")
	alert("app-5", "recovered", "sos", "warn", "sos")
	alert("app-6", "recovered", "sos", "sos")
	groupAt(t, hub, now.Add(time.Second))
	groupAt(t, hub, now.Add(2*time.Minute))
	got := []string{}
	for _, e := range queuedEvents(t, hub) {
		got = append(got, e.Type+" "+e.AlertSeverity+" "+strings.Join(e.AlertSeveritiesSentForIncident, ",")+" "+strings.Join(e.Nodes, ","))
	}
	// The first message of each severity and recovery group goes out at
	// once; app-2's warn waits for the window app-1's opened.
	want := []string{"failed warn  ", "failed sos  app-3,app-4", "recovered sos warn,sos ", "recovered sos sos ", "failed warn  "}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

// Each joined node's silence alert takes the severity its reports carry,
// also after the node went quiet and after the hub restarted. The hub's
// own setting reaches its hub-outage alerter in the fleet settings.
func TestNodeSilenceSeverityFollowsTheNode(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	if fleetCheck(hub, "app-1") != nil {
		t.Fatal("checked before joining")
	}
	node.mu.Lock()
	node.config.AlertSeverityWhenThisNodeGoesSilent = "warn"
	node.mu.Unlock()
	node.report(context.Background())
	// The join carries it, so the hub knows it before the first report.
	hub.hub.Mu.Lock()
	joined := hub.hub.Nodes["app-1"].AlertSeverityWhenThisNodeGoesSilent
	hub.hub.Mu.Unlock()
	if p := fleetCheck(hub, "app-1").AlertSeveritySettings; p.AlertSeverity != "warn" || joined != "warn" {
		t.Fatal(p, joined)
	}
	node.mu.Lock()
	node.config.AlertSeverityWhenThisNodeGoesSilent = "sos"
	node.mu.Unlock()
	node.report(context.Background())
	if p := fleetCheck(hub, "app-1").AlertSeveritySettings; p.AlertSeverity != "sos" {
		t.Fatal("the report did not change it", p)
	}
	received := fleetCheck(hub, "app-1").Receipt
	// A restarted hub learns it from the node's saved report.
	hub.mu.Lock()
	hub.snapshot.Checks[fleet.FleetKey("app-1")].AlertSeveritySettings.AlertSeverity = "warn"
	hub.startFleet(hub.hub.Members(), time.Now())
	hub.snapshot.Checks[fleet.FleetKey("app-1")].Deadline = received.Add(3 * reportInterval)
	hub.mu.Unlock()
	tick(hub, received.Add(3*reportInterval))
	events := intakeEvents(t, hub)
	if len(events) != 1 || events[0].Type != "failed" || events[0].AlertSeverity != "sos" {
		t.Fatal(events)
	}
	// The hub-outage alerter's alert about the hub takes the hub's setting.
	hub.mu.Lock()
	settings := hub.hub.FleetSettings.Load()
	hub.mu.Unlock()
	if settings.AlertSeverityWhenTheHubGoesSilent != "sos" {
		t.Fatal(settings)
	}
	other := *settings
	other.AlertSeverityWhenTheHubGoesSilent = "info"
	other.Version = other.ComputeVersion()
	node.mu.Lock()
	node.watching = true
	node.applyFleet(time.Now())
	if p := node.snapshot.Checks[fleet.FleetKey(checksandalerts.HubCheck)].AlertSeveritySettings; p.AlertSeverity != "sos" {
		node.mu.Unlock()
		t.Fatal(p)
	}
	err := node.applyFleetSettings(&other)
	p := node.snapshot.Checks[fleet.FleetKey(checksandalerts.HubCheck)].AlertSeveritySettings
	node.mu.Unlock()
	if err != nil || p.AlertSeverity != "info" {
		t.Fatal(err, p)
	}
}

// intakeEvents lists the hub's own alerts waiting in fleet alert grouping.
func intakeEvents(t *testing.T, hub *Daemon) []checksandalerts.Event {
	t.Helper()
	rows, e := hub.store.DB.Query("SELECT event FROM intake WHERE node=? ORDER BY seq", hub.store.Node)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	out := []checksandalerts.Event{}
	for rows.Next() {
		var b []byte
		rows.Scan(&b)
		var event checksandalerts.Event
		if e = json.Unmarshal(b, &event); e != nil {
			t.Fatal(e)
		}
		if event.CheckGroup == checksandalerts.NodeReportsCheckGroup {
			out = append(out, event)
		}
	}
	return out
}

// raise_to_sos_when_silent_nodes_reach_count raises every silent node's alert to sos
// while that many are silent at once, and they stay sos after fewer are.
func TestSilentNodesRaiseToSosThreshold(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		threshold      config.FleetHubRole
		silent, joined int
		reached        bool
	}{
		{config.FleetHubRole{RaiseToSosWhenSilentNodesReachCount: new(2)}, 1, 10, false},
		{config.FleetHubRole{RaiseToSosWhenSilentNodesReachCount: new(2)}, 2, 10, true},
		{config.FleetHubRole{RaiseToSosWhenSilentNodesReachPercent: new(5.0)}, 1, 30, false},
		{config.FleetHubRole{RaiseToSosWhenSilentNodesReachPercent: new(5.0)}, 2, 30, true},
		{config.FleetHubRole{RaiseToSosWhenSilentNodesReachPercent: new(0.5)}, 1, 200, true},
		{config.FleetHubRole{}, 100, 100, false},
	} {
		if got := tc.threshold.SilentNodesReachSosThreshold(tc.silent, tc.joined); got != tc.reached {
			t.Errorf("%+v: %v", tc, got)
		}
	}
	hub := newHubDaemon(t, nil)
	hub.mu.Lock()
	hub.config.FleetHubRole.RaiseToSosWhenSilentNodesReachCount = new(2)
	hub.mu.Unlock()
	now := time.Now().UTC()
	for _, name := range []string{"app-1", "app-2", "app-3"} {
		hub.joined(name, "192.0.2.1", now, nodeSettings("warn"))
	}
	deadline := func(name string, at time.Time) {
		hub.mu.Lock()
		hub.snapshot.Checks[fleet.FleetKey(name)].Deadline = at
		hub.mu.Unlock()
	}
	deadline("app-1", now.Add(time.Minute))
	deadline("app-2", now.Add(2*time.Minute))
	deadline("app-3", now.Add(time.Hour))
	tick(hub, now.Add(time.Minute))
	if got := eventSummary(intakeEvents(t, hub)); !reflect.DeepEqual(got, []string{"failed warn"}) {
		t.Fatal("one silent node", got)
	}
	tick(hub, now.Add(2*time.Minute))
	got := []string{}
	for _, e := range intakeEvents(t, hub) {
		got = append(got, e.Type+" "+e.Check+" "+e.AlertSeverity)
	}
	want := []string{"failed app-1 warn", "failed app-2 warn", "failure_alert_severity_raised app-1 sos", "failure_alert_severity_raised app-2 sos"}
	if !reflect.DeepEqual(got, want) || intakeEvents(t, hub)[2].Message != "2 of 3 nodes are not reporting to the hub at the same time, so this alert is now SOS (raise_to_sos_when_silent_nodes_reach_count: 2)" {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	// app-2 reports again: app-1 stays sos, and a later silent node alone
	// is warn.
	hub.receipt("app-2", time.Time{}, nodeSettings("warn"), now.Add(3*time.Minute))
	tick(hub, now.Add(3*time.Minute))
	if st := fleetCheck(hub, "app-1"); st.FailureIncident.AlertSeverity != "sos" {
		t.Fatal(st.FailureIncident)
	}
	deadline("app-3", now.Add(4*time.Minute))
	tick(hub, now.Add(4*time.Minute))
	events := intakeEvents(t, hub)
	if last := events[len(events)-1]; last.Type != "failure_alert_severity_raised" || last.Check != "app-3" {
		// Two are silent again: app-1 and app-3.
		t.Fatal(eventSummary(events))
	}
}

// mmhotlineSummary is each queued MMHotline alert as "severity key: title".
func mmhotlineSummary(t *testing.T, d *Daemon) []string {
	t.Helper()
	out := []string{}
	for _, a := range mmhotlineQueued(t, d) {
		out = append(out, a["severity"]+" "+a["sender_incident_key"]+": "+a["title"])
	}
	return out
}

// The mmhotline destination passes MMWatchdog's severity through: a failure at its
// incident's severity, a raise as a new alert for the same key, the
// resolution at the highest severity reached, and notices as info. With
// alert_severities it takes only those, so a raise can be its first alert.
func TestMMHotlineSeverityPassesThrough(t *testing.T) {
	t.Parallel()
	for _, onlySos := range []bool{false, true} {
		d := mmhotlineNode(t)
		if onlySos {
			d.mu.Lock()
			dest := d.config.Alerts.Destinations[mmhotlineDestinationName]
			dest.AlertSeverities = []string{"sos"}
			d.config.Alerts.Destinations[mmhotlineDestinationName] = dest
			d.mu.Unlock()
		}
		now := time.Now().UTC()
		d.mu.Lock()
		st := d.snapshot.Checks["app/shell"]
		timing := checksandalerts.Timing{}
		events := st.Observe(checksandalerts.Observation{Status: "failed", Message: "exit 1", At: now}, timing, false)
		events = append(events, st.Raise(&st.FailureIncident, checksandalerts.FailureIncidentEvents, "sos", "test", now, false)...)
		events = append(events, st.Observe(checksandalerts.Observation{Status: "ok", Message: "exit 0", At: now}, timing, false)...)
		// A warn incident that stays warn.
		events = append(events, st.Observe(checksandalerts.Observation{Status: "failed", Message: "exit 1", At: now}, timing, false)...)
		events = append(events, st.Observe(checksandalerts.Observation{Status: "ok", Message: "exit 0", At: now}, timing, false)...)
		events = append(events, incidents.MaintenanceEvent("maintenance_on", []string{"app"}, checksandalerts.Window{Until: now.Add(time.Hour)}, now, "maintenance"))
		err := d.record(events)
		d.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			"warn " + incidentKeyFor(d, "app/shell failed") + ": web-3: app/shell failed",
			"sos " + incidentKeyFor(d, "app/shell failed") + ": web-3: app/shell still failing, now SOS",
			"resolved-sos " + incidentKeyFor(d, "app/shell failed") + ": web-3: app/shell is ok again",
			"warn " + incidentKeyFor(d, "app/shell failed") + ": web-3: app/shell failed",
			"resolved-warn " + incidentKeyFor(d, "app/shell failed") + ": web-3: app/shell is ok again",
			"info : web-3: maintenance on for app",
		}
		if onlySos {
			want = want[1:3]
		}
		if got := mmhotlineSummary(t, d); !reflect.DeepEqual(got, want) {
			t.Errorf("only sos %v:\n got %q\nwant %q", onlySos, got, want)
		}
	}
}

// A fleet key's alert carries the highest severity of its open incidents.
// Its SOS resolves as soon as no node is sos, while nodes still open at
// warn keep the key as a warn key: a later raise opens a new SOS, and the
// last recovery is resolved-warn. The key's state survives a hub restart.
func TestMMHotlineFleetKeySeverity(t *testing.T) {
	t.Parallel()
	hub, _, _ := startHubWith(t, mmhotlineDestinations(t))
	now := time.Now().UTC()
	a1, a2, a3 := incidents.ID(), incidents.ID(), incidents.ID()
	step := 0
	send := func(hub *Daemon, node, kind, id, severity string, sent ...string) {
		e := checkAlert(kind, id, "myapp", "http", now)
		e.AlertSeverity, e.AlertSeveritiesSentForIncident = severity, sent
		acceptAlert(t, hub, node, e)
		step++
		groupAt(t, hub, now.Add(time.Duration(step)*2*time.Minute))
	}
	a4 := incidents.ID()
	send(hub, "app-1", "failed", a1, "warn")
	send(hub, "app-2", "failed", a2, "sos")
	send(hub, "app-4", "failed", a4, "warn")
	send(hub, "app-2", "recovered", a2, "sos", "sos")
	hub.Close()
	restarted, err := NewDaemon(hub.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
	send(restarted, "app-3", "failed", a3, "warn")
	send(restarted, "app-3", checksandalerts.FailureIncidentEvents.Raised, a3, "sos")
	send(restarted, "app-3", "recovered", a3, "sos", "warn", "sos")
	send(restarted, "app-4", "recovered", a4, "warn", "warn")
	send(restarted, "app-1", "recovered", a1, "warn", "warn")
	want := []string{
		"warn " + incidentKeyFor(hub, "myapp/http failed") + ": app-1: myapp/http failed",
		"sos " + incidentKeyFor(hub, "myapp/http failed") + ": app-2: myapp/http failed",
		"sos " + incidentKeyFor(hub, "myapp/http failed") + ": app-4: myapp/http failed",
		"resolved-sos " + incidentKeyFor(hub, "myapp/http failed") + ": myapp/http is no longer SOS on any node",
		"warn " + incidentKeyFor(hub, "myapp/http failed") + ": app-3: myapp/http failed",
		"sos " + incidentKeyFor(hub, "myapp/http failed") + ": app-3: myapp/http still failing, now SOS",
		"resolved-sos " + incidentKeyFor(hub, "myapp/http failed") + ": myapp/http is no longer SOS on any node",
		"resolved-warn " + incidentKeyFor(hub, "myapp/http failed") + ": myapp/http is ok on every node again",
	}
	queued := mmhotlineQueued(t, restarted)
	got := []string{}
	for _, a := range queued {
		got = append(got, a["severity"]+" "+a["sender_incident_key"]+": "+a["title"])
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if !strings.HasPrefix(queued[3]["message"], "myapp/http is no longer SOS on any node; still Warning on app-1, app-4.") ||
		!strings.HasPrefix(queued[7]["message"], "myapp/http is ok on every node again (was failing on app-1, app-4).") {
		t.Fatal(queued[3]["message"], queued[7]["message"])
	}
}

// An incident a joined node sent straight to MMHotline at sos counts as
// sos for its key, and a report sent before an SOS resolution cannot add
// its incident again.
func TestMMHotlineSosSentWithoutTheHubResolvesFirst(t *testing.T) {
	t.Parallel()
	hub, _, _ := startHubWith(t, mmhotlineDestinations(t))
	now := time.Now().UTC()
	warn, direct := incidents.ID(), incidents.ID()
	acceptAlert(t, hub, "app-1", checkAlert("failed", warn, "myapp", "http", now))
	groupAt(t, hub, now)
	reconcile := func(reporter string, r store.ReportedIncidents) {
		t.Helper()
		hub.mu.Lock()
		defer hub.mu.Unlock()
		if e := hub.store.ReconcileIncidentSets(map[string]store.ReportedIncidents{reporter: r}, hub.config.Alerts.Destinations, false, time.Now()); e != nil {
			t.Fatal(e)
		}
	}
	sent := checksandalerts.Event{Node: "app-2", CheckGroup: "myapp", Check: "http", Type: "failed", Incident: direct, AlertSeverity: "sos", AlertSeveritiesSentForIncident: []string{"sos"}}
	shownWarn := map[string]checksandalerts.Event{warn: {Node: "app-1", CheckGroup: "myapp", Check: "http", Type: "failed", Incident: warn, AlertSeverity: "warn", AlertSeveritiesSentForIncident: []string{"warn"}}}
	reconcile("app-2", store.ReportedIncidents{Sent: now.Add(time.Second), Incidents: map[string]checksandalerts.Event{}, SentWithoutTheHub: map[string]map[string]checksandalerts.Event{mmhotlineDestinationName: {direct: sent}}})
	reconcile("app-1", store.ReportedIncidents{Sent: now.Add(time.Second), Incidents: shownWarn})
	got := mmhotlineSummary(t, hub)
	if len(got) != 2 || got[1] != "resolved-sos "+incidentKeyFor(hub, "myapp/http failed")+": myapp/http is no longer SOS on any node" {
		t.Fatal(got)
	}
	// app-2's older report still showing the incident adds nothing.
	reconcile("app-2", store.ReportedIncidents{Sent: now.Add(2 * time.Second), Incidents: map[string]checksandalerts.Event{direct: sent}})
	reconcile("app-1", store.ReportedIncidents{Sent: now.Add(time.Minute), Incidents: map[string]checksandalerts.Event{}})
	got = mmhotlineSummary(t, hub)
	if len(got) != 3 || got[2] != "resolved-warn "+incidentKeyFor(hub, "myapp/http failed")+": app-1: myapp/http is ok again" {
		t.Fatal(got)
	}
}

// A raise the hub never delivered, such as one a joined node sent straight
// to MMHotline, reaches the hub's incident set in reports and decides the
// resolution. The direct fallback records the severity it sent.
func TestMMHotlineReportedRaiseDecidesResolution(t *testing.T) {
	t.Parallel()
	hub, _, _ := startHubWith(t, mmhotlineDestinations(t))
	now := time.Now().UTC()
	incident := incidents.ID()
	acceptAlert(t, hub, "app-1", checkAlert("failed", incident, "myapp", "http", now))
	groupAt(t, hub, now)
	reconcile := func(r store.ReportedIncidents) {
		t.Helper()
		hub.mu.Lock()
		defer hub.mu.Unlock()
		if e := hub.store.ReconcileIncidentSets(map[string]store.ReportedIncidents{"app-1": r}, hub.config.Alerts.Destinations, false, time.Now()); e != nil {
			t.Fatal(e)
		}
	}
	raised := checksandalerts.Event{Node: "app-1", CheckGroup: "myapp", Check: "http", Type: "failed", Incident: incident, AlertSeverity: "sos", AlertSeveritiesSentForIncident: []string{"warn", "sos"}}
	reconcile(store.ReportedIncidents{Sent: now.Add(time.Second), Incidents: map[string]checksandalerts.Event{incident: raised}})
	reconcile(store.ReportedIncidents{Sent: now.Add(time.Minute), Incidents: map[string]checksandalerts.Event{}})
	got := mmhotlineSummary(t, hub)
	if len(got) != 2 || got[1] != "resolved-sos "+incidentKeyFor(hub, "myapp/http failed")+": app-1: myapp/http is ok again" {
		t.Fatal(got)
	}
	// The fallback keeps every severity it sent for the incident.
	node := mmhotlineNode(t)
	node.mu.Lock()
	defer node.mu.Unlock()
	tx, e := node.store.DB.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	id := incidents.ID()
	for _, kind := range []string{"failed", checksandalerts.FailureIncidentEvents.Raised} {
		e := checkAlert(kind, id, "app", "shell", now)
		if kind != "failed" {
			e.AlertSeverity = "sos"
		}
		if err := store.QueueMMHotline(tx, now.UnixNano(), mmhotlineDestinationName, node.config.Alerts.Destinations[mmhotlineDestinationName], e, []checksandalerts.Event{e}, store.QueuedBy{Role: store.FallbackSender, Node: "web-3"}, now); err != nil {
			t.Fatal(err)
		}
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	// Each send is recorded once MMHotline accepts it.
	for {
		heads, e := node.store.Pending()
		if e != nil {
			t.Fatal(e)
		}
		if len(heads) == 0 {
			break
		}
		if _, e = node.store.Delivered(heads[0], nil); e != nil {
			t.Fatal(e)
		}
	}
	var kind, severity string
	if e = node.store.DB.QueryRow("SELECT kind, alert_severities_sent_for_incident FROM incidents_sent_without_the_hub WHERE destination=? AND incident_id=?", mmhotlineDestinationName, id).Scan(&kind, &severity); e != nil || kind != "failed" || severity != "warn,sos" {
		t.Fatal(kind, severity, e)
	}
}

func failApp(at time.Time) checksandalerts.Observation {
	return checksandalerts.Observation{Status: "failed", Message: "exit 1", At: at}
}
func okApp(at time.Time) checksandalerts.Observation {
	return checksandalerts.Observation{Status: "ok", Message: "exit 0", At: at}
}

// A recovery goes where its incident's alerts went, also after
// alert_severities changed during the incident: a destination narrowed to
// sos still hears that the warn incident it got ended, and one widened
// from sos does not get a recovery for a failure it never received.
func TestDestinationFilterChangedDuringIncident(t *testing.T) {
	t.Parallel()
	webhook := func(url string, severities ...string) checksandalerts.Destination {
		return checksandalerts.Destination{RemindEvery: "never", Inputs: &checksandalerts.Webhook{URL: url}, AlertSeverities: severities}
	}
	d, _, _ := severityDaemon(t, map[string]checksandalerts.Destination{"chat": webhook("https://chat.example/", testhelpers.AllSeverities...), "pager": webhook("https://pager.example/", "sos")})
	now := time.Now().UTC()
	observeApp(t, d, func(st *incidents.CheckState) []checksandalerts.Event {
		return st.Observe(failApp(now), checksandalerts.Timing{}, false)
	})
	setDestinations(d, map[string]checksandalerts.Destination{"chat": webhook("https://chat.example/", "sos"), "pager": webhook("https://pager.example/", testhelpers.AllSeverities...)})
	observeApp(t, d, func(st *incidents.CheckState) []checksandalerts.Event {
		return st.Observe(okApp(now.Add(time.Minute)), checksandalerts.Timing{}, false)
	})
	// The next incident follows the new filters.
	observeApp(t, d, func(st *incidents.CheckState) []checksandalerts.Event {
		events := st.Observe(failApp(now.Add(2*time.Minute)), checksandalerts.Timing{}, false)
		return append(events, st.Observe(okApp(now.Add(3*time.Minute)), checksandalerts.Timing{}, false)...)
	})
	want := map[string][]string{"chat": {"failed warn", "recovered warn"}, "pager": {"failed warn", "recovered warn"}}
	if got := queuedByDestination(t, d); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	var remembered int
	d.store.DB.QueryRow("SELECT COUNT(*) FROM incident_alert_destinations").Scan(&remembered)
	if remembered != 0 {
		t.Fatal("closed incidents are still remembered", remembered)
	}
}

// Narrowing the mmhotline destination's alert_severities during a warn
// incident still closes that incident's row, so its key resolves and a
// later sos incident on the key gets its resolved-sos.
func TestMMHotlineFilterNarrowedDuringIncident(t *testing.T) {
	t.Parallel()
	d := mmhotlineNode(t)
	now := time.Now().UTC()
	observeApp(t, d, func(st *incidents.CheckState) []checksandalerts.Event {
		return st.Observe(failApp(now), checksandalerts.Timing{}, false)
	})
	d.mu.Lock()
	dest := d.config.Alerts.Destinations[mmhotlineDestinationName]
	dest.AlertSeverities = []string{"sos"}
	d.config.Alerts.Destinations[mmhotlineDestinationName] = dest
	d.mu.Unlock()
	observeApp(t, d, func(st *incidents.CheckState) []checksandalerts.Event {
		events := st.Observe(okApp(now.Add(time.Minute)), checksandalerts.Timing{}, false)
		events = append(events, st.Observe(failApp(now.Add(2*time.Minute)), checksandalerts.Timing{}, false)...)
		events = append(events, st.Raise(&st.FailureIncident, checksandalerts.FailureIncidentEvents, "sos", "test", now.Add(3*time.Minute), false)...)
		return append(events, st.Observe(okApp(now.Add(4*time.Minute)), checksandalerts.Timing{}, false)...)
	})
	want := []string{
		"warn " + incidentKeyFor(d, "app/shell failed") + ": web-3: app/shell failed",
		"resolved-warn " + incidentKeyFor(d, "app/shell failed") + ": web-3: app/shell is ok again",
		"sos " + incidentKeyFor(d, "app/shell failed") + ": web-3: app/shell still failing, now SOS",
		"resolved-sos " + incidentKeyFor(d, "app/shell failed") + ": web-3: app/shell is ok again",
	}
	if got := mmhotlineSummary(t, d); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	var open int
	d.store.DB.QueryRow("SELECT COUNT(*) FROM fleet_incident_set WHERE state=?", store.FleetIncidentOpen).Scan(&open)
	if open != 0 {
		t.Fatal("rows left open", open)
	}
}

// A raise that maintenance held after a reload goes out on the first
// tick after maintenance ends, for a cannot-check incident too.
func TestHeldRaiseAfterReloadGoesOutAfterMaintenance(t *testing.T) {
	t.Parallel()
	d, p, c := severityDaemon(t, map[string]checksandalerts.Destination{"chat": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://chat.example/"}}})
	now := time.Now().UTC()
	observeApp(t, d, func(st *incidents.CheckState) []checksandalerts.Event {
		return st.Observe(checksandalerts.Observation{Status: "unknown", Message: "cannot run", At: now}, checksandalerts.Timing{}, false)
	})
	setMaintenance(t, d, "app", "10m")
	group := c.CheckGroups["app"]
	group.Check.AlertSeverity = "sos"
	c.CheckGroups["app"] = group
	writeConfig(t, p, c)
	if _, e := d.reload(); e != nil {
		t.Fatal(e)
	}
	raises := func() []string {
		out := []string{}
		for _, e := range queuedEvents(t, d) {
			if e.Type == checksandalerts.CannotCheckIncidentEvents.Raised {
				out = append(out, e.AlertSeverity)
			}
		}
		return out
	}
	// The scheduler tick's raise step, without running the check, whose
	// next observation would resolve the cannot-check incident.
	raiseStep := func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if e := d.record(d.raiseOpenIncidents(time.Now().UTC())); e != nil {
			t.Fatal(e)
		}
	}
	raiseStep()
	if got := raises(); len(got) != 0 {
		t.Fatal("raised during maintenance", got)
	}
	d.mu.Lock()
	err := d.record(d.snapshot.EndMaintenance([]string{"app"}, time.Now().UTC(), incidents.MaintenanceExpiredMessage))
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	raiseStep()
	if got := raises(); !reflect.DeepEqual(got, []string{"sos"}) {
		t.Fatal(got)
	}
}

// A raised incident keeps its severity, the severities it went out with,
// and where they went across a daemon restart.
func TestRaisedIncidentSurvivesRestart(t *testing.T) {
	t.Parallel()
	d, p, _ := severityDaemon(t, map[string]checksandalerts.Destination{
		"chat":  {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://chat.example/"}},
		"pager": {RemindEvery: "never", Inputs: &checksandalerts.Webhook{URL: "https://pager.example/"}, AlertSeverities: []string{"sos"}},
	})
	now := time.Now().UTC()
	observeApp(t, d, func(st *incidents.CheckState) []checksandalerts.Event {
		events := st.Observe(failApp(now), checksandalerts.Timing{}, false)
		return append(events, st.Raise(&st.FailureIncident, checksandalerts.FailureIncidentEvents, "sos", "test", now, false)...)
	})
	d.Close()
	restarted, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
	st := restarted.snapshot.Checks["app/shell"]
	if st.FailureIncident.AlertSeverity != "sos" || !reflect.DeepEqual(st.FailureIncident.AnnouncedAlertSeverities, []string{"warn", "sos"}) {
		t.Fatal(st.FailureIncident)
	}
	if events := st.RaiseToPolicy(now, false); len(events) != 0 {
		t.Fatal("raised again", events)
	}
	observeApp(t, restarted, func(st *incidents.CheckState) []checksandalerts.Event {
		return st.Observe(okApp(now.Add(time.Minute)), checksandalerts.Timing{}, false)
	})
	want := map[string][]string{
		"chat":  {"failed warn", "failure_alert_severity_raised sos", "recovered sos"},
		"pager": {"failure_alert_severity_raised sos", "recovered sos"},
	}
	if got := queuedByDestination(t, restarted); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
}

// Reports are untrusted: their incidents' severities are checked.
func TestReportSeveritiesAreValidated(t *testing.T) {
	t.Parallel()
	report := func(t *incidents.Track) fleet.NodeStatusReport {
		return fleet.NodeStatusReport{StartID: incidents.ID(), History: incidents.ID(), MeasurementHistoryID: incidents.ID(), Seq: 1, After: -1, ReadingsAfter: -1, AlertSeverityWhenThisNodeGoesSilent: "warn",
			Checks: map[string]fleet.ReportedCheck{"a/b": {State: "failed", CannotCheckIncident: t}}}
	}
	id := incidents.ID()
	for _, bad := range []*incidents.Track{
		{Incident: id, Announced: id, AlertSeverity: "bogus", AnnouncedAlertSeverities: []string{"warn"}},
		{Incident: id, Announced: id, AlertSeverity: "warn", AnnouncedAlertSeverities: []string{"bogus"}},
		{Incident: id, Announced: id, AlertSeverity: "sos", AnnouncedAlertSeverities: []string{"sos", "sos"}},
		{Incident: id, AlertSeverity: ""},
	} {
		r := report(bad)
		if r.Validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	r := report(&incidents.Track{Incident: id, Announced: id, AlertSeverity: "sos", AnnouncedAlertSeverities: []string{"warn", "sos"}})
	if e := r.Validate(); e != nil {
		t.Fatal(e)
	}
}

// Without a record of where an incident's alerts went, such as for a
// failure a joined node delivered directly, a recovery goes to the
// destinations that take one of the severities the incident went out with.
func TestRecoveryWithoutRecordFollowsSeverities(t *testing.T) {
	t.Parallel()
	d, _, _ := severityDaemon(t, nil)
	webhook := func(severities ...string) checksandalerts.Destination {
		return checksandalerts.Destination{RemindEvery: "never", Inputs: &checksandalerts.Webhook{URL: "https://example.com/"}, AlertSeverities: severities}
	}
	recovered := checkAlert("recovered", incidents.ID(), "app", "shell", time.Now())
	recovered.AlertSeverity, recovered.AlertSeveritiesSentForIncident = "sos", []string{"warn", "sos"}
	tx, e := d.store.DB.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	destinations := map[string]checksandalerts.Destination{"warn_only": webhook("warn"), "info_only": webhook("info"), "every": webhook(testhelpers.AllSeverities...)}
	if e = store.QueueDirect(tx, time.Now().UnixNano(), recovered, []checksandalerts.Event{recovered}, destinations, store.QueuedBy{Role: store.StandaloneSender, Node: "web-3"}); e != nil {
		t.Fatal(e)
	}
	names := []string{}
	rows, _ := tx.Query("SELECT destination FROM outbox ORDER BY destination")
	for rows.Next() {
		var name string
		rows.Scan(&name)
		names = append(names, name)
	}
	rows.Close()
	if !reflect.DeepEqual(names, []string{"every", "warn_only"}) {
		t.Fatal(names)
	}
}

// A join must carry the node's alert_severity_when_this_node_goes_silent,
// and the hub's check takes it before the node's first report.
func TestJoinRequiresSilenceSeverity(t *testing.T) {
	t.Parallel()
	hub := newHubDaemon(t, nil)
	for _, severity := range []string{"", "loud"} {
		if _, status, err := hub.hub.Join("app-1", hub.hub.Code.Secret, nodeSettings(severity), "10.0.0.5", time.Now()); status != 400 || err == nil {
			t.Fatal(severity, status, err)
		}
	}
	now := time.Now().UTC()
	if _, status, err := hub.hub.Join("app-1", hub.hub.Code.Secret, nodeSettings("info"), "10.0.0.5", now); status != 200 || err != nil {
		t.Fatal(status, err)
	}
	hub.joined("app-1", "192.0.2.1", now, nodeSettings("info"))
	if st := fleetCheck(hub, "app-1"); st == nil || st.AlertSeveritySettings.AlertSeverity != "info" {
		t.Fatal(st)
	}
}

// A check that only records never opens an incident, also when a read-only
// filesystem would fail a check with an alert_severity, so a joined node's
// reports never carry an incident without a severity.
func TestRecordingOnlyNeverOpensIncidents(t *testing.T) {
	t.Parallel()
	st := &incidents.CheckState{CheckGroup: "data", Check: "disk", RecordsOnly: true}
	timing := checksandalerts.Timing{AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}
	now := time.Now().UTC()
	for _, o := range []checksandalerts.Observation{
		checksandalerts.FilesystemObservation(checksandalerts.Check{Inputs: new(checksandalerts.NodeFilesystem("/ro"))}, "/ro", primitives.FilesystemCounts{Blocks: 100, BlocksAvailable: 50, BlockBytes: 4096, ReadOnly: true}),
		{Status: "failed", Message: "failed"},
		{Status: "unknown", Message: "cannot read"},
	} {
		o.At = now
		if events := st.Observe(o, timing, false); len(events) != 0 || st.FailureIncident.Incident != "" || st.CannotCheckIncident.Incident != "" {
			t.Fatal(o, events, st)
		}
	}
	if !strings.HasPrefix(st.Last.Message, "cannot read") || !st.UnknownSince.Equal(now) {
		t.Fatal(st.Last, st.UnknownSince)
	}
}

// No incident ever opens without an alert severity: if a path forgot it,
// the incident still opens, as sos, and the hub accepts it.
func TestIncidentNeverOpensWithoutSeverity(t *testing.T) {
	t.Parallel()
	st := &incidents.CheckState{CheckGroup: "app", Check: "shell"}
	events := st.Observe(checksandalerts.Observation{Status: "failed", Message: "exit 1", At: time.Now()}, checksandalerts.Timing{}, false)
	if len(events) != 1 || events[0].AlertSeverity != "sos" || st.FailureIncident.AlertSeverity != "sos" || !fleet.ValidSeverities(events[0]) {
		t.Fatal(events, st.FailureIncident)
	}
}

// A hub-outage alerter node holds node_reports/to_hub only once it holds the hub's
// settings, whose silence severity the check takes.
func TestOutageAlerterHoldsHubCheckOnlyWithTheFleetSettings(t *testing.T) {
	t.Parallel()
	_, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	node.mu.Lock()
	defer node.mu.Unlock()
	node.watching, node.fleetSettings = true, nil
	if _, ok := node.definitions(node.config)[fleet.FleetKey(checksandalerts.HubCheck)]; ok {
		t.Fatal("the hub check exists without the fleet settings")
	}
	node.fleetSettings = &fleet.FleetSettings{AlertSeverityWhenTheHubGoesSilent: "sos"}
	if def, ok := node.definitions(node.config)[fleet.FleetKey(checksandalerts.HubCheck)]; !ok || def.Severity.AlertSeverity != "sos" {
		t.Fatal(def)
	}
}

// Recipients survive retention for as long as the incident is open.
func TestIncidentDestinationsRemainUntilTheIncidentCloses(t *testing.T) {
	t.Parallel()
	d, _, _ := severityDaemon(t, nil)
	webhook := func(severities ...string) checksandalerts.Destination {
		return checksandalerts.Destination{RemindEvery: "never", Inputs: &checksandalerts.Webhook{URL: "https://example.com/"}, AlertSeverities: severities}
	}
	destinations := map[string]checksandalerts.Destination{"email_warn": webhook("warn"), "phone_sos": webhook("sos")}
	incident := incidents.ID()
	t0 := time.Now().Add(-30 * 24 * time.Hour)
	failed := checkAlert("failed", incident, "app", "shell", t0)
	raised := checkAlert(checksandalerts.FailureIncidentEvents.Raised, incident, "app", "shell", t0.Add(time.Hour))
	raised.AlertSeverity = "sos"
	recovered := checkAlert("recovered", incident, "app", "shell", time.Now())
	recovered.AlertSeverity, recovered.AlertSeveritiesSentForIncident = "sos", []string{"warn", "sos"}
	tx, e := d.store.DB.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	from := store.QueuedBy{Role: store.StandaloneSender, Node: "web-3"}
	for _, step := range []struct {
		at    time.Time
		event checksandalerts.Event
	}{{t0, failed}, {t0.Add(time.Hour), raised}} {
		if e = store.QueueDirect(tx, step.at.UnixNano(), step.event, []checksandalerts.Event{step.event}, destinations, from); e != nil {
			t.Fatal(e)
		}
	}
	count := func() int {
		var n int
		tx.QueryRow("SELECT COUNT(*) FROM incident_alert_destinations").Scan(&n)
		return n
	}
	before := count()
	if e = d.store.PruneInTransaction(tx, time.Now()); e != nil {
		t.Fatal(e)
	}
	if count() != before {
		t.Fatal("pruned part of a recent incident", before, count())
	}
	if e = d.store.PruneInTransaction(tx, time.Now().Add(365*24*time.Hour)); e != nil {
		t.Fatal(e)
	}
	if count() != before {
		t.Fatal("forgot recipients of an open incident", before, count())
	}
	tx.Exec("DELETE FROM outbox")
	if e = store.QueueDirect(tx, time.Now().UnixNano(), recovered, []checksandalerts.Event{recovered}, destinations, from); e != nil {
		t.Fatal(e)
	}
	names := []string{}
	rows, _ := tx.Query("SELECT destination FROM outbox ORDER BY destination")
	for rows.Next() {
		var name string
		rows.Scan(&name)
		names = append(names, name)
	}
	rows.Close()
	if !reflect.DeepEqual(names, []string{"email_warn", "phone_sos"}) {
		t.Fatal(names)
	}
	if count() != 0 {
		t.Fatal("kept recipients after closing", count())
	}
}
