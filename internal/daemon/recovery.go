package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/primitives"
)

// healthySince derives when a budget's current healthy window started: the
// latest healthy-streak start among its governing checks, all of which
// must be healthy with no open incident. A zero time means the budget's
// checks are not currently healthy.
func (d *Daemon) healthySince(key string) time.Time {
	p := d.policies[key]
	if p == nil {
		return time.Time{}
	}
	var since time.Time
	for _, check := range p.When {
		st := d.snapshot.Checks[check]
		if st == nil || st.HealthySince.IsZero() || st.FailureIncidentOpen() {
			return time.Time{}
		}
		since = incidents.MaxTime(since, st.HealthySince)
	}
	return since
}

// recoveryDue is the only test for a restart or a recovery command. It
// returns the governing check whose failure calls for the action, or ""
// when none does: a check whose latest observation failed and came when its
// failing time had lasted the action's waiting time. The incident of the
// check need not be open: the action's waiting time and the alert's count
// from the same start, each on its own, and both to the check's own
// observation, never to the moment that another check's run completes. So
// an action and an alert that wait equally long come with the same
// observation. unitState is the unit's systemd state
// when a restart has just inspected it, or "" otherwise. A unit that became
// active since its failed observation no longer justifies a restart through
// its systemd_unit_state check. A check that is kept quiet while a check
// that it names as its cause fails calls for no action (quietWhile). A check
// group's recovery command and a restart of one of its units never run at
// once. The caller holds d.mu.
func (d *Daemon) recoveryDue(key string, now time.Time, unitState string) string {
	p := d.policies[key]
	if p == nil {
		return ""
	}
	b := d.snapshot.RecoveryAttemptsByTarget[key]
	if b == nil {
		b = &incidents.RecoveryAttempts{}
	}
	// The wait counts from the end of the last attempt. An attempt that
	// MMWatchdog never saw end, because MMWatchdog stopped while it ran,
	// has no end, so the wait counts from its start.
	lastAttempt := b.LastFinished
	if b.Last.After(lastAttempt) {
		lastAttempt = b.Last
	}
	if b.Attempts >= p.Attempts || now.Sub(lastAttempt) < p.MinTimeBetweenAttempts {
		return ""
	}
	for _, group := range p.CheckGroups {
		if d.suppressed(group) {
			return ""
		}
		for _, other := range append([]string{group}, d.config.CheckGroups[group].SystemdUnits()...) {
			if other != key && d.recovering[other] {
				return ""
			}
		}
	}
	for _, check := range p.When {
		st, rc := d.snapshot.Checks[check], d.checks[check]
		if st == nil || rc == nil {
			continue
		}
		// A unit that runs again calls for no restart through the check
		// that says that this unit must run.
		if module, ok := rc.def.Check.Inputs.(checksandalerts.RequiresActiveUnit); ok && module.UnitThatMustBeActive() == key && primitives.UnitRuns(unitState) {
			continue
		}
		// A check that is kept quiet while a check that it names fails
		// calls for no action. Another check of the same action still does.
		if d.quietWhile(check) != "" {
			continue
		}
		// The waiting time counts to the check's own latest observation.
		timing := rc.def.Timing
		failingLongEnough := !st.FailingSince.IsZero() && st.Last.At.Sub(st.FailingSince) >= p.After[check]
		if !rc.needsFresh() && failingLongEnough && st.Last.Status == "failed" && now.Sub(st.Last.At) <= timing.CheckEvery.Value()+timing.TimeLimitPerRun.Value() {
			return check
		}
	}
	return ""
}

// startRecovery refills budgets after a full healthy window and starts due
// actions. Recovery commands start first, so a restart due at the same time
// waits for a failure observed after the command. The caller holds d.mu and
// commits the record afterward.
func (d *Daemon) startRecovery(ctx context.Context, now time.Time) {
	keys := checksandalerts.Names(d.policies)
	sort.SliceStable(keys, func(i, j int) bool { return d.policies[keys[i]].Run != nil && d.policies[keys[j]].Run == nil })
	for _, key := range keys {
		if d.recovering[key] {
			continue
		}
		p := d.policies[key]
		b := d.snapshot.RecoveryAttemptsByTarget[key]
		if b == nil {
			b = &incidents.RecoveryAttempts{}
			d.snapshot.RecoveryAttemptsByTarget[key] = b
		}
		//nolint:nilaway // RecoveryPolicies inserts only allocated policies; startRecovery enumerates that map under the daemon lock and passes the allocated policy to its worker.
		if since := d.healthySince(key); !since.IsZero() && now.Sub(since) >= p.Reset {
			// The result belongs to an attempt that no longer counts.
			b.Attempts, b.LastResult = 0, ""
		}
		due := d.recoveryDue(key, now, "")
		// A unit that MMWatchdog found stopped stays stopped until someone starts
		// it, which can take days: one look at each failed observation of
		// the check that calls for the restart is enough.
		//nolint:nilaway // RecoveryPolicies inserts only allocated policies; startRecovery enumerates that map under the daemon lock and passes the allocated policy to its worker. recoveryDue returns a check key only after checking its state and runtime are nonnil; the caller retains the daemon lock.
		if due == "" || (p.Run == nil && d.snapshot.Checks[due].Last.At.Equal(d.stoppedUnitSeenAt[key])) {
			continue
		}
		d.recovering[key] = true
		d.workers.Add(1)
		if p.Run != nil {
			go d.runCommand(ctx, key, p)
		} else {
			go d.restart(ctx, key, p)
		}
	}
}

