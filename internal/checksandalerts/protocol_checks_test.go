package checksandalerts_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/testhelpers"
)

// The tests of the checks of protocols: a text exchange over TCP and UDP,
// SSH, SMTP, IMAP, POP, DNS, memcached, and Redis. The rules are in
// docs/changing-mmwatchdog/design/checks.md under "Protocols and common server software". Each
// test talks to a stand-in server on this machine.

// A socket that a test closes is not closed while a process that another
// test starts at that moment holds a copy of it: a new process holds every
// descriptor of the test process until it starts its program. A port or a
// Unix socket that a test needs closed therefore never belongs to a
// listener of the test. The two helpers below make their socket and close
// it while they hold syscall.ForkLock, which every start of a process
// waits for. They use the system calls themselves, because the net package
// takes syscall.ForkLock too.

// leaveUnixSocketFile makes path a Unix socket file on which no program
// accepts connections, as a program leaves it that ended without removing
// its socket.
func leaveUnixSocketFile(t *testing.T, path string) {
	t.Helper()
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	socket, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(socket)
	if err = syscall.Bind(socket, &syscall.SockaddrUnix{Name: path}); err != nil {
		t.Fatal(err)
	}
}

// tcpServer listens on this machine and gives each connection to serve. It
// returns the address as 127.0.0.1:port.
func tcpServer(t *testing.T, serve func(conn net.Conn)) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				serve(conn)
			}()
		}
	}()
	return listener.Addr().String()
}

// tcpServerAtLocalhost is tcpServer for a server that a check reaches by
// the name localhost, such as one whose certificate names localhost. It
// returns localhost:port.
func tcpServerAtLocalhost(t *testing.T, serve func(conn net.Conn)) string {
	t.Helper()
	listeners := testhelpers.ListenAtLocalhost(t)
	for _, listener := range listeners {
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				go func() {
					defer conn.Close()
					serve(conn)
				}()
			}
		}()
	}
	_, port, _ := net.SplitHostPort(listeners[0].Addr().String())
	return "localhost:" + port
}

// timeLimitFarAboveTheRun is the time limit of a run of a check that ends
// by itself: far longer than the run takes, also on a busy machine, so
// that the limit never decides the result.
const timeLimitFarAboveTheRun = time.Minute

// withinTimeLimit runs a check once under a time limit, as the daemon does.
func withinTimeLimit(t *testing.T, limit time.Duration, check checksandalerts.Check) checksandalerts.Observation {
	t.Helper()
	ctx, cancel := context.WithTimeout(testhelpers.TestContext(t), limit)
	defer cancel()
	return checksandalerts.RunCheck(ctx, checksandalerts.NewCheckRun(check, checksandalerts.Timing{TimeLimitPerRun: checksandalerts.Duration(limit.String())}))
}

// A tcp check with a text to send and a rule for the reply reads until the
// rule holds. Without a rule it only connects, as the check with the
// address alone does.
func TestTCPCheckSendsATextAndExpectsAReply(t *testing.T) {
	t.Parallel()
	testEnds := make(chan struct{})
	t.Cleanup(func() { close(testEnds) })
	address := tcpServer(t, func(conn net.Conn) {
		conn.Write([]byte("220 ready\r\n"))
		line, _ := bufio.NewReader(conn).ReadString('\n')
		switch strings.TrimSpace(line) {
		case "STATUS":
			// The reply arrives in two parts, and the connection stays
			// open after it.
			conn.Write([]byte("STATE "))
			time.Sleep(20 * time.Millisecond)
			conn.Write([]byte("READY 42 jobs\r\n"))
			<-testEnds
		case "QUIET":
			<-testEnds
		}
	})
	observe := func(tcp checksandalerts.TCP) checksandalerts.Observation {
		tcp.Address = address
		return withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: &tcp})
	}
	observeUntilTheTimeLimit := func(tcp checksandalerts.TCP) checksandalerts.Observation {
		tcp.Address = address
		return withinTimeLimit(t, testhelpers.TimeLimitThatATestLetsPass, checksandalerts.Check{Inputs: &tcp})
	}
	if o := observe(checksandalerts.TCP{}); o.Status != "ok" || o.Message != "TCP connection accepted" {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.TCP{TextToSend: "STATUS\r\n"}); o.Status != "ok" || o.Message != "TCP connection accepted" {
		t.Fatal(o)
	}
	// The greeting alone, without a text to send.
	if o := observe(checksandalerts.TCP{ReplyRules: checksandalerts.ReplyRules{ReplyContains: "220 ready"}}); o.Status != "ok" || o.Message != "the reply holds what the check expects" {
		t.Fatal(o)
	}
	// The check ends once the reply holds what it expects. A check that
	// waited for the end of the connection or of its time limit would take
	// a minute.
	started := time.Now()
	if o := observe(checksandalerts.TCP{TextToSend: "STATUS\r\n", ReplyRules: checksandalerts.ReplyRules{ReplyContains: "READY", ReplyMatchesRegex: `READY \d+ jobs`}}); o.Status != "ok" || time.Since(started) > 10*time.Second {
		t.Fatal(o, time.Since(started))
	}
	// The server closes the connection, and its reply does not hold the text.
	o := observe(checksandalerts.TCP{TextToSend: "OTHER\r\n", ReplyRules: checksandalerts.ReplyRules{ReplyContains: "READY"}})
	if o.Status != "failed" || o.Message != `the reply is not one that contains "READY"` || !reflect.DeepEqual(o.Details, []string{"the server sent: 220 ready"}) {
		t.Fatal(o)
	}
	if o := observeUntilTheTimeLimit(checksandalerts.TCP{TextToSend: "STATUS\r\n", ReplyRules: checksandalerts.ReplyRules{ReplyContains: "READY", ReplyMatchesRegex: `READY \d+ workers`}}); o.Status != "failed" ||
		o.Message != `the reply is not one that matches the regular expression READY \d+ workers; MMWatchdog waited until the time limit of the run` {
		t.Fatal(o)
	}
	// The server says nothing more, and the time limit ends the wait.
	if o := observeUntilTheTimeLimit(checksandalerts.TCP{TextToSend: "QUIET\r\n", ReplyRules: checksandalerts.ReplyRules{ReplyContains: "READY"}}); o.Status != "failed" || !strings.HasSuffix(o.Message, "; MMWatchdog waited until the time limit of the run") || len(o.Details) != 1 {
		t.Fatal(o)
	}
	silent := tcpServer(t, func(conn net.Conn) {})
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: &checksandalerts.TCP{Address: silent, ReplyRules: checksandalerts.ReplyRules{ReplyContains: "x"}}}); o.Status != "failed" || o.Message != `the server sent no reply, and the check expects one that contains "x"` {
		t.Fatal(o)
	}
	for _, expression := range []string{".*", "^$"} {
		if o := withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: &checksandalerts.TCP{Address: silent, ReplyRules: checksandalerts.ReplyRules{ReplyMatchesRegex: expression}}}); o.Status != "failed" {
			t.Fatal(o)
		}
	}
	// A port that nothing listens on.
	closed := testhelpers.AddressWhereNothingListens(t, "tcp")
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: &checksandalerts.TCP{Address: closed, ReplyRules: checksandalerts.ReplyRules{ReplyContains: "x"}}}); o.Status != "failed" || !strings.Contains(o.Message, "connection refused") {
		t.Fatal(o)
	}

	loadChecks(t, map[string]string{
		"{tcp: '127.0.0.1:5432', alert_severity: sos}":                                                                                          "",
		"{tcp: {address: '127.0.0.1:7000'}, alert_severity: sos}":                                                                               "",
		`{tcp: {address: '127.0.0.1:7000', text_to_send: "STATUS\r\n", reply_contains: READY}, alert_severity: sos}`:                            "",
		"{tcp: {address: '127.0.0.1:7000', reply_matches_regex: '^220 '}, alert_severity: sos}":                                                 "",
		"{tcp: {text_to_send: x}, alert_severity: sos}":                                                                                         "check_groups.c.tcp.address: ",
		"{tcp: {address: '127.0.0.1'}, alert_severity: sos}":                                                                                    "check_groups.c.tcp.address: ",
		"{tcp: '127.0.0.1:0', alert_severity: sos}":                                                                                             "check_groups.c.tcp.address: port must be a number from 1 to 65535",
		"{tcp: {address: '127.0.0.1:7000', text_to_send: ''}, alert_severity: sos}":                                                             "check_groups.c.tcp.text_to_send: cannot be empty",
		"{tcp: {address: '127.0.0.1:7000', reply_contains: ''}, alert_severity: sos}":                                                           "check_groups.c.tcp.reply_contains: cannot be empty",
		"{tcp: {address: '127.0.0.1:7000', reply_matches_regex: '('}, alert_severity: sos}":                                                     "check_groups.c.tcp.reply_matches_regex: is no valid regular expression: ",
		"{tcp: {address: '127.0.0.1:7000', expect: READY}, alert_severity: sos}":                                                                `unknown field "expect"`,
		"{tcp: [1], alert_severity: sos}":                                                                                                       "expected a mapping",
		`{udp: {address: '127.0.0.1:7001', text_to_send: "STATUS\n", reply_contains: READY}, alert_severity: sos}`:                              "",
		"{udp: {address: '127.0.0.1:7001', text_to_send: x}, alert_severity: sos}":                                                              "",
		"{udp: {address: '127.0.0.1:7001'}, alert_severity: sos}":                                                                               "check_groups.c.udp.text_to_send: is required: the text that the check sends in one UDP packet",
		"{udp: {text_to_send: x}, alert_severity: sos}":                                                                                         "check_groups.c.udp.address: ",
		"{udp: '127.0.0.1:7001', alert_severity: sos}":                                                                                          "expected a mapping",
		"{udp: {address: '127.0.0.1:7001', text_to_send: x, reply_matches_regex: '['}, alert_severity: sos}":                                    "check_groups.c.udp.reply_matches_regex: is no valid regular expression: ",
		"{ssh: '127.0.0.1:22', alert_severity: sos}":                                                                                            "",
		"{ssh: 127.0.0.1, alert_severity: sos}":                                                                                                 "check_groups.c.ssh: ",
		"{memcached: '127.0.0.1:11211', alert_severity: warn}":                                                                                  "",
		"{memcached: '127.0.0.1:11211', fail_when_measurement_above: {cache_used_percent: 90, current_connections: 900}, alert_severity: warn}": "",
		"{memcached: '127.0.0.1:11211', fail_when_measurement_below: {cache_hit_percent: 80}, fail_when_measurement_above: {cache_evictions_per_second: 10, cache_misses_per_second: 500}, alert_severity: warn}": "",
		"{memcached: '127.0.0.1:11211', fail_when_measurement_above: {evictions: 1}, alert_severity: warn}":                                                                                                       "check_groups.c.fail_when_measurement_above.evictions: this check has no such measurement; it measures current_connections, cached_items, cache_used_percent, cache_hits_per_second, cache_misses_per_second, cache_hit_percent, cache_evictions_per_second",
		"{memcached: '127.0.0.1:11211', fail_when_measurement_below: {cache_hit_percent: 101}, alert_severity: warn}":                                                                                             "check_groups.c.fail_when_measurement_below.cache_hit_percent: expected a number from 0 to 100",
		"{check_every: 1s, time_limit_per_run: 200ms, memcached: '127.0.0.1:11211', alert_severity: warn}":                                                                                                        "check_groups.c.time_limit_per_run: must exceed 200ms for the initial sample of the memcached server's counters",
		"{memcached: localhost, alert_severity: warn}":                                                                                                                            "check_groups.c.memcached: ",
		"{redis: {connection_security: unencrypted, address: '127.0.0.1:6379'}, alert_severity: sos}":                                                                             "",
		"{redis: {connection_security: unencrypted, address: '127.0.0.1:6379', login_password_file: /etc/mmwatchdog/secrets/redis}, alert_severity: sos}":                         "",
		"{redis: {connection_security: unencrypted, address: '127.0.0.1:6379', login_user: mmwatchdog, login_password_file: /etc/mmwatchdog/secrets/redis}, alert_severity: sos}": "",
		"{redis: {connection_security: unencrypted, address: '127.0.0.1:6379', login_user: mmwatchdog}, alert_severity: sos}":                                                     "check_groups.c.redis.login_password_file: required with login_user: the file that holds the password of mmwatchdog",
		"{redis: {connection_security: unencrypted, address: '127.0.0.1:6379', login_password_file: secrets/redis}, alert_severity: sos}":                                         "check_groups.c.redis.login_password_file: must be absolute",
		"{redis: {connection_security: unencrypted, address: '127.0.0.1:6379', login_user: 'two words', login_password_file: /a}, alert_severity: sos}":                           "check_groups.c.redis.login_user: expected one word of at most 256 bytes",
		"{redis: {connection_security: unencrypted, login_password_file: /a}, alert_severity: sos}":                                                                               "check_groups.c.redis.address: ",
		"{redis: '127.0.0.1:6379', alert_severity: sos}":                                                                                                                          "expected a mapping",
		"{redis: {connection_security: unencrypted, address: '127.0.0.1:6379', password: secret}, alert_severity: sos}":                                                           `unknown field "password"`,
	})
}

