package daemon

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
)

// fleetTimings are the fleet timings under configuration c: the hub's own, or
// on a joined node those in its copy of the fleet settings. The caller
// holds d.mu or owns d.
func (d *Daemon) fleetTimings(c *config.Config) config.FleetTimings {
	switch {
	case c.FleetHubRole != nil:
		return c.FleetHubRole.FleetTimings
	case d.fleetSettings != nil:
		return d.fleetSettings.Fleet
	}
	return config.DefaultFleetTimings
}

// definitions are the checks under configuration c: its checks, and the
// fleet checks. A joined node's check is identified by when it
// joined, so a replacement retires the old one; its deadline follows the
// fleet timings without changing its identity. The caller holds d.mu or owns
// d.
func (d *Daemon) definitions(c *config.Config) map[string]config.Definition {
	defs := config.CheckDefinitions(c)
	timings := d.fleetTimings(c)
	if c.FleetHubRole != nil {
		for name, m := range d.members {
			defs[fleet.FleetKey(name)] = fleet.MemberDefinition(name, m.Joined, timings, m.Silence(), m.SendSilenceAlertsOfThisNodeOnlyToDestinations)
		}
	}
	if c.JoinFleetHub != nil && d.watching && d.fleetSettings != nil {
		// The hub check exists once this node holds the fleet settings,
		// whose silence severity and raise time it takes; applyFleetSettings adds
		// it then.
		severity := checksandalerts.AlertSeveritySettings{AlertSeverity: d.fleetSettings.AlertSeverityWhenTheHubGoesSilent, RaiseToSosAfterFailingFor: d.fleetSettings.RaiseToSosAfterTheHubIsSilentFor}
		defs[fleet.FleetKey(checksandalerts.HubCheck)] = config.Definition{CheckGroup: checksandalerts.NodeReportsCheckGroup, Name: checksandalerts.HubCheck, Heartbeat: &checksandalerts.Heartbeat{MaxTimeBetweenHeartbeats: timings.AlertAfterHubUnreachableFor}, Severity: severity, SendAlertsOnlyToDestinations: d.fleetSettings.SendSilenceAlertsOfTheHubOnlyToDestinations}
	}
	return defs
}

// startFleet gives every fleet check a fresh deadline at daemon start, so
// a restart never manufactures alerts. On the hub, members carry each
// node's last report, for messages, and the whole-node window it showed.
// The caller owns d.
func (d *Daemon) startFleet(members map[string]fleet.FleetMember, now time.Time) {
	for key, rc := range d.checks {
		if rc.def.CheckGroup != checksandalerts.NodeReportsCheckGroup {
			continue
		}
		st := d.snapshot.Checks[key]
		st.Deadline = now.Add(rc.def.Heartbeat.MaxTimeBetweenHeartbeats.Value()) //nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
		if m, ok := members[rc.def.Name]; ok {
			st.Receipt, rc.until = incidents.OnThisClock(m.Received, now), m.Until //nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
			// The node's latest saved report keeps its silence severity and
			// its raise time.
			//nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
			if silence := m.Silence(); silence.AlertSeverity != "" && st.AlertSeveritySettings != silence {
				st.Touch()
				st.AlertSeveritySettings = silence
			}
		}
	}
}

// applyFleet is the hub-outage alerter role entry point, a record: on a joined node,
// a change of the role creates or retires the hub check. The caller holds
// d.mu.
func (d *Daemon) applyFleet(now time.Time) {
	defs := d.definitions(d.config)
	events := d.snapshot.Reconcile(d.config, defs, now)
	d.setDefinitions(defs)
	_ = d.record(events)
}

// applyMember is the fleet membership entry point on the hub: a join, a
// replacement, or a forget creates or retires that node's check alone,
// so its cost does not grow with the fleet. The membership is already
// committed in fleet.db, from which daemon start derives the checks
// again, so the change and its retirement alerts wait for the scheduler's
// next tick to commit. The caller holds d.mu.
func (d *Daemon) applyMember(name string, now time.Time) {
	key := fleet.FleetKey(name)
	var def *config.Definition
	if m, ok := d.members[name]; ok {
		member := fleet.MemberDefinition(name, m.Joined, d.fleetTimings(d.config), m.Silence(), m.SendSilenceAlertsOfThisNodeOnlyToDestinations)
		def = &member
	}
	d.pending = append(d.pending, d.snapshot.ReconcileOne(d.config, key, def, now)...)
	d.setDefinition(key, def)
}

