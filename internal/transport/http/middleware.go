package http

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/JerryLegend254/ratiba/internal/platform/apperror"
	"github.com/JerryLegend254/ratiba/internal/platform/logging"
)

// requestIDHeader is both accepted from clients and always echoed back.
const requestIDHeader = "X-Request-Id"

// maxInboundRequestIDLength bounds an inbound identifier before it reaches the
// logs.
const maxInboundRequestIDLength = 64

// requestID attaches a correlation identifier to every request.
//
// An inbound X-Request-Id is honoured so a caller can correlate across
// services, but only after validation: the value ends up in log fields and
// response headers, so an unbounded or control-character-laden identifier would
// be a log-injection vector. Anything that does not pass is replaced with a
// fresh UUID rather than rejected, because a malformed correlation header is
// not a reason to fail a patient's booking.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitiseRequestID(r.Header.Get(requestIDHeader))
		if id == "" {
			id = uuid.NewString()
		}

		ctx := logging.WithRequestID(r.Context(), id)
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// sanitiseRequestID accepts only short, printable, non-whitespace identifiers.
func sanitiseRequestID(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > maxInboundRequestIDLength {
		return ""
	}
	for _, r := range value {
		isAllowed := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !isAllowed {
			return ""
		}
	}
	return value
}

// traceIDFrom returns the current trace ID, or "" when tracing is inactive.
func traceIDFrom(ctx context.Context) string {
	if spanCtx := trace.SpanContextFromContext(ctx); spanCtx.IsValid() {
		return spanCtx.TraceID().String()
	}
	return ""
}

// observe records metrics, enriches the trace and writes the access log for
// every completed request.
//
// It does its bookkeeping AFTER the inner handler returns, which is the only
// point at which chi has resolved the route template. Logging the raw path
// instead would put patient and appointment UUIDs into every log line and give
// the metrics an unbounded label.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		s.metrics.IncInFlight()
		defer s.metrics.DecInFlight()

		recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)

		route := routeTemplate(r)
		duration := time.Since(start)

		s.metrics.ObserveRequest(r.Method, route, recorder.status, duration, recorder.written)

		// Name the span after the matched route, for the same cardinality
		// reason. otelhttp created it before routing, so it starts out named
		// after the method alone.
		if span := trace.SpanFromContext(r.Context()); span.IsRecording() {
			span.SetName(r.Method + " " + route)
			span.SetAttributes(
				attribute.String("http.route", route),
				attribute.Int("http.response.status_code", recorder.status),
			)
		}

		// Health probes and metric scrapes arrive every few seconds. Logging
		// them at info drowns real traffic; they are still counted in metrics.
		level := slog.LevelInfo
		switch {
		case recorder.status >= http.StatusInternalServerError:
			level = slog.LevelError
		case isProbePath(r.URL.Path):
			level = slog.LevelDebug
		}

		s.logger.LogAttrs(r.Context(), level, "request completed",
			slog.String("event", "http.request"),
			slog.String("method", r.Method),
			slog.String("route", route),
			slog.Int("status", recorder.status),
			slog.Int64("duration_ms", duration.Milliseconds()),
			slog.Int64("response_bytes", recorder.written),
			slog.String("user_agent", truncate(r.UserAgent(), 120)),
		)
	})
}

// routeTemplate returns the matched chi route pattern, falling back to a fixed
// placeholder so an unmatched request cannot introduce a new metric series.
func routeTemplate(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if pattern := rctx.RoutePattern(); pattern != "" {
			return pattern
		}
	}
	return "unmatched"
}

// truncate bounds a string destined for a log field.
func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

