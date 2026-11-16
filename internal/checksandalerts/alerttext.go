package checksandalerts

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"mmwatchdog/internal/primitives"
)

// The title and the body of an alert. MMWatchdog writes both once, when the alert is
// queued for its destinations, and every destination gets both. The rules
// are in docs/changing-mmwatchdog/design/alerts.md under "Title and body".

// AlertTitleLimit is the most characters a title has: a title must fit an
// SMS and a phone call, with the words a service adds to it.
const AlertTitleLimit = 120

// ShortestCutName is the fewest characters MMWatchdog leaves of a name that it cuts
// in a title, with the mark of the cut.
const ShortestCutName = 12

// TitlePart is a piece of a title: fixed words, or the name of a node, a
// check group, or a check, which MMWatchdog may cut when the title is too long.
type TitlePart struct {
	Text string
	name bool
}

// WriteAlertTexts gives an alert its title and its body. members are the
// alerts the message stands for: itself, or for a hub's grouped message the
// alert of each node. hub is the hub's own name on the hub, and "" elsewhere.
func (e *Event) WriteAlertTexts(members []Event, hub string) {
	e.Title, e.TitleWasShortened = FitTitle(AlertTitleParts(*e, hub))
	e.TitleWithAlertSeverity = titleWithAlertSeverity(*e)
	e.Body = primitives.AsReadableLines(AlertBody(*e, members, hub))
}

// The labels that say how urgent an alert is, in front of its title. They
// are the labels that MMHotline shows for an alert, so an alert reads the
// same in an email, in a chat, and in MMHotline. An incident that closed
// without recovery is "Closed", not "Resolved": MMWatchdog does not know
// whether its problem is fixed.
var (
	alertSeverityLabels         = map[string]string{"info": "Info", "warn": "Warning", "sos": "SOS"}
	resolvedAlertSeverityLabels = map[string]string{"info": "Resolved info", "warn": "Resolved warning", "sos": "Resolved SOS"}
	closedAlertSeverityLabels   = map[string]string{"info": "Closed info", "warn": "Closed warning", "sos": "Closed SOS"}
)

// AlertSeverityLabel is the word for a severity in a text that people
// read, such as "SOS" for sos and "Warning" for warn.
func AlertSeverityLabel(severity string) string {
	if label, known := alertSeverityLabels[severity]; known {
		return label
	}
	return severity
}

// titleWithAlertSeverity is an alert's title with its severity in front,
// such as "SOS: app-1: myapp/http failed". An alert that closes an incident
// says "Resolved" (recovered, can_check_again) or "Closed"
// (closed_without_recovery) and the highest severity that the incident
// reached, such as "Resolved SOS: app-1: myapp/http is ok again". The label
// is not counted in AlertTitleLimit.
func titleWithAlertSeverity(e Event) string {
	labels := alertSeverityLabels
	switch {
	case e.Type == "closed_without_recovery":
		labels = closedAlertSeverityLabels
	case Closes(e.Type):
		labels = resolvedAlertSeverityLabels
	}
	label, known := labels[e.AlertSeverity]
	if !known {
		return e.Title
	}
	return label + ": " + e.Title
}

// WriteTitleWithAlertSeverity writes the title with the alert's severity in
// front again, after its severity changed: for the node's own closing alert
// that a destination with a fleet incident set gets with the severities
// that it got for the incident's key.
func (e *Event) WriteTitleWithAlertSeverity() {
	if e.Title != "" {
		e.TitleWithAlertSeverity = titleWithAlertSeverity(*e)
	}
}

// OnlyTheTitle is the alert for a destination that states
// send_only_the_alert_title: the title, with and without the severity in
// front, and the fields that identify the alert, with no body, message, or
// evidence, and without the checks that the alert's check keeps quiet.
func (e Event) OnlyTheTitle() Event {
	e.Body, e.Message, e.Evidence = "", "", nil
	e.NoAlertsOrRecoveryActionsForTheseChecksWhileThisCheckFails, e.NumberOfChecksWithNoAlertsOrRecoveryActionsWhileThisCheckFails = nil, 0
	return e
}

// aboutCannotCheck reports an alert about a cannot-check incident. A
// reminder says which incident it is about only in its message
// (ReminderIsAboutCannotCheck).
func aboutCannotCheck(e Event) bool {
	if e.Type == "reminder" {
		return ReminderIsAboutCannotCheck(e)
	}
	return IncidentKind(e.Type) == "cannot_check"
}

