package checksandalerts

import (
	"context"
	"errors"
	"time"

	"mmwatchdog/internal/primitives"
)

// SystemdUnitResource is the inputs of the systemd_unit_resource check
// module, which measures one resource of one systemd unit: cpu, memory, or
// processes.
type SystemdUnitResource struct {
	SystemdUnit string `yaml:"systemd_unit" json:"systemd_unit"`
	Resource    string `yaml:"resource" json:"resource"`
}

// SystemdUnitResourceMeasurements are what a systemd_unit_resource check
// measures, by resource.
var SystemdUnitResourceMeasurements = map[string][]string{
	"cpu":       {"cpu_used_percent"},
	"memory":    {"memory_used_mib"},
	"processes": {"process_count"},
}

func (u *SystemdUnitResource) Validate(InputValidation) *FieldError {
	if err := unitNameError("systemd_unit", u.SystemdUnit); err != nil {
		return err
	}
	if SystemdUnitResourceMeasurements[u.Resource] == nil {
		return &FieldError{"resource", "must be cpu, memory, or processes"}
	}
	return nil
}

func (u *SystemdUnitResource) SystemdUnitName() string { return u.SystemdUnit }

func (u *SystemdUnitResource) MeasurementNames() []string {
	return SystemdUnitResourceMeasurements[u.Resource]
}

// TimeOfEachRun is the sample of a CPU measurement: its first run measures
// over CPUSampleWindow.
func (u *SystemdUnitResource) TimeOfEachRun() (time.Duration, string) {
	if u.Resource == "cpu" {
		return CPUSampleWindow, "the initial CPU sample"
	}
	return 0, ""
}

func (u *SystemdUnitResource) Run(ctx context.Context, r *CheckRun) Observation {
	if u.Resource == "processes" {
		// A unit that does not run has no count, as it has no CPU and no
		// memory reading: the count of a stopped unit is not 0 processes of
		// a running one.
		processes, err := primitives.ProcessesOfSystemdUnit(ctx, u.SystemdUnit)
		if errors.Is(err, primitives.ErrSystemdUnitNotFound) {
			return Result("unknown", err.Error())
		}
		if err != nil {
			return Result("unknown", "systemd unit "+u.SystemdUnit+" process count unavailable: "+err.Error())
		}
		return MeasurementObservation(r.Check, map[string]Measurement{"process_count": {float64(len(processes)), "processes"}}, nil)
	}
	read := func(ctx context.Context) (primitives.CPUReading, float64, error) {
		fields, err := primitives.ServiceProperties(ctx, u.SystemdUnit, "ActiveState", "InvocationID", "CPUUsageNSec", "MemoryCurrent")
		at := time.Now()
		if err != nil {
			return primitives.CPUReading{}, 0, err
		}
		reading, memory, err := UsageOfProperties(fields, u.SystemdUnit, u.Resource)
		if err != nil {
			return reading, 0, err
		}
		if u.Resource == "cpu" {
			node, err := ReadCPUStat(ctx)
			if err != nil {
				return reading, 0, err
			}
			reading.Processors = node.Processors
		}
		reading.At = at
		return reading, memory, nil
	}
	if u.Resource == "cpu" {
		return r.CPUSampler().Observe(ctx, r.Check, func(ctx context.Context) (primitives.CPUReading, error) {
			reading, _, err := read(ctx)
			return reading, err
		}, "cpu_used_percent", true)
	}
	_, value, err := read(ctx)
	if err != nil {
		return Result("unknown", err.Error())
	}
	return MeasurementObservation(r.Check, map[string]Measurement{"memory_used_mib": {value, "MiB"}}, nil)
}

// systemdUnitResourceDescription is what "mmwatchdog describe-module systemd_unit_resource" shows, and what
// validation knows about the module's inputs.
var systemdUnitResourceDescription = ModuleDescription{
	Summary: "Measures one resource of one systemd unit: its CPU, its memory, or the number of its processes.",
	UsedAs:  UsedAsCheck,
	Inputs: []ModuleInput{
		{Name: "systemd_unit", Type: "string", Required: true, Description: "The .service unit to measure, such as myapp.service."},
		{Name: "resource", Type: "string", Required: true, AllowedValues: []any{"cpu", "memory", "processes"}, Description: "The resource to measure."},
	},
	Measurements: []ModuleMeasurement{
		{Name: "cpu_used_percent", Unit: "percent", Description: "For cpu: the share of the node's processor time that the unit used since the run before."},
		{Name: "memory_used_mib", Unit: "MiB", Description: "For memory: the memory that the unit uses."},
		{Name: "process_count", Unit: "processes", Description: "For processes: the number of processes that the unit runs, which is the number of commands that \"systemctl status\" lists for it."},
	},
	Examples: []ModuleExample{{
		Description:   "Fail when the unit myapp.service uses more than 512 MiB of memory.",
		Configuration: "systemd_unit_resource:\n  systemd_unit: myapp.service\n  resource: memory\nfail_when_measurement_above:\n  memory_used_mib: 512\nalert_severity: warn\n",
	}},
}
