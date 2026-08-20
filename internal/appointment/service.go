package appointment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/JerryLegend254/ratiba/internal/doctor"
	"github.com/JerryLegend254/ratiba/internal/patient"
	"github.com/JerryLegend254/ratiba/internal/platform/apperror"
	"github.com/JerryLegend254/ratiba/internal/platform/calendar"
	"github.com/JerryLegend254/ratiba/internal/platform/clock"
)

// sourceAPI marks history entries created by an inbound HTTP request. It is
// provenance, not identity: Ratiba has no authentication yet.
const sourceAPI = "api"

// Idempotency-Key bounds. The lower bound rejects keys too short to be
// plausibly unique; both are also enforced by CHECK constraints.
const (
	MinIdempotencyKeyLength = 8
	MaxIdempotencyKeyLength = 255
)

// MaxCancellationReasonLength bounds the free-text reason. The same limit is
// enforced by a CHECK constraint, so a client cannot bypass it by any route.
const MaxCancellationReasonLength = 500

// Metrics receives coarse, bounded-cardinality domain counters.
//
// The domain must not import Prometheus, so it depends on this two-string
// interface instead. Both arguments come from a small fixed vocabulary; neither
// ever carries an identifier, so metric cardinality stays bounded.
type Metrics interface {
	// RecordOutcome counts one attempt at a domain operation.
	RecordOutcome(operation, outcome string)
}

// Operation and outcome vocabularies used with Metrics.
const (
	OperationBook       = "book"
	OperationCancel     = "cancel"
	OperationReschedule = "reschedule"

	OutcomeSucceeded = "succeeded"
	OutcomeReplayed  = "replayed"
	OutcomeConflict  = "conflict"
	OutcomeRejected  = "rejected"
	OutcomeFailed    = "failed"
)

// NopMetrics discards domain counters. Used in tests and wherever metrics are
// not wired up.
type NopMetrics struct{}

// RecordOutcome implements Metrics.
func (NopMetrics) RecordOutcome(string, string) {}

// ServiceConfig carries the tunable parts of the service.
type ServiceConfig struct {
	// Policy is the booking rule set.
	Policy Policy
	// IdempotencyTTL is how long a stored booking response stays replayable.
	IdempotencyTTL time.Duration
	// DefaultPageSize applies when a client does not ask for a page size.
	DefaultPageSize int32
	// MaxPageSize bounds any single response.
	MaxPageSize int32
}

// Service implements Ratiba's booking use cases.
//
// Invariant worth knowing before changing anything here: no method calls the
// doctor or patient readers, or any Repository read method, from inside a
// WithinTx callback. Every read happens before the transaction opens. Nesting a
// pool acquisition inside a held transaction is how a connection pool
// deadlocks itself under load, and keeping that rule makes it impossible.
type Service struct {
	repo     Repository
	doctors  ScheduleReader
	patients PatientReader
	clock    clock.Clock
	logger   *slog.Logger
	metrics  Metrics
	cfg      ServiceConfig
}

// NewService wires a Service. It fails closed on a configuration that would
// produce incorrect behaviour rather than silently substituting defaults.
func NewService(
	repo Repository,
	doctors ScheduleReader,
	patients PatientReader,
	clk clock.Clock,
	logger *slog.Logger,
	metrics Metrics,
	cfg ServiceConfig,
) (*Service, error) {
	switch {
	case repo == nil:
		return nil, errors.New("appointment: repository is required")
	case doctors == nil:
		return nil, errors.New("appointment: doctor reader is required")
	case patients == nil:
		return nil, errors.New("appointment: patient reader is required")
	case clk == nil:
		return nil, errors.New("appointment: clock is required")
	case logger == nil:
		return nil, errors.New("appointment: logger is required")
	}
	if metrics == nil {
		metrics = NopMetrics{}
	}
	if _, err := NewPolicy(cfg.Policy.SlotDuration, cfg.Policy.MinLeadTime); err != nil {
		return nil, fmt.Errorf("appointment: invalid policy: %w", err)
	}
	if cfg.IdempotencyTTL <= 0 {
		return nil, errors.New("appointment: idempotency TTL must be positive")
	}
	if cfg.DefaultPageSize <= 0 || cfg.MaxPageSize <= 0 || cfg.DefaultPageSize > cfg.MaxPageSize {
		return nil, errors.New("appointment: page sizes must be positive with default <= max")
	}
	return &Service{
		repo: repo, doctors: doctors, patients: patients,
		clock: clk, logger: logger, metrics: metrics, cfg: cfg,
	}, nil
}

