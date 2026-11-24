package daemon

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/testhelpers"
)

// A hub whose mmhotline destination states an alert source for nodes
// sending without the hub gives its joined nodes that source's signing
// secret with the fleet settings. A joined node whose alert the hub does
// not accept sends it straight to MMHotline under that source, signed with
// its secret, and records the source for its reports. Check-ins stay on the
// hub's own source. While a joined node reports a release older than
// 0.2.0, validation on the hub warns.
func TestAJoinedNodeSendsUnderTheAlertSourceForNodesSendingWithoutTheHub(t *testing.T) {
	f := testhelpers.NewFakeMMHotline(t, "ok")
	nodeSecret := bytes.Repeat([]byte{0x3c}, 32)
	f.AddAlertSource("mmwatchdog_nodes", nodeSecret)
	destinations := mmhotlineDestinations(t, f.URL)
	inputs := destinations[mmhotlineDestinationName].MMHotline()
	inputs.AlertSourceForNodesSendingWithoutTheHub = &checksandalerts.MMHotlineAlertSource{AlertSourceID: "mmwatchdog_nodes", SigningSecretFile: testhelpers.WriteToken(t, string(bytes.Repeat([]byte("3c"), 32))+"\n")}
	hub, code, srv := startHubWith(t, destinations)
	hub.sendCheckIn(context.Background(), time.Now().UTC())
	node := joinedNode(t, code, "app-1")
	node.Build = fleet.Build{Version: "0.1.3"}
	node.report(context.Background())
	if warnings := ValidationWarnings(hub.config, hub.store.Node, true).OlderJoinedNodesWithAlertSourceForNodesSendingWithoutTheHub; len(warnings) != 1 || !strings.HasSuffix(warnings[0], ": app-1 (0.1.3)") {
		t.Fatal("no warning of the older joined node", warnings)
	}
	node.Build = fleet.Build{Version: "0.2.0"}
	node.report(context.Background())
	if warnings := ValidationWarnings(hub.config, hub.store.Node, true).OlderJoinedNodesWithAlertSourceForNodesSendingWithoutTheHub; len(warnings) != 0 {
		t.Fatal(warnings)
	}
	node.fleetSettings.Fleet.SendAlertWithoutHubAfterWaitingFor = "300ms"
	srv.Close()
	incident := incidents.ID()
	recordEvents(t, node, checksandalerts.Event{ID: incidents.ID(), Incident: incident, CheckGroup: "app", Check: "shell", Type: "failed", AlertSeverity: "sos", At: time.Now(), Message: "app-1: exit 1", Notify: true})
	runWorker(t, node)
	testhelpers.WaitFor(t, "the alert to leave the hub's queue and the MMHotline queue", func() bool {
		return outboxCount(t, node, config.HubDestination) == 0 && outboxCount(t, node, mmhotlineDestinationName) == 0
	})
	alerts, _, _ := f.Received()
	alertSources, checkInSources := f.ReceivedSources()
	if len(alerts) != 1 || !slices.Equal(alertSources, []string{"mmwatchdog_nodes"}) || alerts[0]["sender_incident_key"] != incidentKeyFor(hub, "app/shell failed") || !strings.HasPrefix(alerts[0]["message"], "exit 1\n") {
		t.Fatalf("%+v under %v", alerts, alertSources)
	}
	if len(checkInSources) == 0 || slices.ContainsFunc(checkInSources, func(s string) bool { return s != testhelpers.TestSourceID }) {
		t.Fatal("check-ins went under", checkInSources)
	}
	if sent, _ := node.store.SentWithoutTheHub(); len(sent) != 1 || sent[0].AlertSource != "mmwatchdog_nodes" {
		t.Fatalf("%+v", sent)
	}
}