// AlertTitleParts is the title of an alert before it is fitted: one short
// line that names the node, the check group and the check, and what
// changed. A hub's grouped message counts its nodes in place of a name.
func AlertTitleParts(e Event, hub string) []TitlePart {
	words := func(text string) TitlePart { return TitlePart{Text: text} }
	name := func(text string) TitlePart { return TitlePart{Text: text, name: true} }
	count := max(e.NodeCount, 1)
	several := count > 1
	nodes := fmt.Sprintf("%d nodes", count)
	raised := ""
	if e.Type == FailureIncidentEvents.Raised || e.Type == CannotCheckIncidentEvents.Raised {
		raised = ", now " + AlertSeverityLabel(e.AlertSeverity)
	}
	// who names the node, or counts the nodes, at the start of a title
	// about the node itself.
	who := func(node string) TitlePart {
		if several {
			return words(nodes)
		}
		return name(node)
	}
	// plural picks the verb form for one node or for several.
	plural := func(one, more string) string {
		if several {
			return more
		}
		return one
	}
	switch {
	case e.Maintenance != nil:
		change := "maintenance on for "
		if e.Type == "maintenance_off" {
			change = "maintenance off for "
		}
		if several {
			return []TitlePart{words(change + nodes)}
		}
		parts := []TitlePart{name(e.Node), words(": " + change)}
		checkGroups := e.CheckGroups
		if len(checkGroups) == 0 && e.CheckGroup != "" {
			checkGroups = []string{e.CheckGroup}
		}
		switch {
		case len(checkGroups) == 1 && checkGroups[0] == EveryCheckGroup:
			return append(parts, words("the whole node"))
		case len(checkGroups) > 3:
			return append(parts, words(fmt.Sprintf("%d check groups", len(checkGroups))))
		}
		for i, checkGroup := range checkGroups {
			if i > 0 {
				parts = append(parts, words(", "))
			}
			parts = append(parts, name(checkGroup))
		}
		return parts
	case e.Type == "rebooted":
		return []TitlePart{who(e.Node), words(" rebooted")}
	case e.Type == "notice":
		return []TitlePart{name(e.Node), words(": " + e.Message)}
	}
	// The hub's alert about a silent node: the check is named after the node.
	silence := e.CheckGroup == NodeReportsCheckGroup && hub != "" && e.Node == hub
	outageAlerter := e.CheckGroup == NodeReportsCheckGroup && e.Check == HubCheck && !silence
	// still is an alert that says that the problem goes on: a raise, or a
	// reminder of a failure.
	still := e.Type == FailureIncidentEvents.Raised || e.Type == "reminder" && !aboutCannotCheck(e)
	switch {
	case silence && e.Type == "failed":
		return []TitlePart{who(e.Check), words(" stopped reporting to the MMWatchdog fleet hub")}
	case silence && still:
		return []TitlePart{who(e.Check), words(" still not reporting to the MMWatchdog fleet hub" + raised)}
	case silence && e.Type == "recovered":
		return []TitlePart{who(e.Check), words(plural(" reports", " report") + " to the MMWatchdog fleet hub again")}
	case silence && e.Type == "closed_without_recovery":
		return []TitlePart{who(e.Check), words(" stopped reporting to the MMWatchdog fleet hub; " + plural("alert", "alerts") + " closed, maybe not fixed")}
	case outageAlerter && e.Type == "failed":
		return []TitlePart{who(e.Node), words(" cannot reach the MMWatchdog fleet hub")}
	case outageAlerter && still:
		return []TitlePart{who(e.Node), words(" still cannot reach the MMWatchdog fleet hub" + raised)}
	case outageAlerter && e.Type == "recovered":
		return []TitlePart{who(e.Node), words(plural(" reaches", " reach") + " the MMWatchdog fleet hub again")}
	case outageAlerter && e.Type == "closed_without_recovery":
		return []TitlePart{who(e.Node), words(plural(" no longer watches", " no longer watch") + " the MMWatchdog fleet hub; " + plural("its alert", "their alerts") + " about the hub " + plural("is", "are") + " closed")}
	}
	// An alert about a check: "NODE: BEFORE GROUP/CHECK AFTER", or for
	// several nodes "BEFORE GROUP/CHECK AFTER on N nodes". An alert about
	// one failed unit of an every_systemd_unit check names the unit
	// after the check: "NODE: GROUP/CHECK: UNIT AFTER". An incident that
	// closed without recovery ends with ", maybe not fixed".
	before, after, closed := "", "", ""
	switch {
	case e.Type == "failed":
		after = " failed"
	case still:
		after = " still failing"
	case e.Type == "cannot_check":
		before = "MMWatchdog cannot check "
	case e.Type == CannotCheckIncidentEvents.Raised || e.Type == "reminder":
		before = "MMWatchdog still cannot check "
	case e.Type == "recovered":
		after = " is ok again"
	case e.Type == "can_check_again":
		before, after = "MMWatchdog can check ", " again"
	case e.Type == "closed_without_recovery":
		after, closed = plural(" incident", " incidents")+" closed", ", maybe not fixed"
	}
	parts := []TitlePart{}
	if !several {
		parts = append(parts, name(e.Node), words(": "))
	}
	parts = append(parts, words(before), name(e.CheckGroup))
	if e.Check != "" {
		parts = append(parts, words("/"), name(e.Check))
	}
	if e.Finding != "" {
		parts = append(parts, words(": "), name(e.Finding))
	}
	parts = append(parts, words(after))
	if several {
		parts = append(parts, words(" on "+nodes))
	}
	return append(parts, words(closed+raised))
}

