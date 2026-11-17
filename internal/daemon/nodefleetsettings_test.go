package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
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

// raise_to_sos_after_this_node_is_silent_for is a duration of at least 1s,
// and this_node_is_in_node_groups is a list of names that follow the rules
// for the name of a check, each stated once. Both are optional on every
// node, and a node may state an empty list.
func TestTheNodeSettingsForTheFleetAreValidated(t *testing.T) {
	t.Parallel()
	checks := "check_groups: {a: {shell: 'true'}}\n"
	for text, want := range map[string]string{
		"raise_to_sos_after_this_node_is_silent_for: 500ms\n":             "raise_to_sos_after_this_node_is_silent_for: expected a duration of at least 1s",
		"raise_to_sos_after_this_node_is_silent_for: ''\n":                "a duration",
		"raise_to_sos_after_this_node_is_silent_for: soon\n":              "a duration",
		"this_node_is_in_node_groups: application_servers\n":              "expected a list",
		"this_node_is_in_node_groups: [application servers]\n":            `this_node_is_in_node_groups: "application servers" is not a valid name of a node group: use letters, digits, hyphens or underscores (1..64 characters)`,
		"this_node_is_in_node_groups: ['']\n":                             `this_node_is_in_node_groups: "" is not a valid name of a node group`,
		"this_node_is_in_node_groups: [frankfurt, database, frankfurt]\n": `this_node_is_in_node_groups: names the node group "frankfurt" twice`,
	} {
		if _, err := config.Load(testhelpers.ConfigFile(t, text+checks)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", text, err, want)
		}
	}
	for _, text := range []string{
		"",
		"this_node_is_in_node_groups: []\n",
		"raise_to_sos_after_this_node_is_silent_for: 10m\nthis_node_is_in_node_groups: [database_servers, frankfurt]\n",
		"join_fleet_hub: {join_code_file: /etc/mmwatchdog/secrets/fleet-hub-join-code}\nraise_to_sos_after_this_node_is_silent_for: 1s\nthis_node_is_in_node_groups: [application_servers]\n",
		"fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'hub.test:8445'}\nraise_to_sos_after_this_node_is_silent_for: 1h\nthis_node_is_in_node_groups: [operations]\n",
	} {
		c, err := config.Load(testhelpers.ConfigFile(t, text+checks))
		if err != nil {
			t.Errorf("%q: %v", text, err)
			continue
		}
		if got := c.NodeFleetSettings(); got.Problem() != "" || got.AlertSeverityWhenThisNodeGoesSilent != "sos" || strings.Contains(text, "10m") && !reflect.DeepEqual(got, config.NodeFleetSettings{AlertSeverityWhenThisNodeGoesSilent: "sos", RaiseToSosAfterThisNodeIsSilentFor: "10m", ThisNodeIsInNodeGroups: []string{"database_servers", "frankfurt"}}) {
			t.Errorf("%q: %+v", text, got)
		}
	}
	// What another machine sends is checked in the same way.
	for settings, want := range map[*config.NodeFleetSettings]string{
		{}: "needs alert_severity_when_this_node_goes_silent: info, warn, or sos",
		{AlertSeverityWhenThisNodeGoesSilent: "loud", RaiseToSosAfterThisNodeIsSilentFor: "", ThisNodeIsInNodeGroups: nil}:                   "needs alert_severity_when_this_node_goes_silent: info, warn, or sos",
		{AlertSeverityWhenThisNodeGoesSilent: "warn", RaiseToSosAfterThisNodeIsSilentFor: "0s", ThisNodeIsInNodeGroups: nil}:                 "has an invalid raise_to_sos_after_this_node_is_silent_for: expected a duration of at least 1s",
		{AlertSeverityWhenThisNodeGoesSilent: "warn", RaiseToSosAfterThisNodeIsSilentFor: "ten minutes", ThisNodeIsInNodeGroups: nil}:        "has an invalid raise_to_sos_after_this_node_is_silent_for: expected a duration of at least 1s",
		{AlertSeverityWhenThisNodeGoesSilent: "warn", RaiseToSosAfterThisNodeIsSilentFor: "", ThisNodeIsInNodeGroups: []string{"a b"}}:       `has an invalid this_node_is_in_node_groups: "a b" is not a valid name of a node group`,
		{AlertSeverityWhenThisNodeGoesSilent: "warn", RaiseToSosAfterThisNodeIsSilentFor: "", ThisNodeIsInNodeGroups: []string{"a", "a"}}:    `has an invalid this_node_is_in_node_groups: names the node group "a" twice`,
		{AlertSeverityWhenThisNodeGoesSilent: "warn", RaiseToSosAfterThisNodeIsSilentFor: "10m", ThisNodeIsInNodeGroups: []string{"a", "b"}}: "",
		{AlertSeverityWhenThisNodeGoesSilent: "sos", RaiseToSosAfterThisNodeIsSilentFor: "", ThisNodeIsInNodeGroups: nil}:                    "",
	} {
		if got := settings.Problem(); want == "" && got != "" || want != "" && !strings.Contains(got, want) {
			t.Errorf("%+v: got %q, want %q", *settings, got, want)
		}
	}
}

// A node states raise_to_sos_after_this_node_is_silent_for beside the
// severity of its silence alert, and sends both when it joins and in every
// report. When the node has been silent for that long, counted from the
// last report, the hub raises the alert to sos, and
// the alert stays sos until the node reports again. A node without the
// setting is never raised by time.
func TestASilentNodeIsRaisedToSosByTime(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node, other := joinedNode(t, code, "app-1"), joinedNode(t, code, "app-2")
	node.mu.Lock()
	node.config.AlertSeverityWhenThisNodeGoesSilent, node.config.RaiseToSosAfterThisNodeIsSilentFor = "warn", "10m"
	node.mu.Unlock()
	other.mu.Lock()
	other.config.AlertSeverityWhenThisNodeGoesSilent = "warn"
	other.mu.Unlock()
	node.report(context.Background())
	other.report(context.Background())
	// The join carries the setting, so the hub knows it before the first
	// report, and the node's check on the hub has it.
	hub.hub.Mu.Lock()
	joined := hub.hub.Nodes["app-1"].NodeFleetSettings
	hub.hub.Mu.Unlock()
	warnThenSos := checksandalerts.AlertSeveritySettings{AlertSeverity: "warn", RaiseToSosAfterFailingFor: "10m"}
	if got := fleetCheck(hub, "app-1").AlertSeveritySettings; got != warnThenSos || joined.RaiseToSosAfterThisNodeIsSilentFor != "10m" {
		t.Fatal(got, joined)
	}
	if got := fleetCheck(hub, "app-2").AlertSeveritySettings; got != (checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}) {
		t.Fatal(got)
	}
	// Both nodes go silent at the same moment.
	opened := time.Now().UTC().Add(3 * reportInterval)
	hub.mu.Lock()
	for _, name := range []string{"app-1", "app-2"} {
		hub.snapshot.Checks[fleet.FleetKey(name)].Deadline = opened
	}
	hub.mu.Unlock()
	summary := func() []string {
		got := []string{}
		for _, e := range intakeEvents(t, hub) {
			got = append(got, e.Type+" "+e.Check+" "+e.AlertSeverity)
		}
		return got
	}
	tick(hub, opened)
	lastReport := fleetCheck(hub, "app-1").Receipt
	tick(hub, lastReport.Add(10*time.Minute-time.Second))
	want := []string{"failed app-1 warn", "failed app-2 warn"}
	if got := summary(); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	// Ten minutes after the last report, only the node with the setting
	// is raised, once.
	tick(hub, lastReport.Add(10*time.Minute))
	tick(hub, opened.Add(time.Hour))
	want = append(want, "failure_alert_severity_raised app-1 sos")
	events := intakeEvents(t, hub)
	if got := summary(); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	raised := events[2]
	if raised.Message != "app-1 has sent no report to the hub for 10m, so this alert is now SOS (raise_to_sos_after_this_node_is_silent_for: 10m)" ||
		raised.Title != "app-1 still not reporting to the MMWatchdog fleet hub, now SOS" || raised.Incident != events[0].Incident {
		t.Fatalf("%+v", raised)
	}
	if st := fleetCheck(hub, "app-2"); st.FailureIncident.AlertSeverity != "warn" {
		t.Fatal(st.FailureIncident)
	}
	// A restarted hub learns the setting from the node's saved report.
	hub.mu.Lock()
	hub.snapshot.Checks[fleet.FleetKey("app-1")].AlertSeveritySettings = checksandalerts.AlertSeveritySettings{AlertSeverity: "info"}
	hub.startFleet(hub.hub.Members(), opened.Add(time.Hour))
	restored := hub.snapshot.Checks[fleet.FleetKey("app-1")].AlertSeveritySettings
	hub.snapshot.Checks[fleet.FleetKey("app-1")].Deadline = opened
	hub.mu.Unlock()
	if restored != warnThenSos {
		t.Fatal(restored)
	}
	// The node reports again, now without the setting: the recovery has the
	// highest severity that the incident reached, and the hub forgets the
	// setting.
	hub.receipt("app-1", time.Time{}, nodeSettings("warn"), opened.Add(2*time.Hour))
	tick(hub, opened.Add(2*time.Hour))
	events = intakeEvents(t, hub)
	last := events[len(events)-1]
	if last.Type != "recovered" || last.Check != "app-1" || last.AlertSeverity != "sos" || !reflect.DeepEqual(last.AlertSeveritiesSentForIncident, []string{"warn", "sos"}) {
		t.Fatalf("%+v", last)
	}
	if got := fleetCheck(hub, "app-1").AlertSeveritySettings; got != (checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}) {
		t.Fatal(got)
	}
}

