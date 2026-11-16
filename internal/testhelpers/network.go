package testhelpers

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// AddressWhereNothingListens returns an address of this machine,
// 127.0.0.1:port, on which no program listens for network "tcp" or "udp".
// For "tcp" it is a port that a socket of the test held and gave back. For
// "udp" a socket of the test holds the port until the test ends, so that no
// other program, such as a server of a test that runs beside it, can take
// the port in the meantime. That socket is connected to its own address
// and takes no datagram from another, so this machine answers a datagram
// to the port as for a port that nothing holds. A TCP socket cannot hold a
// port in this way: on macOS a connection to a port that a socket holds
// without listening waits, where one to a port that nothing holds is
// refused at once.
func AddressWhereNothingListens(t *testing.T, network string) string {
	t.Helper()
	kind := map[string]int{"tcp": syscall.SOCK_STREAM, "udp": syscall.SOCK_DGRAM}[network]
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	socket, err := syscall.Socket(syscall.AF_INET, kind, 0)
	if err != nil {
		t.Fatal(err)
	}
	if network == "udp" {
		syscall.CloseOnExec(socket)
		t.Cleanup(func() { syscall.Close(socket) })
	} else {
		defer syscall.Close(socket)
	}
	if err = syscall.Bind(socket, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	bound, err := syscall.Getsockname(socket)
	if err != nil {
		t.Fatal(err)
	}
	if network == "udp" {
		if err = syscall.Connect(socket, bound); err != nil {
			t.Fatal(err)
		}
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(bound.(*syscall.SockaddrInet4).Port))
}

// ListenAtLocalhost listens on one port at both addresses of localhost,
// 127.0.0.1 and ::1, until the test ends. A program that is given
// localhost:port tries one address of localhost and then the other. With
// the port taken at both, each leads to the test's server, and never to
// another program of this machine that happens to listen on the same port
// at the other address. A machine without IPv6 has 127.0.0.1 alone.
func ListenAtLocalhost(t *testing.T) []net.Listener {
	t.Helper()
	for range 100 {
		ipv4, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		_, port, _ := net.SplitHostPort(ipv4.Addr().String())
		ipv6, err := net.Listen("tcp", net.JoinHostPort("::1", port))
		switch {
		case err == nil:
			t.Cleanup(func() { ipv4.Close(); ipv6.Close() })
			return []net.Listener{ipv4, ipv6}
		case errors.Is(err, syscall.EADDRINUSE):
			// Another program listens on this port at ::1: take another.
			ipv4.Close()
		default:
			t.Cleanup(func() { ipv4.Close() })
			return []net.Listener{ipv4}
		}
	}
	t.Fatal("no port is free at both 127.0.0.1 and ::1")
	return nil
}

// TestCertificate writes a certificate for localhost and its private key
// into dir, and returns the paths of the two files.
func TestCertificate(t *testing.T, dir string) (string, string) {
	t.Helper()
	certificate, privateKey, e := LocalhostCertificatePEM()
	if e != nil {
		t.Fatal(e)
	}
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, certificate, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, privateKey, 0600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// LocalhostCertificatePEM makes a self-signed certificate for localhost,
// valid from an hour ago to an hour from now, and its private key, both in
// PEM.
func LocalhostCertificatePEM() (certificate, privateKey []byte, e error) {
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		return nil, nil, e
	}
	template := x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"localhost"}}
	der, e := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if e != nil {
		return nil, nil, e
	}
	kb, e := x509.MarshalPKCS8PrivateKey(key)
	if e != nil {
		return nil, nil, e
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), nil
}
