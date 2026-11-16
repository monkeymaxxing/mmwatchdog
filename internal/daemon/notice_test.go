package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/store"
	"mmwatchdog/internal/testhelpers"
)

// sendNotice sends req to the daemon as mmwatchdog send-notice does, and returns the
// daemon's answer: the result, or the HTTP status and the error.
func sendNotice(t *testing.T, d *Daemon, req store.NoticeRequest) (store.NoticeResult, int, string) {
	t.Helper()
	api := httptest.NewServer(d.routes())
	defer api.Close()
	body, _ := json.Marshal(req)
	client := api.Client()
	client.Timeout = 10 * time.Second
	response, err := client.Post(api.URL+"/send-notice", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		var failure struct {
			Error string `json:"error"`
		}
		json.NewDecoder(response.Body).Decode(&failure)
		return store.NoticeResult{}, response.StatusCode, failure.Error
	}
	var result store.NoticeResult
	if err = json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result, response.StatusCode, ""
}

// noticeDaemon starts a standalone daemon with these destinations. A test
// that delivers starts its delivery worker with runWorker.
func noticeDaemon(t *testing.T, destinations map[string]checksandalerts.Destination) *Daemon {
	t.Helper()
	p, c := daemonConfig(t)
	c.Alerts.Destinations = destinations
	writeConfig(t, p, c)
	d, err := NewDaemon(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func delivered(names ...string) []store.NoticeDelivery {
	deliveries := []store.NoticeDelivery{}
	for _, name := range names {
		deliveries = append(deliveries, store.NoticeDelivery{Destination: name, Result: store.NoticeDelivered})
	}
	return deliveries
}

// A notice goes the same way as a real alert: to every destination that
// takes its severity, with the node's name in front of its title and the
// body its sender gave. The command gets the result of the first delivery
// attempt to each destination. With one destination named, the notice goes
// to that destination whatever its alert_severities say.
func TestANoticeGoesToEveryDestinationThatTakesItsSeverity(t *testing.T) {
	t.Parallel()
	team, pager, mail := captureAlerts(t), captureAlerts(t), captureAlerts(t)
	d := noticeDaemon(t, map[string]checksandalerts.Destination{
		"team_chat":               {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: team.URL}, TimeLimitPerDeliveryAttempt: "1m"},
		"on_call_pager":           {RemindEvery: "never", AlertSeverities: []string{"sos"}, Inputs: &checksandalerts.Webhook{URL: pager.URL}, TimeLimitPerDeliveryAttempt: "1m"},
		"mail_administrator_chat": {RemindEvery: "never", AlertSeverities: []string{}, Inputs: &checksandalerts.Webhook{URL: mail.URL}, TimeLimitPerDeliveryAttempt: "1m"},
	})
	runWorker(t, d)
	const text = "Test of the alert destinations. Nothing is wrong."
	result, status, problem := sendNotice(t, d, store.NoticeRequest{AlertSeverity: "warn", Title: text, Body: "Sent by the deploy script.\nSecond line."})
	if status != 200 || result.Title != d.store.Node+": "+text || result.SentThroughFleetHubAt != "" || !reflect.DeepEqual(result.Deliveries, delivered("team_chat")) {
		t.Fatalf("%d %q %+v", status, problem, result)
	}
	got := team.wait(t, 1)[0]
	if got.ID != result.NoticeID || got.Type != "notice" || got.AlertSeverity != "warn" || got.Node != d.store.Node || got.Title != result.Title || got.TitleWasShortened ||
		got.Body != "Sent by the deploy script.\nSecond line." || got.Message != text || got.CheckGroup != "" || got.Check != "" || got.Incident != "" || got.SendAlertsOnlyToDestinations != nil {
		t.Fatalf("%+v", got)
	}
	// An sos notice also reaches the pager. Without --body there is no body.
	result, _, problem = sendNotice(t, d, store.NoticeRequest{AlertSeverity: "sos", Title: text})
	if !reflect.DeepEqual(result.Deliveries, delivered("on_call_pager", "team_chat")) {
		t.Fatalf("%q %+v", problem, result)
	}
	if got = pager.wait(t, 1)[0]; got.Type != "notice" || got.AlertSeverity != "sos" || got.Body != "" || got.Title != result.Title {
		t.Fatalf("%+v", got)
	}
	// One destination by name, though it takes no severity.
	result, _, problem = sendNotice(t, d, store.NoticeRequest{AlertSeverity: "info", Title: text, Destination: "mail_administrator_chat"})
	if !reflect.DeepEqual(result.Deliveries, delivered("mail_administrator_chat")) {
		t.Fatalf("%q %+v", problem, result)
	}
	if got = mail.wait(t, 1)[0]; got.SendAlertsOnlyToDestinations == nil || !reflect.DeepEqual(*got.SendAlertsOnlyToDestinations, []string{"mail_administrator_chat"}) {
		t.Fatalf("%+v", got)
	}
	if len(team.received()) != 2 || len(pager.received()) != 1 || len(mail.received()) != 1 {
		t.Fatal("a notice reached a destination that does not take it", team.received(), pager.received(), mail.received())
	}
	// Every notice is in the event history with its title, and none waits.
	events, err := d.store.Events(10)
	if err != nil || len(events) != 3 || events[0].Type != "notice" || events[0].Title != result.Title || len(queuedEvents(t, d)) != 0 || len(d.noticeAttempts) != 0 {
		t.Fatal(events, err, queuedEvents(t, d), d.noticeAttempts)
	}
}

// A notice that no destination takes is recorded in the event history, and
// the command learns that it went to nobody.
func TestANoticeThatNoDestinationTakesIsRecorded(t *testing.T) {
	t.Parallel()
	pager := captureAlerts(t)
	for name, destinations := range map[string]map[string]checksandalerts.Destination{
		"only the pager": {"on_call_pager": {AlertSeverities: []string{"sos"}, Inputs: &checksandalerts.Webhook{URL: pager.URL}}},
		"no destination": {},
	} {
		d := noticeDaemon(t, destinations)
		result, status, problem := sendNotice(t, d, store.NoticeRequest{AlertSeverity: "info", Title: "backup finished"})
		if status != 200 || len(result.Deliveries) != 0 || result.Title != d.store.Node+": backup finished" {
			t.Fatalf("%s: %d %q %+v", name, status, problem, result)
		}
		events, err := d.store.Events(10)
		if err != nil || len(events) != 1 || events[0].Type != "notice" || events[0].AlertSeverity != "info" || events[0].Title != result.Title || len(queuedEvents(t, d)) != 0 {
			t.Fatal(name, events, err)
		}
	}
	if len(pager.received()) != 0 {
		t.Fatal(pager.received())
	}
}

// The daemon refuses a notice it cannot send as asked, and then records and
// queues nothing. A title is never cut: the error states its length.
func TestSendNoticeRefusals(t *testing.T) {
	t.Parallel()
	d := noticeDaemon(t, map[string]checksandalerts.Destination{"team_chat": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: "https://example.com/team"}}})
	node := d.store.Node
	fits := strings.Repeat("t", checksandalerts.AlertTitleLimit-len(node+": "))
	for _, tc := range []struct {
		req    store.NoticeRequest
		status int
		want   string
	}{
		{store.NoticeRequest{Title: "x"}, 400, "a notice requires --alert-severity: info, warn, or sos"},
		{store.NoticeRequest{AlertSeverity: "loud", Title: "x"}, 400, "a notice requires --alert-severity: info, warn, or sos"},
		{store.NoticeRequest{AlertSeverity: "sos"}, 400, "a notice requires --title with one line of text"},
		{store.NoticeRequest{AlertSeverity: "sos", Title: "  "}, 400, "a notice requires --title with one line of text"},
		{store.NoticeRequest{AlertSeverity: "sos", Title: "two\nlines"}, 400, "a notice requires --title with one line of text"},
		{store.NoticeRequest{AlertSeverity: "sos", Title: fits + "123"}, 400, fmt.Sprintf(`the title would have 123 characters with the node's name in front (%q), and a title has at most 120: shorten --title by 3 characters`, node+": ")},
		{store.NoticeRequest{AlertSeverity: "sos", Title: "x", Body: strings.Repeat("b", store.NoticeBodyLimit+1)}, 400, "--body has 8193 bytes; the body of a notice is UTF-8 text of at most 8192 bytes"},
		{store.NoticeRequest{AlertSeverity: "sos", Title: "x", Destination: "pager"}, 404, `alert_destinations.yml has no destination named "pager"; its destinations are: team_chat`},
	} {
		if _, status, problem := sendNotice(t, d, tc.req); status != tc.status || problem != tc.want {
			t.Errorf("%+v: %d %q, want %d %q", tc.req, status, problem, tc.status, tc.want)
		}
	}
	events, err := d.store.Events(10)
	if err != nil || len(events) != 0 || len(queuedEvents(t, d)) != 0 || len(d.noticeAttempts) != 0 {
		t.Fatal("a refused notice was recorded or queued", events, err)
	}
	// A title of exactly 120 characters and a body of exactly the limit fit.
	if _, _, _, err = d.queueNotice(store.NoticeRequest{AlertSeverity: "info", Title: fits, Body: strings.Repeat("<", store.NoticeBodyLimit)}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// However its text encodes, the largest notice stays within what a hub takes.
	queued := queuedEvents(t, d)
	if b, _ := json.Marshal(queued[0]); len(queued) != 1 || len([]rune(queued[0].Title)) != checksandalerts.AlertTitleLimit || len(b) > fleet.AlertLimit {
		t.Fatal(len(queued), len(b))
	}
}