// The hub reminds of a silent node only the destinations that get the
// node's silence alerts: a pager that takes only sos gets no reminder while
// the node is silent at warn, and after the raise to sos its first reminder
// comes one remind_every after the raise.
func TestTheHubRemindsOfASilentNodeOnlyTheDestinationsThatGetItsAlerts(t *testing.T) {
	t.Parallel()
	hub, code, _ := startHubWith(t, map[string]checksandalerts.Destination{
		"team_chat":                          {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://example.com/team"}},
		"on_call_pager_reminding_every_hour": {AlertSeverities: []string{"sos"}, Inputs: &checksandalerts.Webhook{URL: "https://example.com/pager"}, RemindEvery: "1h"},
	})
	node := joinedNode(t, code, "app-1")
	node.mu.Lock()
	node.config.AlertSeverityWhenThisNodeGoesSilent, node.config.RaiseToSosAfterThisNodeIsSilentFor = "warn", "2h"
	node.mu.Unlock()
	node.report(context.Background())
	opened := time.Now().UTC().Add(3 * reportInterval)
	hub.mu.Lock()
	hub.snapshot.Checks[fleet.FleetKey("app-1")].Deadline = opened
	hub.mu.Unlock()
	for _, at := range []time.Duration{0, time.Hour, 2 * time.Hour, 3 * time.Hour} {
		tick(hub, opened.Add(at))
	}
	history, err := hub.store.CheckGroupEvents(checksandalerts.NodeReportsCheckGroup, 100)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, e := range slices.Backward(history) {
		got = append(got, strings.TrimSpace(e.Type+" "+e.AlertSeverity+" "+e.Destination))
	}
	want := []string{"failed warn", "failure_alert_severity_raised sos", "reminder sos on_call_pager_reminding_every_hour"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the hub's event history of the silent node\n%q\nwant\n%q", got, want)
	}
}

// While a node's reported whole-node window lasts, the hub sends no silence
// alert for the node and raises none. A node that is still silent when the
// window ends alerts then, at sos when it has been silent for its
// raise_to_sos_after_this_node_is_silent_for.
func TestMaintenanceHoldsTheRaiseOfASilentNode(t *testing.T) {
	t.Parallel()
	hub := newHubDaemon(t, nil)
	now := time.Now().UTC()
	settings := config.NodeFleetSettings{AlertSeverityWhenThisNodeGoesSilent: "warn", RaiseToSosAfterThisNodeIsSilentFor: "10m"}
	hub.joined("app-1", now, settings)
	// The join carried the setting: the hub has it before the first report.
	if got := fleetCheck(hub, "app-1").AlertSeveritySettings; got != settings.Silence() || got.RaiseToSosAfterFailingFor != "10m" {
		t.Fatal(got)
	}
	// The node's last report shows a whole-node window of one hour.
	hub.receipt("app-1", now.Add(time.Hour), settings, now)
	opened := now.Add(3 * reportInterval)
	tick(hub, opened)
	tick(hub, opened.Add(30*time.Minute))
	if st := fleetCheck(hub, "app-1"); st.FailureIncident.Incident == "" || st.FailureIncident.Announced != "" || len(intakeEvents(t, hub)) != 0 {
		t.Fatalf("%+v %v", st.FailureIncident, eventSummary(intakeEvents(t, hub)))
	}
	tick(hub, now.Add(time.Hour))
	if got := eventSummary(intakeEvents(t, hub)); !reflect.DeepEqual(got, []string{"failed sos"}) {
		t.Fatal(got)
	}
}

// The hub's own raise_to_sos_after_this_node_is_silent_for reaches the hub
// hub-outage alerter node with the fleet settings and raises the hub-outage alerter's alert about
// the hub in the same way.
func TestTheHubOutageAlerterNodeRaisesItsAlertByTime(t *testing.T) {
	t.Parallel()
	hub, code, _ := startHubWith(t, outageAlerterDestinations())
	hub.mu.Lock()
	withRaise := *hub.config
	withRaise.RaiseToSosAfterThisNodeIsSilentFor = "10m"
	served, err := fleet.NewFleetSettings(&withRaise)
	without := hub.hub.FleetSettings.Load()
	hub.mu.Unlock()
	if err != nil || served.RaiseToSosAfterTheHubIsSilentFor != "10m" || served.Validate(hub.config.Modules) != nil || without.RaiseToSosAfterTheHubIsSilentFor != "" || served.Version == without.Version {
		t.Fatal(err, served, without)
	}
	for _, invalid := range []checksandalerts.Duration{"0s", "soon"} {
		bad := *served
		bad.RaiseToSosAfterTheHubIsSilentFor = invalid
		bad.Version = bad.ComputeVersion()
		if err := bad.Validate(hub.config.Modules); err == nil || !strings.Contains(err.Error(), "raise_to_sos_after_the_hub_is_silent_for") {
			t.Fatal(invalid, err)
		}
	}
	node := joinedNode(t, code, "app-1")
	now := time.Now().UTC()
	node.mu.Lock()
	node.watching = true
	if err := node.applyFleetSettings(served); err != nil {
		node.mu.Unlock()
		t.Fatal(err)
	}
	st := node.snapshot.Checks[fleet.FleetKey(checksandalerts.HubCheck)]
	if st == nil || st.AlertSeveritySettings != (checksandalerts.AlertSeveritySettings{AlertSeverity: "sos", RaiseToSosAfterFailingFor: "10m"}) {
		node.mu.Unlock()
		t.Fatalf("%+v", st)
	}
	// The hub of this test is sos; a hub-outage alerter's alert that starts lower is
	// raised.
	st.AlertSeveritySettings.AlertSeverity = "warn"
	st.Receipt, st.Deadline = now, now.Add(hubWatchAfter)
	node.mu.Unlock()
	tick(node, now.Add(hubWatchAfter))
	tick(node, now.Add(10*time.Minute-time.Second))
	if got := eventSummary(queuedEvents(t, node)); !reflect.DeepEqual(got, []string{"failed warn"}) {
		t.Fatal(got)
	}
	tick(node, now.Add(10*time.Minute))
	events := queuedEvents(t, node)
	if got := eventSummary(events); !reflect.DeepEqual(got, []string{"failed warn", "failure_alert_severity_raised sos"}) {
		t.Fatal(got)
	}
	if raised := events[1]; raised.Message != "no report from this node has reached the MMWatchdog fleet hub for 10m, so this alert is now SOS (the hub's raise_to_sos_after_this_node_is_silent_for: 10m)" ||
		raised.Title != "app-1 still cannot reach the MMWatchdog fleet hub, now SOS" {
		t.Fatalf("%+v", raised)
	}
}

// A node says which node groups it is in, when it joins and in every report.
// The hub keeps no list of node groups: the fleet view shows each node's
// node groups from its latest saved report, also while the node is silent,
// and the view of one node group shows the nodes that name it, with the
// worst exit code among them. A name that no node states is an error that
// lists the node groups of the fleet.
func TestNodeGroupsInTheFleetView(t *testing.T) {
	t.Parallel()
	hub := newHubDaemon(t, nil)
	h := hub.hub
	now := time.Now().UTC()
	groupsOf := func(name string) []string {
		t.Helper()
		for _, node := range h.Fleet(now).Nodes {
			if node.Name == name {
				return node.NodeGroups
			}
		}
		t.Fatalf("the fleet has no node %q", name)
		return nil
	}
	// No node states a node group yet, and the list is never null in JSON.
	if _, err := h.FleetOfNodeGroup(now, "frankfurt"); err == nil || !strings.Contains(err.Error(), `no node is in node group "frankfurt", because no node in the fleet states a node group`) {
		t.Fatal(err)
	}
	if got := groupsOf(hub.store.Node); got == nil || len(got) != 0 {
		t.Fatalf("%#v", got)
	}
	// The join carries the node groups, so the view has them before the
	// first report.
	join := func(name string, nodeGroups ...string) {
		t.Helper()
		if _, status, err := h.Join(name, h.Code.Secret, config.NodeFleetSettings{AlertSeverityWhenThisNodeGoesSilent: "warn", ThisNodeIsInNodeGroups: nodeGroups}, "10.0.0.5", now); status != 200 || err != nil {
			t.Fatal(status, err)
		}
	}
	report := func(name string, exitCode int, nodeGroups ...string) {
		t.Helper()
		h.Mu.Lock()
		seq := h.Nodes[name].Seq + 1
		h.Mu.Unlock()
		r := &fleet.NodeStatusReport{AlertSeverityWhenThisNodeGoesSilent: "warn", ThisNodeIsInNodeGroups: nodeGroups, StartID: strings.Repeat("a", 32), Seq: seq, History: strings.Repeat("b", 32), After: -1, MeasurementHistoryID: strings.Repeat("c", 32), ReadingsAfter: -1, Summary: incidents.StatusSummary{ExitCode: exitCode, Attention: []incidents.Attention{}}}
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
		if _, status, err := h.Accept(name, r, now); status != 200 || err != nil {
			t.Fatal(status, err)
		}
	}
	join("app-1", "application_servers", "frankfurt")
	join("app-2", "application_servers")
	join("db-1", "database_servers", "frankfurt")
	join("old-1")
	if got := groupsOf("app-1"); !reflect.DeepEqual(got, []string{"application_servers", "frankfurt"}) {
		t.Fatal(got)
	}
	if _, status, err := h.Join("app-9", h.Code.Secret, config.NodeFleetSettings{AlertSeverityWhenThisNodeGoesSilent: "warn", ThisNodeIsInNodeGroups: []string{"not a name"}}, "10.0.0.5", now); status != 400 || err == nil || !strings.Contains(err.Error(), "join has an invalid this_node_is_in_node_groups") {
		t.Fatal(status, err)
	}
	report("app-1", 0, "application_servers", "frankfurt")
	report("app-2", 1, "application_servers")
	report("db-1", 3, "database_servers", "frankfurt")
	report("old-1", 0)
	names := func(f fleet.Fleet) []string {
		out := []string{}
		for _, node := range f.Nodes {
			out = append(out, node.Name)
		}
		return out
	}
	for _, tc := range []struct {
		nodeGroup string
		nodes     []string
		exitCode  int
	}{
		{"application_servers", []string{"app-1", "app-2"}, 1},
		{"database_servers", []string{"db-1"}, 3},
		{"frankfurt", []string{"app-1", "db-1"}, 3},
	} {
		f, err := h.FleetOfNodeGroup(now, tc.nodeGroup)
		if err != nil || f.NodeGroup != tc.nodeGroup || !reflect.DeepEqual(names(f), tc.nodes) || f.ExitCode != tc.exitCode {
			t.Fatalf("%s: %v %d %v", tc.nodeGroup, names(f), f.ExitCode, err)
		}
	}
	if f := h.Fleet(now); f.NodeGroup != "" || len(f.Nodes) != 5 || f.ExitCode != 1 {
		t.Fatalf("%v %d", names(f), f.ExitCode)
	}
	if _, err := h.FleetOfNodeGroup(now, "berlin"); err == nil || err.Error() != `no node is in a node group named "berlin"; the node groups of this fleet are application_servers, database_servers, frankfurt` {
		t.Fatal(err)
	}
	// A report with a name that is no name of a node group is refused.
	bad := &fleet.NodeStatusReport{AlertSeverityWhenThisNodeGoesSilent: "warn", ThisNodeIsInNodeGroups: []string{"a", "a"}, StartID: strings.Repeat("a", 32), Seq: 9, History: strings.Repeat("b", 32), After: -1, MeasurementHistoryID: strings.Repeat("c", 32), ReadingsAfter: -1}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), `report has an invalid this_node_is_in_node_groups: names the node group "a" twice`) {
		t.Fatal(err)
	}
	// db-1 leaves frankfurt and joins berlin. A silent node keeps its node
	// groups, and a node group that no node names any more is gone.
	report("db-1", 0, "database_servers", "berlin")
	silent := now.Add(4 * reportInterval)
	f, err := h.FleetOfNodeGroup(silent, "berlin")
	if err != nil || !reflect.DeepEqual(names(f), []string{"db-1"}) || f.Nodes[0].State != "silent" || f.ExitCode != 1 {
		t.Fatalf("%+v %v", f, err)
	}
	report("app-1", 0, "application_servers")
	if _, err = h.FleetOfNodeGroup(now, "frankfurt"); err == nil || !strings.Contains(err.Error(), "the node groups of this fleet are application_servers, berlin, database_servers") {
		t.Fatal(err)
	}
	// The command line asks the daemon for one node group.
	status, body := fleetCommand(t, hub, "GET", "/fleet?node_group=application_servers")
	if status != 200 || !strings.Contains(body, `"node_group":"application_servers"`) || !strings.Contains(body, `"this_node_is_in_node_groups":["application_servers"]`) || strings.Contains(body, `"db-1"`) {
		t.Fatal(status, body)
	}
	if status, body = fleetCommand(t, hub, "GET", "/fleet?node_group=frankfurt"); status != 404 || !strings.Contains(body, "the node groups of this fleet are") {
		t.Fatal(status, body)
	}
	if status, body = fleetCommand(t, hub, "GET", "/fleet?node=db-1"); status != 200 || !strings.Contains(body, `"this_node_is_in_node_groups":["database_servers","berlin"]`) {
		t.Fatal(status, body)
	}
}

