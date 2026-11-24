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
		// The address of the alert source for nodes sending without the
		// hub, when it states its own, is the one that joined nodes use.
		addresses := map[string]string{"mmhotline_url": destination.URL}
		if b := destination.AlertSourceForNodesSendingWithoutTheHub; b != nil && b.URL != "" {
			addresses = map[string]string{checksandalerts.MMHotlineNodeAlertSourceBlock + ".mmhotline_url": b.URL}
		}
		for _, setting := range checksandalerts.Names(addresses) {
			// Validation owns invalid addresses.
			address, err := url.Parse(addresses[setting])
			if err != nil {
				continue
			}
			host := address.Hostname()
			if !strings.EqualFold(host, "localhost") && !net.ParseIP(host).IsLoopback() {
				continue
			}
			warnings = append(warnings, c.ErrorAt("destinations."+name+".mmhotline."+setting, "joined nodes send alerts to the mmhotline_url address when the hub does not accept an alert in time. On a joined node, 127.0.0.1 points to the joined node. Set mmhotline_url to an address that every node can reach").Error())
		}
	}
	return warnings
}

// OlderJoinedNodesWithNodeAlertSource is, on a hub, a validation warning
// for each mmhotline destination that states
// alert_source_for_nodes_sending_without_the_hub while some joined nodes
// report a release older than 0.2.0 (versions, by node name, with
// olderThan0_2 deciding). Such a node refuses the fleet settings that
// carry the block, and keeps its earlier copy of them.
func (c *Config) OlderJoinedNodesWithNodeAlertSource(versions map[string]string, olderThan0_2 func(string) bool) []string {
	warnings := []string{}
	if c.FleetHubRole == nil {
		return warnings
	}
	older := []string{}
	for _, node := range checksandalerts.Names(versions) {
		if olderThan0_2(versions[node]) {
			older = append(older, node+" ("+versions[node]+")")
		}
	}
	if len(older) == 0 {
		return warnings
	}
	for _, name := range checksandalerts.Names(c.Alerts.Destinations) {
		if p := c.Alerts.Destinations[name].MMHotline(); p == nil || p.AlertSourceForNodesSendingWithoutTheHub == nil {
			continue
		}
		warnings = append(warnings, c.ErrorAt("destinations."+name+".mmhotline."+checksandalerts.MMHotlineNodeAlertSourceBlock, "joined nodes older than MMWatchdog 0.2.0 refuse fleet settings with this block and keep their earlier copy of the fleet settings, so they send alerts without the hub to the destinations of that copy. Upgrade these joined nodes to 0.2.0 or later: "+strings.Join(older, ", ")).Error())
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
