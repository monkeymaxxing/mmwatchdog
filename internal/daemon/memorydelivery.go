package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/store"
)

// While state.db cannot be written, such as on a full disk, the alerts of
// the events in d.pending reach no outbox. The notifications worker then
// sends each of them straight from memory, to the destinations that its
// route selects, and tries again at the pace of the outbox while an
// attempt fails. The commit that succeeds later puts the same alerts in the
// outbox, and they go out a second time: a duplicate alert is better than a
// dropped one, and remembering what went out would need the state.db that
// cannot be written. Only a joined node's direct send that its destination
// accepted is kept in memory, until that commit records it for the node's
// reports (acceptedFromMemory): the hub must learn of it to close what it
// opened, also when the hub is reachable again before state.db is.

// memoryDeliveryKey names one alert, by its event ID, for one destination,
// or for the fleet hub (config.HubDestination).
type memoryDeliveryKey struct{ eventID, destination string }

// memoryDelivery is how the delivery of one alert from memory goes. The
// times count on the monotonic clock.
type memoryDelivery struct {
	since    time.Time // when the notifications worker first saw it
	next     time.Time // when the next attempt is due
	attempts int
	// done is set once the destination accepted the alert, or refused it
	// for good: nothing is sent again from memory.
	done bool
}

// deliverFromMemory starts an attempt for each alert in d.pending, for
// each destination whose attempt is due, and forgets the alerts that have
// left d.pending: those a commit has put in the outbox, or that
// boundPending dropped. d.fromMemory so holds no more alerts than
// d.pending does.
func (d *Daemon) deliverFromMemory(ctx context.Context, now time.Time, wg *sync.WaitGroup) {
	d.mu.Lock()
	defer d.mu.Unlock()
	route := d.route()
	waiting := map[string]bool{}
	// Events also wait in d.pending, with state.db writable, for the
	// scheduler's next commit (a hub closing a silent node's alert on a
	// report, a join, or a forget). That commit queues them, so sending
	// them from memory too would send them twice. Only a failed commit
	// means they cannot reach the outbox.
	commitFailed := d.storage[stateCommit] != ""
	for _, event := range d.pending {
		if !commitFailed || !event.Notify {
			continue
		}
		waiting[event.ID] = true
		for _, delivery := range d.deliveriesFromMemory(event, route, now) {
			key := memoryDeliveryKey{event.ID, delivery.Destination}
			r := d.fromMemory[key]
			if r == nil {
				r = &memoryDelivery{since: now, next: now}
				d.fromMemory[key] = r
			}
			if r.done || now.Before(r.next) || d.deliveries[delivery.Destination] {
				continue
			}
			sender, ok := d.sender(delivery)
			if !ok {
				continue
			}
			d.deliveries[delivery.Destination] = true
			wg.Add(1)
			go func() {
				defer wg.Done()
				e := send(ctx, sender, delivery.Event)
				d.mu.Lock()
				defer d.mu.Unlock()
				delete(d.deliveries, delivery.Destination)
				if ctx.Err() != nil {
					return
				}
				r.attempts++
				var refused *checksandalerts.MMHotlineRefusal
				r.done = e == nil || errors.As(e, &refused)
				if e == nil {
					if kept := d.acceptedFromMemory.Add(delivery); kept != nil {
						d.storage.Report("notification bookkeeping "+delivery.Destination, kept)
					}
				}
				delay := time.Duration(0)
				if !r.done {
					delay = store.DelayAfterFailedAttempts(r.attempts)
					r.next = d.now().Add(delay)
				}
				log.Printf("state.db cannot be written, so MMWatchdog sends the alert from memory: %s", deliveryLine(delivery.Destination, delivery.Event, e, delay, nil))
			}()
		}
	}
	for key := range d.fromMemory {
		if !waiting[key.eventID] {
			delete(d.fromMemory, key)
		}
	}
}

// deliveriesFromMemory are where event goes from memory, as Store.Save
// would queue it under route. A joined node sends its alerts to the hub, and
// once the hub has not accepted an alert for
// send_alert_without_hub_after_waiting_for after a failed attempt, also
// straight to the destinations of its fleet settings, as the outbox's
// fallback does. A hub sends its own alerts straight to its destinations:
// its alert grouping works on the outbox. The caller holds d.mu.
func (d *Daemon) deliveriesFromMemory(event checksandalerts.Event, route store.AlertRoute, now time.Time) []store.Delivery {
	alert := d.store.PreparedEvent(event, route)
	from := store.QueuedBy{Role: store.StandaloneSender, Node: d.store.Node, FleetIdentity: d.store.FleetIdentity}
	switch {
	case event.DeliverOnThisNode || !route.Joined && !route.Hub:
	case route.Hub:
		from.Role = store.FleetHubSender
	case event.CheckGroup == checksandalerts.NodeReportsCheckGroup && event.Check == checksandalerts.HubCheck:
		from.Role, from.DestinationsWhoseServiceKeepsIncidents = store.FallbackSender, route.DestinationsWhoseServiceKeepsIncidents
	default:
		toHub := []store.Delivery{{Event: alert, Destination: config.HubDestination}}
		hub := d.fromMemory[memoryDeliveryKey{event.ID, config.HubDestination}]
		if d.fleetSettings == nil || hub == nil || hub.done || hub.attempts == 0 || now.Sub(hub.since) < d.fleetSettings.Fleet.SendAlertWithoutHubAfterWaitingFor.Value() {
			return toHub
		}
		from.Role, from.DestinationsWhoseServiceKeepsIncidents = store.FallbackSender, route.DestinationsWhoseServiceKeepsIncidents
		return append(toHub, d.store.DeliveriesFromMemory(alert, route.Destinations, from)...)
	}
	return d.store.DeliveriesFromMemory(alert, route.Destinations, from)
}

// send delivers event with sender. A panic in the destination's module
// counts as a failed attempt, and the journal gets its stack: a panic that
// ended the node monitoring process would end every check's failing time
// whose incident has not opened yet, at each start.
func send(ctx context.Context, sender checksandalerts.NotificationSender, event checksandalerts.Event) (e error) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("an alert delivery panicked: %v\n%s", p, debug.Stack())
			e = fmt.Errorf("the destination's module stopped with an internal error: %s", primitives.DetailLine(fmt.Sprint(p)))
		}
	}()
	return sender.Send(ctx, event)
}
