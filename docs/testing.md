# Testing

## The pyramid

```
        ┌──────────────────────────────────────┐
        │  Integration — real PostgreSQL       │   25 assertions, ~3s
        │  concurrency · transactions ·        │   build tag: integration
        │  constraints · migrations            │
        ├──────────────────────────────────────┤
        │  HTTP contract — real router,        │   84 assertions, ~3s
        │  in-memory store                     │
        ├──────────────────────────────────────┤
        │  Domain — pure, no I/O               │   97 assertions, ~2s
        │  policy · service · schedule · dates │
        └──────────────────────────────────────┘

276 passing assertions in total (counting subtests), measured on 2026-08-21.
```

The split is decided by **what a layer can honestly prove**, not by convention.

| Layer | Proves | Cannot prove |
|---|---|---|
| Domain | Every booking rule, DST behaviour, error codes | Anything about concurrency or SQL |
| HTTP contract | Status codes, problem documents, strict decoding, headers, observability | Anything about concurrency or SQL |
| Integration | Concurrency, transactions, constraints, constraint→error mapping | Nothing else needs it |

The in-memory store in [`internal/testsupport`](../internal/testsupport/) models
the partial unique index and transaction rollback well enough to test use-case
logic — but **a mutex-guarded Go map proves nothing about PostgreSQL**. Every
concurrency claim in this project is made only by the integration suite. That
boundary is stated in the package doc so nobody later mistakes a fast green
result for a real guarantee.

---

## Running the tests

```bash
make unit-test          # no database, race detector, ~5s
make integration-test   # real PostgreSQL, ~5s
make test               # both
make coverage           # both, with the threshold enforced
```

Under the hood:

```bash
go test -race -count=1 -shuffle=on ./...
go test -tags=integration -race -count=1 ./internal/postgres/...
```

- **`-race` always.** This code is concurrent by nature; without it the
  concurrency tests would prove much less.
- **`-count=1`** disables the result cache, so a "pass" is a real run.
- **`-shuffle=on`** in CI catches tests that accidentally depend on ordering.

### Integration test prerequisites

They need a real PostgreSQL and are skipped from `./...` by the `integration`
build tag, so `go test ./...` stays fast and dependency-free.

```bash
docker compose up -d postgres
make test-db            # creates ratiba_test if needed
make integration-test
```

`make integration-test` does the `test-db` step for you. To point at a different
server:

```bash
export TEST_DATABASE_URL='postgres://user:pass@host:5432/db?sslmode=disable'
go test -tags=integration ./internal/postgres/...
```

`TestMain` applies migrations before running anything, so the database only has
to exist and be reachable.

---

## Determinism

Two rules make this suite reproducible.

### Time is injected, never read

No test calls `time.Now()`. Every component takes a `clock.Clock`, and tests use
`clock.Fixed`:

```go
clk := testsupport.NewFixedClock()  // Monday 2026-09-07 05:00 UTC
clk.Set(nairobiAt(11, 0))           // move it
clk.Advance(2 * time.Hour)          // or nudge it
```

The fixture clock sits at 08:00 Nairobi — an hour before both test doctors open
— so every slot that day satisfies the one-hour lead time. Without this, a test
asserting "14 slots are available" would pass in the morning and fail in the
afternoon, and the failure would look like a code bug.

### Identifiers are fixed

Test doctors and patients use stable UUIDs from
[`testsupport/clinic.go`](../internal/testsupport/clinic.go). A failure that
says `expected 7f3c0a1e-…-0001` is far easier to read than one full of freshly
generated UUIDs.

Integration tests are the exception: each creates its **own** doctor and patients
with fresh UUIDs, which is what lets them run in parallel against a shared
database without truncating tables between runs.

---

## Fixtures

`testsupport.NewClinic()` gives every test the same clinic:

| Fixture | Detail |
|---|---|
| `NairobiDoctorID` | Mon–Fri 09:00–13:00 and 14:00–17:00, `Africa/Nairobi` (UTC+3, no DST) |
| `LondonDoctorID` | Mon–Fri 09:00–17:00, `Europe/London` (**observes DST**) |
| `InactiveDoctorID` | Exists, `is_active = false` |
| `ActivePatientID`, `OtherPatientID` | Can book |
| `InactivePatientID` | Exists, cannot book |

The Nairobi doctor has a **lunch gap**, which is what makes it possible to test
that a slot inside a gap is rejected rather than merely a slot outside all
working hours. The London doctor exists so DST is exercised by the standard
fixture, not by a special case.

---

## What is tested

### Booking rules — [`policy_test.go`](../internal/appointment/policy_test.go)

Table-driven, pure. Every case names the behaviour, not the mechanism:

- Valid at the opening slot, the last slot before the lunch gap, the first slot after it, and the last slot of the day
- A slot starting exactly at closing time is rejected — the appointment would run past the end
- Before opening, after closing, inside the lunch gap
- Misaligned by 15 minutes and by 1 minute
- A non-working weekday
- In the past, and inside the one-hour lead time
- **The lead-time boundary exactly**: `now + 1h` is allowed, one second later is not
- **DST**: slot count, local wall clock and UTC instant either side of a
  transition; a window a transition passes through yields fewer slots
- `NewPolicy` rejects a duration that does not divide an hour (45 minutes would
  slide slots off the half-hour grid partway through the day)

One test deserves calling out:

```go
t.Run("every offered slot passes validation", ...)
```

It takes everything the availability endpoint would return and feeds each one
back through the booking validator. This is the guarantee that makes availability
trustworthy — the two can never disagree, because they are the same code.

### Use cases — [`service_test.go`](../internal/appointment/service_test.go)

- Booking succeeds and writes exactly one audit event
- A taken slot is `slot_unavailable`, with exactly one active row remaining
- The same instant with a *different* doctor is fine
- Unknown and inactive doctors and patients
- **A failed transaction leaves no partial state** — no appointment, no event
- Idempotency: replay, key reuse with a different payload, malformed keys, and
  keys being independent per patient
- **A replay returns the appointment's *original* state**, even after it has been
  cancelled
- Cancellation releases the slot; a repeat conflicts; the reason is required,
  trimmed and length-bounded
- Reschedule frees the old slot, claims the new one, and writes a `from`/`to`
  audit event
- **A conflicting reschedule rolls back and leaves the original intact**
- The destination is validated exactly like a new booking
- Patient listing: ordering, exclusion of cancelled and past, paging, clamping

### HTTP contract — [`server_test.go`](../internal/transport/http/server_test.go)

Real router, real middleware, in-memory store. Status codes, problem documents,
headers, and the strict-decoding rejections (malformed JSON, unknown fields,
trailing values, oversized bodies, wrong content type, naive timestamps).

Also: `/readyz` behaviour when the database is up and down, **and that its
response never leaks the driver error or connection string**; request-ID
sanitisation against injection; CORS off by default.

### Contract drift — [`openapi_test.go`](../internal/transport/http/openapi_test.go)

Walks the real chi router and compares it with `api/openapi.yaml` **in both
directions**. A route served but undocumented fails; a route documented but not
served fails.

This is not theoretical — it caught two real problems the first time it ran:
`/metrics` was registered for every HTTP verb rather than `GET`, and `/docs` and
`/openapi.yaml` were undocumented.

### Observability — [`observability_test.go`](../internal/transport/http/observability_test.go)

Asserts that the access log and metrics agree with what the client actually
received, and that the cancellation reason, raw paths with identifiers, and
credentials never reach a log.

This file exists because of a real bug: the response recorder was pre-seeded with
`200`, and since `WriteHeader` records only the first status, **every 4xx and 5xx
was logged and counted as a success**. Every other test passed, because they
assert on the response the client sees, not on what was recorded about it. It was
found by reading real container logs, not by any test.

### Concurrency — [`concurrency_test.go`](../internal/postgres/concurrency_test.go)

The tests the whole design exists to satisfy.

| Test | Claim |
|---|---|
| `TestExactlyTwoConcurrentBookingsForTheSameSlot` | Two simultaneous requests → one `201`, one `409`, **one row** |
| `TestManyConcurrentBookingsForTheSameSlot` | 24 simultaneous → exactly one winner |
| `TestConcurrentBookingsForDifferentSlotsAllSucceed` | Unrelated slots are not serialised |
| `TestConcurrentIdempotentRetries` | 8 same-key retries → one appointment, one event, all callers get it |
| `TestConcurrentCancellations` | Exactly one caller cancels |
| `TestConcurrentReschedulesOntoOneSlot` | One winner; **every loser keeps its original slot** |

Each uses a `sync.WaitGroup` as a **starting gate** so the goroutines genuinely
overlap:

```go
var gate sync.WaitGroup
gate.Add(1)
// …every goroutine calls gate.Wait() first…
gate.Done()   // released together
```

Without it, goroutines would start staggered and the writes would mostly not
race — the test would pass while proving nothing.

`TestConcurrentBookingsForDifferentSlotsAllSucceed` is the inverse guard: an
over-broad lock (say, `SELECT … FOR UPDATE` on the doctor row) would still pass
every double-booking test while serialising the whole clinic. Only a test that
asserts unrelated bookings *succeed concurrently* catches it.

### Database invariants — [`repository_test.go`](../internal/postgres/repository_test.go)

