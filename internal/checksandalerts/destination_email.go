package checksandalerts

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"mmwatchdog/internal/primitives"
)

// The email destination. The rules are in docs/changing-mmwatchdog/design/alerts.md under
// "Email".

// The values of smtp_connection_security.
const (
	SMTPStartTLS    = "starttls"
	SMTPImplicitTLS = "implicit_tls"
	SMTPUnencrypted = "unencrypted"
)

// SMTPRootCAs replaces the system trust store for mail servers in tests
// only; nil uses the system trust store.
var SMTPRootCAs *x509.CertPool

// Email is the inputs of the email destination module, which sends each
// alert as one email through a mail server. SMTPConnectionSecurity is required, so a reader sees on the
// destination how the connection is protected. LoginUser and LoginPasswordFile are
// given together, or not at all for a mail server that needs no login.
type Email struct {
	TrustedCertificateAuthorityFile       string   `yaml:"trusted_certificate_authority_file" json:"trusted_certificate_authority_file,omitempty"`
	DangerouslySkipServerCertificateCheck bool     `yaml:"dangerously_skip_server_certificate_check" json:"dangerously_skip_server_certificate_check,omitempty"`
	SMTPServer                            string   `yaml:"smtp_server" json:"smtp_server"`
	SMTPConnectionSecurity                string   `yaml:"smtp_connection_security" json:"smtp_connection_security"`
	From                                  string   `yaml:"from" json:"from"`
	To                                    []string `yaml:"to" json:"to"`
	LoginUser                             string   `yaml:"login_user" json:"login_user,omitempty"`
	LoginPasswordFile                     string   `yaml:"login_password_file" json:"login_password_file,omitempty"`
}

func (m *Email) Validate(InputValidation) *FieldError {
	if field, message := m.Problem(); message != "" {
		return &FieldError{field, message}
	}
	return nil
}

func (m *Email) sameTarget(other destinationInputs) bool { return sameEmail(m, other.(*Email)) }

func (m *Email) CredentialFiles() map[string]string {
	files := map[string]string{}
	if m.LoginPasswordFile != "" {
		files["login_password_file"] = m.LoginPasswordFile
	}
	if m.TrustedCertificateAuthorityFile != "" {
		files["trusted_certificate_authority_file"] = m.TrustedCertificateAuthorityFile
	}
	return files
}

func (m *Email) ReadCredentialFile(input, path string) (string, error) {
	if input == "trusted_certificate_authority_file" {
		return readDestinationCertificateAuthorities(path)
	}
	return ReadPassword(path)
}

func (m *Email) ValidCredential(input, content string) bool {
	if input == "trusted_certificate_authority_file" {
		return validCertificateAuthorities(content)
	}
	return validPassword(content)
}

func (m *Email) VerifyDestination(ctx context.Context, delivery DeliveryOf) (bool, string) {
	if err := (EmailSender{delivery}).conversation(ctx, Event{}, true); err != nil {
		return false, err.Error()
	}
	return true, "the mail server accepted the sender, every recipient, and any configured login; no message was sent"
}

func (m *Email) Sender(delivery DeliveryOf) NotificationSender { return EmailSender{delivery} }

