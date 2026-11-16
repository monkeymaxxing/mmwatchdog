package daemon

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// HeartbeatSocketName is the file name of the heartbeat socket. The socket
// sits beside the inspection socket, so by default at
// /run/mmwatchdog/heartbeat.sock. A job on the same node reports through it
// without a certificate. Only root can listen there: systemd creates
// /run/mmwatchdog for the unit and removes it when the unit stops, and no
// other account can create a directory in /run or put a socket into a
// directory that root owns. A job therefore never sends its token to an
// impostor, also while MMWatchdog is stopped. The design is in
// docs/changing-mmwatchdog/design/operation.md.
const HeartbeatSocketName = "heartbeat.sock"

// heartbeatSocket keeps the heartbeat socket in step with the configuration:
// it exists while the configuration has at least one heartbeat check group.
// It does nothing until Run (or a test) calls manage, so that code which
// loads and reloads configurations without serving does not open sockets.
type heartbeatSocket struct {
	mu     sync.Mutex
	manage bool
	server *http.Server
}

// listenOnUnixSocket removes a leftover socket file of a stopped process and
// listens on path with the permission bits mode. It refuses a path that is
// not a socket and a socket that another process still answers on.
func listenOnUnixSocket(path string, mode os.FileMode) (net.Listener, error) {
	if info, e := os.Lstat(path); e == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("socket path is not a socket")
		}
		if conn, e := net.DialTimeout("unix", path, 100*time.Millisecond); e == nil {
			conn.Close()
			return nil, fmt.Errorf("socket is already in use")
		}
		if e := os.Remove(path); e != nil {
			return nil, e
		}
	}
	listener, e := net.Listen("unix", path)
	if e != nil {
		return nil, e
	}
	if e = os.Chmod(path, mode); e != nil {
		listener.Close()
		return nil, e
	}
	return listener, nil
}

// manageHeartbeatSocket starts the heartbeat socket's following of the
// configuration and brings the socket in step with it now.
func (d *Daemon) manageHeartbeatSocket() error {
	d.heartbeatSocket.mu.Lock()
	d.heartbeatSocket.manage = true
	d.heartbeatSocket.mu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.syncHeartbeatSocket(d.config.Socket, len(d.tokens) > 0)
}

// syncHeartbeatSocket opens the heartbeat socket beside socket when wanted
// and closes it when not. The caller passes the configuration's values and
// need not hold d.mu.
func (d *Daemon) syncHeartbeatSocket(socket string, wanted bool) error {
	h := &d.heartbeatSocket
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.manage {
		return nil
	}
	if !wanted {
		h.closeLocked()
		return nil
	}
	if h.server != nil {
		return nil
	}
	l, e := listenOnUnixSocket(filepath.Join(filepath.Dir(socket), HeartbeatSocketName), 0666)
	if e != nil {
		return fmt.Errorf("heartbeat socket: %w", e)
	}
	// The same handler as the HTTPS receiver: the bearer token of the check
	// group is the only credential on both routes.
	h.server = &http.Server{Handler: http.HandlerFunc(d.heartbeat), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, MaxHeaderBytes: 8192}
	go func(s *http.Server) { _ = s.Serve(l) }(h.server) // Serve ends with the error ErrServerClosed when the socket closes; nothing else reaches it.
	return nil
}

func (h *heartbeatSocket) closeLocked() {
	if h.server != nil {
		_ = h.server.Close() // Closing the server closes the listener, which removes the socket file.
		h.server = nil
	}
}

func (d *Daemon) closeHeartbeatSocket() {
	d.heartbeatSocket.mu.Lock()
	defer d.heartbeatSocket.mu.Unlock()
	d.heartbeatSocket.manage = false
	d.heartbeatSocket.closeLocked()
}
