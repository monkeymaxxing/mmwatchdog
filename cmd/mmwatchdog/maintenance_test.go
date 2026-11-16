package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/incidents"
)

func TestMaintenanceWithoutStartAndEndAlertsCommand(t *testing.T) {
	socket := filepath.Join(os.TempDir(), "mmwatchdog-"+incidents.ID()+".sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	window := checksandalerts.Window{Until: time.Now().Add(time.Hour), WithoutMaintenanceOnAndOffAlerts: true}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/events" {
			json.NewEncoder(w).Encode([]checksandalerts.Event{{Type: "maintenance_on", Maintenance: &window, Message: "maintenance " + window.Describe()}})
			return
		}
		var request struct {
			WithoutMaintenanceOnAndOffAlerts bool `json:"without_maintenance_on_and_off_alerts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !request.WithoutMaintenanceOnAndOffAlerts {
			t.Error("quiet choice did not reach daemon", request, err)
		}
		json.NewEncoder(w).Encode(map[string]any{"maintenance": window})
	}))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	output, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	original := os.Stdout
	os.Stdout = output
	defer func() { os.Stdout = original }()
	if err := run([]string{"maintenance", "on", "app", "--for", "1h", "--without-maintenance-on-and-off-alerts", "--socket", socket}); err != nil {
		t.Fatal(err)
	}
	status, _ := json.Marshal(map[string]any{"maintenance": map[string]checksandalerts.Window{"app": window}})
	if err := printStatus(status, "", false); err != nil {
		t.Fatal(err)
	}
	if err := printStatus(status, "", true); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"events", "--socket", socket}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"events", "--json", "--socket", socket}); err != nil {
		t.Fatal(err)
	}
	output.Seek(0, 0)
	text, _ := os.ReadFile(output.Name())
	for _, expected := range []string{"Maintenance start and end are not announced.", "Maintenance on: app", "; maintenance start and end are not announced", `"without_maintenance_on_and_off_alerts": true`} {
		if !strings.Contains(string(text), expected) {
			t.Fatalf("missing %q in %s", expected, text)
		}
	}
}
