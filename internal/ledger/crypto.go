package ledger

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// DescriptionCipher is the minimal interface TransactionService,
// AccountService, and AuditService need to encrypt posting descriptions at
// post time and decrypt them at read time (Task 6.2, audit A9.3):
// *internal/crypto.Cipher satisfies it. It is declared here, in the
// consuming package, rather than depended on directly, the same
// "accept a narrow interface" shape fx.Provider already uses for
// TransactionService's rate lookups.
//
// A nil DescriptionCipher (the zero value every service here starts with
// unless WithCipher/WithAuditCipher is used) leaves encryption disabled: a
// caller or test that never wires one in keeps behaving exactly as it did
// before Task 6.2, storing and returning descriptions as plain strings. This
// is the "ENCRYPTION_ENABLED only when the master key is set" default (see
// cmd/server's config loading).
type DescriptionCipher interface {
	// Encrypt returns plaintext encrypted for tenantID, or an error (for
	// example crypto.ErrTenantKeyShredded, if the caller tries to encrypt new
	// content for a tenant whose key has already been shredded).
	Encrypt(ctx context.Context, tenantID, plaintext string) (string, error)
	// Decrypt returns stored's plaintext, a legacy value unchanged, or
	// crypto.RedactedMarker for a shredded tenant; see
	// internal/crypto.Cipher.Decrypt's own doc comment for the exact
	// contract every implementation must honor.
	Decrypt(ctx context.Context, tenantID, stored string) (string, error)
}

// encryptPostings returns a NEW slice of postings (a different backing
// array from postings) with each non-empty Description encrypted via
// cipher. postings itself is never mutated: a caller that still holds the
// original slice header (for example TransactionService.Post's "before"
// snapshot, captured before this call for idempotency-fingerprint replay on
// a later conflict) is completely unaffected by this call, since reassigning
// a *domain.Transaction's Postings field to the returned slice never writes
// through the original backing array.
func encryptPostings(ctx context.Context, cipher DescriptionCipher, tenantID string, postings []domain.Posting) ([]domain.Posting, error) {
	out := make([]domain.Posting, len(postings))
	copy(out, postings)
	for i := range out {
		if out[i].Description == "" {
			continue
		}
		ct, err := cipher.Encrypt(ctx, tenantID, out[i].Description)
		if err != nil {
			return nil, err
		}
		out[i].Description = ct
	}
	return out, nil
}

// decryptPostings returns a NEW slice of postings with each non-empty
// Description decrypted via cipher (see DescriptionCipher.Decrypt for what
// "decrypt" means for a legacy plaintext or a shredded tenant's ciphertext).
func decryptPostings(ctx context.Context, cipher DescriptionCipher, tenantID string, postings []domain.Posting) ([]domain.Posting, error) {
	out := make([]domain.Posting, len(postings))
	copy(out, postings)
	for i := range out {
		if out[i].Description == "" {
			continue
		}
		pt, err := cipher.Decrypt(ctx, tenantID, out[i].Description)
		if err != nil {
			return nil, err
		}
		out[i].Description = pt
	}
	return out, nil
}

// decryptAuditEntries returns entries with every embedded posting
// description, inside Before and After's "postings" array, decrypted via
// cipher, FOR DISPLAY ONLY (the get-transaction-audit and get-account-audit
// REST endpoints, internal/api/audit.go). It never touches PrevHash or
// RowHash, and is never called from Verify/VerifyFromLatestAnchor/verifyFrom:
// the tamper-evident hash chain (ADR-012) is computed over, and must always
// be recomputed and compared against, the EXACT stored bytes, ciphertext
// included; decrypting before hashing would make Verify unable to ever
// detect a tampered row, and decrypting only for humans to read is the
// entire point of crypto-shredding still leaving the chain intact after an
// erasure (see internal/crypto's package doc comment). A nil cipher (the
// disabled default) returns entries completely unchanged, deliberately
// skipping the JSON round trip below.
func decryptAuditEntries(ctx context.Context, cipher DescriptionCipher, tenantID string, entries []domain.AuditEntry) ([]domain.AuditEntry, error) {
	if cipher == nil {
		return entries, nil
	}
	out := make([]domain.AuditEntry, len(entries))
	copy(out, entries)
	for i := range out {
		after, err := decryptSnapshotDescriptions(ctx, cipher, tenantID, out[i].After)
		if err != nil {
			return nil, err
		}
		out[i].After = after
		before, err := decryptSnapshotDescriptions(ctx, cipher, tenantID, out[i].Before)
		if err != nil {
			return nil, err
		}
		out[i].Before = before
	}
	return out, nil
}

// decryptSnapshotDescriptions decrypts every posting's "description" field
// embedded in an audit snapshot's raw JSON (see auditSnapshot in service.go
// for the shape written), returning a re-marshaled copy. raw is decoded with
// json.Decoder.UseNumber so every numeric field (amounts, rates, ids) round
// trips through decode/re-encode byte-for-byte instead of through float64,
// which would silently lose precision on an amount larger than 2^53 minor
// units: this function is display-only, but a wrong amount shown to an
// auditor is still a real defect, not merely a cosmetic one.
//
// An empty raw, or one that does not look like the map-of-postings shape
// auditSnapshot writes, is returned completely unchanged rather than erroring:
// this must never fail a read merely because a future snapshot shape (or a
// pre-6.2 row with no "postings" key at all, though every existing snapshot
// does have one) does not match what this function expects.
func decryptSnapshotDescriptions(ctx context.Context, cipher DescriptionCipher, tenantID string, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var snapshot map[string]any
	if err := dec.Decode(&snapshot); err != nil {
		return raw, nil
	}
	postingsRaw, ok := snapshot["postings"]
	if !ok {
		return raw, nil
	}
	postings, ok := postingsRaw.([]any)
	if !ok {
		return raw, nil
	}
	changed := false
	for _, p := range postings {
		posting, ok := p.(map[string]any)
		if !ok {
			continue
		}
		desc, ok := posting["description"].(string)
		if !ok || desc == "" {
			continue
		}
		pt, err := cipher.Decrypt(ctx, tenantID, desc)
		if err != nil {
			return nil, err
		}
		posting["description"] = pt
		changed = true
	}
	if !changed {
		return raw, nil
	}
	out, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	return out, nil
}