Writes SQL **directly**, bypassing the application, to prove the schema protects
the data even against a future code path that forgets to validate. Also pins the
constraint-name → domain-error mapping, so renaming a constraint in a migration
fails a test rather than silently degrading a `409` into a `500`.

`TestMigrationsRollBackAndReapply` creates a throwaway database, migrates all the
way down and back up, and confirms the schema still works.

---

## Coverage

```bash
make coverage        # enforces the gate
make coverage-html   # writes coverage.html
```

### Measured — 2026-08-21

| Package | Coverage |
|---|---|
| `internal/patient` | 100.0% |
| `internal/doctor` | 98.9% |
| `internal/platform/config` | 88.3% |
| `internal/platform/apperror` | 87.5% |
| `internal/appointment` | 84.6% |
| `internal/platform/calendar` | 80.5% |
| `internal/transport/http` | 72.8% |
| `internal/postgres` | 65.7% |
| **Total** | **67.7%** |

### The policy

**≥80% on `appointment`, `doctor` and `patient`.** These hold the business rules.

Deliberately **not** a module-wide threshold. One number averaged over generated
sqlc code, wiring in `main`, and cross-cutting platform helpers is easy to
inflate and would let the booking logic rot while the percentage stayed green. A
coverage gate should protect something specific.

Both suites contribute to a single profile — the integration tests exercise the
adapter and much of the service, so measuring only the unit suite would
understate real coverage and push toward writing tests that inflate a number.

What is intentionally uncovered: generated code, `main`, and error branches that
require a database failure mid-transaction.

---

## Adding a test

| Change | Where |
|---|---|
| A booking rule | `policy_test.go` — a table case, both accepted and rejected sides |
| A use case | `service_test.go` — in-memory store |
| An endpoint | `server_test.go` — status, body shape, error code |
| A query or schema change | `internal/postgres/` — integration |
| Anything concurrent | `concurrency_test.go` — real goroutines, real database |

### Conventions

**Name the behaviour, not the mechanism.**

```go
// Bad
t.Run("test case 3", ...)
t.Run("TestValidateStartReturnsError", ...)

// Good
t.Run("slot starting exactly at closing time does not fit", ...)
t.Run("a conflicting destination rolls back and leaves the original intact", ...)
```

**Failure messages should say what went wrong and why it matters.**

```go
t.Fatalf("expected exactly 1 active appointment in the database, found %d", count)
```

**Assert on behaviour, not implementation.** Check the returned error code, not
which internal function was called.

---

## Troubleshooting

### `TEST_DATABASE_URL is not set`

```bash
docker compose up -d postgres
make integration-test
```

### Integration tests fail with `connection refused`

```bash
docker compose ps postgres          # is it running and healthy?
docker compose logs postgres        # did initdb fail?
lsof -i :5432                       # is something else on the port?
```

If another PostgreSQL owns 5432, set `POSTGRES_PORT=5433` in `.env` and restart.

### `relation "appointments" does not exist`

Migrations did not apply. `TestMain` runs them, so this usually means the
connection points at the wrong database:

```bash
echo "$TEST_DATABASE_URL"
make migrate-status
```

### A test passes alone but fails with the others

Almost always shared state. Integration tests must create their **own** doctor
and patients — copy the pattern in `newFixture`, do not reuse another test's IDs.

If it fails only with `-shuffle=on`, the test depended on execution order.

### A concurrency test is flaky

It should never be. If one is:

1. Confirm the starting gate is in place — without it, goroutines do not actually
   overlap.
2. Re-run under stress: `go test -tags=integration -race -count=20 -run TestExactly… ./internal/postgres/...`
3. Check the assertion is on the **database**, not on the Go-side result. The
   database row count is the ground truth.

### A test depends on the current date

It should not. If a test only fails on certain days, it is reading the wall
clock somewhere. Search for `time.Now()` in the test and its helpers.

### `race detected`

Real, and worth fixing before anything else. The report names both goroutines
and both stacks. Note that a data race in a *test helper* is still a bug — a
shared fixture mutated from parallel subtests is the usual cause.

---

## What is not tested, and why

| Not tested | Why |
|---|---|
| Generated sqlc code | Generated. The queries are covered through the repositories that call them |
| `cmd/api` wiring | Covered end-to-end by the smoke test against a real container |
| Prometheus scrape format | The client library's responsibility; the tests assert the series exist |
| OTLP export | Requires a collector. Startup with tracing enabled but no collector *is* tested by way of the config tests |
| Load and performance | Out of scope for the assessment. The concurrency tests establish correctness under contention, not throughput |
| Restore from backup | No backup procedure is implemented; see [operations](operations.md#backup-and-restore) |