// Policy exposes the active booking rules, for documentation endpoints and
// tests.
func (s *Service) Policy() Policy { return s.cfg.Policy }

// BookCommand requests a new appointment.
type BookCommand struct {
	DoctorID  uuid.UUID
	PatientID uuid.UUID
	// StartsAt is the requested slot start as an absolute instant.
	StartsAt time.Time
	// IdempotencyKey is optional. When present, a retry with the same key and
	// the same payload returns the original result instead of booking twice.
	IdempotencyKey string
}

// fingerprint hashes the meaningful content of the command.
//
// It is computed from the parsed fields rather than the raw request bytes, so
// two byte-different but semantically identical retries (different key order,
// different whitespace, "+00:00" vs "Z") are correctly recognised as the same
// request. The "v1" prefix leaves room to change this scheme later without
// silently reinterpreting stored fingerprints.
func (c BookCommand) fingerprint() string {
	sum := sha256.Sum256(fmt.Appendf(nil,
		"v1|%s|%s|%s",
		c.DoctorID, c.PatientID, c.StartsAt.UTC().Format(time.RFC3339Nano),
	))
	return hex.EncodeToString(sum[:])
}

// BookResult is the outcome of a booking attempt.
type BookResult struct {
	Appointment Appointment
	// Replayed reports that this response was served from a stored
	// Idempotency-Key record rather than newly created.
	Replayed bool
}

// Book creates an appointment.
//
// The database's partial unique index is the sole authority on whether the slot
// was free. Everything before the INSERT is there to produce a good error
// message quickly; only the INSERT decides the race.
func (s *Service) Book(ctx context.Context, cmd BookCommand) (BookResult, error) {
	if cmd.DoctorID == uuid.Nil || cmd.PatientID == uuid.Nil {
		return BookResult{}, apperror.New(apperror.KindUnprocessable, apperror.CodeValidationFailed,
			"A doctor and a patient are required.")
	}
	if cmd.StartsAt.IsZero() {
		return BookResult{}, apperror.New(apperror.KindUnprocessable, apperror.CodeValidationFailed,
			"A start time is required.")
	}
	if cmd.IdempotencyKey != "" {
		if err := validateIdempotencyKey(cmd.IdempotencyKey); err != nil {
			return BookResult{}, err
		}
	}

	fingerprint := cmd.fingerprint()

	// Fast path: a key we have already answered.
	if cmd.IdempotencyKey != "" {
		record, found, err := s.repo.FindIdempotencyRecord(ctx, cmd.PatientID, cmd.IdempotencyKey)
		if err != nil {
			return BookResult{}, apperror.Internal(fmt.Errorf("look up idempotency record: %w", err))
		}
		if found {
			return s.replay(record, fingerprint)
		}
	}

	doc, err := s.loadBookableDoctor(ctx, cmd.DoctorID)
	if err != nil {
		s.metrics.RecordOutcome(OperationBook, OutcomeRejected)
		return BookResult{}, err
	}
	if err := s.assertBookablePatient(ctx, cmd.PatientID); err != nil {
		s.metrics.RecordOutcome(OperationBook, OutcomeRejected)
		return BookResult{}, err
	}

	schedule, loc, err := s.loadSchedule(ctx, doc)
	if err != nil {
		return BookResult{}, err
	}

	now := s.clock.Now()
	if err := s.cfg.Policy.ValidateStart(schedule, loc, now, cmd.StartsAt); err != nil {
		s.metrics.RecordOutcome(OperationBook, OutcomeRejected)
		return BookResult{}, err
	}

	var created Appointment
	txErr := s.repo.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		appt, err := tx.Create(ctx, cmd.DoctorID, cmd.PatientID, s.cfg.Policy.SlotAt(cmd.StartsAt))
		if err != nil {
			return err
		}
		if err := tx.AppendEvent(ctx, Event{
			AppointmentID: appt.ID,
			Type:          EventBooked,
			ToStartsAt:    &appt.StartsAt,
			Source:        sourceAPI,
		}); err != nil {
			return err
		}
		if cmd.IdempotencyKey != "" {
			snapshot, err := encodeSnapshot(appt)
			if err != nil {
				return err
			}
			if err := tx.SaveIdempotencyRecord(ctx, IdempotencyRecord{
				PatientID:      cmd.PatientID,
				Key:            cmd.IdempotencyKey,
				Fingerprint:    fingerprint,
				AppointmentID:  appt.ID,
				ResponseStatus: 201,
				Snapshot:       snapshot,
				ExpiresAt:      now.Add(s.cfg.IdempotencyTTL),
			}); err != nil {
				return err
			}
		}
		created = appt
		return nil
	})

	switch {
	case txErr == nil:
		s.metrics.RecordOutcome(OperationBook, OutcomeSucceeded)
		s.logger.InfoContext(ctx, "appointment booked",
			slog.String("event", "appointment.booked"),
			slog.String("appointment_id", created.ID.String()),
			slog.String("doctor_id", created.DoctorID.String()),
			slog.Time("starts_at", created.StartsAt),
		)
		return BookResult{Appointment: created}, nil

	case errors.Is(txErr, ErrSlotTaken):
		s.metrics.RecordOutcome(OperationBook, OutcomeConflict)
		s.logger.InfoContext(ctx, "booking rejected: slot taken",
			slog.String("event", "appointment.slot_conflict"),
			slog.String("doctor_id", cmd.DoctorID.String()),
			slog.Time("starts_at", cmd.StartsAt),
		)
		return BookResult{}, ErrSlotUnavailable()

	case errors.Is(txErr, ErrIdempotencyKeyExists):
		// A concurrent request with the same key committed first. Its
		// transaction included the stored response, so the record is now
		// visible to a fresh read and the retry can be answered from it.
		record, found, err := s.repo.FindIdempotencyRecord(ctx, cmd.PatientID, cmd.IdempotencyKey)
		if err != nil || !found {
			return BookResult{}, apperror.Internal(fmt.Errorf("resolve concurrent idempotent booking: %w", txErr))
		}
		return s.replay(record, fingerprint)

	default:
		s.metrics.RecordOutcome(OperationBook, OutcomeFailed)
		if appErr, ok := apperror.From(txErr); ok {
			return BookResult{}, appErr
		}
		return BookResult{}, apperror.Internal(fmt.Errorf("book appointment: %w", txErr))
	}
}

