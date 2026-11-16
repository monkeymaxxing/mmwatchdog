// Package config reads and checks a node's whole configuration: its node
// settings, its check groups, and its alert destinations, with the place in
// the files where each setting is written. It checks the settings across
// checks (the severities, the checks named as causes, the modules in use)
// and derives each check's definition. The settings of one check or
// destination, and the modules, are in checksandalerts; how a node keeps
// its incidents, its store, and its fleet use this package.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/primitives"
)

type Config struct {
	Sources  map[string]checksandalerts.SourceLocation `yaml:"-" json:"sources,omitempty"`
	Socket   string                                    `yaml:"socket" json:"socket"`
	StateDir string                                    `yaml:"state_dir" json:"state_dir"`
	// OfficialExtraModuleDir and ExternalModuleDir are the two directories
	// in which the node holds the modules that are not core: the official
	// extra modules of the installed MMWatchdog release, and the modules of the
	// project's own. ModuleStateDir holds one directory for each check
	// whose module is not core, for what the module keeps between runs.
	OfficialExtraModuleDir string `yaml:"official_extra_module_dir" json:"official_extra_module_dir"`
	ExternalModuleDir      string `yaml:"external_module_dir" json:"external_module_dir"`
	ModuleStateDir         string `yaml:"module_state_dir" json:"module_state_dir"`
	MaxConcurrentChecks    int    `yaml:"max_concurrent_checks" json:"max_concurrent_checks"`
	// AlertSeverityWhenThisNodeGoesSilent is the severity of the alert its
	// hub-outage alerter sends when this node stops reporting: the hub for a joined
	// node, the hub-outage alerter nodes for the hub. It is required.
	AlertSeverityWhenThisNodeGoesSilent string `yaml:"alert_severity_when_this_node_goes_silent" json:"alert_severity_when_this_node_goes_silent,omitempty"`
	// SendSilenceAlertsOfThisNodeOnlyToDestinations selects the recipients of every silence alert.
	// nil uses severity; an empty list sends no new alert.
	SendSilenceAlertsOfThisNodeOnlyToDestinations *[]string `yaml:"send_silence_alerts_of_this_node_only_to_destinations" json:"send_silence_alerts_of_this_node_only_to_destinations,omitempty"`
	// RaiseToSosAfterThisNodeIsSilentFor raises the alert that this node
	// stopped reporting to sos once the node has been silent for that long.
	// Without it, time never raises that alert.
	RaiseToSosAfterThisNodeIsSilentFor checksandalerts.Duration `yaml:"raise_to_sos_after_this_node_is_silent_for" json:"raise_to_sos_after_this_node_is_silent_for,omitempty"`
	// ThisNodeIsInNodeGroups names the node groups that this node is in, for
	// the fleet view. A node group exists because nodes name it.
	ThisNodeIsInNodeGroups []string `yaml:"this_node_is_in_node_groups" json:"this_node_is_in_node_groups,omitempty"`
	// RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent names node groups
	// of this node's ThisNodeIsInNodeGroups: the hub raises the alert that
	// this node stopped reporting to sos while every node of one of them is
	// silent. Without it, or with an empty list, no node group raises it.
	RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent []string `yaml:"raise_to_sos_when_every_node_of_these_node_groups_is_silent" json:"raise_to_sos_when_every_node_of_these_node_groups_is_silent,omitempty"`
	// AlertSeverityWhenThisNodeHasRebooted is the severity of the notice
	// this node sends after an unplanned reboot. It is required.
	AlertSeverityWhenThisNodeHasRebooted string `yaml:"alert_severity_when_this_node_has_rebooted" json:"alert_severity_when_this_node_has_rebooted,omitempty"`
	// KeepMeasurementHistoryFor and MeasurementHistorySizeLimit say how long
	// this node keeps the readings of its measurements and how much of its
	// disk they may take. Both are required: see measurementhistory.go.
	KeepMeasurementHistoryFor   checksandalerts.Duration `yaml:"keep_measurement_history_for" json:"keep_measurement_history_for,omitempty"`
	MeasurementHistorySizeLimit checksandalerts.Size     `yaml:"measurement_history_size_limit" json:"measurement_history_size_limit,omitempty"`
	// The storage settings bound the stores in state.db that grow with
	// time. Queue retention is required; event and database limits have
	// defaults. See StorageLimits.
	KeepEventHistoryFor               checksandalerts.Duration              `yaml:"keep_event_history_for" json:"keep_event_history_for"`
	EventHistoryContentSizeLimit      checksandalerts.Size                  `yaml:"event_history_content_size_limit" json:"event_history_content_size_limit"`
	KeepUndeliveredAlertsFor          checksandalerts.Duration              `yaml:"keep_undelivered_alerts_for" json:"keep_undelivered_alerts_for"`
	UndeliveredAlertsContentSizeLimit checksandalerts.Size                  `yaml:"undelivered_alerts_content_size_limit" json:"undelivered_alerts_content_size_limit"`
	StateDatabaseSizeLimit            checksandalerts.Size                  `yaml:"state_database_size_limit" json:"state_database_size_limit"`
	Alerts                            Alerts                                `yaml:"-" json:"alerts"`
	HeartbeatReceiver                 *HeartbeatReceiver                    `yaml:"heartbeat_receiver" json:"heartbeat_receiver,omitempty"`
	FleetHubRole                      *FleetHubRole                         `yaml:"fleet_hub_role" json:"fleet_hub_role,omitempty"`
	JoinFleetHub                      *JoinFleetHub                         `yaml:"join_fleet_hub" json:"join_fleet_hub,omitempty"`
	CheckGroups                       map[string]checksandalerts.CheckGroup `yaml:"-" json:"check_groups"`
	// Modules are the modules that are not core on this node, from the two
	// module directories. Load sets them.
	Modules *checksandalerts.ModuleCatalog `json:"-" yaml:"-"`
}

// InFleet reports a hub or a node that joins one: both have fleet checks.
func (c *Config) InFleet() bool { return c.FleetHubRole != nil || c.JoinFleetHub != nil }

// JoinFleetHub selects this node's join code and hub-outage alerter preference.
type JoinFleetHub struct {
	checksandalerts.SecretFileAccess `yaml:",inline"`
	JoinCodeFile                     string `yaml:"join_code_file" json:"join_code_file"`
	PreferThisNodeAsHubOutageAlerter bool   `yaml:"prefer_this_node_as_hub_outage_alerter" json:"prefer_this_node_as_hub_outage_alerter,omitempty"`
}

// FleetHubRole enables the hub and names the address nodes connect to.
type FleetHubRole struct {
	ListenOnAddress string `yaml:"listen_on_address" json:"listen_on_address"`
	AddressForNodes string `yaml:"address_for_nodes" json:"address_for_nodes"`
	FleetTimings    `yaml:",inline"`
	// Choose a count or a percentage of joined nodes whose simultaneous
	// silence raises their alerts to sos. With neither, no count raises.
	RaiseToSosWhenSilentNodesReachCount   *int     `yaml:"raise_to_sos_when_silent_nodes_reach_count" json:"raise_to_sos_when_silent_nodes_reach_count,omitempty"`
	RaiseToSosWhenSilentNodesReachPercent *float64 `yaml:"raise_to_sos_when_silent_nodes_reach_percent" json:"raise_to_sos_when_silent_nodes_reach_percent,omitempty"`
	// The hub's storage settings bound the events it keeps for its nodes
	// and the alerts of nodes that it has accepted and not delivered yet.
	// Event retention has defaults; queued-alert capacity is required.
	// See HubLimits.
	// KeepMeasurementHistoryOfNodesFor and MeasurementHistoryOfNodesSizeLimit
	// say how long the hub keeps the readings that the joined nodes send and
	// how much of the hub's disk they may take. Both are required: see
	// measurementhistory.go.
	KeepMeasurementHistoryOfNodesFor         checksandalerts.Duration `yaml:"keep_measurement_history_of_nodes_for" json:"keep_measurement_history_of_nodes_for,omitempty"`
	MeasurementHistoryOfNodesSizeLimit       checksandalerts.Size     `yaml:"measurement_history_of_nodes_size_limit" json:"measurement_history_of_nodes_size_limit,omitempty"`
	KeepEventHistoryOfNodesFor               checksandalerts.Duration `yaml:"keep_event_history_of_nodes_for" json:"keep_event_history_of_nodes_for,omitempty"`
	EventHistoryOfNodesContentSizeLimit      checksandalerts.Size     `yaml:"event_history_of_nodes_content_size_limit" json:"event_history_of_nodes_content_size_limit,omitempty"`
	UndeliveredAlertsOfNodesContentSizeLimit checksandalerts.Size     `yaml:"undelivered_alerts_of_nodes_content_size_limit" json:"undelivered_alerts_of_nodes_content_size_limit,omitempty"`
}

// The defaults of the hub's storage settings.
const (
	defaultKeepEventHistoryOfNodesFor          checksandalerts.Duration = "7d"
	defaultEventHistoryOfNodesContentSizeLimit checksandalerts.Size     = "4GiB"
)

// StorageLimits bound the stores in state.db that grow with time: the event
// history, the outbox of alerts that wait for delivery, on a hub the alerts
// of nodes that wait in its alert grouping step, and the database file
// itself. They come from the node settings and fleet_hub_role.
type StorageLimits struct {
	KeepEventHistoryFor           time.Duration
	EventHistoryBytes             int64
	KeepUndeliveredAlertsFor      time.Duration
	UndeliveredAlertsBytes        int64
	StateDatabaseBytes            int64
	UndeliveredAlertsOfNodesBytes int64
}

// DefaultStorageLimits are the limits of a configuration that states none.
var DefaultStorageLimits = StorageLimits{24 * time.Hour, 16 * 1024 * 1024, 24 * time.Hour, 8 * 1024 * 1024, 128 * 1024 * 1024, 32 * 1024 * 1024}

