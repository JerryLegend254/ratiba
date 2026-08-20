package http_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JerryLegend254/ratiba/api"
	"github.com/JerryLegend254/ratiba/internal/platform/apperror"
	"github.com/JerryLegend254/ratiba/internal/platform/clock"
	"github.com/JerryLegend254/ratiba/internal/platform/config"
	"github.com/JerryLegend254/ratiba/internal/platform/httpserver"
	"github.com/JerryLegend254/ratiba/internal/platform/logging"
	"github.com/JerryLegend254/ratiba/internal/platform/observability"
	"github.com/JerryLegend254/ratiba/internal/testsupport"
	transporthttp "github.com/JerryLegend254/ratiba/internal/transport/http"
)

// These tests exercise the real router, middleware stack and handlers over the
// in-memory store. They prove the HTTP contract — status codes, problem
// documents, header handling, strict decoding — without needing a database.
// Concurrency and transactional behaviour are proven separately, against real
// PostgreSQL, in internal/postgres.

const maxTestBodyBytes = 4096

type harness struct {
	server    *transporthttp.Server
	handler   http.Handler
	store     *testsupport.MemoryStore
	clock     *clock.Fixed
	readiness *httpserver.ReadinessGate
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	store := testsupport.NewClinic()
	clk := testsupport.NewFixedClock()

	service, err := testsupport.NewService(store, clk)
	if err != nil {
		t.Fatalf("build service: %v", err)
	}

	cfg := config.Config{
		Env:         config.EnvTest,
		ServiceName: "ratiba-api",
		Build:       config.BuildInfo{Version: "test", Commit: "abc1234", BuildTime: "now"},
		HTTP:        config.HTTPConfig{MaxRequestBodyBytes: maxTestBodyBytes, HandlerTimeout: 5 * time.Second},
		Booking:     config.BookingConfig{DefaultPageSize: 20, MaxPageSize: 100},
	}

	readiness := httpserver.NewReadinessGate()
	readiness.Open()

	server := transporthttp.NewServer(cfg, transporthttp.Dependencies{
		Appointments: service,
		Doctors:      store.Doctors(),
		Patients:     store.Patients(),
		Health:       store,
		Readiness:    readiness,
		Metrics:      observability.NewMetrics(cfg),
		Logger:       logging.Discard(),
		OpenAPISpec:  api.OpenAPISpec,
	})

	return &harness{
		server:    server,
		handler:   server.Handler(true),
		store:     store,
		clock:     clk,
		readiness: readiness,
	}
}

// do issues a request against the router and returns the recorded response.
func (h *harness) do(t *testing.T, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	// NewRequestWithContext rather than NewRequest, so the handler's context is
	// tied to the test and is cancelled when the test finishes.
	req := httptest.NewRequestWithContext(t.Context(), method, target, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, req)
	return recorder
}

// decodeBody unmarshals a response body into target.
func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), target); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
}

