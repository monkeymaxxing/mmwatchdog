package checksandalerts

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"mmwatchdog/internal/primitives"
)

const CPUSampleWindow = 200 * time.Millisecond

// CPU baselines are ephemeral and owned by a scheduled check's execution
// gate. Diagnostic checks use their own sampler; no rates survive a reboot.
type CPUSampler struct{ Previous *primitives.CPUReading }

// baselineIsTooOld reports that a reading which a check took at a time says
// nothing about the node now: a check with a schedule forgets a baseline
// that is older than two of its intervals and one time limit.
func (r *CheckRun) baselineIsTooOld(at time.Time) bool {
	every := r.Timing.CheckEvery.Value()
	return every > 0 && time.Since(at) > 2*every+r.Timing.TimeLimitPerRun.Value()
}

// CPUSampler returns the sampler that the check keeps in the memory of its
// runs, without a baseline that is too old.
func (r *CheckRun) CPUSampler() *CPUSampler {
	sampler, _ := r.Memory.Value.(*CPUSampler)
	if sampler == nil {
		sampler = &CPUSampler{}
		r.Memory.Value = sampler
	}
	if sampler.Previous != nil && r.baselineIsTooOld(sampler.Previous.At) {
		sampler.Previous = nil
	}
	return sampler
}

// Observe measures one share of the processor time for check c:
// cpu_used_percent of the whole node, or with service of one systemd unit,
// or io_wait_percent of the whole node. The observation of a check that
// fails names the commands that used the most CPU, when the readings hold
// the processes.
func (s *CPUSampler) Observe(ctx context.Context, c Check, read func(context.Context) (primitives.CPUReading, error), measurement string, service bool) Observation {
	previous, current, problem := s.readings(ctx, read, service)
	if problem != "" {
		return Result("unknown", problem)
	}
	share := CPUPercent
	if measurement == "io_wait_percent" {
		share = func(a, b primitives.CPUReading, _ bool) (float64, bool) { return IOWaitPercent(a, b) }
	}
	value, ok := share(previous, current, service)
	if !ok {
		return Result("unknown", "CPU counters changed or did not advance between samples; MMWatchdog waits for a new sample")
	}
	o := MeasurementObservation(c, map[string]Measurement{measurement: {value, "percent"}}, nil)
	o.SampleWindow = current.At.Sub(previous.At)
	o.Message += fmt.Sprintf(" over %.3fs", o.SampleWindow.Seconds())
	if o.Status == "failed" {
		o.Details = primitives.CommandsThatUsedTheMostCPU(previous, current)
	}
	return o
}

// Measure returns the percent of CPU time that was in use since the
// sampler's previous reading, and the time between the two readings. A
// measurement that cannot be made returns why.
func (s *CPUSampler) Measure(ctx context.Context, read func(context.Context) (primitives.CPUReading, error), service bool) (value float64, window time.Duration, problem string) {
	previous, current, problem := s.readings(ctx, read, service)
	if problem != "" {
		return 0, 0, problem
	}
	value, ok := CPUPercent(previous, current, service)
	if !ok {
		return 0, 0, "CPU counters changed or did not advance between samples; MMWatchdog waits for a new sample"
	}
	return value, current.At.Sub(previous.At), ""
}

// readings returns the sampler's previous reading and a new one, which the
// sampler keeps for the next call. The first call of a sampler, and one
// whose baseline no longer compares, takes its own baseline and waits for
// CPUSampleWindow. Readings that cannot be made return why.
func (s *CPUSampler) readings(ctx context.Context, read func(context.Context) (primitives.CPUReading, error), service bool) (before, now primitives.CPUReading, problem string) {
	current, err := read(ctx)
	if err != nil {
		s.Previous = nil
		return before, now, err.Error()
	}
	previous := s.Previous
	if previous == nil || !cpuComparable(*previous, current, service) {
		baseline := current
		previous = &baseline
		timer := time.NewTimer(CPUSampleWindow)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			s.Previous = nil
			return before, now, "CPU sampling: " + ctx.Err().Error()
		case <-timer.C:
		}
		current, err = read(ctx)
		if err != nil {
			s.Previous = nil
			return before, now, err.Error()
		}
	}
	s.Previous = &current
	return *previous, current, ""
}

// CounterSampler keeps, for a check that measures a rate, the previous
// reading of the kernel's counters, as CPUSampler does for the CPU: a disk
// device's and a network interface's counters only grow, and a rate is the
// growth between two readings.
type CounterSampler struct {
	Previous []uint64
	At       time.Time
}

