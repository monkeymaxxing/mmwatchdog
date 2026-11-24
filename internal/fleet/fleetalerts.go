package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/store"
)

// The fleet alert route: a joined node sends its alerts to the hub, falling
// back to the destinations in its copy of the fleet settings; the hub saves
// them and passes them, with its own, through one alert grouping step. The rules are in
// docs/changing-mmwatchdog/implementation-rules.md under "Alert route on a joined node" and "Alert
// intake and alert groups". A test of a hub shortens its alert grouping
// window (AlertGroupingState.window).
const (
	AlertGroupingWindow = time.Minute
	groupQuiet          = 10 * time.Minute
	lateAfter           = 2 * time.Minute
)

const (
	AlertLimit        = 64 * 1024 // encoded alert sent to the hub
	FleetSettingsFile = "fleet-settings-from-the-hub.json"
	AlertIntake       = "alert intake"
	AlertGrouping     = "fleet alert grouping"
	AlertFallback     = "alert fallback"
)

// FleetSettings is what joined nodes copy from the hub: each alert
// destination with the content of its credential files, for direct delivery
// and reminders, the destinations whose service keeps incidents, the fleet
// timings, and the hub's own silence severity and its raise to sos.
// Version changes whenever any
// of them changes.
type FleetSettings struct {
	Version      string                                 `json:"version"`
	Destinations map[string]checksandalerts.Destination `json:"destinations"`
	// FileContentsByDestination holds, for each destination, the content of
	// every credential file that an input of its module names, under the
	// input's name: a webhook's token under bearer_token_file, a mail
	// server's password under login_password_file, the MMHotline signing secret
	// under mmhotline_signing_secret_file, and each file of a module that is
	// not core.
	FileContentsByDestination map[string]map[string]string `json:"file_contents_by_destination"`
	Fleet                     config.FleetTimings          `json:"fleet"`
	// AlertSeverityWhenTheHubGoesSilent is the hub's
	// alert_severity_when_this_node_goes_silent, for the alert of its hub-outage alerter, and
	// RaiseToSosAfterTheHubIsSilentFor the hub's
	// raise_to_sos_after_this_node_is_silent_for, which raises that alert.
	AlertSeverityWhenTheHubGoesSilent           string                   `json:"alert_severity_when_the_hub_goes_silent"`
	SendSilenceAlertsOfTheHubOnlyToDestinations *[]string                `json:"send_silence_alerts_of_the_hub_only_to_destinations,omitempty"`
	RaiseToSosAfterTheHubIsSilentFor            checksandalerts.Duration `json:"raise_to_sos_after_the_hub_is_silent_for,omitempty"`
	// DestinationsWhoseServiceKeepsIncidents are the destinations whose
	// module's description on the hub states service_keeps_incidents: true.
	// A joined node that delivers without the hub sends them no alert that
	// closes an incident, whatever its own copy of the module says, because
	// the hub's modules decide how the hub sends them alerts.
	DestinationsWhoseServiceKeepsIncidents []string `json:"destinations_whose_service_keeps_incidents,omitempty"`
}

// NewFleetSettings reads the hub's destinations and the content of their
// credential files, and takes its fleet timings. The hub reads them when it
// starts and reloads.
func NewFleetSettings(c *config.Config) (*FleetSettings, error) {
	s := &FleetSettings{Destinations: map[string]checksandalerts.Destination{}, FileContentsByDestination: map[string]map[string]string{}, Fleet: c.FleetHubRole.FleetTimings, AlertSeverityWhenTheHubGoesSilent: c.AlertSeverityWhenThisNodeGoesSilent, SendSilenceAlertsOfTheHubOnlyToDestinations: c.SendSilenceAlertsOfThisNodeOnlyToDestinations, RaiseToSosAfterTheHubIsSilentFor: c.RaiseToSosAfterThisNodeIsSilentFor}
	for _, name := range checksandalerts.Names(c.Alerts.Destinations) {
		d := c.Alerts.Destinations[name]
		if d.ServiceKeepsIncidents() {
			s.DestinationsWhoseServiceKeepsIncidents = append(s.DestinationsWhoseServiceKeepsIncidents, name)
		}
		contents, input, e := d.ReadCredentialFiles()
		if e != nil {
			return nil, c.ErrorAt("destinations."+name+"."+d.PathOfInput(input), e.Error())
		}
		d.CaptureCredentialContent(contents)
		s.Destinations[name] = d
		if len(contents) > 0 {
			s.FileContentsByDestination[name] = contents
		}
	}
	s.Version = s.ComputeVersion()
	return s, nil
}

