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
| **CI runs the test suite on every pull request** | ✅ | [`ci.yml`](../.github/workflows/ci.yml) — `on: pull_request`. 7 parallel jobs. Note: an invalid workflow expression meant this file failed to parse and every run died in 0s until it was fixed on 21 Aug; the jobs themselves are verified locally, and `make verify-workflows` now gates the file |
| **Automatic deploy when a PR is merged into a designated branch** | ⚠️ | [`deploy.yml`](../.github/workflows/deploy.yml) — `on: push` to `dev`/`staging`/`main`. Merging a PR *is* a push; GitHub has no separate "merged" event |
| README states the public URL | ✅ | https://ratiba-api-production.up.railway.app, in the status table |
| README states which branch triggers a deploy, and how | ✅ | [README — Branch → environment mapping](../README.md#branch--environment-mapping) |
| README describes what the pipeline does | ✅ | [README — CI/CD](../README.md#cicd) with a flow diagram |

**Honest summary:** all three environments are live and verified. The one thing
not yet observed end to end is a CI-*driven* deploy — the environments were
provisioned and first-deployed from a workstation, so the next push to a
protected branch is the first time the workflow itself does the deploying.

The first attempts at that failed, for reasons worth recording rather than
quietly fixing: the deploy job pinned a Railway CLI predating the current
project-token exchange, and the CI workflow contained an expression error that
prevented it from ever running. Both are fixed and the diagnosis is in
[docs/ai-worklog.md](ai-worklog.md). Neither fix can be validated by re-running
the failed jobs, because a re-run replays the workflow file from the commit it
is re-running — only a fresh push exercises it.

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
| Link to a public GitHub/GitLab repository | ⚠️ Push required before submission |
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
