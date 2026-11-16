package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/testhelpers"
)

// answering is the script of a module that saves the request it reads to
// the file requestPath and writes answer to its standard output.
func answering(requestPath, answer string) string {
	return "cat > " + testhelpers.ShellQuote(requestPath) + "\nprintf '%s' " + testhelpers.ShellQuote(answer)
}

const moduleDelivered = `{"protocol":1,"status":"delivered","message":"Slack accepted the message"}`

// moduleDestination is a destination that delivers through the module name
// with these inputs, as the account of the test.
func moduleDestination(t *testing.T, name string, inputs map[string]any) checksandalerts.Destination {
	t.Helper()
	// The time limit of an attempt is testhelpers.LongestWaitForAnEvent, so
	// that a module that delivers is never ended by it, also on a busy
	// machine. A test of the time limit sets its own.
	return checksandalerts.Destination{RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, TimeLimitPerDeliveryAttempt: "1m", Inputs: &checksandalerts.ModuleCall{Name: name, Inputs: inputs, ExecOptions: testhelpers.OwnOptions(t)}}
}

// slackDescription describes the stand-in for the module slack that the
// tests install: one required file input and two optional inputs.
const slackDescription = `summary: Stands for the module slack in a test.
used_as: alert_destination
inputs:
  webhook_url_file: {type: string, required: true, description: The file that holds the address of the webhook.}
  mention: {type: string, required: false, description: A mention that starts every message.}
  message_thread_id: {type: integer, required: false, description: The thread that gets the message.}
examples:
  - description: Send every alert to the team's channel.
    config: |
      slack:
        webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team
      remind_every: never
      alert_severities: [info, warn, sos]
`

// sendThrough hands an alert to the module of a destination, as a node
// does whose external_module_dir is modules.
func sendThrough(t *testing.T, ctx context.Context, modules string, destination checksandalerts.Destination, alert checksandalerts.Event) error {
	t.Helper()
	return checksandalerts.ModuleSender{Delivery: checksandalerts.DeliveryOf{Destination: destination, Modules: testhelpers.ModulesIn(t, modules)}}.Send(ctx, alert)
}

func writeModuleFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "slack-webhook-team")
	if e := os.WriteFile(path, []byte(content), 0600); e != nil {
		t.Fatal(e)
	}
	return path
}

// waitForFile returns the content of a file once a module has written it.
func waitForFile(t *testing.T, path string) []byte {
	t.Helper()
	for end := time.Now().Add(testhelpers.LongestWaitForAnEvent); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if b, e := os.ReadFile(path); e == nil && len(b) > 0 {
			return b
		}
	}
	t.Fatal("no module wrote", path)
	return nil
}

var moduleAlert = checksandalerts.Event{ID: "0123456789abcdef0123456789abcdef", Incident: "fedcba9876543210fedcba9876543210", Node: "app-1", CheckGroup: "myapp", Check: "http", Type: "failed", AlertSeverity: "sos",
	At: time.Date(2026, 9, 29, 14, 3, 5, 0, time.UTC), Title: "app-1: myapp/http failed", TitleWithAlertSeverity: "SOS: app-1: myapp/http failed", Body: "expected 200, received 503\nfailing since 14:02 UTC", Message: "expected 200, received 503"}

// MMWatchdog writes one JSON object to a destination module's standard input: the
// protocol, the whole alert, the destination's inputs as written, the
// content of each file that an input names, read again for every delivery
// and given without the line break at its end, and when MMWatchdog ends the module.
// The module has MMWATCHDOG_MODULE_HELPER in its environment.
func TestDestinationModuleRequest(t *testing.T) {
	t.Parallel()
	modules, request := t.TempDir(), filepath.Join(t.TempDir(), "request.json")
	testhelpers.InstallModule(t, modules, "slack", slackDescription, "[[ $MMWATCHDOG_MODULE_HELPER = /* ]] || exit 9\n"+answering(request, moduleDelivered))
	file := writeModuleFile(t, "https://hooks.slack.example/T000/B000/XXXX\n")
	// The characters <, >, and & of an input arrive as the destination
	// states them, not as \u003c, \u003e, and \u0026.
	destination := moduleDestination(t, "slack", map[string]any{"webhook_url_file": file, "mention": "<@U024BE7LH> & <!here>", "message_thread_id": 42})
	var sender checksandalerts.NotificationSender = checksandalerts.ModuleSender{Delivery: checksandalerts.DeliveryOf{Destination: destination, Modules: testhelpers.ModulesIn(t, modules)}}
	if e := sender.Send(context.Background(), moduleAlert); e != nil {
		t.Fatal(e)
	}
	got, _ := os.ReadFile(request)
	want := `{"protocol":1,"alert":{"id":"0123456789abcdef0123456789abcdef","incident_id":"fedcba9876543210fedcba9876543210","node":"app-1","check_group":"myapp","check":"http","type":"failed","alert_severity":"sos","at":"2026-09-29T14:03:05Z","title":"app-1: myapp/http failed","title_with_alert_severity":"SOS: app-1: myapp/http failed","body":"expected 200, received 503\nfailing since 14:02 UTC","message":"expected 200, received 503"},` +
		`"inputs":{"mention":"<@U024BE7LH> & <!here>","message_thread_id":42,"webhook_url_file":"` + file + `"},"file_contents":{"webhook_url_file":"https://hooks.slack.example/T000/B000/XXXX"},"mmwatchdog_ends_the_module_after_milliseconds":`
	// MMWatchdog ends the module at the destination's time limit of 1 minute,
	// which began just before MMWatchdog started the module.
	left, stated := strings.CutPrefix(string(got), want)
	milliseconds, e := strconv.Atoi(strings.TrimSuffix(left, "}"))
	if !stated || !strings.HasSuffix(left, "}") || e != nil || milliseconds <= 0 || milliseconds > 60000 {
		t.Fatalf("request:\n got %s\nwant %s and the milliseconds until MMWatchdog ends the module, at most 60000}", got, want)
	}
	// The file is read for every delivery, so a rotated credential needs no
	// reload. A destination that takes only the title gets an alert without
	// body, message, and evidence.
	if e := os.WriteFile(file, []byte("https://hooks.slack.example/T000/B000/ROTATED\r\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := sender.Send(context.Background(), moduleAlert.OnlyTheTitle()); e != nil {
		t.Fatal(e)
	}
	var second struct {
		Alert        map[string]any    `json:"alert"`
		FileContents map[string]string `json:"file_contents"`
	}
	got, _ = os.ReadFile(request)
	if e := json.Unmarshal(got, &second); e != nil {
		t.Fatal(e)
	}
	if second.FileContents["webhook_url_file"] != "https://hooks.slack.example/T000/B000/ROTATED" {
		t.Fatal("the file was not read again:", second.FileContents)
	}
	if second.Alert["title"] != "app-1: myapp/http failed" || second.Alert["title_with_alert_severity"] != "SOS: app-1: myapp/http failed" || second.Alert["id"] != moduleAlert.ID || second.Alert["body"] != nil || second.Alert["message"] != nil || second.Alert["evidence"] != nil {
		t.Fatal("the alert of a destination that takes only the title:", second.Alert)
	}
	// A module without inputs gets empty objects, not null.
	testhelpers.InstallModule(t, modules, "own_alarm", testhelpers.TestModuleDescription("own_alarm", checksandalerts.UsedAsAlertDestination, nil, nil), answering(request, moduleDelivered))
	if e := sendThrough(t, context.Background(), modules, moduleDestination(t, "own_alarm", nil), moduleAlert); e != nil {
		t.Fatal(e)
	}
	if got, _ = os.ReadFile(request); !strings.Contains(string(got), `,"inputs":{},"file_contents":{},`) {
		t.Fatalf("request without inputs: %s", got)
	}
}

// A destination module answers delivered or failed with a message. A
// nonzero exit status, a run past the time limit, and any other output are
// failed attempts, each with an error that names the module and says what
// happened.
func TestDestinationModuleAnswers(t *testing.T) {
	t.Parallel()
	modules := t.TempDir()
	secret := "https://hooks.slack.example/T000/B000/XXXX"
	file := writeModuleFile(t, secret+"\n")
	for _, tc := range []struct {
		name, script, want string
	}{
		{"delivered", "cat >/dev/null\nprintf '%s\\n' " + testhelpers.ShellQuote(moduleDelivered), ""},
		{"failed", "cat >/dev/null\nprintf '%s' '{\"protocol\":1,\"status\":\"failed\",\"message\":\"Slack answered 404:\\nthe webhook does not exist\"}'", "the module slack reported: Slack answered 404: the webhook does not exist"},
		{"a nonzero exit status", "echo 'curl: (6) Could not resolve host' >&2\necho 'a second line' >&2\nprintf '%s' " + testhelpers.ShellQuote(moduleDelivered) + "\nexit 3", "the module slack ended with exit status 3: curl: (6) Could not resolve host"},
		{"a nonzero exit status without a word", "exit 1", "the module slack ended with exit status 1"},
		{"no answer", "cat >/dev/null", "the module slack ended without an answer on its standard output"},
		{"an answer that is not JSON", "echo delivered", "the module slack did not answer with one JSON object of protocol, status, and message: "},
		{"two answers", "printf '%s %s' " + testhelpers.ShellQuote(moduleDelivered) + " " + testhelpers.ShellQuote(moduleDelivered), "the module slack did not answer with one JSON object of protocol, status, and message: "},
		{"a member that the protocol does not have", `printf '%s' '{"protocol":1,"status":"delivered","message":"sent","measurements":{}}'`, `the module slack did not answer with one JSON object of protocol, status, and message: json: unknown field "measurements"`},
		{"a status twice", `printf '%s' '{"protocol":1,"status":"failed","status":"delivered","message":"sent"}'`, `duplicate JSON key "status"`},
		{"the status of a check", `printf '%s' '{"protocol":1,"status":"ok","message":"sent"}'`, `the module slack answered with the status "ok", and a destination module answers delivered or failed`},
		{"another protocol", `printf '%s' '{"protocol":2,"status":"delivered","message":"sent"}'`, "the module slack returned protocol 2; MMWatchdog requires protocol: 1"},
		{"delivered without a message", `printf '%s' '{"protocol":1,"status":"delivered"}'`, "the module slack answered delivered without a message, and a message is required"},
		{"failed without a message", `printf '%s' '{"protocol":1,"status":"failed","message":""}'`, "the module slack answered failed without a message, and a message is required"},
		{"ended by a signal", "echo stopping >&2\nkill -KILL $$", "the module slack was ended by a signal: stopping"},
		{"too much output", "head -c 70000 /dev/zero | tr '\\0' x", "the module slack wrote more than 65536 bytes to its standard output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testhelpers.InstallModule(t, modules, "slack", slackDescription, tc.script)
			e := sendThrough(t, context.Background(), modules, moduleDestination(t, "slack", map[string]any{"webhook_url_file": file}), moduleAlert)
			if tc.want == "" && e != nil || tc.want != "" && (e == nil || !strings.Contains(e.Error(), tc.want)) {
				t.Fatalf("got %v, want %q", e, tc.want)
			}
			if e != nil && strings.ContainsAny(e.Error(), "\n\r") {
				t.Fatalf("the error has more than one line: %q", e)
			}
		})
	}
	// A module must never repeat a credential. If one does, in its message
	// or on its standard error, the content of the file does not reach the
	// error of the attempt.
	for _, script := range []string{
		`url=$(sed 's/.*"file_contents":{"webhook_url_file":"\([^"]*\)"}.*/\1/')` + "\n" + `printf '{"protocol":1,"status":"failed","message":"POST %s answered 404"}' "$url"`,
		`url=$(sed 's/.*"file_contents":{"webhook_url_file":"\([^"]*\)"}.*/\1/')` + "\n" + `echo "curl: cannot reach $url" >&2` + "\nexit 7",
	} {
		testhelpers.InstallModule(t, modules, "slack", slackDescription, script)
		e := sendThrough(t, context.Background(), modules, moduleDestination(t, "slack", map[string]any{"webhook_url_file": file}), moduleAlert)
		if e == nil || strings.Contains(e.Error(), "hooks.slack.example") || !strings.Contains(e.Error(), "[content of webhook_url_file]") {
			t.Fatal("a file content reached the error:", e)
		}
	}
}

