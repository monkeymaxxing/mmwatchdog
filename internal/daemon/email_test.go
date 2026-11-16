package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/store"
	"mmwatchdog/internal/testhelpers"
)

// receivedEmail is one message a fakeMailServer accepted.
type receivedEmail struct {
	from string
	to   []string
	// mailFrom is the whole command that named the sender.
	mailFrom string
	// wire holds the message as MMWatchdog sent it, line by line with each line's
	// ending, before the mail server takes the doubled dots out. message
	// is what the mail server keeps.
	wire      []string
	message   string
	encrypted bool
	// loggedInAs is the name of the login that succeeded before the
	// message, or "".
	loggedInAs string
}

// fakeMailServer speaks enough SMTP to take a message from MMWatchdog, and records
// what it took. Its fields are set before start and not changed after.
type fakeMailServer struct {
	// implicitTLS encrypts each connection from its first byte.
	implicitTLS bool
	// offersStartTLS lets a connection switch to encryption.
	offersStartTLS bool
	// loginMethods is what the server offers after AUTH, such as "PLAIN
	// LOGIN"; "" offers no login.
	loginMethods       string
	username, password string
	// loginQuestions are the two questions of the LOGIN method; a mail
	// server words them as it likes. Empty asks "Username:" and "Password:".
	loginQuestions [2]string
	// refuses maps the start of a command, such as "RCPT TO:<gone@", to the
	// answer that refuses it; "DATA END" refuses the finished message.
	refuses map[string]string
	// silent accepts each connection and never says anything.
	silent bool
	// untrustedCertificate gives the server a certificate of its own, which
	// MMWatchdog does not trust.
	untrustedCertificate bool

	certificate tls.Certificate
	mu          sync.Mutex
	received    []receivedEmail
	commands    []string
}

// trustedMailServerCertificate is the certificate of every fake mail server
// that MMWatchdog trusts. TestMain makes it and puts it in SMTPRootCAs once
// (trustMailServerCertificate), so that tests of mail servers run beside
// each other.
var trustedMailServerCertificate tls.Certificate

// trustMailServerCertificate makes trustedMailServerCertificate, the one
// certificate in SMTPRootCAs.
func trustMailServerCertificate() error {
	certificate, privateKey, e := testhelpers.LocalhostCertificatePEM()
	if e != nil {
		return e
	}
	if trustedMailServerCertificate, e = tls.X509KeyPair(certificate, privateKey); e != nil {
		return e
	}
	checksandalerts.SMTPRootCAs = x509.NewCertPool()
	checksandalerts.SMTPRootCAs.AppendCertsFromPEM(certificate)
	return nil
}

// start listens on a loopback port with trustedMailServerCertificate, or a
// certificate of its own (untrustedCertificate), and returns the smtp_server
// value to reach the server by.
func (f *fakeMailServer) start(t *testing.T) string {
	t.Helper()
	f.certificate = trustedMailServerCertificate
	if f.untrustedCertificate {
		certificate, privateKey, e := testhelpers.LocalhostCertificatePEM()
		if e == nil {
			f.certificate, e = tls.X509KeyPair(certificate, privateKey)
		}
		if e != nil {
			t.Fatal(e)
		}
	}
	// The certificate names localhost.
	listeners := testhelpers.ListenAtLocalhost(t)
	var connections sync.WaitGroup
	for _, listener := range listeners {
		go func() {
			for {
				conn, e := listener.Accept()
				if e != nil {
					return
				}
				connections.Add(1)
				go func() { defer connections.Done(); defer conn.Close(); f.serve(conn) }()
			}
		}()
	}
	t.Cleanup(func() {
		for _, listener := range listeners {
			listener.Close()
		}
		connections.Wait()
	})
	_, port, _ := net.SplitHostPort(listeners[0].Addr().String())
	return "localhost:" + port
}

func (f *fakeMailServer) emails() []receivedEmail {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]receivedEmail{}, f.received...)
}

func (f *fakeMailServer) serve(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if f.silent {
		_, _ = io.Copy(io.Discard, conn)
		return
	}
	encrypted := f.implicitTLS
	if encrypted {
		conn = tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{f.certificate}})
	}
	text := textproto.NewConn(conn)
	say := func(line string) { _ = text.PrintfLine("%s", line) }
	say("220 fake mail server")
	email := receivedEmail{}
	loggedInAs := ""
	login := func(username, password string) {
		if username == f.username && password == f.password {
			loggedInAs = username
			say("235 2.7.0 logged in")
			return
		}
		say("535 5.7.8 the name or the password is wrong")
	}
	for {
		line, e := text.ReadLine()
		if e != nil {
			return
		}
		f.mu.Lock()
		f.commands = append(f.commands, line)
		f.mu.Unlock()
		refused := false
		for start, answer := range f.refuses {
			if strings.HasPrefix(line, start) {
				say(answer)
				refused = true
			}
		}
		if refused {
			continue
		}
		command := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(command, "EHLO"):
			say("250-fake mail server")
			if f.offersStartTLS && !encrypted {
				say("250-STARTTLS")
			}
			if f.loginMethods != "" {
				say("250-AUTH " + f.loginMethods)
			}
			say("250 8BITMIME")
		case command == "STARTTLS":
			say("220 switch to encryption")
			conn = tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{f.certificate}})
			text = textproto.NewConn(conn)
			encrypted = true
		case strings.HasPrefix(command, "AUTH PLAIN "):
			parts := strings.Split(decoded(line[len("AUTH PLAIN "):]), "\x00")
			if len(parts) != 3 {
				say("501 5.5.2 not a PLAIN login")
				continue
			}
			login(parts[1], parts[2])
		case command == "AUTH LOGIN":
			questions := f.loginQuestions
			if questions == [2]string{} {
				questions = [2]string{"Username:", "Password:"}
			}
			say("334 " + base64.StdEncoding.EncodeToString([]byte(questions[0])))
			username, _ := text.ReadLine()
			say("334 " + base64.StdEncoding.EncodeToString([]byte(questions[1])))
			password, _ := text.ReadLine()
			login(decoded(username), decoded(password))
		case strings.HasPrefix(command, "MAIL FROM:<"):
			email = receivedEmail{from: between(line), mailFrom: line, encrypted: encrypted, loggedInAs: loggedInAs}
			say("250 2.1.0 sender accepted")
		case strings.HasPrefix(command, "RCPT TO:<"):
			email.to = append(email.to, between(line))
			say("250 2.1.5 recipient accepted")
		case command == "DATA":
			say("354 send the message")
			// The message ends with a line that holds only a dot, and a
			// line of the message that starts with a dot arrives with a
			// second dot in front.
			for {
				line, e := text.R.ReadString('\n')
				if e != nil {
					return
				}
				if line == ".\r\n" {
					break
				}
				email.wire = append(email.wire, line)
				email.message += strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(line, "."), "\n"), "\r") + "\n"
			}
			if answer := f.refuses["DATA END"]; answer != "" {
				say(answer)
				continue
			}
			f.mu.Lock()
			f.received = append(f.received, email)
			f.mu.Unlock()
			say("250 2.0.0 message accepted")
		case command == "QUIT":
			say("221 2.0.0 goodbye")
			return
		default:
			say("502 5.5.1 the fake mail server does not know this command")
		}
	}
}

