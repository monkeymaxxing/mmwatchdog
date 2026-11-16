package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/store"
)

var chartStart = time.Date(2026, 10, 7, 14, 30, 0, 0, time.UTC)

func chartPoint(seconds int, value float64, above, below *float64) readingPoint {
	return readingPoint{At: chartStart.Add(time.Duration(seconds) * time.Second), MeasurementReading: store.MeasurementReading{Value: value, Unit: "percent", FailWhenMeasurementAbove: above, FailWhenMeasurementBelow: below}}
}

// cutAtChartAxis splits a drawing line at the value axis, which is ┤ in a
// labelled row and │ elsewhere; the label before it holds no such character.
func cutAtChartAxis(line string) (string, string, bool) {
	for index, character := range line {
		if character == '│' || character == '┤' {
			return line[:index], line[index+len(string(character)):], true
		}
	}
	return line, "", false
}

// The public drawing has twelve rows, highest first. Inspect cells by their
// row from the bottom without depending on how the renderer stores them.
func chartCell(t *testing.T, output string, row, column int) rune {
	t.Helper()
	line := strings.Split(output, "\n")[11-row]
	_, drawing, ok := cutAtChartAxis(line)
	if !ok || len([]rune(drawing)) <= column {
		t.Fatalf("missing chart cell in %q", line)
	}
	return []rune(drawing)[column]
}

func TestChartPreservesSpikesAndRecordOnlyMaximum(t *testing.T) {
	above, below := 90.0, 10.0
	for _, test := range []struct {
		name     string
		readings []readingPoint
		row      int
		cell     rune
		limit    bool
	}{
		{"above spike", []readingPoint{chartPoint(0, 10, &above, nil), chartPoint(1, 100, &above, nil), chartPoint(2, 20, &above, nil)}, 11, '─', true},
		// The lowest reading, 1, is selected and sits at the bottom of 1..40.
		{"below spike", []readingPoint{chartPoint(0, 40, nil, &below), chartPoint(1, 1, nil, &below), chartPoint(2, 30, nil, &below)}, 0, '─', true},
		{"records highest", []readingPoint{chartPoint(0, 1, nil, nil), chartPoint(1, 100, nil, nil), chartPoint(2, 20, nil, nil)}, 11, '─', false},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := drawReadingsChart([]chartSeries{{readings: test.readings}}, chartStart, chartStart.Add(1000*time.Second), 10, false, false)
			if got := chartCell(t, output, test.row, 0); got != test.cell {
				t.Fatalf("cell %q, want %q\n%s", got, test.cell, output)
			}
			if strings.Contains(output, "╌ limit") != test.limit {
				t.Fatal(output)
			}
			if !test.limit && strings.Contains(output, "╌") {
				t.Fatal(output)
			}
		})
	}
}

func TestChartGapsAndIntervalHolding(t *testing.T) {
	readings := []readingPoint{}
	for _, seconds := range []int{0, 30, 60, 3660, 3690, 3720} {
		value := 10.0
		if seconds >= 3660 {
			value = 20
		}
		readings = append(readings, chartPoint(seconds, value, nil, nil))
	}
	output := drawReadingsChart([]chartSeries{{readings: readings}}, chartStart, chartStart.Add(4000*time.Second), 10, false, false)
	for column := 1; column <= 8; column++ {
		for row := 0; row < 12; row++ {
			if chartCell(t, output, row, column) != ' ' {
				t.Fatalf("gap column %d row %d\n%s", column, row, output)
			}
		}
	}
	if chartCell(t, output, 11, 9) != '─' {
		t.Fatal("line must restart after a gap", output)
	}
	for row := 0; row < 11; row++ {
		if chartCell(t, output, row, 9) != ' ' {
			t.Fatal("line must not connect across a gap", output)
		}
	}
	readings = nil
	for seconds := 0; seconds <= 480; seconds += 60 {
		readings = append(readings, chartPoint(seconds, 10, nil, nil))
	}
	output = drawReadingsChart([]chartSeries{{readings: readings}}, chartStart, chartStart.Add(500*time.Second), 10, false, false)
	for column := 0; column < 10; column++ {
		if chartCell(t, output, 0, column) != '─' {
			t.Fatalf("unexpected gap in column %d\n%s", column, output)
		}
	}
	output = drawReadingsChart([]chartSeries{{readings: []readingPoint{chartPoint(0, 10, nil, nil)}}}, chartStart, chartStart.Add(500*time.Second), 10, false, false)
	if chartCell(t, output, 0, 1) != ' ' {
		t.Fatal("one reading must not hold", output)
	}
}

