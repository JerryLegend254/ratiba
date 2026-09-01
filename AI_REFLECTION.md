# Section 4 — AI reflection

AI tooling was used throughout this project, and used hard. This document is a
straight account of where it helped, where it was wrong, and where the decisions
were mine.

The short version: **AI was most valuable on the parts of the work that are
repetitive, pattern-shaped or exhaustive** — HTTP handlers that all look alike,
table-driven test matrices, comment density, documentation, and the long tail of
security hardening. **The architecture, the invariants and the trade-offs were
mine**, worked out first and then implemented with AI as a fast, tireless pair
who never gets bored of writing the twelfth error-path test.

The division mattered, because the two categories fail differently. Repetitive
code fails visibly — it does not compile, or a test goes red. Architectural
decisions fail silently, months later, under load. I kept the second category
close.

---

## 1. What did you use AI for across the four sections?

### Section 1 — System design

I set the shape before writing any code: a modular monolith, PostgreSQL, the
domain isolated from I/O, and the no-double-booking guarantee enforced by the
database rather than by application code. Those were the constraints I wanted the
implementation to satisfy.

AI was useful here as an adversary rather than an author. I would state a
position and ask it to argue the opposite — *"make the case for an advisory
lock"*, *"why would a row lock on the doctor be wrong?"* — which is a fast way to
find the weak part of your own reasoning. What survived that became the
*Alternatives considered* sections in the ADRs, and they are honest: those are
options I actually rejected and the reasons I rejected them.

One thing AI is genuinely bad at, and worth naming: it will endorse whichever
design you raised most recently. It never says "no, your first idea was better."
That has to come from you.

### Section 2 — Implementation

This is where AI earned its place, on four kinds of work:

**Repetitive, pattern-shaped code.** Once the first handler existed — decode,
call one use case, map the result — the remaining nine are the same shape with
different types. Same for the DTO mapping, the repository methods, and the
per-endpoint error plumbing. I wrote the pattern; AI applied it consistently and
without the copy-paste slips I would have made by handler seven.

**Exhaustive test matrices.** The booking policy has a lot of edge cases: the
first slot, the last slot that fits, a slot starting exactly at closing time,
misaligned by fifteen minutes, misaligned by one minute, inside the lunch gap,
on a non-working weekday, in the past, one second inside the lead-time boundary,
one second outside it. I decided *which* boundaries mattered; AI wrote them out
as table-driven cases and did not get bored around case eleven, which is exactly
where I would have started cutting corners.

**Comment density.** This codebase explains its reasoning at the point of the
decision — why the recorder starts at zero, why reads happen before the
transaction opens, why the constraint name matters. I know from experience that
this is the first thing to get skipped under time pressure. Having a tireless
collaborator draft the explanation, which I then corrected where it had the
reasoning wrong, is how the density stayed consistent from the first file to the
last.

**Security hardening's long tail.** Strict JSON decoding, request body limits,
the full timeout set, constant-time token comparison, request-ID sanitisation,
secret redaction. Individually obvious; collectively easy to leave half-done.
This is checklist work and AI is good at checklists.

What I kept: the transaction boundaries, the lock strategy, the port
definitions, and the rule that no repository read happens inside a `WithinTx`
callback. That last one is a pool-deadlock waiting to happen and it is invisible
in testing, so it is written on the type as a comment for whoever refactors this
next.

### Section 3 — Deployment and CI/CD

AI drafted the workflow YAML, the multi-stage Dockerfile and the compose stack —
all high-syntax, low-judgement work where a missing key costs twenty minutes.

Two things I would not delegate. The **pinned action SHAs** were resolved from
the GitHub API rather than accepted from the model, because a hallucinated commit
SHA looks completely plausible and is a supply-chain hole. And the **environment
isolation** — three Railway environments, three databases, three project-scoped
tokens — is a blast-radius decision, not a configuration detail.

### Section 4 and documentation

AI drafted the README, the eight guides, seven ADRs and seven runbooks from the
decisions already made and the code already written.

The largest correction I made was to **claims**. Early drafts described the
deployment as if it existed and promised a "five-minute setup". Neither was true
at the time. The deployment section was rewritten to say plainly that nothing was
deployed yet, and to separate what had been verified from what had only been
written; the onboarding document records a measured time and immediately explains
why that number should not be trusted on different hardware.

The deployment is real now, and the same rule applied to writing it up: every row
of the *verified vs. not* table in [docs/deployment.md](docs/deployment.md) names
how that item was actually checked, and the rows that are still unproven say so.

Every number in this repository came from a command that was actually run.

---

## 2. One example where an AI suggestion improved the work

**The bidirectional contract-drift test.**

