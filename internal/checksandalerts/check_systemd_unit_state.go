package checksandalerts

import (
	"context"
	"strings"

	"mmwatchdog/internal/primitives"
)

// SystemdUnitState is the inputs of the systemd_unit_state check module,
// which asks systemd for the state of one unit of any kind, such as a
// service, a timer, or a path unit: the check is healthy while the unit runs
// (UnitRuns), and fails when it is not active.
type SystemdUnitState struct {
	SystemdUnit string `yaml:"systemd_unit" json:"systemd_unit"`
	FailWhen    string `yaml:"fail_when" json:"fail_when"`
}

func (u *SystemdUnitState) Validate(v InputValidation) *FieldError {
	// Every kind of unit has an ActiveState, so the check takes any. A path
	// or timer unit that stopped silently stops what it starts.
	if u.SystemdUnit == "" {
		return &FieldError{"systemd_unit", "is required: the systemd unit this check is about, such as myapp.service or backup.timer"}
	}
	if !primitives.AnyUnitPattern.MatchString(u.SystemdUnit) {
		return &FieldError{"systemd_unit", "must be the name of a systemd unit with its kind, such as myapp.service, backup.timer, or myapp.path"}
	}
	inputs := map[string]any{"systemd_unit": u.SystemdUnit}
	if u.FailWhen != "" || v.Authored != nil && v.Authored("fail_when") {
		inputs["fail_when"] = u.FailWhen
	}
	return systemdUnitStateDescription.InputsProblem("systemd_unit_state", inputs)
}

func (u *SystemdUnitState) SystemdUnitName() string { return u.SystemdUnit }

func (u *SystemdUnitState) UnitThatMustBeActive() string { return u.SystemdUnit }

func (u *SystemdUnitState) Run(ctx context.Context, _ *CheckRun) Observation {
	fields, err := primitives.ServiceProperties(ctx, u.SystemdUnit, "ActiveState", "Result")
	if err != nil {
		return Result("unknown", err.Error())
	}
	return UnitStateObservation(u.SystemdUnit, fields["ActiveState"], fields["Result"])
}

// UnitStateObservation is the result of a systemd_unit_state check from
// the ActiveState and Result that systemd reports for the unit. A unit that
// runs (UnitRuns) is healthy. The message says which state systemd reports:
// a stopped unit is not a failed one, and MMWatchdog leaves a stopped unit stopped.
func UnitStateObservation(unit, activeState, systemdResult string) Observation {
	switch {
	case activeState == "":
		return Result("unknown", "systemd returned no ActiveState for "+unit)
	case primitives.UnitRuns(activeState):
		return Result("ok", "systemd unit "+unit+" is "+activeState)
	case activeState == "inactive" && strings.HasSuffix(unit, ".service"):
		return Result("failed", "systemd unit "+unit+" is stopped. systemd reports no failure: the unit was stopped on request, or its program ended normally. MMWatchdog did not start the unit.")
	case activeState == "inactive":
		return Result("failed", "systemd unit "+unit+" is stopped. systemd reports no failure: the unit was stopped on request, or was never started. MMWatchdog did not start the unit.")
	case activeState == "failed":
		return Result("failed", primitives.FailedUnitMessage(unit, systemdResult))
	}
	return Result("failed", "systemd unit "+unit+" is "+activeState)
}

// systemdUnitStateDescription is what "mmwatchdog describe-module systemd_unit_state" shows, and what
// validation knows about the module's inputs.
var systemdUnitStateDescription = ModuleDescription{
	Summary: "Asks systemd for the state of one unit of any kind, such as a service, a timer, or a path unit. The check is healthy while systemd reports the unit as active or reloading; a unit that is stopped, failed, or in another state fails the check.",
	UsedAs:  UsedAsCheck,
	Inputs: []ModuleInput{
		{Name: "systemd_unit", Type: "string", Required: true, Description: "The unit that the check watches, with its kind, such as myapp.service, backup.timer, or myapp.path."},
		{Name: "fail_when", Type: "string", Required: true, AllowedValues: []any{"the_unit_is_not_active"}, Description: "The check fails while systemd reports the unit as neither active nor reloading; write the_unit_is_not_active, the only value."},
	},
	Examples: []ModuleExample{{
		Description:   "Alert when the unit myapp.service is not active.",
		Configuration: "systemd_unit_state:\n  systemd_unit: myapp.service\n  fail_when: the_unit_is_not_active\nalert_severity: sos\n",
	}},
}