// joined applies a join the fleet store has committed, with the settings
// that the node sent for the fleet. Concurrent joins apply in any order, so
// an older membership never replaces a newer one.
func (d *Daemon) joined(name, address string, at time.Time, settings config.NodeFleetSettings) {
	d.mu.Lock()
	defer d.mu.Unlock()
	current, replaced := d.members[name]
	if replaced && !at.After(current.Joined) {
		return
	}
	d.members[name] = fleet.FleetMember{Joined: at, NodeFleetSettings: settings}
	d.applyMember(name, at)
	if d.withoutJoinNotices {
		return
	}
	// Anyone with the join code can join, so each join is told. Reason: a
	// node that nobody added could take a hub-outage alerter place or a
	// node group, and receives the destination credentials.
	title := "node " + name + " joined the fleet from " + address
	if replaced {
		title = "a new node from " + address + " took the name " + name + " of a silent node"
	}
	body := "If you did not add this node, someone else has the join code: run mmwatchdog fleet rotate-join-code, then mmwatchdog fleet forget " + name + ", and change the credentials of the alert destinations."
	event := checksandalerts.Event{ID: incidents.ID(), Type: "notice", AlertSeverity: "info", At: at, Message: title, Body: body, Notify: true}
	_ = d.record([]checksandalerts.Event{event}) // record keeps the notice pending and reports a storage failure.
}

// forgot applies a forget the fleet store has committed: the node's
// check retires without claiming recovery. joined is when the forgotten
// node joined, so a join committed since then stays.
func (d *Daemon) forgot(name string, joined, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if current, ok := d.members[name]; !ok || !current.Joined.Equal(joined) {
		return
	}
	delete(d.members, name)
	d.applyMember(name, now)
}

// receipt applies a current report from a joined node. until is the
// whole-node window that report shows, and settings are the node's settings
// for the fleet: the hub keeps the node's silence severity, its raises, and
// its node groups for when the node goes quiet.
func (d *Daemon) receipt(name string, until time.Time, settings config.NodeFleetSettings, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	silence := settings.Silence()
	if m, ok := d.members[name]; ok {
		m.NodeFleetSettings = settings
		d.members[name] = m
	}
	if st := d.snapshot.Checks[fleet.FleetKey(name)]; st != nil && st.AlertSeveritySettings != silence {
		st.Touch()
		st.AlertSeveritySettings = silence
	}
	if st := d.snapshot.Checks[fleet.FleetKey(name)]; st != nil && !reflect.DeepEqual(st.SendAlertsOnlyToDestinations, settings.SendSilenceAlertsOfThisNodeOnlyToDestinations) {
		st.Touch()
		st.SendAlertsOnlyToDestinations = settings.SendSilenceAlertsOfThisNodeOnlyToDestinations
	}
	d.receive(fleet.FleetKey(name), name+" is reporting", until, now)
}

// contact applies a current report from a joined node that the hub could not
// save: the node is reporting, and the settings that the hub keeps for it do
// not change. until is the whole-node window that the report shows.
func (d *Daemon) contact(name string, until, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.receive(fleet.FleetKey(name), name+" is reporting", until, now)
}

// watch applies a successful report on a joined node: the hub-outage alerter role the
// reply names, and the receipt for the hub check. A new hub-outage alerter's
// check exists before the receipt, and a former hub-outage alerter's receipt resolves
// any incident before its check retires. The caller holds d.mu.
func (d *Daemon) watch(outageAlerter bool, now time.Time) {
	if outageAlerter && !d.watching {
		d.watching = true
		d.applyFleet(now)
	}
	d.receive(fleet.FleetKey(checksandalerts.HubCheck), "reports reach the hub", time.Time{}, now)
	if !outageAlerter && d.watching {
		d.watching = false
		d.applyFleet(now)
	}
}

