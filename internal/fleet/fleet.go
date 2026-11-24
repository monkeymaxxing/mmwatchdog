// Package fleet holds a fleet hub and its joined nodes: the hub's own
// database of members and their reports (fleet.db), the node status
// report and the reporter that sends it, the fleet's alert settings and
// the hub's grouping of alerts (fleetalerts.go), the fleet checks that
// notice a silent node or hub (fleetwatch.go), and what the hub tells
// MMHotline about its nodes (mmhotline.go). The daemon runs it; the HTTP
// handlers and the loops are the daemon's.
package fleet

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/store"
)

// The fleet rules this file implements are in docs/changing-mmwatchdog/implementation-rules.md under
// "Fleet reporting". The hub side lives here; the reporter is in reporter.go.

const (
	ReportLimit = 4 * 1024 * 1024 // encoded report
	// JoinLimit bounds an encoded join request. It is more than a node with
	// the most node groups that a node can name sends
	// (MaxNodeGroupsOfANode), and small enough for a request that anyone who
	// reaches the hub's port can send.
	JoinLimit        = 64 * 1024
	ReportEvents     = 500 // events per report
	ReportEventBytes = 1024 * 1024
	fleetFormat      = 8
	clockOffsetShown = 5 * time.Second
	fleetCommit      = "fleet commit"
	// ReportSpareBytes stay free in a report beside its readings, for what
	// the encoding of the whole report adds.
	ReportSpareBytes = 64 * 1024
	// ReportReadingsBytes is the most readings, as JSON, that one report
	// carries when nodes report every reportReadingsEvery. A node that
	// reports less often carries more in proportion, and one that reports
	// more often less, but never less than ReportReadingsFloor, which holds
	// the largest run that a hub accepts. A node with more to send, as after
	// a hub outage, sends it over its next reports. Readings are for looking
	// back, so they can arrive slowly; a hub that decodes and saves megabytes
	// for each node that catches up has no time left for the other nodes.
	ReportReadingsBytes = 256 * 1024
	reportReadingsEvery = 30 * time.Second
	// ReportReadingsFloor lets the largest run's readings, with their limits,
	// fit in one report: the maximum encoded run measures about 80 KiB.
	ReportReadingsFloor = 128 * 1024
	// MaxSequenceNumber is the highest sequence number of an event or of a
	// reading that a hub and a node take from each other. No node reaches
	// it: at a thousand events a second it is 285,000 years away. A higher
	// number is a mistake, and a node that continued its numbers after it
	// could give no number to its next event.
	MaxSequenceNumber = 1 << 53
)

// serverNamePattern accepts system host names, including fully qualified ones.
var serverNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)
var tokenPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var IDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// JoinCode is what a node needs to join the hub: where it is, the
// fingerprint that identifies its certificate, and the join secret.
type JoinCode struct {
	Address, Fingerprint, Secret string
}

func (j JoinCode) String() string {
	return "mmwatchdog-join " + j.Address + " " + j.Fingerprint + " " + j.Secret
}

func ParseJoinCode(s string) (JoinCode, error) {
	f := strings.Fields(s)
	if len(f) != 4 || f[0] != "mmwatchdog-join" {
		return JoinCode{}, fmt.Errorf("expected a join code printed by mmwatchdog fleet join-code")
	}
	j := JoinCode{f[1], f[2], f[3]}
	if node, _, e := net.SplitHostPort(j.Address); e != nil || node == "" || checksandalerts.HostPort(j.Address, 1) != nil {
		return JoinCode{}, fmt.Errorf("join code has an invalid hub address")
	}
	if !tokenPattern.MatchString(j.Fingerprint) || !tokenPattern.MatchString(j.Secret) {
		return JoinCode{}, fmt.Errorf("join code has an invalid fingerprint or secret")
	}
	return j, nil
}

// LoadJoinCode reads join_fleet_hub.join_code_file, or returns nil without one.
func LoadJoinCode(c *config.Config) (*JoinCode, error) {
	if c.JoinFleetHub == nil {
		return nil, nil
	}
	content, e := primitives.ReadFile(c.JoinFleetHub.JoinCodeFile, 4096)
	s := strings.TrimSpace(string(content))
	if e == nil {
		var j JoinCode
		if j, e = ParseJoinCode(s); e == nil {
			return &j, nil
		}
	}
	return nil, c.ErrorAt("join_fleet_hub.join_code_file", e.Error())
}

// Build identifies the executable, as mmwatchdog version prints it.
type Build struct {
	Arch     string `json:"arch"`
	Go       string `json:"go"`
	Modified bool   `json:"modified"`
	OS       string `json:"os"`
	Revision string `json:"revision"`
	Version  string `json:"version"`
}

// Report is one node's complete state, sent every report interval. The hub
// keeps the latest one it saved; Events carry the local history after After.
type NodeStatusReport struct {
	StartID       string                                 `json:"daemon_start_id"`
	Seq           int64                                  `json:"report_sequence_number"`
	Sent          time.Time                              `json:"sent_at"`
	Build         Build                                  `json:"build"`
	Config        string                                 `json:"config_fingerprint"`
	Summary       incidents.StatusSummary                `json:"summary"`
	Checks        map[string]ReportedCheck               `json:"checks"`
	NodeResources map[string]checksandalerts.Measurement `json:"node_resources"`
	Maintenance   map[string]checksandalerts.Window      `json:"maintenance"`
	// PreferOutageAlerter asks the hub to choose this node as its hub-outage alerter.
	PreferOutageAlerter bool                    `json:"prefer_this_node_as_hub_outage_alerter,omitempty"`
	Delivery            store.DeliverySummary   `json:"delivery"`
	StorageError        string                  `json:"storage_error"`
	History             string                  `json:"event_history_id"`
	After               int64                   `json:"events_after_sequence_number"`
	Events              []checksandalerts.Event `json:"events,omitempty"`
	// MeasurementHistoryID identifies the node's measurement history, and
	// Readings are the node's readings after ReadingsAfter, the sequence
	// number that the hub last told the node, oldest first: as many as the
	// report has room for. ReadingsAfter is -1 until a reply has given it.
	MeasurementHistoryID string              `json:"measurement_history_id"`
	ReadingsAfter        int64               `json:"readings_after_sequence_number"`
	Readings             []store.RunReadings `json:"readings,omitempty"`
	// IncidentsSentWithoutTheHub lists the incidents whose failure or raise
	// this node sent without the hub to a destination that keeps a fleet
	// incident set, each with that destination, until a reply names it in
	// IncidentsKnownToTheHub.
	IncidentsSentWithoutTheHub []store.IncidentSentWithoutTheHub `json:"incidents_sent_without_the_hub,omitempty"`
	// The node settings for the fleet have their own names on the wire: see
	// NodeFleetSettings. The hub keeps them for when the node stops reporting.
	AlertSeverityWhenThisNodeGoesSilent              string                   `json:"alert_severity_when_this_node_goes_silent"`
	SendSilenceAlertsOfThisNodeOnlyToDestinations    *[]string                `json:"send_silence_alerts_of_this_node_only_to_destinations,omitempty"`
	RaiseToSosAfterThisNodeIsSilentFor               checksandalerts.Duration `json:"raise_to_sos_after_this_node_is_silent_for,omitempty"`
	ThisNodeIsInNodeGroups                           []string                 `json:"this_node_is_in_node_groups,omitempty"`
	RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent []string                 `json:"raise_to_sos_when_every_node_of_these_node_groups_is_silent,omitempty"`
}

