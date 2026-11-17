package checksandalerts_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/testhelpers"
)

func TestShellContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		script string
		codes  []int
		want   string
		exit   int
	}{
		{"false | cat", []int{0}, "failed", 1}, {"false; printf ok", []int{0}, "ok", 0}, {"exit 2", []int{0, 2}, "ok", 2}, {"exit 0", []int{2}, "failed", 0}, {"printf '%s' \"${UNSET:-fine}\"", []int{0}, "ok", 0},
	} {
		o := checksandalerts.ObserveCheck(context.Background(), checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: tc.script, SuccessExitCodes: tc.codes, ExecOptions: testhelpers.OwnOptions(t)}})
		if o.Status != tc.want || o.ExitCode == nil || *o.ExitCode != tc.exit {
			t.Fatalf("%s: %+v", tc.script, o)
		}
	}
}

// A run that time_limit_per_run ends says so in plain words, with the limit,
// and has no exit code: -1 is not a code that the script wrote.
func TestShellRunThatTheTimeLimitEndsSaysSoAndHasNoExitCode(t *testing.T) {
	t.Parallel()
	started := filepath.Join(t.TempDir(), "started")
	script := "echo $$ > " + testhelpers.ShellQuote(started) + "; sleep 30"
	limit := testhelpers.NewTimeLimitThatTheTestEnds(context.Background())
	run := checksandalerts.NewCheckRun(checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: script, SuccessExitCodes: []int{0}, ExecOptions: testhelpers.OwnOptions(t)}}, checksandalerts.Timing{TimeLimitPerRun: "5s"})
	ended := make(chan checksandalerts.Observation, 1)
	go func() { ended <- checksandalerts.RunCheck(limit, run) }()
	testhelpers.StartedProgram(t, started)
	limit.End()
	o := <-ended
	want := "the run took longer than 5s (time_limit_per_run), so MMWatchdog ended it"
	if o.Status != "unknown" || o.Message != want || o.ExitCode != nil {
		t.Fatalf("status %q, message %q, exit code %v; want unknown, %q, no exit code", o.Status, o.Message, o.ExitCode, want)
	}
}

// A script that names a command or a file that Bash cannot find or cannot
// run did not look at what it checks: MMWatchdog cannot check, and the message says
// what Bash said. A program that ends with 127 by itself, such as one whose
// shared library is missing, fails the check like any other exit code, and
// so does a script that exits with 127 itself. An exit code that the check
// lists as healthy stays healthy.
func TestShellCheckThatCannotFindOrRunACommandIsUnknown(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	notExecutable := filepath.Join(dir, "selftest")
	if e := os.WriteFile(notExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o644); e != nil {
		t.Fatal(e)
	}
	missing := filepath.Join(dir, "missing-selftest")
	encoder := filepath.Join(dir, "lame")
	testhelpers.WriteStandInProgram(t, encoder, "#!/bin/sh\necho 'lame: error while loading shared libraries: libmp3lame.so.0: cannot open shared object file: No such file or directory' >&2\nexit 127\n")
	// Bash's own words start with its name, which differs between releases.
	for _, tc := range []struct {
		script                        string
		codes                         []int
		want, mmwatchdogSays, bashEnd string
	}{
		{missing, []int{0}, "unknown", "command exited 127, which Bash uses when it cannot find a command: ", missing + ": No such file or directory"},
		{"echo checking; myapp-cli-that-is-not-installed selftest", []int{0}, "unknown", "command exited 127, which Bash uses when it cannot find a command: ", " myapp-cli-that-is-not-installed: command not found"},
		{notExecutable, []int{0}, "unknown", "command exited 126, which Bash uses when it cannot run a command: ", notExecutable + ": Permission denied"},
		{encoder + " --encode tone.wav", []int{0}, "failed", "command exited 127", ""},
		{"exit 127", []int{0}, "failed", "command exited 127", ""},
		{"exit 126", []int{0}, "failed", "command exited 126", ""},
		{missing, []int{0, 127}, "ok", "command exited 127", ""},
	} {
		o := checksandalerts.ObserveCheck(context.Background(), checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: tc.script, SuccessExitCodes: tc.codes, ExecOptions: testhelpers.OwnOptions(t)}})
		if o.Status != tc.want || !strings.HasPrefix(o.Message, tc.mmwatchdogSays) || !strings.HasSuffix(o.Message, tc.bashEnd) || (tc.bashEnd == "" && o.Message != tc.mmwatchdogSays) {
			t.Errorf("%s with %v: %+v", tc.script, tc.codes, o)
		}
	}
}

// The example in the shell module's description, which "mmwatchdog describe-module
// shell" prints, is healthy when the self-test passes, also when the
// self-test prints more after the line that the example looks for. A grep
// that stops reading at the first match, as grep -q does, would end the
// self-test with a broken pipe, and pipefail would fail the healthy run.
func TestTheShellExampleOfTheDescriptionIsHealthyWhenTheSelfTestPasses(t *testing.T) {
	t.Parallel()
	programs := t.TempDir()
	testhelpers.WriteStandInProgram(t, filepath.Join(programs, "myapp-cli"), "#!/bin/sh\necho 'all tests passed'\nyes 'more output of the self-test' | head -n 100000\n")
	var example struct {
		Shell checksandalerts.Shell `yaml:"shell"`
	}
	for _, module := range checksandalerts.CoreCheckModules {
		if module.Name == "shell" {
			if e := yaml.Unmarshal([]byte(module.Description.Examples[0].Configuration), &example); e != nil {
				t.Fatal(e)
			}
		}
	}
	if problem := example.Shell.Validate(checksandalerts.InputValidation{}); problem != nil || example.Shell.Script == "" {
		t.Fatal(problem, example)
	}
	example.Shell.ExecOptions = testhelpers.OwnOptions(t)
	example.Shell.Env = map[string]string{"PATH": programs + ":/usr/bin:/bin"}
	if o := checksandalerts.ObserveCheck(context.Background(), checksandalerts.Check{Inputs: &example.Shell}); o.Status != "ok" {
		t.Fatalf("%s: %+v", example.Shell.Script, o)
	}
}

