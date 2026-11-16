package checksandalerts

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/primitives"
)

var fuzzSigningSecret = bytes.Repeat([]byte{0x5a}, 32)

func FuzzMMHotlineReceipt(f *testing.F) {
	alertBody := []byte(`{"sender_alert_id":"a1","severity":"warn","title":"web-3: myapp/http failed","message":""}`)
	_, requestHMAC := mmhotlineSign(fuzzSigningSecret, time.Unix(1_700_000_000, 0), alertBody)
	const at = int64(1_700_000_003)
	timestamp := strconv.FormatInt(at, 10)
	receiptBody := []byte(`{"mmhotline_alert_id":"al_a1","already_received":false}`)
	checkInBody := []byte(`{"check_in_received_at":"2026-10-16T12:00:00Z"}`)
	refusalBody := []byte(`{"error":"title must contain 1-256 characters"}`)
	conflictBody := []byte(`{"error":"conflict"}`)
	limitBody := []byte(`{"error":"admission limit reached; retry later"}`)
	otherSecret := bytes.Repeat([]byte{1}, 32)
	signedBy := func(secret []byte, route string, status int, body []byte) string {
		return documentedReceipt(secret, route, status, requestHMAC, timestamp, body)
	}
	valid := signedBy(fuzzSigningSecret, "alert-intake", 200, receiptBody)
	atLimit := append(bytes.Clone(receiptBody), bytes.Repeat([]byte(" "), MMHotlineAnswerLimit-len(receiptBody))...)
	overLimit := append(bytes.Clone(atLimit), ' ')
	digest := strings.TrimPrefix(valid, "timestamp_unix_seconds="+timestamp+",hmac_sha256_hex=")
	for _, seed := range []struct {
		header string
		route  string
		status int
		body   []byte
	}{
		{valid, "alert-intake", 200, receiptBody},
		{signedBy(fuzzSigningSecret, "alert-intake", 200, atLimit), "alert-intake", 200, atLimit},
		{signedBy(fuzzSigningSecret, "alert-intake", 200, overLimit), "alert-intake", 200, overLimit},
		{signedBy(fuzzSigningSecret, "check-in", 200, checkInBody), "check-in", 200, checkInBody},
		{signedBy(fuzzSigningSecret, "alert-intake", 400, refusalBody), "alert-intake", 400, refusalBody},
		{signedBy(fuzzSigningSecret, "alert-intake", 409, conflictBody), "alert-intake", 409, conflictBody},
		{signedBy(fuzzSigningSecret, "alert-intake", 429, limitBody), "alert-intake", 429, limitBody},
		// A wrongly signed receipt, and a receipt of another route replayed.
		{signedBy(otherSecret, "alert-intake", 200, receiptBody), "alert-intake", 200, receiptBody},
		{signedBy(fuzzSigningSecret, "check-in", 200, receiptBody), "alert-intake", 200, receiptBody},
		// Headers of another form.
		{"", "alert-intake", 200, receiptBody},
		{"timestamp_unix_seconds=+" + timestamp + ",hmac_sha256_hex=" + digest, "alert-intake", 200, receiptBody},
		{"timestamp_unix_seconds=0" + timestamp + ",hmac_sha256_hex=" + digest, "alert-intake", 200, receiptBody},
		{"timestamp_unix_seconds=" + timestamp + ",hmac_sha256_hex=" + strings.ToUpper(digest), "alert-intake", 200, receiptBody},
		{"hmac_sha256_hex=" + digest + ",timestamp_unix_seconds=" + timestamp, "alert-intake", 200, receiptBody},
		{valid + ",", "alert-intake", 200, receiptBody},
	} {
		f.Add(seed.header, seed.route, seed.status, requestHMAC, seed.body, at, uint32(0))
	}

	f.Fuzz(func(t *testing.T, header, route string, status int, requestHMAC string, body []byte, timestampUnixSeconds int64, flipAt uint32) {
		// post bounds the HTTP response body before checking its signature.
		received, readError := primitives.ReadBody(bytes.NewReader(body), MMHotlineAnswerLimit)
		if (readError != nil) != (len(body) > MMHotlineAnswerLimit) {
			t.Fatalf("receipt body of %d bytes, limit %d: %v", len(body), MMHotlineAnswerLimit, readError)
		}
		if readError != nil {
			return
		}
		if !bytes.Equal(received, body) {
			t.Fatal("bounded receipt reader changed the body before verification")
		}
		body = received
		// The header as an answer carries it.
		if mmhotlineReceiptValid(fuzzSigningSecret, []string{header}, route, status, requestHMAC, body) {
			requireDocumentedReceipt(t, header, route, status, requestHMAC, body)
		}

		// A receipt that MMHotline signs as documented, and the same receipt
		// with one change.
		signed := documentedReceipt(fuzzSigningSecret, route, status, requestHMAC, strconv.FormatInt(timestampUnixSeconds, 10), body)
		if !mmhotlineReceiptValid(fuzzSigningSecret, []string{signed}, route, status, requestHMAC, body) {
			t.Fatalf("the documented receipt %q did not verify: route %q, HTTP status code %d, request %q, body %q", signed, route, status, requestHMAC, body)
		}
		// XOR 0x01 turns a digit into another digit and a lower-case hex
		// letter into another letter or into a character that is no hex.
		// XOR 0x20 would only change the case, which hex.DecodeString reads
		// as the same digest.
		changedHeader := []byte(signed)
		changedHeader[flipAt%uint32(len(changedHeader))] ^= 0x01
		refused := map[string]bool{}
		refused["no receipt header"] = mmhotlineReceiptValid(fuzzSigningSecret, nil, route, status, requestHMAC, body)
		refused["the receipt header twice"] = mmhotlineReceiptValid(fuzzSigningSecret, []string{signed, signed}, route, status, requestHMAC, body)
		refused["another signing secret"] = mmhotlineReceiptValid(otherSecret, []string{signed}, route, status, requestHMAC, body)
		refused["a changed header byte"] = mmhotlineReceiptValid(fuzzSigningSecret, []string{string(changedHeader)}, route, status, requestHMAC, body)
		refused["another route"] = mmhotlineReceiptValid(fuzzSigningSecret, []string{signed}, route+"x", status, requestHMAC, body)
		refused["another HTTP status code"] = mmhotlineReceiptValid(fuzzSigningSecret, []string{signed}, route, status^1, requestHMAC, body)
		refused["another request"] = mmhotlineReceiptValid(fuzzSigningSecret, []string{signed}, route, status, requestHMAC+"0", body)
		refused["one more body byte"] = mmhotlineReceiptValid(fuzzSigningSecret, []string{signed}, route, status, requestHMAC, append(bytes.Clone(body), '}'))
		if len(body) > 0 {
			changedBody := bytes.Clone(body)
			changedBody[flipAt%uint32(len(changedBody))] ^= 0x01
			refused["a changed body byte"] = mmhotlineReceiptValid(fuzzSigningSecret, []string{signed}, route, status, requestHMAC, changedBody)
		}
		for change, accepted := range refused {
			if accepted {
				t.Fatalf("the receipt %q verified with %s: route %q, HTTP status code %d, request %q, body %q", signed, change, route, status, requestHMAC, body)
			}
		}
	})
}

