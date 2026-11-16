package primitives_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"mmwatchdog/internal/primitives"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestServerTLSCertificatePolicy(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	for _, tc := range []struct {
		roots          *x509.CertPool
		skip, accepted bool
	}{
		{roots, false, true}, {nil, false, false}, {nil, true, true},
	} {
		connection, err := tls.Dial("tcp", server.Listener.Addr().String(), primitives.ServerTLSConfig(tc.roots, tc.skip))
		if connection != nil {
			connection.Close()
		}
		if (err == nil) != tc.accepted {
			t.Fatalf("skip %t: %v", tc.skip, err)
		}
	}
}

// A user can give a TLS handshake more than Go's transport default of ten
// seconds. The server holds the handshake until the test has crossed that
// default; the test then releases it, without a deadline on server work.
func TestHTTPRequestUsesOnlyTheCallersTimeLimit(t *testing.T) {
	t.Parallel()
	handshakeStarted, allowHandshake := make(chan struct{}), make(chan struct{})
	var release sync.Once
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ready")) }))
	server.TLS = &tls.Config{GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		close(handshakeStarted)
		<-allowHandshake
		return nil, nil
	}}
	server.StartTLS()
	defer func() { release.Do(func() { close(allowHandshake) }); server.Close() }()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		response, err := primitives.HTTPRequest(ctx, primitives.HTTPRequestToSend{Method: "GET", URL: server.URL, CertificateAuthorities: roots})
		if err == nil && string(response.Body) != "ready" {
			err = fmt.Errorf("body: %q", response.Body)
		}
		finished <- err
	}()
	select {
	case <-handshakeStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-finished:
		t.Fatalf("request ended before the caller's time limit: %v", err)
	case <-time.After(11 * time.Second):
	}
	release.Do(func() { close(allowHandshake) })
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