func TestTimeoutAndOutputLimits(t *testing.T) {
	t.Parallel()
	for _, script := range []string{"sleep 20 | cat", "while :; do printf 'long output'; done"} {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		start := time.Now()
		o := checksandalerts.ObserveCheck(ctx, checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: script, SuccessExitCodes: []int{0, 137}, ExecOptions: testhelpers.OwnOptions(t)}})
		cancel()
		// A run that the time limit did not end would take 20 seconds.
		if o.Status != "unknown" || time.Since(start) > 10*time.Second {
			t.Fatal(o, time.Since(start))
		}
	}
	o := checksandalerts.ObserveCheck(context.Background(), checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "printf '%70000s' x", SuccessExitCodes: []int{0}, ExecOptions: testhelpers.OwnOptions(t)}})
	if o.Status != "unknown" || !strings.Contains(o.Message, "exceeds") {
		t.Fatal(o)
	}
}

// An answer with a status code that the check does not expect fails the
// check at once: the check does not wait for the body, which can be slow or
// have no end. A check whose inputs nobody validated does not stop MMWatchdog when
// its request fails.
func TestHTTPCheckDoesNotWaitForTheBodyOfAnUnexpectedAnswer(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte("the start of a body"))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer node.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	o := checksandalerts.ObserveCheck(ctx, checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: node.URL, Method: "GET", ExpectedHTTPStatusCodes: []int{200}, ResponseBodyContains: "ready"}})
	if o.Status != "failed" || o.Message != "expected HTTP 200, received 500" || time.Since(start) > 3*time.Second {
		t.Fatalf("%+v after %s", o, time.Since(start))
	}
	address := "http://" + testhelpers.AddressWhereNothingListens(t, "tcp")
	if o := checksandalerts.ObserveCheck(context.Background(), checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: address}}); o.Status != "failed" || !strings.HasPrefix(o.Message, "Get "+strconv.Quote(address)+": ") {
		t.Fatalf("%+v", o)
	}
}

func TestHTTPContentAndStatus(t *testing.T) {
	t.Parallel()
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bad":
			w.WriteHeader(503)
		case "/redirect":
			http.Redirect(w, r, "/good", http.StatusFound)
		default:
			w.Write([]byte("ready healthy"))
		}
	}))
	defer node.Close()
	for _, tc := range []struct{ path, bodyContains, forbidden, bodyMatchesRegex, status string }{
		{"/good", "ready", "broken", "healthy$", "ok"}, {"/good", "missing", "", "", "failed"}, {"/good", "", "ready", "", "failed"}, {"/good", "", "", "^broken", "failed"}, {"/bad", "", "", "", "failed"}, {"/redirect", "", "", "", "failed"},
	} {
		o := checksandalerts.ObserveCheck(context.Background(), checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: node.URL + tc.path, ExpectedHTTPStatusCodes: []int{200}, ResponseBodyContains: tc.bodyContains, ResponseBodyDoesNotContain: tc.forbidden, ResponseBodyMatchesRegex: tc.bodyMatchesRegex}})
		if o.Status != tc.status {
			t.Fatal(o)
		}
	}
	// Several status codes can count as healthy; a redirect is not followed,
	// so its own status is the one compared.
	for _, tc := range []struct {
		path            string
		codes           []int
		status, message string
	}{
		{"/redirect", []int{200, 302}, "ok", "HTTP 302 and configured content match"},
		{"/good", []int{200, 302}, "ok", "HTTP 200 and configured content match"},
		{"/bad", []int{200}, "failed", "expected HTTP 200, received 503"},
		{"/bad", []int{200, 302}, "failed", "expected HTTP 200 or 302, received 503"},
		{"/bad", []int{200, 301, 302}, "failed", "expected HTTP 200, 301, or 302, received 503"},
	} {
		o := checksandalerts.ObserveCheck(context.Background(), checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: node.URL + tc.path, Method: "GET", ExpectedHTTPStatusCodes: tc.codes}})
		if o.Status != tc.status || o.Message != tc.message {
			t.Fatalf("%s %v: %+v", tc.path, tc.codes, o)
		}
	}
}

