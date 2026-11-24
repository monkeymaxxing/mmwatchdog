package daemon

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
)

const responseWriteTimeout = 30 * time.Second

func (d *Daemon) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		group := r.URL.Query().Get("check_group")
		// The fleet store has its own lock; read it before taking d.mu.
		var fleetProblems []string
		if d.hub != nil {
			fleetProblems = d.hub.Problems()
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		checks, delivery, storageError := d.status(group, d.now(), fleetProblems)
		if group != "" && len(checks) == 0 {
			apiError(w, fmt.Errorf("unknown check group name %q", group), http.StatusNotFound)
			return
		}
		var link *fleet.HubLink
		if d.link != nil {
			l := *d.link
			if d.fleetSettings != nil {
				l.FleetSettingsVersion = d.fleetSettings.Version
			}
			link = &l
		}
		unitAttempts, commandAttempts := d.recoveryAttemptStatus(group)
		summary := incidents.Summarize(checks, storageError)
		if len(delivery.FailingDestinations) > 0 {
			// A failed check (exit 1) takes precedence over health unknown (exit 3).
			summary.ExitCode = incidents.HealthExitCode(summary.ExitCode, 3)
		}
		status := map[string]any{"summary": summary, "checks": checks, "systemd_unit_restart_attempts": unitAttempts, "recovery_command_attempts": commandAttempts, "maintenance": d.snapshot.Maintenance, "delivery": delivery, "storage_error": storageError, "joined_fleet_hub": link,
			// Without a hub and without a destination, MMWatchdog tells nobody.
			"sends_alerts_to_nobody": d.config.JoinFleetHub == nil && len(d.config.Alerts.Destinations) == 0,
			"measurement_history":    d.measurementHistoryStatus(d.now())}
		if d.hub != nil {
			status["fleet_hub_role_address"] = d.hub.Address
		}
		status["mmhotline_destinations"] = d.mmhotlineStatus()
		writeJSON(w, status)
	})
	mux.HandleFunc("GET /fleet", func(w http.ResponseWriter, r *http.Request) {
		if d.hub == nil {
			apiError(w, fmt.Errorf("fleet inspection runs on the hub; this node has no fleet_hub_role setting"), 400)
			return
		}
		name := r.URL.Query().Get("node")
		if name == "" {
			fleet, e := d.hub.FleetOfNodeGroup(d.now(), r.URL.Query().Get("node_group"))
			if e != nil {
				apiError(w, e, 404)
				return
			}
			writeJSON(w, fleet)
			return
		}
		since, until, e := eventTimeRange(r)
		if e != nil {
			apiError(w, e, 400)
			return
		}
		node, e := d.hub.NodeEventsInTime(name, d.now(), since, until)
		if node.Name == "" {
			apiError(w, e, 404)
			return
		}
		problems := d.hub.Problems()
		if e != nil {
			problems = append(problems, "fleet event history: "+e.Error())
		}
		node.StorageError = strings.Join(problems, "; ")
		if node.StorageError != "" {
			node.ExitCode = incidents.HealthExitCode(node.ExitCode, 3)
		}
		writeJSON(w, node)
	})
	mux.HandleFunc("GET /fleet/findings", func(w http.ResponseWriter, r *http.Request) {
		if d.hub == nil {
			apiError(w, fmt.Errorf("fleet findings runs on the hub; this node has no fleet_hub_role setting"), 400)
			return
		}
		writeJSON(w, d.hub.Findings(r.URL.Query().Get("check"), r.URL.Query().Get("finding"), d.now()))
	})
	mux.HandleFunc("GET /fleet/set-aside-findings", func(w http.ResponseWriter, r *http.Request) {
		if d.hub == nil {
			apiError(w, fmt.Errorf("fleet set-aside-findings runs on the hub; this node has no fleet_hub_role setting"), 400)
			return
		}
		writeJSON(w, map[string]any{"set_aside_findings": d.hub.SetAsideFindingsNow(d.now())})
	})
	mux.HandleFunc("GET /fleet/join-code", func(w http.ResponseWriter, r *http.Request) {
		if d.hub == nil {
			apiError(w, fmt.Errorf("only the hub has a join code; this node has no fleet_hub_role setting"), 400)
			return
		}
		writeJSON(w, map[string]string{"join_code": d.hub.JoinCode().String()})
	})
	mux.HandleFunc("POST /fleet/rotate-join-code", func(w http.ResponseWriter, r *http.Request) {
		if d.hub == nil {
			apiError(w, fmt.Errorf("only the hub has a join code; this node has no fleet_hub_role setting"), 400)
			return
		}
		code, e := d.hub.Rotate()
		if e != nil {
			apiError(w, e, 503)
			return
		}
		writeJSON(w, map[string]string{"join_code": code.String()})
	})
	mux.HandleFunc("POST /fleet/forget/{node}", func(w http.ResponseWriter, r *http.Request) {
		if d.hub == nil {
			apiError(w, fmt.Errorf("fleet commands run on the hub; this node has no fleet_hub_role setting"), 400)
			return
		}
		// The fleet store commits the forget first; the daemon then retires the
		// node's check.
		name := r.PathValue("node")
		joined, status, e := d.hub.Forget(name)
		if e != nil {
			apiError(w, e, status)
			return
		}
		d.forgot(name, joined, d.now())
		writeJSON(w, map[string]string{"forgotten": name})
	})
	mux.HandleFunc("GET /config", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		current := *d.config
		var timings *config.FleetTimings
		if current.JoinFleetHub != nil && d.fleetSettings != nil {
			current.Alerts.Destinations = d.fleetSettings.Destinations
			copy := d.fleetSettings.Fleet
			timings = &copy
		}
		writeJSON(w, struct {
			config.Config
			FleetTimingsFromHub *config.FleetTimings `json:"fleet_timings_from_hub,omitempty"`
		}{current, timings})
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		since, until, e := eventTimeRange(r)
		if e != nil {
			apiError(w, e, 400)
			return
		}
		var es []checksandalerts.Event
		if since.IsZero() {
			es, e = d.store.CheckGroupEvents(r.URL.Query().Get("check_group"), 100)
		} else {
			es, e = d.store.CheckGroupEventsInTime(r.URL.Query().Get("check_group"), since, until)
		}
		if e != nil {
			apiError(w, e, 500)
			return
		}
		writeJSON(w, es)
	})
	mux.HandleFunc("POST /reload", func(w http.ResponseWriter, r *http.Request) {
		response := http.NewResponseController(w)
		if e := response.SetWriteDeadline(time.Time{}); e != nil {
			apiError(w, e, 500)
			return
		}
		result, e := d.reload()
		if e != nil {
			apiError(w, e, 400)
			return
		}
		result.DestinationVerifications = d.verifyDestinations(r.Context(), result.NewOrChangedDestinations)
		if e := response.SetWriteDeadline(time.Now().Add(responseWriteTimeout)); e != nil {
			apiError(w, e, 500)
			return
		}
		writeJSON(w, result)
	})
	mux.HandleFunc("POST /check-now/{check_group}", func(w http.ResponseWriter, r *http.Request) {
		// Execution has per-check timeouts and may wait for shared slots.
		// Start the response write deadline only when the observations are ready.
		response := http.NewResponseController(w)
		if e := response.SetWriteDeadline(time.Time{}); e != nil {
			apiError(w, e, 500)
			return
		}
		runChecks := d.checkNowGroup
		if r.URL.Query().Get("count_as_scheduled_run") == "true" {
			runChecks = d.runScheduledChecksNow
		}
		obs, e := runChecks(r.Context(), r.PathValue("check_group"))
		if err := response.SetWriteDeadline(time.Now().Add(responseWriteTimeout)); err != nil {
			apiError(w, err, 500)
			return
		}
		if e != nil {
			apiError(w, e, 400)
			return
		}
		writeJSON(w, obs)
	})
	mux.HandleFunc("POST /maintenance/{action}/{check_groups}", d.maintenance)
	mux.HandleFunc("POST /send-notice", d.sendNotice)
	mux.HandleFunc("POST /reset-recovery-attempts/{check_group}", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		name := r.PathValue("check_group")
		// A check group's budgets are the restarts of the unit that its
		// restart_systemd_unit names and its recovery command.
		ch, ok := d.config.CheckGroups[name]
		keys := []string{}
		if !ok {
			apiError(w, fmt.Errorf("unknown check group name %q", name), 404)
			return
		}
		if unit := ch.RestartUnit(); unit != "" {
			keys = append(keys, unit)
		}
		if ok && ch.RecoveryCommand != nil {
			keys = append(keys, name)
		}
		if len(keys) == 0 {
			apiError(w, fmt.Errorf("check group has no restart_systemd_unit and no recovery_command"), 400)
			return
		}
		candidate := d.snapshot.Clone()
		for _, key := range keys {
			if d.recovering[key] {
				apiError(w, fmt.Errorf("a recovery action is in progress for the check group"), 409)
				return
			}
			candidate.RecoveryAttemptsByTarget[key] = &incidents.RecoveryAttempts{}
		}
		event := checksandalerts.Event{ID: incidents.ID(), CheckGroup: name, Type: "recovery_attempts_reset", At: d.now(), Message: "operator reset recovery attempts"}
		if e := d.control(candidate, []checksandalerts.Event{event}, d.route()); e != nil {
			apiError(w, e, 500)
			return
		}
		writeJSON(w, map[string]bool{"recovery_attempts_reset": true})
	})
	return mux
}

