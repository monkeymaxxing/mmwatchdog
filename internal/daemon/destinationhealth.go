package daemon

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
)

// Ten minutes allows transient service failures and exponential retries to
// clear before another destination has to alert a person.
const destinationFailureNoticeAfter = 10 * time.Minute
const verifyDestinationsEvery = 24 * time.Hour

type destinationHealth struct {
	deliveryFailure *store.FailingDestination
	store.DestinationHealth
	verifying bool
	// oldestAt puts a loaded queue timestamp on the daemon's monotonic clock.
	oldestAt       time.Time
	oldestStoredAt time.Time
}

// DestinationVerificationResult is one reload verification, named so text
// and JSON both say exactly which destination was verified.
type DestinationVerificationResult struct {
	Destination string `json:"destination"`
	checksandalerts.DestinationVerification
}

func captureDestinationCredentials(c *config.Config) {
	for name, destination := range c.Alerts.Destinations {
		destination.CaptureCredentialContent(nil)
		c.Alerts.Destinations[name] = destination
	}
}

// healthOf is called with d.mu held. Startup schedules the first verification
// a day away: starting a daemon never sends verification requests.
func (d *Daemon) healthOf(name string) *destinationHealth {
	if d.destinationHealth == nil {
		d.destinationHealth = map[string]*destinationHealth{}
	}
	if d.destinationHealth[name] == nil {
		d.destinationHealth[name] = &destinationHealth{DestinationHealth: store.DestinationHealth{Target: d.route().Known()[name], NextVerificationAt: d.started.Add(verifyDestinationsEvery)}}
	}
	return d.destinationHealth[name]
}

// verificationDelivery captures the active route under d.mu; requests run
// outside the lock. The caller reserves the verification on this state.
func (d *Daemon) verificationDelivery(name string) (checksandalerts.DeliveryOf, *destinationHealth, bool) {
	route := d.route()
	destination, exists := route.Destinations[name]
	if !exists {
		return checksandalerts.DeliveryOf{}, nil, false
	}
	health := d.healthOf(name)
	if health.verifying {
		return checksandalerts.DeliveryOf{}, nil, false
	}
	health.verifying = true
	delivery := checksandalerts.DeliveryOf{Destination: destination, Modules: d.config.Modules}
	if route.Joined {
		delivery.FileContents = maps.Clone(d.fleetSettings.FileContentsByDestination[name])
		if delivery.FileContents == nil {
			delivery.FileContents = map[string]string{}
		}
	}
	return delivery, health, true
}

func (d *Daemon) finishVerification(name string, delivery checksandalerts.DeliveryOf, health *destinationHealth, result checksandalerts.DestinationVerification) {
	d.mu.Lock()
	defer d.mu.Unlock()
	current, exists := d.route().Destinations[name]
	if !exists || d.destinationHealth[name] != health || !checksandalerts.SameDestination(current, delivery.Destination) {
		return
	}
	health.verifying = false
	now := d.now()
	health.NextVerificationAt = now.Add(verifyDestinationsEvery)
	if !result.CanVerifyWithoutMessage {
		_ = d.record(nil)
		return
	}
	if result.AcceptsAddressAndCredential {
		health.VerificationFailingSince, health.VerificationError = time.Time{}, ""
		d.destinationSucceeded(name, delivery.Destination, now, "verified its address and credential")
		return
	}
	if health.VerificationFailingSince.IsZero() {
		health.VerificationFailingSince = now
	}
	health.VerificationError = result.VerificationMessage
	// Retry quietly once a minute; the failure notice still waits ten minutes.
	health.NextVerificationAt = now.Add(time.Minute)
	_ = d.record(nil)
}

func (d *Daemon) verifyDestinations(ctx context.Context, names []string) []DestinationVerificationResult {
	results := make([]DestinationVerificationResult, len(names))
	var workers sync.WaitGroup
	for index, name := range names {
		d.mu.Lock()
		delivery, health, ok := d.verificationDelivery(name)
		d.mu.Unlock()
		if !ok {
			results[index] = DestinationVerificationResult{Destination: name, DestinationVerification: checksandalerts.DestinationVerification{VerificationMessage: "destination was removed or is already being verified"}}
			continue
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			result := delivery.VerifyDestination(ctx)
			d.finishVerification(name, delivery, health, result)
			results[index] = DestinationVerificationResult{Destination: name, DestinationVerification: result}
		}()
	}
	workers.Wait()
	return results
}

func (d *Daemon) scheduleDestinationVerifications(ctx context.Context, now time.Time, workers *sync.WaitGroup) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, name := range checksandalerts.Names(d.route().Destinations) {
		health := d.healthOf(name)
		if health.verifying || now.Before(health.NextVerificationAt) {
			continue
		}
		delivery, reserved, ok := d.verificationDelivery(name)
		if !ok {
			continue
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			result := delivery.VerifyDestination(ctx)
			d.finishVerification(name, delivery, reserved, result)
		}()
	}
}