// Request options reach the endpoint; credential and CA files are read for
// each request, and one that cannot be read leaves health unknown.
func TestHTTPRequestOptions(t *testing.T) {
	t.Parallel()
	node := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, basic := r.BasicAuth()
		authorized := r.Header.Get("Authorization") == "Bearer test-http-token-value" || basic && user == "monitor" && password == "pass:word"
		if r.Host != "example.com" || r.Header.Get("X-Check") != "mmwatchdog" || !authorized {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte("ready"))
	}))
	defer node.Close()
	dir := t.TempDir()
	ca, token, basic, notPEM := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "token"), filepath.Join(dir, "basic"), filepath.Join(dir, "not-pem")
	otherCA, _ := testhelpers.TestCertificate(t, dir)
	for path, value := range map[string]string{
		ca:     string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: node.Certificate().Raw})),
		token:  "test-http-token-value\n",
		basic:  "monitor:pass:word\n",
		notPEM: "not a certificate",
	} {
		if e := os.WriteFile(path, []byte(value), 0600); e != nil {
			t.Fatal(e)
		}
	}
	missing := filepath.Join(dir, "missing")
	check := func(change func(h *checksandalerts.HTTP)) checksandalerts.Observation {
		h := &checksandalerts.HTTP{URL: node.URL, Method: "GET", Headers: map[string]string{"host": "example.com", "X-Check": "mmwatchdog"}, BearerTokenFile: token, TrustedCertificateAuthorityFile: ca, ExpectedHTTPStatusCodes: []int{200}, ResponseBodyContains: "ready"}
		change(h)
		return checksandalerts.ObserveCheck(context.Background(), checksandalerts.Check{Inputs: h})
	}
	for _, tc := range []struct {
		name, want string
		change     func(h *checksandalerts.HTTP)
	}{
		{"bearer token", "ok", func(h *checksandalerts.HTTP) {}},
		{"basic authentication", "ok", func(h *checksandalerts.HTTP) { h.BearerTokenFile, h.BasicAuthFile = "", basic }},
		{"HEAD", "ok", func(h *checksandalerts.HTTP) { h.Method, h.ResponseBodyContains = "HEAD", "" }},
		{"missing header", "failed", func(h *checksandalerts.HTTP) { delete(h.Headers, "X-Check") }},
		{"no credential", "failed", func(h *checksandalerts.HTTP) { h.BearerTokenFile = "" }},
		{"system trust store", "failed", func(h *checksandalerts.HTTP) { h.TrustedCertificateAuthorityFile = "" }},
		{"skip certificate checking", "ok", func(h *checksandalerts.HTTP) {
			h.TrustedCertificateAuthorityFile = ""
			h.DangerouslySkipServerCertificateCheck = true
		}},
		{"another CA", "failed", func(h *checksandalerts.HTTP) { h.TrustedCertificateAuthorityFile = otherCA }},
		{"unreadable token", "unknown", func(h *checksandalerts.HTTP) { h.BearerTokenFile = missing }},
		{"unreadable basic authentication", "unknown", func(h *checksandalerts.HTTP) { h.BearerTokenFile, h.BasicAuthFile = "", missing }},
		{"invalid basic authentication", "unknown", func(h *checksandalerts.HTTP) { h.BearerTokenFile, h.BasicAuthFile = "", notPEM }},
		{"unreadable CA", "unknown", func(h *checksandalerts.HTTP) { h.TrustedCertificateAuthorityFile = missing }},
		{"CA without certificates", "unknown", func(h *checksandalerts.HTTP) { h.TrustedCertificateAuthorityFile = notPEM }},
	} {
		if o := check(tc.change); o.Status != tc.want {
			t.Errorf("%s: got %+v, want %s", tc.name, o, tc.want)
		}
	}
	// The Host header is also the TLS name sent and verified.
	if o := check(func(h *checksandalerts.HTTP) { h.Headers["host"] = "other.example" }); o.Status != "failed" || !strings.Contains(o.Message, "not other.example") {
		t.Fatal("TLS name from Host", o)
	}
	// A rotated token applies to the next request without a reload.
	if e := os.WriteFile(token, []byte("rotated-http-token-value\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if o := check(func(h *checksandalerts.HTTP) {}); o.Status != "failed" || !strings.Contains(o.Message, "401") {
		t.Fatal("rotated token", o)
	}
}

// A TLS check's CA file replaces the system trust store.
func TestTLSCAFile(t *testing.T) {
	t.Parallel()
	node := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer node.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if e := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: node.Certificate().Raw}), 0600); e != nil {
		t.Fatal(e)
	}
	for file, want := range map[string]string{ca: "ok", "": "failed", ca + ".missing": "unknown"} {
		o := checksandalerts.ObserveCheck(context.Background(), checksandalerts.Check{Inputs: &checksandalerts.TLS{Address: node.Listener.Addr().String(), ServerName: "example.com", TrustedCertificateAuthorityFile: file, FailWhenCertificateExpiresWithin: "1h"}})
		if o.Status != want {
			t.Errorf("trusted_certificate_authority_file %q: got %+v, want %s", file, o, want)
		}
	}
}

