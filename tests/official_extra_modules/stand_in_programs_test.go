package official_extra_modules

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A stand-in for a program that a module asks, such as zpool or mongosh,
// is a symbolic link to one launcher script, which runs the stand-in's own
// /bin/sh script from the hidden directory .stand-in-scripts beside the
// link. One file for all stand-ins, and not a new executable file for each:
// macOS checks every new executable file at its first start, one file at a
// time, which takes up to half a second and seconds on a busy machine, and
// a link to a file that has started before starts at once.

// standInLauncher is the launcher script of this test process, made by
// makeStandInLauncher.
var standInLauncher string

// standInLauncherText keeps $0 and the arguments: the script that the
// launcher reads runs as if it had been started itself.
const standInLauncherText = "#!/bin/sh\n. \"${0%/*}/" + standInScriptsDirectory + "/${0##*/}\"\n"

// standInScriptsDirectory is the directory, beside a stand-in's link, that
// holds the stand-in's own script.
const standInScriptsDirectory = ".stand-in-scripts"

// makeStandInLauncher writes the launcher script into a new directory and
// returns it. TestMain calls it once.
func makeStandInLauncher() (string, error) {
	directory, err := os.MkdirTemp("", "mmwatchdog-module-tests-stand-in-launcher-")
	if err != nil {
		return "", err
	}
	if err = os.Chmod(directory, 0755); err != nil {
		return directory, err
	}
	standInLauncher = filepath.Join(directory, "sh-launcher")
	return directory, os.WriteFile(standInLauncher, []byte(standInLauncherText), 0555)
}

// standInLauncherUnchanged reports a test that wrote to a stand-in's path
// or changed its mode, and so changed the launcher of every stand-in.
func standInLauncherUnchanged() error {
	written, err := os.ReadFile(standInLauncher)
	info, statErr := os.Stat(standInLauncher)
	if err != nil || statErr != nil || string(written) != standInLauncherText || info.Mode().Perm() != 0555 {
		return fmt.Errorf("a test changed the launcher %s of the stand-in programs; write a stand-in with writeStandInProgram", standInLauncher)
	}
	return nil
}

// writeStandInProgram makes path a stand-in program that runs script, a
// script that starts with "#!/bin/sh". Never write to path itself or change
// its mode: it is a link to the launcher of every stand-in.
func writeStandInProgram(t *testing.T, path, script string) {
	t.Helper()
	if !strings.HasPrefix(script, "#!/bin/sh\n") {
		t.Fatalf("the stand-in %s must be a /bin/sh script", path)
	}
	scripts := filepath.Join(filepath.Dir(path), standInScriptsDirectory)
	if err := os.MkdirAll(scripts, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, filepath.Base(path)), []byte(script), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink(standInLauncher, path); err != nil {
		t.Fatal(err)
	}
}
