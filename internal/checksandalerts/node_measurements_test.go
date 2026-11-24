package checksandalerts_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/testhelpers"
)

// The tests of the measurements of the node beyond CPU, memory, swap, and
// load: the time that programs wait for a disk, the memory pressure, the
// open files, disk devices, network interfaces, and every mounted
// filesystem. The rules are in docs/changing-mmwatchdog/design/checks.md under "Resource
// measurements".

// configFileWithoutCheckSeverities writes text as a configuration in which
// the test itself states the alert_severity of each check, or leaves it
// out: the node's settings and the timings of the check groups are filled
// in.
func configFileWithoutCheckSeverities(t *testing.T, text string) string {
	t.Helper()
	node := ""
	for _, setting := range checksandalerts.Names(testhelpers.TestNodeSeverities) {
		node += setting + ": " + testhelpers.TestNodeSeverities[setting] + "\n"
	}
	return testhelpers.ConfigFileWithoutSeverities(t, node+text)
}

func TestIOWaitMemoryPressureAndOpenFilesReadings(t *testing.T) {
	t.Parallel()
	// 1000 ticks passed, 150 of them waiting for a disk.
	a := primitives.CPUReading{Ticks: [8]uint64{100, 0, 100, 700, 100}, Processors: 1, At: time.Unix(0, 0)}
	b := primitives.CPUReading{Ticks: [8]uint64{500, 0, 250, 1000, 250}, Processors: 1, At: time.Unix(1, 0)}
	if got, ok := checksandalerts.IOWaitPercent(a, b); !ok || got != 15 {
		t.Fatal(got, ok)
	}
	// The kernel documents that the I/O wait can fall back: that is zero.
	b.Ticks[4] = 50
	if got, ok := checksandalerts.IOWaitPercent(a, b); !ok || got != 0 {
		t.Fatal(got, ok)
	}
	if _, ok := checksandalerts.IOWaitPercent(a, a); ok {
		t.Fatal("measured readings of one moment")
	}

	if got, err := checksandalerts.MemoryPressurePercent("some avg10=1.50 avg60=12.25 avg300=3.00 total=123456\nfull avg10=0.00 avg60=4.00 avg300=0.00 total=789\n"); err != nil || got != 12.25 {
		t.Fatal(got, err)
	}
	for _, text := range []string{"", "full avg10=0.00 avg60=4.00 avg300=0.00 total=789\n", "some avg10=1.50 total=1\n", "some avg60=many\n", "some avg60=101\n"} {
		if got, err := checksandalerts.MemoryPressurePercent(text); err == nil {
			t.Errorf("%q: read %v", text, got)
		}
	}

	if got, err := checksandalerts.OpenFilesPercent("2500\t500\t8000\n"); err != nil || got != 25 {
		t.Fatal(got, err)
	}
	for _, text := range []string{"", "1 2", "1 2 0", "5 6 100", "200 0 100", "a 0 100"} {
		if got, err := checksandalerts.OpenFilesPercent(text); err == nil {
			t.Errorf("%q: read %v", text, got)
		}
	}
}