// FleetSettings are the node's settings that the report carries for the
// fleet.
func (r *NodeStatusReport) FleetSettings() config.NodeFleetSettings {
	return config.NodeFleetSettings{AlertSeverityWhenThisNodeGoesSilent: r.AlertSeverityWhenThisNodeGoesSilent, SendSilenceAlertsOfThisNodeOnlyToDestinations: r.SendSilenceAlertsOfThisNodeOnlyToDestinations, RaiseToSosAfterThisNodeIsSilentFor: r.RaiseToSosAfterThisNodeIsSilentFor, ThisNodeIsInNodeGroups: r.ThisNodeIsInNodeGroups, RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent: r.RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent}
}

// ReportedCheck is a check's state and explanation, with its open
// incidents and their evidence.
type ReportedCheck struct {
	State               string           `json:"state"`
	Message             string           `json:"message"`
	FailureIncident     *incidents.Track `json:"failure_incident,omitempty"`
	CannotCheckIncident *incidents.Track `json:"cannot_check_incident,omitempty"`
	// SendAlertsOnlyToDestinations are the destinations that the check
	// names, so that each of the hub's fleet incident sets takes its
	// incidents by the same rule as its alerts (config.GoesTo).
	SendAlertsOnlyToDestinations *[]string `json:"send_alerts_only_to_destinations,omitempty"`
	// Findings are the findings of a check whose module reports them, by
	// the ID of the finding, such as the failed units of a
	// every_systemd_unit check by unit name: each has its own state,
	// message, and incident.
	Findings map[string]ReportedCheck `json:"findings,omitempty"`
}

// Validate checks what the hub relies on; size limits bound the rest.
func (r *NodeStatusReport) Validate() error {
	if !IDPattern.MatchString(r.StartID) || !IDPattern.MatchString(r.History) || r.Seq < 1 || r.After < -1 || r.After > MaxSequenceNumber {
		return fmt.Errorf("node status report requires valid daemon_start_id, event_history_id, report_sequence_number, and events_after_sequence_number")
	}
	if r.Summary.ExitCode != 0 && r.Summary.ExitCode != 1 && r.Summary.ExitCode != 3 {
		return fmt.Errorf("report exit code must be 0, 1 or 3")
	}
	if problem := r.FleetSettings().Problem(); problem != "" {
		return fmt.Errorf("report %s", problem)
	}
	for _, c := range r.Checks {
		tracks := []*incidents.Track{c.FailureIncident, c.CannotCheckIncident}
		for unit, u := range c.Findings {
			if !checksandalerts.ValidFindingID(unit) || len(u.Findings) > 0 {
				return fmt.Errorf("report carries a finding with an invalid ID")
			}
			tracks = append(tracks, u.FailureIncident, u.CannotCheckIncident)
		}
		for _, t := range tracks {
			if t != nil && (config.SeverityRank(t.AlertSeverity) < 0 || !config.ValidSeverityList(t.AnnouncedAlertSeverities)) {
				return fmt.Errorf("report carries an incident with an invalid alert severity")
			}
			if t != nil && !ValidExactFinding(checksandalerts.Event{Finding: "f", ExactFinding: t.CurrentExactFinding, ExactFindingSequence: t.ExactFindingSequence}) {
				return fmt.Errorf("report carries an incident with an invalid exact finding")
			}
		}
	}
	if len(r.Events) > ReportEvents {
		return fmt.Errorf("report carries more than %d events", ReportEvents)
	}
	for _, d := range r.IncidentsSentWithoutTheHub {
		if !checksandalerts.NamePattern.MatchString(d.Destination) || !IDPattern.MatchString(d.Incident) || !checksandalerts.NamePattern.MatchString(d.CheckGroup) || !checksandalerts.NamePattern.MatchString(d.Check) || (d.Finding != "" && !checksandalerts.ValidFindingID(d.Finding)) || (d.Type != "failed" && d.Type != "cannot_check") || len(d.AlertSeveritiesSentForIncident) == 0 || !config.ValidSeverityList(d.AlertSeveritiesSentForIncident) ||
			!ValidExactFinding(checksandalerts.Event{Finding: d.Finding, ExactFinding: d.ExactFinding, ExactFindingSequence: d.ExactFindingSequence}) ||
			(d.AlertSource != "" && !checksandalerts.MMHotlineIDPattern.MatchString(d.AlertSource)) {
			return fmt.Errorf("report carries an invalid incident sent without the hub")
		}
	}
	previous := r.After
	for _, e := range r.Events {
		if e.Seq <= previous || e.Seq > MaxSequenceNumber {
			return fmt.Errorf("node status report events must have increasing sequence numbers after events_after_sequence_number")
		}
		previous = e.Seq
	}
	if !IDPattern.MatchString(r.MeasurementHistoryID) || r.ReadingsAfter < -1 || r.ReadingsAfter > MaxSequenceNumber {
		return fmt.Errorf("node status report requires a valid measurement_history_id and readings_after_sequence_number")
	}
	previous = r.ReadingsAfter
	for _, readings := range r.Readings {
		if readings.SequenceNumber <= previous || readings.SequenceNumber > MaxSequenceNumber || r.ReadingsAfter < 0 {
			return fmt.Errorf("node status report readings must have increasing sequence numbers after readings_after_sequence_number, which must be 0 or more")
		}
		previous = readings.SequenceNumber
		if e := readings.Validate(); e != nil {
			return fmt.Errorf("report carries invalid readings: %w", e)
		}
	}
	return nil
}

// member is one node as the hub holds it. It is stored whole in fleet.db.
type member struct {
	// Credential is the hash of the node's credential. The hub's own entry
	// has none, so it can never authenticate a request.
	Credential string    `json:"credential,omitempty"`
	Joined     time.Time `json:"joined_at"`
	Address    string    `json:"address,omitempty"`
	// NodeFleetSettings are the node's settings from its join; its reports
	// carry the current ones. See FleetSettings.
	config.NodeFleetSettings
	Received time.Time     `json:"received,omitempty"`
	Offset   time.Duration `json:"offset_ns,omitempty"`
	// Since starts the node's continuous reporting, and Preferred is its
	// latest report's hub-outage alerter preference: both order the hub-outage alerter choice.
	Since     time.Time `json:"reporting_since,omitempty"`
	Preferred bool      `json:"prefer_this_node_as_hub_outage_alerter,omitempty"`
	// The newer-report test: reports order by sequence within a daemon start,
	// and the start most recently replaced is refused.
	StartID          string            `json:"start_id,omitempty"`
	ReplacedStart    string            `json:"replaced_start,omitempty"`
	Seq              int64             `json:"seq,omitempty"`
	History          string            `json:"history_id,omitempty"`
	Cursor           int64             `json:"cursor"`
	NodeStatusReport *NodeStatusReport `json:"report,omitempty"`
	Replaced         time.Time         `json:"replaced_silent_node_of_same_name_at,omitempty"`
	Conflict         *Conflict         `json:"conflict,omitempty"`
	// Reporting evidence is local to this hub run. A restart grants a fresh
	// deadline; a timing change applies from this member's next receipt.
	Deadline time.Time `json:"-" yaml:"-"`
	Reported bool      `json:"-" yaml:"-"`
}