// A first attempt that fails is reported with its error, and the notice
// stays queued like every alert. A notice behind earlier alerts that the
// destination has not taken is reported at once, with their count and the
// latest error, because a destination gets its alerts in order.
func TestSendNoticeReportsAFailedFirstAttempt(t *testing.T) {
	t.Parallel()
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer refusing.Close()
	team := captureAlerts(t)
	d := noticeDaemon(t, map[string]checksandalerts.Destination{
		"on_call_pager": {RemindEvery: "never", AlertSeverities: []string{"sos"}, Inputs: &checksandalerts.Webhook{URL: refusing.URL}, TimeLimitPerDeliveryAttempt: "1m"},
		"team_chat":     {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: team.URL}, TimeLimitPerDeliveryAttempt: "1m"},
	})
	runWorker(t, d)
	result, status, problem := sendNotice(t, d, store.NoticeRequest{AlertSeverity: "sos", Title: "first"})
	want := []store.NoticeDelivery{{Destination: "on_call_pager", Result: store.NoticeFailed, Error: "webhook returned HTTP 503"}, {Destination: "team_chat", Result: store.NoticeDelivered}}
	if status != 200 || !reflect.DeepEqual(result.Deliveries, want) {
		t.Fatalf("%d %q %+v", status, problem, result)
	}
	if outboxCount(t, d, "on_call_pager") != 1 || outboxCount(t, d, "team_chat") != 0 {
		t.Fatal("the failed notice is not queued for its next attempt")
	}
	result, _, problem = sendNotice(t, d, store.NoticeRequest{AlertSeverity: "sos", Title: "second"})
	want = []store.NoticeDelivery{{Destination: "on_call_pager", Result: store.NoticeWaitsBehindEarlierAlerts, Error: "webhook returned HTTP 503", EarlierUndeliveredAlerts: 1}, {Destination: "team_chat", Result: store.NoticeDelivered}}
	if !reflect.DeepEqual(result.Deliveries, want) {
		t.Fatalf("%q %+v", problem, result)
	}
	if outboxCount(t, d, "on_call_pager") != 2 {
		t.Fatal("the second notice is not queued behind the first")
	}
}

