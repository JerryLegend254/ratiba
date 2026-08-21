//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	ratibadb "github.com/JerryLegend254/ratiba/db"
	"github.com/JerryLegend254/ratiba/internal/appointment"
	"github.com/JerryLegend254/ratiba/internal/platform/apperror"
	"github.com/JerryLegend254/ratiba/internal/platform/calendar"
	"github.com/JerryLegend254/ratiba/internal/platform/logging"
	"github.com/JerryLegend254/ratiba/internal/postgres"
)

// TestDatabaseEnforcesInvariants checks the constraints directly, bypassing the
// application entirely.
//
// The point is that the schema protects the data even against a future code
// path that forgets to validate — which is the whole reason the rules live in
// the database as well as in Go.
// The subtests here deliberately run in sequence: each builds on the state the
// previous one left, which is what lets the last two prove that cancelling
// really does free a slot. They therefore do not call t.Parallel().
func TestDatabaseEnforcesInvariants(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	start := slotAt(t, 9, 0)

	insert := func(startsAt, endsAt time.Time) error {
		_, err := testPool.Exec(ctx, `
			INSERT INTO appointments (doctor_id, patient_id, starts_at, ends_at)
			VALUES ($1, $2, $3, $4)
		`, f.doctorID, f.patients[0], startsAt, endsAt)
		return err
	}

	t.Run("a second active appointment for the same doctor and start is rejected", func(t *testing.T) {
		if err := insert(start, start.Add(30*time.Minute)); err != nil {
			t.Fatalf("first insert should succeed: %v", err)
		}
		if err := insert(start, start.Add(30*time.Minute)); err == nil {
			t.Fatal("the partial unique index did not reject the duplicate")
		}
	})

	t.Run("an appointment of the wrong length is rejected", func(t *testing.T) {
		other := slotAt(t, 14, 0)
		if err := insert(other, other.Add(45*time.Minute)); err == nil {
			t.Fatal("the duration check did not reject a 45-minute appointment")
		}
	})

	t.Run("sub-minute precision is rejected", func(t *testing.T) {
		odd := slotAt(t, 15, 0).Add(30 * time.Second)
		if err := insert(odd, odd.Add(30*time.Minute)); err == nil {
			t.Fatal("the whole-minute check did not reject a start with seconds")
		}
	})

	t.Run("a cancelled appointment must carry a reason and a timestamp", func(t *testing.T) {
		_, err := testPool.Exec(ctx, `
			UPDATE appointments SET status = 'cancelled', cancelled_at = now()
			WHERE doctor_id = $1 AND starts_at = $2
		`, f.doctorID, start)
		if err == nil {
			t.Fatal("the consistency check allowed a cancellation with no reason")
		}
	})

	t.Run("cancelling frees the slot for a new active appointment", func(t *testing.T) {
		if _, err := testPool.Exec(ctx, `
			UPDATE appointments SET status = 'cancelled', cancelled_at = now(),
			       cancellation_reason = 'freeing the slot'
			WHERE doctor_id = $1 AND starts_at = $2
		`, f.doctorID, start); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if err := insert(start, start.Add(30*time.Minute)); err != nil {
			t.Fatalf("the freed slot should be bookable again: %v", err)
		}
	})

	t.Run("overlapping working hours are rejected", func(t *testing.T) {
		// The doctor already works 09:00-17:00 on Monday.
		_, err := testPool.Exec(ctx, `
			INSERT INTO doctor_working_hours (doctor_id, weekday, starts_at_local, ends_at_local)
			VALUES ($1, 1, '16:00', '18:00')
		`, f.doctorID)
		if err == nil {
			t.Fatal("the exclusion constraint did not reject overlapping working hours")
		}
	})

	t.Run("adjacent working hours are allowed", func(t *testing.T) {
		// Half-open intervals: 17:00-19:00 touches but does not overlap.
		if _, err := testPool.Exec(ctx, `
			INSERT INTO doctor_working_hours (doctor_id, weekday, starts_at_local, ends_at_local)
			VALUES ($1, 1, '17:00', '19:00')
		`, f.doctorID); err != nil {
			t.Fatalf("adjacent intervals should be allowed: %v", err)
		}
	})

	t.Run("misaligned working hours are rejected", func(t *testing.T) {
		if _, err := testPool.Exec(ctx, `
			INSERT INTO doctor_working_hours (doctor_id, weekday, starts_at_local, ends_at_local)
			VALUES ($1, 6, '09:10', '17:00')
		`, f.doctorID); err == nil {
			t.Fatal("the alignment check did not reject 09:10")
		}
	})
}

