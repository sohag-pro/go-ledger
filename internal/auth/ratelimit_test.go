package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// intp returns a pointer to n, for building domain.APIKey.RateLimitRPM
// literals in table tests.
func intp(n int) *int { return &n }

// rateLimitEchoOutput is a minimal huma response body for the rate limit
// tests' protected operation; its content does not matter, only whether the
// handler ran at all.
type rateLimitEchoOutput struct {
	Body struct {
		OK bool `json:"ok"`
	}
}

func TestNewLimiter_ClampsNonPositiveDefault(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   int
		want int
	}{
		{name: "positive default kept as-is", in: 45, want: 45},
		{name: "zero default clamped to builtin", in: 0, want: builtinDefaultRPM},
		{name: "negative default clamped to builtin", in: -30, want: builtinDefaultRPM},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l := NewLimiter(tt.in)
			if l.defaultRPM != tt.want {
				t.Errorf("defaultRPM = %d, want %d", l.defaultRPM, tt.want)
			}
		})
	}
}

func TestLimiter_AllowBurstThenBlocks(t *testing.T) {
	t.Parallel()

	// Each case exercises the RateLimitRPM narrowing directly: the effective
	// limit (and therefore the burst size, since burst == rpm) should be the
	// key's own override when it is a usable positive number, and the
	// limiter's default otherwise (nil, zero, or negative).
	tests := []struct {
		name       string
		rpm        *int
		defaultRPM int
		wantBurst  int
	}{
		{name: "key override applies", rpm: intp(3), defaultRPM: 120, wantBurst: 3},
		{name: "nil rpm falls back to default", rpm: nil, defaultRPM: 5, wantBurst: 5},
		{name: "zero rpm falls back to default", rpm: intp(0), defaultRPM: 4, wantBurst: 4},
		{name: "negative rpm falls back to default", rpm: intp(-10), defaultRPM: 6, wantBurst: 6},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l := NewLimiter(tt.defaultRPM)
			key := domain.APIKey{ID: "key-" + tt.name, TenantID: "tenant-1", RateLimitRPM: tt.rpm}

			for i := range tt.wantBurst {
				if !l.Allow(key) {
					t.Fatalf("request %d of burst %d was rejected, want allowed", i+1, tt.wantBurst)
				}
			}
			if l.Allow(key) {
				t.Fatalf("request %d beyond burst %d was allowed, want rejected (429)", tt.wantBurst+1, tt.wantBurst)
			}
		})
	}
}

func TestLimiter_IndependentBucketsPerKey(t *testing.T) {
	t.Parallel()

	l := NewLimiter(2)
	keyA := domain.APIKey{ID: "key-a", TenantID: "tenant-1"}
	keyB := domain.APIKey{ID: "key-b", TenantID: "tenant-1"}

	firstA, secondA := l.Allow(keyA), l.Allow(keyA)
	if !firstA || !secondA {
		t.Fatal("key A should get its full burst of 2")
	}
	if l.Allow(keyA) {
		t.Fatal("key A should be rate limited after exhausting its burst")
	}

	// Key B must not be affected by key A having exhausted its bucket: each
	// key gets its own *rate.Limiter, keyed by id.
	firstB, secondB := l.Allow(keyB), l.Allow(keyB)
	if !firstB || !secondB {
		t.Fatal("key B should have its own untouched burst of 2, independent of key A")
	}
	if l.Allow(keyB) {
		t.Fatal("key B should be rate limited after exhausting its own burst")
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rpm  int
		want int
	}{
		{name: "60 rpm refills every second", rpm: 60, want: 1},
		{name: "30 rpm refills every two seconds", rpm: 30, want: 2},
		{name: "120 rpm floors to at least one second", rpm: 120, want: 1},
		{name: "zero rpm clamps to default then floors to one second", rpm: 0, want: 1},
		{name: "negative rpm clamps to default then floors to one second", rpm: -5, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := retryAfterSeconds(tt.rpm); got != tt.want {
				t.Errorf("retryAfterSeconds(%d) = %d, want %d", tt.rpm, got, tt.want)
			}
		})
	}
}

