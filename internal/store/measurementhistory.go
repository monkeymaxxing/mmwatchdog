package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/incidents"
)

// The measurement history. Every run of a check that measures a number
// produces one reading for each of its measurements, and MMWatchdog keeps every
// reading as it was measured, for a time and up to a size that the settings
// state. A node keeps its own readings in measurement_history.db. A hub also
// keeps the readings that the joined nodes send with their reports, in
// measurement_history_of_nodes.db. The rules are in
// docs/changing-mmwatchdog/design/operation.md under "Measurement history", in
// docs/changing-mmwatchdog/design/fleet.md under "Measurement history on the hub", and in
// docs/changing-mmwatchdog/implementation-rules.md under "Measurement history".

const (
	// MeasurementHistoryFormat is the layout version of both database files.
	MeasurementHistoryFormat      = 1
	MeasurementHistoryFile        = "measurement_history.db"
	MeasurementHistoryOfNodesFile = "measurement_history_of_nodes.db"
	// readingsWriteBytes is the most encoded readings that one transaction
	// writes, so that room for them can be made below the size limit before
	// they are written.
	readingsWriteBytes = 256 * 1024
	// historySparePages stay free below the size limit, for the pages that
	// SQLite needs beside the rows of a write.
	historySparePages = 8
)

// HistoryRemovalRows is the most readings that one step removes, and
// HistoryVacuumPages the most free pages that one step gives back to the file
// system. A step holds the history's lock and writes one transaction, so it
// stays short and its write-ahead log small: a large removal is many steps,
// and checks and reports run between them. Tests make the steps smaller.
var (
	HistoryRemovalRows int64 = 20000
	HistoryVacuumPages int64 = 2048
)

// The names of the measurement history's storage operations in a storage
// problem (storageHealth).
const (
	MeasurementHistoryWrite            = "measurement history write"
	MeasurementHistoryRetention        = "measurement history retention"
	MeasurementHistoryOfNodesWrite     = "measurement history of nodes write"
	MeasurementHistoryOfNodesRetention = "measurement history of nodes retention"
)

// The statements that run while MMWatchdog works, however large the history is. Each
// one finds its rows through the sequence number or through an index: the
// minimum and the maximum of a column are asked for one at a time, because
// SQLite reads the whole index for a statement that asks for both.
const (
	HistoryReachQuery           = "SELECT (SELECT MIN(kept_from) FROM readings), (SELECT MAX(kept_from) FROM readings)"
	HistorySequenceNumbersQuery = "SELECT (SELECT MIN(id) FROM readings), (SELECT MAX(id) FROM readings)"
	HistoryReadingsAfterQuery   = "SELECT id, at, check_group, check_name, measurements FROM readings WHERE id>? ORDER BY id"
	HistoryRemoveOldest         = "DELETE FROM readings WHERE id IN (SELECT id FROM readings ORDER BY kept_from, id LIMIT ?) RETURNING kept_from"
	HistoryRemoveOlderThan      = "DELETE FROM readings WHERE id IN (SELECT id FROM readings WHERE kept_from<? ORDER BY kept_from LIMIT ?)"
	// The index of a hub's readings by node leaves out a node's own readings,
	// which have no node name; the second term lets that index serve.
	HistoryRemoveOfNode = "DELETE FROM readings WHERE id IN (SELECT id FROM readings WHERE node=? AND node!='' AND id<=? ORDER BY id LIMIT ?)"
)

// MeasurementReading keeps the value, unit, and limits in force at a run.
// Module results remain value and unit only.
type MeasurementReading struct {
	Value                    float64  `json:"value"`
	Unit                     string   `json:"unit"`
	FailWhenMeasurementAbove *float64 `json:"fail_when_measurement_above,omitempty"`
	FailWhenMeasurementBelow *float64 `json:"fail_when_measurement_below,omitempty"`
}

// RunReadings are the readings of one run of one check: the value of each of
// its measurements at the time of the run.
type RunReadings struct {
	// SequenceNumber is the position of these readings in the measurement
	// history of the node that measured them, set when they are read from
	// it. A node sends the hub its readings in this order.
	SequenceNumber int64                         `json:"sequence_number"`
	At             time.Time                     `json:"at"`
	CheckGroup     string                        `json:"check_group"`
	Check          string                        `json:"check"`
	Measurements   map[string]MeasurementReading `json:"measurements"`
}

