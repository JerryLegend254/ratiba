-- +goose Up
-- +goose StatementBegin
-- Replay protection for POST /appointments.
--
-- Scope: (patient_id, idempotency_key). The service has no authentication, so
-- the patient in the request body is the closest thing to a client principal.
-- When auth lands, the scope becomes (authenticated principal, key) with no
-- other change. See docs/adr/0005-idempotent-booking.md.
--
-- The row is written in the SAME transaction as the appointment it describes,
-- so a committed key always has a committed appointment and a replay can be
-- answered from the stored response alone.
CREATE TABLE idempotency_keys (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    patient_id          uuid        NOT NULL REFERENCES patients (id) ON DELETE CASCADE,
    idempotency_key     text        NOT NULL,
    -- SHA-256 over the canonicalised request body. A second request that reuses
    -- a key with a different payload is a client bug, and is rejected.
    request_fingerprint text        NOT NULL,
    appointment_id      uuid        NOT NULL REFERENCES appointments (id) ON DELETE CASCADE,
    response_status     smallint    NOT NULL,
    response_body       jsonb       NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    expires_at          timestamptz NOT NULL,

    CONSTRAINT idempotency_keys_scope_key UNIQUE (patient_id, idempotency_key),
    CONSTRAINT idempotency_keys_key_length_check CHECK (char_length(idempotency_key) BETWEEN 8 AND 255),
    CONSTRAINT idempotency_keys_fingerprint_check CHECK (request_fingerprint ~ '^[0-9a-f]{64}$'),
    CONSTRAINT idempotency_keys_status_check CHECK (response_status BETWEEN 100 AND 599),
    CONSTRAINT idempotency_keys_expiry_check CHECK (expires_at > created_at)
);
-- +goose StatementEnd

-- +goose StatementBegin
-- Supports the retention sweep documented in docs/operations.md.
CREATE INDEX idempotency_keys_expires_at_idx ON idempotency_keys (expires_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS idempotency_keys;
-- +goose StatementEnd
