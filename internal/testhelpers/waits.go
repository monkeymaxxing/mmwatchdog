package testhelpers

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// LongestWaitForAnEvent is how long a test waits for something that it
// expects to happen before it fails: far above the normal time, which is
// milliseconds to a few seconds, also on a busy machine where everything
// takes many times longer. A wait returns as soon as the event happens, so
// the long deadline costs nothing while MMWatchdog works.
const LongestWaitForAnEvent = time.Minute

// WaitFor waits until done reports true, and fails the test after
// LongestWaitForAnEvent. what names the event in the failure.
func WaitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	for end := time.Now().Add(LongestWaitForAnEvent); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if done() {
			return
		}
	}
	t.Fatal("timed out waiting for", what)
}

// TimeLimitThatATestLetsPass is the time limit of a run that waits for a
// server of the test that sends nothing more, so that the limit ends the
// run. It is far longer than the test's own part of the run, a connection
// to this machine and a few bytes, also on a busy machine, so that the run
// has got to the point that the test studies when the limit passes.
const TimeLimitThatATestLetsPass = 2 * time.Second

// AGoroutineWaitsInASelect reports whether a goroutine of the test process
// waits in a select statement of the function named function, as the stack
// traces of the goroutines name it, such as "(*Daemon).execute".
func AGoroutineWaitsInASelect(function string) bool {
	stacks := make([]byte, 1<<20)
	stacks = stacks[:runtime.Stack(stacks, true)]
	for _, goroutine := range strings.Split(string(stacks), "\n\n") {
		lines := strings.Split(goroutine, "\n")
		if len(lines) > 1 && strings.Contains(lines[0], " [select") && strings.Contains(lines[1], function+"(") {
			return true
		}
	}
	return false
}

// TimeLimitThatTheTestEnds is the time limit of a run that passes when the
// test ends it, and not before. A test of what MMWatchdog does at the time limit
// first lets the run get to the point that the test studies, such as a
// program that the run started, however long that takes on a busy machine,
// and then ends the limit. The error of the context is then
// context.DeadlineExceeded, as for a time limit that passed. The context
// carries the values of its parent, such as the stand-in programs of
// TestContext.
type TimeLimitThatTheTestEnds struct {
	context.Context
	passed     chan struct{}
	passedOnce sync.Once
	// deadline is the end that the time limit states, or zero for none.
	deadline time.Time
}

// NewTimeLimitThatTheTestEnds returns a time limit that passes when the
// test calls its End.
func NewTimeLimitThatTheTestEnds(parent context.Context) *TimeLimitThatTheTestEnds {
	return &TimeLimitThatTheTestEnds{Context: parent, passed: make(chan struct{})}
}

// NewTimeLimitThatTheTestEndsWithADeadline returns a time limit that states
// deadline as its end, as the time limit of a run does, and that still
// passes only when the test calls its End. A module learns from the
// deadline how much time its run has, and a test sees what the module does
// with that time without a race against the end of the run.
func NewTimeLimitThatTheTestEndsWithADeadline(parent context.Context, deadline time.Time) *TimeLimitThatTheTestEnds {
	return &TimeLimitThatTheTestEnds{Context: parent, passed: make(chan struct{}), deadline: deadline}
}

func (l *TimeLimitThatTheTestEnds) Deadline() (time.Time, bool) {
	if l.deadline.IsZero() {
		return l.Context.Deadline()
	}
	return l.deadline, true
}

func (l *TimeLimitThatTheTestEnds) Done() <-chan struct{} { return l.passed }

func (l *TimeLimitThatTheTestEnds) Err() error {
	select {
	case <-l.passed:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// End lets the time limit pass.
func (l *TimeLimitThatTheTestEnds) End() { l.passedOnce.Do(func() { close(l.passed) }) }