// StorageLimits are the limits a validated configuration states.
func (c *Config) StorageLimits() StorageLimits {
	limits := StorageLimits{c.KeepEventHistoryFor.Value(), c.EventHistoryContentSizeLimit.Bytes(), c.KeepUndeliveredAlertsFor.Value(), c.UndeliveredAlertsContentSizeLimit.Bytes(), c.StateDatabaseSizeLimit.Bytes(), 0}
	if c.FleetHubRole != nil {
		limits.UndeliveredAlertsOfNodesBytes = c.FleetHubRole.UndeliveredAlertsOfNodesContentSizeLimit.Bytes()
	}
	return limits
}

// HubLimits bound the events and the readings that the hub keeps for its
// nodes. The two settings of the readings are kept as written, for the line
// that says how far back the measurement history of nodes reaches.
type HubLimits struct {
	KeepEventHistoryOfNodesFor         time.Duration
	EventHistoryOfNodesBytes           int64
	KeepMeasurementHistoryOfNodesFor   checksandalerts.Duration
	MeasurementHistoryOfNodesSizeLimit checksandalerts.Size
}

// DefaultHubLimits are the limits of a hub that states no limit for its
// events, and for its readings the two required settings with the values of
// the design's example.
var DefaultHubLimits = HubLimits{7 * 24 * time.Hour, 4 * 1024 * 1024 * 1024, "7d", "20GiB"}

// HubLimits are the limits a validated hub configuration states.
func (h *FleetHubRole) HubLimits() HubLimits {
	return HubLimits{h.KeepEventHistoryOfNodesFor.Value(), h.EventHistoryOfNodesContentSizeLimit.Bytes(), h.KeepMeasurementHistoryOfNodesFor, h.MeasurementHistoryOfNodesSizeLimit}
}

// FleetTimings are the hub's fleet-wide timings: how often nodes report
// (three missed reports make one silent), how long the hub-outage alerter waits before
// it alerts that the hub is unreachable, and how long a joined node waits
// for the hub to accept an alert before delivering it directly.
type FleetTimings struct {
	ReportEvery                        checksandalerts.Duration `yaml:"report_every" json:"report_every,omitempty"`
	AlertAfterHubUnreachableFor        checksandalerts.Duration `yaml:"alert_after_hub_unreachable_for" json:"alert_after_hub_unreachable_for,omitempty"`
	SendAlertWithoutHubAfterWaitingFor checksandalerts.Duration `yaml:"send_alert_without_hub_after_waiting_for" json:"send_alert_without_hub_after_waiting_for,omitempty"`
}

// DefaultFleetTimings are the initial timings of a joined node before it
// holds fleet settings. A hub must state all three timings.
var DefaultFleetTimings = FleetTimings{"30s", "3m", "30s"}

// Problem returns the first invalid timing and why. A timing has no upper
// bound: a fleet with slow links states longer ones.
func (t FleetTimings) Problem() (string, string) {
	for _, f := range []struct {
		name  string
		value checksandalerts.Duration
	}{{"report_every", t.ReportEvery}, {"alert_after_hub_unreachable_for", t.AlertAfterHubUnreachableFor}, {"send_alert_without_hub_after_waiting_for", t.SendAlertWithoutHubAfterWaitingFor}} {
		if v, e := checksandalerts.ParseDuration(string(f.value)); e != nil || v < time.Second {
			return f.name, "expected a duration of at least 1s"
		}
	}
	if t.AlertAfterHubUnreachableFor.Value() < 2*t.ReportEvery.Value() {
		return "alert_after_hub_unreachable_for", "must be at least twice report_every, so one late report does not alert"
	}
	return "", ""
}

// Alerts holds the alert destinations, read from alert_destinations.yml beside the main
// configuration file.
type Alerts struct {
	Destinations map[string]checksandalerts.Destination `yaml:"destinations" json:"destinations"`
}

type HeartbeatReceiver struct {
	checksandalerts.SecretFileAccess `yaml:",inline"`
	ListenOnAddress                  string `yaml:"listen_on_address" json:"listen_on_address"`
	TLSCertificateFile               string `yaml:"tls_certificate_file" json:"tls_certificate_file"`
	TLSPrivateKeyFile                string `yaml:"tls_private_key_file" json:"tls_private_key_file"`
}

// MaxAttemptsOf writes a number of attempts as the setting states it.
func MaxAttemptsOf(n int) checksandalerts.MaxAttempts {
	if n == checksandalerts.NoAttemptLimit {
		return checksandalerts.UnlimitedAttempts
	}
	return checksandalerts.MaxAttempts(strconv.Itoa(n))
}

// ValidationWarnings are what mmwatchdog validate and mmwatchdog reload say about a valid
// configuration that will not do what its author probably expects. A
// validation warning does not make the configuration invalid.
type ValidationWarnings struct {
	MMHotlineLoopbackWarnings []string `json:"mmhotline_loopback_warnings"`
	// MMHotlineWithoutAnotherDestination names each mmhotline destination
	// of a node that has no other destination to pass an alert on to when
	// MMHotline permanently refuses it.
	MMHotlineWithoutAnotherDestination []string                            `json:"mmhotline_without_another_destination"`
	ChecksWithoutAlertDestination      []CheckWithoutAlertDestination      `json:"checks_without_alert_destination"`
	ChecksWithUnknownAlertDestinations []CheckWithUnknownAlertDestinations `json:"checks_with_unknown_alert_destinations"`

	AlertTitlesThatWillBeShortened              []checksandalerts.AlertTitleThatWillBeShortened            `json:"alert_titles_that_will_be_shortened"`
	SystemdUnitsMissingFromSystemdUnitsToIgnore []checksandalerts.SystemdUnitMissingFromUnitsToIgnore      `json:"systemd_units_missing_from_systemd_units_to_ignore"`
	FilesystemsMissingFromFilesystemsToIgnore   []checksandalerts.FilesystemMissingFromFilesystemsToIgnore `json:"filesystems_missing_from_filesystems_to_ignore"`
	ChecksThatNameACheckWithALowerAlertSeverity []CheckThatNamesACheckWithALowerAlertSeverity              `json:"checks_that_name_a_check_with_a_lower_alert_severity"`
}

// ValidationWarnings returns every validation warning of the configuration.
// An alert title names the node, so its length depends on node, the name of
// the machine that runs the configuration. The checkNodeFiles argument is
// reserved for node-only warnings.
func (c *Config) ValidationWarnings(node string, checkNodeFiles bool) ValidationWarnings {
	warnings := ValidationWarnings{
		MMHotlineLoopbackWarnings:                   c.mmhotlineLoopbackWarnings(),
		MMHotlineWithoutAnotherDestination:          c.mmhotlineWithoutAnotherDestinationWarnings(),
		AlertTitlesThatWillBeShortened:              c.AlertTitlesThatWillBeShortened(node),
		SystemdUnitsMissingFromSystemdUnitsToIgnore: c.SystemdUnitsMissingFromUnitsToIgnore(),
		FilesystemsMissingFromFilesystemsToIgnore:   c.FilesystemsMissingFromFilesystemsToIgnore(),
		ChecksThatNameACheckWithALowerAlertSeverity: c.ChecksThatNameACheckWithALowerAlertSeverity(),
		ChecksWithoutAlertDestination:               []CheckWithoutAlertDestination{},
		ChecksWithUnknownAlertDestinations:          []CheckWithUnknownAlertDestinations{},
	}
	if c.JoinFleetHub == nil {
		c.RoutingWarnings(c.Alerts.Destinations, &warnings)
	}
	return warnings
}

// The directories of a node that states none.
const (
	defaultModuleStateDir = "/var/lib/mmwatchdog-module-state"
)

// Load validates the complete configuration directory before activation. It
// finds the modules that are not core in the two module directories that
// the node settings state.
func Load(directory string) (*Config, error) {
	return LoadWithModuleDirs(directory, checksandalerts.ModuleDirs{})
}

