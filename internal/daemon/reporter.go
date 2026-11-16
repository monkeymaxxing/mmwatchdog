package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/store"
)

// reports sends a report every report_every, starting at a random offset so
// nodes restarted together spread out. A node that holds no credential for
// its hub skips that wait, at the start and after a reload, because its
// first report joins: a new node appears in the fleet view at once. A
// changed report_every applies from the next report. The hub reports to
// itself at once, so the fleet view lists it from the start. The hub's
// retention runs in the notifications worker (Daemon.retention).
func (d *Daemon) reports(ctx context.Context) {
	d.reporting.Store(true)
	defer d.reporting.Store(false)
	next := time.Now()
	if d.hub == nil && !d.joinsWithNextReport() {
		next = next.Add(rand.N(d.reportEvery()))
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.joinNow:
			if !d.joinsWithNextReport() {
				continue
			}
			next = time.Now()
		case accepted := <-d.reportNow:
			accepted <- d.report(ctx)
			continue
		case <-time.After(time.Until(next)):
		}
		_ = d.report(ctx) // report logs its own error; the next report supersedes a failed one.
		// A report that outlasts the interval is followed at once.
		if next = next.Add(d.reportEvery()); next.Before(time.Now()) {
			next = time.Now()
		}
	}
}

// reportEvery is the report interval in force.
func (d *Daemon) reportEvery() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fleetTimings(d.config).ReportEvery.Value()
}