// restart inspects the unit, reserves an attempt durably, and only then asks
// systemd to restart it. Its ticket is the unit's policy at the time it started.
//
// MMWatchdog does not start a stopped unit. systemd reports a unit that was stopped
// on request, or whose program ended normally, as inactive; MMWatchdog then
// withholds the restart, uses no attempt, and records that once for the
// failing time. It restarts a unit that is active, because a triggering
// check fails all the same, or failed.
func (d *Daemon) restart(ctx context.Context, unit string, ticket *incidents.Policy) {
	defer d.workers.Done()
	defer func() { d.mu.Lock(); delete(d.recovering, unit); d.mu.Unlock() }()
	state, e := primitives.SystemdUnitState(ctx, unit)
	// Wait while systemd is changing the unit.
	if e != nil || state == "activating" || state == "deactivating" || state == "reloading" {
		return
	}
	d.mu.Lock()
	now := d.now()
	due := ""
	if ctx.Err() == nil && d.policies[unit] == ticket {
		due = d.recoveryDue(unit, now, state)
	}
	if due == "" {
		d.mu.Unlock()
		return
	}
	group, check, _ := strings.Cut(due, "/")
	candidate := d.snapshot.Clone()
	b := candidate.RecoveryAttemptsByTarget[unit]
	if b == nil {
		b = &incidents.RecoveryAttempts{}
		candidate.RecoveryAttemptsByTarget[unit] = b
	}
	if state == "inactive" {
		d.stoppedUnitSeenAt[unit] = d.snapshot.Checks[due].Last.At //nolint:nilaway // recoveryDue returns a check key only after checking its state and runtime are nonnil; the caller retains the daemon lock.
		//nolint:nilaway // recoveryDue returns a check key only after checking its state and runtime are nonnil; the caller retains the daemon lock.
		if failingSince := d.snapshot.Checks[due].FailingSince; !b.WithheldForFailingTimeSince.Equal(failingSince) {
			b.WithheldForFailingTimeSince = failingSince
			withheld := checksandalerts.Event{ID: incidents.ID(), CheckGroup: group, Check: check, Type: "systemd_unit_restart_withheld", At: now, Message: "no restart: systemd unit " + unit + " is stopped, and MMWatchdog never starts a stopped unit"}
			_ = d.control(candidate, []checksandalerts.Event{withheld}, d.route())
		}
		d.mu.Unlock()
		return
	}
	b.Attempts++
	b.Last = now
	b.LastFinished = time.Time{}
	b.LastResult = "restart requested"
	reservation := checksandalerts.Event{ID: incidents.ID(), CheckGroup: group, Check: check, Type: "systemd_unit_restart_requested", At: now, Message: fmt.Sprintf("restart systemd unit %s, attempt %d", unit, b.Attempts)}
	e = d.control(candidate, []checksandalerts.Event{reservation}, d.route())
	d.mu.Unlock()
	if e != nil {
		return
	}
	e = primitives.RestartSystemdUnit(ctx, unit)
	d.mu.Lock()
	defer d.mu.Unlock()
	now = d.now()
	message := "systemd unit restart finished. The next check runs will show whether the check has recovered"
	if e != nil {
		message = e.Error()
	}
	if b := d.snapshot.RecoveryAttemptsByTarget[unit]; b != nil {
		b.LastFinished = now
		b.LastResult = message
	}
	// Evidence from before or during the command no longer describes the
	// unit: a further restart needs a failure observed after it. That holds
	// for every check group that restarts or checks the unit, including one a
	// reload gave a restart policy while the command ran.
	for _, group := range checksandalerts.Names(d.config.CheckGroups) {
		if slices.Contains(d.config.CheckGroups[group].SystemdUnits(), unit) {
			d.requireFresh(group, now, true)
		}
	}
	_ = d.record([]checksandalerts.Event{{ID: incidents.ID(), CheckGroup: group, Check: check, Type: "systemd_unit_restart_result", At: now, Message: message}})
}