// LoadWithModuleDirs is Load with the modules of the directories given, in
// place of those that the node settings state. A deploy validates a new
// configuration against the module directories that it has staged, and a
// person validates the configuration of a deployment project against the
// project's directories. An empty directory name keeps the node's own.
func LoadWithModuleDirs(directory string, dirs checksandalerts.ModuleDirs) (*Config, error) {
	path := filepath.Join(directory, "mmwatchdog_node_settings.yml")
	c := &Config{Socket: "/run/mmwatchdog/mmwatchdog.sock", StateDir: "/var/lib/mmwatchdog", OfficialExtraModuleDir: checksandalerts.DefaultOfficialExtraModuleDir, ExternalModuleDir: checksandalerts.DefaultExternalModuleDir, ModuleStateDir: defaultModuleStateDir, MaxConcurrentChecks: 8, KeepEventHistoryFor: "24h", EventHistoryContentSizeLimit: "16MiB", StateDatabaseSizeLimit: "128MiB", Sources: map[string]checksandalerts.SourceLocation{}, CheckGroups: map[string]checksandalerts.CheckGroup{}}
	if err := readYAML(path, c, c.Sources, nil); err != nil {
		return nil, err
	}
	for _, field := range []struct{ name, value string }{{"official_extra_module_dir", c.OfficialExtraModuleDir}, {"external_module_dir", c.ExternalModuleDir}, {"module_state_dir", c.ModuleStateDir}} {
		if !filepath.IsAbs(field.value) {
			return nil, c.ErrorAt(field.name, "expected an absolute path")
		}
	}
	if dirs.OfficialExtra == "" {
		dirs.OfficialExtra = c.OfficialExtraModuleDir
	}
	if dirs.External == "" {
		dirs.External = c.ExternalModuleDir
	}
	var err error
	if c.Modules, err = checksandalerts.LoadModuleCatalog(dirs); err != nil {
		return nil, err
	}
	alerts := filepath.Join(directory, "alert_destinations.yml")
	if c.JoinFleetHub != nil {
		if _, err := os.Lstat(alerts); err == nil {
			return nil, c.ErrorAt("join_fleet_hub", "remove "+alerts+": a node that joins a hub gets its alert settings from the hub")
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	} else if err := c.readAlerts(alerts); os.IsNotExist(err) {
		return nil, fmt.Errorf("%s: alert destinations file is missing. Create the file with a destination, or write \"destinations: {}\" into the file to run MMWatchdog with no destination. With no destination, MMWatchdog runs checks, restarts units, and runs recovery commands, and sends alerts to nobody", alerts)
	} else if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	} else if c.Alerts.Destinations == nil {
		// Also an empty file, which the YAML decoder reports as its end.
		return nil, fmt.Errorf("%s: expected a destinations: mapping; write \"destinations: {}\" to run MMWatchdog with no destination", alerts)
	}
	groupsDir := filepath.Join(directory, "check_groups")
	entries, err := os.ReadDir(groupsDir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		p := filepath.Join(groupsDir, entry.Name())
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yml") {
			return nil, fmt.Errorf("%s: check_groups must contain only .yml files", p)
		}
		var groups struct {
			CheckGroups map[string]checksandalerts.CheckGroup `yaml:"check_groups"`
		}
		sources := map[string]checksandalerts.SourceLocation{}
		if err := readYAML(p, &groups, sources, c.Modules); err != nil {
			return nil, err
		}
		if groups.CheckGroups == nil {
			return nil, fmt.Errorf("%s: expected a check_groups: mapping", p)
		}
		for _, name := range checksandalerts.Names(groups.CheckGroups) {
			if _, exists := c.CheckGroups[name]; exists {
				return nil, fmt.Errorf("%s: duplicate check group name %q (first defined at %s)", sources["check_groups."+name], name, c.Source("check_groups."+name))
			}
			c.CheckGroups[name] = groups.CheckGroups[name]
		}
		for key, location := range sources {
			if strings.HasPrefix(key, "check_groups") {
				c.Sources[key] = location
			}
		}
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// readAlerts reads the independently owned destination settings.
func (c *Config) readAlerts(path string) error {
	sources := map[string]checksandalerts.SourceLocation{}
	if err := readYAML(path, &c.Alerts, sources, c.Modules); err != nil {
		return err
	}
	for key, loc := range sources {
		if key != "" {
			c.Sources[key] = loc
		}
	}
	return nil
}

// readYAML reads one configuration file strictly into out, and records
// where each setting is written. modules are the node's modules that are
// not core, which a check or a destination in the file can name.
func readYAML(path string, out any, sources map[string]checksandalerts.SourceLocation, modules *checksandalerts.ModuleCatalog) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close() // Closing this read-only handle has no pending writes to commit.
	}()
	b, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil {
		return err
	}
	if len(b) > 1024*1024 {
		return fmt.Errorf("%s: configuration exceeds 1 MiB", path)
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	var n yaml.Node
	if err = d.Decode(&n); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var extra yaml.Node
	if err = d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("%s: expected one YAML document", path)
	}
	if len(n.Content) != 1 {
		return fmt.Errorf("%s: expected a mapping", path)
	}
	root := n.Content[0]
	if err = validateNode(root, reflect.TypeOf(out).Elem(), modules, "", checksandalerts.SourceLocation{File: path, Line: root.Line, Column: root.Column}); err != nil {
		return err
	}
	checksandalerts.RecordSources(sources, path, "", root)
	if err = n.Decode(out); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// validateNode checks one node of a configuration file against the Go type
// that it is read into: every key is known, every value has the type of its
// setting. A mapping of a check or a destination also names one module: a
// core module, whose inputs are checked against its input type here, or one
// of modules, the node's modules that are not core, whose inputs
// ModuleCall.validate checks against the module's description.
//
// path is the setting's path, such as check_groups.web.checks.ssh_answers.ssh,
// and at is where the setting is written: its key, or the item of a list.
// Every error names both, in the form of MMWatchdog's other errors of a setting.
func validateNode(n *yaml.Node, t reflect.Type, modules *checksandalerts.ModuleCatalog, path string, at checksandalerts.SourceLocation) error {
	bad := func(expected string) error {
		return nodeError(at, path, "expected "+expected+", found "+foundInYAML(n))
	}
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return bad("a value without YAML aliases or anchors")
	}
	if n.Style&yaml.TaggedStyle != 0 {
		return bad("a value without explicit YAML tags")
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if path == "fleet_hub_role.raise_to_sos_when_silent_nodes_reach_count" && (n.Kind != yaml.ScalarNode || n.Tag != "!!int") {
		return nodeError(at, path, "expected an integer for the number of silent nodes that raises silence alerts to sos; set raise_to_sos_when_silent_nodes_reach_count: 5")
	}
	if path == "fleet_hub_role.raise_to_sos_when_silent_nodes_reach_percent" {
		var percent float64
		if n.Kind != yaml.ScalarNode || (n.Tag != "!!int" && n.Tag != "!!float") {
			return nodeError(at, path, "expected a number for the percentage of silent nodes that raises silence alerts to sos; set raise_to_sos_when_silent_nodes_reach_percent: 5 without a percent sign")
		}
		if err := n.Decode(&percent); err != nil || math.IsNaN(percent) || math.IsInf(percent, 0) {
			return nodeError(at, path, "expected a finite number for the percentage of silent nodes that raises silence alerts to sos; set raise_to_sos_when_silent_nodes_reach_percent: 5")
		}
	}
	scalarExpected := checksandalerts.ScalarStandsFor(t)
	if scalarExpected != "" && n.Kind == yaml.ScalarNode {
		if n.Tag != "!!str" {
			return bad(scalarExpected)
		}
		return nil
	}
	if t == reflect.TypeOf(checksandalerts.MaxAttempts("")) {
		if n.Kind != yaml.ScalarNode || (n.Tag != "!!int" && n.Tag != "!!str") {
			return bad("a whole number of attempts such as 3, or unlimited")
		}
		return nil
	}
	if t == reflect.TypeOf(checksandalerts.Size("")) {
		if n.Tag != "!!str" {
			return bad("a size such as 256MiB")
		}
		if _, err := checksandalerts.ParseSize(n.Value); err != nil {
			return bad("a size: a whole number with MiB or GiB, such as 256MiB")
		}
		return nil
	}
	if t == reflect.TypeOf(checksandalerts.Duration("")) {
		if strings.HasSuffix(path, ".remind_every") && n.Tag == "!!str" && n.Value == "never" {
			return nil
		}
		if len(n.Value) > 64 {
			return bad("a duration of at most 64 characters")
		}
		if n.Tag != "!!str" {
			return bad("a duration such as 30s")
		}
		// No setting of MMWatchdog is a negative time.
		if v, err := checksandalerts.ParseDuration(n.Value); strings.HasPrefix(n.Value, "-") || (err == nil && v < 0) {
			return bad("a duration of 0s or more, such as 30s, 5m, 26h, or 7d")
		} else if err != nil {
			return bad("a duration such as 30s, 5m, 26h, or 7d")
		}
		return nil
	}
	if n.Tag == "!!float" {
		var value float64
		if err := n.Decode(&value); err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return bad("a finite number")
		}
	}
	if t.Kind() == reflect.Interface {
		switch n.Kind {
		case yaml.MappingNode:
			t = reflect.TypeOf(map[string]any{})
		case yaml.SequenceNode:
			t = reflect.TypeOf([]any{})
		case yaml.ScalarNode:
			if n.Tag != "!!str" && n.Tag != "!!int" && n.Tag != "!!float" && n.Tag != "!!bool" && n.Tag != "!!null" {
				return bad("a JSON-compatible value")
			}
			if n.Tag == "!!bool" && n.Value != "true" && n.Value != "false" {
				return bad("true or false")
			}
			return nil
		default:
			return bad("a JSON-compatible value")
		}
	}
	// A module that takes no input is called with nothing below its name.
	callsModuleThatIsNotCore := t == reflect.TypeOf(checksandalerts.ModuleCallMapping{})
	if callsModuleThatIsNotCore && n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct, reflect.Map:
		if n.Kind != yaml.MappingNode {
			if scalarExpected != "" {
				return bad("a mapping, or " + scalarExpected)
			}
			return bad("a mapping")
		}
		seen := map[string]*yaml.Node{}
		// A mapping of a check names one module beside its own settings.
		var fs, core map[string]reflect.Type
		what := ""
		if t.Kind() == reflect.Struct {
			fs = checksandalerts.Fields(t)
			core, what = checksandalerts.ModulesNamedIn(t)
		}
		named := []string{}
		for i := 0; i < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			keyAt := checksandalerts.SourceLocation{File: at.File, Line: k.Line, Column: k.Column}
			if k.Tag != "!!str" || k.Value == "<<" {
				found := foundInYAML(k)
				if k.Value == "<<" {
					found = "the YAML merge key <<"
				}
				return nodeError(keyAt, path, "expected an ordinary string key, found "+found)
			}
			child := k.Value
			if path != "" {
				child = path + "." + k.Value
			}
			if first := seen[k.Value]; first != nil {
				return nodeError(keyAt, child, fmt.Sprintf("duplicate key %q: it is also written on line %d", k.Value, first.Line))
			}
			seen[k.Value] = k
			var vt reflect.Type
			if fs != nil {
				vt = fs[k.Value]
				if vt == nil && what != "" {
					named = append(named, k.Value)
					if vt = core[k.Value]; vt == nil {
						if !modules.Has(k.Value) {
							return nodeError(keyAt, child, fmt.Sprintf("unknown field %q: the field name matches neither a setting of a %s nor an installed module. %s", k.Value, what, modules.WhereItLooked()))
						}
						vt = reflect.TypeOf(checksandalerts.ModuleCallMapping{})
					} else if vt.Kind() == reflect.Struct && vt.NumField() == 0 && v.Kind == yaml.ScalarNode && v.Tag == "!!null" {
						return nodeError(keyAt, child, "the module "+k.Value+" takes no inputs and is written "+k.Value+": {}")
					}
				}
				if vt == nil && k.Value == "prefer_this_node_as_hub_watcher" {
					return nodeError(keyAt, child, "prefer_this_node_as_hub_watcher was renamed to prefer_this_node_as_hub_outage_alerter; use the new name")
				}
				if vt == nil {
					return nodeError(keyAt, child, fmt.Sprintf("unknown field %q", k.Value)+inputsOfCoreModule(t))
				}
			} else if runSetting := checksandalerts.Fields(reflect.TypeOf(primitives.ExecOptions{}))[k.Value]; callsModuleThatIsNotCore && runSetting != nil {
				// The three run settings stand beside the inputs of the
				// module and have the types of a shell check's settings.
				vt = runSetting
			} else {
				vt = t.Elem()
			}
			if err := validateNode(v, vt, modules, child, keyAt); err != nil {
				return err
			}
		}
		if len(named) > 1 {
			return nodeError(at, path, fmt.Sprintf("names the modules %s: a %s names exactly one module", strings.Join(named, " and "), what))
		}
	case reflect.Slice:
		if n.Kind != yaml.SequenceNode {
			return bad("a list")
		}
		for i, v := range n.Content {
			if err := validateNode(v, t.Elem(), modules, fmt.Sprintf("%s.%d", path, i), checksandalerts.SourceLocation{File: at.File, Line: v.Line, Column: v.Column}); err != nil {
				return err
			}
		}
	case reflect.String:
		if n.Tag != "!!str" {
			return bad("a string")
		}
	case reflect.Int:
		if n.Tag != "!!int" {
			return bad("an integer")
		}
	case reflect.Float64:
		if n.Tag != "!!float" && n.Tag != "!!int" {
			return bad("a number")
		}
	case reflect.Bool:
		if n.Tag != "!!bool" || (n.Value != "true" && n.Value != "false") {
			return bad("true or false")
		}
	default:
		return bad("a supported configuration value")
	}
	return nil
}

