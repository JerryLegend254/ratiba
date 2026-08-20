#!/usr/bin/env bash
# Create the integration test database if it does not already exist.
#
# The integration suite runs against a database separate from the development
# one, so a test run can never destroy data a developer was looking at.

set -euo pipefail

POSTGRES_USER="${POSTGRES_USER:-ratiba}"
POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-ratiba_local_dev}"
POSTGRES_DB="${POSTGRES_DB:-ratiba}"
POSTGRES_PORT="${POSTGRES_PORT:-5432}"
POSTGRES_HOST="${POSTGRES_HOST:-localhost}"
TEST_DB="${POSTGRES_DB}_test"

export PGPASSWORD="$POSTGRES_PASSWORD"

# Prefer a psql on the host; fall back to the one inside the compose container,
# so this works whether or not the developer has postgresql-client installed.
if command -v psql >/dev/null 2>&1 && \
   psql -h "$POSTGRES_HOST" -p "$POSTGRES_PORT" -U "$POSTGRES_USER" -d postgres -c 'SELECT 1' >/dev/null 2>&1; then
  run_sql() { psql -h "$POSTGRES_HOST" -p "$POSTGRES_PORT" -U "$POSTGRES_USER" -d postgres -tAc "$1"; }
elif docker compose ps postgres --status running --quiet >/dev/null 2>&1; then
  run_sql() { docker compose exec -T postgres psql -U "$POSTGRES_USER" -d postgres -tAc "$1"; }
else
  echo "ERROR: cannot reach PostgreSQL at ${POSTGRES_HOST}:${POSTGRES_PORT}."
  echo "Start it first:  make up      (or: docker compose up -d postgres)"
  exit 1
fi

if [ "$(run_sql "SELECT 1 FROM pg_database WHERE datname = '${TEST_DB}'")" = "1" ]; then
  echo "==> Test database ${TEST_DB} already exists"
else
  echo "==> Creating test database ${TEST_DB}"
  run_sql "CREATE DATABASE ${TEST_DB}" >/dev/null
fi

echo "    TEST_DATABASE_URL=postgres://${POSTGRES_USER}:***@${POSTGRES_HOST}:${POSTGRES_PORT}/${TEST_DB}?sslmode=disable"