// A udp check sends its text in one packet and is healthy when a reply
// arrives that keeps the rules. No reply within the time limit, and a
// machine that says that no program listens, are failed.
func TestUDPCheck(t *testing.T) {
	t.Parallel()
	server, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	go func() {
		packet := make([]byte, 1500)
		for {
			n, from, err := server.ReadFrom(packet)
			if err != nil {
				return
			}
			if text := string(packet[:n]); text == "EMPTY" {
				server.WriteTo(nil, from)
			} else if text != "QUIET" {
				server.WriteTo([]byte("you sent "+text), from)
			}
		}
	}()
	address := server.LocalAddr().String()
	observe := func(udp checksandalerts.UDP) checksandalerts.Observation {
		udp.Address = address
		return withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: &udp})
	}
	if received, err := primitives.UDPExchange(context.Background(), address, "STATUS"); err != nil || received != "you sent STATUS" {
		t.Fatal(received, err)
	}
	if o := observe(checksandalerts.UDP{TextToSend: "EMPTY", ReplyRules: checksandalerts.ReplyRules{ReplyMatchesRegex: "^$"}}); o.Status != "ok" {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.UDP{TextToSend: "STATUS"}); o.Status != "ok" || o.Message != "a reply arrived from "+address {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.UDP{TextToSend: "STATUS", ReplyRules: checksandalerts.ReplyRules{ReplyContains: "sent STATUS", ReplyMatchesRegex: "^you "}}); o.Status != "ok" || o.Message != "the reply holds what the check expects" {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.UDP{TextToSend: "STATUS", ReplyRules: checksandalerts.ReplyRules{ReplyContains: "READY"}}); o.Status != "failed" || o.Message != `the reply is not one that contains "READY"` || !reflect.DeepEqual(o.Details, []string{"the server sent: you sent STATUS"}) {
		t.Fatal(o)
	}
	if o := withinTimeLimit(t, testhelpers.TimeLimitThatATestLetsPass, checksandalerts.Check{Inputs: &checksandalerts.UDP{Address: address, TextToSend: "QUIET"}}); o.Status != "failed" || o.Message != "no reply arrived from "+address+" within 2s (time_limit_per_run)" {
		t.Fatal(o)
	}
	// A port that no program listens on: this machine says so at once. A
	// check that waited for a reply would take its time limit, a minute.
	closed := testhelpers.AddressWhereNothingListens(t, "udp")
	started := time.Now()
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: &checksandalerts.UDP{Address: closed, TextToSend: "STATUS"}}); o.Status != "failed" || o.Message != "no program listens on the UDP port of "+closed || time.Since(started) > 10*time.Second {
		t.Fatal(o, time.Since(started))
	}
}

// An ssh check reads the greeting of the server, also after other lines,
// names itself, and leaves.
func TestSSHCheck(t *testing.T) {
	t.Parallel()
	testEnds := make(chan struct{})
	t.Cleanup(func() { close(testEnds) })
	said := make(chan string, 1)
	address := tcpServer(t, func(conn net.Conn) {
		conn.Write([]byte("Welcome to this machine\r\nSSH-2.0-OpenSSH_10.0p2 Debian-7\r\n"))
		line, _ := bufio.NewReader(conn).ReadString('\n')
		said <- line
	})
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: new(checksandalerts.SSH(address))}); o.Status != "ok" || o.Message != "the SSH server at "+address+" greeted with SSH-2.0-OpenSSH_10.0p2 Debian-7" {
		t.Fatal(o)
	}
	if line := <-said; line != "SSH-2.0-MMWatchdog\r\n" {
		t.Fatalf("MMWatchdog named itself with %q", line)
	}
	other := tcpServer(t, func(conn net.Conn) { conn.Write([]byte("220 mail.example.com ESMTP\r\n")) })
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: new(checksandalerts.SSH(other))}); o.Status != "failed" || o.Message != "the server at "+other+" sent no SSH greeting" || !reflect.DeepEqual(o.Details, []string{"the server sent: 220 mail.example.com ESMTP"}) {
		t.Fatal(o)
	}
	silent := tcpServer(t, func(conn net.Conn) { <-testEnds })
	if o := withinTimeLimit(t, testhelpers.TimeLimitThatATestLetsPass, checksandalerts.Check{Inputs: new(checksandalerts.SSH(silent))}); o.Status != "failed" || o.Message != "the server at "+silent+" sent no SSH greeting within the time limit of the run" || o.Details != nil {
		t.Fatal(o)
	}
	// A mail server greets in its own way and then waits for the client.
	waiting := tcpServer(t, func(conn net.Conn) {
		conn.Write([]byte("220 mail.example.com ESMTP\r\n"))
		<-testEnds
	})
	if o := withinTimeLimit(t, testhelpers.TimeLimitThatATestLetsPass, checksandalerts.Check{Inputs: new(checksandalerts.SSH(waiting))}); o.Status != "failed" || o.Message != "the server at "+waiting+" sent no SSH greeting within the time limit of the run" || !reflect.DeepEqual(o.Details, []string{"the server sent: 220 mail.example.com ESMTP"}) {
		t.Fatal(o)
	}
}

