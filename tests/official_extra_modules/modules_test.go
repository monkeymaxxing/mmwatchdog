// Package official_extra_modules tests the official extra modules that
// deliver alerts. Each test runs a module's script through the same
// Starlark entry point as "mmwatchdog run-starlark-module", compiled into the test
// binary, against a stand-in for the module's service. The tests
// are not beside the modules, because a deploy copies the whole directory
// official_extra_modules to every node.
package official_extra_modules

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	moduleseeds "mmwatchdog"
	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
)

var (
	prepareModules       sync.Once
	moduleSeedsDirectory string
	moduleSeedsError     error
)

// program returns this compiled test binary. Its TestMain runs the same
// Starlark entry point as mmwatchdog, so guests need neither Go nor a source tree.
func program(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "run-starlark-module" {
		if err := checksandalerts.RunStarlarkModule(context.Background(), os.Args[2], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	launcher, e := makeStandInLauncher()
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	code := m.Run()
	if e = standInLauncherUnchanged(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		code = 1
	}
	os.RemoveAll(launcher)
	if moduleSeedsDirectory != "" {
		os.RemoveAll(moduleSeedsDirectory)
	}
	os.Exit(code)
}

// officialExtraModules materializes the embedded source files once. Tests
// use these immutable seeds without opening files outside their package.
func officialExtraModules(t *testing.T) string {
	t.Helper()
	prepareModules.Do(func() {
		moduleSeedsDirectory, moduleSeedsError = os.MkdirTemp("", "mmwatchdog-official-module-test-seeds-")
		if moduleSeedsError != nil {
			return
		}
		moduleSeedsError = fs.WalkDir(moduleseeds.ModuleTestSeeds, ".", func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return os.MkdirAll(filepath.Join(moduleSeedsDirectory, path), 0755)
			}
			content, err := fs.ReadFile(moduleseeds.ModuleTestSeeds, path)
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(moduleSeedsDirectory, path), content, 0644)
		})
	})
	if moduleSeedsError != nil {
		t.Fatal(moduleSeedsError)
	}
	return moduleSeedsDirectory
}

// received is one request that a stand-in service got.
type received struct {
	method, path, contentType string
	body                      []byte
}

// service stands for Slack or for Telegram's Bot API server. answer decides
// what it answers to each request.
type service struct {
	*httptest.Server
	mu       sync.Mutex
	requests []received
}

func startService(t *testing.T, answer func(received) (status int, body string)) *service {
	t.Helper()
	s := &service{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		request := received{r.Method, r.URL.Path, r.Header.Get("Content-Type"), body}
		s.mu.Lock()
		s.requests = append(s.requests, request)
		s.mu.Unlock()
		status, text := answer(request)
		// A redirect names another address of the same service, which a
		// module must not follow.
		if status/100 == 3 {
			w.Header().Set("Location", "/moved"+r.URL.Path)
		}
		w.WriteHeader(status)
		io.WriteString(w, text)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *service) got() []received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]received{}, s.requests...)
}

// outcome is what one run of a module left: its result, and what it wrote.
type outcome struct {
	status, message string
	stdout, stderr  string
	exitStatus      int
}

// run runs a module as MMWatchdog does: "mmwatchdog run-starlark-module" with the module's
// directory, the request on standard input, and an environment with PATH,
// HOME, USER, and LANG, and the variables of environment beside them. The
// PATH holds no program: a module sends with the primitive http_request and
// needs nothing on the node but MMWatchdog.
func run(t *testing.T, module string, alert checksandalerts.Event, inputs map[string]any, fileContents map[string]string, environment ...string) outcome {
	t.Helper()
	return runWithRequest(t, module, map[string]any{"protocol": 1, "alert": alert, "inputs": inputs, "file_contents": fileContents}, environment...)
}

// timeLimitOfARunThatGetsNoAnswer is the time that the request of a test
// leaves the module when the module asks a server that never answers
// (silentServer). MMWatchdog would end the module then. The test does not end the
// module, so the module ends its call by itself shortly before that time,
// however long the start of the module takes on a busy machine.
const timeLimitOfARunThatGetsNoAnswer = 2 * time.Second

// silentServer returns the address, as host:port, of a TCP server that
// accepts each connection, reads what comes, and never answers, as a server
// that hangs does.
func silentServer(t *testing.T) string {
	t.Helper()
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, e := listener.Accept()
			if e != nil {
				return
			}
			go func() { defer conn.Close(); io.Copy(io.Discard, conn) }()
		}
	}()
	return listener.Addr().String()
}