// report reads the daemon's state under d.mu, then measures and sends without
// it. A failed report is not retried; the next one supersedes it. It returns
// the error of sending the report, nil once the hub accepted it.
func (d *Daemon) report(ctx context.Context) error {
	d.mu.Lock()
	code, stateDir, name := d.joinCode, d.config.StateDir, d.store.Node
	if code == nil && d.hub == nil {
		d.mu.Unlock()
		return fmt.Errorf("this node has no join code for the MMWatchdog fleet hub")
	}
	now := d.now()
	r := d.buildReport(now)
	d.mu.Unlock()
	measure, cancel := context.WithTimeout(ctx, 5*time.Second)
	r.NodeResources = fleet.NodeResourceMetrics(measure, &d.rep.Sampler)
	cancel()
	if d.hub != nil {
		reply, _, e := d.hub.Accept(name, &r, now)
		if e != nil {
			log.Printf("fleet report: %v", e)
		}
		d.rep.After = reply.EventsCursor
		d.continueEventsAfter(reply.EventsCursor)
		return e
	}
	d.addReadings(&r)
	sendError := d.send(ctx, *code, stateDir, name, &r)
	e, sent := sendError, sendError == nil
	if sent && len(d.rep.IncidentsKnownToTheHub) > 0 {
		d.reportStorage("incidents sent without the hub", d.store.CarriedToHub(d.rep.IncidentsKnownToTheHub))
	}
	if sent {
		e = d.syncFleetSettings(ctx, *code, stateDir)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.link == nil {
		return sendError // a reload removed join_fleet_hub meanwhile
	}
	if sent {
		d.watch(d.rep.OutageAlerter, now)
	}
	if e != nil {
		log.Printf("fleet report: %v", e)
		d.link.LastError, d.link.ErrorAt = e.Error(), d.now()
		return sendError
	}
	d.link.LastReport, d.link.LastError, d.link.ErrorAt = now, "", time.Time{}
	return nil
}

// reportToHubAtOnce sends a report at once and waits until the hub has
// accepted it, so that a maintenance command on a joined node returns only
// once the hub holds the window: a reboot right after it then sends no alert
// that the node stopped reporting. The caller does not hold d.mu.
func (d *Daemon) reportToHubAtOnce(ctx context.Context) error {
	// The control API must answer within responseWriteTimeout, and a
	// report that runs already finishes first.
	if !d.reporting.Load() {
		return fmt.Errorf("node status reporting is not running. MMWatchdog may be stopping")
	}
	ctx, cancel := context.WithTimeout(ctx, responseWriteTimeout-5*time.Second)
	defer cancel()
	accepted := make(chan error, 1)
	select {
	case d.reportNow <- accepted:
	case <-ctx.Done():
		return fmt.Errorf("the node status report could not start: %w", ctx.Err())
	}
	select {
	case e := <-accepted:
		return e
	case <-ctx.Done():
		return fmt.Errorf("the fleet hub did not accept the node status report before the time limit: %w", ctx.Err())
	}
}

// buildReport copies what a report carries. It uses the same summary as
// status. The caller holds d.mu.
func (d *Daemon) buildReport(now time.Time) fleet.NodeStatusReport {
	checks, delivery, storageError := d.status("", now, nil)
	reported := map[string]fleet.ReportedCheck{}
	for key, st := range checks {
		rc := fleet.ReportedCheckOf(st)
		// Each failed unit of an every_systemd_unit check is reported
		// with its own state and incident.
		for name, unit := range st.FindingStatuses {
			if rc.Findings == nil {
				rc.Findings = map[string]fleet.ReportedCheck{}
			}
			rc.Findings[name] = fleet.ReportedCheckOf(unit)
		}
		reported[key] = rc
	}
	d.rep.Seq++
	r := fleet.NodeStatusReport{StartID: d.startID, Seq: d.rep.Seq, Sent: now, Build: d.Build, Config: config.Fingerprint(d.config), Summary: incidents.Summarize(checks, storageError), Checks: reported, Maintenance: maps.Clone(d.snapshot.Maintenance), PreferOutageAlerter: d.config.JoinFleetHub != nil && d.config.JoinFleetHub.PreferThisNodeAsHubOutageAlerter, Delivery: delivery, StorageError: storageError, History: d.store.History, After: d.rep.After, MeasurementHistoryID: d.history.ID, ReadingsAfter: d.rep.ReadingsAfter,
		AlertSeverityWhenThisNodeGoesSilent: d.config.AlertSeverityWhenThisNodeGoesSilent, SendSilenceAlertsOfThisNodeOnlyToDestinations: d.config.SendSilenceAlertsOfThisNodeOnlyToDestinations, RaiseToSosAfterThisNodeIsSilentFor: d.config.RaiseToSosAfterThisNodeIsSilentFor, ThisNodeIsInNodeGroups: d.config.ThisNodeIsInNodeGroups, RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent: d.config.RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent}
	sent, e := d.store.SentWithoutTheHub()
	if e != nil {
		log.Printf("fleet report: incidents sent without the hub: %v", e)
	}
	if r.After >= 0 {
		events, e := d.store.EventsAfter(r.After, fleet.ReportEvents, fleet.ReportEventBytes)
		if e != nil {
			// Send the state now; the events follow in a later report.
			log.Printf("fleet report events: %v", e)
		}
		r.Events = events
	}
	// A long outage can leave more fallback incidents than one request
	// carries. Keep every record, and send the oldest that fit; confirmed
	// records leave the store so following reports make progress.
	encoded, _ := json.Marshal(r)
	room := fleet.ReportLimit - fleet.ReportSpareBytes - len(encoded)
	for _, incident := range sent {
		encoded, _ := json.Marshal(incident)
		if len(encoded)+1 > room {
			break
		}
		r.IncidentsSentWithoutTheHub = append(r.IncidentsSentWithoutTheHub, incident)
		room -= len(encoded) + 1
	}
	return r
}

// addReadings gives a joined node's report the readings that the hub has not
// received yet, oldest first: at most ReportReadingsBytes of them, and no
// more than the request size limit leaves room for beside the rest of the
// report. The following reports carry the rest, so a node catches up after a
// hub outage. The reporting goroutine calls it without d.mu.
func (d *Daemon) addReadings(r *fleet.NodeStatusReport) {
	if r.ReadingsAfter < 0 {
		return // the first reply tells this node where the hub is
	}
	encoded, e := json.Marshal(r)
	if e != nil {
		return // sending the report will say why
	}
	readings, e := d.history.After(r.ReadingsAfter, fleet.ReadingsRoom(len(encoded), d.reportEvery()))
	if e != nil {
		// Send the state now; the readings follow in a later report.
		log.Printf("fleet report readings: %v", e)
	}
	r.Readings = readings
}

// ContinueEventsAfter keeps this node's event sequence numbers from falling
// behind the number that the hub holds (Store.ContinueEventsAfter).
func (d *Daemon) continueEventsAfter(sequenceNumber int64) {
	if e := d.store.ContinueEventsAfter(sequenceNumber); e != nil {
		d.reportStorage(stateCommit, e)
	}
}

// joinsWithNextReport reports whether this node has a join code but no
// credential for that hub under its name, so that its next report joins.
func (d *Daemon) joinsWithNextReport() bool {
	d.mu.Lock()
	code, stateDir, name := d.joinCode, d.config.StateDir, d.store.Node
	d.mu.Unlock()
	return code != nil && d.credentialFor(*code, stateDir, name) == nil
}

// credentialFor returns the credential that this node holds for the hub of
// code under name, or nil when it holds none for them.
func (d *Daemon) credentialFor(code fleet.JoinCode, stateDir, name string) *fleet.HubCredential {
	cred := d.rep.Credential
	if cred == nil {
		cred = fleet.ReadCredential(filepath.Join(stateDir, fleet.CredentialFile))
	}
	if cred != nil && (cred.Hub != code.Fingerprint || cred.Node != name) {
		return nil
	}
	return cred
}

// send joins the hub when this node has no credential for it, then sends
// the report. A credential for another hub or host name is discarded, and one
// the hub refuses is discarded so the next report joins again.
func (d *Daemon) send(ctx context.Context, code fleet.JoinCode, stateDir, name string, r *fleet.NodeStatusReport) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := fleet.PinnedClient(code.Fingerprint)
	defer client.CloseIdleConnections()
	path := filepath.Join(stateDir, fleet.CredentialFile)
	cred := d.credentialFor(code, stateDir, name)
	if cred == nil {
		d.rep.Credential = nil
		if e := os.Remove(path); e != nil && !os.IsNotExist(e) {
			return e
		}
		credential, e := fleet.JoinHub(ctx, client, code, name, r.FleetSettings())
		if e != nil {
			return e
		}
		cred = &fleet.HubCredential{Hub: code.Fingerprint, Node: name, Credential: credential}
		b, _ := json.Marshal(cred)
		if e = primitives.WriteFileAtomic(path, b); e != nil {
			return fmt.Errorf("saving the hub credential: %w", e)
		}
		d.rep.After, d.rep.ReadingsAfter = -1, -1
	}
	d.rep.Credential = cred
	status, body, e := fleet.Post(ctx, client, "https://"+code.Address+"/v1/report", cred.Credential, r)
	if e != nil {
		return e
	}
	switch status {
	case http.StatusOK:
		var reply fleet.ReportReply
		if e = json.Unmarshal(body, &reply); e != nil || !reply.Valid() {
			return fmt.Errorf("the hub sent an invalid report reply")
		}
		d.rep.After, d.rep.ReadingsAfter, d.rep.FleetSettingsVersion, d.rep.OutageAlerter, d.rep.IncidentsKnownToTheHub = reply.EventsCursor, reply.ReadingsCursor, reply.FleetSettingsVersion, reply.OutageAlerter, reply.IncidentsKnownToTheHub
		// A history that is behind the hub, as after a restore from an older
		// copy, gives its next events and readings numbers that the hub will
		// take.
		d.continueEventsAfter(reply.EventsCursor)
		if e = d.history.ContinueAfter(reply.ReadingsCursor); e != nil {
			d.reportStorage(store.MeasurementHistoryWrite, e)
		}
		return nil
	case http.StatusUnauthorized:
		d.rep.Credential, d.rep.After, d.rep.ReadingsAfter = nil, -1, -1
		if e := os.Remove(path); e != nil && !os.IsNotExist(e) {
			return e
		}
		return fmt.Errorf("the fleet hub refused the node credential. The node will join again before sending the next status report")
	}
	return fleet.HubError(status, body)
}
