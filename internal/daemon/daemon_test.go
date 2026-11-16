package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
	"mmwatchdog/internal/testhelpers"
)

// daemonConfig writes the configuration of a daemon of a test and returns
// its directory. The node's three directories for modules are below that
// directory too, so the daemon never reads the modules or touches the module
// state of the machine that the test runs on. A test installs the modules
// of the project's own that it needs in c.ExternalModuleDir.
func daemonConfig(t *testing.T) (string, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	path := dir
	c := &config.Config{StateDir: filepath.Join(dir, "state"), Socket: filepath.Join(dir, "api.sock"), CheckGroups: map[string]checksandalerts.CheckGroup{"app": {Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true"}}}}}
	c.OfficialExtraModuleDir, c.ExternalModuleDir, c.ModuleStateDir = testhelpers.TestModuleDirs(dir)
	writeConfig(t, path, c)
	return path, c
}

func writeConfig(t *testing.T, path string, c *config.Config) {
	t.Helper()
	b, e := json.Marshal(c)
	if e != nil {
		t.Fatal(e)
	}
	// Use a minimal authored representation instead of zero-valued normalized fields.
	var m map[string]any
	json.Unmarshal(b, &m)
	delete(m, "max_concurrent_checks")
	// A configuration that names no directory for modules gets the test's
	// own, not those of the machine that the test runs on.
	officialExtraModuleDir, externalModuleDir, moduleStateDir := testhelpers.TestModuleDirs(path)
	for key, directory := range map[string]string{"official_extra_module_dir": officialExtraModuleDir, "external_module_dir": externalModuleDir, "module_state_dir": moduleStateDir} {
		if m[key] == "" {
			m[key] = directory
		}
	}
	// The storage settings a test leaves out take their defaults.
	for _, key := range []string{"keep_event_history_for", "event_history_content_size_limit", "keep_undelivered_alerts_for", "undelivered_alerts_content_size_limit", "state_database_size_limit"} {
		if m[key] == "" {
			delete(m, key)
		}
	}
	// Alert destinations live in alert_destinations.yml beside the main file.
	alerts := filepath.Join(path, "alert_destinations.yml")
	delete(m, "alerts")
	switch {
	case c.Alerts.Destinations == nil && c.JoinFleetHub != nil:
		// A node that has joined a hub gets its destinations from the hub.
		if e = os.Remove(alerts); e != nil && !os.IsNotExist(e) {
			t.Fatal(e)
		}
	case c.Alerts.Destinations == nil:
		// A test about other features runs with no destination.
		if e = os.WriteFile(alerts, []byte("destinations: {}\n"), 0600); e != nil {
			t.Fatal(e)
		}
	default:
		alertsMap := map[string]any{}
		b, _ = json.Marshal(c.Alerts)
		json.Unmarshal(b, &alertsMap)
		for _, d := range alertsMap["destinations"].(map[string]any) {
			if d.(map[string]any)["mmhotline"] == nil && d.(map[string]any)["remind_every"] == nil {
				d.(map[string]any)["remind_every"] = "never"
			}
			if d.(map[string]any)["alert_severities"] == nil {
				d.(map[string]any)["alert_severities"] = []string{"info", "warn", "sos"}
			}
		}
		b, _ = json.Marshal(alertsMap)
		if e = os.WriteFile(alerts, b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	// Tests about other features get the required alert severity
	// settings and timings filled in where they leave them out.
	for key, severity := range testhelpers.TestNodeSeverities {
		if _, ok := m[key]; !ok {
			m[key] = severity
		}
	}
	if hub, ok := m["fleet_hub_role"].(map[string]any); ok {
		for key, value := range testhelpers.TestHubHistory {
			if _, ok := hub[key]; !ok {
				hub[key] = value
			}
		}
	}
	for name, v := range m["check_groups"].(map[string]any) {
		ch := v.(map[string]any)
		fillCheckSeverity(ch, c.CheckGroups[name].Check.ModuleName())
		if checks, ok := ch["checks"].(map[string]any); ok {
			for checkName, check := range checks {
				fillCheckSeverity(check.(map[string]any), c.CheckGroups[name].Checks[checkName].ModuleName())
			}
		}
		if _, heartbeat := ch["heartbeat"]; !heartbeat {
			for key, timing := range testhelpers.TestTimings {
				if key == "alert_after_failing_for" && c.CheckGroups[name].Check.OpensIncidentAtFirstEvent() {
					continue
				}
				if _, ok := ch[key]; !ok {
					ch[key] = timing
				}
			}
		}
		account, err := user.Current()
		if err != nil {
			t.Fatal(err)
		}
		fillShellAccount := func(check map[string]any) {
			if shell, ok := check["shell"].(map[string]any); ok {
				if shell["run_as_user_account"] == "" || shell["run_as_user_account"] == nil {
					shell["run_as_user_account"] = account.Username
				}
				for _, key := range []string{"run_from_directory", "success_exit_codes"} {
					if shell[key] == "" || shell[key] == nil {
						delete(shell, key)
					}
				}
			}
		}
		fillShellAccount(ch)
		if checks, ok := ch["checks"].(map[string]any); ok {
			for _, check := range checks {
				fillShellAccount(check.(map[string]any))
			}
		}
		// A recovery action that states no waiting time is due at the
		// first failed run, and one that states no max_attempts gets the
		// default.
		for action, waitingTime := range testhelpers.TestActionWaitingTimes {
			if settings, ok := ch[action].(map[string]any); ok {
				if settings[waitingTime] == "" {
					settings[waitingTime] = "0s"
				}
				if settings["max_attempts"] == "" {
					settings["max_attempts"] = "3"
				}
				for key, value := range map[string]string{"min_time_between_attempts": "1m", "reset_attempts_after_healthy_for": "10m"} {
					if settings[key] == "" || settings[key] == nil {
						settings[key] = value
					}
				}
				if action == "recovery_command" {
					if settings["time_limit_per_attempt"] == "" {
						settings["time_limit_per_attempt"] = "1m"
					}
					if settings["run_as_user_account"] == "" {
						settings["run_as_user_account"] = account.Username
					}
					if settings["triggered_by_checks"] == nil {
						names := []string{}
						group := c.CheckGroups[name]
						if group.Check.Inputs != nil {
							names = append(names, group.Check.ModuleName())
						}
						for _, checkName := range checksandalerts.Names(group.Checks) {
							check := group.Checks[checkName]
							if !check.RecordsOnly() && check.ModuleName() != "systemd_unit_crash_loop" {
								names = append(names, checkName)
							}
						}
						settings["triggered_by_checks"] = names
					}
				}

			}
		}
		dropUnstatedStatusCodes := func(check map[string]any) {
			if http, ok := check["http"].(map[string]any); ok && http["expected_http_status_codes"] == nil {
				delete(http, "expected_http_status_codes")
			}
		}
		dropUnstatedStatusCodes(ch)
		if checks, ok := ch["checks"].(map[string]any); ok {
			for _, check := range checks {
				dropUnstatedStatusCodes(check.(map[string]any))
			}
		}
	}
	groups, _ := json.Marshal(map[string]any{"check_groups": m["check_groups"]})
	delete(m, "check_groups")
	if e = os.MkdirAll(filepath.Join(path, "check_groups"), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(path, "check_groups", "checks.yml"), groups, 0600); e != nil {
		t.Fatal(e)
	}
	b, _ = json.Marshal(m)
	if e = os.WriteFile(filepath.Join(path, "mmwatchdog_node_settings.yml"), b, 0600); e != nil {
		t.Fatal(e)
	}
}

// fillCheckSeverity gives a check, or a group mapping that is one, that
// can alert alert_severity: warn when it has none. module is the name of
// the check's module, or "" for a mapping that names none, such as a
// heartbeat.
func fillCheckSeverity(check map[string]any, module string) {
	if _, ok := check["alert_severity"]; ok {
		return
	}
	if _, heartbeat := check["heartbeat"]; module == "" && !heartbeat {
		return
	}
	if testhelpers.MeasurementKinds[module] && check["fail_when_measurement_above"] == nil && check["fail_when_measurement_below"] == nil {
		check["only_record_readings"] = true
		return // records only
	}
	check["alert_severity"] = "warn"
}

func TestDaemonLockBeforeStateAccess(t *testing.T) {
	t.Parallel()
	p, _ := daemonConfig(t)
	d, e := NewDaemon(p)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	if other, e := NewDaemon(p); e == nil {
		other.Close()
		t.Fatal("second daemon opened the same state")
	}
}
func TestDaemonLockReleasedWithInheritedDescriptor(t *testing.T) {
	t.Parallel()
	p, _ := daemonConfig(t)
	d, e := NewDaemon(p)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if d != nil {
			d.Close()
		}
	}()
	// Keep a fork's copy of the lock alive across the daemon restart.
	child := exec.Command("sleep", "60")
	child.ExtraFiles = []*os.File{d.lock}
	if e = child.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() {
		child.Process.Kill()
		child.Wait()
	}()
	if e = d.Close(); e != nil {
		t.Fatal(e)
	}
	if d, e = NewDaemon(p); e != nil {
		t.Fatal(e)
	}
}
func TestReloadIsValidatedAndRetiresChangedCondition(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	d, e := NewDaemon(p)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	s := d.snapshot.Checks["app/shell"]
	s.FailureIncident.Incident = "incident"
	s.Last = checksandalerts.Result("failed", "bad")
	if e = d.record(nil); e != nil {
		t.Fatal(e)
	}
	testhelpers.WriteConfigText(p, "check_groups: {bad: {typo: 3}}")
	if _, e = d.reload(); e == nil {
		t.Fatal("invalid reload succeeded")
	}
	if d.snapshot.Checks["app/shell"].FailureIncident.Incident != "incident" {
		t.Fatal("invalid reload changed state")
	}
	writeConfig(t, p, c)
	if _, e = d.reload(); e != nil {
		t.Fatal(e)
	}
	if d.snapshot.Checks["app/shell"].FailureIncident.Incident != "incident" {
		t.Fatal("unchanged reload lost state")
	}
	c.CheckGroups["app"] = checksandalerts.CheckGroup{Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "false"}}}
	writeConfig(t, p, c)
	if _, e = d.reload(); e != nil {
		t.Fatal(e)
	}
	if d.snapshot.Checks["app/shell"].FailureIncident.Incident != "" {
		t.Fatal("changed check retained incident")
	}
	events, e := d.store.Events(10)
	if e != nil || len(events) != 1 || events[0].Type != "closed_without_recovery" {
		t.Fatal(events, e)
	}
}

func TestInFlightObservationAcrossControlChanges(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"unchanged reload", "unrelated reload", "changed check", "removed check", "start maintenance", "end maintenance", "maintenance expiry"} {
		t.Run(action, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				select {
				case <-release:
					w.WriteHeader(200)
				case <-r.Context().Done():
				}
			}))
			defer node.Close()
			path, c := daemonConfig(t)
			// The run's time limit is far longer than the test's control
			// change takes, also on a busy machine: the server answers when
			// the test lets it.
			c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {Timing: checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "1m", AlertAfterFailingFor: "2s", RecoveredAfterHealthyFor: "1s"}, Check: checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: node.URL}}}}
			writeConfig(t, path, c)
			d, err := NewDaemon(path)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer func() { cancel(); d.workers.Wait() }()
			now := time.Now()
			st := d.snapshot.Checks["app/http"]
			st.Observe(checksandalerts.Observation{Status: "failed", At: now.Add(-2 * time.Second)}, st.Timing, false)
			st.Observe(checksandalerts.Observation{Status: "failed", At: now.Add(-time.Second)}, st.Timing, false)
			d.checks["app/http"].scheduled = now.Add(-time.Second)
			switch action {
			case "end maintenance":
				d.snapshot.Maintenance["app"] = checksandalerts.Window{Until: now.Add(time.Hour)}
			case "maintenance expiry":
				d.snapshot.Maintenance["app"] = checksandalerts.Window{Until: now.Add(500 * time.Millisecond)}
			}
			d.schedule(ctx, now)
			select {
			case <-entered:
			case <-time.After(testhelpers.LongestWaitForAnEvent):
				t.Fatal("scheduled observation did not start")
			}
			d.mu.Lock()
			next := d.checks["app/http"].next
			d.mu.Unlock()
			switch action {
			case "unrelated reload", "removed check":
				c.CheckGroups["other"] = checksandalerts.CheckGroup{Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true"}}}
				if action == "removed check" {
					delete(c.CheckGroups, "app")
				}
			case "changed check":
				c.CheckGroups["app"].Inputs.(*checksandalerts.HTTP).URL += "/changed"
			}
			if action == "maintenance expiry" {
				d.schedule(ctx, now.Add(600*time.Millisecond))
			} else if action == "start maintenance" || action == "end maintenance" {
				duration := "1h"
				if action == "end maintenance" {
					duration = "off"
				}
				w := httptest.NewRecorder()
				d.routes().ServeHTTP(w, httptest.NewRequest("POST", maintenanceURL("app", duration), bytes.NewBufferString(maintenanceBody(duration))))
				if w.Code != http.StatusOK {
					t.Fatal(w.Code, w.Body.String())
				}
			} else {
				writeConfig(t, path, c)
				if _, err := d.reload(); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			d.workers.Wait()
			st = d.snapshot.Checks["app/http"]
			switch action {
			case "unchanged reload", "unrelated reload":
				// The accepted healthy result starts the healthy count. The
				// failing time that began two seconds ago continues until the
				// check has been healthy for recovered_after_healthy_for.
				if st.Last.Status != "ok" || st.HealthySince.IsZero() || !st.FailingSince.Equal(now.Add(-2*time.Second)) || st.FailureIncident.Incident != "" || !d.checks["app/http"].next.Equal(next) {
					t.Fatalf("reload lost a valid result, the failing time, or the schedule: %+v", st)
				}
			case "removed check":
				if st != nil {
					t.Fatal("removed observation resurrected its check")
				}
			default:
				if !d.checks["app/http"].needsFresh() || st.Last.Status == "ok" || !st.FailingSince.IsZero() {
					t.Fatalf("accepted a result from before the control change: %+v", st)
				}
			}
		})
	}
}

// Reload can retire a scheduled observation before its goroutine takes d.mu.
func TestObservationInvalidatedBeforeStart(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"removed", "replaced", "new epoch"} {
		t.Run(change, func(t *testing.T) {
			var executions atomic.Int32
			node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				executions.Add(1)
			}))
			defer node.Close()
			path, c := daemonConfig(t)
			c.CheckGroups["app"] = checksandalerts.CheckGroup{Check: checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: node.URL}}}
			writeConfig(t, path, c)
			d, err := NewDaemon(path)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			// Reserve exactly as schedule does, but start observe only after
			// the control change, making the interleaving deterministic.
			rc := d.checks["app/http"]
			ticket := rc.epoch
			rc.memory = &checksandalerts.CheckMemory{}
			rc.running = true
			d.running++
			d.workers.Add(1)
			switch change {
			case "removed":
				delete(c.CheckGroups, "app")
				c.CheckGroups["other"] = checksandalerts.CheckGroup{Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true"}}}
			case "replaced":
				c.CheckGroups["app"].Inputs.(*checksandalerts.HTTP).URL += "/replacement"
			}
			writeConfig(t, path, c)
			if _, err := d.reload(); err != nil {
				t.Fatal(err)
			}
			if change == "new epoch" {
				d.mu.Lock()
				d.requireFresh("app", time.Now(), false)
				d.mu.Unlock()
			}
			if d.checks["app/http"] == rc && rc.epoch == ticket {
				t.Fatal("control change did not invalidate the observation")
			}
			before := d.snapshot.Clone()
			d.observe(context.Background(), "app/http", rc, ticket)
			d.workers.Wait()
			if rc.running || d.running != 0 {
				t.Fatalf("observation leaked running bookkeeping: check=%v daemon=%d", rc.running, d.running)
			}
			if got := executions.Load(); got != 0 {
				t.Fatalf("invalidated observation executed %d times", got)
			}
			if !reflect.DeepEqual(d.snapshot, before) {
				t.Fatal("invalidated observation changed the snapshot")
			}
			if current := d.checks["app/http"]; current != nil && !current.needsFresh() {
				t.Fatal("invalidated observation marked the current check fresh")
			}
		})
	}
}

// A scheduled goroutine may start while unchanged reload activation owns
// d.mu. Its execution arguments must share that lock with setDefinitions.
func TestUnchangedReloadSynchronizesExecutionArguments(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer node.Close()
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {Check: checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: node.URL}}}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.mu.Lock()
	rc := d.checks["app/http"]
	rc.memory = &checksandalerts.CheckMemory{}
	rc.running = true
	d.running++
	d.workers.Add(1)
	go d.observe(context.Background(), "app/http", rc, rc.epoch)
	// Give the scheduled goroutine time to reach its first lock while
	// reload still owns it. The race detector checks the argument reads.
	time.Sleep(20 * time.Millisecond)
	d.setDefinitions(d.definitions(d.config))
	d.mu.Unlock()
	d.workers.Wait()
	if d.snapshot.Checks["app/http"].Last.Status != "ok" {
		t.Fatal("unchanged reload discarded its scheduled observation")
	}
}

func TestShortIntervalsConfirmFailureAndRecovery(t *testing.T) {
	t.Parallel()
	for _, every := range []checksandalerts.Duration{"100ms", "120ms"} {
		t.Run(string(every), func(t *testing.T) {
			t.Parallel()
			path, c := daemonConfig(t)
			// Keep the Unix socket below macOS's path-length limit.
			c.Socket = filepath.Join(os.TempDir(), "mmwatchdog-"+incidents.ID()+".sock")
			c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {Timing: checksandalerts.Timing{CheckEvery: every, TimeLimitPerRun: "50ms", AlertAfterFailingFor: "1s", RecoveredAfterHealthyFor: "1s"}, Check: checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: "http://127.0.0.1:9/"}}}}
			writeConfig(t, path, c)
			d, err := NewDaemon(path)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			// The check answers at once, without a server, so that each run
			// ends far within its time limit also on a busy machine.
			var failing atomic.Bool
			failing.Store(true)
			d.checks["app/http"].def.Check.Inputs = runThatAnswersAtOnce{&failing}
			// A late start of a run on a busy machine ends the failing or
			// healthy time, and confirmation starts over: the test waits
			// for both confirmations as long as that takes.
			ctx, cancel := context.WithTimeout(context.Background(), testhelpers.LongestWaitForAnEvent)
			done := make(chan error, 1)
			go func() { done <- d.Run(ctx) }()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			failed := false
			for ctx.Err() == nil {
				d.mu.Lock()
				confirmed := d.snapshot.Checks["app/http"].FailureIncident.Incident != ""
				d.mu.Unlock()
				if !failed && confirmed {
					failed = true
					failing.Store(false)
				} else if failed && !confirmed {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatalf("failure and recovery did not both confirm; failure confirmed: %v", failed)
		})
	}
}

