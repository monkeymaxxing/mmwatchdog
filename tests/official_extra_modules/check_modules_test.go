package official_extra_modules

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
)

// The tests of the official extra modules that check a tool: zfs_pool,
// apache_status, and mongodb. Each test runs the module's script as
// MMWatchdog runs it. A program that a module asks is a stand-in in the
// test's own directory, which writes an answer of the real program.

// checked is the result of one run of a check module.
type checked struct {
	Protocol     int                `json:"protocol"`
	Status       string             `json:"status"`
	Message      string             `json:"message"`
	Measurements map[string]float64 `json:"measurements"`
	// written is everything that the run wrote, for a test that looks for a
	// secret in it.
	written string
}

// standIns writes one stand-in program for each name into a directory of
// the test and returns that directory. Each program is a shell script.
func standIns(t *testing.T, programs map[string]string) string {
	t.Helper()
	directory := t.TempDir()
	for name, script := range programs {
		writeStandInProgram(t, filepath.Join(directory, name), "#!/bin/sh\n"+script+"\n")
	}
	return directory
}

// check runs a check module once, as MMWatchdog does, with the directory programs
// as the only directory on its PATH.
func check(t *testing.T, module string, inputs map[string]any, programs string) checked {
	t.Helper()
	contents := map[string]string{}
	if file, ok := inputs["connection_string_file"].(string); ok {
		value, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		contents["connection_string_file"] = string(value)
	}
	return checkWithRequest(t, module, map[string]any{"protocol": 1, "inputs": inputs, "file_contents": contents, "state_dir": t.TempDir()}, programs)
}

// checkWithRequest runs a check module once as check does, with the request
// as given.
func checkWithRequest(t *testing.T, module string, fields map[string]any, programs string) checked {
	t.Helper()
	request, e := json.Marshal(fields)
	if e != nil {
		t.Fatal(e)
	}
	if programs == "" {
		programs = t.TempDir()
	}
	command := exec.Command(program(t), "run-starlark-module", filepath.Join(officialExtraModules(t), module))
	command.Stdin = bytes.NewReader(request)
	command.Dir = t.TempDir()
	command.Env = []string{"PATH=" + programs, "HOME=" + command.Dir, "USER=mmwatchdog", "LANG=C.UTF-8"}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	done := make(chan error, 1)
	if e := command.Start(); e != nil {
		t.Fatal(e)
	}
	go func() { done <- command.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		command.Process.Kill()
		t.Fatal("the module did not end:", stderr.String())
	}
	result := checked{written: stdout.String() + stderr.String()}
	if code := command.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("the module %s ended with exit code %d: %s", module, code, stderr.String())
	}
	if e := checksandalerts.DecodeJSON(stdout.Bytes(), &result); e != nil || result.Protocol != 1 {
		t.Fatalf("the module %s did not answer with its result: %v: %q %q", module, e, stdout.String(), stderr.String())
	}
	if e := checksandalerts.ValidateResult(checksandalerts.ModuleResult{Protocol: result.Protocol, Status: result.Status, Message: result.Message}); e != nil {
		t.Fatalf("the module %s answered with a result that is not valid: %v", module, e)
	}
	return result
}

// answerWith is the script of a stand-in that writes the file at path and
// ends with an exit code.
func answerWith(path string, exitCode int) string {
	return "/bin/cat '" + path + "'\nexit " + string(rune('0'+exitCode))
}

// writeAnswer writes an answer of a stand-in program into a file of the
// test and returns the file's path.
func writeAnswer(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "answer")
	if e := os.WriteFile(path, []byte(text), 0644); e != nil {
		t.Fatal(e)
	}
	return path
}

