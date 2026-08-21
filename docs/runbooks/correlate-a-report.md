# Runbook — correlating a customer report

## The situation

A user says: *"I tried to book an appointment this morning and got an error."*

This is the most common support task, and the system is built so it takes about
two minutes.

## Step 1 — ask for the request ID

Every error response contains it, and every response carries the `X-Request-Id`
header:

```json
{
  "type": "/problems/slot_unavailable",
  "status": 409,
  "detail": "That slot is no longer available.",
  "code": "slot_unavailable",
  "request_id": "018f4e0a-1c2b-7d3e-9f01-2a3b4c5d6e7f"
}
```

**Ask for `request_id` or `code`.** With either one this is quick; without them
you are searching by time and guesswork.

If the user only has a screenshot, the `code` alone often answers the question —
`GET /problems/{code}` explains what it means and what to do:

```bash
curl -s "$API/problems/slot_unavailable" | jq
```

## Step 2 — find every line for that request

```bash
# Railway
railway logs --environment production | grep '018f4e0a-1c2b-7d3e-9f01-2a3b4c5d6e7f'

# Local
docker compose logs api --no-log-prefix | grep '018f4e0a-…'
```

**Every** line from that request carries the ID — including domain events written
deep in the service layer that never saw an `*http.Request`. That is why the ID
lives in the context rather than being threaded through function signatures.

## Step 3 — read the story

```json
{"event":"http.rejected","error_code":"slot_unavailable","status":409,"request_id":"018f…"}
{"event":"appointment.slot_conflict","doctor_id":"7f3c…","starts_at":"2026-09-07T06:00:00Z","request_id":"018f…"}
{"event":"http.request","method":"POST","route":"/appointments","status":409,"duration_ms":12,"request_id":"018f…"}
```

Reading outward: someone else took the slot 12 ms into the request. Working as
designed. The answer to the user is "that slot was taken while you were booking
— please pick another".

### If it was a 5xx

The `http.error` line carries the internal cause the client never saw:

```json
{"event":"http.error","error_code":"internal_error","status":500,
 "error":"internal_error (internal): The server encountered an unexpected condition.: book appointment: create appointment: ERROR: …"}
```

### If it was a panic

`event=http.panic` carries the full stack trace, the route and the request ID.
Go to [elevated-5xx.md](elevated-5xx.md#panics).

## Step 4 — pivot into the trace

If `trace_id` is present, open it in Grafana/Tempo. It shows the HTTP span and
every SQL statement with timings — the fastest way to answer "where did the time
go?".

## Step 5 — confirm the current state

The user may not know whether their booking actually happened, particularly if
their client timed out.

```bash
# By appointment ID, if they have one
curl -s "$API/appointments/$APPOINTMENT_ID" | jq

# Or everything upcoming for that patient
curl -s "$API/patients/$PATIENT_ID/appointments" | jq
```

If they used an `Idempotency-Key`, **replaying the exact same request is safe**
and returns the original outcome — which is precisely the situation that feature
exists for.

## Step 6 — if there is no request ID

Narrow by time and route:

```bash
railway logs --environment production \
  | grep 'route=/appointments' | grep 'status=4\|status=5' | tail -50
```

Then correlate with what the user described. If you know the patient ID:

```sql
SELECT id, doctor_id, starts_at, status, created_at
FROM appointments
WHERE patient_id = '…' AND created_at > now() - interval '6 hours'
ORDER BY created_at DESC;
```

The `appointment_events` table gives the full history of one appointment:

```sql
SELECT event_type, from_starts_at, to_starts_at, source, occurred_at
FROM appointment_events WHERE appointment_id = '…' ORDER BY occurred_at;
```

## What you will not find in the logs, by design

| Not logged | Where to find it |
|---|---|
| The cancellation reason | The `appointments` row — it is patient free text and treated as clinical data |
| Patient names or emails | The `patients` table |
| Request or response bodies | Never captured |
| Raw paths with identifiers | Only the route template is logged; the IDs are in domain event fields where they are structured and bounded |

This is deliberate. See [security](../security.md#data-classification).

## Common codes and what to tell the user

| Code | Meaning | Response to the user |
|---|---|---|
| `slot_unavailable` | Someone else took it | "Please refresh and pick another slot" |
| `slot_too_soon` | Less than an hour away | "Bookings need at least an hour's notice" |
| `slot_not_aligned` | Not on the :00/:30 grid | Their client should use the availability endpoint |
| `slot_outside_working_hours` | Outside the doctor's hours | "That doctor isn't available then" |
| `doctor_not_working_on_date` | Not a working day | "That doctor doesn't work that day" |
| `appointment_already_cancelled` | Already cancelled | "That appointment was already cancelled" |
| `reschedule_same_slot` | Already at that time | "It's already booked for that time" |
| `idempotency_key_reuse` | Client bug | Escalate to the client's developer |
| `internal_error` | Our fault | Apologise, escalate with the request ID |