// FleetSettings are the node's settings as the hub knows them: those of its
// latest saved report, or those of its join before the first one.
func (m *member) fleetSettings() config.NodeFleetSettings {
	if m.NodeStatusReport != nil {
		return m.NodeStatusReport.FleetSettings()
	}
	return m.NodeFleetSettings
}

// Conflict is the latest refused claim on a node's name.
type Conflict struct {
	Address string    `json:"claimed_from_address"`
	At      time.Time `json:"at"`
}

// RefusedJoins counts joins with a wrong secret. They are kept in memory only,
// so an unknown sender never adds a row.
type RefusedJoins struct {
	Count   int       `json:"count"`
	Last    time.Time `json:"last,omitempty"`
	Address string    `json:"last_refused_from_address,omitempty"`
}

// Hub is the fleet store and its memory. mu guards everything below and is
// never held together with Daemon.mu.
type Hub struct {
	Address string
	Name    string // the hub's own node name
	Cert    tls.Certificate
	Code    JoinCode
	Mu      sync.Mutex
	DB      *sql.DB
	Nodes   map[string]*member
	// Credentials maps a credential hash to its node's name.
	Credentials map[string]string
	Refused     RefusedJoins
	Storage     StorageHealth
	// OutageAlerters are the ranked nodes chosen to watch the hub, or empty while no
	// node reports.
	OutageAlerters []string
	// ReportEvery is atomic so reload can change the interval while holding
	// only Daemon.mu. Three missed reports make a node silent.
	ReportEvery atomic.Int64
	// Limits bound the events of the nodes; atomic for the same reason.
	Limits atomic.Pointer[config.HubLimits]
	// EventBytes is the encoded size of the events kept for each node, and
	// eventBytesTotal their sum, so that the size limit needs no scan of
	// the events on each report.
	EventBytes      map[string]int64
	EventBytesTotal int64
	// FleetSettings is the settings copy joined nodes fetch. The daemon
	// replaces it on start and reload; it needs no lock.
	FleetSettings atomic.Pointer[FleetSettings]
	// Reported holds what each report accepted since the fleet incident set
	// step last ran shows about the node's announced incidents and those
	// that it sent without the hub.
	Reported map[string]store.ReportedIncidents
	// Readings is the measurement history of the joined nodes, in its own
	// file. It has its own lock, and mu is never held while that lock is
	// taken, except when the hub starts.
	Readings *store.MeasurementHistory
	// forgetting orders mmwatchdog fleet forget and the saving of readings, which
	// runs without mu. forget holds it for writing while it removes a node's
	// entry and forgets how far the node's readings were received.
	// SaveReadings holds it for reading from its look at the node until the
	// readings are saved. So the readings of a report that was accepted
	// before a forget are saved before the forget or not at all. It is taken
	// before mu and before the history's lock.
	forgetting sync.RWMutex
}

// OpenHub is the hub start entry point: it creates the hub's identity on first
// start, loads the nodes, and adds the hub's own entry.
func OpenHub(dir, address, name string, reportEvery time.Duration, limits config.HubLimits, now time.Time) (*Hub, error) {
	db, e := store.OpenDatabase(filepath.Join(dir, "fleet.db"), "fleet", fleetFormat, store.FleetDatabaseLimit(limits.EventHistoryOfNodesBytes), `
 CREATE TABLE IF NOT EXISTS identity (id INTEGER PRIMARY KEY CHECK(id=1), key BLOB NOT NULL, cert BLOB NOT NULL, secret TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS nodes (name TEXT PRIMARY KEY, data BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS hub_watcher_nodes (name TEXT PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS events (id INTEGER PRIMARY KEY AUTOINCREMENT, node TEXT NOT NULL, at INTEGER NOT NULL, happened INTEGER NOT NULL, data BLOB NOT NULL);
 CREATE INDEX IF NOT EXISTS events_node ON events(node,id);
 CREATE INDEX IF NOT EXISTS events_node_happened ON events(node,happened);
 CREATE INDEX IF NOT EXISTS events_received ON events(at);`)
	if e != nil {
		return nil, e
	}
	// The hub runs without a measurement history of nodes that it cannot
	// use: reports and alerts never depend on it.
	readings, readingsUnusable := store.OpenMeasurementHistory(filepath.Join(dir, store.MeasurementHistoryOfNodesFile), limits.MeasurementHistoryOfNodesSizeLimit.Bytes())
	h := &Hub{Address: address, Name: name, DB: db, Readings: readings, Nodes: map[string]*member{}, Credentials: map[string]string{}, Storage: StorageHealth{}, Reported: map[string]store.ReportedIncidents{}, EventBytes: map[string]int64{}}
	h.Storage.Report(store.MeasurementHistoryOfNodesWrite, readingsUnusable)
	h.SetReportEvery(reportEvery)
	h.SetLimits(limits)
	if e = h.load(now); e != nil {
		h.Close()
		return nil, e
	}
	return h, nil
}

