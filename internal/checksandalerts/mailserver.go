package checksandalerts

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"

	"mmwatchdog/internal/primitives"
)

// What the smtp, imap, and pop3 check modules share: how a check names its
// mail server, and the connection on which the three protocols exchange
// lines of text.

// MailServer is the inputs that the three modules share. Address is the
// server, written as host:port. ConnectionSecurity is required, as it is
// for an email destination, so a reader sees on the check how the
// connection is protected: starttls, implicit_tls, or unencrypted.
type MailServer struct {
	DangerouslySkipServerCertificateCheck bool   `yaml:"dangerously_skip_server_certificate_check" json:"dangerously_skip_server_certificate_check,omitempty"`
	Address                               string `yaml:"address" json:"address"`
	ConnectionSecurity                    string `yaml:"connection_security" json:"connection_security"`
	TrustedCertificateAuthorityFile       string `yaml:"trusted_certificate_authority_file" json:"trusted_certificate_authority_file,omitempty"`
}

func (m *MailServer) Validate(InputValidation) *FieldError {
	if e := HostPort(m.Address, 1); e != nil {
		return &FieldError{"address", e.Error()}
	}
	switch m.ConnectionSecurity {
	case SMTPStartTLS, SMTPImplicitTLS:
	case SMTPUnencrypted:
		if m.TrustedCertificateAuthorityFile != "" {
			return &FieldError{"trusted_certificate_authority_file", "has no use with connection_security: unencrypted, which verifies no certificate"}
		}
	default:
		return &FieldError{"connection_security", "required: starttls, implicit_tls, or unencrypted"}
	}
	if m.TrustedCertificateAuthorityFile != "" && !filepath.IsAbs(m.TrustedCertificateAuthorityFile) {
		return &FieldError{"trusted_certificate_authority_file", "must be absolute"}
	}
	return nil
}

// ValidateOnNode checks that the certificate authority file can be read
// here. The check reads it again for each run.
func (m *MailServer) ValidateOnNode() *FieldError {
	if m.TrustedCertificateAuthorityFile != "" {
		if _, err := readTrustedCertificateAuthorityFile(m.TrustedCertificateAuthorityFile); err != nil {
			return &FieldError{"trusted_certificate_authority_file", err.Error()}
		}
	}
	return nil
}

// mailServerInputs are the descriptions of the three shared inputs. protocol
// is the name of the protocol for the texts.
func mailServerInputs(protocol string) []ModuleInput {
	return []ModuleInput{
		{Name: "address", Type: "string", Required: true, Description: "The address of the " + protocol + " server, as host:port."},
		DangerouslySkipServerCertificateCheckInput,
		{Name: "connection_security", Type: "string", Required: true, AllowedValues: []any{SMTPStartTLS, SMTPImplicitTLS, SMTPUnencrypted},
			Description: "How the connection is protected: starttls starts without encryption and switches to it, implicit_tls is encrypted from the first byte, and unencrypted is never encrypted."},
		{Name: "trusted_certificate_authority_file", Type: "string", Description: "The absolute path of a file of PEM certificates that MMWatchdog trusts for this check in place of the system's."},
	}
}

// mailConversationLimit is the most that MMWatchdog reads from a mail server in one
// run of a check.
const mailConversationLimit = 64 * 1024

// mailConversation is one connection to a mail server on which a check and
// the server exchange lines of text.
type mailConversation struct {
	ctx    context.Context
	conn   net.Conn
	lines  *bufio.Reader
	secure *tls.Config
	stop   func() bool
}

// open connects to the mail server, with encryption from the first byte for
// implicit_tls. A problem is the observation of the check: unknown for a
// certificate authority file that cannot be read, and failed for a server
// that cannot be reached.
func (m *MailServer) open(ctx context.Context) (*mailConversation, *Observation) {
	problem := func(status, message string) (*mailConversation, *Observation) {
		o := Result(status, message)
		return nil, &o
	}
	host, _, _ := net.SplitHostPort(m.Address)
	secure := primitives.ServerTLSConfig(nil, m.DangerouslySkipServerCertificateCheck)
	secure.ServerName, secure.MinVersion = host, tls.VersionTLS12
	if m.TrustedCertificateAuthorityFile != "" {
		roots, err := readTrustedCertificateAuthorityFile(m.TrustedCertificateAuthorityFile)
		if err != nil {
			return problem("unknown", err.Error())
		}
		secure.RootCAs = roots
	}
	conn, err := primitives.ConnectTCP(ctx, m.Address)
	if err != nil {
		return problem("failed", connectionError(ctx, m.Address, err))
	}
	c := &mailConversation{ctx: ctx, conn: conn, secure: secure}
	// Closing the connection ends a wait for the server when the time limit
	// of the run passes.
	c.stop = context.AfterFunc(ctx, func() { conn.Close() })
	c.read(conn)
	if m.ConnectionSecurity == SMTPImplicitTLS {
		if err := c.startTLS(); err != nil {
			c.close()
			return problem("failed", primitives.DetailLine("the encrypted connection to "+m.Address+" did not start: "+err.Error()))
		}
	}
	return c, nil
}

// read reads the lines of the server from conn, up to the limit of a mail
// conversation. A server that sends more fails with errReadLimit.
func (c *mailConversation) read(conn net.Conn) {
	c.conn, c.lines = conn, bufio.NewReader(&primitives.ReadLimitedConnection{Conn: conn, Remaining: mailConversationLimit})
}

// startTLS switches the connection to encryption and verifies the server's
// certificate for the host of the check's address.
func (c *mailConversation) startTLS() error {
	encrypted := tls.Client(c.conn, c.secure)
	if err := encrypted.HandshakeContext(c.ctx); err != nil {
		return err
	}
	c.read(encrypted)
	return nil
}

func (c *mailConversation) close() {
	c.stop()
	c.conn.Close()
}

// send writes one line to the server.
func (c *mailConversation) send(line string) error {
	_, err := io.WriteString(c.conn, line+"\r\n")
	return c.ended(err)
}

// line reads one line from the server, without the line break at its end.
func (c *mailConversation) line() (string, error) {
	line, err := c.lines.ReadString('\n')
	if err != nil {
		if err == io.EOF {
			err = fmt.Errorf("the server closed the connection")
		}
		return "", c.ended(err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// ended returns the reason why an exchange stopped: the time limit of the
// run when that has passed, and err otherwise.
func (c *mailConversation) ended(err error) error {
	if err != nil && c.ctx.Err() != nil {
		return fmt.Errorf("no answer within the time limit of the run")
	}
	return err
}
