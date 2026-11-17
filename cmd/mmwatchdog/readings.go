package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/store"
)

type readingsOptions struct {
	onHub, asJSON, chart                          bool
	configDir, node, nodeGroup, checkGroup, check string
	since, until                                  time.Time
}

func parseReadingsOptions(args []string, onHub bool, now time.Time) (readingsOptions, error) {
	options := readingsOptions{onHub: onHub}
	flags := flag.NewFlagSet("readings", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var last, since, until string
	flags.StringVar(&options.configDir, "config-dir", "/etc/mmwatchdog", "configuration directory containing state_dir")
	flags.StringVar(&last, "last", "1h", "readings from the last duration, such as 30s, 5m, 26h, or 7d")
	flags.StringVar(&since, "since", "", "readings from the RFC 3339 time given by --since")
	flags.StringVar(&until, "until", "", "readings through the RFC 3339 time given by --until; requires --since")
	flags.BoolVar(&options.asJSON, "json", false, "print readings and summaries as JSON")
	flags.BoolVar(&options.chart, "chart", false, "draw a chart of each measurement instead of listing its readings")
	if onHub {
		flags.StringVar(&options.nodeGroup, "node-group", "", "readings of every node in the node group named by --node-group")
	}
	names := []string{}
	// Go's flag parser stops at a positional argument. Parse each remaining
	// segment so both documented placements of --node-group work.
	for len(args) > 0 {
		if strings.HasPrefix(args[0], "-") {
			if err := flags.Parse(args); err != nil {
				if errors.Is(err, flag.ErrHelp) {
					return options, flag.ErrHelp
				}
				return options, fmt.Errorf("%w; use mmwatchdog --help for the readings command forms", err)
			}
			args = flags.Args()
		} else {
			names = append(names, args[0])
			args = args[1:]
		}
	}
	if options.chart && options.asJSON {
		return options, fmt.Errorf("--chart and --json cannot be used together; choose one output format")
	}
	var err error
	options.since, options.until, err = parseTimeOptions(flags, last, since, until, now)
	if err != nil {
		return options, err
	}
	if visited(flags, "node-group") && options.nodeGroup == "" {
		return options, fmt.Errorf("--node-group needs a node group name; use mmwatchdog fleet to see the node groups")
	}
	if onHub && options.nodeGroup == "" {
		if len(names) < 2 || len(names) > 3 {
			return options, fmt.Errorf("fleet readings needs NODE CHECK_GROUP [CHECK]; use mmwatchdog --help for the command forms")
		}
		options.node, names = names[0], names[1:]
	} else if options.nodeGroup != "" && len(names) != 2 {
		return options, fmt.Errorf("fleet readings --node-group needs CHECK_GROUP CHECK; name one check to compare across nodes")
	}
	if len(names) < 1 || len(names) > 2 {
		return options, fmt.Errorf("readings needs CHECK_GROUP [CHECK]; use mmwatchdog status to see the check names")
	}
	options.checkGroup = names[0]
	if len(names) == 2 {
		options.check = names[1]
	}
	return options, nil
}

// Each point contains the limits of its own run; summaries never apply a
// current configuration's limits to an earlier value.
type readingPoint struct {
	At time.Time `json:"at"`
	store.MeasurementReading
	LimitStatus string `json:"limit_status"`
}
type readingsSummary struct {
	ReadingCount        int          `json:"reading_count"`
	Lowest              readingPoint `json:"lowest"`
	Highest             readingPoint `json:"highest"`
	Latest              readingPoint `json:"latest"`
	ReadingsBeyondLimit int          `json:"readings_beyond_limit"`
}
type measurementReadings struct {
	CheckGroup  string          `json:"check_group"`
	Check       string          `json:"check"`
	Measurement string          `json:"measurement"`
	Readings    []readingPoint  `json:"readings"`
	Summary     readingsSummary `json:"summary"`
}
type nodeReadings struct {
	Node                string                `json:"node,omitempty"`
	Measurements        []measurementReadings `json:"measurements"`
	WhyNoReadings       string                `json:"why_no_readings,omitempty"`
	ReadingsAreHealthy  bool                  `json:"readings_are_healthy"`
	readingsBeyondLimit int
	farthestBeyondLimit float64
}
type readingsResult struct {
	Since                         time.Time      `json:"since"`
	Until                         time.Time      `json:"until"`
	CheckGroup                    string         `json:"check_group"`
	Check                         string         `json:"check,omitempty"`
	NodeGroup                     string         `json:"node_group,omitempty"`
	Nodes                         []nodeReadings `json:"nodes"`
	NodesWithHealthyReadingsCount int            `json:"nodes_with_healthy_readings_count"`
}

func readingLimitStatus(reading store.MeasurementReading) (string, float64) {
	if reading.FailWhenMeasurementAbove != nil && reading.Value > *reading.FailWhenMeasurementAbove {
		return "above the limit", reading.Value - *reading.FailWhenMeasurementAbove
	}
	if reading.FailWhenMeasurementBelow != nil && reading.Value < *reading.FailWhenMeasurementBelow {
		return "below the limit", *reading.FailWhenMeasurementBelow - reading.Value
	}
	if reading.FailWhenMeasurementAbove != nil || reading.FailWhenMeasurementBelow != nil {
		return "within the limits", 0
	}
	return "only records", 0
}

func summarizeReadings(node string, runs []store.RunReadings) nodeReadings {
	result := nodeReadings{Node: node, Measurements: []measurementReadings{}, ReadingsAreHealthy: len(runs) > 0}
	// The index of each check's measurement in result.Measurements.
	blocks := map[string]int{}
	for _, run := range runs {
		for _, name := range checksandalerts.Names(run.Measurements) {
			reading := run.Measurements[name]
			key := run.Check + "/" + name
			index, exists := blocks[key]
			if !exists {
				index = len(result.Measurements)
				blocks[key] = index
				result.Measurements = append(result.Measurements, measurementReadings{CheckGroup: run.CheckGroup, Check: run.Check, Measurement: name, Readings: []readingPoint{}})
			}
			block := &result.Measurements[index]
			status, distance := readingLimitStatus(reading)
			point := readingPoint{At: run.At, MeasurementReading: reading, LimitStatus: status}
			block.Readings = append(block.Readings, point)
			summary := &block.Summary
			if summary.ReadingCount == 0 || point.Value < summary.Lowest.Value {
				summary.Lowest = point
			}
			if summary.ReadingCount == 0 || point.Value > summary.Highest.Value {
				summary.Highest = point
			}
			summary.Latest = point
			summary.ReadingCount++
			if distance > 0 {
				summary.ReadingsBeyondLimit++
				result.readingsBeyondLimit++
				result.farthestBeyondLimit = math.Max(result.farthestBeyondLimit, distance)
				result.ReadingsAreHealthy = false
			}
		}
	}
	sort.Slice(result.Measurements, func(left, right int) bool {
		a, b := result.Measurements[left], result.Measurements[right]
		return a.Check+"/"+a.Measurement < b.Check+"/"+b.Measurement
	})
	return result
}

func showReadings(options readingsOptions) error {
	configuration, err := config.Load(options.configDir)
	if err != nil {
		return err
	}
	nodes := []fleet.ReadingNode{{}}
	historyFile := store.MeasurementHistoryFile
	if options.onHub {
		if configuration.FleetHubRole == nil {
			return fmt.Errorf("fleet readings runs on the fleet hub; run this command on the node with fleet_hub_role")
		}
		nodes, err = fleet.ReadReadingNodes(filepath.Join(configuration.StateDir, "fleet.db"), options.node, options.nodeGroup)
		if err != nil {
			return err
		}
		historyFile = store.MeasurementHistoryOfNodesFile
	}
	result := readingsResult{Since: options.since, Until: options.until, CheckGroup: options.checkGroup, Check: options.check, NodeGroup: options.nodeGroup, Nodes: []nodeReadings{}}
	for _, node := range nodes {
		selectedFile, historyNode := historyFile, node.Name
		if node.OwnHistory {
			selectedFile, historyNode = store.MeasurementHistoryFile, ""
		}
		runs, catalog, err := store.ReadMeasurementHistory(filepath.Join(configuration.StateDir, selectedFile), historyNode, options.checkGroup, options.check, options.since, options.until)
		if err != nil {
			return err
		}
		groupExists, checkExists, recordsMeasurements := false, false, false
		for _, entry := range catalog {
			if entry.CheckGroup == options.checkGroup {
				groupExists = true
				if options.check == "" || entry.Check == options.check {
					checkExists, recordsMeasurements = true, true
				}
			}
		}
		if !options.onHub || node.OwnHistory {
			_, configured := configuration.CheckGroups[options.checkGroup]
			groupExists = groupExists || configured
			for _, definition := range config.CheckDefinitions(configuration) {
				if definition.CheckGroup == options.checkGroup && (options.check == "" || definition.Name == options.check) {
					checkExists = true
					recordsMeasurements = recordsMeasurements || definition.Check.KeepsReadings()
				}
			}
		} else if node.Report != nil {
			for key := range node.Report.Checks {
				group, check, _ := strings.Cut(key, "/")
				if group == options.checkGroup {
					groupExists = true
					if options.check == "" || check == options.check {
						checkExists = true
					}
				}
			}
		}
		var problem string
		switch {
		case !groupExists:
			problem = fmt.Sprintf("unknown check group %q; use mmwatchdog status or mmwatchdog fleet NODE to see the check groups", options.checkGroup)
		case !checkExists:
			problem = fmt.Sprintf("unknown check %q in check group %q; use mmwatchdog status or mmwatchdog fleet NODE to see the checks", options.check, options.checkGroup)
		case !recordsMeasurements && (!options.onHub || node.OwnHistory) && options.check == "":
			problem = fmt.Sprintf("check group %q records no measurements; choose a check group with a check that measures a number", options.checkGroup)
		case !recordsMeasurements && (!options.onHub || node.OwnHistory):
			problem = fmt.Sprintf("check %q in check group %q records no measurements; choose a check that measures a number", options.check, options.checkGroup)
		case !recordsMeasurements:
			problem = "the hub has no recorded measurements for this check; choose a measuring check or let the node send its readings"
		}
		if problem != "" && options.nodeGroup == "" {
			if options.onHub {
				return fmt.Errorf("node %q: %s", node.Name, problem)
			}
			return fmt.Errorf("%s", problem)
		}
		summary := summarizeReadings(node.Name, runs)
		if problem != "" {
			summary.WhyNoReadings, summary.ReadingsAreHealthy = problem, false
		} else if len(runs) == 0 {
			summary.WhyNoReadings = "no readings in this time span; choose a longer --last or an earlier --since"
		}
		if summary.ReadingsAreHealthy {
			result.NodesWithHealthyReadingsCount++
		}
		result.Nodes = append(result.Nodes, summary)
	}
	if options.nodeGroup != "" {
		sort.SliceStable(result.Nodes, func(left, right int) bool {
			a, b := result.Nodes[left], result.Nodes[right]
			if a.readingsBeyondLimit != b.readingsBeyondLimit {
				return a.readingsBeyondLimit > b.readingsBeyondLimit
			}
			if a.farthestBeyondLimit != b.farthestBeyondLimit {
				return a.farthestBeyondLimit > b.farthestBeyondLimit
			}
			if a.ReadingsAreHealthy != b.ReadingsAreHealthy {
				return !a.ReadingsAreHealthy
			}
			return a.Node < b.Node
		})
	}
	if options.asJSON {
		return printJSON(result)
	}
	width, terminal := terminalSize(os.Stdout)
	var chart *readingsChartOptions
	if options.chart {
		chart = &readingsChartOptions{width: width}
	}
	printReadings(cleaned(result), chart, chartColor(terminal, os.Getenv("NO_COLOR")))
	return nil
}

// textNumber keeps four significant digits where decimals are needed, without exponents.
func textNumber(value float64) string {
	if value == 0 {
		return "0"
	}
	decimals := max(0, 3-int(math.Floor(math.Log10(math.Abs(value)))))
	number := strconv.FormatFloat(value, 'f', decimals, 64)
	if strings.Contains(number, ".") {
		number = strings.TrimRight(strings.TrimRight(number, "0"), ".")
	}
	if number == "-0" {
		return "0"
	}
	return number
}

func textTime(t, since, until time.Time) string {
	format := "2006-01-02 15:04:05"
	since, until = since.UTC(), until.UTC()
	if since.Format("2006-01-02") == until.Format("2006-01-02") {
		format = "15:04:05"
	} else if since.Year() == until.Year() {
		format = "Jan 2 15:04:05"
	}
	return t.UTC().Format(format)
}

func textSpan(since, until time.Time) string {
	since, until = since.UTC(), until.UTC()
	startFormat, endFormat := "2006-01-02 15:04:05", "2006-01-02 15:04:05"
	if since.Year() == until.Year() {
		startFormat, endFormat = "Jan 2 15:04:05", "Jan 2 15:04:05"
		if since.Format("2006-01-02") == until.Format("2006-01-02") {
			endFormat = "15:04:05"
		}
	}
	return since.Format(startFormat) + " to " + until.Format(endFormat) + " UTC"
}

func readingLimitDescription(reading store.MeasurementReading) string {
	parts := []string{}
	if reading.FailWhenMeasurementAbove != nil {
		parts = append(parts, "fails above "+textNumber(*reading.FailWhenMeasurementAbove))
	}
	if reading.FailWhenMeasurementBelow != nil {
		parts = append(parts, "fails below "+textNumber(*reading.FailWhenMeasurementBelow))
	}
	if len(parts) > 0 {
		return strings.Join(parts, ", ")
	}
	return "only records"
}

type readingsChartOptions struct{ width int }

func styledText(text, style string, styled bool) string {
	if styled {
		return "\x1b[" + style + "m" + text + "\x1b[0m"
	}
	return text
}

// printReadings uses the same measurement blocks for lists and charts.
func printReadings(result readingsResult, chart *readingsChartOptions, styled bool) {
	selection := []string{}
	if result.NodeGroup != "" {
		selection = append(selection, "node group "+result.NodeGroup)
	} else if len(result.Nodes) > 0 && result.Nodes[0].Node != "" {
		selection = append(selection, "node "+result.Nodes[0].Node)
	}
	selection = append(selection, "check group "+result.CheckGroup, textSpan(result.Since, result.Until))
	fmt.Println(strings.Join(selection, " · "))
	// Collect the union once, in the same check/measurement order as the history.
	blocks := map[string]measurementReadings{}
	series := map[string][]chartSeries{}
	for _, node := range result.Nodes {
		for _, block := range node.Measurements {
			key := block.Check + "/" + block.Measurement
			blocks[key] = block
			series[key] = append(series[key], chartSeries{node: node.Node, readings: block.Readings})
		}
	}
	keys := make([]string, 0, len(blocks))
	// The checks that judge at least one of their measurements against a limit.
	checksWithALimit := map[string]bool{}
	for key := range blocks {
		keys = append(keys, key)
		for _, item := range series[key] {
			for _, reading := range item.readings {
				if len(readingLimits(reading)) > 0 {
					checksWithALimit[blocks[key].Check] = true
				}
			}
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		block := blocks[key]
		var readings []readingPoint
		for _, item := range series[key] {
			readings = append(readings, item.readings...)
		}
		limits, varying := measurementLimitDescription(readings)
		if varying {
			limits = "limits vary; the limits recorded with each reading appear beside the reading"
			if chart != nil {
				limits = "limits vary; the dashed line follows the recorded limits"
			}
		}
		if limits == "only records" && checksWithALimit[block.Check] {
			// Another measurement of this check has a limit, so "only records" would read as if the whole check never fails.
			limits = "recorded, not judged by this check"
		}
		fmt.Println()
		fmt.Println(styledText(block.Check, "1", styled))
		title := block.Measurement
		if unit := block.Readings[0].Unit; unit != "" {
			title += ", " + unit
		}
		fmt.Println(title + " · " + limits)
		if chart != nil {
			fmt.Println()
			fmt.Print(drawReadingsChart(series[key], result.Since, result.Until, chart.width, styled, result.NodeGroup != ""))
		}
		fmt.Println()
		printReadingNodes(result, key, chart != nil, varying)
	}
	if len(keys) == 0 {
		fmt.Println()
		printReadingNodes(result, "", chart != nil, false)
	}
}

// printReadingNodes aligns each node's count, readings, and summary after its name.
func printReadingNodes(result readingsResult, key string, chart, varying bool) {
	nodeWidth := 0
	for _, node := range result.Nodes {
		if node.ReadingsAreHealthy {
			continue
		}
		listed := node.WhyNoReadings != ""
		for _, block := range node.Measurements {
			listed = listed || block.Check+"/"+block.Measurement == key
		}
		if listed {
			nodeWidth = max(nodeWidth, len([]rune(node.Node)))
		}
	}
	var healthy []string
	for _, node := range result.Nodes {
		if result.NodeGroup != "" && node.ReadingsAreHealthy {
			healthy = append(healthy, node.Node)
			continue
		}
		prefix, indent := "  ", "  "
		if result.NodeGroup != "" {
			prefix += fmt.Sprintf("%-*s   ", nodeWidth, node.Node)
			indent = strings.Repeat(" ", nodeWidth+5)
		}
		if node.WhyNoReadings != "" {
			fmt.Println(prefix + node.WhyNoReadings)
			continue
		}
		for _, block := range node.Measurements {
			if block.Check+"/"+block.Measurement != key {
				continue
			}
			summary := block.Summary
			count := fmt.Sprintf("%d readings", summary.ReadingCount)
			if summary.ReadingCount == 1 {
				count = "1 reading"
			}
			onlyRecords := true
			for _, reading := range block.Readings {
				if len(readingLimits(reading)) > 0 {
					onlyRecords = false
					break
				}
			}
			if !onlyRecords {
				if summary.ReadingsBeyondLimit == 0 {
					count += ", none beyond the limit"
				} else {
					count += fmt.Sprintf(", %d beyond the limit", summary.ReadingsBeyondLimit)
				}
			}
			fmt.Println(prefix + count)
			if chart {
				fmt.Print(readingRuns(block.Readings, result.Since, result.Until, indent+"  "))
			} else {
				for _, reading := range block.Readings {
					line := indent + "  " + textTime(reading.At, result.Since, result.Until) + "   " + textNumber(reading.Value)
					if readingBeyondLimit(reading) {
						status, _ := readingLimitStatus(reading.MeasurementReading)
						line += "   " + status
					}
					if varying {
						line += "   (" + readingLimitDescription(reading.MeasurementReading) + ")"
					}
					fmt.Println(line)
				}
			}
			fmt.Printf("%slowest %s · highest %s at %s · latest %s at %s\n", indent, textNumber(summary.Lowest.Value), textNumber(summary.Highest.Value), textTime(summary.Highest.At, result.Since, result.Until), textNumber(summary.Latest.Value), textTime(summary.Latest.At, result.Since, result.Until))
		}
	}
	if len(healthy) > 0 {
		fmt.Printf("  %d healthy, none beyond the limit: %s\n", len(healthy), strings.Join(healthy, ", "))
	}
}
