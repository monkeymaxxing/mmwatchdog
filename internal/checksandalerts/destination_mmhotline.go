package checksandalerts

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// MMHotlineDestination is the inputs of the mmhotline destination module,
// which sends alerts to one MMHotline alert source. The connection to
// MMHotline is a part of MMWatchdog of its own; mmhotline.go signs requests
// and verifies receipts.
type MMHotlineDestination struct {
	SecretFileAccess  `yaml:",inline"`
	URL               string `yaml:"mmhotline_url" json:"mmhotline_url"`
	AlertSourceID     string `yaml:"mmhotline_alert_source_id" json:"mmhotline_alert_source_id"`
	SigningSecretFile string `yaml:"mmhotline_signing_secret_file" json:"mmhotline_signing_secret_file"`
	// AlertSourceForNodesSendingWithoutTheHub is the optional second alert
	// source, under which a hub's joined nodes send what they deliver
	// without the hub. MMHotline merges repeated warnings of that source
	// only. Left out (nil), it is not in the fleet settings, so their
	// version stays the version of a fleet without it.
	AlertSourceForNodesSendingWithoutTheHub *MMHotlineAlertSource `yaml:"alert_source_for_nodes_sending_without_the_hub" json:"alert_source_for_nodes_sending_without_the_hub,omitempty"`
}

// MMHotlineAlertSource is the second alert source of an mmhotline
// destination: its ID, its signing secret file, and MMHotline's address
// when the nodes reach MMHotline at another one than the hub.
type MMHotlineAlertSource struct {
	URL               string `yaml:"mmhotline_url" json:"mmhotline_url,omitempty"`
	AlertSourceID     string `yaml:"mmhotline_alert_source_id" json:"mmhotline_alert_source_id"`
	SigningSecretFile string `yaml:"mmhotline_signing_secret_file" json:"mmhotline_signing_secret_file"`
}

// The name of the second alert source's block, and of its signing secret
// file among the destination's credential files.
const (
	MMHotlineNodeAlertSourceBlock       = "alert_source_for_nodes_sending_without_the_hub"
	mmhotlineNodeSigningSecretFileInput = MMHotlineNodeAlertSourceBlock + ".mmhotline_signing_secret_file"
)

// AlertSourceNamed returns the inputs that send under the alert source id:
// the destination's second alert source, with the destination's address
// unless the block states its own, or else the destination's own alert
// source; and the name of the credential input that holds its signing
// secret. Any other id, such as that of a second source removed since an
// alert was queued, is the destination's own alert source.
func (p *MMHotlineDestination) AlertSourceNamed(id string) (MMHotlineDestination, string) {
	if b := p.AlertSourceForNodesSendingWithoutTheHub; b != nil && id == b.AlertSourceID {
		source := MMHotlineDestination{URL: p.URL, AlertSourceID: b.AlertSourceID, SigningSecretFile: b.SigningSecretFile}
		if b.URL != "" {
			source.URL = b.URL
		}
		return source, mmhotlineNodeSigningSecretFileInput
	}
	return MMHotlineDestination{URL: p.URL, AlertSourceID: p.AlertSourceID, SigningSecretFile: p.SigningSecretFile}, "mmhotline_signing_secret_file"
}

// NodeAlertSourceID is the ID of the second alert source, or "" without
// one.
func (p *MMHotlineDestination) NodeAlertSourceID() string {
	if b := p.AlertSourceForNodesSendingWithoutTheHub; b != nil {
		return b.AlertSourceID
	}
	return ""
}

// MMHotline returns the inputs of an mmhotline destination, or nil for a
// destination with another module.
func (d Destination) MMHotline() *MMHotlineDestination {
	p, _ := d.Inputs.(*MMHotlineDestination)
	return p
}