// A node_resource check measures each of the three from the kernel's files,
// and a kernel without memory pressure makes that check unknown with a
// message that says so.
func TestNodeResourceChecksOfIOWaitMemoryPressureAndOpenFiles(t *testing.T) {
	root := testhelpers.KernelFiles(t, map[string]string{
		"proc/stat":            "cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 100 0 100 700 100 0 0 0 0 0\n",
		"proc/pressure/memory": "some avg10=0.00 avg60=12.25 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n",
		"proc/sys/fs/file-nr":  "2500\t0\t10000\n",
	})
	observe := func(resource string, above float64) checksandalerts.Observation {
		measurement := checksandalerts.NodeResourceMeasurements[resource][0]
		check := checksandalerts.Check{Inputs: new(checksandalerts.NodeResource(resource)), FailWhenMeasurementAbove: map[string]float64{measurement: above}}
		check.AlertSeverity = "warn"
		return checksandalerts.ObserveCheck(context.Background(), check)
	}
	if o := observe("memory_pressure", 10); o.Status != "failed" || o.Message != "memory_pressure_percent is 12.25 (fails above 10)" || o.Measurements["memory_pressure_percent"] != (checksandalerts.Measurement{Value: 12.25, Unit: "percent"}) {
		t.Fatal(o)
	}
	if o := observe("open_files", 80); o.Status != "ok" || o.Message != "open_files_used_percent is 25 (fails above 80)" {
		t.Fatal(o)
	}
	// The check's previous run read the counters as they stand now. Since
	// then 600 ticks passed, 300 of them waiting for a disk.
	ioWait := checksandalerts.Check{Inputs: new(checksandalerts.NodeResource("io_wait")), FailWhenMeasurementAbove: map[string]float64{"io_wait_percent": 30}}
	ioWait.AlertSeverity = "warn"
	run := checksandalerts.NewCheckRun(ioWait, checksandalerts.Timing{})
	previous, err := checksandalerts.ReadCPUStat(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run.Memory.Value = &checksandalerts.CPUSampler{Previous: &previous}
	os.WriteFile(filepath.Join(root, "proc/stat"), []byte("cpu  200 0 200 800 400 0 0 0 0 0\ncpu0 200 0 200 800 400 0 0 0 0 0\n"), 0644)
	if o := checksandalerts.RunCheck(context.Background(), run); o.Status != "failed" || !strings.HasPrefix(o.Message, "io_wait_percent is 50 (fails above 30) over ") || o.SampleWindow <= 0 || o.Details != nil {
		t.Fatal(o)
	}
	os.Remove(filepath.Join(root, "proc/pressure/memory"))
	if o := observe("memory_pressure", 10); o.Status != "unknown" || o.Message != "the kernel of this machine does not provide the memory pressure (/proc/pressure/memory)" {
		t.Fatal(o)
	}
	os.WriteFile(filepath.Join(root, "proc/sys/fs/file-nr"), []byte("broken"), 0644)
	if o := observe("open_files", 80); o.Status != "unknown" || o.Measurements != nil {
		t.Fatal(o)
	}

	// A check group with a check of the I/O wait needs the time of the
	// first sample, like one of the CPU.
	for resource, valid := range map[string]bool{"io_wait": false, "cpu": false, "memory_pressure": true, "open_files": true, "disk": false} {
		_, err := config.Load(testhelpers.ConfigFile(t, "check_groups: {machine: {check_every: 1s, time_limit_per_run: 200ms, node_resource: "+resource+"}}"))
		if valid != (err == nil) {
			t.Errorf("%s: %v", resource, err)
		}
	}
	if _, err := config.Load(testhelpers.ConfigFile(t, "check_groups: {machine: {only_record_readings: true, node_resource: disk}}")); err == nil || !strings.Contains(err.Error(), "check_groups.machine.node_resource: must be one of cpu, memory, swap, load, io_wait, memory_pressure, open_files") {
		t.Fatal(err)
	}
}

// A rate is the growth of the kernel's counters between two readings of a
// check. The first run takes its own baseline and waits for the sample
// window; a later run measures since the run before. A counter that fell
// back, and a baseline that is too old for the check's schedule, start a
// fresh sample.
func TestCounterReadingsOfACheck(t *testing.T) {
	t.Parallel()
	counter, reads := uint64(100), 0
	read := func() ([]uint64, error) {
		reads++
		counter += 50
		return []uint64{counter, 7}, nil
	}
	run := checksandalerts.NewCheckRun(checksandalerts.Check{}, checksandalerts.Timing{CheckEvery: "10s", TimeLimitPerRun: "1s"})
	started := time.Now()
	before, now, window, problem := run.CounterReadings(context.Background(), read)
	if problem != "" || reads != 2 || !reflect.DeepEqual(before, []uint64{150, 7}) || !reflect.DeepEqual(now, []uint64{200, 7}) || window < checksandalerts.CPUSampleWindow || time.Since(started) < checksandalerts.CPUSampleWindow {
		t.Fatal(before, now, window, problem, reads)
	}
	// The next run measures since this one, without a wait: it measures
	// also under a context that has ended, where a wait for a sample would
	// end at once and say so.
	time.Sleep(10 * time.Millisecond)
	ended, end := context.WithCancel(context.Background())
	end()
	before, now, window, problem = run.CounterReadings(ended, read)
	if problem != "" || reads != 3 || before[0] != 200 || now[0] != 250 || window < 10*time.Millisecond {
		t.Fatal(before, now, window, problem, reads)
	}
	// A counter that fell back, as after the device went away and came
	// back, starts a fresh sample.
	counter = 0
	if before, now, _, problem = run.CounterReadings(context.Background(), read); problem != "" || reads != 5 || before[0] != 50 || now[0] != 100 {
		t.Fatal(before, now, problem, reads)
	}
	// So does a baseline that is older than two intervals and a time limit.
	run.Memory.Value.(*checksandalerts.CounterSampler).At = time.Now().Add(-22 * time.Second)
	if _, _, window, problem = run.CounterReadings(context.Background(), read); problem != "" || reads != 7 || window > time.Second {
		t.Fatal(window, problem, reads)
	}
	// A run of mmwatchdog check-now has no schedule and keeps its baseline.
	diagnostic := checksandalerts.NewCheckRun(checksandalerts.Check{}, checksandalerts.Timing{})
	diagnostic.Memory.Value = &checksandalerts.CounterSampler{Previous: []uint64{1, 1}, At: time.Now().Add(-time.Hour)}
	if before, _, window, problem = diagnostic.CounterReadings(context.Background(), read); problem != "" || before[0] != 1 || window < time.Hour {
		t.Fatal(before, window, problem)
	}
	// A reading that fails, and a run that ends in the sample, measure
	// nothing and keep no baseline.
	failing := func() ([]uint64, error) { return nil, errors.New("the counters are gone") }
	if _, _, _, problem = run.CounterReadings(context.Background(), failing); problem != "the counters are gone" || run.Memory.Value.(*checksandalerts.CounterSampler).Previous != nil {
		t.Fatal(problem)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, problem = run.CounterReadings(ctx, read); problem != "sampling: context canceled" || run.Memory.Value.(*checksandalerts.CounterSampler).Previous != nil {
		t.Fatal(problem)
	}
}

// diskstats is one line of /proc/diskstats for a device, with the sectors
// read, the sectors written, and the milliseconds in which it had work.
func diskstats(name string, sectorsRead, sectorsWritten, millisecondsWithWork uint64) string {
	return fmt.Sprintf("   8       0 %s 100 0 %d 50 200 0 %d 80 0 %d 130 0 0 0 0 0 0\n", name, sectorsRead, sectorsWritten, millisecondsWithWork)
}

// A node_disk_device check measures how busy one disk device is and how
// much is read from it and written to it, from two readings of the kernel's
// counters.
func TestNodeDiskDeviceCheck(t *testing.T) {
	nodeFiles(t, map[string]string{"dev/sda": ""})
	root := testhelpers.KernelFiles(t, map[string]string{"proc/diskstats": diskstats("sda", 1000, 2000, 500) + diskstats("sda1", 1, 1, 1)})
	run := checksandalerts.NewCheckRun(checksandalerts.Check{Inputs: new(checksandalerts.NodeDiskDevice("/dev/sda")), FailWhenMeasurementAbove: map[string]float64{"disk_device_busy_percent": 90}}, checksandalerts.Timing{})
	run.Check.AlertSeverity = "warn"
	// One hour before, the counters of the device stood at zero. Since then
	// the device had work in 57 of the 60 minutes, 95 percent, and 3.6
	// million sectors were read and 7.2 million written. The rates are
	// those over the sample window that the observation names: the hour
	// and the time that the test took to get to the run, which is far
	// shorter than the three minutes that would bring the device below 90
	// percent, also on a busy machine.
	os.WriteFile(filepath.Join(root, "proc/diskstats"), []byte(diskstats("sda", 3600000, 7200000, 3420000)), 0644)
	run.Memory.Value = &checksandalerts.CounterSampler{Previous: []uint64{0, 0, 0}, At: time.Now().Add(-time.Hour)}
	o := checksandalerts.RunCheck(context.Background(), run)
	busy, read, written := o.Measurements["disk_device_busy_percent"], o.Measurements["read_mib_per_second"], o.Measurements["write_mib_per_second"]
	seconds := o.SampleWindow.Seconds()
	isAbout := func(got, want float64) bool { return math.Abs(got-want) < 1e-9 }
	if o.Status != "failed" || busy.Unit != "percent" || !isAbout(busy.Value, 3420000/seconds/10) || busy.Value <= 90 || read.Unit != "MiB per second" ||
		!isAbout(read.Value, 3600000*512/seconds/(1024*1024)) || !isAbout(written.Value, 7200000*512/seconds/(1024*1024)) ||
		!strings.HasPrefix(o.Message, "disk_device_busy_percent is ") || !strings.HasSuffix(o.Message, fmt.Sprintf(" (fails above 90) over %.3fs", seconds)) || o.SampleWindow < time.Hour {
		t.Fatal(o)
	}
	// The first run of a check takes a sample of its own.
	first := checksandalerts.ObserveCheck(context.Background(), checksandalerts.Check{Inputs: new(checksandalerts.NodeDiskDevice("/dev/sda"))})
	if first.Status != "ok" || first.Measurements["disk_device_busy_percent"].Value != 0 || first.SampleWindow < checksandalerts.CPUSampleWindow ||
		first.Message != fmt.Sprintf("disk_device_busy_percent is 0; read_mib_per_second is 0; write_mib_per_second is 0 over %.3fs", first.SampleWindow.Seconds()) {
		t.Fatal(first)
	}
	// A device that is busy all the time reads 100, never more.
	if got, _ := checksandalerts.DiskDeviceCounters(diskstats("sda", 0, 0, 99999), "sda", "/dev/sda"); !reflect.DeepEqual(got, []uint64{0, 0, 99999}) {
		t.Fatal(got)
	}
	run.Memory.Value = &checksandalerts.CounterSampler{Previous: []uint64{0, 0, 0}, At: time.Now().Add(-100 * time.Millisecond)}
	if o := checksandalerts.RunCheck(context.Background(), run); o.Measurements["disk_device_busy_percent"].Value != 100 {
		t.Fatal(o)
	}
	// A device that does not exist makes the check unknown.
	if o := checksandalerts.ObserveCheck(context.Background(), checksandalerts.Check{Inputs: new(checksandalerts.NodeDiskDevice("/dev/sdx"))}); o.Status != "unknown" ||
		!strings.Contains(o.Message, "cannot resolve the device path /dev/sdx") {
		t.Fatal(o)
	}
	if o := checksandalerts.ObserveCheck(context.Background(), checksandalerts.Check{Inputs: new(checksandalerts.NodeDiskDevice("/dev/missing/sda"))}); o.Status != "unknown" || !strings.Contains(o.Message, "/dev/missing/sda") {
		t.Fatal(o)
	}
	for path, want := range map[string]string{"/dev/sda": "", "/dev/mapper/data": "", "sda": "must be the path of a disk device, such as /dev/sda", "/dev/../etc": "must be the path of a disk device", "/srv": "must be the path of a disk device"} {
		_, err := config.Load(testhelpers.ConfigFile(t, "check_groups: {disk: {node_disk_device: "+path+"}}"))
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), "check_groups.disk.node_disk_device: "+want)) {
			t.Errorf("%s: %v", path, err)
		}
	}
}

