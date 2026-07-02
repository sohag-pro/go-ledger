package grpcserver

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type ctxKey int

const tenantKey ctxKey = iota

// tenantFrom returns the tenant id the tenant interceptor stored on the context,
// or "" if none. Handlers use it as REST handlers use deps.DefaultTenant.
func tenantFrom(ctx context.Context) string {
	if v, ok := ctx.Value(tenantKey).(string); ok {
		return v
	}
	return ""
}

// tenantUnaryInterceptor injects the tenant into the context for every call.
// For now it is a single configured default, the same seam as the REST layer;
// when auth lands it will resolve the tenant from a token here instead.
func tenantUnaryInterceptor(defaultTenant string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		return handler(context.WithValue(ctx, tenantKey, defaultTenant), req)
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
