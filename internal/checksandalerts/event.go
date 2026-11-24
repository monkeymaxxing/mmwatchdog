package checksandalerts

import (
	"strings"
	"time"
)

type Event struct {
	// DeliverOnThisNode keeps a destination-health notice off the fleet hub's
	// queue. It affects queueing only, and is not part of the stored event.
	DeliverOnThisNode bool `json:"-"`

	Evidence *Evidence `json:"evidence,omitempty"`
	// Maintenance is the window a maintenance_on or maintenance_off
	// event describes.
	Maintenance *Window `json:"maintenance,omitempty"`
	ID          string  `json:"id"`
	Incident    string  `json:"incident_id,omitempty"`
	// FleetIncidentKey is the same for every alert about one check, or one
	// finding of a check, on every node of a fleet and on a standalone node:
	// 32 lowercase hexadecimal digits. A destination whose service keeps
	// incidents of its own opens one there per key and closes it with the
	// one closing alert that MMWatchdog sends when the last node recovers.
	FleetIncidentKey string `json:"fleet_incident_key,omitempty"`
	Node             string `json:"node"`
	CheckGroup       string `json:"check_group,omitempty"`
	// CheckGroups are, on a maintenance event, the check groups of its
	// command, or every_check_group alone for the whole node. CheckGroup
	// names the one when there is only one.
	CheckGroups []string `json:"check_groups,omitempty"`
	Check       string   `json:"check"`
	// Finding is, on an event about one finding of a check, the ID of that
	// finding, such as the failed unit of an every_systemd_unit check.
	// Each finding is its own incident, so the finding is part of what the
	// event is about: see AboutCheck.
	Finding string `json:"finding,omitempty"`
	// ExactFinding is, on an alert about a finding whose module names the
	// problem precisely, that exact finding as the node's latest run
	// reported it (Finding.ExactFinding), and ExactFindingSequence counts
	// its changes during the incident, so that the hub ignores an older one
	// that arrives after a newer one. See HasExactFinding.
	ExactFinding         string `json:"exact_finding,omitempty"`
	ExactFindingSequence int64  `json:"exact_finding_sequence,omitempty"`
	Type                 string `json:"type"`
	// AlertSeverity is info, warn, or sos: an incident's current severity,
	// on a recovery or retirement the highest it reached, for a rebooted
	// notice the node's alert_severity_when_this_node_has_rebooted, for a
	// notice sent with mmwatchdog send-notice the severity its command stated, and
	// info for MMWatchdog's maintenance notices.
	AlertSeverity string `json:"alert_severity,omitempty"`
	// AlertSeveritiesSentForIncident are, on a recovery or retirement, the
	// severities the incident's alerts went out with, so the recovery goes
	// where they went.
	AlertSeveritiesSentForIncident []string  `json:"alert_severities_sent_for_incident,omitempty"`
	At                             time.Time `json:"at"`
	// Title is one short line that names the node, the check, and what
	// changed, and Body is everything else a person needs to act. MMWatchdog writes
	// both when the alert is queued (WriteAlertTexts); an event that is
	// only recorded has neither.
	Title             string `json:"title,omitempty"`
	TitleWasShortened bool   `json:"title_was_shortened,omitempty"`
	// TitleWithAlertSeverity is the title with the alert's severity in
	// front, such as "SOS: app-1: myapp/http failed": the line for a person
	// who reads the alert in an email or a chat. See titleWithAlertSeverity.
	TitleWithAlertSeverity string `json:"title_with_alert_severity,omitempty"`
	Body                   string `json:"body,omitempty"`
	// Message explains the observation, such as "expected 200, received
	// 503"; for a notice it is the title text that its sender gave. A
	// destination that takes only the title gets none.
	Message string `json:"message,omitempty"`
	// A hub's combined message names up to 20 of its nodes and counts all.
	Nodes     []string `json:"nodes,omitempty"`
	NodeCount int      `json:"node_count,omitempty"`
	// NodesByExactFinding, RecoveredNodes, and RecoveredNodeCount are, on
	// the hub's finding_nodes_changed update, the nodes that joined or
	// changed by exact finding, and the nodes that are ok again, each named
	// up to 20 and counted.
	NodesByExactFinding []ExactFindingNodes `json:"nodes_by_exact_finding,omitempty"`
	RecoveredNodes      []string            `json:"recovered_nodes,omitempty"`
	RecoveredNodeCount  int                 `json:"recovered_node_count,omitempty"`
	// Destination is the one destination a reminder is for.
	Destination string `json:"destination,omitempty"`
	// SendAlertsOnlyToDestinations are the destinations the alert's check
	// names, when it names its destinations, or the one destination that
	// mmwatchdog send-notice named: the alert goes to exactly those. The hub
	// delivers a joined node's alert by them. See goesTo.
	SendAlertsOnlyToDestinations *[]string `json:"send_alerts_only_to_destinations,omitempty"`
	// NoAlertsOrRecoveryActionsForTheseChecksWhileThisCheckFails are, on an
	// alert that says that a check fails, the checks that MMWatchdog keeps quiet
	// while it does: those that name it in their
	// no_alerts_or_recovery_actions_while_these_checks_fail. The alert
	// names at most quietChecksNamedInAnAlert of them, and
	// NumberOfChecksWithNoAlertsOrRecoveryActionsWhileThisCheckFails counts
	// all. The body of the alert says both. See onecause.go.
	NoAlertsOrRecoveryActionsForTheseChecksWhileThisCheckFails     []string `json:"no_alerts_or_recovery_actions_for_these_checks_while_this_check_fails,omitempty"`
	NumberOfChecksWithNoAlertsOrRecoveryActionsWhileThisCheckFails int      `json:"number_of_checks_with_no_alerts_or_recovery_actions_while_this_check_fails,omitempty"`
	// Seq is the event's position in its database's history, set when read.
	Seq int64 `json:"seq,omitempty"`
	// MMHotlineAlert is what a mmhotline destination's queued delivery sends
	// in place of the event.
	MMHotlineAlert *MMHotlineAlert `json:"mmhotline_alert,omitempty"`
	// MMHotlineAlertSource is, on a queued mmhotline delivery, the alert
	// source that it sends under (MMHotlineDestination.AlertSourceNamed),
	// and on the record of a direct send that a node's report carries, the
	// alert source that the node sent under; "" is the destination's own.
	MMHotlineAlertSource string `json:"mmhotline_alert_source,omitempty"`
	Notify               bool   `json:"-"`
}

