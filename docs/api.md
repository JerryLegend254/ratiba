# API reference

The machine-readable contract is [`api/openapi.yaml`](../api/openapi.yaml),
served by any running instance at `/openapi.yaml` with an interactive explorer at
`/docs`. CI validates the document and checks it against the actual routes **in
both directions**, so it cannot silently drift from the implementation.

A runnable request collection is in [`api/examples.http`](../api/examples.http).

---

## Conventions

### Authentication

**There is none.** Every endpoint is open. This is a documented scope decision
for the assessment, not an oversight — see [security](security.md). The only
credential-protected endpoint is `GET /metrics`, and only when
`METRICS_AUTH_TOKEN` is configured (mandatory in production).

### Base URLs

| Environment | URL |
|---|---|
| Local | `http://localhost:8080` |
| Development | *(set as the `PUBLIC_URL` variable in the GitHub `development` environment)* |
| Staging | *(likewise)* |
| Production | *(likewise — not yet deployed)* |

### Content negotiation

Requests carrying a body **must** send `Content-Type: application/json`; anything
else is `415`. Successful responses are `application/json; charset=utf-8`.
Errors are `application/problem+json`.

Request bodies are decoded strictly. Four things are rejected that many APIs
silently tolerate:

| Rejected | Code | Why |
|---|---|---|
| Unknown fields | `unknown_field` | `"start_at"` instead of `"starts_at"` would otherwise be ignored and book the zero time |
| Trailing JSON | `trailing_content` | A doubled body could smuggle a second document past validation |
| Oversized bodies | `payload_too_large` | Bounded by `HTTP_MAX_BODY_BYTES` (64 KiB default) |
| Wrong content type | `unsupported_media_type` | |

### Identifiers

UUIDs, as strings. Non-sequential, so an ID leaks no information about volume or
ordering. A malformed UUID in a path is `400 invalid_path_parameter`, **not**
`404` — the request could not be parsed, so claiming the resource was not found
would be a lie.

### Timestamps and timezones

- **On the wire, instants are RFC 3339 in UTC**: `2026-09-07T06:00:00Z`.
- **`starts_at` on a request may carry any offset.** `2026-09-07T09:00:00+03:00`
  and `2026-09-07T06:00:00Z` are the same instant and are treated identically.
- **An offset is mandatory.** `2026-09-07T09:00:00` is rejected `422`. A naive
  timestamp would have to be interpreted against *something* — the server's
  zone, UTC, the doctor's zone — and every choice silently books the wrong hour
  when it is the wrong one.
- **Calendar dates** (`?date=2026-09-07`) are interpreted in the **doctor's**
  timezone, which is returned alongside the result.
- Responses that benefit from a local rendering carry it as an **additional**
  field (`starts_at_local`), never instead of the UTC one.

### Pagination

Collection endpoints return:

```json
{
  "data": [ ... ],
  "pagination": { "limit": 20, "offset": 0, "total": 3, "has_more": false }
}
```

`limit` defaults to 20 and is capped at 100. A `limit` outside `1..100` is
**rejected with `400`, not silently clamped** — clamping would let a client
believe it asked for 5000 records and received all of them.

Ordering is `(starts_at ASC, id ASC)`. The `id` tie-breaker makes the order
total, so paging is stable.

*Known limitation:* offset paging means a concurrent insert can shift an item
between page fetches. Keyset pagination would fix it and matters at a data volume
this system is not at.

### Idempotency

`POST /appointments` accepts an `Idempotency-Key` header (8–255 printable ASCII
characters).

- **Same key, same payload** → the original `201` response, with
  `Idempotency-Replayed: true`. No second appointment; no second audit event.
- **Same key, different payload** → `409 idempotency_key_reuse`.
- **Concurrent retries with the same key** → exactly one appointment; every
  caller receives it.

The stored response is a snapshot of the appointment *as first returned*. If the
appointment is later cancelled, a replay still returns the original `booked`
state — answering a `201`-shaped request with a cancelled appointment would be
incoherent.

Keys are scoped `(patient_id, key)` and expire after `BOOKING_IDEMPOTENCY_TTL`
(24 hours by default). See [ADR 0005](adr/0005-idempotent-booking.md).

### Request IDs

Every response carries `X-Request-Id`. Send your own and it is echoed, provided
it is at most 64 characters of `[A-Za-z0-9._-]`; anything else is replaced with a
generated UUID (the value reaches log fields, so it is sanitised rather than
trusted). Error bodies repeat it as `request_id`.