func (p *MMHotlineDestination) Validate(InputValidation) *FieldError {
	if e := validateMMHotlineAlertSource("", p.URL, p.AlertSourceID, p.SigningSecretFile); e != nil {
		return e
	}
	b := p.AlertSourceForNodesSendingWithoutTheHub
	if b == nil {
		return nil
	}
	address := b.URL
	if address == "" {
		address = p.URL
	}
	if e := validateMMHotlineAlertSource(MMHotlineNodeAlertSourceBlock+".", address, b.AlertSourceID, b.SigningSecretFile); e != nil {
		return e
	}
	// Reason: the block gives the nodes a second alert source; without the
	// block they already send under the destination's own.
	if b.AlertSourceID == p.AlertSourceID {
		return &FieldError{MMHotlineNodeAlertSourceBlock + ".mmhotline_alert_source_id", "must differ from the destination's own mmhotline_alert_source_id: the block gives the nodes that send without the hub an alert source of their own; leave the block out to have them send under the destination's alert source"}
	}
	return nil
}

// validateMMHotlineAlertSource checks the address, the alert source ID, and
// the signing secret file of one alert source, whose inputs are below
// prefix.
func validateMMHotlineAlertSource(prefix, mmhotlineURL, alertSourceID, signingSecretFile string) *FieldError {
	address, err := url.Parse(mmhotlineURL)
	if err != nil || (address.Scheme != "http" && address.Scheme != "https") || address.Hostname() == "" || address.User != nil || address.RawQuery != "" || address.ForceQuery || strings.Contains(mmhotlineURL, "#") {
		return &FieldError{prefix + "mmhotline_url", "expected MMHotline's address: http:// or https://, a host, and an optional port and path, such as http://127.0.0.1:8484"}
	}
	path := strings.TrimRight(address.Path, "/")
	if strings.Contains(path, "/alert-intake/") || strings.Contains(path, "/check-in/") || strings.HasSuffix(path, "/alert-intake") || strings.HasSuffix(path, "/check-in") {
		return &FieldError{prefix + "mmhotline_url", "write MMHotline's address without the route, such as http://10.20.0.1:8484; MMWatchdog adds /alert-intake/{alert source ID} itself"}
	}
	if !MMHotlineIDPattern.MatchString(alertSourceID) {
		return &FieldError{prefix + "mmhotline_alert_source_id", "expected the alert_source_id of an alert source in MMHotline's settings file: 1 to 64 letters, digits, _ and -, such as monitoring"}
	}
	if !filepath.IsAbs(signingSecretFile) {
		return &FieldError{prefix + "mmhotline_signing_secret_file", "must be absolute"}
	}
	return nil
}

// ValidateDestination refuses the two settings that MMHotline covers by
// itself.
func (p *MMHotlineDestination) ValidateDestination(d Destination) *FieldError {
	if d.SecretReadableByGroup {
		return &FieldError{"secret_readable_by_group", "the signing secret belongs to mmhotline; move secret_readable_by_group: true inside the mmhotline mapping"}
	}
	if d.DangerouslyLetEveryAccountReadSecret {
		return &FieldError{"dangerously_let_every_account_read_secret", "the signing secret belongs to mmhotline; move dangerously_let_every_account_read_secret: true inside the mmhotline mapping"}
	}
	if d.RemindEvery != "" {
		return &FieldError{"remind_every", "an mmhotline destination takes no remind_every: MMHotline repeats and escalates on its own"}
	}
	if d.SendOnlyTheAlertTitle {
		return &FieldError{"send_only_the_alert_title", "an mmhotline destination takes no send_only_the_alert_title: MMHotline puts the title in its notifications on its own, and shows the body on the alert's page"}
	}
	return nil
}

func (p *MMHotlineDestination) sameTarget(other destinationInputs) bool {
	receiver := other.(*MMHotlineDestination)
	return p.AlertSourceID == receiver.AlertSourceID
}

func (p *MMHotlineDestination) CredentialFiles() map[string]string {
	files := map[string]string{"mmhotline_signing_secret_file": p.SigningSecretFile}
	if b := p.AlertSourceForNodesSendingWithoutTheHub; b != nil {
		files[mmhotlineNodeSigningSecretFileInput] = b.SigningSecretFile
	}
	return files
}

// ReadCredentialFile returns the signing secret as the file writes it: 64
// hex characters.
func (p *MMHotlineDestination) ReadCredentialFile(_, path string) (string, error) {
	secret, e := ReadMMHotlineSecret(path)
	return hex.EncodeToString(secret), e
}

