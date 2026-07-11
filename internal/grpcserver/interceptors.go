package grpcserver

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/sohag-pro/go-ledger/internal/auth"
	"github.com/sohag-pro/go-ledger/internal/domain"
)

// healthMethodPrefix is the gRPC health service's method prefix
// (grpc.health.v1.Health). Calls to it bypass authentication so liveness
// probes work without an API key, matching /healthz on the REST side.
const healthMethodPrefix = "/grpc.health.v1.Health/"

// ledgerServiceMethodPrefix is prepended to each ledger.v1.LedgerService RPC
// name in grpcMethodScopes below, matching the full method string gRPC hands
// the server interceptor (e.g. "/ledger.v1.LedgerService/GetAccount").
const ledgerServiceMethodPrefix = "/ledger.v1.LedgerService/"

// grpcMethodScopes maps every ledger.v1.LedgerService RPC (proto/ledger/v1/ledger.proto)
// to the domain.Scope it requires (Task 2.2, audit A3.2/A2.3): read-only RPCs
// (Get*, List*, and the audit read RPCs) require domain.ScopeRead; every RPC
// that writes requires domain.ScopePost. A method not in this map defaults to
// domain.ScopePost in requiredGRPCScope below: the fail-closed choice, since
// treating an unrecognized RPC as read-only would be the wrong direction to
// fail in if the .proto grows a new mutating RPC that this map is not updated
// for.
var grpcMethodScopes = map[string]domain.Scope{
	ledgerServiceMethodPrefix + "CreateAccount":       domain.ScopePost,
	ledgerServiceMethodPrefix + "GetAccount":          domain.ScopeRead,
	ledgerServiceMethodPrefix + "SetAccountStatus":    domain.ScopePost,
	ledgerServiceMethodPrefix + "ListAccounts":        domain.ScopeRead,
	ledgerServiceMethodPrefix + "GetBalance":          domain.ScopeRead,
	ledgerServiceMethodPrefix + "GetStatement":        domain.ScopeRead,
	ledgerServiceMethodPrefix + "PostTransaction":     domain.ScopePost,
	ledgerServiceMethodPrefix + "Convert":             domain.ScopePost,
	ledgerServiceMethodPrefix + "GetTransaction":      domain.ScopeRead,
	ledgerServiceMethodPrefix + "ListTransactions":    domain.ScopeRead,
	ledgerServiceMethodPrefix + "ReverseTransaction":  domain.ScopePost,
	ledgerServiceMethodPrefix + "GetTransactionAudit": domain.ScopeRead,
	ledgerServiceMethodPrefix + "GetAccountAudit":     domain.ScopeRead,
}

// requiredGRPCScope returns the domain.Scope fullMethod requires, defaulting
// to domain.ScopePost for anything not in grpcMethodScopes.
func requiredGRPCScope(fullMethod string) domain.Scope {
	if scope, ok := grpcMethodScopes[fullMethod]; ok {
		return scope
	}
	return domain.ScopePost
}

// tenantFrom returns the tenant id the auth interceptor resolved from the
// caller's API key and stored on the context via auth.WithTenant, or "" if
// none (an unauthenticated call, e.g. the health check).
func tenantFrom(ctx context.Context) string {
	tenant, _ := auth.TenantFromContext(ctx)
	return tenant
}

// authUnaryInterceptor authenticates every unary call except the gRPC health
// check: it reads the "authorization" metadata value, resolves it to an API
// key through resolver, and on success injects the key's tenant and the key
// itself into the context (auth.WithTenant, auth.WithKey) before calling the
// handler. A missing token or an auth.ErrUnauthorized from resolver (an
// unknown, expired, or empty key) is rejected with codes.Unauthenticated
// before the handler ever runs, with nothing logged. A
// *domain.TenantNotActiveError (the key is valid but its tenant is suspended
// or closed, ADR-015/Task 2.1) is rejected with codes.PermissionDenied
// instead, naming the reason, since the credential itself was fine.
//
// Once a key resolves, its scope is checked against requiredGRPCScope(method)
// (Task 2.2): a key missing the required scope is also rejected with
// codes.PermissionDenied, naming the missing scope, the same shape as the
// tenant-status rejection, since again the credential is valid, it just
// lacks the scope the RPC needs.
//
// Any other resolver error is an unexpected infra failure (e.g. the
// key-lookup datastore is down): it is logged at error level with the method
// and the underlying cause, then rejected with codes.Internal, mirroring the
// REST auth middleware (internal/auth/middleware.go) so a backend outage does
// not read as "bad key". The token itself is never logged in either branch.
func authUnaryInterceptor(resolver *auth.Resolver, log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if strings.HasPrefix(info.FullMethod, healthMethodPrefix) {
			return handler(ctx, req)
		}

		var bearer string
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if vals := md.Get("authorization"); len(vals) > 0 {
				bearer = vals[0]
			}
		}

		key, err := resolver.Resolve(ctx, bearer)
		if err != nil {
			var tenantErr *domain.TenantNotActiveError
			if errors.As(err, &tenantErr) {
				return nil, status.Error(codes.PermissionDenied, tenantErr.Reason())
			}
			if errors.Is(err, auth.ErrUnauthorized) {
				return nil, status.Error(codes.Unauthenticated, "missing or invalid API key")
			}
			log.LogAttrs(ctx, slog.LevelError, "auth: resolve failed",
				slog.String("method", info.FullMethod), slog.String("error", err.Error()))
			return nil, status.Error(codes.Internal, "authentication backend error")
		}

		if scopeErr := auth.CheckScope(key, requiredGRPCScope(info.FullMethod)); scopeErr != nil {
			var insufficientErr *domain.InsufficientScopeError
			errors.As(scopeErr, &insufficientErr)
			return nil, status.Error(codes.PermissionDenied, insufficientErr.Reason())
		}

		ctx = auth.WithKey(auth.WithTenant(ctx, key.TenantID), key)
		return handler(ctx, req)
	}
}

