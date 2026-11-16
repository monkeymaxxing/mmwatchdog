package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
	"mmwatchdog/internal/testhelpers"
)

const mib = 1024 * 1024

// openTestHistory opens a measurement history in a new directory.
func openTestHistory(t *testing.T, sizeLimit int64) (*store.MeasurementHistory, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), store.MeasurementHistoryFile)
	h, err := store.OpenMeasurementHistory(path, sizeLimit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h, path
}

// trimToLimit takes steps toward a lower size limit until the file is within
// it, as the daemon does several times a second, and returns how many steps
// it took.
func trimToLimit(t *testing.T, h *store.MeasurementHistory) int {
	t.Helper()
	for steps := 0; ; steps++ {
		shrinking, err := h.TrimStep()
		if err != nil {
			t.Fatal(err)
		}
		if !shrinking {
			return steps
		}
		if steps > 100000 {
			t.Fatal("the history does not reach its size limit")
		}
	}
}

// memoryReadings are the readings of one run of the check machine/memory.
func memoryReadings(at time.Time, percent float64) store.RunReadings {
	return store.RunReadings{At: at, CheckGroup: "machine", Check: "memory", Measurements: map[string]store.MeasurementReading{"memory_used_percent": {Value: percent, Unit: "percent"}}}
}

// largeReadings are the readings of one run of a check that reports many
// measurements, about 3 KiB of them.
func largeReadings(at time.Time, i int) store.RunReadings {
	measurements := map[string]store.MeasurementReading{}
	for n := 0; n < 40; n++ {
		measurements[fmt.Sprintf("queue_%02d_%s", n, strings.Repeat("x", 30))] = store.MeasurementReading{Value: float64(i*100 + n), Unit: "messages"}
	}
	return store.RunReadings{At: at, CheckGroup: "queues", Check: "depth", Measurements: measurements}
}