// After a fix, mmwatchdog check-now only looks: the check's alert stays open
// until its next scheduled run, an hour later for an hourly check. With
// --count-as-scheduled-run the daemon runs the check's scheduled run now, so
// the alert closes at once, or waits for the check's
// recovered_after_healthy_for, and the next run comes an hour later.
func TestCheckNowCountsAsTheScheduledRunWhenAsked(t *testing.T) {
	t.Parallel()
	for recoveredAfter, want := range map[checksandalerts.Duration]string{"0s": "ok", "1h": "confirming_recovery"} {
		t.Run(string(recoveredAfter), func(t *testing.T) {
			t.Parallel()
			path, c := daemonConfig(t)
			// Keep the Unix socket below macOS's path-length limit.
			c.Socket = filepath.Join(os.TempDir(), "mmwatchdog-"+incidents.ID()+".sock")
			c.CheckGroups = map[string]checksandalerts.CheckGroup{"drill": {Timing: checksandalerts.Timing{CheckEvery: "1h", TimeLimitPerRun: "5s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: recoveredAfter},
				Check: checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: "http://127.0.0.1:9/"}, AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}}}
			writeConfig(t, path, c)
			d, err := NewDaemon(path)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			var failing atomic.Bool
			failing.Store(true)
			d.checks["drill/http"].def.Check.Inputs = runThatAnswersAtOnce{&failing}
			ctx, cancel := context.WithTimeout(context.Background(), testhelpers.LongestWaitForAnEvent)
			done := make(chan error, 1)
			go func() { done <- d.Run(ctx) }()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			state := func() (string, string) {
				d.mu.Lock()
				defer d.mu.Unlock()
				st := d.snapshot.Checks["drill/http"]
				return st.Summary(d.now(), false), st.FailureIncident.Incident
			}
			testhelpers.WaitFor(t, "the first run's alert", func() bool { _, incident := state(); return incident != "" })
			failing.Store(false)
			if obs, err := d.checkNowGroup(ctx, "drill"); err != nil || obs["http"].Status != "ok" {
				t.Fatal(obs, err)
			}
			if got, _ := state(); got != "failed" {
				t.Fatalf("a look with check-now changed the state to %s", got)
			}
			// mmwatchdog check-now --count-as-scheduled-run, through the API.
			asked := time.Now()
			client := http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
			}}}
			request, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/check-now/drill?count_as_scheduled_run=true", nil)
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var obs map[string]checksandalerts.Observation
			if err := json.NewDecoder(response.Body).Decode(&obs); err != nil || response.StatusCode != 200 || obs["http"].Status != "ok" || obs["http"].At.Before(asked) {
				t.Fatal(response.StatusCode, obs, err)
			}
			if got, incident := state(); got != want || (incident == "") != (want == "ok") {
				t.Fatalf("after the counted run: %s, incident %q", got, incident)
			}
			d.mu.Lock()
			next := d.checks["drill/http"].next
			d.mu.Unlock()
			if next.Sub(asked) < 59*time.Minute {
				t.Fatalf("the next scheduled run comes at %s, not an hour after the counted run", next)
			}
			if _, err := d.runScheduledChecksNow(ctx, "nothing"); err == nil || err.Error() != `unknown check group name "nothing"` {
				t.Fatal(err)
			}
		})
	}
}

func TestLateScheduleKeepsFailingTime(t *testing.T) {
	t.Parallel()
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer node.Close()
	path, c := daemonConfig(t)
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {Timing: checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "5s", AlertAfterFailingFor: "1m", RecoveredAfterHealthyFor: "1m"}, Check: checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: node.URL}}}}
	writeConfig(t, path, c)
	d, err := NewDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Now()
	before := now.Add(-2 * time.Minute)
	st := d.snapshot.Checks["app/http"]
	st.Observe(checksandalerts.Observation{Status: "failed", At: before}, st.Timing, false)
	d.checks["app/http"].scheduled = before
	d.checks["app/http"].next = before.Add(st.Timing.CheckEvery.Value())
	d.schedule(context.Background(), now)
	d.workers.Wait()
	if st.Last.Status != "failed" || st.FailureIncident.Incident == "" || !st.FailingSince.Equal(before) {
		t.Fatalf("late observations lost the failing time: %+v", st)
	}
}

func TestHeartbeatDurabilityAndAuthentication(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	dir := filepath.Dir(p)
	cert, key := testhelpers.TestCertificate(t, dir)
	tokenPath := filepath.Join(dir, "token")
	os.WriteFile(tokenPath, []byte("test-heartbeat-token"), 0600)
	c.HeartbeatReceiver = &config.HeartbeatReceiver{ListenOnAddress: "127.0.0.1:0", TLSCertificateFile: cert, TLSPrivateKeyFile: key}
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"backup": {Heartbeat: &checksandalerts.Heartbeat{MaxTimeBetweenHeartbeats: "1h", ExpectedBearerTokenFile: tokenPath}}}
	writeConfig(t, p, c)
	d, e := NewDaemon(p)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	before := *d.snapshot.Checks["backup/heartbeat"]
	if before.Summary(time.Now(), d.checks["backup/heartbeat"].needsFresh()) != "waiting_for_first_heartbeat" || before.FailureIncident.Incident != "" {
		t.Fatal("first heartbeat did not receive its grace period", before)
	}
	if _, err := d.reload(); err != nil {
		t.Fatal(err)
	}
	restored, err := d.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	waiting := restored.Checks["backup/heartbeat"]
	if waiting.Summary(time.Now(), false) != "waiting_for_first_heartbeat" || !waiting.Deadline.Equal(before.Deadline) {
		t.Fatal("activation or persistence changed heartbeat grace", waiting)
	}
	post := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "https://localhost/v1/heartbeats/backup", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		d.heartbeat(w, r)
		return w
	}
	if w := post("wrong"); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if !d.snapshot.Checks["backup/heartbeat"].Deadline.Equal(before.Deadline) {
		t.Fatal("unauthorized receipt changed deadline")
	}
	if _, e = d.store.DB.Exec("CREATE TRIGGER reject_snapshot BEFORE UPDATE ON snapshot BEGIN SELECT RAISE(FAIL, 'fixture storage failure'); END"); e != nil {
		t.Fatal(e)
	}
	if w := post("test-heartbeat-token"); w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
	if !d.snapshot.Checks["backup/heartbeat"].Receipt.IsZero() {
		t.Fatal("acknowledged unpersisted receipt")
	}
	d.store.DB.Exec("DROP TRIGGER reject_snapshot")
	if w := post("test-heartbeat-token"); w.Code != 204 {
		t.Fatal(w.Code)
	}
	loaded, e := d.store.Load()
	if e != nil || loaded.Checks["backup/heartbeat"].Receipt.IsZero() {
		t.Fatal(loaded, e)
	}
}
func TestStatusSummarizesSelectedCheck(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	c.CheckGroups["other"] = checksandalerts.CheckGroup{Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true"}}}
	writeConfig(t, p, c)
	d, e := NewDaemon(p)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	d.snapshot.Checks["app/shell"].Last = checksandalerts.Observation{Status: "ok", At: time.Now()}
	d.checks["app/shell"].fresh = true
	status := func(query string) (int, map[string]incidents.CheckStatus, incidents.StatusSummary) {
		t.Helper()
		w := httptest.NewRecorder()
		d.routes().ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/status"+query, nil))
		var s struct {
			Checks  map[string]incidents.CheckStatus
			Summary incidents.StatusSummary
		}
		if w.Code == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
				t.Fatal(err)
			}
		}
		return w.Code, s.Checks, s.Summary
	}
	// Other has not been observed, so the whole daemon has unknown health.
	code, checks, summary := status("")
	if code != 200 || len(checks) != 2 || summary.ExitCode != 3 || len(summary.Attention) != 1 || summary.Attention[0].Check != "other/shell" {
		t.Fatal(code, checks, summary)
	}
	code, checks, summary = status("?check_group=app")
	if code != 200 || len(checks) != 1 || summary.ExitCode != 0 || len(summary.Attention) != 0 {
		t.Fatal(code, checks, summary)
	}
	if code, _, _ = status("?check_group=missing"); code != http.StatusNotFound {
		t.Fatal(code)
	}
}

func TestMaintenanceWaitsForFreshObservation(t *testing.T) {
	t.Parallel()
	p, _ := daemonConfig(t)
	d, e := NewDaemon(p)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	s := d.snapshot.Checks["app/shell"]
	s.Last = checksandalerts.Observation{Status: "ok", At: time.Now()}
	rc := d.checks["app/shell"]
	rc.fresh = true
	d.snapshot.Maintenance["app"] = checksandalerts.Window{Until: time.Now().Add(time.Hour)}
	end := func() {
		t.Helper()
		w := httptest.NewRecorder()
		d.routes().ServeHTTP(w, httptest.NewRequest("POST", "http://localhost/maintenance/off/app", bytes.NewBufferString(`{}`)))
		if w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	end()
	s = d.snapshot.Checks["app/shell"]
	if !rc.needsFresh() || s.Summary(time.Now(), rc.needsFresh()) != "unknown" {
		t.Fatal(s)
	}
	// Ending a window that already ended, as a deploy script may after
	// expiry, changes nothing and sends no second alert.
	rc.fresh = true
	end()
	events, err := d.store.Events(10)
	if err != nil || !rc.fresh || len(events) != 1 || events[0].Type != "maintenance_off" {
		t.Fatal("ending absent maintenance changed state or recorded another event", events, err)
	}
}

// Receipts and deadlines are durable heartbeat evidence; maintenance
// boundaries do not make a received heartbeat unknown.
func TestHeartbeatStaysKnownAcrossMaintenance(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	dir := filepath.Dir(p)
	cert, key := testhelpers.TestCertificate(t, dir)
	tokenPath := filepath.Join(dir, "token")
	os.WriteFile(tokenPath, []byte("test-heartbeat-token"), 0600)
	c.HeartbeatReceiver = &config.HeartbeatReceiver{ListenOnAddress: "127.0.0.1:0", TLSCertificateFile: cert, TLSPrivateKeyFile: key}
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"backup": {Heartbeat: &checksandalerts.Heartbeat{MaxTimeBetweenHeartbeats: "1h", ExpectedBearerTokenFile: tokenPath}}}
	writeConfig(t, p, c)
	d, e := NewDaemon(p)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	r := httptest.NewRequest("POST", "https://localhost/v1/heartbeats/backup", nil)
	r.Header.Set("Authorization", "Bearer test-heartbeat-token")
	w := httptest.NewRecorder()
	d.heartbeat(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatal(w.Code)
	}
	status := func() incidents.CheckStatus {
		t.Helper()
		w := httptest.NewRecorder()
		d.routes().ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/status", nil))
		var s struct {
			Checks map[string]incidents.CheckStatus
		}
		if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		return s.Checks["backup/heartbeat"]
	}
	for _, duration := range []string{"10m", "off"} {
		w = httptest.NewRecorder()
		d.routes().ServeHTTP(w, httptest.NewRequest("POST", maintenanceURL("backup", duration), bytes.NewBufferString(maintenanceBody(duration))))
		if w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
		d.schedule(context.Background(), time.Now().UTC())
		if s := status(); s.State != "ok" || s.NeedsFresh {
			t.Fatalf("maintenance %s made a received heartbeat %s (needs_fresh_observation=%v)", duration, s.State, s.NeedsFresh)
		}
	}
}

// Daemon start keeps confirmed incidents but ends unfinished confirmation.
func TestDaemonRestartInterruptsConfirmation(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {Timing: checksandalerts.Timing{CheckEvery: "1s", TimeLimitPerRun: "500ms", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "10s"}, Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "false"}}}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	st := d.snapshot.Checks["app/shell"]
	events := st.Observe(checksandalerts.Observation{Status: "failed", At: now}, st.Timing, false)
	events = append(events, st.Observe(checksandalerts.Observation{Status: "ok", At: now.Add(time.Second)}, st.Timing, false)...)
	id := st.FailureIncident.Incident
	if err := d.record(events); err != nil || id == "" || st.HealthySince.IsZero() {
		t.Fatal(st, err)
	}
	d.Close()
	d, err = NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	st = d.snapshot.Checks["app/shell"]
	if st.FailureIncident.Incident != id || !st.HealthySince.IsZero() || !d.checks["app/shell"].needsFresh() {
		t.Fatalf("restart manufactured recovery progress or lost the incident: %+v", st)
	}
	events, err = d.store.Events(10)
	if err != nil || len(events) != 1 {
		t.Fatal("restart produced another transition", events, err)
	}
}

func TestControlCommandsRequireDurableCommit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, path, body, event string
		inMaintenance           bool
	}{
		{"start maintenance", "/maintenance/on/app", `{"for":"1h"}`, "maintenance_on", false},
		{"extend maintenance", "/maintenance/on/app", `{"for":"1h"}`, "maintenance_on", true},
		{"end maintenance", "/maintenance/off/app", `{}`, "maintenance_off", true},
		{"start whole-node maintenance", "/maintenance/on/every_check_group", `{"for":"1h"}`, "maintenance_on", false},
		{"reset budget", "/reset-recovery-attempts/app", "", "recovery_attempts_reset", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, c := daemonConfig(t)
			c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {Checks: map[string]checksandalerts.NamedCheck{"service": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "app.service", FailWhen: "the_unit_is_not_active"}}}}, RestartSystemdUnit: &checksandalerts.RestartSystemdUnit{SystemdUnit: "app.service", RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"service"}, Attempts: "3", MinTimeBetweenAttempts: "1s", ResetAttemptsAfterHealthyFor: "10m"}}}}
			writeConfig(t, path, c)
			d, err := NewDaemon(path)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			now := time.Now().UTC()
			d.snapshot.RecoveryAttemptsByTarget["app.service"] = &incidents.RecoveryAttempts{Attempts: 3, Last: now.Add(-time.Minute), LastResult: "recovery attempts exhausted"}
			if tc.inMaintenance {
				d.snapshot.Maintenance["app"] = checksandalerts.Window{Until: now.Add(10 * time.Minute)}
			}
			rc := d.checks["app/service"]
			rc.next, rc.fresh = now.Add(time.Minute), true
			epoch := rc.epoch
			if err := d.record(nil); err != nil {
				t.Fatal(err)
			}
			if _, err := d.store.DB.Exec("CREATE TRIGGER reject_snapshot BEFORE UPDATE ON snapshot BEGIN SELECT RAISE(FAIL, 'fixture storage failure'); END"); err != nil {
				t.Fatal(err)
			}
			// An observed transition still needs to survive the rejected command
			// and be committed when storage becomes writable again.
			st := d.snapshot.Checks["app/service"]
			events := st.Observe(checksandalerts.Observation{Status: "failed", At: now, Message: "service down"}, checksandalerts.Timing{CheckEvery: "1s", TimeLimitPerRun: "1s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}, false)
			if err := d.record(events); err == nil {
				t.Fatal("fixture write unexpectedly succeeded")
			}
			state := func() []byte {
				b, err := json.Marshal(map[string]any{"snapshot": d.snapshot, "next": rc.next, "fresh": rc.fresh, "epoch": rc.epoch})
				if err != nil {
					t.Fatal(err)
				}
				return b
			}
			before, pending := state(), append([]checksandalerts.Event{}, d.pending...)
			post := func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				d.routes().ServeHTTP(w, httptest.NewRequest("POST", "http://localhost"+tc.path, bytes.NewBufferString(tc.body)))
				return w
			}
			if w := post(); w.Code != http.StatusInternalServerError {
				t.Fatal(w.Code, w.Body.String())
			}
			if !bytes.Equal(before, state()) || !reflect.DeepEqual(pending, d.pending) {
				t.Fatal("rejected command changed live state, scheduling, or pending transitions")
			}
			if d.storage[stateCommit] == "" {
				t.Fatal("storage failure is not visible")
			}
			if _, err := d.store.DB.Exec("DROP TRIGGER reject_snapshot"); err != nil {
				t.Fatal(err)
			}
			if err := d.record(nil); err != nil {
				t.Fatal(err)
			}
			events, err = d.store.Events(10)
			if err != nil || len(events) != 1 || events[0].ID != pending[0].ID {
				t.Fatal("storage recovery lost the observation or committed a rejected command", events, err)
			}
			if w := post(); w.Code != http.StatusOK {
				t.Fatal(w.Code, w.Body.String())
			}
			loaded, err := d.store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if tc.event == "recovery_attempts_reset" {
				if d.snapshot.RecoveryAttemptsByTarget["app.service"].Attempts != 0 || loaded.RecoveryAttemptsByTarget["app.service"].Attempts != 0 {
					t.Fatal("successful reset was not applied and persisted")
				}
			} else {
				window := tc.path[strings.LastIndex(tc.path, "/")+1:]
				until := d.snapshot.Maintenance[window].Until
				if !until.Equal(loaded.Maintenance[window].Until) || (tc.body == `{}` && !until.IsZero()) || (tc.body != `{}` && !until.After(now.Add(30*time.Minute))) {
					t.Fatal("successful maintenance change was not applied and persisted", until)
				}
				if !rc.needsFresh() || rc.epoch == epoch || !rc.next.After(now) || !rc.next.Before(now.Add(time.Minute)) {
					t.Fatal("successful maintenance change did not require a fresh observation")
				}
			}
			events, err = d.store.Events(10)
			if err != nil || len(events) != 2 || events[0].Type != tc.event || d.storage[stateCommit] != "" || len(d.pending) != 0 {
				t.Fatal("successful command did not commit exactly once", events, err)
			}
		})
	}
}

func TestSchedulerDoesNotStarveLastCondition(t *testing.T) {
	t.Parallel()
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer node.Close()
	p, c := daemonConfig(t)
	c.CheckGroups = map[string]checksandalerts.CheckGroup{}
	for _, name := range []string{"a", "b", "c"} {
		c.CheckGroups[name] = checksandalerts.CheckGroup{Check: checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: node.URL, ExpectedHTTPStatusCodes: []int{200}}}}
	}
	writeConfig(t, p, c)
	d, e := NewDaemon(p)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	d.config.MaxConcurrentChecks = 1
	// Each check answers at once, without a server, so that a run ends far
	// within its time limit also on a busy machine.
	for _, rc := range d.checks {
		rc.def.Timing = checksandalerts.Timing{CheckEvery: "100ms", TimeLimitPerRun: "80ms", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}
		rc.def.Check.Inputs = runThatAnswersAtOnce{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); d.workers.Wait() }()
	// The scheduler runs until each check has had a run. On a busy machine
	// some runs find no place, and the check misses them; a check that the
	// scheduler starves has none at all.
	ran := map[string]bool{}
	scheduleUntil(t, ctx, d, "a run of each check", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		for key, st := range d.snapshot.Checks {
			if st.Last.Status == "ok" && !st.Last.At.IsZero() {
				ran[key] = true
			}
		}
		return len(ran) == 3
	})
}

func TestDiagnosticAndScheduledExecutionShareGate(t *testing.T) {
	t.Parallel()
	var active, maximum atomic.Int32
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old; old = maximum.Load() {
			if maximum.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer node.Close()
	p, c := daemonConfig(t)
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {Check: checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: node.URL}}}}
	writeConfig(t, p, c)
	d, e := NewDaemon(p)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		d.execute(context.Background(), "app/http", d.checks["app/http"].def, &checksandalerts.CheckMemory{}, &checksandalerts.ModuleState{})
	}()
	go func() {
		defer wg.Done()
		if _, e := d.checkNowGroup(context.Background(), "app"); e != nil {
			t.Error(e)
		}
	}()
	wg.Wait()
	if maximum.Load() != 1 {
		t.Fatal("overlapping executions", maximum.Load())
	}
}

func TestReloadRefusesExposedCredential(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	token := filepath.Join(p, "webhook-token")
	os.WriteFile(token, []byte("x"), 0600)
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:9/hook", BearerTokenFile: token}, TimeLimitPerDeliveryAttempt: "5s"}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	os.Chmod(token, 0640)
	if _, err := d.reload(); err == nil || !strings.Contains(err.Error(), "secret_readable_by_group: true") {
		t.Fatal(err)
	}
	destination := c.Alerts.Destinations["ops"]
	destination.SecretReadableByGroup = true
	c.Alerts.Destinations["ops"] = destination
	writeConfig(t, p, c)
	if _, err := d.reload(); err != nil {
		t.Fatal(err)
	}
}

// runThatWaitsForTheKernel is the module of a check of a test whose run
// does not mind its time limit: it ends when the test lets it end, as a run
// does that waits for the kernel's answer about a file on a filesystem of
// another machine that is gone.
type runThatWaitsForTheKernel struct {
	// started gets, for each run, the memory and the stored state that the
	// run found.
	started chan [2]string
	// theKernelAnswers is closed when the test lets every run end.
	theKernelAnswers chan struct{}
}

func newRunThatWaitsForTheKernel() *runThatWaitsForTheKernel {
	return &runThatWaitsForTheKernel{started: make(chan [2]string, 1000), theKernelAnswers: make(chan struct{})}
}

func (w *runThatWaitsForTheKernel) Validate(checksandalerts.InputValidation) *checksandalerts.FieldError {
	return nil
}
func (w *runThatWaitsForTheKernel) Run(_ context.Context, r *checksandalerts.CheckRun) checksandalerts.Observation {
	memory, _ := r.Memory.Value.(string)
	w.started <- [2]string{memory, string(r.State.Stored)}
	r.Memory.Value = "the memory of a run"
	r.State.Save("the state of a run")
	<-w.theKernelAnswers
	return checksandalerts.Result("ok", "the kernel answered")
}

