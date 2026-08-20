// Package apperror defines the error vocabulary shared by Ratiba's domain and
// transport layers.
//
// The domain must be able to say "this slot is already taken" without importing
// net/http, and the transport layer must be able to turn that into a correct
// status code and a stable machine-readable code without a giant switch on
// sentinel values. An apperror.Error carries both: a Kind (which the transport
// layer maps to a status) and a Code (which clients can branch on and which is
// promised not to change).
//
// Message is always safe to return to a client. Any underlying cause is
// attached with WithCause and is only ever written to internal logs.
package apperror

import (
	"errors"
	"fmt"
)

// Kind classifies an error broadly enough for a transport to choose a status
// code, without the domain knowing what a status code is.
type Kind string

const (
	// KindInvalidInput means the request could not be understood: malformed
	// JSON, an unparseable UUID, a bad date string.
	KindInvalidInput Kind = "invalid_input"
	// KindUnprocessable means the request was understood but violates a rule:
	// a slot outside working hours, a blank cancellation reason.
	KindUnprocessable Kind = "unprocessable"
	// KindNotFound means a referenced resource does not exist.
	KindNotFound Kind = "not_found"
	// KindConflict means the request collides with current state: a taken
	// slot, an already-cancelled appointment.
	KindConflict Kind = "conflict"
	// KindInternal is the catch-all. Its Message is never derived from the
	// underlying cause.
	KindInternal Kind = "internal"
)

// FieldViolation pins an error to a specific input field so clients can render
// it next to the offending form control.
type FieldViolation struct {
	// Field is a dotted path into the request body, or a query parameter name.
	Field string `json:"field"`
	// Code is a stable machine-readable reason.
	Code string `json:"code"`
	// Message is a human-readable explanation, safe to display.
	Message string `json:"message"`
}

// Error is Ratiba's structured application error.
type Error struct {
	Kind       Kind
	Code       string
	Message    string
	Violations []FieldViolation

	cause error
}

// New builds an Error. Message must be safe to show to an unauthenticated
// caller.
func New(kind Kind, code, message string) *Error {
	return &Error{Kind: kind, Code: code, Message: message}
}

// Newf is New with formatting. The format string and arguments must not contain
// patient data, credentials or raw request content.
func Newf(kind Kind, code, format string, args ...any) *Error {
	return &Error{Kind: kind, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s (%s): %s: %v", e.Code, e.Kind, e.Message, e.cause)
	}
	return fmt.Sprintf("%s (%s): %s", e.Code, e.Kind, e.Message)
}

// Unwrap exposes the internal cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.cause }

// WithCause attaches an internal cause. The cause is logged, never returned to
// clients.
func (e *Error) WithCause(cause error) *Error {
	clone := *e
	clone.cause = cause
	return &clone
}

// WithViolations attaches field-level detail.
func (e *Error) WithViolations(violations ...FieldViolation) *Error {
	clone := *e
	clone.Violations = append(append([]FieldViolation(nil), clone.Violations...), violations...)
	return &clone
}

// From extracts an *Error from an error chain.
func From(err error) (*Error, bool) {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr, true
	}
	return nil, false
}

// Internal wraps an unexpected error. The caller's message is discarded from
// client output; only the fixed, safe message is exposed.
func Internal(cause error) *Error {
	return New(KindInternal, CodeInternalError, "The server encountered an unexpected condition.").WithCause(cause)
}

// Stable machine-readable codes.
//
// These are part of Ratiba's public API contract and must not be renamed
// without a version bump. The list grows as endpoints are built; each code is
// added by the change that first returns it, so an unused code cannot
// accumulate here.
const (
	CodeInternalError    = "internal_error"
	CodeNotFound         = "not_found"
	CodeValidationFailed = "validation_failed"

	CodeDoctorNotFound      = "doctor_not_found"
	CodePatientNotFound     = "patient_not_found"
	CodeAppointmentNotFound = "appointment_not_found"
	CodeDoctorInactive      = "doctor_inactive"
	CodePatientInactive     = "patient_inactive"

	CodeSlotUnavailable    = "slot_unavailable"
	CodeSlotNotAligned     = "slot_not_aligned"
	CodeSlotOutsideHours   = "slot_outside_working_hours"
	CodeDoctorNotWorking   = "doctor_not_working_on_date"
	CodeSlotInPast         = "slot_in_past"
	CodeSlotTooSoon        = "slot_too_soon"
	CodeAlreadyCancelled   = "appointment_already_cancelled"
	CodeRescheduleSameSlot = "reschedule_same_slot"

	CodeUnsupportedTimezone = "unsupported_timezone"
)
