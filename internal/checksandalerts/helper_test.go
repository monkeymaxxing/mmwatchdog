package checksandalerts_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"

	"mmwatchdog/internal/checksandalerts"
)

func TestModuleJSONStringsAndMessages(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "quotes \" backslash \\", "multiline\n\n", "Știință 🐒"} {
		input, _ := json.Marshal(map[string]any{"protocol": 1, "inputs": map[string]any{"message": value}})
		var out bytes.Buffer
		if err := checksandalerts.ModuleJSONHelper([]string{"get", "--type", "string", "/inputs/message"}, bytes.NewReader(input), &out); err != nil {
			t.Fatal(err)
		}
		if out.String() != value {
			t.Fatalf("changed string: %q", out.String())
		}
		for _, args := range [][]string{{"result", "--status", "ok", "--message", "message: " + value}, {"result", "--status", "ok", "--message-stdin"}} {
			out.Reset()
			if err := checksandalerts.ModuleJSONHelper(args, strings.NewReader("message: "+value), &out); err != nil {
				t.Fatal(err)
			}
			var result checksandalerts.ModuleResult
			if err := checksandalerts.DecodeJSON(out.Bytes(), &result); err != nil || result.Message != "message: "+value {
				t.Fatal(result, err)
			}
		}
	}
}

func TestModuleJSONGet(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ value, kind, want string }{
		{"2.0", "integer", "2\n"}, {"2e3", "integer", "2000\n"}, {"-9223372036854775808", "integer", "-9223372036854775808\n"}, {"9223372036854775807.0", "integer", "9223372036854775807\n"},
		{"false", "boolean", "false\n"}, {"0", "number", "0\n"}, {"null", "null", "null\n"}, {"[]", "array", "[]\n"}, {"{}", "object", "{}\n"}, {`""`, "json", `""` + "\n"},
	} {
		t.Run(tc.value+tc.kind, func(t *testing.T) {
			var out bytes.Buffer
			err := checksandalerts.ModuleJSONHelper([]string{"get", "--type", tc.kind, "/inputs/value"}, strings.NewReader(`{"protocol":1,"inputs":{"value":`+tc.value+`}}`), &out)
			if err != nil || out.String() != tc.want {
				t.Fatal(out.String(), err)
			}
		})
	}
	var out bytes.Buffer
	if err := checksandalerts.ModuleJSONHelper([]string{"get", "--type", "string", "/inputs/a~1b/~0key/0"}, strings.NewReader(`{"inputs":{"a/b":{"~key":["exact\n"]}}}`), &out); err != nil || out.String() != "exact\n" {
		t.Fatal(out.String(), err)
	}
}

func TestModuleJSONRequestAndMeasurements(t *testing.T) {
	t.Parallel()
	// The request of a check's module holds its inputs, the state
	// directory of the check, and when MMWatchdog ends the module.
	input := `{"protocol":1,"inputs":{"limit":2,"path":"/tmp/jobs"},"state_dir":"/var/lib/mmwatchdog-module-state/myapp/job_queue_is_short","mmwatchdog_ends_the_module_after_milliseconds":9987}`
	for _, args := range [][]string{{"request"}, {"request", "--inputs", "limit,path"}} {
		var out bytes.Buffer
		if err := checksandalerts.ModuleJSONHelper(args, strings.NewReader(input), &out); err != nil || out.String() != input {
			t.Fatal(out.String(), err)
		}
	}
	var out bytes.Buffer
	if err := checksandalerts.ModuleJSONHelper([]string{"get", "--type", "string", "/state_dir"}, strings.NewReader(input), &out); err != nil || out.String() != "/var/lib/mmwatchdog-module-state/myapp/job_queue_is_short" {
		t.Fatal(out.String(), err)
	}
	out.Reset()
	// A measurement is its name and its number: the unit is in the module's
	// description. A finding is its ID and its message.
	args := []string{"result", "--status", "failed", "--message-stdin", "--measurement", "pending", "780", "--measurement", "age", "1.5", "--finding", "www.example.com", "the certificate of www.example.com expires in 3 days", "--finding", "api.example.com", "the certificate of \"api.example.com\" expires in 12 days"}
	if err := checksandalerts.ModuleJSONHelper(args, strings.NewReader("queue\n\n"), &out); err != nil {
		t.Fatal(err)
	}
	if want := `{"protocol":1,"status":"failed","message":"queue\n\n","measurements":{"age":1.5,"pending":780},"findings":[{"id":"www.example.com","message":"the certificate of www.example.com expires in 3 days"},{"id":"api.example.com","message":"the certificate of \"api.example.com\" expires in 12 days"}]}` + "\n"; out.String() != want {
		t.Fatalf("got  %swant %s", out.String(), want)
	}
	var result checksandalerts.ModuleResult
	if err := checksandalerts.DecodeJSON(out.Bytes(), &result); err != nil || result.Message != "queue\n\n" || result.Measurements["pending"] != 780 || result.Measurements["age"] != 1.5 || len(result.Findings) != 2 || result.Findings[1].ID != "api.example.com" {
		t.Fatal(result, err)
	}
	for _, b := range []string{`{"protocol":1,"status":"ok","message":"fine","measurements":{"x":{"value":1,"unit":"count"}}}`, `{"protocol":1,"status":"ok","message":"fine","measurements":{"x":"1"}}`} {
		if err := checksandalerts.DecodeJSON([]byte(b), &result); err == nil {
			t.Fatal("a measurement that is no plain number accepted:", b)
		}
	}
}

