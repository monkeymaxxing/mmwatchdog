package daemon

// The fuzz tests of the hub's join and report endpoints are in package
// daemon, not daemon_test: the endpoints are the unexported handlers
// handleJoin and handleReport of *Daemon, and a hub daemon that runs no
// server and no worker comes from newFuzzHub below.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/testhelpers"
)

// fuzzJoinSecret replaces the random join secret of each fuzzed hub, so that
// a seed can carry the secret that the hub expects.
const fuzzJoinSecret = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"

// The IDs of the seed reports.
const (
	fuzzDaemonStartID        = "0123456789abcdef0123456789abcdef"
	fuzzEventHistoryID       = "fedcba9876543210fedcba9876543210"
	fuzzMeasurementHistoryID = "00112233445566778899aabbccddeeff"
	fuzzIncidentID           = "aaaaaaaaaaaaaaaabbbbbbbbbbbbbbbb"
)

// fuzzJoinedNodeName is the node that sends the fuzzed reports.
const fuzzJoinedNodeName = "app-1"

// fuzzUnknownField is a field that no request to the hub has, in any letter
// case.
const fuzzUnknownField = `"mmwatchdog_fuzz_unknown_field":0,`

var fuzzCredentialPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// FuzzJoinRequestOnTheHub sends each join request to a shared hub. A join that
// the hub refuses changes nothing and gets a JSON error. A join that it
// accepts fits JoinLimit, carries the join secret, and gives a credential
// that authenticates the joined node, whose entry the fleet store holds.
func FuzzJoinRequestOnTheHub(f *testing.F) {
	valid := `"name":"app-1","secret":"` + fuzzJoinSecret + `","alert_severity_when_this_node_goes_silent":"warn"`
	join := func(fields string) []byte { return []byte("{" + fields + "}") }
	nodeGroups := func(n int) string {
		names := []string{}
		for i := range n {
			names = append(names, `"g`+strconv.Itoa(i)+`"`)
		}
		return strings.Join(names, ",")
	}
	for _, seed := range [][]byte{
		join(valid),
		join(valid + `,"raise_to_sos_after_this_node_is_silent_for":"10m","this_node_is_in_node_groups":["web","eu-central"],"raise_to_sos_when_every_node_of_these_node_groups_is_silent":["web"]`),
		join(strings.Replace(valid, `"app-1"`, `"web-2.example.com"`, 1)),
		join(valid + `,"this_node_is_in_node_groups":[` + nodeGroups(100) + `]`),
		join(valid + `,"this_node_is_in_node_groups":[` + nodeGroups(101) + `]`),
		join(valid + `,"raise_to_sos_after_this_node_is_silent_for":"500ms"`),
		join(strings.Replace(valid, fuzzJoinSecret, strings.Repeat("0", 64), 1)),
		join(`"name":"app-1","secret":"` + fuzzJoinSecret + `"`),
		join(strings.Replace(valid, `"warn"`, `"loud"`, 1)),
		join(strings.Replace(valid, `"app-1"`, `"a/b"`, 1)),
		join(strings.Replace(valid, `"app-1"`, `""`, 1)),
		join(strings.Replace(valid, `"app-1"`, `"`+strings.Repeat("a", 254)+`"`, 1)),
		join(valid + `,"extra":1`),
		join(valid + `,"name":"app-2"`),
		[]byte("{" + valid + "} {}"),
		[]byte("{" + valid + "}x"),
		[]byte("null"),
		[]byte("[]"),
		[]byte(""),
		[]byte("not json"),
	} {
		f.Add(seed, uint8(0))
	}
	f.Add(join(valid), uint8(1))
	f.Add(join(valid), uint8(2))
	d, now := newFuzzHub(f)
	f.Fuzz(func(t *testing.T, body []byte, boundary uint8) {
		body = fuzzBodyAtLimit(body, boundary, fleet.JoinLimit)
		nodesBefore, credentialsBefore, membersBefore := fuzzHubSizes(d)
		rowsBefore := fuzzRowCount(t, d, "SELECT COUNT(*) FROM nodes")
		d.hub.Mu.Lock()
		refusedBefore := d.hub.Refused.Count
		d.hub.Mu.Unlock()
		pendingBefore := d.pending
		w := postJoin(d, body)
		if w.Code == http.StatusOK {
			// Clean up even after a failed assertion, so minimization can
			// reproduce the fault without a previous input's joined node.
			t.Cleanup(func() {
				for name := range d.hub.Nodes {
					if name != d.hub.Name {
						forgetFuzzNode(t, d, name, now)
					}
				}
				d.pending = pendingBefore
			})
		}
		d.hub.Mu.Lock()
		refused := d.hub.Refused.Count - refusedBefore
		d.hub.Mu.Unlock()
		// The hub compares the secret only of a join that it decoded, and
		// counts every wrong one.
		if (w.Code == http.StatusForbidden) != (refused == 1) {
			t.Fatalf("status %d with %d refused joins", w.Code, refused)
		}
		if len(body) > fleet.JoinLimit && w.Code != http.StatusBadRequest {
			t.Fatalf("a join of %d bytes, over the limit of %d, got status %d", len(body), fleet.JoinLimit, w.Code)
		}
		if w.Code != http.StatusOK {
			if w.Code != http.StatusBadRequest && w.Code != http.StatusForbidden && w.Code != http.StatusConflict {
				t.Fatalf("a join got status %d: %s", w.Code, w.Body)
			}
			requireFuzzError(t, w, w.Code)
			nodes, credentials, members := fuzzHubSizes(d)
			if nodes != nodesBefore || credentials != credentialsBefore || members != membersBefore || fuzzRowCount(t, d, "SELECT COUNT(*) FROM nodes") != rowsBefore {
				t.Fatalf("a refused join (status %d) added a node", w.Code)
			}
			return
		}
		var reply struct {
			Credential string `json:"credential"`
		}
		decodeFuzzReply(t, w, &reply)
		if !fuzzCredentialPattern.MatchString(reply.Credential) {
			t.Fatalf("the credential %q is not 64 hexadecimal digits", reply.Credential)
		}
		if !json.Valid(body) {
			t.Fatal("the hub accepted a join that is not JSON")
		}
		// A plain decoder reads the same request from the accepted body.
		var request fleet.JoinRequest
		if e := json.Unmarshal(body, &request); e != nil {
			t.Fatal(e)
		}
		if request.Secret != fuzzJoinSecret {
			t.Fatalf("the hub accepted the join secret %q", request.Secret)
		}
		if request.Name == d.store.Node {
			t.Fatal("a node joined under the hub's own name")
		}
		d.hub.Mu.Lock()
		var credentialHash, settingsProblem string
		var joined time.Time
		if m := d.hub.Nodes[request.Name]; m != nil {
			credentialHash, settingsProblem, joined = m.Credential, m.NodeFleetSettings.Problem(), m.Joined
		}
		d.hub.Mu.Unlock()
		if credentialHash != fleet.HashCredential(reply.Credential) || settingsProblem != "" || !joined.Equal(now) {
			t.Fatalf("the hub holds node %q with the credential hash %q, joined at %s, settings problem %q", request.Name, credentialHash, joined, settingsProblem)
		}
		nodes, credentials, members := fuzzHubSizes(d)
		if nodes != nodesBefore+1 || credentials != credentialsBefore+1 || members != membersBefore+1 || fuzzRowCount(t, d, "SELECT COUNT(*) FROM nodes WHERE name=?", request.Name) != 1 {
			t.Fatalf("one join added %d nodes, %d credentials, and %d fleet members", nodes-nodesBefore, credentials-credentialsBefore, members-membersBefore)
		}
		if fleetCheck(d, request.Name) == nil {
			t.Fatal("the joined node has no fleet check")
		}
		authenticated := httptest.NewRequest(http.MethodGet, "https://hub.test/v1/settings", nil)
		authenticated.Header.Set("Authorization", "Bearer "+reply.Credential)
		if name, ok := d.hub.Authenticate(authenticated); !ok || name != request.Name {
			t.Fatalf("the credential authenticates %q, %v", name, ok)
		}
		// The same join stays strict, and the name now belongs to its node.
		requireFuzzError(t, postJoin(d, withAnUnknownField(t, body)), http.StatusBadRequest)
		requireFuzzError(t, postJoin(d, append(bytes.Clone(body), " {}"...)), http.StatusBadRequest)
		requireFuzzError(t, postJoin(d, body), http.StatusConflict)
		if _, credentials, _ := fuzzHubSizes(d); credentials != credentialsBefore+1 {
			t.Fatal("a refused join added a credential")
		}
	})
}

