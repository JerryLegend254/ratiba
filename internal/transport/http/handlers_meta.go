package http

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JerryLegend254/ratiba/internal/platform/apperror"
)

// handleLivez implements GET /livez.
//
// Liveness answers exactly one question: is this process running and able to
// serve a handler? It deliberately does NOT check the database. A liveness
// probe that fails when a dependency is down causes the orchestrator to restart
// every replica during a database incident, which turns a degraded service into
// no service at all.
func (s *Server) handleLivez(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, healthResponse{
		Status:  "ok",
		Version: s.build.Version,
		Commit:  s.build.Commit,
	}, s.logger)
}

// handleReadyz implements GET /readyz.
//
// Readiness answers "should traffic be routed here right now?", so it does
// verify the database — with a short timeout of its own, because a readiness
// probe that hangs is worse than one that fails. It reports 503 during graceful
// shutdown so the platform stops sending new requests before the listener
// closes.
//
// The response names each dependency and whether it is reachable. It never
// includes the connection string, the driver error, or anything else that would
// leak internals to an unauthenticated caller.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{}
	ready := true

	if !s.readiness.IsOpen() {
		checks["accepting_traffic"] = "draining"
		ready = false
	} else {
		checks["accepting_traffic"] = "ok"
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.readinessTimeout)
	defer cancel()

	if err := s.health.Ping(ctx); err != nil {
		checks["database"] = "unavailable"
		ready = false
		s.logger.WarnContext(r.Context(), "readiness check failed",
			slog.String("event", "health.not_ready"),
			slog.String("dependency", "database"),
			slog.String("error", err.Error()),
		)
	} else {
		checks["database"] = "ok"
	}

	status := http.StatusOK
	body := healthResponse{Status: "ready", Checks: checks, Version: s.build.Version, Commit: s.build.Commit}
	if !ready {
		status = http.StatusServiceUnavailable
		body.Status = "not_ready"
	}
	writeJSON(w, r, status, body, s.logger)
}

// handleServiceInfo implements GET /.
//
// It is a signpost: a reviewer who opens the base URL should immediately see
// what this service is, which build is running, and where the documentation is.
func (s *Server) handleServiceInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, serviceInfoResponse{
		Service:     s.serviceName,
		Version:     s.build.Version,
		Commit:      s.build.Commit,
		Environment: s.environment,
		Docs: map[string]string{
			"interactive": "/docs",
			"openapi":     "/openapi.yaml",
			"errors":      "/problems",
		},
		Endpoints: []string{
			"POST   /appointments",
			"GET    /appointments/{id}",
			"PATCH  /appointments/{id}/cancel",
			"PATCH  /appointments/{id}/reschedule",
			"GET    /doctors",
			"GET    /doctors/{id}",
			"GET    /doctors/{id}/availability?date=YYYY-MM-DD",
			"GET    /patients",
			"GET    /patients/{id}",
			"GET    /patients/{id}/appointments",
			"GET    /livez",
			"GET    /readyz",
		},
	}, s.logger)
}

// handleListProblems implements GET /problems.
//
// The problem catalogue is served rather than only documented so that the
// "type" URI in every error response resolves to something real, and so a
// client developer can enumerate every failure mode without reading the source.
func (s *Server) handleListProblems(w http.ResponseWriter, r *http.Request) {
	codes := make([]string, 0, len(problemCatalogue))
	for code := range problemCatalogue {
		codes = append(codes, code)
	}
	sort.Strings(codes)

	descriptions := make([]problemDescription, 0, len(codes))
	for _, code := range codes {
		descriptions = append(descriptions, problemCatalogue[code])
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"data": descriptions}, s.logger)
}

// handleGetProblem implements GET /problems/{code}.
func (s *Server) handleGetProblem(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	description, ok := problemCatalogue[code]
	if !ok {
		writeProblem(w, r, apperror.New(apperror.KindNotFound, apperror.CodeNotFound,
			"No problem type is registered with that code."), s.logger)
		return
	}
	writeJSON(w, r, http.StatusOK, description, s.logger)
}