// A node_network_interface check measures the speed of the link and the
// traffic. With an alert severity it fails while the interface has no link,
// with or without a limit; a check that only records notes it.
func TestNodeNetworkInterfaceCheck(t *testing.T) {
	net := "sys/class/net/eth0/"
	root := testhelpers.KernelFiles(t, map[string]string{net + "statistics/rx_bytes": "1250000\n", net + "statistics/tx_bytes": "2500000\n", net + "speed": "1000\n", net + "carrier": "1\n"})
	observe := func(check checksandalerts.Check, severity string) checksandalerts.Observation {
		check.Inputs, check.AlertSeverity = new(checksandalerts.NodeNetworkInterface("eth0")), severity
		run := checksandalerts.NewCheckRun(check, checksandalerts.Timing{})
		// One second before, the counters stood at zero: 10 megabits were
		// received and 20 sent.
		run.Memory.Value = &checksandalerts.CounterSampler{Previous: []uint64{0, 0}, At: time.Now().Add(-time.Second)}
		return checksandalerts.RunCheck(context.Background(), run)
	}
	slow := checksandalerts.Check{FailWhenMeasurementBelow: map[string]float64{"link_speed_mbit_per_second": 10000}}
	o := observe(checksandalerts.Check{}, "")
	// The rates are those over the sample window that the observation
	// names: the second and the time that the test took to get to the run,
	// which is longer on a busy machine.
	received, sent, seconds := o.Measurements["received_mbit_per_second"], o.Measurements["sent_mbit_per_second"], o.SampleWindow.Seconds()
	if o.Status != "ok" || o.Measurements["link_speed_mbit_per_second"] != (checksandalerts.Measurement{Value: 1000, Unit: "Mbit per second"}) || received != (checksandalerts.Measurement{Value: 10 / seconds, Unit: "Mbit per second"}) || sent.Value != 20/seconds || o.SampleWindow < time.Second ||
		!strings.HasPrefix(o.Message, "link_speed_mbit_per_second is 1000; received_mbit_per_second is ") || !strings.HasSuffix(o.Message, fmt.Sprintf(" over %.3fs", seconds)) {
		t.Fatal(o)
	}
	if o := observe(slow, "sos"); o.Status != "failed" || !strings.HasPrefix(o.Message, "link_speed_mbit_per_second is 1000 (fails below 10000) over ") {
		t.Fatal(o)
	}
	// Many virtual machines report no speed: a limit on it is unknown there.
	os.WriteFile(filepath.Join(root, net+"speed"), []byte("-1\n"), 0644)
	if o := observe(slow, "sos"); o.Status != "unknown" || o.Message != "network interface eth0 reports no link speed, so link_speed_mbit_per_second cannot be measured" {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.Check{}, ""); o.Status != "ok" || len(o.Measurements) != 2 {
		t.Fatal(o)
	}
	// Without a link, a check with an alert_severity fails: with a limit on
	// the traffic, without a limit, and with a limit on the speed, which the
	// interface no longer reports. The traffic is still recorded.
	os.WriteFile(filepath.Join(root, net+"carrier"), []byte("0\n"), 0644)
	for _, check := range []checksandalerts.Check{{}, slow, {FailWhenMeasurementAbove: map[string]float64{"received_mbit_per_second": 900}}} {
		if o := observe(check, "sos"); o.Status != "failed" || !strings.HasPrefix(o.Message, "network interface eth0 has no link") || len(o.Measurements) != 2 {
			t.Fatal(o)
		}
	}
	if o := observe(slow, "sos"); o.Message != "network interface eth0 has no link" {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.Check{}, ""); o.Status != "ok" || !strings.HasPrefix(o.Message, "network interface eth0 has no link; received_mbit_per_second is ") {
		t.Fatal(o)
	}
	// An interface that is turned off gives no answer about its link.
	os.Remove(filepath.Join(root, net+"carrier"))
	if o := observe(checksandalerts.Check{}, "warn"); o.Status != "failed" {
		t.Fatal(o)
	}
	// An interface that does not exist, and one without counters, are unknown.
	if o := checksandalerts.ObserveCheck(context.Background(), checksandalerts.Check{Inputs: new(checksandalerts.NodeNetworkInterface("eth9"))}); o.Status != "unknown" || o.Message != "network interface eth9 does not exist" {
		t.Fatal(o)
	}
	os.Remove(filepath.Join(root, net+"statistics/tx_bytes"))
	if o := observe(checksandalerts.Check{}, "warn"); o.Status != "unknown" || !strings.HasPrefix(o.Message, "network interface eth0: the count of sent bytes is unavailable: ") {
		t.Fatal(o)
	}

	for text, want := range map[string]string{
		"{only_record_readings: true, node_network_interface: eth0}":                                                    "",
		"{node_network_interface: eth0, alert_severity: sos}":                                                           "",
		"{only_record_readings: true, node_network_interface: 'br-1a2b.100'}":                                           "",
		"{only_record_readings: true, node_network_interface: 'eth0/../x'}":                                             "check_groups.net.node_network_interface: must be the name of a network interface, such as eth0",
		"{only_record_readings: true, node_network_interface: an_interface_name_that_is_too_long}":                      "check_groups.net.node_network_interface: must be the name of a network interface",
		"{only_record_readings: true, node_network_interface: '..'}":                                                    "check_groups.net.node_network_interface: must be the name of a network interface",
		"{node_network_interface: eth0, fail_when_measurement_below: {speed: 5}}":                                       "check_groups.net.fail_when_measurement_below.speed: this check has no such measurement; it measures link_speed_mbit_per_second, received_mbit_per_second, sent_mbit_per_second",
		"{only_record_readings: true, node_network_interface: eth0, raise_to_sos_after_failing_for: 5m}":                "a check that only records readings has no limits or alert severity",
		"{node_disk_device: /dev/sda, alert_severity: warn}":                                                            "a node_disk_device check needs a limit to alert",
		"{node_disk_device: /dev/sda, fail_when_measurement_above: {disk_device_busy_percent: 101}}":                    "check_groups.net.fail_when_measurement_above.disk_device_busy_percent: expected a number from 0 to 100",
		"{node_disk_device: /dev/sda, fail_when_measurement_above: {read_mib_per_second: -1}}":                          "check_groups.net.fail_when_measurement_above.read_mib_per_second: expected a number of 0 or more",
		"{only_record_readings: true, check_every: 1s, time_limit_per_run: 200ms, node_network_interface: eth0}":        "check_groups.net.time_limit_per_run: must exceed 200ms for the initial sample of the network interface",
		"{only_record_readings: true, check_every: 1s, time_limit_per_run: 200ms, node_disk_device: /dev/sda}":          "check_groups.net.time_limit_per_run: must exceed 200ms for the initial sample of the disk device",
		"{node_resource: memory, fail_when_measurement_above: {memory_pressure_percent: 5}}":                            "check_groups.net.fail_when_measurement_above.memory_pressure_percent: this check has no such measurement; it measures memory_used_percent",
		"{node_resource: open_files, fail_when_measurement_above: {open_files_used_percent: 80}, alert_severity: warn}": "",
	} {
		_, err := config.Load(configFileWithoutCheckSeverities(t, "check_groups: {net: "+text+"}"))
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v", text, err)
		}
	}
}