// RunCommand reserves an attempt of a check group's recovery command durably, then
// runs it. The command gets the failure that triggered it but no observation
// output. Its ticket is the check group's policy at the time it started.
func (d *Daemon) runCommand(ctx context.Context, group string, ticket *incidents.Policy) {
	defer d.workers.Done()
	defer func() { d.mu.Lock(); delete(d.recovering, group); d.mu.Unlock() }()
	d.mu.Lock()
	now := d.now()
	key := ""
	if ctx.Err() == nil && d.policies[group] == ticket {
		key = d.recoveryDue(group, now, "")
	}
	if key == "" {
		d.mu.Unlock()
		return
	}
	check, incident := d.snapshot.Checks[key].Check, d.snapshot.Checks[key].FailureIncident.Incident //nolint:nilaway // recoveryDue returns a check key only after checking its state and runtime are nonnil; the caller retains the daemon lock.
	candidate := d.snapshot.Clone()
	b := candidate.RecoveryAttemptsByTarget[group]
	if b == nil {
		b = &incidents.RecoveryAttempts{}
		candidate.RecoveryAttemptsByTarget[group] = b
	}
	b.Attempts++
	b.Last = now
	b.LastFinished = time.Time{}
	b.LastResult = "recovery command started"
	reservation := checksandalerts.Event{ID: incidents.ID(), Incident: incident, CheckGroup: group, Check: check, Type: "recovery_command_started", At: now, Message: fmt.Sprintf("run recovery command, attempt %d", b.Attempts)}
	e := d.control(candidate, []checksandalerts.Event{reservation}, d.route())
	d.mu.Unlock()
	if e != nil {
		return
	}
	r := ticket.Run //nolint:nilaway // RecoveryPolicies inserts only allocated policies; startRecovery enumerates that map under the daemon lock and passes the allocated policy to its worker.
	opts := r.ExecOptions
	opts.Env = map[string]string{}
	maps.Copy(opts.Env, r.Env)
	opts.Env["MMWATCHDOG_CHECK_GROUP"], opts.Env["MMWATCHDOG_CHECK"], opts.Env["MMWATCHDOG_INCIDENT_ID"] = group, check, incident
	c, cancel := context.WithTimeout(ctx, r.TimeLimitPerAttempt.Value())
	start := time.Now()
	ran, err := primitives.RunCommand(c, primitives.Command{Arguments: primitives.Bash(r.Script), RunAs: &opts})
	out, diag, code := ran.StandardOutput, ran.StandardError, ran.ExitCode
	timedOut := errors.Is(c.Err(), context.DeadlineExceeded)
	cancel()
	// Long output does not make the command fail; the evidence marks what it
	// leaves out.
	if errors.Is(err, primitives.ErrStandardOutputLimit) {
		err = nil
	}
	o := checksandalerts.Result("ok", fmt.Sprintf("recovery command finished with exit code %d", code))
	switch {
	case timedOut:
		o = checksandalerts.Result("failed", "recovery command timed out after "+string(r.TimeLimitPerAttempt))
	case err != nil:
		o = checksandalerts.Result("failed", "recovery command failed: "+err.Error())
	case code != 0:
		o.Status = "failed"
	}
	if err == nil && !timedOut {
		o.ExitCode = &code
	}
	o.Stdout, o.Stderr, o.At, o.Duration = out, diag, d.now(), time.Since(start)
	d.mu.Lock()
	defer d.mu.Unlock()
	now = o.At
	if b := d.snapshot.RecoveryAttemptsByTarget[group]; b != nil {
		b.LastFinished = now
		b.LastResult = o.Message
		if o.ExitCode != nil {
			b.LastResult = fmt.Sprintf("exit %d", *o.ExitCode)
		}
	}
	// Evidence from before or during the command no longer describes the
	// group, or its units: another command or a restart needs a failure
	// observed after it.
	units := d.config.CheckGroups[group].SystemdUnits()
	for _, name := range checksandalerts.Names(d.config.CheckGroups) {
		shares := func(unit string) bool { return slices.Contains(units, unit) }
		if name == group || slices.ContainsFunc(d.config.CheckGroups[name].SystemdUnits(), shares) {
			d.requireFresh(name, now, true)
		}
	}
	evidence := checksandalerts.CaptureEvidence(incident, o, d.config.Source("check_groups."+group+".recovery_command"), checksandalerts.Timing{})
	_ = d.record([]checksandalerts.Event{{ID: incidents.ID(), Incident: incident, CheckGroup: group, Check: check, Type: "recovery_command_result", At: now, Message: o.Message, Evidence: evidence}})
}
