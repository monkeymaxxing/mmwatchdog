package primitives_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/testhelpers"
)

// processStat is the text of /proc/PROCESS_ID/stat for a process with a
// name, a processor time in user and in system mode, a start, and a number
// of pages in physical memory.
func processStat(id int, name string, userTicks, systemTicks, startTicks, pages uint64) string {
	return fmt.Sprintf("%d (%s) S 1 %d %d 0 -1 4194560 100 0 0 0 %d %d 0 0 20 0 1 0 %d 1000000 %d 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0\n",
		id, name, id, id, userTicks, systemTicks, startTicks, pages)
}

// The name of a program can hold spaces and parentheses, so the fields of
// /proc/PROCESS_ID/stat are counted from the last closing parenthesis.
func TestTheStatFileOfAProcessIsReadFromItsLastParenthesis(t *testing.T) {
	t.Parallel()
	pageBytes := uint64(os.Getpagesize())
	for name, want := range map[string]primitives.ProcessReading{
		"mysqld":            {ID: 7, StartTicks: 500, CPUTicks: 30, ResidentBytes: 10 * pageBytes},
		"tmux: server (1)":  {ID: 7, StartTicks: 500, CPUTicks: 30, ResidentBytes: 10 * pageBytes},
		"a) R 1 2 3 (still": {ID: 7, StartTicks: 500, CPUTicks: 30, ResidentBytes: 10 * pageBytes},
	} {
		if got, valid := primitives.ParseProcessStat(7, processStat(7, name, 10, 20, 500, 10)); !valid || got != want {
			t.Errorf("%q: %+v %v", name, got, valid)
		}
	}
	for _, text := range []string{"", "7 (short) S 1 2 3", "7 no parenthesis S 1 2", strings.Replace(processStat(7, "x", 10, 20, 500, 10), " 10 20 ", " ten 20 ", 1)} {
		if got, valid := primitives.ParseProcessStat(7, text); valid {
			t.Errorf("%q: read %+v", text, got)
		}
	}
}

