package daemon

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"slices"
	"sort"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/store"
)

func (d *Daemon) schedule(ctx context.Context, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Windows that expire at this tick end together, so the windows of one
	// command send one alert.
	expired := []string{}
	for _, name := range checksandalerts.Names(d.snapshot.Maintenance) {
		if !now.Before(d.snapshot.Maintenance[name].Until) {
			expired = append(expired, name)
		}
	}
	events := d.snapshot.EndMaintenance(expired, now, incidents.MaintenanceExpiredMessage)
	for _, name := range expired {
		d.requireFresh(name, now, false)
	}
	keys := make([]string, 0, len(d.checks))
	fleet := map[string][]checksandalerts.Event{}
	for key, rc := range d.checks {
		// Fleet checks execute nothing, so a hub's thousands of nodes
		// stay out of the queue below.
		if rc.def.CheckGroup == checksandalerts.NodeReportsCheckGroup {
			if e := d.fleetTick(rc, d.snapshot.Checks[key], now); len(e) > 0 {
				fleet[key] = e
			}
			continue
		}
		keys = append(keys, key)
	}
	for _, key := range checksandalerts.Names(fleet) {
		events = append(events, fleet[key]...)
	}
	sort.Strings(keys)
	// Keep the oldest due check first when a slot opens.
	sort.SliceStable(keys, func(i, j int) bool { return d.checks[keys[i]].next.Before(d.checks[keys[j]].next) })
	for _, key := range keys {
		rc, st := d.checks[key], d.snapshot.Checks[key]
		every := rc.def.Timing.CheckEvery.Value() //nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
		suppressed := d.suppressed(rc.def.CheckGroup)
		if rc.def.Heartbeat != nil {
			// Open an overdue incident at once. Maintenance end announces it
			// if maintenance suppressed it.
			//nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
			if !now.Before(st.Deadline) && st.FailureIncident.Incident == "" {
				o := checksandalerts.Result("failed", overdueMessage(st, rc.def.Heartbeat))
				o.At = now
				events = append(events, st.Observe(o, rc.def.Timing, suppressed)...)
			}
			continue
		}
		//nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
		if now.Before(rc.next) {
			continue
		}
		if rc.next.IsZero() {
			rc.next = now
		}
		//nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
		if rc.running || d.running >= d.config.MaxConcurrentChecks {
			// A run of the check that has its place and is under way brings
			// its own observation, at the latest a moment after its time
			// limit (waitForRun), and a check whose run did not end has
			// said so. Neither has missed an observation for lack of a
			// place.
			if d.runsInProgress[key] != nil {
				continue
			}
			// Coalesce missed intervals into one unknown observation rather than
			// creating a replay backlog.
			//nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
			if now.Sub(rc.next) >= every && (st.Last.At.IsZero() || now.Sub(st.Last.At) >= every) {
				o := checksandalerts.Result("unknown", "scheduled observation missed: execution capacity unavailable")
				o.At = now
				quiet := d.quietAtObservation(key, now)
				events = append(events, st.Observe(o, rc.def.Timing, suppressed || quiet)...)
				rc.fresh = true //nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
			}
			continue
		}
		rc.next = now.Add(every)
		// A due execution can wait up to one scheduler tick. Include that
		// resolution in the lateness tolerance, especially for short intervals.
		// A start is late when MMWatchdog did not run the check in between. A run
		// that took longer than its interval, as one that MMWatchdog stopped
		// waiting for, leaves no such gap: MMWatchdog ran the check until that run's
		// end.
		late := every/2 + config.ScheduleInterval
		//nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
		if !rc.scheduled.IsZero() && now.Sub(rc.scheduled) > every+late && now.Sub(rc.finished) > late {
			st.BreakHealthyStreak()
		}
		rc.scheduled = now
		rc.running = true
		d.running++
		//nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
		if rc.memory == nil {
			rc.memory = &checksandalerts.CheckMemory{}
		}
		d.workers.Add(1)
		go d.observe(ctx, key, rc, rc.epoch) //nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
	}
	events = append(events, d.raiseOpenIncidents(now)...)
	events = append(events, d.remind(now)...)
	if len(events) > 0 || len(d.pending) > 0 || d.storage[stateCommit] != "" {
		_ = d.record(events)
	}
}

