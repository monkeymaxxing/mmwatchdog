package official_extra_modules

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"mmwatchdog/internal/checksandalerts"
)

// verificationInputs supplies the credentials that MMWatchdog has read, as it does
// for a delivery. These tests use the embedded scripts and compiled module runner.
func verificationInputs(module, serverURL string) (map[string]any, map[string]string) {
	switch module {
	case "discord":
		return map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/discord"}, map[string]string{"webhook_url_file": serverURL + discordWebhookPath}
	case "slack":
		return map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/slack"}, map[string]string{"webhook_url_file": serverURL + "/services/" + slackSecret}
	case "mattermost":
		return map[string]any{"webhook_url_file": "/etc/mmwatchdog/secrets/mattermost"}, map[string]string{"webhook_url_file": serverURL + "/hooks/abcdefghijklmnopqrstuvwxyz"}
	case "pushover":
		return pushoverInputs(serverURL), pushoverFileContents
	case "telegram":
		return map[string]any{"bot_token_file": "/etc/mmwatchdog/secrets/telegram", "bot_api_server_url": serverURL, "chat_id": "-1001234567890"}, map[string]string{"bot_token_file": telegramToken}
	}
	panic("unknown destination module: " + module)
}

func verifyOfficialDestination(t *testing.T, module, serverURL string, environment ...string) (checksandalerts.DestinationVerification, outcome) {
	t.Helper()
	inputs, files := verificationInputs(module, serverURL)
	result := runWithRequest(t, module, map[string]any{"protocol": 1, "verify_destination": true, "inputs": inputs, "file_contents": files}, environment...)
	if result.exitStatus != 0 {
		t.Fatalf("verification exited %d: %s", result.exitStatus, result.stderr)
	}
	var verified checksandalerts.DestinationVerification
	if err := json.Unmarshal([]byte(result.stdout), &verified); err != nil {
		t.Fatal(err)
	}
	if !verified.CanVerifyWithoutMessage || verified.VerificationMessage == "" {
		t.Fatalf("missing verification result: %+v", verified)
	}
	for _, secret := range []string{discordWebhookToken, slackSecret, pushoverApplicationToken, pushoverUserOrGroupKey, telegramToken} {
		if result.holds(secret) {
			t.Fatalf("verification exposed a credential: %+v", result)
		}
	}
	return verified, result
}

// The probe must be the quiet request researched for the service, never an
// alert delivery.
func TestOfficialDestinationsMakeOnlyTheirQuietVerificationRequest(t *testing.T) {
	t.Parallel()
	for _, module := range []string{"discord", "slack", "mattermost", "pushover", "telegram"} {
		t.Run(module, func(t *testing.T) {
			t.Parallel()
			type request struct {
				method, path, contentType string
				body                      []byte
			}
			requests := make(chan request, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				requests <- request{r.Method, r.URL.Path, r.Header.Get("Content-Type"), body}
				switch module {
				case "discord":
					io.WriteString(w, `{"id":"123","token":"`+discordWebhookToken+`"}`)
				case "slack":
					w.WriteHeader(400)
					io.WriteString(w, "invalid_payload")
				case "mattermost":
					w.WriteHeader(404)
					io.WriteString(w, `{"status_code":404}`)
				case "pushover":
					io.WriteString(w, `{"status":1,"devices":["phone"]}`)
				case "telegram":
					io.WriteString(w, `{"ok":true,"result":{"id":-1001234567890}}`)
				}
			}))
			defer server.Close()
			baseURL := server.URL
			var environment []string
			if module == "slack" {
				baseURL = "http://hooks.slack.com"
				environment = []string{"HTTP_PROXY=" + server.URL}
			}
			verified, _ := verifyOfficialDestination(t, module, baseURL, environment...)
			if !verified.AcceptsAddressAndCredential {
				t.Fatalf("quiet request refused: %+v", verified)
			}
			var got request
			select {
			case got = <-requests:
			default:
				t.Fatal("no verification request")
			}
			if len(requests) != 0 {
				t.Fatalf("sent %d extra requests", len(requests))
			}
			wantMethod, wantPath := "POST", ""
			var wantBody map[string]any
			switch module {
			case "discord":
				wantMethod, wantPath = "GET", discordExecutePath
			case "slack":
				wantPath = "/services/" + slackSecret
				if got.contentType != "application/json" || json.Valid(got.body) {
					t.Fatalf("Slack probe must be invalid JSON: %+v", got)
				}
			case "mattermost":
				wantPath = "/hooks/abcdefghijklmnopqrstuvwxyz"
				wantBody = map[string]any{"text": "MMWatchdog checks this webhook. Mattermost does not post this text.", "channel": "mmwatchdog!verify"}
			case "pushover":
				wantPath = "/1/users/validate.json"
				form, err := url.ParseQuery(string(got.body))
				if err != nil {
					t.Fatal(err)
				}
				if got.contentType != "application/x-www-form-urlencoded" || !reflect.DeepEqual(form, url.Values{"token": {pushoverApplicationToken}, "user": {pushoverUserOrGroupKey}}) {
					t.Fatalf("wrong validation form: %+v", got)
				}
			case "telegram":
				wantPath = "/bot" + telegramToken + "/getChat"
				wantBody = map[string]any{"chat_id": "-1001234567890"}
			}
			if got.method != wantMethod || got.path != wantPath {
				t.Fatalf("got %s %s; want %s %s", got.method, got.path, wantMethod, wantPath)
			}
			if wantMethod == "GET" && len(got.body) != 0 {
				t.Fatal("GET verification has a body")
			}
			if wantBody != nil {
				var body map[string]any
				if err := json.Unmarshal(got.body, &body); err != nil {
					t.Fatal(err)
				}
				if got.contentType != "application/json" || !reflect.DeepEqual(body, wantBody) {
					t.Fatalf("wrong quiet request body: %+v; want %+v", body, wantBody)
				}
			}
		})
	}
}

