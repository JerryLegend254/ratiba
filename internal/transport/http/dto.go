package http

import (
	"time"

	"github.com/JerryLegend254/ratiba/internal/appointment"
	"github.com/JerryLegend254/ratiba/internal/doctor"
	"github.com/JerryLegend254/ratiba/internal/patient"
	"github.com/JerryLegend254/ratiba/internal/platform/calendar"
)

// Wire format conventions, applied everywhere in this file:
//
//   - Instants are RFC 3339 in UTC ("2026-09-01T09:00:00Z"). One representation
//     on the wire means no client has to guess.
//   - Where a local rendering genuinely helps a human read the response, it is
//     provided as an ADDITIONAL field alongside the UTC one, never instead of it.
//   - Single resources are returned as bare objects; collections are wrapped in
//     {"data": [...], "pagination": {...}} so a page can carry metadata.
//   - Identifiers are UUIDs, rendered as strings.

// bookAppointmentRequest is the POST /appointments body.
type bookAppointmentRequest struct {
	DoctorID  string `json:"doctor_id"`
	PatientID string `json:"patient_id"`
	StartsAt  string `json:"starts_at"`
}

// cancelAppointmentRequest is the PATCH /appointments/{id}/cancel body.
type cancelAppointmentRequest struct {
	Reason string `json:"reason"`
}

// rescheduleAppointmentRequest is the PATCH /appointments/{id}/reschedule body.
type rescheduleAppointmentRequest struct {
	StartsAt string `json:"starts_at"`
}

// appointmentResponse is the canonical appointment representation.
type appointmentResponse struct {
	ID                 string     `json:"id"`
	DoctorID           string     `json:"doctor_id"`
	PatientID          string     `json:"patient_id"`
	StartsAt           time.Time  `json:"starts_at"`
	EndsAt             time.Time  `json:"ends_at"`
	Status             string     `json:"status"`
	CancellationReason *string    `json:"cancellation_reason"`
	CancelledAt        *time.Time `json:"cancelled_at"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

func toAppointmentResponse(a appointment.Appointment) appointmentResponse {
	return appointmentResponse{
		ID:                 a.ID.String(),
		DoctorID:           a.DoctorID.String(),
		PatientID:          a.PatientID.String(),
		StartsAt:           a.StartsAt.UTC(),
		EndsAt:             a.EndsAt.UTC(),
		Status:             string(a.Status),
		CancellationReason: a.CancellationReason,
		CancelledAt:        utcPointer(a.CancelledAt),
		CreatedAt:          a.CreatedAt.UTC(),
		UpdatedAt:          a.UpdatedAt.UTC(),
	}
}

// patientAppointmentResponse embeds the doctor so a patient's list renders
// without N additional requests.
type patientAppointmentResponse struct {
	appointmentResponse
	Doctor doctorSummaryResponse `json:"doctor"`
	// StartsAtLocal renders the start in the doctor's timezone, which is the
	// clinic's frame of reference and the one a patient recognises.
	StartsAtLocal string `json:"starts_at_local"`
}

// doctorSummaryResponse is the compact doctor representation embedded in other
// payloads.
type doctorSummaryResponse struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	FullName  string `json:"full_name"`
	Specialty string `json:"specialty"`
	Timezone  string `json:"timezone"`
}

// doctorResponse is the full doctor representation.
type doctorResponse struct {
	doctorSummaryResponse
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toDoctorSummary(d doctor.Doctor) doctorSummaryResponse {
	return doctorSummaryResponse{
		ID:        d.ID.String(),
		Slug:      d.Slug,
		FullName:  d.FullName,
		Specialty: d.Specialty,
		Timezone:  d.Timezone,
	}
}

func toDoctorResponse(d doctor.Doctor) doctorResponse {
	return doctorResponse{
		doctorSummaryResponse: toDoctorSummary(d),
		IsActive:              d.IsActive,
		CreatedAt:             d.CreatedAt.UTC(),
		UpdatedAt:             d.UpdatedAt.UTC(),
	}
}

// patientResponse is the patient directory representation.
//
// It carries an email because the seeded directory is how a reviewer finds a
// patient to book for. In a system with real patients this endpoint would sit
// behind authorisation; see docs/security.md.
type patientResponse struct {
	ID        string    `json:"id"`
	FullName  string    `json:"full_name"`
	Email     string    `json:"email"`
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toPatientResponse(p patient.Patient) patientResponse {
	return patientResponse{
		ID:        p.ID.String(),
		FullName:  p.FullName,
		Email:     p.Email,
		IsActive:  p.IsActive,
		CreatedAt: p.CreatedAt.UTC(),
		UpdatedAt: p.UpdatedAt.UTC(),
	}
}

// slotResponse is one free 30-minute slot.
type slotResponse struct {
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
	// StartsAtLocal is the same instant in the doctor's timezone, offset
	// included, so a human can verify the clinic-local hour at a glance.
	StartsAtLocal string `json:"starts_at_local"`
}

// availabilityResponse is the GET /doctors/{id}/availability payload.
type availabilityResponse struct {
	Doctor doctorSummaryResponse `json:"doctor"`
	// Date is the local calendar date the slots belong to, in the doctor's
	// timezone.
	Date string `json:"date"`
	// Timezone repeats the doctor's IANA zone so a client can interpret Date
	// without cross-referencing the doctor object.
	Timezone            string         `json:"timezone"`
	SlotDurationMinutes int            `json:"slot_duration_minutes"`
	MinLeadTimeMinutes  int            `json:"min_lead_time_minutes"`
	Slots               []slotResponse `json:"slots"`
}

// pagination describes an offset-based page.
type pagination struct {
	Limit  int32 `json:"limit"`
	Offset int32 `json:"offset"`
	Total  int64 `json:"total"`
	// HasMore saves a client from computing offset+limit < total itself.
	HasMore bool `json:"has_more"`
}

func newPagination(limit, offset int32, total int64) pagination {
	return pagination{
		Limit:   limit,
		Offset:  offset,
		Total:   total,
		HasMore: int64(offset)+int64(limit) < total,
	}
}

// collection is the envelope for every list endpoint.
type collection[T any] struct {
	Data       []T        `json:"data"`
	Pagination pagination `json:"pagination"`
}

// healthResponse is the /livez and /readyz payload.
type healthResponse struct {
	Status string `json:"status"`
	// Checks maps a dependency name to "ok" or "unavailable". It never
	// contains connection strings or driver error text.
	Checks  map[string]string `json:"checks,omitempty"`
	Version string            `json:"version,omitempty"`
	Commit  string            `json:"commit,omitempty"`
}

// serviceInfoResponse is the API root payload, giving a reviewer a map of the
// service without reading the docs first.
type serviceInfoResponse struct {
	Service     string            `json:"service"`
	Version     string            `json:"version"`
	Commit      string            `json:"commit"`
	Environment string            `json:"environment"`
	Docs        map[string]string `json:"docs"`
	Endpoints   []string          `json:"endpoints"`
}

// problemDescription documents one error code, served at /problems/{code}.
type problemDescription struct {
	Code   string `json:"code"`
	Status int    `json:"status"`
	Title  string `json:"title"`
	// Meaning explains when the code is returned and what to do about it.
	Meaning string `json:"meaning"`
}

// localRFC3339 renders an instant in a location, preserving the offset.
func localRFC3339(t time.Time, loc *time.Location) string {
	return t.In(loc).Format(time.RFC3339)
}

func utcPointer(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	utc := t.UTC()
	return &utc
}

// formatDate renders a calendar date for the wire.
func formatDate(d calendar.Date) string { return d.String() }