// assertProblem checks that a response is a well-formed problem document with
// the expected status and code.
func assertProblem(t *testing.T, recorder *httptest.ResponseRecorder, wantStatus int, wantCode string) transporthttp.Problem {
	t.Helper()

	if recorder.Code != wantStatus {
		t.Fatalf("expected status %d, got %d (body: %s)", wantStatus, recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != transporthttp.ProblemContentType {
		t.Errorf("expected content type %s, got %s", transporthttp.ProblemContentType, got)
	}

	var problem transporthttp.Problem
	decodeBody(t, recorder, &problem)

	if problem.Code != wantCode {
		t.Fatalf("expected code %s, got %s (detail: %s)", wantCode, problem.Code, problem.Detail)
	}
	if problem.Status != wantStatus {
		t.Errorf("the problem document reports status %d but the response was %d", problem.Status, wantStatus)
	}
	if problem.Title == "" || problem.Detail == "" {
		t.Error("a problem document must carry both a title and a detail")
	}
	if problem.RequestID == "" {
		t.Error("a problem document must carry the request ID so it can be correlated with logs")
	}
	return problem
}

// bookBody builds a booking request body for a Nairobi wall-clock time on
// Monday 2026-09-07.
func bookBody(t *testing.T, hour, minute int) string {
	t.Helper()
	start := time.Date(2026, 9, 7, hour, minute, 0, 0, testsupport.MustLocation("Africa/Nairobi"))
	return fmt.Sprintf(`{"doctor_id":%q,"patient_id":%q,"starts_at":%q}`,
		testsupport.NairobiDoctorID, testsupport.ActivePatientID, start.Format(time.RFC3339))
}

func TestBookAppointmentEndpoint(t *testing.T) {
	t.Parallel()

	t.Run("creates an appointment and returns 201 with a Location header", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)

		recorder := h.do(t, http.MethodPost, "/appointments", bookBody(t, 9, 0), nil)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d (%s)", recorder.Code, recorder.Body.String())
		}

		var body map[string]any
		decodeBody(t, recorder, &body)

		if body["status"] != "booked" {
			t.Errorf("expected status booked, got %v", body["status"])
		}
		if body["cancellation_reason"] != nil {
			t.Errorf("expected a null cancellation reason, got %v", body["cancellation_reason"])
		}
		if location := recorder.Header().Get("Location"); location != "/appointments/"+body["id"].(string) {
			t.Errorf("unexpected Location header: %s", location)
		}
		if recorder.Header().Get("X-Request-Id") == "" {
			t.Error("every response must carry a request ID")
		}
		if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("expected the nosniff header, got %q", got)
		}
	})

	t.Run("a taken slot is 409 slot_unavailable", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)

		if got := h.do(t, http.MethodPost, "/appointments", bookBody(t, 9, 0), nil).Code; got != http.StatusCreated {
			t.Fatalf("setup booking returned %d", got)
		}
		recorder := h.do(t, http.MethodPost, "/appointments", bookBody(t, 9, 0), nil)
		assertProblem(t, recorder, http.StatusConflict, apperror.CodeSlotUnavailable)
	})

	t.Run("rejects malformed and hostile bodies", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name        string
			body        string
			contentType string
			wantStatus  int
			wantCode    string
		}{
			{
				name:       "malformed JSON",
				body:       `{"doctor_id":`,
				wantStatus: http.StatusBadRequest,
				wantCode:   apperror.CodeMalformedJSON,
			},
			{
				name:       "unknown field",
				body:       `{"doctor_id":"7f3c0a1e-1111-4a10-9c01-000000000001","patient_id":"9b2d5e40-2222-4b20-8d02-000000000001","starts_at":"2026-09-07T06:00:00Z","start_at":"typo"}`,
				wantStatus: http.StatusBadRequest,
				wantCode:   apperror.CodeUnknownField,
			},
			{
				name:       "trailing JSON value",
				body:       `{"doctor_id":"7f3c0a1e-1111-4a10-9c01-000000000001","patient_id":"9b2d5e40-2222-4b20-8d02-000000000001","starts_at":"2026-09-07T06:00:00Z"}{"extra":true}`,
				wantStatus: http.StatusBadRequest,
				wantCode:   apperror.CodeTrailingContent,
			},
			{
				name:       "empty body",
				body:       ` `,
				wantStatus: http.StatusBadRequest,
				wantCode:   apperror.CodeMalformedJSON,
			},
			{
				name:        "wrong content type",
				body:        `{"doctor_id":"x"}`,
				contentType: "text/plain",
				wantStatus:  http.StatusUnsupportedMediaType,
				wantCode:    apperror.CodeUnsupportedMediaType,
			},
			{
				name:       "oversized body",
				body:       `{"doctor_id":"` + strings.Repeat("A", maxTestBodyBytes) + `"}`,
				wantStatus: http.StatusRequestEntityTooLarge,
				wantCode:   apperror.CodePayloadTooLarge,
			},
			{
				name:       "wrong field type",
				body:       `{"doctor_id":123,"patient_id":"9b2d5e40-2222-4b20-8d02-000000000001","starts_at":"2026-09-07T06:00:00Z"}`,
				wantStatus: http.StatusUnprocessableEntity,
				wantCode:   apperror.CodeValidationFailed,
			},
			{
				name:       "invalid UUID",
				body:       `{"doctor_id":"not-a-uuid","patient_id":"9b2d5e40-2222-4b20-8d02-000000000001","starts_at":"2026-09-07T06:00:00Z"}`,
				wantStatus: http.StatusUnprocessableEntity,
				wantCode:   apperror.CodeValidationFailed,
			},
			{
				name:       "timestamp without an offset",
				body:       `{"doctor_id":"7f3c0a1e-1111-4a10-9c01-000000000001","patient_id":"9b2d5e40-2222-4b20-8d02-000000000001","starts_at":"2026-09-07T09:00:00"}`,
				wantStatus: http.StatusUnprocessableEntity,
				wantCode:   apperror.CodeValidationFailed,
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				h := newHarness(t)

				headers := map[string]string{}
				if tc.contentType != "" {
					headers["Content-Type"] = tc.contentType
				}
				recorder := h.do(t, http.MethodPost, "/appointments", tc.body, headers)
				assertProblem(t, recorder, tc.wantStatus, tc.wantCode)
			})
		}
	})

	t.Run("business rule rejections carry the right code and status", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name       string
			hour       int
			minute     int
			wantStatus int
			wantCode   string
		}{
			{name: "before opening", hour: 8, minute: 0, wantStatus: 422, wantCode: apperror.CodeSlotTooSoon},
			{name: "misaligned", hour: 10, minute: 15, wantStatus: 422, wantCode: apperror.CodeSlotNotAligned},
			{name: "after closing", hour: 19, minute: 0, wantStatus: 422, wantCode: apperror.CodeSlotOutsideHours},
			{name: "in the lunch gap", hour: 13, minute: 30, wantStatus: 422, wantCode: apperror.CodeSlotOutsideHours},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				h := newHarness(t)

				recorder := h.do(t, http.MethodPost, "/appointments", bookBody(t, tc.hour, tc.minute), nil)
				assertProblem(t, recorder, tc.wantStatus, tc.wantCode)
			})
		}
	})

	t.Run("field violations name the offending field", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)

		body := `{"doctor_id":"","patient_id":"9b2d5e40-2222-4b20-8d02-000000000001","starts_at":"2026-09-07T06:00:00Z"}`
		problem := assertProblem(t,
			h.do(t, http.MethodPost, "/appointments", body, nil),
			http.StatusUnprocessableEntity, apperror.CodeValidationFailed)

		if len(problem.Violations) == 0 {
			t.Fatal("expected a field violation")
		}
		if problem.Violations[0].Field != "doctor_id" {
			t.Errorf("expected the violation on doctor_id, got %s", problem.Violations[0].Field)
		}
	})

}

