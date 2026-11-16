package checksandalerts_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/testhelpers"
)

// limited returns a copy of check with one upper limit and the alert
// severity that every check with a limit states.
func limited(check checksandalerts.Check, measurement string, above float64) checksandalerts.Check {
	check.FailWhenMeasurementAbove = map[string]float64{measurement: above}
	check.AlertSeverity = "warn"
	return check
}

func TestMeasurementLimitsAreOptionalAndStrict(t *testing.T) {
	t.Parallel()
	// Without a limit, a measurement check only records its readings.
	for _, kind := range []string{"only_record_readings: true, node_resource: cpu", "only_record_readings: true, node_resource: memory", "only_record_readings: true, node_resource: swap", "only_record_readings: true, node_resource: load", "only_record_readings: true, node_filesystem: /", "only_record_readings: true, node_filesystem: /srv"} {
		c, err := config.Load(testhelpers.ConfigFile(t, "check_groups: {metric: {"+kind+"}}"))
		if err != nil {
			t.Fatal(kind, err)
		}
		for name, co := range c.CheckGroups["metric"].Checks {
			reading := map[string]checksandalerts.Measurement{co.MeasurementNames()[0]: {Value: 95, Unit: "percent"}}
			if o := checksandalerts.MeasurementObservation(co.Check, reading, nil); !co.RecordsOnly() || o.Status != "ok" || o.Message != co.MeasurementNames()[0]+" is 95" {
				t.Fatal(name, co, o)
			}
		}
	}
	// A reading strictly above or strictly below its limit fails; a reading
	// equal to the limit passes, and zero is a valid limit.
	memory := checksandalerts.Check{Inputs: new(checksandalerts.NodeResource("memory"))}
	for _, tc := range []struct {
		value        float64
		above, below map[string]float64
		status       string
		message      string
	}{
		{10, nil, nil, "ok", "memory_used_percent is 10"},
		{10, map[string]float64{"memory_used_percent": 10}, nil, "ok", "memory_used_percent is 10 (fails above 10)"},
		{10.126, map[string]float64{"memory_used_percent": 10}, nil, "failed", "memory_used_percent is 10.13 (fails above 10)"},
		{0, map[string]float64{"memory_used_percent": 0}, nil, "ok", "memory_used_percent is 0 (fails above 0)"},
		{1, map[string]float64{"memory_used_percent": 0}, nil, "failed", "memory_used_percent is 1 (fails above 0)"},
		{10, nil, map[string]float64{"memory_used_percent": 10}, "ok", "memory_used_percent is 10 (fails below 10)"},
		{9.5, nil, map[string]float64{"memory_used_percent": 10}, "failed", "memory_used_percent is 9.5 (fails below 10)"},
		{50, map[string]float64{"memory_used_percent": 90}, map[string]float64{"memory_used_percent": 5}, "ok", "memory_used_percent is 50 (fails above 90, below 5)"},
		{math.NaN(), nil, nil, "unknown", "memory_used_percent measurement is invalid"},
	} {
		check := memory
		check.FailWhenMeasurementAbove, check.FailWhenMeasurementBelow = tc.above, tc.below
		if o := checksandalerts.MeasurementObservation(check, map[string]checksandalerts.Measurement{"memory_used_percent": {Value: tc.value, Unit: "percent"}}, nil); o.Status != tc.status || o.Message != tc.message {
			t.Fatalf("%+v: %+v", tc, o)
		}
	}
	// A check with limits on several measurements names each of them, and
	// fails when any reading passes its limit.
	load := checksandalerts.Check{Inputs: new(checksandalerts.NodeResource("load")), FailWhenMeasurementAbove: map[string]float64{"load_average_5m_per_core": 1.5, "load_average_1m": 8}}
	values, err := checksandalerts.LoadMeasurements("0.50 4.00 2.00 3/200 4000\n", 2)
	if err != nil {
		t.Fatal(err)
	}
	if o := checksandalerts.MeasurementObservation(load, values, nil); o.Status != "failed" || o.Message != "load_average_1m is 0.5 (fails above 8); load_average_5m_per_core is 2 (fails above 1.5)" || len(o.Measurements) != 6 {
		t.Fatal(o)
	}
	// A limit on a measurement that could not be made is unknown, never a
	// zero reading.
	values, _ = checksandalerts.LoadMeasurements("0.50 4.00 2.00 3/200 4000\n", 0)
	if o := checksandalerts.MeasurementObservation(load, values, map[string]string{"load_average_5m_per_core": "the number of processors is unavailable"}); o.Status != "unknown" || o.Message != "the number of processors is unavailable" || len(values) != 3 {
		t.Fatal(o)
	}
	invalid := []string{
		"check_groups: {metric: {node_resource: cpu, fail_when_measurement_above: {cpu_used_percent: 101}}}",
		"check_groups: {metric: {node_resource: memory, fail_when_measurement_above: {memory_used_percent: -1}}}",
		"check_groups: {metric: {node_resource: memory, fail_when_measurement_above: {memory_used_percent: null}}}",
		"check_groups: {metric: {node_resource: memory, fail_when_measurement_above: {memory_used_percent: '90'}}}",
		"check_groups: {metric: {node_resource: memory, fail_when_measurement_above: {}}}",
		"check_groups: {metric: {node_resource: memory, fail_when_measurement_above: {cpu_used_percent: 90}}}",
		"check_groups: {metric: {node_resource: memory, fail_when_measurement_below: {swap_used_percent: 90}}}",
		"check_groups: {metric: {node_resource: load, fail_when_measurement_above: {load_average_10m: 1}}}",
		"check_groups: {metric: {node_resource: load, fail_when_measurement_above: {load_average_1m: -1}}}",
		"check_groups: {metric: {only_record_readings: true, node_resource: iops}}",
		"check_groups: {metric: {only_record_readings: true, node_resource: disk}}",
		"check_groups: {metric: {only_record_readings: true, node_resource: {resource: memory}}}",
		"check_groups: {metric: {node_resource: memory, alert_severity: warn}}",
		"check_groups: {metric: {only_record_readings: true, node_filesystem: srv}}",
		"check_groups: {metric: {node_filesystem: /, fail_when_measurement_above: {inodes_used_percent: 101}}}",
		"check_groups: {metric: {node_filesystem: /, fail_when_measurement_above: {memory_used_percent: 90}}}",
		"check_groups: {metric: {only_record_readings: true, node_filesystem: /, raise_to_sos_after_failing_for: 1h}}",
		"check_groups: {metric: {only_record_readings: true, time_limit_per_run: 100ms, node_resource: cpu}}",
		"check_groups: {metric: {only_record_readings: true, systemd_unit_resource: {resource: memory}}}",
		"check_groups: {metric: {only_record_readings: true, systemd_unit_resource: {systemd_unit: app, resource: memory}}}",
		"check_groups: {metric: {only_record_readings: true, systemd_unit_resource: {systemd_unit: app.service}}}",
		"check_groups: {metric: {systemd_unit_resource: {systemd_unit: app.service, resource: cpu}, fail_when_measurement_above: {cpu_used_percent: 101}}}",
		"check_groups: {metric: {systemd_unit_resource: {systemd_unit: app.service, resource: cpu}, fail_when_measurement_above: {memory_used_mib: 100}}}",
		"check_groups: {metric: {only_record_readings: true, systemd_unit_resource: {systemd_unit: app.service, resource: disk}}}",
		"check_groups: {app: {checks: {cpu: {only_record_readings: true, systemd_unit_resource: {systemd_unit: app.service, resource: cpu}}}, restart_systemd_unit: {max_attempts: 3, min_time_between_attempts: 1m, reset_attempts_after_healthy_for: 10m, systemd_unit: app.service, triggered_by_checks: [cpu]}}}",
		// Only a check that measures a number takes a limit, and a limit is
		// written on the check, not on a check group of named checks.
		"check_groups: {metric: {shell: 'true', fail_when_measurement_above: {exit_code: 0}}}",
		"check_groups: {metric: {tcp: '127.0.0.1:80', fail_when_measurement_below: {connections: 1}}}",
		"check_groups: {metric: {fail_when_measurement_above: {memory_used_percent: 90}, checks: {memory: {only_record_readings: true, node_resource: memory}}}}",
		// A check whose module is not core takes limits on the measurements
		// that the module's description lists, and on no other.
		"check_groups: {metric: {queue: {}, fail_when_measurement_above: {consumers: 5}}}",
		"check_groups: {metric: {unmeasured: {}, fail_when_measurement_above: {queued_jobs: 5}}}",
	}
	modules := t.TempDir()
	testhelpers.InstallModule(t, modules, "queue", testhelpers.TestModuleDescription("queue", checksandalerts.UsedAsCheck, nil, map[string]string{"queued_jobs": "count", "workers": "count"}), "true")
	testhelpers.InstallModule(t, modules, "unmeasured", testhelpers.TestModuleDescription("unmeasured", checksandalerts.UsedAsCheck, nil, nil), "true")
	for _, text := range invalid {
		if _, err := config.LoadWithModuleDirs(testhelpers.ConfigFile(t, text), checksandalerts.ModuleDirs{External: modules}); err == nil {
			t.Fatal("accepted", text)
		}
	}
	for text, want := range map[string]string{
		"check_groups: {metric: {queue: {}, fail_when_measurement_above: {consumers: 5}}}":        "check_groups.metric.fail_when_measurement_above.consumers: this check has no such measurement; it measures queued_jobs, workers",
		"check_groups: {metric: {unmeasured: {}, fail_when_measurement_above: {queued_jobs: 5}}}": "check_groups.metric.fail_when_measurement_above.queued_jobs: the module unmeasured reports no measurement, so its check takes no limit",
		"check_groups: {metric: {shell: 'true', fail_when_measurement_above: {exit_code: 0}}}":    "check_groups.metric.fail_when_measurement_above: the module shell reports no measurement, so its check takes no limit",
	} {
		if _, err := config.LoadWithModuleDirs(testhelpers.ConfigFile(t, text), checksandalerts.ModuleDirs{External: modules}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s:\n got %v\nwant %q", text, err, want)
		}
	}
	c, err := config.LoadWithModuleDirs(testhelpers.ConfigFile(t, `check_groups:
  app:
    checks:
      cpu: {only_record_readings: true, systemd_unit_resource: {systemd_unit: app.service, resource: cpu}}
      ram: {systemd_unit_resource: {systemd_unit: app.service, resource: memory}, fail_when_measurement_above: {memory_used_mib: 512}}
      load: {node_resource: load, fail_when_measurement_above: {load_average_5m_per_core: 1.5}}
      space: {node_filesystem: /, fail_when_measurement_below: {space_free_gib: 10}, fail_when_measurement_above: {space_used_percent: 90, inodes_used_percent: 90}}
      mounted_writable: {node_filesystem: /srv, alert_severity: sos}
      queue: {queue: {}, fail_when_measurement_above: {queued_jobs: 500}, fail_when_measurement_below: {workers: 1}}
`), checksandalerts.ModuleDirs{External: modules})
	if err != nil {
		t.Fatal(err)
	}
	co := c.CheckGroups["app"].Checks
	if co["ram"].SystemdUnitName() != "app.service" || co["ram"].FailWhenMeasurementAbove["memory_used_mib"] != 512 || co["ram"].RecordsOnly() || !co["cpu"].RecordsOnly() {
		t.Fatal(co)
	}
	// A filesystem check with an alert_severity and no limit can still fail,
	// so it does not only record.
	if co["mounted_writable"].RecordsOnly() || co["mounted_writable"].HasLimit() || co["space"].FailWhenMeasurementBelow["space_free_gib"] != 10 {
		t.Fatal(co)
	}
}

