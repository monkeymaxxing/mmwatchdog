package checksandalerts

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"mmwatchdog/internal/primitives"
)

// The result of a module that is not core holds at most 128 measurements
// (docs/reference/configuration-files.md), and the name of a measurement has at most 64 characters,
// as every name in MMWatchdog's configuration.
const (
	measurementsInOneResultLimit = 128
	measurementNameLimit         = 64
)

// moduleResultSeeds are results of modules from the tests of ModuleCall.Run,
// of "mmwatchdog module-json", and of findings: results that MMWatchdog takes and results
// that it refuses.
var moduleResultSeeds = []string{
	`{"protocol":1,"status":"failed","message":"queue full"}`,
	`{"protocol":1,"status":"ok","message":"780 queued jobs","measurements":{"queued_jobs":780}}`,
	`{"protocol":1,"status":"unknown","message":"cannot read the queue file"}`,
	`{"protocol":1,"status":"ok","message":"780 queued jobs","measurements":{"queued_jobs":780,"workers":4}}`,
	`{"protocol":1,"status":"failed","message":"queue\n\n","measurements":{"age":1.5,"pending":780},"findings":[{"id":"www.example.com","message":"the certificate of www.example.com expires in 3 days"},{"id":"api.example.com","message":"the certificate of \"api.example.com\" expires in 12 days"}]}` + "\n",
	`{"protocol":1,"status":"failed","message":"1 queue is full","findings":[{"id":"mail","message":"the queue mail is full"}]}`,
	`{"protocol":1,"status":"failed","message":"2 certificates expire","findings":[{"id":"api.example.com","message":"soon"},{"id":"api.example.com","message":"soon"}]}`,
	`{"protocol":1,"status":"failed","message":"1 certificate expires","findings":[{"id":"api.example.com"}]}`,
	`{"protocol":1,"status":"failed","message":"1 certificate expires","findings":[{"id":"two\nlines","message":"soon"}]}`,
	`{"protocol":1,"status":"ok","status":"failed","message":"fine"}`,
	`{"protocol":1,"status":"ok","message":"fine"} {}`,
	`{"protocol":1,"status":"ok","message":"fine","extra":true}`,
	`{"protocol":1,"status":"delivered","message":"sent"}`,
	`{"protocol":1,"status":"ok"}`,
	`{"protocol":2,"status":"ok","message":"fine"}`,
	`{"protocol":1,"status":"ok","message":"fine","measurements":{"queued_jobs":{"value":780,"unit":"count"}}}`,
	`{"protocol":1,"status":"ok","message":"fine","measurements":{"x":"1"}}`,
	`{"protocol":1,"status":"ok","message":"fine","measurements":{"-queued_jobs":1}}`,
	`{"protocol":1,"status":"ok","message":"fine","measurements":{"queued_jobs":1e400}}`,
	`[{"protocol":1,"status":"ok","message":"fine"}]`,
	``,
}

// FuzzExecutableModuleOutput gives the standard output of a module's
// executable to the decoding used by ModuleCall.Run and ModuleSender.Send.
// Check results also pass ValidateResult. No program runs.
func FuzzExecutableModuleOutput(f *testing.F) {
	for _, seed := range moduleResultSeeds {
		f.Add([]byte(seed), false)
	}
	for _, seed := range destinationResultSeeds {
		f.Add([]byte(seed), true)
	}
	f.Fuzz(func(t *testing.T, standardOutput []byte, deliversAlerts bool) {
		if deliversAlerts {
			var result destinationModuleResult
			if DecodeJSON(standardOutput, &result) == nil {
				requireStrictModuleJSON(t, standardOutput, &destinationModuleResult{})
				requireDestinationResultRoundTrip(t, result)
			}
			return
		}
		var answer ModuleResult
		if DecodeJSON(standardOutput, &answer) != nil {
			return
		}
		if len(standardOutput) > primitives.ResultLimit {
			t.Fatalf("DecodeJSON accepted %d bytes, and the result of a module is at most %d bytes", len(standardOutput), primitives.ResultLimit)
		}
		if !json.Valid(standardOutput) || !bytes.HasPrefix(bytes.TrimLeft(standardOutput, " \t\r\n"), []byte("{")) {
			t.Fatalf("DecodeJSON accepted %q, which is not one JSON object", standardOutput)
		}
		requireStrictModuleJSON(t, standardOutput, &ModuleResult{})
		if ValidateResult(answer) != nil {
			return
		}
		requireValidModuleResult(t, answer)
		requireModuleResultRoundTrip(t, answer)
	})
}

