package fleet_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
)

func TestNodeStatusReportValidatesAttentionAlertSeverity(t *testing.T) {
	t.Parallel()
	base := func(severity string) *fleet.NodeStatusReport {
		return &fleet.NodeStatusReport{
			StartID:                             incidents.ID(),
			Seq:                                 1,
			Sent:                                time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC),
			History:                             incidents.ID(),
			After:                               -1,
			MeasurementHistoryID:                incidents.ID(),
			ReadingsAfter:                       -1,
			AlertSeverityWhenThisNodeGoesSilent: "warn",
			Summary: incidents.StatusSummary{
				ExitCode: 1,
				Attention: []incidents.Attention{
					{Check: "app/http", State: "failed", AlertSeverity: severity, Message: "503"},
				},
			},
		}
	}
	// Empty severity must stay accepted for joined nodes one minor release older.
	for _, valid := range []string{"", "info", "warn", "sos"} {
		if err := base(valid).Validate(); err != nil {
			t.Errorf("severity %q should validate: %v", valid, err)
		}
	}
	for _, invalid := range []string{"critical", "warning", "SOS", "error"} {
		err := base(invalid).Validate()
		if err == nil || !strings.Contains(err.Error(), "alert severity") {
			t.Errorf("severity %q: got %v, want alert severity validation error", invalid, err)
		}
	}
}

func TestHubViewSetsSilenceAlertSeverityOnlyForSilentNodes(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	h, err := fleet.OpenHub(t.TempDir(), "hub.test:8445", "util", 30*time.Second, config.DefaultHubLimits, start)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })

	if _, status, err := h.Join("warn-node", h.Code.Secret, config.NodeFleetSettings{AlertSeverityWhenThisNodeGoesSilent: "warn"}, "10.0.0.1", start); status != http.StatusOK || err != nil {
		t.Fatal(status, err)
	}
	if _, status, err := h.Join("sos-node", h.Code.Secret, config.NodeFleetSettings{AlertSeverityWhenThisNodeGoesSilent: "sos"}, "10.0.0.2", start); status != http.StatusOK || err != nil {
		t.Fatal(status, err)
	}
	// warn-node reports once; sos-node never reports.
	r := &fleet.NodeStatusReport{
		StartID:                             incidents.ID(),
		Seq:                                 1,
		Sent:                                start,
		History:                             incidents.ID(),
		After:                               -1,
		MeasurementHistoryID:                incidents.ID(),
		ReadingsAfter:                       -1,
		AlertSeverityWhenThisNodeGoesSilent: "warn",
		Summary:                             incidents.StatusSummary{Attention: []incidents.Attention{}},
	}
	if _, status, err := h.Accept("warn-node", r, start); status != http.StatusOK || err != nil {
		t.Fatal(status, err)
	}

	// While reporting or waiting_for_first_report, silence_alert_severity is empty and omitted from JSON.
	beforeSilence := start.Add(10 * time.Second)
	view := h.Fleet(beforeSilence)
	for _, n := range view.Nodes {
		if n.SilenceAlertSeverity != "" {
			t.Fatalf("%s (%s): unexpected silence_alert_severity %q", n.Name, n.State, n.SilenceAlertSeverity)
		}
		raw, err := json.Marshal(n)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "silence_alert_severity") {
			t.Fatalf("%s (%s): silence_alert_severity should be omitted in JSON: %s", n.Name, n.State, raw)
		}
	}

	// Past the 3*30s silence deadline, silent nodes carry their configured silence severity.
	afterSilence := start.Add(2 * time.Minute)
	silentView := h.Fleet(afterSilence)
	byName := map[string]fleet.FleetNode{}
	for _, n := range silentView.Nodes {
		byName[n.Name] = n
	}
	if byName["warn-node"].State != "silent" || byName["warn-node"].SilenceAlertSeverity != "warn" {
		t.Fatalf("warn-node: %+v", byName["warn-node"])
	}
	if byName["sos-node"].State != "silent" || byName["sos-node"].SilenceAlertSeverity != "sos" {
		t.Fatalf("sos-node: %+v", byName["sos-node"])
	}
}
