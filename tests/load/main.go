// Command load is the synthetic fleet for the large-fleet load run. It joins
// many nodes to one hub, then reports for each on the fleet schedule with a
// new TLS connection per report, as a real node does. Phases follow on a
// fixed timeline: steady reporting, a group of nodes going silent, steady
// again, a group failing the same check, and steady again. It prints one JSON
// line of statistics every stats interval and one per phase at its end, and
// last the most failed report attempts in a row of any one node and the
// alerts still in flight when the phases ended.
// tests/integration/load.py runs it and measures the hub.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
)

type phase struct {
	Name string
	For  time.Duration
}

// readingsPerReport is how many runs with measurements a synthetic node has
// between two reports: a node with ten measurement checks that reports as
// often as it runs them.
const readingsPerReport = 10

// node is one synthetic node; only its own goroutine uses it after the join.
type node struct {
	name, credential, start, history string
	seq, after                       int64
	// measurementHistory identifies the node's measurement history, and
	// readingsAfter is the sequence number of the last reading that the hub
	// holds from the node; -1 until a reply gives it.
	measurementHistory string
	readingsAfter      int64
	failing            bool                    // its app/http check is failed
	log                []checksandalerts.Event // its local history, numbered from 1
	// A success resets the current count, but keeps the largest count.
	failedReportsInARow, mostFailedReportsInARow int
}

type stats struct {
	mu        sync.Mutex
	latencies []time.Duration
	errors    map[string]int
}

func (s *stats) add(d time.Duration, e error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e != nil {
		s.errors[strings.SplitN(e.Error(), ":", 2)[0]]++
		return
	}
	s.latencies = append(s.latencies, d)
}

// take returns the samples so far and starts over.
func (s *stats) take() ([]time.Duration, map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, e := s.latencies, s.errors
	s.latencies, s.errors = nil, map[string]int{}
	return l, e
}

func summary(l []time.Duration, errors map[string]int) map[string]any {
	slices.Sort(l)
	at := func(q float64) float64 {
		if len(l) == 0 {
			return 0
		}
		return float64(l[min(len(l)-1, int(q*float64(len(l))))].Microseconds()) / 1000
	}
	return map[string]any{"ok": len(l), "errors": errors, "p50_ms": at(0.5), "p95_ms": at(0.95), "p99_ms": at(0.99), "max_ms": at(1)}
}