type verificationAnswer struct {
	name     string
	status   int
	body     string
	accepted bool
}

func TestOfficialDestinationVerificationRefusesOnlyConfirmedErrors(t *testing.T) {
	t.Parallel()
	cases := map[string][]verificationAnswer{
		"discord": {
			{"deleted webhook", 404, `{"message":"Unknown Webhook","code":10015}`, false},
			{"wrong token", 401, `{"message":"Invalid Webhook Token","code":50027}`, false},
			{"wrong token on another status", 400, `{"message":"Invalid Webhook Token","code":50027}`, false},
			{"unconfirmed forbidden", 403, `{"message":"Missing Access","code":50001}`, true},
			{"unconfirmed not found", 404, `{"message":"Not found"}`, true},
			{"unknown webhook code on another status", 400, `{"message":"Unknown Webhook","code":10015}`, true},
		},
		"slack": {
			{"valid invalid payload", 400, "invalid_payload", true},
			{"unknown workspace", 404, "no_team", false},
			{"invalid token", 400, "invalid_token", false},
			{"disabled webhook", 404, "no_service", false},
			{"inferred forbidden", 403, "Forbidden", true},
			{"unknown not found", 404, "Not Found", true},
		},
		"mattermost": {
			{"valid unknown channel", 404, `{"status_code":404}`, true},
			{"invalid webhook", 400, `{"status_code":400}`, false},
			{"unconfirmed permission refusal", 403, `{"status_code":403}`, true},
		},
		"pushover": {
			{"valid keys", 200, `{"status":1}`, true},
			{"invalid token", 400, `{"token":"invalid","status":0}`, false},
			{"invalid user", 400, `{"user":"invalid","status":0}`, false},
			{"unconfirmed no active device", 400, `{"status":0,"errors":["No devices"]}`, true},
			{"generic bad request", 400, "Bad Request", true},
			{"status without invalid field", 400, `{"status":0}`, true},
		},
		"telegram": {
			{"valid chat", 200, `{"ok":true,"result":{}}`, true},
			{"unauthorized token", 401, `{"ok":false,"error_code":401,"description":"Unauthorized"}`, false},
			{"unknown chat", 400, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`, false},
			{"inferred bot removed", 403, `{"ok":false,"error_code":403,"description":"Forbidden: bot was kicked from the group chat"}`, true},
			{"unconfirmed unauthorized", 401, `{"ok":false,"error_code":401,"description":"Other unauthorized response"}`, true},
			{"unknown error", 400, `{"ok":false,"error_code":400,"description":"Bad Request: other failure"}`, true},
		},
	}
	for module, answers := range cases {
		t.Run(module, func(t *testing.T) {
			t.Parallel()
			answers = append(answers, verificationAnswer{"server error", 500, "Internal Server Error", false}, verificationAnswer{"proxy error", 503, "Unavailable", false}, verificationAnswer{"rate limited", 429, "Too Many Requests", true})
			for _, answer := range answers {
				t.Run(answer.name, func(t *testing.T) {
					server := startService(t, func(received) (int, string) { return answer.status, answer.body })
					baseURL := server.URL
					var environment []string
					if module == "slack" {
						baseURL = "http://hooks.slack.com"
						environment = []string{"HTTP_PROXY=" + server.URL}
					}
					verified, _ := verifyOfficialDestination(t, module, baseURL, environment...)
					if verified.AcceptsAddressAndCredential != answer.accepted {
						t.Fatalf("HTTP %d %q: %+v; accepted should be %t", answer.status, answer.body, verified, answer.accepted)
					}
					if len(server.got()) != 1 {
						t.Fatalf("got %d requests; want one", len(server.got()))
					}
				})
			}
		})
	}
}

func TestSlackCompatibleServiceVerificationMakesNoHTTPRequest(t *testing.T) {
	t.Parallel()
	server := startService(t, func(received) (int, string) { return 500, "This must not be requested" })
	verified, _ := verifyOfficialDestination(t, "slack", server.URL)
	if !verified.AcceptsAddressAndCredential || len(server.got()) != 0 {
		t.Fatalf("connection-only check: %+v; requests %d", verified, len(server.got()))
	}
	if !strings.Contains(strings.ToLower(verified.VerificationMessage), "connection") {
		t.Fatalf("verification does not explain its limit: %+v", verified)
	}
}

func TestOfficialDestinationVerificationRefusesConnectionAndCertificateFailures(t *testing.T) {
	t.Parallel()
	for _, module := range []string{"discord", "slack", "mattermost", "pushover", "telegram"} {
		t.Run(module, func(t *testing.T) {
			t.Parallel()
			t.Run("connection", func(t *testing.T) {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				address := "http://" + listener.Addr().String()
				listener.Close()
				verified, _ := verifyOfficialDestination(t, module, address)
				if verified.AcceptsAddressAndCredential {
					t.Fatalf("connection refusal accepted: %+v", verified)
				}
			})
			t.Run("certificate", func(t *testing.T) {
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("HTTP request used an untrusted certificate") }))
				defer server.Close()
				verified, _ := verifyOfficialDestination(t, module, server.URL)
				if verified.AcceptsAddressAndCredential {
					t.Fatalf("untrusted certificate accepted: %+v", verified)
				}
			})
		})
	}
}