// zfs_pool asks zpool for the health, the use, and the fragmentation of one
// pool, and measures how long ago the pool was last scrubbed.
func TestZFSPool(t *testing.T) {
	t.Parallel()
	// The time of the test, and the days before it of the dates that the
	// stand-in for date knows, in seconds since 1970 as date writes them.
	const now, day = 1792200000, 86400
	// Stand-ins for zpool, zfs, and date. zpool: "list" writes one line for
	// the pool, "status" the whole status of the pool, and "status -x" what
	// that says about a problem. zfs writes the time of the pool's creation,
	// 3 days ago. date reads the current time and the dates that the module
	// asks for in the node's own time zone, and knows two dates.
	zpool := func(listed, whole, explained string) string {
		list, state, explanation := writeAnswer(t, listed), writeAnswer(t, whole), writeAnswer(t, explained)
		return standIns(t, map[string]string{
			"zpool": `case "$1 $2" in
"list -H") if [ "$6" = tank ]; then /bin/cat '` + list + `'; else echo "cannot open '$6': no such pool" >&2; exit 1; fi ;;
"status -x") /bin/cat '` + explanation + `' ;;
"status tank") /bin/cat '` + state + `' ;;
*) echo "zpool: unexpected arguments: $*" >&2; exit 2 ;;
esac`,
			"zfs": `[ "$*" = "get -H -p -o value creation tank" ] || { echo "zfs: unexpected arguments: $*" >&2; exit 2; }
echo ` + fmt.Sprint(now-3*day),
			"date": `[ "$*" = "--file=- +%s" ] || { echo "date: unexpected arguments: $*" >&2; exit 2; }
while IFS= read -r line; do
  case "$line" in
    now) echo ` + fmt.Sprint(now) + ` ;;
    "Fri Oct 2 08:51:09 2026") echo ` + fmt.Sprint(now-14*day) + ` ;;
    "Thu Oct 15 20:00:00 2026") echo ` + fmt.Sprint(now-day/2) + ` ;;
    *) echo "date: invalid date '$line'" >&2; exit 1 ;;
  esac
done`})
	}
	// The whole status of a healthy pool, with the scan line that is given.
	status := func(scan string) string {
		return "  pool: tank\n state: ONLINE\n" + scan + "config:\n\n\tNAME        STATE     READ WRITE CKSUM\n\ttank        ONLINE       0     0     0\n\nerrors: No known data errors\n"
	}
	const healthy = "pool 'tank' is healthy\n"
	inputs := map[string]any{"pool_name": "tank"}
	// The scan line in each form that zpool 2.1 to 2.4 writes, with the days
	// since the last scrub and what the message says about the scrub. A
	// scrub that has not ended cannot replace a completed scrub. Without
	// a completed scrub in state, the reading counts from pool creation.
	for _, scan := range []struct {
		line, about string
		days        float64
	}{
		{"  scan: scrub repaired 0B in 00:00:01 with 0 errors on Fri Oct  2 08:51:09 2026\n",
			"its last scrub ended on Fri Oct 2 08:51:09 2026", 14},
		{"  scan: scrub in progress since Thu Oct 15 20:00:00 2026\n\t0B / 100M scanned, 0B / 100M issued\n\t0B repaired, 0.00% done, no estimated completion time\n",
			"MMWatchdog has not observed a completed scrub, so days_since_last_scrub counts from the pool's creation", 3},
		{"  scan: scrub paused since Fri Oct 16 08:51:33 2026\n\tscrub started on Thu Oct 15 20:00:00 2026\n\t0B / 100M scanned, 0B / 100M issued\n\t0B repaired, 0.00% done\n",
			"MMWatchdog has not observed a completed scrub, so days_since_last_scrub counts from the pool's creation", 3},
		{"  scan: scrub canceled on Thu Oct 15 20:00:00 2026\n",
			"MMWatchdog has not observed a completed scrub, so days_since_last_scrub counts from the pool's creation", 3},
		// A resilver takes the place of the last scrub in zpool's status.
		{"  scan: resilvered 20.1M in 00:00:02 with 0 errors on Fri Oct  2 08:51:09 2026\n",
			"MMWatchdog has not observed a completed scrub, so days_since_last_scrub counts from the pool's creation", 3},
		{"  scan: resilver in progress since Thu Oct 15 20:00:00 2026\n\t0B / 120M scanned, 0B / 120M issued\n\t0B resilvered, 0.00% done, no estimated completion time\n",
			"MMWatchdog has not observed a completed scrub, so days_since_last_scrub counts from the pool's creation", 3},
		// A pool that was never scrubbed has no scan line.
		{"", "MMWatchdog has not observed a completed scrub, so days_since_last_scrub counts from the pool's creation", 3},
	} {
		o := check(t, "zfs_pool", inputs, zpool("tank\tONLINE\t42\t7\n", status(scan.line), healthy))
		if o.Status != "ok" || o.Message != "the ZFS pool tank is ONLINE and healthy; "+scan.about || !reflect.DeepEqual(o.Measurements, map[string]float64{"space_used_percent": 42, "free_space_fragmentation_percent": 7, "days_since_last_scrub": scan.days}) {
			t.Fatalf("%q: %+v", scan.line, o)
		}
	}
	// State survives separate runs. Starting, pausing, or canceling a
	// scrub, and a resilver, cannot replace the last completed scrub.
	stateDirectory := t.TempDir()
	runWithState := func(scan string) checked {
		return checkWithRequest(t, "zfs_pool", map[string]any{"protocol": 1, "inputs": inputs, "state_dir": stateDirectory}, zpool("tank\tONLINE\t42\t7\n", status(scan), healthy))
	}
	if o := runWithState("  scan: scrub repaired 0B in 00:00:01 with 0 errors on Fri Oct 2 08:51:09 2026\n"); o.Status != "ok" || o.Measurements["days_since_last_scrub"] != 14 {
		t.Fatalf("%+v", o)
	}
	for _, scan := range []string{
		"  scan: scrub in progress since Thu Oct 15 20:00:00 2026\n",
		"  scan: scrub paused since Fri Oct 16 08:51:33 2026\n\tscrub started on Thu Oct 15 20:00:00 2026\n",
		"  scan: scrub canceled on Thu Oct 15 20:00:00 2026\n",
		"  scan: resilvered 20.1M in 00:00:02 with 0 errors on Thu Oct 15 20:00:00 2026\n",
		"  scan: resilver in progress since Thu Oct 15 20:00:00 2026\n",
	} {
		if o := runWithState(scan); o.Status != "ok" || o.Measurements["days_since_last_scrub"] != 14 || o.Message != "the ZFS pool tank is ONLINE and healthy; its last completed scrub observed by MMWatchdog ended on Fri Oct 2 08:51:09 2026" {
			t.Fatalf("%q: %+v", scan, o)
		}
	}
	if o := runWithState("  scan: scrub repaired 0B in 00:00:01 with 0 errors on Thu Oct 15 20:00:00 2026\n"); o.Status != "ok" || o.Measurements["days_since_last_scrub"] != 0.5 {
		t.Fatalf("%+v", o)
	}
	if o := runWithState("  scan: scrub canceled on Thu Oct 15 20:00:00 2026\n"); o.Status != "ok" || o.Measurements["days_since_last_scrub"] != 0.5 {
		t.Fatalf("%+v", o)
	}
	// A date that the date program cannot read: the run has no such
	// measurement, and its message says why.
	o := check(t, "zfs_pool", inputs, zpool("tank\tONLINE\t42\t7\n", status("  scan: scrub repaired 0B in 00:00:01 with 0 errors on Sat Jan  1 00:00:00 2000\n"), healthy))
	if o.Status != "ok" || o.Message != "the ZFS pool tank is ONLINE and healthy; its last scrub ended on Sat Jan 1 00:00:00 2000; MMWatchdog could not read the date Sat Jan 1 00:00:00 2000 with the date program: date: invalid date 'Sat Jan 1 00:00:00 2000'" || !reflect.DeepEqual(o.Measurements, map[string]float64{"space_used_percent": 42, "free_space_fragmentation_percent": 7}) {
		t.Fatalf("%+v", o)
	}
	const degraded = `  pool: tank
 state: DEGRADED
status: One or more devices could not be used because the label is missing or
	invalid.  Sufficient replicas exist for the pool to continue
	functioning in a degraded state.
action: Replace the device using 'zpool replace'.
  scan: scrub repaired 0B in 00:00:01 with 0 errors on Fri Oct  2 08:51:09 2026
config:

	NAME        STATE     READ WRITE CKSUM
	tank        DEGRADED     0     0     0
`
	if o := check(t, "zfs_pool", inputs, zpool("tank\tDEGRADED\t42\t7\n", degraded, degraded)); o.Status != "failed" || o.Message != "the ZFS pool tank is DEGRADED: One or more devices could not be used because the label is missing or invalid.  Sufficient replicas exist for the pool to continue functioning in a degraded state." || !reflect.DeepEqual(o.Measurements, map[string]float64{"space_used_percent": 42, "free_space_fragmentation_percent": 7, "days_since_last_scrub": 14}) {
		t.Fatalf("%+v", o)
	}
	// A pool that is ONLINE and has a device with errors.
	const errors = "  pool: tank\n state: ONLINE\nstatus: One or more devices has experienced an unrecoverable error.\naction: Determine if the device needs to be replaced.\n"
	if o := check(t, "zfs_pool", inputs, zpool("tank\tONLINE\t42\t7\n", errors, errors)); o.Status != "failed" || o.Message != "the ZFS pool tank is ONLINE and has a problem: One or more devices has experienced an unrecoverable error." {
		t.Fatalf("%+v", o)
	}
	// A pool without a number for its fragmentation has no such measurement.
	if o := check(t, "zfs_pool", inputs, zpool("tank\tONLINE\t3\t-\n", status(""), healthy)); o.Status != "ok" || !reflect.DeepEqual(o.Measurements, map[string]float64{"space_used_percent": 3, "days_since_last_scrub": 3}) {
		t.Fatalf("%+v", o)
	}
	// When zpool cannot say whether a pool that is ONLINE has a problem,
	// the check does not call the pool healthy.
	list := writeAnswer(t, "tank\tONLINE\t42\t7\n")
	broken := standIns(t, map[string]string{"zpool": `case "$1" in
list) /bin/cat '` + list + `' ;;
status) echo "internal error: out of memory" >&2; exit 1 ;;
esac`})
	if o := check(t, "zfs_pool", inputs, broken); o.Status != "unknown" || o.Message != "the ZFS pool tank is ONLINE, and zpool could not say whether it has a problem: internal error: out of memory" || !reflect.DeepEqual(o.Measurements, map[string]float64{"space_used_percent": 42, "free_space_fragmentation_percent": 7}) {
		t.Fatalf("%+v", o)
	}
	// A pool that this node did not import fails the check.
	if o := check(t, "zfs_pool", map[string]any{"pool_name": "backup"}, zpool("tank\tONLINE\t42\t7\n", "", "")); o.Status != "failed" || o.Message != "this node has no ZFS pool named backup" {
		t.Fatalf("%+v", o)
	}
	// A node without the zpool program establishes nothing.
	if o := check(t, "zfs_pool", inputs, ""); o.Status != "unknown" || !strings.HasPrefix(o.Message, "MMWatchdog cannot run the zpool program: ") {
		t.Fatalf("%+v", o)
	}
}

