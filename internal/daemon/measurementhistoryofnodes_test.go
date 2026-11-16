package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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

// readingsOnHub returns what the hub holds of one node's readings, in the
// order in which it saved them: the value of memory_used_percent of each run,
// or -1 for a run of another check.
func readingsOnHub(t *testing.T, hub *Daemon, node string) []float64 {
	t.Helper()
	rows, err := hub.hub.Readings.DB.Query("SELECT check_name, measurements FROM readings WHERE node=? ORDER BY id", node)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []float64{}
	for rows.Next() {
		var check string
		var encoded []byte
		var measurements map[string]store.MeasurementReading
		if err = rows.Scan(&check, &encoded); err == nil {
			err = json.Unmarshal(encoded, &measurements)
		}
		if err != nil {
			t.Fatal(err)
		}
		if check != "memory" {
			out = append(out, -1)
			continue
		}
		out = append(out, measurements["memory_used_percent"].Value)
	}
	return out
}

// addMemoryReadings stores one run of machine/memory for each value in the
// node's own measurement history, a second apart, ending now.
func addMemoryReadings(t *testing.T, d *Daemon, values ...float64) {
	t.Helper()
	now := time.Now().UTC()
	for i, value := range values {
		if err := d.history.Add(memoryReadings(now.Add(-time.Duration(len(values)-i)*time.Second), value)); err != nil {
			t.Fatal(err)
		}
	}
}

