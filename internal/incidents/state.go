// Package incidents turns the observations of each check into its state,
// incidents, and events: the failing time, recovery, the findings of a
// check, the severity an incident reaches, the time in which MMWatchdog keeps a
// check quiet, and which recovery action is due. The store saves what it
// decides, and the daemon acts on it. It touches the disk in one place: it
// moves aside the module state of a check that is gone.
package incidents

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
)

// Track is one incident lifecycle: health or monitoring. Announced is the
// incident whose failure notification was queued and whose recovery or
// retirement has not been. It can differ from Incident after maintenance.
// AlertSeverity is the open incident's severity, which only rises;
// AnnouncedAlertSeverities are those the announced incident's alerts went
// out with.
type Track struct {
	Incident                 string                    `json:"incident_id,omitempty"`
	Opened                   time.Time                 `json:"opened,omitempty"`
	AlertSeverity            string                    `json:"alert_severity,omitempty"`
	Evidence                 *checksandalerts.Evidence `json:"evidence,omitempty"`
	Announced                string                    `json:"announced_incident_id,omitempty"`
	AnnouncedAlertSeverities []string                  `json:"alert_severities_sent_for_announced_incident,omitempty"`
	// RemindFrom is when reminders of the announced incident start counting:
	// its announcement, its latest raise, or the end of the maintenance
	// window that held them.
	// Reminded is the last reminder sent to each destination.
	RemindFrom time.Time            `json:"remind_from,omitempty"`
	Reminded   map[string]time.Time `json:"reminded,omitempty"`
	// CurrentExactFinding is, on the failure track of a finding whose
	// module names its exact finding, the exact finding that the latest run
	// that reported the finding gave (checksandalerts.Finding.ExactFinding).
	// ExactFindingSequence counts its changes, so it only grows during an
	// incident. AnnouncedExactFinding is the exact finding that the
	// incident's latest alert carried: when the two differ, the incident's
	// next run outside maintenance and quiet time sends
	// exact_finding_changed (announceExactFindingChange).
	CurrentExactFinding   string `json:"current_exact_finding,omitempty"`
	AnnouncedExactFinding string `json:"announced_exact_finding,omitempty"`
	ExactFindingSequence  int64  `json:"exact_finding_sequence,omitempty"`
	// ReminderEligible reports, in status and in a node status report, an
	// incident that is open and announced while no maintenance window and
	// no quiet time holds its alerts back: one whose reminders are due.
	// MarkReminderEligible sets it when status is built; the state that
	// the store keeps never has it.
	ReminderEligible bool `json:"reminder_eligible"`
}

// reminding reports that the track's incident is open and announced.
func (t *Track) reminding() bool { return t.Incident != "" && t.Announced == t.Incident }

// reportExactFinding records the exact finding that a run reported for the
// track's finding, and counts it when it changed.
func (t *Track) reportExactFinding(exact string) {
	if exact != t.CurrentExactFinding {
		t.CurrentExactFinding = exact
		t.ExactFindingSequence++
	}
}

type CheckState struct {
	// RecordsOnly marks a measurement check without an alert_severity: it
	// records its readings and opens no incident.
	RecordsOnly bool                           `json:"records_only,omitempty"`
	Source      checksandalerts.SourceLocation `json:"source"`
	Timing      checksandalerts.Timing         `json:"timing"`
	// AlertSeveritySettings are the check's alert_severity and
	// raise_to_sos_after_failing_for.
	AlertSeveritySettings checksandalerts.AlertSeveritySettings `json:"alert_severity_settings"`
	// SendAlertsOnlyToDestinations are the destinations the check names,
	// when it names its destinations; every event of the check carries them.
	SendAlertsOnlyToDestinations *[]string `json:"send_alerts_only_to_destinations,omitempty"`
	// QuietWhileTheseChecksFail are the checks that the check names in its
	// no_alerts_or_recovery_actions_while_these_checks_fail, and
	// KeepsTheseChecksQuiet are the checks that name this check in theirs.
	// QuietNowWhileCheckFails is the named check that keeps this check
	// quiet, or "": quietAtObservation sets it at each observation of the
	// check. In status it is the named check that counts as failing now
	// (Daemon.status). See onecause.go.
	QuietWhileTheseChecksFail []string                    `json:"no_alerts_or_recovery_actions_while_these_checks_fail,omitempty"`
	QuietNowWhileCheckFails   string                      `json:"no_alerts_or_recovery_actions_now_while_check_fails,omitempty"`
	KeepsTheseChecksQuiet     []string                    `json:"no_alerts_or_recovery_actions_for_these_checks_while_this_check_fails,omitempty"`
	CheckGroup                string                      `json:"check_group"`
	Check                     string                      `json:"check"`
	Fingerprint               string                      `json:"fingerprint"`
	Last                      checksandalerts.Observation `json:"last_observation"`
	FailureIncident           Track                       `json:"failure_incident"`
	CannotCheckIncident       Track                       `json:"cannot_check_incident"`
	// FailingSince is the start of the check's failing time: its first
	// failed observation. The failing time ends, and FailingSince clears,
	// when the check has been healthy for recovered_after_healthy_for, or
	// when unobserved time ends it before its incident opened. Every
	// waiting time of the check counts from it.
	FailingSince time.Time `json:"failing_since,omitempty"`
	// FailingTimeRunsFailed says, oldest first, whether each of the last
	// runs of the failing time failed, at most failingTimeRunsKept of them,
	// for the note that a check failed on and off.
	FailingTimeRunsFailed []bool `json:"failing_time_runs_failed,omitempty"`
	// HealthySince and UnknownSince are streaks: only continuous accepted
	// observations extend them.
	HealthySince time.Time `json:"healthy_since,omitempty"`
	UnknownSince time.Time `json:"unknown_since,omitempty"`
	Deadline     time.Time `json:"next_due_by,omitempty"`
	Receipt      time.Time `json:"last_received_at,omitempty"`
	// ModuleState is what the check's module keeps from one run to the
	// next, such as a crash-loop check's history of automatic restarts: see
	// ModuleState.
	ModuleState json.RawMessage `json:"module_state,omitempty"`
	// OneIncidentForEachFinding marks a check whose module reports
	// findings, such as an every_systemd_unit check: the check itself
	// opens no failure incident, and each finding has one of its own, in
	// Findings.
	OneIncidentForEachFinding bool `json:"one_incident_for_each_finding,omitempty"`
	// Findings holds, by the ID of the finding, the state of each finding
	// of the check that is in a failing time or has an incident that is open
	// or still announced: for an every_systemd_unit check, each failed
	// unit by its name. A finding's state has the shape of a check's state,
	// because each finding follows the check's timings, severity, and
	// destinations by itself: its own failing time, incident, alert, raise,
	// reminders, and recovery. Its settings are the check's; findings()
	// copies them in before every use. observeFindings adds a finding and
	// forgets it again.
	Findings map[string]*CheckState `json:"findings,omitempty"`
	// Finding is, in the state of one finding, the ID of that finding.
	Finding string `json:"finding,omitempty"`
	// FindingRecoveryMessage is supplied by the finding's module and stays
	// with its incident until recovery, also across a daemon restart.
	FindingRecoveryMessage string `json:"finding_recovery_message,omitempty"`
	// Rev changes whenever a method here changes a persisted field. Fleet
	// checks grow with the fleet, so Save rewrites one only when its rev
	// changed; see "Two commit paths" in docs/implementation-rules.md.
	Rev uint64 `json:"-" yaml:"-"`
}

