package daemon

import (
	"time"

	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
)

// failingCause is the first failing check that st, the state of the check
// key, names and whose alerts reach a destination under the current route.
// When the check key alerts and its cause reaches nobody, nobody learns why the
// check is held quiet, so the check alerts for itself. The hub can change the
// destinations of a joined node at any time, after the load-time check
// (ValidateCauseDestinations). Without destinations known yet, every failing
// cause counts. The caller holds d.mu.
func (d *Daemon) failingCause(key string, st *incidents.CheckState) string {
	if st == nil {
		return ""
	}
	destinations := d.route().Destinations
	if rc := d.checks[key]; rc == nil || !config.CheckReachesDestination(rc.def.CheckWithSeverity(), destinations) {
		destinations = nil
	}
	for _, named := range st.QuietWhileTheseChecksFail {
		cause := d.snapshot.Checks[named]
		if cause == nil || !cause.CountsAsFailing() {
			continue
		}
		if rc := d.checks[named]; destinations == nil || rc == nil || config.CheckReachesDestination(rc.def.CheckWithSeverity(), destinations) {
			return named
		}
	}
	return ""
}

// quietWhile returns the check whose failing keeps the check key quiet now,
// or "" when the check is not kept quiet. It is the one question that every
// step outside the check's own observations asks: the raise and reminder
// steps of the scheduler tick and recoveryDue. A check is kept quiet from
// the moment a check that it names starts to fail, and until its own first
// observation after the quiet time: that observation decides, not one from
// before. Daemon.status shows the two parts of the answer apart. The caller
// holds d.mu.
func (d *Daemon) quietWhile(key string) string {
	st := d.snapshot.Checks[key]
	if st == nil {
		return ""
	}
	if cause := d.failingCause(key, st); cause != "" {
		return cause
	}
	return st.QuietNowWhileCheckFails
}

// quietAtObservation decides, for an observation of the check key that is
// about to be applied, whether the check is kept quiet, and keeps the answer
// in the check's state. It is the only place that starts and ends a check's
// quiet time. The quiet time lasts while a check that it names counts as
// failing. It also lasts while one of the named checks needs a fresh
// observation: after a daemon start, a maintenance boundary, or a reload
// that changed the named check, MMWatchdog does not know yet whether the cause is
// gone, and the quiet check's run must not decide before MMWatchdog does. The caller
// holds d.mu.
func (d *Daemon) quietAtObservation(key string, at time.Time) bool {
	st := d.snapshot.Checks[key]
	cause := d.failingCause(key, st)
	//nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
	if previous := st.QuietNowWhileCheckFails; cause == "" && previous != "" {
		for _, named := range st.QuietWhileTheseChecksFail { //nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
			if rc := d.checks[named]; rc != nil && rc.needsFresh() {
				cause = previous
				break
			}
		}
	}
	st.SetQuietNow(cause, at)
	return cause != ""
}