func decoded(text string) string {
	b, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
	return string(b)
}

// between returns what a command holds between < and >.
func between(line string) string {
	return line[strings.Index(line, "<")+1 : strings.Index(line, ">")]
}

// parsed reads an accepted message as a mail program does.
func parsed(t *testing.T, email receivedEmail) (subject, text string, header mail.Header) {
	t.Helper()
	message, e := mail.ReadMessage(strings.NewReader(email.message))
	if e != nil {
		t.Fatal(e, email.message)
	}
	if subject, e = new(mime.WordDecoder).DecodeHeader(message.Header.Get("Subject")); e != nil {
		t.Fatal(e)
	}
	if message.Header.Get("Content-Transfer-Encoding") != "quoted-printable" || message.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatal("the message does not say how its text is written", message.Header)
	}
	body, e := io.ReadAll(quotedprintable.NewReader(message.Body))
	if e != nil {
		t.Fatal(e)
	}
	return subject, string(body), message.Header
}

// emailDestination is an email destination of a test. Its time limit is
// far longer than a conversation with a mail server of the test takes, also
// on a busy machine.
func emailDestination(server, security string) checksandalerts.Destination {
	return checksandalerts.Destination{RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, TimeLimitPerDeliveryAttempt: "1m", Inputs: &checksandalerts.Email{
		SMTPServer: server, SMTPConnectionSecurity: security, From: "mmwatchdog@example.com", To: []string{"oncall@example.com", "ops@example.com"}}}
}

func writePassword(t *testing.T, password string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "smtp-password")
	if e := os.WriteFile(path, []byte(password+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	return path
}

var emailAlert = checksandalerts.Event{ID: "0123456789abcdef0123456789abcdef", Incident: "fedcba9876543210fedcba9876543210", Node: "app-1", CheckGroup: "myapp", Check: "http", Type: "failed", AlertSeverity: "sos",
	At: time.Date(2026, 9, 29, 14, 3, 5, 0, time.UTC), Title: "app-1: myapp/http failed", TitleWithAlertSeverity: "SOS: app-1: myapp/http failed", Body: "expected 200, received 503\nfailing since 14:02 UTC", Message: "expected 200, received 503"}

// An email destination sends the title with the alert's severity in front
// as the subject and the body as the text, followed by the time and the IDs, to every recipient in one message,
// over each kind of connection and with each login a mail server offers.
func TestEmailSenderContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, security, loginMethods string
		login                        bool
	}{
		{"starttls with a PLAIN login", checksandalerts.SMTPStartTLS, "PLAIN LOGIN", true},
		{"implicit TLS with a LOGIN login", checksandalerts.SMTPImplicitTLS, "LOGIN", true},
		{"starttls without a login", checksandalerts.SMTPStartTLS, "", false},
		{"unencrypted on the same machine with a login", checksandalerts.SMTPUnencrypted, "PLAIN", true},
		{"unencrypted on the same machine without a login", checksandalerts.SMTPUnencrypted, "PLAIN", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &fakeMailServer{implicitTLS: tc.security == checksandalerts.SMTPImplicitTLS, offersStartTLS: tc.security == checksandalerts.SMTPStartTLS, loginMethods: tc.loginMethods, username: "mmwatchdog-login", password: "a short pw"}
			destination := emailDestination(server.start(t), tc.security)
			if tc.login {
				destination.Inputs.(*checksandalerts.Email).LoginUser, destination.Inputs.(*checksandalerts.Email).LoginPasswordFile = "mmwatchdog-login", writePassword(t, "a short pw")
			}
			if field, message := destination.Inputs.(*checksandalerts.Email).Problem(); message != "" {
				t.Fatal(field, message)
			}
			var sender checksandalerts.NotificationSender = checksandalerts.EmailSender{Delivery: checksandalerts.DeliveryOf{Destination: destination}}
			if e := sender.Send(context.Background(), emailAlert); e != nil {
				t.Fatal(e)
			}
			emails := server.emails()
			if len(emails) != 1 {
				t.Fatal("expected one message", emails)
			}
			got := emails[0]
			if got.from != "mmwatchdog@example.com" || !slices.Equal(got.to, []string{"oncall@example.com", "ops@example.com"}) || got.encrypted != (tc.security != checksandalerts.SMTPUnencrypted) || (got.loggedInAs == "mmwatchdog-login") != tc.login {
				t.Fatalf("envelope: %+v", got)
			}
			// The message is ASCII, so MMWatchdog asks the mail server for nothing
			// that only a message with other characters needs.
			if got.mailFrom != "MAIL FROM:<mmwatchdog@example.com>" {
				t.Fatalf("the command that names the sender: %q", got.mailFrom)
			}
			subject, text, header := parsed(t, got)
			if subject != "SOS: app-1: myapp/http failed" || header.Get("Message-ID") != "<0123456789abcdef0123456789abcdef@example.com>" || header.Get("From") != "mmwatchdog@example.com" || header.Get("Auto-Submitted") != "auto-generated" {
				t.Fatal(subject, header)
			}
			if to, e := header.AddressList("To"); e != nil || len(to) != 2 || to[0].Address != "oncall@example.com" || to[1].Address != "ops@example.com" {
				t.Fatal(to, e)
			}
			if _, e := header.Date(); e != nil {
				t.Fatal(e)
			}
			if text != "expected 200, received 503\nfailing since 14:02 UTC\n\nTime of the alert: 2026-09-29 14:03:05 UTC\nIncident ID: fedcba9876543210fedcba9876543210\nEvent ID: 0123456789abcdef0123456789abcdef\n" {
				t.Fatalf("text: %q", text)
			}
		})
	}
}