// revisions numbers check changes across the process, so a check
// created again under an old key never repeats a written rev.
var revisions atomic.Uint64

func (s *CheckState) Touch() { s.Rev = revisions.Add(1) } //nolint:nilaway // EndMaintenance visits existing checks; ReconcileOne and Store.Load allocate every entry, and daemon locking prevents removal during iteration. Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.

// CheckStatus is the inspection view of a check at one moment.
type CheckStatus struct {
	CheckState
	// Module is the name of the check's module, and ModuleKind its kind:
	// core, official_extra, project_starlark, or project_executable. A
	// heartbeat and a check of the fleet have no module.
	Module     string `json:"module,omitempty"`
	ModuleKind string `json:"module_kind,omitempty"`
	State      string `json:"state"`
	NeedsFresh bool   `json:"needs_fresh_observation"`
	// QuietUntilNextRunAfterCheckFailed is the named check that kept the
	// check quiet and no longer counts as failing: the check stays quiet
	// until its own next run, which decides. While a named check counts as
	// failing, CheckState.QuietNowWhileCheckFails names it instead. See
	// Daemon.status and onecause.go.
	QuietUntilNextRunAfterCheckFailed string `json:"no_alerts_or_recovery_actions_until_next_run_because_check_was_failing,omitempty"`
	// AlertDestinationsTheHubDoesNotHave are, on a node that has joined a
	// hub, the destinations the check names that the fleet settings do not
	// have. The check's alerts still go to the named destinations that the
	// hub has, and go by severity only when the hub has none of them
	// (checksandalerts.Event.NamedDestinations).
	AlertDestinationsTheHubDoesNotHave []string `json:"alert_destinations_the_hub_does_not_have,omitempty"`
	// RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent is, on the hub's
	// check of a joined node, that node's setting of the same name: the
	// node groups whose silence raises the node's silence alert to sos.
	RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent []string `json:"raise_to_sos_when_every_node_of_these_node_groups_is_silent,omitempty"`
	// FindingStatuses are the findings of a check whose module reports
	// them, such as the failed units of an every_systemd_unit check,
	// each with its own state. In JSON they are the check's findings: this
	// field is less deeply nested than CheckState.Findings, so encoding/json
	// writes and reads this one under that name.
	FindingStatuses map[string]CheckStatus `json:"findings,omitempty"`
}

type RecoveryAttempts struct {
	Attempts     int       `json:"attempts_used"`
	Last         time.Time `json:"last_attempt_at"`
	LastFinished time.Time `json:"last_attempt_finished_at,omitzero"`
	LastResult   string    `json:"last_result,omitempty"`
	// WithheldForFailingTimeSince is the start of the failing time for
	// which MMWatchdog last withheld the restart of a stopped unit, so that it
	// records the withheld restart once for that failing time.
	WithheldForFailingTimeSince time.Time `json:"restart_withheld_for_failing_time_since,omitempty"`
}

type Snapshot struct {
	Checks                   map[string]*CheckState       `json:"checks"`
	RecoveryAttemptsByTarget map[string]*RecoveryAttempts `json:"recovery_attempts_by_target"`
	// Maintenance holds windows by check group name, and the whole-node window
	// under EveryCheckGroup.
	Maintenance map[string]checksandalerts.Window `json:"maintenance"`
	// Boot is the kernel boot ID the daemon last started in, or empty before
	// its first start on the node.
	Boot string `json:"boot,omitempty"`
	// Machine is the machine ID (/etc/machine-id) of the machine that saved
	// this state, or empty when it was unknown. State copied to another
	// machine, as when moving a fleet hub, carries the other machine's ID.
	Machine string `json:"machine,omitempty"`
}

func FreshSnapshot() Snapshot {
	return Snapshot{Checks: map[string]*CheckState{}, RecoveryAttemptsByTarget: map[string]*RecoveryAttempts{}, Maintenance: map[string]checksandalerts.Window{}}
}

// Covers reports that the named maintenance window applies to a check group.
func Covers(window, group string) bool {
	return window == checksandalerts.EveryCheckGroup || window == group
}

// InMaintenance reports that the check group's own window or the whole-node
// window exists. A window lasts until the command or the scheduler tick
// removes it.
func (s Snapshot) InMaintenance(group string) bool {
	_, own := s.Maintenance[group]
	_, all := s.Maintenance[checksandalerts.EveryCheckGroup]
	return own || all
}

// Window returns the named maintenance window, or nil when there is none.
func (s Snapshot) Window(name string) *checksandalerts.Window {
	if w, ok := s.Maintenance[name]; ok {
		return &w
	}
	return nil
}

// MaintenanceEvent is the one event for a maintenance command, or for
// windows that end together: it names all their check groups.
func MaintenanceEvent(kind string, windows []string, w checksandalerts.Window, at time.Time, message string) checksandalerts.Event {
	if w.WithoutMaintenanceOnAndOffAlerts && kind == "maintenance_off" {
		message += "; maintenance start and end are not announced"
	}
	event := checksandalerts.Event{ID: ID(), CheckGroups: windows, Type: kind, AlertSeverity: "info", At: at, Message: message, Maintenance: &w, Notify: !w.WithoutMaintenanceOnAndOffAlerts}
	if len(windows) == 1 {
		event.CheckGroup = windows[0]
	}
	return event
}

// Clone returns a deep copy for building a control change: the copy shares
// no map, slice, or pointer with s. Each time keeps its monotonic reading,
// so the waits of the copy count on the monotonic clock as those of s do; a
// copy through JSON would drop that reading.
// TestCloneCopiesEveryFieldAndSharesNothing fails for a field that Clone
// does not copy.
func (s Snapshot) Clone() Snapshot {
	clone := s
	if s.Checks != nil {
		clone.Checks = make(map[string]*CheckState, len(s.Checks))
		for key, st := range s.Checks {
			clone.Checks[key] = st.clone()
		}
	}
	if s.RecoveryAttemptsByTarget != nil {
		clone.RecoveryAttemptsByTarget = make(map[string]*RecoveryAttempts, len(s.RecoveryAttemptsByTarget))
		for key, attempts := range s.RecoveryAttemptsByTarget {
			copied := *attempts
			clone.RecoveryAttemptsByTarget[key] = &copied
		}
	}
	clone.Maintenance = maps.Clone(s.Maintenance)
	return clone
}

// clone is a deep copy of a check's state, with the states of its findings.
func (s *CheckState) clone() *CheckState {
	clone := *s
	if s.SendAlertsOnlyToDestinations != nil {
		names := slices.Clone(*s.SendAlertsOnlyToDestinations)
		clone.SendAlertsOnlyToDestinations = &names
	}
	clone.QuietWhileTheseChecksFail = slices.Clone(s.QuietWhileTheseChecksFail)
	clone.KeepsTheseChecksQuiet = slices.Clone(s.KeepsTheseChecksQuiet)
	clone.Last = cloneObservation(s.Last)
	clone.FailureIncident, clone.CannotCheckIncident = s.FailureIncident.clone(), s.CannotCheckIncident.clone()
	clone.FailingTimeRunsFailed = slices.Clone(s.FailingTimeRunsFailed)
	clone.ModuleState = slices.Clone(s.ModuleState)
	if s.Findings != nil {
		clone.Findings = make(map[string]*CheckState, len(s.Findings))
		for id, finding := range s.Findings {
			clone.Findings[id] = finding.clone()
		}
	}
	return &clone
}

