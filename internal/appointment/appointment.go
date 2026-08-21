// Package appointment holds Ratiba's core domain: what an appointment is, the
// rules that decide whether a slot may be booked, and the use cases that move
// appointments between states.
//
// Everything in this package is independently testable. It reaches the database
// only through the ports declared in ports.go, and reads the current time only
// through a clock.Clock, so the entire booking rule set can be exercised with
// no PostgreSQL and no wall-clock dependency.
package appointment

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/JerryLegend254/ratiba/internal/platform/apperror"
)

// Status is an appointment's lifecycle state.
//
// There are deliberately only two. A cancelled appointment is retained rather
// than deleted so the audit trail stays complete, and because only 'booked'
// rows participate in the database's uniqueness rule, cancelling frees the slot
// with no extra bookkeeping.
type Status string

const (
	// StatusBooked is an active appointment occupying its slot.
	StatusBooked Status = "booked"
	// StatusCancelled is a retained, slot-releasing terminal state.
	StatusCancelled Status = "cancelled"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool { return s == StatusBooked || s == StatusCancelled }

// Appointment is a booked 30-minute consultation.
//
// StartsAt and EndsAt are UTC instants describing the half-open interval
// [StartsAt, EndsAt). Rendering them in a doctor's local timezone is a
// presentation concern and never changes what is stored.
type Appointment struct {
	ID                 uuid.UUID
	DoctorID           uuid.UUID
	PatientID          uuid.UUID
	StartsAt           time.Time
	EndsAt             time.Time
	Status             Status
	CancellationReason *string
	CancelledAt        *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// IsActive reports whether the appointment still occupies its slot.
func (a Appointment) IsActive() bool { return a.Status == StatusBooked }

// Slot is a half-open [Start, End) bookable interval.
type Slot struct {
	Start time.Time
	End   time.Time
}

// EventType names an entry in the append-only appointment history.
type EventType string

const (
	// EventBooked records the creation of an appointment.
	EventBooked EventType = "booked"
	// EventCancelled records a cancellation. It deliberately carries no reason
	// text: the reason lives on the appointment row and is never logged.
	EventCancelled EventType = "cancelled"
	// EventRescheduled records a move, with both the old and new start.
	EventRescheduled EventType = "rescheduled"
)

// Event is one entry in an appointment's history. Events are written in the
// same transaction as the state change they describe.
type Event struct {
	AppointmentID uuid.UUID
	Type          EventType
	FromStartsAt  *time.Time
	ToStartsAt    *time.Time
	// Source is coarse provenance ("api", "seed"), not an authenticated actor.
	Source string
}

// DoctorSummary is the subset of doctor detail returned alongside a patient's
// appointments, so a client can render a list without a second round trip.
type DoctorSummary struct {
	ID        uuid.UUID
	Slug      string
	FullName  string
	Specialty string
	Timezone  string
}

// PatientAppointment is an appointment enriched with its doctor.
type PatientAppointment struct {
	Appointment Appointment
	Doctor      DoctorSummary
}

// IdempotencyRecord is the persisted outcome of a booking request that carried
// an Idempotency-Key. Snapshot holds the exact appointment state that was
// returned the first time, so a replay answers with the original response even
// if the appointment has since been cancelled or moved.
type IdempotencyRecord struct {
	PatientID      uuid.UUID
	Key            string
	Fingerprint    string
	AppointmentID  uuid.UUID
	ResponseStatus int
	Snapshot       []byte
	ExpiresAt      time.Time
}

// Page is a bounded offset window over a collection.
type Page struct {
	Limit  int32
	Offset int32
}

// Repository-level sentinel errors.
//
// These are returned by the persistence adapter and translated into
// apperror.Error values by the service, which keeps PostgreSQL specifics
// (constraint names, SQLSTATE codes) out of the domain.
var (
	// ErrSlotTaken means the database's partial unique index rejected a write
	// because another active appointment already holds that doctor and start.
	// This — not any earlier availability read — is the authority on conflicts.
	ErrSlotTaken = errors.New("appointment: slot already taken")

	// ErrIdempotencyKeyExists means a concurrent request committed the same
	// (patient, key) pair first.
	ErrIdempotencyKeyExists = errors.New("appointment: idempotency key already exists")

	// ErrNotFound means no appointment row matched.
	ErrNotFound = errors.New("appointment: not found")

	// ErrNotActive means the row exists but was not in the 'booked' state when
	// a state-changing statement tried to match it.
	ErrNotActive = errors.New("appointment: not active")
)

// Canonical domain errors. Each maps to a stable API code documented in
// api/openapi.yaml.

// ErrAppointmentNotFound is returned when an appointment ID does not resolve.
func ErrAppointmentNotFound() *apperror.Error {
	return apperror.New(apperror.KindNotFound, apperror.CodeAppointmentNotFound,
		"No appointment exists with that identifier.")
}

// ErrSlotUnavailable is returned when the requested slot is already held by
// another active appointment.
func ErrSlotUnavailable() *apperror.Error {
	return apperror.New(apperror.KindConflict, apperror.CodeSlotUnavailable,
		"That slot is no longer available.")
}

// ErrAlreadyCancelled is returned when a cancelled appointment is cancelled or
// rescheduled again.
func ErrAlreadyCancelled() *apperror.Error {
	return apperror.New(apperror.KindConflict, apperror.CodeAlreadyCancelled,
		"This appointment has already been cancelled.")
}

// ErrRescheduleSameSlot is returned when a reschedule names the slot the
// appointment already occupies.
//
// This is modelled as a conflict rather than a no-op success: the request
// collides with the appointment's own current state, and treating it as a
// success would append a misleading 'rescheduled' audit event describing a move
// that never happened. See docs/adr/0006-reschedule-semantics.md.
func ErrRescheduleSameSlot() *apperror.Error {
	return apperror.New(apperror.KindConflict, apperror.CodeRescheduleSameSlot,
		"The appointment is already booked at that time.")
}

// ErrIdempotencyKeyReuse is returned when a key is replayed with a different
// payload.
func ErrIdempotencyKeyReuse() *apperror.Error {
	return apperror.New(apperror.KindConflict, apperror.CodeIdempotencyKeyReuse,
		"This Idempotency-Key was already used for a different booking request.")
}
