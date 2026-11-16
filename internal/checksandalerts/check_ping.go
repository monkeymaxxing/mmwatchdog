package checksandalerts

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"mmwatchdog/internal/primitives"
)

// Ping is the inputs of the ping check module: the name or the address of
// the machine that the check asks to answer, with the kind of request that
// the ping program sends. MMWatchdog sends the requests itself, so the node needs
// no ping program. The check is healthy when an answer arrives.
type Ping string

func (p *Ping) Validate(InputValidation) *FieldError {
	if host := string(*p); host == "" || len(host) > 253 || strings.ContainsAny(host, " \t\r\n/") || strings.Contains(host, "://") {
		return &FieldError{"", "must be the name or the address of a machine, such as db-1.internal.example.com or 10.0.0.5"}
	}
	return nil
}

func (p *Ping) Run(ctx context.Context, r *CheckRun) Observation {
	host := string(*p)
	addresses, err := primitives.ResolveHostName(ctx, host)
	if err != nil || len(addresses) == 0 {
		problem := "the name has no address"
		if err != nil {
			problem = err.Error()
		}
		return Result("failed", "MMWatchdog cannot find the address of "+host+": "+problem)
	}
	// A name and its address are both in the message, so a reader sees
	// which machine was asked.
	machine := host
	if addresses[0].String() != host {
		machine = host + " (" + addresses[0].String() + ")"
	}
	roundTrip, err := primitives.Ping(ctx, addresses[0])
	switch {
	case errors.Is(err, primitives.ErrPingNeedsRoot):
		return Result("unknown", err.Error())
	case errors.Is(err, primitives.ErrNoPingAnswer):
		return Result("failed", fmt.Sprintf("%s did not answer %d ping requests within %s", machine, primitives.PingRequests, timeLimitWords(r)))
	case err != nil:
		return Result("failed", "MMWatchdog cannot send a ping request to "+machine+": "+err.Error())
	}
	milliseconds := float64(roundTrip.Microseconds()) / 1000
	o := Result("ok", machine+" answered a ping request in "+primitives.FormatReading(milliseconds)+" ms")
	o.Measurements = map[string]Measurement{"round_trip_milliseconds": {milliseconds, "milliseconds"}}
	return withLimits(r.Check, o, nil)
}

// pingDescription is what "mmwatchdog describe-module ping" shows, and what
// validation knows about the module's inputs.
var pingDescription = ModuleDescription{
	Summary: "Asks another machine to answer, with the kind of request that the ping program sends. The check sends up to three requests and is healthy when one answer arrives.",
	UsedAs:  UsedAsCheck,
	Value:   &ModuleInput{Type: "string", Required: true, Description: "The name or the address of the machine to ask."},
	Measurements: []ModuleMeasurement{
		{Name: "round_trip_milliseconds", Unit: "milliseconds", Description: "The time between a request and its answer."},
	},
	Examples: []ModuleExample{{
		Description:   "The database machine answers.",
		Configuration: "ping: db-1.internal.example.com\nalert_severity: warn\n",
	}},
}
