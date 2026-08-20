package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JerryLegend254/ratiba/internal/appointment"
	"github.com/JerryLegend254/ratiba/internal/postgres/sqlcgen"
)

// AppointmentRepository is the appointment aggregate's persistence adapter.
type AppointmentRepository struct {
	pool    *pgxpool.Pool
	queries *sqlcgen.Queries
}

var _ appointment.Repository = (*AppointmentRepository)(nil)

// Get implements appointment.Repository.
func (r *AppointmentRepository) Get(ctx context.Context, id uuid.UUID) (appointment.Appointment, error) {
	row, err := r.queries.GetAppointmentByID(ctx, id)
	if err != nil {
		if noRows(err) {
			return appointment.Appointment{}, appointment.ErrNotFound
		}
		return appointment.Appointment{}, fmt.Errorf("get appointment: %w", err)
	}
	return toAppointment(row), nil
}

// ListBookedStarts implements appointment.Repository.
func (r *AppointmentRepository) ListBookedStarts(ctx context.Context, doctorID uuid.UUID, from, to time.Time) ([]time.Time, error) {
	starts, err := r.queries.ListBookedStartsForDoctorInRange(ctx, sqlcgen.ListBookedStartsForDoctorInRangeParams{
		DoctorID:   doctorID,
		RangeStart: from,
		RangeEnd:   to,
	})
	if err != nil {
		return nil, fmt.Errorf("list booked starts: %w", err)
	}
	return starts, nil
}