// replay answers a booking retry from a stored response.
func (s *Service) replay(record IdempotencyRecord, fingerprint string) (BookResult, error) {
	if record.Fingerprint != fingerprint {
		s.metrics.RecordOutcome(OperationBook, OutcomeConflict)
		return BookResult{}, ErrIdempotencyKeyReuse()
	}
	appt, err := decodeSnapshot(record.Snapshot)
	if err != nil {
		return BookResult{}, apperror.Internal(fmt.Errorf("decode idempotency snapshot: %w", err))
	}
	s.metrics.RecordOutcome(OperationBook, OutcomeReplayed)
	return BookResult{Appointment: appt, Replayed: true}, nil
}

// CancelCommand cancels an appointment.
type CancelCommand struct {
	ID     uuid.UUID
	Reason string
}

// Cancel releases a booked appointment's slot.
//
// The row is locked for the duration of the transaction, so two concurrent
// cancellations are serialised and the second one sees the cancelled state.
func (s *Service) Cancel(ctx context.Context, cmd CancelCommand) (Appointment, error) {
	reason, err := normaliseCancellationReason(cmd.Reason)
	if err != nil {
		s.metrics.RecordOutcome(OperationCancel, OutcomeRejected)
		return Appointment{}, err
	}

	now := s.clock.Now()
	var cancelled Appointment

	txErr := s.repo.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		appt, err := tx.LockAppointment(ctx, cmd.ID)
		if err != nil {
			return err
		}
		if !appt.IsActive() {
			return ErrAlreadyCancelled()
		}
		updated, err := tx.Cancel(ctx, cmd.ID, reason, now)
		if err != nil {
			return err
		}
		if err := tx.AppendEvent(ctx, Event{
			AppointmentID: updated.ID,
			Type:          EventCancelled,
			FromStartsAt:  &updated.StartsAt,
			Source:        sourceAPI,
		}); err != nil {
			return err
		}
		cancelled = updated
		return nil
	})

	if txErr != nil {
		return Appointment{}, s.mapWriteError(ctx, OperationCancel, txErr)
	}

	s.metrics.RecordOutcome(OperationCancel, OutcomeSucceeded)
	// The reason is intentionally absent from this log line: it is free text
	// entered by a patient and must be treated as clinical data.
	s.logger.InfoContext(ctx, "appointment cancelled",
		slog.String("event", "appointment.cancelled"),
		slog.String("appointment_id", cancelled.ID.String()),
		slog.String("doctor_id", cancelled.DoctorID.String()),
		slog.Time("starts_at", cancelled.StartsAt),
	)
	return cancelled, nil
}

