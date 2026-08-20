// Command migrate applies Ratiba's database schema and seed data.
//
// It is a separate binary from the API on purpose. Schema changes are run once
// per deployment, as a pre-deploy step, not by every API replica at startup —
// concurrent migrations from N replicas are a classic way to corrupt a schema
// or deadlock a rollout. See docs/deployment.md.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	// The seed command validates each doctor's IANA timezone, which needs the
	// embedded zone database in a distroless image.
	_ "time/tzdata"

	"github.com/jackc/pgx/v5/pgxpool"
	// Registers the "pgx" database/sql driver that goose opens below.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/JerryLegend254/ratiba/db"
	"github.com/JerryLegend254/ratiba/internal/platform/config"
)

const migrationsDir = "migrations"

// advisoryLockID guards schema changes.
//
// Even though migrations are meant to run once per deployment, a lock costs
// nothing and makes a mistake — two pre-deploy jobs, or somebody running
// `make migrate-up` during a deploy — safe rather than catastrophic. The
// constant is arbitrary but must never change, or the lock stops working.
const advisoryLockID int64 = 7264178295551002

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func usage() string {
	return strings.TrimSpace(`
ratiba-migrate — database schema and seed management

Usage:
  ratiba-migrate <command> [flags]

Commands:
  up                 Apply all pending migrations.
  up-by-one          Apply the next pending migration only.
  down               Roll back the most recent migration.
  status             Show which migrations have been applied.
  version            Print the current schema version.
  seed               Insert or refresh the deterministic demo dataset.
  purge-idempotency  Delete expired idempotency records.

Environment:
  DATABASE_URL       PostgreSQL connection string (required).

Flags:
  -timeout duration  Overall timeout for the command (default 5m).
`)
}

func run() error {
	timeout := flag.Duration("timeout", 5*time.Minute, "overall timeout for the command")
	flag.Usage = func() { fmt.Fprintln(os.Stderr, usage()) }
	flag.Parse()

	command := flag.Arg(0)
	if command == "" {
		fmt.Fprintln(os.Stderr, usage())
		return errors.New("a command is required")
	}

	databaseURL, err := config.DatabaseURLFromEnv()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	switch command {
	case "seed":
		return seed(ctx, pool, logger)
	case "purge-idempotency":
		return purgeIdempotency(ctx, pool, logger)
	}

	return withAdvisoryLock(ctx, pool, logger, func(ctx context.Context) error {
		return runGoose(ctx, databaseURL, command, logger)
	})
}

// withAdvisoryLock serialises schema changes across processes.
//
// The lock is session-scoped and taken on a dedicated connection, so it is
// released when that connection is returned even if the process is killed
// mid-migration.
func withAdvisoryLock(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, fn func(context.Context) error) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for advisory lock: %w", err)
	}
	defer conn.Release()

	logger.Info("acquiring migration advisory lock", slog.Int64("lock_id", advisoryLockID))
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryLockID); err != nil {
		return fmt.Errorf("acquire advisory lock: %w", err)
	}
	defer func() {
		// A best-effort unlock on a fresh context: the caller's context may
		// already be cancelled by the time this runs.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", advisoryLockID); err != nil {
			logger.Warn("failed to release advisory lock", slog.String("error", err.Error()))
		}
	}()

	return fn(ctx)
}

// runGoose executes a migration command against the embedded migration set.
func runGoose(ctx context.Context, databaseURL, command string, logger *slog.Logger) error {
	goose.SetBaseFS(db.Migrations)
	goose.SetLogger(gooseLogger{logger: logger})
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}

	// goose speaks database/sql, so pgx is used through its stdlib adapter here
	// rather than through the pool the API uses. The driver is registered by
	// the blank import of pgx/v5/stdlib at the top of this file.
	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open database for migrations: %w", err)
	}
	defer func() {
		if err := sqlDB.Close(); err != nil {
			logger.Warn("failed to close migration connection", slog.String("error", err.Error()))
		}
	}()

	start := time.Now()
	switch command {
	case "up":
		err = goose.UpContext(ctx, sqlDB, migrationsDir)
	case "up-by-one":
		err = goose.UpByOneContext(ctx, sqlDB, migrationsDir)
	case "down":
		// Rolling back in production destroys data by design. The runbook says
		// to forward-fix instead; this exists for local development and CI.
		err = goose.DownContext(ctx, sqlDB, migrationsDir)
	case "status":
		err = goose.StatusContext(ctx, sqlDB, migrationsDir)
	case "version":
		err = goose.VersionContext(ctx, sqlDB, migrationsDir)
	default:
		fmt.Fprintln(os.Stderr, usage())
		return fmt.Errorf("unknown command %q", command)
	}
	if err != nil {
		return fmt.Errorf("goose %s: %w", command, err)
	}

	logger.Info("migration command complete",
		slog.String("command", command),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
	)
	return nil
}

// gooseLogger adapts goose's logging interface onto slog so migration output
// is structured like everything else.
type gooseLogger struct {
	logger *slog.Logger
}

func (g gooseLogger) Fatalf(format string, v ...any) {
	g.logger.Error(strings.TrimSpace(fmt.Sprintf(format, v...)))
	os.Exit(1)
}

func (g gooseLogger) Printf(format string, v ...any) {
	message := strings.TrimSpace(fmt.Sprintf(format, v...))
	if message == "" {
		return
	}
	g.logger.Info(message)
}
