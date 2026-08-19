# ADR 0003 — A partial unique index enforces the booking invariant

- **Status:** Accepted
- **Date:** 2026-08-19
- **Supersedes:** —

## Context

The clinic's requirement is one sentence: *"Once booked, that slot must not be
available to others."*

That sentence has an uncomfortable property: **it cannot be enforced by
reading.** Any availability check performed before a write is stale the moment it
returns. Between "is 09:00 free?" and "insert the appointment", another request
can commit. The window can be made small; it cannot be made zero.

So a decision must be made somewhere that can be **atomic with the write**. The
system also has to keep working when there is more than one API replica, so
anything living in a single process is out.

Two more constraints shape the answer:

- Cancelled appointments must be **retained** for audit, and cancelling must free
  the slot immediately.
- Appointments are a **fixed 30 minutes** and are **aligned** to the slot grid.

## Decision

Enforce the invariant with a **partial unique index** in PostgreSQL:

```sql
CREATE UNIQUE INDEX appointments_active_slot_uniq
    ON appointments (doctor_id, starts_at)
    WHERE status = 'booked';
```

And treat a violation of it — never an earlier read — as the authoritative answer
to "was this slot free?".

Concretely:

1. Application validation exists to produce a **better error, faster**. It never
   prevents the conflict.
2. Booking inserts and lets the database decide. `SQLSTATE 23505` on this
   constraint becomes `appointment.ErrSlotTaken` → `409 slot_unavailable`.
3. Rescheduling is a **single `UPDATE`**, so releasing the old slot and claiming
   the new one is one atomic change the index sees at once.
4. Cancel and reschedule take a row lock (`SELECT … FOR UPDATE`) so concurrent
   state transitions on the *same* appointment serialise.
5. Isolation stays at the default `READ COMMITTED`.

**Partial** is what makes it work. Only `booked` rows participate, so a cancelled
row does not block rebooking, and a slot's full history — many cancelled rows
sharing `(doctor_id, starts_at)` — is preserved with no cleanup job.

## Alternatives considered

### Application-level check-then-insert

Read availability, then insert if free.

**Rejected.** It is exactly the race described above. It appears to work under
manual testing and fails under load — the worst possible failure mode, because it
fails silently and produces two patients in one room.

### A mutex or singleflight in the application

**Rejected.** Correct within one process, useless across replicas. It would also
create a false sense of safety that survives right up until the service is scaled
out.

### An advisory lock per doctor+slot

`pg_advisory_xact_lock(hashtext(doctor_id || starts_at))`

**Rejected.** It works and is cross-replica, but it is strictly worse than the
index: it relies on a hash with collision potential, the lock is invisible in the
schema (nothing tells a future reader the invariant exists), and it protects the
rule only on code paths that remember to take it. The index protects the data
against *every* writer, including a migration or a manual `INSERT`.

### `SELECT … FOR UPDATE` on the doctor row

**Rejected.** It serialises **all** bookings for a doctor, including for
completely unrelated slots. A popular doctor becomes a global bottleneck.

This is a genuinely tempting design, which is why
`TestConcurrentBookingsForDifferentSlotsAllSucceed` exists: an over-broad lock
would pass every double-booking test while quietly serialising the whole clinic.
Only a test asserting that unrelated bookings succeed *concurrently* catches it.

### An exclusion constraint over `tstzrange`

```sql
EXCLUDE USING gist (doctor_id WITH =, tstzrange(starts_at, ends_at) WITH &&)
```

**Rejected — for now.** It is the right tool for arbitrary overlapping
durations, and it is genuinely more general. It is not used because:

- Slots are fixed at 30 minutes **and aligned**, so two appointments overlap if
  and only if they start at the same instant. Equality is sufficient.
- A B-tree unique index is smaller, faster to maintain, and understood by every
  engineer who will read this schema. A GiST exclusion constraint is not.
- It would require `btree_gist` on the busiest table.

**This is a real trade-off, not a dismissal.** If appointments ever gain variable
durations, the unique index becomes insufficient and this **must** become an
exclusion constraint. That is a migration, and this record is where a future
engineer should find out why.

(An exclusion constraint *is* used on `doctor_working_hours`, where intervals
genuinely have arbitrary lengths.)

### `SERIALIZABLE` isolation

**Rejected.** Correctness here rests on row locks and a unique index, both of
which behave identically at `READ COMMITTED`. `SERIALIZABLE` would add
serialisation failures to retry, and a retry loop, for no additional guarantee.

## Consequences

### Good

- **The invariant is a property of the data**, not of the code. It holds against
  any writer: a future endpoint, a background job, a manual `psql` session.
- Cross-replica by construction. Scaling out changes nothing.
- Cancelling frees the slot the instant it commits, with no cleanup.
- The audit trail is complete — nothing is deleted.
- It is **provable**, and proven: `TestExactlyTwoConcurrentBookingsForTheSameSlot`
  and `TestManyConcurrentBookingsForTheSameSlot` assert the database ends up with
  exactly one row.

### Bad

- A conflict is discovered **at write time**, so a client can be told a slot is
  free and then refused. This is inherent to the problem — the API documents
  availability as advisory, and the retry guidance is to refetch.
- The error path depends on a **constraint name**. A migration that renames it
  would silently degrade a `409` into a `500`. Mitigated by
  `TestConstraintTranslation`, which pins the mapping and fails loudly.
- Variable durations would require the migration described above.

### Neutral

- A conflicting insert briefly **blocks** on the index rather than failing
  immediately, until the holding transaction commits or rolls back. Transactions
  here are short, so the wait is negligible.

## How this will be verified

Nothing above is proven yet. The claim is falsifiable, and these are the tests
that must exist before it may be stated as fact:

- Two concurrent bookings for one slot produce exactly one success, one
  conflict, and **one row in the database**.
- Many concurrent bookings for one slot produce exactly one winner.
- Concurrent bookings for *different* slots all succeed — an over-broad lock
  would pass every test above while serialising the whole clinic.
- A failed reschedule leaves the appointment at its original time.
- The constraint name maps to the right domain error, so renaming it in a
  migration fails loudly rather than degrading a 409 into a 500.

They run against real PostgreSQL. An in-memory double cannot prove any of this.
