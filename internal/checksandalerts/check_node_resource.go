package checksandalerts

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"time"

	"mmwatchdog/internal/primitives"
)

// NodeResource is the inputs of the node_resource check module: the one
// resource of the node that the check measures, which is one of the names
// in nodeResources.
type NodeResource string

// nodeResources are the resources that a node_resource check can name, in
// the order in which the documents list them.
var nodeResources = []string{"cpu", "memory", "swap", "load", "io_wait", "memory_pressure", "open_files"}

// NodeResourceMeasurements are what a node_resource check measures, by
// resource. The name of a measurement carries its unit; a limit, an alert,
// and a chart use the same name.
var NodeResourceMeasurements = map[string][]string{
	"cpu":             {"cpu_used_percent"},
	"memory":          {"memory_used_percent"},
	"swap":            {"swap_used_percent"},
	"load":            {"load_average_1m", "load_average_5m", "load_average_15m", "load_average_1m_per_core", "load_average_5m_per_core", "load_average_15m_per_core"},
	"io_wait":         {"io_wait_percent"},
	"memory_pressure": {"memory_pressure_percent"},
	"open_files":      {"open_files_used_percent"},
}

func (n *NodeResource) Validate(InputValidation) *FieldError {
	if NodeResourceMeasurements[string(*n)] == nil {
		return &FieldError{"", "must be one of " + strings.Join(nodeResources, ", ")}
	}
	return nil
}

func (n *NodeResource) MeasurementNames() []string { return NodeResourceMeasurements[string(*n)] }

// TimeOfEachRun is the sample of a measurement that comes from the CPU's
// counters: its first run measures over CPUSampleWindow.
func (n *NodeResource) TimeOfEachRun() (time.Duration, string) {
	if *n == "cpu" || *n == "io_wait" {
		return CPUSampleWindow, "the initial CPU sample"
	}
	return 0, ""
}

func (n *NodeResource) Run(ctx context.Context, r *CheckRun) Observation {
	// The alert of a check of the CPU, the memory, the swap space, or the
	// load names the commands that use the most of that resource. Only a
	// check that can fail needs them.
	canFail := r.Check.HasLimit() && r.Check.AlertSeverity != ""
	switch *n {
	case "cpu":
		read := ReadCPUStat
		if canFail {
			read = func(ctx context.Context) (primitives.CPUReading, error) {
				reading, err := ReadCPUStat(ctx)
				reading.Processes = primitives.ProcessCPUTicks()
				return reading, err
			}
		}
		return r.CPUSampler().Observe(ctx, r.Check, read, "cpu_used_percent", false)
	case "io_wait":
		return r.CPUSampler().Observe(ctx, r.Check, ReadCPUStat, "io_wait_percent", false)
	case "memory", "swap":
		name, percent, commands := "memory_used_percent", MemoryPercent, primitives.CommandsThatUseTheMostMemory
		if *n == "swap" {
			name, percent, commands = "swap_used_percent", SwapPercent, primitives.CommandsThatUseTheMostSwap
		}
		v, err := ReadMemoryInformation(percent)
		if err != nil {
			return Result("unknown", err.Error())
		}
		o := MeasurementObservation(r.Check, map[string]Measurement{name: {v, "percent"}}, nil)
		if o.Status == "failed" {
			o.Details = commands()
		}
		return o
	case "load":
		data, err := primitives.ReadFile(primitives.ProcDirectory+"/loadavg", primitives.ProcFileLimit)
		if err != nil {
			return Result("unknown", err.Error())
		}
		// Without the number of processors, the per-core measurements are
		// left out, and a limit on one makes the observation unknown.
		processors, unavailable := 0, map[string]string{}
		if cpu, err := ReadCPUStat(ctx); err == nil {
			processors = cpu.Processors
		} else {
			for _, window := range []string{"1m", "5m", "15m"} {
				unavailable["load_average_"+window+"_per_core"] = "the number of processors is unavailable: " + err.Error()
			}
		}
		values, err := LoadMeasurements(string(data), processors)
		if err != nil {
			return Result("unknown", err.Error())
		}
		o := MeasurementObservation(r.Check, values, unavailable)
		if o.Status == "failed" {
			o.Details = primitives.CommandsThatAddTheMostToTheLoad()
		}
		return o
	case "memory_pressure":
		data, err := primitives.ReadFile(primitives.ProcDirectory+"/pressure/memory", primitives.ProcFileLimit)
		if err != nil {
			// A kernel that was built or started without this accounting
			// has no such file, or refuses to read it.
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errors.ErrUnsupported) {
				return Result("unknown", "the kernel of this machine does not provide the memory pressure (/proc/pressure/memory)")
			}
			return Result("unknown", err.Error())
		}
		v, err := MemoryPressurePercent(string(data))
		if err != nil {
			return Result("unknown", err.Error())
		}
		return MeasurementObservation(r.Check, map[string]Measurement{"memory_pressure_percent": {v, "percent"}}, nil)
	case "open_files":
		data, err := primitives.ReadFile(primitives.ProcDirectory+"/sys/fs/file-nr", primitives.ProcFileLimit)
		if err != nil {
			return Result("unknown", err.Error())
		}
		v, err := OpenFilesPercent(string(data))
		if err != nil {
			return Result("unknown", err.Error())
		}
		return MeasurementObservation(r.Check, map[string]Measurement{"open_files_used_percent": {v, "percent"}}, nil)
	}
	return Result("unknown", "unsupported node resource "+string(*n))
}

// nodeResourceDescription is what "mmwatchdog describe-module node_resource" shows, and what
// validation knows about the module's inputs.
var nodeResourceDescription = ModuleDescription{
	Summary: "Measures one resource of the node: its CPU, its memory, its swap space, its load, the time that programs wait for a disk, the memory pressure, or its open files.",
	UsedAs:  UsedAsCheck,
	Value: &ModuleInput{Type: "string", Required: true, AllowedValues: func() []any {
		values := []any{}
		for _, resource := range nodeResources {
			values = append(values, resource)
		}
		return values
	}(), Description: "The resource to measure."},
	Measurements: []ModuleMeasurement{
		{Name: "cpu_used_percent", Unit: "percent", Description: "For cpu: the share of the node's processor time that was in use since the run before."},
		{Name: "memory_used_percent", Unit: "percent", Description: "For memory: the share of the node's memory that is not available to programs."},
		{Name: "swap_used_percent", Unit: "percent", Description: "For swap: the share of the swap space that is in use. A node without swap space uses 0."},
		{Name: "load_average_1m", Unit: "load", Description: "For load: the kernel's load average over 1 minute. load_average_5m and load_average_15m are the same over 5 and 15 minutes."},
		{Name: "load_average_1m_per_core", Unit: "load per core", Description: "For load: the load average over 1 minute for each processor of the node. load_average_5m_per_core and load_average_15m_per_core are the same over 5 and 15 minutes."},
		{Name: "io_wait_percent", Unit: "percent", Description: "For io_wait: the share of the processors' time since the run before in which they had nothing to run because programs waited for a disk."},
		{Name: "memory_pressure_percent", Unit: "percent", Description: "For memory_pressure: the share of the last 60 seconds in which at least one program had to wait for memory."},
		{Name: "open_files_used_percent", Unit: "percent", Description: "For open_files: the open files of the whole machine, as a share of the kernel's limit."},
	},
	Examples: []ModuleExample{{
		Description:   "Fail when more than 90 percent of the node's memory is in use.",
		Configuration: "node_resource: memory\nfail_when_measurement_above:\n  memory_used_percent: 90\nalert_severity: warn\n",
	}},
}
