package main

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
)

// chartSeries is one node's readings of one measurement, oldest first.
type chartSeries struct {
	node     string
	readings []readingPoint
}

func chartColor(terminal bool, noColor string) bool { return terminal && noColor == "" }

// limitMargin orders both sampled columns and the node highlighted in a fleet.
func limitMargin(reading readingPoint) float64 {
	margin := math.Inf(1)
	if reading.FailWhenMeasurementAbove != nil {
		margin = *reading.FailWhenMeasurementAbove - reading.Value
	}
	if reading.FailWhenMeasurementBelow != nil {
		margin = math.Min(margin, reading.Value-*reading.FailWhenMeasurementBelow)
	}
	if math.IsInf(margin, 1) {
		return -reading.Value
	}
	return margin
}

func readingLimits(reading readingPoint) []float64 {
	limits := []float64{}
	if reading.FailWhenMeasurementAbove != nil {
		limits = append(limits, *reading.FailWhenMeasurementAbove)
	}
	if reading.FailWhenMeasurementBelow != nil {
		limits = append(limits, *reading.FailWhenMeasurementBelow)
	}
	return limits
}

func readingBeyondLimit(reading readingPoint) bool {
	_, distance := readingLimitStatus(reading.MeasurementReading)
	return distance > 0
}

// chartColumn is what one column of the chart shows of one series: the
// reading closest to or farthest beyond its limit, which marks the column
// and joins the line, and the lowest and highest readings of the column,
// between which the column draws a stroke, so that no dip and no spike is
// lost when several readings share a column.
type chartColumn struct {
	closest, lowest, highest *readingPoint
}

// chartColumns preserves the closest, lowest, and highest reading of a
// slice, but holds its latest reading into later empty slices only while the
// median interval permits it. A column without a reading is nil.
func chartColumns(readings []readingPoint, since, until time.Time, columns int) []*chartColumn {
	within := readingsWithin(readings, since, until)
	median := medianInterval(within)
	shown := make([]*chartColumn, columns)
	span := until.Sub(since)
	next := 0
	var latest *readingPoint
	for column := range shown {
		start := since.Add(time.Duration(float64(span) * float64(column) / float64(columns)))
		for next < len(within) {
			target := 0
			if span > 0 {
				target = min(columns-1, int(float64(within[next].At.Sub(since))*float64(columns)/float64(span)))
			}
			if target > column {
				break
			}
			reading := &within[next]
			switch kept := shown[column]; {
			case kept == nil:
				shown[column] = &chartColumn{reading, reading, reading}
			default:
				if limitMargin(*reading) < limitMargin(*kept.closest) {
					kept.closest = reading
				}
				if reading.Value < kept.lowest.Value {
					kept.lowest = reading
				}
				if reading.Value > kept.highest.Value {
					kept.highest = reading
				}
			}
			latest = reading
			next++
		}
		if shown[column] == nil && latest != nil && median > 0 && start.Sub(latest.At) <= 2*median {
			shown[column] = &chartColumn{latest, latest, latest}
		}
	}
	return shown
}

// readingsWithin keeps the readings from since through until.
func readingsWithin(readings []readingPoint, since, until time.Time) []readingPoint {
	within := make([]readingPoint, 0, len(readings))
	for _, reading := range readings {
		if !reading.At.Before(since) && !reading.At.After(until) {
			within = append(within, reading)
		}
	}
	return within
}

// medianInterval is the usual time between consecutive readings, oldest
// first, or 0 when there are fewer than two.
func medianInterval(readings []readingPoint) time.Duration {
	gaps := make([]time.Duration, 0, len(readings))
	for index := 1; index < len(readings); index++ {
		gaps = append(gaps, readings[index].At.Sub(readings[index-1].At))
	}
	if len(gaps) == 0 {
		return 0
	}
	sort.Slice(gaps, func(left, right int) bool { return gaps[left] < gaps[right] })
	median := gaps[len(gaps)/2]
	if len(gaps)%2 == 0 {
		median = gaps[len(gaps)/2-1] + (median-gaps[len(gaps)/2-1])/2
	}
	return median
}

func chartRow(value, lowest, highest float64) int {
	return max(0, min(11, int(math.Round((value-lowest)/(highest-lowest)*11))))
}

