-- +goose Up
-- +goose StatementBegin
-- btree_gist lets a GiST exclusion constraint mix equality operators (uuid, int)
-- with the range overlap operator, which is how doctor_working_hours guarantees
-- that a doctor never has two overlapping intervals on the same weekday.
CREATE EXTENSION IF NOT EXISTS btree_gist;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE doctors (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Human-friendly stable handle so reviewers and smoke tests can address a
    -- doctor without first querying the database for a UUID.
    slug        text        NOT NULL,
    full_name   text        NOT NULL,
    specialty   text        NOT NULL,
    -- IANA zone name. This, not the host machine, is the authority for
    -- interpreting working hours and requested calendar dates.
    timezone    text        NOT NULL,
    is_active   boolean     NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT doctors_slug_key UNIQUE (slug),
    CONSTRAINT doctors_slug_format_check CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
    CONSTRAINT doctors_slug_length_check CHECK (char_length(slug) BETWEEN 1 AND 64),
    CONSTRAINT doctors_full_name_length_check CHECK (char_length(full_name) BETWEEN 1 AND 200),
    CONSTRAINT doctors_specialty_length_check CHECK (char_length(specialty) BETWEEN 1 AND 120),
    CONSTRAINT doctors_timezone_length_check CHECK (char_length(timezone) BETWEEN 1 AND 64)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER doctors_set_updated_at
    BEFORE UPDATE ON doctors
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE patients (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    full_name   text        NOT NULL,
    email       text        NOT NULL,
    is_active   boolean     NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT patients_email_key UNIQUE (email),
    CONSTRAINT patients_email_format_check CHECK (email ~ '^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$'),
    CONSTRAINT patients_email_length_check CHECK (char_length(email) BETWEEN 3 AND 254),
    CONSTRAINT patients_full_name_length_check CHECK (char_length(full_name) BETWEEN 1 AND 200)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER patients_set_updated_at
    BEFORE UPDATE ON patients
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
-- +goose StatementEnd

-- +goose StatementBegin
-- Weekly recurring availability template, expressed in the doctor's local wall
-- clock. Intervals are half-open [starts_at_local, ends_at_local) and may not
-- cross midnight, which keeps "which local day does this slot belong to?" a
-- single-day question everywhere in the codebase.
CREATE TABLE doctor_working_hours (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    doctor_id       uuid        NOT NULL REFERENCES doctors (id) ON DELETE CASCADE,
    -- 0 = Sunday .. 6 = Saturday, matching Go's time.Weekday.
    weekday         smallint    NOT NULL,
    starts_at_local time        NOT NULL,
    ends_at_local   time        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    -- Minute-of-day range derived from the wall-clock columns purely so the
    -- exclusion constraint below has a range type to work with.
    minute_range int4range GENERATED ALWAYS AS (
        int4range(
            (EXTRACT(HOUR FROM starts_at_local) * 60 + EXTRACT(MINUTE FROM starts_at_local))::int,
            (EXTRACT(HOUR FROM ends_at_local) * 60 + EXTRACT(MINUTE FROM ends_at_local))::int
        )
    ) STORED,

    CONSTRAINT doctor_working_hours_weekday_check CHECK (weekday BETWEEN 0 AND 6),
    CONSTRAINT doctor_working_hours_order_check CHECK (starts_at_local < ends_at_local),
    -- Working hours must sit on the same 30-minute grid as appointments,
    -- otherwise generated slots could not be :00/:30 aligned.
    CONSTRAINT doctor_working_hours_alignment_check CHECK (
        EXTRACT(MINUTE FROM starts_at_local)::int % 30 = 0
        AND EXTRACT(SECOND FROM starts_at_local) = 0
        AND EXTRACT(MINUTE FROM ends_at_local)::int % 30 = 0
        AND EXTRACT(SECOND FROM ends_at_local) = 0
    ),
    CONSTRAINT doctor_working_hours_no_overlap EXCLUDE USING gist (
        doctor_id WITH =,
        weekday WITH =,
        minute_range WITH &&
    )
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX doctor_working_hours_doctor_weekday_idx
    ON doctor_working_hours (doctor_id, weekday, starts_at_local);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER doctor_working_hours_set_updated_at
    BEFORE UPDATE ON doctor_working_hours
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS doctor_working_hours;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS patients;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS doctors;
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS set_updated_at();
-- +goose StatementEnd