// mailServer is a stand-in for a mail server that speaks one of the three
// protocols: it greets, answers the commands of a check, and switches to
// encryption when asked. With implicit, it is encrypted from the first
// byte. It returns its address as localhost:port, the name that its
// certificate is valid for.
func mailServer(t *testing.T, protocol string, certificate tls.Certificate, implicit bool) string {
	t.Helper()
	secure := &tls.Config{Certificates: []tls.Certificate{certificate}}
	return tcpServerAtLocalhost(t, func(conn net.Conn) {
		if implicit {
			encrypted := tls.Server(conn, secure)
			if encrypted.Handshake() != nil {
				return
			}
			conn = encrypted
		}
		greeting := map[string]string{"smtp": "220 mail.example.com ESMTP stand-in", "imap": "* OK IMAP4rev1 stand-in ready", "pop3": "+OK POP3 stand-in ready"}[protocol]
		conn.Write([]byte(greeting + "\r\n"))
		lines := bufio.NewReader(conn)
		for {
			line, err := lines.ReadString('\n')
			if err != nil {
				return
			}
			command, tag := strings.TrimSpace(line), ""
			if protocol == "imap" {
				tag, command, _ = strings.Cut(command, " ")
			}
			command = strings.ToUpper(command)
			switch {
			case strings.HasPrefix(command, "EHLO"):
				conn.Write([]byte("250-mail.example.com\r\n250-STARTTLS\r\n250 SIZE 1000\r\n"))
			case command == "STARTTLS" || command == "STLS":
				conn.Write([]byte(map[string]string{"smtp": "220 go ahead", "imap": tag + " OK begin TLS", "pop3": "+OK begin TLS"}[protocol] + "\r\n"))
				encrypted := tls.Server(conn, secure)
				if encrypted.Handshake() != nil {
					return
				}
				conn, lines = encrypted, bufio.NewReader(encrypted)
			case command == "QUIT" && protocol == "smtp":
				conn.Write([]byte("221 bye\r\n"))
				return
			case command == "QUIT":
				conn.Write([]byte("+OK bye\r\n"))
				return
			case command == "LOGOUT":
				conn.Write([]byte("* BYE logging out\r\n" + tag + " OK LOGOUT completed\r\n"))
				return
			default:
				conn.Write([]byte("500 unknown\r\n"))
			}
		}
	})
}

// The smtp, imap, and pop3 checks talk to a mail server with each kind of
// connection security, verify its certificate, and say which step failed.
func TestMailServerChecks(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	certificateFile, keyFile := testhelpers.TestCertificate(t, directory)
	certificate, err := tls.LoadX509KeyPair(certificateFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	check := func(protocol string, server checksandalerts.MailServer) checksandalerts.Check {
		switch protocol {
		case "smtp":
			return checksandalerts.Check{Inputs: &checksandalerts.SMTP{MailServer: server}}
		case "imap":
			return checksandalerts.Check{Inputs: &checksandalerts.IMAP{MailServer: server}}
		}
		return checksandalerts.Check{Inputs: &checksandalerts.POP3{MailServer: server}}
	}
	names := map[string]string{"smtp": "SMTP", "imap": "IMAP", "pop3": "POP3"}
	// Each protocol has servers of its own, and the time limit of a run
	// against a server that says nothing passes for each at once.
	for _, protocol := range []string{"smtp", "imap", "pop3"} {
		t.Run(protocol, func(t *testing.T) {
			t.Parallel()
			testEnds := make(chan struct{})
			t.Cleanup(func() { close(testEnds) })
			plain, implicit := mailServer(t, protocol, certificate, false), mailServer(t, protocol, certificate, true)
			if o := withinTimeLimit(t, timeLimitFarAboveTheRun, check(protocol, checksandalerts.MailServer{Address: plain, ConnectionSecurity: "unencrypted"})); o.Status != "ok" || o.Message != "the "+names[protocol]+" server at "+plain+" answers" {
				t.Errorf("%s unencrypted: %+v", protocol, o)
			}
			for security, address := range map[string]string{"starttls": plain, "implicit_tls": implicit} {
				trusted := checksandalerts.MailServer{Address: address, ConnectionSecurity: security, TrustedCertificateAuthorityFile: certificateFile}
				if o := withinTimeLimit(t, timeLimitFarAboveTheRun, check(protocol, trusted)); o.Status != "ok" || o.Message != "the "+names[protocol]+" server at "+address+" answers, and its certificate is valid" {
					t.Errorf("%s %s: %+v", protocol, security, o)
				}
				// Without the certificate authority file, the system's trust
				// store does not know the certificate.
				if o := withinTimeLimit(t, timeLimitFarAboveTheRun, check(protocol, checksandalerts.MailServer{Address: address, ConnectionSecurity: security})); o.Status != "failed" || !strings.Contains(o.Message, "certificate") {
					t.Errorf("%s %s with an unknown certificate: %+v", protocol, security, o)
				}
				skipped := trusted
				skipped.TrustedCertificateAuthorityFile = ""
				skipped.DangerouslySkipServerCertificateCheck = true
				if o := withinTimeLimit(t, timeLimitFarAboveTheRun, check(protocol, skipped)); o.Status != "ok" || strings.Contains(o.Message, "certificate is valid") {
					t.Fatal(o)
				}
				missing := trusted
				missing.TrustedCertificateAuthorityFile = filepath.Join(directory, "missing.pem")
				if o := withinTimeLimit(t, timeLimitFarAboveTheRun, check(protocol, missing)); o.Status != "unknown" || !strings.Contains(o.Message, "missing.pem") {
					t.Errorf("%s %s without its certificate authority file: %+v", protocol, security, o)
				}
				if problem := check(protocol, missing).Inputs.(checksandalerts.ValidatedOnNode).ValidateOnNode(); problem == nil || problem.Path != "trusted_certificate_authority_file" {
					t.Errorf("%s: runtime validation accepted a missing certificate authority file: %v", protocol, problem)
				}
			}
			// A server of another protocol, and one that says nothing.
			other := tcpServer(t, func(conn net.Conn) { conn.Write([]byte("SSH-2.0-OpenSSH_10.0\r\n")) })
			if o := withinTimeLimit(t, timeLimitFarAboveTheRun, check(protocol, checksandalerts.MailServer{Address: other, ConnectionSecurity: "unencrypted"})); o.Status != "failed" || !strings.HasPrefix(o.Message, "the "+names[protocol]+" server at "+other+": its greeting: ") {
				t.Errorf("%s against another protocol: %+v", protocol, o)
			}
			silent := tcpServer(t, func(conn net.Conn) { <-testEnds })
			if o := withinTimeLimit(t, testhelpers.TimeLimitThatATestLetsPass, check(protocol, checksandalerts.MailServer{Address: silent, ConnectionSecurity: "unencrypted"})); o.Status != "failed" || !strings.HasSuffix(o.Message, "its greeting: no answer within the time limit of the run") {
				t.Errorf("%s against a silent server: %+v", protocol, o)
			}
		})
	}
	// A mail server that does not offer STARTTLS.
	withoutStartTLS := tcpServer(t, func(conn net.Conn) {
		conn.Write([]byte("220 mail.example.com ESMTP\r\n"))
		lines := bufio.NewReader(conn)
		for {
			if _, err := lines.ReadString('\n'); err != nil {
				return
			}
			conn.Write([]byte("250 mail.example.com\r\n"))
		}
	})
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, check("smtp", checksandalerts.MailServer{Address: withoutStartTLS, ConnectionSecurity: "starttls"})); o.Status != "failed" || o.Message != "the SMTP server at "+withoutStartTLS+" does not offer STARTTLS, which connection_security: starttls requires" {
		t.Fatal(o)
	}
	// An IMAP and a POP3 server that refuse the switch to encryption, with
	// the words of Dovecot.
	imapWithoutStartTLS := tcpServer(t, func(conn net.Conn) {
		conn.Write([]byte("* OK [CAPABILITY IMAP4rev1 LOGINDISABLED] Dovecot ready.\r\n"))
		lines := bufio.NewReader(conn)
		for {
			line, err := lines.ReadString('\n')
			if err != nil {
				return
			}
			tag, _, _ := strings.Cut(line, " ")
			conn.Write([]byte(tag + " BAD TLS support isn't enabled.\r\n"))
		}
	})
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, check("imap", checksandalerts.MailServer{Address: imapWithoutStartTLS, ConnectionSecurity: "starttls"})); o.Status != "failed" ||
		o.Message != "the IMAP server at "+imapWithoutStartTLS+" refused to switch to encryption, which connection_security: starttls requires. Its answer to STARTTLS: BAD TLS support isn't enabled." {
		t.Fatal(o)
	}
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, check("imap", checksandalerts.MailServer{Address: imapWithoutStartTLS, ConnectionSecurity: "unencrypted"})); o.Status != "failed" || !strings.HasSuffix(o.Message, ": its answer to LOGOUT: m2 BAD TLS support isn't enabled.") {
		t.Fatal(o)
	}
	pop3WithoutStartTLS := tcpServer(t, func(conn net.Conn) {
		conn.Write([]byte("+OK Dovecot ready.\r\n"))
		lines := bufio.NewReader(conn)
		for {
			line, err := lines.ReadString('\n')
			if err != nil {
				return
			}
			if strings.TrimSpace(line) == "QUIT" {
				conn.Write([]byte("+OK Logging out\r\n"))
				return
			}
			conn.Write([]byte("-ERR TLS support isn't enabled.\r\n"))
		}
	})
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, check("pop3", checksandalerts.MailServer{Address: pop3WithoutStartTLS, ConnectionSecurity: "starttls"})); o.Status != "failed" ||
		o.Message != "the POP3 server at "+pop3WithoutStartTLS+" refused to switch to encryption, which connection_security: starttls requires. Its answer to STLS: -ERR TLS support isn't enabled." {
		t.Fatal(o)
	}
	// The same server answers a check that asks for no encryption.
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, check("pop3", checksandalerts.MailServer{Address: pop3WithoutStartTLS, ConnectionSecurity: "unencrypted"})); o.Status != "ok" {
		t.Fatal(o)
	}
	// A POP3 server that closes the connection in place of an answer.
	pop3ThatCloses := tcpServer(t, func(conn net.Conn) {
		conn.Write([]byte("+OK ready\r\n"))
		bufio.NewReader(conn).ReadString('\n')
	})
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, check("pop3", checksandalerts.MailServer{Address: pop3ThatCloses, ConnectionSecurity: "starttls"})); o.Status != "failed" || !strings.HasPrefix(o.Message, "the POP3 server at "+pop3ThatCloses+": its answer to STLS: ") {
		t.Fatal(o)
	}
	refusing := tcpServer(t, func(conn net.Conn) { conn.Write([]byte("* BYE too many connections\r\n")) })
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, check("imap", checksandalerts.MailServer{Address: refusing, ConnectionSecurity: "unencrypted"})); o.Status != "failed" || o.Message != "the IMAP server at "+refusing+": its greeting: expected a line that starts with * OK, received * BYE too many connections" {
		t.Fatal(o)
	}

	for _, protocol := range []string{"smtp", "imap", "pop3"} {
		loadChecks(t, map[string]string{
			"{" + protocol + ": {address: 'mail.example.com:993', connection_security: implicit_tls}, alert_severity: sos}":                                                         "",
			"{" + protocol + ": {address: 'mail.example.com:587', connection_security: starttls, trusted_certificate_authority_file: /etc/mmwatchdog/ca.pem}, alert_severity: sos}": "",
			"{" + protocol + ": {address: '127.0.0.1:25', connection_security: unencrypted}, alert_severity: sos}":                                                                  "",
			"{" + protocol + ": {address: 'mail.example.com:993'}, alert_severity: sos}":                                                                                            "check_groups.c." + protocol + ".connection_security: required: starttls, implicit_tls, or unencrypted",
			"{" + protocol + ": {address: 'mail.example.com:993', connection_security: tls}, alert_severity: sos}":                                                                  "check_groups.c." + protocol + ".connection_security: required: starttls, implicit_tls, or unencrypted",
			"{" + protocol + ": {connection_security: starttls}, alert_severity: sos}":                                                                                              "check_groups.c." + protocol + ".address: ",
			"{" + protocol + ": {address: 'mail.example.com:993', connection_security: starttls, trusted_certificate_authority_file: ca.pem}, alert_severity: sos}":                 "check_groups.c." + protocol + ".trusted_certificate_authority_file: must be absolute",
			"{" + protocol + ": {address: '127.0.0.1:25', connection_security: unencrypted, trusted_certificate_authority_file: /etc/mmwatchdog/ca.pem}, alert_severity: sos}":      "check_groups.c." + protocol + ".trusted_certificate_authority_file: has no use with connection_security: unencrypted, which verifies no certificate",
			"{" + protocol + ": 'mail.example.com:993', alert_severity: sos}":                                                                                                       "expected a mapping",
		})
	}
}

