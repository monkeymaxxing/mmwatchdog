package fleet

import (
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
)

func FleetKey(name string) string { return checksandalerts.NodeReportsCheckGroup + "/" + name }

// MemberDefinition is a joined node's check on the hub. Its alert severity
// settings are silence: the node's alert_severity_when_this_node_goes_silent
// and raise_to_sos_after_this_node_is_silent_for, from its join and then its
// reports.
func MemberDefinition(name string, joined time.Time, timings config.FleetTimings, silence checksandalerts.AlertSeveritySettings, destinations *[]string) config.Definition {
	within := checksandalerts.Duration((3 * timings.ReportEvery.Value()).String())
	return config.Definition{CheckGroup: checksandalerts.NodeReportsCheckGroup, Name: name, Heartbeat: &checksandalerts.Heartbeat{MaxTimeBetweenHeartbeats: within}, Joined: joined.UTC().Format(time.RFC3339Nano), Severity: silence, SendAlertsOnlyToDestinations: destinations, SeverityFromReports: true}
}
