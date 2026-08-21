#!/usr/bin/env bash
# Smoke test a running Ratiba deployment.
#
# Two modes:
#
#   smoke.sh <base-url>           read-only. Safe against ANY environment,
#                                 including production. This is what CI runs
#                                 after a deploy.
#   smoke.sh <base-url> --write   full book → list → reschedule → cancel
#                                 lifecycle. Creates real rows and cleans up
#                                 after itself, so it is for development and
#                                 staging only.
#
# The read-only mode is the default precisely so that "run the smoke test" can
# never mean "write to production by accident".

set -uo pipefail

BASE_URL="${1:-http://localhost:8080}"
MODE="${2:-}"

# Seeded demo identities. Deterministic, so no discovery step is needed.
DOCTOR_ID="7f3c0a1e-1111-4a10-9c01-000000000001"   # Dr. Amina Wanjiru, Africa/Nairobi
PATIENT_ID="9b2d5e40-2222-4b20-8d02-000000000001"  # Grace Achieng

passed=0
failed=0

pass() { printf '  \033[32m✓\033[0m %s\n' "$1"; passed=$((passed + 1)); }
fail() { printf '  \033[31m✗\033[0m %s\n' "$1"; failed=$((failed + 1)); }

# request METHOD PATH [BODY] [EXTRA_HEADER]
# Writes the response body to $BODY_FILE and echoes the status code.
BODY_FILE=$(mktemp)
trap 'rm -f "$BODY_FILE"' EXIT

request() {
  local method="$1" path="$2" body="${3:-}" header="${4:-}"
  local args=(-sS -o "$BODY_FILE" -w '%{http_code}' -X "$method" --max-time 20)
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
  [ -n "$header" ] && args+=(-H "$header")
  curl "${args[@]}" "${BASE_URL}${path}" 2>/dev/null || echo "000"
}

json() { python3 -c "import json,sys;d=json.load(open('$BODY_FILE'));print($1)" 2>/dev/null; }

expect() {
  local description="$1" actual="$2" wanted="$3"
  if [ "$actual" = "$wanted" ]; then
    pass "$description ($actual)"
    return 0
  fi
  fail "$description — expected $wanted, got $actual: $(head -c 300 "$BODY_FILE")"
  return 1
}

echo "Smoke test: $BASE_URL"
[ "$MODE" = "--write" ] && echo "Mode: READ-WRITE (creates and removes test data)" || echo "Mode: read-only"
echo ""

# ---------------------------------------------------------------------------
# Read-only checks. Safe everywhere.
# ---------------------------------------------------------------------------

echo "Health and metadata"
expect "GET /livez"  "$(request GET /livez)"  200
expect "GET /readyz" "$(request GET /readyz)" 200
if [ "$(json "d['checks']['database']")" = "ok" ]; then
  pass "readiness reports the database is reachable"
else
  fail "readiness does not report a healthy database"
fi

status=$(request GET /)
if expect "GET /" "$status" 200; then
  echo "      version=$(json "d['version']") commit=$(json "d['commit']") env=$(json "d['environment']")"
fi

expect "GET /openapi.yaml" "$(request GET /openapi.yaml)" 200
expect "GET /problems"     "$(request GET /problems)"     200

echo ""
echo "Directory"
status=$(request GET /doctors)
if expect "GET /doctors" "$status" 200; then
  count=$(json "d['pagination']['total']")
  if [ "$count" = "5" ]; then
    pass "the clinic has the 5 seeded doctors"
  else
    fail "expected 5 seeded doctors, found $count"
  fi
fi

expect "GET /doctors/{id}" "$(request GET "/doctors/$DOCTOR_ID")" 200

