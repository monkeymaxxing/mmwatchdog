package daemon

import (
	"os"
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
	"mmwatchdog/internal/testhelpers"
)

// A check that names its destinations sends every alert to exactly those,
// whatever their alert_severities say: the failure, a raise, a reminder,
// and the recovery. A check that names none goes by severity, and so does
// an alert that belongs to no check. A destination with alert_severities:
// [] gets only the alerts of the checks that name it.
func TestACheckThatNamesItsDestinations(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	webhook := func(name string) *checksandalerts.Webhook {
		return &checksandalerts.Webhook{URL: "https://example.com/" + name}
	}
	c.Alerts.Destinations = map[string]checksandalerts.Destination{
		"team_chat":               {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: webhook("team")},
		"on_call_pager":           {RemindEvery: "never", AlertSeverities: []string{"sos"}, Inputs: webhook("pager")},
		"mail_administrator_chat": {AlertSeverities: []string{}, Inputs: webhook("mail"), RemindEvery: "1h"},
	}
	administrator := []string{"mail_administrator_chat"}
	immediately := checksandalerts.Timing{AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}
	c.CheckGroups = map[string]checksandalerts.CheckGroup{
		"mail_server": {Timing: immediately, Checks: map[string]checksandalerts.NamedCheck{
			"postfix_service_is_running": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "postfix.service", FailWhen: "the_unit_is_not_active"}, SendAlertsOnlyToDestinations: &administrator}},
		}},
		"app": {Timing: immediately, Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "false"}}},
	}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if got := d.config.CheckGroups["mail_server"].Checks["postfix_service_is_running"].SendAlertsOnlyToDestinations; got == nil || !reflect.DeepEqual(*got, administrator) {
		t.Fatal("the configuration lost the destinations of the check", got)
	}
	now := time.Now().UTC()
	const postfix, app = "mail_server/postfix_service_is_running", "app/shell"
	observe := func(key, status string, at time.Time) []checksandalerts.Event {
		return d.snapshot.Checks[key].Observe(checksandalerts.Observation{Status: status, Message: status, At: at}, d.checks[key].def.Timing, false)
	}
	d.mu.Lock()
	events := append(observe(postfix, "failed", now), observe(app, "failed", now)...)
	// A raise adds no destination: the pager takes sos and is not named.
	st := d.snapshot.Checks[postfix]
	events = append(events, st.Raise(&st.FailureIncident, checksandalerts.FailureIncidentEvents, "sos", "test", now.Add(time.Minute), false)...)
	if err = d.record(events); err != nil {
		t.Fatal(err)
	}
	// The named destination reminds at its own remind_every after the
	// raise, also though it takes no severity; the other check has no
	// destination to remind.
	if err = d.record(d.remind(now.Add(time.Hour + time.Minute))); err != nil {
		t.Fatal(err)
	}
	events = append(observe(postfix, "ok", now.Add(2*time.Hour)), observe(app, "ok", now.Add(2*time.Hour))...)
	// A maintenance alert belongs to a check group, so it goes by severity.
	events = append(events, incidents.MaintenanceEvent("maintenance_on", []string{"mail_server"}, checksandalerts.Window{Until: now.Add(3 * time.Hour)}, now.Add(2*time.Hour), "maintenance"))
	err = d.record(events)
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"mail_administrator_chat": {"failed warn", "failure_alert_severity_raised sos", "reminder sos", "recovered sos"},
		"team_chat":               {"failed warn", "recovered warn", "maintenance_on info"},
	}
	if got := queuedByDestination(t, d); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	// Every alert of the check carries the names, for the hub of a fleet.
	var named int
	if e := d.store.DB.QueryRow(`SELECT COUNT(*) FROM outbox WHERE json_extract(event,'$.send_alerts_only_to_destinations[0]')='mail_administrator_chat'`).Scan(&named); e != nil || named != 4 {
		t.Fatal(named, e)
	}

	// The list is no part of the check's identity: changing it keeps an open
	// incident, and the next alert follows the new list. An empty list
	// sends no alert for the check; its events are only recorded.
	d.mu.Lock()
	err = d.record(observe(postfix, "failed", now.Add(4*time.Hour)))
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	incident := d.snapshot.Checks[postfix].FailureIncident.Incident
	nobody := []string{}
	group := c.CheckGroups["mail_server"]
	group.Checks["postfix_service_is_running"] = checksandalerts.NamedCheck{Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "postfix.service", FailWhen: "the_unit_is_not_active"}, SendAlertsOnlyToDestinations: &nobody}}
	writeConfig(t, p, c)
	if _, err = d.reload(); err != nil {
		t.Fatal(err)
	}
	if got := d.snapshot.Checks[postfix]; got.FailureIncident.Incident != incident || got.SendAlertsOnlyToDestinations == nil || len(*got.SendAlertsOnlyToDestinations) != 0 {
		t.Fatalf("the reload lost the incident or the new list: %+v", got)
	}
	before := outboxCount(t, d, "mail_administrator_chat") + outboxCount(t, d, "team_chat") + outboxCount(t, d, "on_call_pager")
	d.mu.Lock()
	err = d.record(observe(postfix, "ok", now.Add(5*time.Hour)))
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if after := outboxCount(t, d, "mail_administrator_chat") + outboxCount(t, d, "team_chat") + outboxCount(t, d, "on_call_pager"); after != before+1 {
		t.Fatal("the closing alert lost the incident's recipient after the named list became empty", before, after)
	}
	history, err := d.store.CheckGroupEvents("mail_server", 1)
	if err != nil || len(history) != 1 || history[0].Type != "recovered" || history[0].Title == "" {
		t.Fatal("the recovery of an announced incident was not sent to its recorded recipients", history, err)
	}
}