// RescheduleCommand moves an appointment to a new slot.
type RescheduleCommand struct {
	ID       uuid.UUID
	StartsAt time.Time
}

// Reschedule atomically moves an active appointment.
//
// The destination is validated with exactly the policy a fresh booking uses.
// The move itself is a single UPDATE inside the transaction that holds the row
// lock, so the old slot is released and the new one claimed together: if the
// destination turns out to be taken, the whole transaction rolls back and the
// appointment stays exactly where it was.
func (s *Service) Reschedule(ctx context.Context, cmd RescheduleCommand) (Appointment, error) {
	if cmd.StartsAt.IsZero() {
		return Appointment{}, apperror.New(apperror.KindUnprocessable, apperror.CodeValidationFailed,
			"A new start time is required.")
	}

	// Read the appointment before opening the transaction so the doctor and
	// schedule lookups it implies happen outside any held lock. The appointment
	// state read here is advisory; it is re-read under lock below.
	existing, err := s.repo.Get(ctx, cmd.ID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Appointment{}, ErrAppointmentNotFound()
		}
		return Appointment{}, apperror.Internal(fmt.Errorf("load appointment: %w", err))
	}

	doc, err := s.loadBookableDoctor(ctx, existing.DoctorID)
	if err != nil {
		s.metrics.RecordOutcome(OperationReschedule, OutcomeRejected)
		return Appointment{}, err
	}
	schedule, loc, err := s.loadSchedule(ctx, doc)
	if err != nil {
		return Appointment{}, err
	}

	now := s.clock.Now()
	if err := s.cfg.Policy.ValidateStart(schedule, loc, now, cmd.StartsAt); err != nil {
		s.metrics.RecordOutcome(OperationReschedule, OutcomeRejected)
		return Appointment{}, err
	}

	var moved Appointment
	txErr := s.repo.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		appt, err := tx.LockAppointment(ctx, cmd.ID)
		if err != nil {
			return err
		}
		if !appt.IsActive() {
			return ErrAlreadyCancelled()
		}
		if appt.StartsAt.Equal(cmd.StartsAt) {
			return ErrRescheduleSameSlot()
		}
		previousStart := appt.StartsAt

		updated, err := tx.Move(ctx, cmd.ID, s.cfg.Policy.SlotAt(cmd.StartsAt))
		if err != nil {
			return err
		}
		if err := tx.AppendEvent(ctx, Event{
			AppointmentID: updated.ID,
			Type:          EventRescheduled,
			FromStartsAt:  &previousStart,
			ToStartsAt:    &updated.StartsAt,
			Source:        sourceAPI,
		}); err != nil {
			return err
		}
		moved = updated
		return nil
	})

	if txErr != nil {
		return Appointment{}, s.mapWriteError(ctx, OperationReschedule, txErr)
	}

	s.metrics.RecordOutcome(OperationReschedule, OutcomeSucceeded)
	s.logger.InfoContext(ctx, "appointment rescheduled",
		slog.String("event", "appointment.rescheduled"),
		slog.String("appointment_id", moved.ID.String()),
		slog.String("doctor_id", moved.DoctorID.String()),
		slog.Time("starts_at", moved.StartsAt),
	)
	return moved, nil
}

// mapWriteError turns a failed write transaction into the right client-facing
// error, counting the outcome on the way past.
func (s *Service) mapWriteError(ctx context.Context, operation string, err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		s.metrics.RecordOutcome(operation, OutcomeRejected)
		return ErrAppointmentNotFound()

	case errors.Is(err, ErrSlotTaken):
		s.metrics.RecordOutcome(operation, OutcomeConflict)
		s.logger.InfoContext(ctx, "write rejected: slot taken",
			slog.String("event", "appointment.slot_conflict"),
			slog.String("operation", operation),
		)
		return ErrSlotUnavailable()

	case errors.Is(err, ErrNotActive):
		// The row changed state between the lock and the statement, which
		// should be impossible while the lock is held. Report it as the same
		// conflict a caller would otherwise have seen.
		s.metrics.RecordOutcome(operation, OutcomeConflict)
		return ErrAlreadyCancelled()
	}

	if appErr, ok := apperror.From(err); ok {
		switch appErr.Kind {
		case apperror.KindConflict:
			s.metrics.RecordOutcome(operation, OutcomeConflict)
		case apperror.KindInternal:
			s.metrics.RecordOutcome(operation, OutcomeFailed)
		default:
			s.metrics.RecordOutcome(operation, OutcomeRejected)
		}
		return appErr
	}

	s.metrics.RecordOutcome(operation, OutcomeFailed)
	return apperror.Internal(fmt.Errorf("%s appointment: %w", operation, err))
}

