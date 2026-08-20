package http

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/JerryLegend254/ratiba/internal/appointment"
	"github.com/JerryLegend254/ratiba/internal/doctor"
	"github.com/JerryLegend254/ratiba/internal/patient"
	"github.com/JerryLegend254/ratiba/internal/platform/apperror"
	"github.com/JerryLegend254/ratiba/internal/platform/config"
	"github.com/JerryLegend254/ratiba/internal/platform/observability"
)

// HealthChecker is the dependency /readyz probes.
type HealthChecker interface {
	// Ping must return quickly, or respect the context deadline.
	Ping(ctx context.Context) error
}

// Server holds everything the HTTP handlers need.
//
// Dependencies are injected explicitly through NewServer. There is no service
// locator and no package-level state, so a test can construct a server with
// fakes and no database in a few lines.
type Server struct {
	appointments *appointment.Service
	doctors      doctor.Repository
	patients     patient.Repository
	health       HealthChecker
	metrics      *observability.Metrics
	logger       *slog.Logger

	build       config.BuildInfo
	serviceName string
	environment string

	maxBodyBytes     int64
	defaultPageSize  int32
	maxPageSize      int32
	handlerTimeout   time.Duration
	readinessTimeout time.Duration

	metricsToken string
}

// Dependencies are the collaborators NewServer requires.
type Dependencies struct {
	Appointments *appointment.Service
	Doctors      doctor.Repository
	Patients     patient.Repository
	Health       HealthChecker
	Metrics      *observability.Metrics
	Logger       *slog.Logger
}

// NewServer builds the API server from configuration and dependencies.
func NewServer(cfg config.Config, deps Dependencies) *Server {
	return &Server{
		appointments:     deps.Appointments,
		doctors:          deps.Doctors,
		patients:         deps.Patients,
		health:           deps.Health,
		metrics:          deps.Metrics,
		logger:           deps.Logger,
		build:            cfg.Build,
		serviceName:      cfg.ServiceName,
		environment:      string(cfg.Env),
		maxBodyBytes:     cfg.HTTP.MaxRequestBodyBytes,
		defaultPageSize:  cfg.Booking.DefaultPageSize,
		maxPageSize:      cfg.Booking.MaxPageSize,
		handlerTimeout:   cfg.HTTP.HandlerTimeout,
		readinessTimeout: readinessTimeoutDefault,
		metricsToken:     cfg.Telemetry.MetricsToken,
	}
}

// Handler builds the fully wrapped HTTP handler.
//
// Middleware order, outermost first, and why:
//
//  1. otelhttp      — starts the server span so everything below is inside it.
//  2. requestID     — every later layer, including panic recovery, can log it.
//  3. securityHeaders — cheap, and must apply even to error responses.
//  4. observe       — wraps recovery so a panic is still counted and logged as
//     the 500 the client actually received.
//  5. recoverPanic  — converts a panic into a problem response.
//  6. timeout       — innermost, so the deadline covers only handler work.
func (s *Server) Handler(metricsEnabled bool) http.Handler {
	router := chi.NewRouter()

	router.Use(requestID)
	router.Use(securityHeaders)
	router.Use(s.observe)
	router.Use(s.recoverPanic)
	router.Use(s.timeout(s.handlerTimeout))

	router.NotFound(s.handleNotFound)
	router.MethodNotAllowed(s.handleMethodNotAllowed)

	// Service metadata and the error catalogue.
	router.Get("/", s.handleServiceInfo)
	router.Get("/problems", s.handleListProblems)
	router.Get("/problems/{code}", s.handleGetProblem)

	// Health probes.
	router.Get("/livez", s.handleLivez)
	router.Get("/readyz", s.handleReadyz)

	// The assessment's required endpoints.
	router.Post("/appointments", s.handleBookAppointment)
	router.Get("/appointments/{appointmentID}", s.handleGetAppointment)
	router.Patch("/appointments/{appointmentID}/cancel", s.handleCancelAppointment)
	router.Patch("/appointments/{appointmentID}/reschedule", s.handleRescheduleAppointment)
	router.Get("/doctors/{doctorID}/availability", s.handleDoctorAvailability)
	router.Get("/patients/{patientID}/appointments", s.handlePatientAppointments)

	// Directory endpoints, so the API is usable without database access.
	router.Get("/doctors", s.handleListDoctors)
	router.Get("/doctors/{doctorID}", s.handleGetDoctor)
	router.Get("/patients", s.handleListPatients)
	router.Get("/patients/{patientID}", s.handleGetPatient)

	if metricsEnabled {
		router.Group(func(protected chi.Router) {
			if s.metricsToken != "" {
				protected.Use(s.requireMetricsToken(s.metricsToken))
			}
			// Registered for GET only rather than with chi.Handle, which would
			// bind every verb.
			protected.Get("/metrics", promhttp.HandlerFor(
				s.metrics.Registry(),
				promhttp.HandlerOpts{
					// Scrape failures must not become 500s in the API's own
					// error budget; report them to the scraper instead.
					ErrorHandling:     promhttp.ContinueOnError,
					EnableOpenMetrics: true,
				},
			).ServeHTTP)
		})
	}

	return otelhttp.NewHandler(router, "ratiba.api",
		// Probes and scrapes would otherwise dominate trace volume without
		// telling anyone anything.
		otelhttp.WithFilter(func(r *http.Request) bool {
			return !isProbePath(r.URL.Path)
		}),
	)
}

// isProbePath reports whether a path is infrastructure chatter rather than API
// traffic.
func isProbePath(path string) bool {
	switch path {
	case "/livez", "/readyz", "/metrics":
		return true
	default:
		return false
	}
}

// handleNotFound answers unmatched routes in the API's own error format, so a
// client never has to parse two different error shapes.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, apperror.New(apperror.KindNotFound, apperror.CodeNotFound,
		"No route matches this request. See GET / for the endpoint list."), s.logger)
}

// handleMethodNotAllowed answers a known path with the wrong method.
func (s *Server) handleMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	problem := apperror.New(apperror.KindNotFound, apperror.CodeMethodNotAllowed,
		"That method is not supported on this path.")
	// Kind maps to 404 by default; this specific case needs 405.
	writeJSONWithContentType(w, r, http.StatusMethodNotAllowed, Problem{
		Type:     "/problems/" + problem.Code,
		Title:    titleForStatus(http.StatusMethodNotAllowed),
		Status:   http.StatusMethodNotAllowed,
		Detail:   problem.Message,
		Instance: r.URL.Path,
		Code:     problem.Code,
	}, ProblemContentType, s.logger)
}
