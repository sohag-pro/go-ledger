package auth

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// fakeLookup is a keyLookup that serves a fixed set of hash to APIKey
// mappings and counts how many times it was called, so tests can assert the
// cache avoided (or did not avoid) a repository round trip. It also records
// every TouchAPIKeyLastUsed call, so tests can assert the last-used throttle
// fires at most once per window instead of once per request.
type fakeLookup struct {
	mu         sync.Mutex
	keys       map[string]domain.APIKey
	calls      int32
	touches    []touchCall
	touchErr   error
	touchCalls int32
	// touchNotify, when non-nil, receives one value per TouchAPIKeyLastUsed
	// call: since Resolve fires the touch from a detached goroutine, a test
	// asserting on touchCount needs a deterministic way to wait for it rather
	// than sleeping and hoping the goroutine has run.
	touchNotify chan struct{}
}

// touchCall records one TouchAPIKeyLastUsed invocation.
type touchCall struct {
	id   string
	when time.Time
}

// keyEqualIgnoringLastUsed reports whether a and b are equal except for
// LastUsedAt: a successful Resolve may populate LastUsedAt via the
// best-effort throttled touch (Task 2.2), which most of these tests are not
// about.
func keyEqualIgnoringLastUsed(a, b domain.APIKey) bool {
	a.LastUsedAt = nil
	b.LastUsedAt = nil
	return reflect.DeepEqual(a, b)
}

func newFakeLookup(keys map[string]domain.APIKey) *fakeLookup {
	return &fakeLookup{keys: keys}
}

func (f *fakeLookup) GetAPIKeyByHash(_ context.Context, hash string) (domain.APIKey, error) {
	atomic.AddInt32(&f.calls, 1)

	f.mu.Lock()
	defer f.mu.Unlock()
	if k, ok := f.keys[hash]; ok {
		return k, nil
	}
	return domain.APIKey{}, domain.ErrAPIKeyNotFound
}

func (f *fakeLookup) TouchAPIKeyLastUsed(_ context.Context, id string, when time.Time) error {
	atomic.AddInt32(&f.touchCalls, 1)

	f.mu.Lock()
	f.touches = append(f.touches, touchCall{id: id, when: when})
	notify := f.touchNotify
	err := f.touchErr
	f.mu.Unlock()

	if notify != nil {
		notify <- struct{}{}
	}
	return err
}

// waitForTouch blocks until at least one TouchAPIKeyLastUsed call has been
// recorded, or fails the test after a generous timeout: f must have been
// constructed with a buffered touchNotify channel.
func (f *fakeLookup) waitForTouch(t *testing.T) {
	t.Helper()
	select {
	case <-f.touchNotify:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an asynchronous TouchAPIKeyLastUsed call")
	}
}

func (f *fakeLookup) callCount() int {
	return int(atomic.LoadInt32(&f.calls))
}

// touchCount returns how many times TouchAPIKeyLastUsed was called,
// synchronized the same way callCount is.
func (f *fakeLookup) touchCount() int {
	return int(atomic.LoadInt32(&f.touchCalls))
}

// setKey replaces the stored key for hash, simulating a tenant's status
// changing between one lookup and the next (e.g. an operator suspending a
// tenant): the fake's caller is expected to have already advanced r.now past
// the cached entry's TTL, since a real database update only becomes visible
// to Resolve on its next fetch, never mid-TTL.
func (f *fakeLookup) setKey(hash string, k domain.APIKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[hash] = k
}

const testPlaintext = "glk_test-key-plaintext"

func testKey() (domain.APIKey, string) {
	key := domain.APIKey{ID: "key-1", TenantID: "tenant-1", Name: "test key", TenantStatus: domain.TenantActive}
	return key, testPlaintext
}

