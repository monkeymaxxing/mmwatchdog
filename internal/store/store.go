// Package store keeps a node's state in its SQLite database: the snapshot of
// its checks, the event history, the outbox of alerts and its order, the
// alerts that a hub receives from its nodes (the intake) and their
// grouping, the fleet incident set of each destination that keeps
// incidents, what MMWatchdog has sent to MMHotline, and the measurement history in
// its own file. Store.Save commits a snapshot with its events and queues
// their alerts in one transaction. The worker that delivers the outbox and
// the notices are here too.
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
)

// Retention is how long MMWatchdog keeps what it remembers for a while only: the
// resolved incidents of the fleet incident sets. The stores
// that grow with a node's own work, the event history and the outbox,
// follow the node's storage settings instead; see StorageLimits.
const Retention = 24 * time.Hour

// stateFormat is the persisted layout version. Change it with the layout,
// and migrate the previous format in place (migration.go).
const stateFormat = 21

// stateTables is the layout of state.db in stateFormat.
//
// Grouping by exact finding on the hub (findinggrouping.go) keeps:
// fleet_node_incidents, one row for each open incident of a node that the
// hub knows, with the set-aside entry that covers it, if any, the
// destinations that its check names (setaside.go), and the sequence of the
// exact finding that an opening after a set aside named
// (exact_finding_sequence_named, -1 for none); in fleet_incident_set, for every destination, the membership
// row of each node incident, marked grouped_by_exact_finding, and its
// pending_change for the next finding_nodes_changed update; in
// fleet_direct_sends_settled, the direct sends of nodes that the hub has
// settled, each with the version of the node's record that it settled
// (directSendVersion); and in fleet_incident_keys, what has been
// announced to the destination for the key, and when the key's next update
// and reminder are due. The outbox keeps, for its order (Pending), each
// delivery's key, whether it is urgent (sos) or closes its key, and the key
// whose announcement it carries. The intake keeps whether an alert has an
// exact finding, and intake_queued_bytes the bytes that wait, per node.
const stateTables = `
 CREATE TABLE IF NOT EXISTS snapshot (id INTEGER PRIMARY KEY CHECK(id=1), data BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS checks (key TEXT PRIMARY KEY, data BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS events (seq INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, data BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS outbox (seq INTEGER PRIMARY KEY, at INTEGER NOT NULL, queued_at INTEGER NOT NULL, destination TEXT NOT NULL, event BLOB NOT NULL, target BLOB NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, next INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '',
  urgent INTEGER NOT NULL DEFAULT 0, incident_key TEXT NOT NULL DEFAULT '', closes INTEGER NOT NULL DEFAULT 0, announced_key TEXT NOT NULL DEFAULT '');
 CREATE INDEX IF NOT EXISTS outbox_dest ON outbox(destination,seq);
 CREATE INDEX IF NOT EXISTS outbox_urgent ON outbox(destination,urgent,seq);
 CREATE INDEX IF NOT EXISTS outbox_key ON outbox(destination,incident_key,seq);
 CREATE TABLE IF NOT EXISTS counters (name TEXT PRIMARY KEY, value INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS destination_health (destination TEXT PRIMARY KEY, data BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS history (id INTEGER PRIMARY KEY CHECK(id=1), history_id TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS intake (seq INTEGER PRIMARY KEY AUTOINCREMENT, node TEXT NOT NULL, id TEXT NOT NULL, received INTEGER NOT NULL, type TEXT NOT NULL, subject TEXT NOT NULL, grp TEXT NOT NULL, destination TEXT NOT NULL, severity TEXT NOT NULL, event BLOB NOT NULL, state INTEGER NOT NULL DEFAULT 0,
  exact_finding INTEGER NOT NULL DEFAULT 0, UNIQUE(node,id));
 CREATE TABLE IF NOT EXISTS intake_queued_bytes (node TEXT PRIMARY KEY, bytes INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS fleet_incident_set (destination TEXT NOT NULL, alert_source TEXT NOT NULL, reporter TEXT NOT NULL, incident_id TEXT NOT NULL, incident_key TEXT NOT NULL, check_group TEXT NOT NULL, check_name TEXT NOT NULL, finding TEXT NOT NULL, node TEXT NOT NULL, opened INTEGER NOT NULL, alert_severity TEXT NOT NULL, state INTEGER NOT NULL, changed INTEGER NOT NULL,
  pending_change TEXT NOT NULL DEFAULT '', grouped_by_exact_finding INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(destination,alert_source,reporter,incident_id));
 CREATE INDEX IF NOT EXISTS fleet_incident_set_not_grouped ON fleet_incident_set(destination,alert_source) WHERE grouped_by_exact_finding=0;
 CREATE INDEX IF NOT EXISTS fleet_incident_set_key_state ON fleet_incident_set(destination,alert_source,incident_key,state,alert_severity);
 CREATE INDEX IF NOT EXISTS fleet_incident_set_state ON fleet_incident_set(destination,alert_source,state,incident_key);
 CREATE INDEX IF NOT EXISTS fleet_incident_set_pending ON fleet_incident_set(destination,alert_source,incident_key,pending_change);
 CREATE TABLE IF NOT EXISTS incidents_sent_without_the_hub (destination TEXT NOT NULL, incident_id TEXT NOT NULL, check_group TEXT NOT NULL, check_name TEXT NOT NULL, finding TEXT NOT NULL, kind TEXT NOT NULL, alert_severities_sent_for_incident TEXT NOT NULL, at INTEGER NOT NULL,
  exact_finding TEXT NOT NULL DEFAULT '', exact_finding_sequence INTEGER NOT NULL DEFAULT 0, alert_source TEXT NOT NULL DEFAULT '', PRIMARY KEY(destination,incident_id));
 CREATE TABLE IF NOT EXISTS fleet_incident_keys (destination TEXT NOT NULL, alert_source TEXT NOT NULL, incident_key TEXT NOT NULL, alert_severities_sent TEXT NOT NULL, last_closing_alert_at INTEGER NOT NULL,
  announced_alert_severity TEXT NOT NULL DEFAULT '', announced_exact_findings TEXT NOT NULL DEFAULT '[]', next_update_due INTEGER NOT NULL DEFAULT 0, next_reminder_due INTEGER NOT NULL DEFAULT 0, mmhotline_alert_sources_sent TEXT NOT NULL DEFAULT '', PRIMARY KEY(destination,alert_source,incident_key));
 CREATE INDEX IF NOT EXISTS fleet_incident_keys_update ON fleet_incident_keys(next_update_due);
 CREATE INDEX IF NOT EXISTS fleet_incident_keys_reminder ON fleet_incident_keys(next_reminder_due);
 CREATE TABLE IF NOT EXISTS fleet_node_incidents (reporter TEXT NOT NULL, incident_id TEXT NOT NULL, incident_key TEXT NOT NULL, node TEXT NOT NULL, check_group TEXT NOT NULL, check_name TEXT NOT NULL, finding TEXT NOT NULL,
  exact_finding TEXT NOT NULL, exact_finding_sequence INTEGER NOT NULL, alert_severity TEXT NOT NULL, opened INTEGER NOT NULL, reminder_eligible INTEGER NOT NULL, set_aside_entry TEXT NOT NULL DEFAULT '', set_aside_reason TEXT NOT NULL DEFAULT '', send_alerts_only_to_destinations TEXT NOT NULL DEFAULT '',
  exact_finding_sequence_named INTEGER NOT NULL DEFAULT -1, PRIMARY KEY(reporter,incident_id));
 CREATE INDEX IF NOT EXISTS fleet_node_incidents_key ON fleet_node_incidents(incident_key,exact_finding);
 CREATE TABLE IF NOT EXISTS fleet_direct_sends_settled (destination TEXT NOT NULL, reporter TEXT NOT NULL, incident_id TEXT NOT NULL, settled INTEGER NOT NULL, settled_version TEXT NOT NULL DEFAULT '', PRIMARY KEY(destination,reporter,incident_id));
 CREATE TABLE IF NOT EXISTS incident_alert_destinations (incident_id TEXT NOT NULL, destination TEXT NOT NULL, at INTEGER NOT NULL, PRIMARY KEY(incident_id,destination));
`

