package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// heartbeatSocketServer serves a socket in a directory of its own, owned by
// the account that runs the test, and points the sender at it. It returns
// the directory and a channel with the token of each heartbeat that arrived.
func heartbeatSocketServer(t *testing.T, directoryMode os.FileMode) (string, chan string) {
	t.Helper()
	directory, e := os.MkdirTemp("", "mmwh")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	if e := os.Chmod(directory, directoryMode); e != nil {
		t.Fatal(e)
	}
	socket := filepath.Join(directory, "heartbeat.sock")
	l, e := net.Listen("unix", socket)
	if e != nil {
		t.Fatal(e)
	}
	received := make(chan string, 4)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Authorization")
		if r.URL.Path != "/v1/heartbeats/backup" || r.Header.Get("Authorization") != "Bearer a-long-heartbeat-test-token" {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(204)
	})}
	go server.Serve(l)
	t.Cleanup(func() { server.Close() })
	original, originalOwner := heartbeatSocketPath, heartbeatSocketOwnerID
	heartbeatSocketPath, heartbeatSocketOwnerID = socket, uint32(os.Getuid())
	t.Cleanup(func() { heartbeatSocketPath, heartbeatSocketOwnerID = original, originalOwner })
	return directory, received
}

func heartbeatTokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if e := os.WriteFile(path, []byte(token), 0600); e != nil {
		t.Fatal(e)
	}
	return path
}

func TestHeartbeatThroughTheSocketNeedsNoReceiverUrl(t *testing.T) {
	heartbeatSocketServer(t, 0755)
	token := heartbeatTokenFile(t, "a-long-heartbeat-test-token")
	output, e := capture(t, func() error { return run([]string{"heartbeat", "backup", "--bearer-token-file", token}) })
	if e != nil || output != "Heartbeat accepted: backup\n" {
		t.Fatal(output, e)
	}
}

func TestHeartbeatThroughTheSocketSaysWhatARefusalCanMean(t *testing.T) {
	heartbeatSocketServer(t, 0755)
	token := heartbeatTokenFile(t, "another-long-heartbeat-token")
	_, e := capture(t, func() error { return run([]string{"heartbeat", "backup", "--bearer-token-file", token}) })
	want := "MMWatchdog refused the heartbeat (HTTP status code 401): the token does not match, or this node has no heartbeat check group named backup"
	if e == nil || e.Error() != want {
		t.Fatal(e)
	}
}

func TestHeartbeatSenderSendsNoTokenToASocketThatRootDoesNotControl(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, directory string)
		mode  os.FileMode
		owner func() uint32
		why   string
	}{
		{name: "directory other accounts can write", mode: 0777, why: "can be written by accounts other than root"},
		{name: "directory the group can write", mode: 0775, why: "can be written by accounts other than root"},
		{name: "directory of another owner", mode: 0755, owner: func() uint32 { return uint32(os.Getuid()) + 1 }, why: "does not belong to root"},
		{name: "directory that is a symbolic link", mode: 0755, why: "symbolic link", setup: func(t *testing.T, directory string) {
			link := directory + "-link"
			if e := os.Symlink(directory, link); e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { os.Remove(link) })
			heartbeatSocketPath = filepath.Join(link, "heartbeat.sock")
		}},
		{name: "socket that is a symbolic link", mode: 0755, why: "not a socket", setup: func(t *testing.T, directory string) {
			link := filepath.Join(directory, "other.sock")
			if e := os.Symlink(heartbeatSocketPath, link); e != nil {
				t.Fatal(e)
			}
			heartbeatSocketPath = link
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			directory, received := heartbeatSocketServer(t, c.mode)
			if c.setup != nil {
				c.setup(t, directory)
			}
			if c.owner != nil {
				heartbeatSocketOwnerID = c.owner()
			}
			token := heartbeatTokenFile(t, "a-long-heartbeat-test-token")
			_, e := capture(t, func() error { return run([]string{"heartbeat", "backup", "--bearer-token-file", token}) })
			if e == nil || !strings.Contains(e.Error(), "the heartbeat was not sent") || !strings.Contains(e.Error(), c.why) {
				t.Fatal(e)
			}
			select {
			case got := <-received:
				t.Fatalf("the sender sent %q to a socket that root does not control", got)
			default:
			}
		})
	}
}

