package http_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The access log and the metrics are the only view an operator has into a live
// system. If they disagree with what the client actually received, every
// dashboard, alert and incident investigation built on them is wrong — and
// wrong in the most dangerous direction, because a 4xx counted as a 2xx makes a
// broken service look healthy.
//
// This file pins that agreement. It exists because an earlier version of the
// recorder was seeded with 200 before the handler ran; since WriteHeader only
// records the first status (matching net/http, which ignores later calls), the
// seed WAS the first status and every error was logged and counted as a
// success. It was invisible in every other test, because they assert on the
// response the client sees, not on what was recorded about it. It showed up
// only when reading real container logs.

// TestObservedStatusMatchesResponseStatus checks the recorded status against the
// real one across the full range of outcomes.
func TestObservedStatusMatchesResponseStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		method     string
		target     string
		body       string
		wantStatus int
	}{
		{name: "success", method: http.MethodGet, target: "/livez", wantStatus: 200},
		{name: "created", method: http.MethodPost, target: "/appointments", body: "", wantStatus: 201},
		{name: "bad request", method: http.MethodPost, target: "/appointments", body: `{"broken":`, wantStatus: 400},
		{name: "not found", method: http.MethodGet, target: "/no-such-route", wantStatus: 404},
		{name: "method not allowed", method: http.MethodDelete, target: "/appointments", wantStatus: 405},
		{
			name:       "unprocessable",
			method:     http.MethodPost,
			target:     "/appointments",
			body:       `{"doctor_id":"not-a-uuid","patient_id":"9b2d5e40-2222-4b20-8d02-000000000001","starts_at":"2026-09-07T06:00:00Z"}`,
			wantStatus: 422,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h, logged := newHarnessWithLogs(t)

			body := tc.body
			if tc.name == "created" {
				body = bookBody(t, 9, 0)
			}

			recorder := h.do(t, tc.method, tc.target, body, nil)
			if recorder.Code != tc.wantStatus {
				t.Fatalf("expected the response to be %d, got %d (%s)",
					tc.wantStatus, recorder.Code, recorder.Body.String())
			}

			// Find the access-log line and compare its status with reality.
			var found bool
			for line := range strings.SplitSeq(strings.TrimSpace(logged.String()), "\n") {
				if line == "" {
					continue
				}
				var entry map[string]any
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					continue
				}
				if entry["event"] != "http.request" {
					continue
				}
				found = true

				status, ok := entry["status"].(float64)
				if !ok {
					t.Fatalf("the access log has no numeric status: %s", line)
				}
				if int(status) != tc.wantStatus {
					t.Fatalf("the client received %d but the access log recorded %d.\n"+
						"Logs and metrics that disagree with reality make a broken service look healthy.\n"+
						"log line: %s", tc.wantStatus, int(status), line)
				}
			}
			if !found {
				t.Fatalf("no access-log line was written. Logged output:\n%s", logged.String())
			}
		})
	}
}

// TestMetricsRecordTheCorrectStatusClass checks the same agreement on the
// metrics side, since they are labelled independently of the log.
func TestMetricsRecordTheCorrectStatusClass(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// One request in each class.
	h.do(t, http.MethodGet, "/livez", "", nil)                   // 2xx
	h.do(t, http.MethodGet, "/no-such-route", "", nil)           // 4xx
	h.do(t, http.MethodPost, "/appointments", `{"broken":`, nil) // 4xx

	metrics := h.do(t, http.MethodGet, "/metrics", "", nil).Body.String()

	if !strings.Contains(metrics, `status_class="2xx"`) {
		t.Error("no 2xx series was recorded despite a successful request")
	}
	if !strings.Contains(metrics, `status_class="4xx"`) {
		t.Fatal("no 4xx series was recorded despite two failing requests — " +
			"errors are being counted as successes")
	}
}

// TestAccessLogOmitsSensitiveData is the counterpart guarantee: the log must be
// complete enough to debug with, and must never contain clinical or credential
// material.
func TestAccessLogOmitsSensitiveData(t *testing.T) {
	t.Parallel()

	h, logged := newHarnessWithLogs(t)

	created := h.do(t, http.MethodPost, "/appointments", bookBody(t, 9, 0), nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("setup booking returned %d", created.Code)
	}

	var appointment map[string]any
	decodeBody(t, created, &appointment)
	id := appointment["id"].(string)

	const reason = "Patient is having chemotherapy that week"
	cancelled := h.do(t, http.MethodPatch, "/appointments/"+id+"/cancel",
		`{"reason":"`+reason+`"}`, nil)
	if cancelled.Code != http.StatusOK {
		t.Fatalf("cancel returned %d", cancelled.Code)
	}

	output := logged.String()

	t.Run("the cancellation reason is never logged", func(t *testing.T) {
		t.Parallel()
		// A patient's free text is clinical data. It is stored on the row where
		// the business needs it and must not reach a log aggregator.
		if strings.Contains(output, reason) {
			t.Fatalf("the cancellation reason leaked into the logs:\n%s", output)
		}
		if strings.Contains(output, "chemotherapy") {
			t.Fatal("clinical free text leaked into the logs")
		}
	})

	t.Run("the route template is logged, not the raw path", func(t *testing.T) {
		t.Parallel()
		// The raw path contains appointment UUIDs. Logging it would make log
		// cardinality unbounded and put identifiers in every line.
		if !strings.Contains(output, "/appointments/{appointmentID}/cancel") {
			t.Error("the matched route template is missing from the access log")
		}
		if strings.Contains(output, "/appointments/"+id+"/cancel") {
			t.Errorf("the raw path with an identifier was logged instead of the route template")
		}
	})

	t.Run("the request ID is on every line", func(t *testing.T) {
		t.Parallel()
		requestID := cancelled.Header().Get("X-Request-Id")
		if requestID == "" {
			t.Fatal("no request ID was returned")
		}
		if !strings.Contains(output, requestID) {
			t.Error("the request ID is missing from the logs, so a user's report could not be correlated")
		}
	})

	t.Run("domain events are logged with safe fields", func(t *testing.T) {
		t.Parallel()
		if !strings.Contains(output, "appointment.cancelled") {
			t.Error("the cancellation domain event was not logged")
		}
	})
}
