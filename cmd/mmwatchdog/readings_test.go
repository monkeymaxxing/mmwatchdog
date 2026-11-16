package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/store"
)

func TestReadingsTimeFlags(t *testing.T) {
	now := time.Date(2026, 10, 7, 15, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		args []string
		span time.Duration
	}{
		{[]string{"machine"}, time.Hour},
		{[]string{"machine", "memory", "--last", "7d"}, 7 * 24 * time.Hour},
		{[]string{"--since", "2026-10-07T14:00:00Z", "machine", "--until", "2026-10-07T15:00:00Z"}, time.Hour},
	} {
		options, err := parseReadingsOptions(test.args, false, now)
		if err != nil || options.until.Sub(options.since) != test.span {
			t.Fatal(options, err)
		}
	}
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"machine", "--last", "1h", "--since", "2026-10-07T14:00:00Z"}, "choose one time span"},
		{[]string{"machine", "--until", "2026-10-07T15:00:00Z"}, "--until needs --since"},
		{[]string{"machine", "--since", "yesterday"}, "--since needs an RFC 3339 time"},
		{[]string{"machine", "--since", "2026-10-07T14:00:00Z", "--until", "later"}, "--until needs an RFC 3339 time"},
		{[]string{"machine", "--since", "2026-10-07T15:00:00Z", "--until", "2026-10-07T14:00:00Z"}, "choose an end at or after the start"},
		{[]string{"machine", "--last", "0s"}, "positive duration"},
		{[]string{"machine", "--last", "tomorrow"}, "positive duration"},
		{[]string{"machine", "--last"}, "flag needs an argument"},
		{[]string{}, "needs CHECK_GROUP"},
	} {
		_, err := parseReadingsOptions(test.args, false, now)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatal(test.args, err)
		}
	}
	for _, args := range [][]string{{"--node-group", "apps", "machine", "memory"}, {"machine", "memory", "--node-group", "apps"}, {"node-1", "machine", "memory"}} {
		if _, err := parseReadingsOptions(args, true, now); err != nil {
			t.Fatal(args, err)
		}
	}
	for _, args := range [][]string{{"--node-group", "apps", "machine"}, {"--node-group", "", "machine", "memory"}, {"node-1"}} {
		if _, err := parseReadingsOptions(args, true, now); err == nil {
			t.Fatal(args, "accepted incomplete fleet arguments")
		}
	}
}

