package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/testhelpers"
)

func TestMMHotlineLoopbackWarningOnlyOnAHub(t *testing.T) {
	t.Parallel()
	for _, address := range []string{"http://localhost:8484", "http://127.0.0.1:8484", "http://127.9.8.7:8484/prefix", "http://[::1]:8484", "https://hotline.example.com", "http://192.0.2.1:8484"} {
		t.Run(address, func(t *testing.T) {
			settings := &config.Config{Alerts: config.Alerts{Destinations: map[string]checksandalerts.Destination{"on_call": {Inputs: &checksandalerts.MMHotlineDestination{URL: address}}}}}
			if warnings := settings.ValidationWarnings("web-3", false).MMHotlineLoopbackWarnings; len(warnings) != 0 {
				t.Fatal("standalone warning", warnings)
			}
			settings.FleetHubRole = &config.FleetHubRole{}
			warnings := settings.ValidationWarnings("hub", false).MMHotlineLoopbackWarnings
			loopback := strings.Contains(address, "localhost") || strings.Contains(address, "127.") || strings.Contains(address, "[::1]")
			if loopback {
				want := "destinations.on_call.mmhotline.mmhotline_url: joined nodes send alerts to the mmhotline_url address when the hub does not accept an alert in time. On a joined node, 127.0.0.1 points to the joined node. Set mmhotline_url to an address that every node can reach"
				if len(warnings) != 1 || !strings.HasSuffix(warnings[0], want) {
					t.Fatal(warnings)
				}
			} else if len(warnings) != 0 {
				t.Fatal(warnings)
			}
		})
	}
}

// A node that delivers its own alerts and has only mmhotline destinations
// is warned that an alert MMHotline permanently refuses reaches nobody; one
// other destination, or a joined node, gets no warning.
func TestMMHotlineWithoutAnotherDestinationWarning(t *testing.T) {
	t.Parallel()
	hotline := checksandalerts.Destination{Inputs: &checksandalerts.MMHotlineDestination{URL: "https://hotline.example.com"}}
	settings := &config.Config{Alerts: config.Alerts{Destinations: map[string]checksandalerts.Destination{"on_call": hotline, "other_team": hotline}}}
	warnings := settings.ValidationWarnings("web-3", false).MMHotlineWithoutAnotherDestination
	want := "when MMHotline permanently refuses an alert, MMWatchdog sends the refused alert to the node's destinations that do not use the mmhotline module. The node has no destination with another module, so a refused alert, including an SOS, reaches nobody. Add a destination with another module, such as pushover or email"
	if len(warnings) != 2 || !strings.HasSuffix(warnings[0], "destinations.on_call: "+want) || !strings.HasSuffix(warnings[1], "destinations.other_team: "+want) {
		t.Fatal(warnings)
	}
	settings.Alerts.Destinations["pager"] = checksandalerts.Destination{Inputs: &checksandalerts.Webhook{URL: "https://pager.example.com"}}
	if warnings := settings.ValidationWarnings("web-3", false).MMHotlineWithoutAnotherDestination; len(warnings) != 0 {
		t.Fatal("warned with another destination", warnings)
	}
	delete(settings.Alerts.Destinations, "pager")
	settings.JoinFleetHub = &config.JoinFleetHub{}
	if warnings := settings.ValidationWarnings("web-3", false).MMHotlineWithoutAnotherDestination; len(warnings) != 0 {
		t.Fatal("warned on a joined node", warnings)
	}
}

