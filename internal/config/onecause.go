package config

import (
	"fmt"
	"slices"
	"strings"

	"mmwatchdog/internal/checksandalerts"
)

// One cause, one alert. A check can name the checks of its causes with
// no_alerts_or_recovery_actions_while_these_checks_fail. While one of the
// named checks is in its failing time, the check that has the setting is
// kept quiet: MMWatchdog sends no alert for it and takes no recovery action because
// of it. The check keeps running, and its state keeps moving as in
// maintenance, without the alerts. The rules are in docs/changing-mmwatchdog/design/checks.md
// under "One cause, one alert" and in docs/changing-mmwatchdog/implementation-rules.md under
// "One rule for a check that is kept quiet".

// quietWhileChecksFailSetting is the name of the setting.
const quietWhileChecksFailSetting = "no_alerts_or_recovery_actions_while_these_checks_fail"

// QuietChecksNamedInAnAlert is how many of the checks that a failing check
// keeps quiet its alert names; it counts all of them.
const QuietChecksNamedInAnAlert = 20

// checkNamed returns the check that key names as CHECK_GROUP/CHECK: a named
// check, a direct check under the name of its kind, or the heartbeat of a
// heartbeat check group as CHECK_GROUP/heartbeat.
func (c *Config) checkNamed(key string) (checksandalerts.Check, bool) {
	group, check, _ := strings.Cut(key, "/")
	ch, exists := c.CheckGroups[group]
	if !exists {
		return checksandalerts.Check{}, false
	}
	if ch.Heartbeat != nil {
		return ch.Check, check == "heartbeat"
	}
	co, exists := ch.Checks[check]
	return co.Check, exists
}

// validateChecksNamedAsCauses checks the setting
// no_alerts_or_recovery_actions_while_these_checks_fail of every check,
// once every check group is known. Each name is a check of this node that
// can fail, written as CHECK_GROUP/CHECK. Checks that name each other in a
// circle are refused: while all of them fail, each would keep the next one
// quiet, and MMWatchdog would send no alert at all.
func (c *Config) validateChecksNamedAsCauses() error {
	causes, fields := map[string][]string{}, map[string]string{}
	for _, group := range checksandalerts.Names(c.CheckGroups) {
		for _, check := range checksandalerts.Names(c.CheckGroups[group].Checks) {
			co := c.CheckGroups[group].Checks[check]
			base := "check_groups." + group + ".checks." + check
			if _, ok := c.Sources[base]; !ok {
				base = "check_groups." + group // a direct check
			}
			field := base + "." + quietWhileChecksFailSetting
			named := co.NoAlertsOrRecoveryActionsWhileTheseChecksFail
			if len(named) == 0 {
				if c.authored(field) {
					return c.ErrorAt(field, "cannot be empty: name a check as CHECK_GROUP/CHECK, or remove "+quietWhileChecksFailSetting)
				}
				continue
			}
			if co.RecordsOnly() {
				return c.ErrorAt(field, "a "+co.ModuleName()+" check that only records its readings never alerts; remove "+quietWhileChecksFailSetting)
			}
			own := group + "/" + check
			seen := map[string]bool{}
			for _, name := range named {
				namedGroup, namedCheck, slash := strings.Cut(name, "/")
				cause, exists := c.checkNamed(name)
				switch {
				case !slash || !checksandalerts.NamePattern.MatchString(namedGroup) || !checksandalerts.NamePattern.MatchString(namedCheck):
					return c.ErrorAt(field, fmt.Sprintf("%q is not written as CHECK_GROUP/CHECK, such as postgres/postgres_service_is_running", name))
				case name == own:
					return c.ErrorAt(field, "names the check itself")
				case seen[name]:
					return c.ErrorAt(field, fmt.Sprintf("names the check %q twice", name))
				case !exists:
					return c.ErrorAt(field, fmt.Sprintf("this node has no check named %q: %s", name, c.checksOfGroup(namedGroup)))
				case cause.RecordsOnly():
					return c.ErrorAt(field, fmt.Sprintf("the check %q only records readings and never fails: no_alerts_or_recovery_actions_while_these_checks_fail can name only checks with an alert_severity", name))
				}

				seen[name] = true
			}
			causes[own], fields[own] = named, field
		}
	}
	for _, start := range checksandalerts.Names(causes) {
		if circle := circleBackTo(start, causes); circle != nil {
			return c.ErrorAt(fields[start], "the checks "+strings.Join(circle, " -> ")+" name each other in a circle: while all of them fail, MMWatchdog would send no alert for any of them")
		}
	}
	if c.JoinFleetHub == nil {
		return c.ValidateCauseDestinations(c.Alerts.Destinations)
	}
	return nil
}