func readingsFixture(t *testing.T, onHub bool) (string, string) {
	t.Helper()
	directory := t.TempDir()
	stateDirectory := filepath.Join(directory, "state")
	if err := os.Mkdir(stateDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	settings := nodeSettingsOfATest(directory) + "state_dir: " + stateDirectory + "\n"
	if onHub {
		settings += "fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'hub.example.com:8445', keep_measurement_history_of_nodes_for: 7d, measurement_history_of_nodes_size_limit: 1GiB}\n"
	}
	writeFiles(t, directory, map[string]string{
		"mmwatchdog_node_settings.yml": settings,
		"alert_destinations.yml":       "destinations: {}\n",
		"check_groups/machine.yml":     "check_groups:\n  machine:\n    " + strings.ReplaceAll(timingsOfACheckGroup, ", ", "\n    ") + "\n    checks:\n      memory: {only_record_readings: true, node_resource: memory}\n      web: {http: {url: 'http://localhost/'}, alert_severity: warn}\n",
	})
	return directory, stateDirectory
}

func addReading(t *testing.T, history *store.MeasurementHistory, at time.Time, value float64, above, below *float64) {
	t.Helper()
	err := history.Add(store.RunReadings{At: at, CheckGroup: "machine", Check: "memory", Measurements: map[string]store.MeasurementReading{"memory_used_percent": {Value: value, Unit: "percent", FailWhenMeasurementAbove: above, FailWhenMeasurementBelow: below}}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestReadingsCommandWithoutDaemon(t *testing.T) {
	directory, stateDirectory := readingsFixture(t, false)
	history, err := store.OpenMeasurementHistory(filepath.Join(stateDirectory, store.MeasurementHistoryFile), 16*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 7, 15, 0, 0, 0, time.UTC)
	above, below := 90.0, 10.0
	addReading(t, history, now.Add(-3*time.Minute), 41, &above, nil)
	addReading(t, history, now.Add(-2*time.Minute), 95, &above, nil)
	addReading(t, history, now.Add(-time.Minute), 5, nil, &below)
	addReading(t, history, now, 60, nil, nil)
	history.Close()
	options, err := parseReadingsOptions([]string{"machine", "memory", "--config-dir", directory}, false, now)
	if err != nil {
		t.Fatal(err)
	}
	output, err := capture(t, func() error { return showReadings(options) })
	if err != nil {
		t.Fatal(err)
	}
	want := "check group machine · Oct 7 14:00:00 to 15:00:00 UTC\n\nmemory\nmemory_used_percent, percent · limits vary; the limits recorded with each reading appear beside the reading\n\n  4 readings, 2 beyond the limit\n    14:57:00   41   (fails above 90)\n    14:58:00   95   above the limit   (fails above 90)\n    14:59:00   5   below the limit   (fails below 10)\n    15:00:00   60   (only records)\n  lowest 5 · highest 95 at 14:58:00 · latest 60 at 15:00:00\n"
	if output != want {
		t.Errorf("got:\n%s\nwant:\n%s", output, want)
	}

	options.asJSON = true
	output, err = capture(t, func() error { return showReadings(options) })
	var result readingsResult
	if err != nil || json.Unmarshal([]byte(output), &result) != nil {
		t.Fatal(output, err)
	}
	summary := result.Nodes[0].Measurements[0].Summary
	if summary.ReadingCount != 4 || summary.ReadingsBeyondLimit != 2 || summary.Highest.Value != 95 || result.Nodes[0].Measurements[0].Readings[2].FailWhenMeasurementBelow == nil {
		t.Fatal(output)
	}
	options.asJSON = false
	for _, test := range []struct{ group, check, want string }{{"missing", "", "unknown check group"}, {"machine", "missing", "unknown check"}, {"machine", "web", "records no measurements"}} {
		options.checkGroup, options.check = test.group, test.check
		if err = showReadings(options); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatal(test, err)
		}
	}
	options.checkGroup, options.check = "machine", "memory"
	options.since, options.until = now.Add(time.Hour), now.Add(2*time.Hour)
	output, err = capture(t, func() error { return showReadings(options) })
	if err != nil || !strings.Contains(output, "no readings in this time span") {
		t.Fatal(output, err)
	}
}

func TestFleetReadingsNodeGroupOrderingAndHealthyLine(t *testing.T) {
	directory, stateDirectory := readingsFixture(t, true)
	hub, err := fleet.OpenHub(stateDirectory, "hub.example.com:8445", "hub", time.Second, config.DefaultHubLimits, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 7, 15, 0, 0, 0, time.UTC)
	above := 90.0
	for _, node := range []struct {
		name   string
		values []float64
		limit  *float64
	}{
		{"a_small", []float64{95}, &above}, {"z_many", []float64{91, 92}, &above}, {"b_far", []float64{99}, &above}, {"healthy_1", []float64{90}, &above}, {"healthy_2", []float64{41}, &above}, {"records_only", []float64{41}, nil}, {"no_readings", nil, &above},
	} {
		saved, _ := json.Marshal(map[string]any{"credential": "test-credential-" + node.name, "this_node_is_in_node_groups": []string{"apps"}})
		if _, err = hub.DB.Exec("INSERT INTO nodes(name,data) VALUES(?,?)", node.name, saved); err != nil {
			t.Fatal(err)
		}
		runs := []store.RunReadings{}
		for index, value := range node.values {
			runs = append(runs, store.RunReadings{SequenceNumber: int64(index + 1), At: now.Add(-time.Duration(len(node.values)-index) * time.Minute), CheckGroup: "machine", Check: "memory", Measurements: map[string]store.MeasurementReading{"memory_used_percent": {Value: value, Unit: "percent", FailWhenMeasurementAbove: node.limit}}})
		}
		if _, err = hub.Readings.AddFromNode(node.name, "history", 0, runs, now); err != nil {
			t.Fatal(err)
		}
	}
	hub.Close()
	options, err := parseReadingsOptions([]string{"--node-group", "apps", "machine", "memory", "--config-dir", directory}, true, now)
	if err != nil {
		t.Fatal(err)
	}
	output, err := capture(t, func() error { return showReadings(options) })
	if err != nil {
		t.Fatal(err)
	}
	previous := -1
	for _, name := range []string{"z_many", "b_far", "a_small", "no_readings"} {
		position := strings.Index(output, "  "+name+strings.Repeat(" ", 11-len(name))+"   ")
		if position <= previous {
			t.Fatal("incorrect order", output)
		}
		previous = position
	}
	if !strings.Contains(output, "  3 healthy, none beyond the limit: healthy_1, healthy_2, records_only") || strings.Contains(output, "  healthy_") || strings.Contains(output, "  records_only") {
		t.Fatal(output)
	}
	options.asJSON = true
	output, err = capture(t, func() error { return showReadings(options) })
	var result readingsResult
	if err != nil || json.Unmarshal([]byte(output), &result) != nil || len(result.Nodes) != 7 || result.NodesWithHealthyReadingsCount != 3 || !strings.Contains(output, `"why_no_readings":`) || result.Nodes[0].Node != "z_many" {
		t.Fatal(output, err)
	}
	options.nodeGroup, options.node = "", "b_far"
	output, err = capture(t, func() error { return showReadings(options) })
	if err != nil || !strings.Contains(output, `"node": "b_far"`) {
		t.Fatal(output, err)
	}
	options.node = "missing"
	if err = showReadings(options); err == nil || !strings.Contains(err.Error(), "has no record of node") {
		t.Fatal(err)
	}
	options.nodeGroup = "missing"
	if err = showReadings(options); err == nil || !strings.Contains(err.Error(), "has no nodes") {
		t.Fatal(err)
	}
}

func TestReadingsRecordOnlyAndZeroLimits(t *testing.T) {
	zero := 0.0
	for _, test := range []struct {
		reading store.MeasurementReading
		want    string
	}{
		{store.MeasurementReading{}, "only records"},
		{store.MeasurementReading{Value: 0, FailWhenMeasurementAbove: &zero, FailWhenMeasurementBelow: &zero}, "within the limits"},
		{store.MeasurementReading{Value: 1, FailWhenMeasurementAbove: &zero}, "above the limit"},
		{store.MeasurementReading{Value: -1, FailWhenMeasurementBelow: &zero}, "below the limit"},
	} {
		status, _ := readingLimitStatus(test.reading)
		if status != test.want {
			t.Fatal(test, status)
		}
	}
}

func TestReadingsShowsEveryReading(t *testing.T) {
	now := time.Date(2026, 10, 7, 15, 0, 0, 0, time.UTC)
	runs := []store.RunReadings{}
	for index := 0; index < 150; index++ {
		runs = append(runs, store.RunReadings{At: now.Add(time.Duration(index) * time.Second), CheckGroup: "machine", Check: "memory", Measurements: map[string]store.MeasurementReading{"memory_used_percent": {Value: float64(index), Unit: "percent"}}})
	}
	result := readingsResult{Since: now, Until: now.Add(149 * time.Second), CheckGroup: "machine", Nodes: []nodeReadings{summarizeReadings("", runs)}}
	output, err := capture(t, func() error { printReadings(result, nil, false); return nil })
	if err != nil || strings.Count(output, "\n    15:") != 150 || !strings.Contains(output, "150 readings\n") || !strings.Contains(output, "only records") {
		t.Fatal(output, err)
	}
}

func TestReadingsCommandDispatch(t *testing.T) {
	for _, args := range [][]string{{"readings", "machine", "--last", "0s"}, {"fleet", "readings", "node-1", "machine", "--last", "0s"}} {
		if err := run(args); err == nil || !strings.Contains(err.Error(), "--last needs a positive duration") {
			t.Fatal(args, err)
		}
	}
	output, err := capture(t, func() error { return run([]string{"readings", "--help"}) })
	if err != nil || !strings.Contains(output, "mmwatchdog fleet readings --node-group NODE_GROUP CHECK_GROUP CHECK") {
		t.Fatal(output, err)
	}
}

func TestFleetReadingsIncludesHubsOwnHistory(t *testing.T) {
	directory, stateDirectory := readingsFixture(t, true)
	now := time.Date(2026, 10, 7, 15, 0, 0, 0, time.UTC)
	hub, err := fleet.OpenHub(stateDirectory, "hub.example.com:8445", "hub", time.Second, config.DefaultHubLimits, now)
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := json.Marshal(map[string]any{"this_node_is_in_node_groups": []string{"apps"}})
	if _, err = hub.DB.Exec("UPDATE nodes SET data=? WHERE name=?", saved, "hub"); err != nil {
		t.Fatal(err)
	}
	hub.Close()
	history, err := store.OpenMeasurementHistory(filepath.Join(stateDirectory, store.MeasurementHistoryFile), 16*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	addReading(t, history, now, 41, nil, nil)
	history.Close()
	for _, names := range [][]string{{"hub", "machine", "memory"}, {"--node-group", "apps", "machine", "memory"}} {
		options, err := parseReadingsOptions(append(names, "--config-dir", directory, "--json"), true, now)
		if err != nil {
			t.Fatal(err)
		}
		output, err := capture(t, func() error { return showReadings(options) })
		var result readingsResult
		if err != nil || json.Unmarshal([]byte(output), &result) != nil || len(result.Nodes) != 1 || result.Nodes[0].Node != "hub" || result.Nodes[0].Measurements[0].Summary.Latest.Value != 41 {
			t.Fatal(output, err)
		}
	}
}

func TestReadingsChartFlags(t *testing.T) {
	now := time.Date(2026, 10, 7, 15, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		args []string
		hub  bool
	}{
		{[]string{"machine", "memory", "--chart"}, false},
		{[]string{"node-1", "machine", "memory", "--chart"}, true},
		{[]string{"--node-group", "apps", "machine", "memory", "--chart"}, true},
	} {
		options, err := parseReadingsOptions(test.args, test.hub, now)
		if err != nil || !options.chart {
			t.Fatal(options, err)
		}
		_, err = parseReadingsOptions(append(test.args, "--json"), test.hub, now)
		if err == nil || !strings.Contains(err.Error(), "--chart") || !strings.Contains(err.Error(), "--json") {
			t.Fatal(err)
		}
	}
}

func TestReadingsChartCommandWithoutDaemon(t *testing.T) {
	directory, stateDirectory := readingsFixture(t, false)
	history, err := store.OpenMeasurementHistory(filepath.Join(stateDirectory, store.MeasurementHistoryFile), 16*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 7, 15, 0, 0, 0, time.UTC)
	above, changed := 90.0, 80.0
	addReading(t, history, now.Add(-time.Minute), 95, &above, nil)
	addReading(t, history, now, 50, &changed, nil)
	history.Close()
	options, err := parseReadingsOptions([]string{"machine", "memory", "--config-dir", directory, "--chart"}, false, now)
	if err != nil {
		t.Fatal(err)
	}
	output, err := capture(t, func() error { return showReadings(options) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"check group machine · Oct 7 14:00:00 to 15:00:00 UTC\n\nmemory\n",
		"memory_used_percent, percent · limits vary; the dashed line follows the recorded limits\n\n",
		"    14:59:00   95 above the limit",
		"  ╌ limit   ▲ beyond the limit\n  one column is less than 1s · no readings before 14:59:00\n\n  2 readings, 1 beyond the limit\n    14:59:00   95 above the limit\n  lowest 50 · highest 95 at 14:59:00 · latest 50 at 15:00:00",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q\n%s", want, output)
		}
	}
	if strings.Contains(output, "\x1b") || strings.Contains(output, "  2026-10-07T14:59:00Z  95 percent") {
		t.Fatal(output)
	}
	drawing := strings.Split(output, "\n")[5]
	if len([]rune(drawing)) != 79 {
		t.Fatalf("pipe uses 80 columns with one spare, got %d: %q", len([]rune(drawing)), drawing)
	}
}

func TestFleetReadingsChartBeforeNodeBlocks(t *testing.T) {
	directory, stateDirectory := readingsFixture(t, true)
	now := time.Date(2026, 10, 7, 15, 0, 0, 0, time.UTC)
	hub, err := fleet.OpenHub(stateDirectory, "hub.example.com:8445", "hub", time.Second, config.DefaultHubLimits, now)
	if err != nil {
		t.Fatal(err)
	}
	above := 90.0
	for _, node := range []struct {
		name   string
		values []float64
	}{
		{"many", []float64{91, 92}}, {"farthest", []float64{99}}, {"healthy", []float64{20}}, {"empty", nil},
	} {
		saved, _ := json.Marshal(map[string]any{"credential": "test-" + node.name, "this_node_is_in_node_groups": []string{"apps"}})
		if _, err = hub.DB.Exec("INSERT INTO nodes(name,data) VALUES(?,?)", node.name, saved); err != nil {
			t.Fatal(err)
		}
		runs := []store.RunReadings{}
		for index, value := range node.values {
			runs = append(runs, store.RunReadings{SequenceNumber: int64(index + 1), At: now.Add(-time.Duration(len(node.values)-index) * time.Minute), CheckGroup: "machine", Check: "memory", Measurements: map[string]store.MeasurementReading{"memory_used_percent": {Value: value, Unit: "percent", FailWhenMeasurementAbove: &above}}})
		}
		if _, err = hub.Readings.AddFromNode(node.name, "history", 0, runs, now); err != nil {
			t.Fatal(err)
		}
	}
	hub.Close()
	options, err := parseReadingsOptions([]string{"--node-group", "apps", "machine", "memory", "--config-dir", directory, "--chart"}, true, now)
	if err != nil {
		t.Fatal(err)
	}
	output, err := capture(t, func() error { return showReadings(options) })
	if err != nil {
		t.Fatal(err)
	}
	previous := -1
	for _, want := range []string{
		"memory\nmemory_used_percent, percent · fails above 90",
		"─ farthest, farthest beyond the limit",
		"  many       2 readings, 2 beyond the limit", "14:58:00 to 14:59:00   2 readings above the limit, farthest 92 at 14:59:00", "  farthest   1 reading, 1 beyond the limit", "14:59:00   99 above the limit", "  empty      ", "  1 healthy, none beyond the limit: healthy",
	} {
		position := strings.Index(output, want)
		if position <= previous {
			t.Fatalf("missing or out of order %q\n%s", want, output)
		}
		previous = position
	}
	if strings.Contains(output, "  healthy   ") || strings.Count(output, "one column is") != 1 {
		t.Fatal(output)
	}
	options.nodeGroup, options.node = "", "farthest"
	output, err = capture(t, func() error { return showReadings(options) })
	if err != nil || !strings.HasPrefix(output, "node farthest · check group machine · ") || strings.Count(output, "one column is") != 1 || strings.Contains(output, "░ all nodes") {
		t.Fatal(output, err)
	}
	options.since, options.until = now.Add(time.Hour), now.Add(2*time.Hour)
	output, err = capture(t, func() error { return showReadings(options) })
	if err != nil || strings.Contains(output, "│") {
		t.Fatal(output, err)
	}
	options.nodeGroup, options.node = "apps", ""
	output, err = capture(t, func() error { return showReadings(options) })
	if err != nil || strings.Contains(output, "│") {
		t.Fatal(output, err)
	}
}

func TestReadingsChartMeasurementUnionAndCleanedText(t *testing.T) {
	above, changed := 90.0, 80.0
	result := readingsResult{Since: chartStart, Until: chartStart.Add(time.Minute), CheckGroup: "machine\x1b", NodeGroup: "apps\x1b", Nodes: []nodeReadings{
		{Node: "one\x1b", Measurements: []measurementReadings{
			{CheckGroup: "machine\x1b", Check: "memory\x1b", Measurement: "z\x1b", Readings: []readingPoint{chartPoint(0, 95, &above, nil)}},
			{CheckGroup: "machine\x1b", Check: "memory\x1b", Measurement: "a\x1b", Readings: []readingPoint{chartPoint(0, 95, &above, nil), chartPoint(30, 95, &changed, nil)}},
		}},
		{Node: "two", Measurements: []measurementReadings{
			{CheckGroup: "machine\x1b", Check: "memory\x1b", Measurement: "a\x1b", Readings: []readingPoint{chartPoint(0, 95, &changed, nil)}},
			{CheckGroup: "machine\x1b", Check: "memory\x1b", Measurement: "only_on_two", Readings: []readingPoint{chartPoint(0, 95, &above, nil)}},
		}},
		{Node: "empty", WhyNoReadings: "no readings\x1b"},
	}}
	result.Nodes[0].Measurements[0].Readings[0].Unit = "percent\x1b"
	output, err := capture(t, func() error { printReadings(cleaned(result), &readingsChartOptions{width: 80}, false); return nil })
	if err != nil || strings.Contains(output, "\x1b") {
		t.Fatal(output, err)
	}
	previous := -1
	for _, want := range []string{
		"a�, percent · limits vary; the dashed line follows the recorded limits",
		"only_on_two, percent · fails above 90",
		"z�, percent� · fails above 90",
	} {
		position := strings.Index(output, want)
		if position <= previous {
			t.Fatalf("missing or out of order %q\n%s", want, output)
		}
		previous = position
	}
	if strings.Count(output, "one column is") != 3 || strings.Count(output, "memory�\n") != 3 || !strings.Contains(output, "one�    ") || strings.Count(output, "no readings�\n") != 3 || !strings.HasPrefix(output, "node group apps� · check group machine� · ") {
		t.Fatal(output)
	}
}

func TestTextNumber(t *testing.T) {
	for _, test := range []struct {
		value float64
		want  string
	}{
		{0, "0"}, {math.Copysign(0, -1), "0"}, {37.25, "37.25"}, {91, "91"}, {0.014066676, "0.01407"}, {1.3136446, "1.314"}, {123456.7, "123457"}, {80.85, "80.85"}, {-1.3136446, "-1.314"}, {9.99996, "10"}, {100, "100"},
	} {
		if got := textNumber(test.value); got != test.want {
			t.Errorf("%g: got %q, want %q", test.value, got, test.want)
		}
	}
}

func TestTextTimeAndSpan(t *testing.T) {
	for _, test := range []struct{ since, until, wantTime, wantSpan string }{
		{"2026-10-22T19:00:00+02:00", "2026-10-22T18:00:00Z", "17:00:00", "Oct 22 17:00:00 to 18:00:00 UTC"},
		{"2026-10-21T23:00:00Z", "2026-10-22T23:00:00Z", "Oct 21 23:00:00", "Oct 21 23:00:00 to Oct 22 23:00:00 UTC"},
		{"2025-12-31T23:00:00Z", "2026-01-01T23:00:00Z", "2025-12-31 23:00:00", "2025-12-31 23:00:00 to 2026-01-01 23:00:00 UTC"},
	} {
		since, _ := time.Parse(time.RFC3339, test.since)
		until, _ := time.Parse(time.RFC3339, test.until)
		if got := textTime(since, since, until); got != test.wantTime {
			t.Errorf("time %q, want %q", got, test.wantTime)
		}
		if got := textSpan(since, until); got != test.wantSpan {
			t.Errorf("span %q, want %q", got, test.wantSpan)
		}
	}
}

// A single run is enough to exercise count grammar, limits, and summary alignment.
func textReadingsNode(name string, value float64, above, below *float64) nodeReadings {
	return summarizeReadings(name, []store.RunReadings{{At: chartStart, CheckGroup: "machine", Check: "memory", Measurements: map[string]store.MeasurementReading{
		"used": {Value: value, FailWhenMeasurementAbove: above, FailWhenMeasurementBelow: below},
	}}})
}

func TestReadingsSingleReadingLayout(t *testing.T) {
	for _, test := range []struct {
		node         string
		above, below *float64
		title, count string
	}{
		{"", nil, nil, "only records", "1 reading"},
		{"peer", floatPointer(90), nil, "fails above 90", "1 reading, none beyond the limit"},
		{"", nil, floatPointer(10), "fails below 10", "1 reading, none beyond the limit"},
		{"", floatPointer(90), floatPointer(10), "fails above 90, fails below 10", "1 reading, none beyond the limit"},
	} {
		result := readingsResult{Since: chartStart, Until: chartStart.Add(time.Hour), CheckGroup: "machine", Nodes: []nodeReadings{textReadingsNode(test.node, 37.25, test.above, test.below)}}
		want := "check group machine · Oct 7 14:30:00 to 15:30:00 UTC\n\nmemory\nused · " + test.title + "\n\n  " + test.count + "\n    14:30:00   37.25\n  lowest 37.25 · highest 37.25 at 14:30:00 · latest 37.25 at 14:30:00\n"
		if test.node != "" {
			want = "node peer · " + want
		}
		output, err := capture(t, func() error { printReadings(result, nil, false); return nil })
		if err != nil || output != want {
			t.Fatalf("got:\n%s\nwant:\n%s\n%v", output, want, err)
		}
	}
}

func TestReadingsNodeGroupLayout(t *testing.T) {
	result := readingsResult{Since: chartStart, Until: chartStart.Add(time.Hour), CheckGroup: "machine", NodeGroup: "apps", Nodes: []nodeReadings{
		textReadingsNode("a", 91, floatPointer(90), nil),
		{Node: "empty", WhyNoReadings: "no readings in this time span; choose a longer --last or an earlier --since"},
		textReadingsNode("healthy", 37.25, floatPointer(90), nil),
	}}
	want := `node group apps · check group machine · Oct 7 14:30:00 to 15:30:00 UTC

memory
used · fails above 90

  a       1 reading, 1 beyond the limit
            14:30:00   91   above the limit
          lowest 91 · highest 91 at 14:30:00 · latest 91 at 14:30:00
  empty   no readings in this time span; choose a longer --last or an earlier --since
  1 healthy, none beyond the limit: healthy
`
	output, err := capture(t, func() error { printReadings(result, nil, false); return nil })
	if err != nil || output != want {
		t.Fatalf("got:\n%s\nwant:\n%s\n%v", output, want, err)
	}
	// Without a healthy node there is no compressed healthy line.
	result.Nodes = result.Nodes[:2]
	output, err = capture(t, func() error { printReadings(result, nil, false); return nil })
	if err != nil || output != strings.TrimSuffix(want, "  1 healthy, none beyond the limit: healthy\n") {
		t.Fatal(output, err)
	}
}

func TestReadingsWithoutMeasurementsLayout(t *testing.T) {
	for _, chart := range []*readingsChartOptions{nil, {width: 80}} {
		result := readingsResult{Since: chartStart, Until: chartStart.Add(time.Hour), CheckGroup: "machine", NodeGroup: "apps", Nodes: []nodeReadings{
			{Node: "a", WhyNoReadings: "no readings in this time span; choose a longer --last or an earlier --since"},
			{Node: "empty", WhyNoReadings: "the hub has no recorded measurements for this check; choose a measuring check or let the node send its readings"},
		}}
		want := `node group apps · check group machine · Oct 7 14:30:00 to 15:30:00 UTC

  a       no readings in this time span; choose a longer --last or an earlier --since
  empty   the hub has no recorded measurements for this check; choose a measuring check or let the node send its readings
`
		output, err := capture(t, func() error { printReadings(result, chart, false); return nil })
		if err != nil || output != want {
			t.Fatal(output, err)
		}
		result.NodeGroup = ""
		result.Nodes = result.Nodes[:1]
		result.Nodes[0].Node = ""
		want = "check group machine · Oct 7 14:30:00 to 15:30:00 UTC\n\n  no readings in this time span; choose a longer --last or an earlier --since\n"
		output, err = capture(t, func() error { printReadings(result, chart, false); return nil })
		if err != nil || output != want {
			t.Fatal(output, err)
		}
	}
}

func TestReadingsVaryingLimitsAcrossNodes(t *testing.T) {
	result := readingsResult{Since: chartStart, Until: chartStart.Add(time.Hour), CheckGroup: "machine", NodeGroup: "apps", Nodes: []nodeReadings{
		textReadingsNode("a", 91, floatPointer(90), nil), textReadingsNode("b", 92, floatPointer(80), nil),
	}}
	output, err := capture(t, func() error { printReadings(result, nil, false); return nil })
	for _, want := range []string{
		"used · limits vary; the limits recorded with each reading appear beside the reading\n",
		"14:30:00   91   above the limit   (fails above 90)\n",
		"14:30:00   92   above the limit   (fails above 80)\n",
	} {
		if err != nil || !strings.Contains(output, want) {
			t.Fatalf("missing %q in\n%s\n%v", want, output, err)
		}
	}
	// Rounding must not hide a real change in the recorded limits.
	result.Nodes[1] = textReadingsNode("b", 92, floatPointer(90.00001), nil)
	output, err = capture(t, func() error { printReadings(result, nil, false); return nil })
	if err != nil || !strings.Contains(output, "limits vary;") {
		t.Fatal(output, err)
	}
}

func TestReadingsStyling(t *testing.T) {
	result := readingsResult{Since: chartStart, Until: chartStart.Add(time.Hour), CheckGroup: "machine", Nodes: []nodeReadings{textReadingsNode("", 91, floatPointer(90), nil)}}
	for _, chart := range []*readingsChartOptions{nil, {width: 80}} {
		plain, err := capture(t, func() error { printReadings(result, chart, false); return nil })
		if err != nil || strings.Contains(plain, "\x1b") {
			t.Fatal(plain, err)
		}
		styled, err := capture(t, func() error { printReadings(result, chart, true); return nil })
		if err != nil || !strings.Contains(styled, "\x1b[1mmemory\x1b[0m\nused · fails above 90") {
			t.Fatal(styled, err)
		}
		if chart == nil && strings.Contains(styled, "\x1b[2m") {
			t.Fatal("list has no dim chart text", styled)
		}
		stripped := strings.NewReplacer("\x1b[1m", "", "\x1b[2m", "", "\x1b[31m", "", "\x1b[0m", "").Replace(styled)
		if stripped != plain {
			t.Fatalf("styling changed text\n%s\n%s", styled, plain)
		}
	}
}

func TestReadingsJSONPreservesPrecision(t *testing.T) {
	directory, stateDirectory := readingsFixture(t, false)
	history, err := store.OpenMeasurementHistory(filepath.Join(stateDirectory, store.MeasurementHistoryFile), 16*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 22, 17, 30, 50, 482524207, time.UTC)
	value, limit := 0.014066676, 0.0123456789
	addReading(t, history, now, value, &limit, nil)
	history.Close()
	options, err := parseReadingsOptions([]string{"machine", "memory", "--config-dir", directory, "--json"}, false, now)
	if err != nil {
		t.Fatal(err)
	}
	output, err := capture(t, func() error { return showReadings(options) })
	var result readingsResult
	if err != nil || json.Unmarshal([]byte(output), &result) != nil {
		t.Fatal(output, err)
	}
	point := result.Nodes[0].Measurements[0].Readings[0]
	if !point.At.Equal(now) || point.Value != value || point.FailWhenMeasurementAbove == nil || *point.FailWhenMeasurementAbove != limit || !strings.Contains(output, "2026-10-22T17:30:50.482524207Z") {
		t.Fatal(output)
	}
	options.asJSON = false
	output, err = capture(t, func() error { return showReadings(options) })
	want := "check group machine · Oct 22 16:30:50 to 17:30:50 UTC\n\nmemory\nmemory_used_percent, percent · fails above 0.01235\n\n  1 reading, 1 beyond the limit\n    17:30:50   0.01407   above the limit\n  lowest 0.01407 · highest 0.01407 at 17:30:50 · latest 0.01407 at 17:30:50\n"
	if err != nil || output != want {
		t.Fatalf("got:\n%s\nwant:\n%s\n%v", output, want, err)
	}

}

func TestReadingsTitleSaysWhichMeasurementTheCheckDoesNotJudge(t *testing.T) {
	now := time.Date(2026, 10, 7, 15, 0, 0, 0, time.UTC)
	limit := 85.0
	runs := []store.RunReadings{
		{At: now, CheckGroup: "machine", Check: "root_filesystem_is_below_85_percent", Measurements: map[string]store.MeasurementReading{
			"used_percent":        {Value: 40, Unit: "percent", FailWhenMeasurementAbove: &limit},
			"inodes_used_percent": {Value: 3, Unit: "percent"},
		}},
		{At: now, CheckGroup: "machine", Check: "memory_is_recorded", Measurements: map[string]store.MeasurementReading{
			"memory_used_percent": {Value: 50, Unit: "percent"},
		}},
	}
	result := readingsResult{Since: now, Until: now, CheckGroup: "machine", Nodes: []nodeReadings{summarizeReadings("", runs)}}
	output, err := capture(t, func() error { printReadings(result, nil, false); return nil })
	if err != nil || !strings.Contains(output, "inodes_used_percent, percent · recorded, not judged by this check\n") ||
		!strings.Contains(output, "used_percent, percent · fails above 85\n") ||
		!strings.Contains(output, "memory_used_percent, percent · only records\n") {
		t.Fatal(output, err)
	}
}