// daemonWithARunThatDoesNotEnd starts a daemon with the checks waits/http
// and other/http of a server of the test. The daemon has one place for a
// run. The module whose run waits for the kernel takes the place of the
// module of the first check, which runs every 100 ms with a time limit of
// 80 ms; the tests let MMWatchdog wait 30 ms more (waitingForARunToEnd), or as
// long as they study. The other check runs every second with a time limit
// of a second, far longer than its answer takes also on a busy machine,
// and a check that waits a second for a place misses nothing.
func daemonWithARunThatDoesNotEnd(t *testing.T) (d *Daemon, kernel *runThatWaitsForTheKernel, configDir string, c *config.Config) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	t.Cleanup(server.Close)
	configDir, c = daemonConfig(t)
	c.CheckGroups = map[string]checksandalerts.CheckGroup{
		"waits": {Timing: checksandalerts.Timing{CheckEvery: "100ms", TimeLimitPerRun: "80ms", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}, Check: checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: server.URL, ExpectedHTTPStatusCodes: []int{200}}}},
		"other": {Timing: checksandalerts.Timing{CheckEvery: "1s", TimeLimitPerRun: "1s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}, Check: checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: server.URL, ExpectedHTTPStatusCodes: []int{200}}}},
	}
	writeConfig(t, configDir, c)
	d, e := NewDaemon(configDir)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	d.slots = make(chan struct{}, 1)
	kernel = newRunThatWaitsForTheKernel()
	d.checks["waits/http"].def.Check.Inputs = kernel
	return d, kernel, configDir, c
}

// scheduleUntil runs the daemon's scheduler, ten times as often as MMWatchdog's
// own, until done reports that what the test waits for happened.
func scheduleUntil(t *testing.T, ctx context.Context, d *Daemon, what string, done func() bool) {
	t.Helper()
	for end := time.Now().Add(testhelpers.LongestWaitForAnEvent); !done(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the scheduler ran for a minute, and still not:", what)
		}
		d.schedule(ctx, time.Now())
	}
}

// startARun runs the daemon's scheduler until it starts a run of the check
// key.
func startARun(t *testing.T, ctx context.Context, d *Daemon, key string) {
	t.Helper()
	scheduled := func() time.Time {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.checks[key].scheduled
	}
	before := scheduled()
	scheduleUntil(t, ctx, d, "a run of "+key+" started", func() bool { return !scheduled().Equal(before) })
}

// runOnce starts a run of the check key and waits, without further ticks
// of the scheduler, until the observation of the run is in. A tick while
// the run gets going would find the check overdue on a busy machine, and
// record that it found no place.
func runOnce(t *testing.T, ctx context.Context, d *Daemon, key string) {
	t.Helper()
	startARun(t, ctx, d, key)
	testhelpers.WaitFor(t, "the observation of a run of "+key, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return !d.checks[key].running
	})
}

// aRunIsInProgress reports whether a run of the check key is under way, also
// one that MMWatchdog stopped waiting for.
func aRunIsInProgress(d *Daemon, key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.runsInProgress[key] != nil
}

// waitingForARunToEnd is ctx under which MMWatchdog waits for a run for wait once
// the run's time limit has passed, in place of
// timeForARunToEndAfterItsTimeLimit.
func waitingForARunToEnd(ctx context.Context, wait time.Duration) context.Context {
	return context.WithValue(ctx, testTimeForARunToEndAfterItsTimeLimitKey{}, wait)
}

// nextRun returns what the next run of the module found, and fails the test
// when no further run started.
func (w *runThatWaitsForTheKernel) nextRun(t *testing.T) [2]string {
	t.Helper()
	select {
	case found := <-w.started:
		return found
	case <-time.After(testhelpers.LongestWaitForAnEvent):
		t.Fatal("no further run of the check started")
		return [2]string{}
	}
}

// lastObservation is the latest observation of a check of a daemon.
func lastObservation(d *Daemon, key string) checksandalerts.Observation {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snapshot.Checks[key].Last
}

// The kernel can hold a question about a file without end, and nothing in
// MMWatchdog can take the question back. Such a run gives its place among the
// concurrent checks back, so the other checks of the node go on, and the
// check says in plain words what happened.
func TestARunThatDoesNotEndGivesItsPlaceBack(t *testing.T) {
	t.Parallel()
	d, kernel, _, _ := daemonWithARunThatDoesNotEnd(t)
	ctx, cancel := context.WithCancel(waitingForARunToEnd(context.Background(), 30*time.Millisecond))
	defer func() { cancel(); d.workers.Wait() }()
	// The run that does not end has the one place until MMWatchdog stops waiting
	// for it. The next run of the check is one that MMWatchdog does not start.
	// Then the other check gets the place: it would wait for ever had the
	// run that does not end kept it.
	runOnce(t, ctx, d, "waits/http")
	runOnce(t, ctx, d, "waits/http")
	runOnce(t, ctx, d, "other/http")
	if started := len(kernel.started); started != 1 {
		t.Fatalf("MMWatchdog started %d runs of a check whose run does not end, and must start one", started)
	}
	waits, other := lastObservation(d, "waits/http"), lastObservation(d, "other/http")
	if waits.Status != "unknown" || !regexp.MustCompile(`^the check run that started at \d\d:\d\d UTC has not finished\. MMWatchdog will start no new run of the check until the earlier run finishes$`).MatchString(waits.Message) || len(waits.Details) != 2 || !strings.Contains(waits.Details[1], "a file on a filesystem of another machine that does not answer") {
		t.Fatalf("the check whose run does not end: %+v", waits)
	}
	if other.Status != "ok" {
		t.Fatalf("the other check of the node got no place: %+v", other)
	}
	// The first of the observations is that of the run itself, and both of
	// them are in the event history as they came.
	events, e := d.store.Events(100)
	if e != nil {
		t.Fatal(e)
	}
	messages := []string{}
	for _, event := range events {
		if event.CheckGroup == "waits" {
			messages = append(messages, event.Type+": "+event.Message)
		}
		if strings.Contains(event.Message, "execution capacity unavailable") {
			t.Fatalf("a check found no place although the run that does not end gave its place back: %+v", event)
		}
	}
	if len(messages) == 0 || !strings.Contains(messages[len(messages)-1], "the check run did not finish within the time limit of 80ms. MMWatchdog stopped waiting for the run to finish") {
		t.Fatalf("the first event of the check does not say that its run did not end: %q", messages)
	}

	// The kernel answers at last. MMWatchdog drops what that run brings: the next
	// run finds an empty memory and no stored state, and counts. From here
	// on each run ends at once, and MMWatchdog waits for it as long as it takes.
	close(kernel.theKernelAnswers)
	testhelpers.WaitFor(t, "the end of the run that the kernel held", func() bool { return !aRunIsInProgress(d, "waits/http") })
	answered, stopAnswered := context.WithCancel(waitingForARunToEnd(context.Background(), testhelpers.LongestWaitForAnEvent))
	defer func() { stopAnswered(); d.workers.Wait() }()
	runOnce(t, answered, d, "waits/http")
	if again := lastObservation(d, "waits/http"); again.Status != "ok" || again.Message != "the kernel answered" {
		t.Fatalf("the check after its run ended: %+v", again)
	}
	runOnce(t, answered, d, "waits/http")
	kernel.nextRun(t)
	if second := kernel.nextRun(t); second != [2]string{"", ""} {
		t.Fatalf("the run after the one that MMWatchdog dropped found memory and state %q, and must find none", second)
	}
	if third := kernel.nextRun(t); third != [2]string{"the memory of a run", `"the state of a run"`} {
		t.Fatalf("the run after an ordinary run found memory and state %q", third)
	}
}

// A check can run more often than MMWatchdog waits for one run. Its first
// observation is still the one of the run itself, which says what happened:
// while the run has its place and is under way, the check has missed
// nothing for lack of a place.
func TestTheFirstObservationOfARunThatDoesNotEndSaysSo(t *testing.T) {
	t.Parallel()
	d, kernel, _, _ := daemonWithARunThatDoesNotEnd(t)
	defer close(kernel.theKernelAnswers)
	ctx, cancel := context.WithCancel(waitingForARunToEnd(context.Background(), 300*time.Millisecond))
	defer func() { cancel(); d.workers.Wait() }()
	// The scheduler ticks again only once the run is under way: a tick
	// while the run gets going would find the check overdue on a busy
	// machine. While the run is under way, it ticks on.
	startARun(t, ctx, d, "waits/http")
	testhelpers.WaitFor(t, "the run that does not end to start", func() bool { return len(kernel.started) == 1 })
	scheduleUntil(t, ctx, d, "an observation of a run that MMWatchdog did not start", func() bool {
		return strings.Contains(lastObservation(d, "waits/http").Message, "has not finished. MMWatchdog will start no new run")
	})
	events, e := d.store.Events(100)
	if e != nil {
		t.Fatal(e)
	}
	messages := []string{}
	for _, event := range events {
		if event.CheckGroup == "waits" {
			messages = append(messages, event.Type+": "+event.Message)
		}
	}
	if len(messages) == 0 || !strings.Contains(messages[len(messages)-1], "the check run did not finish within the time limit of 80ms. MMWatchdog stopped waiting for the run to finish") || strings.Contains(strings.Join(messages, "\n"), "execution capacity unavailable") {
		t.Fatalf("the events of the check whose run does not end: %q", messages)
	}
}

// A stop of MMWatchdog does not wait for a run that does not end.
func TestTheDaemonStopsWhileARunDoesNotEnd(t *testing.T) {
	t.Parallel()
	d, kernel, _, _ := daemonWithARunThatDoesNotEnd(t)
	defer close(kernel.theKernelAnswers)
	ctx, cancel := context.WithCancel(waitingForARunToEnd(context.Background(), 30*time.Millisecond))
	startARun(t, ctx, d, "waits/http")
	kernel.nextRun(t)
	cancel()
	stopped := make(chan struct{})
	go func() { d.workers.Wait(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(testhelpers.LongestWaitForAnEvent):
		t.Fatal("the daemon's stop waits for a run that does not end")
	}
}

// "mmwatchdog check-now" of a check whose run has not ended starts no second run,
// and answers at once. A run of "mmwatchdog check-now" that does not end is no
// other: the scheduled runs of the check then wait for it.
func TestCheckNowAndARunThatDoesNotEnd(t *testing.T) {
	t.Parallel()
	d, kernel, _, _ := daemonWithARunThatDoesNotEnd(t)
	ctx, cancel := context.WithCancel(waitingForARunToEnd(context.Background(), 30*time.Millisecond))
	defer func() { cancel(); d.workers.Wait() }()
	asked := time.Now()
	observations, e := d.checkNowGroup(ctx, "waits")
	if e != nil {
		t.Fatal(e)
	}
	// MMWatchdog waits for the run 110 ms, its time limit and the 30 ms of the
	// test; one that waited the 5 seconds of a run outside a test would
	// take far longer than the bound.
	o := observations["http"]
	if o.Status != "unknown" || o.Message != "the check run did not finish within the time limit of 80ms. MMWatchdog stopped waiting for the run to finish" || len(o.Details) != 3 || o.Details[2] != "MMWatchdog will start no new run of the check until the unfinished run ends" || o.At.IsZero() || o.Duration < 110*time.Millisecond || time.Since(asked) > 3*time.Second {
		t.Fatalf("check-now of a check whose run does not end, after %s: %+v", time.Since(asked), o)
	}
	// Only a run that MMWatchdog did not start has this message.
	if observations, e = d.checkNowGroup(ctx, "waits"); e != nil || !strings.Contains(observations["http"].Message, "has not finished. MMWatchdog will start no new run") {
		t.Fatalf("a second check-now: %+v %v", observations, e)
	}
	runOnce(t, ctx, d, "waits/http")
	if scheduled := lastObservation(d, "waits/http"); scheduled.Status != "unknown" || !strings.Contains(scheduled.Message, "has not finished. MMWatchdog will start no new run") {
		t.Fatalf("a scheduled run while the run of check-now has not ended: %+v", scheduled)
	}
	if started := len(kernel.started); started != 1 {
		t.Fatalf("MMWatchdog started %d runs, and must start one", started)
	}
	// The kernel answers. From here on each run ends at once, and MMWatchdog waits
	// for it as long as it takes.
	close(kernel.theKernelAnswers)
	testhelpers.WaitFor(t, "the end of the run that the kernel held", func() bool { return !aRunIsInProgress(d, "waits/http") })
	answered, stopAnswered := context.WithCancel(waitingForARunToEnd(context.Background(), testhelpers.LongestWaitForAnEvent))
	defer func() { stopAnswered(); d.workers.Wait() }()
	runOnce(t, answered, d, "waits/http")
	if scheduled := lastObservation(d, "waits/http"); scheduled.Status != "ok" {
		t.Fatalf("a scheduled run after the run of check-now ended: %+v", scheduled)
	}
}

// runThatEndsAfterItsTimeLimit is the module of a check of a test whose run
// minds its time limit, and needs a moment to end then, as a run does that
// ends a program.
type runThatEndsAfterItsTimeLimit time.Duration

func (w runThatEndsAfterItsTimeLimit) Validate(checksandalerts.InputValidation) *checksandalerts.FieldError {
	return nil
}
func (w runThatEndsAfterItsTimeLimit) Run(ctx context.Context, r *checksandalerts.CheckRun) checksandalerts.Observation {
	<-ctx.Done()
	time.Sleep(time.Duration(w))
	r.State.Save("the state of a run that ended a moment late")
	return checksandalerts.Result("failed", "the program did not finish in time")
}

// runThatAnswersAtOnce is the module of a check of a test whose run ends at
// once, without a server or a program, so that it ends far within its time
// limit also on a busy machine. It fails while failing is set.
type runThatAnswersAtOnce struct{ failing *atomic.Bool }

func (w runThatAnswersAtOnce) Validate(checksandalerts.InputValidation) *checksandalerts.FieldError {
	return nil
}
func (w runThatAnswersAtOnce) Run(context.Context, *checksandalerts.CheckRun) checksandalerts.Observation {
	if w.failing != nil && w.failing.Load() {
		return checksandalerts.Result("failed", "the test lets the check fail")
	}
	return checksandalerts.Result("ok", "the test lets the check pass")
}

// A run that minds its time limit and ends a moment after it counts. MMWatchdog
// waits a minute here once the time limit has passed, far longer than the
// moment that the run takes then, also on a busy machine.
func TestARunThatEndsAMomentAfterItsTimeLimitCounts(t *testing.T) {
	t.Parallel()
	d, _, _, _ := daemonWithARunThatDoesNotEnd(t)
	d.checks["waits/http"].def.Check.Inputs = runThatEndsAfterItsTimeLimit(10 * time.Millisecond)
	def := d.checks["waits/http"].def
	state := &checksandalerts.ModuleState{}
	o := d.execute(waitingForARunToEnd(context.Background(), testhelpers.LongestWaitForAnEvent), "waits/http", def, &checksandalerts.CheckMemory{}, state)
	if o.Status != "failed" || o.Message != "the program did not finish in time" || string(state.Saved) != `"the state of a run that ended a moment late"` {
		t.Fatalf("a run that ended 10 ms after its time limit: %+v, state %s", o, state.Saved)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.runsInProgress) != 0 {
		t.Fatalf("a run that ended is still in progress: %v", d.runsInProgress)
	}
}

// A reload that changes the check makes a new check of it, with a memory of
// its own. Its runs start although a run of the check as it was has not
// ended, and that old run changes nothing of the new check when it ends.
func TestAReloadThatChangesTheCheckStartsItsRunsAgain(t *testing.T) {
	t.Parallel()
	d, kernel, configDir, c := daemonWithARunThatDoesNotEnd(t)
	ctx, cancel := context.WithCancel(waitingForARunToEnd(context.Background(), 30*time.Millisecond))
	defer func() { cancel(); d.workers.Wait() }()
	runOnce(t, ctx, d, "waits/http")
	if o := lastObservation(d, "waits/http"); o.Status != "unknown" {
		t.Fatalf("the check whose run does not end: %+v", o)
	}
	c.CheckGroups["waits"] = checksandalerts.CheckGroup{Timing: checksandalerts.Timing{CheckEvery: "100ms", TimeLimitPerRun: "80ms", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}, Check: checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: c.CheckGroups["other"].Check.Inputs.(*checksandalerts.HTTP).URL + "/changed", ExpectedHTTPStatusCodes: []int{200}}}}
	writeConfig(t, configDir, c)
	if _, e := d.reload(); e != nil {
		t.Fatal(e)
	}
	// The changed check answers at once, and MMWatchdog waits for its run as long
	// as it takes.
	d.mu.Lock()
	d.checks["waits/http"].def.Check.Inputs = runThatAnswersAtOnce{}
	d.mu.Unlock()
	answered, stopAnswered := context.WithCancel(waitingForARunToEnd(context.Background(), testhelpers.LongestWaitForAnEvent))
	defer func() { stopAnswered(); d.workers.Wait() }()
	runOnce(t, answered, d, "waits/http")
	if o := lastObservation(d, "waits/http"); o.Status != "ok" {
		t.Fatalf("the changed check, while the run of the check as it was has not ended: %+v", o)
	}
	// A run of the new check does not end either. The end of the old run
	// then leaves the new one as it is: the test gives the old run a
	// moment to end, and a run that has not ended by then leaves the new
	// check as it is too.
	secondKernel := newRunThatWaitsForTheKernel()
	defer close(secondKernel.theKernelAnswers)
	d.mu.Lock()
	d.checks["waits/http"].def.Check.Inputs = secondKernel
	d.mu.Unlock()
	runOnce(t, ctx, d, "waits/http")
	close(kernel.theKernelAnswers)
	time.Sleep(50 * time.Millisecond)
	runOnce(t, ctx, d, "waits/http")
	if o := lastObservation(d, "waits/http"); o.Status != "unknown" || !strings.Contains(o.Message, "has not finished. MMWatchdog will start no new run") {
		t.Fatalf("the new check, whose run does not end, after the old run ended: %+v", o)
	}
	if started := len(secondKernel.started); started != 1 {
		t.Fatalf("MMWatchdog started %d runs of the new check, and must start one", started)
	}
}

// A reload changes a check while a run of the check as it was waits for
// its turn, behind a run of "mmwatchdog check-now". That old run does not start: it
// would count as the run in progress of the changed check, and would hold
// the changed check up when it does not end.
func TestAReloadWhileARunOfTheOldCheckWaitsForItsTurn(t *testing.T) {
	t.Parallel()
	d, kernel, configDir, c := daemonWithARunThatDoesNotEnd(t)
	defer close(kernel.theKernelAnswers)
	// Only the check whose run does not end runs here.
	d.mu.Lock()
	d.checks["other/http"].next = time.Now().Add(time.Hour)
	d.mu.Unlock()
	ctx, cancel := context.WithCancel(waitingForARunToEnd(context.Background(), 400*time.Millisecond))
	var checkNow sync.WaitGroup
	defer func() { cancel(); d.workers.Wait(); checkNow.Wait() }()
	// The run of check-now does not end, and holds the turn of the check
	// for 480 ms.
	checkNow.Add(1)
	go func() { defer checkNow.Done(); d.checkNowGroup(ctx, "waits") }()
	kernel.nextRun(t)
	// A scheduled run of the check as it is starts and waits for its turn.
	startARun(t, ctx, d, "waits/http")
	testhelpers.WaitFor(t, "the scheduled run to wait for its turn", func() bool { return testhelpers.AGoroutineWaitsInASelect("(*Daemon).execute") })
	// The time limit of the changed check is far longer than its answer
	// takes, also on a busy machine.
	c.CheckGroups["waits"] = checksandalerts.CheckGroup{Timing: checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "1m", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}, Check: checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: c.CheckGroups["other"].Check.Inputs.(*checksandalerts.HTTP).URL + "/changed", ExpectedHTTPStatusCodes: []int{200}}}}
	writeConfig(t, configDir, c)
	if _, e := d.reload(); e != nil {
		t.Fatal(e)
	}
	// The run of the changed check waits for its turn behind the old run,
	// so once it has run, the old run has had its turn too.
	runOnce(t, ctx, d, "waits/http")
	d.workers.Wait()
	if started := len(kernel.started); started != 0 {
		t.Fatalf("%d more runs of the check as it was started after the reload", started)
	}
	if o := lastObservation(d, "waits/http"); o.Status != "ok" {
		t.Fatalf("the changed check: %+v", o)
	}
}