// newRateLimitTestAPI builds a huma test API that stands in for the resolved
// key already being in context (as auth.HumaMiddleware would leave it after
// a successful resolve; that middleware is covered on its own in
// middleware_test.go) and installs l's middleware behind it, then registers
// one protected operation under /v1.
func newRateLimitTestAPI(t *testing.T, l *Limiter, key domain.APIKey) http.Handler {
	t.Helper()

	mux, api := humatest.New(t)

	api.UseMiddleware(func(ctx huma.Context, next func(huma.Context)) {
		next(huma.WithContext(ctx, WithKey(ctx.Context(), key)))
	})
	api.UseMiddleware(l.HumaMiddleware(api))

	huma.Register(api, huma.Operation{
		OperationID: "rate-limited-thing",
		Method:      http.MethodGet,
		Path:        "/v1/thing",
	}, func(_ context.Context, _ *struct{}) (*rateLimitEchoOutput, error) {
		out := &rateLimitEchoOutput{}
		out.Body.OK = true
		return out, nil
	})

	return mux
}

func TestHumaMiddleware_AllowsBurstThen429WithRetryAfter(t *testing.T) {
	t.Parallel()

	key := domain.APIKey{ID: "rl-key-1", TenantID: "tenant-1", RateLimitRPM: intp(2)}
	l := NewLimiter(120)
	mux := newRateLimitTestAPI(t, l, key)

	for i := range 2 {
		req := httptest.NewRequest(http.MethodGet, "/v1/thing", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 (%s)", i+1, rec.Code, rec.Body.String())
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/thing", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (%s)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("response missing Retry-After header")
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v (%q)", err, rec.Body.String())
	}
	if body["status"] != float64(429) {
		t.Errorf("body status = %v, want 429", body["status"])
	}
}

func TestHumaMiddleware_TwoKeysHaveIndependentBucketsThroughTheMiddleware(t *testing.T) {
	t.Parallel()

	l := NewLimiter(120)
	keyA := domain.APIKey{ID: "rl-key-a", TenantID: "tenant-a", RateLimitRPM: intp(1)}
	keyB := domain.APIKey{ID: "rl-key-b", TenantID: "tenant-b", RateLimitRPM: intp(1)}

	muxA := newRateLimitTestAPI(t, l, keyA)
	muxB := newRateLimitTestAPI(t, l, keyB)

	// Exhaust key A's single-request burst.
	rec := httptest.NewRecorder()
	muxA.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/thing", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("key A first request: status = %d, want 200", rec.Code)
	}
	rec = httptest.NewRecorder()
	muxA.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/thing", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("key A second request: status = %d, want 429", rec.Code)
	}

	// Key B must be unaffected by key A's exhausted bucket.
	rec = httptest.NewRecorder()
	muxB.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/thing", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("key B first request: status = %d, want 200 (independent bucket from key A)", rec.Code)
	}
}

func TestHumaMiddleware_NoKeyInContextAllowsThrough(t *testing.T) {
	t.Parallel()

	l := NewLimiter(120)
	mux, api := humatest.New(t)
	api.UseMiddleware(l.HumaMiddleware(api))

	huma.Register(api, huma.Operation{
		OperationID: "no-key-thing",
		Method:      http.MethodGet,
		Path:        "/healthz",
	}, func(_ context.Context, _ *struct{}) (*rateLimitEchoOutput, error) {
		out := &rateLimitEchoOutput{}
		out.Body.OK = true
		return out, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 when no key is in context (fail open)", rec.Code)
	}
}

func TestMiddleware_NetHTTPFallbackRateLimits(t *testing.T) {
	t.Parallel()

	key := domain.APIKey{ID: "rl-key-2", TenantID: "tenant-2", RateLimitRPM: intp(1)}
	l := NewLimiter(120)

	calls := 0
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	})
	handler := l.Middleware(inner)

	withKey := func() *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/v1/thing", nil)
		return req.WithContext(WithKey(req.Context(), key))
	}

	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, withKey())
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, withKey())
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429 (%s)", rec2.Code, rec2.Body.String())
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Errorf("response missing Retry-After header")
	}
	if rec2.Body.String() != `{"status":429,"title":"Too Many Requests"}` {
		t.Errorf("body = %q, want the exact 429 problem+json shape", rec2.Body.String())
	}

	if calls != 1 {
		t.Errorf("inner handler called %d times, want 1 (the rate limited request must never reach it)", calls)
	}
}

func TestLimiter_ConcurrentAccessIsRaceClean(t *testing.T) {
	t.Parallel()

	// A high default keeps most of these calls succeeding; the point of this
	// test is exercising concurrent map access under -race, not asserting an
	// exact allow/deny count.
	l := NewLimiter(100000)

	const goroutines = 50
	const requestsPerGoroutine = 20
	keyIDs := []string{"race-k1", "race-k2", "race-k3", "race-k4", "race-k5"}

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			key := domain.APIKey{ID: keyIDs[g%len(keyIDs)], TenantID: "tenant-race"}
			for range requestsPerGoroutine {
				l.Allow(key)
			}
		}(g)
	}
	wg.Wait()
}