// MMWatchdog makes a reminder only for a destination that gets the incident's
// alerts. A destination that reminds for the checks that name it, with
// alert_severities: [], gets no reminder of a check that names none, and a
// destination that takes only sos gets reminders of a warn incident only
// after its raise to sos, one remind_every after the raise. So the event
// history holds no reminder that no destination got: each reminder in it
// is queued for the destination it names.
func TestAReminderIsMadeOnlyForADestinationThatGetsTheIncident(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	webhook := func(name string) *checksandalerts.Webhook {
		return &checksandalerts.Webhook{URL: "https://example.com/" + name}
	}
	c.Alerts.Destinations = map[string]checksandalerts.Destination{
		"team_chat":                          {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: webhook("team")},
		"team_chat_reminding_every_hour":     {AlertSeverities: []string{}, Inputs: webhook("team"), RemindEvery: "1h"},
		"on_call_pager_reminding_every_hour": {AlertSeverities: []string{"sos"}, Inputs: webhook("pager"), RemindEvery: "1h"},
	}
	reminding := []string{"team_chat_reminding_every_hour"}
	immediately := checksandalerts.Timing{AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}
	c.CheckGroups = map[string]checksandalerts.CheckGroup{
		"mail_server": {Timing: immediately, Checks: map[string]checksandalerts.NamedCheck{
			"postfix_service_is_running": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "postfix.service", FailWhen: "the_unit_is_not_active"}, SendAlertsOnlyToDestinations: &reminding}},
		}},
		"app": {Timing: immediately, Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "false"}, AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn", RaiseToSosAfterFailingFor: "90m"}}},
	}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Now().UTC()
	const postfix, app = "mail_server/postfix_service_is_running", "app/shell"
	observe := func(key string, at time.Time) []checksandalerts.Event {
		return d.snapshot.Checks[key].Observe(checksandalerts.Observation{Status: "failed", Message: "failed", At: at}, d.checks[key].def.Timing, false)
	}
	d.mu.Lock()
	events := append(observe(postfix, now), observe(app, now)...)
	// The scheduler tick's raise and reminder steps.
	for _, at := range []time.Duration{time.Hour, 90 * time.Minute, 150 * time.Minute} {
		events = append(append(events, d.raiseOpenIncidents(now.Add(at))...), d.remind(now.Add(at))...)
	}
	err = d.record(events)
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	history, err := d.store.Events(100)
	if err != nil {
		t.Fatal(err)
	}
	recorded := []string{}
	for _, e := range slices.Backward(history) {
		if e.Type == "reminder" {
			recorded = append(recorded, e.AboutCheck()+" "+e.AlertSeverity+" to "+e.Destination)
		}
	}
	want := []string{
		postfix + " warn to team_chat_reminding_every_hour",
		app + " sos to on_call_pager_reminding_every_hour",
		postfix + " warn to team_chat_reminding_every_hour",
	}
	if !reflect.DeepEqual(recorded, want) {
		t.Fatalf("the event history holds the reminders\n%q\nwant\n%q", recorded, want)
	}
	// The second reminder of the mail server's check replaced its first,
	// undelivered one.
	queued := []string{}
	for _, e := range queuedReminders(t, d) {
		queued = append(queued, e.AboutCheck()+" "+e.AlertSeverity+" to "+e.Destination)
	}
	if !reflect.DeepEqual(queued, want[1:]) {
		t.Fatalf("queued the reminders\n%q\nwant\n%q", queued, want[1:])
	}
}

