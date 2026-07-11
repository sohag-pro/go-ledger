package grpcserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/sohag-pro/go-ledger/internal/auth"
	"github.com/sohag-pro/go-ledger/internal/domain"
)

// fakeLookup is a minimal keyLookup (see internal/auth) backed by a fixed
// hash to APIKey map, so authUnaryInterceptor can be tested against a real
// auth.Resolver without a database. If err is set, GetAPIKeyByHash returns it
// unconditionally instead of consulting keys, simulating an infra failure
// (e.g. the datastore backing the key lookup is down) that is distinct from
// domain.ErrAPIKeyNotFound.
type fakeLookup struct {
	keys map[string]domain.APIKey
	err  error
}

func (f *fakeLookup) GetAPIKeyByHash(_ context.Context, hash string) (domain.APIKey, error) {
	if f.err != nil {
		return domain.APIKey{}, f.err
	}
	k, ok := f.keys[hash]
	if !ok {
		return domain.APIKey{}, domain.ErrAPIKeyNotFound
	}
	return k, nil
}

// TouchAPIKeyLastUsed is a no-op: these interceptor tests exercise auth and
// scope enforcement, not the last-used throttle, which is covered in
// internal/auth's own tests.
func (f *fakeLookup) TouchAPIKeyLastUsed(_ context.Context, _ string, _ time.Time) error {
	return nil
}

const testPlaintextKey = "glk_interceptor-test-key" //nolint:gosec // test fixture key, not a real credential

// testKeyScopes is what an ordinary (pre-2.2b-admin) key carries: read and
// post, matching the demo and load-test keys' DB-default scopes (migration
// 0012). Tests that specifically exercise scope enforcement build their own
// key with a narrower or wider set instead of using this helper.
var testKeyScopes = []domain.Scope{domain.ScopeRead, domain.ScopePost}

func newTestResolver() *auth.Resolver {
	lookup := &fakeLookup{keys: map[string]domain.APIKey{
		domain.HashAPIKey(testPlaintextKey): {ID: "key-1", TenantID: "tenant-xyz", Name: "test", TenantStatus: domain.TenantActive, Scopes: testKeyScopes},
	}}
	return auth.NewResolver(lookup, time.Minute)
}

// newTestResolverWithStatus is newTestResolver but for a key whose tenant
// carries status, so tests can exercise the suspended/closed gate
// (Task 2.1, ADR-015).
func newTestResolverWithStatus(status domain.TenantStatus) *auth.Resolver {
	lookup := &fakeLookup{keys: map[string]domain.APIKey{
		domain.HashAPIKey(testPlaintextKey): {ID: "key-1", TenantID: "tenant-xyz", Name: "test", TenantStatus: status, Scopes: testKeyScopes},
	}}
	return auth.NewResolver(lookup, time.Minute)
}

// newTestResolverWithScopes returns a resolver whose sole key carries exactly
// scopes, so scope-enforcement tests can build a read-only, post-only, or
// admin key and exercise authUnaryInterceptor against it directly.
func newTestResolverWithScopes(scopes ...domain.Scope) *auth.Resolver {
	lookup := &fakeLookup{keys: map[string]domain.APIKey{
		domain.HashAPIKey(testPlaintextKey): {ID: "key-1", TenantID: "tenant-xyz", Name: "test", TenantStatus: domain.TenantActive, Scopes: scopes},
	}}
	return auth.NewResolver(lookup, time.Minute)
}

// newFailingTestResolver returns a resolver whose backing lookup always fails
// with a generic (non-auth) error, simulating a key-lookup datastore outage.
func newFailingTestResolver(cause error) *auth.Resolver {
	return auth.NewResolver(&fakeLookup{err: cause}, time.Minute)
}

// discardLogger is a *slog.Logger used by tests that do not care about log
// output, so authUnaryInterceptor's required logger parameter never panics
// on a nil receiver.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func ctxWithAuthMetadata(bearer string) context.Context {
	if bearer == "" {
		return context.Background()
	}
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", bearer))
}

func TestAuthInterceptorRejectsMissingMetadata(t *testing.T) {
	called := false
	handler := func(_ context.Context, _ any) (any, error) {
		called = true
		return nil, nil
	}
	interceptor := authUnaryInterceptor(newTestResolver(), discardLogger())
	_, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/ledger.v1.LedgerService/GetAccount"}, handler)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", status.Code(err))
	}
	if called {
		t.Error("handler should not run when authorization metadata is missing")
	}
}

