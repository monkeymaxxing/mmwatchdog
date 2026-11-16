package primitives_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"

	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/testhelpers"
)

func TestMySQLTLSLoginGuard(t *testing.T) {
	t.Parallel()
	certFile, keyFile := testhelpers.TestCertificate(t, t.TempDir())
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, encrypted := range []bool{false, true} {
		t.Run(map[bool]string{true: "encrypted login", false: "unencrypted login refused"}[encrypted], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			received := make(chan string, 1)
			go func() {
				server, err := listener.Accept()
				if err != nil {
					received <- err.Error()
					return
				}
				defer server.Close()
				stop := context.AfterFunc(ctx, func() { server.Close() })
				defer stop()
				server.Write([]byte{1, 0, 0, 0, 10}) // one greeting packet
				var packet [36]byte
				n, err := io.ReadFull(server, packet[:])
				if !encrypted {
					if n != 0 {
						received <- "unencrypted login reached the server"
					} else {
						received <- "refused"
					}
					return
				}
				if err != nil {
					received <- err.Error()
					return
				}
				secure := tls.Server(server, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
				var login [5]byte
				if _, err := io.ReadFull(secure, login[:]); err != nil {
					received <- err.Error()
					return
				}
				received <- string(login[:])
			}()
			address, stop, err := primitives.MySQLTLSLoginGuard(ctx, listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			client, err := net.Dial("tcp", address)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			stopClient := context.AfterFunc(ctx, func() { client.Close() })
			defer stopClient()
			var greeting [5]byte
			if _, err := io.ReadFull(client, greeting[:]); err != nil {
				t.Fatal(err)
			}
			expected := "refused"
			if encrypted {
				packet := make([]byte, 36)
				packet[0], packet[3], packet[5] = 32, 1, 8 // CLIENT_SSL
				client.Write(packet)
				secure := tls.Client(client, primitives.ServerTLSConfig(nil, true))
				if _, err := io.WriteString(secure, "login"); err != nil {
					t.Fatal(err)
				}
				expected = "login"
			} else {
				// A full handshake response includes credentials; it is not an SSLRequest.
				client.Write(append([]byte{40, 0, 0, 1}, make([]byte, 40)...))
			}
			select {
			case actual := <-received:
				if actual != expected {
					t.Fatal(actual)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}
