package checksandalerts

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mmwatchdog/internal/primitives"
)

// NodeNetworkInterface is the inputs of the node_network_interface check
// module: the name of the network interface that the check measures, such
// as eth0. A check that states an alert_severity also fails while the
// interface has no link.
type NodeNetworkInterface string

// NodeNetworkInterfaceMeasurements are what a node_network_interface check
// measures.
var NodeNetworkInterfaceMeasurements = []string{"link_speed_mbit_per_second", "received_mbit_per_second", "sent_mbit_per_second"}

// networkInterfacePattern accepts the name of a network interface: at most
// 15 characters, without a slash and without white space.
var networkInterfacePattern = regexp.MustCompile(`^[A-Za-z0-9_.:@-]{1,15}$`)

func (n *NodeNetworkInterface) Validate(InputValidation) *FieldError {
	if name := string(*n); !networkInterfacePattern.MatchString(name) || name == "." || name == ".." {
		return &FieldError{"", "must be the name of a network interface, such as eth0"}
	}
	return nil
}

func (n *NodeNetworkInterface) MeasurementNames() []string { return NodeNetworkInterfaceMeasurements }

// TimeOfEachRun is the sample of the first run: a rate needs two readings of
// the kernel's counters.
func (n *NodeNetworkInterface) TimeOfEachRun() (time.Duration, string) {
	return CPUSampleWindow, "the initial sample of the network interface"
}

func (n *NodeNetworkInterface) Run(ctx context.Context, r *CheckRun) Observation {
	name := string(*n)
	directory := primitives.SysDirectory + "/class/net/" + name
	if !primitives.IsDirectory(directory) {
		return Result("unknown", "network interface "+name+" does not exist")
	}
	number := func(file string) (uint64, error) {
		data, err := primitives.ReadFile(directory+"/"+file, primitives.ProcFileLimit)
		if err != nil {
			return 0, err
		}
		return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	}
	read := func() ([]uint64, error) {
		received, err := number("statistics/rx_bytes")
		if err != nil {
			return nil, fmt.Errorf("network interface %s: the count of received bytes is unavailable: %w", name, err)
		}
		sent, err := number("statistics/tx_bytes")
		if err != nil {
			return nil, fmt.Errorf("network interface %s: the count of sent bytes is unavailable: %w", name, err)
		}
		return []uint64{received, sent}, nil
	}
	before, now, window, problem := r.CounterReadings(ctx, read)
	if problem != "" {
		return Result("unknown", problem)
	}
	megabitsPerSecond := func(counter int) float64 {
		return float64(now[counter]-before[counter]) * 8 / 1e6 / window.Seconds()
	}
	values := map[string]Measurement{
		"received_mbit_per_second": {megabitsPerSecond(0), "Mbit per second"},
		"sent_mbit_per_second":     {megabitsPerSecond(1), "Mbit per second"},
	}
	// The kernel gives no speed for an interface that has none, as the
	// interfaces of many virtual machines, and for one without a link.
	unavailable := map[string]string{}
	if speed, err := number("speed"); err == nil && speed > 0 {
		values["link_speed_mbit_per_second"] = Measurement{float64(speed), "Mbit per second"}
	} else {
		unavailable["link_speed_mbit_per_second"] = "network interface " + name + " reports no link speed, so link_speed_mbit_per_second cannot be measured"
	}
	o := MeasurementObservation(r.Check, values, unavailable)
	o.SampleWindow = window
	if o.Status != "unknown" {
		o.Message += fmt.Sprintf(" over %.3fs", window.Seconds())
	}
	// The kernel writes 1 into the file carrier while the interface has a
	// link, and gives no answer for an interface that is turned off.
	if carrier, err := number("carrier"); err != nil || carrier != 1 {
		// Without a link, a check that states an alert_severity fails, with
		// or without a limit, also when its limit is on the link speed,
		// which an interface without a link does not report. A check that
		// only records notes it in its message.
		noLink := "network interface " + name + " has no link"
		switch {
		case r.Check.AlertSeverity != "" && o.Status == "unknown":
			o.Status, o.Message = "failed", noLink
		case r.Check.AlertSeverity != "":
			o.Status, o.Message = "failed", noLink+"; "+o.Message
		default:
			o.Message = noLink + "; " + o.Message
		}
	}
	return o
}

// nodeNetworkInterfaceDescription is what "mmwatchdog describe-module node_network_interface" shows, and what
// validation knows about the module's inputs.
var nodeNetworkInterfaceDescription = ModuleDescription{
	Summary: "Measures one network interface: the speed of its link, and the data that it receives and sends. With an alert severity, the check also fails while the interface has no link.",
	UsedAs:  UsedAsCheck,
	Value:   &ModuleInput{Type: "string", Required: true, Description: "The name of the network interface, such as eth0."},
	Measurements: []ModuleMeasurement{
		{Name: "link_speed_mbit_per_second", Unit: "Mbit per second", Description: "The speed that the interface and the device at the other end of the cable agreed on, in megabits per second. An interface that reports no speed has no such measurement."},
		{Name: "received_mbit_per_second", Unit: "Mbit per second", Description: "The data that the interface received since the run before."},
		{Name: "sent_mbit_per_second", Unit: "Mbit per second", Description: "The data that the interface sent since the run before."},
	},
	Examples: []ModuleExample{{
		Description:   "Fail while the interface eth0 has no link, and while its link is slower than 1000 megabits per second.",
		Configuration: "node_network_interface: eth0\nfail_when_measurement_below:\n  link_speed_mbit_per_second: 1000\nalert_severity: sos\n",
	}},
}
