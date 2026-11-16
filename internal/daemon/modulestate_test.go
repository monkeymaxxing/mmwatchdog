package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/testhelpers"
)

// scheduledRun runs the check key of a daemon once, as its schedule does,
// and returns the observation that MMWatchdog accepted.
func scheduledRun(t *testing.T, d *Daemon, key string) checksandalerts.Observation {
	t.Helper()
	d.mu.Lock()
	rc := d.checks[key]
	if rc == nil {
		d.mu.Unlock()
		t.Fatalf("the daemon has no check %s", key)
	}
	if rc.memory == nil {
		rc.memory = &checksandalerts.CheckMemory{}
	}
	rc.running = true
	d.running++
	d.workers.Add(1)
	ticket := rc.epoch
	d.mu.Unlock()
	d.observe(context.Background(), key, rc, ticket)
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snapshot.Checks[key].Last
}

// countedRunsScript counts its own runs with read_state and write_state.
const countedRunsScript = `def run_check(inputs, file_contents):
    runs = (read_state() or {"runs": 0})["runs"] + 1
    write_state({"runs": runs})
    return {"status": "ok", "message": "run %d" % runs, "measurements": {"runs": runs}}
`

// countedRunsExecutable counts its own runs in a file that it keeps in the
// state directory of its request.
const countedRunsExecutable = `state_dir=$(sed 's/.*"state_dir":"\([^"]*\)".*/\1/')
echo run >> "$state_dir/runs"
printf '{"protocol":1,"status":"ok","message":"run %d"}' "$(wc -l < "$state_dir/runs")"`

