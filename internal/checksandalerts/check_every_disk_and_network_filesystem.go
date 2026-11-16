package checksandalerts

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"mmwatchdog/internal/primitives"
)

// EveryDiskAndNetworkFilesystem is the inputs of the every_disk_and_network_filesystem check
// module, which applies the limits of its check to every filesystem of the
// node without naming each: a filesystem that nobody listed is the one that
// fills up. Each filesystem that passes a limit, is mounted read-only, or
// does not answer is one finding of the check, and so its own incident.
// FilesystemsToIgnore is required, so a reader sees whether any filesystem
// is left out; an empty list leaves none out.
type EveryDiskAndNetworkFilesystem struct {
	FilesystemsToIgnore []string `yaml:"filesystems_to_ignore" json:"filesystems_to_ignore"`
}

func (m *EveryDiskAndNetworkFilesystem) Validate(v InputValidation) *FieldError {
	if !v.Authored("filesystems_to_ignore") || m.FilesystemsToIgnore == nil {
		return &FieldError{"filesystems_to_ignore", "is required: the paths where the filesystems are mounted that this check leaves out, such as [/boot/efi], or [] to leave none out"}
	}
	seen := map[string]bool{}
	for _, path := range m.FilesystemsToIgnore {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || seen[path] {
			return &FieldError{"filesystems_to_ignore", "expected distinct absolute paths where filesystems are mounted, such as [/boot/efi, /mnt/backup]"}
		}
		seen[path] = true
	}
	return nil
}

// Identity leaves the order in which the ignored filesystems are written
// out of the check's identity.
func (m *EveryDiskAndNetworkFilesystem) Identity() CheckInputs {
	ignored := slices.Clone(m.FilesystemsToIgnore)
	slices.Sort(ignored)
	return &EveryDiskAndNetworkFilesystem{FilesystemsToIgnore: ignored}
}

func (m *EveryDiskAndNetworkFilesystem) MeasurementNames() []string {
	return NodeFilesystemMeasurements
}

// filesystemTypesOnOtherMachines are the kinds of filesystem that are
// mounted from another machine, or, as zfs, from a pool of disks that the
// kernel does not name as one device.
var filesystemTypesOnOtherMachines = []string{"nfs", "nfs4", "cifs", "smb3", "ceph", "9p", "virtiofs", "lustre", "afs", "fuse.sshfs", "fuse.glusterfs", "fuse.ceph", "zfs"}

// readOnlyImageFilesystemTypes are the kinds of filesystem that hold a
// finished image, such as a CD or the package of a snap. Such a filesystem
// is always full and always read-only.
var readOnlyImageFilesystemTypes = []string{"squashfs", "iso9660", "udf", "erofs", "cramfs", "romfs"}

// FilesystemsWithStorage returns, of the kernel's list of mounts, the
// filesystems that an every_disk_and_network_filesystem check covers: the ones that
// are mounted from a disk device or from another machine. The ones that the
// kernel provides in memory, such as /proc and /run, are left out, and so is
// an image that is read-only by its nature. A filesystem that is mounted at
// several paths is covered once, at the path that the kernel lists first;
// alsoMountedAt holds its other paths, by that first path.
func FilesystemsWithStorage(mounted []primitives.MountedFilesystem) (covered []primitives.MountedFilesystem, alsoMountedAt map[string][]string) {
	covered, alsoMountedAt = []primitives.MountedFilesystem{}, map[string][]string{}
	firstPath := map[string]string{}
	for _, m := range mounted {
		onDisk := strings.HasPrefix(m.Source, "/dev/") && !slices.Contains(readOnlyImageFilesystemTypes, m.FilesystemType)
		if !onDisk && !slices.Contains(filesystemTypesOnOtherMachines, m.FilesystemType) {
			continue
		}
		source := m.FilesystemType + " " + m.Source
		if first, seen := firstPath[source]; seen {
			alsoMountedAt[first] = append(alsoMountedAt[first], m.MountPoint)
			continue
		}
		firstPath[source] = m.MountPoint
		covered = append(covered, m)
	}
	return covered, alsoMountedAt
}

