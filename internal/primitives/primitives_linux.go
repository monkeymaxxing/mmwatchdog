//go:build linux

package primitives

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// clockIsNotSynchronizedAtMicroseconds is the largest error of the clock, in
// microseconds, at which the kernel no longer counts the clock as following
// a time server.
const clockIsNotSynchronizedAtMicroseconds = 16_000_000

// ClockIsSynchronized asks the kernel whether the machine's clock follows a
// time server, whichever time service the machine runs. The answer is the
// one that "timedatectl" prints as "System clock synchronized": the kernel
// keeps the largest error that the clock can have, a time service lowers
// that number each time it sets the clock, and without one the number grows
// to 16 seconds and stays there.
func ClockIsSynchronized() (bool, error) {
	var state unix.Timex
	if _, err := unix.Adjtimex(&state); err != nil {
		return false, err
	}
	return state.Maxerror < clockIsNotSynchronizedAtMicroseconds, nil
}

// statFilesystem asks the kernel for the space and the inodes of the
// filesystem that holds path. It returns when the kernel answers, which a
// filesystem on another machine can hold up without end:
// FilesystemStatistics waits for it no longer than its caller allows.
func statFilesystem(path string) (FilesystemCounts, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return FilesystemCounts{}, err
	}
	// The kernel counts blocks in units of the fragment size.
	blockBytes := uint64(stat.Frsize)
	if blockBytes == 0 {
		blockBytes = uint64(stat.Bsize)
	}
	return FilesystemCounts{Blocks: stat.Blocks, BlocksAvailable: stat.Bavail, BlockBytes: blockBytes, Inodes: stat.Files, InodesFree: stat.Ffree, ReadOnly: stat.Flags&unix.ST_RDONLY != 0}, nil
}

// openRegularFileThroughRootLinks opens path one part at a time, each part
// without following a link. A link that root owns is followed, and any other
// link is LinkNotOwnedByRoot. Each part is opened with O_PATH before its
// owner is read from the open file, so another account cannot swap a part
// between the check and the use.
func openRegularFileThroughRootLinks(path string) (*os.File, FileInformation, error) {
	if !filepath.IsAbs(path) {
		return nil, FileInformation{}, &os.PathError{Op: "open", Path: path, Err: syscall.EINVAL}
	}
	fail := func(e error) (*os.File, FileInformation, error) {
		return nil, FileInformation{}, &os.PathError{Op: "open", Path: path, Err: e}
	}
	directory, e := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return fail(e)
	}
	defer func() { _ = unix.Close(directory) }() // An O_PATH handle has nothing to commit.
	parts, reached, links := strings.Split(path, "/"), "", 0
	for len(parts) > 0 {
		name := parts[0]
		parts = parts[1:]
		if name == "" || name == "." {
			continue
		}
		part, e := unix.Openat(directory, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e != nil {
			return fail(e)
		}
		var about unix.Stat_t
		if e := unix.Fstat(part, &about); e != nil {
			_ = unix.Close(part) // An O_PATH handle has nothing to commit.
			return fail(e)
		}
		reached = filepath.Join(reached, "/", name)
		if about.Mode&unix.S_IFMT == unix.S_IFLNK {
			if about.Uid != 0 {
				_ = unix.Close(part) // An O_PATH handle has nothing to commit.
				return nil, FileInformation{Kind: "a symbolic link"}, LinkNotOwnedByRoot{reached}
			}
			if links++; links > 40 {
				_ = unix.Close(part) // An O_PATH handle has nothing to commit.
				return fail(syscall.ELOOP)
			}
			target := make([]byte, unix.PathMax)
			n, e := unix.Readlinkat(part, "", target)
			_ = unix.Close(part) // An O_PATH handle has nothing to commit.
			if e != nil {
				return fail(e)
			}
			if strings.HasPrefix(string(target[:n]), "/") {
				root, e := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
				if e != nil {
					return fail(e)
				}
				_ = unix.Close(directory) // An O_PATH handle has nothing to commit.
				directory, reached = root, ""
			} else {
				reached = filepath.Dir(reached)
			}
			parts = append(strings.Split(string(target[:n]), "/"), parts...)
			continue
		}
		if len(parts) == 0 {
			_ = unix.Close(part) // An O_PATH handle has nothing to commit.
			opened, e := unix.Openat(directory, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if errors.Is(e, syscall.ELOOP) {
				return nil, FileInformation{Kind: "a symbolic link"}, LinkNotOwnedByRoot{reached}
			}
			if e != nil {
				return fail(e)
			}
			return regularFileFrom(os.NewFile(uintptr(opened), path))
		}
		_ = unix.Close(directory) // An O_PATH handle has nothing to commit.
		directory = part
	}
	return fail(syscall.EISDIR)
}
