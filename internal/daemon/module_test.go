package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/testhelpers"
)

// Every core module has a name that no other module and no setting has, and
// an input type of its own. So a mapping never leaves a doubt about which
// of its keys is the module.
func TestEveryCoreModuleIsRegisteredOnce(t *testing.T) {
	t.Parallel()
	type registered struct {
		name   string
		inputs reflect.Type
	}
	for what, registry := range map[string]struct {
		modules  []registered
		settings []reflect.Type
	}{
		"check": {func() (m []registered) {
			for _, module := range checksandalerts.CoreCheckModules {
				m = append(m, registered{module.Name, reflect.TypeOf(module.NewInputs())})
			}
			return m
		}(), []reflect.Type{reflect.TypeOf(checksandalerts.CheckGroup{}), reflect.TypeOf(checksandalerts.NamedCheck{})}},
		"destination": {func() (m []registered) {
			for _, module := range checksandalerts.CoreDestinationModules {
				m = append(m, registered{module.Name, reflect.TypeOf(module.NewInputs())})
			}
			return m
		}(), []reflect.Type{reflect.TypeOf(checksandalerts.Destination{RemindEvery: "never"})}},
	} {
		names, types := map[string]bool{}, map[reflect.Type]bool{}
		for _, module := range registry.modules {
			if !checksandalerts.NamePattern.MatchString(module.name) || names[module.name] {
				t.Errorf("the %s module %q has no name of its own", what, module.name)
			}
			if module.inputs.Kind() != reflect.Pointer || types[module.inputs] {
				t.Errorf("the %s module %s has no input type of its own: %s", what, module.name, module.inputs)
			}
			names[module.name], types[module.inputs] = true, true
			for _, settings := range registry.settings {
				if _, isSetting := checksandalerts.Fields(settings)[module.name]; isSetting {
					t.Errorf("the %s module %s has the name of a setting of %s", what, module.name, settings.Name())
				}
			}
		}
	}
	if checksandalerts.NoCheckModule.Name != "" || (checksandalerts.Check{}).ModuleName() != "" || (checksandalerts.Destination{RemindEvery: "never"}).ModuleName() != "" {
		t.Error("a check or a destination without a module has the name of one")
	}
}

// A check and a destination name exactly one module. Two modules are an
// error that names both and the line; none is an error too.
func TestACheckAndADestinationNameExactlyOneModule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ groups, want string }{
		{"check_groups: {a: {http: {url: 'http://127.0.0.1/'}, tcp: '127.0.0.1:80'}}", "checks.yml:1:16: check_groups.a: names the modules http and tcp: a check names exactly one module"},
		{"check_groups:\n  a:\n    checks:\n      both:\n        shell: 'true'\n        systemd_unit_state:\n          systemd_unit: a.service\n          fail_when: the_unit_is_not_active\n", "checks.yml:4:7: check_groups.a.checks.both: names the modules shell and systemd_unit_state: a check names exactly one module"},
		{"check_groups: {a: {checks: {none: {alert_severity: warn}}}}", "check_groups.a.checks.none: each check needs a valid name and exactly one module, such as http or shell"},
		{"check_groups: {a: {checks: {typo: {htpp: {url: 'http://127.0.0.1/'}}}}}", `checks.yml:1:36: check_groups.a.checks.typo.htpp: unknown field "htpp": the field name matches neither a setting of a check nor an installed module. MMWatchdog has no core module of that name, and no directory of that name is in `},
	} {
		if _, e := config.Load(testhelpers.ConfigFile(t, tc.groups)); e == nil || !strings.Contains(e.Error(), tc.want) {
			t.Errorf("%s:\n got %v\nwant %q", tc.groups, e, tc.want)
		}
	}
	directory := testhelpers.ConfigFile(t, "check_groups: {a: {shell: 'true'}}")
	for destinations, want := range map[string]string{
		"destinations:\n  ops:\n    remind_every: never\n    webhook: {url: 'https://example.com'}\n    email: {smtp_server: 'localhost:25'}\n    alert_severities: [sos]\n": "alert_destinations.yml:2:3: destinations.ops: names the modules webhook and email: a destination names exactly one module",
		"destinations: {ops: {remind_every: never, alert_severities: [sos]}}\n":                                                                                              "destinations.ops: expected one module: webhook, email, system_log, or mmhotline",
		// A name that is no setting is the name of a module, and this node
		// has no module slack.
		"destinations: {ops: {remind_every: never, slack: {webhook_url_file: /etc/mmwatchdog/secrets/slack}, alert_severities: [sos]}}\n": `alert_destinations.yml:1:43: destinations.ops.slack: unknown field "slack": the field name matches neither a setting of a destination nor an installed module. MMWatchdog has no core module of that name, and no directory of that name is in `,
	} {
		if e := os.WriteFile(filepath.Join(directory, "alert_destinations.yml"), []byte(destinations), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := config.Load(directory); e == nil || !strings.Contains(e.Error(), want) {
			t.Errorf("%s:\n got %v\nwant %q", destinations, e, want)
		}
	}
}