type Store struct {
	DB   *sql.DB
	Node string
	// FleetIdentity is the hub certificate fingerprint, empty on a standalone node.
	// The daemon sets it under its lock when the hub or join code activates.
	FleetIdentity string
	// History identifies this database's event sequence, so a hub can tell a
	// new database from an old one whose sequence numbers it already holds.
	History string
	// written holds a hash of each check row as last committed, so a
	// commit writes only the checks that changed. revs holds each fleet
	// check's rev as last committed, so a commit does not even encode the
	// unchanged ones. The daemon lock serializes Save.
	written map[string][32]byte
	revs    map[string]uint64
	// Saved is when the loaded state was last committed: the last time the
	// previous daemon recorded state.
	Saved time.Time
	// Limits bound the event history, the outbox, and the hub's alert
	// intake. A reload replaces them while deliveries prune, so they are
	// atomic.
	Limits atomic.Pointer[config.StorageLimits]
	// RetentionNow is the time that the age limits of a commit count back
	// from. The daemon sets it to its retention clock (Daemon.retentionNow),
	// which a forward step of the system clock does not move ahead. Without
	// it, a commit counts back from the wall clock.
	RetentionNow func() time.Time
	// NewlyAffectedNodesEvery is, on the hub, the shared tick of the updates
	// of keys grouped by exact finding, in nanoseconds:
	// fleet_hub_role.send_newly_affected_nodes_every, which a reload
	// replaces. Zero is config.DefaultSendNewlyAffectedNodesEvery.
	NewlyAffectedNodesEvery atomic.Int64
	// setAside are, on the hub, the entries of set_aside_findings.yml in
	// force. SettleSetAsideFindings alone replaces them.
	setAside atomic.Pointer[config.SetAsideFindings]
}

func (s *Store) setAsideFindings() config.SetAsideFindings {
	if p := s.setAside.Load(); p != nil {
		return *p
	}
	return nil
}

func (s *Store) newlyAffectedNodesEvery() time.Duration {
	if every := s.NewlyAffectedNodesEvery.Load(); every > 0 {
		return time.Duration(every)
	}
	return config.DefaultSendNewlyAffectedNodesEvery.Value()
}

// retentionNow is the time that the age limits of a commit count back from.
func (s *Store) retentionNow() time.Time {
	if s.RetentionNow == nil {
		return time.Now()
	}
	return s.RetentionNow()
}

type Delivery struct {
	Seq         int64
	Event       checksandalerts.Event
	Destination string
	// Target is the destination as it was when the delivery was queued; see
	// SameDestination.
	Target    checksandalerts.Destination
	Attempts  int
	Next      time.Time
	LastError string
}

// storedDestination is private state. Public destination JSON never contains
// a credential fingerprint, which could reveal guesses of a weak password.
type storedDestination struct {
	Destination                  checksandalerts.Destination `json:"destination"`
	CredentialContentFingerprint string                      `json:"credential_content_fingerprint,omitempty"`
}

func destinationForStorage(d checksandalerts.Destination) storedDestination {
	return storedDestination{d, d.CredentialContentFingerprint}
}

func (d storedDestination) destination() checksandalerts.Destination {
	d.Destination.CredentialContentFingerprint = d.CredentialContentFingerprint
	return d.Destination
}

// Production runs as root; tests may use their own UID. Check before opening
// state or the daemon lock, so checks running as another account cannot own it.
func PrepareStateDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	uid := uint32(os.Geteuid())
	if !info.IsDir() || info.Sys().(*syscall.Stat_t).Uid != uid || info.Mode().Perm() != 0700 {
		return fmt.Errorf("state directory %s must be a directory, not a symbolic link, owned by user ID %d with permissions 0700", dir, uid)
	}
	return nil
}

// hostName reads the machine's host name; a test replaces it.
var hostName = os.Hostname

// OpenStore opens the node's state. MMWatchdog names the node by its host name
// in incident keys, so a machine without one cannot run.
func OpenStore(dir string, limits config.StorageLimits) (*Store, error) {
	node, hostNameError := hostName()
	if hostNameError != nil || node == "" {
		reason := ""
		if hostNameError != nil {
			reason = " (" + hostNameError.Error() + ")"
		}
		return nil, fmt.Errorf("MMWatchdog cannot run: this machine has no host name%s, and MMWatchdog names the node by it: set a host name with hostnamectl set-hostname NAME", reason)
	}
	if err := PrepareStateDir(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "state.db")
	if e := migrateStateDatabase(path); e != nil {
		return nil, e
	}
	db, e := OpenDatabase(path, "state", stateFormat, StateDatabaseLimit(limits.StateDatabaseBytes), stateTables)
	if e != nil {
		return nil, e
	}
	var history string
	if _, e = db.Exec("INSERT OR IGNORE INTO history(id,history_id) VALUES(1,?)", incidents.ID()); e == nil {
		e = db.QueryRow("SELECT history_id FROM history WHERE id=1").Scan(&history)
	}
	if e == nil {
		// A database from before the times of drops were kept takes the
		// newest one from the event history, while that still holds it.
		_, e = db.Exec(`INSERT OR IGNORE INTO counters(name,value) SELECT 'newest_delivery_gap_at', MAX(at) FROM events
 WHERE json_extract(data,'$.type')='delivery_gap' AND EXISTS (SELECT 1 FROM counters WHERE name='delivery_gaps' AND value>0) HAVING MAX(at) IS NOT NULL`)
	}
	if e != nil {
		db.Close()
		return nil, e
	}
	s := &Store{DB: db, Node: node, History: history, written: map[string][32]byte{}}
	s.Limits.Store(&limits)
	return s, nil
}

