// Package daemon is the node monitoring process: the one systemd-managed
// mmwatchdog process on each node. Daemon owns the configuration in force, the
// snapshot of the checks, the one lock, and the two commit paths; it runs
// the checks on their schedule, acts on recovery, delivers alerts, serves
// the local API and the fleet's HTTP handlers, and runs the loops of the
// fleet. The files carry the names of the features whose state, store, or
// fleet parts live in the other packages of internal/.
package daemon

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
)

// The rules this file implements are in docs/implementation-rules.md.
type Daemon struct {
	// Build identifies the executable in fleet reports. Set it before Run.
	Build fleet.Build
	// withoutJoinNotices leaves out the notice of each join. Only tests set
	// it, so that the alerts that they count are the ones they study.
	withoutJoinNotices bool
	// now is the daemon's one clock, time.Now, which tests replace: every
	// entry point takes its time from it. Its times carry Go's monotonic
	// reading, on which every wait counts; see "Time is a parameter" in
	// docs/implementation-rules.md.
	now func() time.Time
	// started is when this daemon started, on its clock: the retention
	// steps count from it (retentionNow).
	started time.Time
	lock    *os.File
	store   *store.Store
	path    string
	workers sync.WaitGroup
	// history is the node's measurement history. It has its own lock, which
	// is taken after mu.
	history *store.MeasurementHistory
	// hub is the fleet store when this daemon is the hub. It has its own lock
	// and is never used while holding mu.
	hub *fleet.Hub
	// startID identifies this daemon start in fleet reports.
	startID string
	rep     fleet.Reporter
	// mmhotlineDestinations holds one connection per destination name.
	mmhotlineDestinations map[string]*mmhotlineDestinationState
	// checkInNow asks for check-ins at once, after a reload.
	checkInNow chan struct{}
	// runRetentionNow asks the notifications worker for its retention step
	// at once. Every reload sends it, so that a shorter retention time
	// applies right after the reload and not up to a minute later.
	runRetentionNow chan struct{}
	// joinNow asks the reporter to check its credential at once. Every
	// reload with a join code sends it. The reporter reports at once only
	// when it holds no credential for that hub, so that the node joins
	// without waiting for its next report.
	joinNow chan struct{}
	// reportNow asks the reporter for a report at once, and takes back the
	// error of sending it, nil once the hub accepted it: a maintenance
	// command on a joined node returns only once the hub has the window.
	reportNow chan chan error
	// reporting is true while the reporter runs, so that a maintenance
	// command does not wait for a report that nobody sends, such as while
	// the daemon stops.
	reporting atomic.Bool
	// heartbeatSocket is the socket through which jobs on this node send
	// heartbeats. It has its own lock.
	heartbeatSocket heartbeatSocket
	// mu guards everything below. State commits happen while holding it;
	// check execution, systemd commands and webhook requests do not.
	mu       sync.Mutex
	config   *config.Config
	tokens   map[string]string
	joinCode *fleet.JoinCode
	link     *fleet.HubLink
	// fleetSettings is a joined node's copy of the fleet settings, or nil
	// before it has one.
	fleetSettings *fleet.FleetSettings
	// latestHubAttemptError is the error of a joined node's latest attempt
	// to deliver an alert to the hub, or nil when that attempt succeeded or
	// none was made. While it is set, older alerts fall back without an
	// attempt each, and the fallback's log line names it.
	latestHubAttemptError error
	// alert groups is the hub's alert grouping memory.
	alertGroups fleet.AlertGroupingState
	// setAsideUnsettled is set on the hub while the set-aside entries of
	// the configuration in force could not be settled against the open
	// node incidents; the fleet incident set step tries again.
	setAsideUnsettled bool
	// members is the hub's copy of the fleet membership: when each joined
	// node joined. watching is set on the node the hub chose as its hub-outage alerter.
	members  map[string]fleet.FleetMember
	watching bool
	snapshot incidents.Snapshot
	pending  []checksandalerts.Event
	storage  fleet.StorageHealth
	checks   map[string]*checkRuntime
	policies map[string]*incidents.Policy
	epoch    int
	running  int
	gates    map[string]chan struct{}
	slots    chan struct{}
	// runsInProgress holds, by check, the run whose module has not returned
	// yet. A run that MMWatchdog stopped waiting for stays here until it ends, and
	// no other run of that check starts while it is here: see execute.
	runsInProgress map[string]*runInProgress
	recovering     map[string]bool
	// stoppedUnitSeenAt holds, for a unit that MMWatchdog found stopped and did not
	// restart, the time of the failed observation that called for the
	// restart. MMWatchdog asks systemd about the unit again at the next failed
	// observation of that check, not at every observation on the node.
	stoppedUnitSeenAt map[string]time.Time
	// deliveries holds the destinations with a delivery in flight.
	deliveries map[string]bool
	// fromMemory holds, while commits fail, how the delivery of each alert
	// in pending goes that the notifications worker sends straight from
	// memory (deliverFromMemory).
	fromMemory map[memoryDeliveryKey]*memoryDelivery
	// acceptedFromMemory holds the deliveries from memory that their
	// destination accepted, until a commit succeeds: it records a joined
	// node's direct send among them for the node's reports
	// (store.AlertRoute.AcceptedFromMemory).
	acceptedFromMemory []store.Delivery
	// deliveriesFinished counts, by destination, the deliveries that have
	// finished. The notifications worker reads the queue without d.mu, so a
	// delivery that finishes after that read can be of a row in what it read.
	deliveriesFinished map[string]int
	// nextAttempts holds, by destination, when the notifications worker
	// next tries to deliver the head of the destination's queue, on the
	// monotonic clock (attemptDue).
	nextAttempts map[string]queueHead
	// hubAlert is, on a joined node, the oldest alert queued for the hub,
	// with when it happened on the monotonic clock (hubAlertHappened).
	hubAlert queueHead
	// noticeAttempts holds, for each notice that an mmwatchdog send-notice command
	// waits for, by event ID, the result of the first delivery attempt to
	// each destination that has had one: nil when it was delivered.
	noticeAttempts map[string]map[string]error
	// destinationHealth keeps the verification schedule and notice lifecycle.
	destinationHealth map[string]*destinationHealth
}

