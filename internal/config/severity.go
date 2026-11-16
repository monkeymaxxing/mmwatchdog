package config

import (
	"fmt"
	"slices"
	"time"

	"mmwatchdog/internal/checksandalerts"
)

// Alert severity: every alert carries one, destinations can take only
// chosen severities, and an open incident's severity can rise, never fall.
// The rules are in docs/changing-mmwatchdog/design/alerts.md under "Alert severity".

// alertSeverities are the levels, lowest first, in MMHotline's words.
var alertSeverities = []string{"info", "warn", "sos"}

// SeverityRank orders the levels; an unknown or empty level ranks lowest.
func SeverityRank(severity string) int { return slices.Index(alertSeverities, severity) }

// HigherSeverity returns the higher of two levels.
func HigherSeverity(a, b string) string {
	if SeverityRank(b) > SeverityRank(a) {
		return b
	}
	return a
}

// HighestSeverity returns the highest level in severities, or "".
func HighestSeverity(severities []string) string {
	highest := ""
	for _, s := range severities {
		highest = HigherSeverity(highest, s)
	}
	return highest
}

// validateSeverity checks the explicit choice between recording readings
// and alerting. A measuring module that can fail without a numeric limit
// may state an alert severity without a limit.
func (c *Config) validateSeverity(path string, co checksandalerts.Check) error {
	module := co.Module()
	if co.OnlyRecordReadings {
		if !module.Measures {
			return c.ErrorAt(path+".only_record_readings", "only a measurement check can record readings; remove only_record_readings")
		}
		if co.HasLimit() || co.AlertSeverity != "" || co.RaiseToSosAfterFailingFor != "" {
			return c.ErrorAt(path+".only_record_readings", "a check that only records readings has no limits or alert severity; remove only_record_readings to alert")
		}
		return nil
	}
	if module.Measures && co.AlertSeverity == "" {
		return c.ErrorAt(path+".only_record_readings", "is required without alert_severity: choose whether to record or alert; add only_record_readings: true, or add alert_severity: warn and a limit for a module that needs one")
	}
	if module.Measures && !module.FailsWithoutALimit && !co.HasLimit() {
		return c.ErrorAt(path+".alert_severity", "a "+module.Name+" check needs a limit to alert; add fail_when_measurement_above or fail_when_measurement_below, or replace alert_severity with only_record_readings: true")
	}
	if SeverityRank(co.AlertSeverity) < 0 {
		return c.ErrorAt(path+".alert_severity", "required: info, warn, or sos")
	}
	if co.RaiseToSosAfterFailingFor != "" || c.authored(path+".raise_to_sos_after_failing_for") {
		if v, e := checksandalerts.ParseDuration(string(co.RaiseToSosAfterFailingFor)); e != nil || v < time.Second {
			return c.ErrorAt(path+".raise_to_sos_after_failing_for", "expected a duration of at least 1s")
		}
	}
	return nil
}

// validateNamedDestinations checks send_alerts_only_to_destinations of the
// check at path. On a standalone node and on the hub every name must be a
// destination in alert_destinations.yml, so a mistyped name is an error and
// never a lost alert. A node that has joined a hub gets its destinations
// from the hub, so its names are checked when an alert goes out (GoesTo).
func (c *Config) validateNamedDestinations(path string, co checksandalerts.Check) error {
	if co.SendAlertsOnlyToDestinations == nil {
		return nil
	}
	field := path + ".send_alerts_only_to_destinations"
	if co.RecordsOnly() {
		return c.ErrorAt(field, "a "+co.ModuleName()+" check that only records its readings never alerts; remove send_alerts_only_to_destinations")
	}
	return c.validateDestinationNames(field, co.SendAlertsOnlyToDestinations)
}

// DestinationNamesProblem checks the names supplied by configuration or
// another machine, without needing that machine's destinations.
func DestinationNamesProblem(named *[]string) string {
	if named == nil {
		return ""
	}
	seen := map[string]bool{}
	for _, name := range *named {
		switch {
		case !checksandalerts.NamePattern.MatchString(name):
			return fmt.Sprintf("%q is not a valid destination name", name)
		case seen[name]:
			return fmt.Sprintf("names the destination %q twice", name)
		}
		seen[name] = true
	}
	return ""
}

func (c *Config) validateDestinationNames(field string, named *[]string) error {
	if problem := DestinationNamesProblem(named); problem != "" {
		return c.ErrorAt(field, problem)
	}
	if c.JoinFleetHub == nil {
		if unknown := UnknownDestinationNames(named, c.Alerts.Destinations); len(unknown) > 0 {
			return c.ErrorAt(field, fmt.Sprintf("alert_destinations.yml has no destination named %q", unknown[0]))
		}
	}
	return nil
}

