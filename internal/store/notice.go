package store

import (
	"database/sql"

	"mmwatchdog/internal/checksandalerts"
)

// A notice is a one-time message that a person or another program sends
// through MMWatchdog's destinations with mmwatchdog send-notice: a test of the destinations,
// or an event such as a failover. It is an alert event of the type notice,
// with no check group, no incident, and no recovery. The rules are in
// docs/changing-mmwatchdog/design/alerts.md under "Send a notice".

// NoticeBodyLimit is the most bytes the body of a notice has. The hub takes
// an alert of at most AlertLimit encoded bytes, and JSON encoding can make a
// text six times as long.
const NoticeBodyLimit = 8 * 1024

// NoticeRequest is what mmwatchdog send-notice asks the daemon to send. Title is the
// text without the node's name, which MMWatchdog puts in front. Destination is
// empty, or the one destination that gets the notice whatever its
// alert_severities say.
type NoticeRequest struct {
	AlertSeverity string `json:"alert_severity"`
	Title         string `json:"title"`
	Body          string `json:"body"`
	Destination   string `json:"destination"`
}

// The results of a notice for one destination.
const (
	// NoticeDelivered: the destination took the notice at the first attempt.
	NoticeDelivered = "delivered"
	// NoticeFailed: the first attempt failed. MMWatchdog keeps trying.
	NoticeFailed = "failed"
	// NoticeWaitsBehindEarlierAlerts: the destination has earlier alerts
	// that MMWatchdog could not deliver, and a destination gets its alerts in order.
	NoticeWaitsBehindEarlierAlerts = "waits_behind_earlier_alerts"
	// NoticeNotSent: the notice left the outbox without an attempt, because
	// its destination was removed or changed, or the outbox was full.
	NoticeNotSent = "not_sent"
)

// NoticeDelivery is what became of a notice for one destination: the result
// of the first delivery attempt, or why there was none yet. On a node that
// has joined a hub, the one destination is the hub.
type NoticeDelivery struct {
	Destination string `json:"destination"`
	Result      string `json:"result"`
	// Error is why the first attempt failed, or the error of the latest
	// attempt for the earlier alerts that the notice waits behind.
	Error                    string `json:"error,omitempty"`
	EarlierUndeliveredAlerts int    `json:"earlier_undelivered_alerts,omitempty"`
}

// NoticeResult is the daemon's answer to mmwatchdog send-notice.
type NoticeResult struct {
	NoticeID string `json:"notice_id"`
	// Title is the title as MMWatchdog sends it, with the node's name in front.
	Title string `json:"title"`
	// SentThroughFleetHubAt is the address of the hub that delivers the
	// notice, on a node that has joined a hub.
	SentThroughFleetHubAt string `json:"sent_through_fleet_hub_at,omitempty"`
	// SendAlertWithoutHubAfterWaitingFor is, on such a node, how long it
	// waits for the hub before it sends an alert to the destinations itself;
	// empty before the node has the fleet settings.
	SendAlertWithoutHubAfterWaitingFor checksandalerts.Duration `json:"send_alert_without_hub_after_waiting_for,omitempty"`
	Deliveries                         []NoticeDelivery         `json:"deliveries"`
}

// NoticeQueue is where an undelivered notice stands in the outbox for one
// destination.
type noticeQueue struct {
	// WaitsForFleetAlertGrouping: on the hub, the fleet alert grouping step
	// has not queued the notice for the destinations yet.
	WaitsForFleetAlertGrouping bool
	Queued                     bool
	// Earlier counts the alerts queued for the destination before the
	// notice. oldestAttempts and oldestError are the failed attempts and the
	// latest error of the oldest of them.
	Earlier        int
	OldestAttempts int
	OldestError    string
}

func (s *Store) NoticeQueue(id, destination string) (noticeQueue, error) {
	var q noticeQueue
	var grouping int
	if e := s.DB.QueryRow("SELECT COUNT(*) FROM intake WHERE node=? AND id=? AND state!=?", s.Node, id, intakeDone).Scan(&grouping); e != nil {
		return q, e
	}
	if grouping > 0 {
		q.WaitsForFleetAlertGrouping = true
		return q, nil
	}
	var seq sql.NullInt64
	if e := s.DB.QueryRow("SELECT MIN(seq) FROM outbox WHERE destination=? AND json_extract(event,'$.id')=?", destination, id).Scan(&seq); e != nil || !seq.Valid {
		return q, e
	}
	q.Queued = true
	if e := s.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE destination=? AND seq<?", destination, seq.Int64).Scan(&q.Earlier); e != nil || q.Earlier == 0 {
		return q, e
	}
	e := s.DB.QueryRow("SELECT attempts, error FROM outbox WHERE destination=? ORDER BY seq LIMIT 1", destination).Scan(&q.OldestAttempts, &q.OldestError)
	return q, e
}