// Run measures every covered filesystem and applies the limits of the check
// to each. The observation lists the filesystems that fail as its findings,
// and CheckState.observeFindings gives each of them an incident of its own.
func (m *EveryDiskAndNetworkFilesystem) Run(ctx context.Context, r *CheckRun) Observation {
	mounted, err := primitives.MountedFilesystems()
	if err != nil {
		return Result("unknown", "MMWatchdog cannot read the kernel's list of mounted filesystems: "+err.Error())
	}
	paths, ignored := []string{}, []string{}
	covered, alsoMountedAt := FilesystemsWithStorage(mounted)
	for _, filesystem := range covered {
		path := filesystem.MountPoint
		// filesystems_to_ignore can name any path where the filesystem is
		// mounted.
		listed := slices.IndexFunc(append([]string{path}, alsoMountedAt[path]...), func(mountedAt string) bool { return slices.Contains(m.FilesystemsToIgnore, mountedAt) })
		switch {
		case listed == 0:
			ignored = append(ignored, path)
		case listed > 0:
			ignored = append(ignored, alsoMountedAt[path][listed-1]) //nolint:nilaway // listed > 0 selects an additional mount path found in the same alsoMountedAt slice.
		default:
			paths = append(paths, path)
		}
	}
	// MMWatchdog asks about all filesystems at once, so a filesystem that does not
	// answer takes no time from the others.
	type answer struct {
		counts primitives.FilesystemCounts
		err    error
	}
	answers := make([]answer, len(paths))
	var asked sync.WaitGroup
	for i, path := range paths {
		asked.Go(func() { answers[i].counts, answers[i].err = primitives.FilesystemStatistics(ctx, path) })
	}
	asked.Wait()
	failed, within := []Finding{}, 0
	failedPaths := map[string]string{}
	for _, path := range paths {
		failedPaths[filesystemFindingID(path)] = path
	}
	// A filesystem that failed in the run before and is now absent is not
	// recovered: it may be unmounted, and its failure may still be there.
	var failedBefore []string
	r.State.load(&failedBefore)
	answered := map[string]bool{}
	for i, path := range paths {
		counts, err := answers[i].counts, answers[i].err
		answered[path] = !errors.Is(err, fs.ErrNotExist)
		switch {
		case errors.Is(err, primitives.ErrFilesystemDoesNotAnswer):
			failed = append(failed, Finding{ID: filesystemFindingID(path), Message: err.Error()})
			continue
		case errors.Is(err, fs.ErrNotExist):
			// The filesystem went away since the kernel listed it; the
			// check below reports it if it failed before.
			continue
		case err != nil:
			// A filesystem whose other side is gone answers with an error,
			// as a network filesystem does when its server restarted.
			failed = append(failed, Finding{ID: filesystemFindingID(path), Message: primitives.OnOneLine(fmt.Sprintf("the filesystem at %s gives an error in place of its size: %s", path, err))})
			continue
		case counts.Blocks == 0:
			// The kernel gives this filesystem no size.
			continue
		}
		switch o := FilesystemObservation(limitsOnWhatWasMeasured(r.Check, counts), path, counts); o.Status {
		case "failed", "unknown":
			message := o.Message
			if !strings.HasPrefix(message, path+" ") && !strings.HasPrefix(message, path+":") {
				message = path + ": " + message
			}
			failed = append(failed, Finding{ID: filesystemFindingID(path), Message: message})
		case "ok":
			within++
		}
	}
	for _, path := range failedBefore {
		if id := filesystemFindingID(path); !answered[path] && !slices.Contains(ignored, path) {
			failedPaths[id] = path
			failed = append(failed, Finding{ID: id, Message: "the filesystem at " + path + " is no longer mounted"})
		}
	}
	failedNow := []string{}
	for _, finding := range failed {
		failedNow = append(failedNow, failedPaths[finding.ID])
	}
	r.State.Save(failedNow)
	for index := range failed {
		path := failedPaths[failed[index].ID]
		if failed[index].ID != path {
			failed[index].RecoveryMessage = filesystemIsOkAgain(path)
		}
	}
	slices.SortFunc(failed, func(a, b Finding) int { return strings.Compare(a.ID, b.ID) })
	o := Result("ok", fmt.Sprintf("%s within the limits", countOf(within, "filesystem is", "filesystems are")))
	if len(failed) > 0 {
		paths := []string{}
		for _, finding := range failed {
			paths = append(paths, failedPaths[finding.ID])
		}
		o = Result("failed", fmt.Sprintf("%s: %s", countOf(len(failed), "filesystem fails", "filesystems fail"), shownNames(paths)))
		o.Findings = failed
	}
	if len(ignored) > 0 {
		o.Message += fmt.Sprintf(" (not counting %d in filesystems_to_ignore: %s)", len(ignored), shownNames(ignored))
	}
	return o
}