func TestAuthInterceptorRejectsInvalidKey(t *testing.T) {
	called := false
	handler := func(_ context.Context, _ any) (any, error) {
		called = true
		return nil, nil
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	interceptor := authUnaryInterceptor(newTestResolver(), log)
	ctx := ctxWithAuthMetadata("Bearer glk_does-not-exist")
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/ledger.v1.LedgerService/GetAccount"}, handler)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", status.Code(err))
	}
	if called {
		t.Error("handler should not run with an invalid key")
	}
	if buf.Len() != 0 {
		t.Errorf("an unknown key is a normal auth rejection and should not be logged, got: %s", buf.String())
	}
	if strings.Contains(buf.String(), "glk_does-not-exist") {
		t.Error("the token must never be logged")
	}
}

func TestAuthInterceptorLogsAndRejectsInfraErrorAsInternal(t *testing.T) {
	called := false
	handler := func(_ context.Context, _ any) (any, error) {
		called = true
		return nil, nil
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	cause := errors.New("db down")
	interceptor := authUnaryInterceptor(newFailingTestResolver(cause), log)
	ctx := ctxWithAuthMetadata("Bearer " + testPlaintextKey)
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/ledger.v1.LedgerService/GetAccount"}, handler)
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
	if called {
		t.Error("handler should not run when the key resolver fails with an infra error")
	}
	logged := buf.String()
	if !strings.Contains(logged, "db down") {
		t.Fatalf("expected the underlying cause to be logged, got: %s", logged)
	}
	if !strings.Contains(logged, "/ledger.v1.LedgerService/GetAccount") {
		t.Fatalf("expected the method to be logged, got: %s", logged)
	}
	if strings.Contains(logged, testPlaintextKey) {
		t.Error("the token must never be logged")
	}
}

func TestAuthInterceptorInjectsTenantAndKeyForValidKey(t *testing.T) {
	var seenTenant string
	var seenKey domain.APIKey
	handler := func(ctx context.Context, _ any) (any, error) {
		seenTenant = tenantFrom(ctx)
		seenKey, _ = auth.KeyFromContext(ctx)
		return nil, nil
	}
	interceptor := authUnaryInterceptor(newTestResolver(), discardLogger())
	ctx := ctxWithAuthMetadata("Bearer " + testPlaintextKey)
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/ledger.v1.LedgerService/GetAccount"}, handler)
	if err != nil {
		t.Fatalf("interceptor returned error for a valid key: %v", err)
	}
	if seenTenant != "tenant-xyz" {
		t.Errorf("tenantFrom = %q, want tenant-xyz", seenTenant)
	}
	if seenKey.ID != "key-1" {
		t.Errorf("key id = %q, want key-1", seenKey.ID)
	}
}

// TestAuthInterceptorRejectsSuspendedOrClosedTenantAsPermissionDenied proves a
// valid key whose tenant is suspended or closed is rejected with
// codes.PermissionDenied, not codes.Unauthenticated: the credential itself is
// fine, only the tenant is gated (Task 2.1, ADR-015).
func TestAuthInterceptorRejectsSuspendedOrClosedTenantAsPermissionDenied(t *testing.T) {
	tenantStatuses := []domain.TenantStatus{domain.TenantSuspended, domain.TenantClosed}
	for _, tenantStatus := range tenantStatuses {
		t.Run(string(tenantStatus), func(t *testing.T) {
			called := false
			handler := func(_ context.Context, _ any) (any, error) {
				called = true
				return nil, nil
			}
			interceptor := authUnaryInterceptor(newTestResolverWithStatus(tenantStatus), discardLogger())
			ctx := ctxWithAuthMetadata("Bearer " + testPlaintextKey)
			_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/ledger.v1.LedgerService/GetAccount"}, handler)
			if status.Code(err) != codes.PermissionDenied {
				t.Fatalf("code = %v, want PermissionDenied", status.Code(err))
			}
			if err.Error() == "" || !strings.Contains(err.Error(), string(tenantStatus)) {
				t.Errorf("error = %v, want it to name the status %q", err, tenantStatus)
			}
			if called {
				t.Error("handler should not run for a suspended or closed tenant")
			}
		})
	}
}

func TestAuthInterceptorAllowsHealthCheckWithoutKey(t *testing.T) {
	called := false
	handler := func(ctx context.Context, _ any) (any, error) {
		called = true
		if tenant := tenantFrom(ctx); tenant != "" {
			t.Errorf("health check should not have a tenant, got %q", tenant)
		}
		return nil, nil
	}
	interceptor := authUnaryInterceptor(newTestResolver(), discardLogger())
	_, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/grpc.health.v1.Health/Check"}, handler)
	if err != nil {
		t.Fatalf("health check should be allowed through without a key: %v", err)
	}
	if !called {
		t.Error("handler should run for the health check even without authorization metadata")
	}
}

func TestRecoveryInterceptorTurnsPanicIntoInternal(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	handler := func(_ context.Context, _ any) (any, error) {
		panic("boom")
	}
	interceptor := recoveryUnaryInterceptor(log)
	_, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/x/Y"}, handler)
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
	logged := buf.String()
	if !strings.Contains(logged, "/x/Y") || !strings.Contains(logged, "panic") {
		t.Fatalf("expected panic log to mention method and panic, got: %s", logged)
	}
}

func TestLoggingInterceptorPassesThrough(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	want := errors.New("downstream")
	handler := func(_ context.Context, _ any) (any, error) { return "ok", want }
	interceptor := loggingUnaryInterceptor(log)
	resp, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/x/Y"}, handler)
	if resp != "ok" || !errors.Is(err, want) {
		t.Fatalf("logging interceptor altered the call: resp=%v err=%v", resp, err)
	}
}

// --- Scope enforcement (Task 2.2): these exercise authUnaryInterceptor
// itself, not just requiredGRPCScope in isolation. ---

func TestRequiredGRPCScope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		method string
		want   domain.Scope
	}{
		{"/ledger.v1.LedgerService/CreateAccount", domain.ScopePost},
		{"/ledger.v1.LedgerService/GetAccount", domain.ScopeRead},
		{"/ledger.v1.LedgerService/ListAccounts", domain.ScopeRead},
		{"/ledger.v1.LedgerService/GetBalance", domain.ScopeRead},
		{"/ledger.v1.LedgerService/GetStatement", domain.ScopeRead},
		{"/ledger.v1.LedgerService/PostTransaction", domain.ScopePost},
		{"/ledger.v1.LedgerService/Convert", domain.ScopePost},
		{"/ledger.v1.LedgerService/GetTransaction", domain.ScopeRead},
		{"/ledger.v1.LedgerService/ReverseTransaction", domain.ScopePost},
		{"/ledger.v1.LedgerService/GetTransactionAudit", domain.ScopeRead},
		{"/ledger.v1.LedgerService/GetAccountAudit", domain.ScopeRead},
		{"/ledger.v1.LedgerService/SomeFutureRPCNotYetMapped", domain.ScopePost},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			t.Parallel()
			if got := requiredGRPCScope(tt.method); got != tt.want {
				t.Errorf("requiredGRPCScope(%q) = %q, want %q", tt.method, got, tt.want)
			}
		})
	}
}