// A node's reports carry its node groups, and the hub is in node groups in
// the same way as every other node.
func TestANodeReportsItsNodeGroups(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	node.mu.Lock()
	node.config.ThisNodeIsInNodeGroups = []string{"application_servers", "frankfurt"}
	node.mu.Unlock()
	hub.mu.Lock()
	hub.config.ThisNodeIsInNodeGroups = []string{"operations"}
	hub.mu.Unlock()
	node.report(context.Background())
	hub.report(context.Background())
	if got := fleetServer(t, hub, "app-1").NodeGroups; !reflect.DeepEqual(got, []string{"application_servers", "frankfurt"}) {
		t.Fatal(got)
	}
	if got := fleetServer(t, hub, hub.store.Node).NodeGroups; !reflect.DeepEqual(got, []string{"operations"}) {
		t.Fatal(got)
	}
	f, err := hub.hub.FleetOfNodeGroup(time.Now(), "operations")
	if err != nil || len(f.Nodes) != 1 || !f.Nodes[0].Hub {
		t.Fatalf("%+v %v", f, err)
	}
	// The node no longer states the setting: its next report says so.
	node.mu.Lock()
	node.config.ThisNodeIsInNodeGroups = nil
	node.mu.Unlock()
	node.report(context.Background())
	if got := fleetServer(t, hub, "app-1").NodeGroups; len(got) != 0 {
		t.Fatal(got)
	}
}