// mountinfo is one line of /proc/self/mountinfo.
func mountinfo(deviceNumbers, mountPoint, filesystemType, source string) string {
	return "36 35 " + deviceNumbers + " / " + mountPoint + " rw,noatime shared:1 - " + filesystemType + " " + source + " rw\n"
}

// The kernel's list of mounts gives, for each mount, where it is mounted,
// the kind of filesystem, and what is mounted. A line can have any number
// of optional fields in front of the hyphen, and the kernel writes a space
// in a path as a backslash and three digits.
func TestTheKernelsListOfMountedFilesystems(t *testing.T) {
	testhelpers.KernelFiles(t, map[string]string{"proc/self/mountinfo": mountinfo("8:1", "/", "ext4", "/dev/sda1") +
		"40 36 8:2 / /mnt/my\\040disk\\134here rw - xfs /dev/sdb1 rw\n" +
		"41 36 0:30 /sub /mnt/nfs rw,relatime shared:5 master:1 propagate_from:2 - nfs4 files.example.com:/export rw\n" +
		"broken line\n42 36 0:5 / /proc rw\n\n"})
	mounted, err := primitives.MountedFilesystems()
	if want := []primitives.MountedFilesystem{{MountPoint: "/", FilesystemType: "ext4", Source: "/dev/sda1", DeviceNumbers: "8:1"}, {MountPoint: "/mnt/my disk\\here", FilesystemType: "xfs", Source: "/dev/sdb1", DeviceNumbers: "8:2"}, {MountPoint: "/mnt/nfs", FilesystemType: "nfs4", Source: "files.example.com:/export", DeviceNumbers: "0:30"}}; err != nil || !reflect.DeepEqual(mounted, want) {
		t.Fatalf("%+v %v", mounted, err)
	}
	if primitives.UnescapeMountField(`a\04`) != `a\04` || primitives.UnescapeMountField(`a\999b`) != `a\999b` {
		t.Fatal("changed a backslash that starts no octal number")
	}
}

// An every_disk_and_network_filesystem check covers the filesystems that are mounted
// from a disk device or from another machine, each once.
func TestTheFilesystemsThatAnEveryDiskAndNetworkFilesystemCheckCovers(t *testing.T) {
	t.Parallel()
	covered, alsoMountedAt := checksandalerts.FilesystemsWithStorage(primitives.ParseMountInformation(
		mountinfo("0:5", "/proc", "proc", "proc") + mountinfo("0:6", "/run", "tmpfs", "tmpfs") + mountinfo("0:7", "/dev", "devtmpfs", "udev") +
			mountinfo("8:1", "/", "ext4", "/dev/sda1") + mountinfo("8:1", "/var/lib/docker/volumes/data", "ext4", "/dev/sda1") +
			mountinfo("0:40", "/var/lib/docker/overlay2/abc/merged", "overlay", "overlay") + mountinfo("7:0", "/snap/core/1", "squashfs", "/dev/loop0") +
			mountinfo("7:1", "/media/cd", "iso9660", "/dev/sr0") + mountinfo("253:0", "/srv/data", "xfs", "/dev/mapper/vg-data") +
			mountinfo("0:50", "/mnt/nfs", "nfs4", "files.example.com:/export") + mountinfo("0:51", "/mnt/share", "cifs", "//files/share") +
			mountinfo("0:60", "/tank", "zfs", "tank") + mountinfo("0:61", "/tank/home", "zfs", "tank/home") + mountinfo("0:70", "/mnt/fuse", "fuse.rclone", "remote:bucket") +
			mountinfo("0:80", "/sys/fs/cgroup", "cgroup2", "cgroup2") + mountinfo("8:17", "/boot/efi", "vfat", "/dev/sdb1")))
	paths := []string{}
	for _, filesystem := range covered {
		paths = append(paths, filesystem.MountPoint)
	}
	if want := []string{"/", "/srv/data", "/mnt/nfs", "/mnt/share", "/tank", "/tank/home", "/boot/efi"}; !reflect.DeepEqual(paths, want) {
		t.Fatal(paths)
	}
	// A filesystem that is mounted at a second path is covered once, and
	// its other path is kept.
	if want := map[string][]string{"/": {"/var/lib/docker/volumes/data"}}; !reflect.DeepEqual(alsoMountedAt, want) {
		t.Fatal(alsoMountedAt)
	}
}