func fileBytes(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func countReadings(t *testing.T, h *store.MeasurementHistory) int {
	t.Helper()
	var n int
	if err := h.DB.QueryRow("SELECT COUNT(*) FROM readings").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A node stores the readings of every run as they were measured, in its own
// private database file, and reads them back in order after a sequence
// number, as many as fit in a size. The history survives a restart, and a
// file of another format is refused.
func TestMeasurementHistoryKeepsEveryReading(t *testing.T) {
	t.Parallel()
	h, path := openTestHistory(t, 16*mib)
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	var autoVacuum int
	if err := h.DB.QueryRow("PRAGMA auto_vacuum").Scan(&autoVacuum); err != nil || autoVacuum != 2 {
		t.Fatal("the file cannot give pages back", autoVacuum, err)
	}
	start := time.Date(2026, 10, 7, 14, 2, 30, 123456789, time.UTC)
	if reach := h.Reach(start, time.Hour); !reach.Oldest.IsZero() || !reach.Newest.IsZero() || reach.CutShortBySizeLimit {
		t.Fatalf("%+v", reach)
	}
	load := store.RunReadings{At: start.Add(time.Second), CheckGroup: "machine", Check: "load", Measurements: map[string]store.MeasurementReading{"load_average_1m": {Value: 0.42, Unit: "load"}, "load_average_1m_per_core": {Value: 0.105, Unit: "load per core"}}}
	for _, r := range []store.RunReadings{memoryReadings(start, 41), load, memoryReadings(start.Add(30*time.Second), 41.5)} {
		if err := h.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := h.After(0, mib)
	want := []store.RunReadings{memoryReadings(start, 41), load, memoryReadings(start.Add(30*time.Second), 41.5)}
	for i := range want {
		want[i].SequenceNumber = int64(i + 1)
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v %v", got, err)
	}
	if got, err = h.After(2, mib); err != nil || !reflect.DeepEqual(got, want[2:]) {
		t.Fatalf("%+v %v", got, err)
	}
	if got, err = h.After(3, mib); err != nil || len(got) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
	// The file holds the measurements as JSON text, which a person who opens
	// it can read and query.
	var kind, unit string
	if err = h.DB.QueryRow("SELECT typeof(measurements), json_extract(measurements, '$.memory_used_percent.unit') FROM readings WHERE id=1").Scan(&kind, &unit); err != nil || kind != "text" || unit != "percent" {
		t.Fatal(kind, unit, err)
	}
	// Only whole runs that fit in the size are returned, oldest first.
	if got, err = h.After(0, 200); err != nil || !reflect.DeepEqual(got, want[:1]) {
		t.Fatalf("%+v %v", got, err)
	}
	if got, err = h.After(0, 10); err != nil || len(got) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
	if reach := h.Reach(start.Add(time.Minute), time.Hour); !reach.Oldest.Equal(start) || !reach.Newest.Equal(start.Add(30*time.Second)) || reach.CutShortBySizeLimit {
		t.Fatalf("%+v", reach)
	}
	// A restart keeps the readings, their sequence numbers, and the ID of
	// the history.
	id := h.ID
	h.Close()
	again, err := store.OpenMeasurementHistory(path, 16*mib)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got, err = again.After(0, mib); err != nil || !reflect.DeepEqual(got, want) || again.ID != id || !fleet.IDPattern.MatchString(id) {
		t.Fatalf("%+v %v %s %s", got, err, again.ID, id)
	}
	if err = again.Add(memoryReadings(start.Add(time.Minute), 42)); err != nil {
		t.Fatal(err)
	}
	if got, _ = again.After(3, mib); len(got) != 1 || got[0].SequenceNumber != 4 {
		t.Fatalf("%+v", got)
	}
	if reach := again.Reach(start.Add(time.Minute), time.Hour); !reach.Oldest.Equal(start) || !reach.Newest.Equal(start.Add(time.Minute)) {
		t.Fatalf("%+v", reach)
	}
	again.Close()
	other := filepath.Join(t.TempDir(), store.MeasurementHistoryFile)
	if db, err := store.OpenDatabaseFile(other, "measurement history", store.MeasurementHistoryFormat+1, "", "CREATE TABLE t (a);"); err != nil {
		t.Fatal(err)
	} else {
		db.Close()
	}
	// MMWatchdog runs without a history that it cannot use. Such a history stores
	// nothing and says why at every write, with the file and what to do.
	unusable, err := store.OpenMeasurementHistory(other, 16*mib)
	if err == nil || !strings.Contains(err.Error(), "uses measurement history format 2; the running MMWatchdog build uses format 1") ||
		!strings.HasPrefix(err.Error(), "cannot save readings in measurement history file "+other+": ") || !strings.HasSuffix(err.Error(), "; move the measurement history file aside and restart MMWatchdog to create an empty measurement history") {
		t.Fatal(err)
	}
	if e := unusable.Add(memoryReadings(start, 41)); e != err {
		t.Fatal(e)
	}
	if _, e := unusable.AddFromNode("app-1", strings.Repeat("a", 32), 0, nil, start); e != err {
		t.Fatal(e)
	}
	got, e := unusable.After(0, mib)
	shrinking, stepError := unusable.TrimStep()
	if e != nil || len(got) != 0 || shrinking || stepError != nil || unusable.Prune(start, time.Hour) != nil || unusable.ForgetNode("app-1") != nil || unusable.ContinueAfter(5) != nil ||
		!fleet.IDPattern.MatchString(unusable.ID) || !unusable.Reach(start, time.Hour).Oldest.IsZero() || unusable.Close() != nil {
		t.Fatal(got, e, shrinking, stepError)
	}
	unusable.SetSizeLimit(1 * mib)
}

// A reading that is older than the time the history keeps readings for is
// removed, by its own time: a reading stored late with an old time goes with
// the readings of that time.
func TestMeasurementHistoryRemovesOldReadings(t *testing.T) {
	t.Parallel()
	h, _ := openTestHistory(t, 16*mib)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for _, age := range []time.Duration{8 * 24 * time.Hour, 7*24*time.Hour + time.Second, 7 * 24 * time.Hour, 3 * 24 * time.Hour, 9 * 24 * time.Hour, time.Minute} {
		if err := h.Add(memoryReadings(now.Add(-age), 40)); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Prune(now, 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	got, err := h.After(0, mib)
	ages := []time.Duration{}
	for _, r := range got {
		ages = append(ages, now.Sub(r.At))
	}
	if err != nil || !reflect.DeepEqual(ages, []time.Duration{7 * 24 * time.Hour, 3 * 24 * time.Hour, time.Minute}) {
		t.Fatal(ages, err)
	}
	// Time removed these readings, not the size limit: nothing is cut short.
	if reach := h.Reach(now, 7*24*time.Hour); !reach.Oldest.Equal(now.Add(-7*24*time.Hour)) || !reach.Newest.Equal(now.Add(-time.Minute)) || reach.CutShortBySizeLimit {
		t.Fatalf("%+v", reach)
	}
	// A shorter time, as after a reload, applies at the next prune.
	if err = h.Prune(now, 24*time.Hour); err != nil || countReadings(t, h) != 1 {
		t.Fatal(countReadings(t, h), err)
	}
	if err = h.Prune(now.Add(48*time.Hour), 24*time.Hour); err != nil || countReadings(t, h) != 0 {
		t.Fatal(countReadings(t, h), err)
	}
	if reach := h.Reach(now, time.Hour); !reach.Oldest.IsZero() || !reach.Newest.IsZero() {
		t.Fatalf("%+v", reach)
	}
}

// The file never grows beyond the size limit: when the readings reach it,
// the oldest go first. The history then says that the size limit cut it
// short, until the removed readings would have been too old anyway.
func TestMeasurementHistorySizeLimitRemovesTheOldestReadings(t *testing.T) {
	t.Parallel()
	h, path := openTestHistory(t, 1*mib)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	const runs = 1200 // about 3.6 MiB of readings
	for i := 0; i < runs; i++ {
		if err := h.Add(largeReadings(start.Add(time.Duration(i)*time.Minute), i)); err != nil {
			t.Fatal(i, err)
		}
		if size := fileBytes(t, path); size > 1*mib {
			t.Fatalf("after %d runs the file holds %d bytes", i+1, size)
		}
	}
	end := start.Add((runs - 1) * time.Minute)
	got, err := h.After(0, 64*mib)
	if err != nil || len(got) < 100 || len(got) >= runs {
		t.Fatal(len(got), err)
	}
	// What is left is the newest readings without a gap.
	for i, r := range got {
		if want := end.Add(-time.Duration(len(got)-1-i) * time.Minute); !r.At.Equal(want) || r.SequenceNumber != int64(runs-len(got)+1+i) {
			t.Fatalf("reading %d of %d: %v %d, want %v", i, len(got), r.At, r.SequenceNumber, want)
		}
	}
	keep := 7 * 24 * time.Hour
	reach := h.Reach(end, keep)
	if !reach.Oldest.Equal(got[0].At) || !reach.Newest.Equal(end) || !reach.CutShortBySizeLimit {
		t.Fatalf("%+v", reach)
	}
	if sentence := store.HistoryCutShortSentence("measurement history", reach.Oldest, "7d", "1MiB", end.Add(5*24*time.Hour+3*time.Hour-end.Sub(reach.Oldest))); sentence != "measurement history reaches back 5 days 3 hours; the size limit of 1 MiB prevented keeping the requested 7 days" {
		t.Fatal(sentence)
	}
	// Seven days after the newest reading that the size limit removed, time
	// would have removed it too.
	removedUntil := got[0].At.Add(-time.Minute)
	if h.RemovedBySizeLimitUntil != removedUntil {
		t.Fatal(h.RemovedBySizeLimitUntil, removedUntil)
	}
	if h.Reach(removedUntil.Add(keep-time.Second), keep).CutShortBySizeLimit != true || h.Reach(removedUntil.Add(keep), keep).CutShortBySizeLimit != false {
		t.Fatal("the history is cut short for as long as the removed readings would have been kept")
	}
	// A restart keeps what the size limit removed.
	h.Close()
	again, err := store.OpenMeasurementHistory(path, 1*mib)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.RemovedBySizeLimitUntil != removedUntil || !again.Reach(end, keep).CutShortBySizeLimit || countReadings(t, again) != len(got) {
		t.Fatal(again.RemovedBySizeLimitUntil, countReadings(t, again))
	}
	// A daemon that starts with a higher limit is no longer cut short by it.
	again.Close()
	raised, err := store.OpenMeasurementHistory(path, 4*mib)
	if err != nil {
		t.Fatal(err)
	}
	defer raised.Close()
	trimToLimit(t, raised)
	if raised.Reach(end, keep).CutShortBySizeLimit || countReadings(t, raised) != len(got) {
		t.Fatal("a history opened under a higher size limit says that this limit cut it short")
	}
}

// A lower size limit applies at once, when the daemon starts and when a
// reload states it: the oldest readings go, and the file shrinks to the new
// limit. A higher one gives the history room again.
func TestMeasurementHistoryAppliesAChangedSizeLimit(t *testing.T) {
	h, path := openTestHistory(t, 8*mib)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 1000; i++ {
		if err := h.Add(largeReadings(start.Add(time.Duration(i)*time.Minute), i)); err != nil {
			t.Fatal(i, err)
		}
	}
	before := fileBytes(t, path)
	if before < 3*mib || countReadings(t, h) != 1000 || h.Reach(start.Add(24*time.Hour), 7*24*time.Hour).CutShortBySizeLimit {
		t.Fatal(before, countReadings(t, h))
	}
	// A lower limit does no work when it is stated: the daemon stays free
	// for its checks, and the file shrinks in steps. Between the steps new
	// readings are stored, and the file never grows.
	removalRows, vacuumPages := store.HistoryRemovalRows, store.HistoryVacuumPages
	store.HistoryRemovalRows, store.HistoryVacuumPages = 100, 64
	defer func() { store.HistoryRemovalRows, store.HistoryVacuumPages = removalRows, vacuumPages }()
	// The file's pages, counted with those that wait in its write-ahead log.
	pagesInFile := func() int64 {
		t.Helper()
		var pages int64
		if err := h.DB.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
			t.Fatal(err)
		}
		return pages
	}
	pagesBefore := pagesInFile()
	h.SetSizeLimit(1 * mib)
	if countReadings(t, h) != 1000 || pagesInFile() != pagesBefore {
		t.Fatal("stating a lower size limit removed readings at once", countReadings(t, h))
	}
	steps, largestLog := 0, int64(0)
	for i := 1000; ; i++ {
		held := countReadings(t, h)
		shrinking, err := h.TrimStep()
		if err != nil {
			t.Fatal(err)
		}
		if !shrinking {
			break
		}
		steps++
		// A step removes at most HistoryRemovalRows readings, and so does a
		// write between two steps: neither does the whole removal at once.
		if removed := held - countReadings(t, h); removed > int(store.HistoryRemovalRows) {
			t.Fatalf("one step removed %d readings", removed)
		}
		held = countReadings(t, h)
		if err = h.Add(largeReadings(start.Add(time.Duration(i)*time.Minute), i)); err != nil {
			t.Fatal(i, err)
		}
		if removed := held + 1 - countReadings(t, h); removed > int(store.HistoryRemovalRows) {
			t.Fatalf("one write removed %d readings while the file shrinks", removed)
		}
		if pages := pagesInFile(); pages > pagesBefore {
			t.Fatalf("the file grew from %d to %d pages while it shrinks", pagesBefore, pages)
		}
		if info, err := os.Stat(path + "-wal"); err == nil {
			largestLog = max(largestLog, info.Size())
		}
	}
	left := countReadings(t, h)
	newest := 1000 + steps
	if size := fileBytes(t, path); size > 1*mib || left < 100 || left >= 1000 || steps < 10 || largestLog > 2*mib || !h.Reach(start.Add(48*time.Hour), 7*24*time.Hour).CutShortBySizeLimit {
		t.Fatal(size, left, steps, largestLog)
	}
	// A step costs nothing once the file is within its limit.
	if shrinking, err := h.TrimStep(); shrinking || err != nil || countReadings(t, h) != left {
		t.Fatal(shrinking, err)
	}
	// A higher limit has removed no reading: the history is no longer cut
	// short by it. The lower limit, stated again, has.
	h.SetSizeLimit(8 * mib)
	if trimToLimit(t, h) != 0 || h.Reach(start.Add(48*time.Hour), 7*24*time.Hour).CutShortBySizeLimit {
		t.Fatal("a raised size limit still counts as the limit that cut the history short")
	}
	h.SetSizeLimit(1 * mib)
	if trimToLimit(t, h); !h.Reach(start.Add(48*time.Hour), 7*24*time.Hour).CutShortBySizeLimit {
		t.Fatal("the lower limit, stated again, does not count as the limit that cut the history short")
	}
	// The newest reading is still there, and new ones fit.
	got, err := h.After(int64(newest-1), mib)
	if err != nil || len(got) != 1 || got[0].SequenceNumber != int64(newest) {
		t.Fatalf("%+v %v", got, err)
	}
	for i := newest; i < newest+400; i++ {
		if err = h.Add(largeReadings(start.Add(time.Duration(i)*time.Minute), i)); err != nil {
			t.Fatal(i, err)
		}
	}
	if size := fileBytes(t, path); size > 1*mib {
		t.Fatal(size)
	}
	// The daemon starts with a lower limit than the file was written under.
	h.Close()
	again, err := store.OpenMeasurementHistory(path, 8*mib)
	if err != nil {
		t.Fatal(err)
	}
	for i := newest + 400; i < newest+1400; i++ {
		if err = again.Add(largeReadings(start.Add(time.Duration(i)*time.Minute), i)); err != nil {
			t.Fatal(i, err)
		}
	}
	grown := countReadings(t, again)
	again.Close()
	if fileBytes(t, path) < 3*mib || grown < 1000 {
		t.Fatal(fileBytes(t, path), grown)
	}
	lower, err := store.OpenMeasurementHistory(path, 2*mib)
	if err != nil {
		t.Fatal(err)
	}
	defer lower.Close()
	if countReadings(t, lower) != grown {
		t.Fatal("opening the history under a lower size limit removed readings at once")
	}
	if steps := trimToLimit(t, lower); steps < 2 {
		t.Fatal(steps)
	}
	if size, left := fileBytes(t, path), countReadings(t, lower); size > 2*mib || left >= grown || left < 200 {
		t.Fatal(size, left, grown)
	}
}

// A hub saves the readings that a node sends with a report, together with
// how far it has received them, so a report that is sent again adds nothing
// twice. Readings count only when they continue from where the hub is. A
// node with a new measurement history starts over, and forgetting a node
// removes its readings.
func TestMeasurementHistoryOfNodes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), store.MeasurementHistoryOfNodesFile)
	h, err := store.OpenMeasurementHistory(path, 16*mib)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	history, other := strings.Repeat("a", 32), strings.Repeat("b", 32)
	run := func(sequenceNumber int64, at time.Time) store.RunReadings {
		r := memoryReadings(at, float64(sequenceNumber))
		r.SequenceNumber = sequenceNumber
		return r
	}
	of := func(node string) []int64 {
		t.Helper()
		rows, err := h.DB.Query("SELECT kept_from, measurements FROM readings WHERE node=? ORDER BY id", node)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := []int64{}
		for rows.Next() {
			var keptFrom int64
			var measurements string
			rows.Scan(&keptFrom, &measurements)
			var value int64
			fmt.Sscanf(measurements, `{"memory_used_percent":{"value":%d`, &value)
			out = append(out, value)
			if time.Unix(0, keptFrom).After(now) {
				t.Fatalf("the age of a reading of %s counts from after its receipt", node)
			}
		}
		return out
	}
	if got := h.ReceivedFrom("app-1", history); got != 0 {
		t.Fatal(got)
	}
	// The first readings of a node need not start at 1: the node may have
	// removed older ones.
	saved, err := h.AddFromNode("app-1", history, 0, []store.RunReadings{run(5, now.Add(-2*time.Minute)), run(6, now.Add(-time.Minute))}, now)
	if err != nil || saved != 6 || h.ReceivedFrom("app-1", history) != 6 {
		t.Fatal(saved, err)
	}
	// The same report again, and readings that do not continue from 6, add
	// nothing and tell the node where the hub is.
	for _, after := range []int64{0, 5, 7} {
		if saved, err = h.AddFromNode("app-1", history, after, []store.RunReadings{run(after+1, now)}, now); err != nil || saved != 6 {
			t.Fatal(after, saved, err)
		}
	}
	// A reading from a clock ahead of the hub's keeps the time of its run,
	// and its age counts from its receipt: the sender's clock cannot keep it
	// longer than the measurement history settings on the hub say.
	ahead := now.Add(365 * 24 * time.Hour)
	if saved, err = h.AddFromNode("app-1", history, 6, []store.RunReadings{run(7, ahead)}, now); err != nil || saved != 7 {
		t.Fatal(saved, err)
	}
	var at, keptFrom int64
	if err = h.DB.QueryRow("SELECT at, kept_from FROM readings WHERE node='app-1' ORDER BY id DESC LIMIT 1").Scan(&at, &keptFrom); err != nil || at != ahead.UnixNano() || keptFrom != now.UnixNano() {
		t.Fatal(time.Unix(0, at), time.Unix(0, keptFrom), err)
	}
	if saved, err = h.AddFromNode("app-2", other, 0, []store.RunReadings{run(1, now)}, now); err != nil || saved != 1 {
		t.Fatal(saved, err)
	}
	if got := of("app-1"); !reflect.DeepEqual(got, []int64{5, 6, 7}) {
		t.Fatal(got)
	}
	if reach := h.Reach(now, time.Hour); !reach.Oldest.Equal(now.Add(-2*time.Minute)) || !reach.Newest.Equal(now) {
		t.Fatalf("%+v", reach)
	}
	// A report without readings changes nothing.
	if saved, err = h.AddFromNode("app-1", history, 7, nil, now); err != nil || saved != 7 {
		t.Fatal(saved, err)
	}
	// A restarted hub knows how far it received each node's readings.
	h.Close()
	if h, err = store.OpenMeasurementHistory(path, 16*mib); err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if h.ReceivedFrom("app-1", history) != 7 || h.ReceivedFrom("app-2", other) != 1 || !reflect.DeepEqual(h.NodesWithReadings(), []string{"app-1", "app-2"}) {
		t.Fatal(h.Received)
	}
	// app-1 has a new measurement history: its sequence numbers start over,
	// and the readings that the hub holds stay.
	if got := h.ReceivedFrom("app-1", other); got != 0 {
		t.Fatal(got)
	}
	if saved, err = h.AddFromNode("app-1", other, 7, []store.RunReadings{run(8, now)}, now); err != nil || saved != 0 {
		t.Fatal(saved, err)
	}
	if saved, err = h.AddFromNode("app-1", other, 0, []store.RunReadings{run(1, now), run(2, now)}, now); err != nil || saved != 2 {
		t.Fatal(saved, err)
	}
	if got := of("app-1"); !reflect.DeepEqual(got, []int64{5, 6, 7, 1, 2}) {
		t.Fatal(got)
	}
	// More readings than one transaction holds arrive whole.
	many := []store.RunReadings{}
	for i := 0; i < 400; i++ {
		r := largeReadings(now.Add(-time.Duration(400-i)*time.Second), i)
		r.SequenceNumber = int64(i + 1)
		many = append(many, r)
	}
	if saved, err = h.AddFromNode("db-1", history, 0, many, now); err != nil || saved != 400 {
		t.Fatal(saved, err)
	}
	var count int
	if err = h.DB.QueryRow("SELECT COUNT(*) FROM readings WHERE node='db-1'").Scan(&count); err != nil || count != 400 {
		t.Fatal(count, err)
	}
	// Forgetting a node removes its readings and where the hub was.
	if err = h.ForgetNode("app-1"); err != nil || len(of("app-1")) != 0 || h.ReceivedFrom("app-1", other) != 0 || len(of("app-2")) != 1 || !reflect.DeepEqual(h.NodesWithReadings(), []string{"app-2", "db-1"}) {
		t.Fatal(err, of("app-1"), h.NodesWithReadings())
	}
	// A restarted hub has forgotten the node too.
	h.Close()
	if h, err = store.OpenMeasurementHistory(path, 16*mib); err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if h.ReceivedFrom("app-1", other) != 0 || !reflect.DeepEqual(h.NodesWithReadings(), []string{"app-2", "db-1"}) {
		t.Fatal(h.Received)
	}
	// The hub's size limit removes the oldest readings, whichever node they
	// came from.
	h.SetSizeLimit(1 * mib)
	trimToLimit(t, h)
	if fileBytes(t, path) > 1*mib || len(of("app-2")) != 1 {
		t.Fatal(fileBytes(t, path), of("app-2"))
	}
	if err = h.DB.QueryRow("SELECT COUNT(*) FROM readings WHERE node='db-1'").Scan(&count); err != nil || count >= 400 || count < 50 {
		t.Fatal(count, err)
	}
}

func TestDaysAndHours(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{
		5*24*time.Hour + 3*time.Hour + 20*time.Minute: "5 days 3 hours",
		7 * 24 * time.Hour:            "7 days",
		24*time.Hour + 30*time.Minute: "1 day",
		25 * time.Hour:                "1 day 1 hour",
		90 * time.Minute:              "1 hour 30 minutes",
		61 * time.Second:              "1 minute 1 second",
		45 * time.Second:              "45 seconds",
		400 * time.Millisecond:        "0 seconds",
		-time.Hour:                    "0 seconds",
	} {
		if got := store.DaysAndHours(d); got != want {
			t.Errorf("%v: got %q, want %q", d, got, want)
		}
	}
	if got := store.HistoryCutShortSentence("measurement history of nodes", time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC), "30d", "20GiB", time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)); got != "measurement history of nodes reaches back 5 days 3 hours; the size limit of 20 GiB prevented keeping the requested 30 days" {
		t.Fatal(got)
	}
}

