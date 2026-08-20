// Package logging builds Ratiba's structured logger and carries the correlation
// identifiers that make a single request traceable end to end.
//
// The central idea is that a support engineer holding one request ID from a
// customer report can find every log line that request produced — including
// lines written deep in the service layer that never saw an *http.Request.
// That is achieved by putting the identifier in the context and having the
// handler read it back out, rather than threading it through every signature.
//
// Nothing in this package logs patient data, cancellation reasons, request
// bodies or credentials. See docs/security.md for the full rule.
package logging

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/JerryLegend254/ratiba/internal/platform/config"
)

type contextKey int

const (
	requestIDKey contextKey = iota
	routeKey
)

// WithRequestID returns a context carrying the request's correlation ID.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFrom reads the correlation ID, returning "" when absent.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// WithRoute returns a context carrying the matched route template.
//
// The template ("/doctors/{doctorID}/availability") is stored rather than the
// raw path, because the raw path contains identifiers and would make log and
// metric cardinality unbounded.
func WithRoute(ctx context.Context, route string) context.Context {
	return context.WithValue(ctx, routeKey, route)
}

// RouteFrom reads the matched route template.
func RouteFrom(ctx context.Context) string {
	route, _ := ctx.Value(routeKey).(string)
	return route
}

// New builds the application logger.
//
// Deployed environments emit JSON. Text output exists only for local
// development, where a human is reading the terminal; config.Load refuses to
// start production with it.
func New(w io.Writer, cfg config.Config) *slog.Logger {
	options := &slog.HandlerOptions{
		Level: cfg.Logging.Level,
		// Source is attached only at debug, where the cost is acceptable and
		// the caller location is genuinely useful.
		AddSource: cfg.Logging.Level <= slog.LevelDebug,
	}

	var handler slog.Handler
	if strings.EqualFold(cfg.Logging.Format, "text") {
		handler = slog.NewTextHandler(w, options)
	} else {
		handler = slog.NewJSONHandler(w, options)
	}

	logger := slog.New(&contextHandler{Handler: handler})
	return logger.With(
		slog.String("service", cfg.ServiceName),
		slog.String("env", string(cfg.Env)),
		slog.String("version", cfg.Build.Version),
		slog.String("commit", cfg.Build.Commit),
	)
}

// contextHandler enriches every record with the correlation identifiers held in
// the context.
type contextHandler struct {
	slog.Handler
}

// Handle attaches request and trace identifiers, then delegates.
func (h *contextHandler) Handle(ctx context.Context, record slog.Record) error {
	if id := RequestIDFrom(ctx); id != "" {
		record.AddAttrs(slog.String("request_id", id))
	}
	// When tracing is enabled the trace ID lets a log line be pivoted straight
	// into the corresponding trace. When it is not, these attributes are simply
	// absent.
	if spanCtx := trace.SpanContextFromContext(ctx); spanCtx.IsValid() {
		record.AddAttrs(
			slog.String("trace_id", spanCtx.TraceID().String()),
			slog.String("span_id", spanCtx.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, record)
}

// WithAttrs preserves the context enrichment across logger.With calls.
func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup preserves the context enrichment across logger.WithGroup calls.
func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithGroup(name)}
}

// Discard returns a logger that writes nothing, for tests that do not assert on
// log output.
func Discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}
