// Package testhelpers holds what the tests of several packages share: the
// start of a test program (RunTests), configuration files, modules and
// stand-in programs, a fake MMHotline, and waits. Only tests import it.
package testhelpers

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"testing"

	"mmwatchdog/internal/checksandalerts"
)

// RunTests runs the tests of a package from its TestMain and ends the test
// program. Every package whose tests run a module or a stand-in program
// starts its tests with it.
//
// MMWatchdog runs a Starlark script through "SELF run-starlark-module
// MODULE_DIR", where SELF is the program that runs. In a test that is
// the test program, so RunTests runs a script when the test program is
// started in that way, as the mmwatchdog program does. It ends with syscall.Exit,
// not os.Exit: in a test program built with -race, os.Exit first waits a
// second (GORACE's atexit_sleep_ms), and tests run scripts hundreds of
// times.
//
// Otherwise RunTests makes the launchers of the stand-in programs, calls
// prepare unless it is nil, runs the tests, and fails them when a test
// changed a launcher.
func RunTests(m *testing.M, prepare func() error) {
	if len(os.Args) > 1 && os.Args[1] == "run-starlark-module" {
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "mmwatchdog: run-starlark-module requires the directory of a module")
			syscall.Exit(1)
		}
		if e := checksandalerts.RunStarlarkModule(context.Background(), os.Args[2], os.Stdin, os.Stdout); e != nil {
			fmt.Fprintln(os.Stderr, "mmwatchdog:", e)
			syscall.Exit(1)
		}
		syscall.Exit(0)
	}
	launchers, e := makeStandInLaunchers()
	if e == nil && prepare != nil {
		e = prepare()
	}
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.RemoveAll(launchers)
		os.Exit(1)
	}
	code := m.Run()
	if e = standInLaunchersUnchanged(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		code = 1
	}
	os.RemoveAll(launchers)
	os.Exit(code)
}