func TestChartRecordedLimitsAndMarks(t *testing.T) {
	above, changed := 80.0, 40.0
	readings := []readingPoint{chartPoint(0, 0, &above, nil), chartPoint(500, 0, &changed, nil)}
	output := drawReadingsChart([]chartSeries{{readings: readings}}, chartStart, chartStart.Add(1000*time.Second), 10, false, false)
	if chartCell(t, output, 11, 0) != '╌' || chartCell(t, output, 6, 5) != '╌' {
		t.Fatal(output)
	}
	lines := strings.Split(output, "\n")
	if !strings.HasPrefix(lines[0], "  80┤") || !strings.HasPrefix(lines[5], "  40┤") {
		t.Fatal(output)
	}
	readings = []readingPoint{chartPoint(0, 90, &above, nil), chartPoint(500, 20, &above, nil), chartPoint(1000, 81, &above, nil)}
	output = drawReadingsChart([]chartSeries{{readings: readings}}, chartStart, chartStart.Add(1000*time.Second), 10, false, false)
	_, axis, _ := strings.Cut(strings.Split(output, "\n")[12], "└")
	if axis != "▲▲▲▲▲────▲" {
		t.Fatalf("axis %q\n%s", axis, output)
	}
	if !strings.Contains(output, "▲ beyond the limit") {
		t.Fatal(output)
	}
	if strings.Contains(output, "\x1b") {
		t.Fatal("unexpected color", output)
	}
	colored := drawReadingsChart([]chartSeries{{readings: readings}}, chartStart, chartStart.Add(1000*time.Second), 10, true, false)
	if !strings.Contains(colored, "\x1b[31m─\x1b[0m") || !strings.Contains(colored, "\x1b[31m▲\x1b[0m") || strings.Contains(colored, "\x1b[31m╌") {
		t.Fatal(colored)
	}
}

func TestChartBreachRuns(t *testing.T) {
	above, below := 90.0, 10.0
	readings := []readingPoint{chartPoint(0, 95, &above, nil), chartPoint(30, 40, &above, nil), chartPoint(60, 91, &above, nil), chartPoint(90, 97, &above, nil), chartPoint(120, 92, &above, nil), chartPoint(150, 5, nil, &below)}
	output := readingRuns(readings, chartStart, chartStart.Add(time.Hour), "    ")
	for _, line := range []string{
		"    14:30:00   95 above the limit\n",
		"    14:31:00 to 14:32:00   3 readings above the limit, farthest 97 at 14:31:30\n",
		"    14:32:30   5 below the limit\n",
	} {
		if !strings.Contains(output, line) {
			t.Fatalf("missing %q in %s", line, output)
		}
	}
	if strings.Count(output, "\n") != 3 {
		t.Fatal(output)
	}
}

func TestChartFleetBandAndClosestNode(t *testing.T) {
	above := 90.0
	series := []chartSeries{
		{node: "many", readings: []readingPoint{chartPoint(0, 91, &above, nil), chartPoint(1, 92, &above, nil)}},
		{node: "farthest", readings: []readingPoint{chartPoint(0, 99, &above, nil)}},
		{node: "healthy", readings: []readingPoint{chartPoint(0, 20, &above, nil)}},
	}
	output := drawReadingsChart(series, chartStart, chartStart.Add(1000*time.Second), 10, false, true)
	// The band spans 20..99; the limit at 90 is in row 10.
	for row := 0; row < 11; row++ {
		want := '░'
		if row == 10 {
			want = '╌'
		}
		if chartCell(t, output, row, 0) != want {
			t.Fatal(output)
		}
	}
	if chartCell(t, output, 11, 0) != '─' || !strings.Contains(output, "─ farthest, farthest beyond the limit") {
		t.Fatal(output)
	}
	if chartCell(t, output, 0, 0) != '░' || chartCell(t, output, 11, 1) != ' ' {
		t.Fatal(output)
	}
	series = []chartSeries{{node: "z", readings: []readingPoint{chartPoint(0, 20, nil, nil)}}, {node: "a", readings: []readingPoint{chartPoint(0, 20, nil, nil)}}}
	output = drawReadingsChart(series, chartStart, chartStart.Add(time.Minute), 10, false, true)
	if !strings.Contains(output, "─ a, highest reading") || strings.Contains(output, "╌") {
		t.Fatal(output)
	}
}

