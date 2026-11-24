package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/daemon"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/store"
)

// hostName reads the machine's host name, which alert titles start with;
// the tests replace it, so that the test output never depends on the machine.
var hostName = os.Hostname

var version = "0.1.0-dev"
var revision = "unknown"

func buildInfo() fleet.Build {
	rev := revision
	dirty := false
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && rev == "unknown" {
				rev = setting.Value
			}
			if setting.Key == "vcs.modified" {
				dirty = setting.Value == "true"
			}
		}
	}
	return fleet.Build{Version: version, Revision: rev, Modified: dirty, Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
}

func main() {
	if e := run(os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, "mmwatchdog:", e)
		os.Exit(errorCode(e))
	}
}

type exitStatus int

func (e exitStatus) Error() string { return "one or more checks need attention" }
func usage() {
	fmt.Println(`MMWatchdog — local node monitoring

  mmwatchdog version [--json]
  mmwatchdog validate [--config-dir DIR] [--official-extra-module-dir DIR] [--external-module-dir DIR] [--config-only] [--json]
  mmwatchdog daemon [--config-dir DIR]
  mmwatchdog status [CHECK_GROUP] [--json]
  mmwatchdog config
  mmwatchdog events [CHECK_GROUP[/CHECK]] [--last DURATION | --since TIME [--until TIME]] [--json]
  mmwatchdog readings CHECK_GROUP [CHECK] [--last DURATION | --since TIME [--until TIME]] [--config-dir DIR] [--json | --chart]
  mmwatchdog check-now CHECK_GROUP [--count-as-scheduled-run] [--json]
  mmwatchdog check-now CHECK_GROUP --without-daemon [--config-dir DIR] [--json]
  mmwatchdog reload [--json]
  mmwatchdog maintenance on CHECK_GROUP [CHECK_GROUP ...]|--every-check-group --for DURATION [--reason TEXT] [--without-maintenance-on-and-off-alerts]
  mmwatchdog maintenance off CHECK_GROUP [CHECK_GROUP ...]|--every-check-group
  mmwatchdog reset-recovery-attempts CHECK_GROUP
  mmwatchdog send-notice --alert-severity info|warn|sos --title TEXT [--body TEXT] [--destination NAME]
  mmwatchdog heartbeat CHECK_GROUP --bearer-token-file FILE [--receiver-url HTTPS_URL [--trusted-certificate-authority-file FILE]]
    exit 0 heartbeat accepted, 1 heartbeat refused (wrong token, or no heartbeat check group of that name), 4 could not reach MMWatchdog or the receiver
  mmwatchdog fleet [NODE [--last DURATION | --since TIME [--until TIME]]] [--json]
  mmwatchdog fleet --node-group NODE_GROUP [--json]
  mmwatchdog fleet readings NODE CHECK_GROUP [CHECK] [--last DURATION | --since TIME [--until TIME]] [--config-dir DIR] [--json | --chart]
  mmwatchdog fleet readings --node-group NODE_GROUP CHECK_GROUP CHECK [--last DURATION | --since TIME [--until TIME]] [--config-dir DIR] [--json | --chart]
  mmwatchdog fleet join-code [--json]
  mmwatchdog fleet rotate-join-code [--json]
  mmwatchdog fleet forget NODE [--json]
  mmwatchdog describe-module [NAME] [--reference-only] [--config-dir DIR] [--official-extra-module-dir DIR] [--external-module-dir DIR] [--json]
  mmwatchdog module-usage [NAME] [--config-dir DIR] [--official-extra-module-dir DIR] [--external-module-dir DIR] [--json]
  mmwatchdog run-starlark-module MODULE_DIR
  mmwatchdog module-json request [--inputs NAME,...]
  mmwatchdog module-json get [--type TYPE] POINTER
  mmwatchdog module-json result --status ok|failed|unknown|delivered (--message TEXT | --message-stdin) [--measurement NAME VALUE]... [--finding ID MESSAGE]... [--exact-finding ID TEXT]...
  mmwatchdog module-json object [--string NAME TEXT]... [--string-stdin NAME] [--json NAME JSON]...

Commands that talk to the running mmwatchdog service accept --socket PATH.
Configuration defaults to /etc/mmwatchdog.
Exit: 0 success, 1 failure, 3 health unknown, 4 command error, 130 interrupted.
Failure takes precedence over health unknown.`)
}

type commandOutcome struct {
	code int
	err  error
}

func (e *commandOutcome) Error() string { return e.err.Error() }
func (e *commandOutcome) Unwrap() error { return e.err }
func errorCode(err error) int {
	if err == nil {
		return 0
	}
	var outcome *commandOutcome
	if errors.As(err, &outcome) {
		return outcome.code
	}
	var status exitStatus
	if errors.As(err, &status) {
		return int(status)
	}
	var helper *checksandalerts.HelperError
	if errors.As(err, &helper) {
		return helper.Code
	}
	return 4
}

