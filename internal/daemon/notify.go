package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/store"
)

// readNotificationQueueEvery is how often the notifications worker reads its
// queue and delivers what is due. TestMain makes it shorter, so that a test
// of several deliveries in turn does not wait for each.
var readNotificationQueueEvery = 200 * time.Millisecond

// finishDeliveriesOnStopFor is how long a stopping run lets an attempt that
// is already on its way finish, so that it records the result in state.db.
// Without it the destination can get the alert and the next run still finds
// it in the outbox and sends it again. The reason for 5 seconds: a normal
// answer takes well under one, and systemd waits 90 seconds for the stop.
const finishDeliveriesOnStopFor = 5 * time.Second

// attemptContext is the context of one delivery attempt: it ends
// finishDeliveriesOnStopFor after the run's context ends, not at once.
func attemptContext(run context.Context) (context.Context, context.CancelFunc) {
	attempt, cancel := context.WithCancel(context.WithoutCancel(run))
	stop := context.AfterFunc(run, func() { time.AfterFunc(finishDeliveriesOnStopFor, cancel) })
	return attempt, func() { stop(); cancel() }
}

// sender selects the transport for a queued delivery, after checking its
// captured destination identity against the active route, independently of
// check logic. It returns false when that destination is gone or changed. The
// caller holds d.mu.
func (d *Daemon) sender(delivery store.Delivery) (checksandalerts.NotificationSender, bool) {
	route := d.route()
	if route.Joined && delivery.Destination == config.HubDestination {
		return fleet.HubSender{Code: *d.joinCode, Path: filepath.Join(d.config.StateDir, fleet.CredentialFile), Node: d.store.Node}, true
	}
	current, exists := d.activeTarget(delivery)
	if !exists || current.Inputs == nil {
		return nil, false
	}
	d.syncMMHotlineDestinations()
	of := checksandalerts.DeliveryOf{Destination: current, SendOnlyTheAlertTitle: delivery.Target.SendOnlyTheAlertTitle, Modules: d.config.Modules}
	if state := d.mmhotlineDestinations[delivery.Destination]; state != nil {
		of.MMHotline = state.connection
	}
	if route.Joined {
		// A joined node never reads the hub's paths on its own disk.
		of.FileContents = map[string]string{}
		maps.Copy(of.FileContents, d.fleetSettings.FileContentsByDestination[delivery.Destination])
	}
	return current.Inputs.Sender(of), true
}

// activeTarget is the destination that the active route has under the
// delivery's name, when it is still the one that the delivery captured
// (checksandalerts.SameDestination): a reload has not removed or replaced
// it. The caller holds d.mu.
func (d *Daemon) activeTarget(delivery store.Delivery) (checksandalerts.Destination, bool) {
	route := d.route()
	if route.Joined && delivery.Destination == config.HubDestination {
		return checksandalerts.Destination{}, true
	}
	current, exists := route.Destinations[delivery.Destination]
	return current, exists && checksandalerts.SameDestination(delivery.Target, current)
}

// attemptFinished records the end of an attempt to deliver the alert event,
// as delivery: what the destination answered, the wait before the next
// attempt, and the journal line. A failure to reach a destination that a
// reload replaced while the attempt was under way belongs to the old
// receiver: the reload cancelled the alert and started the new receiver's
// failed attempts and wait over (clearChanged), so the failure records
// nothing for the new one. The caller holds d.mu.
func (d *Daemon) attemptFinished(delivery store.Delivery, event checksandalerts.Event, e error) {
	var refused *checksandalerts.MMHotlineRefusal
	if _, current := d.activeTarget(delivery); e != nil && !errors.As(e, &refused) && !current {
		log.Printf("could not deliver the alert %q to alert destination %s, which a reload has replaced since; the alert goes no further: %v", cmp.Or(event.TitleWithAlertSeverity, event.Title, event.Message), delivery.Destination, e)
		return
	}
	delay, bookkeeping := d.store.Delivered(delivery, e)
	var passedTo []string
	if refused != nil && bookkeeping == nil {
		passedTo = d.passOnRefusedAlert(delivery.Destination, event, refused, d.now())
	}
	log.Print(deliveryLine(delivery.Destination, event, e, delay, passedTo))
	d.storage.Report("notification bookkeeping "+delivery.Destination, bookkeeping)
	d.afterAttempt(delivery, delay)
	d.noticeAttempted(delivery, e)
	if e == nil && bookkeeping == nil {
		d.destinationSucceeded(delivery.Destination, delivery.Target, d.now(), "delivered an alert")
	}
	if delivery.Destination == config.HubDestination {
		d.latestHubAttemptError = e
	}
}