// dnsServer is a stand-in for a DNS server that knows the records in
// records, by "TYPE name". It answers over UDP, with each value of a record
// in the form that the kind of record has on the wire. For a name below
// tcp.example.com it says over UDP that its answer does not fit a packet,
// and gives the answer over TCP.
func dnsServer(t *testing.T, records map[string][]string) string {
	t.Helper()
	// The server answers over UDP and TCP on one port. The kernel picks a
	// free UDP port, whose TCP port another test can hold: then the next
	// free UDP port is tried.
	var server net.PacketConn
	var overTCP net.Listener
	for attempt := 1; ; attempt++ {
		var err error
		if server, err = net.ListenPacket("udp", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
		if overTCP, err = net.Listen("tcp", server.LocalAddr().String()); err == nil {
			break
		}
		server.Close()
		if attempt == 20 {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { server.Close() })
	t.Cleanup(func() { overTCP.Close() })
	types := map[uint16]string{1: "A", 2: "NS", 5: "CNAME", 12: "PTR", 15: "MX", 16: "TXT", 28: "AAAA", 33: "SRV"}
	// A name below this domain is refused, as a server refuses a name that
	// it is not there for.
	const refusedDomain = "elsewhere.example.org"
	// name writes a host name as DNS does: each label with its length. The
	// root name "." has no label.
	name := func(host string) []byte {
		written := []byte{}
		if host == "." {
			return []byte{0}
		}
		for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
			written = append(append(written, byte(len(label))), label...)
		}
		return append(written, 0)
	}
	answerTo := func(packet []byte, n int, fitsAPacket bool) []byte {
		question := packet[12:n]
		labels, end := []string{}, 0
		for question[end] != 0 {
			length := int(question[end])
			labels = append(labels, string(question[end+1:end+1+length]))
			end += 1 + length
		}
		asked := strings.Join(labels, ".")
		recordType := binary.BigEndian.Uint16(question[end+1:])
		question = question[:end+5]
		values, known := records[types[recordType]+" "+asked]
		answer := append([]byte{packet[0], packet[1], 0x84, 0x00, 0, 1, 0, byte(len(values)), 0, 0, 0, 0}, question...)
		switch {
		case strings.HasSuffix(asked, refusedDomain):
			answer[3] = 5
		case !known:
			// The name does not exist.
			answer[3] = 3
		case strings.HasSuffix(asked, "tcp.example.com") && fitsAPacket:
			// The answer does not fit a packet: ask again over TCP.
			answer[2], answer[7] = 0x86, 0
			return answer
		}
		for _, value := range values {
			var data []byte
			switch types[recordType] {
			case "A":
				data = net.ParseIP(value).To4()
			case "AAAA":
				data = net.ParseIP(value).To16()
			case "CNAME", "NS", "PTR":
				data = name(value)
			case "MX":
				data = append([]byte{0, 10}, name(value)...)
			case "TXT":
				data = append([]byte{byte(len(value))}, value...)
			case "SRV":
				host, port, _ := net.SplitHostPort(value)
				number, _ := net.LookupPort("tcp", port)
				data = append([]byte{0, 1, 0, 1, byte(number >> 8), byte(number)}, name(host)...)
			}
			// The name of the answer points back to the question.
			answer = append(answer, 0xc0, 12, byte(recordType>>8), byte(recordType), 0, 1, 0, 0, 0, 60, byte(len(data)>>8), byte(len(data)))
			answer = append(answer, data...)
		}
		return answer
	}
	go func() {
		packet := make([]byte, 1500)
		for {
			n, from, err := server.ReadFrom(packet)
			if err != nil || n < 12 {
				return
			}
			server.WriteTo(answerTo(packet, n, true), from)
		}
	}()
	go func() {
		for {
			conn, err := overTCP.Accept()
			if err != nil {
				return
			}
			// Over TCP each message has its length in front of it.
			var length [2]byte
			if _, err := io.ReadFull(conn, length[:]); err == nil {
				packet := make([]byte, binary.BigEndian.Uint16(length[:]))
				if _, err := io.ReadFull(conn, packet); err == nil {
					answer := answerTo(packet, len(packet), false)
					conn.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(answer))), answer...))
				}
			}
			conn.Close()
		}
	}()
	return server.LocalAddr().String()
}