// receive moves a fleet check's deadline. Receipts stay in memory: only a
// receipt that changes the check's observation is applied, and its
// transitions wait for the scheduler's next tick to commit, so the reports of
// many nodes returning together share a few commits. The caller holds d.mu.
func (d *Daemon) receive(key, message string, until, now time.Time) {
	rc, st := d.checks[key], d.snapshot.Checks[key]
	if rc == nil || st == nil {
		return
	}
	rc.until = until
	st.Receipt, st.Deadline = now, now.Add(rc.def.Heartbeat.MaxTimeBetweenHeartbeats.Value())
	// The hub-outage alerter keeps its last successful contact for raises after restart.
	// One row per hub-outage alerter report does not grow with the fleet's node count.
	if d.config.JoinFleetHub != nil && rc.def.Name == checksandalerts.HubCheck {
		st.Touch()
	}
	if st.Last.Status == "ok" && st.FailureIncident.Incident == "" && st.FailureIncident.Announced == "" {
		return
	}
	if st.FailureIncident.Incident != "" || st.FailureIncident.Announced != "" {
		message += " again"
	}
	o := checksandalerts.Result("ok", message)
	o.At = now
	d.pending = append(d.pending, st.Observe(o, rc.def.Timing, d.fleetSuppressed(rc, now))...)
}

// fleetSuppressed reports that the fleet check is in maintenance, or that a
// joined node's last report showed a whole-node window that still lasts.
// The caller holds d.mu.
func (d *Daemon) fleetSuppressed(rc *checkRuntime, now time.Time) bool {
	return d.suppressed(checksandalerts.NodeReportsCheckGroup) || now.Before(rc.until)
}

// fleetTick is the scheduler tick for a fleet check. A missed deadline
// opens its incident. Once nothing suppresses it, an incident still open at
// the deadline is announced and an announced one that resolved meanwhile is
// closed, so a window's end is a maintenance boundary like any other. The
// caller holds d.mu.
func (d *Daemon) fleetTick(rc *checkRuntime, st *incidents.CheckState, now time.Time) []checksandalerts.Event {
	suppressed := d.fleetSuppressed(rc, now)
	switch {
	case !now.Before(st.Deadline) && (st.FailureIncident.Incident == "" || !suppressed && st.FailureIncident.Announced != st.FailureIncident.Incident):
		o := checksandalerts.Result("failed", d.silenceMessage(rc, st))
		o.At = now
		return st.Observe(o, rc.def.Timing, suppressed)
	case !suppressed && st.FailureIncident.Incident == "" && st.FailureIncident.Announced != "" && st.Last.Status == "ok":
		o := st.Last //nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
		o.At = now
		return st.Observe(o, rc.def.Timing, false)
	}
	return nil
}

// silentForReason is the message of the raise of a fleet check's alert to
// sos by time: on the hub, a joined node has sent no report for its
// raise_to_sos_after_this_node_is_silent_for, and on the hub-outage alerter node,
// no report of this node has reached the hub for the hub's. The time counts
// from the moment the node last reported. The caller holds d.mu.
func (d *Daemon) silentForReason(rc *checkRuntime, st *incidents.CheckState) string {
	after := string(st.AlertSeveritySettings.RaiseToSosAfterFailingFor) //nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
	if d.hub == nil {
		return incidents.RaiseMessage("no report from this node has reached the MMWatchdog fleet hub for "+after, "sos", "the hub's raise_to_sos_after_this_node_is_silent_for: "+after)
	}
	return incidents.RaiseMessage(rc.def.Name+" has sent no report to the hub for "+after, "sos", "raise_to_sos_after_this_node_is_silent_for: "+after)
}

// silenceMessage explains a missed deadline: since when a node has sent no
// report, or on the hub-outage alerter, since when no report of this node has reached
// the hub. The hub-outage alerter's alert points to status for the latest report's
// error, which is detail for whoever looks into it, not text for a phone's
// lock screen; status and the journal show it. A node that has not reported
// since the hub started or since it joined counts from then. The caller
// holds d.mu.
func (d *Daemon) silenceMessage(rc *checkRuntime, st *incidents.CheckState) string {
	last := st.Receipt
	if last.IsZero() {
		last = st.Deadline.Add(-rc.def.Heartbeat.MaxTimeBetweenHeartbeats.Value())
	}
	since := last.UTC().Format("15:04 UTC")
	if d.hub == nil {
		message := "no report from this node has reached the MMWatchdog fleet hub since " + since
		if d.link != nil && d.link.LastError != "" {
			message += "; mmwatchdog status on this node shows the latest error"
		}
		return message
	}
	return rc.def.Name + " has sent no report to the hub since " + since
}

