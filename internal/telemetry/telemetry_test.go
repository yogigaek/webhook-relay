package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestLogHandlerAddsTraceIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(LogHandler{slog.NewJSONHandler(&buf, nil)}).With("component", "test")

	ctx, span := sdktrace.NewTracerProvider().Tracer("test").Start(context.Background(), "op")
	defer span.End()

	logger.InfoContext(ctx, "inside span")
	logger.InfoContext(context.Background(), "outside span")

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want 2", len(lines))
	}
	var inside, outside map[string]any
	_ = json.Unmarshal(lines[0], &inside)
	_ = json.Unmarshal(lines[1], &outside)

	if inside["trace_id"] != span.SpanContext().TraceID().String() {
		t.Errorf("trace_id = %v, want %s", inside["trace_id"], span.SpanContext().TraceID())
	}
	if inside["component"] != "test" {
		t.Error("attributes added with With were lost")
	}
	if _, ok := outside["trace_id"]; ok {
		t.Error("trace_id logged without a span")
	}
}
