# ADR 0005 — Idempotency-Key on booking

- **Status:** Accepted
- **Date:** 2026-08-19

## Context

`POST /appointments` is not naturally idempotent: sending it twice books twice.

The failure that matters is not a user double-clicking — it is a **network
timeout**. The client sends a booking, the response is lost, and the client now
cannot tell whether the appointment exists. Retrying risks a duplicate; not
retrying risks the patient believing they have an appointment they do not.

A production booking API must give clients a safe answer.

## Decision

Support an optional `Idempotency-Key` request header on `POST /appointments`.

**Storage.** A row in `idempotency_keys` holding the key, a fingerprint of the
request, the appointment ID, and a **snapshot of the response**, written in the
**same transaction** as the appointment. A committed key therefore always has a
committed appointment.

**Fingerprint.** SHA-256 over the *parsed* fields
(`v1|doctor_id|patient_id|starts_at`), not the raw bytes. Two semantically
identical retries that differ in whitespace, key order, or `+00:00` versus `Z`
are correctly recognised as the same request.

**Behaviour.**

| Case | Result |
|---|---|
| Same key, same payload | The original `201`, plus `Idempotency-Replayed: true` |
| Same key, different payload | `409 idempotency_key_reuse` |
| Concurrent retries, same key | Exactly one appointment; every caller gets it |
| No key | Ordinary behaviour |

**Scope.** `(patient_id, idempotency_key)`. With no authentication, the patient
in the body is the closest available client principal. When auth lands this
becomes `(authenticated principal, key)` — a one-line change.

**Replay returns the stored snapshot, not the current row.** If the appointment
was later cancelled, a replay still shows the original `booked` state. Answering
a `201`-shaped request with a cancelled appointment would be incoherent.

## The race that this decision had to accommodate

The appointment `INSERT` happens **before** the idempotency-key `INSERT`, because
the key row has a foreign key to the appointment.

That means a retry racing its own original trips the **slot** unique index first,
and would naively be answered `409 slot_unavailable` — turning a safe retry into
a spurious failure.

The service therefore checks, on a slot conflict with a key present, whether a
committed record exists for the same `(patient, key)`; if so, the conflict was
caused by its own twin and the stored response is returned.

**This was found by `TestConcurrentIdempotentRetries`, not by review.** The
single-threaded tests all passed. See `AI_REFLECTION.md`.

## Alternatives considered

### Do nothing; document that clients should check before retrying

**Rejected.** "Check first" is the same read-then-write race the whole design
exists to avoid, pushed onto every client.

### A natural uniqueness key (patient + doctor + slot)

**Rejected.** It prevents a duplicate but cannot distinguish "the same request
retried" from "a genuine second attempt at a slot that has since been freed". It
also cannot replay the original response, so the retry gets a `409` and the
client still does not know whether *it* holds the appointment.

### A client-supplied appointment ID (`PUT /appointments/{id}`)

Genuinely idempotent, and a reasonable design. **Rejected** because the
assessment specifies `POST /appointments`, and because `Idempotency-Key` is the
conventional pattern clients already implement (Stripe, PayPal, and others).

### Store the response body rendered by the transport layer

**Rejected.** It would push HTTP concerns into the service. The snapshot is an
internal versioned representation of the appointment; the transport renders it
normally.

## Consequences

### Good

- Retries are safe. A client that times out can simply resend.
- A key reused with a different payload is caught rather than silently booking
  something unintended.
- Concurrent retries collapse to one appointment and one audit event.
- Optional — clients that do not care are unaffected.

### Bad

- An extra table and an extra write on the booking path. Negligible, and inside
  the same transaction.
- Records need sweeping. A manual command (`ratiba-migrate purge-idempotency`)
  rather than a background goroutine, because a scheduled job is visible and has
  logs.
- The scope is imperfect without authentication, as described above.

### Neutral

- The fingerprint scheme is versioned (`v1|…`), so it can change later without
  silently reinterpreting stored records.

## Verification

- `TestServiceBookIdempotency` — replay, key reuse, malformed keys, per-patient
  independence, and that a replay reflects the original state after cancellation.
- `TestConcurrentIdempotentRetries` — 8 concurrent retries against real
  PostgreSQL produce one appointment and one audit event.
- `TestIdempotencyPersistence` — a replay works from a *different* service
  instance, which is what happens when a retry lands on another replica.