// nodeError is an error of the setting at path, written at at, in the form
// of MMWatchdog's other errors of a setting: FILE:LINE:COLUMN: PATH: MESSAGE. The
// whole file has no path.
func nodeError(at checksandalerts.SourceLocation, path, message string) error {
	if path == "" {
		return fmt.Errorf("%s: %s", at, message)
	}
	return fmt.Errorf("%s: %s: %s", at, path, message)
}

// foundInYAML says what a node of a configuration file is, for a message
// that says what was expected there and what was found. A value is quoted
// and cut after 80 bytes.
func foundInYAML(n *yaml.Node) string {
	switch {
	case n.Kind == yaml.AliasNode:
		return "the YAML alias *" + n.Value
	case n.Anchor != "":
		return "the YAML anchor &" + n.Anchor
	case n.Style&yaml.TaggedStyle != 0:
		return "the explicit YAML tag " + n.Tag
	case n.Kind == yaml.MappingNode:
		return "a mapping"
	case n.Kind == yaml.SequenceNode:
		return "a list"
	}
	value := strconv.Quote(primitives.Clip(n.Value, 80))
	switch n.Tag {
	case "!!null":
		if n.Value == "" {
			return "nothing"
		}
		return "the empty value " + value
	case "!!str":
		return "the string " + value
	// YAML reads a number, true, or false only from a value of letters,
	// digits, and signs, so these need no quotes.
	case "!!int":
		return "the whole number " + primitives.Clip(n.Value, 80)
	case "!!float":
		return "the number " + primitives.Clip(n.Value, 80)
	case "!!bool":
		return primitives.Clip(n.Value, 80)
	}
	return "the value " + value
}

// inputsOfCoreModule names the inputs of the core module whose inputs have
// the type t, for a message about an unknown field, or is "" for any other
// type.
func inputsOfCoreModule(t reflect.Type) string {
	var module string
	var description *checksandalerts.ModuleDescription
	if m := checksandalerts.CheckModulesByInputsType[reflect.PointerTo(t)]; m != nil {
		module, description = m.Name, m.Description
	} else if m := checksandalerts.DestinationModulesByInputsType[reflect.PointerTo(t)]; m != nil {
		module, description = m.Name, m.Description
	} else {
		return ""
	}
	if names := description.InputNames(); len(names) > 0 {
		return ": the inputs of the module " + module + " are " + strings.Join(names, ", ")
	}
	return ": the module " + module + " takes no inputs"
}

// timingSettings are the four timings every check group states, in the
// order a check group lists them, each with what it means.
var timingSettings = []struct{ name, meaning string }{
	{"check_every", "how often each check of this check group runs, such as 30s"},
	{"time_limit_per_run", "how long one run of a check may take, such as 5s"},
	{"alert_after_failing_for", "how long a check must be failing before its alert goes out, such as 1m"},
	{"recovered_after_healthy_for", "how long a check must be healthy before its failure is over, such as 30s"},
}

func timingError(t checksandalerts.Timing) (string, string) {
	if t.CheckEvery.Value() < ScheduleInterval {
		return "check_every", "must be at least 100ms"
	}
	if t.TimeLimitPerRun.Value() <= 0 {
		return "time_limit_per_run", "must be positive"
	}
	if t.TimeLimitPerRun.Value() > t.CheckEvery.Value() {
		return "time_limit_per_run", fmt.Sprintf("%s exceeds check_every %s", t.TimeLimitPerRun, t.CheckEvery)
	}
	return "", ""
}