// A filesystem that failed and then disappears is not recovered: it stays a
// finding, "no longer mounted". A healthy one that disappears just leaves.
func TestAFailingFilesystemThatIsUnmountedStaysFailed(t *testing.T) {
	testhelpers.KernelFiles(t, map[string]string{"proc/self/mountinfo": mountinfo("8:1", "/", "ext4", "/dev/sda1") + mountinfo("8:2", "/var", "ext4", "/dev/sda2") + mountinfo("8:3", "/srv", "ext4", "/dev/sda3")})
	used := func(percent uint64) primitives.FilesystemCounts {
		return primitives.FilesystemCounts{Blocks: 1000, BlocksAvailable: 10 * (100 - percent), BlockBytes: 1024 * 1024, Inodes: 1000, InodesFree: 500}
	}
	check := checksandalerts.Check{Inputs: &checksandalerts.EveryDiskAndNetworkFilesystem{FilesystemsToIgnore: []string{}}, FailWhenMeasurementAbove: map[string]float64{"space_used_percent": 90}}
	check.AlertSeverity = "warn"
	state := &checksandalerts.ModuleState{}
	run := func() checksandalerts.Observation {
		r := checksandalerts.NewCheckRun(check, checksandalerts.Timing{})
		r.State = &checksandalerts.ModuleState{Stored: state.Saved}
		o := checksandalerts.RunCheck(context.Background(), r)
		state = r.State
		return o
	}
	kernelFilesystems(t, map[string]primitives.FilesystemCounts{"/": used(40), "/var": used(95), "/srv": used(20)})
	if o := run(); len(o.Findings) != 1 || o.Findings[0].ID != "/var" {
		t.Fatal(o)
	}
	// /var and /srv are unmounted: /var stays failed, /srv leaves silently.
	testhelpers.KernelFiles(t, map[string]string{"proc/self/mountinfo": mountinfo("8:1", "/", "ext4", "/dev/sda1")})
	kernelFilesystems(t, map[string]primitives.FilesystemCounts{"/": used(40)})
	want := []checksandalerts.Finding{{ID: "/var", Message: "the filesystem at /var is no longer mounted", ExactFinding: "/var unreadable"}}
	for range 2 {
		if o := run(); o.Status != "failed" || !reflect.DeepEqual(o.Findings, want) {
			t.Fatal(o)
		}
	}
	// Still listed by the kernel but gone when asked: the same.
	testhelpers.KernelFiles(t, map[string]string{"proc/self/mountinfo": mountinfo("8:1", "/", "ext4", "/dev/sda1") + mountinfo("8:2", "/var", "ext4", "/dev/sda2")})
	if o := run(); !reflect.DeepEqual(o.Findings, want) {
		t.Fatal(o)
	}
	// Mounted and healthy again: recovered.
	kernelFilesystems(t, map[string]primitives.FilesystemCounts{"/": used(40), "/var": used(50)})
	if o := run(); o.Status != "ok" || o.Findings != nil {
		t.Fatal(o)
	}
}

// kernelFilesystems puts a kernel in the place of the real one that knows
// the filesystems in counts, by their paths. Asking about a path in hangs
// does not return until the test calls answer, or ends.
func kernelFilesystems(t *testing.T, counts map[string]primitives.FilesystemCounts, hangs ...string) (answer func()) {
	t.Helper()
	real, release := primitives.AskKernelAboutFilesystem, make(chan struct{})
	primitives.AskKernelAboutFilesystem = func(path string) (primitives.FilesystemCounts, error) {
		if slices.Contains(hangs, path) {
			<-release
		}
		if c, mounted := counts[path]; mounted {
			return c, nil
		}
		return primitives.FilesystemCounts{}, syscall.ENOENT
	}
	answered := false
	answer = func() {
		if answered {
			return
		}
		answered = true
		close(release)
		for deadline := time.Now().Add(testhelpers.LongestWaitForAnEvent); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			primitives.FilesystemQuestions.Lock()
			waiting := len(primitives.FilesystemQuestions.Waiting)
			primitives.FilesystemQuestions.Unlock()
			if waiting == 0 {
				return
			}
		}
		t.Error("a question about a filesystem is still waiting")
	}
	t.Cleanup(func() {
		answer()
		primitives.AskKernelAboutFilesystem = real
	})
	return answer
}

// A node_filesystem check with an alert_severity fails when nothing is
// mounted at its path: without this rule it would measure the filesystem
// that holds the empty directory. A check that only records has nothing to
// record there.
func TestANodeFilesystemCheckFailsWhenItsFilesystemIsNotMounted(t *testing.T) {
	testhelpers.KernelFiles(t, map[string]string{"proc/self/mountinfo": mountinfo("8:1", "/", "ext4", "/dev/sda1") + mountinfo("8:2", "/mnt/san", "xfs", "/dev/sdb1")})
	half := primitives.FilesystemCounts{Blocks: 1000, BlocksAvailable: 500, BlockBytes: 4096, Inodes: 100, InodesFree: 50}
	kernelFilesystems(t, map[string]primitives.FilesystemCounts{"/": half, "/mnt/san": half, "/mnt/gone": half})
	observe := func(path, severity string) checksandalerts.Observation {
		check := checksandalerts.Check{Inputs: new(checksandalerts.NodeFilesystem(path))}
		check.AlertSeverity = severity
		return checksandalerts.ObserveCheck(context.Background(), check)
	}
	if o := observe("/mnt/san", "sos"); o.Status != "ok" || o.Measurements["space_used_percent"].Value != 50 {
		t.Fatal(o)
	}
	if o := observe("/mnt/san/", "sos"); o.Status != "ok" {
		t.Fatal(o)
	}
	if o := observe("/mnt/gone", "sos"); o.Status != "failed" || o.Message != "no filesystem is mounted at /mnt/gone" || o.Measurements != nil {
		t.Fatal(o)
	}
	if o := observe("/mnt/gone", ""); o.Status != "unknown" || o.Message != "no filesystem is mounted at /mnt/gone" || o.Measurements != nil {
		t.Fatal(o)
	}
	// Without the kernel's list, MMWatchdog does not know, and says so.
	testhelpers.KernelFiles(t, map[string]string{"proc/": ""})
	if o := observe("/", "sos"); o.Status != "unknown" || !strings.HasPrefix(o.Message, "MMWatchdog cannot read the kernel's list of mounted filesystems: ") {
		t.Fatal(o)
	}
}

// One question about a filesystem that does not answer keeps waiting, and
// MMWatchdog asks no second one while it does.
func TestAFilesystemThatDoesNotAnswer(t *testing.T) {
	var asked atomic.Int32
	real, release := primitives.AskKernelAboutFilesystem, make(chan struct{})
	primitives.AskKernelAboutFilesystem = func(path string) (primitives.FilesystemCounts, error) {
		asked.Add(1)
		<-release
		return primitives.FilesystemCounts{Blocks: 7}, nil
	}
	defer func() { primitives.AskKernelAboutFilesystem = real }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := primitives.FilesystemStatistics(ctx, "/mnt/nfs")
	if !errors.Is(err, primitives.ErrFilesystemDoesNotAnswer) || err.Error() != "the filesystem does not answer: the filesystem at /mnt/nfs gave no answer within the time limit of the run" {
		t.Fatal(err)
	}
	// The next run asks no second question. It waits for the answer to the
	// first one for its own time, and says since when MMWatchdog waits.
	again, cancelAgain := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelAgain()
	_, err = primitives.FilesystemStatistics(again, "/mnt/nfs")
	if !errors.Is(err, primitives.ErrFilesystemDoesNotAnswer) || !strings.HasPrefix(err.Error(), "the filesystem does not answer: MMWatchdog has waited for the filesystem at /mnt/nfs since ") || asked.Load() != 1 {
		t.Fatal(err, asked.Load())
	}
	// A run that waits when the kernel answers gets that answer.
	answer := make(chan error, 1)
	go func() {
		counts, err := primitives.FilesystemStatistics(context.Background(), "/mnt/nfs")
		if err == nil && counts.Blocks != 7 {
			err = fmt.Errorf("the answer has %d blocks", counts.Blocks)
		}
		answer <- err
	}()
	testhelpers.WaitFor(t, "the run to wait for the kernel's answer", func() bool { return testhelpers.AGoroutineWaitsInASelect("primitives.FilesystemStatistics") })
	close(release)
	if err := <-answer; err != nil || asked.Load() != 1 {
		t.Fatal(err, asked.Load())
	}
	// Once the kernel has answered, MMWatchdog asks again.
	if counts, err := primitives.FilesystemStatistics(context.Background(), "/mnt/nfs"); err != nil || counts.Blocks != 7 || asked.Load() != 2 {
		t.Fatal(counts, err, asked.Load())
	}
}

