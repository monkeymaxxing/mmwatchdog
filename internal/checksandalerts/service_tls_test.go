package checksandalerts_test

import (
	"bufio"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/testhelpers"
)

func TestRedisTLS(t *testing.T) {
	t.Parallel()
	certificateFile, keyFile := testhelpers.TestCertificate(t, t.TempDir())
	certificate, err := tls.LoadX509KeyPair(certificateFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	address := tcpServerAtLocalhost(t, func(raw net.Conn) {
		connection := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
		defer connection.Close()
		// The password must arrive inside TLS, before PING.
		reader := bufio.NewReader(connection)
		for _, word := range strings.Split(checksandalerts.RedisCommand("AUTH", "secret")+checksandalerts.RedisCommand("PING"), "\r\n") {
			if word == "" {
				continue
			}
			line, err := reader.ReadString('\n')
			if err != nil || strings.TrimSuffix(line, "\r\n") != word {
				return
			}
		}
		connection.Write([]byte("+OK\r\n+PONG\r\n"))
	})
	password := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(password, []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		authorities string
		skip        bool
		status      string
	}{
		{certificateFile, false, "ok"}, {"", false, "failed"}, {"", true, "ok"},
	} {
		redis := &checksandalerts.Redis{Address: address, ConnectionSecurity: "verified_tls", TrustedCertificateAuthorityFile: test.authorities, DangerouslySkipServerCertificateCheck: test.skip, LoginPasswordFile: password}
		if observation := withinTimeLimit(t, timeLimitFarAboveTheRun, checksandalerts.Check{Inputs: redis}); observation.Status != test.status {
			t.Fatal(test, observation)
		}
	}
}

func TestServiceInputValidation(t *testing.T) {
	t.Parallel()
	loadChecks(t, map[string]string{
		"{tcp: ':5432', alert_severity: sos}":                                                                                                       "the address has no host: write the host and port, such as 127.0.0.1:5432",
		"{http: {url: 'http://example.com', response_body_contains: ''}, alert_severity: sos}":                                                      "http.response_body_contains: cannot be empty",
		"{http: {url: 'http://example.com', response_body_does_not_contain: ''}, alert_severity: sos}":                                              "http.response_body_does_not_contain: cannot be empty",
		"{http: {url: 'http://example.com', response_body_matches_regex: ''}, alert_severity: sos}":                                                 "http.response_body_matches_regex: cannot be empty",
		"{redis: {address: '127.0.0.1:6379'}, alert_severity: sos}":                                                                                 "redis.connection_security: is required",
		"{redis: {address: '127.0.0.1:6379', connection_security: starttls}, alert_severity: sos}":                                                  "redis.connection_security: is required",
		"{redis: {address: '127.0.0.1:6379', connection_security: unencrypted, trusted_certificate_authority_file: relative}, alert_severity: sos}": "trusted_certificate_authority_file: must be absolute",
		"{redis: {address: '127.0.0.1:6379', connection_security: unencrypted, trusted_certificate_authority_file: /ca}, alert_severity: sos}":      "trusted_certificate_authority_file: has no use without TLS",
	})
}
