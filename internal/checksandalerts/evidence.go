package checksandalerts

import (
	"encoding/json"
	"maps"
	"slices"
	"time"

	"mmwatchdog/internal/primitives"
)

const EvidenceLimit = 16 * 1024

// Evidence freezes the confirming observation and rule, independent of later
// observations or edits to the configuration file.
type Evidence struct {
	Incident    string         `json:"incident_id"`
	Observation Observation    `json:"observation"`
	Source      SourceLocation `json:"source"`
	Timing      Timing         `json:"timing"`
	// FailingSince is when the failing time of a failure incident's check
	// began. CannotCheckSince is when MMWatchdog first could not run the check of a
	// cannot-check incident.
	FailingSince     time.Time `json:"failing_since,omitzero"`
	CannotCheckSince time.Time `json:"cannot_check_since,omitzero"`
	// LastRunsFailed of the LastRunsCounted runs before the incident opened
	// failed, for a check that failed on and off. Both are left out when
	// every run failed.
	LastRunsFailed  int  `json:"last_runs_failed,omitempty"`
	LastRunsCounted int  `json:"last_runs_counted,omitempty"`
	Truncated       bool `json:"truncated,omitempty"`
}

func CaptureEvidence(incident string, o Observation, source SourceLocation, timing Timing) *Evidence {
	o.Measurements, o.Details = maps.Clone(o.Measurements), slices.Clone(o.Details)
	if o.ExitCode != nil {
		code := *o.ExitCode
		o.ExitCode = &code
	}
	e := &Evidence{Incident: incident, Observation: o, Source: source, Timing: timing}
	// Bound the encoded payload, including JSON escaping, not just input bytes.
	for {
		b, err := json.Marshal(e)
		if err == nil && len(b) <= EvidenceLimit {
			return e
		}
		e.Truncated = true
		o := &e.Observation
		switch {
		case len(o.Stdout) > 0 || len(o.Stderr) > 0:
			if len(o.Stdout) >= len(o.Stderr) {
				o.Stdout = primitives.Clip(o.Stdout, len(o.Stdout)/2)
			} else {
				o.Stderr = primitives.Clip(o.Stderr, len(o.Stderr)/2)
			}
		case len(o.Details) > 0:
			o.Details = o.Details[:len(o.Details)-1]
		case len(o.Measurements) > 0:
			names := Names(o.Measurements)
			delete(o.Measurements, names[len(names)-1])
		case len(o.Message) > 0:
			o.Message = primitives.Clip(o.Message, len(o.Message)/2)
		default:
			// Filesystem paths are bounded; this only guards invalid in-memory callers.
			e.Source.File = primitives.Clip(e.Source.File, len(e.Source.File)/2)
		}
	}
}