// runThatEndsLate is the module of a check of a test whose run ends by
// itself, long after its time limit, as a run does that asks about a file
// on a network filesystem that gives up after a minute.
type runThatEndsLate time.Duration

func (w runThatEndsLate) Validate(checksandalerts.InputValidation) *checksandalerts.FieldError {
	return nil
}
func (w runThatEndsLate) Run(context.Context, *checksandalerts.CheckRun) checksandalerts.Observation {
	time.Sleep(time.Duration(w))
	return checksandalerts.Result("failed", "the filesystem gave up")
}

// A check whose every run ends long after MMWatchdog stopped waiting for it is
// unknown without a break, so it sends its cannot-check alert after
// alert_after_failing_for. The start of the run after such a run is no late
// start: MMWatchdog ran the check until then.
//
// Each run ends after 900 ms, and MMWatchdog waits 380 ms for it, which is more
// than one and a half intervals. Were the start after such a run late,
// every run would end the unknown time, which would never last the 2
// seconds of alert_after_failing_for. A start that comes late on a busy
// machine ends it too, so the test waits for the alert as long as that
// takes, and not for a fixed time.
func TestACheckWhoseRunsEndLateSendsItsCannotCheckAlert(t *testing.T) {
	t.Parallel()
	d, _, _, _ := daemonWithARunThatDoesNotEnd(t)
	d.mu.Lock()
	d.checks["waits/http"].def.Check.Inputs = runThatEndsLate(900 * time.Millisecond)
	d.checks["waits/http"].def.Timing.AlertAfterFailingFor = "2s"
	d.mu.Unlock()
	ctx, cancel := context.WithCancel(waitingForARunToEnd(context.Background(), 300*time.Millisecond))
	defer func() { cancel(); d.workers.Wait() }()
	opened := func() int {
		events, e := d.store.Events(1000)
		if e != nil {
			t.Fatal(e)
		}
		count := 0
		for _, event := range events {
			if event.CheckGroup == "waits" && event.Type == "cannot_check" {
				count++
			}
		}
		return count
	}
	scheduleUntil(t, ctx, d, "the cannot-check alert", func() bool { return opened() > 0 })
	d.mu.Lock()
	state := *d.snapshot.Checks["waits/http"]
	d.mu.Unlock()
	if count := opened(); count != 1 || state.Last.Status != "unknown" {
		t.Fatalf("%d cannot-check alerts after unknown observations, and one is expected; the check: last %+v, unknown since %s", count, state.Last, state.UnknownSince)
	}
}

// "mmwatchdog check-now --without-daemon" goes on to the next check of the check
// group when a run does not end. MMWatchdog waits a second for a run once its time
// limit has passed: far longer than the second check needs to end then,
// also on a busy machine.
func TestCheckNowWithoutDaemonGoesOnAfterARunThatDoesNotEnd(t *testing.T) {
	t.Parallel()
	kernel := newRunThatWaitsForTheKernel()
	defer close(kernel.theKernelAnswers)
	timing := checksandalerts.Timing{CheckEvery: "100ms", TimeLimitPerRun: "80ms", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}
	c := &config.Config{CheckGroups: map[string]checksandalerts.CheckGroup{"app": {Timing: timing, Checks: map[string]checksandalerts.NamedCheck{
		"a_file_that_the_kernel_does_not_show": {Check: checksandalerts.Check{Inputs: kernel}},
		"works":                                {Check: checksandalerts.Check{Inputs: runThatEndsAfterItsTimeLimit(0)}},
	}}}}
	observations, e := CheckNow(waitingForARunToEnd(context.Background(), time.Second), c, "app")
	if e != nil {
		t.Fatal(e)
	}
	stuck, works := observations["a_file_that_the_kernel_does_not_show"], observations["works"]
	if stuck.Status != "unknown" || stuck.Message != "the check run did not finish within the time limit of 80ms. MMWatchdog stopped waiting for the run to finish" || len(stuck.Details) != 2 {
		t.Fatalf("the check whose run does not end: %+v", stuck)
	}
	if works.Status != "failed" || works.Message != "the program did not finish in time" {
		t.Fatalf("the check after it: %+v", works)
	}
}

func TestStorageFailureKeepsInspectionAndWithholdsRestart(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {Checks: map[string]checksandalerts.NamedCheck{"service": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "app.service", FailWhen: "the_unit_is_not_active"}}}}, RestartSystemdUnit: &checksandalerts.RestartSystemdUnit{SystemdUnit: "app.service", RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"service"}, Attempts: "1", MinTimeBetweenAttempts: "1s", ResetAttemptsAfterHealthyFor: "10s"}}}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	dir := t.TempDir()
	marker := filepath.Join(dir, "restarted")
	testhelpers.WriteStandInProgram(t, filepath.Join(dir, "systemctl"), "#!/bin/sh\nif [ \"$1\" = show ]; then printf 'LoadState=loaded\\nActiveState=failed\\n'; else touch "+testhelpers.ShellQuote(marker)+"; fi\n")
	testhelpers.PutProgramsFirst(t, dir)
	if _, err = d.store.DB.Exec("CREATE TRIGGER reject_snapshot BEFORE UPDATE ON snapshot BEGIN SELECT RAISE(FAIL, 'fixture disk full'); END"); err != nil {
		t.Fatal(err)
	}
	st := d.snapshot.Checks["app/service"]
	ev := st.Observe(checksandalerts.Observation{Status: "failed", At: time.Now(), Message: "service down"}, checksandalerts.Timing{CheckEvery: "1s", TimeLimitPerRun: "1s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}, false)
	d.checks["app/service"].fresh = true
	if err = d.record(ev); err == nil {
		t.Fatal("write unexpectedly succeeded")
	}
	d.mu.Lock()
	d.startRecovery(testhelpers.TestContext(t), time.Now())
	d.mu.Unlock()
	d.workers.Wait()
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("restart without durable budget", err)
	}
	budget := d.snapshot.RecoveryAttemptsByTarget["app.service"]
	if budget.Attempts != 0 || !budget.Last.IsZero() || budget.LastResult != "" {
		t.Fatal("uncommitted reservation changed restart budget", budget)
	}
	for _, event := range d.pending {
		if event.Type == "service_restart_requested" {
			t.Fatal("uncommitted restart left an action event", event)
		}
	}
	w := httptest.NewRecorder()
	d.routes().ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/status", nil))
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("fixture disk full")) || !bytes.Contains(w.Body.Bytes(), []byte("service down")) {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err = d.store.DB.Exec("DROP TRIGGER reject_snapshot"); err != nil {
		t.Fatal(err)
	}
	if err = d.record(nil); err != nil {
		t.Fatal(err)
	}
	events, err := d.store.Events(100)
	if err != nil {
		t.Fatal(err)
	}
	failures := 0
	for _, ev := range events {
		if ev.Type == "failed" {
			failures++
		}
	}
	if failures != 1 || d.storage[stateCommit] != "" {
		t.Fatal(failures, d.storage)
	}
	d.mu.Lock()
	d.startRecovery(testhelpers.TestContext(t), time.Now())
	d.mu.Unlock()
	d.workers.Wait()
	if _, err = os.Stat(marker); err != nil || d.snapshot.RecoveryAttemptsByTarget["app.service"].Attempts != 1 {
		t.Fatal("durable recovery did not execute its available attempt", d.snapshot.RecoveryAttemptsByTarget["app.service"], err)
	}
	// Read errors also leave the in-memory status available.
	d.store.Close()
	w = httptest.NewRecorder()
	d.routes().ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/status", nil))
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("database is closed")) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestStorageFailureInterruptsRecoveryHealth(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"failed", "unknown"} {
		t.Run(state, func(t *testing.T) {
			path, c := daemonConfig(t)
			c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {
				Checks: map[string]checksandalerts.NamedCheck{"service": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "app.service", FailWhen: "the_unit_is_not_active"}}}}, Timing: checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "30s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"},
				RestartSystemdUnit: &checksandalerts.RestartSystemdUnit{SystemdUnit: "app.service", RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"service"}, Attempts: "3", MinTimeBetweenAttempts: "1s", ResetAttemptsAfterHealthyFor: "10m"}},
			}}
			writeConfig(t, path, c)
			d, err := NewDaemon(path)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			dir := t.TempDir()
			statePath := filepath.Join(dir, "state")
			script := "#!/bin/sh\nstate=$(cat " + testhelpers.ShellQuote(statePath) + ")\n[ \"$state\" != unknown ] || exit 1\nprintf 'LoadState=loaded\\nActiveState=%s\\n' \"$state\"\n"
			testhelpers.WriteStandInProgram(t, filepath.Join(dir, "systemctl"), script)
			if err := os.WriteFile(statePath, []byte(state), 0600); err != nil {
				t.Fatal(err)
			}
			testhelpers.PutProgramsFirst(t, dir)
			now := time.Now().UTC()
			d.snapshot.RecoveryAttemptsByTarget["app.service"] = &incidents.RecoveryAttempts{Attempts: 3, Last: now.Add(-time.Hour)}
			st := d.snapshot.Checks["app/service"]
			st.Observe(checksandalerts.Observation{Status: "ok", At: now.Add(-time.Second)}, st.Timing, false)
			// The check has been healthy for just short of the reset period.
			// Had the storage failure not interrupted that health, the healthy
			// observation below would complete the period and reset the
			// budget, however soon after now it comes. The runs take their
			// observation times from the clock, so no other margin of real
			// time decides the result: the time limit of a run is far longer
			// than a stand-in takes on a busy machine.
			st.HealthySince = now.Add(-600*time.Second - 500*time.Millisecond)
			d.checks["app/service"].scheduled = now.Add(-time.Second)
			if err := d.record(nil); err != nil {
				t.Fatal(err)
			}
			if _, err := d.store.DB.Exec("CREATE TRIGGER reject_snapshot BEFORE UPDATE ON snapshot BEGIN SELECT RAISE(FAIL, 'fixture storage failure'); END"); err != nil {
				t.Fatal(err)
			}
			d.schedule(testhelpers.TestContext(t), now)
			d.workers.Wait()
			if st.Last.Status != state || d.storage[stateCommit] == "" {
				t.Fatalf("fixture did not fail: %+v %v", st.Last, d.storage)
			}
			budget := d.snapshot.RecoveryAttemptsByTarget["app.service"]
			if !d.healthySince("app.service").IsZero() || budget.Attempts != 3 {
				t.Fatal("write failure preserved interrupted health or reset the budget", budget)
			}
			if _, err := d.store.DB.Exec("DROP TRIGGER reject_snapshot"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(statePath, []byte("active"), 0600); err != nil {
				t.Fatal(err)
			}
			// Advance the schedule; the execution supplies a fresh observation time.
			d.schedule(testhelpers.TestContext(t), now.Add(time.Minute))
			d.workers.Wait()
			if st.Last.Status != "ok" || !d.healthySince("app.service").Equal(st.Last.At) || budget.Attempts != 3 {
				t.Fatal("first healthy observation must start a new full reset window", st.Last, budget)
			}
			saved, err := d.store.Load()
			if err != nil || saved.RecoveryAttemptsByTarget["app.service"].Attempts != 3 {
				t.Fatal("recovered storage did not preserve consumed attempts", saved.RecoveryAttemptsByTarget, err)
			}
		})
	}
}

func TestDiagnosticRequestUsesExecutionTimeouts(t *testing.T) {
	t.Parallel()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(100 * time.Millisecond):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer target.Close()
	p, c := daemonConfig(t)
	// The time limit of a run is far longer than the target's answer takes,
	// also on a busy machine. The answer takes longer than the write
	// timeout of the daemon's server.
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {
		Timing: checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "1m"},
		Checks: map[string]checksandalerts.NamedCheck{
			"first":  {Check: checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: target.URL}}},
			"second": {Check: checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: target.URL}}},
		},
	}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	node := httptest.NewUnstartedServer(d.routes())
	node.Config.WriteTimeout = 50 * time.Millisecond
	node.Start()
	defer node.Close()
	client := node.Client()
	client.Timeout = testhelpers.LongestWaitForAnEvent
	response, err := client.Post(node.URL+"/check-now/app", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var observations map[string]checksandalerts.Observation
	if err := json.NewDecoder(response.Body).Decode(&observations); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || len(observations) != 2 {
		t.Fatal(response.StatusCode, observations)
	}
	for name, observation := range observations {
		if observation.Status != "ok" {
			t.Fatal(name, observation)
		}
	}
}

func TestRecoveryDoesNotRestartSystemdUnitThatJustBecameActive(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {
		Timing:             checksandalerts.Timing{CheckEvery: "1s", TimeLimitPerRun: "500ms"},
		Checks:             map[string]checksandalerts.NamedCheck{"service": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "app.service", FailWhen: "the_unit_is_not_active"}}}, "health": {Check: checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: "http://127.0.0.1:1/health"}}}},
		RestartSystemdUnit: &checksandalerts.RestartSystemdUnit{SystemdUnit: "app.service", RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"service"}, Attempts: "3", MinTimeBetweenAttempts: "1s", ResetAttemptsAfterHealthyFor: "10m"}},
	}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Now()
	failed := func(key, message string) {
		st := d.snapshot.Checks[key]
		st.FailureIncident.Incident, st.FailingSince = "incident-"+key, now
		st.Last = checksandalerts.Observation{Status: "failed", Message: message, At: now}
		d.checks[key].fresh = true
	}
	failed("app/service", "systemd service is activating")
	if d.recoveryDue("app.service", now, "active") != "" {
		t.Fatal("restarted a now-active unit based on its previous state")
	}
	if d.recoveryDue("app.service", now, "failed") == "" {
		t.Fatal("blocked a still-failed unit")
	}
	c.CheckGroups["app"].RestartSystemdUnit.TriggeredByChecks = []string{"service", "health"}
	writeConfig(t, p, c)
	if _, err := d.reload(); err != nil {
		t.Fatal(err)
	}
	failed("app/health", "HTTP 503")
	if d.recoveryDue("app.service", now, "active") == "" {
		t.Fatal("ignored a separate confirmed health failure")
	}
	if d.snapshot.Checks["app/service"].FailureIncident.Incident != "incident-app/service" {
		t.Fatal("pre-action inspection manufactured recovery")
	}
}

// The restart names its unit, so the checks that trigger it do not have to
// be checks of that unit: a machine check restarts the unit, and MMWatchdog asks
// systemd to restart exactly the named unit. Only a systemd_unit_state
// check of the restarted unit itself stops counting once that unit is
// active again.
func TestRestartNamesItsUnit(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"web": {
		Timing: checksandalerts.Timing{CheckEvery: "30s", TimeLimitPerRun: "5s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"},
		Checks: map[string]checksandalerts.NamedCheck{
			"memory":         {Check: checksandalerts.Check{Inputs: new(checksandalerts.NodeResource("memory")), FailWhenMeasurementAbove: map[string]float64{"memory_used_percent": 95}}},
			"nginx_running":  {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "nginx.service", FailWhen: "the_unit_is_not_active"}}},
			"worker_running": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "worker.service", FailWhen: "the_unit_is_not_active"}}},
		},
		RestartSystemdUnit: &checksandalerts.RestartSystemdUnit{SystemdUnit: "nginx.service", RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"memory", "nginx_running", "worker_running"}, Attempts: "3", MinTimeBetweenAttempts: "1ms", ResetAttemptsAfterHealthyFor: "10m"}},
	}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if policy := d.policies["nginx.service"]; policy == nil || len(d.policies) != 1 || !reflect.DeepEqual(policy.CheckGroups, []string{"web"}) {
		t.Fatal("the restart's budget is not kept under the unit it names", d.policies)
	}
	now := time.Now()
	failed := func(key string, failing bool) {
		st := d.snapshot.Checks[key]
		st.FailingSince, st.Last = time.Time{}, checksandalerts.Observation{Status: "ok", Message: "healthy", At: now}
		if failing {
			st.FailingSince, st.Last = now, checksandalerts.Observation{Status: "failed", Message: "failed", At: now}
		}
		d.checks[key].fresh = true
	}
	for _, tc := range []struct {
		failing string
		state   string
		due     string
	}{
		{"web/memory", "active", "web/memory"},
		{"web/nginx_running", "active", ""},
		{"web/nginx_running", "failed", "web/nginx_running"},
		{"web/worker_running", "active", "web/worker_running"},
	} {
		for _, key := range []string{"web/memory", "web/nginx_running", "web/worker_running"} {
			failed(key, key == tc.failing)
		}
		if due := d.recoveryDue("nginx.service", now, tc.state); due != tc.due {
			t.Fatalf("%s failing while nginx.service is %s: restart due for %q, want %q", tc.failing, tc.state, due, tc.due)
		}
	}
	// The restart command goes to the named unit, and its checks and every
	// other check group of that unit need fresh observations afterward.
	dir := t.TempDir()
	log := filepath.Join(dir, "restarted")
	script := "#!/bin/sh\nif [ \"$1\" = show ]; then printf 'LoadState=loaded\\nActiveState=active\\n'; else echo \"$3\" >> " + testhelpers.ShellQuote(log) + "; fi\n"
	testhelpers.WriteStandInProgram(t, filepath.Join(dir, "systemctl"), script)
	testhelpers.PutProgramsFirst(t, dir)
	for _, key := range []string{"web/nginx_running", "web/worker_running"} {
		failed(key, false)
	}
	failNow(t, d, "web/memory")
	d.workers.Wait()
	if b, _ := os.ReadFile(log); string(b) != "nginx.service\n" {
		t.Fatalf("restarted %q, want nginx.service", b)
	}
	if attempts := d.snapshot.RecoveryAttemptsByTarget["nginx.service"]; attempts == nil || attempts.Attempts != 1 || d.checks["web/memory"].fresh {
		t.Fatal("the restart did not use an attempt of its unit and ask for fresh observations", attempts)
	}
}

func TestDaemonRejectsUnsafeStateDirectory(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"readable", "writable", "symlink", "other owner"} {
		t.Run(mode, func(t *testing.T) {
			p, c := daemonConfig(t)
			if err := os.Mkdir(c.StateDir, 0700); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "readable":
				if err := os.Chmod(c.StateDir, 0755); err != nil {
					t.Fatal(err)
				}
			case "writable":
				if err := os.Chmod(c.StateDir, 0777); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := c.StateDir + "-target"
				if err := os.Rename(c.StateDir, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, c.StateDir); err != nil {
					t.Fatal(err)
				}
			case "other owner":
				if os.Geteuid() != 0 {
					t.Skip("ownership check requires root; covered by native Linux tests")
				}
				if err := os.Chown(c.StateDir, 65534, 65534); err != nil {
					t.Fatal(err)
				}
			}
			d, err := NewDaemon(p)
			if err == nil {
				d.Close()
				t.Fatal("unsafe state accepted")
			}
			if !strings.Contains(err.Error(), "state directory") {
				t.Fatal(err)
			}
			for _, file := range []string{"daemon.lock", "state.db"} {
				if _, err := os.Lstat(filepath.Join(c.StateDir, file)); !os.IsNotExist(err) {
					t.Fatalf("opened %s before checking directory: %v", file, err)
				}
			}
		})
	}
}