// A module that is not installed, an executable that cannot be run, a
// module that this node uses as a check, an account that does not exist, a
// file that cannot be read, a run past the time limit, and a cancelled
// delivery each fail the attempt.
func TestDestinationModuleCannotRun(t *testing.T) {
	t.Parallel()
	modules := t.TempDir()
	send := func(d checksandalerts.Destination) error {
		return sendThrough(t, context.Background(), modules, d, moduleAlert)
	}
	// A joined node gets the hub's destination and need not have its module.
	if e := send(moduleDestination(t, "own_alarm", nil)); e == nil || e.Error() != "the module own_alarm is not installed on this node" {
		t.Fatal(e)
	}
	if e := (checksandalerts.ModuleSender{Delivery: checksandalerts.DeliveryOf{Destination: moduleDestination(t, "own_alarm", nil)}}).Send(context.Background(), moduleAlert); e == nil || e.Error() != "the module own_alarm is not installed on this node" {
		t.Fatal("a node that has no module that is not core:", e)
	}
	description := testhelpers.TestModuleDescription("own_alarm", checksandalerts.UsedAsAlertDestination, map[string]string{"api_token_file": "string"}, nil)
	testhelpers.InstallModule(t, modules, "own_alarm", description, "true")
	program := filepath.Join(modules, "own_alarm", checksandalerts.ModuleExecutableFile)
	testhelpers.WriteNotExecutableFile(t, program, "#!/bin/bash\ntrue\n")
	if e := send(moduleDestination(t, "own_alarm", nil)); e == nil || e.Error() != "the module own_alarm is not installed on this node: "+program+" is not an executable file" {
		t.Fatal("a file that is not executable ran:", e)
	}
	ran := filepath.Join(t.TempDir(), "ran")
	testhelpers.InstallModule(t, modules, "queued_jobs", testhelpers.TestModuleDescription("queued_jobs", checksandalerts.UsedAsCheck, nil, nil), answering(ran, `{"protocol":1,"status":"ok","message":"fine"}`))
	if e := send(moduleDestination(t, "queued_jobs", nil)); e == nil || e.Error() != "the module queued_jobs on this node is used as a check, not as an alert destination" {
		t.Fatal(e)
	}
	testhelpers.InstallModule(t, modules, "own_alarm", description, answering(ran, moduleDelivered))
	stranger := moduleDestination(t, "own_alarm", nil)
	stranger.Inputs.(*checksandalerts.ModuleCall).RunAsUserAccount = "mmwatchdog-no-such-user"
	if e := send(stranger); e == nil || !strings.Contains(e.Error(), `the module own_alarm could not run: run_as_user_account "mmwatchdog-no-such-user"`) {
		t.Fatal(e)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if e := send(moduleDestination(t, "own_alarm", map[string]any{"api_token_file": missing})); e == nil || !strings.Contains(e.Error(), "MMWatchdog cannot read the file of the input api_token_file of the module own_alarm: open "+missing) {
		t.Fatal(e)
	}
	if _, e := os.Stat(ran); e == nil {
		t.Fatal("the module ran although it could not get what it needs")
	}

	testhelpers.InstallModule(t, modules, "own_alarm", description, "sleep 30")
	slow := moduleDestination(t, "own_alarm", nil)
	slow.TimeLimitPerDeliveryAttempt = "300ms"
	start := time.Now()
	if e := send(slow); e == nil || e.Error() != "the module own_alarm did not finish within 300ms (time_limit_per_delivery_attempt), so MMWatchdog stopped the module" {
		t.Fatal(e)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the time limit did not end the module:", time.Since(start))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := sendThrough(t, ctx, modules, moduleDestination(t, "own_alarm", nil), moduleAlert); e != context.Canceled {
		t.Fatal("ignored a cancelled delivery:", e)
	}
	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start = time.Now()
	if e := sendThrough(t, ctx, modules, moduleDestination(t, "own_alarm", nil), moduleAlert); e != context.Canceled || time.Since(start) > 4*time.Second {
		t.Fatal("a delivery cancelled while its module ran:", e, time.Since(start))
	}
}

// The content of a file that an input of a destination module names is UTF-8
// text of at most 16 KiB, given without the line break at its end.
func TestReadModuleFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "credential")
	for content, want := range map[string]string{
		"token\n":       "token",
		"token\r\n":     "token",
		"token":         "token",
		" two words \n": " two words ",
		"-----BEGIN KEY-----\nabc\n-----END-----\n": "-----BEGIN KEY-----\nabc\n-----END-----",
		"": "",
		strings.Repeat("k", checksandalerts.ModuleFileLimit): strings.Repeat("k", checksandalerts.ModuleFileLimit),
	} {
		os.WriteFile(path, []byte(content), 0600)
		if got, e := (&checksandalerts.ModuleCall{}).ReadCredentialFile("api_token_file", path); e != nil || got != want {
			t.Errorf("%q: got %q, %v", content, got, e)
		}
	}
	for _, content := range []string{strings.Repeat("k", checksandalerts.ModuleFileLimit+1), "not \xff UTF-8", "a\x00b"} {
		os.WriteFile(path, []byte(content), 0600)
		if _, e := (&checksandalerts.ModuleCall{}).ReadCredentialFile("api_token_file", path); e == nil || e.Error() != path+": expected UTF-8 text of at most 16384 bytes" {
			t.Errorf("%.20q: %v", content, e)
		}
	}
}

