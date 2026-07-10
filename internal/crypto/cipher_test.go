package crypto_test

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sohag-pro/go-ledger/internal/crypto"
)

// fakeKeyRow is one crypto_keys row in fakeKeyStore's in-memory model:
// exactly what migration 0028 added a version to (ADR-018).
type fakeKeyRow struct {
	wrappedDEK []byte // nil once shredded
	shredded   bool
}

// fakeKeyStore is an in-memory, VERSIONED crypto.KeyStore, standing in for
// internal/postgres.Repository's crypto_keys-backed implementation. It is
// deliberately race-safe (a mutex) so TestCipher_ConcurrentFirstUse can prove
// MintTenantDEKVersion's "first caller wins" contract under -race.
type fakeKeyStore struct {
	mu   sync.Mutex
	rows map[string]map[int]*fakeKeyRow // tenantID -> version -> row
}

func newFakeKeyStore() *fakeKeyStore {
	return &fakeKeyStore{rows: map[string]map[int]*fakeKeyRow{}}
}

func (f *fakeKeyStore) CurrentTenantDEK(_ context.Context, tenantID string) ([]byte, int, bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	versions := f.rows[tenantID]
	best := 0
	for v := range versions {
		if v > best {
			best = v
		}
	}
	if best == 0 {
		return nil, 0, false, false, nil
	}
	row := versions[best]
	return row.wrappedDEK, best, row.shredded, true, nil
}

func (f *fakeKeyStore) MintTenantDEKVersion(_ context.Context, tenantID string, version int, candidate []byte) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows[tenantID] == nil {
		f.rows[tenantID] = map[int]*fakeKeyRow{}
	}
	if existing, ok := f.rows[tenantID][version]; ok {
		return existing.wrappedDEK, existing.shredded, nil
	}
	f.rows[tenantID][version] = &fakeKeyRow{wrappedDEK: candidate}
	return candidate, false, nil
}

func (f *fakeKeyStore) TenantDEKVersion(_ context.Context, tenantID string, version int) ([]byte, bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[tenantID][version]
	if !ok {
		return nil, false, false, nil
	}
	return row.wrappedDEK, row.shredded, true, nil
}

// shred is the test's stand-in for domain.Repository.ShredTenantCryptoKey:
// it destroys only the CURRENT (highest) version, exactly like migration
// 0028's ShredCurrentCryptoKey query, minting a version-1 tombstone if the
// tenant had never encrypted anything yet.
func (f *fakeKeyStore) shredTenant(tenantID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows[tenantID] == nil {
		f.rows[tenantID] = map[int]*fakeKeyRow{}
	}
	best := 0
	for v := range f.rows[tenantID] {
		if v > best {
			best = v
		}
	}
	if best == 0 {
		best = 1
	}
	f.rows[tenantID][best] = &fakeKeyRow{shredded: true}
}

// testMasterKey is a fixed, valid 32-byte master key, base64-encoded, used
// throughout this file: unit tests need a real key to exercise real AES-GCM
// round trips, but the exact bytes carry no meaning.
const testMasterKey = "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=" // base64("01234567890123456789012345678901")

func newTestCipher(t *testing.T, store crypto.KeyStore) *crypto.Cipher {
	t.Helper()
	c, err := crypto.NewCipher(testMasterKey, store)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

func TestNewCipher_RejectsMalformedMasterKey(t *testing.T) {
	store := newFakeKeyStore()
	cases := map[string]string{
		"not base64": "not-valid-base64!!!",
		"too short":  base64.StdEncoding.EncodeToString([]byte("short")),
		"empty":      "",
		"33 bytes":   base64.StdEncoding.EncodeToString(make([]byte, 33)),
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := crypto.NewCipher(key, store); err == nil {
				t.Errorf("NewCipher(%q) = nil error, want a fail-fast error", key)
			}
		})
	}
}

func TestNewCipher_RejectsNilStore(t *testing.T) {
	if _, err := crypto.NewCipher(testMasterKey, nil); err == nil {
		t.Error("NewCipher with a nil store = nil error, want an error")
	}
}