// ComputeVersion is the fingerprint of the settings that Version names.
func (s *FleetSettings) ComputeVersion() string {
	return config.Fingerprint(struct {
		Destinations        map[string]checksandalerts.Destination
		FileContents        map[string]map[string]string
		Fleet               config.FleetTimings
		SilenceDestinations *[]string
		SilentSeverity      string
		RaiseToSos          checksandalerts.Duration
		KeepIncidents       []string
	}{s.Destinations, s.FileContentsByDestination, s.Fleet, s.SendSilenceAlertsOfTheHubOnlyToDestinations, s.AlertSeverityWhenTheHubGoesSilent, s.RaiseToSosAfterTheHubIsSilentFor, s.DestinationsWhoseServiceKeepsIncidents})
}

// Validate checks a fetched copy with the rules for alert_destinations.yml and the hub's
// fleet timings.
func (s *FleetSettings) Validate(modules *checksandalerts.ModuleCatalog) error {
	if s.Destinations == nil || s.FileContentsByDestination == nil || s.Version != s.ComputeVersion() {
		return fmt.Errorf("fleet settings are incomplete or the version does not match the contents")
	}
	if field, message := s.Fleet.Problem(); message != "" {
		return fmt.Errorf("fleet settings have an invalid %s: %s", field, message)
	}
	if config.SeverityRank(s.AlertSeverityWhenTheHubGoesSilent) < 0 {
		return fmt.Errorf("fleet settings have an invalid alert_severity_when_the_hub_goes_silent")
	}
	if problem := config.DestinationNamesProblem(s.SendSilenceAlertsOfTheHubOnlyToDestinations); problem != "" {
		return fmt.Errorf("fleet settings have an invalid send_silence_alerts_of_the_hub_only_to_destinations: %s", problem)
	}
	if unknown := config.UnknownDestinationNames(s.SendSilenceAlertsOfTheHubOnlyToDestinations, s.Destinations); len(unknown) > 0 {
		return fmt.Errorf("fleet settings send_silence_alerts_of_the_hub_only_to_destinations names an unknown destination %q", unknown[0])
	}
	if s.RaiseToSosAfterTheHubIsSilentFor != "" {
		if v, e := checksandalerts.ParseDuration(string(s.RaiseToSosAfterTheHubIsSilentFor)); e != nil || v < time.Second {
			return fmt.Errorf("fleet settings have an invalid raise_to_sos_after_the_hub_is_silent_for")
		}
	}
	// A destination of the hub can name a module that is not core. This
	// node runs the module when it has it, so the modules are this node's.
	c := &config.Config{Sources: map[string]checksandalerts.SourceLocation{}, Alerts: config.Alerts{Destinations: s.Destinations}, Modules: modules}
	if e := c.ValidateAlerts(true); e != nil {
		return e
	}
	// Each destination has the content of exactly the credential files that
	// its inputs name, and each content is one that its module can have
	// read.
	for _, name := range checksandalerts.Names(s.FileContentsByDestination) {
		if _, ok := s.Destinations[name]; !ok {
			return fmt.Errorf("fleet settings contain credential file contents for unknown hub destination %q", name)
		}
	}
	for _, name := range s.DestinationsWhoseServiceKeepsIncidents {
		if _, ok := s.Destinations[name]; !ok {
			return fmt.Errorf("fleet settings list %q as a destination whose service keeps incidents, but the hub has no destination with that name", name)
		}
	}
	for _, name := range checksandalerts.Names(s.Destinations) {
		d, contents := s.Destinations[name], s.FileContentsByDestination[name]
		files := d.Inputs.CredentialFiles()
		for _, input := range checksandalerts.Names(files) {
			if _, ok := contents[input]; !ok {
				return fmt.Errorf("fleet settings lack credential file contents for destination %q, input %s", name, input)
			}
		}
		if len(contents) != len(files) {
			return fmt.Errorf("fleet settings contain credential file contents not named by the module inputs of destination %q", name)
		}
		for _, input := range checksandalerts.Names(contents) {
			if !d.Inputs.ValidCredential(input, contents[input]) {
				return fmt.Errorf("fleet settings contain invalid credential file contents for destination %q, input %s", name, input)
			}
		}
		if d.CredentialFilesTooLarge(contents) != nil {
			return fmt.Errorf("fleet settings contain credential file contents for destination %q whose combined size exceeds the limit", name)
		}
		// Rebuild this private comparison value from the authenticated copy.
		// It never travels in the fleet settings JSON.
		d.CaptureCredentialContent(contents)
		s.Destinations[name] = d
	}
	return nil
}