// A destination that asks for the title only gets an email with no text; a
// title with a character outside ASCII and a long text arrive as written;
// an alert without a body and without an incident, such as a notice, has
// only the closing lines.
func TestEmailMessageText(t *testing.T) {
	t.Parallel()
	server := &fakeMailServer{}
	destination := emailDestination(server.start(t), checksandalerts.SMTPUnencrypted)
	send := func(event checksandalerts.Event, onlyTheTitle bool) receivedEmail {
		t.Helper()
		before := len(server.emails())
		if e := (checksandalerts.EmailSender{Delivery: checksandalerts.DeliveryOf{Destination: destination, SendOnlyTheAlertTitle: onlyTheTitle}}).Send(context.Background(), event); e != nil {
			t.Fatal(e)
		}
		return server.emails()[before]
	}
	subject, text, _ := parsed(t, send(emailAlert.OnlyTheTitle(), true))
	if subject != "SOS: app-1: myapp/http failed" || text != "" {
		t.Fatalf("only the title: %q %q", subject, text)
	}
	long := emailAlert
	long.TitleWithAlertSeverity = "Resolved warning: db-replica-03.frankfurt.example.com: nightly_backup_of_the_customer_database/the_restored_copy_of_last_night_has… is ok again"
	long.Body = ".a line that starts with a dot\n" + strings.Repeat("a long line of output with an ümlaut ", 40) + "\nan equals sign = and the end"
	got := send(long, false)
	subject, text, _ = parsed(t, got)
	if subject != long.TitleWithAlertSeverity || !strings.HasPrefix(text, long.Body+"\n\nTime of the alert: ") {
		t.Fatalf("a long alert changed on the way: %q %q", subject, text)
	}
	// On the wire every line ends with CR LF and has at most 78 bytes
	// before it, every byte is ASCII, and the line that starts with a dot
	// has a second dot in front.
	dotted := false
	for _, line := range got.wire {
		if !strings.HasSuffix(line, "\r\n") || strings.Contains(strings.TrimSuffix(line, "\r\n"), "\r") || len(line) > 80 {
			t.Fatalf("a line of the message on the wire: %q", line)
		}
		for i := 0; i < len(line); i++ {
			if line[i] >= 0x80 {
				t.Fatalf("a byte outside ASCII on the wire: %q", line)
			}
		}
		dotted = dotted || strings.HasPrefix(line, "..a line that starts with a dot")
	}
	if !dotted {
		t.Fatalf("the line that starts with a dot was not sent with a second dot: %q", got.wire)
	}
	notice := checksandalerts.Event{ID: "00000000000000000000000000000001", Type: "notice", AlertSeverity: "info", At: emailAlert.At, Title: "app-1: Deploy 4f2a91c finished", TitleWithAlertSeverity: "Info: app-1: Deploy 4f2a91c finished"}
	if _, text, _ = parsed(t, send(notice, false)); text != "Time of the alert: 2026-09-29 14:03:05 UTC\nEvent ID: 00000000000000000000000000000001\n" {
		t.Fatalf("notice: %q", text)
	}
}

