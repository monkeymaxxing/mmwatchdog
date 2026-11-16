package checksandalerts_test

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/testhelpers"
)

// Every destination module says which of its inputs name a credential
// file. MMWatchdog reads each file for a delivery; on a joined node the content
// comes from the node's copy of the fleet settings, and the node never opens
// the hub's path on its own disk.
func TestADestinationReadsItsCredentialFilesOrTakesTheHubsCopy(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	file := func(name, content string) string {
		t.Helper()
		path := filepath.Join(directory, name)
		if e := os.WriteFile(path, []byte(content), 0600); e != nil {
			t.Fatal(e)
		}
		return path
	}
	secret := hex.EncodeToString(testhelpers.TestSigningSecret)
	for _, tc := range []struct {
		name, input, content string
		destination          func(path string) checksandalerts.Destination
	}{
		{"webhook", "bearer_token_file", "a-webhook-token-of-16-bytes", func(path string) checksandalerts.Destination {
			return checksandalerts.Destination{RemindEvery: "never", Inputs: &checksandalerts.Webhook{URL: "https://example.com", BearerTokenFile: path}}
		}},
		{"email", "login_password_file", "hunter2", func(path string) checksandalerts.Destination {
			return checksandalerts.Destination{RemindEvery: "never", Inputs: &checksandalerts.Email{SMTPServer: "smtp.example.com:587", LoginUser: "mmwatchdog", LoginPasswordFile: path}}
		}},
		{"mmhotline", "mmhotline_signing_secret_file", secret, func(path string) checksandalerts.Destination {
			return checksandalerts.Destination{Inputs: &checksandalerts.MMHotlineDestination{URL: "https://hotline.example.com", AlertSourceID: testhelpers.TestSourceID, SigningSecretFile: path}}
		}},
		{"slack", "webhook_url_file", "https://hooks.slack.example/T000/B000/XXXX", func(path string) checksandalerts.Destination {
			return checksandalerts.Destination{RemindEvery: "never", Inputs: &checksandalerts.ModuleCall{Name: "slack", Inputs: map[string]any{"webhook_url_file": path, "mention": "<@U024BE7LH>"}}}
		}},
	} {
		path := file(tc.name, tc.content+"\n")
		destination := tc.destination(path)
		if files := destination.Inputs.CredentialFiles(); len(files) != 1 || files[tc.input] != path {
			t.Errorf("%s: the credential files are %v", tc.name, files)
		}
		contents, _, e := destination.ReadCredentialFiles()
		if e != nil || len(contents) != 1 || contents[tc.input] != tc.content || !destination.Inputs.ValidCredential(tc.input, tc.content) {
			t.Errorf("%s: read %v %v", tc.name, contents, e)
		}
		if got, e := (checksandalerts.DeliveryOf{Destination: destination}).Credential(tc.input); e != nil || got != tc.content {
			t.Errorf("%s: a delivery read %q %v", tc.name, got, e)
		}
		// A joined node holds the content and has no such file.
		joined := tc.destination(filepath.Join(directory, "only-on-the-hub"))
		if got, e := (checksandalerts.DeliveryOf{Destination: joined, FileContents: map[string]string{tc.input: "from the hub"}}).Credential(tc.input); e != nil || got != "from the hub" {
			t.Errorf("%s: a delivery on a joined node read %q %v", tc.name, got, e)
		}
		// A file that cannot be read is an error that names its input, whose
		// path in the configuration is below the module's name.
		_, input, e := joined.ReadCredentialFiles()
		if e == nil || input != tc.input || joined.PathOfInput(input) != tc.name+"."+tc.input {
			t.Errorf("%s: a missing file is reported for the input %q at %q: %v", tc.name, input, joined.PathOfInput(input), e)
		}
	}
	// A destination without a credential file has none to read.
	for name, destination := range map[string]checksandalerts.Destination{
		"webhook": {RemindEvery: "never", Inputs: &checksandalerts.Webhook{URL: "https://example.com"}},
		"email":   {RemindEvery: "never", Inputs: &checksandalerts.Email{SMTPServer: "localhost:25"}},
		"module":  {RemindEvery: "never", Inputs: &checksandalerts.ModuleCall{Name: "own_alarm", Inputs: map[string]any{"alarm_system_url": "https://alarm.example.com"}}},
		"none":    {RemindEvery: "never"},
	} {
		if contents, _, e := destination.ReadCredentialFiles(); e != nil || len(contents) != 0 {
			t.Errorf("%s: %v %v", name, contents, e)
		}
	}
	// The files of one destination hold at most 64 KiB together.
	inputs := map[string]any{}
	for _, name := range []string{"a_file", "b_file", "c_file", "d_file", "e_file"} {
		inputs[name] = file(name, strings.Repeat("k", checksandalerts.ModuleFileLimit))
	}
	tooLarge := checksandalerts.Destination{RemindEvery: "never", Inputs: &checksandalerts.ModuleCall{Name: "own_alarm", Inputs: inputs}}
	_, input, e := tooLarge.ReadCredentialFiles()
	if e == nil || input != "" || tooLarge.PathOfInput(input) != "own_alarm" || !strings.Contains(e.Error(), "limit of 65536 bytes for one destination") {
		t.Errorf("files that are too large together are reported for the input %q: %v", input, e)
	}
}