// runWithRequest runs a module as run does, with the request as given.
func runWithRequest(t *testing.T, module string, fields map[string]any, environment ...string) outcome {
	t.Helper()
	dir := t.TempDir()
	request, e := json.Marshal(fields)
	if e != nil {
		t.Fatal(e)
	}
	command := exec.Command(program(t), "run-starlark-module", filepath.Join(officialExtraModules(t), module))
	command.Stdin = bytes.NewReader(request)
	command.Dir = dir
	command.Env = append([]string{"PATH=" + dir, "HOME=" + dir, "USER=mmwatchdog", "LANG=C.UTF-8"}, environment...)
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
	o := outcome{stdout: stdout.String(), stderr: stderr.String(), exitStatus: command.ProcessState.ExitCode()}
	var result struct {
		Protocol                    int    `json:"protocol"`
		Status                      string `json:"status"`
		Message                     string `json:"message"`
		CanVerifyWithoutMessage     bool   `json:"can_verify_without_message"`
		AcceptsAddressAndCredential bool   `json:"accepts_address_and_credential"`
		VerificationMessage         string `json:"verification_message"`
	}
	if o.exitStatus == 0 {
		if e := checksandalerts.DecodeJSON(stdout.Bytes(), &result); e != nil || result.Protocol != 1 {
			t.Fatalf("the module did not answer with its result: %v: %q %q", e, o.stdout, o.stderr)
		}
	}
	o.status, o.message = result.Status, result.Message
	return o
}

// holds reports a secret in anything that a run left where others can read
// it: the module's result and what it wrote to its standard error. MMWatchdog puts
// both into mmwatchdog status and into a report to the hub.
func (o outcome) holds(secret string) bool {
	return strings.Contains(o.stdout+o.stderr, secret)
}

var failure = checksandalerts.Event{ID: "0123456789abcdef0123456789abcdef", Incident: "fedcba9876543210fedcba9876543210", Node: "app-1", CheckGroup: "myapp", Check: "http", Type: "failed", AlertSeverity: "sos",
	At: time.Date(2026, 9, 29, 14, 3, 5, 0, time.UTC), Title: "app-1: myapp/http failed", TitleWithAlertSeverity: "SOS: app-1: myapp/http failed", Body: "expected HTTP 200, received 503\nfailing since 14:02 UTC", Message: "expected HTTP 200, received 503"}

// awkward is text that a careless module would send wrongly: quotes,
// backslashes, the three characters that Slack reads as control characters,
// characters that Slack and Telegram read as formatting, characters outside
// ASCII, and an empty line.
const awkward = "it said \"no\" & 'never'; C:\\temp\\x <tag> a > b *bold* _under_ `code` ~strike~\n\nȘtiință 🐒 naïve – 100 % done"

func slackText(t *testing.T, request received) (text string, payload map[string]any) {
	t.Helper()
	if request.method != "POST" || request.contentType != "application/json" {
		t.Fatalf("not a JSON POST: %+v", request)
	}
	if e := json.Unmarshal(request.body, &payload); e != nil {
		t.Fatal(e, string(request.body))
	}
	text, _ = payload["text"].(string)
	return text, payload
}

const slackSecret = "T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX"

// codeBlock is the line of three backticks that starts and ends a code block
// in Slack.
const codeBlock = "`" + "`" + "`"

// zeroWidthSpace is the invisible character that the modules discord and
// mattermost put after an @ in the text of an alert, so that the @ mentions
// nobody.
const zeroWidthSpace = "\u200b"

