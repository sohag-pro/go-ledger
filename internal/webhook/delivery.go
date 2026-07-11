package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres/sqlc"
)

// deliverBatch reads up to cfg.DeliveryBatch due webhook_deliveries rows
// (pending or failed, next_attempt_at <= now, whose subscription is still
// active) and attempts each one: a 2xx response marks it delivered; any
// other outcome (a non-2xx response or a transport error) increments its
// attempts and either schedules the next backoff (status stays/returns to
// 'failed') or, once attempts reaches cfg.MaxAttempts, marks it 'dead'
// (Task 4.1, audit A7.1). Each row's outbound HTTP call and its own status
// update run independently of every other row: one slow or failing receiver
// never blocks another row's delivery or its own eventual retry.
//
// It returns the delivered/retried/dead counts for OUTCOMES observed in this
// batch's HTTP attempts; err is non-nil only for an infrastructure failure
// (a DB read or write error), never for an individual delivery's HTTP
// failure, which is itself a normal, expected outcome this function records
// rather than propagates.
func (w *Worker) deliverBatch(ctx context.Context, db dbtx) (delivered, retried, dead int, err error) {
	q := sqlc.New(db)
	rows, err := q.ListDueWebhookDeliveries(ctx, int32(w.cfg.DeliveryBatch)) //nolint:gosec // DeliveryBatch is an application-configured, small positive value
	if err != nil {
		return 0, 0, 0, fmt.Errorf("webhook delivery: list due deliveries: %w", err)
	}

	for _, row := range rows {
		outcome, attemptErr := w.attemptOne(ctx, row)
		switch outcome {
		case outcomeDelivered:
			// A 0 rows-affected result means a concurrent worker already
			// carried this row to a terminal state (delivered or dead)
			// before this write landed (see MarkWebhookDeliveryDelivered's
			// doc comment): this write is simply discarded, not an error,
			// and the counters below still reflect this worker's own HTTP
			// outcome for observability, even though the DB row itself did
			// not move.
			if _, err := q.MarkWebhookDeliveryDelivered(ctx, sqlc.MarkWebhookDeliveryDeliveredParams{
				ID:       row.ID,
				Attempts: row.Attempts + 1,
			}); err != nil {
				return delivered, retried, dead, fmt.Errorf("webhook delivery: mark delivered %s: %w", row.ID, err)
			}
			delivered++
		case outcomeDead:
			if _, err := q.MarkWebhookDeliveryFailed(ctx, sqlc.MarkWebhookDeliveryFailedParams{
				ID:            row.ID,
				Status:        string(domain.WebhookDeliveryDead),
				Attempts:      row.Attempts + 1,
				NextAttemptAt: time.Now().UTC(),
				LastError:     errText(attemptErr),
			}); err != nil {
				return delivered, retried, dead, fmt.Errorf("webhook delivery: mark dead %s: %w", row.ID, err)
			}
			dead++
		case outcomeRetry:
			newAttempts := row.Attempts + 1
			next := time.Now().UTC().Add(backoffFor(int(newAttempts), w.cfg.BackoffBase, w.cfg.BackoffCap))
			if _, err := q.MarkWebhookDeliveryFailed(ctx, sqlc.MarkWebhookDeliveryFailedParams{
				ID:            row.ID,
				Status:        string(domain.WebhookDeliveryFailed),
				Attempts:      newAttempts,
				NextAttemptAt: next,
				LastError:     errText(attemptErr),
			}); err != nil {
				return delivered, retried, dead, fmt.Errorf("webhook delivery: mark failed %s: %w", row.ID, err)
			}
			retried++
		}
	}
	return delivered, retried, dead, nil
}

// deliveryOutcome is attemptOne's classification of a single delivery
// attempt: whether it succeeded, should be retried later, or has exhausted
// its attempts and must be marked dead.
type deliveryOutcome int

const (
	outcomeDelivered deliveryOutcome = iota
	outcomeRetry
	outcomeDead
)

// attemptOne makes exactly one HTTP attempt for row (unless its stored
// payload cannot even be decoded, in which case it is dead on arrival: no
// retry could ever fix a payload this worker itself wrote incorrectly). It
// returns the outcome and, for a non-delivered outcome, the error describing
// why (a transport error, a non-2xx status, or a payload decode failure),
// which the caller records as last_error.
func (w *Worker) attemptOne(ctx context.Context, row sqlc.ListDueWebhookDeliveriesRow) (deliveryOutcome, error) {
	var payload domain.WebhookPayload
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		return outcomeDead, fmt.Errorf("decode stored payload: %w", err)
	}
	// Re-marshaling the decoded struct, rather than resending row.Payload's
	// raw jsonb bytes verbatim, is deliberate (see domain.WebhookPayload's
	// own doc comment): Postgres does not preserve a jsonb value's original
	// byte-for-byte text, but json.Marshal on this same fixed Go type always
	// emits the same bytes every time, so every retry of this row signs and
	// sends an identical body.
	body, err := json.Marshal(payload)
	if err != nil {
		return outcomeDead, fmt.Errorf("re-marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, row.Url, bytes.NewReader(body))
	if err != nil {
		return outcomeDead, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderSignature, SignatureHeader(row.Secret, body))
	req.Header.Set(HeaderDeliveryID, row.ID.String())
	req.Header.Set(HeaderEvent, row.EventType)

	resp, err := w.client.Do(req)
	if err != nil {
		return w.retryOrDead(row.Attempts, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return outcomeDelivered, nil
	}
	return w.retryOrDead(row.Attempts, fmt.Errorf("unexpected status code %d", resp.StatusCode))
}

// retryOrDead classifies a failed attempt as retryable or dead based on
// whether the NEXT attempts count (currentAttempts+1) would reach
// cfg.MaxAttempts.
func (w *Worker) retryOrDead(currentAttempts int32, cause error) (deliveryOutcome, error) {
	if int(currentAttempts)+1 >= w.cfg.MaxAttempts {
		return outcomeDead, cause
	}
	return outcomeRetry, cause
}

// errText converts err to the nullable pgtype.Text last_error stores, NULL
// for a nil err (never expected here: every non-delivered outcome carries a
// cause), a truncated message otherwise so one runaway error string cannot
// bloat the table.
func errText(err error) pgtype.Text {
	if err == nil {
		return pgtype.Text{}
	}
	msg := err.Error()
	const maxLen = 1000
	if len(msg) > maxLen {
		msg = msg[:maxLen]
	}
	return pgtype.Text{String: msg, Valid: true}
}
