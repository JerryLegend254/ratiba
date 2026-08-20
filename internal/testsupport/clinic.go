package testsupport

import (
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/JerryLegend254/ratiba/internal/appointment"
	"github.com/JerryLegend254/ratiba/internal/doctor"
	"github.com/JerryLegend254/ratiba/internal/patient"
	"github.com/JerryLegend254/ratiba/internal/platform/clock"
	"github.com/JerryLegend254/ratiba/internal/platform/logging"
)

// Fixed identities used across the test suite.
//
// Stable IDs make failures readable: a diff that says "expected
// 7f3c0a1e-...-0001" is far easier to interpret than one full of freshly
// generated UUIDs.
var (
	// NairobiDoctorID works 09:00-13:00 and 14:00-17:00, Monday to Friday,
	// in Africa/Nairobi (UTC+3, no DST).
	NairobiDoctorID = uuid.MustParse("7f3c0a1e-1111-4a10-9c01-000000000001")
	// LondonDoctorID works 09:00-17:00 Monday to Friday in Europe/London,
	// which observes DST. Used for timezone and DST tests.
	LondonDoctorID = uuid.MustParse("7f3c0a1e-1111-4a10-9c01-000000000005")
	// InactiveDoctorID exists but does not accept appointments.
	InactiveDoctorID = uuid.MustParse("7f3c0a1e-1111-4a10-9c01-00000000000f")

	// ActivePatientID can book.
	ActivePatientID = uuid.MustParse("9b2d5e40-2222-4b20-8d02-000000000001")
	// OtherPatientID is a second bookable patient.
	OtherPatientID = uuid.MustParse("9b2d5e40-2222-4b20-8d02-000000000002")
	// InactivePatientID exists but cannot book.
	InactivePatientID = uuid.MustParse("9b2d5e40-2222-4b20-8d02-000000000006")
)

// Weekdays is the Monday-to-Friday set.
var Weekdays = []time.Weekday{
	time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday,
}

// Hours builds working-hours entries for the given weekdays.
func Hours(startHour, startMinute, endHour, endMinute int, days ...time.Weekday) []doctor.WorkingHours {
	hours := make([]doctor.WorkingHours, 0, len(days))
	for _, day := range days {
		hours = append(hours, doctor.WorkingHours{
			Weekday: day,
			Start:   doctor.LocalTime{Hour: startHour, Minute: startMinute},
			End:     doctor.LocalTime{Hour: endHour, Minute: endMinute},
		})
	}
	return hours
}

// NewClinic returns a store populated with the standard test fixture: two
// doctors in different timezones, one inactive doctor, and three patients.
func NewClinic() *MemoryStore {
	store := NewMemoryStore()

	store.AddDoctor(
		doctor.Doctor{
			ID: NairobiDoctorID, Slug: "amina-wanjiru", FullName: "Dr. Amina Wanjiru",
			Specialty: "General Practice", Timezone: "Africa/Nairobi", IsActive: true,
		},
		append(
			Hours(9, 0, 13, 0, Weekdays...),
			Hours(14, 0, 17, 0, Weekdays...)...,
		)...,
	)

	store.AddDoctor(
		doctor.Doctor{
			ID: LondonDoctorID, Slug: "samuel-kiptoo", FullName: "Dr. Samuel Kiptoo",
			Specialty: "Physiotherapy", Timezone: "Europe/London", IsActive: true,
		},
		Hours(9, 0, 17, 0, Weekdays...)...,
	)

	store.AddDoctor(doctor.Doctor{
		ID: InactiveDoctorID, Slug: "retired-locum", FullName: "Dr. Retired Locum",
		Specialty: "General Practice", Timezone: "Africa/Nairobi", IsActive: false,
	})

	store.AddPatient(patient.Patient{
		ID: ActivePatientID, FullName: "Grace Achieng",
		Email: "grace.achieng@example.com", IsActive: true,
	})
	store.AddPatient(patient.Patient{
		ID: OtherPatientID, FullName: "Brian Omondi",
		Email: "brian.omondi@example.com", IsActive: true,
	})
	store.AddPatient(patient.Patient{
		ID: InactivePatientID, FullName: "Daniel Kariuki",
		Email: "daniel.kariuki@example.com", IsActive: false,
	})

	return store
}

// NewService builds an appointment service over the store with a fixed clock
// and no log output.
func NewService(store *MemoryStore, clk clock.Clock) (*appointment.Service, error) {
	return NewServiceWithLogger(store, clk, logging.Discard())
}

// NewServiceWithLogger is NewService with log output a test can inspect, for
// asserting on what the service does and does not write.
func NewServiceWithLogger(store *MemoryStore, clk clock.Clock, logger *slog.Logger) (*appointment.Service, error) {
	return appointment.NewService(
		store.Appointments(),
		store.Doctors(),
		store.Patients(),
		clk,
		logger,
		appointment.NopMetrics{},
		appointment.ServiceConfig{
			Policy:          appointment.DefaultPolicy(),
			IdempotencyTTL:  24 * time.Hour,
			DefaultPageSize: 20,
			MaxPageSize:     100,
		},
	)
}

// NewFixedClock pins time to a known instant.
//
// The default is Monday 2026-09-07 05:00 UTC, which is 08:00 in Nairobi and
// 06:00 in London: an hour before both test doctors open, so the whole working
// day is bookable and the one-hour lead-time rule is satisfied for every slot.
func NewFixedClock() *clock.Fixed {
	return clock.NewFixed(time.Date(2026, 9, 7, 5, 0, 0, 0, time.UTC))
}

// Logger returns a silent logger for tests that do not assert on log output.
func Logger() *slog.Logger { return logging.Discard() }

// MustLocation loads an IANA zone or panics. Only for test setup, where a
// missing zone database is a broken environment rather than a runtime error.
func MustLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic("testsupport: cannot load location " + name + ": " + err.Error())
	}
	return loc
}

// sortByName orders records by a display name with the ID as a tie-breaker, so
// listings are deterministic the way the SQL ORDER BY clauses are.
func sortByName[T any](items []T, key func(T) (string, uuid.UUID)) {
	sort.SliceStable(items, func(i, j int) bool {
		nameI, idI := key(items[i])
		nameJ, idJ := key(items[j])
		if nameI != nameJ {
			return nameI < nameJ
		}
		return idI.String() < idJ.String()
	})
}

// sortSlice is a generic stable sort helper.
func sortSlice[T any](items []T, less func(a, b T) bool) {
	sort.SliceStable(items, func(i, j int) bool { return less(items[i], items[j]) })
}

// paginate applies an offset window, mirroring LIMIT/OFFSET.
func paginate[T any](items []T, limit, offset int32) []T {
	// Widen to int rather than narrowing len() to int32: the slice length is
	// the authority here, and widening is always safe.
	count := len(items)
	start := int(offset)
	if start >= count {
		return []T{}
	}
	end := start + int(limit)
	if end > count {
		end = count
	}
	return items[start:end]
}