// The null MX record (RFC 7505), whose host is the root name ".", says that
// a domain takes no mail, and an SRV record whose target is "." (RFC 2782)
// says that the service is not available. A dns check writes the root name
// as ".". Without expected values the answer fails the check, as the check
// asks where the mail or the service goes; with the expected value "." the
// check is healthy.
func TestDNSCheckOfANullMXAndOfAnSRVRecordOfNoService(t *testing.T) {
	t.Parallel()
	server := dnsServer(t, map[string][]string{
		"MX example.com": {"."}, "MX mixed.example.com": {".", "mail.example.com."}, "SRV _sip._tcp.example.com": {".:0"},
	})
	observe := func(dns checksandalerts.DNS) checksandalerts.Observation {
		dns.DNSServer = server
		return withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: &dns})
	}
	for _, tc := range []struct {
		dns     checksandalerts.DNS
		status  string
		message string
	}{
		{checksandalerts.DNS{RecordName: "example.com", RecordType: "MX"}, "failed",
			"the DNS server " + server + " answers that example.com takes no mail: its MX record is the null MX (.)"},
		{checksandalerts.DNS{RecordName: "mixed.example.com", RecordType: "MX"}, "failed",
			"the DNS server " + server + " answers that mixed.example.com takes no mail: one of its MX records is the null MX (.); its MX records are ., mail.example.com"},
		{checksandalerts.DNS{RecordName: "_sip._tcp.example.com", RecordType: "SRV"}, "failed",
			"the DNS server " + server + " answers that the service _sip._tcp.example.com is not available: its SRV record names no host (its target is .)"},
		// A domain that must take no mail, and a service that must not be
		// offered, expect the value ".".
		{checksandalerts.DNS{RecordName: "example.com", RecordType: "MX", ExpectedRecordValues: []string{"."}}, "ok",
			"the MX record of example.com at the DNS server " + server + " is ."},
		{checksandalerts.DNS{RecordName: "_sip._tcp.example.com", RecordType: "SRV", ExpectedRecordValues: []string{"."}}, "ok",
			"the SRV record of _sip._tcp.example.com at the DNS server " + server + " is ."},
		// "." is a value of its own, not an empty one: it matches no host.
		{checksandalerts.DNS{RecordName: "example.com", RecordType: "MX", ExpectedRecordValues: []string{"mail.example.com"}}, "failed",
			"the MX record of example.com at the DNS server " + server + " is ., expected mail.example.com"},
		{checksandalerts.DNS{RecordName: "mixed.example.com", RecordType: "MX", ExpectedRecordValues: []string{"."}}, "failed",
			"the MX record of mixed.example.com at the DNS server " + server + " is ., mail.example.com, expected ."},
	} {
		if o := observe(tc.dns); o.Status != tc.status || o.Message != tc.message {
			t.Errorf("%+v: %s %q", tc.dns, o.Status, o.Message)
		}
	}
}

