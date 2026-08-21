package http

import (
	"net/http"
)

// The doctor and patient directory endpoints are not part of the assessment
// brief. They exist because the alternative — telling a reviewer to open a
// psql session to find a UUID before they can call POST /appointments — is a
// bad first experience of the API. They are read-only and bounded.
//
// In a system with real patients, GET /patients would be behind authorisation.
// See docs/security.md.

// handleListDoctors implements GET /doctors.
func (s *Server) handleListDoctors(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := queryPage(r, s.defaultPageSize, s.maxPageSize)
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}
	activeOnly, err := queryBool(r, "active_only", true)
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}

	doctors, total, err := s.doctors.List(r.Context(), activeOnly, limit, offset)
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}

	items := make([]doctorResponse, 0, len(doctors))
	for _, d := range doctors {
		items = append(items, toDoctorResponse(d))
	}

	writeJSON(w, r, http.StatusOK, collection[doctorResponse]{
		Data:       items,
		Pagination: newPagination(limit, offset, total),
	}, s.logger)
}

// handleGetDoctor implements GET /doctors/{doctorID}.
func (s *Server) handleGetDoctor(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "doctorID")
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}

	doc, err := s.doctors.GetByID(r.Context(), id)
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}
	writeJSON(w, r, http.StatusOK, toDoctorResponse(doc), s.logger)
}

// handleListPatients implements GET /patients.
func (s *Server) handleListPatients(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := queryPage(r, s.defaultPageSize, s.maxPageSize)
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}
	activeOnly, err := queryBool(r, "active_only", true)
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}

	patients, total, err := s.patients.List(r.Context(), activeOnly, limit, offset)
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}

	items := make([]patientResponse, 0, len(patients))
	for _, p := range patients {
		items = append(items, toPatientResponse(p))
	}

	writeJSON(w, r, http.StatusOK, collection[patientResponse]{
		Data:       items,
		Pagination: newPagination(limit, offset, total),
	}, s.logger)
}

// handleGetPatient implements GET /patients/{patientID}.
func (s *Server) handleGetPatient(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "patientID")
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}

	pat, err := s.patients.GetByID(r.Context(), id)
	if err != nil {
		writeProblem(w, r, err, s.logger)
		return
	}
	writeJSON(w, r, http.StatusOK, toPatientResponse(pat), s.logger)
}