func TestAvailabilityEndpoint(t *testing.T) {
	t.Parallel()

	t.Run("returns free slots with timezone context", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)

		recorder := h.do(t, http.MethodGet,
			"/doctors/"+testsupport.NairobiDoctorID.String()+"/availability?date=2026-09-07", "", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d (%s)", recorder.Code, recorder.Body.String())
		}

		var body struct {
			Date                string `json:"date"`
			Timezone            string `json:"timezone"`
			SlotDurationMinutes int    `json:"slot_duration_minutes"`
			MinLeadTimeMinutes  int    `json:"min_lead_time_minutes"`
			Slots               []struct {
				StartsAt      string `json:"starts_at"`
				EndsAt        string `json:"ends_at"`
				StartsAtLocal string `json:"starts_at_local"`
			} `json:"slots"`
		}
		decodeBody(t, recorder, &body)

		if body.Date != "2026-09-07" {
			t.Errorf("expected the requested date to be echoed, got %s", body.Date)
		}
		if body.Timezone != "Africa/Nairobi" {
			t.Errorf("expected the doctor's timezone, got %s", body.Timezone)
		}
		if body.SlotDurationMinutes != 30 || body.MinLeadTimeMinutes != 60 {
			t.Errorf("expected 30-minute slots and a 60-minute lead time, got %d and %d",
				body.SlotDurationMinutes, body.MinLeadTimeMinutes)
		}
		if len(body.Slots) != 14 {
			t.Fatalf("expected 14 slots, got %d", len(body.Slots))
		}
		if !strings.HasSuffix(body.Slots[0].StartsAt, "Z") {
			t.Errorf("instants must be rendered in UTC, got %s", body.Slots[0].StartsAt)
		}
		if !strings.Contains(body.Slots[0].StartsAtLocal, "+03:00") {
			t.Errorf("the local rendering must carry the doctor's offset, got %s", body.Slots[0].StartsAtLocal)
		}
	})

	t.Run("rejects a missing or malformed date", func(t *testing.T) {
		t.Parallel()

		tests := []struct{ name, query string }{
			{name: "missing", query: ""},
			{name: "wrong format", query: "?date=01-09-2026"},
			{name: "not a real date", query: "?date=2026-02-30"},
			{name: "empty", query: "?date="},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				h := newHarness(t)

				recorder := h.do(t, http.MethodGet,
					"/doctors/"+testsupport.NairobiDoctorID.String()+"/availability"+tc.query, "", nil)
				assertProblem(t, recorder, http.StatusBadRequest, apperror.CodeInvalidQueryParam)
			})
		}
	})

	t.Run("a malformed doctor ID is 400, not 404", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)

		recorder := h.do(t, http.MethodGet, "/doctors/not-a-uuid/availability?date=2026-09-07", "", nil)
		assertProblem(t, recorder, http.StatusBadRequest, apperror.CodeInvalidPathParameter)
	})

	t.Run("an unknown doctor is 404", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)

		recorder := h.do(t, http.MethodGet,
			"/doctors/00000000-0000-4000-8000-000000000099/availability?date=2026-09-07", "", nil)
		assertProblem(t, recorder, http.StatusNotFound, apperror.CodeDoctorNotFound)
	})

	t.Run("every offered slot is actually bookable", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)

		recorder := h.do(t, http.MethodGet,
			"/doctors/"+testsupport.NairobiDoctorID.String()+"/availability?date=2026-09-07", "", nil)

		var body struct {
			Slots []struct {
				StartsAt string `json:"starts_at"`
			} `json:"slots"`
		}
		decodeBody(t, recorder, &body)

		for _, slot := range body.Slots {
			payload := fmt.Sprintf(`{"doctor_id":%q,"patient_id":%q,"starts_at":%q}`,
				testsupport.NairobiDoctorID, testsupport.ActivePatientID, slot.StartsAt)
			if got := h.do(t, http.MethodPost, "/appointments", payload, nil).Code; got != http.StatusCreated {
				t.Fatalf("availability offered %s but booking it returned %d", slot.StartsAt, got)
			}
		}
	})
}