func TestChartColorDecisionAndPipeWidth(t *testing.T) {
	for _, test := range []struct {
		terminal bool
		noColor  string
		want     bool
	}{{false, "", false}, {false, "1", false}, {true, "", true}, {true, "1", false}} {
		if got := chartColor(test.terminal, test.noColor); got != test.want {
			t.Fatal(test, got)
		}
	}
	file, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if width, terminal := terminalSize(file); width != 80 || terminal {
		t.Fatal(width, terminal)
	}
}

func TestChartTimeLabelsAndZeroSpan(t *testing.T) {
	tests := []struct {
		span               time.Duration
		width              int
		start, middle, end string
	}{
		{4 * time.Minute, 40, "14:30:00", "14:32:00", "14:34:00"},
		{time.Hour, 40, "14:30", "15:00", "15:30"},
		{24 * time.Hour, 80, "Oct 7 14:30", "Oct 8 02:30", "Oct 8 14:30"},
	}
	for _, test := range tests {
		labels := chartTimeLabels(chartStart, chartStart.Add(test.span), test.width)
		if !strings.HasPrefix(labels, test.start) || !strings.HasSuffix(labels, test.end) || !strings.Contains(labels, test.middle) {
			t.Fatal(labels)
		}
	}
	labels := chartTimeLabels(chartStart, chartStart.Add(4*time.Minute), 20)
	if strings.Contains(labels, "14:32:00") {
		t.Fatal(labels)
	}
	output := drawReadingsChart([]chartSeries{{readings: []readingPoint{chartPoint(0, 0, nil, nil)}}}, chartStart, chartStart, 80, false, false)
	if chartCell(t, output, 0, 0) != '─' || chartCell(t, output, 0, 1) != ' ' || !strings.Contains(output, "one column is less than 1s") {
		t.Fatal(output)
	}
}

func TestChartBothLimitsAndNegativeRange(t *testing.T) {
	above, below := 90.0, 10.0
	// The lower breach is 20 beyond its limit, the upper breach only 5.
	// -10 is selected; every reading still contributes to the range -10..95.
	output := drawReadingsChart([]chartSeries{{readings: []readingPoint{chartPoint(0, 95, &above, &below), chartPoint(1, -10, &above, &below)}}}, chartStart, chartStart.Add(time.Minute), 10, false, false)
	if chartCell(t, output, 0, 0) != '─' || chartCell(t, output, 10, 0) != '╌' || chartCell(t, output, 2, 0) != '╌' {
		t.Fatal(output)
	}
	lines := strings.Split(output, "\n")
	if !strings.HasPrefix(lines[0], "   95┤") || !strings.HasPrefix(lines[11], "  -10┤") {
		t.Fatal(output)
	}
}

func TestChartEarliestReadingWinsEqualMargins(t *testing.T) {
	above, below := 90.0, 10.0
	// Both are five beyond a limit. The earliest, 5, wins and leaves the
	// top row empty; the upper limit is in row 10, below the unselected 95.
	output := drawReadingsChart([]chartSeries{{readings: []readingPoint{chartPoint(0, 5, &above, &below), chartPoint(1, 95, &above, &below)}}}, chartStart, chartStart.Add(time.Minute), 10, false, false)
	if chartCell(t, output, 10, 0) != '╌' || chartCell(t, output, 11, 0) != ' ' || chartCell(t, output, 0, 0) != '─' {
		t.Fatal(output)
	}
}