// SetLimits applies the storage limits of a reloaded configuration. The
// size of the database file is applied when the store opens, so a reload
// does not change it.
func (s *Store) SetLimits(limits config.StorageLimits) { s.Limits.Store(&limits) }

// databaseLimit is the size a database file may reach: times what its
// setting states.
type databaseLimit struct {
	setting string
	stated  int64
	times   int64
}

// StateDatabaseLimit lets state.db hold what state_database_size_limit
// states.
func StateDatabaseLimit(stated int64) databaseLimit {
	return databaseLimit{"state_database_size_limit", stated, 1}
}

// FleetDatabaseLimit lets fleet.db hold twice what
// event_history_of_nodes_content_size_limit states: the events take at most the
// stated size, and the rest holds each node's latest report and the free
// pages SQLite needs to rewrite them.
func FleetDatabaseLimit(stated int64) databaseLimit {
	return databaseLimit{"fleet_hub_role.event_history_of_nodes_content_size_limit", stated, 2}
}

func (l databaseLimit) maxBytes() int64 { return l.stated * l.times }

// tooLarge is the error for a database file at path that already holds
// bytes, more than the limit allows. SQLite never shrinks a file on its
// own, so the setting must state a size that fits the file.
func (l databaseLimit) tooLarge(path string, bytes int64) error {
	const mib = 1024 * 1024
	allows := l.setting
	if l.times == 2 {
		allows = "twice " + l.setting
	}
	return fmt.Errorf("%s already holds %d MiB, more than %s allows (%d MiB); state at least %dMiB", path, (bytes+mib-1)/mib, allows, l.maxBytes()/mib, (bytes+l.times*mib-1)/(l.times*mib))
}

// SizeError checks, without opening it, that an existing database file at
// path fits the limit. Runtime validation uses it, so a lowered limit is
// refused before the daemon restarts with it.
func (l databaseLimit) SizeError(path string) error {
	info, e := os.Stat(path)
	if e != nil || info.Size() <= l.maxBytes() {
		return nil
	}
	return l.tooLarge(path, info.Size())
}

// OpenDatabase opens a private SQLite database, refusing one written in
// another format rather than load part of it, and applies its tables and
// size limit. The caller has checked the state directory.
func OpenDatabase(path, kind string, format int, limit databaseLimit, tables string) (*sql.DB, error) {
	db, e := OpenDatabaseFile(path, kind, format, "", tables)
	if e != nil {
		return nil, e
	}
	maxBytes := limit.maxBytes()
	var pageSize, maxPages int64
	if e = db.QueryRow("PRAGMA page_size").Scan(&pageSize); e == nil {
		e = db.QueryRow(fmt.Sprintf("PRAGMA max_page_count=%d", maxBytes/pageSize)).Scan(&maxPages)
	}
	if e != nil {
		db.Close()
		return nil, e
	}
	if maxPages*pageSize > maxBytes {
		db.Close()
		return nil, limit.tooLarge(path, maxPages*pageSize)
	}
	return db, nil
}

// OpenDatabaseFile opens a private SQLite database, refusing one written in
// another format rather than load part of it, and applies its tables. first
// holds the statements that SQLite accepts only before anything else is
// written to a new file. It sets no size limit: OpenDatabase does, and the
// measurement history keeps its own (measurementhistory.go).
func OpenDatabaseFile(path, kind string, format int, first, tables string) (*sql.DB, error) {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	_ = f.Close() // This handle only creates the database file; SQLite performs and checks all content writes separately.
	if e = os.Chmod(path, 0600); e != nil {
		return nil, e
	}
	db, e := sql.Open("sqlite", path)
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	var version, count int
	if e = db.QueryRow("PRAGMA user_version").Scan(&version); e == nil {
		e = db.QueryRow("SELECT count(*) FROM sqlite_master").Scan(&count)
	}
	if e == nil && version != format && (version != 0 || count != 0) {
		e = fmt.Errorf("%s uses %s format %d; the running MMWatchdog build uses format %d. Move the database file aside to start with empty state", path, kind, version, format)
	}
	if e != nil {
		db.Close()
		return nil, e
	}
	schema := first + `PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000; PRAGMA wal_autocheckpoint=256; PRAGMA journal_size_limit=1048576;` +
		tables + `
 PRAGMA user_version=` + strconv.Itoa(format) + `;`
	if _, e = db.Exec(schema); e != nil {
		db.Close()
		return nil, e
	}
	return db, nil
}
func (s *Store) Close() error { return s.DB.Close() }

// The snapshot row holds budgets, maintenance, the boot ID, and when it was
// committed; each check has its own row. Every commit writes it.
type snapshotRow struct {
	RecoveryAttemptsByTarget map[string]*incidents.RecoveryAttempts `json:"recovery_attempts_by_target"`
	Maintenance              map[string]checksandalerts.Window      `json:"maintenance"`
	Boot                     string                                 `json:"boot,omitempty"`
	Machine                  string                                 `json:"machine,omitempty"`
	Saved                    time.Time                              `json:"saved"`
}

func (s *Store) Load() (incidents.Snapshot, error) {
	snap := incidents.FreshSnapshot()
	var b []byte
	e := s.DB.QueryRow("SELECT data FROM snapshot WHERE id=1").Scan(&b)
	if e == sql.ErrNoRows {
		return snap, nil
	}
	if e != nil {
		return snap, e
	}
	var row snapshotRow
	if e = json.Unmarshal(b, &row); e != nil {
		return snap, e
	}
	if row.RecoveryAttemptsByTarget == nil || row.Maintenance == nil {
		return snap, fmt.Errorf("saved monitoring state is missing recovery attempt budgets or maintenance windows")
	}
	snap.RecoveryAttemptsByTarget, snap.Maintenance, snap.Boot, snap.Machine = row.RecoveryAttemptsByTarget, row.Maintenance, row.Boot, row.Machine
	s.Saved = row.Saved
	rows, e := s.DB.Query("SELECT key, data FROM checks")
	if e != nil {
		return snap, e
	}
	defer func() {
		_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	}()
	written, revs := map[string][32]byte{}, map[string]uint64{}
	for rows.Next() {
		var key string
		st := &incidents.CheckState{}
		if e = rows.Scan(&key, &b); e != nil {
			return snap, e
		}
		if e = json.Unmarshal(b, st); e != nil {
			return snap, e
		}
		snap.Checks[key] = st
		written[key] = sha256.Sum256(b)
		if st.CheckGroup == checksandalerts.NodeReportsCheckGroup {
			revs[key] = st.Rev
		}
	}
	if e = rows.Err(); e != nil {
		return snap, e
	}
	s.written, s.revs = written, revs
	return snap, nil
}

// AlertRoute is where a commit's notifying events go. A solo node queues
// them for each destination, and a reminder for the destination it names. A
// joined node queues them for the hub alone; its destinations are its copy
// of the fleet settings, for fallback and reminders. The hub puts them
// through its alert grouping step. Every commit cancels queued deliveries for any
// destination the route does not list.
type AlertRoute struct {
	// DestinationHealth is supplied by the daemon's record/control paths.
	// nil leaves it untouched for callers that only exercise alert routing.
	DestinationHealth map[string]DestinationHealth

	Destinations                           map[string]checksandalerts.Destination
	Joined, Hub                            bool
	DestinationsWhoseServiceKeepsIncidents []string
}

