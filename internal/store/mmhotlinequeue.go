package store

import (
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
// its SOS as soon as no open incident of the key is sos. A joined node's
// fallback sends under the destination's alert source for nodes sending
// without the hub, when the destination has one, and records what it sent
// for its reports.
func QueueMMHotline(tx *sql.Tx, at int64, name string, dest checksandalerts.Destination, message checksandalerts.Event, members []checksandalerts.Event, from QueuedBy, now time.Time) error {
	set := IncidentSet{Destination: name, AlertSource: dest.MMHotline().AlertSourceID}
	var alerts []checksandalerts.Event
	// key is the readable key of an alert of an incident that the hub or a
	// standalone node sends.
	key := ""
	changed := map[string]string{}
	switch message.Type {
	case "reminder":
		return nil
	case "failed", "cannot_check", checksandalerts.FailureIncidentEvents.Raised, checksandalerts.CannotCheckIncidentEvents.Raised:
		if from.Role == FallbackSender {
			return queueWithoutTheHub(tx, at, name, dest, MMHotlineFailure(message, members, from, message.AlertSeverity), members, now)
		}
		kept, over, e := set.openIncidents(tx, members, from.hub(), now)
		if e != nil {
			return e
		}
		changed = over
		if len(kept) > 0 {
			key = checksandalerts.ReadableFleetIncidentKey(kept[0], checksandalerts.IncidentKind(kept[0].Type), from.hub())
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
			return queueWithoutTheHub(tx, at, name, dest, mmhotlineGroupedAlert(message, from), members, now)
		}
		key = checksandalerts.ReadableFleetIncidentKey(message, "failed", from.hub())
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
		notice := MMHotlineNotice(message, from)
		if from.Role == FallbackSender {
			notice.MMHotlineAlertSource = dest.MMHotline().NodeAlertSourceID()
		}
		alerts = []checksandalerts.Event{notice}
	}
	if e := set.queueMMHotlineAlerts(tx, at, name, dest, key, alerts, sendsMore); e != nil || len(changed) == 0 || from.AwaitingFirstReports {
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

// queueWithoutTheHub queues a joined node's direct alert, of the incidents
// of members, under the destination's alert source for nodes sending
// without the hub when it has one. Once MMHotline accepts it, it is recorded
// with that source for the node's reports, which carry it until the hub has
// heard of it: so the hub can resolve it even if it recovers before the hub
// returns.
func queueWithoutTheHub(tx *sql.Tx, at int64, name string, dest checksandalerts.Destination, alert checksandalerts.Event, members []checksandalerts.Event, now time.Time) error {
	alert.MMHotlineAlertSource = dest.MMHotline().NodeAlertSourceID()
	if e := queueAlerts(tx, at, name, dest, []checksandalerts.Event{alert}); e != nil {
		return e
	}
	return recordOnAcceptance(tx, members, alert.MMHotlineAlertSource)
}

// What an alert of a key does to the key's incident in MMHotline, for the
// choice of its alert source (queueMMHotlineAlerts).
type mmhotlineKeyStep int

const (
	// sendsMore: a failure, a raise, an update, or a new exact finding.
	sendsMore mmhotlineKeyStep = iota
	// resolvesSos: the key's resolved-sos, while the key stays open below sos.
	resolvesSos
	// closesKey: the key's one resolution.
	closesKey
)

// queueMMHotlineAlerts queues MMHotline alerts of the readable key (""
// for alerts without one) for the mmhotline destination name, each under
// the alert source of the key's incident in MMHotline. MMHotline matches an
// incident by alert source and key, and a joined node sending without the
// hub may have opened it under the destination's alert source for those
// nodes. So once a settled direct send says so (recordMMHotlineSource),
// every further alert of the key goes under that source, or a hub's sos
// would open a second incident and wake someone again. A resolution goes
// under each alert source that got alerts of the key since its last
// resolution, each with its own sender_alert_id, so none is left open; the
// key's one resolution then starts the key over.
func (set IncidentSet) queueMMHotlineAlerts(tx *sql.Tx, at int64, name string, dest checksandalerts.Destination, key string, alerts []checksandalerts.Event, step mmhotlineKeyStep) error {
	if key == "" || len(alerts) == 0 {
		return queueAlerts(tx, at, name, dest, alerts)
	}
	sent, e := set.mmhotlineSourcesSent(tx, dest, key)
	if e != nil {
		return e
	}
	queued := []checksandalerts.Event{}
	if step == sendsMore {
		for _, a := range alerts {
			if a, e = set.underTheKeysAlertSource(tx, dest, key, a); e != nil {
				return e
			}
			queued = append(queued, a)
		}
	} else {
		if len(sent) == 0 {
			sent = []string{set.AlertSource}
		}
		for i, source := range sent {
			if source == set.AlertSource {
				source = ""
			}
			for _, a := range alerts {
				if i > 0 {
					a = withOwnSenderAlertID(a)
				}
				a.MMHotlineAlertSource = source
				queued = append(queued, a)
			}
		}
		if step == closesKey {
			if _, e = tx.Exec("UPDATE fleet_incident_keys SET mmhotline_alert_sources_sent='' WHERE "+inSet+" AND incident_key=?", set.with(key)...); e != nil {
				return e
			}
		}
	}
	return queueAlerts(tx, at, name, dest, queued)
}

// underTheKeysAlertSource is alert, a further alert of the key that is
// not its resolution, under the alert source of the key's incident in
// MMHotline: the alert source for nodes sending without the hub once a
// node's direct send under it was settled, until the key's resolution;
// otherwise the destination's own. It records the source.
func (set IncidentSet) underTheKeysAlertSource(tx *sql.Tx, dest checksandalerts.Destination, key string, alert checksandalerts.Event) (checksandalerts.Event, error) {
	sent, e := set.mmhotlineSourcesSent(tx, dest, key)
	if e != nil {
		return alert, e
	}
	alert.MMHotlineAlertSource = ""
	if node := dest.MMHotline().NodeAlertSourceID(); node != "" && slices.Contains(sent, node) {
		alert.MMHotlineAlertSource = node
	}
	return alert, set.recordMMHotlineSource(tx, dest, key, alert.MMHotlineAlertSource)
}

// mmhotlineSourcesSent lists the alert sources of the destination that got
// alerts of the key since its last resolution, the destination's own
// first; a source that the destination no longer has is left out.
func (set IncidentSet) mmhotlineSourcesSent(tx *sql.Tx, dest checksandalerts.Destination, key string) ([]string, error) {
	var written string
	e := tx.QueryRow("SELECT mmhotline_alert_sources_sent FROM fleet_incident_keys WHERE "+inSet+" AND incident_key=?", set.with(key)...).Scan(&written)
	if e != nil && e != sql.ErrNoRows {
		return nil, e
	}
	sent := []string{}
	for _, source := range []string{set.AlertSource, dest.MMHotline().NodeAlertSourceID()} {
		if source != "" && slices.Contains(strings.Split(written, ","), source) {
			sent = append(sent, source)
		}
	}
	return sent, nil
}

// recordMMHotlineSource records that the key got an alert under the alert
// source source ("" for the destination's own) since its last resolution.
// A source that the destination does not have, such as one that a node's
// report names, is not recorded.
func (set IncidentSet) recordMMHotlineSource(tx *sql.Tx, dest checksandalerts.Destination, key, source string) error {
	if source == "" {
		source = set.AlertSource
	}
	if p := dest.MMHotline(); p == nil || source != set.AlertSource && source != p.NodeAlertSourceID() {
		return nil
	}
	var written string
	e := tx.QueryRow("SELECT mmhotline_alert_sources_sent FROM fleet_incident_keys WHERE "+inSet+" AND incident_key=?", set.with(key)...).Scan(&written)
	if e != nil && e != sql.ErrNoRows {
		return e
	}
	sources := checksandalerts.NonEmpty(strings.Split(written, ","))
	if slices.Contains(sources, source) {
		return nil
	}
	_, e = tx.Exec("INSERT INTO fleet_incident_keys(destination,alert_source,incident_key,alert_severities_sent,last_closing_alert_at,mmhotline_alert_sources_sent) VALUES(?,?,?,'',0,?) ON CONFLICT(destination,alert_source,incident_key) DO UPDATE SET mmhotline_alert_sources_sent=excluded.mmhotline_alert_sources_sent",
		set.with(key, strings.Join(append(sources, source), ","))...)
	return e
}

// withOwnSenderAlertID is a copy of a queued MMHotline alert for another
// alert source, with an ID of its own.
func withOwnSenderAlertID(a checksandalerts.Event) checksandalerts.Event {
	alert := *a.MMHotlineAlert
	a.ID = incidents.ID()
	alert.SenderAlertID = a.ID
	a.MMHotlineAlert = &alert
	return a
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
	return MMHotlineEvent(message, message.ID, checksandalerts.ScopeFleetIncidentKey(checksandalerts.FleetIncidentKey(key), from.Node, from.FleetIdentity), severity, shown.Title, []string{withProblemFirst(shown.Body, members[0], len(members) == 1, from.hub()), from.SentBy()})
}

// mmhotlineGroupedAlert is MMHotline's alert for an alert of a key that
// grouping by exact finding handles: a node's failure, raise, or changed
// exact finding, or the hub's finding_nodes_changed update. It goes at the
// alert's own severity, never promoted, with the alert's key, its own
// sender_alert_id, title, and body. A node's alert names the problem on
// its first line (withProblemFirst); an update's first line is its summary,
// which names no node.
func mmhotlineGroupedAlert(e checksandalerts.Event, from QueuedBy) checksandalerts.Event {
	if e.Title == "" {
		e.WriteAlertTexts([]checksandalerts.Event{e}, from.hub())
	}
	body := e.Body
	if e.Type != checksandalerts.FindingNodesChanged {
		body = withProblemFirst(body, e, true, from.hub())
	}
	return MMHotlineEvent(e, e.ID, e.FleetIncidentKey, e.AlertSeverity, e.Title, []string{body, from.SentBy()})
}

// withProblemFirst is the body of an alert to MMHotline about the failure
// of node alert m, whose first line names the problem in the same words on
// every node: m's exact finding when it has one, otherwise the first line
// of m's own failure message with the node's name taken out. MMHotline
// merges repeated warnings of the alert source for nodes sending without
// the hub by key and first line, so a node's name there would keep repeats
// apart, and different causes have different first lines. The node's name
// stays in the title and in the lines below. A body of one node (single)
// that starts with m's message starts with that line in place of the
// message's first line instead.
func withProblemFirst(body string, m checksandalerts.Event, single bool, hub string) string {
	message, _, _ := strings.Cut(primitives.AsReadableLines(m.Message), "\n")
	first := m.ExactFinding
	if !checksandalerts.HasExactFinding(m) {
		first = withoutNodeNames(message, checksandalerts.AlertNode(m, hub), m.Node)
		if first == "" {
			first = m.AboutCheck()
		}
	}
	head, rest, more := strings.Cut(body, "\n")
	switch {
	case head == first:
		return body
	case single && head == message && !checksandalerts.HasExactFinding(m):
		if !more {
			return first
		}
		return first + "\n" + rest
	}
	return strings.Join(checksandalerts.NonEmpty([]string{first, body}), "\n")
}

// withoutNodeNames is line with each of names taken out where it stands as
// a name of its own, not inside a longer name such as a host name, and the
// spaces and separators left behind tidied.
func withoutNodeNames(line string, names ...string) string {
	nameCharacter := func(r rune) bool {
		return r == '-' || r == '_' || r == '.' || unicode.IsLetter(r) || unicode.IsDigit(r)
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		var b strings.Builder
		for start := 0; ; {
			i := strings.Index(line[start:], name)
			if i < 0 {
				b.WriteString(line[start:])
				break
			}
			i += start
			end := i + len(name)
			before, _ := utf8.DecodeLastRuneInString(line[:i])
			after, _ := utf8.DecodeRuneInString(line[end:])
			// A dot that ends the line ends the sentence, not the name.
			alone := (i == 0 || !nameCharacter(before)) && (end == len(line) || !nameCharacter(after) || after == '.' && end+1 == len(line))
			if alone {
				b.WriteString(line[start:i])
			} else {
				b.WriteString(line[start:end])
			}
			start = end
		}
		line = b.String()
	}
	line = strings.Join(strings.Fields(line), " ")
	line = strings.TrimLeft(line, ":;, ")
	return strings.ReplaceAll(strings.ReplaceAll(line, " :", ":"), " ,", ",")
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