// apache_status reads the status page of mod_status in the form for
// programs.
func TestApacheStatus(t *testing.T) {
	t.Parallel()
	page := "localhost\nServerVersion: Apache/2.4.65 (Debian)\nTotal Accesses: 120\nBusyWorkers: 3\nIdleWorkers: 47\nReqPerSec: .5\nScoreboard: W_W_W" + strings.Repeat("_", 45) + strings.Repeat(".", 100) + "\n"
	apache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/server-status" && r.URL.RawQuery == "auto":
			w.Write([]byte(page))
		case r.URL.Path == "/server-status":
			w.Write([]byte("<html><body><h1>Apache Server Status</h1></body></html>"))
		case r.URL.Path == "/forbidden":
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer apache.Close()
	o := check(t, "apache_status", map[string]any{"url": apache.URL + "/server-status?auto"}, "")
	if o.Status != "ok" || o.Message != "Apache answers; 3 of at most 150 workers are busy" || !reflect.DeepEqual(o.Measurements, map[string]float64{"busy_workers": 3, "idle_workers": 47, "busy_workers_percent": 2}) {
		t.Fatalf("%+v", o)
	}
	// The page for people, without ?auto.
	if o := check(t, "apache_status", map[string]any{"url": apache.URL + "/server-status"}, ""); o.Status != "unknown" || o.Message != "the answer is not the status page of Apache in the form for programs: the address must be the one of mod_status and end with ?auto" {
		t.Fatalf("%+v", o)
	}
	if o := check(t, "apache_status", map[string]any{"url": apache.URL + "/forbidden"}, ""); o.Status != "failed" || o.Message != "Apache answered the request for its status page with HTTP 403; the page of mod_status answers with 200" {
		t.Fatalf("%+v", o)
	}
	address := apache.URL
	apache.Close()
	if o := check(t, "apache_status", map[string]any{"url": address + "/server-status?auto"}, ""); o.Status != "failed" || !strings.HasPrefix(o.Message, "Apache did not answer the request for its status page: ") {
		t.Fatalf("%+v", o)
	}
	// An Apache that hangs accepts the connection and never answers. The
	// check fails before the time limit of the run, so a recovery that the
	// check drives restarts Apache.
	hung := map[string]any{"protocol": 1, "inputs": map[string]any{"url": "http://" + silentServer(t) + "/server-status?auto"}, "state_dir": t.TempDir(),
		"mmwatchdog_ends_the_module_after_milliseconds": timeLimitOfARunThatGetsNoAnswer.Milliseconds()}
	if o := checkWithRequest(t, "apache_status", hung, ""); o.Status != "failed" || o.Message != "Apache did not answer the request for its status page: MMWatchdog ended the call shortly before the time limit of the run (time_limit_per_run)" {
		t.Fatalf("%+v", o)
	}
}

