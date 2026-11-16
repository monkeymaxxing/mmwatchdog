package checksandalerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"mmwatchdog/internal/primitives"
)

// ModuleRequestLimit is the most bytes of the request that MMWatchdog writes to a
// destination module, and the most that "mmwatchdog module-json" reads. The largest
// alert that MMWatchdog writes and the files and the inputs of a destination always
// fit in it together: see ModuleFilesLimit.
const ModuleRequestLimit = 1024 * 1024

type Measurement struct {
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}

func (m *Measurement) UnmarshalJSON(b []byte) error {
	var fields map[string]json.RawMessage
	if e := json.Unmarshal(b, &fields); e != nil {
		return e
	}
	value, ok := fields["value"]
	if !ok || string(value) == "null" || len(fields) != 2 {
		return fmt.Errorf("measurement requires value and unit")
	}
	unit, ok := fields["unit"]
	if !ok || string(unit) == "null" {
		return fmt.Errorf("measurement requires unit")
	}
	if e := json.Unmarshal(value, &m.Value); e != nil {
		return e
	}
	return json.Unmarshal(unit, &m.Unit)
}

type Observation struct {
	SampleWindow time.Duration          `json:"sample_window_ns,omitempty"`
	Protocol     int                    `json:"protocol"`
	Status       string                 `json:"status"`
	Message      string                 `json:"message"`
	Measurements map[string]Measurement `json:"measurements,omitempty"`
	// Details are lines that say more about what the run found than the
	// message has room for, such as the five commands that use the most
	// memory, or the new lines of a log that matched. The body of the
	// check's alert shows them. A module writes at most DetailLinesLimit
	// lines, each on one line of plain text.
	Details  []string `json:"details,omitempty"`
	ExitCode *int     `json:"exit_code,omitempty"`
	Stdout   string   `json:"stdout,omitempty"`
	Stderr   string   `json:"stderr,omitempty"`
	// Findings are, in an observation of a check whose module reports
	// findings, the things that the run found to be wrong, in order of their
	// IDs: for an every_systemd_unit check the units that systemd lists
	// as failed and that the check does not ignore. Each finding is its own
	// incident: see CheckState.observeFindings.
	Findings []Finding     `json:"findings,omitempty"`
	At       time.Time     `json:"at"`
	Duration time.Duration `json:"duration_ns"`
	// FindingIsGoneMessage writes the message of the observation in which
	// a finding is no longer reported. RunCheck sets it from the check's
	// module; without it the message is FindingIsGone's.
	FindingIsGoneMessage func(finding string) string `json:"-" yaml:"-"`
}

// Finding is one thing that a run of a check found to be wrong. ID is its
// stable identity: the same thing has the same ID at every run, such as the
// name of a failed systemd unit. Message says what is wrong.
type Finding struct {
	ID      string `json:"id"`
	Message string `json:"message"`
	// RecoveryMessage names the finding when it becomes healthy, including
	// when ID is a checksum. Its incident state keeps it across restarts.
	RecoveryMessage string `json:"recovery_message,omitempty"`
}

// FindingIDLimit is the most bytes that the ID of a finding has: the length
// of the longest name that a systemd unit can have, because the name of a
// failed unit is the ID of its finding. A module with longer identities
// uses a stable checksum as the ID and names the object in its message.
const FindingIDLimit = 255

// ValidFindingID reports an ID that a finding can have: text on one line of
// at most FindingIDLimit bytes.
func ValidFindingID(id string) bool { return id != "" && PlainText(id, FindingIDLimit) }

// FindingIsGone is the message of the observation in which a finding is no
// longer reported, for a module that states none of its own.
func FindingIsGone(finding string) string {
	return finding + " is no longer among the findings of this check"
}

func Result(status, message string) Observation {
	return Observation{Protocol: 1, Status: status, Message: message}
}

// ObserveCheck runs a check once, as a run that keeps nothing from an
// earlier one and leaves nothing behind.
func ObserveCheck(ctx context.Context, c Check) Observation {
	return RunCheck(ctx, NewCheckRun(c, Timing{}))
}

// RunCheck runs the check of r once, through its module, and stamps the
// observation with the time and the duration of the run.
func RunCheck(ctx context.Context, r *CheckRun) Observation {
	start := time.Now()
	o := Result("unknown", "no check configured")
	if r.Check.Inputs != nil {
		if limit := r.Timing.TimeLimitPerRun.Value(); limit > 0 {
			ctx = primitives.WithTimeLimitPerRun(ctx, limit)
		}
		o = r.Check.Inputs.Run(ctx, r)
		o.FindingIsGoneMessage = r.Check.Module().findingIsGoneMessage
	}
	o.At = r.now()
	o.Duration = time.Since(start)
	return o
}

