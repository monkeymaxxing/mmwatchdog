package checksandalerts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"mmwatchdog/internal/primitives"
)

// MMHotline request signing, signed receipts, one request per attempt, and clocks.

const (
	MMHotlineAnswerLimit = 64 * 1024
	// ClockShownAfter is when mmwatchdog status names the clock offset; MMHotline
	// refuses a signature more than clockRefusedAfter off.
	ClockShownAfter   = time.Minute
	clockRefusedAfter = 5 * time.Minute
)

// MMHotlineIDPattern accepts MMHotline's alert source IDs, by MMHotline's
// own rule for its settings file: 1 to 64 letters, digits, _ and -, such
// as monitoring, or the 32 hexadecimal characters of an ID that MMHotline
// made. Each character is safe in the request paths that carry the ID.
var MMHotlineIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var signingSecretPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ReadMMHotlineSecret reads a signing secret file: 64 lowercase hex characters.
func ReadMMHotlineSecret(path string) ([]byte, error) {
	b, e := primitives.ReadFile(path, 4096)
	if e != nil && !errors.Is(e, primitives.ErrFileLargerThanLimit) {
		return nil, e
	}
	// A file that is larger than a secret can be holds none.
	secret, e := DecodeSigningSecret(strings.TrimSpace(string(b)))
	if e != nil {
		return nil, fmt.Errorf("%s: %w", path, e)
	}
	return secret, nil
}

func DecodeSigningSecret(s string) ([]byte, error) {
	if !signingSecretPattern.MatchString(s) {
		return nil, fmt.Errorf("expected an MMHotline signing secret of 64 hex characters")
	}
	return hex.DecodeString(s)
}

// mmhotlineSign returns the Mmhotline-Signature header for body, and its HMAC,
// which the receipt signature covers.
func mmhotlineSign(secret []byte, at time.Time, body []byte) (string, string) {
	timestamp := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	sum := hex.EncodeToString(mac.Sum(nil))
	return "timestamp_unix_seconds=" + timestamp + ",hmac_sha256_hex=" + sum, sum
}

// mmhotlineReceiptValid checks a Mmhotline-Receipt-Signature header: an
// HMAC-SHA256 with the signing secret over "mmhotline-receipt.", the route, the
// status, the request's own HMAC, the timestamp, and the answer's exact body,
// each followed by a dot. Binding the route, status, and request means an
// answer to another request, including MMWatchdog's own request replayed elsewhere,
// never verifies.
func mmhotlineReceiptValid(secret []byte, headers []string, route string, status int, requestHMAC string, body []byte) bool {
	if len(headers) != 1 {
		return false
	}
	parts := strings.Split(headers[0], ",")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "timestamp_unix_seconds=") || !strings.HasPrefix(parts[1], "hmac_sha256_hex=") {
		return false
	}
	timestamp := strings.TrimPrefix(parts[0], "timestamp_unix_seconds=")
	if seconds, e := strconv.ParseInt(timestamp, 10, 64); e != nil || strconv.FormatInt(seconds, 10) != timestamp {
		return false
	}
	digest, e := hex.DecodeString(strings.TrimPrefix(parts[1], "hmac_sha256_hex="))
	if e != nil || len(digest) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("mmhotline-receipt." + route + "." + strconv.Itoa(status) + "." + requestHMAC + "." + timestamp + "."))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), digest)
}

// MMHotlineConnection holds one destination's signed-answer clock and latest
// check-in result. Mu is never held during network requests.
type MMHotlineConnection struct {
	Mu                 sync.Mutex
	Offset             time.Duration
	OffsetKnown        bool
	anyOffset          time.Duration
	anyOffsetKnown     bool
	LastSignedAnswerAt time.Time
	CheckInError       string
	CheckInErrorAt     time.Time
	CheckInShortened   string
}

// observeClock records the offset of this node's clock from an answer's
// Date header. Only a signed answer sets the offset status shows, so an
// impostor cannot make MMWatchdog blame the clock; any answer sets the hint a
// refused signature names.
func (p *MMHotlineConnection) observeClock(date string, signed bool, now time.Time) {
	at, e := http.ParseTime(date)
	if e != nil {
		return
	}
	p.Mu.Lock()
	defer p.Mu.Unlock()
	offset := now.Sub(at).Round(time.Second)
	p.anyOffset, p.anyOffsetKnown = offset, true
	if signed {
		p.Offset, p.OffsetKnown = offset, true
	}
}

// ClockProblem describes the offset from signed answers when it is over
// limit, in plain words, or returns "".
func (p *MMHotlineConnection) ClockProblem(limit time.Duration) string {
	p.Mu.Lock()
	offset, known := p.Offset, p.OffsetKnown
	p.Mu.Unlock()
	if !known || (offset <= limit && offset >= -limit) {
		return ""
	}
	amount, direction := describeOffset(offset)
	return "This node's clock is " + amount + " " + direction + " MMHotline's. Fix time sync (for example systemd-timesyncd or chrony)."
}

// clockHint describes the offset from any answer when MMHotline would
// refuse a request that far off, or returns "".
func (p *MMHotlineConnection) clockHint() string {
	p.Mu.Lock()
	offset, known := p.anyOffset, p.anyOffsetKnown
	p.Mu.Unlock()
	if !known || (offset <= clockRefusedAfter && offset >= -clockRefusedAfter) {
		return ""
	}
	amount, direction := describeOffset(offset)
	return "This node's clock may be " + amount + " " + direction + " MMHotline's; MMHotline refuses requests more than 5 minutes off."
}

func describeOffset(offset time.Duration) (string, string) {
	direction := "ahead of"
	if offset < 0 {
		direction, offset = "behind", -offset
	}
	if offset < 90*time.Second {
		return fmt.Sprintf("%d seconds", int(offset.Seconds())), direction
	}
	return fmt.Sprintf("%d minutes", int(math.Round(offset.Minutes()))), direction
}