// The alert of a node_resource check names the five commands that use the
// most of its resource, the highest first, by program name and process ID
// only: the arguments of another account's program, such as a password, never
// reach an alert. A process without a command line has its name in brackets,
// and a process that uses nothing is left out.
func TestTheCommandsThatUseTheMostOfAResource(t *testing.T) {
	megabyte := uint64(1024 * 1024 / os.Getpagesize())
	files := map[string]string{"proc/uptime": "1 1\n", "proc/not-a-process/stat": "x"}
	for id, pages := range map[int]uint64{1: 10, 2: 300, 3: 50, 4: 200, 5: 40, 6: 100, 7: 0} {
		files[fmt.Sprintf("proc/%d/stat", id)] = processStat(id, "program", 1, 1, 100, pages*megabyte)
		files[fmt.Sprintf("proc/%d/cmdline", id)] = fmt.Sprintf("/usr/bin/program-%d\x00--flag\x00", id)
		files[fmt.Sprintf("proc/%d/status", id)] = fmt.Sprintf("Name:\tprogram\nVmSwap:\t%8d kB\n", uint64(id)*1024)
	}
	// Process 2 has a password among its arguments, and process 4 has no
	// command line, as a thread of the kernel.
	files["proc/2/cmdline"] = "/usr/bin/mysql\x00-pS3cretPassw0rd\x00"
	files["proc/4/cmdline"] = ""
	// Process 5 rewrote its first argument to a whole line, as nginx does.
	files["proc/5/cmdline"] = "nginx: worker process\x00"
	files["proc/4/comm"] = "kworker/0:1\n"
	testhelpers.KernelFiles(t, files)
	if got, want := primitives.CommandsThatUseTheMostMemory(), []string{
		"the commands that use the most memory:",
		"  300 MiB: mysql (process 2)",
		"  200 MiB: [kworker/0:1] (process 4)",
		"  100 MiB: program-6 (process 6)",
		"  50 MiB: program-3 (process 3)",
		"  40 MiB: nginx (process 5)",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("%q", got)
	}
	if got := primitives.CommandsThatUseTheMostSwap(); len(got) != 6 || got[0] != "the commands that use the most swap space:" || got[1] != "  7 MiB: program-7 (process 7)" || got[5] != "  3 MiB: program-3 (process 3)" {
		t.Fatalf("%q", got)
	}
	// Threads that run or wait for a disk count for the load. Process 3 has
	// two such threads of three, and process 5 one.
	root := filepath.Dir(primitives.ProcDirectory)
	for path, state := range map[string]string{"proc/3/task/3/stat": "R", "proc/3/task/31/stat": "D", "proc/3/task/32/stat": "S", "proc/5/task/5/stat": "R", "proc/6/task/6/stat": "S"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte("3 (program) "+state+" 1 3 3 0\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := primitives.CommandsThatAddTheMostToTheLoad(), []string{
		"the commands with the most threads that run or wait for a disk now:",
		"  2 threads: program-3 (process 3)",
		"  1 thread: nginx (process 5)",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("%q", got)
	}
	// A node on which no process uses the resource names none.
	testhelpers.KernelFiles(t, map[string]string{"proc/1/stat": processStat(1, "idle", 0, 0, 1, 0), "proc/1/cmdline": "idle\x00"})
	if got := primitives.CommandsThatUseTheMostMemory(); got != nil {
		t.Fatalf("%q", got)
	}
}

// The commands that used the most CPU are measured between the two readings
// of the node's CPU, as a percent of the processor time of the whole node.
// A process ID that was given out again is a new process.
func TestTheCommandsThatUsedTheMostCPU(t *testing.T) {
	testhelpers.KernelFiles(t, map[string]string{"proc/10/cmdline": "ffmpeg\x00-i\x00in.mp4\x00", "proc/11/cmdline": "sleep\x001\x00", "proc/12/cmdline": "new-process\x00", "proc/13/cmdline": "quiet\x00"})
	previous := primitives.CPUReading{Ticks: [8]uint64{100, 0, 100, 800}, Processes: map[primitives.ProcessKey]uint64{{ID: 10, StartTicks: 5}: 50, {ID: 11, StartTicks: 5}: 20, {ID: 12, StartTicks: 5}: 1000, {ID: 13, StartTicks: 5}: 7}}
	// 400 ticks passed on the node. Process 10 used 100 of them and process
	// 11 used 2. Process 12 started anew, with 40 ticks in all. Process 13
	// used nothing.
	current := primitives.CPUReading{Ticks: [8]uint64{300, 0, 200, 900}, Processes: map[primitives.ProcessKey]uint64{{ID: 10, StartTicks: 5}: 150, {ID: 11, StartTicks: 5}: 22, {ID: 12, StartTicks: 9}: 40, {ID: 13, StartTicks: 5}: 7}}
	if got, want := primitives.CommandsThatUsedTheMostCPU(previous, current), []string{
		"the commands that used the most CPU between the two CPU readings:",
		"  25 percent: ffmpeg (process 10)",
		"  10 percent: new-process (process 12)",
		"  0.5 percent: sleep (process 11)",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("%q", got)
	}
	// Readings without the processes, as a check without a limit takes
	// them, name no command.
	if got := primitives.CommandsThatUsedTheMostCPU(primitives.CPUReading{Ticks: previous.Ticks}, primitives.CPUReading{Ticks: current.Ticks}); got != nil {
		t.Fatalf("%q", got)
	}
}

// A node_resource check that fails names the commands in the details of its
// observation, and a check that passes its limit does not read them.
func TestAFailedNodeResourceCheckNamesTheCommands(t *testing.T) {
	megabyte := uint64(1024 * 1024 / os.Getpagesize())
	testhelpers.KernelFiles(t, map[string]string{
		"proc/meminfo":  "MemTotal: 1000 kB\nMemAvailable: 100 kB\nSwapTotal: 1000 kB\nSwapFree: 200 kB\n",
		"proc/loadavg":  "8.00 8.00 8.00 3/200 4000\n",
		"proc/stat":     "cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 100 0 100 800 0 0 0 0 0 0\n",
		"proc/1/stat":   processStat(1, "mysqld", 5, 5, 100, 64*megabyte),
		"proc/1/status": "VmSwap:\t2048 kB\n", "proc/1/cmdline": "/usr/sbin/mysqld\x00",
		"proc/1/task/1/stat": "1 (mysqld) R 1 1 1\n",
	})
	for resource, want := range map[string][]string{
		"memory": {"the commands that use the most memory:", "  64 MiB: mysqld (process 1)"},
		"swap":   {"the commands that use the most swap space:", "  2 MiB: mysqld (process 1)"},
		"load":   {"the commands with the most threads that run or wait for a disk now:", "  1 thread: mysqld (process 1)"},
	} {
		measurement := checksandalerts.NodeResourceMeasurements[resource][0]
		check := checksandalerts.Check{Inputs: new(checksandalerts.NodeResource(resource)), FailWhenMeasurementAbove: map[string]float64{measurement: 1}}
		check.AlertSeverity = "warn"
		if o := checksandalerts.ObserveCheck(context.Background(), check); o.Status != "failed" || !reflect.DeepEqual(o.Details, want) {
			t.Errorf("%s: %s %q %q", resource, o.Status, o.Message, o.Details)
		}
		check.FailWhenMeasurementAbove = map[string]float64{measurement: 100}
		if o := checksandalerts.ObserveCheck(context.Background(), check); o.Status != "ok" || o.Details != nil {
			t.Errorf("%s within its limit: %s %q %q", resource, o.Status, o.Message, o.Details)
		}
	}
}

// A CPU check that can fail reads the processes with each reading of the
// CPU, so the commands cover the same time as the reading. A check that
// only records does not read them.
func TestAFailedCPUCheckNamesTheCommandsOfItsSampleWindow(t *testing.T) {
	root := testhelpers.KernelFiles(t, map[string]string{
		"proc/stat":   "cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 100 0 100 800 0 0 0 0 0 0\n",
		"proc/1/stat": processStat(1, "ffmpeg", 10, 0, 100, 1), "proc/1/cmdline": "ffmpeg\x00",
	})
	// advance lets the node's busy time and the time of the process grow
	// between two runs of a check.
	advance := func(busyTicks, processTicks int) {
		for name, content := range map[string]string{"proc/stat": fmt.Sprintf("cpu  %[1]d 0 100 800 0 0 0 0 0 0\ncpu0 %[1]d 0 100 800 0 0 0 0 0 0\n", busyTicks), "proc/1/stat": processStat(1, "ffmpeg", uint64(processTicks), 0, 100, 1)} {
			if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The first run of a check takes a sample of its own, over which the
	// counters do not move here: it measures nothing, and keeps its second
	// reading, with the processes, for the next run.
	check := checksandalerts.Check{Inputs: new(checksandalerts.NodeResource("cpu")), FailWhenMeasurementAbove: map[string]float64{"cpu_used_percent": 50}}
	check.AlertSeverity = "warn"
	sampler := &checksandalerts.CPUSampler{}
	run := checksandalerts.NewCheckRun(check, checksandalerts.Timing{})
	run.Memory.Value = sampler
	if o := checksandalerts.RunCheck(context.Background(), run); o.Status != "unknown" || sampler.Previous == nil || sampler.Previous.Processes == nil {
		t.Fatalf("%s %q %+v", o.Status, o.Message, sampler.Previous)
	}
	// 200 ticks pass, all of them busy, and the process uses 100 of them.
	advance(300, 110)
	o := checksandalerts.RunCheck(context.Background(), run)
	if want := []string{"the commands that used the most CPU between the two CPU readings:", "  50 percent: ffmpeg (process 1)"}; o.Status != "failed" || o.Measurements["cpu_used_percent"].Value != 100 || !reflect.DeepEqual(o.Details, want) {
		t.Fatalf("%s %q %q", o.Status, o.Message, o.Details)
	}
	sampler = &checksandalerts.CPUSampler{}
	run = checksandalerts.NewCheckRun(checksandalerts.Check{Inputs: new(checksandalerts.NodeResource("cpu"))}, checksandalerts.Timing{})
	run.Memory.Value = sampler
	if o := checksandalerts.RunCheck(context.Background(), run); o.Status != "unknown" || sampler.Previous == nil || sampler.Previous.Processes != nil {
		t.Fatalf("%s %q %+v", o.Status, o.Message, sampler.Previous)
	}
	advance(500, 210)
	if o := checksandalerts.RunCheck(context.Background(), run); o.Status != "ok" || sampler.Previous.Processes != nil {
		t.Fatalf("%s %q %v", o.Status, o.Message, sampler.Previous.Processes)
	}
}

// The processes of a systemd unit are the ones in its control group and in
// the control groups inside it, which is what "systemctl status" lists.
func TestTheProcessesOfASystemdUnit(t *testing.T) {
	group := "sys/fs/cgroup/system.slice/myapp.service/"
	testhelpers.KernelFiles(t, map[string]string{
		group + "cgroup.procs": "1201\n1215\n", group + "workers/cgroup.procs": "1300\n", group + "workers/empty/cgroup.procs": "",
		"proc/1201/cmdline": "/usr/local/bin/myapp-web\x00--port\x008080\x00", "proc/1215/cmdline": "/usr/local/bin/myapp-worker\x00--queue\x00mail\x00",
		"proc/1300/cmdline": "", "proc/1300/comm": "helper\n",
		// A process of another unit.
		"sys/fs/cgroup/system.slice/other.service/cgroup.procs": "99\n", "proc/99/cmdline": "myapp-worker\x00",
	})
	shown := testhelpers.FakeUnit(t, "LoadState=loaded\nActiveState=active\nControlGroup=/system.slice/myapp.service\n")
	processes, err := primitives.ProcessesOfSystemdUnit(testhelpers.TestContext(t), "myapp.service")
	if want := []primitives.UnitProcess{{ID: 1201, Command: "/usr/local/bin/myapp-web --port 8080"}, {ID: 1215, Command: "/usr/local/bin/myapp-worker --queue mail"}, {ID: 1300, Command: "[helper]"}}; err != nil || !reflect.DeepEqual(processes, want) {
		t.Fatal(processes, err)
	}

	// systemd_unit_resource counts them.
	count := checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitResource{SystemdUnit: "myapp.service", Resource: "processes"}, FailWhenMeasurementAbove: map[string]float64{"process_count": 2}}
	count.AlertSeverity = "warn"
	if o := checksandalerts.ObserveCheck(testhelpers.TestContext(t), count); o.Status != "failed" || o.Message != "process_count is 3 (fails above 2)" || o.Measurements["process_count"] != (checksandalerts.Measurement{Value: 3, Unit: "processes"}) {
		t.Fatal(o)
	}

	// systemd_unit_commands looks for one of them.
	runs := func(text string) checksandalerts.Observation {
		return checksandalerts.ObserveCheck(testhelpers.TestContext(t), checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitCommands{SystemdUnit: "myapp.service", FailWhenNoCommandContains: text}})
	}
	if o := runs("myapp-worker"); o.Status != "ok" || o.Message != "systemd unit myapp.service runs /usr/local/bin/myapp-worker --queue mail (process 1215)" {
		t.Fatal(o)
	}
	if o := runs("--port 8080"); o.Status != "ok" {
		t.Fatal(o)
	}
	o := runs("myapp-mailer")
	if want := []string{"the commands that the unit runs:", "  /usr/local/bin/myapp-web --port 8080 (process 1201)", "  /usr/local/bin/myapp-worker --queue mail (process 1215)", "  [helper] (process 1300)"}; o.Status != "failed" ||
		o.Message != "systemd unit myapp.service runs no command that contains myapp-mailer" || !reflect.DeepEqual(o.Details, want) {
		t.Fatal(o)
	}

	// A reloading unit runs: MMWatchdog reads its commands.
	os.WriteFile(shown, []byte("LoadState=loaded\nActiveState=reloading\nControlGroup=/system.slice/myapp.service\n"), 0644)
	if o := runs("myapp-worker"); o.Status != "ok" {
		t.Fatal(o)
	}
	if o := checksandalerts.ObserveCheck(testhelpers.TestContext(t), count); o.Status != "failed" || o.Measurements["process_count"] != (checksandalerts.Measurement{Value: 3, Unit: "processes"}) {
		t.Fatal(o)
	}
	// A unit that does not run, such as a stopped one or one that systemd
	// is stopping, runs no command, so the check of a command fails; its
	// number of processes cannot be measured.
	for _, state := range []string{"inactive", "deactivating"} {
		os.WriteFile(shown, []byte("LoadState=loaded\nActiveState="+state+"\nControlGroup=/system.slice/myapp.service\n"), 0644)
		if o := runs("myapp-worker"); o.Status != "failed" || o.Message != "systemd unit myapp.service runs no command that contains myapp-worker: systemd unit myapp.service is "+state {
			t.Fatal(o)
		}
		if o := checksandalerts.ObserveCheck(testhelpers.TestContext(t), count); o.Status != "unknown" || o.Message != "systemd unit myapp.service process count unavailable: systemd unit myapp.service is "+state {
			t.Fatal(o)
		}
	}
	// An older arrangement of control groups keeps systemd's tree in a
	// directory of its own.
	testhelpers.KernelFiles(t, map[string]string{"sys/fs/cgroup/systemd/system.slice/myapp.service/cgroup.procs": "5\n", "proc/5/cmdline": "old\x00"})
	os.WriteFile(shown, []byte("LoadState=loaded\nActiveState=active\nControlGroup=/system.slice/myapp.service\n"), 0644)
	if processes, err := primitives.ProcessesOfSystemdUnit(testhelpers.TestContext(t), "myapp.service"); err != nil || !reflect.DeepEqual(processes, []primitives.UnitProcess{{ID: 5, Command: "old"}}) {
		t.Fatal(processes, err)
	}
	// The kernel removes a control group with its last process. A unit
	// whose control group is gone has no readable process count.
	testhelpers.KernelFiles(t, map[string]string{"sys/fs/cgroup/": ""})
	if processes, err := primitives.ProcessesOfSystemdUnit(testhelpers.TestContext(t), "myapp.service"); err == nil || len(processes) != 0 {
		t.Fatal(processes, err)
	}
	// On a node whose control groups MMWatchdog cannot find at all, MMWatchdog says where
	// it looked.
	testhelpers.KernelFiles(t, map[string]string{"proc/1/cmdline": "init\x00"})
	if _, err := primitives.ProcessesOfSystemdUnit(testhelpers.TestContext(t), "myapp.service"); err == nil || !strings.Contains(err.Error(), "cannot read the control group of myapp.service") {
		t.Fatal(err)
	}

	// A unit that systemd keeps active after its program has ended
	// (RemainAfterExit) has no control group and runs no process: the check
	// of a command fails, and the number of processes is 0.
	testhelpers.KernelFiles(t, map[string]string{"sys/fs/cgroup/": ""})
	os.WriteFile(shown, []byte("LoadState=loaded\nActiveState=active\nSubState=exited\nControlGroup=\n"), 0644)
	if o := runs("myapp-worker"); o.Status != "failed" || o.Message != "systemd unit myapp.service runs no command that contains myapp-worker" || o.Details != nil {
		t.Fatal(o)
	}
	if o := checksandalerts.ObserveCheck(testhelpers.TestContext(t), count); o.Status != "ok" || o.Measurements["process_count"].Value != 0 {
		t.Fatal(o)
	}
}

// A systemd_unit_commands check states its unit and the text.
func TestTheInputsOfASystemdUnitCommandsCheck(t *testing.T) {
	t.Parallel()
	for inputs, want := range map[string]string{
		"{systemd_unit: myapp.service, fail_when_no_command_contains: worker}": "",
		"{fail_when_no_command_contains: worker}":                              "check_groups.app.systemd_unit_commands.systemd_unit: is required",
		"{systemd_unit: myapp.timer, fail_when_no_command_contains: worker}":   "check_groups.app.systemd_unit_commands.systemd_unit: must be a systemd .service unit name",
		"{systemd_unit: myapp.service}":                                        "check_groups.app.systemd_unit_commands.fail_when_no_command_contains: is required",
		"{systemd_unit: myapp.service, fail_when_no_command_contains: ' '}":    "check_groups.app.systemd_unit_commands.fail_when_no_command_contains: is required",
	} {
		_, err := config.Load(testhelpers.ConfigFile(t, "check_groups: {app: {systemd_unit_commands: "+inputs+", alert_severity: warn}}"))
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v", inputs, err)
		}
	}
}