// Validate checks the readings of one run that another machine sent: the
// names of its check group and its check, a time that the history can store,
// and its measurements, by the rules for the result of an external module
// (ValidateResult), with a unit that is plain text.
func (r RunReadings) Validate() error {
	if !checksandalerts.NamePattern.MatchString(r.CheckGroup) || !checksandalerts.NamePattern.MatchString(r.Check) {
		return fmt.Errorf("readings must name a valid check group and check for the recorded run")
	}
	// The history stores a time as nanoseconds since 1970.
	if r.At.Before(time.Unix(0, 0)) || r.At.After(time.Unix(0, math.MaxInt64)) {
		return fmt.Errorf("readings must include a run time within the supported range, from the year 1970 to the year 2262")
	}
	if len(r.Measurements) == 0 || len(r.Measurements) > 128 {
		return fmt.Errorf("a recorded run must contain 1 to 128 measurements")
	}
	for name, m := range r.Measurements {
		for _, limit := range []*float64{m.FailWhenMeasurementAbove, m.FailWhenMeasurementBelow} {
			if limit != nil && (math.IsNaN(*limit) || math.IsInf(*limit, 0)) {
				return fmt.Errorf("a recorded measurement limit must be a finite number; send the limit used when the check ran")
			}
		}
		if !checksandalerts.NamePattern.MatchString(name) || m.Unit == "" || !checksandalerts.PlainText(m.Unit, 64) || math.IsNaN(m.Value) || math.IsInf(m.Value, 0) {
			return fmt.Errorf("measurement needs a valid name, a number, and a unit")
		}
	}
	return nil
}

// MeasurementHistory is one database file of readings. A node's holds its
// own readings, with an empty node name. A hub's holds the readings of the
// joined nodes, each under the name of its node, and for each node how far
// the hub has received its readings.
//
// Each reading has two times. at is the time of its run, as the node that ran
// it measured it, and never changes. kept_from is the time from which the
// reading's age counts: its own time, or on a hub the time of its receipt
// when that is earlier, so that a node whose clock runs ahead cannot keep a
// reading longer than the measurement history settings on the hub say. Readings are removed in the
// order of kept_from, the oldest first, for the time and for the size alike.
type MeasurementHistory struct {
	// Mu guards everything below. It is taken after Daemon.mu or hub.mu, and
	// no other lock is taken while it is held. A removal of many readings
	// takes it once for each step, never for the whole removal.
	Mu sync.Mutex
	// DB is nil when the file cannot be used; unusable then says why. MMWatchdog runs
	// without that history: nothing is stored, and every write returns
	// unusable, so the storage problem stays visible.
	DB       *sql.DB
	unusable error
	// ID identifies this database's sequence of readings, so a hub can tell
	// a new database from the one whose sequence numbers it holds.
	ID string
	// The file may hold maxPages pages of pageSize bytes: the size limit.
	// shrinking says that the file holds more pages than that, after a lower
	// limit: TrimStep brings it down in steps.
	pageSize, maxPages int64
	Shrinking          bool
	// LastSequenceNumber is the highest sequence number that this history
	// has given to a reading.
	LastSequenceNumber int64
	// oldest and newest are the lowest and the highest kept_from, zero while
	// there is no reading. removedBySizeLimitUntil is the kept_from of the
	// newest reading that the size limit has removed, and
	// removedUnderMaxPages the size limit, in pages, that removed it.
	oldest, newest, RemovedBySizeLimitUntil time.Time
	RemovedUnderMaxPages                    int64
	// Received holds, on a hub, for each node the measurement history that
	// its readings come from and the sequence number of the last one saved.
	Received map[string]ReadingsReceived
}

// ReadingsReceived is how far a hub has received the readings of one node:
// the measurement history of the node that they come from, and the sequence
// number of the last one saved.
type ReadingsReceived struct {
	MeasurementHistoryID string
	SequenceNumber       int64
}