// MMHotlineRefusal is a signed 400: MMHotline permanently refuses this request,
// so retrying it cannot help.
type MMHotlineRefusal struct {
	Status int
	reason string
}

// Reason is MMHotline's own words for the refusal.
func (r *MMHotlineRefusal) Reason() string { return r.reason }

func (r *MMHotlineRefusal) Error() string {
	return fmt.Sprintf("MMHotline permanently refused this alert (HTTP %d): %s", r.Status, r.reason)
}

// RetryLater is a signed 429: try again after the time MMHotline asked for,
// at the configured address.
type RetryLater struct{ After time.Duration }

func (r *RetryLater) Error() string {
	return fmt.Sprintf("MMHotline asked MMWatchdog to wait %s before sending again (HTTP 429).", r.After)
}

// RetryAfter reads a Retry-After header in seconds or as an HTTP date,
// defaulting to a minute and bounded to an hour.
func RetryAfter(value string, now time.Time) time.Duration {
	after := time.Minute
	if seconds, e := strconv.Atoi(strings.TrimSpace(value)); e == nil {
		after = time.Duration(seconds) * time.Second
	} else if at, e := http.ParseTime(value); e == nil {
		after = at.Sub(now)
	}
	return min(max(after, time.Second), time.Hour)
}

// Send makes exactly one signed request. Only a signed 200 receipt delivers;
// signed 400 and 429 answers refuse or postpone delivery.
func (p *MMHotlineConnection) Send(ctx context.Context, route string, dest MMHotlineDestination, secret, body []byte, limit time.Duration) ([]byte, error) {
	target := strings.TrimRight(dest.URL, "/") + "/" + route + "/" + dest.AlertSourceID
	answer, err := p.post(ctx, route, target, secret, body, limit)
	if err != nil {
		if answer.status == http.StatusUnauthorized && errors.Is(err, errUnsignedAnswer) {
			message := "MMHotline rejected the signature (HTTP 401). MMHotline's settings file may no longer list this signing secret or this alert source. Write the signing secret that it lists for the alert source into " + dest.SigningSecretFile + " and run sudo mmwatchdog reload."
			if hint := p.clockHint(); hint != "" {
				message += " " + hint
			}
			return nil, errors.New(message)
		}
		if answer.status != 0 {
			return nil, fmt.Errorf("MMHotline gave no signed answer (HTTP %d).", answer.status)
		}
		return nil, fmt.Errorf("MMHotline did not answer (%v).", err)
	}
	switch answer.status {
	case http.StatusBadRequest:
		return nil, &MMHotlineRefusal{answer.status, primitives.Clip(strings.TrimSpace(string(answer.body)), 512)}
	case http.StatusTooManyRequests:
		return nil, &RetryLater{RetryAfter(answer.retryAfter, time.Now())}
	case http.StatusOK:
		var receipt struct {
			AlertID    string `json:"mmhotline_alert_id"`
			ReceivedAt string `json:"check_in_received_at"`
		}
		if json.Unmarshal(answer.body, &receipt) == nil && ((route == "alert-intake" && receipt.AlertID != "") || (route == "check-in" && receipt.ReceivedAt != "")) {
			return answer.body, nil
		}
	}
	return nil, fmt.Errorf("MMHotline gave no signed answer (HTTP %d).", answer.status)
}

var errUnsignedAnswer = errors.New("answer without a valid receipt signature")

// mmhotlineAnswer is one answer from MMHotline.
type mmhotlineAnswer struct {
	status     int
	body       []byte
	retryAfter string
}

// post sends one signed request. It returns an answer whose receipt
// signature verifies, and errUnsignedAnswer, with the status, for any other
// answer.
func (p *MMHotlineConnection) post(ctx context.Context, route, target string, secret, body []byte, limit time.Duration) (mmhotlineAnswer, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, "POST", target, bytes.NewReader(body))
	if e != nil {
		return mmhotlineAnswer{}, e
	}
	signature, requestHMAC := mmhotlineSign(secret, time.Now(), body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Mmhotline-Signature", signature)
	client := MMHotlineClient()
	defer client.CloseIdleConnections()
	resp, e := client.Do(req)
	if e != nil {
		return mmhotlineAnswer{}, e
	}
	defer resp.Body.Close()
	answer := mmhotlineAnswer{status: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After")}
	if answer.body, e = primitives.ReadBody(resp.Body, MMHotlineAnswerLimit); e != nil {
		return answer, e
	}
	signed := mmhotlineReceiptValid(secret, resp.Header.Values("Mmhotline-Receipt-Signature"), route, resp.StatusCode, requestHMAC, answer.body)
	now := time.Now()
	p.observeClock(resp.Header.Get("Date"), signed, now)
	if signed {
		p.Mu.Lock()
		p.LastSignedAnswerAt = now
		p.Mu.Unlock()
	}
	if !signed {
		return answer, fmt.Errorf("HTTP %d: %w", resp.StatusCode, errUnsignedAnswer)
	}
	return answer, nil
}

// MMHotlineClient verifies MMHotline's certificate with the system trust
// store and follows no redirects.
func MMHotlineClient() *http.Client {
	secure := primitives.ServerTLSConfig(nil, false)
	secure.MinVersion = tls.VersionTLS12
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: secure}
	return &http.Client{Transport: tr, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}

func (p *MMHotlineConnection) CheckInResult(now time.Time, e error, shortened string) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	p.CheckInShortened = shortened
	if e != nil {
		p.CheckInError, p.CheckInErrorAt = e.Error(), now
		return
	}
	p.CheckInError, p.CheckInErrorAt = "", time.Time{}
}