// clone is a deep copy of an incident lifecycle.
func (t Track) clone() Track {
	clone := t
	if t.Evidence != nil {
		evidence := *t.Evidence
		evidence.Observation = cloneObservation(t.Evidence.Observation)
		clone.Evidence = &evidence
	}
	clone.AnnouncedAlertSeverities = slices.Clone(t.AnnouncedAlertSeverities)
	clone.Reminded = maps.Clone(t.Reminded)
	return clone
}

// cloneObservation is a deep copy of an observation.
func cloneObservation(o checksandalerts.Observation) checksandalerts.Observation {
	clone := o
	clone.Measurements = maps.Clone(o.Measurements)
	clone.Details = slices.Clone(o.Details)
	clone.Findings = slices.Clone(o.Findings)
	if o.ExitCode != nil {
		code := *o.ExitCode
		clone.ExitCode = &code
	}
	return clone
}

// OnThisClock puts every time of a snapshot that the store loaded on the
// monotonic clock of this process, so that each wait that counts from such
// a time, such as the end of a maintenance window, a heartbeat's deadline,
// or the start of an open incident's failing time, counts on the monotonic
// clock after a restart too. A loaded time has only its wall reading, and Go
// compares it with a time that has both on the wall clock, which a step of
// the system clock moves. Each time keeps its distance from now on the wall
// clock, and its wall reading. Daemon start calls it once, right after the
// load. TestOnThisClockPutsEveryLoadedTimeOnTheMonotonicClock fails for a
// time that it leaves out.
func (s *Snapshot) OnThisClock(now time.Time) {
	for _, st := range s.Checks {
		st.onThisClock(now)
	}
	for _, attempts := range s.RecoveryAttemptsByTarget {
		attempts.Last = OnThisClock(attempts.Last, now)
		attempts.LastFinished = OnThisClock(attempts.LastFinished, now)
		attempts.WithheldForFailingTimeSince = OnThisClock(attempts.WithheldForFailingTimeSince, now)
	}
	for name, w := range s.Maintenance {
		w.Until = OnThisClock(w.Until, now)
		s.Maintenance[name] = w
	}
}

// onThisClock puts every time of a check's state, and of the states of its
// findings, on the monotonic clock: see Snapshot.OnThisClock.
func (s *CheckState) onThisClock(now time.Time) {
	for _, at := range []*time.Time{&s.FailingSince, &s.HealthySince, &s.UnknownSince, &s.Deadline, &s.Receipt, &s.Last.At} {
		*at = OnThisClock(*at, now)
	}
	for _, t := range []*Track{&s.FailureIncident, &s.CannotCheckIncident} {
		t.Opened, t.RemindFrom = OnThisClock(t.Opened, now), OnThisClock(t.RemindFrom, now)
		for destination, at := range t.Reminded {
			t.Reminded[destination] = OnThisClock(at, now)
		}
		if e := t.Evidence; e != nil {
			e.Observation.At = OnThisClock(e.Observation.At, now)
			e.FailingSince, e.CannotCheckSince = OnThisClock(e.FailingSince, now), OnThisClock(e.CannotCheckSince, now)
		}
	}
	for _, finding := range s.Findings {
		finding.onThisClock(now)
	}
}

// OnThisClock returns at, a time that MMWatchdog read back from its storage, on the
// monotonic clock of now: the time at the same distance from now on the
// wall clock, with the same wall reading. The zero time stays zero.
func OnThisClock(at, now time.Time) time.Time {
	if at.IsZero() {
		return at
	}
	return now.Add(at.Sub(now))
}

func ID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}

// failingTimeRunsKept is how many runs the on-and-off note looks back.
const failingTimeRunsKept = 10

// Interrupt records a maintenance boundary or a daemon start. Unobserved time
// earns no credit, so the healthy and unknown counts start over, and a
// failing time whose incident has not opened ends. An open failure incident
// keeps its failing time: only observed health resolves it.
func (s *CheckState) Interrupt() {
	s.Touch()
	s.HealthySince, s.UnknownSince = time.Time{}, time.Time{} //nolint:nilaway // EndMaintenance visits existing checks; ReconcileOne and Store.Load allocate every entry, and daemon locking prevents removal during iteration. Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
	for _, unit := range s.FindingStates() {
		unit.Interrupt()
	}
	s.forgetFindingsWithoutIncident()
	if !s.FailureIncidentOpen() {
		s.endFailingTime()
	}
}

// BreakHealthyStreak records a gap in observations or a finished recovery
// action. Neither proves health or ends a failing time, and neither clears
// the time for which MMWatchdog has been unable to check.
func (s *CheckState) BreakHealthyStreak() {
	s.Touch()
	s.HealthySince = time.Time{} //nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
	for _, finding := range s.FindingStates() {
		finding.BreakHealthyStreak()
	}
}

// FindingStates returns the state of each finding of the check, in order of ID,
// with the check's current names and settings, so that a finding's events
// and waiting times are those of its check. For an every_systemd_unit
// check, each failed unit is one finding.
func (s *CheckState) FindingStates() []*CheckState {
	units := make([]*CheckState, 0, len(s.Findings))
	for _, name := range checksandalerts.Names(s.Findings) {
		unit := s.Findings[name]
		unit.CheckGroup, unit.Check, unit.Finding = s.CheckGroup, s.Check, name                                                 //nolint:nilaway // FindingStates visits existing findings; observeFindings allocates each entry, and snapshot cloning preserves allocated entries.
		unit.Source, unit.Timing = s.Source, s.Timing                                                                           //nolint:nilaway // FindingStates visits existing findings; observeFindings allocates each entry, and snapshot cloning preserves allocated entries.
		unit.AlertSeveritySettings, unit.SendAlertsOnlyToDestinations = s.AlertSeveritySettings, s.SendAlertsOnlyToDestinations //nolint:nilaway // FindingStates visits existing findings; observeFindings allocates each entry, and snapshot cloning preserves allocated entries.
		unit.QuietNowWhileCheckFails, unit.KeepsTheseChecksQuiet = s.QuietNowWhileCheckFails, s.KeepsTheseChecksQuiet           //nolint:nilaway // EndMaintenance visits existing checks; ReconcileOne and Store.Load allocate every entry, and daemon locking prevents removal during iteration. FindingStates visits existing findings; observeFindings allocates each entry, and snapshot cloning preserves allocated entries.
		units = append(units, unit)
	}
	return units
}

// FailureIncidentOpen reports an open failure incident of the check, or of
// one of the failed units of an every_systemd_unit check.
func (s *CheckState) FailureIncidentOpen() bool {
	if s.FailureIncident.Incident != "" {
		return true
	}
	//nolint:nilaway // EndMaintenance visits existing checks; ReconcileOne and Store.Load allocate every entry, and daemon locking prevents removal during iteration.
	for _, unit := range s.Findings {
		if unit.FailureIncident.Incident != "" {
			return true
		}
	}
	return false
}

// forgetFindingsWithoutIncident drops the units whose failing time has ended
// and that have no incident left, open or announced, so the check holds only
// the units that still need its attention.
func (s *CheckState) forgetFindingsWithoutIncident() {
	for name, unit := range s.Findings {
		if unit.FailingSince.IsZero() && unit.FailureIncident.Incident == "" && unit.FailureIncident.Announced == "" {
			delete(s.Findings, name) //nolint:nilaway // EndMaintenance visits existing checks; ReconcileOne and Store.Load allocate every entry, and daemon locking prevents removal during iteration.
		}
	}
}

