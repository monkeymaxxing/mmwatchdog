package store_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
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
	t.Run("joined node", func(t *testing.T) {
		t.Parallel()
		dir := format20Database(t, `
 INSERT INTO incidents_sent_without_the_hub VALUES('pager','i-1','app','http','','failed','warn,sos',1);
 INSERT INTO outbox(at,queued_at,destination,event,target) VALUES(1,1,'hub',?,'{}')`, sosFailure)
		s := openMigrated(t, dir)
		records, e := s.SentWithoutTheHub()
		if e != nil || len(records) != 1 || records[0].Incident != "i-1" || len(records[0].AlertSeveritiesSentForIncident) != 2 {
			t.Fatal("the record of a direct send is lost", records, e)
		}
		if ds, e := s.Pending(); e != nil || len(ds) != 1 || ds[0].Destination != "hub" {
			t.Fatal("the alert for the hub is lost", ds, e)
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