// FuzzNodeStatusReportOnTheHub sends reports from one joined node to a shared
// hub. A report that the hub refuses changes nothing and gets a JSON error.
// A report that it accepts fits ReportLimit, passes Validate as the hub saved
// it, is a receipt for the node's fleet check, cannot be sent again, and is
// what a restarted hub reads from its fleet store.
func FuzzNodeStatusReportOnTheHub(f *testing.F) {
	for _, seed := range fuzzReportSeeds() {
		f.Add(seed, uint8(0))
	}
	f.Add(fuzzReportSeeds()[0], uint8(1))
	f.Add(fuzzReportSeeds()[0], uint8(2))
	d, now := newFuzzHub(f)
	credential := joinFuzzNode(f, d)
	f.Fuzz(func(t *testing.T, body []byte, boundary uint8) {
		body = fuzzBodyAtLimit(body, boundary, fleet.ReportLimit)
		d.hub.Mu.Lock()
		memberBefore := *d.hub.Nodes[fuzzJoinedNodeName]
		d.hub.Mu.Unlock()
		entryBefore := storedNodeEntry(t, d, fuzzJoinedNodeName)
		eventsBefore := fuzzRowCount(t, d, "SELECT COUNT(*) FROM events WHERE node=?", fuzzJoinedNodeName)
		pendingBefore := d.pending
		w := postReport(d, credential, body)
		if w.Code == http.StatusOK {
			// Reset after all assertions, even a failed one. Forget clears
			// report ordering, histories, both cursors, and hub-outage alerter selection;
			// rejoining creates a fleet check with no previous receipt.
			t.Cleanup(func() {
				forgetFuzzNode(t, d, fuzzJoinedNodeName, now)
				credential = joinFuzzNode(t, d)
				d.pending = pendingBefore
			})
		}
		if len(body) > fleet.ReportLimit && w.Code != http.StatusBadRequest {
			t.Fatalf("a report of %d bytes, over the limit of %d, got status %d", len(body), fleet.ReportLimit, w.Code)
		}
		if w.Code != http.StatusOK {
			// The node has no saved report that this one could be older than,
			// so a refusal is the fault of the report itself.
			requireFuzzError(t, w, http.StatusBadRequest)
			d.hub.Mu.Lock()
			memberAfter := *d.hub.Nodes[fuzzJoinedNodeName]
			d.hub.Mu.Unlock()
			if memberAfter.NodeStatusReport != nil || !reflect.DeepEqual(memberAfter, memberBefore) || !bytes.Equal(storedNodeEntry(t, d, fuzzJoinedNodeName), entryBefore) || fuzzRowCount(t, d, "SELECT COUNT(*) FROM events WHERE node=?", fuzzJoinedNodeName) != eventsBefore {
				t.Fatal("a refused report changed the node's entry or its events")
			}
			return
		}
		var reply fleet.ReportReply
		decodeFuzzReply(t, w, &reply)
		if !json.Valid(body) {
			t.Fatal("the hub accepted a report that is not JSON")
		}
		var accepted fleet.NodeStatusReport
		if err := json.Unmarshal(body, &accepted); err != nil {
			t.Fatal(err)
		}
		// Check the original batch too: the saved snapshot deliberately
		// omits events and readings, which have their own stores.
		if err := accepted.Validate(); err != nil {
			t.Fatalf("the hub accepted an invalid report batch: %v", err)
		}
		d.hub.Mu.Lock()
		m := d.hub.Nodes[fuzzJoinedNodeName]
		saved, cursor := m.NodeStatusReport, m.Cursor
		d.hub.Mu.Unlock()
		if saved == nil {
			t.Fatal("the hub accepted a report that it did not save")
		}
		if e := saved.Validate(); e != nil {
			t.Fatalf("the saved report is invalid: %v", e)
		}
		if saved.Events != nil || saved.Readings != nil {
			t.Fatal("the saved report holds its events or its readings")
		}
		// The node is the one joined node that reports, so it is the hub-outage alerter.
		if !reply.Valid() || reply.EventsCursor != cursor || !reply.OutageAlerter || reply.FleetSettingsVersion != d.hub.FleetSettings.Load().Version {
			t.Fatalf("reply %+v for the hub's cursor %d", reply, cursor)
		}
		if st := fleetCheck(d, fuzzJoinedNodeName); st == nil || !st.Receipt.Equal(now) {
			t.Fatal("the accepted report is no receipt for the node's fleet check")
		}
		// The same report stays strict, and is older than itself once saved.
		requireFuzzError(t, postReport(d, credential, withAnUnknownField(t, body)), http.StatusBadRequest)
		requireFuzzError(t, postReport(d, credential, append(bytes.Clone(body), " {}"...)), http.StatusBadRequest)
		requireFuzzError(t, postReport(d, credential, body), http.StatusConflict)
		// A restarted hub reads the same report from its fleet store.
		want, e := json.Marshal(saved)
		if e != nil {
			t.Fatal(e)
		}
		if e := d.hub.Close(); e != nil {
			t.Fatal(e)
		}
		settings := d.hub.FleetSettings.Load()
		reopened, e := fleet.OpenHub(d.config.StateDir, d.config.FleetHubRole.AddressForNodes, d.store.Node, d.config.FleetHubRole.ReportEvery.Value(), d.config.FleetHubRole.HubLimits(), now)
		if e != nil {
			t.Fatalf("the hub cannot start again after it accepted the report: %v", e)
		}
		d.hub = reopened
		// OpenHub restores storage; daemon startup normally supplies these
		// settings. Keep the shared fixture's settings and fixed join secret.
		reopened.FleetSettings.Store(settings)
		reopened.Code.Secret = fuzzJoinSecret
		restored := reopened.Nodes[fuzzJoinedNodeName]
		if restored == nil || restored.NodeStatusReport == nil || restored.Cursor != cursor {
			t.Fatal("the restarted hub lost the accepted report or its cursor")
		}
		got, e := json.Marshal(restored.NodeStatusReport)
		if e != nil {
			t.Fatal(e)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("the restarted hub reads %s, not the saved %s", got, want)
		}
	})
}

