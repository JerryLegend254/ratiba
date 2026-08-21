# ADR 0007 — Authentication is deferred and documented

- **Status:** Accepted
- **Date:** 2026-08-19

## Context

The assessment describes a patient-facing booking API and does not mention
authentication, accounts, or sessions. It also, correctly, expects a
production-minded submission.

An appointment system without authentication is not production-ready. So there
are two honest options and one dishonest one:

1. Build authentication properly.
2. Leave it out, state it prominently, and document what closing it involves.
3. Build something token-shaped that *looks* like security.

## Decision

**Option 2.** No authentication or authorisation. Every endpoint is open. The gap
is documented in the README, `docs/security.md`, `docs/api.md`, the OpenAPI
description, and here.

The only credential in the system guards `GET /metrics`, and only because Railway
routes every path publicly and an unauthenticated metrics endpoint would publish
booking volumes and internal timings to anyone.

The codebase is **arranged so that adding auth is additive**, not a rewrite:

| Seam already in place | Becomes |
|---|---|
| `appointment_events.source` (currently `"api"`) | The authenticated actor ID |
| Idempotency scope `(patient_id, key)` | `(principal, key)` — a one-line change, noted in [ADR 0005](0005-idempotent-booking.md) |
| Middleware chain with context propagation | Where an auth middleware slots in |
| Service methods taking a `context.Context` | Where a principal is read from |

## Alternatives considered

### Build full authentication

**Rejected for scope.** It is a substantial subsystem — credential storage,
sessions or tokens, rotation, recovery flows, and a decision about whether clinic
staff are a distinct principal type with different permissions. Done badly it is
worse than absent; done well it is larger than the rest of this assessment.

### A shared API key

**Rejected, and this is the important rejection.** A single static key would look
like security while providing almost none: it authenticates nothing about *which*
patient is calling, so it cannot support the authorisation rule that actually
matters — "a patient may only act on their own appointments". Every caller would
still be able to cancel every appointment.

Worse, it would create the *impression* of protection. A reviewer, or a future
engineer, might reasonably assume the endpoints are guarded. **An openly absent
control is safer than a decorative one**, because it cannot be mistaken for a
real one.

### Basic auth on everything

**Rejected** for the same reason, plus it would make the demo unusable without
distributing credentials.

### JWT validation with no issuer

**Rejected.** Validating tokens nobody issues is theatre.

## Consequences

### Good

- The scope is honest and the gap is impossible to miss — it is in the README's
  first section on security, in the API docs, and in the OpenAPI description a
  client reads.
- No misleading security surface.
- The seams exist, so the work is bounded and identified.

### Bad — stated plainly

- **Anyone can read the patient directory**, including names and email addresses.
- **Anyone with an appointment ID can cancel or reschedule it.** UUIDs make
  guessing impractical, which is obscurity, not access control.
- **No rate limiting**, which is partly a consequence: without a principal there
  is nothing meaningful to rate-limit per.
- The deployment must be treated as a **demonstration**, not a service. It holds
  only fictitious data, and that is what makes this acceptable.

### What would close it

1. OIDC or a signed-token scheme. No bespoke password handling.
2. Authorisation on every appointment path: a patient acts only on their own
   appointments; clinic staff get a broader role.
3. `GET /patients` restricted to staff.
4. `appointment_events.source` carries the authenticated actor.
5. Idempotency keys re-scoped to the principal.
6. Audit logging of authorisation failures.
7. Rate limiting keyed on the principal, with shared state across replicas.

Full detail in [docs/security.md](../security.md).