// On a standalone node and on the hub, a name that is not in
// alert_destinations.yml is a validation error, so a mistyped name never
// loses an alert. A node that has joined a hub cannot know the hub's
// destinations when it validates, so it accepts any valid name.
func TestNamedDestinationsAreValidated(t *testing.T) {
	t.Parallel()
	destinations := "destinations:\n  team_chat: {remind_every: never, webhook: {url: 'https://example.com/team'}, alert_severities: [info, warn, sos]}\n  mail_administrator_chat: {remind_every: never, webhook: {url: 'https://example.com/mail'}, alert_severities: []}\n"
	load := func(checkGroups, node string) (*config.Config, error) {
		t.Helper()
		path := testhelpers.ConfigFile(t, node+checkGroups)
		alerts := filepath.Join(path, "alert_destinations.yml")
		if node == "" {
			if e := os.WriteFile(alerts, []byte(destinations), 0600); e != nil {
				t.Fatal(e)
			}
		}
		return config.Load(path)
	}
	for _, tc := range []struct{ checkGroups, want string }{
		{"check_groups: {mail: {checks: {postfix: {shell: 'true', send_alerts_only_to_destinations: [mail_administrator_chat]}}}}\n", ""},
		{"check_groups: {mail: {checks: {postfix: {shell: 'true', send_alerts_only_to_destinations: [team_chat, mail_administrator_chat]}}}}\n", ""},
		// A check that names nobody is silent on purpose.
		{"check_groups: {mail: {checks: {postfix: {shell: 'true', send_alerts_only_to_destinations: []}}}}\n", ""},
		// A direct check and a heartbeat state it beside their alert_severity.
		{"check_groups: {mail: {shell: 'true', send_alerts_only_to_destinations: [team_chat]}}\n", ""},
		{"heartbeat_receiver: {listen_on_address: '127.0.0.1:8444', tls_certificate_file: /c, tls_private_key_file: /k}\ncheck_groups: {backup: {heartbeat: {max_time_between_heartbeats: 1h, expected_bearer_token_file: /t}, send_alerts_only_to_destinations: [team_chat]}}\n", ""},
		{"check_groups: {mail: {checks: {postfix: {shell: 'true', send_alerts_only_to_destinations: [mail_admin_chat]}}}}\n", `check_groups.mail.checks.postfix.send_alerts_only_to_destinations: alert_destinations.yml has no destination named "mail_admin_chat"`},
		{"check_groups: {mail: {shell: 'true', send_alerts_only_to_destinations: [pager]}}\n", `check_groups.mail.send_alerts_only_to_destinations: alert_destinations.yml has no destination named "pager"`},
		{"check_groups: {mail: {checks: {postfix: {shell: 'true', send_alerts_only_to_destinations: [team_chat, team_chat]}}}}\n", `send_alerts_only_to_destinations: names the destination "team_chat" twice`},
		{"check_groups: {mail: {checks: {postfix: {shell: 'true', send_alerts_only_to_destinations: ['team chat']}}}}\n", `send_alerts_only_to_destinations: "team chat" is not a valid destination name`},
		{"check_groups: {mail: {checks: {postfix: {shell: 'true', send_alerts_only_to_destinations: team_chat}}}}\n", "expected a list"},
		// A check that only records never alerts, so it names nobody.
		{"check_groups: {machine: {checks: {memory: {only_record_readings: true, node_resource: memory, send_alerts_only_to_destinations: [team_chat]}}}}\n", "check_groups.machine.checks.memory.send_alerts_only_to_destinations: a node_resource check that only records its readings never alerts"},
		// A check group of named checks states it on each check: on the
		// check group it would apply to none of them.
		{"check_groups: {mail: {send_alerts_only_to_destinations: [], checks: {postfix: {shell: 'true'}}}}\n", "check_groups.mail.send_alerts_only_to_destinations: set send_alerts_only_to_destinations on each check under checks, not on the check group"},
	} {
		c, err := load(tc.checkGroups, "")
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%q: got %v, want %q", tc.checkGroups, err, tc.want)
		}
		if tc.want == "" && err == nil && len(config.CheckDefinitions(c)) != 1 {
			t.Errorf("%q: %v", tc.checkGroups, config.CheckDefinitions(c))
		}
	}
	// The destinations a check names are no part of its identity.
	named, _ := load("check_groups: {mail: {checks: {postfix: {shell: 'true', send_alerts_only_to_destinations: [team_chat]}}}}\n", "")
	plain, _ := load("check_groups: {mail: {checks: {postfix: {shell: 'true'}}}}\n", "")
	if a, b := config.CheckDefinitions(named)["mail/postfix"], config.CheckDefinitions(plain)["mail/postfix"]; a.Identity() != b.Identity() || a.SendAlertsOnlyToDestinations == nil || b.SendAlertsOnlyToDestinations != nil {
		t.Fatal("naming destinations changed the identity of the check, or the definition lost the names")
	}
	// A joined node has no destinations file; the fleet settings decide.
	code := filepath.Join(t.TempDir(), "join-code")
	joined := "join_fleet_hub: {join_code_file: " + code + "}\n"
	if _, err := load("check_groups: {mail: {checks: {postfix: {shell: 'true', send_alerts_only_to_destinations: [a_destination_of_the_hub]}}}}\n", joined); err != nil {
		t.Fatal("a joined node refused a destination name it cannot check:", err)
	}
	if _, err := load("check_groups: {mail: {checks: {postfix: {shell: 'true', send_alerts_only_to_destinations: ['not a name']}}}}\n", joined); err == nil {
		t.Fatal("a joined node accepted an invalid destination name")
	}
}