func TestResolve_CacheHitAvoidsSecondLookup(t *testing.T) {
	t.Parallel()

	key, plaintext := testKey()
	lookup := newFakeLookup(map[string]domain.APIKey{domain.HashAPIKey(plaintext): key})
	r := NewResolver(lookup, time.Minute)

	first, err := r.Resolve(context.Background(), "Bearer "+plaintext)
	if err != nil {
		t.Fatalf("first Resolve error = %v, want nil", err)
	}
	if !keyEqualIgnoringLastUsed(first, key) {
		t.Fatalf("first Resolve = %+v, want %+v", first, key)
	}

	second, err := r.Resolve(context.Background(), "Bearer "+plaintext)
	if err != nil {
		t.Fatalf("second Resolve error = %v, want nil", err)
	}
	if !keyEqualIgnoringLastUsed(second, key) {
		t.Fatalf("second Resolve = %+v, want %+v", second, key)
	}

	if got := lookup.callCount(); got != 1 {
		t.Fatalf("lookup call count = %d, want 1 (second Resolve should hit cache)", got)
	}
}

func TestResolve_ExpiredCacheEntryRefetches(t *testing.T) {
	t.Parallel()

	key, plaintext := testKey()
	lookup := newFakeLookup(map[string]domain.APIKey{domain.HashAPIKey(plaintext): key})
	r := NewResolver(lookup, time.Second)

	current := time.Now()
	r.now = func() time.Time { return current }

	if _, err := r.Resolve(context.Background(), plaintext); err != nil {
		t.Fatalf("first Resolve error = %v, want nil", err)
	}
	if got := lookup.callCount(); got != 1 {
		t.Fatalf("lookup call count after first Resolve = %d, want 1", got)
	}

	// Advance the injected clock past the TTL: no real sleep needed.
	current = current.Add(2 * time.Second)

	if _, err := r.Resolve(context.Background(), plaintext); err != nil {
		t.Fatalf("second Resolve (after expiry) error = %v, want nil", err)
	}
	if got := lookup.callCount(); got != 2 {
		t.Fatalf("lookup call count after expiry = %d, want 2 (expired entry should refetch)", got)
	}
}

func TestResolve_StillCachedBeforeExpiry(t *testing.T) {
	t.Parallel()

	key, plaintext := testKey()
	lookup := newFakeLookup(map[string]domain.APIKey{domain.HashAPIKey(plaintext): key})
	r := NewResolver(lookup, time.Second)

	current := time.Now()
	r.now = func() time.Time { return current }

	if _, err := r.Resolve(context.Background(), plaintext); err != nil {
		t.Fatalf("first Resolve error = %v, want nil", err)
	}

	current = current.Add(500 * time.Millisecond)

	if _, err := r.Resolve(context.Background(), plaintext); err != nil {
		t.Fatalf("second Resolve (before expiry) error = %v, want nil", err)
	}
	if got := lookup.callCount(); got != 1 {
		t.Fatalf("lookup call count before expiry = %d, want 1 (entry should still be cached)", got)
	}
}

func TestResolve_EmptyOrGarbageToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		bearer string
	}{
		{"empty string", ""},
		{"whitespace only", "   "},
		{"bearer scheme with nothing after", "Bearer "},
		{"bearer scheme, no trailing space, nothing after", "Bearer"},
		{"bearer scheme with only whitespace after", "Bearer    "},
		{"lowercase bearer scheme with nothing after", "bearer "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			lookup := newFakeLookup(nil)
			r := NewResolver(lookup, time.Minute)

			_, err := r.Resolve(context.Background(), tt.bearer)
			if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("Resolve(%q) error = %v, want ErrUnauthorized", tt.bearer, err)
			}
			if got := lookup.callCount(); got != 0 {
				t.Fatalf("Resolve(%q) called lookup %d times, want 0", tt.bearer, got)
			}
		})
	}
}

func TestResolve_UnknownKey(t *testing.T) {
	t.Parallel()

	lookup := newFakeLookup(nil) // no keys primed: every hash is unknown
	r := NewResolver(lookup, time.Minute)

	_, err := r.Resolve(context.Background(), "Bearer glk_never-issued")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Resolve error = %v, want ErrUnauthorized", err)
	}
	if got := lookup.callCount(); got != 1 {
		t.Fatalf("lookup call count = %d, want 1", got)
	}
}

func TestResolve_LookupErrorOtherThanNotFound(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("db unavailable")
	r := NewResolver(errLookup{err: wantErr}, time.Minute)

	_, err := r.Resolve(context.Background(), "Bearer glk_whatever")
	if err == nil || errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Resolve error = %v, want a wrapped non-ErrUnauthorized error", err)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("Resolve error = %v, want it to wrap %v", err, wantErr)
	}
}

