package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The demo dataset.
//
// Every identifier is fixed rather than generated, for three reasons: the
// README can publish working IDs, smoke tests can address a known doctor
// without a lookup, and re-running the seed is a no-op instead of a duplicate.
// All people are fictitious and all addresses use the reserved example.com
// domain.

// seedDoctor is one clinician plus their weekly template.
type seedDoctor struct {
	ID        uuid.UUID
	Slug      string
	FullName  string
	Specialty string
	Timezone  string
	Hours     []seedHours
}

// seedHours is a working-hours interval on one weekday, in local wall time.
type seedHours struct {
	Weekday time.Weekday
	Start   string
	End     string
}

// weekdays is shorthand for the common "same hours every weekday" pattern.
func weekdays(start, end string, days ...time.Weekday) []seedHours {
	hours := make([]seedHours, 0, len(days))
	for _, day := range days {
		hours = append(hours, seedHours{Weekday: day, Start: start, End: end})
	}
	return hours
}

var (
	monToFri = []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}
	tueToSat = []time.Weekday{time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday}
	monToThu = []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday}
)

// seedDoctors is the clinic's five doctors.
//
// Dr. Kiptoo is deliberately in Europe/London rather than Africa/Nairobi. A
// clinic with every doctor in one zone would let a timezone bug hide forever;
// this one has a DST transition, so availability and booking are exercised
// against a moving UTC offset in both the tests and a live demo.
var seedDoctors = []seedDoctor{
	{
		ID:        uuid.MustParse("7f3c0a1e-1111-4a10-9c01-000000000001"),
		Slug:      "amina-wanjiru",
		FullName:  "Dr. Amina Wanjiru",
		Specialty: "General Practice",
		Timezone:  "Africa/Nairobi",
		Hours: append(
			weekdays("09:00", "13:00", monToFri...),
			weekdays("14:00", "17:00", monToFri...)...,
		),
	},
	{
		ID:        uuid.MustParse("7f3c0a1e-1111-4a10-9c01-000000000002"),
		Slug:      "otieno-mwangi",
		FullName:  "Dr. Otieno Mwangi",
		Specialty: "Paediatrics",
		Timezone:  "Africa/Nairobi",
		Hours: append(
			weekdays("08:00", "12:00", time.Monday, time.Wednesday, time.Friday),
			weekdays("13:00", "17:00", time.Tuesday, time.Thursday)...,
		),
	},
	{
		ID:        uuid.MustParse("7f3c0a1e-1111-4a10-9c01-000000000003"),
		Slug:      "fatuma-hassan",
		FullName:  "Dr. Fatuma Hassan",
		Specialty: "Dermatology",
		Timezone:  "Africa/Nairobi",
		Hours:     weekdays("10:00", "16:00", tueToSat...),
	},
	{
		ID:        uuid.MustParse("7f3c0a1e-1111-4a10-9c01-000000000004"),
		Slug:      "njeri-kamau",
		FullName:  "Dr. Njeri Kamau",
		Specialty: "Cardiology",
		Timezone:  "Africa/Nairobi",
		Hours:     weekdays("08:30", "12:30", monToThu...),
	},
	{
		ID:        uuid.MustParse("7f3c0a1e-1111-4a10-9c01-000000000005"),
		Slug:      "samuel-kiptoo",
		FullName:  "Dr. Samuel Kiptoo",
		Specialty: "Physiotherapy",
		Timezone:  "Europe/London",
		Hours: append(
			weekdays("09:00", "12:00", monToFri...),
			weekdays("13:00", "16:30", monToFri...)...,
		),
	},
}

// seedPatient is one directory record.
type seedPatient struct {
	ID       uuid.UUID
	FullName string
	Email    string
	IsActive bool
}

// seedPatients includes one deactivated record so the patient_inactive
// rejection path can be demonstrated against a running service, not only in
// tests.
var seedPatients = []seedPatient{
	{uuid.MustParse("9b2d5e40-2222-4b20-8d02-000000000001"), "Grace Achieng", "grace.achieng@example.com", true},
	{uuid.MustParse("9b2d5e40-2222-4b20-8d02-000000000002"), "Brian Omondi", "brian.omondi@example.com", true},
	{uuid.MustParse("9b2d5e40-2222-4b20-8d02-000000000003"), "Lucy Njoroge", "lucy.njoroge@example.com", true},
	{uuid.MustParse("9b2d5e40-2222-4b20-8d02-000000000004"), "Peter Mutiso", "peter.mutiso@example.com", true},
	{uuid.MustParse("9b2d5e40-2222-4b20-8d02-000000000005"), "Halima Yusuf", "halima.yusuf@example.com", true},
	{uuid.MustParse("9b2d5e40-2222-4b20-8d02-000000000006"), "Daniel Kariuki", "daniel.kariuki@example.com", false},
}