// OpenMeasurementHistory opens the database file at path, creating it when
// it does not exist, and applies the size limit in bytes. It always returns a
// history. When the file cannot be used, the history is unusable and the
// error says why: MMWatchdog runs without it, because checks and alerts must never
// depend on the history. A file that holds more than its limit allows is not
// refused either: its oldest readings go, and the file shrinks (TrimStep).
func OpenMeasurementHistory(path string, sizeLimit int64) (*MeasurementHistory, error) {
	h := &MeasurementHistory{ID: incidents.ID(), Received: map[string]ReadingsReceived{}}
	// A file gives pages back only in incremental auto-vacuum mode, which
	// SQLite lets a new file choose before anything else is written to it.
	db, e := OpenDatabaseFile(path, "measurement history", MeasurementHistoryFormat, "PRAGMA auto_vacuum=INCREMENTAL;", `
 CREATE TABLE IF NOT EXISTS readings (id INTEGER PRIMARY KEY AUTOINCREMENT, node TEXT NOT NULL, at INTEGER NOT NULL, kept_from INTEGER NOT NULL, check_group TEXT NOT NULL, check_name TEXT NOT NULL, measurements TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS readings_kept_from ON readings(kept_from);
 CREATE INDEX IF NOT EXISTS readings_node ON readings(node,id) WHERE node!='';
 CREATE TABLE IF NOT EXISTS history (id INTEGER PRIMARY KEY CHECK(id=1), history_id TEXT NOT NULL, removed_by_size_limit_until INTEGER NOT NULL DEFAULT 0, removed_under_max_pages INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS received (node TEXT PRIMARY KEY, measurement_history_id TEXT NOT NULL, sequence_number INTEGER NOT NULL);`)
	if e == nil {
		h.DB = db
		if e = h.load(); e != nil {
			db.Close()
		}
	}
	if e == nil {
		h.SetSizeLimit(sizeLimit)
	} else {
		h.DB = nil
		h.unusable = fmt.Errorf("cannot save readings in measurement history file %s: %w; move the measurement history file aside and restart MMWatchdog to create an empty measurement history", path, e)
	}
	return h, h.unusable
}

func (h *MeasurementHistory) load() error {
	var removedUntil int64
	if _, e := h.DB.Exec("INSERT OR IGNORE INTO history(id,history_id) VALUES(1,?)", h.ID); e != nil {
		return e
	}
	if e := h.DB.QueryRow("SELECT history_id, removed_by_size_limit_until, removed_under_max_pages FROM history WHERE id=1").Scan(&h.ID, &removedUntil, &h.RemovedUnderMaxPages); e != nil {
		return e
	}
	if removedUntil > 0 {
		h.RemovedBySizeLimitUntil = time.Unix(0, removedUntil).UTC()
	}
	if e := h.DB.QueryRow("PRAGMA page_size").Scan(&h.pageSize); e != nil {
		return e
	}
	// SQLite keeps the highest sequence number that the table has given out,
	// also after its readings are gone.
	if e := h.DB.QueryRow("SELECT seq FROM sqlite_sequence WHERE name='readings'").Scan(&h.LastSequenceNumber); e != nil && e != sql.ErrNoRows {
		return e
	}
	rows, e := h.DB.Query("SELECT node, measurement_history_id, sequence_number FROM received")
	if e != nil {
		return e
	}
	for rows.Next() {
		var node string
		var r ReadingsReceived
		if e = rows.Scan(&node, &r.MeasurementHistoryID, &r.SequenceNumber); e != nil {
			_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
			return e
		}
		h.Received[node] = r
	}
	if e = errors.Join(rows.Err(), rows.Close()); e != nil {
		return e
	}
	return h.readReach()
}

func (h *MeasurementHistory) Close() error {
	if h.DB == nil {
		return nil
	}
	return h.DB.Close() //nolint:nilaway // OpenMeasurementHistory returns an allocated history even on failure; Close checks whether its database is nil.
}

// readReach reads the lowest and the highest kept_from. The caller holds
// h.mu or owns h.
func (h *MeasurementHistory) readReach() error {
	var oldest, newest sql.NullInt64
	if e := h.DB.QueryRow(HistoryReachQuery).Scan(&oldest, &newest); e != nil {
		return e
	}
	h.oldest, h.newest = time.Time{}, time.Time{}
	if oldest.Valid {
		h.oldest, h.newest = time.Unix(0, oldest.Int64).UTC(), time.Unix(0, newest.Int64).UTC()
	}
	return nil
}

// pages returns how many pages the file has and how many of them hold
// something. Pages that removed readings have left free are used again
// before the file grows. The caller holds h.mu or owns h.
func (h *MeasurementHistory) pages() (inFile, used int64, e error) {
	var free int64
	if e = h.DB.QueryRow("PRAGMA page_count").Scan(&inFile); e == nil {
		e = h.DB.QueryRow("PRAGMA freelist_count").Scan(&free)
	}
	return inFile, inFile - free, e
}

// SetSizeLimit states the size limit in bytes: when the daemon starts, and
// when a reload changes it. A higher limit applies at once. A limit below
// what the file holds does no work here: the oldest readings go and the file
// shrinks in the steps of TrimStep, which the daemon runs several times a
// second, so that a large removal never holds up checks or reports.
func (h *MeasurementHistory) SetSizeLimit(bytes int64) {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	if h.DB == nil {
		return
	}
	h.maxPages = bytes / h.pageSize
	// SQLite never sets the limit below the size of the file. If it cannot
	// set the limit now, TrimStep sets it and reports what fails.
	var applied int64
	if e := h.DB.QueryRow(fmt.Sprintf("PRAGMA max_page_count=%d", h.maxPages)).Scan(&applied); e != nil {
		h.Shrinking = true
		return
	}
	h.Shrinking = applied > h.maxPages
}