// ReadFleetSettings returns the stored copy, or nil when there is none or it
// cannot be used. modules are this node's modules that are not core.
func ReadFleetSettings(path string, modules *checksandalerts.ModuleCatalog) *FleetSettings {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil
	}
	var s FleetSettings
	if json.Unmarshal(b, &s) != nil || s.Validate(modules) != nil {
		return nil
	}
	s.RunDestinationModulesAsAJoinedNode()
	return &s
}

// RunDestinationModulesAsAJoinedNode makes a joined node run the hub's
// destination modules as MMWatchdog's own unprivileged account, from /,
// with no added environment variables: the hub's run_as_user_account,
// run_from_directory, and env stay on the hub. Reason: otherwise root on
// the hub could choose the account and the environment of programs that
// run on every joined node. Call it after Validate: the settings' version
// covers what the hub sent.
func (s *FleetSettings) RunDestinationModulesAsAJoinedNode() {
	for _, d := range s.Destinations {
		if call, notCore := d.Inputs.(*checksandalerts.ModuleCall); notCore {
			call.ExecOptions = JoinedNodeDestinationRunSettings()
		}
	}
}

// JoinedNodeDestinationRunSettings are the run settings of every hub
// destination module on a joined node. Tests, which have no account
// mmwatchdog, replace it.
var JoinedNodeDestinationRunSettings = func() primitives.ExecOptions {
	return primitives.ExecOptions{RunAsUserAccount: primitives.UnprivilegedAccount, RunFromDirectory: "/"}
}

// HubSender delivers a joined node's alert to the hub with the credential
// the reporter keeps. Only the reporter joins.
type HubSender struct {
	Code JoinCode
	Path string // the credential file
	Node string
}