// ValidateResult checks what a result says by itself: its protocol, its
// status, its message, and the form of its measurements and findings.
func TestValidateResult(t *testing.T) {
	t.Parallel()
	valid := func() checksandalerts.ModuleResult {
		return checksandalerts.ModuleResult{Protocol: 1, Status: "failed", Message: "2 certificates expire within 14 days", Measurements: map[string]float64{"days_left": 3}, Findings: []checksandalerts.Finding{{ID: "www.example.com", Message: "expires in 3 days"}, {ID: "api.example.com", Message: "expires in 12 days"}}}
	}
	if e := checksandalerts.ValidateResult(valid()); e != nil {
		t.Fatal(e)
	}
	many := func(n int) []checksandalerts.Finding {
		findings := []checksandalerts.Finding{}
		for i := range n {
			findings = append(findings, checksandalerts.Finding{ID: "host-" + strconv.Itoa(i), Message: "expires soon"})
		}
		return findings
	}
	if r := valid(); checksandalerts.ValidateResult(checksandalerts.ModuleResult{Protocol: 1, Status: r.Status, Message: r.Message, Findings: many(checksandalerts.FindingsLimit)}) != nil {
		t.Fatal("a result with the most findings was refused")
	}
	for want, change := range map[string]func(r *checksandalerts.ModuleResult){
		"unsupported module result protocol 0; expected protocol: 1": func(r *checksandalerts.ModuleResult) { r.Protocol = 0 },
		"the check result status must be ok, failed, or unknown":     func(r *checksandalerts.ModuleResult) { r.Status = "delivered" },
		"a message is required": func(r *checksandalerts.ModuleResult) { r.Message = "" },
		`the measurement "days left" needs a name of 1 to 64 letters, digits, underscores, or hyphens, starting with a letter or digit`: func(r *checksandalerts.ModuleResult) { r.Measurements["days left"] = 3 },
		"a check result can contain at most 500 findings":                             func(r *checksandalerts.ModuleResult) { r.Findings = many(checksandalerts.FindingsLimit + 1) },
		"each finding needs an id of its own: text on one line of at most 255 bytes":  func(r *checksandalerts.ModuleResult) { r.Findings[1].ID = r.Findings[0].ID },
		`the finding "api.example.com" needs a message`:                               func(r *checksandalerts.ModuleResult) { r.Findings[1].Message = "" },
		"each finding needs an id of its own: text on one line of at most 255 bytes ": func(r *checksandalerts.ModuleResult) { r.Findings[0].ID = "two\nlines" },
		" each finding needs an id of its own: text on one line of at most 255 bytes": func(r *checksandalerts.ModuleResult) { r.Findings[0].ID = "" },
		"  each finding needs an id of its own: text on one line of at most 255 bytes": func(r *checksandalerts.ModuleResult) {
			r.Findings[0].ID = strings.Repeat("u", checksandalerts.FindingIDLimit+1)
		},
	} {
		r := valid()
		change(&r)
		if e := checksandalerts.ValidateResult(r); e == nil || e.Error() != strings.TrimSpace(want) {
			t.Errorf("got %v, want %q", e, strings.TrimSpace(want))
		}
	}
	nan := valid()
	nan.Measurements["days_left"] = math.NaN()
	if e := checksandalerts.ValidateResult(nan); e == nil || e.Error() != `the measurement "days_left" needs a finite number as its value` {
		t.Errorf("a measurement that is no number: got %v", e)
	}
}

func TestModuleJSONErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args  []string
		input string
		code  int
	}{
		{nil, "", 4}, {[]string{"unknown"}, "", 4}, {[]string{"get"}, "", 4}, {[]string{"get", "--type", "bad", "/x"}, "{}", 4}, {[]string{"get", "/bad~2"}, "{}", 4},
		{[]string{"get", "x"}, "{}", 4}, {[]string{"get", "/x"}, "{}", 1}, {[]string{"get", "/x"}, `{"x":1,"x":2}`, 1},
		{[]string{"get", "--type", "integer", "/x"}, `{"x":9223372036854775808}`, 1}, {[]string{"get", "--type", "integer", "/x"}, `{"x":2.1}`, 1},
		{[]string{"get", "--type", "integer", "/x"}, `{"x":2.00000000000000000001}`, 1}, {[]string{"get", "--type", "string", "/x"}, `{"x":null}`, 1},
		{[]string{"get", "--type", "string", "/x"}, `{"x":"a\u0000b"}`, 1}, {[]string{"get", "/x/01"}, `{"x":[0,1]}`, 1},
		{[]string{"request"}, `{"protocol":2,"inputs":{}}`, 1}, {[]string{"request"}, `{"protocol":1,"inputs":null}`, 1},
		{[]string{"request", "--inputs", "limit"}, `{"protocol":1,"inputs":{"path":"x"}}`, 1}, {[]string{"request"}, `{"protocol":1,"inputs":{},"extra":0}`, 1},
		{[]string{"result", "ok"}, "", 4}, {[]string{"result", "--status", "ok"}, "", 4}, {[]string{"result", "--status", "invalid", "--message", "x"}, "", 4},
		{[]string{"result", "--status", "ok", "--message", "x", "--message-stdin"}, "", 4},
		{[]string{"result", "--status", "ok", "--message", "x", "--measurement", "x"}, "", 4},
		{[]string{"result", "--status", "ok", "--message", "x", "--measurement", "x", "1", "count"}, "", 4},
		{[]string{"result", "--status", "ok", "--message", "x", "--measurement", "x", "NaN"}, "", 1},
		{[]string{"result", "--status", "ok", "--message", "x", "--measurement", "x", "many"}, "", 1},
		{[]string{"result", "--status", "ok", "--message", "x", "--measurement", "x", "1", "--measurement", "x", "2"}, "", 1},
		{[]string{"result", "--status", "failed", "--message", "x", "--finding", "www.example.com"}, "", 4},
		{[]string{"result", "--status", "failed", "--message", "x", "--finding", "www.example.com", "expires soon", "--finding", "www.example.com", "expires soon"}, "", 1},
		{[]string{"result", "--status", "failed", "--message", "x", "--finding", "", "expires soon"}, "", 1},
		{[]string{"result", "--status", "failed", "--message", "x", "--finding", "www.example.com", ""}, "", 1},
	} {
		t.Run(strings.Join(tc.args, " ")+tc.input, func(t *testing.T) {
			var out bytes.Buffer
			err := checksandalerts.ModuleJSONHelper(tc.args, strings.NewReader(tc.input), &out)
			var helper *checksandalerts.HelperError
			if !errors.As(err, &helper) || helper.Code != tc.code || out.Len() != 0 {
				t.Fatalf("code %d: %v, output %q", tc.code, err, out.String())
			}
		})
	}
}

