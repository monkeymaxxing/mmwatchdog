package checksandalerts

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// One incident per check across the fleet. Every alert about one incident
// of a check, or of one finding of a check, carries the same key on every
// node of one fleet. Store.Save adds the fleet identity or standalone
// node name, so that a destination whose
// service keeps incidents of its own opens one incident there for the
// check, whichever nodes fail. The rules are in docs/changing-mmwatchdog/design/fleet.md under
// "One incident per check across the fleet".

// SilenceIncidentKey is the readable fleet incident key of every joined
// node that the hub finds silent: one incident for all of them.
const SilenceIncidentKey = NodeReportsCheckGroup + " stopped reporting"

// ReadableFleetIncidentKey is the readable key of the incident that an alert
// of the incident kind (failed or cannot_check) is about: "GROUP/CHECK
// failed" or "GROUP/CHECK cannot check", with the finding after the check
// (AboutCheck), and SilenceIncidentKey for the hub's alert about a silent
// node. It names the local incident-set rows. Destinations receive the
// fixed-length scoped key instead. hub is the fleet hub's name, or empty
// elsewhere: only the hub knows that a check of
// node_reports names a node.
func ReadableFleetIncidentKey(e Event, kind, hub string) string {
	if e.CheckGroup == NodeReportsCheckGroup && hub != "" && e.Node == hub {
		return SilenceIncidentKey
	}
	if kind == "cannot_check" {
		return e.AboutCheck() + " cannot check"
	}
	return e.AboutCheck() + " failed"
}

// ReadableKeyIsAboutCannotCheck reports a readable fleet incident key of a
// cannot-check incident.
func ReadableKeyIsAboutCannotCheck(readable string) bool {
	return strings.HasSuffix(readable, " cannot check")
}

// FleetIncidentKey is the check-and-kind component before ScopeFleetIncidentKey
// adds its fleet or standalone-node identity: the first 16 bytes of the
// readable key's SHA-256, as 32
// lowercase hexadecimal digits. Its length is fixed, so it fits the field
// of every service for its own incident's key, however long the names of
// the check are.
func FleetIncidentKey(readable string) string {
	sum := sha256.Sum256([]byte(readable))
	return hex.EncodeToString(sum[:16])
}

// FleetIncidentKeyPattern is what a fleet_incident_key looks like, for an
// alert that the hub takes from a node.
var FleetIncidentKeyPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// BelongsToAnIncident reports an alert that belongs to an incident and so
// carries its fleet incident key: a failure, a cannot-check alert, a raise,
// a reminder, and an alert that closes the incident. A notice, a reboot
// notice, and a maintenance alert do not.
func BelongsToAnIncident(eventType string) bool {
	return Opens(eventType) || Closes(eventType) || eventType == "reminder"
}

// ScopeFleetIncidentKey separates independent fleets and standalone nodes.
// A fleet uses its hub certificate fingerprint even after a move of the hub.
// The input key already identifies the check and incident kind. The zero
// byte separates that key from the identity.
func ScopeFleetIncidentKey(incidentKey, node, fleetIdentity string) string {
	identity := "node:" + node
	if fleetIdentity != "" {
		identity = "fleet:" + fleetIdentity
	}
	return FleetIncidentKey(identity + "\x00" + incidentKey)
}
