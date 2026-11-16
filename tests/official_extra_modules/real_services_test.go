package official_extra_modules

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"mmwatchdog/internal/checksandalerts"
)

// The tests in this file send real alerts through the modules slack,
// telegram, discord, and pushover to the real services. A person runs them by
// hand before a release, with the keys of test accounts in environment
// variables; without those variables they skip. docs/changing-mmwatchdog/testing.md, section
// "Real services before a release", says how to run them.
//
// The keys are secrets, and test output can end up anywhere. So nothing that
// a service or a module wrote reaches the output except through
// realServiceTest.text, which writes [secret] in place of each key.

// exchange is one request that the recording proxy passed to the real
// service, and the service's answer. Its path and bodies can hold keys.
type exchange struct {
	method, path   string
	requestBody    []byte
	httpStatusCode int
	answerBody     []byte
}

func (recorded exchange) String() string {
	return fmt.Sprintf("%s %s request=%s; HTTP %d answer=%s", recorded.method, recorded.path, recorded.requestBody, recorded.httpStatusCode, recorded.answerBody)
}

// recordingProxy is a server on loopback that passes each request of a module
// unchanged to one real service and keeps the service's answer.
type recordingProxy struct {
	*httptest.Server
	mutex     sync.Mutex
	exchanges []exchange
}

// realServiceTest holds the secrets of one test, which its output never
// shows, and its recording proxy, whose exchanges a failure prints.
type realServiceTest struct {
	secrets   []string
	recording *recordingProxy
}

func realServiceSettings(t *testing.T, names ...string) (*realServiceTest, []string) {
	t.Helper()
	values := make([]string, len(names))
	for index, name := range names {
		values[index] = os.Getenv(name)
		if values[index] == "" {
			t.Skipf("%s is empty; see docs/changing-mmwatchdog/testing.md, section \"Real services before a release\"", name)
		}
	}
	return &realServiceTest{secrets: append([]string(nil), values...)}, values
}

// All dynamic diagnostics pass through this one redactor, including errors
// whose URLs or response bodies may repeat a credential.
func (test *realServiceTest) text(format string, args ...any) string {
	text := fmt.Sprintf(format, args...)
	secrets := append([]string(nil), test.secrets...)
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[secret]")
			text = strings.ReplaceAll(text, url.QueryEscape(secret), "[secret]")
		}
	}
	return text
}

func (test *realServiceTest) fatal(t *testing.T, format string, args ...any) {
	t.Helper()
	if test.recording != nil {
		test.log(t, "Real service exchanges: %v", test.recording.got())
	}
	t.Fatal(test.text(format, args...))
}

func (test *realServiceTest) log(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Log(test.text(format, args...))
}

func randomHex(t *testing.T, byteCount int) string {
	t.Helper()
	value := make([]byte, byteCount)
	if _, err := rand.Read(value); err != nil {
		t.Fatal("cannot generate a random test identifier")
	}
	return hex.EncodeToString(value)
}

// realServiceAlerts returns the three alerts that every test sends, in this
// order: a failure with a check's output and the awkward text, its recovery,
// and a long alert of about 6,600 characters with ü and emoji, which each
// module cuts to its service's limit. Every title names one run, so that a
// person can tell the messages of this run in a channel from earlier ones.
func realServiceAlerts(t *testing.T) []checksandalerts.Event {
	t.Helper()
	runID := randomHex(t, 4)
	testAlerts := make([]checksandalerts.Event, 3)
	for index := range testAlerts {
		alert := failure
		alert.ID, alert.Incident = randomHex(t, 16), randomHex(t, 16)
		alert.Node, alert.CheckGroup, alert.Check = "real-service-test", "mmwatchdog_release", "http"
		alert.Body = "expected HTTP 200, received 503\nfailing since 14:02 UTC\n" + awkward
		alert.Title = fmt.Sprintf("%s: mmwatchdog_release/http failed (run %s)", alert.Node, runID)
		alert.TitleWithAlertSeverity = "SOS: " + alert.Title
		if index == 1 {
			alert.Type, alert.Body = "recovered", "HTTP 200 again"
			alert.Title = fmt.Sprintf("%s: mmwatchdog_release/http is ok again (run %s)", alert.Node, runID)
			alert.TitleWithAlertSeverity = "Resolved SOS: " + alert.Title
		}
		if index == 2 {
			alert.AlertSeverity = "warn"
			alert.Body = strings.Repeat("ASCII ü 🐒 mixed line\n", 300)
			alert.TitleWithAlertSeverity = "Warn: " + alert.Title
		}
		testAlerts[index] = alert
	}
	return testAlerts
}

