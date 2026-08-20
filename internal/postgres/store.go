// Package postgres is the PostgreSQL adapter behind Ratiba's domain ports.
//
// It is the only package that knows about pgx, SQLSTATE codes and constraint
// names. Everything it returns to the domain is either a domain type or one of
// the sentinel errors declared in the domain packages, so a change of database
// would not ripple past this boundary.
//
// All SQL lives in db/queries and is compiled by sqlc into internal/postgres/
// sqlcgen. No query in this package is assembled by string concatenation.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JerryLegend254/ratiba/internal/appointment"
	"github.com/JerryLegend254/ratiba/internal/platform/apperror"
	"github.com/JerryLegend254/ratiba/internal/postgres/sqlcgen"
)

// SQLSTATE codes Ratiba reacts to. Declared here rather than pulled in as a
// dependency, since only a handful matter.
const (
	sqlStateUniqueViolation      = "23505"
	sqlStateExclusionViolation   = "23P01"
	sqlStateForeignKeyViolation  = "23503"
	sqlStateCheckViolation       = "23514"
	sqlStateSerializationFailure = "40001"
	sqlStateDeadlockDetected     = "40P01"
)

// Constraint names the adapter translates into domain errors. If a migration
// ever renames one of these, the corresponding test in the integration suite
// fails loudly rather than the conflict silently becoming a 500.
const constraintActiveSlotUnique = "appointments_active_slot_uniq"

// Store owns the connection pool and hands out the per-aggregate repositories.
type Store struct {
	pool    *pgxpool.Pool
	queries *sqlcgen.Queries
}

// NewStore builds a Store over an existing pool. The pool's lifecycle belongs
// to the caller.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, queries: sqlcgen.New(pool)}
}

// Pool exposes the underlying pool for health checks and pool metrics.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Doctors returns the doctor repository.
func (s *Store) Doctors() *DoctorRepository { return &DoctorRepository{queries: s.queries} }

// Patients returns the patient repository.
func (s *Store) Patients() *PatientRepository { return &PatientRepository{queries: s.queries} }

// Appointments returns the appointment repository.
func (s *Store) Appointments() *AppointmentRepository {
	return &AppointmentRepository{pool: s.pool, queries: s.queries}
}

// Ping verifies the database is reachable and answering queries. Used by
// /readyz, where a connection that exists but cannot serve a trivial query is
// as bad as no connection at all.
func (s *Store) Ping(ctx context.Context) error {
	var result int
	if err := s.pool.QueryRow(ctx, "SELECT 1").Scan(&result); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	if result != 1 {
		return errors.New("ping database: unexpected result")
	}
	return nil
}

// isPgErr reports whether err is a PostgreSQL error with the given SQLSTATE.
func isPgErr(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// constraintName returns the constraint a PostgreSQL error names, if any.
func constraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// IsRetryable reports whether a failed transaction could succeed if replayed.
//
// Ratiba does not currently retry automatically — with row locks held in a
// consistent order these are not expected — but the classification is exposed
// so operators and the runbook can distinguish transient contention from a real
// fault when reading logs.
func IsRetryable(err error) bool {
	return isPgErr(err, sqlStateSerializationFailure) || isPgErr(err, sqlStateDeadlockDetected)
}

// translateWriteError maps a PostgreSQL failure onto a domain error.
//
// Only integrity failures that a client could plausibly have caused are
// translated. Anything unrecognised is returned unchanged so it surfaces as a
// 500 rather than being mislabelled as a client problem — a bug that silently
// turns server faults into 4xx is far worse than a noisy 500.
func translateWriteError(err error) error {
	if err == nil {
		return nil
	}

	switch {
	case isPgErr(err, sqlStateUniqueViolation):
		if constraintName(err) == constraintActiveSlotUnique {
			return appointment.ErrSlotTaken
		}

	case isPgErr(err, sqlStateExclusionViolation):
		// Only doctor_working_hours carries an exclusion constraint, and only
		// seeding writes to it. Reaching this from the API would mean a new
		// write path was added without a matching rule here.
		return apperror.New(apperror.KindConflict, apperror.CodeSlotUnavailable,
			"The requested change conflicts with an existing schedule entry.").WithCause(err)

	case isPgErr(err, sqlStateForeignKeyViolation):
		// The doctor or patient was removed between validation and the insert.
		return apperror.New(apperror.KindUnprocessable, apperror.CodeValidationFailed,
			"A referenced doctor or patient no longer exists.").WithCause(err)

	case isPgErr(err, sqlStateCheckViolation):
		// A CHECK constraint is the last line of defence behind the domain
		// rules. Reaching one means validation and schema disagree, so it is
		// reported as a client-visible rejection and logged with the cause for
		// investigation.
		return apperror.New(apperror.KindUnprocessable, apperror.CodeValidationFailed,
			"The request violates a data integrity rule.").WithCause(err)
	}

	return err
}

// WithinTx runs fn in a single transaction.
//
// The default READ COMMITTED isolation is sufficient here because correctness
// does not rest on the snapshot: it rests on the row locks taken by
// LockAppointment and on the partial unique index, both of which behave
// identically at this level. See docs/adr/0003-concurrency-strategy.md.
func (r *AppointmentRepository) WithinTx(ctx context.Context, fn func(context.Context, appointment.Tx) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	// Rollback after a successful Commit is a no-op, so this is safe on every
	// path and guarantees no connection is leaked if fn panics.
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(ctx, &txRepository{queries: r.queries.WithTx(tx)}); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		// A conflict can surface at COMMIT rather than at the statement, so the
		// same translation has to run here too.
		return translateWriteError(fmt.Errorf("commit transaction: %w", err))
	}
	return nil
}

// noRows reports whether err is pgx's "query returned no rows".
func noRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