// AvailabilityQuery asks for a doctor's free slots on one local calendar date.
type AvailabilityQuery struct {
	DoctorID uuid.UUID
	Date     calendar.Date
}

// AvailabilityResult is a doctor's free slots on a date, with the timezone the
// date was interpreted in.
type AvailabilityResult struct {
	Doctor doctor.Doctor
	Date   calendar.Date
	Slots  []Slot
}

// Availability returns every slot a patient could book right now.
//
// The result is advisory. A slot listed here can be taken by someone else a
// millisecond later; only POST /appointments is authoritative.
func (s *Service) Availability(ctx context.Context, q AvailabilityQuery) (AvailabilityResult, error) {
	doc, err := s.loadBookableDoctor(ctx, q.DoctorID)
	if err != nil {
		return AvailabilityResult{}, err
	}
	schedule, loc, err := s.loadSchedule(ctx, doc)
	if err != nil {
		return AvailabilityResult{}, err
	}

	var booked []time.Time
	if from, to, ok := s.cfg.Policy.SearchRange(schedule, q.Date, loc); ok {
		booked, err = s.repo.ListBookedStarts(ctx, q.DoctorID, from, to)
		if err != nil {
			return AvailabilityResult{}, apperror.Internal(fmt.Errorf("list booked starts: %w", err))
		}
	}

	slots := s.cfg.Policy.FreeSlotsOn(schedule, q.Date, loc, s.clock.Now(), booked)
	return AvailabilityResult{Doctor: doc, Date: q.Date, Slots: slots}, nil
}

// PatientAppointmentsQuery asks for a patient's upcoming appointments.
type PatientAppointmentsQuery struct {
	PatientID uuid.UUID
	Page      Page
}

// PatientAppointmentsResult is one bounded page of upcoming appointments.
type PatientAppointmentsResult struct {
	Items  []PatientAppointment
	Total  int64
	Limit  int32
	Offset int32
}

// ListUpcomingForPatient returns future active appointments in chronological
// order.
//
// Cancelled and past appointments are excluded: this endpoint answers "what do
// I still need to show up for?". History is deliberately not exposed here.
// Listing works for deactivated patients, since reading a record is harmless
// even when booking against it is not.
func (s *Service) ListUpcomingForPatient(ctx context.Context, q PatientAppointmentsQuery) (PatientAppointmentsResult, error) {
	if _, err := s.patients.GetByID(ctx, q.PatientID); err != nil {
		return PatientAppointmentsResult{}, err
	}

	page := s.normalisePage(q.Page)
	items, total, err := s.repo.ListUpcomingForPatient(ctx, q.PatientID, s.clock.Now(), page)
	if err != nil {
		return PatientAppointmentsResult{}, apperror.Internal(fmt.Errorf("list patient appointments: %w", err))
	}
	return PatientAppointmentsResult{Items: items, Total: total, Limit: page.Limit, Offset: page.Offset}, nil
}

// Get returns a single appointment regardless of status.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Appointment, error) {
	appt, err := s.repo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Appointment{}, ErrAppointmentNotFound()
		}
		return Appointment{}, apperror.Internal(fmt.Errorf("get appointment: %w", err))
	}
	return appt, nil
}

// loadBookableDoctor fetches a doctor and rejects one that is not accepting
// appointments. Availability, booking and rescheduling all use it, so an
// inactive doctor produces the same answer everywhere.
func (s *Service) loadBookableDoctor(ctx context.Context, id uuid.UUID) (doctor.Doctor, error) {
	doc, err := s.doctors.GetByID(ctx, id)
	if err != nil {
		return doctor.Doctor{}, err
	}
	if !doc.IsActive {
		return doctor.Doctor{}, doctor.ErrInactive()
	}
	return doc, nil
}