func (test *realServiceTest) webhook(t *testing.T, address string) (*url.URL, string) {
	t.Helper()
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		test.fatal(t, "expected an HTTPS webhook address: %v", err)
	}
	segments := strings.Split(parsed.Path, "/")
	if len(segments) < 2 || segments[len(segments)-1] == "" {
		test.fatal(t, "webhook address has no final credential segment")
	}
	test.secrets = append(test.secrets, segments[len(segments)-1])
	// Slack treats every segment after /services/ as secret, even in errors
	// which repeat only part of the webhook address.
	if strings.Contains(parsed.Path, "/services/") {
		for _, segment := range segments {
			if segment != "" && segment != "services" {
				test.secrets = append(test.secrets, segment)
			}
		}
	}
	segments[len(segments)-1] = strings.Repeat("A", len(segments[len(segments)-1]))
	wrong := *parsed
	wrong.Path, wrong.RawPath = strings.Join(segments, "/"), ""
	test.secrets = append(test.secrets, wrong.String(), segments[len(segments)-1])
	return parsed, wrong.String()
}

// This uses the same compiled entry point as runWithRequest. Its failure
// paths also redact output, which the stand-in helper can safely print raw.
func (test *realServiceTest) run(t *testing.T, module string, alert *checksandalerts.Event, inputs map[string]any, files map[string]string) outcome {
	t.Helper()
	request := map[string]any{"protocol": 1, "inputs": inputs, "file_contents": files}
	if alert == nil {
		request["verify_destination"] = true
	} else {
		request["alert"] = *alert
	}
	body, err := json.Marshal(request)
	if err != nil {
		test.fatal(t, "encode module request: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	directory := t.TempDir()
	command := exec.CommandContext(ctx, program(t), "run-starlark-module", filepath.Join(officialExtraModules(t), module))
	command.Dir, command.Stdin = directory, bytes.NewReader(body)
	command.Env = []string{"PATH=" + directory, "HOME=" + directory, "USER=mmwatchdog", "LANG=C.UTF-8"}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	result := outcome{stdout: stdout.String(), stderr: stderr.String()}
	if command.ProcessState != nil {
		result.exitStatus = command.ProcessState.ExitCode()
	}
	for _, secret := range test.secrets {
		if secret != "" && result.holds(secret) {
			test.fatal(t, "%s exposed a secret in module output: stdout=%s stderr=%s", module, result.stdout, result.stderr)
		}
	}
	if err != nil {
		test.fatal(t, "%s module run: %v; stdout=%s stderr=%s", module, err, result.stdout, result.stderr)
	}
	var decoded struct {
		Protocol int    `json:"protocol"`
		Status   string `json:"status"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil || decoded.Protocol != 1 {
		test.fatal(t, "%s invalid module answer: %v; stdout=%s stderr=%s", module, err, result.stdout, result.stderr)
	}
	result.status, result.message = decoded.Status, decoded.Message
	return result
}

func (test *realServiceTest) verify(t *testing.T, module string, inputs map[string]any, files map[string]string, accepted bool) {
	t.Helper()
	result := test.run(t, module, nil, inputs, files)
	var answer checksandalerts.DestinationVerification
	if err := json.Unmarshal([]byte(result.stdout), &answer); err != nil {
		test.fatal(t, "decode verification: %v; module answered %s", err, result.stdout)
	}
	test.log(t, "%s verification_message: %s", module, answer.VerificationMessage)
	if !answer.CanVerifyWithoutMessage || answer.AcceptsAddressAndCredential != accepted || answer.VerificationMessage == "" {
		test.fatal(t, "expected accepts_address_and_credential=%t and quiet verification; module answered %s", accepted, result.stdout)
	}
}

func (test *realServiceTest) delivery(t *testing.T, module string, alert checksandalerts.Event, inputs map[string]any, files map[string]string, status, message string) {
	t.Helper()
	result := test.run(t, module, &alert, inputs, files)
	test.log(t, "%s delivery: %s", module, result.message)
	if result.status != status || !strings.Contains(result.message, message) {
		test.fatal(t, "expected status %s and message containing %q; module answered %s", status, message, result.stdout)
	}
	if module == "slack" && status == "delivered" && result.message != message {
		test.fatal(t, "expected exact Slack acceptance %q; module answered %s", message, result.stdout)
	}
}

func (proxy *recordingProxy) got() []exchange {
	proxy.mutex.Lock()
	defer proxy.mutex.Unlock()
	return append([]exchange(nil), proxy.exchanges...)
}

// proxy starts the recording proxy for one real service. Telegram and Discord
// answer a delivered message with the message as they stored it, and
// Pushover answers an emergency message with its receipt. The modules do not
// pass these answers on, so the test reads them at the proxy. The module's
// request reaches the service unchanged; only the connection from the
// module to the proxy is plain HTTP on loopback.
func (test *realServiceTest) proxy(t *testing.T, baseAddress string) *recordingProxy {
	t.Helper()
	base, err := url.Parse(baseAddress)
	if err != nil || base.Scheme != "https" {
		test.fatal(t, "invalid real service base address: %v", err)
	}
	proxy := &recordingProxy{}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 20 * time.Second
	t.Cleanup(transport.CloseIdleConnections)
	proxy.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, test.text("read module request: %v", err), http.StatusBadRequest)
			return
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		recorded := exchange{method: request.Method, path: request.URL.RequestURI(), requestBody: body}
		forward := &httputil.ReverseProxy{
			Transport: transport,
			Rewrite: func(outgoing *httputil.ProxyRequest) {
				outgoing.SetURL(base)
				outgoing.Out.Host = base.Host
				// The module's HTTP client asks for a compressed answer. Without
				// this line the proxy would pass that on and keep the answer
				// compressed, which the test cannot read. The proxy's own client
				// then asks for compression and unpacks the answer itself.
				outgoing.Out.Header.Del("Accept-Encoding")
				for header := range outgoing.Out.Header {
					if strings.HasPrefix(strings.ToLower(header), "x-forwarded-") {
						outgoing.Out.Header.Del(header)
					}
				}
			},
			ModifyResponse: func(response *http.Response) error {
				answer, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil {
					return err
				}
				response.Body = io.NopCloser(bytes.NewReader(answer))
				recorded.httpStatusCode, recorded.answerBody = response.StatusCode, answer
				proxy.mutex.Lock()
				proxy.exchanges = append(proxy.exchanges, recorded)
				proxy.mutex.Unlock()
				return nil
			},
			ErrorHandler: func(writer http.ResponseWriter, request *http.Request, err error) {
				recorded.httpStatusCode, recorded.answerBody = http.StatusBadGateway, []byte(test.text("forward to real service: %v", err))
				proxy.mutex.Lock()
				proxy.exchanges = append(proxy.exchanges, recorded)
				proxy.mutex.Unlock()
				http.Error(writer, string(recorded.answerBody), http.StatusBadGateway)
			},
		}
		forward.ServeHTTP(writer, request)
	}))
	proxy.Config.ErrorLog = log.New(io.Discard, "", 0)
	proxy.Start()
	t.Cleanup(proxy.Close)
	test.recording = proxy
	return proxy
}

func (test *realServiceTest) decode(t *testing.T, body []byte, destination any) {
	t.Helper()
	if err := json.Unmarshal(body, destination); err != nil {
		test.fatal(t, "invalid JSON: %v; body=%s", err, body)
	}
}

func (test *realServiceTest) last(t *testing.T, exchanges []exchange, pathSuffix string) exchange {
	t.Helper()
	if len(exchanges) == 0 {
		test.fatal(t, "no request reached the real service; expected POST ending in %s", pathSuffix)
	}
	last := exchanges[len(exchanges)-1]
	path, _, _ := strings.Cut(last.path, "?")
	if last.method != "POST" || !strings.HasSuffix(path, pathSuffix) || last.httpStatusCode != http.StatusOK {
		test.fatal(t, "expected POST ending in %s and HTTP 200; got %s %s HTTP %d; answer=%s", pathSuffix, last.method, last.path, last.httpStatusCode, last.answerBody)
	}
	return last
}

func (test *realServiceTest) lengths(t *testing.T, label, text string) {
	t.Helper()
	test.log(t, "%s: %d code points, %d UTF-16 code units", label, utf8.RuneCountInString(text), len(utf16.Encode([]rune(text))))
}

// The slack module's verification accepts the real webhook and refuses a
// wrong one, a delivery to a wrong webhook gives the advice to make a new
// one, and Slack accepts the three alerts. Slack answers only "ok", so a
// person looks at the channel once, as the test's output says.
func TestRealServiceSlack(t *testing.T) {
	test, settings := realServiceSettings(t, "MMWATCHDOG_REAL_SERVICE_TEST_SLACK_WEBHOOK_URL")
	t.Parallel()
	_, wrong := test.webhook(t, settings[0])
	alerts := realServiceAlerts(t)
	inputs := map[string]any{"webhook_url_file": "/secret/slack"}
	realFiles := map[string]string{"webhook_url_file": settings[0]}
	wrongFiles := map[string]string{"webhook_url_file": wrong}
	test.verify(t, "slack", inputs, realFiles, true)
	test.verify(t, "slack", inputs, wrongFiles, false)
	test.delivery(t, "slack", alerts[0], inputs, wrongFiles, "failed", "The address of the webhook does not work any more.")
	for index, name := range []string{"failure", "recovery", "long_alert"} {
		if !t.Run(name, func(t *testing.T) {
			deliveryInputs := map[string]any{"webhook_url_file": "/secret/slack"}
			if index == 0 {
				deliveryInputs["mention"] = "<!here>"
			}
			test.delivery(t, "slack", alerts[index], deliveryInputs, realFiles, "delivered", "Slack accepted the message")
		}) {
			return
		}
	}
	test.log(t, "Inspect this run in Slack: first line is plain text with <!here>: %s. The body is a code block showing these awkward characters literally:\n%s\nThe long message ends with … and a closed code block.", alerts[0].TitleWithAlertSeverity, awkward)
}

// The telegram module's verification accepts the real bot and chat and
// refuses a wrong token and an unknown chat, a delivery with either gives the
// advice for it, and Telegram stores each of the three alerts with exactly
// the text that the module sent. The output shows how many tries the long
// alert took, which shows how Telegram counts an emoji.
func TestRealServiceTelegram(t *testing.T) {
	test, settings := realServiceSettings(t, "MMWATCHDOG_REAL_SERVICE_TEST_TELEGRAM_BOT_TOKEN", "MMWATCHDOG_REAL_SERVICE_TEST_TELEGRAM_CHAT_ID")
	t.Parallel()
	prefix, tokenSecret, found := strings.Cut(settings[0], ":")
	if !found || tokenSecret == "" {
		test.fatal(t, "bot token must contain a colon followed by its secret")
	}
	wrong := prefix + ":" + strings.Repeat("A", len(tokenSecret))
	test.secrets = append(test.secrets, tokenSecret, wrong, strings.Repeat("A", len(tokenSecret)))
	alerts := realServiceAlerts(t)
	inputs := map[string]any{"bot_token_file": "/secret/telegram", "chat_id": settings[1]}
	realFiles := map[string]string{"bot_token_file": settings[0]}
	wrongFiles := map[string]string{"bot_token_file": wrong}
	unknownChat := map[string]any{"bot_token_file": "/secret/telegram", "chat_id": "-1009999999999"}
	test.verify(t, "telegram", inputs, realFiles, true)
	test.verify(t, "telegram", inputs, wrongFiles, false)
	test.verify(t, "telegram", unknownChat, realFiles, false)
	test.delivery(t, "telegram", alerts[0], inputs, wrongFiles, "failed", "Telegram does not know this bot token.")
	test.delivery(t, "telegram", alerts[0], unknownChat, realFiles, "failed", "Check chat_id, and that the bot is a member of the chat.")
	proxy := test.proxy(t, "https://api.telegram.org")
	inputs["bot_api_server_url"] = proxy.URL
	for index, name := range []string{"failure", "recovery", "long_alert"} {
		if !t.Run(name, func(t *testing.T) {
			before := len(proxy.got())
			test.delivery(t, "telegram", alerts[index], inputs, realFiles, "delivered", "")
			exchanges := proxy.got()[before:]
			last := test.last(t, exchanges, "/sendMessage")
			var request struct {
				Text string `json:"text"`
			}
			var answer struct {
				OK     bool `json:"ok"`
				Result struct {
					Text string `json:"text"`
				} `json:"result"`
			}
			test.decode(t, last.requestBody, &request)
			test.decode(t, last.answerBody, &answer)
			if !answer.OK || request.Text == "" || answer.Result.Text != request.Text {
				test.fatal(t, "Telegram stored different text; request=%s; HTTP %d answer=%s", last.requestBody, last.httpStatusCode, last.answerBody)
			}
			if index == 2 {
				test.log(t, "long alert took %d sendMessage exchanges", len(exchanges))
				for _, attempt := range exchanges[:len(exchanges)-1] {
					var refusal struct {
						Description string `json:"description"`
					}
					test.decode(t, attempt.answerBody, &refusal)
					test.log(t, "Telegram refused attempt: HTTP %d description=%s; answer=%s", attempt.httpStatusCode, refusal.Description, attempt.answerBody)
				}
				test.lengths(t, "Telegram stored text", answer.Result.Text)
			}
		}) {
			return
		}
	}
}

// The discord module's verification accepts the real webhook and refuses a
// wrong token, a delivery with the wrong token gives the advice for it, and
// Discord stores each of the three alerts with exactly the content that the
// module sent. The input mention notifies the channel, and @everyone in a
// check's output notifies nobody. The output shows how many tries the long
// alert took, which shows how Discord counts an emoji.
func TestRealServiceDiscord(t *testing.T) {
	test, settings := realServiceSettings(t, "MMWATCHDOG_REAL_SERVICE_TEST_DISCORD_WEBHOOK_URL")
	t.Parallel()
	address, wrong := test.webhook(t, settings[0])
	alerts := realServiceAlerts(t)
	inputs := map[string]any{"webhook_url_file": "/secret/discord"}
	realFiles := map[string]string{"webhook_url_file": settings[0]}
	wrongFiles := map[string]string{"webhook_url_file": wrong}
	test.verify(t, "discord", inputs, realFiles, true)
	test.verify(t, "discord", inputs, wrongFiles, false)
	test.delivery(t, "discord", alerts[0], inputs, wrongFiles, "failed", "The webhook was deleted, or the address in the file is wrong.")
	proxy := test.proxy(t, "https://discord.com")
	proxyAddress := proxy.URL + address.EscapedPath()
	test.secrets = append(test.secrets, proxyAddress)
	realFiles["webhook_url_file"] = proxyAddress
	alerts[1].Body += "\n@everyone and @here in a check's output"
	for index, name := range []string{"failure", "recovery", "long_alert"} {
		if !t.Run(name, func(t *testing.T) {
			deliveryInputs := map[string]any{"webhook_url_file": "/secret/discord"}
			if index == 0 {
				deliveryInputs["mention"] = "@here"
			}
			before := len(proxy.got())
			test.delivery(t, "discord", alerts[index], deliveryInputs, realFiles, "delivered", "")
			exchanges := proxy.got()[before:]
			last := test.last(t, exchanges, "")
			var request struct {
				Content string `json:"content"`
			}
			var answer struct {
				Content         string `json:"content"`
				MentionEveryone *bool  `json:"mention_everyone"`
			}
			test.decode(t, last.requestBody, &request)
			test.decode(t, last.answerBody, &answer)
			if request.Content == "" || answer.Content != request.Content || answer.MentionEveryone == nil || *answer.MentionEveryone != (index == 0) {
				test.fatal(t, "Discord content or mention_everyone differs; expected mention_everyone=%t; request=%s; HTTP %d answer=%s", index == 0, last.requestBody, last.httpStatusCode, last.answerBody)
			}
			if index == 2 {
				test.log(t, "long alert took %d exchanges", len(exchanges))
				for _, attempt := range exchanges[:len(exchanges)-1] {
					test.log(t, "Discord refused attempt: HTTP %d error=%s", attempt.httpStatusCode, attempt.answerBody)
				}
				test.lengths(t, "Discord stored content", answer.Content)
			}
		}) {
			return
		}
	}
}

type pushoverReceiptReading struct {
	Status          int   `json:"status"`
	Acknowledged    int   `json:"acknowledged"`
	AcknowledgedAt  int64 `json:"acknowledged_at"`
	LastDeliveredAt int64 `json:"last_delivered_at"`
	Expired         int   `json:"expired"`
	ExpiresAt       int64 `json:"expires_at"`
}

func (test *realServiceTest) pushoverRequest(t *testing.T, method, address string, form url.Values) (int, []byte) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request, err := http.NewRequest(method, address, body)
	if err != nil {
		test.fatal(t, "make Pushover request: %v", err)
	}
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	client := &http.Client{Timeout: 20 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		test.fatal(t, "Pushover request: %v", err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		test.fatal(t, "read Pushover HTTP %d answer: %v; partial answer=%s", response.StatusCode, err, answer)
	}
	return response.StatusCode, answer
}

func (test *realServiceTest) receipt(t *testing.T, token, receipt, node, phase string) pushoverReceiptReading {
	t.Helper()
	address := "https://api.pushover.net/1/receipts/" + url.PathEscape(receipt) + ".json?" + url.Values{"token": {token}}.Encode()
	status, body := test.pushoverRequest(t, "GET", address, nil)
	test.log(t, "%s receipt of %s: HTTP %d answer=%s", phase, node, status, body)
	var reading pushoverReceiptReading
	test.decode(t, body, &reading)
	var fields map[string]json.RawMessage
	test.decode(t, body, &fields)
	for _, name := range []string{"status", "acknowledged", "acknowledged_at", "last_delivered_at", "expired", "expires_at"} {
		if value, exists := fields[name]; !exists || string(value) == "null" {
			test.fatal(t, "Pushover receipt of %s lacks %s: HTTP %d answer=%s", node, name, status, body)
		}
	}
	if status != http.StatusOK || reading.Status != 1 {
		test.fatal(t, "Pushover refused receipt of %s: HTTP %d answer=%s", node, status, body)
	}
	if reading.Acknowledged == 1 {
		test.fatal(t, "somebody acknowledged the emergency on a device; the stop by tag was not tested; run again without acknowledging; node=%s; HTTP %d answer=%s", node, status, body)
	}
	return reading
}

func (test *realServiceTest) closingExchange(t *testing.T, exchanges []exchange, key string) {
	t.Helper()
	last := test.last(t, exchanges, "/1/messages.json")
	if len(exchanges) != 2 || exchanges[0].method != "POST" || exchanges[0].path != "/1/receipts/cancel_by_tag/mmwatchdog_fleet_"+key+".json" {
		test.fatal(t, "expected a stop by tag before the closing message; exchanges=%+v; closing answer=%s", exchanges, last.answerBody)
	}
	test.log(t, "cancel_by_tag before closing message: HTTP %d answer=%s", exchanges[0].httpStatusCode, exchanges[0].answerBody)
}

// The pushover module's verification accepts the real keys and refuses a
// wrong application token, and a delivery with the wrong token gives the
// advice for it. Pushover takes a long info alert at low priority in one
// try, and a closing alert of a fleet incident key that no message has.
// Then two nodes send an sos alert of one check, which Pushover repeats as
// emergencies, and the closing alert of the check stops the repeats of both
// by their tag. Pushover's receipts show both: the repeats before the stop,
// and none after it.
func TestRealServicePushover(t *testing.T) {
	test, settings := realServiceSettings(t, "MMWATCHDOG_REAL_SERVICE_TEST_PUSHOVER_APPLICATION_TOKEN", "MMWATCHDOG_REAL_SERVICE_TEST_PUSHOVER_USER_OR_GROUP_KEY")
	t.Parallel()
	token, userKey := settings[0], settings[1]
	wrongToken := strings.Repeat("A", len(token))
	test.secrets = append(test.secrets, wrongToken)
	alerts := realServiceAlerts(t)
	inputs := map[string]any{
		"application_token_file":          "/secret/pushover-application-token",
		"user_or_group_key_file":          "/secret/pushover-user-or-group-key",
		"repeat_sos_alerts_every":         "30s",
		"stop_repeating_sos_alerts_after": "5m",
	}
	files := map[string]string{"application_token_file": token, "user_or_group_key_file": userKey}
	wrongFiles := map[string]string{"application_token_file": wrongToken, "user_or_group_key_file": userKey}
	test.verify(t, "pushover", inputs, files, true)
	test.verify(t, "pushover", inputs, wrongFiles, false)
	test.delivery(t, "pushover", alerts[0], inputs, wrongFiles, "failed", "Pushover does not know this application token.")
	proxy := test.proxy(t, "https://api.pushover.net")
	inputs["api_server_url"] = proxy.URL
	if !t.Run("long_alert", func(t *testing.T) {
		alert := alerts[2]
		alert.AlertSeverity = "info"
		alert.TitleWithAlertSeverity = "Info: " + alert.Title
		before := len(proxy.got())
		test.delivery(t, "pushover", alert, inputs, files, "delivered", "")
		exchanges := proxy.got()[before:]
		last := test.last(t, exchanges, "/1/messages.json")
		if len(exchanges) != 1 {
			test.fatal(t, "long Pushover alert took %d exchanges; exchanges=%+v", len(exchanges), exchanges)
		}
		var request struct {
			Message  string `json:"message"`
			Priority int    `json:"priority"`
		}
		test.decode(t, last.requestBody, &request)
		if request.Priority != -1 || request.Message == "" {
			test.fatal(t, "expected low priority and a nonempty message; request=%s; answer=%s", last.requestBody, last.answerBody)
		}
		test.lengths(t, "Pushover sent message", request.Message)
	}) {
		return
	}
	if !t.Run("close_unused_tag", func(t *testing.T) {
		alert := alerts[1]
		alert.FleetIncidentKey = randomHex(t, 16)
		alert.AlertSeveritiesSentForIncident = []string{"sos"}
		before := len(proxy.got())
		test.delivery(t, "pushover", alert, inputs, files, "delivered", "")
		test.closingExchange(t, proxy.got()[before:], alert.FleetIncidentKey)
	}) {
		return
	}
	if !t.Run("stop_both_nodes_by_tag", func(t *testing.T) {
		key := randomHex(t, 16)
		tag := "mmwatchdog_fleet_" + key
		// Register before either send: even a failed assertion must stop both
		// emergencies. The module's five-minute expiry is a further safety bound.
		t.Cleanup(func() {
			status, body := test.pushoverRequest(t, "POST", "https://api.pushover.net/1/receipts/cancel_by_tag/"+tag+".json", url.Values{"token": {token}})
			test.log(t, "cleanup cancel_by_tag: HTTP %d answer=%s", status, body)
		})
		nodes := []string{"real-service-test-1", "real-service-test-2"}
		receipts := make([]string, len(nodes))
		for index, node := range nodes {
			alert := alerts[0]
			alert.ID, alert.Incident = randomHex(t, 16), randomHex(t, 16)
			alert.Node, alert.FleetIncidentKey = node, key
			alert.Title = strings.Replace(alert.Title, "real-service-test:", node+":", 1)
			alert.TitleWithAlertSeverity = "SOS: " + alert.Title
			before := len(proxy.got())
			test.delivery(t, "pushover", alert, inputs, files, "delivered", "")
			exchanges := proxy.got()[before:]
			last := test.last(t, exchanges, "/1/messages.json")
			var answer struct {
				Status  int    `json:"status"`
				Receipt string `json:"receipt"`
			}
			test.decode(t, last.answerBody, &answer)
			if len(exchanges) != 1 || answer.Status != 1 || answer.Receipt == "" {
				test.fatal(t, "expected one emergency receipt for %s; exchanges=%+v; answer=%s", node, exchanges, last.answerBody)
			}
			receipts[index] = answer.Receipt
		}
		sentAt := time.Now()
		time.Sleep(time.Until(sentAt.Add(5 * time.Second)))
		first := make([]pushoverReceiptReading, len(nodes))
		for index, node := range nodes {
			first[index] = test.receipt(t, token, receipts[index], node, "5 seconds after sending")
			if first[index].LastDeliveredAt == 0 {
				test.fatal(t, "Pushover delivered the emergency to no device; node=%s; reading=%+v", node, first[index])
			}
			if first[index].Expired != 0 {
				test.fatal(t, "Pushover emergency expired before the stop could be measured; node=%s; reading=%+v", node, first[index])
			}
		}
		time.Sleep(time.Until(sentAt.Add(40 * time.Second)))
		for index, node := range nodes {
			repeated := test.receipt(t, token, receipts[index], node, "40 seconds after sending")
			if repeated.LastDeliveredAt <= first[index].LastDeliveredAt {
				test.fatal(t, "Pushover showed no repeat within 40 seconds, so the stop cannot be measured; node=%s; first=%+v; second=%+v", node, first[index], repeated)
			}
			if repeated.Expired != 0 {
				test.fatal(t, "Pushover emergency expired before the stop could be measured; node=%s; reading=%+v", node, repeated)
			}
		}
		closing := alerts[1]
		closing.ID, closing.Incident = randomHex(t, 16), randomHex(t, 16)
		closing.Node, closing.FleetIncidentKey = nodes[1], key
		closing.Title = strings.Replace(closing.Title, "real-service-test:", nodes[1]+":", 1)
		closing.TitleWithAlertSeverity = "Resolved SOS: " + closing.Title
		closing.AlertSeveritiesSentForIncident = []string{"sos"}
		before := len(proxy.got())
		test.delivery(t, "pushover", closing, inputs, files, "delivered", "")
		test.closingExchange(t, proxy.got()[before:], key)
		stopped := make([]pushoverReceiptReading, len(nodes))
		for index, node := range nodes {
			stopped[index] = test.receipt(t, token, receipts[index], node, "immediately after stop")
		}
		time.Sleep(40 * time.Second)
		for index, node := range nodes {
			later := test.receipt(t, token, receipts[index], node, "40 seconds after stop")
			// A delivery already in flight when cancellation arrives can finish.
			// Five seconds allows that delivery, but not another 30-second repeat.
			if later.LastDeliveredAt > stopped[index].LastDeliveredAt+5 {
				test.fatal(t, "Pushover kept repeating the emergency of %s after the stop by tag; immediately=%+v; later=%+v", node, stopped[index], later)
			}
		}
	}) {
		return
	}
	test.log(t, "Inspect this run on the phone: low-priority long message (no sound), one closing message for an unused tag, two emergencies from real-service-test-1 and real-service-test-2 that repeated until the stop, and the incident's closing message. Run title: %s", alerts[0].TitleWithAlertSeverity)
}