func TestMissedScheduleOnlyInterruptsRelevantRecovery(t *testing.T) {
	t.Parallel()
	for _, check := range []string{"service", "cpu"} {
		for _, gap := range []string{"capacity", "late start"} {
			t.Run(check+"/"+gap, func(t *testing.T) {
				p, c := daemonConfig(t)
				c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {
					Timing:             checksandalerts.Timing{CheckEvery: "1s", TimeLimitPerRun: "500ms", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"},
					Checks:             map[string]checksandalerts.NamedCheck{"service": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "app.service", FailWhen: "the_unit_is_not_active"}}}, "cpu": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitResource{SystemdUnit: "app.service", Resource: "cpu"}}}},
					RestartSystemdUnit: &checksandalerts.RestartSystemdUnit{SystemdUnit: "app.service", RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"service"}, Attempts: "3", MinTimeBetweenAttempts: "1s", ResetAttemptsAfterHealthyFor: "10m"}},
				}}
				writeConfig(t, p, c)
				d, err := NewDaemon(p)
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				now := time.Now().UTC()
				for key, st := range d.snapshot.Checks {
					st.Observe(checksandalerts.Observation{Status: "ok", At: now}, st.Timing, false)
					st.HealthySince = now.Add(-599 * time.Second)
					d.checks[key].next = now.Add(time.Hour)
					d.checks[key].fresh = true
				}
				key := "app/" + check
				rc := d.checks[key]
				rc.next = now.Add(-3 * time.Second)
				rc.scheduled = now.Add(-3 * time.Second)
				d.snapshot.Checks[key].Last.At = now.Add(-2 * time.Second)
				if gap == "capacity" {
					rc.running = true
				}
				b := &incidents.RecoveryAttempts{Attempts: 3}
				d.snapshot.RecoveryAttemptsByTarget["app.service"] = b
				ctx, cancel := context.WithCancel(context.Background())
				cancel() // Exercise scheduling without launching a real systemd/CPU check.
				d.schedule(ctx, now)
				d.workers.Wait()
				if d.healthySince("app.service").IsZero() != (check == "service") {
					t.Fatalf("incorrect interruption for %s", check)
				}
				later := now.Add(2 * time.Second)
				st := d.snapshot.Checks["app/service"]
				st.Observe(checksandalerts.Observation{Status: "ok", At: later}, st.Timing, false)
				d.startRecovery(ctx, later)
				want := 0
				if check == "service" {
					want = 3
				}
				if b.Attempts != want {
					t.Fatalf("attempts=%d, want %d", b.Attempts, want)
				}
			})
		}
	}
}

func TestReloadQueuesRetirementAfterFailure(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:1"}, TimeLimitPerDeliveryAttempt: "1s"}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	st := d.snapshot.Checks["app/shell"]
	events := st.Observe(checksandalerts.Observation{Status: "failed", At: time.Now()}, checksandalerts.Timing{CheckEvery: "1s", TimeLimitPerRun: "800ms", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}, false)
	id := st.FailureIncident.Incident
	if err := d.record(events); err != nil {
		t.Fatal(err)
	}
	c.CheckGroups["app"] = checksandalerts.CheckGroup{Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "false"}}}
	writeConfig(t, p, c)
	if _, err := d.reload(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"failed", "closed_without_recovery"} {
		pending, err := d.store.Pending()
		if err != nil || len(pending) != 1 {
			t.Fatal(pending, err)
		}
		event := pending[0].Event
		if event.Type != want || event.Incident != id || event.Evidence == nil || event.Evidence.Incident != id {
			t.Fatalf("invalid terminal ordering/evidence: %+v", event)
		}
		if _, err := d.store.Delivered(pending[0], nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSharedUnitUsesOneBudget(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	c.CheckGroups = map[string]checksandalerts.CheckGroup{
		"app":  {Checks: map[string]checksandalerts.NamedCheck{"service": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "app.service", FailWhen: "the_unit_is_not_active"}}}}, RestartSystemdUnit: &checksandalerts.RestartSystemdUnit{SystemdUnit: "app.service", RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"service"}, Attempts: "2", MinTimeBetweenAttempts: "1m", ResetAttemptsAfterHealthyFor: "10m"}}},
		"peer": {Checks: map[string]checksandalerts.NamedCheck{"service": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "app.service", FailWhen: "the_unit_is_not_active"}}}}, RestartSystemdUnit: &checksandalerts.RestartSystemdUnit{SystemdUnit: "app.service", RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"service"}, Attempts: "2", MinTimeBetweenAttempts: "1m", ResetAttemptsAfterHealthyFor: "10m"}}},
	}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Now()
	st := d.snapshot.Checks["app/service"]
	st.FailureIncident.Incident, st.FailingSince = "incident", now
	st.Last = checksandalerts.Observation{Status: "failed", At: now}
	d.checks["app/service"].fresh = true
	for _, tc := range []struct {
		budget incidents.RecoveryAttempts
		due    bool
	}{
		{incidents.RecoveryAttempts{Attempts: 1, Last: now.Add(-10 * time.Minute), LastFinished: now.Add(-2 * time.Minute)}, true},
		{incidents.RecoveryAttempts{Attempts: 2, Last: now.Add(-10 * time.Minute), LastFinished: now.Add(-2 * time.Minute)}, false},
		{incidents.RecoveryAttempts{Attempts: 1, Last: now.Add(-10 * time.Minute), LastFinished: now.Add(-30 * time.Second)}, false},
		// An attempt that MMWatchdog never saw end, because MMWatchdog
		// stopped while it ran, waits from its start.
		{incidents.RecoveryAttempts{Attempts: 1, Last: now.Add(-30 * time.Second)}, false},
		{incidents.RecoveryAttempts{Attempts: 1, Last: now.Add(-2 * time.Minute)}, true},
	} {
		budget := tc.budget
		d.snapshot.RecoveryAttemptsByTarget["app.service"] = &budget
		for _, state := range []string{"", "failed"} {
			if got := d.recoveryDue("app.service", now, state) != ""; got != tc.due {
				t.Fatalf("budget %+v, state %q: due=%v, want %v", tc.budget, state, got, tc.due)
			}
		}
	}
	d.snapshot.RecoveryAttemptsByTarget["app.service"] = &incidents.RecoveryAttempts{}
	for _, window := range []string{"peer", checksandalerts.EveryCheckGroup} {
		d.snapshot.Maintenance = map[string]checksandalerts.Window{window: {Until: now.Add(time.Hour)}}
		if d.recoveryDue("app.service", now, "failed") != "" {
			t.Fatalf("restarted a unit during %s maintenance", window)
		}
	}
}

// A restart in progress survives a reload that leaves its unit's policy
// unchanged, and is discarded by one that changes it.
func TestReloadKeepsRestartOnlyForUnchangedPolicy(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"unrelated check", "unit policy"} {
		t.Run(change, func(t *testing.T) {
			p, c := daemonConfig(t)
			c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {Checks: map[string]checksandalerts.NamedCheck{"service": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "app.service", FailWhen: "the_unit_is_not_active"}}}}, RestartSystemdUnit: &checksandalerts.RestartSystemdUnit{SystemdUnit: "app.service", RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"service"}, Attempts: "3", MinTimeBetweenAttempts: "1s", ResetAttemptsAfterHealthyFor: "10m"}}}}
			writeConfig(t, p, c)
			d, err := NewDaemon(p)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			dir := t.TempDir()
			release, marker := filepath.Join(dir, "release"), filepath.Join(dir, "restarted")
			script := "#!/bin/sh\nif [ \"$1\" = show ]; then while [ ! -e " + testhelpers.ShellQuote(release) + " ]; do sleep 0.01; done; printf 'LoadState=loaded\\nActiveState=failed\\n'; else touch " + testhelpers.ShellQuote(marker) + "; fi\n"
			testhelpers.WriteStandInProgram(t, filepath.Join(dir, "systemctl"), script)
			testhelpers.PutProgramsFirst(t, dir)
			st := d.snapshot.Checks["app/service"]
			st.Observe(checksandalerts.Observation{Status: "failed", At: time.Now().UTC(), Message: "service down"}, checksandalerts.Timing{CheckEvery: "30s", TimeLimitPerRun: "5s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}, false)
			d.mu.Lock()
			d.checks["app/service"].fresh = true
			d.startRecovery(testhelpers.TestContext(t), time.Now().UTC())
			started := d.recovering["app.service"]
			d.mu.Unlock()
			if !started {
				t.Fatal("restart did not start")
			}
			switch change {
			case "unrelated check":
				c.CheckGroups["other"] = checksandalerts.CheckGroup{Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true"}}}
			case "unit policy":
				c.CheckGroups["app"].RestartSystemdUnit.Attempts = "5"
			}
			writeConfig(t, p, c)
			if _, err := d.reload(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}
			d.workers.Wait()
			_, err = os.Stat(marker)
			if restarted := err == nil; restarted != (change == "unrelated check") {
				t.Fatalf("restarted=%v after %s reload", restarted, change)
			}
		})
	}
}

// heartbeatConfig configures one heartbeat check, "backup", and a webhook.
func heartbeatConfig(t *testing.T) (string, *config.Config) {
	t.Helper()
	p, c := daemonConfig(t)
	dir := filepath.Dir(p)
	cert, key := testhelpers.TestCertificate(t, dir)
	tokenPath := filepath.Join(dir, "token")
	os.WriteFile(tokenPath, []byte("test-heartbeat-token"), 0600)
	c.HeartbeatReceiver = &config.HeartbeatReceiver{ListenOnAddress: "127.0.0.1:0", TLSCertificateFile: cert, TLSPrivateKeyFile: key}
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:9/hook"}, TimeLimitPerDeliveryAttempt: "5s"}}
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"backup": {Heartbeat: &checksandalerts.Heartbeat{MaxTimeBetweenHeartbeats: "26h", ExpectedBearerTokenFile: tokenPath}}}
	writeConfig(t, p, c)
	return p, c
}
func postHeartbeat(d *Daemon) int {
	r := httptest.NewRequest("POST", "https://localhost/v1/heartbeats/backup", nil)
	r.Header.Set("Authorization", "Bearer test-heartbeat-token")
	w := httptest.NewRecorder()
	d.heartbeat(w, r)
	return w.Code
}
func setMaintenance(t *testing.T, d *Daemon, check, duration string) {
	t.Helper()
	w := httptest.NewRecorder()
	d.routes().ServeHTTP(w, httptest.NewRequest("POST", maintenanceURL(check, duration), bytes.NewBufferString(maintenanceBody(duration))))
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
}

// queuedEvents lists the queued deliveries' events in order.
func queuedEvents(t *testing.T, d *Daemon) []checksandalerts.Event {
	t.Helper()
	rows, e := d.store.DB.Query("SELECT event FROM outbox ORDER BY seq")
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	events := []checksandalerts.Event{}
	for rows.Next() {
		var b []byte
		var ev checksandalerts.Event
		if e = rows.Scan(&b); e == nil {
			e = json.Unmarshal(b, &ev)
		}
		if e != nil {
			t.Fatal(e)
		}
		events = append(events, ev)
	}
	return events
}

// queued lists the types of the queued deliveries in order.
func queued(t *testing.T, d *Daemon) []string {
	t.Helper()
	types := []string{}
	for _, ev := range queuedEvents(t, d) {
		types = append(types, ev.Type)
	}
	return types
}

// A heartbeat has no executions, so maintenance end applies its current
// evidence: a failure suppressed during maintenance is announced, and an
// announced failure that recovered during maintenance is closed.
func TestMaintenanceEndReconcilesHeartbeat(t *testing.T) {
	t.Parallel()
	for _, recovered := range []bool{false, true} {
		for _, end := range []string{"command", "expiry"} {
			name := "failed during maintenance/" + end
			if recovered {
				name = "recovered during maintenance/" + end
			}
			t.Run(name, func(t *testing.T) {
				p, _ := heartbeatConfig(t)
				d, err := NewDaemon(p)
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				overdue := func() {
					d.snapshot.Checks["backup/heartbeat"].Deadline = time.Now().UTC().Add(-time.Second)
					d.schedule(context.Background(), time.Now().UTC())
				}
				// The end is announced before the reconciliation it causes.
				during, want := []string{"maintenance_on"}, []string{"maintenance_on", "maintenance_off", "failed"}
				if recovered {
					overdue()
					setMaintenance(t, d, "backup", "10m")
					if code := postHeartbeat(d); code != http.StatusNoContent {
						t.Fatal(code)
					}
					during, want = []string{"failed", "maintenance_on"}, []string{"failed", "maintenance_on", "maintenance_off", "recovered"}
				} else {
					setMaintenance(t, d, "backup", "10m")
					overdue()
				}
				if got := queued(t, d); !reflect.DeepEqual(got, during) {
					t.Fatalf("maintenance did not suppress the change: queued %v", got)
				}
				if end == "command" {
					setMaintenance(t, d, "backup", "off")
				} else {
					w := d.snapshot.Maintenance["backup"]
					w.Until = time.Now().UTC()
					d.snapshot.Maintenance["backup"] = w
					d.schedule(context.Background(), time.Now().UTC())
				}
				st := d.snapshot.Checks["backup/heartbeat"]
				if got := queued(t, d); !reflect.DeepEqual(got, want) || (recovered && st.FailureIncident.Announced != "") || (!recovered && st.FailureIncident.Announced != st.FailureIncident.Incident) {
					t.Fatalf("maintenance end did not reconcile the heartbeat: queued %v, health %+v", got, st.FailureIncident)
				}
			})
		}
	}
}

// A reload that removes a check in maintenance ends its window with an alert,
// since its start was announced.
func TestRemovedCheckEndsItsWindow(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:9/hook"}, TimeLimitPerDeliveryAttempt: "5s"}}
	c.CheckGroups["web"] = checksandalerts.CheckGroup{Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true"}}}
	writeConfig(t, p, c)
	d, e := NewDaemon(p)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	setMaintenance(t, d, "web", "1h")
	delete(c.CheckGroups, "web")
	writeConfig(t, p, c)
	if _, e = d.reload(); e != nil {
		t.Fatal(e)
	}
	types := []string{}
	for _, ev := range queuedEvents(t, d) {
		types = append(types, ev.CheckGroup+" "+ev.Type)
	}
	if !reflect.DeepEqual(types, []string{"web maintenance_on", "web maintenance_off"}) || len(d.snapshot.Maintenance) != 0 {
		t.Fatal(types, d.snapshot.Maintenance)
	}
}

// A whole-node window covers every check, including one a reload adds, and
// overlaps per-check windows: a check stays suppressed until no window covers
// it. Starting and ending windows alert, and an end is queued before the
// reconciliation it causes.
func TestWholeServerMaintenance(t *testing.T) {
	t.Parallel()
	p, c := heartbeatConfig(t)
	c.CheckGroups["app"] = checksandalerts.CheckGroup{Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true"}}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	post := func(path, body string) int {
		t.Helper()
		w := httptest.NewRecorder()
		d.routes().ServeHTTP(w, httptest.NewRequest("POST", "http://localhost/maintenance/on/"+path, bytes.NewBufferString(body)))
		return w.Code
	}
	for _, body := range []string{`{"for":"10m","reason":"two\nlines"}`, `{"for":"10m","user":"` + strings.Repeat("u", 65) + `"}`, `{"for":"10m","reason":"` + strings.Repeat("r", 201) + `"}`} {
		if code := post(checksandalerts.EveryCheckGroup, body); code != http.StatusBadRequest {
			t.Fatal("accepted", body, code)
		}
	}
	if code := post("missing", `{"for":"10m"}`); code != http.StatusNotFound {
		t.Fatal(code)
	}
	d.checks["app/shell"].fresh = true
	if code := post(checksandalerts.EveryCheckGroup, `{"for":"10m","reason":"deploy v1.4","user":"alice"}`); code != http.StatusOK {
		t.Fatal(code)
	}
	started := queuedEvents(t, d)
	if len(started) != 1 || started[0].Type != "maintenance_on" || started[0].CheckGroup != checksandalerts.EveryCheckGroup || started[0].Maintenance == nil ||
		started[0].Maintenance.Reason != "deploy v1.4" || started[0].Maintenance.User != "alice" || !strings.HasSuffix(started[0].Message, ", turned on by alice: deploy v1.4") {
		t.Fatalf("start alert does not describe the window: %+v", started)
	}
	if !d.checks["app/shell"].needsFresh() {
		t.Fatal("whole-node maintenance kept evidence from before it started")
	}
	c.CheckGroups["added"] = checksandalerts.CheckGroup{Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true"}}}
	writeConfig(t, p, c)
	if _, err := d.reload(); err != nil {
		t.Fatal(err)
	}
	for _, check := range []string{"app", "backup", "added"} {
		if !d.suppressed(check) {
			t.Fatalf("%s is not covered by whole-node maintenance", check)
		}
	}
	setMaintenance(t, d, "backup", "1h")
	d.snapshot.Checks["backup/heartbeat"].Deadline = time.Now().UTC().Add(-time.Second)
	d.schedule(context.Background(), time.Now().UTC())
	w := d.snapshot.Maintenance[checksandalerts.EveryCheckGroup]
	w.Until = time.Now().UTC()
	d.snapshot.Maintenance[checksandalerts.EveryCheckGroup] = w
	d.schedule(context.Background(), time.Now().UTC())
	if !d.suppressed("backup") || d.suppressed("app") {
		t.Fatal("expiry of the whole-node window did not leave only the check's own window")
	}
	setMaintenance(t, d, "backup", "off")
	events := queuedEvents(t, d)
	types := []string{}
	for _, ev := range events {
		types = append(types, ev.CheckGroup+" "+ev.Type)
	}
	want := []string{"every_check_group maintenance_on", "backup maintenance_on", "every_check_group maintenance_off", "backup maintenance_off", "backup failed"}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("queued %v, want %v", types, want)
	}
	if events[2].Message != "maintenance expired: deploy v1.4" || events[2].Maintenance.User != "alice" {
		t.Fatalf("expiry alert does not describe the window: %+v", events[2])
	}
}

// setBoot makes daemons started in this test read *id as the kernel's boot ID
// and booted as its boot time.
func setBoot(t *testing.T, id *string, booted time.Time) {
	t.Helper()
	restore := readBoot
	t.Cleanup(func() { readBoot = restore })
	readBoot = func() (string, time.Time, error) { return *id, booted, nil }
}

func TestParseBootTime(t *testing.T) {
	t.Parallel()
	booted, e := parseBootTime("cpu  1 2 3 4 5 6 7 8\nbtime 1767362580\nprocesses 42\n")
	if e != nil || !booted.Equal(time.Date(2026, 1, 2, 14, 3, 0, 0, time.UTC)) {
		t.Fatal(booted, e)
	}
	for _, stat := range []string{"cpu  1 2 3 4 5 6 7 8\n", "btime soon\n", "btime 0\n"} {
		if _, e := parseBootTime(stat); e == nil {
			t.Errorf("accepted %q", stat)
		}
	}
}

// Starting in a new boot alerts once, at the severity the node states for
// its own reboot, naming the boot time and when MMWatchdog last recorded state. The
// first start on a host and restarts within a boot send
// nothing, a check's own window does not hold the alert, and a whole-node
// window open before the reboot records it locally only.
func TestRebootAlerts(t *testing.T) {
	boot := "boot-1"
	setBoot(t, &boot, time.Date(2026, 1, 2, 14, 3, 0, 0, time.UTC))
	p, c := daemonConfig(t)
	c.AlertSeverityWhenThisNodeHasRebooted = "warn"
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"ops": {AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:9/hook"}, TimeLimitPerDeliveryAttempt: "5s", RemindEvery: "1h"}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	restart := func(next string) {
		t.Helper()
		d.Close()
		boot = next
		if d, err = NewDaemon(p); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		if d != nil {
			d.Close()
		}
	}()
	if events, _ := d.store.Events(10); len(events) != 0 || d.snapshot.Boot != "boot-1" {
		t.Fatal("the first start on a host recorded more than its boot", events, d.snapshot.Boot)
	}
	restart("boot-1")
	if events, _ := d.store.Events(10); len(events) != 0 {
		t.Fatal("a restart within the boot recorded an event", events)
	}
	before := time.Now().UTC()
	restart("boot-1")
	after := time.Now().UTC()
	restart("boot-2")
	if d.store.Saved.Before(before) || d.store.Saved.After(after) {
		t.Fatalf("last ran %s, not the previous start's commit between %s and %s", d.store.Saved, before, after)
	}
	got := queuedEvents(t, d)
	want := d.store.Node + " rebooted at 14:03 UTC; MMWatchdog last ran at " + d.store.Saved.Format("15:04 UTC")
	if len(got) != 1 || got[0].Type != "rebooted" || got[0].AlertSeverity != "warn" || got[0].CheckGroup != "" || got[0].Check != "" || got[0].Node != d.store.Node || got[0].Message != want || got[0].At.IsZero() {
		t.Fatalf("queued %+v, want one rebooted alert %q", got, want)
	}
	// A reboot is a notice, not an incident, so it is never reminded.
	if reminders := d.remind(time.Now().Add(48 * time.Hour)); len(reminders) != 0 {
		t.Fatal("a reboot alert was reminded", reminders)
	}
	setMaintenance(t, d, "app", "10m")
	restart("boot-3")
	if got := queued(t, d); !reflect.DeepEqual(got, []string{"rebooted", "maintenance_on", "rebooted"}) {
		t.Fatal("a check's window held the reboot alert", got)
	}
	setMaintenance(t, d, checksandalerts.EveryCheckGroup, "10m")
	restart("boot-4")
	events, _ := d.store.Events(1)
	if got := queued(t, d); !reflect.DeepEqual(got, []string{"rebooted", "maintenance_on", "rebooted", "maintenance_on"}) || events[0].Type != "rebooted" {
		t.Fatal("a whole-node window did not keep the reboot local", got, events)
	}
}

// State copied from another machine, as when moving a fleet hub, holds that
// machine's boot ID. The first start on the new machine sends no reboot
// notice, and a later real reboot of the new machine still does.
func TestNoRebootAlertForStateFromAnotherMachine(t *testing.T) {
	boot, machine := "boot-1", "machine-a"
	setBoot(t, &boot, time.Date(2026, 1, 2, 14, 3, 0, 0, time.UTC))
	restore := readMachineID
	t.Cleanup(func() { readMachineID = restore })
	readMachineID = func() string { return machine }
	p, c := daemonConfig(t)
	c.AlertSeverityWhenThisNodeHasRebooted = "warn"
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"ops": {AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:9/hook"}, TimeLimitPerDeliveryAttempt: "5s", RemindEvery: "1h"}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { d.Close() }()
	restart := func(nextBoot, nextMachine string) []string {
		t.Helper()
		d.Close()
		boot, machine = nextBoot, nextMachine
		if d, err = NewDaemon(p); err != nil {
			t.Fatal(err)
		}
		return queued(t, d)
	}
	if got := restart("boot-9", "machine-b"); len(got) != 0 {
		t.Fatal("state from another machine sent", got)
	}
	if got := restart("boot-10", "machine-b"); !reflect.DeepEqual(got, []string{"rebooted"}) {
		t.Fatal("a real reboot of the new machine did not send its notice", got)
	}
}

// A restart command ends the evidence that justified it: another restart
// needs a failure observed after the command, even once the min_time_between_attempts passes.
// That holds for every check of the unit, since a reload can give one a
// restart policy while the command runs.
func TestRestartNeedsFailureObservedAfterCommand(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {
		Timing:             checksandalerts.Timing{CheckEvery: "30s", TimeLimitPerRun: "5s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"},
		Checks:             map[string]checksandalerts.NamedCheck{"service": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "app.service", FailWhen: "the_unit_is_not_active"}}}, "health": {Check: checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: "http://127.0.0.1:1/health"}}}},
		RestartSystemdUnit: &checksandalerts.RestartSystemdUnit{SystemdUnit: "app.service", RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"service", "health"}, Attempts: "3", MinTimeBetweenAttempts: "1s", ResetAttemptsAfterHealthyFor: "10m"}},
	}, "watch": {Checks: map[string]checksandalerts.NamedCheck{"service": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "app.service", FailWhen: "the_unit_is_not_active"}}}}, Timing: checksandalerts.Timing{CheckEvery: "30s", TimeLimitPerRun: "5s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	dir := t.TempDir()
	marker := filepath.Join(dir, "automatic_restarts")
	script := "#!/bin/sh\nif [ \"$1\" = show ]; then printf 'LoadState=loaded\\nActiveState=active\\n'; else echo restart >> " + testhelpers.ShellQuote(marker) + "; fi\n"
	testhelpers.WriteStandInProgram(t, filepath.Join(dir, "systemctl"), script)
	testhelpers.PutProgramsFirst(t, dir)
	restarts := func() int { b, _ := os.ReadFile(marker); return strings.Count(string(b), "restart") }
	fail := func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		now := time.Now().UTC()
		health := d.snapshot.Checks["app/health"]
		d.checks["app/health"].fresh = true
		_ = d.record(health.Observe(checksandalerts.Observation{Status: "failed", At: now, Message: "HTTP 503"}, health.Timing, false))
		d.startRecovery(testhelpers.TestContext(t), now)
	}
	d.mu.Lock()
	d.checks["watch/service"].fresh = true
	d.mu.Unlock()
	fail()
	d.workers.Wait()
	if restarts() != 1 || !d.checks["watch/service"].needsFresh() {
		t.Fatalf("first restart did not run, or left the unit's other check's evidence current: %d", restarts())
	}
	time.Sleep(1100 * time.Millisecond) // the min_time_between_attempts passes with no new observation
	d.mu.Lock()
	d.startRecovery(testhelpers.TestContext(t), time.Now().UTC())
	d.mu.Unlock()
	d.workers.Wait()
	if restarts() != 1 || !d.checks["app/health"].needsFresh() {
		t.Fatalf("restarted again on evidence from before the command: %d restarts", restarts())
	}
	fail()
	d.workers.Wait()
	if restarts() != 2 {
		t.Fatalf("a failure observed after the command did not restart_service: %d restarts", restarts())
	}
}