// AboutCheck names what a check's alert is about, for people: the check as
// CHECK_GROUP/CHECK, and for a check with one incident for each finding the
// finding after it, as CHECK_GROUP/CHECK: FINDING. Messages, MMHotline
// incident keys, and MMHotline resolutions use it, so that two findings of
// one check, such as two failed systemd units, never share a text or a key.
func AboutCheck(checkGroup, check, systemdUnit string) string {
	if systemdUnit == "" {
		return checkGroup + "/" + check
	}
	return checkGroup + "/" + check + ": " + systemdUnit
}

func (e Event) AboutCheck() string { return AboutCheck(e.CheckGroup, e.Check, e.Finding) }

// ExactFindingNodes are the nodes of one exact finding in a
// finding_nodes_changed update: up to 20 named, and all counted.
type ExactFindingNodes struct {
	ExactFinding string   `json:"exact_finding"`
	Nodes        []string `json:"nodes"`
	NodeCount    int      `json:"node_count"`
}

// ExactFindingChanged is the alert that a finding's open, announced incident
// now has another exact finding than its last alert carried: the problem
// changed, such as the installed version of a flawed package, and the
// incident stays open. It belongs to the incident, and goes where a raise
// of the incident goes.
const ExactFindingChanged = "exact_finding_changed"

// ExactFindingChangedMessage is the message of an ExactFindingChanged alert.
func ExactFindingChangedMessage(before, now string) string {
	if before == "" {
		return "the exact finding is now: " + now
	}
	return "the exact finding changed to: " + now + "; it was: " + before
}

// HasExactFinding reports an alert about a finding that names its exact
// finding: the one test for whether a hub groups the alerts of a fleet
// incident key by exact finding. Every other alert keeps the one-minute
// fleet alert grouping.
func HasExactFinding(e Event) bool { return e.Finding != "" && e.ExactFinding != "" }