// status derives the inspection view that status and fleet reports share:
// the checks, the notification queue, and every storage problem, with
// extra problems last. The caller holds d.mu.
func (d *Daemon) status(group string, now time.Time, extra []string) (map[string]incidents.CheckStatus, store.DeliverySummary, string) {
	checks := map[string]incidents.CheckStatus{}
	for key, st := range d.snapshot.Checks {
		if group != "" && st.CheckGroup != group {
			continue
		}
		needsFresh := d.checks[key].needsFresh()
		status := incidents.CheckStatus{CheckState: *st, State: st.Summary(now, needsFresh), NeedsFresh: needsFresh}
		//nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
		if check := d.checks[key].def.Check; check.Inputs != nil {
			status.Module, status.ModuleKind = check.ModuleName(), check.ModuleKind()
		}
		// Status shows what every step applies (quietWhile): a check is kept
		// quiet from the moment a check that it names starts to fail, and
		// until its own next run after that check has stopped failing.
		status.QuietNowWhileCheckFails = d.failingCause(key, st)
		if status.QuietNowWhileCheckFails == "" {
			status.QuietUntilNextRunAfterCheckFailed = st.QuietNowWhileCheckFails
		}
		// Each failed unit of an every_systemd_unit check has its own
		// state. While MMWatchdog cannot ask systemd, nothing is known about a unit
		// either, and its open incident stays.
		for _, unit := range st.FindingStates() {
			unitNeedsFresh := needsFresh || st.Last.Status == "unknown"
			if status.FindingStatuses == nil {
				status.FindingStatuses = map[string]incidents.CheckStatus{}
			}
			unitStatus := incidents.CheckStatus{CheckState: *unit, State: unit.Summary(now, unitNeedsFresh), NeedsFresh: unitNeedsFresh}
			unitStatus.QuietNowWhileCheckFails, unitStatus.QuietUntilNextRunAfterCheckFailed = status.QuietNowWhileCheckFails, status.QuietUntilNextRunAfterCheckFailed
			status.FindingStatuses[unit.Finding] = unitStatus
		}
		// A joined node knows the hub's destinations from its copy of the
		// fleet settings; a wrong name must show, because nothing refuses it.
		if d.config.JoinFleetHub != nil && d.fleetSettings != nil {
			status.AlertDestinationsTheHubDoesNotHave = config.UnknownDestinationNames(st.SendAlertsOnlyToDestinations, d.fleetSettings.Destinations)
		} else if d.config.FleetHubRole != nil && st.CheckGroup == checksandalerts.NodeReportsCheckGroup {
			status.AlertDestinationsTheHubDoesNotHave = config.UnknownDestinationNames(st.SendAlertsOnlyToDestinations, d.config.Alerts.Destinations)
			status.RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent = d.members[st.Check].RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent
		}
		// A node status report carries it, so the hub reminds of a fleet
		// incident key only while one of its incidents is due a reminder.
		if rc := d.checks[key]; rc != nil {
			status.MarkReminderEligible(d.remindersHeldBack(key, rc, now))
		}
		checks[key] = status
	}
	problems := d.storage.Problems()
	delivery, e := d.store.DeliveryStats()
	if e == nil {
		// Every failing destination affects status, including MMHotline;
		// mmhotlineStatus adds the endpoint and signature details.
		delivery.FailingDestinations, e = d.store.FailingDestinations("")
		if e == nil {
			delivery.FailingDestinations = d.withFailingVerifications(delivery.FailingDestinations, now)
		}
	}
	if e != nil {
		problems = append(problems, "delivery statistics: "+e.Error())
	}
	return checks, delivery, strings.Join(append(problems, extra...), "; ")
}