// A joined node sends the hub the readings that the hub has not received
// yet, with its reports and in order, and the hub keeps them under the
// node's name. The first reply tells the node where the hub is, so nothing
// arrives twice: not after a restart of the node, and not after a hub
// outage, when the node sends what the hub missed. The hub's own readings
// stay in its own measurement history.
func TestANodeSendsItsReadingsToTheHub(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	addMemoryReadings(t, node, 1, 2, 3)
	addMemoryReadings(t, hub, 91, 92)
	report := func(d *Daemon) {
		t.Helper()
		d.report(context.Background())
		if d.link != nil && d.link.LastError != "" {
			t.Fatal(d.link.LastError)
		}
	}
	// The report that joins carries no readings: the node does not know yet
	// where the hub is.
	report(node)
	if got := readingsOnHub(t, hub, "app-1"); len(got) != 0 || node.rep.ReadingsAfter != 0 {
		t.Fatal(got, node.rep.ReadingsAfter)
	}
	report(node)
	if got := readingsOnHub(t, hub, "app-1"); !reflect.DeepEqual(got, []float64{1, 2, 3}) || node.rep.ReadingsAfter != 3 {
		t.Fatal(got, node.rep.ReadingsAfter)
	}
	// The readings are in the hub's measurement history and nowhere else:
	// not in the report that the hub keeps for the node in fleet.db and in
	// memory, and not in the fleet view.
	var kept []byte
	if err := hub.hub.DB.QueryRow("SELECT data FROM nodes WHERE name='app-1'").Scan(&kept); err != nil {
		t.Fatal(err)
	}
	status, view := fleetCommand(t, hub, "GET", "/fleet?node=app-1")
	hub.hub.Mu.Lock()
	inMemory := len(hub.hub.Nodes["app-1"].NodeStatusReport.Readings)
	hub.hub.Mu.Unlock()
	if inMemory != 0 || strings.Contains(string(kept), "memory_used_percent") || status != 200 || strings.Contains(view, "memory_used_percent") || !strings.Contains(view, `"measurement_history_id"`) {
		t.Fatal(inMemory, string(kept), view)
	}
	// Nothing new, then two new runs.
	report(node)
	addMemoryReadings(t, node, 4, 5)
	report(node)
	if got := readingsOnHub(t, hub, "app-1"); !reflect.DeepEqual(got, []float64{1, 2, 3, 4, 5}) {
		t.Fatal(got)
	}
	// The hub's own report copies nothing: the hub reads its own readings
	// from its own measurement history.
	report(hub)
	if got := readingsOnHub(t, hub, hub.store.Node); len(got) != 0 || len(storedReadings(t, hub)) != 2 || !reflect.DeepEqual(hub.hub.Readings.NodesWithReadings(), []string{"app-1"}) {
		t.Fatal(got, hub.hub.Readings.NodesWithReadings())
	}
	// The node keeps its own readings.
	if got := storedReadings(t, node); len(got) != 5 {
		t.Fatal(len(got))
	}
	// While the hub is unreachable the node keeps storing readings; when
	// reports are accepted again it sends what the hub missed.
	down := code
	down.Address = "127.0.0.1:1"
	node.mu.Lock()
	node.setJoinCode(&down)
	node.mu.Unlock()
	addMemoryReadings(t, node, 6, 7)
	node.report(context.Background())
	if node.link.LastError == "" || len(readingsOnHub(t, hub, "app-1")) != 5 {
		t.Fatal("the report reached a hub that is down")
	}
	node.mu.Lock()
	node.setJoinCode(&code)
	node.mu.Unlock()
	addMemoryReadings(t, node, 8)
	report(node)
	if got := readingsOnHub(t, hub, "app-1"); !reflect.DeepEqual(got, []float64{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Fatal(got)
	}
	// A restarted node asks the hub where it is before it sends readings.
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewDaemon(node.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
	restarted.store.Node, restarted.link.Node = "app-1", "app-1"
	addMemoryReadings(t, restarted, 9)
	report(restarted)
	report(restarted)
	if got := readingsOnHub(t, hub, "app-1"); !reflect.DeepEqual(got, []float64{1, 2, 3, 4, 5, 6, 7, 8, 9}) {
		t.Fatal(got)
	}
	// The fleet view says how far back the hub's history reaches.
	history := hub.hub.Fleet(time.Now()).MeasurementHistoryOfNodes
	if history.OldestReadingAt == nil || history.NewestReadingAt == nil || history.NewestReadingAt.Before(*history.OldestReadingAt) || history.KeepMeasurementHistoryOfNodesFor != "7d" || history.MeasurementHistoryOfNodesSizeLimit != "20GiB" || history.CutShortByMeasurementHistoryOfNodesSizeLimit {
		t.Fatalf("%+v", history)
	}
}

// One report carries as many readings as the request size limit leaves room
// for, and the following reports carry the rest, in order.
func TestReadingsThatDoNotFitOneReportFollowInTheNext(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	now := time.Now().UTC()
	const runs = 300 // about 900 KiB of readings
	for i := 0; i < runs; i++ {
		if err := node.history.Add(largeReadings(now.Add(-time.Duration(runs-i)*time.Second), i)); err != nil {
			t.Fatal(i, err)
		}
	}
	oneRun, err := json.Marshal(largeReadings(now, 0))
	if err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var n int
		if err := hub.hub.Readings.DB.QueryRow("SELECT COUNT(*) FROM readings WHERE node='app-1'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	node.report(context.Background())
	reports, previous := 0, 0
	for count() < runs {
		node.report(context.Background())
		reports++
		arrived := count() - previous
		if node.link.LastError != "" || arrived <= 0 || reports > 10 {
			t.Fatal(node.link.LastError, count(), previous, reports)
		}
		// One report carries at most ReportReadingsBytes of readings when
		// nodes report every 30 seconds: a hub must not spend its time on the
		// backlog of one node.
		if arrived*len(oneRun) > fleet.ReportReadingsBytes {
			t.Fatalf("one report carried %d runs, about %d KiB of readings", arrived, arrived*len(oneRun)/1024)
		}
		previous = count()
	}
	if every := node.reportEvery(); every != 30*time.Second || reports < 4 {
		t.Fatal("900 KiB of readings arrived in fewer than four reports of at most 256 KiB:", reports, every)
	}
	// The hub holds every run once, in the node's order.
	rows, err := hub.hub.Readings.DB.Query("SELECT at FROM readings WHERE node='app-1' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for i := 0; rows.Next(); i++ {
		var at int64
		rows.Scan(&at)
		if want := now.Add(-time.Duration(runs-i) * time.Second); at != want.UnixNano() {
			t.Fatalf("reading %d is from %v, want %v", i, time.Unix(0, at), want)
		}
	}
	if hub.hub.Readings.ReceivedFrom("app-1", node.history.ID) != runs {
		t.Fatal(hub.hub.Readings.ReceivedFrom("app-1", node.history.ID))
	}
	// A node sends the same amount of readings in the same time however
	// often it reports: 256 KiB for each 30 seconds between two reports, and
	// never less than the largest run that a hub accepts takes. The readings
	// never take the room that the rest of a report needs: a report near the
	// request size limit carries fewer, and a full one none.
	const kib = 1024
	for _, tc := range []struct {
		encodedBytes int
		every        time.Duration
		want         int
	}{
		{2000, 30 * time.Second, 256 * kib},
		{2000, time.Minute, 512 * kib},
		{2000, 5 * time.Minute, 2560 * kib},
		{2000, 15 * time.Second, 128 * kib},
		{2000, time.Second, 128 * kib},
		{2000, 200 * time.Millisecond, 128 * kib},
		{2000, time.Hour, fleet.ReportLimit - fleet.ReportSpareBytes - 2000},
		{2000, 10 * 365 * 24 * time.Hour, fleet.ReportLimit - fleet.ReportSpareBytes - 2000},
		{fleet.ReportLimit - fleet.ReportSpareBytes - 256*kib, 30 * time.Second, 256 * kib},
		{fleet.ReportLimit - fleet.ReportSpareBytes - 1000, 30 * time.Second, 1000},
		{fleet.ReportLimit - fleet.ReportSpareBytes - 1000, time.Second, 1000},
		{fleet.ReportLimit - fleet.ReportSpareBytes, 30 * time.Second, 0},
	} {
		if got := fleet.ReadingsRoom(tc.encodedBytes, tc.every); got != tc.want {
			t.Errorf("a report of %d bytes every %s has room for %d bytes of readings, want %d", tc.encodedBytes, tc.every, got, tc.want)
		}
	}
	if fleet.ReadingsRoom(fleet.ReportLimit, 30*time.Second) >= 0 {
		t.Error("a report at the request size limit has room for readings")
	}
	if got, err := node.history.After(0, fleet.ReadingsRoom(fleet.ReportLimit, 30*time.Second)); err != nil || len(got) != 0 {
		t.Fatal(len(got), err)
	}
	// The largest run that a hub accepts fits the least room, so no run can
	// permanently stop a node's readings.
	largest := store.RunReadings{SequenceNumber: fleet.MaxSequenceNumber, At: now, CheckGroup: strings.Repeat("g", 64), Check: strings.Repeat("c", 64), Measurements: map[string]store.MeasurementReading{}}
	for i := 0; i < 128; i++ {
		above, below := math.MaxFloat64, -math.MaxFloat64
		largest.Measurements[fmt.Sprintf("%03d%s", i, strings.Repeat("m", 61))] = store.MeasurementReading{Value: -math.MaxFloat64, Unit: strings.Repeat("&", 64), FailWhenMeasurementAbove: &above, FailWhenMeasurementBelow: &below}
	}
	encoded, err := json.Marshal(largest)
	if err != nil || largest.Validate() != nil || len(encoded)+1 > fleet.ReportReadingsFloor {
		t.Fatal(err, largest.Validate(), len(encoded))
	}
}

// A node whose measurement history is a new one starts over on the hub,
// which keeps what it holds. Forgetting a node removes its readings, also
// when the hub stopped before it could remove them.
func TestANewMeasurementHistoryAndAForgottenNode(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	addMemoryReadings(t, node, 1, 2)
	node.report(context.Background())
	node.report(context.Background())
	// The node's measurement history is lost, and the node starts with an
	// empty one.
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(filepath.Join(node.config.StateDir, store.MeasurementHistoryFile+suffix))
	}
	fresh, err := NewDaemon(node.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fresh.Close() })
	fresh.store.Node, fresh.link.Node = "app-1", "app-1"
	if fresh.history.ID == node.history.ID {
		t.Fatal("the new measurement history has the ID of the lost one")
	}
	addMemoryReadings(t, fresh, 11, 12, 13)
	fresh.report(context.Background())
	fresh.report(context.Background())
	if got := readingsOnHub(t, hub, "app-1"); !reflect.DeepEqual(got, []float64{1, 2, 11, 12, 13}) || fresh.rep.ReadingsAfter != 3 {
		t.Fatal(got, fresh.rep.ReadingsAfter)
	}
	// Forgetting the node removes its readings on the hub.
	if status, body := fleetCommand(t, hub, "POST", "/fleet/forget/app-1"); status != 200 {
		t.Fatal(status, body)
	}
	if got := readingsOnHub(t, hub, "app-1"); len(got) != 0 || len(hub.hub.Readings.NodesWithReadings()) != 0 {
		t.Fatal(got)
	}
	// Readings of a node that fleet.db no longer lists go when the hub starts.
	now := time.Now().UTC()
	ghost := memoryReadings(now, 7)
	ghost.SequenceNumber = 1
	if _, err = hub.hub.Readings.AddFromNode("ghost-1", strings.Repeat("a", 32), 0, []store.RunReadings{ghost}, now); err != nil || len(readingsOnHub(t, hub, "ghost-1")) != 1 {
		t.Fatal(err)
	}
	if err = hub.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewDaemon(hub.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
	if got := readingsOnHub(t, restarted, "ghost-1"); len(got) != 0 || len(restarted.hub.Readings.NodesWithReadings()) != 0 {
		t.Fatal(got, restarted.hub.Readings.NodesWithReadings())
	}
}

// Readings that the hub cannot save are a storage problem of the hub and no
// reason to refuse the report. The node sends them again until the hub has
// them.
func TestReadingsThatTheHubCannotSaveArriveLater(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	addMemoryReadings(t, node, 1, 2)
	node.report(context.Background())
	if _, err := hub.hub.Readings.DB.Exec("CREATE TRIGGER reject_reading BEFORE INSERT ON readings BEGIN SELECT RAISE(FAIL, 'fixture storage failure'); END"); err != nil {
		t.Fatal(err)
	}
	node.report(context.Background())
	fleet := hub.hub.Fleet(time.Now())
	if node.link.LastError != "" || fleetServer(t, hub, "app-1").State != "reporting" || len(readingsOnHub(t, hub, "app-1")) != 0 || node.rep.ReadingsAfter != 0 ||
		!strings.Contains(fleet.StorageError, "measurement history of nodes write: ") || fleet.ExitCode != 3 {
		t.Fatalf("%q %q %d", node.link.LastError, fleet.StorageError, fleet.ExitCode)
	}
	if _, err := hub.hub.Readings.DB.Exec("DROP TRIGGER reject_reading"); err != nil {
		t.Fatal(err)
	}
	node.report(context.Background())
	if got := readingsOnHub(t, hub, "app-1"); !reflect.DeepEqual(got, []float64{1, 2}) || hub.hub.Fleet(time.Now()).StorageError != "" {
		t.Fatal(got, hub.hub.Fleet(time.Now()).StorageError)
	}
}

// The hub removes a reading when it is older than
// keep_measurement_history_of_nodes_for, and its size limit removes the
// oldest readings first, whichever node they came from. The fleet view then
// says how far back the history reaches; that line is information and does
// not change the exit code. A reload applies both settings.
func TestTheHubAppliesTheSettingsOfTheMeasurementHistoryOfNodes(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	c.FleetHubRole = &config.FleetHubRole{ListenOnAddress: "0.0.0.0:8445", AddressForNodes: "hub.test:8445"}
	writeConfig(t, p, c)
	hub, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.Close() })
	now := time.Now().UTC()
	history := strings.Repeat("a", 32)
	old := memoryReadings(now.Add(-8*24*time.Hour), 1)
	old.SequenceNumber = 1
	recent := memoryReadings(now.Add(-time.Hour), 2)
	recent.SequenceNumber = 2
	// The hub takes the time of a reading as the node measured it.
	if _, err := hub.hub.Readings.AddFromNode("app-1", history, 0, []store.RunReadings{old, recent}, now); err != nil {
		t.Fatal(err)
	}
	hub.hub.Prune(now)
	if got := readingsOnHub(t, hub, "app-1"); !reflect.DeepEqual(got, []float64{2}) || len(hub.hub.Problems()) != 0 {
		t.Fatal(got, hub.hub.Problems())
	}
	// Two nodes send about 3 MiB together; app-2's readings are the older.
	for i, name := range []string{"app-2", "app-3"} {
		many := []store.RunReadings{}
		for n := 0; n < 500; n++ {
			r := largeReadings(now.Add(-time.Duration(2-i)*time.Hour).Add(time.Duration(n)*time.Second), n)
			r.SequenceNumber = int64(n + 1)
			many = append(many, r)
		}
		if _, err := hub.hub.Readings.AddFromNode(name, history, 0, many, now); err != nil {
			t.Fatal(err)
		}
	}
	fleet := hub.hub.Fleet(now)
	if fleet.MeasurementHistoryOfNodes.CutShortByMeasurementHistoryOfNodesSizeLimit || fleet.MeasurementHistoryOfNodes.CutShortSentence(now) != "" || fleet.ExitCode != 0 {
		t.Fatalf("%+v", fleet)
	}
	// A reload with a lower size limit does no removal itself: the daemon
	// shrinks the file in steps, several times a second.
	edited := *c
	role := *c.FleetHubRole
	role.MeasurementHistoryOfNodesSizeLimit = "1MiB"
	edited.FleetHubRole = &role
	writeConfig(t, p, &edited)
	if _, err := hub.reload(); err != nil {
		t.Fatal(err)
	}
	shrinking := func() bool {
		hub.hub.Readings.Mu.Lock()
		defer hub.hub.Readings.Mu.Unlock()
		return hub.hub.Readings.Shrinking
	}
	if !shrinking() {
		t.Fatal("the hub's file is within a limit of 1 MiB although it holds 3 MiB")
	}
	for steps := 0; shrinking(); steps++ {
		hub.trimMeasurementHistories()
		if steps > 10000 {
			t.Fatal("the hub's file does not reach its size limit")
		}
	}
	countOf := func(name string) int {
		var n int
		if err := hub.hub.Readings.DB.QueryRow("SELECT COUNT(*) FROM readings WHERE node=?", name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	path := filepath.Join(c.StateDir, store.MeasurementHistoryOfNodesFile)
	if fileBytes(t, path) > 1*mib || countOf("app-1") != 0 || countOf("app-2") != 0 || countOf("app-3") < 50 || countOf("app-3") >= 500 {
		t.Fatal(fileBytes(t, path), countOf("app-1"), countOf("app-2"), countOf("app-3"))
	}
	fleet = hub.hub.Fleet(now)
	status := fleet.MeasurementHistoryOfNodes
	if !status.CutShortByMeasurementHistoryOfNodesSizeLimit || status.MeasurementHistoryOfNodesSizeLimit != "1MiB" || fleet.ExitCode != 0 || fleet.StorageError != "" {
		t.Fatalf("%+v", fleet)
	}
	if sentence := status.CutShortSentence(now); !strings.HasPrefix(sentence, "measurement history of nodes reaches back ") || !strings.HasSuffix(sentence, "; the size limit of 1 MiB prevented keeping the requested 7 days") {
		t.Fatal(sentence)
	}
	// A reload with a shorter time asks for the retention step at once,
	// which removes the hub's readings that are older.
	asksForRetention(t, hub, func() {
		role.KeepMeasurementHistoryOfNodesFor = "30m"
		writeConfig(t, p, &edited)
		if _, err := hub.reload(); err != nil {
			t.Fatal(err)
		}
	})
	hub.retention(now)
	if countOf("app-3") != 0 || hub.hub.Fleet(now).MeasurementHistoryOfNodes.KeepMeasurementHistoryOfNodesFor != "30m" {
		t.Fatal(countOf("app-3"))
	}
}

// A hub states both settings of its measurement history of nodes, and the
// readings of a report are checked like every input from another machine.
func TestTheHubsMeasurementHistorySettingsAndReportedReadingsAreValidated(t *testing.T) {
	t.Parallel()
	checks := "check_groups: {a: {shell: 'true'}}\n"
	for text, want := range map[string]string{
		"fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'util:8445', measurement_history_of_nodes_size_limit: 20GiB}\n":                                            "fleet_hub_role.keep_measurement_history_of_nodes_for: is required: how long the hub keeps the readings that the nodes send, such as 7d",
		"fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'util:8445', keep_measurement_history_of_nodes_for: 7d}\n":                                                 "fleet_hub_role.measurement_history_of_nodes_size_limit: is required: how much of the hub's disk the readings of the nodes may take, such as 20GiB",
		"fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'util:8445', keep_measurement_history_of_nodes_for: 0s, measurement_history_of_nodes_size_limit: 20GiB}\n": "fleet_hub_role.keep_measurement_history_of_nodes_for: must be positive",
		"fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'util:8445', keep_measurement_history_of_nodes_for: 7d, measurement_history_of_nodes_size_limit: 20GB}\n":  "a size: a whole number with MiB or GiB, such as 256MiB",
		"keep_measurement_history_of_nodes_for: 7d\n": `unknown field "keep_measurement_history_of_nodes_for"`,
	} {
		if _, err := config.Load(testhelpers.ConfigFileAsWritten(t, "keep_undelivered_alerts_for: 24h\nundelivered_alerts_content_size_limit: 8MiB\nalert_severity_when_this_node_goes_silent: sos\nalert_severity_when_this_node_has_rebooted: info\nkeep_measurement_history_for: 7d\nmeasurement_history_size_limit: 256MiB\n"+text+
			"check_groups: {a: {check_every: 30s, time_limit_per_run: 5s, alert_after_failing_for: 1m, recovered_after_healthy_for: 30s, shell: 'true', alert_severity: warn}}\n")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", text, err, want)
		}
	}
	c, err := config.Load(testhelpers.ConfigFile(t, "fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'util:8445', keep_measurement_history_of_nodes_for: 30d, measurement_history_of_nodes_size_limit: 50GiB}\n"+checks))
	if err != nil || c.FleetHubRole.HubLimits().KeepMeasurementHistoryOfNodesFor != "30d" || c.FleetHubRole.HubLimits().MeasurementHistoryOfNodesSizeLimit.Bytes() != 50*1024*mib {
		t.Fatal(c, err)
	}
	now := time.Now().UTC()
	report := func(after int64, readings ...store.RunReadings) *fleet.NodeStatusReport {
		return &fleet.NodeStatusReport{AlertSeverityWhenThisNodeGoesSilent: "warn", StartID: incidents.ID(), Seq: 1, History: incidents.ID(), After: -1, MeasurementHistoryID: incidents.ID(), ReadingsAfter: after, Readings: readings}
	}
	run := func(sequenceNumber int64, change func(*store.RunReadings)) store.RunReadings {
		r := memoryReadings(now, 41)
		r.SequenceNumber = sequenceNumber
		if change != nil {
			change(&r)
		}
		return r
	}
	tooMany := map[string]store.MeasurementReading{}
	for i := 0; i < 129; i++ {
		tooMany["m"+strings.Repeat("x", 3)+string(rune('a'+i%26))+string(rune('a'+i/26))] = store.MeasurementReading{Value: 1, Unit: "count"}
	}
	for name, tc := range map[string]struct {
		report *fleet.NodeStatusReport
		want   string
	}{
		"valid":                         {report(4, run(5, nil), run(9, nil)), ""},
		"no readings":                   {report(-1), ""},
		"no measurement history ID":     {&fleet.NodeStatusReport{AlertSeverityWhenThisNodeGoesSilent: "warn", StartID: incidents.ID(), Seq: 1, History: incidents.ID(), After: -1, ReadingsAfter: -1}, "node status report requires a valid measurement_history_id"},
		"a sequence number below -1":    {report(-2), "node status report requires a valid measurement_history_id"},
		"readings before a reply":       {report(-1, run(1, nil)), "node status report readings must have increasing sequence numbers after readings_after_sequence_number, which must be 0 or more"},
		"readings out of order":         {report(4, run(6, nil), run(5, nil)), "node status report readings must have increasing sequence numbers after readings_after_sequence_number, which must be 0 or more"},
		"readings at the hub's number":  {report(4, run(4, nil)), "node status report readings must have increasing sequence numbers after readings_after_sequence_number, which must be 0 or more"},
		"a check group that is no name": {report(0, run(1, func(r *store.RunReadings) { r.CheckGroup = "not a name" })), "report carries invalid readings: readings must name a valid check group and check for the recorded run"},
		"no time":                       {report(0, run(1, func(r *store.RunReadings) { r.At = time.Time{} })), "report carries invalid readings: readings must include a run time within the supported range"},
		"no measurements":               {report(0, run(1, func(r *store.RunReadings) { r.Measurements = nil })), "a recorded run must contain 1 to 128 measurements"},
		"too many measurements":         {report(0, run(1, func(r *store.RunReadings) { r.Measurements = tooMany })), "a recorded run must contain 1 to 128 measurements"},
		"a measurement without a unit": {report(0, run(1, func(r *store.RunReadings) {
			r.Measurements = map[string]store.MeasurementReading{"memory_used_percent": {Value: 41, Unit: ""}}
		})), "measurement needs a valid name, a number, and a unit"},
		"a measurement that is no name": {report(0, run(1, func(r *store.RunReadings) {
			r.Measurements = map[string]store.MeasurementReading{"memory used": {Value: 41, Unit: "percent"}}
		})), "measurement needs a valid name, a number, and a unit"},
		"a unit with a control character": {report(0, run(1, func(r *store.RunReadings) {
			r.Measurements = map[string]store.MeasurementReading{"memory_used_percent": {Value: 41, Unit: "per\x1bcent"}}
		})), "measurement needs a valid name, a number, and a unit"},
		// The history stores a time as nanoseconds since 1970: a time that it
		// cannot store would come back as another time.
		"a time before 1970":   {report(0, run(1, func(r *store.RunReadings) { r.At = time.Date(1500, 1, 1, 0, 0, 0, 0, time.UTC) })), "readings must include a run time within the supported range, from the year 1970 to the year 2262"},
		"a time after 2262":    {report(0, run(1, func(r *store.RunReadings) { r.At = time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC) })), "readings must include a run time within the supported range, from the year 1970 to the year 2262"},
		"a time in the future": {report(0, run(1, func(r *store.RunReadings) { r.At = now.Add(50 * 365 * 24 * time.Hour) })), ""},
		// A node continues its numbers after the hub's, so the hub holds no
		// number that a node could not continue after.
		"the highest sequence number":      {report(fleet.MaxSequenceNumber-1, run(fleet.MaxSequenceNumber, nil)), ""},
		"a sequence number above it":       {report(fleet.MaxSequenceNumber, run(fleet.MaxSequenceNumber+1, nil)), "node status report readings must have increasing sequence numbers after readings_after_sequence_number, which must be 0 or more"},
		"readings after a number above it": {report(fleet.MaxSequenceNumber + 1), "node status report requires a valid measurement_history_id and readings_after_sequence_number"},
	} {
		if err := tc.report.Validate(); tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}
	if len(tooMany) != 129 {
		t.Fatal(len(tooMany))
	}
}

// A node whose measurement history was restored from an older copy has given
// out fewer sequence numbers than the hub holds. Its next readings continue
// after the hub's number, so the hub takes them: without that, the hub would
// ignore every new reading until the node's numbers had passed its own.
func TestANodeWithARestoredMeasurementHistoryKeepsSendingReadings(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	addMemoryReadings(t, node, 1, 2, 3, 4, 5, 6, 7, 8)
	node.report(context.Background())
	node.report(context.Background())
	if got := readingsOnHub(t, hub, "app-1"); len(got) != 8 {
		t.Fatal(got)
	}
	// The file goes back to the copy that held three readings.
	if _, err := node.history.DB.Exec("DELETE FROM readings WHERE id>3; UPDATE sqlite_sequence SET seq=3 WHERE name='readings'"); err != nil {
		t.Fatal(err)
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := NewDaemon(node.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restored.Close() })
	restored.store.Node, restored.link.Node = "app-1", "app-1"
	if restored.history.ID != node.history.ID || restored.history.LastSequenceNumber != 3 {
		t.Fatal(restored.history.ID, restored.history.LastSequenceNumber)
	}
	// The first reply says that the hub holds readings up to 8.
	restored.report(context.Background())
	if restored.link.LastError != "" || restored.rep.ReadingsAfter != 8 || restored.history.LastSequenceNumber != 8 {
		t.Fatal(restored.link.LastError, restored.rep.ReadingsAfter, restored.history.LastSequenceNumber)
	}
	addMemoryReadings(t, restored, 9, 10)
	restored.report(context.Background())
	if got := readingsOnHub(t, hub, "app-1"); !reflect.DeepEqual(got, []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}) {
		t.Fatal(got)
	}
}