// fuzzReportSeeds are the reports of TestReportRequestsAreAuthenticatedAndStrict
// and of the tests of events, readings, and incidents sent without the hub,
// with fixed IDs, and malformed and oversized bodies.
func fuzzReportSeeds() [][]byte {
	valid := `{"daemon_start_id":"` + fuzzDaemonStartID + `","report_sequence_number":99,"event_history_id":"` + fuzzEventHistoryID + `","events_after_sequence_number":-1,"measurement_history_id":"` + fuzzMeasurementHistoryID + `","readings_after_sequence_number":-1,"summary":{"exit_code":0,"attention":[]},"alert_severity_when_this_node_goes_silent":"warn"`
	report := func(fields string) []byte { return []byte(valid + fields + "}") }
	changed := func(old, replacement, fields string) []byte {
		return []byte(strings.Replace(valid, old, replacement, 1) + fields + "}")
	}
	eventsAfterZero := func(fields string) []byte {
		return changed(`"events_after_sequence_number":-1`, `"events_after_sequence_number":0`, fields)
	}
	readingsAfterFour := func(fields string) []byte {
		return changed(`"readings_after_sequence_number":-1`, `"readings_after_sequence_number":4`, fields)
	}
	sentWithoutTheHub := func(destination, incident, checkGroup, check, kind, severities string) string {
		return `,"incidents_sent_without_the_hub":[{"destination":"` + destination + `","incident_id":"` + incident + `","check_group":"` + checkGroup + `","check":"` + check + `","type":"` + kind + `","alert_severities_sent_for_incident":[` + severities + `]}]`
	}
	full := `,"sent_at":"2026-10-16T12:00:00Z","build":{"arch":"amd64","go":"go1.26.0","modified":false,"os":"linux","revision":"0123abc","version":"test"},"config_fingerprint":"` + fuzzMeasurementHistoryID + `",` +
		`"checks":{"app/shell":{"state":"ok","message":"fine"},"machine/failed_units":{"state":"failed","message":"1 unit failed","failure_incident":{"incident_id":"` + fuzzIncidentID + `","opened":"2026-10-16T11:50:00Z","alert_severity":"warn"},` +
		`"findings":{"nginx.service":{"state":"failed","message":"nginx.service failed","failure_incident":{"incident_id":"` + fuzzIncidentID + `","alert_severity":"sos","announced_incident_id":"` + fuzzIncidentID + `","alert_severities_sent_for_announced_incident":["warn","sos"]}}}}},` +
		`"node_resources":{"memory_used_percent":{"value":41,"unit":"percent"}},"maintenance":{"` + checksandalerts.EveryCheckGroup + `":{"until":"2026-10-16T12:10:00Z","reason":"deploy"}},"prefer_this_node_as_hub_outage_alerter":true,` +
		`"delivery":{"queued_notifications":0,"delivery_gaps":0,"last_error":"","failing_destinations":[]},"storage_error":"","raise_to_sos_after_this_node_is_silent_for":"10m","this_node_is_in_node_groups":["web","eu-central"],"raise_to_sos_when_every_node_of_these_node_groups_is_silent":["web"]`
	return [][]byte{
		report(""),
		report(full),
		eventsAfterZero(`,"events":[` + fuzzEvents(1, 2) + `]`),
		eventsAfterZero(`,"events":[` + fuzzEvents(3, 3) + `]`),
		eventsAfterZero(`,"events":[` + fuzzEvent(2) + `,` + fuzzEvent(1) + `]`),
		eventsAfterZero(`,"events":[` + fuzzEvents(1, fleet.ReportEvents+1) + `]`),
		readingsAfterFour(`,"readings":[` + fuzzReadings(5, "2026-10-16T11:00:00Z") + `,` + fuzzReadings(9, "2026-10-16T11:30:00Z") + `]`),
		report(`,"readings":[` + fuzzReadings(1, "2026-10-16T11:00:00Z") + `]`),
		readingsAfterFour(`,"readings":[` + fuzzReadings(5, "1500-01-01T00:00:00Z") + `]`),
		report(sentWithoutTheHub("on_call_pager", fuzzIncidentID, checksandalerts.NodeReportsCheckGroup, checksandalerts.HubCheck, "failed", `"warn","sos"`)),
		report(sentWithoutTheHub("a/b", fuzzIncidentID, "app", "shell", "failed", `"warn","sos"`)),
		report(sentWithoutTheHub("on_call_pager", "x", "app", "shell", "failed", `"warn","sos"`)),
		report(sentWithoutTheHub("on_call_pager", fuzzIncidentID, "app", "shell", "recovered", `"warn","sos"`)),
		report(sentWithoutTheHub("on_call_pager", fuzzIncidentID, "app", "shell", "failed", ``)),
		report(sentWithoutTheHub("on_call_pager", fuzzIncidentID, "app", "shell", "failed", `"sos","sos"`)),
		report(`,"checks":{"app/shell":{"state":"failed","message":"down","failure_incident":{"incident_id":"` + fuzzIncidentID + `","alert_severity":"loud"}}}`),
		report(`,"extra":1`),
		report(`,"report_sequence_number":100`),
		changed(`"warn"`, `"loud"`, ""),
		changed(`"exit_code":0`, `"exit_code":2`, ""),
		changed(`"report_sequence_number":99`, `"report_sequence_number":0`, ""),
		changed(`"report_sequence_number":99`, `"report_sequence_number":1e400`, ""),
		changed(`"events_after_sequence_number":-1`, `"events_after_sequence_number":9007199254740993`, ""),
		changed(fuzzDaemonStartID, "x", ""),
		[]byte(valid + "} {}"),
		[]byte(valid + "}x"),
		[]byte("null"),
		[]byte("[]"),
		[]byte(""),
		[]byte("not json"),
	}
}