// A mail server that refuses any step fails the attempt with a message that
// names the step, and nobody gets the message. The password is in no error.
func TestEmailSenderFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		server   *fakeMailServer
		security string
		password string
		want     string
	}{
		{"no STARTTLS offered", &fakeMailServer{}, checksandalerts.SMTPStartTLS, "", "does not offer STARTTLS, which smtp_connection_security: starttls requires"},
		{"a wrong password", &fakeMailServer{offersStartTLS: true, loginMethods: "PLAIN", username: "mmwatchdog-login", password: "the right one"}, checksandalerts.SMTPStartTLS, "the wrong one", "refused the login of mmwatchdog-login: 535 5.7.8 the name or the password is wrong"},
		{"a wrong password with the LOGIN method", &fakeMailServer{offersStartTLS: true, loginMethods: "LOGIN", username: "mmwatchdog-login", password: "the right one"}, checksandalerts.SMTPStartTLS, "the wrong one", "refused the login of mmwatchdog-login: 535 5.7.8 the name or the password is wrong"},
		{"a question after the PLAIN login", &fakeMailServer{offersStartTLS: true, loginMethods: "PLAIN", refuses: map[string]string{"AUTH PLAIN": "334 UGFzc3dvcmQ6"}}, checksandalerts.SMTPStartTLS, "any password", "failed while MMWatchdog was logging in: the mail server asked a question after the PLAIN login, which has none"},
		{"login methods with control characters", &fakeMailServer{offersStartTLS: true, loginMethods: "CRAM-MD5 \x1b[31mRED " + strings.Repeat("X", 5000)}, checksandalerts.SMTPStartTLS, "any password", "offers no login that MMWatchdog can use: MMWatchdog uses PLAIN or LOGIN, and the mail server offers CRAM-MD5, [31MRED, XXXX"},
		{"no login offered", &fakeMailServer{offersStartTLS: true}, checksandalerts.SMTPStartTLS, "any password", "offers no login on this connection, and the destination states login_user and login_password_file"},
		{"only a login MMWatchdog does not know", &fakeMailServer{offersStartTLS: true, loginMethods: "CRAM-MD5 GSSAPI"}, checksandalerts.SMTPStartTLS, "any password", "offers no login that MMWatchdog can use: MMWatchdog uses PLAIN or LOGIN, and the mail server offers CRAM-MD5, GSSAPI"},
		{"a refused sender", &fakeMailServer{refuses: map[string]string{"MAIL FROM:<mmwatchdog@example.com>": "550 5.7.1 this sender may not send"}}, checksandalerts.SMTPUnencrypted, "", "refused the sender mmwatchdog@example.com: 550 5.7.1 this sender may not send"},
		{"a refused recipient", &fakeMailServer{refuses: map[string]string{"RCPT TO:<ops@example.com>": "550 5.1.1 no such mailbox"}}, checksandalerts.SMTPUnencrypted, "", "refused the recipient ops@example.com: 550 5.1.1 no such mailbox"},
		{"a refused message", &fakeMailServer{refuses: map[string]string{"DATA END": "554 5.7.1 the message looks like spam"}}, checksandalerts.SMTPUnencrypted, "", "refused the message: 554 5.7.1 the message looks like spam"},
		{"a mail server that says nothing", &fakeMailServer{silent: true}, checksandalerts.SMTPUnencrypted, "", "did not finish within 2s (time_limit_per_delivery_attempt); MMWatchdog was waiting for the mail server's greeting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			address := tc.server.start(t)
			destination := emailDestination(address, tc.security)
			// The time limit ends only the attempt at the server that
			// says nothing.
			if tc.server.silent {
				destination.TimeLimitPerDeliveryAttempt = checksandalerts.Duration(testhelpers.TimeLimitThatATestLetsPass.String())
			}
			if tc.password != "" {
				destination.Inputs.(*checksandalerts.Email).LoginUser, destination.Inputs.(*checksandalerts.Email).LoginPasswordFile = "mmwatchdog-login", writePassword(t, tc.password)
			}
			e := checksandalerts.EmailSender{Delivery: checksandalerts.DeliveryOf{Destination: destination}}.Send(context.Background(), emailAlert)
			if e == nil || !strings.Contains(e.Error(), "the mail server "+address+" "+tc.want) {
				t.Fatalf("got %v, want %q", e, tc.want)
			}
			if len(e.Error()) > 600 || strings.ContainsFunc(e.Error(), unicode.IsControl) {
				t.Fatalf("the error is not one short line: %q", e)
			}
			if tc.password != "" && strings.Contains(e.Error(), tc.password) {
				t.Fatal("the password is in the error:", e)
			}
			if emails := tc.server.emails(); len(emails) != 0 && tc.name != "a refused recipient" {
				t.Fatal("a failed attempt delivered a message", emails)
			}
		})
	}
	t.Run("a certificate that MMWatchdog does not trust", func(t *testing.T) {
		for _, security := range []string{checksandalerts.SMTPStartTLS, checksandalerts.SMTPImplicitTLS} {
			server := &fakeMailServer{implicitTLS: security == checksandalerts.SMTPImplicitTLS, offersStartTLS: true, untrustedCertificate: true}
			destination := emailDestination(server.start(t), security)
			step := map[string]string{checksandalerts.SMTPStartTLS: "switching to encryption", checksandalerts.SMTPImplicitTLS: "starting the encrypted connection"}[security]
			if e := (checksandalerts.EmailSender{Delivery: checksandalerts.DeliveryOf{Destination: destination}}).Send(context.Background(), emailAlert); e == nil || !strings.Contains(e.Error(), "failed while MMWatchdog was "+step+": tls: failed to verify certificate") || len(server.emails()) != 0 {
				t.Fatal(security, e, server.emails())
			}
		}
	})
	t.Run("a certificate for another host name", func(t *testing.T) {
		server := &fakeMailServer{offersStartTLS: true}
		_, port, _ := net.SplitHostPort(server.start(t))
		// The certificate names localhost, and the destination names the
		// mail server by its address.
		e := checksandalerts.EmailSender{Delivery: checksandalerts.DeliveryOf{Destination: emailDestination("127.0.0.1:"+port, checksandalerts.SMTPStartTLS)}}.Send(context.Background(), emailAlert)
		if e == nil || !strings.Contains(e.Error(), "failed while MMWatchdog was switching to encryption: tls: failed to verify certificate") || len(server.emails()) != 0 {
			t.Fatal(e, server.emails())
		}
	})
	t.Run("no mail server at the address", func(t *testing.T) {
		address := testhelpers.AddressWhereNothingListens(t, "tcp")
		if e := (checksandalerts.EmailSender{Delivery: checksandalerts.DeliveryOf{Destination: emailDestination(address, checksandalerts.SMTPUnencrypted)}}).Send(context.Background(), emailAlert); e == nil || !strings.Contains(e.Error(), "the conversation with the mail server "+address+" failed while MMWatchdog was connecting: ") {
			t.Fatal(e)
		}
	})
	t.Run("a missing password file", func(t *testing.T) {
		server := &fakeMailServer{loginMethods: "PLAIN"}
		destination := emailDestination(server.start(t), checksandalerts.SMTPUnencrypted)
		destination.Inputs.(*checksandalerts.Email).LoginUser, destination.Inputs.(*checksandalerts.Email).LoginPasswordFile = "mmwatchdog-login", filepath.Join(t.TempDir(), "missing")
		if e := (checksandalerts.EmailSender{Delivery: checksandalerts.DeliveryOf{Destination: destination}}).Send(context.Background(), emailAlert); e == nil || !strings.Contains(e.Error(), "missing") || len(server.emails()) != 0 {
			t.Fatal(e)
		}
	})
	t.Run("a cancelled delivery", func(t *testing.T) {
		server := &fakeMailServer{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if e := (checksandalerts.EmailSender{Delivery: checksandalerts.DeliveryOf{Destination: emailDestination(server.start(t), checksandalerts.SMTPUnencrypted)}}).Send(ctx, emailAlert); e == nil || len(server.emails()) != 0 {
			t.Fatal("ignored cancelled delivery", e)
		}
	})
}

// A mail server words the two questions of the LOGIN method as it likes, so
// MMWatchdog answers them by their order.
func TestEmailLoginAnswersByTheOrderOfTheQuestions(t *testing.T) {
	t.Parallel()
	server := &fakeMailServer{offersStartTLS: true, loginMethods: "LOGIN", username: "mmwatchdog-login", password: "a short pw", loginQuestions: [2]string{"User Name", "Kennwort:"}}
	destination := emailDestination(server.start(t), checksandalerts.SMTPStartTLS)
	destination.Inputs.(*checksandalerts.Email).LoginUser, destination.Inputs.(*checksandalerts.Email).LoginPasswordFile = "mmwatchdog-login", writePassword(t, "a short pw")
	if e := (checksandalerts.EmailSender{Delivery: checksandalerts.DeliveryOf{Destination: destination}}).Send(context.Background(), emailAlert); e != nil {
		t.Fatal(e)
	}
	if emails := server.emails(); len(emails) != 1 || emails[0].loggedInAs != "mmwatchdog-login" {
		t.Fatal(emails)
	}
}