// A destination names a module that is not core by the module's name and
// writes the module's inputs below that name, with the run settings beside
// them. MMWatchdog checks the inputs against the module's description. An input
// whose name ends in _file names a file by its absolute path. The module
// runs as the account mmwatchdog unless the destination states another.
func TestDestinationModuleSettingsAreValidated(t *testing.T) {
	t.Parallel()
	modules := t.TempDir()
	delivers := "def deliver_alert(alert, inputs, file_contents):\n    return {\"status\": \"delivered\", \"message\": \"sent\"}\n"
	testhelpers.InstallStarlarkModule(t, modules, "slack", slackDescription, delivers)
	testhelpers.InstallStarlarkModule(t, modules, "telegram", testhelpers.TestModuleDescription("telegram", checksandalerts.UsedAsAlertDestination, map[string]string{"bot_token_file": "string", "chat_id": "string"}, nil), delivers)
	testhelpers.InstallModule(t, modules, "own_alarm", testhelpers.TestModuleDescription("own_alarm", checksandalerts.UsedAsAlertDestination, nil, nil), "true")
	testhelpers.InstallModule(t, modules, "queued_jobs", testhelpers.TestModuleDescription("queued_jobs", checksandalerts.UsedAsCheck, nil, nil), "true")
	path := testhelpers.ConfigFile(t, "external_module_dir: "+modules+"\ncheck_groups: {a: {shell: echo ok}}\n")
	alerts := filepath.Join(path, "alert_destinations.yml")
	load := func(destination string) (*config.Config, error) {
		os.WriteFile(alerts, []byte("destinations:\n  team_chat: {"+destination+"}\n"), 0600)
		return config.Load(path)
	}
	c, e := load("slack: {webhook_url_file: /etc/mmwatchdog/secrets/slack-webhook-team, mention: '<@U024BE7LH>'}, remind_every: never, alert_severities: [info, warn, sos]")
	if e != nil {
		t.Fatal(e)
	}
	d := c.Alerts.Destinations["team_chat"]
	call := d.Inputs.(*checksandalerts.ModuleCall)
	if d.TimeLimitPerDeliveryAttempt != "5s" || d.ModuleName() != "slack" || d.ModuleKind() != checksandalerts.ModuleKindProjectStarlark || call.RunAsUserAccount != "mmwatchdog" || call.RunFromDirectory != "/" ||
		!reflect.DeepEqual(call.Inputs, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/slack-webhook-team", "mention": "<@U024BE7LH>"}) ||
		!reflect.DeepEqual(call.CredentialFiles(), map[string]string{"webhook_url_file": "/etc/mmwatchdog/secrets/slack-webhook-team"}) {
		t.Fatalf("defaults: %+v %+v", d, call)
	}
	// The run settings are written beside the inputs and are no inputs.
	c, e = load("telegram: {bot_token_file: /etc/mmwatchdog/secrets/telegram-bot, chat_id: '-1001234567890', run_as_user_account: alerts, run_from_directory: /var/lib/alerts, env: {HTTPS_PROXY: 'http://proxy.example.com:3128'}}, remind_every: never, alert_severities: [sos]")
	if e != nil {
		t.Fatal(e)
	}
	call = c.Alerts.Destinations["team_chat"].Inputs.(*checksandalerts.ModuleCall)
	if call.RunAsUserAccount != "alerts" || call.RunFromDirectory != "/var/lib/alerts" || !reflect.DeepEqual(call.Env, map[string]string{"HTTPS_PROXY": "http://proxy.example.com:3128"}) ||
		!reflect.DeepEqual(call.Inputs, map[string]any{"bot_token_file": "/etc/mmwatchdog/secrets/telegram-bot", "chat_id": "-1001234567890"}) {
		t.Fatalf("run settings beside the inputs: %+v", call)
	}
	for _, valid := range []string{
		"own_alarm: {}, remind_every: never, alert_severities: [sos]",
		"own_alarm: {run_as_user_account: alerts}, remind_every: never, alert_severities: [sos]",
		"slack: {webhook_url_file: /etc/mmwatchdog/secrets/slack, message_thread_id: 42}, remind_every: 1m, send_only_the_alert_title: true, time_limit_per_delivery_attempt: 20s, alert_severities: []",
	} {
		if _, e := load(valid); e != nil {
			t.Errorf("%s: %v", valid, e)
		}
	}
	for _, tc := range []struct{ destination, want string }{
		{"own-alarm: {}, remind_every: never, alert_severities: [sos]", `alert_destinations.yml:2:15: destinations.team_chat.own-alarm: unknown field "own-alarm": the field name matches neither a setting of a destination nor an installed module. MMWatchdog has no core module of that name, and no directory of that name is in `},
		{"unavailable_alarm: {}, remind_every: never, alert_severities: [sos]", ` or in ` + modules + ` (own_alarm, queued_jobs, slack, telegram)`},
		{"external_module: {name: slack, inputs: {webhook_url_file: /etc/mmwatchdog/secrets/slack}}, remind_every: never, alert_severities: [sos]", `unknown field "external_module"`},
		{"slack: /etc/mmwatchdog/secrets/slack, remind_every: never, alert_severities: [sos]", `alert_destinations.yml:2:15: destinations.team_chat.slack: expected a mapping, found the string "/etc/mmwatchdog/secrets/slack"`},
		{"slack: {webhook_url_file: secrets/slack}, remind_every: never, alert_severities: [sos]", "destinations.team_chat.slack.webhook_url_file: an input whose name ends in _file names a file that MMWatchdog reads and gives to the module. Set webhook_url_file to an absolute file path"},
		{"slack: {webhook_url_file: 7}, remind_every: never, alert_severities: [sos]", "destinations.team_chat.slack.webhook_url_file: an input whose name ends in _file names a file that MMWatchdog reads and gives to the module. Set webhook_url_file to an absolute file path"},
		{"slack: {webhook_url_file: /etc/mmwatchdog/secrets/slack, run_from_directory: relative}, remind_every: never, alert_severities: [sos]", "destinations.team_chat.slack.run_from_directory: must be absolute"},
		{"slack: {webhook_url_file: /etc/mmwatchdog/secrets/slack, env: {BASH_ENV: /tmp/x}}, remind_every: never, alert_severities: [sos]", "destinations.team_chat.slack.env.BASH_ENV: unsupported environment variable"},
		{"slack: {webhook_url_file: /etc/mmwatchdog/secrets/slack, channel: '#ops'}, remind_every: never, alert_severities: [sos]", `destinations.team_chat.slack.channel: unknown field "channel": the inputs of the module slack are webhook_url_file, mention, message_thread_id`},
		{"slack: {mention: '<!channel>'}, remind_every: never, alert_severities: [sos]", "destinations.team_chat.slack.webhook_url_file: is required: The file that holds the address of the webhook."},
		{"slack: {webhook_url_file: /etc/mmwatchdog/secrets/slack, mention: 7}, remind_every: never, alert_severities: [sos]", "destinations.team_chat.slack.mention: expected a string"},
		{"slack: {webhook_url_file: /etc/mmwatchdog/secrets/slack, message_thread_id: '42'}, remind_every: never, alert_severities: [sos]", "destinations.team_chat.slack.message_thread_id: expected a whole number"},
		{"own_alarm: {webhook_url_file: /etc/mmwatchdog/secrets/slack}, remind_every: never, alert_severities: [sos]", `destinations.team_chat.own_alarm.webhook_url_file: unknown field "webhook_url_file": the module own_alarm takes no input`},
		{"queued_jobs: {}, remind_every: never, alert_severities: [sos]", "destinations.team_chat.queued_jobs: the module queued_jobs is used as a check, not as an alert destination: its description says used_as: check"},
		{"slack: {webhook_url_file: /etc/mmwatchdog/secrets/slack}, webhook: {url: 'https://example.com'}, remind_every: never, alert_severities: [sos]", "names the modules slack and webhook: a destination names exactly one module"},
		{"slack: {webhook_url_file: /etc/mmwatchdog/secrets/slack}, own_alarm: {}, remind_every: never, alert_severities: [sos]", "names the modules slack and own_alarm: a destination names exactly one module"},
		{"slack: {webhook_url_file: /etc/mmwatchdog/secrets/slack}, remind_every: never", "destinations.team_chat.alert_severities: required"},
	} {
		if _, e := load(tc.destination); e == nil || !strings.Contains(e.Error(), tc.want) {
			t.Errorf("%s:\n got %v\nwant %q", tc.destination, e, tc.want)
		}
	}
	// Two destinations that use the same module each keep their own
	// settings.
	os.WriteFile(alerts, []byte("destinations:\n  team_chat: {remind_every: never, slack: {webhook_url_file: /etc/mmwatchdog/secrets/team}, alert_severities: [sos]}\n  mail_administrator_chat: {remind_every: never, slack: {webhook_url_file: /etc/mmwatchdog/secrets/mail, mention: '@mail', run_as_user_account: alerts}, alert_severities: []}\n"), 0600)
	if c, e = config.Load(path); e != nil || c.Alerts.Destinations["team_chat"].Inputs.(*checksandalerts.ModuleCall).RunAsUserAccount != "mmwatchdog" || c.Alerts.Destinations["mail_administrator_chat"].Inputs.(*checksandalerts.ModuleCall).RunAsUserAccount != "alerts" {
		t.Fatal(c, e)
	}
}

// mmwatchdog validate finds the code of a destination's module, the
// account and the directory it runs with, and every file that its inputs
// name. It reads a script without running it, and refuses one that does not
// define the function deliver_alert.
func TestValidateRuntimeChecksDestinationModules(t *testing.T) {
	t.Parallel()
	own := testhelpers.OwnOptions(t)
	modules := t.TempDir()
	testhelpers.InstallModule(t, modules, "slack", slackDescription, "true")
	testhelpers.InstallStarlarkModule(t, modules, "own_alarm", testhelpers.TestModuleDescription("own_alarm", checksandalerts.UsedAsAlertDestination, nil, nil), "def deliver_alert(alert, inputs, file_contents):\n    return {\"status\": \"delivered\", \"message\": \"sent\"}\n")
	testhelpers.InstallModule(t, modules, "plain", testhelpers.TestModuleDescription("plain", checksandalerts.UsedAsAlertDestination, nil, nil), "true")
	testhelpers.WriteNotExecutableFile(t, filepath.Join(modules, "plain", checksandalerts.ModuleExecutableFile), "#!/bin/bash\ntrue\n")
	testhelpers.InstallStarlarkModule(t, modules, "script_of_a_check", testhelpers.TestModuleDescription("script_of_a_check", checksandalerts.UsedAsAlertDestination, nil, nil), testhelpers.ScriptOfACheck)
	testhelpers.InstallStarlarkModule(t, modules, "wrong_syntax", testhelpers.TestModuleDescription("wrong_syntax", checksandalerts.UsedAsAlertDestination, nil, nil), "def deliver_alert(alert, inputs, file_contents):\nreturn {}\n")
	file := writeModuleFile(t, "https://hooks.slack.example/T000/B000/XXXX\n")
	binary := filepath.Join(t.TempDir(), "binary")
	os.WriteFile(binary, []byte("not \xff UTF-8"), 0600)
	path := testhelpers.ConfigFile(t, "external_module_dir: "+modules+"\ncheck_groups: {a: {http: {url: 'https://example.com'}}}\n")
	runAs := "run_as_user_account: " + own.RunAsUserAccount + ", run_from_directory: " + own.RunFromDirectory
	for _, tc := range []struct{ module, want string }{
		{"slack: {webhook_url_file: " + file + ", " + runAs + "}", ""},
		{"own_alarm: {" + runAs + "}", ""},
		{"plain: {" + runAs + "}", "destinations.team_chat.plain: " + filepath.Join(modules, "plain", checksandalerts.ModuleExecutableFile) + " is not an executable file"},
		{"script_of_a_check: {" + runAs + "}", "destinations.team_chat.script_of_a_check: " + filepath.Join(modules, "script_of_a_check", checksandalerts.ModuleScriptFile) + " defines no function deliver_alert(alert, inputs, file_contents)"},
		{"wrong_syntax: {" + runAs + "}", "destinations.team_chat.wrong_syntax: " + filepath.Join(modules, "wrong_syntax", checksandalerts.ModuleScriptFile) + ":2: got return, want indent"},
		{"slack: {webhook_url_file: " + file + ", run_as_user_account: mmwatchdog-no-such-user, run_from_directory: " + own.RunFromDirectory + "}", "destinations.team_chat.slack.run_as_user_account: "},
		{"slack: {webhook_url_file: " + file + ", run_as_user_account: " + own.RunAsUserAccount + ", run_from_directory: /no/such/dir}", "destinations.team_chat.slack.run_from_directory: /no/such/dir is not a directory"},
		{"slack: {webhook_url_file: /no/such/file, " + runAs + "}", "destinations.team_chat.slack.webhook_url_file: open /no/such/file"},
		{"slack: {webhook_url_file: " + binary + ", " + runAs + "}", "destinations.team_chat.slack.webhook_url_file: " + binary + ": expected UTF-8 text of at most 16384 bytes"},
	} {
		os.WriteFile(filepath.Join(path, "alert_destinations.yml"), []byte("destinations:\n  team_chat: {"+tc.module+", remind_every: never, alert_severities: [sos]}\n"), 0600)
		c, e := config.Load(path)
		if e != nil {
			t.Fatal(tc.module, e)
		}
		e = ValidateRuntime(c)
		if tc.want == "" && e != nil || tc.want != "" && (e == nil || !strings.Contains(e.Error(), tc.want)) {
			t.Errorf("%s:\n got %v\nwant %q", tc.module, e, tc.want)
		}
	}
}

// A deploy stages the complete new module directories beside the installed
// ones and validates the new configuration against them, before anything
// live changes: LoadWithModuleDirs, which is mmwatchdog validate with
// --external-module-dir and --official-extra-module-dir, looks for the
// module of a destination and of a check in the directories given, and not
// in those of the node's settings.
func TestValidationLooksForModulesInTheGivenDirectories(t *testing.T) {
	t.Parallel()
	own := testhelpers.OwnOptions(t)
	installed, candidate, release := t.TempDir(), t.TempDir(), t.TempDir()
	delivers := "def deliver_alert(alert, inputs, file_contents):\n    return {\"status\": \"delivered\", \"message\": \"sent\"}\n"
	testhelpers.InstallModule(t, installed, "removed_by_this_deploy", testhelpers.TestModuleDescription("removed_by_this_deploy", checksandalerts.UsedAsAlertDestination, nil, nil), "true")
	testhelpers.InstallModule(t, candidate, "added_by_this_deploy", testhelpers.TestModuleDescription("added_by_this_deploy", checksandalerts.UsedAsAlertDestination, nil, nil), "true")
	testhelpers.InstallStarlarkModule(t, release, "slack", testhelpers.TestModuleDescription("slack", checksandalerts.UsedAsAlertDestination, nil, nil), delivers)
	if e := os.WriteFile(filepath.Join(release, checksandalerts.ShippedWithVersionFile), []byte(checksandalerts.Version+"\n"), 0644); e != nil {
		t.Fatal(e)
	}
	runAs := "run_as_user_account: " + own.RunAsUserAccount + ", run_from_directory: " + own.RunFromDirectory
	configuration := func(module string) string {
		t.Helper()
		path := testhelpers.ConfigFile(t, "external_module_dir: "+installed+"\ncheck_groups: {a: {http: {url: 'https://example.com'}}}\n")
		if e := os.WriteFile(filepath.Join(path, "alert_destinations.yml"), []byte("destinations:\n  team_chat: {"+module+": {"+runAs+"}, remind_every: never, alert_severities: [sos]}\n"), 0600); e != nil {
			t.Fatal(e)
		}
		return path
	}
	added, removed, official := configuration("added_by_this_deploy"), configuration("removed_by_this_deploy"), configuration("slack")
	c, e := config.LoadWithModuleDirs(added, checksandalerts.ModuleDirs{External: candidate})
	if e != nil {
		t.Fatal("a module that the deploy adds was not found in the new directory:", e)
	}
	// The configuration keeps the node's own setting, and the checks of
	// Validation on the node look for the module's code where the module was found.
	if e := ValidateRuntime(c); e != nil || c.ExternalModuleDir != installed || c.Alerts.Destinations["team_chat"].Inputs.(*checksandalerts.ModuleCall).Module.Directory != filepath.Join(candidate, "added_by_this_deploy") {
		t.Fatal(c.ExternalModuleDir, e)
	}
	if _, e := config.Load(added); e == nil || !strings.Contains(e.Error(), `unknown field "added_by_this_deploy"`) || !strings.Contains(e.Error(), " or in "+installed+" (removed_by_this_deploy)") {
		t.Fatal("without the new directory, validation looks in the node's own:", e)
	}
	// A module that the deploy removes fails validation although the node
	// still has it.
	if _, e := config.LoadWithModuleDirs(removed, checksandalerts.ModuleDirs{External: candidate}); e == nil || !strings.Contains(e.Error(), `alert_destinations.yml:2:15: destinations.team_chat.removed_by_this_deploy: unknown field "removed_by_this_deploy"`) || !strings.Contains(e.Error(), " or in "+candidate+" (added_by_this_deploy)") {
		t.Fatal(e)
	}
	if c, e := config.Load(removed); e != nil || ValidateRuntime(c) != nil {
		t.Fatal(e)
	}
	// The official extra modules of the release that the deploy installs.
	if _, e := config.Load(official); e == nil || !strings.Contains(e.Error(), `unknown field "slack"`) {
		t.Fatal(e)
	}
	c, e = config.LoadWithModuleDirs(official, checksandalerts.ModuleDirs{OfficialExtra: release})
	if e != nil || c.Alerts.Destinations["team_chat"].ModuleKind() != checksandalerts.ModuleKindOfficialExtra || c.Modules.Dirs != (checksandalerts.ModuleDirs{OfficialExtra: release, External: installed}) {
		t.Fatal(e)
	}
}

// The largest alert that MMWatchdog writes, the files of one destination, and its
// inputs always fit together in one request to a module, which is at most
// ModuleRequestLimit bytes: each file holds at most 16 KiB, the files of
// one destination at most 64 KiB together, and its inputs at most 256 KiB
// as JSON. mmwatchdog validate and every delivery refuse files that hold
// more, and mmwatchdog validate refuses larger inputs.
func TestAnAlertAndTheFilesOfADestinationFitInOneRequest(t *testing.T) {
	t.Parallel()
	// The largest alert: every text as long as MMWatchdog lets it be, of a character
	// that JSON writes with six bytes.
	costly := func(n int) string { return strings.Repeat("\x01", n) }
	longName := strings.Repeat("n", 253)
	observation := checksandalerts.Observation{Status: "failed", Message: costly(8192), Stdout: costly(primitives.ResultLimit), Stderr: costly(primitives.DiagnosticLimit)}
	state := &incidents.CheckState{CheckGroup: longName, Check: longName}
	track := &incidents.Track{AlertSeverity: "sos", Evidence: checksandalerts.CaptureEvidence("incident-1", observation, checksandalerts.SourceLocation{File: "/etc/mmwatchdog/check_groups/" + longName + ".yml"}, checksandalerts.Timing{})}
	alert := state.Event(track, "failed", "incident-1", costly(8192), time.Now().UTC(), true)
	alert.Node = longName
	alert.WriteAlertTexts([]checksandalerts.Event{alert}, "")
	// The alert names up to 20 checks that its check keeps quiet, each as
	// CHECK_GROUP/CHECK, and its body names them again.
	for range config.QuietChecksNamedInAnAlert {
		alert.NoAlertsOrRecoveryActionsForTheseChecksWhileThisCheckFails = append(alert.NoAlertsOrRecoveryActionsForTheseChecksWhileThisCheckFails, longName+"/"+longName)
	}
	alert.NumberOfChecksWithNoAlertsOrRecoveryActionsWhileThisCheckFails = 1000
	alert.WriteAlertTexts([]checksandalerts.Event{alert}, "")
	if !strings.Contains(alert.Body, "/"+longName+", and 980 more") {
		t.Fatal("the body of the largest alert does not name the quiet checks")
	}
	// A hub's grouped message names up to 20 nodes, and a late one says so.
	for range 20 {
		alert.Nodes = append(alert.Nodes, longName)
	}
	alert.Message += costly(1024)
	alert.Body += costly(1024)
	encoded, e := checksandalerts.PlainJSON(alert)
	if e != nil || len(encoded) > 256*1024 {
		t.Fatalf("the largest alert is %d bytes as JSON, and moduleFilesLimit counts on less than 256 KiB: %v", len(encoded), e)
	}

	modules, request := t.TempDir(), filepath.Join(t.TempDir(), "request.json")
	testhelpers.InstallModule(t, modules, "own_alarm", testhelpers.TestModuleDescription("own_alarm", checksandalerts.UsedAsAlertDestination, map[string]string{"a_file": "string", "b_file": "string", "c_file": "string", "d_file": "string", "e_file": "string", "text": "string"}, nil), answering(request, moduleDelivered))
	inputs := map[string]any{}
	for _, input := range []string{"a_file", "b_file", "c_file", "d_file"} {
		inputs[input] = writeModuleFile(t, costly(checksandalerts.ModuleFileLimit))
	}
	// Inputs of the largest size that validation accepts.
	inputs["text"] = ""
	empty, _ := checksandalerts.PlainJSON(checksandalerts.ModuleInputs(inputs))
	inputs["text"] = strings.Repeat("t", checksandalerts.ModuleInputsLimit-len(empty))
	destination := moduleDestination(t, "own_alarm", inputs)
	asLoadValidatesIt := checksandalerts.InputValidation{UsedAs: checksandalerts.UsedAsAlertDestination, Modules: testhelpers.ModulesIn(t, modules)}
	if problem := destination.Inputs.Validate(asLoadValidatesIt); problem != nil {
		t.Fatal(problem)
	}
	if e := sendThrough(t, context.Background(), modules, destination, alert); e != nil {
		t.Fatal(e)
	}
	sent, e := os.ReadFile(request)
	if e != nil || len(sent) > checksandalerts.ModuleRequestLimit || len(sent) < len(encoded)+6*checksandalerts.ModuleFilesLimit+checksandalerts.ModuleInputsLimit {
		t.Fatalf("the request is %d bytes: %v", len(sent), e)
	}
	// The helper reads a request of that size.
	var out bytes.Buffer
	if e := checksandalerts.ModuleJSONHelper([]string{"request"}, bytes.NewReader(sent), &out); e != nil || !bytes.Equal(out.Bytes(), sent) {
		t.Fatal("the helper did not read the largest request:", e)
	}
	out.Reset()
	if e := checksandalerts.ModuleJSONHelper([]string{"get", "--type", "string", "/alert/title_with_alert_severity"}, bytes.NewReader(sent), &out); e != nil || out.String() != alert.TitleWithAlertSeverity {
		t.Fatal(out.String(), e)
	}

	// One more byte in the files, and the delivery and the validation refuse.
	inputs["e_file"] = writeModuleFile(t, "x")
	e = sendThrough(t, context.Background(), modules, destination, alert)
	if e == nil || e.Error() != "MMWatchdog cannot use the files of the module own_alarm: the files named in the destination's inputs total 65537 bytes, more than the limit of 65536 bytes for one destination" {
		t.Fatal(e)
	}
	own := testhelpers.OwnOptions(t)
	path := testhelpers.ConfigFile(t, "external_module_dir: "+modules+"\ncheck_groups: {a: {http: {url: 'https://example.com'}}}\n")
	files := ""
	for _, input := range checksandalerts.Names(inputs) {
		if strings.HasSuffix(input, checksandalerts.FileInputSuffix) {
			files += input + ": " + inputs[input].(string) + ", "
		}
	}
	os.WriteFile(filepath.Join(path, "alert_destinations.yml"), []byte("destinations:\n  on_call: {own_alarm: {"+files+"run_as_user_account: "+own.RunAsUserAccount+", run_from_directory: "+own.RunFromDirectory+"}, remind_every: never, alert_severities: [sos]}\n"), 0600)
	c, e := config.Load(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = ValidateRuntime(c); e == nil || !strings.Contains(e.Error(), "destinations.on_call.own_alarm: the files named in the destination's inputs total 65537 bytes, more than the limit of 65536 bytes for one destination") {
		t.Fatal(e)
	}
	// One more byte in the inputs, and validation refuses the destination.
	delete(inputs, "e_file")
	inputs["text"] = inputs["text"].(string) + "t"
	if problem := destination.Inputs.Validate(asLoadValidatesIt); problem == nil || problem.Path != "" || problem.Message != "the inputs are 262145 bytes as JSON, and the inputs of one destination are at most 262144 bytes" {
		t.Fatal(problem)
	}
	// A request that is too large all the same never reaches the module.
	inputs["text"] = strings.Repeat("t", checksandalerts.ModuleRequestLimit)
	e = sendThrough(t, context.Background(), modules, destination, alert)
	if e == nil || !strings.HasPrefix(e.Error(), "the request to the module own_alarm is ") || !strings.HasSuffix(e.Error(), " bytes, and a request is at most 1048576 bytes") {
		t.Fatal(e)
	}
}

// A module destination's identity is the module's name and its inputs,
// excluding credential paths and presentation settings. An input reads the same from the
// configuration and from a queued delivery, whatever its type.
func TestModuleDestinationIdentity(t *testing.T) {
	t.Parallel()
	base := func() checksandalerts.Destination {
		return checksandalerts.Destination{RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, TimeLimitPerDeliveryAttempt: "5s", Inputs: &checksandalerts.ModuleCall{Name: "telegram",
			Inputs:      map[string]any{"bot_token_file": "/etc/mmwatchdog/secrets/telegram-bot", "chat_id": "-1001234567890", "message_thread_id": 42, "larger_than_a_float_holds": int64(9007199254740993), "ratio": 1.5, "silent": true, "tags": []any{"a", 2}},
			ExecOptions: primitives.ExecOptions{RunAsUserAccount: "mmwatchdog", RunFromDirectory: "/"}}}
	}
	for name, change := range map[string]func(*checksandalerts.Destination){
		"removed input": func(d *checksandalerts.Destination) { delete(d.Inputs.(*checksandalerts.ModuleCall).Inputs, "silent") },
		"list in an input": func(d *checksandalerts.Destination) {
			d.Inputs.(*checksandalerts.ModuleCall).Inputs["tags"] = []any{"a", 3}
		},
		"module": func(d *checksandalerts.Destination) { d.Inputs.(*checksandalerts.ModuleCall).Name = "slack" },
		"input": func(d *checksandalerts.Destination) {
			d.Inputs.(*checksandalerts.ModuleCall).Inputs["chat_id"] = "-1009999999999"
		},
		"number": func(d *checksandalerts.Destination) {
			d.Inputs.(*checksandalerts.ModuleCall).Inputs["message_thread_id"] = 43
		},

		"core module": func(d *checksandalerts.Destination) { d.Inputs = &checksandalerts.Webhook{URL: "https://example.com"} },
	} {
		changed := base()
		change(&changed)
		if checksandalerts.SameDestination(base(), changed) || checksandalerts.SameDestination(changed, base()) {
			t.Error("the destination kept its identity after a change of its", name)
		}
	}
	for name, change := range map[string]func(*checksandalerts.Destination){
		"file path": func(d *checksandalerts.Destination) {
			d.Inputs.(*checksandalerts.ModuleCall).Inputs["bot_token_file"] = "/etc/mmwatchdog/secrets/telegram-bot-2"
		},
		"new input": func(d *checksandalerts.Destination) {
			d.Inputs.(*checksandalerts.ModuleCall).Inputs["mention"] = "@oncall"
		},

		"account": func(d *checksandalerts.Destination) {
			d.Inputs.(*checksandalerts.ModuleCall).RunAsUserAccount = "alerts"
		},
		"directory": func(d *checksandalerts.Destination) {
			d.Inputs.(*checksandalerts.ModuleCall).RunFromDirectory = "/var/lib/alerts"
		},
		"environment": func(d *checksandalerts.Destination) {
			d.Inputs.(*checksandalerts.ModuleCall).Env = map[string]string{"HTTPS_PROXY": "http://proxy:3128"}
		},
		"time limit":   func(d *checksandalerts.Destination) { d.TimeLimitPerDeliveryAttempt = "30s" },
		"reminders":    func(d *checksandalerts.Destination) { d.RemindEvery = "1m" },
		"title only":   func(d *checksandalerts.Destination) { d.SendOnlyTheAlertTitle = true },
		"severities":   func(d *checksandalerts.Destination) { d.AlertSeverities = []string{} },
		"nothing":      func(d *checksandalerts.Destination) {},
		"stored again": func(d *checksandalerts.Destination) { *d = storedAndReadBack(t, *d) },
	} {
		changed := base()
		change(&changed)
		if !checksandalerts.SameDestination(base(), changed) || !checksandalerts.SameDestination(changed, base()) {
			t.Error("the destination lost its identity after a change of its", name)
		}
	}
	none, empty := base(), base()
	none.Inputs.(*checksandalerts.ModuleCall).Inputs, empty.Inputs.(*checksandalerts.ModuleCall).Inputs = nil, map[string]any{}
	if !checksandalerts.SameDestination(none, empty) || !checksandalerts.SameDestination(storedAndReadBack(t, none), empty) {
		t.Error("a module without inputs differs from one with an empty inputs mapping")
	}
}

// storedAndReadBack is a destination as a queued delivery holds it: written
// as JSON and read again, so every number is a float.
func storedAndReadBack(t *testing.T, d checksandalerts.Destination) checksandalerts.Destination {
	t.Helper()
	b, e := json.Marshal(d)
	if e != nil {
		t.Fatal(e)
	}
	var stored checksandalerts.Destination
	if e = json.Unmarshal(b, &stored); e != nil {
		t.Fatal(e)
	}
	return stored
}

// The daemon delivers an alert to a module destination through the outbox.
// A queued delivery keeps its place through a commit, a reload, and a
// restart, also when the module's inputs hold numbers; send_only_the_alert_title
// gives the module the title and no body; and a module that reports failed
// is tried again with the same event ID.
func TestTheDaemonDeliversAlertsThroughAModule(t *testing.T) {
	t.Parallel()
	requests := t.TempDir()
	p, c := daemonConfig(t)
	// The module keeps every request in a file named for the alert's ID. It
	// fails the first attempt of each alert for each destination, and
	// delivers the next. The alert, inputs, and file contents of a retry
	// stay the same. The remaining time of each attempt can differ, so the
	// counter leaves mmwatchdog_ends_the_module_after_milliseconds out of its checksum.
	description := testhelpers.TestModuleDescription("chat", checksandalerts.UsedAsAlertDestination, map[string]string{"token_file": "string", "message_thread_id": "integer", "larger_than_a_float_holds": "integer", "ratio": "number", "channel": "string"}, nil)
	testhelpers.InstallModule(t, c.ExternalModuleDir, "chat", description, `cd `+testhelpers.ShellQuote(requests)+`
request=$(cat)
id=$(printf '%s' "$request" | sed 's/.*"alert":{"id":"\([0-9a-f]*\)".*/\1/')
attempts=attempts-$(printf '%s' "$request" | sed 's/,"mmwatchdog_ends_the_module_after_milliseconds":[0-9]*//' | cksum | cut -d' ' -f1)
printf '%s\n' "$request" >> "$id"
echo attempt >> "$attempts"
if [[ $(wc -l < "$attempts") -lt 2 ]]; then
  printf '{"protocol":1,"status":"failed","message":"the chat service answered 503"}'
else
  printf '{"protocol":1,"status":"delivered","message":"sent"}'
fi`)
	file := writeModuleFile(t, "a-secret-token-of-the-chat-service\n")
	full := moduleDestination(t, "chat", map[string]any{"token_file": file, "message_thread_id": 42, "larger_than_a_float_holds": int64(9007199254740993), "ratio": 1.5})
	short := moduleDestination(t, "chat", map[string]any{"token_file": file, "channel": "sms"})
	short.SendOnlyTheAlertTitle = true
	c.Alerts.Destinations = map[string]checksandalerts.Destination{"team_chat": full}
	writeConfig(t, p, c)
	d, e := NewDaemon(p)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	event := checksandalerts.Event{ID: incidents.ID(), CheckGroup: "app", Check: "shell", Type: "failed", AlertSeverity: "warn", Message: "exit status 1", At: time.Now().UTC(), Notify: true}
	if e = d.record([]checksandalerts.Event{event}); e != nil {
		t.Fatal(e)
	}
	// Nothing about the destination changed, so nothing is cancelled: not
	// by a commit, not by a reload, and not by a restart.
	d.mu.Lock()
	e = d.record(nil)
	d.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = d.reload(); e != nil {
		t.Fatal(e)
	}
	if e = d.Close(); e != nil {
		t.Fatal(e)
	}
	if d, e = NewDaemon(p); e != nil {
		t.Fatal(e)
	}
	events, e := d.store.Events(20)
	if e != nil {
		t.Fatal(e)
	}
	for _, recorded := range events {
		if recorded.Type == "delivery_cancelled" {
			t.Fatal("the queued delivery was cancelled although the destination is the same:", recorded.Message)
		}
	}
	if outboxCount(t, d, "team_chat") != 1 {
		t.Fatal("the queued delivery is gone")
	}
	// A second destination that takes only the title is added; it gets the
	// next alert without a body.
	c.Alerts.Destinations["on_call_sms"] = short
	writeConfig(t, p, c)
	if _, e = d.reload(); e != nil {
		t.Fatal(e)
	}
	second := checksandalerts.Event{ID: incidents.ID(), CheckGroup: "app", Check: "shell", Type: "failed", AlertSeverity: "warn", Message: "exit status 2", At: time.Now().UTC(), Notify: true}
	d.mu.Lock()
	e = d.record([]checksandalerts.Event{second})
	d.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	runWorker(t, d)
	for end := time.Now().Add(testhelpers.LongestWaitForAnEvent); outboxCount(t, d, "team_chat")+outboxCount(t, d, "on_call_sms") > 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			stats, _ := d.store.DeliveryStats()
			t.Fatalf("the alerts were not delivered: %+v", stats)
		}
	}
	type request struct {
		Alert        checksandalerts.Event `json:"alert"`
		Inputs       map[string]any        `json:"inputs"`
		FileContents map[string]string     `json:"file_contents"`
	}
	read := func(id string) []request {
		t.Helper()
		got := []request{}
		for _, line := range strings.Split(strings.TrimSpace(string(waitForFile(t, filepath.Join(requests, id)))), "\n") {
			var r request
			if e := json.Unmarshal([]byte(line), &r); e != nil {
				t.Fatal(e, line)
			}
			got = append(got, r)
		}
		return got
	}
	// The first alert went to team_chat twice: the failed attempt and its
	// retry. The second went to both destinations, twice each.
	first := read(event.ID)
	if len(first) != 2 || first[0].Alert.ID != event.ID || first[1].Alert.ID != event.ID || first[0].Alert.Title != d.store.Node+": app/shell failed" || first[0].Alert.Body == "" ||
		first[0].Inputs["message_thread_id"] != float64(42) || first[0].FileContents["token_file"] != "a-secret-token-of-the-chat-service" {
		t.Fatalf("the first alert: %+v", first)
	}
	titleOnly, withBody := 0, 0
	for _, r := range read(second.ID) {
		if r.Alert.Title != d.store.Node+": app/shell failed" {
			t.Fatal(r.Alert)
		}
		if r.Inputs["channel"] == "sms" {
			titleOnly++
			if r.Alert.Body != "" || r.Alert.Message != "" {
				t.Fatalf("the destination that takes only the title got more: %+v", r.Alert)
			}
		} else {
			withBody++
			if r.Alert.Body == "" {
				t.Fatalf("the destination that takes the body got none: %+v", r.Alert)
			}
		}
	}
	if titleOnly != 2 || withBody != 2 {
		t.Fatal("expected a failed attempt and a retry for each destination:", titleOnly, withBody)
	}
}