// filesystemFindingID keeps a mount path when it fits the finding protocol.
// A longer path uses its SHA-256 checksum; the message still names the path.
func filesystemFindingID(path string) string {
	if ValidFindingID(path) {
		return path
	}
	return fmt.Sprintf("filesystem_path_sha256:%x", sha256.Sum256([]byte(path)))
}

// countOf writes a count with the words for one thing or for several.
func countOf(count int, one, several string) string {
	if count == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", count, several)
}

// limitsOnWhatWasMeasured returns check c without its limits on
// inodes_used_percent for a filesystem that has no fixed inode count, such
// as btrfs. One such filesystem among the others must not make the whole
// check unknown; its space still has its limits.
func limitsOnWhatWasMeasured(c Check, counts primitives.FilesystemCounts) Check {
	if counts.Inodes != 0 {
		return c
	}
	c.FailWhenMeasurementAbove, c.FailWhenMeasurementBelow = maps.Clone(c.FailWhenMeasurementAbove), maps.Clone(c.FailWhenMeasurementBelow)
	delete(c.FailWhenMeasurementAbove, "inodes_used_percent")
	delete(c.FailWhenMeasurementBelow, "inodes_used_percent")
	return c
}

// filesystemIsOkAgain is the message of the observation in which a
// filesystem no longer fails an every_disk_and_network_filesystem check.
func filesystemIsOkAgain(path string) string {
	return "the filesystem at " + path + " no longer fails this check"
}

// FilesystemMissingFromFilesystemsToIgnore is a filesystem that has its own
// node_filesystem check with a limit on the node and is missing from the
// filesystems_to_ignore of an every_disk_and_network_filesystem check, for mmwatchdog
// validate: one full filesystem would send two alerts.
type FilesystemMissingFromFilesystemsToIgnore struct {
	// CheckGroup and Check name the every_disk_and_network_filesystem check.
	CheckGroup string `json:"check_group"`
	Check      string `json:"check"`
	Filesystem string `json:"filesystem"`
	// NodeFilesystemCheck is the filesystem's own check, as CHECK_GROUP/CHECK.
	NodeFilesystemCheck string `json:"node_filesystem_check"`
}

// everyMountedFilesystemDescription is what "mmwatchdog describe-module every_disk_and_network_filesystem" shows, and what
// validation knows about the module's inputs.
var everyMountedFilesystemDescription = ModuleDescription{
	Summary:                   "Applies its limits to every filesystem of the node that is mounted from a disk device or from another machine. Each filesystem that passes a limit, is mounted read-only, or does not answer gets its own failure alert and its own recovery alert.",
	UsedAs:                    UsedAsCheck,
	OneIncidentForEachFinding: true,
	Inputs: []ModuleInput{
		{Name: "filesystems_to_ignore", Type: "list_of_strings", Required: true, Description: "The filesystems that the check leaves out, each by the path where it is mounted, such as /boot/efi. An empty list leaves none out."},
	},
	Measurements: []ModuleMeasurement{
		{Name: "space_used_percent", Unit: "percent", Description: "For each filesystem: the share of its space that is in use."},
		{Name: "inodes_used_percent", Unit: "percent", Description: "For each filesystem: the share of its inodes that is in use. A limit on it does not apply to a filesystem without a fixed number of inodes."},
		{Name: "space_free_gib", Unit: "GiB", Description: "For each filesystem: the space that an ordinary account can still fill."},
	},
	Examples: []ModuleExample{{
		Description:   "Alert for every filesystem that is more than 90 percent full, except the one at /boot/efi.",
		Configuration: "every_disk_and_network_filesystem:\n  filesystems_to_ignore: [/boot/efi]\nfail_when_measurement_above:\n  space_used_percent: 90\nalert_severity: warn\n",
	}},
}