// TestAuthInterceptorReadOnlyKeyAllowedOnReadRPCRejectedOnWriteRPC proves the
// scope gate runs inside the real interceptor, not just requiredGRPCScope in
// isolation: a read-only key can call a read RPC (GetAccount) but is rejected
// with PermissionDenied on a write RPC (PostTransaction).
func TestAuthInterceptorReadOnlyKeyAllowedOnReadRPCRejectedOnWriteRPC(t *testing.T) {
	resolver := newTestResolverWithScopes(domain.ScopeRead)
	interceptor := authUnaryInterceptor(resolver, discardLogger())
	ctx := ctxWithAuthMetadata("Bearer " + testPlaintextKey)

	called := false
	handler := func(_ context.Context, _ any) (any, error) {
		called = true
		return nil, nil
	}

	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/ledger.v1.LedgerService/GetAccount"}, handler)
	if err != nil {
		t.Fatalf("read-only key on a read RPC: err = %v, want nil", err)
	}
	if !called {
		t.Error("handler should have run for a read-only key on a read RPC")
	}

	called = false
	_, err = interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/ledger.v1.LedgerService/PostTransaction"}, handler)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("read-only key on a write RPC: code = %v, want PermissionDenied", status.Code(err))
	}
	if called {
		t.Error("handler should not run for a read-only key on a write RPC")
	}
	if !strings.Contains(err.Error(), "post") {
		t.Errorf("error = %v, want it to name the missing scope (post)", err)
	}
}

