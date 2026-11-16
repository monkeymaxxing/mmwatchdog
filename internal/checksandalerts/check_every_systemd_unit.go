package checksandalerts

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"mmwatchdog/internal/primitives"
)

// EverySystemdUnit is the inputs of the every_systemd_unit check
// module, which watches every systemd unit on the node at once: the check
// is healthy while systemd lists no failed unit. Each unit that systemd
// reports as failed is one finding of the check, and so its own incident;
// see CheckState.observeFindings.
// SystemdUnitsToIgnore is required, so a reader sees whether any unit is
// left out; an empty list leaves none out.
type EverySystemdUnit struct {
	FailWhen             string   `yaml:"fail_when" json:"fail_when"`
	SystemdUnitsToIgnore []string `yaml:"systemd_units_to_ignore" json:"systemd_units_to_ignore"`
}

func (n *EverySystemdUnit) Validate(v InputValidation) *FieldError {
	// A reader must see whether any unit is left out, so the list is
	// written even when it is empty.
	if !v.Authored("systemd_units_to_ignore") || n.SystemdUnitsToIgnore == nil {
		return &FieldError{"systemd_units_to_ignore", "is required: the systemd units whose failure this check leaves out, such as [myapp.service], or [] to leave none out"}
	}
	inputs := map[string]any{}
	if n.FailWhen != "" || v.Authored != nil && v.Authored("fail_when") {
		inputs["fail_when"] = n.FailWhen
	}
	ignored := make([]any, len(n.SystemdUnitsToIgnore))
	for index, unit := range n.SystemdUnitsToIgnore {
		ignored[index] = unit
	}
	inputs["systemd_units_to_ignore"] = ignored
	if problem := everySystemdUnitDescription.InputsProblem("every_systemd_unit", inputs); problem != nil {
		return problem
	}
	seen := map[string]bool{}
	for _, unit := range n.SystemdUnitsToIgnore {
		if !primitives.AnyUnitPattern.MatchString(unit) || seen[unit] {
			return &FieldError{"systemd_units_to_ignore", "expected distinct systemd unit names with their kind, such as [myapp.service, backup.timer]"}
		}
		seen[unit] = true
	}
	return nil
}

// Identity leaves the order in which the ignored units are written out of
// the check's identity.
func (n *EverySystemdUnit) Identity() CheckInputs {
	ignored := slices.Clone(n.SystemdUnitsToIgnore)
	slices.Sort(ignored)
	return &EverySystemdUnit{FailWhen: n.FailWhen, SystemdUnitsToIgnore: ignored}
}

// unitNamesShown is how many unit names an every_systemd_unit message
// lists before it counts the rest.
const unitNamesShown = 20

// Run asks systemd for every unit it reports as failed, of every kind, and
// leaves out the units the check ignores. The check is healthy while none
// is left. The observation lists the failed units as its findings, and
// CheckState.observeFindings gives each of them an incident of its own.
// When systemd cannot be asked, the observation is unknown.
func (n *EverySystemdUnit) Run(ctx context.Context, _ *CheckRun) Observation {
	listed, err := primitives.Systemctl(ctx, "list-units", "--state=failed", "--all", "--plain", "--full", "--no-legend", "--no-pager")
	if err != nil {
		return Result("unknown", "systemd inspection: MMWatchdog could not ask systemd for its failed units: "+err.Error())
	}
	units := []string{}
	for _, line := range strings.Split(listed, "\n") {
		// The first column is the unit; a systemd that marks the line with a
		// dot writes the unit second.
		fields := strings.Fields(line)
		if len(fields) > 1 && !primitives.AnyUnitPattern.MatchString(fields[0]) {
			fields = fields[1:]
		}
		if len(fields) > 0 && primitives.AnyUnitPattern.MatchString(fields[0]) && !slices.Contains(units, fields[0]) {
			units = append(units, fields[0])
		}
	}
	slices.Sort(units)
	failed, ignored := []Finding{}, []string{}
	results := failedUnitResults(ctx, units)
	for _, unit := range units {
		state, known := results[unit]
		switch {
		case slices.Contains(n.SystemdUnitsToIgnore, unit):
			ignored = append(ignored, unit)
		case known && state.activeState != "" && state.activeState != "failed":
			// The unit left the failed state between the two questions.
		default:
			failed = append(failed, Finding{ID: unit, Message: primitives.FailedUnitMessage(unit, state.result)})
		}
	}
	o := Result("ok", "no failed systemd units")
	if len(failed) > 0 {
		names := []string{}
		for _, f := range failed {
			names = append(names, f.ID)
		}
		noun := "systemd units"
		if len(failed) == 1 {
			noun = "systemd unit"
		}
		o = Result("failed", fmt.Sprintf("%d %s failed: %s", len(failed), noun, shownNames(names)))
		o.Findings = failed
	}
	if len(ignored) > 0 {
		o.Message += fmt.Sprintf(" (not counting %d in systemd_units_to_ignore: %s)", len(ignored), shownNames(ignored))
	}
	return o
}

