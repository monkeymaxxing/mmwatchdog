package daemon

import (
	"context"
	"log"
	"sync"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/fleet"
)

// One state per destination name. A URL or alert source change starts fresh;
// secret rotation keeps the connection, queued alerts, and incident set.
type mmhotlineDestinationState struct {
	url             string
	alertSourceID   string
	connection      *checksandalerts.MMHotlineConnection
	checkInInFlight bool
}

// The caller holds d.mu. The map follows the active route, including copied
// destinations on a joined node.
func (d *Daemon) syncMMHotlineDestinations() {
	destinations := d.route().Destinations
	for name, state := range d.mmhotlineDestinations {
		inputs := destinations[name].MMHotline()
		if inputs == nil || inputs.URL != state.url || inputs.AlertSourceID != state.alertSourceID {
			delete(d.mmhotlineDestinations, name)
		}
	}
	for name, destination := range destinations {
		if inputs := destination.MMHotline(); inputs != nil && d.mmhotlineDestinations[name] == nil {
			d.mmhotlineDestinations[name] = &mmhotlineDestinationState{url: inputs.URL, alertSourceID: inputs.AlertSourceID, connection: &checksandalerts.MMHotlineConnection{}}
		}
	}
}

// checkIns sends at start, after each reload, and every 30 seconds.
func (d *Daemon) checkIns(ctx context.Context) {
	ticker := time.NewTicker(fleet.CheckInEvery)
	defer ticker.Stop()
	var pending sync.WaitGroup
	defer pending.Wait()
	d.queueCheckIns(ctx, d.now(), &pending)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-d.checkInNow:
		}
		d.queueCheckIns(ctx, d.now(), &pending)
	}
}

// Each destination sends concurrently with its own delivery time limit.
// Joined nodes never send check-ins.
func (d *Daemon) sendCheckIn(ctx context.Context, now time.Time) {
	var pending sync.WaitGroup
	d.queueCheckIns(ctx, now, &pending)
	pending.Wait()
}

// queueCheckIns starts one check-in for each destination that has
// none in flight.
func (d *Daemon) queueCheckIns(ctx context.Context, now time.Time, pending *sync.WaitGroup) {
	type target struct {
		name        string
		destination checksandalerts.Destination
		state       *mmhotlineDestinationState
	}
	d.mu.Lock()
	route := d.route()
	d.syncMMHotlineDestinations()
	targets := []target{}
	if !route.Joined {
		for _, name := range checksandalerts.Names(d.mmhotlineDestinations) {
			state := d.mmhotlineDestinations[name]
			//nolint:nilaway // syncMMHotlineDestinations above stores a state for every name, and Names returns only the map's own keys.
			if state.checkInInFlight {
				continue
			}
			state.checkInInFlight = true
			targets = append(targets, target{name, route.Destinations[name], state})
		}
	}
	message := fleet.CheckIn{SentAt: now, SentByNodeName: d.store.Node}
	if window, open := d.snapshot.Maintenance[checksandalerts.EveryCheckGroup]; open && window.Until.After(now) {
		message.NodesInWholeNodeMaintenance = append(message.NodesInWholeNodeMaintenance, fleet.NodeInMaintenance{NodeName: d.store.Node, MaintenanceUntil: window.Until, Reason: window.Reason, StartedBy: window.User})
	}
	d.mu.Unlock()
	if len(targets) == 0 {
		return
	}
	if d.hub != nil {
		message.NodesInWholeNodeMaintenance = append(message.NodesInWholeNodeMaintenance, d.hub.WholeNodeMaintenance(now)...)
	}
	body, shortened := fleet.FitCheckIn(message, fleet.CheckInBytes, fleet.MaxNodesInWholeNodeMaintenance)
	for _, target := range targets {
		pending.Go(func() {
			defer func() { d.mu.Lock(); target.state.checkInInFlight = false; d.mu.Unlock() }()
			secret, err := checksandalerts.ReadMMHotlineSecret(target.destination.MMHotline().SigningSecretFile)
			if err == nil {
				_, err = target.state.connection.Send(ctx, "check-in", *target.destination.MMHotline(), secret, body, target.destination.TimeLimitPerDeliveryAttempt.Value())
			}
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				log.Printf("MMHotline %s check-in: %v", target.name, err)
			}
			target.state.connection.CheckInResult(now, err, shortened)
		})
	}
}

// mmhotlineStatus describes every destination in name order. The caller
// holds d.mu; each connection has its own lock for in-flight requests.
func (d *Daemon) mmhotlineStatus() []fleet.MMHotlineStatus {
	d.syncMMHotlineDestinations()
	statuses := []fleet.MMHotlineStatus{}
	for _, name := range checksandalerts.Names(d.mmhotlineDestinations) {
		state := d.mmhotlineDestinations[name]
		connection := state.connection //nolint:nilaway // syncMMHotlineDestinations above stores a state for every name, and Names returns only the map's own keys.
		connection.Mu.Lock()
		//nolint:nilaway // syncMMHotlineDestinations above stores a state for every name, and Names returns only the map's own keys.
		status := fleet.MMHotlineStatus{Destination: name, URL: state.url, AlertSourceID: state.alertSourceID, DeliveryProblems: []string{}, CheckInShortened: connection.CheckInShortened}
		if !connection.LastSignedAnswerAt.IsZero() {
			at := connection.LastSignedAnswerAt.UTC()
			status.LastSignedAnswerAt = &at
		}
		if connection.OffsetKnown {
			seconds := int64(connection.Offset.Seconds())
			status.NodeClockAheadSeconds = &seconds
		}
		checkInError, checkInErrorAt := connection.CheckInError, connection.CheckInErrorAt
		connection.Mu.Unlock()
		next, lastError, err := d.store.Head(name)
		switch {
		case err != nil:
			status.DeliveryProblems = append(status.DeliveryProblems, "MMHotline delivery status: "+err.Error())
		case lastError != "":
			status.DeliveryProblems = append(status.DeliveryProblems, lastError+" Trying again at "+next.UTC().Format("15:04 UTC")+".")
		}
		if checkInError != "" {
			status.DeliveryProblems = append(status.DeliveryProblems, "The check-in to MMHotline at "+checkInErrorAt.UTC().Format("15:04 UTC")+" failed: "+checkInError)
		}
		status.ClockWarning = connection.ClockProblem(checksandalerts.ClockShownAfter)
		statuses = append(statuses, status)
	}
	return statuses
}