func (s *Service) assertBookablePatient(ctx context.Context, id uuid.UUID) error {
	pat, err := s.patients.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if !pat.IsActive {
		return patient.ErrInactive()
	}
	return nil
}

func (s *Service) loadSchedule(ctx context.Context, doc doctor.Doctor) (doctor.Schedule, *time.Location, error) {
	loc, err := doc.Location()
	if err != nil {
		return doctor.Schedule{}, nil, err
	}
	schedule, err := s.doctors.ScheduleFor(ctx, doc.ID)
	if err != nil {
		return doctor.Schedule{}, nil, err
	}
	return schedule, loc, nil
}

func (s *Service) normalisePage(p Page) Page {
	if p.Limit <= 0 {
		p.Limit = s.cfg.DefaultPageSize
	}
	if p.Limit > s.cfg.MaxPageSize {
		p.Limit = s.cfg.MaxPageSize
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	return p
}

// normaliseCancellationReason trims and bounds the reason.
//
// A reason is mandatory: "why was this cancelled?" is the first question asked
// when a patient calls the clinic back, and an empty string is not an answer.
func normaliseCancellationReason(raw string) (string, error) {
	reason := strings.TrimSpace(raw)
	if reason == "" {
		return "", apperror.New(apperror.KindUnprocessable, apperror.CodeValidationFailed,
			"A cancellation reason is required.").
			WithViolations(apperror.FieldViolation{
				Field:   "reason",
				Code:    "required",
				Message: "Provide a non-empty cancellation reason.",
			})
	}
	if utf8.RuneCountInString(reason) > MaxCancellationReasonLength {
		return "", apperror.Newf(apperror.KindUnprocessable, apperror.CodeValidationFailed,
			"The cancellation reason must be at most %d characters.", MaxCancellationReasonLength).
			WithViolations(apperror.FieldViolation{
				Field:   "reason",
				Code:    "too_long",
				Message: fmt.Sprintf("At most %d characters.", MaxCancellationReasonLength),
			})
	}
	return reason, nil
}

// validateIdempotencyKey bounds the header value. Keys are echoed into no logs
// and stored verbatim, so the only requirements are length and printability.
func validateIdempotencyKey(key string) error {
	length := utf8.RuneCountInString(key)
	if length < MinIdempotencyKeyLength || length > MaxIdempotencyKeyLength {
		return apperror.Newf(apperror.KindInvalidInput, apperror.CodeInvalidIdempotencyKey,
			"Idempotency-Key must be between %d and %d characters.",
			MinIdempotencyKeyLength, MaxIdempotencyKeyLength)
	}
	for _, r := range key {
		if r < 0x21 || r > 0x7e {
			return apperror.New(apperror.KindInvalidInput, apperror.CodeInvalidIdempotencyKey,
				"Idempotency-Key must contain only printable ASCII characters.")
		}
	}
	return nil
}

// appointmentSnapshot is the stored form of an idempotent booking response.
//
// It has explicit field names so a future rename of an Appointment field cannot
// silently invalidate records already in the database.
type appointmentSnapshot struct {
	Version   int       `json:"v"`
	ID        uuid.UUID `json:"id"`
	DoctorID  uuid.UUID `json:"doctor_id"`
	PatientID uuid.UUID `json:"patient_id"`
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func encodeSnapshot(a Appointment) ([]byte, error) {
	data, err := json.Marshal(appointmentSnapshot{
		Version: 1, ID: a.ID, DoctorID: a.DoctorID, PatientID: a.PatientID,
		StartsAt: a.StartsAt, EndsAt: a.EndsAt, Status: string(a.Status),
		CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("encode appointment snapshot: %w", err)
	}
	return data, nil
}

func decodeSnapshot(data []byte) (Appointment, error) {
	var snap appointmentSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return Appointment{}, fmt.Errorf("decode appointment snapshot: %w", err)
	}
	if snap.Version != 1 {
		return Appointment{}, fmt.Errorf("unsupported appointment snapshot version %d", snap.Version)
	}
	return Appointment{
		ID: snap.ID, DoctorID: snap.DoctorID, PatientID: snap.PatientID,
		StartsAt: snap.StartsAt, EndsAt: snap.EndsAt, Status: Status(snap.Status),
		CreatedAt: snap.CreatedAt, UpdatedAt: snap.UpdatedAt,
	}, nil
}
