package testhelpers

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"mmwatchdog/internal/primitives"
)

// Many tests give MMWatchdog stand-ins for a node's programs, such as systemctl or
// the mysql client, and modules whose module_executable is a script: shell
// scripts that answer as the real program would.
//
// Every such script is a symbolic link to one of two launcher scripts, one
// for /bin/sh and one for /bin/bash, which runs the script's own text from
// the hidden directory .stand-in-scripts beside the link. Two files for all
// stand-ins, and not a new executable file for each: macOS checks every new
// executable file at its first start, one file at a time, which takes up to
// half a second and seconds on a busy machine, and a link to a file that
// has started before starts at once.
//
// A test hands its stand-ins to MMWatchdog through the context (TestContext and
// PutProgramsFirst), not through the PATH of the whole test process, so
// that it can run beside other tests.

// standInLaunchers are the launcher scripts of this test process by the
// interpreter line of the scripts that they run, made by
// makeStandInLaunchers.
var standInLaunchers = map[string]string{}

// standInLauncherTexts are the texts of the launcher scripts. A launcher
// keeps $0 and the arguments, and the script that it reads runs as if it
// had been started itself.
var standInLauncherTexts = map[string]string{
	"#!/bin/sh\n":   "#!/bin/sh\n. \"${0%/*}/" + standInScriptsDirectory + "/${0##*/}\"\n",
	"#!/bin/bash\n": "#!/bin/bash\n. \"${0%/*}/" + standInScriptsDirectory + "/${0##*/}\"\n",
}

// standInScriptsDirectory is the directory, beside a stand-in's link, that
// holds the stand-in's own script.
const standInScriptsDirectory = ".stand-in-scripts"

// makeStandInLaunchers writes the launcher scripts into a new directory and
// returns it. RunTests calls it once.
func makeStandInLaunchers() (string, error) {
	directory, err := os.MkdirTemp("", "mmwatchdog-test-stand-in-launchers-")
	if err != nil {
		return "", err
	}
	// Readable by every account: MMWatchdog may start a stand-in as a check's
	// account.
	if err = os.Chmod(directory, 0755); err != nil {
		return directory, err
	}
	for interpreterLine, text := range standInLauncherTexts {
		launcher := filepath.Join(directory, strings.TrimPrefix(strings.TrimSpace(interpreterLine), "#!/bin/")+"-launcher")
		if err = os.WriteFile(launcher, []byte(text), 0555); err != nil {
			return directory, err
		}
		standInLaunchers[interpreterLine] = launcher
	}
	return directory, nil
}

// standInLaunchersUnchanged reports a test that wrote to a stand-in's path
// or changed its mode, and so changed the launcher of every stand-in.
func standInLaunchersUnchanged() error {
	for interpreterLine, launcher := range standInLaunchers {
		written, err := os.ReadFile(launcher)
		info, statErr := os.Stat(launcher)
		if err != nil || statErr != nil || string(written) != standInLauncherTexts[interpreterLine] || info.Mode().Perm() != 0555 {
			return fmt.Errorf("a test changed the launcher %s of the stand-in programs; write a stand-in with writeStandInProgram, and replace it with writeNotExecutableFile", launcher)
		}
	}
	return nil
}

// WriteStandInProgram makes path a stand-in program that runs script, a
// script that starts with "#!/bin/sh" or "#!/bin/bash". Writing it again
// replaces the script. Never write to path itself or change its mode: it is
// a link to the launcher of every stand-in (WriteNotExecutableFile).
func WriteStandInProgram(t *testing.T, path, script string) {
	t.Helper()
	interpreterLine, _, _ := strings.Cut(script, "\n")
	launcher := standInLaunchers[interpreterLine+"\n"]
	if launcher == "" {
		t.Fatalf("the stand-in %s must be a /bin/sh or /bin/bash script, and starts with %q", path, interpreterLine)
	}
	scripts := filepath.Join(filepath.Dir(path), standInScriptsDirectory)
	if err := os.MkdirAll(scripts, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, filepath.Base(path)), []byte(script), 0644); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(path); err == nil && target == launcher {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink(launcher, path); err != nil {
		t.Fatal(err)
	}
}

// WriteNotExecutableFile replaces what is at path, such as a stand-in's
// link, with a regular file of that text that no account may run.
func WriteNotExecutableFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
}

// programEnvironmentsOfTests holds, by the name of a test, the environment
// variables that SetProgramEnvironment set for the programs that MMWatchdog starts
// under the test's TestContext.
var programEnvironmentsOfTests = struct {
	sync.Mutex
	byTest map[string]map[string]string
}{byTest: map[string]map[string]string{}}