// drawReadingsChart reads no terminal or environment state. Every series is
// the same measurement; nodeGroup selects a band and one node's line.
func drawReadingsChart(series []chartSeries, since, until time.Time, width int, color, nodeGroup bool) string {
	// The value axis and the line node come from every reading in the span,
	// so they do not depend on the width.
	lineNode := 0
	var closest *readingPoint
	lowest, highest := math.Inf(1), math.Inf(-1)
	chartStart, firstSeries := until, 0
	limits := []float64{}
	for index, item := range series {
		for pointIndex := range item.readings {
			reading := &item.readings[pointIndex]
			if reading.At.Before(since) || reading.At.After(until) {
				continue
			}
			if closest == nil || limitMargin(*reading) < limitMargin(*closest) || limitMargin(*reading) == limitMargin(*closest) && item.node < series[lineNode].node {
				closest, lineNode = reading, index
			}
			if reading.At.Before(chartStart) {
				chartStart, firstSeries = reading.At, index
			}
			lowest, highest = math.Min(lowest, reading.Value), math.Max(highest, reading.Value)
			for _, limit := range readingLimits(*reading) {
				lowest, highest = math.Min(lowest, limit), math.Max(highest, limit)
				limits = append(limits, limit)
			}
		}
	}
	if closest == nil {
		return ""
	}
	// The chart starts at the first reading only when it came later than the
	// usual interval allows; a first reading shortly after the start of the
	// span is no late start of the history.
	if usual := medianInterval(readingsWithin(series[firstSeries].readings, since, until)); usual == 0 || chartStart.Sub(since) <= 2*usual {
		chartStart = since
	}
	if highest == lowest {
		highest = lowest + 1
	}
	labels := make([]string, 12)
	labels[0], labels[11] = textNumber(lowest), textNumber(highest)
	// A limit's label replaces another label in its row; of two limits in one
	// row, the higher one is named.
	sort.Float64s(limits)
	for _, limit := range limits {
		labels[chartRow(limit, lowest, highest)] = textNumber(limit)
	}
	labelWidth := 1
	for _, label := range labels {
		labelWidth = max(labelWidth, len(label))
	}
	columns := max(10, width-(2+labelWidth+1)-1)
	shown := make([][]*chartColumn, len(series))
	for index, item := range series {
		shown[index] = chartColumns(item.readings, chartStart, until, columns)
	}
	cells := make([][]string, 12)
	for row := range cells {
		cells[row] = make([]string, columns)
		for column := range cells[row] {
			cells[row][column] = " "
		}
	}
	red := func(cell string, beyond bool) string {
		if color && beyond {
			return "\x1b[31m" + cell + "\x1b[0m"
		}
		return cell
	}
	marks := make([]bool, columns)
	hasLimits := false
	for column := 0; column < columns; column++ {
		bottom, top := 12, -1
		for _, points := range shown {
			at := points[column]
			if at == nil {
				continue
			}
			bottom, top = min(bottom, chartRow(at.lowest.Value, lowest, highest)), max(top, chartRow(at.highest.Value, lowest, highest))
			marks[column] = marks[column] || readingBeyondLimit(*at.closest)
		}
		if nodeGroup {
			for row := bottom; row <= top; row++ {
				cells[row][column] = "░"
			}
		}
		for _, points := range shown {
			if at := points[column]; at != nil {
				for _, limit := range readingLimits(*at.closest) {
					hasLimits = true
					row := chartRow(limit, lowest, highest)
					cells[row][column] = "╌"
				}
			}
		}
	}
	previousRow := -1
	for column, at := range shown[lineNode] {
		if at == nil {
			previousRow = -1
			continue
		}
		// The stroke from the column's lowest to its highest reading comes
		// first, and leaves a limit's row showing the limit; the line's own
		// turns are drawn over it.
		low, high := chartRow(at.lowest.Value, lowest, highest), chartRow(at.highest.Value, lowest, highest)
		for between := low; between <= high && low < high; between++ {
			if cells[between][column] != "╌" {
				cells[between][column] = red("│", readingBeyondLimit(*at.lowest) || readingBeyondLimit(*at.highest))
			}
		}
		reading := at.closest
		row := chartRow(reading.Value, lowest, highest)
		beyond := readingBeyondLimit(*reading)
		if previousRow < 0 || previousRow == row {
			cells[row][column] = red("─", beyond)
		} else {
			bottom, top := min(previousRow, row), max(previousRow, row)
			for between := bottom + 1; between < top; between++ {
				cells[between][column] = red("│", beyond)
			}
			if row > previousRow {
				cells[previousRow][column] = red("╯", beyond)
				cells[row][column] = red("╭", beyond)
			} else {
				cells[previousRow][column] = red("╮", beyond)
				cells[row][column] = red("╰", beyond)
			}
		}
		previousRow = row
	}
	var text strings.Builder
	for row := 11; row >= 0; row-- {
		axis := "│"
		if labels[row] != "" {
			axis = "┤"
		}
		fmt.Fprintf(&text, "  %s%s\n", styledText(fmt.Sprintf("%*s%s", labelWidth, labels[row], axis), "2", color), strings.Join(cells[row], ""))
	}
	fmt.Fprintf(&text, "  %s", styledText(strings.Repeat(" ", labelWidth)+"└", "2", color))
	hasMark := false
	for _, mark := range marks {
		if mark {
			text.WriteString(red("▲", true))
			hasMark = true
		} else {
			text.WriteString(styledText("─", "2", color))
		}
	}
	text.WriteByte('\n')
	fmt.Fprintf(&text, "  %s\n", styledText(strings.Repeat(" ", labelWidth)+" "+chartTimeLabels(chartStart, until, columns), "2", color))
	symbols := []string{}
	if nodeGroup {
		description := "closest to the limit"
		if len(readingLimits(*closest)) == 0 {
			description = "highest reading"
		} else if readingBeyondLimit(*closest) {
			description = "farthest beyond the limit"
		}
		//nolint:nilaway // printReadings draws a chart only for a key that has a series, and lineNode is an index into series.
		symbols = append(symbols, "─ "+series[lineNode].node+", "+description, "░ all nodes, lowest to highest")
	}
	if hasLimits {
		symbols = append(symbols, "╌ limit")
	}
	if hasMark {
		symbols = append(symbols, "▲ beyond the limit")
	}
	text.WriteString(chartLegend(symbols, "   ", width, color))
	timing := []string{"one column is " + textDuration(until.Sub(chartStart)/time.Duration(columns))}
	if chartStart.After(since) {
		timing = append(timing, "no readings before "+chartStart.UTC().Format(chartTimeFormat(chartStart, until)))
	}
	text.WriteString(chartLegend(timing, " · ", width, color))
	return text.String()
}