func TestChartLineStepsAndPeak(t *testing.T) {
	for _, test := range []struct {
		name   string
		values []float64
		rows   []string // Highest row first, ten columns per row.
	}{
		{"rising", []float64{0, 11, 11}, []string{
			" ╭────────", " │        ", " │        ", " │        ",
			" │        ", " │        ", " │        ", " │        ",
			" │        ", " │        ", " │        ", "─╯        ",
		}},
		{"falling", []float64{11, 0, 0}, []string{
			"─╮        ", " │        ", " │        ", " │        ",
			" │        ", " │        ", " │        ", " │        ",
			" │        ", " │        ", " │        ", " ╰────────",
		}},
		{"peak", []float64{0, 11, 0, 0}, []string{
			" ╭╮       ", " ││       ", " ││       ", " ││       ",
			" ││       ", " ││       ", " ││       ", " ││       ",
			" ││       ", " ││       ", " ││       ", "─╯╰───────",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var readings []readingPoint
			for column := 0; column < 10; column++ {
				readings = append(readings, chartPoint(column*10, test.values[min(column, len(test.values)-1)], nil, nil))
			}
			output := drawReadingsChart([]chartSeries{{readings: readings}}, chartStart, chartStart.Add(100*time.Second), 16, false, false)
			for fromTop, expected := range test.rows {
				for column, want := range []rune(expected) {
					if got := chartCell(t, output, 11-fromTop, column); got != want {
						t.Fatalf("row %d column %d: got %q, want %q\n%s", 11-fromTop, column, got, want, output)
					}
				}
			}
		})
	}
}

func TestChartValueRangeAndLimitLabels(t *testing.T) {
	for _, test := range []struct {
		name        string
		readings    []readingPoint
		bottom, top string
	}{
		{"positive readings", []readingPoint{chartPoint(0, 40, nil, nil), chartPoint(100, 60, nil, nil)}, "40", "60"},
		{"upper limit", []readingPoint{chartPoint(0, 40, floatPointer(90), nil), chartPoint(100, 60, nil, nil)}, "40", "90"},
		{"lower limit", []readingPoint{chartPoint(0, 40, nil, floatPointer(20)), chartPoint(100, 60, nil, nil)}, "20", "60"},
		{"equal values", []readingPoint{chartPoint(0, 40, nil, nil)}, "40", "41"},
		{"rounded decimals", []readingPoint{chartPoint(0, 0.014066676, nil, nil), chartPoint(100, 1.3136446, nil, nil)}, "0.01407", "1.314"},
		{"large values without exponent", []readingPoint{chartPoint(0, 123456.7, nil, nil), chartPoint(100, 200000, nil, nil)}, "123457", "200000"},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := drawReadingsChart([]chartSeries{{readings: test.readings}}, chartStart, chartStart.Add(100*time.Second), 80, false, false)
			lines := strings.Split(output, "\n")
			for _, expected := range []struct {
				line  int
				label string
			}{{0, test.top}, {11, test.bottom}} {
				label, _, _ := cutAtChartAxis(lines[expected.line])
				if strings.TrimSpace(label) != expected.label {
					t.Fatal(output)
				}
			}
		})
	}
	// Limits 50 and 51 both round to row 6 in a range of 0..100.
	output := drawReadingsChart([]chartSeries{{readings: []readingPoint{chartPoint(0, 0, floatPointer(51), floatPointer(50)), chartPoint(100, 100, nil, nil)}}}, chartStart, chartStart.Add(100*time.Second), 80, false, false)
	label, _, _ := cutAtChartAxis(strings.Split(output, "\n")[5])
	if strings.TrimSpace(label) != "51" {
		t.Fatal(output)
	}
}

func TestChartLayerOrderAndColor(t *testing.T) {
	limit := 5.0
	series := []chartSeries{
		{node: "line", readings: []readingPoint{chartPoint(0, 0, &limit, nil), chartPoint(10, 11, &limit, nil), chartPoint(20, 5, &limit, nil)}},
		{node: "band", readings: []readingPoint{chartPoint(0, 11, floatPointer(11), nil), chartPoint(10, 0, floatPointer(11), nil), chartPoint(20, 0, floatPointer(11), nil)}},
	}
	plain := drawReadingsChart(series, chartStart, chartStart.Add(100*time.Second), 16, false, true)
	// The band fills column 0; limit 5 is in row 5. The next column's
	// rising line covers the limit. In column 2 the line equals the limit.
	for _, cell := range []struct {
		row, column int
		want        rune
	}{{4, 0, '░'}, {5, 0, '╌'}, {5, 1, '│'}, {5, 2, '╰'}} {
		if chartCell(t, plain, cell.row, cell.column) != cell.want {
			t.Fatal(plain)
		}
	}
	colored := drawReadingsChart(series, chartStart, chartStart.Add(100*time.Second), 16, true, true)
	// Only column 1 is beyond the limit: its twelve line cells and one mark.
	if strings.Count(colored, "\x1b[31m") != 13 {
		t.Fatal(colored)
	}
	for row := 0; row < 12; row++ {
		line := strings.ReplaceAll(strings.Split(colored, "\n")[11-row], "\x1b[2m", "")
		line = strings.Replace(line, "\x1b[0m", "", 1)
		_, drawing, _ := cutAtChartAxis(line)
		want := "\x1b[31m" + string(chartCell(t, plain, row, 1)) + "\x1b[0m"
		if !strings.HasPrefix(drawing, string(chartCell(t, plain, row, 0))+want) {
			t.Fatal(colored)
		}
	}
	if !strings.Contains(colored, "\x1b[31m▲\x1b[0m") || strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(colored, "\x1b[31m", ""), "\x1b[0m", ""), "\x1b[2m", "") != plain {
		t.Fatal(colored)
	}
}

