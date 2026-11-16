package primitives

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// What the systemd unit check modules and the restart of a unit share: the
// form of a unit's name, and the questions that MMWatchdog asks systemd.

var UnitPattern = regexp.MustCompile(`^[A-Za-z0-9_@.:\\-]+\.service$`)

// AnyUnitPattern accepts the name of a systemd unit of any kind, such as
// backup.service, backup.timer, or var-lib-data.mount: the characters systemd
// allows, the kind after the last dot, and at most 255 characters.
var AnyUnitPattern = regexp.MustCompile(`^[A-Za-z0-9_@.:\\-]{1,245}\.[a-z]{1,9}$`)

// ErrSystemdUnitNotFound is the error of every question about a unit that
// systemd does not know (its LoadState is not-found). Every check of a
// systemd unit then says the same: systemd unit myapp.service not found.
var ErrSystemdUnitNotFound = errors.New("not found")

// UnitRuns says whether a unit runs in the ActiveState that systemd reports
// for it: active or reloading, the two states in which "systemctl
// is-active" succeeds. A reload does not stop the unit's program. In every
// other state, such as activating, deactivating, inactive, or failed, the
// unit does not run.
func UnitRuns(activeState string) bool {
	return activeState == "active" || activeState == "reloading"
}

// ServiceProperties reads a unit's systemd properties. A unit systemd does not
// know is ErrSystemdUnitNotFound, since its properties establish nothing.
func ServiceProperties(ctx context.Context, unit string, names ...string) (map[string]string, error) {
	shown, err := Systemctl(ctx, "show", "--property=LoadState,"+strings.Join(names, ","), "--", unit)
	if err != nil {
		return nil, fmt.Errorf("systemd inspection: MMWatchdog could not ask systemd about systemd unit %s: %w", unit, err)
	}
	fields := map[string]string{}
	for _, line := range strings.Split(shown, "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			fields[key] = value
		}
	}
	if fields["LoadState"] == "not-found" {
		return nil, fmt.Errorf("systemd unit %s %w", unit, ErrSystemdUnitNotFound)
	}
	return fields, nil
}

func SystemdUnitState(ctx context.Context, unit string) (string, error) {
	fields, err := ServiceProperties(ctx, unit, "ActiveState")
	if err != nil {
		return "", err
	}
	if fields["ActiveState"] == "" {
		return "", fmt.Errorf("systemd returned no ActiveState for %s", unit)
	}
	return fields["ActiveState"], nil
}

func RestartSystemdUnit(ctx context.Context, unit string) error {
	if _, e := Systemctl(ctx, "restart", "--", unit); e != nil {
		return fmt.Errorf("systemd restart: MMWatchdog could not restart systemd unit %s: %w", unit, e)
	}
	return nil
}

// UnitIsNoLongerFailed is the message of the observation in which systemd
// no longer lists a unit as failed.
func UnitIsNoLongerFailed(unit string) string {
	return "systemd unit " + unit + " is no longer failed"
}

// FailedUnitMessage says that systemd reports the unit as failed, with
// systemd's Result when it gives one.
func FailedUnitMessage(unit, systemdResult string) string {
	if systemdResult == "" {
		return "systemd unit " + unit + " failed"
	}
	return "systemd unit " + unit + " failed (systemd result: " + systemdResult + ")"
}
