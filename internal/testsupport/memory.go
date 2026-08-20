// Package testsupport provides in-memory doubles for Ratiba's persistence
// ports.
//
// These exist so that domain rules and HTTP contract behaviour can be tested in
// milliseconds without a database. What they deliberately do NOT prove is
// concurrency: a Go map with a mutex cannot demonstrate that PostgreSQL's
// partial unique index prevents a double booking across processes. That claim
// is only ever made by the integration suite in internal/postgres, which runs
// against a real server. See docs/testing.md.
//
// The store does model transaction rollback (by snapshotting state before the
// callback runs), because a use case that forgets to roll back should fail here
// too, not only in the slower suite.
package testsupport

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/JerryLegend254/ratiba/internal/appointment"
	"github.com/JerryLegend254/ratiba/internal/doctor"
	"github.com/JerryLegend254/ratiba/internal/patient"
)

// MemoryStore is an in-memory stand-in for the PostgreSQL adapter.
type MemoryStore struct {
	mu sync.Mutex

	doctors      map[uuid.UUID]doctor.Doctor
	workingHours map[uuid.UUID][]doctor.WorkingHours
	patients     map[uuid.UUID]patient.Patient
	appointments map[uuid.UUID]appointment.Appointment
	idempotency  map[string]appointment.IdempotencyRecord

	events []appointment.Event

	// FailWith, when set, makes the next write transaction fail with this
	// error. Used to prove that a failed commit leaves no partial state.
	FailWith error
	// PingErr, when set, makes Ping fail. Used for readiness tests.
	PingErr error
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		doctors:      map[uuid.UUID]doctor.Doctor{},
		workingHours: map[uuid.UUID][]doctor.WorkingHours{},
		patients:     map[uuid.UUID]patient.Patient{},
		appointments: map[uuid.UUID]appointment.Appointment{},
		idempotency:  map[string]appointment.IdempotencyRecord{},
	}
}

// AddDoctor registers a doctor and their weekly schedule.
func (s *MemoryStore) AddDoctor(doc doctor.Doctor, hours ...doctor.WorkingHours) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doctors[doc.ID] = doc
	s.workingHours[doc.ID] = hours
}

// AddPatient registers a patient.
func (s *MemoryStore) AddPatient(pat patient.Patient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.patients[pat.ID] = pat
}

// AddAppointment inserts an appointment directly, bypassing the service. Used
// to arrange state a test needs to already exist.
func (s *MemoryStore) AddAppointment(appt appointment.Appointment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appointments[appt.ID] = appt
}

// Appointment reads one appointment back for assertions.
func (s *MemoryStore) Appointment(id uuid.UUID) (appointment.Appointment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	appt, ok := s.appointments[id]
	return appt, ok
}

// ActiveCount reports how many active appointments hold a given doctor and
// start. It must never exceed one.
func (s *MemoryStore) ActiveCount(doctorID uuid.UUID, start time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, appt := range s.appointments {
		if appt.DoctorID == doctorID && appt.Status == appointment.StatusBooked && appt.StartsAt.Equal(start) {
			count++
		}
	}
	return count
}

// Events returns the recorded audit trail.
func (s *MemoryStore) Events() []appointment.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]appointment.Event(nil), s.events...)
}

// Ping implements the readiness check port.
func (s *MemoryStore) Ping(context.Context) error { return s.PingErr }

// Doctors returns the doctor repository view.
func (s *MemoryStore) Doctors() *MemoryDoctors { return &MemoryDoctors{store: s} }

// Patients returns the patient repository view.
func (s *MemoryStore) Patients() *MemoryPatients { return &MemoryPatients{store: s} }

// Appointments returns the appointment repository view.
func (s *MemoryStore) Appointments() *MemoryAppointments { return &MemoryAppointments{store: s} }

// snapshot copies the mutable state so a failed transaction can restore it.
func (s *MemoryStore) snapshot() (map[uuid.UUID]appointment.Appointment, map[string]appointment.IdempotencyRecord, []appointment.Event) {
	return maps.Clone(s.appointments), maps.Clone(s.idempotency), append([]appointment.Event(nil), s.events...)
}

// MemoryDoctors implements doctor.Repository.
type MemoryDoctors struct{ store *MemoryStore }

var _ doctor.Repository = (*MemoryDoctors)(nil)

// GetByID implements doctor.Repository.
func (d *MemoryDoctors) GetByID(_ context.Context, id uuid.UUID) (doctor.Doctor, error) {
	d.store.mu.Lock()
	defer d.store.mu.Unlock()
	doc, ok := d.store.doctors[id]
	if !ok {
		return doctor.Doctor{}, doctor.ErrNotFound()
	}
	return doc, nil
}