// Two checks of one filesystem that run in the same moment both get the
// kernel's answer: neither takes the other's question for a filesystem that
// does not answer.
func TestTwoQuestionsAboutOneFilesystemAtOnce(t *testing.T) {
	real := primitives.AskKernelAboutFilesystem
	primitives.AskKernelAboutFilesystem = func(path string) (primitives.FilesystemCounts, error) {
		return primitives.FilesystemCounts{Blocks: 7}, nil
	}
	defer func() { primitives.AskKernelAboutFilesystem = real }()
	for range 2000 {
		answers := make(chan error, 2)
		for range 2 {
			go func() {
				_, err := primitives.FilesystemStatistics(context.Background(), "/")
				answers <- err
			}()
		}
		for range 2 {
			if err := <-answers; err != nil {
				t.Fatal(err)
			}
		}
	}
}

// An every_disk_and_network_filesystem check applies its limits to every covered
// filesystem. Each filesystem that passes a limit, is mounted read-only, or
// does not answer is one finding. A filesystem that does not answer takes
// no time from the others.
func TestEveryDiskAndNetworkFilesystemCheck(t *testing.T) {
	testhelpers.KernelFiles(t, map[string]string{"proc/self/mountinfo": mountinfo("8:1", "/", "ext4", "/dev/sda1") + mountinfo("8:2", "/var", "ext4", "/dev/sda2") +
		mountinfo("8:3", "/srv/data", "xfs", "/dev/sda3") + mountinfo("8:4", "/boot/efi", "vfat", "/dev/sda4") + mountinfo("0:40", "/mnt/btrfs", "btrfs", "/dev/sdb1") +
		mountinfo("0:50", "/mnt/nfs", "nfs4", "files.example.com:/export") + mountinfo("0:6", "/run", "tmpfs", "tmpfs") + mountinfo("8:9", "/mnt/just-unmounted", "ext4", "/dev/sdc1")})
	megabyte := uint64(1024 * 1024)
	used := func(percent uint64) primitives.FilesystemCounts {
		return primitives.FilesystemCounts{Blocks: 1000, BlocksAvailable: 10 * (100 - percent), BlockBytes: megabyte, Inodes: 1000, InodesFree: 500}
	}
	full, readOnly, noInodes := used(95), used(20), used(30)
	readOnly.ReadOnly = true
	noInodes.Inodes, noInodes.InodesFree = 0, 0
	inodesFull := used(10)
	inodesFull.InodesFree = 10
	answer := kernelFilesystems(t, map[string]primitives.FilesystemCounts{"/": used(40), "/var": full, "/srv/data": readOnly, "/boot/efi": used(99), "/mnt/btrfs": noInodes, "/mnt/nfs": used(1), "/run": used(100)}, "/mnt/nfs")
	// The kernel answers about every other filesystem at once. The test
	// keeps the question that MMWatchdog asked for each answer, to see when MMWatchdog has
	// the answer.
	var questionsLock sync.Mutex
	answeredQuestions := map[string][]*primitives.FilesystemQuestion{}
	askTheTestKernel := primitives.AskKernelAboutFilesystem
	primitives.AskKernelAboutFilesystem = func(path string) (primitives.FilesystemCounts, error) {
		primitives.FilesystemQuestions.Lock()
		question := primitives.FilesystemQuestions.Waiting[path]
		primitives.FilesystemQuestions.Unlock()
		counts, err := askTheTestKernel(path)
		questionsLock.Lock()
		answeredQuestions[path] = append(answeredQuestions[path], question)
		questionsLock.Unlock()
		return counts, err
	}
	// runUntilOnlyNFSWaits runs the check under a time limit that passes
	// once MMWatchdog has the answers about the filesystems other than /mnt/nfs for
	// the run, the run with the number run, however long they take on a
	// busy machine.
	runUntilOnlyNFSWaits := func(check checksandalerts.Check, run int) checksandalerts.Observation {
		limit := testhelpers.NewTimeLimitThatTheTestEnds(context.Background())
		observed := make(chan checksandalerts.Observation, 1)
		go func() {
			observed <- checksandalerts.RunCheck(limit, checksandalerts.NewCheckRun(check, checksandalerts.Timing{}))
		}()
		testhelpers.WaitFor(t, "the answers about the filesystems other than /mnt/nfs", func() bool {
			questionsLock.Lock()
			defer questionsLock.Unlock()
			for _, path := range []string{"/", "/var", "/srv/data", "/mnt/btrfs", "/mnt/just-unmounted"} {
				if len(answeredQuestions[path]) < run {
					return false
				}
				select {
				case <-answeredQuestions[path][run-1].Answered:
				default:
					return false
				}
			}
			return true
		})
		limit.End()
		return <-observed
	}
	check := checksandalerts.Check{Inputs: &checksandalerts.EveryDiskAndNetworkFilesystem{FilesystemsToIgnore: []string{"/boot/efi"}}, FailWhenMeasurementAbove: map[string]float64{"space_used_percent": 90, "inodes_used_percent": 90}}
	check.AlertSeverity = "warn"
	o := runUntilOnlyNFSWaits(check, 1)
	want := []checksandalerts.Finding{
		{ID: "/mnt/nfs", Message: "the filesystem does not answer: the filesystem at /mnt/nfs gave no answer within the time limit of the run", ExactFinding: "/mnt/nfs unreadable"},
		{ID: "/srv/data", Message: "/srv/data is mounted read-only; space_used_percent is 20 (fails above 90); inodes_used_percent is 50 (fails above 90)", ExactFinding: "/srv/data read-only"},
		{ID: "/var", Message: "/var: space_used_percent is 95 (fails above 90); inodes_used_percent is 50 (fails above 90)", ExactFinding: "/var space"},
	}
	if o.Status != "failed" || o.Message != "3 filesystems fail: /mnt/nfs, /srv/data, /var (not counting 1 in filesystems_to_ignore: /boot/efi)" || !reflect.DeepEqual(o.Findings, want) || o.Measurements != nil {
		t.Fatalf("%s %q\n%+v", o.Status, o.Message, o.Findings)
	}
	if got := o.FindingIsGoneMessage("/var"); got != "the filesystem at /var no longer fails this check" {
		t.Fatal(got)
	}

	// While that question waits, the next run asks no second one. It waits
	// for the answer for its own time, and says since when MMWatchdog has waited.
	if o := runUntilOnlyNFSWaits(check, 2); len(o.Findings) != 3 || !strings.HasPrefix(o.Findings[0].Message, "the filesystem does not answer: MMWatchdog has waited for the filesystem at /mnt/nfs since ") {
		t.Fatal(o)
	}
	answer()

	// Healthy filesystems, and one that the check ignores.
	kernelFilesystems(t, map[string]primitives.FilesystemCounts{"/": used(40), "/var": used(50), "/srv/data": used(20), "/boot/efi": used(99), "/mnt/btrfs": noInodes, "/mnt/nfs": used(1)})
	check.Inputs = &checksandalerts.EveryDiskAndNetworkFilesystem{FilesystemsToIgnore: []string{"/boot/efi", "/mnt/nfs"}}
	if o := checksandalerts.ObserveCheck(context.Background(), check); o.Status != "ok" || o.Message != "4 filesystems are within the limits (not counting 2 in filesystems_to_ignore: /boot/efi, /mnt/nfs)" || o.Findings != nil {
		t.Fatal(o)
	}
	// A limit on the inodes applies where the filesystem counts them: the
	// filesystem at /mnt/btrfs counts none and stays within the limits.
	kernelFilesystems(t, map[string]primitives.FilesystemCounts{"/": inodesFull, "/mnt/btrfs": noInodes})
	check.Inputs = &checksandalerts.EveryDiskAndNetworkFilesystem{FilesystemsToIgnore: []string{}}
	if o := checksandalerts.ObserveCheck(context.Background(), check); o.Status != "failed" || len(o.Findings) != 1 ||
		o.Findings[0] != (checksandalerts.Finding{ID: "/", Message: "/: space_used_percent is 10 (fails above 90); inodes_used_percent is 99 (fails above 90)", ExactFinding: "/ inodes"}) || !strings.HasPrefix(o.Message, "1 filesystem fails: /") {
		t.Fatal(o)
	}
	// Without a limit, the check still fails for a read-only filesystem.
	kernelFilesystems(t, map[string]primitives.FilesystemCounts{"/": used(99), "/srv/data": readOnly})
	check.FailWhenMeasurementAbove = nil
	if o := checksandalerts.ObserveCheck(context.Background(), check); o.Status != "failed" || len(o.Findings) != 1 || o.Findings[0].ID != "/srv/data" || !strings.HasPrefix(o.Findings[0].Message, "/srv/data is mounted read-only; space_used_percent is 20; ") {
		t.Fatal(o)
	}
	// filesystems_to_ignore names a filesystem by any path where it is
	// mounted, also by one that the kernel does not list first.
	testhelpers.KernelFiles(t, map[string]string{"proc/self/mountinfo": mountinfo("8:1", "/", "ext4", "/dev/sda1") + mountinfo("8:17", "/mnt/backup", "ext4", "/dev/sdb1") + mountinfo("8:17", "/srv/backup", "ext4", "/dev/sdb1")})
	kernelFilesystems(t, map[string]primitives.FilesystemCounts{"/": used(40), "/mnt/backup": used(99), "/srv/backup": used(99)})
	check.FailWhenMeasurementAbove = map[string]float64{"space_used_percent": 90}
	check.Inputs = &checksandalerts.EveryDiskAndNetworkFilesystem{FilesystemsToIgnore: []string{"/srv/backup"}}
	if o := checksandalerts.ObserveCheck(context.Background(), check); o.Status != "ok" || o.Message != "1 filesystem is within the limits (not counting 1 in filesystems_to_ignore: /srv/backup)" {
		t.Fatal(o)
	}
	check.Inputs = &checksandalerts.EveryDiskAndNetworkFilesystem{FilesystemsToIgnore: []string{}}
	if o := checksandalerts.ObserveCheck(context.Background(), check); o.Status != "failed" || len(o.Findings) != 1 || o.Findings[0].ID != "/mnt/backup" {
		t.Fatal(o)
	}

	// A filesystem that answers with an error in place of its size is a
	// finding: a network filesystem whose server restarted does that. A
	// permission error is also a finding; a filesystem unmounted since
	// the kernel listed it is left out.
	testhelpers.KernelFiles(t, map[string]string{"proc/self/mountinfo": mountinfo("8:1", "/", "ext4", "/dev/sda1") + mountinfo("0:50", "/mnt/nfs", "nfs4", "files.example.com:/export") +
		mountinfo("0:51", "/mnt/share", "cifs", "//files/share") + mountinfo("0:52", "/mnt/gone", "nfs4", "files.example.com:/gone") + mountinfo("0:53", "/home/alice/remote", "nfs4", "files.example.com:/alice")})
	real := primitives.AskKernelAboutFilesystem
	primitives.AskKernelAboutFilesystem = func(path string) (primitives.FilesystemCounts, error) {
		switch path {
		case "/mnt/nfs":
			return primitives.FilesystemCounts{}, syscall.ESTALE
		case "/mnt/share":
			return primitives.FilesystemCounts{}, syscall.ENOTCONN
		case "/mnt/gone":
			return primitives.FilesystemCounts{}, syscall.ENOENT
		case "/home/alice/remote":
			return primitives.FilesystemCounts{}, syscall.EACCES
		}
		return used(40), nil
	}
	o = checksandalerts.ObserveCheck(context.Background(), check)
	primitives.AskKernelAboutFilesystem = real
	if o.Status != "failed" || o.Message != "3 filesystems fail: /home/alice/remote, /mnt/nfs, /mnt/share" || len(o.Findings) != 3 ||
		!strings.Contains(o.Findings[0].Message, "permission denied") || !strings.HasPrefix(o.Findings[1].Message, "the filesystem at /mnt/nfs gives an error in place of its size: ") || !strings.HasPrefix(o.Findings[2].Message, "the filesystem at /mnt/share gives an error in place of its size: ") ||
		o.Findings[0].ExactFinding != "/home/alice/remote unreadable" || o.Findings[1].ExactFinding != "/mnt/nfs unreadable" {
		t.Fatalf("%s %q %+v", o.Status, o.Message, o.Findings)
	}

	testhelpers.KernelFiles(t, map[string]string{"proc/self/mountinfo": mountinfo("8:1", "/", "ext4", "/dev/sda1")})
	kernelFilesystems(t, map[string]primitives.FilesystemCounts{"/": used(99)})
	check.FailWhenMeasurementAbove = nil
	if o := checksandalerts.ObserveCheck(context.Background(), check); o.Status != "ok" || o.Message != "1 filesystem is within the limits" {
		t.Fatal(o)
	}
	testhelpers.KernelFiles(t, map[string]string{"proc/": ""})
	if o := checksandalerts.ObserveCheck(context.Background(), check); o.Status != "unknown" {
		t.Fatal(o)
	}
}