// The delivery worker gives the email sender send_only_the_alert_title as it
// was when the alert was queued, not as the destination states it now.
func TestEmailSenderTakesTheTitleOnlySettingOfTheQueuedDelivery(t *testing.T) {
	t.Parallel()
	now := emailDestination("smtp.example.com:587", checksandalerts.SMTPStartTLS)
	d := noticeDaemon(t, map[string]checksandalerts.Destination{"mailbox": now})
	queued := d.config.Alerts.Destinations["mailbox"]
	for _, asQueued := range []bool{true, false} {
		queued.SendOnlyTheAlertTitle = asQueued
		d.mu.Lock()
		sender, ok := d.sender(store.Delivery{Destination: "mailbox", Target: queued})
		d.mu.Unlock()
		if email, isEmail := sender.(checksandalerts.EmailSender); !ok || !isEmail || email.Delivery.SendOnlyTheAlertTitle != asQueued {
			t.Fatalf("queued with %v: %+v %v", asQueued, sender, ok)
		}
	}
}

// The password file holds one line; its spaces belong to the password.
func TestReadSMTPPassword(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for content, want := range map[string]string{
		"hunter2\n":                      "hunter2",
		"hunter2":                        "hunter2",
		" two words \r\n":                " two words ",
		"":                               "",
		"\n":                             "",
		"two\nlines\n":                   "",
		"a\ttab\n":                       "",
		"latin-1 \xe9\n":                 "",
		strings.Repeat("p", 1025):        "",
		strings.Repeat("p", 1024) + "\n": strings.Repeat("p", 1024),
	} {
		path := filepath.Join(dir, "password")
		os.WriteFile(path, []byte(content), 0600)
		got, e := checksandalerts.ReadPassword(path)
		if got != want || (e == nil) != (want != "") {
			t.Errorf("%q: got %q, %v", content, got, e)
		}
		if e != nil && !strings.Contains(e.Error(), path+": expected one line with the password: UTF-8 text of at most 1,024 bytes, without a tab or another control character") {
			t.Errorf("%q: %v", content, e)
		}
	}
}

// Every email destination states its mail server, how the connection is
// protected, a sender, and its recipients. A login is a name and a password
// file together, and never crosses the network without encryption.
func TestEmailSettingsAreValidated(t *testing.T) {
	t.Parallel()
	path := testhelpers.ConfigFile(t, "check_groups: {a: {shell: echo ok}}\n")
	alerts := filepath.Join(path, "alert_destinations.yml")
	load := func(email string) (*config.Config, error) {
		os.WriteFile(alerts, []byte("destinations:\n  mailbox:\n    email: {"+email+"}\n    remind_every: never\n    alert_severities: [warn, sos]\n"), 0600)
		return config.Load(path)
	}
	const complete = "smtp_server: 'smtp.example.com:587', smtp_connection_security: starttls, from: mmwatchdog@example.com, to: [oncall@example.com, ops@example.com]"
	for _, valid := range []string{
		complete,
		complete + ", login_user: mmwatchdog@example.com, login_password_file: /etc/mmwatchdog/secrets/smtp",
		"smtp_server: 'smtp.example.com:465', smtp_connection_security: implicit_tls, from: mmwatchdog@example.com, to: [oncall@example.com], login_user: mmwatchdog, login_password_file: /etc/mmwatchdog/secrets/smtp",
		"smtp_server: 'smtp.example.com:25', smtp_connection_security: unencrypted, from: mmwatchdog@example.com, to: [oncall@example.com], login_user: mmwatchdog, login_password_file: /etc/mmwatchdog/secrets/smtp",
		"smtp_server: 'localhost:25', smtp_connection_security: unencrypted, from: mmwatchdog@app-1.example.com, to: [operations@example.com]",
		"smtp_server: 'localhost:25', smtp_connection_security: unencrypted, from: mmwatchdog@localhost, to: [root@localhost], login_user: mmwatchdog, login_password_file: /etc/mmwatchdog/secrets/smtp",
		"smtp_server: '127.0.0.1:25', smtp_connection_security: unencrypted, from: mmwatchdog@example.com, to: [oncall@example.com], login_user: mmwatchdog, login_password_file: /etc/mmwatchdog/secrets/smtp",
		"smtp_server: '[::1]:25', smtp_connection_security: unencrypted, from: mmwatchdog@example.com, to: [oncall@example.com], login_user: mmwatchdog, login_password_file: /etc/mmwatchdog/secrets/smtp",
		"smtp_server: 'mail.internal:25', smtp_connection_security: unencrypted, from: mmwatchdog@example.com, to: [oncall@example.com]",
	} {
		c, e := load(valid)
		// An email destination has 30 seconds for one attempt unless it
		// states another limit: a mail server may take seconds to accept
		// a message.
		if e != nil || c.Alerts.Destinations["mailbox"].TimeLimitPerDeliveryAttempt != "30s" {
			t.Errorf("%s: %v", valid, e)
		}
	}
	for _, tc := range []struct{ email, want string }{
		{"smtp_connection_security: starttls, from: mmwatchdog@example.com, to: [oncall@example.com]", "destinations.mailbox.email.smtp_server: expected the mail server's host name and port, such as smtp.example.com:587"},
		{strings.Replace(complete, "smtp.example.com:587", "smtp.example.com", 1), "destinations.mailbox.email.smtp_server: expected the mail server's host name and port"},
		{strings.Replace(complete, "smtp.example.com:587", ":587", 1), "destinations.mailbox.email.smtp_server: expected the mail server's host name and port"},
		{strings.Replace(complete, "smtp.example.com:587", "smtp.example.com:smtp", 1), "destinations.mailbox.email.smtp_server: expected the mail server's host name and port"},
		{strings.Replace(complete, "smtp_connection_security: starttls, ", "", 1), "destinations.mailbox.email.smtp_connection_security: required: starttls, implicit_tls, or unencrypted"},
		{strings.Replace(complete, "starttls", "tls", 1), "destinations.mailbox.email.smtp_connection_security: required: starttls, implicit_tls, or unencrypted"},
		{strings.Replace(complete, "from: mmwatchdog@example.com, ", "", 1), "destinations.mailbox.email.from: expected one email address in ASCII, such as mmwatchdog@example.com"},
		{strings.Replace(complete, "from: mmwatchdog@example.com", "from: 'MMWatchdog <mmwatchdog@example.com>'", 1), "destinations.mailbox.email.from: expected one email address in ASCII"},
		{strings.Replace(complete, "from: mmwatchdog@example.com", "from: 'mmwatchdog@example.com, x@example.com'", 1), "destinations.mailbox.email.from: expected one email address in ASCII"},
		{strings.Replace(complete, "from: mmwatchdog@example.com", "from: 'mäil@example.com'", 1), "destinations.mailbox.email.from: expected one email address in ASCII"},
		{strings.Replace(complete, ", to: [oncall@example.com, ops@example.com]", "", 1), "destinations.mailbox.email.to: expected a list of at least one email address, such as [oncall@example.com]"},
		{strings.Replace(complete, "[oncall@example.com, ops@example.com]", "[]", 1), "destinations.mailbox.email.to: expected a list of at least one email address"},
		{strings.Replace(complete, "ops@example.com", "ops", 1), "destinations.mailbox.email.to.1: expected one email address in ASCII"},
		{strings.Replace(complete, "ops@example.com", "oncall@example.com", 1), "destinations.mailbox.email.to.1: oncall@example.com is listed twice"},
		{complete + ", trusted_certificate_authority_file: relative.pem", "trusted_certificate_authority_file: must be absolute"},
		{strings.Replace(complete, "starttls", "unencrypted", 1) + ", trusted_certificate_authority_file: /etc/mmwatchdog/ca.pem", "trusted_certificate_authority_file: has no use with smtp_connection_security: unencrypted"},
		{strings.Replace(complete, "starttls", "unencrypted", 1) + ", dangerously_skip_server_certificate_check: true", "dangerously_skip_server_certificate_check: has no use with smtp_connection_security: unencrypted"},
		{complete + ", login_user: mmwatchdog", "destinations.mailbox.email.login_password_file: required with login_user: the file that holds the password of mmwatchdog"},
		{complete + ", login_password_file: /etc/mmwatchdog/secrets/smtp", "destinations.mailbox.email.login_user: required with login_password_file: the name that MMWatchdog logs in to the mail server with"},
		{complete + ", login_user: mmwatchdog, login_password_file: secrets/smtp", "destinations.mailbox.email.login_password_file: must be absolute"},
		{complete + ", login_user: mmwatchdog, password: hunter2", `unknown field "password"`},
	} {
		if _, e := load(tc.email); e == nil || !strings.Contains(e.Error(), tc.want) {
			t.Errorf("%s:\n got %v\nwant %q", tc.email, e, tc.want)
		}
	}
	os.WriteFile(alerts, []byte("destinations:\n  mailbox:\n    email: {"+complete+"}\n    webhook: {url: 'https://example.com'}\n    remind_every: never\n    alert_severities: [sos]\n"), 0600)
	if _, e := config.Load(path); e == nil || !strings.Contains(e.Error(), "names the modules email and webhook: a destination names exactly one module") {
		t.Error("a destination with two modules was accepted:", e)
	}

}