func run(args []string) (result error) {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		usage()
		return nil
	}
	// The official extra modules of a release carry the release's version,
	// and MMWatchdog compares it with its own.
	checksandalerts.Version = version
	if args[0] == "module-json" {
		return checksandalerts.ModuleJSONHelper(args[1:], os.Stdin, os.Stdout)
	}
	if args[0] == "readings" || len(args) > 1 && args[0] == "fleet" && args[1] == "readings" {
		onHub := args[0] == "fleet"
		rest := args[1:]
		if onHub {
			rest = args[2:]
		}
		options, err := parseReadingsOptions(rest, onHub, time.Now())
		if errors.Is(err, flag.ErrHelp) {
			usage()
			return nil
		}
		if err != nil {
			return err
		}
		if os.Geteuid() != 0 {
			return fmt.Errorf("readings requires access to root's measurement history; run with sudo")
		}
		return showReadings(options)
	}
	command := args[0]
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	var configDir, officialExtraModuleDir, externalModuleDir, socket, duration, reason, receiverURL, token, trustedCertificateAuthorityFile, nodeGroup string
	var notice store.NoticeRequest
	var configOnly, asJSON, everyGroup, withoutDaemon, countAsScheduledRun, referenceOnly, withoutMaintenanceOnAndOffAlerts bool
	switch command {
	case "version", "validate", "status", "config", "events", "check-now", "fleet", "describe-module", "module-usage", "reload":
		f.BoolVar(&asJSON, "json", false, "JSON output")
	case "daemon", "maintenance", "reset-recovery-attempts", "send-notice", "heartbeat", "run-starlark-module":
	default:
		return fmt.Errorf("unknown command %q; use mmwatchdog --help", command)
	}
	switch command {
	case "validate", "daemon", "check-now", "describe-module", "module-usage":
		f.StringVar(&configDir, "config-dir", "/etc/mmwatchdog", "configuration directory")
	}
	switch command {
	case "validate", "describe-module", "module-usage":
		f.StringVar(&officialExtraModuleDir, "official-extra-module-dir", "", "look for official extra modules in the directory given by --official-extra-module-dir instead of the node setting official_extra_module_dir")
		f.StringVar(&externalModuleDir, "external-module-dir", "", "look for the project's own modules in the directory given by --external-module-dir instead of the node setting external_module_dir")
	}
	switch command {
	case "status", "config", "events", "check-now", "fleet", "reload", "maintenance", "reset-recovery-attempts", "send-notice":
		f.StringVar(&socket, "socket", "/run/mmwatchdog/mmwatchdog.sock", "Unix socket of the node monitoring process")
	}
	var eventLast, eventSince, eventUntil string
	if command == "fleet" || command == "events" {
		f.StringVar(&eventLast, "last", "", "show events from the past duration given by --last, such as 30s, 5m, 26h, or 7d; the duration must be positive")
		f.StringVar(&eventSince, "since", "", "show events from the RFC 3339 time given by --since")
		f.StringVar(&eventUntil, "until", "", "show events through the RFC 3339 time given by --until; requires --since")
	}
	if command == "fleet" {
		f.StringVar(&nodeGroup, "node-group", "", "show only nodes in the node group named by --node-group")
	}
	if command == "describe-module" {
		f.BoolVar(&referenceOnly, "reference-only", false, "print only the module's inputs, measurements, and examples, without its page")
	}
	if command == "validate" {
		f.BoolVar(&configOnly, "config-only", false, "check only configuration files; skip checks of referenced files and accounts, so validation can run on a machine other than the monitored node")
	}
	if command == "send-notice" {
		f.StringVar(&notice.AlertSeverity, "alert-severity", "", "info, warn, or sos: send the notice to destinations whose alert_severities include the severity given by --alert-severity")
		f.StringVar(&notice.Title, "title", "", "the one line that says what happened; MMWatchdog puts the node's name in front")
		f.StringVar(&notice.Body, "body", "", "more text, for destinations that send the body")
		f.StringVar(&notice.Destination, "destination", "", "send the notice only to the destination named by --destination, regardless of the destination's alert_severities")
	}
	if command == "check-now" {
		f.BoolVar(&withoutDaemon, "without-daemon", false, "run the check group's checks locally using the configuration directory, without the node monitoring process")
		f.BoolVar(&countAsScheduledRun, "count-as-scheduled-run", false, "run the check group's scheduled checks now through the node monitoring process; record results, send alerts, and attempt recovery actions as for scheduled runs")
	}
	action := ""
	rest := args[1:]
	if command == "maintenance" {
		if len(rest) > 0 && (rest[0] == "-h" || rest[0] == "--help" || rest[0] == "-help") {
			// Help lists the flags of "maintenance on", the longer form.
			f.BoolVar(&everyGroup, "every-check-group", false, "maintenance for every check group on this node")
			f.StringVar(&duration, "for", "", "how long the maintenance lasts, such as 1h or 30d")
			f.StringVar(&reason, "reason", "", "why maintenance is needed")
			f.BoolVar(&withoutMaintenanceOnAndOffAlerts, "without-maintenance-on-and-off-alerts", false, "record maintenance start and end without sending alerts about maintenance start or end")
			_ = f.Parse(rest[:1]) // prints the flags and returns flag.ErrHelp
			return nil
		}
		if len(rest) == 0 || (rest[0] != "on" && rest[0] != "off") {
			return fmt.Errorf("maintenance requires on or off")
		}
		action, rest = rest[0], rest[1:]
		f.BoolVar(&everyGroup, "every-check-group", false, "maintenance for every check group on this node")
		if action == "on" {
			f.StringVar(&duration, "for", "", "how long the maintenance lasts, such as 1h or 30d")
			f.StringVar(&reason, "reason", "", "why maintenance is needed")
			f.BoolVar(&withoutMaintenanceOnAndOffAlerts, "without-maintenance-on-and-off-alerts", false, "record maintenance start and end without sending alerts about maintenance start or end")
		}
	}
	if command == "heartbeat" {
		f.StringVar(&receiverURL, "receiver-url", "", "HTTPS URL of a heartbeat receiver on another machine; without it, the heartbeat goes through the heartbeat socket of this node")
		f.StringVar(&token, "bearer-token-file", "", "heartbeat bearer token file")
		f.StringVar(&trustedCertificateAuthorityFile, "trusted-certificate-authority-file", "", "PEM certificates to trust instead of the system certificate authorities")
	}
	name, node := "", ""
	// A maintenance command names one or more check groups.
	checkGroups := []string{}
	for command == "maintenance" && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		checkGroups, rest = append(checkGroups, rest[0]), rest[1:]
	}
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		name, rest = rest[0], rest[1:]
	}
	if command == "fleet" && name == "forget" && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		node, rest = rest[0], rest[1:]
	}
	if e := f.Parse(rest); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return nil
		}
		return e
	}
	if command == "maintenance" {
		checkGroups = append(checkGroups, f.Args()...)
	} else if f.NArg() > 0 {
		if name != "" || f.NArg() != 1 {
			return fmt.Errorf("unexpected arguments")
		}
		name = f.Arg(0)
	}
	switch command {
	case "version", "validate", "daemon", "config", "reload", "send-notice":
		if name != "" {
			return fmt.Errorf("%s accepts no name after the command", command)
		}
	}
	if command == "check-now" && visited(f, "config-dir") && !withoutDaemon {
		return fmt.Errorf("--config-dir requires --without-daemon for check-now")
	}
	if countAsScheduledRun && withoutDaemon {
		return fmt.Errorf("--count-as-scheduled-run needs the node monitoring process, so it cannot be combined with --without-daemon")
	}
	// The two module directories of the command line take the place of the
	// node's own. A path can be relative to the directory of the command.
	moduleDirs := checksandalerts.ModuleDirs{}
	for option, directory := range map[string]*string{"official-extra-module-dir": &officialExtraModuleDir, "external-module-dir": &externalModuleDir} {
		if !visited(f, option) {
			continue
		}
		if *directory == "" {
			return fmt.Errorf("--%s requires a directory", option)
		}
		absolute, e := filepath.Abs(*directory)
		if e != nil {
			return fmt.Errorf("--%s: %w", option, e)
		}
		*directory = absolute
	}
	moduleDirs.OfficialExtra, moduleDirs.External = officialExtraModuleDir, externalModuleDir
	interruptContext, stopInterrupt := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopInterrupt()
	ctx, cancel := signal.NotifyContext(interruptContext, syscall.SIGTERM)
	defer cancel()
	defer func() {
		if interruptContext.Err() != nil {
			result = &commandOutcome{130, interruptContext.Err()}
		}
	}()
	eventQuery := ""
	if command == "fleet" || command == "events" {
		since, until, err := parseTimeOptions(f, eventLast, eventSince, eventUntil, time.Now())
		if err != nil {
			return err
		}
		if visited(f, "last") || visited(f, "since") || visited(f, "until") {
			if command == "fleet" && (name == "" || name == "forget" || name == "join-code" || name == "rotate-join-code" || nodeGroup != "") {
				return fmt.Errorf("event time options require mmwatchdog fleet NODE; name the node whose events you want")
			}
			eventQuery = neturl.Values{"since": {since.Format(time.RFC3339Nano)}, "until": {until.Format(time.RFC3339Nano)}}.Encode()
		}
	}
	switch command {
	case "version":
		info := buildInfo()
		if asJSON {
			return printJSON(info)
		}
		fmt.Printf("mmwatchdog %s (%s, %s/%s, modified=%v)\n", info.Version, info.Revision, info.OS, info.Arch, info.Modified)
		return nil
	case "run-starlark-module":
		if name == "" {
			return fmt.Errorf("run-starlark-module requires the directory of a module, which holds the script module.star")
		}
		return checksandalerts.RunStarlarkModule(ctx, name, os.Stdin, os.Stdout)
	case "describe-module":
		return describeModule(configDir, moduleDirs, name, asJSON, referenceOnly)
	case "module-usage":
		return moduleUsage(configDir, moduleDirs, name, asJSON)
	case "validate":
		// A deploy validates against the module directories that it has
		// staged and installs only afterwards.
		c, e := config.LoadWithModuleDirs(configDir, moduleDirs)
		if e != nil {
			return &commandOutcome{1, e}
		}
		if !configOnly {
			if e = daemon.ValidateRuntime(c); e != nil {
				return &commandOutcome{1, e}
			}
		}
		// A title names this node, so its length is known only here.
		node, _ := hostName()
		if !configOnly {
			if e = daemon.ValidateRouting(c); e != nil {
				return &commandOutcome{1, e}
			}
		}
		warnings := daemon.ValidationWarnings(c, node, !configOnly)
		if asJSON {
			return printJSON(struct {
				Valid  bool           `json:"valid"`
				Config *config.Config `json:"config"`
				config.ValidationWarnings
			}{true, c, warnings})
		}
		if c.JoinFleetHub != nil {
			fmt.Printf("Valid configuration: %s; alert destinations come from the hub\n", countOf(len(c.CheckGroups), "check group"))
		} else {
			fmt.Printf("Valid configuration: %s, %s\n", countOf(len(c.CheckGroups), "check group"), countOf(len(c.Alerts.Destinations), "alert destination"))
			if len(c.Alerts.Destinations) == 0 {
				fmt.Println("This node sends alerts to nobody.")
			}
		}
		fmt.Print(validationWarningsText(node, warnings))
		return nil
	case "daemon":
		if os.Geteuid() != 0 {
			return fmt.Errorf("daemon requires root; run with sudo")
		}
		// MMWatchdog watches only servers that systemd runs. sd_booted's rule:
		// systemd made /run/systemd/system on every machine that it started.
		if about, e := os.Stat("/run/systemd/system"); e != nil || !about.IsDir() {
			return fmt.Errorf("this node does not run systemd: the directory /run/systemd/system, which exists on every machine that systemd started, is not there. MMWatchdog watches only Linux servers that run systemd: its daemon runs as a systemd service, and its checks of systemd units, its restarts, and its checks of the journal rest on systemd")
		}
		// A time from time.Now carries a monotonic reading, on which the
		// daemon times its waits, and is in time.Local. UTC, In, and Local
		// drop that reading, so the daemon makes UTC its local time: every
		// time that it records or shows is in UTC anyway.
		time.Local = time.UTC
		d, e := daemon.NewDaemon(configDir)
		if e != nil {
			return e
		}
		defer d.Close()
		d.Build = buildInfo()
		return d.Run(ctx)
	case "heartbeat":
		return sendHeartbeat(ctx, name, receiverURL, token, trustedCertificateAuthorityFile)
	case "send-notice":
		return sendNotice(ctx, socket, notice)
	case "fleet":
		if visited(f, "node-group") {
			if name != "" {
				return fmt.Errorf("mmwatchdog fleet --node-group shows the nodes of one node group and takes nothing else: remove %s or --node-group", name)
			}
			if nodeGroup == "" {
				return fmt.Errorf("--node-group requires the name of a node group")
			}
		}
		if name == "forget" {
			if node == "" {
				return fmt.Errorf("fleet forget requires a node name")
			}
			if _, e := request(ctx, socket, "POST", "/fleet/forget/"+neturl.PathEscape(node), nil); e != nil {
				return e
			}
			if asJSON {
				return printJSON(map[string]string{"removed_node": node})
			}
			fmt.Printf("Removed node %s from the fleet; if MMWatchdog still runs on the removed node, the node joins again with the current join code.\n", node)
			return nil
		}
		if name == "join-code" || name == "rotate-join-code" {
			method, path := "GET", "/fleet/join-code"
			if name == "rotate-join-code" {
				method, path = "POST", "/fleet/rotate-join-code"
			}
			b, e := request(ctx, socket, method, path, nil)
			if e != nil {
				return e
			}
			var code struct {
				JoinCode string `json:"join_code"`
			}
			if e = json.Unmarshal(b, &code); e != nil {
				return e
			}
			if asJSON {
				return printJSON(code)
			}
			fmt.Println(code.JoinCode)
			return nil
		}
		path := "/fleet"
		if name != "" {
			path += "?node=" + neturl.QueryEscape(name)
		}
		if nodeGroup != "" {
			path += "?node_group=" + neturl.QueryEscape(nodeGroup)
		}
		if eventQuery != "" {
			path += "&" + eventQuery
		}
		b, e := request(ctx, socket, "GET", path, nil)
		if e != nil {
			return e
		}
		if name != "" {
			return printNode(b, asJSON, time.Now())
		}
		return printFleet(b, asJSON, time.Now())
	case "check-now":
		if name == "" {
			return fmt.Errorf("check-now requires a check group name")
		}
		var observations map[string]checksandalerts.Observation
		if withoutDaemon {
			c, e := config.Load(configDir)
			if e != nil {
				return e
			}
			observations, e = daemon.CheckNow(ctx, c, name)
			if e != nil {
				return e
			}
		} else {
			path := "/check-now/" + neturl.PathEscape(name)
			if countAsScheduledRun {
				path += "?count_as_scheduled_run=true"
			}
			b, e := request(ctx, socket, "POST", path, nil)
			if e != nil {
				return e
			}
			if e = json.Unmarshal(b, &observations); e != nil {
				return e
			}
		}
		if asJSON {
			if e := printJSON(observations); e != nil {
				return e
			}
		} else {
			names := checksandalerts.Names(observations)
			fullNames := make([]string, len(names))
			for i, n := range names {
				fullNames[i] = name + "/" + n
			}
			width := nameColumnWidth(fullNames, 24)
			for _, n := range names {
				o := observations[n]
				fmt.Printf("%-*s %-8s %s\n", width, name+"/"+n, o.Status, o.Message)
				printObservation(o)
			}
			if countAsScheduledRun {
				fmt.Printf("The runs counted as scheduled runs for check group %s; run mmwatchdog status %s to see the resulting check status.\n", name, name)
			}
		}
		code := 0
		for _, o := range observations {
			if o.Status == "failed" {
				code = incidents.HealthExitCode(code, 1)
			}
			if o.Status == "unknown" {
				code = incidents.HealthExitCode(code, 3)
			}
		}
		if code != 0 {
			return exitStatus(code)
		}
		return nil
	default:
		method, path := "GET", "/"+command
		var body []byte
		if command == "reload" || command == "maintenance" || command == "reset-recovery-attempts" {
			method = "POST"
		}
		if command == "maintenance" {
			if everyGroup {
				if len(checkGroups) > 0 {
					return fmt.Errorf("maintenance takes check group names or --every-check-group, not both")
				}
				checkGroups = []string{checksandalerts.EveryCheckGroup}
			}
			if len(checkGroups) == 0 {
				return fmt.Errorf("maintenance requires one or more check group names, or --every-check-group")
			}
			for _, group := range checkGroups {
				if strings.Contains(group, ",") {
					return fmt.Errorf("%q is not a check group name; separate check group names with spaces", group)
				}
			}
			if action == "on" {
				if d, e := checksandalerts.ParseDuration(duration); e != nil || d <= 0 {
					return fmt.Errorf("maintenance on requires --for with a positive duration, such as 1h or 30d")
				}
			}
			path += "/" + action + "/" + neturl.PathEscape(strings.Join(checkGroups, ","))
			body, _ = json.Marshal(map[string]any{"for": duration, "reason": reason, "user": invokingUser(), "without_maintenance_on_and_off_alerts": withoutMaintenanceOnAndOffAlerts})
		}
		if command == "reset-recovery-attempts" {
			if name == "" {
				return fmt.Errorf("%s requires a check group name", command)
			}
			path += "/" + neturl.PathEscape(name)
		}
		// events also takes CHECK_GROUP/CHECK: the node returns the check
		// group's events, and the command keeps the events of that check.
		eventsOfCheck := ""
		if command == "events" {
			if group, check, found := strings.Cut(name, "/"); found {
				name, eventsOfCheck = group, check
			}
		}
		if (command == "events" || command == "status") && name != "" {
			path += "?check_group=" + neturl.QueryEscape(name)
		}
		if command == "events" && eventQuery != "" {
			separator := "?"
			if strings.Contains(path, "?") {
				separator = "&"
			}
			path += separator + eventQuery
		}
		b, e := request(ctx, socket, method, path, body)
		if e != nil {
			return e
		}
		if command == "status" {
			return printStatus(b, name, asJSON)
		}
		if command == "reload" {
			var result daemon.ReloadResult
			if e = json.Unmarshal(b, &result); e != nil {
				return e
			}
			// The daemon runs on this node, so its warnings are about this
			// node's name.
			node, _ := hostName()
			if asJSON {
				if e := printJSON(result); e != nil {
					return e
				}
			} else {
				fmt.Print(reloadResultText(node, result))
			}
			for _, verification := range result.DestinationVerifications {
				if verification.CanVerifyWithoutMessage && !verification.AcceptsAddressAndCredential {
					return &commandOutcome{1, fmt.Errorf("destination verification failed; the new configuration is active")}
				}
			}
			return nil
		}
		if command == "maintenance" {
			var reply struct {
				Maintenance               *checksandalerts.Window `json:"maintenance"`
				FleetHubAcceptedTheReport bool                    `json:"fleet_hub_accepted_the_report"`
				FleetHubReportError       string                  `json:"fleet_hub_report_error"`
			}
			if e = json.Unmarshal(b, &reply); e != nil {
				return e
			}
			switch {
			// Ending the whole-node window leaves a check group's own window in place.
			case action == "off" && everyGroup:
				fmt.Println("The maintenance for every check group on this node is off. Alerts, restarts and recovery commands resume after fresh checks. A check group that has its own maintenance window stays in maintenance until that window ends.")
			case action == "off":
				fmt.Printf("%s out of maintenance. Alerts, restarts and recovery commands resume after fresh checks.\n", maintenanceSubject(checkGroups, everyGroup))
			case reply.Maintenance == nil:
				return fmt.Errorf("daemon returned no maintenance window")
			default:
				fmt.Print(maintenanceOnText(checkGroups, everyGroup, reply.Maintenance.Until, duration))
				if reply.Maintenance.WithoutMaintenanceOnAndOffAlerts {
					fmt.Println("  Maintenance start and end are not announced.")
				}
			}
			return maintenanceReportedToTheHub(action, everyGroup, reply.FleetHubAcceptedTheReport, reply.FleetHubReportError)
		}
		if command == "reset-recovery-attempts" {
			fmt.Printf("The recovery attempts of check group %s are reset. MMWatchdog can attempt recovery actions again.\n", name)
			return nil
		}
		if command == "events" && eventsOfCheck != "" {
			var events []checksandalerts.Event
			if e = json.Unmarshal(b, &events); e != nil {
				return e
			}
			kept := []checksandalerts.Event{}
			for _, ev := range events {
				if ev.Check == eventsOfCheck {
					kept = append(kept, ev)
				}
			}
			if b, e = json.Marshal(kept); e != nil {
				return e
			}
		}
		if asJSON || command == "config" {
			var v any
			if e = json.Unmarshal(b, &v); e != nil {
				return e
			}
			return printJSON(v)
		}
		if command == "events" {
			var events []checksandalerts.Event
			if e = json.Unmarshal(b, &events); e != nil {
				return e
			}
			if len(events) == 0 {
				fmt.Println(noEventsText(name, eventsOfCheck))
			}
			for _, ev := range events {
				fmt.Printf("%s %-20s %s %s\n", ev.At.Format(time.RFC3339), ev.Type, subjectOfEvent(ev), ev.Message)
				if name != "" {
					if ev.Incident != "" {
						fmt.Println("  Incident:", ev.Incident)
					}
					if ev.Type == "recovery_command_result" && ev.Evidence != nil {
						printObservation(ev.Evidence.Observation)
						if ev.Evidence.Truncated {
							fmt.Println("  Output truncated to the retained size limit")
						}
					} else {
						printEvidence(ev.Evidence)
					}
				}
			}
			return nil
		}
		fmt.Println(strings.TrimSpace(string(b)))
		return nil
	}
}

