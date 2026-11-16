package checksandalerts

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mmwatchdog/internal/primitives"
)

// NodeDiskDevice is the inputs of the node_disk_device check module: the
// path of the disk device that the check measures, such as /dev/sda. A disk
// device is the disk itself; a filesystem is what is mounted at a path.
type NodeDiskDevice string

// NodeDiskDeviceMeasurements are what a node_disk_device check measures.
var NodeDiskDeviceMeasurements = []string{"disk_device_busy_percent", "read_mib_per_second", "write_mib_per_second"}

func (n *NodeDiskDevice) Validate(InputValidation) *FieldError {
	if path := string(*n); !strings.HasPrefix(path, "/dev/") || filepath.Clean(path) != path {
		return &FieldError{"", "must be the path of a disk device, such as /dev/sda"}
	}
	return nil
}

func (n *NodeDiskDevice) MeasurementNames() []string { return NodeDiskDeviceMeasurements }

// TimeOfEachRun is the sample of the first run: a rate needs two readings of
// the kernel's counters.
func (n *NodeDiskDevice) TimeOfEachRun() (time.Duration, string) {
	return CPUSampleWindow, "the initial sample of the disk device"
}

// sectorBytes is the size of the sectors in which /proc/diskstats counts,
// whatever the sector size of the disk device is.
const sectorBytes = 512

func (n *NodeDiskDevice) Run(ctx context.Context, r *CheckRun) Observation {
	path := string(*n)
	// The kernel lists a disk device under its own name, such as dm-0, also
	// when the path is a link to it, such as /dev/mapper/data.
	resolved, err := primitives.ResolvePath(primitives.NodeFile(path))
	if err != nil {
		return Result("unknown", "MMWatchdog cannot resolve the device path "+path+": "+err.Error())
	}
	name := filepath.Base(resolved)
	read := func() ([]uint64, error) {
		data, err := primitives.ReadFile(primitives.ProcDirectory+"/diskstats", primitives.ProcFileLimit)
		if err != nil {
			return nil, err
		}
		return DiskDeviceCounters(string(data), name, path)
	}
	before, now, window, problem := r.CounterReadings(ctx, read)
	if problem != "" {
		return Result("unknown", problem)
	}
	seconds := window.Seconds()
	perSecond := func(counter int) float64 { return float64(now[counter]-before[counter]) / seconds }
	o := MeasurementObservation(r.Check, map[string]Measurement{
		// The kernel counts the milliseconds in which the device had work.
		"disk_device_busy_percent": {math.Min(100, perSecond(2)/10), "percent"},
		"read_mib_per_second":      {perSecond(0) * sectorBytes / (1024 * 1024), "MiB per second"},
		"write_mib_per_second":     {perSecond(1) * sectorBytes / (1024 * 1024), "MiB per second"},
	}, nil)
	o.SampleWindow = window
	o.Message += fmt.Sprintf(" over %.3fs", seconds)
	return o
}

// DiskDeviceCounters reads, from the text of /proc/diskstats, three counters
// of the disk device that the kernel lists under name: the sectors read,
// the sectors written, and the milliseconds in which the device had at
// least one request to work on. A line holds the two device numbers, the
// name, and then the counters: the sectors read are the third, the sectors
// written the seventh, and the milliseconds with work the tenth. path is
// the device as the check names it, for the error.
func DiskDeviceCounters(data, name, path string) ([]uint64, error) {
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 13 || fields[2] != name {
			continue
		}
		counters := []uint64{}
		for _, field := range []int{5, 9, 12} {
			value, err := strconv.ParseUint(fields[field], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("/proc/diskstats: invalid counter of %s", name)
			}
			counters = append(counters, value)
		}
		return counters, nil
	}
	return nil, fmt.Errorf("the disk device %s does not exist: the kernel's list of disk devices (/proc/diskstats) has no %s", path, name)
}

// nodeDiskDeviceDescription is what "mmwatchdog describe-module node_disk_device" shows, and what
// validation knows about the module's inputs.
var nodeDiskDeviceDescription = ModuleDescription{
	Summary: "Measures one disk device: how busy it is, and how much data is read from it and written to it.",
	UsedAs:  UsedAsCheck,
	Value:   &ModuleInput{Type: "string", Required: true, Description: "The path of the disk device, such as /dev/sda."},
	Measurements: []ModuleMeasurement{
		{Name: "disk_device_busy_percent", Unit: "percent", Description: "The share of the time since the run before in which the disk device had at least one request to work on."},
		{Name: "read_mib_per_second", Unit: "MiB per second", Description: "The data that was read from the disk device since the run before."},
		{Name: "write_mib_per_second", Unit: "MiB per second", Description: "The data that was written to the disk device since the run before."},
	},
	Examples: []ModuleExample{{
		Description:   "Fail when the disk device /dev/sda is busy more than 90 percent of the time.",
		Configuration: "node_disk_device: /dev/sda\nfail_when_measurement_above:\n  disk_device_busy_percent: 90\nalert_severity: warn\n",
	}},
}
