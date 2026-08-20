// Package database owns the PostgreSQL connection pool's lifecycle and
// configuration.
//
// It exists so that pool sizing, timeouts and tracing are decided in exactly
// one place. Everything else in the application receives a ready pool and never
// reasons about connection strings.
package database

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JerryLegend254/ratiba/internal/platform/config"
)

// Options tunes pool construction beyond what DatabaseConfig carries.
type Options struct {
	// ApplicationName appears in pg_stat_activity, which is how an operator
	// tells Ratiba's connections apart from a migration job or a psql session.
	ApplicationName string
}

// NewPool builds and verifies a connection pool.
//
// The pool is proven usable before it is returned: a pool that connects lazily
// would let the process report itself healthy while the database is refusing
// connections, which is precisely the failure an operator needs to see at
// startup.
func NewPool(ctx context.Context, cfg config.DatabaseConfig, opts Options, logger *slog.Logger) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		// The error from ParseConfig can echo the connection string, so it is
		// deliberately not wrapped into the returned message.
		return nil, fmt.Errorf("parse database configuration for %s: invalid connection string", cfg.RedactedURL())
	}

	poolConfig.MaxConns = cfg.MaxConns
	poolConfig.MinConns = cfg.MinConns
	poolConfig.MaxConnLifetime = cfg.MaxConnLifetime
	poolConfig.MaxConnIdleTime = cfg.MaxConnIdleTime
	// Jitter stops a fleet of replicas from recycling every connection at the
	// same instant after a rolling restart.
	poolConfig.MaxConnLifetimeJitter = cfg.MaxConnLifetime / 10
	poolConfig.HealthCheckPeriod = 30 * time.Second
	poolConfig.ConnConfig.ConnectTimeout = cfg.ConnectTimeout

	if poolConfig.ConnConfig.RuntimeParams == nil {
		poolConfig.ConnConfig.RuntimeParams = map[string]string{}
	}
	poolConfig.ConnConfig.RuntimeParams["application_name"] = opts.ApplicationName
	// A server-side statement timeout is the backstop for a leaked or
	// over-generous context: PostgreSQL will cancel the query itself rather
	// than let a connection be held indefinitely.
	poolConfig.ConnConfig.RuntimeParams["statement_timeout"] =
		strconv.FormatInt(cfg.StatementTimeout.Milliseconds(), 10)

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to %s: %w", cfg.RedactedURL(), err)
	}

	logger.Info("database pool ready",
		slog.String("database", cfg.RedactedURL()),
		slog.Int("max_conns", int(cfg.MaxConns)),
		slog.Int("min_conns", int(cfg.MinConns)),
		slog.Duration("statement_timeout", cfg.StatementTimeout),
	)
	return pool, nil
}