// silenceSummary lists the hub's alerts about its nodes as "type node
// severity".
func silenceSummary(t *testing.T, hub *Daemon) []string {
	t.Helper()
	got := []string{}
	for _, e := range intakeEvents(t, hub) {
		if e.CheckGroup == checksandalerts.NodeReportsCheckGroup && e.Check != "" {
			got = append(got, e.Type+" "+e.Check+" "+e.AlertSeverity)
		}
	}
	return got
}

// A daemon start gives every fleet check a fresh deadline. A node whose
// silence incident was open before the hub restarted is not raised before
// that deadline, however long the hub was down: the hub does not know yet
// whether the node is still silent. A node that reports again recovers at
// the severity that its alert had, and a node that is still silent at the
// deadline is raised then.
func TestARestartedHubRaisesNoSilentNodeBeforeItsFreshDeadline(t *testing.T) {
	t.Parallel()
	settings := config.NodeFleetSettings{AlertSeverityWhenThisNodeGoesSilent: "warn", RaiseToSosAfterThisNodeIsSilentFor: "10m"}
	for _, stillSilent := range []bool{false, true} {
		clock := testhelpers.NewClock()
		hub, code, _ := startHubOnClock(t, nil, nil, clock.Now)
		node := joinedNode(t, code, "app-1")
		node.mu.Lock()
		node.config.AlertSeverityWhenThisNodeGoesSilent, node.config.RaiseToSosAfterThisNodeIsSilentFor = "warn", "10m"
		node.mu.Unlock()
		node.report(context.Background())
		// The silence incident opened 11 minutes ago, and the hub stopped
		// before the raise was due.
		clock.Pass(3 * reportInterval)
		opened := clock.Now()
		hub.mu.Lock()
		hub.snapshot.Checks[fleet.FleetKey("app-1")].Deadline = opened
		hub.mu.Unlock()
		tick(hub, opened)
		if err := hub.Close(); err != nil {
			t.Fatal(err)
		}
		clock.Pass(11 * time.Minute)
		restarted, err := newDaemon(hub.path, clock.Now)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { restarted.Close() })
		now := clock.Now()
		tick(restarted, now)
		st := fleetCheck(restarted, "app-1")
		if !st.Deadline.After(now) || st.AlertSeveritySettings != settings.Silence() || st.FailureIncident.AlertSeverity != "warn" {
			t.Fatalf("%+v", st)
		}
		if got := silenceSummary(t, restarted); !reflect.DeepEqual(got, []string{"failed app-1 warn"}) {
			t.Fatal("the restarted hub raised a node before its fresh deadline", got)
		}
		want := []string{"failed app-1 warn", "recovered app-1 warn"}
		if stillSilent {
			tick(restarted, st.Deadline.Add(-time.Second))
			tick(restarted, st.Deadline)
			want = []string{"failed app-1 warn", "failure_alert_severity_raised app-1 sos"}
		} else {
			restarted.receipt("app-1", time.Time{}, settings, now.Add(5*time.Second))
			tick(restarted, now.Add(5*time.Second))
		}
		if got := silenceSummary(t, restarted); !reflect.DeepEqual(got, want) {
			t.Fatal(stillSilent, got)
		}
	}
}

