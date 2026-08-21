package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/JerryLegend254/ratiba/internal/platform/apperror"
	"github.com/JerryLegend254/ratiba/internal/platform/calendar"
)

// decodeJSON reads a strict JSON request body.
//
// "Strict" means four things, each of which turns a silent client bug into a
// loud, immediate error:
//
//   - the content type must actually be JSON;
//   - the body is size-limited, so a hostile client cannot exhaust memory;
//   - unknown fields are rejected, so a typo like "start_at" fails instead of
//     being silently ignored and booking the zero time;
//   - trailing content after the JSON value is rejected, so a doubled body
//     cannot smuggle a second document past validation.
func decodeJSON[T any](w http.ResponseWriter, r *http.Request, maxBytes int64) (T, error) {
	var target T

	if err := requireJSONContentType(r); err != nil {
		return target, err
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&target); err != nil {
		return target, decodeError(err, maxBytes)
	}

	// Exactly one JSON value is allowed. A second one means the client sent
	// something Ratiba would otherwise silently discard.
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return target, apperror.New(apperror.KindInvalidInput, apperror.CodeTrailingContent,
			"The request body must contain exactly one JSON object.")
	}

	return target, nil
}

// requireJSONContentType enforces the media type on requests that carry a body.
func requireJSONContentType(r *http.Request) error {
	raw := r.Header.Get("Content-Type")
	if raw == "" {
		return apperror.New(apperror.KindUnsupportedMedia, apperror.CodeUnsupportedMediaType,
			"A Content-Type of application/json is required.")
	}
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return apperror.New(apperror.KindUnsupportedMedia, apperror.CodeUnsupportedMediaType,
			"A Content-Type of application/json is required.")
	}
	return nil
}

// decodeError turns a JSON decoding failure into a precise client error.
func decodeError(err error, maxBytes int64) error {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return apperror.Newf(apperror.KindPayloadTooLarge, apperror.CodePayloadTooLarge,
			"The request body must not exceed %d bytes.", maxBytes)
	}

	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return apperror.Newf(apperror.KindInvalidInput, apperror.CodeMalformedJSON,
			"The request body is not valid JSON (at byte %d).", syntaxErr.Offset)
	}

	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		field := typeErr.Field
		if field == "" {
			field = "body"
		}
		return apperror.New(apperror.KindUnprocessable, apperror.CodeValidationFailed,
			"A field in the request body has the wrong type.").
			WithViolations(apperror.FieldViolation{
				Field:   field,
				Code:    "invalid_type",
				Message: fmt.Sprintf("Expected a value of type %s.", typeErr.Type.String()),
			})
	}

	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return apperror.New(apperror.KindInvalidInput, apperror.CodeMalformedJSON,
			"A JSON request body is required.")
	}

	// encoding/json reports unknown fields only as a formatted string, so the
	// field name has to be recovered from the message.
	const unknownFieldPrefix = "json: unknown field "
	if message := err.Error(); strings.HasPrefix(message, unknownFieldPrefix) {
		field := strings.Trim(strings.TrimPrefix(message, unknownFieldPrefix), `"`)
		return apperror.New(apperror.KindInvalidInput, apperror.CodeUnknownField,
			"The request body contains a field this endpoint does not accept.").
			WithViolations(apperror.FieldViolation{
				Field:   field,
				Code:    "unknown",
				Message: "Remove this field.",
			})
	}

	return apperror.New(apperror.KindInvalidInput, apperror.CodeMalformedJSON,
		"The request body could not be decoded.").WithCause(err)
}

// pathUUID reads and validates a UUID path parameter.
//
// A malformed UUID is a 400, not a 404: the request itself is unparseable, and
// answering 404 would imply Ratiba looked for the resource and did not find it.
func pathUUID(r *http.Request, name string) (uuid.UUID, error) {
	raw := chi.URLParam(r, name)
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, apperror.New(apperror.KindInvalidInput, apperror.CodeInvalidPathParameter,
			"The identifier in the URL is not a valid UUID.").
			WithViolations(apperror.FieldViolation{
				Field:   name,
				Code:    "invalid_uuid",
				Message: "Expected a UUID, for example 018f4e0a-1c2b-7d3e-9f01-2a3b4c5d6e7f.",
			})
	}
	if id == uuid.Nil {
		return uuid.Nil, apperror.New(apperror.KindInvalidInput, apperror.CodeInvalidPathParameter,
			"The identifier in the URL must not be the nil UUID.")
	}
	return id, nil
}