// Known lists every destination whose queued deliveries stay valid.
func (r AlertRoute) Known() map[string]checksandalerts.Destination {
	if !r.Joined {
		return r.Destinations
	}
	known := map[string]checksandalerts.Destination{config.HubDestination: {}}
	for name, w := range r.Destinations {
		known[name] = w
	}
	return known
}

// Save commits the snapshot, events and their deliveries in one transaction.
// It writes only the check rows whose content changed since the last commit.
func (s *Store) Save(snap incidents.Snapshot, events []checksandalerts.Event, route AlertRoute) error {
	b, e := json.Marshal(snapshotRow{snap.RecoveryAttemptsByTarget, snap.Maintenance, snap.Boot, snap.Machine, time.Now().UTC()})
	if e != nil {
		return e
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("INSERT INTO snapshot(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", b); e != nil {
		return e
	}
	written, revs := map[string][32]byte{}, map[string]uint64{}
	for key, st := range snap.Checks {
		if st.CheckGroup == checksandalerts.NodeReportsCheckGroup {
			if rev, ok := s.revs[key]; ok && rev == st.Rev {
				written[key], revs[key] = s.written[key], rev
				continue
			}
			revs[key] = st.Rev
			// Hub member receipts stay in fleet.db; the hub-outage alerter keeps its
			// receipt here. Daemon start sets a fresh deadline for both.
			persisted := *st
			persisted.Deadline = time.Time{}
			if !route.Joined || st.Check != checksandalerts.HubCheck {
				persisted.Receipt = time.Time{}
			}
			st = &persisted
		}
		if b, e = json.Marshal(st); e != nil {
			return e
		}
		written[key] = sha256.Sum256(b)
		if previous, ok := s.written[key]; ok && previous == written[key] {
			continue
		}
		if _, e = tx.Exec("INSERT INTO checks(key,data) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET data=excluded.data", key, b); e != nil {
			return e
		}
	}
	for key := range s.written {
		if _, exists := written[key]; !exists {
			if _, e = tx.Exec("DELETE FROM checks WHERE key=?", key); e != nil {
				return e
			}
		}
	}
	// Every commit cancels queued deliveries whose destination identity is no
	// longer configured, so activation and cancellation are always atomic.
	cancelled, e := CancelDeliveries(tx, route.Known())
	if e != nil {
		return e
	}
	// A fleet incident set belongs to its destination, while the
	// destination keeps incidents or for keys grouped by exact finding, and
	// the mmhotline destination's to one alert source: anything else starts
	// the set over.
	keeping := route.Destinations
	if route.Joined {
		keeping = nil
	}
	if e = keepOnlyIncidentSets(tx, keeping); e != nil {
		return e
	}
	if cancelled > 0 {
		// This is an intentional cancellation, distinct from retention loss.
		events = append(append([]checksandalerts.Event{}, events...), checksandalerts.Event{ID: incidents.ID(), Type: "delivery_cancelled", At: time.Now().UTC(), Message: fmt.Sprintf("%d notification deliveries cancelled because their destination was removed or changed", cancelled)})
	}
	for _, event := range events {
		event = s.PreparedEvent(event, route)
		b, e = json.Marshal(event)
		if e != nil {
			return e
		}
		if _, e = tx.Exec("INSERT INTO events(at,data) VALUES(?,?)", event.At.UnixNano(), b); e != nil {
			return e
		}
		switch {
		case !event.Notify:
		case event.DeliverOnThisNode:
			if e = QueueDirect(tx, event.At.UnixNano(), event, []checksandalerts.Event{event}, route.Destinations, QueuedBy{Role: StandaloneSender, Node: s.Node}); e != nil {
				return e
			}
		case route.Hub:
			if e = queueIntake(tx, s.Node, event, b, time.Now()); e != nil {
				return e
			}
		case route.Joined && event.CheckGroup == checksandalerts.NodeReportsCheckGroup && event.Check == checksandalerts.HubCheck:
			// The hub-outage alerter already waited for the hub's silence deadline.
			// Sending this alert through the unreachable hub would delay it twice.
			if e = QueueDirect(tx, event.At.UnixNano(), event, []checksandalerts.Event{event}, route.Destinations, QueuedBy{Role: FallbackSender, Node: s.Node, FleetIdentity: s.FleetIdentity, DestinationsWhoseServiceKeepsIncidents: route.DestinationsWhoseServiceKeepsIncidents}); e != nil {
				return e
			}
		case route.Joined:
			if e = replaceQueuedReminder(tx, config.HubDestination, event); e != nil {
				return e
			}
			if _, e = tx.Exec("INSERT INTO outbox(at,queued_at,destination,event,target) VALUES(?,?,?,?,'{}')", event.At.UnixNano(), time.Now().UnixNano(), config.HubDestination, b); e != nil {
				return e
			}
		default:
			if e = QueueDirect(tx, event.At.UnixNano(), event, []checksandalerts.Event{event}, route.Destinations, QueuedBy{Role: StandaloneSender, Node: s.Node, FleetIdentity: s.FleetIdentity}); e != nil {
				return e
			}
		}
	}
	// Only added events and deliveries can exceed the byte limits. Age
	// expiry runs separately through Prune.
	if len(events) > 0 {
		if e = s.PruneInTransaction(tx, s.retentionNow()); e != nil {
			return e
		}
	}
	if e = saveDestinationHealth(tx, route.DestinationHealth); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	s.written, s.revs = written, revs
	return nil
}

// QueueDirect queues an alert message for each destination it goes to: the
// one a reminder names, when it is configured, or every destination, each
// only when it takes the message (receives). members are the alerts the
// message stands for, and from is who sends it. A destination that keeps a
// fleet incident set gets the message through its set: a mmhotline
// destination as MMHotline's alerts (QueueMMHotline), and a destination
// whose service keeps incidents of its own with one closing alert for each
// key of the set (queueWithIncidentSet). On the hub, the message of a
// finding with an exact finding goes to every destination through the
// destination's set (queueGrouped, findinggrouping.go).
func QueueDirect(tx *sql.Tx, at int64, message checksandalerts.Event, members []checksandalerts.Event, destinations map[string]checksandalerts.Destination, from QueuedBy) error {
	grouped := false
	if from.Role == FleetHubSender {
		kept, isGrouped, coverage, e := nodeIncidentsOfMessage(tx, message, members, from)
		if e == nil {
			e = applyCoverageChanges(tx, at, coverage, destinations, from, from.now())
		}
		if e != nil || len(kept) == 0 {
			return e
		}
		if len(kept) != len(members) {
			message = onlyStillOpen(message, kept, members, from.hub())
		}
		members, grouped = kept, isGrouped
	}
	for _, name := range checksandalerts.Names(destinations) {
		if message.Destination != "" && name != message.Destination {
			continue
		}
		dest := destinations[name]
		if ok, e := Receives(tx, name, dest, message, members, destinations); e != nil || !ok {
			if e != nil {
				return e
			}
			continue
		}
		var e error
		switch {
		case grouped:
			e = queueGrouped(tx, at, name, dest, message, members, from, from.now())
		case dest.MMHotline() != nil:
			e = QueueMMHotline(tx, at, name, dest, message, members, from, from.now())
		case from.keepsIncidents(name, dest):
			e = queueWithIncidentSet(tx, at, name, dest, message, members, from, from.now())
		case from.Role == FallbackSender:
			e = queueFallbackForExactFinding(tx, at, name, dest, message, members, from.now())
		default:
			e = queueEvent(tx, at, name, dest, message)
		}
		if e != nil {
			return e
		}
	}
	return RememberIncidentDestinations(tx, at, message, members, destinations)
}

// queueFallbackForExactFinding queues a joined node's direct alert for a
// destination whose service keeps no incidents, such as a chat or a
// mailbox. For a finding with an exact finding the node keeps the record
// of the direct send, as for a destination that keeps incidents, and
// sends no alert that closes it: the hub settles the record and sends the
// closing alert for its key. Other alerts go as they are.
func queueFallbackForExactFinding(tx *sql.Tx, at int64, name string, dest checksandalerts.Destination, message checksandalerts.Event, members []checksandalerts.Event, now time.Time) error {
	switch {
	case opensOrChanges(message.Type) && checksandalerts.HasExactFinding(message):
		if e := recordSentWithoutTheHub(tx, name, members, "", now); e != nil {
			return e
		}
	case checksandalerts.Closes(message.Type):
		var recorded bool
		if e := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM incidents_sent_without_the_hub WHERE destination=? AND incident_id=? AND exact_finding!='')", name, message.Incident).Scan(&recorded); e != nil || recorded {
			return e
		}
	}
	return queueEvent(tx, at, name, dest, message)
}

