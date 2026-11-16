package main

import (
	"flag"
	"fmt"
	"time"

	"mmwatchdog/internal/checksandalerts"
)

// parseTimeOptions is shared by readings and event history commands.
// An empty default duration selects the latest events instead of a span.
func parseTimeOptions(flags *flag.FlagSet, last, since, until string, now time.Time) (time.Time, time.Time, error) {
	var start, end time.Time
	if last == "" && !visited(flags, "last") && !visited(flags, "since") && !visited(flags, "until") {
		return start, end, nil
	}
	if visited(flags, "last") && visited(flags, "since") {
		return time.Time{}, time.Time{}, fmt.Errorf("--last and --since cannot be used together; choose one time span")
	}
	if visited(flags, "until") && !visited(flags, "since") {
		return time.Time{}, time.Time{}, fmt.Errorf("--until needs --since; state the start of the time span")
	}
	end = now.UTC()
	if visited(flags, "since") {
		var err error
		start, err = time.Parse(time.RFC3339, since)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("--since needs an RFC 3339 time, such as 2026-10-07T14:00:00Z")
		}
		if visited(flags, "until") {
			end, err = time.Parse(time.RFC3339, until)
			if err != nil {
				return time.Time{}, time.Time{}, fmt.Errorf("--until needs an RFC 3339 time, such as 2026-10-07T15:00:00Z")
			}
		}
	} else {
		duration, err := checksandalerts.ParseDuration(last)
		if err != nil || duration <= 0 {
			return time.Time{}, time.Time{}, fmt.Errorf("--last needs a positive duration, such as 30s, 5m, 26h, or 7d")
		}
		start = end.Add(-duration)
	}
	start, end = start.UTC(), end.UTC()
	if end.Before(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("--until is before --since; choose an end at or after the start")
	}
	return start, end, nil
}
