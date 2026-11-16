package daemon

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/testhelpers"
)

// Quiet windows retain their history and suppression, including after a
// restart, but never enter any delivery queue on the node or the hub.
func TestMaintenanceWithoutStartAndEndAlerts(t *testing.T) {
	t.Parallel()
	for _, joined := range []bool{false, true} {
		for _, end := range []string{"off", "expiry"} {
			name := "standalone/" + end
			if joined {
				name = "joined/" + end
			}
			t.Run(name, func(t *testing.T) {
				destination := captureAlerts(t)
				destinations := map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: destination.URL}, TimeLimitPerDeliveryAttempt: "1s"}}
				var node, hub *Daemon
				var directory string
				if joined {
					codeHub, code, _ := startHubWith(t, destinations)
					hub = codeHub
					node = joinedNode(t, code, "app-1")
					node.report(context.Background())
				} else {
					var c *config.Config
					directory, c = daemonConfig(t)
					c.Alerts.Destinations = destinations
					writeConfig(t, directory, c)
					var err error
					node, err = NewDaemon(directory)
					if err != nil {
						t.Fatal(err)
					}
				}
				if !joined {
					defer func() { node.Close() }()
				}
				post := func(action, body string) {
					t.Helper()
					response := httptest.NewRecorder()
					node.routes().ServeHTTP(response, httptest.NewRequest("POST", "/maintenance/"+action+"/app", strings.NewReader(body)))
					if response.Code != 200 {
						t.Fatal(response.Code, response.Body.String())
					}
				}
				post("on", `{"for":"1m","reason":"frequent deploy","without_maintenance_on_and_off_alerts":true}`)
				if !node.suppressed("app") || len(queuedEvents(t, node)) != 0 {
					t.Fatal("quiet start did not suppress checks without queueing alerts")
				}
				status := httptest.NewRecorder()
				node.routes().ServeHTTP(status, httptest.NewRequest("GET", "/status", nil))
				if !strings.Contains(status.Body.String(), `"without_maintenance_on_and_off_alerts":true`) {
					t.Fatal(status.Body.String())
				}
				if joined {
					node.report(context.Background())
					detail, err := hub.hub.Node("app-1", time.Now())
					if err != nil || !detail.NodeStatusReport.Maintenance["app"].WithoutMaintenanceOnAndOffAlerts {
						t.Fatal(detail, err)
					}
					// A quiet window must not hold another window's grouped end alert.
					if hub.hub.MaintenanceGroups(time.Now())["frequent deploy\x00"] {
						t.Fatal("quiet window participates in maintenance alert grouping")
					}
				} else {
					if err := node.Close(); err != nil {
						t.Fatal(err)
					}
					var err error
					node, err = NewDaemon(directory)
					if err != nil {
						t.Fatal(err)
					}
					if !node.snapshot.Maintenance["app"].WithoutMaintenanceOnAndOffAlerts || !node.suppressed("app") {
						t.Fatal("quiet window was lost at restart")
					}
				}
				if end == "off" {
					post("off", `{}`)
				} else {
					node.schedule(context.Background(), node.snapshot.Maintenance["app"].Until.Add(time.Second))
					node.workers.Wait()
				}
				if node.suppressed("app") || len(queuedEvents(t, node)) != 0 {
					t.Fatal("quiet end queued alerts or kept suppression")
				}
				events, err := node.store.CheckGroupEvents("app", 100)
				if err != nil {
					t.Fatal(err)
				}
				count := 0
				for _, event := range events {
					if !strings.HasPrefix(event.Type, "maintenance_") {
						continue
					}
					count++
					if event.Notify || event.Maintenance == nil || !event.Maintenance.WithoutMaintenanceOnAndOffAlerts {
						t.Fatal(event)
					}
				}
				if count != 2 {
					t.Fatal("missing start or end history", events)
				}
				if joined {
					node.report(context.Background())
					detail, err := hub.hub.Node("app-1", time.Now())
					if err != nil {
						t.Fatal(err)
					}
					count = 0
					for _, event := range detail.Events {
						if strings.HasPrefix(event.Type, "maintenance_") {
							count++
						}
					}
					if count != 2 {
						t.Fatal("hub lost quiet history", detail.Events)
					}
					if rows, err := hub.store.Intake(); err != nil || len(rows) != 0 {
						t.Fatal("quiet history entered hub intake", rows, err)
					}
				}
				// A notifying event behind the quiet events proves the actual delivery
				// route is usable and delivers no earlier quiet event to the webhook.
				runWorker(t, node)
				if joined {
					runWorker(t, hub)
				}
				setMaintenance(t, node, "app", "10m")
				delivered := destination.wait(t, 1)
				if len(destination.received()) != 1 || delivered[0].Maintenance.WithoutMaintenanceOnAndOffAlerts {
					t.Fatal(destination.received())
				}
				// The events endpoint, used by mmwatchdog events, also keeps the quiet choice.
				response := httptest.NewRecorder()
				node.routes().ServeHTTP(response, httptest.NewRequest("GET", "/events?check_group=app", nil))
				var history []checksandalerts.Event
				if err := json.Unmarshal(response.Body.Bytes(), &history); err != nil {
					t.Fatal(err)
				}
				quiet := 0
				for _, event := range history {
					if event.Maintenance != nil && event.Maintenance.WithoutMaintenanceOnAndOffAlerts {
						quiet++
					}
				}
				if quiet != 2 {
					t.Fatal(response.Body.String())
				}
			})
		}
	}
}

func TestQuietMaintenanceHoldsFailureUntilItsEnd(t *testing.T) {
	t.Parallel()
	directory, _ := heartbeatConfig(t)
	node, err := NewDaemon(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	response := httptest.NewRecorder()
	node.routes().ServeHTTP(response, httptest.NewRequest("POST", "/maintenance/on/backup", strings.NewReader(`{"for":"1h","without_maintenance_on_and_off_alerts":true}`)))
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	node.snapshot.Checks["backup/heartbeat"].Deadline = time.Now().Add(-time.Second)
	node.schedule(context.Background(), time.Now())
	node.workers.Wait()
	if got := queued(t, node); len(got) != 0 {
		t.Fatal("failure escaped maintenance", got)
	}
	setMaintenance(t, node, "backup", "off")
	if got := queued(t, node); len(got) != 1 || got[0] != "failed" {
		t.Fatal("failure did not resume after quiet maintenance", got)
	}
}