// A dns check asks one server for the records of one kind for one name, and
// can compare the answer with the values that are expected.
func TestDNSCheck(t *testing.T) {
	t.Parallel()
	server := dnsServer(t, map[string][]string{
		"A www.example.com": {"192.0.2.11", "192.0.2.10"}, "AAAA www.example.com": {"2001:db8::10"}, "CNAME app.example.com": {"www.example.com."},
		"MX example.com": {"mail.example.com."}, "NS example.com": {"ns1.example.com.", "ns2.example.com."}, "TXT example.com": {"v=spf1 mx -all"},
		"PTR 10.2.0.192.in-addr.arpa": {"www.example.com."}, "SRV _sip._tcp.example.com": {"sip.example.com.:5060"}, "A empty.example.com": {},
		"A localhost": {"192.0.2.77"}, "PTR 1.0.0.127.in-addr.arpa": {"asked.example.com."}, "A *.example.com": {"192.0.2.50"},
		"PTR 0.1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa": {"six.example.com."}, "TXT long.tcp.example.com": {"an answer that came over TCP"},
	})
	observe := func(dns checksandalerts.DNS) checksandalerts.Observation {
		dns.DNSServer = server
		return withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: &dns})
	}
	for _, tc := range []struct {
		name, recordType string
		values           []string
	}{
		{"www.example.com", "A", []string{"192.0.2.10", "192.0.2.11"}}, {"www.example.com", "AAAA", []string{"2001:db8::10"}}, {"app.example.com", "CNAME", []string{"www.example.com"}},
		{"example.com", "MX", []string{"mail.example.com"}}, {"example.com", "NS", []string{"ns1.example.com", "ns2.example.com"}}, {"example.com", "TXT", []string{"v=spf1 mx -all"}},
		{"192.0.2.10", "PTR", []string{"www.example.com"}}, {"_sip._tcp.example.com", "SRV", []string{"sip.example.com:5060"}},
	} {
		want := "the " + tc.recordType + " record of " + tc.name + " at the DNS server " + server + " is " + strings.Join(tc.values, ", ")
		if o := observe(checksandalerts.DNS{RecordName: tc.name, RecordType: tc.recordType}); o.Status != "ok" || o.Message != want {
			t.Errorf("%s %s: %s %q", tc.recordType, tc.name, o.Status, o.Message)
		}
		// The answer holds exactly the expected values, in any order and in
		// any way of writing them.
		expected := []string{}
		for i := len(tc.values) - 1; i >= 0; i-- {
			expected = append(expected, strings.ToUpper(tc.values[i])+map[bool]string{true: "."}[tc.recordType == "MX" || tc.recordType == "NS"])
		}
		if tc.recordType == "TXT" {
			expected = tc.values
		}
		if o := observe(checksandalerts.DNS{RecordName: tc.name, RecordType: tc.recordType, ExpectedRecordValues: expected}); o.Status != "ok" || o.Message != want {
			t.Errorf("%s %s with the expected values %v: %s %q", tc.recordType, tc.name, expected, o.Status, o.Message)
		}
	}
	for _, expected := range []string{"V=SPF1 MX -ALL", "v=spf1 mx -all."} {
		if o := observe(checksandalerts.DNS{RecordName: "example.com", RecordType: "TXT", ExpectedRecordValues: []string{expected}}); o.Status != "failed" {
			t.Fatal(o)
		}
	}
	// Another value, a value too many, and a value too few all fail.
	for _, expected := range [][]string{{"192.0.2.10.", "192.0.2.11"}, {"192.0.2.99"}, {"192.0.2.10"}, {"192.0.2.10", "192.0.2.11", "192.0.2.12"}} {
		o := observe(checksandalerts.DNS{RecordName: "www.example.com", RecordType: "A", ExpectedRecordValues: expected})
		if want := "the A record of www.example.com at the DNS server " + server + " is 192.0.2.10, 192.0.2.11, expected " + strings.Join(expected, ", "); o.Status != "failed" || o.Message != want {
			t.Errorf("%v: %s %q", expected, o.Status, o.Message)
		}
	}
	// The name is complete as written: MMWatchdog adds no search domain to it.
	for _, dns := range []checksandalerts.DNS{{RecordName: "missing.example.com", RecordType: "A"}, {RecordName: "www.example.com", RecordType: "MX"}, {RecordName: "www.example.com", RecordType: "CNAME"}, {RecordName: "empty.example.com", RecordType: "A"}} {
		if o := observe(dns); o.Status != "failed" || o.Message != "the DNS server "+server+" has no "+dns.RecordType+" record of "+dns.RecordName {
			t.Errorf("%+v: %s %q", dns, o.Status, o.Message)
		}
	}
	// A server that refuses the question. The message names the server
	// that MMWatchdog asked, and no DNS server of this machine's own settings.
	if o := observe(checksandalerts.DNS{RecordName: "www.elsewhere.example.org", RecordType: "A"}); o.Status != "failed" ||
		o.Message != "MMWatchdog got no A record of www.elsewhere.example.org at the DNS server "+server+": the DNS server refused the query" {
		t.Fatal(o)
	}
	// The answer is the one of the server that the check names. The hosts
	// file and the resolver of this machine take no part: this machine has
	// its own address for localhost and its own name for 127.0.0.1.
	for _, tc := range []struct{ name, recordType, value string }{
		{"localhost", "A", "192.0.2.77"}, {"127.0.0.1", "PTR", "asked.example.com"}, {"2001:db8::10", "PTR", "six.example.com"},
		// A name with a star is asked for as it is written.
		{"*.example.com", "A", "192.0.2.50"},
		// An answer that does not fit a UDP packet is asked for again over TCP.
		{"long.tcp.example.com", "TXT", "an answer that came over TCP"},
	} {
		want := "the " + tc.recordType + " record of " + tc.name + " at the DNS server " + server + " is " + tc.value
		if o := observe(checksandalerts.DNS{RecordName: tc.name, RecordType: tc.recordType}); o.Status != "ok" || o.Message != want {
			t.Errorf("%s %s: %s %q", tc.recordType, tc.name, o.Status, o.Message)
		}
	}
	// A server that is named by a host name with several addresses is
	// asked at each address until one answers: the stand-in listens on
	// 127.0.0.1 only, and localhost is also ::1.
	_, port, _ := net.SplitHostPort(server)
	if o := withinTimeLimit(t, 4*time.Second, checksandalerts.Check{Inputs: &checksandalerts.DNS{DNSServer: "localhost:" + port, RecordName: "www.example.com", RecordType: "AAAA"}}); o.Status != "ok" ||
		o.Message != "the AAAA record of www.example.com at the DNS server localhost:"+port+" is 2001:db8::10" {
		t.Fatal(o)
	}
	// A name that a DNS question cannot hold is refused when the
	// configuration is loaded.
	for name, want := range map[string]string{
		strings.Repeat("a", 64) + ".example.com":         "each part of a DNS name between dots must have 1 to 63 characters",
		"www..example.com":                               "each part of a DNS name between dots must have 1 to 63 characters",
		"bücher.example.com":                             "a DNS name must contain only printable ASCII characters without spaces; use the xn-- form for a name with non-ASCII characters",
		strings.Repeat("abcdefghi.", 25) + "example.com": "has 261 characters: a DNS name has at most 253 characters",
		"www example.com":                                "holds a space, which a DNS name cannot hold",
		"www\texample.com":                               "holds a tab, which a DNS name cannot hold",
		"www.example.com\n":                              "holds a line break, which a DNS name cannot hold",
		"www.example.com/index.html":                     "holds a slash, which a DNS name cannot hold",
	} {
		if e := (&checksandalerts.DNS{DNSServer: "10.0.0.2:53", RecordName: name, RecordType: "A"}).Validate(checksandalerts.InputValidation{Authored: func(string) bool { return false }}); e == nil || e.Path != "record_name" || e.Message != want {
			t.Errorf("%s: %+v", name, e)
		}
	}
	// A server that does not answer.
	silent, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer silent.Close()
	if o := withinTimeLimit(t, testhelpers.TimeLimitThatATestLetsPass, checksandalerts.Check{Inputs: &checksandalerts.DNS{DNSServer: silent.LocalAddr().String(), RecordName: "www.example.com", RecordType: "A"}}); o.Status != "failed" ||
		o.Message != "the DNS server "+silent.LocalAddr().String()+" did not answer within the time limit of the run" {
		t.Fatal(o)
	}
	if _, err := primitives.DNSLookup(context.Background(), server, "www.example.com", "SOA"); err == nil || err.Error() != "unsupported DNS record type SOA" {
		t.Fatal(err)
	}

	loadChecks(t, map[string]string{
		"{dns: {dns_server: '10.0.0.2:53', record_name: app.internal.example.com, record_type: A, expected_record_values: [10.0.0.5]}, alert_severity: sos}": "",
		"{dns: {dns_server: '10.0.0.2:53', record_name: example.com, record_type: TXT, expected_record_values: [text, TEXT, 'text.']}, alert_severity: sos}": "",
		"{dns: {dns_server: '10.0.0.2:53', record_name: example.com, record_type: MX}, alert_severity: sos}":                                                 "",
		"{dns: {dns_server: '10.0.0.2:53', record_name: 192.0.2.10, record_type: PTR}, alert_severity: sos}":                                                 "",
		"{dns: {record_name: example.com, record_type: A}, alert_severity: sos}":                                                                             "check_groups.c.dns.dns_server: is required: the DNS server that the check asks, as host:port, such as 10.0.0.2:53",
		"{dns: {dns_server: 10.0.0.2, record_name: example.com, record_type: A}, alert_severity: sos}":                                                       "check_groups.c.dns.dns_server: is required",
		"{dns: {dns_server: '10.0.0.2:53', record_type: A}, alert_severity: sos}":                                                                            "check_groups.c.dns.record_name: is required: the name that the check asks for, such as www.example.com",
		"{dns: {dns_server: '10.0.0.2:53', record_name: example.com}, alert_severity: sos}":                                                                  "check_groups.c.dns.record_type: must be one of A, AAAA, CNAME, MX, NS, TXT, PTR, SRV",
		"{dns: {dns_server: '10.0.0.2:53', record_name: example.com, record_type: a}, alert_severity: sos}":                                                  "check_groups.c.dns.record_type: must be one of A, AAAA, CNAME, MX, NS, TXT, PTR, SRV",
		"{dns: {dns_server: '10.0.0.2:53', record_name: example.com, record_type: PTR}, alert_severity: sos}":                                                "check_groups.c.dns.record_name: must be an address for record_type: PTR, such as 192.0.2.10",
		"{dns: {dns_server: '10.0.0.2:53', record_name: example.com, record_type: A, expected_record_values: []}, alert_severity: sos}":                      "check_groups.c.dns.expected_record_values: cannot be empty",
		"{dns: {dns_server: '10.0.0.2:53', record_name: example.com, record_type: A, expected_record_values: [10.0.0.5, 10.0.0.5]}, alert_severity: sos}":    "check_groups.c.dns.expected_record_values: expected distinct values that are not empty",
		"{dns: {dns_server: '10.0.0.2:53', record_name: example.com, record_type: A, expected_record_values: 10.0.0.5}, alert_severity: sos}":                `check_groups.c.dns.expected_record_values: expected a list, found the string "10.0.0.5"`,
	})
	// The order of the expected values is no part of what the check is.
	a, b := &checksandalerts.DNS{DNSServer: "10.0.0.2:53", RecordName: "x", RecordType: "A", ExpectedRecordValues: []string{"10.0.0.6", "10.0.0.5"}}, &checksandalerts.DNS{DNSServer: "10.0.0.2:53", RecordName: "x", RecordType: "A", ExpectedRecordValues: []string{"10.0.0.5", "10.0.0.6"}}
	if !reflect.DeepEqual(a.Identity(), b.Identity()) || a.ExpectedRecordValues[0] != "10.0.0.6" {
		t.Fatal(a.Identity(), b.Identity())
	}
}