// TrimStep is one step toward a lower size limit: it removes some of the
// oldest readings while more pages are in use than the limit allows, then
// gives some free pages back to the file system, and reports whether the
// file is still larger than its limit. It does nothing, at no cost, while
// the file is within its limit.
func (h *MeasurementHistory) TrimStep() (shrinking bool, e error) {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	if h.DB == nil || !h.Shrinking {
		return false, nil
	}
	inFile, used, e := h.pages()
	if e != nil {
		return true, e
	}
	if used+historySparePages > h.maxPages {
		_, e = h.removeOldest(h.rowsFor(used + historySparePages - h.maxPages))
		return true, e
	}
	if inFile > h.maxPages {
		// Each step of the statement frees one page.
		rows, e := h.DB.Query(fmt.Sprintf("PRAGMA incremental_vacuum(%d)", HistoryVacuumPages))
		if e != nil {
			return true, e
		}
		for rows.Next() {
		}
		if e = errors.Join(rows.Err(), rows.Close()); e != nil {
			return true, e
		}
		// The file gets shorter when the write-ahead log is applied.
		if _, e = h.DB.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); e != nil {
			return true, e
		}
	}
	var applied int64
	if e = h.DB.QueryRow(fmt.Sprintf("PRAGMA max_page_count=%d", h.maxPages)).Scan(&applied); e != nil {
		return true, e
	}
	h.Shrinking = applied > h.maxPages
	return h.Shrinking, nil
}

// rowsFor estimates how many of the oldest readings take the given number of
// pages, at most HistoryRemovalRows: sequence numbers have no gaps except
// where readings were removed. The caller holds h.mu or owns h.
func (h *MeasurementHistory) rowsFor(pages int64) int64 {
	var first, last sql.NullInt64
	_, used, e := h.pages()
	if e != nil || used == 0 || h.DB.QueryRow(HistorySequenceNumbersQuery).Scan(&first, &last) != nil || !first.Valid {
		return 64
	}
	return min(max(pages*(last.Int64-first.Int64+1)/used, 64), HistoryRemovalRows)
}

// makeRoom removes the oldest readings, whichever node they came from, until
// needBytes more fit below the size limit, or until there is no reading
// left. While the file shrinks toward a lower limit, a write needs room only
// in the file as it is: TrimStep does the rest. The caller holds h.mu or
// owns h.
func (h *MeasurementHistory) makeRoom(needBytes int64) error {
	need := needBytes/h.pageSize + historySparePages
	for {
		inFile, used, e := h.pages()
		if e != nil || used+need <= h.maxPages || (h.Shrinking && used+need <= inFile) {
			return e
		}
		removed, e := h.removeOldest(h.rowsFor(used + need - h.maxPages))
		if e != nil || removed == 0 {
			return e
		}
	}
}

// removeOldest removes up to limit of the oldest readings because the size
// limit needs their room, records the newest one that it removed and the
// limit that removed it, and returns how many it removed. The caller holds
// h.mu or owns h.
func (h *MeasurementHistory) removeOldest(limit int64) (int64, error) {
	tx, e := h.DB.Begin()
	if e != nil {
		return 0, e
	}
	defer tx.Rollback()
	rows, e := tx.Query(HistoryRemoveOldest, limit)
	if e != nil {
		return 0, e
	}
	var removed, newest int64
	for rows.Next() {
		var keptFrom int64
		if e = rows.Scan(&keptFrom); e != nil {
			_ = rows.Close() // Scanning already failed, so this transaction is rejected rather than accepted after cursor cleanup.
			return 0, e
		}
		removed, newest = removed+1, max(newest, keptFrom)
	}
	if e = errors.Join(rows.Err(), rows.Close()); e != nil {
		return 0, e
	}
	until := h.RemovedBySizeLimitUntil
	if removed > 0 {
		if at := time.Unix(0, newest).UTC(); at.After(until) {
			until = at
		}
		if _, e = tx.Exec("UPDATE history SET removed_by_size_limit_until=?, removed_under_max_pages=? WHERE id=1", until.UnixNano(), h.maxPages); e != nil {
			return 0, e
		}
	}
	if e = tx.Commit(); e != nil {
		return 0, e
	}
	if removed > 0 {
		h.RemovedBySizeLimitUntil, h.RemovedUnderMaxPages = until, h.maxPages
	}
	return removed, h.readReach()
}