// A restart's budget can reset only after a full healthy window observed
// after its command completes, never during the command or from its samples.
func TestRestartBudgetNeedsPostCommandHealthyWindow(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	// A restart is due on a failed observation no older than check_every and
	// time_limit_per_run: far longer than the stand-in for systemctl takes to
	// answer before the restart, also on a busy machine.
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {
		Checks: map[string]checksandalerts.NamedCheck{"service": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "app.service", FailWhen: "the_unit_is_not_active"}}}}, Timing: checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "1m", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"},
		RestartSystemdUnit: &checksandalerts.RestartSystemdUnit{SystemdUnit: "app.service", RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"service"}, Attempts: "3", MinTimeBetweenAttempts: "1s", ResetAttemptsAfterHealthyFor: "1s"}},
	}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	dir := t.TempDir()
	entered, release := filepath.Join(dir, "entered"), filepath.Join(dir, "release")
	script := "#!/bin/sh\nif [ \"$1\" = show ]; then printf 'LoadState=loaded\\nActiveState=failed\\n'; else touch " + testhelpers.ShellQuote(entered) + "; while [ ! -e " + testhelpers.ShellQuote(release) + " ]; do sleep 0.01; done; fi\n"
	testhelpers.WriteStandInProgram(t, filepath.Join(dir, "systemctl"), script)
	testhelpers.PutProgramsFirst(t, dir)
	ctx, cancel := context.WithCancel(testhelpers.TestContext(t))
	defer func() {
		_ = os.WriteFile(release, nil, 0600)
		cancel()
		d.workers.Wait()
	}()
	d.mu.Lock()
	now := time.Now().UTC()
	st := d.snapshot.Checks["app/service"]
	st.Observe(checksandalerts.Observation{Status: "failed", At: now}, st.Timing, false)
	d.checks["app/service"].fresh = true
	d.startRecovery(ctx, now)
	d.mu.Unlock()
	deadline := time.Now().Add(testhelpers.LongestWaitForAnEvent)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restart command did not start")
		}
		time.Sleep(time.Millisecond)
	}
	d.mu.Lock()
	st = d.snapshot.Checks["app/service"] // reservation committed a clone
	during := time.Now().UTC()
	st.Observe(checksandalerts.Observation{Status: "ok", At: during}, st.Timing, false)
	// Another check can finish after the reset interval while the
	// systemd command remains in progress.
	d.startRecovery(ctx, during.Add(2*time.Second))
	attempts := d.snapshot.RecoveryAttemptsByTarget["app.service"].Attempts
	d.mu.Unlock()
	if attempts != 1 {
		t.Fatal("healthy observations during the command reset its reserved attempt")
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	d.workers.Wait()
	d.mu.Lock()
	defer d.mu.Unlock()
	st = d.snapshot.Checks["app/service"]
	post := during.Add(3 * time.Second)
	d.startRecovery(ctx, post)
	if d.snapshot.RecoveryAttemptsByTarget["app.service"].Attempts != 1 || !st.HealthySince.IsZero() || !d.checks["app/service"].needsFresh() {
		t.Fatal("command completion kept earlier health as evidence for a budget reset")
	}
	for _, elapsed := range []time.Duration{0, 500 * time.Millisecond, time.Second} {
		at := post.Add(elapsed)
		st.Observe(checksandalerts.Observation{Status: "ok", At: at}, st.Timing, false)
		d.checks["app/service"].fresh = true
		d.startRecovery(ctx, at)
		want := 1
		if elapsed == time.Second {
			want = 0
		}
		if got := d.snapshot.RecoveryAttemptsByTarget["app.service"].Attempts; got != want {
			t.Fatalf("after %s of post-command health: got %d attempts, want %d", elapsed, got, want)
		}
	}
}

// commandCheck is a direct check whose recovery command runs script as the
// current user.
func commandCheck(t *testing.T, script, timeout string, budget checksandalerts.RecoveryAttemptSettings) checksandalerts.CheckGroup {
	return checksandalerts.CheckGroup{Timing: checksandalerts.Timing{CheckEvery: "30s", TimeLimitPerRun: "5s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}, Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "false"}},
		RecoveryCommand: &checksandalerts.RecoveryCommand{Script: script, TimeLimitPerAttempt: checksandalerts.Duration(timeout), ExecOptions: testhelpers.OwnOptions(t), RecoveryAttemptSettings: budget}}
}

// failNow records a failed observation of key that confirms at once, then
// starts due recovery, as an observation completion does.
func failNow(t *testing.T, d *Daemon, key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now().UTC()
	st := d.snapshot.Checks[key]
	d.checks[key].fresh = true
	_ = d.record(st.Observe(checksandalerts.Observation{Status: "failed", At: now, Message: "disk above 90%"}, st.Timing, d.suppressed(st.CheckGroup)))
	d.startRecovery(testhelpers.TestContext(t), now)
}

// recoveryCommandResult returns the check's latest recovery command result.
func recoveryCommandResult(t *testing.T, d *Daemon, check string) checksandalerts.Event {
	t.Helper()
	events, err := d.store.CheckGroupEvents(check, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type == "recovery_command_result" {
			return ev
		}
	}
	t.Fatal("no recovery command result", events)
	return checksandalerts.Event{}
}

// A recovery command runs once per confirmed failure within its budget, with
// only MMWatchdog's environment and the failure's identity. Its result is recorded
// locally and never sent to destinations.
func TestRecoveryCommandRunsWithinBudget(t *testing.T) {
	p, c := daemonConfig(t)
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:1"}, TimeLimitPerDeliveryAttempt: "1s"}}
	dir := t.TempDir()
	runs, env := filepath.Join(dir, "runs"), filepath.Join(dir, "env")
	script := "echo run >> " + testhelpers.ShellQuote(runs) + "; env > " + testhelpers.ShellQuote(env) + "; printf 'removed 3 files'; printf 'cache busy' >&2; exit 3"
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"disk": commandCheck(t, script, "5s", checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"shell"}, Attempts: "2", MinTimeBetweenAttempts: "1ms", ResetAttemptsAfterHealthyFor: "1m"})}
	writeConfig(t, p, c)
	t.Setenv("MMWATCHDOG_DAEMON_SECRET", "not for commands")
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	count := func() int { b, _ := os.ReadFile(runs); return strings.Count(string(b), "run") }
	failNow(t, d, "disk/shell")
	d.workers.Wait()
	incident := d.snapshot.Checks["disk/shell"].FailureIncident.Incident
	if count() != 1 || incident == "" {
		t.Fatalf("confirmed failure ran the command %d times", count())
	}
	b, _ := os.ReadFile(env)
	for _, want := range []string{"MMWATCHDOG_CHECK_GROUP=disk\n", "MMWATCHDOG_CHECK=shell\n", "MMWATCHDOG_INCIDENT_ID=" + incident + "\n"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("command environment lacks %q:\n%s", want, b)
		}
	}
	if strings.Contains(string(b), "MMWATCHDOG_DAEMON_SECRET") || strings.Contains(string(b), "MMWATCHDOG_MODULE_HELPER") || strings.Contains(string(b), "MMWATCHDOG_BIN") {
		t.Fatal("command inherited the daemon's environment")
	}
	events, err := d.store.CheckGroupEvents("disk", 10)
	if err != nil || len(events) != 3 || events[2].Type != "failed" || events[1].Type != "recovery_command_started" || events[0].Type != "recovery_command_result" {
		t.Fatal("expected failure, reservation, and result", events, err)
	}
	result := events[0]
	o := result.Evidence.Observation
	if result.Message != "recovery command finished with exit code 3" || result.Incident != incident || result.Check != "shell" || o.ExitCode == nil || *o.ExitCode != 3 || o.Stdout != "removed 3 files" || o.Stderr != "cache busy" {
		t.Fatalf("result does not record the exit status and output: %+v", result)
	}
	if got := queued(t, d); !reflect.DeepEqual(got, []string{"failed"}) {
		t.Fatal("recovery command events reached destinations:", got)
	}
	budget := d.snapshot.RecoveryAttemptsByTarget["disk"]
	if budget.Attempts != 1 || budget.LastResult != "exit 3" || !d.checks["disk/shell"].needsFresh() {
		t.Fatal("result did not update the budget or require a fresh observation", budget)
	}
	if saved, err := d.store.Load(); err != nil || saved.RecoveryAttemptsByTarget["disk"].Attempts != 1 {
		t.Fatal("attempt is not durable", saved.RecoveryAttemptsByTarget, err)
	}
	// Another attempt needs a failure observed after the command.
	time.Sleep(2 * time.Millisecond)
	d.mu.Lock()
	d.startRecovery(context.Background(), time.Now().UTC())
	d.mu.Unlock()
	d.workers.Wait()
	if count() != 1 {
		t.Fatal("ran again on evidence from before the command")
	}
	failNow(t, d, "disk/shell")
	d.workers.Wait()
	failNow(t, d, "disk/shell")
	d.workers.Wait()
	if count() != 2 || d.snapshot.RecoveryAttemptsByTarget["disk"].Attempts != 2 {
		t.Fatalf("budget of 2 allowed %d runs", count())
	}
	w := httptest.NewRecorder()
	d.routes().ServeHTTP(w, httptest.NewRequest("POST", "http://localhost/reset-recovery-attempts/disk", nil))
	if w.Code != http.StatusOK || d.snapshot.RecoveryAttemptsByTarget["disk"].Attempts != 0 {
		t.Fatal("reset did not clear the command budget", w.Code, w.Body.String())
	}
	failNow(t, d, "disk/shell")
	d.workers.Wait()
	if count() != 3 {
		t.Fatal("reset budget did not allow another run")
	}
	// Health of every triggering check for reset_attempts_after_healthy_for refills it.
	d.mu.Lock()
	defer d.mu.Unlock()
	st, now := d.snapshot.Checks["disk/shell"], time.Now().UTC()
	for _, at := range []time.Time{now, now.Add(time.Minute)} {
		st.Observe(checksandalerts.Observation{Status: "ok", At: at}, st.Timing, false)
		d.checks["disk/shell"].fresh = true
		d.startRecovery(context.Background(), at)
	}
	if d.snapshot.RecoveryAttemptsByTarget["disk"].Attempts != 0 {
		t.Fatal("sustained health did not refill the budget")
	}
}

// Maintenance holds a recovery command, and an attempt storage cannot record
// does not run.
func TestRecoveryCommandWaitsForMaintenanceAndDurableReservation(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	runs := filepath.Join(t.TempDir(), "runs")
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"disk": commandCheck(t, "echo run >> "+testhelpers.ShellQuote(runs), "5s", checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"shell"}, Attempts: "3", MinTimeBetweenAttempts: "1m", ResetAttemptsAfterHealthyFor: "10m"})}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ran := func() bool { _, err := os.Stat(runs); return err == nil }
	setMaintenance(t, d, "disk", "1h")
	failNow(t, d, "disk/shell")
	d.workers.Wait()
	if ran() || d.snapshot.Checks["disk/shell"].FailureIncident.Incident == "" {
		t.Fatal("recovery command ran during maintenance")
	}
	setMaintenance(t, d, "disk", "off")
	if _, err := d.store.DB.Exec("CREATE TRIGGER reject_snapshot BEFORE UPDATE ON snapshot BEGIN SELECT RAISE(FAIL, 'fixture disk full'); END"); err != nil {
		t.Fatal(err)
	}
	failNow(t, d, "disk/shell")
	d.workers.Wait()
	if ran() || d.snapshot.RecoveryAttemptsByTarget["disk"].Attempts != 0 {
		t.Fatal("recovery command ran without a durable attempt")
	}
	for _, event := range d.pending {
		if event.Type == "recovery_command_started" {
			t.Fatal("uncommitted attempt left an action event", event)
		}
	}
	if _, err := d.store.DB.Exec("DROP TRIGGER reject_snapshot"); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	if err := d.record(nil); err != nil {
		t.Fatal(err)
	}
	d.startRecovery(context.Background(), time.Now().UTC())
	d.mu.Unlock()
	d.workers.Wait()
	if !ran() || d.snapshot.RecoveryAttemptsByTarget["disk"].Attempts != 1 {
		t.Fatal("recorded attempt did not run once storage recovered")
	}
}

// A timeout, a failed launch, and output beyond the limits are each the
// attempt's result.
func TestRecoveryCommandResults(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, script, timeout, user, message string
		exit                                 bool
		truncated                            bool
	}{
		{"time_limit_per_run", "sleep 60", "100ms", "", "recovery command timed out after 100ms", false, false},
		{"launch", "true", "5s", "mmwatchdog-no-such-user", "recovery command failed: run_as_user_account \"mmwatchdog-no-such-user\"", false, false},
		{"output", "head -c 70000 /dev/zero | tr '\\0' x", "5s", "", "recovery command finished with exit code 0", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, c := daemonConfig(t)
			check := commandCheck(t, tc.script, tc.timeout, checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"shell"}, Attempts: "3", MinTimeBetweenAttempts: "1m", ResetAttemptsAfterHealthyFor: "10m"})
			if tc.user != "" {
				check.RecoveryCommand.RunAsUserAccount = tc.user
			}
			c.CheckGroups = map[string]checksandalerts.CheckGroup{"disk": check}
			writeConfig(t, p, c)
			d, err := NewDaemon(p)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			start := time.Now()
			failNow(t, d, "disk/shell")
			d.workers.Wait()
			result := recoveryCommandResult(t, d, "disk")
			e := result.Evidence
			// A command that its time limit did not end would take a
			// minute.
			if !strings.HasPrefix(result.Message, tc.message) || (e.Observation.ExitCode != nil) != tc.exit || e.Truncated != tc.truncated || time.Since(start) > 30*time.Second {
				t.Fatalf("%+v %+v", result, e.Observation)
			}
			if d.snapshot.RecoveryAttemptsByTarget["disk"].Attempts != 1 {
				t.Fatal("result did not consume the attempt")
			}
		})
	}
}

