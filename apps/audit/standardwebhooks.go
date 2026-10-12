package audit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Standard Webhooks signature verification (https://www.standardwebhooks.com),
// the scheme Polar signs with. It is a few lines of HMAC, so it is written here
// rather than imported: a dependency for this would be larger than the thing it
// replaces, and every line below is covered by a test.
//
// A delivery carries three headers:
//
//	webhook-id         unique per message; the same on a retry
//	webhook-timestamp  unix seconds when it was sent
//	webhook-signature  space-separated "v1,<base64 HMAC-SHA256>" entries
//
// signed over "<id>.<timestamp>.<raw body>". The timestamp is bound into the
// signature, so a captured delivery cannot be replayed later by rewriting it,
// and a stale one is refused outright.

// webhookTolerance is how far a timestamp may sit from now, either way. It is
// the specification's recommended five minutes.
const webhookTolerance = 5 * time.Minute

var (
	errWebhookMissingHeaders = errors.New("webhook: missing signature headers")
	errWebhookTimestamp      = errors.New("webhook: timestamp outside the tolerance")
	errWebhookSignature      = errors.New("webhook: no valid signature")
)

// webhookKeys derives the HMAC keys a signing secret can mean. Secrets Polar
// created from 2026-09-08 are Standard Webhooks secrets: "whsec_" followed by
// the base64 of the key. Older ones used the UTF-8 bytes of the whole string as
// the key. Both are tried so rotating onto either keeps working; a wrong key
// simply fails to match.
func webhookKeys(secret string) [][]byte {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil
	}
	keys := [][]byte{}
	encoded := strings.TrimPrefix(secret, "whsec_")
	if raw, err := base64.StdEncoding.DecodeString(encoded); err == nil && len(raw) > 0 {
		keys = append(keys, raw)
	}
	keys = append(keys, []byte(secret))
	return keys
}

// signWebhook computes the v1 signature for a delivery. It is used by the
// verifier and by the tests and the live check, which have to sign too.
func signWebhook(key []byte, id string, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id))
	mac.Write([]byte("."))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// verifyWebhook checks a delivery against the secret. It returns nil only when
// the headers are present, the timestamp is within tolerance of now, and at
// least one signature entry matches. Comparison is constant-time.
func verifyWebhook(secret, id, timestamp, signature string, body []byte, now time.Time) error {
	if id == "" || timestamp == "" || signature == "" {
		return errWebhookMissingHeaders
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errWebhookTimestamp
	}
	age := now.Sub(time.Unix(seconds, 0))
	if age > webhookTolerance || age < -webhookTolerance {
		return errWebhookTimestamp
	}
	for _, key := range webhookKeys(secret) {
		want := signWebhook(key, id, timestamp, body)
		for _, candidate := range strings.Fields(signature) {
			if hmac.Equal([]byte(candidate), []byte(want)) {
				return nil
			}
		}
	}
	return errWebhookSignature
}
