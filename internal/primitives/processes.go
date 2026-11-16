package primitives

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// What MMWatchdog reads about the processes of the node from /proc: for the five
// commands in the alert of a node_resource check, and for the commands of
// one systemd unit.

// ProcessReading is what the kernel says about one process in
// /proc/PROCESS_ID/stat.
type ProcessReading struct {
	ID int
	// StartTicks is when the process started, in the kernel's clock ticks
	// since boot. A process ID is given out again after its process ended,
	// so the ID and the start together name one process.
	StartTicks uint64
	// CPUTicks is the processor time that the process has used, in the
	// units of /proc/stat.
	CPUTicks uint64
	// ResidentBytes is the memory of the process that is in physical
	// memory now.
	ResidentBytes uint64
}

// ProcessKey names one process across two readings.
type ProcessKey struct {
	ID         int
	StartTicks uint64
}

// nodeProcesses reads every process of the node. A process that ends while
// MMWatchdog reads is left out.
func nodeProcesses() ([]ProcessReading, error) {
	names, e := ListDirectory(ProcDirectory)
	if e != nil {
		return nil, e
	}
	processes := []ProcessReading{}
	for _, name := range names {
		id, e := strconv.Atoi(name)
		if e != nil {
			continue
		}
		data, e := ReadFile(fmt.Sprintf("%s/%d/stat", ProcDirectory, id), ProcFileLimit)
		if e != nil {
			continue
		}
		if reading, valid := ParseProcessStat(id, string(data)); valid {
			processes = append(processes, reading)
		}
	}
	return processes, nil
}

// ParseProcessStat reads the text of /proc/PROCESS_ID/stat. The second field
// is the name of the program in parentheses, and that name can hold spaces
// and parentheses of its own, so the fields are counted from the last
// closing parenthesis: the state is field 3, the processor time in user and
// in system mode fields 14 and 15, the start field 22, and the pages in
// physical memory field 24.
func ParseProcessStat(id int, data string) (ProcessReading, bool) {
	end := strings.LastIndexByte(data, ')')
	if end < 0 {
		return ProcessReading{}, false
	}
	fields := strings.Fields(data[end+1:])
	// fields[0] is field 3.
	if len(fields) < 22 {
		return ProcessReading{}, false
	}
	number := func(field int) (uint64, bool) {
		value, e := strconv.ParseUint(fields[field-3], 10, 64)
		return value, e == nil
	}
	user, validUser := number(14)
	system, validSystem := number(15)
	start, validStart := number(22)
	pages, validPages := number(24)
	if !validUser || !validSystem || !validStart || !validPages {
		return ProcessReading{}, false
	}
	return ProcessReading{ID: id, StartTicks: start, CPUTicks: user + system, ResidentBytes: pages * uint64(os.Getpagesize())}, true
}

// commandOfProcess returns the command line of a process, as "systemctl
// status" shows it. A process without one, such as a thread of the kernel,
// has its name in brackets. A process that ended has "".
func commandOfProcess(id int) string {
	if data, e := ReadFile(fmt.Sprintf("%s/%d/cmdline", ProcDirectory, id), ProcFileLimit); e == nil {
		// The kernel separates the arguments with a zero byte.
		if command := strings.TrimSpace(strings.ReplaceAll(strings.TrimRight(string(data), "\x00"), "\x00", " ")); command != "" {
			return command
		}
	}
	if data, e := ReadFile(fmt.Sprintf("%s/%d/comm", ProcDirectory, id), ProcFileLimit); e == nil {
		if name := strings.TrimSpace(string(data)); name != "" {
			return "[" + name + "]"
		}
	}
	return ""
}

// commandsShown is how many commands the alert of a node_resource check
// names.
const commandsShown = 5

// processUse is how much of one resource a process uses.
type processUse struct {
	id     int
	amount float64
}

