package grpcserver

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/sohag-pro/go-ledger/internal/auth"
	"github.com/sohag-pro/go-ledger/internal/domain"
)

// healthMethodPrefix is the gRPC health service's method prefix
// (grpc.health.v1.Health). Calls to it bypass authentication so liveness
// probes work without an API key, matching /healthz on the REST side.
const healthMethodPrefix = "/grpc.health.v1.Health/"

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
// unknown or empty key) is rejected with codes.Unauthenticated before the
// handler ever runs, with nothing logged. A *domain.TenantNotActiveError (the
// key is valid but its tenant is suspended or closed, ADR-015/Task 2.1) is
// rejected with codes.PermissionDenied instead, naming the reason, since the
// credential itself was fine. Any other resolver error is an unexpected infra
// failure (e.g. the key-lookup datastore is down): it is logged at error
// level with the method and the underlying cause, then rejected with
// codes.Internal, mirroring the REST auth middleware
// (internal/auth/middleware.go) so a backend outage does not read as "bad
// key". The token itself is never logged in either branch.
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

		ctx = auth.WithKey(auth.WithTenant(ctx, key.TenantID), key)
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
