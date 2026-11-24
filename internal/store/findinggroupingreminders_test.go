package store_test

import (
	"slices"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/store"
)

// The hub keeps one reminder schedule per destination and key, following
// the destination's remind_every, and reminds only while a member is
// eligible: not in maintenance or quiet time, as its node reports. Nodes'
// own reminders of the key are dropped at intake, also those of a 0.1.x
// node, which knows no exact finding. MMHotline gets no reminders.
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
	// A node's own reminder of the key never reaches a destination.
	old := h.alert("old-1", "reminder", "warn", "", 0, tenOFive.Add(3*time.Hour))
	old.Destination = "chat"
	current := h.alert("app-2", "reminder", "warn", versionA, 0, tenOFive.Add(3*time.Hour))
	current.Destination = "chat"
	h.arrive(tenOFive.Add(3*time.Hour), old, current)
	expectKinds(t, h.take("chat"))
	// MMHotline got the growth update of 12:00, and no reminder.
	expectKinds(t, h.take("hotline"), "finding_nodes_changed warn")
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