type errLookup struct{ err error }

func (e errLookup) GetAPIKeyByHash(_ context.Context, _ string) (domain.APIKey, error) {
	return domain.APIKey{}, e.err
}

func (errLookup) TouchAPIKeyLastUsed(_ context.Context, _ string, _ time.Time) error {
	return nil
}

func TestResolve_PrefixOptional(t *testing.T) {
	t.Parallel()

	key, plaintext := testKey()
	lookup := newFakeLookup(map[string]domain.APIKey{domain.HashAPIKey(plaintext): key})
	r := NewResolver(lookup, time.Minute)

	withPrefix, err := r.Resolve(context.Background(), "Bearer "+plaintext)
	if err != nil {
		t.Fatalf("Resolve with Bearer prefix error = %v, want nil", err)
	}
	if !keyEqualIgnoringLastUsed(withPrefix, key) {
		t.Fatalf("Resolve with Bearer prefix = %+v, want %+v", withPrefix, key)
	}

	bare, err := r.Resolve(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("Resolve with bare token error = %v, want nil", err)
	}
	if !keyEqualIgnoringLastUsed(bare, key) {
		t.Fatalf("Resolve with bare token = %+v, want %+v", bare, key)
	}
}

func TestResolve_ConcurrentSameAndDifferentTokens(t *testing.T) {
	const numKeys = 5
	const goroutinesPerKey = 20

	keys := make(map[string]domain.APIKey, numKeys)
	plaintexts := make([]string, numKeys)
	for i := 0; i < numKeys; i++ {
		p := fmt.Sprintf("glk_concurrent-key-%d", i)
		plaintexts[i] = p
		keys[domain.HashAPIKey(p)] = domain.APIKey{
			ID:           fmt.Sprintf("key-%d", i),
			TenantID:     fmt.Sprintf("tenant-%d", i),
			Name:         fmt.Sprintf("key %d", i),
			TenantStatus: domain.TenantActive,
		}
	}
	lookup := newFakeLookup(keys)
	r := NewResolver(lookup, time.Minute)

	var wg sync.WaitGroup
	errCh := make(chan error, numKeys*goroutinesPerKey)

	for i := 0; i < numKeys; i++ {
		plaintext := plaintexts[i]
		wantTenant := keys[domain.HashAPIKey(plaintext)].TenantID
		for j := 0; j < goroutinesPerKey; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, err := r.Resolve(context.Background(), "Bearer "+plaintext)
				if err != nil {
					errCh <- fmt.Errorf("Resolve(%q) error = %w", plaintext, err)
					return
				}
				if got.TenantID != wantTenant {
					errCh <- fmt.Errorf("Resolve(%q) tenant = %q, want %q", plaintext, got.TenantID, wantTenant)
				}
			}()
		}
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Error(err)
	}
}

// --- Tenant status gating (Task 2.1, ADR-015). ---

// TestResolve_ActiveTenantPasses proves the ordinary case is unaffected: a
// key whose tenant is active resolves exactly as before.
func TestResolve_ActiveTenantPasses(t *testing.T) {
	t.Parallel()

	key, plaintext := testKey() // TenantStatus: domain.TenantActive
	lookup := newFakeLookup(map[string]domain.APIKey{domain.HashAPIKey(plaintext): key})
	r := NewResolver(lookup, time.Minute)

	got, err := r.Resolve(context.Background(), "Bearer "+plaintext)
	if err != nil {
		t.Fatalf("Resolve error = %v, want nil", err)
	}
	if !keyEqualIgnoringLastUsed(got, key) {
		t.Fatalf("Resolve = %+v, want %+v", got, key)
	}
}