// A memcached check asks for the server's statistics. It reports the
// connections, the items, and how full the cache is, as the server gives
// them, and the hits, the misses, and the evictions as the growth of the
// server's counters since the run before, so that a cold cache and a warm
// cache look different whatever the server counted since it started.
func TestMemcachedCheck(t *testing.T) {
	t.Parallel()
	var statistics atomic.Value
	// serverHasCounted makes the stand-in server give these counters: the
	// gets that found their item, the gets that found none, the items that
	// it removed to make room, and the seconds since it started.
	serverHasCounted := func(hits, misses, evictions, uptime int) {
		statistics.Store(fmt.Sprintf("STAT pid 812\r\nSTAT uptime %d\r\nSTAT version 1.6.38\r\nSTAT curr_connections 12\r\nSTAT get_hits %d\r\nSTAT get_misses %d\r\n"+
			"STAT curr_items 400\r\nSTAT bytes 16777216\r\nSTAT limit_maxbytes 67108864\r\nSTAT evictions %d\r\nEND\r\n", uptime, hits, misses, evictions))
	}
	address := tcpServer(t, func(conn net.Conn) {
		if line, _ := bufio.NewReader(conn).ReadString('\n'); line == "stats\r\n" {
			conn.Write([]byte(statistics.Load().(string)))
		}
	})
	// The scheduled runs of one check, which keep the counters of each run
	// for the next one.
	run := checksandalerts.NewCheckRun(checksandalerts.Check{Inputs: new(checksandalerts.Memcached(address)), FailWhenMeasurementAbove: map[string]float64{"cache_used_percent": 90}},
		checksandalerts.Timing{TimeLimitPerRun: checksandalerts.Duration(timeLimitFarAboveTheRun.String())})
	observe := func() checksandalerts.Observation { return checksandalerts.RunCheck(testhelpers.TestContext(t), run) }

	// The first run asks the server twice, 200 milliseconds apart. The
	// counters did not grow in between, so there is no hit ratio.
	serverHasCounted(0, 200, 10, 4990)
	o := observe()
	if o.Status != "ok" || o.SampleWindow < checksandalerts.CPUSampleWindow || o.Measurements["cache_hits_per_second"] != (checksandalerts.Measurement{Value: 0, Unit: "hits per second"}) || len(o.Measurements) != 6 {
		t.Fatal(o)
	}
	// Since the run before, 900 gets found their item and 100 found none,
	// and the server removed 20 items: 90 percent of these gets were hits.
	serverHasCounted(900, 300, 30, 5000)
	o = observe()
	seconds := o.SampleWindow.Seconds()
	if o.Status != "ok" || o.Message != fmt.Sprintf("cache_used_percent is 25 (fails above 90); the memcached server at %s gave its statistics; hits, misses, and evictions over %.3fs", address, seconds) ||
		o.Measurements["current_connections"] != (checksandalerts.Measurement{Value: 12, Unit: "connections"}) ||
		o.Measurements["cached_items"] != (checksandalerts.Measurement{Value: 400, Unit: "items"}) || o.Measurements["cache_used_percent"] != (checksandalerts.Measurement{Value: 25, Unit: "percent"}) ||
		o.Measurements["cache_hits_per_second"] != (checksandalerts.Measurement{Value: 900 / seconds, Unit: "hits per second"}) ||
		o.Measurements["cache_misses_per_second"] != (checksandalerts.Measurement{Value: 100 / seconds, Unit: "misses per second"}) ||
		o.Measurements["cache_evictions_per_second"] != (checksandalerts.Measurement{Value: 20 / seconds, Unit: "evictions per second"}) ||
		o.Measurements["cache_hit_percent"] != (checksandalerts.Measurement{Value: 90, Unit: "percent"}) {
		t.Fatal(o)
	}
	// A cold cache: since the run before, every get found no item. Then a
	// warm cache: every get found its item. Since the server started, it
	// counted 75 percent hits.
	run.Check.FailWhenMeasurementBelow = map[string]float64{"cache_hit_percent": 80}
	serverHasCounted(900, 600, 30, 5010)
	if o := observe(); o.Status != "failed" || !strings.HasPrefix(o.Message, "cache_hit_percent is 0 (fails below 80); cache_used_percent is 25 (fails above 90); the memcached server at ") {
		t.Fatal(o)
	}
	serverHasCounted(1800, 600, 30, 5020)
	if o := observe(); o.Status != "ok" || o.Measurements["cache_hit_percent"].Value != 100 || o.Measurements["cache_misses_per_second"].Value != 0 {
		t.Fatal(o)
	}
	// No get since the run before: there is no hit ratio, and the limit on
	// it makes the run unknown. A check without that limit is healthy.
	if o := observe(); o.Status != "unknown" || !strings.HasPrefix(o.Message, "the memcached server answered no gets in the last ") || !strings.HasSuffix(o.Message, "s, so cache_hit_percent cannot be measured") {
		t.Fatal(o)
	}
	run.Check.FailWhenMeasurementBelow = nil
	if o := observe(); o.Status != "ok" || o.Measurements["cache_hits_per_second"].Value != 0 || len(o.Measurements) != 6 {
		t.Fatal(o)
	}
	// A server that started again counts from zero, and the check takes a
	// fresh sample, in which these counters do not grow. So does a server
	// that started again and has already counted more than the one before:
	// only its seconds since the start fell back.
	serverHasCounted(5, 1, 0, 2)
	if o := observe(); o.Status != "ok" || o.SampleWindow < checksandalerts.CPUSampleWindow || o.Measurements["cache_hits_per_second"].Value != 0 {
		t.Fatal(o)
	}
	serverHasCounted(5000, 5000, 5000, 1)
	if o := observe(); o.Status != "ok" || o.SampleWindow < checksandalerts.CPUSampleWindow || o.Measurements["cache_hits_per_second"].Value != 0 || o.Measurements["cache_evictions_per_second"].Value != 0 {
		t.Fatal(o)
	}

	// A run of mmwatchdog check-now takes its own sample.
	check := checksandalerts.Check{Inputs: new(checksandalerts.Memcached(address)), FailWhenMeasurementAbove: map[string]float64{"current_connections": 10}}
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, check); o.Status != "failed" || !strings.HasPrefix(o.Message, "current_connections is 12 (fails above 10); ") || o.SampleWindow < checksandalerts.CPUSampleWindow {
		t.Fatal(o)
	}
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: new(checksandalerts.Memcached(address))}); o.Status != "ok" ||
		!strings.HasPrefix(o.Message, "the memcached server at "+address+" gave its statistics; hits, misses, and evictions over ") || !strings.HasSuffix(o.Message, "s") {
		t.Fatal(o)
	}
	// A server that gives its statistics without the counters of gets is
	// healthy. A limit on what they measure makes the run unknown.
	withoutCounters := tcpServer(t, func(conn net.Conn) {
		bufio.NewReader(conn).ReadString('\n')
		conn.Write([]byte("STAT curr_connections 3\r\nSTAT bytes 0\r\nSTAT limit_maxbytes 1024\r\nEND\r\n"))
	})
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: new(checksandalerts.Memcached(withoutCounters))}); o.Status != "ok" || o.Message != "the memcached server at "+withoutCounters+" gave its statistics" || len(o.Measurements) != 2 {
		t.Fatal(o)
	}
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: new(checksandalerts.Memcached(withoutCounters)), FailWhenMeasurementAbove: map[string]float64{"cache_misses_per_second": 50}}); o.Status != "unknown" || o.Message != "the memcached server gave no get_hits" {
		t.Fatal(o)
	}
	withoutBytes := tcpServer(t, func(conn net.Conn) {
		bufio.NewReader(conn).ReadString('\n')
		conn.Write([]byte("STAT limit_maxbytes 1024\r\nEND\r\n"))
	})
	for _, limited := range []bool{false, true} {
		check := checksandalerts.Check{Inputs: new(checksandalerts.Memcached(withoutBytes))}
		if limited {
			check.FailWhenMeasurementAbove = map[string]float64{"cache_used_percent": 90}
		}
		o := withinTimeLimit(t, timeLimitFarAboveTheRun, check)
		_, recorded := o.Measurements["cache_used_percent"]
		if recorded || (!limited && o.Status != "ok") || (limited && (o.Status != "unknown" || o.Message != "the memcached server gave no bytes")) {
			t.Fatal(o)
		}
	}
	other := tcpServer(t, func(conn net.Conn) {
		bufio.NewReader(conn).ReadString('\n')
		conn.Write([]byte("-ERR unknown command 'stats'\r\n"))
	})
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: new(checksandalerts.Memcached(other))}); o.Status != "failed" {
		t.Fatal(o)
	}
	refusing := tcpServer(t, func(conn net.Conn) {
		bufio.NewReader(conn).ReadString('\n')
		conn.Write([]byte("ERROR\r\n"))
	})
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: new(checksandalerts.Memcached(refusing))}); o.Status != "failed" || o.Message != "the server at "+refusing+" did not answer with the statistics of a memcached server" || !reflect.DeepEqual(o.Details, []string{"the server sent: ERROR"}) {
		t.Fatal(o)
	}
}