// The slack module posts one text to the address in its file, for every
// kind of alert, and tells MMWatchdog that Slack took it: the title with the alert's
// severity in front as MMWatchdog wrote it, after the mention if there is one, and
// below it the body in a code block, which Slack shows as it is.
func TestSlackSendsTheTitleAndTheBody(t *testing.T) {
	t.Parallel()
	slack := startService(t, func(received) (int, string) { return 200, "ok" })
	webhook := slack.URL + "/services/" + slackSecret
	recovery := failure
	recovery.Type, recovery.Title, recovery.TitleWithAlertSeverity, recovery.Body = "recovered", "app-1: myapp/http is ok again", "Resolved SOS: app-1: myapp/http is ok again", "HTTP 200"
	reminder := failure
	reminder.Type, reminder.Title, reminder.TitleWithAlertSeverity, reminder.Body = "reminder", "app-1: myapp/http still failing", "SOS: app-1: myapp/http still failing", "myapp/http still failing since 14:02 UTC; latest result: expected HTTP 200, received 503"
	onlyTheTitle := failure
	onlyTheTitle.Body, onlyTheTitle.Message = "", ""
	odd := failure
	odd.Title, odd.TitleWithAlertSeverity, odd.Body = "app-1: "+awkward[:22], "SOS: app-1: "+awkward[:22], awkward
	// Three backticks would end the code block, so they arrive as three
	// single quotes; fewer stay.
	inTitle := failure
	inTitle.Type, inTitle.AlertSeverity, inTitle.Body = "notice", "info", "HTTP 200"
	inTitle.TitleWithAlertSeverity = "Info: app-1: restarted *all* _workers_ ~now~ `x` <b> & " + codeBlock
	fenced := failure
	fenced.Body = "the output was:\n" + codeBlock + "\nstarted `myapp` with ``two``\n" + codeBlock + codeBlock + "`"
	for _, tc := range []struct {
		name   string
		alert  checksandalerts.Event
		inputs map[string]any
		want   string
	}{
		{"a failure", failure, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/slack"}, "SOS: app-1: myapp/http failed\n" + codeBlock + "\nexpected HTTP 200, received 503\nfailing since 14:02 UTC\n" + codeBlock},
		{"a failure with a mention", failure, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/slack", "mention": "<@U024BE7LH>"}, "<@U024BE7LH> SOS: app-1: myapp/http failed\n" + codeBlock + "\nexpected HTTP 200, received 503\nfailing since 14:02 UTC\n" + codeBlock},
		{"a recovery", recovery, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/slack"}, "Resolved SOS: app-1: myapp/http is ok again\n" + codeBlock + "\nHTTP 200\n" + codeBlock},
		{"a reminder", reminder, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/slack"}, "SOS: app-1: myapp/http still failing\n" + codeBlock + "\nmyapp/http still failing since 14:02 UTC; latest result: expected HTTP 200, received 503\n" + codeBlock},
		// Without a body there is no code block.
		{"only the title", onlyTheTitle, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/slack", "mention": "<!channel>"}, "<!channel> SOS: app-1: myapp/http failed"},
		// Slack gets &, <, and > of the alert's text as &amp;, &lt;, and
		// &gt;, and everything else as it is.
		{"awkward text", odd, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/slack"},
			"SOS: app-1: it said \"no\" &amp; 'never'\n" + codeBlock + "\nit said \"no\" &amp; 'never'; C:\\temp\\x &lt;tag&gt; a &gt; b *bold* _under_ `code` ~strike~\n\nȘtiință 🐒 naïve – 100 % done\n" + codeBlock},
		// The first line is ordinary text for Slack: its formatting characters
		// stay, its &, <, and > are written as Slack asks, and three
		// backticks, which would start a code block there, arrive as three
		// single quotes.
		{"formatting characters in the title", inTitle, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/slack", "mention": "<!here>"},
			"<!here> Info: app-1: restarted *all* _workers_ ~now~ `x` &lt;b&gt; &amp; '''\n" + codeBlock + "\nHTTP 200\n" + codeBlock},
		{"three backticks in the body", fenced, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/slack"},
			"SOS: app-1: myapp/http failed\n" + codeBlock + "\nthe output was:\n'''\nstarted `myapp` with ``two``\n''''''`\n" + codeBlock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(slack.got())
			o := run(t, "slack", tc.alert, tc.inputs, map[string]string{"webhook_url_file": webhook})
			if o.exitStatus != 0 || o.status != "delivered" || o.message != "Slack accepted the message" || o.stderr != "" {
				t.Fatalf("%+v", o)
			}
			requests := slack.got()[before:]
			if len(requests) != 1 || requests[0].path != "/services/"+slackSecret {
				t.Fatalf("%+v", requests)
			}
			text, payload := slackText(t, requests[0])
			// The message states nothing but its text, so Slack's formatting
			// is on, and a mention notifies.
			if text != tc.want || len(payload) != 1 {
				t.Fatalf("got  %q\nwant %q\npayload %v", text, tc.want, payload)
			}
			if o.holds(slackSecret) {
				t.Fatalf("the address of the webhook is in what the module wrote: %+v", o)
			}
		})
	}
}

// A message of more than 4,000 characters, counted as Slack gets them with
// & written as &amp;, is cut to at most 4,000: the body is cut between two
// characters and never inside &amp;, the cut is marked, and the first line
// and the end of the code block always arrive.
func TestSlackCutsALongText(t *testing.T) {
	t.Parallel()
	slack := startService(t, func(received) (int, string) { return 200, "ok" })
	long := failure
	long.Body = strings.Repeat("ü🐒& ", 2000)
	o := run(t, "slack", long, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/slack", "mention": "<!here>"}, map[string]string{"webhook_url_file": slack.URL + "/services/" + slackSecret})
	if o.status != "delivered" {
		t.Fatalf("%+v", o)
	}
	text, _ := slackText(t, slack.got()[0])
	shown := strings.NewReplacer("&amp;", "&").Replace(text)
	start, end := "<!here> SOS: app-1: myapp/http failed\n"+codeBlock+"\n", "…\n"+codeBlock
	if length := utf8.RuneCountInString(text); !utf8.ValidString(text) || length > 4000 || length < 4000-len("&amp;") || !strings.HasPrefix(shown, start) || !strings.HasSuffix(shown, end) ||
		strings.Count(text, "&") != strings.Count(text, "&amp;") || !strings.HasPrefix(long.Body, strings.TrimSuffix(strings.TrimPrefix(shown, start), end)) {
		t.Fatalf("%d characters, ends with %q", length, text[len(text)-24:])
	}
	// A body of nothing but &: the cut never leaves a part of &amp;.
	ampersands := failure
	ampersands.Body = strings.Repeat("&", 3000)
	if o = run(t, "slack", ampersands, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/slack"}, map[string]string{"webhook_url_file": slack.URL + "/services/" + slackSecret}); o.status != "delivered" {
		t.Fatalf("%+v", o)
	}
	text, _ = slackText(t, slack.got()[1])
	sent := strings.TrimSuffix(strings.TrimPrefix(text, "SOS: app-1: myapp/http failed\n"+codeBlock+"\n"), "…\n"+codeBlock)
	if length := utf8.RuneCountInString(text); length > 4000 || length < 4000-len("&amp;") || strings.ReplaceAll(sent, "&amp;", "") != "" {
		t.Fatalf("%d characters, ends with %q", length, text[len(text)-24:])
	}
	// A body that fits exactly is not cut.
	exact := failure
	exact.Body = strings.Repeat("x", 4000-len("SOS: app-1: myapp/http failed")-9)
	if o = run(t, "slack", exact, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/slack"}, map[string]string{"webhook_url_file": slack.URL + "/services/" + slackSecret}); o.status != "delivered" {
		t.Fatalf("%+v", o)
	}
	if text, _ = slackText(t, slack.got()[2]); utf8.RuneCountInString(text) != 4000 || text != "SOS: app-1: myapp/http failed\n"+codeBlock+"\n"+exact.Body+"\n"+codeBlock {
		t.Fatalf("%d characters, ends with %q", utf8.RuneCountInString(text), text[len(text)-16:])
	}
}

// Every answer of Slack other than an accepted message is a failed attempt
// with a message that a person can act on, and no failure shows the address
// of the webhook.
func TestSlackFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"a login page instead of acknowledgment", 200, "<html>Sign in</html>", "Slack answered 200: <html>Sign in</html>."},
		{"an empty success", 204, "", "Slack answered 204: no text."},
		{"another success body", 200, "OK", "Slack answered 200: OK."},
		{"a webhook that was removed", 404, "no_service", "Slack answered 404: no_service. The address of the webhook does not work any more. Make a new incoming webhook in Slack and write its address into the file that webhook_url_file names."},
		{"a revoked webhook", 403, "invalid_token", "Slack answered 403: invalid_token. The address of the webhook does not work any more. Make a new incoming webhook in Slack and write its address into the file that webhook_url_file names."},
		{"an archived channel", 410, "channel_is_archived", "Slack answered 410: channel_is_archived. The channel of the webhook does not exist any more or is archived. Make a new incoming webhook for another channel."},
		{"a restricted channel", 403, "action_prohibited", "Slack answered 403: action_prohibited. An administrator of the Slack workspace does not let this webhook post to its channel."},
		{"a rate limit", 429, "", "Slack answered 429: no text. Slack takes no more messages from this webhook for a short time."},
		{"an error of Slack", 500, "rollup_error", "Slack answered 500: rollup_error."},
		{"a page of another server", 502, "<html>\n<body>Bad Gateway</body>\n</html>", "Slack answered 502: <html>."},
		{"a redirect, which the module does not follow", 302, "", "Slack answered 302: no text."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slack := startService(t, func(received) (int, string) { return tc.status, tc.body })
			o := run(t, "slack", failure, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/slack"}, map[string]string{"webhook_url_file": slack.URL + "/services/" + slackSecret})
			if o.exitStatus != 0 || o.status != "failed" || o.message != tc.want {
				t.Fatalf("got  %q\nwant %q\n%+v", o.message, tc.want, o)
			}
			if len(slack.got()) != 1 || o.holds(slackSecret) {
				t.Fatalf("%d requests; %+v", len(slack.got()), o)
			}
		})
	}
	t.Run("nothing listens at the address", func(t *testing.T) {
		listener, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		address := "http://" + listener.Addr().String()
		listener.Close()
		o := run(t, "slack", failure, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/slack"}, map[string]string{"webhook_url_file": address + "/services/" + slackSecret})
		if o.exitStatus != 0 || o.status != "failed" || !strings.HasPrefix(o.message, "MMWatchdog could not send the message to Slack: ") || !strings.Contains(o.message, "connection refused") || o.holds(slackSecret) || o.holds("/services/") {
			t.Fatalf("%+v", o)
		}
	})
	// A server that hangs fails the attempt before the time limit of the
	// delivery attempt, with the module's own message, and MMWatchdog tries again
	// later.
	t.Run("a server that accepts the connection and never answers", func(t *testing.T) {
		address := "http://" + silentServer(t) + "/services/" + slackSecret
		o := runWithRequest(t, "slack", map[string]any{"protocol": 1, "alert": failure, "inputs": map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/slack"}, "file_contents": map[string]string{"webhook_url_file": address},
			"mmwatchdog_ends_the_module_after_milliseconds": timeLimitOfARunThatGetsNoAnswer.Milliseconds()})
		want := "MMWatchdog could not send the message to Slack: MMWatchdog ended the call shortly before the time limit of the delivery attempt (time_limit_per_delivery_attempt)"
		if o.exitStatus != 0 || o.status != "failed" || o.message != want || o.holds(slackSecret) {
			t.Fatalf("got  %q\nwant %q\n%+v", o.message, want, o)
		}
	})
	// A file that holds no web address, or more than one, fails the attempt
	// before the module sends anything.
	t.Run("a file that holds no web address", func(t *testing.T) {
		slack := startService(t, func(received) (int, string) { return 200, "ok" })
		for _, content := range []string{
			strings.TrimPrefix(slack.URL, "http://") + "/services/" + slackSecret,
			slack.URL + "/services/" + slackSecret + "\nmore",
			slack.URL + "/services/" + slackSecret + " " + slack.URL,
			slack.URL + "/\" --output /tmp/x \"",
			"ftp://hooks.slack.example/services/" + slackSecret,
			"https://",
			"",
		} {
			o := run(t, "slack", failure, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/slack"}, map[string]string{"webhook_url_file": content})
			if o.exitStatus != 0 || o.status != "failed" || o.message != "the file that webhook_url_file names must hold one web address that starts with https:// or http://, and nothing else" || o.holds(slackSecret) {
				t.Fatalf("%q: %+v", content, o)
			}
		}
		if requests := slack.got(); len(requests) != 0 {
			t.Fatalf("the module sent a request to an address that is none: %+v", requests)
		}
	})
	// The shortest web address is a scheme and one character. The request
	// goes to a proxy of the test, so no name is looked up.
	t.Run("the shortest web address", func(t *testing.T) {
		proxy := startService(t, func(received) (int, string) { return 200, "ok" })
		o := run(t, "slack", failure, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/slack"}, map[string]string{"webhook_url_file": "http://a"}, "HTTP_PROXY="+proxy.URL)
		if o.exitStatus != 0 || o.status != "delivered" || len(proxy.got()) != 1 {
			t.Fatalf("%+v, %d requests", o, len(proxy.got()))
		}
	})
	// Slack and services using its format must acknowledge a delivery with ok.
	t.Run("another service that takes Slack's format", func(t *testing.T) {
		other := startService(t, func(received) (int, string) { return 201, " \n ok\t " })
		o := run(t, "slack", failure, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/slack"}, map[string]string{"webhook_url_file": other.URL + "/hooks/" + slackSecret})
		if o.exitStatus != 0 || o.status != "delivered" || len(other.got()) != 1 {
			t.Fatalf("%+v", o)
		}
	})
}

// MMWatchdog checks what a destination states for slack and for telegram against
// the module's description, before the module runs: a required input that
// is missing, an input that the module does not have, and a value of
// another type are validation errors. The modules count on that and check
// only what a description cannot say.
func TestTheInputsOfSlackAndTelegramAreValidated(t *testing.T) {
	t.Parallel()
	modules := checksandalerts.ModuleDirs{OfficialExtra: officialExtraModules(t), External: filepath.Join(t.TempDir(), "external_modules")}
	for _, name := range []string{"slack", "telegram"} {
		if module, e := config.DescribeModule(modules, name); e != nil || module.Kind != "official_extra" || module.ShippedWithMMWatchdogVersion != checksandalerts.Version || module.Description.UsedAs != "alert_destination" || len(module.Description.Examples) == 0 {
			t.Fatalf("%s: %+v %v", name, module, e)
		}
	}
	directory := t.TempDir()
	if e := os.Mkdir(filepath.Join(directory, "check_groups"), 0700); e != nil {
		t.Fatal(e)
	}
	for file, content := range map[string]string{
		"mmwatchdog_node_settings.yml":              "keep_undelivered_alerts_for: 24h\nundelivered_alerts_content_size_limit: 8MiB\nalert_severity_when_this_node_goes_silent: sos\nalert_severity_when_this_node_has_rebooted: info\nkeep_measurement_history_for: 7d\nmeasurement_history_size_limit: 256MiB\n",
		filepath.Join("check_groups", "checks.yml"): "check_groups: {app: {check_every: 30s, time_limit_per_run: 5s, alert_after_failing_for: 1m, recovered_after_healthy_for: 30s, systemd_unit_state: {systemd_unit: myapp.service, fail_when: the_unit_is_not_active}, alert_severity: warn}}\n",
	} {
		if e := os.WriteFile(filepath.Join(directory, file), []byte(content), 0600); e != nil {
			t.Fatal(e)
		}
	}
	for _, tc := range []struct{ destination, want string }{
		{"slack: {webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team, mention: '<@U024BE7LH>'}", ""},
		{"slack: {webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team, run_as_user_account: alerts, env: {HTTPS_PROXY: 'http://proxy.example.com:3128'}}", ""},
		{"telegram: {bot_token_file: /etc/mmwatchdog/secrets/telegram-bot, chat_id: '-1001234567890'}", ""},
		{"telegram: {bot_token_file: /etc/mmwatchdog/secrets/telegram-bot, chat_id: '@mychannel', bot_api_server_url: 'https://bot-api.example.com'}", ""},
		{"telegram: {bot_token_file: /etc/mmwatchdog/secrets/telegram-bot, chat_id: '1', bot_api_server_url: 'api.telegram.org'}", "destinations.chat.telegram.bot_api_server_url: "},
		{"telegram: {bot_token_file: /etc/mmwatchdog/secrets/telegram-bot, chat_id: '1', bot_api_server_url: 'https://api.telegram.org https://other.example.com'}", "destinations.chat.telegram.bot_api_server_url: "},
		{"slack: {mention: '<!channel>'}", "destinations.chat.slack.webhook_url_file: is required: The absolute path of a file that holds the address of the Slack incoming webhook, and nothing else."},
		{"slack: {}", "destinations.chat.slack.webhook_url_file: is required: "},
		{"slack: {webhook_url_file: /etc/mmwatchdog/secrets/slack, channel: '#ops'}", `destinations.chat.slack.channel: unknown field "channel": the inputs of the module slack are webhook_url_file, mention`},
		{"slack: {webhook_url_file: secrets/slack}", "destinations.chat.slack.webhook_url_file: an input whose name ends in _file names a file that MMWatchdog reads and gives to the module. Set webhook_url_file to an absolute file path"},
		{"slack: {webhook_url_file: /etc/mmwatchdog/secrets/slack, mention: 7}", "destinations.chat.slack.mention: expected a string"},
		{"telegram: {chat_id: '-1001234567890'}", "destinations.chat.telegram.bot_token_file: is required: The absolute path of a file that holds the bot's token"},
		{"telegram: {bot_token_file: /etc/mmwatchdog/secrets/telegram-bot}", "destinations.chat.telegram.chat_id: is required: The chat that gets the alerts"},
		// The number of a chat is written in quotes: a chat is a number or
		// the name of a channel.
		{"telegram: {bot_token_file: /etc/mmwatchdog/secrets/telegram-bot, chat_id: -1001234567890}", "destinations.chat.telegram.chat_id: expected a string"},
		{"telegram: {bot_token_file: /etc/mmwatchdog/secrets/telegram-bot, chat_id: '1', parse_mode: HTML}", `destinations.chat.telegram.parse_mode: unknown field "parse_mode": the inputs of the module telegram are bot_token_file, chat_id, bot_api_server_url`},
		{"unavailable_alarm: {api_key_file: /etc/mmwatchdog/secrets/unavailable_alarm}", `unknown field "unavailable_alarm": the field name matches neither a setting of a destination nor an installed module.`},
	} {
		if e := os.WriteFile(filepath.Join(directory, "alert_destinations.yml"), []byte("destinations:\n  chat:\n    "+tc.destination+"\n    remind_every: never\n    alert_severities: [sos]\n"), 0600); e != nil {
			t.Fatal(e)
		}
		_, e := config.LoadWithModuleDirs(directory, modules)
		if tc.want == "" && e != nil || tc.want != "" && (e == nil || !strings.Contains(e.Error(), tc.want)) {
			t.Errorf("%s:\n got %v\nwant %q", tc.destination, e, tc.want)
		}
	}
}

const telegramToken = "123456789:AAE-x1y2z3_SECRETsecretSECRET"

// telegramMessage reads a request to the method sendMessage.
func telegramMessage(t *testing.T, request received) map[string]any {
	t.Helper()
	if request.method != "POST" || request.contentType != "application/json" || request.path != "/bot"+telegramToken+"/sendMessage" {
		t.Fatalf("not a JSON POST to sendMessage of the bot: %+v", request)
	}
	var message map[string]any
	decoder := json.NewDecoder(bytes.NewReader(request.body))
	decoder.UseNumber()
	if e := decoder.Decode(&message); e != nil {
		t.Fatal(e, string(request.body))
	}
	return message
}

// telegram answers as the Bot API does: it takes a text of at most 4,096
// UTF-16 code units and refuses a longer one.
func telegram(t *testing.T) *service {
	return startService(t, func(request received) (int, string) {
		var message struct {
			Text string `json:"text"`
		}
		json.Unmarshal(request.body, &message)
		if len(utf16.Encode([]rune(message.Text))) > 4096 {
			return 400, `{"ok":false,"error_code":400,"description":"Bad Request: message is too long"}`
		}
		return 200, `{"ok":true,"result":{"message_id":7,"date":1790000000,"chat":{"id":-1001234567890,"type":"supergroup"},"text":"x"}}`
	})
}

// The telegram module sends the title with the alert's severity in front,
// as MMWatchdog wrote it, and the body as plain text to the chat, through
// sendMessage of the bot whose token is in its file.
func TestTelegramSendsTheTitleAndTheBody(t *testing.T) {
	t.Parallel()
	server := telegram(t)
	recovery := failure
	recovery.Type, recovery.Title, recovery.TitleWithAlertSeverity, recovery.Body = "recovered", "app-1: myapp/http is ok again", "Resolved SOS: app-1: myapp/http is ok again", "HTTP 200"
	onlyTheTitle := failure
	onlyTheTitle.Body, onlyTheTitle.Message = "", ""
	odd := failure
	odd.Body = awkward
	for _, tc := range []struct {
		name       string
		alert      checksandalerts.Event
		chat, want string
	}{
		{"a failure", failure, `"-1001234567890"`, "SOS: app-1: myapp/http failed\nexpected HTTP 200, received 503\nfailing since 14:02 UTC"},
		{"a channel by its name", failure, `"@mychannel"`, "SOS: app-1: myapp/http failed\nexpected HTTP 200, received 503\nfailing since 14:02 UTC"},
		{"a recovery", recovery, `"-1001234567890"`, "Resolved SOS: app-1: myapp/http is ok again\nHTTP 200"},
		{"only the title", onlyTheTitle, `"-1001234567890"`, "SOS: app-1: myapp/http failed"},
		{"awkward text", odd, `"-1001234567890"`, "SOS: app-1: myapp/http failed\n" + awkward},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(server.got())
			inputs := map[string]any{"bot_token_file": "/etc/mmwatchdog/secrets/telegram-bot", "chat_id": json.RawMessage(tc.chat), "bot_api_server_url": server.URL + "/"}
			o := run(t, "telegram", tc.alert, inputs, map[string]string{"bot_token_file": telegramToken})
			if o.exitStatus != 0 || o.status != "delivered" || o.message != "Telegram accepted the message" || o.stderr != "" {
				t.Fatalf("%+v", o)
			}
			requests := server.got()[before:]
			if len(requests) != 1 {
				t.Fatalf("%+v", requests)
			}
			message := telegramMessage(t, requests[0])
			chat, _ := json.Marshal(message["chat_id"])
			preview, _ := json.Marshal(message["link_preview_options"])
			if message["text"] != tc.want || string(chat) != tc.chat || string(preview) != `{"is_disabled":true}` || len(message) != 3 {
				t.Fatalf("got  %q\nwant %q\nmessage %v", message["text"], tc.want, message)
			}
			if o.holds(telegramToken) {
				t.Fatalf("the bot token is in what the module wrote: %+v", o)
			}
		})
	}
}

// Without bot_api_server_url the module sends to Telegram's own server,
// https://api.telegram.org. The test gives the module a proxy that stands on
// this machine and refuses every connection, so the request reaches nothing
// but the proxy, which sees where the module wanted to connect.
func TestTelegramUsesTelegramsServerUnlessToldOtherwise(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	asked := []string{}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Method+" "+r.Host)
		mu.Unlock()
		http.Error(w, "this proxy connects to nothing", http.StatusForbidden)
	}))
	defer proxy.Close()
	o := run(t, "telegram", failure, map[string]any{"bot_token_file": "/etc/mmwatchdog/secrets/telegram-bot", "chat_id": "-1001234567890"}, map[string]string{"bot_token_file": telegramToken}, "HTTPS_PROXY="+proxy.URL)
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || asked[0] != "CONNECT api.telegram.org:443" {
		t.Fatalf("the module asked the proxy for %q: %+v", asked, o)
	}
	if o.exitStatus != 0 || o.status != "failed" || !strings.HasPrefix(o.message, "MMWatchdog could not send the message to Telegram: ") || o.holds(telegramToken) {
		t.Fatalf("%+v", o)
	}
}