// validationWarningsText is what mmwatchdog validate and mmwatchdog reload print for the
// validation warnings of a configuration on the node named node, or nothing
// when there is none: the alert titles that MMWatchdog will shorten, the systemd
// units that have their own systemd_unit_state check and are missing from
// the systemd_units_to_ignore of an every_systemd_unit check, the
// filesystems that have their own node_filesystem check with a limit and
// are missing from the filesystems_to_ignore of an every_disk_and_network_filesystem
// check, the checks that name a check with a lower alert_severity in
// their no_alerts_or_recovery_actions_while_these_checks_fail, and the
// files of a credential that other accounts of this machine can use.
func validationWarningsText(node string, warnings config.ValidationWarnings) string {
	text := ""
	for _, warning := range warnings.MMHotlineLoopbackWarnings {
		text += warning + "\n"
	}
	for _, warning := range warnings.DestinationRunSettingsThatJoinedNodesIgnore {
		text += warning + "\n"
	}
	for _, warning := range warnings.MMHotlineWithoutAnotherDestination {
		text += warning + "\n"
	}
	for _, check := range warnings.ChecksWithoutAlertDestination {
		text += fmt.Sprintf("%s/%s: no destination receives alerts with alert_severity %s; add the severity to a destination's alert_severities, or change the check's send_alerts_only_to_destinations.\n", check.CheckGroup, check.Check, check.AlertSeverity)
	}
	for _, check := range warnings.ChecksWithUnknownAlertDestinations {
		text += fmt.Sprintf("%s/%s: the saved fleet settings have no destination named %s; change send_alerts_only_to_destinations or add the destination on the hub. Destinations listed in send_alerts_only_to_destinations that the hub knows still receive alerts. If the hub knows none of the listed destinations, the check's alerts go to every destination whose alert_severities include the alert's severity.\n", check.CheckGroup, check.Check, strings.Join(check.UnknownDestinationNames, ", "))
	}
	if shortened := warnings.AlertTitlesThatWillBeShortened; len(shortened) > 0 {
		text += fmt.Sprintf("An alert title has at most 120 characters. For node %s, MMWatchdog will shorten the alert titles listed below; shorten the check group name or check name to keep the full title:\n", node)
		for _, title := range shortened {
			text += fmt.Sprintf("  %s/%s: the check's longest alert title has %d characters and will be sent as: %s\n", title.CheckGroup, title.Check, title.Characters, title.Title)
		}
	}
	if missing := warnings.SystemdUnitsMissingFromSystemdUnitsToIgnore; len(missing) > 0 {
		text += "An every_systemd_unit check alerts for every failed systemd unit outside the check's systemd_units_to_ignore list. The units listed below also have a systemd_unit_state check, so a unit failure would send two alerts. Add each listed unit to systemd_units_to_ignore:\n"
		for _, unit := range missing {
			text += fmt.Sprintf("  %s/%s: %s is missing from systemd_units_to_ignore; %s also checks the systemd unit\n", unit.CheckGroup, unit.Check, unit.SystemdUnit, unit.SystemdUnitStateCheck)
		}
	}
	if missing := warnings.FilesystemsMissingFromFilesystemsToIgnore; len(missing) > 0 {
		text += "An every_disk_and_network_filesystem check alerts for every filesystem beyond the check's limits and outside the check's filesystems_to_ignore list. The filesystems listed below also have a node_filesystem check with a limit, so a full filesystem would send two alerts. Add each listed filesystem to filesystems_to_ignore:\n"
		for _, filesystem := range missing {
			text += fmt.Sprintf("  %s/%s: %s is missing from filesystems_to_ignore; %s also checks the filesystem\n", filesystem.CheckGroup, filesystem.Check, filesystem.Filesystem, filesystem.NodeFilesystemCheck)
		}
	}
	if lower := warnings.ChecksThatNameACheckWithALowerAlertSeverity; len(lower) > 0 {
		text += "While a check named in another check's no_alerts_or_recovery_actions_while_these_checks_fail fails, MMWatchdog sends one alert, for the named check and with the named check's alert_severity. Each check listed below names a check that has a lower alert_severity than the listed check. So while the named check fails, a destination that takes only the listed check's higher alert_severity gets no alert. Give the named check the alert_severity that the named check's failure deserves:\n"
		for _, check := range lower {
			text += fmt.Sprintf("  %s/%s has alert_severity %s and names %s, which has alert_severity %s\n", check.CheckGroup, check.Check, check.AlertSeverity, check.NamedCheck, check.NamedCheckAlertSeverity)
		}
	}

	return text
}