// The fleet settings carry, for each module destination, the content of
// every file that its inputs name. A copy with a missing, a surplus, or an
// invalid content is refused, for any mix of destination modules.
func TestFleetSettingsCarryModuleFileContents(t *testing.T) {
	t.Parallel()
	file := writeModuleFile(t, "https://hooks.slack.example/T000/B000/XXXX\n")
	hub, _, _ := startHubWithModules(t, map[string]checksandalerts.Destination{
		"team_chat": moduleDestination(t, "slack", map[string]any{"webhook_url_file": file, "mention": "@oncall"}),
		"own_alarm": moduleDestination(t, "own_alarm", map[string]any{}),
		"ops":       {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://example.com"}, TimeLimitPerDeliveryAttempt: "1s"},
	}, func(externalModuleDir string) {
		testhelpers.InstallModule(t, externalModuleDir, "slack", slackDescription, "true")
		testhelpers.InstallModule(t, externalModuleDir, "own_alarm", testhelpers.TestModuleDescription("own_alarm", checksandalerts.UsedAsAlertDestination, nil, nil), "true")
	})
	read := hub.hub.FleetSettings.Load()
	if !reflect.DeepEqual(read.FileContentsByDestination, map[string]map[string]string{"team_chat": {"webhook_url_file": "https://hooks.slack.example/T000/B000/XXXX"}}) {
		t.Fatalf("the fleet settings: %+v", read.FileContentsByDestination)
	}
	if e := read.Validate(hub.config.Modules); e != nil {
		t.Fatal(e)
	}
	// A rotated file changes the version, so joined nodes fetch it.
	os.WriteFile(file, []byte("https://hooks.slack.example/T000/B000/ROTATED\n"), 0600)
	rotated, e := fleet.NewFleetSettings(hub.config)
	if e != nil || rotated.Version == read.Version || rotated.FileContentsByDestination["team_chat"]["webhook_url_file"] != "https://hooks.slack.example/T000/B000/ROTATED" {
		t.Fatal(rotated, e)
	}
	os.Remove(file)
	if _, e = fleet.NewFleetSettings(hub.config); e == nil || !strings.Contains(e.Error(), "destinations.team_chat.slack.webhook_url_file: open "+file) {
		t.Fatal("a hub with an unreadable module file built its settings:", e)
	}

	module := checksandalerts.Destination{RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, TimeLimitPerDeliveryAttempt: "5s", Inputs: &checksandalerts.ModuleCall{Name: "slack", Inputs: map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/slack", "mention": "@oncall"}}}
	bare := checksandalerts.Destination{RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, TimeLimitPerDeliveryAttempt: "5s", Inputs: &checksandalerts.ModuleCall{Name: "own_alarm"}}
	webhook := checksandalerts.Destination{RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, TimeLimitPerDeliveryAttempt: "5s", Inputs: &checksandalerts.Webhook{URL: "https://example.com", BearerTokenFile: "/etc/mmwatchdog/secrets/webhook"}}
	email := emailDestination("smtp.example.com:587", checksandalerts.SMTPStartTLS)
	manyFiles := checksandalerts.Destination{RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, TimeLimitPerDeliveryAttempt: "5s", Inputs: &checksandalerts.ModuleCall{Name: "own_alarm", Inputs: map[string]any{"a_file": "/a", "b_file": "/b", "c_file": "/c", "d_file": "/d", "e_file": "/e"}}}
	largest := strings.Repeat("k", checksandalerts.ModuleFileLimit)
	type contents = map[string]map[string]string
	for _, tc := range []struct {
		name         string
		destinations map[string]checksandalerts.Destination
		files        contents
		want         string
	}{
		{"every file content", map[string]checksandalerts.Destination{"team_chat": module, "own_alarm": bare}, contents{"team_chat": {"webhook_url_file": "https://hooks.slack.example/x"}}, ""},
		{"an empty file", map[string]checksandalerts.Destination{"team_chat": module}, contents{"team_chat": {"webhook_url_file": ""}}, ""},
		{"a module without files, with its empty entry", map[string]checksandalerts.Destination{"own_alarm": bare}, contents{"own_alarm": {}}, ""},
		{"a module beside every core module", map[string]checksandalerts.Destination{"team_chat": module, "ops": webhook, "mailbox": email}, contents{"team_chat": {"webhook_url_file": "x"}, "ops": {"bearer_token_file": "0123456789abcdef"}}, ""},
		{"no file content at all", map[string]checksandalerts.Destination{"team_chat": module}, contents{}, `fleet settings lack credential file contents for destination "team_chat", input webhook_url_file`},
		{"a missing file content", map[string]checksandalerts.Destination{"team_chat": module}, contents{"team_chat": {}}, `fleet settings lack credential file contents for destination "team_chat", input webhook_url_file`},
		{"the content of an input that is no file", map[string]checksandalerts.Destination{"team_chat": module}, contents{"team_chat": {"webhook_url_file": "x", "mention": "y"}}, `fleet settings contain credential file contents not named by the module inputs of destination "team_chat"`},
		{"a content for a module without files", map[string]checksandalerts.Destination{"own_alarm": bare}, contents{"own_alarm": {"token_file": "x"}}, `fleet settings contain credential file contents not named by the module inputs of destination "own_alarm"`},
		{"a content under another name than the input's", map[string]checksandalerts.Destination{"ops": webhook}, contents{"ops": {"token_file": "0123456789abcdef"}}, `fleet settings lack credential file contents for destination "ops", input bearer_token_file`},
		{"a content for no destination", map[string]checksandalerts.Destination{"team_chat": module}, contents{"team_chat": {"webhook_url_file": "x"}, "gone": {}}, `fleet settings contain credential file contents for unknown hub destination "gone"`},
		{"a content that is too large", map[string]checksandalerts.Destination{"team_chat": module}, contents{"team_chat": {"webhook_url_file": strings.Repeat("k", checksandalerts.ModuleFileLimit+1)}}, `fleet settings contain invalid credential file contents for destination "team_chat", input webhook_url_file`},
		{"contents that are too large together", map[string]checksandalerts.Destination{"own_alarm": manyFiles}, contents{"own_alarm": {"a_file": largest, "b_file": largest, "c_file": largest, "d_file": largest, "e_file": "x"}}, `fleet settings contain credential file contents for destination "own_alarm" whose combined size exceeds the limit`},
		{"contents of the largest size together", map[string]checksandalerts.Destination{"own_alarm": manyFiles}, contents{"own_alarm": {"a_file": largest, "b_file": largest, "c_file": largest, "d_file": largest, "e_file": ""}}, ""},
		{"a content that is not UTF-8", map[string]checksandalerts.Destination{"team_chat": module}, contents{"team_chat": {"webhook_url_file": "not \xff UTF-8"}}, `fleet settings contain invalid credential file contents for destination "team_chat", input webhook_url_file`},
		{"a content with a NUL", map[string]checksandalerts.Destination{"team_chat": module}, contents{"team_chat": {"webhook_url_file": "a\x00b"}}, `fleet settings contain invalid credential file contents for destination "team_chat", input webhook_url_file`},
		{"a webhook token that no token file can hold", map[string]checksandalerts.Destination{"ops": webhook}, contents{"ops": {"bearer_token_file": ""}}, `fleet settings contain invalid credential file contents for destination "ops", input bearer_token_file`},
		{"a webhook without its token beside a module", map[string]checksandalerts.Destination{"team_chat": module, "ops": webhook}, contents{"team_chat": {"webhook_url_file": "x"}}, `fleet settings lack credential file contents for destination "ops", input bearer_token_file`},
		{"no map of file contents", map[string]checksandalerts.Destination{"own_alarm": bare}, nil, `fleet settings are incomplete or the version does not match the contents`},
	} {
		// The joined node that checks the copy has none of the modules: the
		// inputs of the hub's destinations were the hub's to check.
		s := &fleet.FleetSettings{Destinations: tc.destinations, FileContentsByDestination: tc.files, Fleet: config.DefaultFleetTimings, AlertSeverityWhenTheHubGoesSilent: "sos"}
		s.Version = s.ComputeVersion()
		e := s.Validate(nil)
		if tc.want == "" && e != nil || tc.want != "" && (e == nil || e.Error() != tc.want) {
			t.Errorf("%s: got %v, want %q", tc.name, e, tc.want)
		}
	}
}

// A joined node that delivers an alert without the hub runs the module that
// is installed on the node itself, with the inputs and the file contents
// from its copy of the fleet settings: the hub's files are not on the node.
// On a node without the module the attempt fails and names the module. The
// node finds a module that a deploy installed when MMWatchdog reads its
// configuration again.
func TestAJoinedNodeRunsItsOwnModuleWithoutTheHub(t *testing.T) {
	t.Parallel()
	file := writeModuleFile(t, "https://hooks.slack.example/T000/B000/XXXX\n")
	_, code, srv := startHubWithModules(t, map[string]checksandalerts.Destination{"team_chat": moduleDestination(t, "slack", map[string]any{"webhook_url_file": file, "mention": "@oncall"})}, func(externalModuleDir string) {
		testhelpers.InstallModule(t, externalModuleDir, "slack", slackDescription, "true")
	})
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	if node.fleetSettings == nil || node.fleetSettings.FileContentsByDestination["team_chat"]["webhook_url_file"] != "https://hooks.slack.example/T000/B000/XXXX" {
		t.Fatal("the settings copy lacks the content of the module's file", node.fleetSettings)
	}
	node.fleetSettings.Fleet.SendAlertWithoutHubAfterWaitingFor = "300ms" // below what configuration allows
	srv.Close()
	os.Remove(file)
	request := filepath.Join(t.TempDir(), "request.json")
	setMaintenance(t, node, "app", "10m")
	runWorker(t, node)
	// The node's status, which is also what it reports to the hub, names
	// the destination and the module that is missing.
	missing := "the module slack is not installed on this node"
	for end := time.Now().Add(testhelpers.LongestWaitForAnEvent); ; time.Sleep(20 * time.Millisecond) {
		node.mu.Lock()
		_, delivery, _ := node.status("", time.Now().UTC(), nil)
		node.mu.Unlock()
		if failing := delivery.FailingDestinations; len(failing) == 1 && failing[0].Destination == "team_chat" && failing[0].LatestAttemptError == missing && failing[0].UndeliveredAlerts == 1 {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("the node without the module did not say so: %+v", delivery)
		}
	}
	// The node's own module is a Starlark script here and an executable on
	// the hub: the fleet settings name the module, not its kind.
	testhelpers.InstallStarlarkModule(t, node.config.ExternalModuleDir, "slack", slackDescription, `def deliver_alert(alert, inputs, file_contents):
    written = run_command(["/bin/sh", "-c", "cat > `+request+`.new && mv `+request+`.new `+request+`"], standard_input = json.encode({"alert": alert, "inputs": inputs, "file_contents": file_contents}))
    if written.error or written.exit_code != 0:
        return {"status": "failed", "message": "the request was not written: " + written.error + written.standard_error}
    return {"status": "delivered", "message": "Slack accepted the message"}
`)
	if _, e := node.reload(); e != nil {
		t.Fatal(e)
	}
	var got struct {
		Alert        checksandalerts.Event `json:"alert"`
		Inputs       map[string]any        `json:"inputs"`
		FileContents map[string]string     `json:"file_contents"`
	}
	if e := json.Unmarshal(waitForFile(t, request), &got); e != nil {
		t.Fatal(e)
	}
	if got.Alert.Title != "app-1: maintenance on for app" || got.Alert.Node != "app-1" || got.Inputs["mention"] != "@oncall" || got.Inputs["webhook_url_file"] != file ||
		!reflect.DeepEqual(got.FileContents, map[string]string{"webhook_url_file": "https://hooks.slack.example/T000/B000/XXXX"}) {
		t.Fatalf("the request on the joined node: %+v", got)
	}
}

// alarmSystemScript is the destination module of the project's own that the
// design shows: it posts each alert as JSON to the team's own alarm system,
// with a token that MMWatchdog read from a file.
const alarmSystemScript = `def deliver_alert(alert, inputs, file_contents):
    response = http_request(
        method = "POST",
        url = inputs["alarm_system_url"],
        headers = {
            "Content-Type": "application/json",
            "Authorization": "Bearer " + file_contents["api_token_file"],
        },
        body = json.encode({
            "title": alert["title"],
            "alert_severity": alert["alert_severity"],
            "body": alert.get("body", ""),
        }),
    )
    if response.error:
        return {"status": "failed", "message": "MMWatchdog could not reach the alarm system: " + response.error}
    if response.http_status_code // 100 != 2:
        return {"status": "failed", "message": "the alarm system answered HTTP %d" % response.http_status_code}
    return {"status": "delivered", "message": "the alarm system accepted the alert"}
`

const alarmSystemDescription = `summary: Posts each alert as JSON to the team's own alarm system, with a token.
used_as: alert_destination
inputs:
  alarm_system_url: {type: string, required: true, description: The address of the alarm system that takes the alerts.}
  api_token_file: {type: string, required: true, description: The file that holds the token of the alarm system.}
examples:
  - description: Send every sos alert to the alarm system.
    config: |
      alarm_system:
        alarm_system_url: https://alarm.example.com/alerts
        api_token_file: /etc/mmwatchdog/secrets/alarm-system-token
      remind_every: never
      alert_severities: [sos]
`

// A Starlark script is a destination module: MMWatchdog calls its deliver_alert
// with the alert, the destination's inputs, and the content of the files
// that the inputs name, and the dictionary that it returns is the answer.
// Anything else is a failed attempt: a failed answer, a script that ends
// with an error, a script without the function, and another result. A
// credential that a script repeats does not reach the error of the attempt.
func TestAStarlarkScriptDeliversAnAlert(t *testing.T) {
	t.Parallel()
	type posted struct {
		authorization, contentType string
		body                       map[string]any
	}
	var mu sync.Mutex
	got, answer := []posted{}, http.StatusAccepted
	alarms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := posted{authorization: r.Header.Get("Authorization"), contentType: r.Header.Get("Content-Type")}
		json.NewDecoder(r.Body).Decode(&request.body)
		mu.Lock()
		got = append(got, request)
		status := answer
		mu.Unlock()
		w.WriteHeader(status)
	}))
	defer alarms.Close()
	modules := t.TempDir()
	testhelpers.InstallStarlarkModule(t, modules, "alarm_system", alarmSystemDescription, alarmSystemScript)
	token := "the-token-of-the-alarm-system"
	destination := moduleDestination(t, "alarm_system", map[string]any{"alarm_system_url": alarms.URL + "/alerts", "api_token_file": writeModuleFile(t, token+"\n")})
	if problem := destination.Inputs.Validate(checksandalerts.InputValidation{UsedAs: checksandalerts.UsedAsAlertDestination, Modules: testhelpers.ModulesIn(t, modules)}); problem != nil {
		t.Fatal(problem)
	}
	if e := sendThrough(t, context.Background(), modules, destination, moduleAlert); e != nil {
		t.Fatal(e)
	}
	// A destination that takes only the title gets an alert without a body.
	if e := sendThrough(t, context.Background(), modules, destination, moduleAlert.OnlyTheTitle()); e != nil {
		t.Fatal(e)
	}
	want := []posted{
		{"Bearer " + token, "application/json", map[string]any{"title": "app-1: myapp/http failed", "alert_severity": "sos", "body": "expected 200, received 503\nfailing since 14:02 UTC"}},
		{"Bearer " + token, "application/json", map[string]any{"title": "app-1: myapp/http failed", "alert_severity": "sos", "body": ""}},
	}
	mu.Lock()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the alarm system got %+v", got)
	}
	answer = http.StatusServiceUnavailable
	mu.Unlock()
	if e := sendThrough(t, context.Background(), modules, destination, moduleAlert); e == nil || e.Error() != "the module alarm_system reported: the alarm system answered HTTP 503" {
		t.Fatal(e)
	}
	// The error of a request that reaches nobody holds no address.
	alarms.Close()
	if e := sendThrough(t, context.Background(), modules, destination, moduleAlert); e == nil || !strings.HasPrefix(e.Error(), "the module alarm_system reported: MMWatchdog could not reach the alarm system: ") || strings.Contains(e.Error(), "/alerts") || strings.Contains(e.Error(), token) {
		t.Fatal(e)
	}
	script := filepath.Join(modules, "alarm_system", checksandalerts.ModuleScriptFile)
	ended := "the script of the module alarm_system ended with an error: " + script
	for _, tc := range []struct{ name, script, want string }{
		{"a script that repeats the token in its message", "def deliver_alert(alert, inputs, file_contents):\n    return {\"status\": \"failed\", \"message\": \"the alarm system refused \" + file_contents[\"api_token_file\"]}\n", "the module alarm_system reported: the alarm system refused [content of api_token_file]"},
		{"a script that repeats the token in an error", "def deliver_alert(alert, inputs, file_contents):\n    fail(\"cannot send with \" + file_contents[\"api_token_file\"])\n", ended + ":2: fail: cannot send with [content of api_token_file]"},
		{"an error in the script", "def deliver_alert(alert, inputs, file_contents):\n    return {\"status\": \"delivered\", \"message\": alert[\"summary\"]}\n", ended + `:2: key "summary" not in dict`},
		{"no function deliver_alert", testhelpers.ScriptOfACheck, ended + " defines no function deliver_alert(alert, inputs, file_contents)"},
		{"a result that is no dictionary", "def deliver_alert(alert, inputs, file_contents):\n    return \"delivered\"\n", ended + ": deliver_alert returned string, and it must return a dictionary with a status and a message"},
		{"the status of a check", "def deliver_alert(alert, inputs, file_contents):\n    return {\"status\": \"ok\", \"message\": \"sent\"}\n", `the module alarm_system answered with the status "ok", and a destination module answers delivered or failed`},
		{"delivered without a message", "def deliver_alert(alert, inputs, file_contents):\n    return {\"status\": \"delivered\"}\n", "the module alarm_system answered delivered without a message, and a message is required"},
		{"a member that the protocol does not have", "def deliver_alert(alert, inputs, file_contents):\n    return {\"status\": \"delivered\", \"message\": \"sent\", \"measurements\": {}}\n", `the module alarm_system did not answer with one JSON object of protocol, status, and message: json: unknown field "measurements"`},
	} {
		testhelpers.InstallStarlarkModule(t, modules, "alarm_system", alarmSystemDescription, tc.script)
		e := sendThrough(t, context.Background(), modules, destination, moduleAlert)
		if e == nil || e.Error() != tc.want || strings.Contains(e.Error(), token) {
			t.Errorf("%s:\n got %v\nwant %s", tc.name, e, tc.want)
		}
	}
}

