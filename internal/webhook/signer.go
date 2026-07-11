package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