// Problem names the first setting of the email destination that is wrong,
// and says what is wrong with it.
func (m Email) Problem() (field, message string) {
	host, _, e := net.SplitHostPort(m.SMTPServer)
	if e != nil || host == "" || HostPort(m.SMTPServer, 1) != nil {
		return "smtp_server", "expected the mail server's host name and port, such as smtp.example.com:587"
	}
	switch m.SMTPConnectionSecurity {
	case SMTPStartTLS, SMTPImplicitTLS, SMTPUnencrypted:
	default:
		return "smtp_connection_security", "required: starttls, implicit_tls, or unencrypted"
	}
	const address = "expected one email address in ASCII, such as mmwatchdog@example.com"
	if !emailAddress(m.From) {
		return "from", address
	}
	if len(m.To) == 0 {
		return "to", "expected a list of at least one email address, such as [oncall@example.com]"
	}
	for i, to := range m.To {
		field := fmt.Sprintf("to.%d", i)
		if !emailAddress(to) {
			return field, address
		}
		if slices.Contains(m.To[:i], to) {
			return field, to + " is listed twice"
		}
	}
	switch {
	case m.LoginUser != "" && m.LoginPasswordFile == "":
		return "login_password_file", "required with login_user: the file that holds the password of " + m.LoginUser
	case m.LoginUser == "" && m.LoginPasswordFile != "":
		return "login_user", "required with login_password_file: the name that MMWatchdog logs in to the mail server with"
	case !PlainText(m.LoginUser, 256):
		return "login_user", "expected one line of text of at most 256 bytes"
	case m.LoginPasswordFile != "" && !filepath.IsAbs(m.LoginPasswordFile):
		return "login_password_file", "must be absolute"
	}
	if m.TrustedCertificateAuthorityFile != "" && !filepath.IsAbs(m.TrustedCertificateAuthorityFile) {
		return "trusted_certificate_authority_file", "must be absolute"
	}
	if m.SMTPConnectionSecurity == SMTPUnencrypted {
		if m.TrustedCertificateAuthorityFile != "" {
			return "trusted_certificate_authority_file", "has no use with smtp_connection_security: unencrypted, which verifies no certificate; remove trusted_certificate_authority_file or use starttls or implicit_tls"
		}
		if m.DangerouslySkipServerCertificateCheck {
			return "dangerously_skip_server_certificate_check", "has no use with smtp_connection_security: unencrypted, which checks no certificate; remove dangerously_skip_server_certificate_check or use starttls or implicit_tls"
		}
	}

	return "", ""
}

// emailAddress accepts one plain address, such as mmwatchdog@example.com, without a
// display name or angle brackets and in ASCII: MMWatchdog writes the address as it
// is into the SMTP commands and into the headers of the message.
func emailAddress(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	a, e := mail.ParseAddress(s)
	return e == nil && a.Name == "" && a.Address == s && len(s) <= 254 //nolint:nilaway // mail.ParseAddress returns a nonnil address on success; the short-circuit error guard runs before field access.
}

// sameEmail compares the identity of two email destinations: the mail
// server and the recipients in any order. The password proves who sends,
// so a new password keeps the identity (CaptureCredentialContent).
func sameEmail(a, b *Email) bool {
	if a == nil || b == nil {
		return a == b
	}
	recipients := func(m *Email) []string {
		to := slices.Clone(m.To)
		slices.Sort(to)
		return to
	}
	return a.SMTPServer == b.SMTPServer && slices.Equal(recipients(a), recipients(b))
}

// EmailSender sends an alert as one email through the destination's mail
// server. A joined node's copy of the fleet settings supplies the password;
// otherwise it is read from the password file for each delivery. With the
// destination's send_only_the_alert_title as it was when the alert was
// queued, the email has the title and no text.
type EmailSender struct{ Delivery DeliveryOf }

func (s EmailSender) Send(ctx context.Context, event Event) error {
	return s.conversation(ctx, event, false)
}

