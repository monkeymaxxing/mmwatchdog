//go:build !linux

package primitives

import "errors"

// ClockIsSynchronized works on Linux only.
func ClockIsSynchronized() (bool, error) {
	return false, errors.New("the check of the clock requires Linux")
}

// statFilesystem works on Linux only: MMWatchdog monitors Linux nodes, and its
// command line builds on other systems for validation and authoring.
func statFilesystem(path string) (FilesystemCounts, error) {
	return FilesystemCounts{}, errors.New("node filesystem checks require Linux")
}