// mmwatchdog validate reads the password file of every email destination.
func TestValidateRuntimeReadsTheEmailPasswordFile(t *testing.T) {
	t.Parallel()
	path := testhelpers.ConfigFile(t, "check_groups: {a: {http: {url: 'https://example.com'}}}\n")
	password := filepath.Join(t.TempDir(), "smtp")
	os.WriteFile(filepath.Join(path, "alert_destinations.yml"), []byte("destinations:\n  mailbox:\n    email: {smtp_server: 'smtp.example.com:587', smtp_connection_security: starttls, from: mmwatchdog@example.com, to: [oncall@example.com], login_user: mmwatchdog, login_password_file: "+password+"}\n    remind_every: never\n    alert_severities: [sos]\n"), 0600)
	c, e := config.Load(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = ValidateRuntime(c); e == nil || !strings.Contains(e.Error(), "destinations.mailbox.email.login_password_file") {
		t.Fatal("a missing password file passed runtime validation:", e)
	}
	os.WriteFile(password, []byte("two\nlines\n"), 0600)
	if e = ValidateRuntime(c); e == nil || !strings.Contains(e.Error(), "destinations.mailbox.email.login_password_file: "+password+": expected one line with the password") {
		t.Fatal(e)
	}
	os.WriteFile(password, []byte("hunter2\n"), 0600)
	if e = ValidateRuntime(c); e != nil {
		t.Fatal(e)
	}
}

// An email destination's identity is its mail server, its recipients in any
// order, and the content of its password file. The sender, the login name, and
// how the connection is protected are not part of it.
func TestEmailDestinationIdentity(t *testing.T) {
	t.Parallel()
	base := func() checksandalerts.Destination {
		d := emailDestination("smtp.example.com:587", checksandalerts.SMTPStartTLS)
		d.Inputs.(*checksandalerts.Email).LoginUser, d.Inputs.(*checksandalerts.Email).LoginPasswordFile = "mmwatchdog", "/etc/mmwatchdog/secrets/smtp"
		return d
	}
	for name, change := range map[string]func(*checksandalerts.Destination){
		"mail server": func(d *checksandalerts.Destination) {
			d.Inputs.(*checksandalerts.Email).SMTPServer = "smtp.example.com:465"
		},
		"recipients": func(d *checksandalerts.Destination) {
			d.Inputs.(*checksandalerts.Email).To = []string{"oncall@example.com"}
		},
		"kind": func(d *checksandalerts.Destination) {
			d.Inputs = &checksandalerts.Webhook{URL: "https://example.com"}
		},
	} {
		changed := base()
		change(&changed)
		if checksandalerts.SameDestination(base(), changed) || checksandalerts.SameDestination(changed, base()) {
			t.Error("the destination kept its identity after a change of its", name)
		}
	}
	for name, change := range map[string]func(*checksandalerts.Destination){
		"password file": func(d *checksandalerts.Destination) {
			d.Inputs.(*checksandalerts.Email).LoginPasswordFile = "/etc/mmwatchdog/secrets/smtp-2"
		},
		"order of recipients": func(d *checksandalerts.Destination) {
			d.Inputs.(*checksandalerts.Email).To = []string{"ops@example.com", "oncall@example.com"}
		},
		"sender":     func(d *checksandalerts.Destination) { d.Inputs.(*checksandalerts.Email).From = "alerts@example.com" },
		"login name": func(d *checksandalerts.Destination) { d.Inputs.(*checksandalerts.Email).LoginUser = "alerts" },
		"connection security": func(d *checksandalerts.Destination) {
			d.Inputs.(*checksandalerts.Email).SMTPConnectionSecurity = checksandalerts.SMTPImplicitTLS
		},
		"time limit": func(d *checksandalerts.Destination) { d.TimeLimitPerDeliveryAttempt = "30s" },
		"title only": func(d *checksandalerts.Destination) { d.SendOnlyTheAlertTitle = true },
		"severities": func(d *checksandalerts.Destination) { d.AlertSeverities = []string{"sos"} },
	} {
		changed := base()
		change(&changed)
		if !checksandalerts.SameDestination(base(), changed) {
			t.Error("the destination lost its identity after a change of its", name)
		}
	}
}

// The daemon delivers an alert to an email destination through the outbox:
// a failed attempt is retried with the same event ID, the password file is
// read again for the retry, and send_only_the_alert_title applies as it was
// when the alert was queued.
func TestTheDaemonDeliversAlertsByEmail(t *testing.T) {
	t.Parallel()
	server := &fakeMailServer{offersStartTLS: true, loginMethods: "PLAIN", username: "mmwatchdog-login", password: "the right password"}
	address := server.start(t)
	password := writePassword(t, "a wrong password")
	mailbox := emailDestination(address, checksandalerts.SMTPStartTLS)
	mailbox.Inputs.(*checksandalerts.Email).LoginUser, mailbox.Inputs.(*checksandalerts.Email).LoginPasswordFile = "mmwatchdog-login", password
	sms := emailDestination(address, checksandalerts.SMTPStartTLS)
	sms.Inputs.(*checksandalerts.Email).To, sms.SendOnlyTheAlertTitle = []string{"15551234567@sms-gateway.example.com"}, true
	d := noticeDaemon(t, map[string]checksandalerts.Destination{"mailbox": mailbox, "sms": sms})
	runWorker(t, d)
	result, _, _ := sendNotice(t, d, store.NoticeRequest{AlertSeverity: "sos", Title: "Test of the alert destinations", Body: "Nothing is wrong."})
	failed := false
	for _, delivery := range result.Deliveries {
		switch delivery.Destination {
		case "mailbox":
			failed = delivery.Result == store.NoticeFailed && strings.Contains(delivery.Error, "refused the login of mmwatchdog-login")
		case "sms":
			if delivery.Result != store.NoticeDelivered {
				t.Fatal(delivery)
			}
		}
	}
	if !failed {
		t.Fatal("the wrong password did not fail the first attempt", result)
	}
	// MMWatchdog reads the password file again for the retry.
	if e := os.WriteFile(password, []byte("the right password\n"), 0600); e != nil {
		t.Fatal(e)
	}
	var emails []receivedEmail
	for end := time.Now().Add(testhelpers.LongestWaitForAnEvent); len(emails) < 2 && time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		emails = server.emails()
	}
	if len(emails) != 2 {
		t.Fatal("expected one message for each destination", emails)
	}
	for _, email := range emails {
		subject, text, header := parsed(t, email)
		if subject != "SOS: "+d.store.Node+": Test of the alert destinations" || header.Get("Message-ID") != "<"+result.NoticeID+"@example.com>" {
			t.Fatal(subject, header)
		}
		switch email.to[0] {
		case "15551234567@sms-gateway.example.com":
			if text != "" {
				t.Fatalf("the destination that takes only the title got a text: %q", text)
			}
		default:
			if !strings.HasPrefix(text, "Nothing is wrong.\n\nTime of the alert: ") || !strings.HasSuffix(text, "Event ID: "+result.NoticeID+"\n") || email.loggedInAs != "mmwatchdog-login" {
				t.Fatalf("%q %+v", text, email)
			}
		}
	}
}

