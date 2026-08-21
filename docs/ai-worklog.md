# AI working log

A dated record of where AI assistance was used, what it produced, how it was
verified, and what was changed or rejected on review. Kept while building rather
than reconstructed afterwards, so the account in
[AI_REFLECTION.md](../AI_REFLECTION.md) rests on evidence rather than memory.

No secrets or full transcripts — prompts are summarised, and only the parts that
affected a decision are quoted.

---

## 2026-08-19 — Schema and invariants

**Task.** Design the PostgreSQL schema, deciding where each invariant is
enforced.

**AI contribution.** Proposed the table layout, and specifically the **partial
unique index** `(doctor_id, starts_at) WHERE status = 'booked'` as the
double-booking guard, with the argument that a partial predicate lets cancelled
rows be retained without blocking a rebooking.

**Verification — not accepted on argument alone.** Before any Go was written, the
migrations were applied to a real PostgreSQL 17 container and each invariant was
probed with deliberate pass/fail SQL:

```
--- EXPECT FAIL: overlapping interval same weekday
ERROR:  conflicting key value violates exclusion constraint "doctor_working_hours_no_overlap"
--- EXPECT OK: adjacent (half-open) interval
--- EXPECT FAIL: double book same slot
ERROR:  duplicate key value violates unique constraint "appointments_active_slot_uniq"
--- EXPECT FAIL: cancelled without reason
ERROR:  ... violates check constraint "appointments_cancellation_consistency_check"
--- EXPECT OK: cancel with reason, then rebook the freed slot
```

**Outcome.** Accepted. The behaviour was demonstrated, not assumed. That
exercise is now `TestDatabaseEnforcesInvariants`.

**Changed from the suggestion.** The first draft used an `EXCLUDE` constraint
over a `tstzrange` for appointments. Rejected after reasoning about the actual
data: slots are fixed-length and aligned, so overlap reduces to equality on the
start instant, and a B-tree unique index is smaller, faster, and legible to any
engineer. Recorded with its migration trigger in
[ADR 0003](adr/0003-concurrency-strategy.md).

---

## 2026-08-19 — sqlc parameter inference

**Task.** Write the `CreateAppointment` query.

**AI suggestion.**

```sql
VALUES ($1, $2, $3, $3 + interval '30 minutes', 'booked')
```

Reusing `$3` to derive `ends_at`. It reads well.

**How it failed.** The first live booking returned `500`. The structured log gave
the cause immediately:

```json
{"event":"http.error","error":"... create appointment: ERROR: inconsistent types
 deduced for parameter $3 (SQLSTATE 42P08)"}
```

PostgreSQL cannot deduce one type for a parameter used both as a bare value and
inside interval arithmetic.

**Second attempt** — adding casts (`$3::timestamptz`) fixed the SQL but made sqlc
name the generated field `Column3`, losing the column association. Switching to a
named parameter (`@starts_at`) used twice made sqlc's rewriting fail outright.

**Correction.** Restructured so the domain supplies both ends of the interval:

```sql
VALUES ($1, $2, $3, $4, 'booked')
```

The `appointments_duration_check` constraint still verifies the two agree, so
nothing is lost. This also moved slot arithmetic into `Policy.SlotAt`, where the
half-open convention is applied in exactly one place.