// queueEvent queues one alert for the destination name, as the destination
// takes it: without the body for one that states send_only_the_alert_title.
// A reminder replaces the undelivered ones that it supersedes.
func queueEvent(tx *sql.Tx, at int64, name string, dest checksandalerts.Destination, alert checksandalerts.Event) error {
	if e := replaceQueuedReminder(tx, name, alert); e != nil {
		return e
	}
	if dest.SendOnlyTheAlertTitle {
		alert = alert.OnlyTheTitle()
	}
	target, _ := json.Marshal(destinationForStorage(dest))
	return insertOutbox(tx, at, name, alert, target, "")
}

// replaceQueuedReminder removes from one queue of the outbox the undelivered
// reminders that the reminder message supersedes: those for the same
// incident and the same destination, or for a hub's combined reminder those
// for the same check, the same failed systemd unit if it is about one, and
// the same destination. However short remind_every is and
// however long a destination is unreachable, reminders therefore cannot fill
// the outbox and push an alert out of it.
func replaceQueuedReminder(tx *sql.Tx, queue string, message checksandalerts.Event) error {
	if message.Type != "reminder" {
		return nil
	}
	_, e := tx.Exec(`DELETE FROM outbox WHERE destination=? AND json_extract(event,'$.type')='reminder'
 AND COALESCE(json_extract(event,'$.incident_id'),'')=? AND COALESCE(json_extract(event,'$.destination'),'')=?
 AND COALESCE(json_extract(event,'$.check_group'),'')=? AND json_extract(event,'$.check')=? AND COALESCE(json_extract(event,'$.finding'),'')=?`, queue, message.Incident, message.Destination, message.CheckGroup, message.Check, message.Finding)
	return e
}

// CancelDeliveries removes queued deliveries whose destination no longer
// matches a configured one under SameDestination, and returns how many.
func CancelDeliveries(tx *sql.Tx, destinations map[string]checksandalerts.Destination) (int64, error) {
	var cancelled int64
	rows, e := tx.Query("SELECT DISTINCT destination, target FROM outbox")
	if e != nil {
		return 0, e
	}
	type queued struct {
		name   string
		target []byte
	}
	stale := []queued{}
	for rows.Next() {
		var q queued
		var target storedDestination
		if e = rows.Scan(&q.name, &q.target); e != nil {
			_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
			return 0, e
		}
		if e = json.Unmarshal(q.target, &target); e != nil {
			_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
			return 0, e
		}
		if current, exists := destinations[q.name]; !exists || !checksandalerts.SameDestination(target.destination(), current) {
			stale = append(stale, q)
		}
	}
	e = rows.Err()
	_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	if e != nil {
		return 0, e
	}
	for _, q := range stale {
		if e = forgetUndeliveredAnnouncements(tx, "destination=? AND target=?", q.name, q.target); e != nil {
			return 0, e
		}
		r, e := tx.Exec("DELETE FROM outbox WHERE destination=? AND target=?", q.name, q.target)
		if e != nil {
			return 0, e
		}
		n, _ := r.RowsAffected()
		cancelled += n
	}
	return cancelled, nil
}

func (s *Store) PruneInTransaction(tx *sql.Tx, now time.Time) error {
	limits := s.Limits.Load()
	// Retain the newest deliveries within the age and encoded-byte limits.
	expired := `at < ? OR seq IN (
  SELECT seq FROM (SELECT seq, SUM(length(event)+length(target)) OVER (ORDER BY seq DESC) AS bytes FROM outbox)
  WHERE bytes > ?)`
	if e := forgetUndeliveredAnnouncements(tx, expired, now.Add(-limits.KeepUndeliveredAlertsFor).UnixNano(), limits.UndeliveredAlertsBytes); e != nil {
		return e
	}
	r, e := tx.Exec(`DELETE FROM outbox WHERE `+expired, now.Add(-limits.KeepUndeliveredAlertsFor).UnixNano(), limits.UndeliveredAlertsBytes)
	if e != nil {
		return e
	}
	lost, _ := r.RowsAffected()
	if lost > 0 {
		// now is the retention clock, which a forward step of the system
		// clock leaves behind; the event says when the gap happened.
		at := time.Now().UTC()
		event := checksandalerts.Event{ID: incidents.ID(), Node: s.Node, Type: "delivery_gap", At: at, Message: fmt.Sprintf("%d alert deliveries were dropped: they waited longer than keep_undelivered_alerts_for, or the queue of undelivered alerts grew past undelivered_alerts_content_size_limit", lost)}
		b, _ := json.Marshal(event)
		if _, e = tx.Exec("INSERT INTO events(at,data) VALUES(?,?)", at.UnixNano(), b); e != nil {
			return e
		}
		if e = addDeliveryGaps(tx, lost, at); e != nil {
			return e
		}
	}
	// A resolved incident of a fleet incident set is remembered for a day
	// at most, and so is the last closing alert of a key that has nothing
	// open.
	// A key keeps what it announced, and its due update and reminder.
	if _, e = tx.Exec("DELETE FROM fleet_incident_keys WHERE alert_severities_sent='' AND announced_alert_severity='' AND next_update_due=0 AND next_reminder_due=0 AND last_closing_alert_at < ?", now.Add(-Retention).UnixNano()); e != nil {
		return e
	}
	if _, e = tx.Exec("DELETE FROM fleet_incident_set WHERE state=? AND changed < ?", fleetIncidentResolved, now.Add(-Retention).UnixNano()); e != nil {
		return e
	}
	// A node stops carrying a settled direct send at its next report.
	if _, e = tx.Exec("DELETE FROM fleet_direct_sends_settled WHERE settled < ?", now.Add(-Retention).UnixNano()); e != nil {
		return e
	}
	// Delivered alerts keep only their IDs, so a retry is not accepted twice.
	if _, e = tx.Exec("DELETE FROM intake WHERE state=? AND received < ?", intakeDone, now.Add(-intakeMemory).UnixNano()); e != nil {
		return e
	}
	if _, e = tx.Exec(`DELETE FROM events WHERE at < ? OR seq IN (
 SELECT seq FROM (SELECT seq, SUM(length(data)) OVER (ORDER BY seq DESC) AS bytes FROM events) WHERE bytes > ?)`, now.Add(-limits.KeepEventHistoryFor).UnixNano(), limits.EventHistoryBytes); e != nil {
		return e
	}
	return nil
}