// A hub runs without a measurement history of nodes that it cannot use: it
// takes joins, reports, and alerts as always, and the fleet view shows a
// storage problem that names the file and says what to do.
func TestTheHubRunsWithoutAMeasurementHistoryOfNodesThatItCannotUse(t *testing.T) {
	t.Parallel()
	p, c := daemonConfig(t)
	c.FleetHubRole = &config.FleetHubRole{ListenOnAddress: "0.0.0.0:8445", AddressForNodes: "hub.test:8445"}
	writeConfig(t, p, c)
	if err := os.MkdirAll(c.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(c.StateDir, store.MeasurementHistoryOfNodesFile)
	if err := os.WriteFile(path, []byte(strings.Repeat("this is no database\n", 300)), 0600); err != nil {
		t.Fatal(err)
	}
	hub, err := NewDaemon(p)
	if err != nil {
		t.Fatal("the hub does not start because of its measurement history of nodes:", err)
	}
	t.Cleanup(func() { hub.Close() })
	h := hub.hub
	now := time.Now().UTC()
	want := "measurement history of nodes write: cannot save readings in measurement history file " + path + ": "
	// The storage problem is there from the start, before a node has reported.
	if atStart := h.Fleet(now); !strings.Contains(atStart.StorageError, want) || atStart.ExitCode != 3 {
		t.Fatalf("%+v", atStart)
	}
	if _, status, err := h.Join("app-1", h.Code.Secret, nodeSettings("warn"), "10.0.0.5", now); status != 200 || err != nil {
		t.Fatal(status, err)
	}
	reading := memoryReadings(now, 41)
	reading.SequenceNumber = 1
	report := &fleet.NodeStatusReport{AlertSeverityWhenThisNodeGoesSilent: "warn", StartID: incidents.ID(), Seq: 1, History: incidents.ID(), After: -1, MeasurementHistoryID: incidents.ID(), ReadingsAfter: 0, Readings: []store.RunReadings{reading}, Summary: incidents.StatusSummary{Attention: []incidents.Attention{}}}
	// The report is accepted. The reply's number is the one that a node has
	// before its first reply, after which a node sends no readings: no node
	// sends readings that the hub cannot keep.
	if _, status, err := h.Accept("app-1", report, now); status != 200 || err != nil {
		t.Fatal(status, err)
	}
	held := h.SaveReadings("app-1", report, now)
	if held != -1 {
		t.Fatal(held)
	}
	next := *report
	next.Seq, next.ReadingsAfter, next.Readings = 2, held, nil
	if _, status, err := h.Accept("app-1", &next, now.Add(time.Second)); status != 200 || err != nil || h.SaveReadings("app-1", &next, now.Add(time.Second)) != -1 {
		t.Fatal(status, err)
	}
	hub.hub.Prune(now)
	hub.trimMeasurementHistories()
	if _, status, err := h.Forget("app-1"); status != 200 || err != nil {
		t.Fatal(status, err)
	}
	fleet := h.Fleet(now)
	if !strings.Contains(fleet.StorageError, want) || !strings.Contains(fleet.StorageError, "move the measurement history file aside and restart MMWatchdog to create an empty measurement history") || fleet.ExitCode != 3 || fleet.MeasurementHistoryOfNodes.OldestReadingAt != nil {
		t.Fatalf("%+v", fleet)
	}
}

// The hub saves a report and counts its node as reporting before it saves
// the node's readings, and it saves them without its own lock. So a
// measurement history that is busy holds up neither that node's report nor
// the report of any other node.
func TestReadingsNeverHoldUpAReport(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	first, second := joinedNode(t, code, "app-1"), joinedNode(t, code, "app-2")
	first.report(context.Background())
	second.report(context.Background())
	if first.rep.ReadingsAfter != 0 || second.rep.ReadingsAfter != 0 {
		t.Fatal(first.link.LastError, second.link.LastError)
	}
	addMemoryReadings(t, first, 1, 2, 3)
	receipt := func() time.Time {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return hub.snapshot.Checks[fleet.FleetKey("app-1")].Receipt
	}
	before := receipt()
	// The hub's measurement history is busy: nothing can be saved in it.
	hub.hub.Readings.Mu.Lock()
	busy := true
	release := func() {
		if busy {
			busy = false
			hub.hub.Readings.Mu.Unlock()
		}
	}
	defer release()
	sent := make(chan struct{})
	go func() { first.report(context.Background()); close(sent) }()
	// The report of app-1 is saved, and app-1 counts as reporting, while its
	// readings wait.
	for deadline := time.Now().Add(testhelpers.LongestWaitForAnEvent); !receipt().After(before); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the hub does not count a node as reporting before its readings are saved")
		}
	}
	select {
	case <-sent:
		t.Fatal("the report was answered although its readings were not saved")
	default:
	}
	// A fleet view waits for the history too, to say how far back it
	// reaches, and it waits without the hub's lock.
	viewed := make(chan struct{})
	go func() { hub.hub.Fleet(time.Now().UTC()); close(viewed) }()
	time.Sleep(50 * time.Millisecond)
	// The hub's own lock is free: it takes another node's report.
	second.mu.Lock()
	report := second.buildReport(time.Now().UTC())
	second.mu.Unlock()
	accepted := make(chan int, 1)
	go func() {
		_, status, _ := hub.hub.Accept("app-2", &report, time.Now().UTC())
		accepted <- status
	}()
	select {
	case status := <-accepted:
		if status != 200 {
			t.Fatal(status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hub holds its own lock while it waits for its measurement history")
	}
	if got := readingsOnHub(t, hub, "app-1"); len(got) != 0 {
		t.Fatal(got)
	}
	release()
	<-sent
	<-viewed
	if got := readingsOnHub(t, hub, "app-1"); first.link.LastError != "" || !reflect.DeepEqual(got, []float64{1, 2, 3}) || first.rep.ReadingsAfter != 3 {
		t.Fatal(first.link.LastError, got, first.rep.ReadingsAfter)
	}
	if problem := hub.hub.Fleet(time.Now().UTC()).StorageError; problem != "" {
		t.Fatal(problem)
	}
}

// The hub's lock is free between the moment it accepts a report and the
// moment it saves the report's readings. A node that was forgotten in that
// time leaves no readings behind, and a node that joined under its name in
// that time gets none of them and keeps its own.
func TestAReportOfAForgottenNodeSavesNoReadings(t *testing.T) {
	t.Parallel()
	// staleReport is a report of a node with three readings that the hub
	// has accepted and whose readings it has not saved yet.
	staleReport := func(t *testing.T, hub *Daemon, code fleet.JoinCode) fleet.NodeStatusReport {
		t.Helper()
		old := joinedNode(t, code, "app-1")
		old.report(context.Background())
		addMemoryReadings(t, old, 1, 2, 3)
		old.mu.Lock()
		report := old.buildReport(time.Now().UTC())
		old.mu.Unlock()
		old.addReadings(&report)
		if report.ReadingsAfter != 0 || len(report.Readings) != 3 {
			t.Fatal(report.ReadingsAfter, len(report.Readings))
		}
		if _, status, err := hub.hub.Accept("app-1", &report, time.Now().UTC()); status != 200 || err != nil {
			t.Fatal(status, err)
		}
		return report
	}
	forget := func(t *testing.T, hub *Daemon) {
		t.Helper()
		if _, status, err := hub.hub.Forget("app-1"); status != 200 || err != nil {
			t.Fatal(status, err)
		}
	}
	nothingSaved := func(t *testing.T, hub *Daemon, held int64) {
		t.Helper()
		if got := readingsOnHub(t, hub, "app-1"); held != -1 || len(got) != 0 || slices.Contains(hub.hub.Readings.NodesWithReadings(), "app-1") {
			t.Fatal(held, got, hub.hub.Readings.NodesWithReadings())
		}
	}

	t.Run("without a forget the readings are saved", func(t *testing.T) {
		hub, code := startHub(t)
		report := staleReport(t, hub, code)
		if held := hub.hub.SaveReadings("app-1", &report, time.Now().UTC()); held != 3 || len(readingsOnHub(t, hub, "app-1")) != 3 {
			t.Fatal(held, readingsOnHub(t, hub, "app-1"))
		}
	})
	t.Run("a forgotten node", func(t *testing.T) {
		hub, code := startHub(t)
		report := staleReport(t, hub, code)
		forget(t, hub)
		nothingSaved(t, hub, hub.hub.SaveReadings("app-1", &report, time.Now().UTC()))
	})
	t.Run("another node has joined under the name", func(t *testing.T) {
		hub, code := startHub(t)
		report := staleReport(t, hub, code)
		forget(t, hub)
		if _, status, err := hub.hub.Join("app-1", hub.hub.Code.Secret, nodeSettings("warn"), "10.0.0.5", time.Now().UTC()); status != 200 || err != nil {
			t.Fatal(status, err)
		}
		nothingSaved(t, hub, hub.hub.SaveReadings("app-1", &report, time.Now().UTC()))
	})
	t.Run("another node has joined under the name and sent readings", func(t *testing.T) {
		hub, code := startHub(t)
		report := staleReport(t, hub, code)
		forget(t, hub)
		replacement := joinedNode(t, code, "app-1")
		replacement.report(context.Background())
		addMemoryReadings(t, replacement, 11, 12)
		replacement.report(context.Background())
		if got := readingsOnHub(t, hub, "app-1"); replacement.link.LastError != "" || !reflect.DeepEqual(got, []float64{11, 12}) {
			t.Fatal(replacement.link.LastError, got)
		}
		if held := hub.hub.SaveReadings("app-1", &report, time.Now().UTC()); held != -1 {
			t.Fatal(held)
		}
		// The hub still knows how far it is with the new node, so the new
		// node sends nothing again.
		addMemoryReadings(t, replacement, 13)
		replacement.report(context.Background())
		if got := readingsOnHub(t, hub, "app-1"); !reflect.DeepEqual(got, []float64{11, 12, 13}) || hub.hub.Readings.ReceivedFrom("app-1", replacement.history.ID) != 3 {
			t.Fatal(got, hub.hub.Readings.ReceivedFrom("app-1", replacement.history.ID))
		}
	})
}

// A node takes the two sequence numbers of a reply only when a hub can hold
// them. It continues its own numbers after them, so a number that no node
// reaches would leave it without a number for its next event.
func TestANodeRefusesAReplyWithASequenceNumberThatNoHubHolds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		reply fleet.ReportReply
		valid bool
	}{
		{fleet.ReportReply{}, true},
		{fleet.ReportReply{EventsCursor: 12, ReadingsCursor: -1}, true},
		{fleet.ReportReply{EventsCursor: fleet.MaxSequenceNumber, ReadingsCursor: fleet.MaxSequenceNumber}, true},
		{fleet.ReportReply{EventsCursor: -1}, false},
		{fleet.ReportReply{EventsCursor: fleet.MaxSequenceNumber + 1}, false},
		{fleet.ReportReply{EventsCursor: math.MaxInt64}, false},
		{fleet.ReportReply{ReadingsCursor: -2}, false},
		{fleet.ReportReply{ReadingsCursor: fleet.MaxSequenceNumber + 1}, false},
	} {
		if tc.reply.Valid() != tc.valid {
			t.Errorf("%+v", tc.reply)
		}
	}
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	node.report(context.Background())
	addMemoryReadings(t, node, 1, 2)
	setMaintenance(t, node, "app", "10m")
	node.report(context.Background())
	events, readings := node.rep.After, node.rep.ReadingsAfter
	if node.link.LastError != "" || events < 1 || readings != 2 {
		t.Fatal(node.link.LastError, events, readings)
	}
	// The hub answers with a number of events that no node reaches.
	hub.hub.Mu.Lock()
	hub.hub.Nodes["app-1"].Cursor = math.MaxInt64
	hub.hub.Mu.Unlock()
	node.report(context.Background())
	if !strings.Contains(node.link.LastError, "the hub sent an invalid report reply") || node.rep.After != events || node.rep.ReadingsAfter != readings {
		t.Fatal(node.link.LastError, node.rep.After, node.rep.ReadingsAfter)
	}
	hub.hub.Mu.Lock()
	hub.hub.Nodes["app-1"].Cursor = events
	hub.hub.Mu.Unlock()
	// And with a number of readings that no node reaches.
	hub.hub.Readings.Mu.Lock()
	hub.hub.Readings.Received["app-1"] = store.ReadingsReceived{MeasurementHistoryID: node.history.ID, SequenceNumber: math.MaxInt64}
	hub.hub.Readings.Mu.Unlock()
	node.report(context.Background())
	if !strings.Contains(node.link.LastError, "the hub sent an invalid report reply") || node.rep.After != events || node.rep.ReadingsAfter != readings {
		t.Fatal(node.link.LastError, node.rep.After, node.rep.ReadingsAfter)
	}
	// The node's own numbers went on from where they were.
	setMaintenance(t, node, "app", "off")
	addMemoryReadings(t, node, 3)
	stored, err := node.store.EventsAfter(events, 10, 0)
	if err != nil || len(stored) != 1 || stored[0].Seq != events+1 || node.history.LastSequenceNumber != 3 {
		t.Fatal(stored, err, node.history.LastSequenceNumber)
	}
	if problem := node.storage.Problems(); len(problem) != 0 {
		t.Fatal(problem)
	}
}

