package checksandalerts

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"mmwatchdog/internal/primitives"
)

// Duration is a number with a unit, such as 30s, 5m, 26h, or 7d, kept as
// written so that inspection shows what the author wrote.
type Duration string

func (d Duration) Value() time.Duration { v, _ := ParseDuration(string(d)); return v }

// ParseDuration reads a duration. Days are a whole number followed by d;
// every other form follows Go's syntax, such as 100ms, 30s, or 1h30m.
func ParseDuration(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, e := strconv.ParseUint(days, 10, 64)
		if e != nil || n > uint64(math.MaxInt64/(24*time.Hour)) {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// Size is a whole number with a unit, MiB or GiB, such as 256MiB, kept as
// written so that inspection shows what the author wrote.
type Size string

func (s Size) Bytes() int64 { v, _ := ParseSize(string(s)); return v }

// ParseSize reads a size: a positive whole number followed by MiB or GiB.
func ParseSize(s string) (int64, error) {
	for unit, bytes := range map[string]int64{"MiB": 1024 * 1024, "GiB": 1024 * 1024 * 1024} {
		if number, ok := strings.CutSuffix(s, unit); ok {
			n, e := strconv.ParseInt(number, 10, 64)
			if e != nil || n < 1 || n > math.MaxInt64/bytes || strings.TrimLeft(number, "0123456789") != "" {
				break
			}
			return n * bytes, nil
		}
	}
	return 0, fmt.Errorf("invalid size %q", s)
}

// Timing holds the four timings every check group states.
type Timing struct {
	CheckEvery               Duration `yaml:"check_every" json:"check_every,omitempty"`
	TimeLimitPerRun          Duration `yaml:"time_limit_per_run" json:"time_limit_per_run,omitempty"`
	AlertAfterFailingFor     Duration `yaml:"alert_after_failing_for" json:"alert_after_failing_for,omitempty"`
	RecoveredAfterHealthyFor Duration `yaml:"recovered_after_healthy_for" json:"recovered_after_healthy_for,omitempty"`
}

// Destination is one named alert destination: the inputs of its one module,
// the limit for one delivery attempt, and how often an announced incident
// that is still open is repeated to it, if at all. SameDestination defines
// its identity.
type Destination struct {
	SecretFileAccess `yaml:",inline"`
	// CredentialContentFingerprint captures credentials at activation, without
	// keeping their secrets in queued deliveries. It is never public JSON.
	CredentialContentFingerprint string `yaml:"-" json:"-"`

	// Inputs holds what the destination states for its module: a pointer to
	// a value of the module's input type, which also says which module it
	// is (see destination.go). nil is a destination that names no module.
	Inputs                      destinationInputs `yaml:"-" json:"-"`
	TimeLimitPerDeliveryAttempt Duration          `yaml:"time_limit_per_delivery_attempt" json:"time_limit_per_delivery_attempt,omitempty"`
	RemindEvery                 Duration          `yaml:"remind_every" json:"remind_every,omitempty"`
	// SendOnlyTheAlertTitle gives the destination each alert's title and
	// no body, for a receiver that is short, such as an email address that
	// turns each email into an SMS. See Event.OnlyTheTitle.
	SendOnlyTheAlertTitle bool `yaml:"send_only_the_alert_title" json:"send_only_the_alert_title,omitempty"`
	// AlertSeverities are the severities of the alerts the destination
	// takes from checks that name no destinations; it is required. An
	// empty list is valid: then only the checks that name the destination
	// reach it. See goesTo.
	AlertSeverities []string `yaml:"alert_severities" json:"alert_severities"`
}

// CheckGroup is a named set of checks that share four timings, with an
// optional restart of one systemd unit and an optional recovery command.
type CheckGroup struct {
	Timing             `yaml:",inline"`
	Checks             map[string]NamedCheck `yaml:"checks" json:"checks,omitempty"`
	RestartSystemdUnit *RestartSystemdUnit   `yaml:"restart_systemd_unit" json:"restart_systemd_unit,omitempty"`
	RecoveryCommand    *RecoveryCommand      `yaml:"recovery_command" json:"recovery_command,omitempty"`
	Heartbeat          *Heartbeat            `yaml:"heartbeat" json:"heartbeat,omitempty"`
	// Check holds a direct check, or a heartbeat's alert severity
	// settings: the group mapping is then the check.
	Check `yaml:",inline"`
}

// NamedCheck is a check under checks. It may state its own two waiting
// times; where it states none, those of its check group apply. Validation
// fills in the effective values and where each one is stated, for mmwatchdog config.
// A direct check is written on the group mapping, so the check group's
// waiting times are its own.
type NamedCheck struct {
	Check                            `yaml:",inline"`
	AlertAfterFailingFor             Duration `yaml:"alert_after_failing_for" json:"alert_after_failing_for,omitempty"`
	RecoveredAfterHealthyFor         Duration `yaml:"recovered_after_healthy_for" json:"recovered_after_healthy_for,omitempty"`
	AlertAfterFailingForStatedOn     string   `yaml:"-" json:"alert_after_failing_for_stated_on,omitempty"`
	RecoveredAfterHealthyForStatedOn string   `yaml:"-" json:"recovered_after_healthy_for_stated_on,omitempty"`
}

// WaitingTimes returns the check's two waiting times: its own, or those of
// its check group's timing where it states none.
func (c NamedCheck) WaitingTimes(group Timing) (alertAfterFailingFor, recoveredAfterHealthyFor Duration) {
	alertAfterFailingFor, recoveredAfterHealthyFor = c.AlertAfterFailingFor, c.RecoveredAfterHealthyFor
	if alertAfterFailingFor == "" {
		alertAfterFailingFor = group.AlertAfterFailingFor
	}
	if recoveredAfterHealthyFor == "" {
		recoveredAfterHealthyFor = group.RecoveredAfterHealthyFor
	}
	if c.OpensIncidentAtFirstEvent() {
		alertAfterFailingFor = "0s"
	}
	return alertAfterFailingFor, recoveredAfterHealthyFor
}

// OpensIncidentAtFirstEvent reports the registry policy for these inputs.
func (c Check) OpensIncidentAtFirstEvent() bool {
	module := c.Module()
	return module.OpensIncidentAtFirstEvent != nil && module.OpensIncidentAtFirstEvent(c.Inputs)
}

// Check is one check: the inputs of its one module, the limits MMWatchdog applies to
// the module's measurements, and the check's alert severity settings.
type Check struct {
	SecretFileAccess `yaml:",inline"`
	// Inputs holds what the check states for its module: a pointer to a
	// value of the module's input type, which also says which module it is
	// (see module.go). nil is a check that names no module.
	Inputs CheckInputs `yaml:"-" json:"-"`
	// OnlyRecordReadings explicitly selects readings without alerts or recovery.
	OnlyRecordReadings bool `yaml:"only_record_readings" json:"only_record_readings,omitempty"`
	// FailWhenMeasurementAbove and FailWhenMeasurementBelow map the name of
	// a measurement to its limit: a reading strictly above or strictly
	// below it fails the check. See applyLimits.
	FailWhenMeasurementAbove map[string]float64 `yaml:"fail_when_measurement_above" json:"fail_when_measurement_above,omitempty"`
	FailWhenMeasurementBelow map[string]float64 `yaml:"fail_when_measurement_below" json:"fail_when_measurement_below,omitempty"`
	// AlertSeveritySettings are the check's own. A check's definition
	// leaves them out, so changing them keeps its incidents.
	AlertSeveritySettings `yaml:",inline"`
	// SendAlertsOnlyToDestinations names the destinations that get every
	// alert of the check; no other destination gets one, whatever its
	// alert_severities say. Not stated, the check's alerts go by severity.
	// An empty list sends no alert for the check. Like the alert severity
	// settings, it is kept out of a check's identity. See goesTo.
	SendAlertsOnlyToDestinations *[]string `yaml:"send_alerts_only_to_destinations" json:"send_alerts_only_to_destinations,omitempty"`
	// NoAlertsOrRecoveryActionsWhileTheseChecksFail names the checks of the
	// check's causes, each as CHECK_GROUP/CHECK. While one of them is in
	// its failing time, MMWatchdog sends no alert for this check and takes no
	// recovery action because of it. Like the alert severity settings, it
	// is kept out of a check's identity. See onecause.go.
	NoAlertsOrRecoveryActionsWhileTheseChecksFail []string `yaml:"no_alerts_or_recovery_actions_while_these_checks_fail" json:"no_alerts_or_recovery_actions_while_these_checks_fail,omitempty"`
}

// ValidateExecOptions fills the omitted execution options and checks them. The path of
// its error is below the setting that holds the options.
func ValidateExecOptions(opts *primitives.ExecOptions) *FieldError {
	if opts.RunAsUserAccount == "" {
		opts.RunAsUserAccount = primitives.UnprivilegedAccount
	}
	if opts.RunFromDirectory == "" {
		opts.RunFromDirectory = "/"
	}
	if !filepath.IsAbs(opts.RunFromDirectory) {
		return &FieldError{"run_from_directory", "must be absolute"}
	}
	for k, v := range opts.Env {
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(k) || strings.ContainsRune(v, 0) || k == "BASH_ENV" || k == "ENV" || (k == "MMWATCHDOG_BIN" || k == "MMWATCHDOG_MODULE_HELPER") {
			return &FieldError{"env." + k, "unsupported environment variable"}
		}
	}
	return nil
}

// ValidateExecOptionsOnNode checks that the user account and the directory of the
// execution options exist on this node.
func ValidateExecOptionsOnNode(opts primitives.ExecOptions) *FieldError {
	if _, err := primitives.LookUpUserAccount(opts.RunAsUserAccount); err != nil {
		return &FieldError{"run_as_user_account", err.Error()}
	}
	if !primitives.IsDirectory(opts.RunFromDirectory) {
		return &FieldError{"run_from_directory", opts.RunFromDirectory + " is not a directory"}
	}
	return nil
}

// RecoveryAttemptSettings are the budget settings a restart and a recovery
// command share: the checks whose failure calls for the action, and how
// many attempts it has.
type RecoveryAttemptSettings struct {
	TriggeredByChecks            []string    `yaml:"triggered_by_checks" json:"triggered_by_checks"`
	Attempts                     MaxAttempts `yaml:"max_attempts" json:"max_attempts"`
	MinTimeBetweenAttempts       Duration    `yaml:"min_time_between_attempts" json:"min_time_between_attempts"`
	ResetAttemptsAfterHealthyFor Duration    `yaml:"reset_attempts_after_healthy_for" json:"reset_attempts_after_healthy_for"`
}

// MaxAttempts is how many attempts a recovery action has: a whole number
// from 1, or unlimited. It is kept as written and is a number in JSON
// unless it is unlimited.
type MaxAttempts string

// UnlimitedAttempts is the value of max_attempts that never stops trying,
// and NoAttemptLimit the number that stands for it, so that the smallest of
// several limits still governs.
const (
	UnlimitedAttempts MaxAttempts = "unlimited"
	NoAttemptLimit                = math.MaxInt
)

// Limit returns the number of attempts, NoAttemptLimit for unlimited, or
// false for a value that is neither.
func (m MaxAttempts) Limit() (int, bool) {
	if m == UnlimitedAttempts {
		return NoAttemptLimit, true
	}
	n, e := strconv.Atoi(string(m))
	return n, e == nil && n >= 1 && strings.TrimLeft(string(m), "0123456789") == ""
}

func (m MaxAttempts) MarshalJSON() ([]byte, error) {
	if n, e := strconv.Atoi(string(m)); e == nil {
		return json.Marshal(n)
	}
	return json.Marshal(string(m))
}

func (m *MaxAttempts) UnmarshalJSON(b []byte) error {
	var n int
	if json.Unmarshal(b, &n) == nil {
		*m = MaxAttempts(strconv.Itoa(n))
		return nil
	}
	var s string
	if e := json.Unmarshal(b, &s); e != nil {
		return e
	}
	*m = MaxAttempts(s)
	return nil
}

// RestartSystemdUnit restarts SystemdUnit when one of TriggeredByChecks
// has been failing for RestartAfterFailingFor, within its budget.
type RestartSystemdUnit struct {
	SystemdUnit             string   `yaml:"systemd_unit" json:"systemd_unit"`
	RestartAfterFailingFor  Duration `yaml:"restart_after_failing_for" json:"restart_after_failing_for"`
	RecoveryAttemptSettings `yaml:",inline"`
}

// RecoveryCommand is a check group's configured repair command. It runs
// like a shell check when one of its TriggeredByChecks has been failing for
// RunAfterFailingFor, and its budget settings follow the restart rules.
type RecoveryCommand struct {
	Script                  string   `yaml:"script" json:"script"`
	RunAfterFailingFor      Duration `yaml:"run_after_failing_for" json:"run_after_failing_for"`
	TimeLimitPerAttempt     Duration `yaml:"time_limit_per_attempt" json:"time_limit_per_attempt"`
	primitives.ExecOptions  `yaml:",inline"`
	RecoveryAttemptSettings `yaml:",inline"`
}

// RestartUnit is the systemd unit the check group's restart_systemd_unit
// names, or "" when it has none.
func (g CheckGroup) RestartUnit() string {
	if g.RestartSystemdUnit == nil {
		return ""
	}
	return g.RestartSystemdUnit.SystemdUnit
}

// SystemdUnits lists the systemd units the check group is about: the one
// its restart names and those its unit checks name. An action on one of
// them makes the group's earlier evidence old.
func (g CheckGroup) SystemdUnits() []string {
	units := []string{}
	if unit := g.RestartUnit(); unit != "" {
		units = append(units, unit)
	}
	for _, name := range Names(g.Checks) {
		if unit := g.Checks[name].SystemdUnitName(); unit != "" && !slices.Contains(units, unit) {
			units = append(units, unit)
		}
	}
	return units
}

type Heartbeat struct {
	MaxTimeBetweenHeartbeats Duration `yaml:"max_time_between_heartbeats" json:"max_time_between_heartbeats"`
	ExpectedBearerTokenFile  string   `yaml:"expected_bearer_token_file" json:"expected_bearer_token_file"`
}

var NamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// EveryCheckGroup names the whole-node maintenance window, so no check group may use it.
const EveryCheckGroup = "every_check_group"

const DefaultOfficialExtraModuleDir = "/usr/local/lib/mmwatchdog/official_extra_modules"

const DefaultExternalModuleDir = "/usr/local/lib/mmwatchdog/external_modules"

func Fields(t reflect.Type) map[string]reflect.Type {
	m := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("yaml"), ",")
		if len(tag) > 1 && tag[1] == "inline" {
			for k, v := range Fields(f.Type) {
				m[k] = v
			}
		} else if tag[0] != "-" {
			m[tag[0]] = f.Type
		}
	}
	return m
}

