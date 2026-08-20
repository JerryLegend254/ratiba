-- name: GetDoctorByID :one
SELECT id, slug, full_name, specialty, timezone, is_active, created_at, updated_at
FROM doctors
WHERE id = $1;

-- name: GetDoctorBySlug :one
SELECT id, slug, full_name, specialty, timezone, is_active, created_at, updated_at
FROM doctors
WHERE slug = $1;

-- name: ListDoctors :many
SELECT id, slug, full_name, specialty, timezone, is_active, created_at, updated_at
FROM doctors
WHERE (NOT @active_only::boolean) OR is_active
ORDER BY full_name ASC, id ASC
LIMIT $1 OFFSET $2;

-- name: CountDoctors :one
SELECT count(*)
FROM doctors
WHERE (NOT @active_only::boolean) OR is_active;

-- name: ListWorkingHoursByDoctor :many
SELECT id, doctor_id, weekday, starts_at_local, ends_at_local
FROM doctor_working_hours
WHERE doctor_id = $1
ORDER BY weekday ASC, starts_at_local ASC;
