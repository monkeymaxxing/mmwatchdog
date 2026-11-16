package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/testhelpers"
)

// TestMain shortens the waits that no test studies and makes the
// certificate of the tests' mail servers.
func TestMain(m *testing.M) {
	// As in the daemon's process (cmd/mmwatchdog): the daemon's times are in UTC
	// and carry their monotonic readings.
	time.Local = time.UTC
	testhelpers.RunTests(m, func() error {
		// The notifications worker reads its queue every 200 ms; tests
		// deliver alerts one after another and wait for each.
		readNotificationQueueEvery = 25 * time.Millisecond
		return trustMailServerCertificate()
	})
}

// A Starlark script is a check: MMWatchdog calls its run_check with the check's
// inputs, and the dictionary that it returns is the observation. The script
// only measures. The unit of a measurement comes from the module's
// description, and MMWatchdog applies the check's limit.
func TestAStarlarkScriptIsACheck(t *testing.T) {
	t.Parallel()
	modules := t.TempDir()
	testhelpers.InstallStarlarkModule(t, modules, "queued_jobs", testhelpers.QueuedJobsDescription, testhelpers.QueuedJobsScript)
	queue := filepath.Join(t.TempDir(), "pending")
	for _, tc := range []struct {
		content         string
		above           map[string]float64
		status, message string
	}{
		{"780\n", nil, "ok", "780 queued jobs"},
		{"780\n", map[string]float64{"queued_jobs": 1000}, "ok", "queued_jobs is 780 (fails above 1000); the module reported: 780 queued jobs"},
		{"780\n", map[string]float64{"queued_jobs": 500}, "failed", "queued_jobs is 780 (fails above 500); the module reported: 780 queued jobs"},
		{"many\n", map[string]float64{"queued_jobs": 500}, "unknown", "the queue file does not hold a whole number"},
		{"", nil, "unknown", "MMWatchdog cannot read the queue file: open " + queue + ": no such file or directory"},
	} {
		os.Remove(queue)
		if tc.content != "" {
			if e := os.WriteFile(queue, []byte(tc.content), 0600); e != nil {
				t.Fatal(e)
			}
		}
		check := testhelpers.ModuleCheck(t, modules, "queued_jobs", map[string]any{"queue_file_path": queue})
		check.FailWhenMeasurementAbove = tc.above
		o := checksandalerts.ObserveCheck(context.Background(), check)
		if o.Status != tc.status || o.Message != tc.message || o.Stdout != "" || o.ExitCode != nil {
			t.Fatalf("%q %v: %+v", tc.content, tc.above, o)
		}
		if measured := tc.content == "780\n"; measured != reflect.DeepEqual(o.Measurements, map[string]checksandalerts.Measurement{"queued_jobs": {Value: 780, Unit: "count"}}) {
			t.Fatalf("%q: the measurements are %+v", tc.content, o.Measurements)
		}
	}
}