// destinationNotice is an ordinary notice with an explicit recipient list.
// It is delivered on this node, so a failed fleet hub cannot hold it up.
// The caller holds d.mu. record retains an uncommitted notice for retry.
func (d *Daemon) destinationNotice(name, severity, title, body string, now time.Time) {
	recipients := []string{}
	for _, other := range checksandalerts.Names(d.route().Destinations) {
		if other != name {
			recipients = append(recipients, other)
		}
	}
	d.noticeTo(recipients, severity, title, body, now)
}

// noticeTo records a notice of this node for exactly the recipients. The
// caller holds d.mu.
func (d *Daemon) noticeTo(recipients []string, severity, title, body string, now time.Time) {
	event := checksandalerts.Event{ID: incidents.ID(), Type: "notice", AlertSeverity: severity, At: now, Message: title, Body: body, Notify: true, DeliverOnThisNode: true, SendAlertsOnlyToDestinations: &recipients}
	_ = d.record([]checksandalerts.Event{event}) // record keeps the notice pending and reports a storage failure.
}

// passOnRefusedAlert sends an alert that destination name permanently refused
// at once to every other destination of this node that is not an mmhotline
// destination, with the alert's own severity, title, and body, whatever
// those destinations' alert_severities say: a refused SOS still reaches a
// person, and a duplicate is better than a dropped alert. Another MMHotline
// serves another team, and only an MMHotline permanently refuses, so the alert
// never goes back and forth. It returns the destinations that get the
// alert, none when the node has no such destination. The caller holds d.mu.
func (d *Daemon) passOnRefusedAlert(name string, refused checksandalerts.Event, refusal *checksandalerts.MMHotlineRefusal, now time.Time) []string {
	destinations := d.route().Destinations
	recipients := []string{}
	for _, other := range checksandalerts.Names(destinations) {
		if other != name && destinations[other].MMHotline() == nil {
			recipients = append(recipients, other)
		}
	}
	if len(recipients) == 0 {
		return nil
	}
	severity, title, text := refused.AlertSeverity, cmp.Or(refused.Title, refused.Message), refused.Body
	// An alert for MMHotline holds what it says in MMHotline's form. Its
	// resolution, such as resolved-sos, comes as information: it calls
	// nobody out.
	if a := refused.MMHotlineAlert; a != nil {
		severity, title, text = a.Severity, a.Title, a.Message
		if strings.HasPrefix(severity, "resolved-") {
			severity = "info"
		}
	}
	body := fmt.Sprintf("MMHotline (destination %s) permanently refused this alert, so MMWatchdog sends it here instead. MMHotline's reason (HTTP %d): %s", name, refusal.Status, refusal.Reason())
	if text != "" {
		body += "\n\n" + text
	}
	// The notice's title names this node itself.
	d.noticeTo(recipients, severity, strings.TrimPrefix(title, d.store.Node+": "), body, now)
	return recipients
}

func (d *Daemon) destinationSucceeded(name string, target checksandalerts.Destination, now time.Time, success string) {
	current, exists := d.route().Known()[name]
	if !exists || !checksandalerts.SameDestination(target, current) {
		return
	}
	health := d.healthOf(name)
	// A successful connection check cannot prove that delivery works. A
	// delivery also cannot replace a verification that has failed.
	if !d.refreshDestinationDeliveryFailures(now) || health.deliveryFailure != nil || !health.VerificationFailingSince.IsZero() {
		_ = d.record(nil)
		return
	}
	if health.FailingNoticeSent {
		health.FailingNoticeSent = false
		d.destinationNotice(name, "warn", "Destination "+name+" works again.", "The destination "+name+" "+success+". MMWatchdog continues delivery and verification.", now)
	} else {
		_ = d.record(nil) // Persist recovery of verification, without changing its schedule.
	}
}

// refreshDestinationDeliveryFailures checks the alerts still queued, including
// after partial delivery. The caller holds d.mu.
func (d *Daemon) refreshDestinationDeliveryFailures(now time.Time) bool {
	failing, err := d.store.DestinationQueues()
	d.storage.Report("destination health", err)
	if err != nil {
		return false
	}
	for _, health := range d.destinationHealth {
		health.deliveryFailure = nil
	}
	for _, destination := range failing {
		health := d.healthOf(destination.Destination)
		if !health.oldestStoredAt.Equal(destination.OldestUndeliveredAlertQueuedAt) {
			health.oldestStoredAt = destination.OldestUndeliveredAlertQueuedAt
			health.oldestAt = incidents.OnThisClock(destination.OldestUndeliveredAlertQueuedAt, now)
		}
		since := health.oldestAt
		if now.Sub(since) >= destinationFailureNoticeAfter {
			latest := destination.LatestAttemptError
			if latest == "" {
				latest = "the first delivery attempt has not completed"
			}
			destination.LatestAttemptError = latest
			health.deliveryFailure = &destination
		}
	}
	return true
}

