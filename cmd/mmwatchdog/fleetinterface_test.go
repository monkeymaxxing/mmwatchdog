package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
)

func TestEventCommandsSendTheRequestedTimeSpan(t *testing.T) {
	socket := filepath.Join(os.TempDir(), "mmwatchdog-"+incidents.ID()+".sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan url.Values, 10)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Query()
		if r.URL.Path == "/events" {
			json.NewEncoder(w).Encode([]checksandalerts.Event{})
		} else {
			json.NewEncoder(w).Encode(fleet.FleetNode{Name: "app-1"})
		}
	}))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	for _, command := range [][]string{{"events", "app"}, {"fleet", "app-1"}} {
		args := append(append([]string{}, command...), "--since", "2026-10-07T14:00:00+02:00", "--until", "2026-10-07T15:00:00+02:00", "--socket", socket, "--json")
		if _, err := capture(t, func() error { return run(args) }); err != nil {
			t.Fatal(args, err)
		}
		query := <-requests
		if query.Get("since") != "2026-10-07T12:00:00Z" || query.Get("until") != "2026-10-07T13:00:00Z" {
			t.Fatal(query)
		}
		args = append(append([]string{}, command...), "--last", "7d", "--socket", socket, "--json")
		if _, err := capture(t, func() error { return run(args) }); err != nil {
			t.Fatal(args, err)
		}
		query = <-requests
		since, err := time.Parse(time.RFC3339Nano, query.Get("since"))
		if err != nil {
			t.Fatal(err)
		}
		until, err := time.Parse(time.RFC3339Nano, query.Get("until"))
		if err != nil {
			t.Fatal(err)
		}
		if until.Sub(since) != 7*24*time.Hour {
			t.Fatal(query)
		}
	}
	for _, command := range [][]string{{"events", "app"}, {"fleet", "app-1"}} {
		for _, tc := range []struct {
			flags   []string
			problem string
		}{
			{[]string{"--last", "1h", "--since", "2026-10-07T14:00:00Z"}, "choose one time span"},
			{[]string{"--until", "2026-10-07T14:00:00Z"}, "--until needs --since"},
			{[]string{"--since", "yesterday"}, "--since needs an RFC 3339 time"},
			{[]string{"--since", "2026-10-07T14:00:00Z", "--until", "later"}, "--until needs an RFC 3339 time"},
			{[]string{"--since", "2026-10-07T15:00:00Z", "--until", "2026-10-07T14:00:00Z"}, "choose an end at or after the start"},
			{[]string{"--last", "0s"}, "positive duration"},
		} {
			if err := run(append(append([]string{}, command...), tc.flags...)); err == nil || !strings.Contains(err.Error(), tc.problem) {
				t.Fatal(command, tc.flags, err)
			}
		}
	}
	for _, args := range [][]string{{"fleet", "--last", "1h"}, {"fleet", "join-code", "--last", "1h"}, {"fleet", "forget", "app-1", "--last", "1h"}} {
		if err := run(args); err == nil || !strings.Contains(err.Error(), "time options require mmwatchdog fleet NODE") {
			t.Fatal(args, err)
		}
	}
}

func TestEventLineNamesOnlyWhatTheEventIsAbout(t *testing.T) {
	for _, c := range []struct {
		event checksandalerts.Event
		want  string
	}{
		{checksandalerts.Event{CheckGroup: "services", Check: "no_systemd_unit_failed", Finding: "backup.service"}, "services/no_systemd_unit_failed: backup.service"},
		{checksandalerts.Event{CheckGroup: "services", Check: "ssh_server_answers"}, "services/ssh_server_answers"},
		{checksandalerts.Event{CheckGroup: "every_check_group"}, "every_check_group"},
		{checksandalerts.Event{}, ""},
	} {
		if got := subjectOfEvent(c.event); got != c.want {
			t.Errorf("subjectOfEvent(%+v) = %q, want %q", c.event, got, c.want)
		}
	}
}

