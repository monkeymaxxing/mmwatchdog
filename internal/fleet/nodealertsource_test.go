package fleet

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/testhelpers"
)

// A fleet whose mmhotline destination has no alert source for nodes
// sending without the hub keeps the fleet settings version that MMWatchdog
// 0.1.3 computes for the same settings, so its 0.1.x nodes take them.
// The expected version is what v0.1.3's ComputeVersion returns for this
// value.
func TestAFleetWithoutTheNodeAlertSourceKeepsItsSettingsVersion(t *testing.T) {
	t.Parallel()
	var destinations map[string]checksandalerts.Destination
	if e := json.Unmarshal([]byte(`{"team_hotline":{"alert_severities":["warn","sos"],"mmhotline":{"mmhotline_url":"http://10.20.0.1:8484","mmhotline_alert_source_id":"mmwatchdog_fleet","mmhotline_signing_secret_file":"/etc/mmwatchdog/secrets/mmhotline-fleet"}}}`), &destinations); e != nil {
		t.Fatal(e)
	}
	s := FleetSettings{Destinations: destinations, FileContentsByDestination: map[string]map[string]string{"team_hotline": {"mmhotline_signing_secret_file": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}},
		Fleet: config.FleetTimings{ReportEvery: "30s", AlertAfterHubUnreachableFor: "3m", SendAlertWithoutHubAfterWaitingFor: "30s"}, AlertSeverityWhenTheHubGoesSilent: "sos"}
	if got := s.ComputeVersion(); got != "93607076c5b12a8d4151e1dea2f160a658ed1b338354cdc976e5a9a2a59261cb" {
		t.Fatal("the fleet settings version changed for a fleet without the block:", got)
	}
}

// The block travels to joined nodes in the fleet settings with the content
// of its signing secret file, like the destination's own secret, and
// changes the version. A joined node checks a copy: one without the
// block's secret is refused.
func TestTheNodeAlertSourceTravelsInTheFleetSettings(t *testing.T) {
	t.Parallel()
	nodeSecret := strings.Repeat("ab", 32)
	hub := &config.Config{FleetHubRole: &config.FleetHubRole{FleetTimings: config.FleetTimings{ReportEvery: "30s", AlertAfterHubUnreachableFor: "3m", SendAlertWithoutHubAfterWaitingFor: "30s"}}, AlertSeverityWhenThisNodeGoesSilent: "sos",
		Sources: map[string]checksandalerts.SourceLocation{}, Alerts: config.Alerts{Destinations: map[string]checksandalerts.Destination{"team_hotline": {AlertSeverities: []string{"warn", "sos"}, Inputs: &checksandalerts.MMHotlineDestination{URL: "http://10.20.0.1:8484", AlertSourceID: "mmwatchdog_fleet", SigningSecretFile: testhelpers.WriteSigningSecret(t)}}}}}
	without, e := NewFleetSettings(hub)
	if e != nil {
		t.Fatal(e)
	}
	hub.Alerts.Destinations["team_hotline"].MMHotline().AlertSourceForNodesSendingWithoutTheHub = &checksandalerts.MMHotlineAlertSource{AlertSourceID: "mmwatchdog_nodes", SigningSecretFile: testhelpers.WriteToken(t, nodeSecret+"\n")}
	with, e := NewFleetSettings(hub)
	if e != nil {
		t.Fatal(e)
	}
	contents := with.FileContentsByDestination["team_hotline"]
	if with.Version == without.Version || contents["mmhotline_signing_secret_file"] != hex.EncodeToString(testhelpers.TestSigningSecret) || contents["alert_source_for_nodes_sending_without_the_hub.mmhotline_signing_secret_file"] != nodeSecret {
		t.Fatalf("version %s against %s, contents %v", with.Version, without.Version, contents)
	}
	b, _ := json.Marshal(with)
	var copied FleetSettings
	if e = json.Unmarshal(b, &copied); e != nil {
		t.Fatal(e)
	}
	if e = copied.Validate(nil); e != nil {
		t.Fatal(e)
	}
	if source, input, known := copied.Destinations["team_hotline"].MMHotline().AlertSourceNamed("mmwatchdog_nodes"); !known || source.AlertSourceID != "mmwatchdog_nodes" || source.URL != "http://10.20.0.1:8484" || copied.FileContentsByDestination["team_hotline"][input] != nodeSecret {
		t.Fatalf("%+v %s", source, input)
	}
	delete(copied.FileContentsByDestination["team_hotline"], "alert_source_for_nodes_sending_without_the_hub.mmhotline_signing_secret_file")
	if e = copied.Validate(nil); e == nil {
		t.Fatal("a copy without the block's secret was taken")
	}
}

// Only a release older than the one asked names an older release; a
// development build names none.
func TestReleaseOlderThan(t *testing.T) {
	t.Parallel()
	for version, older := range map[string]bool{"0.1.3": true, "v0.1.0": true, "0.2.0": false, "0.10.0": false, "1.0.0": false, "0.1.0-dev": false, "": false, "0.1": false, "0.01.3": false} {
		if ReleaseOlderThan(version, 0, 2) != older {
			t.Errorf("%q", version)
		}
	}
}