// TestResolve_SuspendedAndClosedTenantsAreRejected proves a valid key whose
// tenant is suspended or closed is rejected with a *domain.TenantNotActiveError
// (matched via errors.Is(err, domain.ErrTenantNotActive)), not ErrUnauthorized:
// the credential itself is fine, only the tenant is gated, and the error names
// the exact status so a transport layer can put it in a 403 / PermissionDenied
// response.
func TestResolve_SuspendedAndClosedTenantsAreRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status domain.TenantStatus
	}{
		{"suspended", domain.TenantSuspended},
		{"closed", domain.TenantClosed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			key := domain.APIKey{ID: "key-1", TenantID: "tenant-1", Name: "test key", TenantStatus: tt.status}
			lookup := newFakeLookup(map[string]domain.APIKey{domain.HashAPIKey(testPlaintext): key})
			r := NewResolver(lookup, time.Minute)

			_, err := r.Resolve(context.Background(), "Bearer "+testPlaintext)
			if err == nil {
				t.Fatal("Resolve error = nil, want a TenantNotActiveError")
			}
			if errors.Is(err, ErrUnauthorized) {
				t.Errorf("Resolve error = %v, want a TenantNotActiveError, not ErrUnauthorized (the key is valid)", err)
			}
			if !errors.Is(err, domain.ErrTenantNotActive) {
				t.Fatalf("Resolve error = %v, want it to match domain.ErrTenantNotActive", err)
			}
			var tenantErr *domain.TenantNotActiveError
			if !errors.As(err, &tenantErr) {
				t.Fatalf("Resolve error = %v (%T), want a *domain.TenantNotActiveError", err, err)
			}
			if tenantErr.Status != tt.status {
				t.Errorf("TenantNotActiveError.Status = %q, want %q", tenantErr.Status, tt.status)
			}
			wantReason := "tenant is " + string(tt.status)
			if got := tenantErr.Reason(); got != wantReason {
				t.Errorf("Reason() = %q, want %q", got, wantReason)
			}
		})
	}
}

// TestResolve_SuspensionIsPickedUpWithinOneTTL proves the cache does not let a
// suspended tenant's key keep working past its cache entry's TTL: gating is
// folded into the same cache entry as the key, so it is checked on a cache hit
// (not just a fresh lookup), and a tenant suspended after the entry was cached
// is rejected the next time the entry expires and is refetched.
func TestResolve_SuspensionIsPickedUpWithinOneTTL(t *testing.T) {
	t.Parallel()

	activeKey := domain.APIKey{ID: "key-1", TenantID: "tenant-1", Name: "test key", TenantStatus: domain.TenantActive}
	lookup := newFakeLookup(map[string]domain.APIKey{domain.HashAPIKey(testPlaintext): activeKey})
	r := NewResolver(lookup, time.Second)

	current := time.Now()
	r.now = func() time.Time { return current }

	if _, err := r.Resolve(context.Background(), testPlaintext); err != nil {
		t.Fatalf("first Resolve (active) error = %v, want nil", err)
	}
	if got := lookup.callCount(); got != 1 {
		t.Fatalf("lookup call count after first Resolve = %d, want 1", got)
	}

	// The tenant is suspended in the backing store, but the cache entry has
	// not expired yet: Resolve must still see the pre-suspension cached
	// entry, exactly like the key-only cache-hit behavior this gating sits on
	// top of.
	suspendedKey := activeKey
	suspendedKey.TenantStatus = domain.TenantSuspended
	lookup.setKey(domain.HashAPIKey(testPlaintext), suspendedKey)

	if _, err := r.Resolve(context.Background(), testPlaintext); err != nil {
		t.Fatalf("second Resolve (still cached, pre-suspension) error = %v, want nil", err)
	}
	if got := lookup.callCount(); got != 1 {
		t.Fatalf("lookup call count while still cached = %d, want 1 (no refetch yet)", got)
	}

	// Advance past the TTL: the next Resolve refetches and must now see the
	// suspension.
	current = current.Add(2 * time.Second)

	_, err := r.Resolve(context.Background(), testPlaintext)
	if !errors.Is(err, domain.ErrTenantNotActive) {
		t.Fatalf("Resolve after TTL expiry (suspended) error = %v, want a TenantNotActiveError", err)
	}
	if got := lookup.callCount(); got != 2 {
		t.Fatalf("lookup call count after TTL expiry = %d, want 2 (expired entry should refetch)", got)
	}

	// And the rejection itself is also cached: a third call within the new
	// TTL window must not hit the database again.
	_, err = r.Resolve(context.Background(), testPlaintext)
	if !errors.Is(err, domain.ErrTenantNotActive) {
		t.Fatalf("third Resolve (still within post-suspension TTL) error = %v, want a TenantNotActiveError", err)
	}
	if got := lookup.callCount(); got != 2 {
		t.Fatalf("lookup call count for cached suspended entry = %d, want 2 (no extra refetch)", got)
	}
}

