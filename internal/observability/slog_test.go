package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(NewTraceHandler(slog.NewJSONHandler(buf, nil)))
}

func TestTraceHandlerInjectsIDsWhenSpanPresent(t *testing.T) {
	var buf bytes.Buffer
	log := newTestLogger(&buf)

	tid, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	sid, _ := trace.SpanIDFromHex("0102030405060708")
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	log.InfoContext(ctx, "hello")

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if m["trace_id"] != "0102030405060708090a0b0c0d0e0f10" {
		t.Errorf("trace_id = %v", m["trace_id"])
	}
	if m["span_id"] != "0102030405060708" {
		t.Errorf("span_id = %v", m["span_id"])
	}
	if m["sampled"] != true {
		t.Errorf("sampled = %v", m["sampled"])
	}
}

func TestTraceHandlerOmitsIDsWithoutSpan(t *testing.T) {
	var buf bytes.Buffer
	newTestLogger(&buf).InfoContext(context.Background(), "hello")

	var m map[string]any
	_ = json.Unmarshal(buf.Bytes(), &m)
	if _, ok := m["trace_id"]; ok {
		t.Error("trace_id should be absent without a span")
	}
}

func TestTraceHandlerPreservesWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewTraceHandler(slog.NewJSONHandler(&buf, nil))).With("component", "test")
	log.Info("hello")

	var m map[string]any
	_ = json.Unmarshal(buf.Bytes(), &m)
	if m["component"] != "test" {
		t.Errorf("component attr lost: %v", m["component"])
	}
}