// A joined node that delivers an alert without the hub sends the email
// itself, with the password from its copy of the fleet settings.
func TestAJoinedNodeSendsEmailWithoutTheHub(t *testing.T) {
	t.Parallel()
	server := &fakeMailServer{offersStartTLS: true, loginMethods: "LOGIN", username: "mmwatchdog-login", password: "hub smtp password"}
	mailbox := emailDestination(server.start(t), checksandalerts.SMTPStartTLS)
	password := writePassword(t, "hub smtp password")
	mailbox.Inputs.(*checksandalerts.Email).LoginUser, mailbox.Inputs.(*checksandalerts.Email).LoginPasswordFile = "mmwatchdog-login", password
	_, code, srv := startHubWith(t, map[string]checksandalerts.Destination{"mailbox": mailbox})
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	if node.fleetSettings == nil || node.fleetSettings.FileContentsByDestination["mailbox"]["login_password_file"] != "hub smtp password" {
		t.Fatal("the settings copy lacks the mail server password", node.fleetSettings)
	}
	node.fleetSettings.Fleet.SendAlertWithoutHubAfterWaitingFor = "300ms" // below what configuration allows
	srv.Close()
	os.Remove(password)
	setMaintenance(t, node, "app", "10m")
	runWorker(t, node)
	var emails []receivedEmail
	for end := time.Now().Add(testhelpers.LongestWaitForAnEvent); len(emails) < 1 && time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		emails = server.emails()
	}
	if len(emails) != 1 || emails[0].loggedInAs != "mmwatchdog-login" {
		t.Fatal("the node did not send the email itself", emails)
	}
	if subject, _, _ := parsed(t, emails[0]); subject != "Info: app-1: maintenance on for app" {
		t.Fatal(subject)
	}
}