// shownNames lists up to unitNamesShown names and counts the rest.
func shownNames(names []string) string {
	if more := len(names) - unitNamesShown; more > 0 {
		return strings.Join(names[:unitNamesShown], ", ") + fmt.Sprintf(", and %d more", more)
	}
	return strings.Join(names, ", ")
}

// failedUnitState is what systemd says about one unit that it listed as
// failed: its ActiveState and its Result.
type failedUnitState struct{ activeState, result string }

// failedUnitResults reads systemd's ActiveState and Result of each unit, in
// one command. A unit systemd gives no answer for is left out, so its alert
// names the unit without a result.
func failedUnitResults(ctx context.Context, units []string) map[string]failedUnitState {
	states := map[string]failedUnitState{}
	if len(units) == 0 {
		return states
	}
	shown, err := primitives.Systemctl(ctx, append([]string{"show", "--property=Id,ActiveState,Result", "--"}, units...)...)
	if err != nil {
		return states
	}
	// systemd writes one block of properties for each unit, with an empty
	// line between two blocks.
	for _, block := range strings.Split(shown, "\n\n") {
		fields := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			if key, value, ok := strings.Cut(line, "="); ok {
				fields[key] = value
			}
		}
		if id := fields["Id"]; id != "" {
			states[id] = failedUnitState{fields["ActiveState"], fields["Result"]}
		}
	}
	return states
}

// SystemdUnitMissingFromUnitsToIgnore is a systemd unit that has its own
// systemd_unit_state check on the node and is missing from the
// systemd_units_to_ignore of an every_systemd_unit check, for mmwatchdog
// validate: one failure of that unit would send two alerts.
type SystemdUnitMissingFromUnitsToIgnore struct {
	// CheckGroup and Check name the every_systemd_unit check.
	CheckGroup  string `json:"check_group"`
	Check       string `json:"check"`
	SystemdUnit string `json:"systemd_unit"`
	// SystemdUnitStateCheck is the unit's own check, as CHECK_GROUP/CHECK.
	SystemdUnitStateCheck string `json:"systemd_unit_state_check"`
}

// everySystemdUnitDescription is what "mmwatchdog describe-module every_systemd_unit" shows, and what
// validation knows about the module's inputs.
var everySystemdUnitDescription = ModuleDescription{
	Summary:                   "Watches every systemd unit of the node at once. Each unit that systemd reports as failed gets its own failure alert and its own recovery alert.",
	UsedAs:                    UsedAsCheck,
	OneIncidentForEachFinding: true,
	Inputs: []ModuleInput{
		{Name: "fail_when", Type: "string", Required: true, AllowedValues: []any{"a_systemd_unit_has_failed"}, Description: "The check fails while a systemd unit has failed; write a_systemd_unit_has_failed, the only value."},
		{Name: "systemd_units_to_ignore", Type: "list_of_strings", Required: true, Description: "The units whose failure the check leaves out, each with its kind, such as myapp.service or backup.timer. An empty list leaves none out."},
	},
	Examples: []ModuleExample{{
		Description:   "Alert for every failed unit except myapp.service, which has a check of its own.",
		Configuration: "every_systemd_unit:\n  fail_when: a_systemd_unit_has_failed\n  systemd_units_to_ignore: [myapp.service]\nalert_severity: warn\n",
	}},
}
