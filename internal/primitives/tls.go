package primitives

import (
	"crypto/tls"
	"crypto/x509"
)

// ServerTLSConfig builds the common server certificate policy. A nil pool
// uses the system authorities. Callers set the server name and protocol
// version appropriate to their transport.
func ServerTLSConfig(trustedCertificateAuthorities *x509.CertPool, dangerouslySkipServerCertificateCheck bool) *tls.Config {
	return &tls.Config{RootCAs: trustedCertificateAuthorities, InsecureSkipVerify: dangerouslySkipServerCertificateCheck}
}