func TestCPUPercentUsesDeltasAndMachineCapacity(t *testing.T) {
	t.Parallel()
	text := "cpu 100 20 40 800 10 5 5 20 90 10\ncpu0 0\ncpu1 0\n"
	a, err := checksandalerts.ParseCPUStat(text)
	if err != nil {
		t.Fatal(err)
	}
	a.At = time.Now()
	b := a
	b.At = a.At.Add(time.Second)
	// 100 busy, 70 idle, 10 I/O wait and 20 stolen ticks = 50% busy.
	b.Ticks[0] += 100
	b.Ticks[3] += 70
	b.Ticks[4] += 10
	b.Ticks[7] += 20
	if got, ok := checksandalerts.CPUPercent(a, b, false); !ok || got != 50 {
		t.Fatal(got, ok)
	}
	a.Invocation = "first"
	a.Usage = 10_000_000_000
	b.Invocation = "first"
	b.Usage = a.Usage + 1_000_000_000
	if got, ok := checksandalerts.CPUPercent(a, b, true); !ok || got != 50 {
		t.Fatal(got, ok)
	}
	b.Invocation = "restarted"
	if _, ok := checksandalerts.CPUPercent(a, b, true); ok {
		t.Fatal("used counters across service invocations")
	}
	b = a
	b.At = a.At.Add(time.Second)
	b.Ticks[0]--
	if _, ok := checksandalerts.CPUPercent(a, b, false); ok {
		t.Fatal("accepted backward CPU counter")
	}
	b = a
	b.At = a.At.Add(time.Second)
	b.Ticks[0] += 50
	b.Ticks[3] += 50
	b.Ticks[4]--
	if got, ok := checksandalerts.CPUPercent(a, b, false); !ok || got != 50 {
		t.Fatal("backward iowait", got, ok)
	}
	b.Processors = 3
	if _, ok := checksandalerts.CPUPercent(a, b, false); ok {
		t.Fatal("accepted changed processor count")
	}
	for _, bad := range []string{"", "cpu 1 2 3 4\ncpu0 0", "cpu bad 2 3 4 5 6 7 8\ncpu0 0", "cpu 1 2 3 4 5 6 7 8"} {
		if _, err := checksandalerts.ParseCPUStat(bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
}

func TestCPUSamplerBootstrapsResetsAndHonorsCancellation(t *testing.T) {
	t.Parallel()
	base := time.Now().Add(-time.Minute)
	index := 0
	read := func(context.Context) (primitives.CPUReading, error) {
		index++
		return primitives.CPUReading{Processors: 2, Invocation: "one", Usage: uint64(index) * 1_000_000_000, At: base.Add(time.Duration(index) * time.Second)}, nil
	}
	s := &checksandalerts.CPUSampler{}
	check := checksandalerts.Check{Inputs: &checksandalerts.SystemdUnitResource{SystemdUnit: "app.service", Resource: "cpu"}}
	first := s.Observe(context.Background(), check, read, "cpu_used_percent", true)
	if index != 2 || first.Status != "ok" || first.SampleWindow != time.Second || first.Measurements["cpu_used_percent"].Value != 50 || first.Message != "cpu_used_percent is 50 over 1.000s" {
		t.Fatal(index, first)
	}
	next := s.Observe(context.Background(), limited(check, "cpu_used_percent", 40), read, "cpu_used_percent", true)
	if index != 3 || next.SampleWindow != time.Second || next.Status != "failed" || next.Message != "cpu_used_percent is 50 (fails above 40) over 1.000s" {
		t.Fatal(index, next)
	}
	s.Previous.Invocation = "old"
	next = s.Observe(context.Background(), check, read, "cpu_used_percent", true)
	if index != 5 || next.Status != "ok" || next.Measurements["cpu_used_percent"].Value != 50 {
		t.Fatal(index, next)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Previous = nil
	o := s.Observe(ctx, check, read, "cpu_used_percent", true)
	if o.Status != "unknown" || s.Previous != nil {
		t.Fatal(o)
	}
	o = s.Observe(context.Background(), check, func(context.Context) (primitives.CPUReading, error) {
		return primitives.CPUReading{}, fmt.Errorf("missing counters")
	}, "cpu_used_percent", true)
	if o.Status != "unknown" || s.Previous != nil {
		t.Fatal(o)
	}
}

func TestResourceInputsNeverInventZeroUsage(t *testing.T) {
	t.Parallel()
	if v, err := checksandalerts.MemoryPercent("MemTotal: 1000 kB\nMemAvailable: 250 kB\n"); err != nil || v != 75 {
		t.Fatal(v, err)
	}
	for _, bad := range []string{"MemTotal: 1000 kB", "MemTotal: 0 kB\nMemAvailable: 0 kB", "MemTotal: 10 kB\nMemAvailable: 20 kB", "MemTotal: no kB\nMemAvailable: 0 kB"} {
		if _, err := checksandalerts.MemoryPercent(bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	properties := func(activeState string, more ...string) map[string]string {
		fields := map[string]string{"LoadState": "loaded", "ActiveState": activeState, "InvocationID": "instance"}
		for i := 0; i < len(more); i += 2 {
			fields[more[i]] = more[i+1]
		}
		return fields
	}
	// A unit that runs, active or reloading, has a reading.
	for _, state := range []string{"active", "reloading"} {
		r, value, err := checksandalerts.UsageOfProperties(properties(state, "CPUUsageNSec", "123", "MemoryCurrent", "67108864"), "app.service", "memory")
		if err != nil || value != 64 {
			t.Fatal(state, r, value, err)
		}
		r, _, err = checksandalerts.UsageOfProperties(properties(state, "CPUUsageNSec", "123"), "app.service", "cpu")
		if err != nil || r.Usage != 123 || r.Invocation != "instance" {
			t.Fatal(state, r, err)
		}
	}
	for _, bad := range []map[string]string{properties("active"), properties("active", "MemoryCurrent", "[not set]"), properties("active", "MemoryCurrent", "18446744073709551615")} {
		if _, _, err := checksandalerts.UsageOfProperties(bad, "app.service", "memory"); err == nil {
			t.Fatal("invented measurement", bad)
		}
	}
	// A unit in any other state does not run and has no reading; the
	// message says its state.
	for _, state := range []string{"activating", "deactivating", "inactive", "failed"} {
		_, _, err := checksandalerts.UsageOfProperties(properties(state, "MemoryCurrent", "0"), "app.service", "memory")
		if err == nil || err.Error() != "systemd unit app.service usage unavailable: the unit is "+state {
			t.Fatal("invented measurement", state, err)
		}
	}
}

func TestSwapLoadAndFilesystemReadings(t *testing.T) {
	t.Parallel()
	if v, err := checksandalerts.SwapPercent("MemTotal: 1000 kB\nSwapTotal: 2000 kB\nSwapFree: 1500 kB\n"); err != nil || v != 25 {
		t.Fatal(v, err)
	}
	if v, err := checksandalerts.SwapPercent("SwapTotal: 0 kB\nSwapFree: 0 kB\n"); err != nil || v != 0 {
		t.Fatal("host without swap", v, err)
	}
	for _, bad := range []string{"MemTotal: 1000 kB", "SwapTotal: 10 kB\nSwapFree: 20 kB", "SwapTotal: 10 MB\nSwapFree: 0 kB"} {
		if _, err := checksandalerts.SwapPercent(bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	for window, want := range map[string]float64{"1m": 0.5, "5m": 1.25, "15m": 2} {
		if v, err := checksandalerts.LoadAverage("0.50 1.25 2.00 3/200 4000\n", window); err != nil || v != want {
			t.Fatal(window, v, err)
		}
	}
	for _, bad := range []string{"", "0.50 1.25", "0.50 bad 2.00 3/200 4000"} {
		if _, err := checksandalerts.LoadAverage(bad, "5m"); err == nil {
			t.Fatal("accepted", bad)
		}
	}

	values, err := checksandalerts.LoadMeasurements("0.50 1.25 2.00 3/200 4000\n", 4)
	if err != nil || len(values) != 6 || values["load_average_5m"] != (checksandalerts.Measurement{Value: 1.25, Unit: "load"}) || values["load_average_15m_per_core"] != (checksandalerts.Measurement{Value: 0.5, Unit: "load per core"}) {
		t.Fatal(values, err)
	}
	if _, err := checksandalerts.LoadMeasurements("0.50 1.25", 4); err == nil {
		t.Fatal("accepted a short /proc/loadavg")
	}

	// 1000 blocks of 1 MiB with 250 left, and 400 inodes with 100 left.
	counts := primitives.FilesystemCounts{Blocks: 1000, BlocksAvailable: 250, BlockBytes: 1024 * 1024, Inodes: 400, InodesFree: 100}
	recording := checksandalerts.Check{Inputs: new(checksandalerts.NodeFilesystem("/srv/data"))}
	space := limited(recording, "space_used_percent", 85)
	o := checksandalerts.FilesystemObservation(space, "/srv/data", counts)
	if o.Status != "ok" || o.Message != "space_used_percent is 75 (fails above 85)" || o.Measurements["space_used_percent"] != (checksandalerts.Measurement{Value: 75, Unit: "percent"}) || o.Measurements["inodes_used_percent"] != (checksandalerts.Measurement{Value: 75, Unit: "percent"}) || o.Measurements["space_free_gib"] != (checksandalerts.Measurement{Value: 250.0 / 1024, Unit: "GiB"}) {
		t.Fatal(o)
	}
	full := counts
	full.BlocksAvailable = 100
	if o := checksandalerts.FilesystemObservation(space, "/srv/data", full); o.Status != "failed" || o.Message != "space_used_percent is 90 (fails above 85)" {
		t.Fatal(o)
	}
	free := recording
	free.FailWhenMeasurementBelow, free.AlertSeverity = map[string]float64{"space_free_gib": 1}, "sos"
	if o := checksandalerts.FilesystemObservation(free, "/srv/data", counts); o.Status != "failed" || o.Message != "space_free_gib is 0.24 (fails below 1)" {
		t.Fatal(o)
	}
	if o := checksandalerts.FilesystemObservation(recording, "/srv/data", counts); o.Status != "ok" || o.Message != "space_used_percent is 75; inodes_used_percent is 75; space_free_gib is 0.24" {
		t.Fatal(o)
	}
	// Read-only fails a check that states an alert_severity, below its limit
	// and without a limit too. A check that only records notes it. All of
	// them record the readings.
	readOnly := counts
	readOnly.ReadOnly = true
	mounted := recording
	mounted.AlertSeverity = "sos"
	for _, tc := range []struct {
		check  checksandalerts.Check
		status string
	}{{space, "failed"}, {mounted, "failed"}, {recording, "ok"}} {
		o := checksandalerts.FilesystemObservation(tc.check, "/srv/data", readOnly)
		if o.Status != tc.status || !strings.HasPrefix(o.Message, "/srv/data is mounted read-only; space_used_percent is 75") || o.Measurements["space_used_percent"].Value != 75 {
			t.Fatal(o)
		}
	}
	// A filesystem without a fixed inode count, such as btrfs, has no
	// inodes_used_percent: a limit on it is unknown, and the other readings
	// are still made.
	noInodes := counts
	noInodes.Inodes, noInodes.InodesFree = 0, 0
	inodes := limited(recording, "inodes_used_percent", 90)
	if o := checksandalerts.FilesystemObservation(inodes, "/srv/data", counts); o.Status != "ok" || o.Message != "inodes_used_percent is 75 (fails above 90)" {
		t.Fatal(o)
	}
	if o := checksandalerts.FilesystemObservation(inodes, "/srv/data", noInodes); o.Status != "unknown" || o.Message != "/srv/data has no fixed inode count, so inodes_used_percent cannot be measured" {
		t.Fatal(o)
	}
	if o := checksandalerts.FilesystemObservation(space, "/srv/data", noInodes); o.Status != "ok" || len(o.Measurements) != 2 {
		t.Fatal(o)
	}
	if o := checksandalerts.FilesystemObservation(recording, "/srv/data", noInodes); o.Status != "ok" || o.Message != "space_used_percent is 75; space_free_gib is 0.24" {
		t.Fatal(o)
	}
	noInodes.ReadOnly = true
	if o := checksandalerts.FilesystemObservation(inodes, "/srv/data", noInodes); o.Status != "failed" || !strings.Contains(o.Message, "read-only") {
		t.Fatal(o)
	}
	if o := checksandalerts.FilesystemObservation(space, "/srv/data", primitives.FilesystemCounts{Blocks: 10, BlocksAvailable: 20, BlockBytes: 4096}); o.Status != "unknown" || o.Measurements != nil {
		t.Fatal(o)
	}
}

func TestRecordingOnlyTransitionRetiresLimitIncident(t *testing.T) {
	t.Parallel()
	c, err := config.Load(testhelpers.ConfigFile(t, "check_groups: {cpu: {alert_after_failing_for: 0s, node_resource: cpu, fail_when_measurement_above: {cpu_used_percent: 80}}}"))
	if err != nil {
		t.Fatal(err)
	}
	snap := incidents.FreshSnapshot()
	now := time.Now()
	snap.Reconcile(c, config.CheckDefinitions(c), now)
	st := snap.Checks["cpu/node_resource"]
	check := c.CheckGroups["cpu"].Checks["node_resource"].Check
	reading := map[string]checksandalerts.Measurement{"cpu_used_percent": {Value: 95, Unit: "percent"}}
	observation := checksandalerts.MeasurementObservation(check, reading, nil)
	observation.At = now
	st.Observe(observation, c.CheckGroups["cpu"].Timing, false)
	id := st.FailureIncident.Incident
	if id == "" || st.RecordsOnly {
		t.Fatal(st)
	}
	check.FailWhenMeasurementAbove, check.AlertSeverity = nil, ""
	check.OnlyRecordReadings = true
	c.CheckGroups["cpu"].Checks["node_resource"] = checksandalerts.NamedCheck{Check: check}
	events := snap.Reconcile(c, config.CheckDefinitions(c), now.Add(time.Second))
	st = snap.Checks["cpu/node_resource"]
	if len(events) != 1 || events[0].Type != "closed_without_recovery" || events[0].Incident != id || !st.RecordsOnly {
		t.Fatal(events, st)
	}
	observation = checksandalerts.MeasurementObservation(check, reading, nil)
	observation.At = now.Add(2 * time.Second)
	if events = st.Observe(observation, c.CheckGroups["cpu"].Timing, false); len(events) != 0 || st.Summary(time.Now(), false) != "ok" {
		t.Fatal(events, st)
	}
	// A check that only records never alerts: a reading it cannot make
	// shows as unknown in status, without an incident.
	events = st.Observe(checksandalerts.Observation{Status: "unknown", Message: "cannot read counters", At: now.Add(3 * time.Second)}, c.CheckGroups["cpu"].Timing, false)
	if len(events) != 0 || st.CannotCheckIncident.Incident != "" || st.Summary(time.Now(), false) != "unknown" {
		t.Fatal(events, st)
	}
}
