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

// Backends join logs to traces on the top-level trace_id, so a group must not swallow it.
func TestLogHandlerTraceIDsOutsideGroups(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(LogHandler{slog.NewJSONHandler(&buf, nil)}).
		With("component", "test").WithGroup("req").With("method", "POST")

	ctx, span := sdktrace.NewTracerProvider().Tracer("test").Start(context.Background(), "op")
	defer span.End()

	logger.InfoContext(ctx, "grouped", "path", "/x")

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	if line["trace_id"] != span.SpanContext().TraceID().String() {
		t.Errorf("top-level trace_id = %v, want %s; line: %s", line["trace_id"], span.SpanContext().TraceID(), buf.String())
	}
	if line["span_id"] != span.SpanContext().SpanID().String() {
		t.Errorf("top-level span_id = %v, want %s", line["span_id"], span.SpanContext().SpanID())
	}
	req, _ := line["req"].(map[string]any)
	if req["method"] != "POST" || req["path"] != "/x" {
		t.Errorf("group attributes = %v, want method and path inside req", line["req"])
	}
	if _, ok := req["trace_id"]; ok {
		t.Error("trace_id also logged inside the group")
	}
	if line["component"] != "test" {
		t.Error("attributes added before the group were lost")
	}
}

// Two loggers made from the same grouped parent must not see each other's attributes.
func TestGroupedLogHandlerSiblingsStayApart(t *testing.T) {
	var buf bytes.Buffer
	// three steps: without slices.Clip the parent's slice would have spare capacity to share
	parent := slog.New(LogHandler{slog.NewJSONHandler(&buf, nil)}).WithGroup("req").With("x", 1).With("y", 2)
	a := parent.With("who", "a")
	b := parent.With("who", "b")

	ctx, span := sdktrace.NewTracerProvider().Tracer("test").Start(context.Background(), "op")
	defer span.End()
	a.InfoContext(ctx, "from a")
	b.InfoContext(ctx, "from b")

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want 2", len(lines))
	}
	for i, want := range []string{"a", "b"} {
		var line map[string]any
		if err := json.Unmarshal(lines[i], &line); err != nil {
			t.Fatal(err)
		}
		if req, _ := line["req"].(map[string]any); req["who"] != want {
			t.Errorf("line %d: req = %v, want who=%s", i, line["req"], want)
		}
	}
}
