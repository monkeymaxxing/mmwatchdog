//go:build !linux

package primitives

import (
	"errors"
	"os"
	"syscall"
)

// ClockIsSynchronized works on Linux only.
func ClockIsSynchronized() (bool, error) {
	return false, errors.New("the check of the clock requires Linux")
}

// statFilesystem works on Linux only: MMWatchdog monitors Linux nodes, and its
// command line builds on other systems for validation and authoring.
func statFilesystem(path string) (FilesystemCounts, error) {
	return FilesystemCounts{}, errors.New("node filesystem checks require Linux")
}

// openRegularFileThroughRootLinks refuses only a link at the path itself on
// systems other than Linux, where MMWatchdog runs for validation and tests.
func openRegularFileThroughRootLinks(path string) (*os.File, FileInformation, error) {
	f, about, e := openRegularFile(path, syscall.O_NOFOLLOW)
	if errors.Is(e, syscall.ELOOP) {
		return nil, FileInformation{Kind: "a symbolic link"}, LinkNotOwnedByRoot{path}
	}
	return f, about, e
}