// A tls check completes the TLS handshake and sends nothing after it, so it
// checks a server that speaks TLS and not HTTP. It fails when the
// certificate expires within fail_when_certificate_expires_within, 30d
// without the input, names the time of expiry, and records the time left.
func TestTLSCheckFailsWhenTheCertificateExpiresWithinItsTime(t *testing.T) {
	t.Parallel()
	// The certificate expires in one hour.
	certificatePEM, keyPEM, e := testhelpers.LocalhostCertificatePEM()
	if e != nil {
		t.Fatal(e)
	}
	pair, e := tls.X509KeyPair(certificatePEM, keyPEM)
	if e != nil {
		t.Fatal(e)
	}
	block, _ := pem.Decode(certificatePEM)
	certificate, e := x509.ParseCertificate(block.Bytes)
	if e != nil {
		t.Fatal(e)
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if e := os.WriteFile(ca, certificatePEM, 0600); e != nil {
		t.Fatal(e)
	}
	listener, e := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { listener.Close() })
	// The number of bytes that each connection sent after its handshake.
	sentAfterTheHandshake := make(chan int64, 4)
	go func() {
		for {
			connection, e := listener.Accept()
			if e != nil {
				return
			}
			go func() {
				defer connection.Close()
				if connection.(*tls.Conn).Handshake() != nil {
					sentAfterTheHandshake <- -1
					return
				}
				n, _ := io.Copy(io.Discard, connection)
				sentAfterTheHandshake <- n
			}()
		}
	}()
	expiresAt := "TLS certificate expires at " + certificate.NotAfter.UTC().Format(time.RFC3339)
	for _, tc := range []struct{ within, status, message string }{
		{"30d", "failed", expiresAt},
		{"30m", "ok", "TLS certificate is valid"},
		{"2h", "failed", expiresAt},
	} {
		inputs := &checksandalerts.TLS{Address: listener.Addr().String(), ServerName: "localhost", TrustedCertificateAuthorityFile: ca, FailWhenCertificateExpiresWithin: checksandalerts.Duration(tc.within)}
		if problem := inputs.Validate(checksandalerts.InputValidation{}); problem != nil {
			t.Fatal(problem)
		}

		o := checksandalerts.ObserveCheck(context.Background(), checksandalerts.Check{Inputs: inputs})
		left := o.Measurements["certificate_expires_in_seconds"]
		if o.Status != tc.status || o.Message != tc.message || left.Unit != "seconds" || left.Value <= 0 || left.Value > time.Hour.Seconds() {
			t.Errorf("fail_when_certificate_expires_within %q: got %+v, want %s: %s", tc.within, o, tc.status, tc.message)
		}
		select {
		case n := <-sentAfterTheHandshake:
			if n != 0 {
				t.Errorf("fail_when_certificate_expires_within %q: the check sent %d bytes after the handshake", tc.within, n)
			}
		case <-time.After(testhelpers.LongestWaitForAnEvent):
			t.Fatal("the check's connection did not end")
		}
	}
}

// An executable module gets one JSON object on its standard input: the
// protocol, the check's inputs as written, and the state directory of the
// check. Its answer with exit status 0 is the observation. A nonzero exit
// status, another protocol, a key that is written twice, more than one JSON
// value, a member that the protocol does not have, and a result that does
// not keep to the module's description are execution errors: the
// observation is unknown, its message names the module and says what is
// wrong, and MMWatchdog keeps what the module wrote.
func TestModuleProtocol(t *testing.T) {
	t.Parallel()
	dir, request := t.TempDir(), filepath.Join(t.TempDir(), "request.json")
	description := testhelpers.TestModuleDescription("queue", checksandalerts.UsedAsCheck, map[string]string{"queue_file_path": "string", "longest_wait_seconds": "integer"}, map[string]string{"queued_jobs": "count"})
	notValid := "the module queue wrote a result that is not valid: "
	for _, tc := range []struct {
		out           string
		exit          int
		want, message string
	}{
		{`{"protocol":1,"status":"failed","message":"queue full"}`, 0, "failed", "queue full"},
		{`{"protocol":1,"status":"ok","message":"780 queued jobs","measurements":{"queued_jobs":780}}`, 0, "ok", "780 queued jobs"},
		{`{"protocol":1,"status":"unknown","message":"cannot read the queue file"}`, 0, "unknown", "cannot read the queue file"},
		{`{"protocol":1,"status":"ok","message":"fine"}`, 2, "unknown", "the module queue ended with exit status 2: diagnostic"},
		{`{"protocol":2,"status":"ok","message":"fine"}`, 0, "unknown", notValid + "unsupported module result protocol 2; expected protocol: 1"},
		{`{"protocol":1,"status":"ok","status":"failed","message":"fine"}`, 0, "unknown", notValid + `duplicate JSON key "status"`},
		{`{"protocol":1,"status":"ok","message":"fine"} {}`, 0, "unknown", notValid + "expected one JSON object"},
		{`{"protocol":1,"status":"ok","message":"fine","extra":true}`, 0, "unknown", notValid + `json: unknown field "extra"`},
		{`{"protocol":1,"status":"delivered","message":"sent"}`, 0, "unknown", notValid + "the check result status must be ok, failed, or unknown"},
		{`{"protocol":1,"status":"ok"}`, 0, "unknown", notValid + "a message is required"},
		{`not JSON`, 0, "unknown", notValid + "invalid character 'o' in literal null (expecting 'u')"},
		// A measurement is a plain number, and the description lists it.
		{`{"protocol":1,"status":"ok","message":"fine","measurements":{"queued_jobs":{"value":780,"unit":"count"}}}`, 0, "unknown", notValid + "json: cannot unmarshal object into Go struct field ModuleResult.measurements.queued_jobs of type float64"},
		{`{"protocol":1,"status":"ok","message":"fine","measurements":{"queued_jobs":780,"workers":4}}`, 0, "unknown", "the module queue reported the measurement workers, which its description does not list"},
		// Only a module whose description states one_incident_for_each_finding
		// reports findings.
		{`{"protocol":1,"status":"failed","message":"1 queue is full","findings":[{"id":"mail","message":"the queue mail is full"}]}`, 0, "unknown", "the module queue reported findings, and its description does not state one_incident_for_each_finding"},
	} {
		testhelpers.InstallModule(t, dir, "queue", description, "cat > "+testhelpers.ShellQuote(request)+"\nprintf '%s' "+testhelpers.ShellQuote(tc.out)+"\nprintf diagnostic >&2\nexit "+strconv.Itoa(tc.exit))
		o := checksandalerts.ObserveCheck(context.Background(), testhelpers.ModuleCheck(t, dir, "queue", map[string]any{"queue_file_path": "/var/lib/myapp/pending", "longest_wait_seconds": 42}))
		if o.Status != tc.want || o.Message != tc.message || o.Stderr != "diagnostic" {
			t.Fatalf("%s:\n got %+v\nwant %s: %s", tc.out, o, tc.want, tc.message)
		}
		if failedToRun := tc.message != "" && strings.HasPrefix(tc.message, "the module queue "); failedToRun && (o.Stdout != tc.out || o.ExitCode == nil || *o.ExitCode != tc.exit) {
			t.Fatal("lost module error diagnostics", o)
		}
	}
	// A run that belongs to no check of a node gets an empty state directory
	// of its own, which is gone after the run.
	var got struct {
		Protocol       int               `json:"protocol"`
		Inputs         map[string]any    `json:"inputs"`
		StateDirectory string            `json:"state_dir"`
		FileContents   map[string]string `json:"file_contents"`
	}
	written, _ := os.ReadFile(request)
	if e := checksandalerts.DecodeJSON(written, &got); e != nil || got.Protocol != 1 || !reflect.DeepEqual(got.Inputs, map[string]any{"queue_file_path": "/var/lib/myapp/pending", "longest_wait_seconds": json.Number("42")}) || !filepath.IsAbs(got.StateDirectory) {
		t.Fatalf("the request of a check module: %s: %v", written, e)
	}
	if !strings.HasPrefix(string(written), `{"protocol":1,"inputs":{"longest_wait_seconds":42,"queue_file_path":"/var/lib/myapp/pending"},"state_dir":"`) {
		t.Fatalf("the request of a check module: %s", written)
	}
	if _, e := os.Stat(got.StateDirectory); !os.IsNotExist(e) {
		t.Fatal("the state directory of a run without a check stayed:", got.StateDirectory, e)
	}
}