// reloadResultText prints the verification and the optional message test
// command for each new or changed destination. No test message is sent.
func reloadResultText(node string, result daemon.ReloadResult) string {
	text := "The configuration is active.\n" + validationWarningsText(node, result.ValidationWarnings)
	for _, verification := range result.DestinationVerifications {
		outcome := verification.VerificationMessage
		if !verification.CanVerifyWithoutMessage {
			outcome = "not tested: " + outcome
		} else if !verification.AcceptsAddressAndCredential {
			outcome = "failed: " + outcome
		}
		text += fmt.Sprintf("Destination %s: %s\n", verification.Destination, outcome)
	}
	for _, name := range result.NewOrChangedDestinations {
		text += fmt.Sprintf("Destination %s is new or changed. To send a test notice, run:\n", name)
		text += fmt.Sprintf("sudo mmwatchdog send-notice --alert-severity info --title %q --destination %s\n", "Test of the destination "+name+". Nothing is wrong.", name)
	}
	return text
}

// subjectOfEvent names what an event is about in an event line: the check
// group and check, with the finding after the check, such as
// "services/no_systemd_unit_failed: backup.service"; only the check group for
// an event about a whole check group, such as a maintenance window; and
// nothing for an event about the node, such as rebooted.
func subjectOfEvent(ev checksandalerts.Event) string {
	if ev.Check == "" {
		return ev.CheckGroup
	}
	subject := ev.CheckGroup + "/" + ev.Check
	if ev.Finding != "" {
		subject += ": " + ev.Finding
	}
	return subject
}