// FuzzStarlarkModuleResult gives the value that run_check or deliver_alert returns
// to RunStarlarkModule, which converts it to the JSON line of the module's
// result, and gives that line to the decoding of ModuleCall.Run. The script
// is the same text for every input and calls no primitive but read_file: it
// reads the input from a file of the test and returns json.decode of it. A
// string literal in the script cannot hold every input, because Starlark
// refuses a \x escape above 127 and reads bytes that are not UTF-8 in its
// source as U+FFFD.
func FuzzStarlarkModuleResult(f *testing.F) {
	for _, seed := range moduleResultSeeds {
		f.Add([]byte(seed), false)
	}
	for _, seed := range []string{
		`{"status":"ok","message":"fine"}`,
		`{"protocol":7,"status":"ok","message":"fine"}`,
		`{"Protocol":7,"status":"ok","message":"fine"}`,
		`{"status":"ok","message":"fine","measurements":{"queued_jobs":1e20}}`,
		`{"status":"ok","message":"fine","measurements":{"queued_jobs":100000000000000000000}}`,
		`{"status":"ok","message":"fine","measurements":{"queued_jobs":-0}}`,
		`{"status":"ok","message":"fine","measurements":null,"findings":null}`,
		`{"status":"ok","message":"fine","measurements":{"queued_jobs":[780]}}`,
		`{"status":"failed","message":"1 job failed","findings":[{"id":"nightly_report","message":"the job nightly_report failed"}]}`,
		`{"status":"ok","message":"` + "\x01" + ` a control character as it is"}`,
		`{"status":"ok","message":"` + "\xff" + ` a byte that is not UTF-8"}`,
		`"fine"`,
		`780`,
		`null`,
	} {
		f.Add([]byte(seed), false)
	}
	for _, seed := range destinationResultSeeds {
		f.Add([]byte(seed), true)
	}
	f.Fuzz(func(t *testing.T, returnedValue []byte, deliversAlerts bool) {
		directory := t.TempDir()
		returnedValueFile := filepath.Join(directory, "returned_value.json")
		if e := os.WriteFile(returnedValueFile, returnedValue, 0o600); e != nil {
			t.Fatal(e)
		}
		function := "run_check(inputs, file_contents)"
		if deliversAlerts {
			function = "deliver_alert(alert, inputs, file_contents)"
		}
		script := "def " + function + ":\n    return json.decode(read_file(" + strconv.Quote(returnedValueFile) + ").content)\n"
		if e := os.WriteFile(filepath.Join(directory, ModuleScriptFile), []byte(script), 0o600); e != nil {
			t.Fatal(e)
		}
		requestFields := map[string]any{"protocol": 1, "inputs": map[string]any{}, "state_dir": directory}
		if deliversAlerts {
			requestFields["alert"] = map[string]any{}
			requestFields["file_contents"] = map[string]any{}
		}
		request, e := json.Marshal(requestFields)
		if e != nil {
			t.Fatal(e)
		}
		var written bytes.Buffer
		if e := RunStarlarkModule(context.Background(), directory, bytes.NewReader(request), &written); e != nil {
			if written.Len() != 0 {
				t.Fatalf("RunStarlarkModule failed with %q and still wrote %q", e, written.Bytes())
			}
			return
		}
		if !bytes.HasPrefix(bytes.TrimLeft(returnedValue, " \t\r\n"), []byte("{")) {
			t.Fatalf("RunStarlarkModule wrote a result for %q, which is no JSON object", returnedValue)
		}
		line, ended := bytes.CutSuffix(written.Bytes(), []byte("\n"))
		if !ended || bytes.Contains(line, []byte("\n")) {
			t.Fatalf("RunStarlarkModule wrote %q, which is not one line", written.Bytes())
		}
		var answer ModuleResult
		var destinationAnswer destinationModuleResult
		var output any = &answer
		if deliversAlerts {
			output = &destinationAnswer
		}
		decodeError := DecodeJSON(written.Bytes(), output)
		var fields map[string]json.RawMessage
		if e := json.Unmarshal(line, &fields); e != nil {
			// encoding/json reads at most 10000 levels of nesting, and the
			// value of a script can hold more. MMWatchdog then refuses the result.
			if decodeError == nil {
				t.Fatalf("DecodeJSON accepted %q, which encoding/json cannot read: %v", line, e)
			}
			return
		}
		if string(fields["protocol"]) != "1" {
			t.Fatalf("the result %s of the value %q does not state protocol 1", line, returnedValue)
		}
		if decodeError != nil {
			return
		}
		if deliversAlerts {
			if destinationAnswer.Protocol != 1 {
				t.Fatalf("the destination result chose protocol %d", destinationAnswer.Protocol)
			}
			requireStrictModuleJSON(t, written.Bytes(), &destinationModuleResult{})
			requireDestinationResultRoundTrip(t, destinationAnswer)
			return
		}
		if answer.Protocol != 1 {
			t.Fatalf("the value %q chose protocol %d", returnedValue, answer.Protocol)
		}
		if ValidateResult(answer) != nil {
			return
		}
		requireValidModuleResult(t, answer)
		requireModuleResultRoundTrip(t, answer)
	})
}