// CounterReadings returns the previous reading of the counters of the
// check, a new one, and the time between the two. The first run of a check,
// and one whose baseline is too old or lies after a counter that fell back,
// takes its own baseline and waits for CPUSampleWindow. Readings that cannot
// be made return why.
func (r *CheckRun) CounterReadings(ctx context.Context, read func() ([]uint64, error)) (before, now []uint64, window time.Duration, problem string) {
	sampler, _ := r.Memory.Value.(*CounterSampler)
	if sampler == nil {
		sampler = &CounterSampler{}
		r.Memory.Value = sampler
	}
	current, err := read()
	at := time.Now()
	if err != nil {
		sampler.Previous = nil
		return nil, nil, 0, err.Error()
	}
	previous, previousAt := sampler.Previous, sampler.At
	fellBack := len(previous) != len(current)
	for i := 0; !fellBack && i < len(current); i++ {
		fellBack = current[i] < previous[i]
	}
	if previous == nil || fellBack || r.baselineIsTooOld(previousAt) || !at.After(previousAt) {
		previous, previousAt = current, at
		timer := time.NewTimer(CPUSampleWindow)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			sampler.Previous = nil
			return nil, nil, 0, "sampling: " + ctx.Err().Error()
		case <-timer.C:
		}
		if current, err = read(); err != nil {
			sampler.Previous = nil
			return nil, nil, 0, err.Error()
		}
		at = time.Now()
		for i := range current {
			if len(current) != len(previous) || current[i] < previous[i] {
				sampler.Previous = nil
				return nil, nil, 0, "the kernel's counters changed while sampling; awaiting a fresh sample"
			}
		}
	}
	sampler.Previous, sampler.At = current, at
	return previous, current, at.Sub(previousAt), ""
}

// ReadCPUStat reads the node's CPU counters and its number of processors
// from /proc/stat.
func ReadCPUStat(ctx context.Context) (primitives.CPUReading, error) {
	if err := ctx.Err(); err != nil {
		return primitives.CPUReading{}, err
	}
	data, err := primitives.ReadFile(primitives.ProcDirectory+"/stat", primitives.ProcFileLimit)
	if err != nil {
		return primitives.CPUReading{}, err
	}
	r, err := ParseCPUStat(string(data))
	r.At = time.Now()
	return r, err
}

// ReadMemoryInformation reads /proc/meminfo and returns what percent makes
// of it: the percent of memory or of swap space that is in use.
func ReadMemoryInformation(percent func(string) (float64, error)) (float64, error) {
	data, err := primitives.ReadFile(primitives.ProcDirectory+"/meminfo", primitives.ProcFileLimit)
	if err != nil {
		return 0, err
	}
	return percent(string(data))
}
func cpuComparable(a, b primitives.CPUReading, service bool) bool {
	if a.Processors <= 0 || a.Processors != b.Processors || !b.At.After(a.At) {
		return false
	}
	if service {
		return a.Invocation != "" && a.Invocation == b.Invocation && b.Usage >= a.Usage
	}
	for i := range a.Ticks {
		if i != 4 && b.Ticks[i] < a.Ticks[i] {
			return false
		}
	}
	return true
}
func CPUPercent(a, b primitives.CPUReading, service bool) (float64, bool) {
	if !cpuComparable(a, b, service) {
		return 0, false
	}
	if service {
		return math.Min(100, 100*float64(b.Usage-a.Usage)/float64(b.At.Sub(a.At))/float64(b.Processors)), true
	}
	var total, busy float64
	for i := range a.Ticks {
		// Linux documents that iowait can decrease. Treat a backward iowait delta
		// as zero; other backward counters require a new baseline.
		if b.Ticks[i] < a.Ticks[i] {
			continue
		}
		delta := float64(b.Ticks[i] - a.Ticks[i])
		total += delta
		if i != 3 && i != 4 && i != 7 {
			busy += delta
		}
	}
	if total == 0 {
		return 0, false
	}
	return 100 * busy / total, true
}

// IOWaitPercent is the percent of the processors' time between two readings
// in which they had nothing to run because programs waited for a disk.
func IOWaitPercent(a, b primitives.CPUReading) (float64, bool) {
	if !cpuComparable(a, b, false) {
		return 0, false
	}
	var total, waiting float64
	for i := range a.Ticks {
		// As in CPUPercent, a backward delta of the I/O wait counts as zero.
		if b.Ticks[i] < a.Ticks[i] {
			continue
		}
		delta := float64(b.Ticks[i] - a.Ticks[i])
		total += delta
		if i == 4 {
			waiting = delta
		}
	}
	if total == 0 {
		return 0, false
	}
	return 100 * waiting / total, true
}