// A scheduled observation keeps the limits of that definition, without
// changing the module result. The reporter carries those limits unchanged.
func TestScheduledReadingsKeepLimitsAndReportsCarryThem(t *testing.T) {
	t.Parallel()
	hub, code := startHub(t)
	node := joinedNode(t, code, "app-1")
	now := time.Now().UTC()
	observation := checksandalerts.Observation{At: now, Status: "failed", Measurements: map[string]checksandalerts.Measurement{"memory_used_percent": {Value: 95, Unit: "percent"}}}
	runtime := &checkRuntime{def: config.Definition{CheckGroup: "machine", Name: "memory", Check: checksandalerts.Check{FailWhenMeasurementAbove: map[string]float64{"memory_used_percent": 90}, FailWhenMeasurementBelow: map[string]float64{"memory_used_percent": 0}}}}
	node.mu.Lock()
	node.keepReadings(runtime, observation)
	runtime.def.Check.FailWhenMeasurementAbove["memory_used_percent"] = 99
	observation.At = now.Add(time.Second)
	node.keepReadings(runtime, observation)
	node.mu.Unlock()
	node.report(context.Background())
	node.report(context.Background())
	path := filepath.Join(hub.config.StateDir, store.MeasurementHistoryOfNodesFile)
	runs, _, err := store.ReadMeasurementHistory(path, "app-1", "machine", "memory", now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil || len(runs) != 2 {
		t.Fatal(runs, err)
	}
	for index, want := range []float64{90, 99} {
		reading := runs[index].Measurements["memory_used_percent"]
		if reading.FailWhenMeasurementAbove == nil || *reading.FailWhenMeasurementAbove != want || reading.FailWhenMeasurementBelow == nil || *reading.FailWhenMeasurementBelow != 0 {
			t.Fatalf("%+v", reading)
		}
	}
}