// TestAuthInterceptorPostScopedKeyAllowedOnWriteRPC proves a post-scoped key
// (no read) can still call a write RPC.
func TestAuthInterceptorPostScopedKeyAllowedOnWriteRPC(t *testing.T) {
	resolver := newTestResolverWithScopes(domain.ScopePost)
	interceptor := authUnaryInterceptor(resolver, discardLogger())
	ctx := ctxWithAuthMetadata("Bearer " + testPlaintextKey)

	called := false
	handler := func(_ context.Context, _ any) (any, error) {
		called = true
		return nil, nil
	}

	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/ledger.v1.LedgerService/PostTransaction"}, handler)
	if err != nil {
		t.Fatalf("post-scoped key on a write RPC: err = %v, want nil", err)
	}
	if !called {
		t.Error("handler should have run for a post-scoped key on a write RPC")
	}
}

// --- Rate limiting (Task 5.1, audit A2.2): these exercise
// rateLimitUnaryInterceptor directly, the way the auth interceptor tests
// above exercise authUnaryInterceptor, rather than through a full bufconn
// server (server_test.go's TestGRPCRateLimit* covers the real chain wiring
// end to end). ---

// rateLimitTestKey is a fixed key id every rate-limit interceptor test below
// authenticates as unless a test specifically needs a second, independent
// key.
var rateLimitTestKey = domain.APIKey{ID: "rate-limit-key-1", TenantID: "tenant-rl"}

func rateLimitHandler(called *bool) grpc.UnaryHandler {
	return func(_ context.Context, _ any) (any, error) {
		*called = true
		return nil, nil
	}
}

func TestRateLimitInterceptorAllowsHealthCheckWithoutConsumingBudget(t *testing.T) {
	limiter := auth.NewLimiter(1) // burst of exactly 1
	interceptor := rateLimitUnaryInterceptor(limiter, discardLogger())
	ctx := auth.WithKey(context.Background(), rateLimitTestKey)

	for i := 0; i < 5; i++ {
		called := false
		_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: healthMethodPrefix + "Check"}, rateLimitHandler(&called))
		if err != nil {
			t.Fatalf("health check call %d: err = %v, want nil", i, err)
		}
		if !called {
			t.Errorf("health check call %d: handler should have run", i)
		}
	}
}

func TestRateLimitInterceptorAllowsThroughWithNoKeyInContext(t *testing.T) {
	limiter := auth.NewLimiter(1)
	interceptor := rateLimitUnaryInterceptor(limiter, discardLogger())

	called := false
	_, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/ledger.v1.LedgerService/GetAccount"}, rateLimitHandler(&called))
	if err != nil {
		t.Fatalf("no key in context: err = %v, want nil (defense-in-depth, not access control)", err)
	}
	if !called {
		t.Error("handler should run when no key is in context, the same fail-open stance as HumaMiddleware")
	}
}

// TestRateLimitInterceptorAllowsUnderLimitRejectsOverLimit proves a key within
// its burst passes through untouched, and the call past its burst is
// rejected with codes.ResourceExhausted carrying an errdetails.RetryInfo
// detail, without ever reaching the handler.
func TestRateLimitInterceptorAllowsUnderLimitRejectsOverLimit(t *testing.T) {
	limiter := auth.NewLimiter(2) // burst of exactly 2
	interceptor := rateLimitUnaryInterceptor(limiter, discardLogger())
	ctx := auth.WithKey(context.Background(), rateLimitTestKey)
	info := &grpc.UnaryServerInfo{FullMethod: "/ledger.v1.LedgerService/GetAccount"}

	for i := 0; i < 2; i++ {
		called := false
		_, err := interceptor(ctx, nil, info, rateLimitHandler(&called))
		if err != nil {
			t.Fatalf("call %d within burst: err = %v, want nil", i, err)
		}
		if !called {
			t.Errorf("call %d within burst: handler should have run", i)
		}
	}

	called := false
	_, err := interceptor(ctx, nil, info, rateLimitHandler(&called))
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("call past burst: code = %v, want ResourceExhausted", status.Code(err))
	}
	if called {
		t.Error("handler should not run once the key's budget is exhausted")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected a status error, got %v", err)
	}
	details := st.Details()
	if len(details) != 1 {
		t.Fatalf("expected exactly one detail, got %d: %v", len(details), details)
	}
	if _, ok := details[0].(*errdetails.RetryInfo); !ok {
		t.Errorf("detail = %T, want *errdetails.RetryInfo", details[0])
	}
}