func (s EmailSender) conversation(ctx context.Context, event Event, verifyOnly bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m := s.Delivery.Destination.Inputs.(*Email)
	password := ""
	if m.LoginPasswordFile != "" {
		var e error
		if password, e = s.Delivery.Credential("login_password_file"); e != nil {
			return e
		}
	}
	limit := s.Delivery.Destination.TimeLimitPerDeliveryAttempt.Value()
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	// step says what MMWatchdog was doing with the mail server when an attempt
	// failed.
	step := "connecting"
	// problem is the error of an attempt that failed. When the mail server
	// answered a command with a refusal, the error says what the mail server
	// refused and gives its answer. For any other error, such as one of the
	// connection, the error says which step MMWatchdog was at.
	problem := func(refused string, e error) error {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return fmt.Errorf("the mail server %s did not finish within %s (time_limit_per_delivery_attempt); MMWatchdog was %s", m.SMTPServer, limit, step)
		case ctx.Err() != nil:
			return ctx.Err()
		}
		var answer *textproto.Error
		if errors.As(e, &answer) {
			return fmt.Errorf("the mail server %s refused %s: %s", m.SMTPServer, refused, smtpAnswer(e))
		}
		return fmt.Errorf("the conversation with the mail server %s failed while MMWatchdog was %s: %s", m.SMTPServer, step, smtpAnswer(e))
	}
	host, _, _ := net.SplitHostPort(m.SMTPServer)
	roots, e := destinationCertificateAuthorities(s.Delivery, m.TrustedCertificateAuthorityFile)
	if e != nil {
		return e
	}
	if roots == nil {
		roots = SMTPRootCAs
	}
	secure := primitives.ServerTLSConfig(roots, m.DangerouslySkipServerCertificateCheck)
	secure.ServerName, secure.MinVersion = host, tls.VersionTLS12
	network, e := primitives.ConnectTCP(ctx, m.SMTPServer)
	if e != nil {
		return problem("the connection", e)
	}
	// Closing the network connection ends a command that waits for the mail
	// server when the time limit passes or the daemon stops.
	stop := context.AfterFunc(ctx, func() { network.Close() })
	defer stop()
	conn := network
	if m.SMTPConnectionSecurity == SMTPImplicitTLS {
		step = "starting the encrypted connection"
		encrypted := tls.Client(network, secure)
		if e = encrypted.HandshakeContext(ctx); e != nil {
			network.Close()
			return problem("the encrypted connection", e)
		}
		conn = encrypted
	}
	step = "waiting for the mail server's greeting"
	// Go's SMTP client takes each answer of the server into memory, so it
	// reads through a limit: no mail server needs more to take one message.
	client, e := smtp.NewClient(&primitives.ReadLimitedConnection{Conn: conn, Remaining: smtpAnswersLimit}, host)
	if e != nil {
		conn.Close()
		return problem("the connection", e)
	}
	defer client.Close()
	step = "greeting the mail server"
	if e = client.Hello(smtpHelloName()); e != nil {
		return problem("MMWatchdog's greeting", e)
	}
	if m.SMTPConnectionSecurity == SMTPStartTLS {
		step = "switching to encryption"
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("the mail server %s does not offer STARTTLS, which smtp_connection_security: starttls requires", m.SMTPServer)
		}
		if e = client.StartTLS(secure); e != nil {
			return problem("the switch to encryption", e)
		}
	}
	if m.LoginUser != "" {
		step = "logging in"
		_, offered := client.Extension("AUTH")
		login, e := smtpLogin(m.LoginUser, password, offered)
		if e != nil {
			return fmt.Errorf("the mail server %s %w", m.SMTPServer, e)
		}
		if e = client.Auth(login); e != nil {
			return problem("the login of "+m.LoginUser, e)
		}
	}
	step = "naming the sender and the recipients"
	if e = smtpMailFrom(client, m.From); e != nil {
		return problem("the sender "+m.From, e)
	}
	var refused []error
	accepted := 0
	for _, to := range m.To {
		if e = client.Rcpt(to); e != nil {
			refused = append(refused, problem("the recipient "+to, e))
			var answer *textproto.Error
			if !errors.As(e, &answer) {
				return errors.Join(refused...)
			}
		} else {
			accepted++
		}
	}
	if verifyOnly || accepted == 0 {
		_ = client.Quit()
		return errors.Join(refused...)
	}
	step = "sending the message"
	w, e := client.Data()
	if e == nil {
		_, e = w.Write(emailMessage(*m, event, s.Delivery.SendOnlyTheAlertTitle, time.Now()))
	}
	// Closing the message is the point where the mail server accepts it.
	if e == nil {
		e = w.Close() //nolint:nilaway // smtp.Client.Data returns a writer on success; a Data error skips Write and remains nonnil at the Close guard.
	}
	if e != nil {
		return errors.Join(append(refused, problem("the message", e))...)
	}
	// The mail server has the message; how the conversation ends changes
	// nothing about that.
	_ = client.Quit()
	return errors.Join(refused...)
}

