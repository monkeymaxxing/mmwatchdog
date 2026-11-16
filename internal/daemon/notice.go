package daemon

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/store"
)

// sendNotice is the entry point of mmwatchdog send-notice: it queues the notice and
// answers once the first delivery attempt to each destination has a result.
func (d *Daemon) sendNotice(w http.ResponseWriter, r *http.Request) {
	var req store.NoticeRequest
	b, e := primitives.ReadBody(r.Body, fleet.AlertLimit)
	if e == nil {
		e = checksandalerts.DecodeJSONUpTo(b, &req, fleet.AlertLimit)
	}
	if e != nil {
		apiError(w, e, 400)
		return
	}
	// A delivery attempt takes as long as its destination's time limit
	// allows. Start the response write deadline only when the results are
	// ready.
	response := http.NewResponseController(w)
	if e = response.SetWriteDeadline(time.Time{}); e != nil {
		apiError(w, e, 500)
		return
	}
	result, destinations, status, e := d.queueNotice(req, d.now())
	if e == nil {
		result.Deliveries = d.awaitNotice(r.Context(), result.NoticeID, destinations)
	}
	if err := response.SetWriteDeadline(time.Now().Add(responseWriteTimeout)); err != nil {
		apiError(w, err, 500)
		return
	}
	if e != nil {
		apiError(w, e, status)
		return
	}
	writeJSON(w, result)
}

// queueNotice checks a notice and commits it with its deliveries. It returns
// the destinations whose first delivery attempt the command waits for: the
// hub on a node that has joined one, otherwise every destination the notice
// goes to (GoesTo). A notice that cannot be committed is not sent.
func (d *Daemon) queueNotice(req store.NoticeRequest, now time.Time) (store.NoticeResult, []string, int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fail := func(status int, format string, args ...any) (store.NoticeResult, []string, int, error) {
		return store.NoticeResult{}, nil, status, fmt.Errorf(format, args...)
	}
	if config.SeverityRank(req.AlertSeverity) < 0 {
		return fail(400, "a notice requires --alert-severity: info, warn, or sos")
	}
	// A title is one line. MMWatchdog never cuts a title that a person typed: the
	// person is there to shorten it.
	if strings.TrimSpace(req.Title) == "" || !checksandalerts.PlainText(req.Title, len(req.Title)) {
		return fail(400, "a notice requires --title with one line of text")
	}
	node := d.store.Node
	if n := utf8.RuneCountInString(node + ": " + req.Title); n > checksandalerts.AlertTitleLimit {
		by := fmt.Sprintf("%d characters", n-checksandalerts.AlertTitleLimit)
		if n-checksandalerts.AlertTitleLimit == 1 {
			by = "1 character"
		}
		return fail(400, "the title would have %d characters with the node's name in front (%q), and a title has at most %d: shorten --title by %s", n, node+": ", checksandalerts.AlertTitleLimit, by)
	}
	if len(req.Body) > store.NoticeBodyLimit || !utf8.ValidString(req.Body) {
		return fail(400, "--body has %d bytes; the body of a notice is UTF-8 text of at most %d bytes", len(req.Body), store.NoticeBodyLimit)
	}
	route := d.route()
	event := checksandalerts.Event{ID: incidents.ID(), Type: "notice", AlertSeverity: req.AlertSeverity, At: now, Message: req.Title, Body: req.Body, Notify: true}
	if req.Destination != "" {
		if route.Joined && d.fleetSettings == nil {
			return fail(409, "this node does not have the settings of its MMWatchdog fleet hub yet, so it does not know the hub's destinations and cannot check the name %q; send the notice without --destination, or try again when mmwatchdog status no longer says \"no settings from the hub yet\"", req.Destination)
		}
		if _, exists := route.Destinations[req.Destination]; !exists {
			whose := "alert_destinations.yml has"
			if route.Joined {
				whose = "the MMWatchdog fleet hub has"
			}
			if len(route.Destinations) == 0 {
				return fail(404, "%s no destination named %q: it has no destination at all", whose, req.Destination)
			}
			return fail(404, "%s no destination named %q; its destinations are: %s", whose, req.Destination, strings.Join(checksandalerts.Names(route.Destinations), ", "))
		}
		// The name travels with the notice, so the hub delivers a joined
		// node's notice to that destination too.
		event.SendAlertsOnlyToDestinations = &[]string{req.Destination}
	}
	result := store.NoticeResult{NoticeID: event.ID, Title: node + ": " + req.Title}
	destinations := []string{}
	if route.Joined {
		destinations = []string{config.HubDestination}
		result.SentThroughFleetHubAt = d.joinCode.Address
		if d.fleetSettings != nil {
			result.SendAlertWithoutHubAfterWaitingFor = d.fleetSettings.Fleet.SendAlertWithoutHubAfterWaitingFor
		}
	} else {
		for _, name := range checksandalerts.Names(route.Destinations) {
			if config.GoesTo(name, route.Destinations[name], event, route.Destinations) {
				destinations = append(destinations, name)
			}
		}
	}
	// The delivery worker fills this in from now on; see noticeAttempted.
	d.noticeAttempts[event.ID] = map[string]error{}
	if e := d.control(d.snapshot.Clone(), []checksandalerts.Event{event}, route); e != nil {
		delete(d.noticeAttempts, event.ID)
		return fail(500, "the notice could not be saved, so it is not sent: %v", e)
	}
	return result, destinations, 0, nil
}

