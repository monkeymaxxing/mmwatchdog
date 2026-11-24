package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/store"
)

// syncFleetSettings fetches the fleet settings when the latest report reply
// names a version this node does not hold, keeps them privately, and
// activates them.
func (d *Daemon) syncFleetSettings(ctx context.Context, code fleet.JoinCode, stateDir string) error {
	d.mu.Lock()
	held, modules := "", d.config.Modules
	if d.fleetSettings != nil {
		held = d.fleetSettings.Version
	}
	d.mu.Unlock()
	if d.rep.FleetSettingsVersion == held {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := fleet.PinnedClient(code.Fingerprint)
	defer client.CloseIdleConnections()
	req, e := http.NewRequestWithContext(ctx, "GET", "https://"+code.Address+"/v1/settings", nil)
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+d.rep.Credential.Credential)
	resp, e := client.Do(req)
	if e != nil {
		return fmt.Errorf("fetching fleet settings: %w", e)
	}
	defer resp.Body.Close()
	body, e := primitives.ReadBody(resp.Body, 1024*1024)
	if e != nil {
		return fmt.Errorf("fetching fleet settings: %w", e)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching fleet settings: %w", fleet.HubError(resp.StatusCode, body))
	}
	s, e := decodeHubSettings(body, modules)
	if e != nil {
		return fmt.Errorf("the hub sent invalid settings: %w", e)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.config.JoinFleetHub == nil {
		return nil // a reload removed join_fleet_hub meanwhile
	}
	if e = primitives.WriteFileAtomic(filepath.Join(stateDir, fleet.FleetSettingsFile), body); e != nil {
		return fmt.Errorf("saving fleet settings: %w", e)
	}
	return d.applyFleetSettings(s)
}

// decodeHubSettings reads the hub's settings and validates them. A hub may be
// one minor release newer than this node and send a field that this node does
// not know; the node ignores that field rather than keep stale destinations.
func decodeHubSettings(body []byte, modules *checksandalerts.ModuleCatalog) (*fleet.FleetSettings, error) {
	var s fleet.FleetSettings
	if e := checksandalerts.DecodeJSONUpToAllowingUnknownFields(body, &s, 1024*1024); e != nil {
		return nil, e
	}
	if e := s.Validate(modules); e != nil {
		return &s, e
	}
	s.RunDestinationModulesAsAJoinedNode()
	return &s, nil
}

// applyFleetSettings is the settings activation entry point on a joined node. As
// on reload, the commit cancels queued deliveries to destinations whose
// identity changed; requests already in flight finish. New fleet timings
// apply to the hub-outage alerter's hub check from its next receipt. The caller
// holds d.mu.
func (d *Daemon) applyFleetSettings(s *fleet.FleetSettings) error {
	old := d.route()
	next := old
	next.Destinations = s.Destinations
	next.DestinationsWhoseServiceKeepsIncidents = s.DestinationsWhoseServiceKeepsIncidents
	if e := d.control(d.snapshot.Clone(), nil, next); e != nil {
		return e
	}
	d.clearChanged(old, next)
	d.fleetSettings = s
	defs := d.definitions(d.config)
	d.setDefinitions(defs)
	if def, ok := defs[fleet.FleetKey(checksandalerts.HubCheck)]; ok {
		// The hub's silence severity may have changed with its settings.
		d.pending = append(d.pending, d.snapshot.ReconcileOne(d.config, fleet.FleetKey(checksandalerts.HubCheck), &def, d.now())...)
	}
	return nil
}

// fallBack is the joined node's fallback step: see Store.FallBack. Nothing
// falls back before this node holds the fleet settings, or while an alert
// is in flight to the hub. Once the hub's latest attempt failed, every alert
// older than send_alert_without_hub_after_waiting_for falls back without
// waiting for its own attempt. The step moves alerts until none is due: the
// delivery worker reads the queue right after it, and would otherwise send
// the hub a later alert of a check whose earlier alert has just fallen
// back, which the hub could deliver first.
func (d *Daemon) fallBack(now time.Time) {
	for d.fallBackOneAlert(now) {
	}
}

// fallBackOneAlert moves the oldest alert for the hub to the destinations
// when it is due, and reports whether it moved one.
func (d *Daemon) fallBackOneAlert(now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.config.JoinFleetHub == nil || d.fleetSettings == nil || d.deliveries[config.HubDestination] {
		return false
	}
	happened := func(seq int64, eventID string, at time.Time) time.Time {
		return d.hubAlertHappened(seq, eventID, at, now)
	}
	moved, e := d.store.FallBack(d.fleetSettings.Destinations, d.fleetSettings.DestinationsWhoseServiceKeepsIncidents, d.fleetSettings.Fleet.SendAlertWithoutHubAfterWaitingFor.Value(), d.latestHubAttemptError != nil, now, happened)
	d.storage.Report(fleet.AlertFallback, e)
	if moved == nil {
		return false
	}
	alert := "the " + moved.Alert.Type + " alert"
	if moved.Alert.Check != "" {
		alert += " of " + moved.Alert.AboutCheck()
	}
	if moved.FollowsEarlierAlertOfTheSameCheck {
		log.Printf("%s goes directly to the alert destinations because an earlier alert for the check is still queued for direct delivery", alert)
		return true
	}
	log.Printf("%s goes directly to the alert destinations, because the latest attempt to deliver an alert to the hub failed: %v", alert, d.latestHubAttemptError)
	return true
}

// hubAlertHappened is when the oldest alert queued for the hub, the one with
// seq and eventID, happened on the monotonic clock, for the wait of the
// fallback. The outbox keeps its time on the wall clock, at. When the node
// first sees the alert at the head of the queue, at counts on the monotonic
// clock from then on, and is never later than that moment: a backward step
// of the clock between the alert and that moment so holds back no fallback.
// The caller holds d.mu.
func (d *Daemon) hubAlertHappened(seq int64, eventID string, at, now time.Time) time.Time {
	if d.hubAlert.seq != seq || d.hubAlert.eventID != eventID {
		d.hubAlert = queueHead{seq: seq, eventID: eventID, at: now.Add(min(at.Sub(now), 0))}
	}
	return d.hubAlert.at
}

// handleAlert is the alert acceptance entry point on the hub. The alert is
// saved under its event ID before the reply, and a repeated ID is acknowledged
// without a second entry. A node can only send alerts in its own name. A
// reminder, and only a reminder, names its destination.
func (d *Daemon) handleAlert(w http.ResponseWriter, r *http.Request) {
	name, ok := d.hub.Authenticate(r)
	if !ok {
		apiError(w, fmt.Errorf("unknown node credential"), 401)
		return
	}
	var event checksandalerts.Event
	b, e := primitives.ReadBody(r.Body, fleet.AlertLimit)
	if e == nil {
		e = checksandalerts.DecodeJSONUpTo(b, &event, fleet.AlertLimit)
	}
	if e == nil && (!fleet.IDPattern.MatchString(event.ID) || !fleet.Notifying[event.Type] || !fleet.ValidSeverities(event) || event.At.IsZero() || event.Nodes != nil || event.NodeCount != 0 || event.MMHotlineAlert != nil || event.MMHotlineAlertSource != "" || (event.Maintenance != nil) != strings.HasPrefix(event.Type, "maintenance_") || !fleet.ValidMaintenanceCheckGroups(event) ||
		(event.Finding != "" && !checksandalerts.ValidFindingID(event.Finding)) || !fleet.ValidExactFinding(event) ||
		(event.FleetIncidentKey != "") != checksandalerts.BelongsToAnIncident(event.Type) ||
		(event.FleetIncidentKey != "" && !checksandalerts.FleetIncidentKeyPattern.MatchString(event.FleetIncidentKey)) ||
		(event.Type == "reminder") != checksandalerts.NamePattern.MatchString(event.Destination)) {
		e = fmt.Errorf("invalid alert")
	}
	if e != nil {
		apiError(w, e, 400)
		return
	}
	event.Node, event.Seq = name, 0
	e = d.store.Accept(name, event, d.now())
	if errors.Is(e, store.ErrIntakeFull) {
		d.reportStorage(fleet.AlertIntake, nil)
	} else {
		d.reportStorage(fleet.AlertIntake, e)
	}
	if e != nil {
		apiError(w, fmt.Errorf("alert could not be saved: %w", e), 503)
		return
	}
	writeJSON(w, map[string]bool{"accepted": true})
}

// alert grouping runs the alert grouping step on the hub and commits its messages to the
// outbox in order. active and awaitingFirstReports are read from the fleet
// store before d.mu is taken (fleet.Hub.AwaitingFirstReports). The caller
// holds d.mu.
func (d *Daemon) groupFleetAlerts(active map[string]bool, awaitingFirstReports bool, now time.Time) error {
	rows, e := d.store.Intake()
	if e != nil {
		return e
	}
	for key, at := range d.alertGroups.Sent {
		if now.Sub(at) >= d.alertGroups.Window {
			delete(d.alertGroups.Sent, key)
		}
	}
	p := d.alertGroups.Plan(rows, active, now)
	if len(p.Messages) == 0 && len(p.Hold) == 0 && len(p.Done) == 0 {
		return nil
	}
	if e = d.store.Emit(p, d.config.Alerts.Destinations, awaitingFirstReports, now); e != nil {
		return e
	}
	for _, key := range p.Sent {
		d.alertGroups.Sent[key] = now
	}
	for _, g := range p.Closed {
		delete(d.alertGroups.Quiet, g)
	}
	return nil
}
