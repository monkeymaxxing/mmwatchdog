package store_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/store"
)

// The many-server case of the design of grouping by exact finding: a hub
// takes the alerts and reports of 5,000 simulated nodes with 200 findings
// and 3 destinations through its actual intake, alert grouping step,
// report reconciliation, and update step, over a simulated 6 h. The
// grouping step runs once a simulated minute, so alerts wait in the intake
// between arrivals, within a stated intake byte limit. The expected counts
// follow from the rules: one message for each exact finding of each key
// that a destination takes, one sos update for each node added at sos,
// one update per key on the shared tick, and nothing for the rest.
// Nothing here depends on the machine's speed.
func TestManyServersGroupedByExactFinding(t *testing.T) {
	t.Parallel()
	const (
		nodes          = 5000
		findings       = 200
		burstNodes     = 2000
		manyNodes      = 400
		intakeLimit    = 256 * 1024
		simulatedHours = 6
	)
	start := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	limits := config.DefaultStorageLimits
	limits.UndeliveredAlertsOfNodesBytes = intakeLimit
	s, e := store.OpenStore(filepath.Join(t.TempDir(), "state"), limits)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	s.FleetIdentity = groupingFleet
	// The size of state.db's file, and of its pages in use: SQLite keeps
	// the pages that deleted rows freed for reuse.
	sizeOfStateDatabase := func() (file, used int64) {
		t.Helper()
		var pages, free, pageSize int64
		if _, e := s.DB.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); e != nil {
			t.Fatal(e)
		}
		for statement, value := range map[string]*int64{"PRAGMA page_count": &pages, "PRAGMA freelist_count": &free, "PRAGMA page_size": &pageSize} {
			if e := s.DB.QueryRow(statement).Scan(value); e != nil {
				t.Fatal(e)
			}
		}
		return pages * pageSize, (pages - free) * pageSize
	}
	emptyFile, emptyUsed := sizeOfStateDatabase()
	destinations := map[string]checksandalerts.Destination{
		"chat":    chatTaking("https://chat.example.com", "warn", "sos"),
		"pager":   chatTaking("https://pager.example.com", "sos"),
		"hotline": hotline(),
	}
	h := &groupingHub{t: t, s: s, destinations: destinations, grouping: fleet.AlertGroupingState{Sent: map[string]time.Time{}, Quiet: map[string]time.Time{}, Window: fleet.AlertGroupingWindow}}

	// The scenario. Node i has findings i%200 and (7i+3)%200, version A on
	// even nodes and B on odd ones, and finding 0 is sos on every 400th
	// node. Nodes 0..1999 get a new flaw at 03:00 all at once, and nodes
	// 0..399 a flaw whose exact finding is one of 200, each 250 bytes.
	type incident struct{ finding, exactFinding, severity string }
	findingName := func(f int) string { return fmt.Sprintf("CVE-2026-%05d openssh", f) }
	incidentsOf := func(i int, burst bool) []incident {
		out := []incident{}
		for _, f := range []int{i % findings, (7*i + 3) % findings} {
			version := "A"
			if i%2 == 1 {
				version = "B"
			}
			severity := "warn"
			if f == 0 && i%400 == 0 {
				severity = "sos"
			}
			out = append(out, incident{findingName(f), findingName(f) + " version " + version, severity})
		}
		if i < manyNodes {
			out = append(out, incident{"many-flaws", fmt.Sprintf("%-250s", fmt.Sprintf("many-flaws exact finding %03d", i%(manyNodes/2))), "warn"})
		}
		if burst && i < burstNodes {
			out = append(out, incident{"burst-flaw", "burst-flaw version 1", "warn"})
		}
		return out
	}
	node := func(i int) string { return fmt.Sprintf("node-%04d", i) }
	alertOf := func(i int, in incident, at time.Time) checksandalerts.Event {
		h.events++
		a := checksandalerts.Event{ID: fmt.Sprintf("event-%06d", h.events), Incident: node(i) + " " + in.finding, Node: node(i), CheckGroup: "packages", Check: "flaws", Finding: in.finding, Type: "failed",
			AlertSeverity: in.severity, ExactFinding: in.exactFinding, At: at, Message: in.exactFinding}
		a.FleetIncidentKey = checksandalerts.ScopeFleetIncidentKey(checksandalerts.FleetIncidentKey(checksandalerts.ReadableFleetIncidentKey(a, "failed", "")), node(i), groupingFleet)
		a.WriteAlertTexts([]checksandalerts.Event{a}, "")
		return a
	}
	refused := map[int]checksandalerts.Event{}
	reportOf := func(i int, sent time.Time, burst bool) store.ReportedIncidents {
		r := store.ReportedIncidents{Sent: sent, Incidents: map[string]checksandalerts.Event{}, ReminderEligible: map[string]bool{}, SentWithoutTheHub: map[string]map[string]checksandalerts.Event{}}
		for _, in := range incidentsOf(i, burst) {
			shown := checksandalerts.Event{Node: node(i), CheckGroup: "packages", Check: "flaws", Finding: in.finding, Type: "failed", Incident: node(i) + " " + in.finding,
				AlertSeverity: in.severity, AlertSeveritiesSentForIncident: []string{in.severity}, At: start, ExactFinding: in.exactFinding}
			r.Incidents[shown.Incident], r.ReminderEligible[shown.Incident] = shown, true
		}
		// A node whose alert the hub refused delivered it itself, to each
		// destination that takes it, and carries the record of the direct
		// send.
		if direct, ok := refused[i]; ok {
			for _, name := range []string{"chat", "hotline"} {
				direct.AlertSeveritiesSentForIncident = []string{direct.AlertSeverity}
				r.SentWithoutTheHub[name] = map[string]checksandalerts.Event{direct.Incident: direct}
			}
		}
		return r
	}

	counts := map[string]map[string]int{}
	updates := map[string][]checksandalerts.Event{}
	deliver := func() {
		for name := range destinations {
			if counts[name] == nil {
				counts[name] = map[string]int{}
			}
			for _, a := range h.take(name) {
				counts[name][a.Type]++
				if a.Type == checksandalerts.FindingNodesChanged {
					updates[name] = append(updates[name], a)
				}
				if a.MMHotlineAlert != nil && len(a.MMHotlineAlert.Encode()) > 16*1024 {
					t.Fatalf("an MMHotline alert of %d bytes is beyond MMHotline's limit", len(a.MMHotlineAlert.Encode()))
				}
			}
		}
	}
	accepted, refusals := 0, 0
	accept := func(i int, a checksandalerts.Event, at time.Time) {
		switch e := s.Accept(a.Node, a, at); {
		case errors.Is(e, store.ErrIntakeFull):
			// The hub answers 503, and the node delivers the alert itself.
			refusals++
			refused[i] = a
		case e != nil:
			t.Fatal(e)
		default:
			accepted++
		}
	}
	largestBacklog := int64(0)
	step := func(at time.Time, reports map[string]store.ReportedIncidents) {
		var backlog int64
		if e := s.DB.QueryRow("SELECT COALESCE(SUM(length(event)),0) FROM intake WHERE state!=2").Scan(&backlog); e != nil {
			t.Fatal(e)
		}
		largestBacklog = max(largestBacklog, backlog)
		rows, e := s.Intake()
		if e != nil {
			t.Fatal(e)
		}
		p := h.grouping.Plan(rows, nil, at)
		if e = s.Emit(p, destinations, false, at); e != nil {
			t.Fatal(e)
		}
		for _, key := range p.Sent {
			h.grouping.Sent[key] = at
		}
		if len(reports) > 0 {
			h.report(at, reports)
		}
		h.tick(at)
		deliver()
	}

	every := simulatedHours * time.Hour / nodes
	next := 0
	burstAt := start.Add(3 * time.Hour)
	for at := start.Add(time.Minute); !at.After(start.Add(simulatedHours * time.Hour)); at = at.Add(time.Minute) {
		reports := map[string]store.ReportedIncidents{}
		for ; next < nodes && !start.Add(time.Duration(next)*every).After(at); next++ {
			for _, in := range incidentsOf(next, false) {
				accept(next, alertOf(next, in, start.Add(time.Duration(next)*every)), at)
			}
			reports[node(next)] = reportOf(next, at, !at.Before(burstAt))
		}
		if at.Equal(burstAt) {
			for i := range burstNodes {
				accept(i, alertOf(i, incident{"burst-flaw", "burst-flaw version 1", "warn"}, burstAt), burstAt)
			}
		}
		// The nodes of the burst report a minute later, the refused ones
		// with their direct sends, and again a minute after that.
		if at.Equal(burstAt.Add(time.Minute)) || at.Equal(burstAt.Add(2*time.Minute)) {
			for i := range burstNodes {
				reports[node(i)] = reportOf(i, at, true)
			}
		}
		step(at, reports)
	}

	if refusals == 0 || refusals >= burstNodes {
		t.Fatalf("the burst of %d alerts met the intake limit of %d bytes %d times", burstNodes, intakeLimit, refusals)
	}
	if largestBacklog > intakeLimit {
		t.Fatalf("the intake held %d bytes, beyond its limit of %d", largestBacklog, intakeLimit)
	}
	var waiting int
	if e = s.DB.QueryRow("SELECT COUNT(*) FROM intake WHERE state!=2").Scan(&waiting); e != nil || waiting != 0 {
		t.Fatal("alerts left in the intake", waiting, e)
	}
	// The refused nodes' direct sends are settled and acknowledged.
	acknowledged := 0
	for i, direct := range refused {
		carried := []store.IncidentSentWithoutTheHub{{Destination: "chat", Incident: direct.Incident, CheckGroup: "packages", Check: "flaws", Finding: direct.Finding, Type: "failed", AlertSeveritiesSentForIncident: []string{"warn"}, ExactFinding: direct.ExactFinding}}
		known, e := s.IncidentsKnownToTheHub(node(i), carried)
		if e != nil {
			t.Fatal(e)
		}
		acknowledged += len(known)
	}
	if acknowledged != len(refused) {
		t.Fatalf("%d of %d direct sends acknowledged", acknowledged, len(refused))
	}

	// What each destination got. Every key of the 200 has two exact
	// findings, the burst one, and many-flaws 200; sos is announced by node
	// 0 for finding 0, and its 12 other sos nodes come 1728 s apart, each in
	// an sos update; every key has growth for the shared tick at 06:00.
	sosUpdates := (nodes+399)/400 - 1
	immediate := findings*2 + 1 + manyNodes/2
	wantByDestination := map[string]map[string]int{
		"chat":    {"failed": immediate, checksandalerts.FindingNodesChanged: sosUpdates + findings + 2},
		"hotline": {"failed": immediate, checksandalerts.FindingNodesChanged: sosUpdates + findings + 2},
		"pager":   {"failed": 1, checksandalerts.FindingNodesChanged: sosUpdates},
	}
	for name, want := range wantByDestination {
		if fmt.Sprint(counts[name]) != fmt.Sprint(want) {
			t.Errorf("%s got %v, want %v", name, counts[name], want)
		}
	}
	for _, update := range updates["chat"] {
		switch update.Finding {
		case "burst-flaw":
			if update.NodeCount != burstNodes-1 {
				t.Errorf("the burst update names %d nodes, want every node of the burst but the first, %d", update.NodeCount, burstNodes-1)
			}
		case "many-flaws":
			if len(update.Body) > store.UpdateMessageLimit || len(update.NodesByExactFinding) >= manyNodes/2 || !strings.Contains(update.Body, "more exact findings on") {
				t.Errorf("the update of 200 exact findings has %d bytes and lists %d of them", len(update.Body), len(update.NodesByExactFinding))
			}
		}
	}
	for _, update := range updates["hotline"] {
		if len(update.MMHotlineAlert.Message) > store.UpdateMessageLimit+256 {
			t.Errorf("an MMHotline update of %d bytes", len(update.MMHotlineAlert.Message))
		}
	}
	var open int64
	if e = s.DB.QueryRow("SELECT COUNT(*) FROM fleet_node_incidents").Scan(&open); e != nil {
		t.Fatal(e)
	}
	wantOpen := int64(nodes*2 + manyNodes + burstNodes)
	if open != wantOpen {
		t.Fatalf("the hub holds %d open node incidents, want %d", open, wantOpen)
	}
	t.Logf("accepted %d alerts, refused %d into direct delivery; largest intake backlog %d bytes of %d", accepted, refusals, largestBacklog, intakeLimit)
	file, used := sizeOfStateDatabase()
	t.Logf("state.db with %d open incidents and 3 destinations: the file grew %d bytes, %d per open incident; the pages in use grew %d bytes, %d per open incident",
		open, file-emptyFile, (file-emptyFile)/open, used-emptyUsed, (used-emptyUsed)/open)
}
