#!/usr/bin/env bash
# Check that migrations are well-formed before they reach a database.
#
# goose applies migrations in filename order, so a duplicate or out-of-sequence
# number means two environments can end up with different schemas from the same
# commit. That is worth catching in CI, not during a deploy.

set -euo pipefail

MIGRATIONS_DIR="db/migrations"
problems=0

report() { printf '  \033[31m✗\033[0m %s\n' "$1"; problems=$((problems + 1)); }

echo "==> Verifying $MIGRATIONS_DIR"

if [ ! -d "$MIGRATIONS_DIR" ]; then
  echo "ERROR: $MIGRATIONS_DIR does not exist"
  exit 1
fi

files=()
while IFS= read -r file; do files+=("$file"); done < <(find "$MIGRATIONS_DIR" -name '*.sql' | sort)

if [ ${#files[@]} -eq 0 ]; then
  echo "ERROR: no migrations found"
  exit 1
fi

expected=1
seen_numbers=""

for file in "${files[@]}"; do
  name=$(basename "$file")

  # Filenames must be NNNNN_description.sql.
  if [[ ! "$name" =~ ^([0-9]{5})_[a-z0-9_]+\.sql$ ]]; then
    report "$name does not match NNNNN_snake_case_description.sql"
    continue
  fi
  number=$((10#${BASH_REMATCH[1]}))

  if [[ " $seen_numbers " == *" $number "* ]]; then
    report "$name reuses migration number $number"
  fi
  seen_numbers="$seen_numbers $number"

  if [ "$number" -ne "$expected" ]; then
    report "$name is numbered $number but $expected was expected (gaps break ordering)"
  fi
  expected=$((number + 1))

  # Every migration needs both directions. A missing Down section is only
  # discovered when somebody needs to roll back, which is the worst moment.
  grep -q -- '-- +goose Up'   "$file" || report "$name has no '-- +goose Up' section"
  grep -q -- '-- +goose Down' "$file" || report "$name has no '-- +goose Down' section"

  # goose needs explicit statement delimiters around anything containing a
  # semicolon inside a body, such as a PL/pgSQL function.
  if grep -q 'CREATE FUNCTION\|CREATE OR REPLACE FUNCTION' "$file"; then
    grep -q -- '-- +goose StatementBegin' "$file" \
      || report "$name defines a function but has no '-- +goose StatementBegin' markers"
  fi

  printf '  \033[32m✓\033[0m %s\n' "$name"
done

echo ""
if [ "$problems" -gt 0 ]; then
  echo "$problems problem(s) found in migrations."
  exit 1
fi
echo "${#files[@]} migration(s) verified."
