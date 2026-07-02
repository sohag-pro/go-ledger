package grpcserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
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