// primitivesScript calls every primitive and returns what each gave it, as
// JSON in the message of its result.
const primitivesScript = `def run_check(inputs, file_contents):
    seen = {}
    file = read_file(inputs["file_path"])
    seen["read_file"] = [file.error, file.content]
    missing = read_file(inputs["file_path"] + ".missing")
    seen["read_file of a file that is missing"] = [missing.error != "", missing.content]
    large = read_file(inputs["large_file_path"])
    seen["read_file of a file of more than 1 MiB"] = ["holds more than 1048576 bytes" in large.error, large.content]

    ran = run_command(["/bin/sh", "-c", "cat; printf err >&2; exit 3"], standard_input = "in")
    seen["run_command"] = [ran.error, ran.exit_code, ran.standard_output, ran.standard_error]
    no_shell = run_command(["printf", "%s", "a | b; $HOME"])
    seen["run_command runs no shell"] = [no_shell.error, no_shell.exit_code, no_shell.standard_output]
    no_program = run_command(["/no/such/program"])
    seen["run_command of a program that is missing"] = [no_program.error != "", no_program.exit_code]
    environment = run_command(["/usr/bin/env"]).standard_output.splitlines()
    seen["environment"] = sorted([line for line in environment if line.split("=")[0] in ["QUEUE_NAME", "USER", "LANG", "MMWATCHDOG_MODULE_HELPER"]])
    seen["directory"] = run_command(["/bin/sh", "-c", "pwd -P"]).standard_output.strip()

    answer = http_request(method = "POST", url = inputs["service_url"] + "/jobs?state=waiting", headers = {"X-Queue": "mail"}, body = "how many?")
    seen["http_request"] = [answer.error, answer.http_status_code, answer.headers.get("X-Answer"), answer.body]
    redirect = http_request("GET", inputs["service_url"] + "/redirect")
    seen["http_request does not follow a redirect"] = [redirect.error, redirect.http_status_code, redirect.headers.get("Location")]
    nobody = http_request("GET", inputs["closed_url"])
    seen["http_request to an address where nothing listens"] = [nobody.error != "", "secret" in nobody.error, nobody.http_status_code, nobody.body]
    trusted = http_request("GET", inputs["https_url"], trusted_certificate_authority_file = inputs["trusted_certificate_authority_file"])
    seen["http_request with a certificate authority file"] = [trusted.error, trusted.http_status_code, trusted.body]
    untrusted = http_request("GET", inputs["https_url"])
    seen["http_request verifies the certificate"] = [untrusted.error != "", "secret" in untrusted.error, untrusted.http_status_code]
    no_authorities = http_request("GET", inputs["https_url"], trusted_certificate_authority_file = inputs["file_path"])
    seen["http_request with a file that holds no certificate"] = [no_authorities.error != "", no_authorities.http_status_code]

    exchange = tcp_exchange(inputs["tcp_address"], text_to_send = "PING\r\n", read_until_text = "\n")
    seen["tcp_exchange"] = [exchange.error, exchange.received]
    opened = tcp_exchange(inputs["tcp_address"])
    seen["tcp_exchange that only opens a connection"] = [opened.error, opened.received]
    closed = tcp_exchange(inputs["closed_address"], "PING\r\n", "\n")
    seen["tcp_exchange to an address where nothing listens"] = [closed.error != "", closed.received]

    packet = udp_exchange(inputs["udp_address"], text_to_send = "STATUS")
    seen["udp_exchange"] = [packet.error, packet.received]
    unheard = udp_exchange(inputs["closed_address"], "STATUS")
    seen["udp_exchange to an address where nothing listens"] = [unheard.error != "", unheard.received]

    before = read_state()
    write_state({"runs": 1, "names": ["a", "b"], "ratio": 0.5, "nothing": None, "done": True})
    seen["state"] = [before, read_state()]
    seen["json"] = [json.encode({"b": [1, 2.5, None, True], "a": "x"}), json.decode('{"n": 3, "list": ["x"]}')]
    return {"status": "ok", "message": json.encode(seen)}
`

// A script touches the node only through the primitives: read_file,
// run_command, http_request, tcp_exchange, udp_exchange, read_state, and
// write_state, with json.encode and json.decode beside them. Each of the
// first five returns its fields and an error text, which is empty when the
// call worked; a call that failed does not stop the script. The script runs with the run
// settings of its check: the account, the directory, and the environment,
// without MMWATCHDOG_MODULE_HELPER, which is for executables.
func TestAScriptReadsTheNodeThroughThePrimitives(t *testing.T) {
	t.Parallel()
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.URL.Path == "/redirect":
			http.Redirect(w, r, "/jobs", http.StatusFound)
		case r.Method == "POST" && r.URL.RequestURI() == "/jobs?state=waiting" && r.Header.Get("X-Queue") == "mail" && string(body) == "how many?":
			w.Header().Set("X-Answer", "from the service")
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, "780")
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer service.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "over TLS") }))
	defer secure.Close()
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	go func() {
		for {
			conn, e := listener.Accept()
			if e != nil {
				return
			}
			go func() {
				defer conn.Close()
				if line, _ := bufio.NewReader(conn).ReadString('\n'); line == "PING\r\n" {
					io.WriteString(conn, "PONG\r\n")
				}
			}()
		}
	}()
	closedAddress := testhelpers.AddressWhereNothingListens(t, "tcp")

	files := t.TempDir()
	file, large, authorities := filepath.Join(files, "pending"), filepath.Join(files, "large"), filepath.Join(files, "ca.pem")
	for path, content := range map[string][]byte{file: []byte("780\n"), large: bytes.Repeat([]byte("x"), checksandalerts.ScriptReadFileLimit+1), authorities: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: secure.Certificate().Raw})} {
		if e := os.WriteFile(path, content, 0600); e != nil {
			t.Fatal(e)
		}
	}
	packets, e := net.ListenPacket("udp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer packets.Close()
	go func() {
		packet := make([]byte, 1500)
		for {
			n, from, e := packets.ReadFrom(packet)
			if e != nil {
				return
			}
			packets.WriteTo([]byte("you sent "+string(packet[:n])), from)
		}
	}()
	inputs := map[string]any{"file_path": file, "large_file_path": large, "service_url": service.URL, "closed_url": "http://" + closedAddress + "/services/secret", "https_url": secure.URL + "/services/secret",
		"trusted_certificate_authority_file": authorities, "tcp_address": listener.Addr().String(), "closed_address": closedAddress, "udp_address": packets.LocalAddr().String()}
	types := map[string]string{}
	for name := range inputs {
		types[name] = "string"
	}
	modules := t.TempDir()
	testhelpers.InstallStarlarkModule(t, modules, "every_primitive", testhelpers.TestModuleDescription("every_primitive", checksandalerts.UsedAsCheck, types, nil), primitivesScript)
	check := testhelpers.ModuleCheck(t, modules, "every_primitive", inputs)
	call := check.Inputs.(*checksandalerts.ModuleCall)
	call.Env = map[string]string{"QUEUE_NAME": "mail"}
	o := checksandalerts.ObserveCheck(context.Background(), check)
	if o.Status != "ok" {
		t.Fatalf("%+v", o)
	}
	var seen map[string]json.RawMessage
	if e := json.Unmarshal([]byte(o.Message), &seen); e != nil {
		t.Fatal(e, o.Message)
	}
	directory, e := filepath.EvalSymlinks(call.RunFromDirectory)
	if e != nil {
		t.Fatal(e)
	}
	quoted := func(text string) string { b, _ := json.Marshal(text); return string(b) }
	want := map[string]string{
		"read_file":                                          `["","780\n"]`,
		"read_file of a file that is missing":                `[true,""]`,
		"read_file of a file of more than 1 MiB":             `[true,""]`,
		"run_command":                                        `["",3,"in","err"]`,
		"run_command runs no shell":                          `["",0,"a | b; $HOME"]`,
		"run_command of a program that is missing":           `[true,-1]`,
		"environment":                                        `["LANG=C.UTF-8","QUEUE_NAME=mail",` + quoted("USER="+call.RunAsUserAccount) + `]`,
		"directory":                                          quoted(directory),
		"http_request":                                       `["",201,"from the service","780"]`,
		"http_request does not follow a redirect":            `["",302,"/jobs"]`,
		"http_request to an address where nothing listens":   `[true,false,0,""]`,
		"http_request with a certificate authority file":     `["",200,"over TLS"]`,
		"http_request verifies the certificate":              `[true,false,0]`,
		"http_request with a file that holds no certificate": `[true,0]`,
		"tcp_exchange":                                       `["","PONG\r\n"]`,
		"tcp_exchange that only opens a connection":          `["",""]`,
		"tcp_exchange to an address where nothing listens":   `[true,""]`,
		"udp_exchange":                                       `["","you sent STATUS"]`,
		"udp_exchange to an address where nothing listens":   `[true,""]`,
		"state": `[null,{"done":true,"names":["a","b"],"nothing":null,"ratio":0.5,"runs":1}]`,
		"json":  `["{\"a\":\"x\",\"b\":[1,2.5,null,true]}",{"list":["x"],"n":3}]`,
	}
	for name := range seen {
		if want[name] == "" {
			t.Errorf("the script reported %s, which the test does not expect", name)
		}
	}
	for name, expected := range want {
		if string(seen[name]) != expected {
			t.Errorf("%s:\n got %s\nwant %s", name, seen[name], expected)
		}
	}
}