// fuzzEvent is one event of the node's history with the sequence number seq.
func fuzzEvent(seq int) string {
	return `{"id":"` + fuzzIncidentID + `","node":"` + fuzzJoinedNodeName + `","check_group":"app","check":"shell","type":"maintenance_on","at":"2026-10-16T12:00:00Z","seq":` + strconv.Itoa(seq) + `}`
}

// fuzzEvents are the events from first to last, separated by commas.
func fuzzEvents(first, last int) string {
	events := []string{}
	for seq := first; seq <= last; seq++ {
		events = append(events, fuzzEvent(seq))
	}
	return strings.Join(events, ",")
}

// fuzzReadings are the readings of one run with the sequence number seq.
func fuzzReadings(seq int, at string) string {
	return `{"sequence_number":` + strconv.Itoa(seq) + `,"at":"` + at + `","check_group":"machine","check":"memory","measurements":{"memory_used_percent":{"value":41,"unit":"percent"}}}`
}

// fuzzBodyAtLimit keeps large padding out of the mutation corpus. Selectors
// 1 and 2 exercise the handler at its byte limit and one byte over it. All
// other selectors pass the raw bytes unchanged. Padding is JSON whitespace.
func fuzzBodyAtLimit(body []byte, boundary uint8, limit int) []byte {
	if boundary != 1 && boundary != 2 {
		return body
	}
	size := limit + int(boundary) - 1
	if len(body) >= size {
		return body
	}
	return append(bytes.Clone(body), bytes.Repeat([]byte(" "), size-len(body))...)
}