// checkRuntime is the in-memory execution state of one check.
type checkRuntime struct {
	def config.Definition
	// A result counts only when its ticket matches epoch. The epoch advances
	// when the definition changes, maintenance starts or ends, a restart
	// command for a systemd unit of its check group completes, a recovery
	// command of its check group or of a check group that shares one of its
	// units completes, or the daemon starts.
	epoch     int
	fresh     bool // a result was accepted in the current epoch
	next      time.Time
	scheduled time.Time // start of the latest scheduled execution
	finished  time.Time // end of the latest scheduled execution
	running   bool
	memory    *checksandalerts.CheckMemory // owned by the check's gate while in use
	// until ends the whole-node window a joined node last reported.
	until time.Time
}

// needsFresh reports that an executed check has no accepted result in its
// current epoch. Heartbeat receipts and deadlines are durable evidence.
func (rc *checkRuntime) needsFresh() bool {
	return rc.def.Heartbeat == nil && !rc.fresh //nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
}

const stateCommit = "state commit"

func NewDaemon(path string) (*Daemon, error) { return newDaemon(path, time.Now) }

// newDaemon is NewDaemon with clock as the daemon's clock.
func newDaemon(path string, clock func() time.Time) (*Daemon, error) {
	c, e := config.Load(path)
	if e != nil {
		return nil, e
	}
	for _, problem := range c.SecretFilePermissionErrors() {
		log.Printf("warning: %v", problem)
	}
	if e := ValidateRouting(c); e != nil {
		return nil, e
	}
	captureDestinationCredentials(c)
	tokens, e := loadTokens(c)
	if e != nil {
		return nil, e
	}
	code, e := fleet.LoadJoinCode(c)
	if e != nil {
		return nil, e
	}
	var served *fleet.FleetSettings
	if c.FleetHubRole != nil {
		if served, e = fleet.NewFleetSettings(c); e != nil {
			return nil, e
		}
	}
	if e := store.PrepareStateDir(c.StateDir); e != nil {
		return nil, e
	}
	lock, e := os.OpenFile(filepath.Join(c.StateDir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		_ = lock.Close() // This descriptor carries only the daemon lock and has no pending writes.
		return nil, fmt.Errorf("another MMWatchdog node monitoring process is using state_dir %s", c.StateDir)
	}
	success := false
	defer func() {
		if !success {
			_ = lock.Close() // This descriptor carries only the daemon lock and has no pending writes.
		}
	}()
	stateStore, e := store.OpenStore(c.StateDir, c.StorageLimits())
	if e != nil {
		return nil, e
	}
	snap, e := stateStore.Load()
	if e != nil {
		stateStore.Close()
		return nil, e
	}
	now := clock()
	// Every wait that counts from a loaded time counts on the monotonic
	// clock from now on.
	snap.OnThisClock(now)
	// MMWatchdog runs without a measurement history that it cannot use: checks and
	// alerts never depend on it, and the storage problem says what to do.
	history, historyUnusable := store.OpenMeasurementHistory(filepath.Join(c.StateDir, store.MeasurementHistoryFile), c.MeasurementHistorySizeLimit.Bytes())
	var h *fleet.Hub
	if c.FleetHubRole != nil {
		if h, e = fleet.OpenHub(c.StateDir, c.FleetHubRole.AddressForNodes, stateStore.Node, c.FleetHubRole.ReportEvery.Value(), c.FleetHubRole.HubLimits(), now); e != nil {
			stateStore.Close()
			history.Close()
			return nil, e
		}
	}
	d := &Daemon{
		path: path, now: clock, started: now, store: stateStore, history: history, lock: lock, snapshot: snap, hub: h, startID: incidents.ID(),
		rep:                   fleet.Reporter{After: -1, ReadingsAfter: -1},
		storage:               fleet.StorageHealth{},
		checks:                map[string]*checkRuntime{},
		gates:                 map[string]chan struct{}{},
		runsInProgress:        map[string]*runInProgress{},
		recovering:            map[string]bool{},
		stoppedUnitSeenAt:     map[string]time.Time{},
		deliveries:            map[string]bool{},
		fromMemory:            map[memoryDeliveryKey]*memoryDelivery{},
		deliveriesFinished:    map[string]int{},
		nextAttempts:          map[string]queueHead{},
		noticeAttempts:        map[string]map[string]error{},
		destinationHealth:     map[string]*destinationHealth{},
		alertGroups:           fleet.AlertGroupingState{Sent: map[string]time.Time{}, Quiet: map[string]time.Time{}, Window: fleet.AlertGroupingWindow},
		members:               map[string]fleet.FleetMember{},
		mmhotlineDestinations: map[string]*mmhotlineDestinationState{},
		checkInNow:            make(chan struct{}, 1),
		runRetentionNow:       make(chan struct{}, 1),
		joinNow:               make(chan struct{}, 1),
		reportNow:             make(chan chan error),
	}
	stateStore.RetentionNow = d.retentionNow
	setNewlyAffectedNodesEvery(stateStore, c)
	d.storage.Report(store.MeasurementHistoryWrite, historyUnusable)
	// The readings that became too old while MMWatchdog did not run go now.
	d.storage.Report(store.MeasurementHistoryRetention, d.history.Prune(now, c.KeepMeasurementHistoryFor.Value()))
	if c.JoinFleetHub != nil {
		d.fleetSettings = fleet.ReadFleetSettings(filepath.Join(c.StateDir, fleet.FleetSettingsFile), c.Modules)
	}
	if h != nil {
		h.FleetSettings.Store(served)
	}
	d.setJoinCode(code)
	// The fleet store's membership decides the hub's fleet checks; a
	// hub-outage alerter keeps its role while it holds the hub check.
	var members map[string]fleet.FleetMember
	if h != nil {
		members = h.Members()
		for name, m := range members {
			d.members[name] = m
		}
		// A forget the fleet incident set step had not applied before the
		// hub stopped is applied now: fleet.db no longer lists the node.
		reporters, e := stateStore.IncidentSetReporters()
		if e != nil {
			stateStore.Close()
			history.Close()
			h.Close()
			return nil, e
		}
		for _, name := range reporters {
			if _, member := members[name]; !member && name != stateStore.Node {
				h.Reported[name] = store.ReportedIncidents{Forgotten: true}
			}
		}
	}
	d.watching = c.JoinFleetHub != nil && snap.Checks[fleet.FleetKey(checksandalerts.HubCheck)] != nil
	defs := d.definitions(c)
	before := incidents.CheckFingerprints(&d.snapshot)
	events := append(d.rebooted(now, c.AlertSeverityWhenThisNodeHasRebooted), d.snapshot.Reconcile(c, defs, now)...)
	// A check that is gone, or that changed into a new one, takes the state
	// of its module with it. A start also removes what an earlier run of MMWatchdog
	// left there for a moment only.
	removeModuleState, e := incidents.TakeAwayModuleStateOfChecksThatAreGone(c.ModuleStateDir, before, &d.snapshot)
	if e != nil {
		log.Printf("module state: %v", e)
	}
	removeModuleState()
	if e := checksandalerts.RemoveLeftoverModuleState(c.ModuleStateDir); e != nil {
		log.Printf("module state: %v", e)
	}
	// Daemon start: unfinished confirmation starts over.
	for _, st := range d.snapshot.Checks {
		st.Interrupt()
	}
	d.config, d.tokens, d.slots = c, tokens, make(chan struct{}, c.MaxConcurrentChecks)
	// A set_aside_findings.yml changed while the hub was stopped takes
	// effect now.
	d.settleSetAsideFindings(c, now)
	if err := d.restoreDestinationHealth(); err != nil {
		d.Close()
		return nil, fmt.Errorf("destination health: %w", err)
	}
	d.policies = incidents.RecoveryPolicies(c, nil)
	d.setDefinitions(defs)
	d.startFleet(members, now)
	_ = d.record(events)
	success = true
	return d, nil
}

// readBoot reads the kernel's boot ID and boot time. Tests replace it.
var readBoot = func() (string, time.Time, error) {
	id, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if e != nil {
		return "", time.Time{}, e
	}
	stat, e := os.ReadFile("/proc/stat")
	if e != nil {
		return "", time.Time{}, e
	}
	booted, e := parseBootTime(string(stat))
	return strings.TrimSpace(string(id)), booted, e
}

// readMachineID reads the machine ID that systemd keeps in /etc/machine-id. It
// returns "" when the file is missing or empty. Tests replace it.
//
// The machine ID tells whether saved state belongs to this machine. The boot ID
// cannot: it is random on every boot, so a boot ID that this boot does not
// match could come from a reboot or from state copied from another machine.
// /etc/machine-id is the one identity that every supported distribution has
// (all of them run systemd), that survives reboots and package upgrades, and
// that differs between machines installed separately. It can be the same on
// machines cloned from one image; then a move looks like a reboot of one
// machine and the notice is sent, which is the safe direction.
var readMachineID = func() string {
	id, e := os.ReadFile("/etc/machine-id")
	if e != nil {
		return ""
	}
	return strings.TrimSpace(string(id))
}

// parseBootTime reads the boot time, btime, from /proc/stat.
func parseBootTime(stat string) (time.Time, error) {
	for _, line := range strings.Split(stat, "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "btime" {
			if seconds, e := strconv.ParseInt(fields[1], 10, 64); e == nil && seconds > 0 {
				return time.Unix(seconds, 0).UTC(), nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("/proc/stat: boot time unavailable")
}

// rebooted keeps the current boot ID in the snapshot and returns a rebooted
// event when the daemon last started in another boot, at severity, the
// node's alert_severity_when_this_node_has_rebooted. A whole-node window
// open before the reboot, and so still in the loaded snapshot, records it
// locally only. The first start on a node has no event, and neither has the
// first start with state saved by another machine (see readMachineID).
func (d *Daemon) rebooted(now time.Time, severity string) []checksandalerts.Event {
	id, booted, e := readBoot()
	if e == nil && id == "" {
		e = fmt.Errorf("/proc/sys/kernel/random/boot_id contains an empty boot ID")
	}
	if e != nil {
		log.Printf("reboot detection: %v", e)
		return nil
	}
	previous, previousMachine, machine := d.snapshot.Boot, d.snapshot.Machine, readMachineID()
	d.snapshot.Boot, d.snapshot.Machine = id, machine
	if previous == "" || previous == id {
		return nil
	}
	if previousMachine != "" && machine != "" && previousMachine != machine {
		return nil
	}
	_, quiet := d.snapshot.Maintenance[checksandalerts.EveryCheckGroup]
	message := fmt.Sprintf("%s rebooted at %s; MMWatchdog last ran at %s", d.store.Node, booted.Format("15:04 UTC"), d.store.Saved.UTC().Format("15:04 UTC"))
	return []checksandalerts.Event{{ID: incidents.ID(), Type: "rebooted", AlertSeverity: severity, At: now, Message: message, Notify: !quiet}}
}

// setDefinitions keeps runtime state for unchanged definitions, taking a
// fleet check's new deadline period. Changed and new definitions start a
// new epoch with no schedule, result or CPU baseline.
func (d *Daemon) setDefinitions(defs map[string]config.Definition) {
	for _, key := range checksandalerts.Names(defs) {
		def := defs[key]
		d.setDefinition(key, &def)
	}
	for key := range d.checks {
		if _, exists := defs[key]; !exists {
			d.setDefinition(key, nil)
		}
	}
}

// setDefinition activates one check's definition, or removes the
// check when def is nil. A check that is new or changed starts with a
// memory of its own, so a run of the check as it was that has not ended
// holds no run of the new check up.
func (d *Daemon) setDefinition(key string, def *config.Definition) {
	switch rc := d.checks[key]; {
	case def == nil:
		delete(d.checks, key)
		delete(d.runsInProgress, key)
	case rc != nil && rc.def.Identity() == def.Identity():
		// Keep the observation state, but apply a new interval from the last
		// scheduled start. An overdue run is due on the next scheduler tick.
		if rc.def.Timing.CheckEvery.Value() != def.Timing.CheckEvery.Value() && !rc.scheduled.IsZero() {
			rc.next = rc.scheduled.Add(def.Timing.CheckEvery.Value())
		}
		rc.def = *def
	default:
		d.epoch++
		d.checks[key] = &checkRuntime{def: *def, epoch: d.epoch}
		delete(d.runsInProgress, key)
	}
}

// hubAddress is the hub address under configuration c, or empty.
func hubAddress(c *config.Config) string {
	if c.FleetHubRole == nil {
		return ""
	}
	return c.FleetHubRole.ListenOnAddress + " " + c.FleetHubRole.AddressForNodes
}

// setJoinCode activates a join code, or none. The reporter compares it with
// its credential before each report. The caller holds d.mu or owns d.
func (d *Daemon) setJoinCode(code *fleet.JoinCode) {
	d.joinCode = code
	d.store.FleetIdentity = ""
	if d.hub != nil {
		d.store.FleetIdentity = d.hub.Code.Fingerprint
	}
	if code != nil {
		d.store.FleetIdentity = code.Fingerprint
	}
	switch {
	case code == nil:
		d.link = nil
	case d.link == nil || d.link.Address != code.Address:
		d.link = &fleet.HubLink{Address: code.Address, Node: d.store.Node}
	}
}

// requireFresh starts a new epoch for every executed check the boundary
// covers, so earlier evidence no longer counts, and schedules it
// immediately. A maintenance boundary interrupts the checks: a failing time
// whose incident has not opened ends. A finished restart or recovery
// command, with BreakHealthyStreak, leaves the failing time running.
func (d *Daemon) requireFresh(window string, now time.Time, actionFinished bool) {
	for _, key := range checksandalerts.Names(d.checks) {
		rc := d.checks[key]
		//nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
		if !incidents.Covers(window, rc.def.CheckGroup) || rc.def.Heartbeat != nil {
			continue
		}
		if actionFinished {
			d.snapshot.Checks[key].BreakHealthyStreak()
		} else {
			d.snapshot.Checks[key].Interrupt()
		}
		d.epoch++
		rc.epoch, rc.fresh, rc.next = d.epoch, false, now //nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
	}
}

// suppressed reports that a check group is in maintenance. A window lasts until the
// command or the scheduler tick removes it, so suppression and the new epoch
// change at the same moment.
func (d *Daemon) suppressed(group string) bool {
	return d.snapshot.InMaintenance(group)
}

// routeFor is where alerts go under configuration c: see AlertRoute. The
// caller holds d.mu or owns d.
func (d *Daemon) routeFor(c *config.Config) store.AlertRoute {
	switch {
	case c.JoinFleetHub != nil:
		r := store.AlertRoute{Joined: true}
		if d.fleetSettings != nil {
			r.Destinations = d.fleetSettings.Destinations
			r.DestinationsWhoseServiceKeepsIncidents = d.fleetSettings.DestinationsWhoseServiceKeepsIncidents
		}
		return r
	case c.FleetHubRole != nil:
		return store.AlertRoute{Destinations: c.Alerts.Destinations, Hub: true}
	}
	return store.AlertRoute{Destinations: c.Alerts.Destinations}
}
func (d *Daemon) route() store.AlertRoute { return d.routeFor(d.config) }

// clearChanged clears the delivery bookkeeping errors of destinations the next
// route no longer lists as they were; the commit already cancelled their
// queues. A request already in flight finishes. The caller holds d.mu.
func (d *Daemon) clearChanged(old, next store.AlertRoute) {
	known := next.Known()
	for name, w := range old.Known() {
		if current, exists := known[name]; !exists || !checksandalerts.SameDestination(w, current) {
			d.storage.Report("notification bookkeeping "+name, nil)
		}
	}
}

// record commits observed facts already applied to live state. If the write
// fails, they stay in memory and their transitions stay pending, bounded with
// a visible gap, until a later commit succeeds; meanwhile the notifications
// worker sends their alerts from memory (deliverFromMemory). The caller holds
// d.mu.
func (d *Daemon) record(events []checksandalerts.Event) error {
	d.pending = boundPending(append(d.pending, events...), d.store.Limits.Load().UndeliveredAlertsBytes)
	route := d.route()
	route.DestinationHealth = d.destinationHealthRecords(route)
	route.AcceptedFromMemory = d.acceptedFromMemory
	err := d.store.Save(d.snapshot, d.pending, route)
	d.storage.Report(stateCommit, err)
	if err == nil {
		d.pending, d.acceptedFromMemory = nil, nil
		d.activateDestinationHealth(route.DestinationHealth)
	}
	return err
}

// control commits a change built on Snapshot.Clone, together with pending
// transitions, and swaps it in only after the commit succeeds. A failed
// control changes nothing. The caller holds d.mu.
func (d *Daemon) control(candidate incidents.Snapshot, events []checksandalerts.Event, route store.AlertRoute) error {
	route.DestinationHealth = d.destinationHealthRecords(route)
	route.AcceptedFromMemory = d.acceptedFromMemory
	err := d.store.Save(candidate, append(append([]checksandalerts.Event{}, d.pending...), events...), route)
	d.storage.Report(stateCommit, err)
	if err != nil {
		return err
	}
	d.snapshot, d.pending, d.acceptedFromMemory = candidate, nil, nil
	d.activateDestinationHealth(route.DestinationHealth)
	return nil
}

// boundPending keeps the newest transitions within limit encoded bytes, the
// outbox's size limit, replacing the dropped ones with a visible gap.
func boundPending(pending []checksandalerts.Event, limit int64) []checksandalerts.Event {
	bytes := int64(0)
	for i := len(pending) - 1; i >= 0; i-- {
		b, _ := json.Marshal(pending[i]) //nolint:nilaway // The reverse loop starts at len(pending)-1; nil pending skips every indexed access.
		if bytes+int64(len(b)) > limit {
			gap := checksandalerts.Event{ID: incidents.ID(), Type: "delivery_gap", At: time.Now().UTC(), Message: fmt.Sprintf("%d alerts and status changes were lost because MMWatchdog could not save them to state.db", i+1)}
			return append(append([]checksandalerts.Event{}, pending[i+1:]...), gap)
		}
		bytes += int64(len(b))
	}
	return pending
}

func (d *Daemon) Close() error {
	d.closeHeartbeatSocket()
	err := d.store.Close()
	d.history.Close()
	if d.hub != nil {
		d.hub.Close()
	}
	// A forked child may still hold a copy until exec closes it.
	_ = syscall.Flock(int(d.lock.Fd()), syscall.LOCK_UN) // The immediately following descriptor close releases the lock even if explicit unlocking fails.
	_ = d.lock.Close()                                   // This descriptor carries only the daemon lock and has no pending writes.
	return err
}
func (d *Daemon) Run(ctx context.Context) error {
	if e := os.MkdirAll(filepath.Dir(d.config.Socket), 0755); e != nil {
		return e
	}
	listener, e := listenOnUnixSocket(d.config.Socket, 0600)
	if e != nil {
		return e
	}
	defer listener.Close()
	defer func() {
		_ = os.Remove(d.config.Socket) // A leftover socket is detected and removed by the next daemon start.
	}()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errorsCh := make(chan error, 3)
	controlAPI := &http.Server{Handler: d.routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: responseWriteTimeout, MaxHeaderBytes: 8192}
	go func() { errorsCh <- controlAPI.Serve(listener) }()
	defer controlAPI.Close()
	if e = d.manageHeartbeatSocket(); e != nil {
		return e
	}
	defer d.closeHeartbeatSocket()
	if r := d.config.HeartbeatReceiver; r != nil {
		cert, e := tls.LoadX509KeyPair(r.TLSCertificateFile, r.TLSPrivateKeyFile)
		if e != nil {
			return e
		}
		l, e := tls.Listen("tcp", r.ListenOnAddress, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}})
		if e != nil {
			return e
		}
		defer l.Close()
		hs := &http.Server{Handler: http.HandlerFunc(d.heartbeat), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, MaxHeaderBytes: 8192}
		defer hs.Close()
		go func() { errorsCh <- hs.Serve(l) }()
	}
	if d.hub != nil {
		l, e := tls.Listen("tcp", d.config.FleetHubRole.ListenOnAddress, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{d.hub.Cert}})
		if e != nil {
			return e
		}
		defer l.Close()
		// Each join or report is bounded to 10 seconds end to end.
		hs := &http.Server{Handler: d.hubRoutes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, MaxHeaderBytes: 8192}
		defer hs.Close()
		go func() { errorsCh <- hs.Serve(l) }()
	}
	notifyDone := make(chan struct{})
	go func() { defer close(notifyDone); d.notifications(ctx) }()
	reportsDone := make(chan struct{})
	go func() { defer close(reportsDone); d.reports(ctx) }()
	checkInDone := make(chan struct{})
	go func() { defer close(checkInDone); d.checkIns(ctx) }()
	tick := time.NewTicker(config.ScheduleInterval)
	defer tick.Stop()
	var runErr error
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case e := <-errorsCh:
			if !errors.Is(e, http.ErrServerClosed) {
				runErr = e
			}
			break loop
		case <-tick.C:
			d.schedule(ctx, d.now())
		}
	}
	cancel()
	d.workers.Wait()
	<-notifyDone
	<-reportsDone
	<-checkInDone
	d.mu.Lock()
	e = d.record(nil)
	d.mu.Unlock()
	if runErr != nil {
		return runErr
	}
	return e
}

