package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// Sign returns the hex-encoded HMAC-SHA256 of body, keyed by secret (Task
// 4.1, audit A7.1). It is the same digest a subscriber recomputes to verify
// a delivery: the subscriber knows its own secret, reads the raw request
// body, and checks that Sign(secret, body) matches the hex digest carried
// after "sha256=" in the X-Ledger-Signature header (see SignatureHeader).
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// SignatureHeader returns the full X-Ledger-Signature header value for body
// signed with secret: "sha256=<hex hmac>". The "sha256=" prefix names the
// algorithm explicitly in the wire format itself, the same shape widely used
// by other webhook providers, so a future signing algorithm could be added
// as a second, additionally-sent header without breaking an existing
// subscriber that only understands this one.
func SignatureHeader(secret string, body []byte) string {
	return "sha256=" + Sign(secret, body)
}

// signingInput binds the send timestamp to the body so the signature covers
// both: "<unix seconds>.<body>", the same construction Stripe and others use.
// A subscriber recomputes it from the X-Ledger-Timestamp header and the raw
// request body, so a captured delivery replayed later fails the freshness check
// the subscriber runs on the timestamp before comparing signatures.
func signingInput(timestamp int64, body []byte) []byte {
	prefix := strconv.FormatInt(timestamp, 10) + "."
	buf := make([]byte, 0, len(prefix)+len(body))
	buf = append(buf, prefix...)
	buf = append(buf, body...)
	return buf
}

// SignatureHeaderAt returns the X-Ledger-Signature value for body sent at
// timestamp (Unix seconds), signing "<timestamp>.<body>" so the timestamp is
// tamper-evident alongside the body (audit A: webhook signed timestamp). The
// wire format is unchanged ("sha256=<hex>"); what changed is the signed
// content, so a subscriber verifies by recomputing over the timestamp header
// plus the body rather than the body alone.
func SignatureHeaderAt(secret string, timestamp int64, body []byte) string {
	return "sha256=" + Sign(secret, signingInput(timestamp, body))
}