// deliveryLine is the journal line of one attempt to deliver an alert:
// the alert's title, where it went, and whether the destination accepted
// it. Every alert that a node sends, also a hub-outage alerter's direct alert and
// each resolution, gets one, so a person who reads journalctl -u mmwatchdog
// on the node sees that it went out. delay is when the next attempt comes,
// from Store.Delivered, and passedTo the destinations that get an alert
// that the destination permanently refused (passOnRefusedAlert).
func deliveryLine(destination string, event checksandalerts.Event, err error, delay time.Duration, passedTo []string) string {
	to := "alert destination " + destination
	if destination == config.HubDestination {
		to = "the MMWatchdog fleet hub"
	}
	title := cmp.Or(event.TitleWithAlertSeverity, event.Title, event.Message)
	var refused *checksandalerts.MMHotlineRefusal
	switch {
	case err == nil:
		return fmt.Sprintf("delivered the alert %q to %s, which accepted it", title, to)
	case errors.As(err, &refused) && len(passedTo) > 0:
		return fmt.Sprintf("%s permanently refused the alert %q, so MMWatchdog sends it to %s: %v", to, title, strings.Join(passedTo, ", "), err)
	case errors.As(err, &refused):
		return fmt.Sprintf("%s permanently refused the alert %q, and no other destination can take it, so MMWatchdog dropped it: %v", to, title, err)
	}
	return fmt.Sprintf("could not deliver the alert %q to %s. MMWatchdog will try again in %s: %v", title, to, delay, err)
}

func (d *Daemon) reportStorage(operation string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.storage.Report(operation, err)
}

// queueHead is the alert at the head of a destination's queue in the
// outbox, by its sequence number and event ID, with a time of it on the
// monotonic clock.
type queueHead struct {
	seq     int64
	eventID string
	at      time.Time
}

// attemptDue reports that the delivery, the head of its destination's
// queue, is due for an attempt at now. The wait counts on the monotonic
// clock. After an attempt that failed, the delay that Store.Delivered
// returned counts from the attempt's end (afterAttempt). A head that the
// worker sees for the first time, such as a new alert or one that waited
// across a restart, is due when the outbox's time of its next attempt has
// come on the wall clock, but no later than the doubling delay after its
// failed attempts (store.DelayAfterFailedAttempts) from now: a clock that
// was stepped while MMWatchdog was stopped, or that MMWatchdog starts with before the time
// service has set it, can put the outbox's time off by any amount. The
// limit holds for every destination, also for a receiver that asked for a
// longer wait with Retry-After: such a receiver answers again, and an
// alert must not wait an hour on a time that may be off. A wait after a
// failed attempt holds for the destination as a whole: a new head, such as
// an sos alert that went ahead of the one that failed (Store.Pending), is
// due no sooner, so a failing or rate-limited destination gets no retry
// storm across many keys, and the sos goes first when the next attempt is
// allowed. The caller holds d.mu.
func (d *Daemon) attemptDue(delivery store.Delivery, now time.Time) bool {
	head, seen := d.nextAttempts[delivery.Destination]
	if !seen || head.seq != delivery.Seq || head.eventID != delivery.Event.ID {
		at := now.Add(min(max(delivery.Next.Sub(now), 0), store.DelayAfterFailedAttempts(delivery.Attempts)))
		if seen && head.at.After(at) {
			at = head.at
		}
		head = queueHead{seq: delivery.Seq, eventID: delivery.Event.ID, at: at}
		d.nextAttempts[delivery.Destination] = head
	}
	return !now.Before(head.at)
}

// afterAttempt records when the head of the delivery's queue is due again
// after an attempt: delay from now on the monotonic clock. An alert that
// was delivered, or that left the head of its queue, leaves no wait. The
// caller holds d.mu.
func (d *Daemon) afterAttempt(delivery store.Delivery, delay time.Duration) {
	if delay <= 0 {
		delete(d.nextAttempts, delivery.Destination)
		return
	}
	d.nextAttempts[delivery.Destination] = queueHead{seq: delivery.Seq, eventID: delivery.Event.ID, at: d.now().Add(delay)}
}

