//go:build linux

package primitives

import (
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