// addDeliveryGaps counts n deliveries dropped at at, and keeps when the
// first and the newest drop happened, so the status can say when.
func addDeliveryGaps(tx *sql.Tx, n int64, at time.Time) error {
	_, e := tx.Exec(`INSERT INTO counters(name,value) VALUES('delivery_gaps',?),('first_delivery_gap_at',?),('newest_delivery_gap_at',?)
 ON CONFLICT(name) DO UPDATE SET value=CASE name WHEN 'delivery_gaps' THEN value+excluded.value WHEN 'newest_delivery_gap_at' THEN MAX(value,excluded.value) ELSE value END`, n, at.UnixNano(), at.UnixNano())
	return e
}
func (s *Store) Events(limit int) ([]checksandalerts.Event, error) {
	return s.CheckGroupEvents("", limit)
}
func (s *Store) CheckGroupEvents(check string, limit int) ([]checksandalerts.Event, error) {
	// A maintenance event for several check groups belongs to each of them.
	rows, e := s.DB.Query(`SELECT seq, data FROM events WHERE ?='' OR json_extract(data,'$.check_group')=?
 OR EXISTS (SELECT 1 FROM json_each(data,'$.check_groups') WHERE value=?) ORDER BY seq DESC LIMIT ?`, check, check, check, limit)
	if e != nil {
		return nil, e
	}
	return scanEvents(rows, 0)
}

// CheckGroupEventsInTime returns every retained event in an inclusive span.
// A multi-check-group maintenance event belongs to each named check group.
func (s *Store) CheckGroupEventsInTime(check string, since, until time.Time) ([]checksandalerts.Event, error) {
	if since.After(until) {
		return []checksandalerts.Event{}, nil
	}
	rows, err := s.DB.Query(`SELECT seq, data FROM events WHERE at>=? AND at<=? AND
 (?='' OR json_extract(data,'$.check_group')=? OR EXISTS (SELECT 1 FROM json_each(data,'$.check_groups') WHERE value=?)) ORDER BY seq DESC`, since.UnixNano(), until.UnixNano(), check, check, check)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows, 0)
}

// EventsAfter returns the oldest events after seq, in order, up to limit
// events and about maxBytes of encoded data.
func (s *Store) EventsAfter(seq int64, limit, maxBytes int) ([]checksandalerts.Event, error) {
	rows, e := s.DB.Query("SELECT seq, data FROM events WHERE seq>? ORDER BY seq LIMIT ?", seq, limit)
	if e != nil {
		return nil, e
	}
	return scanEvents(rows, maxBytes)
}

// ContinueEventsAfter makes the sequence numbers of this node's next events
// follow seq, the number of the last event that the hub holds from this event
// history, when the history has given out only lower ones. That is so after
// state.db was restored from an older copy: the hub takes only the events
// after the number that it holds, so without this step it would ignore every
// new event until the node's numbers had passed that number again.
func (s *Store) ContinueEventsAfter(seq int64) error {
	// SQLite keeps the highest number that the table has given out, and has
	// no row for the table before its first event.
	var last sql.NullInt64
	if e := s.DB.QueryRow("SELECT seq FROM sqlite_sequence WHERE name='events'").Scan(&last); e != nil && e != sql.ErrNoRows {
		return e
	}
	// Neither statement lowers the number: an event that is recorded at this
	// moment may have raised it, and a hub that holds less changes nothing.
	if !last.Valid {
		_, e := s.DB.Exec("INSERT INTO sqlite_sequence(name,seq) SELECT 'events',? WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name='events')", seq)
		return e
	}
	_, e := s.DB.Exec("UPDATE sqlite_sequence SET seq=? WHERE name='events' AND seq<?", seq, seq)
	return e
}

// scanEvents reads seq/data rows, stopping before maxBytes when it is positive.
func scanEvents(rows *sql.Rows, maxBytes int) ([]checksandalerts.Event, error) {
	defer func() {
		_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	}()
	es := []checksandalerts.Event{}
	total := 0
	for rows.Next() {
		var b []byte
		var event checksandalerts.Event
		if e := rows.Scan(&event.Seq, &b); e != nil {
			return nil, e
		}
		if total += len(b); maxBytes > 0 && total > maxBytes && len(es) > 0 {
			break
		}
		seq := event.Seq
		if e := json.Unmarshal(b, &event); e != nil {
			return nil, e
		}
		event.Seq = seq
		es = append(es, event)
	}
	return es, rows.Err()
}

// Pending returns the next undelivered alert of each destination, its
// head: one head per destination preserves ordering while allowing
// destinations to proceed independently. The head is the oldest alert,
// unless an sos alert of an incident waits behind alerts below sos: then
// the oldest such sos alert that no earlier closing alert of its own key
// precedes goes first (queueorder.go). Whether a head is due for an
// attempt the notifications worker decides on the monotonic clock
// (Daemon.attemptDue), for the destination as a whole; Next is for display
// and for the first wait after a start.
func (s *Store) Pending() ([]Delivery, error) {
	rows, e := s.DB.Query(`SELECT seq,destination,event,target,attempts,next,error FROM outbox WHERE seq IN (
 SELECT COALESCE((SELECT MIN(u.seq) FROM outbox u WHERE u.destination=o.destination AND u.urgent=1
  AND NOT EXISTS (SELECT 1 FROM outbox c WHERE c.destination=u.destination AND c.incident_key=u.incident_key AND c.closes=1 AND c.seq<u.seq)), MIN(o.seq))
 FROM outbox o GROUP BY o.destination) ORDER BY seq`)
	if e != nil {
		return nil, e
	}
	defer func() {
		_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	}()
	ds := []Delivery{}
	for rows.Next() {
		var d Delivery
		var event, w []byte
		var next int64
		if e = rows.Scan(&d.Seq, &d.Destination, &event, &w, &d.Attempts, &next, &d.LastError); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(event, &d.Event); e != nil {
			return nil, e
		}
		var target storedDestination
		if e = json.Unmarshal(w, &target); e != nil {
			return nil, e
		}
		d.Target = target.destination()
		d.Next = time.Unix(0, next)
		ds = append(ds, d)
	}
	return ds, rows.Err()
}