func (h *Hub) load(now time.Time) error {
	var key, cert []byte
	var secret string
	e := h.DB.QueryRow("SELECT key, cert, secret FROM identity WHERE id=1").Scan(&key, &cert, &secret)
	if e == sql.ErrNoRows {
		if key, cert, e = newIdentity(h.Address); e != nil {
			return e
		}
		secret = token()
		_, e = h.DB.Exec("INSERT INTO identity(id,key,cert,secret) VALUES(1,?,?,?)", key, cert, secret)
	}
	if e != nil {
		return e
	}
	parsed, e := x509.ParsePKCS8PrivateKey(key)
	if e != nil {
		return e
	}
	sum := sha256.Sum256(cert)
	h.Cert = tls.Certificate{Certificate: [][]byte{cert}, PrivateKey: parsed}
	h.Code = JoinCode{h.Address, hex.EncodeToString(sum[:]), secret}
	rows, e := h.DB.Query("SELECT name, data FROM nodes")
	if e != nil {
		return e
	}
	defer func() {
		_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	}()
	for rows.Next() {
		var name string
		var b []byte
		m := &member{}
		if e = rows.Scan(&name, &b); e != nil {
			return e
		}
		if e = json.Unmarshal(b, m); e != nil {
			return e
		}
		m.Deadline = now.Add(3 * time.Duration(h.ReportEvery.Load()))
		h.Nodes[name] = m
		if m.Credential != "" {
			h.Credentials[m.Credential] = name
		}
	}
	if e = rows.Err(); e != nil {
		return e
	}
	outageAlerters, e := h.DB.Query("SELECT name FROM hub_watcher_nodes ORDER BY name")
	if e != nil {
		return e
	}
	for outageAlerters.Next() {
		var name string
		if e = outageAlerters.Scan(&name); e != nil {
			_ = outageAlerters.Close() // This cursor only reads data; the scan error already determines the result.
			return e
		}
		h.OutageAlerters = append(h.OutageAlerters, name)
	}
	if e = errors.Join(outageAlerters.Err(), outageAlerters.Close()); e != nil {
		return e
	}
	slices.SortStableFunc(h.OutageAlerters, func(a, b string) int {
		if h.ranksBefore(a, b) {
			return -1
		}
		if h.ranksBefore(b, a) {
			return 1
		}
		return 0
	})
	// Restoring the hub under a new host name must not turn its former self
	// into a joined node. Only the hub's own entry has no credential.
	for name, m := range h.Nodes {
		if name != h.Name && m.Credential == "" {
			if e = h.deleteNode(name, h.OutageAlerters); e != nil {
				return e
			}
			delete(h.Nodes, name)
		}
	}
	// One scan at start counts the events kept for each node; save, prune,
	// and forget keep the counts current afterward.
	sizes, e := h.DB.Query("SELECT node, SUM(length(data)) FROM events GROUP BY node")
	if e != nil {
		return e
	}
	for sizes.Next() {
		var node string
		var bytes int64
		if e = sizes.Scan(&node, &bytes); e != nil {
			_ = sizes.Close() // This cursor only reads data; scan and iteration errors already determine the result.
			return e
		}
		h.EventBytes[node] = bytes
		h.EventBytesTotal += bytes
	}
	if e = errors.Join(sizes.Err(), sizes.Close()); e != nil {
		return e
	}
	// The readings are in a file of their own. A node that fleet.db no longer
	// lists was forgotten, and its readings go with it: also when the hub
	// stopped between the two files. Readings that cannot be removed are a
	// storage problem and no reason for the hub not to start.
	for _, node := range h.Readings.NodesWithReadings() {
		if h.Nodes[node] == nil {
			if e = h.Readings.ForgetNode(node); e != nil {
				h.Storage.Report(store.MeasurementHistoryOfNodesRetention, e)
			}
		}
	}
	if h.Nodes[h.Name] == nil {
		self := &member{Joined: now, Deadline: now.Add(3 * time.Duration(h.ReportEvery.Load()))}
		if e = h.Save(h.Name, self, nil, h.OutageAlerters, now); e != nil {
			return e
		}
		h.Nodes[h.Name] = self
	}
	return nil
}

// newIdentity creates the hub's key and a long-lived self-signed certificate.
// Nodes trust it by fingerprint, so its names and lifetime are not checked.
func newIdentity(address string) ([]byte, []byte, error) {
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		return nil, nil, e
	}
	serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if e != nil {
		return nil, nil, e
	}
	node, _, _ := net.SplitHostPort(address)
	template := x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "MMWatchdog hub " + node}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(100, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if ip := net.ParseIP(node); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{node}
	}
	cert, e := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if e != nil {
		return nil, nil, e
	}
	der, e := x509.MarshalPKCS8PrivateKey(key)
	return der, cert, e
}

// token returns 256 random bits in hex: join secrets and node credentials.
func token() string {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func HashCredential(credential string) string {
	sum := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(sum[:])
}

func (h *Hub) Close() error {
	h.Readings.Close()
	return h.DB.Close()
}

// Save commits one node's entry with its new events, keeping the events of
// all nodes within the size limit, and the hub-outage alerter set when it changed. The
// caller holds h.mu and updates memory only after save succeeds.
func (h *Hub) Save(name string, m *member, events []checksandalerts.Event, outageAlerters []string, now time.Time) error {
	b, e := json.Marshal(m)
	if e != nil {
		return e
	}
	tx, e := h.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("INSERT INTO nodes(name,data) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET data=excluded.data", name, b); e != nil {
		return e
	}
	if e = h.saveOutageAlerters(tx, outageAlerters); e != nil {
		return e
	}
	var added int64
	for _, event := range events {
		if b, e = json.Marshal(event); e != nil {
			return e
		}
		// Receipt time orders retention, so a sender's clock cannot keep events.
		if _, e = tx.Exec("INSERT INTO events(node,at,happened,data) VALUES(?,?,?,?)", name, now.UnixNano(), event.At.UnixNano(), b); e != nil {
			return e
		}
		added += int64(len(b))
	}
	removed, e := h.trimEvents(tx, name, added)
	if e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	if added > 0 {
		h.EventBytes[name] += added
		h.EventBytesTotal += added
	}
	for node, bytes := range removed {
		h.forgetEventBytes(node, bytes)
	}
	return nil
}

// forgetEventBytes takes bytes of removed events out of a node's count.
// The caller holds h.mu.
func (h *Hub) forgetEventBytes(node string, bytes int64) {
	h.EventBytesTotal -= bytes
	if h.EventBytes[node] -= bytes; h.EventBytes[node] <= 0 {
		delete(h.EventBytes, node)
	}
}

// trimEvents removes events in tx until the events of all nodes fit
// event_history_of_nodes_content_size_limit again, counting the added bytes that tx
// inserted for the node name. It always removes the oldest events of the
// node that has the most, and only until that node is level with the next
// one, so one noisy node cannot push out the events of the others. It
// returns the bytes removed for each node. The caller holds h.mu.
func (h *Hub) trimEvents(tx *sql.Tx, name string, added int64) (map[string]int64, error) {
	limit := h.Limits.Load().EventHistoryOfNodesBytes
	removed := map[string]int64{}
	total := h.EventBytesTotal + added
	size := func(node string) int64 {
		bytes := h.EventBytes[node] - removed[node]
		if node == name {
			bytes += added
		}
		return bytes
	}
	for total > limit {
		// The node with the most, the first by name among equals, and the
		// size of the next one.
		largest, most, next := name, size(name), int64(0)
		for node := range h.EventBytes {
			if bytes := size(node); bytes > most || (bytes == most && node < largest) {
				largest, most = node, bytes
			}
		}
		for node := range h.EventBytes {
			if node != largest {
				next = max(next, size(node))
			}
		}
		if largest != name {
			next = max(next, size(name))
		}
		freed, e := removeOldestEvents(tx, largest, max(1, min(total-limit, most-next)))
		if e != nil {
			return nil, e
		}
		if freed == 0 {
			return nil, fmt.Errorf("the events of %s are counted as %d bytes, but none are stored", largest, most)
		}
		removed[largest] += freed
		total -= freed
	}
	return removed, nil
}

// removeOldestEvents deletes the oldest events of node in tx until at least
// want bytes are gone or the node has none left, and returns the bytes
// removed.
func removeOldestEvents(tx *sql.Tx, node string, want int64) (int64, error) {
	var freed int64
	for freed < want {
		rows, e := tx.Query("SELECT id, length(data) FROM events WHERE node=? ORDER BY id LIMIT 256", node)
		if e != nil {
			return 0, e
		}
		last, found := int64(0), false
		for freed < want && rows.Next() {
			var size int64
			if e = rows.Scan(&last, &size); e != nil {
				_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
				return 0, e
			}
			freed, found = freed+size, true
		}
		if e = errors.Join(rows.Err(), rows.Close()); e != nil {
			return 0, e
		}
		if !found {
			break
		}
		if _, e = tx.Exec("DELETE FROM events WHERE node=? AND id<=?", node, last); e != nil {
			return 0, e
		}
	}
	return freed, nil
}

// saveOutageAlerters writes the whole hub-outage alerter set in transaction tx when it changed.
func (h *Hub) saveOutageAlerters(tx *sql.Tx, outageAlerters []string) error {
	if slices.Equal(outageAlerters, h.OutageAlerters) {
		return nil
	}
	if _, e := tx.Exec("DELETE FROM hub_watcher_nodes"); e != nil {
		return e
	}
	for _, name := range outageAlerters {
		if _, e := tx.Exec("INSERT INTO hub_watcher_nodes(name) VALUES(?)", name); e != nil {
			return e
		}
	}
	return nil
}

// Prune is the retention tick: it removes events older than
// keep_event_history_of_nodes_for and readings older than
// keep_measurement_history_of_nodes_for. The readings have their own lock
// and go in steps, so that removing many of them holds up no report.
func (h *Hub) Prune(now time.Time) {
	h.Mu.Lock()
	limits := h.Limits.Load()
	h.Storage.Report("fleet retention", h.pruneEvents(now.Add(-limits.KeepEventHistoryOfNodesFor)))
	h.Mu.Unlock()
	e := h.Readings.Prune(now, limits.KeepMeasurementHistoryOfNodesFor.Value())
	h.Mu.Lock()
	h.Storage.Report(store.MeasurementHistoryOfNodesRetention, e)
	h.Mu.Unlock()
}

// HubRemoveEventsReceivedBefore removes the events that the hub received
// before a time. It runs once a minute under the hub's lock, so it finds the
// old events through the index events_received and never reads the others.
const HubRemoveEventsReceivedBefore = "DELETE FROM events WHERE at < ? RETURNING node, length(data)"

// pruneEvents removes the events received before cutoff and takes them out
// of the nodes' counts. The caller holds h.mu.
func (h *Hub) pruneEvents(cutoff time.Time) error {
	tx, e := h.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	rows, e := tx.Query(HubRemoveEventsReceivedBefore, cutoff.UnixNano())
	if e != nil {
		return e
	}
	removed := map[string]int64{}
	for rows.Next() {
		var node string
		var size int64
		if e = rows.Scan(&node, &size); e != nil {
			_ = rows.Close() // Scanning already failed, so this transaction is rejected rather than accepted after cursor cleanup.
			return e
		}
		removed[node] += size
	}
	if e = errors.Join(rows.Err(), rows.Close()); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	for node, bytes := range removed {
		h.forgetEventBytes(node, bytes)
	}
	return nil
}

// SetLimits applies the hub's storage limits on start and reload. The size
// of fleet.db is applied when it opens, so a reload does not change it.
func (h *Hub) SetLimits(limits config.HubLimits) { h.Limits.Store(&limits) }

// SetReportEvery applies the hub's report_every on start and reload. Existing
// members keep their deadline until their next receipt.
func (h *Hub) SetReportEvery(every time.Duration) {
	h.ReportEvery.Store(int64(every))
}

// State is reporting, waiting_for_first_report or silent. A node that joined, or a hub that
// started, less than three intervals ago waits for its first report. The
// caller holds h.mu.
func (h *Hub) State(m *member, now time.Time) string {
	if now.Before(m.Deadline) {
		//nolint:nilaway // Hub-outage alerter selection uses existing member keys under the hub mutex; loading and joining insert allocated members.
		if m.Reported {
			return "reporting"
		}
		return "waiting_for_first_report"
	}
	return "silent"
}

func RemoteHost(r *http.Request) string {
	node, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		return r.RemoteAddr
	}
	return node
}