// observeFindings applies one observation of a check whose module reports
// findings, in which the module could check, to each finding: a finding
// that the observation reports gets a failed observation of its own, and a
// finding that it no longer reports a healthy one. Each finding then follows
// the lifecycle of a check by itself: its failing time starts at the first
// observation that reports it, its incident opens after
// alert_after_failing_for, and it recovers when no observation has reported
// it for recovered_after_healthy_for. A second finding is therefore
// announced while the incident of the first is open. For a
// every_systemd_unit check, each failed unit is one finding.
func (s *CheckState) observeFindings(o checksandalerts.Observation, t checksandalerts.Timing, suppressed bool) []checksandalerts.Event {
	messages, exactFindings := map[string]string{}, map[string]string{}
	for _, reported := range o.Findings {
		messages[reported.ID], exactFindings[reported.ID] = reported.Message, reported.ExactFinding
		if s.Findings[reported.ID] == nil {
			if s.Findings == nil {
				s.Findings = map[string]*CheckState{}
			}
			s.Findings[reported.ID] = &CheckState{}
		}
		s.Findings[reported.ID].FindingRecoveryMessage = reported.RecoveryMessage
	}
	gone := o.FindingIsGoneMessage
	if gone == nil {
		gone = checksandalerts.FindingIsGone
	}
	events := []checksandalerts.Event{}
	for _, unit := range s.FindingStates() {
		message := unit.FindingRecoveryMessage
		if message == "" {
			message = gone(unit.Finding)
		}
		own := checksandalerts.Result("ok", message)
		if message, failed := messages[unit.Finding]; failed {
			own = checksandalerts.Result("failed", message)
			// Before the observation, so that the alert that announces the
			// incident carries it.
			unit.FailureIncident.reportExactFinding(exactFindings[unit.Finding])
		}
		own.At, own.Duration = o.At, o.Duration
		events = append(events, unit.Observe(own, t, suppressed)...)
		events = append(events, unit.announceExactFindingChange(o.At, suppressed)...)
	}
	s.forgetFindingsWithoutIncident()
	return events
}

// announceExactFindingChange is the exact_finding_changed alert of a
// finding whose incident is open and announced, when the latest run
// reported another exact finding than the incident's latest alert carried.
// While maintenance or quiet time holds the finding's alerts back
// (suppressed), the change waits: the first run after that time sends it,
// if the exact finding still differs then.
func (s *CheckState) announceExactFindingChange(at time.Time, suppressed bool) []checksandalerts.Event {
	t := &s.FailureIncident
	if suppressed || !t.reminding() || t.CurrentExactFinding == "" || t.CurrentExactFinding == t.AnnouncedExactFinding {
		return nil
	}
	message := checksandalerts.ExactFindingChangedMessage(t.AnnouncedExactFinding, t.CurrentExactFinding)
	t.AnnouncedExactFinding = t.CurrentExactFinding
	return []checksandalerts.Event{s.Event(t, checksandalerts.ExactFindingChanged, t.Incident, message, at, true)}
}

func (s *CheckState) endFailingTime() {
	s.FailingSince, s.FailingTimeRunsFailed = time.Time{}, nil //nolint:nilaway // EndMaintenance visits existing checks; ReconcileOne and Store.Load allocate every entry, and daemon locking prevents removal during iteration.
}

// countRun adds one run of the failing time to the last runs kept.
func (s *CheckState) countRun(failed bool) {
	s.FailingTimeRunsFailed = append(s.FailingTimeRunsFailed, failed)
	if extra := len(s.FailingTimeRunsFailed) - failingTimeRunsKept; extra > 0 {
		s.FailingTimeRunsFailed = s.FailingTimeRunsFailed[extra:] //nolint:nilaway // EndMaintenance visits existing checks; ReconcileOne and Store.Load allocate every entry, and daemon locking prevents removal during iteration.
	}
}

// onAndOffRuns says, for a check that failed on and off during its failing
// time, how many of its last runs failed and how many runs that counts. Both
// are 0 when every run failed. The body of the alert says it.
func (s *CheckState) onAndOffRuns() (failed, counted int) {
	for _, runFailed := range s.FailingTimeRunsFailed {
		if runFailed {
			failed++
		}
	}
	if failed == len(s.FailingTimeRunsFailed) {
		return 0, 0
	}
	return failed, len(s.FailingTimeRunsFailed) //nolint:nilaway // EndMaintenance visits existing checks; ReconcileOne and Store.Load allocate every entry, and daemon locking prevents removal during iteration.
}

// EndMaintenance removes a window, announces its end, and interrupts the
// streaks of the checks it covered. A heartbeat has no executions to
// reconcile it afterward, so once no window covers it, its latest
// observation is applied again: an open incident is announced, or an
// announced one that recovered during maintenance is closed. The end is
// queued before those transitions. Reminders of incidents still open resume
// one interval after the end.
// The messages a maintenance_off event starts with when MMWatchdog, not an
// operator, ends the window.
const (
	MaintenanceExpiredMessage = "maintenance expired"
	MaintenanceRemovedMessage = "maintenance ended because the check group was removed"
)

