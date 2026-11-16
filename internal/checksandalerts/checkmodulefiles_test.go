package checksandalerts_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/testhelpers"
)

func TestCheckModuleReceivesFileContentsAndRedactsResults(t *testing.T) {
	t.Parallel()
	for _, script := range []bool{false, true} {
		t.Run(fmt.Sprint(script), func(t *testing.T) {
			modules := t.TempDir()
			secret := "check-module-secret"
			path := filepath.Join(t.TempDir(), "secret")
			if err := os.WriteFile(path, []byte(secret+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			description := testhelpers.TestModuleDescription("credentials", checksandalerts.UsedAsCheck, map[string]string{"connection_string_file": "string"}, nil, "one_incident_for_each_finding: true")
			cases := []struct{ source, status string }{
				{`return {"status":"failed", "message":secret, "findings":[{"id":secret,"message":secret}]}`, "failed"},
				{`fail(secret)`, "unknown"},
			}
			for _, tc := range cases {
				if script {
					source := "def run_check(inputs, file_contents):\n    secret = file_contents[\"connection_string_file\"]\n    if secret != \"" + secret + "\":\n        fail(\"wrong file content\")\n    print(secret)\n    " + tc.source + "\n"
					testhelpers.InstallStarlarkModule(t, modules, "credentials", description, source)
				} else {
					result := `{"protocol":1,"status":"failed","message":"` + secret + `","findings":[{"id":"` + secret + `","message":"` + secret + `"}]}`
					source := "cat > " + testhelpers.ShellQuote(filepath.Join(modules, "request.json")) + "\nprintf '%s' " + testhelpers.ShellQuote(secret) + " >&2\n"
					if tc.status == "unknown" {
						source += "exit 2\n"
					} else {
						source += "printf '%s' " + testhelpers.ShellQuote(result) + "\n"
					}
					testhelpers.InstallModule(t, modules, "credentials", description, source)
				}
				check := testhelpers.ModuleCheck(t, modules, "credentials", map[string]any{"connection_string_file": path})
				observation := checksandalerts.ObserveCheck(context.Background(), check)
				if !script {
					received, err := os.ReadFile(filepath.Join(modules, "request.json"))
					if err != nil {
						t.Fatal(err)
					}
					var request struct {
						FileContents map[string]string `json:"file_contents"`
						Inputs       map[string]any    `json:"inputs"`
					}
					if err := json.Unmarshal(received, &request); err != nil {
						t.Fatal(err)
					}
					if request.FileContents["connection_string_file"] != secret || request.Inputs["connection_string_file"] != path {
						t.Fatalf("incorrect request: %s", received)
					}
				}

				encoded, _ := json.Marshal(observation)
				if observation.Status != tc.status || strings.Contains(string(encoded), secret) || !strings.Contains(string(encoded), "content of connection_string_file") {
					t.Fatalf("got %s", encoded)
				}
			}
			for _, value := range []any{"relative/secret", 5, ""} {
				call := checkCallForFile(t, modules, value)
				if call == nil || call.Path != "connection_string_file" || !strings.Contains(call.Message, "absolute file path") {
					t.Fatalf("%v: %+v", value, call)
				}
			}
		})
	}
}

func checkCallForFile(t *testing.T, modules string, path any) *checksandalerts.FieldError {
	t.Helper()
	catalog, err := checksandalerts.LoadModuleCatalog(checksandalerts.ModuleDirs{External: modules, OfficialExtra: filepath.Join(modules, "absent")})
	if err != nil {
		t.Fatal(err)
	}
	call := &checksandalerts.ModuleCall{Name: "credentials", Inputs: map[string]any{"connection_string_file": path}, ExecOptions: testhelpers.OwnOptions(t)}
	return call.Validate(checksandalerts.InputValidation{Modules: catalog, UsedAs: checksandalerts.UsedAsCheck})
}

func TestVerifyConnectionSendsNoRequestAndChecksCertificate(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) })
	plain := httptest.NewServer(handler)
	defer plain.Close()
	secure := httptest.NewTLSServer(handler)
	defer secure.Close()
	for _, tc := range []struct {
		url      string
		accepted bool
	}{
		{plain.URL + "/webhook-secret?token=hidden", true},
		{secure.URL + "/webhook-secret", false},
		{"http://" + testhelpers.AddressWhereNothingListens(t, "tcp") + "/webhook-secret", false},
		{"not a URL/webhook-secret", false},
	} {
		directory := t.TempDir()
		source := "def run_check(inputs, file_contents):\n    result = verify_connection(inputs[\"url\"])\n    return {\"status\": \"ok\", \"message\": json.encode([result.error == \"\", result.error])}\n"
		if err := os.WriteFile(filepath.Join(directory, "module.star"), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		request, _ := json.Marshal(map[string]any{"protocol": 1, "inputs": map[string]any{"url": tc.url}, "file_contents": map[string]any{}})
		var out bytes.Buffer
		if err := checksandalerts.RunStarlarkModule(context.Background(), directory, bytes.NewReader(request), &out); err != nil {
			t.Fatal(err)
		}
		var result checksandalerts.ModuleResult
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		var answer []any
		if err := json.Unmarshal([]byte(result.Message), &answer); err != nil {
			t.Fatal(err)
		}
		if answer[0] != tc.accepted || strings.Contains(answer[1].(string), "webhook-secret") {
			t.Fatal(result.Message)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("connection verification sent an HTTP request")
	}
}

func TestInputValidationCannotVerifyAConnection(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	source := "def run_check(inputs, file_contents):\n    return {\"status\": \"ok\", \"message\": \"unused\"}\n\ndef validate_inputs(inputs):\n    verify_connection(\"https://example.com\")\n    return None\n"
	testhelpers.InstallStarlarkModule(t, directory, "pure_validation", testhelpers.TestModuleDescription("pure_validation", checksandalerts.UsedAsCheck, nil, nil), source)
	catalog, err := checksandalerts.LoadModuleCatalog(checksandalerts.ModuleDirs{External: directory, OfficialExtra: filepath.Join(directory, "absent")})
	if err != nil {
		t.Fatal(err)
	}
	call := &checksandalerts.ModuleCall{Name: "pure_validation", ExecOptions: testhelpers.OwnOptions(t)}
	problem := call.Validate(checksandalerts.InputValidation{Modules: catalog, UsedAs: checksandalerts.UsedAsCheck})
	if problem == nil || !strings.Contains(problem.Message, "verify_connection cannot be called during input validation") {
		t.Fatal(problem)
	}
}

func TestModuleDiagnosticsRedactJSONEscapedSecrets(t *testing.T) {
	t.Parallel()
	contents := map[string]string{"password_file": "secret\"with\nline&<>", "token_file": "xyz"}
	encoded, _ := json.Marshal(contents["password_file"])
	plain, _ := checksandalerts.PlainJSON(contents["password_file"])
	diagnostic := "invalid result: " + string(encoded) + " " + string(plain) + " xyz"
	redacted := checksandalerts.WithoutFileContents(diagnostic, contents)
	if strings.Contains(redacted, "secret") || strings.Contains(redacted, "xyz") || !strings.Contains(redacted, "[content of password_file]") || !strings.Contains(redacted, "[content of token_file]") {
		t.Fatal(redacted)
	}
}

func TestCheckModuleFilesAreCheckedBeforeStartingTheModule(t *testing.T) {
	t.Parallel()
	modules := t.TempDir()
	inputTypes := map[string]string{}
	inputs := map[string]any{}
	for index := range 5 {
		name := fmt.Sprintf("secret_%d_file", index)
		inputTypes[name] = "string"
		path := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(path, bytes.Repeat([]byte("s"), checksandalerts.ModuleFileLimit), 0600); err != nil {
			t.Fatal(err)
		}
		inputs[name] = path
	}
	testhelpers.InstallStarlarkModule(t, modules, "file_limits", testhelpers.TestModuleDescription("file_limits", checksandalerts.UsedAsCheck, inputTypes, nil), "def run_check(inputs, file_contents):\n    fail(\"the module must not start\")\n")
	check := testhelpers.ModuleCheck(t, modules, "file_limits", inputs)
	call := check.Inputs.(*checksandalerts.ModuleCall)
	if problem := call.ValidateOnNode(); problem == nil || !strings.Contains(problem.Message, "file contents") {
		t.Fatalf("oversized file contents accepted by runtime validation: %+v", problem)
	}
	if observation := checksandalerts.ObserveCheck(context.Background(), check); observation.Status != "unknown" || !strings.Contains(observation.Message, "file contents") {
		t.Fatalf("oversized files reached the module: %+v", observation)
	}
	call.Inputs["secret_0_file"] = filepath.Join(t.TempDir(), "missing")
	if problem := call.ValidateOnNode(); problem == nil || problem.Path != "secret_0_file" || !strings.Contains(problem.Message, "missing") {
		t.Fatalf("missing file accepted by runtime validation: %+v", problem)
	}
	if observation := checksandalerts.ObserveCheck(context.Background(), check); observation.Status != "unknown" || !strings.Contains(observation.Message, "missing") {
		t.Fatalf("missing file reached the module: %+v", observation)
	}
}