func httpURL(s string) bool {
	u, e := url.Parse(s)
	return e == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil //nolint:nilaway // url.Parse returns a nonnil URL on success; the short-circuit error guard runs before field access.
}

// HostPort accepts host:port with a numeric port from minPort to 65535.
func HostPort(s string, minPort int) error {
	host, port, e := net.SplitHostPort(s)
	if e != nil {
		return e
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("the address has no host: write the host and port, such as 127.0.0.1:5432")
	}
	if n, e := strconv.Atoi(port); e != nil || n < minPort || n > 65535 {
		return fmt.Errorf("port must be a number from %d to 65535", minPort)
	}
	return nil
}

// systemdUnit is the systemd unit that the check is about, or "" for a
// check about something else.
func (c Check) SystemdUnitName() string {
	if about, ok := c.Inputs.(aboutSystemdUnit); ok {
		return about.SystemdUnitName()
	}
	return ""
}

// LimitSettings are the two settings that hold a check's limits.
var LimitSettings = []string{"fail_when_measurement_above", "fail_when_measurement_below"}

// Limits returns the limit map that the setting named field holds.
func (c Check) Limits(field string) map[string]float64 {
	if field == "fail_when_measurement_above" {
		return c.FailWhenMeasurementAbove
	}
	return c.FailWhenMeasurementBelow
}

