package daemon

import (
	"time"

	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/store"
)

// reconcileReports is the hub's fleet incident set step on the delivery
// worker's tick: it applies the reports accepted since the last step to
// the fleet incident set of every destination that keeps one. Reports that
// could not be applied wait for the next step. The fleet store's lock is
// never held with d.mu.
func (d *Daemon) reconcileReports(now time.Time) {
	changes := d.hub.TakeReported()
	if len(changes) == 0 {
		return
	}
	awaiting := d.hub.AwaitingFirstReports(now)
	d.mu.Lock()
	var e error
	if len(store.IncidentSets(d.config.Alerts.Destinations)) > 0 {
		e = d.store.ReconcileIncidentSets(changes, d.config.Alerts.Destinations, awaiting, now)
		d.storage.Report(fleet.FleetIncidentSets, e)
	}
	d.mu.Unlock()
	if e != nil {
		d.hub.RestoreReported(changes)
	}
}
