package http

import (
	"net/http"
	"time"

	"github.com/JerryLegend254/ratiba/internal/appointment"
)

// handleBookAppointment implements POST /appointments.
//
// Responds 201 with the created appointment, or 409 when the slot was taken.
func (s *Server) handleBookAppointment(w http.ResponseWriter, r *http.Request) {
	body, err := decodeJSON[bookAppointmentRequest](w, r, s.maxBodyBytes)
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}

	doctorID, err := requireUUID(body.DoctorID, "doctor_id")
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}
	patientID, err := requireUUID(body.PatientID, "patient_id")
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}
	startsAt, err := parseTimestamp(body.StartsAt, "starts_at")
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}

	result, err := s.appointments.Book(r.Context(), appointment.BookCommand{
		DoctorID:  doctorID,
		PatientID: patientID,
		StartsAt:  startsAt,
	})
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}

	w.Header().Set("Location", "/appointments/"+result.Appointment.ID.String())
	writeJSON(w, r, http.StatusCreated, toAppointmentResponse(result.Appointment), s.logger)
}

// handleGetAppointment implements GET /appointments/{appointmentID}.
//
// Cancelled appointments are returned as well as booked ones: a client that
// holds an appointment ID should be able to discover that it was cancelled.
func (s *Server) handleGetAppointment(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "appointmentID")
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}

	appt, err := s.appointments.Get(r.Context(), id)
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}
	writeJSON(w, r, http.StatusOK, toAppointmentResponse(appt), s.logger)
}

// handleCancelAppointment implements PATCH /appointments/{appointmentID}/cancel.
//
// Responds 200 with the cancelled appointment, 409 if it was already cancelled,
// and 422 if the reason is missing or too long.
func (s *Server) handleCancelAppointment(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "appointmentID")
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}

	body, err := decodeJSON[cancelAppointmentRequest](w, r, s.maxBodyBytes)
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}

	cancelled, err := s.appointments.Cancel(r.Context(), appointment.CancelCommand{
		ID:     id,
		Reason: body.Reason,
	})
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}
	writeJSON(w, r, http.StatusOK, toAppointmentResponse(cancelled), s.logger)
}

// handleRescheduleAppointment implements
// PATCH /appointments/{appointmentID}/reschedule.
//
// Responds 200 with the moved appointment, 409 if the destination is taken or
// the appointment is cancelled, and 422 if the destination breaks a booking
// rule. On any failure the appointment keeps its original slot.
func (s *Server) handleRescheduleAppointment(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "appointmentID")
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}

	body, err := decodeJSON[rescheduleAppointmentRequest](w, r, s.maxBodyBytes)
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}

	startsAt, err := parseTimestamp(body.StartsAt, "starts_at")
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}

	moved, err := s.appointments.Reschedule(r.Context(), appointment.RescheduleCommand{
		ID:       id,
		StartsAt: startsAt,
	})
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}
	writeJSON(w, r, http.StatusOK, toAppointmentResponse(moved), s.logger)
}

// handleDoctorAvailability implements
// GET /doctors/{doctorID}/availability?date=YYYY-MM-DD.
//
// The response is advisory: a listed slot can be taken by another patient
// before this one books it. Only POST /appointments is authoritative.
func (s *Server) handleDoctorAvailability(w http.ResponseWriter, r *http.Request) {
	doctorID, err := pathUUID(r, "doctorID")
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}
	date, err := queryDate(r, "date")
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}

	result, err := s.appointments.Availability(r.Context(), appointment.AvailabilityQuery{
		DoctorID: doctorID,
		Date:     date,
	})
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}

	loc, err := result.Doctor.Location()
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}

	policy := s.appointments.Policy()
	slots := make([]slotResponse, 0, len(result.Slots))
	for _, slot := range result.Slots {
		slots = append(slots, slotResponse{
			StartsAt:      slot.Start.UTC(),
			EndsAt:        slot.End.UTC(),
			StartsAtLocal: localRFC3339(slot.Start, loc),
		})
	}

	writeJSON(w, r, http.StatusOK, availabilityResponse{
		Doctor:              toDoctorSummary(result.Doctor),
		Date:                formatDate(result.Date),
		Timezone:            result.Doctor.Timezone,
		SlotDurationMinutes: int(policy.SlotDuration / time.Minute),
		MinLeadTimeMinutes:  int(policy.MinLeadTime / time.Minute),
		Slots:               slots,
	}, s.logger)
}

// handlePatientAppointments implements
// GET /patients/{patientID}/appointments.
//
// Returns future active appointments only, ordered by start time with the
// appointment ID as a tie-breaker so paging is stable.
func (s *Server) handlePatientAppointments(w http.ResponseWriter, r *http.Request) {
	patientID, err := pathUUID(r, "patientID")
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}

	limit, offset, err := queryPage(r, s.defaultPageSize, s.maxPageSize)
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}

	result, err := s.appointments.ListUpcomingForPatient(r.Context(), appointment.PatientAppointmentsQuery{
		PatientID: patientID,
		Page:      appointment.Page{Limit: limit, Offset: offset},
	})
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}

	items := make([]patientAppointmentResponse, 0, len(result.Items))
	for _, item := range result.Items {
		local := item.Appointment.StartsAt.UTC().Format(time.RFC3339)
		// A doctor whose timezone cannot be loaded must not fail the whole
		// listing; fall back to the UTC rendering and carry on.
		if loc, locErr := time.LoadLocation(item.Doctor.Timezone); locErr == nil {
			local = localRFC3339(item.Appointment.StartsAt, loc)
		}
		items = append(items, patientAppointmentResponse{
			appointmentResponse: toAppointmentResponse(item.Appointment),
			Doctor: doctorSummaryResponse{
				ID:        item.Doctor.ID.String(),
				Slug:      item.Doctor.Slug,
				FullName:  item.Doctor.FullName,
				Specialty: item.Doctor.Specialty,
				Timezone:  item.Doctor.Timezone,
			},
			StartsAtLocal: local,
		})
	}

	writeJSON(w, r, http.StatusOK, collection[patientAppointmentResponse]{
		Data:       items,
		Pagination: newPagination(result.Limit, result.Offset, result.Total),
	}, s.logger)
}