func TestCancelAndRescheduleEndpoints(t *testing.T) {
	t.Parallel()

	book := func(t *testing.T, h *harness, hour int) string {
		t.Helper()
		recorder := h.do(t, http.MethodPost, "/appointments", bookBody(t, hour, 0), nil)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("setup booking returned %d (%s)", recorder.Code, recorder.Body.String())
		}
		var body map[string]any
		decodeBody(t, recorder, &body)
		return body["id"].(string)
	}

	t.Run("cancel returns the cancelled appointment", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		id := book(t, h, 9)

		recorder := h.do(t, http.MethodPatch, "/appointments/"+id+"/cancel",
			`{"reason":"Patient is travelling"}`, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d (%s)", recorder.Code, recorder.Body.String())
		}

		var body map[string]any
		decodeBody(t, recorder, &body)
		if body["status"] != "cancelled" {
			t.Errorf("expected status cancelled, got %v", body["status"])
		}
		if body["cancellation_reason"] != "Patient is travelling" {
			t.Errorf("expected the reason to be echoed, got %v", body["cancellation_reason"])
		}
		if body["cancelled_at"] == nil {
			t.Error("expected a cancellation timestamp")
		}
	})

	t.Run("cancelling twice is 409", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		id := book(t, h, 9)

		h.do(t, http.MethodPatch, "/appointments/"+id+"/cancel", `{"reason":"first"}`, nil)
		recorder := h.do(t, http.MethodPatch, "/appointments/"+id+"/cancel", `{"reason":"second"}`, nil)
		assertProblem(t, recorder, http.StatusConflict, apperror.CodeAlreadyCancelled)
	})

	t.Run("a blank reason is 422 with a field violation", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		id := book(t, h, 9)

		problem := assertProblem(t,
			h.do(t, http.MethodPatch, "/appointments/"+id+"/cancel", `{"reason":"   "}`, nil),
			http.StatusUnprocessableEntity, apperror.CodeValidationFailed)

		if len(problem.Violations) == 0 || problem.Violations[0].Field != "reason" {
			t.Errorf("expected a violation on the reason field, got %+v", problem.Violations)
		}
	})

	t.Run("an unknown appointment is 404", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)

		recorder := h.do(t, http.MethodPatch,
			"/appointments/00000000-0000-4000-8000-000000000099/cancel", `{"reason":"x"}`, nil)
		assertProblem(t, recorder, http.StatusNotFound, apperror.CodeAppointmentNotFound)
	})

	t.Run("reschedule moves the appointment", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		id := book(t, h, 9)

		destination := time.Date(2026, 9, 7, 11, 0, 0, 0, testsupport.MustLocation("Africa/Nairobi"))
		recorder := h.do(t, http.MethodPatch, "/appointments/"+id+"/reschedule",
			fmt.Sprintf(`{"starts_at":%q}`, destination.Format(time.RFC3339)), nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d (%s)", recorder.Code, recorder.Body.String())
		}

		var body map[string]any
		decodeBody(t, recorder, &body)
		if body["starts_at"] != destination.UTC().Format(time.RFC3339) {
			t.Errorf("expected the appointment at %s, got %v",
				destination.UTC().Format(time.RFC3339), body["starts_at"])
		}
	})

	t.Run("rescheduling to the current slot is 409", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		id := book(t, h, 9)

		start := time.Date(2026, 9, 7, 9, 0, 0, 0, testsupport.MustLocation("Africa/Nairobi"))
		recorder := h.do(t, http.MethodPatch, "/appointments/"+id+"/reschedule",
			fmt.Sprintf(`{"starts_at":%q}`, start.Format(time.RFC3339)), nil)
		assertProblem(t, recorder, http.StatusConflict, apperror.CodeRescheduleSameSlot)
	})

	t.Run("rescheduling a cancelled appointment is 409", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		id := book(t, h, 9)

		h.do(t, http.MethodPatch, "/appointments/"+id+"/cancel", `{"reason":"not needed"}`, nil)

		destination := time.Date(2026, 9, 7, 11, 0, 0, 0, testsupport.MustLocation("Africa/Nairobi"))
		recorder := h.do(t, http.MethodPatch, "/appointments/"+id+"/reschedule",
			fmt.Sprintf(`{"starts_at":%q}`, destination.Format(time.RFC3339)), nil)
		assertProblem(t, recorder, http.StatusConflict, apperror.CodeAlreadyCancelled)
	})
}