// SetProgramEnvironment is t.Setenv for only the programs that MMWatchdog starts
// under TestContext of this test and of its subtests. Unlike t.Setenv, it
// lets the test run beside others (t.Parallel).
func SetProgramEnvironment(t *testing.T, name, value string) {
	test := t.Name()
	programEnvironmentsOfTests.Lock()
	defer programEnvironmentsOfTests.Unlock()
	if programEnvironmentsOfTests.byTest[test] == nil {
		programEnvironmentsOfTests.byTest[test] = map[string]string{}
		t.Cleanup(func() {
			programEnvironmentsOfTests.Lock()
			defer programEnvironmentsOfTests.Unlock()
			delete(programEnvironmentsOfTests.byTest, test)
		})
	}
	programEnvironmentsOfTests.byTest[test][name] = value
}

// programEnvironmentOfTest is what SetProgramEnvironment set for the test
// and for the tests that it is a subtest of; a subtest's own values come
// first.
func programEnvironmentOfTest(test string) map[string]string {
	programEnvironmentsOfTests.Lock()
	defer programEnvironmentsOfTests.Unlock()
	variables := map[string]string{}
	for {
		for name, value := range programEnvironmentsOfTests.byTest[test] {
			if _, set := variables[name]; !set {
				variables[name] = value
			}
		}
		parent := strings.LastIndex(test, "/")
		if parent < 0 {
			return variables
		}
		test = test[:parent]
	}
}

// TestContext is the context under which the programs that MMWatchdog starts get
// what SetProgramEnvironment set for the test, as it is when each program
// starts (TestProgramEnvironmentKey in primitives.go).
func TestContext(t *testing.T) context.Context {
	test := t.Name()
	return context.WithValue(context.Background(), primitives.TestProgramEnvironmentKey{}, func() map[string]string {
		return programEnvironmentOfTest(test)
	})
}

// ProgramPath is the PATH on which MMWatchdog finds programs when it starts them
// under TestContext of the test.
func ProgramPath(t *testing.T) string {
	if path, set := programEnvironmentOfTest(t.Name())["PATH"]; set {
		return path
	}
	return os.Getenv("PATH")
}

// PutProgramsFirst makes MMWatchdog find programs first in directory, then where
// it found them before, when it starts them under TestContext of the test.
func PutProgramsFirst(t *testing.T, directory string) {
	SetProgramEnvironment(t, "PATH", directory+string(os.PathListSeparator)+ProgramPath(t))
}

// FakeUnit puts a systemctl on PATH that answers "show" for one unit with
// the properties in the file that it returns.
func FakeUnit(t *testing.T, properties string) string {
	t.Helper()
	_, shown, _ := FakeSystemctl(t)
	if err := os.WriteFile(shown, []byte(properties), 0644); err != nil {
		t.Fatal(err)
	}
	return shown
}

// FakeSystemctl puts a systemctl on PATH that answers list-units from the
// file it returns first and show from the second, and fails while the third
// exists.
func FakeSystemctl(t *testing.T) (listed, shown, broken string) {
	t.Helper()
	dir := t.TempDir()
	listed, shown, broken = filepath.Join(dir, "listed"), filepath.Join(dir, "shown"), filepath.Join(dir, "broken")
	script := "#!/bin/sh\nif [ -e " + ShellQuote(broken) + " ]; then echo 'Failed to connect to bus' >&2; exit 1; fi\nif [ \"$1\" = list-units ]; then cat " + ShellQuote(listed) + "; else cat " + ShellQuote(shown) + "; fi\n"
	WriteStandInProgram(t, filepath.Join(dir, "systemctl"), script)
	for _, path := range []string{listed, shown} {
		if err := os.WriteFile(path, nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	PutProgramsFirst(t, dir)
	return listed, shown, broken
}

// OwnOptions are the options of a program that MMWatchdog starts as the account
// that runs the test, in a directory of the test's own.
func OwnOptions(t *testing.T) primitives.ExecOptions {
	u, e := user.Current()
	if e != nil {
		t.Fatal(e)
	}
	return primitives.ExecOptions{RunAsUserAccount: u.Username, RunFromDirectory: t.TempDir()}
}

// StartedProgram returns the number of the process that a program of a
// test wrote, with a line break after it, into the file at path, once the
// program has written it.
func StartedProgram(t *testing.T, path string) int {
	t.Helper()
	process := 0
	WaitFor(t, "the program to write its process number into "+path, func() bool {
		written, e := os.ReadFile(path)
		if e != nil || !strings.HasSuffix(string(written), "\n") {
			return false
		}
		process, e = strconv.Atoi(strings.TrimSpace(string(written)))
		return e == nil
	})
	return process
}

// ShellQuote quotes s as one word of a shell command.
func ShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