func TestHeartbeatSenderSaysWhenThereIsNoSocket(t *testing.T) {
	directory, _ := heartbeatSocketServer(t, 0755)
	e := checkHeartbeatSocketOwnership(filepath.Join(directory, "absent.sock"), uint32(os.Getuid()))
	if e == nil || !strings.Contains(e.Error(), "there is no heartbeat socket") || !strings.Contains(e.Error(), "MMWatchdog is not running on this node, or its configuration has no heartbeat check group") {
		t.Fatal(e)
	}
}

func TestHeartbeatThroughTheSocketDropsTheRawErrorWhenNothingExists(t *testing.T) {
	directory, _ := heartbeatSocketServer(t, 0755)
	absent := filepath.Join(directory, "no-such-directory", "heartbeat.sock")
	want := "there is no heartbeat socket " + absent + ": MMWatchdog is not running on this node, or its configuration has no heartbeat check group"
	if e := checkHeartbeatSocketOwnership(absent, uint32(os.Getuid())); e == nil || e.Error() != want {
		t.Fatal(e)
	}
	// Another error keeps its raw text.
	blocked := filepath.Join(directory, "blocked")
	if e := os.Mkdir(blocked, 0); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.Chmod(blocked, 0700) })
	if os.Getuid() != 0 {
		e := checkHeartbeatSocketOwnership(filepath.Join(blocked, "sub", "heartbeat.sock"), uint32(os.Getuid()))
		if e == nil || !strings.Contains(e.Error(), "permission denied") {
			t.Fatal(e)
		}
	}
}

func TestHeartbeatTokenFileTheAccountCannotReadGetsOneMessage(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read every file")
	}
	for _, c := range []string{"directory", "file"} {
		t.Run(c, func(t *testing.T) {
			heartbeatSocketServer(t, 0755)
			directory := t.TempDir()
			path := filepath.Join(directory, "token")
			if e := os.WriteFile(path, []byte("a-long-heartbeat-test-token"), 0600); e != nil {
				t.Fatal(e)
			}
			if c == "directory" {
				os.Chmod(directory, 0)
			} else {
				os.Chmod(path, 0)
			}
			t.Cleanup(func() { os.Chmod(directory, 0700) })
			reader, writer, _ := os.Pipe()
			original := os.Stderr
			os.Stderr = writer
			_, e := capture(t, func() error { return run([]string{"heartbeat", "backup", "--bearer-token-file", path}) })
			os.Stderr = original
			writer.Close()
			warning, _ := io.ReadAll(reader)
			want := "the account that runs this job cannot read " + path + ": give the job its own copy of the token that its account can read, for example: sudo install -o ACCOUNT -m 0600 SOURCE_TOKEN_FILE " + path
			if e == nil || e.Error() != want || len(warning) != 0 {
				t.Fatalf("error %v, standard error %q", e, warning)
			}
		})
	}
}

func TestHeartbeatCertificateAuthorityFileNeedsAReceiverUrl(t *testing.T) {
	token := heartbeatTokenFile(t, "a-long-heartbeat-test-token")
	_, e := capture(t, func() error {
		return run([]string{"heartbeat", "backup", "--bearer-token-file", token, "--trusted-certificate-authority-file", "/etc/ssl/ca.pem"})
	})
	if e == nil || !strings.Contains(e.Error(), "applies only with --receiver-url") {
		t.Fatal(e)
	}
}

func TestHeartbeatExitCodeSaysRefusedOrUnreachable(t *testing.T) {
	heartbeatSocketServer(t, 0755)
	wrong := heartbeatTokenFile(t, "another-long-heartbeat-token")
	_, e := capture(t, func() error { return run([]string{"heartbeat", "backup", "--bearer-token-file", wrong}) })
	if errorCode(e) != 1 {
		t.Errorf("a refused heartbeat exits 1, got %d: %v", errorCode(e), e)
	}
	// No such socket: MMWatchdog could not be reached.
	heartbeatSocketPath = filepath.Join(t.TempDir(), "absent.sock")
	_, e = capture(t, func() error { return run([]string{"heartbeat", "backup", "--bearer-token-file", wrong}) })
	if errorCode(e) != 4 {
		t.Errorf("an unreachable MMWatchdog exits 4, got %d: %v", errorCode(e), e)
	}
}