// JSON shows a check and a destination as the configuration writes them:
// the module's name with its inputs, beside the settings. A module that is
// not core has its run settings beside its inputs. A destination that was
// written as JSON reads back as the same destination, because a queued
// delivery and a joined node keep theirs that way.
func TestJSONShowsACheckAndADestinationAsTheConfigurationWritesThem(t *testing.T) {
	t.Parallel()
	c, e := config.Load(testhelpers.ConfigFile(t, `check_groups:
  api: {http: {url: 'http://127.0.0.1/health'}, alert_severity: sos}
  machine:
    checks:
      root_filesystem_is_below_90_percent: {node_filesystem: /, fail_when_measurement_above: {space_used_percent: 90}, alert_severity: warn, alert_after_failing_for: 10m}
      myapp_service_is_running: {systemd_unit_state: {systemd_unit: myapp.service, fail_when: the_unit_is_not_active}, alert_severity: sos}
`))
	if e != nil {
		t.Fatal(e)
	}
	written, e := json.Marshal(c.CheckGroups)
	if e != nil {
		t.Fatal(e)
	}
	var read map[string]map[string]any
	if e := json.Unmarshal(written, &read); e != nil {
		t.Fatal(e, string(written))
	}
	direct := read["api"]["checks"].(map[string]any)["http"].(map[string]any)
	if direct["http"].(map[string]any)["url"] != "http://127.0.0.1/health" || direct["alert_severity"] != "sos" || read["api"]["check_every"] != "30s" {
		t.Fatalf("a check that is written directly on its check group: %s", written)
	}
	named := read["machine"]["checks"].(map[string]any)
	filesystem := named["root_filesystem_is_below_90_percent"].(map[string]any)
	if filesystem["node_filesystem"] != "/" || filesystem["alert_after_failing_for"] != "10m" || filesystem["alert_after_failing_for_stated_on"] != "check" ||
		filesystem["fail_when_measurement_above"].(map[string]any)["space_used_percent"] != 90.0 || named["myapp_service_is_running"].(map[string]any)["systemd_unit_state"].(map[string]any)["systemd_unit"] != "myapp.service" {
		t.Fatalf("named checks: %s", written)
	}
	// The identity of a check is the fingerprint of this JSON without the
	// settings that are not part of it, so it must hold the module.
	definitions := config.CheckDefinitions(c)
	other := definitions["api/http"]
	other.Check.Inputs = &checksandalerts.HTTP{URL: "http://127.0.0.1/ready", Method: "GET", ExpectedHTTPStatusCodes: []int{200}}
	if definitions["api/http"].Identity() == other.Identity() {
		t.Fatal("another input of the module kept the check's identity")
	}

	for name, destination := range map[string]checksandalerts.Destination{
		"webhook":   {RemindEvery: "never", Inputs: &checksandalerts.Webhook{URL: "https://example.com/mmwatchdog", BearerTokenFile: "/etc/mmwatchdog/secrets/webhook"}, TimeLimitPerDeliveryAttempt: "5s", AlertSeverities: []string{"sos"}},
		"email":     {Inputs: &checksandalerts.Email{SMTPServer: "smtp.example.com:587", SMTPConnectionSecurity: checksandalerts.SMTPStartTLS, From: "mmwatchdog@example.com", To: []string{"ops@example.com"}}, RemindEvery: "4h", AlertSeverities: []string{}},
		"slack":     {RemindEvery: "never", Inputs: &checksandalerts.ModuleCall{Name: "slack", Inputs: map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/slack", "mention": "<@U024BE7LH>"}, ExecOptions: primitives.ExecOptions{RunAsUserAccount: "alerts", RunFromDirectory: "/var/lib/alerts", Env: map[string]string{"HTTPS_PROXY": "http://proxy.example.com:3128"}}}, SendOnlyTheAlertTitle: true, AlertSeverities: testhelpers.AllSeverities},
		"mmhotline": {Inputs: &checksandalerts.MMHotlineDestination{URL: "https://hotline.example.com", AlertSourceID: testhelpers.TestSourceID, SigningSecretFile: "/etc/mmwatchdog/secrets/mmhotline"}, TimeLimitPerDeliveryAttempt: "5s", AlertSeverities: testhelpers.AllSeverities},
		"":          {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities},
	} {
		written, e := json.Marshal(destination)
		if e != nil {
			t.Fatal(name, e)
		}
		if name != "" && !strings.HasPrefix(string(written), `{"`+name+`":{`) {
			t.Errorf("the JSON of a %s destination does not start with its module: %s", name, written)
		}
		var read checksandalerts.Destination
		if e := json.Unmarshal(written, &read); e != nil {
			t.Fatal(name, e)
		}
		if read.ModuleName() != name || !checksandalerts.SameDestination(destination, read) || !reflect.DeepEqual(read, destination) {
			t.Errorf("a %s destination read back as %+v from %s", name, read, written)
		}
	}
}

// A core module keeps its state with the check: a scheduled run that MMWatchdog
// accepted stores it, and a run of "mmwatchdog check-now" starts from it and stores
// nothing.
func TestTheStateOfAModuleIsStoredOnlyByAnAcceptedScheduledRun(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	shown := filepath.Join(directory, "show")
	testhelpers.WriteStandInProgram(t, filepath.Join(directory, "systemctl"), "#!/bin/sh\ncat "+testhelpers.ShellQuote(shown)+"\n")
	testhelpers.PutProgramsFirst(t, directory)
	restarts := func(count string) {
		t.Helper()
		if e := os.WriteFile(shown, []byte("LoadState=loaded\nNRestarts="+count+"\nResult=success\nExecMainStatus=0\n"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	path, c := daemonConfig(t)
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {Check: checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitCrashLoop{SystemdUnit: "app.service", AutomaticRestartsAbove: 5, AutomaticRestartsWithin: "1h"}}}}
	writeConfig(t, path, c)
	d, e := NewDaemon(path)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	const key = "app/systemd_unit_crash_loop"
	stored := func() checksandalerts.RestartHistory {
		t.Helper()
		d.mu.Lock()
		defer d.mu.Unlock()
		var history checksandalerts.RestartHistory
		if state := d.snapshot.Checks[key].ModuleState; len(state) > 0 {
			if e := json.Unmarshal(state, &history); e != nil {
				t.Fatal(e)
			}
		}
		return history
	}
	scheduled := func() checksandalerts.Observation {
		t.Helper()
		d.mu.Lock()
		rc := d.checks[key]
		rc.memory, rc.running = &checksandalerts.CheckMemory{}, true
		d.running++
		d.workers.Add(1)
		d.mu.Unlock()
		d.observe(testhelpers.TestContext(t), key, rc, rc.epoch)
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.snapshot.Checks[key].Last
	}
	restarts("4")
	if o := scheduled(); o.Status != "ok" || stored().Count != 4 || !stored().Started {
		t.Fatalf("the first scheduled run: %+v %+v", o, stored())
	}
	// A run of check-now sees two more restarts and stores nothing.
	restarts("6")
	observations, e := d.checkNowGroup(testhelpers.TestContext(t), "app")
	if o := observations["systemd_unit_crash_loop"]; e != nil || o.Message != "2 automatic restarts in the last hour" || stored().Count != 4 || len(stored().Batches) != 0 {
		t.Fatalf("a run of check-now: %+v %v %+v", o, e, stored())
	}
	if o := scheduled(); o.Message != "2 automatic restarts in the last hour" || stored().Count != 6 || len(stored().Batches) != 1 {
		t.Fatalf("the next scheduled run: %+v %+v", o, stored())
	}
	// The state survives a restart of MMWatchdog, and a changed check starts afresh.
	d.Close()
	if d, e = NewDaemon(path); e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	if stored().Count != 6 {
		t.Fatalf("after a restart of MMWatchdog: %+v", stored())
	}
	c.CheckGroups["app"].Inputs.(*checksandalerts.SystemdUnitCrashLoop).AutomaticRestartsAbove = 2
	writeConfig(t, path, c)
	if _, e := d.reload(); e != nil {
		t.Fatal(e)
	}
	if stored().Started {
		t.Fatalf("a changed check kept the state of its module: %+v", stored())
	}
}

// A check with a schedule forgets a CPU baseline that is older than two of
// its intervals and one time limit. A run without a schedule, such as the
// measurement for a node's report, keeps its baseline.
func TestACheckForgetsAnOldCPUBaselineOnlyWithASchedule(t *testing.T) {
	t.Parallel()
	old := &checksandalerts.CPUSampler{Previous: &primitives.CPUReading{At: time.Now().Add(-time.Minute)}}
	run := checksandalerts.NewCheckRun(checksandalerts.Check{Inputs: new(checksandalerts.NodeResource("cpu"))}, checksandalerts.Timing{CheckEvery: "10s", TimeLimitPerRun: "5s"})
	run.Memory.Value = old
	if run.CPUSampler() != old || old.Previous != nil {
		t.Fatal("a baseline of a minute ago survived a check that runs every 10s")
	}
	recent := &checksandalerts.CPUSampler{Previous: &primitives.CPUReading{At: time.Now().Add(-20 * time.Second)}}
	run.Memory.Value = recent
	if run.CPUSampler().Previous == nil {
		t.Fatal("a baseline within two intervals and one time limit was forgotten")
	}
	old.Previous = &primitives.CPUReading{At: time.Now().Add(-time.Hour)}
	unscheduled := checksandalerts.NewCheckRun(checksandalerts.Check{Inputs: new(checksandalerts.NodeResource("cpu"))}, checksandalerts.Timing{})
	unscheduled.Memory.Value = old
	if unscheduled.CPUSampler().Previous == nil {
		t.Fatal("a run without a schedule forgot its baseline")
	}
	if fresh := checksandalerts.NewCheckRun(checksandalerts.Check{}, checksandalerts.Timing{}); fresh.CPUSampler() == nil || fresh.Memory.Value == nil {
		t.Fatal("a run without a sampler got none")
	}
}

// A finding that a run no longer reports gets the general message, unless
// the check's module writes one of its own. The ID of a finding is text on
// one line of at most 200 bytes.
func TestAFindingThatIsGone(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	timing := checksandalerts.Timing{AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}
	st := &incidents.CheckState{CheckGroup: "certificates", Check: "expiry", OneIncidentForEachFinding: true, AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
	reported := checksandalerts.Observation{Status: "failed", Message: "1 certificate expires soon", At: at, Findings: []checksandalerts.Finding{{ID: "www.example.com", Message: "the certificate of www.example.com expires in 3 days"}}}
	if events := st.Observe(reported, timing, false); len(events) != 1 || events[0].Type != "failed" || events[0].Finding != "www.example.com" || events[0].Message != "the certificate of www.example.com expires in 3 days" {
		t.Fatalf("%+v", events)
	}
	gone := checksandalerts.Observation{Status: "ok", Message: "every certificate is valid", At: at.Add(time.Minute)}
	if events := st.Observe(gone, timing, false); len(events) != 1 || events[0].Type != "recovered" || events[0].Message != "www.example.com is no longer among the findings of this check" {
		t.Fatalf("%+v", events)
	}
	// The longest name of a systemd unit is the ID of its finding.
	longestUnit := strings.Repeat("u", 245) + ".automount"
	if !primitives.AnyUnitPattern.MatchString(longestUnit) || primitives.AnyUnitPattern.MatchString("u"+longestUnit) || len(longestUnit) != checksandalerts.FindingIDLimit {
		t.Fatal("the longest name of a systemd unit and the longest ID of a finding differ")
	}
	for id, valid := range map[string]bool{"backup.service": true, "www.example.com": true, "CVE-2026-1234 in nginx": true, longestUnit: true,
		"": false, "two\nlines": false, "a\x00b": false, strings.Repeat("u", checksandalerts.FindingIDLimit+1): false} {
		if checksandalerts.ValidFindingID(id) != valid {
			t.Errorf("validFindingID(%q) = %v", id, !valid)
		}
	}
}