func TestPatientAppointmentsEndpoint(t *testing.T) {
	t.Parallel()

	t.Run("returns a page with pagination metadata", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)

		for _, hour := range []int{9, 10, 11} {
			if got := h.do(t, http.MethodPost, "/appointments", bookBody(t, hour, 0), nil).Code; got != http.StatusCreated {
				t.Fatalf("setup booking returned %d", got)
			}
		}

		recorder := h.do(t, http.MethodGet,
			"/patients/"+testsupport.ActivePatientID.String()+"/appointments?limit=2", "", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", recorder.Code)
		}

		var body struct {
			Data []struct {
				StartsAt      string `json:"starts_at"`
				StartsAtLocal string `json:"starts_at_local"`
				Doctor        struct {
					FullName string `json:"full_name"`
				} `json:"doctor"`
			} `json:"data"`
			Pagination struct {
				Limit   int32 `json:"limit"`
				Offset  int32 `json:"offset"`
				Total   int64 `json:"total"`
				HasMore bool  `json:"has_more"`
			} `json:"pagination"`
		}
		decodeBody(t, recorder, &body)

		if len(body.Data) != 2 {
			t.Fatalf("expected 2 items, got %d", len(body.Data))
		}
		if body.Pagination.Total != 3 || !body.Pagination.HasMore {
			t.Errorf("expected total 3 with has_more true, got %d/%v",
				body.Pagination.Total, body.Pagination.HasMore)
		}
		if body.Data[0].Doctor.FullName == "" {
			t.Error("each item must embed its doctor")
		}
		if !strings.Contains(body.Data[0].StartsAtLocal, "+03:00") {
			t.Errorf("expected a local rendering with offset, got %s", body.Data[0].StartsAtLocal)
		}
		if body.Data[0].StartsAt > body.Data[1].StartsAt {
			t.Error("appointments must be ordered chronologically")
		}
	})

	t.Run("an empty result is an empty array, never null", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)

		recorder := h.do(t, http.MethodGet,
			"/patients/"+testsupport.OtherPatientID.String()+"/appointments", "", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), `"data":[]`) {
			t.Errorf("expected an empty array, got %s", recorder.Body.String())
		}
	})

	t.Run("out-of-range paging parameters are rejected, not clamped", func(t *testing.T) {
		t.Parallel()

		for _, query := range []string{"?limit=0", "?limit=101", "?limit=abc", "?offset=-1", "?offset=xyz"} {
			t.Run(query, func(t *testing.T) {
				t.Parallel()
				h := newHarness(t)

				recorder := h.do(t, http.MethodGet,
					"/patients/"+testsupport.ActivePatientID.String()+"/appointments"+query, "", nil)
				assertProblem(t, recorder, http.StatusBadRequest, apperror.CodeInvalidQueryParam)
			})
		}
	})

	t.Run("an unknown patient is 404", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)

		recorder := h.do(t, http.MethodGet,
			"/patients/00000000-0000-4000-8000-000000000099/appointments", "", nil)
		assertProblem(t, recorder, http.StatusNotFound, apperror.CodePatientNotFound)
	})
}

