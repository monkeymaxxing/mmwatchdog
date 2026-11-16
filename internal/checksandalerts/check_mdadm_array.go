package checksandalerts

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"mmwatchdog/internal/primitives"
)

// MdadmArray is the inputs of the mdadm_array check module: the path of one
// software RAID array of Linux, the kind that the mdadm program manages,
// such as /dev/md0. The check fails while the array misses a device, and
// while the array is not there at all: an array that was not put together
// at the start of the machine is the worst case, and it must not look
// healthy.
type MdadmArray string

func (m *MdadmArray) Validate(InputValidation) *FieldError {
	if path := string(*m); !strings.HasPrefix(path, "/dev/") || filepath.Clean(path) != path {
		return &FieldError{"", "must be the path of a software RAID array, such as /dev/md0"}
	}
	return nil
}

// mdadmArrayStatesThatWork are the states in which the kernel has an array
// in use or ready for use.
var mdadmArrayStatesThatWork = []string{"clean", "active", "active-idle", "write-pending", "readonly", "read-auto"}

func (m *MdadmArray) Run(context.Context, *CheckRun) Observation {
	path := string(*m)
	// The kernel shows an array under its own name, such as md0, also when
	// the path is a link to it, such as /dev/md/data.
	resolved, err := primitives.ResolvePath(primitives.NodeFile(path))
	if err != nil {
		return Result("failed", "MMWatchdog cannot resolve the device path "+path+": "+err.Error())
	}
	name := filepath.Base(resolved)
	directory := primitives.SysDirectory + "/block/" + name + "/md"
	read := func(file string) string {
		data, err := primitives.ReadFile(directory+"/"+file, primitives.ProcFileLimit)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(data))
	}
	data, err := primitives.ReadFile(directory+"/array_state", primitives.ProcFileLimit)
	if errors.Is(err, fs.ErrNotExist) {
		return Result("failed", path+" is not an assembled software RAID array on the node")
	}
	if err != nil {
		return Result("unknown", fmt.Sprintf("MMWatchdog cannot read the array state of %s: %s", path, err))
	}
	if strings.TrimSpace(string(data)) == "" {
		return Result("unknown", "the kernel returned no array state for "+path+" in "+directory+"/array_state")
	}
	state := strings.TrimSpace(string(data))
	if !slices.Contains(mdadmArrayStatesThatWork, state) {
		return Result("failed", path+" does not work: the kernel reports the array as "+state)
	}
	// The devices of the array, and what the kernel says about each.
	working, faulty := 0, []string{}
	entries, _ := primitives.ListDirectory(directory)
	for _, entry := range entries {
		device, isDevice := strings.CutPrefix(entry, "dev-")
		if !isDevice {
			continue
		}
		switch flags := strings.Split(read(entry+"/state"), ","); {
		case slices.Contains(flags, "faulty"):
			faulty = append(faulty, device)
		case slices.Contains(flags, "in_sync"):
			working++
		}
	}
	expected, _ := strconv.Atoi(read("raid_disks"))
	of := fmt.Sprintf("%d of %d devices work", working, expected)
	if expected == 1 {
		of = fmt.Sprintf("%d of 1 device works", working)
	}
	// The kernel says how far a rebuild or a check of the array is, as the
	// sectors that are done and the sectors in all.
	progress := ""
	if done, all, found := strings.Cut(read("sync_completed"), " / "); found {
		if d, err := strconv.ParseFloat(done, 64); err == nil {
			if a, err := strconv.ParseFloat(all, 64); err == nil && a > 0 {
				progress = fmt.Sprintf(", %.0f percent done", 100*d/a)
			}
		}
	}
	action := read("sync_action")
	data, err = primitives.ReadFile(directory+"/degraded", primitives.ProcFileLimit)
	if err != nil {
		return Result("unknown", fmt.Sprintf("MMWatchdog cannot read the degraded device count of %s: %s", path, err))
	}
	missing, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || missing < 0 {
		return Result("unknown", "the kernel returned no valid degraded device count for "+path+" in "+directory+"/degraded")
	}
	if missing > 0 {
		message := fmt.Sprintf("%s (%s) is degraded: %s", path, read("level"), of)
		if len(faulty) > 0 {
			message += "; the kernel reports as faulty: " + strings.Join(faulty, ", ")
		}
		if action == "recover" {
			message += "; the array is being rebuilt" + progress
		}
		return Result("failed", message)
	}
	message := fmt.Sprintf("%s (%s) has all expected devices: %s", path, read("level"), of)
	if action != "" && action != "idle" {
		message += "; the kernel runs a " + action + " of the array" + progress
	}
	return Result("ok", message)
}

// mdadmArrayDescription is what "mmwatchdog describe-module mdadm_array" shows, and what
// validation knows about the module's inputs.
var mdadmArrayDescription = ModuleDescription{
	Summary: "Watches one software RAID array of Linux, the kind that the mdadm program manages. The check fails while the array misses a device, and while the node has not put the array together.",
	UsedAs:  UsedAsCheck,
	Value:   &ModuleInput{Type: "string", Required: true, Description: "The path of the array, such as /dev/md0."},
	Examples: []ModuleExample{{
		Description:   "The array that holds the data has all its devices.",
		Configuration: "mdadm_array: /dev/md0\nalert_severity: sos\n",
	}},
}
