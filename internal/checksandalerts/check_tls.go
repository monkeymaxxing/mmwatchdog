package checksandalerts

import (
	"context"
	"crypto/x509"
	"path/filepath"
	"time"

	"mmwatchdog/internal/primitives"
)

// TLS is the inputs of the tls check module, which opens a TLS connection,
// verifies the server's certificate, and fails when the certificate expires
// within FailWhenCertificateExpiresWithin.
type TLS struct {
	Address                          string   `yaml:"address" json:"address"`
	ServerName                       string   `yaml:"server_name" json:"server_name,omitempty"`
	TrustedCertificateAuthorityFile  string   `yaml:"trusted_certificate_authority_file" json:"trusted_certificate_authority_file,omitempty"`
	FailWhenCertificateExpiresWithin Duration `yaml:"fail_when_certificate_expires_within" json:"fail_when_certificate_expires_within"`
}

func (t *TLS) Validate(InputValidation) *FieldError {
	if e := HostPort(t.Address, 1); e != nil {
		return &FieldError{"address", e.Error()}
	}
	if t.FailWhenCertificateExpiresWithin == "" {
		return &FieldError{"fail_when_certificate_expires_within", "is required: how soon before certificate expiry the check fails; add fail_when_certificate_expires_within: 30d"}
	}
	if t.TrustedCertificateAuthorityFile != "" && !filepath.IsAbs(t.TrustedCertificateAuthorityFile) {
		return &FieldError{"trusted_certificate_authority_file", "must be absolute"}
	}
	return nil
}

// ValidateOnNode checks that the certificate authority file can be read
// here. The check reads it again for each run.
func (t *TLS) ValidateOnNode() *FieldError {
	if t.TrustedCertificateAuthorityFile != "" {
		if _, err := readTrustedCertificateAuthorityFile(t.TrustedCertificateAuthorityFile); err != nil {
			return &FieldError{"trusted_certificate_authority_file", err.Error()}
		}
	}
	return nil
}

func (t *TLS) Run(ctx context.Context, _ *CheckRun) Observation {
	var roots *x509.CertPool
	if t.TrustedCertificateAuthorityFile != "" {
		var err error
		if roots, err = readTrustedCertificateAuthorityFile(t.TrustedCertificateAuthorityFile); err != nil {
			return Result("unknown", err.Error())
		}
	}
	expires, err := primitives.TLSCertificateExpiry(ctx, t.Address, t.ServerName, roots)
	if err != nil {
		return Result("failed", err.Error())
	}
	left := time.Until(expires)
	o := Result("ok", "TLS certificate is valid")
	o.Measurements = map[string]Measurement{"certificate_expires_in_seconds": {left.Seconds(), "seconds"}}
	if left <= t.FailWhenCertificateExpiresWithin.Value() {
		o.Status = "failed"
		o.Message = "TLS certificate expires at " + expires.UTC().Format(time.RFC3339)
	}
	return o
}

// tlsDescription is what "mmwatchdog describe-module tls" shows, and what
// validation knows about the module's inputs.
var tlsDescription = ModuleDescription{
	Summary: "Opens a TLS connection, verifies the server's certificate, and fails when the certificate expires soon.",
	UsedAs:  UsedAsCheck,
	Inputs: []ModuleInput{
		{Name: "address", Type: "string", Required: true, Description: "The address to connect to, as host:port."},
		{Name: "server_name", Type: "string", Description: "The name that the certificate must be valid for, when it is not the host in address."},
		{Name: "trusted_certificate_authority_file", Type: "string", Description: "The absolute path of a file of PEM certificates that MMWatchdog trusts for this check in place of the system's."},
		{Name: "fail_when_certificate_expires_within", Type: "duration", Required: true, Description: "The check fails when the certificate expires within this time, such as 30d."},
	},
	Measurements: []ModuleMeasurement{{Name: "certificate_expires_in_seconds", Unit: "seconds", Description: "The time until the server's certificate expires. Use fail_when_certificate_expires_within instead of a measurement limit."}},
	Examples: []ModuleExample{{
		Description:   "The certificate of the mail server is valid for at least 14 more days.",
		Configuration: "tls:\n  address: mail.example.com:993\n  fail_when_certificate_expires_within: 14d\nalert_severity: warn\n",
	}},
}