echo ""
echo "Availability"
# Ask for the next Monday, on which every seeded doctor works.
DATE=$(python3 -c "
import datetime
d = datetime.date.today()
print((d + datetime.timedelta(days=(7 - d.weekday()) % 7 or 7)).isoformat())")

status=$(request GET "/doctors/$DOCTOR_ID/availability?date=$DATE")
if expect "GET /doctors/{id}/availability?date=$DATE" "$status" 200; then
  slots=$(json "len(d['slots'])")
  tz=$(json "d['timezone']")
  echo "      $slots free slots, timezone $tz"
  [ "$slots" -gt 0 ] && pass "slots are offered" || fail "no slots offered on a working day"
fi

echo ""
echo "Error contract"
expect "unknown doctor is 404"        "$(request GET "/doctors/00000000-0000-4000-8000-0000000000ff/availability?date=$DATE")" 404
expect "malformed UUID is 400"        "$(request GET "/doctors/not-a-uuid/availability?date=$DATE")"                           400
expect "missing date is 400"          "$(request GET "/doctors/$DOCTOR_ID/availability")"                                      400
expect "unknown route is 404"         "$(request GET /no-such-route)"                                                          404
expect "malformed JSON is 400"        "$(request POST /appointments '{"broken":')"                                             400

if [ "$(json "d['code']")" = "malformed_json" ]; then
  pass "errors carry a stable machine-readable code"
else
  fail "the error response has no usable code field"
fi

# ---------------------------------------------------------------------------
# Write checks. Development and staging only.
# ---------------------------------------------------------------------------

if [ "$MODE" = "--write" ]; then
  echo ""
  echo "Booking lifecycle"

  START=$(json "d['slots'][0]['starts_at']" 2>/dev/null || true)
  request GET "/doctors/$DOCTOR_ID/availability?date=$DATE" > /dev/null
  START=$(json "d['slots'][0]['starts_at']")
  SECOND=$(json "d['slots'][1]['starts_at']")

  if [ -z "$START" ]; then
    fail "no slot available to book"
  else
    body="{\"doctor_id\":\"$DOCTOR_ID\",\"patient_id\":\"$PATIENT_ID\",\"starts_at\":\"$START\"}"
    status=$(request POST /appointments "$body")
    if expect "POST /appointments" "$status" 201; then
      APPOINTMENT_ID=$(json "d['id']")
      echo "      booked $APPOINTMENT_ID at $START"

      expect "double booking is rejected" "$(request POST /appointments "$body")" 409
      [ "$(json "d['code']")" = "slot_unavailable" ] \
        && pass "the conflict reports slot_unavailable" \
        || fail "the conflict has the wrong code"

      # Idempotent retry.
      KEY="smoke-$(date +%s)-$RANDOM"
      request POST /appointments \
        "{\"doctor_id\":\"$DOCTOR_ID\",\"patient_id\":\"$PATIENT_ID\",\"starts_at\":\"$SECOND\"}" \
        "Idempotency-Key: $KEY" > /dev/null
      FIRST_ID=$(json "d['id']")
      request POST /appointments \
        "{\"doctor_id\":\"$DOCTOR_ID\",\"patient_id\":\"$PATIENT_ID\",\"starts_at\":\"$SECOND\"}" \
        "Idempotency-Key: $KEY" > /dev/null
      RETRY_ID=$(json "d['id']")
      [ "$FIRST_ID" = "$RETRY_ID" ] \
        && pass "an idempotent retry returns the original appointment" \
        || fail "the retry created a second appointment"
      SECOND_ID="$RETRY_ID"

      expect "patient appointment list" "$(request GET "/patients/$PATIENT_ID/appointments")" 200

      # Reschedule onto a third free slot.
      request GET "/doctors/$DOCTOR_ID/availability?date=$DATE" > /dev/null
      THIRD=$(json "d['slots'][0]['starts_at']")
      status=$(request PATCH "/appointments/$APPOINTMENT_ID/reschedule" "{\"starts_at\":\"$THIRD\"}")
      expect "PATCH reschedule" "$status" 200

      expect "rescheduling to the same slot conflicts" \
        "$(request PATCH "/appointments/$APPOINTMENT_ID/reschedule" "{\"starts_at\":\"$THIRD\"}")" 409

      # Clean up everything this run created.
      for id in "$APPOINTMENT_ID" "$SECOND_ID"; do
        [ -n "$id" ] || continue
        status=$(request PATCH "/appointments/$id/cancel" '{"reason":"automated smoke test cleanup"}')
        expect "cancelled $id" "$status" 200
      done

      expect "cancelling twice conflicts" \
        "$(request PATCH "/appointments/$APPOINTMENT_ID/cancel" '{"reason":"again"}')" 409
    fi
  fi
fi

echo ""
echo "----------------------------------------"
echo "passed: $passed   failed: $failed"
[ "$failed" -eq 0 ] || exit 1
echo "Smoke test passed."