func emit(v map[string]any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func main() {
	code := flag.String("code", "", "the hub's join code")
	count := flag.Int("nodes", 10000, "synthetic nodes")
	every := flag.Duration("every", 30*time.Second, "the hub's report_every")
	prefix := flag.String("prefix", "load", "node name prefix")
	silence := flag.Int("silence", 1000, "nodes that go silent")
	fail := flag.Int("fail", 1000, "nodes that fail the same check")
	steady := flag.Duration("steady", 5*time.Minute, "first steady phase")
	silenceFor := flag.Duration("silence-for", 3*time.Minute, "silent phase")
	failFor := flag.Duration("fail-for", 2*time.Minute, "failing phase")
	settle := flag.Duration("settle", 2*time.Minute, "steady phase after each group")
	interval := flag.Duration("stats", 10*time.Second, "statistics interval")
	flag.Parse()
	join, e := fleet.ParseJoinCode(*code)
	if e != nil || *silence+*fail > *count {
		log.Fatalf("need a valid -code and -silence plus -fail within -nodes: %v", e)
	}
	nodes := make([]*node, *count)
	for i := range nodes {
		nodes[i] = &node{name: fmt.Sprintf("%s-%05d", *prefix, i), start: incidents.ID(), history: incidents.ID(), after: -1, measurementHistory: incidents.ID(), readingsAfter: -1}
	}

	// A real node joins as soon as it starts. Each synthetic node joins at a
	// random moment in the first report interval, as nodes that a deploy
	// starts during that interval would, tries again each interval until it
	// has, and reports from then on. The phases start once all have joined.
	joinStats := &stats{errors: map[string]int{}}
	reports, alerts := &stats{errors: map[string]int{}}, &stats{errors: map[string]int{}}
	phaseReports := &stats{errors: map[string]int{}}
	phases := []phase{{"steady", *steady}, {"silence", *silenceFor}, {"settle-silence", *settle}, {"fail", *failFor}, {"settle-fail", *settle}}
	var current atomic.Value
	current.Store("join")
	began := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	var wg, joined sync.WaitGroup
	joined.Add(len(nodes))
	for i, s := range nodes {
		wg.Go(func() {
			next := time.Now().Add(rand.N(*every))
			for s.credential == "" {
				time.Sleep(time.Until(next))
				next = next.Add(*every)
				t := time.Now()
				var reply struct{ Credential string }
				e := send(join, "/v1/join", "", s.joinRequest(join.Secret), &reply)
				joinStats.add(time.Since(t), e)
				s.credential = reply.Credential
			}
			joined.Done()
			for {
				p := current.Load().(string)
				if p != "silence" || i >= *silence {
					failing := p == "fail" && i >= *silence && i < *silence+*fail
					if failing != s.failing {
						s.failing = failing
						t := time.Now()
						e := send(join, "/v1/alerts", s.credential, s.transition(), nil)
						alerts.add(time.Since(t), e)
					}
					t := time.Now()
					var reply struct {
						EventsCursor   int64 `json:"events_cursor"`
						ReadingsCursor int64 `json:"readings_cursor"`
					}
					e := send(join, "/v1/report", s.credential, s.report(), &reply)
					reports.add(time.Since(t), e)
					phaseReports.add(time.Since(t), e)
					s.countReport(e)
					if e == nil {
						s.after, s.readingsAfter = reply.EventsCursor, reply.ReadingsCursor
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Until(next)):
				}
				next = next.Add(*every)
			}
		})
	}
	done := make(chan struct{})
	go func() { joined.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * *every):
		log.Fatal("not every node joined within five report intervals")
	}
	l, errors := joinStats.take()
	result := summary(l, errors)
	result["phase"], result["seconds"] = "join", time.Since(began).Seconds()
	emit(result)
	phaseReports.take()

	go func() {
		for range time.Tick(*interval) {
			l, errors := reports.take()
			line := summary(l, errors)
			line["phase"], line["t"] = current.Load(), int(time.Since(began).Seconds())
			emit(line)
		}
	}()
	for _, p := range phases {
		current.Store(p.Name)
		emit(map[string]any{"phase": p.Name, "started": float64(time.Now().UnixMilli()) / 1000, "t": int(time.Since(began).Seconds())})
		time.Sleep(p.For)
		l, errors := phaseReports.take()
		line := summary(l, errors)
		line["phase"], line["ended"] = p.Name, true
		al, ae := alerts.take()
		line["alerts"] = summary(al, ae)
		emit(line)
	}
	cancel()
	wg.Wait()
	// Every node's goroutine has ended, its reports and alerts in flight
	// counted.
	name, most := "", 0
	for _, s := range nodes {
		if s.mostFailedReportsInARow > most {
			name, most = s.name, s.mostFailedReportsInARow
		}
	}
	al, ae := alerts.take()
	emit(map[string]any{"phase": "end", "most_failed_reports_in_a_row_of_one_node": most, "node_with_most_failed_reports_in_a_row": name, "alerts": summary(al, ae)})
}

// joinRequest is the node's join with the join code's secret.
func (s *node) joinRequest(secret string) map[string]string {
	return map[string]string{"name": s.name, "secret": secret, "alert_severity_when_this_node_goes_silent": "warn"}
}

// countReport counts actual report attempts. The deliberate silence sends
// nothing and never calls it; phase changes do not reset the count.
func (s *node) countReport(e error) {
	if e == nil {
		s.failedReportsInARow = 0
		return
	}
	s.failedReportsInARow++
	s.mostFailedReportsInARow = max(s.mostFailedReportsInARow, s.failedReportsInARow)
}

