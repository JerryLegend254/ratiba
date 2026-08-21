// Package httpserver runs the API listener and owns its shutdown sequence.
//
// The interesting part is Run's ordering. On SIGTERM a naive server closes the
// listener immediately, which drops requests already in flight and, worse,
// requests the load balancer sent microseconds before it noticed. Run instead
// fails readiness first, gives the platform a moment to stop routing new
// traffic, and only then drains.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/JerryLegend254/ratiba/internal/platform/config"
)

// ReadinessGate is the flag /readyz consults before reporting the service
// ready. It is flipped to false at the very start of shutdown.
type ReadinessGate struct {
	ready atomic.Bool
}

// NewReadinessGate returns a gate that starts closed. The application opens it
// once its dependencies are verified.
func NewReadinessGate() *ReadinessGate { return &ReadinessGate{} }

// Open marks the service ready to receive traffic.
func (g *ReadinessGate) Open() { g.ready.Store(true) }

// Close marks the service as no longer accepting new traffic.
func (g *ReadinessGate) Close() { g.ready.Store(false) }

// IsOpen reports the current state.
func (g *ReadinessGate) IsOpen() bool { return g.ready.Load() }

// Server wraps net/http with Ratiba's timeouts and shutdown behaviour.
type Server struct {
	httpServer      *http.Server
	logger          *slog.Logger
	readiness       *ReadinessGate
	shutdownTimeout time.Duration
	// drainDelay is the pause between failing readiness and closing the
	// listener, giving the platform's health checker time to notice.
	drainDelay time.Duration
}

// New builds a Server.
//
// Every timeout is set explicitly. Go's defaults are all "no limit", which
// means a single slow or malicious client can hold a connection, and therefore
// a goroutine and a file descriptor, forever.
func New(handler http.Handler, cfg config.HTTPConfig, readiness *ReadinessGate, logger *slog.Logger) *Server {
	return &Server{
		httpServer: &http.Server{
			Addr:              net.JoinHostPort("", fmt.Sprint(cfg.Port)),
			Handler:           handler,
			ReadHeaderTimeout: cfg.ReadHeaderTimeout,
			ReadTimeout:       cfg.ReadTimeout,
			WriteTimeout:      cfg.WriteTimeout,
			IdleTimeout:       cfg.IdleTimeout,
			ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		},
		logger:          logger,
		readiness:       readiness,
		shutdownTimeout: cfg.ShutdownTimeout,
		drainDelay:      drainDelayFor(cfg.ShutdownTimeout),
	}
}

// drainDelay is capped so shutdown never spends most of its budget waiting
// before it starts draining.
func drainDelayFor(shutdownTimeout time.Duration) time.Duration {
	const preferred = 2 * time.Second
	if quarter := shutdownTimeout / 4; quarter < preferred {
		return quarter
	}
	return preferred
}

// Run serves until ctx is cancelled, then shuts down gracefully.
//
// It returns nil on a clean shutdown, and an error if the listener failed or
// draining exceeded the shutdown timeout.
func (s *Server) Run(ctx context.Context) error {
	// ListenConfig rather than net.Listen, so a context cancelled during
	// startup aborts the bind instead of leaving a socket open.
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", s.httpServer.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.httpServer.Addr, err)
	}

	serveErr := make(chan error, 1)
	go func() {
		s.logger.Info("http server listening", slog.String("addr", listener.Addr().String()))
		if err := s.httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	s.logger.Info("shutdown signal received, draining",
		slog.Duration("drain_delay", s.drainDelay),
		slog.Duration("shutdown_timeout", s.shutdownTimeout),
	)

	// Step 1: fail readiness. The platform's health check sees this and stops
	// routing new requests here.
	s.readiness.Close()

	// Step 2: keep serving for a moment. Requests already dispatched to this
	// instance still get answered, and the health checker gets a chance to
	// observe the failed probe before the listener disappears.
	select {
	case <-time.After(s.drainDelay):
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
	}

	// Step 3: stop accepting, then wait for in-flight requests to finish.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownTimeout)
	defer cancel()

	if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
		// Requests still running at this point are forcibly cut. Report it
		// rather than exiting 0 and hiding the truncated work.
		return fmt.Errorf("graceful shutdown exceeded %s: %w", s.shutdownTimeout, err)
	}

	s.logger.Info("http server stopped cleanly")
	return nil
}