// A destination module reads its request, which also holds the alert and the
// content of the files its inputs name, answers delivered or failed, and
// builds the JSON for its service with module-json object.
func TestModuleJSONForDestinationModules(t *testing.T) {
	t.Parallel()
	input := `{"protocol":1,"alert":{"title":"app-1: myapp/http failed","body":"expected 200, received 503\nfailing since 14:02 UTC","alert_severity":"sos"},"inputs":{"webhook_url_file":"/etc/mmwatchdog/secrets/slack","mention":"<@U024BE7LH>"},"file_contents":{"webhook_url_file":"https://hooks.slack.example/T000/B000/XXXX"}}`
	var out bytes.Buffer
	if err := checksandalerts.ModuleJSONHelper([]string{"request", "--inputs", "webhook_url_file,mention"}, strings.NewReader(input), &out); err != nil || out.String() != input {
		t.Fatal(out.String(), err)
	}
	for pointer, want := range map[string]string{"/alert/title": "app-1: myapp/http failed", "/alert/body": "expected 200, received 503\nfailing since 14:02 UTC", "/file_contents/webhook_url_file": "https://hooks.slack.example/T000/B000/XXXX", "/inputs/mention": "<@U024BE7LH>"} {
		out.Reset()
		if err := checksandalerts.ModuleJSONHelper([]string{"get", "--type", "string", pointer}, strings.NewReader(input), &out); err != nil || out.String() != want {
			t.Fatal(pointer, out.String(), err)
		}
	}
	out.Reset()
	if err := checksandalerts.ModuleJSONHelper([]string{"result", "--status", "delivered", "--message", "Slack accepted the message"}, strings.NewReader(""), &out); err != nil || out.String() != `{"protocol":1,"status":"delivered","message":"Slack accepted the message"}`+"\n" {
		t.Fatal(out.String(), err)
	}
	out.Reset()
	if err := checksandalerts.ModuleJSONHelper([]string{"result", "--status", "failed", "--message", "Slack answered 404: the webhook does not exist"}, strings.NewReader(""), &out); err != nil || out.String() != `{"protocol":1,"status":"failed","message":"Slack answered 404: the webhook does not exist"}`+"\n" {
		t.Fatal(out.String(), err)
	}
	for _, tc := range []struct {
		args        []string
		stdin, want string
	}{
		{[]string{"object"}, "", `{}`},
		{[]string{"object", "--string", "text", "<@U024BE7LH> app-1: \"quoted\" \\ & two\nlines Știință 🐒"}, "", `{"text":"<@U024BE7LH> app-1: \"quoted\" \\ & two\nlines Știință 🐒"}`},
		{[]string{"object", "--string", "event_action", "trigger", "--string-stdin", "routing_key", "--json", "payload", ` {"summary":"s","severity":"critical"} `, "--json", "count", "3", "--json", "urgent", "true", "--json", "nothing", "null", "--string", "", "an empty name"}, "a secret\n",
			`{"event_action":"trigger","routing_key":"a secret\n","payload":{"summary":"s","severity":"critical"},"count":3,"urgent":true,"nothing":null,"":"an empty name"}`},
	} {
		out.Reset()
		if err := checksandalerts.ModuleJSONHelper(tc.args, strings.NewReader(tc.stdin), &out); err != nil || out.String() != tc.want+"\n" {
			t.Fatalf("%q: got %s, %v", tc.args, out.String(), err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
	}
	// A request is at most ModuleRequestLimit bytes, and the helper reads no
	// more.
	tooLarge := `{"protocol":1,"inputs":{"text":"` + strings.Repeat("t", checksandalerts.ModuleRequestLimit) + `"}}`
	if err := checksandalerts.ModuleJSONHelper([]string{"request"}, strings.NewReader(tooLarge), &out); err == nil || err.Error() != "input exceeds 1048576 bytes" {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args  []string
		input string
		code  int
	}{
		{[]string{"request"}, `{"protocol":1,"inputs":{},"alert":"not an object"}`, 1},
		{[]string{"request"}, `{"protocol":1,"inputs":{},"file_contents":{"token_file":1}}`, 1},
		{[]string{"result", "--status", "delivered"}, "", 4},
		{[]string{"result", "--status", "delivered", "--message", ""}, "", 1},
		{[]string{"result", "--status", "delivered", "--message", "x", "--measurement", "x", "1"}, "", 4},
		{[]string{"result", "--status", "delivered", "--message", "x", "--finding", "www.example.com", "expires soon"}, "", 4},
		{[]string{"object", "--text", "x"}, "", 4},
		{[]string{"object", "--string", "text"}, "", 4},
		{[]string{"object", "--json", "payload"}, "", 4},
		{[]string{"object", "--string-stdin"}, "", 4},
		{[]string{"object", "--string-stdin", "a", "--string-stdin", "b"}, "", 4},
		{[]string{"object", "--string", "text", "a", "--string", "text", "b"}, "", 1},
		{[]string{"object", "--string", "text", "a", "--string-stdin", "text"}, "", 1},
		{[]string{"object", "--string", "text", "not \xff UTF-8"}, "", 1},
		{[]string{"object", "--string-stdin", "text"}, "not \xff UTF-8", 1},
		{[]string{"object", "--json", "payload", ""}, "", 1},
		{[]string{"object", "--json", "payload", "{"}, "", 1},
		{[]string{"object", "--json", "payload", "1 2"}, "", 1},
		{[]string{"object", "--json", "payload", `{"a":1,"a":2}`}, "", 1},
		{[]string{"object", "--json", "payload", "unquoted"}, "", 1},
	} {
		t.Run(strings.Join(tc.args, " ")+tc.input, func(t *testing.T) {
			var out bytes.Buffer
			err := checksandalerts.ModuleJSONHelper(tc.args, strings.NewReader(tc.input), &out)
			var helper *checksandalerts.HelperError
			if !errors.As(err, &helper) || helper.Code != tc.code || out.Len() != 0 {
				t.Fatalf("code %d: %v, output %q", tc.code, err, out.String())
			}
		})
	}
}