// ReloadResult is the daemon's answer to mmwatchdog reload: the configuration is
// active, and these are its validation warnings.
type ReloadResult struct {
	Reloaded                 bool                            `json:"reloaded"`
	NewOrChangedDestinations []string                        `json:"new_or_changed_destinations"`
	DestinationVerifications []DestinationVerificationResult `json:"destination_verifications"`
	config.ValidationWarnings
}

// reload validates the candidate configuration and activates it. It returns
// the validation warnings of the configuration that it activated.
func (d *Daemon) reload() (ReloadResult, error) {
	none := ReloadResult{}
	c, e := config.Load(d.path)
	if e != nil {
		return none, e
	}
	if e = ValidateRuntime(c); e != nil {
		return none, e
	}
	tokens, e := loadTokens(c)
	if e != nil {
		return none, e
	}
	code, e := fleet.LoadJoinCode(c)
	if e != nil {
		return none, e
	}
	var served *fleet.FleetSettings
	if c.FleetHubRole != nil {
		if served, e = fleet.NewFleetSettings(c); e != nil {
			return none, e
		}
	}
	// The warnings look at the credential files of the node, so they are
	// made before the daemon's lock is taken.
	if e = ValidateRouting(c); e != nil {
		return none, e
	}
	captureDestinationCredentials(c)
	warnings := ValidationWarnings(c, d.store.Node, true)
	// The state directories of checks that the reload does not keep are
	// removed after the reload, when the daemon's lock is free again: how
	// long that takes is up to the accounts of the modules.
	removeModuleState := func() {}
	defer func() { removeModuleState() }()
	d.mu.Lock()
	defer d.mu.Unlock()
	if c.MaxConcurrentChecks != d.config.MaxConcurrentChecks || c.Socket != d.config.Socket || c.StateDir != d.config.StateDir || c.ModuleStateDir != d.config.ModuleStateDir || !reflect.DeepEqual(c.HeartbeatReceiver, d.config.HeartbeatReceiver) || hubAddress(c) != hubAddress(d.config) {
		return none, fmt.Errorf("changing max_concurrent_checks, socket, state_dir, module_state_dir, heartbeat_receiver, fleet_hub_role.listen_on_address, or fleet_hub_role.address_for_nodes requires restarting MMWatchdog")
	}
	// The two settings that size a database file apply when the file opens.
	if c.StateDatabaseSizeLimit.Bytes() != d.config.StateDatabaseSizeLimit.Bytes() || (c.FleetHubRole != nil && d.config.FleetHubRole != nil && c.FleetHubRole.EventHistoryOfNodesContentSizeLimit.Bytes() != d.config.FleetHubRole.EventHistoryOfNodesContentSizeLimit.Bytes()) {
		return none, fmt.Errorf("changing state_database_size_limit or fleet_hub_role.event_history_of_nodes_content_size_limit requires restarting MMWatchdog")
	}
	now := d.now()
	candidate := d.snapshot.Clone()
	defs := d.definitions(c)
	events := candidate.Reconcile(c, defs, now)
	policies := incidents.RecoveryPolicies(c, d.policies)
	old, next := d.route(), d.routeFor(c)
	changed := []string{}
	for _, name := range checksandalerts.Names(next.Destinations) {
		if previous, exists := old.Destinations[name]; !exists || !checksandalerts.SameDestination(previous, next.Destinations[name]) {
			changed = append(changed, name)
		}
	}
	before := incidents.CheckFingerprints(&d.snapshot)
	// A first heartbeat check group opens the heartbeat socket before the
	// reload commits, so that a socket that cannot open leaves the old
	// configuration in force. The last group's removal closes it below.
	if len(tokens) > 0 {
		if e = d.syncHeartbeatSocket(c.Socket, true); e != nil {
			return none, e
		}
	}
	if e = d.control(candidate, events, next); e != nil {
		return none, e
	}
	// A check that the reload removed or changed into a new one takes the
	// state of its module with it.
	var stateError error
	if removeModuleState, stateError = incidents.TakeAwayModuleStateOfChecksThatAreGone(c.ModuleStateDir, before, &d.snapshot); stateError != nil {
		log.Printf("module state: %v", stateError)
	}
	d.clearChanged(old, next)
	d.store.SetLimits(c.StorageLimits())
	setNewlyAffectedNodesEvery(d.store, c)
	// A changed size limit of the measurement history applies from now on:
	// a file that is larger than a lower limit shrinks in steps
	// (trimMeasurementHistories). A changed time applies at the retention
	// step that the reload asks for at its end.
	if c.MeasurementHistorySizeLimit.Bytes() != d.config.MeasurementHistorySizeLimit.Bytes() {
		d.history.SetSizeLimit(c.MeasurementHistorySizeLimit.Bytes())
	}
	if served != nil {
		d.hub.FleetSettings.Store(served)
		d.hub.SetReportEvery(c.FleetHubRole.ReportEvery.Value())
		// A changed size limit of the measurement history of nodes applies
		// from now on, in the same way, and so does a changed time.
		if before := d.hub.Limits.Load().MeasurementHistoryOfNodesSizeLimit.Bytes(); c.FleetHubRole.MeasurementHistoryOfNodesSizeLimit.Bytes() != before {
			d.hub.Readings.SetSizeLimit(c.FleetHubRole.MeasurementHistoryOfNodesSizeLimit.Bytes())
		}
		d.hub.SetLimits(c.FleetHubRole.HubLimits())
	}
	if c.JoinFleetHub == nil {
		select {
		case d.checkInNow <- struct{}{}:
		default:
		}
	}
	d.config, d.tokens, d.policies = c, tokens, policies
	d.settleSetAsideFindings(c, now)
	if len(tokens) == 0 {
		_ = d.syncHeartbeatSocket(c.Socket, false) // Closing cannot fail.
	}
	d.syncMMHotlineDestinations()
	d.watching = d.watching && c.JoinFleetHub != nil
	d.setJoinCode(code)
	if code != nil {
		select {
		case d.joinNow <- struct{}{}:
		default:
		}
	}
	d.setDefinitions(defs)
	// Every retention time of the new configuration applies at once. The
	// worker's retention step waits for d.mu, so it reads what this reload
	// committed.
	select {
	case d.runRetentionNow <- struct{}{}:
	default:
	}
	return ReloadResult{Reloaded: true, ValidationWarnings: warnings, NewOrChangedDestinations: changed}, nil
}