// requireValidModuleResult holds a result that ValidateResult accepted to
// the rules of a module's result in docs/reference/writing-modules.md.
func requireValidModuleResult(t *testing.T, r ModuleResult) {
	t.Helper()
	switch {
	case r.Protocol != 1:
		t.Fatalf("ValidateResult accepted protocol %d", r.Protocol)
	case r.Status != "ok" && r.Status != "failed" && r.Status != "unknown":
		t.Fatalf("ValidateResult accepted the status %q", r.Status)
	case r.Message == "":
		t.Fatal("ValidateResult accepted a result without a message")
	case len(r.Measurements) > measurementsInOneResultLimit:
		t.Fatalf("ValidateResult accepted %d measurements", len(r.Measurements))
	case len(r.Findings) > FindingsLimit:
		t.Fatalf("ValidateResult accepted %d findings", len(r.Findings))
	}
	for name, value := range r.Measurements {
		if !isMeasurementName(name) || math.IsNaN(value) || math.IsInf(value, 0) {
			t.Fatalf("ValidateResult accepted the measurement %q with the value %v", name, value)
		}
	}
	ids := map[string]bool{}
	for _, finding := range r.Findings {
		oneLineOfText := utf8.ValidString(finding.ID) && !strings.ContainsFunc(finding.ID, unicode.IsControl)
		if finding.ID == "" || len(finding.ID) > FindingIDLimit || !oneLineOfText || ids[finding.ID] || finding.Message == "" {
			t.Fatalf("ValidateResult accepted the finding %+v among %+v", finding, r.Findings)
		}
		ids[finding.ID] = true
	}
}

// isMeasurementName reports a name that a measurement can have: a letter or
// a digit, then letters, digits, underscores, and hyphens.
func isMeasurementName(name string) bool {
	if name == "" || len(name) > measurementNameLimit {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		letterOrDigit := 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
		if !letterOrDigit && (i == 0 || (c != '_' && c != '-')) {
			return false
		}
	}
	return true
}