// problemCatalogue documents every error code Ratiba can return.
//
// It is exercised by a test that asserts each code used in the codebase appears
// here, so the catalogue cannot quietly fall behind the implementation.
var problemCatalogue = map[string]problemDescription{
	apperror.CodeInternalError: {
		Code: apperror.CodeInternalError, Status: 500, Title: "Internal error",
		Meaning: "An unexpected condition occurred. The response carries a request_id; quote it when reporting the problem.",
	},
	apperror.CodeNotFound: {
		Code: apperror.CodeNotFound, Status: 404, Title: "Not found",
		Meaning: "No route or resource matches this request.",
	},
	apperror.CodeMethodNotAllowed: {
		Code: apperror.CodeMethodNotAllowed, Status: 405, Title: "Method not allowed",
		Meaning: "The path exists but does not accept this HTTP method.",
	},
	apperror.CodeMalformedJSON: {
		Code: apperror.CodeMalformedJSON, Status: 400, Title: "Malformed JSON",
		Meaning: "The request body is absent or is not valid JSON.",
	},
	apperror.CodeUnknownField: {
		Code: apperror.CodeUnknownField, Status: 400, Title: "Unknown field",
		Meaning: "The body contains a field this endpoint does not accept. Check for a typo; unknown fields are rejected rather than ignored.",
	},
	apperror.CodeTrailingContent: {
		Code: apperror.CodeTrailingContent, Status: 400, Title: "Trailing content",
		Meaning: "The body contains more than one JSON value.",
	},
	apperror.CodeUnsupportedMediaType: {
		Code: apperror.CodeUnsupportedMediaType, Status: 415, Title: "Unsupported media type",
		Meaning: "Send Content-Type: application/json on requests that carry a body.",
	},
	apperror.CodePayloadTooLarge: {
		Code: apperror.CodePayloadTooLarge, Status: 413, Title: "Payload too large",
		Meaning: "The request body exceeded the configured size limit.",
	},
	apperror.CodeInvalidPathParameter: {
		Code: apperror.CodeInvalidPathParameter, Status: 400, Title: "Invalid path parameter",
		Meaning: "An identifier in the URL is not a valid UUID.",
	},
	apperror.CodeInvalidQueryParam: {
		Code: apperror.CodeInvalidQueryParam, Status: 400, Title: "Invalid query parameter",
		Meaning: "A query parameter is missing, malformed, or out of range.",
	},
	apperror.CodeValidationFailed: {
		Code: apperror.CodeValidationFailed, Status: 422, Title: "Validation failed",
		Meaning: "The body was understood but a field is invalid. See the violations array.",
	},
	apperror.CodeUnauthorized: {
		Code: apperror.CodeUnauthorized, Status: 401, Title: "Unauthorized",
		Meaning: "A credential is required. Currently only /metrics requires one.",
	},
	apperror.CodeDoctorNotFound: {
		Code: apperror.CodeDoctorNotFound, Status: 404, Title: "Doctor not found",
		Meaning: "No doctor exists with that identifier. List doctors at GET /doctors.",
	},
	apperror.CodePatientNotFound: {
		Code: apperror.CodePatientNotFound, Status: 404, Title: "Patient not found",
		Meaning: "No patient exists with that identifier. List patients at GET /patients.",
	},
	apperror.CodeAppointmentNotFound: {
		Code: apperror.CodeAppointmentNotFound, Status: 404, Title: "Appointment not found",
		Meaning: "No appointment exists with that identifier.",
	},
	apperror.CodeDoctorInactive: {
		Code: apperror.CodeDoctorInactive, Status: 422, Title: "Doctor inactive",
		Meaning: "The doctor exists but is not accepting appointments, so they have no availability and cannot be booked.",
	},
	apperror.CodePatientInactive: {
		Code: apperror.CodePatientInactive, Status: 422, Title: "Patient inactive",
		Meaning: "The patient record is deactivated and cannot be booked for. Existing appointments remain readable.",
	},
	apperror.CodeSlotUnavailable: {
		Code: apperror.CodeSlotUnavailable, Status: 409, Title: "Slot unavailable",
		Meaning: "Another active appointment already holds that doctor and start time. Fetch availability again and choose another slot.",
	},
	apperror.CodeSlotNotAligned: {
		Code: apperror.CodeSlotNotAligned, Status: 422, Title: "Slot not aligned",
		Meaning: "Appointments start on a 30-minute boundary within the doctor's working hours. Use a start time returned by the availability endpoint.",
	},
	apperror.CodeSlotOutsideHours: {
		Code: apperror.CodeSlotOutsideHours, Status: 422, Title: "Outside working hours",
		Meaning: "The slot falls outside the doctor's working hours, or the appointment would end after they finish.",
	},
	apperror.CodeDoctorNotWorking: {
		Code: apperror.CodeDoctorNotWorking, Status: 422, Title: "Doctor not working",
		Meaning: "The doctor has no working hours on that local calendar date.",
	},
	apperror.CodeSlotInPast: {
		Code: apperror.CodeSlotInPast, Status: 422, Title: "Slot in the past",
		Meaning: "The requested start time has already passed.",
	},
	apperror.CodeSlotTooSoon: {
		Code: apperror.CodeSlotTooSoon, Status: 422, Title: "Slot too soon",
		Meaning: "Appointments must start at least the configured lead time from now (one hour by default).",
	},
	apperror.CodeAlreadyCancelled: {
		Code: apperror.CodeAlreadyCancelled, Status: 409, Title: "Already cancelled",
		Meaning: "The appointment is cancelled and can no longer be cancelled or rescheduled.",
	},
	apperror.CodeRescheduleSameSlot: {
		Code: apperror.CodeRescheduleSameSlot, Status: 409, Title: "Reschedule to the same slot",
		Meaning: "The appointment already starts at that time. Choose a different slot, or make no request at all.",
	},
	apperror.CodeIdempotencyKeyReuse: {
		Code: apperror.CodeIdempotencyKeyReuse, Status: 409, Title: "Idempotency key reuse",
		Meaning: "This Idempotency-Key was already used with a different payload. Use a fresh key for a different booking.",
	},
	apperror.CodeInvalidIdempotencyKey: {
		Code: apperror.CodeInvalidIdempotencyKey, Status: 400, Title: "Invalid idempotency key",
		Meaning: "Idempotency-Key must be 8 to 255 printable ASCII characters.",
	},
	apperror.CodeUnsupportedTimezone: {
		Code: apperror.CodeUnsupportedTimezone, Status: 500, Title: "Unsupported timezone",
		Meaning: "The doctor's configured IANA timezone could not be resolved. This is a data problem; report it with the request_id.",
	},
	apperror.CodeRequestTimeout: {
		Code: apperror.CodeRequestTimeout, Status: 503, Title: "Request timed out",
		Meaning: "The request exceeded the server's processing budget and was abandoned. It may or may not have taken effect; use GET to confirm.",
	},
}

// readinessTimeoutDefault bounds the database check in /readyz.
const readinessTimeoutDefault = 2 * time.Second