// rateLimitUnaryInterceptor enforces limiter against the calling API key for
// every unary call except the gRPC health check (Task 5.1, audit A2.2),
// bringing gRPC to parity with REST's auth.Limiter.HumaMiddleware. It must
// run AFTER authUnaryInterceptor in the chain (see NewGRPCServer), so that
// auth.KeyFromContext has a resolved key to find, the same ordering
// HumaMiddleware depends on relative to the REST auth middleware.
//
// limiter is the SAME *auth.Limiter instance the REST layer enforces,
// constructed once in cmd/server/main.go and passed to both api.Deps and
// grpcserver.Deps: a key's per-minute budget is one shared token bucket
// across both transports, so a client cannot spend a fresh budget on gRPC
// after exhausting it on REST, or vice versa. Two different keys still get
// fully independent buckets (auth.Limiter is keyed by APIKey.ID).
//
// Like HumaMiddleware, a missing key in context is let through rather than
// failed closed: rate limiting is defense-in-depth on top of authentication,
// not itself an access control, and authUnaryInterceptor is the component
// that rejects an unauthenticated call. By the time a non-health call
// reaches this interceptor, authUnaryInterceptor has always set a key on the
// path that got here, so this branch is only ever reached by a future
// ordering bug in NewGRPCServer's chain, not by normal traffic.
//
// Over budget -> codes.ResourceExhausted, carrying an errdetails.RetryInfo
// with the SAME Retry-After value (via auth.Limiter.RetryAfterSeconds) REST
// sends as a header, so a well-behaved gRPC client can back off with an
// actual number instead of guessing.
func rateLimitUnaryInterceptor(limiter *auth.Limiter, log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if strings.HasPrefix(info.FullMethod, healthMethodPrefix) {
			return handler(ctx, req)
		}

		key, ok := auth.KeyFromContext(ctx)
		if !ok {
			return handler(ctx, req)
		}

		if !limiter.Allow(key) {
			st := status.New(codes.ResourceExhausted, "rate limit exceeded")
			retryAfter := limiter.RetryAfterSeconds(key)
			if withDetail, err := st.WithDetails(&errdetails.RetryInfo{
				RetryDelay: durationpb.New(time.Duration(retryAfter) * time.Second),
			}); err == nil {
				st = withDetail
			} else {
				// WithDetails only fails if the detail message cannot be
				// marshaled into an Any, which cannot happen for a
				// well-formed RetryInfo; log it and still return the
				// undecorated status rather than dropping the rejection.
				log.LogAttrs(ctx, slog.LevelWarn, "rate limit: failed to attach RetryInfo detail",
					slog.String("method", info.FullMethod), slog.String("error", err.Error()))
			}
			return nil, st.Err()
		}

		return handler(ctx, req)
	}
}

// recoveryUnaryInterceptor turns a panic in a handler into a codes.Internal
// error instead of tearing down the connection. It sits outermost in the
// chain, so it logs the panic itself instead of relying on the logging
// interceptor, which never runs once a panic unwinds past it.
func recoveryUnaryInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				log.LogAttrs(ctx, slog.LevelError, "grpc handler panic",
					slog.String("method", info.FullMethod),
					slog.Any("panic", r),
					slog.String("stack", string(debug.Stack())),
				)
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return handler(ctx, req)
	}
}

// loggingUnaryInterceptor logs one structured line per RPC (method, code,
// duration), mirroring the REST request-logging middleware. It never alters the
// response or error.
func loggingUnaryInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		log.LogAttrs(ctx, slog.LevelInfo, "grpc request",
			slog.String("method", info.FullMethod),
			slog.String("code", status.Code(err).String()),
			slog.Duration("elapsed", time.Since(start)),
		)
		return resp, err
	}
}
