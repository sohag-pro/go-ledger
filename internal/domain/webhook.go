package domain

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

// webhookSecretPrefix mirrors apiKeyPrefix's role for api keys: a
// recognizable, greppable prefix on every generated webhook signing secret,
// distinct from "glk_" so the two credential families are never confused by
// eye or by a leaked-secret scanner.
const webhookSecretPrefix = "whsec_"

// GenerateWebhookSecret returns a new random CSPRNG signing secret
// ("whsec_<base64url>"), Task 4.1 (audit A7.1). Unlike GenerateAPIKey, there
// is no separate hash: a webhook secret must be read back in full to sign
// every outbound delivery (HMAC-SHA256), so it is stored as-is, not hashed,
// the same way a third-party payment provider's own webhook secrets work.
// It is shown to the caller exactly once, at subscription-creation time, and
// is never included in a subsequent list response (see
// domain.Repository.ListWebhookSubscriptionsByTenant, which does not select
// it at all: WebhookSubscription itself carries no secret field, so there is
// nothing to leak even by mistake).
func GenerateWebhookSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return webhookSecretPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// WebhookSubscription is a tenant's registered callback: a URL to POST
// signed events to, and an optional event-type filter. EventTypes empty
// means "every action" (see Matches); non-empty means only the named
// actions (domain.ActionTransactionCreated, ActionTransactionReversed, or
// any future action) are delivered to this subscription.
//
// There is deliberately no Secret field here: the signing secret is
// generated once (GenerateWebhookSecret), returned to the caller exactly
// once by admin.Service.CreateSubscription, and from then on lives only in
// the webhook_subscriptions.secret column, read back directly by the
// delivery worker (internal/webhook), which talks to Postgres through sqlc
// rather than through domain.Repository. This mirrors domain.APIKey, which
// likewise carries no Hash field: a type that cannot express a stored secret
// cannot be made to leak one through a list endpoint by a future accidental
// change.
type WebhookSubscription struct {
	ID         string
	TenantID   string
	URL        string
	EventTypes []string
	Active     bool
	CreatedAt  time.Time
}

// Matches reports whether action should be delivered to s: true if s has no
// event-type filter configured (EventTypes is empty, meaning "every
// action"), or if action is one of the filter's entries.
func (s WebhookSubscription) Matches(action string) bool {
	if len(s.EventTypes) == 0 {
		return true
	}
	for _, et := range s.EventTypes {
		if et == action {
			return true
		}
	}
	return false
}

// Validate reports ErrInvalidWebhookURL unless s.URL is a well-formed
// absolute http or https URL with a non-empty host. This is checked before
// ever generating a secret or writing a row (admin.Service.CreateSubscription),
// the same fail-closed, validate-before-any-side-effect style
// admin.validateScopes and TenantPolicy.Validate already use elsewhere in
// this codebase.
func (s WebhookSubscription) Validate() error {
	if strings.TrimSpace(s.URL) == "" {
		return ErrInvalidWebhookURL
	}
	u, err := url.Parse(s.URL)
	if err != nil {
		return ErrInvalidWebhookURL
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ErrInvalidWebhookURL
	}
	if u.Host == "" {
		return ErrInvalidWebhookURL
	}
	return nil
}

// WebhookDeliveryStatus is one webhook_deliveries row's lifecycle state
// (Task 4.1). A delivery starts pending, and either ends delivered (a 2xx
// response) or cycles through failed (a non-2xx response or transport
// error, with attempts incremented and next_attempt_at pushed out by
// exponential backoff) until it is either retried into delivered or,
// after MaxAttempts failed attempts, marked dead.
type WebhookDeliveryStatus string

// The four WebhookDeliveryStatus values a webhook_deliveries row can carry;
// see WebhookDeliveryStatus's own doc comment for the lifecycle between them.
const (
	WebhookDeliveryPending   WebhookDeliveryStatus = "pending"
	WebhookDeliveryDelivered WebhookDeliveryStatus = "delivered"
	WebhookDeliveryFailed    WebhookDeliveryStatus = "failed"
	WebhookDeliveryDead      WebhookDeliveryStatus = "dead"
)

// WebhookDelivery is one fan-out row: a single (subscription, audit event)
// pairing the fan-out step created and the delivery worker attempts to
// deliver, at least once, until it succeeds or exhausts its attempts. See
// migration 0021's UNIQUE (subscription_id, audit_chain_seq): fan-out
// creates at most one WebhookDelivery per (subscription, event), no matter
// how many times the fan-out step itself runs.
type WebhookDelivery struct {
	ID             string
	TenantID       string
	SubscriptionID string
	AuditChainSeq  int64
	EventType      string
	Payload        json.RawMessage
	Status         WebhookDeliveryStatus
	Attempts       int
	NextAttemptAt  time.Time
	LastError      *string
	CreatedAt      time.Time
	DeliveredAt    *time.Time
}

// WebhookPayload is the JSON body sent to a subscriber, and also exactly
// what is stored in webhook_deliveries.payload at fan-out time (Task 4.1).
// The delivery worker decodes the stored jsonb back into this same struct
// and re-marshals it fresh on every delivery attempt, rather than trying to
// replay Postgres's own (re-ordered, re-whitespaced) jsonb text: json.Marshal
// on a fixed Go struct always emits its fields in the same order, so every
// retry of the same delivery signs and sends byte-identical bytes, which is
// what makes the signature verifiable and the delivery id-based dedup
// meaningful across retries.
type WebhookPayload struct {
	// ID is the delivery's own id (webhook_deliveries.id), sent again as the
	// X-Ledger-Delivery-Id header: a receiver dedups repeat deliveries of the
	// same event by this value, which is stable across every retry of the
	// same row.
	ID       string `json:"id"`
	Event    string `json:"event"`
	TenantID string `json:"tenant_id"`

	// TransactionID is omitempty (ADR-025): a chained non-transaction
	// lifecycle event (for example approval.requested) has no transaction,
	// only a subject (see SubjectType/SubjectID below), and a consumer
	// should not see a misleading present-but-empty transaction_id key on
	// that kind of delivery.
	TransactionID string `json:"transaction_id,omitempty"`

	// SubjectType and SubjectID (ADR-025) name the entity a non-transaction
	// lifecycle event is about (for example subject_type
	// "pending_transaction", subject_id the pending's own id), so a
	// consumer of that event can tell which pending it concerns. Both are
	// omitempty: an ordinary transaction event carries neither.
	SubjectType string `json:"subject_type,omitempty"`
	SubjectID   string `json:"subject_id,omitempty"`

	OccurredAt time.Time       `json:"occurred_at"`
	Data       json.RawMessage `json:"data"`
}