I was writing the OpenAPI document and already knew the failure mode: the file
would be accurate on the day it was written and wrong within a month. My prompt
was roughly:

> *"This OpenAPI file will drift from the code the moment someone adds an
> endpoint and forgets to update it. Is there a way to make that structurally
> impossible rather than a review checklist item?"*

I expected to be told to generate the spec from code annotations, which I did not
want — it makes the contract a side effect of the implementation instead of a
deliberate artifact. Instead the suggestion was to keep the hand-written document
and add a test that walks the live chi router with `chi.Walk`, then compares the
two **in both directions**, normalising parameter names so `{doctorID}` and
`{doctorId}` match.

The bidirectional part is the good idea, and I would probably not have bothered
with it. Checking that every documented route exists catches a stale document.
Checking that every *served* route is documented catches undocumented surface —
and that is the failure that actually happens, because nobody forgets to add the
endpoint, they forget to add the spec entry.

It found real problems on its first run, including two documentation endpoints
that were being served and were entirely absent from the contract. I have since
verified it fails correctly by adding an unlisted route:

```
route "GET /undocumented-probe" is served but missing from api/openapi.yaml
```

It also forced a small refactor I liked: `Handler` wraps the router in `otelhttp`,
which is opaque to `chi.Walk`, so `Routes` now builds the router and `Handler`
wraps it. That is a better separation regardless of the test.

This is the shape of AI being genuinely useful — not writing code I could not
write, but proposing a *check* I would not have thought to add.

---

## 3. One example where AI output was wrong or incomplete

There were several. The most instructive is the one **no test caught**, because
it says something about where review has to happen.

### The bug

The access-log and metrics middleware wraps the `ResponseWriter` to record what
was sent. The AI-written recorder was initialised like this:

```go
recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
```

with `WriteHeader` recording only the *first* status — which is correct in
isolation, because `net/http` ignores every call after the first.

Both halves are individually defensible. Together they are a serious bug:
pre-seeding `status` with `200` made the seed the "first" status, so
`WriteHeader(404)` never overwrote it.

**Every 4xx and 5xx was logged and counted as a success.**

### How I caught it

Not by reading the diff — it looks fine. By running the service and reading its
own output, which is something I do before believing any observability code:

```
client saw: 404      logged: 200   GET unmatched
client saw: 400      logged: 200   POST /appointments
client saw: 409      logged: 200   POST /appointments
```

### Why the test suite missed it

This is the part worth dwelling on. There were over 250 passing assertions,
including thorough HTTP contract tests. Every one of them asserted on **the
response the client receives**, which was correct throughout. Not one asserted
on **what was recorded about that response**. The two had silently diverged, and
adding more tests of the same kind would never have found it.

It is also the worst possible direction for the bug to point. A broken service
*looks healthy*: error-rate alerts never fire, dashboards stay green, and an
incident is invisible until a customer complains. For a role that is partly
technical support, shipping that would have been a genuine own goal.

### The correction

`status` now starts at 0, and a `statusCode()` accessor applies net/http's
implicit 200 only when nothing was written. I had the reasoning written into the
type, because the wrong version is the one that looks reasonable:

```go
// status stays 0 until something is actually written. Seeding it with 200
// would be a subtle and expensive mistake: WriteHeader only records the FIRST
// status ... and a pre-seeded 200 is already "first" — every 4xx and 5xx would
// then be logged and counted as a success.
```

Then I added `observability_test.go`, which asserts the logged status matches the
real one across 200/201/400/404/405/422 and that the metrics carry the right
`status_class`. I verified it catches the original bug by reintroducing it:

```
the client received 400 but the access log recorded 200.
```

**I deliberately left this bug in the commit history** rather than quietly
folding the fix into the commit that introduced it. The repository contains a
commit that ships the flaw (`1bdf6ba`) and a later one that finds and fixes it
(`f055d2b`), and the evidence is in that second diff: the one-line change to the
recorder, the reasoning written onto the type as a comment, and the 188-line
`observability_test.go` that fails without the fix. A history that only ever
shows correct code is not a history of real work, and the fix is more informative
with the failure still visible above it.

### The general lesson

AI is very good at producing code that is locally plausible. Both halves of that
recorder would pass a line-by-line review. Only their interaction was wrong, and
interaction is exactly what a diff does not show you.

What caught the real bugs in this project, in order: **running the system and
reading its output**, **concurrency tests that genuinely run concurrently**, and
**tests that assert a different property than the obvious one**.

> A close runner-up, and a good interview story: `TestConcurrentIdempotentRetries`
> found a real ordering bug in the idempotency implementation. A retry that races
> its own original trips the *slot* unique index before it ever reaches the
> idempotency key, so the safest thing a client can do was being turned into a
> spurious 409. Every single-threaded test passed. Detail in
> [ADR 0005](docs/adr/0005-idempotent-booking.md).