// insertReadings writes the readings of one run in tx and returns their
// sequence number. keptFrom is the time from which their age counts. The
// measurements are stored as JSON text, so that a person who opens the file
// reads them as they are. The caller has made room for them.
func insertReadings(tx *sql.Tx, node string, r RunReadings, keptFrom time.Time, measurements []byte) (int64, error) {
	result, e := tx.Exec("INSERT INTO readings(node,at,kept_from,check_group,check_name,measurements) VALUES(?,?,?,?,?,?)", node, r.At.UnixNano(), keptFrom.UnixNano(), r.CheckGroup, r.Check, string(measurements))
	if e != nil {
		return 0, e
	}
	return result.LastInsertId()
}

// reached records the kept_from of a new reading in the reach of the
// history. The caller holds h.mu.
func (h *MeasurementHistory) reached(keptFrom time.Time) {
	keptFrom = keptFrom.UTC()
	if h.oldest.IsZero() || keptFrom.Before(h.oldest) {
		h.oldest = keptFrom
	}
	if keptFrom.After(h.newest) {
		h.newest = keptFrom
	}
}

// Add stores the readings of one run of a check on this node.
func (h *MeasurementHistory) Add(r RunReadings) error {
	measurements, e := json.Marshal(r.Measurements)
	if e != nil {
		return e
	}
	h.Mu.Lock()
	defer h.Mu.Unlock()
	if h.DB == nil {
		return h.unusable
	}
	if e = h.makeRoom(2 * int64(len(measurements)+len(r.CheckGroup)+len(r.Check)+64)); e != nil {
		return e
	}
	tx, e := h.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	sequenceNumber, e := insertReadings(tx, "", r, r.At, measurements)
	if e == nil {
		e = tx.Commit()
	}
	if e == nil {
		h.reached(r.At)
		h.LastSequenceNumber = sequenceNumber
	}
	return e
}