// TestRateLimitInterceptorIndependentBucketsPerKey proves two distinct keys
// each get their own budget: exhausting one does not affect the other.
func TestRateLimitInterceptorIndependentBucketsPerKey(t *testing.T) {
	limiter := auth.NewLimiter(1) // burst of exactly 1 per key
	interceptor := rateLimitUnaryInterceptor(limiter, discardLogger())
	info := &grpc.UnaryServerInfo{FullMethod: "/ledger.v1.LedgerService/GetAccount"}

	keyA := domain.APIKey{ID: "rate-limit-key-A"}
	keyB := domain.APIKey{ID: "rate-limit-key-B"}
	ctxA := auth.WithKey(context.Background(), keyA)
	ctxB := auth.WithKey(context.Background(), keyB)

	calledA, calledB := false, false
	if _, err := interceptor(ctxA, nil, info, rateLimitHandler(&calledA)); err != nil {
		t.Fatalf("key A first call: err = %v, want nil", err)
	}
	if _, err := interceptor(ctxA, nil, info, rateLimitHandler(&calledA)); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("key A second call: code = %v, want ResourceExhausted", status.Code(err))
	}
	if _, err := interceptor(ctxB, nil, info, rateLimitHandler(&calledB)); err != nil {
		t.Fatalf("key B first call: err = %v, want nil (independent bucket from key A)", err)
	}
	if !calledA || !calledB {
		t.Errorf("calledA=%v calledB=%v, want both true from their own first call", calledA, calledB)
	}
}

// TestRateLimitInterceptorSharesBucketAcrossTransports proves the design
// decision documented on rateLimitUnaryInterceptor: passing the SAME
// *auth.Limiter instance REST enforces means a token consumed through
// auth.Limiter.Allow directly (standing in for a REST request having already
// spent the key's budget) is visible to the gRPC interceptor for the exact
// same key, so gRPC cannot be used to bypass the REST limit or vice versa.
func TestRateLimitInterceptorSharesBucketAcrossTransports(t *testing.T) {
	limiter := auth.NewLimiter(1) // burst of exactly 1
	key := domain.APIKey{ID: "shared-bucket-key"}

	// Simulate a REST request spending the key's entire budget.
	if !limiter.Allow(key) {
		t.Fatal("setup: REST-simulated Allow should have succeeded (fresh bucket)")
	}

	interceptor := rateLimitUnaryInterceptor(limiter, discardLogger())
	ctx := auth.WithKey(context.Background(), key)
	called := false
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/ledger.v1.LedgerService/GetAccount"}, rateLimitHandler(&called))
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("gRPC call after REST spent the budget: code = %v, want ResourceExhausted", status.Code(err))
	}
	if called {
		t.Error("handler should not run: the shared limiter had no budget left for this key")
	}
}

// TestAuthInterceptorAdminKeyAllowedOnAnyRPC proves ScopeAdmin is a superset
// (Task 2.2): a key carrying only ScopeAdmin can call both a read and a write
// RPC without also listing read/post.
func TestAuthInterceptorAdminKeyAllowedOnAnyRPC(t *testing.T) {
	resolver := newTestResolverWithScopes(domain.ScopeAdmin)
	interceptor := authUnaryInterceptor(resolver, discardLogger())
	ctx := ctxWithAuthMetadata("Bearer " + testPlaintextKey)

	handler := func(_ context.Context, _ any) (any, error) { return nil, nil }

	for _, method := range []string{
		"/ledger.v1.LedgerService/GetAccount",
		"/ledger.v1.LedgerService/PostTransaction",
	} {
		if _, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method}, handler); err != nil {
			t.Errorf("admin key on %s: err = %v, want nil", method, err)
		}
	}
}