// A text of more than 4,096 characters is cut to 4,096 between two
// characters and marked. Telegram counts most emoji as two characters; when
// it still refuses the text as too long, the module sends 2,048 characters.
func TestTelegramCutsALongText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body     string
		requests, want int
	}{
		{"characters that Telegram counts once", strings.Repeat("ü& ", 3000), 1, 4096},
		{"characters that Telegram counts twice", strings.Repeat("🐒", 6000), 2, 2048},
		{"a text of exactly the limit", strings.Repeat("x", 4096-len(failure.TitleWithAlertSeverity)-1), 1, 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := telegram(t)
			long := failure
			long.Body = tc.body
			inputs := map[string]any{"bot_token_file": "/etc/mmwatchdog/secrets/telegram-bot", "chat_id": "-1001234567890", "bot_api_server_url": server.URL}
			o := run(t, "telegram", long, inputs, map[string]string{"bot_token_file": telegramToken})
			requests := server.got()
			if o.status != "delivered" || len(requests) != tc.requests {
				t.Fatalf("%d requests; %+v", len(requests), o)
			}
			text, _ := telegramMessage(t, requests[len(requests)-1])["text"].(string)
			whole := long.TitleWithAlertSeverity + "\n" + long.Body
			cut := utf8.RuneCountInString(whole) > tc.want
			if !utf8.ValidString(text) || utf8.RuneCountInString(text) != tc.want || cut != strings.HasSuffix(text, "…") || !strings.HasPrefix(whole, strings.TrimSuffix(text, "…")) {
				t.Fatalf("%d characters, cut %v, ends with %q", utf8.RuneCountInString(text), cut, text[len(text)-12:])
			}
		})
	}
}