// The same holds for the number of nodes that are silent at once: after a
// daemon start, a node counts as silent only when its fresh deadline has
// passed.
func TestARestartedHubCountsNoNodeAsSilentBeforeItsFreshDeadline(t *testing.T) {
	t.Parallel()
	hub := newHubDaemon(t, nil)
	now := time.Now().UTC()
	for _, name := range []string{"app-1", "app-2", "app-3"} {
		hub.joined(name, now, nodeSettings("warn"))
	}
	hub.mu.Lock()
	for _, name := range []string{"app-1", "app-2"} {
		hub.snapshot.Checks[fleet.FleetKey(name)].Deadline = now
	}
	hub.mu.Unlock()
	tick(hub, now)
	want := []string{"failed app-1 warn", "failed app-2 warn"}
	if got := silenceSummary(t, hub); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	// The hub starts again, now with raise_to_sos_when_silent_nodes_reach_count.
	restarted := now.Add(time.Hour)
	hub.mu.Lock()
	hub.config.FleetHubRole.RaiseToSosWhenSilentNodesReachCount = new(2)
	hub.startFleet(nil, restarted)
	deadline := hub.snapshot.Checks[fleet.FleetKey("app-1")].Deadline
	hub.mu.Unlock()
	tick(hub, restarted)
	tick(hub, deadline.Add(-time.Second))
	if got := silenceSummary(t, hub); !deadline.After(restarted) || !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	// app-2 and app-3 report before their deadline: one silent node raises
	// nothing.
	hub.receipt("app-2", time.Time{}, nodeSettings("warn"), deadline.Add(-time.Second))
	hub.receipt("app-3", time.Time{}, nodeSettings("warn"), deadline.Add(-time.Second))
	tick(hub, deadline)
	want = append(want, "recovered app-2 warn")
	if got := silenceSummary(t, hub); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	// app-3 goes silent too: two nodes are silent at once.
	hub.mu.Lock()
	hub.snapshot.Checks[fleet.FleetKey("app-3")].Deadline = deadline.Add(time.Minute)
	hub.mu.Unlock()
	tick(hub, deadline.Add(time.Minute))
	want = append(want, "failed app-3 warn", "failure_alert_severity_raised app-1 sos", "failure_alert_severity_raised app-3 sos")
	if got := silenceSummary(t, hub); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

// After its own restart, the hub-outage alerter node does not raise its alert about
// the hub before one report could be tried: its check has a fresh deadline
// too.
func TestARestartedHubOutageAlerterNodeRaisesNothingBeforeItsFreshDeadline(t *testing.T) {
	t.Parallel()
	hub, code, _ := startHubWith(t, outageAlerterDestinations())
	hub.mu.Lock()
	withRaise := *hub.config
	withRaise.AlertSeverityWhenThisNodeGoesSilent, withRaise.RaiseToSosAfterThisNodeIsSilentFor = "warn", "10m"
	served, err := fleet.NewFleetSettings(&withRaise)
	hub.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	node := joinedNode(t, code, "app-1")
	opened := time.Now().UTC().Add(-11 * time.Minute)
	node.mu.Lock()
	node.watching = true
	b, _ := json.Marshal(served)
	if err = node.applyFleetSettings(served); err == nil {
		err = primitives.WriteFileAtomic(filepath.Join(node.config.StateDir, fleet.FleetSettingsFile), b)
	}
	if err != nil {
		node.mu.Unlock()
		t.Fatal(err)
	}
	node.snapshot.Checks[fleet.FleetKey(checksandalerts.HubCheck)].Deadline = opened
	node.snapshot.Checks[fleet.FleetKey(checksandalerts.HubCheck)].Receipt = opened.Add(-hubWatchAfter)
	node.mu.Unlock()
	tick(node, opened)
	if err = node.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewDaemon(node.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
	now := time.Now().UTC()
	tick(restarted, now)
	st := fleetCheck(restarted, checksandalerts.HubCheck)
	if st == nil || !st.Deadline.After(now) || st.FailureIncident.AlertSeverity != "warn" {
		t.Fatalf("%+v", st)
	}
	if got := eventSummary(queuedEvents(t, restarted)); !reflect.DeepEqual(got, []string{"failed warn"}) {
		t.Fatal("the restarted hub-outage alerter raised its alert before its fresh deadline", got)
	}
	// No report reaches the hub until the deadline: the alert is raised then.
	tick(restarted, st.Deadline)
	if got := eventSummary(queuedEvents(t, restarted)); !reflect.DeepEqual(got, []string{"failed warn", "failure_alert_severity_raised sos"}) {
		t.Fatal(got)
	}
}

// mmwatchdog maintenance on node_reports on the hub holds the raise of a silence
// incident that was announced before the window. The raise goes out when the
// window ends and the node is still silent.
func TestMaintenanceOnNodeReportsHoldsTheRaiseOfAnAnnouncedIncident(t *testing.T) {
	t.Parallel()
	hub := newHubDaemon(t, nil)
	now := time.Now().UTC()
	settings := config.NodeFleetSettings{AlertSeverityWhenThisNodeGoesSilent: "warn", RaiseToSosAfterThisNodeIsSilentFor: "10m"}
	hub.joined("app-1", now, settings)
	hub.receipt("app-1", time.Time{}, settings, now)
	opened := now.Add(3 * reportInterval)
	tick(hub, opened)
	setMaintenance(t, hub, checksandalerts.NodeReportsCheckGroup, "1h")
	tick(hub, opened.Add(10*time.Minute))
	tick(hub, opened.Add(30*time.Minute))
	if got := silenceSummary(t, hub); !reflect.DeepEqual(got, []string{"failed app-1 warn"}) || fleetCheck(hub, "app-1").FailureIncident.AlertSeverity != "warn" {
		t.Fatal(got)
	}
	setMaintenance(t, hub, checksandalerts.NodeReportsCheckGroup, "off")
	tick(hub, opened.Add(31*time.Minute))
	if got := silenceSummary(t, hub); !reflect.DeepEqual(got, []string{"failed app-1 warn", "failure_alert_severity_raised app-1 sos"}) {
		t.Fatal(got)
	}
}

// A node that reaches its raise_to_sos_after_this_node_is_silent_for at the
// tick at which raise_to_sos_when_silent_nodes_reach_count is reached is raised once.
func TestTheRaiseByTimeAndTheRaiseByCountRaiseANodeOnce(t *testing.T) {
	t.Parallel()
	hub := newHubDaemon(t, nil)
	hub.mu.Lock()
	hub.config.FleetHubRole.RaiseToSosWhenSilentNodesReachCount = new(2)
	hub.mu.Unlock()
	now := time.Now().UTC()
	hub.joined("app-1", now, config.NodeFleetSettings{AlertSeverityWhenThisNodeGoesSilent: "warn", RaiseToSosAfterThisNodeIsSilentFor: "10m"})
	hub.joined("app-2", now, nodeSettings("warn"))
	hub.joined("app-3", now, nodeSettings("warn"))
	hub.mu.Lock()
	hub.snapshot.Checks[fleet.FleetKey("app-1")].Deadline = now.Add(time.Minute)
	hub.snapshot.Checks[fleet.FleetKey("app-2")].Deadline = now.Add(11 * time.Minute)
	hub.snapshot.Checks[fleet.FleetKey("app-3")].Deadline = now.Add(time.Hour)
	hub.mu.Unlock()
	tick(hub, now.Add(time.Minute))
	// At 11 minutes app-1 reaches its time, and app-2 goes silent.
	tick(hub, now.Add(11*time.Minute))
	tick(hub, now.Add(12*time.Minute))
	want := []string{"failed app-1 warn", "failed app-2 warn", "failure_alert_severity_raised app-1 sos", "failure_alert_severity_raised app-2 sos"}
	if got := silenceSummary(t, hub); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

// A restarted hub knows each node's settings for the fleet from fleet.db:
// from the node's latest saved report, and for a node that joined and has
// not reported, from its join.
func TestARestartedHubKeepsTheNodeSettingsForTheFleet(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	node.mu.Lock()
	node.config.AlertSeverityWhenThisNodeGoesSilent, node.config.RaiseToSosAfterThisNodeIsSilentFor = "warn", "10m"
	node.config.SendSilenceAlertsOfThisNodeOnlyToDestinations = &[]string{"node_owner"}
	node.config.ThisNodeIsInNodeGroups = []string{"application_servers", "frankfurt"}
	node.config.RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent = []string{"application_servers"}
	node.mu.Unlock()
	node.report(context.Background())
	now := time.Now().UTC()
	joinedOnly := config.NodeFleetSettings{SendSilenceAlertsOfThisNodeOnlyToDestinations: &[]string{}, AlertSeverityWhenThisNodeGoesSilent: "info", RaiseToSosAfterThisNodeIsSilentFor: "5m", ThisNodeIsInNodeGroups: []string{"database_servers"}, RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent: []string{"database_servers"}}
	if _, status, err := hub.hub.Join("db-1", code.Secret, joinedOnly, "10.0.0.5", now); status != 200 || err != nil {
		t.Fatal(status, err)
	}
	hub.joined("db-1", now, joinedOnly)
	tick(hub, now)
	if err := hub.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewDaemon(hub.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
	if got := fleetCheck(restarted, "app-1").AlertSeveritySettings; got != (checksandalerts.AlertSeveritySettings{AlertSeverity: "warn", RaiseToSosAfterFailingFor: "10m"}) {
		t.Fatal(got)
	}
	if got := fleetCheck(restarted, "db-1").AlertSeveritySettings; got != joinedOnly.Silence() {
		t.Fatal(got)
	}
	if got := fleetCheck(restarted, "app-1").SendAlertsOnlyToDestinations; !reflect.DeepEqual(got, &[]string{"node_owner"}) {
		t.Fatal("the report's silence destinations were lost on restart", got)
	}
	if got := fleetCheck(restarted, "db-1").SendAlertsOnlyToDestinations; got == nil || len(*got) != 0 {
		t.Fatal("the join's empty silence destinations were lost on restart", got)
	}
	if got := fleetServer(t, restarted, "app-1").NodeGroups; !reflect.DeepEqual(got, []string{"application_servers", "frankfurt"}) {
		t.Fatal(got)
	}
	if got := fleetServer(t, restarted, "db-1").NodeGroups; !reflect.DeepEqual(got, []string{"database_servers"}) {
		t.Fatal(got)
	}
	// The raise by node group takes the node groups from there too: each
	// node is the only node of its node group, and both are silent at their
	// fresh deadline, long before either raise by time is due.
	restarted.mu.Lock()
	checks, _, _ := restarted.status(checksandalerts.NodeReportsCheckGroup, now, nil)
	deadline := restarted.snapshot.Checks[fleet.FleetKey("db-1")].Deadline
	restarted.mu.Unlock()
	if got := checks[fleet.FleetKey("app-1")].RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent; !reflect.DeepEqual(got, []string{"application_servers"}) {
		t.Fatal(got)
	}
	if got := checks[fleet.FleetKey("db-1")].RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent; !reflect.DeepEqual(got, []string{"database_servers"}) {
		t.Fatal(got)
	}
	tick(restarted, deadline)
	// The silence alerts of db-1 go to no destination, so its raise shows
	// only in its incident.
	if got := silenceSummary(t, restarted); !reflect.DeepEqual(got, []string{"failed app-1 warn", "failure_alert_severity_raised app-1 sos"}) || fleetCheck(restarted, "db-1").FailureIncident.AlertSeverity != "sos" {
		t.Fatal(got, fleetCheck(restarted, "db-1").FailureIncident)
	}
	if raised := intakeEvents(t, restarted)[1]; raised.Message != "no node of the node group application_servers is reporting to the hub (app-1 is silent), so this alert is now SOS (raise_to_sos_when_every_node_of_these_node_groups_is_silent: [application_servers])" {
		t.Fatalf("%+v", raised)
	}
}

// A node that replaces a silent node of the same name takes none of the old
// node's settings for the fleet.
func TestAReplacementNodeTakesNoneOfTheOldNodesSettingsForTheFleet(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	original := joinedNode(t, code, "app-1")
	original.mu.Lock()
	original.config.AlertSeverityWhenThisNodeGoesSilent, original.config.RaiseToSosAfterThisNodeIsSilentFor = "warn", "10m"
	original.config.ThisNodeIsInNodeGroups = []string{"old_node_group"}
	original.mu.Unlock()
	original.report(context.Background())
	if _, err := hub.hub.FleetOfNodeGroup(time.Now(), "old_node_group"); err != nil {
		t.Fatal(err)
	}
	// The original node is silent, and a new node joins under its name.
	hub.hub.Mu.Lock()
	hub.hub.Nodes["app-1"].Deadline = time.Now().Add(-time.Second)
	hub.hub.Mu.Unlock()
	now := time.Now().UTC()
	if _, status, err := hub.hub.Join("app-1", code.Secret, nodeSettings("info"), "10.0.0.6", now); status != 200 || err != nil {
		t.Fatal(status, err)
	}
	hub.joined("app-1", now, nodeSettings("info"))
	tick(hub, now)
	if got := fleetCheck(hub, "app-1").AlertSeveritySettings; got != (checksandalerts.AlertSeveritySettings{AlertSeverity: "info"}) {
		t.Fatal(got)
	}
	if got := fleetServer(t, hub, "app-1").NodeGroups; len(got) != 0 {
		t.Fatal(got)
	}
	if _, err := hub.hub.FleetOfNodeGroup(time.Now(), "old_node_group"); err == nil {
		t.Fatal("the node group of the replaced node is still there")
	}
}

// A report that the hub cannot save still counts as contact, and it changes
// none of the settings that the hub keeps for the node: they stay those of
// the node's latest saved report, which a restarted hub reads again.
func TestAReportThatTheHubCannotSaveChangesNoNodeSettings(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	node.mu.Lock()
	node.config.AlertSeverityWhenThisNodeGoesSilent, node.config.RaiseToSosAfterThisNodeIsSilentFor = "sos", "10m"
	node.config.SendSilenceAlertsOfThisNodeOnlyToDestinations = &[]string{"original"}
	node.config.ThisNodeIsInNodeGroups = []string{"frankfurt"}
	node.mu.Unlock()
	node.report(context.Background())
	saved := checksandalerts.AlertSeveritySettings{AlertSeverity: "sos", RaiseToSosAfterFailingFor: "10m"}
	if _, e := hub.hub.DB.Exec("CREATE TRIGGER reject_server BEFORE UPDATE ON nodes BEGIN SELECT RAISE(FAIL, 'fixture storage failure'); END"); e != nil {
		t.Fatal(e)
	}
	node.mu.Lock()
	node.config.AlertSeverityWhenThisNodeGoesSilent, node.config.RaiseToSosAfterThisNodeIsSilentFor = "warn", ""
	node.config.SendSilenceAlertsOfThisNodeOnlyToDestinations = &[]string{}
	node.config.ThisNodeIsInNodeGroups = []string{"berlin"}
	node.mu.Unlock()
	before := fleetCheck(hub, "app-1").Receipt
	time.Sleep(2 * time.Millisecond)
	node.report(context.Background())
	st := fleetCheck(hub, "app-1")
	if !strings.Contains(node.link.LastError, "503") || !st.Receipt.After(before) || st.AlertSeveritySettings != saved || !reflect.DeepEqual(st.SendAlertsOnlyToDestinations, &[]string{"original"}) {
		t.Fatalf("%q %+v", node.link.LastError, st)
	}
	if got := fleetServer(t, hub, "app-1").NodeGroups; !reflect.DeepEqual(got, []string{"frankfurt"}) {
		t.Fatal(got)
	}
	// The hub can save again: the next report brings the node's settings.
	if _, e := hub.hub.DB.Exec("DROP TRIGGER reject_server"); e != nil {
		t.Fatal(e)
	}
	node.report(context.Background())
	if got := fleetCheck(hub, "app-1").AlertSeveritySettings; got != (checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}) {
		t.Fatal(got)
	}
	if got := fleetCheck(hub, "app-1").SendAlertsOnlyToDestinations; got == nil || len(*got) != 0 {
		t.Fatal("the next saved report lost the empty destination list", got)
	}
	if got := fleetServer(t, hub, "app-1").NodeGroups; !reflect.DeepEqual(got, []string{"berlin"}) {
		t.Fatal(got)
	}
}

// A node can name at most MaxNodeGroupsOfANode node groups, so that a node
// whose configuration is valid can always join: a join request has a fixed
// largest size.
func TestANodeNamesAtMostAHundredNodeGroups(t *testing.T) {
	t.Parallel()
	names := func(n int) []string {
		out := []string{}
		for i := 0; i < n; i++ {
			out = append(out, fmt.Sprintf("%s_%03d", strings.Repeat("g", 60), i))
		}
		return out
	}
	most := config.NodeFleetSettings{AlertSeverityWhenThisNodeGoesSilent: "warn", ThisNodeIsInNodeGroups: names(config.MaxNodeGroupsOfANode)}
	request, _ := json.Marshal(fleet.JoinRequest{Name: strings.Repeat("n", 253), Secret: strings.Repeat("a", 64), NodeFleetSettings: most})
	if most.Problem() != "" || len(request) > fleet.JoinLimit {
		t.Fatal(most.Problem(), len(request))
	}
	tooMany := config.NodeFleetSettings{AlertSeverityWhenThisNodeGoesSilent: "warn", ThisNodeIsInNodeGroups: names(config.MaxNodeGroupsOfANode + 1)}
	if got := tooMany.Problem(); got != "has an invalid this_node_is_in_node_groups: names 101 node groups; a node can be in at most 100" {
		t.Fatal(got)
	}
	text := "this_node_is_in_node_groups: [" + strings.Join(names(config.MaxNodeGroupsOfANode+1), ", ") + "]\ncheck_groups: {a: {shell: 'true'}}\n"
	if _, err := config.Load(testhelpers.ConfigFile(t, text)); err == nil || !strings.Contains(err.Error(), "this_node_is_in_node_groups: names 101 node groups; a node can be in at most 100") {
		t.Fatal(err)
	}
	// A node with the most node groups joins and reports.
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	node.mu.Lock()
	node.config.ThisNodeIsInNodeGroups = names(config.MaxNodeGroupsOfANode)
	node.mu.Unlock()
	node.report(context.Background())
	if got := fleetServer(t, hub, "app-1").NodeGroups; node.link.LastError != "" || len(got) != config.MaxNodeGroupsOfANode {
		t.Fatal(node.link.LastError, len(got))
	}
}

// Silence alerts use the same routing rule as checks, including recorded
// recipients after a report changes the list while resolving the incident.
func TestSilenceAlertsFollowNamedDestinations(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		named   *[]string
		want    string
		unknown []string
	}{
		{name: "by severity", want: "team"},
		{name: "empty", named: &[]string{}},
		{name: "named", named: &[]string{"owner"}, want: "owner"},
		{name: "unknown", named: &[]string{"missing"}, want: "team", unknown: []string{"missing"}},
		{name: "mixed", named: &[]string{"owner", "missing"}, want: "owner", unknown: []string{"missing"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			hub, code, _ := startHubWith(t, map[string]checksandalerts.Destination{
				"team":  {RemindEvery: "1h", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://example.com/team"}},
				"owner": {RemindEvery: "1h", AlertSeverities: []string{}, Inputs: &checksandalerts.Webhook{URL: "https://example.com/owner"}},
			})
			node := joinedNode(t, code, "app-1")
			node.config.AlertSeverityWhenThisNodeGoesSilent = "warn"
			node.config.RaiseToSosAfterThisNodeIsSilentFor = "10m"
			node.config.SendSilenceAlertsOfThisNodeOnlyToDestinations = test.named
			node.report(context.Background())
			st := fleetCheck(hub, "app-1")
			if !reflect.DeepEqual(st.SendAlertsOnlyToDestinations, test.named) {
				t.Fatal("report lost the silence destinations", st)
			}
			hub.mu.Lock()
			checks, _, _ := hub.status("node_reports", st.Receipt, nil)
			hub.mu.Unlock()
			if got := checks["node_reports/app-1"].AlertDestinationsTheHubDoesNotHave; !slices.Equal(got, test.unknown) {
				t.Fatal("status lost the unknown destinations", got)
			}
			tick(hub, st.Deadline)
			groupAt(t, hub, st.Deadline)
			raisedAt := st.Receipt.Add(10 * time.Minute)
			tick(hub, raisedAt)
			groupAt(t, hub, raisedAt)
			hub.mu.Lock()
			err := hub.record(hub.remind(raisedAt.Add(time.Hour)))
			hub.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			groupAt(t, hub, raisedAt.Add(time.Hour))
			// A different current list cannot redirect the closing alert.
			node.config.SendSilenceAlertsOfThisNodeOnlyToDestinations = &[]string{}
			node.report(context.Background())
			tick(hub, time.Now())
			groupAt(t, hub, raisedAt.Add(2*time.Hour))
			want := map[string][]string{}
			if test.want != "" {
				want[test.want] = []string{"failed warn", "failure_alert_severity_raised sos", "reminder sos", "recovered sos"}
			}
			if got := queuedByDestination(t, hub); !reflect.DeepEqual(got, want) {
				t.Fatalf("deliveries: got %v, want %v", got, want)
			}
		})
	}
}

// Both hub-outage alerters receive the hub's selected destinations and deliver directly
// to them. Their independent incidents share one fleet incident key.
func TestBothHubOutageAlertersDeliverToTheHubsNamedDestinations(t *testing.T) {
	t.Parallel()
	destinations := outageAlerterDestinations()
	destinations["push"] = checksandalerts.Destination{RemindEvery: "never", AlertSeverities: []string{}, Inputs: &checksandalerts.Webhook{URL: "https://example.com/push"}}
	hub, code, _ := startHubWith(t, destinations)
	without := hub.hub.FleetSettings.Load()
	configured := *hub.config
	configured.SendSilenceAlertsOfThisNodeOnlyToDestinations = &[]string{"push"}
	served, err := fleet.NewFleetSettings(&configured)
	if err != nil || served.Validate(configured.Modules) != nil || served.Version == without.Version || !reflect.DeepEqual(served.SendSilenceAlertsOfTheHubOnlyToDestinations, &[]string{"push"}) {
		t.Fatal("hub's silence destinations were lost", served, err)
	}
	for _, names := range [][]string{{"bad name"}, {"push", "push"}, {"missing"}} {
		bad := *served
		bad.SendSilenceAlertsOfTheHubOnlyToDestinations = &names
		bad.Version = bad.ComputeVersion()
		if err := bad.Validate(configured.Modules); err == nil || !strings.Contains(err.Error(), "send_silence_alerts_of_the_hub_only_to_destinations") {
			t.Fatal("invalid fleet settings accepted", names, err)
		}
	}
	configured.SendSilenceAlertsOfThisNodeOnlyToDestinations = &[]string{}
	empty, err := fleet.NewFleetSettings(&configured)
	if err != nil || empty.Version == served.Version || empty.Version == without.Version {
		t.Fatal("omitted, empty and selected destinations need distinct versions", err)
	}
	hub.hub.FleetSettings.Store(served)
	var key string
	for _, name := range []string{"app-1", "app-2"} {
		hub.hub.FleetSettings.Store(served)
		node := joinedNode(t, code, name)
		node.report(context.Background())
		st := fleetCheck(node, checksandalerts.HubCheck)
		if !node.watching || st == nil || !reflect.DeepEqual(st.SendAlertsOnlyToDestinations, &[]string{"push"}) {
			t.Fatal("the hub-outage alerter did not fetch the hub's destinations", st)
		}
		tick(node, st.Deadline)
		want := map[string][]string{"push": {"failed sos"}}
		if got := queuedByDestination(t, node); !reflect.DeepEqual(got, want) {
			t.Fatalf("hub-outage alerter %s: got %v, want %v", name, got, want)
		}
		events := queuedEvents(t, node)
		if len(events) != 1 || events[0].FleetIncidentKey == "" || key != "" && key != events[0].FleetIncidentKey {
			t.Fatal("hub-outage alerters did not share the hub outage key", key, events)
		}
		key = events[0].FleetIncidentKey
		// Changing the current list to empty keeps the failure's recipients.
		hub.hub.FleetSettings.Store(empty)
		node.mu.Lock()
		if err := node.applyFleetSettings(empty); err != nil {
			t.Fatal(err)
		}
		node.mu.Unlock()
		node.report(context.Background())
		tick(node, time.Now())
		want["push"] = append(want["push"], "recovered sos")
		if got := queuedByDestination(t, node); !reflect.DeepEqual(got, want) {
			t.Fatal("hub-outage alerter recovery lost its original recipient", got)
		}
	}
}

// The hub validates the names that other machines send for silence alerts.
func TestTheHubValidatesSilenceAlertDestinationNames(t *testing.T) {
	t.Parallel()
	// The hub checks the syntax of names sent by another machine at join
	// and in reports, while allowing names absent from its destinations.
	hub, code := startHub(t)
	for index, names := range [][]string{{"bad name"}, {"owner", "owner"}, {"missing"}, {}} {
		settings := config.NodeFleetSettings{AlertSeverityWhenThisNodeGoesSilent: "warn", SendSilenceAlertsOfThisNodeOnlyToDestinations: &names}
		_, status, err := hub.hub.Join(fmt.Sprintf("app-%d", index), code.Secret, settings, "10.0.0.5", time.Now())
		report := fleet.NodeStatusReport{StartID: incidents.ID(), History: incidents.ID(), Seq: 1, After: -1, ReadingsAfter: -1, MeasurementHistoryID: incidents.ID(), AlertSeverityWhenThisNodeGoesSilent: "warn", SendSilenceAlertsOfThisNodeOnlyToDestinations: &names}
		invalid := len(names) > 0 && names[0] != "missing"
		if invalid && (status != 400 || err == nil || report.Validate() == nil) || !invalid && (status != 200 || err != nil || report.Validate() != nil) {
			t.Fatal("untrusted destination names", names, status, err, report.Validate())
		}
	}
}

// A node states raise_to_sos_when_every_node_of_these_node_groups_is_silent
// next to its node groups and sends it when it joins and in every report.
// While one node of the node group is silent, the alert keeps the node's own
// severity. Once every node of the node group is silent, the hub raises the
// silence alert of each node that states the setting to sos, with a message
// that names the node group and its silent nodes: of the node groups that a
// node names, the first one whose nodes are all silent. A raised alert stays
// sos after another node of the node group reports again.
func TestTheHubRaisesANodeGroupWhoseNodesAreAllSilent(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	jobs1, jobs2, app := joinedNode(t, code, "jobs-1"), joinedNode(t, code, "jobs-2"), joinedNode(t, code, "app-1")
	for node, raiseWhenSilent := range map[*Daemon][]string{jobs1: {"frankfurt", "jobs_servers"}, jobs2: {"jobs_servers"}, app: nil} {
		node.mu.Lock()
		node.config.AlertSeverityWhenThisNodeGoesSilent = "warn"
		node.config.ThisNodeIsInNodeGroups = []string{"frankfurt"}
		if node != app {
			node.config.ThisNodeIsInNodeGroups = []string{"frankfurt", "jobs_servers"}
		}
		node.config.RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent = raiseWhenSilent
		node.mu.Unlock()
		node.report(context.Background())
		if node.link.LastError != "" {
			t.Fatal(node.link.LastError)
		}
	}
	// The join carried the setting, and so did the report.
	hub.hub.Mu.Lock()
	joined, reported := hub.hub.Nodes["jobs-1"].NodeFleetSettings, hub.hub.Nodes["jobs-1"].NodeStatusReport.RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent
	hub.hub.Mu.Unlock()
	if want := []string{"frankfurt", "jobs_servers"}; !reflect.DeepEqual(joined.RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent, want) || !reflect.DeepEqual(reported, want) {
		t.Fatal(joined, reported)
	}
	// The hub refuses a node group that the node is not in.
	outside := config.NodeFleetSettings{AlertSeverityWhenThisNodeGoesSilent: "warn", RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent: []string{"jobs_servers"}}
	if _, status, err := hub.hub.Join("jobs-3", code.Secret, outside, "10.0.0.5", time.Now()); status != 400 || err == nil || !strings.Contains(err.Error(), `names the node group "jobs_servers", which the node is not in`) {
		t.Fatal(status, err)
	}
	// jobs-1 goes silent first, jobs-2 a minute later; app-1, the other
	// node in frankfurt, keeps reporting.
	opened := time.Now().UTC().Add(3 * reportInterval)
	hub.mu.Lock()
	hub.snapshot.Checks[fleet.FleetKey("jobs-1")].Deadline = opened
	hub.snapshot.Checks[fleet.FleetKey("jobs-2")].Deadline = opened.Add(time.Minute)
	hub.snapshot.Checks[fleet.FleetKey("app-1")].Deadline = opened.Add(time.Hour)
	checks, _, _ := hub.status(checksandalerts.NodeReportsCheckGroup, opened, nil)
	hub.mu.Unlock()
	if got := checks[fleet.FleetKey("jobs-2")].RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent; !reflect.DeepEqual(got, []string{"jobs_servers"}) {
		t.Fatal("status", got)
	}
	tick(hub, opened)
	tick(hub, opened.Add(30*time.Second))
	want := []string{"failed jobs-1 warn"}
	if got := silenceSummary(t, hub); !reflect.DeepEqual(got, want) {
		t.Fatal("one of two nodes of the node group is silent", got)
	}
	tick(hub, opened.Add(time.Minute))
	want = append(want, "failed jobs-2 warn", "failure_alert_severity_raised jobs-1 sos", "failure_alert_severity_raised jobs-2 sos")
	if got := silenceSummary(t, hub); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	events := intakeEvents(t, hub)
	if raised := events[2]; raised.Message != "no node of the node group jobs_servers is reporting to the hub (jobs-1 and jobs-2 are silent), so this alert is now SOS (raise_to_sos_when_every_node_of_these_node_groups_is_silent: [frankfurt, jobs_servers])" ||
		raised.Title != "jobs-1 still not reporting to the MMWatchdog fleet hub, now SOS" || raised.Incident != events[0].Incident {
		t.Fatalf("%+v", raised)
	}
	if raised := events[3]; raised.Message != "no node of the node group jobs_servers is reporting to the hub (jobs-1 and jobs-2 are silent), so this alert is now SOS (raise_to_sos_when_every_node_of_these_node_groups_is_silent: [jobs_servers])" {
		t.Fatalf("%+v", raised)
	}
	// jobs-2 reports again: jobs-1 stays sos until it reports itself.
	jobs2.mu.Lock()
	settings := jobs2.config.NodeFleetSettings()
	jobs2.mu.Unlock()
	hub.receipt("jobs-2", time.Time{}, settings, opened.Add(2*time.Minute))
	tick(hub, opened.Add(2*time.Minute))
	want = append(want, "recovered jobs-2 sos")
	if got := silenceSummary(t, hub); !reflect.DeepEqual(got, want) || fleetCheck(hub, "jobs-1").FailureIncident.AlertSeverity != "sos" {
		t.Fatal(got, fleetCheck(hub, "jobs-1").FailureIncident)
	}
}

// Only a node that states the setting itself is raised: another silent node
// of the same node group without the setting keeps its own severity, because
// each node's configuration says what its own silence means.
func TestANodeGroupRaisesOnlyTheNodesThatStateTheSetting(t *testing.T) {
	t.Parallel()
	hub := newHubDaemon(t, nil)
	now := time.Now().UTC()
	hub.joined("jobs-1", now, config.NodeFleetSettings{AlertSeverityWhenThisNodeGoesSilent: "warn", ThisNodeIsInNodeGroups: []string{"jobs_servers"}, RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent: []string{"jobs_servers"}})
	hub.joined("jobs-2", now, config.NodeFleetSettings{AlertSeverityWhenThisNodeGoesSilent: "warn", ThisNodeIsInNodeGroups: []string{"jobs_servers"}})
	hub.mu.Lock()
	for _, name := range []string{"jobs-1", "jobs-2"} {
		hub.snapshot.Checks[fleet.FleetKey(name)].Deadline = now
	}
	hub.mu.Unlock()
	tick(hub, now)
	tick(hub, now.Add(time.Minute))
	want := []string{"failed jobs-1 warn", "failed jobs-2 warn", "failure_alert_severity_raised jobs-1 sos"}
	if got := silenceSummary(t, hub); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	if st := fleetCheck(hub, "jobs-2"); st.FailureIncident.AlertSeverity != "warn" {
		t.Fatal(st.FailureIncident)
	}
}

// A node whose silence alert a window holds does not count as silent, as
// for the raise by count: while the last report of jobs-2 shows a whole-node
// window, the silence of jobs-1 raises nothing. When the window ends and
// jobs-2 is still silent, every node of the node group is silent. The hub
// itself counts as a node that reports: a node group that the hub is in is
// never silent.
func TestANodeGroupWithANodeThatIsHeldOrReportingIsNotSilent(t *testing.T) {
	t.Parallel()
	settings := config.NodeFleetSettings{AlertSeverityWhenThisNodeGoesSilent: "warn", ThisNodeIsInNodeGroups: []string{"jobs_servers"}, RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent: []string{"jobs_servers"}}
	for _, hubInNodeGroup := range []bool{false, true} {
		hub := newHubDaemon(t, nil)
		now := time.Now().UTC()
		hub.mu.Lock()
		if hubInNodeGroup {
			hub.config.ThisNodeIsInNodeGroups = []string{"jobs_servers"}
		}
		hub.mu.Unlock()
		hub.joined("jobs-1", now, settings)
		hub.joined("jobs-2", now, settings)
		hub.receipt("jobs-1", time.Time{}, settings, now)
		// The last report of jobs-2 shows a whole-node window of one hour.
		hub.receipt("jobs-2", now.Add(time.Hour), settings, now)
		tick(hub, now.Add(3*reportInterval))
		tick(hub, now.Add(30*time.Minute))
		if got := silenceSummary(t, hub); !reflect.DeepEqual(got, []string{"failed jobs-1 warn"}) {
			t.Fatal(hubInNodeGroup, got)
		}
		tick(hub, now.Add(time.Hour))
		tick(hub, now.Add(time.Hour+time.Minute))
		want := []string{"failed jobs-1 warn", "failed jobs-2 warn", "failure_alert_severity_raised jobs-1 sos", "failure_alert_severity_raised jobs-2 sos"}
		if hubInNodeGroup {
			want = want[:2]
		}
		if got := silenceSummary(t, hub); !reflect.DeepEqual(got, want) {
			t.Fatal(hubInNodeGroup, got)
		}
	}
}

// A hub one minor release newer than its node may send a setting that the node
// does not know. The node reads the settings it knows, and applies them.
func TestANodeReadsTheSettingsOfANewerHub(t *testing.T) {
	t.Parallel()
	hub, code, _ := startHubWith(t, outageAlerterDestinations())
	hub.mu.Lock()
	served, err := fleet.NewFleetSettings(hub.config)
	hub.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	written, err := json.Marshal(served)
	if err != nil {
		t.Fatal(err)
	}
	written = append(bytes.TrimSuffix(written, []byte("}")), []byte(`,"a_setting_of_a_newer_release":{"on":true}}`)...)
	node := joinedNode(t, code, "app-1")
	received, err := decodeHubSettings(written, node.config.Modules)
	if err != nil || received.Version != served.Version {
		t.Fatal("the node refused the settings of a newer hub:", err)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if err := node.applyFleetSettings(received); err != nil || len(node.fleetSettings.Destinations) != len(served.Destinations) {
		t.Fatal(err)
	}
}

// A joined node runs the hub's destination modules as MMWatchdog's own
// account, from /, with no added environment: the run settings that the hub
// states for a destination stay on the hub.
func TestANodeIgnoresTheRunSettingsOfTheHubsDestinations(t *testing.T) {
	t.Parallel()
	call := &checksandalerts.ModuleCall{Name: "own_alarm", ExecOptions: primitives.ExecOptions{RunAsUserAccount: "root", RunFromDirectory: "/tmp", Env: map[string]string{"PYTHONWARNINGS": "ignore"}}}
	served := &fleet.FleetSettings{Destinations: map[string]checksandalerts.Destination{"own_alarm": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, TimeLimitPerDeliveryAttempt: "5s", Inputs: call}}, FileContentsByDestination: map[string]map[string]string{"own_alarm": {}}, Fleet: config.DefaultFleetTimings, AlertSeverityWhenTheHubGoesSilent: "sos"}
	served.Version = served.ComputeVersion()
	written, err := json.Marshal(served)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), `"run_as_user_account":"root"`) {
		t.Fatal("the hub's settings do not carry the run settings:", string(written))
	}
	received, err := decodeHubSettings(written, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, notCore := received.Destinations["own_alarm"].Inputs.(*checksandalerts.ModuleCall)
	if !notCore || !reflect.DeepEqual(got.ExecOptions, fleet.JoinedNodeDestinationRunSettings()) || got.RunAsUserAccount == "root" || len(got.Env) != 0 {
		t.Fatalf("%+v", received.Destinations["own_alarm"].Inputs)
	}
}
