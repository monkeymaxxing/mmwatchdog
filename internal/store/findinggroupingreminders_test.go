package store_test

import (
	"slices"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/store"
	"mmwatchdog/internal/testhelpers"
)

// The hub keeps one reminder schedule per destination and key, following
// the destination's remind_every, and reminds only while a member is
// eligible: not in maintenance or quiet time, as its node reports. A
// node's own reminder that the hub's reminder covers is dropped at intake;
// that of a 0.1.x node, which knows no exact finding and so is in no
// grouped reminder, goes on. MMHotline gets no reminders.
func TestOneReminderPerDestinationAndKeyWhileAMemberIsEligible(t *testing.T) {
	t.Parallel()
	chat := chatTaking("https://chat.example.com", "warn")
	chat.RemindEvery = "1h"
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chat, "hotline": hotline()})
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	h.arrive(tenOFive.Add(time.Minute), h.alert("app-2", "failed", "warn", versionA, 0, tenOFive.Add(time.Minute)))
	expectKinds(t, h.take("chat"), "failed warn")
	expectKinds(t, h.take("hotline"), "failed warn")
	eligible := func(at time.Time, app1, app2 bool) {
		one, two := shows("app-1", "warn", versionA, 0, tenOFive, at), shows("app-2", "warn", versionA, 0, tenOFive.Add(time.Minute), at)
		one.ReminderEligible["incident-app-1"], two.ReminderEligible["incident-app-2"] = app1, app2
		h.report(at, map[string]store.ReportedIncidents{"app-1": one, "app-2": two})
	}
	eligible(tenOFive.Add(30*time.Minute), true, false)
	h.tick(tenOFive.Add(time.Hour - time.Second))
	expectKinds(t, h.take("chat"))
	h.tick(tenOFive.Add(time.Hour))
	got := h.take("chat")
	expectKinds(t, got, "reminder warn")
	if got[0].Node != "app-1" || got[0].Destination != "chat" || got[0].NodeCount != 0 {
		t.Fatal("the reminder is not about the one eligible node", got[0].Node, got[0].Nodes)
	}
	eligible(tenOFive.Add(90*time.Minute), false, false)
	// The shared tick at 12:00 has passed: app-2's growth goes out, and no
	// reminder.
	h.tick(tenOFive.Add(2 * time.Hour))
	expectKinds(t, h.take("chat"), "finding_nodes_changed warn")
	eligible(tenOFive.Add(2*time.Hour+30*time.Second), true, true)
	h.tick(tenOFive.Add(2*time.Hour + time.Minute))
	got = h.take("chat")
	expectKinds(t, got, "reminder warn")
	if !slices.Equal(got[0].Nodes, []string{"app-1", "app-2"}) {
		t.Fatal(got[0].Nodes)
	}
	// A node's own reminder that the hub's reminder covers never reaches
	// the destination; the 0.1.x node's does.
	old := h.alert("old-1", "reminder", "warn", "", 0, tenOFive.Add(3*time.Hour))
	old.Destination = "chat"
	current := h.alert("app-2", "reminder", "warn", versionA, 0, tenOFive.Add(3*time.Hour))
	current.Destination = "chat"
	h.arrive(tenOFive.Add(3*time.Hour), old, current)
	got = h.take("chat")
	expectKinds(t, got, "reminder warn")
	if got[0].Node != "old-1" {
		t.Fatal("the reminder that went on is", got[0].Node)
	}
	// MMHotline got the growth update of 12:00, and no reminder.
	expectKinds(t, h.take("hotline"), "finding_nodes_changed warn")
}

// A node's reminder to a destination whose grouped reminder does not
// cover the node's incident goes on: node app-1 has no exact finding and
// sends to ops, while app-2 has one for the same key and sends only to
// security. The hub reminds security of app-2 and drops app-2's own
// reminder there, and app-1's reminder reaches ops.
func TestOnlyANodesReminderThatTheHubsReminderCoversIsDropped(t *testing.T) {
	t.Parallel()
	ops, security := chatTaking("https://ops.example.com", "warn"), chatTaking("https://security.example.com", "warn")
	ops.RemindEvery, security.RemindEvery = "1h", "1h"
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"ops": ops, "security": security})
	to := func(alert checksandalerts.Event, destination string) checksandalerts.Event {
		alert.SendAlertsOnlyToDestinations = &[]string{destination}
		return alert
	}
	h.arrive(tenOFive, to(h.alert("app-2", "failed", "warn", versionA, 0, tenOFive), "security"))
	h.arrive(tenOFive.Add(time.Minute), to(h.alert("app-1", "failed", "warn", "", 0, tenOFive.Add(time.Minute)), "ops"))
	expectKinds(t, h.take("security"), "failed warn")
	expectKinds(t, h.take("ops"), "failed warn")
	reminders := []checksandalerts.Event{}
	for _, r := range []struct{ node, exactFinding, destination string }{{"app-1", "", "ops"}, {"app-2", versionA, "security"}} {
		reminder := to(h.alert(r.node, "reminder", "warn", r.exactFinding, 0, tenOFive.Add(time.Hour)), r.destination)
		reminder.Destination = r.destination
		reminders = append(reminders, reminder)
	}
	h.arrive(tenOFive.Add(time.Hour), reminders...)
	got := h.take("ops")
	expectKinds(t, got, "reminder warn")
	if got[0].Node != "app-1" {
		t.Fatal("ops got the reminder of", got[0].Node)
	}
	h.tick(tenOFive.Add(time.Hour + time.Minute))
	got = h.take("security")
	expectKinds(t, got, "reminder warn")
	if got[0].Node != "app-2" || got[0].ID == reminders[1].ID {
		t.Fatal("security got a reminder other than the hub's own", got[0])
	}
}