// A hub warns about a destination that states how its module runs: joined
// nodes run it as the account mmwatchdog, from /, with no added environment.
func TestHubWarnsThatJoinedNodesIgnoreDestinationRunSettings(t *testing.T) {
	t.Parallel()
	stated := checksandalerts.Destination{Inputs: &checksandalerts.ModuleCall{Name: "own_pager", StatedRunSettings: true}}
	plain := checksandalerts.Destination{Inputs: &checksandalerts.ModuleCall{Name: "own_pager"}}
	settings := &config.Config{Alerts: config.Alerts{Destinations: map[string]checksandalerts.Destination{"pager": stated, "chat": plain}}}
	if warnings := settings.ValidationWarnings("web-3", false).DestinationRunSettingsThatJoinedNodesIgnore; len(warnings) != 0 {
		t.Fatal("standalone warning", warnings)
	}
	settings.FleetHubRole = &config.FleetHubRole{}
	warnings := settings.ValidationWarnings("hub", false).DestinationRunSettingsThatJoinedNodesIgnore
	if len(warnings) != 1 || !strings.Contains(warnings[0], "destinations.pager: joined nodes run this destination's module as the account mmwatchdog, from /") {
		t.Fatal(warnings)
	}
}

// The alert source for nodes sending without the hub is validated like the
// destination's own: its address (the destination's when it states none),
// its ID, which must differ from the destination's, and its absolute
// signing secret file, which follows the credential permission rule. A
// key that the block does not take is refused.
func TestTheAlertSourceForNodesSendingWithoutTheHubIsValidated(t *testing.T) {
	t.Parallel()
	const block = "destinations.on_call.mmhotline.alert_source_for_nodes_sending_without_the_hub."
	secret := testhelpers.WriteSigningSecret(t)
	for written, problem := range map[string]string{
		"{mmhotline_alert_source_id: nodes, mmhotline_signing_secret_file: SECRET}":                                         "",
		"{mmhotline_url: 'http://10.20.0.2:8484', mmhotline_alert_source_id: nodes, mmhotline_signing_secret_file: SECRET}": "",
		"{mmhotline_url: 'ftp://10.20.0.2', mmhotline_alert_source_id: nodes, mmhotline_signing_secret_file: SECRET}":       block + "mmhotline_url: expected MMHotline's address",
		"{mmhotline_alert_source_id: 'two words', mmhotline_signing_secret_file: SECRET}":                                   block + "mmhotline_alert_source_id: expected the alert_source_id",
		"{mmhotline_alert_source_id: nodes, mmhotline_signing_secret_file: secrets/nodes}":                                  block + "mmhotline_signing_secret_file: must be absolute",
		"{mmhotline_alert_source_id: fleet, mmhotline_signing_secret_file: SECRET}":                                         block + "mmhotline_alert_source_id: must differ from the destination's own mmhotline_alert_source_id",
		"{mmhotline_alert_source_id: nodes, mmhotline_signing_secret_file: SECRET, alert_severities: [warn]}":               "alert_source_for_nodes_sending_without_the_hub",
	} {
		path := testhelpers.ConfigFile(t, "fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'hub.test:8445'}\ncheck_groups: {app: {shell: 'true'}}\n")
		destinations := "destinations: {on_call: {mmhotline: {mmhotline_url: 'http://10.20.0.1:8484', mmhotline_alert_source_id: fleet, mmhotline_signing_secret_file: " + secret + ", alert_source_for_nodes_sending_without_the_hub: " + strings.ReplaceAll(written, "SECRET", secret) + "}, alert_severities: [warn, sos]}}\n"
		if err := os.WriteFile(filepath.Join(path, "alert_destinations.yml"), []byte(destinations), 0600); err != nil {
			t.Fatal(err)
		}
		c, err := config.Load(path)
		if problem == "" && err != nil || problem != "" && (err == nil || !strings.Contains(err.Error(), problem)) {
			t.Errorf("%s: got %v, want %q", written, err, problem)
		}
		if problem == "" && err == nil {
			if b := c.Alerts.Destinations["on_call"].MMHotline().AlertSourceForNodesSendingWithoutTheHub; b == nil || b.AlertSourceID != "nodes" || b.SigningSecretFile != secret {
				t.Errorf("%s: read as %+v", written, b)
			}
		}
	}
	// The block's signing secret file follows the credential permission rule.
	exposed := testhelpers.WriteSigningSecret(t)
	if err := os.Chmod(exposed, 0644); err != nil {
		t.Fatal(err)
	}
	c := &config.Config{Sources: map[string]checksandalerts.SourceLocation{}, Alerts: config.Alerts{Destinations: map[string]checksandalerts.Destination{"on_call": {Inputs: &checksandalerts.MMHotlineDestination{URL: "http://10.20.0.1:8484", AlertSourceID: "fleet", SigningSecretFile: secret,
		AlertSourceForNodesSendingWithoutTheHub: &checksandalerts.MMHotlineAlertSource{AlertSourceID: "nodes", SigningSecretFile: exposed}}}}}}
	if problems := c.SecretFilePermissionErrors(); len(problems) != 1 || !strings.Contains(problems[0].Error(), block+"mmhotline_signing_secret_file") {
		t.Fatal(problems)
	}
}

