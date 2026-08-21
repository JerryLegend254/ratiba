// Package doctor models clinicians and the weekly template that describes when
// they see patients.
//
// The package deliberately knows nothing about appointments. It answers one
// question — "which absolute time windows is this doctor available in on a
// given local date?" — and leaves slot arithmetic to the appointment package.
package doctor

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/JerryLegend254/ratiba/internal/platform/apperror"
	"github.com/JerryLegend254/ratiba/internal/platform/calendar"
)

// Doctor is a clinician who accepts appointments.
type Doctor struct {
	ID        uuid.UUID
	Slug      string
	FullName  string
	Specialty string
	// Timezone is an IANA zone name (for example "Africa/Nairobi"). It is the
	// authority for interpreting this doctor's working hours and for deciding
	// which calendar day an instant belongs to. The host machine's timezone is
	// never consulted.
	Timezone  string
	IsActive  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Location resolves the doctor's IANA timezone.
//
// The zone database is embedded into the binary (see the time/tzdata import in
// cmd/api), so this works in a scratch container with no system tzdata. A zone
// that cannot be resolved is a data-integrity problem, not a client error, but
// it is reported as a typed error so the API can answer with something better
// than a bare 500.
func (d Doctor) Location() (*time.Location, error) {
	loc, err := time.LoadLocation(d.Timezone)
	if err != nil {
		return nil, apperror.New(
			apperror.KindInternal,
			apperror.CodeUnsupportedTimezone,
			"The doctor's configured timezone could not be resolved.",
		).WithCause(fmt.Errorf("load location %q: %w", d.Timezone, err))
	}
	return loc, nil
}

// LocalTime is a wall-clock time of day with no date and no timezone. It is how
// working hours are expressed: "this doctor starts at 09:00 local", regardless
// of what UTC offset that happens to be on a particular day.
type LocalTime struct {
	Hour   int
	Minute int
}

// ParseLocalTime parses a strict "HH:MM" 24-hour wall-clock time.
func ParseLocalTime(s string) (LocalTime, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return LocalTime{}, fmt.Errorf("parse local time %q: %w", s, err)
	}
	return LocalTime{Hour: t.Hour(), Minute: t.Minute()}, nil
}

// String renders the time as HH:MM.
func (l LocalTime) String() string { return fmt.Sprintf("%02d:%02d", l.Hour, l.Minute) }

// MinuteOfDay returns minutes elapsed since local midnight.
func (l LocalTime) MinuteOfDay() int { return l.Hour*60 + l.Minute }

// WorkingHours is one half-open [Start, End) interval on one weekday of the
// doctor's recurring week. Intervals never cross midnight; the database
// enforces Start < End and that the two are 30-minute aligned.
type WorkingHours struct {
	Weekday time.Weekday
	Start   LocalTime
	End     LocalTime
}

// Window is an absolute half-open [Start, End) interval. Turning wall-clock
// working hours into windows is where a doctor's timezone is applied.
type Window struct {
	Start time.Time
	End   time.Time
}

// Schedule is a doctor together with their full weekly availability template.
type Schedule struct {
	Doctor       Doctor
	WorkingHours []WorkingHours
}

// WindowsOn returns the absolute time windows the doctor works during on the
// given local calendar date, ordered by start time.
//
// Because working-hours intervals cannot cross midnight, every window returned
// begins and ends on the requested local date, which is what lets the rest of
// the system treat "which day does this slot belong to?" as a single-day
// question.
//
// Around a DST transition the returned window can be shorter or longer than the
// wall-clock difference suggests — 09:00 to 17:00 across a spring-forward is
// seven real hours, not eight. That is correct: appointments consume real time,
// so the appointment package fits whole 30-minute slots into whatever real
// duration the window has.
func (s Schedule) WindowsOn(date calendar.Date, loc *time.Location) []Window {
	weekday := date.Weekday()

	windows := make([]Window, 0, 2)
	for _, wh := range s.WorkingHours {
		if wh.Weekday != weekday {
			continue
		}
		start := date.AtTime(wh.Start.Hour, wh.Start.Minute, loc)
		end := date.AtTime(wh.End.Hour, wh.End.Minute, loc)
		// A DST jump can collapse or invert a short interval once mapped to
		// absolute time. Such a window holds no slots, so drop it rather than
		// let a negative duration reach the slot generator.
		if !end.After(start) {
			continue
		}
		windows = append(windows, Window{Start: start, End: end})
	}

	sort.Slice(windows, func(i, j int) bool { return windows[i].Start.Before(windows[j].Start) })
	return windows
}

// Repository reads doctors and their schedules.
type Repository interface {
	// GetByID returns the doctor, or an apperror with CodeDoctorNotFound.
	GetByID(ctx context.Context, id uuid.UUID) (Doctor, error)
	// GetBySlug returns the doctor addressed by their stable handle.
	GetBySlug(ctx context.Context, slug string) (Doctor, error)
	// ScheduleFor returns the doctor plus their weekly working hours.
	ScheduleFor(ctx context.Context, id uuid.UUID) (Schedule, error)
	// List returns a bounded page of doctors together with the total count.
	List(ctx context.Context, activeOnly bool, limit, offset int32) ([]Doctor, int64, error)
}

// ErrNotFound is the canonical "no such doctor" error.
func ErrNotFound() *apperror.Error {
	return apperror.New(apperror.KindNotFound, apperror.CodeDoctorNotFound, "No doctor exists with that identifier.")
}

// ErrInactive is returned when a doctor exists but no longer accepts bookings.
func ErrInactive() *apperror.Error {
	return apperror.New(apperror.KindUnprocessable, apperror.CodeDoctorInactive, "This doctor is not currently accepting appointments.")
}
