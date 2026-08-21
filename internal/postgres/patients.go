package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/JerryLegend254/ratiba/internal/patient"
	"github.com/JerryLegend254/ratiba/internal/platform/apperror"
	"github.com/JerryLegend254/ratiba/internal/postgres/sqlcgen"
)

// PatientRepository reads patients.
type PatientRepository struct {
	queries *sqlcgen.Queries
}

var _ patient.Repository = (*PatientRepository)(nil)

// GetByID implements patient.Repository.
func (r *PatientRepository) GetByID(ctx context.Context, id uuid.UUID) (patient.Patient, error) {
	row, err := r.queries.GetPatientByID(ctx, id)
	if err != nil {
		if noRows(err) {
			return patient.Patient{}, patient.ErrNotFound()
		}
		return patient.Patient{}, apperror.Internal(fmt.Errorf("get patient: %w", err))
	}
	return toPatient(row), nil
}

// List implements patient.Repository.
func (r *PatientRepository) List(ctx context.Context, activeOnly bool, limit, offset int32) ([]patient.Patient, int64, error) {
	rows, err := r.queries.ListPatients(ctx, sqlcgen.ListPatientsParams{
		Limit: limit, Offset: offset, ActiveOnly: activeOnly,
	})
	if err != nil {
		return nil, 0, apperror.Internal(fmt.Errorf("list patients: %w", err))
	}
	total, err := r.queries.CountPatients(ctx, activeOnly)
	if err != nil {
		return nil, 0, apperror.Internal(fmt.Errorf("count patients: %w", err))
	}

	patients := make([]patient.Patient, 0, len(rows))
	for _, row := range rows {
		patients = append(patients, toPatient(row))
	}
	return patients, total, nil
}

func toPatient(row sqlcgen.Patient) patient.Patient {
	return patient.Patient{
		ID:        row.ID,
		FullName:  row.FullName,
		Email:     row.Email,
		IsActive:  row.IsActive,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}