// A module that cannot run to its end makes the observation unknown: more
// output than MMWatchdog reads, a run past the time limit, an end by a signal, and an
// executable that is gone since the configuration was loaded.
func TestModuleThatCannotRunToItsEnd(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	description := testhelpers.TestModuleDescription("queue", checksandalerts.UsedAsCheck, nil, nil)
	for _, tc := range []struct{ name, script, want string }{
		{"too much output", "head -c 70000 /dev/zero | tr '\\0' x", "check standard output exceeds 65536 bytes"},
		{"ended by a signal", "kill -KILL $$", "check process was terminated by a signal"},
	} {
		testhelpers.InstallModule(t, dir, "queue", description, tc.script)
		if o := checksandalerts.ObserveCheck(context.Background(), testhelpers.ModuleCheck(t, dir, "queue", nil)); o.Status != "unknown" || o.Message != tc.want {
			t.Errorf("%s: %+v", tc.name, o)
		}
	}
	// A run past its time limit: the limit passes once the module runs.
	// A run of a check of a node names the time limit of its check group.
	started := filepath.Join(t.TempDir(), "started")
	testhelpers.InstallModule(t, dir, "queue", description, "echo $$ > "+testhelpers.ShellQuote(started)+"; exec sleep 30")
	for timing, want := range map[checksandalerts.Timing]string{
		{}:                         "the module queue did not finish within its time limit, so MMWatchdog stopped the module",
		{TimeLimitPerRun: "300ms"}: "the module queue did not finish within 300ms (time_limit_per_run), so MMWatchdog stopped the module",
	} {
		os.Remove(started)
		limit := testhelpers.NewTimeLimitThatTheTestEnds(context.Background())
		observed := make(chan checksandalerts.Observation, 1)
		go func() {
			observed <- checksandalerts.RunCheck(limit, checksandalerts.NewCheckRun(testhelpers.ModuleCheck(t, dir, "queue", nil), timing))
		}()
		testhelpers.StartedProgram(t, started)
		ended := time.Now()
		limit.End()
		// A run that the time limit did not end would take 30 seconds.
		if o := <-observed; o.Status != "unknown" || o.Message != want || time.Since(ended) > 10*time.Second {
			t.Errorf("a run past its time limit, with the timing %+v: %+v after %s", timing, o, time.Since(ended))
		}
	}
	check := testhelpers.ModuleCheck(t, dir, "queue", nil)
	program := filepath.Join(dir, "queue", checksandalerts.ModuleExecutableFile)
	testhelpers.WriteNotExecutableFile(t, program, "#!/bin/bash\ntrue\n")
	if o := checksandalerts.ObserveCheck(context.Background(), check); o.Status != "unknown" || o.Message != "the module queue is not installed on this node: "+program+" is not an executable file" {
		t.Fatalf("a module whose file is not executable: %+v", o)
	}
	// A check that was read without its module, as a joined node reads a
	// destination of its hub, says that the module is missing.
	if o := checksandalerts.ObserveCheck(context.Background(), checksandalerts.Check{Inputs: &checksandalerts.ModuleCall{Name: "queue", ExecOptions: testhelpers.OwnOptions(t)}}); o.Status != "unknown" || o.Message != "the module queue is not installed on this node" {
		t.Fatalf("a check without its module: %+v", o)
	}
}

