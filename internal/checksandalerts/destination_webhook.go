package checksandalerts

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"

	"mmwatchdog/internal/primitives"
)

// Webhook is the inputs of the webhook destination module, which posts each
// alert event as JSON to a URL. The rules are in docs/changing-mmwatchdog/design/alerts.md
// under "Webhook".
type Webhook struct {
	TrustedCertificateAuthorityFile       string `yaml:"trusted_certificate_authority_file" json:"trusted_certificate_authority_file,omitempty"`
	DangerouslySkipServerCertificateCheck bool   `yaml:"dangerously_skip_server_certificate_check" json:"dangerously_skip_server_certificate_check,omitempty"`
	URL                                   string `yaml:"url" json:"url"`
	BearerTokenFile                       string `yaml:"bearer_token_file" json:"bearer_token_file,omitempty"`
}

func (w *Webhook) Validate(InputValidation) *FieldError {
	if !httpURL(w.URL) {
		return &FieldError{"url", "expected HTTP(S) URL without credentials and with a host, such as https://example.com/health"}
	}
	if w.TrustedCertificateAuthorityFile != "" && !filepath.IsAbs(w.TrustedCertificateAuthorityFile) {
		return &FieldError{"trusted_certificate_authority_file", "must be absolute"}
	}
	if strings.HasPrefix(w.URL, "http://") && (w.TrustedCertificateAuthorityFile != "" || w.DangerouslySkipServerCertificateCheck) {
		return &FieldError{"url", "HTTP checks no server certificate; use https:// or remove trusted_certificate_authority_file and dangerously_skip_server_certificate_check"}
	}
	if w.BearerTokenFile != "" && !filepath.IsAbs(w.BearerTokenFile) {
		return &FieldError{"bearer_token_file", "must be absolute"}
	}
	return nil
}

func (w *Webhook) sameTarget(other destinationInputs) bool { return w.URL == other.(*Webhook).URL }

func (w *Webhook) CredentialFiles() map[string]string {
	files := map[string]string{}
	if w.BearerTokenFile != "" {
		files["bearer_token_file"] = w.BearerTokenFile
	}
	if w.TrustedCertificateAuthorityFile != "" {
		files["trusted_certificate_authority_file"] = w.TrustedCertificateAuthorityFile
	}
	return files
}

func (w *Webhook) ReadCredentialFile(input, path string) (string, error) {
	if input == "trusted_certificate_authority_file" {
		return readDestinationCertificateAuthorities(path)
	}
	return ReadBearerToken(path)
}

func (w *Webhook) ValidCredential(input, content string) bool {
	if input == "trusted_certificate_authority_file" {
		return validCertificateAuthorities(content)
	}
	return len(content) >= 1 && len(content) <= 4096 && strings.TrimSpace(content) == content && !strings.ContainsAny(content, "\r\n")
}

// VerifyDestination connects without making an HTTP request. A successful
// result proves neither that the URL path exists nor that the token works.
func (w *Webhook) VerifyDestination(ctx context.Context, delivery DeliveryOf) (bool, string) {
	roots, err := destinationCertificateAuthorities(delivery, w.TrustedCertificateAuthorityFile)
	if err != nil {
		return false, err.Error()
	}
	address, err := url.Parse(w.URL)
	if err != nil {
		return false, "invalid webhook URL"
	}
	port := address.Port()
	if port == "" {
		port = "80"
		if address.Scheme == "https" {
			port = "443"
		}
	}
	connection, err := primitives.ConnectTCP(ctx, net.JoinHostPort(address.Hostname(), port))
	if err != nil {
		return false, err.Error()
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	if address.Scheme == "https" {
		secure := primitives.ServerTLSConfig(roots, w.DangerouslySkipServerCertificateCheck)
		secure.ServerName, secure.MinVersion = address.Hostname(), tls.VersionTLS12
		if err := tls.Client(connection, secure).HandshakeContext(ctx); err != nil {
			return false, err.Error()
		}
	}
	return true, "verified the connection and server certificate policy only; the webhook path and bearer token were not verified, and no HTTP request was sent"
}

func (w *Webhook) Sender(delivery DeliveryOf) NotificationSender { return WebhookSender{delivery} }

// WebhookSender posts an alert to a webhook. A joined node's copy of the
// fleet settings supplies the token; otherwise it is read from the token
// file for each delivery.
type WebhookSender struct{ Delivery DeliveryOf }

func (s WebhookSender) Send(ctx context.Context, event Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, e := json.Marshal(event)
	if e != nil {
		return e
	}
	webhook := s.Delivery.Destination.Inputs.(*Webhook)
	headers := map[string]string{"Content-Type": "application/json", "Idempotency-Key": event.ID}
	if webhook.BearerTokenFile != "" {
		token, e := s.Delivery.Credential("bearer_token_file")
		if e != nil {
			return e
		}
		headers["Authorization"] = "Bearer " + token
	}
	ctx, cancel := context.WithTimeout(ctx, s.Delivery.Destination.TimeLimitPerDeliveryAttempt.Value())
	defer cancel()
	roots, e := destinationCertificateAuthorities(s.Delivery, webhook.TrustedCertificateAuthorityFile)
	if e != nil {
		return e
	}
	response, e := primitives.HTTPRequest(ctx, primitives.HTTPRequestToSend{Method: "POST", URL: webhook.URL, CertificateAuthorities: roots, DangerouslySkipServerCertificateCheck: webhook.DangerouslySkipServerCertificateCheck, Headers: headers, Body: b, ReadsResponseBody: func(int) bool { return false }})
	if e != nil {
		return fmt.Errorf("MMWatchdog could not post the alert to the webhook: %w", e)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("webhook returned HTTP %d", response.StatusCode)
	}
	return nil
}

// WebhookDescription is what "mmwatchdog describe-module webhook" shows, and what
// validation knows about the module's inputs.
var WebhookDescription = ModuleDescription{
	Summary: "Posts each alert event as JSON to a web address.",
	UsedAs:  UsedAsAlertDestination,
	Inputs: []ModuleInput{
		DangerouslySkipServerCertificateCheckInput,
		{Name: "trusted_certificate_authority_file", Type: "string", Description: "The absolute path of PEM certificates that replace the system certificate authorities for this webhook."},
		{Name: "url", Type: "string", Required: true, Description: "The address that takes the alerts, with http:// or https://, without a user name or a password in it."},
		{Name: "bearer_token_file", Type: "string", Description: "The absolute path of a file that holds a bearer token. MMWatchdog reads it for each delivery and sends it in the Authorization header."},
	},
	Examples: []ModuleExample{{
		Description:   "Send every alert to a receiver of the team's own.",
		Configuration: "webhook:\n  url: https://alerts.example.com/mmwatchdog\n  bearer_token_file: /etc/mmwatchdog/secrets/webhook\nalert_severities: [info, warn, sos]\nremind_every: never\n",
	}},
}