// An every_disk_and_network_filesystem check states its list of ignored filesystems
// and an alert_severity, and takes limits on the measurements of a
// filesystem. Each filesystem is an incident of its own.
func TestTheInputsOfAnEveryDiskAndNetworkFilesystemCheck(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]string{
		"{every_disk_and_network_filesystem: {filesystems_to_ignore: [/boot/efi]}, fail_when_measurement_above: {space_used_percent: 90}, alert_severity: warn}": "",
		"{every_disk_and_network_filesystem: {filesystems_to_ignore: []}, alert_severity: warn}":                                                                 "",
		"{every_disk_and_network_filesystem: {}, alert_severity: warn}":                                                                                          "check_groups.fs.every_disk_and_network_filesystem.filesystems_to_ignore: is required: the paths where the filesystems are mounted that this check leaves out, such as [/boot/efi], or [] to leave none out",
		"{every_disk_and_network_filesystem: {filesystems_to_ignore: null}, alert_severity: warn}":                                                               "expected a list",
		"{every_disk_and_network_filesystem: {filesystems_to_ignore: [boot]}, alert_severity: warn}":                                                             "check_groups.fs.every_disk_and_network_filesystem.filesystems_to_ignore: expected distinct absolute paths",
		"{every_disk_and_network_filesystem: {filesystems_to_ignore: [/boot/]}, alert_severity: warn}":                                                           "check_groups.fs.every_disk_and_network_filesystem.filesystems_to_ignore: expected distinct absolute paths",
		"{every_disk_and_network_filesystem: {filesystems_to_ignore: [/a, /a]}, alert_severity: warn}":                                                           "check_groups.fs.every_disk_and_network_filesystem.filesystems_to_ignore: expected distinct absolute paths",
		"{every_disk_and_network_filesystem: {filesystems_to_ignore: []}}":                                                                                       "check_groups.fs.alert_severity: required: info, warn, or sos",
		"{every_disk_and_network_filesystem: {filesystems_to_ignore: []}, fail_when_measurement_above: {space_used: 90}, alert_severity: warn}":                  "check_groups.fs.fail_when_measurement_above.space_used: this check has no such measurement; it measures space_used_percent, inodes_used_percent, space_free_gib",
		"{every_disk_and_network_filesystem: {filesystems_to_ignore: []}, fail_when_measurement_above: {space_used_percent: 190}, alert_severity: warn}":         "check_groups.fs.fail_when_measurement_above.space_used_percent: expected a number from 0 to 100",
	} {
		_, err := config.Load(configFileWithoutCheckSeverities(t, "check_groups: {fs: "+text+"}"))
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v", text, err)
		}
	}
	// The order of the ignored filesystems is no part of what the check is.
	definition := func(ignored string) config.Definition {
		c, err := config.Load(testhelpers.ConfigFile(t, "check_groups: {filesystems: {checks: {every: {every_disk_and_network_filesystem: {filesystems_to_ignore: "+ignored+"}, fail_when_measurement_above: {space_used_percent: 90}}}}}"))
		if err != nil {
			t.Fatal(err)
		}
		return config.CheckDefinitions(c)["filesystems/every"]
	}
	a, b, other := definition("[/b, /a]"), definition("[/a, /b]"), definition("[/a]")
	if a.Identity() != b.Identity() || a.Identity() == other.Identity() || !a.Check.Module().OneIncidentForEachFinding || a.CheckWithSeverity().RecordsOnly() {
		t.Fatal(a, b)
	}
}

