package checksandalerts_test

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"testing"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/testhelpers"
)

func TestScriptDestinationVerificationReadsCredentialsAndSendsNoMessage(t *testing.T) {
	t.Parallel()
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for _, hasHook := range []bool{true, false} {
		directory := t.TempDir()
		script := "def deliver_alert(alert, inputs, file_contents):\n    fail(\"verification must never deliver an alert\")\n"
		if !hasHook {
			script = "fail(\"a module without verification must not execute\")\n" + script
		}
		if hasHook {
			script += "\ndef verify_destination(inputs, file_contents):\n    return (inputs[\"api_token_file\"].endswith(\"/token\") and file_contents[\"api_token_file\"] == \"the-verification-secret\", \"verified address and credential\")\n"
		}
		testhelpers.InstallStarlarkModule(t, directory, "verification_example", testhelpers.TestModuleDescription("verification_example", checksandalerts.UsedAsAlertDestination, map[string]string{"api_token_file": "string"}, nil), script)
		path := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(path, []byte("the-verification-secret"), 0600); err != nil {
			t.Fatal(err)
		}
		catalog, err := checksandalerts.LoadModuleCatalog(checksandalerts.ModuleDirs{External: directory, OfficialExtra: filepath.Join(directory, "absent")})
		if err != nil {
			t.Fatal(err)
		}
		call := &checksandalerts.ModuleCall{Name: "verification_example", Inputs: map[string]any{"api_token_file": path}, ExecOptions: primitives.ExecOptions{RunAsUserAccount: account.Username, RunFromDirectory: "/"}}
		if problem := call.Validate(checksandalerts.InputValidation{Modules: catalog, UsedAs: checksandalerts.UsedAsAlertDestination, Authored: func(string) bool { return true }}); problem != nil {
			t.Fatal(problem)
		}
		result := (checksandalerts.DeliveryOf{Destination: checksandalerts.Destination{Inputs: call, TimeLimitPerDeliveryAttempt: "1m", RemindEvery: "never"}}).VerifyDestination(context.Background())
		if result.CanVerifyWithoutMessage != hasHook || result.AcceptsAddressAndCredential != hasHook {
			t.Fatal(result)
		}
		if hasHook && result.VerificationMessage != "verified address and credential" {
			t.Fatal(result)
		}
		if !hasHook && result.VerificationMessage != checksandalerts.TestedOnlyByAMessage {
			t.Fatal(result)
		}
	}
}