// A module only measures: MMWatchdog applies the check's limits to the measurements
// it reports and names each reading and limit in the message. The unit of a
// measurement comes from the module's description. A module's own failed or
// unknown answer stands, and a limit on a measurement the result does not
// have leaves health unknown.
func TestModuleMeasurementsTakeLimits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	description := testhelpers.TestModuleDescription("queue", checksandalerts.UsedAsCheck, nil, map[string]string{"queued_jobs": "count", "workers": "processes", "consumers": "count"})
	measured := `{"protocol":1,"status":"ok","message":"780 queued jobs","measurements":{"queued_jobs":780,"workers":4}}`
	for _, tc := range []struct {
		result          string
		above, below    map[string]float64
		status, message string
	}{
		{measured, nil, nil, "ok", "780 queued jobs"},
		{measured, map[string]float64{"queued_jobs": 1000}, nil, "ok", "queued_jobs is 780 (fails above 1000); the module reported: 780 queued jobs"},
		{measured, map[string]float64{"queued_jobs": 500}, nil, "failed", "queued_jobs is 780 (fails above 500); the module reported: 780 queued jobs"},
		{measured, map[string]float64{"queued_jobs": 1000}, map[string]float64{"workers": 5}, "failed", "queued_jobs is 780 (fails above 1000); workers is 4 (fails below 5); the module reported: 780 queued jobs"},
		{measured, nil, map[string]float64{"consumers": 1}, "unknown", "the result has no measurement named consumers, which this check has a limit on"},
		{`{"protocol":1,"status":"failed","message":"queue is unreachable"}`, map[string]float64{"queued_jobs": 500}, nil, "failed", "queue is unreachable"},
		{`{"protocol":1,"status":"unknown","message":"cannot read the queue"}`, map[string]float64{"queued_jobs": 500}, nil, "unknown", "cannot read the queue"},
	} {
		testhelpers.InstallModule(t, dir, "queue", description, "cat >/dev/null\nprintf '%s' "+testhelpers.ShellQuote(tc.result))
		check := testhelpers.ModuleCheck(t, dir, "queue", nil)
		check.FailWhenMeasurementAbove, check.FailWhenMeasurementBelow = tc.above, tc.below
		o := checksandalerts.ObserveCheck(context.Background(), check)
		if o.Status != tc.status || o.Message != tc.message {
			t.Fatalf("%v %v: %+v", tc.above, tc.below, o)
		}
		if tc.result == measured && !reflect.DeepEqual(o.Measurements, map[string]checksandalerts.Measurement{"queued_jobs": {Value: 780, Unit: "count"}, "workers": {Value: 4, Unit: "processes"}}) {
			t.Fatalf("the measurements do not have the units of the description: %+v", o.Measurements)
		}
	}
}

// An increase in systemd's count is that many restarts at the reading, a
// decrease continues from the new value, and the check fails while more
// than the limit remain within the window.
func TestRestartsCountAutomaticRestartsWithinWindow(t *testing.T) {
	t.Parallel()
	r := &checksandalerts.SystemdUnitCrashLoop{SystemdUnit: "app.service", AutomaticRestartsAbove: 2, AutomaticRestartsWithin: "1h"}
	h := &checksandalerts.RestartHistory{}
	start := time.Now()
	for _, tc := range []struct {
		at      time.Duration
		count   int
		exit    string
		status  string
		message string
	}{
		// The first reading has no earlier one to compare with: counting
		// starts from systemd's current value, and the message says so.
		{0, 5, "", "ok", "first look at systemd unit app.service: MMWatchdog counts the unit's automatic restarts between check runs, from the current run on; systemd's own count of the unit's automatic restarts (NRestarts) is 5"},
		{time.Minute, 5, "exit-code (status 3)", "ok", "0 automatic restarts in the last hour"},
		{2 * time.Minute, 6, "", "ok", "1 automatic restart in the last hour; last exit: exit-code (status 3)"},
		{3 * time.Minute, 8, "", "failed", "3 automatic restarts in the last hour; last exit: exit-code (status 3)"},
		// Started by hand: nothing is counted, and counting continues from 0.
		{4 * time.Minute, 0, "", "failed", "3 automatic restarts in the last hour; last exit: exit-code (status 3)"},
		{5 * time.Minute, 1, "oom-kill (status 9)", "failed", "4 automatic restarts in the last hour; last exit: oom-kill (status 9)"},
		// The restart at 2m leaves the window; 3 still exceed the limit.
		{62 * time.Minute, 1, "", "failed", "3 automatic restarts in the last hour; last exit: oom-kill (status 9)"},
		// The two at 3m leave it: healthy again.
		{63 * time.Minute, 1, "", "ok", "1 automatic restart in the last hour; last exit: oom-kill (status 9)"},
		{65 * time.Minute, 1, "", "ok", "0 automatic restarts in the last hour"},
		// The exit seen at 5m is outside the window, so it is not named.
		{70 * time.Minute, 2, "", "ok", "1 automatic restart in the last hour"},
	} {
		o := h.Observe(start.Add(tc.at), tc.count, tc.exit, r)
		if o.Status != tc.status || o.Message != tc.message || o.Measurements["automatic_restarts"].Unit != "count" {
			t.Fatalf("at %s: %+v", tc.at, o)
		}
	}
	short := &checksandalerts.SystemdUnitCrashLoop{SystemdUnit: "app.service", AutomaticRestartsAbove: 0, AutomaticRestartsWithin: "10m"}
	reset := &checksandalerts.RestartHistory{}
	reset.Observe(start, 10, "", short)
	if o := reset.Observe(start.Add(time.Minute), 3, "", short); o.Status != "failed" || o.Measurements["automatic_restarts"].Value != 3 {
		t.Fatal(o)
	}
	h = &checksandalerts.RestartHistory{}
	h.Observe(start, 0, "", short)
	if o := h.Observe(start.Add(time.Second), 1, "", short); o.Status != "failed" || o.Message != "1 automatic restart in the last 10m" || o.Measurements["automatic_restarts"].Value != 1 {
		t.Fatal(o)
	}
}

