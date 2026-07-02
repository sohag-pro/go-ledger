package grpcserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestTenantInterceptorInjectsTenant(t *testing.T) {
	var seen string
	handler := func(ctx context.Context, _ any) (any, error) {
		seen = tenantFrom(ctx)
		return nil, nil
	}
	interceptor := tenantUnaryInterceptor("tenant-xyz")
	_, _ = interceptor(context.Background(), nil, &grpc.UnaryServerInfo{}, handler)
	if seen != "tenant-xyz" {
		t.Errorf("tenantFrom = %q, want tenant-xyz", seen)
	}
}

func TestRecoveryInterceptorTurnsPanicIntoInternal(t *testing.T) {
	handler := func(_ context.Context, _ any) (any, error) {
		panic("boom")
	}
	interceptor := recoveryUnaryInterceptor()
	_, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/x/Y"}, handler)
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
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