// The hub delivers a joined node's alert to exactly the destinations its
// check names. A name the hub does not have never loses the alert: it then
// goes by severity. Alerts of checks that name different destinations
// never share a grouped message.
func TestTheHubDeliversToNamedDestinations(t *testing.T) {
	t.Parallel()
	team, mail := captureAlerts(t), captureAlerts(t)
	hub, _, _ := startHubWith(t, map[string]checksandalerts.Destination{
		"team_chat":               {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: team.URL}},
		"mail_administrator_chat": {RemindEvery: "never", AlertSeverities: []string{}, Inputs: &checksandalerts.Webhook{URL: mail.URL}},
	})
	now := time.Now().UTC()
	alert := func(node, kind string, names ...string) checksandalerts.Event {
		e := checkAlert(kind, incidents.ID(), "mail_server", "postfix", now)
		if names != nil {
			e.SendAlertsOnlyToDestinations = &names
		}
		return acceptAlert(t, hub, node, e)
	}
	queued := func() map[string][]string {
		t.Helper()
		rows, e := hub.store.DB.Query("SELECT destination, COALESCE(json_extract(event,'$.node'),'') || ' ' || json_extract(event,'$.type') || ' ' || COALESCE(json_extract(event,'$.node_count'),1) FROM outbox ORDER BY seq")
		if e != nil {
			t.Fatal(e)
		}
		defer rows.Close()
		out := map[string][]string{}
		for rows.Next() {
			var name, what string
			rows.Scan(&name, &what)
			out[name] = append(out[name], what)
		}
		return out
	}
	alert("mail-1", "failed", "mail_administrator_chat")
	groupAt(t, hub, now)
	// Within the grouping window: two more nodes that name the same
	// destination share a message; the node that names none, and the node
	// with a name the hub does not have, go by severity, each on its own.
	alert("mail-2", "failed", "mail_administrator_chat")
	alert("mail-3", "failed", "mail_administrator_chat")
	alert("mail-4", "failed")
	alert("mail-5", "failed", "mail_admin_chat")
	groupAt(t, hub, now.Add(time.Second))
	groupAt(t, hub, now.Add(2*time.Minute))
	want := map[string][]string{
		"mail_administrator_chat": {"mail-1 failed 1", " failed 2"},
		"team_chat":               {"mail-4 failed 1", "mail-5 failed 1"},
	}
	if got := queued(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	// The recovery goes where the failure went.
	recovered := checkAlert("recovered", "", "mail_server", "postfix", now.Add(3*time.Minute))
	recovered.Incident = "incident-of-mail-1"
	names := []string{"mail_administrator_chat"}
	recovered.SendAlertsOnlyToDestinations = &names
	acceptAlert(t, hub, "mail-1", recovered)
	groupAt(t, hub, now.Add(10*time.Minute))
	if got := queued(); len(got["mail_administrator_chat"]) != 3 || got["mail_administrator_chat"][2] != "mail-1 recovered 1" || len(got["team_chat"]) != 2 {
		t.Fatalf("%v", got)
	}
}