// smtpMailFrom names the sender of a message. The message is ASCII in every
// line, so the command asks for nothing more. The Mail method of net/smtp
// asks for SMTPUTF8 whenever the mail server offers it, and a mail server
// that must then pass the message on to one without SMTPUTF8 accepts the
// message and cannot deliver it.
func smtpMailFrom(client *smtp.Client, from string) error {
	id, e := client.Text.Cmd("MAIL FROM:<%s>", from)
	if e != nil {
		return e
	}
	client.Text.StartResponse(id)
	defer client.Text.EndResponse(id)
	_, _, e = client.Text.ReadResponse(250)
	return e
}

// smtpAnswer writes an error of the conversation with a mail server on one
// line: the mail server's answer as its code and its text, which can have
// several lines, or the error of the connection.
func smtpAnswer(e error) string {
	var answer *textproto.Error
	if errors.As(e, &answer) {
		return OneLine(fmt.Sprintf("%d %s", answer.Code, answer.Msg))
	}
	return OneLine(e.Error())
}

// smtpAnswersLimit is the most that MMWatchdog reads from a mail server while it
// delivers one message.
const smtpAnswersLimit = 1024 * 1024

// OneLine writes text that came from outside MMWatchdog on one line of at
// most 300 bytes, as primitives.OnOneLine does: without control characters
// and without characters that change how the text around them is shown.
// The error of a delivery attempt is printed by mmwatchdog send-notice and
// kept for mmwatchdog status, and a module's error goes into its check's
// message.
func OneLine(text string) string {
	text = strings.Join(strings.Fields(primitives.OnOneLine(text)), " ")
	if len(text) > 300 {
		text = primitives.Clip(text, 300) + "…"
	}
	return text
}

// smtpHelloName is the name MMWatchdog greets a mail server with: this machine's
// host name, which some mail servers require in place of "localhost".
func smtpHelloName() string {
	name, e := os.Hostname()
	if e != nil || name == "" || strings.ContainsFunc(name, func(r rune) bool {
		return !(r == '-' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	}) {
		return "localhost"
	}
	return name
}

// smtpLogin picks the login method from those the mail server offers. MMWatchdog
// knows the two that mail servers offer for a name and a password: PLAIN and
// LOGIN. The error completes a sentence that starts with the mail server.
func smtpLogin(loginUser, password, offered string) (smtp.Auth, error) {
	methods := strings.Fields(strings.ToUpper(offered))
	switch {
	case slices.Contains(methods, "PLAIN"):
		return smtpPlainLogin{loginUser, password}, nil
	case slices.Contains(methods, "LOGIN"):
		return &smtpLoginLogin{loginUser: loginUser, password: password}, nil
	case len(methods) == 0:
		return nil, errors.New("offers no login on this connection, and the destination states login_user and login_password_file")
	}
	return nil, fmt.Errorf("offers no login that MMWatchdog can use: MMWatchdog uses PLAIN or LOGIN, and the mail server offers %s", OneLine(strings.Join(methods, ", ")))
}

// smtpPlainLogin is the PLAIN method. The destination explicitly states
// whether its connection is encrypted.
type smtpPlainLogin struct{ loginUser, password string }

func (l smtpPlainLogin) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + l.loginUser + "\x00" + l.password), nil
}

func (l smtpPlainLogin) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("the mail server asked a question after the PLAIN login, which has none")
	}
	return nil, nil
}

// smtpLoginLogin is the LOGIN method: the mail server asks two questions,
// first for the name and then for the password. Mail servers word the
// questions differently, so the answers go by their order.
type smtpLoginLogin struct {
	loginUser, password string
	questions           int
}

func (l *smtpLoginLogin) Start(*smtp.ServerInfo) (string, []byte, error) { return "LOGIN", nil, nil }

func (l *smtpLoginLogin) Next(_ []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	l.questions++
	switch l.questions {
	case 1:
		return []byte(l.loginUser), nil
	case 2:
		return []byte(l.password), nil
	}
	return nil, errors.New("the mail server asked a third question during the LOGIN login, which has two")
}