// mmwatchdog validate names every filesystem that has its own node_filesystem check
// with a limit and is missing from the filesystems_to_ignore of an
// every_disk_and_network_filesystem check: one full filesystem would send two alerts.
func TestFilesystemsMissingFromFilesystemsToIgnore(t *testing.T) {
	t.Parallel()
	c, err := config.Load(testhelpers.ConfigFile(t, `check_groups:
  filesystems:
    checks:
      every: {every_disk_and_network_filesystem: {filesystems_to_ignore: [/var]}, fail_when_measurement_above: {space_used_percent: 90}}
  own:
    checks:
      root: {node_filesystem: /, fail_when_measurement_above: {space_used_percent: 80}}
      root_again: {node_filesystem: /, fail_when_measurement_above: {space_used_percent: 95}}
      var: {node_filesystem: /var, fail_when_measurement_above: {space_used_percent: 80}}
      data: {node_filesystem: /srv/data/, fail_when_measurement_below: {space_free_gib: 5}}
      recorded: {only_record_readings: true, node_filesystem: /srv/recorded}
      mounted: {node_filesystem: /mnt/san, alert_severity: sos}
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []checksandalerts.FilesystemMissingFromFilesystemsToIgnore{{CheckGroup: "filesystems", Check: "every", Filesystem: "/", NodeFilesystemCheck: "own/root"}, {CheckGroup: "filesystems", Check: "every", Filesystem: "/srv/data", NodeFilesystemCheck: "own/data"}}
	if got := c.FilesystemsMissingFromFilesystemsToIgnore(); !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	if got := c.ValidationWarnings("node", true).FilesystemsMissingFromFilesystemsToIgnore; !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
}

func TestFilesystemFindingsKeepLongPathsAndUnknownObservations(t *testing.T) {
	path := "/mnt/" + strings.Repeat("long-path/", 40) + "data"
	testhelpers.KernelFiles(t, map[string]string{"proc/self/mountinfo": mountinfo("8:1", path, "ext4", "/dev/sda1")})
	kernelFilesystems(t, map[string]primitives.FilesystemCounts{path: {Blocks: 100, BlocksAvailable: 1, BlockBytes: 4096}})
	check := checksandalerts.Check{Inputs: &checksandalerts.EveryDiskAndNetworkFilesystem{FilesystemsToIgnore: []string{}}, FailWhenMeasurementAbove: map[string]float64{"space_used_percent": 90}}
	first := checksandalerts.ObserveCheck(context.Background(), check)
	second := checksandalerts.ObserveCheck(context.Background(), check)
	if first.Status != "failed" || len(first.Findings) != 1 || len(second.Findings) != 1 {
		t.Fatal(first, second)
	}
	finding := first.Findings[0]
	if !checksandalerts.ValidFindingID(finding.ID) || !strings.HasPrefix(finding.ID, "filesystem_path_sha256:") || second.Findings[0].ID != finding.ID || !strings.Contains(finding.Message, path) || !strings.Contains(first.Message, path) {
		t.Fatal(first, second)
	}
	// Impossible statistics make the individual observation unknown.
	kernelFilesystems(t, map[string]primitives.FilesystemCounts{path: {Blocks: 100, BlocksAvailable: 101, BlockBytes: 4096}})
	unknown := checksandalerts.ObserveCheck(context.Background(), check)
	if unknown.Status != "failed" || len(unknown.Findings) != 1 || unknown.Findings[0].ID != finding.ID || !strings.Contains(unknown.Findings[0].Message, path) {
		t.Fatal(unknown)
	}
	check.FailWhenMeasurementAbove = map[string]float64{"inodes_used_percent": 90}
	kernelFilesystems(t, map[string]primitives.FilesystemCounts{path: {Blocks: 100, BlocksAvailable: 50, BlockBytes: 4096, Inodes: 1, InodesFree: 2}})
	invalidInodes := checksandalerts.ObserveCheck(context.Background(), check)
	if invalidInodes.Status != "failed" || len(invalidInodes.Findings) != 1 || !strings.Contains(invalidInodes.Findings[0].Message, "inode counts invalid") {
		t.Fatal(invalidInodes)
	}
}
