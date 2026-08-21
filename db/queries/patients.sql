-- name: GetPatientByID :one
SELECT id, full_name, email, is_active, created_at, updated_at
FROM patients
WHERE id = $1;

-- name: ListPatients :many
SELECT id, full_name, email, is_active, created_at, updated_at
FROM patients
WHERE (NOT @active_only::boolean) OR is_active
ORDER BY full_name ASC, id ASC
LIMIT $1 OFFSET $2;

-- name: CountPatients :one
SELECT count(*)
FROM patients
WHERE (NOT @active_only::boolean) OR is_active;