// seed writes the demo dataset.
//
// It is idempotent and safe to run on every deployment: doctors and patients
// are upserted by primary key, and a doctor's working hours are replaced
// wholesale so removing an interval from this file actually removes it from the
// database. Appointments are never touched — seeding must not destroy data a
// reviewer just created.
func seed(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	// Fail before touching the database if a timezone is wrong. A doctor with
	// an unloadable zone would break every request that touches them.
	for _, doc := range seedDoctors {
		if _, err := time.LoadLocation(doc.Timezone); err != nil {
			return fmt.Errorf("doctor %s has an invalid IANA timezone %q: %w", doc.Slug, doc.Timezone, err)
		}
		for _, hours := range doc.Hours {
			if err := validateInterval(hours); err != nil {
				return fmt.Errorf("doctor %s: %w", doc.Slug, err)
			}
		}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin seed transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, doc := range seedDoctors {
		if err := upsertDoctor(ctx, tx, doc); err != nil {
			return err
		}
	}
	for _, pat := range seedPatients {
		if err := upsertPatient(ctx, tx, pat); err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit seed transaction: %w", err)
	}

	logger.Info("seed complete",
		slog.Int("doctors", len(seedDoctors)),
		slog.Int("patients", len(seedPatients)),
	)
	return nil
}

func upsertDoctor(ctx context.Context, tx pgx.Tx, doc seedDoctor) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO doctors (id, slug, full_name, specialty, timezone, is_active)
		VALUES ($1, $2, $3, $4, $5, true)
		ON CONFLICT (id) DO UPDATE SET
			slug = EXCLUDED.slug,
			full_name = EXCLUDED.full_name,
			specialty = EXCLUDED.specialty,
			timezone = EXCLUDED.timezone,
			is_active = EXCLUDED.is_active
	`, doc.ID, doc.Slug, doc.FullName, doc.Specialty, doc.Timezone); err != nil {
		return fmt.Errorf("upsert doctor %s: %w", doc.Slug, err)
	}

	// Replace rather than merge, so this file is the whole truth about a
	// doctor's schedule.
	if _, err := tx.Exec(ctx, `DELETE FROM doctor_working_hours WHERE doctor_id = $1`, doc.ID); err != nil {
		return fmt.Errorf("clear working hours for %s: %w", doc.Slug, err)
	}

	for _, hours := range doc.Hours {
		if _, err := tx.Exec(ctx, `
			INSERT INTO doctor_working_hours (doctor_id, weekday, starts_at_local, ends_at_local)
			VALUES ($1, $2, $3::time, $4::time)
		`, doc.ID, weekdayColumn(hours.Weekday), hours.Start, hours.End); err != nil {
			return fmt.Errorf("insert working hours for %s (%s %s-%s): %w",
				doc.Slug, hours.Weekday, hours.Start, hours.End, err)
		}
	}
	return nil
}

func upsertPatient(ctx context.Context, tx pgx.Tx, pat seedPatient) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO patients (id, full_name, email, is_active)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE SET
			full_name = EXCLUDED.full_name,
			email = EXCLUDED.email,
			is_active = EXCLUDED.is_active
	`, pat.ID, pat.FullName, pat.Email, pat.IsActive); err != nil {
		return fmt.Errorf("upsert patient %s: %w", pat.ID, err)
	}
	return nil
}

// weekdayColumn narrows a time.Weekday for the smallint column.
//
// time.Weekday is constrained to 0..6 by the standard library, so the
// conversion cannot lose information; the mask makes that provable to a reader
// and to static analysis rather than relying on an unstated invariant.
func weekdayColumn(day time.Weekday) int16 {
	return int16(day % 7)
}

// validateInterval catches a bad interval here, with a message naming the
// doctor, rather than as a CHECK constraint violation from PostgreSQL.
func validateInterval(hours seedHours) error {
	start, err := time.Parse("15:04", hours.Start)
	if err != nil {
		return fmt.Errorf("working hours start %q is not HH:MM", hours.Start)
	}
	end, err := time.Parse("15:04", hours.End)
	if err != nil {
		return fmt.Errorf("working hours end %q is not HH:MM", hours.End)
	}
	if !end.After(start) {
		return fmt.Errorf("working hours %s-%s do not end after they start", hours.Start, hours.End)
	}
	if start.Minute()%30 != 0 || end.Minute()%30 != 0 {
		return fmt.Errorf("working hours %s-%s are not on a 30-minute boundary", hours.Start, hours.End)
	}
	return nil
}

// purgeIdempotency deletes expired replay records.
//
// Kept as an explicit command rather than a background goroutine in the API:
// a scheduled job is visible, has logs, and cannot silently stop running the
// way a forgotten goroutine can.
func purgeIdempotency(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	tag, err := pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE expires_at <= now()`)
	if err != nil {
		return fmt.Errorf("purge expired idempotency records: %w", err)
	}
	logger.Info("purged expired idempotency records", slog.Int64("deleted", tag.RowsAffected()))
	return nil
}
