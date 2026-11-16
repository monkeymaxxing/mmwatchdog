//go:build linux

package daemon

import (
	"context"
	"reflect"
	"testing"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/primitives"
)

func TestDiagnosticCPUDoesNotChangeScheduledBaseline(t *testing.T) {
	t.Parallel()
	path, c := daemonConfig(t)
	c.CheckGroups = map[string]checksandalerts.CheckGroup{"cpu": {Check: checksandalerts.Check{Inputs: new(checksandalerts.NodeResource("cpu"))}}}
	writeConfig(t, path, c)
	d, err := NewDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rc := d.checks["cpu/node_resource"]
	rc.memory = &checksandalerts.CheckMemory{}
	o := d.execute(context.Background(), "cpu/node_resource", rc.def, rc.memory, &checksandalerts.ModuleState{})
	if o.Status != "ok" || o.SampleWindow < checksandalerts.CPUSampleWindow {
		t.Fatal(o)
	}
	// The baseline of the scheduled runs is the sampler in the check's memory.
	scheduled := func() *checksandalerts.CPUSampler {
		return d.checks["cpu/node_resource"].memory.Value.(*checksandalerts.CPUSampler)
	}
	previous := *scheduled().Previous
	o = d.execute(context.Background(), "cpu/node_resource", rc.def, &checksandalerts.CheckMemory{}, nil)
	if o.Status != "ok" || !reflect.DeepEqual(*scheduled().Previous, previous) {
		t.Fatal("diagnostic changed scheduled sample", o)
	}
	if _, err = d.reload(); err != nil {
		t.Fatal(err)
	}
	if scheduled().Previous.At != previous.At {
		t.Fatal("unchanged reload reset baseline")
	}
	// A limit changes behavior and starts a new sampling window. No reading
	// is above 100, so a busy test machine cannot fail the check.
	group := c.CheckGroups["cpu"]
	group.FailWhenMeasurementAbove = map[string]float64{"cpu_used_percent": 100}
	c.CheckGroups["cpu"] = group
	writeConfig(t, path, c)
	if _, err = d.reload(); err != nil {
		t.Fatal(err)
	}
	rc = d.checks["cpu/node_resource"]
	if rc.memory != nil {
		t.Fatal("changed configuration kept baseline")
	}
	// A diagnostic run takes a sample of its own.
	o = d.execute(context.Background(), "cpu/node_resource", rc.def, &checksandalerts.CheckMemory{}, nil)
	if o.Status != "ok" || o.SampleWindow < checksandalerts.CPUSampleWindow {
		t.Fatal(o)
	}
}

// A node's report carries the node's CPU, memory, and root-disk use, each
// in percent, read as the check modules read them.
func TestNodeResourceMetricsOfAReport(t *testing.T) {
	t.Parallel()
	sampler := &checksandalerts.CPUSampler{}
	metrics := fleet.NodeResourceMetrics(context.Background(), sampler)
	for _, name := range []string{"cpu", "memory", "disk"} {
		if m, ok := metrics[name]; !ok || m.Unit != "percent" || m.Value < 0 || m.Value > 100 {
			t.Fatalf("%s: %+v in %+v", name, m, metrics)
		}
	}
	memory, err := checksandalerts.ReadMemoryInformation(checksandalerts.MemoryPercent)
	if err != nil || metrics["memory"].Value < memory-3 || metrics["memory"].Value > memory+3 {
		t.Fatalf("memory is %v, and /proc/meminfo says %v (%v)", metrics["memory"].Value, memory, err)
	}
	counts, err := primitives.FilesystemStatistics(context.Background(), "/")
	if used, known := counts.SpaceUsedPercent(); err != nil || !known || metrics["disk"].Value < used-1 || metrics["disk"].Value > used+1 {
		t.Fatalf("disk is %v, and the filesystem at / says %v (%v)", metrics["disk"].Value, used, err)
	}
	// The second report measures the CPU since the first, without a new
	// sample window. To tell the two apart without a clock, the test halves
	// the user time of the first report's reading: the share of the CPU
	// that the second report gives is then the one between that reading
	// and its own, and a sample of its own would give another.
	if sampler.Previous == nil {
		t.Fatal("the first report left no CPU baseline")
	}
	baseline := *sampler.Previous
	baseline.Ticks[0] /= 2
	sampler.Previous = &baseline
	again := fleet.NodeResourceMetrics(context.Background(), sampler)
	want, comparable := checksandalerts.CPUPercent(baseline, *sampler.Previous, false)
	if !comparable || again["cpu"] != (checksandalerts.Measurement{Value: want, Unit: "percent"}) || !sampler.Previous.At.After(baseline.At) {
		t.Fatalf("the second report did not measure the CPU since the first: %+v, and %v since the first report", again, want)
	}
}