// FindingNodesChanged is the hub's update of an exact-finding key: the
// nodes that joined, changed, or recovered since the last update.
const FindingNodesChanged = "finding_nodes_changed"

type TrackEvents struct{ Failed, Recovered, Raised, Stale string }

var (
	// stale is the message of the recovery of an announced incident that
	// resolved while MMWatchdog sent no alerts for the check: in maintenance, or
	// while the check was kept quiet (onecause.go).
	FailureIncidentEvents     = TrackEvents{"failed", "recovered", "failure_alert_severity_raised", "previous incident resolved while MMWatchdog sent no alerts for this check"}
	CannotCheckIncidentEvents = TrackEvents{"cannot_check", "can_check_again", "cannot_check_alert_severity_raised", "previous cannot-check incident resolved while MMWatchdog sent no alerts for this check"}
)

// CannotCheckReminderStart is how the message of a reminder of a
// cannot-check incident starts, a node's own and a hub's grouped one:
// "MMWatchdog has not been able to run GROUP/CHECK ". MMWatchdog
// recognizes such a reminder by these words (ReminderIsAboutCannotCheck).
func CannotCheckReminderStart(about string) string {
	return "MMWatchdog has not been able to run " + about + " "
}

// ReminderMessage is the message of a reminder of an open incident about
// the check named about: what is still wrong, since when the alert has been
// open, and the latest result of the check.
func ReminderMessage(kinds TrackEvents, about, opened, latest string) string {
	if kinds == CannotCheckIncidentEvents {
		return CannotCheckReminderStart(about) + "since " + opened + "; latest result: " + latest
	}
	return about + " still failing since " + opened + "; latest result: " + latest
}

// ReminderIsAboutCannotCheck reports a reminder of a cannot-check
// incident. The event carries no field for the kind of its incident, so
// MMWatchdog tells it by the words that it writes at the start of every
// such reminder.
func ReminderIsAboutCannotCheck(e Event) bool {
	return e.Type == "reminder" && strings.HasPrefix(e.Message, CannotCheckReminderStart(e.AboutCheck()))
}

// Window is a maintenance window: when it ends, why, who started it, and
// whether its start and end are kept in history without alert delivery.
type Window struct {
	Until                            time.Time `json:"until"`
	Reason                           string    `json:"reason,omitempty"`
	User                             string    `json:"user,omitempty"`
	WithoutMaintenanceOnAndOffAlerts bool      `json:"without_maintenance_on_and_off_alerts"`
}

// Describe explains the window for status and alert messages.
func (w Window) Describe() string {
	s := "until " + w.Until.UTC().Format("2006-01-02 15:04 UTC")
	if w.User != "" {
		s += ", turned on by " + w.User
	}
	if w.Reason != "" {
		s += ": " + w.Reason
	}
	if w.WithoutMaintenanceOnAndOffAlerts {
		s += "; maintenance start and end are not announced"
	}
	return s
}

// NamedDestinations keeps every configured name. An explicitly empty list
// sends nothing; a nonempty list with no known name falls back to severity.
func (e Event) NamedDestinations(destinations map[string]Destination) ([]string, bool) {
	if e.SendAlertsOnlyToDestinations == nil {
		return nil, false
	}
	named := []string{}
	for _, name := range *e.SendAlertsOnlyToDestinations {
		if _, exists := destinations[name]; exists {
			named = append(named, name)
		}
	}
	return named, len(named) > 0 || len(*e.SendAlertsOnlyToDestinations) == 0
}

// RoutingGroup is what fleet alert grouping keeps apart: alerts of
// different severities, recoveries whose incidents went out with different
// severities, and alerts of checks that name different destinations never
// share a message.
func (e Event) RoutingGroup() string {
	group := e.AlertSeverity + "/" + strings.Join(e.AlertSeveritiesSentForIncident, ",")
	if e.SendAlertsOnlyToDestinations != nil {
		group += "/only:" + strings.Join(*e.SendAlertsOnlyToDestinations, ",")
	}
	return group
}