func TestSystemdProcessCountRequiresEveryNestedList(t *testing.T) {
	group := "sys/fs/cgroup/system.slice/myapp.service/"
	files := map[string]string{group + "cgroup.procs": "1", "proc/1/cmdline": "parent"}
	nested := group
	for depth := 0; depth < 12; depth++ {
		nested += "child/"
		files[nested+"cgroup.procs"] = ""
	}
	files[nested+"cgroup.procs"] = "2"
	files["proc/2/cmdline"] = "deep-child"
	testhelpers.KernelFiles(t, files)
	testhelpers.FakeUnit(t, "LoadState=loaded\nActiveState=active\nControlGroup=/system.slice/myapp.service\n")
	check := checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitResource{SystemdUnit: "myapp.service", Resource: "processes"}}
	observe := func() checksandalerts.Observation {
		return checksandalerts.ObserveCheck(testhelpers.TestContext(t), check)
	}
	if o := observe(); o.Status != "ok" || o.Measurements["process_count"].Value != 2 {
		t.Fatal(o)
	}
	for _, data := range []string{"not-a-process-id", "-2"} {
		files[nested+"cgroup.procs"] = data
		testhelpers.KernelFiles(t, files)
		if o := observe(); o.Status != "unknown" || !strings.Contains(o.Message, "cgroup.procs") {
			t.Fatal(o)
		}
	}
	testhelpers.KernelFiles(t, map[string]string{group + "cgroup.procs": "1", group + "child/cgroup.procs/": "", "proc/1/cmdline": "parent"})
	if o := observe(); o.Status != "unknown" || !strings.Contains(o.Message, "child/cgroup.procs") {
		t.Fatal(o)
	}
}
