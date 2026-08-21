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
	"github.com/JerryLegend254/ratiba/internal/platform/httpserver"
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
	readiness    *httpserver.ReadinessGate
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

	corsOrigins  []string
	metricsToken string
	openAPISpec  []byte
}

// Dependencies are the collaborators NewServer requires.
type Dependencies struct {
	Appointments *appointment.Service
	Doctors      doctor.Repository
	Patients     patient.Repository
	Health       HealthChecker
	Readiness    *httpserver.ReadinessGate
	Metrics      *observability.Metrics
	Logger       *slog.Logger
	OpenAPISpec  []byte
}

// NewServer builds the API server from configuration and dependencies.
func NewServer(cfg config.Config, deps Dependencies) *Server {
	return &Server{
		appointments:     deps.Appointments,
		doctors:          deps.Doctors,
		patients:         deps.Patients,
		health:           deps.Health,
		readiness:        deps.Readiness,
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
		corsOrigins:      cfg.Security.CORSAllowedOrigins,
		metricsToken:     cfg.Telemetry.MetricsToken,
		openAPISpec:      deps.OpenAPISpec,
	}
}

// Handler builds the fully wrapped HTTP handler.
//
// Middleware order, outermost first, and why:
//
//  1. otelhttp      — starts the server span so everything below is inside it.
//  2. requestID     — every later layer, including panic recovery, can log it.
//  3. securityHeaders / cors — cheap, and must apply even to error responses.
//  4. observe       — wraps recovery so a panic is still counted and logged as
//     the 500 the client actually received.
//  5. recoverPanic  — converts a panic into a problem response.
//  6. timeout       — innermost, so the deadline covers only handler work.
func (s *Server) Handler(metricsEnabled bool) http.Handler {
	return otelhttp.NewHandler(s.Routes(metricsEnabled), "ratiba.api",
		// Probes and scrapes would otherwise dominate trace volume without
		// telling anyone anything.
		otelhttp.WithFilter(func(r *http.Request) bool {
			return !isProbePath(r.URL.Path)
		}),
	)
}

// Routes builds the router itself, without the tracing wrapper.
//
// It is exported so the contract test can enumerate the served routes with
// chi.Walk and compare them against api/openapi.yaml in both directions. That
// check is what stops the published schema drifting from the implementation.
func (s *Server) Routes(metricsEnabled bool) chi.Router {
	router := chi.NewRouter()

	router.Use(requestID)
	router.Use(securityHeaders)
	if len(s.corsOrigins) > 0 {
		router.Use(cors(s.corsOrigins))
	}
	router.Use(s.observe)
	router.Use(s.recoverPanic)
	router.Use(s.timeout(s.handlerTimeout))

	router.NotFound(s.handleNotFound)
	router.MethodNotAllowed(s.handleMethodNotAllowed)

	// Service metadata and documentation.
	router.Get("/", s.handleServiceInfo)
	router.Get("/openapi.yaml", s.handleOpenAPISpec)
	router.Get("/docs", s.handleDocs)
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

// handleOpenAPISpec serves the embedded contract.
func (s *Server) handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(s.openAPISpec); err != nil {
		s.logger.DebugContext(r.Context(), "failed to write OpenAPI document",
			slog.String("error", err.Error()))
	}
}

// handleDocs serves an interactive API explorer.
//
// The Swagger UI assets come from a pinned CDN rather than being vendored: the
// dist bundle is over a megabyte, and this is a documentation page, not part of
// the API's runtime path. The page degrades to a plain link, and GET
// /openapi.yaml always works with no external dependency at all. That trade-off
// is recorded in docs/api.md.
func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(docsPage)); err != nil {
		s.logger.DebugContext(r.Context(), "failed to write docs page",
			slog.String("error", err.Error()))
	}
}

// docsPage is the Swagger UI host page.
const docsPage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Ratiba API reference</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5.17.14/swagger-ui.css">
  <style>
    body { margin: 0; font-family: system-ui, sans-serif; }
    .fallback { padding: 1rem 1.5rem; border-bottom: 1px solid #ddd; background: #fafafa; }
    .fallback a { color: #0b6; }
  </style>
</head>
<body>
  <div class="fallback">
    Ratiba clinic appointment API &mdash; raw contract:
    <a href="/openapi.yaml">/openapi.yaml</a> &middot;
    error catalogue: <a href="/problems">/problems</a>
  </div>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5.17.14/swagger-ui-bundle.js" crossorigin></script>
  <script>
    window.onload = function () {
      if (!window.SwaggerUIBundle) { return; }
      window.SwaggerUIBundle({ url: '/openapi.yaml', dom_id: '#swagger-ui', deepLinking: true });
    };
  </script>
</body>
</html>
`