// ValidSeverityList reports distinct, valid severities, at most one of each.
func ValidSeverityList(severities []string) bool {
	seen := map[string]bool{}
	for _, s := range severities {
		if SeverityRank(s) < 0 || seen[s] {
			return false
		}
		seen[s] = true
	}
	return true
}

// UnknownDestinationNames returns the destinations a check names that
// destinations does not have, for status on a node that has joined a hub.
func UnknownDestinationNames(named *[]string, destinations map[string]checksandalerts.Destination) []string {
	unknown := []string{}
	if named == nil {
		return unknown
	}
	for _, name := range *named {
		if _, exists := destinations[name]; !exists {
			unknown = append(unknown, name)
		}
	}
	return unknown
}

// GoesTo is the one rule for which destination gets an alert. An alert of a
// check that names its destinations goes to exactly those, whatever their
// alert_severities say: the failure, a raise, the recovery, a cannot-check
// alert, and every reminder. So does a notice that its command sent to one
// destination by name. Every other alert goes by severity
// (Destination.takes): the alerts of checks that name no destinations, and
// the maintenance, reboot, and other notice alerts, which belong to no check.
func GoesTo(name string, d checksandalerts.Destination, message checksandalerts.Event, destinations map[string]checksandalerts.Destination) bool {
	if named, ok := message.NamedDestinations(destinations); ok {
		return slices.Contains(named, name)
	}
	return d.Takes(message)
}

// SilentNodesReachSosThreshold reports whether simultaneous silence reaches
// the configured count or share of joined nodes.
func (h *FleetHubRole) SilentNodesReachSosThreshold(silent, joined int) bool {
	if silent == 0 {
		return false
	}
	if h.RaiseToSosWhenSilentNodesReachCount != nil {
		return silent >= *h.RaiseToSosWhenSilentNodesReachCount
	}
	return h.RaiseToSosWhenSilentNodesReachPercent != nil && float64(silent)*100 >= *h.RaiseToSosWhenSilentNodesReachPercent*float64(joined)
}

// CheckWithoutAlertDestination identifies a check whose configured severity
// reaches no destination, for validation text and JSON.
type CheckWithoutAlertDestination struct {
	CheckGroup    string `json:"check_group"`
	Check         string `json:"check"`
	AlertSeverity string `json:"alert_severity"`
}

// CheckWithUnknownAlertDestinations identifies names absent from the hub's copy.
type CheckWithUnknownAlertDestinations struct {
	CheckGroup              string   `json:"check_group"`
	Check                   string   `json:"check"`
	UnknownDestinationNames []string `json:"unknown_destination_names"`
}

// CheckReachesDestination reports that an alert of the check goes to at
// least one of the destinations.
func CheckReachesDestination(check checksandalerts.Check, destinations map[string]checksandalerts.Destination) bool {
	if check.RecordsOnly() {
		return false
	}
	event := checksandalerts.Event{AlertSeverity: check.AlertSeverity, SendAlertsOnlyToDestinations: check.SendAlertsOnlyToDestinations}
	for name, destination := range destinations {
		if GoesTo(name, destination, event, destinations) {
			return true
		}
	}
	return false
}

// RoutingWarnings checks the destinations currently known, including a joined
// node's saved copy of its hub's settings. nil means no copy is available.
func (c *Config) RoutingWarnings(destinations map[string]checksandalerts.Destination, warnings *ValidationWarnings) {
	if destinations == nil {
		return
	}
	definitions := CheckDefinitions(c)
	for _, key := range checksandalerts.Names(definitions) {
		def := definitions[key]
		check := def.CheckWithSeverity()
		if c.JoinFleetHub != nil {
			if unknown := UnknownDestinationNames(check.SendAlertsOnlyToDestinations, destinations); len(unknown) > 0 {
				warnings.ChecksWithUnknownAlertDestinations = append(warnings.ChecksWithUnknownAlertDestinations, CheckWithUnknownAlertDestinations{def.CheckGroup, def.Name, unknown})
			}
		}
		if (check.AlertSeverity == "warn" || check.AlertSeverity == "sos") && !check.RecordsOnly() && !CheckReachesDestination(check, destinations) {
			warnings.ChecksWithoutAlertDestination = append(warnings.ChecksWithoutAlertDestination, CheckWithoutAlertDestination{def.CheckGroup, def.Name, check.AlertSeverity})
		}
	}
}