// commandLines writes the details of an observation that name the commands
// which use the most of a resource: a heading, and one line for each of the
// commandsShown processes with the highest amount, the highest first. amount
// writes one amount with its unit, such as "3051 MiB". A process that uses
// nothing is left out, and so is one that ended in the meantime. Without a
// process to name, there are no lines.
func commandLines(heading string, uses []processUse, amount func(float64) string) []string {
	slices.SortFunc(uses, func(a, b processUse) int {
		return cmp.Or(cmp.Compare(b.amount, a.amount), cmp.Compare(a.id, b.id))
	})
	lines := []string{}
	for _, use := range uses {
		if len(lines) == commandsShown || use.amount <= 0 {
			break
		}
		if command := commandOfProcess(use.id); command != "" {
			lines = append(lines, "  "+DetailLine(fmt.Sprintf("%s: %s (process %d)", amount(use.amount), Clip(command, 120), use.id)))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return append([]string{heading}, lines...)
}

// mebibytes writes a number of bytes in MiB for a detail line.
func mebibytes(bytes float64) string { return fmt.Sprintf("%.0f MiB", bytes/(1024*1024)) }

// CommandsThatUseTheMostMemory names the processes with the most memory in
// physical memory.
func CommandsThatUseTheMostMemory() []string {
	processes, e := nodeProcesses()
	if e != nil {
		return nil
	}
	uses := []processUse{}
	for _, p := range processes {
		uses = append(uses, processUse{p.ID, float64(p.ResidentBytes)})
	}
	return commandLines("the commands that use the most memory:", uses, mebibytes)
}

// CommandsThatUseTheMostSwap names the processes with the most memory in
// swap space, which the kernel gives as VmSwap in /proc/PROCESS_ID/status.
func CommandsThatUseTheMostSwap() []string {
	processes, e := nodeProcesses()
	if e != nil {
		return nil
	}
	uses := []processUse{}
	for _, p := range processes {
		data, e := ReadFile(fmt.Sprintf("%s/%d/status", ProcDirectory, p.ID), ProcFileLimit)
		if e != nil {
			continue
		}
		if values, e := Meminfo(string(data), "VmSwap:"); e == nil {
			uses = append(uses, processUse{p.ID, float64(values["VmSwap:"]) * 1024})
		}
	}
	return commandLines("the commands that use the most swap space:", uses, mebibytes)
}

// CommandsThatAddTheMostToTheLoad names the processes with the most threads
// that run or wait for a disk at this moment. The kernel's load average
// counts exactly those threads, over a longer time.
func CommandsThatAddTheMostToTheLoad() []string {
	processes, e := nodeProcesses()
	if e != nil {
		return nil
	}
	uses := []processUse{}
	for _, p := range processes {
		threads, e := ListDirectory(fmt.Sprintf("%s/%d/task", ProcDirectory, p.ID))
		if e != nil {
			continue
		}
		busy := 0
		for _, thread := range threads {
			data, e := ReadFile(fmt.Sprintf("%s/%d/task/%s/stat", ProcDirectory, p.ID, thread), ProcFileLimit)
			if e != nil {
				continue
			}
			// The state is the first field after the name: R runs or waits
			// for a processor, and D waits for a disk.
			if end := strings.LastIndexByte(string(data), ')'); end >= 0 {
				if state := strings.TrimSpace(string(data[end+1:])); strings.HasPrefix(state, "R") || strings.HasPrefix(state, "D") {
					busy++
				}
			}
		}
		uses = append(uses, processUse{p.ID, float64(busy)})
	}
	return commandLines("the commands with the most threads that run or wait for a disk now:", uses, func(threads float64) string {
		if threads == 1 {
			return "1 thread"
		}
		return fmt.Sprintf("%.0f threads", threads)
	})
}

// ProcessCPUTicks reads the processor time that each process of the node
// has used.
func ProcessCPUTicks() map[ProcessKey]uint64 {
	processes, e := nodeProcesses()
	if e != nil {
		return nil
	}
	ticks := map[ProcessKey]uint64{}
	for _, p := range processes {
		ticks[ProcessKey{p.ID, p.StartTicks}] = p.CPUTicks
	}
	return ticks
}

// CommandsThatUsedTheMostCPU names the processes that used the most
// processor time between two readings of the node's CPU, each as a percent
// of the processor time of the whole node in that time, like
// cpu_used_percent itself. A process that started after the first reading
// counts with all that it has used.
func CommandsThatUsedTheMostCPU(previous, current CPUReading) []string {
	var total float64
	for i := range current.Ticks {
		if current.Ticks[i] > previous.Ticks[i] {
			total += float64(current.Ticks[i] - previous.Ticks[i])
		}
	}
	if total == 0 || current.Processes == nil || previous.Processes == nil {
		return nil
	}
	uses := []processUse{}
	for process, ticks := range current.Processes {
		if before := previous.Processes[process]; ticks > before {
			uses = append(uses, processUse{process.ID, 100 * float64(ticks-before) / total})
		}
	}
	return commandLines("the commands that used the most CPU between the two CPU readings:", uses, func(percent float64) string {
		return FormatReading(percent) + " percent"
	})
}

// UnitProcess is one process that a systemd unit runs.
type UnitProcess struct {
	ID      int
	Command string
}

// ErrUnitDoesNotRun reports a systemd unit that runs no process because it
// does not run (UnitRuns), with the state that systemd reports.
type ErrUnitDoesNotRun struct{ unit, activeState string }

func (e ErrUnitDoesNotRun) Error() string {
	return "systemd unit " + e.unit + " is " + e.activeState
}

// ProcessesOfSystemdUnit returns the processes that a systemd unit runs: the
// ones that "systemctl status UNIT" lists. systemd keeps the processes of a
// unit in one control group of the kernel, and the kernel lists the process
// IDs of a control group, and of each control group inside it, in the file
// cgroup.procs. A unit that systemd does not know is ErrSystemdUnitNotFound,
// and a unit that does not run is ErrUnitDoesNotRun. A unit that runs can
// run no process at all: a unit whose program has ended and that systemd
// keeps active (RemainAfterExit) has no control group, and its list is
// empty.
func ProcessesOfSystemdUnit(ctx context.Context, unit string) ([]UnitProcess, error) {
	fields, e := ServiceProperties(ctx, unit, "ActiveState", "ControlGroup")
	if e != nil {
		return nil, e
	}
	if state := fields["ActiveState"]; !UnitRuns(state) {
		return nil, ErrUnitDoesNotRun{unit, state}
	}
	group := fields["ControlGroup"]
	if group == "" {
		return []UnitProcess{}, nil
	}
	if strings.Contains(group, "..") {
		return nil, fmt.Errorf("systemd gave a control group for %s that MMWatchdog does not read: %s", unit, group)
	}
	// With one tree of control groups, the kernel shows it at
	// /sys/fs/cgroup. An older arrangement keeps systemd's own tree in the
	// directory unified or systemd below it.
	for _, tree := range []string{"", "/unified", "/systemd"} {
		directory := SysDirectory + "/fs/cgroup" + tree + group
		if _, err := InformationAboutFile(directory); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		ids, err := processIDsOfControlGroup(directory)
		if err != nil {
			return nil, err
		}
		slices.Sort(ids)
		processes := []UnitProcess{}
		for _, id := range slices.Compact(ids) {
			command := commandOfProcess(id)
			if command == "" {
				// A process can end between reading its ID and its command.
				if _, err := InformationAboutFile(fmt.Sprintf("%s/%d", ProcDirectory, id)); os.IsNotExist(err) {
					continue
				}
				return nil, fmt.Errorf("MMWatchdog cannot read the command of process %d of %s", id, unit)
			}
			processes = append(processes, UnitProcess{id, command})
		}
		return processes, nil
	}
	return nil, fmt.Errorf("MMWatchdog cannot read the control group of %s below %s/fs/cgroup", unit, SysDirectory)
}

// processIDsOfControlGroup reads every nested control group. A partial
// list is not a process count, so every read error is returned.
func processIDsOfControlGroup(directory string) ([]int, error) {
	data, err := ReadFile(directory+"/cgroup.procs", ProcFileLimit)
	if err != nil {
		return nil, err
	}
	ids := []int{}
	for _, field := range strings.Fields(string(data)) {
		id, err := strconv.Atoi(field)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("%s/cgroup.procs contains an invalid process ID: %q", directory, field)
		}
		ids = append(ids, id)
	}
	names, err := ListDirectory(directory)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		path := directory + "/" + name
		about, err := InformationAboutFile(path)
		if err != nil {
			return nil, err
		}
		if about.IsDirectory {
			children, err := processIDsOfControlGroup(path)
			if err != nil {
				return nil, err
			}
			ids = append(ids, children...)
		}
	}
	return ids, nil
}

type CPUReading struct {
	Ticks      [8]uint64
	Processors int
	Usage      uint64
	Invocation string
	At         time.Time
	// Processes holds the processor time that each process of the node has
	// used, in a reading that asked for it: see CommandsThatUsedTheMostCPU.
	Processes map[ProcessKey]uint64
}

// Meminfo reads the named kB fields from /proc/meminfo text; each must be present.
func Meminfo(data string, names ...string) (map[string]uint64, error) {
	values := map[string]uint64{}
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !slices.Contains(names, fields[0]) {
			continue
		}
		if len(fields) != 3 || fields[2] != "kB" {
			return nil, fmt.Errorf("/proc/meminfo: invalid %s", fields[0])
		}
		n, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("/proc/meminfo: invalid %s", fields[0])
		}
		values[fields[0]] = n
	}
	for _, name := range names {
		if _, ok := values[name]; !ok {
			return nil, fmt.Errorf("/proc/meminfo: %s unavailable", strings.TrimSuffix(name, ":"))
		}
	}
	return values, nil
}

// FormatReading writes a reading for a message: at most two decimals,
// without trailing zeros.
func FormatReading(value float64) string {
	return strconv.FormatFloat(math.Round(value*100)/100, 'f', -1, 64)
}