// FleetIncidentClosingTitle is the title of the one alert that closes a key
// of a destination's fleet incident set on the hub, once no node has an
// incident of the key open any more, before it is fitted, and what was open,
// for the body. readable is the key, about holds the check group, the
// check, and the finding of the key's incidents, nodes are the nodes whose
// incidents closed, more counts those that nodes leaves out, and change is
// what closed the last of them: recovered, can_check_again, or
// closed_without_recovery. When the incidents of one node closed, the title
// is that node's own, such as "util01: myapp/http is ok again" or "util01
// reports to the MMWatchdog fleet hub again", so it pairs with the failure
// alert. The mmhotline
// destination writes the same words into the resolution that it sends to
// MMHotline.
func FleetIncidentClosingTitle(readable string, about Event, nodes []string, more int, change string) ([]TitlePart, string) {
	names := strings.Join(nodes, ", ")
	if more > 0 {
		names += fmt.Sprintf(", and %d more", more)
	}
	words := func(text string) TitlePart { return TitlePart{Text: text} }
	name := func(text string) TitlePart { return TitlePart{Text: text, name: true} }
	subject := func(before, after string) []TitlePart {
		parts := []TitlePart{words(before), name(about.CheckGroup)}
		if about.Check != "" {
			parts = append(parts, words("/"), name(about.Check))
		}
		if about.Finding != "" {
			parts = append(parts, words(": "), name(about.Finding))
		}
		return append(parts, words(after))
	}
	closed := change == "closed_without_recovery"
	outageAlerter := about.CheckGroup == NodeReportsCheckGroup && about.Check == HubCheck
	switch {
	case outageAlerter && closed:
		return []TitlePart{words("Alerts that "), name(names), words(" cannot reach the MMWatchdog fleet hub closed, maybe not fixed")}, ""
	case outageAlerter:
		return []TitlePart{words("Reports from "), name(names), words(" reach the MMWatchdog fleet hub again")}, ""
	case len(nodes) == 1 && more == 0:
		one := Event{Node: nodes[0], CheckGroup: about.CheckGroup, Check: about.Check, Finding: about.Finding, Type: FailureIncidentEvents.Recovered}
		switch {
		case closed:
			one.Type = "closed_without_recovery"
		case ReadableKeyIsAboutCannotCheck(readable):
			one.Type = CannotCheckIncidentEvents.Recovered
		}
		// The hub reports the silence of a node as a check of its own
		// named after the node.
		hub := ""
		if readable == SilenceIncidentKey {
			one.Node, hub = about.Node, about.Node
		}
		return AlertTitleParts(one, hub), ""
	case readable == SilenceIncidentKey && closed:
		return []TitlePart{words("Alerts about nodes that stopped reporting to the MMWatchdog fleet hub closed, maybe not fixed")}, "stopped reporting: " + names
	case readable == SilenceIncidentKey:
		return []TitlePart{words("Every node is reporting to the MMWatchdog fleet hub again")}, "stopped reporting: " + names
	case closed:
		return subject("", " incidents closed on every node, maybe not fixed"), "was open on " + names
	case ReadableKeyIsAboutCannotCheck(readable):
		return subject("MMWatchdog can check ", " on every node again"), "could not be checked on " + names
	}
	return subject("", " is ok on every node again"), "was failing on " + names
}

