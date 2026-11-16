package checksandalerts

import (
	"context"
	"path/filepath"
	"slices"

	"mmwatchdog/internal/primitives"
)

// NodeFilesystem is the inputs of the node_filesystem check module: the
// absolute path where the filesystem that the check measures is mounted.
type NodeFilesystem string

// NodeFilesystemMeasurements are what a node_filesystem check measures.
var NodeFilesystemMeasurements = []string{"space_used_percent", "inodes_used_percent", "space_free_gib"}

func (n *NodeFilesystem) Validate(InputValidation) *FieldError {
	if !filepath.IsAbs(string(*n)) {
		return &FieldError{"", "must be the absolute path where the filesystem is mounted, such as /"}
	}
	return nil
}

func (n *NodeFilesystem) MeasurementNames() []string { return NodeFilesystemMeasurements }

func (n *NodeFilesystem) Run(ctx context.Context, r *CheckRun) Observation {
	path := filepath.Clean(string(*n))
	mounted, err := primitives.MountedFilesystems()
	if err != nil {
		return Result("unknown", "MMWatchdog cannot read the kernel's list of mounted filesystems: "+err.Error())
	}
	// Without a filesystem at the path, the kernel would answer for the
	// filesystem that holds the empty directory, usually /. A check that
	// states an alert_severity says that the filesystem must be mounted, so
	// it fails; a check that only records has nothing to record.
	if !slices.ContainsFunc(mounted, func(m primitives.MountedFilesystem) bool { return m.MountPoint == path }) {
		status := "unknown"
		if r.Check.AlertSeverity != "" {
			status = "failed"
		}
		return Result(status, "no filesystem is mounted at "+path)
	}
	counts, err := primitives.FilesystemStatistics(ctx, path)
	if err != nil {
		return Result("unknown", err.Error())
	}
	return FilesystemObservation(r.Check, path, counts)
}

// FilesystemObservation measures the space and inodes of the filesystem
// that is mounted at path, for check c. A read-only filesystem fails every
// write, so it fails a check that states an alert_severity, with or without
// a limit, and the readings are still recorded. A check that only records
// notes it in its message and does not fail.
func FilesystemObservation(c Check, path string, counts primitives.FilesystemCounts) Observation {
	var o Observation
	if used, known := counts.SpaceUsedPercent(); !known {
		o = Result("unknown", path+": filesystem counts unavailable or invalid")
	} else {
		values := map[string]Measurement{
			"space_used_percent": {used, "percent"},
			"space_free_gib":     {float64(counts.BlocksAvailable) * float64(counts.BlockBytes) / (1024 * 1024 * 1024), "GiB"},
		}
		unavailable := map[string]string{}
		switch {
		case counts.Inodes == 0:
			unavailable["inodes_used_percent"] = path + " has no fixed inode count, so inodes_used_percent cannot be measured"
		case counts.InodesFree > counts.Inodes:
			unavailable["inodes_used_percent"] = path + ": inode counts invalid"
		default:
			values["inodes_used_percent"] = Measurement{100 * (1 - float64(counts.InodesFree)/float64(counts.Inodes)), "percent"}
		}
		o = MeasurementObservation(c, values, unavailable)
	}
	if counts.ReadOnly {
		if c.AlertSeverity != "" {
			o.Status = "failed"
		}
		o.Message = path + " is mounted read-only; " + o.Message
	}
	return o
}

// nodeFilesystemDescription is what "mmwatchdog describe-module node_filesystem" shows, and what
// validation knows about the module's inputs.
var nodeFilesystemDescription = ModuleDescription{
	Summary: "Measures the space and the inodes of one filesystem. With an alert severity, the check also fails while nothing is mounted at the path, and while the filesystem is mounted read-only.",
	UsedAs:  UsedAsCheck,
	Value:   &ModuleInput{Type: "string", Required: true, Description: "The absolute path where the filesystem is mounted, such as /."},
	Measurements: []ModuleMeasurement{
		{Name: "space_used_percent", Unit: "percent", Description: "The share of the filesystem's space that is in use."},
		{Name: "inodes_used_percent", Unit: "percent", Description: "The share of the filesystem's inodes that is in use. A filesystem without a fixed number of inodes has no such measurement."},
		{Name: "space_free_gib", Unit: "GiB", Description: "The space that an ordinary account can still fill."},
	},
	Examples: []ModuleExample{{
		Description:   "Fail when the filesystem at / is more than 90 percent full, by space or by inodes.",
		Configuration: "node_filesystem: /\nfail_when_measurement_above:\n  space_used_percent: 90\n  inodes_used_percent: 90\nalert_severity: warn\n",
	}},
}