// mmwatchdog status on a node that has joined a hub names the destinations of a
// check that the fleet settings do not have, and a node with no hub and no
// destination says that it sends alerts to nobody.
func TestStatusShowsWrongDestinationNamesAndNoDestination(t *testing.T) {
	t.Parallel()
	_, code, _ := startHubWith(t, map[string]checksandalerts.Destination{"team_chat": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://example.com/team"}}})
	node := joinedNode(t, code, "mail-1")
	names := []string{"team_chat", "mail_admin_chat"}
	node.mu.Lock()
	node.snapshot.Checks["app/shell"].SendAlertsOnlyToDestinations = &names
	node.fleetSettings = &fleet.FleetSettings{Destinations: map[string]checksandalerts.Destination{"team_chat": {RemindEvery: "never"}}}
	checks, _, _ := node.status("", time.Now(), nil)
	node.mu.Unlock()
	if got := checks["app/shell"].AlertDestinationsTheHubDoesNotHave; !reflect.DeepEqual(got, []string{"mail_admin_chat"}) {
		t.Fatal(got)
	}
	// The known name still gets the alert; the unknown name cannot add
	// other recipients by severity.
	alert := checksandalerts.Event{AlertSeverity: "warn", Type: "failed", SendAlertsOnlyToDestinations: &names}
	destinations := map[string]checksandalerts.Destination{"team_chat": {RemindEvery: "never", AlertSeverities: []string{"sos"}}, "other": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities}}
	if !config.GoesTo("team_chat", destinations["team_chat"], alert, destinations) || config.GoesTo("other", destinations["other"], alert, destinations) {
		t.Fatal("an unknown destination discarded the valid name")
	}
}
