package incidents

import "time"

// FailingCause returns the first of the checks that st names as its causes
// that counts as failing, or "" when none does.
func (s Snapshot) FailingCause(st *CheckState) string {
	//nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
	for _, key := range st.QuietWhileTheseChecksFail {
		if cause := s.Checks[key]; cause != nil && cause.CountsAsFailing() {
			return key
		}
	}
	return ""
}

// CountsAsFailing reports that the check keeps the checks quiet that name
// it. A named check counts as failing during its whole failing time: from
// its first failed observation until it has been healthy for its
// recovered_after_healthy_for. At a daemon start or maintenance boundary,
// Interrupt ends a failing time whose incident has not opened. The
// check's latest observation is then still a failed one, and the check goes
// on counting as failing until its next observation: unobserved time is no
// evidence that the cause is gone.
func (s *CheckState) CountsAsFailing() bool {
	return !s.FailingSince.IsZero() || s.Last.Status == "failed"
}

// SetQuietNow records the check that keeps this check quiet now, or "" at
// the end of a quiet time. Nothing is sent later for the quiet time, so the
// reminders of an incident that was announced before it count from its end,
// as after maintenance.
func (s *CheckState) SetQuietNow(cause string, at time.Time) {
	if s.QuietNowWhileCheckFails == cause {
		return
	}
	s.Touch()
	s.QuietNowWhileCheckFails = cause //nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
	if cause == "" {
		s.ResumeReminders(at)
	}
}

// KeptQuietNote says, for a check that is kept quiet, what the check's state
// is and what MMWatchdog does not send or do because of the quiet time, with the
// reason: "myapp/http is failing" and "no alert and no recovery action while
// postgres/postgres_service_is_running fails". Both are "" for a check that
// is not kept quiet, and for a quiet check that is healthy and has no
// recovery alert to send: MMWatchdog leaves nothing out for it.
func (s CheckStatus) KeptQuietNote() (state, note string) {
	reason := ""
	switch {
	case s.QuietNowWhileCheckFails != "":
		reason = " while " + s.QuietNowWhileCheckFails + " fails"
	case s.QuietUntilNextRunAfterCheckFailed != "":
		// The named check no longer counts as failing, and the next run of
		// the quiet check decides.
		reason = " until its next run, because " + s.QuietUntilNextRunAfterCheckFailed + " was failing"
	default:
		return "", ""
	}
	about := s.CheckGroup + "/" + s.Check
	switch {
	case !s.FailingSince.IsZero() || s.failureIncidentOpen():
		return about + " is failing", "no alert and no recovery action" + reason
	case !s.UnknownSince.IsZero() || s.CannotCheckIncident.Incident != "":
		// An unknown observation never calls for a recovery action.
		return "MMWatchdog cannot check " + about, "no alert" + reason
	case s.FailureIncident.Announced != "":
		// The incident was announced before the quiet time and resolved in
		// it: its recovery alert goes out after the quiet time.
		return about + " is ok again", "no recovery alert" + reason
	case s.CannotCheckIncident.Announced != "":
		return "MMWatchdog can check " + about + " again", "no alert" + reason
	}
	return "", ""
}

// KeptQuietSentence is the line that mmwatchdog status shows for a check that MMWatchdog
// keeps quiet, such as "myapp/http is failing; no alert and no recovery
// action while postgres/postgres_service_is_running fails". It is "" for
// every other check.
func (s CheckStatus) KeptQuietSentence() string {
	state, note := s.KeptQuietNote()
	if note == "" {
		return ""
	}
	return state + "; " + note
}