// JoinRequest is what a node sends to join: its name, the join secret, and
// its settings for the fleet.
type JoinRequest struct {
	Name   string `json:"name"`
	Secret string `json:"secret"`
	config.NodeFleetSettings
}

// Join is the join entry point. A name may be taken over only from a silent
// node; any other claim is refused and recorded as a conflict.
func (h *Hub) Join(name, secret string, settings config.NodeFleetSettings, address string, now time.Time) (string, int, error) {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	if subtle.ConstantTimeCompare([]byte(secret), []byte(h.Code.Secret)) != 1 {
		h.Refused = RefusedJoins{h.Refused.Count + 1, now, address}
		return "", 403, fmt.Errorf("invalid join code")
	}
	if !serverNamePattern.MatchString(name) {
		return "", 400, fmt.Errorf("node name must be a host name")
	}
	if problem := settings.Problem(); problem != "" {
		return "", 400, fmt.Errorf("join %s", problem)
	}
	existing := h.Nodes[name]
	joined := len(h.Nodes)
	if _, hubIsANode := h.Nodes[h.Name]; hubIsANode {
		joined--
	}
	if limit := h.Limits.Load().JoinedNodeLimit; existing == nil && name != h.Name && joined >= limit {
		return "", 403, fmt.Errorf("the fleet hub has %d joined nodes, its joined_node_limit; to let more nodes join, raise joined_node_limit under fleet_hub_role in the hub's mmwatchdog_node_settings.yml and reload MMWatchdog on the hub", joined)
	}
	if name == h.Name || (existing != nil && h.State(existing, now) != "silent") {
		if existing != nil {
			claimed := *existing
			claimed.Conflict = &Conflict{address, now}
			e := h.Save(name, &claimed, nil, h.OutageAlerters, now)
			if h.Storage.Report(fleetCommit, e); e == nil {
				*existing = claimed
			}
		}
		return "", 409, fmt.Errorf("node name %q is in use. A new node can use the name only after the fleet hub marks the current node as silent", name)
	}
	credential := token()
	m := &member{Credential: HashCredential(credential), Joined: now, Address: address, NodeFleetSettings: settings, Deadline: now.Add(3 * time.Duration(h.ReportEvery.Load()))}
	event := checksandalerts.Event{ID: incidents.ID(), Node: name, Type: "node_joined", At: now, Message: "joined from " + address}
	if existing != nil {
		m.Replaced = now
		event.Type, event.Message = "node_replaced", "a new node from "+address+" replaced the silent node"
	}
	if e := h.Save(name, m, []checksandalerts.Event{event}, h.OutageAlerters, now); e != nil {
		h.Storage.Report(fleetCommit, e)
		return "", 503, fmt.Errorf("join could not be saved")
	}
	h.Storage.Report(fleetCommit, nil)
	if existing != nil {
		delete(h.Credentials, existing.Credential)
	}
	h.Nodes[name], h.Credentials[m.Credential] = m, name
	return credential, 200, nil
}

// JoinCode returns the current join code.
func (h *Hub) JoinCode() JoinCode {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	return h.Code
}