// report is the node's next report: a small node's usual state, with its
// events after the hub's cursor.
func (s *node) report() fleet.NodeStatusReport {
	s.seq++
	now := time.Now().UTC()
	ok := func(message string) fleet.ReportedCheck { return fleet.ReportedCheck{State: "ok", Message: message} }
	checks := map[string]fleet.ReportedCheck{
		"app/service":                                 ok("app.service is active (running)"),
		"app/http":                                    ok("GET http://127.0.0.1:8080/health returned 200 in 3ms"),
		"machine/cpu_use_is_recorded":                 ok("cpu_used_percent is 12.5 over 30.000s"),
		"machine/memory_is_below_90_percent":          ok("memory_used_percent is 41.2 (fails above 90)"),
		"machine/root_filesystem_is_below_85_percent": ok("space_used_percent is 37.9 (fails above 85)"),
	}
	summary := incidents.StatusSummary{Attention: []incidents.Attention{}}
	if s.failing {
		checks["app/http"] = fleet.ReportedCheck{State: "failed", Message: "GET http://127.0.0.1:8080/health returned 503", FailureIncident: &incidents.Track{Incident: incidents.ID(), Opened: now, AlertSeverity: "warn"}}
		summary = incidents.StatusSummary{ExitCode: 1, Attention: []incidents.Attention{{Check: "app/http", State: "failed", Since: now, Message: "GET http://127.0.0.1:8080/health returned 503"}}}
	}
	r := fleet.NodeStatusReport{StartID: s.start, Seq: s.seq, Sent: now, Config: strings.Repeat("ab", 32), Summary: summary, Checks: checks,
		Build:         fleet.Build{Arch: "amd64", Go: "go1.26", OS: "linux", Revision: strings.Repeat("0", 40), Version: "load"},
		NodeResources: map[string]checksandalerts.Measurement{"cpu": {Value: 12.5, Unit: "percent"}, "memory": {Value: 41.2, Unit: "percent"}, "disk": {Value: 37.9, Unit: "percent"}},
		Maintenance:   map[string]checksandalerts.Window{}, History: s.history, After: s.after, AlertSeverityWhenThisNodeGoesSilent: "warn",
		MeasurementHistoryID: s.measurementHistory, ReadingsAfter: s.readingsAfter}
	// Once the hub has said where it is, each report carries the readings of
	// the checks that a node runs between two reports.
	for i := int64(1); s.readingsAfter >= 0 && i <= readingsPerReport; i++ {
		r.Readings = append(r.Readings, store.RunReadings{SequenceNumber: s.readingsAfter + i, At: now, CheckGroup: "machine", Check: fmt.Sprintf("resource_%d", i),
			Measurements: map[string]store.MeasurementReading{"used_percent": {Value: 41.2, Unit: "percent"}, "free_gib": {Value: 12.5, Unit: "GiB"}}})
	}
	if s.after >= 0 && s.after < int64(len(s.log)) {
		r.Events = s.log[s.after:]
	}
	return r
}

// transition is the alert for the node's app/http change, which its next
// report also carries as a local event. The failure and the recovery carry
// the fleet incident key of the app/http failure, as a real node's do.
func (s *node) transition() checksandalerts.Event {
	e := checksandalerts.Event{ID: incidents.ID(), Node: s.name, CheckGroup: "app", Check: "http", Type: "recovered", AlertSeverity: "warn", AlertSeveritiesSentForIncident: []string{"warn"}, At: time.Now().UTC(), Message: "GET http://127.0.0.1:8080/health returned 200"}
	if s.failing {
		e.Type, e.Message = "failed", "GET http://127.0.0.1:8080/health returned 503"
	}
	e.FleetIncidentKey = checksandalerts.FleetIncidentKey(checksandalerts.ReadableFleetIncidentKey(e, checksandalerts.FailureIncidentEvents.Failed, ""))
	s.log = append(s.log, e)
	s.log[len(s.log)-1].Seq = int64(len(s.log))
	return e
}

// send posts v on a new connection that trusts only the join code's
// certificate, and decodes a 200 reply into reply.
func send(join fleet.JoinCode, path, credential string, v, reply any) error {
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if hex.EncodeToString(sum[:]) != join.Fingerprint {
				return fmt.Errorf("certificate mismatch")
			}
			return nil
		}}}
	defer transport.CloseIdleConnections()
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, "POST", "https://"+join.Address+path, bytes.NewReader(b))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	resp, e := (&http.Client{Transport: transport}).Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("timeout: %v", e)
		}
		return fmt.Errorf("connection: %v", e)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	if reply != nil {
		return json.Unmarshal(body, reply)
	}
	return nil
}