func (c *Config) validate() error {
	for _, field := range []struct{ name, value string }{{"socket", c.Socket}, {"state_dir", c.StateDir}} {
		if !filepath.IsAbs(field.value) {
			return c.ErrorAt(field.name, "expected an absolute path")
		}
	}
	if c.MaxConcurrentChecks < 1 {
		return c.ErrorAt("max_concurrent_checks", "expected a whole number of at least 1")
	}
	for _, setting := range []struct{ name, reason string }{
		{"keep_undelivered_alerts_for", "how long the node keeps alerts awaiting delivery, such as 24h"},
		{"undelivered_alerts_content_size_limit", "how much alert content the node keeps awaiting delivery, such as 8MiB"},
	} {
		if !c.authored(setting.name) {
			return c.ErrorAt(setting.name, "is required: "+setting.reason)
		}
	}
	for _, setting := range []struct {
		name  string
		value checksandalerts.Duration
	}{{"keep_event_history_for", c.KeepEventHistoryFor}, {"keep_undelivered_alerts_for", c.KeepUndeliveredAlertsFor}} {
		if setting.value.Value() <= 0 {
			return c.ErrorAt(setting.name, "must be positive")
		}
	}
	if SeverityRank(c.AlertSeverityWhenThisNodeGoesSilent) < 0 {
		return c.ErrorAt("alert_severity_when_this_node_goes_silent", "required: info, warn, or sos")
	}
	if SeverityRank(c.AlertSeverityWhenThisNodeHasRebooted) < 0 {
		return c.ErrorAt("alert_severity_when_this_node_has_rebooted", "required: info, warn, or sos")
	}
	// Every node states how much measurement history it keeps: the two
	// settings have no default, so a reader finds them in the node's file.
	if !c.authored("keep_measurement_history_for") {
		return c.ErrorAt("keep_measurement_history_for", "is required: how long this node keeps the readings of its measurements, such as 7d")
	}
	if c.KeepMeasurementHistoryFor.Value() <= 0 {
		return c.ErrorAt("keep_measurement_history_for", "must be positive")
	}
	if !c.authored("measurement_history_size_limit") {
		return c.ErrorAt("measurement_history_size_limit", "is required: how much of this node's disk the readings of its measurements may take, such as 256MiB")
	}
	if c.RaiseToSosAfterThisNodeIsSilentFor != "" || c.authored("raise_to_sos_after_this_node_is_silent_for") {
		if v, e := checksandalerts.ParseDuration(string(c.RaiseToSosAfterThisNodeIsSilentFor)); e != nil || v < time.Second {
			return c.ErrorAt("raise_to_sos_after_this_node_is_silent_for", "expected a duration of at least 1s")
		}
	}
	if err := c.validateDestinationNames("send_silence_alerts_of_this_node_only_to_destinations", c.SendSilenceAlertsOfThisNodeOnlyToDestinations); err != nil {
		return err
	}
	if problem := nodeGroupsProblem(c.ThisNodeIsInNodeGroups); problem != "" {
		return c.ErrorAt("this_node_is_in_node_groups", problem)
	}
	if problem := raiseWhenNodeGroupsAreSilentProblem(c.ThisNodeIsInNodeGroups, c.RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent); problem != "" {
		return c.ErrorAt("raise_to_sos_when_every_node_of_these_node_groups_is_silent", problem)
	}
	if len(c.CheckGroups) == 0 {
		return c.ErrorAt("check_groups", "expected at least one named check group")
	}
	if err := c.ValidateAlerts(false); err != nil {
		return err
	}
	if h := c.FleetHubRole; h != nil {
		if h.ListenOnAddress == "" {
			return c.ErrorAt("fleet_hub_role.listen_on_address", "is required: the local host and port where the hub accepts reports; add listen_on_address: '10.0.0.5:8445', or listen_on_address: '0.0.0.0:8445' for every interface")
		}
		if problem := listeningAddressProblem(h.ListenOnAddress); problem != "" {
			return c.ErrorAt("fleet_hub_role.listen_on_address", problem)
		}
		if node, _, e := net.SplitHostPort(h.AddressForNodes); e == nil && node == "" {
			return c.ErrorAt("fleet_hub_role.address_for_nodes", "needs the host name or address that nodes connect to")
		}
		if e := checksandalerts.HostPort(h.AddressForNodes, 1); e != nil {
			return c.ErrorAt("fleet_hub_role.address_for_nodes", e.Error())
		}
		for _, setting := range []struct{ name, reason string }{
			{"report_every", "how often nodes report to the hub, such as 30s"},
			{"alert_after_hub_unreachable_for", "how long the hub-outage alerter waits before alerting, such as 3m"},
			{"send_alert_without_hub_after_waiting_for", "how long nodes wait before delivering directly, such as 30s"},
			{"undelivered_alerts_of_nodes_content_size_limit", "how much alert content the hub keeps for nodes awaiting delivery, such as 32MiB"},
		} {
			if !c.authored("fleet_hub_role." + setting.name) {
				return c.ErrorAt("fleet_hub_role."+setting.name, "is required: "+setting.reason)
			}
		}
		if field, message := h.Problem(); message != "" {
			return c.ErrorAt("fleet_hub_role."+field, message)
		}
		// A hub states how much measurement history of its nodes it keeps:
		// the two settings have no default.
		if !c.authored("fleet_hub_role.keep_measurement_history_of_nodes_for") {
			return c.ErrorAt("fleet_hub_role.keep_measurement_history_of_nodes_for", "is required: how long the hub keeps the readings that the nodes send, such as 7d")
		}
		if h.KeepMeasurementHistoryOfNodesFor.Value() <= 0 {
			return c.ErrorAt("fleet_hub_role.keep_measurement_history_of_nodes_for", "must be positive")
		}
		if !c.authored("fleet_hub_role.measurement_history_of_nodes_size_limit") {
			return c.ErrorAt("fleet_hub_role.measurement_history_of_nodes_size_limit", "is required: how much of the hub's disk the readings of the nodes may take, such as 20GiB")
		}
		if h.KeepEventHistoryOfNodesFor == "" {
			h.KeepEventHistoryOfNodesFor = defaultKeepEventHistoryOfNodesFor
		}
		if h.EventHistoryOfNodesContentSizeLimit == "" {
			h.EventHistoryOfNodesContentSizeLimit = defaultEventHistoryOfNodesContentSizeLimit
		}

		if h.KeepEventHistoryOfNodesFor.Value() <= 0 {
			return c.ErrorAt("fleet_hub_role.keep_event_history_of_nodes_for", "must be positive")
		}
		if h.RaiseToSosWhenSilentNodesReachCount != nil && h.RaiseToSosWhenSilentNodesReachPercent != nil {
			return c.ErrorAt("fleet_hub_role.raise_to_sos_when_silent_nodes_reach_count", "both silence thresholds are set; keep only raise_to_sos_when_silent_nodes_reach_count or raise_to_sos_when_silent_nodes_reach_percent to choose when silence alerts rise to sos")
		}
		if h.RaiseToSosWhenSilentNodesReachCount != nil && *h.RaiseToSosWhenSilentNodesReachCount < 1 {
			return c.ErrorAt("fleet_hub_role.raise_to_sos_when_silent_nodes_reach_count", "the number of silent nodes that raises silence alerts to sos must be at least 1; set raise_to_sos_when_silent_nodes_reach_count: 5")
		}
		if h.RaiseToSosWhenSilentNodesReachPercent != nil && (*h.RaiseToSosWhenSilentNodesReachPercent <= 0 || *h.RaiseToSosWhenSilentNodesReachPercent > 100) {
			return c.ErrorAt("fleet_hub_role.raise_to_sos_when_silent_nodes_reach_percent", "the percentage of silent nodes that raises silence alerts to sos must be above 0 and at most 100; set raise_to_sos_when_silent_nodes_reach_percent: 5")
		}
	}
	// state.db holds the event history, the outbox, and on a hub the alerts
	// of nodes that wait for delivery, beside every check's state, and
	// SQLite needs free pages to rewrite them. So the file may grow to at
	// least twice what these stores may hold.
	limits := c.StorageLimits()
	if need := 2 * (limits.EventHistoryBytes + limits.UndeliveredAlertsBytes + limits.UndeliveredAlertsOfNodesBytes); limits.StateDatabaseBytes < need {
		stores := "event_history_content_size_limit and undelivered_alerts_content_size_limit"
		if c.FleetHubRole != nil {
			stores = "event_history_content_size_limit, undelivered_alerts_content_size_limit, and fleet_hub_role.undelivered_alerts_of_nodes_content_size_limit"
		}
		return c.ErrorAt("state_database_size_limit", fmt.Sprintf("must be at least %dMiB, twice the sum of %s, which state.db holds beside every check's state", need/(1024*1024), stores))
	}
	if c.JoinFleetHub != nil {
		if c.FleetHubRole != nil {
			return c.ErrorAt("join_fleet_hub", "a hub cannot join another hub")
		}
		if !filepath.IsAbs(c.JoinFleetHub.JoinCodeFile) {
			return c.ErrorAt("join_fleet_hub.join_code_file", "must be absolute")
		}
	}

	if r := c.HeartbeatReceiver; r != nil {
		if problem := listeningAddressProblem(r.ListenOnAddress); problem != "" {
			return c.ErrorAt("heartbeat_receiver.listen_on_address", problem)
		}
		if !filepath.IsAbs(r.TLSCertificateFile) {
			return c.ErrorAt("heartbeat_receiver.tls_certificate_file", "must be absolute")
		}
		if !filepath.IsAbs(r.TLSPrivateKeyFile) {
			return c.ErrorAt("heartbeat_receiver.tls_private_key_file", "must be absolute")
		}
	}
	for _, n := range checksandalerts.Names(c.CheckGroups) {
		ch := c.CheckGroups[n]
		path := "check_groups." + n
		fail := func(field, message string) error {
			if field != "" {
				field = "." + field
			}
			return c.ErrorAt(path+field, message)
		}
		if !checksandalerts.NamePattern.MatchString(n) {
			return fail("", "name must use letters, digits, hyphens or underscores (1..64 characters)")
		}
		if n == checksandalerts.EveryCheckGroup {
			return fail("", "name is reserved for whole-node maintenance")
		}
		if n == checksandalerts.NodeReportsCheckGroup {
			return fail("", "name is reserved for the built-in node_reports check group")
		}
		if ch.Heartbeat != nil {
			// A heartbeat has no executions or confirmation periods: its
			// deadline is its only rule, so timing and limits do not apply
			// to it.
			for _, setting := range timingSettings {
				if c.authored(path + "." + setting.name) {
					return fail(setting.name, "does not apply to a heartbeat check")
				}
			}
			for _, field := range append([]string{quietWhileChecksFailSetting, "only_record_readings"}, checksandalerts.LimitSettings...) {
				if c.authored(path + "." + field) {
					return fail(field, "does not apply to a heartbeat check")
				}
			}
		} else {
			// Ordinary check groups state four timings: there are no
			// node-wide defaults, so a reader finds them in the group's file.
			for _, setting := range timingSettings {
				if setting.name == "alert_after_failing_for" && ch.Check.OpensIncidentAtFirstEvent() {
					continue
				}
				if !c.authored(path + "." + setting.name) {
					return fail(setting.name, "is required: "+setting.meaning)
				}
			}
			if field, msg := timingError(ch.Timing); msg != "" {
				return fail(field, msg)
			}
		}
		if ch.Checks == nil {
			ch.Checks = map[string]checksandalerts.NamedCheck{}
		}
		// A direct check or a heartbeat is the group mapping itself, so its
		// alert severity settings and limits are written there; a group of
		// named checks has them on each check.
		direct := ch.Check.Inputs != nil
		if !direct && ch.Heartbeat == nil {
			for _, field := range append([]string{"only_record_readings", "alert_severity", "raise_to_sos_after_failing_for", "send_alerts_only_to_destinations", quietWhileChecksFailSetting}, checksandalerts.LimitSettings...) {
				if c.authored(path + "." + field) {
					return fail(field, "set "+field+" on each check under checks, not on the check group")
				}
			}
		}
		if direct {
			if len(ch.Checks) > 0 || ch.Heartbeat != nil {
				return fail("", "a direct check cannot share a check group with named checks or a heartbeat")
			}
			ch.Checks[ch.Check.ModuleName()] = checksandalerts.NamedCheck{Check: ch.Check}
			ch.Check = checksandalerts.Check{}
		}
		if h := ch.Heartbeat; h != nil {
			if h.MaxTimeBetweenHeartbeats.Value() <= 0 {
				return fail("heartbeat.max_time_between_heartbeats", "must be positive")
			}
			if !filepath.IsAbs(h.ExpectedBearerTokenFile) {
				return fail("heartbeat.expected_bearer_token_file", "must be absolute")
			}
			if ch.RecoveryCommand != nil {
				return fail("recovery_command", "does not apply to a heartbeat check")
			}
			if len(ch.Checks) > 0 || ch.RestartSystemdUnit != nil {
				return fail("heartbeat", "must be its own check group")
			}
			if e := c.validateSeverity(path, ch.Check); e != nil {
				return e
			}
			if e := c.validateNamedDestinations(path, ch.Check); e != nil {
				return e
			}
		}
		if len(ch.Checks) == 0 && ch.Heartbeat == nil {
			return fail("", "no checks defined")
		}
		for _, cn := range checksandalerts.Names(ch.Checks) {
			co := ch.Checks[cn]
			base := path + ".checks." + cn
			if direct {
				base = path
			}
			if !checksandalerts.NamePattern.MatchString(cn) || co.Inputs == nil {
				return c.ErrorAt(base, "each check needs a valid name and exactly one module, such as http or shell")
			}
			for _, field := range checksandalerts.LimitSettings {
				if c.authored(base+"."+field) && len(co.Limits(field)) == 0 {
					return c.ErrorAt(base+"."+field, "cannot be empty: name a measurement and its limit, or remove "+field)
				}
			}
			if module, ok := co.Inputs.(checksandalerts.TakesTimeForEachRun); ok {
				if taken, what := module.TimeOfEachRun(); taken > 0 && ch.TimeLimitPerRun.Value() <= taken {
					return fail("time_limit_per_run", "must exceed "+taken.String()+" for "+what)
				}
			}
			// The path of an input is below the module's name.
			inputs := base + "." + co.ModuleName()
			if problem := co.Inputs.Validate(checksandalerts.InputValidation{Authored: func(path string) bool { return c.authored(inputs + "." + path) }, UsedAs: checksandalerts.UsedAsCheck, Modules: c.Modules}); problem != nil {
				return c.ErrorAt(strings.TrimSuffix(inputs+"."+problem.Path, "."), problem.Message)
			}
			if problem := co.ValidateLimits(); problem != nil {
				return c.ErrorAt(base+"."+problem.Path, problem.Message)
			}
			if module, ok := co.Inputs.(checksandalerts.ValidatesWholeCheck); ok {
				if problem := module.ValidateCheck(co.Check); problem != nil {
					return c.ErrorAt(base+"."+problem.Path, problem.Message)
				}
			}
			if e := c.validateSeverity(base, co.Check); e != nil {
				return e
			}
			if e := c.validateNamedDestinations(base, co.Check); e != nil {
				return e
			}
			if co.OpensIncidentAtFirstEvent() && c.authored(base+".alert_after_failing_for") {
				return c.ErrorAt(base+".alert_after_failing_for", "an event check opens its incident at the first new event; remove alert_after_failing_for from this check. Set recovered_after_healthy_for to keep the incident open after the last event")
			}
			// A named check may state its own waiting times; the others
			// are those of its check group.
			co.AlertAfterFailingForStatedOn, co.RecoveredAfterHealthyForStatedOn = "check_group", "check_group"
			if co.AlertAfterFailingFor != "" {
				co.AlertAfterFailingForStatedOn = "check"
			}
			if co.RecoveredAfterHealthyFor != "" {
				co.RecoveredAfterHealthyForStatedOn = "check"
			}
			co.AlertAfterFailingFor, co.RecoveredAfterHealthyFor = co.WaitingTimes(ch.Timing)
			if co.OpensIncidentAtFirstEvent() {
				co.AlertAfterFailingForStatedOn = ""
			}
			ch.Checks[cn] = co
		}
		if r := ch.RestartSystemdUnit; r != nil {
			if r.SystemdUnit == "" {
				return fail("restart_systemd_unit.systemd_unit", "is required: the systemd unit that MMWatchdog restarts, such as myapp.service")
			}
			if !primitives.UnitPattern.MatchString(r.SystemdUnit) {
				return fail("restart_systemd_unit.systemd_unit", "must be a systemd .service unit name, such as myapp.service")
			}
			if r.TriggeredByChecks == nil {
				return fail("restart_systemd_unit.triggered_by_checks", "is required: the checks of this check group whose failure calls for the restart")
			}
			if !c.authored(path + ".restart_systemd_unit.restart_after_failing_for") {
				return fail("restart_systemd_unit.restart_after_failing_for", "is required: how long a check in triggered_by_checks must be failing before MMWatchdog restarts the unit, such as 1m")
			}
			if err := c.validateBudget(path+".restart_systemd_unit", ch, &r.RecoveryAttemptSettings); err != nil {
				return err
			}
		}
		if r := ch.RecoveryCommand; r != nil {
			if strings.TrimSpace(r.Script) == "" {
				return fail("recovery_command.script", "is required")
			}
			if !c.authored(path + ".recovery_command.run_after_failing_for") {
				return fail("recovery_command.run_after_failing_for", "is required: how long a check in triggered_by_checks must be failing before MMWatchdog runs the command, such as 1m")
			}
			if r.TimeLimitPerAttempt == "" {
				return fail("recovery_command.time_limit_per_attempt", "is required: how long each recovery command may run; add time_limit_per_attempt: 1m")
			}
			if r.TimeLimitPerAttempt.Value() <= 0 {
				return fail("recovery_command.time_limit_per_attempt", "must be positive")
			}
			if problem := checksandalerts.ValidateExecOptions(&r.ExecOptions); problem != nil {
				return fail("recovery_command."+problem.Path, problem.Message)
			}
			if r.TriggeredByChecks == nil {
				return fail("recovery_command.triggered_by_checks", "is required: the checks whose failure calls for the command; add triggered_by_checks: [check_name]")
			}
			if err := c.validateBudget(path+".recovery_command", ch, &r.RecoveryAttemptSettings); err != nil {
				return err
			}

		}
		c.CheckGroups[n] = ch
	}
	if err := c.validateSharedRestartSettings(); err != nil {
		return err
	}
	return c.validateChecksNamedAsCauses()
}