// On a hub, a loopback address that the block states for the nodes is
// warned about like the destination's own.
func TestTheLoopbackWarningCoversTheAddressOfTheNodeAlertSource(t *testing.T) {
	t.Parallel()
	settings := &config.Config{FleetHubRole: &config.FleetHubRole{}, Alerts: config.Alerts{Destinations: map[string]checksandalerts.Destination{"on_call": {Inputs: &checksandalerts.MMHotlineDestination{URL: "http://127.0.0.1:8484",
		AlertSourceForNodesSendingWithoutTheHub: &checksandalerts.MMHotlineAlertSource{URL: "http://10.20.0.1:8484"}}}}}}
	if warnings := settings.ValidationWarnings("hub", false).MMHotlineLoopbackWarnings; len(warnings) != 0 {
		t.Fatal("the nodes use the block's address", warnings)
	}
	settings.Alerts.Destinations["on_call"].MMHotline().AlertSourceForNodesSendingWithoutTheHub.URL = "http://localhost:8484"
	if warnings := settings.ValidationWarnings("hub", false).MMHotlineLoopbackWarnings; len(warnings) != 1 || !strings.Contains(warnings[0], "destinations.on_call.mmhotline.alert_source_for_nodes_sending_without_the_hub.mmhotline_url: joined nodes send alerts") {
		t.Fatal(warnings)
	}
}

// On a hub, a destination with the block is warned about while a joined
// node reports a release older than 0.2.0, which refuses such fleet
// settings.
func TestAHubWarnsOfOlderJoinedNodesWhileTheNodeAlertSourceIsSet(t *testing.T) {
	t.Parallel()
	older := func(version string) bool { return version == "0.1.3" }
	versions := map[string]string{"app-1": "0.2.0", "app-2": "0.1.3"}
	settings := &config.Config{FleetHubRole: &config.FleetHubRole{}, Alerts: config.Alerts{Destinations: map[string]checksandalerts.Destination{
		"on_call": {Inputs: &checksandalerts.MMHotlineDestination{URL: "http://10.20.0.1:8484", AlertSourceForNodesSendingWithoutTheHub: &checksandalerts.MMHotlineAlertSource{AlertSourceID: "nodes"}}},
		"other":   {Inputs: &checksandalerts.MMHotlineDestination{URL: "http://10.20.0.2:8484"}}}}}
	warnings := settings.OlderJoinedNodesWithNodeAlertSource(versions, older)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "destinations.on_call.mmhotline.alert_source_for_nodes_sending_without_the_hub: joined nodes older than MMWatchdog 0.2.0 refuse fleet settings with this block") || !strings.HasSuffix(warnings[0], ": app-2 (0.1.3)") {
		t.Fatal(warnings)
	}
	if warnings := settings.OlderJoinedNodesWithNodeAlertSource(map[string]string{"app-1": "0.2.0"}, older); len(warnings) != 0 {
		t.Fatal(warnings)
	}
	settings.FleetHubRole = nil
	if warnings := settings.OlderJoinedNodesWithNodeAlertSource(versions, older); len(warnings) != 0 {
		t.Fatal("warned on a standalone node", warnings)
	}
}
