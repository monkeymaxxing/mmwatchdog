package checksandalerts

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"mmwatchdog/internal/primitives"
)

// SystemdUnitCrashLoop is the inputs of the systemd_unit_crash_loop check
// module, which fails when systemd automatically restarted SystemdUnit more
// than AutomaticRestartsAbove times within AutomaticRestartsWithin.
type SystemdUnitCrashLoop struct {
	SystemdUnit             string   `yaml:"systemd_unit" json:"systemd_unit"`
	AutomaticRestartsAbove  int      `yaml:"automatic_restarts_above" json:"automatic_restarts_above"`
	AutomaticRestartsWithin Duration `yaml:"automatic_restarts_within" json:"automatic_restarts_within"`
}

func (c *SystemdUnitCrashLoop) Validate(v InputValidation) *FieldError {
	if !v.Authored("automatic_restarts_above") {
		return &FieldError{"automatic_restarts_above", "is required: a whole number of restarts, such as 2"}
	}
	if err := unitNameError("systemd_unit", c.SystemdUnit); err != nil {
		return err
	}
	if c.AutomaticRestartsAbove < 0 {
		return &FieldError{"automatic_restarts_above", "cannot be negative"}
	}
	if c.AutomaticRestartsWithin.Value() <= 0 {
		return &FieldError{"automatic_restarts_within", "is required: a positive duration such as 1h"}
	}
	return nil
}

func (c *SystemdUnitCrashLoop) SystemdUnitName() string { return c.SystemdUnit }

// Run reads the unit's automatic restarts (NRestarts, which counts only
// restarts by its Restart= setting) and how its main process last exited,
// and counts them on the check's history of restarts, which is the state
// that the module keeps between runs.
func (c *SystemdUnitCrashLoop) Run(ctx context.Context, r *CheckRun) Observation {
	fields, err := primitives.ServiceProperties(ctx, c.SystemdUnit, "NRestarts", "Result", "ExecMainStatus")
	if err != nil {
		return Result("unknown", err.Error())
	}
	count, err := strconv.Atoi(fields["NRestarts"])
	if err != nil || count < 0 {
		return Result("unknown", "systemd returned no valid NRestarts for "+c.SystemdUnit)
	}
	exit := ""
	if res, status := fields["Result"], fields["ExecMainStatus"]; res != "success" || status != "0" {
		exit = res + " (status " + status + ")"
	}
	history := &RestartHistory{}
	r.State.load(history)
	o := history.Observe(time.Now(), count, exit, c)
	r.State.Save(history)
	return o
}

// RestartHistory counts a unit's automatic restarts for its crash-loop
// check. It is the module's state, so after MMWatchdog starts, counting continues
// from the last reading. A run works on a copy; only a scheduled run that
// MMWatchdog accepted replaces the check's history.
type RestartHistory struct {
	Started bool `json:"started"`
	Count   int  `json:"count"` // systemd's NRestarts at the previous reading
	// Batches are the restarts still within the window, by reading.
	Batches []restartBatch `json:"batches,omitempty"`
	// systemd clears Result and the exit status when the unit starts again,
	// so the last exit is the latest one a reading showed.
	LastExit   string    `json:"last_exit,omitempty"`
	LastExitAt time.Time `json:"last_exit_at,omitempty"`
}

// restartBatch holds the restarts one reading found since the previous one.
type restartBatch struct {
	At time.Time `json:"at"`
	N  int       `json:"n"`
}

// Observe applies one reading taken at now: systemd's restart count and the
// unit's exit, or "" when it shows none. An increase is that many restarts
// at now; after a decrease, the new value counts the automatic restarts
// since systemd reset its counter. The check fails while more than AutomaticRestartsAbove
// restarts fall within AutomaticRestartsWithin.
//
// The first reading, in a history that has none, only sets the starting
// count, and its message says so: "0 automatic restarts" would read as if
// MMWatchdog had counted over the window and found none.
func (h *RestartHistory) Observe(now time.Time, count int, exit string, r *SystemdUnitCrashLoop) Observation {
	first := !h.Started
	if !first {
		restarts := count - h.Count
		if count < h.Count {
			restarts = count
		}
		if restarts > 0 {
			h.Batches = append(h.Batches, restartBatch{now, restarts})
		}
	}
	h.Started, h.Count = true, count
	if exit != "" {
		h.LastExit, h.LastExitAt = exit, now
	}
	if first {
		o := Result("ok", fmt.Sprintf("first look at systemd unit %s: MMWatchdog counts the unit's automatic restarts between check runs, from the current run on; systemd's own count of the unit's automatic restarts (NRestarts) is %d", r.SystemdUnit, count))
		o.Measurements = map[string]Measurement{"automatic_restarts": {0, "count"}}
		return o
	}
	within := r.AutomaticRestartsWithin.Value()
	for len(h.Batches) > 0 && now.Sub(h.Batches[0].At) >= within {
		h.Batches = h.Batches[1:]
	}
	n := 0
	for _, b := range h.Batches {
		n += b.N
	}
	period := "the last " + string(r.AutomaticRestartsWithin)
	if within == time.Hour {
		period = "the last hour"
	}
	noun := "restarts"
	if n == 1 {
		noun = "restart"
	}
	o := Result("ok", fmt.Sprintf("%d automatic %s in %s", n, noun, period))
	if n > 0 && h.LastExit != "" && now.Sub(h.LastExitAt) < within {
		o.Message += "; last exit: " + h.LastExit
	}
	if n > r.AutomaticRestartsAbove {
		o.Status = "failed"
	}
	o.Measurements = map[string]Measurement{"automatic_restarts": {float64(n), "count"}}
	return o
}

// systemdUnitCrashLoopDescription is what "mmwatchdog describe-module systemd_unit_crash_loop" shows, and what
// validation knows about the module's inputs.
var systemdUnitCrashLoopDescription = ModuleDescription{
	Summary: "Counts automatic restarts of a systemd unit. The check fails when the restart count exceeds automatic_restarts_above within automatic_restarts_within.",
	UsedAs:  UsedAsCheck,
	Inputs: []ModuleInput{
		{Name: "systemd_unit", Type: "string", Required: true, Description: "The .service unit to watch, such as myapp.service."},
		{Name: "automatic_restarts_above", Type: "integer", Required: true, Description: "The maximum number of automatic restarts allowed within automatic_restarts_within. A value of 0 fails the check at the first automatic restart."},
		{Name: "automatic_restarts_within", Type: "duration", Required: true, Description: "The time over which the restarts count, such as 1h."},
	},
	Measurements: []ModuleMeasurement{{Name: "automatic_restarts", Unit: "count", Description: "The number of automatic restarts within automatic_restarts_within."}},
	Examples: []ModuleExample{{
		Description:   "Fail when systemd restarted myapp.service more than 2 times within an hour.",
		Configuration: "systemd_unit_crash_loop:\n  systemd_unit: myapp.service\n  automatic_restarts_above: 2\n  automatic_restarts_within: 1h\nalert_severity: warn\n",
	}},
}
