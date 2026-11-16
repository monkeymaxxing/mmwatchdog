package checksandalerts

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mmwatchdog/internal/primitives"
)

// OutOfMemoryKills is the inputs of the out_of_memory_kills check module.
// A run fails when the kernel killed a process for lack of memory since
// the previous run.
type OutOfMemoryKills struct {
	FailWhen string `yaml:"fail_when" json:"fail_when"`
}

func (n *OutOfMemoryKills) Validate(v InputValidation) *FieldError {
	inputs := map[string]any{}
	if n.FailWhen != "" || v.Authored != nil && v.Authored("fail_when") {
		inputs["fail_when"] = n.FailWhen
	}
	return outOfMemoryKillsDescription.InputsProblem("out_of_memory_kills", inputs)
}

// OutOfMemoryKillsSeen is what the check keeps between its runs: the
// kernel's count of these kills at the last run, the boot in which the
// kernel counted them, and the time of that run. The kernel starts the
// count at zero in each boot.
type OutOfMemoryKillsSeen struct {
	Kills  uint64    `json:"kills"`
	BootID string    `json:"boot_id"`
	At     time.Time `json:"at"`
}

func (n *OutOfMemoryKills) Run(ctx context.Context, r *CheckRun) Observation {
	data, err := primitives.ReadFile(primitives.ProcDirectory+"/vmstat", primitives.ProcFileLimit)
	if err != nil {
		return Result("unknown", err.Error())
	}
	kills, counted := outOfMemoryKillCount(string(data))
	if !counted {
		return Result("unknown", "the kernel of this machine does not count the processes that it ends to free memory (/proc/vmstat has no oom_kill)")
	}
	boot, err := primitives.ReadFile(primitives.ProcDirectory+"/sys/kernel/random/boot_id", primitives.ProcFileLimit)
	if err != nil {
		return Result("unknown", err.Error())
	}
	var seen OutOfMemoryKillsSeen
	r.State.load(&seen)
	now := time.Now().UTC()
	r.State.Save(OutOfMemoryKillsSeen{Kills: kills, BootID: strings.TrimSpace(string(boot)), At: now})
	if seen.At.IsZero() {
		// The first run only notes the count: a kill from before MMWatchdog watched
		// is no news.
		return Result("ok", "the kernel has ended no process to free memory since this check began to watch")
	}
	// In a new boot, every kill that the kernel has counted came after the
	// run before.
	since := kills
	if seen.BootID == strings.TrimSpace(string(boot)) && kills >= seen.Kills {
		since = kills - seen.Kills
	}
	if since == 0 {
		return Result("ok", "the kernel ended no process to free memory since the run before")
	}
	o := Result("failed", fmt.Sprintf("the kernel ended %s to free memory since the run before", countOf(int(since), "process", "processes")))
	// The kernel's log names each process. A second before the run before
	// allows for the two clocks.
	// The kernel writes one line for each process of the node before the
	// line that names the process that it ended, so the log of one kill can
	// be long. journalctl writes the message and the time of each entry and
	// no other field.
	ran, err := primitives.RunCommand(ctx, primitives.Command{Arguments: []string{"journalctl", "--no-pager", "--dmesg", "--output=json", "--all", "--output-fields=MESSAGE", "--since=@" + strconv.FormatInt(seen.At.Add(-time.Second).Unix(), 10)}, AsMMWatchdogItself: true, StandardOutputLimit: kernelLogReadLimit})
	if err == nil && ran.ExitCode != 0 {
		err = fmt.Errorf("exit status %d: %s", ran.ExitCode, primitives.Clip(strings.TrimSpace(ran.StandardError), 512))
	}
	ended := OutOfMemoryKillsInLog(ran.StandardOutput, now)
	if len(ended) > int(since) {
		ended = ended[len(ended)-int(since):]
	}
	switch {
	case len(ended) == 0 && err != nil:
		o.Details = []string{primitives.DetailLine("MMWatchdog could not read the names of the processes from the kernel's log: " + err.Error())}
	case len(ended) == 0:
		o.Details = []string{"the kernel's log does not name the processes"}
	case since == 1:
		o.Message = ended[0]
	default:
		o.Details = ended[:min(len(ended), primitives.DetailLinesLimit)]
	}
	return o
}

// kernelLogReadLimit is the most that the check reads of the kernel's log
// since the run before.
const kernelLogReadLimit = 16 * 1024 * 1024

// outOfMemoryKillCount reads the kernel's count of the processes that it
// ended to free memory from the text of /proc/vmstat.
func outOfMemoryKillCount(data string) (uint64, bool) {
	for _, line := range strings.Split(data, "\n") {
		if text, found := strings.CutPrefix(line, "oom_kill "); found {
			count, err := strconv.ParseUint(strings.TrimSpace(text), 10, 64)
			return count, err == nil
		}
	}
	return 0, false
}

// killedProcessPattern finds, in a line of the kernel's log, the process
// that the kernel ended to free memory: its ID, and its name in
// parentheses.
var killedProcessPattern = regexp.MustCompile(`Killed process (\d+) \((.+?)\)(?: total-vm|,|$)`)

// OutOfMemoryKillsInLog reads the entries of the kernel's log as
// "journalctl --output=json" writes them, one JSON object on each line, and
// returns one sentence for each process that the kernel ended to free
// memory, the oldest first. today decides whether a time carries its date.
func OutOfMemoryKillsInLog(logged string, today time.Time) []string {
	ended := []string{}
	for _, line := range strings.Split(logged, "\n") {
		var entry struct {
			Message   any    `json:"MESSAGE"`
			Timestamp string `json:"__REALTIME_TIMESTAMP"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		message, isText := entry.Message.(string)
		if !isText {
			continue
		}
		found := killedProcessPattern.FindStringSubmatch(message)
		if found == nil {
			continue
		}
		sentence := fmt.Sprintf("the kernel ended %s (process %s)", found[2], found[1])
		if microseconds, err := strconv.ParseInt(entry.Timestamp, 10, 64); err == nil {
			sentence += " at " + ClockTime(time.UnixMicro(microseconds).UTC(), today)
		}
		sentence += " to free memory"
		// A unit with a memory limit of its own can run out of memory
		// while the machine has plenty.
		if strings.Contains(message, "Memory cgroup out of memory") {
			sentence += ", because its control group reached its memory limit"
		}
		ended = append(ended, primitives.DetailLine(sentence))
	}
	return ended
}

// outOfMemoryKillsDescription is what "mmwatchdog describe-module out_of_memory_kills" shows, and what
// validation knows about the module's inputs.
var outOfMemoryKillsDescription = ModuleDescription{
	Summary: "Fails in a run when the kernel ended at least one process to free memory since the check's previous run. The first run that sees a new kill opens its incident; the alert names each ended command.",
	UsedAs:  UsedAsCheck,
	Inputs: []ModuleInput{
		{Name: "fail_when", Type: "string", Required: true, AllowedValues: []any{"the_kernel_kills_a_process_for_lack_of_memory"}, Description: "The check fails when the kernel kills a process for lack of memory; write the_kernel_kills_a_process_for_lack_of_memory, the only value."},
	},
	Examples: []ModuleExample{{
		Description:   "Alert when the kernel ended a process because memory ran out.",
		Configuration: "out_of_memory_kills:\n  fail_when: the_kernel_kills_a_process_for_lack_of_memory\nalert_severity: warn\n",
	}},
}
