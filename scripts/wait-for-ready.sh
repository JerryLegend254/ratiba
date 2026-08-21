#!/usr/bin/env bash
# Poll an API's readiness endpoint until it answers, with bounded backoff.
#
# Used by `make dev` locally and by the deployment workflow after a release. A
# deploy that never becomes ready must fail the pipeline rather than hang it, so
# the retry budget is finite and the failure prints the last response.

set -uo pipefail

BASE_URL="${1:?usage: wait-for-ready.sh <base-url> [max-attempts]}"
MAX_ATTEMPTS="${2:-40}"

echo "==> Waiting for ${BASE_URL}/readyz (up to $MAX_ATTEMPTS attempts)"

delay=1
for attempt in $(seq 1 "$MAX_ATTEMPTS"); do
  response=$(curl -sS --max-time 10 -w '\n%{http_code}' "${BASE_URL}/readyz" 2>&1) || response=$'\n000'
  status="${response##*$'\n'}"
  body="${response%$'\n'*}"

  if [ "$status" = "200" ]; then
    echo "    ready after $attempt attempt(s)"
    echo "    $body"
    exit 0
  fi

  echo "    attempt $attempt/$MAX_ATTEMPTS: status $status, retrying in ${delay}s"
  sleep "$delay"

  # Exponential backoff capped at 8s: quick early polls catch a fast start,
  # while the cap keeps the total wait predictable.
  if [ "$delay" -lt 8 ]; then
    delay=$((delay * 2))
  fi
done

echo ""
echo "ERROR: ${BASE_URL}/readyz did not become ready after $MAX_ATTEMPTS attempts."
echo "Last response: ${body:-<none>} (status ${status:-none})"
echo ""
echo "Check, in order:"
echo "  1. Did the pre-deploy migration succeed? (deployment logs)"
echo "  2. Is DATABASE_URL set and reachable from the service?"
echo "  3. Did the process fail configuration validation at startup?"
echo "See docs/runbooks/api-unhealthy.md"
exit 1
