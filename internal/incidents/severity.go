package incidents

import (
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
)

// RaiseMessage is the message of a raise: why, the new severity in the word
// that people read, and the setting that raised it when one did, such as
// "myapp/http has been failing for 30m, so this alert is now SOS
// (raise_to_sos_after_failing_for: 30m)".
func RaiseMessage(because, severity, setting string) string {
	message := because + ", so this alert is now " + checksandalerts.AlertSeverityLabel(severity)
	if setting != "" {
		message += " (" + setting + ")"
	}
	return message
}

// Raise lifts a track's open incident to severity; it never lowers one.
// An announced incident sends kinds.raised with the new severity and
// message (RaiseMessage), so a destination that takes only that severity
// gets it as its first message; an unannounced one carries the severity
// when it is announced. While maintenance suppresses the check, an
// announced incident waits, so the raise happens when maintenance ends.
func (s *CheckState) Raise(t *Track, kinds checksandalerts.TrackEvents, severity, message string, now time.Time, suppressed bool) []checksandalerts.Event {
	announced := t.Incident != "" && t.Announced == t.Incident
	if t.Incident == "" || config.SeverityRank(severity) <= config.SeverityRank(t.AlertSeverity) || (suppressed && announced) {
		return nil
	}
	s.Touch()
	t.AlertSeverity = severity
	if !announced {
		return nil
	}
	t.AnnouncedAlertSeverities = append(t.AnnouncedAlertSeverities, severity)
	// Reminders count from the raise. A reminder takes the new severity, so
	// each destination that gets one from now on gets the raise, and one
	// that takes only the new severity hears of the incident first now.
	t.RemindFrom = now
	return []checksandalerts.Event{s.Event(t, kinds.Raised, t.Incident, message, now, true)}
}

// RaiseAfterTime is the escalation by time. An open failure incident is
// raised to sos when the check's failing time has lasted
// raise_to_sos_after_failing_for, and an open cannot-check incident when MMWatchdog
// has been unable to observe the check for that long. Both count from the
// start, like every waiting time, not from the moment the incident opened.
// The checks of the fleet are raised by Daemon.raiseSilentNodeAfterTime.
func (s *CheckState) RaiseAfterTime(now time.Time, suppressed bool) []checksandalerts.Event {
	after := s.AlertSeveritySettings.RaiseToSosAfterFailingFor
	if after.Value() <= 0 {
		return nil
	}
	setting := "raise_to_sos_after_failing_for: " + string(after)
	about := checksandalerts.AboutCheck(s.CheckGroup, s.Check, s.Finding)
	events := []checksandalerts.Event{}
	for _, track := range []struct {
		t       *Track
		kinds   checksandalerts.TrackEvents
		since   time.Time
		message string
	}{
		{&s.FailureIncident, checksandalerts.FailureIncidentEvents, s.FailingSince, RaiseMessage(about+" has been failing for "+string(after), "sos", setting)}, //nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
		{&s.CannotCheckIncident, checksandalerts.CannotCheckIncidentEvents, s.UnknownSince, RaiseMessage("MMWatchdog has not been able to run "+about+" for "+string(after), "sos", setting)},
	} {
		// State without the start, such as a fleet check after the hub
		// started, counts from the opening of the incident.
		since := track.since
		if since.IsZero() || since.After(track.t.Opened) {
			since = track.t.Opened
		}
		if track.t.Incident != "" && now.Sub(since) >= after.Value() {
			events = append(events, s.Raise(track.t, track.kinds, "sos", track.message, now, suppressed)...)
		}
	}
	// Each failed unit of an every_systemd_unit check counts from the
	// start of its own failing time.
	for _, unit := range s.FindingStates() {
		events = append(events, unit.RaiseAfterTime(now, suppressed)...)
	}
	return events
}

// RaiseToPolicy raises open incidents to the check's alert_severity after
// it changed. The scheduler tick calls it, so a reload raises at the next
// tick, and a raise held by maintenance goes out when it ends.
func (s *CheckState) RaiseToPolicy(now time.Time, suppressed bool) []checksandalerts.Event {
	severity := s.AlertSeveritySettings.AlertSeverity
	message := RaiseMessage("the check's alert_severity was changed to "+severity+" in the configuration", severity, "")
	events := s.Raise(&s.FailureIncident, checksandalerts.FailureIncidentEvents, severity, message, now, suppressed)
	events = append(events, s.Raise(&s.CannotCheckIncident, checksandalerts.CannotCheckIncidentEvents, severity, message, now, suppressed)...) //nolint:nilaway // Runtime and snapshot checks are allocated from the same definitions; startup and locked updates keep both maps paired.
	for _, unit := range s.FindingStates() {
		events = append(events, unit.RaiseToPolicy(now, suppressed)...)
	}
	return events
}