// A module must never repeat a credential in its answer. If one does, MMWatchdog
// puts the name of the input in the place of the file's content, also when
// the module took the white space off the ends of the content first.
// Short secrets are removed too.
func TestAnErrorHoldsNoContentOfAFile(t *testing.T) {
	t.Parallel()
	contents := map[string]string{"api_token_file": "  SECRET-token-123456\n\n", "bot_token_file": "123456:AAE-x1y2z3", "region_file": "eu-1"}
	for said, want := range map[string]string{
		"the token   SECRET-token-123456\n\n was refused": "the token [content of api_token_file] was refused",
		"the token SECRET-token-123456 was refused":       "the token [content of api_token_file] was refused",
		"POST /bot123456:AAE-x1y2z3/sendMessage: 401":     "POST /bot[content of bot_token_file]/sendMessage: 401",
		"the region eu-1 answered 503":                    "the region [content of region_file] answered 503",
		"SECRET-token-12345 is another text":              "SECRET-token-12345 is another text",
	} {
		if got := checksandalerts.WithoutFileContents(said, contents); got != want {
			t.Errorf("%q:\n got %q\nwant %q", said, got, want)
		}
	}
}

// installTokenModule installs a destination module with one credential file
// input, api_token_file: a module that is not core, so its credential counts
// in the identity of its destination, and returns a destination that uses it.
func installTokenModule(t *testing.T, c *config.Config, tokenFile string) checksandalerts.Destination {
	t.Helper()
	testhelpers.InstallStarlarkModule(t, c.ExternalModuleDir, "token_module", testhelpers.TestModuleDescription("token_module", checksandalerts.UsedAsAlertDestination, map[string]string{"api_token_file": "string"}, nil), `def deliver_alert(alert, inputs, file_contents):
    return {"status": "delivered", "message": "accepted"}
`)
	return moduleDestination(t, "token_module", map[string]any{"api_token_file": tokenFile})
}