func (h HubSender) Send(ctx context.Context, event checksandalerts.Event) error {
	cred := ReadCredential(h.Path)
	if cred == nil || cred.Hub != h.Code.Fingerprint || cred.Node != h.Node {
		return fmt.Errorf("the node has no valid credential for the fleet hub")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := PinnedClient(h.Code.Fingerprint)
	defer client.CloseIdleConnections()
	status, body, e := Post(ctx, client, "https://"+h.Code.Address+"/v1/alerts", cred.Credential, event)
	if e != nil {
		return e
	}
	if status != http.StatusOK {
		return HubError(status, body)
	}
	return nil
}

var Notifying = map[string]bool{"failed": true, "recovered": true, "cannot_check": true, "can_check_again": true, "closed_without_recovery": true, "maintenance_on": true, "maintenance_off": true, "rebooted": true, "notice": true, "reminder": true,
	checksandalerts.FailureIncidentEvents.Raised: true, checksandalerts.CannotCheckIncidentEvents.Raised: true, checksandalerts.ExactFindingChanged: true}

// ValidExactFinding reports an exact finding that a node may send: plain
// text on one line of at most 255 bytes, with its sequence number in
// range. A node is less trusted than the hub.
func ValidExactFinding(e checksandalerts.Event) bool {
	if e.ExactFindingSequence < 0 || e.ExactFindingSequence > MaxSequenceNumber || len(e.ExactFinding) > 255 || !utf8.ValidString(e.ExactFinding) {
		return false
	}
	if e.ExactFinding != "" && e.Finding == "" || e.Type == checksandalerts.ExactFindingChanged && !checksandalerts.HasExactFinding(e) {
		return false
	}
	return !strings.ContainsFunc(e.ExactFinding, unicode.IsControl)
}

// ValidSeverities reports that an alert carries a severity, on a recovery
// or retirement only distinct severities sent for its incident, and as the
// destinations its check names only names a destination can have.
func ValidSeverities(e checksandalerts.Event) bool {
	if e.SendAlertsOnlyToDestinations != nil {
		for _, name := range *e.SendAlertsOnlyToDestinations {
			if !checksandalerts.NamePattern.MatchString(name) {
				return false
			}
		}
	}
	return config.SeverityRank(e.AlertSeverity) >= 0 && config.ValidSeverityList(e.AlertSeveritiesSentForIncident)
}

// ValidMaintenanceCheckGroups reports that a node's alert names check
// groups only as a maintenance alert does: the check groups of one command,
// each a valid name, and the one in check_group when there is only one.
func ValidMaintenanceCheckGroups(event checksandalerts.Event) bool {
	if event.Maintenance == nil {
		return len(event.CheckGroups) == 0
	}
	for _, name := range event.CheckGroups {
		if !checksandalerts.NamePattern.MatchString(name) {
			return false
		}
	}
	return len(event.CheckGroups) != 1 || event.CheckGroups[0] == event.CheckGroup
}

// AlertGroupingState is the alert grouping step's memory on the hub: when each alert grouping key
// last sent a message, and since when each open maintenance group has had no
// node in maintenance. A hub restart forgets it, which can only send a
// message sooner or end a group later. Daemon.mu guards it.
type AlertGroupingState struct {
	Sent  map[string]time.Time
	Quiet map[string]time.Time
	// Window is AlertGroupingWindow. A test makes it shorter before the
	// hub's notifications start.
	Window time.Duration
}

// Plan is the alert grouping step. Rows are the queued and held alerts in order;
// active names the maintenance groups some node reports being in.
//
//   - An alert waits while an earlier alert for the same node and
//     check is queued, so no message passes an earlier one.
//   - Otherwise it goes out unless its AlertGroupKey sent a message in the last
//     AlertGroupingWindow; then it waits for the one combined message sent when that
//     window ends.
//   - A notice goes out at once and alone: it is one person's or one
//     program's message, never part of a combined one.
//   - A maintenance group (reason and user) opens with one message for its
//     first starts. Later starts join the open group quietly, and ends wait
//     in it. The group's end is announced, naming the nodes that ended,
//     once none of its nodes has been in maintenance for groupQuiet.
func (r *AlertGroupingState) Plan(rows []store.IntakeRow, active map[string]bool, now time.Time) store.AlertGroupingPlan {
	var p store.AlertGroupingPlan
	open, members, ended := map[string]bool{}, map[string][]int64{}, map[string][]int64{}
	var queued []store.IntakeRow
	for _, row := range rows {
		if !row.Held {
			queued = append(queued, row)
			continue
		}
		open[row.Group] = true
		members[row.Group] = append(members[row.Group], row.Seq)
		if row.Kind == "maintenance_off" {
			ended[row.Group] = append(ended[row.Group], row.Seq)
		}
	}
	// waiting holds each node and check's queued alerts in order; only
	// the first of them may leave.
	waiting := map[string][]int64{}
	for _, row := range queued {
		k := row.Node + "\x00" + row.Subject
		waiting[k] = append(waiting[k], row.Seq)
	}
	first := func(row store.IntakeRow) bool {
		q := waiting[row.Node+"\x00"+row.Subject]
		return len(q) > 0 && q[0] == row.Seq
	}
	leave := func(row store.IntakeRow) {
		k := row.Node + "\x00" + row.Subject
		waiting[k] = waiting[k][1:]
	}
	collect := func(from int, same func(store.IntakeRow) bool) []int64 {
		seqs := []int64{}
		for _, row := range queued[from:] {
			if same(row) && first(row) {
				leave(row)
				seqs = append(seqs, row.Seq)
			}
		}
		return seqs
	}
	sentNow := map[string]bool{}
	for i, row := range queued {
		if !first(row) {
			continue
		}
		switch row.Kind {
		case "maintenance_off":
			leave(row)
			p.Hold = append(p.Hold, row.Seq)
			open[row.Group] = true
			members[row.Group] = append(members[row.Group], row.Seq)
			ended[row.Group] = append(ended[row.Group], row.Seq)
		case "maintenance_on":
			if open[row.Group] {
				leave(row)
				p.Done = append(p.Done, row.Seq)
				continue
			}
			seqs := collect(i, func(o store.IntakeRow) bool { return o.Kind == row.Kind && o.Group == row.Group })
			p.Messages = append(p.Messages, store.AlertGroupMessage{Members: seqs, Hold: true})
			open[row.Group] = true
			members[row.Group] = append(members[row.Group], seqs...)
		case "notice":
			leave(row)
			p.Messages = append(p.Messages, store.AlertGroupMessage{Members: []int64{row.Seq}})
		default:
			// Grouping by exact finding decides, per destination and key,
			// what such an alert sends; it has no one-minute window.
			if row.ExactFinding {
				leave(row)
				p.Messages = append(p.Messages, store.AlertGroupMessage{Members: []int64{row.Seq}})
				continue
			}
			key := row.AlertGroupKey()
			if sentNow[key] || now.Sub(r.Sent[key]) < r.Window {
				continue
			}
			seqs := collect(i, func(o store.IntakeRow) bool { return o.AlertGroupKey() == key })
			p.Messages = append(p.Messages, store.AlertGroupMessage{Members: seqs})
			p.Sent = append(p.Sent, key)
			sentNow[key] = true
		}
	}
	for _, g := range checksandalerts.Names(open) {
		if active[g] {
			delete(r.Quiet, g)
			continue
		}
		since, ok := r.Quiet[g]
		if !ok {
			r.Quiet[g] = now
			continue
		}
		if now.Sub(since) < groupQuiet {
			continue
		}
		if len(ended[g]) > 0 {
			p.Messages = append(p.Messages, store.AlertGroupMessage{Members: ended[g]})
		}
		p.Done = append(p.Done, members[g]...)
		p.Closed = append(p.Closed, g)
	}
	return p
}

// lateAlertStateWords say, in a late alert, the state of a node (Hub.State)
// or of a check in a node's latest report (incidents.CheckState.Summary).
var lateAlertStateWords = map[string]string{
	"ok":                          "ok",
	"failed":                      "failing",
	"unknown":                     "unknown",
	"confirming_failure":          "failing, not yet alerted",
	"confirming_recovery":         "healthy, but not yet healthy long enough to close the alert",
	"waiting_for_first_heartbeat": "waiting for the first heartbeat",
	"waiting_for_first_report":    "waiting for the first report",
	"reporting":                   "reporting to the hub",
	"silent":                      "not reporting to the hub",
}

// Late adds the present to a check alert the hub delivers more than
// lateAfter after it happened: each named node's current state of that
// check from its latest report, or for a silence alert whether the node
// reports. A maintenance end, which Plan holds for groupQuiet, says when it
// ended and why it comes later. Maintenance starts, reboot alerts, and
// notices name no check.
func (h *Hub) Late(event checksandalerts.Event, now time.Time) checksandalerts.Event {
	if now.Sub(event.At) <= lateAfter {
		return event
	}
	if event.Type == "maintenance_off" {
		ended := "maintenance ended at " + checksandalerts.ClockTime(event.At, now) + ", and alerts resumed then."
		if event.NodeCount > 1 {
			ended = "maintenance ended on the last node in the alert at " + checksandalerts.ClockTime(event.At, now) + ". Alerts on each node resumed when the node's maintenance ended."
		}
		return addToAlert(event, fmt.Sprintf("%s The MMWatchdog fleet hub waits %d minutes after maintenance ends on every affected node before sending the end alert. The wait combines maintenance alerts from a deployment across several nodes into one start alert and one end alert", ended, int(groupQuiet.Minutes())))
	}
	// An update of a grouped key happened when the hub sent it.
	if event.Maintenance != nil || event.Type == "rebooted" || event.Type == "notice" || event.Type == checksandalerts.FindingNodesChanged {
		return event
	}
	silence := event.CheckGroup == checksandalerts.NodeReportsCheckGroup && event.Node == h.Name
	key := event.CheckGroup + "/" + event.Check
	nodes := event.Nodes
	if len(nodes) == 0 {
		nodes = []string{checksandalerts.AlertNode(event, h.Name)}
	}
	h.Mu.Lock()
	states := []string{}
	for _, name := range nodes {
		state := "unknown"
		m := h.Nodes[name]
		switch {
		case m == nil:
		case silence || h.State(m, now) == "silent":
			state = h.State(m, now)
		case m.NodeStatusReport != nil:
			if c, ok := m.NodeStatusReport.Checks[key]; ok {
				state = c.State
				// A unit has its own state while its check reports it as
				// failed. One that the check no longer reports is ok,
				// unless the check itself is unknown.
				if unit, failed := c.Findings[event.Finding]; failed {
					state = unit.State
				} else if event.Finding != "" && c.State != "unknown" {
					state = "ok"
				}
			}
		}
		states = append(states, state)
	}
	h.Mu.Unlock()
	words := func(state string) string {
		if w, known := lateAlertStateWords[state]; known {
			return w
		}
		return state
	}
	var present string
	switch {
	case len(nodes) == 1 && (silence || states[0] == "silent"):
		present = "Now " + nodes[0] + " is " + words(states[0])
	case len(nodes) == 1:
		present = "Now " + nodes[0] + " shows " + event.AboutCheck() + " as " + words(states[0])
	default:
		current := []string{}
		for i, name := range nodes {
			current = append(current, name+" "+words(states[i]))
		}
		present = "Now: " + strings.Join(current, "; ")
	}
	return addToAlert(event, "alert delivered late; the event happened at "+checksandalerts.ClockTime(event.At, now)+". "+present)
}

// addToAlert ends an alert's message, in brackets, and its body, on a line
// of its own, with text. A destination that takes only the title gets no
// message and no body.
func addToAlert(event checksandalerts.Event, text string) checksandalerts.Event {
	if event.Message != "" {
		event.Message += " (" + text + ")"
	}
	if event.Body != "" {
		event.Body += "\n" + text
	}
	return event
}

// MaintenanceGroups names the maintenance groups, reason and user, that some
// node's latest report shows a current window for, even a silent node's.
func (h *Hub) MaintenanceGroups(now time.Time) map[string]bool {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	active := map[string]bool{}
	for _, m := range h.Nodes {
		if m.NodeStatusReport == nil {
			continue
		}
		for _, w := range m.NodeStatusReport.Maintenance {
			if w.Until.After(now) && !w.WithoutMaintenanceOnAndOffAlerts {
				active[w.Reason+"\x00"+w.User] = true
			}
		}
	}
	return active
}
