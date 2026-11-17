package config

import (
	"net"
	"net/url"
	"strings"

	"mmwatchdog/internal/checksandalerts"
)

// destinationRunSettingsThatJoinedNodesIgnore names each destination of a
// hub whose module runs with a stated account, folder, or environment,
// which joined nodes leave out.
func (c *Config) destinationRunSettingsThatJoinedNodesIgnore() []string {
	warnings := []string{}
	if c.FleetHubRole == nil {
		return warnings
	}
	for _, name := range checksandalerts.Names(c.Alerts.Destinations) {
		call, notCore := c.Alerts.Destinations[name].Inputs.(*checksandalerts.ModuleCall)
		if !notCore || !call.StatedRunSettings {
			continue
		}
		warnings = append(warnings, c.ErrorAt("destinations."+name, "joined nodes run this destination's module as the account mmwatchdog, from /, with no added environment variables; only this hub uses its run_as_user_account, run_from_directory, and env. Reason: the hub must not choose how programs run on joined nodes").Error())
	}
	return warnings
}

// mmhotlineLoopbackWarnings names hub addresses that joined nodes would
// reach on themselves when they send alerts without the hub.
func (c *Config) mmhotlineLoopbackWarnings() []string {
	warnings := []string{}
	if c.FleetHubRole == nil {
		return warnings
	}
	for _, name := range checksandalerts.Names(c.Alerts.Destinations) {
		destination := c.Alerts.Destinations[name].MMHotline()
		if destination == nil {
			continue
		}
		// Validation owns invalid addresses.
		address, err := url.Parse(destination.URL)
		if err != nil {
			continue
		}
		host := address.Hostname()
		if !strings.EqualFold(host, "localhost") && !net.ParseIP(host).IsLoopback() {
			continue
		}
		warnings = append(warnings, c.ErrorAt("destinations."+name+".mmhotline.mmhotline_url", "joined nodes send alerts to the mmhotline_url address when the hub does not accept an alert in time. On a joined node, 127.0.0.1 points to the joined node. Set mmhotline_url to an address that every node can reach").Error())
	}
	return warnings
}

// mmhotlineWithoutAnotherDestinationWarnings names each mmhotline
// destination of a node that delivers its alerts itself, the hub or a
// standalone node, when the node has no destination that is not an
// mmhotline destination. When MMHotline permanently refuses an alert,
// MMWatchdog sends it to those other destinations; without one, a refused
// SOS reaches nobody.
func (c *Config) mmhotlineWithoutAnotherDestinationWarnings() []string {
	warnings := []string{}
	if c.JoinFleetHub != nil {
		return warnings
	}
	names := checksandalerts.Names(c.Alerts.Destinations)
	for _, name := range names {
		if c.Alerts.Destinations[name].MMHotline() == nil {
			return warnings
		}
	}
	for _, name := range names {
		warnings = append(warnings, c.ErrorAt("destinations."+name, "when MMHotline permanently refuses an alert, MMWatchdog sends the refused alert to the node's destinations that do not use the mmhotline module. The node has no destination with another module, so a refused alert, including an SOS, reaches nobody. Add a destination with another module, such as pushover or email").Error())
	}
	return warnings
}