// Delivered acknowledges a delivery, or records its failed attempt and
// returns the delay of the next one: the time a receiver asked for, or 1
// second doubling to 256. The notifications worker waits that delay on the
// monotonic clock; the outbox keeps the time of the next attempt for
// status and for the first wait after a start. A permanent refusal drops
// the delivery with a visible delivery gap. The delay is 0 when the alert
// leaves the head of its queue.
func (s *Store) Delivered(d Delivery, err error) (time.Duration, error) {
	// Retention can remove an in-flight delivery and SQLite can reuse its sequence.
	// Only acknowledge or reschedule the event that was actually sent.
	if err == nil {
		_, e := s.DB.Exec("DELETE FROM outbox WHERE seq=? AND destination=? AND json_extract(event,'$.id')=?", d.Seq, d.Destination, d.Event.ID)
		return 0, e
	}
	var refused *checksandalerts.MMHotlineRefusal
	if errors.As(err, &refused) {
		return 0, s.refused(d, refused)
	}
	delay := DelayAfterFailedAttempts(d.Attempts + 1)
	var later *checksandalerts.RetryLater
	if errors.As(err, &later) {
		delay = later.After
	}
	msg := err.Error()
	if len(msg) > 2048 {
		msg = msg[:2048]
	}
	_, e := s.DB.Exec("UPDATE outbox SET attempts=attempts+1,next=?,error=? WHERE seq=? AND destination=? AND json_extract(event,'$.id')=?", time.Now().Add(delay).UnixNano(), msg, d.Seq, d.Destination, d.Event.ID)
	return delay, e
}

// DelayAfterFailedAttempts is how long MMWatchdog waits before the next attempt of
// a delivery whose last attempts attempts failed: 1 second, doubling up to
// 256 seconds.
func DelayAfterFailedAttempts(attempts int) time.Duration {
	return time.Second << min(max(attempts-1, 0), 8)
}

// refused drops a delivery the receiver permanently refused, with a durable
// delivery gap and an event that says why, so one bad alert never holds up
// the alerts behind it.
func (s *Store) refused(d Delivery, refused *checksandalerts.MMHotlineRefusal) error {
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = forgetUndeliveredAnnouncements(tx, "seq=? AND destination=? AND json_extract(event,'$.id')=?", d.Seq, d.Destination, d.Event.ID); e != nil {
		return e
	}
	r, e := tx.Exec("DELETE FROM outbox WHERE seq=? AND destination=? AND json_extract(event,'$.id')=?", d.Seq, d.Destination, d.Event.ID)
	if e != nil {
		return e
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return nil
	}
	now := time.Now().UTC()
	event := checksandalerts.Event{ID: incidents.ID(), Node: s.Node, CheckGroup: d.Event.CheckGroup, Check: d.Event.Check, Type: "delivery_gap", At: now,
		Message: fmt.Sprintf("%s: the alert %q (%s) was not delivered. %s", d.Destination, d.Event.Message, d.Event.ID, refused.Error())}
	b, _ := json.Marshal(event)
	if _, e = tx.Exec("INSERT INTO events(at,data) VALUES(?,?)", now.UnixNano(), b); e != nil {
		return e
	}
	if e = addDeliveryGaps(tx, 1, now); e != nil {
		return e
	}
	return tx.Commit()
}

// FallBack moves a joined node's oldest alert for the hub to each
// destination in its copy of the fleet settings, or a reminder to the one it
// names, when it happened after ago and the hub's latest delivery attempt
// failed, or at once while an earlier alert for the same check is queued
// there, so a check's alerts never pass each other. keepIncidents are the
// destinations that the fleet settings say keep incidents. happened returns
// when the oldest alert, the one with seq and eventID that the outbox says
// happened at at, happened on the caller's clock: the daemon's counts on
// the monotonic clock (Daemon.hubAlertHappened). It returns the alert it
// moved, or nil.
func (s *Store) FallBack(destinations map[string]checksandalerts.Destination, keepIncidents []string, after time.Duration, hubFailed bool, now time.Time, happened func(seq int64, eventID string, at time.Time) time.Time) (*FallenBackAlert, error) {
	tx, e := s.DB.Begin()
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	var seq, at int64
	var event []byte
	var eventID string
	e = tx.QueryRow("SELECT seq, at, event, json_extract(event,'$.id') FROM outbox WHERE destination=? ORDER BY seq LIMIT 1", config.HubDestination).Scan(&seq, &at, &event, &eventID)
	if e == sql.ErrNoRows {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var direct int
	if e = tx.QueryRow(`SELECT COUNT(*) FROM outbox WHERE destination!=? AND json_extract(event,'$.check_group')=json_extract(?,'$.check_group')
 AND json_extract(event,'$.check')=json_extract(?,'$.check')`, config.HubDestination, event, event).Scan(&direct); e != nil {
		return nil, e
	}
	if direct == 0 && (!hubFailed || now.Sub(happened(seq, eventID, time.Unix(0, at))) < after) {
		return nil, nil
	}
	if _, e = tx.Exec("DELETE FROM outbox WHERE seq=?", seq); e != nil {
		return nil, e
	}
	moved := FallenBackAlert{FollowsEarlierAlertOfTheSameCheck: direct != 0}
	if e = json.Unmarshal(event, &moved.Alert); e != nil {
		return nil, e
	}
	if e = QueueDirect(tx, at, moved.Alert, []checksandalerts.Event{moved.Alert}, destinations, QueuedBy{Role: FallbackSender, Node: s.Node, FleetIdentity: s.FleetIdentity, DestinationsWhoseServiceKeepsIncidents: keepIncidents}); e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return &moved, nil
}

// FallenBackAlert is an alert that FallBack moved from the hub's queue to
// the destinations, and why: because the latest attempt to deliver an alert
// to the hub failed, or because an earlier alert of the same check is still
// queued there.
type FallenBackAlert struct {
	Alert                             checksandalerts.Event
	FollowsEarlierAlertOfTheSameCheck bool
}

// DeliverySummary is the state of the notification queue. Gaps counts the
// deliveries dropped since state.db was created; FirstGapAt and NewestGapAt
// say when the first and the newest of them were dropped, and are zero when
// that is not known. LastError is the error of the latest failed attempt of
// any undelivered alert.
// FailingDestinations says which destinations are failing; the status and
// the node's report fill it (Daemon.status).
type DeliverySummary struct {
	Pending             int                  `json:"queued_notifications"`
	Gaps                int64                `json:"delivery_gaps"`
	FirstGapAt          time.Time            `json:"first_delivery_gap_at,omitzero"`
	NewestGapAt         time.Time            `json:"newest_delivery_gap_at,omitzero"`
	LastError           string               `json:"last_error"`
	FailingDestinations []FailingDestination `json:"failing_destinations"`
}

// Describe says in one line what mmwatchdog status and mmwatchdog fleet NODE
// show of the notification queue: how many deliveries wait, and how many
// were dropped and when, so a reader can tell an old drop from a new one.
func (d DeliverySummary) Describe() string {
	line := fmt.Sprintf("Alert deliveries: %d waiting, %d dropped", d.Pending, d.Gaps)
	when := func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }
	switch {
	case d.Gaps == 0:
	case d.NewestGapAt.IsZero():
		line += " (the time is not recorded)"
	case d.Gaps == 1 || d.FirstGapAt.Equal(d.NewestGapAt):
		line += " at " + when(d.NewestGapAt)
	case d.FirstGapAt.IsZero():
		line += ", the newest at " + when(d.NewestGapAt)
	default:
		line += " since " + when(d.FirstGapAt) + ", the newest at " + when(d.NewestGapAt)
	}
	return line
}