// TestConstraintTranslation pins the mapping from PostgreSQL constraint names
// to domain errors.
//
// If a migration renames a constraint, this test fails loudly — which is far
// better than the conflict silently degrading into a 500 in production.
func TestConstraintTranslation(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()
	slot := appointment.DefaultPolicy().SlotAt(slotAt(t, 10, 30))

	repo := f.store.Appointments()

	// Take the slot.
	if err := repo.WithinTx(ctx, func(ctx context.Context, tx appointment.Tx) error {
		_, err := tx.Create(ctx, f.doctorID, f.patients[0], slot)
		return err
	}); err != nil {
		t.Fatalf("first booking failed: %v", err)
	}

	// A second attempt must surface as the domain sentinel, not a raw pgx error.
	err := repo.WithinTx(ctx, func(ctx context.Context, tx appointment.Tx) error {
		_, err := tx.Create(ctx, f.doctorID, f.patients[1], slot)
		return err
	})
	if !errors.Is(err, appointment.ErrSlotTaken) {
		t.Fatalf("expected ErrSlotTaken, got %v", err)
	}
}

// TestTransactionRollback proves that an error anywhere in the callback undoes
// everything, including the audit event.
func TestTransactionRollback(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()
	slot := appointment.DefaultPolicy().SlotAt(slotAt(t, 11, 30))

	sentinel := errors.New("deliberate failure after the writes")

	err := f.store.Appointments().WithinTx(ctx, func(ctx context.Context, tx appointment.Tx) error {
		created, err := tx.Create(ctx, f.doctorID, f.patients[0], slot)
		if err != nil {
			return err
		}
		if err := tx.AppendEvent(ctx, appointment.Event{
			AppointmentID: created.ID, Type: appointment.EventBooked,
			ToStartsAt: &created.StartsAt, Source: "test",
		}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the sentinel error, got %v", err)
	}

	if count := countActive(t, f.doctorID, slot.Start); count != 0 {
		t.Errorf("the appointment survived a rolled-back transaction (%d rows)", count)
	}

	var events int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM appointment_events e
		JOIN appointments a ON a.id = e.appointment_id
		WHERE a.doctor_id = $1
	`, f.doctorID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 0 {
		t.Errorf("audit events survived a rolled-back transaction (%d rows)", events)
	}
}

// TestRescheduleAtomicity is the end-to-end version of the rollback guarantee:
// a reschedule that fails must leave the appointment exactly where it was.
func TestRescheduleAtomicity(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()

	mine, err := f.service.Book(ctx, appointment.BookCommand{
		DoctorID: f.doctorID, PatientID: f.patients[0], StartsAt: slotAt(t, 9, 0),
	})
	if err != nil {
		t.Fatalf("first booking failed: %v", err)
	}
	theirs, err := f.service.Book(ctx, appointment.BookCommand{
		DoctorID: f.doctorID, PatientID: f.patients[1], StartsAt: slotAt(t, 10, 0),
	})
	if err != nil {
		t.Fatalf("second booking failed: %v", err)
	}

	if _, err := f.service.Reschedule(ctx, appointment.RescheduleCommand{
		ID: mine.Appointment.ID, StartsAt: theirs.Appointment.StartsAt,
	}); !isCode(err, apperror.CodeSlotUnavailable) {
		t.Fatalf("expected a slot conflict, got %v", err)
	}

	current, err := f.service.Get(ctx, mine.Appointment.ID)
	if err != nil {
		t.Fatalf("re-read appointment: %v", err)
	}
	if !current.StartsAt.Equal(mine.Appointment.StartsAt) {
		t.Errorf("the appointment moved despite the failure: %s", current.StartsAt)
	}
	if current.Status != appointment.StatusBooked {
		t.Errorf("expected the appointment to remain booked, got %s", current.Status)
	}
	if count := countActive(t, f.doctorID, mine.Appointment.StartsAt); count != 1 {
		t.Errorf("the original slot should still be held, found %d rows", count)
	}

	// A successful move, by contrast, frees the source and claims the target.
	destination := slotAt(t, 14, 0)
	if _, err := f.service.Reschedule(ctx, appointment.RescheduleCommand{
		ID: mine.Appointment.ID, StartsAt: destination,
	}); err != nil {
		t.Fatalf("reschedule to a free slot failed: %v", err)
	}
	if count := countActive(t, f.doctorID, mine.Appointment.StartsAt); count != 0 {
		t.Errorf("the source slot was not released, found %d rows", count)
	}
	if count := countActive(t, f.doctorID, destination); count != 1 {
		t.Errorf("the destination slot was not claimed, found %d rows", count)
	}
	if events := countEvents(t, mine.Appointment.ID); events != 2 {
		t.Errorf("expected a booked and a rescheduled event, found %d", events)
	}
}

// TestAvailabilityAgainstRealData checks the read path end to end, including
// the SQL range query.
func TestAvailabilityAgainstRealData(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()
	date := calendar.Date{Year: 2026, Month: time.September, Day: 7}

	result, err := f.service.Availability(ctx, appointment.AvailabilityQuery{
		DoctorID: f.doctorID, Date: date,
	})
	if err != nil {
		t.Fatalf("availability failed: %v", err)
	}
	// 09:00-17:00 is sixteen 30-minute slots.
	if len(result.Slots) != 16 {
		t.Fatalf("expected 16 slots, got %d", len(result.Slots))
	}

	if _, err := f.service.Book(ctx, appointment.BookCommand{
		DoctorID: f.doctorID, PatientID: f.patients[0], StartsAt: result.Slots[5].Start,
	}); err != nil {
		t.Fatalf("booking failed: %v", err)
	}

	after, err := f.service.Availability(ctx, appointment.AvailabilityQuery{
		DoctorID: f.doctorID, Date: date,
	})
	if err != nil {
		t.Fatalf("availability failed: %v", err)
	}
	if len(after.Slots) != 15 {
		t.Fatalf("expected 15 slots after one booking, got %d", len(after.Slots))
	}

	// The schedule loaded from the database must produce the same slot grid the
	// policy generates, or availability and booking would disagree.
	schedule := scheduleOf(t, f.store, f.doctorID)
	nairobi, err := time.LoadLocation("Africa/Nairobi")
	if err != nil {
		t.Fatalf("load timezone: %v", err)
	}
	if generated := appointment.DefaultPolicy().SlotsOn(schedule, date, nairobi); len(generated) != 16 {
		t.Fatalf("expected the stored schedule to generate 16 slots, got %d", len(generated))
	}
}

// TestPatientAppointmentsOrderingAndPaging checks the SQL ordering and paging
// rather than the in-memory approximation of it.
func TestPatientAppointmentsOrderingAndPaging(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()

	// Book out of chronological order so a missing ORDER BY would show up.
	for _, hour := range []int{15, 9, 12, 10} {
		if _, err := f.service.Book(ctx, appointment.BookCommand{
			DoctorID: f.doctorID, PatientID: f.patients[0], StartsAt: slotAt(t, hour, 0),
		}); err != nil {
			t.Fatalf("booking at %d:00 failed: %v", hour, err)
		}
	}

	// Cancel one; it must disappear from the upcoming list.
	cancelled, err := f.service.Book(ctx, appointment.BookCommand{
		DoctorID: f.doctorID, PatientID: f.patients[0], StartsAt: slotAt(t, 16, 0),
	})
	if err != nil {
		t.Fatalf("booking failed: %v", err)
	}
	if _, err := f.service.Cancel(ctx, appointment.CancelCommand{
		ID: cancelled.Appointment.ID, Reason: "excluded from the upcoming list",
	}); err != nil {
		t.Fatalf("cancel failed: %v", err)
	}

	all, err := f.service.ListUpcomingForPatient(ctx, appointment.PatientAppointmentsQuery{
		PatientID: f.patients[0],
	})
	if err != nil {
		t.Fatalf("listing failed: %v", err)
	}
	if all.Total != 4 {
		t.Fatalf("expected 4 upcoming appointments, got %d", all.Total)
	}
	for i := 1; i < len(all.Items); i++ {
		if all.Items[i-1].Appointment.StartsAt.After(all.Items[i].Appointment.StartsAt) {
			t.Fatal("results are not in ascending start order")
		}
	}
	if all.Items[0].Doctor.FullName == "" {
		t.Error("the joined doctor detail is missing")
	}

	// Paging must partition the same ordered list with no gaps or repeats.
	seen := map[uuid.UUID]bool{}
	for offset := int32(0); offset < 4; offset += 2 {
		page, err := f.service.ListUpcomingForPatient(ctx, appointment.PatientAppointmentsQuery{
			PatientID: f.patients[0],
			Page:      appointment.Page{Limit: 2, Offset: offset},
		})
		if err != nil {
			t.Fatalf("paged listing failed: %v", err)
		}
		for _, item := range page.Items {
			if seen[item.Appointment.ID] {
				t.Errorf("appointment %s appeared on more than one page", item.Appointment.ID)
			}
			seen[item.Appointment.ID] = true
		}
	}
	if len(seen) != 4 {
		t.Errorf("paging returned %d distinct appointments, expected 4", len(seen))
	}
}

// TestIdempotencyPersistence checks replay across service instances, which is
// what happens when a retry lands on a different replica.
func TestIdempotencyPersistence(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()

	cmd := appointment.BookCommand{
		DoctorID:       f.doctorID,
		PatientID:      f.patients[0],
		StartsAt:       slotAt(t, 13, 0),
		IdempotencyKey: "persisted-key-" + f.doctorID.String()[:8],
	}

	first, err := f.service.Book(ctx, cmd)
	if err != nil {
		t.Fatalf("first booking failed: %v", err)
	}

	// A second service over the same pool stands in for a second replica.
	other := newFixture(t)
	replayService, err := appointment.NewService(
		f.store.Appointments(), f.store.Doctors(), f.store.Patients(),
		other.clock, logging.Discard(), appointment.NopMetrics{},
		appointment.ServiceConfig{
			Policy: appointment.DefaultPolicy(), IdempotencyTTL: 24 * time.Hour,
			DefaultPageSize: 20, MaxPageSize: 100,
		},
	)
	if err != nil {
		t.Fatalf("build second service: %v", err)
	}

	replayed, err := replayService.Book(ctx, cmd)
	if err != nil {
		t.Fatalf("replay on a second instance failed: %v", err)
	}
	if !replayed.Replayed {
		t.Error("expected the response to be flagged as replayed")
	}
	if replayed.Appointment.ID != first.Appointment.ID {
		t.Errorf("the replay returned a different appointment: %s vs %s",
			replayed.Appointment.ID, first.Appointment.ID)
	}
	if count := countActive(t, f.doctorID, cmd.StartsAt); count != 1 {
		t.Fatalf("expected exactly 1 appointment, found %d", count)
	}
}

// TestPingReportsDatabaseHealth covers what /readyz depends on.
func TestPingReportsDatabaseHealth(t *testing.T) {
	f := newFixture(t)

	if err := f.store.Ping(t.Context()); err != nil {
		t.Fatalf("expected a healthy ping, got %v", err)
	}

	t.Run("a cancelled context fails fast", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := f.store.Ping(ctx); err == nil {
			t.Fatal("expected the ping to fail on a cancelled context")
		}
	})

	t.Run("an unreachable database is reported, not hidden", func(t *testing.T) {
		// Port 1 has nothing listening on it.
		pool, err := pgxpool.New(t.Context(), "postgres://nobody@127.0.0.1:1/nothing?connect_timeout=1")
		if err != nil {
			t.Fatalf("build pool: %v", err)
		}
		defer pool.Close()

		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()

		if err := postgres.NewStore(pool).Ping(ctx); err == nil {
			t.Fatal("expected the ping to fail against an unreachable database")
		}
	})
}

// TestMigrationsRollBackAndReapply proves every migration has a working Down.
//
// It runs against its own throwaway database so it cannot disturb the schema
// the parallel tests are using.
func TestMigrationsRollBackAndReapply(t *testing.T) {
	ctx := t.Context()

	adminURL := mustEnv(t, "TEST_DATABASE_URL")
	dbName := "ratiba_migration_" + uuid.New().String()[:8]

	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer func() { _ = admin.Close() }()

	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(cleanupCtx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)"); err != nil {
			t.Logf("could not drop scratch database %s: %v", dbName, err)
		}
	})

	scratchURL := replaceDatabaseName(t, adminURL, dbName)
	scratch, err := sql.Open("pgx", scratchURL)
	if err != nil {
		t.Fatalf("open scratch connection: %v", err)
	}
	defer func() { _ = scratch.Close() }()

	goose.SetBaseFS(ratibadb.Migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}

	if err := goose.UpContext(ctx, scratch, "migrations"); err != nil {
		t.Fatalf("initial migration up: %v", err)
	}
	version, err := goose.GetDBVersionContext(ctx, scratch)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version == 0 {
		t.Fatal("expected a non-zero schema version after migrating up")
	}

	// Roll every migration back, then reapply. A missing or broken Down section
	// fails here rather than during an incident.
	if err := goose.DownToContext(ctx, scratch, "migrations", 0); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	if err := goose.UpContext(ctx, scratch, "migrations"); err != nil {
		t.Fatalf("reapply migrations: %v", err)
	}

	reapplied, err := goose.GetDBVersionContext(ctx, scratch)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if reapplied != version {
		t.Errorf("expected to return to version %d, got %d", version, reapplied)
	}

	// The schema must actually work after the round trip.
	var count int
	if err := scratch.QueryRowContext(ctx, "SELECT count(*) FROM appointments").Scan(&count); err != nil {
		t.Fatalf("query the reapplied schema: %v", err)
	}
}