// A check whose module is not core has a state directory of its own, below
// the node's module_state_dir. What a script stores with write_state, and
// what an executable keeps in the directory of its request, is there at the
// check's next scheduled run and after a restart of MMWatchdog. A run of
// "mmwatchdog check-now" works on a copy: it sees the state and leaves it as it is.
// The state belongs to the check as the configuration states it: a reload
// that keeps the check keeps the directory, and one that changes or removes
// the check removes it.
func TestTheStateDirectoryOfACheck(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	testhelpers.InstallStarlarkModule(t, c.ExternalModuleDir, "counted_runs", testhelpers.TestModuleDescription("counted_runs", checksandalerts.UsedAsCheck, map[string]string{"label": "string"}, map[string]string{"runs": "count"}), countedRunsScript)
	testhelpers.InstallModule(t, c.ExternalModuleDir, "counted_runs_in_bash", testhelpers.TestModuleDescription("counted_runs_in_bash", checksandalerts.UsedAsCheck, nil, nil), countedRunsExecutable)
	// The run settings are part of what a check is, so they stay the same.
	own := testhelpers.OwnOptions(t)
	checks := func(label string) map[string]checksandalerts.NamedCheck {
		return map[string]checksandalerts.NamedCheck{
			"script":     {Check: checksandalerts.Check{Inputs: &checksandalerts.ModuleCall{Name: "counted_runs", Inputs: map[string]any{"label": label}, ExecOptions: own}}},
			"executable": {Check: checksandalerts.Check{Inputs: &checksandalerts.ModuleCall{Name: "counted_runs_in_bash", ExecOptions: own}}},
		}
	}
	c.CheckGroups["jobs"] = checksandalerts.CheckGroup{Checks: checks("first")}
	writeConfig(t, path, c)
	d, e := NewDaemon(path)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { d.Close() }()
	scriptState, executableState := filepath.Join(c.ModuleStateDir, "jobs", "script"), filepath.Join(c.ModuleStateDir, "jobs", "executable")
	stored := func() string {
		t.Helper()
		value, e := os.ReadFile(filepath.Join(scriptState, checksandalerts.ModuleStateFile))
		if e != nil && !os.IsNotExist(e) {
			t.Fatal(e)
		}
		lines, e := os.ReadFile(filepath.Join(executableState, "runs"))
		if e != nil && !os.IsNotExist(e) {
			t.Fatal(e)
		}
		return string(value) + " and " + strings.Join(strings.Fields(string(lines)), ",")
	}
	run := func(want string) {
		t.Helper()
		for _, key := range []string{"jobs/script", "jobs/executable"} {
			if o := scheduledRun(t, d, key); o.Status != "ok" || o.Message != want {
				t.Fatalf("%s: got %+v, want %s", key, o, want)
			}
		}
	}
	if _, e := os.Stat(c.ModuleStateDir); !os.IsNotExist(e) {
		t.Fatal("MMWatchdog made a state directory before a check ran:", e)
	}
	run("run 1")
	run("run 2")
	if stored() != `{"runs":2} and run,run` {
		t.Fatal(stored())
	}
	// The unit of a script's measurement is the description's.
	d.mu.Lock()
	last := d.snapshot.Checks["jobs/script"].Last
	d.mu.Unlock()
	if !reflect.DeepEqual(last.Measurements, map[string]checksandalerts.Measurement{"runs": {Value: 2, Unit: "count"}}) {
		t.Fatalf("%+v", last.Measurements)
	}
	// Only the module's account enters the directory of a check; every
	// account passes through the two directories above it.
	for directory, mode := range map[string]os.FileMode{c.ModuleStateDir: 0711, filepath.Dir(scriptState): 0711, scriptState: 0700, executableState: 0700} {
		if info, e := os.Stat(directory); e != nil || info.Mode().Perm() != mode {
			t.Errorf("%s: %v %v, want %v", directory, info.Mode().Perm(), e, mode)
		}
	}

	// mmwatchdog check-now, through the daemon and without it, sees two earlier
	// runs, and the state stays that of two runs.
	observations, e := d.checkNowGroup(context.Background(), "jobs")
	if e != nil || observations["script"].Message != "run 3" || observations["executable"].Message != "run 3" || stored() != `{"runs":2} and run,run` {
		t.Fatalf("mmwatchdog check-now: %+v %v, state %s", observations, e, stored())
	}
	loaded, e := config.Load(path)
	if e != nil {
		t.Fatal(e)
	}
	observations, e = CheckNow(context.Background(), loaded, "jobs")
	if e != nil || observations["script"].Message != "run 3" || observations["executable"].Message != "run 3" || stored() != `{"runs":2} and run,run` {
		t.Fatalf("mmwatchdog check-now --without-daemon: %+v %v, state %s", observations, e, stored())
	}
	// The copies are gone after the runs.
	if copies, e := os.ReadDir(filepath.Join(c.ModuleStateDir, checksandalerts.CheckNowStateDirectory)); e != nil || len(copies) != 0 {
		t.Fatal("a run of mmwatchdog check-now left its copy of the state:", copies, e)
	}
	// A run that belongs to no check of the node starts without a state.
	if o := checksandalerts.ObserveCheck(context.Background(), loaded.CheckGroups["jobs"].Checks["script"].Check); o.Message != "run 1" || stored() != `{"runs":2} and run,run` {
		t.Fatalf("a run without a check: %+v, state %s", o, stored())
	}
	run("run 3")

	// A restart of MMWatchdog keeps the state, and removes a copy that a run of
	// mmwatchdog check-now could not remove.
	leftover := filepath.Join(c.ModuleStateDir, checksandalerts.CheckNowStateDirectory, "jobs-script-1234")
	if e := os.MkdirAll(leftover, 0700); e != nil {
		t.Fatal(e)
	}
	if e := d.Close(); e != nil {
		t.Fatal(e)
	}
	if d, e = NewDaemon(path); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Dir(leftover)); !os.IsNotExist(e) {
		t.Fatal("a restart of MMWatchdog kept the copies of mmwatchdog check-now:", e)
	}
	run("run 4")
	// A reload that changes nothing about the checks keeps their state.
	c.CheckGroups["other"] = checksandalerts.CheckGroup{Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true"}}}
	writeConfig(t, path, c)
	if _, e := d.reload(); e != nil {
		t.Fatal(e)
	}
	run("run 5")
	// A changed input makes the check a new one, which starts without the
	// state of the old one. The other check of the check group keeps its own.
	c.CheckGroups["jobs"] = checksandalerts.CheckGroup{Checks: checks("second")}
	writeConfig(t, path, c)
	if _, e := d.reload(); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(scriptState); !os.IsNotExist(e) || stored() != " and run,run,run,run,run" {
		t.Fatal("a changed check kept the state of its module:", e, stored())
	}
	if o := scheduledRun(t, d, "jobs/script"); o.Message != "run 1" {
		t.Fatalf("%+v", o)
	}
	if o := scheduledRun(t, d, "jobs/executable"); o.Message != "run 6" {
		t.Fatalf("%+v", o)
	}
	// A check that is removed takes its state with it, and the directory of
	// a check group goes with its last check.
	delete(c.CheckGroups["jobs"].Checks, "executable")
	writeConfig(t, path, c)
	if _, e := d.reload(); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(executableState); !os.IsNotExist(e) || stored() != `{"runs":1} and ` {
		t.Fatal("a removed check kept the state of its module:", e, stored())
	}
	delete(c.CheckGroups, "jobs")
	writeConfig(t, path, c)
	if _, e := d.reload(); e != nil {
		t.Fatal(e)
	}
	// What is left is MMWatchdog's own: the directory through which the removed
	// state directories went, which is empty again.
	if left := filesBelow(t, c.ModuleStateDir); !reflect.DeepEqual(left, []string{checksandalerts.RemovedStateDirectory}) {
		t.Fatal("a removed check group kept a state directory:", left)
	}
}