// GetBySlug implements doctor.Repository.
func (d *MemoryDoctors) GetBySlug(_ context.Context, slug string) (doctor.Doctor, error) {
	d.store.mu.Lock()
	defer d.store.mu.Unlock()
	for _, doc := range d.store.doctors {
		if doc.Slug == slug {
			return doc, nil
		}
	}
	return doctor.Doctor{}, doctor.ErrNotFound()
}

// ScheduleFor implements doctor.Repository.
func (d *MemoryDoctors) ScheduleFor(ctx context.Context, id uuid.UUID) (doctor.Schedule, error) {
	doc, err := d.GetByID(ctx, id)
	if err != nil {
		return doctor.Schedule{}, err
	}
	d.store.mu.Lock()
	defer d.store.mu.Unlock()
	return doctor.Schedule{Doctor: doc, WorkingHours: d.store.workingHours[id]}, nil
}

// List implements doctor.Repository.
func (d *MemoryDoctors) List(_ context.Context, activeOnly bool, limit, offset int32) ([]doctor.Doctor, int64, error) {
	d.store.mu.Lock()
	defer d.store.mu.Unlock()

	all := make([]doctor.Doctor, 0, len(d.store.doctors))
	for _, doc := range d.store.doctors {
		if activeOnly && !doc.IsActive {
			continue
		}
		all = append(all, doc)
	}
	sortByName(all, func(doc doctor.Doctor) (string, uuid.UUID) { return doc.FullName, doc.ID })
	page := paginate(all, limit, offset)
	return page, int64(len(all)), nil
}

// MemoryPatients implements patient.Repository.
type MemoryPatients struct{ store *MemoryStore }

var _ patient.Repository = (*MemoryPatients)(nil)

// GetByID implements patient.Repository.
func (p *MemoryPatients) GetByID(_ context.Context, id uuid.UUID) (patient.Patient, error) {
	p.store.mu.Lock()
	defer p.store.mu.Unlock()
	pat, ok := p.store.patients[id]
	if !ok {
		return patient.Patient{}, patient.ErrNotFound()
	}
	return pat, nil
}

// List implements patient.Repository.
func (p *MemoryPatients) List(_ context.Context, activeOnly bool, limit, offset int32) ([]patient.Patient, int64, error) {
	p.store.mu.Lock()
	defer p.store.mu.Unlock()

	all := make([]patient.Patient, 0, len(p.store.patients))
	for _, pat := range p.store.patients {
		if activeOnly && !pat.IsActive {
			continue
		}
		all = append(all, pat)
	}
	sortByName(all, func(pat patient.Patient) (string, uuid.UUID) { return pat.FullName, pat.ID })
	page := paginate(all, limit, offset)
	return page, int64(len(all)), nil
}

// MemoryAppointments implements appointment.Repository.
type MemoryAppointments struct{ store *MemoryStore }

var _ appointment.Repository = (*MemoryAppointments)(nil)

// WithinTx runs fn against the store, restoring the pre-call state if fn fails.
//
// The lock is held for the whole callback, which serialises writers exactly the
// way the row locks do in the real adapter.
func (a *MemoryAppointments) WithinTx(ctx context.Context, fn func(context.Context, appointment.Tx) error) error {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()

	appointments, idempotency, events := a.store.snapshot()

	if err := fn(ctx, &memoryTx{store: a.store}); err != nil {
		a.store.appointments, a.store.idempotency, a.store.events = appointments, idempotency, events
		return err
	}
	if a.store.FailWith != nil {
		err := a.store.FailWith
		a.store.FailWith = nil
		a.store.appointments, a.store.idempotency, a.store.events = appointments, idempotency, events
		return err
	}
	return nil
}

// Get implements appointment.Repository.
func (a *MemoryAppointments) Get(_ context.Context, id uuid.UUID) (appointment.Appointment, error) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	appt, ok := a.store.appointments[id]
	if !ok {
		return appointment.Appointment{}, appointment.ErrNotFound
	}
	return appt, nil
}

// ListBookedStarts implements appointment.Repository.
func (a *MemoryAppointments) ListBookedStarts(_ context.Context, doctorID uuid.UUID, from, to time.Time) ([]time.Time, error) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()

	var starts []time.Time
	for _, appt := range a.store.appointments {
		if appt.DoctorID != doctorID || appt.Status != appointment.StatusBooked {
			continue
		}
		if appt.StartsAt.Before(from) || !appt.StartsAt.Before(to) {
			continue
		}
		starts = append(starts, appt.StartsAt)
	}
	return starts, nil
}

