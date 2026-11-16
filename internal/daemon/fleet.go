package daemon

import (
	"fmt"
	"net/http"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/primitives"
)

// hubRoutes serves the hub's endpoints: joins, reports, settings, and
// alerts. A join or report reaches the daemon after the fleet store has
// committed it and released its lock.
func (d *Daemon) hubRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/join", d.handleJoin)
	mux.HandleFunc("POST /v1/report", d.handleReport)
	mux.HandleFunc("GET /v1/settings", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := d.hub.Authenticate(r); !ok {
			apiError(w, fmt.Errorf("unknown node credential"), 401)
			return
		}
		writeJSON(w, d.hub.FleetSettings.Load())
	})
	mux.HandleFunc("POST /v1/alerts", d.handleAlert)
	return mux
}

// handleJoin creates the node's fleet check once its join is committed.
func (d *Daemon) handleJoin(w http.ResponseWriter, r *http.Request) {
	var req fleet.JoinRequest
	b, e := primitives.ReadBody(r.Body, fleet.JoinLimit)
	if e == nil {
		e = checksandalerts.DecodeJSON(b, &req)
	}
	if e != nil {
		apiError(w, e, 400)
		return
	}
	now := d.now()
	credential, status, e := d.hub.Join(req.Name, req.Secret, req.NodeFleetSettings, fleet.RemoteHost(r), now)
	if e != nil {
		apiError(w, e, status)
		return
	}
	d.joined(req.Name, now, req.NodeFleetSettings)
	writeJSON(w, map[string]string{"credential": credential})
}

// handleReport accepts a report. An authenticated current report is a receipt
// for the node's fleet check even when it cannot be saved. The reply
// carries the hub's cursor, the version of its settings, and whether
// this node is the hub-outage alerter.
func (d *Daemon) handleReport(w http.ResponseWriter, r *http.Request) {
	now := d.now()
	name, ok := d.hub.Authenticate(r)
	if !ok {
		apiError(w, fmt.Errorf("unknown node credential"), 401)
		return
	}
	var report fleet.NodeStatusReport
	b, e := primitives.ReadBody(r.Body, fleet.ReportLimit)
	if e == nil {
		e = checksandalerts.DecodeJSONUpTo(b, &report, fleet.ReportLimit)
	}
	if e == nil {
		e = report.Validate()
	}
	if e != nil {
		apiError(w, e, 400)
		return
	}
	reply, status, e := d.hub.Accept(name, &report, now)
	switch status {
	case http.StatusOK:
		d.receipt(name, report.Maintenance[checksandalerts.EveryCheckGroup].Until, report.FleetSettings(), now)
		// The report is saved and the node counts as reporting. Its readings
		// are saved after that, so that they never hold up either.
		reply.ReadingsCursor = d.hub.SaveReadings(name, &report, now)
	case http.StatusServiceUnavailable:
		// The hub could not save the report. It still counts as contact, so
		// a full disk on the hub raises no false silence alert. The node's
		// settings for the fleet stay those of its latest saved report,
		// which a restarted hub would read again.
		d.contact(name, report.Maintenance[checksandalerts.EveryCheckGroup].Until, now)
	}
	if e != nil {
		apiError(w, e, status)
		return
	}
	reply.FleetSettingsVersion = d.hub.FleetSettings.Load().Version
	if len(report.IncidentsSentWithoutTheHub) > 0 {
		// An incident is known once a report applied earlier added it; this
		// report is applied after the reply.
		reply.IncidentsKnownToTheHub, _ = d.store.IncidentsKnownToTheHub(name, report.IncidentsSentWithoutTheHub)
	}
	writeJSON(w, reply)
}