func TestFleetPrintsTheDestinationAndItsError(t *testing.T) {
	now := time.Now().UTC()
	view := fleet.Fleet{Nodes: []fleet.FleetNode{{Name: "app-1", State: "reporting", ExitCode: 3, NodeStatusReport: &fleet.NodeStatusReport{Delivery: store.DeliverySummary{FailingDestinations: []store.FailingDestination{{Destination: "on_call", UndeliveredAlerts: 1, LatestAttemptError: "receiver refused the alert"}}}}}}, ExitCode: 3}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	output, err := capture(t, func() error { return printFleet(encoded, false, now) })
	var exit exitStatus
	if !errors.As(err, &exit) || exit != 3 {
		t.Fatal(err)
	}
	for _, want := range []string{"0 healthy", "app-1", "Destination on_call:", "receiver refused the alert"} {
		if !strings.Contains(output, want) {
			t.Fatal(want, output)
		}
	}
}

func TestHelpListsFleetJoinCodeAndEventTimeOptions(t *testing.T) {
	output, err := capture(t, func() error { return run([]string{"--help"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mmwatchdog fleet join-code [--json]", "mmwatchdog events [CHECK_GROUP[/CHECK]] [--last DURATION | --since TIME [--until TIME]]", "mmwatchdog fleet [NODE [--last DURATION | --since TIME [--until TIME]]]"} {
		if !strings.Contains(output, want) {
			t.Fatal(want, output)
		}
	}
}

func TestOversizedEventSpanMessage(t *testing.T) {
	t.Parallel()
	socket := filepath.Join(os.TempDir(), "mmwatchdog-"+incidents.ID()+".sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat(" ", 16*1024*1024+1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, payload) }))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	for _, path := range []string{"/events?check_group=app", "/fleet?node=app-1"} {
		_, err := request(context.Background(), socket, http.MethodGet, path, nil)
		if err == nil || err.Error() != "the events in the requested time span exceed 16 MiB; request a shorter time span with --since and --until or --last" {
			t.Fatal(path, err)
		}
	}
}

// A walk-through agent ran mmwatchdog events GROUP/CHECK and got an empty
// answer, as if nothing had happened. The command asks for the check
// group's events and keeps the check's, and an empty answer says so.
func TestEventsOfOneCheckAndAnEmptyAnswer(t *testing.T) {
	socket := filepath.Join(os.TempDir(), "mmwatchdog-"+incidents.ID()+".sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	groups := make(chan string, 10)
	at := time.Date(2026, 11, 17, 23, 11, 36, 0, time.UTC)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		groups <- r.URL.Query().Get("check_group")
		if r.URL.Query().Get("check_group") != "notes_app" {
			json.NewEncoder(w).Encode([]checksandalerts.Event{})
			return
		}
		json.NewEncoder(w).Encode([]checksandalerts.Event{
			{ID: "1", Node: "app01", CheckGroup: "notes_app", Check: "notes_service_is_running", Type: "failure", Message: "systemd unit notes.service is inactive", At: at},
			{ID: "2", Node: "app01", CheckGroup: "notes_app", Check: "health_page_answers", Type: "failure", Message: "connection refused", At: at},
		})
	}))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()

	out, err := capture(t, func() error { return run([]string{"events", "notes_app/notes_service_is_running", "--socket", socket}) })
	if err != nil {
		t.Fatal(err)
	}
	if group := <-groups; group != "notes_app" {
		t.Fatal(group)
	}
	if !strings.Contains(out, "notes.service is inactive") || strings.Contains(out, "connection refused") {
		t.Fatal(out)
	}
	out, err = capture(t, func() error {
		return run([]string{"events", "notes_app/notes_service_is_running", "--socket", socket, "--json"})
	})
	<-groups
	var events []checksandalerts.Event
	if err != nil || json.Unmarshal([]byte(out), &events) != nil || len(events) != 1 || events[0].ID != "1" {
		t.Fatal(err, out)
	}
	for _, tc := range []struct{ name, text string }{
		{"notes_app/no_such_check", "No events of the check notes_app/no_such_check in the node's event history."},
		{"notes", "No events of the check group notes in the node's event history."},
	} {
		out, err = capture(t, func() error { return run([]string{"events", tc.name, "--socket", socket}) })
		<-groups
		if err != nil || !strings.Contains(out, tc.text) {
			t.Fatal(tc.name, err, out)
		}
	}
}