// sendNotice asks the daemon to send a notice and prints, for each
// destination, the result of the first delivery attempt. The notice is
// queued like every alert, so MMWatchdog keeps trying after a failed attempt; the
// command then ends with exit code 1.
func sendNotice(ctx context.Context, socket string, notice store.NoticeRequest) error {
	// The daemon checks the notice; these two need no daemon to refuse.
	if notice.AlertSeverity == "" || notice.Title == "" {
		return fmt.Errorf("send-notice requires --alert-severity info|warn|sos and --title TEXT")
	}
	body, _ := json.Marshal(notice)
	b, e := request(ctx, socket, "POST", "/send-notice", body)
	if e != nil {
		return e
	}
	var result store.NoticeResult
	if e = json.Unmarshal(b, &result); e != nil {
		return e
	}
	text, failure := noticeText(notice, result)
	fmt.Print(text)
	if failure != "" {
		return &commandOutcome{1, errors.New(failure)}
	}
	return nil
}

// noticeText is what mmwatchdog send-notice prints: where the notice went, and one
// line for each destination with the result of the first delivery attempt.
// failure is empty, or says in one sentence what did not work and what MMWatchdog
// does about it.
func noticeText(notice store.NoticeRequest, result store.NoticeResult) (text, failure string) {
	to := "every destination that takes " + notice.AlertSeverity
	if notice.Destination != "" {
		to = "the destination " + notice.Destination
	}
	hub := result.SentThroughFleetHubAt != ""
	switch {
	case hub:
		text = fmt.Sprintf("Notice sent to the MMWatchdog fleet hub at %s, which delivers it to %s:\n", result.SentThroughFleetHubAt, to)
	case len(result.Deliveries) == 0:
		return fmt.Sprintf("No destination takes %s, so MMWatchdog sent the notice to nobody. The notice is recorded in mmwatchdog events.\n", notice.AlertSeverity), ""
	default:
		text = fmt.Sprintf("Notice sent to %s:\n", to)
	}
	// retried counts the destinations that MMWatchdog keeps trying.
	width, failed, retried := 0, 0, 0
	name := func(d store.NoticeDelivery) string {
		if hub {
			return "MMWatchdog fleet hub"
		}
		return d.Destination
	}
	for _, d := range result.Deliveries {
		width = max(width, len(name(d)))
	}
	for _, d := range result.Deliveries {
		outcome := "delivered"
		if hub {
			outcome = "accepted"
		}
		if d.Result != store.NoticeDelivered {
			failed++
		}
		if d.Result == store.NoticeFailed || d.Result == store.NoticeWaitsBehindEarlierAlerts {
			retried++
		}
		switch d.Result {
		case store.NoticeFailed:
			outcome = "failed: " + d.Error
			if hub {
				outcome = "did not accept the notice: " + d.Error
			}
		case store.NoticeWaitsBehindEarlierAlerts:
			earlier := "1 earlier alert is"
			if d.EarlierUndeliveredAlerts != 1 {
				earlier = fmt.Sprintf("%d earlier alerts are", d.EarlierUndeliveredAlerts)
			}
			outcome = fmt.Sprintf("not attempted yet: %s not delivered, and the latest attempt failed: %s", earlier, d.Error)
		case store.NoticeNotSent:
			outcome = "not sent: the notice left the queue of undelivered alerts before an attempt, because the destination was changed or removed, or the queue was full"
			if d.Error != "" {
				outcome = "not sent: " + d.Error
			}
		}
		text += fmt.Sprintf("  %-*s   %s\n", width, name(d), outcome)
	}
	switch {
	case failed == 0 && hub:
		text += "To see the result for each destination, send the notice from the fleet hub: run the same mmwatchdog send-notice command there.\n"
	case failed == 0:
	case hub && retried > 0 && result.SendAlertWithoutHubAfterWaitingFor != "":
		failure = fmt.Sprintf("the MMWatchdog fleet hub did not accept the notice on the first attempt. MMWatchdog keeps the notice queued and tries again; if the hub has not accepted the notice after %s (send_alert_without_hub_after_waiting_for), the local node sends the notice directly to the alert destinations", result.SendAlertWithoutHubAfterWaitingFor)
	case hub && retried > 0:
		failure = "the MMWatchdog fleet hub did not accept the notice on the first attempt. MMWatchdog keeps the notice queued and tries again"
	case hub:
		failure = "the notice was not sent to the MMWatchdog fleet hub"
	default:
		failure = fmt.Sprintf("the notice was not delivered at the first attempt to %d of %d destinations", failed, len(result.Deliveries))
		if retried > 0 {
			failure += ". MMWatchdog keeps the notice queued and tries again"
		}
	}
	return text, failure
}