// A destination that was written as JSON reads back exactly: a queued
// delivery and a joined node's copy of the fleet settings depend on it. A
// whole number that a float cannot hold stays that number, so the version of
// the fleet settings is the same on the node. What MMWatchdog never wrote is
// refused.
func TestADestinationReadsBackFromJSONExactly(t *testing.T) {
	t.Parallel()
	modules := t.TempDir()
	testhelpers.InstallStarlarkModule(t, modules, "own_alarm", testhelpers.TestModuleDescription("own_alarm", checksandalerts.UsedAsAlertDestination, map[string]string{"chat_id": "integer", "retries": "integer", "ratio": "number"}, nil), "def deliver_alert(alert, inputs, file_contents):\n    return {\"status\": \"delivered\", \"message\": \"sent\"}\n")
	directory := testhelpers.ConfigFile(t, "external_module_dir: "+modules+"\ncheck_groups: {a: {shell: 'true'}}")
	destinations := "destinations:\n  own_alarm:\n    remind_every: never\n    own_alarm: {chat_id: 1234567890123456789, retries: 3, ratio: 0.5}\n    alert_severities: [sos]\n"
	if e := os.WriteFile(filepath.Join(directory, "alert_destinations.yml"), []byte(destinations), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := config.Load(directory)
	if e != nil {
		t.Fatal(e)
	}
	c.FleetHubRole = &config.FleetHubRole{FleetTimings: config.DefaultFleetTimings}
	settings, e := fleet.NewFleetSettings(c)
	if e != nil {
		t.Fatal(e)
	}
	written, e := json.Marshal(settings)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(written), `"chat_id":1234567890123456789`) {
		t.Fatalf("the hub writes the number as %s", written)
	}
	// A joined node checks the copy with its own modules. A node that has
	// no module own_alarm, or no module that is not core at all, takes the
	// hub's destination too: its inputs were the hub's to check.
	for name, modulesOfTheNode := range map[string]*checksandalerts.ModuleCatalog{"with the module": c.Modules, "without the module": testhelpers.ModulesIn(t, t.TempDir()), "without any module": nil} {
		var fetched fleet.FleetSettings
		if e := checksandalerts.DecodeJSONUpTo(written, &fetched, fleet.ReportLimit); e != nil {
			t.Fatal(e)
		}
		if e := fetched.Validate(modulesOfTheNode); e != nil || fetched.Version != settings.Version || !checksandalerts.SameDestination(fetched.Destinations["own_alarm"], settings.Destinations["own_alarm"]) {
			t.Fatalf("a joined node %s reads the fleet settings as %+v: %v", name, fetched.Destinations["own_alarm"].Inputs, e)
		}
		if has := fetched.Destinations["own_alarm"].ModuleKind() == checksandalerts.ModuleKindProjectStarlark; has != (name == "with the module") {
			t.Errorf("a joined node %s knows the kind of the module as %q", name, fetched.Destinations["own_alarm"].ModuleKind())
		}
	}
	for name, tc := range map[string]struct{ written, wantError string }{
		"two modules":                 {`{"remind_every":"never","webhook":{"url":"https://example.com"},"email":{"smtp_server":"localhost:25"},"alert_severities":["sos"]}`, "a destination names exactly one module"},
		"a member that is no setting": {`{"remind_every":"never","webhook":{"url":"https://example.com"},"alert_severities":["sos"],"mention-text":"x"}`, `unknown field "mention-text"`},
		"an input that is none":       {`{"remind_every":"never","webhook":{"url":"https://example.com","token":"x"},"alert_severities":["sos"]}`, `unknown field "token"`},
		"no object":                   {`["webhook"]`, "cannot unmarshal array"},
	} {
		var read checksandalerts.Destination
		if e := json.Unmarshal([]byte(tc.written), &read); e == nil || !strings.Contains(e.Error(), tc.wantError) {
			t.Errorf("%s: got %v, want %q", name, e, tc.wantError)
		}
	}
}