func (p *MMHotlineDestination) ValidCredential(_, secret string) bool {
	_, e := DecodeSigningSecret(secret)
	return e == nil
}

func (p *MMHotlineDestination) Sender(delivery DeliveryOf) NotificationSender {
	return mmhotlineSender{delivery}
}

// mmhotlineSender delivers a queued MMHotline alert. A joined node's copy of
// the fleet settings supplies the signing secret; otherwise it is read from
// the secret file for each delivery.
type mmhotlineSender struct{ delivery DeliveryOf }

func (s mmhotlineSender) Send(ctx context.Context, event Event) error {
	if event.MMHotlineAlert == nil {
		return fmt.Errorf("queued without an MMHotline alert")
	}
	destination := s.delivery.Destination
	source, input := destination.MMHotline().AlertSourceNamed(event.MMHotlineAlertSource)
	written, e := s.delivery.Credential(input)
	if e != nil {
		return e
	}
	secret, e := DecodeSigningSecret(written)
	if e != nil {
		return e
	}
	_, e = s.delivery.MMHotline.Send(ctx, "alert-intake", source, secret, event.MMHotlineAlert.Encode(), destination.TimeLimitPerDeliveryAttempt.Value())
	return e
}

// mmhotlineDescription is what "mmwatchdog describe-module mmhotline" shows, and what
// validation knows about the module's inputs.
var mmhotlineDescription = ModuleDescription{
	Summary: "Sends each alert to MMHotline, the on-call program that a team runs on its own server.",
	UsedAs:  UsedAsAlertDestination,
	Inputs: []ModuleInput{
		{Name: "secret_readable_by_group", Type: "boolean", Description: "With true, allow the file group to read the signing secret."},
		{Name: "dangerously_let_every_account_read_secret", Type: "boolean", Description: "With true, allow every account to read the signing secret."},
		{Name: "mmhotline_url", Type: "string", Required: true, Description: "MMHotline's alert intake address, http:// or https://, without an alert-intake or check-in route."},
		{Name: "mmhotline_alert_source_id", Type: "string", Required: true, Description: "The alert_source_id of the alert source in MMHotline's settings file: 1 to 64 letters, digits, _ and -, such as monitoring."},
		{Name: "mmhotline_signing_secret_file", Type: "string", Required: true, Description: "The absolute path of the file that holds the signing secret. MMWatchdog reads it for each delivery."},
		{Name: MMHotlineNodeAlertSourceBlock, Type: "mapping", Description: "On a fleet hub, a second alert source for the joined nodes that send alerts without the hub: mmhotline_alert_source_id, mmhotline_signing_secret_file, and mmhotline_url when the nodes reach MMHotline at another address. Joined nodes get its signing secret with the fleet settings, and need MMWatchdog 0.2.0 or later."},
	},
	Examples: []ModuleExample{{
		Description:   "Send every alert to the team's MMHotline alert source.",
		Configuration: "mmhotline:\n  mmhotline_url: http://10.20.0.1:8484\n  mmhotline_alert_source_id: mmwatchdog_fleet_hub\n  mmhotline_signing_secret_file: /etc/mmwatchdog/secrets/mmhotline-signing-secret\nalert_severities: [info, warn, sos]\n",
	}},
}

// MMHotlineAlert is MMHotline's generic signed alert.
type MMHotlineAlert struct {
	SenderAlertID     string `json:"sender_alert_id"`
	SenderIncidentKey string `json:"sender_incident_key,omitempty"`
	Severity          string `json:"severity"`
	Title             string `json:"title"`
	Message           string `json:"message"`
	HappenedAt        string `json:"happened_at,omitempty"`
}

// Encode is the alert's request body, without escaping <, >, and &, which
// only HTML needs.
func (a *MMHotlineAlert) Encode() []byte { return EncodeWithoutHTMLEscaping(a) }

// EncodeWithoutHTMLEscaping encodes v as JSON without escaping <, >, and &,
// which only HTML needs, so a body holds more within MMHotline's limits.
func EncodeWithoutHTMLEscaping(v any) []byte {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}