// chartLegend wraps whole parts; the indent and styling never affect packing.
func chartLegend(parts []string, separator string, width int, styled bool) string {
	var text strings.Builder
	line := ""
	for _, part := range parts {
		if line != "" && len([]rune(line+separator+part)) > width-2 {
			fmt.Fprintf(&text, "  %s\n", styledText(line, "2", styled))
			line = ""
		}
		if line != "" {
			line += separator
		}
		line += part
	}
	if line != "" {
		fmt.Fprintf(&text, "  %s\n", styledText(line, "2", styled))
	}
	return text.String()
}

func textDuration(duration time.Duration) string {
	if duration < time.Second {
		return "less than 1s"
	}
	seconds := int64(duration.Round(time.Second) / time.Second)
	parts := []string{}
	for _, unit := range []struct {
		seconds int64
		label   string
	}{{86400, "d"}, {3600, "h"}, {60, "m"}, {1, "s"}} {
		if amount := seconds / unit.seconds; amount > 0 {
			parts = append(parts, fmt.Sprintf("%d%s", amount, unit.label))
			seconds %= unit.seconds
		}
		if len(parts) == 2 {
			break
		}
	}
	return strings.Join(parts, " ")
}

func chartTimeFormat(since, until time.Time) string {
	since, until = since.UTC(), until.UTC()
	format := "2006-01-02 15:04"
	if since.Year() == until.Year() {
		format = "Jan 2 15:04"
	}
	if since.Format("2006-01-02") == until.Format("2006-01-02") {
		format = "15:04"
		if until.Sub(since) < 10*time.Minute {
			format = "15:04:05"
		}
	}
	return format
}

func chartTimeLabels(since, until time.Time, columns int) string {
	since, until = since.UTC(), until.UTC()
	format := chartTimeFormat(since, until)
	start, end := since.Format(format), until.Format(format)
	middle := since.Add(until.Sub(since) / 2).Format(format)
	// A very narrow chart cannot fit both endpoint labels; retain both times.
	if len(start)+len(end) > columns {
		return start + "  " + end
	}
	labels := []byte(strings.Repeat(" ", columns))
	copy(labels, start)
	endAt := columns - len(end)
	copy(labels[endAt:], end)
	middleAt := columns/2 - len(middle)/2
	if middleAt >= len(start)+2 && middleAt+len(middle)+2 <= endAt {
		copy(labels[middleAt:], middle)
	}
	return string(labels)
}

// readingRuns accounts for every breach, including readings not selected for
// the drawing. A healthy reading or a change of direction ends a run.
func readingRuns(readings []readingPoint, since, until time.Time, indent string) string {
	var text strings.Builder
	for start := 0; start < len(readings); {
		status, _ := readingLimitStatus(readings[start].MeasurementReading)
		if !readingBeyondLimit(readings[start]) {
			start++
			continue
		}
		end, farthest := start+1, start
		for end < len(readings) {
			nextStatus, _ := readingLimitStatus(readings[end].MeasurementReading)
			if nextStatus != status {
				break
			}
			if limitMargin(readings[end]) < limitMargin(readings[farthest]) {
				farthest = end
			}
			end++
		}
		first := readings[start]
		if end == start+1 {
			fmt.Fprintf(&text, "%s%s   %s %s\n", indent, textTime(first.At, since, until), textNumber(first.Value), status)
		} else {
			point := readings[farthest]
			fmt.Fprintf(&text, "%s%s to %s   %d readings %s, farthest %s at %s\n", indent, textTime(first.At, since, until), textTime(readings[end-1].At, since, until), end-start, status, textNumber(point.Value), textTime(point.At, since, until))
		}
		start = end
	}
	return text.String()
}

func measurementLimitDescription(readings []readingPoint) (string, bool) {
	limits := readingLimitDescription(readings[0].MeasurementReading)
	for _, reading := range readings[1:] {
		//nolint:nilaway // summarizeReadings makes each block with its first reading, so a block's readings are never empty.
		if !slices.Equal(readingLimits(reading), readingLimits(readings[0])) || (reading.FailWhenMeasurementAbove == nil) != (readings[0].FailWhenMeasurementAbove == nil) {
			return limits, true
		}
	}
	return limits, false
}
