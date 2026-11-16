package official_extra_modules

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
)

// mattermostKey is the key at the end of the address of a Mattermost
// incoming webhook: the secret part of the address.
const mattermostKey = "9xuqwrwgstrb3mzrxb83nb357a"

// mattermostText reads a request to an incoming webhook of Mattermost.
func mattermostText(t *testing.T, request received, path string) (text string, payload map[string]any) {
	t.Helper()
	if request.method != "POST" || request.contentType != "application/json" || request.path != path {
		t.Fatalf("not a JSON POST to %s: %+v", path, request)
	}
	if e := json.Unmarshal(request.body, &payload); e != nil {
		t.Fatal(e, string(request.body))
	}
	text, _ = payload["text"].(string)
	return text, payload
}

// mattermostGeneralRefusal is how recent versions of Mattermost answer every
// refusal of a webhook: one id and one message for every reason, a message
// that names the webhook by its key, and the reason only in the status code.
func mattermostGeneralRefusal(status int) string {
	return fmt.Sprintf(`{"id":"web.incoming_webhook.general.app_error","message":"Failed to handle the payload of media type application/json for incoming webhook %s.","detailed_error":"","request_id":"t1zbxk3ofbbg9dxeyfjwkn4cdr","status_code":%d}`, mattermostKey, status)
}