// withAnUnknownField is an accepted request body with fuzzUnknownField as the
// first field of its object.
func withAnUnknownField(t *testing.T, body []byte) []byte {
	t.Helper()
	start := bytes.IndexByte(body, '{')
	if start < 0 {
		t.Fatalf("the hub accepted a body without an object: %q", body)
	}
	out := append(bytes.Clone(body[:start+1]), fuzzUnknownField...)
	return append(out, body[start+1:]...)
}

// newFuzzHub is one hub daemon per fuzz process, in its own f.TempDir,
// with fuzzJoinSecret as its join secret. It runs no server and no worker,
// and its clock never moves: the time that it returns is the hub's now.
func newFuzzHub(f *testing.F) (*Daemon, time.Time) {
	f.Helper()
	clock := testhelpers.NewClock()
	path := f.TempDir()
	// Use the existing configuration writer so the fuzz test owns cleanup,
	// rather than the first input's testing.T closing the shared daemon.
	if e := testhelpers.WriteConfigText(path, fmt.Sprintf(`state_dir: %q
socket: %q
fleet_hub_role: {listen_on_address: '0.0.0.0:8445', address_for_nodes: "hub.test:8445"}
check_groups:
  app:
    shell: {script: "true"}
`, filepath.Join(path, "state"), filepath.Join(path, "api.sock"))); e != nil {
		f.Fatal(e)
	}
	d, e := newDaemon(path, clock.Now)
	if e != nil {
		f.Fatal(e)
	}
	f.Cleanup(func() { d.Close() })
	d.hub.Mu.Lock()
	d.hub.Code.Secret = fuzzJoinSecret
	d.hub.Mu.Unlock()
	return d, clock.Now()
}