// emailMessage is the message of an alert: the title with the alert's
// severity in front as the subject, and as the text the body followed by the
// time of the alert, the incident ID, and the event ID. The event ID is also
// the Message-ID, so a mail client can recognize a repeated delivery. With
// OnlyTheTitle the message has no text.
func emailMessage(m Email, event Event, onlyTheTitle bool, now time.Time) []byte {
	// A subject with a character outside ASCII is written as encoded words,
	// one on each line, so no line is longer than a mail server takes.
	subject := event.TitleWithAlertSeverity
	if encoded := mime.QEncoding.Encode("utf-8", subject); encoded != subject {
		subject = "\r\n " + strings.ReplaceAll(encoded, "?= =?", "?=\r\n =?")
	}
	var b bytes.Buffer
	for _, header := range [][2]string{
		{"From", m.From},
		{"To", strings.Join(m.To, ",\r\n ")},
		{"Subject", subject},
		{"Date", now.UTC().Format("Mon, 02 Jan 2006 15:04:05 -0700")},
		{"Message-ID", "<" + event.ID + "@" + m.From[strings.LastIndex(m.From, "@")+1:] + ">"},
		// A mail program that answers on its own, such as an out-of-office
		// reply, does not answer a message marked as written by a program.
		{"Auto-Submitted", "auto-generated"},
		{"MIME-Version", "1.0"},
		{"Content-Type", "text/plain; charset=utf-8"},
		{"Content-Transfer-Encoding", "quoted-printable"},
	} {
		b.WriteString(header[0] + ": " + header[1] + "\r\n")
	}
	b.WriteString("\r\n")
	if onlyTheTitle {
		return b.Bytes()
	}
	text := ""
	if event.Body != "" {
		text = strings.TrimRight(event.Body, "\n") + "\n\n"
	}
	text += "Time of the alert: " + event.At.UTC().Format("2006-01-02 15:04:05") + " UTC\n"
	if event.Incident != "" {
		text += "Incident ID: " + event.Incident + "\n"
	}
	text += "Event ID: " + event.ID + "\n"
	encoded := quotedprintable.NewWriter(&b)
	_, _ = encoded.Write([]byte(text))
	_ = encoded.Close()
	return b.Bytes()
}

// emailDescription is what "mmwatchdog describe-module email" shows, and what
// validation knows about the module's inputs.
var emailDescription = ModuleDescription{
	Summary: "Sends each alert as one email through a mail server.",
	UsedAs:  UsedAsAlertDestination,
	Inputs: []ModuleInput{
		DangerouslySkipServerCertificateCheckInput,
		{Name: "trusted_certificate_authority_file", Type: "string", Description: "The absolute path of PEM certificates that replace the system certificate authorities for this mail server."},
		{Name: "smtp_server", Type: "string", Required: true, Description: "The mail server's host name or address and its port, such as smtp.example.com:587."},
		{Name: "smtp_connection_security", Type: "string", Required: true, AllowedValues: []any{"starttls", "implicit_tls", "unencrypted"}, Description: "How the connection to the mail server is protected."},
		{Name: "from", Type: "string", Required: true, Description: "The address that the email comes from, such as mmwatchdog@example.com."},
		{Name: "to", Type: "list_of_strings", Required: true, Description: "The addresses that get the email. All of them get one message."},
		{Name: "login_user", Type: "string", Description: "The name that MMWatchdog logs in to the mail server with. It needs login_password_file."},
		{Name: "login_password_file", Type: "string", Description: "The absolute path of a file that holds the password of login_user on one line. MMWatchdog reads it for each delivery."},
	},
	Examples: []ModuleExample{{
		Description:   "Send warn and sos alerts to the on-call mailbox.",
		Configuration: "email:\n  smtp_server: smtp.example.com:587\n  smtp_connection_security: starttls\n  from: mmwatchdog@example.com\n  to: [oncall@example.com]\n  login_user: mmwatchdog@example.com\n  login_password_file: /etc/mmwatchdog/secrets/smtp\nalert_severities: [warn, sos]\nremind_every: never\n",
	}},
}