// The mattermost module posts one text to the address in its file, for
// every kind of alert, and tells MMWatchdog that Mattermost took it: the title with
// the alert's severity in front as MMWatchdog wrote it, after the mention if there
// is one, and below it the body in a code block, which Mattermost shows as
// it is.
func TestMattermostSendsTheTitleAndTheBody(t *testing.T) {
	t.Parallel()
	mattermost := startService(t, func(received) (int, string) { return 200, "ok" })
	webhook := mattermost.URL + "/hooks/" + mattermostKey
	recovery := failure
	recovery.Type, recovery.Title, recovery.TitleWithAlertSeverity, recovery.Body = "recovered", "app-1: myapp/http is ok again", "Resolved SOS: app-1: myapp/http is ok again", "HTTP 200"
	reminder := failure
	reminder.Type, reminder.Title, reminder.TitleWithAlertSeverity, reminder.Body = "reminder", "app-1: myapp/http still failing", "SOS: app-1: myapp/http still failing", "myapp/http still failing since 14:02 UTC; latest result: expected HTTP 200, received 503"
	grouped := failure
	grouped.Node, grouped.Nodes, grouped.NodeCount, grouped.Title, grouped.TitleWithAlertSeverity = "", []string{"app-1", "app-2"}, 2, "myapp/http failed on 2 nodes", "SOS: myapp/http failed on 2 nodes"
	grouped.Body = "myapp/http failed on 2 nodes: app-1, app-2\nfrom app-1:\n  expected HTTP 200, received 503\n  failing since 14:02 UTC"
	notice := failure
	notice.Type, notice.AlertSeverity, notice.Incident, notice.CheckGroup, notice.Check = "notice", "info", "", "", ""
	notice.Title, notice.TitleWithAlertSeverity, notice.Body = "app-1: Test of the team chat. Nothing is wrong.", "Info: app-1: Test of the team chat. Nothing is wrong.", "sent by Barbara"
	onlyTheTitle := failure
	onlyTheTitle.Body, onlyTheTitle.Message = "", ""
	odd := failure
	odd.Title, odd.TitleWithAlertSeverity, odd.Body = "app-1: "+awkward[:22], "SOS: app-1: "+awkward[:22], awkward
	// The first line is Markdown, which Mattermost formats: a backslash
	// keeps each formatting character a character, an invisible space after
	// an @ keeps it from mentioning, and <\! keeps Mattermost from turning
	// <!here> into a mention. An @ inside a word, as in a template unit, and
	// _ between two letters, as in a check's name, stay as they are.
	inTitle := failure
	inTitle.Type, inTitle.AlertSeverity, inTitle.Body = "notice", "info", "HTTP 200"
	inTitle.TitleWithAlertSeverity = `Info: app-1: restarted *all* _workers_ ~now~ ` + "`x`" + ` [y](z) <b> & C:\temp @channel @all (@barbara) <!here> getty@tty1.service mail_queue`
	// A backslash before an ASCII letter or digit escapes nothing, and stays
	// one backslash, so that a name such as a mount unit's, where systemd
	// writes - as \x2d, can be copied from the post. Every other backslash
	// gets a backslash.
	unit := failure
	unit.Finding = `mnt-data\x2d1.mount`
	unit.Title = `app-1: failed_systemd_units/machine_has_no_failed_systemd_unit: mnt-data\x2d1.mount failed`
	unit.TitleWithAlertSeverity, unit.Body = "SOS: "+unit.Title, ""
	backslashes := failure
	backslashes.Type, backslashes.AlertSeverity, backslashes.Body = "notice", "info", ""
	backslashes.TitleWithAlertSeverity = `Info: app-1: a\*b \ \_x \@all \é \x2d end\`
	// Mattermost turns Slack's <address|text> into a link itself; the module
	// sends it as it is.
	link := failure
	link.Type, link.AlertSeverity, link.Body = "notice", "info", ""
	link.TitleWithAlertSeverity = "Info: app-1: deploy done, see <https://example.com/deploys/7|the deploy>"
	// A body with rows of backticks gets a longer fence, so that no line of
	// the body ends the code block, and the body arrives as it is.
	fenced := failure
	fenced.Body = "the output was:\n" + codeBlock + "\nstarted `myapp` with ``two``\n" + codeBlock + codeBlock + "\n" + codeBlock
	// Nothing in the code block mentions anybody, so a mention in the output
	// of a check stays as it is.
	mentions := failure
	mentions.Body = "@channel @all @here @barbara <!channel> ~town-square #tag :smile: https://example.com"
	for _, tc := range []struct {
		name   string
		alert  checksandalerts.Event
		inputs map[string]any
		want   string
	}{
		{"a failure", failure, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/mattermost"}, "SOS: app-1: myapp/http failed\n" + codeBlock + "\nexpected HTTP 200, received 503\nfailing since 14:02 UTC\n" + codeBlock},
		{"a failure with a mention", failure, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/mattermost", "mention": "@barbara"}, "@barbara SOS: app-1: myapp/http failed\n" + codeBlock + "\nexpected HTTP 200, received 503\nfailing since 14:02 UTC\n" + codeBlock},
		{"a recovery", recovery, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/mattermost"}, "Resolved SOS: app-1: myapp/http is ok again\n" + codeBlock + "\nHTTP 200\n" + codeBlock},
		{"a reminder", reminder, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/mattermost"}, "SOS: app-1: myapp/http still failing\n" + codeBlock + "\nmyapp/http still failing since 14:02 UTC; latest result: expected HTTP 200, received 503\n" + codeBlock},
		{"a grouped message of the hub", grouped, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/mattermost"}, "SOS: myapp/http failed on 2 nodes\n" + codeBlock + "\n" + grouped.Body + "\n" + codeBlock},
		{"a notice", notice, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/mattermost", "mention": "@channel"}, "@channel Info: app-1: Test of the team chat. Nothing is wrong.\n" + codeBlock + "\nsent by Barbara\n" + codeBlock},
		// Without a body there is no code block.
		{"only the title", onlyTheTitle, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/mattermost", "mention": "@here"}, "@here SOS: app-1: myapp/http failed"},
		{"awkward text", odd, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/mattermost"}, "SOS: app-1: it said \"no\" & 'never'\n" + codeBlock + "\n" + awkward + "\n" + codeBlock},
		{"formatting characters in the title", inTitle, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/mattermost", "mention": "@sre-team"},
			`@sre-team Info: app-1: restarted \*all\* \_workers\_ \~now\~ ` + "\\`x\\`" + ` \[y\](z) <b> & C:\temp @` + zeroWidthSpace + `channel @` + zeroWidthSpace + `all (@` + zeroWidthSpace + `barbara) <\!here> getty@tty1.service mail_queue` + "\n" + codeBlock + "\nHTTP 200\n" + codeBlock},
		{"a backslash before a letter", unit, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/mattermost"}, unit.TitleWithAlertSeverity},
		{"backslashes before other characters", backslashes, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/mattermost"},
			`Info: app-1: a\\\*b \\ \\\_x \\@` + zeroWidthSpace + `all \\é \x2d end\\`},
		{"Slack's notation of a link in the title", link, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/mattermost"}, link.TitleWithAlertSeverity},
		{"rows of backticks in the body", fenced, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/mattermost"}, "SOS: app-1: myapp/http failed\n```````\n" + fenced.Body + "\n```````"},
		{"mentions in the body", mentions, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/mattermost"}, "SOS: app-1: myapp/http failed\n" + codeBlock + "\n" + mentions.Body + "\n" + codeBlock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(mattermost.got())
			o := run(t, "mattermost", tc.alert, tc.inputs, map[string]string{"webhook_url_file": webhook})
			if o.exitStatus != 0 || o.status != "delivered" || o.message != "Mattermost accepted the message" || o.stderr != "" {
				t.Fatalf("%+v", o)
			}
			requests := mattermost.got()[before:]
			if len(requests) != 1 {
				t.Fatalf("%+v", requests)
			}
			text, payload := mattermostText(t, requests[0], "/hooks/"+mattermostKey)
			// The message states nothing but its text, so the webhook's own
			// channel, name, and picture stay.
			if text != tc.want || len(payload) != 1 {
				t.Fatalf("got  %q\nwant %q\npayload %v", text, tc.want, payload)
			}
			if o.holds(mattermostKey) {
				t.Fatalf("the webhook's key is in what the module wrote: %+v", o)
			}
		})
	}
	// Mattermost can run below a path of a web server, and the address of a
	// webhook then has that path in front of /hooks/.
	t.Run("a server below a path", func(t *testing.T) {
		before := len(mattermost.got())
		o := run(t, "mattermost", failure, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/mattermost"}, map[string]string{"webhook_url_file": mattermost.URL + "/chat/hooks/" + mattermostKey})
		if o.status != "delivered" || len(mattermost.got()) != before+1 {
			t.Fatalf("%+v", o)
		}
		mattermostText(t, mattermost.got()[before], "/chat/hooks/"+mattermostKey)
	})
}

// A message of more than 16,383 characters is cut to 16,383, so that
// Mattermost keeps it in one post: the body is cut between two characters,
// the cut is marked, and the first line and the end of the code block always
// arrive.
func TestMattermostCutsALongText(t *testing.T) {
	t.Parallel()
	mattermost := startService(t, func(received) (int, string) { return 200, "ok" })
	webhook := mattermost.URL + "/hooks/" + mattermostKey
	for _, tc := range []struct {
		name, body, mention, firstLine, fence string
		cut                                   bool
	}{
		{"characters of several bytes", strings.Repeat("ü🐒& ", 5000), "@here", "@here SOS: app-1: myapp/http failed", codeBlock, true},
		{"a body that fits exactly", strings.Repeat("x", 16383-len("SOS: app-1: myapp/http failed")-9), "", "SOS: app-1: myapp/http failed", codeBlock, false},
		// The longer fence counts, too.
		{"a long row of backticks", strings.Repeat("`", 10) + strings.Repeat("x", 20000), "", "SOS: app-1: myapp/http failed", strings.Repeat("`", 11), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(mattermost.got())
			long := failure
			long.Body = tc.body
			inputs := map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/mattermost"}
			if tc.mention != "" {
				inputs["mention"] = tc.mention
			}
			if o := run(t, "mattermost", long, inputs, map[string]string{"webhook_url_file": webhook}); o.status != "delivered" {
				t.Fatalf("%+v", o)
			}
			text, _ := mattermostText(t, mattermost.got()[before], "/hooks/"+mattermostKey)
			start, end := tc.firstLine+"\n"+tc.fence+"\n", "\n"+tc.fence
			sent := strings.TrimSuffix(strings.TrimPrefix(text, start), end)
			if !utf8.ValidString(text) || utf8.RuneCountInString(text) != 16383 || !strings.HasPrefix(text, start) || !strings.HasSuffix(text, end) ||
				strings.HasSuffix(sent, "…") != tc.cut || !strings.HasPrefix(tc.body, strings.TrimSuffix(sent, "…")) {
				t.Fatalf("%d characters, ends with %q", utf8.RuneCountInString(text), text[len(text)-24:])
			}
		})
	}
}

// Every answer of Mattermost other than an accepted message is a failed
// attempt with a message that a person can act on, and no failure shows the
// address of the webhook or its key, which Mattermost's own answer holds.
// Each message fits in the 300 bytes that MMWatchdog keeps of it, so its advice
// arrives whole.
func TestMattermostFailures(t *testing.T) {
	t.Parallel()
	const unknownWebhook = " Mattermost does not know this webhook: it was deleted, or its channel was archived. Make a new incoming webhook and write its address into the file that webhook_url_file names."
	const general = "web.incoming_webhook.general.app_error"
	const notMattermost = " without the answer of Mattermost. Check the address in the file that webhook_url_file names, and what stands between this node and Mattermost."
	const checkTheAddress = ". Check the address in the file that webhook_url_file names, and what stands between this node and Mattermost."
	// The whole message has exactly the 300 bytes that MMWatchdog keeps.
	longWords := "the server at the webhook's address answered 502 without the answer of Mattermost: " + strings.Repeat("x", 300-len("the server at the webhook's address answered 502 without the answer of Mattermost: ")-len("…")-len(checkTheAddress))
	for _, tc := range []struct {
		name       string
		status     int
		retryAfter string
		body       string
		want       string
	}{
		// A real Mattermost server, Team Edition 11.11.1, answered a post to a
		// deleted webhook in this way in the services suite
		// (tests/integration/mattermost_scenarios.py), with the webhook's key
		// in its message.
		{"a deleted webhook", 400, "", mattermostGeneralRefusal(400), "Mattermost answered 400: " + general + "." + unknownWebhook},
		{"an older Mattermost, which names the reason", 400, "", `{"id":"web.incoming_webhook.invalid.app_error","message":"Invalid webhook.","detailed_error":"","request_id":"t1zbxk3ofbbg9dxeyfjwkn4cdr","status_code":400}`,
			"Mattermost answered 400: web.incoming_webhook.invalid.app_error." + unknownWebhook},
		// In developer mode Mattermost adds a detailed error, which can name
		// the key again.
		{"a detailed error in developer mode", 400, "", `{"id":"web.incoming_webhook.general.app_error","message":"Failed to handle the payload of media type application/json for incoming webhook ` + mattermostKey + `.","detailed_error":"HandleIncomingWebhook: Invalid webhook., resource \"IncomingWebhook\" not found, id: ` + mattermostKey + `","status_code":400}`,
			"Mattermost answered 400: " + general + "." + unknownWebhook},
		{"a channel that is gone", 404, "", mattermostGeneralRefusal(404), "Mattermost answered 404: " + general + "." + unknownWebhook},
		{"incoming webhooks turned off", 501, "", mattermostGeneralRefusal(501),
			"Mattermost answered 501: " + general + ". An administrator of Mattermost has turned off incoming webhooks. Ask them to turn on Enable Incoming Webhooks in the System Console, under Integrations > Integration Management."},
		{"a channel that the webhook may not post to", 403, "", mattermostGeneralRefusal(403),
			"Mattermost answered 403: " + general + ". Mattermost does not let this webhook post to its channel: the account that made it may have left the channel or its team. Make a new incoming webhook as a member of the channel."},
		{"a message too large", 413, "", `{"id":"api.context.request_body_too_large.app_error","message":"The request body is too large.","detailed_error":"","status_code":413}`,
			"Mattermost answered 413: api.context.request_body_too_large.app_error. The message is larger than Mattermost, or a proxy in front of it, takes."},
		{"an error of Mattermost", 500, "", mattermostGeneralRefusal(500), "Mattermost answered 500: " + general + "."},
		// In hardened mode Mattermost answers an error with an empty id.
		{"an error in hardened mode", 500, "", `{"id":"","message":"Internal Server Error","detailed_error":"","status_code":500}`, "Mattermost answered 500: Internal Server Error."},
		// The words of a server get only the bytes that the advice leaves of the
		// 300, and the key is replaced before they are cut.
		{"a message that names the key, without an id", 400, "", `{"id":"","message":"Failed to handle the payload of media type application/json for incoming webhook ` + mattermostKey + `.","status_code":400}`,
			"Mattermost answered 400: Failed to handle the payload of media type application/json for incoming webhook [the webhook'…." + unknownWebhook},
		{"a rate limit", 429, "3", "limit exceeded\n", "Mattermost answered 429: limit exceeded. Mattermost takes no more requests from this node for 3 seconds."},
		{"a rate limit of one second", 429, "1", "limit exceeded\n", "Mattermost answered 429: limit exceeded. Mattermost takes no more requests from this node for 1 second."},
		{"a rate limit without a wait", 429, "", "", "Mattermost answered 429: no text. Mattermost takes no more requests from this node for a short time."},
		{"a rate limit whose text names the key", 429, "", "limit exceeded for incoming webhook " + mattermostKey + "\n", "Mattermost answered 429: limit exceeded for incoming webhook [the webhook's key]. Mattermost takes no more requests from this node for a short time."},
		{"a page of another server", 502, "", "<html>\n<body>Bad Gateway</body>\n</html>", "the server at the webhook's address answered 502" + notMattermost},
		{"another server that answers 200", 200, "", "<html><body>Sign in</body></html>", "the server at the webhook's address answered 200" + notMattermost},
		{"JSON of another server", 400, "", `{"error":"bad request","message":{"text":"no"}}`, "the server at the webhook's address answered 400" + notMattermost},
		// Only an answer with an id and a status code is Mattermost's.
		{"a gateway that refuses", 403, "", `{"message":"Forbidden"}`, "the server at the webhook's address answered 403 without the answer of Mattermost: Forbidden" + checkTheAddress},
		{"JSON with an id and no status code", 400, "", `{"id":"bad_request","message":"no such route"}`, "the server at the webhook's address answered 400 without the answer of Mattermost: no such route" + checkTheAddress},
		{"long words of another server", 502, "", `{"message":"` + strings.Repeat("x", 400) + `"}`, longWords + "…" + checkTheAddress},
		{"a redirect, which the module does not follow", 301, "", "", "the server at the webhook's address answered 301, a redirect, and MMWatchdog follows no redirect. Copy the webhook's address from Mattermost again into the file that webhook_url_file names."},
		{"a permanent redirect", 308, "", "", "the server at the webhook's address answered 308, a redirect, and MMWatchdog follows no redirect. Copy the webhook's address from Mattermost again into the file that webhook_url_file names."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			mattermost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				if tc.status/100 == 3 {
					w.Header().Set("Location", "/moved"+r.URL.Path)
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer mattermost.Close()
			o := run(t, "mattermost", failure, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/mattermost"}, map[string]string{"webhook_url_file": mattermost.URL + "/hooks/" + mattermostKey})
			if o.exitStatus != 0 || o.status != "failed" || o.message != tc.want || len(o.message) > 300 {
				t.Fatalf("got  %q\nwant %q\n%+v", o.message, tc.want, o)
			}
			if requests.Load() != 1 || o.holds(mattermostKey) {
				t.Fatalf("%d requests; %+v", requests.Load(), o)
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
		o := run(t, "mattermost", failure, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/mattermost"}, map[string]string{"webhook_url_file": address + "/hooks/" + mattermostKey})
		if o.exitStatus != 0 || o.status != "failed" || !strings.HasPrefix(o.message, "MMWatchdog could not send the message to Mattermost: ") || !strings.Contains(o.message, "connection refused") || o.holds(mattermostKey) || o.holds("/hooks/") {
			t.Fatalf("%+v", o)
		}
	})
	// A file that holds no address of a webhook fails the attempt before
	// the module sends anything.
	t.Run("a file that holds no address of a webhook", func(t *testing.T) {
		mattermost := startService(t, func(received) (int, string) { return 200, "ok" })
		for _, content := range []string{
			strings.TrimPrefix(mattermost.URL, "http://") + "/hooks/" + mattermostKey,
			mattermost.URL + "/hooks/" + mattermostKey + "\nmore",
			mattermost.URL + "/hooks/" + mattermostKey + " " + mattermost.URL,
			mattermost.URL + "/hooks/" + mattermostKey + "\" --output /tmp/x \"",
			mattermost.URL + "/hooks/" + mattermostKey + "?channel=ops",
			mattermost.URL + "/hooks/" + mattermostKey + "/",
			mattermost.URL + "/hooks/",
			mattermost.URL,
			"https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX",
			"ftp://chat.example.com/hooks/" + mattermostKey,
			"https:///hooks/" + mattermostKey,
			"",
		} {
			o := run(t, "mattermost", failure, map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/mattermost"}, map[string]string{"webhook_url_file": content})
			if o.exitStatus != 0 || o.status != "failed" || o.message != "the file that webhook_url_file names must hold one address of a Mattermost incoming webhook, such as https://chat.example.com/hooks/9xuqwrwgstrb3mzrxb83nb357a, and nothing else" {
				t.Fatalf("%q: %+v", content, o)
			}
		}
		if requests := mattermost.got(); len(requests) != 0 {
			t.Fatalf("the module sent a request to an address that is none: %+v", requests)
		}
	})
}

// MMWatchdog checks what a destination states for mattermost against the module's
// description, before the module runs.
func TestTheInputsOfMattermostAreValidated(t *testing.T) {
	t.Parallel()
	modules := checksandalerts.ModuleDirs{OfficialExtra: officialExtraModules(t), External: filepath.Join(t.TempDir(), "external_modules")}
	if module, e := config.DescribeModule(modules, "mattermost"); e != nil || module.Kind != "official_extra" || module.ShippedWithMMWatchdogVersion != checksandalerts.Version || module.Description.UsedAs != "alert_destination" || len(module.Description.Examples) == 0 {
		t.Fatalf("%+v %v", module, e)
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
		{"mattermost: {webhook_url_file: /etc/mmwatchdog/secrets/mattermost-webhook-team, mention: '@barbara'}", ""},
		{"mattermost: {webhook_url_file: /etc/mmwatchdog/secrets/mattermost-webhook-team}", ""},
		{"mattermost: {mention: '@channel'}", "destinations.chat.mattermost.webhook_url_file: is required: The absolute path of a file that holds the address of the Mattermost incoming webhook, and nothing else."},
		{"mattermost: {webhook_url_file: /etc/mmwatchdog/secrets/mattermost, channel: ops}", `destinations.chat.mattermost.channel: unknown field "channel": the inputs of the module mattermost are webhook_url_file, mention`},
		{"mattermost: {webhook_url_file: /etc/mmwatchdog/secrets/mattermost, username: mmwatchdog}", `destinations.chat.mattermost.username: unknown field "username": the inputs of the module mattermost are webhook_url_file, mention`},
		{"mattermost: {webhook_url_file: secrets/mattermost}", "destinations.chat.mattermost.webhook_url_file: an input whose name ends in _file names a file that MMWatchdog reads and gives to the module. Set webhook_url_file to an absolute file path"},
		{"mattermost: {webhook_url_file: /etc/mmwatchdog/secrets/mattermost, mention: 7}", "destinations.chat.mattermost.mention: expected a string"},
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
