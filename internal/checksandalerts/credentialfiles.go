package checksandalerts

import (
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"

	"mmwatchdog/internal/primitives"
)

// The files that a check or a destination names for a secret or for
// certificates, and how MMWatchdog reads each. A file is read anew for each use, so
// a secret that is replaced at the same path needs no reload.

// ReadBearerToken reads a token that MMWatchdog sends to another service.
func ReadBearerToken(path string) (string, error) { return readToken(path, 1) }

// ReadHeartbeatToken reads a token that MMWatchdog checks. Sixteen bytes protect
// the heartbeat receiver against guessed tokens.
func ReadHeartbeatToken(path string) (string, error) { return readToken(path, 16) }

func readToken(path string, minimum int) (string, error) {
	b, err := primitives.ReadFile(path, 4098)
	token := strings.TrimSpace(string(b))
	if errors.Is(err, primitives.ErrFileLargerThanLimit) || (err == nil && (len(token) < minimum || len(token) > 4096 || strings.ContainsAny(token, "\r\n"))) {
		return "", fmt.Errorf("%s: expected a single token of %d..4096 bytes", path, minimum)
	}
	if err != nil {
		return "", err
	}
	return token, nil
}

// readBasicAuth reads one user:password line for HTTP basic authentication.
func readBasicAuth(path string) (string, string, error) {
	b, e := primitives.ReadFile(path, 4096)
	line := strings.TrimSpace(string(b))
	user, password, ok := strings.Cut(line, ":")
	if errors.Is(e, primitives.ErrFileLargerThanLimit) || (e == nil && (!ok || user == "" || strings.ContainsAny(line, "\r\n"))) {
		return "", "", fmt.Errorf("%s: expected one user:password line of at most 4096 bytes", path)
	}
	if e != nil {
		return "", "", e
	}
	return user, password, nil
}

// passwordLimit is the most bytes a password may have; the error of
// ReadPassword states the number.
const passwordLimit = 1024

// ReadPassword reads the password of a login, such as the one of a mail
// server or of a database: the one line of the file, with its spaces kept,
// since a space can belong to a password.
func ReadPassword(path string) (string, error) {
	// The limit leaves room for the line break at the end of the line.
	b, e := primitives.ReadFile(path, passwordLimit+2)
	if e != nil && !errors.Is(e, primitives.ErrFileLargerThanLimit) {
		return "", e
	}
	password := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if e != nil || !validPassword(password) {
		return "", fmt.Errorf("%s: expected one line with the password: UTF-8 text of at most 1,024 bytes, without a tab or another control character", path)
	}
	return password, nil
}

// validPassword accepts one line of 1 to passwordLimit bytes.
func validPassword(password string) bool {
	return password != "" && PlainText(password, passwordLimit)
}

// IsCertificateAuthorityFileInput identifies public trust bundles in core and
// custom modules. These files are read for use, but are neither secrets nor
// part of a destination's address and credential identity.
func IsCertificateAuthorityFileInput(input string) bool {
	return strings.HasSuffix(input, "certificate_authority_file")
}

// SecretFileAccess is the explicit permission policy of one check,
// destination, or node setting block. Only read bits expose a secret.
type SecretFileAccess struct {
	SecretReadableByGroup                bool `yaml:"secret_readable_by_group" json:"secret_readable_by_group,omitempty"`
	DangerouslyLetEveryAccountReadSecret bool `yaml:"dangerously_let_every_account_read_secret" json:"dangerously_let_every_account_read_secret,omitempty"`
}

// ValidateSecretFile reports secret permissions for validation and warnings.
// The reader reports a missing file with its normal read error.
func (access SecretFileAccess) ValidateSecretFile(path string) error {
	mode, readers, settings, err := access.secretFileReaders(path)
	if err != nil || len(readers) == 0 {
		return err
	}
	return fmt.Errorf("%s has permissions mode %04o: %s can read the secret; run chmod 600 %s, or add %s to the owning check, destination, or setting block to allow that access", path, mode, strings.Join(readers, " and "), path, strings.Join(settings, " and "))
}

// ValidateOwnTokenFile reports the permissions of a token file that belongs
// to no check, destination, or setting block, such as the token file of
// "mmwatchdog heartbeat". Such a file has no block that could allow wider
// access, so the only fix is the file mode.
func ValidateOwnTokenFile(path string) error {
	mode, readers, _, err := SecretFileAccess{}.secretFileReaders(path)
	if err != nil || len(readers) == 0 {
		return err
	}
	return fmt.Errorf("%s has permissions mode %04o: %s can read the token; run chmod 600 %s so that only the account of this job can read it", path, mode, strings.Join(readers, " and "), path)
}

func (access SecretFileAccess) secretFileReaders(path string) (mode uint32, readers, settings []string, err error) {
	about, err := primitives.InformationAboutFile(path)
	if os.IsNotExist(err) {
		return 0, nil, nil, nil
	}
	if err != nil {
		return 0, nil, nil, err
	}
	if !about.IsRegularFile {
		return 0, nil, nil, fmt.Errorf("%s: expected a regular file holding the secret", path)
	}
	if about.PermissionsMode&0004 != 0 && !access.DangerouslyLetEveryAccountReadSecret {
		readers = append(readers, "every account")
		settings = append(settings, "dangerously_let_every_account_read_secret: true")
	}
	if about.PermissionsMode&0040 != 0 && !access.SecretReadableByGroup {
		readers = append(readers, "the file's group")
		settings = append(settings, "secret_readable_by_group: true")
	}
	return about.PermissionsMode, readers, settings, nil
}

// certificateAuthorityFileLimit is the most that a file of certificate
// authorities holds.
const certificateAuthorityFileLimit = 1024 * 1024

// readTrustedCertificateAuthorityFile reads the PEM certificates that replace the
// system trust store for one check.
func readTrustedCertificateAuthorityFile(path string) (*x509.CertPool, error) {
	b, e := primitives.ReadFile(path, certificateAuthorityFileLimit)
	if e != nil {
		return nil, e
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("%s: contains no PEM certificates", path)
	}
	return roots, nil
}

// destinationCertificateAuthorities uses PEM content carried with fleet
// settings, or read on this node, as a replacement trust store.
func destinationCertificateAuthorities(delivery DeliveryOf, path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	content, err := delivery.Credential("trusted_certificate_authority_file")
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(content)) {
		return nil, fmt.Errorf("%s: contains no PEM certificates", path)
	}
	return roots, nil
}

func readDestinationCertificateAuthorities(path string) (string, error) {
	b, err := primitives.ReadFile(path, certificateAuthorityFileLimit)
	if err != nil {
		return "", err
	}
	if !validCertificateAuthorities(string(b)) {
		return "", fmt.Errorf("%s: contains no PEM certificates", path)
	}
	return string(b), nil
}

func validCertificateAuthorities(content string) bool {
	return x509.NewCertPool().AppendCertsFromPEM([]byte(content))
}