// FailingDestination is a destination whose oldest undelivered alert has a
// failed attempt. A destination gets its alerts in order, so every later
// alert for it waits behind that one. On a joined node the name hub stands
// for the MMWatchdog fleet hub, which takes the node's alerts first.
type FailingDestination struct {
	Destination                    string    `json:"destination"`
	OldestUndeliveredAlertQueuedAt time.Time `json:"oldest_undelivered_alert_queued_at,omitempty"`
	VerificationFailingSince       time.Time `json:"verification_failing_since,omitempty"`
	// UndeliveredAlerts counts the alerts that wait for the destination.
	UndeliveredAlerts int `json:"undelivered_alerts"`
	// LatestAttemptError is the error of the latest attempt to deliver the
	// oldest of them, on one line.
	LatestAttemptError string `json:"latest_attempt_error"`
	// NextAttemptAt is when MMWatchdog tries to deliver it again.
	NextAttemptAt time.Time `json:"next_attempt_at"`
}

// Describe says in one line what mmwatchdog status and mmwatchdog fleet NODE show of a
// failing destination. The error comes last, because it can be long.
func (f FailingDestination) Describe() string {
	if !f.VerificationFailingSince.IsZero() {
		return "Destination " + checksandalerts.OneLine(f.Destination) + ": verification has failed since " + f.VerificationFailingSince.UTC().Format("15:04 UTC") + ". The latest verification failed: " + checksandalerts.OneLine(f.LatestAttemptError)
	}
	alerts, are := fmt.Sprintf("%d alerts", f.UndeliveredAlerts), "are"
	if f.UndeliveredAlerts == 1 {
		alerts, are = "1 alert", "is"
	}
	waiting := "Destination " + checksandalerts.OneLine(f.Destination) + ": " + alerts + " " + are + " not delivered."
	if f.Destination == config.HubDestination {
		waiting = "The MMWatchdog fleet hub has not accepted " + alerts + "."
	}
	return waiting + " MMWatchdog tries again at " + f.NextAttemptAt.UTC().Format("15:04 UTC") + ". The latest attempt failed: " + checksandalerts.OneLine(f.LatestAttemptError)
}

// ShowsError reports that lastError, the delivery summary's latest error,
// is the error that this destination's line already shows.
func (f FailingDestination) ShowsError(lastError string) bool {
	return checksandalerts.OneLine(lastError) == checksandalerts.OneLine(f.LatestAttemptError)
}

// FailingDestinations lists, by name, every destination whose oldest
// undelivered alert has a failed attempt. It leaves out the destination
// named except: the mmhotline destination, whose problems the status shows
// with the rest of what it says about MMHotline.
func (s *Store) FailingDestinations(except string) ([]FailingDestination, error) {
	queues, err := s.DestinationQueues()
	if err != nil {
		return nil, err
	}
	failing := []FailingDestination{}
	for _, queue := range queues {
		if queue.LatestAttemptError != "" && queue.Destination != except {
			failing = append(failing, queue)
		}
	}
	return failing, nil
}

// DestinationQueues lists each queue's oldest alert, including an alert
// whose first attempt is still running. Health notices measure queue age.
func (s *Store) DestinationQueues() ([]FailingDestination, error) {
	rows, e := s.DB.Query(`SELECT destination, (SELECT COUNT(*) FROM outbox AS waiting WHERE waiting.destination=oldest.destination), error, next, queued_at
 FROM outbox AS oldest WHERE seq IN (SELECT MIN(seq) FROM outbox GROUP BY destination) ORDER BY destination`)
	if e != nil {
		return nil, e
	}
	defer func() {
		_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	}()
	failing := []FailingDestination{}
	for rows.Next() {
		var f FailingDestination
		var next, at int64
		if e = rows.Scan(&f.Destination, &f.UndeliveredAlerts, &f.LatestAttemptError, &next, &at); e != nil {
			return nil, e
		}
		f.LatestAttemptError, f.NextAttemptAt = checksandalerts.OneLine(f.LatestAttemptError), time.Unix(0, next).UTC()
		f.OldestUndeliveredAlertQueuedAt = time.Unix(0, at).UTC()
		failing = append(failing, f)
	}
	return failing, rows.Err()
}

func (s *Store) DeliveryStats() (DeliverySummary, error) {
	var d DeliverySummary
	if e := s.DB.QueryRow("SELECT (SELECT COUNT(*) FROM outbox) + (SELECT COUNT(*) FROM intake WHERE state=?)", intakeQueued).Scan(&d.Pending); e != nil {
		return DeliverySummary{}, e
	}
	var first, newest int64
	if e := s.DB.QueryRow(`SELECT COALESCE((SELECT value FROM counters WHERE name='delivery_gaps'),0),
 COALESCE((SELECT value FROM counters WHERE name='first_delivery_gap_at'),0), COALESCE((SELECT value FROM counters WHERE name='newest_delivery_gap_at'),0)`).Scan(&d.Gaps, &first, &newest); e != nil {
		return DeliverySummary{}, e
	}
	if first != 0 {
		d.FirstGapAt = time.Unix(0, first).UTC()
	}
	if newest != 0 {
		d.NewestGapAt = time.Unix(0, newest).UTC()
	}
	e := s.DB.QueryRow("SELECT error FROM outbox WHERE error!='' ORDER BY seq DESC LIMIT 1").Scan(&d.LastError)
	if e != nil && e != sql.ErrNoRows {
		return DeliverySummary{}, e
	}
	return d, nil
}

func (s *Store) Prune(now time.Time) error {
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = s.PruneInTransaction(tx, now); e != nil {
		return e
	}
	return tx.Commit()
}
