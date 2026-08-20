-- name: GetIdempotencyRecord :one
SELECT id, patient_id, idempotency_key, request_fingerprint, appointment_id,
       response_status, response_body, created_at, expires_at
FROM idempotency_keys
WHERE patient_id = $1 AND idempotency_key = $2;

-- name: CreateIdempotencyRecord :one
INSERT INTO idempotency_keys (
    patient_id, idempotency_key, request_fingerprint, appointment_id,
    response_status, response_body, expires_at
) VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, patient_id, idempotency_key, request_fingerprint, appointment_id,
          response_status, response_body, created_at, expires_at;

-- name: DeleteExpiredIdempotencyRecords :execrows
DELETE FROM idempotency_keys
WHERE expires_at <= @now::timestamptz;
