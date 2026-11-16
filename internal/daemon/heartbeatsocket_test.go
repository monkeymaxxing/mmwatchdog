package daemon

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
)

// heartbeatSocketConfig configures the check group "backup", which takes
// heartbeats, without a heartbeat receiver: the node needs no certificate
// for jobs on the same node.
func heartbeatSocketConfig(t *testing.T) (path string, c *config.Config, socketDirectory string) {
	t.Helper()
	path, c = daemonConfig(t)
	// Socket paths are short: a Unix socket path has a length limit.
	socketDirectory, e := os.MkdirTemp("", "mmwd")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(socketDirectory) })
	c.Socket = filepath.Join(socketDirectory, "mmwatchdog.sock")
	tokenPath := filepath.Join(filepath.Dir(path), "token")
	if e := os.WriteFile(tokenPath, []byte("test-heartbeat-token"), 0600); e != nil {
		t.Fatal(e)
	}
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"backup": {Heartbeat: &checksandalerts.Heartbeat{MaxTimeBetweenHeartbeats: "26h", ExpectedBearerTokenFile: tokenPath}}}
	writeConfig(t, path, c)
	return path, c, socketDirectory
}

func postThroughSocket(t *testing.T, socket, name, token string) int {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	r, e := http.NewRequest("POST", "http://mmwatchdog-heartbeat-socket/v1/heartbeats/"+name, nil)
	if e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	resp, e := client.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestHeartbeatSocketTakesHeartbeatsWithTheCheckGroupsToken(t *testing.T) {
	path, _, directory := heartbeatSocketConfig(t)
	d, e := NewDaemon(path)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	socket := filepath.Join(directory, HeartbeatSocketName)
	if _, e := os.Lstat(socket); e == nil {
		t.Fatal("the heartbeat socket exists before the daemon serves")
	}
	if e := d.manageHeartbeatSocket(); e != nil {
		t.Fatal(e)
	}
	info, e := os.Lstat(socket)
	if e != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0666 {
		t.Fatalf("the heartbeat socket must be a socket that every account can connect to: %v %v", info, e)
	}
	for _, refused := range []struct{ name, token string }{{"backup", "wrong"}, {"backup", ""}, {"unknown", "test-heartbeat-token"}} {
		if code := postThroughSocket(t, socket, refused.name, refused.token); code != 401 {
			t.Fatalf("%+v: status %d, want 401", refused, code)
		}
	}
	if !d.snapshot.Checks["backup/heartbeat"].Receipt.IsZero() {
		t.Fatal("a refused heartbeat was saved")
	}
	if code := postThroughSocket(t, socket, "backup", "test-heartbeat-token"); code != 204 {
		t.Fatalf("status %d, want 204", code)
	}
	if d.snapshot.Checks["backup/heartbeat"].Receipt.IsZero() {
		t.Fatal("the heartbeat was not saved")
	}
	d.closeHeartbeatSocket()
	if _, e := os.Lstat(socket); e == nil {
		t.Fatal("the heartbeat socket stays after the daemon stops serving")
	}
}

func TestHeartbeatSocketFollowsTheHeartbeatCheckGroupsOnReload(t *testing.T) {
	path, c, directory := heartbeatSocketConfig(t)
	withGroups := c.CheckGroups
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true"}}}}
	writeConfig(t, path, c)
	d, e := NewDaemon(path)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	socket := filepath.Join(directory, HeartbeatSocketName)
	exists := func() bool { _, e := os.Lstat(socket); return e == nil }
	if e := d.manageHeartbeatSocket(); e != nil || exists() {
		t.Fatalf("a node without heartbeat check groups has a heartbeat socket: %v", e)
	}
	c.CheckGroups = withGroups
	writeConfig(t, path, c)
	if _, e := d.reload(); e != nil || !exists() {
		t.Fatalf("a reload that adds the first heartbeat check group opens no socket: %v", e)
	}
	if code := postThroughSocket(t, socket, "backup", "test-heartbeat-token"); code != 204 {
		t.Fatalf("status %d, want 204", code)
	}
	if _, e := d.reload(); e != nil || !exists() {
		t.Fatalf("a reload that changes nothing closed the socket: %v", e)
	}
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"app": {Check: checksandalerts.Check{Inputs: &checksandalerts.Shell{Script: "true"}}}}
	writeConfig(t, path, c)
	if _, e := d.reload(); e != nil || exists() {
		t.Fatalf("a reload that removes the last heartbeat check group leaves the socket: %v", e)
	}
}

func TestHeartbeatSocketRefusesToReplaceAnotherProcessesSocket(t *testing.T) {
	path, _, directory := heartbeatSocketConfig(t)
	l, e := net.Listen("unix", filepath.Join(directory, HeartbeatSocketName))
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	d, e := NewDaemon(path)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	if e := d.manageHeartbeatSocket(); e == nil {
		t.Fatal("the daemon took the socket of a process that still answers on it")
	}
}
