-- +goose Up
-- +goose StatementBegin
CREATE TYPE appointment_status AS ENUM ('booked', 'cancelled');
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TYPE appointment_event_type AS ENUM ('booked', 'cancelled', 'rescheduled');
-- +goose StatementEnd

-- +goose StatementBegin
-- Appointments occupy a half-open [starts_at, ends_at) interval stored as UTC
-- instants. Cancelled rows are retained for auditability and are never deleted.
CREATE TABLE appointments (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    doctor_id           uuid               NOT NULL REFERENCES doctors (id) ON DELETE RESTRICT,
    patient_id          uuid               NOT NULL REFERENCES patients (id) ON DELETE RESTRICT,
    starts_at           timestamptz        NOT NULL,
    ends_at             timestamptz        NOT NULL,
    status              appointment_status NOT NULL DEFAULT 'booked',
    cancellation_reason text,
    cancelled_at        timestamptz,
    created_at          timestamptz        NOT NULL DEFAULT now(),
    updated_at          timestamptz        NOT NULL DEFAULT now(),

    CONSTRAINT appointments_duration_check CHECK (ends_at = starts_at + interval '30 minutes'),
    -- Sub-minute precision would let two "identical" slots differ by seconds and
    -- slip past the uniqueness rule below.
    CONSTRAINT appointments_whole_minute_check CHECK (EXTRACT(SECOND FROM starts_at) = 0),
    -- A cancelled row always carries both a reason and a timestamp; an active
    -- row carries neither. This keeps the audit trail self-consistent.
    CONSTRAINT appointments_cancellation_consistency_check CHECK (
        (status = 'cancelled' AND cancelled_at IS NOT NULL AND cancellation_reason IS NOT NULL)
        OR (status = 'booked' AND cancelled_at IS NULL AND cancellation_reason IS NULL)
    ),
    CONSTRAINT appointments_cancellation_reason_length_check CHECK (
        cancellation_reason IS NULL OR char_length(btrim(cancellation_reason)) BETWEEN 1 AND 500
    )
);
-- +goose StatementEnd

-- +goose StatementBegin
-- THE concurrency invariant. Only rows in the 'booked' state participate, so a
-- cancellation frees the slot the instant its transaction commits, and two
-- concurrent bookings for the same doctor+start can never both commit. Every
-- booking path treats a violation of this index -- not an earlier availability
-- read -- as the final authority on whether a slot was taken.
CREATE UNIQUE INDEX appointments_active_slot_uniq
    ON appointments (doctor_id, starts_at)
    WHERE status = 'booked';
-- +goose StatementEnd

-- +goose StatementBegin
-- Serves GET /doctors/{id}/availability: booked starts for a doctor in a range.
CREATE INDEX appointments_doctor_starts_at_idx
    ON appointments (doctor_id, starts_at)
    WHERE status = 'booked';
-- +goose StatementEnd

-- +goose StatementBegin
-- Serves GET /patients/{id}/appointments: upcoming active rows, chronological.
CREATE INDEX appointments_patient_starts_at_idx
    ON appointments (patient_id, starts_at, id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER appointments_set_updated_at
    BEFORE UPDATE ON appointments
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
-- +goose StatementEnd

-- +goose StatementBegin
-- Append-only history. Written in the same transaction as the state change it
-- describes, so the audit trail cannot drift from the appointment row.
-- Deliberately free of patient-identifying text and of cancellation reasons.
CREATE TABLE appointment_events (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    appointment_id uuid                   NOT NULL REFERENCES appointments (id) ON DELETE CASCADE,
    event_type     appointment_event_type NOT NULL,
    from_starts_at timestamptz,
    to_starts_at   timestamptz,
    -- Free-form provenance ("api"), not an authenticated identity: this service
    -- has no auth yet. See docs/security.md.
    source         text                   NOT NULL DEFAULT 'api',
    occurred_at    timestamptz            NOT NULL DEFAULT now(),

    CONSTRAINT appointment_events_source_length_check CHECK (char_length(source) BETWEEN 1 AND 64)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX appointment_events_appointment_idx
    ON appointment_events (appointment_id, occurred_at, id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS appointment_events;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS appointments;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TYPE IF EXISTS appointment_event_type;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TYPE IF EXISTS appointment_status;
-- +goose StatementEnd
