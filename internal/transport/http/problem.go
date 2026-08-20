// Package http is Ratiba's HTTP transport: routing, request decoding, response
// shaping, middleware and error mapping.
//
// Handlers here are deliberately thin. Each one decodes and validates its
// input, calls exactly one use case, and maps the result. No business rule
// lives in this package — if a rule appears in a handler, it can no longer be
// tested without an HTTP server, and it will eventually disagree with the same
// rule as applied elsewhere.
//
// Transport DTOs are separate types from domain models on purpose: the wire
// format is a contract with clients (see api/openapi.yaml) and must be able to
// stay stable while domain types evolve.
package http

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/JerryLegend254/ratiba/internal/platform/apperror"
	"github.com/JerryLegend254/ratiba/internal/platform/logging"
)

// ProblemContentType is the media type mandated by RFC 9457.
const ProblemContentType = "application/problem+json"

// Problem is an RFC 9457 problem details document.
//
// Beyond the standard members it carries two additions that matter operationally:
//
//   - Code is a stable machine-readable identifier. Clients branch on this, not
//     on Title or Detail, both of which may be reworded.
//   - RequestID lets a user paste an error straight into a support ticket, and
//     lets an engineer find the exact logs for it.
type Problem struct {
	// Type is a URI reference identifying the problem kind. Ratiba serves a
	// description of each at GET /problems/{code}.
	Type string `json:"type"`
	// Title is a short, human-readable summary of the problem kind.
	Title string `json:"title"`
	// Status is the HTTP status code, repeated here per RFC 9457.
	Status int `json:"status"`
	// Detail explains this specific occurrence. It never contains internal
	// error text, SQL, or configuration values.
	Detail string `json:"detail"`
	// Instance is the request path that produced the problem.
	Instance string `json:"instance,omitempty"`
	// Code is the stable machine-readable error code.
	Code string `json:"code"`
	// RequestID correlates with the X-Request-Id response header and the logs.
	RequestID string `json:"request_id,omitempty"`
	// Violations lists field-level problems, when the error has them.
	Violations []apperror.FieldViolation `json:"violations,omitempty"`
}

// statusForKind maps a domain error kind onto an HTTP status code.
//
// This is the single place the translation happens. The distinction between 400
// and 422 is the one worth stating plainly: 400 means Ratiba could not
// understand the request at all (bad JSON, an unparseable UUID), 422 means it
// understood the request perfectly and is refusing it (a slot outside working
// hours, a blank reason).
func statusForKind(kind apperror.Kind) int {
	switch kind {
	case apperror.KindInvalidInput:
		return http.StatusBadRequest
	case apperror.KindUnprocessable:
		return http.StatusUnprocessableEntity
	case apperror.KindNotFound:
		return http.StatusNotFound
	case apperror.KindConflict:
		return http.StatusConflict
	case apperror.KindUnsupportedMedia:
		return http.StatusUnsupportedMediaType
	case apperror.KindPayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case apperror.KindUnauthorized:
		return http.StatusUnauthorized
	case apperror.KindUnavailable:
		return http.StatusServiceUnavailable
	case apperror.KindInternal:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

// titleForStatus gives each status class a stable, human-readable title.
func titleForStatus(status int) string {
	if text := http.StatusText(status); text != "" {
		return text
	}
	return "Error"
}

// writeProblem renders an error as a problem details response.
//
// Internal errors are logged at error level with their cause attached and
// answered with a fixed, safe message. Client errors are logged at info level:
// they are normal traffic, and logging them as errors would drown the signal
// that actually needs attention.
func writeProblem(w http.ResponseWriter, r *http.Request, err error, logger *slog.Logger) {
	appErr, ok := apperror.From(err)
	if !ok {
		appErr = apperror.Internal(err)
	}

	status := statusForKind(appErr.Kind)
	problem := Problem{
		Type:       "/problems/" + appErr.Code,
		Title:      titleForStatus(status),
		Status:     status,
		Detail:     appErr.Message,
		Instance:   r.URL.Path,
		Code:       appErr.Code,
		RequestID:  logging.RequestIDFrom(r.Context()),
		Violations: appErr.Violations,
	}

	if status >= http.StatusInternalServerError {
		logger.ErrorContext(r.Context(), "request failed",
			slog.String("event", "http.error"),
			slog.String("error_code", appErr.Code),
			slog.Int("status", status),
			// The underlying cause goes to the log, never to the client.
			slog.String("error", appErr.Error()),
		)
	} else {
		logger.InfoContext(r.Context(), "request rejected",
			slog.String("event", "http.rejected"),
			slog.String("error_code", appErr.Code),
			slog.Int("status", status),
		)
	}

	writeJSONWithContentType(w, r, status, problem, ProblemContentType, logger)
}

// writeJSON renders a successful response.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, payload any, logger *slog.Logger) {
	writeJSONWithContentType(w, r, status, payload, "application/json; charset=utf-8", logger)
}

// writeJSONWithContentType marshals before writing the header so that an
// encoding failure can still produce a correct 500 instead of a truncated body
// under an already-sent 200.
func writeJSONWithContentType(w http.ResponseWriter, r *http.Request, status int, payload any, contentType string, logger *slog.Logger) {
	body, err := json.Marshal(payload)
	if err != nil {
		logger.ErrorContext(r.Context(), "failed to encode response",
			slog.String("event", "http.encode_failed"),
			slog.String("error", err.Error()),
		)
		w.Header().Set("Content-Type", ProblemContentType)
		w.WriteHeader(http.StatusInternalServerError)
		// Hand-written so this path cannot itself fail to encode.
		_, _ = w.Write([]byte(`{"type":"/problems/internal_error","title":"Internal Server Error","status":500,` +
			`"detail":"The server encountered an unexpected condition.","code":"internal_error"}`))
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(body); err != nil {
		// The client hung up mid-response. Nothing to do but record it.
		logger.DebugContext(r.Context(), "failed to write response body",
			slog.String("event", "http.write_failed"),
			slog.String("error", err.Error()),
		)
	}
}