// After returns this node's readings after the sequence number, oldest
// first, as many as fit in maxBytes of JSON.
func (h *MeasurementHistory) After(sequenceNumber int64, maxBytes int) ([]RunReadings, error) {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	if h.DB == nil {
		return nil, nil
	}
	rows, e := h.DB.Query(HistoryReadingsAfterQuery, sequenceNumber)
	if e != nil {
		return nil, e
	}
	defer func() {
		_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	}()
	out, total := []RunReadings{}, 0
	for rows.Next() {
		var r RunReadings
		var at int64
		var measurements []byte
		if e = rows.Scan(&r.SequenceNumber, &at, &r.CheckGroup, &r.Check, &measurements); e != nil {
			return nil, e
		}
		r.At = time.Unix(0, at).UTC()
		if e = json.Unmarshal(measurements, &r.Measurements); e != nil {
			return nil, e
		}
		encoded, e := json.Marshal(r)
		if e != nil {
			return nil, e
		}
		if total += len(encoded) + 1; total > maxBytes {
			break
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ContinueAfter makes the sequence numbers of this node's next readings
// follow sequenceNumber, the number of the last reading that the hub holds
// from this history, when the history has given out only lower ones. That is
// so after the file was restored from an older copy: the hub takes only what
// follows the number it holds, so without this step it would ignore every
// new reading until the node's numbers had passed that number again.
func (h *MeasurementHistory) ContinueAfter(sequenceNumber int64) error {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	if h.DB == nil || sequenceNumber <= h.LastSequenceNumber {
		return nil
	}
	result, e := h.DB.Exec("UPDATE sqlite_sequence SET seq=? WHERE name='readings'", sequenceNumber)
	if e != nil {
		return e
	}
	// The table has no row there before its first reading.
	if n, _ := result.RowsAffected(); n == 0 {
		if _, e = h.DB.Exec("INSERT INTO sqlite_sequence(name,seq) VALUES('readings',?)", sequenceNumber); e != nil {
			return e
		}
	}
	h.LastSequenceNumber = sequenceNumber
	return nil
}

// ReceivedFrom returns the sequence number of the last reading that this hub
// has saved from the measurement history with this ID of the node, or 0 when
// it has none: also for a node whose measurement history is a new one, whose
// sequence numbers start over.
func (h *MeasurementHistory) ReceivedFrom(node, measurementHistoryID string) int64 {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	if r := h.Received[node]; r.MeasurementHistoryID == measurementHistoryID {
		return r.SequenceNumber
	}
	return 0
}

// AddFromNode saves, on a hub, the readings that a node sent with a report,
// and returns the sequence number of the last reading that the hub now holds
// from that node. after is the sequence number that the readings follow. The
// readings count only when they continue from where the hub is; otherwise
// nothing is saved, and the returned number tells the node where to
// continue. The readings and the number are saved together, so a node that
// sends its readings again after a failure never adds one twice. A reading
// keeps the time of its run. Its age counts from that time, or from now, the
// time of its receipt, when the node's clock is ahead of the hub's.
//
// A hub whose file cannot be used returns -1, the number that a node has
// before its first reply: a node sends no readings after it, so no node
// sends readings that the hub cannot keep.
func (h *MeasurementHistory) AddFromNode(node, measurementHistoryID string, after int64, readings []RunReadings, now time.Time) (int64, error) {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	if h.DB == nil {
		return -1, h.unusable
	}
	saved := h.Received[node]
	if saved.MeasurementHistoryID != measurementHistoryID {
		saved = ReadingsReceived{MeasurementHistoryID: measurementHistoryID}
	}
	if after != saved.SequenceNumber {
		readings = nil
	}
	for len(readings) > 0 || saved != h.Received[node] {
		// One transaction holds at most readingsWriteBytes of readings.
		encoded, bytes := [][]byte{}, 0
		for len(encoded) < len(readings) && (bytes < readingsWriteBytes || len(encoded) == 0) {
			measurements, e := json.Marshal(readings[len(encoded)].Measurements)
			if e != nil {
				return h.Received[node].SequenceNumber, e
			}
			encoded, bytes = append(encoded, measurements), bytes+len(measurements)+len(node)+128
		}
		if e := h.makeRoom(2 * int64(bytes)); e != nil {
			return h.Received[node].SequenceNumber, e
		}
		tx, e := h.DB.Begin()
		if e != nil {
			return h.Received[node].SequenceNumber, e
		}
		for i, measurements := range encoded {
			//nolint:nilaway // encoded has at most len(readings) entries; nil readings produce no indexed iterations, and slicing nil at zero is valid.
			if _, e = insertReadings(tx, node, readings[i], minTime(readings[i].At, now), measurements); e != nil {
				break
			}
			saved.SequenceNumber = readings[i].SequenceNumber
		}
		if e == nil {
			_, e = tx.Exec("INSERT INTO received(node,measurement_history_id,sequence_number) VALUES(?,?,?) ON CONFLICT(node) DO UPDATE SET measurement_history_id=excluded.measurement_history_id, sequence_number=excluded.sequence_number", node, saved.MeasurementHistoryID, saved.SequenceNumber)
		}
		if e == nil {
			e = tx.Commit()
		}
		if e != nil {
			tx.Rollback()
			return h.Received[node].SequenceNumber, e
		}
		for i := range encoded {
			h.reached(minTime(readings[i].At, now))
		}
		h.Received[node], readings = saved, readings[len(encoded):]
	}
	return saved.SequenceNumber, nil
}

func minTime(a, b time.Time) time.Time {
	if a.After(b) {
		return b
	}
	return a
}

// ForgetNode removes, on a hub, every reading of a node and how far the hub
// has received its readings. It forgets first how far the hub was, so a node
// that joins again under the name starts over, and then removes the readings
// that were there at that moment, in steps: a node of the same name can
// report between them, and its new readings stay.
func (h *MeasurementHistory) ForgetNode(node string) error {
	last, e := h.ForgetReceived(node)
	if e != nil {
		return e
	}
	return h.RemoveReadingsOfNode(node, last)
}

// RemoveReadingsOfNode removes the node's readings up to the sequence number
// last, in steps.
func (h *MeasurementHistory) RemoveReadingsOfNode(node string, last int64) error {
	for {
		more, e := h.RemoveReadingsOfNodeStep(node, last)
		if e != nil || !more {
			return e
		}
	}
}

// ForgetReceived is the first step of ForgetNode: it forgets how far the hub
// has received the readings of a node, and returns the sequence number of
// the newest reading in the file at that moment. The node's readings up to
// that number are the ones to remove.
func (h *MeasurementHistory) ForgetReceived(node string) (int64, error) {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	if h.DB == nil {
		return 0, nil
	}
	if _, e := h.DB.Exec("DELETE FROM received WHERE node=?", node); e != nil {
		return 0, e
	}
	delete(h.Received, node)
	return h.lastSequenceNumberInFile(), nil
}

// RemoveReadingsOfNodeStep is one step of RemoveReadingsOfNode: it removes up
// to HistoryRemovalRows of the node's readings up to the sequence number
// last, and reports whether there may be more to remove.
func (h *MeasurementHistory) RemoveReadingsOfNodeStep(node string, last int64) (bool, error) {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	if h.DB == nil {
		return false, nil
	}
	result, e := h.DB.Exec(HistoryRemoveOfNode, node, last, HistoryRemovalRows)
	if e != nil {
		return false, e
	}
	removed, _ := result.RowsAffected()
	return removed == HistoryRemovalRows, h.readReach()
}

// lastSequenceNumberInFile is the highest sequence number of a reading in
// the file, or 0. The caller holds h.mu.
func (h *MeasurementHistory) lastSequenceNumberInFile() int64 {
	var first, last sql.NullInt64
	if h.DB.QueryRow(HistorySequenceNumbersQuery).Scan(&first, &last) != nil {
		return 0
	}
	return last.Int64
}

// NodesWithReadings names, on a hub, every node that the hub has received
// readings from.
func (h *MeasurementHistory) NodesWithReadings() []string {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	return checksandalerts.Names(h.Received)
}

// Prune removes the readings that are older than keep at now, in steps. It
// also forgets what the size limit removed once those readings would be too
// old anyway: from then on the history is as long as its time allows.
func (h *MeasurementHistory) Prune(now time.Time, keep time.Duration) error {
	cutoff := now.Add(-keep)
	for {
		h.Mu.Lock()
		more, e := h.pruneStep(cutoff)
		h.Mu.Unlock()
		if e != nil || !more {
			return e
		}
	}
}

// pruneStep is one step of prune, and reports whether there may be more to
// remove. The caller holds h.mu.
func (h *MeasurementHistory) pruneStep(cutoff time.Time) (bool, error) {
	if h.DB == nil {
		return false, nil
	}
	if !h.RemovedBySizeLimitUntil.IsZero() && !h.RemovedBySizeLimitUntil.After(cutoff) {
		if _, e := h.DB.Exec("UPDATE history SET removed_by_size_limit_until=0, removed_under_max_pages=0 WHERE id=1"); e != nil {
			return false, e
		}
		h.RemovedBySizeLimitUntil, h.RemovedUnderMaxPages = time.Time{}, 0
	}
	if h.oldest.IsZero() || !h.oldest.Before(cutoff) {
		return false, nil
	}
	result, e := h.DB.Exec(HistoryRemoveOlderThan, cutoff.UnixNano(), HistoryRemovalRows)
	if e != nil {
		return false, e
	}
	removed, _ := result.RowsAffected()
	return removed == HistoryRemovalRows, h.readReach()
}

// historyReach says how far back a measurement history reaches: the times
// from which its oldest and its newest reading count, both zero while it
// holds none, and whether the size limit has cut it short.
type historyReach struct {
	Oldest, Newest time.Time
	// CutShortBySizeLimit says that the size limit has removed readings that
	// are younger than the time the history keeps readings for.
	CutShortBySizeLimit bool
}

// Reach says how far back the history reaches at now, when its settings keep
// readings for keep. The size limit has cut the history short while a
// reading that it removed would still be young enough to be kept. A size
// limit that was raised since then has cut nothing: the history fills again,
// and the line that names the limit would name one that removed no reading.
func (h *MeasurementHistory) Reach(now time.Time, keep time.Duration) historyReach {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	cutShort := !h.oldest.IsZero() && h.RemovedBySizeLimitUntil.After(now.Add(-keep)) && h.RemovedUnderMaxPages >= h.maxPages
	return historyReach{h.oldest, h.newest, cutShort}
}

// HistoryCutShortSentence is the line that mmwatchdog status and mmwatchdog fleet show when
// the size limit has cut a measurement history short, such as "measurement
// history reaches back 5 days 3 hours; the size limit of 256 MiB prevented keeping the requested
// 7 days". name is "measurement history" on a node, and "measurement
// history of nodes" on a hub; oldest is the time of the oldest reading.
func HistoryCutShortSentence(name string, oldest time.Time, keep checksandalerts.Duration, sizeLimit checksandalerts.Size, now time.Time) string {
	size := string(sizeLimit)
	for _, unit := range []string{"MiB", "GiB"} {
		if number, ok := strings.CutSuffix(size, unit); ok {
			size = number + " " + unit
		}
	}
	return fmt.Sprintf("%s reaches back %s; the size limit of %s prevented keeping the requested %s", name, DaysAndHours(now.Sub(oldest)), size, DaysAndHours(keep.Value()))
}

// DaysAndHours writes a length of time with its largest unit and, when it is
// not zero, the next smaller one, such as "5 days 3 hours", "7 days", or
// "1 hour 30 minutes".
func DaysAndHours(d time.Duration) string {
	d = max(d, 0).Round(time.Second)
	units := []struct {
		length time.Duration
		name   string
	}{{24 * time.Hour, "day"}, {time.Hour, "hour"}, {time.Minute, "minute"}, {time.Second, "second"}}
	count := func(n time.Duration, name string) string {
		if n != 1 {
			name += "s"
		}
		return fmt.Sprintf("%d %s", n, name)
	}
	for i, unit := range units {
		n := d / unit.length
		if n == 0 && i < len(units)-1 {
			continue
		}
		text := count(n, unit.name)
		if i < len(units)-1 {
			if next := (d - n*unit.length) / units[i+1].length; next > 0 {
				text += " " + count(next, units[i+1].name)
			}
		}
		return text
	}
	return ""
}

// MeasurementHistoryStatus says how much measurement history this node
// keeps and how far back it reaches now.
type MeasurementHistoryStatus struct {
	// OldestReadingAt and NewestReadingAt are the times of the oldest and of
	// the newest reading that the node holds; both are absent while it holds
	// none.
	OldestReadingAt *time.Time `json:"oldest_reading_at,omitempty"`
	NewestReadingAt *time.Time `json:"newest_reading_at,omitempty"`
	// KeepMeasurementHistoryFor and MeasurementHistorySizeLimit are the
	// node's settings of those names.
	KeepMeasurementHistoryFor   checksandalerts.Duration `json:"keep_measurement_history_for"`
	MeasurementHistorySizeLimit checksandalerts.Size     `json:"measurement_history_size_limit"`
	// CutShortByMeasurementHistorySizeLimit says that the size limit has
	// removed readings that are younger than keep_measurement_history_for,
	// so the history reaches back less far than that.
	CutShortByMeasurementHistorySizeLimit bool `json:"cut_short_by_measurement_history_size_limit"`
}

// CutShortSentence is the line that mmwatchdog status shows when the size limit has
// cut the measurement history short, such as "measurement history reaches
// back 5 days 3 hours; the size limit of 256 MiB prevented keeping the requested 7 days". It
// is "" when the history reaches as far back as its settings keep readings.
func (s MeasurementHistoryStatus) CutShortSentence(now time.Time) string {
	if !s.CutShortByMeasurementHistorySizeLimit || s.OldestReadingAt == nil {
		return ""
	}
	return HistoryCutShortSentence("measurement history", *s.OldestReadingAt, s.KeepMeasurementHistoryFor, s.MeasurementHistorySizeLimit, now)
}

// MeasurementHistoryOfNodesStatus says how much measurement history of the
// joined nodes a hub keeps and how far back it reaches now.
type MeasurementHistoryOfNodesStatus struct {
	// OldestReadingAt and NewestReadingAt are the times of the oldest and of
	// the newest reading of any node that the hub holds; both are absent
	// while it holds none.
	OldestReadingAt *time.Time `json:"oldest_reading_at,omitempty"`
	NewestReadingAt *time.Time `json:"newest_reading_at,omitempty"`
	// KeepMeasurementHistoryOfNodesFor and MeasurementHistoryOfNodesSizeLimit
	// are the settings of those names in the hub's fleet_hub_role.
	KeepMeasurementHistoryOfNodesFor   checksandalerts.Duration `json:"keep_measurement_history_of_nodes_for"`
	MeasurementHistoryOfNodesSizeLimit checksandalerts.Size     `json:"measurement_history_of_nodes_size_limit"`
	// CutShortByMeasurementHistoryOfNodesSizeLimit says that the size limit
	// has removed readings that are younger than
	// keep_measurement_history_of_nodes_for, so the history reaches back
	// less far than that.
	CutShortByMeasurementHistoryOfNodesSizeLimit bool `json:"cut_short_by_measurement_history_of_nodes_size_limit"`
}

// CutShortSentence is the line that mmwatchdog fleet shows when the size limit has
// cut the measurement history of nodes short, such as "measurement history
// of nodes reaches back 5 days 3 hours; the size limit of 20 GiB prevented keeping the requested
// 7 days". It is "" when the history reaches as far back as the hub keeps
// readings. The line is information: it is no storage problem.
func (s MeasurementHistoryOfNodesStatus) CutShortSentence(now time.Time) string {
	if !s.CutShortByMeasurementHistoryOfNodesSizeLimit || s.OldestReadingAt == nil {
		return ""
	}
	return HistoryCutShortSentence("measurement history of nodes", *s.OldestReadingAt, s.KeepMeasurementHistoryOfNodesFor, s.MeasurementHistoryOfNodesSizeLimit, now)
}