// validateSharedRestartSettings makes one unit's shared budget unambiguous.
func (c *Config) validateSharedRestartSettings() error {
	firstGroupByUnit := map[string]string{}
	for _, group := range checksandalerts.Names(c.CheckGroups) {
		restart := c.CheckGroups[group].RestartSystemdUnit
		if restart == nil {
			continue
		}
		first, exists := firstGroupByUnit[restart.SystemdUnit]
		if !exists {
			firstGroupByUnit[restart.SystemdUnit] = group
			continue
		}
		previous := c.CheckGroups[first].RestartSystemdUnit
		for _, setting := range []struct {
			name string
			same bool
		}{
			{"max_attempts", restart.Attempts == previous.Attempts},
			{"min_time_between_attempts", restart.MinTimeBetweenAttempts.Value() == previous.MinTimeBetweenAttempts.Value()},
			{"reset_attempts_after_healthy_for", restart.ResetAttemptsAfterHealthyFor.Value() == previous.ResetAttemptsAfterHealthyFor.Value()},
		} {
			if !setting.same {
				path := "check_groups." + group + ".restart_systemd_unit." + setting.name
				other := "check_groups." + first + ".restart_systemd_unit." + setting.name
				return c.ErrorAt(path, fmt.Sprintf("differs from %s at %s: restart actions for %s share an attempt budget. Set %s to the same value in the restart_systemd_unit of both check groups", other, c.Source(other), restart.SystemdUnit, setting.name))
			}
		}
	}
	return nil
}

// validateBudget checks the required budget of the restart or recovery
// command at path. Every named trigger must be able to fail.
func (c *Config) validateBudget(path string, ch checksandalerts.CheckGroup, r *checksandalerts.RecoveryAttemptSettings) error {
	for _, setting := range []struct{ name, reason string }{
		{"max_attempts", "how many recovery attempts MMWatchdog may make; add max_attempts: 3, or max_attempts: unlimited"},
		{"min_time_between_attempts", "how long MMWatchdog waits between recovery attempts; add min_time_between_attempts: 1m"},
		{"reset_attempts_after_healthy_for", "how long the checks must stay healthy before attempts reset; add reset_attempts_after_healthy_for: 10m"},
	} {
		if !c.authored(path + "." + setting.name) {
			return c.ErrorAt(path+"."+setting.name, "is required: "+setting.reason)
		}
	}
	if len(r.TriggeredByChecks) == 0 {
		return c.ErrorAt(path+".triggered_by_checks", "cannot be empty")
	}
	if _, ok := r.Attempts.Limit(); !ok {
		return c.ErrorAt(path+".max_attempts", "expected a whole number of at least 1, or unlimited")
	}
	if r.MinTimeBetweenAttempts.Value() <= 0 {
		return c.ErrorAt(path+".min_time_between_attempts", "must be positive")
	}
	if r.ResetAttemptsAfterHealthyFor.Value() <= 0 {
		return c.ErrorAt(path+".reset_attempts_after_healthy_for", "must be positive")
	}
	for i, cn := range r.TriggeredByChecks {
		field := fmt.Sprintf("%s.triggered_by_checks.%d", path, i)
		co, ok := ch.Checks[cn]
		if !ok {
			return c.ErrorAt(field, "unknown check: "+cn)
		}
		if co.RecordsOnly() {
			return c.ErrorAt(field, "check "+cn+" only records its readings and never fails; give it a limit to use it for recovery")
		}
	}
	return nil
}

// ValidateAlerts checks the destinations. fromTheHub is set where a joined
// node checks its copy of the hub's destinations: a module that is not core
// need not be on this node, and its inputs were the hub's to check.
func (c *Config) ValidateAlerts(fromTheHub bool) error {
	for _, n := range checksandalerts.Names(c.Alerts.Destinations) {
		d := c.Alerts.Destinations[n]
		path := "destinations." + n
		if !checksandalerts.NamePattern.MatchString(n) {
			return c.ErrorAt(path, "invalid destination name")
		}
		if n == HubDestination {
			return c.ErrorAt(path, "name is reserved for the fleet hub")
		}
		if d.Inputs == nil {
			names := checksandalerts.DestinationModuleNames()
			return c.ErrorAt(path, "expected one module: "+strings.Join(names[:len(names)-1], ", ")+", or "+names[len(names)-1])
		}
		module := d.Module()
		// The path of an input is below the module's name.
		inputs := path + "." + module.Name
		if problem := d.Inputs.Validate(checksandalerts.InputValidation{Authored: func(input string) bool { return c.authored(inputs + "." + input) }, UsedAs: checksandalerts.UsedAsAlertDestination, Modules: c.Modules, WithoutModules: fromTheHub}); problem != nil {
			return c.ErrorAt(strings.TrimSuffix(inputs+"."+problem.Path, "."), problem.Message)
		}
		if whole, ok := d.Inputs.(checksandalerts.ValidatesWholeDestination); ok {
			if problem := whole.ValidateDestination(d); problem != nil {
				return c.ErrorAt(path+"."+problem.Path, problem.Message)
			}
		}
		limit, limitName := &d.TimeLimitPerDeliveryAttempt, "time_limit_per_delivery_attempt"
		if *limit == "" {
			*limit = "5s"
			if module.DefaultTimeLimit != "" {
				*limit = module.DefaultTimeLimit
			}
		}
		if limit.Value() <= 0 {
			return c.ErrorAt(path+"."+limitName, "must be positive")
		}
		if d.MMHotline() == nil {
			if d.RemindEvery == "" {
				return c.ErrorAt(path+".remind_every", "is required: how often to repeat an open incident; add remind_every: 1h, or remind_every: never")
			}
			if d.RemindEvery != "never" && d.RemindEvery.Value() <= 0 {
				return c.ErrorAt(path+".remind_every", "must be a positive duration or never")
			}
		}
		if d.AlertSeverities == nil || !ValidSeverityList(d.AlertSeverities) {
			return c.ErrorAt(path+".alert_severities", "required: a list of distinct severities from info, warn, sos, such as [info, warn, sos]. Use [] to receive alerts only from checks that name the destination")
		}
		c.Alerts.Destinations[n] = d
	}
	return nil
}