// noticeAttempted keeps the result of the first delivery attempt of a notice
// that mmwatchdog send-notice waits for. The delivery worker calls it with the
// bookkeeping of each attempt. The caller holds d.mu.
func (d *Daemon) noticeAttempted(delivery store.Delivery, err error) {
	attempts, waiting := d.noticeAttempts[delivery.Event.ID]
	if !waiting {
		return
	}
	if _, attempted := attempts[delivery.Destination]; !attempted {
		attempts[delivery.Destination] = err
	}
}

// awaitNotice waits until the notice has a result for each of destinations,
// and returns the results in that order. When the command goes away first,
// the notice stays queued and MMWatchdog delivers it like every alert.
func (d *Daemon) awaitNotice(ctx context.Context, id string, destinations []string) []store.NoticeDelivery {
	defer func() {
		d.mu.Lock()
		delete(d.noticeAttempts, id)
		d.mu.Unlock()
	}()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	results := map[string]store.NoticeDelivery{}
	for {
		d.mu.Lock()
		for _, name := range destinations {
			if _, settled := results[name]; settled {
				continue
			}
			if delivery, settled := d.noticeDelivery(id, name); settled {
				results[name] = delivery
			}
		}
		d.mu.Unlock()
		if len(results) == len(destinations) {
			break
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
	deliveries := []store.NoticeDelivery{}
	for _, name := range destinations {
		deliveries = append(deliveries, results[name])
	}
	return deliveries
}

// noticeDelivery is what became of a notice for one destination, and whether
// that is settled: the first attempt has a result, or it cannot happen now.
// Each attempt is bounded by its destination's time limit, so every wait
// ends. The caller holds d.mu.
func (d *Daemon) noticeDelivery(id, destination string) (store.NoticeDelivery, bool) {
	delivery := store.NoticeDelivery{Destination: destination}
	if err, attempted := d.noticeAttempts[id][destination]; attempted {
		delivery.Result = store.NoticeDelivered
		if err != nil {
			delivery.Result, delivery.Error = store.NoticeFailed, err.Error()
		}
		return delivery, true
	}
	// A delivery to the destination is in flight: the notice itself, or an
	// earlier alert whose result decides whether the notice is next.
	if d.deliveries[destination] {
		return delivery, false
	}
	queue, e := d.store.NoticeQueue(id, destination)
	switch {
	case e != nil:
		delivery.Result, delivery.Error = store.NoticeNotSent, "MMWatchdog could not read its queue of undelivered alerts: "+e.Error()
		return delivery, true
	case queue.WaitsForFleetAlertGrouping:
		return delivery, false
	case !queue.Queued:
		delivery.Result = store.NoticeNotSent
		return delivery, true
	case queue.Earlier > 0 && queue.OldestAttempts > 0:
		// The destination is failing: its oldest alert has been tried and
		// waits for its next attempt, which can be minutes away.
		delivery.Result, delivery.Error, delivery.EarlierUndeliveredAlerts = store.NoticeWaitsBehindEarlierAlerts, queue.OldestError, queue.Earlier
		return delivery, true
	}
	return delivery, false
}
