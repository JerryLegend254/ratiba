-- name: CreateAppointment :one
-- Both ends of the interval are supplied by the caller rather than derived here.
-- The domain owns slot arithmetic, and appointments_duration_check verifies the
-- two agree, so the database still rejects a mismatched pair.
INSERT INTO appointments (doctor_id, patient_id, starts_at, ends_at, status)
VALUES ($1, $2, $3, $4, 'booked')
RETURNING id, doctor_id, patient_id, starts_at, ends_at, status,
          cancellation_reason, cancelled_at, created_at, updated_at;

-- name: GetAppointmentByID :one
SELECT id, doctor_id, patient_id, starts_at, ends_at, status,
       cancellation_reason, cancelled_at, created_at, updated_at
FROM appointments
WHERE id = $1;

-- name: GetAppointmentByIDForUpdate :one
-- Row lock taken at the start of every cancel/reschedule transaction. Because
-- both flows lock exactly one appointment row and never a second one, there is
-- no lock-ordering cycle and therefore no deadlock between them.
SELECT id, doctor_id, patient_id, starts_at, ends_at, status,
       cancellation_reason, cancelled_at, created_at, updated_at
FROM appointments
WHERE id = $1
FOR UPDATE;

-- name: CancelAppointment :one
UPDATE appointments
SET status = 'cancelled',
    cancellation_reason = $2,
    cancelled_at = $3
WHERE id = $1 AND status = 'booked'
RETURNING id, doctor_id, patient_id, starts_at, ends_at, status,
          cancellation_reason, cancelled_at, created_at, updated_at;

-- name: RescheduleAppointment :one
-- Moving the row is what releases the old slot and claims the new one: the
-- partial unique index sees a single UPDATE, so both halves are atomic.
UPDATE appointments
SET starts_at = $2,
    ends_at = $3
WHERE id = $1 AND status = 'booked'
RETURNING id, doctor_id, patient_id, starts_at, ends_at, status,
          cancellation_reason, cancelled_at, created_at, updated_at;

-- name: ListBookedStartsForDoctorInRange :many
-- Half-open [from, to) window, matching the interval convention used throughout.
SELECT starts_at
FROM appointments
WHERE doctor_id = $1
  AND status = 'booked'
  AND starts_at >= @range_start::timestamptz
  AND starts_at < @range_end::timestamptz
ORDER BY starts_at ASC;

-- name: ListUpcomingAppointmentsForPatient :many
-- "Upcoming" is defined against a caller-supplied instant so the query stays
-- deterministic under test; (starts_at, id) makes the ordering total.
SELECT a.id, a.doctor_id, a.patient_id, a.starts_at, a.ends_at, a.status,
       a.cancellation_reason, a.cancelled_at, a.created_at, a.updated_at,
       d.slug AS doctor_slug, d.full_name AS doctor_full_name,
       d.specialty AS doctor_specialty, d.timezone AS doctor_timezone
FROM appointments a
JOIN doctors d ON d.id = a.doctor_id
WHERE a.patient_id = $1
  AND a.status = 'booked'
  AND a.starts_at >= @from_time::timestamptz
ORDER BY a.starts_at ASC, a.id ASC
LIMIT $2 OFFSET $3;

-- name: CountUpcomingAppointmentsForPatient :one
SELECT count(*)
FROM appointments
WHERE patient_id = $1
  AND status = 'booked'
  AND starts_at >= @from_time::timestamptz;

-- name: InsertAppointmentEvent :exec
INSERT INTO appointment_events (appointment_id, event_type, from_starts_at, to_starts_at, source)
VALUES ($1, $2, $3, $4, $5);

-- name: ListAppointmentEvents :many
SELECT id, appointment_id, event_type, from_starts_at, to_starts_at, source, occurred_at
FROM appointment_events
WHERE appointment_id = $1
ORDER BY occurred_at ASC, id ASC;
