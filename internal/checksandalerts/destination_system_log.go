package checksandalerts

import (
	"context"
	"fmt"

	"mmwatchdog/internal/primitives"
)

// SystemLog is the inputs of the system_log destination module, which
// writes one line to the system log of the machine, the systemd journal,
// for each alert: the title with the alert's severity in front. The module
// has no inputs. The rules are in docs/changing-mmwatchdog/design/alerts.md under "System log".
type SystemLog struct{}

func (s *SystemLog) Validate(InputValidation) *FieldError { return nil }

// ValidateDestination refuses the setting that a destination of one line
// has no use for.
func (s *SystemLog) ValidateDestination(d Destination) *FieldError {
	if d.SendOnlyTheAlertTitle {
		return &FieldError{"send_only_the_alert_title", "a system_log destination takes no send_only_the_alert_title: it writes only the title with the alert's severity in front"}
	}
	return nil
}

func (s *SystemLog) sameTarget(destinationInputs) bool { return true }

func (s *SystemLog) CredentialFiles() map[string]string { return nil }

func (s *SystemLog) ReadCredentialFile(_, _ string) (string, error) { return "", nil }

func (s *SystemLog) ValidCredential(_, _ string) bool { return false }

func (s *SystemLog) Sender(delivery DeliveryOf) NotificationSender { return systemLogSender{delivery} }

// systemLogIdentifier is the name under which MMWatchdog writes alerts to the
// system log: "journalctl --identifier mmwatchdog-alert" shows them and nothing
// else. The daemon's own messages have the name of its unit.
const systemLogIdentifier = "mmwatchdog-alert"

// systemLogSender writes an alert to the system log.
type systemLogSender struct{ delivery DeliveryOf }

func (s systemLogSender) Send(ctx context.Context, event Event) error {
	ctx, cancel := context.WithTimeout(ctx, s.delivery.Destination.TimeLimitPerDeliveryAttempt.Value())
	defer cancel()
	line := event.TitleWithAlertSeverity
	if line == "" {
		line = event.Title
	}
	if e := primitives.WriteToSystemLog(ctx, systemLogPriority(event), systemLogIdentifier, line); e != nil {
		return fmt.Errorf("MMWatchdog could not write the alert to the system log: %w", e)
	}
	return nil
}

// systemLogPriority is the priority of an alert's line, as syslog counts
// it: 2, critical, for sos, 4, warning, for warn, and 6, informational, for
// info. An alert that closes an incident is informational whatever the
// severity of the incident was: a log collector that acts on the priority
// must not take the end of an outage for a new one.
func systemLogPriority(event Event) int {
	if Closes(event.Type) {
		return 6
	}
	switch event.AlertSeverity {
	case "sos":
		return 2
	case "warn":
		return 4
	}
	return 6
}

// systemLogDescription is what "mmwatchdog describe-module system_log" shows, and what
// validation knows about the module's inputs.
var systemLogDescription = ModuleDescription{
	Summary: "Writes one line to the system log of the machine, the systemd journal, for each alert: the title with the alert's severity in front.",
	UsedAs:  UsedAsAlertDestination,
	Examples: []ModuleExample{{
		Description:   "Write every alert to the system log.",
		Configuration: "system_log: {}\nalert_severities: [info, warn, sos]\nremind_every: never\n",
	}},
}

// VerifyDestination opens the journal socket without writing a log entry.
func (s *SystemLog) VerifyDestination(ctx context.Context, delivery DeliveryOf) (bool, string) {
	connection, err := primitives.ConnectSystemLog(ctx)
	if err != nil {
		return false, "MMWatchdog could not connect to the system log: " + err.Error()
	}
	connection.Close()
	return true, "verified connection to the system log socket"
}