func TestOperationalEndpoints(t *testing.T) {
	t.Parallel()

	t.Run("livez reports alive without touching the database", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.store.PingErr = fmt.Errorf("database is down")

		recorder := h.do(t, http.MethodGet, "/livez", "", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("liveness must not depend on the database, got %d", recorder.Code)
		}
	})

	t.Run("readyz reports ready when the database answers", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)

		recorder := h.do(t, http.MethodGet, "/readyz", "", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d (%s)", recorder.Code, recorder.Body.String())
		}

		var body struct {
			Status string            `json:"status"`
			Checks map[string]string `json:"checks"`
		}
		decodeBody(t, recorder, &body)
		if body.Status != "ready" || body.Checks["database"] != "ok" {
			t.Errorf("unexpected readiness body: %+v", body)
		}
	})

	t.Run("readyz reports 503 when the database is unreachable", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.store.PingErr = fmt.Errorf("connection refused to postgres://user:secret@db:5432/ratiba")

		recorder := h.do(t, http.MethodGet, "/readyz", "", nil)
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d", recorder.Code)
		}

		var body struct {
			Status string            `json:"status"`
			Checks map[string]string `json:"checks"`
		}
		decodeBody(t, recorder, &body)
		if body.Status != "not_ready" || body.Checks["database"] != "unavailable" {
			t.Errorf("unexpected readiness body: %+v", body)
		}
		// The probe is unauthenticated and internet-facing on Railway, so it
		// must never echo the driver error.
		if strings.Contains(recorder.Body.String(), "secret") || strings.Contains(recorder.Body.String(), "postgres://") {
			t.Fatalf("the readiness response leaked internal detail: %s", recorder.Body.String())
		}
	})

	t.Run("readyz reports 503 while draining", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.readiness.Close()

		recorder := h.do(t, http.MethodGet, "/readyz", "", nil)
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 during shutdown, got %d", recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), "draining") {
			t.Errorf("expected the body to say it is draining, got %s", recorder.Body.String())
		}
	})

	t.Run("metrics are exposed when unprotected", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)

		h.do(t, http.MethodPost, "/appointments", bookBody(t, 9, 0), nil)

		recorder := h.do(t, http.MethodGet, "/metrics", "", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", recorder.Code)
		}
		body := recorder.Body.String()
		for _, want := range []string{"http_requests_total", "http_request_duration_seconds", "ratiba_build_info"} {
			if !strings.Contains(body, want) {
				t.Errorf("expected the %s metric to be exposed", want)
			}
		}
		// The route label must be the template, never the raw path with IDs.
		if strings.Contains(body, testsupport.NairobiDoctorID.String()) {
			t.Error("metrics must not contain identifiers; route labels would be unbounded")
		}
	})

	t.Run("the OpenAPI document and docs page are served", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)

		spec := h.do(t, http.MethodGet, "/openapi.yaml", "", nil)
		if spec.Code != http.StatusOK {
			t.Fatalf("expected the spec to be served, got %d", spec.Code)
		}
		if !strings.Contains(spec.Body.String(), "openapi: 3.1.0") {
			t.Error("the served document does not look like the OpenAPI contract")
		}

		docs := h.do(t, http.MethodGet, "/docs", "", nil)
		if docs.Code != http.StatusOK {
			t.Fatalf("expected the docs page to be served, got %d", docs.Code)
		}
	})

	t.Run("the problem catalogue documents every code the API uses", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)

		recorder := h.do(t, http.MethodGet, "/problems", "", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", recorder.Code)
		}

		var body struct {
			Data []struct {
				Code    string `json:"code"`
				Status  int    `json:"status"`
				Title   string `json:"title"`
				Meaning string `json:"meaning"`
			} `json:"data"`
		}
		decodeBody(t, recorder, &body)

		documented := map[string]bool{}
		for _, item := range body.Data {
			if item.Meaning == "" || item.Title == "" || item.Status == 0 {
				t.Errorf("catalogue entry %s is incomplete", item.Code)
			}
			documented[item.Code] = true
		}

		// Every code the handlers can emit must be described. A code that
		// escapes into a response without an entry here is undocumented API
		// surface.
		required := []string{
			apperror.CodeSlotUnavailable, apperror.CodeSlotNotAligned, apperror.CodeSlotOutsideHours,
			apperror.CodeSlotInPast, apperror.CodeSlotTooSoon, apperror.CodeDoctorNotWorking,
			apperror.CodeAlreadyCancelled, apperror.CodeRescheduleSameSlot,
			apperror.CodeDoctorNotFound, apperror.CodePatientNotFound, apperror.CodeAppointmentNotFound,
			apperror.CodeDoctorInactive, apperror.CodePatientInactive,
			apperror.CodeMalformedJSON, apperror.CodeUnknownField, apperror.CodeTrailingContent,
			apperror.CodeUnsupportedMediaType, apperror.CodePayloadTooLarge,
			apperror.CodeInvalidPathParameter, apperror.CodeInvalidQueryParam,
			apperror.CodeValidationFailed, apperror.CodeInternalError,
		}
		for _, code := range required {
			if !documented[code] {
				t.Errorf("error code %q is returned by the API but missing from /problems", code)
			}
		}
	})

	t.Run("a problem type URI resolves", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)

		problem := assertProblem(t,
			h.do(t, http.MethodPost, "/appointments", `{"bad":`, nil),
			http.StatusBadRequest, apperror.CodeMalformedJSON)

		recorder := h.do(t, http.MethodGet, problem.Type, "", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("the type URI %s did not resolve (%d)", problem.Type, recorder.Code)
		}
	})

	t.Run("unknown routes and methods return problem documents", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)

		assertProblem(t, h.do(t, http.MethodGet, "/nope", "", nil),
			http.StatusNotFound, apperror.CodeNotFound)

		recorder := h.do(t, http.MethodDelete, "/appointments", "", nil)
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("expected 405, got %d", recorder.Code)
		}
		if got := recorder.Header().Get("Content-Type"); got != transporthttp.ProblemContentType {
			t.Errorf("expected a problem document, got content type %s", got)
		}
	})

	t.Run("the service root advertises the endpoints and build", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)

		recorder := h.do(t, http.MethodGet, "/", "", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", recorder.Code)
		}

		var body struct {
			Service   string   `json:"service"`
			Version   string   `json:"version"`
			Commit    string   `json:"commit"`
			Endpoints []string `json:"endpoints"`
		}
		decodeBody(t, recorder, &body)
		if body.Version != "test" || body.Commit != "abc1234" {
			t.Errorf("expected the injected build info, got %s/%s", body.Version, body.Commit)
		}
		if len(body.Endpoints) == 0 {
			t.Error("expected the endpoint list to be advertised")
		}
	})
}

