# Glossary

Terms used across this codebase and its documentation. Clinic-domain terms first,
then technical ones, then the project's own vocabulary.

---

## Clinic domain

**Appointment**
A booked consultation between one patient and one doctor, occupying exactly one
slot. Has a status of `booked` or `cancelled`. Cancelled appointments are
retained, never deleted.

**Availability**
The set of slots a patient could book right now for a given doctor on a given
local date. **Advisory** — a listed slot can be taken a millisecond later. Only
`POST /appointments` is authoritative.

**Booking**
Creating an appointment. In this codebase, "booking" always means the write; the
read is "availability".

**Doctor**
A clinician who accepts appointments. Carries an IANA timezone, which is the
authority for interpreting their working hours.

**Lead time**
The minimum notice required before an appointment starts. One hour by default:
a slot is bookable when `starts_at >= now + 1h`.

**Patient**
A person who books appointments. In this system a directory record, **not** an
identity — there is no authentication. See [security](security.md).

**Reschedule**
Moving an existing appointment to a different slot. The same row and the same ID;
a move, not a delete-and-recreate.

**Slot**
A bookable 30-minute interval, `[start, end)`. Aligned to `:00` or `:30` in the
doctor's local time.

**Working hours**
A doctor's weekly recurring availability template, expressed in local wall-clock
time per weekday. Not instants — a rule that produces different instants on
different days.

---

## Time and timezones

**DST — daylight saving time**
A seasonal change to a timezone's UTC offset. Why working hours are stored as
wall-clock time plus a zone name, never as a fixed offset.

**Half-open interval — `[start, end)`**
Includes the start, excludes the end. Used for every interval here. It is what
makes 09:00–09:30 and 09:30–10:00 adjacent rather than overlapping. Adopting one
convention everywhere removes a whole category of off-by-one bug.

**IANA timezone**
A zone identifier from the IANA database, such as `Africa/Nairobi`. Encodes the
full history of offset and DST rules, which a bare offset like `+03:00` does not.

**Instant**
A specific moment in time, independent of any timezone. Stored as `timestamptz`
in UTC. Distinct from a wall-clock time.

**Local calendar date**
A year-month-day with no timezone, such as `2026-09-07`. Not an instant. Its
meaning as a range of instants depends on whose day it is — here, the doctor's.

**RFC 3339**
The timestamp format on the wire: `2026-09-07T06:00:00Z`. A profile of ISO 8601.
An offset is mandatory in this API.

**Wall-clock time**
A time of day as a clock on a wall shows it — "09:00" — with no date and no zone.
How working hours are stored.

---

## Concurrency and databases

**Advisory lock**
A PostgreSQL lock with application-defined meaning, not tied to a row. Used by
the migrate command to serialise schema changes across processes.

**Exclusion constraint**
A PostgreSQL constraint preventing rows whose values *overlap* under a given
operator. Used on `doctor_working_hours` to stop a doctor having two overlapping
intervals on a weekday. Considered and rejected for appointments — see
[ADR 0003](adr/0003-concurrency-strategy.md).

**Idempotency**
The property that performing an operation twice has the same effect as once. See
[ADR 0005](adr/0005-idempotent-booking.md).

**Isolation level**
How much concurrent transactions can observe of each other. This project uses
PostgreSQL's default `READ COMMITTED`; correctness rests on row locks and a
unique index, both of which behave identically at any level.

**Optimistic vs. pessimistic concurrency**
*Optimistic*: attempt the write and handle the conflict — how booking works.
*Pessimistic*: lock first, then write — how cancel and reschedule work, since
they must read the current state before deciding.

**Partial index**
An index covering only rows matching a `WHERE` clause. The
`appointments_active_slot_uniq` index covers only `status = 'booked'`, which is
exactly what lets a cancelled appointment free its slot with no cleanup.

**Race condition**
Two operations whose outcome depends on their relative timing. The one this
project exists to eliminate: two patients booking the same slot simultaneously.

**Row lock — `SELECT … FOR UPDATE`**
Locks specific rows until the transaction ends. Used to serialise concurrent
cancels or reschedules of the same appointment.

**SQLSTATE**
A five-character SQL error code. The ones that matter here: `23505` unique
violation, `23P01` exclusion violation, `23503` foreign key violation, `23514`
check violation, `40001` serialisation failure, `40P01` deadlock.

**Transaction**
A group of statements that all succeed or all fail. Every state change in this
system runs in exactly one.

---

## Architecture

**Adapter**
An implementation of a port, for a specific technology. `internal/postgres` is
the persistence adapter.

**Composition root**
The single place where concrete implementations are chosen and wired together.
Here, `cmd/api/main.go`.

**Domain**
The packages holding business rules — `appointment`, `doctor`, `patient`. Contain
no I/O and import no HTTP or SQL package.

**DTO — data transfer object**
The wire-format type, separate from the domain model. Keeps the public API stable
while domain types evolve.

**Modular monolith**
One deployable unit with strong internal boundaries. See
[ADR 0001](adr/0001-modular-monolith.md).

**Port**
An interface the domain declares for something it needs — `Repository`, `Tx`,
`Clock`. Declared at the consumer, not the implementation.

**Repository**
The persistence port for one aggregate.

---

## Operations

**Cardinality**
The number of distinct values a label or field can take. **Bounded** cardinality
is essential for metrics: a label carrying a UUID would create unlimited time
series and eventually exhaust memory. Why the matched route template is logged
rather than the raw path.

**Drain**
Finishing in-flight requests before shutting down, after refusing new ones.

**Liveness — `/livez`**
"Is this process running?" Deliberately does **not** check the database: a
liveness probe that fails during a database incident would restart every replica
and turn a degraded service into no service.

**OTLP — OpenTelemetry protocol**
The wire protocol for exporting traces. Optional here; with no collector
configured the instrumentation becomes a no-op.

**Readiness — `/readyz`**
"Should traffic be routed here right now?" Checks the database, and reports `503`
while draining.

**Route template**
The parameterised pattern a request matched — `/appointments/{appointmentID}/cancel`
— as opposed to the raw path containing real IDs.

**Structured logging**
Logs as machine-parseable key/value records rather than prose, so they can be
filtered and aggregated.

**Trace / span**
A trace is one request's journey; a span is one operation within it. A `trace_id`
in a log line links it to the full trace.

---

## Project vocabulary

**apperror**
The internal error type carrying a `Kind` (which the transport maps to a status
code) and a stable `Code` (which clients branch on).

**Policy**
`appointment.Policy` — the complete set of rules deciding whether an instant is a
bookable slot start. Booking, availability and rescheduling all run through it,
which is why they cannot disagree.

**Problem details — RFC 9457**
The error response format: `application/problem+json` with `type`, `title`,
`status`, `detail` and a stable `code`.

**Ratiba**
Swahili for *schedule* or *timetable*. The name of this service.

**Slot generation**
Turning a doctor's working hours into concrete bookable slots. The generator is
the **single source of truth** for what "aligned" means — a start is aligned
precisely when it appears in the generated list, rather than by a separate
modulo check that could drift.

**testsupport**
`internal/testsupport` — in-memory doubles for the persistence ports. Fast enough
for domain and HTTP tests, and explicitly **not** able to prove anything about
concurrency; that is the integration suite's job.