// EndMaintenance removes the named windows that exist. Windows with the same
// end time, reason, user, and announcement choice end with one
// maintenance_off event together.
func (s *Snapshot) EndMaintenance(windows []string, now time.Time, message string) []checksandalerts.Event {
	together := map[string][]string{}
	ended, order := []string{}, []string{}
	for _, name := range windows {
		w, open := s.Maintenance[name]
		if !open {
			continue
		}
		key := w.Until.UTC().Format(time.RFC3339Nano) + "\x00" + w.Reason + "\x00" + w.User + "\x00" + fmt.Sprint(w.WithoutMaintenanceOnAndOffAlerts)
		if together[key] == nil {
			order = append(order, key)
		}
		together[key] = append(together[key], name)
		ended = append(ended, name)
	}
	events := []checksandalerts.Event{}
	for _, key := range order {
		w, text := s.Maintenance[together[key][0]], message //nolint:nilaway // Every key added to order receives a window name in together before the indexing loop begins.
		if w.Reason != "" {
			text += ": " + w.Reason
		}
		events = append(events, MaintenanceEvent("maintenance_off", together[key], w, now, text))
	}
	for _, name := range ended {
		delete(s.Maintenance, name)
	}
	covered := func(group string) bool {
		return slices.ContainsFunc(ended, func(window string) bool { return Covers(window, group) })
	}
	for _, key := range checksandalerts.Names(s.Checks) {
		st := s.Checks[key]
		//nolint:nilaway // EndMaintenance visits existing checks; ReconcileOne and Store.Load allocate every entry, and daemon locking prevents removal during iteration.
		if !covered(st.CheckGroup) {
			continue
		}
		st.Interrupt()
		// The scheduler tick reconciles fleet checks, which a reported
		// window can also suppress.
		//nolint:nilaway // EndMaintenance visits existing checks; ReconcileOne and Store.Load allocate every entry, and daemon locking prevents removal during iteration.
		if !st.Deadline.IsZero() && st.CheckGroup != checksandalerts.NodeReportsCheckGroup && st.Last.Status != "unknown" && !s.InMaintenance(st.CheckGroup) {
			// The heartbeat's evidence still holds at the window's end, and
			// the alert says so at that time: an alert dated at the start of a
			// long window would be older than keep_undelivered_alerts_for, and
			// the commit that queues it would remove it.
			latest := st.Last //nolint:nilaway // EndMaintenance visits existing checks; ReconcileOne and Store.Load allocate every entry, and daemon locking prevents removal during iteration.
			latest.At = now
			events = append(events, st.Observe(latest, st.Timing, false)...) //nolint:nilaway // EndMaintenance visits existing checks; ReconcileOne and Store.Load allocate every entry, and daemon locking prevents removal during iteration.
		}
		if !s.InMaintenance(st.CheckGroup) {
			st.ResumeReminders(now)
		}
	}
	return events
}
func (s *CheckState) Event(t *Track, kind, incident, msg string, at time.Time, notify bool) checksandalerts.Event {
	if len(msg) > 4096 {
		msg = msg[:4096] + "…"
	}
	var evidence *checksandalerts.Evidence
	if t.Evidence != nil && t.Evidence.Incident == incident {
		evidence = t.Evidence
	}
	// A check that names no destination at all, with an empty list, sends
	// no new alert. Closing an announced incident still reaches its recipients.
	silent := s.SendAlertsOnlyToDestinations != nil && len(*s.SendAlertsOnlyToDestinations) == 0 && !checksandalerts.Closes(kind)
	e := checksandalerts.Event{ID: ID(), Incident: incident, CheckGroup: s.CheckGroup, Check: s.Check, Finding: s.Finding, Type: kind, AlertSeverity: t.AlertSeverity, At: at, Message: msg, Notify: notify && !silent, Evidence: evidence, SendAlertsOnlyToDestinations: s.SendAlertsOnlyToDestinations}
	// Every alert about a finding with an exact finding names the latest
	// one, so the hub groups each by it (checksandalerts.HasExactFinding).
	if t.CurrentExactFinding != "" {
		e.ExactFinding, e.ExactFindingSequence = t.CurrentExactFinding, t.ExactFindingSequence
	}
	// Every alert of the incident carries its key across the fleet. This
	// is the one place that knows the incident kind of every such alert,
	// a reminder's and a retirement's too. Store.Save gives the hub's
	// alerts about silent nodes the one key that they share.
	if checksandalerts.BelongsToAnIncident(kind) {
		incidentKind := checksandalerts.FailureIncidentEvents.Failed
		if t == &s.CannotCheckIncident {
			incidentKind = checksandalerts.CannotCheckIncidentEvents.Failed
		}
		e.FleetIncidentKey = checksandalerts.FleetIncidentKey(checksandalerts.ReadableFleetIncidentKey(e, incidentKind, ""))
	}
	if checksandalerts.Closes(kind) && incident == t.Announced && len(t.AnnouncedAlertSeverities) > 0 {
		e.AlertSeveritiesSentForIncident = t.AnnouncedAlertSeverities
		e.AlertSeverity = config.HighestSeverity(t.AnnouncedAlertSeverities)
	}
	// An alert that says that the check fails names the checks that MMWatchdog
	// keeps quiet while it does.
	if t == &s.FailureIncident && (kind == checksandalerts.FailureIncidentEvents.Failed || kind == checksandalerts.FailureIncidentEvents.Raised || kind == "reminder") {
		e.NoAlertsOrRecoveryActionsForTheseChecksWhileThisCheckFails, e.NumberOfChecksWithNoAlertsOrRecoveryActionsWhileThisCheckFails = config.NamedInAnAlert(s.KeepsTheseChecksQuiet)
	}
	// An event that is only recorded while the check is kept quiet says why
	// no alert went out, for a person who reads the event history later.
	if s.QuietNowWhileCheckFails != "" && !e.Notify {
		e.Message += "; no alert while " + s.QuietNowWhileCheckFails + " fails"
	}
	return e
}

// Observe applies one accepted observation. Observation time, never process
// uptime, is confirmation evidence. An unknown observation affects only the
// monitoring track; ok and failed observations resolve monitoring immediately
// and move the health track.
//
// The health track follows the check's failing time. The first failed
// observation starts it. A failed observation at or after
// alert_after_failing_for into it opens the incident; a healthy observation
// never opens one. The failing time ends, and an open incident resolves,
// when the check has been healthy for recovered_after_healthy_for; a shorter
// healthy period leaves it running.
//
// An every_systemd_unit check opens no failure incident of its own:
// each failed unit has one (observeFindings). The check keeps its own failing
// time, which lasts while systemd lists any unit as failed, for the waiting
// time of a restart or recovery command and for its state. When MMWatchdog cannot
// ask systemd, nothing was observed about the units either: the check's
// cannot-check track runs as for every check, a unit's healthy time starts
// over, and each unit keeps its failing time.
func (s *CheckState) Observe(o checksandalerts.Observation, t checksandalerts.Timing, suppressed bool) []checksandalerts.Event {
	now := o.At
	s.Touch()
	s.Last = o //nolint:nilaway // EndMaintenance visits existing checks; ReconcileOne and Store.Load allocate every entry, and daemon locking prevents removal during iteration.
	severity := s.AlertSeveritySettings.AlertSeverity
	raiseAfter := s.AlertSeveritySettings.RaiseToSosAfterFailingFor.Value()
	switch o.Status {
	case "unknown":
		// Unknown is no evidence of health; both failure clocks keep running.
		s.BreakHealthyStreak()
		if s.UnknownSince.IsZero() {
			s.UnknownSince = now
		}
		if s.RecordsOnly {
			// A check that only records never alerts: status shows it unknown.
			return nil
		}
		// A cannot-check incident takes the check's current severity,
		// including a raise of its open failure incident, and sos once MMWatchdog
		// has been unable to observe the check for raise_to_sos_after_failing_for.
		if s.FailureIncident.Incident != "" {
			severity = config.HigherSeverity(severity, s.FailureIncident.AlertSeverity)
		}
		for _, unit := range s.Findings {
			if unit.FailureIncident.Incident != "" {
				severity = config.HigherSeverity(severity, unit.FailureIncident.AlertSeverity)
			}
		}
		unknownFor := now.Sub(s.UnknownSince)
		if raiseAfter > 0 && unknownFor >= raiseAfter {
			severity = "sos"
		}
		return s.advance(&s.CannotCheckIncident, checksandalerts.CannotCheckIncidentEvents, o, true, unknownFor >= t.CannotCheckAlertAfter(), suppressed, "", t, severity)
	case "failed":
		s.UnknownSince, s.HealthySince = time.Time{}, time.Time{}
		if s.FailingSince.IsZero() {
			s.FailingSince, s.FailingTimeRunsFailed = now, nil
		}
		s.countRun(true)
	default:
		s.UnknownSince = time.Time{}
		if s.HealthySince.IsZero() {
			s.HealthySince = now
		}
		if !s.FailingSince.IsZero() {
			s.countRun(false)
		}
	}
	if s.RecordsOnly {
		// A check that only records never opens an incident, also when a
		// read-only filesystem is observed; status shows it.
		s.endFailingTime()
		return nil
	}
	events := s.advance(&s.CannotCheckIncident, checksandalerts.CannotCheckIncidentEvents, o, false, true, suppressed, "MMWatchdog can run the check again", t, "")
	if s.OneIncidentForEachFinding {
		events = append(events, s.observeFindings(o, t, suppressed)...)
		if o.Status != "failed" && now.Sub(s.HealthySince) >= t.RecoveredAfterHealthyFor.Value() {
			s.endFailingTime()
		}
		return events
	}
	if o.Status == "failed" {
		failingFor := now.Sub(s.FailingSince)
		// A raise that is due when the incident opens makes its first alert sos.
		if raiseAfter > 0 && failingFor >= raiseAfter {
			severity = "sos"
		}
		return append(events, s.advance(&s.FailureIncident, checksandalerts.FailureIncidentEvents, o, true, failingFor >= t.AlertAfterFailingFor.Value(), suppressed, "", t, severity)...)
	}
	recovered := now.Sub(s.HealthySince) >= t.RecoveredAfterHealthyFor.Value()
	events = append(events, s.advance(&s.FailureIncident, checksandalerts.FailureIncidentEvents, o, false, recovered, suppressed, o.Message, t, "")...)
	if recovered {
		s.endFailingTime()
	}
	return events
}