// ListUpcomingForPatient implements appointment.Repository.
//
// The page and the total are read in two statements outside a transaction, so
// a booking committed between them could make the total disagree with the page
// by one. That is acceptable for a "how many upcoming appointments do I have?"
// counter and avoids holding a transaction open for a read-only listing.
func (r *AppointmentRepository) ListUpcomingForPatient(
	ctx context.Context,
	patientID uuid.UUID,
	from time.Time,
	page appointment.Page,
) ([]appointment.PatientAppointment, int64, error) {
	rows, err := r.queries.ListUpcomingAppointmentsForPatient(ctx, sqlcgen.ListUpcomingAppointmentsForPatientParams{
		PatientID: patientID,
		FromTime:  from,
		Limit:     page.Limit,
		Offset:    page.Offset,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list upcoming appointments: %w", err)
	}

	total, err := r.queries.CountUpcomingAppointmentsForPatient(ctx, sqlcgen.CountUpcomingAppointmentsForPatientParams{
		PatientID: patientID,
		FromTime:  from,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("count upcoming appointments: %w", err)
	}

	items := make([]appointment.PatientAppointment, 0, len(rows))
	for _, row := range rows {
		items = append(items, appointment.PatientAppointment{
			Appointment: appointment.Appointment{
				ID:                 row.ID,
				DoctorID:           row.DoctorID,
				PatientID:          row.PatientID,
				StartsAt:           row.StartsAt,
				EndsAt:             row.EndsAt,
				Status:             appointment.Status(row.Status),
				CancellationReason: row.CancellationReason,
				CancelledAt:        row.CancelledAt,
				CreatedAt:          row.CreatedAt,
				UpdatedAt:          row.UpdatedAt,
			},
			Doctor: appointment.DoctorSummary{
				ID:        row.DoctorID,
				Slug:      row.DoctorSlug,
				FullName:  row.DoctorFullName,
				Specialty: row.DoctorSpecialty,
				Timezone:  row.DoctorTimezone,
			},
		})
	}
	return items, total, nil
}

// FindIdempotencyRecord implements appointment.Repository.
func (r *AppointmentRepository) FindIdempotencyRecord(
	ctx context.Context,
	patientID uuid.UUID,
	key string,
) (appointment.IdempotencyRecord, bool, error) {
	row, err := r.queries.GetIdempotencyRecord(ctx, sqlcgen.GetIdempotencyRecordParams{
		PatientID: patientID, IdempotencyKey: key,
	})
	if err != nil {
		if noRows(err) {
			return appointment.IdempotencyRecord{}, false, nil
		}
		return appointment.IdempotencyRecord{}, false, fmt.Errorf("get idempotency record: %w", err)
	}
	return toIdempotencyRecord(row), true, nil
}

// PurgeExpiredIdempotencyRecords deletes records past their TTL and returns how
// many were removed. Exposed for the maintenance command described in
// docs/operations.md.
func (r *AppointmentRepository) PurgeExpiredIdempotencyRecords(ctx context.Context, now time.Time) (int64, error) {
	deleted, err := r.queries.DeleteExpiredIdempotencyRecords(ctx, now)
	if err != nil {
		return 0, fmt.Errorf("purge expired idempotency records: %w", err)
	}
	return deleted, nil
}

// txRepository implements appointment.Tx against a single transaction.
type txRepository struct {
	queries *sqlcgen.Queries
}

var _ appointment.Tx = (*txRepository)(nil)

// LockAppointment implements appointment.Tx.
func (t *txRepository) LockAppointment(ctx context.Context, id uuid.UUID) (appointment.Appointment, error) {
	row, err := t.queries.GetAppointmentByIDForUpdate(ctx, id)
	if err != nil {
		if noRows(err) {
			return appointment.Appointment{}, appointment.ErrNotFound
		}
		return appointment.Appointment{}, fmt.Errorf("lock appointment: %w", err)
	}
	return toAppointment(row), nil
}

// Create implements appointment.Tx.
func (t *txRepository) Create(ctx context.Context, doctorID, patientID uuid.UUID, slot appointment.Slot) (appointment.Appointment, error) {
	row, err := t.queries.CreateAppointment(ctx, sqlcgen.CreateAppointmentParams{
		DoctorID:  doctorID,
		PatientID: patientID,
		StartsAt:  slot.Start,
		EndsAt:    slot.End,
	})
	if err != nil {
		return appointment.Appointment{}, translateWriteError(fmt.Errorf("create appointment: %w", err))
	}
	return toAppointment(row), nil
}

// Cancel implements appointment.Tx.
func (t *txRepository) Cancel(ctx context.Context, id uuid.UUID, reason string, at time.Time) (appointment.Appointment, error) {
	row, err := t.queries.CancelAppointment(ctx, sqlcgen.CancelAppointmentParams{
		ID:                 id,
		CancellationReason: &reason,
		CancelledAt:        &at,
	})
	if err != nil {
		if noRows(err) {
			// The statement matched no row. The caller holds a lock on an
			// existing row, so the only explanation is that it is not booked.
			return appointment.Appointment{}, appointment.ErrNotActive
		}
		return appointment.Appointment{}, fmt.Errorf("cancel appointment: %w", err)
	}
	return toAppointment(row), nil
}

// Move implements appointment.Tx.
func (t *txRepository) Move(ctx context.Context, id uuid.UUID, slot appointment.Slot) (appointment.Appointment, error) {
	row, err := t.queries.RescheduleAppointment(ctx, sqlcgen.RescheduleAppointmentParams{
		ID:       id,
		StartsAt: slot.Start,
		EndsAt:   slot.End,
	})
	if err != nil {
		if noRows(err) {
			return appointment.Appointment{}, appointment.ErrNotActive
		}
		return appointment.Appointment{}, translateWriteError(fmt.Errorf("reschedule appointment: %w", err))
	}
	return toAppointment(row), nil
}

// AppendEvent implements appointment.Tx.
func (t *txRepository) AppendEvent(ctx context.Context, event appointment.Event) error {
	if err := t.queries.InsertAppointmentEvent(ctx, sqlcgen.InsertAppointmentEventParams{
		AppointmentID: event.AppointmentID,
		EventType:     sqlcgen.AppointmentEventType(event.Type),
		FromStartsAt:  event.FromStartsAt,
		ToStartsAt:    event.ToStartsAt,
		Source:        event.Source,
	}); err != nil {
		return fmt.Errorf("append appointment event: %w", err)
	}
	return nil
}

// SaveIdempotencyRecord implements appointment.Tx.
func (t *txRepository) SaveIdempotencyRecord(ctx context.Context, record appointment.IdempotencyRecord) error {
	if _, err := t.queries.CreateIdempotencyRecord(ctx, sqlcgen.CreateIdempotencyRecordParams{
		PatientID:          record.PatientID,
		IdempotencyKey:     record.Key,
		RequestFingerprint: record.Fingerprint,
		AppointmentID:      record.AppointmentID,
		ResponseStatus:     int16(record.ResponseStatus), //nolint:gosec // bounded to 100..599 by a CHECK constraint
		ResponseBody:       record.Snapshot,
		ExpiresAt:          record.ExpiresAt,
	}); err != nil {
		return translateWriteError(fmt.Errorf("save idempotency record: %w", err))
	}
	return nil
}

func toAppointment(row sqlcgen.Appointment) appointment.Appointment {
	return appointment.Appointment{
		ID:                 row.ID,
		DoctorID:           row.DoctorID,
		PatientID:          row.PatientID,
		StartsAt:           row.StartsAt,
		EndsAt:             row.EndsAt,
		Status:             appointment.Status(row.Status),
		CancellationReason: row.CancellationReason,
		CancelledAt:        row.CancelledAt,
		CreatedAt:          row.CreatedAt,
		UpdatedAt:          row.UpdatedAt,
	}
}

func toIdempotencyRecord(row sqlcgen.IdempotencyKey) appointment.IdempotencyRecord {
	return appointment.IdempotencyRecord{
		PatientID:      row.PatientID,
		Key:            row.IdempotencyKey,
		Fingerprint:    row.RequestFingerprint,
		AppointmentID:  row.AppointmentID,
		ResponseStatus: int(row.ResponseStatus),
		Snapshot:       row.ResponseBody,
		ExpiresAt:      row.ExpiresAt,
	}
}
