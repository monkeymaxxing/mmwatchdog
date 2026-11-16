package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/incidents"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/store"
)

// Reporter is the reporting goroutine's own state; only that goroutine, or a
// test standing in for it, uses it. Only the credential is durable.
type Reporter struct {
	Credential *HubCredential
	After      int64 // the hub's event cursor; -1 until a reply gives it
	// ReadingsAfter is the sequence number of the last reading that the hub
	// holds from this node; -1 until a reply gives it.
	ReadingsAfter        int64
	Seq                  int64
	Sampler              checksandalerts.CPUSampler
	FleetSettingsVersion string // the version of the fleet settings in its last reply
	OutageAlerter        bool   // the last reply named this node a hub-outage alerter
	// IncidentsKnownToTheHub are the incidents sent without the hub that
	// the last reply says the hub holds.
	IncidentsKnownToTheHub []store.IncidentKnownToTheHub
}

// HubCredential is the credential a hub issued, with the hub fingerprint and
// node name it was issued for.
type HubCredential struct {
	Hub        string `json:"hub"`
	Node       string `json:"node"`
	Credential string `json:"credential"`
}

// HubLink is a joined node's connection to its hub, shown by status.
type HubLink struct {
	Address    string    `json:"hub_address"`
	Node       string    `json:"this_node_name"`
	LastReport time.Time `json:"last_accepted_report_at,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
	ErrorAt    time.Time `json:"last_error_at,omitempty"`
	// FleetSettingsVersion is the version of the fleet settings this node holds, or
	// empty before it has them.
	FleetSettingsVersion string `json:"fleet_settings_version,omitempty"`
}

const CredentialFile = "hub-credential.json"

// Each incident confirmation copies only the destination and incident ID
// from a record in a report, so all confirmations fit within ReportLimit.
// Reserve ReportSpareBytes for the reply's cursors, version and envelope.
const reportReplyLimit = ReportLimit + ReportSpareBytes

// ReadingsRoom is how many bytes of readings a report may carry whose other
// content takes encodedBytes, when the node reports every every. It is
// ReportReadingsBytes for each reportReadingsEvery between two reports, so
// that a node sends the same amount in the same time however often it
// reports, and at least ReportReadingsFloor. It is never more than the
// request size limit leaves, so that readings cannot make the hub refuse a
// report, and zero or less for a report that has no room.
func ReadingsRoom(encodedBytes int, every time.Duration) int {
	budget := int64(every/time.Millisecond) * ReportReadingsBytes / int64(reportReadingsEvery/time.Millisecond)
	return int(min(max(budget, ReportReadingsFloor), int64(ReportLimit-encodedBytes-ReportSpareBytes)))
}

// ReportedCheckOf is what a report says about one check, or about one failed
// unit of an every_systemd_unit check.
func ReportedCheckOf(st incidents.CheckStatus) ReportedCheck {
	rc := ReportedCheck{State: st.State, Message: primitives.Clip(st.Explanation(), 1024), SendAlertsOnlyToDestinations: st.SendAlertsOnlyToDestinations}
	// The hub's view of the node says when MMWatchdog holds back the alerts of a
	// check that fails while a check that it names as its cause fails.
	if _, note := st.KeptQuietNote(); note != "" {
		rc.Message = primitives.Clip(st.Explanation(), 1024-len("; "+note)) + "; " + note
	}
	// Evidence is never changed once captured, so it can be encoded
	// after the lock is released.
	if st.FailureIncident.Incident != "" {
		t := st.FailureIncident
		rc.FailureIncident = &t
	}
	if st.CannotCheckIncident.Incident != "" {
		t := st.CannotCheckIncident
		rc.CannotCheckIncident = &t
	}
	return rc
}

// NodeResourceMetrics measures node CPU, memory, and root-disk use for a report. The
// CPU sample covers the time since the previous report.
func NodeResourceMetrics(ctx context.Context, sampler *checksandalerts.CPUSampler) map[string]checksandalerts.Measurement {
	// A number that cannot be read is left out of the report.
	metrics := map[string]checksandalerts.Measurement{}
	if runtime.GOOS != "linux" {
		return metrics
	}
	if used, _, problem := sampler.Measure(ctx, checksandalerts.ReadCPUStat, false); problem == "" {
		metrics["cpu"] = checksandalerts.Measurement{Value: used, Unit: "percent"}
	}
	if used, err := checksandalerts.ReadMemoryInformation(checksandalerts.MemoryPercent); err == nil {
		metrics["memory"] = checksandalerts.Measurement{Value: used, Unit: "percent"}
	}
	if counts, err := primitives.FilesystemStatistics(ctx, "/"); err == nil {
		if used, known := counts.SpaceUsedPercent(); known {
			metrics["disk"] = checksandalerts.Measurement{Value: used, Unit: "percent"}
		}
	}
	return metrics
}

func JoinHub(ctx context.Context, client *http.Client, code JoinCode, name string, settings config.NodeFleetSettings) (string, error) {
	status, body, e := Post(ctx, client, "https://"+code.Address+"/v1/join", "", JoinRequest{name, code.Secret, settings})
	if e != nil {
		return "", e
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("joining the hub: %w", HubError(status, body))
	}
	var reply struct {
		Credential string `json:"credential"`
	}
	if e = json.Unmarshal(body, &reply); e != nil || !tokenPattern.MatchString(reply.Credential) {
		return "", fmt.Errorf("the hub sent an invalid join reply")
	}
	return reply.Credential, nil
}

// PinnedClient trusts only the hub certificate the join code names; no
// certificate authority is involved.
func PinnedClient(fingerprint string) *http.Client {
	tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) > 0 {
				sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
				if hex.EncodeToString(sum[:]) == fingerprint {
					return nil
				}
			}
			return fmt.Errorf("the hub's certificate does not match the join code")
		}}}
	return &http.Client{Transport: tr, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}

func Post(ctx context.Context, client *http.Client, url, credential string, v any) (int, []byte, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return 0, nil, e
	}
	if len(b) > ReportLimit {
		return 0, nil, fmt.Errorf("report exceeds %d bytes", ReportLimit)
	}
	req, e := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if e != nil {
		return 0, nil, e
	}
	req.Header.Set("Content-Type", "application/json")
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	resp, e := client.Do(req)
	if e != nil {
		return 0, nil, e
	}
	defer resp.Body.Close()
	body, e := primitives.ReadBody(resp.Body, reportReplyLimit)
	return resp.StatusCode, body, e
}

func HubError(status int, body []byte) error {
	var failure struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &failure) == nil && checksandalerts.PlainText(failure.Error, 200) && failure.Error != "" {
		return fmt.Errorf("hub returned HTTP %d: %s", status, failure.Error)
	}
	return fmt.Errorf("hub returned HTTP %d", status)
}

// ReadCredential returns the stored credential, or nil when there is none or
// it cannot be used.
func ReadCredential(path string) *HubCredential {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil
	}
	var cred HubCredential
	if json.Unmarshal(b, &cred) != nil || !tokenPattern.MatchString(cred.Credential) {
		return nil
	}
	return &cred
}