// joinFuzzNode joins fuzzJoinedNodeName through the join endpoint and returns
// its credential.
func joinFuzzNode(t testing.TB, d *Daemon) string {
	t.Helper()
	w := postJoin(d, []byte(`{"name":"`+fuzzJoinedNodeName+`","secret":"`+fuzzJoinSecret+`","alert_severity_when_this_node_goes_silent":"warn"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("the join got status %d: %s", w.Code, w.Body)
	}
	var reply struct {
		Credential string `json:"credential"`
	}
	decodeFuzzReply(t, w, &reply)
	return reply.Credential
}

// forgetFuzzNode resets the node's persisted and in-memory state after the
// input's assertions. No server or worker runs alongside these fuzz tests.
func forgetFuzzNode(t *testing.T, d *Daemon, name string, now time.Time) {
	t.Helper()
	joined, status, e := d.hub.Forget(name)
	if e != nil || status != http.StatusOK {
		t.Fatalf("resetting node %q: status %d: %v", name, status, e)
	}
	d.forgot(name, joined, now)
	// Forget leaves a tombstone for the fleet incident worker, which this
	// fixture never starts. Do not accumulate those between fuzz inputs.
	delete(d.hub.Reported, name)
}

func postJoin(d *Daemon, body []byte) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	d.handleJoin(w, httptest.NewRequest(http.MethodPost, "https://hub.test/v1/join", bytes.NewReader(body)))
	return w
}

func postReport(d *Daemon, credential string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "https://hub.test/v1/report", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+credential)
	w := httptest.NewRecorder()
	d.handleReport(w, r)
	return w
}

// decodeFuzzReply decodes the hub's reply into out: one JSON object with no
// field that out does not have.
func decodeFuzzReply(t testing.TB, w *httptest.ResponseRecorder, out any) {
	t.Helper()
	if contentType := w.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("status %d with the Content-Type %q", w.Code, contentType)
	}
	decoder := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
	decoder.DisallowUnknownFields()
	if e := decoder.Decode(out); e != nil || decoder.More() {
		t.Fatalf("status %d with the reply %q: %v", w.Code, w.Body.String(), e)
	}
}

// requireFuzzError requires the status wantStatus and a JSON error that says
// something.
func requireFuzzError(t *testing.T, w *httptest.ResponseRecorder, wantStatus int) {
	t.Helper()
	if w.Code != wantStatus {
		t.Fatalf("status %d, want %d: %s", w.Code, wantStatus, w.Body)
	}
	var reply struct {
		Error string `json:"error"`
	}
	decodeFuzzReply(t, w, &reply)
	if reply.Error == "" {
		t.Fatalf("status %d with an empty error", w.Code)
	}
}

// fuzzHubSizes counts the hub's nodes and credentials, and the fleet members
// of the daemon. The hub's lock is never held together with the daemon's.
func fuzzHubSizes(d *Daemon) (nodes, credentials, members int) {
	d.hub.Mu.Lock()
	nodes, credentials = len(d.hub.Nodes), len(d.hub.Credentials)
	d.hub.Mu.Unlock()
	d.mu.Lock()
	members = len(d.members)
	d.mu.Unlock()
	return nodes, credentials, members
}

func fuzzRowCount(t *testing.T, d *Daemon, query string, args ...any) int {
	t.Helper()
	var n int
	if e := d.hub.DB.QueryRow(query, args...).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}

// storedNodeEntry is the node's entry in fleet.db, or nil without one.
func storedNodeEntry(t *testing.T, d *Daemon, name string) []byte {
	t.Helper()
	var data []byte
	e := d.hub.DB.QueryRow("SELECT data FROM nodes WHERE name=?", name).Scan(&data)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		t.Fatal(e)
	}
	return data
}