**Quote this in any support request.** Every log line from that request carries
it, including domain events written deep in the service.

### Rate limiting

None. See [security](security.md#deferred-controls).

---

## Errors

RFC 9457 problem documents:

```json
{
  "type": "/problems/slot_too_soon",
  "title": "Unprocessable Entity",
  "status": 422,
  "detail": "Appointments must start at least 1 hour from now.",
  "instance": "/appointments",
  "code": "slot_too_soon",
  "request_id": "018f4e0a-1c2b-7d3e-9f01-2a3b4c5d6e7f",
  "violations": [
    { "field": "starts_at", "code": "too_soon", "message": "…" }
  ]
}
```

**Branch on `code`.** `title` and `detail` are human-facing and may be reworded;
`code` is part of the contract. `type` resolves — `GET /problems/slot_too_soon`
returns an explanation.

### Status semantics

| Status | Meaning | Example codes |
|---|---|---|
| `400` | Could not be understood | `malformed_json`, `unknown_field`, `invalid_path_parameter`, `invalid_query_parameter` |
| `404` | Referenced resource does not exist | `doctor_not_found`, `patient_not_found`, `appointment_not_found` |
| `405` | Path exists, method does not | `method_not_allowed` |
| `409` | Conflicts with current state | `slot_unavailable`, `appointment_already_cancelled`, `reschedule_same_slot`, `idempotency_key_reuse` |
| `413` | Body too large | `payload_too_large` |
| `415` | Wrong content type | `unsupported_media_type` |
| `422` | Understood, refused by a rule | `slot_in_past`, `slot_too_soon`, `slot_not_aligned`, `slot_outside_working_hours`, `doctor_not_working_on_date`, `doctor_inactive`, `patient_inactive`, `validation_failed` |
| `500` | Unexpected | `internal_error` |
| `503` | Draining, timed out, dependency down | `request_timeout`, `dependency_unavailable`, `service_shutting_down` |

The `400`/`422` split: **`400` means "I could not parse this". `422` means "I
understood you perfectly and I am refusing."**

### Validation order on booking

Deterministic and tested, so an error is reproducible:

1. **Temporal** — `slot_in_past`, then `slot_too_soon`
2. **Structural** — `doctor_not_working_on_date`, then `slot_not_aligned` or
   `slot_outside_working_hours`
3. **Availability** — `slot_unavailable`, decided by the database

Temporal comes first deliberately: *"you cannot book last Tuesday"* is more
useful than *"the doctor does not work on Tuesdays"*, especially since a schedule
may have changed since.

The full catalogue is served at `GET /problems`.

---

## Endpoints

Replace `$API` with your base URL. Demo IDs are in the
[README](../README.md#demo-identifiers).

### Book an appointment

```
POST /appointments
```

```bash
curl -sS -X POST "$API/appointments" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: 018f4e0a-1c2b-7d3e-9f01-2a3b4c5d6e7f' \
  -d '{
        "doctor_id":  "7f3c0a1e-1111-4a10-9c01-000000000001",
        "patient_id": "9b2d5e40-2222-4b20-8d02-000000000001",
        "starts_at":  "2026-09-07T06:00:00Z"
      }'
```

**`201 Created`** — with `Location: /appointments/{id}`:

```json
{
  "id": "3fa85f64-5717-4562-b3fc-2c963f66afa6",
  "doctor_id": "7f3c0a1e-1111-4a10-9c01-000000000001",
  "patient_id": "9b2d5e40-2222-4b20-8d02-000000000001",
  "starts_at": "2026-09-07T06:00:00Z",
  "ends_at": "2026-09-07T06:30:00Z",
  "status": "booked",
  "cancellation_reason": null,
  "cancelled_at": null,
  "created_at": "2026-09-06T11:02:19Z",
  "updated_at": "2026-09-06T11:02:19Z"
}
```

**`409` — slot taken:**

```json
{ "type": "/problems/slot_unavailable", "title": "Conflict", "status": 409,
  "detail": "That slot is no longer available.", "code": "slot_unavailable",
  "request_id": "…" }
```

**`422` — inside the lead time:**

```json
{ "type": "/problems/slot_too_soon", "title": "Unprocessable Entity", "status": 422,
  "detail": "Appointments must start at least 1 hour from now.",
  "code": "slot_too_soon", "request_id": "…" }
```

**`400` — a typo in a field name:**

```json
{ "type": "/problems/unknown_field", "title": "Bad Request", "status": 400,
  "detail": "The request body contains a field this endpoint does not accept.",
  "code": "unknown_field",
  "violations": [ { "field": "start_at", "code": "unknown", "message": "Remove this field." } ] }
```

---

### Get a doctor's availability

```
GET /doctors/{doctorId}/availability?date=YYYY-MM-DD
```

```bash
curl -sS "$API/doctors/7f3c0a1e-1111-4a10-9c01-000000000001/availability?date=2026-09-07"
```

**`200 OK`:**

```json
{
  "doctor": {
    "id": "7f3c0a1e-1111-4a10-9c01-000000000001",
    "slug": "amina-wanjiru",
    "full_name": "Dr. Amina Wanjiru",
    "specialty": "General Practice",
    "timezone": "Africa/Nairobi"
  },
  "date": "2026-09-07",
  "timezone": "Africa/Nairobi",
  "slot_duration_minutes": 30,
  "min_lead_time_minutes": 60,
  "slots": [
    { "starts_at": "2026-09-07T06:00:00Z", "ends_at": "2026-09-07T06:30:00Z",
      "starts_at_local": "2026-09-07T09:00:00+03:00" },
    { "starts_at": "2026-09-07T06:30:00Z", "ends_at": "2026-09-07T07:00:00Z",
      "starts_at_local": "2026-09-07T09:30:00+03:00" }
  ]
}
```

Excluded from the list: slots already held by an active appointment, slots inside
the one-hour lead time, and anything outside a working-hours interval. A
non-working day returns `"slots": []`, not an error.

**This result is advisory.** A listed slot can be taken a millisecond later.
`POST /appointments` is the only authority — which is precisely why booking does
not trust an earlier availability read.

**`400`** if `date` is missing or malformed (including `2026-02-30`, which Go's
parser would otherwise normalise into March). **`404`** for an unknown doctor.
**`422 doctor_inactive`** for a doctor not accepting appointments.

---

### Cancel an appointment

```
PATCH /appointments/{appointmentId}/cancel
```

```bash
curl -sS -X PATCH "$API/appointments/$APPOINTMENT_ID/cancel" \
  -H 'Content-Type: application/json' \
  -d '{"reason": "Patient is travelling that week"}'
```

**`200 OK`:**

```json
{
  "id": "3fa85f64-5717-4562-b3fc-2c963f66afa6",
  "starts_at": "2026-09-07T06:00:00Z",
  "ends_at": "2026-09-07T06:30:00Z",
  "status": "cancelled",
  "cancellation_reason": "Patient is travelling that week",
  "cancelled_at": "2026-09-06T12:15:44Z",
  "…": "…"
}
```

The slot is bookable again the instant this commits. The record is retained, not
deleted.

`reason` is **required**, 1–500 characters after trimming. It is stored on the
appointment and **never written to logs, metrics or traces** — it is free text a
patient typed and is treated as clinical data.

**`409 appointment_already_cancelled`** on a repeat, so a retry cannot overwrite
the original reason and timestamp. **`422 validation_failed`** for a blank or
over-long reason, with a `violations` entry naming `reason`.

---

### Reschedule an appointment

```
PATCH /appointments/{appointmentId}/reschedule
```

```bash
curl -sS -X PATCH "$API/appointments/$APPOINTMENT_ID/reschedule" \
  -H 'Content-Type: application/json' \
  -d '{"starts_at": "2026-09-07T08:00:00Z"}'
```

**`200 OK`** with the moved appointment — same `id`, new times.

The destination is validated by exactly the rules a fresh booking uses. The move
is one `UPDATE` inside the transaction holding the row lock, so the old slot is
released and the new one claimed together.

**If the destination is taken, the whole transaction rolls back and the
appointment keeps its original time.** It is never left slotless. Proven by
`TestRescheduleAtomicity` and `TestConcurrentReschedulesOntoOneSlot`.

| Situation | Response |
|---|---|
| Destination taken | `409 slot_unavailable` |
| Appointment cancelled | `409 appointment_already_cancelled` |
| Destination is the current slot | `409 reschedule_same_slot` |
| Destination breaks a rule | `422` with the specific code |

On `reschedule_same_slot`: returning `200` would append a `rescheduled` audit
event describing a move that never happened. See
[ADR 0006](adr/0006-reschedule-semantics.md).

---

### List a patient's upcoming appointments

```
GET /patients/{patientId}/appointments?limit=20&offset=0
```

```bash
curl -sS "$API/patients/9b2d5e40-2222-4b20-8d02-000000000001/appointments"
```

**`200 OK`:**

```json
{
  "data": [
    {
      "id": "3fa85f64-5717-4562-b3fc-2c963f66afa6",
      "doctor_id": "7f3c0a1e-1111-4a10-9c01-000000000001",
      "patient_id": "9b2d5e40-2222-4b20-8d02-000000000001",
      "starts_at": "2026-09-07T06:00:00Z",
      "ends_at": "2026-09-07T06:30:00Z",
      "status": "booked",
      "cancellation_reason": null,
      "cancelled_at": null,
      "created_at": "2026-09-06T11:02:19Z",
      "updated_at": "2026-09-06T11:02:19Z",
      "starts_at_local": "2026-09-07T09:00:00+03:00",
      "doctor": {
        "id": "7f3c0a1e-1111-4a10-9c01-000000000001",
        "slug": "amina-wanjiru",
        "full_name": "Dr. Amina Wanjiru",
        "specialty": "General Practice",
        "timezone": "Africa/Nairobi"
      }
    }
  ],
  "pagination": { "limit": 20, "offset": 0, "total": 1, "has_more": false }
}
```

**Future active appointments only**, ascending. Cancelled and past ones are
excluded: this answers *"what do I still need to show up for?"*, not *"what is my
history?"*.

The doctor is embedded so a client can render the list without N extra requests.
An empty result is `"data": []`, never `null`.

---

### Fetch a single appointment

```
GET /appointments/{appointmentId}
```

Returns the appointment in any status. A client holding an ID should be able to
discover that it was cancelled.

---

### Directory

```
GET /doctors?active_only=true&limit=20&offset=0
GET /doctors/{doctorId}
GET /patients?active_only=true&limit=20&offset=0
GET /patients/{patientId}
```

Not part of the assessment brief. They exist so a reviewer can find a valid ID
without opening a database session. Read-only and bounded.

`GET /patients` returns names and email addresses. In a system holding real
patient data this would require authorisation; see [security](security.md).

---

### Operational endpoints

| Endpoint | Purpose |
|---|---|
| `GET /` | Service info, build metadata, endpoint list |
| `GET /livez` | Liveness. Does **not** touch the database — a liveness probe failing during a database incident would restart every replica and turn a degraded service into no service |
| `GET /readyz` | Readiness. Verifies the database with a short timeout; `503` while draining |
| `GET /metrics` | Prometheus text. Bearer-token protected when `METRICS_AUTH_TOKEN` is set |
| `GET /openapi.yaml` | The contract, embedded in the binary |
| `GET /docs` | Swagger UI |
| `GET /problems`, `/problems/{code}` | Error catalogue |

`/readyz` response:

```json
{ "status": "ready",
  "checks": { "accepting_traffic": "ok", "database": "ok" },
  "version": "v1.0.0", "commit": "a1b2c3d" }
```

It never includes connection strings or driver errors — it is unauthenticated and
internet-facing. Asserted by a test.

---

## The `/docs` CDN dependency

`/docs` loads Swagger UI from a pinned CDN (`unpkg.com`) rather than vendoring
the ~1 MB dist bundle into the repository and the container image. It is a
documentation page, not part of the API's runtime path, and it degrades to a
plain link when the CDN is unreachable.

`GET /openapi.yaml` has **no external dependency** and always works. If a
zero-external-dependency docs page is required, vendor `swagger-ui-dist` into
`api/` and serve it with `embed.FS`; the trade-off is image size.

---

## Client guidance

1. **Fetch availability, then book.** Do not construct slot times yourself — the
   generator is the authority and its output already accounts for working hours,
   the lead time and DST.
2. **Send an `Idempotency-Key` on every booking.** A network timeout leaves you
   unable to tell whether the booking happened; a retry with the same key is safe.
3. **Handle `409 slot_unavailable` by refetching availability.** It is normal
   under contention, not an error condition.
4. **Branch on `code`, never on `detail`.**
5. **Log the `request_id`** from every error response. It is the fastest path to
   a diagnosis.
