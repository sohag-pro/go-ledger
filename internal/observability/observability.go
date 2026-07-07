package observability

import (
	"context"
	"log/slog"
	"os"
	"runtime/debug"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
)

// Config parameterizes Setup. Logger is used only for the startup no-op warning;
// nil falls back to slog.Default().
type Config struct {
	ServiceName string
	Environment string
	Logger      *slog.Logger
}

// Providers holds the shutdown hook the caller must defer. Shutdown flushes any
// buffered spans within the passed context's deadline and is always safe to call.
type Providers struct {
	Shutdown func(context.Context) error
}

func noop() Providers {
	return Providers{Shutdown: func(context.Context) error { return nil }}
}

// Setup builds the trace pipeline and sets OpenTelemetry globals. Exporter mode is
// chosen from the environment (see docs/adr/010-observability.md): OTLP/HTTP when an
// endpoint is set, stdout when OTEL_TRACES_EXPORTER=console, otherwise a loud no-op.
// The sampler is intentionally left to the SDK default (parent-based always-on) so
// OTEL_TRACES_SAMPLER and OTEL_TRACES_SAMPLER_ARG control sampling.
func Setup(ctx context.Context, cfg Config) (Providers, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(serviceVersion()),
			semconv.DeploymentEnvironmentName(cfg.Environment),
		),
	)
	if err != nil {
		return noop(), err
	}

	var exporter sdktrace.SpanExporter
	switch {
	case os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "":
		exporter, err = otlptracehttp.New(ctx)
	case os.Getenv("OTEL_TRACES_EXPORTER") == "console":
		exporter, err = stdouttrace.New(stdouttrace.WithPrettyPrint())
	default:
		logger.Warn("tracing disabled: no OTEL_EXPORTER_OTLP_ENDPOINT set; spans will not be exported")
		return noop(), nil
	}
	if err != nil {
		return noop(), err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exporter),
	)
	otel.SetTracerProvider(tp)

	return Providers{Shutdown: tp.Shutdown}, nil
}

// serviceVersion reports the VCS revision the binary was built from, so every
// trace can be attributed to a release without a hand-maintained constant.
func serviceVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			return s.Value
		}
	}
	return "unknown"
}
