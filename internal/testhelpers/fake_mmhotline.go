package testhelpers

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// alertIDs lists the sender_alert_id of each alert the endpoint accepted.
func (f *FakeMMHotline) AlertIDs() []string {
	alerts, _, _ := f.Received()
	ids := []string{}
	for _, a := range alerts {
		ids = append(ids, fmt.Sprint(a["sender_alert_id"]))
	}
	return ids
}

// TestSigningSecret is the signing secret of the tests' alert source.
var TestSigningSecret = bytes.Repeat([]byte{0x5a}, 32)

// TestSourceID is the tests' alert source ID, a name of the team's own, as
// MMHotline's settings file allows.
const (
	TestSourceID = "mmwatchdog_fleet_hub"
)

// MMHotlineAlert is an alert as MMHotline received it, by its own field names.
type MMHotlineAlert map[string]string

// DecodeMMHotlineAlert reads an alert-intake body as MMHotline does: one JSON
// object of string fields with MMHotline's names, unknown fields refused,
// title and message required, and MMHotline's limits.
func DecodeMMHotlineAlert(body []byte) (MMHotlineAlert, error) {
	var raw map[string]json.RawMessage
	if len(body) > 64*1024 || !utf8.Valid(body) || json.Unmarshal(body, &raw) != nil {
		return nil, fmt.Errorf("body must be bounded UTF-8 JSON")
	}
	a := MMHotlineAlert{}
	for name, value := range raw {
		switch name {
		case "sender_alert_id", "sender_incident_key", "severity", "title", "message", "happened_at":
		default:
			return nil, fmt.Errorf("unknown field %q", name)
		}
		var s string
		if bytes.Equal(value, []byte("null")) || json.Unmarshal(value, &s) != nil {
			return nil, fmt.Errorf("%s must be a string", name)
		}
		a[name] = s
	}
	title, hasTitle := a["title"]
	message, hasMessage := a["message"]
	switch {
	case !hasTitle || !hasMessage:
		return nil, fmt.Errorf("title and message required")
	case strings.TrimSpace(title) == "" || utf8.RuneCountInString(title) > 256:
		return nil, fmt.Errorf("title must contain 1–256 characters")
	case len(message) > 16*1024:
		return nil, fmt.Errorf("message exceeds 16 KiB")
	}
	for _, name := range []string{"sender_alert_id", "sender_incident_key"} {
		if v, ok := a[name]; ok && (v == "" || len(v) > 256) {
			return nil, fmt.Errorf("%s must be 1–256 bytes", name)
		}
	}
	if value, present := a["severity"]; present {
		switch value {
		case "info", "warn", "sos", "resolved-info", "resolved-warn", "resolved-sos":
		default:
			return nil, fmt.Errorf("invalid severity %q", value)
		}
	}
	if v, ok := a["happened_at"]; ok {
		if _, e := time.Parse(time.RFC3339, v); e != nil {
			return nil, fmt.Errorf("happened_at must be RFC 3339")
		}
	}
	return a, nil
}

// DecodeMMHotlineCheckIn reads a check-in body as MMHotline does.
func DecodeMMHotlineCheckIn(body []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var m struct {
		SentAt         *time.Time `json:"sent_at"`
		SentByNodeName string     `json:"sent_by_node_name"`
		Nodes          []struct {
			NodeName         string     `json:"node_name"`
			MaintenanceUntil *time.Time `json:"maintenance_until"`
			Reason           string     `json:"reason"`
			StartedBy        string     `json:"started_by"`
		} `json:"nodes_in_whole_node_maintenance"`
	}
	if len(body) > 64*1024 || decoder.Decode(&m) != nil || m.SentAt == nil || m.SentByNodeName == "" || len(m.Nodes) > 1000 {
		return nil, fmt.Errorf("invalid check-in")
	}
	for _, n := range m.Nodes {
		if n.NodeName == "" || n.MaintenanceUntil == nil || len(n.Reason) > 256 || len(n.StartedBy) > 256 {
			return nil, fmt.Errorf("each node in maintenance needs node_name and maintenance_until")
		}
	}
	var generic map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		return nil, err
	}
	return generic, nil
}

// FakeMMHotline is one MMHotline backend endpoint, with the alert source
// TestSourceID and any that AddAlertSource adds. It verifies each
// request's Mmhotline-Signature with the alert source's signing secret, decodes
// the body as MMHotline does, refuses a body over 64 KiB with an unsigned
// 413, and answers as its mode says, signing receipts over the route,
// status, and request as MMHotline does.
type FakeMMHotline struct {
	*httptest.Server
	Mu sync.Mutex
	// Mode is ok, unsigned, wrongly_signed, other_route (a signed answer to
	// another request), 401, 503, 400, or 429.
	Mode string
	// secrets are the signing secrets of the alert sources, by ID.
	secrets  map[string][]byte
	date     time.Time
	requests int
	alerts   []MMHotlineAlert
	checkIns []map[string]any
	// alertSources and checkInSources name the alert source of each
	// accepted alert and check-in.
	alertSources, checkInSources []string
}

// NewFakeMMHotline starts a fake MMHotline endpoint that answers as mode
// says, until the test ends.
func NewFakeMMHotline(t *testing.T, mode string) *FakeMMHotline {
	t.Helper()
	f := &FakeMMHotline{Mode: mode, secrets: map[string][]byte{TestSourceID: TestSigningSecret}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// AddAlertSource adds an alert source with its signing secret.
func (f *FakeMMHotline) AddAlertSource(id string, secret []byte) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.secrets[id] = bytes.Clone(secret)
}

// ReceivedSources names the alert source of each alert and each check-in
// that Received returns.
func (f *FakeMMHotline) ReceivedSources() (alerts, checkIns []string) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return append([]string{}, f.alertSources...), append([]string{}, f.checkInSources...)
}