// CheckThatNamesACheckWithALowerAlertSeverity is a check that names, in its
// no_alerts_or_recovery_actions_while_these_checks_fail, a check with a lower
// alert_severity, for mmwatchdog validate.
type CheckThatNamesACheckWithALowerAlertSeverity struct {
	CheckGroup    string `json:"check_group"`
	Check         string `json:"check"`
	AlertSeverity string `json:"alert_severity"`
	// NamedCheck is the named check, as CHECK_GROUP/CHECK.
	NamedCheck              string `json:"named_check"`
	NamedCheckAlertSeverity string `json:"named_check_alert_severity"`
}

// ChecksThatNameACheckWithALowerAlertSeverity names every check that names
// a check with a lower alert_severity as its cause. While the named check
// fails, the one alert that MMWatchdog sends is the named check's, at the named
// check's severity, so the failure of the quiet check reaches only the
// destinations that take the lower severity. It is a validation warning: the
// configuration stays valid.
func (c *Config) ChecksThatNameACheckWithALowerAlertSeverity() []CheckThatNamesACheckWithALowerAlertSeverity {
	out := []CheckThatNamesACheckWithALowerAlertSeverity{}
	for _, group := range checksandalerts.Names(c.CheckGroups) {
		for _, check := range checksandalerts.Names(c.CheckGroups[group].Checks) {
			co := c.CheckGroups[group].Checks[check]
			for _, name := range co.NoAlertsOrRecoveryActionsWhileTheseChecksFail {
				if named, exists := c.checkNamed(name); exists && SeverityRank(named.AlertSeverity) < SeverityRank(co.AlertSeverity) {
					out = append(out, CheckThatNamesACheckWithALowerAlertSeverity{group, check, co.AlertSeverity, name, named.AlertSeverity})
				}
			}
		}
	}
	return out
}

// checksOfGroup says which checks a check group has, for the error about a
// name that matches none of them.
func (c *Config) checksOfGroup(group string) string {
	ch, exists := c.CheckGroups[group]
	switch {
	case !exists:
		return "there is no check group " + group
	case ch.Heartbeat != nil:
		return "the check of the heartbeat check group " + group + " is " + group + "/heartbeat"
	}
	keys := []string{}
	for _, check := range checksandalerts.Names(ch.Checks) {
		keys = append(keys, group+"/"+check)
	}
	return "the checks of the check group " + group + " are " + strings.Join(keys, ", ")
}

// circleBackTo follows the named causes from start and returns the checks
// of a path that comes back to start, with start at both ends, or nil when
// no path does.
func circleBackTo(start string, causes map[string][]string) []string {
	seen := map[string]bool{start: true}
	var follow func(path []string) []string
	follow = func(path []string) []string {
		for _, next := range causes[path[len(path)-1]] {
			if next == start {
				return append(slices.Clone(path), start)
			}
			if seen[next] {
				continue
			}
			seen[next] = true
			if circle := follow(append(slices.Clone(path), next)); circle != nil {
				return circle
			}
		}
		return nil
	}
	return follow([]string{start})
}

// NamedInAnAlert returns the checks that an alert names of those that its
// check keeps quiet, and how many there are in all.
func NamedInAnAlert(checks []string) ([]string, int) {
	if len(checks) > QuietChecksNamedInAnAlert {
		return checks[:QuietChecksNamedInAnAlert], len(checks)
	}
	return checks, len(checks)
}

// ValidateCauseDestinations refuses a silent cause using the joined node's
// copy of the hub's destinations, when that copy is available.
func (c *Config) ValidateCauseDestinations(destinations map[string]checksandalerts.Destination) error {
	if destinations == nil {
		return nil
	}
	definitions := CheckDefinitions(c)
	for _, key := range checksandalerts.Names(definitions) {
		def := definitions[key]
		if !CheckReachesDestination(def.CheckWithSeverity(), destinations) {
			continue
		}
		for _, name := range def.QuietWhileTheseChecksFail {
			cause, exists := c.checkNamed(name)
			if exists && !CheckReachesDestination(cause, destinations) {
				field := "check_groups." + def.CheckGroup + ".checks." + def.Name
				if _, ok := c.Sources[field]; !ok {
					field = "check_groups." + def.CheckGroup
				}
				return c.ErrorAt(field+"."+quietWhileChecksFailSetting, fmt.Sprintf("the cause %q sends alerts to no destination while keeping %q quiet; add a destination for the cause's alert_severity or change its send_alerts_only_to_destinations", name, def.CheckGroup+"/"+def.Name))
			}
		}
	}
	return nil
}