// Rotate is the join-code rotation entry point: a new secret for new joins.
// Joined nodes keep their credentials.
func (h *Hub) Rotate() (JoinCode, error) {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	secret := token()
	_, e := h.DB.Exec("UPDATE identity SET secret=? WHERE id=1", secret)
	if h.Storage.Report(fleetCommit, e); e != nil {
		return JoinCode{}, fmt.Errorf("the new join code could not be saved")
	}
	h.Code.Secret = secret
	return h.Code, nil
}

// Forget is the forget entry point: it deletes a node, its events, its
// credential, and its readings, and returns when the node joined. A forgotten
// hub-outage alerter leaves a place open until the next report acceptance chooses
// another. The readings have their own lock and go in steps after the node's
// entry is gone, so that a node with a long history holds up no report.
// Readings that cannot be removed are a storage problem, and the next start
// of the hub removes them.
func (h *Hub) Forget(name string) (time.Time, int, error) {
	h.forgetting.Lock()
	joined, status, e := h.forgetEntry(name)
	var last int64
	var readingsError error
	if e == nil {
		last, readingsError = h.Readings.ForgetReceived(name)
	}
	h.forgetting.Unlock()
	if e != nil {
		return joined, status, e
	}
	if readingsError == nil {
		readingsError = h.Readings.RemoveReadingsOfNode(name, last)
	}
	h.Mu.Lock()
	h.Storage.Report(store.MeasurementHistoryOfNodesRetention, readingsError)
	h.Mu.Unlock()
	return joined, status, nil
}

// forgetEntry deletes a node's entry, its events, and its credential.
func (h *Hub) forgetEntry(name string) (time.Time, int, error) {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	m := h.Nodes[name]
	switch {
	case name == h.Name:
		return time.Time{}, 400, fmt.Errorf("the hub cannot forget itself")
	case m == nil:
		return time.Time{}, 404, fmt.Errorf("unknown node %q", name)
	}
	outageAlerters := slices.DeleteFunc(slices.Clone(h.OutageAlerters), func(outageAlerter string) bool { return outageAlerter == name })
	e := h.deleteNode(name, outageAlerters)
	if h.Storage.Report(fleetCommit, e); e != nil {
		return time.Time{}, 503, fmt.Errorf("forgetting %s could not be saved", name)
	}
	delete(h.Nodes, name)
	delete(h.Credentials, m.Credential)
	h.forgetEventBytes(name, h.EventBytes[name])
	h.OutageAlerters = outageAlerters
	h.Reported[name] = store.ReportedIncidents{Forgotten: true}
	return m.Joined, 200, nil
}

// deleteNode removes a node's entry and events, and saves the hub-outage alerter set,
// in one transaction. The caller holds h.mu.
func (h *Hub) deleteNode(name string, outageAlerters []string) error {
	tx, e := h.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("DELETE FROM nodes WHERE name=?", name); e != nil {
		return e
	}
	if _, e = tx.Exec("DELETE FROM events WHERE node=?", name); e != nil {
		return e
	}
	if e = h.saveOutageAlerters(tx, outageAlerters); e != nil {
		return e
	}
	return tx.Commit()
}

// Authenticate resolves a request's credential to its node's name. Identity
// comes from the credential alone, before the body is read.
func (h *Hub) Authenticate(r *http.Request) (string, bool) {
	credential, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !tokenPattern.MatchString(credential) {
		return "", false
	}
	h.Mu.Lock()
	defer h.Mu.Unlock()
	name, known := h.Credentials[HashCredential(credential)]
	return name, known
}

// Valid reports whether the sequence numbers of a reply are ones that a hub
// can hold. A node checks the reply of another machine before it uses them:
// it continues its own numbers after them (send).
func (r ReportReply) Valid() bool {
	return r.EventsCursor >= 0 && r.EventsCursor <= MaxSequenceNumber && r.ReadingsCursor >= -1 && r.ReadingsCursor <= MaxSequenceNumber
}

type ReportReply struct {
	// EventsCursor and ReadingsCursor are the sequence numbers of the last
	// event and of the last reading that the hub holds from the node: the
	// node's next report carries what follows them.
	EventsCursor         int64  `json:"events_cursor"`
	ReadingsCursor       int64  `json:"readings_cursor"`
	FleetSettingsVersion string `json:"fleet_settings_version"`
	OutageAlerter        bool   `json:"is_hub_outage_alerter_node"`
	// IncidentsKnownToTheHub lists the incidents that the report carried as
	// sent without the hub and that the hub's fleet incident set of their
	// destination now holds; the node stops carrying them.
	IncidentsKnownToTheHub []store.IncidentKnownToTheHub `json:"incidents_known_to_the_hub,omitempty"`
}

// Accept is the report acceptance entry point. It saves a newer report and the
// events that follow the hub's cursor, chooses the hub-outage alerters, and replies with
// the new cursor and whether this node is a hub-outage alerter. An authenticated
// current report counts as a receipt even when it cannot be saved.
func (h *Hub) Accept(name string, r *NodeStatusReport, now time.Time) (ReportReply, int, error) {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	m := h.Nodes[name]
	if m == nil {
		return ReportReply{}, 401, fmt.Errorf("unknown node credential")
	}
	switch {
	case r.StartID == m.StartID && r.Seq <= m.Seq, r.StartID != m.StartID && r.StartID == m.ReplacedStart:
		return ReportReply{EventsCursor: m.Cursor}, 409, fmt.Errorf("node status report is not newer than the saved report")
	}
	if h.State(m, now) != "reporting" {
		m.Since = now
	}
	wasPreferred := m.Preferred
	m.Received, m.Preferred = now, r.PreferOutageAlerter
	m.Deadline, m.Reported = now.Add(3*time.Duration(h.ReportEvery.Load())), true
	c := *m
	if r.StartID != c.StartID {
		c.ReplacedStart, c.StartID = c.StartID, r.StartID
	}
	c.Seq, c.Offset = r.Seq, r.Sent.Sub(now)
	if r.History != c.History {
		// Another database: its sequence numbers start over.
		c.History, c.Cursor = r.History, 0
	}
	var events []checksandalerts.Event
	// Events count only when they continue from the hub's cursor; otherwise the
	// reply tells the node where to continue.
	if r.After == c.Cursor && len(r.Events) > 0 {
		if lost := r.Events[0].Seq - c.Cursor - 1; lost > 0 {
			events = append(events, checksandalerts.Event{ID: incidents.ID(), Node: name, Type: "history_gap", At: now, Message: fmt.Sprintf("%d events were deleted on %s, to stay within its event history limits (keep_event_history_for, event_history_content_size_limit), before they reached the hub", lost, name)})
		}
		events = append(events, r.Events...)
		c.Cursor = r.Events[len(r.Events)-1].Seq
	}
	// The saved report holds neither the events nor the readings that came
	// with it: each has its own store.
	saved := *r
	saved.Events, saved.Readings = nil, nil
	c.NodeStatusReport = &saved
	// Rank only when a place is open or this report can change the set.
	outageAlerters := h.OutageAlerters
	rerank := len(outageAlerters) < 2
	for _, outageAlerter := range outageAlerters {
		w := h.Nodes[outageAlerter]
		if w == nil || h.State(w, now) != "reporting" || r.PreferOutageAlerter && !w.Preferred || name == outageAlerter && wasPreferred && !r.PreferOutageAlerter {
			rerank = true
		}
	}
	if rerank {
		outageAlerters = h.chooseOutageAlerters(now)
	}
	if e := h.Save(name, &c, events, outageAlerters, now); e != nil {
		h.Storage.Report(fleetCommit, e)
		return ReportReply{EventsCursor: m.Cursor}, 503, fmt.Errorf("report could not be saved")
	}
	h.Storage.Report(fleetCommit, nil)
	*m, h.OutageAlerters = c, outageAlerters
	h.Reported[name] = announcedIncidents(name, r)
	return ReportReply{EventsCursor: c.Cursor, OutageAlerter: slices.Contains(outageAlerters, name)}, 200, nil
}