// SetMode changes how the endpoint answers.
func (f *FakeMMHotline) SetMode(mode string) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Mode = mode
}

// SetDate sets the Date header of the endpoint's answers.
func (f *FakeMMHotline) SetDate(date time.Time) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.date = date
}

// Received returns the alerts and the check-ins that the endpoint
// accepted, and the number of requests that it got.
func (f *FakeMMHotline) Received() ([]MMHotlineAlert, []map[string]any, int) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return append([]MMHotlineAlert{}, f.alerts...), append([]map[string]any{}, f.checkIns...), f.requests
}

func receiptHeader(secret []byte, route string, status int, requestHMAC string, body []byte) string {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("mmhotline-receipt." + route + "." + strconv.Itoa(status) + "." + requestHMAC + "." + timestamp + "."))
	mac.Write(body)
	return "timestamp_unix_seconds=" + timestamp + ",hmac_sha256_hex=" + hex.EncodeToString(mac.Sum(nil))
}

func (f *FakeMMHotline) serve(w http.ResponseWriter, r *http.Request) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.requests++
	if !f.date.IsZero() {
		w.Header().Set("Date", f.date.UTC().Format(http.TimeFormat))
	}
	body, _ := io.ReadAll(r.Body)
	if len(body) > 64*1024 {
		w.WriteHeader(413)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	route := parts[0]
	var timestamp, requestHMAC string
	if fields := strings.Split(r.Header.Get("Mmhotline-Signature"), ","); len(r.Header.Values("Mmhotline-Signature")) == 1 && len(fields) == 2 {
		timestamp, _ = strings.CutPrefix(fields[0], "timestamp_unix_seconds=")
		requestHMAC, _ = strings.CutPrefix(fields[1], "hmac_sha256_hex=")
	}
	source := ""
	if len(parts) == 2 {
		source = parts[1]
	}
	secret, known := f.secrets[source]
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	seconds, _ := strconv.ParseInt(timestamp, 10, 64)
	verified := known && strconv.FormatInt(seconds, 10) == timestamp && hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(requestHMAC)) && time.Since(time.Unix(seconds, 0)).Abs() < 5*time.Minute
	if !verified || f.Mode == "401" || len(parts) != 2 || (route != "alert-intake" && route != "check-in") || r.Header.Get("Content-Type") != "application/json" {
		w.WriteHeader(401)
		return
	}
	if strings.HasPrefix(f.Mode, "unsigned_") {
		status, _ := strconv.Atoi(strings.TrimPrefix(f.Mode, "unsigned_"))
		w.WriteHeader(status)
		return
	}
	answer := func(status int, v any) {
		b, _ := json.Marshal(v)
		switch f.Mode {
		case "unsigned":
		case "wrongly_signed":
			w.Header().Set("Mmhotline-Receipt-Signature", receiptHeader(bytes.Repeat([]byte{1}, 32), route, status, requestHMAC, b))
		case "other_route":
			w.Header().Set("Mmhotline-Receipt-Signature", receiptHeader(secret, "check-in", status, requestHMAC, b))
		default:
			w.Header().Set("Mmhotline-Receipt-Signature", receiptHeader(secret, route, status, requestHMAC, b))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(b) // A failed response write is detected by the requesting client; the fake cannot repair a disconnected response.
	}
	var alert MMHotlineAlert
	var checkIn map[string]any
	var e error
	if route == "alert-intake" {
		alert, e = DecodeMMHotlineAlert(body)
	} else {
		checkIn, e = DecodeMMHotlineCheckIn(body)
	}
	switch {
	case e != nil || f.Mode == "400" || f.Mode == "other_route":
		answer(400, map[string]string{"error": fmt.Sprint("title must contain 1–256 characters ", e)})
	case f.Mode == "429":
		w.Header().Set("Retry-After", "7")
		answer(429, map[string]string{"error": "admission limit reached; retry later"})
	case f.Mode == "missing_receipt":
		answer(200, map[string]string{})
	case f.Mode == "201":
		answer(201, map[string]string{"mmhotline_alert_id": "al_other"})
	case f.Mode == "503":
		w.WriteHeader(503)
	case route == "alert-intake":
		if f.Mode == "ok" {
			f.alerts = append(f.alerts, alert)
			f.alertSources = append(f.alertSources, source)
		}
		answer(200, map[string]any{"mmhotline_alert_id": "al_" + alert["sender_alert_id"], "already_received": false})
	default:
		if f.Mode == "ok" {
			f.checkIns = append(f.checkIns, checkIn)
			f.checkInSources = append(f.checkInSources, source)
		}
		answer(200, map[string]any{"check_in_received_at": time.Now().UTC().Format(time.RFC3339)})
	}
}

// WriteSigningSecret writes TestSigningSecret into a file, as
// the mmhotline_signing_secret_file of a mmhotline destination, and returns
// its path.
func WriteSigningSecret(t *testing.T) string {
	t.Helper()
	return WriteToken(t, hex.EncodeToString(TestSigningSecret)+"\n")
}

// WriteToken writes value into a file of the test's own, with the mode
// that a credential file needs, and returns its path.
func WriteToken(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if e := os.WriteFile(path, []byte(value), 0600); e != nil {
		t.Fatal(e)
	}
	return path
}

// SetSigningSecret rotates the signing secret of TestSourceID for a reload
// test.
func (f *FakeMMHotline) SetSigningSecret(secret []byte) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.secrets[TestSourceID] = bytes.Clone(secret)
}
