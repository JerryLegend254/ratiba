# Assessment traceability

Every requirement from *Savannah Informatics Backend Developer Take-Home
Assessment*, mapped to where it is implemented and where it is proven.

Status: ✅ done and verified · ⚠️ configured but not executed · ❌ not done

---

## Section 1 — System design

> *"Before writing any code, document your system design in your README. Cover
> the models and components you identified, the key decisions you made, and any
> trade-offs you considered."*

| Requirement | Where | Status |
|---|---|---|
| Models identified | [README — Data model](../README.md#data-model) ER diagram; [docs/data-model.md](data-model.md) | ✅ |
| Components identified | [README — Components](../README.md#components); [docs/architecture.md](architecture.md) | ✅ |
| Key decisions | [README — Key decisions](../README.md#key-decisions-and-trade-offs); 7 ADRs in [docs/adr/](adr/) | ✅ |
| Trade-offs considered | Every ADR has an explicit *Alternatives considered* section with reasons for rejection | ✅ |
| Ambiguities decided and noted | [README — Assumptions](../README.md#assumptions-and-known-limitations), 10 numbered decisions | ✅ |

---

## Section 2 — API implementation

### Required endpoints

| Endpoint | Implementation | Tests |
|---|---|---|
| `POST /appointments` | [`handlers_appointments.go`](../internal/transport/http/handlers_appointments.go) → `Service.Book` | `TestServiceBook`, `TestBookAppointmentEndpoint`, 6 concurrency tests |
| `GET /doctors/{id}/availability` | `handleDoctorAvailability` → `Service.Availability` | `TestServiceAvailability`, `TestAvailabilityEndpoint`, `TestAvailabilityAgainstRealData` |
| `PATCH /appointments/{id}/cancel` | `handleCancelAppointment` → `Service.Cancel` | `TestServiceCancel`, `TestCancelAndRescheduleEndpoints`, `TestConcurrentCancellations` |
| `PATCH /appointments/{id}/reschedule` | `handleRescheduleAppointment` → `Service.Reschedule` | `TestServiceReschedule`, `TestRescheduleAtomicity`, `TestConcurrentReschedulesOntoOneSlot` |

**All ✅**, at the exact paths specified.

### Per-endpoint requirements

| Requirement | Where enforced | Proven by |
|---|---|---|
| Booking is within working hours | `Policy.ValidateStart` → `slot_outside_working_hours` | `TestPolicyValidateStart` (before opening, after closing, in the lunch gap) |
| Booking is not in the past | `Policy.ValidateStart` → `slot_in_past` | `TestPolicyValidateStart/in the past` |
| Booking is not already taken | **Partial unique index**, not application code | `TestExactlyTwoConcurrentBookingsForTheSameSlot` — one row, always |
| Availability returns all free 30-min slots for a date | `Policy.FreeSlotsOn` | `TestPolicyFreeSlotsOn`, `TestServiceAvailability` |
| Cancel takes a reason | `normaliseCancellationReason` — required, trimmed, ≤500 chars | `TestServiceCancel/the reason is mandatory and bounded` |
| **Cancelling makes the slot bookable again** | Partial index covers only `status='booked'` | `TestServiceCancel/cancelling releases the slot for rebooking` |
| Cancelling twice errors | Row lock + status check → `409` | `TestServiceCancel/cancelling twice is a conflict` |
| **Reschedule frees the original slot** | Single `UPDATE` inside one transaction | `TestRescheduleAtomicity` |
| Reschedule validates the destination as a fresh booking | The *same* `Policy.ValidateStart` call | `TestServiceReschedule/the destination is validated exactly like a new booking` |
| Rescheduling a cancelled appointment errors | Status check under row lock → `409` | `TestServiceReschedule/a cancelled appointment cannot be moved` |

### Constraints

| Requirement | Status | Evidence |
|---|---|---|
| Meaningful validation errors | ✅ | RFC 9457 problem documents with stable codes, human-readable detail and field-level violations. Catalogue served at `/problems` |
| Correct HTTP status codes | ✅ | 400/404/405/409/413/415/422/500/503, each with documented semantics. `TestObservedStatusMatchesResponseStatus` verifies logs agree with responses |
| Not everything in one file | ✅ | Feature-oriented modular monolith: 8 packages, domain isolated from I/O |
| **At least basic test coverage for the booking logic** | ✅ | `internal/appointment` at **84.6%**, with 97 domain assertions plus 6 real-concurrency integration tests |

### Bonus

| Requirement | Status | Where |
|---|---|---|
| `GET /patients/{id}/appointments`, sorted by date | ✅ | `handlePatientAppointments`; ordered `(starts_at ASC, id ASC)`. `TestServiceListUpcomingForPatient`, `TestPatientAppointmentsOrderingAndPaging` |
| Prevention of bookings within 1 hour of now | ✅ | `Policy.BookableAt`; boundary pinned by `TestPolicyLeadTimeBoundary` (exactly 1h allowed, 1s less rejected) |

---

## Section 3 — Deployment and CI/CD

| Requirement | Status | Detail |
|---|---|---|
| Deployed to a cloud provider | ✅ | Railway, three isolated environments each with its own PostgreSQL |
| Reachable at a public URL | ✅ | https://ratiba-api-production.up.railway.app |
| Deployment configured | ✅ | [`railway.json`](../railway.json): Dockerfile builder, pre-deploy migration, `/readyz` health check, drain and restart policy |
| **CI runs the test suite on every pull request** | ⚠️ | [`ci.yml`](../.github/workflows/ci.yml) — `on: pull_request` and `on: push` to `dev`/`staging`/`main`. 7 parallel jobs behind one aggregate `CI passed` gate. An invalid workflow expression meant the file failed to parse and every run died in 0s until it was fixed on 21 Aug; the suite has since run green on all three branches. **Every run so far was push-triggered** — the `pull_request` trigger is configured and has not been exercised, because the branches were promoted by direct merge rather than by pull request. `make verify-workflows` now gates the file |
| **Automatic deploy when a PR is merged into a designated branch** | ✅ | [`deploy.yml`](../.github/workflows/deploy.yml) — `on: push` to `dev`/`staging`/`main`. Merging a PR *is* a push; GitHub has no separate "merged" event. Verified: a push to each of the three branches deployed the matching environment and passed its smoke suite |
| README states the public URL | ✅ | https://ratiba-api-production.up.railway.app, in the status table |
| README states which branch triggers a deploy, and how | ✅ | [README — Branch → environment mapping](../README.md#branch--environment-mapping) |
| README describes what the pipeline does | ✅ | [README — CI/CD](../README.md#cicd) with a flow diagram |

**Honest summary:** all three environments are live, and the pipeline now
deploys them. CI and Deploy have both run green on `dev`, `staging` and `main`.

Getting there took two failures worth recording rather than quietly fixing: the
deploy job pinned a Railway CLI predating the current project-token exchange, and
the CI workflow contained an expression error that stopped GitHub parsing the
file at all, so every run died in 0s with no logs. Every gate in that file was
correct and none of them had ever executed. Both are fixed, `make
verify-workflows` now catches the second class, and the diagnosis is in
[docs/ai-worklog.md](ai-worklog.md).

Two things are still unproven, and neither is fixed by asserting otherwise:

- **The `pull_request` trigger has never fired.** Branches were promoted by
  direct merge, so CI has only ever run on `push`. The jobs are identical either
  way, but "runs on every pull request" is configuration here, not observation.
- **The deployed-commit check does not gate.** `deploy.yml` compares the commit
  the service reports against the one being deployed, but on a mismatch it emits
  a warning instead of failing. Since Railway does not pass the `COMMIT`
  build-arg through, the service reports `unknown` and that step has warned on
  every deploy. It is a check that looks like a gate and is not one — the same
  shape of defect as the two above.

---

## Section 4 — AI reflection

| Question | Status |
|---|---|
| 1. What did you use AI for across the four sections? | ✅ Answered, evidenced by [docs/ai-worklog.md](ai-worklog.md) |
| 2. One example where AI improved the work (with the prompt) | ✅ The bidirectional OpenAPI drift test — with the real prompt and the four defects it found on its first run |
| 3. One example where AI was wrong, and how it was caught | ✅ The response-recorder status bug — caught by reading real container logs, not by any test |
| 4. Two decisions made *without* AI, and why | ✅ The database-enforced invariant with a falsifiable claim, and refusing a decorative API key — plus a third on keeping a real bug in the git history |

---

## Submission checklist

| Item | Status |
|---|---|
| Link to a public GitHub/GitLab repository | ✅ https://github.com/JerryLegend254/ratiba — public, confirmed with an unauthenticated request |
| Link to the deployed, running application | ✅ https://ratiba-api-production.up.railway.app |
| README covering design decisions | ✅ |
| README covering how to run locally | ✅ Verified from a clean checkout — [onboarding rehearsal](onboarding-rehearsal.md) |
| README covering the CI/CD setup | ✅ |
| Section 4 reflection | ✅ [AI_REFLECTION.md](../AI_REFLECTION.md) |

---

## Rules

> *"When requirements are ambiguous, make a decision and note it in your README."*

Ten ambiguities were resolved and recorded in
[README — Assumptions and known limitations](../README.md#assumptions-and-known-limitations):
appointment duration, slot alignment, whether a slot must fit inside one working
interval, the exact lead-time boundary, same-slot reschedule semantics, inactive
doctor handling, midnight-crossing intervals, what the patient list includes,
retention of cancelled appointments, and the advisory nature of availability.

> *"Clear reasoning matters as much as working code."*

Seven ADRs, each with the alternatives that were rejected and why; a threat model
that names what is *not* protected; and a deployment document that separates what
was verified from what was merely written.