// advance applies the lifecycle table in docs/changing-mmwatchdog/implementation-rules.md to one track.
// failing reports whether the observation is a failure for this track;
// confirmed whether its streak meets the confirmation requirement; severity
// is the alert severity an incident it opens takes.
func (s *CheckState) advance(t *Track, kinds checksandalerts.TrackEvents, o checksandalerts.Observation, failing, confirmed, suppressed bool, recovered string, timing checksandalerts.Timing, severity string) []checksandalerts.Event {
	events := []checksandalerts.Event{}
	add := func(kind, id, msg string, notify bool) {
		events = append(events, s.Event(t, kind, id, msg, o.At, notify))
	}
	switch {
	case failing && confirmed && t.Incident == "":
		if config.SeverityRank(severity) < 0 {
			// Every path gives an incident its severity; an incident
			// without one would be refused by the hub and destinations.
			log.Printf("internal error: %s/%s opened an incident without an alert severity; sending it as sos", s.CheckGroup, s.Check) //nolint:nilaway // EndMaintenance visits existing checks; ReconcileOne and Store.Load allocate every entry, and daemon locking prevents removal during iteration.
			severity = "sos"
		}
		t.Incident, t.Opened, t.AlertSeverity = ID(), o.At, severity
		t.Evidence = checksandalerts.CaptureEvidence(t.Incident, o, s.Source, timing)
		if kinds == checksandalerts.FailureIncidentEvents {
			t.Evidence.FailingSince = s.FailingSince
			t.Evidence.LastRunsFailed, t.Evidence.LastRunsCounted = s.onAndOffRuns()
		} else {
			t.Evidence.CannotCheckSince = s.UnknownSince
		}
		if suppressed {
			// Record locally; announcement waits for maintenance to end. The
			// event says so, because the first run after the window records a
			// second event for the same incident, and that one sends the alert.
			// A check kept quiet by another failing check gets its own words in Event.
			message := o.Message
			if s.QuietNowWhileCheckFails == "" {
				message += " (no alert sent: alerts were paused by a maintenance window)"
			}
			add(kinds.Failed, t.Incident, message, false)
		}
	case !failing && confirmed && t.Incident != "":
		announced := !suppressed && t.Announced == t.Incident
		add(kinds.Recovered, t.Incident, recovered, announced)
		if announced {
			t.Announced = ""
		}
		t.Incident, t.Opened, t.AlertSeverity = "", time.Time{}, ""
	}
	if !suppressed {
		if failing && t.Incident != "" && t.Announced != t.Incident {
			if t.Announced != "" {
				add(kinds.Recovered, t.Announced, kinds.Stale, true)
			}
			add(kinds.Failed, t.Incident, o.Message, true)
			t.Announced, t.RemindFrom, t.Reminded = t.Incident, o.At, nil
			t.AnnouncedAlertSeverities = []string{t.AlertSeverity}
			t.AnnouncedExactFinding = t.CurrentExactFinding
		}
		if !failing && t.Incident == "" && t.Announced != "" {
			add(kinds.Recovered, t.Announced, recovered, true)
			t.Announced = ""
		}
	}
	if t.Announced == "" {
		t.RemindFrom, t.Reminded, t.AnnouncedAlertSeverities, t.AnnouncedExactFinding = time.Time{}, nil, nil, ""
	}
	if t.Incident == "" && t.Announced == "" {
		t.Evidence = nil
	}
	return events
}

// Reminders returns the reminders due for the check's announced
// incidents that are still open: one to each destination in destinations,
// the destinations in force, that gets the reminder and whose remind_every
// has passed since the incident was announced or raised, its reminders
// resumed after maintenance, or its previous reminder to that destination.
// Whether a destination gets it is config.GoesTo, the rule that queues it,
// so MMWatchdog makes no reminder that no destination gets. The caller holds
// reminders while maintenance covers the check.
func (s *CheckState) Reminders(destinations map[string]checksandalerts.Destination, now time.Time) []checksandalerts.Event {
	events := []checksandalerts.Event{}
	for _, track := range []struct {
		t     *Track
		kinds checksandalerts.TrackEvents
	}{{&s.CannotCheckIncident, checksandalerts.CannotCheckIncidentEvents}, {&s.FailureIncident, checksandalerts.FailureIncidentEvents}} {
		t := track.t
		if !t.reminding() {
			continue
		}
		opened := t.Opened.UTC().Format("15:04 UTC")
		msg := checksandalerts.ReminderMessage(track.kinds, checksandalerts.AboutCheck(s.CheckGroup, s.Check, s.Finding), opened, s.Last.Message)
		if s.CheckGroup == checksandalerts.NodeReportsCheckGroup {
			// The latest message of a check of the fleet says since when
			// no report has arrived (Daemon.silenceMessage).
			msg = s.Last.Message + " (alert open since " + opened + ")"
		}
		for _, name := range checksandalerts.Names(destinations) {
			every := destinations[name].RemindEvery.Value()
			if every <= 0 || now.Sub(MaxTime(t.RemindFrom, t.Reminded[name])) < every {
				continue
			}
			event := s.Event(t, "reminder", t.Incident, msg, now, true)
			event.Destination = name
			if !config.GoesTo(name, destinations[name], event, destinations) {
				continue
			}
			if t.Reminded == nil {
				t.Reminded = map[string]time.Time{}
			}
			t.Reminded[name] = now
			events = append(events, event)
		}
	}
	// Each failed unit of an every_systemd_unit check is reminded of by
	// itself.
	for _, unit := range s.FindingStates() {
		events = append(events, unit.Reminders(destinations, now)...)
	}
	if len(events) > 0 {
		s.Touch()
	}
	return events
}

// Reminding reports that the check, or one of the failed units of a
// every_systemd_unit check, has an announced incident that is still
// open.
func (s *CheckState) Reminding() bool {
	if s.FailureIncident.reminding() || s.CannotCheckIncident.reminding() {
		return true
	}
	for _, unit := range s.Findings {
		if unit.FailureIncident.reminding() {
			return true
		}
	}
	return false
}

// ResumeReminders starts the reminder interval of announced incidents that
// are still open again, when the maintenance that held them ends.
func (s *CheckState) ResumeReminders(now time.Time) {
	s.Touch()
	//nolint:nilaway // EndMaintenance visits existing checks; ReconcileOne and Store.Load allocate every entry, and daemon locking prevents removal during iteration.
	for _, t := range []*Track{&s.FailureIncident, &s.CannotCheckIncident} {
		if t.reminding() {
			t.RemindFrom = now
		}
	}
	for _, unit := range s.FindingStates() {
		unit.ResumeReminders(now)
	}
}

// retire closes a track's incidents after its definition changed or was
// removed. Only announced incidents notify destinations.
func (s *CheckState) retire(t *Track, reason string, now time.Time) []checksandalerts.Event {
	s.Touch()
	events := []checksandalerts.Event{}
	if t.Incident != "" {
		events = append(events, s.Event(t, "closed_without_recovery", t.Incident, reason, now, t.Incident == t.Announced))
	}
	if t.Announced != "" && t.Announced != t.Incident {
		events = append(events, s.Event(t, "closed_without_recovery", t.Announced, reason, now, true))
	}
	*t = Track{}
	return events
}