// ListUpcomingForPatient implements appointment.Repository.
func (a *MemoryAppointments) ListUpcomingForPatient(
	_ context.Context, patientID uuid.UUID, from time.Time, page appointment.Page,
) ([]appointment.PatientAppointment, int64, error) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()

	var matching []appointment.PatientAppointment
	for _, appt := range a.store.appointments {
		if appt.PatientID != patientID || appt.Status != appointment.StatusBooked || appt.StartsAt.Before(from) {
			continue
		}
		doc := a.store.doctors[appt.DoctorID]
		matching = append(matching, appointment.PatientAppointment{
			Appointment: appt,
			Doctor: appointment.DoctorSummary{
				ID: doc.ID, Slug: doc.Slug, FullName: doc.FullName,
				Specialty: doc.Specialty, Timezone: doc.Timezone,
			},
		})
	}

	// (starts_at, id) ordering, matching the SQL.
	sortSlice(matching, func(x, y appointment.PatientAppointment) bool {
		if !x.Appointment.StartsAt.Equal(y.Appointment.StartsAt) {
			return x.Appointment.StartsAt.Before(y.Appointment.StartsAt)
		}
		return x.Appointment.ID.String() < y.Appointment.ID.String()
	})

	total := int64(len(matching))
	return paginate(matching, page.Limit, page.Offset), total, nil
}

// FindIdempotencyRecord implements appointment.Repository.
func (a *MemoryAppointments) FindIdempotencyRecord(
	_ context.Context, patientID uuid.UUID, key string,
) (appointment.IdempotencyRecord, bool, error) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	record, ok := a.store.idempotency[idempotencyKey(patientID, key)]
	return record, ok, nil
}

// memoryTx implements appointment.Tx. The store's lock is already held by
// WithinTx, so these methods must not take it again.
type memoryTx struct{ store *MemoryStore }

var _ appointment.Tx = (*memoryTx)(nil)

// LockAppointment implements appointment.Tx.
func (t *memoryTx) LockAppointment(_ context.Context, id uuid.UUID) (appointment.Appointment, error) {
	appt, ok := t.store.appointments[id]
	if !ok {
		return appointment.Appointment{}, appointment.ErrNotFound
	}
	return appt, nil
}

// Create implements appointment.Tx, reproducing the partial unique index.
func (t *memoryTx) Create(_ context.Context, doctorID, patientID uuid.UUID, slot appointment.Slot) (appointment.Appointment, error) {
	for _, existing := range t.store.appointments {
		if existing.DoctorID == doctorID &&
			existing.Status == appointment.StatusBooked &&
			existing.StartsAt.Equal(slot.Start) {
			return appointment.Appointment{}, appointment.ErrSlotTaken
		}
	}

	now := time.Now().UTC()
	appt := appointment.Appointment{
		ID: uuid.New(), DoctorID: doctorID, PatientID: patientID,
		StartsAt: slot.Start, EndsAt: slot.End,
		Status: appointment.StatusBooked, CreatedAt: now, UpdatedAt: now,
	}
	t.store.appointments[appt.ID] = appt
	return appt, nil
}

// Cancel implements appointment.Tx.
func (t *memoryTx) Cancel(_ context.Context, id uuid.UUID, reason string, at time.Time) (appointment.Appointment, error) {
	appt, ok := t.store.appointments[id]
	if !ok {
		return appointment.Appointment{}, appointment.ErrNotFound
	}
	if appt.Status != appointment.StatusBooked {
		return appointment.Appointment{}, appointment.ErrNotActive
	}
	appt.Status = appointment.StatusCancelled
	appt.CancellationReason = &reason
	appt.CancelledAt = &at
	appt.UpdatedAt = at
	t.store.appointments[id] = appt
	return appt, nil
}

// Move implements appointment.Tx, reproducing the partial unique index.
func (t *memoryTx) Move(_ context.Context, id uuid.UUID, slot appointment.Slot) (appointment.Appointment, error) {
	appt, ok := t.store.appointments[id]
	if !ok {
		return appointment.Appointment{}, appointment.ErrNotFound
	}
	if appt.Status != appointment.StatusBooked {
		return appointment.Appointment{}, appointment.ErrNotActive
	}
	for otherID, other := range t.store.appointments {
		if otherID == id {
			continue
		}
		if other.DoctorID == appt.DoctorID &&
			other.Status == appointment.StatusBooked &&
			other.StartsAt.Equal(slot.Start) {
			return appointment.Appointment{}, appointment.ErrSlotTaken
		}
	}
	appt.StartsAt = slot.Start
	appt.EndsAt = slot.End
	appt.UpdatedAt = time.Now().UTC()
	t.store.appointments[id] = appt
	return appt, nil
}

// AppendEvent implements appointment.Tx.
func (t *memoryTx) AppendEvent(_ context.Context, event appointment.Event) error {
	t.store.events = append(t.store.events, event)
	return nil
}

// SaveIdempotencyRecord implements appointment.Tx.
func (t *memoryTx) SaveIdempotencyRecord(_ context.Context, record appointment.IdempotencyRecord) error {
	key := idempotencyKey(record.PatientID, record.Key)
	if _, exists := t.store.idempotency[key]; exists {
		return appointment.ErrIdempotencyKeyExists
	}
	t.store.idempotency[key] = record
	return nil
}

func idempotencyKey(patientID uuid.UUID, key string) string {
	return fmt.Sprintf("%s|%s", patientID, key)
}
