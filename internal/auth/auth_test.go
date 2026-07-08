package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// fakeLookup is a keyLookup that serves a fixed set of hash to APIKey
// mappings and counts how many times it was called, so tests can assert the
// cache avoided (or did not avoid) a repository round trip.
type fakeLookup struct {
	mu    sync.Mutex
	keys  map[string]domain.APIKey
	calls int32
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

func (f *fakeLookup) callCount() int {
	return int(atomic.LoadInt32(&f.calls))
}

const testPlaintext = "glk_test-key-plaintext"

func testKey() (domain.APIKey, string) {
	key := domain.APIKey{ID: "key-1", TenantID: "tenant-1", Name: "test key"}
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
	if first != key {
		t.Fatalf("first Resolve = %+v, want %+v", first, key)
	}

	second, err := r.Resolve(context.Background(), "Bearer "+plaintext)
	if err != nil {
		t.Fatalf("second Resolve error = %v, want nil", err)
	}
	if second != key {
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

func TestResolve_PrefixOptional(t *testing.T) {
	t.Parallel()

	key, plaintext := testKey()
	lookup := newFakeLookup(map[string]domain.APIKey{domain.HashAPIKey(plaintext): key})
	r := NewResolver(lookup, time.Minute)

	withPrefix, err := r.Resolve(context.Background(), "Bearer "+plaintext)
	if err != nil {
		t.Fatalf("Resolve with Bearer prefix error = %v, want nil", err)
	}
	if withPrefix != key {
		t.Fatalf("Resolve with Bearer prefix = %+v, want %+v", withPrefix, key)
	}

	bare, err := r.Resolve(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("Resolve with bare token error = %v, want nil", err)
	}
	if bare != key {
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
			ID:       fmt.Sprintf("key-%d", i),
			TenantID: fmt.Sprintf("tenant-%d", i),
			Name:     fmt.Sprintf("key %d", i),
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