// TitleText joins the parts of a title as they are, without fitting them.
func TitleText(parts []TitlePart) string {
	text := strings.Builder{}
	for _, part := range parts {
		text.WriteString(part.Text)
	}
	return text.String()
}

// WriteFleetIncidentClosingTexts gives the hub's one alert that closes a
// key of a destination's fleet incident set, whose type, check group,
// check, finding, and nodes are set, its title (FleetIncidentClosingTitle, fitted
// like every title), the title with its severity, its message, which says
// what was open, and its body: the message, and the latest message of a
// node when an alert of a node closed the key.
func (e *Event) WriteFleetIncidentClosingTexts(readable, latest string) {
	parts, was := FleetIncidentClosingTitle(readable, *e, e.Nodes, e.NodeCount-len(e.Nodes), e.Type)
	e.Message = TitleText(parts)
	if was != "" {
		e.Message += " (" + was + ")"
	}
	e.Title, e.TitleWasShortened = FitTitle(parts)
	e.TitleWithAlertSeverity = titleWithAlertSeverity(*e)
	lines := []string{e.Message}
	if latest != "" {
		lines = append(lines, "latest: "+latest)
	}
	e.Body = primitives.AsReadableLines(strings.Join(lines, "\n"))
}

// FitTitle joins the parts of a title. A title longer than AlertTitleLimit
// is cut inside its longest name, and then inside the name that is then
// the longest, so the last words, which say what changed, always stay. It
// reports whether it cut.
func FitTitle(parts []TitlePart) (string, bool) {
	length := func() int {
		n := 0
		for _, part := range parts {
			n += utf8.RuneCountInString(part.Text)
		}
		return n
	}
	shortened := false
	for excess := length() - AlertTitleLimit; excess > 0; excess = length() - AlertTitleLimit {
		longest, most := -1, ShortestCutName
		for i, part := range parts {
			if n := utf8.RuneCountInString(part.Text); part.name && n > most {
				longest, most = i, n
			}
		}
		if longest < 0 {
			break
		}
		// The mark of the cut takes one character.
		keep := max(most-excess-1, ShortestCutName-1)
		parts[longest].Text = string([]rune(parts[longest].Text)[:keep]) + "…" //nolint:nilaway // longest is assigned only while ranging over parts; the longest < 0 guard prevents indexing without a selected part.
		shortened = true
	}
	title := TitleText(parts)
	// Names cut as far as they go leave room for every title MMWatchdog writes.
	if utf8.RuneCountInString(title) > AlertTitleLimit {
		return FitCharacters(title, AlertTitleLimit), true
	}
	return title, shortened
}

// ClockTime writes a time as "14:02 UTC", and with its date when it is not
// on the day of the alert.
func ClockTime(t, alert time.Time) string {
	t, alert = t.UTC(), alert.UTC()
	if t.Format(time.DateOnly) == alert.Format(time.DateOnly) {
		return t.Format("15:04 UTC")
	}
	return t.Format("2006-01-02 15:04 UTC")
}

// AlertBody is everything else a person needs to act, as readable text: the
// alert's message, since when the check has been failing, the checks that
// MMWatchdog keeps quiet while this one fails, and a short excerpt of the evidence.
// A hub's grouped message names its nodes in its
// message; the lines that follow are from the first of them. The body of a
// notice is the text its sender gave, or nothing: its message is its title.
func AlertBody(e Event, members []Event, hub string) string {
	if e.Type == "notice" {
		return e.Body
	}
	lines := []string{e.Message}
	if !Opens(e.Type) && e.Type != "reminder" {
		return e.Message
	}
	first := e
	if len(members) > 0 {
		first = members[0]
	}
	detail := []string{}
	if len(members) > 1 {
		detail = append(detail, first.Message)
	}
	// A reminder's message already says since when.
	if evidence := first.Evidence; evidence != nil && e.Type != "reminder" {
		switch {
		case !evidence.CannotCheckSince.IsZero():
			detail = append(detail, "MMWatchdog has not been able to run the check since "+ClockTime(evidence.CannotCheckSince, e.At))
		case !evidence.FailingSince.IsZero() && evidence.LastRunsCounted > 0:
			detail = append(detail, fmt.Sprintf("failing on and off since %s; %d of the last %d runs failed", ClockTime(evidence.FailingSince, e.At), evidence.LastRunsFailed, evidence.LastRunsCounted))
		case !evidence.FailingSince.IsZero():
			detail = append(detail, "failing since "+ClockTime(evidence.FailingSince, e.At))
		}
	}
	if line := quietChecksLine(first); line != "" {
		detail = append(detail, line)
	}
	detail = append(detail, evidenceLines(first.Evidence)...)
	if len(members) > 1 && len(detail) > 0 {
		lines = append(lines, "from "+AlertNode(first, hub)+":")
		for _, line := range detail {
			lines = append(lines, "  "+line)
		}
	} else {
		lines = append(lines, detail...)
	}
	return strings.Join(NonEmpty(lines), "\n")
}