// checkDestinationHealth observes outbox age and verification failure under
// the same lock as activation and delivery results.
func (d *Daemon) checkDestinationHealth(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.refreshDestinationDeliveryFailures(now) {
		return
	}
	errors := map[string]string{}
	for name, health := range d.destinationHealth {
		if health.deliveryFailure != nil {
			errors[name] = health.deliveryFailure.LatestAttemptError
		}
	}
	for name, health := range d.destinationHealth {
		if !health.VerificationFailingSince.IsZero() && now.Sub(health.VerificationFailingSince) >= destinationFailureNoticeAfter {
			errors[name] = health.VerificationError
		}
	}
	for _, name := range checksandalerts.Names(errors) {
		health := d.healthOf(name)
		if health.FailingNoticeSent {
			continue
		}
		health.FailingNoticeSent = true
		d.destinationNotice(name, "warn", "Destination "+name+" is failing.", fmt.Sprintf("The destination %s has had an undelivered alert or a failed verification for at least 10 minutes. Latest error: %s", name, errors[name]), now)
	}
}

func (d *Daemon) withFailingVerifications(failing []store.FailingDestination, now time.Time) []store.FailingDestination {
	for _, name := range checksandalerts.Names(d.destinationHealth) {
		health := d.healthOf(name)
		verificationFailed := !health.VerificationFailingSince.IsZero() && now.Sub(health.VerificationFailingSince) >= destinationFailureNoticeAfter
		if !verificationFailed && health.deliveryFailure == nil {
			continue
		}
		if slices.ContainsFunc(failing, func(f store.FailingDestination) bool { return f.Destination == name }) {
			continue
		}
		failure := store.FailingDestination{Destination: name}
		if health.deliveryFailure != nil {
			failure = *health.deliveryFailure
		}
		if verificationFailed {
			failure.VerificationFailingSince, failure.LatestAttemptError, failure.NextAttemptAt = health.VerificationFailingSince, health.VerificationError, health.NextVerificationAt
		}
		failing = append(failing, failure)
	}
	slices.SortFunc(failing, func(a, b store.FailingDestination) int { return strings.Compare(a.Destination, b.Destination) })
	return failing
}

// destinationHealthRecords includes only the receivers of the candidate route.
// An unavailable credential preserves a previously readable fingerprint.
func (d *Daemon) destinationHealthRecords(route store.AlertRoute) map[string]store.DestinationHealth {
	records := map[string]store.DestinationHealth{}
	for name, target := range route.Known() {
		record := store.DestinationHealth{Target: target, NextVerificationAt: d.started.Add(verifyDestinationsEvery)}
		if health := d.destinationHealth[name]; health != nil && checksandalerts.SameDestination(health.Target, target) {
			record = health.DestinationHealth
			if target.CredentialContentFingerprint != "" {
				record.Target = target
			}
		}
		records[name] = record
	}
	return records
}

// activateDestinationHealth keeps in-flight state for an unchanged receiver.
// The caller holds d.mu and has committed the candidate route.
func (d *Daemon) activateDestinationHealth(records map[string]store.DestinationHealth) {
	for name := range d.destinationHealth {
		if _, exists := records[name]; !exists {
			delete(d.destinationHealth, name)
		}
	}
	for name, record := range records {
		health := d.destinationHealth[name]
		if health == nil || !checksandalerts.SameDestination(health.Target, record.Target) {
			health = &destinationHealth{}
			d.destinationHealth[name] = health
		}
		health.DestinationHealth = record
	}
}

// restoreDestinationHealth reanchors stored waits on this process's clock.
// Starting the daemon does not run a verification.
func (d *Daemon) restoreDestinationHealth() error {
	records, err := d.store.DestinationHealth()
	if err != nil {
		return err
	}
	known := d.route().Known()
	for name, record := range records {
		target, exists := known[name]
		if !exists || !checksandalerts.SameDestination(record.Target, target) {
			delete(records, name)
			continue
		}
		record.VerificationFailingSince = incidents.OnThisClock(record.VerificationFailingSince, d.started)
		record.NextVerificationAt = incidents.OnThisClock(record.NextVerificationAt, d.started)
		records[name] = record
	}
	d.activateDestinationHealth(records)
	return nil
}