// A copy of the fleet settings is refused when an email destination's
// password is missing, not a password, or given for a destination without a
// password file.
func TestFleetSettingsCarryEmailPasswords(t *testing.T) {
	t.Parallel()
	withLogin := emailDestination("smtp.example.com:587", checksandalerts.SMTPStartTLS)
	withLogin.Inputs.(*checksandalerts.Email).LoginUser, withLogin.Inputs.(*checksandalerts.Email).LoginPasswordFile = "mmwatchdog", "/etc/mmwatchdog/secrets/smtp"
	withoutLogin := emailDestination("smtp.example.com:587", checksandalerts.SMTPStartTLS)
	settings := func(destination checksandalerts.Destination, credentials map[string]string) *fleet.FleetSettings {
		contents := map[string]map[string]string{}
		if len(credentials) > 0 {
			contents["mailbox"] = credentials
		}
		s := &fleet.FleetSettings{Destinations: map[string]checksandalerts.Destination{"mailbox": destination}, FileContentsByDestination: contents, Fleet: config.DefaultFleetTimings, AlertSeverityWhenTheHubGoesSilent: "sos"}
		s.Version = s.ComputeVersion()
		return s
	}
	for _, tc := range []struct {
		name        string
		destination checksandalerts.Destination
		credentials map[string]string
		want        string
	}{
		{"a login with its password", withLogin, map[string]string{"login_password_file": "hunter2"}, ""},
		{"no login", withoutLogin, map[string]string{}, ""},
		{"a login without its password", withLogin, map[string]string{}, `fleet settings lack credential file contents for destination "mailbox", input login_password_file`},
		{"a password of two lines", withLogin, map[string]string{"login_password_file": "two\nlines"}, `fleet settings contain invalid credential file contents for destination "mailbox", input login_password_file`},
		{"a password without a login", withoutLogin, map[string]string{"login_password_file": "hunter2"}, `fleet settings contain credential file contents not named by the module inputs of destination "mailbox"`},
	} {
		e := settings(tc.destination, tc.credentials).Validate(nil)
		if tc.want == "" && e != nil || tc.want != "" && (e == nil || e.Error() != tc.want) {
			t.Errorf("%s: got %v, want %q", tc.name, e, tc.want)
		}
	}
}

func TestEmailPartialRecipientsAndQuietVerification(t *testing.T) {
	t.Parallel()
	server := &fakeMailServer{refuses: map[string]string{"RCPT TO:<ops@example.com>": "550 no such mailbox", "RCPT TO:<other@example.com>": "550 another refusal"}}
	destination := emailDestination(server.start(t), checksandalerts.SMTPUnencrypted)
	destination.Inputs.(*checksandalerts.Email).To = append(destination.Inputs.(*checksandalerts.Email).To, "other@example.com")
	delivery := checksandalerts.DeliveryOf{Destination: destination}
	result := delivery.VerifyDestination(context.Background())
	if !result.CanVerifyWithoutMessage || result.AcceptsAddressAndCredential || !strings.Contains(result.VerificationMessage, "ops@example.com") || !strings.Contains(result.VerificationMessage, "other@example.com") || len(server.emails()) != 0 {
		t.Fatal(result, server.emails())
	}
	// Every retry sends to all again. Accepted recipients get the message,
	// and every refused address remains visible as a delivery failure.
	for attempt := 0; attempt < 2; attempt++ {
		err := (checksandalerts.EmailSender{Delivery: delivery}).Send(context.Background(), emailAlert)
		if err == nil || !strings.Contains(err.Error(), "ops@example.com") || !strings.Contains(err.Error(), "other@example.com") {
			t.Fatal(err)
		}
	}
	emails := server.emails()
	if len(emails) != 2 || len(emails[0].to) != 1 || emails[0].to[0] != "oncall@example.com" || len(emails[1].to) != 1 {
		t.Fatal(emails)
	}
}

func TestEmailCertificatePolicyAndQuietVerification(t *testing.T) {
	t.Parallel()
	for _, security := range []string{checksandalerts.SMTPImplicitTLS, checksandalerts.SMTPStartTLS} {
		server := &fakeMailServer{implicitTLS: security == checksandalerts.SMTPImplicitTLS, offersStartTLS: true, untrustedCertificate: true, loginMethods: "PLAIN", username: "mmwatchdog", password: "password"}
		destination := emailDestination(server.start(t), security)
		input := destination.Inputs.(*checksandalerts.Email)
		input.LoginUser, input.LoginPasswordFile = "mmwatchdog", filepath.Join(t.TempDir(), "password")
		os.WriteFile(input.LoginPasswordFile, []byte("password"), 0600)
		delivery := checksandalerts.DeliveryOf{Destination: destination}
		if result := delivery.VerifyDestination(context.Background()); result.AcceptsAddressAndCredential {
			t.Fatal(result)
		}
		input.TrustedCertificateAuthorityFile = filepath.Join(t.TempDir(), "authority.pem")
		os.WriteFile(input.TrustedCertificateAuthorityFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.certificate.Certificate[0]}), 0644)
		if result := delivery.VerifyDestination(context.Background()); !result.AcceptsAddressAndCredential || len(server.emails()) != 0 {
			t.Fatal(result, server.emails())
		}
		if err := (checksandalerts.EmailSender{Delivery: delivery}).Send(context.Background(), emailAlert); err != nil {
			t.Fatal(err)
		}
		input.TrustedCertificateAuthorityFile = ""
		input.DangerouslySkipServerCertificateCheck = true
		if err := (checksandalerts.EmailSender{Delivery: delivery}).Send(context.Background(), emailAlert); err != nil {
			t.Fatal(err)
		}
		// A verified SMTP conversation also checks the password.
		os.WriteFile(input.LoginPasswordFile, []byte("wrong"), 0600)
		if result := delivery.VerifyDestination(context.Background()); result.AcceptsAddressAndCredential || !strings.Contains(result.VerificationMessage, "refused the login") {
			t.Fatal(result)
		}
	}
}

func TestEmailQuietVerificationStopsBeforeData(t *testing.T) {
	t.Parallel()
	server := &fakeMailServer{}
	destination := emailDestination(server.start(t), checksandalerts.SMTPUnencrypted)
	result := (checksandalerts.DeliveryOf{Destination: destination}).VerifyDestination(context.Background())
	if !result.AcceptsAddressAndCredential {
		t.Fatal(result)
	}
	server.mu.Lock()
	commands := slices.Clone(server.commands)
	server.mu.Unlock()
	for _, command := range []string{"MAIL FROM:<mmwatchdog@example.com>", "RCPT TO:<oncall@example.com>", "RCPT TO:<ops@example.com>", "QUIT"} {
		if !slices.Contains(commands, command) {
			t.Fatal("missing SMTP command", command, commands)
		}
	}
	for _, command := range commands {
		if command == "DATA" {
			t.Fatal("quiet verification sent DATA", commands)
		}
	}
}