// MemoryPressurePercent reads the text of /proc/pressure/memory: the percent
// of the last 60 seconds in which at least one program had to wait for
// memory, which the kernel writes as avg60 in the line that starts with
// "some".
func MemoryPressurePercent(data string) (float64, error) {
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "some" {
			continue
		}
		for _, field := range fields[1:] {
			if text, found := strings.CutPrefix(field, "avg60="); found {
				if value, err := strconv.ParseFloat(text, 64); err == nil && value >= 0 && value <= 100 {
					return value, nil
				}
			}
		}
	}
	return 0, fmt.Errorf("/proc/pressure/memory: the pressure of the last 60 seconds is unavailable")
}

// OpenFilesPercent reads the text of /proc/sys/fs/file-nr: the open files
// of the whole machine as a percent of the kernel's limit. The file holds
// the files that the kernel has allocated, those of them that are not in
// use, and the limit.
func OpenFilesPercent(data string) (float64, error) {
	fields := strings.Fields(data)
	if len(fields) != 3 {
		return 0, fmt.Errorf("/proc/sys/fs/file-nr: expected three numbers")
	}
	numbers := [3]uint64{}
	for i, field := range fields {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("/proc/sys/fs/file-nr: invalid number")
		}
		numbers[i] = value
	}
	allocated, unused, limit := numbers[0], numbers[1], numbers[2]
	if limit == 0 || unused > allocated || allocated-unused > limit {
		return 0, fmt.Errorf("/proc/sys/fs/file-nr: valid counts of open files and their limit are required")
	}
	return 100 * float64(allocated-unused) / float64(limit), nil
}

func ParseCPUStat(data string) (primitives.CPUReading, error) {
	r := primitives.CPUReading{}
	found := false
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "cpu" {
			if found || len(fields) < 9 {
				return r, fmt.Errorf("/proc/stat: expected eight CPU counters")
			}
			found = true
			for i := range r.Ticks {
				value, err := strconv.ParseUint(fields[i+1], 10, 64)
				if err != nil {
					return r, fmt.Errorf("/proc/stat: invalid CPU counter")
				}
				r.Ticks[i] = value
			}
			// guest/guest_nice are already counted in user/nice; never add them twice.
		} else if strings.HasPrefix(fields[0], "cpu") {
			if _, err := strconv.ParseUint(strings.TrimPrefix(fields[0], "cpu"), 10, 32); err == nil {
				r.Processors++
			}
		}
	}
	if !found || r.Processors == 0 {
		return r, fmt.Errorf("/proc/stat: CPU counters or processor count unavailable")
	}
	return r, nil
}

func MemoryPercent(data string) (float64, error) {
	values, err := primitives.Meminfo(data, "MemTotal:", "MemAvailable:")
	if err != nil {
		return 0, err
	}
	total, available := values["MemTotal:"], values["MemAvailable:"]
	if total == 0 || available > total {
		return 0, fmt.Errorf("/proc/meminfo: valid MemTotal and MemAvailable are required")
	}
	return 100 * (1 - float64(available)/float64(total)), nil
}

// SwapPercent is the percent of swap space used; a node without swap uses 0%.
func SwapPercent(data string) (float64, error) {
	values, err := primitives.Meminfo(data, "SwapTotal:", "SwapFree:")
	if err != nil {
		return 0, err
	}
	total, free := values["SwapTotal:"], values["SwapFree:"]
	if free > total {
		return 0, fmt.Errorf("/proc/meminfo: SwapFree exceeds SwapTotal")
	}
	if total == 0 {
		return 0, nil
	}
	return 100 * (1 - float64(free)/float64(total)), nil
}

// LoadAverage reads the kernel's 1m, 5m, or 15m load average from /proc/loadavg text.
func LoadAverage(data, window string) (float64, error) {
	fields := strings.Fields(data)
	i := slices.Index([]string{"1m", "5m", "15m"}, window)
	if i < 0 || len(fields) < 3 {
		return 0, fmt.Errorf("/proc/loadavg: %s load average unavailable", window)
	}
	v, err := strconv.ParseFloat(fields[i], 64)
	if err != nil {
		return 0, fmt.Errorf("/proc/loadavg: invalid load average")
	}
	return v, nil
}

// LoadMeasurements reads the kernel's load averages from /proc/loadavg text.
// Each is also given per core when the number of logical processors is
// known; processors is 0 when it is not.
func LoadMeasurements(data string, processors int) (map[string]Measurement, error) {
	values := map[string]Measurement{}
	for _, window := range []string{"1m", "5m", "15m"} {
		v, err := LoadAverage(data, window)
		if err != nil {
			return nil, err
		}
		values["load_average_"+window] = Measurement{v, "load"}
		if processors > 0 {
			values["load_average_"+window+"_per_core"] = Measurement{v / float64(processors), "load per core"}
		}
	}
	return values, nil
}