// SaveReadings saves the readings that came with a joined node's report in
// the hub's measurement history of nodes, and returns the sequence number of
// the last reading that the hub now holds from the node, for the reply. It
// runs after accept has saved the report, and without the hub's lock: the
// history has its own, so a node that sends many readings holds up no other
// node's report. Readings that cannot be saved are a storage problem of the
// hub and no reason to refuse the report: the reply tells the node how far
// the hub is, and the node sends the rest again. The hub's own readings stay
// in its own measurement history: only the handler of a joined node's report
// calls this.
//
// Between accept and this call the hub's lock was free. The node may have
// been forgotten since, and another node may have joined under its name. So
// the readings are saved only while the node's entry still names the daemon
// that sent the report; otherwise the reply says -1, after which a node sends
// no readings until a reply says how far the hub is.
func (h *Hub) SaveReadings(name string, r *NodeStatusReport, now time.Time) int64 {
	h.forgetting.RLock()
	defer h.forgetting.RUnlock()
	h.Mu.Lock()
	m := h.Nodes[name]
	current := m != nil && m.StartID == r.StartID
	h.Mu.Unlock()
	if !current {
		return -1
	}
	held, e := h.Readings.AddFromNode(name, r.MeasurementHistoryID, r.ReadingsAfter, r.Readings, now)
	h.Mu.Lock()
	h.Storage.Report(store.MeasurementHistoryOfNodesWrite, e)
	h.Mu.Unlock()
	return held
}

// chooseOutageAlerters returns the first two reporting nodes in rank order:
// preferred nodes, current hub-outage alerters, then longest continuous reporting.
// The hub never watches itself. The caller holds h.mu.
func (h *Hub) chooseOutageAlerters(now time.Time) []string {
	outageAlerters := []string{}
	for _, name := range checksandalerts.Names(h.Nodes) {
		if name == h.Name || h.State(h.Nodes[name], now) != "reporting" {
			continue
		}
		place := len(outageAlerters)
		for i, outageAlerter := range outageAlerters {
			if h.ranksBefore(name, outageAlerter) {
				place = i
				break
			}
		}
		outageAlerters = slices.Insert(outageAlerters, place, name)
		if len(outageAlerters) > 2 {
			outageAlerters = outageAlerters[:2]
		}
	}
	return outageAlerters
}
func (h *Hub) ranksBefore(a, b string) bool {
	ma, mb := h.Nodes[a], h.Nodes[b]
	switch {
	case ma.Preferred != mb.Preferred: //nolint:nilaway // Hub-outage alerter selection uses existing member keys under the hub mutex; loading and joining insert allocated members.
		return ma.Preferred
	case slices.Contains(h.OutageAlerters, a) != slices.Contains(h.OutageAlerters, b):
		return slices.Contains(h.OutageAlerters, a)
	}
	return ma.Since.Before(mb.Since) //nolint:nilaway // Hub-outage alerter selection uses existing member keys under the hub mutex; loading and joining insert allocated members.
}

// FleetMember is what the daemon takes from a joined node when it starts:
// when it joined, its last report, the whole-node window that report showed,
// and the node's settings for the fleet as the hub knows them
// (member.fleetSettings), from which its silence check takes its severity
// and destinations and the raise step its node groups.
type FleetMember struct {
	Joined, Received, Until time.Time
	config.NodeFleetSettings
}

func (h *Hub) Members() map[string]FleetMember {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	members := map[string]FleetMember{}
	for name, m := range h.Nodes {
		if name == h.Name {
			continue
		}
		f := FleetMember{Joined: m.Joined, Received: m.Received, NodeFleetSettings: m.fleetSettings()}
		if m.NodeStatusReport != nil {
			f.Until = m.NodeStatusReport.Maintenance[checksandalerts.EveryCheckGroup].Until
		}
		members[name] = f
	}
	return members
}

// FleetNode is one node in the fleet view. Report is its latest saved
// report; the fleet list leaves out its checks.
type FleetNode struct {
	StorageError  string `json:"storage_error,omitempty"`
	Name          string `json:"name"`
	State         string `json:"state"`
	ExitCode      int    `json:"exit_code"`
	Hub           bool   `json:"has_fleet_hub_role,omitempty"`
	OutageAlerter bool   `json:"is_hub_outage_alerter_node,omitempty"`
	// PreferOutageAlerter is the node's prefer_this_node_as_hub_outage_alerter setting.
	PreferOutageAlerter bool `json:"prefer_this_node_as_hub_outage_alerter,omitempty"`
	// NodeGroups are the node groups that the node is in, as its
	// this_node_is_in_node_groups states them: from its latest saved report,
	// or from its join before the first one.
	NodeGroups       []string                `json:"this_node_is_in_node_groups"`
	Joined           time.Time               `json:"joined_at"`
	Address          string                  `json:"joined_from_address,omitempty"`
	LastReport       time.Time               `json:"last_report_received_at,omitempty"`
	ClockOffset      time.Duration           `json:"clock_offset_ns,omitempty"`
	Replaced         time.Time               `json:"replaced_silent_node_of_same_name_at,omitempty"`
	Conflict         *Conflict               `json:"conflict,omitempty"`
	NodeStatusReport *NodeStatusReport       `json:"report,omitempty"`
	Events           []checksandalerts.Event `json:"events,omitempty"`
}

// Fleet is the fleet view: every node and the worst exit code among them.
// With NodeGroup it is the view of one node group: only the nodes that are
// in it, and the worst exit code among those.
type Fleet struct {
	ExitCode       int          `json:"exit_code"`
	Address        string       `json:"hub_address"`
	OutageAlerters []string     `json:"hub_outage_alerter_nodes"`
	NodeGroup      string       `json:"node_group,omitempty"`
	Nodes          []FleetNode  `json:"nodes"`
	RefusedJoins   RefusedJoins `json:"refused_joins"`
	StorageError   string       `json:"storage_error"`
	// MeasurementHistoryOfNodes says how much measurement history of the
	// joined nodes the hub keeps and how far back it reaches now.
	MeasurementHistoryOfNodes store.MeasurementHistoryOfNodesStatus `json:"measurement_history_of_nodes"`
}

