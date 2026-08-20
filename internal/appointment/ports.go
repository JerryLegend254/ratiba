package appointment

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/JerryLegend254/ratiba/internal/doctor"
	"github.com/JerryLegend254/ratiba/internal/patient"
)

// Repository is the appointment service's persistence port.
//
// Read methods run outside a transaction. Anything that changes state goes
// through WithinTx, which is the only place the service is allowed to write.
type Repository interface {
	// WithinTx runs fn inside a single database transaction, committing when fn
	// returns nil and rolling back otherwise. The Tx handed to fn is only valid
	// for the duration of the call.
	WithinTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error

	// Get returns one appointment, or ErrNotFound.
	Get(ctx context.Context, id uuid.UUID) (Appointment, error)

	// ListBookedStarts returns the start instants of active appointments for a
	// doctor within the half-open range [from, to).
	ListBookedStarts(ctx context.Context, doctorID uuid.UUID, from, to time.Time) ([]time.Time, error)

	// ListUpcomingForPatient returns a page of the patient's future active
	// appointments ordered by (starts_at, id), plus the unpaged total.
	ListUpcomingForPatient(ctx context.Context, patientID uuid.UUID, from time.Time, page Page) ([]PatientAppointment, int64, error)

	// FindIdempotencyRecord looks up a stored booking response. found is false
	// when no record exists for the (patient, key) pair.
	FindIdempotencyRecord(ctx context.Context, patientID uuid.UUID, key string) (record IdempotencyRecord, found bool, err error)
}

// Tx is the transactional write port. Every method here participates in the
// enclosing WithinTx transaction.
type Tx interface {
	// LockAppointment reads an appointment with a row lock held until the
	// transaction ends (SELECT ... FOR UPDATE). Returns ErrNotFound if absent.
	LockAppointment(ctx context.Context, id uuid.UUID) (Appointment, error)

	// Create inserts an active appointment occupying slot. Returns ErrSlotTaken
	// when the database's partial unique index rejects it.
	Create(ctx context.Context, doctorID, patientID uuid.UUID, slot Slot) (Appointment, error)

	// Cancel moves a booked appointment to cancelled. Returns ErrNotActive when
	// the row was not in the booked state.
	Cancel(ctx context.Context, id uuid.UUID, reason string, at time.Time) (Appointment, error)

	// Move relocates a booked appointment to slot. Releasing the old slot and
	// claiming the new one happen in this single statement, so the unique index
	// sees one atomic change. Returns ErrSlotTaken on conflict.
	Move(ctx context.Context, id uuid.UUID, slot Slot) (Appointment, error)

	// AppendEvent writes one append-only history entry.
	AppendEvent(ctx context.Context, event Event) error

	// SaveIdempotencyRecord persists a booking response for replay. Returns
	// ErrIdempotencyKeyExists when a concurrent transaction committed first.
	SaveIdempotencyRecord(ctx context.Context, record IdempotencyRecord) error
}

// ScheduleReader is the narrow slice of doctor.Repository this service needs.
// Declaring it here rather than importing the full repository keeps the
// dependency honest and makes test doubles trivial.
type ScheduleReader interface {
	GetByID(ctx context.Context, id uuid.UUID) (doctor.Doctor, error)
	ScheduleFor(ctx context.Context, id uuid.UUID) (doctor.Schedule, error)
}

// PatientReader is the narrow slice of patient.Repository this service needs.
type PatientReader interface {
	GetByID(ctx context.Context, id uuid.UUID) (patient.Patient, error)
}
