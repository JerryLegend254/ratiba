package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/JerryLegend254/ratiba/internal/doctor"
	"github.com/JerryLegend254/ratiba/internal/platform/apperror"
	"github.com/JerryLegend254/ratiba/internal/postgres/sqlcgen"
)

// DoctorRepository reads doctors and their weekly schedules.
type DoctorRepository struct {
	queries *sqlcgen.Queries
}

var _ doctor.Repository = (*DoctorRepository)(nil)

// GetByID implements doctor.Repository.
func (r *DoctorRepository) GetByID(ctx context.Context, id uuid.UUID) (doctor.Doctor, error) {
	row, err := r.queries.GetDoctorByID(ctx, id)
	if err != nil {
		if noRows(err) {
			return doctor.Doctor{}, doctor.ErrNotFound()
		}
		return doctor.Doctor{}, apperror.Internal(fmt.Errorf("get doctor: %w", err))
	}
	return toDoctor(row), nil
}

// GetBySlug implements doctor.Repository.
func (r *DoctorRepository) GetBySlug(ctx context.Context, slug string) (doctor.Doctor, error) {
	row, err := r.queries.GetDoctorBySlug(ctx, slug)
	if err != nil {
		if noRows(err) {
			return doctor.Doctor{}, doctor.ErrNotFound()
		}
		return doctor.Doctor{}, apperror.Internal(fmt.Errorf("get doctor by slug: %w", err))
	}
	return toDoctor(row), nil
}

// ScheduleFor implements doctor.Repository.
//
// A doctor with no working-hours rows is legitimate — a locum who has not been
// rostered yet — and yields an empty schedule rather than an error. Booking
// against them fails with doctor_not_working_on_date, which is the accurate
// answer.
func (r *DoctorRepository) ScheduleFor(ctx context.Context, id uuid.UUID) (doctor.Schedule, error) {
	doc, err := r.GetByID(ctx, id)
	if err != nil {
		return doctor.Schedule{}, err
	}

	rows, err := r.queries.ListWorkingHoursByDoctor(ctx, id)
	if err != nil {
		return doctor.Schedule{}, apperror.Internal(fmt.Errorf("list working hours: %w", err))
	}

	hours := make([]doctor.WorkingHours, 0, len(rows))
	for _, row := range rows {
		start, err := toLocalTime(row.StartsAtLocal)
		if err != nil {
			return doctor.Schedule{}, apperror.Internal(fmt.Errorf("working hours start: %w", err))
		}
		end, err := toLocalTime(row.EndsAtLocal)
		if err != nil {
			return doctor.Schedule{}, apperror.Internal(fmt.Errorf("working hours end: %w", err))
		}
		hours = append(hours, doctor.WorkingHours{
			Weekday: time.Weekday(row.Weekday),
			Start:   start,
			End:     end,
		})
	}

	return doctor.Schedule{Doctor: doc, WorkingHours: hours}, nil
}

// List implements doctor.Repository.
func (r *DoctorRepository) List(ctx context.Context, activeOnly bool, limit, offset int32) ([]doctor.Doctor, int64, error) {
	rows, err := r.queries.ListDoctors(ctx, sqlcgen.ListDoctorsParams{
		Limit: limit, Offset: offset, ActiveOnly: activeOnly,
	})
	if err != nil {
		return nil, 0, apperror.Internal(fmt.Errorf("list doctors: %w", err))
	}
	total, err := r.queries.CountDoctors(ctx, activeOnly)
	if err != nil {
		return nil, 0, apperror.Internal(fmt.Errorf("count doctors: %w", err))
	}

	doctors := make([]doctor.Doctor, 0, len(rows))
	for _, row := range rows {
		doctors = append(doctors, toDoctor(row))
	}
	return doctors, total, nil
}

func toDoctor(row sqlcgen.Doctor) doctor.Doctor {
	return doctor.Doctor{
		ID:        row.ID,
		Slug:      row.Slug,
		FullName:  row.FullName,
		Specialty: row.Specialty,
		Timezone:  row.Timezone,
		IsActive:  row.IsActive,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}

// toLocalTime converts PostgreSQL's `time` (microseconds since midnight) into a
// wall-clock hour and minute.
//
// Sub-minute precision is impossible here: a CHECK constraint requires working
// hours to sit on a 30-minute boundary with zero seconds.
func toLocalTime(t pgtype.Time) (doctor.LocalTime, error) {
	if !t.Valid {
		return doctor.LocalTime{}, fmt.Errorf("unexpected NULL time value")
	}
	minutes := t.Microseconds / int64(time.Minute/time.Microsecond)
	return doctor.LocalTime{Hour: int(minutes / 60), Minute: int(minutes % 60)}, nil
}
