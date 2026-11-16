package checksandalerts

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"mmwatchdog/internal/primitives"
)

// SystemdUnitCommands is the inputs of the systemd_unit_commands check
// module, which watches one program of a systemd unit that runs several:
// the check fails when no command that the unit runs contains
// FailWhenNoCommandContains.
type SystemdUnitCommands struct {
	SystemdUnit               string `yaml:"systemd_unit" json:"systemd_unit"`
	FailWhenNoCommandContains string `yaml:"fail_when_no_command_contains" json:"fail_when_no_command_contains"`
}

func (u *SystemdUnitCommands) Validate(InputValidation) *FieldError {
	if err := unitNameError("systemd_unit", u.SystemdUnit); err != nil {
		return err
	}
	if strings.TrimSpace(u.FailWhenNoCommandContains) == "" {
		return &FieldError{"fail_when_no_command_contains", "is required: a text that one of the unit's commands contains, such as the name of the program"}
	}
	return nil
}

func (u *SystemdUnitCommands) SystemdUnitName() string { return u.SystemdUnit }

func (u *SystemdUnitCommands) Run(ctx context.Context, _ *CheckRun) Observation {
	none := "systemd unit " + u.SystemdUnit + " runs no command that contains " + u.FailWhenNoCommandContains
	processes, err := primitives.ProcessesOfSystemdUnit(ctx, u.SystemdUnit)
	// A unit that does not run runs no command.
	if notActive := (primitives.ErrUnitDoesNotRun{}); errors.As(err, &notActive) {
		return Result("failed", none+": "+err.Error())
	}
	if err != nil {
		return Result("unknown", err.Error())
	}
	for _, process := range processes {
		if strings.Contains(process.Command, u.FailWhenNoCommandContains) {
			return Result("ok", primitives.DetailLine(fmt.Sprintf("systemd unit %s runs %s (process %d)", u.SystemdUnit, primitives.Clip(process.Command, 120), process.ID)))
		}
	}
	o := Result("failed", none)
	// The commands that the unit does run show a mistyped text at once.
	for _, process := range processes {
		if len(o.Details) == primitives.DetailLinesLimit {
			break
		}
		if len(o.Details) == 0 {
			o.Details = []string{"the commands that the unit runs:"}
		}
		o.Details = append(o.Details, "  "+primitives.DetailLine(fmt.Sprintf("%s (process %d)", primitives.Clip(process.Command, 120), process.ID)))
	}
	return o
}

// systemdUnitCommandsDescription is what "mmwatchdog describe-module systemd_unit_commands" shows, and what
// validation knows about the module's inputs.
var systemdUnitCommandsDescription = ModuleDescription{
	Summary: "Watches one program of a systemd unit that runs several. The check is healthy while one of the commands that \"systemctl status\" lists for the unit contains a text.",
	UsedAs:  UsedAsCheck,
	Inputs: []ModuleInput{
		{Name: "systemd_unit", Type: "string", Required: true, Description: "The .service unit that runs the program, such as myapp.service."},
		{Name: "fail_when_no_command_contains", Type: "string", Required: true, Description: "The check fails when no command of the unit contains this text: the name of the program, or one of its arguments."},
	},
	Examples: []ModuleExample{{
		Description:   "The unit myapp.service runs its worker program.",
		Configuration: "systemd_unit_commands:\n  systemd_unit: myapp.service\n  fail_when_no_command_contains: myapp-worker\nalert_severity: sos\n",
	}},
}