// The reading comes from systemctl show; a unit systemd does not know is unknown.
func TestRestartsReadSystemdCountAndLastExit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	output := filepath.Join(dir, "show")
	testhelpers.WriteStandInProgram(t, filepath.Join(dir, "systemctl"), "#!/bin/sh\ncat "+testhelpers.ShellQuote(output)+"\n")
	testhelpers.PutProgramsFirst(t, dir)
	r := &checksandalerts.SystemdUnitCrashLoop{SystemdUnit: "app.service", AutomaticRestartsAbove: 0, AutomaticRestartsWithin: "1h"}
	// Each run starts from the history of restarts that the run before it
	// stored, as a scheduled run does.
	var stored json.RawMessage
	read := func(show string) checksandalerts.Observation {
		t.Helper()
		if err := os.WriteFile(output, []byte(show), 0600); err != nil {
			t.Fatal(err)
		}
		run := checksandalerts.NewCheckRun(checksandalerts.Check{Inputs: r}, checksandalerts.Timing{})
		run.State.Stored = stored
		o := checksandalerts.RunCheck(testhelpers.TestContext(t), run)
		if run.State.Saved != nil {
			stored = run.State.Saved
		}
		return o
	}
	for _, show := range []string{"LoadState=not-found\nNRestarts=0\n", "LoadState=loaded\nResult=success\n"} {
		if o := read(show); o.Status != "unknown" {
			t.Fatal(show, o)
		}
	}
	for _, tc := range []struct{ show, status, message string }{
		{"LoadState=loaded\nNRestarts=4\nResult=success\nExecMainStatus=0\n", "ok", "first look at systemd unit app.service: MMWatchdog counts the unit's automatic restarts between check runs, from the current run on; systemd's own count of the unit's automatic restarts (NRestarts) is 4"},
		{"LoadState=loaded\nNRestarts=4\nResult=oom-kill\nExecMainStatus=9\n", "ok", "0 automatic restarts in the last hour"},
		// systemd clears the exit once the unit runs again; the last one seen is kept.
		{"LoadState=loaded\nNRestarts=5\nResult=success\nExecMainStatus=0\n", "failed", "1 automatic restart in the last hour; last exit: oom-kill (status 9)"},
	} {
		if o := read(tc.show); o.Status != tc.status || o.Message != tc.message {
			t.Fatal(tc.show, o)
		}
	}
}

func TestModuleHelperEnvironmentIsExclusive(t *testing.T) {
	t.Setenv("MMWATCHDOG_MODULE_HELPER", "inherited-helper")
	t.Setenv("MMWATCHDOG_BIN", "inherited-old-variable")
	opts := testhelpers.OwnOptions(t)
	shell := checksandalerts.ObserveCheck(context.Background(), checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "env", SuccessExitCodes: []int{0}, ExecOptions: opts}})
	if shell.Status != "ok" || strings.Contains(shell.Stdout, "MMWATCHDOG_MODULE_HELPER=") || strings.Contains(shell.Stdout, "MMWATCHDOG_BIN=") {
		t.Fatal(shell)
	}
	directory := t.TempDir()
	testhelpers.InstallModule(t, directory, "environment", testhelpers.TestModuleDescription("environment", checksandalerts.UsedAsCheck, nil, nil), "[[ $MMWATCHDOG_MODULE_HELPER = /* && -z ${MMWATCHDOG_BIN+x} ]] || exit 1\nprintf '{\"protocol\":1,\"status\":\"ok\",\"message\":\"helper present\"}'")
	check := testhelpers.ModuleCheck(t, directory, "environment", nil)
	check.Inputs.(*checksandalerts.ModuleCall).ExecOptions = opts
	result := checksandalerts.ObserveCheck(context.Background(), check)
	if result.Status != "ok" || result.Message != "helper present" {
		t.Fatal(result)
	}
}

// A systemd_unit_state check says which state systemd reports. A stopped
// unit is not a failed one, and a unit that runs, active or reloading, is
// healthy.
func TestUnitStateMessages(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ activeState, result, status, message string }{
		{"active", "success", "ok", "systemd unit myapp.service is active"},
		{"reloading", "success", "ok", "systemd unit myapp.service is reloading"},
		{"deactivating", "success", "failed", "systemd unit myapp.service is deactivating"},
		{"inactive", "success", "failed", "systemd unit myapp.service is stopped. systemd reports no failure: the unit was stopped on request, or its program ended normally. MMWatchdog did not start the unit."},
		{"failed", "signal", "failed", "systemd unit myapp.service failed (systemd result: signal)"},
		{"failed", "exit-code", "failed", "systemd unit myapp.service failed (systemd result: exit-code)"},
		{"activating", "success", "failed", "systemd unit myapp.service is activating"},
		{"", "", "unknown", "systemd returned no ActiveState for myapp.service"},
	} {
		if o := checksandalerts.UnitStateObservation("myapp.service", tc.activeState, tc.result); o.Status != tc.status || o.Message != tc.message {
			t.Errorf("%s: %+v", tc.activeState, o)
		}
	}
	// A path or timer unit runs no program of its own.
	for _, unit := range []string{"restart_pgbouncer.path", "backup.timer", "myapp.socket", "myapp.service"} {
		if problem := (&checksandalerts.SystemdUnitState{SystemdUnit: unit, FailWhen: "the_unit_is_not_active"}).Validate(checksandalerts.InputValidation{Authored: func(string) bool { return true }}); problem != nil {
			t.Errorf("%s: %v", unit, problem)
		}
	}
	for _, tc := range []struct{ activeState, result, status, message string }{
		{"active", "success", "ok", "systemd unit restart_pgbouncer.path is active"},
		{"inactive", "success", "failed", "systemd unit restart_pgbouncer.path is stopped. systemd reports no failure: the unit was stopped on request, or was never started. MMWatchdog did not start the unit."},
		{"failed", "trigger-limit-hit", "failed", "systemd unit restart_pgbouncer.path failed (systemd result: trigger-limit-hit)"},
	} {
		if o := checksandalerts.UnitStateObservation("restart_pgbouncer.path", tc.activeState, tc.result); o.Status != tc.status || o.Message != tc.message {
			t.Errorf("%s: %+v", tc.activeState, o)
		}
	}
}