// Two destinations with the same name are the same destination while their
// module, their target, and their credential contents stay as they are. The
// hub's queue on a joined node has no module.
func TestTheIdentityOfADestination(t *testing.T) {
	t.Parallel()
	webhook := checksandalerts.Destination{RemindEvery: "never", Inputs: &checksandalerts.Webhook{URL: "https://example.com", BearerTokenFile: "/etc/mmwatchdog/secrets/webhook"}, TimeLimitPerDeliveryAttempt: "5s", AlertSeverities: []string{"sos"}}
	changedSettings := webhook
	changedSettings.TimeLimitPerDeliveryAttempt, changedSettings.RemindEvery, changedSettings.AlertSeverities, changedSettings.SendOnlyTheAlertTitle = "30s", "1h", testhelpers.AllSeverities, true
	changedSettings.Inputs = &checksandalerts.Webhook{URL: "https://example.com", BearerTokenFile: "/etc/mmwatchdog/secrets/webhook"}
	changedSettings.SecretReadableByGroup = true
	changedSettings.DangerouslyLetEveryAccountReadSecret = true
	changedSettings.Inputs.(*checksandalerts.Webhook).TrustedCertificateAuthorityFile = "/etc/mmwatchdog/certificates.pem"
	changedSettings.Inputs.(*checksandalerts.Webhook).DangerouslySkipServerCertificateCheck = true
	otherURL, otherFile, otherModule := webhook, webhook, webhook
	otherURL.Inputs = &checksandalerts.Webhook{URL: "https://example.org", BearerTokenFile: "/etc/mmwatchdog/secrets/webhook"}
	otherFile.Inputs = &checksandalerts.Webhook{URL: "https://example.com", BearerTokenFile: "/etc/mmwatchdog/secrets/webhook-2"}
	otherModule.Inputs = &checksandalerts.Email{SMTPServer: "smtp.example.com:587"}
	for name, tc := range map[string]struct {
		a, b checksandalerts.Destination
		same bool
	}{
		"other settings":         {webhook, changedSettings, true},
		"another URL":            {webhook, otherURL, false},
		"another token file":     {webhook, otherFile, true},
		"another module":         {webhook, otherModule, false},
		"the hub's queue":        {checksandalerts.Destination{RemindEvery: "never"}, checksandalerts.Destination{RemindEvery: "never"}, true},
		"a module and no module": {webhook, checksandalerts.Destination{RemindEvery: "never"}, false},
	} {
		if checksandalerts.SameDestination(tc.a, tc.b) != tc.same || checksandalerts.SameDestination(tc.b, tc.a) != tc.same {
			t.Errorf("%s: sameDestination is %v", name, !tc.same)
		}
	}
}

