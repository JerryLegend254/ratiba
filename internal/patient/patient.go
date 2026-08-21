// Package patient models the people who book appointments.
//
// It is deliberately thin. Ratiba has no authentication, so a patient here is a
// seeded directory record used to attribute and look up appointments, not an
// identity. See docs/security.md.
package patient

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/JerryLegend254/ratiba/internal/platform/apperror"
)

// Patient is a person who books appointments.
type Patient struct {
	ID        uuid.UUID
	FullName  string
	Email     string
	IsActive  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Repository reads patients.
type Repository interface {
	// GetByID returns the patient, or an apperror with CodePatientNotFound.
	GetByID(ctx context.Context, id uuid.UUID) (Patient, error)
	// List returns a bounded page of patients together with the total count.
	List(ctx context.Context, activeOnly bool, limit, offset int32) ([]Patient, int64, error)
}

// ErrNotFound is the canonical "no such patient" error.
func ErrNotFound() *apperror.Error {
	return apperror.New(apperror.KindNotFound, apperror.CodePatientNotFound, "No patient exists with that identifier.")
}

// ErrInactive is returned when a patient record exists but is deactivated.
func ErrInactive() *apperror.Error {
	return apperror.New(apperror.KindUnprocessable, apperror.CodePatientInactive, "This patient record is not active.")
}
