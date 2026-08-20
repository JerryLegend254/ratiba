package http

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JerryLegend254/ratiba/internal/appointment"
	"github.com/JerryLegend254/ratiba/internal/doctor"
	"github.com/JerryLegend254/ratiba/internal/patient"
	"github.com/JerryLegend254/ratiba/internal/platform/apperror"
	"github.com/JerryLegend254/ratiba/internal/platform/config"
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
	logger       *slog.Logger

	build       config.BuildInfo
	serviceName string
	environment string

	maxBodyBytes     int64
	defaultPageSize  int32
	maxPageSize      int32
	handlerTimeout   time.Duration
	readinessTimeout time.Duration
}

// Dependencies are the collaborators NewServer requires.
type Dependencies struct {
	Appointments *appointment.Service
	Doctors      doctor.Repository
	Patients     patient.Repository
	Health       HealthChecker
	Logger       *slog.Logger
}

// NewServer builds the API server from configuration and dependencies.
func NewServer(cfg config.Config, deps Dependencies) *Server {
	return &Server{
		appointments:     deps.Appointments,
		doctors:          deps.Doctors,
		patients:         deps.Patients,
		health:           deps.Health,
		logger:           deps.Logger,
		build:            cfg.Build,
		serviceName:      cfg.ServiceName,
		environment:      string(cfg.Env),
		maxBodyBytes:     cfg.HTTP.MaxRequestBodyBytes,
		defaultPageSize:  cfg.Booking.DefaultPageSize,
		maxPageSize:      cfg.Booking.MaxPageSize,
		handlerTimeout:   cfg.HTTP.HandlerTimeout,
		readinessTimeout: readinessTimeoutDefault,
	}
}

// Handler builds the fully wrapped HTTP handler.
//
// Middleware order, outermost first, and why:
//
//  1. requestID     — every later layer, including panic recovery, can log it.
//  2. securityHeaders — cheap, and must apply even to error responses.
//  3. accessLog     — wraps recovery so a panic is still logged as the 500 the
//     client actually received.
//  4. recoverPanic  — converts a panic into a problem response.
//  5. timeout       — innermost, so the deadline covers only handler work.
func (s *Server) Handler() http.Handler {
	router := chi.NewRouter()

	router.Use(requestID)
	router.Use(securityHeaders)
	router.Use(s.accessLog)
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

	return router
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
