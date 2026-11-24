package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/primitives"
)

// MMHotline's limits for a generic alert.
const MMHotlineTitleLimit = 256 // characters

const mmhotlineMessageLimit = 16 * 1024

const mmhotlineBodyLimit = 64 * 1024

// SentBy is the line that ends every MMHotline alert: who sent it.
func (f QueuedBy) SentBy() string {
	// StandaloneSender uses the standalone message after the switch; hub and fallback senders override it.
	//exhaustive:ignore
	switch f.Role {
	case FleetHubSender:
		return "Sent by the MMWatchdog fleet hub on " + f.Node + "."
	case FallbackSender:
		return "Sent directly by the MMWatchdog node " + f.Node + " because the MMWatchdog fleet hub did not accept the alert in time."
	}
	return "Sent by the standalone MMWatchdog node " + f.Node + "."
}

// QueueMMHotline queues MMHotline's alerts for one alert message: the
// message, standing for members, becomes one alert, or for a recovery the
// one resolution of each incident key it empties, or nothing. Reminders are
// never sent. It keeps the destination's fleet incident set in transaction
// tx (fleetincidentset.go). A failure or raise whose MMWatchdog incident the set
// already knows is over is not sent, because its resolution has gone or
// will go without it. A failure or raise carries its key's severity: the
// highest of the key's open incidents. A key that was sent at sos resolves
// its SOS as soon as no open incident of the key is sos.
func QueueMMHotline(tx *sql.Tx, at int64, name string, dest checksandalerts.Destination, message checksandalerts.Event, members []checksandalerts.Event, from QueuedBy, now time.Time) error {
	set := IncidentSet{Destination: name, AlertSource: dest.MMHotline().AlertSourceID}
	var alerts []checksandalerts.Event
	changed := map[string]string{}
	switch message.Type {
	case "reminder":
		return nil
	case "failed", "cannot_check", checksandalerts.FailureIncidentEvents.Raised, checksandalerts.CannotCheckIncidentEvents.Raised:
		if from.Role == FallbackSender {
			// Reports carry it until the hub has heard of it, so the hub can
			// resolve it even if it recovers before the hub returns.
			if e := recordSentWithoutTheHub(tx, name, members, now); e != nil {
				return e
			}
			alerts = []checksandalerts.Event{MMHotlineFailure(message, members, from, message.AlertSeverity)}
			break
		}
		kept, over, e := set.openIncidents(tx, members, from.hub(), now)
		if e != nil {
			return e
		}
		changed = over
		if len(kept) > 0 {
			key := checksandalerts.ReadableFleetIncidentKey(kept[0], checksandalerts.IncidentKind(kept[0].Type), from.hub())
			open, e := set.keyRows(tx, key, FleetIncidentOpen)
			if e != nil {
				return e
			}
			if e = set.recordSent(tx, key, open.highest); e != nil {
				return e
			}
			alerts = []checksandalerts.Event{MMHotlineFailure(message, kept, from, open.highest)}
		}
	case checksandalerts.ExactFindingChanged, checksandalerts.FindingNodesChanged:
		// On the hub, grouping by exact finding queues these
		// (queueGrouped); a standalone node and a joined node's fallback send
		// a changed exact finding at its incident's severity, with its key.
		if from.Role == FallbackSender {
			if e := recordSentWithoutTheHub(tx, name, members, now); e != nil {
				return e
			}
		}
		alerts = []checksandalerts.Event{mmhotlineGroupedAlert(message, from)}
	case "recovered", "can_check_again", "closed_without_recovery":
		if from.Role == FallbackSender {
			return nil
		}
		var e error
		if changed, _, e = set.closeIncidents(tx, members, message.Type, now); e != nil {
			return e
		}
	default:
		alerts = []checksandalerts.Event{MMHotlineNotice(message, from)}
	}
	if e := queueAlerts(tx, at, name, dest, alerts); e != nil || len(changed) == 0 || from.AwaitingFirstReports {
		return e
	}
	waiting, e := waitingFailureKeys(tx, from.hub(), members)
	if e != nil {
		return e
	}
	closings, e := set.closeKeys(tx, changed, waiting, true, now)
	if e != nil {
		return e
	}
	return set.queueClosings(tx, at, name, dest, closings, nil, message, from, now)
}