// maintenanceSubject starts the sentence that says which check groups a
// maintenance command covers: "myapp is", "worker_1, worker_2 are", or
// "Every check group on this node is".
// maintenanceReportedToTheHub says, on a joined node, whether the MMWatchdog
// fleet hub got the change of the window, and fails when it did not: a
// playbook that reboots the node after "maintenance on" must know that the
// hub may alert that the node stopped reporting.
func maintenanceReportedToTheHub(action string, everyGroup, accepted bool, reportError string) error {
	switch {
	case reportError != "" && action == "on":
		return &commandOutcome{1, fmt.Errorf("maintenance is active on the local node, but the MMWatchdog fleet hub did not receive the node's report; the hub does not know about the maintenance window and may alert that the node stopped reporting: %s", reportError)}
	case reportError != "":
		return &commandOutcome{1, fmt.Errorf("maintenance has ended on the local node, but the MMWatchdog fleet hub did not receive the node's report; the hub learns that maintenance ended from the node's next report: %s", reportError)}
	case accepted && action == "on" && everyGroup:
		fmt.Println("  The MMWatchdog fleet hub has the window, so it sends no alert that this node stopped reporting until the window ends.")
	case accepted:
		fmt.Println("  The MMWatchdog fleet hub received the maintenance change.")
	}
	return nil
}

func maintenanceSubject(checkGroups []string, everyGroup bool) string {
	switch {
	case everyGroup:
		return "Every check group on this node is"
	case len(checkGroups) == 1:
		return checkGroups[0] + " is"
	}
	return strings.Join(checkGroups, ", ") + " are"
}

// maintenanceOnText is what mmwatchdog maintenance on prints: until when the
// maintenance lasts, what MMWatchdog keeps doing and what it stops, and the command
// that ends it early.
func maintenanceOnText(checkGroups []string, everyGroup bool, until time.Time, duration string) string {
	of, them, off := "the check groups in maintenance", "the check groups in maintenance", strings.Join(checkGroups, " ")
	switch {
	case everyGroup:
		of, off = "every check group", "--every-check-group"
	case len(checkGroups) == 1:
		of, them = checkGroups[0], checkGroups[0]
	}
	return fmt.Sprintf("%s in maintenance until %s (%s).\n", maintenanceSubject(checkGroups, everyGroup), until.UTC().Format("2006-01-02 15:04 UTC"), duration) +
		fmt.Sprintf("  MMWatchdog keeps running the checks of %s and shows the results in mmwatchdog status.\n", of) +
		fmt.Sprintf("  During maintenance, MMWatchdog sends no alert and attempts no systemd unit restart or recovery command for %s.\n", them) +
		fmt.Sprintf("  To end the maintenance early: sudo mmwatchdog maintenance off %s\n", off)
}

// visited reports that a flag was set on the command line.
func visited(f *flag.FlagSet, name string) bool {
	set := false
	f.Visit(func(fl *flag.Flag) {
		if fl.Name == name {
			set = true
		}
	})
	return set
}

// invokingUser names the person behind a command, for maintenance alerts: the
// sudo user, or else the Linux login user, which survives "sudo su -".
func invokingUser() string {
	if name := os.Getenv("SUDO_USER"); name != "" && name != "root" {
		return name
	}
	if name := loginUser("/proc/self/loginuid"); name != "" {
		return name
	}
	if u, e := user.Current(); e == nil {
		return u.Username
	}
	return ""
}

// loginUser reads a Linux loginuid file. It is unset (4294967295) for
// processes outside a login session, and absent on other systems.
func loginUser(path string) string {
	b, e := os.ReadFile(path)
	id := strings.TrimSpace(string(b))
	if e != nil || id == "" || id == "4294967295" {
		return ""
	}
	if u, e := user.LookupId(id); e == nil {
		return u.Username
	}
	return ""
}
func printJSON(v any) error {
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	return e.Encode(v)
}
func request(ctx context.Context, socket, method, path string, body []byte) ([]byte, error) {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer tr.CloseIdleConnections()
	cl := &http.Client{Transport: tr, Timeout: 5 * time.Minute}
	if strings.HasPrefix(path, "/check-now/") || path == "/send-notice" || path == "/reload" {
		// The daemon bounds each check's execution and each delivery
		// attempt; cancellation still uses ctx.
		cl.Timeout = 0
	}
	req, e := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bytes.NewReader(body))
	if e != nil {
		return nil, e
	}
	resp, e := cl.Do(req)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024+1))
	if e != nil {
		return nil, e
	}
	if len(b) > 16*1024*1024 {
		if req.URL.Path == "/events" || req.URL.Path == "/fleet" && req.URL.Query().Get("node") != "" {
			return nil, fmt.Errorf("the events in the requested time span exceed 16 MiB; request a shorter time span with --since and --until or --last")
		}
		return nil, fmt.Errorf("the node monitoring process returned a response larger than 16 MiB")
	}
	if resp.StatusCode != 200 {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &failure) == nil && failure.Error != "" {
			return nil, errors.New(failure.Error)
		}
		return nil, fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	return b, nil
}

// printAlertSeverity shows a check's alert_severity,
// raise_to_sos_after_failing_for, the destinations and the checks that it
// names, and the severity of its open incidents. The check that watches a
// node for silence has the node's silence severity, the node's
// raise_to_sos_after_this_node_is_silent_for, and its
// raise_to_sos_when_every_node_of_these_node_groups_is_silent.
func printAlertSeverity(st incidents.CheckStatus) {
	p := st.AlertSeveritySettings
	if p.AlertSeverity != "" {
		fmt.Printf("  Alert severity: %s\n", p.AlertSeverity)
	}
	if p.RaiseToSosAfterFailingFor != "" && st.CheckGroup == "node_reports" {
		fmt.Printf("  Raise to SOS after no report for: %s\n", p.RaiseToSosAfterFailingFor)
	} else if p.RaiseToSosAfterFailingFor != "" {
		fmt.Printf("  Raise to SOS after failing for: %s\n", p.RaiseToSosAfterFailingFor)
	}
	if nodeGroups := st.RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent; len(nodeGroups) > 0 {
		fmt.Printf("  Raise to SOS when every node of these node groups is silent: %s\n", strings.Join(nodeGroups, ", "))
	}
	if named := st.SendAlertsOnlyToDestinations; named != nil && len(*named) == 0 {
		fmt.Println("  Sends alerts to no destination (send_alerts_only_to_destinations: [])")
	} else if named != nil {
		fmt.Printf("  Sends alerts only to: %s\n", strings.Join(*named, ", "))
	}
	if causes := st.QuietWhileTheseChecksFail; len(causes) > 0 {
		fmt.Printf("  No alerts or recovery actions while these checks fail: %s\n", strings.Join(causes, ", "))
	}
	if quiet := st.KeepsTheseChecksQuiet; len(quiet) > 0 {
		fmt.Printf("  While this check fails, no alerts or recovery actions for: %s\n", strings.Join(quiet, ", "))
	}
	if t := st.FailureIncident; t.Incident != "" {
		fmt.Printf("  Alert severity of the open failure incident: %s\n", t.AlertSeverity)
	}
	if t := st.CannotCheckIncident; t.Incident != "" {
		fmt.Printf("  Alert severity of the open cannot-check incident: %s\n", t.AlertSeverity)
	}
}