func TestRequestIDHandling(t *testing.T) {
	t.Parallel()

	t.Run("a safe inbound request ID is honoured", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)

		recorder := h.do(t, http.MethodGet, "/livez", "",
			map[string]string{"X-Request-Id": "trace-abc-123"})
		if got := recorder.Header().Get("X-Request-Id"); got != "trace-abc-123" {
			t.Errorf("expected the inbound ID to be echoed, got %q", got)
		}
	})

	t.Run("an unsafe inbound request ID is replaced", func(t *testing.T) {
		t.Parallel()

		unsafe := []string{
			"has space",
			"newline\ninjected",
			strings.Repeat("x", 65),
			`{"json":"injection"}`,
		}

		for _, value := range unsafe {
			h := newHarness(t)
			recorder := h.do(t, http.MethodGet, "/livez", "", map[string]string{"X-Request-Id": value})

			got := recorder.Header().Get("X-Request-Id")
			if got == value {
				t.Errorf("unsafe request ID %q was echoed back verbatim", value)
			}
			if got == "" {
				t.Error("a replacement request ID should always be generated")
			}
		}
	})
}

func TestCORSIsDisabledByDefault(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	recorder := h.do(t, http.MethodGet, "/livez", "", map[string]string{"Origin": "https://evil.example"})
	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("CORS must be off unless configured, got Allow-Origin %q", got)
	}
}
