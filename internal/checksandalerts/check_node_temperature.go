package checksandalerts

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"mmwatchdog/internal/primitives"
)

// NodeTemperature is the inputs of the node_temperature check module: the
// name of one kind of temperature sensor of the machine, as the kernel
// names it, such as coretemp for the processors of Intel or nvme for NVMe
// drives. The check measures the highest temperature among the sensors of
// that name.
type NodeTemperature string

// temperatureSensorNamePattern accepts the name that a hardware monitoring
// driver gives itself.
var temperatureSensorNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func (n *NodeTemperature) Validate(InputValidation) *FieldError {
	if !temperatureSensorNamePattern.MatchString(string(*n)) {
		return &FieldError{"", "must be the name of a kind of temperature sensor, as the sensors program prints it in front of the first hyphen, such as coretemp, k10temp, or nvme"}
	}
	return nil
}

func (n *NodeTemperature) MeasurementNames() []string { return []string{"highest_temperature_celsius"} }

func (n *NodeTemperature) Run(_ context.Context, r *CheckRun) Observation {
	wanted := string(*n)
	// The kernel shows each hardware monitoring device as one directory,
	// with the driver's name in the file name and one file for each sensor,
	// such as temp1_input, which holds thousandths of a degree Celsius.
	base := primitives.SysDirectory + "/class/hwmon"
	devices, _ := primitives.ListDirectory(base)
	names, hottest, hottestSensor, sensors := []string{}, 0.0, "", 0
	unreadable := []string{}
	for _, device := range devices {
		data, err := primitives.ReadFile(base+"/"+device+"/name", primitives.ProcFileLimit)
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(data))
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
		if name != wanted {
			continue
		}
		files, err := primitives.ListDirectory(base + "/" + device)
		if err != nil {
			unreadable = append(unreadable, base+"/"+device+": "+err.Error())
			continue
		}
		for _, file := range files {
			sensor, isReading := strings.CutSuffix(file, "_input")
			if !isReading || !strings.HasPrefix(sensor, "temp") {
				continue
			}
			path := base + "/" + device + "/" + file
			data, err := primitives.ReadFile(path, primitives.ProcFileLimit)
			if err != nil {
				unreadable = append(unreadable, path+": "+err.Error())
				continue
			}
			thousandths, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
			if err != nil || math.IsNaN(thousandths) || math.IsInf(thousandths, 0) {
				unreadable = append(unreadable, path+": invalid temperature "+strconv.Quote(strings.TrimSpace(string(data))))
				continue
			}
			sensors++
			if celsius := thousandths / 1000; sensors == 1 || celsius > hottest {
				hottest, hottestSensor = celsius, sensor
				if label, err := primitives.ReadFile(base+"/"+device+"/"+sensor+"_label", primitives.ProcFileLimit); err == nil && strings.TrimSpace(string(label)) != "" {
					hottestSensor = strings.TrimSpace(string(label))
				}
			}
		}
	}
	if sensors == 0 && len(unreadable) > 0 {
		return Result("unknown", primitives.OnOneLine("MMWatchdog cannot read the temperature sensors of "+wanted+": "+strings.Join(unreadable, "; ")))
	}
	if sensors == 0 {
		slices.Sort(names)
		has := "it has no temperature sensor that the kernel knows"
		if len(names) > 0 {
			has = "the kernel knows these kinds of sensor: " + strings.Join(names, ", ")
		}
		return Result("unknown", "this machine has no temperature sensor named "+wanted+"; "+has)
	}
	// A sensor in a cold room can read below zero, which is a valid
	// temperature, so the reading takes no part in the rule that a
	// measurement is never negative.
	o := Result("ok", "")
	o.Measurements = map[string]Measurement{"highest_temperature_celsius": {hottest, "degrees Celsius"}}
	o.Status, o.Message = applyLimits(r.Check, o, n.MeasurementNames(), nil)
	o.Message += fmt.Sprintf("; the hottest of %s of %s is %s", countOf(sensors, "sensor", "sensors"), wanted, primitives.DetailLine(hottestSensor))
	if len(unreadable) > 0 {
		problem := primitives.OnOneLine("MMWatchdog cannot read the temperature sensors of " + wanted + ": " + strings.Join(unreadable, "; "))
		if o.Status != "failed" {
			o.Status, o.Message = "unknown", problem
			o.Measurements = nil
		} else {
			o.Message += "; " + problem
		}
	}
	return o
}

// nodeTemperatureDescription is what "mmwatchdog describe-module node_temperature" shows, and what
// validation knows about the module's inputs.
var nodeTemperatureDescription = ModuleDescription{
	Summary: "Measures the temperature of one kind of hardware of the machine: the highest reading among the sensors of one name.",
	UsedAs:  UsedAsCheck,
	Value:   &ModuleInput{Type: "string", Required: true, Description: "The name of the kind of sensor, as the kernel names it and as the sensors program prints it in front of the first hyphen, such as coretemp, k10temp, or nvme."},
	Measurements: []ModuleMeasurement{
		{Name: "highest_temperature_celsius", Unit: "degrees Celsius", Description: "The highest temperature among the sensors matching the configured sensor name."},
	},
	Examples: []ModuleExample{{
		Description:   "Fail when a processor of the machine is hotter than 85 degrees Celsius.",
		Configuration: "node_temperature: coretemp\nfail_when_measurement_above:\n  highest_temperature_celsius: 85\nalert_severity: warn\n",
	}},
}