func TestChartTrimsToEarliestReadingAcrossNodes(t *testing.T) {
	series := []chartSeries{
		{node: "line", readings: []readingPoint{chartPoint(600, 60, nil, nil)}},
		// Readings every 10 seconds that start 500 seconds into the span; the
		// two readings outside the span count for nothing.
		{node: "earlier", readings: []readingPoint{chartPoint(-1, 999, nil, nil), chartPoint(500, 40, nil, nil), chartPoint(510, 41, nil, nil), chartPoint(520, 42, nil, nil), chartPoint(1001, 999, nil, nil)}},
	}
	output := drawReadingsChart(series, chartStart, chartStart.Add(1000*time.Second), 80, false, true)
	lines := strings.Split(output, "\n")
	if chartCell(t, output, 0, 0) != '░' || !strings.HasPrefix(strings.TrimSpace(lines[13]), "14:38:20") || !strings.Contains(output, "no readings before 14:38:20") || !strings.Contains(output, "one column is 7s") {
		t.Fatal(output)
	}
	// A single series also starts in column zero, with no held empty prefix.
	output = drawReadingsChart(series[1:], chartStart, chartStart.Add(1000*time.Second), 80, false, false)
	if chartCell(t, output, 0, 0) != '─' {
		t.Fatal(output)
	}
	output = drawReadingsChart([]chartSeries{{readings: []readingPoint{chartPoint(0, 40, nil, nil)}}}, chartStart, chartStart.Add(1000*time.Second), 80, false, false)
	if strings.Contains(output, "no readings before") {
		t.Fatal(output)
	}
	// A first reading 48 seconds into the span, with a reading every minute,
	// is no late start: the chart starts at the start of the span.
	output = drawReadingsChart([]chartSeries{{readings: []readingPoint{chartPoint(48, 40, nil, nil), chartPoint(108, 41, nil, nil), chartPoint(168, 42, nil, nil)}}}, chartStart, chartStart.Add(time.Hour), 80, false, false)
	lines = strings.Split(output, "\n")
	if strings.Contains(output, "no readings before") || !strings.HasPrefix(strings.TrimSpace(lines[13]), "14:30") {
		t.Fatal(output)
	}
}

func TestChartLegendWrapsBetweenParts(t *testing.T) {
	series := []chartSeries{{node: "a", readings: []readingPoint{chartPoint(0, 100, floatPointer(90), nil)}}}
	parts := []string{"─ a, farthest beyond the limit", "░ all nodes, lowest to highest", "╌ limit", "▲ beyond the limit"}
	for _, test := range []struct {
		width    int
		duration string
	}{{56, "2s"}, {80, "1s"}, {120, "less than 1s"}, {20, "8s"}} {
		output := drawReadingsChart(series, chartStart, chartStart.Add(100*time.Second), test.width, false, true)
		lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")[14:]
		if lines[len(lines)-1] != "  one column is "+test.duration {
			t.Fatal(output)
		}
		var actual []string
		for index, line := range lines[:len(lines)-1] {
			if !strings.HasPrefix(line, "  ") {
				t.Fatal(output)
			}
			lineParts := strings.Split(strings.TrimPrefix(line, "  "), "   ")
			if len([]rune(line)) > test.width && len(lineParts) != 1 {
				t.Fatal(output)
			}
			if index+1 < len(lines)-1 {
				next, _, _ := strings.Cut(strings.TrimPrefix(lines[index+1], "  "), "   ")
				if len([]rune(line))+3+len([]rune(next)) <= test.width {
					t.Fatalf("legend did not pack greedily\n%s", output)
				}
			}
			actual = append(actual, lineParts...)
		}
		if strings.Join(actual, "|") != strings.Join(parts, "|") {
			t.Fatal(output)
		}
	}
}