// A script that MMWatchdog cannot run to a result makes the observation unknown,
// with a message that names the script and the line: an error in the
// script, a script without the function run_check, a result that is no
// dictionary or that the protocol does not allow, and a script that takes
// more steps than one run may take. Starlark itself has no while loop, no
// recursion, and no way to load another file.
func TestAScriptThatMMWatchdogCannotRunToAResult(t *testing.T) {
	t.Parallel()
	modules := t.TempDir()
	script := filepath.Join(modules, "broken", checksandalerts.ModuleScriptFile)
	description := testhelpers.TestModuleDescription("broken", checksandalerts.UsedAsCheck, map[string]string{"queue_file_path": "string"}, map[string]string{"queued_jobs": "count"})
	// The process that runs the script ends with an exit status other than
	// zero and writes the error as the last line of its standard error,
	// after everything that the script printed. In a test that process is
	// the test binary (testhelpers.RunTests).
	ended := "the script of the module broken ended with an error: " + script
	notValid := "the module broken wrote a result that is not valid: "
	for _, tc := range []struct{ name, script, want string }{
		{"an error in the function", "def run_check(inputs, file_contents):\n    waiting = 780\n    return {\"status\": \"ok\", \"message\": inputs[\"queue\"]}\n", ended + `:3: key "queue" not in dict`},
		{"a primitive that is called wrongly", "def run_check(inputs, file_contents):\n    return {\"status\": \"ok\", \"message\": read_file(7).content}\n", ended + ":2: read_file: for parameter path: got int, want string"},
		{"a program that is no list", "def run_check(inputs, file_contents):\n\n    return {\"status\": \"ok\", \"message\": run_command(\"systemctl is-active myapp.service\").standard_output}\n", ended + ":3: run_command: for parameter arguments: got string, want list"},
		{"a program whose arguments are no strings", "def run_check(inputs, file_contents):\n    return {\"status\": \"ok\", \"message\": run_command([\"sleep\", 1]).standard_output}\n", ended + `:2: run_command: arguments must be a list of strings, such as ["systemctl", "is-active", "myapp.service"]`},
		{"no program", "def run_check(inputs, file_contents):\n    return {\"status\": \"ok\", \"message\": run_command([]).standard_output}\n", ended + ":2: run_command: arguments must name the program to run"},
		{"headers that are no strings", "def run_check(inputs, file_contents):\n    return {\"status\": \"ok\", \"message\": http_request(\"GET\", \"http://127.0.0.1:9/\", headers = {\"X-Retries\": 3}).body}\n", ended + ":2: http_request: headers must be a dictionary of strings"},
		{"a state that JSON cannot hold", "def run_check(inputs, file_contents):\n    write_state({1: \"a\"})\n    return {\"status\": \"ok\", \"message\": \"fine\"}\n", ended + ":2: write_state: dictionary key 1 must be a string"},
		{"fail", "def run_check(inputs, file_contents):\n    fail(\"the queue is gone\")\n", ended + ":2: fail: the queue is gone"},
		// A character that turns the direction of text cannot reach an
		// alert from a module's error: it would show the words in another
		// order than the message holds them.
		{"an error that turns the direction of text", "def run_check(inputs, file_contents):\n    fail(\"the queue \u202eenog si\")\n", ended + ":2: fail: the queue enog si"},
		{"an error after the script printed", "def run_check(inputs, file_contents):\n    print(\"reading\", inputs[\"queue_file_path\"])\n    print(\"mmwatchdog: this line is the script's\")\n    return {\"status\": \"ok\", \"message\": inputs[\"queue\"]}\n", ended + `:4: key "queue" not in dict`},
		{"wrong syntax", "def run_check(inputs, file_contents):\n    return {\"status\": \"ok\", \"message\": \"fine\"\n\ndef other():\n    pass\n", ended + ":4: got def, want '}'"},
		{"a name that Starlark does not have", "def run_check(inputs, file_contents):\n    queue = open(inputs[\"queue_file_path\"])\n    return {\"status\": \"ok\", \"message\": queue.read()}\n", ended + ":2: undefined: open"},
		{"a while loop", "def run_check(inputs, file_contents):\n    waiting = 0\n    while waiting < 10:\n        waiting += 1\n    return {\"status\": \"ok\", \"message\": str(waiting)}\n", ended + ":3: this Starlark dialect does not support while loops"},
		{"a loop outside a function", "names = []\nfor name in [\"a\", \"b\"]:\n    names.append(name)\n\ndef run_check(inputs, file_contents):\n    return {\"status\": \"ok\", \"message\": \"fine\"}\n", ended + ":2: for loop not within a function"},
		{"another file of code", "load(\"queue.star\", \"waiting\")\n\ndef run_check(inputs, file_contents):\n    return {\"status\": \"ok\", \"message\": str(waiting())}\n", ended + ":1: load not implemented by this application"},
		{"no function run_check", "def check(inputs):\n    return {\"status\": \"ok\", \"message\": \"fine\"}\n", ended + " defines no function run_check(inputs, file_contents)"},
		{"run_check that is no function", "run_check = {\"status\": \"ok\", \"message\": \"fine\"}\n", ended + " defines no function run_check(inputs, file_contents)"},
		{"the function of a destination module", testhelpers.ScriptOfADestination, ended + " defines no function run_check(inputs, file_contents)"},
		{"a function that takes no inputs", "def run_check():\n    return {\"status\": \"ok\", \"message\": \"fine\"}\n", ended + ":1: function run_check accepts no arguments (2 given)"},
		{"a result that is no dictionary", "def run_check(inputs, file_contents):\n    return \"ok\"\n", ended + ": run_check returned string, and it must return a dictionary with a status and a message"},
		{"no result", "def run_check(inputs, file_contents):\n    pass\n", ended + ": run_check returned NoneType, and it must return a dictionary with a status and a message"},
		{"a result that JSON cannot hold", "def run_check(inputs, file_contents):\n    return {\"status\": \"ok\", \"message\": read_file}\n", ended + ": run_check returned a result that MMWatchdog cannot write as JSON: cannot write a value of type builtin_function_or_method as JSON; use None, True, False, a number, a string, a list, or a dictionary"},
		{"a counter without end", "def run_check(inputs, file_contents):\n    waiting = 0\n    for job in range(1000000000):\n        waiting += job\n    return {\"status\": \"ok\", \"message\": str(waiting)}\n", ended + " exceeded the limit of 10000000 steps per script run"},
		// The dictionary keeps to the protocol of an executable's result,
		// without the protocol number, and to the module's description.
		{"a result without a message", "def run_check(inputs, file_contents):\n    return {\"status\": \"ok\"}\n", notValid + "a message is required"},
		{"a result with another status", "def run_check(inputs, file_contents):\n    return {\"status\": \"healthy\", \"message\": \"fine\"}\n", notValid + "the check result status must be ok, failed, or unknown"},
		{"a result with a member that the protocol does not have", "def run_check(inputs, file_contents):\n    return {\"status\": \"ok\", \"message\": \"fine\", \"queued_jobs\": 780}\n", notValid + `json: unknown field "queued_jobs"`},
		{"a measurement with its unit", "def run_check(inputs, file_contents):\n    return {\"status\": \"ok\", \"message\": \"fine\", \"measurements\": {\"queued_jobs\": {\"value\": 780, \"unit\": \"count\"}}}\n", notValid + "json: cannot unmarshal object into Go struct field ModuleResult.measurements.queued_jobs of type float64"},
		{"a measurement that the description does not list", "def run_check(inputs, file_contents):\n    return {\"status\": \"ok\", \"message\": \"fine\", \"measurements\": {\"workers\": 4}}\n", "the module broken reported the measurement workers, which its description does not list"},
	} {
		testhelpers.InstallStarlarkModule(t, modules, "broken", description, tc.script)
		start := time.Now()
		o := checksandalerts.ObserveCheck(context.Background(), testhelpers.ModuleCheck(t, modules, "broken", map[string]any{"queue_file_path": "/var/lib/myapp/pending"}))
		// MMWatchdog keeps at most 300 bytes of the error.
		if said, isError := strings.CutPrefix(tc.want, "the script of the module broken ended with an error: "); isError {
			tc.want = "the script of the module broken ended with an error: " + checksandalerts.OneLine(said)
		}
		if o.Status != "unknown" || o.Message != tc.want || time.Since(start) > 20*time.Second {
			t.Errorf("%s, after %s:\n got %s: %s\nwant unknown: %s", tc.name, time.Since(start).Round(time.Millisecond), o.Status, o.Message, tc.want)
		}
	}
	// An error in a function that run_check calls, and a function that calls
	// itself, are errors of the script too. The line is where the error
	// happened, line 2 of each script, and not the line in run_check that
	// made the call, line 5.
	for said, script := range map[string]string{
		": floored division by zero":          "def waiting(inputs):\n    return 780 // 0\n\ndef run_check(inputs, file_contents):\n    return {\"status\": \"ok\", \"message\": str(waiting(inputs))}\n",
		": function count called recursively": "def count(n):\n    return count(n - 1) if n > 0 else 0\n\ndef run_check(inputs, file_contents):\n    return {\"status\": \"ok\", \"message\": str(count(3))}\n",
	} {
		testhelpers.InstallStarlarkModule(t, modules, "broken", description, script)
		if o := checksandalerts.ObserveCheck(context.Background(), testhelpers.ModuleCheck(t, modules, "broken", nil)); o.Status != "unknown" || o.Message != ended+":2"+said {
			t.Errorf("got %s: %s\nwant unknown: %s:2%s", o.Status, o.Message, ended, said)
		}
	}
	// A script that prints far more than MMWatchdog keeps, and that fails with a
	// text of several lines, still gets its error to MMWatchdog, on one line.
	testhelpers.InstallStarlarkModule(t, modules, "broken", description, "def run_check(inputs, file_contents):\n    for line in range(3000):\n        print(\"a line that the script prints, number\", line)\n    fail(\"the queue is gone\\nand the script says so on two lines\")\n")
	if o := checksandalerts.ObserveCheck(context.Background(), testhelpers.ModuleCheck(t, modules, "broken", nil)); o.Status != "unknown" || o.Message != ended+":4: fail: the queue is gone and the script says so on two lines" ||
		!strings.Contains(o.Stderr, "a line that the script prints, number 0\n") || !strings.Contains(o.Stderr, "\n[MMWatchdog leaves out what the script printed after its first 8192 bytes]\n") || strings.Contains(o.Stderr, "number 2999") || strings.Contains(o.Stderr, "truncated") {
		t.Fatalf("%s\n%d bytes of standard error, which end with %q", o.Message, len(o.Stderr), o.Stderr[max(0, len(o.Stderr)-300):])
	}
	// What a script prints is diagnostics, like the standard error of an
	// executable: MMWatchdog keeps it beside the observation.
	testhelpers.InstallStarlarkModule(t, modules, "broken", description, "def run_check(inputs, file_contents):\n    print(\"reading\", inputs[\"queue_file_path\"])\n    return {\"status\": \"ok\", \"message\": \"fine\"}\n")
	if o := checksandalerts.ObserveCheck(context.Background(), testhelpers.ModuleCheck(t, modules, "broken", map[string]any{"queue_file_path": "/var/lib/myapp/pending"})); o.Status != "ok" || o.Message != "fine" || o.Stderr != "reading /var/lib/myapp/pending\n" {
		t.Fatalf("%+v", o)
	}
	// A script that does not end by itself ends with the check's time limit,
	// and the program that the script started ends with it: no program of a
	// run stays behind. The program writes the number of its own process
	// into a file before it sleeps.
	processFile := filepath.Join(t.TempDir(), "process")
	testhelpers.InstallStarlarkModule(t, modules, "broken", description, "def run_check(inputs, file_contents):\n    return {\"status\": \"ok\", \"message\": run_command([\"sh\", \"-c\", \"echo $$ > "+processFile+"; exec sleep 30\"]).standard_output}\n")
	// The time limit passes once the program of the script runs. A check
	// with a time limit names the limit in the message.
	for timing, want := range map[checksandalerts.Timing]string{
		{}:                         "the module broken did not finish within its time limit, so MMWatchdog stopped the module",
		{TimeLimitPerRun: "500ms"}: "the module broken did not finish within 500ms (time_limit_per_run), so MMWatchdog stopped the module",
	} {
		os.Remove(processFile)
		limit := testhelpers.NewTimeLimitThatTheTestEnds(context.Background())
		observed := make(chan checksandalerts.Observation, 1)
		go func() {
			observed <- checksandalerts.RunCheck(limit, checksandalerts.NewCheckRun(testhelpers.ModuleCheck(t, modules, "broken", nil), timing))
		}()
		process := testhelpers.StartedProgram(t, processFile)
		ended := time.Now()
		limit.End()
		// A run that the time limit did not end would take 30 seconds.
		if o := <-observed; o.Status != "unknown" || o.Message != want || time.Since(ended) > 10*time.Second {
			t.Fatalf("with the timing %+v, after %s: %+v", timing, time.Since(ended), o)
		}
		for end := time.Now().Add(testhelpers.LongestWaitForAnEvent); syscall.Kill(process, 0) == nil; time.Sleep(20 * time.Millisecond) {
			if time.Now().After(end) {
				syscall.Kill(process, syscall.SIGKILL)
				t.Fatalf("the program that the script started, process %d, still runs after the time limit ended the script", process)
			}
		}
	}
}

