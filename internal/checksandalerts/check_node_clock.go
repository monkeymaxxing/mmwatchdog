package checksandalerts

import (
	"context"

	"mmwatchdog/internal/primitives"
)

// NodeClock is the inputs of the node_clock check module, which asks the
// kernel whether the machine's clock follows a time server.
type NodeClock struct {
	FailWhen string `yaml:"fail_when" json:"fail_when"`
}

func (n *NodeClock) Validate(v InputValidation) *FieldError {
	inputs := map[string]any{}
	if n.FailWhen != "" || v.Authored != nil && v.Authored("fail_when") {
		inputs["fail_when"] = n.FailWhen
	}
	return nodeClockDescription.InputsProblem("node_clock", inputs)
}

func (n *NodeClock) Run(context.Context, *CheckRun) Observation {
	synchronized, err := primitives.ClockIsSynchronized()
	switch {
	case err != nil:
		return Result("unknown", "MMWatchdog cannot ask the kernel about the clock: "+err.Error())
	case !synchronized:
		return Result("failed", "the kernel reports that the clock of this machine does not follow a time server")
	}
	return Result("ok", "the clock of this machine follows a time server")
}

// nodeClockDescription is what "mmwatchdog describe-module node_clock" shows, and what
// validation knows about the module's inputs.
var nodeClockDescription = ModuleDescription{
	Summary: "Asks the kernel whether the machine's clock follows a time server, whichever time service the machine runs.",
	UsedAs:  UsedAsCheck,
	Inputs: []ModuleInput{
		{Name: "fail_when", Type: "string", Required: true, AllowedValues: []any{"the_clock_is_not_synchronized"}, Description: "The check fails while the clock is not synchronized; write the_clock_is_not_synchronized, the only value."},
	},
	Examples: []ModuleExample{{
		Description:   "The clock of this machine follows a time server.",
		Configuration: "node_clock:\n  fail_when: the_clock_is_not_synchronized\nalert_severity: warn\n",
	}},
}