// The hub sends its own notice through the fleet alert grouping step, and
// the command still gets the result for each destination. Notices are never
// combined: each goes out at once and alone, also within the grouping
// window, and a late one gets no line about a check.
func TestANoticeOnTheHubIsNeverCombined(t *testing.T) {
	t.Parallel()
	dest := captureAlerts(t)
	hub, _, _ := startHubWith(t, map[string]checksandalerts.Destination{"ops": {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: dest.URL}, TimeLimitPerDeliveryAttempt: "1m"}})
	runWorker(t, hub)
	for _, text := range []string{"first", "second"} {
		result, status, problem := sendNotice(t, hub, store.NoticeRequest{AlertSeverity: "warn", Title: text})
		if status != 200 || !reflect.DeepEqual(result.Deliveries, delivered("ops")) || result.Title != hub.store.Node+": "+text {
			t.Fatalf("%s: %d %q %+v", text, status, problem, result)
		}
	}
	got := dest.wait(t, 2)
	if got[0].Title != hub.store.Node+": first" || got[1].Title != hub.store.Node+": second" || got[0].NodeCount != 0 || got[1].NodeCount != 0 {
		t.Fatalf("%+v", got)
	}
	// Two nodes send the same words at the same time: two messages. The
	// second is delivered late and says nothing about the state of a check.
	now := time.Now().UTC()
	for _, node := range []string{"app-1", "app-2"} {
		acceptAlert(t, hub, node, checksandalerts.Event{ID: incidents.ID(), Type: "notice", AlertSeverity: "warn", At: now.Add(-10 * time.Minute), Message: "deploy finished", Title: node + ": deploy finished", Body: "version 1.4"})
	}
	got = dest.wait(t, 4)[2:]
	if got[0].Node != "app-1" || got[1].Node != "app-2" || got[1].Title != "app-2: deploy finished" || got[1].Body != "version 1.4" || got[1].Message != "deploy finished" || got[1].NodeCount != 0 {
		t.Fatalf("%+v", got)
	}
}

