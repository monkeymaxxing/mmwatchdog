package store

import (
	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
)

// PreparedEvent is event as every commit stores and queues it: with the
// node, the scoped fleet incident key of an event that belongs to an
// incident, and, for an alert, the title and body, written once for every
// destination and for the hub.
func (s *Store) PreparedEvent(event checksandalerts.Event, route AlertRoute) checksandalerts.Event {
	hub := ""
	if route.Hub {
		hub = s.Node
	}
	event.Node = s.Node
	if checksandalerts.BelongsToAnIncident(event.Type) {
		key := event.FleetIncidentKey
		if key == "" || hub != "" && event.CheckGroup == checksandalerts.NodeReportsCheckGroup {
			key = checksandalerts.FleetIncidentKey(checksandalerts.ReadableFleetIncidentKey(event, checksandalerts.IncidentKind(event.Type), hub))
		}
		event.FleetIncidentKey = checksandalerts.ScopeFleetIncidentKey(key, s.Node, s.FleetIdentity)
	}
	if event.Notify {
		event.WriteAlertTexts([]checksandalerts.Event{event}, hub)
	}
	return event
}

// DeliveriesFromMemory are the deliveries of message, an alert that no
// commit has put in the outbox because state.db cannot be written, to the
// destinations that QueueDirect would queue it for. The daemon sends them
// straight from memory, so a full disk does not hold back the alert that
// reports it. Nothing is written: the routing of a recovery reads where its
// incident's alerts went, as Receives does. A destination that states
// send_only_the_alert_title gets the title only. An mmhotline destination
// gets a failure, a raise, or a notice, but no resolution and no reminder:
// its resolutions come from its fleet incident set, which only a commit
// keeps (QueueMMHotline), and the commit after the failure queues them.
func (s *Store) DeliveriesFromMemory(message checksandalerts.Event, destinations map[string]checksandalerts.Destination, from QueuedBy) []Delivery {
	var deliveries []Delivery
	for _, name := range checksandalerts.Names(destinations) {
		dest := destinations[name]
		if message.Destination != "" && name != message.Destination || !s.receivesWithoutWriting(name, dest, message, destinations) {
			continue
		}
		alert := message
		switch {
		case dest.MMHotline() == nil:
			if dest.SendOnlyTheAlertTitle {
				alert = alert.OnlyTheTitle()
			}
		case checksandalerts.Opens(message.Type):
			alert = MMHotlineFailure(message, []checksandalerts.Event{message}, from, message.AlertSeverity)
		case message.Type == "reminder" || checksandalerts.Closes(message.Type):
			continue
		default:
			alert = MMHotlineNotice(message, from)
		}
		deliveries = append(deliveries, Delivery{Event: alert, Destination: name, Target: dest})
	}
	return deliveries
}

// receivesWithoutWriting is Receives in a transaction that only reads. When
// state.db cannot even be read, a recovery goes where config.GoesTo sends it.
func (s *Store) receivesWithoutWriting(name string, dest checksandalerts.Destination, message checksandalerts.Event, destinations map[string]checksandalerts.Destination) bool {
	if tx, e := s.DB.Begin(); e == nil {
		defer tx.Rollback() //nolint:errcheck // The transaction only reads.
		if ok, e := Receives(tx, name, dest, message, []checksandalerts.Event{message}, destinations); e == nil {
			return ok
		}
	}
	return config.GoesTo(name, dest, message, destinations)
}