// raiseSilentNodeAfterTime counts silence from the last receipt, or from
// the fresh startup/join window when no receipt exists. It keeps the
// incident's actual opening time and uses the scheduler's monotonic clock.
func (d *Daemon) raiseSilentNodeAfterTime(rc *checkRuntime, st *incidents.CheckState, now time.Time, suppressed bool) []checksandalerts.Event {
	after := st.AlertSeveritySettings.RaiseToSosAfterFailingFor.Value()
	since := st.Receipt
	if since.IsZero() {
		since = st.Deadline.Add(-rc.def.Heartbeat.MaxTimeBetweenHeartbeats.Value())
	}
	if after <= 0 || now.Sub(since) < after || st.FailureIncident.Incident == "" {
		return nil
	}
	return st.Raise(&st.FailureIncident, checksandalerts.FailureIncidentEvents, "sos", d.silentForReason(rc, st), now, suppressed)
}

// raiseSilentNodeGroups is the hub's raise by node group. silent are the
// joined nodes that the raise by count counts as silent, in the order of
// their names: an open silence incident whose deadline has passed and that
// no window or maintenance holds. A silent node that names a node group in
// its raise_to_sos_when_every_node_of_these_node_groups_is_silent is raised
// to sos while every node of that node group is silent. The nodes of a node
// group are those whose settings name it, as in the fleet view
// (fleet.Hub.FleetOfNodeGroup), from the settings that the daemon keeps for
// each member; the hub's own node groups always have a node that reports.
// The caller holds d.mu.
func (d *Daemon) raiseSilentNodeGroups(silent []*incidents.CheckState, now time.Time) []checksandalerts.Event {
	// The nodes of each node group that a silent node names.
	nodesOf := map[string][]string{}
	isSilent := map[string]bool{}
	for _, st := range silent {
		isSilent[st.Check] = true
		for _, nodeGroup := range d.members[st.Check].RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent {
			nodesOf[nodeGroup] = nil
		}
	}
	if len(nodesOf) == 0 {
		return nil
	}
	reporting := map[string]bool{}
	for _, nodeGroup := range d.config.ThisNodeIsInNodeGroups {
		reporting[nodeGroup] = true
	}
	for name, m := range d.members {
		for _, nodeGroup := range m.ThisNodeIsInNodeGroups {
			if _, named := nodesOf[nodeGroup]; !named {
				continue
			}
			if !isSilent[name] {
				reporting[nodeGroup] = true
			}
			nodesOf[nodeGroup] = append(nodesOf[nodeGroup], name)
		}
	}
	because := map[string]string{}
	for nodeGroup, nodes := range nodesOf {
		if !reporting[nodeGroup] {
			slices.Sort(nodes)
			because[nodeGroup] = "no node of the node group " + nodeGroup + " is reporting to the hub (" + silentNodesText(nodes) + ")"
		}
	}
	events := []checksandalerts.Event{}
	for _, st := range silent {
		raiseWhenSilent := d.members[st.Check].RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent
		for _, nodeGroup := range raiseWhenSilent {
			if because[nodeGroup] == "" {
				continue
			}
			message := incidents.RaiseMessage(because[nodeGroup], "sos", "raise_to_sos_when_every_node_of_these_node_groups_is_silent: ["+strings.Join(raiseWhenSilent, ", ")+"]")
			events = append(events, st.Raise(&st.FailureIncident, checksandalerts.FailureIncidentEvents, "sos", message, now, false)...)
			break
		}
	}
	return events
}

// silentNodesText names silent nodes in plain words, such as "jobs-1 and
// jobs-2 are silent": the first store.CombinedNames of them, in the order
// given, and how many more there are.
func silentNodesText(nodes []string) string {
	shown := nodes[:min(len(nodes), store.CombinedNames)]
	switch {
	case len(nodes) == 1:
		return nodes[0] + " is silent"
	case len(nodes) == 2:
		return nodes[0] + " and " + nodes[1] + " are silent"
	case len(nodes) > len(shown):
		return fmt.Sprintf("%s, and %d more are silent", strings.Join(shown, ", "), len(nodes)-len(shown))
	}
	return strings.Join(shown[:len(shown)-1], ", ") + ", and " + shown[len(shown)-1] + " are silent"
}