// recoverPanic converts a panic into a 500 problem response.
//
// The stack trace goes to the internal log, tagged with the request ID, so an
// engineer can find it from a user's error message. The client is told nothing
// beyond "something went wrong".
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The deferred closure uses r.Context() via writeProblem; contextcheck
		// cannot see through the defer and reports a false positive.
		defer func() { //nolint:contextcheck // the request context is used via writeProblem
			recovered := recover()
			if recovered == nil {
				return
			}
			// A client disconnecting mid-write surfaces as a panic carrying
			// this sentinel. net/http expects it to propagate, and it is not a
			// bug, so it must not be logged or answered as one.
			if err, ok := recovered.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(recovered)
			}

			s.metrics.RecordPanic()
			s.logger.ErrorContext(r.Context(), "panic recovered in handler",
				slog.String("event", "http.panic"),
				slog.Any("panic", recovered),
				slog.String("stack", string(debug.Stack())),
				slog.String("method", r.Method),
				slog.String("route", routeTemplate(r)),
			)

			writeProblem(w, r, apperror.New(apperror.KindInternal, apperror.CodeInternalError,
				"The server encountered an unexpected condition."), s.logger)
		}()

		next.ServeHTTP(w, r)
	})
}

// timeout bounds how long a handler may run.
//
// The deadline is placed on the request context rather than using
// http.TimeoutHandler, so the timeout propagates all the way into pgx (which
// cancels the query server-side) and so the response is a proper
// problem+json document rather than TimeoutHandler's plain text.
func (s *Server) timeout(duration time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), duration)
			defer cancel()

			recorder := &responseRecorder{ResponseWriter: w, status: 0}
			next.ServeHTTP(recorder, r.WithContext(ctx))

			// If the handler returned without writing because its context
			// expired, answer with a timeout instead of an empty 200.
			if recorder.status == 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				writeProblem(w, r, apperror.New(apperror.KindUnavailable, apperror.CodeRequestTimeout,
					"The request exceeded the server's processing budget."), s.logger)
			}
		})
	}
}

// securityHeaders applies the response headers that are meaningful for a JSON
// API.
//
// Deliberately minimal: a Content-Security-Policy or HSTS header on an API that
// serves no HTML to a browser and terminates TLS at Railway's edge would be
// decoration. The two that matter are nosniff (a JSON body must never be
// interpreted as script) and a restrictive referrer policy.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("X-Frame-Options", "DENY")
		// Nothing here is cacheable by an intermediary: responses are
		// per-patient and change the moment somebody books.
		header.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// cors applies a strict allowlist, and is only installed when origins are
// configured.
//
// Credentials are never allowed, and the wildcard origin is rejected at
// configuration load, so the "wildcard origin with credentials" mistake is
// impossible to make here.
func cors(allowedOrigins []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && slices.Contains(allowedOrigins, origin) {
				header := w.Header()
				header.Set("Access-Control-Allow-Origin", origin)
				header.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, OPTIONS")
				header.Set("Access-Control-Allow-Headers", "Content-Type, Idempotency-Key, X-Request-Id")
				header.Set("Access-Control-Expose-Headers", "X-Request-Id, Idempotency-Replayed, Location")
				header.Set("Access-Control-Max-Age", "600")
				// The response varies by Origin, so a shared cache must not
				// reuse one origin's response for another.
				header.Add("Vary", "Origin")
			}

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireMetricsToken guards /metrics with a bearer token.
//
// Railway routes every path on a service to the public internet, so an
// unauthenticated metrics endpoint would publish booking volumes and internal
// timings to anyone who asked. When no token is configured (local development)
// the guard is not installed at all; config.Load makes the token mandatory in
// production.
func (s *Server) requireMetricsToken(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			// Constant-time comparison: a token check that returns early on the
			// first wrong byte is measurably guessable.
			if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
				writeProblem(w, r, apperror.New(apperror.KindUnauthorized, apperror.CodeUnauthorized,
					"A bearer token is required to read metrics."), s.logger)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// responseRecorder captures the status and byte count for the access log.
type responseRecorder struct {
	http.ResponseWriter
	status  int
	written int64
}

// WriteHeader records the status on its way through. net/http ignores every
// call after the first, so the recorder matches that and keeps the first.
func (r *responseRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

// Write records the byte count, defaulting the status to 200 for a handler that
// writes without calling WriteHeader.
func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Unwrap exposes the underlying writer so http.ResponseController can reach
// optional interfaces such as Flusher.
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