// MeasurementNames lists what the check's module measures, in the order its
// messages name them, or nil for a module that names no measurements.
func (c Check) MeasurementNames() []string {
	if module, ok := c.Inputs.(namesMeasurements); ok {
		return module.MeasurementNames()
	}
	if module := c.Module(); module.LimitsOnItsMeasurements && module.Description != nil {
		return module.Description.MeasurementNames()
	}
	return nil
}

// HasLimit reports that the check states a limit on a measurement.
func (c Check) HasLimit() bool {
	return len(c.FailWhenMeasurementAbove)+len(c.FailWhenMeasurementBelow) > 0
}

// RecordsOnly reports a check that explicitly only records readings.
// Validation permits this choice only on measurement checks without limits
// or alert severity. Status keeps the field name records_only.
func (c Check) RecordsOnly() bool {
	return c.OnlyRecordReadings
}

// takesNoLimit is the error of a limit on a check whose module takes none:
// a module that reports no measurement, or one that records its
// measurements and takes no limit on them, such as tls, whose own input
// fail_when_certificate_expires_within is its limit.
func takesNoLimit(module *CheckModule) string {
	if module.Description != nil && len(module.Description.Measurements) > 0 {
		return "the module " + module.Name + " records its measurements but takes no limit on them"
	}
	return "the module " + module.Name + " reports no measurement, so its check takes no limit"
}

