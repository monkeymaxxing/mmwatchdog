package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// heartbeatSocketPath is where a job on this node finds the heartbeat
// socket. Tests point it at a socket of their own.
var heartbeatSocketPath = "/run/mmwatchdog/heartbeat.sock"

// heartbeatSocketOwnerID is the account that must own the heartbeat socket
// and its directory: root. Tests use their own account.
var heartbeatSocketOwnerID uint32

// heartbeatSocketRefusal is the sender's refusal to send the token to a
// socket that root does not own. Retrying cannot change it.
type heartbeatSocketRefusal struct{ message string }

func (r heartbeatSocketRefusal) Error() string { return r.message }

// checkHeartbeatSocketOwnership refuses to go on unless the socket and the
// directory that holds it belong to the account with the id ownerID (root on
// a node), are not symbolic links, and the directory cannot be written by
// any other account. Only then is the process behind the socket the real
// MMWatchdog: nobody else can put a socket into that directory.
func checkHeartbeatSocketOwnership(socket string, ownerID uint32) error {
	directory := filepath.Dir(socket)
	refuse := func(why string) error {
		return heartbeatSocketRefusal{fmt.Sprintf("the heartbeat was not sent, because the heartbeat socket %s is not safe for the token: %s. MMWatchdog runs as root, so root must own the socket and its directory; otherwise another account could have put its own socket there to collect the token", socket, why)}
	}
	missing := func(e error) error {
		message := "there is no heartbeat socket " + socket + ": MMWatchdog is not running on this node, or its configuration has no heartbeat check group"
		if errors.Is(e, fs.ErrNotExist) {
			return errors.New(message)
		}
		return fmt.Errorf("%s (%v)", message, e)
	}
	dirInfo, e := os.Lstat(directory)
	if e != nil {
		return missing(e)
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return refuse("its directory " + directory + " is a symbolic link or not a directory")
	}
	if stat, ok := dirInfo.Sys().(*syscall.Stat_t); !ok || stat.Uid != ownerID {
		return refuse("its directory " + directory + " does not belong to root")
	}
	if dirInfo.Mode().Perm()&0022 != 0 {
		return refuse("its directory " + directory + " can be written by accounts other than root")
	}
	info, e := os.Lstat(socket)
	if e != nil {
		return missing(e)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return refuse("it is not a socket")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != ownerID {
		return refuse("it does not belong to root")
	}
	return nil
}