// View describes one node. A reporting node exits with its own code, a
// waiting one with 0, and a silent one with 1. The caller holds h.mu.
func (h *Hub) View(name string, m *member, now time.Time) FleetNode {
	s := FleetNode{Name: name, State: h.State(m, now), Hub: name == h.Name, OutageAlerter: slices.Contains(h.OutageAlerters, name), PreferOutageAlerter: m.Preferred, Joined: m.Joined, Address: m.Address, LastReport: m.Received, Replaced: m.Replaced, Conflict: m.Conflict, NodeStatusReport: m.NodeStatusReport}
	s.NodeGroups = append([]string{}, m.fleetSettings().ThisNodeIsInNodeGroups...)
	if m.Offset > clockOffsetShown || m.Offset < -clockOffsetShown {
		s.ClockOffset = m.Offset
	}
	switch {
	case s.State == "silent":
		s.ExitCode = 1
	case s.State == "reporting":
		//nolint:nilaway // View callers hold the hub mutex and pass existing allocated members; only their saved report may be nil.
		if report := m.NodeStatusReport; report == nil {
			s.ExitCode = 3 // its reports arrive but none could be saved
		} else {
			s.ExitCode = report.Summary.ExitCode
			if len(report.Delivery.FailingDestinations) > 0 {
				s.ExitCode = incidents.HealthExitCode(s.ExitCode, 3)
			}
		}
	}
	return s
}

func (h *Hub) Problems() []string {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	return h.Storage.Problems()
}

// Fleet is the fleet view of every node.
func (h *Hub) Fleet(now time.Time) Fleet {
	f, _ := h.FleetOfNodeGroup(now, "")
	return f
}

// FleetOfNodeGroup is the fleet view of the nodes of one node group, or of
// every node when nodeGroup is "". A node group exists because nodes name
// it: the hub keeps no list of node groups, so a name that no node states is
// an error that lists the node groups of the fleet.
func (h *Hub) FleetOfNodeGroup(now time.Time, nodeGroup string) (Fleet, error) {
	// The measurement history has its own lock. It is read before the hub's
	// lock is taken, so that a busy history never holds up a report.
	history := h.measurementHistoryOfNodesStatus(now)
	h.Mu.Lock()
	defer h.Mu.Unlock()
	f := Fleet{Address: h.Address, OutageAlerters: append([]string{}, h.OutageAlerters...), NodeGroup: nodeGroup, Nodes: []FleetNode{}, RefusedJoins: h.Refused, StorageError: strings.Join(h.Storage.Problems(), "; ")}
	f.MeasurementHistoryOfNodes = history
	nodeGroups := map[string]bool{}
	for _, name := range checksandalerts.Names(h.Nodes) {
		s := h.View(name, h.Nodes[name], now)
		for _, group := range s.NodeGroups {
			nodeGroups[group] = true
		}
		if nodeGroup != "" && !slices.Contains(s.NodeGroups, nodeGroup) {
			continue
		}
		if s.NodeStatusReport != nil {
			brief := *s.NodeStatusReport
			brief.Checks = nil
			s.NodeStatusReport = &brief
		}
		f.ExitCode = incidents.HealthExitCode(f.ExitCode, s.ExitCode)
		f.Nodes = append(f.Nodes, s)
	}
	if nodeGroup != "" && !nodeGroups[nodeGroup] {
		if len(nodeGroups) == 0 {
			return Fleet{}, fmt.Errorf("no node is in node group %q, because no node in the fleet states a node group. Each node states its node groups with this_node_is_in_node_groups in the node's own mmwatchdog_node_settings.yml", nodeGroup)
		}
		return Fleet{}, fmt.Errorf("no node is in a node group named %q; the node groups of this fleet are %s", nodeGroup, strings.Join(checksandalerts.Names(nodeGroups), ", "))
	}
	if f.StorageError != "" {
		f.ExitCode = incidents.HealthExitCode(f.ExitCode, 3)
	}
	return f, nil
}

// Node describes one node with its checks and most recent events.
func (h *Hub) Node(name string, now time.Time) (FleetNode, error) {
	return h.NodeEventsInTime(name, now, time.Time{}, time.Time{})
}

// NodeEventsInTime describes a node with all its retained events in the
// inclusive span, or its latest 100 events when no span is given.
func (h *Hub) NodeEventsInTime(name string, now, since, until time.Time) (FleetNode, error) {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	m := h.Nodes[name]
	if m == nil {
		return FleetNode{}, fmt.Errorf("unknown node %q", name)
	}
	s := h.View(name, m, now)
	s.Events = []checksandalerts.Event{}
	if !since.IsZero() && since.After(until) {
		return s, nil
	}
	query, args := "SELECT id, data FROM events WHERE node=? ORDER BY id DESC LIMIT 100", []any{name}
	if !since.IsZero() {
		// Retention uses receipt time; selection uses the event's own time.
		query = "SELECT id, data FROM events WHERE node=? AND happened>=? AND happened<=? ORDER BY id DESC"
		args = append(args, since.UnixNano(), until.UnixNano())
	}
	rows, e := h.DB.Query(query, args...)
	if e != nil {
		return s, e
	}
	defer func() {
		_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	}()
	for rows.Next() {
		var id int64
		var b []byte
		var event checksandalerts.Event
		if e = rows.Scan(&id, &b); e == nil {
			e = json.Unmarshal(b, &event)
		}
		if e != nil {
			return s, e
		}
		s.Events = append(s.Events, event)
	}
	return s, rows.Err()
}

// measurementHistoryOfNodesStatus is the hub's measurement history of nodes
// for the fleet view. The caller does not hold h.mu: the history has its own
// lock, and the hub's lock never waits for it.
func (h *Hub) measurementHistoryOfNodesStatus(now time.Time) store.MeasurementHistoryOfNodesStatus {
	limits := h.Limits.Load()
	reach := h.Readings.Reach(now, limits.KeepMeasurementHistoryOfNodesFor.Value())
	s := store.MeasurementHistoryOfNodesStatus{KeepMeasurementHistoryOfNodesFor: limits.KeepMeasurementHistoryOfNodesFor, MeasurementHistoryOfNodesSizeLimit: limits.MeasurementHistoryOfNodesSizeLimit, CutShortByMeasurementHistoryOfNodesSizeLimit: reach.CutShortBySizeLimit}
	if !reach.Oldest.IsZero() {
		s.OldestReadingAt, s.NewestReadingAt = &reach.Oldest, &reach.Newest
	}
	return s
}

// StorageHealth holds the latest error of each storage operation, by name.
// Success clears only that operation's entry. A problem is logged when it
// starts and when its text changes, not at every failed attempt: MMWatchdog keeps
// running with it, and status shows it for as long as it lasts.
type StorageHealth map[string]string

func (h StorageHealth) Report(operation string, err error) {
	if err == nil {
		delete(h, operation)
		return
	}
	if h[operation] != err.Error() {
		log.Printf("%s: %v", operation, err)
	}
	h[operation] = err.Error()
}

func (h StorageHealth) Problems() []string {
	problems := []string{}
	for _, operation := range checksandalerts.Names(h) {
		problems = append(problems, operation+": "+h[operation])
	}
	return problems
}