// queueAlerts queues MMHotline alerts for the mmhotline destination.
func queueAlerts(tx *sql.Tx, at int64, name string, dest checksandalerts.Destination, alerts []checksandalerts.Event) error {
	target, _ := json.Marshal(destinationForStorage(dest))
	for _, a := range alerts {
		if e := insertOutbox(tx, at, name, a, target, ""); e != nil {
			return e
		}
	}
	return nil
}

// mmhotlineAlert is the MMHotline alert for a key of the mmhotline
// destination's set that closes: resolved-sos, naming the nodes still open
// at a lower severity, after which the key is a key of that severity; or,
// once all its incidents have closed, one resolution at the highest
// severity sent since the key's last resolution. cause carries MMWatchdog's message
// and time, when an alert closed the key.
func (c keyClosing) mmhotlineAlert(cause checksandalerts.Event, from QueuedBy, now time.Time) checksandalerts.Event {
	if c.stillOpen != nil {
		return mmhotlineSosResolution(c.key, c.stillOpen.about, c.stillOpen.nodes, c.stillOpen.highest, cause, from, now)
	}
	return mmhotlineResolution(c.key, c.closed.about, c.closed.nodes, c.change, config.HighestSeverity(c.sent), c.othersStillOpen, cause, from, now)
}

// MMHotlineFailure is the alert for a failure or cannot-check message, or
// for its raise, which MMHotline receives as a new alert for the same key.
// It carries the message's title and body. members are the alerts of the
// message whose incidents are still open; when some are over already, the
// title and the body count only these.
func MMHotlineFailure(message checksandalerts.Event, members []checksandalerts.Event, from QueuedBy, severity string) checksandalerts.Event {
	shown := message
	if len(members) != max(message.NodeCount, 1) || message.Title == "" {
		shown = Combine(members, from.hub())
		shown.WriteAlertTexts(members, from.hub())
	}
	key := checksandalerts.ReadableFleetIncidentKey(members[0], checksandalerts.IncidentKind(members[0].Type), from.hub())
	return MMHotlineEvent(message, message.ID, checksandalerts.ScopeFleetIncidentKey(checksandalerts.FleetIncidentKey(key), from.Node, from.FleetIdentity), severity, shown.Title, []string{shown.Body, from.SentBy()})
}

// mmhotlineGroupedAlert is MMHotline's alert for an alert of a key that
// grouping by exact finding handles: a node's failure, raise, or changed
// exact finding, or the hub's finding_nodes_changed update or reminder. It
// goes at the alert's own severity, never promoted, with the alert's key,
// title, and body. (The MMHotline wire mapping of these alerts, such as
// their first line, is completed with the MMHotline lane.)
func mmhotlineGroupedAlert(e checksandalerts.Event, from QueuedBy) checksandalerts.Event {
	if e.Title == "" {
		e.WriteAlertTexts([]checksandalerts.Event{e}, from.hub())
	}
	return MMHotlineEvent(e, e.ID, e.FleetIncidentKey, e.AlertSeverity, e.Title, []string{e.Body, from.SentBy()})
}

// MMHotlineNotice is the alert for a maintenance notice, a reboot notice, or
// a notice sent with mmwatchdog send-notice, with the message's title and body. A
// maintenance notice is info; a reboot notice has the severity its node
// states in alert_severity_when_this_node_has_rebooted, and a sent notice
// the severity its command stated.
func MMHotlineNotice(message checksandalerts.Event, from QueuedBy) checksandalerts.Event {
	severity := "info"
	if (message.Type == "rebooted" || message.Type == "notice") && config.SeverityRank(message.AlertSeverity) >= 0 {
		severity = message.AlertSeverity
	}
	if message.Title == "" {
		message.WriteAlertTexts([]checksandalerts.Event{message}, from.hub())
	}
	return MMHotlineEvent(message, message.ID, "", severity, message.Title, []string{message.Body, from.SentBy()})
}