func TestChartDuration(t *testing.T) {
	for _, test := range []struct {
		duration time.Duration
		want     string
	}{
		{51 * time.Second, "51s"}, {210500 * time.Millisecond, "3m 31s"}, {2 * time.Hour, "2h"}, {28 * time.Hour, "1d 4h"}, {999 * time.Millisecond, "less than 1s"},
	} {
		// A two-digit value label leaves 74 columns at width 80.
		output := drawReadingsChart([]chartSeries{{readings: []readingPoint{chartPoint(0, 40, nil, nil)}}}, chartStart, chartStart.Add(74*test.duration), 80, false, false)
		if !strings.HasSuffix(output, "  one column is "+test.want+"\n") {
			t.Fatalf("duration %s: %s", test.duration, output)
		}
	}
}

func floatPointer(value float64) *float64 { return &value }

func TestChartStyledAxesAndLegend(t *testing.T) {
	series := []chartSeries{{readings: []readingPoint{chartPoint(0, 91, floatPointer(90), nil)}}}
	output := drawReadingsChart(series, chartStart, chartStart.Add(time.Hour), 80, true, false)
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	for index, line := range lines[:12] {
		axis := "│"
		if index == 0 || index == 11 {
			axis = "┤"
		}
		if !strings.HasPrefix(line, "  \x1b[2m") || !strings.Contains(line, axis+"\x1b[0m") {
			t.Fatal(output)
		}
	}
	if !strings.Contains(lines[12], "└\x1b[0m\x1b[31m▲\x1b[0m\x1b[2m─\x1b[0m") {
		t.Fatal(output)
	}
	for _, line := range lines[13:] {
		if !strings.HasPrefix(line, "  \x1b[2m") || !strings.HasSuffix(line, "\x1b[0m") {
			t.Fatal(output)
		}
	}
	if !strings.Contains(output, "  \x1b[2m╌ limit   ▲ beyond the limit\x1b[0m\n") {
		t.Fatal(output)
	}
}

func TestChartTimeLabelsAcrossYears(t *testing.T) {
	since := time.Date(2025, 12, 31, 23, 0, 0, 0, time.UTC)
	until := since.Add(2 * time.Hour)
	labels := chartTimeLabels(since, until, 100)
	if !strings.HasPrefix(labels, "2025-12-31 23:00") || !strings.Contains(labels, "2026-01-01 00:00") || !strings.HasSuffix(labels, "2026-01-01 01:00") {
		t.Fatal(labels)
	}
}

func TestChartLegendTimingAndLateStart(t *testing.T) {
	for _, test := range []struct {
		start time.Time
		label string
	}{
		{chartStart, "14:30"},
		{time.Date(2026, 10, 7, 23, 50, 0, 0, time.UTC), "Oct 7 23:50"},
		{time.Date(2025, 12, 31, 23, 50, 0, 0, time.UTC), "2025-12-31 23:50"},
	} {
		// Labels 40..42 leave 74 columns at width 80. Each column covers 51 seconds.
		since, until := test.start.Add(-time.Hour), test.start.Add(74*51*time.Second)
		var readings []readingPoint
		for index := 0; index < 3; index++ {
			readings = append(readings, readingPoint{At: test.start.Add(time.Duration(index) * 10 * time.Second), MeasurementReading: store.MeasurementReading{Value: float64(40 + index)}})
		}
		output := drawReadingsChart([]chartSeries{{readings: readings}}, since, until, 80, false, false)
		want := "  one column is 51s · no readings before " + test.label + "\n"
		if !strings.HasSuffix(output, want) {
			t.Fatalf("missing %q in\n%s", want, output)
		}

		output = drawReadingsChart([]chartSeries{{readings: readings}}, since, until, 40, false, false)
		want = "  one column is 1m 51s\n  no readings before " + test.label + "\n"
		if !strings.HasSuffix(output, want) {
			t.Fatalf("missing wrapped timing %q in\n%s", want, output)
		}
	}
}