// A program that a script runs ended by itself when it has an exit code,
// also when something that it started still runs and holds its output open:
// MMWatchdog does not wait for that, and the script gets the exit code and the
// output. A program that writes more than 64 KiB to its standard output is
// an error of the call, in words for the author of a script.
func TestWhatAScriptLearnsOfAProgramThatItRan(t *testing.T) {
	t.Parallel()
	modules := t.TempDir()
	testhelpers.InstallStarlarkModule(t, modules, "programs", testhelpers.TestModuleDescription("programs", checksandalerts.UsedAsCheck, nil, nil), `def run_check(inputs, file_contents):
    left_running = run_command(["sh", "-c", "sleep 30 & echo started"])
    too_much = run_command(["sh", "-c", "head -c 1100000 /dev/zero | tr '\\0' x"])
    with_input = run_command(["sh", "-c", "cat; echo to-standard-error >&2; exit 3"], standard_input = "from the script")
    return {"status": "ok", "message": "%d %s [%s] / [%s] / %d %s %s [%s]" % (
        left_running.exit_code, left_running.standard_output.strip(), left_running.error,
        too_much.error,
        with_input.exit_code, with_input.standard_output, with_input.standard_error.strip(), with_input.error,
    )}
`)
	start := time.Now()
	o := checksandalerts.ObserveCheck(context.Background(), testhelpers.ModuleCheck(t, modules, "programs", nil))
	want := "0 started [] / [the program wrote more than 1 MiB to its standard output] / 3 from the script to-standard-error []"
	if o.Status != "ok" || o.Message != want || time.Since(start) > 15*time.Second {
		t.Fatalf("after %s:\n got %s: %s\nwant ok: %s", time.Since(start), o.Status, o.Message, want)
	}
}