func TestCipher_RoundTrip(t *testing.T) {
	c := newTestCipher(t, newFakeKeyStore())
	ctx := context.Background()
	tenant := "tenant-a"

	ct, err := c.Encrypt(ctx, tenant, "rent payment")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !strings.HasPrefix(ct, crypto.EncodingPrefix) {
		t.Fatalf("ciphertext %q does not carry EncodingPrefix %q", ct, crypto.EncodingPrefix)
	}
	if ct == "rent payment" {
		t.Fatal("ciphertext must not equal the plaintext")
	}

	pt, err := c.Decrypt(ctx, tenant, ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if pt != "rent payment" {
		t.Errorf("Decrypt round trip = %q, want %q", pt, "rent payment")
	}
}

func TestCipher_EmptyDescriptionStaysEmpty(t *testing.T) {
	c := newTestCipher(t, newFakeKeyStore())
	ctx := context.Background()

	ct, err := c.Encrypt(ctx, "tenant-a", "")
	if err != nil {
		t.Fatalf("Encrypt(\"\"): %v", err)
	}
	if ct != "" {
		t.Errorf("Encrypt(\"\") = %q, want \"\" (never encrypt an empty description)", ct)
	}

	pt, err := c.Decrypt(ctx, "tenant-a", "")
	if err != nil {
		t.Fatalf("Decrypt(\"\"): %v", err)
	}
	if pt != "" {
		t.Errorf("Decrypt(\"\") = %q, want \"\"", pt)
	}
}

func TestCipher_DecryptLegacyPlaintextPassesThrough(t *testing.T) {
	c := newTestCipher(t, newFakeKeyStore())
	ctx := context.Background()

	// A description written before Task 6.2 existed: no EncodingPrefix.
	legacy := "dinner repayment"
	pt, err := c.Decrypt(ctx, "tenant-a", legacy)
	if err != nil {
		t.Fatalf("Decrypt(legacy): %v", err)
	}
	if pt != legacy {
		t.Errorf("Decrypt(legacy plaintext) = %q, want unchanged %q", pt, legacy)
	}
}

func TestCipher_DifferentTenantsGetDifferentKeys(t *testing.T) {
	c := newTestCipher(t, newFakeKeyStore())
	ctx := context.Background()

	ctA, err := c.Encrypt(ctx, "tenant-a", "secret")
	if err != nil {
		t.Fatalf("Encrypt tenant-a: %v", err)
	}

	// Tenant B must not be able to decrypt tenant A's ciphertext with its own
	// (different) DEK: Decrypt should fail closed, not silently return
	// garbage or tenant A's real plaintext.
	if pt, err := c.Decrypt(ctx, "tenant-b", ctA); err == nil {
		t.Errorf("tenant-b decrypted tenant-a's ciphertext as %q, want a decrypt failure", pt)
	}
}

func TestCipher_ShreddedTenantDecryptsToRedactedMarker(t *testing.T) {
	store := newFakeKeyStore()
	c := newTestCipher(t, store)
	ctx := context.Background()
	tenant := "tenant-a"

	ct, err := c.Encrypt(ctx, tenant, "rent payment")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	store.shredTenant(tenant)

	pt, err := c.Decrypt(ctx, tenant, ct)
	if err != nil {
		t.Fatalf("Decrypt after shred returned an error, want the redacted marker with no error: %v", err)
	}
	if pt != crypto.RedactedMarker {
		t.Errorf("Decrypt after shred = %q, want %q", pt, crypto.RedactedMarker)
	}
}

// TestCipher_EncryptAfterShredMintsFreshVersionAndSucceeds is the ADR-018
// fix's core proof: Encrypt for a tenant whose CURRENT key version was just
// shredded (even one that had never encrypted anything before, i.e. a
// version-1 tombstone with no prior live key) does NOT fail closed. It
// mints a fresh, forward version and succeeds, so post/convert/reverse keep
// working after an erasure request instead of failing forever.
func TestCipher_EncryptAfterShredMintsFreshVersionAndSucceeds(t *testing.T) {
	store := newFakeKeyStore()
	c := newTestCipher(t, store)
	ctx := context.Background()
	tenant := "tenant-shred-then-mint"

	store.shredTenant(tenant)

	ct, err := c.Encrypt(ctx, tenant, "new description")
	if err != nil {
		t.Fatalf("Encrypt after shred = %v, want success (a fresh version must be minted)", err)
	}

	pt, err := c.Decrypt(ctx, tenant, ct)
	if err != nil {
		t.Fatalf("Decrypt freshly minted ciphertext: %v", err)
	}
	if pt != "new description" {
		t.Errorf("Decrypt freshly minted ciphertext = %q, want %q", pt, "new description")
	}
}

// TestCipher_ShredThenEncryptThenShredAgain_OldCiphertextStaysRedactedNewOneWorks
// proves the full ADR-018 lifecycle: a description encrypted BEFORE a shred
// stays permanently redacted, a description encrypted AFTER the shred (under
// the freshly minted version) reads back fine, and shredding the tenant a
// SECOND time (now targeting the new current version) redacts that one too,
// without disturbing the first shredded version's already-redacted state.
func TestCipher_ShredThenEncryptThenShredAgain_OldCiphertextStaysRedactedNewOneWorks(t *testing.T) {
	store := newFakeKeyStore()
	c := newTestCipher(t, store)
	ctx := context.Background()
	tenant := "tenant-shred-lifecycle"

	ctBefore, err := c.Encrypt(ctx, tenant, "before first shred")
	if err != nil {
		t.Fatalf("Encrypt (v1): %v", err)
	}
	store.shredTenant(tenant) // destroys v1

	ctAfter, err := c.Encrypt(ctx, tenant, "after first shred")
	if err != nil {
		t.Fatalf("Encrypt after shred = %v, want success", err)
	}

	// v1's ciphertext is permanently redacted, even after the tenant has
	// moved on to a later version.
	pt, err := c.Decrypt(ctx, tenant, ctBefore)
	if err != nil {
		t.Fatalf("Decrypt v1 ciphertext after shred: %v", err)
	}
	if pt != crypto.RedactedMarker {
		t.Errorf("Decrypt v1 ciphertext after shred = %q, want %q", pt, crypto.RedactedMarker)
	}

	// v2's ciphertext (minted after the shred) reads back fine.
	pt, err = c.Decrypt(ctx, tenant, ctAfter)
	if err != nil {
		t.Fatalf("Decrypt v2 ciphertext: %v", err)
	}
	if pt != "after first shred" {
		t.Errorf("Decrypt v2 ciphertext = %q, want %q", pt, "after first shred")
	}

	// A second shred targets the NEW current version (v2), not v1 again.
	store.shredTenant(tenant)
	if pt, err := c.Decrypt(ctx, tenant, ctAfter); err != nil {
		t.Fatalf("Decrypt v2 ciphertext after second shred: %v", err)
	} else if pt != crypto.RedactedMarker {
		t.Errorf("Decrypt v2 ciphertext after second shred = %q, want %q", pt, crypto.RedactedMarker)
	}
	// v1 stays exactly as it was: still redacted, not somehow "re-shredded"
	// or restored.
	if pt, err := c.Decrypt(ctx, tenant, ctBefore); err != nil {
		t.Fatalf("Decrypt v1 ciphertext after second shred: %v", err)
	} else if pt != crypto.RedactedMarker {
		t.Errorf("Decrypt v1 ciphertext after second shred = %q, want %q", pt, crypto.RedactedMarker)
	}
}

func TestCipher_OtherTenantUnaffectedByShred(t *testing.T) {
	store := newFakeKeyStore()
	c := newTestCipher(t, store)
	ctx := context.Background()

	ctA, err := c.Encrypt(ctx, "tenant-a", "tenant a secret")
	if err != nil {
		t.Fatalf("Encrypt tenant-a: %v", err)
	}
	ctB, err := c.Encrypt(ctx, "tenant-b", "tenant b secret")
	if err != nil {
		t.Fatalf("Encrypt tenant-b: %v", err)
	}

	store.shredTenant("tenant-a")

	ptA, err := c.Decrypt(ctx, "tenant-a", ctA)
	if err != nil {
		t.Fatalf("Decrypt tenant-a after shred: %v", err)
	}
	if ptA != crypto.RedactedMarker {
		t.Errorf("tenant-a after its own shred = %q, want redacted marker", ptA)
	}

	ptB, err := c.Decrypt(ctx, "tenant-b", ctB)
	if err != nil {
		t.Fatalf("Decrypt tenant-b: %v", err)
	}
	if ptB != "tenant b secret" {
		t.Errorf("tenant-b after tenant-a's shred = %q, want unaffected %q", ptB, "tenant b secret")
	}
}

func TestCipher_DecryptTamperedCiphertextErrors(t *testing.T) {
	c := newTestCipher(t, newFakeKeyStore())
	ctx := context.Background()
	tenant := "tenant-a"

	ct, err := c.Encrypt(ctx, tenant, "rent payment")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	tampered := ct + "x"

	if pt, err := c.Decrypt(ctx, tenant, tampered); err == nil {
		t.Errorf("Decrypt(tampered) = %q, nil error; want a genuine decrypt failure", pt)
	}
}

func TestCipher_DecryptUnknownTenantErrors(t *testing.T) {
	store := newFakeKeyStore()
	c := newTestCipher(t, store)
	ctx := context.Background()

	// Encrypt for tenant-a so we have a real "enc:v1:" ciphertext, then ask a
	// tenant with NO crypto_keys row at all (not even shredded) to decrypt
	// it: an inconsistent state that must fail, not silently substitute.
	ct, err := c.Encrypt(ctx, "tenant-a", "rent payment")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := c.Decrypt(ctx, "tenant-never-seen", ct); err == nil {
		t.Error("Decrypt for a tenant with no key material at all = nil error, want an error")
	}
}

// alwaysShreddedMintStore wraps a fakeKeyStore but makes MintTenantDEKVersion
// report every freshly minted version as already shredded, simulating the
// adversarial (and, in real Postgres, astronomically unlikely) case of a
// shred call racing and winning against every single version Encrypt tries
// to mint: this is the one path that still surfaces ErrTenantKeyShredded
// after ADR-018's fix, bounded by crypto.mintRetryLimit.
type alwaysShreddedMintStore struct {
	*fakeKeyStore
}

func (s *alwaysShreddedMintStore) MintTenantDEKVersion(ctx context.Context, tenantID string, version int, candidate []byte) ([]byte, bool, error) {
	if _, _, err := s.fakeKeyStore.MintTenantDEKVersion(ctx, tenantID, version, candidate); err != nil {
		return nil, false, err
	}
	return nil, true, nil
}

// TestCipher_EncryptExhaustsMintRetriesReturnsErrTenantKeyShredded proves the
// one adversarial case ADR-018's fix cannot fully resolve (every version
// Encrypt tries to mint loses a race to a concurrent shred) still surfaces
// crypto.ErrTenantKeyShredded, matching FIX 2's defensive error mapping in
// internal/api's toHumaErr and internal/grpcserver's toStatus, rather than
// looping forever or returning a confusing wrapped error.
func TestCipher_EncryptExhaustsMintRetriesReturnsErrTenantKeyShredded(t *testing.T) {
	store := &alwaysShreddedMintStore{fakeKeyStore: newFakeKeyStore()}
	c := newTestCipher(t, store)
	ctx := context.Background()

	if _, err := c.Encrypt(ctx, "tenant-a", "new description"); !errors.Is(err, crypto.ErrTenantKeyShredded) {
		t.Errorf("Encrypt with every minted version reported shredded = %v, want ErrTenantKeyShredded", err)
	}
}

// TestCipher_ConcurrentFirstUse proves MintTenantDEKVersion's race is
// resolved to exactly one winning DEK: many goroutines racing Encrypt for the
// same brand-new tenant must all be decryptable by the SAME later Decrypt
// call, never a mix of two different keys. Run with -race.
func TestCipher_ConcurrentFirstUse(t *testing.T) {
	c := newTestCipher(t, newFakeKeyStore())
	ctx := context.Background()
	tenant := "tenant-concurrent"

	const n = 20
	results := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ct, err := c.Encrypt(ctx, tenant, "concurrent description")
			if err != nil {
				t.Errorf("Encrypt goroutine %d: %v", i, err)
				return
			}
			results[i] = ct
		}(i)
	}
	wg.Wait()

	for i, ct := range results {
		if ct == "" {
			continue // a failed goroutine already reported above
		}
		pt, err := c.Decrypt(ctx, tenant, ct)
		if err != nil {
			t.Errorf("Decrypt result %d: %v", i, err)
			continue
		}
		if pt != "concurrent description" {
			t.Errorf("Decrypt result %d = %q, want %q", i, pt, "concurrent description")
		}
	}
}