func TestFleetDestinationCredentialsAreComparedWithoutSendingTheirFingerprint(t *testing.T) {
	t.Parallel()
	destination := checksandalerts.Destination{Inputs: &checksandalerts.Webhook{URL: "https://example.com", BearerTokenFile: "/etc/mmwatchdog/secrets/token"}, RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, TimeLimitPerDeliveryAttempt: "5s"}
	contents := map[string]string{"bearer_token_file": "first-credential"}
	destination.CaptureCredentialContent(contents)
	settings := fleet.FleetSettings{Destinations: map[string]checksandalerts.Destination{"chat": destination}, FileContentsByDestination: map[string]map[string]string{"chat": contents}, Fleet: config.DefaultFleetTimings, AlertSeverityWhenTheHubGoesSilent: "sos"}
	settings.Version = settings.ComputeVersion()
	encoded, err := json.Marshal(settings)
	if err != nil || strings.Contains(string(encoded), "credential_content_fingerprint") || strings.Contains(string(encoded), destination.CredentialContentFingerprint) {
		t.Fatal("fleet settings exposed the credential fingerprint", err)
	}
	var received fleet.FleetSettings
	if err := json.Unmarshal(encoded, &received); err != nil {
		t.Fatal(err)
	}
	if err := received.Validate(nil); err != nil {
		t.Fatal(err)
	}
	if !checksandalerts.SameDestination(destination, received.Destinations["chat"]) {
		t.Fatal("the joined node lost the original receiver identity")
	}
	changed := destination
	changed.CaptureCredentialContent(map[string]string{"bearer_token_file": "rotated-credential"})
	if !checksandalerts.SameDestination(changed, received.Destinations["chat"]) {
		t.Fatal("a new bearer token for the same URL discarded the receiver identity")
	}
}

// A new token or password for the same address keeps the queued alerts; a new
// address does not; a credential of a module that is not core can name the
// receiver, so it still counts.
func TestRotatingASenderCredentialKeepsTheQueue(t *testing.T) {
	t.Parallel()
	capture := func(d checksandalerts.Destination, content map[string]string) checksandalerts.Destination {
		d.CaptureCredentialContent(content)
		return d
	}
	webhook := checksandalerts.Destination{RemindEvery: "never", Inputs: &checksandalerts.Webhook{URL: "https://example.com", BearerTokenFile: "/secrets/token"}}
	otherURL := webhook
	otherURL.Inputs = &checksandalerts.Webhook{URL: "https://example.org", BearerTokenFile: "/secrets/token"}
	email := checksandalerts.Destination{RemindEvery: "never", Inputs: &checksandalerts.Email{SMTPServer: "smtp.example.com:587", LoginUser: "u", LoginPasswordFile: "/secrets/smtp"}}
	pushover := checksandalerts.Destination{RemindEvery: "never", Inputs: &checksandalerts.ModuleCall{Name: "pushover", Inputs: map[string]any{"user_key_file": "/secrets/key"}}}
	for name, tc := range map[string]struct {
		a, b checksandalerts.Destination
		same bool
	}{
		"webhook token rotated":   {capture(webhook, map[string]string{"bearer_token_file": "old"}), capture(webhook, map[string]string{"bearer_token_file": "new"}), true},
		"webhook URL changed":     {capture(webhook, map[string]string{"bearer_token_file": "old"}), capture(otherURL, map[string]string{"bearer_token_file": "old"}), false},
		"email password rotated":  {capture(email, map[string]string{"login_password_file": "old"}), capture(email, map[string]string{"login_password_file": "new"}), true},
		"extra module credential": {capture(pushover, map[string]string{"user_key_file": "old"}), capture(pushover, map[string]string{"user_key_file": "new"}), false},
	} {
		if checksandalerts.SameDestination(tc.a, tc.b) != tc.same {
			t.Errorf("%s: SameDestination is %v", name, !tc.same)
		}
	}
}
