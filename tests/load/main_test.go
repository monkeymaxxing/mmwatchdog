package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/daemon"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/testhelpers"
)

func TestFailedReportsInARowAreCountedForEachNode(t *testing.T) {
	failedReport := errors.New("connection: refused")
	first, second := &node{}, &node{}
	// Failures on different nodes do not combine into one node's streak.
	for range 3 {
		first.countReport(failedReport)
		second.countReport(failedReport)
		first.countReport(nil)
		second.countReport(nil)
	}
	for _, s := range []*node{first, second} {
		if s.failedReportsInARow != 0 || s.mostFailedReportsInARow != 1 {
			t.Fatalf("got current=%d, most=%d; want 0, 1", s.failedReportsInARow, s.mostFailedReportsInARow)
		}
	}
	second.countReport(failedReport)
	second.countReport(failedReport)
	if first.mostFailedReportsInARow != 1 || second.mostFailedReportsInARow != 2 {
		t.Fatalf("got node maxima %d, %d; want 1, 2", first.mostFailedReportsInARow, second.mostFailedReportsInARow)
	}
}

func TestASuccessfulReportStartsTheCountOverAndKeepsTheMost(t *testing.T) {
	failedReport := errors.New("connection: refused")
	s := &node{}
	for _, e := range []error{failedReport, failedReport, failedReport, nil, failedReport} {
		s.countReport(e)
	}
	if s.failedReportsInARow != 1 || s.mostFailedReportsInARow != 3 {
		t.Fatalf("got current=%d, most=%d; want 1, 3", s.failedReportsInARow, s.mostFailedReportsInARow)
	}
}

// A real hub daemon accepts everything that a synthetic node sends: its
// join, its first report, a report with readings, and the alert and the
// report with its event when app/http fails and when it recovers. Both
// alerts carry the fleet incident key of the app/http failure.
func TestTheHubAcceptsWhatASyntheticNodeSends(t *testing.T) {
	join := startHub(t)
	s := &node{name: "load-00000", start: incidents.ID(), history: incidents.ID(), after: -1, measurementHistory: incidents.ID(), readingsAfter: -1}
	var joined struct{ Credential string }
	if e := send(join, "/v1/join", "", s.joinRequest(join.Secret), &joined); e != nil || joined.Credential == "" {
		t.Fatalf("join: %v", e)
	}
	s.credential = joined.Credential

	// The first report carries no events and no readings; the reply says
	// that the hub holds none of either.
	report(t, join, s)
	if s.after != 0 || s.readingsAfter != 0 {
		t.Fatalf("after the first report: events cursor %d, readings cursor %d; want 0, 0", s.after, s.readingsAfter)
	}
	report(t, join, s)
	if s.readingsAfter != readingsPerReport {
		t.Fatalf("after a report with readings: readings cursor %d; want %d", s.readingsAfter, readingsPerReport)
	}

	failureKey := checksandalerts.FleetIncidentKey(checksandalerts.ReadableFleetIncidentKey(checksandalerts.Event{CheckGroup: "app", Check: "http"}, "failed", ""))
	for i, failing := range []bool{true, false} {
		s.failing = failing
		alert := s.transition()
		if alert.FleetIncidentKey != failureKey {
			t.Errorf("%s alert: fleet_incident_key %q; want %q, the key of the app/http failure", alert.Type, alert.FleetIncidentKey, failureKey)
		}
		if e := send(join, "/v1/alerts", s.credential, alert, nil); e != nil {
			t.Errorf("%s alert: %v", alert.Type, e)
		}
		// The next report carries the alert as the node's local event.
		report(t, join, s)
		if want := int64(i + 1); s.after != want {
			t.Fatalf("after the report of the %s event: events cursor %d; want %d", alert.Type, s.after, want)
		}
	}
}

// report sends the node's next report as the load generator does, and takes
// the cursors of the hub's reply.
func report(t *testing.T, join fleet.JoinCode, s *node) {
	t.Helper()
	var reply fleet.ReportReply
	if e := send(join, "/v1/report", s.credential, s.report(), &reply); e != nil {
		t.Fatalf("report %d: %v", s.seq, e)
	}
	s.after, s.readingsAfter = reply.EventsCursor, reply.ReadingsCursor
}

// startHub runs a hub daemon on this machine, with the hub's port for nodes
// on 127.0.0.1, and returns its join code as mmwatchdog fleet join-code prints it.
func startHub(t *testing.T) fleet.JoinCode {
	t.Helper()
	address := testhelpers.AddressWhereNothingListens(t, "tcp")
	// Keep the Unix socket below macOS's path-length limit.
	socket := filepath.Join(os.TempDir(), "mmwatchdog-load-"+incidents.ID()+".sock")
	configDir := testhelpers.ConfigFile(t, fmt.Sprintf("state_dir: %s\nsocket: %s\nfleet_hub_role: {listen_on_address: '%s', address_for_nodes: '%s'}\ncheck_groups: {app: {shell: 'true'}}\n",
		filepath.Join(t.TempDir(), "state"), socket, address, address))
	hub, e := daemon.NewDaemon(configDir)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var runError error
	stopped := make(chan struct{})
	go func() { defer close(stopped); runError = hub.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-stopped
		if runError != nil {
			t.Error(runError)
		}
		hub.Close()
	})
	// Run opens the control socket before the hub's port for nodes.
	testhelpers.WaitFor(t, "the hub's port for nodes", func() bool {
		select {
		case <-stopped:
			t.Fatalf("the hub stopped: %v", runError)
		default:
		}
		connection, e := net.Dial("tcp", address)
		if e == nil {
			connection.Close()
		}
		return e == nil
	})
	control := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	defer control.CloseIdleConnections()
	response, e := control.Get("http://mmwatchdog/fleet/join-code")
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	var reply struct {
		JoinCode string `json:"join_code"`
	}
	if e = json.NewDecoder(response.Body).Decode(&reply); e != nil {
		t.Fatal(e)
	}
	join, e := fleet.ParseJoinCode(reply.JoinCode)
	if e != nil {
		t.Fatal(e)
	}
	return join
}