// requireModuleResultRoundTrip writes a valid result as JSON and reads it
// back: the result stays the same and valid, and writing it again gives the
// same bytes.
func requireModuleResultRoundTrip(t *testing.T, answer ModuleResult) {
	t.Helper()
	written, e := PlainJSON(answer)
	if e != nil {
		t.Fatalf("PlainJSON cannot write the valid result %+v: %v", answer, e)
	}
	var again ModuleResult
	// Writing a result can make it longer than it was when read, such as
	// U+FFFD for each byte that is not UTF-8, so the limit is its own length.
	if e := DecodeJSONUpTo(written, &again, len(written)); e != nil {
		t.Fatalf("DecodeJSON cannot read %s, which PlainJSON wrote: %v", written, e)
	}
	if !reflect.DeepEqual(withoutEmptyLists(answer), withoutEmptyLists(again)) {
		t.Fatalf("the result %+v reads back as %+v", answer, again)
	}
	if e := ValidateResult(again); e != nil {
		t.Fatalf("the result %s is no longer valid when read back: %v", written, e)
	}
	if rewritten, e := PlainJSON(again); e != nil || !bytes.Equal(rewritten, written) {
		t.Fatalf("the result %s is written as %s the second time: %v", written, rewritten, e)
	}
}

// withoutEmptyLists makes empty measurements and findings none, which is how
// JSON with omitempty reads them back.
func withoutEmptyLists(r ModuleResult) ModuleResult {
	if len(r.Measurements) == 0 {
		r.Measurements = nil
	}
	if len(r.Findings) == 0 {
		r.Findings = nil
	}
	return r
}

// Destination results have their own wire type in ModuleSender.Send. Keep
// these tests in this package so they use that type without exporting it.
var destinationResultSeeds = []string{
	`{"protocol":1,"status":"delivered","message":"sent"}`,
	`{"protocol":1,"status":"failed","message":"HTTP 503"}`,
	`{"status":"delivered","message":"sent"}`,
	`{"protocol":1,"status":"delivered","message":"sent","measurements":{"queued_jobs":3}}`,
	`{"protocol":1,"status":"delivered","status":"failed","message":"sent"}`,
	`{"protocol":1,"status":"delivered","message":"sent"} {}`,
}

// A successfully decoded result must still reject unknown fields, duplicate
// keys, and a second JSON value. These mutations remain malformed even when
// the original uses a different letter case for its fields.
func requireStrictModuleJSON(t *testing.T, raw []byte, output any) {
	t.Helper()
	if len(raw) > primitives.ResultLimit || !json.Valid(raw) || !bytes.HasPrefix(bytes.TrimLeft(raw, " \t\r\n"), []byte("{")) {
		t.Fatal("accepted a module result outside the bounded JSON object protocol")
	}
	start := bytes.IndexByte(raw, '{')
	for _, fields := range []string{`"mmwatchdog_fuzz_unknown_field":0,`, `"protocol":1,"protocol":1,`} {
		changed := append(bytes.Clone(raw[:start+1]), fields...)
		changed = append(changed, raw[start+1:]...)
		if DecodeJSON(changed, output) == nil {
			t.Fatalf("accepted malformed module output with %s", fields)
		}
	}
	if DecodeJSON(append(bytes.Clone(raw), " {}"...), output) == nil {
		t.Fatal("accepted a second JSON value in module output")
	}
}

func requireDestinationResultRoundTrip(t *testing.T, result destinationModuleResult) {
	t.Helper()
	written, err := PlainJSON(result)
	if err != nil {
		t.Fatal(err)
	}
	var again destinationModuleResult
	if err := DecodeJSONUpTo(written, &again, len(written)); err != nil || result != again {
		t.Fatalf("destination result changed on round trip: %+v, %+v: %v", result, again, err)
	}
}
