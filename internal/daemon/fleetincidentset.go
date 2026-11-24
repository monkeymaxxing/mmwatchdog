package daemon

import (
	"time"

	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/store"
)

// setNewlyAffectedNodesEvery gives the store the hub's shared tick for the
// updates of findings grouped by exact finding, at start and at reload.
func setNewlyAffectedNodesEvery(s *store.Store, c *config.Config) {
	if c.FleetHubRole != nil {
		s.NewlyAffectedNodesEvery.Store(int64(c.FleetHubRole.SendNewlyAffectedNodesEvery.Value()))
	}
}

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
	// Every destination keeps a set for the findings with an exact finding.
	if len(d.config.Alerts.Destinations) > 0 {
		e = d.store.ReconcileIncidentSets(changes, d.config.Alerts.Destinations, awaiting, now)
		d.storage.Report(fleet.FleetIncidentSets, e)
	}
	d.mu.Unlock()
	if e != nil {
		d.hub.RestoreReported(changes)
	}
}