// ValidateLimits checks the two limit maps. A check takes limits only on
// the measurements that its module reports: a module whose inputs decide
// them names them itself (namesMeasurements), and every other module lists
// them in its description.
func (c Check) ValidateLimits() *FieldError {
	module, names := c.Module(), c.MeasurementNames()
	for _, field := range LimitSettings {
		limits := c.Limits(field)
		if limits == nil {
			continue
		}
		if !module.Measures && !module.LimitsOnItsMeasurements {
			return &FieldError{field, takesNoLimit(module)}
		}
		for _, name := range Names(limits) {
			path := field + "." + name
			if len(names) == 0 {
				return &FieldError{path, "the module " + module.Name + " reports no measurement, so its check takes no limit"}
			}
			if !slices.Contains(names, name) {
				return &FieldError{path, "this check has no such measurement; it measures " + strings.Join(names, ", ")}
			}
			if c.ModuleKind() != ModuleKindCore {
				continue
			}
			if strings.HasSuffix(name, "_percent") && (limits[name] < 0 || limits[name] > 100) {
				return &FieldError{path, "expected a number from 0 to 100"}
			}
			if limits[name] < 0 && name != "highest_temperature_celsius" {
				return &FieldError{path, "expected a number of 0 or more"}
			}
		}
	}
	return nil
}