// requireDocumentedReceipt requires that a header which mmhotlineReceiptValid
// accepted has the documented form and the documented HMAC.
func requireDocumentedReceipt(t *testing.T, header, route string, status int, requestHMAC string, body []byte) {
	t.Helper()
	timestampPart, digestPart, found := strings.Cut(header, ",")
	timestamp, hasTimestamp := strings.CutPrefix(timestampPart, "timestamp_unix_seconds=")
	digestHex, hasDigest := strings.CutPrefix(digestPart, "hmac_sha256_hex=")
	seconds, secondsError := strconv.ParseInt(timestamp, 10, 64)
	digest, digestError := hex.DecodeString(digestHex)
	switch {
	case !found || !hasTimestamp || !hasDigest || strings.Contains(digestPart, ","):
		t.Fatalf("mmhotlineReceiptValid accepted the header %q, which is not timestamp_unix_seconds=...,hmac_sha256_hex=...", header)
	case secondsError != nil || strconv.FormatInt(seconds, 10) != timestamp:
		t.Fatalf("mmhotlineReceiptValid accepted the header %q, whose timestamp %q is not a whole number in its plain form", header, timestamp)
	case digestError != nil || !hmac.Equal(digest, documentedReceiptHMAC(fuzzSigningSecret, route, status, requestHMAC, timestamp, body)):
		t.Fatalf("mmhotlineReceiptValid accepted the header %q, whose HMAC is not the documented one", header)
	}
}

// documentedReceiptHMAC is the HMAC of a receipt as docs/reference/modules/mmhotline.md
// describes it: HMAC-SHA256 with the signing secret over "mmhotline-receipt."
// + route + "." + status + "." + request_hmac + "." + timestamp + "." + body.
func documentedReceiptHMAC(secret []byte, route string, status int, requestHMAC, timestamp string, body []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("mmhotline-receipt." + route + "." + strconv.Itoa(status) + "." + requestHMAC + "." + timestamp + "."))
	mac.Write(body)
	return mac.Sum(nil)
}

// documentedReceipt is the Mmhotline-Receipt-Signature header of a receipt as
// docs/reference/modules/mmhotline.md describes it.
func documentedReceipt(secret []byte, route string, status int, requestHMAC, timestamp string, body []byte) string {
	return "timestamp_unix_seconds=" + timestamp + ",hmac_sha256_hex=" + hex.EncodeToString(documentedReceiptHMAC(secret, route, status, requestHMAC, timestamp, body))
}