// A redis check sends PING and expects PONG, after a login when it states a
// password file. An answer that says what is wrong fails the check with the
// server's words.
func TestRedisCheck(t *testing.T) {
	t.Parallel()
	// A stand-in that reads commands as Redis does: a number of words, and
	// each word with its length.
	command := func(lines *bufio.Reader) []string {
		header, err := lines.ReadString('\n')
		if err != nil || !strings.HasPrefix(header, "*") {
			return nil
		}
		words := []string{}
		for range int(header[1] - '0') {
			lines.ReadString('\n')
			word, _ := lines.ReadString('\n')
			words = append(words, strings.TrimSuffix(word, "\r\n"))
		}
		return words
	}
	server := func(password string) string {
		return tcpServer(t, func(conn net.Conn) {
			lines, loggedIn := bufio.NewReader(conn), password == ""
			for {
				words := command(lines)
				switch {
				case words == nil:
					return
				case words[0] == "AUTH" && words[len(words)-1] == password && (len(words) == 2 || words[1] == "mmwatchdog"):
					loggedIn = true
					conn.Write([]byte("+OK\r\n"))
				case words[0] == "AUTH":
					conn.Write([]byte("-WRONGPASS invalid username-password pair or user is disabled.\r\n"))
				case words[0] == "PING" && loggedIn:
					conn.Write([]byte("+PONG\r\n"))
				default:
					conn.Write([]byte("-NOAUTH Authentication required.\r\n"))
				}
			}
		})
	}
	open, protected := server(""), server("a password with spaces\tand $1")
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: &checksandalerts.Redis{ConnectionSecurity: "unencrypted", Address: open}}); o.Status != "ok" || o.Message != "the Redis server at "+open+" answered PING" {
		t.Fatal(o)
	}
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: &checksandalerts.Redis{ConnectionSecurity: "unencrypted", Address: protected}}); o.Status != "failed" || o.Message != "the Redis server at "+protected+" answered: NOAUTH Authentication required." {
		t.Fatal(o)
	}
	passwordFile := filepath.Join(t.TempDir(), "redis")
	os.WriteFile(passwordFile, []byte("a password with spaces\tand $1\n"), 0600)
	// A password is one line without a tab, as every password file of MMWatchdog.
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: &checksandalerts.Redis{ConnectionSecurity: "unencrypted", Address: protected, LoginPasswordFile: passwordFile}}); o.Status != "unknown" || !strings.Contains(o.Message, "expected one line with the password") {
		t.Fatal(o)
	}
	protected = server("a password with spaces and $1")
	os.WriteFile(passwordFile, []byte("a password with spaces and $1\n"), 0600)
	for _, redis := range []checksandalerts.Redis{{ConnectionSecurity: "unencrypted", LoginPasswordFile: passwordFile}, {ConnectionSecurity: "unencrypted", LoginUser: "mmwatchdog", LoginPasswordFile: passwordFile}} {
		redis.Address = protected
		if o := withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: &redis}); o.Status != "ok" {
			t.Fatal(o)
		}
	}
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: &checksandalerts.Redis{ConnectionSecurity: "unencrypted", Address: protected, LoginUser: "other", LoginPasswordFile: passwordFile}}); o.Status != "failed" || o.Message != "the Redis server at "+protected+" answered: WRONGPASS invalid username-password pair or user is disabled." {
		t.Fatal(o)
	}
	os.WriteFile(passwordFile, []byte("another password\n"), 0600)
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: &checksandalerts.Redis{ConnectionSecurity: "unencrypted", Address: protected, LoginPasswordFile: passwordFile}}); o.Status != "failed" || strings.Contains(o.Message+strings.Join(o.Details, ""), "another password") {
		t.Fatal(o)
	}
	// A password file that cannot be read is unknown, and runtime
	// validation names it.
	missing := &checksandalerts.Redis{ConnectionSecurity: "unencrypted", Address: open, LoginPasswordFile: filepath.Join(t.TempDir(), "missing")}
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: missing}); o.Status != "unknown" {
		t.Fatal(o)
	}
	if problem := missing.ValidateOnNode(); problem == nil || problem.Path != "login_password_file" {
		t.Fatal(problem)
	}
	other := tcpServer(t, func(conn net.Conn) { conn.Write([]byte("220 mail.example.com ESMTP\r\n")) })
	if o := withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: &checksandalerts.Redis{ConnectionSecurity: "unencrypted", Address: other}}); o.Status != "failed" || o.Message != "the server at "+other+" did not answer PING as a Redis server does" || !reflect.DeepEqual(o.Details, []string{"the server sent: 220 mail.example.com ESMTP"}) {
		t.Fatal(o)
	}
	if got := checksandalerts.RedisCommand("AUTH", "mmwatchdog", "p w"); got != "*3\r\n$4\r\nAUTH\r\n$10\r\nmmwatchdog\r\n$3\r\np w\r\n" {
		t.Fatalf("%q", got)
	}
}

// A server that does not behave as the check expects: one that answers a
// part and then waits, one that sends without end, one that repeats what it
// was sent, and one that is not there. Each check says what happened, and
// none takes more of the server's answer than its limit.
func TestChecksOfServersThatDoNotAnswerAsExpected(t *testing.T) {
	t.Parallel()
	testEnds := make(chan struct{})
	t.Cleanup(func() { close(testEnds) })

	// A server that sends a part of its answer and then waits: the check
	// says that the time limit ended the run, and shows what came.
	waits := tcpServer(t, func(conn net.Conn) {
		conn.Write([]byte("STAT pid 812\r\n"))
		<-testEnds
	})
	o := withinTimeLimit(t, testhelpers.TimeLimitThatATestLetsPass, checksandalerts.Check{Inputs: new(checksandalerts.Memcached(waits))})
	if o.Status != "failed" || o.Message != "the server at "+waits+" did not answer within the time limit of the run" || !reflect.DeepEqual(o.Details, []string{"the server sent: STAT pid 812"}) {
		t.Fatal(o)
	}
	half := tcpServer(t, func(conn net.Conn) {
		conn.Write([]byte("+PO"))
		<-testEnds
	})
	o = withinTimeLimit(t, testhelpers.TimeLimitThatATestLetsPass, checksandalerts.Check{Inputs: &checksandalerts.Redis{ConnectionSecurity: "unencrypted", Address: half}})
	if o.Status != "failed" || o.Message != "the server at "+half+" did not answer within the time limit of the run" || !reflect.DeepEqual(o.Details, []string{"the server sent: +PO"}) {
		t.Fatal(o)
	}

	// A server that is no Redis server and repeats what it was sent: the
	// password of the check is in no message and in no detail.
	passwordFile := filepath.Join(t.TempDir(), "redis")
	os.WriteFile(passwordFile, []byte("a-secret-password\n"), 0600)
	repeats := tcpServer(t, func(conn net.Conn) {
		sent := make([]byte, 4096)
		n, _ := conn.Read(sent)
		conn.Write([]byte("-ERR unknown command 'AUTH', with args beginning with: " + strings.ReplaceAll(string(sent[:n]), "\r\n", " ") + "\r\n+what\r\n"))
	})
	o = withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: &checksandalerts.Redis{ConnectionSecurity: "unencrypted", Address: repeats, LoginPasswordFile: passwordFile}})
	if o.Status != "failed" || !strings.Contains(o.Message, "(the password)") || strings.Contains(o.Message+strings.Join(o.Details, " "), "a-secret-password") {
		t.Fatal(o)
	}

	// A port where nothing listens: a tcp check that expects a reply says
	// that the connection did not open, not that the server sent no reply.
	nothing := testhelpers.AddressWhereNothingListens(t, "tcp")
	o = withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: &checksandalerts.TCP{Address: nothing, TextToSend: "STATUS\n", ReplyRules: checksandalerts.ReplyRules{ReplyContains: "READY"}}})
	if o.Status != "failed" || !strings.Contains(o.Message, "connection refused") || strings.Contains(o.Message, "sent no reply") {
		t.Fatal(o)
	}
	for _, check := range []checksandalerts.Check{{Inputs: new(checksandalerts.Memcached(nothing))}, {Inputs: &checksandalerts.Redis{ConnectionSecurity: "unencrypted", Address: nothing}}} {
		if o := withinTimeLimit(t, timeLimitFarAboveTheRun, check); o.Status != "failed" || !strings.Contains(o.Message, "connection refused") || o.Details != nil {
			t.Fatal(o)
		}
	}

	// A mail server that sends lines of its greeting without end: the
	// check stops reading at its limit, and long before its time limit.
	var sent atomic.Int64
	endless := tcpServer(t, func(conn net.Conn) {
		line := []byte("220-" + strings.Repeat("x", 996) + "\r\n")
		for {
			n, err := conn.Write(line)
			sent.Add(int64(n))
			if err != nil {
				return
			}
		}
	})
	started := time.Now()
	o = withinTimeLimit(t, 20*time.Second, checksandalerts.Check{Inputs: &checksandalerts.SMTP{MailServer: checksandalerts.MailServer{Address: endless, ConnectionSecurity: "unencrypted"}}})
	if o.Status != "failed" || !strings.Contains(o.Message, "the server sent more data than MMWatchdog reads from the server") || time.Since(started) > 10*time.Second {
		t.Fatal(o, time.Since(started))
	}
	// An IMAP and a POP3 server whose greeting has no end: the check says
	// so, not that the server closed the connection.
	for greeting, inputs := range map[string]func(string) checksandalerts.CheckInputs{
		"* OK ": func(address string) checksandalerts.CheckInputs {
			return &checksandalerts.IMAP{MailServer: checksandalerts.MailServer{Address: address, ConnectionSecurity: "unencrypted"}}
		},
		"+OK ": func(address string) checksandalerts.CheckInputs {
			return &checksandalerts.POP3{MailServer: checksandalerts.MailServer{Address: address, ConnectionSecurity: "unencrypted"}}
		},
	} {
		without := tcpServer(t, func(conn net.Conn) {
			conn.Write([]byte(greeting))
			for {
				if _, err := conn.Write([]byte(strings.Repeat("x", 1000))); err != nil {
					return
				}
			}
		})
		o = withinTimeLimit(t, 20*time.Second, checksandalerts.Check{Inputs: inputs(without)})
		if o.Status != "failed" || !strings.HasSuffix(o.Message, ": its greeting: the server sent more data than MMWatchdog reads from the server") {
			t.Fatal(o)
		}
	}

	// A server that is named by a host name with several addresses: the
	// stand-in listens on 127.0.0.1 only, and localhost is also ::1. A udp
	// check sends to each address until one answers.
	echo, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		packet := make([]byte, 1500)
		for {
			n, from, err := echo.ReadFrom(packet)
			if err != nil {
				return
			}
			echo.WriteTo([]byte("you sent "+string(packet[:n])), from)
		}
	}()
	_, port, _ := net.SplitHostPort(echo.LocalAddr().String())
	o = withinTimeLimit(t, 4*time.Second, checksandalerts.Check{Inputs: &checksandalerts.UDP{Address: "localhost:" + port, TextToSend: "STATUS", ReplyRules: checksandalerts.ReplyRules{ReplyContains: "you sent STATUS"}}})
	if o.Status != "ok" {
		t.Fatal(o)
	}
}