// A value that write_state stores reads back as it was written, a value of
// more than 1 MiB is refused, and a file that holds no JSON is an error. A
// script that stores nothing reads None.
func TestTheStateOfAScript(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if value, e := checksandalerts.ReadModuleState(directory); value != nil || e != nil {
		t.Fatal(value, e)
	}
	for _, value := range []any{map[string]any{"offset": json.Number("9007199254740993"), "names": []any{"a", "b"}, "ratio": json.Number("0.5"), "done": true, "nothing": nil}, "text <&>", json.Number("3"), []any{}, nil} {
		if e := checksandalerts.WriteModuleState(directory, value); e != nil {
			t.Fatal(value, e)
		}
		if read, e := checksandalerts.ReadModuleState(directory); e != nil || !reflect.DeepEqual(read, value) {
			t.Fatalf("stored %#v, read %#v: %v", value, read, e)
		}
	}
	if written, _ := os.ReadFile(filepath.Join(directory, checksandalerts.ModuleStateFile)); string(written) != "null" {
		t.Fatalf("%s", written)
	}
	if info, e := os.Stat(filepath.Join(directory, checksandalerts.ModuleStateFile)); e != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, e)
	}
	// A value that is too large leaves the earlier one.
	if e := checksandalerts.WriteModuleState(directory, "kept"); e != nil {
		t.Fatal(e)
	}
	if e := checksandalerts.WriteModuleState(directory, strings.Repeat("s", checksandalerts.ModuleStateLimit)); e == nil || e.Error() != "the state is more than 1 MiB as JSON" {
		t.Fatal(e)
	}
	if read, e := checksandalerts.ReadModuleState(directory); e != nil || read != "kept" {
		t.Fatal(read, e)
	}
	if e := os.WriteFile(filepath.Join(directory, checksandalerts.ModuleStateFile), []byte("{not JSON"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := checksandalerts.ReadModuleState(directory); e == nil {
		t.Fatal("a state file that holds no JSON was read")
	}
	// The script of a check whose state file is broken says so, and the
	// script of a destination has no state.
	modules := t.TempDir()
	testhelpers.InstallStarlarkModule(t, modules, "counted_runs", testhelpers.TestModuleDescription("counted_runs", checksandalerts.UsedAsCheck, nil, map[string]string{"runs": "count"}), countedRunsScript)
	check := testhelpers.ModuleCheck(t, modules, "counted_runs", nil)
	run := checksandalerts.NewCheckRun(check, checksandalerts.Timing{})
	run.CheckGroup, run.CheckName, run.ModuleStateDir, run.Diagnostic = "jobs", "script", filepath.Join(t.TempDir(), "module_state"), false
	if o := checksandalerts.RunCheck(context.Background(), run); o.Status != "ok" || o.Message != "run 1" {
		t.Fatalf("%+v", o)
	}
	state := filepath.Join(run.ModuleStateDir, "jobs", "script", checksandalerts.ModuleStateFile)
	if e := os.WriteFile(state, []byte("{not JSON"), 0600); e != nil {
		t.Fatal(e)
	}
	if o := checksandalerts.RunCheck(context.Background(), run); o.Status != "unknown" || !strings.Contains(o.Message, checksandalerts.ModuleScriptFile+":2: read_state: ") {
		t.Fatalf("%+v", o)
	}
	testhelpers.InstallStarlarkModule(t, modules, "own_alarm", testhelpers.TestModuleDescription("own_alarm", checksandalerts.UsedAsAlertDestination, nil, nil), "def deliver_alert(alert, inputs, file_contents):\n    write_state(alert[\"id\"])\n    return {\"status\": \"delivered\", \"message\": \"sent\"}\n")
	if e := sendThrough(t, context.Background(), modules, moduleDestination(t, "own_alarm", nil), moduleAlert); e == nil || !strings.HasSuffix(e.Error(), checksandalerts.ModuleScriptFile+":2: write_state: only a check keeps state between runs") {
		t.Fatal(e)
	}
}

// resultFromFile is the script of an executable module that answers with
// what the file at path holds: a test writes the result of each run there.
func resultFromFile(path string) string {
	return "cat > /dev/null\ncat " + testhelpers.ShellQuote(path)
}

// A module whose description states one_incident_for_each_finding reports
// each thing that is wrong as a finding, and each finding is its own
// incident, with its own alert and its own recovery. A finding that a later
// run no longer reports recovers alone. A result that does not keep to the
// rules for findings makes the observation unknown.
func TestTheFindingsOfAModuleAreIncidentsOfTheirOwn(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	answer := filepath.Join(t.TempDir(), "result.json")
	description := testhelpers.TestModuleDescription("certificates", checksandalerts.UsedAsCheck, nil, nil, "one_incident_for_each_finding: true")
	testhelpers.InstallModule(t, c.ExternalModuleDir, "certificates", description, resultFromFile(answer))
	c.CheckGroups["web"] = checksandalerts.CheckGroup{Timing: checksandalerts.Timing{AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"},
		Checks: map[string]checksandalerts.NamedCheck{"certificates_are_valid": {Check: checksandalerts.Check{Inputs: &checksandalerts.ModuleCall{Name: "certificates", ExecOptions: testhelpers.OwnOptions(t)}, AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "sos"}}}}}
	writeConfig(t, path, c)
	d, e := NewDaemon(path)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	const key = "web/certificates_are_valid"
	seen := map[string]bool{}
	// observe runs the check with this result of the module and returns the
	// observation and the events that the run recorded.
	observe := func(result string) (checksandalerts.Observation, []checksandalerts.Event) {
		t.Helper()
		if e := os.WriteFile(answer, []byte(result), 0600); e != nil {
			t.Fatal(e)
		}
		o := scheduledRun(t, d, key)
		recorded, e := d.store.Events(50)
		if e != nil {
			t.Fatal(e)
		}
		events := []checksandalerts.Event{}
		for _, event := range recorded {
			if !seen[event.ID] && event.CheckGroup == "web" {
				events = append(events, event)
			}
			seen[event.ID] = true
		}
		return o, events
	}
	open := func() map[string]string {
		t.Helper()
		d.mu.Lock()
		defer d.mu.Unlock()
		incidents := map[string]string{}
		for _, finding := range d.snapshot.Checks[key].FindingStates() {
			if finding.FailureIncident.Incident != "" {
				incidents[finding.Finding] = finding.FailureIncident.Incident
			}
		}
		return incidents
	}
	byFinding := func(events []checksandalerts.Event, eventType string) map[string]checksandalerts.Event {
		found := map[string]checksandalerts.Event{}
		for _, event := range events {
			if event.Type == eventType {
				found[event.Finding] = event
			}
		}
		return found
	}
	www, api := `{"id":"www.example.com","message":"the certificate of www.example.com expires in 3 days"}`, `{"id":"api.example.com","message":"the certificate of api.example.com expires in 12 days"}`
	// Two findings are two incidents, and the observation lists them in
	// order of their IDs.
	o, events := observe(`{"protocol":1,"status":"failed","message":"2 certificates expire within 14 days","findings":[` + www + `,` + api + `]}`)
	failed := byFinding(events, "failed")
	if o.Status != "failed" || o.Message != "2 certificates expire within 14 days" || !reflect.DeepEqual(o.Findings, []checksandalerts.Finding{{ID: "api.example.com", Message: "the certificate of api.example.com expires in 12 days"}, {ID: "www.example.com", Message: "the certificate of www.example.com expires in 3 days"}}) {
		t.Fatalf("%+v", o)
	}
	incidents := open()
	if len(failed) != 2 || failed["www.example.com"].Message != "the certificate of www.example.com expires in 3 days" || failed["api.example.com"].Message != "the certificate of api.example.com expires in 12 days" ||
		failed["www.example.com"].Incident == "" || failed["www.example.com"].Incident == failed["api.example.com"].Incident || failed["www.example.com"].AlertSeverity != "sos" || failed["www.example.com"].Check != "certificates_are_valid" ||
		len(incidents) != 2 || incidents["www.example.com"] != failed["www.example.com"].Incident || incidents["api.example.com"] != failed["api.example.com"].Incident {
		t.Fatalf("two findings: %+v, open %v", events, incidents)
	}
	// The check itself has no failure incident of its own.
	d.mu.Lock()
	own := d.snapshot.Checks[key].FailureIncident.Incident
	d.mu.Unlock()
	if own != "" {
		t.Fatal("the check has a failure incident beside those of its findings:", own)
	}
	// A finding that the next run no longer reports recovers alone.
	o, events = observe(`{"protocol":1,"status":"failed","message":"1 certificate expires within 14 days","findings":[` + api + `]}`)
	recovered := byFinding(events, "recovered")
	if o.Status != "failed" || len(events) != 1 || recovered["www.example.com"].Message != "www.example.com is no longer among the findings of this check" || recovered["www.example.com"].Incident != incidents["www.example.com"] ||
		!reflect.DeepEqual(open(), map[string]string{"api.example.com": incidents["api.example.com"]}) {
		t.Fatalf("one finding is gone: %+v %+v, open %v", o, events, open())
	}
	// A result that does not keep to the rules for findings is no result:
	// the observation is unknown, and the open incident stays open.
	for result, want := range map[string]string{
		`{"protocol":1,"status":"failed","message":"1 certificate expires within 14 days"}`:                                    "the module certificates answered failed without a finding, and its description states one_incident_for_each_finding",
		`{"protocol":1,"status":"ok","message":"every certificate is valid","findings":[` + api + `]}`:                         "the module certificates reported findings with the status ok, and only a failed result holds findings",
		`{"protocol":1,"status":"unknown","message":"cannot read the directory","findings":[` + api + `]}`:                     "the module certificates reported findings with the status unknown, and only a failed result holds findings",
		`{"protocol":1,"status":"failed","message":"2 certificates expire","findings":[` + api + `,` + api + `]}`:              "the module certificates wrote a result that is not valid: each finding needs an id of its own: text on one line of at most 255 bytes",
		`{"protocol":1,"status":"failed","message":"1 certificate expires","findings":[{"id":"api.example.com"}]}`:             `the module certificates wrote a result that is not valid: the finding "api.example.com" needs a message`,
		`{"protocol":1,"status":"failed","message":"1 certificate expires","findings":[{"id":"two\nlines","message":"soon"}]}`: "the module certificates wrote a result that is not valid: each finding needs an id of its own: text on one line of at most 255 bytes",
	} {
		if o, _ := observe(result); o.Status != "unknown" || o.Message != want || len(open()) != 1 {
			t.Errorf("%s:\n got %s: %s\nwant unknown: %s", result, o.Status, o.Message, want)
		}
	}
	// A result without findings ends the last incident.
	o, events = observe(`{"protocol":1,"status":"ok","message":"every certificate is valid"}`)
	if recovered = byFinding(events, "recovered"); o.Status != "ok" || recovered["api.example.com"].Incident != incidents["api.example.com"] || len(open()) != 0 {
		t.Fatalf("no finding is left: %+v %+v, open %v", o, events, open())
	}
}

// The status of a check names the check's module and the module's kind,
// because the configuration does not show the kind: core, or for a module
// that is not core where the module comes from and what it is.
func TestTheStatusOfACheckNamesTheKindOfItsModule(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	official := testhelpers.OfficialExtraModules(t, checksandalerts.Version)
	testhelpers.InstallStarlarkModule(t, official, "zfs_pool", testhelpers.TestModuleDescription("zfs_pool", checksandalerts.UsedAsCheck, nil, nil), testhelpers.ScriptOfACheck)
	c.OfficialExtraModuleDir = official
	testhelpers.InstallStarlarkModule(t, c.ExternalModuleDir, "queued_jobs", testhelpers.QueuedJobsDescription, testhelpers.QueuedJobsScript)
	testhelpers.InstallModule(t, c.ExternalModuleDir, "legacy_backup", testhelpers.TestModuleDescription("legacy_backup", checksandalerts.UsedAsCheck, nil, nil), "true")
	c.CheckGroups["jobs"] = checksandalerts.CheckGroup{Checks: map[string]checksandalerts.NamedCheck{
		"job_queue_is_short":  {Check: checksandalerts.Check{Inputs: &checksandalerts.ModuleCall{Name: "queued_jobs", Inputs: map[string]any{"queue_file_path": "/var/lib/myapp/pending"}}}},
		"backup_is_recent":    {Check: checksandalerts.Check{Inputs: &checksandalerts.ModuleCall{Name: "legacy_backup"}}},
		"pool_is_healthy":     {Check: checksandalerts.Check{Inputs: &checksandalerts.ModuleCall{Name: "zfs_pool"}}},
		"myapp_answers":       {Check: checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: "http://127.0.0.1:8080/health"}}},
		"memory_of_this_node": {Check: checksandalerts.Check{Inputs: new(checksandalerts.NodeResource("memory"))}},
	}}
	writeConfig(t, path, c)
	d, e := NewDaemon(path)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	w := httptest.NewRecorder()
	d.routes().ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/status", nil))
	var status struct {
		Checks map[string]struct {
			Module     string `json:"module"`
			ModuleKind string `json:"module_kind"`
		} `json:"checks"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &status); e != nil {
		t.Fatal(e)
	}
	for key, want := range map[string][2]string{
		"jobs/job_queue_is_short":  {"queued_jobs", "project_starlark"},
		"jobs/backup_is_recent":    {"legacy_backup", "project_executable"},
		"jobs/pool_is_healthy":     {"zfs_pool", "official_extra"},
		"jobs/myapp_answers":       {"http", "core"},
		"jobs/memory_of_this_node": {"node_resource", "core"},
		"app/shell":                {"shell", "core"},
	} {
		if got := status.Checks[key]; got.Module != want[0] || got.ModuleKind != want[1] {
			t.Errorf("%s: module %q of the kind %q, want %v", key, got.Module, got.ModuleKind, want)
		}
	}
	for kind, words := range map[string]string{checksandalerts.ModuleKindCore: "core", checksandalerts.ModuleKindOfficialExtra: "official extra", checksandalerts.ModuleKindProjectStarlark: "the project's own, Starlark", checksandalerts.ModuleKindProjectExecutable: "the project's own, executable", "": "not installed on this node"} {
		if config.ModuleKindWords(kind) != words {
			t.Errorf("%q: %q", kind, config.ModuleKindWords(kind))
		}
	}
}

// filesBelow lists what a directory holds, by the path below it.
func filesBelow(t *testing.T, directory string) []string {
	t.Helper()
	found := []string{}
	e := filepath.WalkDir(directory, func(path string, _ os.DirEntry, e error) error {
		if relative, _ := filepath.Rel(directory, path); e == nil && relative != "." {
			found = append(found, relative)
		}
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
	return found
}

// On a node the daemon is root, and the account of a module controls what
// is inside the state directory of its check. The copy that a run of
// "mmwatchdog check-now" works on holds the directories and the regular files of
// that directory. MMWatchdog does not follow a symbolic link and does not copy one,
// so the copy never holds a file from elsewhere on the node, and a named
// pipe does not hold MMWatchdog.
func TestTheCopyOfAStateDirectoryHoldsOnlyItsOwnFilesAndDirectories(t *testing.T) {
	t.Parallel()
	state, copied, elsewhere := t.TempDir(), t.TempDir(), t.TempDir()
	if e := os.WriteFile(filepath.Join(elsewhere, "secret"), []byte("a file from elsewhere on the node"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.MkdirAll(filepath.Join(state, "nested", "deeper"), 0700); e != nil {
		t.Fatal(e)
	}
	for path, content := range map[string]string{"file": "kept", "nested/deeper/file": "deep"} {
		if e := os.WriteFile(filepath.Join(state, path), []byte(content), 0640); e != nil {
			t.Fatal(e)
		}
	}
	for link, target := range map[string]string{"link-to-a-file-elsewhere": filepath.Join(elsewhere, "secret"), "link-to-a-directory-elsewhere": elsewhere, "nested/link-to-a-directory-elsewhere": elsewhere, "link-to-its-own-file": "file"} {
		if e := os.Symlink(target, filepath.Join(state, link)); e != nil {
			t.Fatal(e)
		}
	}
	if e := syscall.Mkfifo(filepath.Join(state, "named-pipe"), 0600); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() {
		done <- checksandalerts.CopyStateDirectory(context.Background(), state, copied, checksandalerts.StateDirectoryOwner{})
	}()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the copy waits for someone to write to the named pipe")
	}
	if got, want := filesBelow(t, copied), []string{"file", "nested", "nested/deeper", "nested/deeper/file"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the copy holds %v, want %v", got, want)
	}
	for path, want := range map[string]string{"file": "kept", "nested/deeper/file": "deep"} {
		if content, e := os.ReadFile(filepath.Join(copied, path)); e != nil || string(content) != want {
			t.Fatalf("%s: %q %v", path, content, e)
		}
		if info, e := os.Stat(filepath.Join(copied, path)); e != nil || info.Mode().Perm() != 0640 {
			t.Fatalf("%s: %v %v", path, info.Mode(), e)
		}
	}
	// A state directory that is not there yet copies nothing.
	empty := t.TempDir()
	if e := checksandalerts.CopyStateDirectory(context.Background(), filepath.Join(state, "no-such-directory"), empty, checksandalerts.StateDirectoryOwner{}); e != nil || len(filesBelow(t, empty)) != 0 {
		t.Fatal(e, filesBelow(t, empty))
	}
}

// A file in a state directory that belongs to another account is not the
// state of the module: the module's account can only have put a hard link
// to that file there. The copy for "mmwatchdog check-now" becomes the account's own,
// so MMWatchdog leaves such a file out. Only root gives a copy to another account,
// so this test needs root, which it has in the native suite on a Linux
// guest.
func TestTheCopyOfAStateDirectoryLeavesOutAFileOfAnotherAccount(t *testing.T) {
	t.Parallel()
	if os.Geteuid() != 0 {
		t.Skip("only root gives the copy of a state directory to another account")
	}
	const accountOfTheModule, anotherAccount = 65534, 0
	state, copied := t.TempDir(), t.TempDir()
	for name, owner := range map[string]int{"its-own-file": accountOfTheModule, "a-file-of-root": anotherAccount} {
		path := filepath.Join(state, name)
		if e := os.WriteFile(path, []byte(name), 0600); e != nil {
			t.Fatal(e)
		}
		if e := os.Chown(path, owner, owner); e != nil {
			t.Fatal(e)
		}
	}
	if e := checksandalerts.CopyStateDirectory(context.Background(), state, copied, checksandalerts.StateDirectoryOwner{UserID: accountOfTheModule, GroupID: accountOfTheModule, IsAnotherAccount: true}); e != nil {
		t.Fatal(e)
	}
	if got := filesBelow(t, copied); !reflect.DeepEqual(got, []string{"its-own-file"}) {
		t.Fatalf("the copy holds %v", got)
	}
	info, e := os.Stat(filepath.Join(copied, "its-own-file"))
	if e != nil || int(info.Sys().(*syscall.Stat_t).Uid) != accountOfTheModule {
		t.Fatalf("the copied file does not belong to the account of the module: %+v %v", info.Sys(), e)
	}
}

// The account of a module decides how much its state directory holds, and a
// file with a hole reads as far more than it takes on the disk. A run of
// "mmwatchdog check-now" copies at most CheckNowStateCopyLimit bytes and says so
// when the directory holds more, and the copy ends with the time limit of
// the check.
func TestTheCopyOfAStateDirectoryHasALimit(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	testhelpers.InstallModule(t, c.ExternalModuleDir, "counted_runs_in_bash", testhelpers.TestModuleDescription("counted_runs_in_bash", checksandalerts.UsedAsCheck, nil, nil), countedRunsExecutable)
	// A time limit that the copy of 10,000 files does not reach on a busy
	// machine, where it takes many seconds: the end of the copy at the time
	// limit is tested below.
	c.CheckGroups["jobs"] = checksandalerts.CheckGroup{Timing: checksandalerts.Timing{CheckEvery: "10m", TimeLimitPerRun: "10m"}, Check: checksandalerts.Check{Inputs: &checksandalerts.ModuleCall{Name: "counted_runs_in_bash", ExecOptions: testhelpers.OwnOptions(t)}}}
	writeConfig(t, path, c)
	loaded, e := config.Load(path)
	if e != nil {
		t.Fatal(e)
	}
	// MMWatchdog makes the state directory at the first scheduled run of the check.
	state, _, e := (&checksandalerts.CheckRun{CheckGroup: "jobs", CheckName: "counted_runs_in_bash", ModuleStateDir: c.ModuleStateDir}).ModuleStateDirectory(context.Background(), testhelpers.OwnOptions(t).RunAsUserAccount)
	if e != nil || state != filepath.Join(c.ModuleStateDir, "jobs", "counted_runs_in_bash") {
		t.Fatal(state, e)
	}
	// A file that holds exactly the limit is copied.
	fileWithAHole := func(name string, size int64) {
		t.Helper()
		file, e := os.Create(filepath.Join(state, name))
		if e == nil {
			e = file.Truncate(size)
		}
		if e != nil || file.Close() != nil {
			t.Fatal(e)
		}
	}
	fileWithAHole("as-large-as-the-limit", checksandalerts.CheckNowStateCopyLimit)
	observations, e := CheckNow(context.Background(), loaded, "jobs")
	if e != nil || observations["counted_runs_in_bash"].Status != "ok" || observations["counted_runs_in_bash"].Message != "run 1" {
		t.Fatalf("%+v %v", observations, e)
	}
	// One byte more is too much: 64 MiB and a byte to read, in a second file.
	fileWithAHole("one-byte", 1)
	start := time.Now()
	observations, e = CheckNow(context.Background(), loaded, "jobs")
	want := "MMWatchdog cannot prepare the state directory of the check: the module state directory exceeds the 64 MiB limit for a copy made by mmwatchdog check-now"
	if o := observations["counted_runs_in_bash"]; e != nil || o.Status != "unknown" || o.Message != want {
		t.Fatalf("got %+v %v\nwant unknown: %s", o, e, want)
	}
	// A file of 1 TiB to read costs the copy no more than the limit.
	fileWithAHole("far-larger-than-the-limit", 1<<40)
	if observations, e = CheckNow(context.Background(), loaded, "jobs"); e != nil || observations["counted_runs_in_bash"].Message != want || time.Since(start) > 60*time.Second {
		t.Fatalf("after %s: %+v %v", time.Since(start), observations, e)
	}
	// So does a directory with more files than MMWatchdog copies, although they hold
	// no bytes.
	for _, name := range []string{"as-large-as-the-limit", "one-byte", "far-larger-than-the-limit"} {
		if e := os.Remove(filepath.Join(state, name)); e != nil {
			t.Fatal(e)
		}
	}
	emptyFiles := func(from, to int) {
		t.Helper()
		for i := from; i < to; i++ {
			if e := os.WriteFile(filepath.Join(state, "empty-"+strconv.Itoa(i)), nil, 0600); e != nil {
				t.Fatal(e)
			}
		}
	}
	emptyFiles(0, checksandalerts.CheckNowStateCopyEntryLimit)
	if observations, e = CheckNow(context.Background(), loaded, "jobs"); e != nil || observations["counted_runs_in_bash"].Message != "run 1" {
		t.Fatalf("as many files as MMWatchdog copies: %+v %v", observations, e)
	}
	emptyFiles(checksandalerts.CheckNowStateCopyEntryLimit, checksandalerts.CheckNowStateCopyEntryLimit+1)
	want = "MMWatchdog cannot prepare the state directory of the check: the module state directory exceeds the limit of 10000 files and directories for a copy made by mmwatchdog check-now"
	if observations, e = CheckNow(context.Background(), loaded, "jobs"); e != nil || observations["counted_runs_in_bash"].Message != want {
		t.Fatalf("got %+v %v\nwant %s", observations, e, want)
	}
	// Each copy is removed, also one that MMWatchdog did not finish.
	if copies, e := os.ReadDir(filepath.Join(c.ModuleStateDir, checksandalerts.CheckNowStateDirectory)); e != nil || len(copies) != 0 {
		t.Fatal("a copy that MMWatchdog did not finish stays:", copies, e)
	}
	// The copy ends when the time limit of the check has passed.
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if e := checksandalerts.CopyStateDirectory(ended, state, t.TempDir(), checksandalerts.StateDirectoryOwner{}); !errors.Is(e, context.Canceled) {
		t.Fatal("the copy went on after the end of the run:", e)
	}
}

// module_state_dir is a directory of MMWatchdog's own: MMWatchdog makes it, and everything
// in it. If the setting names, by mistake, a directory that holds other
// things, MMWatchdog does not work there. It refuses the directory in mmwatchdog validate
// Validation on the node and at every run of a check, and it changes and removes nothing
// in it: not at a start, not at a run, and not when a check goes whose name
// is the name of a directory there.
func TestMMWatchdogWorksOnlyInAModuleStateDirectoryThatItMade(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	testhelpers.InstallStarlarkModule(t, c.ExternalModuleDir, "counted_runs", testhelpers.TestModuleDescription("counted_runs", checksandalerts.UsedAsCheck, map[string]string{"label": "string"}, map[string]string{"runs": "count"}), countedRunsScript)
	// The directory holds the data of other programs, two levels down as
	// MMWatchdog's own state directories are. One of them has the name of a check
	// of this node, "postgresql/counted_runs".
	others := []string{"postgresql/counted_runs/PG_VERSION", "docker/volumes/database.volume", "a-file", checksandalerts.CheckNowStateDirectory + "/of-another-program"}
	for _, other := range others {
		file := filepath.Join(c.ModuleStateDir, other)
		if e := os.MkdirAll(filepath.Dir(file), 0755); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(file, []byte("not MMWatchdog's"), 0644); e != nil {
			t.Fatal(e)
		}
	}
	untouched := func(when string) {
		t.Helper()
		for _, other := range others {
			if content, e := os.ReadFile(filepath.Join(c.ModuleStateDir, other)); e != nil || string(content) != "not MMWatchdog's" {
				t.Fatalf("%s, MMWatchdog removed or changed %s: %v", when, other, e)
			}
		}
		for _, directory := range []string{c.ModuleStateDir, filepath.Join(c.ModuleStateDir, "postgresql"), filepath.Join(c.ModuleStateDir, "postgresql", "counted_runs")} {
			if info, e := os.Stat(directory); e != nil || info.Mode().Perm() != 0755 {
				t.Fatalf("%s, MMWatchdog changed the mode of %s, which it did not make: %v %v", when, directory, info.Mode().Perm(), e)
			}
		}
		if left := filesBelow(t, c.ModuleStateDir); len(left) != 9 {
			t.Fatalf("%s, the directory holds %v", when, left)
		}
	}
	notMMWatchdogs := c.ModuleStateDir + " cannot be used for module state: the permissions mode is 0755; MMWatchdog requires 0711. MMWatchdog creates module_state_dir and the directories below module_state_dir; choose a path for module_state_dir where no file or directory exists"
	jobs := checksandalerts.CheckGroup{Check: checksandalerts.Check{Inputs: &checksandalerts.ModuleCall{Name: "counted_runs", ExecOptions: testhelpers.OwnOptions(t)}}}
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"jobs": jobs, "postgresql": jobs, "port": {Check: checksandalerts.Check{Inputs: &checksandalerts.TCP{Address: "127.0.0.1:9"}}}}
	writeConfig(t, path, c)
	loaded, e := config.Load(path)
	if e != nil {
		t.Fatal(e)
	}
	if e := ValidateRuntime(loaded); e == nil || !strings.HasSuffix(e.Error(), ": module_state_dir: "+notMMWatchdogs) {
		t.Fatalf("got %v\nwant %s", e, notMMWatchdogs)
	}
	d, e := NewDaemon(path)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { d.Close() }()
	untouched("after a start")
	for _, key := range []string{"jobs/counted_runs", "postgresql/counted_runs"} {
		if o := scheduledRun(t, d, key); o.Status != "unknown" || o.Message != "MMWatchdog cannot prepare the state directory of the check: "+notMMWatchdogs {
			t.Fatalf("%s: %+v", key, o)
		}
	}
	if observations, e := d.checkNowGroup(context.Background(), "postgresql"); e != nil || observations["counted_runs"].Message != "MMWatchdog cannot prepare the state directory of the check: "+notMMWatchdogs {
		t.Fatalf("%+v %v", observations, e)
	}
	untouched("after the runs")
	// Both checks go, and one of them has the name of a directory there.
	delete(c.CheckGroups, "jobs")
	delete(c.CheckGroups, "postgresql")
	writeConfig(t, path, c)
	if _, e := d.reload(); e != nil {
		t.Fatal(e)
	}
	untouched("after a reload that removed the checks")
	if e := d.Close(); e != nil {
		t.Fatal(e)
	}
	if d, e = NewDaemon(path); e != nil {
		t.Fatal(e)
	}
	untouched("after a restart")

	// The same holds one level down: in a module_state_dir that MMWatchdog made, a
	// directory of a check group that MMWatchdog did not make, and a state directory
	// that is a link or that belongs to nobody that MMWatchdog knows.
	own := filepath.Join(t.TempDir(), "module_state")
	run := func(checkGroup, check string) error {
		_, done, e := (&checksandalerts.CheckRun{CheckGroup: checkGroup, CheckName: check, ModuleStateDir: own}).ModuleStateDirectory(context.Background(), testhelpers.OwnOptions(t).RunAsUserAccount)
		done()
		return e
	}
	if e := run("jobs", "counted_runs"); e != nil {
		t.Fatal(e)
	}
	elsewhere := t.TempDir()
	if e := os.Mkdir(filepath.Join(own, "made_by_hand"), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(elsewhere, filepath.Join(own, "jobs", "a_link")); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(elsewhere, filepath.Join(own, "a_link")); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct{ checkGroup, check, want string }{
		{"made_by_hand", "counted_runs", filepath.Join(own, "made_by_hand") + " cannot be used for module state: the permissions mode is 0755; MMWatchdog requires 0711."},
		{"a_link", "counted_runs", filepath.Join(own, "a_link") + " cannot be used for module state: the path is not a directory."},
		{"jobs", "a_link", filepath.Join(own, "jobs", "a_link") + " is not a state directory created by MMWatchdog for the check and the check's account; remove the path so MMWatchdog can create the state directory again"},
	} {
		if e := run(tc.checkGroup, tc.check); e == nil || !strings.HasPrefix(e.Error(), tc.want) {
			t.Errorf("%s/%s:\n got %v\nwant %s", tc.checkGroup, tc.check, e, tc.want)
		}
	}
	if info, e := os.Stat(elsewhere); e != nil || info.Mode().Perm() != 0755 || len(filesBelow(t, elsewhere)) != 0 {
		t.Fatal("MMWatchdog followed a link out of module_state_dir:", info.Mode().Perm(), e, filesBelow(t, elsewhere))
	}
	// A module_state_dir below a directory that other accounts can change is
	// refused: such an account could put another directory in its place.
	open := t.TempDir()
	if e := os.Chmod(open, 0777); e != nil {
		t.Fatal(e)
	}
	openToOthers := open + " has the mode 0777, so other accounts can change what is in it: MMWatchdog keeps the state of modules below a directory that only its owner can change"
	if e := checksandalerts.ModuleStateDirProblem(filepath.Join(open, "module_state")); e == nil || e.Error() != openToOthers {
		t.Fatal(e)
	}
	// A run refuses it too, and makes nothing there.
	own = filepath.Join(open, "module_state")
	if e := run("jobs", "counted_runs"); e == nil || e.Error() != openToOthers || len(filesBelow(t, open)) != 0 {
		t.Fatal(e, filesBelow(t, open))
	}
}

// A reload does not take another module_state_dir: the checks that it keeps
// would lose their state without a word. The setting changes with a restart
// of MMWatchdog, like state_dir.
func TestAReloadKeepsTheModuleStateDirectory(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	d, e := NewDaemon(path)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	c.ModuleStateDir = filepath.Join(t.TempDir(), "another")
	writeConfig(t, path, c)
	if _, e := d.reload(); e == nil || e.Error() != "changing max_concurrent_checks, socket, state_dir, module_state_dir, heartbeat_receiver, fleet_hub_role.listen_on_address, or fleet_hub_role.address_for_nodes requires restarting MMWatchdog" {
		t.Fatal(e)
	}
}

// The description of a module says whether each finding is an incident of
// its own, and the state of such a check has another form. A check whose
// module changes in this is a new check, although its configuration reads
// the same: it starts without the incidents and without the state of the
// old one.
func TestAModuleThatStartsToReportFindingsMakesItsCheckANewCheck(t *testing.T) {
	t.Parallel()
	plain, withFindings := t.TempDir(), t.TempDir()
	testhelpers.InstallStarlarkModule(t, plain, "failed_jobs", testhelpers.TestModuleDescription("failed_jobs", checksandalerts.UsedAsCheck, nil, nil), testhelpers.ScriptOfACheck)
	testhelpers.InstallStarlarkModule(t, withFindings, "failed_jobs", testhelpers.TestModuleDescription("failed_jobs", checksandalerts.UsedAsCheck, nil, nil, "one_incident_for_each_finding: true"), testhelpers.ScriptOfACheck)
	identity := func(modules string) string {
		check := testhelpers.ModuleCheck(t, modules, "failed_jobs", nil)
		// The account and the directory of the run are the same in both.
		check.Inputs.(*checksandalerts.ModuleCall).ExecOptions = primitives.ExecOptions{RunAsUserAccount: "mmwatchdog", RunFromDirectory: "/"}
		return config.Definition{CheckGroup: "jobs", Name: "no_job_failed", Check: check}.Identity()
	}
	plainIdentity, findingsIdentity := identity(plain), identity(withFindings)
	if plainIdentity == findingsIdentity {
		t.Fatal("a check keeps its state when its module starts to report findings")
	}
	if identity(plain) != plainIdentity || identity(withFindings) != findingsIdentity {
		t.Fatal("the same check is two checks")
	}
}
