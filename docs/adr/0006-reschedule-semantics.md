# ADR 0006 — Rescheduling to the current slot is a conflict

- **Status:** Accepted
- **Date:** 2026-08-19

## Context

`PATCH /appointments/{id}/reschedule` with a `starts_at` equal to the
appointment's **current** start is ambiguous. Three defensible answers exist:

1. **`200 OK`, no-op** — friendly to retries; "the appointment is at that time,
   which is what you asked for".
2. **`409 Conflict`** — the request collides with the resource's current state.
3. **`422 Unprocessable`** — the input is semantically wrong.

The brief does not say. It has to be decided, documented and tested.

## Decision

**`409` with the stable code `reschedule_same_slot`.**

## Rationale

The deciding argument is the **audit trail**.

Every state change appends to `appointment_events`. If a same-slot reschedule
returned `200`, one of two things must happen:

- Append a `rescheduled` event with `from_starts_at == to_starts_at`. The audit
  log now records a move that never happened. Anyone later reconstructing an
  appointment's history — which is the entire purpose of that table — sees noise
  they must learn to filter.
- Return `200` and append nothing. Now a successful mutating request left no
  trace, and the invariant "every state change has an event" is quietly false.

Neither is acceptable in a system whose audit trail is meant to be trustworthy.

Between `409` and `422`: `409` is the better fit because the request conflicts
with the **current state of the resource**, not with the shape of the input. The
same request would have been perfectly valid a moment earlier, against a
different current state — which is the textbook description of `409`.

It is also consistent: attempting the same move as a fresh booking would hit the
unique index and produce `409 slot_unavailable`. A caller already handles `409`
on this endpoint; this is one more code within it.

## Alternatives considered

### `200 OK` as a no-op

**Rejected** for the audit reasons above. Worth acknowledging the real cost:
a client retrying a reschedule after a timeout gets `409` rather than success,
and must treat that code as "already in the desired state".

That cost is mitigated by the code being **specific**. `reschedule_same_slot` is
distinguishable from `slot_unavailable`, so a client that wants no-op semantics
can implement them in one line:

```js
if (res.status === 409 && body.code === "reschedule_same_slot") {
  // already where we want it
}
```

The reverse is not possible: a `200` cannot be turned back into information that
nothing happened.

### `422 Unprocessable Entity`

**Rejected.** `422` is used throughout this API for *input that breaks a rule
regardless of state* — a misaligned start, a slot outside working hours, a slot
in the past. A same-slot reschedule is not wrong input; it is wrong relative to
the current state. Keeping that distinction sharp is what makes the status codes
worth reading.

### Silently accept and skip the audit event

**Rejected.** It makes the audit trail conditionally complete, which is the same
as incomplete.

## Consequences

### Good

- The audit trail records only moves that actually occurred.
- The code is specific enough for a client to implement no-op semantics itself.
- Consistent with the `409` a fresh booking would produce.
- The behaviour is explicit and tested rather than emergent.

### Bad

- Clients expecting PUT-like idempotency are surprised. The response is at least
  unambiguous about what happened; it will need to be called out prominently
  wherever the API is documented.

### Neutral

- Rescheduling to a **different** slot remains fully idempotent in effect: repeat
  it and the second call returns `409 reschedule_same_slot`, because the first
  one succeeded. A client can treat that as confirmation.

## Verification

- `TestServiceReschedule/moving to the current slot is a conflict`

The HTTP layer and any end-to-end check must assert the same code once they
exist; a behaviour this easy to "simplify" needs a test at every level that
could quietly change it.