// AlertTitlesThatWillBeShortened names every check whose longest alert
// title on the node has more than AlertTitleLimit characters, so the person
// who chose the names learns of the cut before any alert goes out.
func (c *Config) AlertTitlesThatWillBeShortened(node string) []checksandalerts.AlertTitleThatWillBeShortened {
	out := []checksandalerts.AlertTitleThatWillBeShortened{}
	defs := CheckDefinitions(c)
	for _, key := range checksandalerts.Names(defs) {
		def := defs[key]
		var longest []checksandalerts.TitlePart
		most := 0
		for _, kind := range []string{"failed", "recovered", "cannot_check", "can_check_again", checksandalerts.FailureIncidentEvents.Raised, checksandalerts.CannotCheckIncidentEvents.Raised, "closed_without_recovery", "reminder"} {
			parts := checksandalerts.AlertTitleParts(checksandalerts.Event{Node: node, CheckGroup: def.CheckGroup, Check: def.Name, Type: kind, AlertSeverity: "sos"}, "")
			n := 0
			for _, part := range parts {
				n += utf8.RuneCountInString(part.Text)
			}
			if n > most {
				longest, most = parts, n
			}
		}
		if most > checksandalerts.AlertTitleLimit {
			title, _ := checksandalerts.FitTitle(longest)
			out = append(out, checksandalerts.AlertTitleThatWillBeShortened{CheckGroup: def.CheckGroup, Check: def.Name, Characters: most, Title: title})
		}
	}
	return out
}

// Missing fields point to their nearest authored parent, including fragments.
func (c *Config) Source(path string) checksandalerts.SourceLocation {
	for {
		if loc, ok := c.Sources[path]; ok {
			return loc
		}
		if path == "" {
			return checksandalerts.SourceLocation{}
		}
		i := strings.LastIndex(path, ".")
		if i < 0 {
			path = ""
		} else {
			path = path[:i]
		}
	}
}

func (c *Config) ErrorAt(path, message string) error {
	return fmt.Errorf("%s: %s: %s", c.Source(path), path, message)
}

func (c *Config) CheckSource(group, check string) checksandalerts.SourceLocation {
	// Fleet checks are generated from the hub and membership settings.
	if group == checksandalerts.NodeReportsCheckGroup && c.FleetHubRole != nil {
		return c.Source("fleet_hub_role")
	}
	if group == checksandalerts.NodeReportsCheckGroup {
		return c.Source("join_fleet_hub")
	}
	base := "check_groups." + group
	if check == "heartbeat" {
		return c.Source(base + "." + check)
	}
	if _, ok := c.Sources[base+".checks."+check]; ok {
		return c.Source(base + ".checks." + check)
	}
	// A check that is written directly on its check group has no authored
	// checks.check mapping, and it has the name of its module.
	if loc, ok := c.Sources[base+"."+check]; ok {
		return loc
	}
	return c.Source(base)
}

// authored reports whether the setting at path is written in the
// configuration, so an empty or zero value there is the author's.
func (c *Config) authored(path string) bool {
	return c.Sources[path].Line != 0
}

// SecretFilePermissionErrors reports each exposed secret file once, with
// its configuration location. Validation refuses; startup logs and continues.
func (c *Config) SecretFilePermissionErrors() []error {
	type secretFile struct {
		path   string
		access checksandalerts.SecretFileAccess
	}
	files := map[string]secretFile{}
	if receiver := c.HeartbeatReceiver; receiver != nil {
		files["heartbeat_receiver.tls_private_key_file"] = secretFile{receiver.TLSPrivateKeyFile, receiver.SecretFileAccess}
	}
	if join := c.JoinFleetHub; join != nil {
		files["join_fleet_hub.join_code_file"] = secretFile{join.JoinCodeFile, join.SecretFileAccess}
	}
	for name, destination := range c.Alerts.Destinations {
		if destination.Inputs == nil {
			continue
		}
		access := destination.SecretFileAccess
		if p := destination.MMHotline(); p != nil {
			access = p.SecretFileAccess
		}
		for input, path := range destination.Inputs.CredentialFiles() {
			if checksandalerts.IsCertificateAuthorityFileInput(input) {
				continue
			}
			files["destinations."+name+"."+destination.PathOfInput(input)] = secretFile{path, access}
		}
	}
	for name, group := range c.CheckGroups {
		if group.Heartbeat != nil {
			files["check_groups."+name+".heartbeat.expected_bearer_token_file"] = secretFile{group.Heartbeat.ExpectedBearerTokenFile, group.SecretFileAccess}
		}
		for checkName, check := range group.Checks {
			module, ok := check.Inputs.(checksandalerts.NamesCredentialFiles)
			if !ok {
				continue
			}
			base := "check_groups." + name + ".checks." + checkName
			if _, written := c.Sources[base]; !written {
				base = "check_groups." + name
			}
			for input, path := range module.CredentialFiles() {
				if checksandalerts.IsCertificateAuthorityFileInput(input) {
					continue
				}
				files[base+"."+check.ModuleName()+"."+input] = secretFile{path, check.SecretFileAccess}
			}
		}
	}
	var problems []error
	reported := map[string]bool{}
	for _, setting := range checksandalerts.Names(files) {
		file := files[setting]
		if file.path == "" || reported[file.path] {
			continue
		}
		if err := file.access.ValidateSecretFile(file.path); err != nil {
			problems = append(problems, c.ErrorAt(setting, err.Error()))
			reported[file.path] = true
		}
	}
	return problems
}

// FilesystemsMissingFromFilesystemsToIgnore names, for every
// every_disk_and_network_filesystem check, each filesystem that has its own
// node_filesystem check with a limit on the node and is not in the check's
// filesystems_to_ignore. It is a validation warning: the configuration
// stays valid.
func (c *Config) FilesystemsMissingFromFilesystemsToIgnore() []checksandalerts.FilesystemMissingFromFilesystemsToIgnore {
	// The first node_filesystem check with a limit of each filesystem.
	own := map[string]string{}
	for _, group := range checksandalerts.Names(c.CheckGroups) {
		for _, name := range checksandalerts.Names(c.CheckGroups[group].Checks) {
			check := c.CheckGroups[group].Checks[name]
			if filesystem, ok := check.Inputs.(*checksandalerts.NodeFilesystem); ok && check.HasLimit() && check.AlertSeverity != "" {
				if path := filepath.Clean(string(*filesystem)); own[path] == "" {
					own[path] = group + "/" + name
				}
			}
		}
	}
	out := []checksandalerts.FilesystemMissingFromFilesystemsToIgnore{}
	for _, group := range checksandalerts.Names(c.CheckGroups) {
		for _, name := range checksandalerts.Names(c.CheckGroups[group].Checks) {
			every, ok := c.CheckGroups[group].Checks[name].Inputs.(*checksandalerts.EveryDiskAndNetworkFilesystem)
			if !ok {
				continue
			}
			for _, path := range checksandalerts.Names(own) {
				if !slices.Contains(every.FilesystemsToIgnore, path) {
					out = append(out, checksandalerts.FilesystemMissingFromFilesystemsToIgnore{CheckGroup: group, Check: name, Filesystem: path, NodeFilesystemCheck: own[path]})
				}
			}
		}
	}
	return out
}

// SystemdUnitsMissingFromUnitsToIgnore names, for every
// every_systemd_unit check, each unit that has its own
// systemd_unit_state check on the node and is not in the check's
// systemd_units_to_ignore. It is a validation warning: the configuration
// stays valid.
func (c *Config) SystemdUnitsMissingFromUnitsToIgnore() []checksandalerts.SystemdUnitMissingFromUnitsToIgnore {
	// The first systemd_unit_state check of each unit, by name.
	stateChecks := map[string]string{}
	for _, group := range checksandalerts.Names(c.CheckGroups) {
		for _, name := range checksandalerts.Names(c.CheckGroups[group].Checks) {
			if unit, ok := c.CheckGroups[group].Checks[name].Inputs.(*checksandalerts.SystemdUnitState); ok && stateChecks[unit.SystemdUnit] == "" {
				stateChecks[unit.SystemdUnit] = group + "/" + name
			}
		}
	}
	out := []checksandalerts.SystemdUnitMissingFromUnitsToIgnore{}
	for _, group := range checksandalerts.Names(c.CheckGroups) {
		for _, name := range checksandalerts.Names(c.CheckGroups[group].Checks) {
			n, ok := c.CheckGroups[group].Checks[name].Inputs.(*checksandalerts.EverySystemdUnit)
			if !ok {
				continue
			}
			for _, unit := range checksandalerts.Names(stateChecks) {
				if !slices.Contains(n.SystemdUnitsToIgnore, unit) {
					out = append(out, checksandalerts.SystemdUnitMissingFromUnitsToIgnore{CheckGroup: group, Check: name, SystemdUnit: unit, SystemdUnitStateCheck: stateChecks[unit]})
				}
			}
		}
	}
	return out
}

// NodeFleetSettings are the settings of a node that the fleet needs from it:
// the severity of the alert when the node goes silent, the time after which
// that alert is raised to sos, its destinations, the node groups, and the
// node groups whose silence raises that alert to sos. A node sends them when
// it joins and in every report. So the hub knows them before the node's
// first report, and keeps them, from the node's latest saved report, while
// the node is silent and across its own restarts.
type NodeFleetSettings struct {
	AlertSeverityWhenThisNodeGoesSilent              string                   `json:"alert_severity_when_this_node_goes_silent"`
	SendSilenceAlertsOfThisNodeOnlyToDestinations    *[]string                `json:"send_silence_alerts_of_this_node_only_to_destinations,omitempty"`
	RaiseToSosAfterThisNodeIsSilentFor               checksandalerts.Duration `json:"raise_to_sos_after_this_node_is_silent_for,omitempty"`
	ThisNodeIsInNodeGroups                           []string                 `json:"this_node_is_in_node_groups,omitempty"`
	RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent []string                 `json:"raise_to_sos_when_every_node_of_these_node_groups_is_silent,omitempty"`
}

