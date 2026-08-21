package http_test

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"

	"github.com/JerryLegend254/ratiba/api"
)

// The OpenAPI document is the published contract. These tests make sure it is
// valid, and that it stays in step with the routes the server actually serves —
// a contract that silently drifts from the implementation is worse than no
// contract, because clients trust it.

func TestOpenAPIDocumentIsValid(t *testing.T) {
	t.Parallel()

	loader := &openapi3.Loader{IsExternalRefsAllowed: false}
	doc, err := loader.LoadFromData(api.OpenAPISpec)
	if err != nil {
		t.Fatalf("the OpenAPI document could not be parsed: %v", err)
	}

	if err := doc.Validate(t.Context()); err != nil {
		t.Fatalf("the OpenAPI document is not valid: %v", err)
	}

	if doc.Info == nil || doc.Info.Title == "" || doc.Info.Version == "" {
		t.Error("the document must declare a title and version")
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1") {
		t.Errorf("expected OpenAPI 3.1, got %s", doc.OpenAPI)
	}
}

// TestOpenAPIMatchesRoutes walks the real router and compares it with the
// document in both directions.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	t.Parallel()

	doc := loadSpec(t)
	documented := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			documented[method+" "+normalisePath(path)] = true
		}
	}

	implemented := map[string]bool{}
	router := newHarness(t).server.Routes(true)
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		implemented[method+" "+normalisePath(route)] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk routes: %v", err)
	}

	t.Run("every implemented route is documented", func(t *testing.T) {
		t.Parallel()
		for route := range implemented {
			if !documented[route] {
				t.Errorf("route %q is served but missing from api/openapi.yaml", route)
			}
		}
	})

	t.Run("every documented route is implemented", func(t *testing.T) {
		t.Parallel()
		for route := range documented {
			if !implemented[route] {
				t.Errorf("route %q is documented in api/openapi.yaml but not served", route)
			}
		}
	})
}

// TestOpenAPIDocumentsErrorResponses checks that each mutating operation
// documents the failure modes a client has to handle, not just the happy path.
func TestOpenAPIDocumentsErrorResponses(t *testing.T) {
	t.Parallel()

	doc := loadSpec(t)

	required := map[string][]string{
		"POST /appointments":                             {"201", "400", "404", "409", "415", "422", "500"},
		"PATCH /appointments/{appointmentId}/cancel":     {"200", "400", "404", "409", "422", "500"},
		"PATCH /appointments/{appointmentId}/reschedule": {"200", "400", "404", "409", "422", "500"},
		"GET /doctors/{doctorId}/availability":           {"200", "400", "404", "500"},
		"GET /patients/{patientId}/appointments":         {"200", "400", "404", "500"},
	}

	for route, statuses := range required {
		parts := strings.SplitN(route, " ", 2)
		method, path := parts[0], parts[1]

		item := doc.Paths.Find(path)
		if item == nil {
			t.Errorf("path %s is not documented", path)
			continue
		}
		operation := item.Operations()[method]
		if operation == nil {
			t.Errorf("operation %s is not documented", route)
			continue
		}

		for _, status := range statuses {
			if operation.Responses.Value(status) == nil {
				t.Errorf("%s does not document a %s response", route, status)
			}
		}
	}
}

// TestOpenAPIProblemSchemaMatchesImplementation guards the error contract: the
// documented Problem schema must require the fields the server always sends.
func TestOpenAPIProblemSchemaMatchesImplementation(t *testing.T) {
	t.Parallel()

	doc := loadSpec(t)
	schemaRef, ok := doc.Components.Schemas["Problem"]
	if !ok {
		t.Fatal("the Problem schema is not defined")
	}

	required := schemaRef.Value.Required
	sort.Strings(required)
	for _, field := range []string{"code", "detail", "status", "title", "type"} {
		if !contains(required, field) {
			t.Errorf("the Problem schema should require %q; the server always sends it", field)
		}
	}

	for _, field := range []string{"request_id", "trace_id", "violations", "instance"} {
		if _, ok := schemaRef.Value.Properties[field]; !ok {
			t.Errorf("the Problem schema does not document the %q member", field)
		}
	}
}

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()

	loader := &openapi3.Loader{IsExternalRefsAllowed: false}
	doc, err := loader.LoadFromData(api.OpenAPISpec)
	if err != nil {
		t.Fatalf("load OpenAPI document: %v", err)
	}
	return doc
}

// pathParam matches a templated path segment.
var pathParam = regexp.MustCompile(`\{[^}]*\}`)

// normalisePath erases parameter names so chi's {doctorID} and OpenAPI's
// {doctorId} compare equal. Only the path shape and method matter for drift.
func normalisePath(path string) string {
	normalised := pathParam.ReplaceAllString(path, "{}")
	normalised = strings.TrimSuffix(normalised, "/")
	if normalised == "" {
		return "/"
	}
	return normalised
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
