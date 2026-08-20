//go:build integration

// Package postgres_test contains Ratiba's integration suite.
//
// These tests run against a real PostgreSQL server because they assert things
// an in-memory double cannot honestly assert: that a partial unique index stops
// two concurrent transactions from booking the same slot, that a failed
// reschedule rolls back, and that the adapter translates real SQLSTATE codes
// into the right domain errors.
//
// They are behind the `integration` build tag so `go test ./...` stays fast and
// dependency-free. Run them with `make integration-test`.
package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	ratibadb "github.com/JerryLegend254/ratiba/db"
	"github.com/JerryLegend254/ratiba/internal/appointment"
	"github.com/JerryLegend254/ratiba/internal/doctor"
	"github.com/JerryLegend254/ratiba/internal/platform/clock"
	"github.com/JerryLegend254/ratiba/internal/platform/logging"
	"github.com/JerryLegend254/ratiba/internal/postgres"
)

// testPool is shared by every test in this package. Tests isolate themselves by
// creating their own doctors and patients with fresh UUIDs rather than by
// truncating tables, which keeps them safe to run in parallel.
var testPool *pgxpool.Pool

// TestMain connects to the test database and brings the schema up to date.
func TestMain(m *testing.M) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		fmt.Fprintln(os.Stderr,
			"TEST_DATABASE_URL is not set.\n"+
				"Start a database and export it, for example:\n"+
				"  make integration-test\n"+
				"or:\n"+
				"  docker compose up -d postgres\n"+
				"  export TEST_DATABASE_URL='postgres://ratiba:ratiba@localhost:5432/ratiba_test?sslmode=disable'")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := migrate(databaseURL); err != nil {
		fmt.Fprintf(os.Stderr, "migrate test database: %v\n", err)
		os.Exit(1)
	}

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect to test database: %v\n", err)
		os.Exit(1)
	}
	if err := pool.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ping test database: %v\n", err)
		os.Exit(1)
	}
	testPool = pool

	code := m.Run()
	pool.Close()
	os.Exit(code)
}

func migrate(databaseURL string) error {
	goose.SetBaseFS(ratibadb.Migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return err
	}
	defer func() { _ = sqlDB.Close() }()
	return goose.Up(sqlDB, "migrations")
}

// fixture is one test's isolated slice of the database.
type fixture struct {
	store    *postgres.Store
	service  *appointment.Service
	clock    *clock.Fixed
	doctorID uuid.UUID
	patients []uuid.UUID
}

// newFixture creates a doctor working 09:00-17:00 Monday to Friday in
// Africa/Nairobi plus two patients, all with fresh identifiers, and returns a
// service wired to the real store.
//
// The clock is pinned to Monday 2026-09-07 05:00 UTC (08:00 Nairobi), an hour
// before the clinic opens, so every slot that day satisfies the lead-time rule.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := t.Context()

	doctorID := uuid.New()
	slug := "test-" + doctorID.String()[:18]

	if _, err := testPool.Exec(ctx, `
		INSERT INTO doctors (id, slug, full_name, specialty, timezone, is_active)
		VALUES ($1, $2, 'Dr. Integration Test', 'General Practice', 'Africa/Nairobi', true)
	`, doctorID, slug); err != nil {
		t.Fatalf("insert doctor: %v", err)
	}

	for weekday := 1; weekday <= 5; weekday++ {
		if _, err := testPool.Exec(ctx, `
			INSERT INTO doctor_working_hours (doctor_id, weekday, starts_at_local, ends_at_local)
			VALUES ($1, $2, '09:00', '17:00')
		`, doctorID, weekday); err != nil {
			t.Fatalf("insert working hours: %v", err)
		}
	}

	patients := make([]uuid.UUID, 0, 2)
	for i := range 2 {
		patientID := uuid.New()
		if _, err := testPool.Exec(ctx, `
			INSERT INTO patients (id, full_name, email, is_active)
			VALUES ($1, $2, $3, true)
		`, patientID, fmt.Sprintf("Test Patient %d", i), patientID.String()+"@example.com"); err != nil {
			t.Fatalf("insert patient: %v", err)
		}
		patients = append(patients, patientID)
	}

	t.Cleanup(func() {
		// Best effort: appointments cascade from the doctor, so removing the
		// doctor and patients removes everything this test created.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = testPool.Exec(cleanupCtx, `DELETE FROM appointments WHERE doctor_id = $1`, doctorID)
		_, _ = testPool.Exec(cleanupCtx, `DELETE FROM doctors WHERE id = $1`, doctorID)
		for _, patientID := range patients {
			_, _ = testPool.Exec(cleanupCtx, `DELETE FROM patients WHERE id = $1`, patientID)
		}
	})

	store := postgres.NewStore(testPool)
	clk := clock.NewFixed(time.Date(2026, 9, 7, 5, 0, 0, 0, time.UTC))

	service, err := appointment.NewService(
		store.Appointments(), store.Doctors(), store.Patients(),
		clk, logging.Discard(), appointment.NopMetrics{},
		appointment.ServiceConfig{
			Policy:          appointment.DefaultPolicy(),
			IdempotencyTTL:  24 * time.Hour,
			DefaultPageSize: 20,
			MaxPageSize:     100,
		},
	)
	if err != nil {
		t.Fatalf("build service: %v", err)
	}

	return &fixture{
		store: store, service: service, clock: clk,
		doctorID: doctorID, patients: patients,
	}
}

// slotAt builds an instant from a Nairobi wall-clock time on Monday 2026-09-07.
func slotAt(t *testing.T, hour, minute int) time.Time {
	t.Helper()
	nairobi, err := time.LoadLocation("Africa/Nairobi")
	if err != nil {
		t.Fatalf("load timezone: %v", err)
	}
	return time.Date(2026, 9, 7, hour, minute, 0, 0, nairobi)
}

// countActive reports how many active appointments hold a doctor and start.
// This is the ground truth every concurrency test checks.
func countActive(t *testing.T, doctorID uuid.UUID, start time.Time) int {
	t.Helper()

	var count int
	err := testPool.QueryRow(t.Context(), `
		SELECT count(*) FROM appointments
		WHERE doctor_id = $1 AND starts_at = $2 AND status = 'booked'
	`, doctorID, start).Scan(&count)
	if err != nil {
		t.Fatalf("count active appointments: %v", err)
	}
	return count
}

// countEvents reports how many audit events an appointment has.
func countEvents(t *testing.T, appointmentID uuid.UUID) int {
	t.Helper()

	var count int
	err := testPool.QueryRow(t.Context(),
		`SELECT count(*) FROM appointment_events WHERE appointment_id = $1`, appointmentID).Scan(&count)
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	return count
}

// scheduleOf is a convenience for policy-level assertions against real data.
func scheduleOf(t *testing.T, store *postgres.Store, doctorID uuid.UUID) doctor.Schedule {
	t.Helper()

	schedule, err := store.Doctors().ScheduleFor(t.Context(), doctorID)
	if err != nil {
		t.Fatalf("load schedule: %v", err)
	}
	return schedule
}

// mustEnv reads a required environment variable.
func mustEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s must be set", name)
	}
	return value
}

// replaceDatabaseName rewrites the database path of a connection URL, so a test
// can spin up a throwaway database on the same server.
func replaceDatabaseName(t *testing.T, rawURL, name string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse database URL: %v", err)
	}
	parsed.Path = "/" + name
	return parsed.String()
}