func (d *Daemon) maintenance(w http.ResponseWriter, r *http.Request) {
	var req struct {
		For                              checksandalerts.Duration `json:"for"`
		Reason                           string                   `json:"reason"`
		User                             string                   `json:"user"`
		WithoutMaintenanceOnAndOffAlerts bool                     `json:"without_maintenance_on_and_off_alerts"`
	}
	b, e := io.ReadAll(io.LimitReader(r.Body, 4097))
	if e != nil || len(b) > 4096 {
		apiError(w, fmt.Errorf("invalid maintenance request"), 400)
		return
	}
	if e = checksandalerts.DecodeJSON(b, &req); e != nil {
		apiError(w, e, 400)
		return
	}
	action := r.PathValue("action")
	duration := time.Duration(0)
	if action != "on" && action != "off" {
		apiError(w, fmt.Errorf("maintenance requires on or off"), 400)
		return
	}
	if action == "on" {
		duration, e = checksandalerts.ParseDuration(string(req.For))
	}
	// Every window has an end that the operator wrote; no maximum bounds it.
	if (action == "on" && (e != nil || duration <= 0)) || (action == "off" && (req.For != "" || req.Reason != "")) {
		apiError(w, fmt.Errorf("maintenance on requires a positive duration such as 1h or 30d; off takes no duration or reason"), 400)
		return
	}
	if !checksandalerts.PlainText(req.Reason, 200) {
		apiError(w, fmt.Errorf("maintenance reason must be single-line text of at most 200 bytes"), 400)
		return
	}
	if !checksandalerts.PlainText(req.User, 64) {
		apiError(w, fmt.Errorf("maintenance user must be single-line text of at most 64 bytes"), 400)
		return
	}
	// changeMaintenance changes the windows under d.mu and says whether the
	// node then reports to a hub.
	changeMaintenance := func() (response map[string]any, reportToHub bool, status int, e error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		// One command names one or more check groups, separated by commas, or
		// every_check_group alone for the whole node.
		names := []string{}
		for _, name := range strings.Split(r.PathValue("check_groups"), ",") {
			if _, ok := d.config.CheckGroups[name]; !ok && name != checksandalerts.EveryCheckGroup && (name != checksandalerts.NodeReportsCheckGroup || !d.config.InFleet()) {
				return nil, false, 404, fmt.Errorf("unknown check group name %q", name)
			}
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
		if len(names) > 1 && slices.Contains(names, checksandalerts.EveryCheckGroup) {
			return nil, false, 400, fmt.Errorf("%s selects every check group on the node and cannot be combined with check group names", checksandalerts.EveryCheckGroup)
		}
		now := d.now()
		candidate := d.snapshot.Clone()
		var events []checksandalerts.Event
		if action == "off" {
			message := "maintenance turned off"
			if req.User != "" {
				message += " by " + req.User
			}
			// Ending a window that already expired changes nothing, also for
			// the checks of that check group.
			named := names
			names = slices.DeleteFunc(slices.Clone(names), func(name string) bool { _, open := candidate.Maintenance[name]; return !open })
			if len(names) == 0 {
				return map[string]any{"check_groups": named, "maintenance": nil}, false, 0, nil
			}
			events = candidate.EndMaintenance(names, now, message)
		} else {
			// Every check group of the command gets the same end time, and the
			// command sends one alert.
			window := checksandalerts.Window{Until: now.Add(duration), Reason: req.Reason, User: req.User, WithoutMaintenanceOnAndOffAlerts: req.WithoutMaintenanceOnAndOffAlerts}
			for _, name := range names {
				candidate.Maintenance[name] = window
			}
			for _, st := range candidate.Checks {
				if slices.ContainsFunc(names, func(name string) bool { return incidents.Covers(name, st.CheckGroup) }) {
					st.Interrupt()
				}
			}
			events = []checksandalerts.Event{incidents.MaintenanceEvent("maintenance_on", names, window, now, "maintenance "+window.Describe())}
		}
		if e = d.control(candidate, events, d.route()); e != nil {
			return nil, false, 500, e
		}
		for _, name := range names {
			d.requireFresh(name, now, false)
		}
		return map[string]any{"check_groups": names, "maintenance": d.snapshot.Window(names[0])}, d.config.JoinFleetHub != nil, 0, nil
	}
	response, reportToHub, status, e := changeMaintenance()
	if e != nil {
		apiError(w, e, status)
		return
	}
	// On a joined node the command returns once the hub has the change, so
	// that a reboot right after it sends no alert that the node stopped
	// reporting. The window stays on the node when the hub cannot get it.
	if reportToHub {
		if e = d.reportToHubAtOnce(r.Context()); e != nil {
			response["fleet_hub_report_error"] = e.Error()
		} else {
			response["fleet_hub_accepted_the_report"] = true
		}
	}
	writeJSON(w, response)
}

func (d *Daemon) heartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || !strings.HasPrefix(r.URL.Path, "/v1/heartbeats/") {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/v1/heartbeats/")
	d.mu.Lock()
	defer d.mu.Unlock()
	token, ok := d.tokens[name]
	supplied := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare([]byte(token), []byte(supplied)) != 1 {
		apiError(w, fmt.Errorf("invalid heartbeat credentials"), 401)
		return
	}
	key := name + "/heartbeat"
	now := d.now()
	def := d.checks[key].def //nolint:nilaway // loadTokens includes only configured heartbeat groups; authentication and locked reloads preserve their runtime and snapshot entries.
	candidate := d.snapshot.Clone()
	st := candidate.Checks[key]
	st.Receipt = now                                                      //nolint:nilaway // loadTokens includes only configured heartbeat groups; authentication and locked reloads preserve their runtime and snapshot entries.
	st.Deadline = now.Add(def.Heartbeat.MaxTimeBetweenHeartbeats.Value()) //nolint:nilaway // loadTokens includes only configured heartbeat groups; authentication and locked reloads preserve their runtime and snapshot entries.
	o := checksandalerts.Result("ok", "heartbeat received")
	o.At = now
	events := st.Observe(o, def.Timing, d.suppressed(name))
	// Acknowledge only a committed receipt and deadline.
	if e := d.control(candidate, events, d.route()); e != nil {
		apiError(w, fmt.Errorf("heartbeat receipt could not be saved"), 503)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func apiError(w http.ResponseWriter, e error, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": e.Error()})
}
func CheckNow(ctx context.Context, c *config.Config, name string) (map[string]checksandalerts.Observation, error) {
	if _, ok := c.CheckGroups[name]; !ok {
		return nil, fmt.Errorf("unknown check group name %q", name)
	}
	obs := map[string]checksandalerts.Observation{}
	defs := config.CheckDefinitions(c)
	for _, key := range checksandalerts.Names(defs) {
		def := defs[key]
		if def.CheckGroup != name {
			continue
		}
		if def.Heartbeat != nil {
			return nil, fmt.Errorf("heartbeat checks receive heartbeats instead of running on demand. Use mmwatchdog status to inspect the latest receipt and deadline")
		}
		cc, cancel := context.WithTimeout(ctx, def.Timing.TimeLimitPerRun.Value())
		run := checksandalerts.NewCheckRun(def.CheckWithSeverity(), def.Timing)
		run.CheckGroup, run.CheckName, run.ModuleStateDir = def.CheckGroup, def.Name, c.ModuleStateDir
		// The command goes on to the next check when a run does not end.
		started, ended := time.Now(), make(chan checksandalerts.Observation, 1)
		go func() { ended <- checksandalerts.RunCheck(cc, run) }()
		o, inTime := waitForRun(cc, ended)
		if !inTime {
			o = runThatDoesNotEnd(started, time.Now().UTC(), def.Timing.TimeLimitPerRun)
		}
		obs[def.Name] = o
		cancel()
	}
	return obs, nil
}

func (d *Daemon) checkNowGroup(ctx context.Context, name string) (map[string]checksandalerts.Observation, error) {
	d.mu.Lock()
	defs := map[string]config.Definition{}
	for key, rc := range d.checks {
		if rc.def.CheckGroup == name {
			defs[key] = rc.def
		}
	}
	d.mu.Unlock()
	if len(defs) == 0 {
		return nil, fmt.Errorf("unknown check group name %q", name)
	}
	obs := map[string]checksandalerts.Observation{}
	for _, key := range checksandalerts.Names(defs) {
		def := defs[key]
		if def.Heartbeat != nil {
			return nil, fmt.Errorf("heartbeat checks receive heartbeats instead of running on demand. Use mmwatchdog status to inspect the latest receipt and deadline")
		}
		obs[def.Name] = d.execute(ctx, key, def, &checksandalerts.CheckMemory{}, nil)
	}
	return obs, nil
}

// runScheduledChecksNow is mmwatchdog check-now --count-as-scheduled-run:
// it moves the next scheduled run of each check of the check group to now,
// as a maintenance boundary does (requireFresh), and returns the
// observations of those runs once the scheduler has applied them. So after
// a fix the check's state, its alerts, and its recovery follow at once, with
// the check's own confirmation, and its next run comes check_every later.
// A run that had started before the request does not count as the
// requested one. Canceling stops the waiting, not the runs.
func (d *Daemon) runScheduledChecksNow(ctx context.Context, name string) (map[string]checksandalerts.Observation, error) {
	d.mu.Lock()
	requested := d.now()
	keys := []string{}
	for key, rc := range d.checks {
		if rc.def.CheckGroup != name {
			continue
		}
		if rc.def.Heartbeat != nil {
			d.mu.Unlock()
			return nil, fmt.Errorf("heartbeat checks receive heartbeats instead of running on demand. Use mmwatchdog status to inspect the latest receipt and deadline")
		}
		rc.next = requested
		keys = append(keys, key)
	}
	d.mu.Unlock()
	if len(keys) == 0 {
		return nil, fmt.Errorf("unknown check group name %q", name)
	}
	tick := time.NewTicker(config.ScheduleInterval)
	defer tick.Stop()
	for {
		obs := map[string]checksandalerts.Observation{}
		d.mu.Lock()
		for _, key := range keys {
			rc := d.checks[key]
			if rc == nil {
				d.mu.Unlock()
				return nil, fmt.Errorf("a configuration reload removed %s before the requested check result was available", key)
			}
			// The observation of the run that started at or after the
			// request; a run that a reload or a maintenance boundary
			// retired left none, and its check runs again at once.
			if st := d.snapshot.Checks[key]; st != nil && !rc.running && !rc.scheduled.Before(requested) && !st.Last.At.Before(rc.scheduled) {
				obs[rc.def.Name] = st.Last
			}
		}
		d.mu.Unlock()
		if len(obs) == len(keys) {
			return obs, nil
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// ValidateRuntime checks the configured TLS, token, credential, certificate
// authority, and join code files on the target node, and that shell checks,
// modules, and recovery commands can start there. The modules that are not
// core are those of the directories that the configuration was loaded with.
// It is read-only and can run before deployment while the daemon is still live.
func ValidateRuntime(c *config.Config) (err error) {
	defer func() {
		if err != nil && (os.IsNotExist(err) || strings.Contains(err.Error(), "no such file or directory")) {
			err = fmt.Errorf("%w\nThe file named in the error is missing on the machine running mmwatchdog. On a machine other than the monitored node, check only the configuration files: mmwatchdog validate --config-only", err)
		}
	}()
	if problems := c.SecretFilePermissionErrors(); len(problems) > 0 {
		return problems[0]
	}
	if _, err := loadTokens(c); err != nil {
		return err
	}
	if _, err := fleet.LoadJoinCode(c); err != nil {
		return err
	}
	// A database file that already holds more than its setting allows would
	// stop the daemon from starting with this configuration.
	if err := store.StateDatabaseLimit(c.StateDatabaseSizeLimit.Bytes()).SizeError(filepath.Join(c.StateDir, "state.db")); err != nil {
		return c.ErrorAt("state_database_size_limit", err.Error())
	}
	if h := c.FleetHubRole; h != nil {
		if err := store.FleetDatabaseLimit(h.EventHistoryOfNodesContentSizeLimit.Bytes()).SizeError(filepath.Join(c.StateDir, "fleet.db")); err != nil {
			return c.ErrorAt("fleet_hub_role.event_history_of_nodes_content_size_limit", err.Error())
		}
	}
	for _, name := range checksandalerts.Names(c.Alerts.Destinations) {
		d := c.Alerts.Destinations[name]
		base := "destinations." + name
		if module, ok := d.Inputs.(checksandalerts.ValidatedOnNode); ok {
			if problem := module.ValidateOnNode(); problem != nil {
				return c.ErrorAt(strings.TrimSuffix(base+"."+d.ModuleName()+"."+problem.Path, "."), problem.Message)
			}
		}
		if _, input, err := d.ReadCredentialFiles(); err != nil {
			return c.ErrorAt(base+"."+d.PathOfInput(input), err.Error())
		}
	}
	for _, name := range checksandalerts.Names(c.CheckGroups) {
		if r := c.CheckGroups[name].RecoveryCommand; r != nil {
			if problem := checksandalerts.ValidateExecOptionsOnNode(r.ExecOptions); problem != nil {
				return c.ErrorAt("check_groups."+name+".recovery_command."+problem.Path, problem.Message)
			}
		}
		for _, cn := range checksandalerts.Names(c.CheckGroups[name].Checks) {
			base := "check_groups." + name + ".checks." + cn
			if _, ok := c.Sources[base]; !ok {
				base = "check_groups." + name // a direct check
			}
			co := c.CheckGroups[name].Checks[cn]
			// What a check needs on this node is its module's to say.
			if module, ok := co.Inputs.(checksandalerts.ValidatedOnNode); ok {
				if problem := module.ValidateOnNode(); problem != nil {
					return c.ErrorAt(strings.TrimSuffix(base+"."+co.ModuleName()+"."+problem.Path, "."), problem.Message)
				}
			}
			// A check whose module is not core gets a state directory, which
			// MMWatchdog makes in module_state_dir. MMWatchdog makes that directory too, in
			// a directory that is there.
			if _, notCore := co.Inputs.(*checksandalerts.ModuleCall); notCore {
				if problem := checksandalerts.ModuleStateDirProblem(c.ModuleStateDir); problem != nil {
					return c.ErrorAt("module_state_dir", problem.Error())
				}
			}
		}
	}
	return nil
}

func loadTokens(c *config.Config) (map[string]string, error) {
	tokens := map[string]string{}
	seen := map[string]bool{}
	if c.HeartbeatReceiver != nil {
		if _, e := tls.LoadX509KeyPair(c.HeartbeatReceiver.TLSCertificateFile, c.HeartbeatReceiver.TLSPrivateKeyFile); e != nil {
			return nil, c.ErrorAt("heartbeat_receiver", e.Error())
		}
	}
	for n, ch := range c.CheckGroups {
		if ch.Heartbeat == nil {
			continue
		}
		t, e := checksandalerts.ReadHeartbeatToken(ch.Heartbeat.ExpectedBearerTokenFile)
		if e != nil {
			return nil, c.ErrorAt("check_groups."+n+".heartbeat.expected_bearer_token_file", e.Error())
		}
		if seen[t] {
			return nil, c.ErrorAt("check_groups."+n+".heartbeat.expected_bearer_token_file", "must use a different token for each heartbeat check group")
		}
		seen[t] = true
		tokens[n] = t
	}
	return tokens, nil
}

// RecoveryAttemptStatus contains the configured allowance and consumed
// attempts. MaxAttempts is a number, or the text unlimited.
type RecoveryAttemptStatus struct {
	AttemptsUsed          int                         `json:"attempts_used"`
	MaxAttempts           checksandalerts.MaxAttempts `json:"max_attempts"`
	LastAttemptFinishedAt time.Time                   `json:"last_attempt_finished_at,omitzero"`
	LastAttemptAt         time.Time                   `json:"last_attempt_at"`
	LastResult            string                      `json:"last_result"`
}

// recoveryAttemptStatus is the attempts of the unit restarts and of the
// recovery commands of one check group, or of every check group when group is "".
func (d *Daemon) recoveryAttemptStatus(group string) (map[string]RecoveryAttemptStatus, map[string]RecoveryAttemptStatus) {
	units, commands := map[string]RecoveryAttemptStatus{}, map[string]RecoveryAttemptStatus{}
	for key, policy := range d.policies {
		if group != "" && !slices.Contains(policy.CheckGroups, group) {
			continue
		}
		entry := RecoveryAttemptStatus{MaxAttempts: config.MaxAttemptsOf(policy.Attempts)}
		if attempts := d.snapshot.RecoveryAttemptsByTarget[key]; attempts != nil {
			entry.AttemptsUsed, entry.LastAttemptAt, entry.LastResult = attempts.Attempts, attempts.Last, attempts.LastResult
			entry.LastAttemptFinishedAt = attempts.LastFinished
		}
		if policy.Run == nil {
			units[key] = entry
		} else {
			commands[key] = entry
		}
	}
	return units, commands
}

// ValidationWarnings includes routing against a joined node's saved fleet
// settings, and on a hub the versions that its joined nodes report.
func ValidationWarnings(c *config.Config, node string, checkNodeFiles bool) config.ValidationWarnings {
	warnings := c.ValidationWarnings(node, checkNodeFiles)
	if checkNodeFiles && c.FleetHubRole != nil {
		warnings.OlderJoinedNodesWithAlertSourceForNodesSendingWithoutTheHub = c.OlderJoinedNodesWithNodeAlertSource(fleet.JoinedNodeVersions(c.StateDir), func(version string) bool { return fleet.ReleaseOlderThan(version, 0, 2) })
	}
	if checkNodeFiles && c.JoinFleetHub != nil {
		if settings := fleet.ReadFleetSettings(filepath.Join(c.StateDir, fleet.FleetSettingsFile), c.Modules); settings != nil {
			c.RoutingWarnings(settings.Destinations, &warnings)
		}
	}
	return warnings
}

// ValidateRouting checks a cause against a joined node's saved fleet settings.
func ValidateRouting(c *config.Config) error {
	if c.JoinFleetHub != nil {
		if settings := fleet.ReadFleetSettings(filepath.Join(c.StateDir, fleet.FleetSettingsFile), c.Modules); settings != nil {
			return c.ValidateCauseDestinations(settings.Destinations)
		}
	}
	return nil
}

// eventTimeRange decodes the inclusive event span sent by the CLI.
func eventTimeRange(r *http.Request) (time.Time, time.Time, error) {
	q := r.URL.Query()
	var since, until time.Time
	if !q.Has("since") && !q.Has("until") {
		return since, until, nil
	}
	var err error
	since, err = time.Parse(time.RFC3339Nano, q.Get("since"))
	if err != nil {
		return since, until, fmt.Errorf("since needs an RFC 3339 time, such as 2026-10-07T14:00:00Z")
	}
	until, err = time.Parse(time.RFC3339Nano, q.Get("until"))
	if err != nil {
		return since, until, fmt.Errorf("until needs an RFC 3339 time, such as 2026-10-07T15:00:00Z")
	}
	if until.Before(since) {
		return since, until, fmt.Errorf("until is before since; choose an end at or after the start")
	}
	// Match readings: valid RFC 3339 bounds outside the stored time range
	// select the available history instead of overflowing UnixNano.
	if since.Before(time.Unix(0, 0)) {
		since = time.Unix(0, 0)
	}
	if until.After(time.Unix(0, math.MaxInt64)) {
		until = time.Unix(0, math.MaxInt64)
	}
	return since, until, nil
}