// retirementReason is the message of the alert that closes a check's
// incidents without recovery: why the check ended, and that MMWatchdog does
// not know whether the problem is fixed. replaced reports that a new
// definition with another identity takes the check's place; otherwise the
// check has none. A check's identity leaves out its timings, severity, and
// destinations (config.Definition.Identity), so the check that watches the
// hub is never replaced, and a joined node's check is replaced only when
// the node joins again.
func retirementReason(c *config.Config, st *CheckState, replaced bool) string {
	var why string
	switch {
	case st.CheckGroup == checksandalerts.NodeReportsCheckGroup && st.Check == checksandalerts.HubCheck && c.FleetHubRole == nil:
		// The node no longer has the hub-outage alerter role, or left the fleet.
		why = "this node no longer watches the MMWatchdog fleet hub"
	case st.CheckGroup == checksandalerts.NodeReportsCheckGroup && replaced:
		why = st.Check + " joined the fleet again as a new node"
	case st.CheckGroup == checksandalerts.NodeReportsCheckGroup && c.FleetHubRole == nil:
		why = "this node is no longer the MMWatchdog fleet hub"
	case st.CheckGroup == checksandalerts.NodeReportsCheckGroup:
		why = st.Check + " was removed from the fleet"
	case replaced:
		why = "the check was changed in the configuration"
	default:
		why = "the check was removed from the configuration"
	}
	return why + "; MMWatchdog does not know whether the problem is fixed"
}

// sameNames compares two optional lists of names; a list that is not stated
// differs from an empty one.
func sameNames(a, b *[]string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return slices.Equal(*a, *b)
}

// Reconcile matches persisted checks to the definitions of a
// configuration and fleet membership. Unchanged definitions keep their state;
// changed and removed ones retire.
func (s *Snapshot) Reconcile(c *config.Config, defs map[string]config.Definition, now time.Time) []checksandalerts.Event {
	events := []checksandalerts.Event{}
	for key := range s.Checks {
		if _, exists := defs[key]; !exists {
			events = append(events, s.ReconcileOne(c, key, nil, now)...)
		}
	}
	for key, d := range defs {
		events = append(events, s.ReconcileOne(c, key, &d, now)...)
	}
	// A window whose check group is gone ends after that group's
	// checks have retired, so its end cannot reconcile them.
	for _, n := range checksandalerts.Names(s.Maintenance) {
		if _, ok := c.CheckGroups[n]; !ok && n != checksandalerts.EveryCheckGroup && (n != checksandalerts.NodeReportsCheckGroup || !c.InFleet()) {
			events = append(events, s.EndMaintenance([]string{n}, now, MaintenanceRemovedMessage)...)
		}
	}
	return events
}

// ReconcileOne matches one check to its definition d, or to none when d is
// nil. A fleet membership change reconciles only its own node's check.
func (s *Snapshot) ReconcileOne(c *config.Config, key string, d *config.Definition, now time.Time) []checksandalerts.Event {
	events := []checksandalerts.Event{}
	if st := s.Checks[key]; st != nil {
		if d != nil && st.Fingerprint == d.Identity() {
			source, recordsOnly := c.CheckSource(d.CheckGroup, d.Name), d.CheckWithSeverity().RecordsOnly()
			if st.Source != source || st.Timing != d.Timing || st.RecordsOnly != recordsOnly {
				st.Touch()
				st.Source, st.Timing, st.RecordsOnly = source, d.Timing, recordsOnly
			}
			if !d.SeverityFromReports && st.AlertSeveritySettings != d.Severity {
				st.Touch()
				// The scheduler tick raises open incidents to it.
				st.AlertSeveritySettings = d.Severity
			}
			if !sameNames(st.SendAlertsOnlyToDestinations, d.SendAlertsOnlyToDestinations) {
				st.Touch()
				st.SendAlertsOnlyToDestinations = d.SendAlertsOnlyToDestinations
			}
			if !slices.Equal(st.QuietWhileTheseChecksFail, d.QuietWhileTheseChecksFail) || !slices.Equal(st.KeepsTheseChecksQuiet, d.KeepsTheseChecksQuiet) {
				st.Touch()
				st.QuietWhileTheseChecksFail, st.KeepsTheseChecksQuiet = d.QuietWhileTheseChecksFail, d.KeepsTheseChecksQuiet
				// A check that no longer names the check that kept it quiet
				// is no longer kept quiet by it.
				if !slices.Contains(st.QuietWhileTheseChecksFail, st.QuietNowWhileCheckFails) {
					st.QuietNowWhileCheckFails = ""
				}
			}
			return events
		}
		reason := retirementReason(c, st, d != nil)
		events = append(events, st.retire(&st.FailureIncident, reason, now)...)
		events = append(events, st.retire(&st.CannotCheckIncident, reason, now)...)
		// A changed or removed every_systemd_unit check closes the
		// incident of each failed unit too.
		for _, unit := range st.FindingStates() {
			events = append(events, unit.retire(&unit.FailureIncident, reason, now)...)
		}
		delete(s.Checks, key)
	}
	if d == nil {
		return events
	}
	st := &CheckState{CheckGroup: d.CheckGroup, Check: d.Name, Fingerprint: d.Identity(), Last: checksandalerts.Result("unknown", "has not run yet")}
	st.Touch()
	st.Source = c.CheckSource(d.CheckGroup, d.Name)
	st.Timing = d.Timing
	st.AlertSeveritySettings = d.Severity
	st.SendAlertsOnlyToDestinations = d.SendAlertsOnlyToDestinations
	st.QuietWhileTheseChecksFail, st.KeepsTheseChecksQuiet = d.QuietWhileTheseChecksFail, d.KeepsTheseChecksQuiet
	st.RecordsOnly = d.CheckWithSeverity().RecordsOnly()
	st.OneIncidentForEachFinding = d.Check.Module().OneIncidentForEachFinding
	if d.Heartbeat != nil {
		st.Deadline = now.Add(d.Heartbeat.MaxTimeBetweenHeartbeats.Value())
		st.Last.Message = "no heartbeat has arrived yet"
		if d.CheckGroup == checksandalerts.NodeReportsCheckGroup {
			st.Last.Message = "no report has arrived yet"
		}
	}
	s.Checks[key] = st
	return events
}

// Summary names the check's state at now. needsFresh reports that no
// observation has been accepted since earlier evidence stopped counting.
func (s CheckState) Summary(now time.Time, needsFresh bool) string {
	unknown := needsFresh || s.Last.Status == "unknown"
	// An every_systemd_unit check is failed while one of its units has
	// an open incident.
	open := s.FailureIncidentOpen()
	switch {
	case open && !unknown && s.Last.Status == "ok":
		return "confirming_recovery"
	case open:
		return "failed"
	case !s.Deadline.IsZero() && s.Receipt.IsZero() && s.Last.Status == "unknown" && now.Before(s.Deadline):
		if s.CheckGroup == checksandalerts.NodeReportsCheckGroup {
			return "waiting_for_first_report"
		}
		return "waiting_for_first_heartbeat"
	case unknown:
		return "unknown"
	case s.Last.Status == "failed" || !s.FailingSince.IsZero():
		// Inside a failing time whose incident has not opened, also while
		// the latest run of a check that fails on and off was healthy.
		return "confirming_failure"
	}
	return "ok"
}

// MarkReminderEligible sets ReminderEligible on each track of the check's
// status and of its findings' statuses: true for an incident that is open
// and announced, unless heldBack, which reports that a maintenance window
// or quiet time holds the check's alerts back.
func (s *CheckStatus) MarkReminderEligible(heldBack bool) {
	for _, t := range []*Track{&s.FailureIncident, &s.CannotCheckIncident} {
		t.ReminderEligible = !heldBack && t.reminding()
	}
	for name, finding := range s.FindingStatuses {
		finding.MarkReminderEligible(heldBack)
		s.FindingStatuses[name] = finding
	}
}