// --- Expiry (Task 2.2). ---

// TestResolve_ExpiredKeyIsUnauthorized proves an expired key is rejected the
// same way as an unknown or revoked one: ErrUnauthorized, revealing nothing
// more to the caller. domain.ErrAPIKeyExpired is also reachable via
// errors.Is for logging/tests, but the transport layers never distinguish it
// in the response body.
func TestResolve_ExpiredKeyIsUnauthorized(t *testing.T) {
	t.Parallel()

	expiredAt := time.Now().Add(-time.Minute)
	key := domain.APIKey{ID: "key-1", TenantID: "tenant-1", Name: "expired key", TenantStatus: domain.TenantActive, ExpiresAt: &expiredAt}
	lookup := newFakeLookup(map[string]domain.APIKey{domain.HashAPIKey(testPlaintext): key})
	r := NewResolver(lookup, time.Minute)

	_, err := r.Resolve(context.Background(), "Bearer "+testPlaintext)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Resolve error = %v, want ErrUnauthorized (expired key)", err)
	}
	if !errors.Is(err, domain.ErrAPIKeyExpired) {
		t.Fatalf("Resolve error = %v, want it to also match domain.ErrAPIKeyExpired", err)
	}
}

// TestResolve_LiveKeyWithFutureExpiryResolves proves a key with an expiry
// still in the future resolves exactly like a key with no expiry at all.
func TestResolve_LiveKeyWithFutureExpiryResolves(t *testing.T) {
	t.Parallel()

	future := time.Now().Add(time.Hour)
	key := domain.APIKey{ID: "key-1", TenantID: "tenant-1", Name: "test", TenantStatus: domain.TenantActive, ExpiresAt: &future}
	lookup := newFakeLookup(map[string]domain.APIKey{domain.HashAPIKey(testPlaintext): key})
	r := NewResolver(lookup, time.Minute)

	got, err := r.Resolve(context.Background(), "Bearer "+testPlaintext)
	if err != nil {
		t.Fatalf("Resolve error = %v, want nil", err)
	}
	if got.ID != key.ID {
		t.Fatalf("Resolve = %+v, want %+v", got, key)
	}
}

// TestResolve_KeyExpiresWhileCached proves the cache never trusts a key past
// its own ExpiresAt, even when that is sooner than the resolver's TTL:
// cachePut caps the cache entry's expiry at min(ttl, key.ExpiresAt), so the
// entry stops being served the instant the key itself expires rather than
// only at the end of the (much longer) TTL window.
func TestResolve_KeyExpiresWhileCached(t *testing.T) {
	t.Parallel()

	current := time.Now()
	expiresAt := current.Add(500 * time.Millisecond)
	key := domain.APIKey{ID: "key-1", TenantID: "tenant-1", Name: "test", TenantStatus: domain.TenantActive, ExpiresAt: &expiresAt}
	lookup := newFakeLookup(map[string]domain.APIKey{domain.HashAPIKey(testPlaintext): key})
	r := NewResolver(lookup, time.Minute) // TTL far longer than the key's own expiry
	r.now = func() time.Time { return current }

	if _, err := r.Resolve(context.Background(), testPlaintext); err != nil {
		t.Fatalf("first Resolve (before expiry) error = %v, want nil", err)
	}
	if got := lookup.callCount(); got != 1 {
		t.Fatalf("lookup call count = %d, want 1", got)
	}

	// Advance past the key's own expiry, but nowhere near the (much longer)
	// cache TTL: if the cache entry were not capped, this would still be
	// served from cache as if the key were valid.
	current = current.Add(time.Second)

	_, err := r.Resolve(context.Background(), testPlaintext)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Resolve after key expiry error = %v, want ErrUnauthorized", err)
	}
	if got := lookup.callCount(); got != 2 {
		t.Fatalf("lookup call count after key expiry = %d, want 2 (the capped cache entry should have forced a refetch)", got)
	}
}