// raiseOpenIncidents is the scheduler tick's escalation step: to the
// check's current settings, by time for checks with raise_to_sos_after_failing_for
// and for silent nodes with raise_to_sos_after_this_node_is_silent_for,
// and on the hub by the number of joined
// nodes silent at once, under the configured silence threshold, and by a
// node group of which every node is silent (raiseSilentNodeGroups).
// Maintenance holds raises as it holds reminders, and a node in maintenance
// does not count as silent. A check that is kept quiet while a check that it names
// fails is held in the same way (quietWhile).
//
// A daemon start gives every fleet check a fresh deadline (startFleet): until
// it has passed, MMWatchdog does not know whether a node whose silence incident was
// open before the start is still silent. So such a node is not raised by
// time and does not count as silent before its deadline; a node that is
// still silent then is raised then. While MMWatchdog runs, an open silence incident
// always has a deadline that has passed, because the receipt that moves the
// deadline also resolves the incident. The caller holds d.mu.
func (d *Daemon) raiseOpenIncidents(now time.Time) []checksandalerts.Event {
	keys := []string{}
	for key, st := range d.snapshot.Checks {
		if st.FailureIncidentOpen() || st.CannotCheckIncident.Incident != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	events := []checksandalerts.Event{}
	silent := []*incidents.CheckState{}
	for _, key := range keys {
		rc, st := d.checks[key], d.snapshot.Checks[key]
		if rc == nil {
			continue
		}
		suppressed, silenceKnown := d.suppressed(rc.def.CheckGroup) || d.quietWhile(key) != "", true
		if rc.def.CheckGroup == checksandalerts.NodeReportsCheckGroup {
			suppressed, silenceKnown = d.fleetSuppressed(rc, now), !now.Before(st.Deadline) //nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
			//nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
			if d.config.FleetHubRole != nil && !suppressed && silenceKnown && st.FailureIncident.Incident != "" {
				silent = append(silent, st)
			}
		}
		// A raise that maintenance held after a reload goes out now.
		events = append(events, st.RaiseToPolicy(now, suppressed)...)
		if silenceKnown {
			if rc.def.CheckGroup == checksandalerts.NodeReportsCheckGroup {
				events = append(events, d.raiseSilentNodeAfterTime(rc, st, now, suppressed)...)
			} else {
				events = append(events, st.RaiseAfterTime(now, suppressed)...)
			}
		}
	}
	if h := d.config.FleetHubRole; h != nil && h.SilentNodesReachSosThreshold(len(silent), len(d.members)) {
		var setting string
		if h.RaiseToSosWhenSilentNodesReachCount != nil {
			setting = fmt.Sprint("raise_to_sos_when_silent_nodes_reach_count: ", *h.RaiseToSosWhenSilentNodesReachCount)
		} else {
			setting = fmt.Sprint("raise_to_sos_when_silent_nodes_reach_percent: ", *h.RaiseToSosWhenSilentNodesReachPercent)
		}
		message := incidents.RaiseMessage(fmt.Sprintf("%d of %d nodes are not reporting to the hub at the same time", len(silent), len(d.members)), "sos", setting)
		for _, st := range silent {
			events = append(events, st.Raise(&st.FailureIncident, checksandalerts.FailureIncidentEvents, "sos", message, now, false)...)
		}
	}
	return append(events, d.raiseSilentNodeGroups(silent, now)...)
}

// remind is the scheduler tick's reminder step: see CheckState.Reminders.
// The destinations, their remind_every, and who gets an alert are the
// route's, on a joined node its copy of the fleet settings. Maintenance
// holds a check's reminders, as it holds its alerts, and so does the
// failing of a check that the check names as its cause (quietWhile).
//
// A joined node whose reports reach its hub makes no reminder of a finding
// whose incident has an exact finding: the hub reminds once for each fleet
// incident key, for all its nodes. The node reminds of such an incident
// itself only while its latest report failed, when it delivers without the
// hub. Before the first report of a start, the node counts the hub as
// reached: the first report follows within report_every. The caller holds
// d.mu.
func (d *Daemon) remind(now time.Time) []checksandalerts.Event {
	destinations := d.route().Destinations
	keys := []string{}
	for key, st := range d.snapshot.Checks {
		if st.Reminding() {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	hubReminds := d.hub == nil && d.link != nil && d.link.LastError == ""
	events := []checksandalerts.Event{}
	for _, key := range keys {
		rc := d.checks[key]
		if rc == nil || d.remindersHeldBack(key, rc, now) {
			continue
		}
		if hubReminds {
			events = append(events, d.snapshot.Checks[key].RemindersOfIncidentsWithoutExactFinding(destinations, now)...)
			continue
		}
		events = append(events, d.snapshot.Checks[key].Reminders(destinations, now)...)
	}
	return events
}

// remindersHeldBack reports that maintenance or quiet time holds the
// reminders of the check with key back, as it holds its alerts: the one
// rule for the reminder step and for reminder_eligible in status and in
// node status reports. The caller holds d.mu.
func (d *Daemon) remindersHeldBack(key string, rc *checkRuntime, now time.Time) bool {
	if rc.def.CheckGroup == checksandalerts.NodeReportsCheckGroup {
		return d.fleetSuppressed(rc, now)
	}
	return d.suppressed(rc.def.CheckGroup) || d.quietWhile(key) != ""
}

// overdueMessage explains a missed deadline by the last success and the
// expected interval.
func overdueMessage(st *incidents.CheckState, h *checksandalerts.Heartbeat) string {
	last := "last heartbeat " + st.Receipt.UTC().Format(time.RFC3339) //nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
	if st.Receipt.IsZero() {
		last = "no heartbeat since monitoring started " + st.Deadline.Add(-h.MaxTimeBetweenHeartbeats.Value()).UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("heartbeat overdue: %s; expected within %s; deadline %s", last, h.MaxTimeBetweenHeartbeats, st.Deadline.UTC().Format(time.RFC3339))
}

// observe runs one scheduled execution and applies its result only if its
// ticket still matches the check's epoch.
func (d *Daemon) observe(ctx context.Context, key string, rc *checkRuntime, ticket int) {
	defer d.workers.Done()
	d.mu.Lock()
	// Reload or an epoch change can retire this work before it starts.
	//nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
	if d.checks[key] != rc || rc.epoch != ticket {
		rc.running = false
		d.running--
		d.mu.Unlock()
		return
	}
	def, memory := rc.def, rc.memory
	// No other scheduled execution of this check runs until this one
	// returns, so the state of its module that it counts on stays current.
	state := &checksandalerts.ModuleState{Stored: slices.Clone(d.snapshot.Checks[key].ModuleState)} //nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
	d.mu.Unlock()
	o := d.execute(ctx, key, def, memory, state)
	d.mu.Lock()
	defer d.mu.Unlock()
	rc.running = false
	rc.finished = d.now()
	d.running--
	if ctx.Err() != nil || d.checks[key] != rc || rc.epoch != ticket {
		return
	}
	if state.Saved != nil {
		d.snapshot.Checks[key].ModuleState = state.Saved
	}
	_ = d.record(d.accept(ctx, key, rc, o))
}

// accept applies the observation of a scheduled run whose ticket matched,
// and starts the recovery actions that are due after it. Maintenance of the
// check group holds the check's alerts, and so does the failing of a check
// that the check names as its cause (quietAtObservation). The caller holds
// d.mu and commits the events.
func (d *Daemon) accept(ctx context.Context, key string, rc *checkRuntime, o checksandalerts.Observation) []checksandalerts.Event {
	quiet := d.quietAtObservation(key, o.At)
	events := d.snapshot.Checks[key].Observe(o, rc.def.Timing, d.suppressed(rc.def.CheckGroup) || quiet) //nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
	rc.fresh = true
	d.keepReadings(rc, o)
	d.startRecovery(ctx, o.At)
	return events
}

// keepReadings stores the readings of a scheduled run in the node's
// measurement history: one reading for each measurement of the run, whatever
// the run's result, for a check with a limit and for one without. A write
// that fails is a storage problem that status shows until a write succeeds
// again; the check's state does not depend on it. The caller holds d.mu.
func (d *Daemon) keepReadings(rc *checkRuntime, o checksandalerts.Observation) {
	if len(o.Measurements) == 0 {
		return
	}
	def := rc.def //nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
	measurements := make(map[string]store.MeasurementReading, len(o.Measurements))
	for name, measurement := range o.Measurements {
		reading := store.MeasurementReading{Value: measurement.Value, Unit: measurement.Unit}
		if limit, exists := def.Check.FailWhenMeasurementAbove[name]; exists {
			reading.FailWhenMeasurementAbove = &limit
		}
		if limit, exists := def.Check.FailWhenMeasurementBelow[name]; exists {
			reading.FailWhenMeasurementBelow = &limit
		}
		measurements[name] = reading
	}
	d.storage.Report(store.MeasurementHistoryWrite, d.history.Add(store.RunReadings{At: o.At, CheckGroup: def.CheckGroup, Check: def.Name, Measurements: measurements}))
}

// timeForARunToEndAfterItsTimeLimit is how long MMWatchdog waits for a run of a
// check once the run's time limit has passed. A run minds its time limit:
// it ends its programs and closes its connections there, and returns within
// a moment.
const timeForARunToEndAfterItsTimeLimit = 5 * time.Second

// testTimeForARunToEndAfterItsTimeLimitKey is the key of a context value
// that only tests set: a time.Duration that replaces
// timeForARunToEndAfterItsTimeLimit for the runs under that context, so that
// a test of a run that does not end waits milliseconds, and beside other
// tests that wait for other times. In MMWatchdog itself no context carries the
// value.
type testTimeForARunToEndAfterItsTimeLimitKey struct{}

// timeForARunToEndAfterItsTimeLimitUnder is how long MMWatchdog waits for a run
// under ctx once the run's time limit has passed.
func timeForARunToEndAfterItsTimeLimitUnder(ctx context.Context) time.Duration {
	if wait, ok := ctx.Value(testTimeForARunToEndAfterItsTimeLimitKey{}).(time.Duration); ok {
		return wait
	}
	return timeForARunToEndAfterItsTimeLimit
}

// runInProgress is one run of a check, from its start until the code of the
// check's module returns.
type runInProgress struct {
	started time.Time
	// ended is set when the code of the module has returned. notWaitedFor
	// is set when MMWatchdog stopped waiting for the run before that. d.mu guards
	// both.
	ended, notWaitedFor bool
}

// waitForRun waits for the observation of a run of a check: until c ends,
// which holds the run's time limit, and timeForARunToEndAfterItsTimeLimit
// longer. It reports whether the run ended in that time.
func waitForRun(c context.Context, ended <-chan checksandalerts.Observation) (checksandalerts.Observation, bool) {
	select {
	case o := <-ended:
		return o, true
	case <-c.Done():
	}
	timer := time.NewTimer(timeForARunToEndAfterItsTimeLimitUnder(c))
	defer timer.Stop()
	select {
	case o := <-ended:
		return o, true
	case <-timer.C:
		return checksandalerts.Observation{}, false
	}
}

// runThatDoesNotEnd is the observation of a run of a check that started at
// started and has not ended by now, well after its time limit. Nothing in
// MMWatchdog can end such a run: the code of the module waits for the kernel, and
// the kernel holds some questions without end.
func runThatDoesNotEnd(started, now time.Time, limit checksandalerts.Duration) checksandalerts.Observation {
	o := checksandalerts.Result("unknown", fmt.Sprintf("the check run did not finish within the time limit of %s. MMWatchdog stopped waiting for the run to finish", limit))
	o.Details = whyARunDoesNotEnd
	o.At, o.Duration = now, now.Sub(started)
	return o
}

// noRunBeforeTheEarlierOneEnds is the observation of a run that MMWatchdog did not
// start, because the run of the same check that started at started has not
// ended.
func noRunBeforeTheEarlierOneEnds(started, now time.Time) checksandalerts.Observation {
	o := checksandalerts.Result("unknown", fmt.Sprintf("the check run that started at %s has not finished. MMWatchdog will start no new run of the check until the earlier run finishes", checksandalerts.ClockTime(started, now)))
	o.Details = whyARunDoesNotEnd
	o.At = now
	return o
}

// whyARunDoesNotEnd is what the details of both observations say.
var whyARunDoesNotEnd = []string{
	"a run that does not end waits for the kernel, and no time limit ends that wait",
	"typical causes: a file on a filesystem of another machine that does not answer, or a program that the kernel cannot end",
}

// Scheduled and diagnostic executions share the same per-check gate and
// global max_concurrent_checks limit. Waiting does not consume the execution timeout.
// A diagnostic execution passes a memory of its own and no state.
//
// The code of the check's module runs in a goroutine of its own, and MMWatchdog
// waits for it no longer than waitForRun does. A run that has not ended by
// then gives its gate and its place among the concurrent checks back, and
// its observation says that it did not end. That run stays in
// d.runsInProgress until it ends, and MMWatchdog starts no other run of the check
// while it is there: so no two runs use the check's memory at once, and a
// check that cannot end keeps one goroutine waiting, and not one more for
// each interval. MMWatchdog drops what that run brings when it ends at last: its
// observation and its state are not stored, and its memory is emptied, as
// after a start of MMWatchdog.
func (d *Daemon) execute(ctx context.Context, key string, def config.Definition, memory *checksandalerts.CheckMemory, state *checksandalerts.ModuleState) checksandalerts.Observation {
	d.mu.Lock()
	gate := d.gates[key]
	if gate == nil {
		gate = make(chan struct{}, 1)
		d.gates[key] = gate
	}
	d.mu.Unlock()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return checksandalerts.Result("unknown", ctx.Err().Error())
	}
	d.mu.Lock()
	earlier := d.runsInProgress[key]
	d.mu.Unlock()
	if earlier != nil {
		return noRunBeforeTheEarlierOneEnds(earlier.started, d.now())
	}
	select {
	case d.slots <- struct{}{}:
		defer func() { <-d.slots }()
	case <-ctx.Done():
		return checksandalerts.Result("unknown", ctx.Err().Error())
	}
	// The run counts on a copy of the state of the check's module, so a run
	// that MMWatchdog stopped waiting for changes nothing when it ends. A diagnostic
	// takes its copy once the gate has let any scheduled execution finish:
	// it sees what earlier runs stored, and changes nothing either.
	diagnostic := state == nil
	run := &checksandalerts.CheckRun{Check: def.CheckWithSeverity(), Timing: def.Timing, CheckGroup: def.CheckGroup, CheckName: def.Name, Diagnostic: diagnostic, Memory: memory, State: &checksandalerts.ModuleState{}, Now: d.now}
	inProgress := &runInProgress{started: d.now()}
	asked := def.Identity()
	d.mu.Lock()
	// A reload can change or remove the check while this run waits for its
	// turn. A run of the check as it was does not start then: it would
	// count as the run in progress of the check as it is now.
	if current := d.checks[key]; current == nil || current.def.Identity() != asked {
		d.mu.Unlock()
		o := checksandalerts.Result("unknown", "a configuration reload changed or removed the check while the check run was waiting to start")
		o.At = d.now()
		return o
	}
	run.ModuleStateDir = d.config.ModuleStateDir
	if !diagnostic {
		run.State.Stored = state.Stored //nolint:nilaway // diagnostic is state == nil; both state accesses run only when diagnostic is false.
	} else if st := d.snapshot.Checks[key]; st != nil {
		run.State.Stored = slices.Clone(st.ModuleState)
	}
	d.runsInProgress[key] = inProgress
	d.mu.Unlock()
	c, cancel := context.WithTimeout(ctx, def.Timing.TimeLimitPerRun.Value())
	defer cancel()
	ended := make(chan checksandalerts.Observation, 1)
	go func() {
		o := runCheck(c, run)
		d.mu.Lock()
		inProgress.ended = true
		if d.runsInProgress[key] == inProgress {
			delete(d.runsInProgress, key)
		}
		if inProgress.notWaitedFor {
			memory.Value = nil
		}
		d.mu.Unlock()
		ended <- o
	}()
	o, inTime := waitForRun(c, ended)
	if !inTime {
		d.mu.Lock()
		// A run that ended in this same moment counts.
		inTime = inProgress.ended
		inProgress.notWaitedFor = !inTime
		d.mu.Unlock()
		if !inTime {
			o = runThatDoesNotEnd(inProgress.started, d.now(), def.Timing.TimeLimitPerRun)
			o.Details = append(slices.Clone(o.Details), "MMWatchdog will start no new run of the check until the unfinished run ends")
			return o
		}
		o = <-ended
	}
	if !diagnostic {
		state.Saved = run.State.Saved
	}
	return o
}

// runCheck is checksandalerts.RunCheck, with a panic in the check's module
// turned into an unknown observation that names the panic; the journal
// gets the stack. A panic that ended the node monitoring process would end,
// at each start, every check's failing time whose incident has not opened
// yet, so no such incident would ever open.
func runCheck(c context.Context, run *checksandalerts.CheckRun) (o checksandalerts.Observation) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("the module of the check %s stopped with an internal error: %v\n%s", checksandalerts.AboutCheck(run.CheckGroup, run.CheckName, ""), p, debug.Stack())
			o = checksandalerts.Result("unknown", "the check's module stopped with an internal error: "+primitives.DetailLine(fmt.Sprint(p))+". The journal of mmwatchdog shows where")
			o.At = run.Now()
		}
	}()
	return checksandalerts.RunCheck(c, run)
}