// Problem says what is wrong with settings that another machine sent, as the
// rest of a sentence that starts with "join" or "report", or "" when they are
// valid.
func (s NodeFleetSettings) Problem() string {
	if SeverityRank(s.AlertSeverityWhenThisNodeGoesSilent) < 0 {
		return "needs alert_severity_when_this_node_goes_silent: info, warn, or sos"
	}
	if s.RaiseToSosAfterThisNodeIsSilentFor != "" {
		if v, e := checksandalerts.ParseDuration(string(s.RaiseToSosAfterThisNodeIsSilentFor)); e != nil || v < time.Second {
			return "has an invalid raise_to_sos_after_this_node_is_silent_for: expected a duration of at least 1s"
		}
	}
	if problem := DestinationNamesProblem(s.SendSilenceAlertsOfThisNodeOnlyToDestinations); problem != "" {
		return "has an invalid send_silence_alerts_of_this_node_only_to_destinations: " + problem
	}
	if problem := nodeGroupsProblem(s.ThisNodeIsInNodeGroups); problem != "" {
		return "has an invalid this_node_is_in_node_groups: " + problem
	}
	if problem := raiseWhenNodeGroupsAreSilentProblem(s.ThisNodeIsInNodeGroups, s.RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent); problem != "" {
		return "has an invalid raise_to_sos_when_every_node_of_these_node_groups_is_silent: " + problem
	}
	return ""
}

// Silence is the alert severity settings of the check that watches the node:
// its silence severity, raised to sos after its time. The failing time of
// that check is the time the node has been silent.
func (s NodeFleetSettings) Silence() checksandalerts.AlertSeveritySettings {
	return checksandalerts.AlertSeveritySettings{AlertSeverity: s.AlertSeverityWhenThisNodeGoesSilent, RaiseToSosAfterFailingFor: s.RaiseToSosAfterThisNodeIsSilentFor}
}

// MaxNodeGroupsOfANode is how many node groups one node can name. A join
// request is input from a machine that the hub does not know yet, so its
// size is fixed (joinLimit), and a node that validates must be able to join;
// a fleet view also shows the node groups of every node that it lists.
const MaxNodeGroupsOfANode = 100

// nodeGroupsProblem says what is wrong with the node groups that a node
// names, or "" when they are valid: each name follows the rules for the name
// of a check, no name is there twice, and there are at most
// MaxNodeGroupsOfANode. A node may name no node group.
func nodeGroupsProblem(nodeGroups []string) string {
	if len(nodeGroups) > MaxNodeGroupsOfANode {
		return fmt.Sprintf("names %d node groups; a node can be in at most %d", len(nodeGroups), MaxNodeGroupsOfANode)
	}
	seen := map[string]bool{}
	for _, name := range nodeGroups {
		switch {
		case !checksandalerts.NamePattern.MatchString(name):
			return fmt.Sprintf("%q is not a valid name of a node group: use letters, digits, hyphens or underscores (1..64 characters)", name)
		case seen[name]:
			return fmt.Sprintf("names the node group %q twice", name)
		}
		seen[name] = true
	}
	return ""
}

// raiseWhenNodeGroupsAreSilentProblem says what is wrong with the node
// groups of raise_to_sos_when_every_node_of_these_node_groups_is_silent, or
// "" when they are valid: the rules of nodeGroupsProblem, and each name is
// one of the node groups that the node is in. The role that puts a node in a
// node group states there what the silence of that node group means.
func raiseWhenNodeGroupsAreSilentProblem(nodeGroups, raiseWhenSilent []string) string {
	if problem := nodeGroupsProblem(raiseWhenSilent); problem != "" {
		return problem
	}
	for _, name := range raiseWhenSilent {
		if !slices.Contains(nodeGroups, name) {
			return fmt.Sprintf("names the node group %q, which the node is not in: add the node group to this_node_is_in_node_groups, or remove the node group from raise_to_sos_when_every_node_of_these_node_groups_is_silent", name)
		}
	}
	return ""
}

const ScheduleInterval = 100 * time.Millisecond

func Fingerprint(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func CheckDefinitions(c *Config) map[string]Definition {
	defs := map[string]Definition{}
	for name, ch := range c.CheckGroups {
		if ch.Heartbeat != nil {
			// No timing: a missed deadline fails and a receipt recovers at once.
			defs[name+"/heartbeat"] = Definition{CheckGroup: name, Name: "heartbeat", Heartbeat: ch.Heartbeat, Severity: ch.Check.AlertSeveritySettings, SendAlertsOnlyToDestinations: ch.Check.SendAlertsOnlyToDestinations}
		}
		for n, co := range ch.Checks {
			// The alert severity settings are not part of the identity.
			severity, check := co.AlertSeveritySettings, co.Check
			check.AlertSeveritySettings, check.SendAlertsOnlyToDestinations, check.NoAlertsOrRecoveryActionsWhileTheseChecksFail = checksandalerts.AlertSeveritySettings{}, nil, nil
			// The check's own waiting times, or those of its check group.
			timing := ch.Timing
			timing.AlertAfterFailingFor, timing.RecoveredAfterHealthyFor = co.WaitingTimes(ch.Timing)
			// A module can leave the written form of an input out of the
			// identity, such as the order of a list.
			if module, ok := check.Inputs.(checksandalerts.NormalizedForIdentity); ok {
				check.Inputs = module.Identity()
			}
			defs[name+"/"+n] = Definition{CheckGroup: name, Name: n, Check: check, Timing: timing, Severity: severity, SendAlertsOnlyToDestinations: co.SendAlertsOnlyToDestinations, QuietWhileTheseChecksFail: co.NoAlertsOrRecoveryActionsWhileTheseChecksFail}
		}
	}
	// Each check that others name as their cause learns who they are, in
	// order of name, for its alert.
	for _, key := range checksandalerts.Names(defs) {
		for _, cause := range defs[key].QuietWhileTheseChecksFail {
			if d, exists := defs[cause]; exists {
				d.KeepsTheseChecksQuiet = append(d.KeepsTheseChecksQuiet, key)
				defs[cause] = d
			}
		}
	}
	return defs
}

type Definition struct {
	CheckGroup, Name string
	Check            checksandalerts.Check
	Timing           checksandalerts.Timing
	Heartbeat        *checksandalerts.Heartbeat
	// Joined identifies a joined node's membership, so a replacement is a
	// new definition.
	Joined string `json:",omitempty"`
	// Severity is the check's alert severity settings, kept out of its
	// identity, so changing them keeps its incidents. A joined node's
	// silence check takes its severity from the node's join and reports
	// instead.
	Severity            checksandalerts.AlertSeveritySettings `json:"-" yaml:"-"`
	SeverityFromReports bool                                  `json:"-" yaml:"-"`
	// SendAlertsOnlyToDestinations are the destinations the check names,
	// kept out of its identity like the severity.
	SendAlertsOnlyToDestinations *[]string `json:"-" yaml:"-"`
	// QuietWhileTheseChecksFail are the checks that the check names as its
	// causes, and keepsTheseChecksQuiet the checks that name this one. Both
	// are kept out of its identity like the severity.
	QuietWhileTheseChecksFail, KeepsTheseChecksQuiet []string `json:"-" yaml:"-"`
}

// CheckWithSeverity is the check as the configuration states it: the
// identity's check together with its alert severity settings. Observing a
// check and deciding whether it only records both need the severity.
func (d Definition) CheckWithSeverity() checksandalerts.Check {
	check := d.Check
	check.AlertSeveritySettings, check.SendAlertsOnlyToDestinations = d.Severity, d.SendAlertsOnlyToDestinations
	return check
}

// Identity decides whether a check keeps its state. A fleet check's
// deadline period follows the fleet timings, so it is not part of its
// identity: a changed timing applies from the next receipt.
func (d Definition) Identity() string {
	// Timings change when observations run and alert, not what they observe.
	d.Timing = checksandalerts.Timing{}
	if d.CheckGroup == checksandalerts.NodeReportsCheckGroup {
		d.Heartbeat = nil
	}
	// The description of a module that is not core says whether each finding
	// is an incident of its own, and the state of such a check has another
	// form. So a check whose module changes in this is a new check, although
	// its configuration reads the same.
	if call, notCore := d.Check.Inputs.(*checksandalerts.ModuleCall); notCore && call.CheckModule().OneIncidentForEachFinding {
		return Fingerprint(struct {
			Definition
			OneIncidentForEachFinding bool
		}{d, true})
	}
	return Fingerprint(d)
}

// HubDestination is the joined node's queue for the hub. Its identity never
// changes, so a new join code keeps the queue.
const HubDestination = "hub"

// NodeFleetSettings are the settings that this node sends to its hub.
func (c *Config) NodeFleetSettings() NodeFleetSettings {
	return NodeFleetSettings{AlertSeverityWhenThisNodeGoesSilent: c.AlertSeverityWhenThisNodeGoesSilent, SendSilenceAlertsOfThisNodeOnlyToDestinations: c.SendSilenceAlertsOfThisNodeOnlyToDestinations, RaiseToSosAfterThisNodeIsSilentFor: c.RaiseToSosAfterThisNodeIsSilentFor, ThisNodeIsInNodeGroups: c.ThisNodeIsInNodeGroups, RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent: c.RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent}
}

// listeningAddressProblem gives listening examples instead of a database peer.
func listeningAddressProblem(address string) string {
	if err := checksandalerts.HostPort(address, 0); err != nil {
		return "expected the local host and port where MMWatchdog listens; set listen_on_address: '10.0.0.5:8445', or listen_on_address: '0.0.0.0:8445' for every interface"
	}
	return ""
}