// On a node that has joined a hub, the notice goes through the hub like
// every alert, and the command learns whether the hub accepted it. A named
// destination travels with the notice, and the hub delivers to it whatever
// its alert_severities say. A name the hub does not have is refused.
func TestANoticeFromAJoinedNodeGoesThroughTheHub(t *testing.T) {
	t.Parallel()
	team, mail := captureAlerts(t), captureAlerts(t)
	hub, code, server := startHubWith(t, map[string]checksandalerts.Destination{
		"team_chat":               {RemindEvery: "never", AlertSeverities: testhelpers.AllSeverities, Inputs: &checksandalerts.Webhook{URL: team.URL}, TimeLimitPerDeliveryAttempt: "1m"},
		"mail_administrator_chat": {RemindEvery: "never", AlertSeverities: []string{}, Inputs: &checksandalerts.Webhook{URL: mail.URL}, TimeLimitPerDeliveryAttempt: "1m"},
	})
	node := joinedNode(t, code, "app-1")
	// Before the node holds the fleet settings it cannot check a name.
	if _, status, problem := sendNotice(t, node, store.NoticeRequest{AlertSeverity: "info", Title: "x", Destination: "team_chat"}); status != 409 || !strings.Contains(problem, "this node does not have the settings of its MMWatchdog fleet hub yet, so it does not know the hub's destinations and cannot check the name \"team_chat\"") {
		t.Fatal(status, problem)
	}
	node.report(context.Background())
	runWorker(t, node)
	runWorker(t, hub)
	result, status, problem := sendNotice(t, node, store.NoticeRequest{AlertSeverity: "warn", Title: "load balancer: this machine is now the active one"})
	if status != 200 || result.SentThroughFleetHubAt != code.Address || result.SendAlertWithoutHubAfterWaitingFor != node.fleetSettings.Fleet.SendAlertWithoutHubAfterWaitingFor || !reflect.DeepEqual(result.Deliveries, delivered(config.HubDestination)) {
		t.Fatalf("%d %q %+v", status, problem, result)
	}
	if got := team.wait(t, 1)[0]; got.Type != "notice" || got.Node != "app-1" || got.Title != "app-1: load balancer: this machine is now the active one" || got.AlertSeverity != "warn" {
		t.Fatalf("%+v", got)
	}
	result, _, problem = sendNotice(t, node, store.NoticeRequest{AlertSeverity: "info", Title: "test", Destination: "mail_administrator_chat"})
	if !reflect.DeepEqual(result.Deliveries, delivered(config.HubDestination)) {
		t.Fatalf("%q %+v", problem, result)
	}
	if got := mail.wait(t, 1)[0]; got.Title != "app-1: test" || got.Node != "app-1" {
		t.Fatalf("%+v", got)
	}
	if _, status, problem = sendNotice(t, node, store.NoticeRequest{AlertSeverity: "info", Title: "test", Destination: "pager"}); status != 404 || problem != `the MMWatchdog fleet hub has no destination named "pager"; its destinations are: mail_administrator_chat, team_chat` {
		t.Fatal(status, problem)
	}
	if len(team.received()) != 1 || len(mail.received()) != 1 {
		t.Fatal(team.received(), mail.received())
	}
	// The hub does not answer: the first attempt fails, and the notice
	// stays queued for the hub.
	server.Close()
	result, _, problem = sendNotice(t, node, store.NoticeRequest{AlertSeverity: "warn", Title: "while the hub is away"})
	if len(result.Deliveries) != 1 || result.Deliveries[0].Destination != config.HubDestination || result.Deliveries[0].Result != store.NoticeFailed || result.Deliveries[0].Error == "" || outboxCount(t, node, config.HubDestination) != 1 {
		t.Fatalf("%q %+v", problem, result)
	}
}