// evidenceLines is a short excerpt of an incident's evidence: the details of
// the confirming observation, the exit code, and the first five lines of
// stdout and of stderr.
func evidenceLines(e *Evidence) []string {
	if e == nil {
		return nil
	}
	o := e.Observation
	lines := slices.Clone(o.Details)
	if o.ExitCode != nil {
		lines = append(lines, fmt.Sprintf("exit code: %d", *o.ExitCode))
	}
	for _, output := range []struct{ name, text string }{{"stdout", o.Stdout}, {"stderr", o.Stderr}} {
		text := strings.TrimSpace(output.text)
		if text == "" {
			continue
		}
		lines = append(lines, output.name+":")
		first := strings.SplitN(text, "\n", 6)
		for i, line := range first {
			if i == 5 {
				line = "…"
			}
			lines = append(lines, "  "+primitives.Clip(strings.TrimRight(line, "\r"), 200))
		}
	}
	return lines
}

// AlertTitleThatWillBeShortened is a check whose alert titles are too long
// on this node, for mmwatchdog validate.
type AlertTitleThatWillBeShortened struct {
	CheckGroup string `json:"check_group"`
	Check      string `json:"check"`
	// Characters is the length of the check's longest title before MMWatchdog cuts it.
	Characters int `json:"characters"`
	// Title is that title as MMWatchdog would send it.
	Title string `json:"title"`
}

// AlertNode names the node an alert is about: its node, or for the hub's
// alert about a silent node, that node.
func AlertNode(e Event, hub string) string {
	if e.CheckGroup == NodeReportsCheckGroup && e.Node == hub {
		return e.Check
	}
	return e.Node
}

// Silence alerts and the hub-outage alerter's alert about the hub. On the hub, each joined node is a
// check of the built-in fleet check; on the node the hub chose as its
// hub-outage alerter, the hub is one. Both behave like heartbeats whose receipts are
// reports: see "Silence is a heartbeat created by joining" in
// docs/implementation-rules.md.
const (
	NodeReportsCheckGroup = "node_reports"
	HubCheck              = "to_hub" // the hub-outage alerter's check
)

// quietChecksLine is the line in the body of a failing check's alert that
// names the checks it keeps quiet, or "" when it keeps none quiet.
func quietChecksLine(e Event) string {
	named := e.NoAlertsOrRecoveryActionsForTheseChecksWhileThisCheckFails
	if len(named) == 0 {
		return ""
	}
	line := "no alerts and no recovery actions for these checks while this check fails: " + strings.Join(named, ", ")
	if more := e.NumberOfChecksWithNoAlertsOrRecoveryActionsWhileThisCheckFails - len(named); more > 0 {
		line += fmt.Sprintf(", and %d more", more)
	}
	return line
}

// IncidentKind is the incident kind an alert is about: failed for the
// failure incident's alerts, cannot_check for the cannot-check incident's.
func IncidentKind(eventType string) string {
	switch eventType {
	case "cannot_check", "can_check_again", CannotCheckIncidentEvents.Raised:
		return "cannot_check"
	}
	return "failed"
}

// Opens reports an alert that announces or raises an incident.
func Opens(eventType string) bool {
	switch eventType {
	case "failed", "cannot_check", FailureIncidentEvents.Raised, CannotCheckIncidentEvents.Raised:
		return true
	}
	return false
}

func NonEmpty(lines []string) []string {
	out := []string{}
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// FitCharacters shortens s to at most n characters, marking the cut.
func FitCharacters(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// Closes reports an event that ends an incident's alerts.
func Closes(eventType string) bool {
	return eventType == "recovered" || eventType == "can_check_again" || eventType == "closed_without_recovery"
}
