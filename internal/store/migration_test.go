package store_test

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/store"
)

// format20Tables is the layout of state.db in format 20, as MMWatchdog
// 0.1.x writes it.
const format20Tables = `
 CREATE TABLE snapshot (id INTEGER PRIMARY KEY CHECK(id=1), data BLOB NOT NULL);
 CREATE TABLE checks (key TEXT PRIMARY KEY, data BLOB NOT NULL);
 CREATE TABLE events (seq INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, data BLOB NOT NULL);
 CREATE TABLE outbox (seq INTEGER PRIMARY KEY, at INTEGER NOT NULL, queued_at INTEGER NOT NULL, destination TEXT NOT NULL, event BLOB NOT NULL, target BLOB NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, next INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '');
 CREATE INDEX outbox_dest ON outbox(destination,seq);
 CREATE TABLE counters (name TEXT PRIMARY KEY, value INTEGER NOT NULL);
 CREATE TABLE destination_health (destination TEXT PRIMARY KEY, data BLOB NOT NULL);
 CREATE TABLE history (id INTEGER PRIMARY KEY CHECK(id=1), history_id TEXT NOT NULL);
 CREATE TABLE intake (seq INTEGER PRIMARY KEY AUTOINCREMENT, node TEXT NOT NULL, id TEXT NOT NULL, received INTEGER NOT NULL, type TEXT NOT NULL, subject TEXT NOT NULL, grp TEXT NOT NULL, destination TEXT NOT NULL, severity TEXT NOT NULL, event BLOB NOT NULL, state INTEGER NOT NULL DEFAULT 0, UNIQUE(node,id));
 CREATE TABLE fleet_incident_set (destination TEXT NOT NULL, alert_source TEXT NOT NULL, reporter TEXT NOT NULL, incident_id TEXT NOT NULL, incident_key TEXT NOT NULL, check_group TEXT NOT NULL, check_name TEXT NOT NULL, finding TEXT NOT NULL, node TEXT NOT NULL, opened INTEGER NOT NULL, alert_severity TEXT NOT NULL, state INTEGER NOT NULL, changed INTEGER NOT NULL, PRIMARY KEY(destination,alert_source,reporter,incident_id));
 CREATE INDEX fleet_incident_set_key ON fleet_incident_set(destination,alert_source,incident_key,state);
 CREATE TABLE incidents_sent_without_the_hub (destination TEXT NOT NULL, incident_id TEXT NOT NULL, check_group TEXT NOT NULL, check_name TEXT NOT NULL, finding TEXT NOT NULL, kind TEXT NOT NULL, alert_severities_sent_for_incident TEXT NOT NULL, at INTEGER NOT NULL, PRIMARY KEY(destination,incident_id));
 CREATE TABLE fleet_incident_keys (destination TEXT NOT NULL, alert_source TEXT NOT NULL, incident_key TEXT NOT NULL, alert_severities_sent TEXT NOT NULL, last_closing_alert_at INTEGER NOT NULL, PRIMARY KEY(destination,alert_source,incident_key));
 CREATE TABLE incident_alert_destinations (incident_id TEXT NOT NULL, destination TEXT NOT NULL, at INTEGER NOT NULL, PRIMARY KEY(incident_id,destination));
 PRAGMA user_version=20;
`