// Every answer of Telegram other than "ok": true is a failed attempt with a
// message that a person can act on, and no failure shows the bot's token.
func TestTelegramFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"a wrong token", 401, `{"ok":false,"error_code":401,"description":"Unauthorized"}`,
			"Telegram answered 401: Unauthorized. Telegram does not know this bot token. Get the token of the bot from @BotFather in Telegram and write it into the file that bot_token_file names."},
		{"a chat that the bot does not know", 400, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`,
			"Telegram answered 400: Bad Request: chat not found. Check chat_id, and that the bot is a member of the chat."},
		{"a chat that the bot is not in", 403, `{"ok":false,"error_code":403,"description":"Forbidden: bot is not a member of the channel chat"}`,
			"Telegram answered 403: Forbidden: bot is not a member of the channel chat. The bot may not write to this chat. Add the bot to the group or the channel. A person must first send the bot a message."},
		{"a rate limit", 429, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 35","parameters":{"retry_after":35}}`,
			"Telegram answered 429: Too Many Requests: retry after 35. Telegram takes no more messages from this bot for 35 seconds."},
		{"a group that became a supergroup", 400, `{"ok":false,"error_code":400,"description":"Bad Request: group chat was upgraded to a supergroup chat","parameters":{"migrate_to_chat_id":-1001234567890}}`,
			"Telegram answered 400: Bad Request: group chat was upgraded to a supergroup chat. The group became a supergroup, which has a new number. Set chat_id to \"-1001234567890\"."},
		{"another refusal", 400, `{"ok":false,"error_code":400,"description":"Bad Request: message text is empty"}`, "Telegram answered 400: Bad Request: message text is empty."},
		{"a page of another server", 502, "<html><body>Bad Gateway</body></html>",
			"the Bot API server answered 502 without the JSON that Telegram answers with. Check bot_api_server_url, and what stands between this node and that server."},
		{"JSON of another server, whose description is no text", 400, `{"ok":false,"description":{"code":400}}`,
			"the Bot API server answered 400 without the JSON that Telegram answers with. Check bot_api_server_url, and what stands between this node and that server."},
		{"JSON that is no object", 500, `["message is too long"]`,
			"the Bot API server answered 500 without the JSON that Telegram answers with. Check bot_api_server_url, and what stands between this node and that server."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := startService(t, func(received) (int, string) { return tc.status, tc.body })
			inputs := map[string]any{"bot_token_file": "/etc/mmwatchdog/secrets/telegram-bot", "chat_id": "-1001234567890", "bot_api_server_url": server.URL}
			o := run(t, "telegram", failure, inputs, map[string]string{"bot_token_file": telegramToken})
			if o.exitStatus != 0 || o.status != "failed" || o.message != tc.want {
				t.Fatalf("got  %q\nwant %q\n%+v", o.message, tc.want, o)
			}
			if len(server.got()) != 1 || o.holds(telegramToken) {
				t.Fatalf("%d requests; %+v", len(server.got()), o)
			}
		})
	}
	t.Run("nothing listens at the address", func(t *testing.T) {
		listener, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		address := "http://" + listener.Addr().String()
		listener.Close()
		inputs := map[string]any{"bot_token_file": "/etc/mmwatchdog/secrets/telegram-bot", "chat_id": "-1001234567890", "bot_api_server_url": address}
		o := run(t, "telegram", failure, inputs, map[string]string{"bot_token_file": telegramToken})
		if o.exitStatus != 0 || o.status != "failed" || !strings.HasPrefix(o.message, "MMWatchdog could not send the message to Telegram: ") || !strings.Contains(o.message, "connection refused") || o.holds(telegramToken) || o.holds("/sendMessage") {
			t.Fatalf("%+v", o)
		}
	})
	// Invalid secret file contents fail before a request is sent.
	server := telegram(t)
	for _, tc := range []struct {
		name         string
		inputs       map[string]any
		fileContents map[string]string
		want         string
	}{
		{"a file that holds more than a token", map[string]any{"bot_token_file": "/f", "chat_id": "1", "bot_api_server_url": server.URL}, map[string]string{"bot_token_file": "123456789:AAE\" --output /tmp/x \""},
			"the file that bot_token_file names must hold one bot token, which looks like 123456789:AAE-x1y2z3, and nothing else"},
		{"a file that holds two lines", map[string]any{"bot_token_file": "/f", "chat_id": "1", "bot_api_server_url": server.URL}, map[string]string{"bot_token_file": telegramToken + "\n" + telegramToken},
			"the file that bot_token_file names must hold one bot token, which looks like 123456789:AAE-x1y2z3, and nothing else"},
		{"a token that would change the address", map[string]any{"bot_token_file": "/f", "chat_id": "1", "bot_api_server_url": server.URL}, map[string]string{"bot_token_file": "123456789:AAE/../getMe"},
			"the file that bot_token_file names must hold one bot token, which looks like 123456789:AAE-x1y2z3, and nothing else"},
		{"an empty file", map[string]any{"bot_token_file": "/f", "chat_id": "1", "bot_api_server_url": server.URL}, map[string]string{"bot_token_file": ""},
			"the file that bot_token_file names must hold one bot token, which looks like 123456789:AAE-x1y2z3, and nothing else"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := run(t, "telegram", failure, tc.inputs, tc.fileContents)
			if o.exitStatus != 0 || o.status != "failed" || o.message != tc.want || o.holds(telegramToken) {
				t.Fatalf("%+v", o)
			}
		})
	}
	if requests := server.got(); len(requests) != 0 {
		t.Fatalf("the module sent a request with a token or to an address that is none: %+v", requests)
	}
}

func TestSlackHidesTheWebhookPathInDeliveryErrors(t *testing.T) {
	t.Parallel()
	segments := strings.Split(slackSecret, "/")
	for _, body := range []string{"/services/" + slackSecret, slackSecret, segments[0], segments[1], segments[2]} {
		t.Run(body, func(t *testing.T) {
			server := startService(t, func(received) (int, string) { return 500, "upstream refused " + body })
			result := run(t, "slack", failure, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/slack"}, map[string]string{"webhook_url_file": server.URL + "/services/" + slackSecret})
			if result.exitStatus != 0 || result.status != "failed" {
				t.Fatalf("%+v", result)
			}
			for _, secret := range segments {
				if result.holds(secret) {
					t.Fatalf("webhook path exposed in delivery error: %+v", result)
				}
			}
			if !strings.Contains(result.message, "[path of the webhook]") && !strings.Contains(result.message, "[part of the webhook's path]") {
				t.Fatalf("missing redaction: %+v", result)
			}
		})
	}
}