// historyDaemon starts a daemon with a TLS check, whose every run measures
// certificate_expires_in_seconds, beside the shell check of daemonConfig, which measures nothing.
func historyDaemon(t *testing.T) (*Daemon, string, *config.Config) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(server.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	p, c := daemonConfig(t)
	c.CheckGroups["certificate"] = checksandalerts.CheckGroup{Check: checksandalerts.Check{Inputs: &checksandalerts.TLS{Address: server.Listener.Addr().String(), ServerName: "example.com", TrustedCertificateAuthorityFile: ca, FailWhenCertificateExpiresWithin: "1h"}}}
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, p, c
}

// storedReadings returns every run that the node's measurement history holds.
func storedReadings(t *testing.T, d *Daemon) []store.RunReadings {
	t.Helper()
	readings, err := d.history.After(0, 64*mib)
	if err != nil {
		t.Fatal(err)
	}
	return readings
}

// measurementHistoryStatusOf returns the measurement_history of the status.
func measurementHistoryStatusOf(t *testing.T, d *Daemon) store.MeasurementHistoryStatus {
	t.Helper()
	w := httptest.NewRecorder()
	d.routes().ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/status", nil))
	var status struct {
		MeasurementHistory *store.MeasurementHistoryStatus `json:"measurement_history"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || status.MeasurementHistory == nil {
		t.Fatal(w.Body.String(), err)
	}
	return *status.MeasurementHistory
}

// Each scheduled run of a check that measures a number stores its readings in
// the node's measurement history: the run's time, its check, and each
// measurement with its value and unit. A check that measures nothing stores
// nothing, and neither does mmwatchdog check-now. A maintenance window holds alerts,
// not readings. The status says how far back the history reaches, and the
// history survives a daemon restart.
func TestEveryScheduledRunStoresItsReadings(t *testing.T) {
	t.Parallel()
	d, p, _ := historyDaemon(t)
	now := time.Now().UTC()
	if status := measurementHistoryStatusOf(t, d); status.OldestReadingAt != nil || status.NewestReadingAt != nil || status.KeepMeasurementHistoryFor != "7d" || status.MeasurementHistorySizeLimit != "256MiB" || status.CutShortByMeasurementHistorySizeLimit {
		t.Fatalf("%+v", status)
	}
	tick(d, now)
	readings := storedReadings(t, d)
	if len(readings) != 1 {
		t.Fatalf("%+v", readings)
	}
	d.mu.Lock()
	last := d.snapshot.Checks["certificate/tls"].Last
	d.mu.Unlock()
	first := readings[0]
	expires := first.Measurements["certificate_expires_in_seconds"]
	if last.Status != "ok" || first.CheckGroup != "certificate" || first.Check != "tls" || !first.At.Equal(last.At) || first.SequenceNumber != 1 ||
		len(first.Measurements) != 1 || expires.Unit != "seconds" || expires.Value != last.Measurements["certificate_expires_in_seconds"].Value || expires.Value <= 0 {
		t.Fatalf("%+v %+v", first, last)
	}
	// mmwatchdog check-now measures and stores nothing.
	if obs, err := d.checkNowGroup(context.Background(), "certificate"); err != nil || len(obs["tls"].Measurements) != 1 || len(storedReadings(t, d)) != 1 {
		t.Fatal(obs, err, storedReadings(t, d))
	}
	tick(d, now.Add(30*time.Second))
	// In maintenance the check keeps running, and its readings are stored.
	setMaintenance(t, d, "certificate", "10m")
	tick(d, now.Add(31*time.Second))
	readings = storedReadings(t, d)
	if len(readings) != 3 || readings[2].SequenceNumber != 3 || readings[2].At.Before(readings[1].At) {
		t.Fatalf("%+v", readings)
	}
	status := measurementHistoryStatusOf(t, d)
	if status.OldestReadingAt == nil || !status.OldestReadingAt.Equal(first.At) || !status.NewestReadingAt.Equal(readings[2].At) || status.CutShortByMeasurementHistorySizeLimit || status.CutShortSentence(now) != "" {
		t.Fatalf("%+v", status)
	}
	if info, err := os.Stat(filepath.Join(d.config.StateDir, "measurement_history.db")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	// A restarted daemon has the same history and adds to it.
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
	if got := storedReadings(t, restarted); !reflect.DeepEqual(got, readings) {
		t.Fatalf("%+v", got)
	}
	tick(restarted, time.Now().UTC())
	if got := storedReadings(t, restarted); len(got) != 4 || got[3].SequenceNumber != 4 {
		t.Fatalf("%+v", got)
	}
}

// A reading that MMWatchdog cannot store is a storage problem that the status shows
// until a write succeeds again. The check's own state does not depend on it.
func TestAReadingThatCannotBeStoredIsAStorageProblem(t *testing.T) {
	t.Parallel()
	d, _, _ := historyDaemon(t)
	now := time.Now().UTC()
	if _, err := d.history.DB.Exec("CREATE TRIGGER reject_reading BEFORE INSERT ON readings BEGIN SELECT RAISE(FAIL, 'fixture storage failure'); END"); err != nil {
		t.Fatal(err)
	}
	tick(d, now)
	d.mu.Lock()
	checks, _, storageError := d.status("", now, nil)
	summary := incidents.Summarize(checks, storageError)
	d.mu.Unlock()
	if !strings.Contains(storageError, "measurement history write: ") || !strings.Contains(storageError, "fixture storage failure") || summary.ExitCode != 3 || checks["certificate/tls"].State != "ok" || len(storedReadings(t, d)) != 0 {
		t.Fatal(storageError, summary, checks["certificate/tls"].State)
	}
	if _, err := d.history.DB.Exec("DROP TRIGGER reject_reading"); err != nil {
		t.Fatal(err)
	}
	tick(d, now.Add(30*time.Second))
	d.mu.Lock()
	_, _, storageError = d.status("", now, nil)
	d.mu.Unlock()
	if storageError != "" || len(storedReadings(t, d)) != 1 {
		t.Fatal(storageError, storedReadings(t, d))
	}
}

// The two settings of the measurement history apply when the daemon starts
// and when a reload changes them: a lower measurement_history_size_limit at
// once, and a shorter keep_measurement_history_for at the next retention
// tick. When the size limit cut the history short, the status says so.
func TestTheDaemonAppliesTheSettingsOfTheMeasurementHistory(t *testing.T) {
	t.Parallel()
	d, p, c := historyDaemon(t)
	now := time.Now().UTC()
	// Readings of the last 20 hours, about 3 MiB of them, and one reading
	// that is older than seven days.
	if err := d.history.Add(largeReadings(now.Add(-8*24*time.Hour), 0)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		if err := d.history.Add(largeReadings(now.Add(-time.Duration(1000-i)*72*time.Second), i)); err != nil {
			t.Fatal(i, err)
		}
	}
	path := filepath.Join(c.StateDir, "measurement_history.db")
	if len(storedReadings(t, d)) != 1001 || fileBytes(t, path) < 3*mib {
		t.Fatal(len(storedReadings(t, d)), fileBytes(t, path))
	}
	// A daemon start removes what became too old while MMWatchdog did not run.
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if got := storedReadings(t, d); len(got) != 1000 || got[0].SequenceNumber != 2 || measurementHistoryStatusOf(t, d).CutShortByMeasurementHistorySizeLimit {
		t.Fatal(len(got))
	}
	// A reload with a lower size limit does no removal itself: the daemon
	// shrinks the file in steps, several times a second.
	c.MeasurementHistorySizeLimit = "1MiB"
	writeConfig(t, p, c)
	if _, err = d.reload(); err != nil {
		t.Fatal(err)
	}
	if len(storedReadings(t, d)) != 1000 {
		t.Fatal("the reload removed readings while it held the daemon's lock")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.notifications(ctx); close(done) }()
	for deadline := time.Now().Add(testhelpers.LongestWaitForAnEvent); fileBytes(t, path) > 1*mib; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the file did not shrink to the lower size limit", fileBytes(t, path))
		}
	}
	cancel()
	<-done
	left := len(storedReadings(t, d))
	status := measurementHistoryStatusOf(t, d)
	if fileBytes(t, path) > 1*mib || left >= 1000 || left < 100 || !status.CutShortByMeasurementHistorySizeLimit || status.MeasurementHistorySizeLimit != "1MiB" {
		t.Fatal(fileBytes(t, path), left, status)
	}
	sentence := status.CutShortSentence(now)
	if !strings.HasPrefix(sentence, "measurement history reaches back ") || !strings.HasSuffix(sentence, " hours; the size limit of 1 MiB prevented keeping the requested 7 days") && !strings.HasSuffix(sentence, " minutes; the size limit of 1 MiB prevented keeping the requested 7 days") {
		t.Fatal(sentence)
	}
	// A reload with a shorter time asks for the retention step at once,
	// which removes the readings that are older.
	asksForRetention(t, d, func() {
		c.KeepMeasurementHistoryFor = "1h"
		writeConfig(t, p, c)
		if _, err = d.reload(); err != nil {
			t.Fatal(err)
		}
	})
	d.retention(now)
	got := storedReadings(t, d)
	if len(got) != 50 || got[0].At.Before(now.Add(-time.Hour)) || measurementHistoryStatusOf(t, d).KeepMeasurementHistoryFor != "1h" {
		t.Fatal(len(got))
	}
	// One hour later the removed readings would have been too old anyway.
	if reach := d.history.Reach(now.Add(time.Hour), time.Hour); reach.CutShortBySizeLimit {
		t.Fatalf("%+v", reach)
	}
}

// The daemon's retention tick removes the readings that are older than
// keep_measurement_history_for while MMWatchdog runs.
func TestTheRetentionTickRemovesOldReadings(t *testing.T) {
	t.Parallel()
	d, _, _ := historyDaemon(t)
	now := time.Now().UTC()
	for _, age := range []time.Duration{8 * 24 * time.Hour, time.Hour} {
		if err := d.history.Add(memoryReadings(now.Add(-age), 40)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.notifications(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	for deadline := time.Now().Add(testhelpers.LongestWaitForAnEvent); len(storedReadings(t, d)) != 1; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the reading that is older than seven days is still there")
		}
	}
	if got := storedReadings(t, d); !got[0].At.Equal(now.Add(-time.Hour)) {
		t.Fatalf("%+v", got)
	}
	// After a reload the worker runs the retention step at once, not a
	// minute after its last one.
	if err := d.history.Add(memoryReadings(now.Add(-8*24*time.Hour), 40)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.reload(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(testhelpers.LongestWaitForAnEvent); len(storedReadings(t, d)) != 1; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the reload did not bring the retention step forward")
		}
	}
}

// asksForRetention runs reload, a function that reloads the daemon, and
// fails the test unless the reload asked the notifications worker for its
// retention step.
func asksForRetention(t *testing.T, d *Daemon, reload func()) {
	t.Helper()
	select {
	case <-d.runRetentionNow:
	default:
	}
	reload()
	select {
	case <-d.runRetentionNow:
	default:
		t.Fatal("the reload did not ask for the retention step")
	}
}

// A measurement history can hold many gigabytes, so no statement that runs
// while MMWatchdog works may read the whole table or a whole index. Each one finds
// its rows through the sequence number or through an index, and reads an
// index in order only up to a limit.
func TestMeasurementHistoryStatementsReadNoWholeTable(t *testing.T) {
	t.Parallel()
	h, _ := openTestHistory(t, 16*mib)
	plan := func(statement string, arguments ...any) string {
		t.Helper()
		rows, err := h.DB.Query("EXPLAIN QUERY PLAN "+statement, arguments...)
		if err != nil {
			t.Fatal(statement, err)
		}
		defer rows.Close()
		lines := []string{}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, detail)
		}
		return strings.Join(lines, "\n")
	}
	const orderedByTime = "SCAN readings USING COVERING INDEX readings_kept_from"
	for _, tc := range []struct {
		statement string
		arguments []any
		// scans is how often the plan may read an index in order: only the
		// statement that removes the oldest readings does, up to its limit.
		scans int
		uses  string
	}{
		{store.HistoryReachQuery, nil, 0, "SEARCH readings USING COVERING INDEX readings_kept_from"},
		{store.HistorySequenceNumbersQuery, nil, 0, "SEARCH readings"},
		{store.HistoryReadingsAfterQuery, []any{5}, 0, "SEARCH readings USING INTEGER PRIMARY KEY (rowid>?)"},
		{store.HistoryRemoveOldest, []any{64}, 1, orderedByTime},
		{store.HistoryRemoveOlderThan, []any{5, 100}, 0, "SEARCH readings USING COVERING INDEX readings_kept_from (kept_from<?)"},
		{store.HistoryRemoveOfNode, []any{"app-1", 5, 100}, 0, "SEARCH readings USING COVERING INDEX readings_node (node=? AND id<?)"},
	} {
		got := plan(tc.statement, tc.arguments...)
		if strings.Count(got, "SCAN readings") != tc.scans || !strings.Contains(got, tc.uses) {
			t.Errorf("%s\nplan:\n%s", tc.statement, got)
		}
	}
}

// Once the readings that the size limit removed would be too old anyway, the
// history is as long as its time allows, and nothing is left to say about
// the size limit: also when keep_measurement_history_for is raised later.
func TestMeasurementHistoryForgetsWhatTheSizeLimitRemovedOnceItIsOld(t *testing.T) {
	t.Parallel()
	h, path := openTestHistory(t, 1*mib)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 600; i++ {
		if err := h.Add(largeReadings(start.Add(time.Duration(i)*time.Minute), i)); err != nil {
			t.Fatal(i, err)
		}
	}
	week := 7 * 24 * time.Hour
	removedUntil := h.RemovedBySizeLimitUntil
	if removedUntil.IsZero() || !h.Reach(start.Add(24*time.Hour), week).CutShortBySizeLimit {
		t.Fatal("the size limit removed nothing")
	}
	// A retention tick one second before the removed readings would be too
	// old keeps the note, and the tick after it drops the note.
	if err := h.Prune(removedUntil.Add(week-time.Second), 100*week); err != nil || h.RemovedBySizeLimitUntil != removedUntil {
		t.Fatal(err, h.RemovedBySizeLimitUntil)
	}
	if err := h.Prune(removedUntil.Add(week-time.Second), week); err != nil || h.RemovedBySizeLimitUntil != removedUntil {
		t.Fatal(err, h.RemovedBySizeLimitUntil)
	}
	if err := h.Prune(removedUntil.Add(week), 100*week); err != nil || h.RemovedBySizeLimitUntil != removedUntil {
		t.Fatal("a longer time dropped the note", err)
	}
	later := removedUntil.Add(30 * 24 * time.Hour)
	if !h.Reach(later, 90*24*time.Hour).CutShortBySizeLimit {
		t.Fatal("this is the case that the retention tick prevents")
	}
	if err := h.Prune(removedUntil.Add(week), week); err != nil || !h.RemovedBySizeLimitUntil.IsZero() || h.RemovedUnderMaxPages != 0 {
		t.Fatal(err, h.RemovedBySizeLimitUntil)
	}
	// Thirty days later the node keeps readings for 90 days: the history is
	// short because it was short before, not because of its size.
	if h.Reach(later, 90*24*time.Hour).CutShortBySizeLimit {
		t.Fatal("a longer keep_measurement_history_for made an old removal by size count again")
	}
	h.Close()
	again, err := store.OpenMeasurementHistory(path, 1*mib)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if !again.RemovedBySizeLimitUntil.IsZero() || again.Reach(later, 90*24*time.Hour).CutShortBySizeLimit {
		t.Fatal(again.RemovedBySizeLimitUntil)
	}
}

// A removal of many readings is many short steps: by age, and for a
// forgotten node. Each step takes the history's lock on its own, so reports
// and checks run between them.
func TestMeasurementHistoryRemovesManyReadingsInSteps(t *testing.T) {
	removalRows := store.HistoryRemovalRows
	store.HistoryRemovalRows = 100
	defer func() { store.HistoryRemovalRows = removalRows }()
	path := filepath.Join(t.TempDir(), store.MeasurementHistoryOfNodesFile)
	h, err := store.OpenMeasurementHistory(path, 64*mib)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	history := strings.Repeat("a", 32)
	runs := func(n int, age time.Duration) []store.RunReadings {
		out := []store.RunReadings{}
		for i := 1; i <= n; i++ {
			r := memoryReadings(now.Add(-age).Add(time.Duration(i)*time.Millisecond), float64(i))
			r.SequenceNumber = int64(i)
			out = append(out, r)
		}
		return out
	}
	count := func(node string) int {
		t.Helper()
		var n int
		if err := h.DB.QueryRow("SELECT COUNT(*) FROM readings WHERE node=?", node).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for node, age := range map[string]time.Duration{"old-1": 8 * 24 * time.Hour, "app-1": time.Hour, "app-2": 2 * time.Hour} {
		if saved, err := h.AddFromNode(node, history, 0, runs(350, age), now); err != nil || saved != 350 {
			t.Fatal(node, saved, err)
		}
	}
	// 350 readings are older than seven days: four steps of 100.
	if err = h.Prune(now, 7*24*time.Hour); err != nil || count("old-1") != 0 || count("app-1") != 350 || count("app-2") != 350 {
		t.Fatal(err, count("old-1"), count("app-1"), count("app-2"))
	}
	if reach := h.Reach(now, 7*24*time.Hour); !reach.Oldest.Equal(now.Add(-2 * time.Hour).Add(time.Millisecond)) {
		t.Fatalf("%+v", reach)
	}
	if err = h.ForgetNode("app-2"); err != nil || count("app-2") != 0 || count("app-1") != 350 || h.ReceivedFrom("app-2", history) != 0 || h.ReceivedFrom("app-1", history) != 350 {
		t.Fatal(err, count("app-2"), count("app-1"))
	}
	if reach := h.Reach(now, 7*24*time.Hour); !reach.Oldest.Equal(now.Add(-time.Hour).Add(time.Millisecond)) {
		t.Fatalf("%+v", reach)
	}
	// A node that joins again under the name while the hub still removes the
	// old readings starts over, and its new readings stay.
	last, err := h.ForgetReceived("app-1")
	if err != nil || last == 0 || h.ReceivedFrom("app-1", history) != 0 {
		t.Fatal(last, err)
	}
	more, err := h.RemoveReadingsOfNodeStep("app-1", last)
	if err != nil || !more || count("app-1") != 250 {
		t.Fatal(more, err, count("app-1"))
	}
	newHistory := strings.Repeat("b", 32)
	if saved, err := h.AddFromNode("app-1", newHistory, 0, runs(5, time.Minute), now); err != nil || saved != 5 {
		t.Fatal(saved, err)
	}
	for steps := 0; more; steps++ {
		if more, err = h.RemoveReadingsOfNodeStep("app-1", last); err != nil || steps > 10 {
			t.Fatal(steps, err)
		}
	}
	if count("app-1") != 5 || h.ReceivedFrom("app-1", newHistory) != 5 {
		t.Fatal(count("app-1"), h.ReceivedFrom("app-1", newHistory))
	}
}

// The sequence numbers of a node's readings never go back behind the number
// that the hub holds: a history that was restored from an older copy
// continues after the hub's number, so the hub takes its new readings.
func TestMeasurementHistoryContinuesAfterTheHubsSequenceNumber(t *testing.T) {
	t.Parallel()
	h, path := openTestHistory(t, 16*mib)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	next := func() int64 {
		t.Helper()
		if err := h.Add(memoryReadings(now, 41)); err != nil {
			t.Fatal(err)
		}
		return h.LastSequenceNumber
	}
	// An empty history whose hub holds readings up to 50.
	if err := h.ContinueAfter(50); err != nil || next() != 51 || next() != 52 {
		t.Fatal(err, h.LastSequenceNumber)
	}
	// The hub is behind or level: nothing changes.
	for _, held := range []int64{0, 7, 52} {
		if err := h.ContinueAfter(held); err != nil || h.LastSequenceNumber != 52 {
			t.Fatal(held, err, h.LastSequenceNumber)
		}
	}
	if err := h.ContinueAfter(100); err != nil || next() != 101 {
		t.Fatal(err, h.LastSequenceNumber)
	}
	// A restart knows the last number, also when its reading is gone.
	if _, err := h.DB.Exec("DELETE FROM readings"); err != nil {
		t.Fatal(err)
	}
	h.Close()
	again, err := store.OpenMeasurementHistory(path, 16*mib)
	if err != nil || again.LastSequenceNumber != 101 {
		t.Fatal(err, again.LastSequenceNumber)
	}
	defer again.Close()
	if err = again.Add(memoryReadings(now, 42)); err != nil || again.LastSequenceNumber != 102 {
		t.Fatal(err, again.LastSequenceNumber)
	}
}

// MMWatchdog runs without a measurement history that it cannot use: its checks run
// and alert as always, and the status shows a storage problem that names the
// file and says what to do.
func TestTheDaemonRunsWithoutAMeasurementHistoryThatItCannotUse(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(server.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600)
	p, c := daemonConfig(t)
	c.CheckGroups["certificate"] = checksandalerts.CheckGroup{Check: checksandalerts.Check{Inputs: &checksandalerts.TLS{Address: server.Listener.Addr().String(), ServerName: "example.com", TrustedCertificateAuthorityFile: ca, FailWhenCertificateExpiresWithin: "1h"}}}
	writeConfig(t, p, c)
	if err := os.MkdirAll(c.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(c.StateDir, store.MeasurementHistoryFile)
	if err := os.WriteFile(path, []byte(strings.Repeat("this is no database\n", 300)), 0600); err != nil {
		t.Fatal(err)
	}
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal("MMWatchdog does not start because of its measurement history:", err)
	}
	t.Cleanup(func() { d.Close() })
	now := time.Now().UTC()
	want := "measurement history write: cannot save readings in measurement history file " + path + ": "
	// The storage problem is there from the start, before a check has run.
	d.mu.Lock()
	_, _, atStart := d.status("", now, nil)
	d.mu.Unlock()
	if !strings.Contains(atStart, want) {
		t.Fatal(atStart)
	}
	tick(d, now)
	tick(d, now.Add(30*time.Second))
	d.pruneMeasurementHistory(now)
	d.trimMeasurementHistories()
	d.mu.Lock()
	checks, _, storageError := d.status("", now, nil)
	summary := incidents.Summarize(checks, storageError)
	report := d.buildReport(now)
	d.mu.Unlock()
	if !strings.Contains(storageError, want) || !strings.Contains(storageError, "move the measurement history file aside and restart MMWatchdog to create an empty measurement history") || summary.ExitCode != 3 ||
		checks["certificate/tls"].State != "ok" || len(checks["certificate/tls"].Last.Measurements) != 1 {
		t.Fatal(storageError, summary, checks["certificate/tls"].State)
	}
	// A report still carries an ID that a hub accepts.
	if !fleet.IDPattern.MatchString(report.MeasurementHistoryID) {
		t.Fatal(report.MeasurementHistoryID)
	}
	// MMWatchdog runs like this until someone moves the file aside, and every run of
	// a check that measures fails to store its readings. The log gets the
	// problem once, not once for each run.
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	for i := 2; i < 6; i++ {
		tick(d, now.Add(time.Duration(i)*30*time.Second))
	}
	if strings.Contains(logged.String(), "measurement history write") {
		t.Fatal("a storage problem that lasts is logged again at every attempt:\n" + logged.String())
	}
	// A problem that starts, changes, or starts again is logged each time.
	health := fleet.StorageHealth{}
	health.Report("state commit", errors.New("disk full"))
	health.Report("state commit", errors.New("disk full"))
	health.Report("state commit", errors.New("database is locked"))
	health.Report("state commit", nil)
	health.Report("state commit", errors.New("database is locked"))
	if lines := strings.Count(logged.String(), "state commit: "); lines != 3 || len(health.Problems()) != 1 {
		t.Fatal(lines, logged.String())
	}
	if status := measurementHistoryStatusOf(t, d); status.OldestReadingAt != nil || status.CutShortByMeasurementHistorySizeLimit {
		t.Fatalf("%+v", status)
	}
}