// TriggeredByChecks a failure calls for both, the recovery command runs first, and the
// restart waits for a failure observed after it. mmwatchdog reset-recovery-attempts clears both budgets.
func TestRecoveryCommandRunsBeforeRestart(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "actions")
	script := "#!/bin/sh\nif [ \"$1\" = show ]; then printf 'LoadState=loaded\\nActiveState=failed\\n'; else echo restart >> " + testhelpers.ShellQuote(log) + "; fi\n"
	testhelpers.WriteStandInProgram(t, filepath.Join(dir, "systemctl"), script)
	testhelpers.PutProgramsFirst(t, dir)
	// The unit's name sorts before the check's, so order is not by name.
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"web": {
		Timing:             checksandalerts.Timing{CheckEvery: "30s", TimeLimitPerRun: "5s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"},
		Checks:             map[string]checksandalerts.NamedCheck{"service": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "app.service", FailWhen: "the_unit_is_not_active"}}}, "health": {Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "false", SuccessExitCodes: []int{0}}}}},
		RestartSystemdUnit: &checksandalerts.RestartSystemdUnit{SystemdUnit: "app.service", RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"health"}, Attempts: "3", MinTimeBetweenAttempts: "1ms", ResetAttemptsAfterHealthyFor: "10m"}},
		RecoveryCommand:    &checksandalerts.RecoveryCommand{Script: "echo command >> " + testhelpers.ShellQuote(log), TimeLimitPerAttempt: "5s", ExecOptions: testhelpers.OwnOptions(t), RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"health"}, Attempts: "3", MinTimeBetweenAttempts: "1m", ResetAttemptsAfterHealthyFor: "10m"}},
	}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	actions := func() string { b, _ := os.ReadFile(log); return string(b) }
	failNow(t, d, "web/health")
	d.workers.Wait()
	if actions() != "command\n" || !d.checks["web/health"].needsFresh() {
		t.Fatalf("expected only the command, then a fresh observation: %q", actions())
	}
	// The command is in its min_time_between_attempts; a fresh failure calls for the restart.
	failNow(t, d, "web/health")
	d.workers.Wait()
	if actions() != "command\nrestart\n" {
		t.Fatalf("restart did not follow a failure observed after the command: %q", actions())
	}
	w := httptest.NewRecorder()
	d.routes().ServeHTTP(w, httptest.NewRequest("POST", "http://localhost/reset-recovery-attempts/web", nil))
	if w.Code != http.StatusOK || d.snapshot.RecoveryAttemptsByTarget["web"].Attempts != 0 || d.snapshot.RecoveryAttemptsByTarget["app.service"].Attempts != 0 {
		t.Fatal("reset did not clear both budgets", w.Code, w.Body.String(), d.snapshot.RecoveryAttemptsByTarget)
	}
}

// A crash loop opens the restarts incident, with the count as evidence, and
// no MMWatchdog restart, since restarts is not in restart.triggered_by_checks. Diagnostics count on
// a copy of the history, an unchanged reload keeps it, and a daemon restart
// continues from the persisted history without a false recovery.
func TestRestartsConditionFollowsScheduledHistory(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	// The time limit of a run is far longer than the stand-in for systemctl
	// takes, also on a busy machine. Each scheduler tick of the test comes
	// an interval after the one before.
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {
		Timing:             checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "1m", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"},
		Checks:             map[string]checksandalerts.NamedCheck{"service": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "app.service", FailWhen: "the_unit_is_not_active"}}}, "crash_loop": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitCrashLoop{SystemdUnit: "app.service", AutomaticRestartsAbove: 1, AutomaticRestartsWithin: "1h"}}}},
		RestartSystemdUnit: &checksandalerts.RestartSystemdUnit{SystemdUnit: "app.service", RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"service"}, Attempts: "3", MinTimeBetweenAttempts: "1s", ResetAttemptsAfterHealthyFor: "10m"}},
	}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { d.Close() }()
	dir := t.TempDir()
	count, marker := filepath.Join(dir, "count"), filepath.Join(dir, "restarted")
	script := "#!/bin/sh\nif [ \"$1\" = show ]; then printf 'LoadState=loaded\\nActiveState=active\\nNRestarts=%s\\nResult=success\\nExecMainStatus=0\\n' \"$(cat " + testhelpers.ShellQuote(count) + ")\"; else touch " + testhelpers.ShellQuote(marker) + "; fi\n"
	testhelpers.WriteStandInProgram(t, filepath.Join(dir, "systemctl"), script)
	testhelpers.PutProgramsFirst(t, dir)
	now := time.Now().UTC()
	observe := func(restarts string) *incidents.CheckState {
		t.Helper()
		if err := os.WriteFile(count, []byte(restarts), 0600); err != nil {
			t.Fatal(err)
		}
		d.schedule(testhelpers.TestContext(t), now)
		d.workers.Wait()
		now = now.Add(time.Minute)
		return d.snapshot.Checks["app/crash_loop"]
	}
	if st := observe("7"); st.Last.Status != "ok" || st.Last.Message != "first look at systemd unit app.service: MMWatchdog counts the unit's automatic restarts between check runs, from the current run on; systemd's own count of the unit's automatic restarts (NRestarts) is 7" || st.Last.Measurements["automatic_restarts"].Value != 0 {
		t.Fatal(st.Last)
	}
	st := observe("9")
	if st.FailureIncident.Incident == "" || st.FailureIncident.Evidence.Observation.Measurements["automatic_restarts"].Value != 2 || st.FailureIncident.Evidence.Observation.Message != "2 automatic restarts in the last hour" {
		t.Fatalf("%+v", st.FailureIncident)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) || d.snapshot.Checks["app/service"].Last.Status != "ok" {
		t.Fatal("restarted a unit whose service check is healthy", err)
	}
	if err := os.WriteFile(count, []byte("10"), 0600); err != nil {
		t.Fatal(err)
	}
	obs, err := d.checkNowGroup(testhelpers.TestContext(t), "app")
	if err != nil || obs["crash_loop"].Status != "failed" || obs["crash_loop"].Message != "3 automatic restarts in the last hour" {
		t.Fatal(obs, err)
	}
	// The diagnostic left the scheduled history alone, so it counts the same restart.
	if st = observe("10"); st.Last.Message != "3 automatic restarts in the last hour" {
		t.Fatal(st.Last)
	}
	if _, err := d.reload(); err != nil {
		t.Fatal(err)
	}
	if st = observe("10"); st.Last.Message != "3 automatic restarts in the last hour" {
		t.Fatal("unchanged reload lost the history", st.Last)
	}
	incident := st.FailureIncident.Incident
	d.Close()
	if d, err = NewDaemon(p); err != nil {
		t.Fatal(err)
	}
	// The unit restarted once more while MMWatchdog was down; that counts at the
	// first observation, and the open incident stays open without a recovery.
	if st = observe("11"); st.Last.Message != "4 automatic restarts in the last hour" || st.FailureIncident.Incident != incident {
		t.Fatalf("%+v", st)
	}
	events, err := d.store.Events(100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == "recovered" {
			t.Fatalf("daemon restart manufactured a recovery: %+v", events)
		}
	}
	// A manual start lowers systemd's count; counting continues from it.
	if st = observe("0"); st.Last.Message != "4 automatic restarts in the last hour" || st.FailureIncident.Incident != incident {
		t.Fatalf("%+v", st)
	}
}

// Overdue evidence names the last success and the expected interval.
func TestHeartbeatOverdueEvidenceExplainsRule(t *testing.T) {
	t.Parallel()
	p, _ := heartbeatConfig(t)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	st := d.snapshot.Checks["backup/heartbeat"]
	started := st.Deadline.Add(-26 * time.Hour).Format(time.RFC3339)
	d.schedule(context.Background(), st.Deadline.Add(time.Second))
	st = d.snapshot.Checks["backup/heartbeat"]
	if m := st.FailureIncident.Evidence.Observation.Message; !strings.Contains(m, "no heartbeat since monitoring started "+started) || !strings.Contains(m, "expected within 26h") {
		t.Fatal("first-deadline evidence does not explain the rule:", m)
	}
	if code := postHeartbeat(d); code != http.StatusNoContent {
		t.Fatal(code)
	}
	st = d.snapshot.Checks["backup/heartbeat"]
	receipt := st.Receipt.Format(time.RFC3339)
	d.schedule(context.Background(), st.Deadline.Add(time.Second))
	st = d.snapshot.Checks["backup/heartbeat"]
	if m := st.FailureIncident.Evidence.Observation.Message; !strings.Contains(m, "last heartbeat "+receipt) || !strings.Contains(m, "expected within 26h") {
		t.Fatal("overdue evidence does not name the last success:", m)
	}
}

// Transitions held in memory during a storage outage stay within the
// outbox's size limit; the dropped ones leave a visible gap.
func TestBoundPendingLeavesAGap(t *testing.T) {
	t.Parallel()
	pending := []checksandalerts.Event{}
	for i := 0; i < 15; i++ {
		pending = append(pending, checksandalerts.Event{ID: incidents.ID(), Type: "failed", AlertSeverity: "warn", At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Notify: true})
	}
	one, _ := json.Marshal(pending[0])
	if kept := boundPending(pending, int64(15*len(one))); len(kept) != 15 {
		t.Fatal("transitions within the limit were dropped", len(kept))
	}
	bounded := boundPending(pending, int64(10*len(one)))
	if gap := bounded[len(bounded)-1]; len(bounded) != 11 || gap.Type != "delivery_gap" || !strings.HasPrefix(gap.Message, "5 alerts and status changes were lost because MMWatchdog could not save them to state.db") || bounded[0].ID != pending[5].ID {
		t.Fatalf("bounded to %d events ending %+v", len(bounded), gap)
	}
}

func maintenanceURL(group, duration string) string {
	action := "on"
	if duration == "off" {
		action = "off"
	}
	return "http://localhost/maintenance/" + action + "/" + group
}
func maintenanceBody(duration string) string {
	if duration == "off" {
		return "{}"
	}
	return `{"for":"` + duration + `"}`
}

func TestMaintenanceOnOffValidation(t *testing.T) {
	t.Parallel()
	directory, _ := daemonConfig(t)
	d, err := NewDaemon(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, tc := range []struct {
		action, body string
		code         int
	}{
		{"on", `{}`, 400}, {"on", `{"for":"0s"}`, 400}, {"on", `{"for":"-1s"}`, 400}, {"on", `{"for":"bad"}`, 400},
		{"off", `{"for":"1s"}`, 400}, {"off", `{"reason":"why"}`, 400}, {"wrong", `{}`, 400},
		// A window has no maximum length; d counts days.
		{"on", `{"for":"30d","reason":"repair","user":"matt"}`, 200}, {"on", `{"for":"1h"}`, 200}, {"off", `{}`, 200},
	} {
		before := d.snapshot.Clone()
		w := httptest.NewRecorder()
		d.routes().ServeHTTP(w, httptest.NewRequest("POST", "/maintenance/"+tc.action+"/app", strings.NewReader(tc.body)))
		if w.Code != tc.code {
			t.Fatalf("%s %s: %d %s", tc.action, tc.body, w.Code, w.Body.String())
		}
		if tc.code != 200 && !reflect.DeepEqual(before.Maintenance, d.snapshot.Maintenance) {
			t.Fatal("invalid request changed maintenance")
		}
	}
	events, err := d.store.CheckGroupEvents("app", 10)
	if err != nil || len(events) != 3 || events[0].Type != "maintenance_off" || !strings.Contains(events[0].Message, "turned off") {
		t.Fatal(events, err)
	}
	if len(d.snapshot.Maintenance) != 0 {
		t.Fatal(d.snapshot.Maintenance)
	}
}

func TestRecoveryAttemptStatusMaps(t *testing.T) {
	t.Parallel()
	directory, c := daemonConfig(t)
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {Checks: map[string]checksandalerts.NamedCheck{"service": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "app.service", FailWhen: "the_unit_is_not_active"}}}}, RestartSystemdUnit: &checksandalerts.RestartSystemdUnit{SystemdUnit: "app.service", RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{Attempts: "3"}}, RecoveryCommand: &checksandalerts.RecoveryCommand{Script: "true", TimeLimitPerAttempt: "1m", RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{Attempts: "2"}}}, "shared": {Checks: map[string]checksandalerts.NamedCheck{"service": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "app.service", FailWhen: "the_unit_is_not_active"}}}}, RestartSystemdUnit: &checksandalerts.RestartSystemdUnit{SystemdUnit: "app.service", RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{Attempts: "1"}}}}
	for _, group := range c.CheckGroups {
		for _, policy := range []*checksandalerts.RestartSystemdUnit{group.RestartSystemdUnit} {
			policy.Attempts = "3"
			policy.TriggeredByChecks = []string{"service"}
			policy.MinTimeBetweenAttempts = "1m"
			policy.ResetAttemptsAfterHealthyFor = "10m"
		}
		if command := group.RecoveryCommand; command != nil {
			command.TriggeredByChecks = []string{"service"}
			command.MinTimeBetweenAttempts = "1m"
			command.ResetAttemptsAfterHealthyFor = "10m"
		}
	}
	writeConfig(t, directory, c)
	d, err := NewDaemon(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	at := time.Now().UTC()
	d.snapshot.RecoveryAttemptsByTarget["app.service"] = &incidents.RecoveryAttempts{Attempts: 1, Last: at, LastResult: "restart requested"}
	d.snapshot.RecoveryAttemptsByTarget["app"] = &incidents.RecoveryAttempts{Attempts: 1, Last: at, LastFinished: at.Add(time.Second), LastResult: "exit 0"}
	w := httptest.NewRecorder()
	d.routes().ServeHTTP(w, httptest.NewRequest("GET", "/status", nil))
	var status map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"systemd_unit_restart_attempts": "app.service", "recovery_command_attempts": "app"} {
		var attempts map[string]RecoveryAttemptStatus
		if err := json.Unmarshal(status[key], &attempts); err != nil {
			t.Fatal(err)
		}
		entry := attempts[want]
		if key == "recovery_command_attempts" && !entry.LastAttemptFinishedAt.Equal(at.Add(time.Second)) {
			t.Fatal("the completed attempt has no completion time", entry)
		}
		if key == "systemd_unit_restart_attempts" && strings.Contains(string(status[key]), "last_attempt_finished_at") {
			t.Fatal("an attempt still running has a completion time", string(status[key]))
		}
		if len(attempts) != 1 || entry.AttemptsUsed != 1 || !entry.LastAttemptAt.Equal(at) || entry.MaxAttempts != map[string]checksandalerts.MaxAttempts{"app.service": "3", "app": "2"}[want] {
			t.Fatal(key, attempts)
		}
	}
	if _, ok := status["budgets"]; ok {
		t.Fatal("obsolete budgets status map exposed")
	}
}

// The restart has its own waiting time, counted from the first failed run
// like the alert's. With restart_after_failing_for shorter than
// alert_after_failing_for, MMWatchdog restarts the unit before any incident opens,
// and the alert still goes out when the failing time has lasted
// alert_after_failing_for, whatever the restarts did. A restart needs a
// failed run that is newer than the last restart.
func TestRestartBeforeTheAlert(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"myapp": {
		Timing: checksandalerts.Timing{CheckEvery: "30s", TimeLimitPerRun: "5s", AlertAfterFailingFor: "5m", RecoveredAfterHealthyFor: "30s"},
		Checks: map[string]checksandalerts.NamedCheck{"health": {Check: checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: "http://127.0.0.1:1/health"}}}},
		RestartSystemdUnit: &checksandalerts.RestartSystemdUnit{SystemdUnit: "myapp.service", RestartAfterFailingFor: "1m",
			RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"health"}, Attempts: "3", MinTimeBetweenAttempts: "1m", ResetAttemptsAfterHealthyFor: "10m"}},
	}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	dir := t.TempDir()
	log := filepath.Join(dir, "restarted")
	script := "#!/bin/sh\nif [ \"$1\" = show ]; then printf 'LoadState=loaded\\nActiveState=active\\n'; else echo restart >> " + testhelpers.ShellQuote(log) + "; fi\n"
	testhelpers.WriteStandInProgram(t, filepath.Join(dir, "systemctl"), script)
	testhelpers.PutProgramsFirst(t, dir)
	// The runs happened in the past, so that the restart, which reads the
	// clock, sees their waiting times as over.
	start := time.Now().UTC().Add(-10 * time.Minute)
	restarts := func() int {
		b, _ := os.ReadFile(log)
		return strings.Count(string(b), "restart")
	}
	// fail records one failed run at start+sec and lets a due restart
	// finish. recoveryDue judges the freshness of the run by the clock, so
	// the test gives it a current observation time for that moment.
	fail := func(sec int) []checksandalerts.Event {
		t.Helper()
		d.mu.Lock()
		st, rc := d.snapshot.Checks["myapp/health"], d.checks["myapp/health"]
		at := start.Add(time.Duration(sec) * time.Second)
		events := st.Observe(checksandalerts.Observation{Status: "failed", Message: "expected HTTP 200, received 503", At: at}, rc.def.Timing, false)
		rc.fresh = true
		st.Last.At = time.Now().UTC()
		if e := d.record(events); e != nil {
			t.Fatal(e)
		}
		d.startRecovery(testhelpers.TestContext(t), time.Now().UTC())
		d.mu.Unlock()
		d.workers.Wait()
		return events
	}
	if events := fail(0); len(events) != 0 {
		t.Fatal(events)
	}
	// The failing time began 10 minutes ago by the clock, so every waiting
	// time is over; what holds a restart back is a run newer than the last
	// one, min_time_between_attempts, and the attempts.
	if restarts() != 1 || d.snapshot.Checks["myapp/health"].FailureIncident.Incident != "" {
		t.Fatal("the restart did not come before the incident", restarts(), d.snapshot.Checks["myapp/health"].FailureIncident)
	}
	if !d.snapshot.Checks["myapp/health"].FailingSince.Equal(start) {
		t.Fatal("a finished restart ended the failing time", d.snapshot.Checks["myapp/health"].FailingSince)
	}
	// Space the attempts: move the last attempt back past min_time_between_attempts.
	spaced := func() {
		d.mu.Lock()
		b := d.snapshot.RecoveryAttemptsByTarget["myapp.service"]
		b.Last, b.LastFinished = time.Now().UTC().Add(-3*time.Minute), time.Now().UTC().Add(-2*time.Minute)
		d.mu.Unlock()
	}
	spaced()
	fail(60)
	spaced()
	fail(120)
	spaced()
	fail(180)
	if restarts() != 3 || d.snapshot.RecoveryAttemptsByTarget["myapp.service"].Attempts != 3 {
		t.Fatal("expected three restart attempts and no more", restarts(), d.snapshot.RecoveryAttemptsByTarget["myapp.service"])
	}
	// 5 minutes after the first failed run the incident opens, with all
	// attempts used.
	events := fail(300)
	if len(events) != 1 || events[0].Type != "failed" || restarts() != 3 {
		t.Fatal(events, restarts())
	}
	recorded, _ := d.store.CheckGroupEvents("myapp", 20)
	requested := 0
	for _, event := range recorded {
		if event.Type == "systemd_unit_restart_requested" {
			requested++
		}
	}
	if requested != 3 {
		t.Fatal("restart events", requested)
	}
}