// ObservationUnknown reports that the latest observation does not establish health.
func (s CheckStatus) ObservationUnknown() bool {
	return s.NeedsFresh || s.Last.Status == "unknown"
}

// Explanation is the latest message, noting when an open incident is no
// longer confirmed by a current observation, and when the latest result
// stopped counting, such as after MMWatchdog started: the check is unknown
// until its next run, and its earlier message would read as the reason.
// Beside a healthy latest run, a confirming state says what it waits for,
// or the state and the message would read as a contradiction.
func (s CheckStatus) Explanation() string {
	switch {
	case s.failureIncidentOpen() && s.ObservationUnknown():
		return "no current result; the failure alert stays open: " + s.Last.Message
	case s.NeedsFresh && s.Last.Status != "unknown":
		return "waiting for a new run; the earlier result no longer counts: " + s.Last.Message
	case s.State == "confirming_recovery":
		return fmt.Sprintf("the latest run is healthy; the alert closes once the check has been healthy for %s (recovered_after_healthy_for): %s", s.Timing.RecoveredAfterHealthyFor, s.Last.Message)
	case s.State == "confirming_failure" && s.Last.Status != "failed":
		return fmt.Sprintf("the latest run is healthy, but runs have failed since %s; the check counts as failing until it has been healthy for %s (recovered_after_healthy_for), so that a check that fails on and off still alerts: %s", s.FailingSince.UTC().Format("15:04:05 UTC"), s.Timing.RecoveredAfterHealthyFor, s.Last.Message)
	}
	return s.Last.Message
}

// FailureIncidentOpen reports an open failure incident of the check, or of
// one of the failed units of an every_systemd_unit check.
func (s CheckStatus) failureIncidentOpen() bool {
	if s.FailureIncident.Incident != "" {
		return true
	}
	for _, unit := range s.FindingStatuses {
		if unit.FailureIncident.Incident != "" {
			return true
		}
	}
	return false
}

// Attention is one check that needs an operator.
type Attention struct {
	Check string `json:"check"`
	// Finding is the failed unit, for an entry about one unit of a
	// every_systemd_unit check: each unit with an open incident has an
	// entry of its own.
	Finding string    `json:"finding,omitempty"`
	State   string    `json:"state"`
	Since   time.Time `json:"since,omitzero"`
	Message string    `json:"message"`
}

// StatusSummary is the verdict that status, fleet reports, and the TUI share:
// the status exit code and the checks that make it nonzero.
type StatusSummary struct {
	ExitCode  int         `json:"exit_code"`
	Attention []Attention `json:"attention"`
}

// Summarize derives the verdict for the given checks. An open health
// incident needs attention (exit 1); unknown health outside a heartbeat's
// initial grace, or a storage problem, is exit 3.
func Summarize(checks map[string]CheckStatus, storageError string) StatusSummary {
	summary := StatusSummary{Attention: []Attention{}}
	for _, key := range checksandalerts.Names(checks) {
		st := checks[key]
		for _, name := range checksandalerts.Names(st.FindingStatuses) {
			if unit := st.FindingStatuses[name]; unit.FailureIncident.Incident != "" {
				summary.ExitCode = HealthExitCode(summary.ExitCode, 1)
				summary.Attention = append(summary.Attention, Attention{Check: key, Finding: name, State: unit.State, Since: unit.FailureIncident.Opened, Message: unit.Explanation()})
			}
		}
		unknown := st.State != "waiting_for_first_report" && st.State != "waiting_for_first_heartbeat" && st.ObservationUnknown()
		if st.FailureIncident.Incident == "" && !unknown {
			continue
		}
		since := st.FailureIncident.Opened
		if st.FailureIncident.Incident != "" {
			summary.ExitCode = HealthExitCode(summary.ExitCode, 1)
		} else if st.CannotCheckIncident.Incident != "" {
			since = st.CannotCheckIncident.Opened
		} else {
			since = st.UnknownSince
		}
		if unknown {
			summary.ExitCode = HealthExitCode(summary.ExitCode, 3)
		}
		summary.Attention = append(summary.Attention, Attention{Check: key, State: st.State, Since: since, Message: st.Explanation()})
	}
	if storageError != "" {
		summary.ExitCode = HealthExitCode(summary.ExitCode, 3)
	}
	return summary
}

// HealthExitCode combines health outcomes: failure takes precedence over unknown.
func HealthExitCode(a, b int) int {
	if a == 1 || b == 1 {
		return 1
	}
	if a == 3 || b == 3 {
		return 3
	}
	return 0
}

// CheckFingerprints lists the identity of every check of a snapshot, to
// find afterwards which checks a start or a reload kept.
func CheckFingerprints(s *Snapshot) map[string]string {
	fingerprints := map[string]string{}
	for key, st := range s.Checks {
		fingerprints[key] = st.Fingerprint
	}
	return fingerprints
}

// TakeAwayModuleStateOfChecksThatAreGone takes away the state directory of
// every check that a start or a reload did not keep: a check that the
// configuration no longer has, and a check that changed so that MMWatchdog treats
// it as a new one. before are the identities of the checks before the start
// or the reload, and kept the checks after it.
//
// Taking a directory away is one rename into RemovedStateDirectory, which
// costs the same whatever the directory holds, so the daemon does it while
// it holds its lock: a new check of the same name then starts with no
// state. The caller runs remove afterwards, without the lock. It removes
// what was taken away, which takes as long as the module's account made the
// directory large.
//
// MMWatchdog names each directory by the check that it belonged to, and never takes
// away what it finds by listing module_state_dir.
func TakeAwayModuleStateOfChecksThatAreGone(base string, before map[string]string, kept *Snapshot) (remove func(), e error) {
	takenAway := []string{}
	remove = func() {
		for _, directory := range takenAway {
			os.RemoveAll(directory)
		}
	}
	for _, key := range checksandalerts.Names(before) {
		if st := kept.Checks[key]; st != nil && before[key] == st.Fingerprint {
			continue
		}
		checkGroup, check, named := strings.Cut(key, "/")
		if !named || !checksandalerts.NamePattern.MatchString(checkGroup) || !checksandalerts.NamePattern.MatchString(check) {
			continue
		}
		own := filepath.Join(base, checkGroup, check)
		if _, statError := os.Lstat(own); statError != nil {
			continue
		}
		// The directory is MMWatchdog's to take away only below directories that
		// MMWatchdog made.
		for _, above := range []string{base, filepath.Dir(own)} {
			if problem := checksandalerts.NotMMWatchdogsOwnDirectory(above, 0711); problem != nil {
				return remove, errors.Join(e, problem)
			}
		}
		removed := filepath.Join(base, checksandalerts.RemovedStateDirectory)
		if problem := checksandalerts.MakeMMWatchdogsOwnDirectory(removed, 0700); problem != nil {
			return remove, errors.Join(e, problem)
		}
		aside, problem := os.MkdirTemp(removed, checkGroup+"-"+check+"-")
		if problem == nil {
			problem = os.Rename(own, filepath.Join(aside, "state"))
		}
		if problem != nil {
			e = errors.Join(e, problem)
			continue
		}
		takenAway = append(takenAway, aside)
		// The directory of the check group goes with its last check. Remove
		// refuses a directory that still holds one.
		_ = os.Remove(filepath.Dir(own))
	}
	return remove, e
}
