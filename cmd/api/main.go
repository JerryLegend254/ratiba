// Command api runs the Ratiba clinic appointment HTTP API.
//
// This file is the composition root: it is the only place where concrete
// implementations are chosen and wired together. Everything below it receives
// its collaborators through constructors, which is what makes the rest of the
// codebase testable without a database or an HTTP listener.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// The zone database is compiled into the binary rather than read from the
	// host, so a doctor's IANA timezone resolves identically everywhere the
	// binary runs — including a minimal container with no /usr/share/zoneinfo.
	_ "time/tzdata"

	"github.com/JerryLegend254/ratiba/internal/appointment"
	"github.com/JerryLegend254/ratiba/internal/platform/clock"
	"github.com/JerryLegend254/ratiba/internal/platform/config"
	"github.com/JerryLegend254/ratiba/internal/platform/database"
	"github.com/JerryLegend254/ratiba/internal/platform/httpserver"
	"github.com/JerryLegend254/ratiba/internal/platform/logging"
	"github.com/JerryLegend254/ratiba/internal/platform/observability"
	"github.com/JerryLegend254/ratiba/internal/postgres"
	transporthttp "github.com/JerryLegend254/ratiba/internal/transport/http"
)

// Build metadata, injected at link time with -ldflags. See the Makefile and
// Dockerfile. The defaults are what a plain `go run` reports.
var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	if err := run(); err != nil {
		// Startup failures happen before the logger exists, or after it has
		// been shut down. stderr is the only thing guaranteed to work.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(config.BuildInfo{
		Version:   version,
		Commit:    commit,
		BuildTime: buildTime,
	})
	if err != nil {
		return err
	}

	logger := logging.New(os.Stdout, cfg)
	logger.Info("starting ratiba", slog.Any("config", cfg))

	// SIGTERM is what Railway and every container platform send first. SIGINT
	// is Ctrl-C locally. Both begin the same graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := observability.SetupTracing(ctx, cfg, logger)
	if err != nil {
		return fmt.Errorf("set up tracing: %w", err)
	}

	pool, err := database.NewPool(ctx, cfg.Database, database.Options{
		ApplicationName: cfg.ServiceName,
		EnableTracing:   cfg.Telemetry.TracingEnabled,
	}, logger)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	metrics := observability.NewMetrics(cfg)
	if err := metrics.RegisterPoolCollector(pool); err != nil {
		return fmt.Errorf("register pool metrics: %w", err)
	}

	store := postgres.NewStore(pool)

	policy, err := appointment.NewPolicy(appointment.DefaultSlotDuration, cfg.Booking.MinLeadTime)
	if err != nil {
		return fmt.Errorf("build booking policy: %w", err)
	}

	appointments, err := appointment.NewService(
		store.Appointments(),
		store.Doctors(),
		store.Patients(),
		clock.System{},
		logger,
		metrics,
		appointment.ServiceConfig{
			Policy:          policy,
			DefaultPageSize: cfg.Booking.DefaultPageSize,
			MaxPageSize:     cfg.Booking.MaxPageSize,
		},
	)
	if err != nil {
		return fmt.Errorf("build appointment service: %w", err)
	}

	readiness := httpserver.NewReadinessGate()

	apiServer := transporthttp.NewServer(cfg, transporthttp.Dependencies{
		Appointments: appointments,
		Doctors:      store.Doctors(),
		Patients:     store.Patients(),
		Health:       store,
		Readiness:    readiness,
		Metrics:      metrics,
		Logger:       logger,
	})

	server := httpserver.New(
		apiServer.Handler(cfg.Telemetry.MetricsEnabled),
		cfg.HTTP,
		readiness,
		logger,
	)

	stopPprof := startPprof(cfg, logger)

	// Dependencies are verified; start accepting traffic.
	readiness.Open()

	runErr := server.Run(ctx)

	// Shut telemetry and profiling down after the server has drained, so a
	// request finishing during shutdown can still record its span.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	stopPprof(shutdownCtx)
	if err := shutdownTracing(shutdownCtx); err != nil {
		logger.Warn("failed to flush traces during shutdown", slog.String("error", err.Error()))
	}

	if runErr != nil {
		return runErr
	}
	logger.Info("ratiba stopped")
	return nil
}

// startPprof optionally exposes Go runtime profiles on a separate listener.
//
// It binds a dedicated address (127.0.0.1 by default) rather than joining the
// API router, so profiles can never be reached through the service's public
// hostname. config.Load refuses to start production with it enabled at all.
func startPprof(cfg config.Config, logger *slog.Logger) func(context.Context) {
	if !cfg.Telemetry.PprofEnabled {
		return func(context.Context) {}
	}

	// The import is function-local so net/http/pprof's package init, which
	// registers handlers on http.DefaultServeMux, cannot leak those handlers
	// into the application's router.
	mux := http.NewServeMux()
	registerPprof(mux)

	server := &http.Server{
		Addr:              cfg.Telemetry.PprofAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Warn("pprof listener enabled",
			slog.String("addr", cfg.Telemetry.PprofAddr),
			slog.String("note", "debug endpoints are exposed on this address"),
		)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("pprof listener failed", slog.String("error", err.Error()))
		}
	}()

	return func(ctx context.Context) {
		if err := server.Shutdown(ctx); err != nil {
			logger.Warn("pprof listener shutdown failed", slog.String("error", err.Error()))
		}
	}
}