// now is the time of the run's observation.
func (r *CheckRun) now() time.Time {
	if r.Now == nil {
		return time.Now().UTC()
	}
	return r.Now()
}

// decodeJSONValue reads one JSON value of any kind. A number stays as it
// is written.
func decodeJSONValue(b []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	var value any
	if e := decoder.Decode(&value); e != nil {
		return nil, e
	}
	return value, nil
}

// Reject duplicate object keys as well as unknown fields and extra JSON values.
func DecodeJSON(b []byte, out any) error { return DecodeJSONUpTo(b, out, primitives.ResultLimit) }
func DecodeJSONUpTo(b []byte, out any, limit int) error {
	return decodeJSONUpTo(b, out, limit, true)
}

// DecodeJSONUpToAllowingUnknownFields is DecodeJSONUpTo for a document from a
// peer that may be a newer release, whose added fields this release ignores.
func DecodeJSONUpToAllowingUnknownFields(b []byte, out any, limit int) error {
	return decodeJSONUpTo(b, out, limit, false)
}

func decodeJSONUpTo(b []byte, out any, limit int, refuseUnknownFields bool) error {
	if len(b) > limit {
		return fmt.Errorf("JSON exceeds %d bytes", limit)
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := uniqueJSON(d); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("expected one JSON object")
	}
	if len(bytes.TrimSpace(b)) == 0 || bytes.TrimSpace(b)[0] != '{' {
		return fmt.Errorf("expected a JSON object")
	}
	d = json.NewDecoder(bytes.NewReader(b))
	if refuseUnknownFields {
		d.DisallowUnknownFields()
	}
	d.UseNumber()
	return d.Decode(out)
}
func uniqueJSON(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, e := d.Token()
			if e != nil {
				return e
			}
			s := k.(string)
			if seen[s] {
				return fmt.Errorf("duplicate JSON key %q", s)
			}
			seen[s] = true
			if e := uniqueJSON(d); e != nil {
				return e
			}
		}
	case '[':
		for d.More() {
			if e := uniqueJSON(d); e != nil {
				return e
			}
		}
	default:
		return fmt.Errorf("unexpected delimiter")
	}
	_, err = d.Token()
	return err
}

// serverThatDidNotAnswer is the observation of a check for an exchange over
// TCP that ended with an error, or nil for one that did not: the error of a
// connection that did not open, and for a server that did not finish its
// answer within the time limit of the run, that fact with what the server
// sent until then.
func serverThatDidNotAnswer(ctx context.Context, address, received string, err error) *Observation {
	if err == nil {
		return nil
	}
	o := Result("failed", primitives.DetailLine(connectionError(ctx, address, err)))
	if !errors.As(err, &primitives.ConnectionDidNotOpen{}) {
		o.Message = "the server at " + address + " ended the connection before its answer was complete"
		if ctx.Err() != nil {
			o.Message = "the server at " + address + " did not answer within the time limit of the run"
		}
		if received != "" {
			o.Details = []string{"the server sent: " + primitives.DetailLine(primitives.Clip(received, primitives.DetailLineLimit-20))}
		}
	}
	return &o
}

// connectionError is the words for the error of a connection that did not
// open: in plain words when the time limit of the run ended it, in place of
// Go's "i/o timeout", and otherwise the node's system's words.
func connectionError(ctx context.Context, address string, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "the connection to " + address + " did not open within the time limit of the run"
	}
	return err.Error()
}

// timeLimitWords names the time limit of the run in a message: "5s
// (time_limit_per_run)", or "the time limit of the run" for a run that states none.
func timeLimitWords(r *CheckRun) string {
	if stated := r.Timing.TimeLimitPerRun.Value(); stated > 0 {
		return stated.String() + " (time_limit_per_run)"
	}
	return "the time limit of the run"
}

// unitNameError checks the unit that the input at path names.
func unitNameError(path, unit string) *FieldError {
	if unit == "" {
		return &FieldError{path, "is required: the systemd unit this check is about, such as myapp.service"}
	}
	if !primitives.UnitPattern.MatchString(unit) {
		return &FieldError{path, "must be a systemd .service unit name, such as myapp.service"}
	}
	return nil
}