// mongodb asks the server through mongosh. The address of the server
// reaches the shell on its standard input, never as an argument, and never
// appears in a result. The stand-in writes its answer as the shell does
// when it reads its input line by line: with its prompts in front.
func TestMongoDB(t *testing.T) {
	t.Parallel()
	// A stand-in for mongosh that records its arguments and its standard
	// input, and writes an answer.
	mongosh := func(written string, exitCode int) (directory, arguments, questions string) {
		answer := writeAnswer(t, written)
		directory = t.TempDir()
		arguments, questions = filepath.Join(directory, "arguments"), filepath.Join(directory, "questions")
		script := "#!/bin/sh\necho \"$@\" > '" + arguments + "'\n/bin/cat > '" + questions + "'\n" + answerWith(answer, exitCode) + "\n"
		writeStandInProgram(t, filepath.Join(directory, "mongosh"), script)
		return directory, arguments, questions
	}
	answer := func(fields map[string]any) string {
		all := map[string]any{"replica_set": "", "is_primary": true, "is_secondary": false, "is_arbiter": false, "waits_for_a_replica_set": false, "info": "",
			"connections_current": 20, "connections_available": 780, "state": "", "lag_seconds": nil, "unhealthy_members": []string{}, "replica_status_error": ""}
		for name, value := range fields {
			all[name] = value
		}
		written, _ := json.Marshal(all)
		return "Telemetry is now disabled.\n> ... ... ... ... > MMWATCHDOG_ANSWER " + string(written) + "\n> "
	}
	directory, arguments, questions := mongosh(answer(nil), 0)
	o := check(t, "mongodb", map[string]any{}, directory)
	if o.Status != "ok" || o.Message != "the MongoDB server answers; it is a server of its own, in no replica set" || !reflect.DeepEqual(o.Measurements, map[string]float64{"connections_used_percent": 2.5}) {
		t.Fatalf("%+v", o)
	}
	if said, _ := os.ReadFile(arguments); string(said) != "--nodb --quiet --norc\n" {
		t.Fatalf("%q", said)
	}
	// The questions are one statement, and the shell gives up on a server
	// that is gone after 10 seconds.
	if asked, _ := os.ReadFile(questions); !strings.Contains(string(asked), `const connection = new Mongo("mongodb://127.0.0.1:27017/?serverSelectionTimeoutMS=10000");`) || !strings.Contains(string(asked), "replSetGetStatus") ||
		!strings.HasPrefix(strings.TrimSpace(string(asked)), "try {") || !strings.Contains(string(asked), `print("MMWATCHDOG_ERROR " + `) {
		t.Fatalf("%s", asked)
	}
	// A connection string with a password comes from a file. It is in the
	// questions and nowhere else.
	const secret = "mongodb://mmwatchdog_monitoring:a-secret-password@db-1.internal.example.com:27017/?authSource=admin"
	file := filepath.Join(t.TempDir(), "connection-string")
	os.WriteFile(file, []byte(secret+"\n"), 0600)
	directory, arguments, questions = mongosh(answer(map[string]any{"replica_set": "rs0", "is_primary": false, "is_secondary": true, "state": "SECONDARY", "lag_seconds": 4.5}), 0)
	o = check(t, "mongodb", map[string]any{"connection_string_file": file}, directory)
	if o.Status != "ok" || o.Message != "the MongoDB server answers; it is a member of the replica set rs0 in the state SECONDARY" || o.Measurements["replication_lag_seconds"] != 4.5 || strings.Contains(o.written, "a-secret-password") {
		t.Fatalf("%+v", o)
	}
	if said, _ := os.ReadFile(arguments); strings.Contains(string(said), "a-secret-password") {
		t.Fatalf("the password is an argument of mongosh: %q", said)
	}
	if asked, _ := os.ReadFile(questions); !strings.Contains(string(asked), `new Mongo("`+secret+`&serverSelectionTimeoutMS=10000")`) {
		t.Fatalf("%s", asked)
	}
	// A connection string that states its own time keeps it, and one
	// without a slash after its hosts gets one.
	os.WriteFile(file, []byte("mongodb://db-1:27017/?serverSelectionTimeoutMS=2000\n"), 0600)
	check(t, "mongodb", map[string]any{"connection_string_file": file}, directory)
	if asked, _ := os.ReadFile(questions); !strings.Contains(string(asked), `new Mongo("mongodb://db-1:27017/?serverSelectionTimeoutMS=2000")`) {
		t.Fatalf("%s", asked)
	}
	os.WriteFile(file, []byte("mongodb://db-1:27017,db-2:27017\n"), 0600)
	check(t, "mongodb", map[string]any{"connection_string_file": file}, directory)
	if asked, _ := os.ReadFile(questions); !strings.Contains(string(asked), `new Mongo("mongodb://db-1:27017,db-2:27017/?serverSelectionTimeoutMS=10000")`) {
		t.Fatalf("%s", asked)
	}
	os.WriteFile(file, []byte(secret+"\n"), 0600)

	// The script consumes the supplied credential even when its own
	// account cannot read the configured path.
	o = checkWithRequest(t, "mongodb", map[string]any{"protocol": 1, "inputs": map[string]any{"connection_string_file": "/etc/mmwatchdog/secrets/not-readable-by-the-check"}, "file_contents": map[string]string{"connection_string_file": secret}, "state_dir": t.TempDir()}, directory)
	if o.Status != "ok" || strings.Contains(o.written, "a-secret-password") {
		t.Fatalf("%+v", o)
	}
	// A server that answers but refuses replSetGetStatus establishes no
	// replica-set health. Its reason is useful and exposes no credential.
	directory, _, _ = mongosh(answer(map[string]any{"replica_set": "rs0", "replica_status_error": "MongoServerError: not authorized for " + secret + " with a-secret-password"}), 0)
	o = check(t, "mongodb", map[string]any{"connection_string_file": file}, directory)
	if o.Status != "unknown" || o.Message != "MongoDB could not report its replica set status: MongoServerError: not authorized for the server's address with (the password)" || strings.Contains(o.written, "a-secret-password") {
		t.Fatalf("%+v", o)
	}
	// The primary is 0 seconds behind, so one check with a limit fits
	// every member. An arbiter is a healthy member too.
	directory, _, _ = mongosh(answer(map[string]any{"replica_set": "rs0", "state": "PRIMARY", "lag_seconds": 0}), 0)
	if o := check(t, "mongodb", map[string]any{}, directory); o.Status != "ok" || !reflect.DeepEqual(o.Measurements, map[string]float64{"connections_used_percent": 2.5, "replication_lag_seconds": 0}) {
		t.Fatalf("%+v", o)
	}
	directory, _, _ = mongosh(answer(map[string]any{"replica_set": "rs0", "is_primary": false, "is_arbiter": true, "state": "ARBITER"}), 0)
	if o := check(t, "mongodb", map[string]any{}, directory); o.Status != "ok" || o.Message != "the MongoDB server answers; it is a member of the replica set rs0 in the state ARBITER" {
		t.Fatalf("%+v", o)
	}
	// A member that is none of the three, and a member that cannot reach
	// another one, fail the check.
	directory, _, _ = mongosh(answer(map[string]any{"replica_set": "rs0", "is_primary": false, "is_secondary": false, "state": "RECOVERING"}), 0)
	if o := check(t, "mongodb", map[string]any{}, directory); o.Status != "failed" || o.Message != "the MongoDB server answers; it is a member of the replica set rs0 in the state RECOVERING, which is neither PRIMARY, SECONDARY, nor ARBITER" {
		t.Fatalf("%+v", o)
	}
	directory, _, _ = mongosh(answer(map[string]any{"replica_set": "rs0", "state": "PRIMARY", "unhealthy_members": []string{"db-2:27017", "db-3:27017"}}), 0)
	if o := check(t, "mongodb", map[string]any{}, directory); o.Status != "failed" || o.Message != "the MongoDB server answers; it is a member of the replica set rs0 in the state PRIMARY; this member cannot reach db-2:27017, db-3:27017" {
		t.Fatalf("%+v", o)
	}
	// A server that was started for a replica set and is a member of
	// none, as one that was removed from its replica set, is no server of
	// its own: it takes no changes.
	directory, _, _ = mongosh(answer(map[string]any{"is_primary": false, "waits_for_a_replica_set": true, "info": "Does not have a valid replica set config"}), 0)
	if o := check(t, "mongodb", map[string]any{}, directory); o.Status != "failed" ||
		o.Message != "the MongoDB server answers and takes no changes: it was started for a replica set and is a member of none (Does not have a valid replica set config)" {
		t.Fatalf("%+v", o)
	}
	// A server that does not answer: the reason that the questions caught,
	// without the address and without the password in it.
	directory, _, _ = mongosh("Warning: Could not access file: EACCES\n> ... ... > MMWATCHDOG_ERROR MongoServerSelectionError: connect ECONNREFUSED "+secret+"&serverSelectionTimeoutMS=10000 with a-secret-password\n> ", 0)
	o = check(t, "mongodb", map[string]any{"connection_string_file": file}, directory)
	if o.Status != "failed" || o.Message != "the MongoDB server did not answer the check: MongoServerSelectionError: connect ECONNREFUSED the server's address with (the password)" || strings.Contains(o.written, "a-secret-password") {
		t.Fatalf("%+v", o)
	}
	// A shell that ends without either line: the first line that names an
	// error, and not a warning in front of it.
	directory, _, _ = mongosh("Warning: Could not access file: EACCES\nSyntaxError: Unexpected token (3:1)\n", 1)
	if o := check(t, "mongodb", map[string]any{}, directory); o.Status != "failed" || o.Message != "the MongoDB server did not answer the check: SyntaxError: Unexpected token (3:1)" {
		t.Fatalf("%+v", o)
	}
	// A file that holds something else, a password with a character that
	// separates the parts of a connection string, and a node without mongosh
	// establish nothing.
	os.WriteFile(file, []byte("db-1.internal.example.com\n"), 0600)
	if o := check(t, "mongodb", map[string]any{"connection_string_file": file}, directory); o.Status != "unknown" || !strings.HasPrefix(o.Message, "the file that connection_string_file names must hold one MongoDB connection string") {
		t.Fatalf("%+v", o)
	}
	for _, unsafe := range []string{"mongodb://mmwatchdog:first/zzsecond@db-1:27017/", "mongodb://mmwatchdog:first@zzsecond@db-1:27017/", "mongodb://mmwatchdog:first:zzsecond@db-1:27017/"} {
		os.WriteFile(file, []byte(unsafe+"\n"), 0600)
		if o := check(t, "mongodb", map[string]any{"connection_string_file": file}, directory); o.Status != "unknown" || !strings.Contains(o.Message, "must be written in its percent form") || strings.Contains(o.written, "zzsecond") {
			t.Fatalf("%s: %+v", unsafe, o)
		}
	}

	if o := check(t, "mongodb", map[string]any{}, ""); o.Status != "unknown" || !strings.HasPrefix(o.Message, "MMWatchdog cannot run mongosh, the shell of MongoDB: ") {
		t.Fatalf("%+v", o)
	}
}
