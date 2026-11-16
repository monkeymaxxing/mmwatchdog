package config_test

import (
	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"strings"
	"testing"
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
