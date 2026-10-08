// Package telemetry sets up OpenTelemetry tracing and ties log lines to traces.
package telemetry

import (
	"context"
	"log/slog"
	"os"
	"slices"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Setup installs the global tracer provider and propagator. Traces are exported over OTLP/HTTP
// when OTEL_EXPORTER_OTLP_ENDPOINT (or the traces-specific variable) is set; without it spans are
// still created, so trace ids reach the logs and outgoing requests, but nothing is exported.
//
// The returned function flushes buffered spans; call it on shutdown.
func Setup(ctx context.Context, serviceName string) (func(context.Context) error, error) {
	// W3C traceparent on incoming and outgoing requests: the trace continues across services.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	// Detectors run in order and later ones win, so OTEL_SERVICE_NAME overrides the default name.
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", serviceName)),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, err
	}

	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" {
		exporter, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, err
		}
		// batched, so exporting never sits on the request path
		opts = append(opts, sdktrace.WithBatcher(exporter))
	}
	provider := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(provider)
	return provider.Shutdown, nil
}

// LogHandler adds trace_id and span_id to every record logged with a context that carries a span,
// so a log line in any backend leads straight to its trace.
type LogHandler struct {
	slog.Handler
}

func (h LogHandler) Handle(ctx context.Context, r slog.Record) error {
	if ids := traceAttrs(ctx); ids != nil {
		r.AddAttrs(ids...)
	}
	return h.Handler.Handle(ctx, r)
}

func (h LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return LogHandler{h.Handler.WithAttrs(attrs)}
}

func (h LogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return groupedLogHandler{Handler: h.Handler.WithGroup(name), root: h.Handler, steps: []logStep{{group: name}}}
}

// groupedLogHandler is a LogHandler after WithGroup. Record attributes land inside the open group,
// so the trace ids cannot simply be added to the record: they are added to root, the handler from
// before the first group, and the groups and attributes since then are applied again on top.
// That rebuild costs a little on every traced record, and is only paid by loggers that use groups.
type groupedLogHandler struct {
	slog.Handler // root with every step applied: used as is for records without a span
	root         slog.Handler
	steps        []logStep
}

// logStep is one WithGroup (group set) or WithAttrs (attrs set) call made since the first group.
type logStep struct {
	group string
	attrs []slog.Attr
}

func (h groupedLogHandler) Handle(ctx context.Context, r slog.Record) error {
	ids := traceAttrs(ctx)
	if ids == nil {
		return h.Handler.Handle(ctx, r)
	}
	next := h.root.WithAttrs(ids)
	for _, step := range h.steps {
		if step.group != "" {
			next = next.WithGroup(step.group)
		} else {
			next = next.WithAttrs(step.attrs)
		}
	}
	return next.Handle(ctx, r)
}

func (h groupedLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h.with(h.Handler.WithAttrs(attrs), logStep{attrs: attrs})
}

func (h groupedLogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.with(h.Handler.WithGroup(name), logStep{group: name})
}

func (h groupedLogHandler) with(applied slog.Handler, step logStep) slog.Handler {
	// Clip: handlers derived from the same parent must not append into one shared backing array.
	return groupedLogHandler{Handler: applied, root: h.root, steps: append(slices.Clip(h.steps), step)}
}

func traceAttrs(ctx context.Context) []slog.Attr {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []slog.Attr{slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String())}
}