// A node that raised its incident to sos during a hub outage sends its sos
// reminder before its next report: the hub's own reminder of the key is
// still at warn, so the node's sos reminder goes on.
func TestAnSosReminderOfAnIncidentTheHubHoldsAtWarnGoesOut(t *testing.T) {
	t.Parallel()
	chat := chatTaking("https://chat.example.com", "warn", "sos")
	chat.RemindEvery = "1h"
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chat})
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	expectKinds(t, h.take("chat"), "failed warn")
	reminder := h.alert("app-1", "reminder", "sos", versionA, 0, tenOFive.Add(time.Hour))
	reminder.Destination = "chat"
	h.arrive(tenOFive.Add(time.Hour), reminder)
	got := h.take("chat")
	if len(got) == 0 || got[0].Type != "reminder" || got[0].AlertSeverity != "sos" {
		t.Fatalf("the node's sos reminder did not go out: %+v", got)
	}
}

// A key that the destination got only from a node sending without the hub,
// during a hub outage, gets the hub's reminders once the hub settles the
// direct send: the node stops its own reminders while the hub is reachable.
func TestADirectSendStartsTheHubsReminders(t *testing.T) {
	t.Parallel()
	chat := chatTaking("https://chat.example.com", "warn")
	chat.RemindEvery = "1h"
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chat})
	at := tenOFive.Add(10 * time.Minute)
	h.report(at, map[string]store.ReportedIncidents{"app-1": carriedTo(shows("app-1", "warn", versionA, 0, tenOFive, at), "chat", "")})
	h.tick(at.Add(time.Hour))
	got := h.take("chat")
	expectKinds(t, got, "reminder warn")
	if got[0].Node != "app-1" {
		t.Fatal(got[0].Node)
	}
}

// A joined node's own reminder of a finding that a set-aside entry covers
// on the hub never reaches a destination: the node knows no verdict, and
// reminds of the incident while its reports fail.
func TestAJoinedNodesReminderOfASetAsideFindingIsDropped(t *testing.T) {
	t.Parallel()
	chat := chatTaking("https://chat.example.com", "warn")
	chat.RemindEvery = "1h"
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chat})
	h.arrive(tenOFive, h.alert("app-2", "failed", "warn", versionA, 0, tenOFive))
	expectKinds(t, h.take("chat"), "failed warn")
	h.setAside(tenOFive.Add(time.Minute), entryFor(versionA, reasonNoPAM))
	expectKinds(t, h.take("chat"), "closed_without_recovery warn")
	for interval := 1; interval <= 2; interval++ {
		at := tenOFive.Add(time.Duration(interval) * time.Hour)
		reminder := h.alert("app-2", "reminder", "warn", versionA, 0, at)
		reminder.Destination = "chat"
		h.arrive(at, reminder)
		h.tick(at)
		expectKinds(t, h.take("chat"))
	}
}

// A joined node's reminder of an incident that the hub has not learned of
// yet, such as one the node opened directly during a hub outage, is
// dropped when a set-aside entry covers the reminder itself; a reminder of
// another exact finding goes on.
func TestAReminderOfAnIncidentTheHubDoesNotKnowFollowsTheSetAsideFile(t *testing.T) {
	t.Parallel()
	chat := chatTaking("https://chat.example.com", "warn")
	chat.RemindEvery = "1h"
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chat})
	h.setAside(tenOFive, entryFor(versionA, reasonNoPAM))
	at := tenOFive.Add(time.Hour)
	covered, other := h.alert("app-2", "reminder", "warn", versionA, 0, at), h.alert("app-3", "reminder", "warn", versionB, 0, at)
	covered.Destination, other.Destination = "chat", "chat"
	h.arrive(at, covered, other)
	got := h.take("chat")
	expectKinds(t, got, "reminder warn")
	if got[0].Node != "app-3" {
		t.Fatal("the reminder that went on is of", got[0].Node)
	}
}