// A recovery action of a check waits for the check's own runs, the runs that
// open its incident. Another check of the check group runs between them, and
// its run decides nothing about the failing check. So the action comes with
// the first run of the failing check at or after the action's waiting time:
// before the alert with restart_after_failing_for or run_after_failing_for
// shorter than alert_after_failing_for, with the same run as the alert when
// both are equal, and after the alert when the action waits longer.
func TestARecoveryActionWaitsForTheRunsOfItsOwnCheck(t *testing.T) {
	t.Parallel()
	const failing = "nginx/nginx_cpu_runaway"
	// The check that fails runs every 1.1 seconds, and the other check of
	// its check group 0.9 seconds after each of those runs.
	type run struct {
		key, status string
		after       time.Duration
	}
	runs := []run{}
	for i := range 7 {
		own := time.Duration(i) * 1100 * time.Millisecond
		runs = append(runs, run{failing, "failed", own}, run{"nginx/nginx_answers", "ok", own + 900*time.Millisecond})
	}
	// The incident opens with the failing check's run at 4.4 seconds, the
	// first of its runs at or after alert_after_failing_for.
	const alertAfter, incidentAt = 4 * time.Second, 4400 * time.Millisecond
	waits := []struct{ wait, actionAt time.Duration }{
		{2 * time.Second, 2200 * time.Millisecond},
		{alertAfter, incidentAt},
		{6 * time.Second, 6600 * time.Millisecond},
	}
	for _, action := range []string{"restart", "recovery command"} {
		for _, w := range waits {
			t.Run(action+" after "+w.wait.String(), func(t *testing.T) {
				t.Parallel()
				p, c := daemonConfig(t)
				dir := t.TempDir()
				testhelpers.WriteStandInProgram(t, filepath.Join(dir, "systemctl"), "#!/bin/sh\nif [ \"$1\" = show ]; then printf 'LoadState=loaded\\nActiveState=active\\n'; fi\n")
				testhelpers.PutProgramsFirst(t, dir)
				// One action for the failing time: min_time_between_attempts
				// is far longer than the test.
				attempts := checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"nginx_cpu_runaway"}, Attempts: "3", MinTimeBetweenAttempts: "1h", ResetAttemptsAfterHealthyFor: "10m"}
				group := checksandalerts.CheckGroup{
					Timing: checksandalerts.Timing{CheckEvery: "1s", TimeLimitPerRun: "1s", AlertAfterFailingFor: checksandalerts.Duration(alertAfter.String()), RecoveredAfterHealthyFor: "4s"},
					Checks: map[string]checksandalerts.NamedCheck{
						"nginx_cpu_runaway": {Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "false", SuccessExitCodes: []int{0}}}},
						"nginx_answers":     {Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true", SuccessExitCodes: []int{0}}}},
					},
				}
				requested := "systemd_unit_restart_requested"
				if action == "restart" {
					group.RestartSystemdUnit = &checksandalerts.RestartSystemdUnit{SystemdUnit: "nginx.service", RestartAfterFailingFor: checksandalerts.Duration(w.wait.String()), RecoveryAttemptSettings: attempts}
				} else {
					requested = "recovery_command_started"
					group.RecoveryCommand = &checksandalerts.RecoveryCommand{Script: "true", RunAfterFailingFor: checksandalerts.Duration(w.wait.String()), TimeLimitPerAttempt: "10s", ExecOptions: testhelpers.OwnOptions(t), RecoveryAttemptSettings: attempts}
				}
				c.CheckGroups = map[string]checksandalerts.CheckGroup{"nginx": group}
				writeConfig(t, p, c)
				clock := testhelpers.NewClock()
				d := newDaemonOnClock(t, p, clock)
				passed := time.Duration(0)
				for _, r := range runs {
					clock.Pass(r.after - passed)
					passed = r.after
					acceptRun(t, d, r.key, r.status, clock.Now())
					d.workers.Wait()
					d.mu.Lock()
					incidentOpen := d.snapshot.Checks[failing].FailureIncident.Incident != ""
					d.mu.Unlock()
					events, err := d.store.CheckGroupEvents("nginx", 100)
					if err != nil {
						t.Fatal(err)
					}
					actions := 0
					for _, event := range events {
						if event.Type == requested {
							actions++
						}
					}
					if wantActions := map[bool]int{false: 0, true: 1}[r.after >= w.actionAt]; actions != wantActions || incidentOpen != (r.after >= incidentAt) {
						t.Fatalf("after the run of %s at %s: %d %s events and the incident open %t; want the %s with the run of %s at %s, and the incident with its run at %s", r.key, r.after, actions, requested, incidentOpen, action, failing, w.actionAt, incidentAt)
					}
				}
			})
		}
	}
}

// MMWatchdog does not start a stopped unit. For a unit that systemd reports as
// inactive, MMWatchdog withholds the restart, uses no attempt, and records that
// once for the failing time. A unit that systemd reports as failed is
// restarted.
func TestAStoppedUnitStaysStopped(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"myapp": {
		Timing:             checksandalerts.Timing{CheckEvery: "30s", TimeLimitPerRun: "5s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"},
		Checks:             map[string]checksandalerts.NamedCheck{"running": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "myapp.service", FailWhen: "the_unit_is_not_active"}}}},
		RestartSystemdUnit: &checksandalerts.RestartSystemdUnit{SystemdUnit: "myapp.service", RestartAfterFailingFor: "0s", RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"running"}, Attempts: "3", MinTimeBetweenAttempts: "1ms", ResetAttemptsAfterHealthyFor: "10m"}},
	}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	dir := t.TempDir()
	state, log, asked := filepath.Join(dir, "state"), filepath.Join(dir, "restarted"), filepath.Join(dir, "asked")
	script := "#!/bin/sh\nif [ \"$1\" = show ]; then echo asked >> " + testhelpers.ShellQuote(asked) + "; printf 'LoadState=loaded\\nActiveState=%s\\nResult=success\\n' \"$(cat " + testhelpers.ShellQuote(state) + ")\"; else echo restart >> " + testhelpers.ShellQuote(log) + "; fi\n"
	testhelpers.WriteStandInProgram(t, filepath.Join(dir, "systemctl"), script)
	testhelpers.PutProgramsFirst(t, dir)
	observe := func(status string) {
		t.Helper()
		d.mu.Lock()
		st, rc := d.snapshot.Checks["myapp/running"], d.checks["myapp/running"]
		now := time.Now().UTC()
		events := st.Observe(checksandalerts.Observation{Status: status, Message: status, At: now}, rc.def.Timing, false)
		rc.fresh = true
		if e := d.record(events); e != nil {
			t.Fatal(e)
		}
		d.startRecovery(testhelpers.TestContext(t), now)
		d.mu.Unlock()
		d.workers.Wait()
	}
	withheld := func() []checksandalerts.Event {
		t.Helper()
		events, e := d.store.CheckGroupEvents("myapp", 50)
		if e != nil {
			t.Fatal(e)
		}
		out := []checksandalerts.Event{}
		for _, event := range events {
			if event.Type == "systemd_unit_restart_withheld" {
				out = append(out, event)
			}
		}
		return out
	}
	os.WriteFile(state, []byte("inactive"), 0600)
	observe("failed")
	// Observations of other checks on the node decide recovery too. They do
	// not make MMWatchdog ask systemd about the stopped unit again: the next failed
	// observation of the unit's own check does.
	for range 5 {
		d.mu.Lock()
		d.startRecovery(testhelpers.TestContext(t), time.Now().UTC())
		d.mu.Unlock()
		d.workers.Wait()
	}
	if b, _ := os.ReadFile(asked); string(b) != "asked\n" {
		t.Fatalf("MMWatchdog asked systemd about the stopped unit %q, want once for one failed observation", b)
	}
	observe("failed")
	observe("failed")
	if b, _ := os.ReadFile(asked); string(b) != "asked\nasked\nasked\n" {
		t.Fatalf("MMWatchdog asked systemd about the stopped unit %q, want once for each failed observation", b)
	}
	events := withheld()
	if _, e := os.Stat(log); !os.IsNotExist(e) || len(events) != 1 || events[0].Check != "running" || events[0].Message != "no restart: systemd unit myapp.service is stopped, and MMWatchdog never starts a stopped unit" {
		t.Fatalf("MMWatchdog started a stopped unit, or did not record the withheld restart once: %v %+v", e, events)
	}
	if attempts := d.snapshot.RecoveryAttemptsByTarget["myapp.service"]; attempts == nil || attempts.Attempts != 0 {
		t.Fatal("a withheld restart used an attempt", attempts)
	}
	// The stopped unit still alerts: its check failed and the incident is open.
	if d.snapshot.Checks["myapp/running"].FailureIncident.Incident == "" {
		t.Fatal("a stopped unit raised no incident")
	}
	// A new failing time records the withheld restart again.
	observe("ok")
	observe("failed")
	if events = withheld(); len(events) != 2 {
		t.Fatal("a new failing time did not record its withheld restart", events)
	}
	// A unit that failed is restarted.
	os.WriteFile(state, []byte("failed"), 0600)
	observe("failed")
	if b, _ := os.ReadFile(log); string(b) != "restart\n" || d.snapshot.RecoveryAttemptsByTarget["myapp.service"].Attempts != 1 {
		t.Fatalf("a failed unit was not restarted: %q %+v", b, d.snapshot.RecoveryAttemptsByTarget["myapp.service"])
	}
}

// With max_attempts: unlimited MMWatchdog never stops restarting, and status names
// the limit as unlimited. Every check group that restarts one unit states
// the same budget.
func TestUnlimitedAttempts(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	restart := func(attempts checksandalerts.MaxAttempts) *checksandalerts.RestartSystemdUnit {
		return &checksandalerts.RestartSystemdUnit{SystemdUnit: "app.service", RestartAfterFailingFor: "0s", RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"service"}, Attempts: attempts, MinTimeBetweenAttempts: "1m", ResetAttemptsAfterHealthyFor: "10m"}}
	}
	checks := map[string]checksandalerts.NamedCheck{"service": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "app.service", FailWhen: "the_unit_is_not_active"}}}}
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {Checks: checks, RestartSystemdUnit: restart("unlimited")}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Now()
	st := d.snapshot.Checks["app/service"]
	st.FailingSince, st.Last = now, checksandalerts.Observation{Status: "failed", At: now}
	d.checks["app/service"].fresh = true
	d.snapshot.RecoveryAttemptsByTarget["app.service"] = &incidents.RecoveryAttempts{Attempts: 5000, Last: now.Add(-10 * time.Minute), LastFinished: now.Add(-2 * time.Minute)}
	if d.recoveryDue("app.service", now, "failed") == "" {
		t.Fatal("unlimited attempts ran out")
	}
	d.snapshot.RecoveryAttemptsByTarget["app.service"].LastFinished = now.Add(-30 * time.Second)
	if d.recoveryDue("app.service", now, "failed") != "" {
		t.Fatal("unlimited attempts ignored min_time_between_attempts")
	}
	units, _ := d.recoveryAttemptStatus("")
	if b, _ := json.Marshal(units["app.service"]); !strings.Contains(string(b), `"attempts_used":5000,"max_attempts":"unlimited"`) {
		t.Fatal(string(b))
	}
	// A second check group must state the same unlimited budget.
	c.CheckGroups["peer"] = checksandalerts.CheckGroup{Checks: checks, RestartSystemdUnit: restart("unlimited")}
	writeConfig(t, p, c)
	if _, err = d.reload(); err != nil {
		t.Fatal(err)
	}
	units, _ = d.recoveryAttemptStatus("")
	if b, _ := json.Marshal(units["app.service"]); d.policies["app.service"].Attempts != checksandalerts.NoAttemptLimit || !strings.Contains(string(b), `"max_attempts":"unlimited"`) {
		t.Fatal(d.policies["app.service"].Attempts, string(b))
	}
	var status RecoveryAttemptStatus
	if e := json.Unmarshal([]byte(`{"attempts_used":4,"max_attempts":"unlimited"}`), &status); e != nil || status.MaxAttempts != "unlimited" {
		t.Fatal(status, e)
	}
	if e := json.Unmarshal([]byte(`{"attempts_used":1,"max_attempts":3}`), &status); e != nil || status.MaxAttempts != "3" {
		t.Fatal(status, e)
	}
}

// One maintenance command names several check groups: each gets its own
// window with the same end, and the command sends one maintenance_on alert
// that names them. Turning them off together, or their expiring together,
// sends one maintenance_off alert. Windows from different commands end with
// one alert each.
func TestMaintenanceForSeveralCheckGroups(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "http://127.0.0.1:9/hook"}, TimeLimitPerDeliveryAttempt: "5s"}}
	for _, name := range []string{"worker_1", "worker_2", "worker_3"} {
		c.CheckGroups[name] = checksandalerts.CheckGroup{Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true"}}}
	}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	post := func(action, names, body string) (int, string) {
		t.Helper()
		w := httptest.NewRecorder()
		d.routes().ServeHTTP(w, httptest.NewRequest("POST", "http://localhost/maintenance/"+action+"/"+names, strings.NewReader(body)))
		return w.Code, w.Body.String()
	}
	if code, body := post("on", "worker_1,worker_2,worker_3", `{"for":"10m","reason":"deploy v1.4","user":"matt"}`); code != 200 || !strings.Contains(body, `"check_groups":["worker_1","worker_2","worker_3"]`) {
		t.Fatal(code, body)
	}
	windows := d.snapshot.Maintenance
	if len(windows) != 3 || windows["worker_1"] != windows["worker_3"] || windows["worker_2"].Reason != "deploy v1.4" || d.suppressed("app") || !d.suppressed("worker_2") {
		t.Fatal(windows)
	}
	alerts := queuedEvents(t, d)
	if len(alerts) != 1 || alerts[0].Type != "maintenance_on" || alerts[0].CheckGroup != "" || !reflect.DeepEqual(alerts[0].CheckGroups, []string{"worker_1", "worker_2", "worker_3"}) || alerts[0].Maintenance.User != "matt" {
		t.Fatalf("one command did not send one alert that names its check groups: %+v", alerts)
	}
	// The event belongs to the history of each of its check groups.
	for _, name := range []string{"worker_1", "worker_3"} {
		if events, e := d.store.CheckGroupEvents(name, 10); e != nil || len(events) != 1 || events[0].ID != alerts[0].ID {
			t.Fatal(name, events, e)
		}
	}
	if events, _ := d.store.CheckGroupEvents("app", 10); len(events) != 0 {
		t.Fatal("the event reached a check group the command did not name", events)
	}
	// A second command replaces the window of one of them.
	if code, body := post("on", "worker_3", `{"for":"1h","reason":"repair"}`); code != 200 {
		t.Fatal(code, body)
	}
	// Turning all three off ends two windows of the first command together
	// and the window of the second command by itself.
	if code, body := post("off", "worker_1,worker_2,worker_3", `{}`); code != 200 {
		t.Fatal(code, body)
	}
	alerts = queuedEvents(t, d)
	if len(alerts) != 4 || alerts[2].Type != "maintenance_off" || !reflect.DeepEqual(alerts[2].CheckGroups, []string{"worker_1", "worker_2"}) || alerts[2].Message != "maintenance turned off: deploy v1.4" ||
		alerts[3].CheckGroup != "worker_3" || !reflect.DeepEqual(alerts[3].CheckGroups, []string{"worker_3"}) || len(d.snapshot.Maintenance) != 0 {
		t.Fatalf("%+v", alerts[2:])
	}
	// Windows of one command that expire at the same moment send one alert.
	if code, body := post("on", "worker_1,worker_2", `{"for":"10m"}`); code != 200 {
		t.Fatal(code, body)
	}
	d.schedule(context.Background(), time.Now().UTC().Add(11*time.Minute))
	d.workers.Wait()
	alerts = queuedEvents(t, d)
	if last := alerts[len(alerts)-1]; len(alerts) != 6 || last.Type != "maintenance_off" || last.Message != "maintenance expired" || !reflect.DeepEqual(last.CheckGroups, []string{"worker_1", "worker_2"}) || len(d.snapshot.Maintenance) != 0 {
		t.Fatalf("%+v", alerts[4:])
	}
	// The whole-node window stands alone, and every name must be a check group.
	for names, want := range map[string]int{"every_check_group,worker_1": 400, "worker_1,absent": 404, "every_check_group": 200, "worker_1,worker_1": 200} {
		if code, body := post("on", names, `{"for":"10m"}`); code != want {
			t.Fatal(names, code, body)
		}
	}
	// Turning off windows that do not exist changes nothing.
	if code, body := post("off", "worker_2,worker_3", `{}`); code != 200 || !strings.Contains(body, `"maintenance":null`) {
		t.Fatal(code, body)
	}
	// MMHotline's title names the check groups of the command.
	notice := store.MMHotlineNotice(incidents.MaintenanceEvent("maintenance_on", []string{"worker_1", "worker_2"}, checksandalerts.Window{Until: time.Now().Add(time.Hour)}, time.Now(), "maintenance on"), store.QueuedBy{Role: store.StandaloneSender, Node: "web-3"})
	if notice.MMHotlineAlert.Title != ": maintenance on for worker_1, worker_2" {
		t.Fatal(notice.MMHotlineAlert.Title)
	}
}

// When the check group has been healthy for reset_attempts_after_healthy_for,
// MMWatchdog counts no attempt used, and shows no result of the last attempt:
// "0 of 3 attempts used" beside "restart finished" would read as an attempt
// that is counted.
func TestRefilledRecoveryAttemptsShowNoResultOfTheLastAttempt(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {
		Checks: map[string]checksandalerts.NamedCheck{"service": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "app.service", FailWhen: "the_unit_is_not_active"}}}}, Timing: checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "30s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"},
		RestartSystemdUnit: &checksandalerts.RestartSystemdUnit{SystemdUnit: "app.service", RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"service"}, Attempts: "3", MinTimeBetweenAttempts: "1s", ResetAttemptsAfterHealthyFor: "10m"}},
	}}
	writeConfig(t, path, c)
	d, err := NewDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Now().UTC()
	d.snapshot.RecoveryAttemptsByTarget["app.service"] = &incidents.RecoveryAttempts{Attempts: 2, Last: now.Add(-time.Hour), LastResult: "systemd unit restart finished. The next check runs will show whether the check has recovered"}
	st := d.snapshot.Checks["app/service"]
	st.Observe(checksandalerts.Observation{Status: "ok", At: now.Add(-time.Second)}, st.Timing, false)
	st.HealthySince = now.Add(-11 * time.Minute)
	d.mu.Lock()
	d.startRecovery(testhelpers.TestContext(t), now)
	d.mu.Unlock()
	d.workers.Wait()
	units, _ := d.recoveryAttemptStatus("")
	if got := units["app.service"]; got.AttemptsUsed != 0 || got.LastResult != "" {
		t.Fatalf("after the refill: %d attempts used, last result %q; want 0 and none", got.AttemptsUsed, got.LastResult)
	}
}

// status for one check group shows the attempts of that check group only.
func TestStatusOfOneCheckGroupShowsOnlyItsRecoveryAttempts(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	service := map[string]checksandalerts.NamedCheck{"service": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitState{SystemdUnit: "app.service", FailWhen: "the_unit_is_not_active"}}}}
	timing := checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "30s", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}
	c.CheckGroups = map[string]checksandalerts.CheckGroup{
		"app":   {Checks: service, Timing: timing, RestartSystemdUnit: &checksandalerts.RestartSystemdUnit{SystemdUnit: "app.service", RecoveryAttemptSettings: checksandalerts.RecoveryAttemptSettings{TriggeredByChecks: []string{"service"}, Attempts: "3", MinTimeBetweenAttempts: "1s", ResetAttemptsAfterHealthyFor: "10m"}}},
		"other": {Checks: map[string]checksandalerts.NamedCheck{"files": {Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true", SuccessExitCodes: []int{0}}}}}, Timing: timing},
	}
	writeConfig(t, path, c)
	d, err := NewDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for group, want := range map[string]int{"app": 1, "other": 0, "": 1} {
		w := httptest.NewRecorder()
		d.routes().ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/status?check_group="+group, nil))
		var status struct {
			Attempts map[string]RecoveryAttemptStatus `json:"systemd_unit_restart_attempts"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || w.Code != 200 || len(status.Attempts) != want {
			t.Errorf("status of %q: code %d, %d unit restart attempts, want %d: %v %s", group, w.Code, len(status.Attempts), want, err, w.Body.String())
		}
	}
}

// A maintenance request that is refused names only the field that is wrong.
func TestMaintenanceRequestNamesOnlyTheFieldThatIsWrong(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true", SuccessExitCodes: []int{0}}}}}
	writeConfig(t, path, c)
	d, err := NewDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for name, tc := range map[string]struct{ body, mentions, doesNotMention string }{
		"reason too long": {`{"for":"1h","reason":"` + strings.Repeat("r", 201) + `","user":"alice"}`, "maintenance reason must be single-line text of at most 200 bytes", "user"},
		"user too long":   {`{"for":"1h","reason":"patching","user":"` + strings.Repeat("u", 65) + `"}`, "maintenance user must be single-line text of at most 64 bytes", "reason"},
	} {
		w := httptest.NewRecorder()
		d.routes().ServeHTTP(w, httptest.NewRequest("POST", "http://localhost/maintenance/on/app", strings.NewReader(tc.body)))
		if w.Code != 400 || !strings.Contains(w.Body.String(), tc.mentions) || strings.Contains(w.Body.String(), tc.doesNotMention) {
			t.Errorf("%s: code %d, %s; want 400 naming %q only", name, w.Code, w.Body.String(), tc.mentions)
		}
	}
}
