package checksandalerts_test

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"mmwatchdog/internal/checksandalerts"
)

func TestSentTokensAndHeartbeatTokens(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "token")
	for _, token := range []string{"x", strings.Repeat("a", 15), strings.Repeat("b", 16), strings.Repeat("c", 4096), "", strings.Repeat("d", 4097), "first\nsecond"} {
		os.WriteFile(path, []byte(token+"\n"), 0600)
		valid := len(token) > 0 && len(token) <= 4096 && !strings.Contains(token, "\n")
		got, err := checksandalerts.ReadBearerToken(path)
		if (err == nil) != valid || valid && got != token {
			t.Fatalf("sent token of %d bytes: %q %v", len(token), got, err)
		}
		_, err = checksandalerts.ReadHeartbeatToken(path)
		if (err == nil) != (valid && len(token) >= 16) {
			t.Fatalf("heartbeat token of %d bytes: %v", len(token), err)
		}
	}
}

func TestWebhookCertificatePolicyAndQuietVerification(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer x" {
			t.Error("missing token")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	dir := t.TempDir()
	token, authority := filepath.Join(dir, "token"), filepath.Join(dir, "authority.pem")
	os.WriteFile(token, []byte("x"), 0600)
	os.WriteFile(authority, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0644)
	input := &checksandalerts.Webhook{URL: server.URL + "/secret-path", BearerTokenFile: token}
	destination := checksandalerts.Destination{Inputs: input, TimeLimitPerDeliveryAttempt: "1m"}
	delivery := checksandalerts.DeliveryOf{Destination: destination}
	if result := delivery.VerifyDestination(context.Background()); result.AcceptsAddressAndCredential {
		t.Fatal(result)
	}
	if err := (checksandalerts.WebhookSender{Delivery: delivery}).Send(context.Background(), checksandalerts.Event{}); err == nil {
		t.Fatal("untrusted certificate accepted")
	}
	input.TrustedCertificateAuthorityFile = authority
	result := delivery.VerifyDestination(context.Background())
	if !result.AcceptsAddressAndCredential || !strings.Contains(result.VerificationMessage, "token were not verified") || requests.Load() != 0 {
		t.Fatal(result, requests.Load())
	}
	if err := (checksandalerts.WebhookSender{Delivery: delivery}).Send(context.Background(), checksandalerts.Event{}); err != nil {
		t.Fatal(err)
	}
	input.TrustedCertificateAuthorityFile = ""
	input.DangerouslySkipServerCertificateCheck = true
	if err := (checksandalerts.WebhookSender{Delivery: delivery}).Send(context.Background(), checksandalerts.Event{}); err != nil {
		t.Fatal(err)
	}
	os.Chmod(token, 0644)
	// A mode change on a running node must not drop deliveries.
	if err := (checksandalerts.WebhookSender{Delivery: delivery}).Send(context.Background(), checksandalerts.Event{}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 3 {
		t.Fatal(requests.Load())
	}
}

func TestCheckReadsExposedCredentialWithoutDroppingRun(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer x" {
			t.Error("missing short HTTP bearer token")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "token")
	os.WriteFile(path, []byte("x"), 0600)
	os.Chmod(path, 0644)
	input := &checksandalerts.HTTP{URL: server.URL, BearerTokenFile: path}
	if err := input.Validate(checksandalerts.InputValidation{Authored: func(string) bool { return false }}); err != nil {
		t.Fatal(err)
	}
	check := checksandalerts.Check{Inputs: input}
	observation := checksandalerts.ObserveCheck(context.Background(), check)
	if observation.Status != "ok" || requests.Load() != 1 {
		t.Fatal(observation, requests.Load())
	}
}

func TestDestinationCertificateInputsAreValidated(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		webhook checksandalerts.Webhook
		field   string
	}{
		{checksandalerts.Webhook{URL: "https://example.com", TrustedCertificateAuthorityFile: "relative.pem"}, "trusted_certificate_authority_file"},
		{checksandalerts.Webhook{URL: "http://example.com", TrustedCertificateAuthorityFile: "/etc/mmwatchdog/ca.pem"}, "url"},
		{checksandalerts.Webhook{URL: "http://example.com", DangerouslySkipServerCertificateCheck: true}, "url"},
	} {
		err := test.webhook.Validate(checksandalerts.InputValidation{})
		if err == nil || err.Path != test.field {
			t.Fatal(err)
		}
	}
	mmhotline := &checksandalerts.MMHotlineDestination{}
	for _, test := range []struct {
		access checksandalerts.SecretFileAccess
		field  string
	}{
		{checksandalerts.SecretFileAccess{SecretReadableByGroup: true}, "secret_readable_by_group"},
		{checksandalerts.SecretFileAccess{DangerouslyLetEveryAccountReadSecret: true}, "dangerously_let_every_account_read_secret"},
	} {
		err := mmhotline.ValidateDestination(checksandalerts.Destination{SecretFileAccess: test.access})
		if err == nil || err.Path != test.field || !strings.Contains(err.Message, "inside the mmhotline mapping") {
			t.Fatal(err)
		}
	}
}

func TestDestinationAuthoritiesHaveTheirOwnFileSizeBound(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	authority := filepath.Join(t.TempDir(), "authorities.pem")
	bundle := strings.Repeat(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})), 128)
	if len(bundle) <= 65536 || len(bundle) > 1024*1024 {
		t.Fatal(len(bundle))
	}
	os.WriteFile(authority, []byte(bundle), 0644)
	for _, destination := range []checksandalerts.Destination{
		{Inputs: &checksandalerts.Email{TrustedCertificateAuthorityFile: authority}},
		{Inputs: &checksandalerts.Webhook{TrustedCertificateAuthorityFile: authority}},
	} {
		contents, input, err := destination.ReadCredentialFiles()
		if err != nil || input != "" || contents["trusted_certificate_authority_file"] != bundle {
			t.Fatal(input, err)
		}
		if err := destination.CredentialFilesTooLarge(contents); err != nil {
			t.Fatal(err)
		}
		if err := destination.CredentialFilesTooLarge(map[string]string{"trusted_certificate_authority_file": strings.Repeat("x", 1024*1024+1)}); err == nil {
			t.Fatal("oversized certificate authority file accepted")
		}
	}
	module := checksandalerts.Destination{Inputs: &checksandalerts.ModuleCall{}}
	if err := module.CredentialFilesTooLarge(map[string]string{"trusted_certificate_authority_file": bundle}); err == nil {
		t.Fatal("a module's file bypassed its size bound")
	}
}