// A reminder at sos of an incident that the hub holds as set aside at warn
// goes out: the node raised it to sos during a hub outage, and sos is never
// set aside.
func TestAnSosReminderOfAnIncidentSetAsideAtWarnGoesOut(t *testing.T) {
	t.Parallel()
	chat := chatTaking("https://chat.example.com", "warn", "sos")
	chat.RemindEvery = "1h"
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"chat": chat})
	h.arrive(tenOFive, h.alert("app-2", "failed", "warn", versionA, 0, tenOFive))
	expectKinds(t, h.take("chat"), "failed warn")
	h.setAside(tenOFive.Add(time.Minute), entryFor(versionA, reasonNoPAM))
	expectKinds(t, h.take("chat"), "closed_without_recovery warn")
	at := tenOFive.Add(time.Hour)
	reminder := h.alert("app-2", "reminder", "sos", versionA, 0, at)
	reminder.Destination = "chat"
	h.arrive(at, reminder)
	expectKinds(t, h.take("chat"), "reminder sos")
}

// The hub's reminder of a key names only the members grouped by exact
// finding. At one destination whose service keeps incidents, app-1 has an
// exact finding and app-2, at sos, has none: app-2 reminds of its own
// incident, and the hub's reminder names app-1 alone at warn, so app-2's
// sos never wakes anyone twice.
func TestTheHubsReminderNamesOnlyGroupedMembers(t *testing.T) {
	t.Parallel()
	pushover := checksandalerts.Destination{Inputs: &checksandalerts.ModuleCall{Name: "own_pager", Module: &checksandalerts.InstalledModule{Name: "own_pager", Description: &checksandalerts.ModuleDescription{ServiceKeepsIncidents: true}}}, RemindEvery: "1h", AlertSeverities: testhelpers.AllSeverities}
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"pushover": pushover})
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", versionA, 0, tenOFive))
	h.arrive(tenOFive.Add(time.Minute), h.alert("app-2", "failed", "sos", "", 0, tenOFive.Add(time.Minute)))
	expectKinds(t, h.take("pushover"), "failed warn", "failed sos")
	at := tenOFive.Add(time.Hour + time.Minute)
	own := h.alert("app-2", "reminder", "sos", "", 0, at)
	own.Destination = "pushover"
	h.arrive(at, own)
	h.tick(at)
	got := h.take("pushover")
	expectKinds(t, got, "reminder sos", "reminder warn")
	if got[0].Node != "app-2" || got[1].Node != "app-1" || got[1].NodeCount != 0 {
		t.Fatalf("the reminders are of %s and of %s %v", got[0].Node, got[1].Node, got[1].Nodes)
	}
}

// An incident that the hub announced without an exact finding, before its
// node knew one, gets the hub's reminders once the hub settles the node's
// direct send of its first exact finding, made during a hub outage: the
// node leaves the reminders of an incident with an exact finding to the
// hub while the hub is reachable.
func TestADirectSendOfAFirstExactFindingGroupsAnOpenMembership(t *testing.T) {
	t.Parallel()
	pager := checksandalerts.Destination{Inputs: &checksandalerts.ModuleCall{Name: "own_pager", Module: &checksandalerts.InstalledModule{Name: "own_pager", Description: &checksandalerts.ModuleDescription{ServiceKeepsIncidents: true}}}, RemindEvery: "1h", AlertSeverities: testhelpers.AllSeverities}
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"pager": pager})
	h.arrive(tenOFive, h.alert("app-1", "failed", "warn", "", 0, tenOFive))
	expectKinds(t, h.take("pager"), "failed warn")
	back := tenOFive.Add(10 * time.Minute)
	h.report(back, map[string]store.ReportedIncidents{"app-1": carriedTo(shows("app-1", "warn", versionA, 0, tenOFive, back), "pager", "")})
	h.take("pager")
	for interval := 1; interval <= 2; interval++ {
		at := back.Add(time.Duration(interval) * time.Hour)
		h.report(at.Add(-time.Minute), map[string]store.ReportedIncidents{"app-1": shows("app-1", "warn", versionA, 0, tenOFive, at.Add(-time.Minute))})
		h.tick(at)
		got := slices.DeleteFunc(h.take("pager"), func(a checksandalerts.Event) bool { return a.Type != "reminder" })
		if len(got) != 1 || got[0].Node != "app-1" {
			t.Fatalf("interval %d: reminders %q", interval, kinds(got))
		}
	}
}