// --- last_used_at touch, throttled (Task 2.2). ---

// TestResolve_TouchesLastUsedOnceWithinThrottleWindow proves a successful
// Resolve fires exactly one TouchAPIKeyLastUsed for a burst of calls within
// the throttle window, not one per call.
func TestResolve_TouchesLastUsedOnceWithinThrottleWindow(t *testing.T) {
	t.Parallel()

	key, plaintext := testKey()
	lookup := newFakeLookup(map[string]domain.APIKey{domain.HashAPIKey(plaintext): key})
	lookup.touchNotify = make(chan struct{}, 10)
	r := NewResolver(lookup, time.Minute)

	if _, err := r.Resolve(context.Background(), "Bearer "+plaintext); err != nil {
		t.Fatalf("first Resolve error = %v, want nil", err)
	}
	lookup.waitForTouch(t)

	for i := 0; i < 5; i++ {
		if _, err := r.Resolve(context.Background(), "Bearer "+plaintext); err != nil {
			t.Fatalf("Resolve #%d error = %v, want nil", i, err)
		}
	}

	// Give any (incorrect) extra touch a moment to arrive before asserting it
	// didn't: the throttle window (60s) dwarfs this test's real wall-clock
	// runtime, so a second touch here would be a bug, not a timing fluke.
	select {
	case <-lookup.touchNotify:
		t.Fatal("a Resolve within the throttle window fired a second touch")
	case <-time.After(200 * time.Millisecond):
	}

	if got := lookup.touchCount(); got != 1 {
		t.Errorf("touch count = %d, want 1", got)
	}
}

// TestResolve_TouchFiresAgainAfterThrottleWindow proves the throttle is a
// window, not a permanent latch: once touchThrottle has elapsed, the next
// successful Resolve touches last_used_at again.
func TestResolve_TouchFiresAgainAfterThrottleWindow(t *testing.T) {
	t.Parallel()

	key, plaintext := testKey()
	lookup := newFakeLookup(map[string]domain.APIKey{domain.HashAPIKey(plaintext): key})
	lookup.touchNotify = make(chan struct{}, 10)
	r := NewResolver(lookup, time.Hour) // cache TTL long enough to isolate the throttle from cache expiry
	current := time.Now()
	r.now = func() time.Time { return current }

	if _, err := r.Resolve(context.Background(), plaintext); err != nil {
		t.Fatalf("first Resolve error = %v, want nil", err)
	}
	lookup.waitForTouch(t)

	current = current.Add(2 * time.Minute) // past the throttle window

	if _, err := r.Resolve(context.Background(), plaintext); err != nil {
		t.Fatalf("second Resolve error = %v, want nil", err)
	}
	lookup.waitForTouch(t)

	if got := lookup.touchCount(); got != 2 {
		t.Errorf("touch count = %d, want 2 (throttle window elapsed)", got)
	}
}

// TestResolve_TouchErrorDoesNotFailTheRequest proves a failing
// TouchAPIKeyLastUsed (e.g. the database is briefly unavailable) never
// surfaces as a Resolve error: the touch is best-effort.
func TestResolve_TouchErrorDoesNotFailTheRequest(t *testing.T) {
	t.Parallel()

	key, plaintext := testKey()
	lookup := newFakeLookup(map[string]domain.APIKey{domain.HashAPIKey(plaintext): key})
	lookup.touchErr = errors.New("touch failed")
	lookup.touchNotify = make(chan struct{}, 10)
	r := NewResolver(lookup, time.Minute)

	got, err := r.Resolve(context.Background(), "Bearer "+plaintext)
	if err != nil {
		t.Fatalf("Resolve error = %v, want nil even though the touch fails", err)
	}
	if !keyEqualIgnoringLastUsed(got, key) {
		t.Fatalf("Resolve = %+v, want %+v", got, key)
	}
	lookup.waitForTouch(t) // the touch was still attempted despite failing
}
