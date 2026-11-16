package testhelpers_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/testhelpers"
)

// The test clock's times carry a monotonic reading that the jumps of its
// wall clock do not move. This test fails when Go changes the layout of
// time.Time that the clock writes.
func TestClock(t *testing.T) {
	t.Parallel()
	// A new clock stands at the present: the wall reading that it writes
	// counts its seconds from the start that Go's layout uses.
	early := time.Now().Round(0)
	fresh := testhelpers.NewClock()
	late := time.Now().Round(0)
	if at := fresh.Now().Round(0); at.Before(early) || at.After(late) {
		t.Fatalf("a new clock stands at %s, not between %s and %s", at, early, late)
	}
	clock := testhelpers.NewClock()
	before := clock.Now()
	clock.Jump(-time.Hour)
	clock.Pass(time.Second)
	after := clock.Now()
	if elapsed := after.Sub(before); elapsed != time.Second {
		t.Fatal("the monotonic clock did not run on through the jump:", elapsed)
	}
	if wall := after.Round(0).Sub(before.Round(0)); wall != time.Second-time.Hour {
		t.Fatal("the wall clock did not jump back an hour:", wall)
	}
	if after == after.Round(0) || !strings.Contains(after.String(), " m=") {
		t.Fatal("the time carries no monotonic reading:", after.String())
	}
	encoded, _ := json.Marshal(after)
	want, _ := json.Marshal(before.Round(0).Add(time.Second - time.Hour))
	if string(encoded) != string(want) {
		t.Fatalf("a recorded time shows %s, not the wall clock's %s", encoded, want)
	}
	if after.Location() != before.Location() || after.Nanosecond() != before.Nanosecond() {
		t.Fatal("the jump changed the location or the nanoseconds:", before, after)
	}
}