// askSilentServerScript calls the primitive that the input primitive names,
// against a server that gives no answer, and fails the check with the error
// of the call.
const askSilentServerScript = `def run_check(inputs, file_contents):
    primitive = inputs["primitive"]
    if primitive == "tcp_exchange":
        called = tcp_exchange(inputs["tcp_address"], text_to_send = "status\n", read_until_text = "\n")
    elif primitive == "http_request":
        called = http_request("GET", "http://" + inputs["tcp_address"] + "/server-status?auto")
    elif primitive == "udp_exchange":
        called = udp_exchange(inputs["udp_address"], "status\n")
    else:
        called = run_command(["sleep", "30"])
    if called.error:
        return {"status": "failed", "message": primitive + ": " + called.error}
    return {"status": "ok", "message": primitive + " got an answer"}
`

// A server that accepts the connection and then gives no answer, as a hung
// server does, ends the call of the primitive shortly before the time limit
// of the run, with an error that the script sees. So the script decides the
// result, and a check that fails for a server that does not answer drives
// recovery. A program that does not end ends in the same way. The time
// limit of the test states its end to the module and passes only when the
// test ends it, so MMWatchdog's own end of the module at the time limit never
// races the script's result.
func TestACallOfAPrimitiveEndsShortlyBeforeTheTimeLimit(t *testing.T) {
	t.Parallel()
	silent, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	// The subtests run after this function returns.
	t.Cleanup(func() { silent.Close() })
	go func() {
		for {
			conn, e := silent.Accept()
			if e != nil {
				return
			}
			// The server reads what comes and never writes.
			go func() { defer conn.Close(); io.Copy(io.Discard, conn) }()
		}
	}()
	// A UDP socket that reads no packet answers none.
	unheard, e := net.ListenPacket("udp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { unheard.Close() })
	modules := t.TempDir()
	testhelpers.InstallStarlarkModule(t, modules, "asks_a_silent_server", testhelpers.TestModuleDescription("asks_a_silent_server", checksandalerts.UsedAsCheck, map[string]string{"primitive": "string", "tcp_address": "string", "udp_address": "string"}, nil), askSilentServerScript)
	ended := "MMWatchdog ended the call shortly before the time limit of the run (time_limit_per_run)"
	for primitive, want := range map[string]string{
		"tcp_exchange": ended,
		"http_request": ended,
		"udp_exchange": ended,
		"run_command":  "MMWatchdog ended the program shortly before the time limit of the run (time_limit_per_run)",
	} {
		t.Run(primitive, func(t *testing.T) {
			t.Parallel()
			check := testhelpers.ModuleCheck(t, modules, "asks_a_silent_server", map[string]any{"primitive": primitive, "tcp_address": silent.Addr().String(), "udp_address": unheard.LocalAddr().String()})
			timing := checksandalerts.Timing{TimeLimitPerRun: checksandalerts.Duration(testhelpers.TimeLimitThatATestLetsPass.String())}
			limit := testhelpers.NewTimeLimitThatTheTestEndsWithADeadline(context.Background(), time.Now().Add(testhelpers.TimeLimitThatATestLetsPass))
			defer limit.End()
			observed := make(chan checksandalerts.Observation, 1)
			go func() { observed <- checksandalerts.RunCheck(limit, checksandalerts.NewCheckRun(check, timing)) }()
			var o checksandalerts.Observation
			select {
			case o = <-observed:
			case <-time.After(testhelpers.LongestWaitForAnEvent):
				limit.End()
				t.Fatalf("the call did not end by itself, and MMWatchdog ended the module at the time limit: %+v", <-observed)
			}
			if o.Status != "failed" || o.Message != primitive+": "+want {
				t.Fatalf("got %s: %s\nwant failed: %s: %s", o.Status, o.Message, primitive, want)
			}
		})
	}
}

// mmwatchdog validate reads the script of each check that the
// configuration names, without running it: a script that is no Starlark,
// that uses a name that nothing defines, or that does not define run_check
// is an error with the file and the line. A deploy validates this way, so
// such a script stops the deploy, where it would otherwise make every run
// of its check unknown. The same validation names a module_state_dir that
// MMWatchdog cannot make.
func TestRuntimeValidationReadsTheScriptOfACheckWithoutRunningIt(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	// The node's other check needs nothing on the node.
	c.CheckGroups = map[string]checksandalerts.CheckGroup{
		"jobs": {Check: checksandalerts.Check{Inputs: &checksandalerts.ModuleCall{Name: "queue", ExecOptions: testhelpers.OwnOptions(t)}}},
		"port": {Check: checksandalerts.Check{Inputs: &checksandalerts.TCP{Address: "127.0.0.1:9"}}},
	}
	writeConfig(t, path, c)
	script := filepath.Join(c.ExternalModuleDir, "queue", checksandalerts.ModuleScriptFile)
	ran := filepath.Join(t.TempDir(), "ran")
	validate := func(text string) error {
		t.Helper()
		testhelpers.InstallStarlarkModule(t, c.ExternalModuleDir, "queue", testhelpers.TestModuleDescription("queue", checksandalerts.UsedAsCheck, nil, nil), text)
		loaded, e := config.Load(path)
		if e != nil {
			t.Fatal(e)
		}
		return ValidateRuntime(loaded)
	}
	for _, tc := range []struct{ name, script, want string }{
		{"a script that is right", testhelpers.ScriptOfACheck, ""},
		{"a function that has its name from an assignment", "def count(inputs, file_contents):\n    return {\"status\": \"ok\", \"message\": \"fine\"}\n\nrun_check = count\n", ""},
		// Validation reads a script and does not run it: a statement
		// outside a function would run with the script.
		{"a statement outside a function", "started = run_command([\"touch\", \"" + ran + "\"])\n\n" + testhelpers.ScriptOfACheck, ""},
		{"wrong syntax", "def run_check(inputs, file_contents):\n    return {\"status\": \"ok\" \"message\": \"fine\"}\n", script + ":2: got string literal, want '}'"},
		{"a name that nothing defines", "def run_check(inputs, file_contents):\n    queue = open(\"/var/lib/myapp/pending\")\n    return {\"status\": \"ok\", \"message\": queue.read()}\n", script + ":2: undefined: open"},
		{"a while loop", "def run_check(inputs, file_contents):\n    while True:\n        pass\n", script + ":2: this Starlark dialect does not support while loops"},
		{"no function run_check", "def check(inputs):\n    return {\"status\": \"ok\", \"message\": \"fine\"}\n", script + " defines no function run_check(inputs, file_contents)"},
		{"the function of a destination module", testhelpers.ScriptOfADestination, script + " defines no function run_check(inputs, file_contents)"},
		{"run_check that is no function", "run_check = 1\n", script + " defines no function run_check(inputs, file_contents)"},
		{"a name that the script gives twice", testhelpers.ScriptOfACheck + "\nrun_check = None\n", script + ":4: cannot reassign global run_check declared at module.star:1:5"},
		// A script that would fail at every run for a reason that shows
		// without running it.
		{"a function that takes no inputs", "\ndef run_check():\n    return {\"status\": \"ok\", \"message\": \"fine\"}\n", script + ":2: MMWatchdog calls run_check(inputs, file_contents), and the function of the script does not take these arguments"},
		{"a function that takes one argument", "def run_check(inputs):\n    return {\"status\": \"ok\", \"message\": \"fine\"}\n", script + ":1: MMWatchdog calls run_check(inputs, file_contents), and the function of the script does not take these arguments"},
		{"a function that takes three arguments", "def run_check(inputs, file_contents, state):\n    return {\"status\": \"ok\", \"message\": \"fine\"}\n", script + ":1: MMWatchdog calls run_check(inputs, file_contents), and the function of the script does not take these arguments"},
		{"a function with an argument that must be named", "def run_check(inputs, *, state):\n    return {\"status\": \"ok\", \"message\": \"fine\"}\n", script + ":1: MMWatchdog calls run_check(inputs, file_contents), and the function of the script does not take these arguments"},
		{"any number of arguments and one that must be named", "def run_check(*inputs, state):\n    return {\"status\": \"ok\", \"message\": \"fine\"}\n", script + ":1: MMWatchdog calls run_check(inputs, file_contents), and the function of the script does not take these arguments"},
		{"a lambda that takes no inputs", "run_check = lambda: {\"status\": \"ok\", \"message\": \"fine\"}\n", script + ":1: MMWatchdog calls run_check(inputs, file_contents), and the function of the script does not take these arguments"},
		{"another file of code", "load(\"queue.star\", \"waiting\")\n\n" + testhelpers.ScriptOfACheck, script + ":1: a script is one file and cannot load another file of code"},
		// Functions that MMWatchdog can call with the two arguments.
		{"a third argument with a default", "def run_check(inputs, file_contents, state = None):\n    return {\"status\": \"ok\", \"message\": \"fine\"}\n", ""},
		{"any number of arguments", "def run_check(*arguments, **named):\n    return {\"status\": \"ok\", \"message\": \"fine\"}\n", ""},
		{"a lambda", "run_check = lambda inputs, file_contents: {\"status\": \"ok\", \"message\": \"fine\"}\n", ""},
	} {
		e := validate(tc.script)
		if tc.want == "" && e != nil || tc.want != "" && (e == nil || !strings.HasSuffix(e.Error(), ": check_groups.jobs.queue: "+tc.want)) {
			t.Errorf("%s:\n got %v\nwant %s", tc.name, e, tc.want)
		}
	}
	if _, e := os.Stat(ran); !os.IsNotExist(e) {
		t.Fatal("validation ran a script:", e)
	}
	if e := os.Remove(script); e != nil {
		t.Fatal(e)
	}
	// MMWatchdog makes module_state_dir itself, in a directory that is there.
	testhelpers.InstallStarlarkModule(t, c.ExternalModuleDir, "queue", testhelpers.TestModuleDescription("queue", checksandalerts.UsedAsCheck, nil, nil), testhelpers.ScriptOfACheck)
	missing := filepath.Join(t.TempDir(), "not-there")
	c.ModuleStateDir = filepath.Join(missing, "module_state")
	writeConfig(t, path, c)
	loaded, e := config.Load(path)
	if e != nil {
		t.Fatal(e)
	}
	want := "module_state_dir: " + missing + " must be an existing directory, so that MMWatchdog can create " + c.ModuleStateDir + " in that directory"
	if e := ValidateRuntime(loaded); e == nil || !strings.HasSuffix(e.Error(), ": "+want) {
		t.Fatalf("got %v\nwant %s", e, want)
	}
	// A node without a check whose module is not core needs no such directory.
	delete(c.CheckGroups, "jobs")
	writeConfig(t, path, c)
	if loaded, e = config.Load(path); e != nil {
		t.Fatal(e)
	}
	if e := ValidateRuntime(loaded); e != nil {
		t.Fatal(e)
	}
}
