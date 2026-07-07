package observability

import (
	"context"
	"log/slog"
	"testing"
)

func TestSetupNoEndpointReturnsNoopShutdown(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_TRACES_EXPORTER", "")

	p, err := Setup(context.Background(), Config{
		ServiceName: "go-ledger-test",
		Environment: "test",
		Logger:      slog.Default(),
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if p.Shutdown == nil {
		t.Fatal("Shutdown must be non-nil even in no-op mode")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestSetupConsoleExporterBuilds(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_TRACES_EXPORTER", "console")

	p, err := Setup(context.Background(), Config{ServiceName: "go-ledger-test", Environment: "test"})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
}

func TestServiceVersionNonEmpty(t *testing.T) {
	if serviceVersion() == "" {
		t.Fatal("serviceVersion must never be empty")
	}
}