// queryDate reads a required YYYY-MM-DD query parameter.
func queryDate(r *http.Request, name string) (calendar.Date, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return calendar.Date{}, apperror.Newf(apperror.KindInvalidInput, apperror.CodeInvalidQueryParam,
			"The %q query parameter is required.", name).
			WithViolations(apperror.FieldViolation{
				Field:   name,
				Code:    "required",
				Message: "Provide a date in YYYY-MM-DD format.",
			})
	}
	date, err := calendar.ParseDate(raw)
	if err != nil {
		return calendar.Date{}, apperror.Newf(apperror.KindInvalidInput, apperror.CodeInvalidQueryParam,
			"The %q query parameter must be a calendar date in YYYY-MM-DD format.", name).
			WithViolations(apperror.FieldViolation{
				Field:   name,
				Code:    "invalid_date",
				Message: "Expected YYYY-MM-DD, for example 2026-09-01.",
			})
	}
	return date, nil
}

// queryPage reads limit and offset, rejecting out-of-range values rather than
// silently clamping them.
//
// Clamping would let a client believe it asked for 5000 records and received
// all of them. Rejecting makes the boundary visible.
func queryPage(r *http.Request, defaultLimit, maxLimit int32) (limit, offset int32, err error) {
	limit = defaultLimit

	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, parseErr := strconv.ParseInt(raw, 10, 32)
		if parseErr != nil || parsed < 1 || int32(parsed) > maxLimit {
			return 0, 0, apperror.Newf(apperror.KindInvalidInput, apperror.CodeInvalidQueryParam,
				"The \"limit\" query parameter must be an integer between 1 and %d.", maxLimit).
				WithViolations(apperror.FieldViolation{
					Field:   "limit",
					Code:    "out_of_range",
					Message: fmt.Sprintf("Expected 1..%d.", maxLimit),
				})
		}
		limit = int32(parsed)
	}

	if raw := strings.TrimSpace(r.URL.Query().Get("offset")); raw != "" {
		parsed, parseErr := strconv.ParseInt(raw, 10, 32)
		if parseErr != nil || parsed < 0 {
			return 0, 0, apperror.New(apperror.KindInvalidInput, apperror.CodeInvalidQueryParam,
				"The \"offset\" query parameter must be a non-negative integer.").
				WithViolations(apperror.FieldViolation{
					Field:   "offset",
					Code:    "out_of_range",
					Message: "Expected 0 or greater.",
				})
		}
		offset = int32(parsed)
	}

	return limit, offset, nil
}

// queryBool reads an optional boolean query parameter.
func queryBool(r *http.Request, name string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, apperror.Newf(apperror.KindInvalidInput, apperror.CodeInvalidQueryParam,
			"The %q query parameter must be true or false.", name)
	}
	return value, nil
}

// parseTimestamp validates a required RFC 3339 instant from a request body.
//
// An offset is mandatory. "2026-09-01T09:00:00" with no zone would have to be
// interpreted against something, and every available choice — the server's
// timezone, UTC, the doctor's timezone — is a guess that silently books the
// wrong hour when it is wrong.
func parseTimestamp(raw, field string) (time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, apperror.New(apperror.KindUnprocessable, apperror.CodeValidationFailed,
			"A start time is required.").
			WithViolations(apperror.FieldViolation{
				Field:   field,
				Code:    "required",
				Message: "Provide an RFC 3339 timestamp with an offset, for example 2026-09-01T09:00:00Z.",
			})
	}

	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, apperror.New(apperror.KindUnprocessable, apperror.CodeValidationFailed,
			"The start time is not a valid RFC 3339 timestamp.").
			WithViolations(apperror.FieldViolation{
				Field:   field,
				Code:    "invalid_timestamp",
				Message: "Expected an RFC 3339 timestamp with an offset, for example 2026-09-01T09:00:00Z or 2026-09-01T12:00:00+03:00.",
			})
	}

	return parsed.UTC(), nil
}

// requireUUID validates a UUID carried in a request body.
func requireUUID(raw, field string) (uuid.UUID, error) {
	if strings.TrimSpace(raw) == "" {
		return uuid.Nil, apperror.Newf(apperror.KindUnprocessable, apperror.CodeValidationFailed,
			"The %q field is required.", field).
			WithViolations(apperror.FieldViolation{
				Field:   field,
				Code:    "required",
				Message: "Provide a UUID.",
			})
	}
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, apperror.Newf(apperror.KindUnprocessable, apperror.CodeValidationFailed,
			"The %q field must be a valid UUID.", field).
			WithViolations(apperror.FieldViolation{
				Field:   field,
				Code:    "invalid_uuid",
				Message: "Expected a UUID, for example 018f4e0a-1c2b-7d3e-9f01-2a3b4c5d6e7f.",
			})
	}
	return id, nil
}