// mmhotlineResolution is the one alert that resolves a key once no node has
// its incident open. about holds the check group, the check, and the
// finding of the key's incidents, kind is the change that closed the last
// one, and severity the highest the key reached. It has the words of the
// alert that closes a key for every other destination
// (checksandalerts.FleetIncidentClosingTitle): what was open across the
// fleet, or the node's own words when the incidents of one node closed, as
// always on a standalone node, whose set holds its own incidents only.
func mmhotlineResolution(key string, about checksandalerts.Event, nodes []string, kind, severity string, othersStillOpen bool, cause checksandalerts.Event, from QueuedBy, now time.Time) checksandalerts.Event {
	parts, was := closingTitle(key, about, nodes, 0, kind, othersStillOpen)
	title := checksandalerts.TitleText(parts)
	summary := title
	if was != "" {
		summary += " (" + was + ")"
	}
	lines := []string{summary + "."}
	at := now
	if cause.ID != "" {
		at = cause.At
		lines = append(lines, "Latest: "+cause.Message)
	}
	resolution := checksandalerts.Event{ID: incidents.ID(), Node: from.Node, CheckGroup: about.CheckGroup, Type: kind, At: at}
	return MMHotlineEvent(resolution, resolution.ID, checksandalerts.ScopeFleetIncidentKey(checksandalerts.FleetIncidentKey(key), from.Node, from.FleetIdentity), "resolved-"+severity, title, append(lines, from.SentBy()))
}

// mmhotlineSosResolution is the resolved-sos of a fleet key that has no
// sos node any more while nodes at a lower severity keep it open. about
// holds the check group, the check, and the finding of the key's
// incidents.
func mmhotlineSosResolution(key string, about checksandalerts.Event, still []string, severity string, cause checksandalerts.Event, from QueuedBy, now time.Time) checksandalerts.Event {
	title := about.AboutCheck() + " is no longer SOS on any node"
	switch {
	case key == checksandalerts.SilenceIncidentKey:
		title = "No node that stopped reporting to the MMWatchdog fleet hub is SOS any more"
	case about.CheckGroup == checksandalerts.NodeReportsCheckGroup && about.Check == checksandalerts.HubCheck:
		title = "No node that cannot reach the MMWatchdog fleet hub is SOS any more"
	case checksandalerts.ReadableKeyIsAboutCannotCheck(key):
		title = "The alert that MMWatchdog cannot check " + about.AboutCheck() + " is no longer SOS on any node"
	}
	group := about.CheckGroup
	lines := []string{title + "; still " + checksandalerts.AlertSeverityLabel(severity) + " on " + strings.Join(still, ", ") + "."}
	at := now
	if cause.ID != "" {
		at = cause.At
		lines = append(lines, "Latest: "+cause.Message)
	}
	resolution := checksandalerts.Event{ID: incidents.ID(), Node: from.Node, CheckGroup: group, Type: "recovered", At: at}
	return MMHotlineEvent(resolution, resolution.ID, checksandalerts.ScopeFleetIncidentKey(checksandalerts.FleetIncidentKey(key), from.Node, from.FleetIdentity), "resolved-sos", title, append(lines, from.SentBy()))
}

// MMHotlineEvent is the queued delivery: the MMWatchdog message it stands for, with
// the MMHotline alert it sends, fitted to MMHotline's limits.
func MMHotlineEvent(message checksandalerts.Event, id, key, severity, title string, lines []string) checksandalerts.Event {
	queued := checksandalerts.Event{ID: id, Node: message.Node, CheckGroup: message.CheckGroup, Check: message.Check, Type: message.Type, At: message.At, Message: primitives.Clip(title, 1024)}
	if key != "" {
		queued.FleetIncidentKey = key
	}
	body := strings.Join(checksandalerts.NonEmpty(lines), "\n")
	a := &checksandalerts.MMHotlineAlert{SenderAlertID: id, SenderIncidentKey: key, Severity: severity, Title: checksandalerts.FitCharacters(title, MMHotlineTitleLimit), Message: primitives.Clip(body, mmhotlineMessageLimit)}
	if !message.At.IsZero() {
		a.HappenedAt = message.At.UTC().Format(time.RFC3339)
	}
	// JSON escaping can grow a message several times over; a body MMHotline
	// refuses as too large would block every alert queued behind it.
	for len(a.Encode()) > mmhotlineBodyLimit {
		a.Message = primitives.Clip(a.Message, len(a.Message)/2)
	}
	queued.MMHotlineAlert = a
	return queued
}

// Head returns the next attempt and the latest error of a destination's
// oldest queued delivery.
func (s *Store) Head(destination string) (time.Time, string, error) {
	var next int64
	var lastError string
	e := s.DB.QueryRow("SELECT next, error FROM outbox WHERE destination=? ORDER BY seq LIMIT 1", destination).Scan(&next, &lastError)
	if e == sql.ErrNoRows {
		return time.Time{}, "", nil
	}
	return time.Unix(0, next), lastError, e
}