func printStatus(b []byte, name string, asJSON bool) error {
	var status struct {
		Summary                    incidents.StatusSummary                 `json:"summary"`
		Checks                     map[string]incidents.CheckStatus        `json:"checks"`
		SystemdUnitRestartAttempts map[string]daemon.RecoveryAttemptStatus `json:"systemd_unit_restart_attempts"`
		RecoveryCommandAttempts    map[string]daemon.RecoveryAttemptStatus `json:"recovery_command_attempts"`
		Maintenance                map[string]checksandalerts.Window       `json:"maintenance"`
		Delivery                   store.DeliverySummary                   `json:"delivery"`
		StorageError               string                                  `json:"storage_error"`
		Hub                        *fleet.HubLink                          `json:"joined_fleet_hub"`
		SendsAlertsToNobody        bool                                    `json:"sends_alerts_to_nobody"`
		MeasurementHistory         store.MeasurementHistoryStatus          `json:"measurement_history"`
		FleetHub                   string                                  `json:"fleet_hub_role_address,omitempty"`
		MMHotlineDestinations      []fleet.MMHotlineStatus                 `json:"mmhotline_destinations"`
	}
	if e := json.Unmarshal(b, &status); e != nil {
		return e
	}
	if asJSON {
		if e := printJSON(status); e != nil {
			return e
		}
	} else {
		keys := []string{}
		for k := range status.Checks {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		width := nameColumnWidth(keys, 28)
		for _, key := range keys {
			st := status.Checks[key]
			fmt.Printf("%-*s %-12s %s\n", width, key, st.State, st.Explanation())
			// A check whose module is not core says which kind of module
			// that is: the configuration does not show it.
			if st.Module != "" && st.ModuleKind != "core" {
				fmt.Printf("  module %s: %s\n", st.Module, config.ModuleKindWords(st.ModuleKind))
			}
			// An every_systemd_unit check has one line for each failed unit.
			for _, unit := range checksandalerts.Names(st.FindingStatuses) {
				u := st.FindingStatuses[unit]
				fmt.Printf("  %-26s %-12s %s\n", unit, u.State, u.Explanation())
			}
			if name != "" {
				fmt.Printf("  Config: %s\n", st.Source)
				if st.Deadline.IsZero() {
					fmt.Printf("  Rule: check every %s, time limit per run %s, alert after failing for %s, recovered after healthy for %s\n", st.Timing.CheckEvery, st.Timing.TimeLimitPerRun, st.Timing.AlertAfterFailingFor, st.Timing.RecoveredAfterHealthyFor)
				} else {
					// A fleet check's receipts are reports.
					receipt := "heartbeat"
					if st.CheckGroup == "node_reports" {
						receipt = "report"
					}
					fmt.Printf("  Rule: next %s due by %s\n", receipt, st.Deadline.Format(time.RFC3339))
					if !st.Receipt.IsZero() {
						fmt.Printf("  Last %s: %s\n", receipt, st.Receipt.Format(time.RFC3339))
					}
				}
				printAlertSeverity(st)
				if unknown := st.AlertDestinationsTheHubDoesNotHave; len(unknown) > 0 {
					fmt.Printf("  The hub has no destination named %s. Destinations listed in send_alerts_only_to_destinations that the hub knows still receive alerts. If the hub knows none of the listed destinations, the check's alerts go to every destination whose alert_severities include the alert's severity.\n", strings.Join(unknown, ", "))
				}
				printObservation(st.Last)
				if st.FailureIncident.Evidence != nil {
					fmt.Println("  Failure evidence:")
					printEvidence(st.FailureIncident.Evidence)
				}
				if st.CannotCheckIncident.Evidence != nil {
					fmt.Println("  Cannot-check evidence:")
					printEvidence(st.CannotCheckIncident.Evidence)
				}
				for _, unit := range checksandalerts.Names(st.FindingStatuses) {
					if t := st.FindingStatuses[unit].FailureIncident; t.Incident != "" {
						fmt.Printf("  Open failure incident of %s, alert severity %s:\n", unit, t.AlertSeverity)
						printEvidence(t.Evidence)
					}
				}
			}
		}
		// A check that fails while a check that it names as its cause fails
		// sends no alert; each such check has a line that says so.
		for _, key := range keys {
			if sentence := status.Checks[key].KeptQuietSentence(); sentence != "" {
				fmt.Println(sentence)
			}
		}
		for _, n := range checksandalerts.Names(status.Maintenance) {
			fmt.Printf("Maintenance on: %s %s\n", n, status.Maintenance[n].Describe())
		}
		fmt.Println(status.Delivery.Describe())
		for _, failing := range status.Delivery.FailingDestinations {
			fmt.Println(failing.Describe())
		}
		for _, unit := range checksandalerts.Names(status.SystemdUnitRestartAttempts) {
			fmt.Printf("Systemd unit restarts %s: %s\n", unit, attemptsUsed(status.SystemdUnitRestartAttempts[unit]))
		}
		for _, group := range checksandalerts.Names(status.RecoveryCommandAttempts) {
			fmt.Printf("Recovery command %s: %s\n", group, attemptsUsed(status.RecoveryCommandAttempts[group]))
		}
		if h := status.Hub; h != nil {
			line := "Fleet hub: " + h.Address + " as " + h.Node + "; "
			if h.LastReport.IsZero() {
				line += "no report accepted yet"
			} else {
				line += "last report " + h.LastReport.Format(time.RFC3339)
			}
			if h.FleetSettingsVersion == "" {
				line += "; no settings from the hub yet, so alerts wait for the hub"
			}
			if h.LastError != "" {
				line += "; error at " + h.ErrorAt.Format(time.RFC3339) + ": " + h.LastError
			}
			fmt.Println(line)
		}
		if status.FleetHub != "" {
			fmt.Printf("Fleet hub: nodes report to %s; see mmwatchdog fleet\n", status.FleetHub)
		}
		for _, destination := range status.MMHotlineDestinations {
			prefix := "MMHotline " + destination.Destination + ": "
			line := prefix + destination.URL
			if destination.LastSignedAnswerAt == nil {
				line += " has not given a signed answer yet"
			} else {
				line += " last gave a signed answer at " + destination.LastSignedAnswerAt.UTC().Format("15:04 UTC")
			}
			fmt.Println(line)
			for _, problem := range append(destination.DeliveryProblems, destination.ClockWarning, destination.CheckInShortened) {
				if problem != "" {
					fmt.Println(prefix + problem)
				}
			}
		}
		if sentence := status.MeasurementHistory.CutShortSentence(time.Now()); sentence != "" {
			fmt.Println(sentence)
		}
		if status.StorageError != "" {
			fmt.Println("Storage:", status.StorageError)
		}
		if status.SendsAlertsToNobody {
			fmt.Println("This node sends alerts to nobody.")
		}
	}
	if status.Summary.ExitCode != 0 {
		return exitStatus(status.Summary.ExitCode)
	}
	return nil
}

// attemptsUsed describes the attempts of one recovery action and its last
// result: "1 of 3 attempts used; restart requested", or with
// max_attempts: unlimited "4 attempts used, unlimited; restart requested".
func attemptsUsed(a daemon.RecoveryAttemptStatus) string {
	used := fmt.Sprintf("%d of %s attempts used", a.AttemptsUsed, a.MaxAttempts)
	if a.MaxAttempts == "unlimited" {
		used = fmt.Sprintf("%s used, unlimited", countOf(a.AttemptsUsed, "attempt"))
	}
	// After a reset, no attempt has a result to show.
	if a.LastResult == "" {
		return used
	}
	return used + "; " + a.LastResult
}

func printEvidence(e *checksandalerts.Evidence) {
	if e == nil {
		return
	}
	fmt.Printf("  Confirmed incident: %s\n  Config at failure: %s\n", e.Incident, e.Source)
	// A heartbeat has no waiting times; its message states its rule.
	if e.Timing != (checksandalerts.Timing{}) {
		fmt.Printf("  Waiting times: alert after failing for %s, recovered after healthy for %s\n", e.Timing.AlertAfterFailingFor, e.Timing.RecoveredAfterHealthyFor)
	}
	fmt.Printf("  Result: %s — %q\n", e.Observation.Status, e.Observation.Message)
	printObservation(e.Observation)
	if e.Truncated {
		fmt.Println("  Evidence truncated to the retained size limit")
	}
}
func printObservation(o checksandalerts.Observation) {
	if o.SampleWindow > 0 {
		fmt.Printf("  Sample window: %s\n", o.SampleWindow)
	}
	if !o.At.IsZero() {
		fmt.Println("  Observed:", o.At.Format(time.RFC3339Nano))
	}
	if o.ExitCode != nil {
		fmt.Println("  Exit code:", *o.ExitCode)
	}
	if o.Stdout != "" {
		fmt.Printf("  stdout: %q\n", o.Stdout)
	}
	if o.Stderr != "" {
		fmt.Printf("  stderr: %q\n", o.Stderr)
	}
	for _, finding := range o.Findings {
		fmt.Println("  Finding " + finding.ID + ": " + finding.Message)
	}
	for _, line := range o.Details {
		fmt.Println("  " + line)
	}
	for _, n := range checksandalerts.Names(o.Measurements) {
		v := o.Measurements[n]
		fmt.Printf("  %s: %g %s\n", n, v.Value, v.Unit)
	}
}
func sendHeartbeat(ctx context.Context, name, url, tokenPath, trustedCertificateAuthorityPath string) error {
	if name == "" || tokenPath == "" || strings.ContainsAny(name, "/?#") {
		return fmt.Errorf("heartbeat requires a check group name and --bearer-token-file")
	}
	if url == "" && trustedCertificateAuthorityPath != "" {
		return fmt.Errorf("heartbeat --trusted-certificate-authority-file applies only with --receiver-url; a heartbeat without --receiver-url goes through the heartbeat socket, which needs no certificate")
	}
	if url != "" && !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("heartbeat --receiver-url must start with https://")
	}
	cannotRead := fmt.Errorf("the account that runs this job cannot read %s: give the job its own copy of the token that its account can read, for example: sudo install -o ACCOUNT -m 0600 SOURCE_TOKEN_FILE %s", tokenPath, tokenPath)
	if err := checksandalerts.ValidateOwnTokenFile(tokenPath); errors.Is(err, fs.ErrPermission) {
		return cannotRead
	} else if err != nil {
		fmt.Fprintln(os.Stderr, "warning:", err)
	}
	token, e := checksandalerts.ReadHeartbeatToken(tokenPath)
	if errors.Is(e, fs.ErrPermission) {
		return cannotRead
	}
	if e != nil {
		return e
	}
	var cl *http.Client
	var endpoint string
	var beforeSending func() error
	if url == "" {
		// A job on the node reports through the heartbeat socket. The token
		// goes to the socket only after it is clear that root owns the
		// socket and its directory: only root can listen there.
		socket := heartbeatSocketPath
		tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}
		defer tr.CloseIdleConnections()
		cl = &http.Client{Transport: tr, Timeout: 5 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
		endpoint = "http://mmwatchdog-heartbeat-socket"
		beforeSending = func() error { return checkHeartbeatSocketOwnership(socket, heartbeatSocketOwnerID) }
	} else {
		var roots *x509.CertPool
		if trustedCertificateAuthorityPath != "" {
			b, e := os.ReadFile(trustedCertificateAuthorityPath)
			if e != nil {
				return e
			}
			roots = x509.NewCertPool()
			if !roots.AppendCertsFromPEM(b) {
				return fmt.Errorf("%s: contains no PEM certificates", trustedCertificateAuthorityPath)
			}
		}
		secure := primitives.ServerTLSConfig(roots, false)
		secure.MinVersion = tls.VersionTLS12
		tr := &http.Transport{TLSClientConfig: secure}
		defer tr.CloseIdleConnections()
		cl = &http.Client{Transport: tr, Timeout: 5 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
		endpoint = strings.TrimRight(url, "/")
	}
	refuser, holder := "the receiver", "the receiver"
	if url == "" {
		refuser, holder = "MMWatchdog", "this node"
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		if beforeSending != nil {
			if e := beforeSending(); e != nil {
				var refusal heartbeatSocketRefusal
				if errors.As(e, &refusal) {
					return e
				}
				last = e
				continue
			}
		}
		req, e := http.NewRequestWithContext(ctx, "POST", endpoint+"/v1/heartbeats/"+name, nil)
		if e != nil {
			return e
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, e := cl.Do(req)
		if e != nil {
			last = e
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) // Heartbeat acceptance is determined by HTTP status; draining the unused body only helps connection reuse.
		resp.Body.Close()
		if resp.StatusCode == 204 {
			fmt.Println("Heartbeat accepted:", name)
			return nil
		}
		last = fmt.Errorf("the heartbeat receiver returned HTTP status code %d", resp.StatusCode)
		if resp.StatusCode == 401 {
			// The receiver gives one answer for a wrong token and for an unknown check group, so that a stranger cannot learn which check groups exist.
			last = &commandOutcome{1, fmt.Errorf("%s refused the heartbeat (HTTP status code 401): the token does not match, or %s has no heartbeat check group named %s", refuser, holder, name)}
		} else if resp.StatusCode == 404 {
			last = &commandOutcome{1, fmt.Errorf("%s refused the heartbeat (HTTP status code 404): %s has no heartbeat check group named %s", refuser, holder, name)}
		}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return last
		}
	}
	return last
}

// nameColumnWidth returns the width of a table's name column: at least
// minimum, and wide enough for the longest name, so that every state lines
// up under the others.
func nameColumnWidth(names []string, minimum int) int {
	width := minimum
	for _, n := range names {
		if len(n) > width {
			width = len(n)
		}
	}
	return width
}

// noEventsText says that the node keeps no event of what events asked for,
// so that an empty answer is not mistaken for a failed command, and names
// the check group when the check group itself may be misspelled.
func noEventsText(checkGroup, check string) string {
	switch {
	case check != "":
		return fmt.Sprintf("No events of the check %s/%s in the node's event history. mmwatchdog status lists the check groups and checks of this node.", checkGroup, check)
	case checkGroup != "":
		return fmt.Sprintf("No events of the check group %s in the node's event history. mmwatchdog status lists the check groups of this node.", checkGroup)
	default:
		return "No events in the node's event history."
	}
}