// MeasurementObservation turns the readings of a module that measures
// into its observation: applyLimits decides whether a reading passes a
// limit of check c and writes the message.
func MeasurementObservation(c Check, values map[string]Measurement, unavailable map[string]string) Observation {
	for _, name := range Names(values) {
		if v := values[name].Value; math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return Result("unknown", name+" measurement is invalid")
		}
	}
	o := Result("ok", "")
	o.Measurements = values
	o.Status, o.Message = applyLimits(c, o, c.MeasurementNames(), unavailable)
	if o.Message == "" {
		return Result("unknown", "no measurement is available")
	}
	return o
}

// withLimits applies the limits of check c to the observation of a module
// that says "failed" by itself and also reports measurements. The module
// found the thing that it checks healthy, or o is returned as it is. The
// message then names each reading that has a limit, in front of what the
// module said. unavailable gives, for a measurement that this run could not
// make, the reason.
func withLimits(c Check, o Observation, unavailable map[string]string) Observation {
	switch status, text := applyLimits(c, o, nil, unavailable); {
	case status == "unknown" && text != "":
		o.Status, o.Message = status, text
	case text != "":
		o.Status, o.Message = status, text+"; "+o.Message
	}
	return o
}

// applyLimits compares the readings of an ok observation with the limits of
// check c and returns the resulting status and the text that names each
// reading. A reading strictly above its fail_when_measurement_above number,
// or strictly below its fail_when_measurement_below number, fails; a reading
// equal to the number passes. A limit on a measurement the observation does
// not have makes it unknown, with the reason from unavailable when the kind
// gave one.
//
// names orders the measurements of a module that names them; nil stands for
// an external module, whose limited measurements are named in sorted order.
// The text names each measurement that has a limit with its reading and
// limits, such as "memory_used_percent is 94 (fails above 90)"; without any
// limit it names every reading in names. An observation that is not ok is
// returned as it is, with empty text: a module decides failed and unknown
// by itself.
func applyLimits(c Check, o Observation, names []string, unavailable map[string]string) (string, string) {
	if o.Status != "ok" {
		return o.Status, ""
	}
	if names == nil {
		limited := map[string]bool{}
		for _, field := range LimitSettings {
			for name := range c.Limits(field) {
				limited[name] = true
			}
		}
		names = Names(limited)
	}
	status, parts := "ok", []string{}
	for _, name := range names {
		above, hasAbove := c.FailWhenMeasurementAbove[name]
		below, hasBelow := c.FailWhenMeasurementBelow[name]
		if c.HasLimit() && !hasAbove && !hasBelow {
			continue
		}
		reading, measured := o.Measurements[name]
		if !measured {
			if !hasAbove && !hasBelow {
				continue
			}
			reason := unavailable[name]
			if reason == "" {
				reason = "the result has no measurement named " + name + ", which this check has a limit on"
			}
			return "unknown", reason
		}
		text := name + " is " + primitives.FormatReading(reading.Value)
		limits := []string{}
		if hasAbove {
			limits = append(limits, "above "+formatLimit(above))
			if reading.Value > above {
				status = "failed"
			}
		}
		if hasBelow {
			limits = append(limits, "below "+formatLimit(below))
			if reading.Value < below {
				status = "failed"
			}
		}
		if len(limits) > 0 {
			text += " (fails " + strings.Join(limits, ", ") + ")"
		}
		parts = append(parts, text)
	}
	return status, strings.Join(parts, "; ")
}

// formatLimit writes a limit as the configuration states it.
func formatLimit(value float64) string { return strconv.FormatFloat(value, 'f', -1, 64) }

// UsageOfProperties reads the CPU or the memory of a unit from the systemd
// properties that ServiceProperties read. Only a unit that runs (UnitRuns)
// has a reading.
func UsageOfProperties(fields map[string]string, unit, resource string) (primitives.CPUReading, float64, error) {
	r := primitives.CPUReading{}
	state := fields["ActiveState"]
	if !primitives.UnitRuns(state) {
		return r, 0, fmt.Errorf("systemd unit %s usage unavailable: the unit is %s", unit, state)
	}
	property, accounting := "MemoryCurrent", "MemoryAccounting"
	if resource == "cpu" {
		property, accounting = "CPUUsageNSec", "CPUAccounting"
	}
	value, err := strconv.ParseUint(fields[property], 10, 64)
	if err != nil || value == math.MaxUint64 {
		return r, 0, fmt.Errorf("systemd unit %s has no usable %s value; enable %s=yes in the unit configuration", unit, property, accounting)
	}
	if resource == "cpu" {
		r.Invocation = fields["InvocationID"]
		if r.Invocation == "" {
			return r, 0, fmt.Errorf("systemd unit %s has no InvocationID; MMWatchdog cannot start measuring CPU usage", unit)
		}
		r.Usage = value
		return r, 0, nil
	}
	return r, float64(value) / (1024 * 1024), nil
}