// format20Database writes state.db in format 20 into a new state directory
// with the rows of fill, and returns the directory.
func format20Database(t *testing.T, fill string, args ...any) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	db, e := sql.Open("sqlite", filepath.Join(dir, "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e = db.Exec(format20Tables); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(`INSERT INTO snapshot(id,data) VALUES(1,'{"recovery_attempts_by_target":{},"maintenance":{}}');
 INSERT INTO checks(key,data) VALUES('app/http','{"check_group":"app","check":"http","failure_incident":{"incident_id":"i-http","announced_incident_id":"i-http","alert_severity":"warn"}}')`); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(fill, args...); e != nil {
		t.Fatal(e)
	}
	return dir
}

func openMigrated(t *testing.T, dir string) *store.Store {
	t.Helper()
	s, e := store.OpenStore(dir, config.DefaultStorageLimits)
	if e != nil {
		t.Fatal("format 20 did not migrate:", e)
	}
	t.Cleanup(func() { s.Close() })
	var version int
	if e = s.DB.QueryRow("PRAGMA user_version").Scan(&version); e != nil || version != 21 {
		t.Fatal("migrated format", version, e)
	}
	snap, e := s.Load()
	if e != nil || snap.Checks["app/http"] == nil || snap.Checks["app/http"].FailureIncident.Incident != "i-http" {
		t.Fatal("the migrated state lost the open incident", e)
	}
	return s
}

// The migration from format 20 keeps every role's open incidents and
// queued alerts, and seeds what the hub announced from the open sets and
// keys: the highest severity sent since the key's last closing alert, and
// no exact finding.
func TestFormat20MigratesInPlaceOnEveryRole(t *testing.T) {
	t.Parallel()
	sosFailure, _ := json.Marshal(checksandalerts.Event{ID: "e-1", Node: "app-1", CheckGroup: "app", Check: "http", Type: "failed", AlertSeverity: "sos", FleetIncidentKey: "0123456789abcdef0123456789abcdef", At: time.Unix(100, 0)})
	t.Run("hub", func(t *testing.T) {
		t.Parallel()
		queued := `{"id":"n-1","node":"app-2","check_group":"app","check":"http","type":"failed","alert_severity":"warn"}`
		dir := format20Database(t, `
 INSERT INTO fleet_incident_set VALUES('pager','','app-1','i-1','app/http failed','app','http','','app-1',1,'warn',0,1);
 INSERT INTO fleet_incident_set VALUES('pager','','app-1','i-2','app/dns failed','app','dns','','app-1',1,'warn',2,1);
 INSERT INTO fleet_incident_keys VALUES('pager','','app/http failed','info,warn',0);
 INSERT INTO fleet_incident_keys VALUES('pager','','app/dns failed','',5);
 INSERT INTO outbox(at,queued_at,destination,event,target) VALUES(1,1,'pager',?,'{}');
 INSERT INTO intake(node,id,received,type,subject,grp,destination,severity,event,state) VALUES('app-2','n-1',1,'failed','app'||char(0)||'http','','','warn','`+queued+`',0);
 INSERT INTO intake(node,id,received,type,subject,grp,destination,severity,event,state) VALUES('app-3','n-2',1,'failed','app'||char(0)||'http','','','warn',x'',2)`, sosFailure)
		s := openMigrated(t, dir)
		var announced, exactFindings string
		if e := s.DB.QueryRow("SELECT announced_alert_severity, announced_exact_findings FROM fleet_incident_keys WHERE incident_key='app/http failed'").Scan(&announced, &exactFindings); e != nil || announced != "warn" || exactFindings != "[]" {
			t.Fatal("an open key's announcement", announced, exactFindings, e)
		}
		if e := s.DB.QueryRow("SELECT announced_alert_severity FROM fleet_incident_keys WHERE incident_key='app/dns failed'").Scan(&announced); e != nil || announced != "" {
			t.Fatal("a key with nothing open is announced", announced, e)
		}
		var pending string
		if e := s.DB.QueryRow("SELECT pending_change FROM fleet_incident_set WHERE incident_id='i-1'").Scan(&pending); e != nil || pending != "" {
			t.Fatal(pending, e)
		}
		var urgent bool
		var key string
		if e := s.DB.QueryRow("SELECT urgent, incident_key FROM outbox").Scan(&urgent, &key); e != nil || !urgent || key != "0123456789abcdef0123456789abcdef" {
			t.Fatal("a queued sos failure does not keep its place as sos", urgent, key, e)
		}
		var nodeBytes, allBytes int
		if e := s.DB.QueryRow("SELECT bytes FROM intake_queued_bytes WHERE node='app-2'").Scan(&nodeBytes); e != nil || nodeBytes != len(queued) {
			t.Fatal("queued bytes of app-2", nodeBytes, e)
		}
		if e := s.DB.QueryRow("SELECT value FROM counters WHERE name='intake_queued_bytes'").Scan(&allBytes); e != nil || allBytes != len(queued) {
			t.Fatal("queued bytes of the intake", allBytes, e)
		}
		if ds, e := s.Pending(); e != nil || len(ds) != 1 || ds[0].Event.ID != "e-1" {
			t.Fatal("the queued alert is lost", ds, e)
		}
	})
	t.Run("hub with an open MMHotline incident", func(t *testing.T) {
		t.Parallel()
		// 0.1.x opened the incident under the destination's own alert source.
		dir := format20Database(t, `
 INSERT INTO fleet_incident_set VALUES('hotline','source-1','app-1','incident-app-1','myapp/dns failed','myapp','dns','','app-1',1,'sos',0,1);
 INSERT INTO fleet_incident_keys VALUES('hotline','source-1','myapp/dns failed','sos',0)`)
		h := &groupingHub{t: t, s: openMigrated(t, dir), destinations: map[string]checksandalerts.Destination{"hotline": hotlineWithNodeAlertSource()}}
		h.s.FleetIdentity = groupingFleet
		// After the upgrade a node sends the key without the hub, under the
		// node alert source; then every node recovers.
		h.report(tenOFive, map[string]store.ReportedIncidents{"app-1": reportOf("app-1", "sos", "-", time.Unix(0, 1), tenOFive), "app-2": reportOf("app-2", "sos", nodeAlertSource, tenOFive, tenOFive)})
		expectKinds(t, h.take("hotline"))
		later := tenOFive.Add(time.Hour)
		h.report(later, map[string]store.ReportedIncidents{"app-1": {Sent: later}, "app-2": {Sent: later}})
		got := h.take("hotline")
		expectKinds(t, got, "recovered resolved-sos", "recovered resolved-sos")
		if sourceOf(got[0]) != "source-1" || sourceOf(got[1]) != nodeAlertSource {
			t.Fatal("the resolutions go under", sourceOf(got[0]), sourceOf(got[1]))
		}
	})
	t.Run("joined node", func(t *testing.T) {
		t.Parallel()
		// 0.1.x recorded a direct send when it queued it. Pushover, a
		// service that keeps incidents, took the warning of i-2, and its
		// raise to sos still waits to be retried; 0.1.x queued such an
		// alert as the node's own event. Both stay as 0.1.x wrote them.
		raise, _ := json.Marshal(checksandalerts.Event{ID: "e-2", Incident: "i-2", Node: "app-1", CheckGroup: "app", Check: "dns", Type: checksandalerts.FailureIncidentEvents.Raised, AlertSeverity: "sos", FleetIncidentKey: "fedcba9876543210fedcba9876543210", At: time.Unix(200, 0)})
		dir := format20Database(t, `
 INSERT INTO incidents_sent_without_the_hub VALUES('pager','i-1','app','http','','failed','warn,sos',1);
 INSERT INTO incidents_sent_without_the_hub VALUES('pushover','i-2','app','dns','','failed','warn,sos',2);
 INSERT INTO outbox(at,queued_at,destination,event,target) VALUES(1,1,'hub',?,'{}');
 INSERT INTO outbox(at,queued_at,destination,event,target) VALUES(2,2,'pushover','`+string(raise)+`','{}')`, sosFailure)
		s := openMigrated(t, dir)
		records, e := s.SentWithoutTheHub()
		if e != nil || len(records) != 2 || !slices.Equal(records[0].AlertSeveritiesSentForIncident, []string{"warn", "sos"}) || !slices.Equal(records[1].AlertSeveritiesSentForIncident, []string{"warn", "sos"}) {
			t.Fatal("a record of a direct send changed", records, e)
		}
		ds, e := s.Pending()
		if e != nil || len(ds) != 2 || ds[0].Destination != "hub" || ds[1].Destination != "pushover" {
			t.Fatal("a queued alert is lost", ds, e)
		}
	})
	t.Run("standalone node", func(t *testing.T) {
		t.Parallel()
		dir := format20Database(t, `
 INSERT INTO fleet_incident_set VALUES('pager','','web-1','i-1','app/http failed','app','http','','web-1',1,'sos',0,1);
 INSERT INTO fleet_incident_keys VALUES('pager','','app/http failed','warn,sos',0)`)
		s := openMigrated(t, dir)
		var announced string
		if e := s.DB.QueryRow("SELECT announced_alert_severity FROM fleet_incident_keys").Scan(&announced); e != nil || announced != "sos" {
			t.Fatal(announced, e)
		}
	})
}

// joinedNode013 holds the rows that a 0.1.3 joined node wrote for its
// direct MMHotline sends. It is embedded, so the test binary runs where
// testdata/ was not copied, as in the native VM suite.
//
//go:embed testdata/joined_node_0.1.3_direct_mmhotline_raises_waiting.sql
var joinedNode013 string

// recordsOf013 are the columns of a direct-send record in format 20.
const recordsOf013 = "SELECT destination,incident_id,check_group,check_name,finding,kind,alert_severities_sent_for_incident,at FROM incidents_sent_without_the_hub ORDER BY incident_id"

// The migration keeps a 0.1.3 joined node's direct sends as 0.1.3 wrote
// them: the records of its direct sends, and the raises that wait in the
// outbox, which get no record on acceptance. They behave as in 0.1.3: the
// hub settles the waiting raises from the records, and the node delivers
// them.
func TestFormat20KeepsAJoinedNodesDirectSendsAsTheyWere(t *testing.T) {
	t.Parallel()
	dir := format20Database(t, joinedNode013)
	const queued = "SELECT seq,destination,event,target,attempts,next,error FROM outbox ORDER BY seq"
	outboxBefore, recordsBefore := rowsOf(t, dir, queued), rowsOf(t, dir, recordsOf013)
	node := openMigrated(t, dir)
	if got := rowsOf(t, dir, "SELECT seq,destination,event,target,attempts,next,error FROM outbox WHERE sent_without_the_hub='' ORDER BY seq"); !slices.Equal(got, outboxBefore) || len(got) != 2 {
		t.Fatalf("the queued raises changed:\n%q\nwant\n%q", got, outboxBefore)
	}
	if got := rowsOf(t, dir, recordsOf013); !slices.Equal(got, recordsBefore) || len(got) != 2 {
		t.Fatalf("the records of the direct sends changed:\n%q\nwant\n%q", got, recordsBefore)
	}
	h := newGroupingHub(t, map[string]checksandalerts.Destination{"hotline": hotline()})
	// The node has recovered when it reaches the hub again.
	h.report(tenOFive, map[string]store.ReportedIncidents{"app-1": carrying(t, node, "app-1", tenOFive)})
	want := map[string]string{"anacron.service": "resolved-sos", "cron.service": "resolved-sos"}
	if got := resolutionsByFinding(t, h.take("hotline")); !maps.Equal(got, want) {
		t.Fatalf("the hub closed %v, want %v", got, want)
	}
	expectKinds(t, accept(t, node), checksandalerts.FailureIncidentEvents.Raised+" sos", checksandalerts.FailureIncidentEvents.Raised+" sos")
	if got := rowsOf(t, dir, recordsOf013); !slices.Equal(got, recordsBefore) {
		t.Fatalf("the accepted raises changed the records: %q", got)
	}
}

// rowsOf reads the rows of query from state.db in dir, each as one line.
func rowsOf(t *testing.T, dir, query string) []string {
	t.Helper()
	db, e := sql.Open("sqlite", filepath.Join(dir, "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	rows, e := db.Query(query)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	columns, e := rows.Columns()
	if e != nil {
		t.Fatal(e)
	}
	out := []string{}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if e = rows.Scan(pointers...); e != nil {
			t.Fatal(e)
		}
		out = append(out, fmt.Sprintf("%q", values))
	}
	if e = rows.Err(); e != nil {
		t.Fatal(e)
	}
	return out
}

// resolutionsByFinding names the severity of each MMHotline resolution in
// got by the finding of its key, anacron.service or cron.service.
func resolutionsByFinding(t *testing.T, got []checksandalerts.Event) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, a := range got {
		finding := "cron.service"
		if strings.Contains(a.MMHotlineAlert.Title, "anacron.service") {
			finding = "anacron.service"
		}
		if sourceOf(a) != "source-1" || out[finding] != "" {
			t.Fatalf("a resolution under %s, or a second one of %s: %+v", sourceOf(a), finding, got)
		}
		out[finding] = a.MMHotlineAlert.Severity
	}
	return out
}

// MMWatchdog 0.2.0 makes apt_security_updates report its waiting updates
// as the one finding package security. A check of it that a 0.1.3 node
// kept in state.db, in format 20, with its one incident for the whole
// check, is a new check after the migration: its announced incident
// closes once, with a message that says why, and the waiting updates that
// the upgraded check then observes, twice, open the finding as one
// incident of its own. A healthy check changes without an alert.
func TestACheckWhoseModuleNowReportsFindingsRetiresItsIncidentOnce(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC)
	timing := checksandalerts.Timing{CheckEvery: "1h", TimeLimitPerRun: "1m", AlertAfterFailingFor: "0s", RecoveredAfterHealthyFor: "0s"}
	check := checksandalerts.Check{Inputs: &checksandalerts.AptSecurityUpdates{FailWhen: "a_security_update_waits_to_be_installed"}, AlertSeveritySettings: checksandalerts.AlertSeveritySettings{AlertSeverity: "warn"}}
	const key = "software/no_security_update_waits"
	defs := map[string]config.Definition{key: {CheckGroup: "software", Name: "no_security_update_waits", Check: check, Timing: timing, Severity: check.AlertSeveritySettings}}
	const waiting = "2 security updates wait to be installed: curl, openssl"
	for _, failing := range []bool{true, false} {
		// The check's row as MMWatchdog 0.1.3 wrote it: one incident for
		// the whole check, with no incident for each finding.
		row := map[string]any{"check_group": "software", "check": "no_security_update_waits", "fingerprint": defs[key].Identity(),
			"source": map[string]any{}, "timing": map[string]any{"check_every": "1h", "time_limit_per_run": "1m", "alert_after_failing_for": "0s", "recovered_after_healthy_for": "0s"},
			"alert_severity_settings": map[string]any{"alert_severity": "warn"}, "cannot_check_incident": map[string]any{}}
		announced := ""
		if failing {
			announced = "i-apt-013"
			row["failing_since"] = now
			row["last_observation"] = map[string]any{"protocol": 1, "status": "failed", "message": waiting, "at": now}
			row["failure_incident"] = map[string]any{"incident_id": announced, "opened": now, "alert_severity": "warn", "announced_incident_id": announced, "alert_severities_sent_for_announced_incident": []string{"warn"}}
		} else {
			row["healthy_since"] = now
			row["last_observation"] = map[string]any{"protocol": 1, "status": "ok", "message": "no security update waits", "at": now}
			row["failure_incident"] = map[string]any{}
		}
		data, e := json.Marshal(row)
		if e != nil {
			t.Fatal(e)
		}
		s := openMigrated(t, format20Database(t, "INSERT INTO checks(key,data) VALUES(?,?)", key, data))
		snap, e := s.Load()
		if e != nil {
			t.Fatal(e)
		}
		if st := snap.Checks[key]; st == nil || st.OneIncidentForEachFinding || st.FailureIncident.Announced != announced {
			t.Fatalf("the loaded 0.1.3 state: %+v", st)
		}
		events := snap.Reconcile(&config.Config{}, defs, now.Add(time.Minute))
		events = append(events, snap.Reconcile(&config.Config{}, defs, now.Add(2*time.Minute))...)
		st := snap.Checks[key]
		if !st.OneIncidentForEachFinding || st.FailureIncident.Incident != "" || st.FailureIncident.Announced != "" {
			t.Fatalf("the check after the upgrade: %+v", st)
		}
		// The upgraded check observes the waiting updates twice.
		status, findings := "ok", []checksandalerts.Finding(nil)
		if failing {
			status, findings = "failed", []checksandalerts.Finding{{ID: checksandalerts.PackageSecurity, Message: waiting, ExactFinding: "debian 13: security updates wait"}}
		}
		for run := range 2 {
			events = append(events, st.Observe(checksandalerts.Observation{Protocol: 1, Status: status, Message: waiting, At: now.Add(time.Duration(run+1) * time.Hour), Findings: findings}, timing, false)...)
		}
		retired, opened := 0, 0
		for _, e := range events {
			switch {
			case e.CheckGroup != "software":
				// The fixture's own check, app/http, which these
				// definitions leave out.
			case e.Type == "closed_without_recovery" && e.Incident == announced && e.Notify && strings.HasPrefix(e.Message, "the check's module now reports each finding as an incident of its own"):
				retired++
			case e.Type == "failed" && e.Finding == checksandalerts.PackageSecurity && e.Notify && e.Incident != announced:
				opened++
			default:
				t.Fatalf("an alert other than the retirement and the opening: %+v", e)
			}
		}
		if want := map[bool]int{true: 1, false: 0}[failing]; retired != want || opened != want {
			t.Fatalf("failing %t: %d retirements of the 0.1.3 incident and %d openings of %s, want %d of each", failing, retired, opened, checksandalerts.PackageSecurity, want)
		}
	}
}
