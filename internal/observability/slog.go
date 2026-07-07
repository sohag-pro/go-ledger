// Package observability wires OpenTelemetry tracing and metrics for the ledger
// and correlates slog output with the active trace. See docs/adr/010-observability.md.
package observability

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// traceHandler wraps a slog.Handler and stamps every record that carries a valid
// span context with the trace_id, span_id, and sampled flag, so a log line can be
// pivoted to its trace.
type traceHandler struct {
	inner slog.Handler
}

// NewTraceHandler returns a handler that delegates to inner and adds trace
// correlation attributes when the record's context holds a valid span context.
func NewTraceHandler(inner slog.Handler) slog.Handler {
	return traceHandler{inner: inner}
}

func (h traceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h traceHandler) Handle(ctx context.Context, rec slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		rec.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
			slog.Bool("sampled", sc.IsSampled()),
		)
	}
	return h.inner.Handle(ctx, rec)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{inner: h.inner.WithGroup(name)}
}