---

## 4. Name two decisions you made without AI. Why did you trust your own judgement?

### Decision 1 — The invariant goes in the database, and must be *proven*, not asserted

The requirement is one sentence: *"Once booked, that slot must not be available
to others."* The thing I recognised early, and held onto, is that **it cannot be
enforced by reading.** Any availability check is stale the moment it returns.
Between "is 09:00 free?" and "insert the appointment", another request can
commit. You can shrink that window; you cannot close it.

So the decision had to be made somewhere atomic with the write, and somewhere
that still holds with more than one replica. That is a partial unique index on
`(doctor_id, starts_at) WHERE status = 'booked'` — and the application inserts and
treats a constraint violation as the authoritative answer, rather than deciding
for itself.

I trusted my own judgement on two points in particular:

**Choosing the unique index over an exclusion constraint.** The more general tool
is not automatically the better one. Slots here are fixed at 30 minutes and
aligned, so two appointments overlap if and only if they start at the same
instant — equality is sufficient. A B-tree unique index is smaller, faster, and
legible to every engineer who will ever read this schema. I wrote down the
condition under which that becomes wrong (variable durations) in
[ADR 0003](docs/adr/0003-concurrency-strategy.md), so the next person inherits
the reasoning rather than just the result.

**Insisting the claim be falsifiable.** ADR 0003 shipped in the design commit
with a *"how this will be verified"* section instead of a verification table,
because at that point nothing was proven. It only became a table once the tests
existed. I have seen too many systems where the concurrency story is a paragraph
in a README that nobody has ever tested, and I did not want to write another one.

That is also why `TestConcurrentBookingsForDifferentSlotsAllSucceed` exists. It
looks redundant next to the double-booking tests, and it is the most important
one: an over-broad lock — `SELECT ... FOR UPDATE` on the doctor row, say — would
pass every double-booking test while quietly serialising the entire clinic. Only
a test asserting that *unrelated* bookings succeed concurrently catches that.
Nothing suggested that test to me; it came from having debugged a system that
made exactly that mistake.

### Decision 2 — No authentication, said loudly, rather than a shared API key

The brief does not mention authentication. The tempting move is to add a static
API key, because it looks like security and takes an afternoon.

I decided against it, and I am confident that was right.

A shared key authenticates nothing about **which patient is calling**. It
therefore cannot support the authorisation rule that actually matters — *a
patient may only act on their own appointments* — so every caller would still be
able to cancel every appointment. It would buy nothing real.

Worse, it would create the *impression* of protection. A reviewer, or a future
engineer, might reasonably assume the endpoints were guarded. **An openly absent
control is safer than a decorative one, because it cannot be mistaken for a real
one.**

So the gap is stated in the README's security section, in the API documentation,
in the OpenAPI description a client developer reads, and in
[ADR 0007](docs/adr/0007-deferred-authentication.md) with the seven concrete
steps that would close it. The threat model in
[docs/security.md](docs/security.md) marks four threats **NOT mitigated**, by
name, rather than quietly omitting them.

I trusted my judgement here because this is a question about honesty under
review, not a technical question — and because the codebase is arranged so the
work is additive when it comes: `appointment_events.source` is already the place
an authenticated actor ID goes, and the idempotency scope becomes
`(principal, key)` with a one-line change.

### A third, smaller one, since it shaped this repository

**Keeping the response-recorder bug in the git history.** It would have been
trivial to fold the fix into the commit that introduced it and present a clean
record. I chose not to. The commit that ships the flaw and the commit that finds
it are both in the log, and the regression test that pins it landed in the same
diff as the fix. Work that looks like it was right first time is not a useful
account of how software actually gets built.

---

## Honest summary

AI wrote a large share of the lines in this repository, and I would say so in an
interview without hesitation. It was fastest at the things that are tedious and
error-prone by hand: the tenth handler, the twelfth edge case, the security
checklist, the doc comment on every exported symbol.

What it was not good at was anything whose correctness depends on interactions it
cannot observe. Every genuine bug here came from that category — a PostgreSQL
type-inference rule, an ordering constraint between two inserts, two individually
reasonable decisions in one struct. None of them would have been caught by
reading the diff more carefully.

What made the difference was running the system and reading its real output,
writing tests that assert properties other than the obvious one, and refusing to
put a claim in the documentation that had not actually been verified. The coverage
figures come from a real `make coverage` run. The public URLs went into the README
only after `/readyz` answered on all three environments — and for as long as there
was nothing deployed, the deployment section said "not deployed" rather than
describing what it was going to be.

That discipline is the part I would bring to a team, and it is not something the
tooling supplies.