// retentionNow is the time that the retention steps count back from: the
// wall clock, but never later than the daemon's start plus the time that
// has passed since then on the monotonic clock. A forward step of the
// system clock so removes no record early, above all no undelivered alert:
// a server whose clock battery has run out boots years behind, queues an
// alert while its network is down, and its time service then steps the
// clock forward. A backward step only keeps records longer.
func (d *Daemon) retentionNow() time.Time {
	now := d.now()
	sinceStart := d.started.Add(now.Sub(d.started))
	if now.Round(0).After(sinceStart.Round(0)) {
		return sinceStart
	}
	return now
}

// retention removes what is older than the retention times: the node's
// events, undelivered alerts, and readings, and on a hub the events and the
// readings of the nodes. The notifications worker runs it once a minute,
// when it starts, and right after each reload, at retentionNow.
func (d *Daemon) retention(now time.Time) {
	d.reportStorage("notification retention", d.store.Prune(now))
	d.pruneMeasurementHistory(now)
	if d.hub != nil {
		d.hub.Prune(now)
	}
}

func (d *Daemon) notifications(ctx context.Context) {
	ticker := time.NewTicker(readNotificationQueueEvery)
	defer ticker.Stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	lastPrune := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.trimMeasurementHistories()
			// A reload asks for the retention step at once.
			select {
			case <-d.runRetentionNow:
				lastPrune = time.Time{}
			default:
			}
			if time.Since(lastPrune) >= time.Minute {
				d.retention(d.retentionNow())
				lastPrune = time.Now()
			}
			now := d.now()
			d.scheduleDestinationVerifications(ctx, now, &wg)
			d.checkDestinationHealth(now)
			if d.hub != nil {
				// The fleet store's lock is never held with d.mu.
				active, awaiting := d.hub.MaintenanceGroups(now), d.hub.AwaitingFirstReports(now)
				d.mu.Lock()
				e := d.groupFleetAlerts(active, awaiting, now)
				if e == nil {
					e = d.store.SendDueUpdatesAndReminders(d.config.Alerts.Destinations, now)
				}
				d.storage.Report(fleet.AlertGrouping, e)
				d.mu.Unlock()
			}
			if d.hub != nil {
				d.reconcileReports(now)
			}
			// Fallback reads the queue before this worker does, so it never
			// moves an alert this worker is about to send.
			d.fallBack(now)
			// While state.db cannot be written, the alerts that wait for a
			// commit go from memory; this needs no read of state.db's queue.
			d.deliverFromMemory(ctx, now, &wg)
			d.mu.Lock()
			finishedBeforeTheRead := maps.Clone(d.deliveriesFinished)
			d.mu.Unlock()
			deliveries, e := d.store.Pending()
			d.reportStorage("notification queue", e)
			if e != nil {
				continue
			}
			for _, delivery := range deliveries {
				d.mu.Lock()
				// Pending may have been read before a reload committed, so check
				// the destination under the lock that activates configuration. A
				// delivery to the destination that finished since the read may
				// have sent this very row: its rows wait for the next read.
				sender, ok := d.sender(delivery)
				if !ok || d.deliveries[delivery.Destination] || d.deliveriesFinished[delivery.Destination] != finishedBeforeTheRead[delivery.Destination] || !d.attemptDue(delivery, now) {
					d.mu.Unlock()
					continue
				}
				d.deliveries[delivery.Destination] = true
				d.mu.Unlock()
				wg.Add(1)
				go func(delivery store.Delivery) {
					defer wg.Done()
					event := delivery.Event
					if d.hub != nil {
						event = d.hub.Late(event, d.now())
					}
					attempt, endAttempt := attemptContext(ctx)
					e := send(attempt, sender, event)
					cutShort := attempt.Err() != nil
					endAttempt()
					d.mu.Lock()
					// An attempt that the stop cut short after the grace time is
					// not a failed attempt of the destination: it stays unrecorded.
					if !cutShort {
						d.attemptFinished(delivery, event, e)
					}
					d.deliveriesFinished[delivery.Destination]++
					delete(d.deliveries, delivery.Destination)
					d.mu.Unlock()
				}(delivery)
			}
		}
	}
}