// Every check of a systemd unit says the same about a unit that systemd
// does not know, and is unknown.
func TestEveryCheckOfAnUnknownSystemdUnitSaysNotFound(t *testing.T) {
	t.Parallel()
	testhelpers.FakeUnit(t, "LoadState=not-found\nActiveState=inactive\n")
	for _, inputs := range []checksandalerts.CheckInputs{
		&checksandalerts.SystemdUnitState{SystemdUnit: "myapp.service", FailWhen: "the_unit_is_not_active"},
		&checksandalerts.SystemdUnitResource{SystemdUnit: "myapp.service", Resource: "cpu"},
		&checksandalerts.SystemdUnitResource{SystemdUnit: "myapp.service", Resource: "memory"},
		&checksandalerts.SystemdUnitResource{SystemdUnit: "myapp.service", Resource: "processes"},
		&checksandalerts.SystemdUnitCommands{SystemdUnit: "myapp.service", FailWhenNoCommandContains: "myapp-worker"},
		&checksandalerts.SystemdUnitCrashLoop{SystemdUnit: "myapp.service", AutomaticRestartsAbove: 2, AutomaticRestartsWithin: "1h"},
	} {
		if o := checksandalerts.ObserveCheck(testhelpers.TestContext(t), checksandalerts.Check{Inputs: inputs}); o.Status != "unknown" || o.Message != "systemd unit myapp.service not found" {
			t.Errorf("%T %+v: %s %q", inputs, inputs, o.Status, o.Message)
		}
	}
}

func TestHTTPURLsRequireAHost(t *testing.T) {
	t.Parallel()
	for name, module := range map[string]interface {
		Validate(checksandalerts.InputValidation) *checksandalerts.FieldError
	}{
		"http":    &checksandalerts.HTTP{URL: "http://:8080/health"},
		"webhook": &checksandalerts.Webhook{URL: "http://:8080/health"},
	} {
		t.Run(name, func(t *testing.T) {
			problem := module.Validate(checksandalerts.InputValidation{Authored: func(string) bool { return false }})
			if problem == nil || problem.Path != "url" || !strings.Contains(problem.Message, "host") || !strings.Contains(problem.Message, "https://example.com/") {
				t.Fatalf("expected host refusal with an example, got %+v", problem)
			}
		})
	}
}

// A request that gets no answer within the time limit of the run has
// failed, and the message says so in plain words with the limit, in place
// of Go's "context deadline exceeded".
func TestHTTPAndTCPChecksSayPlainlyThatNoAnswerCameInTime(t *testing.T) {
	t.Parallel()
	timing := checksandalerts.Timing{TimeLimitPerRun: "5s"}
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer node.Close()
	ended := func() context.Context {
		limit := testhelpers.NewTimeLimitThatTheTestEnds(context.Background())
		limit.End()
		return limit
	}
	// A token in the query stays out of the message, which goes to every
	// alert destination.
	o := checksandalerts.RunCheck(ended(), checksandalerts.NewCheckRun(checksandalerts.Check{Inputs: &checksandalerts.HTTP{URL: node.URL + "/health?token=s3cret", Method: "GET", ExpectedHTTPStatusCodes: []int{200}}}, timing))
	if want := "Get " + strconv.Quote(node.URL+"/health?...") + ": no answer within 5s (time_limit_per_run)"; o.Status != "failed" || o.Message != want {
		t.Errorf("http: status %q, message %q; want failed, %q", o.Status, o.Message, want)
	}
	address := strings.TrimPrefix(node.URL, "http://")
	o = checksandalerts.RunCheck(ended(), checksandalerts.NewCheckRun(checksandalerts.Check{Inputs: &checksandalerts.TCP{Address: address}}, timing))
	if want := "the connection to " + address + " did not open within 5s (time_limit_per_run)"; o.Status != "failed" || o.Message != want {
		t.Errorf("tcp: status %q, message %q; want failed, %q", o.Status, o.Message, want)
	}
}

// Checks that only open a connection say plainly that it did not open in time.
func TestSSHCheckSaysPlainlyThatTheConnectionDidNotOpenInTime(t *testing.T) {
	t.Parallel()
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer node.Close()
	address := strings.TrimPrefix(node.URL, "http://")
	limit := testhelpers.NewTimeLimitThatTheTestEnds(context.Background())
	limit.End()
	ssh := checksandalerts.SSH(address)
	o := checksandalerts.ObserveCheck(limit, checksandalerts.Check{Inputs: &ssh})
	if want := "the connection to " + address + " did not open within the time limit of the run"; o.Status != "failed" || o.Message != want {
		t.Errorf("status %q, message %q; want failed, %q", o.Status, o.Message, want)
	}
}