**Noted for others.** Both sqlc pitfalls are written up in
[CONTRIBUTING.md](../CONTRIBUTING.md#code-generation).

---

## 2026-08-19 — Idempotency under concurrency

**Task.** Add `Idempotency-Key` support to `POST /appointments`.

**AI contribution.** Proposed the design that was adopted: store the key,
fingerprint and response snapshot in the same transaction as the appointment;
fingerprint the parsed fields rather than raw bytes; scope keys per patient.

**How it failed.** Every single-threaded test passed. Then
`TestConcurrentIdempotentRetries` — 8 concurrent requests sharing one key —
failed:

```
--- FAIL: TestConcurrentIdempotentRetries
    idempotent retry 0 failed: slot_unavailable (conflict): That slot is no
    longer available.
```

**Root cause.** The appointment `INSERT` must happen before the idempotency-key
`INSERT`, because the key row has a foreign key to the appointment. So a retry
racing its own original trips the **slot** unique index first and never reaches
the key logic — a safe retry turned into a spurious `409`.

**Correction.** On a slot conflict with a key present, check for a committed
record under the same `(patient, key)` before reporting a conflict; if one exists,
the conflict was self-inflicted and the stored response is returned
(`resolveIdempotentTwin`).

**Why this matters.** No amount of review found this. A concurrent test did, on
the first run. It is the clearest evidence in this project for writing
concurrency tests that actually run concurrently. Recorded in
[ADR 0005](adr/0005-idempotent-booking.md).

---

## 2026-08-19 — OpenAPI drift detection

**Task.** Keep the published contract honest.

**AI contribution.** Suggested walking the chi router with `chi.Walk` and
comparing it against the OpenAPI paths in **both** directions, normalising
parameter names so `{doctorID}` and `{doctorId}` compare equal.

**Outcome.** Accepted, and it earned its place on the first run by finding two
real defects:

```
route "PUT /metrics" is served but missing from api/openapi.yaml
route "GET /openapi.yaml" is served but missing from api/openapi.yaml
route "GET /docs" is served but missing from api/openapi.yaml
```

`/metrics` had been registered with `chi.Handle`, which binds **every** HTTP
verb — so `PUT`, `DELETE` and `TRACE` all reached the metrics handler. Changed to
`Get`. The two documentation endpoints were genuinely undocumented and were added
to the contract.

---

## 2026-08-19 — Status recorded in logs and metrics

**Task.** Access logging and request metrics.

**AI contribution.** A `responseRecorder` wrapping `http.ResponseWriter`,
initialised as:

```go
recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
```

with `WriteHeader` recording only the first status — correct in itself, since
net/http ignores later calls.

**How it failed.** Not caught by any test. Caught by **reading the real container
logs** after `docker compose up`:

```
event=http.rejected error_code=appointment_already_cancelled status=409
event=http.request  method=PATCH route=/appointments/{id}/cancel status=200
```

The client received `409`. The access log said `200`.

Pre-seeding `status` with `200` made the seed the "first" status, so `WriteHeader`
never overwrote it. **Every 4xx and 5xx was logged and counted as a success.**

**Why no test caught it.** Every existing test asserts on the response the
*client* receives, which was always correct. Nothing asserted on what was
*recorded about* the response. The two had silently diverged.

**Correction.** `status` starts at 0; a `statusCode()` accessor applies net/http's
implicit 200 only when nothing was written. Verified again in a rebuilt
container:

```
status=404  route=unmatched
status=400  route=/appointments
status=200  route=/doctors/{doctorID}/availability
```

**Regression cover.** Added
[`observability_test.go`](../internal/transport/http/observability_test.go),
which asserts the logged status matches the response across the 2xx/4xx/5xx
range, and that the metrics record the right `status_class`.

---

## 2026-08-19 — Lint findings

**Task.** Run `golangci-lint` with a strict configuration.

**AI contribution.** Proposed the linter selection and the initial config.

**Outcome.** 20 findings. Fixed properly rather than suppressed:

| Finding | Fix |
|---|---|
| `G115` int→int32 narrowing in config | Added an `int32Range` parser that bounds-checks *before* narrowing, removing the conversion from call sites |
| `G115` in `paginate` | Widened `offset`/`limit` to `int` instead of narrowing `len()` |
| `noctx` on `net.Listen` | Switched to `net.ListenConfig.Listen(ctx, …)`, so a cancelled context aborts the bind |
| `noctx` in tests | `httptest.NewRequestWithContext(t.Context(), …)` |
| `thelper`/`unused-parameter` | Removed a needless `*testing.T` from the time helpers, simplifying the whole test file |
| `tparallel` | Added `t.Parallel()` where correct; documented why `TestDatabaseEnforcesInvariants` deliberately runs sequentially |

Three suppressions remain, each with a written reason: two `gosec` SSRF findings
on a loopback-only health probe (the port is validated first), and one
`contextcheck` false positive on a deferred recover closure.

**Judgement applied.** Not every finding is a bug, but "the linter is wrong" is
the wrong default. Each was examined; most produced genuinely better code.

---

## 2026-08-19 — A wrong test expectation

**Task.** Test `doctor.ParseLocalTime`.

**AI-written test.** Asserted that `"9:00"` (unpadded hour) is **rejected**.

**Outcome.** The test failed — Go's `15` layout accepts an unpadded hour, so
`"9:00"` parses to 09:00.

**Correction.** **The test was wrong, not the code.** Accepting `"9:00"` is
harmless: it means the same thing, and the canonical rendering is always padded.
Rewritten to assert normalisation (`"9:00"` → `"09:00"`) rather than rejection.

Worth logging because the tempting move is to "fix" the code to match the test.

---

## 2026-08-19 — Configuration that cannot work

**Task.** Expose booking rules as environment variables.

**AI suggestion.** `BOOKING_SLOT_DURATION`, alongside `BOOKING_MIN_LEAD_TIME`.

**Problem noticed on review.** Slot duration is pinned by a `CHECK` constraint
(`ends_at = starts_at + interval '30 minutes'`). Setting `BOOKING_SLOT_DURATION=15m`
would pass configuration validation and then fail **every write** with a
constraint violation.

**Correction.** Removed from configuration entirely, with the reasoning written
into the struct: *"exposing a setting that silently breaks every write would be
worse than not having it."* `Policy` still parameterises the duration so unit
tests can exercise other values, and production uses the constant.

**Lesson.** A configuration option that cannot safely take a second value is not
configuration.

---

## 2026-08-19 — Documentation

**AI contribution.** Drafted the README, architecture, API, data model, testing,
operations, deployment and security documents; the seven ADRs; and the seven
runbooks, working from decisions already made and code already written. Produced
the Mermaid diagrams.

**Verification.** Every documented command was executed against the running
system. `make verify-docs` checks that every relative link resolves;
`make verify-openapi` checks the contract against the routes. The measured
coverage figures come from an actual `make coverage` run, not an estimate.

**Changed from the drafts.** Early versions asserted things that had not been
verified — a "five-minute setup", and a deployment section written as though the
service were live. Both were rewritten: the deployment section now states plainly
that nothing is deployed and lists exactly which artifacts were verified and
which were only written.

---

## 2026-08-21 — Three gates that were never running

**Task.** Get the first CI-driven deploy to succeed.

**What surfaced.** Three defects, each of which had been sitting in a green-
looking repository because the thing meant to catch them was itself broken.

| Defect | Why nobody noticed |
|---|---|
| `join(needs.*.result, " ")` in the CI gate | The expression language allows only single quotes. Valid YAML, invalid expression — GitHub voided the whole workflow and failed every run in **0 seconds** with no job logs. The only visible symptom was the API listing the workflow's name as its file path, because it never parsed far enough to read `name:` |
| `@railway/cli@4.5.3` pinned in the deploy job | Railway changed the project-token exchange after that release. A valid token is rejected with `Unauthorized. Please login with railway login` — a message that accuses the secret, so the instinct is to rotate it, which changes nothing |
| `.golangci.yml` still on the v1 schema | It declared `version: "2"` but kept `linters-settings` and `issues.exclude-*`. v2 *rejects* an unknown key rather than ignoring it, so the Lint job died during config validation and had never linted a single file |

**AI contribution.** Diagnosis and the fixes. The workflow bug was found with
`actionlint` after hand-reading and a YAML parser both passed the file — the
lesson being that "valid YAML" and "valid workflow" are different claims, and
only the second one matters.

**Where AI was wrong.** While migrating the lint config, the reasoning for
disabling `govet`'s `shadow` check was written as a comment — and the
`- shadow` entry itself was never added. `golangci-lint` reported the same 16
findings, and the natural next move was to blame caching or a schema quirk.
Parsing the file and printing the resulting `govet` map showed
`disable: ['fieldalignment']` — the justification existed, the change did not. A
comment explaining a change is not the change.

**Verification.** `golangci-lint config verify` passes and `golangci-lint run
./...` reports **0 issues** against the same 2.12.2 that CI pins. The 16 `shadow`
findings were each inspected before being disabled, including the three in
non-test code; all were the canonical `if err := f(); err != nil` form, which Go
scopes to the `if` precisely so it is safe. Vanilla `go vet` leaves `shadow` off
by default for the same reason. The Railway CLI floor was established by running
one token against both versions: 4.5.3 rejects it, 4.33.0 authenticates.

**Still unproven.** A deploy that runs end to end from CI. Re-running the failed
jobs replays the old workflow file, so the fix cannot be tested by re-run — the
first push carrying these commits is the real test.

**Guards added.** `make verify-workflows` (actionlint) and `make lint-config`,
both wired into the pipeline. Neither is a complete defence: a workflow broken
badly enough prevents the job that would have caught it from starting at all.

---

## Division of work

| Kind of work | Where AI led | Where I led |
|---|---|---|
| Architecture, invariants, trade-offs | arguing the alternatives | the decisions and the ADRs |
| Repetitive implementation (handlers, DTOs, repository methods) | most of it | the pattern the first one set |
| Test matrices and edge cases | writing them out exhaustively | choosing which boundaries matter |
| Concurrency, transactions, lock strategy | — | all of it |
| Comment density and documentation | drafting | correcting the reasoning, and every factual claim |
| Security checklist (timeouts, limits, redaction) | most of it | the threat model and what stays deferred |
| Deployment YAML and Dockerfile | drafting | environment isolation, pinned SHAs |

## Verification summary

Nothing in this project was accepted because it looked right:

| Claim | How it was verified |
|---|---|
| The schema enforces the invariants | Deliberate pass/fail SQL against real PostgreSQL |
| No double booking under concurrency | 6 integration tests with real concurrent transactions, `-race` |
| Migrations are reversible | Migrate fully down and back up on a throwaway database |
| Seeding is idempotent | Run twice, identical result |
| The API behaves as documented | 27-check smoke script against the real container |
| The container is hardened | CI asserts UID 65532 and that `/bin/sh` is **not** reachable |
| Graceful shutdown works | Observed the drain sequence in container logs; exit code 0 |
| Secrets are not logged | A test asserts it; confirmed in real log output |
| The contract matches the code | A test walks the router against the OpenAPI document |
| Coverage figures | An actual `make coverage` run |
| The lint suite is clean | `golangci-lint run ./...` → 0 issues, on the same 2.12.2 CI pins |
| The workflows are valid | `actionlint` over `.github/workflows/`, after a hand-read and a YAML parser both missed a fatal error |