func Names[V any](m map[string]V) []string {
	ns := make([]string, 0, len(m))
	for n := range m {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	return ns
}

// PlainText accepts UTF-8 text without control characters, up to limit bytes.
func PlainText(s string, limit int) bool {
	if len(s) > limit || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// AlertSeveritySettings are a check's alert severity settings, written on
// the check itself: its required alert_severity and an optional
// raise_to_sos_after_failing_for. A check's cannot-check alerts take the
// same severity.
type AlertSeveritySettings struct {
	AlertSeverity             string   `yaml:"alert_severity" json:"alert_severity,omitempty"`
	RaiseToSosAfterFailingFor Duration `yaml:"raise_to_sos_after_failing_for" json:"raise_to_sos_after_failing_for,omitempty"`
}

// Takes reports that a destination takes an alert: its severity, or for
// a recovery or retirement one of the severities its incident went out
// with, is among the destination's alert_severities.
func (d Destination) Takes(e Event) bool {
	severities := []string{e.AlertSeverity}
	if Closes(e.Type) && len(e.AlertSeveritiesSentForIncident) > 0 {
		severities = e.AlertSeveritiesSentForIncident
	}
	for _, s := range severities {
		if slices.Contains(d.AlertSeverities, s) {
			return true
		}
	}
	return false
}
